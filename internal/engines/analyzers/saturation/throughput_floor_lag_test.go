package saturation

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// floorOneReplica applies the throughput floor to one decode replica holding
// resident tokens with queued requests charged queueCharge as residency, at
// arrival rate lambda, with a saturated throughput of 6 req/s from samples
// readings of its own window, and returns the model's demand after the floor.
func floorOneReplica(lambda float64, samples int, resident, queueCharge float64, queued int) float64 {
	const mu = 6.0
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
	got := floorOneReplica(0.8, 2, resident, charge, 160)
	assert.InDelta(t, resident+charge, got, 1, "the occupancy and queue demand stands")
}

// Nor on a term that may not order: at 07:46:12 on the first run a single
// reading (mayOrder=false) took 356 392 to 274 556.
func TestThroughputFloor_DoesNotLowerDemandOnAHeldTerm(t *testing.T) {
	const resident, charge = 274_556.0, 81_836.0
	got := floorOneReplica(2.5, 1, resident, charge, 59)
	assert.InDelta(t, resident+charge, got, 1)
}

// What the swap is for still happens: with an arrival rate that accounts for
// what is resident and a mu that may order, a standing queue is priced as
// work to drain within BacklogDrainSeconds -- here 376 k against the 499 k
// its residency charge comes to.
func TestThroughputFloor_PricesABacklogAsDrainWhenLambdaHolds(t *testing.T) {
	const resident, charge = 260_123.0, 239_323.0
	got := floorOneReplica(6.0, 2, resident, charge, 160)
	want := (6.0 + 160.0/60) / 6.0 * 260_622
	assert.InDelta(t, want, got, 1)
	assert.Less(t, got, resident+charge)
	assert.Greater(t, got, resident)
}

// The swap is blended in, not switched on: demand moves continuously as the
// arrival rate crosses FloorLagFraction of the resident KV. A hard switch
// took this replica from 499 446 to 260 123 between 2.95 and 3.0 req/s.
func TestThroughputFloor_TheSwapIsContinuousInLambda(t *testing.T) {
	const resident, charge = 260_123.0, 239_323.0
	below := floorOneReplica(2.95, 2, resident, charge, 160)
	above := floorOneReplica(3.0, 2, resident, charge, 160)
	t.Logf("demand at 2.95 req/s %.0f, at 3.0 req/s %.0f", below, above)
	assert.InDelta(t, below, above, 0.02*below, "a 2 %% rise in lambda moves demand by at most 2 %%")
	prev := resident + charge
	for _, lambda := range []float64{2.8, 3.2, 3.6, 4.0, 4.4, 4.8, 5.2, 6.0, 8.0} {
		got := floorOneReplica(lambda, 2, resident, charge, 160)
		t.Logf("lambda %.1f: demand %.0f", lambda, got)
		assert.LessOrEqual(t, prev-got, 0.15*(resident+charge),
			"at %.1f req/s demand fell by more than 15 %% of the measured demand in one step", lambda)
		assert.GreaterOrEqual(t, got, resident, "never under the resident KV")
		prev = got
	}
}

// floorTrust: nothing at or under FloorLagFraction, everything at or over
// FloorTrustFraction, linear between.
func TestFloorTrust(t *testing.T) {
	assert.Zero(t, floorTrust(0.13))
	assert.Zero(t, floorTrust(FloorLagFraction))
	assert.InDelta(t, 0.5, floorTrust((FloorLagFraction+FloorTrustFraction)/2), 1e-12)
	assert.InDelta(t, 1.0, floorTrust(FloorTrustFraction), 1e-12)
	assert.InDelta(t, 1.0, floorTrust(1.4), 1e-12)
}

// Review probe (round 2, a step above fleet capacity). One replica of mu 6
// holding P; the true arrival rate steps from 3 to 9 req/s, the [1m] rate
// ramps to it over a minute, and the queue grows at 3/s from 40. The lag
// test reads lambda against the resident KV, which a saturated replica holds
// whatever the rate, so it trusted the ramping rate and priced the growing
// queue as drain: 319 763 before the step, then 280 704 and 354 736 under the
// occupancy that held it. A queue that grew since the last cycle keeps the
// occupancy demand, and the order never dips under its pre-step value.
func TestThroughputFloor_AGrowingQueueKeepsTheOccupancyDemand(t *testing.T) {
	const mu, resident, perReq = 6.0, 260_123.0, 239_323.0 / 160
	a := NewSaturationAnalyzer(capacity.NewStore())
	pre := 0.0
	for ts := 0; ts <= 60; ts += 15 {
		in := makeAnalyzerInput(nil, nil)
		in.ArrivalRate = 3 + 6*min(float64(ts)/60, 1)
		q := 40 + 3*ts
		charge := perReq * float64(q)
		rcs := []capacity.ReplicaCapacity{{
			VariantName: "v", PodName: "p", QueueLength: q, LocalQueueDemand: int64(charge),
			SaturatedThroughput: mu, SaturatedThroughputSamples: 2,
		}}
		vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 260_622}}
		occupancy := resident + float64(int64(charge))
		got := a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs,
			occupancy, nil, nil, 0, false, logr.Discard())
		t.Logf("t=%2ds lambda read %.1f, queue %3d: demand %7.0f, occupancy %7.0f", ts, in.ArrivalRate, q, got, occupancy)
		if ts == 0 {
			pre = got
			continue
		}
		assert.GreaterOrEqual(t, got, pre, "t=%ds: demand fell under its pre-step %.0f while the queue grew", ts, pre)
		assert.GreaterOrEqual(t, got, occupancy-1, "t=%ds: demand under the occupancy holding a growing queue", ts)
	}
}

// Review probe (round 3, a one-request jitter flipped the swap). A standing
// queue reading 160, 161, 160, 161 on 15 s cycles, with lambda steady: each
// +1 cycle read as "grew", skipped the swap and priced the occupancy figure,
// and the next swapped back -- 326 185 <-> 500 942 at 4.75 req/s, 376 454 <->
// 500 941 at 6. Growth is counted past a tolerance (BacklogGrowthMinRequests,
// BacklogGrowthFraction) against the queue BacklogGrowthLookback ago.
func TestThroughputFloor_AQueueJitterDoesNotFlipTheSwap(t *testing.T) {
	const mu, resident, perReq = 6.0, 260_123.0, 239_323.0 / 160
	for _, lambda := range []float64{4.75, 6.0} {
		a := NewSaturationAnalyzer(capacity.NewStore())
		clock := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return clock }
		lo, hi := math.Inf(1), math.Inf(-1)
		var trace strings.Builder
		for i := range 12 {
			in := makeAnalyzerInput(nil, nil)
			in.ArrivalRate = lambda
			q := 160 + i%2
			charge := perReq * float64(q)
			rcs := []capacity.ReplicaCapacity{{
				VariantName: "v", PodName: "p", QueueLength: q, LocalQueueDemand: int64(charge),
				SaturatedThroughput: mu, SaturatedThroughputSamples: 2,
			}}
			vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 260_622}}
			got := a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs,
				resident+float64(int64(charge)), nil, nil, 0, false, logr.Discard())
			fmt.Fprintf(&trace, " %d:%.0f", q, got)
			if i > 0 {
				lo, hi = min(lo, got), max(hi, got)
			}
			clock = clock.Add(15 * time.Second)
		}
		t.Logf("lambda %.2f, queue:demand%s (max/min %.4f)", lambda, trace.String(), hi/lo)
		assert.Less(t, hi/lo, 1.02, "lambda %.2f: a queue standing at 160 +-1 moved demand %.0f..%.0f", lambda, lo, hi)
	}
}
