package saturation

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
)

// Edges by enumeration (2.6): the operating point on each threshold of 2.6's
// table that today's code has, and either side of it. One case per threshold;
// where the threshold is a policy key, a second case runs it at a value an
// operator may set. The noisy edges (the mu latch, backlog growth, the idle
// line, the k2 key, the bimodal fit) are in the learners' own files; these
// are the remaining thresholds, each held at the threshold and one step
// either side.

// probe is a learner's verdict with its operating point at x.
type probe func(r *rand.Rand, x float64) (bool, error)

// edge3 holds a verdict below, at and above a threshold to want.
func edge3(what string, p probe, below, at, above float64, want [3]bool) func(uint64) error {
	return func(seed uint64) error {
		for i, x := range []float64{below, at, above} {
			got, err := p(propRand(seed), x)
			if err != nil {
				return err
			}
			if got != want[i] {
				return violation(kindWrongSide, "%s at %g: %v, want %v", what, x, got, want[i])
			}
		}
		return nil
	}
}

// edgeCase is one enumerated threshold as a property case.
func edgeCase(learner, name string, tn propTuning, pol *config.ScalingPolicy, check func(uint64) error) propCase {
	return propCase{class: classEdges, learner: learner, name: name, family: familyDirect, tuning: tn,
		policy: pol, seeds: 5, check: check}
}

