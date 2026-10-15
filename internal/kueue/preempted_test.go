package kueue

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// preemptionWorkload is a Workload in eligNS carrying the given conditions,
// owned by one pod.
func preemptionWorkload(name string, conds ...interface{}) *unstructured.Unstructured {
	w := obj("v1beta2", KindWorkload, eligNS, name, map[string]interface{}{})
	w.SetUID(types.UID("uid-" + name))
	w.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "pod-" + name, UID: types.UID("pod-" + name)}})
	if len(conds) > 0 {
		w.Object["status"] = map[string]interface{}{"conditions": conds}
	}
	return w
}

func condition(typ, status, reason string, at time.Time) map[string]interface{} {
	return map[string]interface{}{"type": typ, "status": status, "reason": reason,
		"lastTransitionTime": at.UTC().Format(time.RFC3339)}
}

func preemptionClient(t *testing.T, funcs *interceptor.Funcs, mapper meta.RESTMapper, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

// PreemptedWorkloads returns exactly the Workloads Kueue evicted with reason
// Preempted, however long ago (the caller judges the age), with their owners
// and the condition's time.
func TestPreemptedWorkloads(t *testing.T) {
	now := time.Unix(100000, 0).UTC()
	c := preemptionClient(t, nil, eligibilityMapper(),
		preemptionWorkload("preempted", condition("Evicted", "True", "Preempted", now.Add(-time.Minute))),
		preemptionWorkload("old", condition("Evicted", "True", "Preempted", now.Add(-24*time.Hour))),
		preemptionWorkload("other-eviction", condition("Evicted", "True", "PodsReadyTimeout", now)),
		preemptionWorkload("cleared", condition("Evicted", "False", "Preempted", now)),
		preemptionWorkload("admitted", condition("Admitted", "True", "Admitted", now)),
		preemptionWorkload("no-status"),
	)
	got, err := PreemptedWorkloads(context.Background(), APIWorkloads{Client: c}, eligNS)
	if err != nil {
		t.Fatal(err)
	}
	byUID := map[types.UID]PreemptedWorkload{}
	for _, w := range got {
		byUID[w.UID] = w
	}
	if len(byUID) != 2 {
		t.Fatalf("got %d preempted Workloads %v, want the two evicted with reason Preempted", len(byUID), got)
	}
	p, ok := byUID["uid-preempted"]
	if !ok || !p.At.Equal(now.Add(-time.Minute)) {
		t.Errorf("preempted = %+v, want At %v", p, now.Add(-time.Minute))
	}
	if len(p.Owners) != 1 || p.Owners[0] != "pod-preempted" || len(p.OwnerRefs) != 1 || p.OwnerRefs[0].Kind != "Pod" {
		t.Errorf("owners = %v / %v, want the owning pod", p.Owners, p.OwnerRefs)
	}
	if o, ok := byUID["uid-old"]; !ok || !o.At.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("an old preemption is returned with its own time: %+v", o)
	}
}

func TestPreemptedWorkloadsErrors(t *testing.T) {
	failing := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		return errors.New("api down")
	}}
	if _, err := PreemptedWorkloads(context.Background(), APIWorkloads{Client: preemptionClient(t, failing, eligibilityMapper())}, eligNS); err == nil {
		t.Error("a failed Workload list returned no error: the caller would read it as no preemption")
	}
	unserved := meta.NewDefaultRESTMapper(nil)
	if _, err := PreemptedWorkloads(context.Background(), APIWorkloads{Client: preemptionClient(t, nil, unserved)}, eligNS); err == nil {
		t.Error("an unserved Workload kind returned no error")
	}
}

// intField reads a Kueue quantity field written as int64 (the API server's
// JSON decoding), int, or float64, and nothing else.
func TestIntField(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    interface{}
		want int
		ok   bool
	}{
		{"int64", int64(8), 8, true},
		{"int", 4, 4, true},
		{"float64", float64(2), 2, true},
		{"a string is malformed", "8", 0, false},
		{"a map is malformed", map[string]interface{}{"value": 8}, 0, false},
		{"nil", nil, 0, false},
		{"a non-int64 integer type is not read", int32(8), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := intField(map[string]interface{}{"f": tc.v}, "f")
			if got != tc.want || ok != tc.ok {
				t.Errorf("intField(%#v) = %d, %t; want %d, %t", tc.v, got, ok, tc.want, tc.ok)
			}
		})
	}
	if _, ok := intField(map[string]interface{}{}, "missing"); ok {
		t.Error("a missing field read as present")
	}
}
