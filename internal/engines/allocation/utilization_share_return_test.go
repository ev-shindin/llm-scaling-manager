package allocation

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// returnsFromQ runs spareRun(lNeed, 0.4, 7) -- Q above its target of 2 -- for
// four cycles after mut, and counts the replicas Q returned.
func returnsFromQ(lNeed float64, mut func(in *SharePlanInput, l *ShareLedger)) int {
	in := spareRun(lNeed, 0.4, 7)
	l := NewShareLedger()
	if mut != nil {
		mut(&in, l)
	}
	n := 0
	for _, p := range planEach(l, in, 4) {
		for _, tr := range p.Started {
			if tr.Returned && tr.Donor == "Q" {
				n++
			}
		}
	}
	return n
}

// What holds a return back, and what does not.
func TestShareReturnGuards(t *testing.T) {
	if n := returnsFromQ(2, nil); n != 5 {
		t.Fatalf("control: Q returned %d, want 5", n)
	}
	for _, c := range []struct {
		name  string
		lNeed float64
		mut   func(*SharePlanInput, *ShareLedger)
		want  int
	}{
		{"Q backing off", 2, func(_ *SharePlanInput, l *ShareLedger) { l.giveAfter["Q"] = time.Unix(1e9, 0) }, 0},
		{"Q received within the reversal hold", 2, func(_ *SharePlanInput, l *ShareLedger) {
			l.recordMove("Q", true, "x", time.Unix(990, 0), spareTimings())
		}, 0},
		{"Q has no variant to give", 2, func(in *SharePlanInput, _ *ShareLedger) {
			in.Give = map[string]ShareVariant{"L": {Name: "l", GPUs: 1}}
		}, 0},
		{"a short role that cannot grow does not block", 5, func(in *SharePlanInput, _ *ShareLedger) {
			in.Give = map[string]ShareVariant{"Q": {Name: "q", GPUs: 1}}
			in.Grow = map[string]ShareVariant{"Q": {Name: "q", GPUs: 1}}
		}, 5},
		{"a short role held from filling does not block", 5, func(_ *SharePlanInput, l *ShareLedger) {
			l.HoldFill("L", time.Unix(1e9, 0))
		}, 5},
		{"transfers in flight do not hold a return", 0.4, func(in *SharePlanInput, l *ShareLedger) {
			for range ShareMaxConcurrentTransfers {
				l.Start(ShareTransfer{Donor: "L", DonorGPUs: 1, Entitled: true}, in.Held, time.Unix(990, 0), spareTimings())
			}
		}, 5},
	} {
		if got := returnsFromQ(c.lNeed, c.mut); got != c.want {
			t.Errorf("%s: Q returned %d, want %d", c.name, got, c.want)
		}
	}
}

// Only a return is cancelled when its donor turns short: a reserve refill is
// entitled, and a set contributor goes only with its primary.
func TestShareCancelsOnlyReturns(t *testing.T) {
	l := NewShareLedger()
	in := spareRun(2, 0.4, 7)
	l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 1, Entitled: true}, in.Held, time.Unix(1000, 0), spareTimings())
	p := PlanShareTransfers(l, spareRun(2, 8, 7), time.Unix(1015, 0), spareTimings())
	if len(p.Cancelled) != 0 {
		t.Fatalf("cancelled %v: a refill is not a return", p.Cancelled)
	}
}

// GPUs a return has not released are not idle: the donor's pod still runs on
// them, and the quota still counts it.
func TestShareUnreleasedIsNotIdle(t *testing.T) {
	l := NewShareLedger()
	held := map[string]int{"Q": 7}
	tm := spareTimings()
	l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 1, Entitled: true, Returned: true}, held, time.Unix(1000, 0), tm)
	l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 1, Entitled: true}, held, time.Unix(1000, 0), tm)
	if n := l.Unreleased(); n != 2 {
		t.Fatalf("unreleased %d, want 2 (a return and a refill)", n)
	}
	in := SharePlanInput{Budget: 10, PhysicalFree: math.MaxInt}
	if idle := shareIdle(l, in, l.Committed(map[string]int{"L": 3, "Q": 7})); idle != 0 {
		t.Fatalf("idle %d while both still release, want 0", idle)
	}
	held["Q"] = 5
	l.Observe(held, time.Unix(1015, 0), tm)
	if n := l.Unreleased(); n != 0 {
		t.Fatalf("unreleased %d after both released, want 0", n)
	}
}

