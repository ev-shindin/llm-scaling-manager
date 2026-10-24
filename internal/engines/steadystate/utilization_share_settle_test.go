package steadystate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// settleFixture is a ledger holding donor roleA as changing, and a group
// whose origins place it in namespace ns.
func settleFixture(now time.Time, tm allocation.ShareTimings) (*allocation.ShareLedger, allocation.ShareGroup) {
	l := allocation.NewShareLedger()
	l.HoldChanging(roleA, "A-v", roleB, "a pod is not ready", now, tm)
	g := allocation.ShareGroup{Origins: map[string]allocation.ShareRoleOrigin{
		roleA: {Namespace: "ns", ModelID: "A"}, roleB: {Namespace: "ns", ModelID: "B"}}}
	return l, g
}

// A held donor whose workload cannot be read -- its scale target gone, or the
// pod list failing -- counts neither as stable nor as changing: it stays held,
// with its count where it was, until the release timeout.
func TestSettleChangingDonorsKeepsAnUnreadableDonorHeld(t *testing.T) {
	tm := allocation.ShareTimings{ReleaseTimeout: 10 * time.Minute}
	now := time.Unix(1000, 0)
	f := newShareFleet()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	failing := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("list refused")
		}}).Build()
	for _, c := range []struct {
		name     string
		e        *Engine
		accessor func(role, variant string) scaletarget.ScaleTargetAccessor
	}{
		{"no scale target", &Engine{client: sharePods(t, f)},
			func(string, string) scaletarget.ScaleTargetAccessor { return nil }},
		{"no client", &Engine{},
			func(string, string) scaletarget.ScaleTargetAccessor { return f.scaleTargets()["ns/A-v"] }},
		{"pods unreadable", &Engine{client: failing},
			func(string, string) scaletarget.ScaleTargetAccessor { return f.scaleTargets()["ns/A-v"] }},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, g := settleFixture(now, tm)
			at := now
			for range 10 {
				at = at.Add(30 * time.Second)
				c.e.settleChangingDonors(context.Background(), logr.Discard(), l, g, c.accessor, 2, at)
			}
			if h := l.ChangingHolds(at); len(h) != 1 || h[0].SteadyCycles != 0 {
				t.Fatalf("after 10 unreadable cycles: holds %+v, want A still held with no stable cycle", h)
			}
			if l.Changing(roleA, now.Add(tm.ReleaseTimeout)) {
				t.Fatal("an unreadable donor outlived the release timeout")
			}
		})
	}
}

// A donor whose every pod is already given to a live transfer is exhausted,
// not changing: its workload is stable, so the changing hold ends (GivingBusy
// holds it on the transfers alone). An LWS, whose only group is marked.
func TestSettleChangingDonorsCountsAnExhaustedDonorStable(t *testing.T) {
	tm := allocation.ShareTimings{ReleaseTimeout: 10 * time.Minute, ReversalHold: time.Minute}
	now := time.Unix(1000, 0)
	l, g := settleFixture(now, tm)
	tr := l.Start(allocation.ShareTransfer{Donor: roleA, DonorVariant: "A-v", Receiver: roleB, GPUs: 2, DonorGPUs: 2},
		map[string]int{roleA: 4, roleB: 2}, now, tm)
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, 2)
	for w := range 2 {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: fmt.Sprintf("a-0-%d", w),
				Labels:          map[string]string{lwsv1.SetNameLabelKey: "a", lwsv1.GroupIndexLabelKey: strconv.Itoa(0)},
				Annotations:     map[string]string{utilizationShareTransferAnnotation: `{"id":"` + tr.ID + `"}`},
				OwnerReferences: lwsStatefulSetOwner("a")},
			Spec:   corev1.PodSpec{NodeName: "n1"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	e := &Engine{client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()}
	acc := scaletarget.NewLWSAccessor(&lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a"},
		Spec: lwsv1.LeaderWorkerSetSpec{Replicas: ptr.To[int32](1),
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{Size: ptr.To[int32](2)}}})
	if _, err := e.steerableDonorPods(context.Background(), acc, "A-v", "ns", shareMarkOwned(l, nil)); !errors.Is(err, errDonorExhausted) {
		t.Fatalf("setup: err %v, want errDonorExhausted", err)
	}
	accessor := func(string, string) scaletarget.ScaleTargetAccessor { return acc }
	e.settleChangingDonors(context.Background(), logr.Discard(), l, g, accessor, 2, now.Add(30*time.Second))
	if !l.Changing(roleA, now.Add(30*time.Second)) {
		t.Fatal("released after one stable cycle of two")
	}
	e.settleChangingDonors(context.Background(), logr.Discard(), l, g, accessor, 2, now.Add(time.Minute))
	if l.Changing(roleA, now.Add(time.Minute)) {
		t.Fatal("an exhausted donor is still held as changing after two stable cycles")
	}
}
