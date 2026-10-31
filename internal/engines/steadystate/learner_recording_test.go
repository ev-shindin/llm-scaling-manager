package steadystate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// The recorder's acceptance (docs/proposals/analyzer-learner.md, 2.4): a
// scripted multi-cycle engine run, recorded, then replayed at the same
// commit, reproduces every recorded result byte for byte.
//
// The run is the engine's own per-cycle sequence -- the slo cycle, the
// snapshot restore, the presence stamps, the sweep, then runAnalyzers per
// model (which forgets on request, files each scale target in the store and
// runs every listed analyzer), then the snapshot write -- driven with
// scripted rows instead of a Prometheus query. It covers several models, a
// controller restart that restores a snapshot, an ITL line one variant
// publishes and a sibling borrows, a forget on request, sweeps that evict, a
// P/D model whose decode reads the deferred queue, the slo analyzer judging
// with a router, and the throughput analyzer.

const recNS = "rec"

// recVariant is one variant of the script.
type recVariant struct {
	model, name, role string
	gpus              int
	// rows builds the variant's rows for a cycle at the given instant.
	rows func(cycle int, at time.Time) []domain.ReplicaMetrics
}

// recScript is the fleet the scripted run drives, and the clock it runs on.
type recScript struct {
	base     *clocktesting.FakePassiveClock
	k8s      client.Client
	variants []recVariant
	// router is what the slo analyzer reads as the router's latency.
	router *scriptedRouter
	policy config.ScalingPolicy
	// forgetAt is the cycle the forget annotation appears on agg-a, and
	// goneAfter the last cycle the "gone" model is present.
	forgetAt, goneAfter int
	// engineLayer runs each model through collectV2ModelRequest, the
	// analyzers and then the composite, with discovery metadata, instead
	// of the analyzers alone: the engine-level recording (#345 step 0).
	engineLayer bool
	// policies is a model's own policy, where it is not policy.
	policies map[string]config.ScalingPolicy
	// external is an external analyzer each controller registers, nil for
	// none.
	external domain.Analyzer
	// failModel's collection fails on cycle failAt: the engine holds its
	// settle sightings (modelCollectionFailed) and nothing analyzes it. ""
	// for none.
	failModel string
	failAt    int
}

// scriptedRouter answers with a latency that moves with the cycle.
type scriptedRouter struct{ cycle int }

func (r *scriptedRouter) CollectRouterLatency(_ context.Context, modelID, _ string) *domain.RouterLatency {
	if modelID != "acme/agg" {
		return nil
	}
	rate := 2.0 + float64(r.cycle%3)
	return &domain.RouterLatency{
		WindowSeconds: 300,
		TPOTSumRate:   rate * (0.03 + 0.001*float64(r.cycle%5)),
		TPOTCountRate: rate,
		TTFTBuckets: []domain.LatencyBucket{
			{UpperBound: 0.5, Rate: rate * 0.6}, {UpperBound: 1, Rate: rate * 0.9}, {UpperBound: 2, Rate: rate},
		},
	}
}

const recITLA, recITLB = 0.02, 0.006

// recRow is one Ready replica at fill k with every presence bit set.
func recRow(model, variant, pod string, k float64, at time.Time) domain.ReplicaMetrics {
	const kvCap = 100_000
	itl := recITLB + recITLA*k
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: variant, Namespace: recNS, ModelID: model,
		Ready: true, ReadySince: at.Add(-30 * time.Minute), StartSeconds: 75, Revision: "r1",
		KvCacheUsage: k, KvCacheUsageKnown: true, KvUsageInstant: k, KvUsageInstantKnown: true,
		MaxRankKvCacheUsage: k, EngineRanks: 1,
		NumGpuBlocks: kvCap / 16, BlockSize: 16, TotalKvCapacityTokens: kvCap,
		TokensInUse: int64(k * kvCap), RunningRequests: int(k * 40), RunningRequestsKnown: true,
		AvgInputTokens: 900, AvgOutputTokens: 300, AvgInputTokensRecent: 900, AvgOutputTokensRecent: 300,
		PrefixCacheHitRate: 0.1, AvgServiceTime: 2, AvgTTFT: 0.2,
		AvgITL: itl, AvgITLKnown: true,
		GenerationTokenRate: k * 40 / itl, GenerationTokenRateKnown: true,
		PrefillComputedTokenRate: 2000, PrefillComputedTokenRateKnown: true,
		RequestRate: k * 40 / itl / 300, RequestRateKnown: true,
		CapacityQueueKnown: true, PreemptionRateKnown: true,
	}
}

