package steadystate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// failing wraps c so that pod Lists fail while listFails returns true, and pod
// Patches fail while patchFails does.
func failing(c client.Client, listFails, patchFails func() bool) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok && listFails != nil && listFails() {
				return errors.New("the API server is unavailable")
			}
			return cl.List(ctx, list, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*corev1.Pod); ok && patchFails != nil && patchFails() {
				return errors.New("the API server is unavailable")
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
}

// While the marks cannot be read the ledger is not created -- it would start
// blind to releases in flight -- so nothing is planned and every planned
// variant keeps its target. Once the read succeeds, the ledger starts.
func TestUtilizationShareWaitsForTheMarksToBeRead(t *testing.T) {
	f := newShareFleet()
	fresh := sharePods(t, f)
	down := true
	se := newShareEngine(t, f, failing(fresh, func() bool { return down }, nil), time.Unix(0, 0))
	for range 5 {
		o := se.cycle()
		if len(se.e.utilizationShare.ledgers) != 0 {
			t.Fatal("a ledger was created without its marks")
		}
		a, ok := o["ns/A-v"]
		if !ok || a.Target != 9 || !strings.Contains(a.Why, "reading transfer marks") {
			t.Fatalf("A while the marks cannot be read: %+v (present %v), want held at 9", a, ok)
		}
	}
	if m := markedPods(t, fresh); len(m) != 0 {
		t.Fatalf("%d pods marked while the marks could not be read", len(m))
	}
	down = false
	se.cycle()
	if len(se.e.utilizationShare.ledgers) != 1 {
		t.Fatal("the ledger did not start once the marks could be read")
	}
	for _, q := range se.e.utilizationShare.quietUntil {
		if !q.After(se.clock) {
			t.Fatalf("a restored ledger plans at once: quiet until %v, now %v", q, se.clock)
		}
	}
}

// A donor whose pod the API server refuses to patch is not lowered, carries
// no mark, and backs off.
func TestUtilizationShareBacksOffWhenTheMarkPatchFails(t *testing.T) {
	f := newShareFleet()
	fresh := sharePods(t, f)
	se := newShareEngine(t, f, failing(fresh, nil, func() bool { return true }), time.Unix(0, 0))
	for range 20 {
		if a := targetOf(t, se.cycle(), "ns/A-v"); a != 9 {
			t.Fatalf("A lowered to %d although its pod could not be marked", a)
		}
		for _, l := range se.e.utilizationShare.ledgers {
			if l.BackingOff(roleA, se.clock) {
				if m := markedPods(t, fresh); len(m) != 0 {
					t.Fatalf("%d pods marked", len(m))
				}
				return
			}
		}
	}
	t.Fatal("A never backed off after its mark patch failed")
}

