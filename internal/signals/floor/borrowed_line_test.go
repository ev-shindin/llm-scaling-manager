package floor

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// A derived service rate needs no sample count, because it is priced for the
// shape arriving now. That licence belongs to a line the variant fitted from
// its OWN readings.
//
// A line BORROWED from a sibling that merely shares an engine configuration is
// evidence about that configuration, not about this variant's load: the same
// line at the same k implies a different service rate for two variants serving
// different request shapes, because the resident sequence count is k*C/KVreq
// and KVreq is the shape. So such a figure may hold a fleet and must not grow
// one -- the same rule this file already applies to a reading borrowed from a
// neighbouring shape bucket.
var _ = Describe("Estimate with a borrowed ITL line", func() {
	variants := func(ready int) []domain.VariantCapacity {
		return []domain.VariantCapacity{{
			VariantName: "v", Role: domain.RoleDecode, ReplicaCount: ready,
			PerReplicaCapacity: float64(runK1),
		}}
	}

	// A derived figure carries no samples: that is the whole point of deriving
	// it. What distinguishes the two cases is only where the line came from.
	derived := func(borrowedLine bool) capacity.ReplicaCapacity {
		return capacity.ReplicaCapacity{
			VariantName:                     "v",
			SaturatedThroughput:             runMu,
			SaturatedThroughputSamples:      0,
			SaturatedThroughputDerived:      true,
			SaturatedThroughputLineBorrowed: borrowedLine,
		}
	}

	It("orders on a line the variant fitted itself", func() {
		f := Estimate(runLambda, []capacity.ReplicaCapacity{derived(false)},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse(),
			"a variant's own derived line orders with no sample count, by design")
		Expect(f.ByRole[domain.RoleDecode]).To(
			BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
	})

	It("holds but does not order on a borrowed line", func() {
		f := Estimate(runLambda, []capacity.ReplicaCapacity{derived(true)},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue(),
			"a line measured by a sibling must not grow this fleet")
		Expect(f.Terms[domain.RoleDecode].Replicas).To(
			BeNumerically("~", runLambda/runMu, 1e-6),
			"the uncapped figure is still reported, as it is for the other holds")
		Expect(f.ByRole[domain.RoleDecode]).To(
			BeNumerically("~", 0.85*float64(runK1), 1e-6),
			"capped at scaleUp x one replica")
	})

	It("orders once the variant has readings of its own", func() {
		// Borrowing a line says nothing about the variant's own measured
		// window. Once it has enough readings it orders on those, whatever it
		// started from.
		own := capacity.ReplicaCapacity{
			VariantName:                     "v",
			SaturatedThroughput:             runMu,
			SaturatedThroughputSamples:      MinThroughputSamplesToOrder,
			SaturatedThroughputLineBorrowed: true,
		}
		f := Estimate(runLambda, []capacity.ReplicaCapacity{own},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(f.ByRole[domain.RoleDecode]).To(
			BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
	})
})
