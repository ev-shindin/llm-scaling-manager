package saturation

import (
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
)

// What the work nobody has started yet is worth, and which role is charged
// for it.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

// roleHold is what holdPrefillDemand did to prefill's demand: the figure it
// found, the figure it left, and the band it clamped into.
type roleHold struct {
	before, after, lo, hi float64
}

// holdPrefillDemand clamps roleDemand[prefill] into the band where the
// engine neither orders nor releases -- [scaleDown x supply, scaleUp x
// anticipated supply], the demands at which applyUniversalThreshold's RC
// and SC are both zero -- and reports whether it moved. No prefill demand
// entry, no prefill supply anticipated, or a demand already inside the band
// leaves it alone. When the band is empty the cap wins -- a hold that
// cannot avoid both errors must not order -- though with the config
// refusing a scale-down boundary at or above the scale-up threshold and
// each variant's starting term clamped at zero (aggregation.startingReplicas),
// anticipated supply is never below supply and the band is never empty in
// practice. The
// variants' own demand and utilization are moved with the role figure.
// undispatched is the scheduler queue's share of prefill demand: requests the
// gateway is holding that have been given to NO pod. It is the one part of
// prefill's demand the hold must not clamp, and the distinction is physical.
//
// A request whose KV sits on a prefill replica awaiting transfer has already
// been prefilled and has already produced its first token; what it waits for
// is decode, and ordering prefill for it buys nothing. A request in the
// scheduler's queue has been prefilled by nobody. Prefill capacity is exactly
// what it is waiting for, whatever decode is doing, and until it gets some it
// has no first token at all.
//
// Holding both together is what left prefill at one replica through a
// 600-deep queue while decode sat at its ceiling: the clamp lands on
// scaleUp x supply, which is the figure RC = D/scaleUp - anticipated turns
// into exactly zero, so no amount of queued work could order a replica.
// Measured on run PM: demand 550,077 clamped to 495,529, RC 0, for 19 cycles.
func holdPrefillDemand(roleDemand map[string]float64, variants []domain.VariantCapacity, scaleUp, scaleDown, undispatched float64) (roleHold, bool) {
	const role = domain.RolePrefill
	before, ok := roleDemand[role]
	if !ok || scaleUp <= 0 || scaleDown <= 0 {
		return roleHold{}, false
	}
	rc, ok := aggregation.AggregateByRole(variants)[role]
	if !ok || rc.TotalAnticipatedSupply <= 0 {
		return roleHold{}, false
	}
	h := roleHold{before: before, lo: scaleDown * rc.TotalSupply, hi: scaleUp * rc.TotalAnticipatedSupply}
	h.after = min(before, h.hi)
	if h.lo <= h.hi {
		h.after = max(h.after, h.lo)
	}
	// Never below the undispatched share. The band exists to stop prefill
	// being ordered or released on DECODE's backlog; work no pod has started
	// is not that, and clamping it away is what made the queue unable to
	// order anything. Bounded by `before` so this can only decline to hold
	// demand that was already there -- it never invents any.
	if undispatched > h.hi {
		h.after = max(h.after, min(undispatched, before))
	}
	if h.after == before {
		return h, false
	}
	roleDemand[role] = h.after
	// The per-variant figures follow, or the hold does not survive the split:
	// the optimizer prices each variant of a role by its share of the ROLE
	// demand (allocation.variantDemandShare), taken from the variants' own
	// TotalDemand, and the sticky scale-down tests a variant's release
	// against that priced figure. Left raw, the variant whose replica showed
	// decode's backlog would carry nearly the whole held total, and a sibling
	// idling on a genuine reading nearly none. Shared out by anticipated
	// supply instead -- the hold's point is that prefill's measurement is
	// nobody's this cycle, so every variant reads the band's figure at its
	// own size. Utilization moves with it, so what reads it (the warm pool's
	// pressure) sees the held figure too.
	for i := range variants {
		vc := &variants[i]
		if canonicalRole(vc.Role) != role {
			continue
		}
		// Through aggregation, not by hand: rc.TotalAnticipatedSupply is the
		// sum of exactly this term, and a numerator computed from
		// PendingReplicas while the denominator subtracted StuckReplicas made
		// the shares sum to more than 1 and over-distributed the held demand.
		anticipated := aggregation.AnticipatedSupply(*vc)
		vc.TotalDemand = h.after * anticipated / rc.TotalAnticipatedSupply
		vc.Utilization = 0
		if supply := float64(vc.ReplicaCount) * vc.PerReplicaCapacity; supply > 0 {
			vc.Utilization = vc.TotalDemand / supply
		}
	}
	return h, true
}

