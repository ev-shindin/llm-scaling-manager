package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// settleEngine is an engine with a saturation analyzer that always answers,
// its observe clock stepping one interval per read from start, and one
// decode replica Ready an hour before start.
func settleEngine(t *testing.T, interval time.Duration, start time.Time) (*Engine, []domain.ReplicaMetrics, []domain.VariantReplicaState) {
	t.Helper()
	sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
	cfg := config.NewTestConfig()
	config.SetOptimizationIntervalForTest(cfg, interval)
	e := &Engine{analyzers: testAnalyzers(sat), Config: cfg}
	now := start.Add(-interval)
	e.observeNow = func() time.Time {
		now = now.Add(interval)
		return now
	}
	rows := []domain.ReplicaMetrics{{PodName: "dec-0", VariantName: "dec", KvCacheUsage: 0.4, KvCacheUsageKnown: true,
		Ready: true, ReadySince: start.Add(-time.Hour)}}
	states := []domain.VariantReplicaState{{VariantName: "dec", Role: domain.RoleDecode, CurrentReplicas: 1}}
	return e, rows, states
}

var settleKey = settle.Key{Pod: "dec-0", Variant: "dec"}

func runSettleCycle(t *testing.T, e *Engine, rows []domain.ReplicaMetrics, states []domain.VariantReplicaState) {
	t.Helper()
	policy := config.ScalingPolicy{KvCacheThreshold: 0.80, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70}
	if _, err := e.runAnalyzers(context.Background(), "m", "ns", rows, policy, states, nil, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
}

// Review r1 (S1): rows come from Prometheus series, so a failed collection
// (a KV query timing out, Prometheus unreachable) reaches the engine as no
// rows at all, not as rows with unknown gauges. The settle tracker holds its
// sightings through such cycles (modelCollectionFailed, onNothingObserved),
// so an outage longer than the tolerance does not settle the fleet again
// when it ends. Without the hold, five failed cycles at 15 s are a 90 s
// absence, past the 1 min tolerance, and the replica rejoins.
func TestSettle_AFailedCollectionKeepsTheSighting(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		failed func(e *Engine)
		want   settle.Reason
	}{
		{"collection failed", func(e *Engine) { e.modelCollectionFailed("ns", "m") }, settle.Joined},
		{"no rows", func(e *Engine) { e.onNothingObserved("ns", "m") }, settle.Joined},
		{"without the hold", func(e *Engine) { e.loadBalanceCollectionFailed("ns", "m", e.observeClock()) }, settle.Rejoined},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, rows, states := settleEngine(t, 15*time.Second, start)
			runSettleCycle(t, e, rows, states)
			runSettleCycle(t, e, rows, states)
			for range 5 {
				tc.failed(e)
			}
			runSettleCycle(t, e, rows, states)
			r, ok := e.settleTracker().Replica("ns/m", settleKey)
			if !ok {
				t.Fatal("the replica is not known after the failed cycles")
			}
			if r.Reason != tc.want {
				t.Errorf("after five failed cycles: settle %s at %v, want %s", r.Reason, r.Since, tc.want)
			}
			if tc.want == settle.Joined && !r.Since.Equal(start) {
				t.Errorf("the settle began at %v, want the first cycle %v", r.Since, start)
			}
		})
	}
}

// Review r1 (B1): the engine sets the tracker's tolerance from the optimize
// interval. At a 61 s interval every cycle is more than a minute after the
// last; with the tolerance fixed at a minute every replica rejoined on every
// cycle.
func TestSettle_TheEngineSetsTheToleranceFromTheInterval(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	e, rows, states := settleEngine(t, 61*time.Second, start)
	for range 4 {
		runSettleCycle(t, e, rows, states)
	}
	r, _ := e.settleTracker().Replica("ns/m", settleKey)
	if r.Reason != settle.Joined || !r.Since.Equal(start) || r.Tolerance != settle.Tolerance(61*time.Second) {
		t.Errorf("at a 61 s interval: settle %s at %v, tolerance %v; want the join at %v and %v",
			r.Reason, r.Since, r.Tolerance, start, settle.Tolerance(61*time.Second))
	}
}

// Review r1 (S3): the engine sweeps the settle tracker with the model: a
// model no longer reconciled, and every model on a cycle with none.
func TestSettle_TheEngineSweepsTheTrackerWithTheModel(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	e, rows, states := settleEngine(t, 15*time.Second, start)
	runSettleCycle(t, e, rows, states)
	if _, ok := e.settleTracker().Replica("ns/m", settleKey); !ok {
		t.Fatal("precondition: the replica is known")
	}
	e.pruneAnalyzerSeries(map[string]bool{"ns/other": true})
	if _, ok := e.settleTracker().Replica("ns/m", settleKey); ok {
		t.Error("a model no longer reconciled kept its settle record")
	}
	runSettleCycle(t, e, rows, states)
	e.evictAllAnalyzerSeries()
	if _, ok := e.settleTracker().Replica("ns/m", settleKey); ok {
		t.Error("a cycle with no model kept a settle record")
	}
}

// Review r2 (S1): the hold reaches production through optimizeV2's three
// paths that read no rows of a model: a collection that fails
// (modelCollectionFailed), one that returns nothing (onNothingObserved), and
// a namespace whose scaling policy is not loaded yet. Five such cycles at
// 15 s are a 90 s gap, past the minute's tolerance; held, the replica keeps
// its join. Switching the failed-collection call site back to
// loadBalanceCollectionFailed fails the first case.
func TestSettle_OptimizeV2HoldsOnEveryPathWithNoRows(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		source source.MetricsSource
		policy bool
	}{
		{"collection failed", failingMetricsSource{}, true},
		{"nothing collected", emptyMetricsSource{}, true},
		{"scaling policy not loaded", emptyMetricsSource{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := engineConfigEngine(t)
			e.Config = config.NewTestConfig()
			if tc.policy {
				e.Config.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: {}})
			}
			e.optimizer = allocation.NewCostAwareOptimizer()
			e.client = fake.NewClientBuilder().WithObjects(parkedTarget()).Build()
			e.Recorder = record.NewFakeRecorder(100)
			e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(tc.source, nil, nil, record.NewFakeRecorder(100), nil)
			now := start.Add(cycleOf15s)
			e.observeNow = func() time.Time {
				now = now.Add(cycleOf15s)
				return now
			}
			row := settle.Row{Key: settle.Key{Pod: "d-0", Variant: "v"}, Ready: true, ReadySince: start.Add(-time.Hour)}
			e.settleTracker().Observe("ns/m", []settle.Row{row}, start)
			e.settleTracker().Observe("ns/m", []settle.Row{row}, start.Add(cycleOf15s))
			for range 5 {
				e.optimizeV2(context.Background(), map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					"ns/m": {parkedVA()},
				}, nil)
			}
			back := e.observeClock()
			require.Greater(t, back.Sub(start.Add(cycleOf15s)), settle.Tolerance(cycleOf15s),
				"precondition: the failed cycles span more than the tolerance")
			e.settleTracker().Observe("ns/m", []settle.Row{row}, back)
			r, ok := e.settleTracker().Replica("ns/m", row.Key)
			require.True(t, ok)
			assert.Equal(t, settle.Joined, r.Reason, "five cycles with no rows settled the replica again")
			assert.Equal(t, start, r.Since)
		})
	}
}

const cycleOf15s = 15 * time.Second
