package saturation

import (
	"time"

	"github.com/go-logr/logr"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The horizons, and whether configuring them reaches anything.
//
// The last point is the one that matters. This project has shipped a knob that
// reached nothing at all -- LOG_LEVEL/WVA_LOG_LEVEL is read by no code and
// changes no behaviour -- and an inert knob is worse than no knob, because an
// operator who sets it believes they have changed something. So these specs do
// not stop at "the field holds the value I set": each one drives the code the
// horizon governs and asserts the horizon decided the outcome.
var _ = Describe("learned-state horizons", func() {
	It("defaults to the shipped values when nothing configures them", func() {
		h := NewSaturationAnalyzer(capacity.NewStore()).Horizons()
		Expect(h).To(Equal(capacity.DefaultHorizons()))
		Expect(h.EpisodeGap).To(Equal(24 * time.Hour))
		Expect(h.VariantTimeout).To(Equal(24 * time.Hour))
		Expect(h.BucketRetention).To(Equal(7 * 24 * time.Hour))
	})

	It("takes the values it is configured with", func() {
		a, replaced := NewSaturationAnalyzer(capacity.NewStore()).WithHorizons(capacity.Horizons{
			EpisodeGap: 2 * time.Hour, VariantTimeout: 72 * time.Hour, BucketRetention: 240 * time.Hour,
		})
		Expect(replaced).To(BeEmpty(), "all three are usable, so nothing should be reported replaced")
		h := a.Horizons()
		Expect(h.EpisodeGap).To(Equal(2 * time.Hour))
		Expect(h.VariantTimeout).To(Equal(72*time.Hour),
			"72h is the value a reviewer asked for, to clear a long weekend")
		Expect(h.BucketRetention).To(Equal(240 * time.Hour))
	})

	DescribeTable("replaces an unusable value field by field rather than refusing",
		func(given capacity.Horizons, wantFixed []string) {
			got, fixed := given.Sanitized()
			Expect(fixed).To(ConsistOf(wantFixed))
			d := capacity.DefaultHorizons()
			// Every field named in `fixed` is back at its default; the others
			// kept what was asked for.
			for _, f := range wantFixed {
				switch f {
				case "episodeGap":
					Expect(got.EpisodeGap).To(Equal(d.EpisodeGap))
				case "variantTimeout":
					Expect(got.VariantTimeout).To(Equal(d.VariantTimeout))
				case "bucketRetention":
					Expect(got.BucketRetention).To(Equal(d.BucketRetention))
				}
			}
		},
		Entry("all zero, i.e. nothing configured: defaults, nothing reported", capacity.Horizons{},
			[]string{}),
		Entry("one unset, one unusable: only the unusable one is reported",
			capacity.Horizons{EpisodeGap: 0, VariantTimeout: time.Second, BucketRetention: time.Hour},
			[]string{"variantTimeout"}),
		Entry("negative", capacity.Horizons{EpisodeGap: -time.Hour, VariantTimeout: time.Hour, BucketRetention: time.Hour},
			[]string{"episodeGap"}),
		Entry("a unit-less value, which viper parses as nanoseconds",
			capacity.Horizons{EpisodeGap: 24, VariantTimeout: time.Hour, BucketRetention: time.Hour},
			[]string{"episodeGap"}),
		Entry("below the floor", capacity.Horizons{EpisodeGap: time.Second, VariantTimeout: time.Second, BucketRetention: time.Second},
			[]string{"episodeGap", "variantTimeout", "bucketRetention"}),
		Entry("all usable", capacity.Horizons{EpisodeGap: time.Hour, VariantTimeout: time.Hour, BucketRetention: time.Hour},
			[]string{}),
	)

	// NOT INERT. Three specs, one per horizon, each driving the code it
	// governs at a value no default would produce.
	Describe("configuring a horizon changes what the code does", func() {
		It("VariantTimeout decides when per-variant state is swept", func() {
			a, _ := NewSaturationAnalyzer(capacity.NewStore()).
				WithHorizons(capacity.Horizons{
					EpisodeGap: time.Hour, VariantTimeout: time.Hour, BucketRetention: 240 * time.Hour,
				})
			base := time.Now()
			a.now = func() time.Time { return base }
			key := "ns|m|v1|NVIDIA-H200|1"
			a.startSeconds[key] = 70
			a.variantSeenAt[key] = base

			// Two hours on. Past the configured hour, far inside the default day.
			a.now = func() time.Time { return base.Add(2 * time.Hour) }
			ev := a.EvictStaleHistory()

			Expect(a.startSeconds).To(BeEmpty(),
				"a one-hour timeout must evict at two hours; still holding it would mean "+
					"the sweep is reading the 24h default and the knob is inert")
			Expect(ev.StartEstimates).To(Equal(1))
		})

		It("BucketRetention decides when a bucket-keyed window is swept", func() {
			a, _ := NewSaturationAnalyzer(capacity.NewStore()).
				WithHorizons(capacity.Horizons{
					EpisodeGap: time.Hour, VariantTimeout: 240 * time.Hour, BucketRetention: time.Hour,
				})
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			ra.AddAt(50000, time.Now())
			ra.TouchAt(time.Now().Add(-2 * time.Hour))
			a.computeCapacityHistory["hist-key"] = ra

			ev := a.EvictStaleHistory()

			Expect(a.computeCapacityHistory).To(BeEmpty(),
				"a one-hour retention must evict a two-hour-old window; the default is "+
					"seven days, so holding it would prove the knob unread")
			Expect(ev.K2History).To(Equal(1))
		})

		It("EpisodeGap decides when a new observation starts a fresh window", func() {
			a, _ := NewSaturationAnalyzer(capacity.NewStore()).
				WithHorizons(capacity.Horizons{
					EpisodeGap: time.Hour, VariantTimeout: 240 * time.Hour, BucketRetention: 240 * time.Hour,
				})
			key := "m|NVIDIA-H200|1|decode|in1k|medium|q10"
			base := time.Now()
			for i := 0; i < 5; i++ {
				at := base.Add(time.Duration(i) * 2 * ThroughputSampleSpacing)
				a.now = func() time.Time { return at }
				a.recordSaturatedThroughput(key, 36+float64(i))
			}
			Expect(a.saturatedThroughput[key].Len()).To(Equal(5), "precondition")

			// Two hours since the last write: past the configured gap, well
			// inside the 24h default.
			a.saturatedThroughput[key].ObservedAt(base.Add(-2 * time.Hour))
			a.now = func() time.Time { return base.Add(time.Hour) }
			a.recordSaturatedThroughput(key, 5)

			Expect(a.saturatedThroughput[key].Len()).To(Equal(1),
				"a one-hour episode gap must start a new window after two hours; blending "+
					"would mean the write path is reading the 24h default")
		})
	})

	// The replaced-field list is the whole reason Sanitized returns two values.
	// An earlier revision discarded it inside WithHorizons while five comments
	// and the operator reference claimed it was logged, and no spec noticed --
	// because after flooring the surviving value IS the default, so nothing
	// about the result distinguishes "set deliberately" from "rejected".
	Describe("reporting what it had to replace", func() {
		It("names the unusable field and leaves the usable ones alone", func() {
			a, replaced := NewSaturationAnalyzer(capacity.NewStore()).
				WithHorizons(capacity.Horizons{
					// 24 is what viper returns for a unit-less "24": 24ns.
					EpisodeGap: 24, VariantTimeout: 72 * time.Hour, BucketRetention: 240 * time.Hour,
				})
			Expect(replaced).To(ConsistOf("episodeGap"),
				"the caller cannot report what it is not told")
			h := a.Horizons()
			Expect(h.EpisodeGap).To(Equal(capacity.DefaultHorizons().EpisodeGap))
			Expect(h.VariantTimeout).To(Equal(72 * time.Hour))
			Expect(h.BucketRetention).To(Equal(240 * time.Hour))
		})

		// Issue #319: config reads an unset LEARNED_STATE_* variable as 0, and
		// naming those made every controller started with default settings log
		// "Unusable learned-state horizon" at ERROR.
		It("names nothing and uses the defaults when nothing is configured", func() {
			a, replaced := NewSaturationAnalyzer(capacity.NewStore()).
				WithHorizons(capacity.Horizons{})
			Expect(replaced).To(BeEmpty(),
				"unset is not unusable: an empty config must not produce the startup error")
			Expect(a.Horizons()).To(Equal(capacity.DefaultHorizons()))
		})
	})

	// NOT INERT, second call site. The EpisodeGap spec above drives
	// recordSaturatedThroughput; computeK2 reads the SAME field independently,
	// and nothing covered it. Because the default equals the constant the line
	// used to read, reverting replica_capacity.go to `capacity.EpisodeGap` --
	// a plausible bad merge -- left every spec in this file green.
	It("EpisodeGap decides when computeK2 starts a fresh k2 window", func() {
		const historyKey = "ep-gap-k2"
		a, _ := NewSaturationAnalyzer(capacity.NewStore()).
			WithHorizons(capacity.Horizons{
				EpisodeGap: time.Hour, VariantTimeout: 240 * time.Hour, BucketRetention: 240 * time.Hour,
			})
		// A window holding one old measurement, last WRITTEN two hours ago:
		// past the configured hour, far inside the 24h default.
		ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
		ra.AddAt(50000, time.Now())
		ra.ObservedAt(time.Now().Add(-2 * time.Hour))
		a.computeCapacityHistory[historyKey] = ra

		// A saturated queue, so Priority 1 fires and takes the reset branch.
		saturated := func() k2Decision {
			return a.computeK2(historyKey, "m", "ns", "decode-v",
				20,     // queueLen, above the threshold below
				900000, // tokensInUse: this episode's reading
				600,    // avgOutput
				1000,   // avgInput
				10,     // queueThreshold
				nil,    // engineParams
				pricedBatchCap{},
				1,       // ranks
				2000000, // k1
				2000000, // kvCeiling, at or above tokensInUse or the read is refused
				domain.RoleDecode, false, admission{}, false, logr.Discard())
		}
		// The window the reading would start waits for a later cycle's
		// (k2_confirm.go): the first is held, and the old one still prices.
		d := saturated()
		Expect(d.refused).To(Equal(k2ReasonObsUnconfirmed))
		Expect(d.k2).To(Equal(int64(50000)))
		a.analyzeSeq++ // the next cycle
		d = saturated()
		k2, src := d.k2, d.src

		Expect(src).To(Equal(capacity.K2SrcObserved))
		Expect(k2).To(Equal(int64(900000)),
			"a one-hour episode gap must RESET the window after two hours, so k2 is "+
				"this episode's readings alone; blending with the retained 50000 would "+
				"read 616667, and that is what reading the 24h default would produce")
		Expect(a.computeCapacityHistory[historyKey].Len()).To(Equal(2),
			"a reset window holds exactly the two new samples")
	})
})

// horizonsForTest sets every horizon to d, writing the field directly rather
// than going through WithHorizons.
//
// Deliberately bypasses Sanitized, because two specs use 0 -- "evict
// everything" -- which is exactly the value production refuses, and because
// most specs want one short horizon rather than three realistic ones. A spec
// that cares which of the three it is setting should set the field itself.
func horizonsForTest(a *SaturationAnalyzer, d time.Duration) {
	a.horizons = capacity.Horizons{EpisodeGap: d, VariantTimeout: d, BucketRetention: d}
}
