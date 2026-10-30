package saturation

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// Declared estimands and tolerances (2.6). A class has no target without its
// learner's estimand:
//   - ITL line: the k-weighted ratio (ITL - B) / k, priced as ITL at kPrice;
//   - k2: per replica, the KV a saturated replica holds;
//   - measured mu: the arithmetic mean of the pods' mu_i (fleet capacity is
//     their sum);
//   - the floor: lambda / (mu x target) replicas at steady state;
//   - start time: the mean start of the variant's pods.
const (
	// propBand is the first-sample band: how far a second identifying
	// reading may move a figure.
	propBand = 0.05
	// propTransientTol is how far a refused or stamped transient may move a
	// figure against the same run without it.
	propTransientTol = 0.03
	// propTruthTol is known truth's epsilon: priced capacity at most truth x
	// (1 + epsilon).
	propTruthTol = 0.05
	// propNDepBias is the declared bias of an estimate's expectation across
	// N in {1, 2, 4, 16}.
	propNDepBias = 0.03
	// propKPrice is where the ITL line is priced (itl.DefaultKSat).
	propKPrice = itl.DefaultKSat
)

func ok(cond bool, format string, args ...any) error {
	if cond {
		return nil
	}
	return fmt.Errorf(format, args...)
}

// propertyCases is every property case, real learners only. Mutants reuse the
// check functions below with a learner that carries a past defect.
func propertyCases() []propCase {
	def, nonDef := "default", "steadyFor 10m, carry 15m, idleRows 5"
	sDef, sNon := steadinessOf(tuningDefault), steadinessOf(tuningNonDefault)
	itlDef := func() itlLearner { return newTrackerITL(sDef) }
	itlNon := func() itlLearner { return newTrackerITL(sNon) }
	muReal := func() muLearner { return newAnalyzerMu() }
	return []propCase{
		// transient
		{classTransient, "itl", "joiner-warmup", familyDirect, def, checkITLJoiner(itlDef, 1, 20*time.Minute)},
		{classTransient, "itl", "joiner-warmup-nondefault", familyDirect, nonDef, checkITLJoiner(itlNon, 1, 30*time.Minute)},
		{classTransient, "k2", "load-step", familyDirect, def, checkK2LoadStep(nil, nil)},
		{classTransient, "k2", "restart-same-name", familyDirect, def, checkK2Restart(nil)},
		{classTransient, "k2", "slow-fill", familyTimeBase, def, checkK2Fill(nil)},
		{classTransient, "mu", "filling-token-rate", familyDirect, def, checkMuFilling(tuningDefault)},
		// first sample
		{classFirstSample, "k2", "second-reading", familyDirect, def, checkK2FirstSample},
		{classFirstSample, "start-time", "second-pod", familyDirect, def, checkStartFirstSample},
		{classFirstSample, "mu", "low-first-reading", familyDirect, def, checkMuFirstSample(tuningDefault)},
		{classFirstSample, "itl", "one-reading", familyDirect, def, checkITLOneReading(itlDef)},
		// stale value
		{classStale, "itl", "b-converges-n2", familyDirect, def, checkITLStaleB(itlDef, 2, 10*time.Minute)},
		{classStale, "itl", "b-converges-n4", familyDirect, def, checkITLStaleB(itlDef, 4, 10*time.Minute)},
		{classStale, "itl", "b-converges-n2-nondefault", familyDirect, nonDef, checkITLStaleB(itlNon, 2, 20*time.Minute)},
		{classStale, "itl", "b-converges-n2-timebase", familyTimeBase, def, checkITLStaleBTimeBase(itlDef)},
		// missing and duplicated values
		{classMissing, "itl", "missed-row", familyDirect, def, checkITLMissedRow(itlDef)},
		{classMissing, "itl", "missed-row-nondefault", familyDirect, nonDef, checkITLMissedRow(itlNon)},
		{classMissing, "mu", "missing-and-duplicate", familyDirect, def, checkMuMissingDuplicate(muReal)},
		{classMissing, "k2", "unknown-gauges", familyDirect, def, checkK2UnknownGauges},
		// contamination paired with heterogeneity
		{classHeterogeneity, "mu", "one-slow-pod-n4", familyDirect, def, checkMuOneSlowPod(muReal, 4)},
		{classHeterogeneity, "mu", "one-slow-pod-n16", familyDirect, def, checkMuOneSlowPod(muReal, 16)},
		{classHeterogeneity, "mu", "one-pod-doubled", familyDirect, def, checkMuDoubled(muReal)},
		{classHeterogeneity, "itl", "slow-replica-fit", familyDirect, def, checkITLSlowReplica(itl.FitPinnedB)},
		{classHeterogeneity, "itl", "flagged-contamination-to-50pct", familyDirect, def, checkITLFlagged(itlDef)},
		{classHeterogeneity, "mu", "sibling-yield-slow-pod", familyDirect, def, checkMuSiblingYield},
		{classHeterogeneity, "mu", "half-served-max-slow-pod", familyDirect, def, checkMuHalfServed},
		// redistribution invariance
		{classRedistribution, "k2", "queue-moved", familyDirect, def, checkK2Redistribution(nil)},
		{classRedistribution, "demand", "alternating-pods", familyTimeBase, def, checkDemandAlternating(nil)},
		// N-dependence
		{classNDependence, "mu", "iid-fold", familyDirect, def, checkMuNDependence(muReal, false)},
		{classNDependence, "mu", "iid-fold-timebase", familyTimeBase, def, checkMuNDependence(muReal, true)},
		{classNDependence, "prefill-bound", "iid-max", familyDirect, def, checkPrefillBoundNDependence},
		// known truth
		{classKnownTruth, "itl", "line-at-kprice", familyDirect, def, checkITLKnownTruth(itlDef, false)},
		{classKnownTruth, "itl", "line-at-kprice-timebase", familyTimeBase, def, checkITLKnownTruth(itlDef, true)},
		{classKnownTruth, "mu", "choice-priced", familyDirect, def, checkMuChoiceTruth(realMuChooser)},
		{classKnownTruth, "floor", "steady-order", familyDirect, def, checkFloorTruth(nil)},
		// scale
		{classScale, "k2", "block-size-and-cycle", familyDirect, def, checkK2Scale(nil)},
		// liveness
		{classLiveness, "k2", "n2-aggregated", familyDirect, def, checkK2Liveness("aggregated", nil)},
		{classLiveness, "k2", "pd-decode", familyDirect, def, checkK2Liveness("pd", nil)},
		{classLiveness, "k2", "flow-control-on", familyDirect, def, checkK2Liveness("flow-control", nil)},
		{classLiveness, "mu", "n2-aggregated", familyDirect, def, checkMuLiveness},
		// monotonicity
		{classMonotonicity, "floor", "lambda-and-queue-rise", familyDirect, def, checkFloorMonotone},
		{classMonotonicity, "floor", "standing-local-queue", familyDirect, def, checkFloorDrainBound(nil)},
		// edges by enumeration
		{classEdges, "mu", "latch-at-2x", familyDirect, def, checkMuLatch(tuningDefault)},
		{classEdges, "floor", "backlog-grew", familyDirect, def, checkBacklogEdge(tuningDefault)},
		{classEdges, "idle", "idle-line-030", familyTimeBase, def, checkIdleEdge(realIdleVerdict)},
		{classEdges, "itl", "bimodal-fit", familyDirect, def, checkITLBimodal(itl.FitPinnedB)},
		{classEdges, "k2", "key-on-output-boundary", familyDirect, def, checkK2KeyEdge},
	}
}

