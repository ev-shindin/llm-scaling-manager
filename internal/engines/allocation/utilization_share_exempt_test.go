package allocation

import (
	"slices"
	"testing"
	"time"
)

// burstReplay is the cluster run that motivated shareHardImbalance: GPUs moved
// from L to Q as Q's burst ended (its draining backlog still read as need) and
// L's began. Both transfers landed, so L had just given and Q had just
// received -- the reversal hold. L then needs lNeed holding lHeld while Q holds
// qHeld against a need of qNeed; C, when cHeld > 0, is a third role with
// surplus.
type burstReplay struct {
	lNeed, qNeed        float64
	lHeld, qHeld, cHeld int
	// fresh skips the L -> Q history: no reversal hold, the control.
	fresh bool
	// band is the hard-imbalance band; the zero value is the default.
	band ShareRebalance
	// hold, when set, adds to the ledger before the planner runs.
	hold func(l *ShareLedger, now time.Time)
}

// run returns what the planner started inside the reversal hold.
func (b burstReplay) run(t *testing.T) []ShareTransfer {
	t.Helper()
	tm := simTimings()
	t0 := time.Unix(1000, 0)
	l := NewShareLedger()
	after := map[string]int{"L": b.lHeld, "Q": b.qHeld}
	if !b.fresh {
		held := map[string]int{"L": b.lHeld + 2, "Q": b.qHeld - 2}
		for range 2 {
			tr := l.Start(ShareTransfer{Donor: "L", Receiver: "Q", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
			l.ConfirmStarted(tr.ID, nil)
		}
		after["Q"] = b.qHeld - 2
		l.Observe(after, t0.Add(time.Minute), tm)
		l.TakeReleased()
		after["Q"] = b.qHeld
		l.Observe(after, t0.Add(2*time.Minute), tm)
	}
	roles := []ShareRole{
		{Key: "L", Weight: 1, Need: b.lNeed, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: b.qNeed, Ceiling: 8, ReplicaGPUs: 1},
	}
	thresholds := map[string]float64{"L": 0.8, "Q": 0.8}
	budget := 10
	if b.cHeld > 0 {
		roles = append(roles, ShareRole{Key: "C", Weight: 1, Need: 1, Ceiling: 8, ReplicaGPUs: 1})
		after["C"] = b.cHeld
		thresholds["C"] = 0.8
		budget += b.cHeld
	}
	in := SharePlanInput{Roles: roles, Held: after, Thresholds: thresholds, Budget: budget, Tolerance: 0.15,
		Rebalance: b.band}
	start := t0.Add(3 * time.Minute)
	if b.hold != nil {
		b.hold(l, start)
	}
	started := make([]ShareTransfer, 0, 2)
	for i := range 2*ShareConfirmCycles + 2 {
		now := start.Add(time.Duration(30*i) * time.Second)
		if now.Sub(t0) >= tm.ReversalHold {
			t.Fatalf("setup: the cycles run past the reversal hold (%v)", tm.ReversalHold)
		}
		started = append(started, PlanShareTransfers(l, in, now, tm).Started...)
	}
	return started
}

func moved(ts []ShareTransfer, donor, receiver string) bool {
	return slices.ContainsFunc(ts, func(s ShareTransfer) bool { return s.Donor == donor && s.Receiver == receiver })
}

func TestShareHardImbalanceIsRebalancedThroughTheReversalHold(t *testing.T) {
	if got := (burstReplay{lNeed: 8, qNeed: 2, lHeld: 3, qHeld: 7}).run(t); !moved(got, "Q", "L") {
		t.Fatalf("L at 3 of a need of 8, Q at 7 of 2: no transfer back inside the hold: %v", got)
	}
	// Control: Q is not calm after giving (5 of 6 is above ShareRebalanceDonorLoad),
	// so this is no hard imbalance and the hold stands...
	mild := burstReplay{lNeed: 8, qNeed: 5, lHeld: 3, qHeld: 7}
	if got := mild.run(t); moved(got, "Q", "L") {
		t.Fatalf("Q at 5 of 6 after giving is not calm; the hold must stand: %v", got)
	}
	// ...which is what withholds it: without the history Q gives.
	mild.fresh = true
	if got := mild.run(t); !moved(got, "Q", "L") {
		t.Fatalf("control: Q -> L is not planned even without the hold: %v", got)
	}
}

// The band is the operator's: switched off, a hard imbalance waits out the
// hold; widened, a move the default band holds goes through.
func TestShareRebalanceBandIsConfigurable(t *testing.T) {
	if got := (burstReplay{lNeed: 8, qNeed: 2, lHeld: 3, qHeld: 7, band: ShareRebalance{Off: true}}).run(t); moved(got, "Q", "L") {
		t.Fatalf("rebalance off: Q gave to L through the hold: %v", got)
	}
	// Q at 5 of the 6 it keeps (0.83) is not calm by default, and is under a
	// donor load limit of 0.9 (with the receiver's above it).
	wide := ShareRebalance{ReceiverLoad: 1, DonorLoad: 0.9}
	if got := (burstReplay{lNeed: 8, qNeed: 5, lHeld: 3, qHeld: 7, band: wide}).run(t); !moved(got, "Q", "L") {
		t.Fatalf("donor load limit 0.9: Q at 5 of 6 is calm, yet the hold stood: %v", got)
	}
}

// A hard imbalance waits for no confirm cycle: the first cycle starts it.
func TestShareHardImbalanceSkipsTheConfirmCycles(t *testing.T) {
	roles := []ShareRole{
		{Key: "L", Weight: 1, Need: 8, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 2, Ceiling: 8, ReplicaGPUs: 1},
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"L": 3, "Q": 7},
		Thresholds: map[string]float64{"L": 0.8, "Q": 0.8}, Budget: 10, Tolerance: 0.15}
	if got := PlanShareTransfers(NewShareLedger(), in, time.Unix(1000, 0), simTimings()).Started; !moved(got, "Q", "L") {
		t.Fatalf("a hard imbalance was not rebalanced on the first cycle: %v", got)
	}
	// Control: Q not calm -- the move waits for the confirm cycles.
	in.Roles[1].Need = 5
	if got := PlanShareTransfers(NewShareLedger(), in, time.Unix(1000, 0), simTimings()).Started; len(got) != 0 {
		t.Fatalf("a move that is no hard imbalance skipped the confirm cycles: %v", got)
	}
}

// The exemption lifts the reversal hold and nothing else: a donor backing off
// after an abort, one that has given all it can, and a cancelled pair stay
// held however hard the imbalance.
func TestShareHardImbalanceKeepsTheOtherHolds(t *testing.T) {
	for _, c := range []struct {
		name string
		hold func(l *ShareLedger, now time.Time)
	}{
		{"back-off", func(l *ShareLedger, now time.Time) { l.giveAfter["Q"] = now.Add(time.Hour) }},
		{"busy", func(l *ShareLedger, now time.Time) { l.steadyUntil["Q"] = now.Add(time.Hour) }},
		{"cancelled pair", func(l *ShareLedger, now time.Time) { l.noPair[sharePair{"Q", "L"}] = now.Add(time.Hour) }},
	} {
		if got := (burstReplay{lNeed: 8, qNeed: 2, lHeld: 3, qHeld: 7, hold: c.hold}).run(t); moved(got, "Q", "L") {
			t.Errorf("%s: Q gave to L through it: %v", c.name, got)
		}
	}
}

// A cancelled pair holds only that pair: the receiver is still funded by
// another donor. Held as a role, one model could keep another from receiving
// at all by timing its own load to cancel transfers to it.
func TestShareCancelledPairLeavesOtherDonors(t *testing.T) {
	pair := func(l *ShareLedger, now time.Time) { l.noPair[sharePair{"Q", "L"}] = now.Add(time.Hour) }
	got := (burstReplay{lNeed: 8, qNeed: 2, lHeld: 3, qHeld: 7, cHeld: 5, hold: pair}).run(t)
	if moved(got, "Q", "L") {
		t.Fatalf("the cancelled pair gave: %v", got)
	}
	if !moved(got, "C", "L") {
		t.Fatalf("L was not funded by C while only Q -> L is held: %v", got)
	}
}

// The exemption reaches the donor-set and node-set paths. R grows by whole
// 3x8-GPU groups (an LWS) and gave within the hold; D received within it.
func TestShareHardImbalanceReachesSetPaths(t *testing.T) {
	for _, path := range []string{"donor set", "node set"} {
		run := func(dNeed float64, hold bool) []ShareTransfer {
			roles := []ShareRole{
				{Key: "R", Weight: 1, Need: 72, Ceiling: 96, ReplicaGPUs: 24},
				{Key: "D", Weight: 1, Need: dNeed, Ceiling: 96, ReplicaGPUs: 8},
			}
			in := SharePlanInput{Roles: roles, Held: map[string]int{"R": 24, "D": 72},
				Thresholds: map[string]float64{"R": 0.8, "D": 0.8}, Budget: 96, Tolerance: 0.15,
				Grow: map[string]ShareVariant{"R": {Name: "r", GPUs: 24, PodGPUs: []int{8, 8, 8}}},
				Give: map[string]ShareVariant{"D": {Name: "d", GPUs: 8, PodGPUs: []int{8}}}}
			if path == "node set" {
				in.Nodes = map[string]ShareNode{}
				units := make([]ShareUnit, 0, 9)
				for i := range 9 {
					n := string(rune('a' + i))
					in.Nodes[n] = ShareNode{}
					units = append(units, ShareUnit{Pods: []SharePod{{Name: "d-" + n, Node: n, GPUs: 8}}})
				}
				in.DonorUnits = map[string][]ShareUnit{"D": units}
			}
			l := NewShareLedger()
			if hold {
				at := time.Unix(1000, 0).Add(-time.Minute)
				l.recordMove("R", false, "earlier", at, simTimings())
				l.recordMove("D", true, "earlier", at, simTimings())
			}
			return planCycles(in, l)
		}
		// D at 16 of the 48 it keeps is calm; at 40 of 48 it is not.
		if got := run(16, true); !moved(got, "D", "R") {
			t.Errorf("%s: a hard imbalance was not rebalanced through the hold: %v", path, got)
		}
		if got := run(40, false); !moved(got, "D", "R") {
			t.Errorf("%s: control: D at 40 is not tapped even without the hold: %v", path, got)
		}
		if got := run(40, true); moved(got, "D", "R") {
			t.Errorf("%s: D at 40 of 48 is not calm; the hold must stand: %v", path, got)
		}
	}
}

func TestShareHardImbalance(t *testing.T) {
	r := func(need float64) ShareRole { return ShareRole{Need: need} }
	hi, lo := ShareRebalanceReceiverLoad, ShareRebalanceDonorLoad
	for _, c := range []struct {
		name           string
		rcHeld, dnLeft int
		rc, dn         ShareRole
		want           bool
	}{
		{"short receiver, calm donor", 3, 6, r(8), r(2), true},
		{"receiver load exactly at its limit", 10, 10, r(10 * hi), r(1), true},
		{"receiver load just below its limit", 10, 10, r(10*hi - 0.01), r(1), false},
		{"donor load exactly at its limit", 3, 10, r(8), r(10 * lo), true},
		{"donor load just above its limit", 3, 10, r(8), r(10*lo + 0.01), false},
		{"receiver with no need", 0, 10, r(0), r(1), false},
		{"receiver holding nothing", 0, 10, r(1), r(1), true},
	} {
		if got := shareHardImbalance(ShareRebalance{}, c.rcHeld, c.rc, c.dnLeft, c.dn); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
