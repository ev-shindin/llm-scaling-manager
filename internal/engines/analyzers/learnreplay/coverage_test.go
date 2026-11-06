package learnreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
)

// The coverage test (docs/proposals/analyzer-learner.md, 2.4), part 1: every
// exported method of the four recorded types is in exactly one reviewed
// bucket, so a new method fails here until someone decides whether it is an
// input the recording must carry.
type buckets struct {
	// recorded methods write one event per call.
	recorded []string
	// parsing wrappers call a recorded method and record nothing of their own.
	parsing []string
	// readers change nothing a later call reads.
	readers []string
	// builders configure the type before use; the start line carries what
	// they set (the clock and the recorder are the replay's own).
	builders []string
	// internal is, for the store, the mutators only an analyzer calls, each
	// with the analyzer event that reproduces it on replay.
	internal map[string]string
	// seams is the one replay switch hook per package.
	seams []string
}

func (b buckets) all() []string {
	out := slices.Concat(b.recorded, b.parsing, b.readers, b.builders, b.seams)
	for m := range b.internal {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

var reviewed = map[reflect.Type]buckets{
	reflect.TypeFor[*saturation.SaturationAnalyzer](): {
		recorded: []string{"Analyze", "NoteVariantsPresent", "EvictStaleHistory", "ForgetVariant",
			// ForgottenToken is not a pure reader: reading it refreshes the
			// forget mark's liveness stamp, which the sweep reads.
			"ForgottenToken",
			"HoldLowerBounds", "ExportLearnedState", "RestoreLearnedState", "TakeRestoreOutcomes",
			"ForgetLearnedState", "StableAcceleratorFor"},
		// PathCounts reads the per-path counters (2.5), which only a replay
		// reads and nothing restores.
		// LearnerReports copies each model's last cycle of learner reports,
		// which the analyzer overwrites every cycle and nothing restores.
		// ITLReadings copies a key's readings, and PricingBatchCap reads the
		// cap without stamping a learned one as used: the what-if planner's
		// views (whatif_view.go), as is ITLOneSided, which copies the
		// one-sided admission a key was last priced with (issue #467).
		// PrefillEstimate copies a prefill variant's stashed estimate for the
		// slo analyzer, which the analyzer overwrites every cycle and nothing
		// restores (prefill_observe.go).
		readers: []string{"Name", "Horizons", "BorrowTickGap", "PathCounts", "LearnerReports",
			"ITLReadings", "PricingBatchCap", "ITLOneSided", "PrefillEstimate"},
		// WithSettleView hands k2 the engine's settle tracker; the replay hands
		// it its own, observed from the settle events (settle.go).
		builders: []string{"WithHorizons", "WithOptimizationInterval", "WithClock", "WithRecorder", "WithSettleView"},
		seams:    []string{"SetSwitchesForReplay"},
	},
	reflect.TypeFor[*slo.Analyzer](): {
		recorded: []string{"Analyze", "EvictStale", "BeginCycle", "NoteVariantsPresent", "ForgetVariant"},
		// PathCounts reads the per-path counters of the slo line's fits
		// (2.5), which only a replay reads and nothing restores.
		readers: []string{"Name", "StateTimeout", "SlopeHoldFor", "Judging", "PathCounts"},
		// WithSettleView hands the slo line the engine's settle tracker; the
		// replay hands it its own, as it does k2's (settle.go).
		// WithPrefillEstimates hands it the saturation analyzer's prefill
		// estimates; the replay hands it its own saturation analyzer.
		builders: []string{"WithClock", "WithStateTimeout", "WithSlopeHoldFor", "WithRecorder", "WithSettleView",
			"WithOptimizationInterval", "WithPrefillEstimates"},
		seams: []string{"SetSwitchesForReplay"},
	},
	reflect.TypeFor[*throughput.ThroughputAnalyzer](): {
		recorded: []string{"Analyze", "Observe", "ForgetVariant"},
		// StartConfig states what the builders set, for the recording's
		// first line; it changes nothing. PathCounts reads the per-path
		// counters of the ITL line's fits (2.5), which only a replay reads.
		readers: []string{"Name", "VariantState", "StartConfig", "PathCounts"},
		// WithSettleView and WithCapacityStore hand the ITL line's admission
		// the engine's settle tracker and store, read-only; the replay hands
		// it its own (start.ThroughputConfig).
		builders: []string{"WithClock", "WithRecorder", "WithSettleView", "WithOptimizationInterval",
			"WithSlopeHoldFor", "WithCapacityStore"},
		seams: []string{"SetSwitchesForReplay"},
	},
	reflect.TypeFor[*capacity.Store](): {
		recorded: []string{"FileScaleTarget", "EvictStale"},
		parsing:  []string{"LoadFromScaleTarget"},
		readers:  []string{"Get", "IsStale", "FindCompatible", "FindClampBound", "Len"},
		builders: []string{"WithRecorder"},
		internal: map[string]string{
			"Update": saturation.KindAnalyze,
			"Delete": saturation.KindForgetVariant,
		},
		seams: []string{"SetSwitchesForReplay"},
	},
}

func TestCoverage_EveryExportedMethodIsReviewed(t *testing.T) {
	for typ, b := range reviewed {
		methods := make([]string, 0, typ.NumMethod())
		for i := range typ.NumMethod() {
			methods = append(methods, typ.Method(i).Name)
		}
		sort.Strings(methods)
		listed := b.all()
		assert.Equal(t, methods, listed,
			"%s: the exported methods and the reviewed buckets differ; classify the new method (recorded, parsing wrapper, reader, builder, analyzer-internal or seam)", typ)
		seen := map[string]bool{}
		for _, m := range listed {
			assert.False(t, seen[m], "%s.%s is in two buckets", typ, m)
			seen[m] = true
		}
	}
}

// Part 2: each recorded method, called once, writes exactly one event, of its
// own kind. One Analyze is one Analyze event whatever it calls inside; only
// check events (a line published or borrowed) may sit beside it.
func TestCoverage_EachRecordedCallWritesOneEvent(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	policy := &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5,
		SLO: &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 50 * time.Millisecond}}}}
	row := domain.ReplicaMetrics{PodName: "p", VariantName: "v", Namespace: "ns", ModelID: "m", Ready: true,
		KvCacheUsage: 0.5, KvUsageInstant: 0.5, TotalKvCapacityTokens: 10_000, NumGpuBlocks: 625, BlockSize: 16,
		TokensInUse: 5_000, AvgITL: 0.01, AvgITLKnown: true, AvgInputTokens: 500, AvgOutputTokens: 200}
	input := domain.AnalyzerInput{ModelID: "m", Namespace: "ns", Config: policy,
		ReplicaMetrics: []domain.ReplicaMetrics{row},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1,
			AcceleratorName: "", GPUsPerReplica: 1, Role: domain.RoleDecode}}}

	type call struct {
		name, kind string
		do         func(*fixture)
	}
	calls := []call{
		{"saturation.Analyze", saturation.KindAnalyze, func(f *fixture) { _, _ = f.sat.Analyze(ctx, input) }},
		{"saturation.NoteVariantsPresent", saturation.KindNoteVariantsPresent, func(f *fixture) { f.sat.NoteVariantsPresent("ns", "m", []string{"v"}) }},
		{"saturation.EvictStaleHistory", saturation.KindEvictStaleHistory, func(f *fixture) { f.sat.EvictStaleHistory() }},
		{"saturation.ForgetVariant", saturation.KindForgetVariant, func(f *fixture) { f.sat.ForgetVariant("ns", "m", "v", "t") }},
		{"saturation.ForgottenToken", saturation.KindForgottenToken, func(f *fixture) { f.sat.ForgottenToken("ns", "v") }},
		{"saturation.HoldLowerBounds", saturation.KindHoldLowerBounds, func(f *fixture) { f.sat.HoldLowerBounds(true) }},
		{"saturation.ExportLearnedState", saturation.KindExportLearnedState, func(f *fixture) { f.sat.ExportLearnedState() }},
		{"saturation.RestoreLearnedState", saturation.KindRestoreLearnedState, func(f *fixture) { f.sat.RestoreLearnedState(saturation.LearnedState{}) }},
		{"saturation.TakeRestoreOutcomes", saturation.KindTakeRestoreOutcomes, func(f *fixture) { f.sat.TakeRestoreOutcomes() }},
		{"saturation.ForgetLearnedState", saturation.KindForgetLearnedState, func(f *fixture) { f.sat.ForgetLearnedState() }},
		{"saturation.StableAcceleratorFor", saturation.KindStableAcceleratorFor, func(f *fixture) { f.sat.StableAcceleratorFor("ns", "v", "") }},
		{"slo.Analyze", slo.KindAnalyze, func(f *fixture) { _, _ = f.slo.Analyze(ctx, input) }},
		{"slo.EvictStale", slo.KindEvictStale, func(f *fixture) { f.slo.EvictStale(at) }},
		{"slo.BeginCycle", slo.KindBeginCycle, func(f *fixture) { f.slo.BeginCycle() }},
		{"slo.NoteVariantsPresent", slo.KindNoteVariantsPresent, func(f *fixture) { f.slo.NoteVariantsPresent("ns", "m", []string{"v"}) }},
		{"slo.ForgetVariant", slo.KindForgetVariant, func(f *fixture) { f.slo.ForgetVariant(ctx, "ns", "m", "v") }},
		{"throughput.ForgetVariant", throughput.KindForgetVariant, func(f *fixture) { f.tp.ForgetVariant(ctx, "ns", "m", "v") }},
		{"throughput.Analyze", throughput.KindAnalyze, func(f *fixture) { _, _ = f.tp.Analyze(ctx, input) }},
		{"throughput.Observe", throughput.KindObserve, func(f *fixture) { f.tp.Observe(ctx, at, "m", "ns", input.ReplicaMetrics) }},
		{"store.FileScaleTarget", capacity.KindFileScaleTarget, func(f *fixture) {
			f.store.FileScaleTarget("ns", "m", "v", "H100", 1, capacity.EngineParams{Engine: inferenceengine.EngineVLLM}, false, at)
		}},
		{"store.EvictStale", capacity.KindEvictStale, func(f *fixture) { f.store.EvictStale(at, time.Hour) }},
	}
	covered := map[string]bool{}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(at)
			// A filed record, so the analyzers have a configuration to read.
			f.store.FileScaleTarget("ns", "m", "v", "H100", 1, capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64}, false, at)
			f.buf.Reset()
			f.attach(learnrec.New(f.buf, 0))
			c.do(f)
			events, err := readEvents(f.buf.Bytes())
			require.NoError(t, err)
			var own, other []string
			for _, ev := range events {
				switch {
				case ev.Kind == c.kind:
					own = append(own, ev.Kind)
				case checkKinds[ev.Kind]:
				default:
					other = append(other, ev.Kind)
				}
			}
			assert.Len(t, own, 1, "%s wrote %d events of its kind", c.name, len(own))
			assert.Empty(t, other, "%s recorded a nested call as well", c.name)
		})
		covered[c.kind] = true
	}
	// Every recorded method of the four types is in the table.
	for typ, b := range reviewed {
		prefix := map[reflect.Type]string{
			reflect.TypeFor[*saturation.SaturationAnalyzer](): "saturation.",
			reflect.TypeFor[*slo.Analyzer]():                  "slo.",
			reflect.TypeFor[*throughput.ThroughputAnalyzer](): "throughput.",
			reflect.TypeFor[*capacity.Store]():                "store.",
		}[typ]
		for _, m := range b.recorded {
			assert.True(t, covered[prefix+m], "the recorded method %s%s has no row in the table", prefix, m)
		}
	}
}