// Transfers of one donor that abort together back it off once, not once each:
// five starts of one hard imbalance must not reach the 16x back-off at once.
func TestShareAbortsOfOneCycleBackOffOnce(t *testing.T) {
	l := NewShareLedger()
	tm := spareTimings()
	held := map[string]int{"L": 3, "Q": 7}
	for range 5 {
		tr := l.Start(ShareTransfer{Donor: "Q", Receiver: "L", GPUs: 1, DonorGPUs: 1}, held, time.Unix(1000, 0), tm)
		l.ConfirmStarted(tr.ID, nil)
	}
	at := time.Unix(1000, 0).Add(tm.ReleaseTimeout)
	l.Observe(held, at, tm)
	if l.aborts["Q"] != 1 {
		t.Fatalf("aborts %d, want 1", l.aborts["Q"])
	}
	if got := l.GiveAfter("Q"); !got.Equal(at.Add(tm.ReleaseTimeout)) {
		t.Fatalf("back-off until %v, want one release timeout (%v)", got, at.Add(tm.ReleaseTimeout))
	}
}

// An unpaced hard imbalance goes on to the next donor when the first reaches
// its target: L, short of 7, is funded by both Q and C within two cycles.
func TestShareHardImbalanceUsesEveryCalmDonor(t *testing.T) {
	roles := []ShareRole{
		{Key: "L", Weight: 1, Need: 11, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 0.4, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "C", Weight: 1, Need: 0.4, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"L": 1, "Q": 4, "C": 5},
		Thresholds: map[string]float64{"L": 0.85, "Q": 0.85, "C": 0.85}, Budget: 10, Tolerance: 0.15,
		PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	from := map[string]int{}
	for _, p := range planEach(NewShareLedger(), in, 2) {
		for _, tr := range p.Started {
			if tr.Receiver == "L" {
				from[tr.Donor]++
			}
		}
	}
	if from["Q"] == 0 || from["C"] == 0 || from["Q"]+from["C"] != 7 {
		t.Fatalf("L funded %v, want 7 replicas from both Q and C", from)
	}
}

// A multi-pod receiver funded by donor sets is unpaced too: once its hard
// imbalance is confirmed, it takes set after set in one cycle.
func TestShareHardImbalanceUnpacesDonorSets(t *testing.T) {
	roles := []ShareRole{
		{Key: "B", Weight: 1, Need: 10, Floor: 2, Ceiling: 10, ReplicaGPUs: 2},
		{Key: "A", Weight: 1, Need: 0.4, Floor: 1, Ceiling: 10, ReplicaGPUs: 1},
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"B": 2, "A": 8},
		Thresholds: map[string]float64{"A": 0.85, "B": 0.85}, Budget: 10, Tolerance: 0.15,
		Give:         map[string]ShareVariant{"A": {Name: "a", GPUs: 1, PodGPUs: []int{1}}},
		Grow:         map[string]ShareVariant{"B": {Name: "b", GPUs: 2, PodGPUs: []int{1, 1}}},
		PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	// Two ordinary moves fill the concurrency limit; an unpaced set does not
	// count it.
	l := NewShareLedger()
	for range ShareMaxConcurrentTransfers {
		l.Start(ShareTransfer{Donor: "A", DonorGPUs: 1, Entitled: true}, in.Held, time.Unix(900, 0), spareTimings())
	}
	plans := planEach(l, in, 2)
	sets := func(p SharePlan) int {
		n := 0
		for _, tr := range p.Started {
			if tr.Receiver == "B" {
				n++
			}
		}
		return n
	}
	if n := sets(plans[0]); n > 1 {
		t.Fatalf("first cycle funded %d sets, want at most 1: unconfirmed, it is paced", n)
	}
	if n := sets(plans[1]); n < 2 {
		t.Fatalf("second cycle funded %d sets, want the rest of the shortfall (2 or more)", n)
	}
	for _, p := range plans {
		for _, tr := range p.Started {
			if !tr.SkippedWaits {
				t.Fatal("a hard-imbalance set member did not skip the waits")
			}
		}
	}
}

// The idle fill's credit is node-aware: idle GPUs split 4 + 4 across nodes
// cannot place B's 8-GPU pod, so B is funded by a donor set rather than left
// to a fill that cannot land -- and A, above its target, does not return what
// B needs.
func TestShareIdleCreditNeedsAFit(t *testing.T) {
	plan := func(free map[string]int) []ShareTransfer {
		nodes := map[string]ShareNode{}
		for n, f := range free {
			nodes[n] = ShareNode{Free: f}
		}
		four := func(name string) ShareUnit { return ShareUnit{Pods: []SharePod{{Name: name, Node: "n1", GPUs: 4}}} }
		in := SharePlanInput{
			Roles: []ShareRole{
				{Key: "A", Weight: 1, Need: 4, Ceiling: 16, ReplicaGPUs: 4},
				{Key: "B", Weight: 1, Need: 64, Ceiling: 24, ReplicaGPUs: 8},
			},
			Held: map[string]int{"A": 24, "B": 16}, Thresholds: map[string]float64{"A": 0.8, "B": 0.8},
			Budget: 48, Tolerance: 0.15,
			Give:  map[string]ShareVariant{"A": {Name: "a", GPUs: 4, PodGPUs: []int{4}}},
			Grow:  map[string]ShareVariant{"B": {Name: "b", GPUs: 8, PodGPUs: []int{8}}},
			Nodes: nodes, DonorUnits: map[string][]ShareUnit{"A": {four("ns/a-0"), four("ns/a-1")}},
			PhysicalFree: math.MaxInt,
		}
		started := make([]ShareTransfer, 0, 4)
		for _, p := range planEach(NewShareLedger(), in, 3) {
			started = append(started, p.Started...)
		}
		return started
	}
	got := plan(map[string]int{"n1": 4, "n2": 4})
	funded, returned := false, false
	for _, tr := range got {
		funded = funded || tr.Receiver == "B"
		returned = returned || tr.Returned
	}
	if !funded || returned {
		t.Fatalf("fragmented idle: B funded %v (want true), A returned %v (want false): %v", funded, returned, got)
	}
	// Control: 8 free on one node places B's pod, and the fill funds it.
	for _, tr := range plan(map[string]int{"n1": 8}) {
		if tr.Receiver == "B" {
			t.Fatalf("a donor funded B though its pod fits free GPUs: %+v", tr)
		}
	}
}

// The unpaced path takes a receiver to its need, not to its headroom: L needs
// 5 and holds 3 (target 6). The first cycle moves one at the pace; the second,
// unpaced, takes L to its need of 5 -- the replica of headroom above it waits
// for an ordinary, paced move.
func TestShareUnpacedStopsAtNeed(t *testing.T) {
	moved := 0
	for _, p := range planEach(NewShareLedger(), spareRun(5, 0.4, 7), 2) {
		moved += qToL(p.Started)
	}
	if moved != 2 {
		t.Fatalf("Q -> L moved %d over two cycles, want 2 (3 to its need of 5)", moved)
	}
}

// An ordinary move takes one replica per donor per cycle.
func TestShareOrdinaryMoveIsOnePerDonor(t *testing.T) {
	in := spareRun(11, 0.4, 7)
	in.SkipWaits = ShareSkipWaits{Off: true}
	for i, p := range planEach(NewShareLedger(), in, 3) {
		if n := qToL(p.Started); n > 1 {
			t.Fatalf("cycle %d moved %d from one donor, want at most 1", i+1, n)
		}
	}
}

// A return is cancelled when its role turns short of its target alone, and
// when it turns busy alone. Q holds 7 and has returned some; then its demand
// rises.
func TestShareCancelsAReturnOnEitherTrigger(t *testing.T) {
	cancelled := func(returns int, qNeed float64) int {
		l := NewShareLedger()
		burstHistory(l)
		in := spareRun(2, qNeed, 7)
		for range returns {
			l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 1, Entitled: true, Returned: true}, in.Held, time.Unix(1000, 0), spareTimings())
		}
		return len(PlanShareTransfers(l, in, time.Unix(1015, 0), spareTimings()).Cancelled)
	}
	// 5 returned leave Q 2; a need of 1.1 makes its target 3 (2 + its headroom)
	// while it stays calm at 2 (1.1 <= 0.6 x 2).
	if n := cancelled(5, 1.1); n != 1 {
		t.Errorf("short of its target only: cancelled %d, want 1", n)
	}
	// 3 returned leave Q 4, its target at a need of 2.5; but 2.5 > 0.6 x 4.
	if n := cancelled(3, 2.5); n != 1 {
		t.Errorf("busy only: cancelled %d, want 1", n)
	}
	if n := cancelled(3, 1.5); n != 0 {
		t.Errorf("control, neither: cancelled %d, want 0", n)
	}
}

