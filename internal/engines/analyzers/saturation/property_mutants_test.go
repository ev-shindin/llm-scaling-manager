package saturation

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// A mutant per mapping row of 2.7: the past defect put back, and the class the
// row maps to run on it. The class must fail on the mutant; a row whose class
// passes its mutant does not count as catching anything, and is listed in
// stillEscaping instead.
//
// A defect is put back the cheapest faithful way: through a policy key set
// where the guard stops working (the guard is the fix), through the rows
// (the defect was in what the learner was handed), or as a toy learner with
// the pre-fix estimator (the fix was the estimator). Production code has no
// mutant seam.

type propMutant struct {
	// row is the finding, as 2.7 lists it.
	row string
	// caseID is the property case the mutant runs (class/learner/name).
	caseID string
	// how says how the defect is put back.
	how   string
	check func(seed uint64) error
}

func tuningWith(edit func(*config.AnalyzerTuning)) config.AnalyzerTuning {
	var t config.AnalyzerTuning
	edit(&t)
	return t
}

// settleOff turns k2's settle tests off: every reading passes the rate,
// residency and rise checks (the joined test still applies).
func settleOff(p *config.ScalingPolicy) {
	p.AnalyzerTuning.K2Settle = config.K2SettleTuning{RateFraction: 1e-9, MaxRise: 1e9}
}

func propertyMutants() []propMutant {
	steadyOff := steadinessOf(tuningWith(func(t *config.AnalyzerTuning) {
		t.SteadyGate.SteadyFor = config.PolicyDuration(time.Nanosecond)
	}))
	neverSteady := steadinessOf(tuningWith(func(t *config.AnalyzerTuning) { t.SteadyGate.MaxKDrift = 1e-12 }))
	return []propMutant{
		{"GPS ratio first raw", "first-sample/mu/low-first-reading",
			"muChoice.minMeasuredOfDerived 1e-9: the low-first-reading guard off",
			checkMuFirstSample(tuningWith(func(t *config.AnalyzerTuning) { t.MuChoice.MinMeasuredOfDerived = 1e-9 }))},
		{"pinned B from unsteady readings", "transient/itl/joiner-warmup",
			"steadyGate.steadyFor 1ns: no reading is stamped settling",
			checkITLJoiner(func() itlLearner { return newTrackerITL(steadyOff) }, 1, 20*time.Minute)},
		{"k2 at 8 % behind a step", "transient/k2/load-step",
			"k2Settle.rateFraction 1e-9, maxRise 1e9: the settle tests off",
			checkK2LoadStep(settleOff, nil)},
		{"Full fit never runs at N >= 2, B stays 6 ms (#273 A3)", "stale-value/itl/b-converges-n2",
			"steadyGate.maxKDrift 1e-12: no reading is ever steady, so the full fit never runs",
			checkITLStaleB(func() itlLearner { return newTrackerITL(neverSteady) }, 2, 10*time.Minute)},
		{"k == 0 unsettles; router queue without engine metrics", "missing-duplicated/mu/missing-and-duplicate",
			"toy fold over every row it is handed: a missing rate read as 0, a duplicated row as a second pod",
			checkMuMissingDuplicate(func() muLearner { return newToyMu(foldList) })},
		{"Sum of per-pod one-minute peaks", "redistribution/demand/alternating-pods",
			"rows with KvUsageInstantKnown cleared: demand sums each pod's one-minute peak",
			checkDemandAlternating(func(rm *domain.ReplicaMetrics) { rm.KvUsageInstantKnown = false })},
		{"Settle refuses one cycle of a fill (#282 F1)", "transient/k2/slow-fill",
			"k2Settle.rateFraction 1e-9, maxRise 1e9: the rise is not compared a window back",
			checkK2Fill(settleOff)},
		{"measured/derived flips near 2x", "edges/mu/latch-at-2x",
			"muChoice.returnRatio = maxRatio = 2: the latch's hysteresis band removed",
			checkMuLatch(tuningWith(func(t *config.AnalyzerTuning) { t.MuChoice.MaxRatio, t.MuChoice.ReturnRatio = 2, 2 }))},
		{"the 0.30 idle line", "edges/idle/idle-line-030",
			"toy verdict: idle on each cycle's own reading, no 2 min run",
			checkIdleEdge(instantIdleVerdict)},
		{"backlogGrew", "edges/floor/backlog-grew",
			"backlogGrowth.minRequests and fraction 1e-9: grown on any rise, as before e7d9f0ff1",
			checkBacklogEdge(tuningWith(func(t *config.AnalyzerTuning) {
				t.BacklogGrowth.MinRequests, t.BacklogGrowth.Fraction = 1e-9, 1e-9
			}))},
		{"Headroom counted twice", "known-truth/floor/steady-order",
			"toy: the floor's order divided by the 0.85 target a second time",
			checkFloorTruth(func(rows []domain.ReplicaMetrics, lambda float64) (float64, error) {
				n, err := floorImplied(rows, lambda)
				return n / 0.85, err
			})},
		{"the rules together always pick the higher mu", "known-truth/mu/choice-priced",
			"toy chooser: the higher of measured and derived",
			checkMuChoiceTruth(func(m, d float64, _, _ time.Time) float64 { return math.Max(m, d) })},
		{"KV and throughput units mixed", "scale/k2/block-size-and-cycle",
			"rows with TokensInUse in blocks, not tokens",
			checkK2Scale(func(rm *domain.ReplicaMetrics) { rm.TokensInUse /= rm.BlockSize })},
		{"Max across replicas (+13.6 % at 16)", "n-dependence/mu/iid-fold",
			"toy fold: the max across pods within a minute",
			checkMuNDependence(func() muLearner { return newToyMu(foldMax) }, false)},
		{"Prefill double scrape", "contamination-heterogeneity/mu/one-pod-doubled",
			"toy fold: the max across pods, as the prefill bound keeps it",
			checkMuDoubled(func() muLearner { return newToyMu(foldMax) })},
		{"local queue priced as drainable", "monotonicity/floor/standing-local-queue",
			"toy: the floor's order plus the pinned queue's part past mu x T, charged as if added replicas drained it",
			checkFloorDrainBound(func(rows []domain.ReplicaMetrics, lambda float64) (float64, error) {
				n, err := floorImplied(rows, lambda)
				return n + (300-60.0)/60/0.85, err
			})},
		{"Weighted median flapping between modes", "edges/itl/bimodal-fit",
			"toy fit: the k-weighted median of the readings' slopes",
			checkITLBimodal(fitWeightedMedian)},
		{"IQM breaks at 25 % contamination", "contamination-heterogeneity/itl/flagged-contamination-to-50pct",
			"toy learner: every reading, flags ignored, interquartile mean of the slopes",
			checkITLFlagged(func() itlLearner { return &toyITL{fit: fitIQM} })},
		{"A genuinely slow replica trimmed away (#273 r3)", "contamination-heterogeneity/itl/slow-replica-fit",
			"toy fit: the interquartile mean of the slopes",
			checkITLSlowReplica(fitIQM)},
		{"A genuinely slow replica trimmed away (#282 r3)", "contamination-heterogeneity/mu/one-slow-pod-n4",
			"toy fold: the lower median across pods, #282's fold before the mean",
			checkMuOneSlowPod(func() muLearner { return newToyMu(foldLowerMedian) }, 4)},
		{"No floor under flow control (#144)", "liveness/mu/n2-aggregated",
			"rows with no completion rate, as the floor read a fleet under flow control",
			func(seed uint64) error {
				return muLiveness(seed, func(rm *domain.ReplicaMetrics) { rm.RequestRate = 0 })
			}},
		{"Imbalance audit (load moved between replicas)", "redistribution/k2/queue-moved",
			"rows all read queued: k2 learned from replicas the load moved off",
			checkK2Redistribution(func(rm *domain.ReplicaMetrics) { rm.QueueLength = max(rm.QueueLength, 10) })},
		{"Settle holes: restart", "transient/k2/restart-same-name",
			"rows with ReadySince cleared: a restart under the same name is not seen",
			checkK2Restart(func(rm *domain.ReplicaMetrics) { rm.ReadySince = time.Time{} })},
		{"Restart fix stubbed", "transient/k2/restart-same-name",
			"rows with ReadySince cleared, as a stubbed restart fix reads them",
			checkK2Restart(func(rm *domain.ReplicaMetrics) { rm.ReadySince = time.Time{} })},
	}
}

