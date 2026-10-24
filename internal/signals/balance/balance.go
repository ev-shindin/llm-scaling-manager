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

// Thresholds are the scaling policy's per-replica saturation thresholds, the
// ones the saturation analyzer reads, so a replica the analyzer prices as full
// is the one this package reads as saturated.
type Thresholds struct {
	// KvUsage is the policy's kvCacheThreshold: a replica whose busiest-rank
	// KV utilization is at or over it is saturated whatever its queue. At or
	// below zero no replica is saturated on its KV.
	KvUsage float64
	// Queue is the policy's queueLengthThreshold: a replica judged on its
	// queue is saturated when its busiest rank's queue is at or over it. At or
	// below zero no replica is saturated on its queue.
	Queue float64

	// IdleKvUsage is the policy's loadImbalance.idleKvUsage; IdleAfterReady,
	// SustainFor and ClearAfter its loadImbalance durations. At or below zero
	// each takes the package constant of the same name. StaleAfter is
	// SustainFor.
	IdleKvUsage    float64
	IdleAfterReady time.Duration
	SustainFor     time.Duration
	ClearAfter     time.Duration
}

// WithDefaults returns th with each unset tuning field at its constant.
func (th Thresholds) WithDefaults() Thresholds {
	if !(th.IdleKvUsage > 0) {
		th.IdleKvUsage = IdleKvUsage
	}
	if th.IdleAfterReady <= 0 {
		th.IdleAfterReady = IdleAfterReady
	}
	if th.SustainFor <= 0 {
		th.SustainFor = SustainFor
	}
	if th.ClearAfter <= 0 {
		th.ClearAfter = ClearAfter
	}
	return th
}

// staleAfter is how long the model's collection must fail before its figures
// are stale: SustainFor, the time an imbalance takes to be reported.
func (th Thresholds) staleAfter() time.Duration {
	return th.WithDefaults().SustainFor
}

