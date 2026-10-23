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
	// idle counts the replica's latest rows in a row that carried neither an
	// ITL nor a k: no tokens in the rate window and nothing in the cache, so
	// nothing was served.
	idle int
}

// idleRows is how many rows in a row with neither an ITL nor a k make a
// replica idle rather than one that missed a scrape.
//
// An idle replica leaves nothing in its trace -- vLLM reports a KV fill of 0
// with nothing running, and a k of 0 is not traced -- so when the load comes
// back, its first reading had nothing to be judged against and was steady.
// That reading carries the warm-up ITL: on a two-namespace run, about 0.46 of
// k at 435 to 496 ms against a line near 0.09. It reached the full fit, and on
// a load step that gives an idle replica its first reading the full fit
// learned B = 77 ms against a true 12 ms and pinned it into every fit after.
// Two rows, so that one scrape that read nothing on a replica serving either
// side of it does not hold that replica out for DefaultSteadyFor.
const idleRows = 2

// serving notes whether this row of the replica carried an ITL, and whether
// it carried a traceable k. A replica that comes back with an ITL after
// idleRows or more rows with neither starts its trace over as a joiner: it
// waits out DefaultSteadyFor, and its trace begins at now.
//
// A row with a k and no ITL is not idle: the replica has work in its cache,
// and its ITL was not read. The collector drops a whole query's values when
// the query errors, so an avg_itl query that failed on two cycles running,
// with kv_usage_instant read on both, restarted every replica of a busy fleet
// and cost it DefaultSteadyFor of steady readings. Such a row leaves the count
// as it was: it neither idles the replica nor says it served.
func (rt *replicaTrace) serving(hasITL, hasK bool, now time.Time) {
	if !hasITL {
		if !hasK {
			rt.idle++
		}
		return
	}
	if rt.idle >= idleRows {
		rt.since, rt.joined, rt.samples = now, true, rt.samples[:0]
	}
	rt.idle = 0
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
// DefaultSteadyFor. So does one the load came back to after it idled: see
// idleRows.
func (rt *replicaTrace) steady(k float64, now time.Time) bool {
	if rt.settling(now) {
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

// settling reports whether the replica is still inside its settle: it joined
// after the key was first observed, or the load came back to it after it
// idled, less than DefaultSteadyFor ago. Its ITL still covers its warm-up --
// on a two-namespace run, a first reading about 0.46 of k at 435 to 496 ms
// against a line near 0.09 -- and nothing about its k says so.
func (rt *replicaTrace) settling(now time.Time) bool {
	return rt.joined && now.Sub(rt.since) < DefaultSteadyFor
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
// the warm pool, or is missing from the cycle restarts it, and so does load
// coming back after idleRows rows with neither an ITL nor a k.
func traceable(k float64) bool {
	return k > 0 && !math.IsInf(k, 0)
}
