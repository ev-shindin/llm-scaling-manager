package kueue

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const eligNS = "team-a"

// resourceQuotaListKind is the list kind the ResourceQuota read uses.
const resourceQuotaListKind = "ResourceQuotaList"

func eligibilityMapper() meta.RESTMapper {
	m := kueueMapper("v1beta2").(*meta.DefaultRESTMapper)
	m.Add(schema.GroupVersionKind{Group: Group, Version: "v1beta2", Kind: KindWorkload}, meta.RESTScopeNamespace)
	for _, gvk := range []schema.GroupVersionKind{
		{Version: "v1", Kind: "Pod"},
		{Version: "v1", Kind: "ResourceQuota"},
		{Group: "batch", Version: "v1", Kind: "Job"},
		{Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
		{Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSet"},
	} {
		m.Add(gvk, meta.RESTScopeNamespace)
	}
	return m
}

func eligibilityClient(funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	b := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(eligibilityMapper()).WithObjects(objs...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

// gpuPod is a pod in eligNS requesting n GPUs, owned by owner when set.
func gpuPod(name string, n int64, owner *unstructured.Unstructured, mutate ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: eligNS, UID: types.UID("pod-" + name)},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(n, resource.DecimalSI)},
			Limits:   corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(n, resource.DecimalSI)},
		}}}},
	}
	if owner != nil {
		p.OwnerReferences = []metav1.OwnerReference{controllerRef(owner)}
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func controllerRef(owner *unstructured.Unstructured) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: owner.GetAPIVersion(), Kind: owner.GetKind(), Name: owner.GetName(), UID: owner.GetUID(), Controller: &yes}
}

// owned is an object of apiVersion/kind in eligNS, with the queue-name label
// when queue is set, controlled by parent when set.
func owned(apiVersion, kind, name, uid, queue string, parent *unstructured.Unstructured) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(eligNS)
	u.SetName(name)
	u.SetUID(types.UID(uid))
	if queue != "" {
		u.SetLabels(map[string]string{QueueNameLabel: queue})
	}
	if parent != nil {
		u.SetOwnerReferences([]metav1.OwnerReference{controllerRef(parent)})
	}
	return u
}

// pendingWorkloadFor is a Kueue Workload owned by owner, with no quota
// reserved yet.
func pendingWorkloadFor(owner client.Object) *unstructured.Unstructured {
	w := obj("v1beta2", KindWorkload, eligNS, "wl-"+owner.GetName(), map[string]interface{}{})
	yes := true
	gvk := owner.GetObjectKind().GroupVersionKind()
	apiVersion, kind := gvk.GroupVersion().String(), gvk.Kind
	if _, isPod := owner.(*corev1.Pod); isPod {
		apiVersion, kind = "v1", "Pod"
	}
	w.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: owner.GetName(), UID: owner.GetUID(), Controller: &yes}})
	w.Object["spec"] = podSetsRequesting(map[string]interface{}{"nvidia.com/gpu": "8"})
	return w
}

// podSetsRequesting is a Workload spec with one pod set whose container
// requests the given resources.
func podSetsRequesting(requests map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"podSets": []interface{}{map[string]interface{}{
		"name": "main", "count": int64(1),
		"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{
			map[string]interface{}{"name": "c", "resources": map[string]interface{}{"requests": requests}},
		}}},
	}}}
}

// cpuOnly makes a Workload request no GPU, as a tenant's own would.
func cpuOnly(w *unstructured.Unstructured) *unstructured.Unstructured {
	w.Object["spec"] = podSetsRequesting(map[string]interface{}{"cpu": "100m"})
	return w
}

// workloadFor is a Kueue Workload owned by owner with quota reserved, as Kueue
// writes it: status.admission and condition QuotaReserved.
func workloadFor(owner client.Object) *unstructured.Unstructured {
	w := pendingWorkloadFor(owner)
	w.Object["status"] = map[string]interface{}{
		"admission":  map[string]interface{}{"clusterQueue": "cq"},
		"conditions": []interface{}{map[string]interface{}{"type": "QuotaReserved", "status": "True"}},
	}
	return w
}

