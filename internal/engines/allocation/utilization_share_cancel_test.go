package allocation

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

// A one-replica move is called off when its donor's demand comes back
// before the donor has released. Replayed from a two-model run: the busy
// model held three of four GPUs; its load generator was swapped, it read
// almost no demand for two cycles, and it started giving one replica to the
// light model "to even out headroom by weight". Its demand was back 15s
// later, a whole replica above what it keeps after the move -- the move
// exactly reversed -- but a reversal had to be more than twice the band (one
// replica, at 1-GPU replicas) below the donor's target, so the move ran on
// through the donor's scale-down window, landed the busy model's replica on
// the light one six minutes later, and left the busy one short until a move
// back. Two moves were called off at the same kind of gap earlier in that
// run, where the donor was two replicas short. Both orders, with 1-GPU
// replicas and with whole 8-GPU nodes: the band is half a replica either way,
// so twice it is one replica at both sizes.
func TestShareOneReplicaMoveIsCalledOffWhenTheDonorsDemandReturns(t *testing.T) {
	const m = time.Minute
	for _, g := range []int{1, 8} {
		x := float64(g)
		phases := []stallPhase{{30 * m, stallNeeds(0.4*x, 2.6*x)}, {m, stallNeeds(0.4*x, 0.6*x)}, {15 * m, stallNeeds(0.4*x, 2.6*x)}}
		for _, order := range []struct {
			name    string
			busy    string
			phases  []stallPhase
			initial map[string]int
		}{
			{"B busy", "B", phases, map[string]int{"A": g, "B": 3 * g}},
			{"A busy", "A", stallSwap(phases), map[string]int{"A": 3 * g, "B": g}},
		} {
			name := fmt.Sprintf("%d-GPU replicas, %s", g, order.name)
			cycles := stallLoopSized(simTimings(), g, order.initial, false, order.phases, nil)
			started, cancelled, lowest := 0, 0, 3*g
			for _, c := range cycles {
				if c.at >= 30*m {
					started += c.started
					cancelled += c.cancelled
					lowest = min(lowest, c.held[order.busy])
				}
			}
			if started == 0 {
				t.Fatalf("%s, setup: no move started while the busy model read almost no demand; the scenario no longer reaches the rule", name)
			}
			if lowest < 3*g || cancelled == 0 {
				t.Errorf("%s: after its demand came back the busy model fell to %d of its %d GPUs, with %d of %d moves called off: "+
					"a move exactly reversed ran on", name, lowest, 3*g, cancelled, started)
			}
		}
	}
}

// A busy model on four or more replicas has a one-replica move called off
// too. The band is the tolerance of the target, never under half a replica,
// so at the default 0.15 twice the band passes one replica above a target of
// about 3.3 replicas: judged on twice the band alone, a busy model on four
// replicas or more was never short enough for its move to be called off,
// and lost the replica its demand had come back for. The busy model holds n
// of n+1 replicas at a need of n-0.4 beside a light one at 0.8; for one
// minute it reads n-2.6, one replica less, and starts giving that one
// replica to the light model; then its demand is back. n from 3 (the test
// above, where twice the band is still one replica) to 8, with 1-GPU
// replicas and whole 8-GPU nodes, both orders.
func TestShareOneReplicaMoveIsCalledOffOnABusyModelOfManyReplicas(t *testing.T) {
	const m = time.Minute
	for _, g := range []int{1, 8} {
		x := float64(g)
		for n := 3; n <= 8; n++ {
			busyNeed, dip := (float64(n)-0.4)*x, (float64(n)-2.6)*x
			phases := []stallPhase{{30 * m, stallNeeds(0.8*x, busyNeed)}, {m, stallNeeds(0.8*x, dip)}, {15 * m, stallNeeds(0.8*x, busyNeed)}}
			for _, order := range []struct {
				name    string
				busy    string
				phases  []stallPhase
				initial map[string]int
			}{
				{"B busy", "B", phases, map[string]int{"A": g, "B": n * g}},
				{"A busy", "A", stallSwap(phases), map[string]int{"A": n * g, "B": g}},
			} {
				name := fmt.Sprintf("%d-GPU replicas, busy on %d of %d, %s", g, n, n+1, order.name)
				cycles := stallLoopQuota(simTimings(), g, n+1, order.initial, false, order.phases, nil)
				onDip, cancelled, lowest := 0, 0, n*g
				for _, c := range cycles {
					if c.at >= 30*m {
						cancelled += c.cancelled
						lowest = min(lowest, c.held[order.busy])
					}
					if c.at >= 30*m && c.at < 32*m {
						onDip += c.started
					}
				}
				if onDip != 1 {
					t.Fatalf("%s, setup: %d moves started on the dip, want the one replica; the scenario no longer reaches the boundary", name, onDip)
				}
				if lowest < n*g || cancelled == 0 {
					t.Errorf("%s: after its demand came back the busy model fell to %d of its %d GPUs, and the move was not called off: "+
						"a move exactly reversed ran on", name, lowest, n*g)
				}
			}
		}
	}
}

