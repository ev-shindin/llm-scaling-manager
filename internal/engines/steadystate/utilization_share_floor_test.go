package steadystate

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// A model allowed to scale to zero, idle at one replica, is never left at zero
// by a transfer: parking is scale-to-zero's decision, with its retention, not
// the optimizer's.
func TestUtilizationShareNeverGivesTheLastReplica(t *testing.T) {
	f := newShareFleet()
	f.current["C"], f.demand["C"] = 1, 0
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	zero := 0
	for range 40 {
		se.clock = se.clock.Add(30 * time.Second)
		reqs := f.requests()
		for i := range reqs {
			if reqs[i].ModelID == "C" {
				reqs[i].VariantStates[0].MinReplicas = &zero
			}
		}
		o := se.e.evaluateUtilizationShare(se.ctx, reqs, fullQuota(), f.scaleTargets())
		if got, ok := o["ns/C-v"]; ok && got.Target < 1 {
			t.Fatalf("C's last replica given away: target %d", got.Target)
		}
	}
}

// A ledger that has just started is quiet for a fill timeout: it plans
// nothing, and every planned variant holds what it runs. Today's optimizer
// must not lower one model and raise another meanwhile -- a fill in flight
// before a restart has no mark, and its receiver's pods wait for those GPUs.
func TestUtilizationShareQuietPeriodHoldsEveryPlannedVariant(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	o := se.cycle()
	if len(se.e.utilizationShare.ledgers) != 1 {
		t.Fatal("setup: the ledger did not start")
	}
	for k, want := range map[string]int{"ns/A-v": 9, "ns/B-v": 5, "ns/C-v": 2} {
		if got, ok := o[k]; !ok || got.Target != want {
			t.Fatalf("%s during the quiet period: %+v (present %v), want held at %d", k, got, ok, want)
		}
	}
}

// targetOf is the target a cycle leaves a fixture variant at: its override,
// or, with none, the count the share fleet runs -- the variant is left to
// today's optimizer, which these fixtures do not run.
func targetOf(o map[string]utilizationShareOverride, key string) int {
	if v, ok := o[key]; ok {
		return v.Target
	}
	return map[string]int{"ns/A-v": 9, "ns/B-v": 5, "ns/C-v": 2}[key]
}

// A donor with a pod not yet scheduled cannot be steered -- the ReplicaSet
// removes that pod first, whatever the costs -- and is never lowered.
func TestUtilizationShareRefusesADonorWithAnUnscheduledPod(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "A-v-4"}, &p); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	p.ResourceVersion, p.Spec.NodeName = "", ""
	if err := c.Create(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	for range 20 {
		if a := targetOf(se.cycle(), "ns/A-v"); a != 9 {
			t.Fatalf("A lowered to %d although one of its pods is not scheduled", a)
		}
	}
	if m := markedPods(t, c); len(m) != 0 {
		t.Fatalf("%d pods marked", len(m))
	}
}

// The pod given is the one the user protected least: the lowest deletion cost.
func TestUtilizationShareGivesTheLeastProtectedPod(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	for i := range f.current["A"] {
		annotate(t, c, "A-v-"+string(rune('0'+i)), map[string]string{podDeletionCostAnnotation: "100"})
	}
	annotate(t, c, "A-v-6", map[string]string{podDeletionCostAnnotation: "-5"})
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	m := markedPods(t, c)
	if len(m) != 1 || m[0].Name != "A-v-6" {
		names := make([]string, 0, len(m))
		for _, p := range m {
			names = append(names, p.Name)
		}
		t.Fatalf("marked %v, want the least protected A-v-6", names)
	}
}

// A deletion cost someone set after the mark is theirs: the unmark keeps it.
func TestUtilizationShareUnmarkKeepsACostSetSinceTheMark(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	m := markedPods(t, c)
	if len(m) != 1 {
		t.Fatalf("setup: %d marked", len(m))
	}
	annotate(t, c, m[0].Name, map[string]string{podDeletionCostAnnotation: "42"})
	se.e.unmarkDonorPods(se.ctx, ctrl.LoggerFrom(se.ctx), allocation.ShareTransfer{DonorPods: []string{"ns/" + m[0].Name}})
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: m[0].Name}, &p); err != nil {
		t.Fatal(err)
	}
	if got := p.Annotations[podDeletionCostAnnotation]; got != "42" {
		t.Fatalf("deletion cost %q after the unmark, want the 42 set since the mark", got)
	}
	if _, ok := p.Annotations[utilizationShareTransferAnnotation]; ok {
		t.Fatal("the mark itself was kept")
	}
}