// ===== ITL =====

// itlFleet runs l over cycles of rows produced by gen, from propStart, and
// returns each cycle's reading.
func itlRun(l itlLearner, cycles int, gen func(i int, now time.Time) []domain.ReplicaMetrics) []itl.Reading {
	out := make([]itl.Reading, cycles)
	for i := range cycles {
		now := propStart.Add(time.Duration(i) * propCycle)
		out[i] = l.observe(gen(i, now), now)
	}
	return out
}

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

// checkITLJoiner: a replica that joins reads far above the line for its first
// 90 s. Against the same run without it, the line priced at kPrice moves by
// at most propTransientTol once the fleet has a line.
func checkITLJoiner(newL func() itlLearner, joiners int, dur time.Duration) func(uint64) error {
	return func(seed uint64) error {
		cycles := int(dur / propCycle)
		base := steadyFleet(propRand(seed), 3, cycles)
		joinAt := cycles / 3
		control := itlRun(newL(), cycles, func(i int, _ time.Time) []domain.ReplicaMetrics { return base(i) })
		joined := itlRun(newL(), cycles, func(i int, _ time.Time) []domain.ReplicaMetrics {
			rows := base(i)
			if i < joinAt {
				return rows
			}
			for j := range joiners {
				k := 0.5
				lat := propITL(k)
				if i-joinAt < 6 { // 90 s of warm-up
					lat *= 5
				}
				rows = append(rows, itlRow(fmt.Sprintf("j%d", j), k, lat))
			}
			return rows
		})
		for i := joinAt; i < cycles; i++ {
			c, j := itlAt(control[i]), itlAt(joined[i])
			if math.IsNaN(c) || math.IsNaN(j) {
				continue
			}
			if e := relErr(j, c); e > propTransientTol {
				return fmt.Errorf("cycle %d (%s after the join): ITL(%.2f) %.5f with the joiner against %.5f without, %.1f%% apart",
					i, time.Duration(i-joinAt)*propCycle, propKPrice, j, c, 100*e)
			}
		}
		return ok(!math.IsNaN(itlAt(control[cycles-1])), "the control run never fitted a line")
	}
}

// checkITLOneReading: one reading from one replica sets no line.
func checkITLOneReading(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		k := 0.3 + 0.4*r.Float64()
		got := newL().observe([]domain.ReplicaMetrics{itlRow("r0", k, propITL(k)*(1+0.2*r.NormFloat64()))}, propStart)
		return ok(got.Model.IsZero(), "one reading set the line A=%.4f B=%.4f", got.Model.A, got.Model.B)
	}
}

// checkITLStaleB: from identifying data at N >= 2 -- replicas held at
// distinct levels -- B converges to the true intercept (catches #273 A3, B
// stuck at the 6 ms default).
func checkITLStaleB(newL func() itlLearner, n int, dur time.Duration) func(uint64) error {
	return func(seed uint64) error {
		cycles := int(dur / propCycle)
		fleet := steadyFleet(propRand(seed), n, cycles)
		l := newL()
		itlRun(l, cycles, func(i int, _ time.Time) []domain.ReplicaMetrics { return fleet(i) })
		b, have := l.baseline()
		if !have {
			return fmt.Errorf("N=%d: no intercept learned in %s; B stays at the %.0f ms default", n, dur, 1000*itl.DefaultBaselineSec)
		}
		return ok(relErr(b, propTrueB) <= 0.10, "N=%d: B %.4f s against a true %.4f s after %s", n, b, propTrueB, dur)
	}
}

// checkITLStaleBTimeBase is checkITLStaleB with each replica's k and ITL
// passed through the time base: k instant, ITL a rate[1m] over the minute.
func checkITLStaleBTimeBase(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		const n, secs = 2, 12 * 60
		lv := baseLevels(n)
		tbs := make([]timeBase, n)
		itls := make([]timeBase, n)
		for p := range n {
			k := make([]float64, secs)
			lat := make([]float64, secs)
			noise := arNoise(r, secs, 0.02, 0.98)
			for s := range k {
				k[s] = lv[p] * (1 + noise[s])
				lat[s] = propITL(k[s])
			}
			tbs[p], itls[p] = sampleTimeBase(k), sampleTimeBase(lat)
		}
		l := newL()
		itlRun(l, len(tbs[0].instant), func(i int, _ time.Time) []domain.ReplicaMetrics {
			rows := make([]domain.ReplicaMetrics, n)
			for p := range n {
				rows[p] = itlRow(fmt.Sprintf("r%d", p), tbs[p].instant[i], itls[p].rate1m[i])
			}
			return rows
		})
		b, have := l.baseline()
		if !have {
			return errors.New("no intercept learned on the time base; B stays at the default")
		}
		return ok(relErr(b, propTrueB) <= 0.10, "B %.4f s against a true %.4f s", b, propTrueB)
	}
}