// fixture is the four recorded types on one recorder and one fake clock.
type fixture struct {
	buf   *bytes.Buffer
	store *capacity.Store
	sat   *saturation.SaturationAnalyzer
	slo   *slo.Analyzer
	tp    *throughput.ThroughputAnalyzer
	clk   *clocktesting.FakePassiveClock
	rec   *learnrec.Recorder
	// settle is the engine's settle tracker, observed before each cycle's
	// Analyze as the engine observes it (fixture.cycle).
	settle *settle.Tracker
}

type nilRouter struct{}

func (nilRouter) CollectRouterLatency(context.Context, string, string) *domain.RouterLatency {
	return nil
}

func newFixture(at time.Time) *fixture {
	buf := &bytes.Buffer{}
	rec := learnrec.New(buf, 0)
	clk := clocktesting.NewFakePassiveClock(at)
	store := capacity.NewStore().WithRecorder(rec)
	tracker := settle.NewTracker()
	tracker.SetInterval(config.DefaultOptimizationInterval)
	return &fixture{
		buf:    buf,
		clk:    clk,
		rec:    rec,
		store:  store,
		settle: tracker,
		sat:    saturation.NewSaturationAnalyzer(store).WithClock(clk).WithRecorder(rec).WithSettleView(tracker),
		slo:    slo.New(nilRouter{}, store).WithClock(clk).WithRecorder(rec).WithSettleView(tracker),
		tp:     throughput.NewThroughputAnalyzer().WithClock(clk).WithRecorder(rec),
	}
}

