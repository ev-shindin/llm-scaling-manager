package allocation

import (
	"math"
	"slices"
	"testing"
	"time"
)

// swingingL makes L a swinging role whose mean, over many quiet readings, is
// far below a burst it reads now: the case where judging it on the mean and
// judging it on the reading disagree.
func swingingL(l *ShareLedger, quiet float64) {
	for i := 20; i > 0; i-- {
		l.RecordNeeds(map[string]float64{"L": quiet}, time.Unix(int64(1000-15*i), 0), spareTimings())
	}
	l.swingUntil["L"] = time.Unix(1_000_000, 0)
}

// knownFor gives roles a whole swing window of need history before now, so
// their recorded peaks are taken as known (ShareLedger.PeakKnown).
func knownFor(l *ShareLedger, now time.Time, roles ...string) {
	needs := map[string]float64{}
	for _, r := range roles {
		needs[r] = 0
	}
	l.RecordNeeds(needs, now.Add(-spareTimings().SwingWindow), spareTimings())
}

// Until a role has a swing window of history, its peak is not known and is
// taken at its ceiling: after a restart, the cap is on until the history
// shows the peaks fit. Judged on the history it had, the cap stayed off until
// a model burst inside the window -- and the first burst found the spare
// parked on another model.
func TestShareCapsUntilThePeaksAreKnown(t *testing.T) {
	in := spareRun(2, 0.4, 7)
	l := NewShareLedger()
	t0 := time.Unix(1000, 0)
	if p := PlanShareTransfers(l, in, t0, spareTimings()); !p.HeadroomCapped || p.PeakClaimGPUs != 16 {
		t.Fatalf("fresh ledger: capped %v on peak claims %d, want capped on 16 (both ceilings)", p.HeadroomCapped, p.PeakClaimGPUs)
	}
	// Planned every half window, so the history never ages out.
	half := spareTimings().SwingWindow / 2
	PlanShareTransfers(l, in, t0.Add(half), spareTimings())
	later := t0.Add(2*half + time.Second)
	if p := PlanShareTransfers(l, in, later, spareTimings()); p.HeadroomCapped {
		t.Fatalf("a window later: capped on peak claims %d, though the peaks (2 + 1) fit", p.PeakClaimGPUs)
	}
}

// A model is not capped below what the others' peaks leave of the quota: Q
// peaked at 7 and is calm on 6, L peaked at 3.5 and holds 4 of 10. No peak of
// L's can use what Q would return, so Q keeps it warm for its own next burst.
func TestShareCapLeavesWhatNoOtherPeakUses(t *testing.T) {
	in := spareRun(3.5, 1.5, 6)
	in.Held["L"] = 4
	l := NewShareLedger()
	knownFor(l, time.Unix(990, 0), "L", "Q")
	for _, at := range []int64{960, 975} { // two readings, so the peak counts
		l.RecordNeeds(map[string]float64{"L": 3.5, "Q": 7}, time.Unix(at, 0), spareTimings())
	}
	returned := 0
	for i := range 5 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), spareTimings())
		if !p.HeadroomCapped {
			t.Fatalf("cycle %d: setup: the peaks (7 + 4 of 10) contend, and the cap is off", i)
		}
		returned += p.Returns
	}
	if returned != 0 {
		t.Fatalf("Q returned %d replicas no other model's peak could use", returned)
	}
}

// A swinging role at the crest of a burst is capped on its reading, not on
// its mean: capped on the mean, it is held below its demand beside GPUs only
// it would use.
func TestShareCapsASwingingRoleOnItsReading(t *testing.T) {
	in := spareRun(8, 2, 7)
	l := NewShareLedger()
	swingingL(l, 1)
	l.RecordNeeds(map[string]float64{"Q": 8}, time.Unix(990, 0), spareTimings())
	p := PlanShareTransfers(l, in, time.Unix(1000, 0), spareTimings())
	if !p.HeadroomCapped {
		t.Fatal("setup: the peaks (8 + 8 of 10) contend, and the cap is off")
	}
	for _, v := range p.Evaluation.Roles {
		if v.Key == "L" && v.Integer < 7 {
			t.Fatalf("L, reading 8, has target %d: capped on its mean", v.Integer)
		}
	}
}

