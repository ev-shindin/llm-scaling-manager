package allocation

import (
	"testing"
	"time"
)

// A donor held because it has given all it can is not a reversal hold: the
// withheld count would send an operator after oscillation that is not there.
func TestShareBusyDonorIsNotAReversalHold(t *testing.T) {
	roles := []ShareRole{
		// Not calm after giving (5.5 of 7 is above ShareRebalanceDonorLoad): no
		// hard imbalance, so the reversal hold applies and the control below
		// counts.
		{Key: "D", Weight: 1, Need: 5.5, Ceiling: 64, ReplicaGPUs: 1},
		{Key: "R", Weight: 1, Need: 2.5, Ceiling: 64, ReplicaGPUs: 1},
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"D": 8, "R": 2},
		Thresholds: map[string]float64{"D": 0.8, "R": 0.8}, Budget: 10, Tolerance: 0.15}
	tm := simTimings()
	now := time.Unix(1000, 0)

	// Withheld counts accumulate over the cycles: the planner funds only
	// once the receiver has been actionable for the confirm cycles.
	withheld := func(l *ShareLedger) (reversal, started int) {
		for i := range 2 * ShareConfirmCycles {
			plan := PlanShareTransfers(l, in, now.Add(time.Duration(30*i)*time.Second), tm)
			reversal += plan.Withheld[ShareWithheldReversalHold]
			started += len(plan.Started)
		}
		return reversal, started
	}
	l := NewShareLedger()
	// A third role takes D's live transfer: D is exhausted, R stays short.
	l.Start(ShareTransfer{Donor: "D", Receiver: "X", GPUs: 1, DonorGPUs: 1}, map[string]int{"D": 8, "X": 0}, now, tm)
	l.HoldGiving("D", now, tm)
	if reversal, started := withheld(l); started != 0 || reversal != 0 {
		t.Fatalf("a busy donor: %d started, %d reversal holds counted; want none of either", started, reversal)
	}

	// The control: a donor that just received is a reversal hold, counted.
	l = NewShareLedger()
	l.recordMove("D", true, "x", now, tm)
	if reversal, _ := withheld(l); reversal == 0 {
		t.Fatal("control: a donor inside its reversal hold was not counted")
	}
}

// The hold ends when one of the donor's releases lands, not only at the
// release timeout.
func TestShareBusyEndsWhenAReleaseLands(t *testing.T) {
	tm := simTimings()
	now := time.Unix(1000, 0)
	l := NewShareLedger()
	held := map[string]int{"D": 8, "R": 2}
	l.Start(ShareTransfer{Donor: "D", Receiver: "R", GPUs: 1, DonorGPUs: 1}, held, now, tm)
	l.HoldGiving("D", now, tm)
	if !l.GivingBusy("D", now.Add(time.Minute)) {
		t.Fatal("setup: the donor is not busy")
	}
	l.Observe(map[string]int{"D": 7, "R": 2}, now.Add(time.Minute), tm)
	if l.GivingBusy("D", now.Add(time.Minute)) {
		t.Fatal("the donor is still busy after its release landed")
	}
}
