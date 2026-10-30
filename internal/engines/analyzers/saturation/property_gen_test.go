package saturation

import (
	"context"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// Generators and learner adapters for the property tests. Every generator
// takes a seeded *rand.Rand, so a seed reproduces a case.

// propCycle is the controller's cycle: inputs are sampled at it.
const propCycle = 15 * time.Second

// propStart is the synthetic clock's origin.
var propStart = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func propRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, 0x0b3)) }

// arNoise is n relative deviations of an AR(1) process with standard
// deviation sigma and lag-one correlation rho: the gauges' noise is
// autocorrelated (one-minute windows share 45 s), and iid noise over-counts
// flips (3.5).
func arNoise(r *rand.Rand, n int, sigma, rho float64) []float64 {
	out := make([]float64, n)
	innov := sigma * math.Sqrt(1-rho*rho)
	x := r.NormFloat64() * sigma
	for i := range out {
		out[i] = x
		x = rho*x + innov*r.NormFloat64()
	}
	return out
}

// timeBase is the time-base input family: a per-second path, sampled at a
// propCycle cycle as Prometheus serves it to the collector.
type timeBase struct {
	// instant is the path at the cycle's instant; rate1m its mean over the
	// minute before (rate[1m]); peak1m its max over that minute
	// (max_over_time[1m]). Consecutive cycles' minutes share 45 s.
	instant, rate1m, peak1m []float64
}

// sampleTimeBase samples path, one value per second over seconds, at every
// propCycle from the first full minute.
func sampleTimeBase(path []float64) timeBase {
	var tb timeBase
	step := int(propCycle / time.Second)
	for t := 60; t < len(path); t += step {
		sum, peak := 0.0, math.Inf(-1)
		for s := t - 59; s <= t; s++ {
			sum += path[s]
			peak = math.Max(peak, path[s])
		}
		tb.instant = append(tb.instant, path[t])
		tb.rate1m = append(tb.rate1m, sum/60)
		tb.peak1m = append(tb.peak1m, peak)
	}
	return tb
}

// --- ITL ---

// The true ITL line every ITL case is generated from: ITL(k) = A k + B, with
// B away from DefaultBaselineSec so a B stuck at the default shows.
const (
	propTrueA = 0.050
	propTrueB = 0.010
)

func propITL(k float64) float64 { return propTrueA*k + propTrueB }

// itlLearner is the ITL line's learner, as the saturation analyzer drives it:
// one Observe per cycle over the variant's rows.
type itlLearner interface {
	observe(rows []domain.ReplicaMetrics, now time.Time) itl.Reading
	baseline() (float64, bool)
}

const propITLKey = "ns|v|model|H200|1|decode|medium"

// trackerITL is the production learner, itl.Tracker, under an admission.
type trackerITL struct {
	tr  *itl.Tracker
	adm itl.Admission
}

func newTrackerITL(s itl.Steadiness) *trackerITL {
	return &trackerITL{tr: itl.NewTracker(), adm: itl.Admission{Steady: s}}
}

func (l *trackerITL) observe(rows []domain.ReplicaMetrics, now time.Time) itl.Reading {
	return l.tr.Observe(propITLKey, "v", rows, l.adm, now)
}

func (l *trackerITL) baseline() (float64, bool) { return l.tr.Baseline(propITLKey) }

// itlRow is one Ready replica of variant v at instant KV k reading itlSec.
func itlRow(pod string, k, itlSec float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: "v", Ready: true,
		KvUsageInstant: k, KvUsageInstantKnown: true, KvCacheUsage: k,
		AvgITL: itlSec, AvgITLKnown: true,
	}
}

// steadinessOf is the ITL steady gate a policy tuning gives; tuningDefault and
// tuningNonDefault are the defaults and the non-default policy 2.6 names
// (steadyFor 10m, idleRows 5;
// carry raised with steadyFor, as the policy's crossed-pair repair requires).
func steadinessOf(tn config.AnalyzerTuning) itl.Steadiness {
	return tuning{AnalyzerTuning: tn.WithDefaults()}.steadiness()
}

var (
	tuningDefault    = config.AnalyzerTuning{}
	tuningNonDefault = config.AnalyzerTuning{SteadyGate: config.SteadyGateTuning{
		SteadyFor: config.PolicyDuration(10 * time.Minute),
		Carry:     config.PolicyDuration(15 * time.Minute),
		IdleRows:  5,
	}}
)

// fitLearner is the pinned fit alone: the estimator over the readings the
// tracker hands it.
type fitLearner func(obs []itl.Observation, b float64) (itl.Model, bool)

// --- mu ---

// muLearner is measured mu's write path: readings from pods, a minute apart
// or within one, and the figure on record.
type muLearner interface {
	read(pod string, rate float64)
	// next moves the clock past the one-minute sample spacing.
	next()
	figure() float64
}

const propMuKey = "test-ns|test-model|H200|1|decode|medium|q5"

// analyzerMu is the production write path: recordSaturatedThroughputFrom and
// the figure saturatedThroughputFor prices from.
type analyzerMu struct {
	a     *SaturationAnalyzer
	clock time.Time
}

