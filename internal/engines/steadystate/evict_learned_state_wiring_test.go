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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// evictSpy is a domain.Analyzer that records how its eviction entry point was
// called. It analyzes nothing: these tests are about the call, not the result.
type evictSpy struct {
	calls    int
	timeouts []time.Duration
}

func (s *evictSpy) Name() string { return "evict-spy" }

func (s *evictSpy) Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	return nil, nil
}

func (s *evictSpy) EvictStaleHistory(timeout time.Duration) int {
	s.calls++
	s.timeouts = append(s.timeouts, timeout)
	return 0
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
		"swept on the same timeout the read path already checks (RollingAverage.Stale)")
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
