package steadystate

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// A transfer annotation no live transfer owns -- put in a pod template, or
// left by a transfer long gone -- does not stop its model from giving. The
// marks are forged after the ledger started: the restore at start removes
// marks it did not write, but a template puts them on every new pod again.
func TestUtilizationShareIgnoresForeignMarks(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.cycle() // the ledger starts, and restores from the (unmarked) pods
	for i := range f.current["A"] {
		annotate(t, c, "A-v-"+string(rune('0'+i)), map[string]string{
			utilizationShareTransferAnnotation: `{"id":"forged"}`})
	}
	if started := se.untilStarted(); started == 0 {
		t.Fatal("A gave nothing: a forged mark blocked it")
	}
	for _, p := range markedPods(t, c) {
		var m transferMark
		if json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m) == nil && m.ID == "forged" &&
			p.Annotations[podDeletionCostAnnotation] == donorDeletionCost {
			t.Fatalf("pod %s carries our deletion cost under a forged mark", p.Name)
		}
	}
}

// A donor with a pod that is not Ready -- still starting, or a rollout -- is
// held, not failed: it is not planned again every cycle, and it does not back
// off as after an abort (TestUtilizationShareBacksOffWhenTheMarkPatchFails is
// the fault).
func TestUtilizationShareHoldsADonorWithAPodNotReady(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "A-v-4"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	held := false
	for range 20 {
		se.cycle()
		for _, l := range se.e.utilizationShare.ledgers {
			if l.BackingOff(roleA, se.clock) || l.Unsteerable(roleA, se.clock) {
				t.Fatal("a donor with a pod still starting backs off or is unsteerable")
			}
			held = held || l.GivingBusy(roleA, se.clock)
		}
	}
	if !held {
		t.Fatal("A was never held after its pod could not be marked")
	}
}

// The client reads pods through a cache that may not yet show a patch made
// this cycle: two transfers from one donor in one cycle must still mark two
// pods, not the same one twice.
func TestUtilizationShareMarksDistinctPodsThroughAStaleCache(t *testing.T) {
	f := newShareFleet()
	f.demand["C"] = 6000 // B and C both short: A gives to each in one cycle
	fresh := sharePods(t, f)
	var snapshot *corev1.PodList
	c := interceptor.NewClient(fresh.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			pl, ok := list.(*corev1.PodList)
			if !ok {
				return cl.List(ctx, list, opts...)
			}
			if snapshot == nil {
				// The stale cache: what the first list of the cycle saw.
				if err := cl.List(ctx, pl, opts...); err != nil {
					return err
				}
				snapshot = pl.DeepCopy()
				return nil
			}
			snapshot.DeepCopyInto(pl)
			return nil
		},
	})
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	for range 20 {
		snapshot = nil // each cycle starts with a fresh cache, then goes stale
		if given := 9 - targetOf(t, se.cycle(), "ns/A-v"); given > 0 {
			if marked := len(markedPods(t, fresh)); marked != given {
				t.Fatalf("A gave %d replicas but %d distinct pods are marked", given, marked)
			}
			if given < 2 {
				t.Fatalf("setup: only %d transfer started in the cycle; the test needs two", given)
			}
			return
		}
	}
	t.Fatal("setup: no transfer started")
}