func newRecScript(start time.Time) *recScript {
	s := &recScript{
		base:   clocktesting.NewFakePassiveClock(start),
		router: &scriptedRouter{},
		// Reuse on, so agg-b borrows the line agg-a publishes; slo and
		// throughput listed, so both run.
		policy: config.ScalingPolicy{
			KvCacheThreshold: 0.8, QueueLengthThreshold: 5, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7,
			EnableLearnedStateReuse: true,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: "saturation"}, {Name: "slo"}, {Name: "throughput"},
			},
			SLO: &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 40 * time.Millisecond}}},
		},
		forgetAt:  6,
		goneAfter: 3,
	}
	s.variants = []recVariant{
		{model: "acme/agg", name: "agg-a", gpus: 1, rows: func(cycle int, at time.Time) []domain.ReplicaMetrics {
			// Ten replicas spread across the band: enough for a full ITL fit.
			out := make([]domain.ReplicaMetrics, 0, 10)
			for j := range 10 {
				k := 0.30 + 0.04*float64(j) + 0.002*float64(cycle%4)
				rm := recRow("acme/agg", "agg-a", fmt.Sprintf("agg-a-%d", j), k, at)
				if j >= 8 && cycle%3 == 0 {
					// The busiest two full and queued: a saturated reading.
					rm.KvCacheUsage, rm.MaxRankKvCacheUsage, rm.QueueLength, rm.CapacityQueueLength = 0.92, 0.92, 3, 3
					rm.TokensInUse = 92_000
				}
				out = append(out, rm)
			}
			return out
		}},
		{model: "acme/agg", name: "agg-b", gpus: 1, rows: func(cycle int, at time.Time) []domain.ReplicaMetrics {
			// Below the ITL band: the slo analyzer has nothing to fit for it.
			return []domain.ReplicaMetrics{
				recRow("acme/agg", "agg-b", "agg-b-0", 0.05+0.01*float64(cycle%2), at),
				recRow("acme/agg", "agg-b", "agg-b-1", 0.06, at),
			}
		}},
		{model: "acme/pd", name: "pd-p", role: domain.RolePrefill, gpus: 1, rows: func(_ int, at time.Time) []domain.ReplicaMetrics {
			rm := recRow("acme/pd", "pd-p", "pd-p-0", 0.2, at)
			rm.AvgITL, rm.AvgITLKnown, rm.AvgITLEmpty = 0, true, true
			return []domain.ReplicaMetrics{rm}
		}},
		{model: "acme/pd", name: "pd-d", role: domain.RoleDecode, gpus: 1, rows: func(cycle int, at time.Time) []domain.ReplicaMetrics {
			// Eight replicas across the ITL band, which the decode role fits
			// a line from and publishes for its configuration; two full, with
			// requests waiting that the engine has only deferred, so the
			// decode rewrite reads their own queue, not the raw count.
			out := make([]domain.ReplicaMetrics, 0, 10)
			for j := range 8 {
				k := 0.20 + 0.05*float64(j) + 0.002*float64(cycle%4)
				out = append(out, recRow("acme/pd", "pd-d", fmt.Sprintf("pd-d-%d", j), k, at))
			}
			for j := range 2 {
				rm := recRow("acme/pd", "pd-d", fmt.Sprintf("pd-d-%d", 8+j), 0.85, at)
				rm.QueueLength, rm.MaxRankQueueLength = 4+cycle%2, 4+cycle%2
				rm.CapacityQueueLength, rm.MaxRankCapacityQueueLength = j, j
				rm.TokensInUse = 85_000
				out = append(out, rm)
			}
			return out
		}},
		{model: "acme/pd", name: "pd-d2", role: domain.RoleDecode, gpus: 1, rows: func(cycle int, at time.Time) []domain.ReplicaMetrics {
			// Below the ITL band: nothing of its own to fit, so it borrows
			// the line pd-d published for the same configuration.
			return []domain.ReplicaMetrics{
				recRow("acme/pd", "pd-d2", "pd-d2-0", 0.05+0.01*float64(cycle%2), at),
				recRow("acme/pd", "pd-d2", "pd-d2-1", 0.06, at),
			}
		}},
		{model: "acme/gone", name: "gone-a", gpus: 1, rows: func(_ int, at time.Time) []domain.ReplicaMetrics {
			return []domain.ReplicaMetrics{recRow("acme/gone", "gone-a", "gone-a-0", 0.4, at)}
		}},
	}
	objs := make([]client.Object, 0, len(s.variants))
	for _, v := range s.variants {
		objs = append(objs, recDeployment(v, ""))
	}
	s.k8s = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	return s
}

// recDeployment is a variant's Deployment: one engine configuration for the
// two aggregated variants, so they share an ITL line.
func recDeployment(v recVariant, forgetToken string) *appsv1.Deployment {
	args := []string{"--max-num-seqs=64", "--gpu-memory-utilization=0.9", "--block-size=16"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: v.name, Namespace: recNS},
		Spec: appsv1.DeploymentSpec{
			Replicas: utils.Ptr(int32(2)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeSelector: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
				Containers: []corev1.Container{{Name: "vllm", Image: "vllm/vllm-openai:v0.11.0",
					Command: []string{"vllm", "serve", v.model}, Args: args,
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						"nvidia.com/gpu": resource.MustParse(strconv.Itoa(v.gpus)),
					}}}},
			}},
		},
		Status: appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2, ObservedGeneration: 0},
	}
	if forgetToken != "" {
		d.Annotations = map[string]string{annotations.ForgetLearnedState: forgetToken}
	}
	return d
}

func (v recVariant) va() *llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	return &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: v.name, Namespace: recNS},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ModelID:        v.model,
			MaxReplicas:    8,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: v.name, APIVersion: "apps/v1"},
		},
	}
}

