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

var _ = Describe("Retain's horizon and the maps it prunes", func() {
	// The longest window here is ReleaseTimeout<<4 = 160m: written out, not
	// computed with the formula under test.
	tm := ShareTimings{Window: time.Minute, ReleaseTimeout: 10 * time.Minute, FillTimeout: time.Minute,
		ReversalHold: 30 * time.Minute, SwingWindow: 20 * time.Minute}
	const horizon = 160 * time.Minute
	t0 := time.Unix(0, 0)

	It("prunes every per-role map, for a role recorded before Retain first ran", func() {
		l := NewShareLedger()
		// History written by the ledger's own paths, never through Retain.
		held := map[string]int{"gone": 4, "B": 0}
		l.Start(ShareTransfer{Donor: "gone", Receiver: "B", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
		Expect(l.Observe(held, t0.Add(tm.ReleaseTimeout), tm)).To(HaveLen(1), "setup: aborted")
		l.RecordNeeds(map[string]float64{"gone": 2}, t0, tm)
		l.swingUntil["gone"] = t0.Add(tm.SwingWindow)
		// Only in the maps the first seeding missed: needs, and a fill block.
		l.RecordNeeds(map[string]float64{"needs-only": 1}, t0, tm)
		l.FillBlocked("blocked-only", "release-taken", t0.Add(time.Hour))
		l.FillBlocked("gone", "release-taken", t0.Add(time.Hour))
		for _, m := range []bool{len(l.aborts) > 0, len(l.giveAfter) > 0, len(l.lastGave) > 0,
			len(l.moves) > 0, len(l.needs) > 0, len(l.fillBlocked) > 0} {
			Expect(m).To(BeTrue(), "setup: a map is empty")
		}

		at := t0.Add(tm.ReleaseTimeout)
		l.Retain([]string{"B"}, at, tm) // first call: "gone" starts its absence now
		l.Retain([]string{"B"}, at.Add(horizon), tm)
		Expect(l.aborts).To(HaveKey("gone"), "forgotten at the horizon, not past it")

		l.Retain([]string{"B"}, at.Add(horizon+time.Second), tm)
		Expect(l.aborts).NotTo(HaveKey("gone"))
		Expect(l.giveAfter).NotTo(HaveKey("gone"))
		Expect(l.lastGave).NotTo(HaveKey("gone"))
		Expect(l.moves).NotTo(HaveKey("gone"))
		Expect(l.needs).NotTo(HaveKey("gone"))
		Expect(l.swingUntil).NotTo(HaveKey("gone"))
		Expect(l.fillBlocked).NotTo(HaveKey("gone"))
		Expect(l.seen).NotTo(HaveKey("gone"))
		Expect(l.needs).NotTo(HaveKey("needs-only"))
		Expect(l.fillBlocked).NotTo(HaveKey("blocked-only"))
		Expect(l.lastGot).To(HaveKey("B"), "a present role keeps its history")
	})
})