// The idle fill's credit is unpaced below need: L needs 6 and holds 2, with 4
// GPUs unassigned. All 4 are credited, so Q gives only the replica of
// headroom; credited at the pace of 2, Q would give 2 for GPUs that were free.
func TestShareIdleCreditIsUnpacedBelowNeed(t *testing.T) {
	in := spareRun(6, 0.4, 4)
	in.Held = map[string]int{"L": 2, "Q": 4}
	moved := 0
	for _, p := range planEach(NewShareLedger(), in, 2) {
		moved += qToL(p.Started)
	}
	if moved != 1 {
		t.Fatalf("Q -> L moved %d, want 1: the idle 4 cover L to its need", moved)
	}
}

// A receiver the last fill left short is not credited idle GPUs: whatever
// kept it short, the fill sees and the planner does not, so a donor funds it.
func TestShareFillShortReceiverIsFundedByADonor(t *testing.T) {
	in := spareRun(4, 0.4, 5) // L target 5, holding 3; 2 GPUs unassigned
	l := NewShareLedger()
	l.SetFillShort([]string{"L"})
	moved := 0
	for _, p := range planEach(l, in, 3) {
		moved += qToL(p.Started)
	}
	if moved == 0 {
		t.Fatal("a receiver the fill left short was credited idle GPUs; no donor funded it")
	}
}

