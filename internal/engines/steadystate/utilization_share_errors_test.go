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
		if a := se.cycle()["ns/A-v"].Target; a != 9 {
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