// The switch hook is a seam: each package takes the empty set and refuses a
// name it does not know.
func TestSwitchHooks_AcceptTheEmptySetAndRefuseUnknownNames(t *testing.T) {
	f := newFixture(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	hooks := map[string]func(map[string]bool) error{
		PackageSaturation: f.sat.SetSwitchesForReplay,
		PackageSLO:        f.slo.SetSwitchesForReplay,
		PackageThroughput: f.tp.SetSwitchesForReplay,
		PackageCapacity:   f.store.SetSwitchesForReplay,
		PackageSettle:     settle.NewTracker().SetSwitchesForReplay,
	}
	for pkg, hook := range hooks {
		assert.NoError(t, hook(nil), pkg)
		assert.NoError(t, hook(map[string]bool{}), pkg)
		assert.Error(t, hook(map[string]bool{"no-such-switch": true}), pkg)
	}
}

// No production path calls a switch hook, or ReplaySwitchNames: only this
// package, and tests, may.
func TestSwitchHooks_NoProductionPathCallsThem(t *testing.T) {
	root := moduleRoot(t)
	var callers []string
	files, defined, namesDefined := 0, 0, 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "bin", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/engines/analyzers/learnreplay/") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				switch fn.Name.Name {
				case "SetSwitchesForReplay":
					defined++
				case "ReplaySwitchNames":
					namesDefined++
				}
			}
		}
		seam := func(name string) bool { return name == "SetSwitchesForReplay" || name == "ReplaySwitchNames" }
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					if seam(fn.Sel.Name) {
						callers = append(callers, rel)
					}
				case *ast.Ident:
					if seam(fn.Name) {
						callers = append(callers, rel)
					}
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	// The walk reached the five hooks themselves, so it read the packages
	// that define them, and the rest of the tree with them.
	require.Equal(t, 5, defined, "the walk found %d SetSwitchesForReplay definitions in %d files", defined, files)
	// ReplaySwitchNames: saturation's, slo's, throughput's and settle's.
	require.Equal(t, 4, namesDefined, "the walk found %d ReplaySwitchNames definitions", namesDefined)
	assert.Empty(t, callers, "a production path calls a replay switch hook")
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.mod above the test")
		dir = parent
	}
}