// A huge weight does not overflow the headroom it buys.
func TestShareHeadroomReplicasIsBounded(t *testing.T) {
	if got := ShareHeadroomReplicas(1e300, 10); got != MaxShareBudgetGPUs {
		t.Fatalf("headroom %d, want %d", got, MaxShareBudgetGPUs)
	}
}

// A peak counts only once two readings in a row reach it: one reading -- a
// scrape glitch, or a tenant's forged metric -- would otherwise cap every
// other model's headroom for a whole window.
func TestSharePeakNeedsTwoReadingsInARow(t *testing.T) {
	l := NewShareLedger()
	tm := spareTimings()
	at := func(s int64) time.Time { return time.Unix(1000+s, 0) }
	for i, n := range []float64{1, 8, 1} {
		l.RecordNeeds(map[string]float64{"L": n}, at(int64(15*i)), tm)
	}
	if got := l.PeakNeed("L", 1); got != 1 {
		t.Fatalf("peak %v after one reading of 8, want 1", got)
	}
	for i := range 2 {
		l.RecordNeeds(map[string]float64{"L": 8}, at(int64(45+15*i)), tm)
	}
	if got := l.PeakNeed("L", 1); got != 8 {
		t.Fatalf("peak %v after two readings of 8 in a row, want 8", got)
	}
}

// A role whose history has all aged out starts it again: a model back after a
// gap longer than the swing window has one reading, not a window of them, and
// its peak is not known. Recorded without such a gap, it is known once a
// window has passed.
func TestSharePeakHistoryRestartsAfterAGap(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	l := NewShareLedger()
	l.RecordNeeds(map[string]float64{"L": 1}, t0, tm)
	back := t0.Add(tm.SwingWindow + time.Second)
	l.RecordNeeds(map[string]float64{"L": 1}, back, tm)
	if l.PeakKnown("L", back, tm) {
		t.Fatal("back after a gap longer than the window, L's peak is known from one reading")
	}
	l = NewShareLedger()
	half := tm.SwingWindow / 2
	for i := range 3 {
		l.RecordNeeds(map[string]float64{"L": 1}, t0.Add(time.Duration(i)*half), tm)
	}
	if now := t0.Add(2 * half); !l.PeakKnown("L", now, tm) {
		t.Fatal("control: recorded every half window, L's peak is not known a window later")
	}
}