// checkITLMissedRow: one replica's row missing for a cycle, and another's
// read as a failed scrape (k and ITL unknown), leave the priced line where
// the same run without them puts it.
func checkITLMissedRow(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		const cycles = 120
		fleet := steadyFleet(propRand(seed), 3, cycles)
		control := itlRun(newL(), cycles, func(i int, _ time.Time) []domain.ReplicaMetrics { return fleet(i) })
		missed := itlRun(newL(), cycles, func(i int, _ time.Time) []domain.ReplicaMetrics {
			rows := fleet(i)
			switch i {
			case 60:
				rows = append(rows[:1], rows[2:]...)
			case 70:
				rows[2] = domain.ReplicaMetrics{PodName: "r2", VariantName: "v", Ready: true}
			}
			return rows
		})
		for i := 60; i < cycles; i++ {
			c, m := itlAt(control[i]), itlAt(missed[i])
			if math.IsNaN(c) != math.IsNaN(m) {
				return fmt.Errorf("cycle %d: a line in one run and none in the other (%.5f, %.5f)", i, c, m)
			}
			if !math.IsNaN(c) && relErr(m, c) > 0.02 {
				return fmt.Errorf("cycle %d: ITL(%.2f) %.5f after the missed rows against %.5f", i, propKPrice, m, c)
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
			return errors.New("no fit")
		}
		want := num / den
		return ok(relErr(m.A, want) <= 0.01, "A %.4f against the k-weighted %.4f: the slow replica was not weighed in", m.A, want)
	}
}

// checkITLFlagged: joiners whose warm-up the tracker flags (settling) swept up
// to half the fleet's readings are dropped: the line stays where the same run
// without them puts it.
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
// (too low is the side that over-prices derived mu). On the time base, k is
// the instant reading and ITL a rate[1m] (#273 A5), with load that moves
// inside the minute.
func checkITLKnownTruth(newL func() itlLearner, timeBased bool) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		l := newL()
		var last itl.Reading
		if !timeBased {
			fleet := steadyFleet(r, 3, 60)
			for _, rd := range itlRun(l, 60, func(i int, _ time.Time) []domain.ReplicaMetrics { return fleet(i) }) {
				last = rd
			}
		} else {
			const n, secs = 3, 15 * 60
			lv := baseLevels(n)
			k := make([]timeBase, n)
			lat := make([]timeBase, n)
			for p := range n {
				ks := make([]float64, secs)
				ls := make([]float64, secs)
				for s := range ks {
					// Bursty decode: the batch swings by a quarter inside
					// the minute around a steady level.
					ks[s] = math.Max(0.01, lv[p]*(1+0.25*r.NormFloat64()))
					ls[s] = propITL(ks[s])
				}
				k[p], lat[p] = sampleTimeBase(ks), sampleTimeBase(ls)
			}
			for _, rd := range itlRun(l, len(k[0].instant), func(i int, _ time.Time) []domain.ReplicaMetrics {
				rows := make([]domain.ReplicaMetrics, n)
				for p := range n {
					rows[p] = itlRow(fmt.Sprintf("r%d", p), k[p].instant[i], lat[p].rate1m[i])
				}
				return rows
			}) {
				last = rd
			}
		}
		got, want := itlAt(last), propITL(propKPrice)
		if math.IsNaN(got) {
			return errors.New("no line")
		}
		return ok(got >= want*(1-propTruthTol), "ITL(%.2f) priced %.5f s against a true %.5f s (%s tier, A %.4f B %.4f): %.1f%% low",
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
				// Replica i's load share: the second group's weight is w.
				kk := k * 2 * (1 - w)
				a := a1
				if i >= 5 {
					kk, a = k*2*w, a2
				}
				obs = append(obs, itl.Observation{K: kk, ITLSec: a*kk + propTrueB, Timestamp: propStart})
			}
			m, fitted := fit(obs, propTrueB)
			if !fitted {
				return fmt.Errorf("no fit at share %.2f", w)
			}
			if !math.IsNaN(prev) {
				step := math.Abs(m.A - prev)
				if step > 2*(a2-a1)*0.4/steps+1e-9 {
					return fmt.Errorf("share %.2f: A jumped %.4f in one step of %.3f (a mixture moves %.4f)", w, step, 0.4/steps, (a2-a1)*0.4/steps)
				}
			}
			prev = m.A
		}
		return nil
	}
}

// ===== mu =====

// checkMuNDependence: N pods with iid noise; the figure's expectation does
// not depend on N in {1, 2, 4, 16} beyond propNDepBias (catches the max
// across replicas, +13.6 % at 16). On the time base each pod's rate is a
// rate[1m] over per-second completions.
func checkMuNDependence(newL func() muLearner, timeBased bool) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		const trials, minutes = 40, 10
		mean := map[int]float64{}
		for _, n := range []int{1, 2, 4, 16} {
			sum := 0.0
			for range trials {
				l := newL()
				for range minutes {
					for p := range n {
						v := 1 + 0.1*r.NormFloat64()
						if timeBased {
							// rate[1m] of a per-second Poisson count at 1/s.
							c := 0.0
							for range 60 {
								c += poisson(r, 1)
							}
							v = c / 60
						}
						l.read(fmt.Sprintf("p%d", p), v)
					}
					l.next()
				}
				sum += l.figure()
			}
			mean[n] = sum / trials
		}
		for _, n := range []int{2, 4, 16} {
			if e := mean[n]/mean[1] - 1; math.Abs(e) > propNDepBias {
				return fmt.Errorf("E[mu] at N=%d is %.4f against %.4f at N=1: %+.1f%%", n, mean[n], mean[1], 100*e)
			}
		}
		return nil
	}
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

