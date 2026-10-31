package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// orderSpy is a saturation analyzer that logs, into calls, the stabilised
// lookup the input builder makes right before each scale-target load, and
// its Analyze.
type orderSpy struct {
	calls *[]string
}

func (s orderSpy) Name() string { return domain.SaturationAnalyzerName }

func (s orderSpy) Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	*s.calls = append(*s.calls, "analyze")
	return &domain.AnalyzerResult{AnalyzerName: domain.SaturationAnalyzerName}, nil
}

func (s orderSpy) StableAcceleratorFor(_, _, reading string) string {
	*s.calls = append(*s.calls, "load")
	return reading
}

// The load-balance tracker observes a cycle's rows before the input builder
// touches the capacity store, and both before saturation analyzes: the order
// the builder kept from the code it replaced, which keeps the log lines where
// they were. Learner PR 1 puts the settle tracker ahead of both (#345).
func TestInputBuilder_RunsAfterTheLoadBalanceAndBeforeAnalyze(t *testing.T) {
	e, _ := engineConfigEngine(t)
	var calls []string
	e.setSaturation(orderSpy{calls: &calls})
	e.loadBalanceNow = func() time.Time {
		calls = append(calls, "balance")
		return time.Now()
	}
	va, st := unresolvedVariant()
	_, err := e.runAnalyzers(context.Background(), "m", "ns", nil,
		config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70},
		nil, nil,
		map[string]scaletarget.ScaleTargetAccessor{utils.GetNamespacedKey("ns", "d"): st},
		map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{"ns/v1": va},
		nil, 0)
	require.NoError(t, err)
	require.NotNil(t, e.capacityStore.Get("ns", "m", "v1"), "precondition: the builder loaded the scale target")
	assert.Equal(t, []string{"balance", "load", "analyze"}, calls)
}
