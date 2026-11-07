package saturation

import "time"

// One slow pod and mu (docs/proposals/analyzer-learner.md, 2.6; #495). Mu's
// estimand is the fleet's mean, since a fleet serves the sum of its replicas'
// rates. A sample reads the queued replicas alone, so what it means depends on
// who queued. Refuse a slow pod beside three at 1 and it read 1.000 against a
// fleet of 0.835, the side that orders too few. Read a slow pod alone and it
// read 0.55 against 0.97: sixteen replicas ordered to 28 against a need of 18,
// and at 28 nothing queues, so nothing rewrote it. One defect, both sides.
//
// So while the readers sit under Admission.SiblingMuTolerance of what the
// Ready replicas they did not read serve, each of those counts in the sample
// at that rate. A served rate is a lower bound on what a replica sustains
// (servedRates.max), so the sample errs low, toward more replicas. Outside
// that state it is the readers' mean, as before: a replica serving less than
// the readers says nothing about its limit, so it never lowers a sample, and
// equal replicas read inside the tolerance of each other.
//
// Two rules bound what is left (SwitchMuUnreadAtTarget,
// SwitchMuMinorityFigureExpires):
//
//   - An unread replica under the KV line is counted at what it serves over
//     the scale-up target (servedPod.credit). Its rate is what it was offered,
//     not its limit, and the floor charges the target on mu again: counted at
//     what it serves, the floor orders a fleet in which it serves 0.85 of
//     that, a second headroom on a figure that is no limit. Counted at
//     served / target, the floor orders the fleet in which it serves what it
//     served, unqueued. The error is bounded: at most 1 / target over what it
//     served, and it corrects itself, since a replica priced too high queues
//     and is read.
//   - A figure no line can replace, written by a minority of the fleet and
//     under what the fleet's Ready replicas serve, gives way to that served
//     bound after MuChoice.LowHoldFor without a saturated reading that agrees
//     with it (minorityFigureExpired).
//
// What is still open: the closed-loop simulator cannot price a right
// minority figure released, since its floor never binds above occupancy
// without a queue. See the proposal's "One slow pod, as built".

// servedPod is one Ready own replica's served rate this cycle, whether it is
// queued past the threshold, which makes that rate a reading of its limit,
// and whether its cache is full (kvFull), which makes it one at its batch
// limit. An unqueued replica's rate is a lower bound on its limit.
type servedPod struct {
	rate   float64
	queued bool
	full   bool
}

// credit is the rate p counts at when a sample, or the figure a measurement
// yields to, counts it unread: what it serves, over target when its cache is
// under the KV line. A target outside (0, 1) credits nothing
// (SwitchMuUnreadAtTarget off).
func (p servedPod) credit(target float64) float64 {
	if p.full || !(target > 0 && target < 1) {
		return p.rate
	}
	return p.rate / target
}

// withTarget is served with every variant's unread replicas credited at
// target (servedPod.credit).
func withTarget(served map[string]servedRates, target float64) map[string]servedRates {
	for v, s := range served {
		s.target = target
		served[v] = s
	}
	return served
}

// unread is the mean rate the unqueued replicas not in reads count at
// (servedPod.credit), and how many they are.
func (s servedRates) unread(reads map[string]float64) (float64, int) {
	rates := make(map[string]float64, len(s.pods))
	for name, p := range s.pods {
		if _, read := reads[name]; !read && !p.queued {
			rates[name] = p.credit(s.target)
		}
	}
	return meanOf(rates), len(rates)
}

// byWeight is the mean over two groups of replicas, given each group's mean
// and size.
func byWeight(meanA float64, nA int, meanB float64, nB int) float64 {
	return (float64(nA)*meanA + float64(nB)*meanB) / float64(nA+nB)
}