// checkMuOneSlowPod: one pod genuinely at half the others' rate, N in
// {4, 16}. Mu's fold is the mean, the declared estimand, so the slow pod
// moves mu by its weight only: a passing guard (2.6), not a ledger row.
func checkMuOneSlowPod(newL func() muLearner, n int) func(uint64) error {
	return func(seed uint64) error {
		level := 0.5 + propRand(seed).Float64()
		l := newL()
		for range 3 {
			for p := range n {
				v := level
				if p == n-1 {
					v = level / 2
				}
				l.read(fmt.Sprintf("p%d", p), v)
			}
			l.next()
		}
		want := level * (float64(n) - 0.5) / float64(n)
		return ok(relErr(l.figure(), want) <= 1e-9, "N=%d, one pod at half: mu %.6f against the mean %.6f", n, l.figure(), want)
	}
}

// checkMuDoubled: one pod's rate reaches the analyzer at 2x (a series doubled
// inside PromQL, the prefill double scrape): mu moves by that pod's weight,
// no more.
func checkMuDoubled(newL func() muLearner) func(uint64) error {
	return func(seed uint64) error {
		level := 0.5 + propRand(seed).Float64()
		const n = 4
		l := newL()
		for range 3 {
			for p := range n {
				v := level
				if p == 0 {
					v = 2 * level
				}
				l.read(fmt.Sprintf("p%d", p), v)
			}
			l.next()
		}
		want := level * (n + 1) / n
		return ok(relErr(l.figure(), want) <= 1e-9, "one pod at 2x among %d: mu %.6f against %.6f", n, l.figure(), want)
	}
}

// checkMuMissingDuplicate: a pod whose rate is missing (read as 0) and a pod
// listed twice in one cycle leave mu where four clean pods put it.
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
		return ok(relErr(dirty.figure(), clean.figure()) <= 1e-9, "mu %.6f with a missing and a duplicated row, %.6f without", dirty.figure(), clean.figure())
	}
}

// checkMuFirstSample: E1's first sample, a fresh pod at half its true rate
// with the line's derived mu at the truth, is not adopted: a second,
// identifying reading would move it by more than the band.
func checkMuFirstSample(tn config.AnalyzerTuning) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		truth := 0.2 * (1 + 0.1*r.Float64())
		first := truth * (0.40 + 0.05*r.Float64()) // E1: 0.0995 against about 0.2
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		derived := derivedMu{rate: truth, ok: true}
		if t.throughputRefusal(first, first, derived, 0, false) != "" {
			return nil // refused: the figure is not set by it
		}
		moved := relErr((first+truth)/2, first)
		return ok(moved <= propBand, "a first reading of %.4f against a derived %.4f was admitted; the second moves it %.0f%%", first, truth, 100*moved)
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
		return ok(t.tokensOutrunCompletions(rm, domain.RoleDecode, 500),
			"a filling replica (%.0f tokens/s against %.1f completions x 500) was admitted", rm.GenerationTokenRate, rm.RequestRate)
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
			if got > truth*(1+propTruthTol) {
				return fmt.Errorf("measured %.3f (fresh) and derived %.3f priced %.3f", truth, derived, got)
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
	fleet := []float64{1, 1, 1, slow}
	priced := slow
	if s, yielded := a.siblingYield(propMuKey, throughputReading{rate: slow}, servedRates{max: 1, saturated: 1, unqueued: true}, t); yielded {
		priced = s
	}
	mean := (fleet[0] + fleet[1] + fleet[2] + fleet[3]) / 4
	return ok(priced <= mean*(1+propTruthTol), "fleet %v (mean %.3f): the sibling yield priced %.3f, %.0f%% over", fleet, mean, priced, 100*(priced/mean-1))
}

// checkMuHalfServed: a fleet with one genuinely slow pod under half the
// fastest's rate. Its readings are refused (half the served max), so mu is
// the others' mean, over the fleet's.
func checkMuHalfServed(seed uint64) error {
	r := propRand(seed)
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	slow := 0.3 + 0.15*r.Float64()
	fleet := []float64{1, 1, 1, slow}
	var sum float64
	var n int
	for _, v := range fleet {
		if t.throughputRefusal(v, v, derivedMu{}, 1, false) == "" {
			sum += v
			n++
		}
	}
	mean := (3 + slow) / 4
	priced := sum / float64(n)
	return ok(priced <= mean*(1+propTruthTol), "fleet %v (mean %.3f): admitted readings price %.3f, %.0f%% over", fleet, mean, priced, 100*(priced/mean-1))
}

// checkMuLatch: measured over derived sits at 2x, the latch's set point, with
// autocorrelated noise; the noise-free ratio never crosses, so the choice
// changes at most once.
func checkMuLatch(tn config.AnalyzerTuning) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		a := NewSaturationAnalyzer(capacity.NewStore())
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		noise := arNoise(r, 120, 0.04, 0.8)
		v := make([]bool, len(noise))
		for i, e := range noise {
			now := propStart.Add(time.Duration(i) * propCycle)
			v[i] = a.measuredYieldsToDerived(propMuKey, throughputReading{rate: 2 * (1 + e)}, derivedMu{rate: 1, ok: true}, 0, t, now)
		}
		return ok(reversals(v) <= 1, "measured/derived on 2.0 with 4%% noise: the choice reversed %d times in %d cycles", reversals(v), len(v))
	}
}

// checkMuLiveness: two aggregated replicas saturated from the start: mu is
// measured within 3 min, within the band of the rate they serve.
func checkMuLiveness(seed uint64) error { return muLiveness(seed, nil) }

func muLiveness(seed uint64, wrap func(*domain.ReplicaMetrics)) error {
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
	rig.a.mu.Lock()
	var got float64
	for _, ra := range rig.a.saturatedThroughput {
		got = ra.Median()
	}
	rig.a.mu.Unlock()
	return ok(got > 0 && relErr(got, truth) <= propBand, "after 3 min saturated at %.3f req/s, measured mu %.3f", truth, got)
}

// ===== floor, idle =====

// checkBacklogEdge: the backlog stands at 50 with 2 % autocorrelated noise; the
// noise-free backlog never grows, so backlogGrew never sets.
func checkBacklogEdge(tn config.AnalyzerTuning) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		a := NewSaturationAnalyzer(capacity.NewStore())
		clock := propStart
		a.now = func() time.Time { return clock }
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		noise := arNoise(r, 100, 0.02, 0.8)
		v := make([]bool, len(noise))
		for i, e := range noise {
			v[i] = a.noteBacklogGrowth("ns", "m", map[string]float64{"decode": 50 * (1 + e)}, t)["decode"]
			clock = clock.Add(propCycle)
		}
		return ok(reversals(v) == 0, "a standing backlog of 50 with 2%% noise read as grown: %d reversals in %d cycles", reversals(v), len(v))
	}
}

