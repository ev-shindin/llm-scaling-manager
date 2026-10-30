package saturation

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The cases #389 listed as gaps on the classes it had: liveness for 8-GPU
// data-parallel ranks, a prefill role and the ITL line; scale over GPUs per
// replica and output length; the time-base family for the classes it
// exempted; and the prefill bound through a P/D Analyze.

const keyPrefillBound = "enablePrefillThroughputBound"

func moreCases() []propCase {
	def := func() itlLearner { return newTrackerITL(steadinessOf(tuningDefault)) }
	real := func() muLearner { return newAnalyzerMu() }
	return []propCase{
		// Liveness.
		{class: classLiveness, learner: "k2", name: "dp8-whole-node", family: familyDirect, tuning: tunedDefault, check: checkK2Ranks(8, 8)},
		{class: classLiveness, learner: "k2", name: "tp2-dp4-whole-node", family: familyDirect, tuning: tunedDefault, check: checkK2Ranks(4, 8)},
		{class: classLiveness, learner: "k2", name: "tp8-whole-node", family: familyDirect, tuning: tunedDefault, check: checkK2Ranks(1, 8)},
		{class: classLiveness, learner: "k2", name: "prefill-role", family: familyDirect, tuning: tunedDefault, check: checkK2Prefill},
		{class: classLiveness, learner: "itl", name: "line-within-3-min", family: familyDirect, tuning: tunedDefault, check: checkITLLiveness(def)},
		{class: classLiveness, learner: "prefill-bound", name: "pd-analyze", family: familyDirect, tuning: tunedPolicy,
			keys: []string{keyPrefillBound}, check: checkBoundAnalyze(1)},
		// Scale.
		{class: classScale, learner: "k2", name: "gpus-per-replica", family: familyDirect, tuning: tunedDefault, check: checkK2ScaleGPUs},
		{class: classScale, learner: "k2", name: "output-length", family: familyDirect, tuning: tunedDefault, check: checkK2ScaleOutput},
		{class: classScale, learner: "mu", name: "output-length", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuScaleOutput},
		// The time-base family.
		{class: classFirstSample, learner: "mu", name: "joiner-rate-timebase", family: familyTimeBase, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuFirstSampleTimeBase},
		{class: classMissing, learner: "itl", name: "missed-row-timebase", family: familyTimeBase, tuning: tunedDefault, check: checkITLMissedRowTimeBase(def)},
		{class: classHeterogeneity, learner: "mu", name: "one-slow-pod-n4-timebase", family: familyTimeBase, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuSlowPodTimeBase(real)},
		{class: classScale, learner: "k2", name: "block-size-timebase", family: familyTimeBase, tuning: tunedDefault, check: checkK2ScaleTimeBase},
		{class: classLiveness, learner: "k2", name: "n2-timebase", family: familyTimeBase, tuning: tunedDefault, check: checkK2LivenessTimeBase},
		{class: classMonotonicity, learner: "floor", name: "lambda-rise-timebase", family: familyTimeBase, tuning: tunedDefault, check: checkFloorMonotoneTimeBase},
		// The prefill bound through a P/D Analyze.
		{class: classHeterogeneity, learner: "prefill-bound", name: "one-pod-doubled-analyze", family: familyDirect, tuning: tunedPolicy,
			keys: []string{keyPrefillBound}, check: checkBoundAnalyze(2)},
	}
}

// --- liveness ---

