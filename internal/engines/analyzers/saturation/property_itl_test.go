package saturation

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// Property cases for the ITL line (inventory row 1): itl.Tracker, which the
// saturation and SLO analyzers both drive, and its pinned fit.

func itlCases() []propCase {
	sDef, sNon := steadinessOf(tuningDefault), steadinessOf(tuningNonDefault)
	def := func() itlLearner { return newTrackerITL(sDef) }
	non := func() itlLearner { return newTrackerITL(sNon) }
	return []propCase{
		{class: classTransient, learner: "itl", name: "joiner-warmup", family: familyDirect, tuning: tunedDefault, check: checkITLJoiner(def, 1, 20*time.Minute)},
		{class: classTransient, learner: "itl", name: "joiner-warmup-nondefault", family: familyDirect, tuning: tunedNonDefault,
			policy: tuningPolicy(tuningNonDefault), check: checkITLJoiner(non, 1, 30*time.Minute)},
		{class: classFirstSample, learner: "itl", name: "one-reading", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkITLOneReading(def)},
		{class: classStale, learner: "itl", name: "b-converges-n2", family: familyDirect, tuning: tunedDefault, check: checkITLStaleB(def, 2, 10*time.Minute)},
		{class: classStale, learner: "itl", name: "b-converges-n4", family: familyDirect, tuning: tunedDefault, check: checkITLStaleB(def, 4, 10*time.Minute)},
		{class: classStale, learner: "itl", name: "b-converges-n2-nondefault", family: familyDirect, tuning: tunedNonDefault,
			policy: tuningPolicy(tuningNonDefault), check: checkITLStaleB(non, 2, 20*time.Minute)},
		{class: classStale, learner: "itl", name: "b-converges-n2-timebase", family: familyTimeBase, tuning: tunedDefault, check: checkITLStaleBTimeBase(def)},
		{class: classMissing, learner: "itl", name: "missed-row", family: familyDirect, tuning: tunedDefault, check: checkITLMissedRow(def)},
		{class: classMissing, learner: "itl", name: "missed-row-nondefault", family: familyDirect, tuning: tunedNonDefault,
			policy: tuningPolicy(tuningNonDefault), check: checkITLMissedRow(non)},
		{class: classHeterogeneity, learner: "itl", name: "slow-replica-fit", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkITLSlowReplica(itl.FitPinnedB)},
		{class: classHeterogeneity, learner: "itl", name: "flagged-contamination-to-50pct", family: familyDirect, tuning: tunedDefault, check: checkITLFlagged(def)},
		{class: classKnownTruth, learner: "itl", name: "line-at-kprice", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkITLKnownTruth(def, false)},
		{class: classKnownTruth, learner: "itl", name: "line-at-kprice-timebase", family: familyTimeBase, tuning: tunedDefault, seeds: cheapSeeds, check: checkITLKnownTruth(def, true)},
		{class: classEdges, learner: "itl", name: "bimodal-fit", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkITLBimodal(itl.FitPinnedB)},
	}
}

// itlRun runs l over cycles of rows produced by gen, from propStart, and
// returns each cycle's reading.
func itlRun(l itlLearner, cycles int, gen func(i int) []domain.ReplicaMetrics) []itl.Reading {
	out := make([]itl.Reading, cycles)
	for i := range cycles {
		now := propStart.Add(time.Duration(i) * propCycle)
		out[i] = l.observe(gen(i), now)
	}
	return out
}

// itlAt is the line's ITL at propKPrice, NaN when there is no line.
func itlAt(r itl.Reading) float64 {
	if r.Model.IsZero() {
		return math.NaN()
	}
	return r.Model.ITLAt(propKPrice)
}

// baseLevels is n replicas' KV levels spread over [0.25, 0.70], so a steady
// window can reach the full fit's spread.
func baseLevels(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 0.25
		if n > 1 {
			out[i] += 0.45 * float64(i) / float64(n-1)
		}
	}
	return out
}

// steadyFleet is n replicas at baseLevels with AR noise on k and iid noise on
// ITL, from one seed.
func steadyFleet(r *rand.Rand, n, cycles int) func(i int) []domain.ReplicaMetrics {
	lv := baseLevels(n)
	kNoise := make([][]float64, n)
	iNoise := make([][]float64, n)
	for p := range n {
		kNoise[p] = arNoise(r, cycles, 0.01, 0.9)
		iNoise[p] = arNoise(r, cycles, 0.005, 0)
	}
	return func(i int) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, n)
		for p := range n {
			k := lv[p] * (1 + kNoise[p][i])
			rows[p] = itlRow(fmt.Sprintf("r%d", p), k, propITL(k)*(1+iNoise[p][i]))
		}
		return rows
	}
}

