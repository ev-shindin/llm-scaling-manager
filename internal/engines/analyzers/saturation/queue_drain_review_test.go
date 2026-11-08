package saturation

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
)

// The drain price's SLO-side guards (queue_drain.go, #520; PR #533's
// review). Each runs SwitchQueuePricedAsDrain on and off.

// A lambda that is not a finite figure prices nothing: an +Inf ArrivalRate,
// and a NaN completion rate the arrival rate falls back on when the scheduler
// reports none. The builtin max passes a NaN, and an +Inf priced demand at
// +Inf; the floor refuses both (floor.Estimate), and the drain price reads
// them as no lambda. Both switch settings publish a finite demand at or over
// the resident KV.
func TestQueueDrain_ANonFiniteLambdaPricesNothing(t *testing.T) {
	const p = 100_000.0
	resident := 2 * 0.95 * p
	for _, c := range []struct {
		name        string
		arrival     float64
		requestRate float64
	}{
		{"ArrivalRate +Inf", math.Inf(1), 0},
		{"RequestRate NaN, no ArrivalRate", 0, math.NaN()},
	} {
		for _, on := range []bool{true, false} {
			clock := propStart
			a := drainAnalyzer(t, on, &clock)
			rows := []domain.ReplicaMetrics{
				drainRow("a", 60, 0.95, drainBatchCap, drainBatchCap),
				drainRow("b", 60, 0.95, drainBatchCap, drainBatchCap),
			}
			rows[0].RequestRate = c.requestRate
			got := drainCycle(a, c.arrival, rows, 0, 0, 0, p, resident)
			t.Logf("%s, switch %v: demand %v", c.name, on, got)
			assert.False(t, math.IsNaN(got) || math.IsInf(got, 0), "%s, switch %v: demand %v", c.name, on, got)
			assert.GreaterOrEqual(t, got, resident, "%s, switch %v", c.name, on)
		}
	}
}

// The drain horizon is the start, where it is longer than 60 s: a queue
// priced over 300 s on a 300 s start, (6 + 120 / 300) / (0.85 x 1) = 7.53
// replicas at 100 %. Over a fixed 60 s it would price 9.41. Off, the
// residency charge stands.
func TestQueueDrain_TheHorizonIsTheStart(t *testing.T) {
	const p, start = 100_000.0, 300.0
	a := NewSaturationAnalyzer(drainStore(propStart))
	d := drainRead{mu0: 1, throughput: 2, queues: []float64{60, 60}, perReplica: p}
	c := newDrainContext(6, map[string]float64{domain.RoleDecode: start}, nil, nil, propStart, MinMeasuredOfDerived)
	want, priced := a.priceDrain("test-ns", "test-model", domain.RoleDecode, d, c,
		drainTerm{want: 4.3 * p, resident: 1.9 * p})
	t.Logf("horizon %.0f s, %.2f replicas at 100 %%", priced.horizon, want/p)
	require.Equal(t, start, priced.horizon)
	assert.InDelta(t, (6+120.0/start)/FirstReadHaircut*p, want, 1)
	assert.Greater(t, (6+120.0/60)/FirstReadHaircut*p, want+p, "control: a 60 s horizon prices a replica more")
}

// No starting replica is credited against the queue: the engine subtracts
// them as anticipated supply already (#520, the rejected option). The same
// first cycle prices the same with five replicas starting as with none.
func TestQueueDrain_StartingReplicasEarnNoCredit(t *testing.T) {
	const p = 100_000.0
	rows := []domain.ReplicaMetrics{
		drainRow("a", 60, 0.95, drainBatchCap, drainBatchCap),
		drainRow("b", 60, 0.95, drainBatchCap, drainBatchCap),
	}
	price := func(on bool, pending int) float64 {
		clock := propStart
		return drainCycle(drainAnalyzer(t, on, &clock), 6, rows, pending, 0, 0, p, 2*0.95*p)
	}
	none, five := price(true, 0), price(true, 5)
	t.Logf("no replica starting %.2f, five starting %.2f replicas at 100 %%", none/p, five/p)
	assert.InDelta(t, none, five, 1, "starting replicas change nothing")
	assert.InDelta(t, price(false, 0), price(false, 5), 1, "control: off, nor do they")
}