// Constants that classify one replica. The KV figures are fractions of one
// engine's KV cache, read on the replica's busiest engine rank
// (domain.ReplicaMetrics.BusiestRankKvUsage), which for a pod of one rank is
// the pod's own utilization.
const (
	// IdleKvUsage is the KV utilization below which a replica with nothing
	// waiting is idle.
	// The default of loadImbalance.idleKvUsage (Thresholds.IdleKvUsage).
	IdleKvUsage = 0.30
	// IdleAfterReady is how long a replica must have been Ready before it can
	// read idle. A replica that has just passed readiness has had no time to
	// receive work: every scale-up of a saturated role would otherwise read as
	// a saturated replica beside an idle one. The KV figure is a one-minute
	// peak, so this covers that window and a minute for the router to fill
	// the replica. A replica whose Ready time is unknown is not idle either.
	//
	// Tracker.Measure runs it from the later of the Pod's Ready time and the
	// first cycle the tracker saw that Ready time on this variant's replica.
	// A warm-pool Pod lent to the variant keeps the Ready time it had in the
	// pool, long before the lend, and a node clock behind the controller's
	// dates the Ready time into the controller's past; either alone would
	// skip the grace.
	// The default of loadImbalance.idleAfterReady.
	IdleAfterReady = 2 * time.Minute
	// SustainFor is how long a role must read imbalanced, on every cycle,
	// before it is reported as imbalanced (Reading.Sustained). It exceeds the
	// one-minute window the KV and queue peaks are read over, so one burst,
	// which stays in that window for a minute, does not warn.
	// The default of loadImbalance.sustainFor.
	SustainFor = 2 * time.Minute
	// ClearAfter is how long a role reported imbalanced must read balanced,
	// on every cycle, before the report clears. One cycle on which the
	// saturated replica's peak dips under Thresholds.KvUsage does not clear it,
	// so a persistent imbalance does not flap the gauge the alert reads.
	// The default of loadImbalance.clearAfter.
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

	// Saturated counts ready replicas at or over Thresholds.KvUsage, or with
	// a busiest-rank queue at or over Thresholds.Queue. The
	// queue test is left out on decode of a disaggregated model (one with a
	// prefill variant): there the decode replica's waiting count also holds
	// requests waiting for their remote KV, so it is judged on KV alone. A
	// decode variant with no prefill beside it serves whole requests, and its
	// queue is a backlog: measured, one such replica at max-num-seqs held 768
	// waiting at KV 0.86-0.89 while three idled.
	Saturated int
	// Idle counts ready replicas under IdleKvUsage with nothing waiting that
	// have been Ready for at least IdleAfterReady.
	Idle int

	// BusiestPod and IdlePods name the replicas behind the verdict, for the
	// warning line. BusiestPod is ranked by the signal that judges the role
	// saturated: a saturated replica before one that is not, then, on a role
	// judged on its queue, the deepest busiest-rank queue, then the highest
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
// (domain.RoleBoth), as domain.CanonicalRole reads an empty role. A model is
// disaggregated when rolesByVariant names a prefill variant, whether or not
// it has a ready replica, and only then is decode judged on KV alone.
// th is the policy's saturation thresholds.
//
// A replica that is not Ready is left out: nothing routes to it, so its load
// says nothing about routing. So is one whose KV reading is not a finite,
// non-negative number: an unknown load is neither saturated nor idle. A KV
// reading over 1 is read as 1, and a negative queue as empty.
//
// Measure runs the IdleAfterReady grace from each row's ReadySince alone.
// Tracker.Measure, which the engine calls, also runs it from the first cycle
// it saw the replica Ready, which covers a lent warm-pool Pod and a node clock
// behind the controller's.
func Measure(rows []domain.ReplicaMetrics, rolesByVariant map[string]string, th Thresholds, now time.Time) map[string]Distribution {
	return measure(rows, rolesByVariant, th, now,
		func(rm domain.ReplicaMetrics) time.Time { return rm.ReadySince })
}

// measure is Measure with the instant each replica's IdleAfterReady grace runs
// from given by graceFrom; a zero instant is never idle.
func measure(rows []domain.ReplicaMetrics, rolesByVariant map[string]string, th Thresholds, now time.Time,
	graceFrom func(domain.ReplicaMetrics) time.Time) map[string]Distribution {
	th = th.WithDefaults()
	out := map[string]Distribution{}
	sums := map[string]struct{ kv, queue float64 }{}
	busiest := map[string]busiestKey{}
	disaggregated := domain.HasPrefill(rolesByVariant)
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
		byQueue := role != domain.RoleDecode || !disaggregated
		isSaturated := saturated(rm, byQueue, kv, th)
		switch {
		case isSaturated:
			d.Saturated++
		case kv < th.IdleKvUsage && rm.QueueLength <= 0 && readyLongEnough(graceFrom(rm), now, th.IdleAfterReady):
			d.Idle++
			d.IdlePods = append(d.IdlePods, rm.PodName)
			out[role] = d
			continue // an idle replica is never the busiest
		}
		k := busiestKey{saturated: isSaturated, kv: kv}
		if byQueue {
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

// readyLongEnough reports whether grace (Thresholds.IdleAfterReady) has
// passed since since. An unknown instant is not long enough.
func readyLongEnough(since, now time.Time, grace time.Duration) bool {
	return !since.IsZero() && now.Sub(since) >= grace
}

// saturated reports whether rm is saturated: its KV at or over th.KvUsage, or,
// when byQueue, its busiest rank's queue at or over th.Queue.
func saturated(rm domain.ReplicaMetrics, byQueue bool, kv float64, th Thresholds) bool {
	if th.KvUsage > 0 && kv >= th.KvUsage {
		return true
	}
	if !byQueue || !(th.Queue > 0) {
		return false
	}
	return float64(rm.BusiestRankQueue()) >= th.Queue
}

// fraction is v clamped to at most 1, and false when v is not a finite,
// non-negative number.
func fraction(v float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return min(v, 1), true
}
