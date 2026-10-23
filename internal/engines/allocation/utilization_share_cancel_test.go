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
// the rule off in 22, against 2. Bounds: no more than those 18 and 2. Swaps
// back are compared by the cycles the busy model is short after the swap
// back. With 1-GPU replicas and with whole 8-GPU nodes.
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