// A haircut hold ends. One replica is reported starting for good (no age, or
// stuck) while the queue stands: the hold lasts at most drainHoldHorizons
// drain horizons (2 x 60 s) after it was set, then the floor's figure
// stands. A stuck replica is not starting at all. Off, there is no hold.
func TestQueueDrain_AHoldEndsWithAReplicaThatNeverLands(t *testing.T) {
	const p = 100_000.0
	rows := []domain.ReplicaMetrics{
		drainRow("a", 60, 0.95, drainBatchCap, drainBatchCap),
		drainRow("b", 60, 0.95, drainBatchCap, drainBatchCap),
	}
	for _, on := range []bool{true, false} {
		clock := propStart
		a := drainAnalyzer(t, on, &clock)
		var heldAt []int
		for i := range 20 {
			clock = propStart.Add(time.Duration(i) * propCycle)
			mu, samples := 0.0, 0
			if i > 0 {
				mu, samples = 1, 2
			}
			drainCycle(a, 6, rows, 1, mu, samples, p, 2*0.95*p)
			if _, held := a.drainHoldOf("test-ns", "test-model", domain.RoleDecode); held {
				heldAt = append(heldAt, i)
			}
		}
		t.Logf("switch %v: held on cycles %v", on, heldAt)
		if !on {
			assert.Empty(t, heldAt)
			continue
		}
		require.NotEmpty(t, heldAt)
		assert.LessOrEqual(t, heldAt[len(heldAt)-1], int(drainHoldHorizons*60/propCycle.Seconds())+1,
			"held past two horizons of a replica that never lands")
	}
	assert.Zero(t, floor.FreshStarting(1, 1, nil, 60), "a stuck replica is not starting")
	assert.Equal(t, 1, floor.FreshStarting(2, 1, nil, 60))
	assert.Equal(t, 1, floor.FreshStarting(2, 0, []float64{30, 200}, 60), "an age past two starts is not starting")
}

// Two mu readings and a growing queue, with lambda lagging the step (4 -> 8
// req/s on four replicas of true mu 1, read 4, 5, 6, 7, 8) and a one-cycle
// dip of the queue. The floor's mu reads 1.0, or 1.4 high as the staircase's
// derived 26 against the 17 to 19 its replicas served. The replicas with a
// queue complete their true 1 each, so the growing region prices at no more
// than that. (Adopted from the review.)
//
// Measured, at mu read 1.4 and P 100 000 (replicas at 100 %, on / off /
// need): cycle 1 11.27 / 5.80 / 9.67, cycle 2 12.27 / 7.00 / 10.67, cycle 3
// (the dip) 9.33 / 6.67 / 10.33, cycle 4 11.60 / 8.10 / 11.33, then 12.27 to
// 13.60 against 12.00. Held: from the cycle lambda reads the true rate
// (4) demand is at or over the true arrivals plus the queue over 60 s at the
// true mu; on every cycle after the first it is at or over 0.85 of that.
// Off, occupancy prices the queue as resident: under the need at P 100 000
// with the read high, up to 3.5x over it at P 20 000. The dip cycle is the
// one short on: lambda still
// reads 7 of 8, and a shrinking queue shows no arrivals above it.
// Cycle 0 is before any reading shows the step.
func TestQueueDrain_AGrowingQueueIsPricedAtWhatTheReplicasServe(t *testing.T) {
	for _, p := range []float64{100_000, 20_000} {
		for _, muRead := range []float64{1.0, 1.4} {
			run := func(on bool) (demand, need []float64) {
				clock := propStart
				a := drainAnalyzer(t, on, &clock)
				q := 10.0
				for i := range 8 {
					clock = propStart.Add(time.Duration(i) * propCycle)
					if i == 3 {
						q -= 5
					} else if i > 0 {
						q += 15
					}
					lambda := min(4+float64(i), 8)
					rows := make([]domain.ReplicaMetrics, 0, 4)
					for _, pod := range []string{"a", "b", "c", "d"} {
						rm := drainRow(pod, int(q), 0.95, 16, 16/muRead)
						rm.RequestRate = 1
						rows = append(rows, rm)
					}
					demand = append(demand, drainCycle(a, lambda, rows, 0, muRead, 2, p, 4*0.95*p)/p)
					need = append(need, 8+4*min(q, 60)/60)
				}
				return demand, need
			}
			on, need := run(true)
			off, _ := run(false)
			t.Logf("P %.0f mu read %.1f: on %v", p, muRead, scaledReplicas(on))
			t.Logf("P %.0f mu read %.1f: off %v, need %v", p, muRead, scaledReplicas(off), scaledReplicas(need))
			for i := 1; i < len(on); i++ {
				assert.GreaterOrEqual(t, on[i], 0.85*need[i], "P %.0f mu %.1f cycle %d", p, muRead, i)
				if i == 3 {
					// The dip cycle, pinned at its measured 0.90 of need.
					assert.InDelta(t, 0.90, on[i]/need[i], 0.01, "P %.0f mu %.1f: the dip cycle moved", p, muRead)
				}
				if i >= 4 {
					assert.GreaterOrEqual(t, on[i], need[i]-1e-9, "P %.0f mu %.1f cycle %d: under the true arrivals plus the queue", p, muRead, i)
				}
			}
		}
	}
}

