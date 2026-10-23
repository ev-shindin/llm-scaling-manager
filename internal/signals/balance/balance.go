// Package balance measures how evenly the router spreads one model's load over
// the ready replicas of each role, and remembers whether a role has been
// unevenly loaded for long enough to warn about it.
//
// It is a diagnostic. Nothing here feeds a scaling decision: the analyzers
// price capacity per replica and demand per fleet, and a router that loads one
// replica while its siblings idle is something the operator fixes in the
// router's scorer configuration, not something a replica count fixes. The
// figures come from the per-replica rows the analyzers already receive, so
// they cost no query.
package balance

import (
	"math"
	"sort"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Thresholds that classify one replica. The KV figures are fractions of one
// engine's KV cache, read on the replica's busiest engine rank
// (domain.ReplicaMetrics.BusiestRankKvUsage), which for a pod of one rank is
// the pod's own utilization.
const (
	// SaturatedKvUsage is the KV utilization at or above which a replica is
	// saturated whatever its queue.
	SaturatedKvUsage = 0.90
	// IdleKvUsage is the KV utilization below which a replica with nothing
	// waiting is idle.
	IdleKvUsage = 0.30
	// IdleAfterReady is how long a replica must have been Ready before it can
	// read idle. A replica that has just passed readiness has had no time to
	// receive work: every scale-up of a saturated role would otherwise read as
	// a saturated replica beside an idle one. The KV figure is a one-minute
	// peak, so this covers that window and a minute for the router to fill
	// the replica. A replica whose Ready time is unknown is not idle either.
	IdleAfterReady = 2 * time.Minute
	// SustainFor is how long a role must read imbalanced, on every cycle,
	// before it is reported as imbalanced (Reading.Sustained). It exceeds the
	// one-minute window the KV and queue peaks are read over, so one burst,
	// which stays in that window for a minute, does not warn.
	SustainFor = 2 * time.Minute
	// ClearAfter is how long a role reported imbalanced must read balanced,
	// on every cycle, before the report clears. One cycle on which the
	// saturated replica's peak dips under SaturatedKvUsage does not clear it,
	// so a persistent imbalance does not flap the gauge the alert reads.
	ClearAfter = 2 * time.Minute
)

// Distribution is one role's load spread over its ready replicas in one cycle.
//
// The KV figures are per replica, on the replica's busiest engine rank, and
// are the collector's one-minute peak: the same reading the saturation tests
// use. The queue figures are the replica's waiting requests (vLLM
// num_requests_waiting summed over its ranks). The engine's running count is
// not collected, so the in-flight spread is read from the waiting queue alone.
type Distribution struct {
	// Role is the canonical role: prefill, decode or both.
	Role string
	// Ready is how many ready replicas the figures are over. Warm-pool
	// bridges count: the router routes to them like any other replica.
	Ready int

	MaxKvUsage  float64
	MeanKvUsage float64
	MaxQueue    float64
	MeanQueue   float64

	// Saturated counts ready replicas at or over SaturatedKvUsage, or (prefill
	// and aggregated roles only) with a busiest-rank queue at or over the
	// policy's queueLengthThreshold. A decode replica's waiting count also
	// holds requests waiting for their remote KV, so decode is judged on KV
	// alone.
	Saturated int
	// Idle counts ready replicas under IdleKvUsage with nothing waiting that
	// have been Ready for at least IdleAfterReady.
	Idle int

	// BusiestPod and IdlePods name the replicas behind the verdict, for the
	// warning line. BusiestPod is ranked by the signal that judges the role
	// saturated: a saturated replica before one that is not, then, on prefill
	// and aggregated roles, the deepest busiest-rank queue, then the highest
	// KV. It is never an idle replica, and is empty when every replica is idle.
	BusiestPod string
	IdlePods   []string
}

// Imbalanced reports whether, this cycle, at least one ready replica of the
// role is saturated while another is idle.
func (d Distribution) Imbalanced() bool {
	return d.Saturated > 0 && d.Idle > 0
}

// IdleWhileSaturated is the number of idle replicas when another replica of
// the role is saturated, and zero otherwise.
func (d Distribution) IdleWhileSaturated() int {
	if !d.Imbalanced() {
		return 0
	}
	return d.Idle
}

// KvMaxToMean is MaxKvUsage / MeanKvUsage. ok is false with fewer than two
// ready replicas or a mean of zero, where the ratio says nothing.
func (d Distribution) KvMaxToMean() (ratio float64, ok bool) {
	return maxToMean(d.MaxKvUsage, d.MeanKvUsage, d.Ready)
}

// QueueMaxToMean is MaxQueue / MeanQueue, with the same absence as KvMaxToMean.
// With N replicas it is at most N: every waiting request on one replica.
func (d Distribution) QueueMaxToMean() (ratio float64, ok bool) {
	return maxToMean(d.MaxQueue, d.MeanQueue, d.Ready)
}

func maxToMean(maxV, mean float64, n int) (float64, bool) {
	if n < 2 || !(mean > 0) {
		return 0, false
	}
	return maxV / mean, true
}

// busiestKey orders replicas for Distribution.BusiestPod: compared field by
// field, larger is busier.
type busiestKey struct {
	saturated bool
	queue     int
	kv        float64
}

func (k busiestKey) above(o busiestKey) bool {
	if k.saturated != o.saturated {
		return k.saturated
	}
	if k.queue != o.queue {
		return k.queue > o.queue
	}
	return k.kv > o.kv
}

// Measure computes the distribution of every role that has a ready replica
// among rows, as of now. rolesByVariant maps a variant to its role as
// discovery recorded it; a variant it does not name is read as aggregated
// (domain.RoleBoth), as domain.CanonicalRole reads an empty role.
// queueThreshold is the policy's queueLengthThreshold; at or below zero no
// replica is saturated on its queue.
//
// A replica that is not Ready is left out: nothing routes to it, so its load
// says nothing about routing. So is one whose KV reading is not a finite,
// non-negative number: an unknown load is neither saturated nor idle. A KV
// reading over 1 is read as 1, and a negative queue as empty.
func Measure(rows []domain.ReplicaMetrics, rolesByVariant map[string]string, queueThreshold float64, now time.Time) map[string]Distribution {
	out := map[string]Distribution{}
	sums := map[string]struct{ kv, queue float64 }{}
	busiest := map[string]busiestKey{}
	for _, rm := range rows {
		if !rm.Ready {
			continue
		}
		kv, ok := fraction(rm.BusiestRankKvUsage())
		if !ok {
			continue
		}
		role := domain.CanonicalRole(rolesByVariant[rm.VariantName])
		queue := float64(max(rm.QueueLength, 0))
		d := out[role]
		d.Role = role
		d.Ready++
		d.MaxKvUsage = max(d.MaxKvUsage, kv)
		d.MaxQueue = max(d.MaxQueue, queue)
		s := sums[role]
		s.kv += kv
		s.queue += queue
		sums[role] = s
		isSaturated := saturated(rm, role, kv, queueThreshold)
		switch {
		case isSaturated:
			d.Saturated++
		case kv < IdleKvUsage && rm.QueueLength <= 0 && readyLongEnough(rm, now):
			d.Idle++
			d.IdlePods = append(d.IdlePods, rm.PodName)
			out[role] = d
			continue // an idle replica is never the busiest
		}
		k := busiestKey{saturated: isSaturated, kv: kv}
		if role != domain.RoleDecode {
			k.queue = max(rm.BusiestRankQueue(), 0)
		}
		if d.BusiestPod == "" || k.above(busiest[role]) {
			d.BusiestPod, busiest[role] = rm.PodName, k
		}
		out[role] = d
	}
	for role, d := range out {
		d.MeanKvUsage = sums[role].kv / float64(d.Ready)
		d.MeanQueue = sums[role].queue / float64(d.Ready)
		sort.Strings(d.IdlePods)
		out[role] = d
	}
	return out
}

// readyLongEnough reports whether the replica has been Ready for at least
// IdleAfterReady. An unknown Ready time is not long enough.
func readyLongEnough(rm domain.ReplicaMetrics, now time.Time) bool {
	return !rm.ReadySince.IsZero() && now.Sub(rm.ReadySince) >= IdleAfterReady
}

func saturated(rm domain.ReplicaMetrics, role string, kv, queueThreshold float64) bool {
	if kv >= SaturatedKvUsage {
		return true
	}
	if role == domain.RoleDecode || !(queueThreshold > 0) {
		return false
	}
	return float64(rm.BusiestRankQueue()) >= queueThreshold
}

// fraction is v clamped to at most 1, and false when v is not a finite,
// non-negative number.
func fraction(v float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return min(v, 1), true
}