// engine builds a controller over the script's cluster: what a controller
// start builds, with the slo analyzer judging through the script's router
// and the throughput analyzer registered as cmd/main.go registers it.
func (s *recScript) engine(t *testing.T) *Engine {
	t.Helper()
	cfg := config.NewTestConfig()
	config.SetLearnedStateSnapshotForTest(cfg, true)
	cfg.UpdateScalingPolicyConfigForNamespace(recNS, map[string]config.ScalingPolicy{config.GlobalDefaultsKey: s.policy})
	registry := source.NewSourceRegistry()
	registry.MustRegister("prometheus", replicaSource{modelID: "acme/agg"})
	e := newEngine(s.k8s, s.k8s, clientgoscheme.Scheme, nil, registry, cfg, allocation.NewNoOpLimiter("test"), s.base)
	sat := e.saturationAnalyzer().(*saturation.SaturationAnalyzer)
	judging := slo.New(s.router, e.capacityStore).WithClock(e.clock).
		WithStateTimeout(sat.Horizons().VariantTimeout).WithSlopeHoldFor(sat.Horizons().BucketRetention)
	e.sloAnalyzer = judging
	e.analyzers.replaceBuiltin(domain.SLOAnalyzerName, judging)
	require.NoError(t, e.RegisterAnalyzer(throughput.AnalyzerName, throughput.NewThroughputAnalyzer().WithClock(e.Clock())))
	if s.external != nil {
		e.UpsertExternalAnalyzer(s.external.Name(), s.external)
	}
	return e
}

// cycle runs one engine cycle at the given instant, in optimizeV2's order,
// and returns every model's named results.
func (s *recScript) cycle(t *testing.T, e *Engine, n int, at time.Time) []allocation.NamedAnalyzerResult {
	t.Helper()
	ctx := context.Background()
	s.base.SetTime(at)
	s.router.cycle = n
	e.beginSLOCycle()
	release := e.clock.Pin()
	defer release()
	now := e.clock.Now()
	e.restoreLearnedState(ctx, now)

	byModel := map[string][]recVariant{}
	for _, v := range s.variants {
		if v.model == "acme/gone" && n > s.goneAfter {
			continue
		}
		byModel[v.model] = append(byModel[v.model], v)
	}
	models := make([]string, 0, len(byModel))
	groups := map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
	for m, vs := range byModel {
		models = append(models, m)
		for _, v := range vs {
			groups[utils.GetNamespacedKey(recNS, m)] = append(groups[utils.GetNamespacedKey(recNS, m)], *v.va())
		}
	}
	sort.Strings(models)
	e.notePresentVariants(ctx, groups)
	e.evictStaleLearnedState(ctx)

	out := make([]allocation.NamedAnalyzerResult, 0, 3*len(models))
	for _, m := range models {
		if m == s.failModel && n == s.failAt {
			e.modelCollectionFailed(recNS, m)
			continue
		}
		rows := make([]domain.ReplicaMetrics, 0, 16)
		states := make([]domain.VariantReplicaState, 0, len(byModel[m]))
		targets := map[string]scaletarget.ScaleTargetAccessor{}
		vas := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
		for _, v := range byModel[m] {
			vr := v.rows(n, now)
			rows = append(rows, vr...)
			states = append(states, domain.VariantReplicaState{
				VariantName: v.name, CurrentReplicas: len(vr), DesiredReplicas: len(vr),
				GPUsPerReplica: v.gpus, Role: v.role, AcceleratorName: "NVIDIA-H100-80GB-HBM3",
			})
			token := ""
			if v.name == "agg-a" && n >= s.forgetAt {
				token = "forget-1"
			}
			targets[utils.GetNamespacedKey(recNS, v.name)] = scaletarget.NewDeploymentAccessor(recDeployment(v, token))
			vas[v.name] = v.va()
		}
		var sq *domain.SchedulerQueueMetrics
		if m == "acme/agg" {
			sq = &domain.SchedulerQueueMetrics{QueueSize: int64(n % 3)}
		}
		policy := s.policy
		if p, ok := s.policies[m]; ok {
			policy = p
		}
		if s.engineLayer {
			req, err := e.collectV2ModelRequest(ctx, m, recNS, rows, policy, states, recMetadata(states, m, n),
				targets, vas, sq, 1.5+0.1*float64(n%4))
			require.NoError(t, err, "cycle %d model %s", n, m)
			out = append(out, req.CompositeSignal)
			continue
		}
		results, err := e.runAnalyzers(ctx, m, recNS, rows, policy, states, nil, targets, vas, sq, 1.5+0.1*float64(n%4))
		require.NoError(t, err, "cycle %d model %s", n, m)
		out = append(out, results...)
	}
	e.writeLearnedState(ctx, now)
	return out
}

// recSchedule is the instants of the restarted controller's cycles: 15 s
// apart, then a day's gap that ages the departed model past the variant
// timeout, then eight days that age its store records past theirs.
func recSchedule(start time.Time) []time.Time {
	out := make([]time.Time, 0, 12)
	at := start
	for i := range 12 {
		if i == 9 {
			at = at.Add(25 * time.Hour)
		}
		if i == 11 {
			at = at.Add(8 * 24 * time.Hour)
		}
		out = append(out, at)
		at = at.Add(15 * time.Second)
	}
	return out
}

// recordRun runs the script: a first controller that learns and writes its
// snapshot, then a restarted one, recorded, that restores it and runs the
// schedule. It returns the recording, or nil with recording off, and every
// result the restarted controller's analyzers returned.
func recordRun(t *testing.T, start time.Time, record bool) ([]byte, [][]allocation.NamedAnalyzerResult) {
	t.Helper()
	recording, results, _ := recordRunEngine(t, start, record)
	return recording, results
}