// checkK2Ranks: one replica of 8 GPUs running ranks data-parallel engines
// (8: DP=8; 4: TP=2 x DP=4; 1: TP=8), its busiest rank full and queued,
// learns k2 within 3 min: the busiest rank's tokens, scaled to the replica.
func checkK2Ranks(ranks, gpus int) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		rig := newPropRig()
		busy := 0.88 + 0.05*r.Float64()
		states := []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: 1,
			GPUsPerReplica: gpus, Role: domain.RoleDecode}}
		var want int64
		for range 13 {
			rm := satRow("r0", int64(float64(propKv)*busy), 10, 6)
			rm.TotalKvCapacityTokens, rm.EngineRanks = propKv, ranks
			if ranks > 1 {
				// One rank full and queued, the rest at half: the pod's mean
				// is under the threshold, its busiest rank is not.
				mean := (busy + 0.5*float64(ranks-1)) / float64(ranks)
				rm.KvCacheUsage, rm.TokensInUse = mean, int64(mean*float64(propKv))
				rm.KvUsageInstant = mean
				rm.MaxRankKvCacheUsage, rm.MaxRankQueueLength = busy, 10
			}
			want = rm.BusiestRankTokens()
			if _, err := rig.analyze([]domain.ReplicaMetrics{rm}, states, 6, propCycle); err != nil {
				return err
			}
		}
		got := rig.k2("")
		if !(got > 0) {
			return violation(kindNotLearned, "%d ranks on %d GPUs: no k2 in 3 min", ranks, gpus)
		}
		return holds(relErr(got, float64(want)) <= propBand, kindBiased, "%d ranks on %d GPUs: k2 %.0f against the busiest rank's %d scaled", ranks, gpus, got, want)
	}
}

// checkK2Prefill: on a P/D model whose decode is not saturated, a prefill
// replica full and queued learns k2 within 3 min (#144: prefill never
// learns).
func checkK2Prefill(seed uint64) error {
	r := propRand(seed)
	rig := newPropRig()
	truth := float64(propKv) * (0.88 + 0.05*r.Float64())
	states := []domain.VariantReplicaState{decodeStates(1)[0],
		{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill}}
	for range 13 {
		p := satRow("p0", int64(truth), 10, 6)
		p.VariantName = "p"
		if _, err := rig.analyze([]domain.ReplicaMetrics{fullDecode("d0", 0.3, 0), p}, states, 6, propCycle); err != nil {
			return err
		}
	}
	got := rig.k2(domain.RolePrefill)
	if !(got > 0) {
		return violation(kindNotLearned, "after 3 min saturated at %.0f tokens, prefill has no k2", truth)
	}
	return holds(relErr(got, truth) <= propBand, kindBiased, "prefill k2 %.0f against %.0f", got, truth)
}

// checkITLLiveness: three replicas held at distinct steady levels give a line
// within 3 min, within the truth tolerance at kPrice.
func checkITLLiveness(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		const cycles = 12
		readings := itlRun(newL(), cycles, steadyFleet(propRand(seed), 3, cycles))
		got, want := itlAt(readings[cycles-1]), propITL(propKPrice)
		if math.IsNaN(got) {
			return violation(kindNotLearned, "no line after 3 min of three steady replicas")
		}
		return holds(relErr(got, want) <= propTruthTol, kindBiased, "ITL(%.2f) %.5f after 3 min against %.5f", propKPrice, got, want)
	}
}

// --- scale ---

// k2Priced is one saturated replica held for 2 min under edit, then the k2
// learned and the per-replica capacity priced.
func k2Priced(rows func() []domain.ReplicaMetrics, states []domain.VariantReplicaState) (float64, float64, error) {
	rig := newPropRig()
	var res *domain.AnalyzerResult
	for range 8 {
		var err error
		if res, err = rig.analyze(rows(), states, 6, propCycle); err != nil {
			return 0, 0, err
		}
	}
	per := math.NaN()
	for _, vc := range res.VariantCapacities {
		per = vc.PerReplicaCapacity
	}
	return rig.k2(""), per, nil
}

