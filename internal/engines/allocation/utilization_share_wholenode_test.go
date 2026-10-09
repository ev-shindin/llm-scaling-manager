package allocation

import (
	"testing"
	"time"
)

// An LWS receiver of three 8-GPU pods is funded from 8-GPU donor pods: a donor
// set of three, counted once against the concurrency limit, and the donor may
// give as many replicas as the receiver has pods. Counted per member, or held
// to the pace of two, it was never funded.
func TestShareFundsAThreeNodeLWSReceiver(t *testing.T) {
	roles := []ShareRole{
		{Key: "R", Weight: 1, Need: 40, Ceiling: 96, ReplicaGPUs: 24},
		{Key: "D", Weight: 1, Need: 10, Ceiling: 96, ReplicaGPUs: 8},
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"R": 24, "D": 48},
		Thresholds: map[string]float64{"R": 0.8, "D": 0.8}, Budget: 72, Tolerance: 0.15,
		Grow: map[string]ShareVariant{"R": {Name: "r", GPUs: 24, PodGPUs: []int{8, 8, 8}}},
		Give: map[string]ShareVariant{"D": {Name: "d", GPUs: 8, PodGPUs: []int{8}}}}
	started := planCycles(in, NewShareLedger())
	sets := map[string]int{}
	for _, s := range started {
		if s.Donor == "D" {
			sets[s.SetID]++
		}
	}
	funded := false
	for id, n := range sets {
		funded = funded || (id != "" && n == 3)
	}
	if !funded {
		t.Fatalf("no set of three 8-GPU donor pods funded the 3x8 receiver: started %v", started)
	}
}

// A receiver whose last fill timed out with its pods Pending is not funded
// again until the hold ends: funding it would take another whole node for a
// pod that cannot be placed either.
func TestShareFillHeldReceiverIsNotFunded(t *testing.T) {
	roles := []ShareRole{
		{Key: "R", Weight: 1, Need: 20, Ceiling: 64, ReplicaGPUs: 8},
		{Key: "D", Weight: 1, Need: 4, Ceiling: 64, ReplicaGPUs: 8},
	}
	in := SharePlanInput{Roles: roles, Held: map[string]int{"R": 8, "D": 24},
		Thresholds: map[string]float64{"R": 0.8, "D": 0.8}, Budget: 32, Tolerance: 0.15}
	l := NewShareLedger()
	l.HoldFill("R", time.Unix(1000, 0).Add(time.Hour))
	if started := planCycles(in, l); len(started) != 0 {
		t.Fatalf("a fill-held receiver was funded: %v", started)
	}
	if started := planCycles(in, NewShareLedger()); len(started) == 0 {
		t.Fatal("control: the receiver is not funded without the hold")
	}
}

// An LWS receiver's fill ends when every pod of its new group is scheduled,
// not when its leader alone is: the other holes stay promised until then.
func TestShareFillWaitsForTheWholeGroup(t *testing.T) {
	tm := simTimings()
	t0 := time.Unix(0, 0)
	l := NewShareLedger()
	held := map[string]int{"D": 24, "R": 24}
	tr := l.Start(ShareTransfer{Donor: "D", Receiver: "R", GPUs: 24, DonorGPUs: 24}, held, t0, tm)
	l.ReceiverFilled(map[string]int{"D": 24, "R": 24})
	l.Observe(map[string]int{"D": 0, "R": 24}, t0.Add(time.Minute), tm) // released
	l.TakeReleased()
	// The leader of the new group is bound: held counts the group, filled
	// does not.
	l.ReceiverFilled(map[string]int{"D": 0, "R": 24})
	if ends := l.Observe(map[string]int{"D": 0, "R": 48}, t0.Add(2*time.Minute), tm); len(ends) != 0 {
		t.Fatalf("the fill ended on the leader alone: %v", ends)
	}
	if got, _ := l.Transfer(tr.ID); got.State != ShareFilling {
		t.Fatalf("state %v, want still filling", got.State)
	}
	l.ReceiverFilled(map[string]int{"D": 0, "R": 48})
	ends := l.Observe(map[string]int{"D": 0, "R": 48}, t0.Add(3*time.Minute), tm)
	if len(ends) != 1 || ends[0].Outcome != ShareOutcomeDone {
		t.Fatalf("every pod of the group scheduled: ends %v, want done", ends)
	}
}

// A donor set in flight counts once against the concurrency limit, whatever
// its members: a 3x8 set must leave room for one more transfer.
func TestShareInFlightCountsASetOnce(t *testing.T) {
	tm := simTimings()
	t0 := time.Unix(0, 0)
	l := NewShareLedger()
	held := map[string]int{"A": 24, "C": 8, "B": 0}
	p := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 24, DonorGPUs: 8, SetID: "pending"}, held, t0, tm)
	c1 := l.Start(ShareTransfer{Donor: "A", DonorGPUs: 8, SetID: "pending"}, held, t0, tm)
	c2 := l.Start(ShareTransfer{Donor: "C", DonorGPUs: 8, SetID: "pending"}, held, t0, tm)
	l.LinkSet(p.ID, c1.ID, c2.ID)
	if n := l.InFlight(); n != 1 {
		t.Fatalf("a set of three in flight counts %d, want 1", n)
	}
	l.Start(ShareTransfer{Donor: "C", Receiver: "B", GPUs: 8, DonorGPUs: 8}, held, t0, tm)
	if n := l.InFlight(); n != 2 {
		t.Fatalf("a set and a single count %d, want 2", n)
	}
}
