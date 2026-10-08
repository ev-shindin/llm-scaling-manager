package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
)

// The borrowed-line gate has two halves, and gating only one gates nothing.
//
// The floor's half is covered in internal/signals/floor. This covers the other:
// fleetHasMeasuredItself settles the shape-change hold, and settling it clears
// `shapeChanged`, which is what reaches floor.Estimate as `staleShape`. So a
// borrowed line that settles the hold also removes the only other thing
// standing between it and an order -- in the same cycle, because settleShape
// runs before applyFloorAndHolds.
//
// Both halves were originally missed, and the floor's own spec validated the
// gate against a sample count production never produces. These are the controls
// for the half no spec reached at all.
func TestFleetHasMeasuredItselfRejectsABorrowedLine(t *testing.T) {
	derivedFrom := func(borrowedLine bool) capacity.ReplicaCapacity {
		// As computeReplicaCapacity builds it on the derived path: the sample
		// count is MinDerivedThroughputSamples, which equals the floor's
		// ordering threshold by construction.
		return capacity.ReplicaCapacity{
			VariantName:                     "decode-v",
			SaturatedThroughput:             5.4,
			SaturatedThroughputSamples:      MinDerivedThroughputSamples,
			SaturatedThroughputDerived:      true,
			SaturatedThroughputLineBorrowed: borrowedLine,
		}
	}

	t.Run("its own derived line settles the hold", func(t *testing.T) {
		// Unchanged behaviour, and the reason the hold can be released early at
		// all: a figure priced from this variant's own ITL(k) describes the
		// shape that just arrived.
		assert.True(t, fleetHasMeasuredItself(
			[]capacity.ReplicaCapacity{derivedFrom(false)}))
	})

	t.Run("a borrowed line does not settle the hold", func(t *testing.T) {
		assert.False(t, fleetHasMeasuredItself(
			[]capacity.ReplicaCapacity{derivedFrom(true)}),
			"settling on a sibling's line releases the hold the fleet has not "+
				"earned, and clears the staleShape that would otherwise cap it")
	})

	t.Run("a measured window still settles it, borrowed line or not", func(t *testing.T) {
		// A variant may hold a borrowed line AND have its own measured window;
		// the window is what settles the hold, and must keep doing so.
		own := capacity.ReplicaCapacity{
			VariantName:                "decode-v",
			SaturatedThroughput:        5.4,
			SaturatedThroughputSamples: floor.MinThroughputSamplesToOrder,
		}
		assert.True(t, fleetHasMeasuredItself([]capacity.ReplicaCapacity{own}))
	})

	t.Run("a borrowed bucket still does not settle it", func(t *testing.T) {
		// Pre-existing behaviour that must not change.
		bucket := capacity.ReplicaCapacity{
			VariantName:                 "decode-v",
			SaturatedThroughput:         5.4,
			SaturatedThroughputSamples:  floor.MinThroughputSamplesToOrder,
			SaturatedThroughputBorrowed: true,
		}
		assert.False(t, fleetHasMeasuredItself([]capacity.ReplicaCapacity{bucket}))
	})
}

// TestLineBorrowedIsSetOnlyWhenTheDerivedFigureIsUsed pins the narrowing in
// priceReplicas. A variant can hold a borrowed line and still be priced from
// its OWN measured window -- useDerived returns false once it has
// MinThroughputSamplesToOrder unborrowed readings of its own. Flagging that
// record would deny it an order it has earned, so the flag follows the figure
// in use, not the mere existence of a borrowed line.
func TestLineBorrowedIsSetOnlyWhenTheDerivedFigureIsUsed(t *testing.T) {
	// The predicate the narrowing depends on, stated as the production values.
	ownMeasured := throughputReading{
		rate:     5.4,
		samples:  floor.MinThroughputSamplesToOrder,
		borrowed: false,
	}
	require.False(t, useDerived(true, ownMeasured),
		"a variant with its own measured window must not be priced from the "+
			"derived figure, borrowed line or not -- if this flips, the "+
			"narrowing below has nothing to narrow")

	noneOfItsOwn := throughputReading{rate: 0, samples: 0}
	require.True(t, useDerived(true, noneOfItsOwn),
		"a variant with nothing of its own is priced from the derived figure, "+
			"which is the only case the flag may be set in")

	// A borrowed bucket is not an own window either, so it does not suppress
	// the derived figure and the flag can still be set.
	borrowedBucket := throughputReading{
		rate:     5.4,
		samples:  floor.MinThroughputSamplesToOrder,
		borrowed: true,
	}
	assert.True(t, useDerived(true, borrowedBucket))

	// What the flag is composed from is a single `&&` in priceReplicas, over
	// rc.SaturatedThroughputDerived -- which IS whether the derived figure was
	// used -- and c.itlBorrowed. The true-true case is asserted end to end
	// through the real cycle by TestBorrowedLineReachesTheRecord; the
	// assertions above pin the predicate that makes the other cases reachable.
	// Deliberately not re-computing that `&&` here: a test that restates the
	// expression it checks passes however the expression changes.
}