// checkK2ScaleGPUs: the same replica described as 1, 2, 4 or 8 GPUs learns
// the same k2 and prices the same per-replica capacity: both are per
// replica, whatever it spans.
func checkK2ScaleGPUs(seed uint64) error {
	r := propRand(seed)
	tok := int64(float64(propKv) * (0.85 + 0.1*r.Float64()))
	rows := func() []domain.ReplicaMetrics { return []domain.ReplicaMetrics{satRow("r0", tok, 10, 6)} }
	var k1, p1 float64
	for _, g := range []int{1, 2, 4, 8} {
		st := decodeStates(1)
		st[0].GPUsPerReplica = g
		k, p, err := k2Priced(rows, st)
		if err != nil {
			return err
		}
		if g == 1 {
			if !(k > 0 && p > 0) {
				return violation(kindNotLearned, "no k2 (%.0f) or capacity (%.0f) on 1 GPU", k, p)
			}
			k1, p1 = k, p
			continue
		}
		if relErr(k, k1) > 1e-9 || relErr(p, p1) > 1e-9 {
			return violation(kindMoved, "%d GPUs per replica: k2 %.0f and capacity %.0f, against %.0f and %.0f on 1", g, k, p, k1, p1)
		}
	}
	return nil
}

// checkK2ScaleOutput: a replica saturated at the same KV with outputs of 300
// or 380 tokens learns the same k2 and prices the same
// capacity: k2 is KV, not requests.
func checkK2ScaleOutput(seed uint64) error {
	r := propRand(seed)
	tok := int64(float64(propKv) * (0.85 + 0.1*r.Float64()))
	run := func(out float64) (float64, float64, error) {
		return k2Priced(func() []domain.ReplicaMetrics {
			rm := satRow("r0", tok, 10, 6)
			rm.AvgOutputTokens = out
			rm.GenerationTokenRate = rm.RequestRate * out
			return []domain.ReplicaMetrics{rm}
		}, decodeStates(1))
	}
	k3, p3, err := run(300)
	if err != nil {
		return err
	}
	if !(k3 > 0 && p3 > 0) {
		return violation(kindNotLearned, "no k2 at 300 tokens")
	}
	k6, p6, err := run(380)
	if err != nil {
		return err
	}
	return holds(relErr(k6, k3) <= 1e-9 && relErr(p6, p3) <= 1e-9, kindMoved,
		"output 300 -> 380 tokens: k2 %.0f -> %.0f, capacity %.0f -> %.0f", k3, k6, p3, p6)
}

// checkMuScaleOutput: a replica generating the same tokens a second at twice
// the output length completes half the requests: mu, in requests a second,
// scales by its unit.
func checkMuScaleOutput(seed uint64) error {
	r := propRand(seed)
	tokens := 2000 + 1000*r.Float64()
	rm := satRow("p0", 300_000, 10, tokens/500)
	rm.GenerationTokenRate = tokens
	mu1, ok1 := saturatedCompletionRate(rm, domain.RoleDecode, 500, 1000)
	rm.AvgOutputTokens, rm.RequestRate = 1000, tokens/1000
	mu2, ok2 := saturatedCompletionRate(rm, domain.RoleDecode, 1000, 1000)
	if !ok1 || !ok2 || !(mu1 > 0) {
		return violation(kindNotLearned, "no rate (%v %v)", ok1, ok2)
	}
	return holds(relErr(mu2, mu1/2) <= 1e-9, kindMoved, "%.0f tokens/s: mu %.4f at 500 tokens, %.4f at 1000, against %.4f", tokens, mu1, mu2, mu1/2)
}

// --- the time-base family ---

// checkMuFirstSampleTimeBase: a fresh replica's first saturated cycle, its
// completions a rate[1m] over a minute it served for only part of: the
// guard against a low first reading refuses it, or a second minute's
// reading moves it by no more than the band.
func checkMuFirstSampleTimeBase(seed uint64) error {
	r := propRand(seed)
	truth := 0.2 * (1 + 0.1*r.Float64())
	served := 20 + r.IntN(20) // seconds of the first minute it served
	path := make([]float64, 120)
	for s := range path {
		if s >= 60-served {
			path[s] = truth
		}
	}
	first := sampleTimeBase(path).rate1m[0]
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	if t.throughputRefusal(first, first, derivedMu{rate: truth, ok: true}, 0, false) != "" {
		return nil
	}
	return holds(relErr((first+truth)/2, first) <= propBand, kindAdoptedRaw,
		"a first rate[1m] of %.4f (served %d s of the minute) against %.4f was admitted", first, served, truth)
}

