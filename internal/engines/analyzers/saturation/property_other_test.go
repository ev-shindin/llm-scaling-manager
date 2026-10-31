package saturation

import (
	"sort"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Property cases for the prefill bound (row 7), idle room (row 10) and
// replica start time (row 12).

func otherCases() []propCase {
	bound := func() muLearner { return newBoundMu(nil) }
	return []propCase{
		{class: classHeterogeneity, learner: "prefill-bound", name: "one-pod-doubled", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuDoubled(bound)},
		{class: classHeterogeneity, learner: "prefill-bound", name: "one-slow-pod-n4", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkMuOneSlowPod(bound, 4)},
		{class: classNDependence, learner: "prefill-bound", name: "iid-max", family: familyDirect, tuning: tunedDefault, seeds: 10, check: checkMuNDependence(bound, false)},
		{class: classEdges, learner: "idle", name: "idle-line-030", family: familyTimeBase, tuning: tunedDefault, seeds: cheapSeeds, check: checkIdleEdge(realIdleVerdict)},
		{class: classFirstSample, learner: "start-time", name: "between-sample-and-default", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkStartFirstSample},
		{class: classStale, learner: "start-time", name: "reaches-measured", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkStartConverges},
		{class: classEdges, learner: "start-time", name: "outlier-3x", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkStartOutlier},
	}
}

// boundMu is the prefill bound end to end: each reading goes through
// notePrefillBound, a saturated prefill row of the named pod, and the figure
// is the bound notePrefillBound last priced -- its window's max over the
// effective prompt length.
type boundMu struct {
	*analyzerMu
	cfg  *config.ScalingPolicy
	edit func(*domain.ReplicaMetrics)
	last float64
}

const (
	propBoundKey   = "test-ns|v|test-model|H200|1|prefill|ihuge|noout|q5"
	propBoundIleff = 1000.0
)

// newBoundMu is the bound with edit, when given, applied to every row before
// notePrefillBound reads it (a mutant that reintroduces an input defect).
func newBoundMu(edit func(*domain.ReplicaMetrics)) *boundMu {
	return &boundMu{analyzerMu: newAnalyzerMu(), edit: edit,
		cfg: &config.ScalingPolicy{EnablePrefillThroughputBound: true, QueueLengthThreshold: 5}}
}

// read is one saturated prefill row of pod computing rate requests/s at
// propBoundIleff tokens each.
func (m *boundMu) read(pod string, rate float64) {
	rm := makeReplicaMetrics(pod, "v", 100_000, propKv, 10, propBoundIleff, 1)
	rm.PrefillComputedTokenRate = rate * propBoundIleff
	if m.edit != nil {
		m.edit(&rm)
	}
	m.last = m.a.notePrefillBound(rm, m.cfg, domain.RolePrefill, propBoundKey, "", propBoundIleff, admission{}, logr.Discard()).rate
	m.clock = m.clock.Add(time.Second)
}

func (m *boundMu) figure() float64 { return m.last }

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
// line, the per-second path wandering with a 30 s correlation time. The
// noise-free path never crosses, so the idle-room verdict changes at most
// once.
func checkIdleEdge(verdict idleVerdict) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		path := arNoise(r, 30*60, 0.10, 0.967)
		for i := range path {
			path[i] = 1 + path[i]
		}
		tb := sampleTimeBase(path)
		med := append([]float64(nil), tb.peak1m...)
		sort.Float64s(med)
		a := NewSaturationAnalyzer(capacity.NewStore())
		v := make([]bool, len(tb.peak1m))
		for i, k := range tb.peak1m {
			v[i] = verdict(a, k*0.30/med[len(med)/2], propStart.Add(time.Duration(i)*propCycle))
		}
		return holds(reversals(v) <= 1, kindFlipped, "peak KV on the 0.30 line: the idle verdict reversed %d times in %d cycles", reversals(v), len(v))
	}
}

// startRig notes each cycle's pods' start samples on one analyzer.
type startRig struct {
	a    *SaturationAnalyzer
	key  string
	pods []domain.ReplicaMetrics
}

func newStartRig() *startRig {
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.now = func() time.Time { return propStart }
	return &startRig{a: a, key: "start"}
}

// add brings up one more pod with its start sample, and returns the estimate
// (DefaultReplicaStartSeconds when none is measured).
func (s *startRig) add(sample float64) float64 {
	rm := satRow("p"+string(rune('a'+len(s.pods))), 100_000, 0, 1)
	rm.StartSeconds = sample
	s.pods = append(s.pods, rm)
	s.a.noteReplicaStart(s.key, "test-ns", "v", s.pods, logr.Discard())
	if est, ok := s.a.startSeconds[s.key]; ok && est > 0 {
		return est
	}
	return DefaultReplicaStartSeconds
}

// checkStartFirstSample is PR 5's acceptance for a variant with no seed: the
// first sample moves the estimate strictly between itself and the 70 s
// default, never all the way to either.
func checkStartFirstSample(seed uint64) error {
	r := propRand(seed)
	s := []float64{15, 460}[r.IntN(2)] * (1 + 0.1*r.Float64())
	est := newStartRig().add(s)
	lo, hi := min(s, DefaultReplicaStartSeconds), max(s, DefaultReplicaStartSeconds)
	return holds(est > lo && est < hi, kindAdoptedRaw, "a first sample of %.0f s set the estimate to %.1f s: not strictly between it and the %.0f s default", s, est, DefaultReplicaStartSeconds)
}

// checkStartConverges: after a first pod 50 % off, ten pods agreeing on the
// true start bring the estimate within the band of it.
func checkStartConverges(seed uint64) error {
	r := propRand(seed)
	truth := 40 + 120*r.Float64()
	s := newStartRig()
	s.add(truth * 1.5)
	var est float64
	for range 10 {
		est = s.add(truth * (1 + 0.02*r.NormFloat64()))
	}
	return holds(relErr(est, truth) <= propBand, kindStale, "after ten pods at %.0f s the estimate is %.1f s", truth, est)
}

// checkStartOutlier: one pod reporting over 3x the estimate -- the Ready
// condition re-transitioning, so its age -- does not move the estimate.
func checkStartOutlier(seed uint64) error {
	r := propRand(seed)
	truth := 40 + 120*r.Float64()
	s := newStartRig()
	var est float64
	for range 5 {
		est = s.add(truth)
	}
	got := s.add(truth * (3.5 + r.Float64()))
	return holds(relErr(got, est) <= 1e-9, kindMoved, "a %.0f s outlier moved the estimate %.1f -> %.1f s", truth*3.5, est, got)
}
