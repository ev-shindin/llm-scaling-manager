package saturation

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Property cases for measured mu (inventory row 5): its write path and fold
// across pods, its admission, and the measured/derived choice.

func muCases() []propCase {
	real := func() muLearner { return newAnalyzerMu() }
	return []propCase{
		{class: classTransient, learner: "mu", name: "filling-token-rate", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuFilling(tuningDefault)},
		{class: classFirstSample, learner: "mu", name: "low-first-reading", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuFirstSample(tuningDefault)},
		{class: classMissing, learner: "mu", name: "missing-and-duplicate", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuMissingDuplicate(real)},
		{class: classHeterogeneity, learner: "mu", name: "one-slow-pod-n4", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuOneSlowPod(real, 4)},
		{class: classHeterogeneity, learner: "mu", name: "one-slow-pod-n16", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuOneSlowPod(real, 16)},
		{class: classHeterogeneity, learner: "mu", name: "one-pod-doubled", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuDoubled(real)},
		{class: classHeterogeneity, learner: "mu", name: "sibling-yield-slow-pod", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuSiblingYield},
		{class: classHeterogeneity, learner: "mu", name: "half-served-max-slow-pod", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuHalfServed},
		{class: classNDependence, learner: "mu", name: "iid-fold", family: familyDirect, tuning: tunedDefault, seeds: 10, check: checkMuNDependence(real, false)},
		{class: classNDependence, learner: "mu", name: "iid-fold-timebase", family: familyTimeBase, tuning: tunedDefault, seeds: 10, check: checkMuNDependence(real, true)},
		{class: classKnownTruth, learner: "mu", name: "choice-priced", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuChoiceTruth(realMuChooser)},
		{class: classLiveness, learner: "mu", name: "n2-aggregated", family: familyDirect, tuning: tunedDefault, check: checkMuLiveness(nil)},
		{class: classEdges, learner: "mu", name: "latch-at-2x", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuLatch(tuningDefault)},
	}
}

// muFigure runs readings through l: each inner slice is one minute's reads,
// pod p's rate at index p (a NaN skips the pod), and returns the figure.
func muFigure(l muLearner, minutes [][]float64) float64 {
	for _, m := range minutes {
		for p, v := range m {
			if !math.IsNaN(v) {
				l.read(fmt.Sprintf("p%d", p), v)
			}
		}
		l.next()
	}
	return l.figure()
}

// checkMuNDependence: N pods with iid noise around a true mu of 1; the
// figure's expectation is within propNDepBias of the truth at every N in
// {1, 2, 4, 16} (catches the max across replicas, +13.6 % at 16). On the time
// base each pod's rate is a rate[1m] of per-second Poisson completions.
func checkMuNDependence(newL func() muLearner, timeBased bool) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		const trials, minutes = 40, 10
		for _, n := range []int{1, 2, 4, 16} {
			sum := 0.0
			for range trials {
				reads := make([][]float64, minutes)
				for m := range reads {
					reads[m] = make([]float64, n)
					for p := range n {
						reads[m][p] = muDraw(r, timeBased)
					}
				}
				sum += muFigure(newL(), reads)
			}
			mean := sum / trials
			if !(mean > 0) {
				return violation(kindNotLearned, "N=%d: no mu", n)
			}
			if e := mean - 1; !(math.Abs(e) <= propNDepBias) {
				return violation(kindNDependent, "E[mu] at N=%d is %.4f against a true 1: %+.1f%%", n, mean, 100*e)
			}
		}
		return nil
	}
}

func muDraw(r *rand.Rand, timeBased bool) float64 {
	if !timeBased {
		return 1 + 0.1*r.NormFloat64()
	}
	c := 0.0
	for range 60 {
		c += poisson(r, 1)
	}
	return c / 60
}

