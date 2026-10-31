package saturation

import (
	"maps"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
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
//     K2SettleRateFraction, AND it holds K2SettleRateFraction of the k2 on
//     record. A replica just handed a step from idle is still completing at
//     the rate of the minute before it. One handed the hot queue while it was
//     already busy is not: it completes at most of the rate on record and
//     still reads a queue deeper than its batch while it fills -- in review,
//     a replica serving 1.5 req/s against 2.0 on record took 25 waiting on
//     160 000 resident and pulled the k2 mean 250 000 -> 232 000, and every
//     such move pulls it further. A rate shows the replica is busy, not that
//     it is at its limit; residency near the k2 on record does. With no k2 on
//     record there is nothing to be near, and only the next test admits; or
//   - it has been queued for a whole window and its resident KV has settled:
//     within K2SettleMaxRise, up or down, of its reading K2SettleLookback
//     earlier, with the queue standing on every cycle between and no gap
//     longer than the window. A fall is traffic moved off the replica, not
//     a limit. A compute bound reached at low occupancy -- long prompts
//     behind a prefill-bound engine -- persists with flat residency and is
//     admitted a window after it first shows, so a bucket that has no rate
//     on record yet still learns its k2.
//
// A window back, not a cycle back. The readings are one-minute peaks taken
// every 15 s, so two consecutive ones share 45 s, and a band of 25 % per
// cycle lets residency grow 2.4x a minute and still read "settled". A
// replica whose batch is capped by max_num_seqs fills for minutes after a
// step -- its batch is admitted at once and then grows by a token per
// sequence per step toward prompt plus half the output -- and on long
// outputs the cycle-to-cycle test admitted it mid-fill: in review, a
// simulated 1000/6000 replica's first k2 was 0.47-0.49 of its steady
// figure, 1000/4000 0.63-0.66, and k2 is never raised. Against the reading a
// window back the same fills are first admitted near their steady figure
// (k2_settle_test.go replays them).
//
// A rejected reading writes nothing: not the k2 window and not the saturated
// throughput (written only for P1-obs), so the idle minute's rate does not
// become mu either. The cost is that the first reading of a genuinely
// saturated replica at low occupancy waits a window, and that a noisy gauge
// at a true limit is refused more often (k2_settle_test.go measures it). A
// refusal only delays: it never writes a lower figure.
const (
	// K2SettleRateFraction is how close to the rate on record a suspect
	// reading's own service rate must be for the reading to count. Below one
	// because a saturated rate under-reads for the first minute a replica is
	// full; far above the rate of the minute before a load step.
	// The default of analyzerTuning.k2Settle.rateFraction.
	K2SettleRateFraction = config.DefaultK2SettleRateFraction
	// K2SettleMaxRise is how much a suspect reading's resident KV may have
	// grown, or fallen, since its reading K2SettleLookback earlier and still
	// count as settled.
	// The default of analyzerTuning.k2Settle.maxRise.
	K2SettleMaxRise = config.DefaultK2SettleMaxRise
	// K2SettleLookback is how far back the settled test compares: one
	// collector window (max_over_time[1m]), so the two readings share none
	// of it.
	K2SettleLookback = ThroughputSampleSpacing
	// K2SettleMaxGap is how long a replica's readings are kept: the lookback
	// and a minute of slack for a late cycle. A replica unseen for longer is
	// dropped from the memo.
	K2SettleMaxGap = 2 * ThroughputSampleSpacing
)

// k2ReasonObsUnsettled labels the diagnostic emitted when a saturated reading
// is left unrecorded because it looks like a load step, not a limit (see the
// header above). Not a capacity.K2Source: the chain falls through.
const k2ReasonObsUnsettled = "P1-obs-unsettled"

// settleReading is what one replica showed on one cycle, for later cycles'
// settle test.
type settleReading struct {
	at     time.Time
	queue  int
	tokens int64
}

// settleMemo is one model's last readings per replica (SaturationAnalyzer.
// k2Settle).
type settleMemo struct {
	// pods is each replica's readings over the last K2SettleMaxGap, oldest
	// first.
	pods map[string][]settleReading
	// idle is each decode replica's current run of idle readings, for the
	// prefill hold's settled-idle test (settledIdleDecode). Here because it is
	// the same per-model, per-replica memory and is swept with it.
	idle map[string]idleRun
	// readySince is each replica's Pod Ready time and when it was first
	// seen, for the joined test (noteReady). Dropped with the replica's
	// readings, and on any cycle the replica is not Ready.
	readySince map[string]readySighting
	// pastFirstCycle is set once the memo's first cycle has ended
	// (noteIdleRuns, which runs once at the end of every cycle's pricing).
	pastFirstCycle bool
	// firstCycle is the cycle the memo was made on: the analyzer's first
	// cycle for the model, after a controller restart or once the model was
	// swept. The joined test on the settle tracker reads it with
	// SwitchK2WaitsAfterRestart off (k2Joined).
	firstCycle time.Time
	// backlogs is the throughput floor's backlog per role on the cycles since
	// the newest one at least BacklogGrowthLookback ago, oldest first
	// (noteBacklogGrowth); empty before the first.
	backlogs []backlogReading
	seen     time.Time
}

// readySighting is one replica's Pod Ready time (zero when unknown) and the
// cycle the memo first saw it, zero on the memo's first cycle.
type readySighting struct {
	readyAt, firstSeen time.Time
	// restarted is set when the sighting began because the Pod's Ready time
	// changed under a name already seen: a restart the Ready time revealed.
	restarted bool
}

// newSettleMemo is an empty memo in its first cycle, the one at now.
func newSettleMemo(now time.Time) *settleMemo {
	return &settleMemo{
		pods:       make(map[string][]settleReading),
		readySince: make(map[string]readySighting),
		firstCycle: now,
	}
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
	// ready is the row's Pod Ready (domain.ReplicaMetrics.Ready), for the
	// joined test, and readyAt when its Pod last became Ready, zero when
	// unknown (replicaReadySince). bridge is whether a warm-pool Pod lent to
	// the variant serves it (domain.ReplicaMetrics.FromWarmPool): with
	// readyAt's row, the settle tracker's key (k2Joined).
	ready   bool
	readyAt time.Time
	bridge  bool
	// rate is the replica's own service rate this cycle (saturatedCompletionRate)
	// and rateOK whether it has one; ref is the rate on record for the bucket,
	// zero when there is none; k2Ref is the mean of the bucket's k2 window,
	// zero when it has none.
	rate   float64
	rateOK bool
	ref    float64
	k2Ref  float64
	// tune is the policy's tuning: k2Settle's fractions.
	tune tuning
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
		memo = newSettleMemo(now)
		a.k2Settle[model] = memo
	}
	firstCycle := memo.firstCycle
	var joined, byReadyAt bool
	if !a.k2ReadsSettle {
		// Before the settle tracker (SwitchK2ReadsSettle off): the memo's
		// own sighting of each Ready time (noteReady).
		joined, byReadyAt = memo.noteReady(pod, in.ready, in.readyAt, now, a.k2WaitsAfterRestart)
		if !in.readyAt.IsZero() {
			a.paths.add(pathReadySince, 1)
		}
	}
	trace := memo.pods[pod]
	settled := in.tune.settledOverWindow(trace, now, in.tokens, in.threshold)
	trace = append(trace, settleReading{at: now, queue: in.queue, tokens: in.tokens})
	for len(trace) > 0 && now.Sub(trace[0].at) > K2SettleMaxGap {
		trace = trace[1:]
	}
	memo.pods[pod] = trace
	memo.seen = now
	// A replica gone for longer than the gap is a replica a later cycle
	// cannot pair with anything; dropping it keeps the memo the size of the
	// model's live fleet.
	maps.DeleteFunc(memo.pods, func(_ string, rs []settleReading) bool {
		return len(rs) == 0 || now.Sub(rs[len(rs)-1].at) > K2SettleMaxGap
	})
	maps.DeleteFunc(memo.readySince, func(p string, _ readySighting) bool {
		_, live := memo.pods[p]
		return !live
	})
	a.mu.Unlock()

	if a.k2ReadsSettle {
		// The settle tracker has its own lock, and is read outside a.mu.
		var readyKnown bool
		joined, byReadyAt, readyKnown = a.k2Joined(in, firstCycle, now)
		if readyKnown {
			a.paths.add(pathReadySince, 1)
		}
	}

	if in.queue < int(in.threshold) || in.tokens <= 0 {
		return "" // not a saturated reading; nothing to judge
	}
	// A replica that joined under a window ago is filling, whatever its queue
	// says: its one-minute gauges still hold the minute before it was routed
	// to (throughput_admission.go).
	if joined {
		a.paths.add(pathK2UnsettledJoined, 1)
		if byReadyAt {
			a.paths.add(pathJoinedReady, 1)
		}
		return k2UnsettledJoined
	}
	perRank := float64(in.tokens) / float64(max(in.ranks, 1))
	if !(float64(in.queue)*in.perRequest > perRank) {
		return "" // the queue is the overflow of a batch at least as large
	}
	if in.ref > 0 && in.rateOK && in.rate >= in.tune.K2Settle.RateFraction*in.ref &&
		in.k2Ref > 0 && float64(in.tokens) >= in.tune.K2Settle.RateFraction*in.k2Ref {
		return ""
	}
	// Settled is FLAT over a window: within K2SettleMaxRise of the reading a
	// window back, either way. A fall is not settling. A replica that was
	// full and queued and next reads a fraction of that with a deep queue has
	// had its traffic moved off and handed back -- an argmax picker does this
	// -- and is filling again, not at a limit: measured in the imbalance
	// audit, 270 539 resident and queued, then 31 713 with 160 queued,
	// admitted on the rise test alone, took the variant's window 270 539 ->
	// 222 774 and re-priced every replica there.
	if settled {
		return ""
	}
	a.paths.add(pathK2UnsettledWindow, 1)
	return "queue deeper than the replica's whole resident batch, its service rate or its residency short of what is on record, and its resident KV not settled over a window: a load step, not a limit; not recorded"
}

