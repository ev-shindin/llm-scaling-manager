package steadystate

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	promclient "github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scalingpolicy"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

func sloWiringEngine(t *testing.T) *Engine {
	t.Helper()
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	sources := source.NewSourceRegistry()
	if err := sources.Register("prometheus", prometheus.NewPrometheusSource(ctx, nil, prometheus.DefaultPrometheusSourceConfig())); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(c, c, c.Scheme(), nil, sources, config.NewTestConfig(), allocation.NewNoOpLimiter("test"))
	e.Variants = registry.New(0)
	return e
}

// TestNewEngineGivesTheSLOAnalyzerARouterSource pins the wiring that decides
// whether an SLO is ever judged: built without the collector as its router
// source, the analyzer prices variants and leaves every objective unjudged,
// with nothing failing.
func TestNewEngineGivesTheSLOAnalyzerARouterSource(t *testing.T) {
	e := sloWiringEngine(t)
	for _, entry := range e.analyzers.builtins() {
		if entry.name != domain.SLOAnalyzerName {
			continue
		}
		a, ok := entry.analyzer.(*slo.Analyzer)
		if !ok {
			t.Fatalf("the %q entry is a %T", entry.name, entry.analyzer)
		}
		if !a.Judging() {
			t.Fatal("the slo analyzer was built without a router latency source")
		}
		return
	}
	t.Fatal("NewEngine registered no slo analyzer")
}

// TestNewEngineRegistersTheRouterQueries pins that the router queries exist in
// the engine's registry: without them every refresh fails and the SLO is
// silently never judged.
func TestNewEngineRegistersTheRouterQueries(t *testing.T) {
	e := sloWiringEngine(t)
	ql := e.metricsRegistry.Get("prometheus").QueryList()
	for _, name := range []string{"slo_router_ttft_buckets", "slo_router_tpot_sum", "slo_router_tpot_count"} {
		if ql.Get(name) == nil {
			t.Errorf("query %q is not registered", name)
		}
	}
}

// TestOptimizeSweepsStaleSLOSeries pins the per-cycle sweep: the last model to
// stop publishing SLO series would otherwise keep them for ever, because the
// only other sweep runs inside a publish that never comes.
func TestOptimizeSweepsStaleSLOSeries(t *testing.T) {
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	metrics.PublishSLO(metrics.SLOModelSnapshot{Namespace: "ns", ModelName: "m",
		Objectives: []metrics.SLOObjectiveReading{{Objective: constants.SLOObjectiveTTFT, Statistic: "p90",
			TargetSeconds: 3, Observed: 1, Attainment: 0.95, Requests: 300}},
	}, time.Now().Add(-11*time.Minute))

	count := func() int {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range mfs {
			if mf.GetName() == constants.WVASLOTargetSeconds {
				return len(mf.GetMetric())
			}
		}
		return 0
	}
	if count() != 1 {
		t.Fatal("setup: the stale series was not published")
	}
	e := sloWiringEngine(t)
	if err := e.optimize(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("optimize: %v", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("after a cycle with no model %d stale wva_slo_target_seconds series remain", n)
	}
}

// TestResolveModelPolicyReportsAHalfConfiguredSLO pins that the report comes
// from the policy resolution, the one place that sees a model whose policy sets
// targets but does not list the analyzer (which therefore never runs).
func TestResolveModelPolicyReportsAHalfConfiguredSLO(t *testing.T) {
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	ctx := logr.NewContext(context.Background(), logger)
	e := &Engine{policies: scalingpolicy.NewChangeReporter()}
	targets := &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 100 * time.Millisecond}}}
	e.resolveModelPolicy(ctx, map[string]config.ScalingPolicy{
		"default": {Analyzers: []config.AnalyzerScoreConfig{{Name: "saturation"}}, SLO: targets},
	}, "m", "ns", nil)
	for _, l := range lines {
		if strings.Contains(l, "SLO policy is half-configured") && strings.Contains(l, "nothing reads them") {
			return
		}
	}
	t.Fatalf("no half-configured report among %d log lines: %v", len(lines), lines)
}

// sloKeeperSpy records how the engine's sweeps reached the slo analyzer.
type sloKeeperSpy struct {
	evictions, cycles int
	present           map[string][]string
}

func (s *sloKeeperSpy) BeginCycle() { s.cycles++ }

func (s *sloKeeperSpy) NoteVariantsPresent(namespace, modelID string, names []string) {
	if s.present == nil {
		s.present = map[string][]string{}
	}
	s.present[namespace+"/"+modelID] = append(s.present[namespace+"/"+modelID], names...)
}