// idleVerdict says, cycle by cycle, whether a decode replica holding frac of
// its cache counts as settled idle room.
type idleVerdict func(a *SaturationAnalyzer, frac float64, now time.Time) bool

func realIdleVerdict(a *SaturationAnalyzer, frac float64, now time.Time) bool {
	rm := satRow("r0", int64(frac*float64(propKv)), 0, 0.5)
	caps := []capacity.ReplicaCapacity{{VariantName: "v", EffectiveCapacity: propKv}}
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults(), idleKvUsage: config.DefaultIdleKvUsage}
	return a.noteIdleRuns("ns", "m", caps, []domain.ReplicaMetrics{rm}, map[string]string{"v": domain.RoleDecode}, t, now)[0]
}

// checkIdleEdge: a decode replica's one-minute peak KV sits on the 0.30 idle
// line; the per-second path wanders around it. The idle-room verdict changes
// at most once per crossing of the line by the noise-free path (none here,
// so at most once).
func checkIdleEdge(verdict idleVerdict) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		// Per-second KV with a 30 s correlation time; the peak the analyzer
		// reads is put on the line: its median over the run is 0.30.
		path := arNoise(r, 30*60, 0.10, 0.967)
		for i := range path {
			path[i] = 1 + path[i]
		}
		tb := sampleTimeBase(path)
		med := append([]float64(nil), tb.peak1m...)
		sort.Float64s(med)
		for i := range tb.peak1m {
			tb.peak1m[i] *= 0.30 / med[len(med)/2]
		}
		a := NewSaturationAnalyzer(capacity.NewStore())
		v := make([]bool, len(tb.peak1m))
		for i, k := range tb.peak1m {
			v[i] = verdict(a, k, propStart.Add(time.Duration(i)*propCycle))
		}
		return ok(reversals(v) <= 1, "peak KV on the 0.30 line: the idle verdict reversed %d times in %d cycles", reversals(v), len(v))
	}
}

// ===== start time, k2 first sample =====

func checkStartFirstSample(seed uint64) error {
	r := propRand(seed)
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.now = func() time.Time { return propStart }
	truth := 60 + 30*r.Float64()
	sign := 1.0
	if r.IntN(2) == 0 {
		sign = -1
	}
	s1, s2 := truth*(1+0.3*sign), truth*(1-0.3*sign)
	row := func(pod string, s float64) domain.ReplicaMetrics {
		rm := satRow(pod, 100_000, 0, 1)
		rm.StartSeconds = s
		return rm
	}
	key := "start"
	a.noteReplicaStart(key, "test-ns", "v", []domain.ReplicaMetrics{row("p1", s1)}, logr.Discard())
	f1 := a.startSeconds[key]
	a.noteReplicaStart(key, "test-ns", "v", []domain.ReplicaMetrics{row("p1", s1), row("p2", s2)}, logr.Discard())
	f2 := a.startSeconds[key]
	return ok(relErr(f2, f1) <= propBand, "first pod %.0f s set the estimate to %.1f s; a second at %.0f s moved it to %.1f s (%.0f%%)", s1, f1, s2, f2, 100*relErr(f2, f1))
}

func checkK2FirstSample(seed uint64) error {
	r := propRand(seed)
	sign := 1.0
	if r.IntN(2) == 0 {
		sign = -1
	}
	truth := 250_000.0 * (1 + 0.1*r.Float64())
	rig := newPropRig()
	var figs []float64
	for _, f := range []float64{1 + 0.12*sign, 1 - 0.12*sign} {
		if _, err := rig.analyze([]domain.ReplicaMetrics{satRow("r0", int64(truth*f), 10, 6)}, decodeStates(1), 6, propCycle); err != nil {
			return err
		}
		figs = append(figs, rig.k2(""))
	}
	if figs[0] == 0 {
		return errors.New("the first reading was not admitted")
	}
	return ok(relErr(figs[1], figs[0]) <= propBand, "k2 %.0f from its first reading, %.0f after a second (%.0f%%)", figs[0], figs[1], 100*relErr(figs[1], figs[0]))
}

// ===== k2 through the analyzer =====

// checkK2LoadStep is TestK2Settle_ALoadStepIsNotALimit with seeded levels: a
// deep queue lands on an idle replica, which reads 8 % of its cache. The
// step's reading is refused.
func checkK2LoadStep(policy func(*config.ScalingPolicy), wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		rig := newPropRig()
		rig.policy, rig.wrap = policy, wrap
		full := int64(270_000 * (1 + 0.05*r.Float64()))
		steps := []struct {
			at     time.Duration
			tokens int64
			queue  int
			rate   float64
		}{
			{65 * time.Second, full, 30, 6},
			{15 * time.Second, 12_000, 0, 0.0024},
			{15 * time.Second, int64(float64(propKv) * (0.07 + 0.02*r.Float64())), 160, 0.0019},
		}
		var before float64
		for i, s := range steps {
			if i == 2 {
				before = rig.k2("")
			}
			if _, err := rig.analyze([]domain.ReplicaMetrics{satRow("r0", s.tokens, s.queue, s.rate)}, decodeStates(1), 6, s.at); err != nil {
				return err
			}
		}
		after := rig.k2("")
		return ok(before > 0 && relErr(after, before) <= 1e-9, "a load step's reading moved k2 %.0f -> %.0f", before, after)
	}
}