// scaledReplicas is figures to two places.
func scaledReplicas(xs []float64) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%.2f", x)
	}
	return out
}

// firstReadKVSaturatedCase is a property case: four replicas of true mu 1
// saturated by their KV under their batch cap, so their RunningRequests /
// AvgServiceTime reads 1.45x high (the staircase's KV-saturated rows read
// 1.6x to 1.9x), before two mu readings, 8 req/s arriving, queues of 20. Such
// rows give no first read, and occupancy prices them: demand stays at or over
// the arrivals plus the queue over 60 s at the true mu. (Adopted from the
// review: as a first read it priced 7.57 against 9.33, 19 % under.)
func firstReadKVSaturatedCase() propCase {
	return propCase{class: classEdges, learner: "floor", name: "drain-first-read-kv-saturated-1.45x",
		family: familyDirect, tuning: tunedDefault, seeds: 1,
		check: func(uint64) error {
			got, need := firstReadKVSaturated(drainBatchCap * 3 / 4)
			return holds(got >= need, kindStale,
				"KV-saturated rows reading 1.45x high: demand %.2f replicas at 100 %% against %.2f", got, need)
		}}
}

// firstReadKVSaturated prices the case's cycle with each replica's batch at
// the given running count, under a stated cap of drainBatchCap, and returns
// the demand and the
// need, in replicas at 100 %.
func firstReadKVSaturated(running int) (got, need float64) {
	const p = 20_000.0
	clock := propStart
	a := NewSaturationAnalyzer(drainStore(clock))
	a.now = func() time.Time { return clock }
	rows := make([]domain.ReplicaMetrics, 0, 4)
	for _, pod := range []string{"a", "b", "c", "d"} {
		rows = append(rows, drainRow(pod, 20, 0.95, running, float64(running)/1.45))
	}
	return drainCycle(a, 8, rows, 0, 0, 0, p, 4*0.95*p) / p, 8 + 80.0/60
}

// The case's control: the same 1.45x read from rows at their cap would be a
// first read, and would price under the need. The at-cap reads the
// recordings showed are +5 % to +15 %, which the haircut bounds; it is the
// KV-saturated rows that read this high, and they give none.
func TestQueueDrain_AKVSaturatedFirstReadIsNotRead(t *testing.T) {
	got, need := firstReadKVSaturated(drainBatchCap * 3 / 4)
	t.Logf("under the cap: %.2f replicas at 100 %% against %.2f", got, need)
	assert.GreaterOrEqual(t, got, need)
	atCap, _ := firstReadKVSaturated(drainBatchCap)
	t.Logf("control, at the cap: %.2f", atCap)
	assert.Less(t, atCap, need, "control: read as a first read, a 1.45x row under-prices")
}

// While the backlog grows, mu is capped at what the queued replicas serve,
// clamped at MinMeasuredOfDerived (0.5) of the floor's mu, not dropped. The
// floor's mu reads 1.4x to 3.0x what four replicas truly serve (1 each); the
// rows sit one under their batch cap, so there is no first read, and lambda
// lags a 4 -> 8 req/s step. Dropping the cap under the bar priced 2.1x to
// 3.0x reads at the full floor mu, 0.56 to 0.66 of need; clamped, every
// cycle after the step is at or over need. Off, occupancy prices the queue
// as resident. (Adopted from the second review.)
func TestQueueDrain_TheServedCapIsClampedNotDropped(t *testing.T) {
	const p = 20_000.0
	for _, muRead := range []float64{1.4, 1.9, 2.1, 2.5, 3.0} {
		for _, on := range []bool{true, false} {
			clock := propStart
			a := drainAnalyzer(t, on, &clock)
			q := 10.0
			worst := math.Inf(1)
			demand := make([]float64, 0, 8)
			for i := range 8 {
				clock = propStart.Add(time.Duration(i) * propCycle)
				if i > 0 {
					q += 15
				}
				lambda := min(4+float64(i), 8)
				rows := make([]domain.ReplicaMetrics, 0, 4)
				for _, pod := range []string{"a", "b", "c", "d"} {
					rm := drainRow(pod, int(q), 0.95, drainBatchCap-1, float64(drainBatchCap-1)/muRead)
					rm.RequestRate = 1
					rows = append(rows, rm)
				}
				got := drainCycle(a, lambda, rows, 0, muRead, 2, p, 4*0.95*p) / p
				demand = append(demand, got)
				if i >= 1 {
					worst = min(worst, got/(8+4*min(q, 60)/60))
				}
			}
			t.Logf("floor mu read %.1f, switch %v: demand %v, least demand/need after the step %.2f",
				muRead, on, scaledReplicas(demand), worst)
			if on {
				assert.GreaterOrEqual(t, worst, 1.0, "floor mu read %.1f", muRead)
			}
		}
	}
}