// A returned spare does not count against the concurrency limit. A move that
// skipped the waits does: uncounted, ordinary headroom moves started beside a
// burst's and drained its donor for nothing.
func TestShareInFlightCountsEveryMoveButReturns(t *testing.T) {
	l := NewShareLedger()
	held := map[string]int{"L": 3, "Q": 7}
	tm := spareTimings()
	l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 1, Entitled: true, Returned: true}, held, time.Unix(1000, 0), tm)
	if n := l.InFlight(); n != 0 {
		t.Fatalf("in flight %d, want 0: a return is not counted", n)
	}
	l.Start(ShareTransfer{Donor: "Q", Receiver: "L", GPUs: 1, DonorGPUs: 1, SkippedWaits: true}, held, time.Unix(1000, 0), tm)
	if n := l.InFlight(); n != 1 {
		t.Fatalf("in flight %d, want 1: the move that skipped the waits", n)
	}
}

// What a releasing transfer gives no receiver is not idle: a return still
// runs on all of its GPUs, and an 8-GPU replica given to a 4-GPU receiver on
// the four the receiver does not get.
func TestShareUnreleasedCountsWhatNoReceiverGets(t *testing.T) {
	l := NewShareLedger()
	held := map[string]int{"L": 4, "Q": 24}
	tm := spareTimings()
	l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 8, Entitled: true, Returned: true}, held, time.Unix(1000, 0), tm)
	if n := l.Unreleased(); n != 8 {
		t.Fatalf("unreleased %d, want 8: the return goes to nobody yet", n)
	}
	l.Start(ShareTransfer{Donor: "Q", Receiver: "L", GPUs: 4, DonorGPUs: 8}, held, time.Unix(1000, 0), tm)
	if n := l.Unreleased(); n != 12 {
		t.Fatalf("unreleased %d, want 12: the return's 8 and the 4 L does not get", n)
	}
}

// While the quota covers every role at its recent peak plus its headroom, the
// spare is not capped and nothing is returned: no burst can find the GPUs it
// needs parked elsewhere, and a return would only make the next burst cold
// start. Control: once both have peaked at 8 (16 of 10), the cap binds.
func TestShareKeepsHeadroomWhileThePeaksFit(t *testing.T) {
	in := spareRun(2, 0.4, 7)
	in.Roles = []ShareRole{ // uncapped: the planner decides
		{Key: "L", Weight: 1, Need: 2, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 0.4, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}
	l := NewShareLedger()
	knownFor(l, time.Unix(1000, 0), "L", "Q")
	for i := range 4 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
		if p.HeadroomCapped || p.Returns != 0 {
			t.Fatalf("cycle %d: capped %v, returned %d, with every peak fitting the quota", i+1, p.HeadroomCapped, p.Returns)
		}
	}
	returned := 0
	for _, p := range planEach(NewShareLedger(), in, 4) {
		returned += p.Returns
	}
	if returned == 0 {
		t.Fatal("control: nothing returned once the peaks contend")
	}
}

// A role still giving is not a receiver (ShareLedger.Giving): a move that
// skips the waits must not send GPUs back to it while its own release to the
// other way still drains.
func TestShareNoReverseWhileReleasing(t *testing.T) {
	moved := func(reverse bool) int {
		l := NewShareLedger()
		in := spareRun(11, 0.4, 7)
		if reverse {
			tr := l.Start(ShareTransfer{Donor: "L", Receiver: "Q", GPUs: 1, DonorGPUs: 1, SkippedWaits: true},
				map[string]int{"L": 4, "Q": 6}, time.Unix(900, 0), spareTimings()) // past its window: no longer cancellable
			l.ConfirmStarted(tr.ID, nil)
		}
		n := 0
		for _, p := range planEach(l, in, 2) {
			n += qToL(p.Started)
		}
		return n
	}
	if n := moved(true); n != 0 {
		t.Fatalf("Q -> L moved %d while L -> Q still releases", n)
	}
	if moved(false) == 0 {
		t.Fatal("control: Q -> L did not move")
	}
}

// Returns are not blocked by a short role no donor can fund: no single
// donor replica fits L's 2-GPU pod, and no set can be formed, so L is
// unfunded -- and Q's spare, held back for it, would sit parked for good.
func TestShareUnfundedRoleDoesNotBlockReturns(t *testing.T) {
	in := spareRun(4, 0.4, 7)
	in.Held = map[string]int{"L": 2, "Q": 7}
	in.Roles[0].ReplicaGPUs = 2
	in.Give = map[string]ShareVariant{"Q": {Name: "q", GPUs: 1}}
	in.Grow = map[string]ShareVariant{"L": {Name: "l", GPUs: 2}}
	returned := 0
	for _, p := range planEach(NewShareLedger(), in, 5) {
		returned += p.Returns
	}
	if returned == 0 {
		t.Fatal("Q kept its spare parked for a role no donor can fund")
	}
}

