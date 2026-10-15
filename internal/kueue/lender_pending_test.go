package kueue

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Review finding (security, Kueue maintainer): a lender counted as waiting
// whenever Kueue reported any pending Workload, so a CPU job, or one too
// large to admit, made the borrowers give back capacity for nothing; and the
// release was bounded by everything lent out, not by what the lender asked
// for.
var _ = Describe("Lender waiting from its pending Workloads", func() {
	cpu := PendingWorkload{GPUs: nil}
	amd := PendingWorkload{GPUs: map[string]int64{"amd.com/gpu": 2}}
	lender := func(pending ...PendingWorkload) QueueState {
		q := member("lender", QueueResource{Nominal: 8, Reserved: 3}) // 5 unused
		q.PendingWorkloads = pending
		return q
	}

	DescribeTable("what it waits for",
		func(q QueueState, want int) {
			gpus, _ := lenderPending(q, gpuKey)
			Expect(gpus).To(Equal(want))
			Expect(LenderWaiting(q, gpuKey)).To(Equal(want > 0))
		},
		Entry("a CPU-only Workload is not waiting for GPUs", lender(cpu), 0),
		Entry("another resource's Workload is not waiting for this one", lender(amd), 0),
		Entry("a Workload larger than the unused nominal would have to borrow itself", lender(pendingGPU(6, 0)), 0),
		Entry("the largest Workload that fits bounds the release", lender(pendingGPU(2, 0), pendingGPU(5, 0), pendingGPU(9, 0)), 5),
	)

	borrowing := func(priority *int32) QueueState {
		q := member("a", QueueResource{Nominal: 0, Reserved: 4, Borrowed: 4})
		q.ReservedPriority = priority
		return q
	}
	p := func(n int32) *int32 { return &n }
	lenderState := func(lenderPriority int32, borrower QueueState) LenderState {
		l := withReclaim(lender(pendingGPU(2, lenderPriority)), ReclaimLowerPriority)
		b := ComputeBorrowing(topology(l, borrower), false)
		Expect(b.Pools).To(HaveLen(1))
		for _, ls := range b.Pools[0].Lenders {
			if ls.Queue == "lender" {
				return ls
			}
		}
		Fail("no lender")
		return LenderState{}
	}

	It("a LowerPriority lender whose pending Workload outranks the borrowers is one Kueue reclaims for", func() {
		got := lenderState(1000, borrowing(p(100)))
		Expect(got).To(Equal(LenderState{Queue: "lender", Waiting: true, CanReclaim: true, LentOut: 5, PendingGPUs: 2}))
	})

	It("is read as Never when it does not outrank them, or their priority is unknown", func() {
		Expect(lenderState(100, borrowing(p(100))).CanReclaim).To(BeFalse(), "equal priority: Kueue does not preempt")
		Expect(lenderState(10, borrowing(p(100))).CanReclaim).To(BeFalse())
		Expect(lenderState(1000, borrowing(nil)).CanReclaim).To(BeFalse(), "no borrower priority read")
	})

	It("compares against queues that borrow the key only", func() {
		idle := member("idle", QueueResource{Nominal: 4})
		idle.ReservedPriority = p(5000)
		l := withReclaim(lender(pendingGPU(2, 1000)), ReclaimLowerPriority)
		b := ComputeBorrowing(topology(l, idle, borrowing(p(100))), false)
		for _, ls := range b.Pools[0].Lenders {
			if ls.Queue == "lender" {
				Expect(ls.CanReclaim).To(BeTrue(), "a high-priority queue that borrows nothing is not preempted for anyone")
			}
		}
	})
})