// A redirect rewrites a mark under its new receiver and keeps the deletion
// cost the pod had before the mark.
func TestUtilizationShareRemarkKeepsTheUsersCost(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	for i := range f.current["A"] {
		annotate(t, c, "A-v-"+string(rune('0'+i)), map[string]string{podDeletionCostAnnotation: "500"})
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	var id string
	for _, l := range se.e.utilizationShare.ledgers {
		for _, tr := range l.Transfers() {
			id = tr.ID
			se.e.remarkDonorPods(se.ctx, ctrl.LoggerFrom(se.ctx), l, id)
		}
	}
	marked := markedPods(t, c)
	if id == "" || len(marked) != 1 {
		t.Fatalf("setup: id %q, %d marked", id, len(marked))
	}
	var m transferMark
	if err := json.Unmarshal([]byte(marked[0].Annotations[utilizationShareTransferAnnotation]), &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != id || m.PrevCost == nil || *m.PrevCost != "500" {
		t.Fatalf("remarked as %+v, want id %s and the user's cost 500", m, id)
	}
}

// counterSum adds up a counter family's series whose labels include want.
func counterSum(t *testing.T, r *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	sum := 0.0
	for _, m := range family(t, r, name) {
		ok := true
		for k, v := range want {
			if label(m, k) != v {
				ok = false
			}
		}
		if ok {
			sum += m.GetCounter().GetValue()
		}
	}
	return sum
}

// A sweep that could not unmark a pod tries again the next cycle, rather than
// recording the sweep as done.
func TestUtilizationShareRetriesASweepThatCouldNotUnmark(t *testing.T) {
	f := newShareFleet()
	fresh := sharePods(t, f)
	se := newShareEngine(t, f, fresh, time.Unix(0, 0))
	se.untilStarted()
	if len(markedPods(t, fresh)) == 0 {
		t.Fatal("setup: no pod marked")
	}
	down := true
	restarted := newShareEngine(t, f, failing(fresh, nil, func() bool { return down }), se.clock.Add(30*time.Second))
	setShadowPolicy(t, restarted.e.Config, selectedShadow)
	restarted.cycle()
	if len(markedPods(t, fresh)) == 0 {
		t.Fatal("setup: the patch was meant to fail")
	}
	down = false
	restarted.cycle()
	if m := markedPods(t, fresh); len(m) != 0 {
		t.Fatalf("%d pods still marked: the failed sweep was not retried", len(m))
	}
}

// A vanished group whose marks cannot be removed keeps its ledger -- the only
// record of where they are -- and tries again the next cycle.
func TestUtilizationShareRetriesUnmarkingAVanishedGroup(t *testing.T) {
	f := newShareFleet()
	fresh := sharePods(t, f)
	down := false
	se := newShareEngine(t, f, failing(fresh, nil, func() bool { return down }), time.Unix(0, 0))
	se.untilStarted()
	step := func(d time.Duration) {
		se.clock = se.clock.Add(d)
		se.e.evaluateUtilizationShare(se.ctx, nil, fullQuota(), f.scaleTargets())
	}
	step(30 * time.Second)
	down = true
	step(shareAbsenceGrace)
	if len(markedPods(t, fresh)) == 0 || len(se.e.utilizationShare.ledgers) != 1 {
		t.Fatal("setup: the unmark was meant to fail and the ledger to stay")
	}
	down = false
	step(30 * time.Second)
	if m := markedPods(t, fresh); len(m) != 0 {
		t.Fatalf("%d pods still marked: the vanished group's unmark was not retried", len(m))
	}
	if len(se.e.utilizationShare.ledgers) != 0 {
		t.Fatal("the ledger outlived a successful unmark")
	}
}

// Every stop sweeps: an acting cycle re-arms the sweep a previous stop
// recorded as done, so the marks of the next acting period are swept too.
func TestUtilizationShareActingReArmsTheSweep(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	setShadowPolicy(t, se.e.Config, selectedShadow)
	se.cycle()
	if !se.e.utilizationShare.swept {
		t.Fatal("setup: the stop did not sweep")
	}
	setShadowPolicy(t, se.e.Config, activeShare)
	se.cycle()
	if se.e.utilizationShare.swept {
		t.Fatal("acting did not re-arm the sweep: the next stop would leave this period's marks")
	}
}

// A forged prevCost the API server would refuse is not restored: the mark is
// removed with our cost, and the unmark does not fail forever.
func TestUtilizationShareIgnoresAnInvalidPrevCost(t *testing.T) {
	for _, tc := range []struct{ prev, want string }{{"250", "250"}, {"notanumber", ""}, {"99999999999", ""}} {
		if got := validDeletionCost(tc.prev); got != (tc.want != "") {
			t.Fatalf("validDeletionCost(%q) = %v", tc.prev, got)
		}
	}
	f := newShareFleet()
	c := sharePods(t, f)
	raw, _ := json.Marshal(transferMark{ID: "x-t1", Donor: roleA, PrevCost: ptrTo("notanumber")})
	annotate(t, c, "A-v-0", map[string]string{utilizationShareTransferAnnotation: string(raw),
		podDeletionCostAnnotation: donorDeletionCost})
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	if failed := se.e.unmarkDonorPods(se.ctx, ctrl.LoggerFrom(se.ctx),
		allocation.ShareTransfer{DonorPods: []string{"ns/A-v-0"}}); failed != 0 {
		t.Fatalf("%d unmarks failed", failed)
	}
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "A-v-0"}, &p); err != nil {
		t.Fatal(err)
	}
	if v, ok := p.Annotations[podDeletionCostAnnotation]; ok {
		t.Fatalf("deletion cost %q left after unmarking a forged prevCost", v)
	}
}

// A rollback in the cycle of the mark cannot read the pod back: the cache may
// not show the mark yet, and an unmark built from that read patches nothing.
// It unmarks the pod as the mark's patch returned it.
func TestUtilizationShareRollbackUnmarksThePatchedPod(t *testing.T) {
	f := newShareFleet()
	fresh := sharePods(t, f)
	// A cache that never sees writes: every Get returns the pod as it was.
	before := map[string]corev1.Pod{}
	var pods corev1.PodList
	if err := fresh.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		before[p.Name] = *p.DeepCopy()
	}
	stale := interceptor.NewClient(fresh.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if p, ok := obj.(*corev1.Pod); ok {
				if b, found := before[key.Name]; found {
					b.DeepCopyInto(p)
					return nil
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	se := newShareEngine(t, f, stale, time.Unix(0, 0))
	tr := allocation.ShareTransfer{ID: "x-t1", Donor: roleA, DonorVariant: "A-v", DonorGPUs: 1}
	marked, err := se.e.markDonorPods(se.ctx, tr, f.scaleTargets()["ns/A-v"], "ns", func(*corev1.Pod) bool { return false }, false)
	if err != nil || len(marked) != 1 {
		t.Fatalf("setup: marked %d, %v", len(marked), err)
	}

	// Through the stale read, the unmark finds nothing to remove.
	se.e.unmarkDonorPods(se.ctx, ctrl.LoggerFrom(se.ctx), allocation.ShareTransfer{DonorPods: podKeys(marked)})
	if len(markedPods(t, fresh)) != 1 {
		t.Fatal("setup: the stale read was meant to leave the mark")
	}
	if err := se.e.unmarkPod(se.ctx, &marked[0]); err != nil {
		t.Fatal(err)
	}
	if m := markedPods(t, fresh); len(m) != 0 {
		t.Fatalf("%d pods still marked after unmarking the patched object", len(m))
	}
}