func edgeCases() []propCase {
	nd := tuningWith
	polOf := tuningPolicy
	queuePol := &config.ScalingPolicy{QueueLengthThreshold: 10}
	kvPol := &config.ScalingPolicy{KvCacheThreshold: 0.9}
	idlePol := &config.ScalingPolicy{LoadImbalance: config.LoadImbalance{IdleKvUsage: 0.2}}
	scaleUpPol := &config.ScalingPolicy{ScaleUpThreshold: 0.9}
	band := nd(func(t *config.AnalyzerTuning) { t.ITLBand.MinObservableK, t.ITLBand.MaxObservableK = 0.10, 0.90 })
	capRate := 0.05
	capped := nd(func(t *config.AnalyzerTuning) { t.ITLBand.CapMaxPreemptionRate = &capRate })
	settled := nd(func(t *config.AnalyzerTuning) { t.SteadyGate.MinSettledSamples = 5 })
	drift := nd(func(t *config.AnalyzerTuning) { t.SteadyGate.MaxKDrift = 0.2 })
	idle5 := tuningNonDefault
	rate := nd(func(t *config.AnalyzerTuning) { t.K2Settle.RateFraction = 0.5 })
	rise := nd(func(t *config.AnalyzerTuning) { t.K2Settle.MaxRise = 0.4 })
	ratio4 := nd(func(t *config.AnalyzerTuning) { t.MuChoice.MaxRatio = 4 })
	minDer := nd(func(t *config.AnalyzerTuning) { t.MuChoice.MinMeasuredOfDerived = 0.3 })
	tokens := nd(func(t *config.AnalyzerTuning) { t.Admission.MaxTokensOverCompletions = 1.5 })
	sib := nd(func(t *config.AnalyzerTuning) { t.Admission.SiblingMuTolerance = 0.6 })
	fresh := nd(func(t *config.AnalyzerTuning) { t.MuChoice.FreshFor = config.PolicyDuration(10 * time.Minute) })
	low := nd(func(t *config.AnalyzerTuning) { t.MuChoice.LowHoldFor = config.PolicyDuration(45 * time.Minute) })
	ret := nd(func(t *config.AnalyzerTuning) { t.MuChoice.ReturnRatio = 1.3 })
	trust := nd(func(t *config.AnalyzerTuning) {
		t.FloorTrust.LagFraction, t.FloorTrust.TrustFraction = 0.3, 0.9
	})
	growth := nd(func(t *config.AnalyzerTuning) { t.BacklogGrowth.MinRequests, t.BacklogGrowth.Fraction = 5, 0.2 })
	return []propCase{
		// ITL.
		edgeCase("itl", "ols-min-rows", tunedDefault, nil, edge3("OLS at n steady rows", probeOLSRows, 9, 10, 11, [3]bool{false, true, true})),
		edgeCase("itl", "ols-min-spread", tunedDefault, nil, edge3("OLS at spread", probeOLSSpread, 0.29, 0.30, 0.31, [3]bool{false, true, true})),
		edgeCase("itl", "min-settled-samples", tunedDefault, nil, edge3("pinned fit at n settled readings", probeSettled(tuningDefault), 2, 3, 4, [3]bool{false, true, true})),
		edgeCase("itl", "min-settled-samples-nondefault", tunedPolicy, polOf(settled),
			edge3("pinned fit at n settled readings", probeSettled(settled), 4, 5, 6, [3]bool{false, true, true})),
		edgeCase("itl", "steady-for", tunedDefault, nil, edge3("a joiner settling after s", probeSteadyFor(tuningDefault), 105, 120, 135, [3]bool{true, false, false})),
		edgeCase("itl", "steady-for-nondefault", tunedNonDefault, polOf(tuningNonDefault),
			edge3("a joiner settling after s", probeSteadyFor(tuningNonDefault), 585, 600, 615, [3]bool{true, false, false})),
		edgeCase("itl", "max-k-drift", tunedDefault, nil, edge3("steady after a drift of", probeDrift(tuningDefault), 0.09, 0.10, 0.11, [3]bool{true, true, false})),
		edgeCase("itl", "max-k-drift-nondefault", tunedPolicy, polOf(drift),
			edge3("steady after a drift of", probeDrift(drift), 0.19, 0.20, 0.21, [3]bool{true, true, false})),
		edgeCase("itl", "idle-rows", tunedDefault, nil, edge3("restarted as a joiner after idle rows", probeIdleRows(tuningDefault), 1, 2, 3, [3]bool{false, true, true})),
		edgeCase("itl", "idle-rows-nondefault", tunedNonDefault, polOf(idle5),
			edge3("restarted as a joiner after idle rows", probeIdleRows(idle5), 4, 5, 6, [3]bool{false, true, true})),
		edgeCase("itl", "band-lower-edge", tunedDefault, nil, edge3("a reading at k", probeBand(tuningDefault), 0.149, 0.15, 0.151, [3]bool{false, true, true})),
		edgeCase("itl", "band-upper-edge", tunedDefault, nil, edge3("a reading at k", probeBand(tuningDefault), 0.799, 0.80, 0.801, [3]bool{true, true, false})),
		edgeCase("itl", "band-edges-nondefault", tunedPolicy, polOf(band),
			func(seed uint64) error {
				if err := edge3("a reading at k", probeBand(band), 0.099, 0.10, 0.101, [3]bool{false, true, true})(seed); err != nil {
					return err
				}
				return edge3("a reading at k", probeBand(band), 0.899, 0.90, 0.901, [3]bool{true, true, false})(seed)
			}),
		// The default cap is 0 and a preemption rate is never negative, so this
		// threshold has no lower side: below is the threshold itself. The
		// non-default case below has one.
		edgeCase("itl", "cap-preemption", tunedDefault, nil, edge3("a capped reading at preemption rate", probeCap(tuningDefault), 0, 0, 0.001, [3]bool{true, true, false})),
		edgeCase("itl", "cap-preemption-nondefault", tunedPolicy, polOf(capped),
			edge3("a capped reading at preemption rate", probeCap(capped), 0.049, 0.05, 0.051, [3]bool{true, true, false})),
		edgeCase("itl", "window-20-rows", tunedDefault, nil, edge3("window holds every reading of", probeWindowRows, 19, 20, 21, [3]bool{true, true, false})),
		edgeCase("itl", "window-30-min", tunedDefault, nil, edge3("a reading kept at age min", probeWindowAge, 29.75, 30, 30.25, [3]bool{true, true, false})),
		edgeCase("itl", "steady-window-40-rows", tunedDefault, nil, edge3("steady window holds every reading of", probeSteadyRows, 39, 40, 41, [3]bool{true, true, false})),
		// k2.
		edgeCase("k2", "queue-threshold", tunedDefault, nil, edge3("k2 learned at queue", probeK2Queue(0), 4, 5, 6, [3]bool{false, true, true})),
		edgeCase("k2", "queue-threshold-nondefault", tunedPolicy, queuePol,
			edge3("k2 learned at queue", probeK2Queue(queuePol.QueueLengthThreshold), 9, 10, 11, [3]bool{false, true, true})),
		edgeCase("k2", "joined-1-min", tunedDefault, nil, edge3("k2 learned from a replica Ready s before", probeJoined, 59, 60, 61, [3]bool{false, true, true})),
		edgeCase("k2", "rate-fraction", tunedDefault, nil, edge3("a suspect reading admitted at a rate fraction", probeRateFraction(tuningDefault), 0.69, 0.70, 0.71, [3]bool{false, true, true})),
		edgeCase("k2", "rate-fraction-nondefault", tunedPolicy, polOf(rate),
			edge3("a suspect reading admitted at a rate fraction", probeRateFraction(rate), 0.49, 0.50, 0.51, [3]bool{false, true, true})),
		edgeCase("k2", "max-rise", tunedDefault, nil, edge3("a suspect reading admitted after a rise of", probeMaxRise(tuningDefault), 0.24, 0.25, 0.26, [3]bool{true, true, false})),
		edgeCase("k2", "max-rise-nondefault", tunedPolicy, polOf(rise),
			edge3("a suspect reading admitted after a rise of", probeMaxRise(rise), 0.39, 0.40, 0.41, [3]bool{true, true, false})),
		edgeCase("k2", "window-10", tunedDefault, nil, edge3("k2 window holds every reading of", probeK2Window, 9, 10, 11, [3]bool{true, true, false})),
		edgeCase("k2", "episode-gap-24h", tunedDefault, nil, edge3("k2 window continues after a gap of h", probeEpisodeGap, 23.9, 24, 24.1, [3]bool{true, true, false})),
		edgeCase("k2", "eviction-7d", tunedDefault, nil,
			edge3("k2 window kept by the sweep at min from 7 d unused", probeK2Eviction(7*24*time.Hour, false), -1, 0, 1, [3]bool{true, true, false})),
		// The retention is an environment knob (LEARNED_STATE_BUCKET_RETENTION),
		// not a policy key, so this case runs the default policy.
		edgeCase("k2", "eviction-nondefault", tunedDefault, nil,
			edge3("k2 window kept by the sweep at min from 24 h unused", probeK2Eviction(24*time.Hour, true), -1, 0, 1, [3]bool{true, true, false})),
		// Borrowed k2.
		edgeCase("borrowed-k2", "kv-total-3pct", tunedDefault, nil, edge3("borrowed across a KV total gap of", probeBorrowKv, 0.029, 0.03, 0.031, [3]bool{true, true, false})),
		edgeCase("borrowed-k2", "hold-30-min", tunedDefault, nil, edge3("still at full strength after min", probeBorrowHold, 29, 30, 31, [3]bool{true, true, false})),
		edgeCase("borrowed-k2", "relax-6h", tunedDefault, nil, edge3("still priced after the hold and h of relaxation", probeBorrowRelax, 5.99, 6.0, 6.01, [3]bool{true, false, false})),
		edgeCase("borrowed-k2", "rearm-20pct", tunedDefault, nil, edge3("a spent bound re-armed by a figure at a ratio of", probeBorrowRearm, 0.79, 0.80, 0.81, [3]bool{true, false, false})),
		// mu.
		edgeCase("mu", "half-of-served", tunedDefault, nil, edge3("a reading admitted at a ratio to served of", probeServed(tuningDefault), 0.49, 0.50, 0.51, [3]bool{false, true, true})),
		edgeCase("mu", "half-of-served-nondefault", tunedPolicy, polOf(ratio4),
			edge3("a reading admitted at a ratio to served of", probeServed(ratio4), 0.24, 0.25, 0.26, [3]bool{false, true, true})),
		edgeCase("mu", "min-measured-of-derived", tunedDefault, nil, edge3("a reading admitted at a ratio to derived of", probeDerived(tuningDefault), 0.49, 0.50, 0.51, [3]bool{false, true, true})),
		edgeCase("mu", "min-measured-of-derived-nondefault", tunedPolicy, polOf(minDer),
			edge3("a reading admitted at a ratio to derived of", probeDerived(minDer), 0.29, 0.30, 0.31, [3]bool{false, true, true})),
		edgeCase("mu", "pricing-k-across-min-measured", tunedPolicy, scaleUpPol,
			edge3("a reading at half the derived mu priced at scaleUpThreshold 0.9, admitted at scaleUpThreshold",
				probePricingK(scaleUpPol.ScaleUpThreshold), 0.89, scaleUpPol.ScaleUpThreshold, 0.91, [3]bool{true, true, false})),
		edgeCase("mu", "low-confirm-2-min", tunedDefault, nil, edge3("a low reading confirmed after s", probeLowConfirm, 105, 120, 135, [3]bool{false, true, true})),
		edgeCase("mu", "max-tokens-over-completions", tunedDefault, nil, edge3("a replica filling at a token ratio of", probeFilling(tuningDefault), 1.14, 1.15, 1.16, [3]bool{false, false, true})),
		edgeCase("mu", "max-tokens-over-completions-nondefault", tunedPolicy, polOf(tokens),
			edge3("a replica filling at a token ratio of", probeFilling(tokens), 1.49, 1.50, 1.51, [3]bool{false, false, true})),
		edgeCase("mu", "sibling-tolerance", tunedDefault, nil, edge3("yields to its siblings at a ratio of", probeSibling(tuningDefault), 0.79, 0.80, 0.81, [3]bool{true, false, false})),
		edgeCase("mu", "sibling-tolerance-nondefault", tunedPolicy, polOf(sib),
			edge3("yields to its siblings at a ratio of", probeSibling(sib), 0.59, 0.60, 0.61, [3]bool{true, false, false})),
		edgeCase("mu", "fresh-for", tunedDefault, nil, edge3("a high measurement yields after min", probeStale(tuningDefault, 3), 4.75, 5, 5.25, [3]bool{false, true, true})),
		edgeCase("mu", "fresh-for-nondefault", tunedPolicy, polOf(fresh),
			edge3("a high measurement yields after min", probeStale(fresh, 3), 9.75, 10, 10.25, [3]bool{false, true, true})),
		edgeCase("mu", "low-hold-for", tunedDefault, nil, edge3("a low measurement yields after min", probeStale(tuningDefault, 1.0/3), 29.75, 30, 30.25, [3]bool{false, true, true})),
		edgeCase("mu", "low-hold-for-nondefault", tunedPolicy, polOf(low),
			edge3("a low measurement yields after min", probeStale(low, 1.0/3), 44.75, 45, 45.25, [3]bool{false, true, true})),
		edgeCase("mu", "latch-return", tunedDefault, nil, edge3("a latched measurement still yields at a ratio of", probeLatch(tuningDefault), 1.59, 1.60, 1.61, [3]bool{false, false, true})),
		edgeCase("mu", "latch-return-nondefault", tunedPolicy, polOf(ret),
			edge3("a latched measurement still yields at a ratio of", probeLatch(ret), 1.29, 1.30, 1.31, [3]bool{false, false, true})),
		edgeCase("mu", "stale-agreement-2x", tunedDefault, nil, edge3("a reading at a ratio to the median keeps it fresh", probeAgreement, 1.99, 2, 2.01, [3]bool{true, true, false})),
		// Floor.
		edgeCase("floor", "trust-blend", tunedDefault, nil, func(seed uint64) error { return checkTrust(tuningDefault, 0.5, 0.8) }),
		edgeCase("floor", "trust-blend-nondefault", tunedPolicy, polOf(trust),
			func(seed uint64) error { return checkTrust(trust, 0.3, 0.9) }),
		edgeCase("floor", "growth-band", tunedDefault, nil, func(seed uint64) error { return checkGrowth(tuningDefault, 2, 0.10) }),
		edgeCase("floor", "growth-band-nondefault", tunedPolicy, polOf(growth),
			func(seed uint64) error { return checkGrowth(growth, 5, 0.2) }),
		edgeCase("floor", "standing-queue-cap", tunedDefault, nil,
			edge3("a queue counted whole at requests over what its replica drains in the 60 s drain time", probeStandingQueue(30, 60), -1, 0, 1, [3]bool{true, true, false})),
		edgeCase("floor", "standing-queue-cap-start-over-drain", tunedDefault, nil,
			edge3("a queue counted whole at requests over what its replica drains in a 70 s start over the drain time", probeStandingQueue(DefaultReplicaStartSeconds, 70), -1, 0, 1, [3]bool{true, true, false})),
		// Idle room and the decode memory.
		edgeCase("idle", "settled-2-min", tunedDefault, nil, edge3("idle room settled after s", probeIdleSettled, 105, 120, 135, [3]bool{false, true, true})),
		edgeCase("idle", "gap-1-min", tunedDefault, nil, edge3("an idle run survives a gap of s", probeIdleGap, 45, 60, 75, [3]bool{true, true, false})),
		edgeCase("idle", "idle-kv-usage", tunedDefault, nil, edge3("idle at KV", probeIdleKv(0), 0.29, 0.30, 0.31, [3]bool{true, false, false})),
		edgeCase("idle", "idle-kv-usage-nondefault", tunedPolicy, idlePol,
			edge3("idle at KV", probeIdleKv(idlePol.IdleKvUsage()), 0.19, 0.20, 0.21, [3]bool{true, false, false})),
		edgeCase("decode-memory", "memory-1-min", tunedDefault, nil, edge3("decode remembered full after s", probeDecodeMemory, 45, 60, 75, [3]bool{true, false, false})),
		edgeCase("decode-memory", "queue-threshold", tunedDefault, nil, edge3("decode full at queue", probeDecodeQueue, 4, 5, 6, [3]bool{false, true, true})),
		edgeCase("decode-memory", "kv-threshold", tunedDefault, nil, edge3("decode full at KV", probeDecodeKv(0.8), 0.79, 0.80, 0.81, [3]bool{false, true, true})),
		edgeCase("decode-memory", "kv-threshold-nondefault", tunedPolicy, kvPol,
			edge3("decode full at KV", probeDecodeKv(kvPol.KvCacheThreshold), 0.89, 0.90, 0.91, [3]bool{false, true, true})),
		// Shape.
		edgeCase("shape-hold", "tolerance-20pct", tunedDefault, nil, edge3("a change declared at an output change of", probeShapeTolerance, 0.19, shapeTolerance, 0.21, [3]bool{false, false, true})),
		// Start time.
		edgeCase("start-time", "outlier-3x-at", tunedDefault, nil, edge3("a sample taken at a multiple of the estimate of", probeStartOutlier, 2.99, 3, 3.01, [3]bool{true, true, false})),
		edgeCase("start-time", "one-pod-repeating", tunedDefault, nil, edge3("one outlier pod taken on cycle", probeStartConfirm, 3, 4, 20, [3]bool{false, false, false})),
		edgeCase("start-time", "distinct-pods-3", tunedDefault, nil, edge3("outlier pods taken at distinct pods", probeStartConfirmPods, 2, 3, 4, [3]bool{false, true, true})),
		edgeCase("start-time", "alpha-030", tunedDefault, nil, checkStartAlpha),
		edgeCase("start-time", "zero-sample", tunedDefault, nil, edge3("a sample of s moves the estimate", probeStartZero, -1, 0, 1, [3]bool{false, false, true})),
	}
}