// weighUnreadReplicas re-forms the throughput key's newest sample after a
// write w, counting the replicas it did not read when the readers sit under
// the tolerance of what those serve. It reports how many it counted and the
// rate it counted them at, n zero when it did not weigh.
//
// The rate they are counted at is the higher of what they serve now and what
// they held in this state (muChoice.unread). Mean served falls as the fleet
// grows: read fresh, the sample falls with it, which orders more, which lowers
// it again, until the slow pod stops queueing at the order it would have set
// alone. Read fresh, the 0.3 probe peaked at 28, held at 24
// (TestClosedLoopProbe_OneSlowReplicaUnderHalfItsSiblingsDoesNotSealTheFleet).
// The hold is the lower of two consecutive samples' rates, so one drain burst
// is never carried (TestMuSlowPod_TheWeighingHoldsAFallingFleetAndCarriesNoBurst).
func (a *SaturationAnalyzer) weighUnreadReplicas(key string, served servedRates, w muWrite, t tuning) (int, float64) {
	if !a.muWeighsFleet || (w != muSampled && w != muFolded) {
		return 0, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[key]
	c := a.throughputMuChoice[key]
	reads := a.throughputPodReads[key]
	if !ok || c == nil || len(reads.pods) == 0 {
		return 0, 0
	}
	read := meanOf(reads.pods)
	mean, n := served.unread(reads.pods)
	h := max(c.unread, mean)
	if n == 0 || !(read < t.Admission.SiblingMuTolerance*h) {
		c.unread, c.unreadLast = 0, 0
		return 0, 0
	}
	if w == muSampled {
		// Held only over two samples: a drain burst lifts one sample, which
		// the window's median drops, and is not carried past it.
		c.unread = max(c.unread, min(mean, c.unreadLast))
		c.unreadLast = mean
	}
	ra.SetLastAt(byWeight(read, len(reads.pods), h, n), a.now())
	return n, h
}

// fleetBound is what the variant's Ready replicas serve, weighed as a sample
// weighs them: a queued replica at what it serves, its limit, and an unqueued
// one as an unread replica counts (servedPod.credit). Zero with no replica.
// A replica serves no more than it sustains, so this errs toward more
// replicas; the credit's error is bounded as an unread replica's is.
func (s servedRates) fleetBound() float64 {
	rates := make(map[string]float64, len(s.pods))
	for name, p := range s.pods {
		if p.queued {
			rates[name] = p.rate
		} else {
			rates[name] = p.credit(s.target)
		}
	}
	return meanOf(rates)
}

// noteSampleWriters records, after a write w to the throughput key, whether
// the replicas that read its newest sample are a minority of the fleet: fewer
// than half of them and the unqueued replicas that did not read it together
// (muChoice.minority). A queued replica that did not read it is no evidence
// of a faster fleet, and counts on neither side. Every replica queued and
// read is a fleet that saturated together, and no minority.
func (a *SaturationAnalyzer) noteSampleWriters(key string, served servedRates, w muWrite) {
	if w != muSampled && w != muFolded {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.throughputMuChoice[key]
	reads := a.throughputPodReads[key]
	if c == nil || len(reads.pods) == 0 {
		return
	}
	_, n := served.unread(reads.pods)
	c.minority = 2*len(reads.pods) < len(reads.pods)+n
}

// minorityRelease is what minorityFigureExpired decided for one replica's
// reading.
type minorityRelease struct {
	// released is set when the measured mu gives way to bound this cycle.
	released bool
	// bound is the rate it gives way to (servedRates.fleetBound).
	bound float64
	// first is set on the first released cycle of the key's run of them,
	// the one that logs (the #303 rule: no line per replica per cycle).
	first bool
}

// minorityFigureExpired reports whether the throughput key's measured mu
// gives way this cycle to what the fleet serves (SwitchMuMinorityFigureExpires):
// no line prices a derived figure to give way to, a minority of the fleet
// wrote its newest sample (noteSampleWriters), the bucket has read nothing
// saturated in agreement with it, nor taken a sample, for MuChoice.LowHoldFor
// (lowHeldOut), and the figure sits under the Ready replicas' served bound
// (servedRates.fleetBound). Released, mu is that bound, never no mu.
//
// This moves toward the side that breaks the SLO with no new reading, so it
// states its reason (analyzer-learner.md, 3.4). A figure too low over-orders,
// and an over-ordered fleet never saturates, so nothing refreshes or replaces
// it: one slow pod's 0.55, read alone, held sixteen replicas at 28 against a
// need of 18 after it recovered. With a line, MeasuredLowHoldFor already
// releases such a figure to the derived one; with none, the fleet's own
// served rates are the figure it gives way to. They are a lower bound, so the
// release can only raise mu to what the replicas showed they sustain, and a
// figure at or over the bound is kept: a minority that saturated first under
// a steering router read what every replica can do, and the fleet at a lower
// load serves less than that. A figure the whole fleet wrote is kept: it is
// what every replica read, not one replica's. So is one whose writers are
// unknown, a window restored or brought back with no choice on record: an
// unknown resolves to the safe side, the lower figure. A borrowed reading is
// no window of this key's, and is never released.
func (a *SaturationAnalyzer) minorityFigureExpired(key string, reading throughputReading, served servedRates,
	derivedOK bool, t tuning, now time.Time) (out minorityRelease) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.throughputMuChoice[key]
	// Every path records the run's state: one that does not release ends
	// the run, so the next release logs again.
	defer func() {
		if c != nil {
			out.first = out.released && !c.released
			c.released = out.released
		}
	}()
	if !a.muMinorityExpires || derivedOK || !(reading.rate > 0) || reading.borrowed {
		return out
	}
	if c == nil || !c.minority {
		return out
	}
	fresh := reading.sampledAt
	if !c.saturatedAt.IsZero() {
		fresh = c.saturatedAt
	}
	bound := served.fleetBound()
	if !a.lowHeldOut(fresh, reading, t, now) || !(reading.rate < bound) {
		return out
	}
	return minorityRelease{released: true, bound: bound}
}

// yieldCapped reports whether a sibling yield to rate is refused by the cap
// (SwitchMuYieldCappedAtMeasurement): a yield may never price mu above the
// measurement it replaces. The yield opens only while the measurement sits
// under SiblingMuTolerance of what it yields to, so every figure it could
// price is above the measurement, and the cap refuses them all: the yield no
// longer moves the price, and the weighing (weighUnreadReplicas) carries the
// fleet into the sample instead. Uncapped, the lower median of the full
// replicas priced {1, 1, 1, 0.569} at 1.000 against a mean of 0.892, and a
// variant whose replicas served more than its measurement at 5.18 against a
// measured 3.39: both on the side that breaks the SLO, with no reading of the
// replica's own behind them.
func (a *SaturationAnalyzer) yieldCapped(reading throughputReading, rate float64) bool {
	return a.muYieldCapped && !(rate < reading.rate)
}