// recordRunEngine is recordRun, with the engine that recorded.
func recordRunEngine(t *testing.T, start time.Time, record bool) ([]byte, [][]allocation.NamedAnalyzerResult, *Engine) {
	t.Helper()
	s := newRecScript(start)
	first := s.engine(t)
	// Long enough apart for each snapshot write to land.
	for i := range 4 {
		s.cycle(t, first, i, start.Add(time.Duration(i)*2*time.Minute))
	}
	restartAt := start.Add(30 * time.Minute)
	second := s.engine(t)
	var buf bytes.Buffer
	if record {
		second.recordLearners(learnrec.New(&buf, 0))
	}
	results := make([][]allocation.NamedAnalyzerResult, 0, 12)
	for i, at := range recSchedule(restartAt) {
		results = append(results, s.cycle(t, second, i, at))
	}
	if !record {
		return nil, results, second
	}
	require.NoError(t, second.learnRec.Err())
	return buf.Bytes(), results, second
}

// Review r2 (S2): a cycle whose collection fails records a settle.Hold, and
// a replay at the same commit applies it and records it again byte for
// byte, its settle verdicts the engine's. A short run of its own: the
// scripted run's pinned counts stay as they are. A recording made before the
// settle events has no hold to apply; its fallback cannot hold through such
// a cycle, which is a stated limit (learnreplay/settle.go).
func TestLearnerRecording_AFailedCollectionIsRecordedAsAHold(t *testing.T) {
	s := newRecScript(recStart)
	s.failModel, s.failAt = "acme/agg", 2
	e := s.engine(t)
	var buf bytes.Buffer
	e.recordLearners(learnrec.New(&buf, 0))
	for i, at := range recSchedule(recStart)[:5] {
		s.cycle(t, e, i, at)
	}
	require.NoError(t, e.learnRec.Err())
	recording := buf.Bytes()
	require.Equal(t, 1, kindCounts(t, recording)[settle.KindHold], "the failed cycle records one hold")

	rep, err := learnreplay.Replay(context.Background(), recording, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, mismatchText(rep.Mismatches), "the replay diverged from the recording")
	require.Equal(t, string(recording), string(rep.Recording), "the replay's recording")
	assertSettleVerdicts(t, "a recording with a hold", e.settleTracker().Verdicts(), rep.Paths)
}

// assertSettleVerdicts holds a replay's settle counters to the engine's
// verdicts, reason by reason.
func assertSettleVerdicts(t *testing.T, what string, want map[settle.Reason]int64, paths map[string]int64) {
	t.Helper()
	total := int64(0)
	for r, n := range want {
		total += n
		assert.Equal(t, n, paths["settle-"+string(r)], "%s: settle verdict %s", what, r)
	}
	assert.Positive(t, total, "%s: the engine reached no settle verdict", what)
}

// withoutKind is recording with every event of kind left out, and the rest
// numbered again from 1, as a recording that never had them is.
func withoutKind(t *testing.T, recording []byte, kind string) []byte {
	t.Helper()
	const prefix = `{"Seq":`
	var out []byte
	seq := 0
	for _, line := range bytes.SplitAfter(recording, []byte("\n")) {
		if len(line) == 0 || bytes.Contains(line, []byte(`"Kind":"`+kind+`"`)) {
			continue
		}
		require.True(t, bytes.HasPrefix(line, []byte(prefix)), "a line that does not start with its sequence number")
		end := bytes.IndexByte(line, ',')
		require.Positive(t, end)
		seq++
		out = append(out, fmt.Sprintf("%s%d", prefix, seq)...)
		out = append(out, line[end:]...)
	}
	return out
}

// kindCounts counts a recording's events by kind.
func kindCounts(t *testing.T, recording []byte) map[string]int {
	t.Helper()
	events, err := readEvents(recording)
	require.NoError(t, err)
	out := map[string]int{}
	for _, ev := range events {
		out[ev.Kind]++
	}
	return out
}

var recStart = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