// --- ITL ---

// observeOnce offers rows to a fresh tracker, all present on its first cycle.
func observeOnce(tn config.AnalyzerTuning, rows []domain.ReplicaMetrics) itl.Reading {
	lo, hi := (&config.ScalingPolicy{AnalyzerTuning: tn}).ITLBand()
	adm := itl.Admission{MinK: lo, MaxK: hi, Steady: steadinessOf(tn), TierHysteresis: true}
	return itl.NewTracker().Observe(propITLKey, "v", rows, adm, propStart)
}

func fleetAt(ks []float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, len(ks))
	for i, k := range ks {
		rows[i] = itlRow(fmt.Sprintf("r%d", i), k, propITL(k))
	}
	return rows
}

// probeOLSRows: n replicas spread over 0.40 of k on one cycle: a full fit from
// ten steady readings, not nine.
func probeOLSRows(_ *rand.Rand, n float64) (bool, error) {
	ks := make([]float64, int(n))
	for i := range ks {
		ks[i] = 0.25 + 0.40*float64(i)/float64(len(ks)-1)
	}
	return observeOnce(tuningDefault, fleetAt(ks)).Tier == itl.TierOLS, nil
}

// probeOLSSpread: ten replicas spread over s from k = 0.45.
func probeOLSSpread(_ *rand.Rand, s float64) (bool, error) {
	ks := make([]float64, 10)
	for i := range ks {
		ks[i] = 0.45 + s*float64(i)/9
	}
	ks[9] = 0.45 + s
	return observeOnce(tuningDefault, fleetAt(ks)).Tier == itl.TierOLS, nil
}