// checkK2Restart: a replica restarted under the same name (a new Ready time)
// reads a steady, plausible figure from its first cycle. Nothing is admitted
// within a minute of its Ready time.
func checkK2Restart(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		rig := newPropRig()
		rig.wrap = wrap
		before := int64(200_000 * (1 + 0.05*r.Float64()))
		ready := propStart.Add(-time.Hour)
		for range 12 {
			rm := satRow("r0", before, 10, 6)
			rm.ReadySince = ready
			if _, err := rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 6, propCycle); err != nil {
				return err
			}
		}
		held := rig.k2("")
		ready = rig.clock.Add(-5 * time.Second)
		after := int64(float64(before) * 0.78)
		for i := range 3 {
			rm := satRow("r0", after, 10, 6)
			rm.ReadySince = ready
			if _, err := rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 6, propCycle); err != nil {
				return err
			}
			if got := rig.k2(""); relErr(got, held) > 1e-9 {
				return fmt.Errorf("%s after its Ready time, the restarted replica's reading moved k2 %.0f -> %.0f",
					time.Duration(i)*propCycle+5*time.Second, held, got)
			}
		}
		return nil
	}
}

// checkK2Fill: a replica under a standing queue fills toward its plateau with
// a one-minute time constant; its KV reaches the analyzer as the
// max_over_time[1m] peak. No reading under 0.85 of the plateau is admitted.
func checkK2Fill(policy func(*config.ScalingPolicy)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		plateau := 240_000 * (1 + 0.05*r.Float64())
		// Idle for two minutes, then the load lands: a standing queue, and
		// the batch fills toward its plateau with a one-minute time constant.
		const idleFor = 120
		path := make([]float64, 7*60)
		for s := range path {
			path[s] = 12_000
			if s >= idleFor {
				path[s] = plateau - (plateau-12_000)*math.Exp(-float64(s-idleFor)/60)
			}
		}
		tb := sampleTimeBase(path)
		rig := newPropRig()
		rig.policy = policy
		for i, tok := range tb.peak1m {
			queue := 0
			if 60+i*int(propCycle/time.Second) >= idleFor {
				queue = 200
			}
			if _, err := rig.analyze([]domain.ReplicaMetrics{satRow("r0", int64(tok), queue, 0)}, decodeStates(1), 6, propCycle); err != nil {
				return err
			}
			if got := rig.k2(""); got > 0 && got < 0.85*plateau {
				return fmt.Errorf("cycle %d: k2 %.0f admitted while the replica filled toward %.0f (%.0f%%)", i, got, plateau, 100*got/plateau)
			}
		}
		return ok(rig.k2("") > 0, "the filled replica was never admitted")
	}
}

// checkK2UnknownGauges: a cycle whose row has every gauge unknown -- a failed
// scrape, the collector's zeros -- with a scheduler queue standing leaves k2
// where it was.
func checkK2UnknownGauges(seed uint64) error {
	r := propRand(seed)
	rig := newPropRig()
	tok := int64(260_000 * (1 + 0.05*r.Float64()))
	for range 6 {
		if _, err := rig.analyze([]domain.ReplicaMetrics{satRow("r0", tok, 10, 6)}, decodeStates(1), 6, propCycle); err != nil {
			return err
		}
	}
	held := rig.k2("")
	blank := domain.ReplicaMetrics{PodName: "r0", VariantName: "v", Ready: true, ModelID: "test-model", Namespace: "test-ns"}
	in := makeAnalyzerInput([]domain.ReplicaMetrics{blank}, decodeStates(1))
	in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 200}
	if _, err := rig.a.Analyze(rig.ctx(), in); err != nil {
		return err
	}
	return ok(relErr(rig.k2(""), held) <= 1e-9, "a failed scrape moved k2 %.0f -> %.0f", held, rig.k2(""))
}

// checkK2Redistribution: four replicas at the same full cache; the queue
// spread over all four, or moved onto one with the others draining. The k2
// learned is the same within the transient tolerance.
func checkK2Redistribution(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		full := int64(float64(propKv) * (0.9 + 0.05*r.Float64()))
		run := func(queues []int, tokens []int64) (float64, error) {
			rig := newPropRig()
			rig.wrap = wrap
			for range 8 {
				rows := make([]domain.ReplicaMetrics, 4)
				for p := range rows {
					rows[p] = satRow(fmt.Sprintf("r%d", p), tokens[p], queues[p], 6)
				}
				if _, err := rig.analyze(rows, decodeStates(4), 24, propCycle); err != nil {
					return 0, err
				}
			}
			return rig.k2(""), nil
		}
		spread, err := run([]int{10, 10, 10, 10}, []int64{full, full, full, full})
		if err != nil {
			return err
		}
		half := full / 2
		moved, err := run([]int{40, 0, 0, 0}, []int64{full, half, half, half})
		if err != nil {
			return err
		}
		return ok(spread > 0 && relErr(moved, spread) <= propTransientTol, "k2 %.0f with the queue spread, %.0f with it moved onto one replica", spread, moved)
	}
}

// checkDemandAlternating: two replicas share a constant load, either evenly or
// alternating every 30 s; Prometheus serves each pod's one-minute peak and its
// instant reading. Fleet demand is the same within 10 %.
func checkDemandAlternating(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		total := 0.6 + 0.2*r.Float64()
		const secs = 5 * 60
		even, alt0, alt1 := make([]float64, secs), make([]float64, secs), make([]float64, secs)
		for s := range secs {
			even[s] = total / 2
			if (s/30)%2 == 0 {
				alt0[s], alt1[s] = total, 0.02
			} else {
				alt0[s], alt1[s] = 0.02, total
			}
		}
		demand := func(p0, p1 []float64) (float64, error) {
			tb0, tb1 := sampleTimeBase(p0), sampleTimeBase(p1)
			rig := newPropRig()
			rig.wrap = wrap
			var res *domain.AnalyzerResult
			for i := range tb0.instant {
				rows := []domain.ReplicaMetrics{tbRow("r0", tb0, i), tbRow("r1", tb1, i)}
				var err error
				if res, err = rig.analyze(rows, decodeStates(2), 0, propCycle); err != nil {
					return 0, err
				}
			}
			return res.TotalDemand, nil
		}
		de, err := demand(even, even)
		if err != nil {
			return err
		}
		da, err := demand(alt0, alt1)
		if err != nil {
			return err
		}
		return ok(de > 0 && relErr(da, de) <= 0.10, "fleet demand %.0f with the load even, %.0f with it alternating (%.0f%%)", de, da, 100*(da/de-1))
	}
}

