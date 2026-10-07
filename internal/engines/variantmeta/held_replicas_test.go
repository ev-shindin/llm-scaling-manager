package variantmeta

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

func heldPod(name string, labels map[string]string, node string, phase corev1.PodPhase, deleting bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if deleting {
		// The fake client accepts a deletion timestamp only on an object that
		// still has a finalizer, which is exactly a pod still draining.
		p.Finalizers = []string{"drain"}
		p.DeletionTimestamp = &metav1.Time{}
	}
	return p
}

func heldClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// A terminating pod still holds its GPUs; status.replicas has already dropped
// it. Counting it is the point: a release judged without it hands the donor's
// GPUs out while the pod is still draining.
func TestHeldReplicasCountsTerminatingPods(t *testing.T) {
	sel := map[string]string{"app": "m"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](2),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: sel}},
		},
		Status: appsv1.DeploymentStatus{Replicas: 2},
	}
	c := heldClient(t,
		heldPod("running", sel, "n1", corev1.PodRunning, false),
		heldPod("draining", sel, "n1", corev1.PodRunning, true),
		heldPod("starting", sel, "n2", corev1.PodPending, false),
		heldPod("unscheduled", sel, "", corev1.PodPending, false),
		heldPod("finished", sel, "n2", corev1.PodSucceeded, false),
		heldPod("other", map[string]string{"app": "x"}, "n1", corev1.PodRunning, false),
	)
	held, known := heldReplicaCount(context.Background(), c, "ns", scaletarget.NewDeploymentAccessor(d))
	if !known || held != 3 {
		t.Fatalf("held = %d (known %t), want 3: running, draining and scheduled-starting", held, known)
	}
}

// On a LeaderWorkerSet a replica is a group: four pods of one group are one
// held replica, and a group with only its draining leader left still holds.
func TestHeldReplicasCountsLWSGroups(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pd", Namespace: "ns"},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas:             ptr.To[int32](2),
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{Size: ptr.To[int32](4)},
		},
	}
	group := func(idx string) map[string]string {
		return map[string]string{lwsv1.SetNameLabelKey: "pd", lwsv1.GroupIndexLabelKey: idx}
	}
	c := heldClient(t,
		heldPod("pd-0", group("0"), "n1", corev1.PodRunning, false),
		heldPod("pd-0-1", group("0"), "n1", corev1.PodRunning, false),
		heldPod("pd-0-2", group("0"), "n2", corev1.PodRunning, false),
		heldPod("pd-0-3", group("0"), "n2", corev1.PodRunning, false),
		heldPod("pd-1", group("1"), "n3", corev1.PodRunning, true),
		heldPod("pd-2", group("2"), "", corev1.PodPending, false),
	)
	held, known := heldReplicaCount(context.Background(), c, "ns", scaletarget.NewLWSAccessor(lws))
	if !known || held != 2 {
		t.Fatalf("held = %d (known %t), want 2 groups", held, known)
	}
}

func TestHeldReplicasUnknownWithoutAClient(t *testing.T) {
	if _, known := heldReplicaCount(context.Background(), nil, "ns", nil); known {
		t.Fatal("a missing client must report the count as unknown, not zero")
	}
}