// probeSettled: n replicas past their settle and ten joiners settling: the
// pinned fit takes the n, from minSettledSamples on.
func probeSettled(tn config.AnalyzerTuning) probe {
	return func(_ *rand.Rand, n float64) (bool, error) {
		lo, hi := (&config.ScalingPolicy{AnalyzerTuning: tn}).ITLBand()
		adm := itl.Admission{MinK: lo, MaxK: hi, Steady: steadinessOf(tn)}
		tr := itl.NewTracker()
		old := make([]float64, int(n))
		for i := range old {
			old[i] = 0.40 + 0.01*float64(i)
		}
		tr.Observe(propITLKey, "v", fleetAt(old), adm, propStart)
		// The n readings stay in the window; the next cycle's rows are ten
		// joiners, all settling.
		rows := make([]domain.ReplicaMetrics, 0, 10)
		for j := range 10 {
			rows = append(rows, itlRow(fmt.Sprintf("j%d", j), 0.42, propITL(0.42)))
		}
		r := tr.Observe(propITLKey, "v", rows, adm, propStart.Add(propCycle))
		return r.Tier == itl.TierPinnedB && !r.SlopeHeld, nil
	}
}

// probeSteadyFor: a replica joins, and reads s seconds later: settling until
// steadyFor.
func probeSteadyFor(tn config.AnalyzerTuning) probe {
	return func(_ *rand.Rand, s float64) (bool, error) {
		// The engine's learner, past the controller start's own settle, so
		// only the joiner can be settling.
		l := newTrackerITL(steadinessOf(tn))
		join := pastRestart(l, fleetAt([]float64{0.4}), steadinessOf(tn))
		rows := func() []domain.ReplicaMetrics {
			return append(fleetAt([]float64{0.4}), itlRow("joiner", 0.5, propITL(0.5)))
		}
		l.observe(rows(), join)
		var r itl.Reading
		for at := join.Add(propCycle); !at.After(join.Add(time.Duration(s) * time.Second)); at = at.Add(propCycle) {
			r = l.observe(rows(), at)
		}
		return r.Settling > 0, nil
	}
}

// pastRestart observes rows on l from propStart until the controller
// start's settle (s.For) has run out, and returns the next cycle's instant.
func pastRestart(l *trackerITL, rows []domain.ReplicaMetrics, s itl.Steadiness) time.Time {
	at := propStart
	for ; !at.After(propStart.Add(s.For)); at = at.Add(propCycle) {
		l.observe(rows, at)
	}
	return at
}

// idleRow is r0 idle on positive evidence (settle.Row.Activity): a known KV
// fill of 0 and a known empty ITL window.
func idleRow() domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: "r0", VariantName: "v", Ready: true, KvCacheUsageKnown: true,
		KvUsageInstantKnown: true, AvgITLEmpty: true}
}

// probeDrift: a replica present from the first cycle moves its k by d over
// one cycle: steady up to maxKDrift.
func probeDrift(tn config.AnalyzerTuning) probe {
	return func(_ *rand.Rand, d float64) (bool, error) {
		adm := itl.Admission{Steady: steadinessOf(tn)}
		tr := itl.NewTracker()
		tr.Observe(propITLKey, "v", fleetAt([]float64{0.5}), adm, propStart)
		r := tr.Observe(propITLKey, "v", fleetAt([]float64{0.5 + d}), adm, propStart.Add(propCycle))
		return r.Offered == 1 && r.Unsteady == 0, nil
	}
}

// probeIdleRows: a replica reads n rows idle on positive evidence (a known
// KV of 0 and a known empty ITL window, learner PR 4a), then serves again:
// from idleRows such rows its settle starts again, and it is settling.
func probeIdleRows(tn config.AnalyzerTuning) probe {
	return func(_ *rand.Rand, n float64) (bool, error) {
		l := newTrackerITL(steadinessOf(tn))
		at := pastRestart(l, fleetAt([]float64{0.4}), steadinessOf(tn))
		for range int(n) {
			l.observe([]domain.ReplicaMetrics{idleRow()}, at)
			at = at.Add(propCycle)
		}
		r := l.observe(fleetAt([]float64{0.4}), at)
		return r.Settling > 0, nil
	}
}

// probeBand: one replica at k: a reading inside the band, not outside it.
func probeBand(tn config.AnalyzerTuning) probe {
	return func(_ *rand.Rand, k float64) (bool, error) {
		return observeOnce(tn, fleetAt([]float64{k})).Offered == 1, nil
	}
}

// probeCap: a replica at k 0.85, above the band, at its sequence cap with
// requests waiting, preempting x a second: its reading is taken up to
// capMaxPreemptionRate.
func probeCap(tn config.AnalyzerTuning) probe {
	return func(_ *rand.Rand, x float64) (bool, error) {
		rm := itlRow("r0", 0.85, propITL(0.85))
		rm.RunningRequests, rm.QueueLength = 256, 4
		rm.PreemptionRate, rm.PreemptionRateKnown = x, true
		pol := &config.ScalingPolicy{AnalyzerTuning: tn}
		lo, hi := pol.ITLBand()
		adm := itl.Admission{MinK: lo, MaxK: hi, MaxNumSeqs: 256, CapMaxPreemptionRate: pol.ITLCapPreemptionRate(), Steady: steadinessOf(tn)}
		return itl.NewTracker().Observe(propITLKey, "v", []domain.ReplicaMetrics{rm}, adm, propStart).Capped == 1, nil
	}
}