// tbRow is a decode replica whose KV is sampled from tb at cycle i: the peak
// as KvCacheUsage and TokensInUse, the instant as KvUsageInstant.
func tbRow(pod string, tb timeBase, i int) domain.ReplicaMetrics {
	rm := satRow(pod, int64(tb.peak1m[i]*float64(propKv)), 0, 1)
	rm.KvUsageInstant, rm.KvUsageInstantKnown = tb.instant[i], true
	return rm
}

// checkK2Scale: the same fleet described in 16- and 32-token blocks, and
// cycled at 15 s and 30 s, learns the same k2 and prices the same per-replica
// capacity.
func checkK2Scale(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		tok := int64(float64(propKv) * (0.85 + 0.1*r.Float64()))
		run := func(block int64, cycle time.Duration) (float64, float64, error) {
			rig := newPropRig()
			rig.wrap = wrap
			var res *domain.AnalyzerResult
			for range 8 {
				rm := satRow("r0", tok, 10, 6)
				rm.BlockSize, rm.NumGpuBlocks = block, propKv/block
				var err error
				if res, err = rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 6, cycle); err != nil {
					return 0, 0, err
				}
			}
			var per float64
			for _, vc := range res.VariantCapacities {
				per = vc.PerReplicaCapacity
			}
			return rig.k2(""), per, nil
		}
		k16, p16, err := run(16, propCycle)
		if err != nil {
			return err
		}
		for _, alt := range []struct {
			block int64
			cycle time.Duration
		}{{32, propCycle}, {16, 2 * propCycle}} {
			k, p, err := run(alt.block, alt.cycle)
			if err != nil {
				return err
			}
			if relErr(k, k16) > 1e-9 || relErr(p, p16) > 1e-9 {
				return fmt.Errorf("block %d, cycle %s: k2 %.0f and per-replica %.0f, against %.0f and %.0f at 16 / 15 s",
					alt.block, alt.cycle, k, p, k16, p16)
			}
		}
		return nil
	}
}

// checkK2Liveness: a saturated fleet under each topology learns a measured k2
// within 3 min, within the band of what its replicas hold.
func checkK2Liveness(topology string, wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		truth := float64(propKv) * (0.85 + 0.1*r.Float64())
		rig := newPropRig()
		rig.wrap = wrap
		for range 13 {
			var rows []domain.ReplicaMetrics
			var states []domain.VariantReplicaState
			var sched *domain.SchedulerQueueMetrics
			switch topology {
			case "aggregated":
				rows = []domain.ReplicaMetrics{satRow("r0", int64(truth), 10, 6), satRow("r1", int64(truth), 10, 6)}
				states = aggStates(2)
			case "pd":
				rows = make([]domain.ReplicaMetrics, 0, 3)
				rows = append(rows, satRow("d0", int64(truth), 10, 6), satRow("d1", int64(truth), 10, 6))
				for j := range rows {
					rows[j].CapacityQueueKnown = true
					rows[j].CapacityQueueLength = 10
				}
				p := satRow("p0", 20_000, 0, 6)
				p.VariantName = "p"
				rows = append(rows, p)
				states = []domain.VariantReplicaState{decodeStates(2)[0],
					{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill}}
			case "flow-control":
				// The queue stands at the scheduler; the engines dispatch
				// what they take, so their own queues read empty.
				rows = []domain.ReplicaMetrics{satRow("r0", int64(truth), 0, 6), satRow("r1", int64(truth), 0, 6)}
				states = decodeStates(2)
				sched = &domain.SchedulerQueueMetrics{QueueSize: 40}
			}
			in := makeAnalyzerInput(rows, states)
			in.ArrivalRate, in.SchedulerQueue = 12, sched
			for j := range in.ReplicaMetrics {
				if rig.wrap != nil {
					rig.wrap(&in.ReplicaMetrics[j])
				}
			}
			if _, err := rig.a.Analyze(rig.ctx(), in); err != nil {
				return err
			}
			rig.clock = rig.clock.Add(propCycle)
		}
		got := rig.k2(domain.RoleDecode)
		if topology == "aggregated" {
			got = rig.k2("")
		}
		return ok(got > 0 && relErr(got, truth) <= propBand, "%s: after 3 min saturated at %.0f tokens, k2 %.0f", topology, truth, got)
	}
}

// ===== the floor through the analyzer =====

// floorRig is an aggregated variant whose measured mu is about 1 req/s,
// learned on four replicas over four saturated cycles a minute apart
// (imbFloorFleetAt's seed).
func floorRig() (*propRig, error) {
	rig := newPropRig()
	for i := range 4 {
		seed := make([]domain.ReplicaMetrics, 0, 4)
		for _, p := range []string{"r0", "r1", "r2", "r3"} {
			rm := imbRow(p, "v", 0.95, 10, 1000, 1000, []float64{1.0, 1.02, 0.98, 1.01}[i])
			seed = append(seed, rm)
		}
		if _, err := rig.analyze(seed, aggStates(4), 4, ThroughputSampleSpacing+time.Second); err != nil {
			return nil, err
		}
	}
	return rig, nil
}

// impliedReplicas is the demand a result prices, in replicas at the shipped
// scale-up target.
func impliedReplicas(res *domain.AnalyzerResult) float64 {
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "v" && vc.PerReplicaCapacity > 0 {
			return res.TotalDemand / (0.85 * vc.PerReplicaCapacity)
		}
	}
	return math.NaN()
}

// floorImplied is what floorRig orders for rows at lambda, settled.
func floorImplied(rows []domain.ReplicaMetrics, lambda float64) (float64, error) {
	rig, err := floorRig()
	if err != nil {
		return 0, err
	}
	var res *domain.AnalyzerResult
	start := rig.clock
	for rig.clock.Sub(start) <= IdleSettledFor+K2SettleLookback+2*propCycle {
		if res, err = rig.analyze(rows, aggStates(len(rows)), lambda, propCycle); err != nil {
			return 0, err
		}
	}
	return impliedReplicas(res), nil
}

// floorOrder is what the floor prices, in replicas; a mutant edits it.
type floorOrder func(rows []domain.ReplicaMetrics, lambda float64) (float64, error)

