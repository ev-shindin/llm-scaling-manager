package kueue

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// kueueController is the Kueue controller Deployment in ns, running image.
func kueueController(ns, image string) *unstructured.Unstructured {
	d := &unstructured.Unstructured{}
	d.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	d.SetNamespace(ns)
	d.SetName("kueue-controller-manager")
	Expect(unstructured.SetNestedSlice(d.Object, []interface{}{
		map[string]interface{}{"name": "manager", "image": image},
		map[string]interface{}{"name": "kube-rbac-proxy", "image": "quay.io/brancz/kube-rbac-proxy:v0.18.0"},
	}, "spec", "template", "spec", "containers")).To(Succeed())
	Expect(unstructured.SetNestedField(d.Object, int64(1), "status", "readyReplicas")).To(Succeed())
	return d
}

// versionClient serves the Kueue kinds and Deployments.
func versionClient(funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	m := cohortMapper("v1beta2").(*meta.DefaultRESTMapper)
	m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	b := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(m).WithObjects(objs...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

var _ = Describe("Reader checks the Kueue controller version", func() {
	const v = "v1beta2"
	reasonsOf := func(c client.Client, opts Options) []string {
		snap, err := NewReader(c, opts).Quotas(context.Background())
		Expect(err).NotTo(HaveOccurred())
		out := make([]string, 0, 3)
		for _, ns := range []string{"team-a", "team-b"} {
			Expect(snap.Borrow[ns]).To(HaveLen(1))
			out = append(out, snap.Borrow[ns][0].Reason)
		}
		Expect(snap.SharedPools).To(HaveLen(1))
		return append(out, snap.SharedPools[0].Reason)
	}
	counted := []string{"nominal-lent-out", "nominal-lent-out", ""}
	untested := []string{"kueue-version-untested", "kueue-version-untested", "kueue-version-untested"}

	It("counts borrowing under a tested release", func() {
		for _, image := range []string{
			"registry.k8s.io/kueue/kueue:v0.20.0",
			"registry.k8s.io/kueue/kueue:v0.19.5@sha256:0123",
		} {
			objs := append(docsExample(v, "cohortName"), kueueController("kueue-system", image))
			Expect(reasonsOf(versionClient(nil, objs...), Options{})).To(Equal(counted), image)
		}
	})

	It("withholds borrowing under a release outside the tested range, and keeps the nominal figures", func() {
		objs := append(docsExample(v, "cohortName"), kueueController("kueue-system", "registry.k8s.io/kueue/kueue:v0.21.0"))
		c := versionClient(nil, objs...)
		Expect(reasonsOf(c, Options{})).To(Equal(untested))
		snap, err := NewReader(c, Options{}).Quotas(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace).To(HaveKeyWithValue("team-a", typedCaps(map[string]int{h100Product: 9})))
	})

	It("reads the controller in the configured namespace", func() {
		objs := append(docsExample(v, "cohortName"), kueueController("queueing", "registry.k8s.io/kueue/kueue:v0.18.2"))
		Expect(reasonsOf(versionClient(nil, objs...), Options{ControllerNamespace: "queueing"})).To(Equal(untested))
		Expect(reasonsOf(versionClient(nil, objs...), Options{})).To(Equal(counted), "not found in kueue-system: unknown")
	})

	It("treats an unreadable or untagged controller as unknown, which does not withhold", func() {
		digestOnly := append(docsExample(v, "cohortName"), kueueController("kueue-system", "registry.k8s.io/kueue/kueue@sha256:0123"))
		Expect(reasonsOf(versionClient(nil, digestOnly...), Options{})).To(Equal(counted))

		denied := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if o.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
				return forbidden("deployments")
			}
			return c.Get(ctx, key, o, opts...)
		}}
		objs := append(docsExample(v, "cohortName"), kueueController("kueue-system", "registry.k8s.io/kueue/kueue:v0.21.0"))
		Expect(reasonsOf(versionClient(denied, objs...), Options{})).To(Equal(counted))
	})

	It("reports the controller down when it has no ready replica, or is gone after it was seen; the rows are unchanged", func() {
		down := func(r *Reader) bool {
			snap, err := r.Quotas(context.Background())
			Expect(err).NotTo(HaveOccurred())
			return snap.KueueControllerDown
		}
		ctrl := kueueController("kueue-system", "registry.k8s.io/kueue/kueue:v0.20.0")
		c := versionClient(nil, append(docsExample(v, "cohortName"), ctrl)...)
		r := NewReader(c, Options{})
		Expect(down(r)).To(BeFalse(), "one ready replica")

		Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ctrl), ctrl)).To(Succeed())
		Expect(unstructured.SetNestedField(ctrl.Object, int64(0), "status", "readyReplicas")).To(Succeed())
		Expect(c.Status().Update(context.Background(), ctrl)).To(Succeed())
		Expect(down(r)).To(BeTrue(), "scaled to 0: no ready replica")
		Expect(reasonsOf(c, Options{})).To(Equal(counted), "the rows keep their reasons; the engine side withholds")

		Expect(unstructured.SetNestedField(ctrl.Object, int64(1), "status", "readyReplicas")).To(Succeed())
		Expect(c.Status().Update(context.Background(), ctrl)).To(Succeed())
		Expect(down(r)).To(BeFalse(), "ready again, read on the next refresh")

		Expect(c.Delete(context.Background(), ctrl)).To(Succeed())
		Expect(down(r)).To(BeTrue(), "gone while the Kueue CRDs are served")
	})

	It("does not report the controller down when it cannot read it, or never found it", func() {
		down := func(c client.Client) bool {
			snap, err := NewReader(c, Options{}).Quotas(context.Background())
			Expect(err).NotTo(HaveOccurred())
			return snap.KueueControllerDown
		}
		denied := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if o.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
				return forbidden("deployments")
			}
			return c.Get(ctx, key, o, opts...)
		}}
		stopped := kueueController("kueue-system", "registry.k8s.io/kueue/kueue:v0.20.0")
		Expect(unstructured.SetNestedField(stopped.Object, int64(0), "status", "readyReplicas")).To(Succeed())
		Expect(down(versionClient(denied, append(docsExample(v, "cohortName"), stopped)...))).To(BeFalse(), "Forbidden is unknown")
		Expect(down(versionClient(nil, docsExample(v, "cohortName")...))).To(BeFalse(),
			"never found (Kueue in another namespace without controllerNamespace) is unknown")
	})
})