// probeWindowRows: n readings over n cycles: the window holds all of them up
// to its 20.
func probeWindowRows(_ *rand.Rand, n float64) (bool, error) {
	tr := itl.NewTracker()
	var r itl.Reading
	for i := range int(n) {
		r = tr.Observe(propITLKey, "v", fleetAt([]float64{0.4 + 0.001*float64(i%3)}), itl.Admission{}, propStart.Add(time.Duration(i)*propCycle))
	}
	return r.Held == int(n), nil
}

// probeWindowAge: one reading, then the window read m minutes later: kept up
// to 30 min.
func probeWindowAge(_ *rand.Rand, m float64) (bool, error) {
	tr := itl.NewTracker()
	tr.Observe(propITLKey, "v", fleetAt([]float64{0.4}), itl.Admission{}, propStart)
	r := tr.Observe(propITLKey, "v", nil, itl.Admission{}, propStart.Add(time.Duration(m*float64(time.Minute))))
	return r.Held == 1, nil
}

// --- k2 ---

// probeK2Queue: one replica full with q waiting: k2 learns from it at the
// queue threshold (5, or the policy's).
func probeK2Queue(threshold float64) probe {
	return func(r *rand.Rand, q float64) (bool, error) {
		rig := newPropRig()
		if threshold > 0 {
			rig.policy = func(p *config.ScalingPolicy) { p.QueueLengthThreshold = threshold }
		}
		err := rig.runK2(k2Reading{int64(float64(propKv) * (0.88 + 0.05*r.Float64())), int(q), 6, propCycle})
		return rig.k2Admitted(), err
	}
}

// probeJoined: a replica Ready s seconds before its first saturated reading,
// on the analyzer's first cycle: k2 learns from it once a minute has passed,
// at 60 s and not at 59 s.
func probeJoined(r *rand.Rand, s float64) (bool, error) {
	rig := newPropRig()
	rm := satRow("r0", int64(float64(propKv)*(0.88+0.05*r.Float64())), 10, 6)
	rm.ReadySince = rig.clock.Add(-time.Duration(s) * time.Second)
	_, err := rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 6, propCycle)
	return rig.k2Admitted(), err
}

// settleIn is a saturated reading whose queue overflows its batch, so the
// settle test applies, with the rate and k2 on record.
func settleIn(tn config.AnalyzerTuning, tokens int64) k2SettleInput {
	return k2SettleInput{namespace: "ns", modelID: "m", variant: "v", pod: "r0", queue: 200, tokens: tokens, ranks: 1,
		perRequest: 2000, threshold: 5, ready: true, tune: tuning{AnalyzerTuning: tn.WithDefaults()}}
}

// probeRateFraction: a suspect reading at x of the rate on record, holding
// the k2 on record: admitted from rateFraction.
func probeRateFraction(tn config.AnalyzerTuning) probe {
	return func(r *rand.Rand, x float64) (bool, error) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.now = func() time.Time { return propStart }
		ref := 2 + r.Float64()
		in := settleIn(tn, 200_000)
		in.rate, in.rateOK, in.ref, in.k2Ref = x*ref, true, ref, 200_000
		return a.k2Unsettled(in) == "", nil
	}
}

// probeMaxRise: a suspect reading, queue standing a minute, its resident KV
// moved by x from a minute back: admitted within maxRise.
func probeMaxRise(tn config.AnalyzerTuning) probe {
	return func(r *rand.Rand, x float64) (bool, error) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		clock := propStart
		a.now = func() time.Time { return clock }
		base := int64(150_000 + r.IntN(50_000))
		for range 4 {
			a.k2Unsettled(settleIn(tn, base))
			clock = clock.Add(propCycle)
		}
		return a.k2Unsettled(settleIn(tn, int64(float64(base)*(1+x)))) == "", nil
	}
}

// probeK2Window: n saturated readings: the k2 window holds all of them up to
// its 10.
func probeK2Window(r *rand.Rand, n float64) (bool, error) {
	rig := newPropRig()
	tok := int64(float64(propKv) * (0.88 + 0.05*r.Float64()))
	for range int(n) {
		if err := rig.runK2(k2Reading{tok, 10, 6, propCycle}); err != nil {
			return false, err
		}
	}
	rig.a.mu.Lock()
	defer rig.a.mu.Unlock()
	for _, ra := range rig.a.computeCapacityHistory {
		return ra.Len() == int(n), nil
	}
	return false, errors.New("no k2 window")
}

// probeEpisodeGap: a window of two readings, a write gap of h hours, two
// more: the window continues within the 24 h episode gap and starts over
// past it.
func probeEpisodeGap(r *rand.Rand, h float64) (bool, error) {
	rig := newPropRig()
	tok := int64(float64(propKv) * (0.88 + 0.05*r.Float64()))
	// Two readings start the window: the first is held for the second
	// (k2_confirm.go), which writes both. Unseen for most of h hours, the
	// replica is then a new sighting (the settle tracker's absence), so its
	// readings wait a window: four refused, and the fifth read h hours after
	// the window's last write. Past the gap that fifth is a new window's
	// first, held for the sixth.
	readings := make([]k2Reading, 0, 8)
	readings = append(readings, k2Reading{tok, 10, 6, propCycle},
		k2Reading{tok, 10, 6, time.Duration(h*float64(time.Hour)) - JoinedReplicaWindow})
	for range 6 {
		readings = append(readings, k2Reading{tok, 10, 6, propCycle})
	}
	if err := rig.runK2(readings...); err != nil {
		return false, err
	}
	rig.a.mu.Lock()
	defer rig.a.mu.Unlock()
	for _, ra := range rig.a.computeCapacityHistory {
		return ra.Len() == 4, nil
	}
	return false, errors.New("no k2 window")
}

// --- borrowed k2 ---

// probeBorrowKv: a donor whose KV total is a fraction x under the
// borrower's: a candidate within 3 %.
func probeBorrowKv(_ *rand.Rand, x float64) (bool, error) {
	const own = int64(1_000_000)
	rig := newBorrowRig(6000, own, int64(float64(own)*(1-x)))
	_, ok := rig.at(14_000, own)
	return ok, nil
}

// probeBorrowHold: a bound served m minutes: at full strength through the hold.
func probeBorrowHold(_ *rand.Rand, m float64) (bool, error) {
	rig := newBorrowRig(7000, 0, 0)
	rig.at(14_000, 0)
	rig.serve(14_000, time.Duration(m)*time.Minute)
	b, ok := rig.at(14_000, 0)
	return ok && b.relaxed == 0, nil
}