// readEvents is learnrec.Read's events.
func readEvents(b []byte) ([]learnrec.RawEvent, error) {
	r, err := learnrec.Read(b)
	return r.Events, err
}

// attach hands rec to all four recorded types.
func (f *fixture) attach(rec *learnrec.Recorder) {
	f.rec = rec
	f.store.WithRecorder(rec)
	f.sat.WithRecorder(rec)
	f.slo.WithRecorder(rec)
	f.tp.WithRecorder(rec)
}

// ttftInput is an input carrying every field R1 added for the TTFT model
// (docs/proposals/analyzer-learner.md, 2.9), each set.
func ttftInput() domain.AnalyzerInput {
	inf := math.Inf(1)
	row := domain.ReplicaMetrics{PodName: "p", VariantName: "v", Namespace: "ns", ModelID: "m", Ready: true,
		KvCacheUsage: 0.5, KvUsageInstant: 0.5, TotalKvCapacityTokens: 10_000, NumGpuBlocks: 625, BlockSize: 16,
		TokensInUse: 5_000, AvgITL: 0.01, AvgITLKnown: true, AvgInputTokens: 500, AvgOutputTokens: 200,
		QueueLength: 2, QueueLengthKnown: true, AvgServiceTime: 0.6, AvgServiceTimeKnown: true,
		AvgTTFT: 0.15, AvgTTFTKnown: true,
		PromptTokensBuckets: []domain.LengthBucket{{UpperBound: 1024, Rate: 1.25}, {UpperBound: inf, Rate: 2.5}},
		OutputTokensBuckets: []domain.LengthBucket{{UpperBound: 256, Rate: 1.75}, {UpperBound: inf, Rate: 2.5}},
		TTFTBuckets:         []domain.LatencyBucket{{UpperBound: 0.1, Rate: 0.5}, {UpperBound: inf, Rate: 2.5}},
		AvgQueueTime:        0.035, AvgQueueTimeKnown: true,
		QueueTimeBuckets: []domain.LatencyBucket{{UpperBound: 0.3, Rate: 2.25}, {UpperBound: inf, Rate: 2.5}},
		AvgPrefillTime:   0.0625, AvgPrefillTimeKnown: true,
		AvgKVTransferTime: 0.0075, AvgKVTransferTimeKnown: true,
		TPOTBuckets: []domain.LatencyBucket{{UpperBound: 0.025, Rate: 2}, {UpperBound: inf, Rate: 2.5}},
		QueueMin:    1, QueueMinKnown: true}
	return domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
		Config: &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5,
			SLO: &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 50 * time.Millisecond}}}},
		ReplicaMetrics: []domain.ReplicaMetrics{row},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1,
			AcceleratorName: "H100", Role: domain.RoleDecode}},
		RouterLatency: &domain.RouterLatency{WindowSeconds: 300,
			TTFTBuckets: []domain.LatencyBucket{{UpperBound: 1, Rate: 2}, {UpperBound: inf, Rate: 2.5}},
			TPOTSumRate: 0.0625, TPOTCountRate: 2.5,
			TPOTBuckets:            []domain.LatencyBucket{{UpperBound: 0.05, Rate: 2.25}, {UpperBound: inf, Rate: 2.5}},
			FlowControlWaitBuckets: []domain.LatencyBucket{{UpperBound: 0.01, Rate: 2}, {UpperBound: inf, Rate: 2.5}}},
		FlowControlPools: []domain.FlowControlPool{{WindowSeconds: 300,
			EffectiveSaturation: 0.875, EffectiveSaturationKnown: true,
			PrefillSaturation: 0.375, PrefillSaturationKnown: true, DecodeSaturation: 0.875, DecodeSaturationKnown: true,
			DispatchedRate: 2.5, RejectedRate: 0.125, OutcomesKnown: true}},
	}
}

