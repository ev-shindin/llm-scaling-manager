package saturation

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
	if domain.CanonicalRole(role) == domain.RolePrefill {
		outBucket = prefillOutputBucket
	}
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s|i%s|%s|q%g",
		namespace, variantName,
		modelID, a.stableAccelerator(namespace, variantName, accelerator),
		gpuCount, domain.CanonicalRole(role), classifyInputLength(avgInput),
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
//
// Scoped by namespace and variant, so a window holds one variant's readings
// and nobody else's. Without them the key was model|accelerator|gpus|role|...,
// and every variant of a model on that hardware wrote into one window and read
// its average back as its own k2 -- in P1-obs, which returns the blended
// average, as well as in P2-hist. Two variants of one model differing in
// --max-num-seqs then share one k2, and the higher one makes the other claim
// more KV per replica than it holds and order fewer replicas. That is the
// direction that does not recover: a fleet held small does not saturate, so
// it never produces the reading that would correct it. Starting another
// variant from what one has measured is a separate question, answered on the
// read side with the source named (docs/proposals/
// learned-state-sharing-by-similarity.md); pooling on the write side destroys
// the provenance that answer needs.
func (a *SaturationAnalyzer) historyKey(
	modelID, namespace, variantName, accelerator string,
	gpuCount int,
	role string,
	avgOutput float64,
	queueThreshold float64,
) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s|q%g",
		namespace, variantName,
		modelID, a.stableAccelerator(namespace, variantName, accelerator),
		gpuCount, domain.CanonicalRole(role), classifyOutputLength(avgOutput), queueThreshold)
}

// k2KeyOutput is the output length a replica's k2 history is keyed by: the
// fleet's TRACKED length (shapeKeyOutput), the one the throughput key uses,
// and not the replica's own average.
//
// The replica's own average has no hysteresis, and a workload that sits on a
// bucket boundary crosses it every few cycles. Measured on a two-namespace
// run at max_tokens=500: per-replica averages read 482-582, the k2 window
// alternated between "medium" and "long" on consecutive cycles (541 against
// 485 decisions), and the two windows held 260 622 and 187 458. Per-replica
// capacity swung 39 % at constant load, and utilization 1.02 <-> 1.42 with
// it. The tracked length moves only when the fleet's shape changes, so one
// workload has one window.
//
// Prefill keeps its own average. It hands a request to decode after one
// token, so its own figure is always the same bucket ("short"), which is as
// stable as a key can be, and the fleet's generating length is not work
// prefill does (prefillOutputBucket says the same for the throughput key).
// A fleet with no tracked length yet falls back to the replica's own.
func k2KeyOutput(role string, shapeKeyOutput, own float64) float64 {
	if domain.CanonicalRole(role) == domain.RolePrefill || !(shapeKeyOutput > 0) {
		return own
	}
	return shapeKeyOutput
}
