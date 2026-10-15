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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: eligNS},
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

// workloadFor is a Kueue Workload owned by owner.
func workloadFor(owner *unstructured.Unstructured) *unstructured.Unstructured {
	w := obj("v1beta2", KindWorkload, eligNS, "wl-"+owner.GetName(), map[string]interface{}{})
	w.SetOwnerReferences([]metav1.OwnerReference{controllerRef(owner)})
	return w
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

	DescribeTable("the managed check",
		func(want Reason, objs ...client.Object) {
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(want))
		},
		Entry("a pod with Kueue's managed label", Reason(""),
			gpuPod("p", 8, nil, func(p *corev1.Pod) { p.Labels = map[string]string{ManagedLabel: "true"} })),
		Entry("a pod behind Kueue's admission gate", Reason(""),
			gpuPod("p", 8, nil, func(p *corev1.Pod) {
				p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: AdmissionGate}}
			})),
		Entry("a Job-owned pod: the Job carries the queue name and a Workload is owned by it", Reason(""),
			job, workloadFor(job), gpuPod("p", 8, job)),
		Entry("a Job-owned pod with a queue-name label and no Workload is not proof", ReasonNotKueueManaged,
			job, gpuPod("p", 8, job)),
		Entry("a LeaderWorkerSet pod: StatefulSet, then the LWS with the queue name and its Workload", Reason(""),
			lws, sts, workloadFor(lws), gpuPod("p", 8, sts)),
		Entry("a LeaderWorkerSet pod without a Workload", ReasonNotKueueManaged,
			lws, sts, gpuPod("p", 8, sts)),
		Entry("one unmanaged GPU pod hides GPUs from Kueue for the whole namespace", ReasonNotKueueManaged,
			gpuPod("a", 8, nil, func(p *corev1.Pod) { p.Labels = map[string]string{ManagedLabel: "true"} }),
			rs, gpuPod("b", 8, rs)),
		Entry("a pod without GPUs is not judged", Reason(""),
			gpuPod("a", 8, nil, func(p *corev1.Pod) { p.Labels = map[string]string{ManagedLabel: "true"} }),
			gpuPod("cpu", 0, rs)),
		Entry("no GPU pod on the first read: not managed", ReasonNotKueueManaged),
	)

	It("keeps the previous answer for a namespace with no GPU pod", func() {
		managed := gpuPod("p", 8, nil, func(p *corev1.Pod) { p.Labels = map[string]string{ManagedLabel: "true"} })
		c := eligibilityClient(nil, managed)
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
		managed := gpuPod("p", 8, nil, func(p *corev1.Pod) { p.Labels = map[string]string{ManagedLabel: "true"} })
		got := evaluate(eligibilityClient(nil, managed, resourceQuota(map[string]interface{}{"requests.nvidia.com/gpu": "16", "cpu": "100"})))
		Expect(got.Reason).To(BeEmpty())
		Expect(got.GPUQuota).To(Equal(map[string]int{"nvidia.com/gpu": 16}))
	})

	It("reads a Forbidden ResourceQuota list as unreadable, never as absent", func() {
		managed := gpuPod("p", 8, nil, func(p *corev1.Pod) { p.Labels = map[string]string{ManagedLabel: "true"} })
		funcs := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "ResourceQuotaList" {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "resourcequotas"}, "", errors.New("no RBAC"))
			}
			return cl.List(ctx, list, opts...)
		}}
		Expect(evaluate(eligibilityClient(funcs, managed)).Reason).To(Equal(ReasonResourceQuotaUnreadable))
	})
})
