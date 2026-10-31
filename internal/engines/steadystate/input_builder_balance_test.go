package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/balance"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
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

// spyView is a settle view that logs, into calls, each read the load-balance
// tracker makes of it, and whether the settle tracker had observed the
// replica by then.
type spyView struct {
	settle.View
	calls *[]string
}

func (v spyView) Replica(model string, key settle.Key) (settle.Replica, bool) {
	r, ok := v.View.Replica(model, key)
	read := "balance"
	if ok {
		read = "balance, settled"
	}
	*v.calls = append(*v.calls, read)
	return r, ok
}

// The settle tracker observes a cycle's rows first; the load-balance tracker
// then reads them, through the settle tracker's view, before the input
// builder touches the capacity store; and all of it comes before saturation
// analyzes. The builder kept the order of the code it replaced, which keeps
// the log lines where they were; learner PR 1 put the settle tracker ahead of
// both (#345).
func TestInputBuilder_RunsAfterTheSettleAndTheLoadBalanceAndBeforeAnalyze(t *testing.T) {
	e, _ := engineConfigEngine(t)
	var calls []string
	e.setSaturation(orderSpy{calls: &calls})
	e.loadBalance = balance.NewTracker(spyView{View: e.settleTracker(), calls: &calls})
	va, st := unresolvedVariant()
	rows := []domain.ReplicaMetrics{{PodName: "p-0", VariantName: "v1", Ready: true, ReadySince: time.Now().Add(-time.Hour)}}
	_, err := e.runAnalyzers(context.Background(), "m", "ns", rows,
		config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70},
		nil, nil,
		map[string]scaletarget.ScaleTargetAccessor{utils.GetNamespacedKey("ns", "d"): st},
		map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{"ns/v1": va},
		nil, 0)
	require.NoError(t, err)
	require.NotNil(t, e.capacityStore.Get("ns", "m", "v1"), "precondition: the builder loaded the scale target")
	assert.Equal(t, []string{"balance, settled", "load", "analyze"}, calls)
}
