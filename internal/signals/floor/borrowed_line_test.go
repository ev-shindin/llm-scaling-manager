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

	It("composes a borrowed BUCKET with a stale shape too", func() {
		// The composition branch keys on borrowedOnly[role], which is true for
		// EITHER borrow -- a borrowed line or a borrowed bucket. Only the line
		// half was pinned, so narrowing the condition to lineBorrowed[role],
		// an easy slip given the parallel naming, would silently drop a
		// borrowed bucket back to the bare "shape-change" and hide the borrow
		// from an operator. That is the exact invisibility the composition was
		// added to fix, so both halves are pinned.
		bucket := capacity.ReplicaCapacity{
			VariantName:                 "v",
			SaturatedThroughput:         2.67,
			SaturatedThroughputSamples:  MinThroughputSamplesToOrder,
			SaturatedThroughputBorrowed: true,
		}
		withStale := Estimate(runLambda, []capacity.ReplicaCapacity{bucket},
			variants(1), nil, BacklogDrainSeconds, 0.85, true, 0, nil)
		Expect(withStale.Terms[domain.RoleDecode].HeldWhy).To(
			Equal("borrowed+shape-change"),
			"a borrowed bucket under a stale shape must keep naming the borrow")

		// The control: the same fixture without the stale shape, so the
		// assertion above cannot pass on a value that was already composed.
		withoutStale := Estimate(runLambda, []capacity.ReplicaCapacity{bucket},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(withoutStale.Terms[domain.RoleDecode].HeldWhy).To(Equal("borrowed"),
			"and without a stale shape it is the borrow alone")
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

		// And the borrow stays NAMED when both reasons hold. The two caps are
		// the same number, so the reason string is the only thing that tells
		// an operator which one to chase -- and a shape change settles itself
		// within a few cycles while a borrowed line persists until the variant
		// fits its own. Naming only the transient one sends them after the
		// wrong thing, and this is the common case: a fleet ramping into a new
		// shape is exactly when a fresh variant has no line of its own.
		Expect(withStale.Terms[domain.RoleDecode].HeldWhy).To(
			Equal("borrowed-line+shape-change"),
			"both reasons hold, so both are reported")
	})

	// WHY THE GATES ARE REPORTED SEPARATELY FROM Held.
	//
	// The hold is a CAP -- `if floor > hold` -- so Held is false in two
	// completely different states: the role's mu was orderable, or it was not
	// and the cap sat above a floor that was already smaller. Held alone
	// cannot tell them apart, and the difference is the whole question when a
	// borrowed line is suspected of pinning a fleet.
	//
	// This is not hypothetical. A 25-minute cluster run set out to test
	// exactly that, got heldAtFleet=false on every floor record with 40
	// borrowed readings in the same window, and could not say from the log
	// whether the hold branch had run at all. It took a second reading of this
	// function to rule out the alternative. MayOrder and BorrowedOnly are what
	// that run needed.
	It("reports the gates even when the cap does not bind", func() {
		// lambda/mu = 0.370, so the uncapped floor is 0.370 x K1, well under
		// the 0.85 x K1 cap. The cap is evaluated and does not bind -- which
		// is the idle fleet the cluster run was looking at.
		const idleLambda = 2.0
		f := Estimate(idleLambda, []capacity.ReplicaCapacity{derived(true)},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		t := f.Terms[domain.RoleDecode]

		Expect(t.Held).To(BeFalse(),
			"precondition: at lambda/mu = %.3f the floor is below the cap, so it cannot bind",
			idleLambda/runMu)
		Expect(f.ByRole[domain.RoleDecode]).To(
			BeNumerically("~", idleLambda/runMu*float64(runK1), 1e-6),
			"and the floor is the uncapped figure, not the cap")

		// The point: the gates are still reported.
		Expect(t.MayOrder).To(BeFalse(),
			"a borrowed line is not orderable, and that is true whether or not the cap bound")
		Expect(t.BorrowedOnly).To(BeTrue(),
			"the role's only reading is borrowed, which Held=false must not hide")
	})

	It("reports MayOrder true when the reading is the variant's own", func() {
		// The positive control for the field above: same shape, own line, so
		// an assertion of MayOrder=false would be proving the field is simply
		// always false.
		f := Estimate(2.0, []capacity.ReplicaCapacity{derived(false)},
			variants(1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		t := f.Terms[domain.RoleDecode]
		Expect(t.MayOrder).To(BeTrue(), "a variant's own derived line is orderable")
		Expect(t.BorrowedOnly).To(BeFalse(), "and nothing about the role is borrowed")
	})
})