// stillEscaping is each 2.7 finding no 0b class catches, and why.
var stillEscaping = map[string]string{
	"Instant k against a rate[1m] ITL (#273 A5)":         "known truth on the time-base family passes on today's code: the steady gate keeps the in-minute bursts the generator makes out of the full fit, and the pinned fit's ratio is unbiased under noise in k. No generator here reproduces A5; the case is kept as a guard",
	"k2 key flips on the output-length boundary":         "today's code fails the class (ledger row edges/k2/key-on-output-boundary): there is no fixed version to put the defect back into",
	"Demand falls when lambda lags a step":               "the monotonicity trajectory passes on today's code and no pre-fix floor is reproduced in a toy; the standing-queue half of the row has its mutant",
	"Settle holes: fall, rate escape, idle return, lend": "only the restart and joiner events have a case; the others need the settle tracker (PR 1)",
	"Router queue without engine metrics":                "missing-duplicated/k2/unknown-gauges passes on today's code; its mutant is the mu fold's (a missing rate read as a value)",
	"'For good' self-sealing low mu":                     "the closed-loop probe lands in PR 3",
}

// TestPropertyMutants runs every mutant and requires its class to fail on it.
// A finding is either caught by a mutant or listed as still escaping, never
// both.
func TestPropertyMutants(t *testing.T) {
	cases := map[string]bool{}
	for _, c := range propertyCases() {
		cases[c.id()] = true
	}
	rows := make([]string, 0, len(stillEscaping))
	for row := range stillEscaping {
		rows = append(rows, row)
	}
	sort.Strings(rows)
	for _, row := range rows {
		t.Logf("still escaping: %s: %s", row, stillEscaping[row])
	}
	for _, m := range propertyMutants() {
		if _, escaping := stillEscaping[m.row]; escaping {
			t.Errorf("%s has a mutant and is listed as still escaping", m.row)
		}
		t.Run(m.row, func(t *testing.T) {
			if !cases[m.caseID] {
				t.Fatalf("mutant names no property case %s", m.caseID)
			}
			err := runProp(m.check)
			if err == nil {
				t.Errorf("%s passes on the mutant (%s): move the row to stillEscaping", m.caseID, m.how)
				return
			}
			t.Logf("caught: %s fails on the mutant (%s): %v", m.caseID, m.how, err)
		})
	}
}

