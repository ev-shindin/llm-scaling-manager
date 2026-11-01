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

	It("cancels only a Releasing transfer inside the window, and holds only the cancelled pair", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5}
		t := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)
		// As in the engine: a started transfer is confirmed in its own cycle,
		// before any cancel.
		l.ConfirmStarted(t.ID, []string{"a-1"})
		Expect(l.Cancel(t.ID, at(400), tm)).To(BeFalse(), "past the window a pod may have moved")
		Expect(l.Cancel(t.ID, at(100), tm)).To(BeTrue())
		Expect(l.InFlight()).To(BeZero())
		// Nothing moved, and the demand that cancelled it calls for the
		// reverse: A may receive and B may give at once.
		Expect(l.ReceivingHeld("A", at(101), tm)).To(BeFalse(), "the donor whose demand rose is free to receive")
		Expect(l.GivingHeld("B", at(101), tm)).To(BeFalse(), "the receiver whose demand fell is free to give")
		Expect(l.Swinging("A", at(101))).To(BeFalse(), "a cancel is no move toward a swing")
		// The cancelled pair is held one reversal hold, so start and cancel
		// cannot loop; neither role is held toward anyone else.
		Expect(l.PairHeld("A", "B", at(100+719))).To(BeTrue())
		Expect(l.PairHeld("A", "B", at(100+720))).To(BeFalse())
		Expect(l.GivingHeld("A", at(101), tm)).To(BeFalse(), "A may give to another receiver")
		Expect(l.ReceivingHeld("B", at(101), tm)).To(BeFalse(), "B may receive from another donor")
	})

	It("undoes only the cancelled transfer's moves, in any order", func() {
		for _, order := range [][2]int{{0, 1}, {1, 0}} {
			l := NewShareLedger()
			held := map[string]int{"A": 9, "B": 5, "C": 1}
			ts := []ShareTransfer{
				l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm),
				l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm),
			}
			other := l.Start(ShareTransfer{Donor: "A", Receiver: "C", GPUs: 1}, held, at(30), tm)
			for _, x := range append(ts, other) {
				l.ConfirmStarted(x.ID, nil)
			}
			Expect(l.Cancel(ts[order[0]].ID, at(60), tm)).To(BeTrue())
			Expect(l.GivingHeld("B", at(61), tm)).To(BeTrue(), "order %v: the other A -> B transfer still holds B", order)
			Expect(l.Cancel(ts[order[1]].ID, at(60), tm)).To(BeTrue())
			Expect(l.GivingHeld("B", at(61), tm)).To(BeFalse(), "order %v: no A -> B move is left", order)
			Expect(l.ReceivingHeld("A", at(61), tm)).To(BeTrue(), "order %v: A -> C still stands", order)
			Expect(l.GivingHeld("C", at(61), tm)).To(BeTrue(), "order %v: C received", order)
			Expect(l.Cancel(other.ID, at(61), tm)).To(BeTrue())
			Expect(l.ReceivingHeld("A", at(62), tm)).To(BeFalse(), "order %v: nothing A gave is left", order)
		}
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

	It("marks a role swinging after three direction changes and plans it on its mean need", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5}
		for i, need := range []float64{4, 6, 8} {
			l.RecordNeeds(map[string]float64{"A": need}, at(i*60), tm)
		}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)   // A gives
		l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 1}, held, at(900), tm) // A receives: one flip
		Expect(l.Swinging("A", at(900))).To(BeFalse())
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(1800), tm) // A gives: two flips
		Expect(l.Swinging("A", at(1800))).To(BeFalse(), "a load that went and came back once is no swing")
		l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 1}, held, at(2700), tm) // A receives: three flips
		Expect(l.Swinging("A", at(2700))).To(BeTrue())
		Expect(l.PlanningNeed("A", 99, at(2700))).To(BeNumerically("~", 6, 1e-9))
		Expect(l.PlanningNeed("B", 99, at(2700))).To(Equal(99.0), "B flipped too, but has no recorded need")
		Expect(l.Swinging("A", at(2700).Add(tm.SwingWindow))).To(BeFalse())
	})

	It("counts flips only within the swing window", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5}
		gap := int(tm.SwingWindow/time.Second) / 2
		for i := range 4 {
			d, r := "A", "B"
			if i%2 == 1 {
				d, r = r, d
			}
			// Each move more than half a window after the last: three flips
			// in all, never three within one window.
			l.Start(ShareTransfer{Donor: d, Receiver: r, GPUs: 1}, held, at(i*(gap+60)), tm)
		}
		Expect(l.Swinging("A", at(3*(gap+60)))).To(BeFalse())
	})

	It("counts a cancel toward a swing as the move it would have made", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 9, "B": 5}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(0), tm)   // A gives
		l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 1}, held, at(900), tm) // A receives: one flip
		// Two transfers called off in one cycle -- a set's contributors go
		// with its primary -- are one attempted give, one change of
		// direction: two in all.
		c1 := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(1800), tm)
		c2 := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, held, at(1800), tm)
		l.ConfirmStarted(c1.ID, nil)
		l.ConfirmStarted(c2.ID, nil)
		Expect(l.Cancel(c1.ID, at(1860), tm)).To(BeTrue())
		Expect(l.Cancel(c2.ID, at(1860), tm)).To(BeTrue())
		Expect(l.Swinging("A", at(1860))).To(BeFalse())
		c := l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 1}, held, at(2400), tm)
		l.ConfirmStarted(c.ID, nil)
		Expect(l.Cancel(c.ID, at(2460), tm)).To(BeTrue()) // three
		Expect(l.Swinging("A", at(2460))).To(BeTrue())
		Expect(l.Swinging("A", at(2460).Add(tm.SwingWindow))).To(BeFalse())
	})

	// A spare return (Entitled) records no move: it trades no GPU with
	// another role. Counted when called off -- as a bursty role's load came
	// back inside its scale-down window -- three such cancels made a role
	// swinging that never gave to or received from anyone, and held its next
	// swap back on its mean for 28 minutes against 8m30s.
	It("does not count a cancelled spare return toward a swing", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 24, "B": 8}
		for i := range 3 {
			s := i * 600
			r := l.Start(ShareTransfer{Donor: "A", DonorGPUs: 8, GPUs: 8, Entitled: true, Returned: true}, held, at(s), tm)
			l.ConfirmStarted(r.ID, nil)
			Expect(l.Cancel(r.ID, at(s+120), tm)).To(BeTrue())
		}
		Expect(l.Swinging("A", at(1320))).To(BeFalse())
	})

	// A cancel is a change of direction only when it reverses the move
	// before it. Counted as one whatever its direction, a single swap back
	// with one transfer of the first swap called off was a swing, and so was
	// one real reversal after two called-off gives.
	It("counts a cancel as a change of direction only when it reverses the move before it", func() {
		By("raise, swap with one transfer called off, swap back: two changes")
		l := NewShareLedger()
		held := map[string]int{"A": 16, "B": 16}
		l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 8}, held, at(0), tm)    // A raised
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 8}, held, at(1500), tm) // load to B: A gives
		c := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 8}, held, at(1620), tm)
		l.ConfirmStarted(c.ID, nil)
		// A wobble: A's second give is called off.
		Expect(l.Cancel(c.ID, at(1680), tm)).To(BeTrue())
		// The swap back: A receives.
		l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 8}, held, at(3000), tm)
		Expect(l.Swinging("A", at(3000))).To(BeFalse())
		Expect(l.Swinging("B", at(3000))).To(BeFalse())

		By("two called-off gives, then one real reversal: one change")
		l = NewShareLedger()
		held = map[string]int{"A": 24, "B": 8, "C": 8}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 8}, held, at(0), tm)
		for _, s := range []int{300, 900} {
			c := l.Start(ShareTransfer{Donor: "A", Receiver: "C", GPUs: 8}, held, at(s), tm)
			l.ConfirmStarted(c.ID, nil)
			Expect(l.Cancel(c.ID, at(s+60), tm)).To(BeTrue())
		}
		l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 8}, held, at(1800), tm)
		Expect(l.Swinging("A", at(1800))).To(BeFalse())

		By("control: a called-off receive between them reverses both ways")
		c = l.Start(ShareTransfer{Donor: "A", Receiver: "C", GPUs: 8}, held, at(2400), tm)
		l.ConfirmStarted(c.ID, nil)
		Expect(l.Cancel(c.ID, at(2460), tm)).To(BeTrue()) // A gives after receiving: two
		c = l.Start(ShareTransfer{Donor: "C", Receiver: "A", GPUs: 8}, held, at(2700), tm)
		l.ConfirmStarted(c.ID, nil)
		Expect(l.Cancel(c.ID, at(2760), tm)).To(BeTrue()) // and receives again: three
		Expect(l.Swinging("A", at(2760))).To(BeTrue())
	})

	// A give called off because the donor's own demand came back
	// (CancelDonorBack) is a change of direction only at the donor's second
	// such dip inside the swing window. A first dip older than the window is
	// forgotten: the next dip is a first turn again, recorded as the give
	// alone. Without that bound a dip from hours ago made any later one a
	// change of direction, and two real moves after it a swing.
	It("counts a donor's dip as a change of direction only at its second inside the swing window", func() {
		swingsAfter := func(gap time.Duration) bool {
			l := NewShareLedger()
			held := map[string]int{"A": 24, "B": 8}
			c := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 8}, held, t0, tm) // first dip
			l.ConfirmStarted(c.ID, nil)
			Expect(l.CancelDonorBack(c.ID, t0.Add(time.Minute), tm)).To(BeTrue())
			d := t0.Add(time.Minute + gap)
			c = l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 8}, held, d, tm) // second dip
			l.ConfirmStarted(c.ID, nil)
			Expect(l.CancelDonorBack(c.ID, d.Add(time.Minute), tm)).To(BeTrue())
			l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 8}, held, d.Add(5*time.Minute), tm)  // A gives
			l.Start(ShareTransfer{Donor: "B", Receiver: "A", GPUs: 8}, held, d.Add(20*time.Minute), tm) // A receives
			return l.Swinging("A", d.Add(20*time.Minute))
		}
		By("control: the second dip inside the window is a change of direction, and the two moves after it a swing")
		Expect(swingsAfter(30 * time.Minute)).To(BeTrue())
		By("the first dip outside the window: the second is a first turn, one change in all")
		Expect(swingsAfter(tm.SwingWindow + 10*time.Minute)).To(BeFalse())
	})

	It("counts consecutive actionable cycles and resets on a miss", func() {
		l := NewShareLedger()
		Expect(l.Confirm(map[string]bool{"A": true})["A"]).To(Equal(1))
		Expect(l.Confirm(map[string]bool{"A": true})["A"]).To(Equal(2))
		Expect(l.Confirm(map[string]bool{"A": false})).NotTo(HaveKey("A"))
		Expect(l.Confirm(map[string]bool{"A": true})["A"]).To(Equal(1))
	})
})

