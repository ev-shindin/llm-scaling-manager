package learnreplay

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
		readers: []string{"Name", "Horizons", "BorrowTickGap", "PathCounts", "LearnerReports"},
		// WithSettleView hands k2 the engine's settle tracker; the replay hands
		// it its own, observed from the settle events (settle.go).
		builders: []string{"WithHorizons", "WithOptimizationInterval", "WithClock", "WithRecorder", "WithSettleView"},
		seams:    []string{"SetSwitchesForReplay"},
	},
	reflect.TypeFor[*slo.Analyzer](): {
		recorded: []string{"Analyze", "EvictStale", "BeginCycle", "NoteVariantsPresent"},
		readers:  []string{"Name", "StateTimeout", "SlopeHoldFor", "Judging"},
		builders: []string{"WithClock", "WithStateTimeout", "WithSlopeHoldFor", "WithRecorder"},
		seams:    []string{"SetSwitchesForReplay"},
	},
	reflect.TypeFor[*throughput.ThroughputAnalyzer](): {
		recorded: []string{"Analyze", "Observe"},
		readers:  []string{"Name", "VariantState"},
		builders: []string{"WithClock", "WithRecorder"},
		seams:    []string{"SetSwitchesForReplay"},
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
		slo:   slo.New(nilRouter{}, store).WithClock(clk).WithRecorder(rec),
		tp:    throughput.NewThroughputAnalyzer().WithClock(clk).WithRecorder(rec),
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
	}
	for pkg, hook := range hooks {
		assert.NoError(t, hook(nil), pkg)
		assert.NoError(t, hook(map[string]bool{}), pkg)
		assert.Error(t, hook(map[string]bool{"no-such-switch": true}), pkg)
	}
}

// No production path calls a switch hook: only this package, and tests, may.
func TestSwitchHooks_NoProductionPathCallsThem(t *testing.T) {
	root := moduleRoot(t)
	var callers []string
	files, defined := 0, 0
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
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "SetSwitchesForReplay" {
				defined++
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetSwitchesForReplay" {
					callers = append(callers, rel)
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	// The walk reached the four hooks themselves, so it read the packages
	// that define them, and the rest of the tree with them.
	require.Equal(t, 4, defined, "the walk found %d SetSwitchesForReplay definitions in %d files", defined, files)
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
