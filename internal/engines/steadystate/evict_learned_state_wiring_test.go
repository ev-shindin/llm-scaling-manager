package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// evictSpy is a domain.Analyzer that records how its eviction entry point was
// called. It analyzes nothing: these tests are about the call, not the result.
type evictSpy struct {
	calls      int
	timeouts   []time.Duration
	retentions []time.Duration
}

func (s *evictSpy) Name() string { return "evict-spy" }

func (s *evictSpy) Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	return nil, nil
}

func (s *evictSpy) EvictStaleHistory(variantTimeout, bucketRetention time.Duration) saturation.Evicted {
	s.calls++
	s.timeouts = append(s.timeouts, variantTimeout)
	s.retentions = append(s.retentions, bucketRetention)
	return saturation.Evicted{}
}

// plainAnalyzer is a domain.Analyzer with no eviction entry point, which is
// what an injected test double normally is.
type plainAnalyzer struct{}

func (plainAnalyzer) Name() string { return "plain" }

func (plainAnalyzer) Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	return nil, nil
}

// TestEvictStaleLearnedStateIsWiredIntoTheCycle is the regression this closes.
// EvictStaleHistory and Store.EvictStale were both written and both covered by
// pure-function specs, and neither had a caller anywhere on the reconcile
// path -- so the analyzer kept one entry per variant it had EVER seen, for the
// lifetime of the process, which is the leak its own comment warns about. A
// pure-function spec passes either way; only driving the cycle can tell.
func TestEvictStaleLearnedStateIsWiredIntoTheCycle(t *testing.T) {
	spy := &evictSpy{}
	e := &Engine{
		Config:               config.NewTestConfig(),
		optimizer:            allocation.NewCostAwareOptimizer(),
		saturationV2Analyzer: spy,
		capacityStore:        capacity.NewStore(),
	}

	// One cycle over one model. It collects nothing, which is the cheapest
	// cycle there is -- and the sweep must still have run, because it is
	// once-per-cycle fleet maintenance rather than per-model work.
	e.optimizeV2(context.Background(), map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"chat/m": {{
			ObjectMeta: metav1.ObjectMeta{Name: "chat-a", Namespace: "chat"},
			Spec:       llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{ModelID: "m"},
		}},
	}, nil)

	require.Equal(t, 1, spy.calls, "a cycle must sweep learned state exactly once")
	require.Equal(t, []time.Duration{capacity.HistoryEvictionTimeout}, spy.timeouts,
		"per-variant state is swept on the same timeout the read path already checks (RollingAverage.Stale)")
	require.Equal(t, []time.Duration{capacity.HistoryRetention}, spy.retentions,
		"bucket-keyed windows are KEPT past the point they are trusted, so that a "+
			"measurement the read refuses still exists to be refused -- passing the trust "+
			"horizon here would delete it at the top of the cycle and k2 would reach the "+
			"derived figure after all")
}

// AND THE OTHER HALF OF THE SWEEP, which nothing observed.
//
// evictStaleLearnedState sweeps two stores, and this change exists because
// NEITHER had a production caller. The spy above pins the analyzer's half; the
// capacity store's half had no test at all, so deleting that call would have
// left the whole suite green -- the same shape as the defect being fixed, in
// the commit fixing it. A reviewer found it.
//
// Ageing a record requires writing LearnedAt through the pointer Get returns,
// which reaches past the store's documented contract ("a caller reads it and
// writes through Update, never into it") -- because Update stamps LearnedAt to
// now and the store offers no other way to age an entry. That it takes this to
// observe the sweep at all is the reason the call went untested.
func TestEvictStaleLearnedStateSweepsTheCapacityStore(t *testing.T) {
	store := capacity.NewStore()
	e := &Engine{
		Config:               config.NewTestConfig(),
		optimizer:            allocation.NewCostAwareOptimizer(),
		saturationV2Analyzer: &evictSpy{},
		capacityStore:        store,
	}

	store.Update("ns", "m", "v1", capacity.Record{
		AcceleratorName: "NVIDIA-H200",
		GpuCount:        1,
		EngineParams:    &capacity.EngineParams{Engine: inferenceengine.EngineVLLM},
	})
	fresh := store.Get("ns", "m", "v1")
	require.NotNil(t, fresh, "the record must exist before the sweep can be observed")

	// Fresh: the sweep must leave it alone. This is the positive control --
	// without it, a sweep that deleted everything unconditionally would pass
	// the assertion below.
	e.evictStaleLearnedState(context.Background())
	require.NotNil(t, store.Get("ns", "m", "v1"),
		"a record inside the eviction timeout must survive")

	// Older than the store's own timeout.
	fresh.LearnedAt = time.Now().Add(-2 * capacity.EvictionTimeout)
	e.evictStaleLearnedState(context.Background())
	require.Nil(t, store.Get("ns", "m", "v1"),
		"the per-cycle sweep must evict a record past EvictionTimeout, which is "+
			"the half of evictStaleLearnedState no test observed")
}

// TestEvictStaleLearnedStateSurvivesAnAnalyzerThatCannotEvict pins the failure
// mode of reaching eviction through a type assertion: saturationV2Analyzer is
// typed domain.Analyzer so tests can inject one, and neither an injected
// analyzer without the method nor an unset field may panic the cycle.
func TestEvictStaleLearnedStateSurvivesAnAnalyzerThatCannotEvict(t *testing.T) {
	withPlain := &Engine{saturationV2Analyzer: plainAnalyzer{}, capacityStore: capacity.NewStore()}
	require.NotPanics(t, func() { withPlain.evictStaleLearnedState(context.Background()) })

	bare := &Engine{} // no analyzer and no store
	require.NotPanics(t, func() { bare.evictStaleLearnedState(context.Background()) })
}
