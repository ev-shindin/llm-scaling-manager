package saturation

import (
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/fleet"
)

// The one shape the whole fleet is serving this cycle, out of readings that
// disagree.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

// withExpectedOutputTokens returns replicaMetrics with expected filled in as
// the output length of every OUTPUT-GENERATING replica that reports none.
//
// It exists so the scheduler queue can be priced on the first cycle of load.
// estimateSchedulerQueueDemand charges the queue Q x avgOutput, and avgOutput
// is an average over replicas that have COMPLETED something; on a cold fleet
// none has, so the queue is worth nothing to the role that will generate it.
// Measured on run QL: decode's share read {"decode":0} for the first two
// cycles with 141 requests already queued.
//
// A COPY, never a mutation of the caller's slice. The input is the analyzer's
// own AnalyzerInput, read by several other steps in the same cycle, and a
// replica whose reported output length was quietly rewritten would change what
// every one of them measured -- including the throughput keys and the shape
// tracker, which must follow what the fleet actually served.
//
// Only replicas that generate output, and only those reporting zero: a prefill
// replica completes about one token per request, so filling it in would move a
// figure that is already correct, and a replica with a real reading is not
// improved by a default.
//
// Returns the input unchanged when expected is not positive or nothing needs
// filling, so the common case allocates nothing.
func withExpectedOutputTokens(replicaMetrics []domain.ReplicaMetrics,
	rolesByVariant map[string]string, expected float64) []domain.ReplicaMetrics {
	if !(expected > 0) {
		return replicaMetrics
	}
	needed := slices.ContainsFunc(replicaMetrics, func(rm domain.ReplicaMetrics) bool {
		return !(rm.AvgOutputTokens > 0) && generatesOutput(rm, rolesByVariant)
	})
	if !needed {
		return replicaMetrics
	}
	out := slices.Clone(replicaMetrics)
	for i := range out {
		if !(out[i].AvgOutputTokens > 0) && generatesOutput(out[i], rolesByVariant) {
			out[i].AvgOutputTokens = expected
		}
	}
	return out
}

// computeModelWorkloadAverages computes the model-level average input tokens,
// output tokens, and prefix cache hit rate from replica metrics across all
// variants. These averages enable capacity estimation for zero-replica variants
// using the k2 derivation formula, and scheduler queue demand estimation.
//
// The output length is averaged over the replicas that GENERATE output, which
// on a P/D fleet excludes prefill. A prefill replica completes every request
// after one token (it hands off to decode), so its own
// vllm:request_generation_tokens averages ~1 -- not a measurement of the
// workload's output length but a property of the role. Folding it into an
// unweighted mean halves the model's output length at one prefill per decode
// replica and quarters it at three, and everything priced per queued request
// downstream (the scheduler queue's output-token charge, the zero-replica k2
// derivation) shrinks with it. Measured on a P/D run: 213 requests queued at
// the scheduler were charged 500 output tokens each against a 1000-token
// workload, because the one prefill replica's ~1 averaged against the one
// decode replica's 1000.
//
// Input tokens and the prefix-cache hit rate are still averaged over every
// replica: both roles see the same prompts, and in a P/D deployment the prefix
// cache that a queued prompt can hit lives on the prefill side.
//
// rolesByVariant maps variant name to its P/D role; a variant absent from it
// is treated as domain.RoleBoth, so a non-disaggregated fleet averages over
// every replica exactly as before.
func computeModelWorkloadAverages(replicaMetrics []domain.ReplicaMetrics, rolesByVariant map[string]string) (avgInput, avgOutput, avgHitRate float64) {
	var count, outputCount int
	for _, rm := range replicaMetrics {
		if rm.AvgInputTokens > 0 || rm.AvgOutputTokens > 0 {
			avgInput += rm.AvgInputTokens
			avgHitRate += rm.PrefixCacheHitRate
			count++
			if generatesOutput(rm, rolesByVariant) {
				avgOutput += rm.AvgOutputTokens
				outputCount++
			}
		}
	}
	if count > 0 {
		avgInput /= float64(count)
		avgHitRate /= float64(count)
	}
	if outputCount > 0 {
		avgOutput /= float64(outputCount)
	}
	return avgInput, avgOutput, avgHitRate
}

// fleetOutputLength is the output length the fleet is serving this cycle: the
// generating replicas' average output tokens, weighted by their request rate
// (fleet.Mean).
func fleetOutputLength(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.AvgOutputTokens },
		func(rm domain.ReplicaMetrics) bool { return generatesOutput(rm, rolesByVariant) })
}

// fleetOutputLengthRecent is fleetOutputLength over the SHORT window, and is
// the derived mu's divisor (deriveMu) WHILE A SHAPE CHANGE IS OUTSTANDING only.
//
// Zero when no generating replica reports the short-window figure -- an engine
// that does not publish the counter, or a fleet that completed nothing in the
// last minute.
//
// The caller falls back to fleetOutputLength then, and also whenever the shape
// is steady. That gate is not a precaution; it is a measured requirement. This
// window averages over requests that have COMPLETED, so on a fleet ramping into
// long generations it reads far below the length being served, which over-states
// mu and under-orders replicas. The caller's comment carries the bisect. The
// [5m] figure is wrong only for the few minutes after a shape change, which is
// exactly when this one is used.
func fleetOutputLengthRecent(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.AvgOutputTokensRecent },
		func(rm domain.ReplicaMetrics) bool { return generatesOutput(rm, rolesByVariant) })
}

// fleetPrefixHitRate is the prefill side's prefix-cache hit rate as ONE figure
// for the whole role, request-rate weighted.
//
// It passes fleet.ZeroIsAReading(), where the output and prompt lengths take
// fleet.Mean's default: that default skips a value of zero as absent, and a
// hit rate of zero is a reading, not a missing one -- a fleet with prefix
// caching off reports 0 on every replica, and skipping those would leave the
// mean to whichever replica happened to report something. It also bounds with
// fleet.Within(0, 1), because a hit rate is a fraction.
//
// One figure per role per cycle, for the same reason the output length is one
// figure: it buckets a key. rm.PrefixCacheHitRate is per REPLICA, so using it
// directly let two replicas of one variant land in different input buckets in
// the same cycle and split the window the bucket exists to hold together --
// the exact fault prefillOutputBucket was added to remove, on a new axis.
//
// Still not hysteretic: shape.Tracker tracks IL and OL, not this. A fleet
// whose hit rate drifts across a bucket boundary can therefore still move
// prefill's window, just not split it between replicas within a cycle.
func fleetPrefixHitRate(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.PrefixCacheHitRate },
		func(rm domain.ReplicaMetrics) bool {
			return canonicalRole(rolesByVariant[rm.VariantName]) == domain.RolePrefill
		},
		// A hit rate of zero is a reading -- a fleet with prefix caching off
		// reports it on every replica, and skipping those would leave the mean
		// to whichever replica happened to report something. Bounded because a
		// hit rate is a fraction.
		fleet.ZeroIsAReading(), fleet.Within(0, 1))
}
