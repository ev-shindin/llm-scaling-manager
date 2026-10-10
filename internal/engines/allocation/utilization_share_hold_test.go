package allocation

import (
	"testing"
	"time"
)

// A donor that has given all it can is held, not punished: its receiver tries
// another donor, but no abort is counted, it is not backing off and not
// unsteerable -- no blocked reason reads the hold.
func TestShareLedgerHoldGivingIsNotABackOff(t *testing.T) {
	l := NewShareLedger()
	now := time.Unix(1000, 0)
	tm := ShareTimings{ReleaseTimeout: 10 * time.Minute, ReversalHold: time.Minute}

	// Exhausted means every pod is given to a live transfer: there is one.
	l.Start(ShareTransfer{Donor: "ns/a/both", Receiver: "ns/b/both", GPUs: 1, DonorGPUs: 1},
		map[string]int{"ns/a/both": 4, "ns/b/both": 1}, now, tm)
	l.HoldGiving("ns/a/both", now, tm)
	if !l.GivingHeld("ns/a/both", now.Add(time.Minute), tm) {
		t.Fatal("a held donor was offered again within the release timeout")
	}
	if l.BackingOff("ns/a/both", now) || l.Unsteerable("ns/a/both", now) {
		t.Fatal("holding a donor reported it as backing off or unsteerable")
	}
	if l.GivingBusy("ns/a/both", now.Add(tm.ReleaseTimeout+time.Second)) {
		t.Fatal("the hold outlived the release timeout")
	}
	// The first real failure afterwards backs off one release timeout: the
	// hold counted no abort.
	l.MarkFailed("ns/a/both", now, tm)
	if got := l.GiveAfter("ns/a/both"); !got.Equal(now.Add(tm.ReleaseTimeout)) {
		t.Fatalf("back-off after a hold ends %v, want one release timeout (%v)", got, now.Add(tm.ReleaseTimeout))
	}
}

// The exhausted hold lasts only while a transfer of the donor is live: one
// called off may have left it a pod to give.
func TestShareLedgerHoldGivingEndsWithItsTransfers(t *testing.T) {
	l := NewShareLedger()
	now := time.Unix(1000, 0)
	tm := ShareTimings{ReleaseTimeout: 10 * time.Minute, ReversalHold: time.Minute}
	tr := l.Start(ShareTransfer{Donor: "a", Receiver: "b", GPUs: 1, DonorGPUs: 1}, map[string]int{"a": 4, "b": 1}, now, tm)
	l.HoldGiving("a", now, tm)
	if !l.GivingBusy("a", now.Add(time.Minute)) {
		t.Fatal("setup: not busy")
	}
	l.Forget(tr.ID, tm)
	if l.GivingBusy("a", now.Add(time.Minute)) {
		t.Fatal("still busy with no transfer of its live")
	}
}

// A donor whose workload is changing -- a rollout, a pod still starting -- is
// held one release timeout with no transfer of its own, and it is routine: no
// abort, no back-off, not unsteerable, so a rollout cannot ratchet the back-off
// the next real abort starts from.
func TestShareLedgerHoldChangingIsRoutine(t *testing.T) {
	l := NewShareLedger()
	now := time.Unix(1000, 0)
	tm := ShareTimings{ReleaseTimeout: 10 * time.Minute, ReversalHold: time.Minute}
	for range 5 {
		l.HoldChanging("a", now, tm)
	}
	if !l.GivingBusy("a", now.Add(time.Minute)) || !l.GivingHeld("a", now.Add(time.Minute), tm) {
		t.Fatal("a changing donor was offered again within the release timeout")
	}
	if l.BackingOff("a", now) || l.Unsteerable("a", now) {
		t.Fatal("a changing donor reported as backing off or unsteerable")
	}
	if l.GivingBusy("a", now.Add(tm.ReleaseTimeout+time.Second)) {
		t.Fatal("the hold outlived the release timeout")
	}
	l.MarkFailed("a", now, tm)
	if got := l.GiveAfter("a"); !got.Equal(now.Add(tm.ReleaseTimeout)) {
		t.Fatalf("back-off after five holds ends %v, want one release timeout: the holds counted aborts", got)
	}
}
