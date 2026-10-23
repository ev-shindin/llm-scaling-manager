package saturation

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// floorOneReplica applies the throughput floor to one decode replica holding
// resident tokens with queued requests charged queueCharge as residency, at
// arrival rate lambda, with a saturated throughput of mu from samples readings
// of its own window, and returns the model's demand after the floor.
func floorOneReplica(lambda, mu float64, samples int, resident, queueCharge float64, queued int) float64 {
	a := NewSaturationAnalyzer(capacity.NewStore())
	in := makeAnalyzerInput(nil, nil)
	in.ArrivalRate = lambda
	rcs := []capacity.ReplicaCapacity{{
		VariantName: "v", PodName: "p", QueueLength: queued, LocalQueueDemand: int64(queueCharge),
		SaturatedThroughput: mu, SaturatedThroughputSamples: samples,
	}}
	vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 260_622}}
	return a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs,
		resident+queueCharge, nil, nil, 0, false, logr.Discard())
}

// The floor swaps a queue's residency charge for its drain price, which may
// lower demand. With lambda lagging a load step it lowered demand by the whole
// queue: on the two-namespace rerun at 11:20:09, lambda 0.8 req/s with 160
// queued, 499 446 became the 260 123 resident and the analyzer's rc fell
// 312 936 -> 47 265 in one cycle.
func TestThroughputFloor_DoesNotLowerDemandOnALaggingArrivalRate(t *testing.T) {
	const resident, charge = 260_123.0, 239_323.0
	got := floorOneReplica(0.8, 6.0, 2, resident, charge, 160)
	assert.InDelta(t, resident+charge, got, 1, "the occupancy and queue demand stands")
}

// Nor on a term that may not order: at 07:46:12 on the first run a single
// reading (mayOrder=false) took 356 392 to 274 556.
func TestThroughputFloor_DoesNotLowerDemandOnAHeldTerm(t *testing.T) {
	const resident, charge = 274_556.0, 81_836.0
	got := floorOneReplica(2.5, 6.0, 1, resident, charge, 59)
	assert.InDelta(t, resident+charge, got, 1)
}

// What the swap is for still happens: with an arrival rate that accounts for
// what is resident and a mu that may order, a standing queue is priced as
// work to drain within BacklogDrainSeconds -- here 376 k against the 499 k
// its residency charge comes to.
func TestThroughputFloor_PricesABacklogAsDrainWhenLambdaHolds(t *testing.T) {
	const resident, charge = 260_123.0, 239_323.0
	got := floorOneReplica(6.0, 6.0, 2, resident, charge, 160)
	want := (6.0 + 160.0/60) / 6.0 * 260_622
	assert.InDelta(t, want, got, 1)
	assert.Less(t, got, resident+charge)
	assert.Greater(t, got, resident)
}
