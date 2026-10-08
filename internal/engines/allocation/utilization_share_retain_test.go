package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Per-role history of a role that left the group", func() {
	tm := simTimings()
	t0 := time.Unix(0, 0)
	horizon := max(tm.ReversalHold, 2*tm.SwingWindow, tm.ReleaseTimeout<<4)

	It("keeps a briefly absent role's back-off, and forgets a gone role's past the horizon", func() {
		l := NewShareLedger()
		l.MarkFailed("gone", t0, tm)
		l.Retain([]string{"gone", "stays"}, t0, tm)
		Expect(l.BackingOff("gone", t0)).To(BeTrue(), "setup")

		// Absent, but within the horizon: the back-off still holds.
		l.Retain([]string{"stays"}, t0.Add(horizon), tm)
		Expect(l.aborts).To(HaveKey("gone"))

		l.Retain([]string{"stays"}, t0.Add(horizon+time.Second), tm)
		Expect(l.aborts).NotTo(HaveKey("gone"))
		Expect(l.giveAfter).NotTo(HaveKey("gone"))
		Expect(l.seen).NotTo(HaveKey("gone"))
		Expect(l.seen).To(HaveKey("stays"))
	})

	It("never forgets a role a live transfer names", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 4, "B": 0}
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
		l.Retain(nil, t0.Add(10*horizon), tm)
		Expect(l.lastGave).To(HaveKey("A"))
		Expect(l.lastGot).To(HaveKey("B"))
	})
})