// ----- toy learners with the pre-fix estimators -----

// toyMu is mu's write path with another fold: samples a minute apart, pods
// folded within the minute by fold; the figure is the samples' median, as
// the window's.
type toyMu struct {
	fold    func([]podRate) float64
	reads   []podRate
	samples []float64
}

type podRate struct {
	pod  string
	rate float64
}

func newToyMu(fold func([]podRate) float64) *toyMu { return &toyMu{fold: fold} }

func (m *toyMu) read(pod string, rate float64) { m.reads = append(m.reads, podRate{pod, rate}) }
func (m *toyMu) next() {
	if len(m.reads) > 0 {
		m.samples = append(m.samples, m.fold(m.reads))
	}
	m.reads = nil
}
func (m *toyMu) figure() float64 {
	m.next()
	s := append([]float64(nil), m.samples...)
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func foldList(r []podRate) float64 {
	sum := 0.0
	for _, p := range r {
		sum += p.rate
	}
	return sum / float64(len(r))
}

func foldMax(r []podRate) float64 {
	m := 0.0
	for _, p := range r {
		m = math.Max(m, p.rate)
	}
	return m
}

func foldLowerMedian(r []podRate) float64 {
	v := make([]float64, len(r))
	for i, p := range r {
		v[i] = p.rate
	}
	sort.Float64s(v)
	return v[(len(v)-1)/2]
}

// slopes are the readings' own slopes (ITL - b) / k, sorted, with k weights.
func slopes(obs []itl.Observation, b float64) (s, w []float64) {
	idx := make([]int, len(obs))
	for i := range idx {
		idx[i] = i
	}
	sl := func(i int) float64 { return (obs[i].ITLSec - b) / obs[i].K }
	sort.Slice(idx, func(x, y int) bool { return sl(idx[x]) < sl(idx[y]) })
	for _, i := range idx {
		s, w = append(s, sl(i)), append(w, obs[i].K)
	}
	return s, w
}

func fitWeightedMedian(obs []itl.Observation, b float64) (itl.Model, bool) {
	s, w := slopes(obs, b)
	if len(s) == 0 {
		return itl.Model{}, false
	}
	total := 0.0
	for _, x := range w {
		total += x
	}
	acc := 0.0
	for i := range s {
		acc += w[i]
		if acc >= total/2 {
			return itl.Model{A: s[i], B: b}, true
		}
	}
	return itl.Model{A: s[len(s)-1], B: b}, true
}

func fitIQM(obs []itl.Observation, b float64) (itl.Model, bool) {
	s, w := slopes(obs, b)
	if len(s) < 4 {
		return itl.Model{}, false
	}
	lo, hi := len(s)/4, len(s)-len(s)/4
	var num, den float64
	for i := lo; i < hi; i++ {
		num += s[i] * w[i]
		den += w[i]
	}
	return itl.Model{A: num / den, B: b}, true
}

// toyITL keeps the last 20 readings of every Ready row, flags ignored, and fits
// them with fit at the default intercept.
type toyITL struct {
	fit fitLearner
	obs []itl.Observation
}

func (l *toyITL) observe(rows []domain.ReplicaMetrics, now time.Time) itl.Reading {
	for _, rm := range rows {
		if rm.Ready && rm.KvUsageInstant > 0 && rm.AvgITL > 0 {
			l.obs = append(l.obs, itl.Observation{K: rm.KvUsageInstant, ITLSec: rm.AvgITL, Timestamp: now})
		}
	}
	if len(l.obs) > itl.DefaultWindowMaxSize {
		l.obs = l.obs[len(l.obs)-itl.DefaultWindowMaxSize:]
	}
	if len(l.obs) < itl.DefaultMinSamples {
		return itl.Reading{}
	}
	m, ok := l.fit(l.obs, itl.DefaultBaselineSec)
	if !ok {
		return itl.Reading{}
	}
	return itl.Reading{Model: m, Tier: itl.TierPinnedB}
}

func (l *toyITL) baseline() (float64, bool) { return 0, false }

// instantIdleVerdict is idle room judged on each cycle's reading alone.
func instantIdleVerdict(_ *SaturationAnalyzer, frac float64, _ time.Time) bool {
	return frac < config.DefaultIdleKvUsage
}