func (s *sloKeeperSpy) EvictStale(time.Time) slo.Evicted {
	s.evictions++
	return slo.Evicted{}
}

// TestNewEngineHoldsTheSLOAnalyzerItRegisters pins that the analyzer the sweeps
// reach is the one that runs: two instances would leave the running one's state
// unswept.
func TestNewEngineHoldsTheSLOAnalyzerItRegisters(t *testing.T) {
	e := sloWiringEngine(t)
	for _, entry := range e.analyzers.builtins() {
		if entry.name == domain.SLOAnalyzerName {
			if entry.analyzer != e.sloAnalyzer.(domain.Analyzer) {
				t.Fatal("the engine sweeps a different slo analyzer than the one it registered")
			}
			return
		}
	}
	t.Fatal("NewEngine registered no slo analyzer")
}

// TestEvictStaleLearnedStateSweepsTheSLOAnalyzer pins that the slo analyzer's
// state is aged by the engine's once-per-cycle sweep. It used to sweep inside
// Analyze, which never runs again once the last model stops listing it.
func TestEvictStaleLearnedStateSweepsTheSLOAnalyzer(t *testing.T) {
	spy := &sloKeeperSpy{}
	e := &Engine{analyzers: testAnalyzers(plainAnalyzer{}), sloAnalyzer: spy}
	e.evictStaleLearnedState(context.Background())
	if spy.evictions != 1 {
		t.Fatalf("a sweep reached the slo analyzer %d times, want 1", spy.evictions)
	}
}

// TestNotePresentVariantsStampsTheSLOAnalyzer pins that a variant parked at
// zero, which no analyzer runs for, keeps its slo line as it keeps its
// saturation state.
func TestNotePresentVariantsStampsTheSLOAnalyzer(t *testing.T) {
	e := sloWiringEngine(t)
	spy := &sloKeeperSpy{}
	e.sloAnalyzer = spy
	e.notePresentVariants(context.Background(), map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"chat/m": {{
			ObjectMeta: metav1.ObjectMeta{Name: "chat-a", Namespace: "chat"},
			Spec:       llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{ModelID: "m"},
		}},
	})
	if got := spy.present["chat/m"]; len(got) != 1 || got[0] != "chat-a" {
		t.Fatalf("the slo analyzer was told %v present for chat/m, want [chat-a]", got)
	}
}

// TestEveryCycleBeginsAnSLOCycle pins that the cycle count the unobserved
// exclusion reads advances on every cycle, one with no model to analyze
// included: counting only cycles that analyze something would hide the cycles
// a parked fleet was not watched for.
func TestEveryCycleBeginsAnSLOCycle(t *testing.T) {
	e := sloWiringEngine(t)
	spy := &sloKeeperSpy{}
	e.sloAnalyzer = spy
	for range 2 {
		if err := e.optimize(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("optimize: %v", err)
		}
	}
	if spy.cycles != 2 {
		t.Fatalf("two cycles began %d slo cycles, want 2", spy.cycles)
	}
}

// TestAFailedCycleAndItsRetryAreOneSLOCycle pins where the cycle count
// starts: past the cycle's error returns. The executor retries a failed cycle
// within a fraction of a second, and a count taken before the failure made
// the failure and its retry two cycles -- a skipped one -- which left every
// SLO model unobserved for a window and its grace.
func TestAFailedCycleAndItsRetryAreOneSLOCycle(t *testing.T) {
	e := sloWiringEngine(t)
	spy := &sloKeeperSpy{}
	e.sloAnalyzer = spy
	e.Variants.Observe("ns", "m-wva", map[string]string{registry.ModelIDKey: "m"})
	e.Variants.SetTarget("ns", "m-wva", registry.Target{APIVersion: "apps/v1", Kind: "Deployment", Name: "m"})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.optimize(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cycle on a cancelled context returned %v, want context.Canceled", err)
	}
	if spy.cycles != 0 {
		t.Fatalf("a cycle that failed before its models counted %d slo cycles, want 0", spy.cycles)
	}
	if err := e.optimize(context.Background()); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if spy.cycles != 1 {
		t.Fatalf("a failure and its retry counted %d slo cycles, want 1", spy.cycles)
	}
}