func newAnalyzerMu() *analyzerMu {
	m := &analyzerMu{a: NewSaturationAnalyzer(capacity.NewStore()), clock: propStart}
	m.a.now = func() time.Time { return m.clock }
	return m
}

func (m *analyzerMu) read(pod string, rate float64) {
	m.a.recordSaturatedThroughputFrom(propMuKey, pod, rate)
	m.clock = m.clock.Add(time.Second)
}
func (m *analyzerMu) next() { m.clock = m.clock.Add(ThroughputSampleSpacing) }
func (m *analyzerMu) figure() float64 {
	v, _ := m.a.saturatedThroughputFor(propMuKey)
	return v
}

// --- the saturation analyzer, end to end ---

// propRig drives one SaturationAnalyzer cycle by cycle on its own clock.
// policy edits the scaling policy every cycle (the analyzerTuning a case runs
// under, or a mutant's), and wrap edits each row before Analyze sees it (a
// mutant that reintroduces an input defect).
type propRig struct {
	a      *SaturationAnalyzer
	clock  time.Time
	policy func(*config.ScalingPolicy)
	wrap   func(*domain.ReplicaMetrics)
	// ctx carries the logger Analyze writes to; context.Background when nil.
	ctx context.Context
	// input edits the AnalyzerInput last (a scheduler queue, say).
	input func(*domain.AnalyzerInput)
}

func newPropRig() *propRig {
	r := &propRig{a: NewSaturationAnalyzer(capacity.NewStore()), clock: propStart}
	r.a.now = func() time.Time { return r.clock }
	return r
}

// analyze runs one cycle at the rig's clock and then advances it by step.
func (r *propRig) analyze(rows []domain.ReplicaMetrics, states []domain.VariantReplicaState,
	lambda float64, step time.Duration) (*domain.AnalyzerResult, error) {
	rows = append([]domain.ReplicaMetrics(nil), rows...)
	if r.wrap != nil {
		for i := range rows {
			r.wrap(&rows[i])
		}
	}
	in := makeAnalyzerInput(rows, states)
	in.ArrivalRate = lambda
	if r.policy != nil {
		r.policy(in.Config.(*config.ScalingPolicy))
	}
	if r.input != nil {
		r.input(&in)
	}
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := r.a.Analyze(ctx, in)
	r.clock = r.clock.Add(step)
	return res, err
}

// k2 is the mean of the k2 windows holding a reading, for role (every role
// when empty): the k2 learner's figure.
func (r *propRig) k2(role string) float64 {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	keys := make([]string, 0, len(r.a.computeCapacityHistory))
	for k, ra := range r.a.computeCapacityHistory {
		if ra.Len() > 0 && (role == "" || strings.Contains(k, "|"+role+"|")) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return 0
	}
	sort.Strings(keys)
	sum := 0.0
	for _, k := range keys {
		sum += r.a.computeCapacityHistory[k].Average()
	}
	return sum / float64(len(keys))
}

// propKv is one replica's KV cache in tokens.
const propKv = int64(376_480)

// satRow is one decode replica of variant v holding tokens with queue
// waiting, completing reqRate requests a second at 1000 in / 500 out.
func satRow(pod string, tokens int64, queue int, reqRate float64) domain.ReplicaMetrics {
	rm := makeReplicaMetrics(pod, "v", tokens, propKv, queue, 1000, 500)
	rm.KvUsageInstant, rm.KvUsageInstantKnown = rm.KvCacheUsage, true
	rm.RequestRate = reqRate
	rm.GenerationTokenRate = reqRate * 500
	rm.PrefillComputedTokenRate = reqRate * 1000
	return rm
}

func decodeStates(n int) []domain.VariantReplicaState {
	return []domain.VariantReplicaState{
		{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: n, GPUsPerReplica: 1, Role: domain.RoleDecode},
	}
}

func aggStates(n int) []domain.VariantReplicaState {
	return []domain.VariantReplicaState{
		{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: n, GPUsPerReplica: 1},
	}
}

// relErr is |got - want| / |want|, and +Inf when either is NaN, so a NaN never
// passes a tolerance.
func relErr(got, want float64) float64 {
	if math.IsNaN(got) || math.IsNaN(want) {
		return math.Inf(1)
	}
	if want == 0 {
		return math.Abs(got)
	}
	return math.Abs(got-want) / math.Abs(want)
}

// reversals counts how often a boolean verdict changed along a trajectory.
func reversals(v []bool) int {
	n := 0
	for i := 1; i < len(v); i++ {
		if v[i] != v[i-1] {
			n++
		}
	}
	return n
}

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
	// propNDepBias is the declared bias of an estimate's expectation, against
	// the truth, across N in {1, 2, 4, 16}.
	propNDepBias = 0.03
	// propKPrice is where the ITL line is priced (itl.DefaultKSat).
	propKPrice = itl.DefaultKSat
)

// propertyCases is every property case, real learners only. Mutants reuse the
// check functions with a learner that carries a past defect.
func propertyCases() []propCase {
	out := make([]propCase, 0, 192)
	for _, part := range [][]propCase{itlCases(), k2Cases(), muCases(), floorCases(), otherCases(),
		learnerCases(), edgeCases(), moreCases()} {
		out = append(out, part...)
	}
	return out
}