// checkITLMissedRowTimeBase is checkITLMissedRow on the time base: k the
// instant reading, ITL a rate[1m].
func checkITLMissedRowTimeBase(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		const n, secs = 3, 32 * 60
		lv := baseLevels(n)
		ks, lats := make([]timeBase, n), make([]timeBase, n)
		for p := range n {
			k, lat := make([]float64, secs), make([]float64, secs)
			noise := arNoise(r, secs, 0.01, 0.99)
			for s := range k {
				k[s] = lv[p] * (1 + noise[s])
				lat[s] = propITL(k[s])
			}
			ks[p], lats[p] = sampleTimeBase(k), sampleTimeBase(lat)
		}
		cycles := len(ks[0].instant)
		fleet := func(i int) []domain.ReplicaMetrics {
			rows := make([]domain.ReplicaMetrics, n)
			for p := range n {
				rows[p] = itlRow(fmt.Sprintf("r%d", p), ks[p].instant[i], lats[p].rate1m[i])
			}
			return rows
		}
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

// checkMuSlowPodTimeBase: four pods, one at half the others' rate, each a
// rate[1m] of Poisson completions at about 10 a second: mu is the mean of the
// pods' rates within 5 %.
func checkMuSlowPodTimeBase(newL func() muLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		level := 8 + 4*r.Float64()
		rates := []float64{level, level, level, level / 2}
		minutes := make([][]float64, 3)
		for m := range minutes {
			minutes[m] = make([]float64, len(rates))
			for p, lam := range rates {
				c := 0.0
				for range 60 {
					c += poisson(r, lam)
				}
				minutes[m][p] = c / 60
			}
		}
		got := muFigure(newL(), minutes)
		want := (3*level + level/2) / 4
		if !(got > 0) {
			return violation(kindNotLearned, "no mu")
		}
		return holds(relErr(got, want) <= 0.05, kindBiased, "one pod at half among four, on the time base: mu %.3f against the mean %.3f", got, want)
	}
}

// checkK2ScaleTimeBase: a replica whose KV reaches the analyzer as the
// max_over_time[1m] peak of a wandering path learns the same k2 described in
// 16- or 32-token blocks.
func checkK2ScaleTimeBase(seed uint64) error {
	r := propRand(seed)
	noise := arNoise(r, 6*60, 0.02, 0.98)
	path := make([]float64, len(noise))
	for s, e := range noise {
		path[s] = 0.9 * (1 + e)
	}
	tb := sampleTimeBase(path)
	run := func(block int64) (float64, error) {
		rig := newPropRig()
		for _, k := range tb.peak1m {
			rm := satRow("r0", int64(math.Min(k, 0.99)*float64(propKv)), 10, 6)
			rm.BlockSize, rm.NumGpuBlocks = block, propKv/block
			if _, err := rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 6, propCycle); err != nil {
				return 0, err
			}
		}
		return rig.k2(""), nil
	}
	k16, err := run(16)
	if err != nil {
		return err
	}
	if !(k16 > 0) {
		return violation(kindNotLearned, "no k2 on the time base")
	}
	k32, err := run(32)
	if err != nil {
		return err
	}
	return holds(relErr(k32, k16) <= 1e-9, kindMoved, "block 16 -> 32 on the time base: k2 %.0f -> %.0f", k16, k32)
}