// probeBorrowRelax: a bound served through its hold and h hours more: priced
// until the relaxation has run, and spent at exactly its 6 h.
func probeBorrowRelax(_ *rand.Rand, h float64) (bool, error) {
	rig := newBorrowRig(7000, 0, 0)
	rig.at(14_000, 0)
	rig.serve(14_000, BorrowedK2Hold+time.Duration(h*float64(time.Hour)))
	_, ok := rig.at(14_000, 0)
	return ok, nil
}

// probeBorrowRearm: a spent bound, then its donor measures x of what it ran
// on: re-armed only on a figure more than 20 % lower.
func probeBorrowRearm(_ *rand.Rand, x float64) (bool, error) {
	rig := newBorrowRig(5000, 0, 0)
	rig.at(14_000, 0)
	rig.serve(14_000, BorrowedK2Hold+BorrowedK2Relax+time.Minute)
	if _, ok := rig.at(14_000, 0); ok {
		return false, errors.New("precondition: the bound is spent")
	}
	ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, rig.clock)
	ra.AddAt(5000*x, rig.clock)
	rig.a.mu.Lock()
	rig.a.computeCapacityHistory[rig.donor] = ra
	rig.a.mu.Unlock()
	rig.clock = rig.clock.Add(time.Minute)
	_, ok := rig.at(14_000, 0)
	return ok, nil
}

// --- mu ---

// probeServed: a reading at x of what a Ready sibling served: admitted from
// 1/maxRatio.
func probeServed(tn config.AnalyzerTuning) probe {
	return func(r *rand.Rand, x float64) (bool, error) {
		served := 1 + r.Float64()
		mu := served * x
		return tuning{AnalyzerTuning: tn.WithDefaults()}.throughputRefusal(1, mu, whatif.DerivedMu{}, served, false) == "", nil
	}
}

// probeDerived: a reading at x of the derived mu: admitted from
// minMeasuredOfDerived.
func probeDerived(tn config.AnalyzerTuning) probe {
	return func(r *rand.Rand, x float64) (bool, error) {
		d := 1 + r.Float64()
		return tuning{AnalyzerTuning: tn.WithDefaults()}.throughputRefusal(1, x*d, whatif.DerivedMu{Rate: d, OK: true}, 0, false) == "", nil
	}
}

// probeLowConfirm: a replica refused as low on every cycle for s seconds:
// recorded once the run reaches 2 min.
func probeLowConfirm(_ *rand.Rand, s float64) (bool, error) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	var ok bool
	for at := time.Duration(0); at <= time.Duration(s)*time.Second; at += propCycle {
		ok = a.noteSaturatedReading(propMuKey, "p0", 0.3, true, 0, t, propStart.Add(at))
	}
	return ok, nil
}

// probeFilling: a replica generating x times the tokens its completions
// account for: filling past maxTokensOverCompletions.
func probeFilling(tn config.AnalyzerTuning) probe {
	return func(r *rand.Rand, x float64) (bool, error) {
		rm := satRow("p0", 300_000, 10, 2+r.Float64())
		// The same product the guard forms, so x at the threshold is it.
		rm.GenerationTokenRate = x * rm.RequestRate * 500
		return tuning{AnalyzerTuning: tn.WithDefaults()}.tokensOutrunCompletions(rm, domain.RoleDecode, 500), nil
	}
}

// probeSibling: a measurement at x of what its full, unqueued siblings
// serve: it yields under siblingMuTolerance.
func probeSibling(tn config.AnalyzerTuning) probe {
	return func(r *rand.Rand, x float64) (bool, error) {
		// The edge is the tolerance's, which the cap
		// (SwitchMuYieldCappedAtMeasurement) keeps from ever pricing: probed
		// with the cap off, where the yield still prices.
		a := NewSaturationAnalyzer(capacity.NewStore())
		if err := a.SetSwitchesForReplay(map[string]bool{SwitchMuYieldCappedAtMeasurement: false}); err != nil {
			return false, err
		}
		sib := 1 + r.Float64()
		_, y := a.siblingYield(propMuKey, throughputReading{rate: x * sib}, servedRates{max: sib, saturated: sib, unqueued: true},
			tuning{AnalyzerTuning: tn.WithDefaults()})
		return y, nil
	}
}

// probeStale: a measurement at ratio of the derived mu, its bucket last in
// agreement m minutes ago: it yields once stale (freshFor above derived,
// lowHoldFor below).
func probeStale(tn config.AnalyzerTuning, ratio float64) probe {
	return func(r *rand.Rand, m float64) (bool, error) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		d := 1 + r.Float64()
		now := propStart.Add(time.Hour)
		at := now.Add(-time.Duration(m * float64(time.Minute)))
		return a.measuredYieldsToDerived(propMuKey, throughputReading{rate: ratio * d, sampledAt: at}, whatif.DerivedMu{Rate: d, OK: true}, 0,
			tuning{AnalyzerTuning: tn.WithDefaults()}, now), nil
	}
}

// probeLatch: a stale measurement latched at 2.5x the derived mu comes back
// to x: still yielded above returnRatio. The latch reads the ratio smoothed
// over MuLatchRatioSmoothing (SwitchMuLatchReadsSmoothedRatio), so x is
// read latchSettle later, when nothing of 2.5x is left in it.
func probeLatch(tn config.AnalyzerTuning) probe {
	return func(r *rand.Rand, x float64) (bool, error) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		// A derived mu of 1, so the measured rate is the ratio exactly.
		d := whatif.DerivedMu{Rate: 1, OK: true}
		now := propStart.Add(time.Duration(r.IntN(60)) * time.Minute)
		if !a.measuredYieldsToDerived(propMuKey, throughputReading{rate: 2.5 * d.Rate}, d, 0, t, now) {
			return false, errors.New("precondition: a stale measurement at 2.5x did not yield")
		}
		return a.measuredYieldsToDerived(propMuKey, throughputReading{rate: x * d.Rate}, d, 0, t, now.Add(latchSettle)), nil
	}
}

// latchSettle is how long after a reading the latch's smoothed ratio
// (muChoice.smoothRatio) is the next reading's exactly: 40 time constants,
// e^-40 of the old ratio, which rounds the next reading's weight to 1.
const latchSettle = 40 * MuLatchRatioSmoothing

// probeAgreement: a saturated reading at x of the window's median refreshes
// the measurement within 2x.
func probeAgreement(r *rand.Rand, x float64) (bool, error) {
	m := newAnalyzerMu()
	level := 1 + r.Float64()
	m.read("p0", level)
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	now := m.clock.Add(time.Hour)
	m.a.noteSaturatedReading(propMuKey, "p0", x*level, false, 0, t, now)
	m.a.mu.Lock()
	defer m.a.mu.Unlock()
	return m.a.throughputMuChoice[propMuKey].saturatedAt.Equal(now), nil
}

