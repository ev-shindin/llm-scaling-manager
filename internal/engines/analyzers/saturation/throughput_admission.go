package saturation

import (
	"math"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
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
//     Its age is read from when its Pod last became Ready, as the collector
//     reports it (replicaReadySince): a pod back from an engine or LWS group
//     restart under the same name has a new Ready time, and is as fresh as a
//     new one. In review such a pod, absent for 75 s, kept its old
//     first-seen time, and its filling reading (174 327 tokens, 1.0 req/s,
//     against 340 000 and 6.3 served) was written as k2 and mu.
//     The Ready time is the node's clock, so the age runs from the later of
//     it and the analyzer's first sight of that Ready time: a node clock
//     behind the controller's would otherwise date a replica that has just
//     joined into the past and skip the window. Where the time is unknown
//     the first sight alone stands in. A replica already Ready on the
//     analyzer's first cycle for the model has no first sight to go by, and
//     is aged from its Ready time alone, or, where that is unknown too, is
//     taken as established: refusing it would hold every reading back for a
//     window after each restart, and the next two gates still apply to it.
//   - completed requests over the window (RequestRate > 0). A replica that
//     has completed nothing has every sequence still growing: its token rate
//     is the rate of a batch being admitted, not of one being served.
//   - generated no more than MaxTokensOverCompletions times the tokens its
//     completions account for (tokensOutrunCompletions; not judged for
//     prefill, which holds nothing long). The same fill, a minute later: the
//     batch is still growing, so tokens are generated for sequences that have
//     not completed yet, and the token rate over the output length -- the
//     mu recorded -- reads above what the replica completes. On the E1 rerun
//     (2026-10-23) two replicas that had joined a minute before, still
//     queued, read 6.70 and 6.99 req/s at 5.08 and 4.86 completed, 1.32x and
//     1.44x; they became the window's median, and sg1 was priced at 6.70
//     against the 6.0-6.1 its saturated replicas served -- 11 % high, the
//     under-ordering side. Full replicas past their fill read 0.90-1.07x
//     over 24 scrape intervals of the same run.
//   - is not under half of what a Ready replica of the variant served this
//     cycle (servedBounds; by more than MeasuredDerivedMaxRatio). A replica
//     that served a rate can sustain it, so the highest served rate is a
//     lower bound on mu, and a reading under half of it is one slow replica
//     -- a degraded GPU, a CPU-starved pod, an engine thrashing on
//     preemption -- not the variant's service rate. Only queued replicas
//     are admitted, so such a replica is often the only one read: in review
//     one queued at 0.3 req/s beside three serving 3.6 unqueued was
//     confirmed below and priced the fleet at 45 replicas for a load four
//     serve, 55.5 for thirty minutes after it recovered. No repetition
//     confirms a reading the served rate contradicts. Within the ratio, not
//     at it: equally saturated replicas read a few percent apart, and a
//     strict bound would refuse every one but the fastest.
//   - is not implausibly far under the derived figure for the same line:
//     under MinMeasuredOfDerived of it. A saturated replica at its limit
//     completes about what its ITL line prices; one completing a fiftieth of
//     it is still filling, or stalled. A filling replica reads that way for
//     a minute or so, though; one that reads it on every saturated cycle for
//     MeasuredLowConfirmFor is a real low rate -- the line is the wrong
//     figure -- and its readings are recorded from then on
//     (noteSaturatedReading). Without that, a real low measurement could
//     never be refreshed while the line stood, went stale by construction,
//     and gave way to the too-high derived figure however many saturated
//     readings re-confirmed it.
//
// Each refusal only withholds or delays a sample; none lowers or raises a
// figure. A too-low mu over-orders (money), a too-high one under-orders (SLO).
//
// And a measured mu does not outrank, for ever, a derived figure it
// disagrees with by more than MeasuredDerivedMaxRatio (measuredYieldsToDerived).
// It is fresh while its bucket keeps reading saturated at a rate that agrees
// with it -- accepted as a sample or refused -- within MeasuredMuFreshFor, and
// a fresh measurement wins either way. A stale one that disagrees by more than
// the ratio gives way to the LOWER of the two figures, which over-orders: a
// measurement far under derived keeps pricing, one far over it yields to
// derived. In review the two gates together always picked the higher figure
// under continuous saturation, a replica queued every minute at 1.2 req/s
// priced from a derived 3.6. Within the ratio the measurement keeps winning
// however old it is -- run R's own 1.27 against a derived 0.67 is the case the
// measured-first rule exists for, and it is inside 2x.
//
// The lower figure holds for MeasuredLowHoldFor, not for ever. The two
// directions do not correct alike. A derived figure that is too high
// under-orders, the replicas saturate, and their readings re-freshen the
// measurement within a cycle. A measurement that is too low over-orders, and
// an over-ordered fleet never saturates, so nothing refreshes or replaces it:
// in review 1.2 req/s held for 20 unsaturated minutes against a derived 3.585,
// three times the replicas the line priced, with no end date. Past the hold a
// measurement still MeasuredDerivedMaxRatio under derived gives way (latched
// at MeasuredDerivedReturnRatio as above); if it was right, the fleet
// saturates and takes it back -- one saturated episode per hold at worst.
//
// And a measurement under the derived figure gives way at once, fresh or
// not, while a Ready replica of the variant serves more than
// MeasuredDerivedMaxRatio times it (latched while it serves more than
// MeasuredDerivedReturnRatio times it): that replica sustains what it serves,
// so the measurement is not what one replica does. That is what a confirmed
// low reading cannot survive once the replica recovers, or a sibling shows
// it was one replica's. With no line there is no derived figure to give way
// to, and the measurement stands.
//
// Nor does a measurement outrank what the variant's Ready replicas serve
// with a full cache, at least one of them unqueued: under SiblingMuTolerance
// of their median it yields to it, line or no line, and stays yielded once
// they stop filling, until they read what it does (siblingYield). A replica at 0.55x its siblings passes the served bound
// above, and as the only queued replica it is the window: in review it priced
// the fleet at 1.8x the replicas for as long as it stayed slow, and neither
// the bound nor MeasuredLowHoldFor released it.
//
// The yield is latched: once a stale measurement has given way it stays given
// way until the two figures come back within MeasuredDerivedReturnRatio, or
// the bucket reads saturated again. Derived mu is recomputed every cycle from
// the fleet's output length and carries its noise; with a hard edge at 2x, in
// review, a measurement near it flipped the priced mu between the two figures
// 11 times in 20 cycles under 4 % noise on the output length.
const (
	// JoinedReplicaWindow is how long a replica must have been seen Ready
	// before its saturated reading counts: the collector's rate window
	// (ThroughputSampleSpacing, registration.RequestRateWindow).
	JoinedReplicaWindow = ThroughputSampleSpacing
	// MinMeasuredOfDerived is the smallest fraction of the derived mu a
	// saturated reading may be and still be recorded at once.
	// The default of analyzerTuning.muChoice.minMeasuredOfDerived.
	MinMeasuredOfDerived = config.DefaultMinMeasuredOfDerived
	// MeasuredLowConfirmFor is how long one replica must read saturated under
	// MinMeasuredOfDerived of the derived mu, on every saturated cycle with
	// no gap over ThroughputSampleSpacing, before those readings are
	// recorded: two rate windows, longer than a joining replica fills for.
	MeasuredLowConfirmFor = 2 * ThroughputSampleSpacing
	// MeasuredDerivedMaxRatio is how far a stale measured mu may sit above
	// the derived one and still outrank it.
	// The default of analyzerTuning.muChoice.maxRatio.
	MeasuredDerivedMaxRatio = config.DefaultMeasuredDerivedMaxRatio
	// MeasuredDerivedReturnRatio is how close a stale measured mu that has
	// given way to the derived one must come back before it outranks it
	// again: the hysteresis band under MeasuredDerivedMaxRatio.
	// The default of analyzerTuning.muChoice.returnRatio.
	MeasuredDerivedReturnRatio = config.DefaultMeasuredDerivedReturn
	// MeasuredMuFreshFor is how long after its bucket last read saturated in
	// agreement with it a measured mu outranks a derived one it disagrees
	// with by more than MeasuredDerivedMaxRatio.
	// The default of analyzerTuning.muChoice.freshFor.
	MeasuredMuFreshFor = time.Duration(config.DefaultMeasuredMuFreshFor)
	// MeasuredLowHoldFor is how long after its bucket last read saturated in
	// agreement with it a measured mu more than MeasuredDerivedMaxRatio under
	// the derived one keeps pricing.
	// The default of analyzerTuning.muChoice.lowHoldFor.
	MeasuredLowHoldFor = time.Duration(config.DefaultMeasuredLowHoldFor)
)

// k2UnsettledJoined is the settle test's reason for a saturated reading from a
// replica that joined under JoinedReplicaWindow ago.
const k2UnsettledJoined = "the replica joined under a rate window ago: its batch is still filling and its one-minute readings still hold the minute before it was routed to; not recorded"

// replicaReadySince is when rm's Pod last became Ready, zero when unknown:
// the Pod Ready condition's LastTransitionTime, which the collector sets only
// for a Ready Pod. Where it is zero, noteReady falls back to the analyzer's
// first sight of the replica.
func replicaReadySince(rm domain.ReplicaMetrics) time.Time {
	return rm.ReadySince
}

// noteReady records whether pod is Ready this cycle and reports whether it
// joined the fleet under JoinedReplicaWindow ago, counted from the later of
// readyAt, when its Pod last became Ready (zero when unknown), and when the
// analyzer first saw that readyAt on it, and whether the Ready time decided
// a join (byReadyAt): it is the later time, or a new one began the sighting.
// A new readyAt is a new sighting: the pod is back from a
// restart under the same name. Callers hold a.mu.
func (m *settleMemo) noteReady(pod string, ready bool, readyAt, now time.Time) (joined, byReadyAt bool) {
	if m.readySince == nil {
		m.readySince = make(map[string]readySighting)
	}
	if !ready {
		delete(m.readySince, pod)
		return false, false
	}
	s, seen := m.readySince[pod]
	if !seen || !s.readyAt.Equal(readyAt) {
		s = readySighting{readyAt: readyAt, firstSeen: now, restarted: seen && !readyAt.IsZero()}
		if !m.pastFirstCycle {
			s.firstSeen = time.Time{} // already serving when the memo began
		}
		m.readySince[pod] = s
	}
	since := s.firstSeen
	if readyAt.After(since) {
		since = readyAt
	}
	joined = !since.IsZero() && now.Sub(since) < JoinedReplicaWindow
	// The Ready time decided the join when it is the later of the two, or
	// when its change is what started this sighting.
	return joined, joined && !readyAt.IsZero() && (since.Equal(readyAt) || s.restarted)
}

// The reasons throughputRefusal gives.
const (
	refusedNoCompletion = "no request completed over the rate window: every sequence is still growing, so the token rate is a batch being admitted, not served"
	refusedUnderDerived = "under muChoice.minMeasuredOfDerived (half by default) of the mu derived from the variant's ITL line: a replica still filling or stalled, not one at its limit"
	refusedUnderServed  = "under 1/muChoice.maxRatio (half by default) of what a Ready replica of the variant served this cycle: a served rate is a lower bound on mu, so this is one slow replica, not the variant's service rate"
	refusedFilling      = "generated more tokens than its completions account for: sequences are still growing, so the token rate is a batch filling, not one being served"
)

// MaxTokensOverCompletions is the most a replica's generated-token rate may
// be over the tokens its completions account for (its request rate times
// its output length) for its rate to be a service rate
// (tokensOutrunCompletions). A full replica past its fill reads 0.90-1.07x
// (E1, 24 scrape intervals); a replica still filling read 1.32x and 1.44x.
// The default of analyzerTuning.admission.maxTokensOverCompletions.
const MaxTokensOverCompletions = config.DefaultMaxTokensOverCompletions

// tokensOutrunCompletions reports whether rm generated more than
// MaxTokensOverCompletions times the tokens its completions account for over
// the rate window: GenerationTokenRate against RequestRate times its own
// output length (fleetOutput where it has none). At steady state the two
// agree whatever the shape; while a batch fills, tokens go to sequences not
// yet completed, and the token rate runs ahead. A drain burst runs the other
// way and is not caught here. Prefill is not judged: it generates about one
// token per request, and its rate is not read from generated tokens
// (saturatedCompletionRate). Nor is a replica with no completions, which
// throughputRefusal refuses on its own.
func (t tuning) tokensOutrunCompletions(rm domain.ReplicaMetrics, role string, fleetOutput float64) bool {
	if domain.CanonicalRole(role) == domain.RolePrefill || !(rm.RequestRate > 0) || !(rm.GenerationTokenRate > 0) {
		return false
	}
	out := rm.AvgOutputTokens
	if !(out > 0) {
		out = fleetOutput
	}
	if !(out > 0) {
		return false
	}
	return rm.GenerationTokenRate > t.Admission.MaxTokensOverCompletions*rm.RequestRate*out
}

// throughputRefusal says why a saturated replica's completion rate mu is not
// a sample of what one replica sustains, or "" when it may be recorded. The
// replica has already passed the k2 admission, the joined test included.
// served is the variant's highest served rate this cycle (servedBounds), and
// filling whether the replica's tokens outran its completions
// (tokensOutrunCompletions).
func (t tuning) throughputRefusal(requestRate, mu float64, derived derivedMu, served float64, filling bool) string {
	if !(requestRate > 0) {
		return refusedNoCompletion
	}
	if filling {
		return refusedFilling
	}
	if served > t.MuChoice.MaxRatio*mu {
		return refusedUnderServed
	}
	if derived.ok && mu < t.MuChoice.MinMeasuredOfDerived*derived.rate {
		return refusedUnderDerived
	}
	return ""
}

// throughputKeyInput is the prompt length a role's throughput key is built
// with and its rate converted at: the tracked prompt length, and for prefill
// the part of it the prefix cache does not hold, at the role's hit rate
// (computeReplicaCapacity says why).
func throughputKeyInput(role string, shapeKeyInput, shapeKeyOutput, fleetHitRate float64) float64 {
	if domain.CanonicalRole(role) == domain.RolePrefill {
		return shape.New(shapeKeyInput, shapeKeyOutput, fleetHitRate).ILeff
	}
	return shapeKeyInput
}

// servedRates is what a variant's Ready own replicas served this cycle, in
// the requests/s its saturated throughput is measured in (servedBounds).
type servedRates struct {
	// max is the highest rate any of them served, queued or not: a lower
	// bound on the variant's mu (throughputRefusal, measuredYieldsToDerived).
	max float64
	// saturated is the lower median of the rates served by those whose cache
	// is full (kvFull), queued or not: what a replica at its batch limit
	// serves, and the evidence a measured mu yields to (siblingYield). Zero
	// unless at least one of them is unqueued or at least two are full, so
	// that a slow replica, full and queued alone, is never its own sibling
	// evidence.
	//
	// The queued ones count. The router steers away from a queued replica,
	// so in a fleet of mixed speeds the fast replicas are the ones full with
	// nothing queued: read from them alone, three replicas at 2.0 beside one
	// at 3.6 priced every replica at 3.6, and a fleet serving 9.6 req/s
	// against 16 arriving was ordered 5.71 replicas against the 7.84 its
	// mean of 2.4 needs (review, round 5).
	saturated float64
	// unqueued is whether any of the full replicas has no queue past the
	// threshold. Only then may saturated open a yield (siblingYield): a
	// queued replica is measured, and the window already holds what it
	// serves.
	unqueued bool
}

// servedBounds is, per variant, what its Ready own replicas served this
// cycle (servedRates), at the fleet's shape (saturatedCompletionRate). A
// replica that served a rate sustains it, so the highest is a lower bound on
// the variant's mu; and one whose cache is full serves about its limit, so
// the median of those is a reading of it. Warm-pool bridges run the pool's
// engine and are left out, as a Pod that is not Ready is, whose rate is not
// a capacity (admissionOf), and so is a replica still filling, whose token
// rate runs ahead of what it serves (tokensOutrunCompletions).
func (t tuning) servedBounds(rows []domain.ReplicaMetrics, rolesByVariant map[string]string, queueThreshold, kvCacheThreshold,
	fleetOutput, shapeKeyInput, shapeKeyOutput, fleetHitRate float64) map[string]servedRates {
	out := make(map[string]servedRates)
	full := make(map[string]map[string]float64)
	for _, rm := range rows {
		if rm.FromWarmPool || !rm.Ready {
			continue
		}
		role := rolesByVariant[rm.VariantName]
		rate, ok := saturatedCompletionRate(rm, role, fleetOutput,
			throughputKeyInput(role, shapeKeyInput, shapeKeyOutput, fleetHitRate))
		if !ok || !(rate > 0) || math.IsInf(rate, 1) || t.tokensOutrunCompletions(rm, role, fleetOutput) {
			continue
		}
		s := out[rm.VariantName]
		s.max = max(s.max, rate)
		out[rm.VariantName] = s
		if kvFull(rm, kvCacheThreshold) {
			if full[rm.VariantName] == nil {
				full[rm.VariantName] = make(map[string]float64)
			}
			full[rm.VariantName][rm.PodName] = rate
			if rm.BusiestRankQueue() < int(queueThreshold) {
				s.unqueued = true
				out[rm.VariantName] = s
			}
		}
	}
	for v, reads := range full {
		s := out[v]
		if s.unqueued || len(reads) >= 2 {
			s.saturated = lowerMedian(reads)
		}
		out[v] = s
	}
	return out
}

// SiblingMuTolerance is how far under the rate its Ready, full siblings
// serve (servedRates.saturated) a measured mu may sit before it yields to
// them (siblingYield). Equally saturated replicas at one shape read within
// about 10 % of each other: on E1 (2026-10-23) the generated-token rate over
// completions of replicas at full KV read 0.90-1.07 over 24 scrape
// intervals. 0.8 keeps that inside, and bounds what one slow replica can
// over-order to 1.25x.
// The default of analyzerTuning.admission.siblingMuTolerance.
const SiblingMuTolerance = config.DefaultSiblingMuTolerance

// siblingYield reports the rate a measured mu of the throughput key yields
// to, and whether it does: the newest median its Ready, full siblings served
// (served.saturated; zero when there is none this cycle), while the
// measurement sits under SiblingMuTolerance of it.
//
// A yield opens only on a median that includes an unqueued replica
// (served.unqueued). One read from queued replicas alone moves a yield
// already open and drops it once the measurement is no longer under it: the
// replicas the measurement yielded to are now measured themselves, at what
// it reads. Kept on the figure they served before, two replicas queued at
// 2.7 beside two at 3.6 that later queued at 2.7 too priced the fleet at 3.6
// for as long as the window lived (review, round 5).
//
// Remembered, not read fresh each cycle. Only queued replicas are measured, so
// one slow replica -- a throttled GPU, a CPU-starved pod -- can be the only
// reading. Below half its siblings the served bound refuses it
// (throughputRefusal); at 0.55x it passes, sets the window, and prices the
// fleet at 1.8x the replicas. A fleet ordered that large stops saturating, so
// its siblings stop reading full, and evidence read only while it lasts would
// give way to the slow figure again the cycle after. The yield holds until
// newer sibling evidence moves or drops it, the measurement comes within the
// tolerance of it, or the window starts over (recordSaturatedThroughputFrom).
func (a *SaturationAnalyzer) siblingYield(key string, reading throughputReading, served servedRates, t tuning) (float64, bool) {
	tol := t.Admission.SiblingMuTolerance
	if !(reading.rate > 0) {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.throughputMuChoice[key]
	if sibling := served.saturated; sibling > 0 {
		open := c != nil && c.sibling > 0
		switch {
		case !open && (!served.unqueued || !(reading.rate < tol*sibling)):
			return 0, false
		case !served.unqueued && !(reading.rate < tol*sibling):
			c.sibling = 0 // the evidence is gone: the siblings read what it does
			return 0, false
		}
		if c == nil {
			c = &muChoice{}
			a.throughputMuChoice[key] = c
		}
		c.sibling = sibling
	}
	if c == nil || !(reading.rate < tol*c.sibling) {
		return 0, false
	}
	return c.sibling, true
}

// muChoice is one throughput key's memory for choosing between its measured
// mu and the derived one (SaturationAnalyzer.throughputMuChoice).
type muChoice struct {
	// saturatedAt is when the bucket last read saturated at a rate within
	// MeasuredDerivedMaxRatio of its window's median, the reading accepted
	// or refused.
	saturatedAt time.Time
	// yielded is set while a stale measurement has given way to the derived
	// figure, and latches it (measuredYieldsToDerived).
	yielded bool
	// sibling is the newest rate the key's Ready, full replicas served
	// (servedRates.saturated), which a measurement under SiblingMuTolerance
	// of it yields to (siblingYield); zero when none was read, or once
	// queued siblings read what the measurement does.
	sibling float64
	// low is, per replica, its current run of saturated readings refused as
	// under MinMeasuredOfDerived of the derived figure.
	low map[string]lowRun
}

// lowRun is one replica's run of saturated readings refused as low.
type lowRun struct {
	since, last time.Time
}

// noteSaturatedReading records one replica's saturated reading mu of the
// throughput key, before it is recorded or refused, and reports whether a
// reading refused as under the derived figure (refusedLow) has repeated for
// MeasuredLowConfirmFor and is to be recorded after all.
//
// A reading that agrees with the window's median within
// MeasuredDerivedMaxRatio keeps the measurement fresh, accepted or not: the
// bucket is still reading what it measured. One that disagrees does not, and
// a filling replica's reading -- a fraction of the rate on record -- is one.
func (a *SaturationAnalyzer) noteSaturatedReading(key, pod string, mu float64, refusedLow bool, t tuning, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.throughputMuChoice[key]
	if !ok {
		c = &muChoice{}
		a.throughputMuChoice[key] = c
	}
	if ra, ok := a.saturatedThroughput[key]; ok && ra.Len() > 0 {
		if m := ra.Median(); m > 0 && mu <= m*t.MuChoice.MaxRatio && mu >= m/t.MuChoice.MaxRatio {
			c.saturatedAt = now
			c.yielded = false
		}
	}
	for p, r := range c.low {
		if now.Sub(r.last) > ThroughputSampleSpacing {
			delete(c.low, p)
		}
	}
	if !refusedLow {
		delete(c.low, pod)
		return false
	}
	if c.low == nil {
		c.low = make(map[string]lowRun)
	}
	r, had := c.low[pod]
	if !had {
		r.since = now
	}
	r.last = now
	c.low[pod] = r
	return now.Sub(r.since) >= MeasuredLowConfirmFor
}

// measuredYieldsToDerived reports whether the measured mu of the throughput
// key, which useDerived would keep, gives way to the derived one this cycle:
//
//   - a measurement under the derived figure gives way, fresh or not, while
//     served -- the variant's highest served rate this cycle (servedBounds) --
//     is more than MeasuredDerivedMaxRatio times it, or, once it has given
//     way, still MeasuredDerivedReturnRatio times it;
//   - otherwise only a stale one: its bucket has not read saturated in
//     agreement with it, nor taken a sample, for MeasuredMuFreshFor. It gives
//     way when it sits more than MeasuredDerivedMaxRatio above the derived
//     figure, or, once it has given way, still MeasuredDerivedReturnRatio
//     above it; and when it sits as far under it, but only once it has been
//     stale for MeasuredLowHoldFor -- until then the lower of the two prices.
func (a *SaturationAnalyzer) measuredYieldsToDerived(key string, reading throughputReading, derived derivedMu, served float64, t tuning, now time.Time) bool {
	maxR, retR := t.MuChoice.MaxRatio, t.MuChoice.ReturnRatio
	if !derived.ok || !(derived.rate > 0) || !(reading.rate > 0) {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.throughputMuChoice[key]
	latched := c != nil && c.yielded
	ratio := reading.rate / derived.rate
	if ratio < 1 && (served > maxR*reading.rate ||
		(latched && served > retR*reading.rate)) {
		if c == nil {
			c = &muChoice{}
			a.throughputMuChoice[key] = c
		}
		c.yielded = true
		a.paths.add(pathYieldServed, 1)
		return true
	}
	// The last reading that agreed with the measurement; the newest sample
	// only where none is on record (a window restored, or its first
	// sample). A sample is not freshness by itself: a confirmed low rate
	// is written under a median it contradicts.
	fresh := reading.sampledAt
	if c != nil && !c.saturatedAt.IsZero() {
		fresh = c.saturatedAt
	}
	if !fresh.IsZero() && now.Sub(fresh) < t.MuChoice.FreshFor.Duration() {
		if c != nil {
			c.yielded = false
		}
		return false
	}
	var yielded bool
	if ratio >= 1 {
		yielded = ratio > maxR || (latched && ratio > retR)
	} else {
		// An unknown age is as stale as can be, as it is above.
		heldOut := fresh.IsZero() || now.Sub(fresh) >= t.MuChoice.LowHoldFor.Duration()
		yielded = heldOut && (ratio < 1/maxR || (latched && ratio < 1/retR))
	}
	if c == nil {
		if !yielded {
			return false
		}
		c = &muChoice{}
		a.throughputMuChoice[key] = c
	}
	c.yielded = yielded
	if yielded {
		a.paths.add(pathYieldStale, 1)
	}
	return yielded
}
