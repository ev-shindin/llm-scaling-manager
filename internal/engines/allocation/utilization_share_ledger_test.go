package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ShareLedger", func() {
	tm := ShareTimings{
		Window:         300 * time.Second,
		ReleaseTimeout: 600 * time.Second,
		FillTimeout:    120 * time.Second,
		ReversalHold:   720 * time.Second,
		SwingWindow:    96 * time.Minute,
	}
	t0 := time.Unix(1_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	It("commits a Releasing transfer to both sides and a Filling one to the receiver only", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)
		Expect(l.Committed(held)).To(Equal(map[string]int{"A": 8, "B": 6}))
		Expect(l.Promised()).To(BeZero())

		// The donor's pod is gone: released, so Filling, and promised.
		held["A"] = 8
		Expect(l.Observe(held, at(360), tm)).To(BeEmpty())
		Expect(l.Transfers()[0].State).To(Equal(ShareFilling))
		Expect(l.Committed(held)).To(Equal(map[string]int{"A": 8, "B": 6}))
		Expect(l.Promised()).To(Equal(1))

		// The receiver's pod is scheduled: done, and nothing is promised.
		held["B"] = 6
		ended := l.Observe(held, at(390), tm)
		Expect(ended).To(HaveLen(1))
		Expect(ended[0].Outcome).To(Equal(ShareOutcomeDone))
		Expect(l.InFlight()).To(BeZero())
		Expect(l.Promised()).To(BeZero())
	})

	It("charges a donor its own replica size when it is larger than the receiver's", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 16, "B": 4}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 4, DonorGPUs: 8}, held, at(0), tm)
		Expect(l.Committed(held)).To(Equal(map[string]int{"A": 8, "B": 8}))
		held["A"] = 12 // only half the donor replica has gone: not released
		l.Observe(held, at(360), tm)
		Expect(l.Transfers()[0].State).To(Equal(ShareReleasing))
		held["A"] = 8
		l.Observe(held, at(390), tm)
		Expect(l.Transfers()[0].State).To(Equal(ShareFilling))
	})

	It("releases two transfers from one donor in start order", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 10, "B": 2, "C": 2}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)
		l.Start(ShareTransfer{Donor: "A", Receiver: "C", GPUs: 1}, held, at(0), tm)
		held["A"] = 9
		l.Observe(held, at(360), tm)
		ts := l.Transfers()
		Expect(ts[0].State).To(Equal(ShareFilling))
		Expect(ts[1].State).To(Equal(ShareReleasing))
		held["A"] = 8
		l.Observe(held, at(390), tm)
		Expect(l.Transfers()[1].State).To(Equal(ShareFilling))
	})

	It("aborts a release past its timeout and ends a fill past its own", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5, "C": 3, "D": 3}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)
		ended := l.Observe(held, at(600), tm)
		Expect(ended).To(HaveLen(1))
		Expect(ended[0].Outcome).To(Equal(ShareOutcomeAborted))

		l.Start(ShareTransfer{Donor: "C", Receiver: "D", GPUs: 1}, held, at(600), tm)
		held["C"] = 2
		l.Observe(held, at(960), tm) // released, Filling until 1080
		Expect(l.Observe(held, at(1050), tm)).To(BeEmpty())
		ended = l.Observe(held, at(1080), tm)
		Expect(ended).To(HaveLen(1))
		Expect(ended[0].Outcome).To(Equal(ShareOutcomeFillTimeout))
	})

	It("cancels only a Releasing transfer inside the window, and holds the pair", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5}
		t := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)
		Expect(l.Cancel(t.ID, at(400), tm)).To(BeFalse(), "past the window a pod may have moved")
		Expect(l.Cancel(t.ID, at(100), tm)).To(BeTrue())
		Expect(l.InFlight()).To(BeZero())
		Expect(l.ReceivingHeld("A", at(100+719), tm)).To(BeTrue())
		Expect(l.ReceivingHeld("A", at(100+720), tm)).To(BeFalse())
		Expect(l.GivingHeld("B", at(500), tm)).To(BeTrue())
	})

	It("holds only the opposite direction", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)
		Expect(l.ReceivingHeld("A", at(30), tm)).To(BeTrue())
		Expect(l.GivingHeld("A", at(30), tm)).To(BeFalse())
		Expect(l.GivingHeld("B", at(30), tm)).To(BeTrue())
		Expect(l.ReceivingHeld("B", at(30), tm)).To(BeFalse())
	})

	It("sets no hold for an entitled transfer", func() {
		l := NewShareLedger()
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, Entitled: true}, map[string]int{"A": 9}, at(0), tm)
		Expect(l.ReceivingHeld("A", at(30), tm)).To(BeFalse())
		Expect(l.GivingHeld("B", at(30), tm)).To(BeFalse())
	})

	It("marks a role swinging after two direction changes and plans it on its mean need", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5}
		for i, need := range []float64{4, 6, 8} {
			l.RecordNeeds(map[string]float64{"A": need}, at(i*60), tm)
		}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)   // A gives
		l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 1}, held, at(900), tm) // A receives: one flip
		Expect(l.Swinging("A", at(900))).To(BeFalse())
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(1800), tm) // A gives: two flips
		Expect(l.Swinging("A", at(1800))).To(BeTrue())
		Expect(l.PlanningNeed("A", 99, at(1800))).To(BeNumerically("~", 6, 1e-9))
		Expect(l.PlanningNeed("B", 99, at(1800))).To(Equal(99.0), "B flipped too, but has no recorded need")
		Expect(l.Swinging("A", at(1800).Add(tm.SwingWindow))).To(BeFalse())
	})

	It("counts consecutive actionable cycles and resets on a miss", func() {
		l := NewShareLedger()
		Expect(l.Confirm(map[string]bool{"A": true})["A"]).To(Equal(1))
		Expect(l.Confirm(map[string]bool{"A": true})["A"]).To(Equal(2))
		Expect(l.Confirm(map[string]bool{"A": false})).NotTo(HaveKey("A"))
		Expect(l.Confirm(map[string]bool{"A": true})["A"]).To(Equal(1))
	})
})
