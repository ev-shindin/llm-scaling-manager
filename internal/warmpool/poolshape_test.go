package warmpool

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// A pool Deployment's shape: GPUs per Pod from its template, the accelerator
// its nodeSelector pins, and what its scheduled Pods hold -- a Pending Pod
// holds nothing, a finished one nothing either.
func TestPoolShapesRead(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	two := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")}
	labels := map[string]string{"app": "pool"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pool"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeSelector: map[string]string{"nvidia.com/gpu.product": "A100"},
				Containers:   []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: two}}},
			}},
		},
	}
	pod := func(name, node string, phase corev1.PodPhase) client.Object {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Labels: labels},
			Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: two}}}},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep,
		pod("running", "n1", corev1.PodRunning),
		pod("starting", "n1", corev1.PodPending),
		pod("unscheduled", "", corev1.PodPending),
		pod("done", "n1", corev1.PodSucceeded),
	).Build()

	got, ok := (&PoolShapes{Client: c}).Read(context.Background(), "ns", "pool")
	if !ok {
		t.Fatal("the Deployment was not read")
	}
	if got != (PoolShape{Accelerator: "A100", PodGPUs: 2, ScheduledGPUs: 4}) {
		t.Fatalf("got %+v, want A100, 2 per Pod, 4 scheduled (running + starting)", got)
	}
	if _, ok := (&PoolShapes{Client: c}).Read(context.Background(), "ns", "absent"); ok {
		t.Fatal("an absent Deployment has no shape")
	}
}

// carvePool runs one pass of a one-pool reconciler with the given members and
// shape, and returns the A100 carve it published.
func carvePool(t *testing.T, r *Reconciler) int {
	t.Helper()
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	return decision.DefaultWarmPoolUnheld.Latest(time.Hour, r.now())[poolNamespace]["A100"]
}

func shapedPool(t *testing.T, shape PoolShape) *Reconciler {
	t.Helper()
	saved := decision.DefaultWarmPoolUnheld
	decision.DefaultWarmPoolUnheld = &decision.WarmPoolUnheldStore{}
	t.Cleanup(func() { decision.DefaultWarmPoolUnheld = saved })
	cfg := testConfig()
	cfg.SleepMinSize = 1
	r := New(&fakePool{}, &staticDemand{}, cfg) // no member Pod yet
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "sized", Config: cfg, Replicas: 1, Deployment: "wva-warm-pool"}}
	r.PublishSize = func(string, string, int32) {}
	r.Shape = func(context.Context, string, string) (PoolShape, bool) { return shape, true }
	return r
}

// The first Pod of a pool in a full quota is not a member -- it is not even
// scheduled -- so only the Deployment's template can size the carve-out.
func TestAPoolWithNoMemberIsCarvedForFromItsTemplate(t *testing.T) {
	r := shapedPool(t, PoolShape{Accelerator: "A100", PodGPUs: 2})
	if got := carvePool(t, r); got != 4 {
		t.Fatalf("carve %d, want 4: two Pods of two GPUs, none held", got)
	}
}

// A Pod scheduled but still starting is not a member yet, and already holds
// its GPUs: it is not carved for twice.
func TestAStartingPoolPodCountsAsHeld(t *testing.T) {
	r := shapedPool(t, PoolShape{Accelerator: "A100", PodGPUs: 2, ScheduledGPUs: 2})
	if got := carvePool(t, r); got != 2 {
		t.Fatalf("carve %d, want 2: one of the two Pods is scheduled", got)
	}
}

// A pool that makes no progress into its carve-out for CarveStall stops being
// carved for; growing again restores it.
func TestACarveOutWithoutProgressLapses(t *testing.T) {
	shape := PoolShape{Accelerator: "A100", PodGPUs: 2}
	r := shapedPool(t, shape)
	t0 := time.Unix(10_000, 0)
	r.now = func() time.Time { return t0 }
	if got := carvePool(t, r); got != 4 {
		t.Fatalf("setup: carve %d, want 4", got)
	}
	r.now = func() time.Time { return t0.Add(CarveStall - time.Second) }
	if got := carvePool(t, r); got != 4 {
		t.Fatalf("before the stall bound: carve %d, want 4", got)
	}
	r.now = func() time.Time { return t0.Add(CarveStall) }
	if got := carvePool(t, r); got != 0 {
		t.Fatalf("at the stall bound: carve %d, want 0", got)
	}
	shape.ScheduledGPUs = 2 // the pool grew
	r.Shape = func(context.Context, string, string) (PoolShape, bool) { return shape, true }
	if got := carvePool(t, r); got != 2 {
		t.Fatalf("after progress: carve %d, want 2", got)
	}
}

// llm-d modelservice pins the accelerator with a required nodeAffinity term
// and no nodeSelector: the shape still names it.
func TestPoolShapesReadsAnAffinityPin(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pool"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pool"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key: "nvidia.com/gpu.product", Operator: corev1.NodeSelectorOpIn, Values: []string{"H100"},
						}}}},
					},
				}},
				Containers: []corev1.Container{{Name: "c"}},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	got, ok := (&PoolShapes{Client: c}).Read(context.Background(), "ns", "pool")
	if !ok || got.Accelerator != "H100" {
		t.Fatalf("got %+v (%v), want accelerator H100 from the affinity pin", got, ok)
	}
}

// A pool that drops out of a pass keeps no stall record: when it comes back
// its carve-out starts a fresh grace period instead of lapsing at once.
func TestAReturningPoolStartsItsStallAfresh(t *testing.T) {
	shape := PoolShape{Accelerator: "A100", PodGPUs: 2}
	r := shapedPool(t, shape)
	t0 := time.Unix(10_000, 0)
	r.now = func() time.Time { return t0 }
	if got := carvePool(t, r); got != 4 {
		t.Fatalf("setup: carve %d, want 4", got)
	}
	pools := r.Pools
	r.Pools = fakePools{} // the pool is gone for a pass
	r.now = func() time.Time { return t0.Add(CarveStall / 2) }
	carvePool(t, r)
	r.Pools = pools
	r.now = func() time.Time { return t0.Add(CarveStall) }
	if got := carvePool(t, r); got != 4 {
		t.Fatalf("back after %s: carve %d, want 4 -- its stall clock must restart", CarveStall, got)
	}
}

// A shape that cannot be read whole is no shape: a failed Pod list must not
// report a pool's scheduled Pods as zero.
func TestPoolShapesReadFailsWhole(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pool"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pool"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("the API is down")
		},
	}).Build()
	if got, ok := (&PoolShapes{Client: c}).Read(context.Background(), "ns", "pool"); ok {
		t.Fatalf("a failed Pod list returned a shape: %+v", got)
	}
}
