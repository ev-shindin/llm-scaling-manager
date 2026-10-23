package itl

import (
	"math"
	"time"
)

// DefaultSteadyFor is how long a replica's k must have held still before its
// (k, ITL) pair may teach the full fit.
//
// The two halves of a pair are not read over the same interval. k is the
// instantaneous KV fill; ITL is a histogram mean over rate[1m]
// (collector/registration/queueing_model.go), scraped some seconds later. On a
// replica whose load just changed, the pair is this moment's k against the
// last minute's latency, and a line through such pairs is not ITL(k) at all.
// Two minutes covers the rate window and a couple of scrapes after it.
const DefaultSteadyFor = 2 * time.Minute

// DefaultMaxKDrift is how far a replica's k may have moved within
// DefaultSteadyFor and its reading still count as steady.
//
// Well under DefaultMinKSpread, so the spread the full fit requires cannot be
// supplied by drift within one load level: it has to come from two levels the
// replicas each settled at.
const DefaultMaxKDrift = 0.10

// DefaultSteadyCarry is how long a steady reading may outlive the oldest
// reading of the key's main window.
//
// The main window holds the latest DefaultWindowMaxSize readings, steady or
// not, and a load step fills it with unsteady ones: every replica is held out
// for DefaultSteadyFor, so N replicas add 8N unsteady rows at 15s cycles, the
// whole window once N >= 3. Trimmed to that window, the steady window lost the
// old level's readings before any reading of the new level was steady, the two
// levels were never in it together, and no fleet of two or more replicas ever
// had the spread a full fit needs: B stayed DefaultBaselineSec on every
// multi-replica cycle of the issue #270 rerun (0 of 294 Ready), while one
// replica learned it from the same step.
//
// Five minutes is DefaultSteadyFor for the new level to settle, up to two more
// for the load to get there, and a cycle or two of margin. The old level is
// kept that long and no longer, so a fit never rests on a level the fleet left
// more than a few minutes before the main window's oldest reading.
const DefaultSteadyCarry = 5 * time.Minute

// DefaultSteadyWindowMaxSize is how many steady readings a key keeps: twice
// the main window, so the old level's readings are not all displaced by the
// new level's first steady cycle on a fleet of up to about 30 replicas.
const DefaultSteadyWindowMaxSize = 2 * DefaultWindowMaxSize

// kSample is one replica's k at one Observe.
type kSample struct {
	at time.Time
	k  float64
}

// replicaTrace is what a key remembers of one replica's recent k.
type replicaTrace struct {
	// since is when the replica was first seen under the key.
	since time.Time
	// joined is set when the replica appeared after the key's first Observe:
	// it is new, not merely new to this process.
	joined  bool
	samples []kSample
}

// keyTraces is every replica a key is tracing, and when it began.
type keyTraces struct {
	since    time.Time
	replicas map[string]*replicaTrace
}

// steady reports whether a reading of k taken now from this replica is a
// steady-state pair, judged from what was seen BEFORE now: a replica offered
// several rows in one Observe is not unsteady against itself.
//
// A replica present when the key was first observed has no history to judge
// by and is taken as steady: nothing says otherwise, and requiring two minutes
// of history from every replica would delay the first full fit after every
// controller restart. One that joined later is new -- a pod just started, or
// back from a rollout -- and its ITL covers its own warm-up, so it waits out
// DefaultSteadyFor.
func (rt *replicaTrace) steady(k float64, now time.Time) bool {
	if rt.joined && now.Sub(rt.since) < DefaultSteadyFor {
		return false
	}
	cutoff := now.Add(-DefaultSteadyFor)
	for _, s := range rt.samples {
		if !s.at.Before(now) || s.at.Before(cutoff) {
			continue
		}
		if math.Abs(s.k-k) > DefaultMaxKDrift {
			return false
		}
	}
	return true
}

// note records k at now and forgets what is older than DefaultSteadyFor.
func (rt *replicaTrace) note(k float64, now time.Time) {
	cutoff := now.Add(-DefaultSteadyFor)
	kept := rt.samples[:0]
	for _, s := range rt.samples {
		if !s.at.Before(cutoff) {
			kept = append(kept, s)
		}
	}
	kept = append(kept, kSample{at: now, k: k})
	rt.samples = kept
}

// dropBefore removes the observations taken before cutoff.
func (w *Window) dropBefore(cutoff time.Time) {
	i := 0
	for i < len(w.observations) && w.observations[i].Timestamp.Before(cutoff) {
		i++
	}
	w.observations = w.observations[i:]
}

// traceable is whether a row's k says anything about the replica's load: a
// finite k above zero.
//
// Zero is what the collector leaves in KvUsageInstant when it read no k for
// the replica, and Observe already counts it as NoK. Traced as a reading, one
// missed scrape put a k of 0 into the trace and held the replica out of the
// steady readings for DefaultSteadyFor against it. A row without a k neither
// judges the replica nor restarts its trace; one that is not Ready, is lent by
// the warm pool, or is missing from the cycle restarts it.
func traceable(k float64) bool {
	return k > 0 && !math.IsInf(k, 0)
}