// Switching to shadow (the documented rollback) unmarks the donor pods, and
// gives back a deletion cost the user had set before the mark.
func TestUtilizationShareShadowUnmarksAndRestoresTheUsersCost(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	for i := range f.current["A"] {
		annotate(t, c, "A-v-"+string(rune('0'+i)), map[string]string{podDeletionCostAnnotation: "500"})
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	marked := markedPods(t, c)
	if len(marked) == 0 || marked[0].Annotations[podDeletionCostAnnotation] != donorDeletionCost {
		t.Fatalf("setup: want a marked A pod at cost %s", donorDeletionCost)
	}
	name := marked[0].Name

	setShadowPolicy(t, se.e.Config, selectedShadow)
	se.cycle()
	if m := markedPods(t, c); len(m) != 0 {
		t.Fatalf("%d pods still marked in shadow mode", len(m))
	}
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	if got := p.Annotations[podDeletionCostAnnotation]; got != "500" {
		t.Fatalf("pod %s deletion cost %q after unmarking, want the user's 500", name, got)
	}
}

// A donor out of the group for a cycle -- frozen, or not collected -- keeps
// the target its transfer lowered, so the transfer is not lost.
func TestUtilizationShareKeepsAnInFlightDonorsTarget(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	given := se.untilStarted()
	se.clock = se.clock.Add(30 * time.Second)
	var withoutA = f.requests()[1:] // A first in the fixture: drop it for one cycle
	o := se.e.evaluateUtilizationShare(se.ctx, withoutA, fullQuota(), f.scaleTargets())
	if got, ok := o["ns/A-v"]; !ok || got.Target != 9-given {
		t.Fatalf("A's target while it is out of the group: %+v (present %v), want %d", got, ok, 9-given)
	}
}

// A group that vanishes -- its last model gone, its namespace disabled --
// takes its marks with its ledger, as switching the optimizer off does. One
// missing for less than the grace -- a failed collection -- keeps both.
func TestUtilizationShareUnmarksAVanishedGroup(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	given := se.untilStarted()
	marked := len(markedPods(t, c))
	if marked == 0 {
		t.Fatal("setup: no pod marked")
	}
	se.clock = se.clock.Add(30 * time.Second)
	o := se.e.evaluateUtilizationShare(se.ctx, nil, fullQuota(), f.scaleTargets())
	if n := len(markedPods(t, c)); n != marked || len(se.e.utilizationShare.ledgers) != 1 {
		t.Fatalf("a group missing for one cycle lost its state: %d marked (was %d), %d ledgers",
			n, marked, len(se.e.utilizationShare.ledgers))
	}
	if got := targetOf(t, o, "ns/A-v"); got != 9-given {
		t.Fatalf("A's target while its group is missing = %d, want %d held", got, 9-given)
	}
	se.clock = se.clock.Add(shareAbsenceGrace)
	se.e.evaluateUtilizationShare(se.ctx, nil, fullQuota(), f.scaleTargets())
	if m := markedPods(t, c); len(m) != 0 {
		t.Fatalf("%d pods still marked after their group vanished", len(m))
	}
	if len(se.e.utilizationShare.ledgers) != 0 {
		t.Fatal("the vanished group's ledger was kept")
	}
}

// A group back within the grace starts its absence afresh the next time.
func TestUtilizationShareRestartsAGroupsAbsenceWhenItReturns(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	step := func(d time.Duration, reqs bool) {
		se.clock = se.clock.Add(d)
		r := f.requests()
		if !reqs {
			r = nil
		}
		se.e.evaluateUtilizationShare(se.ctx, r, fullQuota(), f.scaleTargets())
	}
	step(30*time.Second, false)
	step(shareAbsenceGrace-time.Minute, true) // back before the grace ran out
	step(30*time.Second, false)
	step(shareAbsenceGrace-time.Minute, false)
	if len(se.e.utilizationShare.ledgers) != 1 {
		t.Fatal("the absence was counted from before the group returned")
	}
}

// A cycle that reads no constraint -- possibly a failed read -- holds the
// targets of the variants transfers move, for a grace; past it the state goes.
func TestUtilizationShareHoldsInFlightTargetsWhileNoBoundIsRead(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	given := se.untilStarted()
	se.clock = se.clock.Add(30 * time.Second)
	o := se.e.evaluateUtilizationShare(se.ctx, f.requests(), nil, f.scaleTargets())
	if got, ok := o["ns/A-v"]; !ok || got.Target != 9-given {
		t.Fatalf("A's target with no bound read: %+v (present %v), want %d", got, ok, 9-given)
	}
	if len(se.e.utilizationShare.ledgers) == 0 {
		t.Fatal("the ledger was dropped on the first cycle with no bound")
	}
	se.clock = se.clock.Add(shareAbsenceGrace)
	if o := se.e.evaluateUtilizationShare(se.ctx, f.requests(), nil, f.scaleTargets()); len(o) != 0 {
		t.Fatalf("targets still held past the grace: %v", o)
	}
	if len(se.e.utilizationShare.ledgers) != 0 {
		t.Fatal("the ledger outlived the grace")
	}
	if m := markedPods(t, c); len(m) != 0 {
		t.Fatalf("%d pods still marked with no bound past the grace: nothing is left to share", len(m))
	}
}

// A transfer has one donor: a mark carrying the id of a transfer from another
// donor -- a copied mark, a forged second primary -- is not restored, and
// neither is the transfer it copies.
func TestUtilizationShareRestoreRejectsAnIDUnderTwoDonors(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	marked := markedPods(t, c)
	if len(marked) != 1 {
		t.Fatalf("setup: want one marked pod, got %d", len(marked))
	}
	var m transferMark
	if err := json.Unmarshal([]byte(marked[0].Annotations[utilizationShareTransferAnnotation]), &m); err != nil {
		t.Fatal(err)
	}
	// Onto C, neither this transfer's donor nor its receiver: the copy is
	// valid on its own, and only its id gives it away.
	m.Donor, m.DonorVariant = "ns/C/"+domain.RoleBoth, "C-v"
	raw, _ := json.Marshal(m)
	annotate(t, c, "C-v-0", map[string]string{utilizationShareTransferAnnotation: string(raw)})

	restarted := newShareEngine(t, f, c, se.clock.Add(30*time.Second))
	if got := targetOf(t, restarted.cycle(), "ns/A-v"); got != 9 {
		t.Fatalf("A target after restart = %d, want 9: the copied id restored a transfer", got)
	}
	if n := len(markedPods(t, c)); n != 0 {
		t.Fatalf("%d pods still marked", n)
	}
}

// A bound read again resets the grace: a later failed read counts from then.
func TestUtilizationShareRestartsTheNoBoundGraceWhenABoundIsRead(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	given := se.untilStarted()
	step := func(d time.Duration, constraints bool) map[string]utilizationShareOverride {
		se.clock = se.clock.Add(d)
		q := fullQuota()
		if !constraints {
			q = nil
		}
		return se.e.evaluateUtilizationShare(se.ctx, f.requests(), q, f.scaleTargets())
	}
	step(30*time.Second, false)
	step(shareAbsenceGrace-time.Minute, true)
	step(30*time.Second, false)
	o := step(shareAbsenceGrace-time.Minute, false)
	if got, ok := o["ns/A-v"]; !ok || got.Target > 9-given {
		t.Fatalf("A's target %+v (present %v): the grace counted from the first failed read", got, ok)
	}
}

// Switched to shadow while no bound is read, the optimizer holds nothing.
func TestUtilizationShareHoldsNothingInShadowWithoutABound(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	setShadowPolicy(t, se.e.Config, selectedShadow)
	se.clock = se.clock.Add(30 * time.Second)
	if o := se.e.evaluateUtilizationShare(se.ctx, f.requests(), nil, f.scaleTargets()); len(o) != 0 {
		t.Fatalf("shadow mode held targets: %v", o)
	}
}

// A controller restarted into shadow mode has no ledger to unmark from: it
// sweeps the marks it wrote, and gives back the user's deletion cost. A mark
// another controller instance wrote is left alone.
func TestUtilizationShareSweepsItsMarksAfterARestartIntoShadow(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	for i := range f.current["A"] {
		annotate(t, c, "A-v-"+string(rune('0'+i)), map[string]string{podDeletionCostAnnotation: "500"})
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	marked := markedPods(t, c)
	if len(marked) == 0 {
		t.Fatal("setup: no pod marked")
	}
	other, _ := json.Marshal(transferMark{ID: "x-t1", Donor: "ns/C/" + domain.RoleBoth, Instance: "other"})
	annotate(t, c, "C-v-0", map[string]string{utilizationShareTransferAnnotation: string(other)})

	restarted := newShareEngine(t, f, c, se.clock.Add(30*time.Second))
	setShadowPolicy(t, restarted.e.Config, selectedShadow)
	restarted.cycle()
	left := markedPods(t, c)
	if len(left) != 1 || left[0].Name != "C-v-0" {
		names := make([]string, 0, len(left))
		for _, p := range left {
			names = append(names, p.Name)
		}
		t.Fatalf("marked after the sweep: %v, want only the other instance's C-v-0", names)
	}
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: marked[0].Name}, &p); err != nil {
		t.Fatal(err)
	}
	if got := p.Annotations[podDeletionCostAnnotation]; got != "500" {
		t.Fatalf("deletion cost %q after the sweep, want the user's 500", got)
	}
}
