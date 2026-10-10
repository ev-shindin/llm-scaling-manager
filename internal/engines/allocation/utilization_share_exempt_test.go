package allocation

import (
	"slices"
	"testing"
	"time"
)

// The cluster run that motivated the exemption: GPUs moved from L to Q as Q's
// burst ended (its draining backlog still read as need) and L's began. Both
// transfers landed, so L had just given and Q had just received -- the
// reversal hold. L then needed 8 and held 3 while Q held 7 against a need of 2,
// and the hold kept the GPUs on Q for 18 minutes, L's whole burst.
func TestShareDeepShortfallIsFundedThroughTheReversalHold(t *testing.T) {
	tm := simTimings()
	t0 := time.Unix(1000, 0)
	plan := func(lNeed float64, lHeld, qHeld int) []ShareTransfer {
		l := NewShareLedger()
		held := map[string]int{"L": lHeld + 2, "Q": qHeld - 2}
		// L gave two replicas to Q; both landed.
		for range 2 {
			l.Start(ShareTransfer{Donor: "L", Receiver: "Q", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
		}
		after := map[string]int{"L": lHeld, "Q": qHeld - 2}
		l.Observe(after, t0.Add(time.Minute), tm)
		l.TakeReleased()
		after["Q"] = qHeld
		l.Observe(after, t0.Add(2*time.Minute), tm)
		// Demand flips: L bursts, Q's backlog has drained.
		roles := []ShareRole{
			{Key: "L", Weight: 1, Need: lNeed, Ceiling: 8, ReplicaGPUs: 1},
			{Key: "Q", Weight: 1, Need: 2, Ceiling: 8, ReplicaGPUs: 1},
		}
		in := SharePlanInput{Roles: roles, Held: after, Thresholds: map[string]float64{"L": 0.8, "Q": 0.8},
			Budget: 10, Tolerance: 0.15}
		started := make([]ShareTransfer, 0, 2)
		for i := range 2*ShareConfirmCycles + 2 {
			// All inside the reversal hold.
			now := t0.Add(3*time.Minute + time.Duration(30*i)*time.Second)
			if now.Sub(t0) >= tm.ReversalHold {
				t.Fatalf("setup: the cycles run past the reversal hold (%v)", tm.ReversalHold)
			}
			started = append(started, PlanShareTransfers(l, in, now, tm).Started...)
		}
		return started
	}
	back := func(ts []ShareTransfer) bool {
		return slices.ContainsFunc(ts, func(s ShareTransfer) bool { return s.Donor == "Q" && s.Receiver == "L" })
	}

	if got := plan(8, 3, 7); !back(got) {
		t.Fatalf("L at 3 of a need of 8, Q at 7 of 2: no transfer back inside the hold: %v", got)
	}
	// Control: L only mildly short (3 of 3.6, above ShareUrgentHeldFraction):
	// the hold still stands, so a swing cannot ping-pong GPUs.
	if got := plan(3.6, 3, 7); back(got) {
		t.Fatalf("L at 3 of 3.6 is not deeply short; the hold must stand: %v", got)
	}
}

func TestShareReversalExempt(t *testing.T) {
	r := func(need float64) ShareRole { return ShareRole{Need: need} }
	for _, c := range []struct {
		name           string
		rcHeld, dnLeft int
		rc, dn         ShareRole
		want           bool
	}{
		{"deeply short receiver, donor keeps its need", 3, 5, r(8), r(2), true},
		{"mildly short receiver", 3, 5, r(3.6), r(2), false},
		{"receiver at its need", 8, 5, r(8), r(2), false},
		{"donor would fall below its need", 3, 1, r(8), r(2), false},
		{"receiver with no need", 0, 5, r(0), r(2), false},
	} {
		if got := shareReversalExempt(c.rcHeld, c.rc, c.dnLeft, c.dn); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
