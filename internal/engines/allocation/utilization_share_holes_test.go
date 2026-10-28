package allocation

import (
	"maps"
	"testing"
	"time"
)

// checkHoles fails unless h places want per node and unplaced elsewhere, and
// its total is what the ledger promises and holds.
func checkHoles(t *testing.T, l *ShareLedger, now time.Time, want map[string]int, unplaced int) {
	t.Helper()
	h := l.PromisedHoles(now)
	if !maps.Equal(h.Nodes, want) || h.Unplaced != unplaced {
		t.Fatalf("holes %v unplaced %d, want %v unplaced %d", h.Nodes, h.Unplaced, want, unplaced)
	}
	if total := l.Promised() + l.WakeHeld(now); h.Total() != total {
		t.Fatalf("holes total %d, want Promised+WakeHeld %d", h.Total(), total)
	}
}

// A filling transfer's promise sits on the node its donor pod ran on; one
// whose node was never recorded -- a transfer restored without its pods'
// nodes -- is unplaced, and counts against no node in particular.
func TestSharePromisedHolesFilling(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	held := map[string]int{"E": 16, "R": 8, "A": 6, "M": 2}
	l := NewShareLedger()
	r := l.Start(ShareTransfer{Donor: "E", Receiver: "R", GPUs: 8, DonorGPUs: 8}, held, t0, tm)
	m := l.Start(ShareTransfer{Donor: "A", Receiver: "M", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
	l.RecordHoles(r.ID, map[string]int{"n1": 8})
	l.RecordHoles(m.ID, map[string]int{"n2": 1})
	checkHoles(t, l, t0, nil, 0) // nothing released yet: nothing promised
	l.Observe(map[string]int{"E": 8, "R": 8, "A": 5, "M": 2}, t0.Add(time.Minute), tm)
	checkHoles(t, l, t0.Add(time.Minute), map[string]int{"n1": 8, "n2": 1}, 0)

	// The same two, the second with no node known.
	l = NewShareLedger()
	l.Restore(ShareTransfer{ID: "r", Donor: "E", Receiver: "R", GPUs: 8, DonorGPUs: 8, Started: t0,
		Deadline: t0.Add(time.Hour), HoleNodes: map[string]int{"n1": 8}}, 16)
	l.Restore(ShareTransfer{ID: "m", Donor: "A", Receiver: "M", GPUs: 1, DonorGPUs: 1, Started: t0,
		Deadline: t0.Add(time.Hour)}, 6)
	l.Observe(map[string]int{"E": 8, "R": 8, "A": 5, "M": 2}, t0.Add(time.Minute), tm)
	checkHoles(t, l, t0.Add(time.Minute), map[string]int{"n1": 8}, 1)
}

// A donor pod larger than the receiver's replica promises only the replica,
// on its node; the surplus goes back to the idle budget, not to the holes.
func TestSharePromisedHolesSurplus(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	l := NewShareLedger()
	q := l.Start(ShareTransfer{Donor: "Q", Receiver: "R", GPUs: 4, DonorGPUs: 8}, map[string]int{"Q": 8, "R": 4}, t0, tm)
	l.RecordHoles(q.ID, map[string]int{"n3": 8})
	l.Observe(map[string]int{"Q": 0, "R": 4}, t0.Add(time.Minute), tm)
	checkHoles(t, l, t0.Add(time.Minute), map[string]int{"n3": 4}, 0)
}

// A promise smaller than its holes, which span several nodes -- a
// LeaderWorkerSet donor's group on two nodes funding a smaller receiver
// replica -- sits on the holes in node-name order, the same every time.
func TestSharePromisedHolesSmallerThanItsHoles(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	for range 50 {
		l := NewShareLedger()
		q := l.Start(ShareTransfer{Donor: "Q", Receiver: "R", GPUs: 6, DonorGPUs: 12}, map[string]int{"Q": 12, "R": 4}, t0, tm)
		l.RecordHoles(q.ID, map[string]int{"nc": 4, "na": 4, "nb": 4})
		l.Observe(map[string]int{"Q": 0, "R": 4}, t0.Add(time.Minute), tm)
		checkHoles(t, l, t0.Add(time.Minute), map[string]int{"na": 4, "nb": 2}, 0)
	}
}

// A donor set whose members release in the same cycle: the primary, first in
// start order, is filling before its contributor is seen to release, and the
// contributor's hole still joins the receiver's promise.
func TestSharePromisedHolesDonorSetReleasedTogether(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	held := map[string]int{"A": 8, "B": 8, "R": 8}
	l := NewShareLedger()
	p := l.Start(ShareTransfer{Donor: "A", Receiver: "R", GPUs: 8, DonorGPUs: 4, SetID: "pending"}, held, t0, tm)
	c := l.Start(ShareTransfer{Donor: "B", DonorGPUs: 4, SetID: "pending"}, held, t0, tm)
	l.LinkSet(p.ID, c.ID)
	l.RecordHoles(p.ID, map[string]int{"na": 4})
	l.RecordHoles(c.ID, map[string]int{"nb": 4})
	l.Observe(map[string]int{"A": 4, "B": 4, "R": 8}, t0.Add(time.Minute), tm)
	if l.Promised() != 8 {
		t.Fatalf("setup: %d promised, want the replica's 8: %+v", l.Promised(), l.Transfers())
	}
	checkHoles(t, l, t0.Add(time.Minute), map[string]int{"na": 4, "nb": 4}, 0)
}

// A donor set restored after a restart: its members come back from their
// marked pods, holes included, and none has released yet -- a set with a
// member whose pod is already gone gives less than its receiver takes, and
// the restore drops it whole. The contributor's hole is then collected as it
// releases, as it is without a restart.
func TestSharePromisedHolesRestoredDonorSet(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	l := NewShareLedger()
	l.Restore(ShareTransfer{ID: "p", Donor: "A", Receiver: "R", GPUs: 8, DonorGPUs: 4, SetID: "p", Started: t0,
		Deadline: t0.Add(time.Hour), HoleNodes: map[string]int{"na": 4}}, 8)
	l.Restore(ShareTransfer{ID: "c", Donor: "B", DonorGPUs: 4, SetID: "p", Started: t0,
		Deadline: t0.Add(time.Hour), HoleNodes: map[string]int{"nb": 4}}, 8)
	l.LinkSet("p", "c")
	checkHoles(t, l, t0, nil, 0)
	l.Observe(map[string]int{"A": 8, "B": 4, "R": 8}, t0.Add(time.Minute), tm)
	checkHoles(t, l, t0.Add(time.Minute), map[string]int{"nb": 4}, 0)
	l.Observe(map[string]int{"A": 4, "B": 4, "R": 8}, t0.Add(2*time.Minute), tm)
	checkHoles(t, l, t0.Add(2*time.Minute), map[string]int{"na": 4, "nb": 4}, 0)
}

// A donor set's promise sits on its members' nodes: while the primary still
// releases, what its released contributor opened; once it has released too,
// the receiver's whole replica across both.
func TestSharePromisedHolesDonorSet(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	held := map[string]int{"A": 8, "B": 8, "R": 8}
	l := NewShareLedger()
	p := l.Start(ShareTransfer{Donor: "A", Receiver: "R", GPUs: 8, DonorGPUs: 4, SetID: "pending"}, held, t0, tm)
	c := l.Start(ShareTransfer{Donor: "B", DonorGPUs: 4, SetID: "pending"}, held, t0, tm)
	l.LinkSet(p.ID, c.ID)
	l.RecordHoles(p.ID, map[string]int{"na": 4})
	l.RecordHoles(c.ID, map[string]int{"nb": 4})
	l.Observe(map[string]int{"A": 8, "B": 4, "R": 8}, t0.Add(time.Minute), tm)
	if l.Promised() != 4 {
		t.Fatalf("setup: %d promised, want the contributor's 4: %+v", l.Promised(), l.Transfers())
	}
	checkHoles(t, l, t0.Add(time.Minute), map[string]int{"nb": 4}, 0)
	l.Observe(map[string]int{"A": 4, "B": 4, "R": 8}, t0.Add(2*time.Minute), tm)
	if l.Promised() != 8 {
		t.Fatalf("setup: %d promised, want the replica's 8: %+v", l.Promised(), l.Transfers())
	}
	checkHoles(t, l, t0.Add(2*time.Minute), map[string]int{"na": 4, "nb": 4}, 0)
}

// GPUs held for a wake sit on the redirected transfer's node, whether its
// holes were recorded before the redirect or after; a hold with no node
// known is unplaced.
func TestSharePromisedHolesWake(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(1000, 0)
	held := map[string]int{"Q": 8, "R": 8}
	l := NewShareLedger()
	a := l.Start(ShareTransfer{Donor: "Q", Receiver: "R", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
	b := l.Start(ShareTransfer{Donor: "Q", Receiver: "R", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
	c := l.Start(ShareTransfer{Donor: "Q", Receiver: "R", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
	l.RecordHoles(a.ID, map[string]int{"n5": 1})
	for _, id := range []string{a.ID, b.ID, c.ID} {
		if _, ok := l.Redirect(id, time.Minute); !ok {
			t.Fatalf("setup: %s not redirected", id)
		}
	}
	l.RecordHoles(b.ID, map[string]int{"n6": 1})
	checkHoles(t, l, t0, map[string]int{"n5": 1, "n6": 1}, 1)
}