// waitingQueueDemand estimates the KV-token demand of the requests waiting in a
// replica's local engine queue (vllm:num_requests_waiting / sglang:num_queue_reqs).
// Their footprint is projected from the replica's average request shape, since
// the queue metric carries no per-request token counts.
//
// Note these requests are not uniformly blockless: on the decode side a
// transfer-pending request (vLLM's WAITING_FOR_REMOTE_KVS) has already had
// blocks allocated, so part of its prompt KV is also inside KvCacheUsage and is
// counted twice. The overlap shrinks toward zero under KV pressure, when block
// allocation starts failing — i.e. in the saturated regime where the scaling
// decision is actually made.
//
// The per-request cost is role-aware, because a replica only pays for the KV it
// actually materializes:
//
//	Prefill:      AvgInputTokens                   (prompt KV only; output is generated elsewhere)
//	Decode/Both:  AvgInputTokens + AvgOutputTokens (holds prompt KV and grows it per generated token)
//
// Charging decode replicas for input alone understates demand for
// long-generation workloads, which are precisely the ones whose KV pressure is
// output-driven. That is the under-reporting this addresses. It mirrors the role
// attribution estimateSchedulerQueueDemand already applies model-wide.
//
// # Why the full output length, and how it relates to the capacity side
//
// I+O is a request's KV footprint at its LAST decode step, not its mean. It is
// deliberately a peak, no-preemption planning charge: once admitted, a decode
// request's KV grows monotonically and the engine cannot shed it without
// preemption and recompute, so a replica expected to host the request needs room
// for its final size.
//
// I+O is this analyzer's demand-side unit. It is deliberately not the unit used
// by the time-averaged models elsewhere, and the distinction matters when reading
// the two together:
//
//   - Saturation V2 (here, demand): I+O — peak footprint. Sizes for what a
//     replica must be able to hold, not what it holds on average.
//   - Throughput analyzer (shape.Shape.KVreq): ILeff + O/2 —
//     time-averaged. A SEPARATE analyzer with its own model and its own
//     supply/demand pairing; it does not constrain this term and is not
//     inconsistent with it.
//
// One asymmetry is genuinely internal to this analyzer and should not be confused
// with the above: estimateCapacityFromParams prices a *concurrent* request at
// I + O/2 when deriving k2, so a queued request is charged more than it will
// occupy on average, and its charge drops once it is admitted and starts being
// measured through KvCacheUsage instead. Both effects bias this term toward
// scale-up.
//
// Note also that this term and the resident term are both derived from 1-minute
// maxima (max_over_time on kv_cache_usage_perc and num_requests_waiting), whose
// peaks need not coincide, so their sum can exceed any demand the replica
// actually saw at a single instant. That is a pre-existing property of the
// collector's queries, not of this function, but it compounds the bias above.
//
// That trade is chosen on purpose: under-provisioning decode capacity causes
// preemption and recompute thrash, which costs more than a spare replica.
// Revisiting it means changing the demand/capacity pair together, not this
// function alone.
//
// Returns 0 when the queue is empty, or when token metrics are absent or
// non-finite.
func waitingQueueDemand(rm domain.ReplicaMetrics, role string) int64 {
	if rm.QueueLength <= 0 {
		return 0
	}

	tokensPerRequest := rm.AvgInputTokens
	if canonicalRole(role) != domain.RolePrefill {
		tokensPerRequest += rm.AvgOutputTokens
	}
	// Compute in float64 and validate before converting, so one check covers
	// NaN, infinities, and finite values too large for an int64. Converting an
	// out-of-range float64 to int64 is implementation-defined in Go (amd64
	// yields math.MinInt64, arm64 saturates), and either way the result is
	// garbage that reads downstream as "idle, remove replicas" or as enormous
	// demand. Note `x <= 0` would NOT catch NaN, since every NaN comparison is
	// false. The collector filters NaN/Inf on the token averages, so this is a
	// guard rather than a live bug; it does not filter QueueLength, which is a
	// bare int conversion, but a garbage value there is caught either by the
	// QueueLength <= 0 check above or by the range bound below.
	//
	// Multiplying before truncating also drops the per-request rounding the
	// previous truncate-then-multiply form accumulated: at queue 3 and an average
	// of 100.9 tokens this yields 302 rather than 300.
	// The bound is >=, not >: float64(math.MaxInt64) rounds up to 2^63, which is
	// one past the largest representable int64, so a demand of exactly 2^63 would
	// pass a > check and then overflow to MinInt64.
	demand := float64(rm.QueueLength) * tokensPerRequest
	if !(demand > 0) || demand >= float64(math.MaxInt64) {
		return 0
	}

	return int64(demand)
}