// A return still releasing stays a return when another role turns short: it
// lands in the unassigned GPUs, where the idle fill places them, and is
// neither handed to the short role nor cancelled while its own role is calm.
// The short role waits on it (awaiting-return) instead of draining Q a second
// time for the same GPUs: what the returns free is still Unreleased, so the
// fill does not count it, and no transfer funds it either.
func TestShareReturnStaysAReturnWhenAReceiverTurnsShort(t *testing.T) {
	l := NewShareLedger()
	plans := planEach(l, spareRun(2, 0.4, 7), 3)
	if plans[1].Returns != 5 {
		t.Fatalf("setup: Q returned %d, want 5", plans[1].Returns)
	}
	p := PlanShareTransfers(l, spareRun(5, 0.4, 7), time.Unix(1045, 0), spareTimings())
	if len(p.Cancelled) != 0 {
		t.Fatalf("returns cancelled while Q is calm: %v", p.Cancelled)
	}
	if qToL(p.Started) != 0 {
		t.Fatalf("Q started transfers to L besides its returns: %+v", p.Started)
	}
	if n := l.Unreleased(); n != 5 {
		t.Fatalf("unreleased %d, want the 5 returned GPUs, not idle until released", n)
	}
	if p.Unfunded["L"] != ShareUnfundedAwaitingReturn {
		t.Fatalf("L waits on the returns and nothing said so: unfunded %v, withheld %v", p.Unfunded, p.Withheld)
	}
	n := 0
	for _, tr := range l.Transfers() {
		if tr.Returned {
			n++
			if tr.Receiver != "" {
				t.Fatalf("a return has a receiver: %+v", tr)
			}
		}
	}
	if n != 5 {
		t.Fatalf("%d returns in the ledger, want all 5 still returning", n)
	}
}

// With no headroom, a return and its cancel are still a replica apart: a
// return at need 2.9 is not cancelled at 3.05.
func TestShareCancelKeepsAReplicaGapAtNoHeadroom(t *testing.T) {
	l := NewShareLedger()
	burstHistory(l)
	in := spareRun(2, 2.9, 7)
	in.Headroom.Replicas = 0
	in.Roles = []ShareRole{
		{Key: "L", Weight: 1, Need: 2, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 2.9, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}
	// Q at 7 returns down to 5 (2.9 <= 0.6 x 5); cancelled only past 0.6 x 6.
	for range 2 {
		l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 1, Entitled: true, Returned: true}, in.Held, time.Unix(1000, 0), spareTimings())
	}
	in.Roles[1].Need = 3.05
	if p := PlanShareTransfers(l, in, time.Unix(1010, 0), spareTimings()); len(p.Cancelled) != 0 {
		t.Fatalf("cancelled %v at need 3.05, within a replica of the return", p.Cancelled)
	}
	in.Roles[1].Need = 3.7
	if p := PlanShareTransfers(l, in, time.Unix(1020, 0), spareTimings()); len(p.Cancelled) == 0 {
		t.Fatal("control: not cancelled at 3.7 (> 0.6 x 6)")
	}
}

// A free node the idle credit has given to one replica is not counted again
// for the next: B's first replica is credited to n1's 8 free GPUs, and its
// second is funded by a donor set on the nodes A's pods free -- not deferred
// to a fill that has no GPUs left to place it.
func TestShareIdleCreditSpendsItsNode(t *testing.T) {
	four := func(name, node string) ShareUnit {
		return ShareUnit{Pods: []SharePod{{Name: name, Node: node, GPUs: 4}}}
	}
	in := SharePlanInput{
		Roles: []ShareRole{
			{Key: "A", Weight: 1, Need: 4, Ceiling: 16, ReplicaGPUs: 4},
			{Key: "B", Weight: 1, Need: 64, Ceiling: 32, ReplicaGPUs: 8},
		},
		Held:       map[string]int{"A": 24, "B": 16},
		Thresholds: map[string]float64{"A": 0.8, "B": 0.8}, Budget: 56, Tolerance: 0.15,
		Give:         map[string]ShareVariant{"A": {Name: "a", GPUs: 4, PodGPUs: []int{4}}},
		Grow:         map[string]ShareVariant{"B": {Name: "b", GPUs: 8, PodGPUs: []int{8}}},
		Nodes:        map[string]ShareNode{"n1": {Free: 8}, "n2": {Free: 4}, "n3": {Free: 4}},
		DonorUnits:   map[string][]ShareUnit{"A": {four("ns/a-0", "n2"), four("ns/a-1", "n3"), four("ns/a-2", "n2")}},
		PhysicalFree: 16,
	}
	p := PlanShareTransfers(NewShareLedger(), in, time.Unix(1000, 0), simTimings())
	toB := 0
	for _, tr := range p.Started {
		if tr.Receiver == "B" {
			toB++
		}
	}
	if toB == 0 {
		t.Fatalf("B's second replica was deferred to a fill that cannot place it: %+v", p.Started)
	}
}

