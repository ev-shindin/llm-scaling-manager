package saturation

import (
	"maps"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Whether a decode replica has been idle long enough to say decode has room
// the router is not using. One thing reads it: the hold on prefill's demand
// while decode is saturated (applyFloorAndHolds), which it relaxes. Relaxing
// that hold can only raise prefill's order; what prefill may LEARN stays gated
// on decode saturation alone (roleSaturated).
//
// Nothing here lowers a demand. A queue standing on one replica while a
// sibling idles is still the role's demand: demand and supply are both sums
// over the role's replicas, so a queue on the wrong replica is in the sum
// either way, and taking it out could only hide demand. An earlier revision
// moved such a queue to the idle sibling's room and charged it nowhere; in
// review that read a two-replica variant whose load filled it to 0.93 at 0.50,
// and three replicas really queued beside a fourth that was never routed to
// at 5.88 replicas against the 9.41 the fleet needed. Uneven routing is
// reported by the load-balance diagnostic, not compensated in the capacity
// math; where the math errs because of it, it over-orders.
//
// A decode replica counts as idle room when:
//
//   - it has read idle against what it can do (pricedIdle) on every cycle for
//     IdleSettledFor, no two readings more than IdleMaxGap apart. One idle
//     row is not an idle minute: the row is a one-minute peak, and a replica
//     that turned Ready seconds ago has a peak over those seconds alone.
//   - it is one of the variant's own replicas. A warm-pool bridge runs the
//     pool's engine and is lent for as long as the pool says; it is no
//     evidence of room the role keeps.

// IdleSettledFor is how long a replica must have read idle, cycle after
// cycle, before it counts as room (settledIdleDecode). Twice the collector's
// one-minute peak window: the first idle row may cover only the seconds since
// the replica turned Ready, and a full window of idle readings after it is
// what shows the minute was idle.
const IdleSettledFor = 2 * ThroughputSampleSpacing

// IdleMaxGap is the longest gap between two idle readings of one replica that
// still counts as one idle run. A reading is a one-minute peak, so readings
// further apart than that leave a stretch no reading covered.
const IdleMaxGap = ThroughputSampleSpacing

// idleRun is one replica's current run of idle readings.
type idleRun struct {
	since, last time.Time
}

// settledIdleDecode records this cycle's idle readings and returns how many
// of the model's own decode replicas have read idle for IdleSettledFor. caps
// and rows are parallel: caps[i] was priced from rows[i].
func (a *SaturationAnalyzer) settledIdleDecode(
	namespace, modelID string,
	caps []capacity.ReplicaCapacity,
	rows []domain.ReplicaMetrics,
	rolesByVariant map[string]string,
	t tuning,
) int {
	settled := a.noteIdleRuns(namespace, modelID, caps, rows, rolesByVariant, t, a.now())
	n := 0
	for i := range caps {
		if settled[i] && domain.CanonicalRole(rolesByVariant[caps[i].VariantName]) == domain.RoleDecode {
			n++
		}
	}
	return n
}

// pricedIdle reports whether a replica reads idle against what it can do, not
// only against its cache: replicaIdle, and its busiest rank holding under
// idleKvUsage of its effective capacity, min(k1, k2).
//
// The cache alone says idle about replicas that are not. A compute-bound
// replica's limit can sit below 30 % of its cache (prefill-heavy traffic
// queues at 16 % KV), and one at its k2 with no queue for a minute is busy.
// A row priced without a cache size has no k1 to measure against and is not
// idle either.
func (t tuning) pricedIdle(rm domain.ReplicaMetrics, rc capacity.ReplicaCapacity) bool {
	if rm.TotalKvCapacityTokens <= 0 || rc.EffectiveCapacity <= 0 || !t.replicaIdle(rm) {
		return false
	}
	return float64(rm.BusiestRankTokens()) < t.idleKvUsage*float64(rc.EffectiveCapacity)
}

// noteIdleRuns records this cycle's idle readings of the model's decode
// replicas in its replica memo and reports, per row, whether that replica has
// been idle for IdleSettledFor. Only decode is read (settledIdleDecode): a
// P/D prefill replica holds KV only until decode pulls it, so its KV is not
// its load and says nothing about whether it is idle.
func (a *SaturationAnalyzer) noteIdleRuns(
	namespace, modelID string,
	caps []capacity.ReplicaCapacity,
	rows []domain.ReplicaMetrics,
	rolesByVariant map[string]string,
	t tuning,
	now time.Time,
) []bool {
	settled := make([]bool, len(caps))
	model := namespace + "|" + modelID
	a.mu.Lock()
	defer a.mu.Unlock()
	memo, ok := a.k2Settle[model]
	if !ok {
		memo = newSettleMemo()
		a.k2Settle[model] = memo
	}
	if memo.idle == nil {
		memo.idle = make(map[string]idleRun)
	}
	memo.seen = now
	// The end of this cycle's pricing: a replica first seen Ready from the
	// next cycle on has joined the fleet (settleMemo.noteReady).
	memo.pastFirstCycle = true
	for i := range caps {
		rm := rows[i]
		key := rm.VariantName + "/" + rm.PodName
		if rm.FromWarmPool || domain.CanonicalRole(rolesByVariant[rm.VariantName]) != domain.RoleDecode ||
			!t.pricedIdle(rm, caps[i]) {
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