// The kind start-time recording, one golden per controller start (learner PR
// 5, "Recording" in analyzer-learner.md): the first golden in which a variant
// sets replicaStartSeconds, and the first recorded with the snapshot on.
const (
	startSeedFixture         = "start-seed"
	startSeedRestartFixture  = "start-seed-restart"
	startSeedRestart2Fixture = "start-seed-restart-2"
)

// startSeedFixtures is the three start-seed goldens, in the order they ran.
var startSeedFixtures = []string{startSeedFixture, startSeedRestartFixture, startSeedRestart2Fixture}

// r1Goldens is the goldens recorded with the R1 fields: the TTFT track's
// prefill-stress runs (P2), the start-seed runs, the throughput-sweep runs and
// the TTFT staircase.
var r1Goldens = append([]string{ttftThreePodsFixture, ttftSaturatedFixture, ttftOpenLongFixture, ttftOpenShortFixture,
	startSeedFixture, startSeedRestartFixture, startSeedRestart2Fixture, startRestoredFixture,
	throughputSweepFixture, throughputSweepRestartFixture}, staircaseFixtures...)

// ttftFieldNames are the R1 field names as a recording writes them.
var ttftFieldNames = []string{"QueueLengthKnown", "AvgServiceTimeKnown", "AvgTTFTKnown", "PromptTokensBuckets",
	"OutputTokensBuckets", "TTFTBuckets", "AvgQueueTime", "AvgQueueTimeKnown", "QueueTimeBuckets", "AvgPrefillTime",
	"AvgPrefillTimeKnown", "AvgKVTransferTime", "AvgKVTransferTimeKnown", "TPOTBuckets", "QueueMin", "QueueMinKnown",
	"RouterLatency", "FlowControlWaitBuckets", "FlowControlPools", "EffectiveSaturation", "PrefillSaturation",
	"DecodeSaturation", "DispatchedRate", "OutcomesKnown"}