// What keeps a role's headroom, when a return is due, is counted.
func TestShareReturnWithheldReasons(t *testing.T) {
	withheld := map[string]int{}
	for _, p := range planEach(NewShareLedger(), spareRun(2, 3, 7), 6) {
		for k, v := range p.Withheld {
			withheld[k] += v
		}
	}
	if withheld[ShareWithheldReturnNotCalm] == 0 {
		t.Errorf("Q kept spare above its target while not calm, and nothing said so: %v", withheld)
	}
	withheld = map[string]int{}
	in := spareRun(7, 0.4, 7)
	in.SkipWaits = ShareSkipWaits{Off: true}
	for _, p := range planEach(NewShareLedger(), in, 4) {
		for k, v := range p.Withheld {
			withheld[k] += v
		}
	}
	if withheld[ShareWithheldReturnRoleShort] == 0 {
		t.Errorf("returns held for a short role, and nothing said so: %v", withheld)
	}
}

func TestShareFillPace(t *testing.T) {
	if got := ShareFillPace(6, 2); got != math.MaxInt {
		t.Errorf("below its need: pace %d, want unbounded", got)
	}
	if got := ShareFillPace(2, 3); got != ShareMaxReplicasPerCycle {
		t.Errorf("at or above its need: pace %d, want %d", got, ShareMaxReplicasPerCycle)
	}
}

// The cap scales by weight in the planner: an important model (weight 2)
// keeps two replicas of headroom where a standard one keeps one.
func TestShareHeadroomScalesByWeight(t *testing.T) {
	roles := []ShareRole{
		{Key: "L", Weight: 2, Need: 2, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 2, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}
	// Both have peaked, at 8 and 7: the quota is contended, and the cap binds
	// -- neither's room beside the other's peak (2 and 3) is above its cap.
	l := NewShareLedger()
	l.RecordNeeds(map[string]float64{"L": 7, "Q": 8}, time.Unix(990, 0), spareTimings())
	p := PlanShareTransfers(l, SharePlanInput{Roles: roles, Held: map[string]int{"L": 2, "Q": 2},
		Thresholds: map[string]float64{"L": 0.85, "Q": 0.85}, Budget: 10, Tolerance: 0.15,
		PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1}}, time.Unix(1000, 0), spareTimings())
	integ := map[string]int{}
	for _, v := range p.Evaluation.Roles {
		integ[v.Key] = v.Integer
	}
	if integ["L"] != 4 || integ["Q"] != 3 {
		t.Fatalf("targets %v, want L=4 (need 2 + 2 for weight 2), Q=3 (need 2 + 1)", integ)
	}
}

// curvedQ is spareRun's two roles with Q on a capacity curve: its first 2
// GPUs serve as its best variant does, the next 12 a third as much -- dense
// replicas first, sparse ones at the end, where it gives from. Q holds 14 in
// 2-GPU replicas and needs qNeed. The ledger knows both peaks, L's at 16 and
// Q's at 18, a scale-down window ago: the quota of 20 is contended, and Q's
// spare is capped at one replica above its claim.
func curvedQ(qNeed float64) (SharePlanInput, *ShareLedger) {
	in := spareRun(2, qNeed, 14)
	in.Budget = 20
	in.Held = map[string]int{"L": 4, "Q": 14}
	in.Headroom.ReturnLoad = 0.6
	in.Roles = []ShareRole{
		{Key: "L", Weight: 1, Need: 2, Floor: 1, Ceiling: 16, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: qNeed, Floor: 2, Ceiling: 14, ReplicaGPUs: 2,
			Capacity: []ShareCapacityStep{{GPUs: 2, PerGPU: 1}, {GPUs: 12, PerGPU: 0.312}}},
	}
	in.Give = map[string]ShareVariant{"L": {Name: "l", GPUs: 1}, "Q": {Name: "q", GPUs: 2}}
	l := NewShareLedger()
	knownFor(l, time.Unix(1000, 0), "L", "Q")
	for _, at := range []int64{900, 915} {
		l.RecordNeeds(map[string]float64{"L": 16, "Q": 18}, time.Unix(at, 0), spareTimings())
	}
	return in, l
}