// A role joining a group whose history is known claims what it holds, not its
// ceiling: at its ceiling, any new model -- or one renamed to look new --
// would cap every other model's headroom for a window. N holds 3 on a need of
// 0.5, so its claim is the 3, not the 1 its need rounds to. Control: in a
// group with no history at all, every role counts at its ceiling and is named.
func TestShareJoinerClaimsWhatItHolds(t *testing.T) {
	tm := spareTimings()
	now := time.Unix(10000, 0)
	roles := []ShareRole{
		{Key: "L", Weight: 1, Need: 2, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "Q", Weight: 1, Need: 1, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "N", Weight: 1, Need: 0.5, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"L": 3, "Q": 3, "N": 3},
		Thresholds: map[string]float64{"L": 0.85, "Q": 0.85, "N": 0.85}, Budget: 10, Tolerance: 0.15,
		Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	l := NewShareLedger()
	knownFor(l, now, "L", "Q")
	for _, ago := range []time.Duration{30 * time.Second, 15 * time.Second} {
		l.RecordNeeds(map[string]float64{"L": 2, "Q": 1}, now.Add(-ago), tm)
	}
	p := PlanShareTransfers(l, in, now, tm)
	// L at its peak of 2, Q at 1, N at the 3 it holds.
	if p.HeadroomCapped || p.PeakClaimGPUs != 6 || len(p.UnknownPeaks) != 0 {
		t.Fatalf("N joining a known group: capped %v on peak claims %d, unknown %v; want uncapped on 6, none unknown",
			p.HeadroomCapped, p.PeakClaimGPUs, p.UnknownPeaks)
	}
	p = PlanShareTransfers(NewShareLedger(), in, now, tm)
	if !p.HeadroomCapped || p.PeakClaimGPUs != 24 || !slices.Equal(p.UnknownPeaks, []string{"L", "Q", "N"}) {
		t.Fatalf("control, no history: capped %v on peak claims %d, unknown %v; want capped on 24, all three unknown",
			p.HeadroomCapped, p.PeakClaimGPUs, p.UnknownPeaks)
	}
}

// donorPeakRun is L at its need of 3 on 3 GPUs and Q, calm on 7, in a quota
// of 10 whose peaks are known and fit: the spare is spread, L's target is 6,
// and Q, at 4, is a donor. qPeak is Q's peak over the swing window, read
// twice in a row before its scale-down window.
func donorPeakRun(qPeak float64) (moved int, withheld int) {
	tm := spareTimings()
	in := spareRun(3, 1, 7)
	l := NewShareLedger()
	knownFor(l, time.Unix(1000, 0), "L", "Q")
	for _, at := range []int64{900, 915} {
		l.RecordNeeds(map[string]float64{"Q": qPeak}, time.Unix(at, 0), tm)
	}
	for i := range 3 {
		p := PlanShareTransfers(l, in, time.Unix(int64(1000+15*i), 0), tm)
		if p.HeadroomCapped {
			return -1, -1
		}
		moved += qToL(p.Started)
		withheld += p.Withheld[ShareWithheldDonorPeak]
	}
	return moved, withheld
}

// Spread, a receiver already at its need takes no headroom that would leave
// its donor below the donor's own recent peak: the split covers both, and the
// move -- from a donor that may drain for minutes -- lands after the
// receiver's burst and just before the donor's own. Control: with Q's peak
// low, the same move is made.
func TestShareSpreadMoveKeepsTheDonorsPeak(t *testing.T) {
	moved, withheld := donorPeakRun(6.5)
	if moved < 0 {
		t.Fatal("setup: the peaks (7 + 1 of 10) fit, and the cap is on")
	}
	if moved != 0 || withheld == 0 {
		t.Fatalf("Q, peaking at 7, gave L %d replicas below its peak (withheld donor-peak %d)", moved, withheld)
	}
	if moved, _ := donorPeakRun(2); moved == 0 {
		t.Fatal("control: Q, peaking at 2, gave L nothing")
	}
}

// What the others' peaks leave a role is whole replicas of it, never above its
// own ceiling and never negative.
func TestShareRoomForOthers(t *testing.T) {
	for _, c := range []struct {
		budget, others, g, ceiling, want int
	}{
		{10, 3, 1, 0, 7},
		{10, 3, 4, 0, 4},   // 7 GPUs left hold one 4-GPU replica
		{16, 3, 4, 8, 8},   // 12 left, but its ceiling is 8
		{10, 3, 4, 16, 4},  // a ceiling above the room does not raise it
		{10, 15, 4, 0, 0},  // the others' peaks overrun the budget
		{10, 10, 1, 8, 0},  // nothing left
		{64, 0, 8, 64, 64}, // alone, up to its ceiling
	} {
		if got := shareRoomForOthers(c.budget, c.others, c.g, c.ceiling); got != c.want {
			t.Errorf("shareRoomForOthers(%d, %d, %d, %d) = %d, want %d", c.budget, c.others, c.g, c.ceiling, got, c.want)
		}
	}
}

// The cap leaves an unbounded role unbounded where ShareCapSpare does: A's
// need -- clamped to the largest budget -- is beyond any limit, so the cap
// gives it none, and the room beside B's peak must not become one. Bounded at
// that room, A would be held at 2 GPUs of a quota it is the only one to need.
func TestShareCapKeepsAnUnboundedRoleUnbounded(t *testing.T) {
	roles := []ShareRole{
		{Key: "A", Weight: 1, Need: MaxShareBudgetGPUs, Floor: 1, ReplicaGPUs: 1},
		{Key: "B", Weight: 1, Need: 0.5, Floor: 1, Ceiling: 8, ReplicaGPUs: 1},
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"A": 5, "B": 5},
		Thresholds: map[string]float64{"A": 0.85, "B": 0.85}, Budget: 10, Tolerance: 0.15,
		Headroom: ShareHeadroom{Capped: true, Replicas: 1}}
	p := PlanShareTransfers(NewShareLedger(), in, time.Unix(1000, 0), spareTimings())
	if !p.HeadroomCapped {
		t.Fatal("setup: the peaks (10 + 8 of 10) contend, and the cap is off")
	}
	for _, v := range p.Evaluation.Roles {
		if v.Key == "A" && v.Integer < 8 {
			t.Fatalf("A, unbounded, has target %d of 10: capped at the room beside B's peak", v.Integer)
		}
	}
}