func TestLearnerRecording_AReplayAtTheSameCommitReproducesEveryResult(t *testing.T) {
	recording, _, recorder := recordRunEngine(t, recStart, true)

	// The run covered what the acceptance names; a recording that missed one
	// would pass a replay without testing it.
	// Pinned, so a change in what the run records is a reviewed change: 12
	// cycles over 3 models (the departed one for the first 4), 28 Analyzes
	// per analyzer, 5 variants filed every cycle and the departed one for 4.
	// The engine observes the settle tracker once per model per cycle, before
	// its analyzers: 28 observations.
	kinds := kindCounts(t, recording)
	require.Equal(t, map[string]int{
		learnrec.KindStart:                  1,
		saturation.KindAnalyze:              28,
		slo.KindAnalyze:                     28,
		throughput.KindAnalyze:              28,
		capacity.KindFileScaleTarget:        64,
		capacity.KindEvictStale:             12,
		saturation.KindRestoreLearnedState:  1,
		saturation.KindForgetVariant:        1,
		saturation.KindForgottenToken:       6,
		saturation.KindEvictStaleHistory:    12,
		saturation.KindNoteVariantsPresent:  28,
		saturation.KindHoldLowerBounds:      2,
		saturation.KindExportLearnedState:   5,
		saturation.KindTakeRestoreOutcomes:  12,
		saturation.KindStableAcceleratorFor: 64,
		saturation.KindPublishLine:          9,
		saturation.KindBorrowLine:           11,
		slo.KindBeginCycle:                  12,
		slo.KindEvictStale:                  12,
		slo.KindNoteVariantsPresent:         28,
		settle.KindObserve:                  28,
	}, kinds, "the events the scripted run records")
	t.Logf("recorded %d bytes: %v", len(recording), kinds)
	assertResultsPriced(t, recording)
	assertSweepsEvicted(t, recording)
	assertRouterServed(t, recording)
	assertDecodeRewriteRecordedRaw(t, recording)

	// Three replays in one process: each its own analyzers, each byte for
	// byte.
	for i := range 3 {
		rep, err := learnreplay.Replay(context.Background(), recording, learnreplay.Options{Mode: learnreplay.Fidelity})
		require.NoError(t, err)
		require.Empty(t, mismatchText(rep.Mismatches), "replay %d diverged from the recording", i+1)
		require.Equal(t, string(recording), string(rep.Recording), "replay %d: the replay's recording", i+1)
		assert.Equal(t, 128, rep.StoreChecks, "store checks")
		assert.Equal(t, kinds[saturation.KindAnalyze], rep.Analyzes[saturation.KindAnalyze])
		assert.Equal(t, kinds[slo.KindAnalyze], rep.Analyzes[slo.KindAnalyze])
		assert.Equal(t, kinds[throughput.KindAnalyze], rep.Analyzes[throughput.KindAnalyze])
	}

	// Every replay that is not at the recording's commit runs in Normal mode;
	// at the same commit it must find nothing to fail on either.
	rep, err := learnreplay.Replay(context.Background(), recording, learnreplay.Options{Mode: learnreplay.Normal})
	require.NoError(t, err)
	require.Empty(t, mismatchText(rep.Failed()), "a Normal replay at the same commit failed")

	// Review r1 (S4): the replay's settle tracker reaches the engine's
	// verdicts, from the recorded observations, and, with them stripped as a
	// recording made before they existed would be, from the fallback that
	// observes at each saturation Analyze's clock: over three models and
	// many replicas, not one.
	engineVerdicts := recorder.settleTracker().Verdicts()
	assertSettleVerdicts(t, "recorded observations", engineVerdicts, rep.Paths)
	stripped := withoutKind(t, recording, settle.KindObserve)
	require.Less(t, len(stripped), len(recording), "precondition: the recording has settle observations")
	old, err := learnreplay.Replay(context.Background(), stripped, learnreplay.Options{Mode: learnreplay.Normal})
	require.NoError(t, err)
	require.Empty(t, mismatchText(old.Failed()), "a replay with the observations stripped failed")
	assertSettleVerdicts(t, "observations stripped", engineVerdicts, old.Paths)
	assert.Equal(t, 128, rep.StoreChecks, "store checks, Normal")
	assert.Equal(t, kinds[saturation.KindAnalyze], rep.Analyzes[saturation.KindAnalyze])

	// What is measured, for the size the docs state: bytes per replica per
	// cycle over the recorded controller's cycles.
	replicaCycles := 0
	events, err := readEvents(recording)
	require.NoError(t, err)
	byKind := map[string]int{}
	for _, ev := range events {
		byKind[ev.Kind] += len(ev.Line) + 1
		if ev.Kind == saturation.KindAnalyze {
			var d saturation.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			var in learnrec.Input
			require.NoError(t, d.Input.Decode(&in))
			replicaCycles += len(in.ReplicaMetrics)
		}
	}
	t.Logf("measured: %d bytes over %d replica-cycles, %.0f bytes per replica per cycle",
		len(recording), replicaCycles, float64(len(recording))/float64(replicaCycles))
	t.Logf("bytes by kind: %v", byKind)
}