// A return is judged calm on what the GPUs serve, not on their count (#233).
// Q needs 1.639 and keeps 2 GPUs of headroom. Kept at 6, the 4 below its
// headroom serve 2.624: a load of 0.625, above the 0.6 return load, though
// 1.639 / 4 reads 0.41 in GPUs. Kept at 8 they serve 3.248, a load of 0.50.
func TestShareReturnJudgesLoadOnWhatTheGPUsServe(t *testing.T) {
	in, l := curvedQ(1.639)
	for i := range 4 {
		PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
	}
	kept := l.Committed(in.Held)["Q"]
	q := in.Roles[1]
	if load := q.serves(q.Need) / q.serves(float64(kept-2)); kept != 8 || load > 0.6 {
		t.Fatalf("Q kept %d, a load of %.3f on what its GPUs serve; want 8, at most 0.6", kept, load)
	}
	// Control: without the curve, every GPU serves alike and 6 is calm
	// (1.639 <= 0.6 x 4).
	in, l = curvedQ(1.639)
	in.Roles[1].Capacity = nil
	for i := range 4 {
		PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
	}
	if kept := l.Committed(in.Held)["Q"]; kept != 6 {
		t.Fatalf("control, no curve: Q kept %d, want 6", kept)
	}
}

// A return is called off on what the GPUs serve too (#233): Q returned three
// replicas, keeping 8, and its need rose to 3.5. One replica above the bar
// the return went under, 8 GPUs, serve 3.872; 3.5 serves 2.468, above 0.6 of
// it, though 3.5 is below 0.6 x 8 in GPUs.
func TestShareCancelsAReturnOnWhatTheGPUsServe(t *testing.T) {
	cancelled := func(qNeed float64) int {
		in, l := curvedQ(qNeed)
		for range 3 {
			l.Start(ShareTransfer{Donor: "Q", DonorGPUs: 2, Entitled: true, Returned: true}, in.Held, time.Unix(1000, 0), spareTimings())
		}
		return len(PlanShareTransfers(l, in, time.Unix(1015, 0), spareTimings()).Cancelled)
	}
	if n := cancelled(3.5); n != 1 {
		t.Errorf("busy on what its GPUs serve: cancelled %d, want 1", n)
	}
	if n := cancelled(2.9); n != 0 {
		t.Errorf("control, calm (2.9 serves 2.281 <= 0.6 x 3.872): cancelled %d, want 0", n)
	}
}

// A transfer is not called off as a reversal while its receiver is still
// below its whole-replica target, whatever the continuous split says (#233):
// that split shares the spare by need in GPUs, the whole-replica targets and
// admission by what the GPUs serve, and judged on the first a move the second
// just started was cancelled the next cycle, and started again. R's sparse
// GPUs come last, so its need of 8 GPUs serves 5: the continuous split gives
// it 22 of 44, the whole-replica targets 28.
func TestShareKeepsATransferItsReceiverTargetStillWants(t *testing.T) {
	in := SharePlanInput{
		Roles: []ShareRole{
			{Key: "D", Weight: 1, Need: 8, Ceiling: 44, ReplicaGPUs: 1},
			{Key: "R", Weight: 1, Need: 8, Ceiling: 44, ReplicaGPUs: 1,
				Capacity: []ShareCapacityStep{{GPUs: 4, PerGPU: 1}, {GPUs: 40, PerGPU: 0.25}}},
		},
		Held:       map[string]int{"D": 18, "R": 26},
		Thresholds: map[string]float64{"D": 0.8, "R": 0.8}, Budget: 44, Tolerance: 0.15,
		PhysicalFree: math.MaxInt,
	}
	l := NewShareLedger()
	knownFor(l, time.Unix(1000, 0), "D", "R")
	started, cancelled := 0, 0
	for i := range 6 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
		started += len(p.Started)
		cancelled += len(p.Cancelled)
	}
	if started == 0 || cancelled != 0 {
		t.Fatalf("D -> R: started %d, cancelled %d under unchanged demand; want started, none cancelled", started, cancelled)
	}
}

// A role the idle fill raises this cycle returns nothing: A holds 5 of a
// target of 7, and the fill's credit of one 8-GPU replica takes it to 13, past
// its target. Judged on that, it returned a 2-GPU replica of the 5 it holds,
// and the return was called off the next cycle, as A was still below its
// target -- every cycle.
func TestShareFillCreditIsNotSpare(t *testing.T) {
	in := SharePlanInput{
		Roles: []ShareRole{
			{Key: "A", Weight: 1, Need: 5.9, Floor: 1, Ceiling: 20, ReplicaGPUs: 1},
			{Key: "B", Weight: 1, Need: 2, Floor: 1, Ceiling: 20, ReplicaGPUs: 1},
		},
		Held:       map[string]int{"A": 5, "B": 3},
		Thresholds: map[string]float64{"A": 0.8, "B": 0.8}, Budget: 20, Tolerance: 0.15,
		Give:         map[string]ShareVariant{"A": {Name: "a2", GPUs: 2}},
		Grow:         map[string]ShareVariant{"A": {Name: "a8", GPUs: 8}},
		PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1},
	}
	l := NewShareLedger()
	for i := range 5 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
		for _, tr := range p.Started {
			if tr.Returned && tr.Donor == "A" {
				t.Fatalf("cycle %d: A, below its target, returned %+v", i+1, tr)
			}
		}
	}
}

