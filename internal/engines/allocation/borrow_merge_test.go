package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

var _ = Describe("namespaceBorrowed", func() {
	// twoTypes is a borrowing namespace "b" holding H100 16 of a guaranteed 8
	// and A100 8 of a guaranteed 4.
	twoTypes := func() []*ResourceConstraints {
		c := borrowQuota(0, nsBorrow{ns: "b", guaranteed: 8, used: 16, cap: -1})
		c.NamespacePools["b"]["A100"] = ResourcePool{Limit: 4, Used: 8}
		c.NamespaceBorrow["b"]["A100"] = NamespaceBorrow{Cap: -1}
		return []*ResourceConstraints{c}
	}

	It("takes gated GPUs off the type they name, not off every type", func() {
		got := namespaceBorrowed(twoTypes(), map[string]map[string]int{"b": {"H100": 8}})
		Expect(got["b"]).To(Equal(map[string]int{"H100": 0, "A100": 4}),
			"8 gated H100 GPUs hold no H100 reservation; the A100 borrowed GPUs are still borrowed")
	})

	It("maps a gated product name to its family's type", func() {
		got := namespaceBorrowed(twoTypes(), map[string]map[string]int{"b": {"NVIDIA-A100-SXM4-80GB": 2}})
		Expect(got["b"]).To(Equal(map[string]int{"H100": 8, "A100": 2}))
	})

	It("shares gated GPUs that name no type over the types, once", func() {
		got := namespaceBorrowed(twoTypes(), map[string]map[string]int{"b": {"": 6}})
		sum := got["b"]["A100"] + got["b"]["H100"]
		Expect(sum).To(Equal(6), "12 borrowed, 6 untyped gated: taken once, not from each type")
		Expect(got["b"]).To(Equal(map[string]int{"A100": 0, "H100": 6}), "in a stable order")
	})
})

var _ = Describe("mergeSharedPools", func() {
	t0 := time.Unix(30000, 0)
	read := func(free int, at time.Time, stale, optIn bool, lenders ...kueue.LenderState) *ResourceConstraints {
		c := borrowQuota(free, nsBorrow{ns: "b", guaranteed: 8, used: 8, cap: -1})
		c.SharedPools[0].ReadAt, c.SharedPools[0].Stale = at, stale
		c.SharedPools[0].ReleaseToNonReclaiming = optIn
		c.SharedPools[0].Lenders = lenders
		return c
	}

	It("takes the cautious side of two entries' reads of one pool", func() {
		a := read(6, t0.Add(time.Minute), false, true, kueue.LenderState{Queue: "l1", LentOut: 8})
		b := read(4, t0, true, false,
			kueue.LenderState{Queue: "l1", Waiting: true, CanReclaim: true, LentOut: 4},
			kueue.LenderState{Queue: "l2", LentOut: 2})
		chunks, _ := mergeSharedPools([]*ResourceConstraints{a, b})
		Expect(chunks).To(HaveLen(1))
		ch := chunks[0]
		Expect(ch.Free).To(Equal(4))
		Expect(ch.Stale).To(BeTrue(), "stale when either read is")
		Expect(ch.ReadAt).To(Equal(t0), "the older read")
		Expect(ch.ReleaseToNonReclaiming).To(BeFalse(), "only when every entry opts in")
		Expect(ch.Lenders).To(Equal([]kueue.LenderState{
			{Queue: "l1", Waiting: true, CanReclaim: true, LentOut: 4},
			{Queue: "l2", LentOut: 2},
		}), "the union of the lenders")
		Expect(a.SharedPools[0].Lenders).To(Equal([]kueue.LenderState{{Queue: "l1", LentOut: 8}}), "the inputs are not written")
	})

	It("keeps the older ReadAt whichever entry comes first", func() {
		chunks, _ := mergeSharedPools([]*ResourceConstraints{read(4, t0, false, true), read(4, t0.Add(time.Minute), false, true)})
		Expect(chunks[0].ReadAt).To(Equal(t0))
		Expect(chunks[0].ReleaseToNonReclaiming).To(BeTrue())
	})
})