// A role gone past the horizon leaves no need history behind -- its first
// reading included: kept, it would call the group's history known, and a new
// group's roles would claim what they hold instead of their ceilings.
func TestShareRetainForgetsWhenANeedWasFirstSeen(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	l := NewShareLedger()
	l.RecordNeeds(map[string]float64{"X": 1}, t0, tm)
	l.Retain([]string{"X"}, t0, tm)
	gone := t0.Add(max(tm.ReversalHold, 2*tm.SwingWindow, tm.ReleaseTimeout<<4) + time.Second)
	l.Retain([]string{"L", "Q"}, gone, tm)
	if l.AnyPeakKnown(gone, tm) {
		t.Fatal("X, gone past the horizon, still makes the group's peaks known")
	}
	p := PlanShareTransfers(l, spareRun(2, 0.4, 7), gone, tm)
	if !slices.Equal(p.UnknownPeaks, []string{"L", "Q"}) || p.PeakClaimGPUs != 16 {
		t.Fatalf("a new group after X left: unknown %v on peak claims %d, want L and Q at their ceilings (16)",
			p.UnknownPeaks, p.PeakClaimGPUs)
	}
	l.RecordNeeds(map[string]float64{"X": 1}, gone, tm)
	if l.PeakKnown("X", gone, tm) {
		t.Fatal("X, back, has a known peak from one reading")
	}
}

// A role's need history is kept in what its GPUs serve, and read back on the
// curve it has now (#233): the same demand reads 12 GPUs on sparse replicas
// and 8 once a dense replica serves part of it. Kept in GPUs, its peak would
// still read 12 after the change, on GPUs it no longer has.
func TestSharePeakIsReadOnTodaysCurve(t *testing.T) {
	sparse := []ShareCapacityStep{{GPUs: 100, PerGPU: 0.5}}
	dense := []ShareCapacityStep{{GPUs: 4, PerGPU: 1}, {GPUs: 100, PerGPU: 0.5}}
	in := SharePlanInput{
		Roles: []ShareRole{
			{Key: "A", Weight: 1, Need: 12, Ceiling: 40, ReplicaGPUs: 1, Capacity: sparse},
			{Key: "B", Weight: 1, Need: 2, Ceiling: 8, ReplicaGPUs: 1},
		},
		Held:       map[string]int{"A": 12, "B": 2},
		Thresholds: map[string]float64{"A": 0.8, "B": 0.8}, Budget: 20, Tolerance: 0.15,
		PhysicalFree: math.MaxInt, Headroom: ShareHeadroom{Capped: true, Replicas: 1},
	}
	l := NewShareLedger()
	t0 := time.Unix(1000, 0)
	knownFor(l, t0, "A", "B")
	PlanShareTransfers(l, in, t0, spareTimings())
	PlanShareTransfers(l, in, t0.Add(15*time.Second), spareTimings())
	in.Roles[0].Need, in.Roles[0].Capacity = 8, dense
	p := PlanShareTransfers(l, in, t0.Add(30*time.Second), spareTimings())
	if p.PeakClaimGPUs != 10 {
		t.Fatalf("peak claims %d, want 10: A's peak of 6 served is 8 GPUs on its curve now, B's is 2", p.PeakClaimGPUs)
	}
}