// Part 3 (analyzer-learner.md, 2.9): every Analyze records the TTFT model's
// inputs with the rest of its input, and a replay reads them back and
// records them again byte for byte. With none set, none is written: the
// input of a recording made before R1.
func TestCoverage_EveryAnalyzeRecordsTheTTFTInputs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, kind string
		do         func(*fixture, domain.AnalyzerInput)
	}{
		{"saturation", saturation.KindAnalyze, func(f *fixture, in domain.AnalyzerInput) { _, _ = f.sat.Analyze(ctx, in) }},
		{"slo", slo.KindAnalyze, func(f *fixture, in domain.AnalyzerInput) { _, _ = f.slo.Analyze(ctx, in) }},
		{"throughput", throughput.KindAnalyze, func(f *fixture, in domain.AnalyzerInput) { _, _ = f.tp.Analyze(ctx, in) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, with := range []bool{true, false} {
				in := ttftInput()
				if !with {
					in = stripTTFT(in)
				}
				f := newFixture(replayAt)
				f.store.FileScaleTarget("ns", "m", "v", "H100", 1, capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64}, false, replayAt)
				f.buf.Reset()
				f.attach(learnrec.New(f.buf, 0))
				tc.do(f, in)
				events, err := readEvents(f.buf.Bytes())
				require.NoError(t, err)
				var raw learnrec.Raw
				for _, ev := range events {
					if ev.Kind == tc.kind {
						var d struct{ Input json.RawMessage }
						require.NoError(t, json.Unmarshal(ev.Data, &d))
						raw = learnrec.Raw(d.Input)
					}
				}
				require.NotNil(t, raw, "no %s event", tc.kind)
				for _, name := range ttftFieldNames {
					if with {
						assert.Contains(t, string(raw), `"`+name+`":`, "the input lost %s", name)
					} else {
						assert.NotContains(t, string(raw), `"`+name+`":`, "a zero %s was written", name)
					}
				}
				got, err := decodeInput(raw)
				require.NoError(t, err)
				again, err := learnrec.Marshal(learnrec.InputOf(got))
				require.NoError(t, err)
				assert.Equal(t, string(raw), string(again), "the replay records the input it read, byte for byte")
				if with {
					assert.Equal(t, in.RouterLatency, got.RouterLatency)
					assert.Equal(t, in.FlowControlPools, got.FlowControlPools)
					assert.Equal(t, in.ReplicaMetrics[0].TPOTBuckets, got.ReplicaMetrics[0].TPOTBuckets)
				}
			}
		})
	}
}