// checkFloorTruth: at mu about 1 and the shipped 0.85 target, a steady fleet
// with no queue -- some replicas idle -- is ordered lambda / (mu x target).
func checkFloorTruth(order floorOrder) func(uint64) error {
	if order == nil {
		order = floorImplied
	}
	return func(seed uint64) error {
		r := propRand(seed)
		lambda := 2 + 2*r.Float64()
		rows := make([]domain.ReplicaMetrics, 6)
		for p := range rows {
			rate, frac := lambda/4, 0.4
			if p >= 4 {
				rate, frac = 0, 0.02 // idle
			}
			rows[p] = imbRow(fmt.Sprintf("r%d", p), "v", frac, 0, 1000, 1000, rate)
		}
		got, err := order(rows, lambda)
		if err != nil {
			return err
		}
		want := lambda / (1.0 * 0.85)
		return ok(relErr(got, want) <= propTruthTol, "lambda %.2f at mu 1: ordered %.2f replicas against %.2f", lambda, got, want)
	}
}

// checkFloorDrainBound: a queue of 300 pinned on one replica, standing,
// beside idle siblings: the order is bounded by what the fleet can drain --
// lambda plus each replica's own queue up to what it drains in the drain
// time (mu x T).
func checkFloorDrainBound(order floorOrder) func(uint64) error {
	if order == nil {
		order = floorImplied
	}
	return func(seed uint64) error {
		r := propRand(seed)
		lambda := 2 + r.Float64()
		rows := []domain.ReplicaMetrics{
			imbRow("r0", "v", 0.95, 300, 1000, 1000, 1.0),
			imbRow("r1", "v", 0.10, 0, 1000, 1000, 0.33),
			imbRow("r2", "v", 0.10, 0, 1000, 1000, 0.33),
			imbRow("r3", "v", 0.10, 0, 1000, 1000, 0.34),
		}
		got, err := order(rows, lambda)
		if err != nil {
			return err
		}
		bound := (lambda + 60.0/60) / 0.85 * (1 + propTruthTol)
		return ok(got <= bound, "a standing queue of 300 on one replica at lambda %.2f: ordered %.2f replicas, over the %.2f it can drain", lambda, got, bound)
	}
}

// checkFloorMonotone: lambda rises and the queue with it, every replica
// busy; along the trajectory the order never falls.
func checkFloorMonotone(seed uint64) error {
	r := propRand(seed)
	rig, err := floorRig()
	if err != nil {
		return err
	}
	prev := 0.0
	queue := 0.0
	for i := range 40 {
		lambda := 2 + 4*float64(i)/40*(1+0.01*r.NormFloat64())
		queue += math.Max(0, lambda-4) * propCycle.Seconds() / 4
		rows := make([]domain.ReplicaMetrics, 4)
		for p := range rows {
			rows[p] = imbRow(fmt.Sprintf("r%d", p), "v", 0.95, int(queue), 1000, 1000, math.Min(lambda/4, 1))
		}
		res, err := rig.analyze(rows, aggStates(4), lambda, propCycle)
		if err != nil {
			return err
		}
		got := impliedReplicas(res)
		if got < prev*(1-0.02) {
			return fmt.Errorf("cycle %d: lambda %.2f, queue %.0f per replica: the order fell %.2f -> %.2f", i, lambda, queue, prev, got)
		}
		prev = math.Max(prev, got)
	}
	return nil
}

// ===== prefill bound, k2 key =====

// checkPrefillBoundNDependence: the prefill bound writes through mu's fold
// under one empty pod name (recordSaturatedThroughput), so within a minute it
// keeps the max of N pods' readings. Its expectation must not depend on N.
func checkPrefillBoundNDependence(seed uint64) error {
	r := propRand(seed)
	const trials, minutes = 30, 10
	mean := map[int]float64{}
	for _, n := range []int{1, 4, 16} {
		sum := 0.0
		for range trials {
			m := newAnalyzerMu()
			for range minutes {
				for range n {
					m.a.recordSaturatedThroughput(propMuKey, 1+0.1*r.NormFloat64())
					m.clock = m.clock.Add(time.Second)
				}
				m.next()
			}
			sum += m.figure()
		}
		mean[n] = sum / trials
	}
	for _, n := range []int{4, 16} {
		if e := mean[n]/mean[1] - 1; math.Abs(e) > propNDepBias {
			return fmt.Errorf("E[bound] at N=%d is %.4f against %.4f at N=1: %+.1f%%", n, mean[n], mean[1], 100*e)
		}
	}
	return nil
}

// checkK2KeyEdge: a saturated fleet whose output length sits on the 500-token
// medium/long boundary with autocorrelated noise. The noise-free length never
// crosses, so the k2 key changes at most once.
func checkK2KeyEdge(seed uint64) error {
	r := propRand(seed)
	rig := newPropRig()
	noise := arNoise(r, 80, 0.06, 0.9)
	var keys []string
	for _, e := range noise {
		rows := make([]domain.ReplicaMetrics, 2)
		for p := range rows {
			rows[p] = satRow(fmt.Sprintf("r%d", p), 330_000, 10, 6)
			rows[p].AvgOutputTokens = 500 * (1 + e)
		}
		if _, err := rig.analyze(rows, decodeStates(2), 12, propCycle); err != nil {
			return err
		}
		keys = append(keys, newestK2Key(rig))
	}
	changes := 0
	for i := 1; i < len(keys); i++ {
		if keys[i] != keys[i-1] {
			changes++
		}
	}
	return ok(changes <= 1, "output length on the 500-token boundary: the k2 key changed %d times in %d cycles (%d keys)", changes, len(keys), len(uniq(keys)))
}

// newestK2Key is the k2 window written last.
func newestK2Key(rig *propRig) string {
	rig.a.mu.Lock()
	defer rig.a.mu.Unlock()
	var best string
	var at time.Time
	for k, ra := range rig.a.computeCapacityHistory {
		if w := ra.LastWritten(); ra.Len() > 0 && (best == "" || w.After(at) || (w.Equal(at) && k < best)) {
			best, at = k, w
		}
	}
	return best
}

func uniq(s []string) []string {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