// checkITLJoiner: joiners read far above the line for their first 90 s.
// Against the same run without them, the line priced at kPrice moves by at
// most propTransientTol.
func checkITLJoiner(newL func() itlLearner, joiners int, dur time.Duration) func(uint64) error {
	return func(seed uint64) error {
		cycles := int(dur / propCycle)
		base := steadyFleet(propRand(seed), 3, cycles)
		joinAt := cycles / 3
		control := itlRun(newL(), cycles, base)
		joined := itlRun(newL(), cycles, func(i int) []domain.ReplicaMetrics {
			rows := base(i)
			if i < joinAt {
				return rows
			}
			for j := range joiners {
				lat := propITL(0.5)
				if i-joinAt < 6 { // 90 s of warm-up
					lat *= 5
				}
				rows = append(rows, itlRow(fmt.Sprintf("j%d", j), 0.5, lat))
			}
			return rows
		})
		compared := 0
		for i := joinAt; i < cycles; i++ {
			c, j := itlAt(control[i]), itlAt(joined[i])
			if math.IsNaN(c) || math.IsNaN(j) {
				continue
			}
			compared++
			if e := relErr(j, c); e > propTransientTol {
				return violation(kindTransient, "cycle %d (%s after the join): ITL(%.2f) %.5f with %d joiners against %.5f without, %.1f%% apart",
					i, time.Duration(i-joinAt)*propCycle, propKPrice, j, joiners, c, 100*e)
			}
		}
		return holds(compared > 0, kindNotLearned, "no cycle after the join had a line in both runs")
	}
}

// checkITLOneReading: one reading from one replica sets no line.
func checkITLOneReading(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		k := 0.3 + 0.4*r.Float64()
		got := newL().observe([]domain.ReplicaMetrics{itlRow("r0", k, propITL(k)*(1+0.2*r.NormFloat64()))}, propStart)
		return holds(got.Model.IsZero(), kindAdoptedRaw, "one reading set the line A=%.4f B=%.4f", got.Model.A, got.Model.B)
	}
}

// checkITLStaleB: from identifying data at N >= 2 -- replicas held at
// distinct levels -- B converges to the true intercept (catches #273 A3, B
// stuck at the 6 ms default).
func checkITLStaleB(newL func() itlLearner, n int, dur time.Duration) func(uint64) error {
	return func(seed uint64) error {
		cycles := int(dur / propCycle)
		l := newL()
		itlRun(l, cycles, steadyFleet(propRand(seed), n, cycles))
		b, have := l.baseline()
		if !have {
			return violation(kindStale, "N=%d: no intercept learned in %s; B stays at the %.0f ms default", n, dur, 1000*itl.DefaultBaselineSec)
		}
		return holds(relErr(b, propTrueB) <= 0.10, kindStale, "N=%d: B %.4f s against a true %.4f s after %s", n, b, propTrueB, dur)
	}
}

// checkITLStaleBTimeBase is checkITLStaleB with k instant and ITL a rate[1m]
// of a per-second path.
func checkITLStaleBTimeBase(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		const n, secs = 2, 12 * 60
		lv := baseLevels(n)
		ks := make([]timeBase, n)
		lats := make([]timeBase, n)
		for p := range n {
			k := make([]float64, secs)
			lat := make([]float64, secs)
			noise := arNoise(r, secs, 0.02, 0.98)
			for s := range k {
				k[s] = lv[p] * (1 + noise[s])
				lat[s] = propITL(k[s])
			}
			ks[p], lats[p] = sampleTimeBase(k), sampleTimeBase(lat)
		}
		l := newL()
		itlRun(l, len(ks[0].instant), func(i int) []domain.ReplicaMetrics {
			rows := make([]domain.ReplicaMetrics, n)
			for p := range n {
				rows[p] = itlRow(fmt.Sprintf("r%d", p), ks[p].instant[i], lats[p].rate1m[i])
			}
			return rows
		})
		b, have := l.baseline()
		if !have {
			return violation(kindStale, "no intercept learned on the time base; B stays at the default")
		}
		return holds(relErr(b, propTrueB) <= 0.10, kindStale, "B %.4f s against a true %.4f s", b, propTrueB)
	}
}

// checkITLMissedRow: one replica's row missing for a cycle, and another's
// read as a failed scrape (k and ITL unknown), leave the priced line where
// the same run without them puts it.
func checkITLMissedRow(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		const cycles = 120
		fleet := steadyFleet(propRand(seed), 3, cycles)
		control := itlRun(newL(), cycles, fleet)
		missed := itlRun(newL(), cycles, func(i int) []domain.ReplicaMetrics {
			rows := fleet(i)
			switch i {
			case 60:
				rows = append(rows[:1], rows[2:]...)
			case 70:
				rows[2] = domain.ReplicaMetrics{PodName: "r2", VariantName: "v", Ready: true}
			}
			return rows
		})
		if math.IsNaN(itlAt(control[59])) {
			return violation(kindNotLearned, "no line before the missed rows")
		}
		for i := 60; i < cycles; i++ {
			c, m := itlAt(control[i]), itlAt(missed[i])
			if math.IsNaN(c) && math.IsNaN(m) {
				continue
			}
			if relErr(m, c) > 0.02 {
				return violation(kindMoved, "cycle %d: ITL(%.2f) %.5f after the missed rows against %.5f", i, propKPrice, m, c)
			}
		}
		return nil
	}
}

