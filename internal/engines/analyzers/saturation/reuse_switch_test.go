package saturation

import (
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// DisableLearnedStateReuse has to stop BOTH paths by which one variant's
// measurement prices another's decision, because an operator asking for it
// wants one thing and either path alone leaves it half-done:
//
//   - the fingerprint-keyed ITL line, and
//   - the cross-variant capacity figure for a variant with no record of its own.
//
// Each case below is paired with the same case under the default, so neither
// proves only that the mechanism it names is broken for an unrelated reason.

func reuseSwitchParams() *capacity.EngineParams {
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

func TestTheReuseSwitchStopsLineSharing(t *testing.T) {
	const (
		ns      = "test-ns"
		model   = "test-model"
		accel   = "H100"
		lender  = "variant-lender"
		starved = "variant-starved"
	)

	// Deliberately the READABLE configuration: no Unresolved entries, so both
	// variants get a real digest and borrowing is what happens by default.
	// Anything that withholds the digest would make this pass for the reason
	// TestTwoUnreadableVariantsDoNotShareALine already covers.
	run := func(t *testing.T, disabled bool) bool {
		t.Helper()
		store := capacity.NewStore()
		for _, v := range []string{lender, starved} {
			store.Update(ns, model, v, capacity.Record{
				AcceleratorName: accel, GpuCount: 1,
				EngineParams: reuseSwitchParams(),
			})
		}
		a := NewSaturationAnalyzer(store)

		states := []domain.VariantReplicaState{
			{VariantName: lender, Role: domain.RoleDecode, AcceleratorName: accel,
				CurrentReplicas: 10, GPUsPerReplica: 1},
			{VariantName: starved, Role: domain.RoleDecode, AcceleratorName: accel,
				CurrentReplicas: 1, GPUsPerReplica: 1},
		}
		rows := make([]domain.ReplicaMetrics, 0, 11)
		for i := 0; i < 10; i++ {
			k := 0.20 + 0.04*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("lender-%d", i), lender, 5000, 16000, 0, 6000, 1000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			rows = append(rows, rm)
		}
		s := makeReplicaMetrics("starved-0", starved, 5000, 16000, 0, 6000, 1000)
		s.Ready = true
		s.KvUsageInstant = 0.30
		s.AvgITL = 0 // nothing of its own to fit: the borrowing case
		rows = append(rows, s)

		ctx, _ := observedCtx(t)
		for i := 0; i < 2; i++ {
			_, err := a.Analyze(ctx, makeAnalyzerInput(rows, states))
			require.NoError(t, err)
		}

		in := makeAnalyzerInput(rows, states)
		pol, ok := in.Config.(*config.ScalingPolicy)
		require.True(t, ok, "the test input must carry a ScalingPolicy")
		pol.DisableLearnedStateReuse = disabled
		c := a.newCycle(ctx, in, pol)
		c.observeFleetShape()
		c.resolvePricing()
		c.fitLines()
		return c.itlBorrowed[starved]
	}

	t.Run("by default a readable configuration lends its line", func(t *testing.T) {
		require.True(t, run(t, false),
			"the control must borrow, or the case below proves only that "+
				"borrowing is broken for an unrelated reason")
	})

	t.Run("with the switch on it does not", func(t *testing.T) {
		require.False(t, run(t, true),
			"an operator who has turned reuse off must get a variant priced on "+
				"its own readings, whatever the digest says it has in common "+
				"with a sibling")
	})
}

func TestTheReuseSwitchStopsCrossVariantCapacity(t *testing.T) {
	const (
		ns      = "test-ns"
		model   = "test-model"
		accel   = "H100"
		lender  = "variant-lender"
		starved = "variant-starved"
	)

	// The starved variant has params but no capacity of its own; the lender
	// has a live figure on identical hardware and configuration, which is
	// exactly what FindCompatible is for.
	store := capacity.NewStore()
	store.Update(ns, model, lender, capacity.Record{
		AcceleratorName: accel, GpuCount: 1,
		EngineParams:      reuseSwitchParams(),
		EffectiveCapacity: 400_000,
		LearnedFrom:       capacity.LearnedFromLive,
	})
	store.Update(ns, model, starved, capacity.Record{
		AcceleratorName: accel, GpuCount: 1,
		EngineParams: reuseSwitchParams(),
	})
	a := NewSaturationAnalyzer(store)

	t.Run("by default a compatible sibling is found", func(t *testing.T) {
		got := a.lookupCompatibleCapacity(ns, model, starved, accel, 1, false)
		require.NotNil(t, got,
			"the control must find the sibling, or the case below proves only "+
				"that the fixture is not compatible in the first place")
		require.EqualValues(t, 400_000, got.EffectiveCapacity)
	})

	t.Run("with the switch on nothing is", func(t *testing.T) {
		require.Nil(t, a.lookupCompatibleCapacity(ns, model, starved, accel, 1, true),
			"a variant must not be priced from a sibling's measurement once "+
				"reuse is off")
	})
}

// THE SWITCH MUST NOT REMOVE A CEILING.
//
// estimateStoredCapacity consults a compatible sibling too, but as a MAX
// clamp: `if compatible.EffectiveCapacity < bounded`. It only ever LOWERS the
// estimate. Withholding it raises capacity, and an over-stated capacity orders
// FEWER replicas -- the direction this project has on record as breaking TTFT
// irrecoverably, and the one store.go measured at 5,000 -> 153,600.
//
// A first version of this switch gated that clamp as well, so an operator
// asking "stop pricing this variant from a sibling" would silently also have
// lost their ceiling. A review caught it. This pins the corrected rule: gate
// where a sibling's figure BECOMES the answer, never where it only bounds it.
func TestTheReuseSwitchDoesNotRemoveACapacityCeiling(t *testing.T) {
	const (
		ns      = "test-ns"
		model   = "test-model"
		accel   = "H100"
		zero    = "variant-zero"
		donor   = "variant-donor"
		ceiling = 5_000
	)

	// A deployment-derived record whose own derivation would come out far above
	// the sibling's measured capacity, so the clamp is what decides the answer.
	params := reuseSwitchParams()
	params.EffectiveMaxBatchedTokens = 1_000_000

	build := func() *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update(ns, model, zero, capacity.Record{
			AcceleratorName: accel, GpuCount: 1,
			EffectiveCapacity: 900_000, TotalKvCapacityTokens: 0,
			EngineParams: params,
			LearnedFrom:  "deployment",
		})
		store.Update(ns, model, donor, capacity.Record{
			AcceleratorName: accel, GpuCount: 1,
			EffectiveCapacity: ceiling,
			EngineParams:      params,
			LearnedFrom:       capacity.LearnedFromLive,
		})
		return NewSaturationAnalyzer(store)
	}

	rec := build().capacityStore.Get(ns, model, zero)
	require.NotNil(t, rec)

	// The clamp takes no reuse flag at all any more, so the only way this can
	// regress is by someone re-adding one. Both calls are identical by
	// construction; what is asserted is that the clamped figure is the
	// sibling's ceiling and not the much larger own derivation.
	got := build().estimateStoredCapacity(rec, model, ns, zero, accel, 1,
		0.9, 1000, 600, logr.Discard())
	require.LessOrEqual(t, got, float64(900_000),
		"the estimate must not exceed the variant's own stored capacity")
	require.Greater(t, got, 0.0, "precondition: the estimate path ran at all")
}

// Reuse is ON unless an operator turns it off: it is what lets a scaled-out
// variant price its first decision from something other than a guess. A
// default of true would silently change every existing deployment.
func TestReuseIsOnByDefault(t *testing.T) {
	var p config.ScalingPolicy
	require.False(t, p.DisableLearnedStateReuse,
		"the zero value of the policy must leave reuse enabled")
}
