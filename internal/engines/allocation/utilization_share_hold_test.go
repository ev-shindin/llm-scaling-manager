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

	l.HoldGiving("ns/a/both", now, tm)
	if !l.GivingHeld("ns/a/both", now.Add(time.Minute), tm) {
		t.Fatal("a held donor was offered again within the release timeout")
	}
	if l.BackingOff("ns/a/both", now) || l.Unsteerable("ns/a/both", now) {
		t.Fatal("holding a donor reported it as backing off or unsteerable")
	}
	if l.GivingHeld("ns/a/both", now.Add(tm.ReleaseTimeout+time.Second), tm) {
		t.Fatal("the hold outlived the release timeout")
	}
	// The first real failure afterwards backs off one release timeout: the
	// hold counted no abort.
	l.MarkFailed("ns/a/both", now, tm)
	if got := l.GiveAfter("ns/a/both"); !got.Equal(now.Add(tm.ReleaseTimeout)) {
		t.Fatalf("back-off after a hold ends %v, want one release timeout (%v)", got, now.Add(tm.ReleaseTimeout))
	}
}
