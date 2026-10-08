package steadystate

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// A donor whose pod cannot be marked backs off, as after an abort, instead of
// being planned -- and failing -- every cycle.
func TestUtilizationShareBacksOffADonorItCannotMark(t *testing.T) {
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
	for range 20 {
		se.cycle()
		for _, l := range se.e.utilizationShare.ledgers {
			if l.BackingOff(roleA, se.clock) {
				return
			}
		}
	}
	t.Fatal("A was never backed off after its pod could not be marked")
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
		if given := 9 - se.cycle()["ns/A-v"].Target; given > 0 {
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
