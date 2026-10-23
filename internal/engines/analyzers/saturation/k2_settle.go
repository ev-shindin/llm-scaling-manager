package saturation

import (
	"maps"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Whether a saturated reading is a replica AT ITS LIMIT, or a replica that has
// just been handed a load step: the stricter admission k2's persistence needs.
//
// A k2 sample is kept indefinitely (P2-hist reads and touches it every cycle),
// and a k2 that is too low never corrects itself: it orders replicas, so
// nothing saturates again to write a higher one. A queue over the threshold was
// the only test, and a queue is exactly what a replica shows in the first
// cycle of a load step, while its KV is still nearly empty -- the engine admits
// a burst of prompts within a second or two, but num_requests_waiting is a
// one-minute maximum and holds the burst's peak for a minute. Measured on the
// two-namespace rerun (2026-10-23, Qwen3-32B-FP8, H200): a replica stepped from
// idle read queue 160 at 31 713 resident tokens (8 % of its cache), the window
// became mean(270 539, 31 713) = 151 126, and the next reading brought it to
// 187 458 -- below the 252-266k one replica then held with no queue at all, so
// it read util 1.34-1.42 and the fleet went 1 -> 3. The same cycle recorded a
// saturated throughput of 0.0019 req/s: the rate window of the idle minute.
//
// docs/plans/analyzers/k2-capacity-model.md, constraint 4, is the rule: "the
// rate-based detector is what rejects it: a just-started replica has not
// reached its measured service rate." Never fixed by raising k2 -- a floor, an
// age-out or a reset would raise the estimate on every workload whose true
// compute bound IS low (prefill-heavy traffic queues at 16 % KV), which is the
// over-estimate that breaks SLOs. Only which readings count changes.
//
// The test is applied only to a reading whose resident KV is far below what
// its queue implies: the busiest rank's waiting requests, at the request's
// footprint, exceed everything that rank holds. A replica at its limit holds a
// full batch and the queue is the overflow; one whose overflow is larger than
// its whole batch is still filling. Such a reading is admitted on either of
// two kinds of evidence that the replica is at its limit after all:
//
//   - its service rate has reached the rate on record for the bucket -- its
//     own measured saturated throughput, or the derived one -- by
//     K2SettleRateFraction. A replica just handed a step is still completing
//     at the rate of the minute before it; or
//   - it was queued on its previous cycle too (within K2SettleMaxGap) and its
//     resident KV has stopped rising (by no more than K2SettleMaxRise). A
//     compute bound reached at low occupancy -- long prompts behind a
//     prefill-bound engine -- persists with flat residency and is admitted the
//     cycle after it first shows, so a bucket that has no rate on record yet
//     still learns its k2.
//
// A rejected reading writes nothing: not the k2 window and not the saturated
// throughput (written only for P1-obs), so the idle minute's rate does not
// become mu either. The cost is that the first reading of a genuinely
// saturated replica at low occupancy waits one cycle.
const (
	// K2SettleRateFraction is how close to the rate on record a suspect
	// reading's own service rate must be for the reading to count. Below one
	// because a saturated rate under-reads for the first minute a replica is
	// full; far above the rate of the minute before a load step.
	K2SettleRateFraction = 0.7
	// K2SettleMaxRise is how much a suspect reading's resident KV may have
	// grown since the previous cycle and still count as settled.
	K2SettleMaxRise = 0.25
	// K2SettleMaxGap is how old the previous cycle's reading may be. Past it,
	// the two readings are not consecutive cycles of one episode.
	K2SettleMaxGap = 2 * time.Minute
)

// k2ReasonObsUnsettled labels the diagnostic emitted when a saturated reading
// is left unrecorded because it looks like a load step, not a limit (see the
// header above). Not a capacity.K2Source: the chain falls through.
const k2ReasonObsUnsettled = "P1-obs-unsettled"

// settleReading is what one replica showed on one cycle, for the next cycle's
// settle test.
type settleReading struct {
	at     time.Time
	queue  int
	tokens int64
}

// settleMemo is one model's last readings per replica (SaturationAnalyzer.
// k2Settle).
type settleMemo struct {
	pods map[string]settleReading
	seen time.Time
}

// k2SettleInput is what the settle test reads for one replica.
type k2SettleInput struct {
	namespace, modelID, variant, pod string
	// queue and tokens are the busiest rank's queue and the tokens the reading
	// would record (BusiestRankTokens); ranks scales the latter to one rank.
	queue  int
	tokens int64
	ranks  int
	// perRequest is a waiting request's footprint on this role, as
	// waitingQueueDemand charges it.
	perRequest float64
	threshold  float64
	// rate is the replica's own service rate this cycle (saturatedCompletionRate)
	// and rateOK whether it has one; ref is the rate on record for the bucket,
	// zero when there is none.
	rate   float64
	rateOK bool
	ref    float64
}

// k2Unsettled returns why a saturated reading is not a replica at its limit,
// or "" when it may be recorded, and remembers this cycle's reading for the
// next. It runs for every priced row, saturated or not, so the previous cycle
// is always on hand.
func (a *SaturationAnalyzer) k2Unsettled(in k2SettleInput) string {
	now := a.now()
	model := in.namespace + "|" + in.modelID
	pod := in.variant + "/" + in.pod
	a.mu.Lock()
	memo, ok := a.k2Settle[model]
	if !ok {
		memo = &settleMemo{pods: make(map[string]settleReading)}
		a.k2Settle[model] = memo
	}
	prev, hadPrev := memo.pods[pod]
	memo.pods[pod] = settleReading{at: now, queue: in.queue, tokens: in.tokens}
	memo.seen = now
	// A replica gone for longer than the gap is a replica the next cycle
	// cannot pair with anything; dropping it keeps the memo the size of the
	// model's live fleet.
	maps.DeleteFunc(memo.pods, func(_ string, r settleReading) bool { return now.Sub(r.at) > K2SettleMaxGap })
	a.mu.Unlock()

	if in.queue < int(in.threshold) || in.tokens <= 0 {
		return "" // not a saturated reading; nothing to judge
	}
	perRank := float64(in.tokens) / float64(max(in.ranks, 1))
	if !(float64(in.queue)*in.perRequest > perRank) {
		return "" // the queue is the overflow of a batch at least as large
	}
	if in.ref > 0 && in.rateOK && in.rate >= K2SettleRateFraction*in.ref {
		return ""
	}
	if hadPrev && now.Sub(prev.at) <= K2SettleMaxGap && prev.queue >= int(in.threshold) &&
		prev.tokens > 0 && float64(in.tokens) <= float64(prev.tokens)*(1+K2SettleMaxRise) {
		return ""
	}
	return "queue deeper than the replica's whole resident batch, its service rate below the rate on record, and its resident KV not yet settled: a load step, not a limit; not recorded"
}

// k2SettlePerRequest is a waiting request's footprint on role: the prompt for
// prefill, the prompt and the generation otherwise (waitingQueueDemand).
func k2SettlePerRequest(rm domain.ReplicaMetrics, role string) float64 {
	per := rm.AvgInputTokens
	if domain.CanonicalRole(role) != domain.RolePrefill {
		per += rm.AvgOutputTokens
	}
	if !(per > 0) {
		return 0
	}
	return per
}

// serviceRateOnRecordLocked is the bucket's own measured saturated throughput,
// read without touching the window: the settle test is not a use of it.
// Callers hold a.mu.
func (a *SaturationAnalyzer) serviceRateOnRecordLocked(throughputKey string) float64 {
	if ra, ok := a.saturatedThroughput[throughputKey]; ok && ra.Len() > 0 {
		return ra.Median()
	}
	return 0
}