// --- floor ---

// checkTrust: the floor's trust is 0 at or under lagFraction, 1 at or over
// trustFraction, and linear between.
func checkTrust(tn config.AnalyzerTuning, lag, full float64) error {
	t := tuning{AnalyzerTuning: tn.WithDefaults()}
	for _, c := range []struct{ ratio, want float64 }{
		{lag - 0.01, 0}, {lag, 0}, {lag + 0.01, 0.01 / (full - lag)},
		{(lag + full) / 2, 0.5}, {full - 0.01, 1 - 0.01/(full-lag)}, {full, 1}, {full + 0.01, 1},
	} {
		if got := t.floorTrust(c.ratio); relErr(got, c.want) > 1e-9 && !(c.want == 0 && got == 0) {
			return violation(kindWrongSide, "trust at %.2f of resident: %.4f, want %.4f", c.ratio, got, c.want)
		}
	}
	return nil
}

// checkGrowth: a backlog grows when it rises past max(minRequests, fraction
// of the backlog a minute back), not at it.
func checkGrowth(tn config.AnalyzerTuning, minReq, frac float64) error {
	t := tuning{AnalyzerTuning: tn.WithDefaults()}
	for _, prev := range []float64{1, 10, 100} {
		band := max(minReq, frac*prev)
		for _, c := range []struct {
			rise float64
			want bool
		}{{band - 0.01, false}, {band, false}, {band + 0.01, true}} {
			a := NewSaturationAnalyzer(capacity.NewStore())
			clock := propStart
			a.now = func() time.Time { return clock }
			a.noteBacklogGrowth("ns", "m", map[string]float64{"decode": prev}, t)
			clock = clock.Add(BacklogGrowthLookback)
			if got := a.noteBacklogGrowth("ns", "m", map[string]float64{"decode": prev + c.rise}, t)["decode"]; got != c.want {
				return violation(kindWrongSide, "backlog %.0f -> %.2f: grown %v, want %v", prev, prev+c.rise, got, c.want)
			}
		}
	}
	return nil
}

// --- idle room and the decode memory ---

// idleRig notes one decode replica's idle readings on one analyzer.
func idleRig() func(frac float64, now time.Time, idleKv float64) bool {
	a := NewSaturationAnalyzer(capacity.NewStore())
	return func(frac float64, now time.Time, idleKv float64) bool {
		rm := satRow("r0", int64(frac*float64(propKv)), 0, 0.5)
		caps := []capacity.ReplicaCapacity{{VariantName: "v", EffectiveCapacity: propKv}}
		if idleKv == 0 {
			idleKv = config.DefaultIdleKvUsage
		}
		t := tuning{AnalyzerTuning: tuningDefault.WithDefaults(), idleKvUsage: idleKv}
		return a.noteIdleRuns("ns", "m", caps, []domain.ReplicaMetrics{rm}, map[string]string{"v": domain.RoleDecode}, t, now)[0]
	}
}

// probeIdleSettled: idle on every cycle for s seconds: settled from 2 min.
func probeIdleSettled(_ *rand.Rand, s float64) (bool, error) {
	note := idleRig()
	var v bool
	for at := time.Duration(0); at <= time.Duration(s)*time.Second; at += propCycle {
		v = note(0.05, propStart.Add(at), 0)
	}
	return v, nil
}

// probeIdleGap: idle for 3 min, a gap of s seconds, idle again: the run
// survives a gap of up to a minute.
func probeIdleGap(_ *rand.Rand, s float64) (bool, error) {
	note := idleRig()
	at := propStart
	for ; at.Sub(propStart) <= 3*time.Minute; at = at.Add(propCycle) {
		note(0.05, at, 0)
	}
	return note(0.05, at.Add(-propCycle).Add(time.Duration(s)*time.Second), 0), nil
}

// probeIdleKv: a replica at KV x, read idle for 3 min: idle room under
// idleKvUsage.
func probeIdleKv(idleKv float64) probe {
	return func(_ *rand.Rand, x float64) (bool, error) {
		note := idleRig()
		var v bool
		for at := time.Duration(0); at <= 3*time.Minute; at += propCycle {
			v = note(x, propStart.Add(at), idleKv)
		}
		return v, nil
	}
}

// probeDecodeMemory: decode saturated once, then read clear s seconds later:
// remembered for under a minute.
func probeDecodeMemory(_ *rand.Rand, s float64) (bool, error) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := propStart
	a.now = func() time.Time { return clock }
	a.rememberDecodeSaturation("ns", "m", true)
	clock = clock.Add(time.Duration(s) * time.Second)
	return a.rememberDecodeSaturation("ns", "m", false), nil
}

// probeDecodeQueue: a full decode replica with q waiting: saturated from the
// queue threshold.
func probeDecodeQueue(_ *rand.Rand, q float64) (bool, error) {
	return roleSaturated([]domain.ReplicaMetrics{fullDecode("d0", 0.95, int(q))}, pdRoles, domain.RoleDecode, 5, 0.8), nil
}

// probeDecodeKv: a queued decode replica at KV x: full from kvCacheThreshold.
func probeDecodeKv(threshold float64) probe {
	return func(_ *rand.Rand, x float64) (bool, error) {
		return roleSaturated([]domain.ReplicaMetrics{fullDecode("d0", x, 10)}, pdRoles, domain.RoleDecode, 5, threshold), nil
	}
}

// --- shape ---

// probeShapeTolerance: a settled shape whose output length moves by x: a
// change past the tolerance, not at it.
func probeShapeTolerance(_ *rand.Rand, x float64) (bool, error) {
	rig := newShapeRig()
	for range 4 {
		rig.note(1000, 1000, ShapeChangeHoldMax)
	}
	_, _, held := rig.note(1000, 1000*(1+x), ShapeChangeHoldMax)
	return held, nil
}

// --- start time ---

// probeStartOutlier: an estimate of 100 s, then a pod at x times it: taken
// up to 3x.
func probeStartOutlier(_ *rand.Rand, x float64) (bool, error) {
	s := newStartRig()
	s.add(100)
	return s.add(100*x) != 100, nil
}

// probeStartConfirm: an estimate of 100 s, then one pod at 5x it, read on n
// cycles: never taken, however many (learner PR 5b,
// SwitchStartConfirmsByDistinctPod). Before 5b the count was per refused
// sample, and one pod repeating passed on its fourth cycle.
func probeStartConfirm(_ *rand.Rand, n float64) (bool, error) {
	s := newStartRig()
	s.add(100)
	est := s.add(500)
	for range int(n) - 1 {
		s.a.noteReplicaStartFor(startNote{key: s.key, namespace: "test-ns", variant: "v"}, s.pods, logr.Discard())
		est = s.a.startSeconds[s.key]
	}
	return est != 100, nil
}