// Every event decodes into its own type and encodes back to the bytes it
// was written with: a replay reads exactly what the run wrote.
func TestLearnerRecording_EveryEventRoundTripsByteForByte(t *testing.T) {
	recording, _ := recordRun(t, recStart, true)
	events, err := readEvents(recording)
	require.NoError(t, err)
	types := map[string]func() any{
		learnrec.KindStart:                  func() any { return &learnrec.Start{} },
		saturation.KindAnalyze:              func() any { return &saturation.AnalyzeEvent{} },
		saturation.KindNoteVariantsPresent:  func() any { return &saturation.PresentEvent{} },
		saturation.KindEvictStaleHistory:    func() any { return &saturation.Evicted{} },
		saturation.KindForgetVariant:        func() any { return &saturation.ForgetVariantEvent{} },
		saturation.KindForgottenToken:       func() any { return &saturation.ForgottenTokenEvent{} },
		saturation.KindHoldLowerBounds:      func() any { return &saturation.HoldEvent{} },
		saturation.KindExportLearnedState:   func() any { return &saturation.LearnedState{} },
		saturation.KindRestoreLearnedState:  func() any { return &saturation.RestoreEvent{} },
		saturation.KindTakeRestoreOutcomes:  func() any { return &map[string]int{} },
		saturation.KindStableAcceleratorFor: func() any { return &saturation.AcceleratorEvent{} },
		saturation.KindPublishLine:          func() any { return &saturation.LineEvent{} },
		saturation.KindBorrowLine:           func() any { return &saturation.LineEvent{} },
		slo.KindAnalyze:                     func() any { return &slo.AnalyzeEvent{} },
		slo.KindEvictStale:                  func() any { return &slo.EvictStaleEvent{} },
		slo.KindBeginCycle:                  func() any { return &struct{}{} },
		slo.KindNoteVariantsPresent:         func() any { return &slo.PresentEvent{} },
		throughput.KindAnalyze:              func() any { return &throughput.AnalyzeEvent{} },
		capacity.KindFileScaleTarget:        func() any { return &capacity.FileScaleTargetEvent{} },
		capacity.KindEvictStale:             func() any { return &capacity.EvictStaleEvent{} },
		settle.KindObserve:                  func() any { return &settle.ObserveEvent{} },
		settle.KindHold:                     func() any { return &settle.ObserveEvent{} },
	}
	checked := map[string]int{}
	for _, ev := range events {
		mk, ok := types[ev.Kind]
		require.True(t, ok, "no type for %s", ev.Kind)
		v := mk()
		require.NoError(t, ev.DecodeData(v), "seq %d %s", ev.Seq, ev.Kind)
		b, err := learnrec.Marshal(v)
		require.NoError(t, err)
		require.Equal(t, string(ev.Data), string(b), "seq %d %s does not encode back to its bytes", ev.Seq, ev.Kind)
		checked[ev.Kind]++
	}
	// The raw input and records inside an Analyze, decoded all the way down.
	for _, ev := range events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		var in struct {
			ModelID, Namespace string
			ReplicaMetrics     []domain.ReplicaMetrics
			VariantStates      []domain.VariantReplicaState
			Config             *config.ScalingPolicy
			ConfigType         string
			SchedulerQueue     *domain.SchedulerQueueMetrics
			ArrivalRate        float64
		}
		require.NoError(t, d.Input.Decode(&in))
		b, err := learnrec.Marshal(in)
		require.NoError(t, err)
		require.Equal(t, string(d.Input), string(b), "seq %d: the input does not encode back to its bytes", ev.Seq)
		var recs map[string]*capacity.Record
		require.NoError(t, d.Records.Decode(&recs))
		b, err = learnrec.Marshal(recs)
		require.NoError(t, err)
		require.Equal(t, string(d.Records), string(b), "seq %d: the records do not encode back to their bytes", ev.Seq)
		checked["input"]++
	}
	assert.Equal(t, len(events)+checked["input"], sum(checked))
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// An optimizer block in the policy, which keeps a decode error behind an
// unexported field, is recorded, and replays.
func TestLearnerRecording_APolicyWithAnOptimizerRecords(t *testing.T) {
	s := newRecScript(recStart)
	s.policy.Optimizer = &config.OptimizerConfig{Type: "utilization-share"}
	e := s.engine(t)
	var buf bytes.Buffer
	e.recordLearners(learnrec.New(&buf, 0))
	s.cycle(t, e, 0, recStart)
	require.NoError(t, e.learnRec.Err(), "the recording stopped on the policy")
	kinds := kindCounts(t, buf.Bytes())
	require.Equal(t, 3, kinds[saturation.KindAnalyze], "analyzes, optimizer run")
	rep, err := learnreplay.Replay(context.Background(), buf.Bytes(), learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, mismatchText(rep.Failed()))
}

