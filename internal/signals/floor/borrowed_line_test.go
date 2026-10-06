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
// evidence about that configuration, not about this variant's load: at a fixed
// k the resident sequence count is k*C/KVreq and KVreq is the shape, so the
// same line implies different service rates for two variants serving different
// request shapes. Such a figure may hold a fleet and must not grow one.
//
// THE SAMPLE COUNT HERE IS THE POINT. An earlier version of this file built the
// record with SaturatedThroughputSamples: 0, under the confident comment "a
// derived figure carries no samples: that is the whole point of deriving it".
// Production does the opposite: the analyzer stamps MinDerivedThroughputSamples
// onto every derived figure, and that constant IS
// MinThroughputSamplesToOrder. So the spec validated a gate that could never
// fire, and the fix it was guarding -- a !LineBorrowed term added to one of two
// disjuncts -- changed no decision at all. Every case below carries the sample
// count production actually produces.
var _ = Describe("Estimate with a borrowed ITL line", func() {
	variants := func(ready int) []domain.VariantCapacity {
		return []domain.VariantCapacity{{
			VariantName: "v", Role: domain.RoleDecode, ReplicaCount: ready,
			PerReplicaCapacity: float64(runK1),
		}}
	}

	// As computeReplicaCapacity builds it on the derived path: the bucket is
	// the derived one, and the sample count is MinDerivedThroughputSamples,
	// which equals MinThroughputSamplesToOrder.
	derived := func(borrowedLine bool) capacity.ReplicaCapacity {
		return capacity.ReplicaCapacity{
			VariantName:                     "v",
			SaturatedThroughput:             runMu,
			SaturatedThroughputSamples:      MinThroughputSamplesToOrder,
			SaturatedThroughputDerived:      true,
			SaturatedThroughputLineBorrowed: borrowedLine,
		}
	}

	It("orders on a line the variant fitted itself", func() {
		f := Estimate(runLambda, []capacity.ReplicaCapacity{derived(false)},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse(),
			"a variant's own derived line orders without waiting for samples, by design")
		Expect(f.ByRole[domain.RoleDecode]).To(
			BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
	})

	It("holds but does not order on a borrowed line", func() {
		f := Estimate(runLambda, []capacity.ReplicaCapacity{derived(true)},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)

		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue(),
			"a line measured by a sibling must not grow this fleet")
		Expect(f.ByRole[domain.RoleDecode]).To(
			BeNumerically("~", 0.85*float64(runK1), 1e-6),
			"capped at scaleUp x one replica, not the uncapped order")
		Expect(f.Terms[domain.RoleDecode].Replicas).To(
			BeNumerically("~", runLambda/runMu, 1e-6),
			"the uncapped figure is still reported, as it is for the other holds")
	})

	It("names the borrowed LINE, not a borrowed bucket", func() {
		// An operator told "borrowed" would look for a neighbouring shape
		// bucket and find none.
		f := Estimate(runLambda, []capacity.ReplicaCapacity{derived(true)},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("borrowed-line"))
	})

	It("still names a borrowed BUCKET as borrowed", func() {
		// The pre-existing reason must not be renamed by the new one.
		bucket := capacity.ReplicaCapacity{
			VariantName:                 "v",
			SaturatedThroughput:         2.67,
			SaturatedThroughputSamples:  MinThroughputSamplesToOrder,
			SaturatedThroughputBorrowed: true,
		}
		f := Estimate(runLambda, []capacity.ReplicaCapacity{bucket},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("borrowed"))
	})

	It("orders once one replica is priced from its own window", func() {
		// Borrowing a line says nothing about a replica whose MEASURED window
		// answered instead. Such a record is not flagged -- the analyzer sets
		// the flag only when the derived figure is the one in use -- so it
		// orders on its own readings.
		own := capacity.ReplicaCapacity{
			VariantName:                "v",
			SaturatedThroughput:        runMu,
			SaturatedThroughputSamples: MinThroughputSamplesToOrder,
		}
		f := Estimate(runLambda, []capacity.ReplicaCapacity{derived(true), own},
			variants(2), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse(),
			"one own reading is enough to order, as it is for a borrowed bucket")
	})

	It("does not let a stale shape be the only thing holding it", func() {
		// The second half of the defect: fleetHasMeasuredItself used to settle
		// the shape-change hold on sight of a borrowed line, clearing
		// staleShape in the same cycle. So the hold must not DEPEND on
		// staleShape -- it has to bind with staleShape false, which every case
		// above asserts, and this states the reason.
		withStale := Estimate(runLambda, []capacity.ReplicaCapacity{derived(true)},
			variants(1), nil, BacklogDrainSeconds, 0.85, true, 0, nil)
		withoutStale := Estimate(runLambda, []capacity.ReplicaCapacity{derived(true)},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(withoutStale.ByRole[domain.RoleDecode]).To(
			Equal(withStale.ByRole[domain.RoleDecode]),
			"the cap on a borrowed line must not depend on the shape-change hold")
	})
})