var _ = Describe("ShareLedger.Forget", func() {
	tm := ShareTimings{Window: time.Minute, ReleaseTimeout: 10 * time.Minute, FillTimeout: 5 * time.Minute,
		ReversalHold: time.Hour, SwingWindow: time.Hour}
	t0 := time.Unix(0, 0)

	It("puts back the holds Start set for a transfer that never took effect", func() {
		l := NewShareLedger()
		t := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, map[string]int{"A": 4}, t0, tm)
		Expect(l.ReceivingHeld("A", t0, tm)).To(BeTrue(), "control: Start holds the donor from receiving")
		Expect(l.GivingHeld("B", t0, tm)).To(BeTrue(), "control: Start holds the receiver from giving")
		l.Forget(t.ID, tm)
		Expect(l.Transfers()).To(BeEmpty())
		Expect(l.ReceivingHeld("A", t0, tm)).To(BeFalse())
		Expect(l.GivingHeld("B", t0, tm)).To(BeFalse())
	})

	It("keeps the holds of a transfer that was confirmed", func() {
		l := NewShareLedger()
		t := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, map[string]int{"A": 4}, t0, tm)
		l.ConfirmStarted(t.ID, []string{"ns/a-0"})
		Expect(l.Transfers()[0].DonorLowered).To(BeTrue())
		Expect(l.ReceivingHeld("A", t0, tm)).To(BeTrue())
	})

	It("never reuses an ID across ledgers, so a restart cannot collide with a restored transfer", func() {
		a := NewShareLedger().Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, map[string]int{}, t0, tm)
		time.Sleep(time.Microsecond)
		b := NewShareLedger().Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1}, map[string]int{}, t0, tm)
		Expect(a.ID).NotTo(Equal(b.ID))
	})
})
