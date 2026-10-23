package itl

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FitPinnedB", func() {
	It("recovers the slope when the points are on a line through the pinned B", func() {
		want := Model{A: 0.0307, B: DefaultBaselineSec}
		obs := make([]Observation, 0, 5)
		for _, k := range []float64{0.2, 0.35, 0.5, 0.65, 0.8} {
			obs = append(obs, Observation{K: k, ITLSec: want.ITLAt(k)})
		}
		got, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", want.A, 1e-9))
		Expect(got.B).To(Equal(DefaultBaselineSec))
	})

	It("fits a fleet whose replicas are all at the same load", func() {
		// The case Fit refuses and Window.Ready is built to detect: no spread
		// in k at all. One parameter is still determined by it.
		want := Model{A: 0.04, B: DefaultBaselineSec}
		obs := []Observation{
			{K: 0.62, ITLSec: want.ITLAt(0.62)},
			{K: 0.62, ITLSec: want.ITLAt(0.62)},
			{K: 0.62, ITLSec: want.ITLAt(0.62)},
		}
		got, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", want.A, 1e-9))

		By("where the two-parameter fit has nothing to work with")
		_, twoParam := Fit(obs)
		Expect(twoParam).To(BeFalse())
	})

	It("ignores readings that are missing either half of the pair", func() {
		want := Model{A: 0.03, B: DefaultBaselineSec}
		obs := []Observation{
			{K: 0.4, ITLSec: want.ITLAt(0.4)},
			{K: 0, ITLSec: 0.02},
			{K: 0.5, ITLSec: 0},
			{K: 0.6, ITLSec: want.ITLAt(0.6)},
		}
		got, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", want.A, 1e-9))
	})

	It("is the k-weighted mean of the readings' slopes", func() {
		// Slopes 0.1, 0.2, 0.9 through B, weighted by k 0.5, 0.3, 0.3: the
		// latency above the floor, 0.05 + 0.06 + 0.27, over the load, 1.1.
		// Nothing is trimmed, the 0.9 included.
		b := DefaultBaselineSec
		obs := []Observation{
			{K: 0.5, ITLSec: b + 0.1*0.5},
			{K: 0.3, ITLSec: b + 0.2*0.3},
			{K: 0.3, ITLSec: b + 0.9*0.3},
		}
		got, ok := FitPinnedB(obs, b)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", (0.5*0.1+0.3*0.2+0.3*0.9)/1.1, 1e-12))

		By("the mean of two slopes of equal weight")
		got, ok = FitPinnedB([]Observation{obs[0], {K: 0.5, ITLSec: b + 0.3*0.5}}, b)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", 0.2, 1e-12))

		By("the one slope when there is one")
		got, ok = FitPinnedB(obs[:1:1], b)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", 0.1, 1e-12))
	})

	It("moves with the weight between two groups of slopes rather than jumping", func() {
		// Two replicas at the same fill serving two shapes. The weighted
		// median answered 0.08 or 0.12, whichever carried more than half.
		b := DefaultBaselineSec
		pair := func(kSlow, kFast float64) []Observation {
			return []Observation{{K: kFast, ITLSec: b + 0.08*kFast}, {K: kSlow, ITLSec: b + 0.12*kSlow}}
		}
		lighter, ok := FitPinnedB(pair(0.59, 0.61), b)
		Expect(ok).To(BeTrue())
		heavier, ok := FitPinnedB(pair(0.61, 0.59), b)
		Expect(ok).To(BeTrue())
		Expect(lighter.A).To(BeNumerically("~", (0.59*0.12+0.61*0.08)/1.2, 1e-12))
		Expect(heavier.A).To(BeNumerically("~", (0.61*0.12+0.59*0.08)/1.2, 1e-12))
	})

	// The round-3 statistics review's probe: one replica serving a heavier
	// shape (slope 0.12) among replicas at 0.08, all at the same fill. Its
	// readings are as real as theirs, and the fleet's capacity is the blend.
	// The interquartile mean set the slow replica aside entirely from four
	// replicas up -- 0.080 against a blend of 0.090 for four, priced capacity
	// 8.5% high -- and the weighted median did from two.
	DescribeTable("blends one slower replica in whole, whatever the fleet's size",
		func(replicas int) {
			b := DefaultBaselineSec
			obs := make([]Observation, 0, 4*replicas)
			for range 4 {
				obs = append(obs, Observation{K: 0.6, ITLSec: b + 0.12*0.6})
				for range replicas - 1 {
					obs = append(obs, Observation{K: 0.6, ITLSec: b + 0.08*0.6})
				}
			}
			got, ok := FitPinnedB(obs, b)
			Expect(ok).To(BeTrue())
			blend := (0.12 + 0.08*float64(replicas-1)) / float64(replicas)
			Expect(got.A).To(BeNumerically("~", blend, 1e-12))
		},
		Entry("1 of 2", 2), Entry("1 of 3", 3), Entry("1 of 4", 4), Entry("1 of 5", 5), Entry("1 of 8", 8),
	)

	It("refuses a fit that says load makes a replica faster", func() {
		// A negative slope is measurement noise, not a model: more cache in use
		// cannot speed a replica up, and a model that says so would price an
		// idle fleet as faster than a busy one.
		// The residual has to be negative where it is weighted most, i.e. the
		// busy replica must read FASTER than an idle one: 2 ms at k=0.8
		// against a 6 ms floor.
		obs := []Observation{
			{K: 0.2, ITLSec: 0.0065},
			{K: 0.8, ITLSec: 0.002},
		}
		_, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeFalse())
	})

	It("declines rather than guessing", func() {
		_, ok := FitPinnedB(nil, DefaultBaselineSec)
		Expect(ok).To(BeFalse(), "no observations")

		_, ok = FitPinnedB([]Observation{{K: 0.5, ITLSec: 0.02}}, 0)
		Expect(ok).To(BeFalse(), "no baseline to pin to")

		_, ok = FitPinnedB([]Observation{{K: 0, ITLSec: 0}}, DefaultBaselineSec)
		Expect(ok).To(BeFalse(), "nothing usable in the observations")

		_, ok = FitPinnedB([]Observation{{K: math.NaN(), ITLSec: 0.02}}, DefaultBaselineSec)
		Expect(ok).To(BeFalse(), "a NaN load places nothing")
	})
})
