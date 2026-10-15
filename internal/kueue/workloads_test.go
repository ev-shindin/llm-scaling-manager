package kueue

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// indexedSource is an APIWorkloads that claims an index, recording the owners
// it was asked for, as the informer source is used once it has synced.
type indexedSource struct {
	APIWorkloads
	mu     sync.Mutex
	owners []types.UID
}

func (s *indexedSource) Workloads(ctx context.Context, ns string, owner types.UID) ([]unstructured.Unstructured, error) {
	s.mu.Lock()
	s.owners = append(s.owners, owner)
	s.mu.Unlock()
	return s.APIWorkloads.Workloads(ctx, ns, owner)
}

func (*indexedSource) Indexed() bool { return true }

var _ = Describe("The managed check through an indexed Workload source", func() {
	lws := owned("leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet", "serve", "lws-uid", "lq", nil)
	sts := owned("apps/v1", "StatefulSet", "serve-0", "sts-uid", "", lws)
	rs := owned("apps/v1", "ReplicaSet", "web-abc", "rs-uid", "", nil)
	pods := func() []client.Object {
		return []client.Object{gpuPod("g0", 8, sts, labelled), gpuPod("g1", 8, sts, labelled)}
	}

	DescribeTable("answers as the namespace list does, looking up owners only",
		func(want Reason, objs []client.Object) {
			c := eligibilityClient(nil, objs...)
			src := &indexedSource{APIWorkloads: APIWorkloads{Client: c}}
			got := NewEligibility(c, Options{Workloads: src}).Evaluate(context.Background(), []string{eligNS})[eligNS]
			Expect(got.Reason).To(Equal(want))
			Expect(evaluate(c).Reason).To(Equal(want), "the unindexed path agrees")
			Expect(src.owners).NotTo(ContainElement(types.UID("")), "never a whole-namespace list")
		},
		Entry("a pod-integration pod with its reserved Workload", Reason(""), chargedPod("p")),
		Entry("two LWS groups covered by one Workload each", Reason(""),
			append(append([]client.Object{lws, sts}, pods()...),
				sizedWorkloadFor(lws, "wl-g0", "8", 1), sizedWorkloadFor(lws, "wl-g1", "8", 1))),
		Entry("two LWS groups, one group's Workload", ReasonNotKueueManaged,
			append(append([]client.Object{lws, sts}, pods()...), sizedWorkloadFor(lws, "wl-g0", "8", 1))),
		Entry("a ReplicaSet-owned Workload is not proof", ReasonNotKueueManaged,
			[]client.Object{rs, workloadFor(rs), gpuPod("b", 8, rs)}),
	)
})

// fakeWorkloadCache is a manager cache whose Workload informer sync is
// controlled by the test and whose reads come from an indexed fake client.
type fakeWorkloadCache struct {
	*informertest.FakeInformers
	reader  client.Client
	mu      sync.Mutex
	lists   int
	indexed bool
}

func (f *fakeWorkloadCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	f.mu.Lock()
	f.lists++
	f.mu.Unlock()
	return f.reader.List(ctx, list, opts...)
}

func (f *fakeWorkloadCache) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	f.indexed = field == WorkloadOwnerUIDField
	return nil
}

var _ = Describe("InformerWorkloads", func() {
	It("reads from the API server until the informer syncs, then from the cache by the owner index", func() {
		p := gpuPod("p", 8, nil, labelled)
		w := workloadFor(p)
		apiLists := 0
		api := eligibilityClient(&interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			apiLists++
			return cl.List(ctx, list, opts...)
		}}, w)
		idxObj := &unstructured.Unstructured{}
		idxObj.SetGroupVersionKind(w.GroupVersionKind())
		reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(eligibilityMapper()).
			WithObjects(w.DeepCopy()).WithIndex(idxObj, WorkloadOwnerUIDField, workloadOwnerUIDs).Build()
		fc := &fakeWorkloadCache{FakeInformers: &informertest.FakeInformers{Scheme: runtime.NewScheme()}, reader: reader}
		src := NewInformerWorkloads(fc, api)
		ctx := context.Background()

		got, err := src.Workloads(ctx, eligNS, p.UID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(apiLists).To(Equal(1), "not synced: the API server answers")
		Expect(src.Indexed()).To(BeFalse())

		inf, err := fc.FakeInformerFor(ctx, idxObj)
		Expect(err).NotTo(HaveOccurred())
		inf.Synced = true
		got, err = src.Workloads(ctx, eligNS, p.UID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(apiLists).To(Equal(1), "synced: no API call")
		Expect(fc.lists).To(Equal(1))
		Expect(fc.indexed).To(BeTrue())
		Expect(src.Indexed()).To(BeTrue())

		got, err = src.Workloads(ctx, eligNS, "someone-else")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("reports a Kueue that does not serve Workloads as the error it is", func() {
		api := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(kueueMapper("v1beta2")).Build()
		src := NewInformerWorkloads(&fakeWorkloadCache{FakeInformers: &informertest.FakeInformers{}}, api)
		_, err := src.Workloads(context.Background(), eligNS, "")
		Expect(workloadsAbsent(err)).To(BeTrue())
	})
})
