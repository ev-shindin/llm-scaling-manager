package saturation_v2

import (
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The keys a reading is filed under. A reading taken under one shape must not
// price another, and these are what keep them apart.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

// prefillOutputBucket is the output bucket every prefill throughput reading is
// recorded under, whatever the fleet is generating.
//
// A prefill replica completes its request after the first token and hands the
// KV to decode, so how long the ANSWER turns out to be is not work it does.
// Keying its saturated throughput by the fleet's output length therefore
// splits one population of readings across unrelated buckets, and a shape swap
// walks it through several of them as the old requests drain.
//
// Measured on the 1k/6000 -> 30k/250 trace (run PJ, 2026-09-29): within a
// single phase whose prompt length never moved, prefill's bucket read xlong,
// then xxlong, then xlong, then medium, with mu re-learned in each. It had no
// mu at all for the first nine minutes of traffic, and when the floor wanted
// about nine replicas it was held to one per cycle -- 1->2->3->4 over two and
// a half minutes -- until a window finally had samples of its own, then
// jumped to 10.
//
// Not in outputBuckets, so nearestSaturatedThroughput declines to borrow
// across it. That is the intent: borrowing exists to cover the empty bucket an
// OUTPUT switch leaves behind, which prefill no longer has. The INPUT bucket
// stays in the key and still partitions these readings, because prompt length
// is what a prefill replica's throughput actually depends on -- so an input
// switch still, correctly, makes prefill re-learn.
const prefillOutputBucket = "noout"

// throughputKey is historyKey with the fleet's input bucket COMPOSED INTO it --
// not appended, see the body: a saturated throughput is a property of a replica
// AND the (I, O) it was measured under, so a reading recorded at one input
// length is not an own reading for another.
func (a *SaturationAnalyzer) throughputKey(
	modelID, namespace, variantName, accelerator string,
	gpuCount int,
	role string,
	avgInput, avgOutput float64,
	queueThreshold float64,
) string {
	// Composed, not appended. splitHistoryKey reads the output bucket as the
	// second-to-last field and the queue threshold as the last; an input
	// bucket tacked on the end made it read "q5" as the output bucket, which
	// is in no bucket table, so nearestSaturatedThroughput returned nothing
	// for every key and the neighbour-bucket borrow silently died. Putting the
	// input bucket ahead of the output one keeps that parse intact, and makes
	// a borrow walk output buckets WITHIN an input bucket -- which is what
	// borrowing should mean anyway.
	// Prefill is keyed by prompt length alone: the output bucket is a constant
	// for it, because output length is not work a prefill replica does. See
	// prefillOutputBucket.
	outBucket := classifyOutputLength(avgOutput)
	if canonicalRole(role) == domain.RolePrefill {
		outBucket = prefillOutputBucket
	}
	return fmt.Sprintf("%s|%s|%d|%s|i%s|%s|q%g",
		modelID, a.stableAccelerator(namespace, variantName, accelerator),
		gpuCount, canonicalRole(role), classifyInputLength(avgInput),
		outBucket, queueThreshold)
}

// historyKey is the bucket a replica's saturated observations (k2, and the
// throughput recorded beside it) are stored and read under.
//
// Scoped by role, not just model/accelerator/bucket: prefill's own
// avgOutputTokens is always ~0-1 (it hands off to decode before
// generating anything), so it always lands in the "short" bucket -- the
// same bucket a cold/fresh decode replica lands in before it's served
// real traffic. Without the role in the key, one role's P1-obs seeds
// history the other role then reads back via P2-hist, silently reusing
// an unrelated role's occupancy reading as its own capacity estimate.
//
// The queue threshold is in the key because it DEFINES what a P1 observation
// means: k2 is recorded as the occupancy seen when the queue was considered
// saturated, so a reading taken at threshold 2 is not a capacity estimate at
// threshold 100. Nothing else invalidates history -- EvictStaleHistory is
// age-based and knows nothing about policy -- so without this an operator
// retuning queueLengthThreshold keeps being sized by observations recorded
// under the old one. Measured: a k2 of 2 learned under a low threshold kept
// a variant at utilization 1.0 under a threshold of 100, where P1 could not
// fire at all.
func (a *SaturationAnalyzer) historyKey(
	modelID, namespace, variantName, accelerator string,
	gpuCount int,
	role string,
	avgOutput float64,
	queueThreshold float64,
) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|q%g",
		modelID, a.stableAccelerator(namespace, variantName, accelerator),
		gpuCount, canonicalRole(role), classifyOutputLength(avgOutput), queueThreshold)
}