// schedulerQueueDemand holds the estimated token demand from scheduler-queued
// requests, broken down by P/D role for disaggregated models.
type schedulerQueueDemand struct {
	total  float64            // model-level total (inputTokens + outputTokens)
	byRole map[string]float64 // per-role demand: "prefill", "decode", "both"
}

// estimateSchedulerQueueDemand estimates the token demand from requests queued
// in the llm-d inference scheduler's flow control layer, with per-role
// attribution for P/D disaggregated models.
//
// These requests have not yet reached any engine pod, so we estimate their
// token footprint using two independent signals:
//
//	inputTokens = max(queueBytes / BytesPerToken, queueSize * avgInputTokens)
//	             * (1 - prefixCacheHitRate)
//	outputTokens = queueSize * avgOutputTokens
//
// Role attribution:
//   - Prefill: inputTokens, discounted by PREFILL's own hit rate (see below)
//   - Decode:  outputTokens when a prefill role exists, inputTokens +
//     outputTokens otherwise (see "charging a queue to the role that serves it")
//   - Both:    inputTokens + outputTokens (handles full request lifecycle)
//   - Model-level total: inputTokens + outputTokens, EXCEPT where a prefill
//     role is active, in which case it is prefillInputTokens + outputTokens so
//     that the per-role charges sum back to it. See the comment above the
//     `total` assignment; "unchanged for backward compat" is what this used to
//     say, and it is the premise the split disproved.
//
// The prefix cache hit rate reduces expected input token KV demand because
// a fraction of prompt tokens will hit the prefix cache and reuse existing
// KV blocks. This does NOT apply to the local engine queue
// (vllm:num_requests_waiting / sglang:num_queue_reqs) because those requests
// have not yet had prefix cache lookup performed.
//
// prefillHitRate is the PREFILL role's own hit rate (fleetPrefixHitRate), and
// the prefill role's charge is discounted by it rather than by the model-wide
// average, BECAUSE THE DIVISOR IS. saturatedCompletionRate prices a prefill
// replica as PrefillComputedTokenRate / ILeff, where ILeff carries exactly this
// factor; the quotient is a replica count only if the dividend carries it too.
// The model-wide average is a different figure over a different set -- every
// replica with token activity, decode included, unweighted -- so on a P/D fleet
// the two diverge with the role ratio. At one prefill replica reading 0.8 and
// nine decode replicas reading 0.0 the average is 0.08: the charge would keep
// 92% of the prompt while the divisor kept 20% of it, inflating mu fivefold
// against the demand it is divided into and under-ordering prefill by the same
// factor -- worse the larger decode grows, which is the fleet this attribution
// exists for. Passing the one variable to both sides is what makes them cancel;
// its VALUE does not have to be right for the quotient to be a replica count,
// and when there is no prefill reading at all both sides fall back to 0
// together and no discount is taken on either.
//
// # Charging a queue to the role that serves it
//
// A queued request is ONE backlog, and it used to be charged in full to both
// roles: prefill got its input tokens and decode got the same input tokens plus
// the output. That is right in two cases and wrong in a third.
//
// It is right with NO DISAGGREGATION. A RoleBoth pod computes the prompt and
// generates from it, so the whole request is its work and the charge is the
// work. This branch is unchanged for that case, and for an unknown role.
//
// It is also right AT A STEADY SHAPE, even disaggregated -- not because the
// figure is accurate but because it is consistently inaccurate. mu is learned
// at the same shape the demand is charged at, so a fixed over-count divides out
// of demand/mu and the replica count survives it, exactly as the hit rate above
// does.
//
// It breaks on a DISAGGREGATED fleet when the shape turns input-heavy, because
// then the over-count stops being fixed. At 1000 in / 6000 out the charge is
// dominated by output, which is decode's real work. After the trace flips to
// 30000 in / 250 out the input term is 120x the output and swamps it, so decode
// is sized by prompts it does not compute and cannot hold until prefill has
// handed them over. Measured on that trace: decode's demand was 96.65% gateway
// queue charge against 2.74% resident KV, and at the peak cycle it was charged
// 1.32e8 tokens while holding 3.63e5 -- a factor of 366. It sat at eight
// replicas whose KV cache was 2% full, with one request waiting, beside a
// prefill side queueing 249.
//
// So the two roles are separated: prefill is charged the tokens it must
// compute, decode the tokens it must generate. Decode's share of the prompt is
// not dropped, it is DEFERRED to where it is real -- the resident KV that
// aggregateRoleDemand already counts from the engines, which rises as prefill
// actually delivers. The queue is charged once, to the role the queue is
// waiting on.
func estimateSchedulerQueueDemand(
	sq *domain.SchedulerQueueMetrics,
	replicaMetrics []domain.ReplicaMetrics,
	rolesByVariant map[string]string,
	activeRoles map[string]bool,
	prefillHitRate float64,
) schedulerQueueDemand {
	if sq == nil || (sq.QueueSize == 0 && sq.QueueBytes == 0) {
		return schedulerQueueDemand{}
	}

	// Compute model-level averages from replica metrics. The output length
	// comes from the replicas that generate output (see
	// computeModelWorkloadAverages): a prefill replica's ~1 must not dilute the
	// per-request charge below.
	avgInput, avgOutput, avgHitRate := computeModelWorkloadAverages(replicaMetrics, rolesByVariant)

	// Estimate input tokens from two signals, take the max for robustness
	tokensFromBytes := float64(sq.QueueBytes) / BytesPerToken
	tokensFromCount := float64(sq.QueueSize) * avgInput
	inputTokens := tokensFromBytes
	if tokensFromCount > inputTokens {
		inputTokens = tokensFromCount
	}

	// Apply prefix cache hit rate reduction to input tokens only
	inputTokensRaw := inputTokens
	inputTokens *= (1 - avgHitRate)

	// The same reduction at the PREFILL role's own hit rate, for the prefill
	// charge alone. Clamped the way shape.New clamps the figure the divisor is
	// built from, so the two cannot disagree about a reading out of range.
	prefillDiscount := prefillHitRate
	if math.IsNaN(prefillDiscount) || prefillDiscount < 0 {
		prefillDiscount = 0
	}
	if prefillDiscount > 1 {
		prefillDiscount = 1
	}
	prefillInputTokens := inputTokensRaw * (1 - prefillDiscount)

	// Estimate output tokens (no cache reduction — output must be generated)
	outputTokens := float64(sq.QueueSize) * avgOutput

	// On a DISAGGREGATED fleet the model total is what the two roles are
	// charged between them, and it has to be, because the roles are charged
	// disjoint slices of it and everything downstream now relies on their
	// summing back to it -- see the note in throughput_floor.go where
	// heldInModelTotal() used to stand.
	//
	// The two prompt figures are not the same number. The total's prompt is
	// discounted at the model-wide mean hit rate and prefill's charge at
	// PREFILL's own, which the fleet that motivated this diverges sharply on:
	// one prefill replica reading 0.8 beside nine decode replicas reading 0.0
	// gives a mean of 0.08. Left on the mean the total would carry 920,000
	// tokens of prompt where prefill is charged 200,000 -- a 720,000-token
	// shortfall between the total and the sum of its roles, owned by nothing,
	// which is the exact fault the split was made to remove.
	//
	// So the total follows the charge, not the other way round. An aggregated
	// fleet keeps the model-wide figure: there is one role, it is charged the
	// whole request, and no split has happened to be consistent with.
	//
	// The condition is PREFILL ALONE, not prefill-and-decode. It was the pair
	// at first, and that left the same divergence behind on a smaller fleet:
	// with a prefill role active and decode absent from activeRoles -- decode
	// scaled to zero, or simply carrying no VariantCapacity this cycle --
	// outputTokens is 0 (generatesOutput excludes prefill replicas, so there is
	// nothing to average), and the total fell back to the mean-discounted
	// prompt while prefill was still charged the rate-weighted one. The two
	// figures are then the SAME replicas averaged two different ways, which is
	// the fault e3218ce3 exists to remove. Two prefill replicas reading 0.9 at
	// 10 req/s and 0.1 at 1 req/s give a plain mean of 0.50 against a weighted
	// 0.83: on a 100-request queue of 10,000-token prompts the total read
	// 500,000 and prefill was charged 172,727, leaving 327,273 tokens -- 65% of
	// it -- owned by no role.
	//
	// Keyed on prefill alone the identity is exact in every shape: with decode
	// present the total is both charges, with decode absent outputTokens is 0
	// and the total IS prefill's charge. Decode-only is untouched, because
	// prefill not being active is what selects the model-wide figure.
	total := inputTokens + outputTokens
	if activeRoles[domain.RolePrefill] {
		total = prefillInputTokens + outputTokens
	}

	// Build per-role attribution
	byRole := make(map[string]float64)
	if len(activeRoles) > 0 {
		for role := range activeRoles {
			switch role {
			case domain.RolePrefill:
				byRole[domain.RolePrefill] = prefillInputTokens
			case domain.RoleDecode:
				// Disaggregated only when a prefill role is actually active:
				// a decode-labelled variant running alone still serves whole
				// requests, so it keeps the full charge.
				if activeRoles[domain.RolePrefill] {
					byRole[domain.RoleDecode] = outputTokens
				} else {
					byRole[domain.RoleDecode] = inputTokens + outputTokens
				}
			default: // domain.RoleBoth or unknown
				byRole[role] = total
			}
		}
	}

	return schedulerQueueDemand{total: total, byRole: byRole}
}