// TestALongRunOfFailedCyclesIsASkippedCycle pins the other side: failures
// that lasted an interval or more are cycles nobody saw, so the cycle after
// them counts as two and every model is unobserved. Counting none, a model
// starting through a half-hour API outage was judged on recovery as one
// pending all along. Real failures through optimize, on a short interval:
// the run is timed from its first failure, not its last.
func TestALongRunOfFailedCyclesIsASkippedCycle(t *testing.T) {
	e := sloWiringEngine(t)
	spy := &sloKeeperSpy{}
	e.sloAnalyzer = spy
	e.Variants.Observe("ns", "m-wva", map[string]string{registry.ModelIDKey: "m"})
	e.Variants.SetTarget("ns", "m-wva", registry.Target{APIVersion: "apps/v1", Kind: "Deployment", Name: "m"})
	config.SetOptimizationIntervalForTest(e.Config, 50*time.Millisecond)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.optimize(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("first failure: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := e.optimize(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("second failure: %v", err)
	}
	if err := e.optimize(context.Background()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if spy.cycles != 2 {
		t.Fatalf("the cycle after more than an interval of failures counted %d slo cycles, want 2", spy.cycles)
	}
	if !e.sloFailedSince.IsZero() {
		t.Fatal("a successful cycle must end the run of failures")
	}
}

// fixedRouter returns the same router latency on every call.
type fixedRouter struct{ rl domain.RouterLatency }

func (r fixedRouter) CollectRouterLatency(context.Context, string, string) *domain.RouterLatency {
	rl := r.rl
	return &rl
}

// recordingAnalyzer passes through to an analyzer and keeps what it returned.
type recordingAnalyzer struct {
	domain.Analyzer
	results []*domain.AnalyzerResult
}

func (r *recordingAnalyzer) Analyze(ctx context.Context, in domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	res, err := r.Analyzer.Analyze(ctx, in)
	r.results = append(r.results, res)
	return res, err
}

// TestListingSLOLeavesTheDecisionUnchanged pins the observe-only promise where
// the decision is made: the request the optimizer receives for a model is the
// same whether or not its policy lists slo and sets targets. The analyzer is
// the real one, and the test checks it priced on the very cycles compared,
// with figures on both sides of saturation's -- so any merge of its result
// into the request (the wrong entry picked; a composite taking the larger or
// the smaller demand or capacity; its per-role demand on a P/D model) fails
// here.
func TestListingSLOLeavesTheDecisionUnchanged(t *testing.T) {
	if err := metrics.InitMetrics(promclient.NewRegistry()); err != nil {
		t.Fatal(err)
	}
	// Readings on ITL = 0.04*k + 0.02 for variant v, two replicas a cycle, k
	// climbing 0.05 a cycle. The slo analyzer prices v at about 1240 tokens/s
	// per replica, and its demand is ArrivalRate 3 x 200 output tokens = 600.
	rows := func(i int) []domain.ReplicaMetrics {
		row := func(pod string, k float64) domain.ReplicaMetrics {
			return domain.ReplicaMetrics{PodName: pod, ModelID: "m", Namespace: "ns", VariantName: "v", Ready: true,
				KvUsageInstant: k, AvgITL: 0.04*k + 0.02, AvgOutputTokens: 200, AvgInputTokens: 1000, RequestRate: 1.5,
				TotalKvCapacityTokens: 100000}
		}
		return []domain.ReplicaMetrics{row("p0", 0.20+0.05*float64(i)), row("p1", 0.25+0.05*float64(i))}
	}
	const arrival, cycles = 3, 10
	router := fixedRouter{domain.RouterLatency{WindowSeconds: 300,
		TTFTBuckets: []domain.LatencyBucket{{UpperBound: 1, Rate: 1}, {UpperBound: 3, Rate: 2}, {UpperBound: math.Inf(1), Rate: 2}},
		TPOTSumRate: 0.08, TPOTCountRate: 2}}
	plain := config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
		Analyzers: []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}}}
	withSLO := plain
	withSLO.Analyzers = []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}, {Name: domain.SLOAnalyzerName}}
	withSLO.SLO = &config.SLOPolicy{
		TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 44 * time.Millisecond}},
		TTFT: &config.SLOObjective{Target: config.SLODuration{Duration: 2 * time.Second}},
	}
	aggregated := []domain.VariantReplicaState{{VariantName: "v", Role: domain.RoleBoth, CurrentReplicas: 2}}
	disaggregated := []domain.VariantReplicaState{
		{VariantName: "v", Role: domain.RoleDecode, CurrentReplicas: 2},
		{VariantName: "p", Role: domain.RolePrefill, CurrentReplicas: 1},
	}
	for _, leg := range []struct {
		name     string
		states   []domain.VariantReplicaState
		demand   float64 // saturation's
		capacity float64 // saturation's, per replica of v
		roleDem  map[string]float64
		sloAbove bool // slo's demand above saturation's and its capacity below
	}{
		{name: "slo demand above, capacity below", states: aggregated, demand: 400, capacity: 5000, sloAbove: true},
		{name: "slo demand below, capacity above", states: aggregated, demand: 800, capacity: 500},
		{name: "P/D, per-role demand", states: disaggregated, demand: 500, capacity: 5000, sloAbove: true,
			roleDem: map[string]float64{domain.RoleDecode: 400, domain.RolePrefill: 100}},
	} {
		t.Run(leg.name, func(t *testing.T) {
			newEngine := func(a domain.Analyzer) *Engine {
				sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{
					AnalyzerName: domain.SaturationAnalyzerName, ModelID: "m", Namespace: "ns",
					TotalDemand: leg.demand, RoleDemand: leg.roleDem,
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Role: leg.states[0].Role, ReplicaCount: 2,
						PerReplicaCapacity: leg.capacity, ObservedReplicas: 2}}}}
				return &Engine{analyzers: testAnalyzers(sat,
					analyzerEntry{name: domain.SLOAnalyzerName, analyzer: a})}
			}
			rec := &recordingAnalyzer{Analyzer: slo.New(router, nil)}
			without, with := newEngine(slo.New(router, nil)), newEngine(rec)
			for i := range cycles {
				a, err := without.collectV2ModelRequest(context.Background(), "m", "ns", rows(i), plain, leg.states, nil, nil, nil, nil, arrival)
				if err != nil {
					t.Fatal(err)
				}
				b, err := with.collectV2ModelRequest(context.Background(), "m", "ns", rows(i), withSLO, leg.states, nil, nil, nil, nil, arrival)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(a, b) {
					t.Fatalf("cycle %d: listing slo changed the optimizer's request:\nwithout %+v\nwith    %+v",
						i, a.CompositeSignal, b.CompositeSignal)
				}
			}
			// The comparison proves something only on cycles where slo priced,
			// with figures on the side of saturation's this leg is about.
			if len(rec.results) != cycles {
				t.Fatalf("setup: the slo analyzer ran %d times in %d cycles", len(rec.results), cycles)
			}
			priced := 0
			for i, res := range rec.results {
				var vc *domain.VariantCapacity
				for j := range res.VariantCapacities {
					if res.VariantCapacities[j].VariantName == "v" {
						vc = &res.VariantCapacities[j]
					}
				}
				if vc == nil || !(vc.PerReplicaCapacity > 0) {
					continue
				}
				priced++
				if leg.sloAbove != (res.TotalDemand > leg.demand) || leg.sloAbove != (vc.PerReplicaCapacity < leg.capacity) {
					t.Fatalf("setup, cycle %d: slo demand %v and capacity %v are not on the intended side of %v and %v",
						i, res.TotalDemand, vc.PerReplicaCapacity, leg.demand, leg.capacity)
				}
				if leg.roleDem != nil && res.RoleDemand[domain.RoleDecode] == leg.roleDem[domain.RoleDecode] {
					t.Fatalf("setup, cycle %d: slo's decode demand equals saturation's", i)
				}
			}
			if priced < cycles/2 {
				t.Fatalf("setup: the slo analyzer priced on %d of the %d compared cycles, want at least %d",
					priced, cycles, cycles/2)
			}
		})
	}
}

