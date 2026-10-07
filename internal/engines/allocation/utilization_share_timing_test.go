package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DeriveShareTimings", func() {
	cycle := 30 * time.Second
	dur := func(d time.Duration) *time.Duration { return &d }

	It("uses the cluster's defaults when nothing is set", func() {
		tm, src := DeriveShareTimings(ShareTimingInputs{Cycle: cycle}, nil)
		bound := 300*time.Second + 15*time.Second + 30*time.Second + 30*time.Second
		Expect(tm.Window).To(Equal(300 * time.Second))
		Expect(tm.ReleaseTimeout).To(Equal(bound*3/2 + 2*cycle))
		Expect(tm.ReversalHold).To(Equal(2 * bound))
		Expect(tm.FillTimeout).To(Equal(30*time.Second + 15*time.Second + 2*cycle + time.Minute))
		Expect(tm.SwingWindow).To(Equal(8 * (bound + 5*time.Minute + 3*cycle)))
		Expect(src).To(HaveKeyWithValue("window", "default"))
		Expect(src).To(HaveKeyWithValue("release", "bound"))
	})

	It("follows a short scale-down window and polling interval from the ScaledObject", func() {
		tm, src := DeriveShareTimings(ShareTimingInputs{
			Cycle: cycle, ScaleDownWindow: dur(60 * time.Second), PollingInterval: dur(15 * time.Second),
		}, nil)
		Expect(tm.Window).To(Equal(60 * time.Second))
		Expect(tm.ReversalHold).To(Equal(2 * (60*time.Second + 15*time.Second + 15*time.Second + 30*time.Second)))
		Expect(src).To(HaveKeyWithValue("window", "scaledobject"))
	})

	It("lets measured releases set the hold, not a long grace that is rarely used", func() {
		l := NewShareLedger()
		tmp := ShareTimings{Window: time.Hour, ReleaseTimeout: time.Hour, FillTimeout: time.Hour}
		t0 := time.Unix(0, 0)
		for i := range 3 {
			held := map[string]int{"A": 4}
			start := t0.Add(time.Duration(i) * time.Hour)
			l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, Entitled: true}, held, start, tmp)
			held["A"] = 3
			l.Observe(held, start.Add(400*time.Second), tmp)
		}
		grace := 30 * time.Minute
		tm, src := DeriveShareTimings(ShareTimingInputs{Cycle: cycle, TerminationGrace: &grace}, l)
		Expect(src).To(HaveKeyWithValue("release", "measured"))
		Expect(tm.ReversalHold).To(Equal(800 * time.Second))
		// The timeout still allows for the configured grace: a release that
		// uses it must not be aborted.
		Expect(tm.ReleaseTimeout).To(BeNumerically(">", grace))
	})

	It("gives a gang-scheduled LWS more time to schedule", func() {
		plain, _ := DeriveShareTimings(ShareTimingInputs{Cycle: cycle}, nil)
		lws, _ := DeriveShareTimings(ShareTimingInputs{Cycle: cycle, LWS: true}, nil)
		Expect(lws.FillTimeout - plain.FillTimeout).To(Equal(time.Minute))
	})
})