// admittedOnlyWorkloadFor carries only condition Admitted, no status.admission.
func admittedOnlyWorkloadFor(owner client.Object) *unstructured.Unstructured {
	w := pendingWorkloadFor(owner)
	w.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "True"}},
	}
	return w
}

// labelled sets Kueue's managed label on a pod, as Kueue's webhook would and
// as a tenant can.
func labelled(p *corev1.Pod) { p.Labels = map[string]string{ManagedLabel: "true"} }

// gated puts a pod behind Kueue's admission gate.
func gated(p *corev1.Pod) { p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: AdmissionGate}} }

// chargedPod is a labelled GPU pod and the reserved Workload Kueue's pod
// integration owns it by.
func chargedPod(name string) []client.Object {
	p := gpuPod(name, 8, nil, labelled)
	return []client.Object{p, workloadFor(p)}
}

func resourceQuota(hard map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"hard": hard}}}
	u.SetAPIVersion("v1")
	u.SetKind("ResourceQuota")
	u.SetNamespace(eligNS)
	u.SetName("gpu")
	return u
}

func evaluate(c client.Client) NamespaceEligibility {
	return NewEligibility(c, Options{}).Evaluate(context.Background(), []string{eligNS})[eligNS]
}

var _ = Describe("Eligibility: the engine-side borrowing conditions", func() {
	job := owned("batch/v1", "Job", "train", "job-uid", "lq", nil)
	lws := owned("leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet", "serve", "lws-uid", "lq", nil)
	sts := owned("apps/v1", "StatefulSet", "serve-0", "sts-uid", "", lws)
	rs := owned("apps/v1", "ReplicaSet", "web-abc", "rs-uid", "", nil)

	// The LeaderWorkerSet worker chain: worker pod, worker StatefulSet (owned
	// by the leader pod), leader pod, leader StatefulSet, LeaderWorkerSet.
	leader := owned("v1", "Pod", "serve-0", "leader-uid", "", sts)
	workerSts := owned("apps/v1", "StatefulSet", "serve-0-workers", "wsts-uid", "", leader)

	labelledPod := gpuPod("p", 8, nil, labelled)
	gatedPod := gpuPod("p", 8, nil, labelled, gated)
	DescribeTable("the managed check",
		func(want Reason, objs []client.Object) {
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(want))
		},
		Entry("a labelled pod with a reserved Workload of its own", Reason(""), chargedPod("p")),
		Entry("a labelled pod whose Workload carries only Admitted", Reason(""),
			[]client.Object{labelledPod, admittedOnlyWorkloadFor(labelledPod)}),
		Entry("Kueue's managed label without a Workload is not proof: a tenant can write it", ReasonNotKueueManaged,
			[]client.Object{labelledPod}),
		Entry("a running labelled pod whose Workload has no quota reserved is not charged", ReasonNotKueueManaged,
			[]client.Object{labelledPod, pendingWorkloadFor(labelledPod)}),
		Entry("a pod behind the admission gate with its Workload, reserved or not", Reason(""),
			[]client.Object{gatedPod, pendingWorkloadFor(gatedPod)}),
		Entry("the admission gate without a Workload is not proof", ReasonNotKueueManaged,
			[]client.Object{gatedPod}),
		Entry("a Job-owned pod: a reserved Workload is owned by the Job", Reason(""),
			[]client.Object{job, workloadFor(job), gpuPod("p", 8, job)}),
		Entry("a Job-owned pod with a queue-name label and no Workload is not proof", ReasonNotKueueManaged,
			[]client.Object{job, gpuPod("p", 8, job)}),
		Entry("a LeaderWorkerSet leader pod: StatefulSet, then the LWS and its reserved Workload", Reason(""),
			[]client.Object{lws, sts, workloadFor(lws), gpuPod("p", 8, sts, labelled)}),
		Entry("a LeaderWorkerSet worker pod: four owners up to the LWS", Reason(""),
			[]client.Object{lws, sts, leader, workerSts, workloadFor(lws), gpuPod("w", 8, workerSts, labelled)}),
		Entry("a labelled LeaderWorkerSet pod without a Workload", ReasonNotKueueManaged,
			[]client.Object{lws, sts, gpuPod("p", 8, sts, labelled)}),
		Entry("one unmanaged GPU pod hides GPUs from Kueue for the whole namespace", ReasonNotKueueManaged,
			append(chargedPod("a"), rs, gpuPod("b", 8, rs))),
		// Review finding (security): a tenant allowed to create Workloads
		// makes a tiny one owned by its own Deployment's ReplicaSet, or by the
		// pod, and Kueue admits it.
		Entry("a reserved Workload owned by the pod's ReplicaSet is not proof: no Kueue integration owns one by it", ReasonNotKueueManaged,
			[]client.Object{rs, workloadFor(rs), gpuPod("b", 8, rs)}),
		Entry("a reserved CPU-only Workload owned by the pod is not proof", ReasonNotKueueManaged,
			[]client.Object{labelledPod, cpuOnly(workloadFor(labelledPod))}),
		Entry("a reserved CPU-only Workload owned by the Job is not proof", ReasonNotKueueManaged,
			[]client.Object{job, cpuOnly(workloadFor(job)), gpuPod("p", 8, job)}),
		Entry("a pod without GPUs is not judged", Reason(""),
			append(chargedPod("a"), gpuPod("cpu", 0, rs))),
		Entry("no GPU pod on the first read: not managed", ReasonNotKueueManaged, []client.Object(nil)),
	)

	It("keeps the previous answer when the Workloads cannot be listed for a reason that may pass", func() {
		fail := false
		funcs := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == KindWorkload+"List" && fail {
				return apierrors.NewServiceUnavailable("blip")
			}
			return cl.List(ctx, list, opts...)
		}}
		c := eligibilityClient(funcs, chargedPod("p")...)
		now := time.Unix(0, 0)
		e := NewEligibility(c, Options{RefreshInterval: time.Minute, now: func() time.Time { return now }})
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(BeEmpty())
		fail = true
		now = now.Add(2 * time.Minute)
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(BeEmpty())
	})

	It("reads a Forbidden Workload list as not managed", func() {
		funcs := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == KindWorkload+"List" {
				return apierrors.NewForbidden(schema.GroupResource{Group: Group, Resource: "workloads"}, "", errors.New("no RBAC"))
			}
			return cl.List(ctx, list, opts...)
		}}
		Expect(evaluate(eligibilityClient(funcs, chargedPod("p")...)).Reason).To(Equal(ReasonNotKueueManaged))
	})

	It("keeps the previous answer for a namespace with no GPU pod", func() {
		objs := chargedPod("p")
		managed := objs[0]
		c := eligibilityClient(nil, objs...)
		now := time.Unix(0, 0)
		e := NewEligibility(c, Options{RefreshInterval: time.Minute, now: func() time.Time { return now }})
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(BeEmpty())
		Expect(c.Delete(context.Background(), managed)).To(Succeed())
		now = now.Add(2 * time.Minute)
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(BeEmpty(), "scaled to zero is not evidence against")
	})

	It("re-evaluates a namespace at most once per refresh interval", func() {
		lists := 0
		funcs := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				lists++
			}
			return cl.List(ctx, list, opts...)
		}}
		c := eligibilityClient(funcs)
		now := time.Unix(0, 0)
		e := NewEligibility(c, Options{RefreshInterval: time.Minute, now: func() time.Time { return now }})
		for range 5 {
			e.Evaluate(context.Background(), []string{eligNS})
		}
		Expect(lists).To(Equal(1))
	})

	It("reads the tightest GPU hard limit of the namespace's ResourceQuotas", func() {
		got := evaluate(eligibilityClient(nil, append(chargedPod("p"), resourceQuota(map[string]interface{}{"requests.nvidia.com/gpu": "16", "cpu": "100"}))...))
		Expect(got.Reason).To(BeEmpty())
		Expect(got.GPUQuota).To(Equal(map[string]int{"nvidia.com/gpu": 16}))
	})

	It("reads a Forbidden ResourceQuota list as unreadable, never as absent", func() {
		funcs := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == resourceQuotaListKind {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "resourcequotas"}, "", errors.New("no RBAC"))
			}
			return cl.List(ctx, list, opts...)
		}}
		Expect(evaluate(eligibilityClient(funcs, chargedPod("p")...)).Reason).To(Equal(ReasonResourceQuotaUnreadable))
	})

	// A transient error is not "unreadable" for a whole refresh interval: it
	// reads stale, which freezes the entry, keeps the last figures, and is asked
	// again on the next call.
	It("reads a transient ResourceQuota list error as stale and asks again next call", func() {
		fail, rqLists := false, 0
		funcs := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == resourceQuotaListKind {
				rqLists++
				if fail {
					return apierrors.NewServiceUnavailable("blip")
				}
			}
			return cl.List(ctx, list, opts...)
		}}
		c := eligibilityClient(funcs, append(chargedPod("p"), resourceQuota(map[string]interface{}{"requests.nvidia.com/gpu": "16"}))...)
		now := time.Unix(0, 0)
		e := NewEligibility(c, Options{RefreshInterval: time.Minute, now: func() time.Time { return now }})
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(BeEmpty())

		fail = true
		now = now.Add(2 * time.Minute)
		got := e.Evaluate(context.Background(), []string{eligNS})[eligNS]
		Expect(got.Reason).To(Equal(ReasonStale))
		Expect(got.GPUQuota).To(Equal(map[string]int{"nvidia.com/gpu": 16}), "the last figures stand in")

		fail = false
		now = now.Add(time.Second)
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(BeEmpty(), "not kept for the interval")
		Expect(rqLists).To(Equal(3))
	})

	// A freeze is for a blip. A tenant whose ResourceQuota reads keep failing
	// is withheld as unreadable after unreadableAfter intervals, and recovers
	// on the first read that passes.
	It("turns a ResourceQuota read that keeps failing into unreadable after five intervals", func() {
		fail := false
		funcs := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == resourceQuotaListKind && fail {
				return apierrors.NewInternalError(errors.New("etcd timeout"))
			}
			return cl.List(ctx, list, opts...)
		}}
		c := eligibilityClient(funcs, append(chargedPod("p"), resourceQuota(map[string]interface{}{"requests.nvidia.com/gpu": "16"}))...)
		now := time.Unix(0, 0)
		e := NewEligibility(c, Options{RefreshInterval: time.Minute, now: func() time.Time { return now }})
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(BeEmpty())

		fail = true
		now = now.Add(2 * time.Minute)
		for i := 0; i < 5; i++ {
			got := e.Evaluate(context.Background(), []string{eligNS})[eligNS]
			Expect(got.Reason).To(Equal(ReasonStale), "still a blip after %d minutes", i)
			now = now.Add(time.Minute - time.Second)
		}
		now = now.Add(time.Minute)
		got := e.Evaluate(context.Background(), []string{eligNS})[eligNS]
		Expect(got.Reason).To(Equal(ReasonResourceQuotaUnreadable), "failing for five intervals")
		Expect(got.GPUQuota).To(Equal(map[string]int{"nvidia.com/gpu": 16}))

		fail = false
		now = now.Add(time.Second)
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(BeEmpty(), "recovers on the first passing read")
		fail = true
		now = now.Add(2 * time.Minute)
		Expect(e.Evaluate(context.Background(), []string{eligNS})[eligNS].Reason).To(Equal(ReasonStale),
			"a new failure starts a new count")
	})

	It("forgets a namespace no longer asked about", func() {
		c := eligibilityClient(nil, chargedPod("p")...)
		now := time.Unix(0, 0)
		e := NewEligibility(c, Options{RefreshInterval: time.Minute, now: func() time.Time { return now }})
		e.Evaluate(context.Background(), []string{eligNS, "gone"})
		now = now.Add(2 * time.Hour)
		e.Evaluate(context.Background(), []string{eligNS})
		Expect(e.byNS).To(HaveKey(eligNS))
		Expect(e.byNS).NotTo(HaveKey("gone"))
	})
})