// checkK2LivenessTimeBase: two replicas whose KV is the one-minute peak of a
// path wandering 2 % around saturation learn k2 within 3 min, within the
// band of the peaks they report.
func checkK2LivenessTimeBase(seed uint64) error {
	r := propRand(seed)
	level := 0.86 + 0.06*r.Float64()
	tbs := make([]timeBase, 2)
	for p := range tbs {
		noise := arNoise(r, 5*60, 0.02, 0.98)
		path := make([]float64, len(noise))
		for s, e := range noise {
			path[s] = math.Min(0.99, level*(1+e))
		}
		tbs[p] = sampleTimeBase(path)
	}
	rig := newPropRig()
	sum, n := 0.0, 0
	for i := range min(13, len(tbs[0].peak1m)) {
		rows := make([]domain.ReplicaMetrics, 2)
		for p := range rows {
			rows[p] = satRow(fmt.Sprintf("r%d", p), int64(tbs[p].peak1m[i]*float64(propKv)), 10, 6)
			sum += tbs[p].peak1m[i] * float64(propKv)
			n++
		}
		if _, err := rig.analyze(rows, decodeStates(2), 12, propCycle); err != nil {
			return err
		}
	}
	got, want := rig.k2(""), sum/float64(n)
	if !(got > 0) {
		return violation(kindNotLearned, "no k2 on the time base in 3 min")
	}
	return holds(relErr(got, want) <= propBand, kindBiased, "k2 %.0f against the peaks' mean %.0f", got, want)
}

// checkFloorMonotoneTimeBase is checkFloorMonotone with lambda a rate[1m] of
// per-second arrivals rising from 2 to 6 a second.
func checkFloorMonotoneTimeBase(seed uint64) error {
	r := propRand(seed)
	secs := 60 + 40*int(propCycle/time.Second)
	arrivals := make([]float64, secs)
	for s := range arrivals {
		arrivals[s] = poisson(r, 2+4*float64(s)/float64(secs))
	}
	lambdas := sampleTimeBase(arrivals).rate1m
	rig, err := floorRig()
	if err != nil {
		return err
	}
	prev, queue := 0.0, 0.0
	for i, lambda := range lambdas {
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
		if math.IsNaN(got) {
			return violation(kindNotLearned, "cycle %d: no order", i)
		}
		// A rate[1m] dips with its Poisson noise; the order may follow a dip
		// in lambda down, never fall while lambda and the queue rise.
		if got < prev*(1-0.02) && lambda >= lambdas[max(i-1, 0)] {
			return violation(kindFell, "cycle %d: lambda %.2f (rising), queue %.0f: the order fell %.2f -> %.2f", i, lambda, queue, prev, got)
		}
		prev = got
	}
	return nil
}

// --- the prefill bound through Analyze ---

// checkBoundAnalyze: a P/D model with the bound on, decode clear, four
// prefill replicas saturated with one computing factor times the others'
// tokens. The bound the analyzer learns is the mean of the replicas' rates,
// the declared estimand: factor 1 is liveness, 2 the double scrape seen
// through a whole Analyze.
func checkBoundAnalyze(factor float64) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		rig := newPropRig()
		rig.policy = func(p *config.ScalingPolicy) { p.EnablePrefillThroughputBound = true }
		states := []domain.VariantReplicaState{decodeStates(1)[0],
			{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1, Role: domain.RolePrefill}}
		level := 3000 + 1000*r.Float64()
		for range 13 {
			rows := make([]domain.ReplicaMetrics, 0, 5)
			rows = append(rows, fullDecode("d0", 0.3, 0))
			for p := range 4 {
				rm := satRow(fmt.Sprintf("p%d", p), int64(float64(propKv)*0.9), 10, 3)
				rm.VariantName = "p"
				rm.PrefillComputedTokenRate = level
				if p == 3 {
					rm.PrefillComputedTokenRate = level * factor
				}
				rows = append(rows, rm)
			}
			if _, err := rig.analyze(rows, states, 6, propCycle); err != nil {
				return err
			}
		}
		got := rig.boundTokens()
		if !(got > 0) {
			return violation(kindNotLearned, "no prefill bound after 3 min")
		}
		want := level * (3 + factor) / 4
		return holds(relErr(got, want) <= propTruthTol, kindBiased, "one replica at %.0fx: bound %.0f tokens/s against the mean %.0f", factor, got, want)
	}
}

// boundTokens is the prefill bound window's max, in computed tokens/s, 0
// when it has none.
func (r *propRig) boundTokens() float64 {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for k, ra := range r.a.saturatedThroughput {
		if strings.Contains(k, "|"+prefillBoundRole+"|") && ra.Len() > 0 {
			return ra.Max()
		}
	}
	return 0
}
