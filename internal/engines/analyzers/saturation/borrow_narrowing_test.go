package saturation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
)

// The borrowed-line flag follows the figure in USE, not the mere existence of a
// borrowed line, and this is the control for that narrowing.
//
// It was disclosed as uncovered for one round, on the grounds that the case
// needed a variant holding both a borrowed line and its own measured window.
// It does, and the case is ordinary: borrowing is driven by the ITL window,
// which needs ten observations spread over k, while the saturated-throughput
// window needs two readings from a queue above its threshold. A variant whose
// engine reports no ITL at all can fill the second window and never the first,
// which is exactly the fixture below.
//
// Why the narrowing matters: such a variant has earned the right to order on
// its own readings. Flagging it would route a measured figure into the floor's
// capped branch and hold a fleet that measured itself -- the same class of
// error as the gate it belongs to, in the opposite direction.
func TestLineBorrowedIsNotStampedOnAnOwnMeasuredFigure(t *testing.T) {
	const (
		ns      = "test-ns"
		model   = "test-model"
		accel   = "H100"
		lender  = "variant-lender"
		starved = "variant-starved"
	)

	params := func() *capacity.EngineParams {
		return &capacity.EngineParams{
			Engine:                    inferenceengine.EngineVLLM,
			GpuMemoryUtilization:      0.9,
			BlockSize:                 16,
			KvCacheDtype:              "auto",
			WeightDtype:               "auto",
			TensorParallelSize:        1,
			MaxNumSeqs:                256,
			EffectiveMaxBatchedTokens: 8192,
		}
	}
	store := capacity.NewStore()
	for _, v := range []string{lender, starved} {
		store.Update(ns, model, v, capacity.Record{
			AcceleratorName: accel, GpuCount: 1, EngineParams: params(),
		})
	}
	a := NewSaturationAnalyzer(store)

	states := []domain.VariantReplicaState{
		{VariantName: lender, Role: domain.RoleDecode, AcceleratorName: accel,
			CurrentReplicas: 10, GPUsPerReplica: 1},
		{VariantName: starved, Role: domain.RoleDecode, AcceleratorName: accel,
			CurrentReplicas: 1, GPUsPerReplica: 1},
	}

	// tokenRate varies per cycle because recordSaturatedThroughput Touch()es a
	// repeat instead of counting it: a window fed one number forever holds one
	// sample, which is how the first draft of this test kept the derived
	// figure in use and asserted the flag's absence for the wrong reason.
	rows := func(tokenRate float64) []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, 11)
		for i := 0; i < 10; i++ {
			k := 0.20 + 0.04*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("lender-%d", i), lender, 5000, 16000, 0, 6000, 1000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			out = append(out, rm)
		}
		// The starved variant: no ITL, so its ITL window stays empty and it
		// must borrow -- but a queue of 6 is over the fixture's threshold of
		// 5, which is what makes k2 OBSERVED and lets a saturated completion
		// rate be recorded at all. The rate is generation tokens/s over the
		// 1000-token output, so ~5.4 req/s either way: the test must not be
		// able to pass merely because the two figures differ in size.
		starvedRow := makeReplicaMetrics("starved-0", starved, 5000, 16000, 6, 6000, 1000)
		starvedRow.Ready = true
		starvedRow.KvUsageInstant = 0.30
		starvedRow.AvgITL = 0
		starvedRow.GenerationTokenRate = tokenRate
		return append(out, starvedRow)
	}

	ctx, _ := observedCtx(t)

	// Three cycles, two minutes apart. The lender publishes a line in the
	// first; the starved variant's window needs readings spaced at least
	// ThroughputSampleSpacing apart, so without the clock moving it would
	// RaiseLast() the one sample it has forever. Both conditions have to hold
	// at once for the narrowing to have anything to narrow.
	base := time.Now()
	for i, rate := range []float64{5400, 5450, 5420} {
		at := base.Add(time.Duration(i) * 2 * time.Minute)
		a.now = func() time.Time { return at }
		_, err := a.Analyze(ctx, makeAnalyzerInput(rows(rate), states))
		require.NoError(t, err)
	}
	require.NotEmpty(t, a.itlLines, "the lender's fit must have been published")

	in := makeAnalyzerInput(rows(5400), states)
	pol, ok := in.Config.(*config.ScalingPolicy)
	require.True(t, ok, "the test input must carry a ScalingPolicy")

	c := a.newCycle(ctx, in, pol)
	c.observeFleetShape()
	c.resolvePricing()
	c.fitLines()
	caps, err := c.priceReplicas(ctx)
	require.NoError(t, err)

	// Both halves of the premise, asserted rather than assumed. Either one
	// failing makes the assertion below vacuous: with no borrow there is
	// nothing to suppress, and with no own window the derived figure is in use
	// and the flag SHOULD be set.
	require.True(t, c.itlBorrowed[starved],
		"the starved variant must have borrowed a line, or this test asserts "+
			"the absence of a flag that was never a candidate")

	var seen bool
	for _, rc := range caps {
		if rc.VariantName != starved {
			continue
		}
		seen = true
		require.GreaterOrEqual(t, rc.SaturatedThroughputSamples, floor.MinThroughputSamplesToOrder,
			"the starved variant must have filled its own saturated window, or "+
				"the derived figure is in use and there is no narrowing to test")
		require.False(t, rc.SaturatedThroughputBorrowed,
			"its window is its own, not a neighbouring bucket's")
		require.False(t, rc.SaturatedThroughputDerived,
			"an own measured window beats the derived figure (useDerived), which "+
				"is the condition the flag is narrowed by")

		// The assertion the test exists for.
		require.False(t, rc.SaturatedThroughputLineBorrowed,
			"the variant holds a borrowed line but was priced from its OWN "+
				"measured window, so flagging it would hold a fleet that has "+
				"measured itself")
	}
	require.True(t, seen, "no record for the starved variant: the test proves nothing")
}
