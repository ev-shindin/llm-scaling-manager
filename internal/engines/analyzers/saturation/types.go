package saturation

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// admission is whether one row's saturated reading may be recorded as the
// variant's own -- its k2, its saturated throughput, a start of its borrow
// clock -- and if not, why. Computed once per row (admissionOf) so the four
// places that gate on it cannot drift apart.
type admission struct {
	// why is empty for an admitted row and names the reason otherwise; it is
	// what the P1-obs-unadmitted log line says.
	why string
	// bridge is a warm-pool bridge lent to the variant. Its cache is the pool
	// engine's, so it files no capacity record, and only its saturated
	// reading may price its own cycle (lower-only) without being recorded.
	bridge bool
	// rollout is the variant mid-rollout (capacity.Record.InRollout): any
	// rollout its scale target reports, image-only included. A saturated
	// reading may be an old pod's, so it is not recorded, and the variant
	// is priced from what it already holds. Since nothing can be recorded,
	// the bound's clock does not run either (serving).
	rollout bool
	// idle is a row with no work this cycle: nothing resident, nothing
	// queued, and no completions. Its variant is not using the bound it
	// would be priced from, so the bound's clock waits (serving), as it does
	// for a parked variant: one Ready replica idle overnight at minReplicas
	// otherwise spent the whole hold and relaxation, and the morning's
	// traffic met the variant at its own, unprotected figure.
	idle bool
	// unsettled is empty for a saturated reading that may be recorded as k2 and
	// names why not otherwise: a reading that looks like a load step rather
	// than a limit (k2Unsettled). It refuses only the P1 write; the row is
	// admitted for everything else.
	unsettled string
}

// admitted reports a row whose reading may be recorded, rollout aside: a Ready
// Pod of the variant's own.
func (ad admission) admitted() bool { return ad.why == "" }

// serving reports a row that runs a lower bound's clock, and arms, re-arms or
// ends a bound: admitted, not mid-rollout, and not idle. With every saturated reading
// held back for the rollout, a clock that ran would relax the bound toward
// derived or k1 above what the fleet demonstrably holds -- for as long as a
// rollout stuck on a pending surge pod or held at a partition lasted -- and a
// normal-length one would hand the new arguments only what was left of their
// hold. Frozen, a bound the arguments' change restarted keeps its whole hold
// for the first serving cycle after the rollout (notePreviousFigureLocked).
func (ad admission) serving() bool { return ad.why == "" && !ad.rollout && !ad.idle }

// admissionOf is the admission rule: a row's reading is the variant's own
// only from a Ready Pod of the variant's own -- the rows its saturated
// throughput is taken from, too. A warm-pool bridge runs the pool's engine
// configuration; a Pod that is not Ready gets no traffic, so its queue and
// resident KV are left over or frozen.
func admissionOf(rm domain.ReplicaMetrics) admission {
	switch {
	case rm.FromWarmPool:
		return admission{why: "a warm-pool bridge runs the pool's engine configuration, not this variant's", bridge: true}
	case !rm.Ready:
		return admission{why: "the pod is not Ready: no traffic is routed to it, so its queue and resident KV do not measure this variant's saturation"}
	}
	return admission{idle: rm.TokensInUse <= 0 && rm.QueueLength <= 0 && rm.RequestRate <= 0}
}

// revisionsMixed names the variants whose own replicas -- warm-pool bridges
// aside, which run the pool's template -- report more than one pod template
// revision (domain.ReplicaMetrics.Revision). A replica that reports none is
// not counted: an unknown is not a second revision.
func revisionsMixed(rows []domain.ReplicaMetrics) map[string]bool {
	first := map[string]string{}
	mixed := map[string]bool{}
	for _, rm := range rows {
		if rm.FromWarmPool || rm.Revision == "" {
			continue
		}
		if seen, ok := first[rm.VariantName]; !ok {
			first[rm.VariantName] = rm.Revision
		} else if seen != rm.Revision {
			mixed[rm.VariantName] = true
		}
	}
	return mixed
}