// A transfer whose donor is not planned this cycle -- frozen, or not
// collected -- is not called off on that donor. Absent from what the group
// holds, the donor reads as holding nothing, net of the transfer below zero,
// and so a whole replica under a target of zero.
func TestShareUnplannedDonorDoesNotCallItsMoveOff(t *testing.T) {
	tm := simTimings()
	t0 := time.Unix(1_000_000, 0)
	l := NewShareLedger()
	tr := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, map[string]int{"A": 4, "B": 1}, t0, tm)
	l.ConfirmStarted(tr.ID, nil)
	in := SharePlanInput{Roles: []ShareRole{{Key: "B", Weight: 1, Need: 3, Floor: 1, MinFloor: 1, ReplicaGPUs: 1}},
		Held: map[string]int{"B": 1}, Thresholds: map[string]float64{"B": 0.85}, DownThresholds: map[string]float64{"B": 0.7},
		Budget: 8, Tolerance: 0.15, PhysicalFree: math.MaxInt}
	plan := PlanShareTransfers(l, in, t0.Add(time.Minute), tm)
	if slices.Contains(plan.Cancelled, tr.ID) {
		t.Fatalf("the transfer of a donor not planned this cycle was called off: %v", plan.Cancelled)
	}
	if got, ok := l.Transfer(tr.ID); !ok || got.State != ShareReleasing {
		t.Fatalf("setup: the transfer is %+v (found %v), want still releasing", got, ok)
	}
}

// A real swap right after a dip called off waits out the reversal hold, and
// no longer. This is the accepted price of calling the move off (issue #285,
// option b): the cancel holds the donor -> receiver pair for ReversalHold, so
// that fast swaps stay planned on the mean, and a swap in that same direction
// one minute after the dip's return lands after 17m30s, against 7m30s with no
// dip -- and 6m30s before this change, when the move the dip started ran on
// and took the busy model's replica in the meantime. The delay shrinks as the
// swap comes later and is gone once the hold has run out. The bound is the
// no-dip time plus the hold: a later swap is a regression, not the price.
// With 1-GPU replicas and whole 8-GPU nodes.
func TestShareSwapRightAfterACalledOffDipWaitsOnlyTheReversalHold(t *testing.T) {
	const m = time.Minute
	tm := simTimings()
	for _, g := range []int{1, 8} {
		x := float64(g)
		busy, idle := stallNeeds(0.4*x, 2.6*x), stallNeeds(2.6*x, 0.4*x)
		noDip := []stallPhase{{31 * m, busy}, {40 * m, idle}}
		base, ok := stallReached(stallLoopSized(tm, g, map[string]int{"A": g, "B": 3 * g}, false, noDip, nil), 31*m, 3*g, g)
		if !ok {
			t.Fatalf("%d-GPU replicas, control: with no dip the swap never reached 3:1", g)
		}
		for _, after := range []time.Duration{m, 2 * m, 4 * m, 6 * m, 10 * m} {
			ph := []stallPhase{{30 * m, busy}, {m, stallNeeds(0.4*x, 0.6*x)}, {after, busy}, {40 * m, idle}}
			cycles := stallLoopSized(tm, g, map[string]int{"A": g, "B": 3 * g}, false, ph, nil)
			cancelled := 0
			for _, c := range cycles {
				cancelled += c.cancelled
			}
			if cancelled == 0 {
				t.Fatalf("%d-GPU replicas, swap %v after the dip, setup: nothing was called off; the dip no longer reaches the rule", g, after)
			}
			d, ok := stallReached(cycles, 31*m+after, 3*g, g)
			if !ok || d > base+tm.ReversalHold {
				t.Errorf("%d-GPU replicas: a swap %v after a dip called off reached 3:1 after %v (reached %v), want within %v (no dip) + %v (the hold)",
					g, after, d, ok, base, tm.ReversalHold)
			}
			t.Logf("%d-GPU replicas: swap %v after the dip reached 3:1 after %v; no dip %v", g, after, d, base)
		}
	}
}

