package saturation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
		h := NewSaturationAnalyzer(capacity.NewStore()).WithHorizons(capacity.Horizons{
			EpisodeGap: 2 * time.Hour, VariantTimeout: 72 * time.Hour, BucketRetention: 240 * time.Hour,
		}).Horizons()
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
		Entry("all zero, i.e. nothing configured", capacity.Horizons{},
			[]string{"episodeGap", "variantTimeout", "bucketRetention"}),
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
			a := NewSaturationAnalyzer(capacity.NewStore()).
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
			a := NewSaturationAnalyzer(capacity.NewStore()).
				WithHorizons(capacity.Horizons{
					EpisodeGap: time.Hour, VariantTimeout: 240 * time.Hour, BucketRetention: time.Hour,
				})
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
			ra.Add(50000)
			ra.TouchAt(time.Now().Add(-2 * time.Hour))
			a.computeCapacityHistory["hist-key"] = ra

			ev := a.EvictStaleHistory()

			Expect(a.computeCapacityHistory).To(BeEmpty(),
				"a one-hour retention must evict a two-hour-old window; the default is "+
					"seven days, so holding it would prove the knob unread")
			Expect(ev.K2History).To(Equal(1))
		})

		It("EpisodeGap decides when a new observation starts a fresh window", func() {
			a := NewSaturationAnalyzer(capacity.NewStore()).
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
