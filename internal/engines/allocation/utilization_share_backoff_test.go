package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("A donor's back-off after aborted releases", func() {
	tm := ShareTimings{Window: time.Minute, ReleaseTimeout: 10 * time.Minute, FillTimeout: time.Hour,
		ReversalHold: time.Minute, SwingWindow: time.Minute}
	held := func() map[string]int { return map[string]int{"A": 4, "B": 0} }

	// abort starts a transfer from A at now and lets it time out unreleased,
	// returning when the abort was observed.
	abort := func(l *ShareLedger, now time.Time) time.Time {
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, held(), now, tm)
		at := now.Add(tm.ReleaseTimeout)
		Expect(l.Observe(held(), at, tm)).To(ConsistOf(HaveField("Outcome", ShareOutcomeAborted)))
		return at
	}

	It("doubles per consecutive abort, up to sixteen release timeouts", func() {
		l := NewShareLedger()
		now := time.Unix(0, 0)
		for n, want := range []int{1, 2, 4, 8, 16, 16} {
			now = abort(l, now)
			Expect(l.giveAfter["A"].Sub(now)).To(Equal(time.Duration(want)*tm.ReleaseTimeout), "abort %d", n+1)
			now = l.giveAfter["A"]
		}
	})

	It("starts over once a release lands", func() {
		l := NewShareLedger()
		abort(l, time.Unix(0, 0))
		now := abort(l, l.giveAfter["A"])
		Expect(l.giveAfter["A"].Sub(now)).To(Equal(2 * tm.ReleaseTimeout))

		now = l.giveAfter["A"]
		h := held()
		l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, h, now, tm)
		h["A"] = 3
		now = now.Add(time.Minute)
		l.Observe(h, now, tm)
		Expect(l.TakeReleased()).To(HaveLen(1), "setup: the release landed")
		Expect(l.aborts).NotTo(HaveKey("A"))

		l.MarkFailed("A", now, tm)
		Expect(l.giveAfter["A"].Sub(now)).To(Equal(tm.ReleaseTimeout), "a failed mark after a landed release")
	})

	It("counts a failed mark in the same run as an abort", func() {
		l := NewShareLedger()
		now := abort(l, time.Unix(0, 0))
		l.MarkFailed("A", now, tm)
		Expect(l.giveAfter["A"].Sub(now)).To(Equal(2 * tm.ReleaseTimeout))
		l.MarkFailed("", now, tm)
		Expect(l.aborts).NotTo(HaveKey(""), "an empty donor is not recorded")
	})
})

var _ = Describe("A node-planned transfer whose planned pod could not be read", func() {
	tm := ShareTimings{Window: time.Minute, ReleaseTimeout: time.Hour, FillTimeout: time.Hour,
		ReversalHold: time.Minute, SwingWindow: time.Minute}
	t0 := time.Unix(0, 0)

	It("neither releases nor ends as wrong-pod until the read succeeds", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 4, "B": 0}
		t1 := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1,
			PlannedPods: []string{"ns/a-0"}}, held, t0, tm)
		held["A"] = 3 // a pod went: which one is not known
		for i := 1; i <= 3; i++ {
			l.PlannedRunning(nil, map[string]bool{t1.ID: true})
			Expect(l.Observe(held, t0.Add(time.Duration(i)*time.Minute), tm)).To(BeEmpty())
			Expect(l.TakeReleased()).To(BeEmpty(), "released while the planned pod is unread")
		}
		// The read succeeds: the planned pod is gone.
		l.PlannedRunning(nil, nil)
		l.Observe(held, t0.Add(4*time.Minute), tm)
		Expect(l.TakeReleased()).To(ConsistOf(HaveField("ID", t1.ID)))
	})

	It("ends as wrong-pod once the read shows the planned pod still running", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 4, "B": 0}
		t1 := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1,
			PlannedPods: []string{"ns/a-0"}}, held, t0, tm)
		held["A"] = 3
		l.PlannedRunning(nil, map[string]bool{t1.ID: true})
		Expect(l.Observe(held, t0.Add(time.Minute), tm)).To(BeEmpty())
		l.PlannedRunning(map[string]bool{t1.ID: true}, nil)
		Expect(l.Observe(held, t0.Add(2*time.Minute), tm)).To(ConsistOf(HaveField("Outcome", ShareOutcomeWrongPod)))
	})
})
