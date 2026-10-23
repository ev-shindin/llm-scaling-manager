package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// targetRC is the replicas the engine orders for a demand on a fleet of n
// replicas of k1 tokens: RC = max(0, D / scaleUp - supply), in replicas.
func targetRC(demand, scaleUp float64, n int, k1 float64) float64 {
	return max(0, demand/scaleUp-float64(n)*k1) / k1
}

// floorAtTarget is the decode floor for n replicas each serving exactly the
// derived mu, with the derived figure as every replica's reading.
func floorAtTarget(n int, lambda, mu, k1, scaleUp float64) float64 {
	reps := make([]capacity.ReplicaCapacity, n)
	for i := range reps {
		reps[i] = capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: mu,
			SaturatedThroughputSamples: MinDerivedThroughputSamples, SaturatedThroughputDerived: true}
	}
	vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: n, PerReplicaCapacity: k1}}
	return floor.Estimate(lambda, reps, vcs, nil, floor.BacklogDrainSeconds, scaleUp, false, 0, nil).ByRole[domain.RoleDecode]
}

// At the fill the derived mu is priced for -- kPrice, "the occupancy the
// autoscaler actually targets" -- occupancy and the throughput floor must agree:
// neither orders. The floor used to divide by the engine's scale-up threshold a
// second time and ordered another 17.6 % of the fleet at exactly that point.
// Line, cache and shape are the two-namespace run's own (Qwen3-32B-FP8, H200,
// 1000 in / 500 out): ITL = 10.5 ms + 82 ms x k from the pods' counters.
func TestThroughputFloor_AgreesWithOccupancyAtTheTarget(t *testing.T) {
	cfg := &config.ScalingPolicy{KvCacheThreshold: 0.80, ScaleUpThreshold: 0.85}
	const kvTokens = 376_480
	params := &capacity.EngineParams{MaxNumSeqs: 256, EffectiveMaxBatchedTokens: 8192}
	line := itl.Model{A: 0.082, B: 0.0105}
	kPrice := pricingK(cfg)
	d := deriveMu(line, params, 1, kvTokens, shape.New(1000, 500, 0), kPrice, 500)
	require.True(t, d.ok)
	require.InDelta(t, 0.68, kPrice, 1e-9)
	k1 := float64(memoryBound(kvTokens, cfg.KvCacheThreshold))

	for _, n := range []int{1, 2, 4} {
		lambda := float64(n) * d.rate // every replica exactly at kPrice
		occupancy := float64(n) * kPrice * kvTokens
		fl := floorAtTarget(n, lambda, d.rate, k1, cfg.ScaleUpThreshold)
		assert.InDelta(t, 0, targetRC(occupancy, cfg.ScaleUpThreshold, n, k1), 1e-6, "N=%d: occupancy holds at the target", n)
		assert.InDelta(t, 0, targetRC(fl, cfg.ScaleUpThreshold, n, k1), 1e-6,
			"N=%d: the floor holds at the target too (floor util %.3f)", n, fl/(float64(n)*k1))
		// And it still orders past it: 5 % more arrivals than the fleet serves
		// at the target is 5 % more fleet.
		over := floorAtTarget(n, 1.05*lambda, d.rate, k1, cfg.ScaleUpThreshold)
		assert.InDelta(t, 0.05*float64(n), targetRC(over, cfg.ScaleUpThreshold, n, k1), 1e-6, "N=%d: orders past the target", n)
	}

	// The run's 08:02 cycle: two replicas at 95-97 sequences each (k 0.30-0.35),
	// no queue, lambda 10.4, derived mu 5.7. It ordered a third replica.
	fl := floorAtTarget(2, 10.4, 5.7, 301_184, cfg.ScaleUpThreshold)
	assert.InDelta(t, 0, targetRC(fl, cfg.ScaleUpThreshold, 2, 301_184), 1e-9,
		"two replicas at half fill with no queue are not short (floor util %.3f)", fl/602_368)
}

// A MEASURED mu is the rate at saturation, measured beside P, so the engine's
// headroom applies to it once and the floor is lambda / mu x P unchanged.
func TestThroughputFloor_MeasuredMuIsPricedAtSaturation(t *testing.T) {
	reps := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: 6, SaturatedThroughputSamples: 2}}
	vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 300_000}}
	f := floor.Estimate(6, reps, vcs, nil, floor.BacklogDrainSeconds, 0.85, false, 0, nil)
	assert.InDelta(t, 300_000, f.ByRole[domain.RoleDecode], 1e-6)
}