// countingRouter is a router latency source that counts what it was asked.
type countingRouter struct{ calls int }

func (r *countingRouter) CollectRouterLatency(context.Context, string, string) *domain.RouterLatency {
	r.calls++
	return &domain.RouterLatency{WindowSeconds: 300,
		TTFTBuckets: []domain.LatencyBucket{{UpperBound: 3, Rate: 1}, {UpperBound: math.Inf(1), Rate: 1}}}
}

// TestTheSLOAnalyzerCostsNothingWhereNoPolicyListsIt pins the zero-impact
// promise at the engine, with the real analyzer: a model whose policy does not
// list slo makes no router query and publishes no wva_slo_* series; a model
// that lists it does both.
func TestTheSLOAnalyzerCostsNothingWhereNoPolicyListsIt(t *testing.T) {
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	sloSeries := func() int {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, mf := range mfs {
			if strings.HasPrefix(mf.GetName(), "wva_slo_") {
				n += len(mf.GetMetric())
			}
		}
		return n
	}
	router := &countingRouter{}
	sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
	e := &Engine{analyzers: testAnalyzers(sat,
		analyzerEntry{name: domain.SLOAnalyzerName, analyzer: slo.New(router, nil)})}
	run := func(namespace string, policy config.ScalingPolicy, states []domain.VariantReplicaState) {
		t.Helper()
		results, err := e.runAnalyzers(context.Background(), "m", namespace, nil, policy, states, nil, nil, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		e.recordAnalyzerMetrics(namespace, "m", results)
	}
	targets := &config.SLOPolicy{TTFT: &config.SLOObjective{Target: config.SLODuration{Duration: 3 * time.Second}}}
	plain := config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70, SLO: targets}
	withSLO := plain
	withSLO.Analyzers = []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}, {Name: domain.SLOAnalyzerName}}
	states := []domain.VariantReplicaState{{VariantName: "v", Role: domain.RoleBoth, CurrentReplicas: 1}}

	// Targets set, analyzer not listed: the half-configured case, which must
	// still cost nothing.
	run("batch", plain, states)
	if router.calls != 0 || sloSeries() != 0 {
		t.Fatalf("unlisted: %d router queries, %d slo series; want none", router.calls, sloSeries())
	}
	run("chat", withSLO, states)
	if router.calls != 1 || sloSeries() == 0 {
		t.Fatalf("listed: %d router queries, %d slo series; want 1, some", router.calls, sloSeries())
	}
}

