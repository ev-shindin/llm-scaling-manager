package slo

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// line is ITL(k) = 40 ms * k + 20 ms: 26 ms at the bottom of the fitted range,
// 44 ms at k = 0.6, 52 ms at the top.
var line = itl.Model{A: 0.040, B: 0.020}

var _ = Describe("priceTPOT", func() {
	It("inverts the line at the target", func() {
		p := priceTPOT(defaultBand, line, 0.044)
		Expect(p.ok).To(BeTrue())
		Expect(p.feasible).To(BeTrue())
		Expect(p.fill).To(BeNumerically("~", 0.6, 1e-12))
	})

	It("clamps a loose target to the top of the fitted range", func() {
		p := priceTPOT(defaultBand, line, 1.0)
		Expect(p.feasible).To(BeTrue())
		Expect(p.fill).To(Equal(defaultBand.hi))
	})

	It("prices just inside the bottom of the range", func() {
		p := priceTPOT(defaultBand, line, 0.027)
		Expect(p.feasible).To(BeTrue())
		Expect(p.fill).To(BeNumerically("~", 0.175, 1e-12))
	})

	It("is infeasible when the line exceeds the target at the bottom of its range", func() {
		p := priceTPOT(defaultBand, line, line.ITLAt(defaultBand.lo))
		Expect(p.ok).To(BeTrue())
		Expect(p.feasible).To(BeFalse())
	})

	DescribeTable("refuses an unusable input instead of pricing it",
		func(m itl.Model, target float64) {
			Expect(priceTPOT(defaultBand, m, target).ok).To(BeFalse())
		},
		Entry("a flat line", itl.Model{A: 0, B: 0.02}, 0.05),
		Entry("an inverted line", itl.Model{A: -0.01, B: 0.02}, 0.05),
		Entry("a NaN slope", itl.Model{A: math.NaN(), B: 0.02}, 0.05),
		Entry("an infinite intercept", itl.Model{A: 0.04, B: math.Inf(1)}, 0.05),
		Entry("a NaN target", line, math.NaN()),
		Entry("a zero target", line, 0.0),
	)

	// The negative control for the bound this replaced. A fill capped at the
	// highest fill observed plus a step ratchets down, because the fleet runs
	// below the fill it is priced at. The fill here depends on the line and the
	// target only, so a loose SLO prices at the top of the range on every cycle
	// and never below the saturation analyzer's default point.
	It("prices a loose SLO at least as high as the saturation analyzer's default point", func() {
		const saturationPricingK = 0.80 * 0.85
		loose := priceTPOT(defaultBand, line, 1.0)
		Expect(loose.fill).To(BeNumerically(">=", saturationPricingK))
		Expect(replicaTokenRate(line, loose.fill, 1e6, 4000, 0)).
			To(BeNumerically(">=", replicaTokenRate(line, saturationPricingK, 1e6, 4000, 0)))
	})
})

var _ = Describe("replicaTokenRate", func() {
	It("is the resident sequences over the ITL at k", func() {
		// 0.6 * 100000 / 500 = 120 sequences, one token each per 44 ms.
		Expect(replicaTokenRate(line, 0.6, 100000, 500, 0)).To(BeNumerically("~", 120/0.044, 1e-9))
	})

	It("caps the sequences at the engine's max_num_seqs", func() {
		Expect(replicaTokenRate(line, 0.6, 100000, 500, 64)).To(BeNumerically("~", 64/0.044, 1e-9))
	})

	It("is zero when an input is unusable", func() {
		Expect(replicaTokenRate(line, 0.6, 0, 500, 0)).To(BeZero())
		Expect(replicaTokenRate(line, 0.6, 100000, 0, 0)).To(BeZero())
		Expect(replicaTokenRate(itl.Model{A: math.NaN()}, 0.6, 100000, 500, 0)).To(BeZero())
	})
})

var _ = Describe("realisedFill", func() {
	It("finds the fill that carries scaleUp times the priced rate, below the priced fill", func() {
		k := realisedFill(defaultBand, line, 0.6, 0.85, 100000, 500, 0)
		Expect(k).To(BeNumerically("<", 0.6))
		Expect(replicaTokenRate(line, k, 100000, 500, 0)).
			To(BeNumerically("~", 0.85*replicaTokenRate(line, 0.6, 100000, 500, 0), 1e-6))
	})

	It("is the priced fill when there is no headroom", func() {
		Expect(realisedFill(defaultBand, line, 0.6, 1, 100000, 500, 0)).To(Equal(0.6))
	})

	It("is NaN when the priced rate is unusable", func() {
		Expect(math.IsNaN(realisedFill(defaultBand, line, 0.6, 0.85, 0, 500, 0))).To(BeTrue())
	})
})

var _ = Describe("reachableFill", func() {
	It("is k when the engine admits every sequence k implies", func() {
		Expect(reachableFill(defaultBand, 0.6, 100000, 500, 256)).To(Equal(0.6))
		Expect(reachableFill(defaultBand, 0.6, 100000, 500, 0)).To(Equal(0.6))
	})

	It("is the fill max_num_seqs stops at when it binds", func() {
		Expect(reachableFill(defaultBand, 0.6, 100000, 500, 64)).To(BeNumerically("~", 0.32, 1e-12))
	})

	It("never goes below the fitted range", func() {
		Expect(reachableFill(defaultBand, 0.6, 100000, 500, 8)).To(Equal(defaultBand.lo))
	})
})

var _ = Describe("realisedFill below the fitted range", func() {
	It("reports the bottom of the range rather than extrapolating under it", func() {
		// A very low headroom drives the realised fill under 0.15.
		Expect(realisedFill(defaultBand, line, 0.2, 0.3, 100000, 500, 0)).To(Equal(defaultBand.lo))
	})
})
