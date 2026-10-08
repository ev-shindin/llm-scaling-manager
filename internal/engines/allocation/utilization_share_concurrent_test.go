package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Concurrent transfers on one role (§6.3)", func() {
	tm := simTimings()
	t0 := time.Unix(0, 0)
	ids := func(ends []ShareTransferEnd) []string {
		out := make([]string, 0, len(ends))
		for _, e := range ends {
			out = append(out, e.Transfer.ID)
		}
		return out
	}

	It("keeps the second release waiting while the donor holds what it still owes, cycle after cycle", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 10, "B": 0, "C": 0}
		t1 := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
		t2 := l.Start(ShareTransfer{Donor: "A", Receiver: "C", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
		held["A"] = 9 // one pod went
		l.Observe(held, t0.Add(time.Minute), tm)
		Expect(l.TakeReleased()).To(ConsistOf(HaveField("ID", t1.ID)))
		// A stays at 9: the second pod has not gone.
		l.Observe(held, t0.Add(2*time.Minute), tm)
		Expect(l.TakeReleased()).To(BeEmpty(), "t2 released while A still holds its pod")
		got, _ := l.Transfer(t2.ID)
		Expect(got.State).To(Equal(ShareReleasing))
		held["A"] = 8
		l.Observe(held, t0.Add(3*time.Minute), tm)
		Expect(l.TakeReleased()).To(ConsistOf(HaveField("ID", t2.ID)))
	})

	It("keeps the second fill open until its own pod lands", func() {
		l := NewShareLedger()
		held := map[string]int{"R": 4}
		f1 := l.StartFill("R", "r", 2, held, t0, tm)
		f2 := l.StartFill("R", "r", 2, held, t0, tm)
		held["R"] = 6 // one pod landed
		Expect(ids(l.Observe(held, t0.Add(10*time.Second), tm))).To(ConsistOf(f1.ID))
		// R stays at 6: the second pod is still Pending.
		Expect(l.Observe(held, t0.Add(20*time.Second), tm)).To(BeEmpty(), "f2 done before its pod landed")
		Expect(l.Committed(held)["R"]).To(Equal(8), "the second pod's GPUs stay committed to R")
		held["R"] = 8
		Expect(ids(l.Observe(held, t0.Add(30*time.Second), tm))).To(ConsistOf(f2.ID))
	})

	It("holds a transfer whose donor is not planned this cycle, rather than reading it as empty", func() {
		l := NewShareLedger()
		t1 := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, map[string]int{"A": 4, "B": 0}, t0, tm)
		l.Observe(map[string]int{"B": 0}, t0.Add(time.Minute), tm) // A frozen this cycle
		Expect(l.TakeReleased()).To(BeEmpty())
		got, _ := l.Transfer(t1.ID)
		Expect(got.State).To(Equal(ShareReleasing))
	})

	It("ends a set's releasing contributors with its primary, and does not back off a primary whose donor released", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 8, "C": 8, "B": 0}
		p := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 16, DonorGPUs: 8, SetID: "pending"}, held, t0, tm)
		c := l.Start(ShareTransfer{Donor: "C", DonorGPUs: 8, SetID: "pending"}, held, t0, tm)
		l.LinkSet(p.ID, c.ID)
		held["A"] = 0 // A released; C never does
		ends := l.Observe(held, t0.Add(tm.ReleaseTimeout+time.Second), tm)
		Expect(ids(ends)).To(ConsistOf(p.ID, c.ID), "the contributor ends with its primary")
		for _, e := range ends {
			Expect(e.Outcome).To(Equal(ShareOutcomeAborted))
		}
		Expect(l.GivingHeld("A", t0.Add(tm.ReleaseTimeout+2*time.Second), tm)).To(BeFalse(),
			"A released: the set failed on C, not on A")
	})
})