// TestPruneAnalyzerSeriesDeletesADepartedModelsSLOSeries pins that a model
// whose VariantAutoscalings are gone loses its SLO series with its other
// analyzer series, not ten minutes later.
func TestPruneAnalyzerSeriesDeletesADepartedModelsSLOSeries(t *testing.T) {
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	metrics.PublishSLO(metrics.SLOModelSnapshot{Namespace: "ns", ModelName: "gone",
		Objectives: []metrics.SLOObjectiveReading{{Objective: constants.SLOObjectiveTTFT, Statistic: "p90",
			TargetSeconds: 3, Observed: 1, Attainment: 0.95, Requests: 300}},
	}, time.Now())
	metrics.PublishSLO(metrics.SLOModelSnapshot{Namespace: "ns", ModelName: "kept",
		Objectives: []metrics.SLOObjectiveReading{{Objective: constants.SLOObjectiveTTFT, Statistic: "p90",
			TargetSeconds: 3, Observed: 1, Attainment: 0.95, Requests: 300}},
	}, time.Now())
	targets := func() map[string]bool {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, mf := range mfs {
			if mf.GetName() != constants.WVASLOTargetSeconds {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == constants.LabelModelName {
						out[l.GetValue()] = true
					}
				}
			}
		}
		return out
	}
	if got := targets(); !got["gone"] || !got["kept"] {
		t.Fatalf("setup: published %v", got)
	}
	e := &Engine{metricsEmitter: metrics.NewMetricsEmitter(), lastAnalyzerSeries: map[string]analyzerSeries{
		"ns/gone": {namespace: "ns", modelID: "gone"},
		"ns/kept": {namespace: "ns", modelID: "kept"},
	}}
	e.pruneAnalyzerSeries(map[string]bool{"ns/kept": true})
	if got := targets(); got["gone"] || !got["kept"] {
		t.Fatalf("after the departed model's prune the SLO series are %v, want only kept", got)
	}
}

// TestEvictAllAnalyzerSeriesDeletesTheSLOSeries is the same for the cycle on
// which every model is gone: that path has its own delete list, and a model's
// SLO series were left to the ten-minute sweep there.
func TestEvictAllAnalyzerSeriesDeletesTheSLOSeries(t *testing.T) {
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	metrics.PublishSLO(metrics.SLOModelSnapshot{Namespace: "ns", ModelName: "gone",
		Objectives: []metrics.SLOObjectiveReading{{Objective: constants.SLOObjectiveTTFT, Statistic: "p90",
			TargetSeconds: 3, Observed: 1, Attainment: 0.95, Requests: 300}},
	}, time.Now())
	count := func() int {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, mf := range mfs {
			if strings.HasPrefix(mf.GetName(), "wva_slo_") {
				n += len(mf.GetMetric())
			}
		}
		return n
	}
	if count() == 0 {
		t.Fatal("setup: nothing published")
	}
	e := &Engine{metricsEmitter: metrics.NewMetricsEmitter(), lastAnalyzerSeries: map[string]analyzerSeries{
		"ns/gone": {namespace: "ns", modelID: "gone"},
	}}
	e.evictAllAnalyzerSeries()
	if n := count(); n != 0 {
		t.Fatalf("with every model gone, %d wva_slo_* series remain", n)
	}
}
