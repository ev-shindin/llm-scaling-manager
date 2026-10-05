package saturation

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The exact field set of the derived-mu record.
//
// logContract asserts that the fields a reader joins on are PRESENT. This
// asserts the set is exactly this and nothing less, which is a different
// guarantee and the one the staged Analyze needs: the stages share state
// through a carrier struct, so a field dropped at a stage boundary -- muInput
// not reaching priceReplicas, say -- produces a log line that still parses,
// still carries modelID, and is quietly missing the one term that explains the
// decision. Nothing else in the suite would fail.
//
// Every term is here because the result alone cannot be attributed to one: a
// mu wrong by 8x looks identical whether the fault is the output length, the
// sequence count, the ITL line or the pricing point, and that ambiguity cost
// four discarded diagnoses in one session.
//
// Adding a field is a deliberate act: extend this list in the same commit.
var derivedMuFields = []string{
	"avgInputTokens",
	"avgOutputTokens",
	"itlA",
	"itlAtKPrice",
	"itlB",
	"itlZero",
	"kPrice",
	"kvReqFleet",
	"kvReqPerSeq",
	"maxNumSeqs",
	"muDivisor",
	"muInputTokens",
	"muShortWindow",
	"ok",
	"pod",
	"rate",
	"replicaKvTokens",
	"seqs",
	"tokenSec",
	"variant",
}

func TestDerivedMuCarriesEveryTerm(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H100",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	rm := makeReplicaMetrics("pod-1", "variant-d", 5000, 16000, 0, 6000, 1000)

	ctx, logs := observedCtx(t)
	a := NewSaturationAnalyzer(capacity.NewStore())
	_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states))
	require.NoError(t, err)

	got := requireLogged(t, logs, "derived-mu")
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	assert.Equal(t, derivedMuFields, keys,
		"the derived-mu record must carry exactly these terms; a field lost at a "+
			"stage boundary leaves a line that still parses and no longer explains "+
			"the decision")
}

// The stages run in an order that matters, and three of the dependencies are
// not visible from the call site -- they are reads of fields an earlier stage
// wrote. This pins them as behaviour rather than as a comment.
func TestAnalyzeStageOrderIsObservable(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H100",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	rm := makeReplicaMetrics("pod-1", "variant-d", 5000, 16000, 0, 6000, 1000)
	input := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states)

	t.Run("the shape is observed before mu is priced at it", func(t *testing.T) {
		// resolvePricing reads fleetOutput and stableOutput, both written by
		// observeFleetShape. Run out of order, muDivisor would fall through to
		// the 512 seed on a fleet that is plainly serving 1000.
		ctx, logs := observedCtx(t)
		a := NewSaturationAnalyzer(capacity.NewStore())
		_, err := a.Analyze(ctx, input)
		require.NoError(t, err)

		got := requireLogged(t, logs, "derived-mu")
		assert.Equal(t, float64(1000), got["muDivisor"],
			"the divisor must be the fleet's own reading, not DefaultExpectedOutputTokens")
		assert.Equal(t, float64(6000), got["avgInputTokens"],
			"and the shape must carry the prompt the fleet reported")
	})

	t.Run("replicas are priced before the fleet can settle its shape", func(t *testing.T) {
		// settleShape asks fleetHasMeasuredItself of the capacities
		// priceReplicas produced. With no capacities it cannot answer, and an
		// outstanding change would release on the first cycle.
		a := NewSaturationAnalyzer(capacity.NewStore())
		ctx1, _ := observedCtx(t)
		_, err := a.Analyze(ctx1, input)
		require.NoError(t, err)

		_, _, outstanding := a.fleetShapeState("test-ns", "test-model")
		assert.False(t, outstanding,
			"a first cycle has no prior shape, so nothing should be outstanding")
	})

	t.Run("a cancelled context stops before any demand is assembled", func(t *testing.T) {
		// priceReplicas is the only stage that can fail, and it must fail
		// before priceDemand runs -- a partial capacity list would aggregate
		// to a demand lower than the truth and read as a release.
		base, _ := observedCtx(t)
		ctx, cancel := context.WithCancel(base)
		cancel()
		a := NewSaturationAnalyzer(capacity.NewStore())
		result, err := a.Analyze(ctx, input)
		require.Error(t, err, "a cancelled cycle must not return a result")
		assert.Nil(t, result)
	})
}
