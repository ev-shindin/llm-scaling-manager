package steadystate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/balance"
)

// loadCycle is the controller's default optimize interval.
const loadCycle = 15 * time.Second

// steppedLoadClock sets the engine's load-balance clock to advance one
// loadCycle per read, from start.
func steppedLoadClock(e *Engine, start time.Time) {
	now := start
	e.loadBalanceNow = func() time.Time {
		now = now.Add(loadCycle)
		return now
	}
}

// sustainCycles is the cycle on which an imbalance held from the first is
// reported. The tracker first sees the replicas on the first cycle, so the
// idle one is idle from IdleAfterReady on, however long its Pod has been
// Ready, and the imbalance is reported SustainFor after that.
var sustainCycles = int((balance.IdleAfterReady+balance.SustainFor)/loadCycle) + 1

// failingSaturation is a saturation analyzer whose every analysis fails.
type failingSaturation struct{}

func (failingSaturation) Name() string { return domain.SaturationAnalyzerName }
func (failingSaturation) Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	return nil, errors.New("analysis failed")
}

// loadSeries is the value of the one wva_load_imbalanced series for model m, and
// how many load-distribution series exist in all.
func loadSeries(t *testing.T, reg *promclient.Registry) (imbalanced float64, present bool, total int) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "wva_load_") {
			continue
		}
		total += len(mf.GetMetric())
		if mf.GetName() != constants.WVALoadImbalanced {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == constants.LabelModelName && l.GetValue() == "m" {
					imbalanced, present = m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return imbalanced, present, total
}

// The measured case, end to end through runAnalyzers: one prefill replica
// queued while three idle warns once, on the cycle the imbalance has held for
// balance.SustainFor, publishes wva_load_imbalanced=1, and leaves what the
// analyzer was given untouched. A model with no rows, or no longer
// reconciled, takes its series with it.
func TestRunAnalyzersReportsLoadImbalanceOnce(t *testing.T) {
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
	e := &Engine{saturationV2Analyzer: sat, started: true,
		analyzersSnapshot: []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: sat}}}
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	steppedLoadClock(e, start)
	states := []domain.VariantReplicaState{
		{VariantName: "pre", Role: domain.RolePrefill, CurrentReplicas: 4},
		{VariantName: "dec", Role: domain.RoleDecode, CurrentReplicas: 1},
	}
	since := start.Add(-time.Hour)
	rows := []domain.ReplicaMetrics{
		{PodName: "pre-0", VariantName: "pre", KvCacheUsage: 0.20, QueueLength: 439, Ready: true, ReadySince: since},
		{PodName: "pre-1", VariantName: "pre", KvCacheUsage: 0.01, Ready: true, ReadySince: since},
		{PodName: "pre-2", VariantName: "pre", Ready: true, ReadySince: since},
		{PodName: "pre-3", VariantName: "pre", KvCacheUsage: 0.02, Ready: true, ReadySince: since},
		{PodName: "dec-0", VariantName: "dec", KvCacheUsage: 0.40, Ready: true, ReadySince: since},
	}
	policy := config.ScalingPolicy{KvCacheThreshold: 0.80, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70, QueueLengthThreshold: 5}
	ctx, logs := zapObserverCtx(t)
	for cycle := 1; cycle <= sustainCycles+1; cycle++ {
		if _, err := e.runAnalyzers(ctx, "m", "ns", rows, policy, states, nil, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
		v, present, _ := loadSeries(t, reg)
		want := 0.0
		if cycle >= sustainCycles {
			want = 1
		}
		if !present || v != want {
			t.Errorf("cycle %d: wva_load_imbalanced = %v (present %v), want %v", cycle, v, present, want)
		}
	}
	warned := 0
	for _, entry := range logs.All() {
		if strings.HasPrefix(entry.Message, "WARNING: load imbalanced") {
			warned++
			fields := entry.ContextMap()
			if fields["role"] != domain.RolePrefill || fields["busiestPod"] != "pre-0" {
				t.Errorf("warning names role %v pod %v, want prefill and pre-0", fields["role"], fields["busiestPod"])
			}
		}
	}
	if warned != 1 {
		t.Errorf("%d imbalance warnings over %d imbalanced cycles, want exactly 1", warned, sustainCycles+1)
	}
	if rows[0].QueueLength != 439 || rows[0].KvCacheUsage != 0.20 {
		t.Error("observing the load changed the rows the analyzers read")
	}

	e.onNothingObserved("ns", "m")
	if _, present, total := loadSeries(t, reg); present || total != 0 {
		t.Errorf("a model with no rows kept %d load series", total)
	}

	if _, err := e.runAnalyzers(ctx, "m", "ns", rows, policy, states, nil, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := loadSeries(t, reg); v != 0 {
		t.Error("a model back from no rows resumed its old streak")
	}
	e.pruneAnalyzerSeries(map[string]bool{"ns/other": true})
	if _, _, total := loadSeries(t, reg); total != 0 {
		t.Errorf("a model no longer reconciled kept %d load series", total)
	}
	if _, err := e.runAnalyzers(ctx, "m", "ns", rows, policy, states, nil, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	e.evictAllAnalyzerSeries()
	if _, _, total := loadSeries(t, reg); total != 0 {
		t.Errorf("evicting every series kept %d load series", total)
	}
}

// Review r1: a model whose analysis failed kept its load series at the last
// value published, so a page could stand on a reading nothing was making any
// more. The rows are this cycle's whatever the analysis makes of them, so the
// load is observed before the analyzers run, and an imbalance that clears
// while the analysis fails clears the series too.
func TestFailingAnalysisDoesNotFreezeTheLoadSeries(t *testing.T) {
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
	e := &Engine{saturationV2Analyzer: sat, started: true,
		analyzersSnapshot: []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: sat}}}
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	steppedLoadClock(e, start)
	since := start.Add(-time.Hour)
	states := []domain.VariantReplicaState{{VariantName: "dec", Role: domain.RoleDecode, CurrentReplicas: 2}}
	uneven := []domain.ReplicaMetrics{
		{PodName: "dec-0", VariantName: "dec", KvCacheUsage: 0.95, Ready: true, ReadySince: since},
		{PodName: "dec-1", VariantName: "dec", KvCacheUsage: 0.05, Ready: true, ReadySince: since},
	}
	even := []domain.ReplicaMetrics{
		{PodName: "dec-0", VariantName: "dec", KvCacheUsage: 0.50, Ready: true, ReadySince: since},
		{PodName: "dec-1", VariantName: "dec", KvCacheUsage: 0.50, Ready: true, ReadySince: since},
	}
	policy := config.ScalingPolicy{KvCacheThreshold: 0.80, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70, QueueLengthThreshold: 5}
	ctx := context.Background()
	for range sustainCycles {
		if _, err := e.runAnalyzers(ctx, "m", "ns", uneven, policy, states, nil, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if v, _, _ := loadSeries(t, reg); v != 1 {
		t.Fatalf("wva_load_imbalanced = %v after the imbalance held, want 1", v)
	}

	e.saturationV2Analyzer = failingSaturation{}
	e.analyzersSnapshot = []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: failingSaturation{}}}
	for range int(balance.ClearAfter/loadCycle) + 1 {
		if _, err := e.runAnalyzers(ctx, "m", "ns", even, policy, states, nil, nil, nil, nil, 0); err == nil {
			t.Fatal("the failing analyzer did not fail")
		}
	}
	if v, present, _ := loadSeries(t, reg); !present || v != 0 {
		t.Errorf("wva_load_imbalanced = %v (present %v) after the load evened out under a failing analysis, want 0", v, present)
	}
}

