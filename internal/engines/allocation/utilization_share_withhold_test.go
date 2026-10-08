package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WithholdPromised", func() {
	cluster := func() []*ResourceConstraints {
		return []*ResourceConstraints{{
			Pools:     map[string]ResourcePool{"A100": {Limit: 16, Used: 10}, "H100": {Limit: -1}},
			TotalUsed: 10, TotalAvail: 6,
		}}
	}

	It("charges a cluster-group promise to the cluster pool and leaves the input alone", func() {
		in := cluster()
		out := WithholdPromised(in, map[string]map[string]int{"": {"A100": 4, "H100": 8}})
		Expect(out[0].Pools["A100"].Used).To(Equal(14))
		Expect(out[0].Pools["H100"].Limit).To(Equal(-1), "an unlimited pool stays unlimited")
		Expect(out[0].TotalAvail).To(Equal(2))
		Expect(in[0].Pools["A100"].Used).To(Equal(10), "the optimizer reads the constraints unchanged")
	})

	It("charges a namespace-group promise to the namespace pool and the cluster pool", func() {
		in := cluster()
		in[0].NamespacePools = map[string]map[string]ResourcePool{"t1": {"A100": {Limit: 8, Used: 4}}}
		out := WithholdPromised(in, map[string]map[string]int{"t1": {"A100": 2}})
		Expect(out[0].NamespacePools["t1"]["A100"].Used).To(Equal(6))
		Expect(out[0].Pools["A100"].Used).To(Equal(12))
		Expect(in[0].NamespacePools["t1"]["A100"].Used).To(Equal(4))
	})

	It("refuses a wake that only fits in promised GPUs (negative control: it fits without the withhold)", func() {
		demand := map[string]int{"A100": 4}
		Expect(FitsGPUBudget(cluster(), "parked", demand)).To(BeTrue())
		Expect(FitsGPUBudget(WithholdPromised(cluster(), map[string]map[string]int{"": {"A100": 4}}), "parked", demand)).To(BeFalse())
	})

	It("returns the constraints as they are when nothing is promised", func() {
		in := cluster()
		Expect(WithholdPromised(in, nil)[0]).To(BeIdenticalTo(in[0]))
	})
})