var _ = Describe("ParseControllerVersion and VersionTested", func() {
	DescribeTable("parses an image reference",
		func(image, want string, ok, tested bool) {
			got, gotOK := ParseControllerVersion(image)
			Expect(gotOK).To(Equal(ok))
			Expect(got).To(Equal(want))
			Expect(VersionTested(got)).To(Equal(tested))
		},
		Entry("release", "registry.k8s.io/kueue/kueue:v0.20.0", "v0.20.0", true, true),
		Entry("no v", "kueue:0.19.3", "v0.19.3", true, true),
		Entry("tag and digest", "r.io/kueue:v0.20.1@sha256:ab", "v0.20.1", true, true),
		Entry("pre-release of an untested minor", "r.io/kueue:v0.21.0-rc.1", "v0.21.0-rc.1", true, false),
		Entry("older minor", "r.io/kueue:v0.18.9", "v0.18.9", true, false),
		Entry("v1", "r.io/kueue:v1.0.0", "v1.0.0", true, false),
		Entry("registry port, no tag", "localhost:5000/kueue", "", false, false),
		Entry("digest only", "r.io/kueue@sha256:ab", "", false, false),
		Entry("floating tag", "r.io/kueue:main", "", false, false),
	)
})

var _ = Describe("BorrowDrift", func() {
	It("is the absolute difference between usage past nominal and Kueue's borrowed figure, per queue in a cohort", func() {
		t := topology(
			member("agree", QueueResource{Nominal: 4, Reserved: 6, Borrowed: 2}),
			member("over", QueueResource{Nominal: 4, Reserved: 6, Borrowed: 5}),
			member("under", QueueResource{Nominal: 4, Reserved: 3, Borrowed: 1}),
			QueueState{Name: "alone", HasStatus: true, Resources: map[ResourceKey]QueueResource{gpuKey: {Nominal: 1, Borrowed: 3}}},
		)
		Expect(BorrowDrift(t)).To(Equal(map[string]int{"agree": 0, "over": 3, "under": 1}))
	})
})