// Review r3 (N2): one failed collection deleted the series and forgot the
// model, so the alert's for: restarted from a stale series and the gauge
// came back at 0 for SustainFor: one failure moved the page from 10 to 21
// minutes, and one every 8 minutes kept it from paging at all. One failure
// leaves the series and the report as they stand; collection failing for
// balance.StaleAfter still deletes them.
func TestOneFailedCollectionDoesNotRestartTheLoadSeries(t *testing.T) {
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
	e := &Engine{saturationV2Analyzer: sat, started: true,
		analyzersSnapshot: []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: sat}}}
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	steppedLoadClock(e, start)
	since := start.Add(-time.Hour)
	states := []domain.VariantReplicaState{{VariantName: "dec", Role: domain.RoleDecode, CurrentReplicas: 2}}
	uneven := []domain.ReplicaMetrics{
		{PodName: "dec-0", VariantName: "dec", KvCacheUsage: 0.95, Ready: true, ReadySince: since},
		{PodName: "dec-1", VariantName: "dec", KvCacheUsage: 0.05, Ready: true, ReadySince: since},
	}
	policy := config.ScalingPolicy{KvCacheThreshold: 0.80, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70, QueueLengthThreshold: 5}
	ctx := context.Background()
	collect := func() {
		t.Helper()
		if _, err := e.runAnalyzers(ctx, "m", "ns", uneven, policy, states, nil, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	for range sustainCycles {
		collect()
	}
	if v, _, _ := loadSeries(t, reg); v != 1 {
		t.Fatalf("wva_load_imbalanced = %v after the imbalance held, want 1", v)
	}

	e.loadBalanceCollectionFailed("ns", "m")
	if v, present, _ := loadSeries(t, reg); !present || v != 1 {
		t.Errorf("one failed collection left wva_load_imbalanced = %v (present %v), want 1", v, present)
	}
	collect()
	if v, present, _ := loadSeries(t, reg); !present || v != 1 {
		t.Errorf("the cycle after one failed collection read wva_load_imbalanced = %v (present %v), want 1", v, present)
	}

	for range int(balance.StaleAfter/loadCycle) + 1 {
		e.loadBalanceCollectionFailed("ns", "m")
	}
	if _, _, total := loadSeries(t, reg); total != 0 {
		t.Errorf("collection failing for StaleAfter kept %d load series", total)
	}
}

// A replica is saturated on KV at the policy's kvCacheThreshold, the ceiling
// the analyzer prices it as full at. The detector used a fixed 0.90 of its
// own, so a replica the analyzer read as full at 0.82 was not saturated for
// it, and its idle siblings never warned. 0.82 is under ScaleUpThreshold
// 0.85 too, so passing that in its place would fail here.
func TestLoadImbalanceReadsThePolicysKvThreshold(t *testing.T) {
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
	e := &Engine{saturationV2Analyzer: sat, started: true,
		analyzersSnapshot: []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: sat}}}
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	steppedLoadClock(e, start)
	since := start.Add(-time.Hour)
	states := []domain.VariantReplicaState{{VariantName: "dec", Role: domain.RoleDecode, CurrentReplicas: 2}}
	rows := []domain.ReplicaMetrics{
		{PodName: "dec-0", VariantName: "dec", KvCacheUsage: 0.82, Ready: true, ReadySince: since},
		{PodName: "dec-1", VariantName: "dec", KvCacheUsage: 0.05, Ready: true, ReadySince: since},
	}
	policy := config.ScalingPolicy{KvCacheThreshold: 0.80, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70, QueueLengthThreshold: 5}
	for range sustainCycles {
		if _, err := e.runAnalyzers(context.Background(), "m", "ns", rows, policy, states, nil, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if v, _, _ := loadSeries(t, reg); v != 1 {
		t.Errorf("wva_load_imbalanced = %v with one replica at KV 0.82 over kvCacheThreshold 0.80 beside an idle one, want 1", v)
	}
}