func poisson(r *rand.Rand, lambda float64) float64 {
	l, k, p := math.Exp(-lambda), 0.0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// slowFleet is three minutes of n pods at level, the last at level x f.
func slowFleet(n int, level, f float64) [][]float64 {
	out := make([][]float64, 3)
	for m := range out {
		out[m] = make([]float64, n)
		for p := range n {
			out[m][p] = level
		}
		out[m][n-1] = level * f
	}
	return out
}

// checkMuOneSlowPod: one pod genuinely at half the others' rate, N in
// {4, 16}. Mu's fold is the mean, the declared estimand, so the slow pod
// moves mu by its weight only: a passing guard (2.6), not a ledger row.
func checkMuOneSlowPod(newL func() muLearner, n int) func(uint64) error {
	return func(seed uint64) error {
		level := 0.5 + propRand(seed).Float64()
		got := muFigure(newL(), slowFleet(n, level, 0.5))
		want := level * (float64(n) - 0.5) / float64(n)
		return holds(relErr(got, want) <= 1e-9, kindBiased, "N=%d, one pod at half: mu %.6f against the mean %.6f", n, got, want)
	}
}

// checkMuDoubled: one pod's rate reaches the analyzer at 2x (a series doubled
// inside PromQL): mu moves by that pod's weight, no more.
func checkMuDoubled(newL func() muLearner) func(uint64) error {
	return func(seed uint64) error {
		level := 0.5 + propRand(seed).Float64()
		const n = 4
		got := muFigure(newL(), slowFleet(n, level, 2))
		want := level * (n + 1) / n
		return holds(relErr(got, want) <= 1e-9, kindBiased, "one pod at 2x among %d: mu %.6f against %.6f", n, got, want)
	}
}

// checkMuMissingDuplicate: a pod whose rate is missing (read as 0) for two
// minutes, and a pod listed twice in a cycle, leave mu where four clean pods
// put it.
func checkMuMissingDuplicate(newL func() muLearner) func(uint64) error {
	return func(seed uint64) error {
		level := 0.5 + propRand(seed).Float64()
		clean, dirty := newL(), newL()
		for m := range 3 {
			for p := range 4 {
				clean.read(fmt.Sprintf("p%d", p), level)
				switch {
				case p == 3 && m >= 1:
					dirty.read("p3", 0) // a missing gauge, as the collector zeroes it
				case p == 0:
					dirty.read("p0", level)
					dirty.read("p0", level) // two rows for one pod
				default:
					dirty.read(fmt.Sprintf("p%d", p), level)
				}
			}
			clean.next()
			dirty.next()
		}
		c, d := clean.figure(), dirty.figure()
		if !(c > 0) {
			return violation(kindNotLearned, "no mu from four clean pods")
		}
		return holds(relErr(d, c) <= 1e-9, kindMoved, "mu %.6f with a missing and a duplicated row, %.6f without", d, c)
	}
}

// checkMuFirstSample: E1's first sample, a fresh pod at under half its true
// rate with the line's derived mu at the truth. Withheld, it passes; admitted,
// a second identifying reading would move it by more than the band.
func checkMuFirstSample(tn config.AnalyzerTuning) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		truth := 0.2 * (1 + 0.1*r.Float64())
		first := truth * (0.40 + 0.05*r.Float64()) // E1: 0.0995 against about 0.2
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		if t.throughputRefusal(first, first, derivedMu{rate: truth, ok: true}, 0, false) != "" {
			return nil
		}
		moved := relErr((first+truth)/2, first)
		return holds(moved <= propBand, kindAdoptedRaw, "a first reading of %.4f against a derived %.4f was admitted; the second moves it %.0f%%", first, truth, 100*moved)
	}
}

// checkMuFilling: a replica generating more tokens than its completions
// account for -- a batch filling -- is not admitted as mu.
func checkMuFilling(tn config.AnalyzerTuning) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		rm := satRow("p0", 300_000, 10, 2+r.Float64())
		rm.GenerationTokenRate = rm.RequestRate * 500 * (1.3 + 0.5*r.Float64())
		return holds(t.tokensOutrunCompletions(rm, domain.RoleDecode, 500), kindTransient,
			"a filling replica (%.0f tokens/s against %.2f completions x 500) was admitted", rm.GenerationTokenRate, rm.RequestRate)
	}
}

// muChooser picks the mu a variant prices at from the measured and derived
// figures; real is measuredYieldsToDerived.
type muChooser func(measured, derived float64, sampledAt, now time.Time) float64

func realMuChooser(measured, derived float64, sampledAt, now time.Time) float64 {
	a := NewSaturationAnalyzer(capacity.NewStore())
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	if a.measuredYieldsToDerived(propMuKey, throughputReading{rate: measured, sampledAt: sampledAt},
		derivedMu{rate: derived, ok: true}, 0, t, now) {
		return derived
	}
	return measured
}

// checkMuChoiceTruth: a fresh measurement at the truth beside a derived
// figure up to 1.9x high: the rules together price at most truth x
// (1 + epsilon).
func checkMuChoiceTruth(choose muChooser) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		for range 20 {
			truth := 0.5 + r.Float64()
			derived := truth * (1 + 0.9*r.Float64())
			got := choose(truth, derived, propStart.Add(-30*time.Second), propStart)
			if !(got <= truth*(1+propTruthTol)) {
				return violation(kindOverPriced, "measured %.3f (fresh) and derived %.3f priced %.3f", truth, derived, got)
			}
		}
		return nil
	}
}