// k2ReasonObsImplausible labels the diagnostic emitted when an observation is
// discarded for exceeding the KV cache's physical ceiling. It is deliberately
// not a capacity.K2Source: no capacity comes from it -- the analyzer falls
// through to the next priority -- so it must not appear where a K2Source label
// is expected. It shares the P-prefix vocabulary because it is read from the same
// k2-decision log line.
const k2ReasonObsImplausible = "P1-obs-invalid"

// k2ReasonObsDownstream labels the diagnostic emitted when a prefill
// replica's saturated queue is left unrecorded because the decode role is
// saturated in the same cycle: a prefill request completes only when decode
// admits it, so what prefill shows then is decode's saturation, not its own
// (computeK2). Like k2ReasonObsImplausible it is not a capacity.K2Source -- the
// analyzer falls through to the next priority.
const k2ReasonObsDownstream = "P1-obs-downstream"

// k2ReasonObsUnadmitted labels the diagnostic emitted when a saturated reading
// is left unrecorded because the row is a warm-pool bridge or a Pod that is not
// Ready -- the rows the saturated-throughput write already refuses
// (computeReplicaCapacity). The analyzer falls through to the next priority;
// where the reading is lower than what that gives, it prices the row for the
// cycle as capacity.K2SrcUnadmitted, whose label this is.
var k2ReasonObsUnadmitted = capacity.K2SrcUnadmitted.String()

// k2ReasonObsRollout labels the diagnostic emitted when a saturated reading is
// left unrecorded because the variant is mid-rollout (admission.rollout): the
// pod may be an old one. The chain goes on and the replica is priced from what
// the variant already holds; where the reading is lower than that, it prices
// the row for this cycle only, as capacity.K2SrcUnadmitted (unadmittedPrice).
// Nothing is written. Not a capacity.K2Source.
const k2ReasonObsRollout = "P1-obs-rollout"

const (
	satReasonP0Store = "P0-store" // capacity from store or compatible-variant record; no live replicas
	// satReasonNoData marks a variant with no live replicas and no store record.
	// It aliases the shared pipeline sentinel so this producer and the engine's
	// liveness gate (allocation.ResultIsInformative) cannot drift apart.
	satReasonNoData = domain.ReasonNoData
)

// outputBuckets lists the output-length buckets in ascending order of length.
// The order is what the throughput floor walks when a bucket has no reading
// of its own (see nearestSaturatedThroughput).
var outputBuckets = []string{"short", "medium", "long", "xlong", "xxlong", "huge"}

// classifyOutputLength returns a workload bucket name based on average
// output token length. The buckets are used to key compute-capacity (k2)
// history, since k2 depends heavily on generation length.
//
// Buckets (the thresholds and why they sit where they do are in constants.go):
//
//	"short"  — avgOutput in [0, 100)
//	"medium" — avgOutput in [100, 500)
//	"long"   — avgOutput in [500, 1500)
//	"xlong"  — avgOutput in [1500, 3000)
//	"xxlong" — avgOutput in [3000, 6000)
//	"huge"   — avgOutput >= 6000
func classifyOutputLength(avgOutputTokens float64) string {
	switch {
	case avgOutputTokens < ShortOutputThreshold:
		return "short"
	case avgOutputTokens < MediumOutputThreshold:
		return "medium"
	case avgOutputTokens < LongOutputThreshold:
		return "long"
	case avgOutputTokens < ExtraLongOutputThreshold:
		return "xlong"
	case avgOutputTokens < VeryLongOutputThreshold:
		return "xxlong"
	default:
		return "huge"
	}
}

// classifyInputLength buckets a prompt length on the same boundaries as
// classifyOutputLength. The thresholds are about how much KV a request holds,
// and a prompt token costs the same as a generated one, so the same table
// applies to both axes.
func classifyInputLength(avgInputTokens float64) string {
	return classifyOutputLength(avgInputTokens)
}
