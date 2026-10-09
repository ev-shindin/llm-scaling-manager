package allocation

import (
	"slices"
	"testing"
	"time"
)

// planCycles runs the planner long enough for its confirm cycles, and returns
// every transfer it started.
func planCycles(in SharePlanInput, l *ShareLedger) []ShareTransfer {
	now := time.Unix(1000, 0)
	started := make([]ShareTransfer, 0, 4)
	for i := range 2*ShareConfirmCycles + 2 {
		plan := PlanShareTransfers(l, in, now.Add(time.Duration(30*i)*time.Second), simTimings())
		started = append(started, plan.Started...)
	}
	return started
}

// A donor may go down to its own whole-replica target even when that target
// lies outside its continuous band: a band wider than a replica's step put it
// there, and refusing starved a short receiver for good while whole replicas
// covered every need.
func TestShareDonorGoesToItsWholeReplicaTarget(t *testing.T) {
	for _, c := range []struct {
		name  string
		roles []ShareRole
		held  map[string]int
		budg  int
	}{
		{"2-GPU receiver, 8-GPU donor",
			[]ShareRole{{Key: "r0", Weight: 1, Need: 6.5, Floor: 2, Ceiling: 64, ReplicaGPUs: 2},
				{Key: "r1", Weight: 1, Need: 15, Ceiling: 64, ReplicaGPUs: 8}},
			map[string]int{"r0": 4, "r1": 24}, 29},
		{"whole-node replicas on both sides",
			[]ShareRole{{Key: "r0", Weight: 1, Need: 8.5, Ceiling: 64, ReplicaGPUs: 8},
				{Key: "r1", Weight: 1, Need: 15.5, Ceiling: 64, ReplicaGPUs: 8}},
			map[string]int{"r0": 8, "r1": 24}, 33},
	} {
		t.Run(c.name, func(t *testing.T) {
			th := map[string]float64{"r0": 0.8, "r1": 0.8}
			in := SharePlanInput{Roles: c.roles, Held: c.held, Thresholds: th, Budget: c.budg, Tolerance: 0.15}
			if !WholeReplicaCoverage(c.roles, c.budg) {
				t.Fatal("setup: whole replicas do not cover the claims")
			}
			started := planCycles(in, NewShareLedger())
			if !slices.ContainsFunc(started, func(s ShareTransfer) bool { return s.Donor == "r1" && s.Receiver == "r0" }) {
				t.Fatalf("r0 stayed short with r1 above its whole-replica target: started %v", started)
			}
		})
	}
}

// The keep is the claim, capped at the ceiling: a role needing 20 under a
// ceiling of 8 is held to 8, not 20, so it can repay a reserve debt from
// above its ceiling.
func TestShareKeepIsCappedAtTheCeiling(t *testing.T) {
	roles := []ShareRole{
		{Key: "D", Weight: 1, Need: 20, Ceiling: 8, ReplicaGPUs: 1},
		{Key: "R", Weight: 1, Need: 1, Ceiling: 64, ReplicaGPUs: 1},
	}
	if got := shareKeep(roles, 10)["D"]; got != 8 {
		t.Fatalf("keep %d, want the ceiling 8", got)
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"D": 12, "R": 1},
		Thresholds: map[string]float64{"D": 0.8, "R": 0.8}, Budget: 10, Tolerance: 0.15}
	plan := PlanShareTransfers(NewShareLedger(), in, time.Unix(0, 0), simTimings())
	if plan.Refills == 0 || !slices.ContainsFunc(plan.Started, func(s ShareTransfer) bool { return s.Donor == "D" }) {
		t.Fatalf("debt not repaid from D above its ceiling: refills %d, started %v", plan.Refills, plan.Started)
	}
}

// DonorReleased says whether an abort that is not the donor's own failure
// came after its donor had already given up the pod.
func TestShareDonorReleasedOnAbort(t *testing.T) {
	tm := ShareTimings{Window: time.Minute, ReleaseTimeout: 10 * time.Minute, FillTimeout: time.Hour,
		ReversalHold: time.Minute, SwingWindow: time.Minute}
	t0 := time.Unix(0, 0)
	ended := func(ends []ShareTransferEnd, id string) ShareTransferEnd {
		for _, e := range ends {
			if e.Transfer.ID == id {
				return e
			}
		}
		t.Fatalf("%s did not end", id)
		return ShareTransferEnd{}
	}

	// A plain release that never lands: the donor's own failure.
	l := NewShareLedger()
	held := map[string]int{"A": 4, "B": 0}
	p := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
	if e := ended(l.Observe(held, t0.Add(tm.ReleaseTimeout+time.Second), tm), p.ID); e.DonorReleased {
		t.Fatal("a release that never landed reported the donor as released")
	}

	// A set whose primary's donor released while its contributor did not.
	l = NewShareLedger()
	held = map[string]int{"A": 8, "C": 8, "B": 0}
	p = l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 16, DonorGPUs: 8, SetID: "pending"}, held, t0, tm)
	c := l.Start(ShareTransfer{Donor: "C", DonorGPUs: 8, SetID: "pending"}, held, t0, tm)
	l.LinkSet(p.ID, c.ID)
	held["A"] = 0
	ends := l.Observe(held, t0.Add(tm.ReleaseTimeout+time.Second), tm)
	if !ended(ends, p.ID).DonorReleased {
		t.Fatal("the primary's donor released, and the abort does not say so")
	}
	if ended(ends, c.ID).DonorReleased {
		t.Fatal("the contributor that never released is reported as released")
	}
}
