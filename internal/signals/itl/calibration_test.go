package itl

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("CalibrationRatio", func() {
	// Issue #270, the sg2 replica over 08:11-08:19, averaged over seven
	// one-minute intervals of its own counters: 190.86 requests running,
	// 3026.65 tokens/s, k 0.634. The line its ITL histogram produced is
	// A = 0.1025 at the pinned 6 ms intercept.
	const (
		kvTokens = 376480.0
		kvReq    = 1250.0 // 1000 in + 500 out / 2
		running  = 190.86
		gps      = 3026.65
		k        = 0.634
	)
	line := Model{A: 0.1025, B: DefaultBaselineSec}

	It("reproduces the run: the line reads 1.13x the time a sequence took per token", func() {
		// The sequence count is right: k x C / KVreq is the running count.
		Expect(Sequences(k, kvTokens, kvReq)).To(BeNumerically("~", running, 0.01*running))
		// The latency is not: running / GPS is what a token took.
		Expect(line.ITLAt(k) / (running / gps)).To(BeNumerically("~", 1.13, 0.01))

		ratio, ok := CalibrationRatio(gps, TokenRate(line, k, kvTokens, kvReq), k)
		Expect(ok).To(BeTrue())
		Expect(ratio).To(BeNumerically("~", 1.13, 0.01))
	})

	It("is no evidence where GPSErrorPct makes no comparison", func() {
		_, ok := CalibrationRatio(0, 2600, k)
		Expect(ok).To(BeFalse(), "no observed rate")
		_, ok = CalibrationRatio(gps, 0, k)
		Expect(ok).To(BeFalse(), "a line that prices nothing")
		_, ok = CalibrationRatio(gps, 2600, DefaultGPSMinKForVerification-0.01)
		Expect(ok).To(BeFalse(), "too few resident sequences")
	})

	It("refuses a ratio a bias cannot explain", func() {
		_, ok := CalibrationRatio(3*2600, 2600, k)
		Expect(ok).To(BeFalse(), "3x is a wrong line, itl-gps-mismatch reports it")
		_, ok = CalibrationRatio(2600/3, 2600, k)
		Expect(ok).To(BeFalse())
		r, ok := CalibrationRatio(DefaultMaxGPSCalibration*2600, 2600, k)
		Expect(ok).To(BeTrue(), "the bounds are inclusive")
		Expect(r).To(Equal(DefaultMaxGPSCalibration))
	})
})

var _ = Describe("Tracker calibration", func() {
	const cycle = 15 * time.Second
	var (
		t   *Tracker
		now time.Time
	)
	BeforeEach(func() {
		t = NewTracker()
		now = time.Date(2026, 10, 23, 8, 11, 0, 0, time.UTC)
	})
	row := func(pod string, k float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: "v", Ready: true, KvUsageInstant: k, AvgITL: 0.07}
	}
	observe := func(rows ...domain.ReplicaMetrics) {
		t.Observe("key", "v", rows, now)
		now = now.Add(cycle)
	}
	at := func() time.Time { return now.Add(-cycle) }

	It("takes the first ratio as it is and smooths the rest", func() {
		Expect(t.NoteGPSRatio("key", 1.2)).To(Equal(1.2))
		Expect(t.NoteGPSRatio("key", 1.0)).To(BeNumerically("~", 1.2-DefaultGPSCalibrationWeight*0.2, 1e-12))
		c, ok := t.GPSCalibration("key")
		Expect(ok).To(BeTrue())
		Expect(c).To(BeNumerically("~", 1.15, 1e-12))
		_, ok = t.GPSCalibration("other")
		Expect(ok).To(BeFalse())
	})

	It("is swept with the key", func() {
		t.NoteGPSRatio("key", 1.1)
		t.NoteGPSRatio("keep", 1.1)
		t.Sweep(now, func(k string) bool { return k == "key" })
		_, ok := t.GPSCalibration("key")
		Expect(ok).To(BeFalse())
		_, ok = t.GPSCalibration("keep")
		Expect(ok).To(BeTrue())
	})

	It("calls a replica settled only after DefaultSteadyFor of history", func() {
		// A replica present at the key's first Observe is steady for the full
		// fit on no history; it is not settled until it has some.
		observe(row("p", 0.64))
		Expect(t.Settled("key", "p", at())).To(BeFalse())
		for now.Sub(time.Date(2026, 10, 23, 8, 11, 0, 0, time.UTC)) <= DefaultSteadyFor {
			observe(row("p", 0.64))
		}
		Expect(t.Settled("key", "p", at())).To(BeTrue())
		Expect(t.Settled("key", "absent", at())).To(BeFalse())
		Expect(t.Settled("other", "p", at())).To(BeFalse())
	})

	It("does not call a moving replica settled", func() {
		for range 9 {
			observe(row("p", 0.64))
		}
		Expect(t.Settled("key", "p", at())).To(BeTrue())
		observe(row("p", 0.64+DefaultMaxKDrift+0.01))
		Expect(t.Settled("key", "p", at())).To(BeFalse(), "its k left the drift band within two minutes")
	})

	It("starts a replica over when it misses a cycle", func() {
		for range 9 {
			observe(row("p", 0.64))
		}
		observe(row("q", 0.64))
		observe(row("p", 0.64))
		Expect(t.Settled("key", "p", at())).To(BeFalse())
	})
})
