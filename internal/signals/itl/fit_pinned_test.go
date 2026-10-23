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

	It("sets aside a new replica's first readings", func() {
		// One namespace's window on a two-namespace run (Qwen3-32B-FP8 on
		// H200, 14 requests/s over three replicas), 14:24 to 14:35. Three
		// readings are a replica's first after it began serving -- one the load
		// came back to, two that had just joined -- at about 0.46 of k and
		// 435 to 496 ms. The others are the replicas at work, at slopes of 0.09
		// to 0.13 through the 6 ms floor; each served 6.1 to 6.2 requests/s.
		// k is the instantaneous fill where the run logged it, and the
		// minute's maximum held to the band (0.790) where it did not.
		obs := []Observation{
			{K: 0.471, ITLSec: 0.4355},
			{K: 0.790, ITLSec: 0.0795},
			{K: 0.790, ITLSec: 0.0903},
			{K: 0.790, ITLSec: 0.0824},
			{K: 0.790, ITLSec: 0.0850},
			{K: 0.790, ITLSec: 0.0852},
			{K: 0.790, ITLSec: 0.0850},
			{K: 0.790, ITLSec: 0.0864},
			{K: 0.790, ITLSec: 0.0863},
			{K: 0.798, ITLSec: 0.0857},
			{K: 0.463, ITLSec: 0.4904},
			{K: 0.766, ITLSec: 0.1036},
			{K: 0.463, ITLSec: 0.4957},
			{K: 0.794, ITLSec: 0.0806},
			{K: 0.753, ITLSec: 0.0859},
			{K: 0.760, ITLSec: 0.0830},
		}
		got, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeTrue())
		// Least squares answered 0.170 here, and the derived mu 3.3 requests/s.
		Expect(got.A).To(BeNumerically("~", 0.100, 0.003),
			"the slope of the thirteen readings of replicas at work")
	})

	It("is the weighted median of the readings' slopes", func() {
		// Slopes 0.1, 0.2, 0.9 through B, weighted by k 0.5, 0.3, 0.3: the
		// first carries 0.5 of 1.1, short of half, so the median is the second.
		b := DefaultBaselineSec
		obs := []Observation{
			{K: 0.5, ITLSec: b + 0.1*0.5},
			{K: 0.3, ITLSec: b + 0.2*0.3},
			{K: 0.3, ITLSec: b + 0.9*0.3},
		}
		got, ok := FitPinnedB(obs, b)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", 0.2, 1e-12))

		By("the first slope alone when it carries more than half")
		got, ok = FitPinnedB(obs[:2:2], b)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", 0.1, 1e-12), "0.5 of 0.8")

		By("and the mean of the two slopes the halves meet between")
		got, ok = FitPinnedB([]Observation{obs[0], {K: 0.5, ITLSec: b + 0.3*0.5}}, b)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", 0.2, 1e-12))
	})

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