// A single wobble called off on its donor does not hold the swap back after
// it on the mean. The swap and swap back of issue #269
// (TestShareSingleSwapBackIsNotHeldOnTheMean), with the first swap's load
// going back to the busy model for w minutes, o minutes into it: a move
// toward it starts, and is called off when the load returns. A cancel for the
// donor is a change of direction only at the donor's second turn of demand
// inside the swing window (ShareLedger.CancelDonorBack); counted as one every
// time, the wobble made the swap back a third change of direction, held the
// busy model on its mean in 36 of these 74 variants against 18 with the
// cancel judged only on the receiver, and made its swap back slower than with
// the rule off in 22, against 2. Bounds: no more than those 18 and 2, the
// figures measured before the donor-side cancel existed (main at the time
// passes at exactly 18 and 2): a regression guard, not a replay that failed
// first. This change measures 10 and 2, so the bound on slower swaps back has
// no margin -- any variant made slower trips it. Swaps back are compared by
// the cycles the busy model is short after the swap back. With 1-GPU
// replicas and with whole 8-GPU nodes.
func TestShareWobbleCalledOffDoesNotHoldTheSwapBack(t *testing.T) {
	const m = time.Minute
	tm, _ := DeriveShareTimings(ShareTimingInputs{Cycle: simCycle, ReplicaStart: 240 * time.Second}, nil)
	off := tm
	off.SwingWindow = 0
	const back = singleSwapBackAt
	served := func(need float64, held int) float64 { return math.Min(need, 1.5*float64(held)) }
	variants, cancels, onMean, slower := 0, 0, 0, 0
	for _, g := range []int{1, 8} {
		x := float64(g)
		initial := map[string]int{"A": g, "B": g}
		for _, o := range []int{0, 1, 2, 3, 4, 6, 8, 10, 12} {
			for _, w := range []int{0, 2, 3, 4, 6} {
				if w == 0 && o > 0 {
					continue
				}
				ph := []stallPhase{{5 * m, stallNeeds(0.1*x, 0.1*x)}, {25 * m, stallNeeds(2.9*x, 0.17*x)}}
				if o > 0 {
					ph = append(ph, stallPhase{time.Duration(o) * m, stallNeeds(0.7*x, 2.9*x)})
				}
				if w > 0 {
					ph = append(ph, stallPhase{time.Duration(w) * m, stallNeeds(2.9*x, 0.17*x)})
				}
				ph = append(ph, stallPhase{time.Duration(20-o-w) * m, stallNeeds(0.7*x, 2.9*x)},
					stallPhase{60 * m, stallNeeds(2.9*x, 0.65*x)})
				variants++
				var short [2]int
				held := false
				for i, tmx := range []ShareTimings{tm, off} {
					for _, c := range stallLoopSized(tmx, g, initial, true, ph, served) {
						if i == 0 {
							cancels += c.cancelled
						}
						if c.at < back {
							continue
						}
						if c.held["A"] < 3*g {
							short[i]++
						}
						if i == 0 && slices.Contains(c.swinging, "A") {
							held = true
						}
					}
				}
				if held {
					onMean++
				}
				if short[0] > short[1] {
					slower++
				}
			}
		}
	}
	if cancels == 0 {
		t.Fatal("setup: no transfer was called off in any variant; the wobbles no longer reach the rule")
	}
	if onMean > 18 {
		t.Errorf("the busy model was planned on its mean after the swap back in %d of %d variants, want at most 18", onMean, variants)
	}
	if slower > 2 {
		t.Errorf("the swap back was slower than with the rule off in %d of %d variants, want at most 2", slower, variants)
	}
	t.Logf("%d variants, %d called off: on the mean after the swap back in %d, slower than with the rule off in %d",
		variants, cancels, onMean, slower)
}
