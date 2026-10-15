package kueue

import (
	"context"
	"errors"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

const h100Product = "NVIDIA-H100-80GB-HBM3"

// cohortMapper is kueueMapper that also serves the Cohort kind.
func cohortMapper(version string) meta.RESTMapper {
	m := kueueMapper(version).(*meta.DefaultRESTMapper)
	m.Add(schema.GroupVersionKind{Group: Group, Version: version, Kind: KindCohort}, meta.RESTScopeRoot)
	return m
}

// borrowingQueue is a ClusterQueue in cohort c, with one h100 GPU line whose
// extra fields (lendingLimit, borrowingLimit) are merged into the quota line,
// and a written reservation of reserved GPUs. cohortField is "cohortName"
// (v1beta2) or "cohort" (v1beta1).
func borrowingQueue(version, name, cohortField, nominal string, extra map[string]interface{}, reserved string) *unstructured.Unstructured {
	line := quota("nvidia.com/gpu", nominal)
	for k, v := range extra {
		line[k] = v
	}
	cq := clusterQueue(version, name, flavorQuotas("h100", line))
	Expect(unstructured.SetNestedField(cq.Object, "c", "spec", cohortField)).To(Succeed())
	cq.Object["status"] = map[string]interface{}{
		"pendingWorkloads": int64(0),
		"flavorsReservation": []interface{}{map[string]interface{}{
			"name": "h100",
			"resources": []interface{}{map[string]interface{}{
				"name": "nvidia.com/gpu", "total": reserved, "borrowed": "0",
			}},
		}},
	}
	return cq
}

// docsExample is Kueue's documented cohort example: team-a nominal 9 with
// borrowingLimit 1 beside an idle team-b nominal 12, both in cohort c.
func docsExample(version, cohortField string, extra ...client.Object) []client.Object {
	objs := []client.Object{
		flavor(version, "h100", map[string]interface{}{"nvidia.com/gpu.product": h100Product}),
		borrowingQueue(version, "team-a-cq", cohortField, "9", map[string]interface{}{"borrowingLimit": "1"}, "0"),
		borrowingQueue(version, "team-b-cq", cohortField, "12", nil, "0"),
		localQueue(version, "team-a", "lq", "team-a-cq"),
		localQueue(version, "team-b", "lq", "team-b-cq"),
	}
	return append(objs, extra...)
}

func cohortObj(version, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return obj(version, KindCohort, "", name, spec)
}

func readBorrow(c client.Client) config.ExternalQuotas {
	snap, err := NewReader(c, Options{}).Quotas(context.Background())
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return snap
}

func newCohortClient(version string, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithRESTMapper(cohortMapper(version)).
		WithObjects(objs...).
		Build()
}

var _ = Describe("Reader borrowing figures", func() {
	teamA := config.ExternalBorrow{
		Queue: "team-a-cq", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
		Accelerator: h100Product, Nominal: 9, Guaranteed: 0, Borrowable: 10,
		BorrowCap: 1, WithinNominal: 9,
		Reason: string(ReasonNominalLentOut),
	}

	for _, tc := range []struct{ version, field string }{
		{"v1beta2", "cohortName"},
		{"v1beta1", "cohort"},
	} {
		It("reads the cohort from spec."+tc.field+" under "+tc.version+" and logs Kueue's example reach of 10", func() {
			snap := readBorrow(newCohortClient(tc.version, docsExample(tc.version, tc.field)...))
			Expect(snap.Borrow).To(HaveKeyWithValue("team-a", []config.ExternalBorrow{teamA}))
			Expect(snap.Borrow["team-b"]).To(HaveLen(1))
			Expect(snap.Borrow["team-b"][0].Borrowable).To(Equal(21), "idle under Never: nothing guaranteed, the whole pool reachable")
			// The nominal figures are exactly what they were.
			Expect(snap.Namespace).To(Equal(map[string]config.ExternalCaps{
				"team-a": typedCaps(map[string]int{h100Product: 9}),
				"team-b": typedCaps(map[string]int{h100Product: 12}),
			}))
			Expect(snap.Cluster).To(Equal(typedCaps(map[string]int{h100Product: 21})))
		})
	}

	It("publishes each namespace's grant at the guaranteed and within-nominal figures, and the cohort's free pool", func() {
		const v = "v1beta2"
		snap := readBorrow(newCohortClient(v, docsExample(v, "cohortName")...))
		// Both queues are Never and idle: nothing guaranteed, each could take its
		// whole nominal from the free pool without borrowing.
		Expect(snap.NamespaceGuaranteed).To(Equal(map[string]config.ExternalCaps{
			"team-a": typedCaps(map[string]int{h100Product: 0}),
			"team-b": typedCaps(map[string]int{h100Product: 0}),
		}))
		Expect(snap.NamespaceWithinNominal).To(Equal(map[string]config.ExternalCaps{
			"team-a": typedCaps(map[string]int{h100Product: 9}),
			"team-b": typedCaps(map[string]int{h100Product: 12}),
		}))
		Expect(snap.SharedPools).To(Equal([]config.ExternalSharedPool{{
			Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Accelerator: h100Product,
			NodeLabels: map[string]string{"nvidia.com/gpu.product": h100Product}, Free: 21,
			Lenders: []config.ExternalLender{{Queue: "team-a-cq", LentOut: 9}, {Queue: "team-b-cq", LentOut: 12}},
		}}))
	})

	It("falls back to nominal for both figures where borrowing is withheld", func() {
		const v = "v1beta2"
		objs := docsExample(v, "cohortName")
		delete(objs[2].(*unstructured.Unstructured).Object, "status")
		snap := readBorrow(newCohortClient(v, objs...))
		Expect(snap.NamespaceGuaranteed).To(Equal(snap.Namespace))
		Expect(snap.NamespaceWithinNominal).To(Equal(snap.Namespace))
		Expect(snap.SharedPools[0].Free).To(Equal(0))
		Expect(snap.SharedPools[0].Reason).To(Equal(string(ReasonNoStatus)))
	})

	It("withholds a nested cohort: an implicit root a Cohort names as its parent", func() {
		const v = "v1beta2"
		snap := readBorrow(newCohortClient(v, docsExample(v, "cohortName",
			cohortObj(v, "child", map[string]interface{}{"parentName": "c"}))...))
		Expect(snap.Borrow["team-a"][0].Reason).To(Equal(string(ReasonHierarchicalCohort)))
		Expect(snap.Borrow["team-a"][0].Borrowable).To(Equal(0))
		Expect(snap.Borrow["team-a"][0].Guaranteed).To(Equal(9))
	})

	It("counts a Cohort object with neither parent nor quota as flat", func() {
		const v = "v1beta2"
		snap := readBorrow(newCohortClient(v, docsExample(v, "cohortName",
			cohortObj(v, "c", map[string]interface{}{}))...))
		Expect(snap.Borrow["team-a"]).To(Equal([]config.ExternalBorrow{teamA}))
	})

	It("reads hierarchy-unknown, never flat, when the Cohort kind is not served", func() {
		const v = "v1beta2"
		snap := readBorrow(newFakeClient(v, docsExample(v, "cohortName")...))
		Expect(snap.Borrow["team-a"][0].Reason).To(Equal(string(ReasonHierarchyUnknown)))
	})

	It("reads hierarchy-unknown on a Forbidden Cohort list, and the read still succeeds", func() {
		const v = "v1beta2"
		c := fake.NewClientBuilder().
			WithScheme(runtime.NewScheme()).
			WithRESTMapper(cohortMapper(v)).
			WithObjects(docsExample(v, "cohortName")...).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == KindCohort+"List" {
						return apierrors.NewForbidden(schema.GroupResource{Group: Group, Resource: "cohorts"}, "", errors.New("no RBAC"))
					}
					return cl.List(ctx, list, opts...)
				},
			}).
			Build()
		snap := readBorrow(c)
		Expect(snap.Borrow["team-a"][0].Reason).To(Equal(string(ReasonHierarchyUnknown)))
		Expect(snap.Namespace["team-a"]).To(Equal(typedCaps(map[string]int{h100Product: 9})))
		Expect(snap.BorrowStale).To(BeFalse(), "Forbidden lasts until someone grants the read")
	})

	// A failed Cohort list that may pass by the next read must freeze, not
	// read as a fresh snapshot that withholds everything (hierarchy-unknown)
	// and drops every counted namespace to nominal.
	It("marks the borrowing figures stale on a transient Cohort list failure, and the read still succeeds", func() {
		const v = "v1beta2"
		c := fake.NewClientBuilder().
			WithScheme(runtime.NewScheme()).
			WithRESTMapper(cohortMapper(v)).
			WithObjects(docsExample(v, "cohortName")...).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == KindCohort+"List" {
						return apierrors.NewServiceUnavailable("etcd blip")
					}
					return cl.List(ctx, list, opts...)
				},
			}).
			Build()
		snap := readBorrow(c)
		Expect(snap.BorrowStale).To(BeTrue())
		Expect(snap.Borrow["team-a"][0].Reason).To(Equal(string(ReasonStale)))
		Expect(snap.SharedPools[0].Free).To(Equal(0))
		Expect(snap.Namespace["team-a"]).To(Equal(typedCaps(map[string]int{h100Product: 9})), "the nominal figures do not depend on it")
	})

	It("keeps an unserved Cohort kind out of the stale path", func() {
		const v = "v1beta2"
		snap := readBorrow(newFakeClient(v, docsExample(v, "cohortName")...))
		Expect(snap.BorrowStale).To(BeFalse())
	})

	It("names a cohort member without status in the log, once", func() {
		const v = "v1beta2"
		objs := docsExample(v, "cohortName")
		delete(objs[2].(*unstructured.Unstructured).Object, "status")
		var lines []string
		ctx := logr.NewContext(context.Background(), funcr.NewJSON(func(obj string) { lines = append(lines, obj) }, funcr.Options{}))
		r := NewReader(newCohortClient(v, objs...), Options{})
		for range 2 {
			_, err := r.Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
		n := 0
		for _, l := range lines {
			if strings.Contains(l, "no status.flavorsReservation") {
				n++
				Expect(l).To(ContainSubstring(`"clusterQueue":"team-b-cq"`))
			}
		}
		Expect(n).To(Equal(1))
	})

	It("reads no-status when Kueue never wrote a queue's status", func() {
		const v = "v1beta2"
		objs := docsExample(v, "cohortName")
		delete(objs[2].(*unstructured.Unstructured).Object, "status")
		snap := readBorrow(newCohortClient(v, objs...))
		Expect(snap.Borrow["team-a"][0].Reason).To(Equal(string(ReasonNoStatus)))
	})

	It("does not fail the read on a malformed lendingLimit: borrowing is withheld, nominal unchanged", func() {
		const v = "v1beta2"
		objs := docsExample(v, "cohortName")
		objs[2] = borrowingQueue(v, "team-b-cq", "cohortName", "12", map[string]interface{}{"lendingLimit": "lots"}, "0")
		snap := readBorrow(newCohortClient(v, objs...))
		Expect(snap.Borrow["team-a"][0].Reason).To(Equal(string(ReasonMalformed)))
		Expect(snap.Namespace["team-b"]).To(Equal(typedCaps(map[string]int{h100Product: 12})))
	})

	It("parses every field borrowing depends on from one ClusterQueue", func() {
		const v = "v1beta2"
		cq := borrowingQueue(v, "cq", "cohortName", "8",
			map[string]interface{}{"lendingLimit": "2", "borrowingLimit": "4"}, "6")
		Expect(unstructured.SetNestedField(cq.Object, "LowerPriority", "spec", "preemption", "reclaimWithinCohort")).To(Succeed())
		status := cq.Object["status"].(map[string]interface{})
		status["pendingWorkloads"] = int64(3)
		status["fairSharing"] = map[string]interface{}{"weightedShare": int64(0)}
		status["effectiveQuotas"] = []interface{}{}
		res := status["flavorsReservation"].([]interface{})[0].(map[string]interface{})["resources"].([]interface{})[0].(map[string]interface{})
		res["borrowed"] = "1"

		r := NewReader(nil, Options{})
		q := r.queueState(cq, map[string]string{"h100": h100Product})
		Expect(q).To(Equal(QueueState{
			Name: "cq", Cohort: "c", Reclaim: ReclaimLowerPriority, Pending: 3,
			HasStatus: true, FairSharing: true, EffectiveQuotas: true,
			Resources: map[ResourceKey]QueueResource{
				{Flavor: "h100", Resource: "nvidia.com/gpu"}: {
					Nominal: 8, LendingLimit: ptr(2), BorrowingLimit: ptr(4), Reserved: 6, Borrowed: 1,
				},
			},
		}))
		Expect(q.CanReclaim()).To(BeFalse(), "LowerPriority reclaims only from lower priorities: read as Never")

		unset := r.queueState(clusterQueue(v, "bare", flavorQuotas("h100", quota("nvidia.com/gpu", "8"))), map[string]string{"h100": ""})
		Expect(unset.Reclaim).To(Equal(ReclaimNever), "absent reclaimWithinCohort is Never")
		Expect(unset.HasStatus).To(BeFalse())
		Expect(unset.Resources[gpuKey].LendingLimit).To(BeNil())
		Expect(unset.Resources[gpuKey].BorrowingLimit).To(BeNil())

		stopped := r.queueState(clusterQueue(v, "gone", flavorQuotas("missing", quota("nvidia.com/gpu", "8"))), map[string]string{})
		Expect(stopped.Inactive).To(BeTrue(), "a flavor the cluster lacks admits nothing")
	})

	It("carries a flavor's full node labels and tolerations", func() {
		rf := flavor("v1beta2", "h100", map[string]interface{}{"nvidia.com/gpu.product": h100Product, "pool": "train"})
		rf.Object["spec"].(map[string]interface{})["tolerations"] = []interface{}{
			map[string]interface{}{"key": "gpu", "operator": "Exists", "effect": "NoSchedule"},
		}
		p := flavorPlacement(rf)
		Expect(p.AcceleratorType).To(Equal(h100Product))
		Expect(p.NodeLabels).To(Equal(map[string]string{"nvidia.com/gpu.product": h100Product, "pool": "train"}))
		Expect(p.Tolerations).To(Equal([]corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}))
	})

	It("reads a Cohort's parent under either spelling, and its own quota", func() {
		Expect(cohortState(cohortObj("v1beta2", "a", map[string]interface{}{"parentName": "root"})).Parent).To(Equal("root"))
		Expect(cohortState(cohortObj("v1alpha1", "a", map[string]interface{}{"parent": "root"})).Parent).To(Equal("root"))
		Expect(cohortState(cohortObj("v1beta2", "a", map[string]interface{}{
			"resourceGroups": []interface{}{map[string]interface{}{"coveredResources": []interface{}{"nvidia.com/gpu"}}},
		})).HasResourceGroups).To(BeTrue())
	})
})
