package kueue

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// Review finding (scale): a ClusterQueue namespaceSelector cost one Namespace
// GET per namespace with a LocalQueue, every refresh.
var _ = Describe("Reader reads Namespace labels with one list per read", func() {
	const v = "v1beta2"
	cqWithSelector := func(name string) *unstructured.Unstructured {
		cq := clusterQueue(v, name, flavorQuotas("h100", quota("nvidia.com/gpu", "8")))
		return withSelector(cq, matchLabels("team", "a"))
	}
	objs := []client.Object{
		flavor(v, "h100", map[string]interface{}{"nvidia.com/gpu.product": h100Product}),
		cqWithSelector("cq"),
		namespace("ns-1", map[string]string{"team": "a"}),
		namespace("ns-2", map[string]string{"team": "a"}),
		namespace("ns-3", map[string]string{"team": "b"}),
		localQueue(v, "ns-1", "lq", "cq"),
		localQueue(v, "ns-2", "lq", "cq"),
		localQueue(v, "ns-3", "lq", "cq"),
		localQueue(v, "gone", "lq", "cq"),
	}
	type calls struct{ gets, nsLists int }
	build := func(c *calls, denyList bool) client.Client {
		return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(kueueMapper(v)).
			WithObjects(objs...).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
					c.gets++
					return cl.Get(ctx, key, o, opts...)
				},
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "NamespaceList" {
						c.nsLists++
						if denyList {
							return forbidden("namespaces")
						}
					}
					return cl.List(ctx, list, opts...)
				},
			}).Build()
	}
	want := map[string]config.ExternalCaps{
		"ns-1": typedCaps(map[string]int{h100Product: 8}),
		"ns-2": typedCaps(map[string]int{h100Product: 8}),
		"gone": {HasUntyped: true}, // a LocalQueue whose Namespace is gone: withheld
	}

	It("lists the Namespaces once and gets none", func() {
		c := &calls{}
		snap, err := NewReader(build(c, false), Options{}).Quotas(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace).To(Equal(want))
		Expect(c.gets).To(BeZero())
		Expect(c.nsLists).To(Equal(1))
	})

	It("falls back to one get per namespace where the list is Forbidden, and stops asking for the list", func() {
		c := &calls{}
		r := NewReader(build(c, true), Options{})
		snap, err := r.Quotas(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace).To(Equal(want))
		Expect(c.gets).To(Equal(4))
		Expect(c.nsLists).To(Equal(1))
		_, err = r.Quotas(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(c.nsLists).To(Equal(1), "a denied list is not retried every read")
	})
})