// fragmented is the whole-node e2e's fragmented cluster (#235): G holds one
// 8-GPU replica and needs two; S1 and S2, idle, hold five 1-GPU replicas each,
// spread over nodes n2, n3 and n4 beside GPUs no model holds, so no node can
// be emptied for G. The quota of 18 is spent. nodes says whether the planner
// knows the nodes.
func fragmented(nodes bool) SharePlanInput {
	in := SharePlanInput{
		Roles: []ShareRole{
			{Key: "G", Weight: 1, Need: 16, Floor: 8, Ceiling: 16, ReplicaGPUs: 8},
			{Key: "S1", Weight: 1, Need: 0.2, Floor: 1, Ceiling: 5, ReplicaGPUs: 1},
			{Key: "S2", Weight: 1, Need: 0.2, Floor: 1, Ceiling: 5, ReplicaGPUs: 1},
		},
		Held:       map[string]int{"G": 8, "S1": 5, "S2": 5},
		Thresholds: map[string]float64{"G": 0.8, "S1": 0.8, "S2": 0.8}, Budget: 18, Tolerance: 0.15,
		Give: map[string]ShareVariant{
			"S1": {Name: "s1", GPUs: 1, PodGPUs: []int{1}},
			"S2": {Name: "s2", GPUs: 1, PodGPUs: []int{1}},
		},
		Grow:         map[string]ShareVariant{"G": {Name: "g", GPUs: 8, PodGPUs: []int{8}}},
		PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1},
	}
	if !nodes {
		return in
	}
	in.Nodes = map[string]ShareNode{"n1": {}, "n2": {}, "n3": {}, "n4": {}}
	in.DonorUnits = map[string][]ShareUnit{}
	for _, s := range []string{"S1", "S2"} {
		for i, n := range []string{"n2", "n2", "n3", "n3", "n4"} {
			name := fmt.Sprintf("ns/%s-%d", strings.ToLower(s), i)
			in.DonorUnits[s] = append(in.DonorUnits[s], ShareUnit{Pods: []SharePod{{Name: name, Node: n, GPUs: 1}}})
		}
	}
	return in
}

// While the only role short of its target is one no node can hold a replica
// of, nothing is returned for it (#235): the GPUs would sit unassigned, and
// the small models would lose their headroom for nothing. Without node
// information the idle fill places blind and a return may still serve G, so
// returns go on as before. Re-judged every cycle: once a node can be emptied
// G is funded, and the spare above the targets is returned again.
func TestShareReturnsNothingForAnUnplaceableReceiver(t *testing.T) {
	l := NewShareLedger()
	in := fragmented(true)
	returns, withheld := 0, 0
	for i := range 6 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
		returns += p.Returns
		withheld += p.Withheld[ShareWithheldReturnUnplaceable]
		if i >= 2 && p.Unfunded["G"] != ShareUnfundedNoCompatibleDonor {
			t.Fatalf("cycle %d: G unfunded %q, want %q", i+1, p.Unfunded["G"], ShareUnfundedNoCompatibleDonor)
		}
	}
	if returns != 0 || withheld == 0 {
		t.Fatalf("fragmented: %d returned, %d withheld for G; want none returned, withheld counted", returns, withheld)
	}

	returns = 0
	for _, p := range planEach(NewShareLedger(), fragmented(false), 6) {
		returns += p.Returns
	}
	if returns == 0 {
		t.Fatal("control, no node information: nothing returned")
	}

	// Judged every cycle, on the same ledger: two pods outside the models
	// leave n2, and its four free GPUs and the four small pods on it make a
	// hole for G. The spare the small models kept funds it: a set takes them
	// to their targets of one.
	in.Nodes["n2"] = ShareNode{Free: 4}
	funded := false
	for i := range 4 {
		for _, tr := range PlanShareTransfers(l, in, time.Unix(int64(1090+15*i), 0), spareTimings()).Started {
			funded = funded || tr.Receiver == "G"
		}
	}
	if c := l.Committed(in.Held); !funded || c["G"] != 16 || c["S1"] != 1 || c["S2"] != 1 {
		t.Fatalf("a node freed: G funded %v, committed %v; want G at 16 from the small models' spare", funded, c)
	}
}