// k2SettlePerRequest is a waiting request's footprint on role: the prompt for
// prefill, the prompt and the generation otherwise (waitingQueueDemand).
//
// The replica's own lengths where it has them, the fleet's (fleetIn,
// fleetOut) where it does not. A replica that has completed nothing reports
// no lengths at all -- the collector's per-pod mean is 0/0 and dropped -- and
// a footprint of zero made every queue on it the overflow of a batch "at
// least as large", so the settle test admitted it on sight. That is exactly
// the replica the test is for: measured on E1 (2026-10-23), one just joined
// read queue 218 on 174 359 resident with no completion, and its 0.066 req/s
// went into the throughput window as a saturated reading.
func k2SettlePerRequest(rm domain.ReplicaMetrics, role string, fleetIn, fleetOut float64) float64 {
	in, out := rm.AvgInputTokens, rm.AvgOutputTokens
	if !(in > 0) {
		in = fleetIn
	}
	if !(out > 0) {
		out = fleetOut
	}
	per := 0.0
	if in > 0 {
		per = in
	}
	if domain.CanonicalRole(role) != domain.RolePrefill && out > 0 {
		per += out
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

// settledOverWindow reports whether a reading of tokens at now has settled
// against trace, the replica's earlier readings oldest first: the newest of
// them at least K2SettleLookback old is within K2SettleMaxRise of tokens
// either way, and it and every reading after it stood queued (queue at or
// over threshold, tokens in use) with no two of them, nor the last and now,
// more than K2SettleLookback apart.
func (t tuning) settledOverWindow(trace []settleReading, now time.Time, tokens int64, threshold float64) bool {
	ref := -1
	for i := len(trace) - 1; i >= 0; i-- {
		if now.Sub(trace[i].at) >= K2SettleLookback {
			ref = i
			break
		}
	}
	if ref < 0 {
		return false
	}
	last := trace[ref].at
	for _, r := range trace[ref:] {
		if r.queue < int(threshold) || r.tokens <= 0 || r.at.Sub(last) > K2SettleLookback {
			return false
		}
		last = r.at
	}
	if now.Sub(last) > K2SettleLookback {
		return false
	}
	base := float64(trace[ref].tokens)
	rise := t.K2Settle.MaxRise
	return float64(tokens) <= base*(1+rise) && float64(tokens) >= base*(1-rise)
}

// k2OnRecordLocked is the mean of the bucket's k2 window, read without
// touching it: the settle test is not a use of it. Zero when there is none.
// Callers hold a.mu.
func (a *SaturationAnalyzer) k2OnRecordLocked(historyKey string) float64 {
	if ra, ok := a.computeCapacityHistory[historyKey]; ok && ra.Len() > 0 {
		return ra.Average()
	}
	return 0
}