// probeStartConfirmPods: an estimate of 100 s, then n distinct pods at 5x
// it, one more each cycle, the earlier ones still reporting: refused until
// the startConfirmPods-th (3), which is taken.
func probeStartConfirmPods(_ *rand.Rand, n float64) (bool, error) {
	s := newStartRig()
	s.add(100)
	est := 100.0
	for range int(n) {
		est = s.add(500)
	}
	return est != 100, nil
}

// checkStartAlpha: the second sample moves the estimate by 0.3 of the way.
func checkStartAlpha(seed uint64) error {
	r := propRand(seed)
	first, second := 40+100*r.Float64(), 40+100*r.Float64()
	s := newStartRig()
	s.add(first)
	got, want := s.add(second), first+0.3*(second-first)
	return holds(relErr(got, want) <= 1e-9, kindWrongSide, "estimate %.3f after %.1f then %.1f, want %.3f", got, first, second, want)
}

// probeStartZero: a pod reporting a start of s seconds: only a positive one
// is a sample.
func probeStartZero(_ *rand.Rand, s float64) (bool, error) {
	st := newStartRig()
	st.add(100)
	return st.add(s) != 100, nil
}

// probePricingK: a measured reading at half the derived mu priced at the
// pricing k center's scaleUpThreshold gives (kvCacheThreshold x
// scaleUpThreshold, pricingK); with scaleUpThreshold at x, the derived mu
// moves with the k it is priced at, so the reading is admitted at and under
// center and refused over it (minMeasuredOfDerived). The cache holds more
// sequences than the cap at every k here, so the rate rises with k.
func probePricingK(center float64) probe {
	derived := func(scaleUp float64) float64 {
		cfg := &config.ScalingPolicy{KvCacheThreshold: config.DefaultKvCacheThreshold, ScaleUpThreshold: scaleUp}
		return deriveMu(tracedModel, &capacity.EngineParams{MaxNumSeqs: 4096}, 1, tracedKv,
			shape.New(1000, 500, 0), pricingK(cfg), 500).Rate
	}
	return func(_ *rand.Rand, x float64) (bool, error) {
		d := derived(x)
		if !(d > 0) {
			return false, errors.New("no derived mu")
		}
		measured := 0.5 * derived(center)
		t := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
		return t.throughputRefusal(1, measured, whatif.DerivedMu{Rate: d, OK: true}, 0, false) == "", nil
	}
}

// --- thresholds with no edge of their own until #495 ---

// probeSteadyRows: n replicas spread over 0.40 of k on one cycle, every
// reading steady: the steady window holds all of them up to its 40
// (itl.DefaultSteadyWindowMaxSize, twice the main window's 20). One cycle,
// so the carry past the main window plays no part.
func probeSteadyRows(_ *rand.Rand, n float64) (bool, error) {
	ks := make([]float64, int(n))
	for i := range ks {
		ks[i] = 0.25 + 0.40*float64(i)/float64(len(ks)-1)
	}
	r := observeOnce(tuningDefault, fleetAt(ks))
	if r.Unsteady != 0 || r.Offered != int(n) {
		return false, fmt.Errorf("precondition: %d readings offered, %d unsteady, want %d and 0", r.Offered, r.Unsteady, int(n))
	}
	return r.SteadyHeld == int(n), nil
}

// probeK2Eviction: a k2 window last used at T, then the sweep
// (EvictStaleHistory) at T + retention + m minutes: kept up to the retention
// and dropped past it: 7 d by default (capacity.HistoryRetention), or the
// retention set when set is true.
func probeK2Eviction(retention time.Duration, set bool) probe {
	return func(r *rand.Rand, m float64) (bool, error) {
		rig := newPropRig()
		if set {
			h := capacity.DefaultHorizons()
			h.BucketRetention = retention
			if _, replaced := rig.a.WithHorizons(h); len(replaced) > 0 {
				return false, fmt.Errorf("precondition: retention %s replaced (%v)", retention, replaced)
			}
		}
		tok := int64(float64(propKv) * (0.88 + 0.05*r.Float64()))
		// The second reading writes both (k2_confirm.go); the clock stays at
		// its cycle, the window's last use.
		if err := rig.runK2(k2Reading{tok, 10, 6, propCycle}, k2Reading{tok, 10, 6, 0}); err != nil {
			return false, err
		}
		used := rig.clock
		window := func() (*capacity.RollingAverage, error) {
			rig.a.mu.Lock()
			defer rig.a.mu.Unlock()
			if len(rig.a.computeCapacityHistory) > 1 {
				return nil, fmt.Errorf("%d k2 windows, want one", len(rig.a.computeCapacityHistory))
			}
			for _, ra := range rig.a.computeCapacityHistory {
				return ra, nil
			}
			return nil, nil
		}
		ra, err := window()
		if err != nil || ra == nil {
			return false, fmt.Errorf("precondition: no k2 window (%v)", err)
		}
		if ra.StaleAt(used, 0) || !ra.StaleAt(used.Add(time.Nanosecond), 0) {
			return false, errors.New("precondition: the window was not last used on the last cycle")
		}
		rig.clock = used.Add(retention + time.Duration(m*float64(time.Minute)))
		rig.a.EvictStaleHistory()
		ra, err = window()
		return ra != nil, err
	}
}

// probeStandingQueue: replicas at mu 1, 2 and 3, each with its queue d
// requests over what it drains in horizon seconds, the drain horizon the
// floor takes at start: its whole queue is backlog up to the cap, and the cap
// past it (localQueueBacklog). Under BacklogDrainSeconds the 60 s drain time
// binds; over it the start does (DrainHorizon). horizon is literal, so a
// threshold moved by a second fails. These hold the two functions, not
// the floor's wiring: which start applyThroughputFloor passes them is not
// tested here, and dropping the start from its DrainHorizon call passes
// both cases. Held at the functions, not through
// Analyze: on the drain-bound case's fleet (checkFloorDrainBound) another
// bound holds the order under the cap's, flat at 4.12 replicas for a queue
// of 69 to 300 at lambda 2.5, so the order does not show which side of the
// cap a queue is on.
func probeStandingQueue(start, horizon float64) probe {
	return func(_ *rand.Rand, d float64) (bool, error) {
		h := floor.DrainHorizon(floor.BacklogDrainSeconds, start)
		whole := true
		for _, mu := range []float64{1, 2, 3} {
			q := mu*horizon + d
			got := localQueueBacklog(capacity.ReplicaCapacity{QueueLength: int(q), SaturatedThroughput: mu}, h)
			whole = whole && got == q
		}
		return whole, nil
	}
}