// checkITLSlowReplica: four replicas on slope 0.08 and one genuinely slower at
// 0.12, none flagged. The pinned fit moves A by the slow replica's k-weight.
func checkITLSlowReplica(fit fitLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		obs := make([]itl.Observation, 0, 20)
		var num, den float64
		for p := range 5 {
			a := 0.08
			if p == 4 {
				a = 0.12
			}
			for range 4 {
				k := 0.5 * (1 + 0.05*r.NormFloat64())
				lat := a*k + propTrueB
				obs = append(obs, itl.Observation{K: k, ITLSec: lat, Timestamp: propStart})
				num += lat - propTrueB
				den += k
			}
		}
		m, fitted := fit(obs, propTrueB)
		if !fitted {
			return violation(kindNotLearned, "no fit")
		}
		want := num / den
		return holds(relErr(m.A, want) <= 0.01, kindBiased, "A %.4f against the k-weighted %.4f: the slow replica was not weighed in", m.A, want)
	}
}

// checkITLFlagged: joiners whose warm-up the tracker flags (settling), swept
// up to half the fleet's readings, are dropped: the line stays where the same
// run without them puts it.
func checkITLFlagged(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		for _, j := range []int{1, 2, 3} {
			if err := checkITLJoiner(newL, j, 20*time.Minute)(seed); err != nil {
				return fmt.Errorf("%d joiners beside 3 (%.0f%% of the readings while they warm): %w", j, 100*float64(j)/float64(j+3), err)
			}
		}
		return nil
	}
}

// checkITLKnownTruth: from a fleet at distinct steady levels, the line priced
// at kPrice is no lower than the true ITL there by more than propTruthTol
// (too low is the side that over-prices derived mu). On the time base k is
// the instant reading and ITL a rate[1m] (#273 A5), with a batch that swings
// by a quarter inside the minute.
func checkITLKnownTruth(newL func() itlLearner, timeBased bool) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		l := newL()
		var readings []itl.Reading
		if !timeBased {
			readings = itlRun(l, 60, steadyFleet(r, 3, 60))
		} else {
			const n, secs = 3, 15 * 60
			lv := baseLevels(n)
			k := make([]timeBase, n)
			lat := make([]timeBase, n)
			for p := range n {
				ks := make([]float64, secs)
				ls := make([]float64, secs)
				for s := range ks {
					ks[s] = math.Max(0.01, lv[p]*(1+0.25*r.NormFloat64()))
					ls[s] = propITL(ks[s])
				}
				k[p], lat[p] = sampleTimeBase(ks), sampleTimeBase(ls)
			}
			readings = itlRun(l, len(k[0].instant), func(i int) []domain.ReplicaMetrics {
				rows := make([]domain.ReplicaMetrics, n)
				for p := range n {
					rows[p] = itlRow(fmt.Sprintf("r%d", p), k[p].instant[i], lat[p].rate1m[i])
				}
				return rows
			})
		}
		last := readings[len(readings)-1]
		got, want := itlAt(last), propITL(propKPrice)
		if math.IsNaN(got) {
			return violation(kindNotLearned, "no line")
		}
		return holds(got >= want*(1-propTruthTol), kindBiased, "ITL(%.2f) priced %.5f s against a true %.5f s (%s tier, A %.4f B %.4f): %.1f%% low",
			propKPrice, got, want, last.Tier, last.Model.A, last.Model.B, 100*(1-got/want))
	}
}

// checkITLBimodal: two groups of replicas on slopes 0.06 and 0.10; the second
// group's share of the load sweeps from 0.3 to 0.7, crossing a half once. The
// fit is Lipschitz in the share: no step moves A by more than twice the
// mixture's own change.
func checkITLBimodal(fit fitLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		const a1, a2, steps = 0.06, 0.10, 40
		ks := make([]float64, 10)
		for i := range ks {
			ks[i] = 0.5 * (1 + 0.02*r.NormFloat64())
		}
		prev := math.NaN()
		for s := 0; s <= steps; s++ {
			w := 0.3 + 0.4*float64(s)/steps
			obs := make([]itl.Observation, 0, len(ks))
			for i, k := range ks {
				kk, a := k*2*(1-w), a1
				if i >= 5 {
					kk, a = k*2*w, a2
				}
				obs = append(obs, itl.Observation{K: kk, ITLSec: a*kk + propTrueB, Timestamp: propStart})
			}
			m, fitted := fit(obs, propTrueB)
			if !fitted {
				return violation(kindNotLearned, "no fit at share %.2f", w)
			}
			if !math.IsNaN(prev) {
				if step := math.Abs(m.A - prev); !(step <= 2*(a2-a1)*0.4/steps+1e-9) {
					return violation(kindFlipped, "share %.2f: A jumped %.4f in one step of %.3f (a mixture moves %.4f)", w, step, 0.4/steps, (a2-a1)*0.4/steps)
				}
			}
			prev = m.A
		}
		return nil
	}
}