// checkMuSiblingYield: a fleet with one genuinely slow pod, its reading the
// one on record. The sibling yield prices the siblings' lower median, over
// the fleet's mean: the risky side. Declared: it over-prices once the slow
// pod reads under siblingMuTolerance (0.8) of its siblings.
func checkMuSiblingYield(seed uint64) error {
	r := propRand(seed)
	a := NewSaturationAnalyzer(capacity.NewStore())
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	slow := 0.5 + 0.25*r.Float64()
	priced := slow
	if s, yielded := a.siblingYield(propMuKey, throughputReading{rate: slow}, servedRates{max: 1, saturated: 1, unqueued: true}, t); yielded {
		priced = s
	}
	mean := (3 + slow) / 4
	return holds(priced <= mean*(1+propTruthTol), kindOverPriced, "fleet {1, 1, 1, %.3f} (mean %.3f): the sibling yield priced %.3f, %.0f%% over", slow, mean, priced, 100*(priced/mean-1))
}

// checkMuHalfServed: a fleet with one genuinely slow pod under half the
// fastest's rate. Its readings are refused (half the served max), so mu is
// the others' mean, over the fleet's.
func checkMuHalfServed(seed uint64) error {
	r := propRand(seed)
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	slow := 0.3 + 0.15*r.Float64()
	var sum float64
	var n int
	for _, v := range []float64{1, 1, 1, slow} {
		if t.throughputRefusal(v, v, derivedMu{}, 1, false) == "" {
			sum += v
			n++
		}
	}
	if n == 0 {
		return violation(kindNotLearned, "every reading refused")
	}
	mean := (3 + slow) / 4
	priced := sum / float64(n)
	return holds(priced <= mean*(1+propTruthTol), kindOverPriced, "fleet {1, 1, 1, %.3f} (mean %.3f): admitted readings price %.3f, %.0f%% over", slow, mean, priced, 100*(priced/mean-1))
}

// checkMuLatch: measured over derived sits at 2x, the latch's set point, with
// autocorrelated noise, then at 1/2, its low set point, stale past the low
// hold. The noise-free ratio never crosses either, so each side's choice
// changes at most once.
func checkMuLatch(tn config.AnalyzerTuning) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		for _, side := range []float64{2, 0.5} {
			a := NewSaturationAnalyzer(capacity.NewStore())
			noise := arNoise(r, 120, 0.04, 0.8)
			v := make([]bool, len(noise))
			for i, e := range noise {
				now := propStart.Add(time.Duration(i) * propCycle)
				v[i] = a.measuredYieldsToDerived(propMuKey, throughputReading{rate: side * (1 + e)}, derivedMu{rate: 1, ok: true}, 0, t, now)
			}
			if n := reversals(v); n > 1 {
				return violation(kindFlipped, "measured/derived on %.1f with 4%% noise: the choice reversed %d times in %d cycles", side, n, len(v))
			}
		}
		return nil
	}
}

// checkMuLiveness: two aggregated replicas saturated from the start: mu is
// measured within 3 min, within the band of the rate they serve.
func checkMuLiveness(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		rig := newPropRig()
		rig.wrap = wrap
		truth := 4 + r.Float64()
		for range 13 {
			rows := []domain.ReplicaMetrics{
				satRow("r0", 330_000, 10, truth*(1+0.01*r.NormFloat64())),
				satRow("r1", 330_000, 10, truth*(1+0.01*r.NormFloat64())),
			}
			if _, err := rig.analyze(rows, aggStates(2), 2*truth, propCycle); err != nil {
				return err
			}
		}
		got := rig.measuredMu()
		if !(got > 0) {
			return violation(kindNotLearned, "after 3 min saturated at %.3f req/s, no measured mu", truth)
		}
		return holds(relErr(got, truth) <= propBand, kindBiased, "after 3 min saturated at %.3f req/s, measured mu %.3f", truth, got)
	}
}

// measuredMu is the median of the one measured-mu window the rig holds, 0
// when none.
func (r *propRig) measuredMu() float64 {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	var got float64
	for _, ra := range r.a.saturatedThroughput {
		if ra.Len() > 0 {
			got = ra.Median()
		}
	}
	return got
}
