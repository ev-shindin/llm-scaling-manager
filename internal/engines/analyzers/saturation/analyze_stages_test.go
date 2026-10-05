package saturation

import (
	"context"
	"fmt"
	"maps"
	"slices"
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
	keys := slices.Collect(maps.Keys(got))
	slices.Sort(keys)

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

	// The ITL line has to be fitted before any replica is priced, and this is
	// the dependency that cost the most to learn: fitLines populates
	// c.itlModels, so running it after priceReplicas prices every decode
	// replica against the zero itl.Model. deriveMu then declines on IsZero(),
	// there is no derived mu, and the throughput floor emits nothing for the
	// role -- leaving occupancy alone, which under-sizes a fleet that is
	// keeping up.
	//
	// That reorder was shipped once on this branch (741e34f9) and passed ALL
	// 279 specs, old and new. The golden test above did not catch it because
	// it asserts the derived-mu record's key SET, and a replica priced off a
	// zero model logs exactly the right keys with zero values. So this asserts
	// the values.
	//
	// Ten-plus replicas of one variant in a single cycle is enough to fit:
	// each contributes one (k, ITL) observation, so DefaultMinSamples (10) and
	// DefaultMinKSpread (0.30) are both met without driving ten cycles.
	t.Run("the ITL line is fitted before replicas are priced", func(t *testing.T) {
		fitted := make([]domain.ReplicaMetrics, 0, 12)
		for i := 0; i < 12; i++ {
			k := 0.20 + 0.05*float64(i) // 0.20 .. 0.75, spread 0.55
			rm := makeReplicaMetrics(fmt.Sprintf("pod-%d", i), "variant-d",
				400_000, tracedKv, 10, 1000, 6000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			fitted = append(fitted, rm)
		}

		ctx, logs := observedCtx(t)
		a := NewSaturationAnalyzer(capacity.NewStore())
		_, err := a.Analyze(ctx, makeAnalyzerInput(fitted, states))
		require.NoError(t, err)

		got := requireLogged(t, logs, "derived-mu")
		assert.Equal(t, false, got["itlZero"],
			"fitLines must have populated itlModels before priceReplicas read it")
		for _, k := range []string{"rate", "seqs", "tokenSec", "itlA"} {
			v, ok := got[k].(float64)
			require.True(t, ok, "%s should be a float64, got %T", k, got[k])
			assert.Greater(t, v, 0.0,
				"%s must be positive: a replica priced off a zero ITL model reports zero here", k)
		}
		assert.Equal(t, true, got["ok"], "the derived mu must be usable")
	})

	// NOT tested here: that settleShape consumes priceReplicas' output. The
	// subtest that used to claim it was vacuous -- it built a single cycle, and
	// on a first cycle fleetShapeState reports nothing outstanding whether or
	// not settleShape ran at all, so it passed with the call deleted outright.
	//
	// The behaviour is genuinely covered, by the multi-cycle shape-change
	// scenarios: deleting c.settleShape(caps) fails shape_change_test.go:181
	// and :616 and mu_from_itl_test.go:293. A test that passes when the code it
	// names is removed is worse than no test, so it is gone rather than
	// reworded.

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
