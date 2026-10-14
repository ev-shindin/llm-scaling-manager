package steadystate

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// fillCase runs the idle fill once over two 1-GPU roles, A and B, with A
// returning one replica (still releasing), and returns the desired counts.
func fillCase(t *testing.T, budget int, integer map[string]int) map[string]int {
	t.Helper()
	g := allocation.ShareGroup{
		Budget: budget, ClusterFree: 1 << 20,
		Roles: []allocation.ShareRole{
			{Key: "A", Weight: 1, Need: 4, ReplicaGPUs: 1},
			{Key: "B", Weight: 1, Need: 4, ReplicaGPUs: 1},
		},
		Grow: map[string]allocation.ShareVariant{"A": {Name: "a", GPUs: 1}, "B": {Name: "b", GPUs: 1}},
	}
	held := map[string]int{"A": 6, "B": 3}
	l := allocation.NewShareLedger()
	tm := allocation.ShareTimings{Window: time.Minute, ReleaseTimeout: time.Hour, FillTimeout: time.Hour}
	now := time.Unix(1000, 0)
	l.Start(allocation.ShareTransfer{Donor: "A", DonorGPUs: 1, DonorVariant: "a", Entitled: true, Returned: true}, held, now, tm)
	ev := allocation.ShareEvaluation{Roles: []allocation.ShareRoleVerdict{{Key: "A", Integer: integer["A"]}, {Key: "B", Integer: integer["B"]}}}
	e := &Engine{}
	e.utilizationShare.desired = map[string]int{"A/a": 5, "B/b": 3}
	key := func(role, variant string) string { return role + "/" + variant }
	e.fillIdleShare(logr.Discard(), l, g, ev, held, shareFit{}, key, now, tm)
	return map[string]int{"A": e.utilizationShare.desired["A/a"], "B": e.utilizationShare.desired["B/b"]}
}

// GPUs a return has not released are not idle: with the quota full but for
// them, nobody is raised into a pod that quota admission would refuse.
func TestShareFillSkipsUnreleasedGPUs(t *testing.T) {
	// Budget 9: A commits 5 (6 held, one returning), B 3; the one idle GPU is
	// the returning one.
	if d := fillCase(t, 9, map[string]int{"A": 5, "B": 4}); d["B"] != 3 {
		t.Fatalf("B raised to %d into GPUs a return still holds", d["B"])
	}
	// Control: one more GPU in the quota is truly idle, and B takes it.
	if d := fillCase(t, 10, map[string]int{"A": 5, "B": 4}); d["B"] != 4 {
		t.Fatalf("control: B at %d, want 4 from the idle GPU", d["B"])
	}
}

// A role whose return is still releasing is not raised by the fill: raised, its
// return would never see it shrink, would abort, and restore it one above.
// Below its need, a model is filled to its target in one cycle: a fill drains
// nobody, and pacing it would leave it short beside idle GPUs. At or above
// its need, the fill keeps the pace.
func TestShareFillIsUnpacedBelowNeed(t *testing.T) {
	fill := func(need float64) int {
		g := allocation.ShareGroup{
			Budget: 8, ClusterFree: 1 << 20,
			Roles: []allocation.ShareRole{{Key: "B", Weight: 1, Need: need, ReplicaGPUs: 1}},
			Grow:  map[string]allocation.ShareVariant{"B": {Name: "b", GPUs: 1}},
		}
		ev := allocation.ShareEvaluation{Roles: []allocation.ShareRoleVerdict{{Key: "B", Integer: 8}}}
		e := &Engine{}
		e.utilizationShare.desired = map[string]int{"B/b": 3}
		key := func(role, variant string) string { return role + "/" + variant }
		e.fillIdleShare(logr.Discard(), allocation.NewShareLedger(), g, ev, map[string]int{"B": 3}, shareFit{}, key,
			time.Unix(1000, 0), allocation.ShareTimings{FillTimeout: time.Hour})
		return e.utilizationShare.desired["B/b"] - 3
	}
	if n := fill(9); n != 5 {
		t.Fatalf("below its need: filled %d, want all 5 idle at once", n)
	}
	if n := fill(2); n != allocation.ShareMaxReplicasPerCycle {
		t.Fatalf("above its need: filled %d, want the pace (%d)", n, allocation.ShareMaxReplicasPerCycle)
	}
}

// A donor whose pod cannot be marked starts nothing more that cycle: an
// unpaced move plans several transfers from it, and starting the rest after
// backing it off would contradict the back-off.
func TestShareFailedDonorStartsNothingMore(t *testing.T) {
	f := newShareFleet()
	f.demand = map[string]float64{"A": 1600, "B": 12000, "C": 1000}
	fresh := sharePods(t, f)
	patches := 0
	// The first mark (the paced first cycle's) lands; the second -- the first
	// of the unpaced cycle -- fails, and so does nothing after it.
	se := newShareEngine(t, f, failing(fresh, nil, func() bool { patches++; return patches == 2 }), time.Unix(0, 0))
	for range 30 {
		se.cycle()
		if patches >= 2 {
			break
		}
	}
	if patches < 2 {
		t.Fatalf("setup: only %d mark patches in 30 cycles", patches)
	}
	if m := len(markedPods(t, fresh)); m != 1 {
		t.Fatalf("%d pods marked, want 1: after its mark failed, A started nothing more that cycle", m)
	}
	for _, l := range se.e.utilizationShare.ledgers {
		n := 0
		for _, tr := range l.Transfers() {
			if tr.Donor == roleA {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("A has %d transfers in the ledger, want the 1 that was marked", n)
		}
	}
}

func TestShareFillSkipsARoleReturning(t *testing.T) {
	if d := fillCase(t, 12, map[string]int{"A": 6, "B": 3}); d["A"] != 5 {
		t.Fatalf("A raised to %d while its return releases", d["A"])
	}
}
