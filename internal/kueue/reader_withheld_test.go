package kueue

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Kueue features that change what quota reserved means, or where a pod lands,
// withhold borrowing in the whole cohort: the figures here would not match what
// Kueue admits.
var _ = Describe("Reader withholds borrowing for Kueue features it does not model", func() {
	const v = "v1beta2"
	reasons := func(objs ...client.Object) []string {
		snap := readBorrow(newCohortClient(v, objs...))
		out := make([]string, 0, 3)
		for _, ns := range []string{"team-a", "team-b"} {
			ExpectWithOffset(1, snap.Borrow[ns]).To(HaveLen(1))
			out = append(out, snap.Borrow[ns][0].Reason)
		}
		for _, p := range snap.SharedPools {
			out = append(out, p.Reason)
		}
		return out
	}
	// example is docsExample with its objects open to a change: mutate gets the
	// flavor and the two queues.
	example := func(mutate func(rf, a, b *unstructured.Unstructured)) []client.Object {
		objs := docsExample(v, "cohortName")
		mutate(objs[0].(*unstructured.Unstructured), objs[1].(*unstructured.Unstructured), objs[2].(*unstructured.Unstructured))
		return objs
	}

	It("counts the plain example (the control)", func() {
		Expect(reasons(docsExample(v, "cohortName")...)).To(Equal([]string{"nominal-lent-out", "nominal-lent-out", ""}))
	})

	It("withholds with topology-aware when a GPU flavor names a Topology (TAS)", func() {
		got := reasons(example(func(rf, _, _ *unstructured.Unstructured) {
			Expect(unstructured.SetNestedField(rf.Object, "default", "spec", "topologyName")).To(Succeed())
		})...)
		Expect(got).To(Equal([]string{"topology-aware", "topology-aware", "topology-aware"}))
	})

	It("withholds with admission-checks when a member queue names admission checks", func() {
		got := reasons(example(func(_, _, b *unstructured.Unstructured) {
			Expect(unstructured.SetNestedSlice(b.Object, []interface{}{"prov-req"}, "spec", "admissionChecks")).To(Succeed())
		})...)
		Expect(got).To(Equal([]string{"admission-checks", "admission-checks", "admission-checks"}),
			"one member's checks withhold the whole cohort: a lender's admission is gated too")
	})

	It("withholds with admission-checks under admissionChecksStrategy, the field MultiKueue uses", func() {
		got := reasons(example(func(_, a, _ *unstructured.Unstructured) {
			strategy := map[string]interface{}{"admissionChecks": []interface{}{
				map[string]interface{}{"name": "multikueue"},
			}}
			Expect(unstructured.SetNestedMap(a.Object, strategy, "spec", "admissionChecksStrategy")).To(Succeed())
		})...)
		Expect(got).To(Equal([]string{"admission-checks", "admission-checks", "admission-checks"}))
	})

	It("counts an empty admissionChecks list as none", func() {
		got := reasons(example(func(_, a, _ *unstructured.Unstructured) {
			Expect(unstructured.SetNestedSlice(a.Object, []interface{}{}, "spec", "admissionChecks")).To(Succeed())
		})...)
		Expect(got).To(Equal([]string{"nominal-lent-out", "nominal-lent-out", ""}))
	})
})