// A shape change drops the haircut holds: the read they were taken at is the
// shape the fleet has left. A hold taken on the first cycle, a shape change
// on the next, and on the third (rows under their cap, so no read of their
// own, nine replicas still starting) nothing prices at the held read:
// occupancy stands, as with the switch off. The control runs the same cycles
// with no shape change, and the hold prices.
func TestQueueDrain_AShapeChangeDropsTheHold(t *testing.T) {
	const p = 100_000.0
	atCap := []domain.ReplicaMetrics{
		drainRow("a", 60, 0.95, drainBatchCap, drainBatchCap),
		drainRow("b", 60, 0.95, drainBatchCap, drainBatchCap),
	}
	under := []domain.ReplicaMetrics{
		drainRow("a", 60, 0.95, drainBatchCap-1, drainBatchCap-1),
		drainRow("b", 60, 0.95, drainBatchCap-1, drainBatchCap-1),
	}
	run := func(on, shapeChange bool) (float64, bool) {
		clock := propStart
		a := drainAnalyzer(t, on, &clock)
		drainCycle(a, 6, atCap, 0, 0, 0, p, 2*0.95*p)
		clock = propStart.Add(propCycle)
		drainCycleShape(a, 6, under, 9, 0, 0, p, 2*0.95*p, shapeChange)
		_, held := a.drainHoldOf("test-ns", "test-model", domain.RoleDecode)
		clock = propStart.Add(2 * propCycle)
		return drainCycle(a, 6, under, 9, 0, 0, p, 2*0.95*p), held
	}
	dropped, held := run(true, true)
	off, _ := run(false, true)
	kept, keptHeld := run(true, false)
	t.Logf("after a shape change %.2f (held %v), off %.2f, no shape change %.2f (held %v)",
		dropped/p, held, off/p, kept/p, keptHeld)
	assert.False(t, held, "the shape change drops the hold")
	assert.InDelta(t, off, dropped, 1, "no held read prices after the shape change")
	assert.True(t, keptHeld, "control: no shape change, the hold stands")
	assert.Greater(t, kept, off+1, "control: the held read prices the queue as drain, over occupancy here")
}

// A hold prices only while a backlog stands. After the first cycle's
// haircut price the queue drains to nothing with nine replicas still
// starting and the floor's mu orderable: the hold adds nothing, and the
// demand is the switch off's. The control keeps the queue standing, and the
// held price stands over the floor's.
func TestQueueDrain_AHoldPricesNothingWithoutABacklog(t *testing.T) {
	const p = 100_000.0
	rows := func(q int) []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{
			drainRow("a", q, 0.95, drainBatchCap, drainBatchCap),
			drainRow("b", q, 0.95, drainBatchCap, drainBatchCap),
		}
	}
	run := func(on bool, q int) float64 {
		clock := propStart
		a := drainAnalyzer(t, on, &clock)
		drainCycle(a, 6, rows(60), 0, 0, 0, p, 2*0.95*p)
		clock = propStart.Add(propCycle)
		return drainCycle(a, 6, rows(q), 9, 1, 2, p, 2*0.95*p)
	}
	drained, off := run(true, 0), run(false, 0)
	standing, standingOff := run(true, 60), run(false, 60)
	t.Logf("no backlog: on %.2f, off %.2f; backlog standing: on %.2f, off %.2f",
		drained/p, off/p, standing/p, standingOff/p)
	assert.InDelta(t, off, drained, 1, "no backlog: the hold prices nothing")
	assert.Greater(t, standing, standingOff, "control: with the queue standing the hold prices")
}