// Stopping the loop closes the recording and takes it back from the store
// and the analyzers; starting the loop again in the same process opens no
// second one, and nothing writes to the first.
func TestLearnerRecording_StartStopStart(t *testing.T) {
	s := newRecScript(recStart)
	e := s.engine(t)
	dir := recordingDir(t)
	e.LearnerRecording = learnrec.Options{Dir: dir}
	e.startLearnerRecording()
	require.NotNil(t, e.learnRec)
	s.cycle(t, e, 0, recStart)
	e.stopLearnerRecording()
	require.Nil(t, e.learnRec, "stop left the recording on the engine")

	files, err := filepathGlob(dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	before := files[0].data

	e.startLearnerRecording()
	assert.Nil(t, e.learnRec, "a second start in one process opened a recording")
	s.cycle(t, e, 1, recStart.Add(15*time.Second))
	e.stopLearnerRecording()

	files, err = filepathGlob(dir)
	require.NoError(t, err)
	require.Len(t, files, 1, "a second recording was opened")
	assert.Equal(t, string(before), string(files[0].data), "something wrote to the closed recording")
	events, err := readEvents(before)
	require.NoError(t, err)
	// 36, and the settle observation of each of the cycle's 3 models.
	assert.Len(t, events, 39, "events of the one recorded cycle")
}

// The same run thirty days later records the same results: nothing an
// analyzer returns reads the wall clock, and the replay of each is exact.
func TestLearnerRecording_ThirtyDaysLaterTheResultsAreTheSame(t *testing.T) {
	now, _ := recordRun(t, recStart, true)
	later, _ := recordRun(t, recStart.Add(30*24*time.Hour), true)
	for _, rec := range [][]byte{now, later} {
		rep, err := learnreplay.Replay(context.Background(), rec, learnreplay.Options{Mode: learnreplay.Fidelity})
		require.NoError(t, err)
		require.Empty(t, mismatchText(rep.Mismatches))
	}
	a, b := analyzeResults(t, now), analyzeResults(t, later)
	require.NotEmpty(t, a)
	require.Equal(t, len(a), len(b), "the two runs analyzed a different number of times")
	for i := range a {
		require.Equal(t, a[i], b[i], "Analyze %d returned another result thirty days later", i)
	}
}

// analyzeResults is every Analyze event's kind and result, in order, without
// the input (whose instants move with the run).
func analyzeResults(t *testing.T, recording []byte) []string {
	t.Helper()
	events, err := readEvents(recording)
	require.NoError(t, err)
	out := make([]string, 0, len(events))
	for _, ev := range events {
		var res struct {
			Result *domain.AnalyzerResult
			Error  string
		}
		switch ev.Kind {
		case saturation.KindAnalyze:
			var d saturation.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			res.Result, res.Error = d.Result, d.Error
		case slo.KindAnalyze:
			var d slo.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			res.Result, res.Error = d.Result, d.Error
		case throughput.KindAnalyze:
			var d throughput.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			res.Result, res.Error = d.Result, d.Error
		default:
			continue
		}
		b, err := learnrec.Marshal(res)
		require.NoError(t, err)
		out = append(out, ev.Kind+" "+string(b))
	}
	return out
}

// Recording changes nothing the analyzers return: the same script with the
// recorder off gives the same results, cycle by cycle.
func TestLearnerRecording_RecordingChangesNoResult(t *testing.T) {
	_, on := recordRun(t, recStart, true)
	_, off := recordRun(t, recStart, false)
	require.Len(t, on, len(recSchedule(recStart)), "a result set per cycle")
	require.Equal(t, len(off), len(on))
	for i := range on {
		require.NotEmpty(t, on[i], "cycle %d returned no results", i)
		require.Equal(t, namedResultsText(t, off[i]), namedResultsText(t, on[i]), "cycle %d", i)
	}
}

// namedResultsText encodes a cycle's results, each analyzer's variant
// capacities sorted by name (the throughput analyzer builds them in map
// order).
func namedResultsText(t *testing.T, results []allocation.NamedAnalyzerResult) string {
	t.Helper()
	parts := make([]string, 0, len(results))
	for _, nr := range results {
		c := nr
		if nr.Result != nil {
			r := *nr.Result
			r.VariantCapacities = slices.Clone(r.VariantCapacities)
			sort.SliceStable(r.VariantCapacities, func(i, j int) bool {
				return r.VariantCapacities[i].VariantName < r.VariantCapacities[j].VariantName
			})
			c.Result = &r
		}
		b, err := learnrec.Marshal(c)
		require.NoError(t, err)
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\n")
}

// The engine reaches the saturation analyzer's capabilities through the
// registry; with recording on every one is still there, and each is the
// recorded analyzer itself.
func TestLearnerRecording_TheSaturationCapabilitiesHoldWithRecordingOn(t *testing.T) {
	s := newRecScript(recStart)
	e := s.engine(t)
	e.recordLearners(learnrec.New(&bytes.Buffer{}, 0))
	caps := e.analyzers.saturationCaps()
	sat := e.saturationAnalyzer()
	for name, c := range map[string]any{
		"variantForgetter": caps.forgetter, "staleHistoryEvictor": caps.evictor,
		"learnedStater": caps.learned, "acceleratorStabiliser": caps.stabiliser,
		"presenceNoter": caps.noter, "saturationRecorded": caps.recorded,
	} {
		assert.Same(t, sat, c, name)
	}
	var keeper any = e.sloAnalyzer
	_, ok := keeper.(sloStateKeeper)
	assert.True(t, ok, "sloStateKeeper")
}

// A recording opened by the controller starts with its start line, and the
// analyzers and the store write to it.
func TestLearnerRecording_TheControllerStartOpensOneRecording(t *testing.T) {
	s := newRecScript(recStart)
	e := s.engine(t)
	dir := recordingDir(t)
	e.LearnerRecording = learnrec.Options{Dir: dir}
	e.startLearnerRecording()
	require.NotNil(t, e.learnRec, "the recording did not open")
	s.cycle(t, e, 0, recStart)
	e.stopLearnerRecording()

	files, err := filepathGlob(dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	kinds := kindCounts(t, files[0].data)
	assert.Equal(t, 1, kinds[learnrec.KindStart])
	assert.Equal(t, 3, kinds[saturation.KindAnalyze], "analyzes, controller start")
	assert.Equal(t, 6, kinds[capacity.KindFileScaleTarget], "filings, controller start")
	events, err := readEvents(files[0].data)
	require.NoError(t, err)
	assert.Equal(t, learnrec.KindStart, events[0].Kind, "the first line is the start")
}

func mismatchText(ms []learnreplay.Mismatch) string {
	const limit = 5
	parts := make([]string, 0, limit+1)
	for i, m := range ms {
		if i == limit {
			parts = append(parts, fmt.Sprintf("... and %d more", len(ms)-limit))
			break
		}
		parts = append(parts, m.String())
	}
	return strings.Join(parts, "\n")
}

// assertSweepsEvicted holds the script to sweeps that removed something.
func assertSweepsEvicted(t *testing.T, recording []byte) {
	t.Helper()
	events, err := readEvents(recording)
	require.NoError(t, err)
	var history, records int
	for _, ev := range events {
		switch ev.Kind {
		case saturation.KindEvictStaleHistory:
			var d saturation.Evicted
			require.NoError(t, ev.DecodeData(&d))
			if d.Any() {
				history++
			}
		case capacity.KindEvictStale:
			var d capacity.EvictStaleEvent
			require.NoError(t, ev.DecodeData(&d))
			records += d.Removed
		}
	}
	assert.Equal(t, 2, history, "saturation sweeps that evicted")
	assert.Equal(t, 6, records, "records the store sweeps evicted")
}

// assertRouterServed holds the script to an slo Analyze that read a latency.
func assertRouterServed(t *testing.T, recording []byte) {
	t.Helper()
	events, err := readEvents(recording)
	require.NoError(t, err)
	served := 0
	for _, ev := range events {
		if ev.Kind != slo.KindAnalyze {
			continue
		}
		var d slo.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		if d.RouterCalled && d.RouterLatency != nil {
			served++
		}
	}
	assert.Equal(t, 12, served, "slo Analyzes served a router latency")
}

// assertDecodeRewriteRecordedRaw holds the recording to the raw decode rows:
// the deferred queue is rewritten inside Analyze, so a recording of the
// rewritten rows could not run it again.
func assertDecodeRewriteRecordedRaw(t *testing.T, recording []byte) {
	t.Helper()
	events, err := readEvents(recording)
	require.NoError(t, err)
	for _, ev := range events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		var in learnrec.Input
		require.NoError(t, d.Input.Decode(&in))
		for _, rm := range in.ReplicaMetrics {
			if rm.VariantName == "pd-d" && rm.KvCacheUsage > 0.8 {
				assert.GreaterOrEqual(t, rm.QueueLength, 4, "the decode row was recorded rewritten")
				assert.True(t, rm.CapacityQueueKnown)
				return
			}
		}
	}
	t.Fatal("no decode row was recorded")
}

// recFile is one recording file read back.
type recFile struct {
	name string
	data []byte
}

// filepathGlob reads every recording in dir, oldest first.
func filepathGlob(dir string) ([]recFile, error) {
	names, err := filepath.Glob(filepath.Join(dir, "learnrec-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]recFile, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			return nil, err
		}
		out = append(out, recFile{name: n, data: b})
	}
	return out, nil
}

// recordingDir is a directory for recordings outside /tmp, which the
// recorder refuses (t.TempDir is under /tmp on Linux).
func recordingDir(t *testing.T) string {
	t.Helper()
	base, err := os.UserCacheDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(base, 0o755))
	dir, err := os.MkdirTemp(base, "learnrec-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// assertResultsPriced holds the script to results that price something: a
// replay of errors, or of variants with no capacity, reproduces nothing worth
// reproducing.
func assertResultsPriced(t *testing.T, recording []byte) {
	t.Helper()
	events, err := readEvents(recording)
	require.NoError(t, err)
	priced := map[string]int{}
	for _, ev := range events {
		var res *domain.AnalyzerResult
		var errText string
		switch ev.Kind {
		case saturation.KindAnalyze:
			var d saturation.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			res, errText = d.Result, d.Error
		case slo.KindAnalyze:
			var d slo.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			res, errText = d.Result, d.Error
		case throughput.KindAnalyze:
			var d throughput.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			res, errText = d.Result, d.Error
		default:
			continue
		}
		require.Empty(t, errText, "seq %d %s erred", ev.Seq, ev.Kind)
		require.NotNil(t, res, "seq %d %s returned nothing", ev.Seq, ev.Kind)
		for _, vc := range res.VariantCapacities {
			if vc.PerReplicaCapacity > 0 {
				priced[ev.Kind]++
			}
		}
	}
	assert.Equal(t, map[string]int{saturation.KindAnalyze: 64, slo.KindAnalyze: 22, throughput.KindAnalyze: 52}, priced,
		"variants priced per analyzer")
}

// readEvents is learnrec.Read's events.
func readEvents(b []byte) ([]learnrec.RawEvent, error) {
	r, err := learnrec.Read(b)
	return r.Events, err
}

// A recording cut short -- at its cap, or by a crash mid-write -- still
// replays: the check events of an Analyze the cut lost are left out, and a
// torn last line is dropped and reported.
func TestLearnerRecording_ARecordingCutShortReplays(t *testing.T) {
	recording, _ := recordRun(t, recStart, true)
	events, err := readEvents(recording)
	require.NoError(t, err)
	// Cut right after the first line a variant published: its Analyze, and
	// everything after, is gone.
	cut := -1
	for i, ev := range events {
		if ev.Kind == saturation.KindPublishLine {
			cut = i
			break
		}
	}
	require.Positive(t, cut, "precondition: the run publishes a line")
	var head bytes.Buffer
	for _, ev := range events[:cut+1] {
		head.WriteString(ev.Line + "\n")
	}
	rep, err := learnreplay.Replay(context.Background(), head.Bytes(), learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, mismatchText(rep.Failed()))
	assert.Equal(t, 1, rep.TrimmedChecks)

	torn := append(bytes.Clone(head.Bytes()), events[cut+1].Line[:40]...)
	rep, err = learnreplay.Replay(context.Background(), torn, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, mismatchText(rep.Failed()))
	assert.Equal(t, events[cut+1].Line[:40], rep.TornTail)

	// The same cut made by the recorder's own cap.
	var capped bytes.Buffer
	r := learnrec.New(&capped, int64(head.Len()))
	for _, ev := range events {
		r.Record(ev.Kind, ev.Now, learnrec.Raw(ev.Data))
	}
	require.Equal(t, head.String(), capped.String(), "the cap keeps the same head")
}
