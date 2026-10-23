package saturation

import (
	"maps"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// A queue standing on one replica while a sibling of its role has room is the
// router's to move, not a shortfall of the role. This file decides how much of
// such a queue the analyzer leaves out, and it errs toward charging it.
//
// The asymmetry is the reason. Charging a queue the router could have moved
// orders a replica the fleet did not need, which costs money. Leaving out a
// queue that only more replicas can serve under-orders a short fleet, which
// breaks SLOs. So a queue is left out only on strong evidence that a sibling
// really is idle and could take it, and only as much of it as that sibling has
// room for:
//
//   - The sibling has read idle against what it can do (pricedIdle) on
//     every cycle for IdleSettledFor, no two readings more than IdleMaxGap
//     apart. One idle row is not an idle minute: the row is a one-minute
//     peak, and a replica
//     that turned Ready seconds ago has a peak over those seconds alone. A
//     fleet scaling up lands replicas one at a time, so on a single-row test
//     there is nearly always a fresh "idle" pod, and the backlog that orders
//     the ramp was dropped for most of it -- measured in review, three full
//     replicas queued 60 each and a fourth just Ready implied 5.88 replicas
//     against 9.41 without the fourth.
//   - It is one of the variant's own replicas. A warm-pool bridge runs the
//     pool's engine and is lent for as long as the pool says; it is no
//     evidence of room the role keeps.
//   - At most its spare room is moved: its effective capacity (min(k1, k2))
//     less what it is charged, summed over the role's idle replicas, and
//     taken off the queued replicas' charges in proportion. A queue deeper
//     than that room still needs replicas, and the rest of it is charged.

// IdleSettledFor is how long a replica must have read idle, cycle after
// cycle, before its room may take a sibling's queue (routeQueuesToIdleRoom).
// Twice the collector's one-minute peak window: the first idle row may cover
// only the seconds since the replica turned Ready, and a full window of idle
// readings after it is what shows the minute was idle.
const IdleSettledFor = 2 * time.Minute

// IdleMaxGap is the longest gap between two idle readings of one replica that
// still counts as one idle run. A reading is a one-minute peak, so readings
// further apart than that leave a stretch no reading covered.
const IdleMaxGap = time.Minute

// idleRun is one replica's current run of idle readings.
type idleRun struct {
	since, last time.Time
}

// idleRoom is what one cycle's settled-idle replicas offer a role: whether it
// has any, and the tokens of room they have between them.
type idleRoom struct {
	replicas int
	spare    float64
}

// routeQueuesToIdleRoom moves as much of each role's engine queues off their
// replicas' charges as the role's settled-idle replicas have room for, and
// returns the room it found per role (domain.CanonicalRole). caps and rows
// are parallel: caps[i] was priced from rows[i].
//
// A moved share comes off LocalQueueDemand and ReplicaDemand, and is recorded
// as QueueRoutedFraction, which the throughput floor reads to leave the same
// share of the requests out of its backlog. Rows priced without a cache size
// (the store fallback) neither offer room nor give up a queue: their charges
// are estimates, and an estimate is no evidence either way.
func (a *SaturationAnalyzer) routeQueuesToIdleRoom(
	namespace, modelID string,
	caps []capacity.ReplicaCapacity,
	rows []domain.ReplicaMetrics,
	rolesByVariant map[string]string,
	logger logr.Logger,
) map[string]idleRoom {
	now := a.now()
	settled := a.noteIdleRuns(namespace, modelID, caps, rows, rolesByVariant, now)

	room := map[string]idleRoom{}
	for i := range caps {
		if !settled[i] {
			continue
		}
		role := domain.CanonicalRole(rolesByVariant[caps[i].VariantName])
		r := room[role]
		r.replicas++
		r.spare += float64(max(caps[i].EffectiveCapacity-caps[i].ReplicaDemand, 0))
		room[role] = r
	}
	if len(room) == 0 {
		return room
	}
	queued := map[string]float64{}
	for i := range caps {
		if takesRoutedQueue(caps[i], rows[i]) {
			queued[domain.CanonicalRole(rolesByVariant[caps[i].VariantName])] += float64(caps[i].LocalQueueDemand)
		}
	}
	for role, r := range room {
		q := queued[role]
		if !(q > 0) || !(r.spare > 0) {
			continue
		}
		frac := min(r.spare/q, 1)
		for i := range caps {
			rc := &caps[i]
			if !takesRoutedQueue(*rc, rows[i]) || domain.CanonicalRole(rolesByVariant[rc.VariantName]) != role {
				continue
			}
			moved := int64(frac * float64(rc.LocalQueueDemand))
			rc.LocalQueueDemand -= moved
			rc.ReplicaDemand -= moved
			rc.QueueRoutedFraction = frac
		}
		logger.V(logging.DEFAULT).Info("queue-routed-to-idle-room",
			"modelID", modelID, "namespace", namespace, "role", role,
			"idleReplicas", r.replicas, "idleSpareTokens", r.spare,
			"queuedTokens", q, "routedFraction", frac,
			"reason", "a sibling has read idle for IdleSettledFor; the share of the queue its room can take is the router's to move, the rest is charged")
	}
	return room
}

// takesRoutedQueue reports whether rc's queue may be moved to a sibling's
// room: one of the variant's own replicas, priced from its cache size, with a
// queue charged.
func takesRoutedQueue(rc capacity.ReplicaCapacity, rm domain.ReplicaMetrics) bool {
	return !rc.FromWarmPool && rm.TotalKvCapacityTokens > 0 && rc.LocalQueueDemand > 0
}

// pricedIdle reports whether a replica reads idle against what it can do, not
// only against its cache: replicaIdle, and its busiest rank holding under
// idleKvUsage of its effective capacity, min(k1, k2), and -- for prefill --
// completing under idleKvUsage of the rate on record.
//
// The cache alone says idle about replicas that are not. A compute-bound
// replica's limit can sit below 30 % of its cache (prefill-heavy traffic
// queues at 16 % KV): one at its k2 with no queue for a minute read idle, and
// in review the whole role's demand flapped 1 467 000 <-> 1 040 000 as one
// request came and went on it. A P/D prefill replica holds KV only until
// decode pulls it, so its KV is not its load at all: it is idle only when its
// own completion rate says so, and with no rate on record it is never taken
// as idle. A row priced without a cache size has no k1 to measure against
// and is not idle either.
func pricedIdle(rm domain.ReplicaMetrics, rc capacity.ReplicaCapacity, role string) bool {
	if rm.TotalKvCapacityTokens <= 0 || rc.EffectiveCapacity <= 0 || !replicaIdle(rm) {
		return false
	}
	if float64(rm.BusiestRankTokens()) >= idleKvUsage*float64(rc.EffectiveCapacity) {
		return false
	}
	if domain.CanonicalRole(role) == domain.RolePrefill {
		return rc.ServiceRateOnRecord > 0 && rc.ServiceRate < idleKvUsage*rc.ServiceRateOnRecord
	}
	return true
}

// noteIdleRuns records this cycle's idle readings in the model's replica memo
// and reports, per row, whether that replica has been idle for IdleSettledFor.
func (a *SaturationAnalyzer) noteIdleRuns(
	namespace, modelID string,
	caps []capacity.ReplicaCapacity,
	rows []domain.ReplicaMetrics,
	rolesByVariant map[string]string,
	now time.Time,
) []bool {
	settled := make([]bool, len(caps))
	model := namespace + "|" + modelID
	a.mu.Lock()
	defer a.mu.Unlock()
	memo, ok := a.k2Settle[model]
	if !ok {
		memo = &settleMemo{pods: make(map[string][]settleReading)}
		a.k2Settle[model] = memo
	}
	if memo.idle == nil {
		memo.idle = make(map[string]idleRun)
	}
	memo.seen = now
	for i := range caps {
		rm := rows[i]
		key := rm.VariantName + "/" + rm.PodName
		if rm.FromWarmPool || !pricedIdle(rm, caps[i], rolesByVariant[rm.VariantName]) {
			delete(memo.idle, key)
			continue
		}
		run, had := memo.idle[key]
		if !had || now.Sub(run.last) > IdleMaxGap {
			run.since = now
		}
		run.last = now
		memo.idle[key] = run
		settled[i] = now.Sub(run.since) >= IdleSettledFor
	}
	maps.DeleteFunc(memo.idle, func(_ string, r idleRun) bool { return now.Sub(r.last) > IdleMaxGap })
	return settled
}
