package saturation

import (
	"time"
)

// Which saturated readings may become a measured service rate (mu), and how
// long a measured mu outranks a derived one that disagrees with it.
//
// A measured mu is kept and preferred over the derived figure (useDerived),
// and under EPP flow control vLLM's own queue rarely builds, so P1-obs -- the
// only writer -- rarely fires again: one bad sample stands for the rest of a
// run. Measured on E1 (2026-10-23, two namespaces, Qwen3-32B-FP8, one H200 per
// replica): a replica that had just joined read the router's queue (84) with
// no completion yet, and its 49 generated tokens a second over the 496-token
// output -- 0.0995 req/s -- became the bucket's mu. Against a derived 3.1-5.7
// and ~6.2 req/s actually served, the floor priced 2 req/s as 12-20 replicas
// (up to ~190), and the share never swung back. The other namespace's window
// took 1.75, 0.11 and 3.24 from replicas filling the same way.
//
// So a reading is a sample only from a replica that:
//
//   - has served a whole rate window since it was first seen Ready
//     (JoinedReplicaWindow). Its one-minute gauges and rates still hold the
//     minute before it was routed to, and its batch is still filling. This
//     gates the k2 reading too (k2Unsettled), and so the mu written beside it.
//     A replica already Ready on the analyzer's first cycle for the model has
//     an age the analyzer cannot know and is taken as established: refusing
//     it would hold every reading back for a window after each restart, and
//     the next two gates still apply to it. (ReplicaMetrics carries no Ready
//     time on this branch; a collector-reported one would close that gap.)
//   - completed requests over the window (RequestRate > 0). A replica that
//     has completed nothing has every sequence still growing: its token rate
//     is the rate of a batch being admitted, not of one being served.
//   - is not implausibly far under the derived figure for the same line:
//     under MinMeasuredOfDerived of it. A saturated replica at its limit
//     completes about what its ITL line prices; one completing a fiftieth of
//     it is still filling, or stalled.
//
// Each refusal only withholds a sample; none lowers or raises a figure. The
// cost of the last one is the case where the derived figure is the wrong one
// by more than 2x and the real rate is low: that measurement is refused, and
// the derived figure -- too high, so too few replicas -- prices until the
// line is refitted. A too-low mu over-orders (money), a too-high one
// under-orders (SLO); no gate here makes a mu higher than a measurement or a
// derivation gave.
//
// And a measured mu that is never refreshed does not outrank, for ever, a
// derived figure it disagrees with by more than MeasuredDerivedMaxRatio
// either way (measuredYieldsToDerived): it wins only while its newest sample
// is under MeasuredMuFreshFor old. Within the ratio it keeps winning however
// old it is -- run R's own 1.27 against a derived 0.67 is the case the
// measured-first rule exists for, and it is inside 2x.
const (
	// JoinedReplicaWindow is how long a replica must have been seen Ready
	// before its saturated reading counts: the collector's rate window
	// (ThroughputSampleSpacing, registration.RequestRateWindow).
	JoinedReplicaWindow = ThroughputSampleSpacing
	// MinMeasuredOfDerived is the smallest fraction of the derived mu a
	// saturated reading may be and still be recorded.
	MinMeasuredOfDerived = 0.5
	// MeasuredDerivedMaxRatio is how far a measured mu may disagree with the
	// derived one, either way, and still outrank it with no recent sample.
	MeasuredDerivedMaxRatio = 2.0
	// MeasuredMuFreshFor is how long after its newest sample a measured mu
	// outranks a derived one it disagrees with by more than
	// MeasuredDerivedMaxRatio.
	MeasuredMuFreshFor = 5 * time.Minute
)

// k2UnsettledJoined is the settle test's reason for a saturated reading from a
// replica that joined under JoinedReplicaWindow ago.
const k2UnsettledJoined = "the replica joined under a rate window ago: its batch is still filling and its one-minute readings still hold the minute before it was routed to; not recorded"

// noteReady records whether pod is Ready this cycle and reports whether it
// joined the fleet under JoinedReplicaWindow ago. Callers hold a.mu.
func (m *settleMemo) noteReady(pod string, ready bool, now time.Time) bool {
	if m.readySince == nil {
		m.readySince = make(map[string]time.Time)
	}
	if !ready {
		delete(m.readySince, pod)
		return false
	}
	since, seen := m.readySince[pod]
	if !seen {
		since = now
		if !m.pastFirstCycle {
			since = time.Time{} // already serving when the memo began
		}
		m.readySince[pod] = since
	}
	return !since.IsZero() && now.Sub(since) < JoinedReplicaWindow
}

// throughputRefusal says why a saturated replica's completion rate mu is not
// a sample of what one replica sustains, or "" when it may be recorded. The
// replica has already passed the k2 admission, the joined test included.
func throughputRefusal(requestRate, mu float64, derived derivedMu) string {
	if !(requestRate > 0) {
		return "no request completed over the rate window: every sequence is still growing, so the token rate is a batch being admitted, not served"
	}
	if derived.ok && mu < MinMeasuredOfDerived*derived.rate {
		return "under half the mu derived from the variant's ITL line: a replica still filling or stalled, not one at its limit"
	}
	return ""
}

// measuredYieldsToDerived reports whether a measured mu that useDerived would
// keep gives way to the derived one: they disagree by more than
// MeasuredDerivedMaxRatio either way, and the measurement's newest sample is
// MeasuredMuFreshFor old or older (or of unknown age).
func measuredYieldsToDerived(reading throughputReading, derived derivedMu, now time.Time) bool {
	if !derived.ok || !(derived.rate > 0) || !(reading.rate > 0) {
		return false
	}
	ratio := reading.rate / derived.rate
	if ratio <= MeasuredDerivedMaxRatio && ratio >= 1/MeasuredDerivedMaxRatio {
		return false
	}
	return reading.sampledAt.IsZero() || now.Sub(reading.sampledAt) >= MeasuredMuFreshFor
}