// stripTTFT is in with every R1 field zero.
func stripTTFT(in domain.AnalyzerInput) domain.AnalyzerInput {
	in.RouterLatency, in.FlowControlPools = nil, nil
	rows := make([]domain.ReplicaMetrics, len(in.ReplicaMetrics))
	for i, m := range in.ReplicaMetrics {
		m.QueueLengthKnown, m.AvgServiceTimeKnown, m.AvgTTFTKnown = false, false, false
		m.PromptTokensBuckets, m.OutputTokensBuckets = nil, nil
		m.TTFTBuckets, m.QueueTimeBuckets, m.TPOTBuckets = nil, nil, nil
		m.AvgQueueTime, m.AvgQueueTimeKnown = 0, false
		m.AvgPrefillTime, m.AvgPrefillTimeKnown = 0, false
		m.AvgKVTransferTime, m.AvgKVTransferTimeKnown = 0, false
		m.QueueMin, m.QueueMinKnown = 0, false
		rows[i] = m
	}
	in.ReplicaMetrics = rows
	return in
}

// olderZeros is again decoded, less every key recorded lacks whose value is
// a zero: a field added after recorded was written, which the codec writes
// now and an older build did not (a golden recorded before the policy's
// Passive, or before a row's LentSince or AvgOutputTokensSquared). The keys
// removed are added to dropped. Any other difference, a value changed or a
// key recorded holds and again lacks, is left for the comparison to find.
func olderZeros(t *testing.T, recorded, again []byte, dropped map[string]bool) (want, got any) {
	t.Helper()
	decode := func(b []byte) any {
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var v any
		require.NoError(t, d.Decode(&v))
		return v
	}
	want, got = decode(recorded), decode(again)
	var prune func(w, g any)
	prune = func(w, g any) {
		switch g := g.(type) {
		case map[string]any:
			wm, _ := w.(map[string]any)
			for k, gv := range g {
				wv, ok := wm[k]
				if !ok && isJSONZero(gv) {
					delete(g, k)
					dropped[k] = true
					continue
				}
				prune(wv, gv)
			}
		case []any:
			wl, _ := w.([]any)
			for i := range g {
				if i < len(wl) {
					prune(wl[i], g[i])
				}
			}
		}
	}
	prune(want, got)
	return want, got
}

// isJSONZero is whether v is what the codec writes for a zero Go value.
func isJSONZero(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case bool:
		return !v
	case string:
		return v == "" || v == "0001-01-01T00:00:00Z"
	case json.Number:
		f, err := v.Float64()
		return err == nil && f == 0
	case []any:
		return len(v) == 0
	case map[string]any:
		for _, e := range v {
			if !isJSONZero(e) {
				return false
			}
		}
		return true
	}
	return false
}

// requireSameUpToOlderZeros fails unless again is recorded written again,
// up to olderZeros, and holds no R1 field: those are left out while zero.
func requireSameUpToOlderZeros(t *testing.T, recorded, again []byte, dropped map[string]bool, msg string, args ...any) {
	t.Helper()
	if string(recorded) == string(again) {
		return
	}
	want, got := olderZeros(t, recorded, again, dropped)
	require.Equal(t, want, got, append([]any{msg}, args...)...)
}

