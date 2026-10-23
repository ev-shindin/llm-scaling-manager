package itl

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("Tracker", func() {
	var (
		t   *Tracker
		now time.Time
	)
	BeforeEach(func() {
		t = NewTracker()
		now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	})
	row := func(pod string, k, itl float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: "v", Ready: true, KvUsageInstant: k, AvgITL: itl}
	}
	// onLine is n readings on ITL = 0.02*k + 0.01, k spread from lo to hi.
	onLine := func(n int, lo, hi float64) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, n)
		for i := range n {
			k := lo + (hi-lo)*float64(i)/float64(n-1)
			rows[i] = row("p", k, 0.02*k+0.01)
		}
		return rows
	}

	It("admits a row only when it is a reading, and counts each reason it turned one away", func() {
		r := t.Observe("key", "v", []domain.ReplicaMetrics{
			row("ok", 0.5, 0.02),
			{PodName: "other", VariantName: "w", Ready: true, KvUsageInstant: 0.5, AvgITL: 0.02},
			{PodName: "starting", VariantName: "v", KvUsageInstant: 0.5, AvgITL: 0.02},
			{PodName: "pool", VariantName: "v", Ready: true, FromWarmPool: true, KvUsageInstant: 0.5, AvgITL: 0.02},
			row("no-itl", 0.5, 0),
			row("inf-itl", 0.5, math.Inf(1)),
			row("no-k", 0, 0.02),
			row("above", 0.9, 0.02),
			row("below", 0.05, 0.02),
			row("below-too", 0.10, 0.02),
			row("nan-k", math.NaN(), 0.02),
			row("nan-itl", 0.5, math.NaN()),
		}, now)
		Expect(r.Considered).To(Equal(11), "another variant's row is not considered at all")
		Expect(r.NotReady).To(Equal(2))
		Expect(r.NoITL).To(Equal(3), "an infinite or NaN ITL is not a reading either")
		Expect(r.NoK).To(Equal(2), "nor is a NaN fill")
		Expect(r.AboveBand).To(Equal(1))
		// Counts that differ, so a swap of the two shows.
		Expect(r.Offered).To(Equal(1), "the reading the window kept")
		Expect(r.BelowBand).To(Equal(2), "the two below the band, which the window itself drops")
		Expect(r.Held).To(Equal(1))
		Expect(r.Tier).To(BeEmpty())
	})

	It("fits the full line once the window spans enough of k, and remembers its B", func() {
		r := t.Observe("key", "v", onLine(10, 0.2, 0.7), now)
		Expect(r.Ready).To(BeTrue())
		Expect(r.Tier).To(Equal(TierOLS))
		Expect(r.Model.A).To(BeNumerically("~", 0.02, 1e-9))
		Expect(r.Model.B).To(BeNumerically("~", 0.01, 1e-9))
		b, ok := t.Baseline("key")
		Expect(ok).To(BeTrue())
		Expect(b).To(BeNumerically("~", 0.01, 1e-9))
	})

	It("does not remember a B that is not a physical floor", func() {
		rows := make([]domain.ReplicaMetrics, 10)
		for i := range rows {
			k := 0.2 + 0.05*float64(i)
			rows[i] = row("p", k, 0.05*k-0.005) // B < 0, positive at saturation
		}
		r := t.Observe("key", "v", rows, now)
		Expect(r.Tier).To(Equal(TierOLS))
		Expect(r.Model.B).To(BeNumerically("<", 0))
		_, ok := t.Baseline("key")
		Expect(ok).To(BeFalse())
	})

	It("pins B to the key's own last full fit when the spread is short", func() {
		t.Observe("key", "v", onLine(10, 0.2, 0.7), now)
		r := t.Observe("key", "v", onLine(10, 0.40, 0.45), now.Add(time.Hour))
		Expect(r.Ready).To(BeFalse(), "the old readings aged out; the new ones span 0.05")
		Expect(r.Tier).To(Equal(TierPinnedB))
		Expect(r.BaselineStored).To(BeTrue())
		Expect(r.BaselineLearned).To(BeTrue())
		Expect(r.Model.B).To(BeNumerically("~", 0.01, 1e-9))
	})

	It("pins B to the default before the key has a full fit of its own", func() {
		r := t.Observe("key", "v", onLine(10, 0.40, 0.45), now)
		Expect(r.Tier).To(Equal(TierPinnedB))
		Expect(r.BaselineStored).To(BeFalse())
		Expect(r.BaselineLearned).To(BeFalse())
		Expect(r.Model.B).To(Equal(DefaultBaselineSec))
	})

	It("treats a stored intercept that is not positive as no intercept", func() {
		t.SetBaseline("key", 0)
		r := t.Observe("key", "v", onLine(10, 0.40, 0.45), now)
		Expect(r.BaselineStored).To(BeTrue())
		Expect(r.BaselineLearned).To(BeFalse())
		Expect(r.Model.B).To(Equal(DefaultBaselineSec))
	})

	It("fits nothing from fewer than the minimum samples, spread or not", func() {
		r := t.Observe("key", "v", onLine(DefaultMinSamples-1, 0.40, 0.45), now)
		Expect(r.Tier).To(BeEmpty())
		Expect(r.Declined).To(BeFalse())
		Expect(r.Model.IsZero()).To(BeTrue())
	})

	It("declines when the pinned fit refuses the readings", func() {
		rows := make([]domain.ReplicaMetrics, 10)
		for i := range rows {
			rows[i] = row("p", 0.40+0.005*float64(i), DefaultBaselineSec/2) // below the floor: A < 0
		}
		r := t.Observe("key", "v", rows, now)
		Expect(r.Tier).To(BeEmpty())
		Expect(r.Declined).To(BeTrue())
		Expect(r.Model.IsZero()).To(BeTrue())
	})

	It("keeps each key's window and intercept apart", func() {
		t.Observe("a", "v", onLine(10, 0.2, 0.7), now)
		t.Observe("b", "v", onLine(3, 0.2, 0.7), now)
		Expect(t.WindowLens()).To(Equal(map[string]int{"a": 10, "b": 3}))
		Expect(t.Baselines()).To(HaveKey("a"))
		Expect(t.Baselines()).NotTo(HaveKey("b"))
	})

	It("sweeps what the caller calls stale, windows and intercepts apart", func() {
		t.Observe("live", "v", onLine(10, 0.2, 0.7), now)
		t.Observe("gone", "v", onLine(10, 0.2, 0.7), now)
		t.RestoreBaseline("restored-gone", 0.01) // an intercept with no window
		w, b := t.Sweep(now, func(key string) bool { return key != "live" })
		Expect(w).To(Equal(1))
		Expect(b).To(Equal(2))
		Expect(t.WindowLens()).To(HaveKey("live"))
		Expect(t.Baselines()).To(HaveKey("live"))
		Expect(t.Baselines()).To(HaveLen(1))
	})

	It("prunes aged readings on a sweep even from keys it keeps", func() {
		t.Observe("live", "v", onLine(10, 0.2, 0.7), now)
		t.Sweep(now.Add(DefaultObservationMaxAge+time.Minute), func(string) bool { return false })
		Expect(t.WindowLens()["live"]).To(BeZero())
	})

	It("restores an intercept only where none was learned since", func() {
		t.Observe("key", "v", onLine(10, 0.2, 0.7), now)
		t.RestoreBaseline("key", 0.5)
		t.RestoreBaseline("new", 0.5)
		b, _ := t.Baseline("key")
		Expect(b).To(BeNumerically("~", 0.01, 1e-9))
		b, _ = t.Baseline("new")
		Expect(b).To(Equal(0.5))
	})

	It("prices a saturated fleet from its replicas at work, not from their first readings", func() {
		// One namespace of a two-namespace run (Qwen3-32B-FP8 on H200, vLLM),
		// the 14 requests/s phase: one replica from idle, two more joining at
		// 14:19:42. Every replica sat above the band but for a dip now and
		// then, and the one there from the start served 5.9 requests/s. The
		// window's first reading is the load arriving at an idle replica (k
		// 0.285 at 268 ms), and a joiner's first is k 0.507 at 503 ms. Least
		// squares took both and the run
		// logged A = 0.162 at 14:21:28, a derived mu of 3.55 requests/s.
		//
		// k is the instantaneous fill where the run logged it (the last
		// cycle), and otherwise the minute's maximum, held to 0.790 on the
		// cycles the run offered a reading at and the maximum was above the
		// band. A reading without k or ITL is the replica idle or starting.
		type cell struct {
			pod     string
			k, itlS float64
		}
		cycles := []struct {
			at    string
			cells []cell
		}{
			{"14:12:53", []cell{{"wnx8v", 0, 0}}},
			{"14:13:23", []cell{{"wnx8v", 0, 0}}},
			{"14:13:53", []cell{{"wnx8v", 0, 0}}},
			{"14:14:23", []cell{{"wnx8v", 0, 0}}},
			{"14:14:54", []cell{{"wnx8v", 0, 0}}},
			{"14:15:09", []cell{{"wnx8v", 0.285, 0.2675}}},
			{"14:15:24", []cell{{"wnx8v", 0.860, 0.0727}}},
			{"14:15:39", []cell{{"wnx8v", 0.966, 0.0630}}},
			{"14:15:54", []cell{{"wnx8v", 0.790, 0.0870}}},
			{"14:16:09", []cell{{"wnx8v", 0.966, 0.0713}}},
			{"14:16:24", []cell{{"wnx8v", 0.966, 0.0896}}},
			{"14:16:39", []cell{{"wnx8v", 0.790, 0.0886}}},
			{"14:16:54", []cell{{"wnx8v", 0.947, 0.0733}}},
			{"14:17:09", []cell{{"wnx8v", 0.947, 0.0882}}},
			{"14:17:25", []cell{{"wnx8v", 0.947, 0.0742}}},
			{"14:17:40", []cell{{"wnx8v", 0.947, 0.0738}}},
			{"14:17:55", []cell{{"wnx8v", 0.790, 0.0882}}},
			{"14:18:11", []cell{{"wnx8v", 0.935, 0.0770}}},
			{"14:18:26", []cell{{"wnx8v", 0.935, 0.0864}}},
			{"14:18:41", []cell{{"wnx8v", 0.790, 0.0882}}},
			{"14:18:57", []cell{{"wnx8v", 0.926, 0.0735}}},
			{"14:19:12", []cell{{"wnx8v", 0.926, 0.0848}}},
			{"14:19:27", []cell{{"wnx8v", 0.923, 0.0805}}},
			{"14:19:42", []cell{{"s92zt", 0.156, 0}, {"wnx8v", 0.923, 0.0729}}},
			{"14:19:57", []cell{{"s92zt", 0.810, 0.0842}, {"wnx8v", 0.919, 0.0871}, {"bpxrs", 0.091, 0}}},
			{"14:20:12", []cell{{"s92zt", 0.952, 0.0622}, {"wnx8v", 0.919, 0.0804}, {"bpxrs", 0.507, 0.5030}}},
			{"14:20:27", []cell{{"s92zt", 0.952, 0.0838}, {"wnx8v", 0.790, 0.0831}, {"bpxrs", 0.932, 0.0644}}},
			{"14:20:42", []cell{{"s92zt", 0.952, 0.0763}, {"wnx8v", 0.899, 0.0864}, {"bpxrs", 0.932, 0.0691}}},
			{"14:20:57", []cell{{"s92zt", 0.953, 0.0843}, {"wnx8v", 0.790, 0.0734}, {"bpxrs", 0.932, 0.0836}}},
			{"14:21:12", []cell{{"s92zt", 0.953, 0.0917}, {"wnx8v", 0.790, 0.0822}, {"bpxrs", 0.932, 0.0690}}},
			{"14:21:28", []cell{{"s92zt", 0.937, 0.0701}, {"wnx8v", 0.799, 0.0831}, {"bpxrs", 0.801, 0.0905}}},
		}
		var r Reading
		for _, c := range cycles {
			at, err := time.Parse(time.TimeOnly, c.at)
			Expect(err).NotTo(HaveOccurred())
			rows := make([]domain.ReplicaMetrics, 0, len(c.cells))
			for _, x := range c.cells {
				rows = append(rows, row(x.pod, x.k, x.itlS))
			}
			offset := time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute +
				time.Duration(at.Second())*time.Second
			r = t.Observe("key", "v", rows, now.Add(offset))
		}
		Expect(r.Held).To(Equal(10), "the run's window held 10 on this cycle too")
		Expect(r.Tier).To(Equal(TierPinnedB))
		// The dips' own slopes through the 6 ms floor are 0.085 to 0.105.
		Expect(r.Model.A).To(BeNumerically("~", 0.100, 0.005))
		Expect(r.SteadyHeld).To(Equal(1), "only the first reading, of a replica present from the start")
		Expect(r.SteadySpread).To(BeZero())
	})

	It("forgets intercepts and keeps the readings", func() {
		t.Observe("key", "v", onLine(10, 0.2, 0.7), now)
		t.ForgetBaselines()
		Expect(t.Baselines()).To(BeEmpty())
		Expect(t.WindowLens()["key"]).To(Equal(10))
	})
})