// A recording made before R1 replays unchanged: every committed golden was
// recorded before R1, and each Analyze input it holds, each slo event with
// the router latency it served, and each row the throughput analyzer
// observed reads back and is written again with the same values, and with no
// R1 field. That is what a replay of it records, so the new fields stay
// absent from it and move no output. Byte for byte, except a golden recorded
// by a build older than a field added before R1, which a re-encoding writes
// as its zero (olderZeros); those keys are logged. The goldens recorded with
// R1 (r1Goldens) are written again byte for byte too, with their R1 fields.
func TestCoverage_PreR1GoldensRecordTheSameBytes(t *testing.T) {
	m, err := ReadManifest(goldenDir)
	require.NoError(t, err)
	require.NotEmpty(t, m.Goldens)
	inputs, r1Inputs := 0, 0
	dropped := map[string]bool{}
	r1Held := map[string]bool{}
	for _, g := range m.Goldens {
		gz, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(g.Recording)))
		require.NoError(t, err, g.Name)
		recording, err := gunzip(gz)
		require.NoError(t, err, g.Name)
		read, err := learnrec.Read(recording)
		require.NoError(t, err, g.Name)
		for _, ev := range read.Events {
			var input learnrec.Raw
			switch ev.Kind {
			case saturation.KindAnalyze:
				var d saturation.AnalyzeEvent
				require.NoError(t, ev.DecodeData(&d), g.Name)
				input = d.Input
			case slo.KindAnalyze:
				var d slo.AnalyzeEvent
				require.NoError(t, ev.DecodeData(&d), g.Name)
				input = d.Input
				again, err := learnrec.Marshal(d)
				require.NoError(t, err)
				requireSameUpToOlderZeros(t, ev.Data, again, dropped, "%s seq %d: the slo event", g.Name, ev.Seq)
			case throughput.KindAnalyze:
				var d throughput.AnalyzeEvent
				require.NoError(t, ev.DecodeData(&d), g.Name)
				input = d.Input
			case throughput.KindObserve:
				var d throughput.ObserveEvent
				require.NoError(t, ev.DecodeData(&d), g.Name)
				var rows []domain.ReplicaMetrics
				require.NoError(t, d.Metrics.Decode(&rows), g.Name)
				again, err := learnrec.Marshal(rows)
				require.NoError(t, err)
				requireSameUpToOlderZeros(t, d.Metrics, again, dropped, "%s seq %d: the observed rows", g.Name, ev.Seq)
				for _, name := range ttftFieldNames {
					require.NotContains(t, string(again), `"`+name+`":`, "%s seq %d: a zero %s was written", g.Name, ev.Seq, name)
				}
				continue
			default:
				continue
			}
			in, err := decodeInput(input)
			require.NoError(t, err, g.Name)
			again, err := learnrec.Marshal(learnrec.InputOf(in))
			require.NoError(t, err)
			requireSameUpToOlderZeros(t, input, again, dropped, "%s seq %d: the input", g.Name, ev.Seq)
			inputs++
			if slices.Contains(r1Goldens, g.Name) {
				// Recorded with R1: its fields are present, and the re-encoding
				// above must still write them back byte for byte.
				r1Inputs++
				for _, name := range ttftFieldNames {
					if strings.Contains(string(input), `"`+name+`":`) {
						r1Held[g.Name] = true
						break
					}
				}
				continue
			}
			for _, name := range ttftFieldNames {
				require.NotContains(t, string(input), `"`+name+`":`, "%s seq %d: a golden recorded before R1 holds %s", g.Name, ev.Seq, name)
				require.NotContains(t, string(again), `"`+name+`":`, "%s seq %d: a zero %s was written", g.Name, ev.Seq, name)
			}
		}
	}
	// An empty walk passes every check above.
	require.Greater(t, inputs, 100, "the walk read %d inputs", inputs)
	require.Greater(t, inputs-r1Inputs, 100, "the walk read %d inputs recorded before R1", inputs-r1Inputs)
	require.Positive(t, r1Inputs, "no input of the goldens recorded with R1")
	// Each listed golden must hold an R1 field, so the list exempts only
	// goldens that need it.
	for _, name := range r1Goldens {
		require.True(t, r1Held[name], "%s is listed in r1Goldens and holds no R1 field", name)
	}
	for _, name := range ttftFieldNames {
		require.False(t, dropped[name], "R1 field %s was written as a zero", name)
	}
	t.Logf("%d goldens, %d inputs written again; zero fields older goldens lack: %v", len(m.Goldens), inputs, slices.Sorted(maps.Keys(dropped)))
}
