package saturation

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// Property cases for the learners #389 left without one: the shared and
// borrowed ITL line (inventory row 2), the borrowed k2 lower bound (row 4),
// the fleet shape and its hold (row 8), the decode-saturation memory (row 9),
// and the SLO analyzer's line through slo.Analyzer (row 13), so its
// admission wiring is tested and not only the itl.Tracker it shares.

func learnerCases() []propCase {
	sloDef := func() itlLearner { return newSLOITL(nil) }
	sloNon := func() itlLearner { return newSLOITL(&tuningNonDefault) }
	return []propCase{
		// Row 2.
		{class: classLiveness, learner: "shared-line", name: "sibling-borrows", family: familyDirect, tuning: tunedPolicy,
			policy: reusePolicy, check: checkSharedLineLiveness},
		{class: classKnownTruth, learner: "shared-line", name: "borrowed-is-the-lenders", family: familyDirect, tuning: tunedPolicy,
			policy: reusePolicy, check: checkSharedLineTruth},
		{class: classEdges, learner: "shared-line", name: "reuse-switch", family: familyDirect, tuning: tunedPolicy,
			policy: reusePolicy, seeds: 5, check: checkSharedLineSwitch},
		{class: classStale, learner: "shared-line", name: "held-slope-not-republished", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkSharedLineHeldNotRepublished},
		// Row 4.
		{class: classKnownTruth, learner: "borrowed-k2", name: "lower-only", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkBorrowLowerOnly},
		{class: classMonotonicity, learner: "borrowed-k2", name: "relaxes-upward", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkBorrowRelaxesUpward},
		{class: classMissing, learner: "borrowed-k2", name: "pause-is-not-serving", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkBorrowPause},
		{class: classLiveness, learner: "borrowed-k2", name: "expires-to-own", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkBorrowExpires},
		{class: classRedistribution, learner: "borrowed-k2", name: "disable-borrow-keeps-own", family: familyDirect, tuning: tunedPolicy,
			policy: borrowOffPolicy, seeds: cheapSeeds, check: checkBorrowDisabled},
		// Row 8.
		{class: classFirstSample, learner: "shape-hold", name: "first-shape-no-change", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkShapeFirst},
		{class: classMissing, learner: "shape-hold", name: "missing-axis-carries", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkShapeMissingAxis},
		{class: classTransient, learner: "shape-hold", name: "wobble-in-band", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkShapeWobble},
		{class: classStale, learner: "shape-hold", name: "hold-released", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkShapeHoldReleased(&config.ScalingPolicy{})},
		{class: classStale, learner: "shape-hold", name: "hold-released-nondefault", family: familyDirect, tuning: tunedPolicy,
			policy: holdLongPolicy, seeds: cheapSeeds, check: checkShapeHoldReleased(holdLongPolicy)},
		{class: classEdges, learner: "shape-hold", name: "hold-disabled", family: familyDirect, tuning: tunedPolicy,
			policy: holdOffPolicy, seeds: cheapSeeds, check: checkShapeHoldDisabled},
		// Row 9.
		{class: classRedistribution, learner: "decode-memory", name: "row-order", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkDecodeMemoryOrder},
		{class: classMissing, learner: "decode-memory", name: "unready-or-unsized-row", family: familyDirect, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkDecodeMemoryMissing},
		{class: classTransient, learner: "decode-memory", name: "prefill-gated-behind-full-decode", family: familyDirect, tuning: tunedDefault,
			check: checkDecodeMemoryGatesPrefill},
		// Row 13, through slo.Analyzer.
		{class: classTransient, learner: "slo-itl", name: "joiner-warmup", family: familyDirect, tuning: tunedDefault,
			check: checkITLJoiner(sloDef, 1, 20*time.Minute)},
		{class: classTransient, learner: "slo-itl", name: "joiner-warmup-nondefault", family: familyDirect, tuning: tunedNonDefault,
			policy: tuningPolicy(tuningNonDefault),
			check:  checkITLJoiner(sloNon, 1, 30*time.Minute)},
		{class: classFirstSample, learner: "slo-itl", name: "one-reading", family: familyDirect, tuning: tunedDefault,
			check: checkITLOneReading(sloDef)},
		{class: classStale, learner: "slo-itl", name: "b-converges-n2", family: familyDirect, tuning: tunedDefault,
			check: checkITLStaleB(sloDef, 2, 10*time.Minute)},
		{class: classMissing, learner: "slo-itl", name: "missed-row", family: familyDirect, tuning: tunedDefault,
			check: checkITLMissedRow(sloDef)},
		{class: classKnownTruth, learner: "slo-itl", name: "line-at-kprice", family: familyDirect, tuning: tunedDefault,
			check: checkITLKnownTruth(sloDef)},
	}
}

// --- the SLO analyzer's line ---

// sloITL drives slo.Analyzer one cycle per observe and reads the line it
// fitted from its slo-variant log line: the analyzer's whole admission path
// (the policy's band and steady gate, mapped by config), not the shared
// tracker alone.
type sloITL struct {
	a      *slo.Analyzer
	clock  *clocktesting.FakePassiveClock
	logs   *observer.ObservedLogs
	ctx    context.Context
	policy *config.ScalingPolicy
	b      float64
	haveB  bool
}

// newSLOITL is the SLO analyzer under the given analyzerTuning (the
// defaults when nil), with a TPOT target so it prices, and logs, its line.
func newSLOITL(tn *config.AnalyzerTuning) *sloITL {
	core, logs := observer.New(zapcore.Level(-3))
	l := &sloITL{
		clock: clocktesting.NewFakePassiveClock(propStart),
		logs:  logs,
		ctx:   logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core))),
		policy: &config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
			SLO: &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 40 * time.Millisecond}}}},
	}
	if tn != nil {
		l.policy.AnalyzerTuning = *tn
	}
	l.a = slo.New(nil, capacity.NewStore()).WithClock(l.clock)
	return l
}

func (l *sloITL) observe(rows []domain.ReplicaMetrics, now time.Time) itl.Reading {
	l.clock.SetTime(now)
	in := domain.AnalyzerInput{ModelID: "m", Namespace: "ns", Config: l.policy,
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200",
			CurrentReplicas: len(rows), GPUsPerReplica: 1, Role: domain.RoleDecode}}}
	for _, rm := range rows {
		rm.AvgOutputTokens, rm.TotalKvCapacityTokens = 500, propKv
		in.ReplicaMetrics = append(in.ReplicaMetrics, rm)
	}
	if _, err := l.a.Analyze(l.ctx, in); err != nil {
		return itl.Reading{}
	}
	var r itl.Reading
	for _, e := range l.logs.TakeAll() {
		if e.Message != "slo-variant" {
			continue
		}
		m := e.ContextMap()
		a, _ := m["lineA"].(float64)
		b, _ := m["lineB"].(float64)
		tier, _ := m["lineTier"].(string)
		source, _ := m["source"].(string)
		r = itl.Reading{Model: itl.Model{A: a, B: b}, Tier: tier}
		if tier == "" {
			r.Model = itl.Model{}
		}
		// A line from a full fit, or B pinned to this key's own full fit,
		// carries the learned intercept.
		if source == "own" || source == "pinned" {
			l.b, l.haveB = b, true
		}
	}
	return r
}

func (l *sloITL) baseline() (float64, bool) { return l.b, l.haveB }

// --- row 2: the shared and borrowed line ---

// The policies the row 2, 4 and 8 cases run, each read by its check: the
// keys a case runs at a non-default value are the keys these set.
var (
	reusePolicy     = &config.ScalingPolicy{EnableLearnedStateReuse: true}
	borrowOffPolicy = &config.ScalingPolicy{DisableK2Borrow: true}
	holdLongPolicy  = &config.ScalingPolicy{ShapeChangeHoldSeconds: 600}
	holdOffPolicy   = &config.ScalingPolicy{DisableShapeChangeHold: true}
)

// Policy keys (non-default values a case runs; property_policy_test.go).
const (
	keyReuse            = "enableLearnedStateReuse"
	keyDisableK2Borrow  = "disableK2Borrow"
	keyShapeHoldSeconds = "shapeChangeHoldSeconds"
	keyDisableShapeHold = "disableShapeChangeHold"
)

// sharedRig is two decode variants on one readable engine configuration: a
// lender of ten replicas across the band, and a starved variant whose
// replicas sit below it, so it has nothing of its own to fit.
type sharedRig struct {
	a     *SaturationAnalyzer
	clock time.Time
	// below puts the lender's replicas under the ITL band: its window then
	// takes no reading, ages out, and its line is held.
	below bool
	// lenderHeld is whether the lender's line was a held slope last cycle.
	lenderHeld bool
}

func sharedParams() *capacity.EngineParams {
	return &capacity.EngineParams{Engine: "vllm", GpuMemoryUtilization: 0.9, BlockSize: 16, KvCacheDtype: "auto",
		WeightDtype: "auto", TensorParallelSize: 1, MaxNumSeqs: 256, EffectiveMaxBatchedTokens: 8192}
}

func newSharedRig() *sharedRig {
	store := capacity.NewStore()
	for _, v := range []string{"lender", "starved"} {
		store.Update("test-ns", "test-model", v, capacity.Record{AcceleratorName: "H100", GpuCount: 1, EngineParams: sharedParams()}, propStart)
	}
	r := &sharedRig{a: NewSaturationAnalyzer(store), clock: propStart}
	r.a.now = func() time.Time { return r.clock }
	return r
}

// sharedB is the intercept of the lender's line.
const sharedB = 0.008

// cycle runs one Analyze of the two variants with the lender's line
// ITL(k) = a k + sharedB, and returns the starved variant's line, its lender,
// and the lender's own line.
func (r *sharedRig) cycle(reuse bool, a float64, jitter func() float64) (itl.Model, string, itl.Model, error) {
	states := []domain.VariantReplicaState{
		{VariantName: "lender", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 10, GPUsPerReplica: 1},
		{VariantName: "starved", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	rows := make([]domain.ReplicaMetrics, 0, 11)
	for i := range 10 {
		k := 0.20 + 0.05*float64(i)
		if r.below {
			k = 0.05 + 0.005*float64(i)
		}
		rm := makeReplicaMetrics(fmt.Sprintf("lender-%d", i), "lender", 5000, 16000, 0, 6000, 1000)
		rm.KvUsageInstant, rm.KvUsageInstantKnown = k, true
		rm.AvgITL, rm.AvgITLKnown = (a*k+sharedB)*(1+jitter()), true
		rows = append(rows, rm)
	}
	s := makeReplicaMetrics("starved-0", "starved", 1000, 16000, 0, 6000, 1000)
	s.KvUsageInstant, s.KvUsageInstantKnown = 0.05, true
	rows = append(rows, s)
	in := makeAnalyzerInput(rows, states)
	in.Config.(*config.ScalingPolicy).EnableLearnedStateReuse = reuse
	if _, err := r.a.Analyze(context.Background(), in); err != nil {
		return itl.Model{}, "", itl.Model{}, err
	}
	c := r.a.newCycle(context.Background(), in, in.Config.(*config.ScalingPolicy))
	c.observeFleetShape()
	c.resolvePricing()
	c.fitLines()
	r.lenderHeld = c.itlHeld["lender"]
	r.clock = r.clock.Add(propCycle)
	return c.itlModels["starved"], c.itlBorrowedFrom["starved"], c.itlModels["lender"], nil
}

// checkSharedLineLiveness: with reuse on, a variant with nothing of its own
// to fit is priced on its sibling's line within 3 min.
func checkSharedLineLiveness(seed uint64) error {
	r := propRand(seed)
	rig := newSharedRig()
	for range 13 {
		got, from, _, err := rig.cycle(reusePolicy.EnableLearnedStateReuse, 0.04+0.02*r.Float64(), func() float64 { return 0.005 * r.NormFloat64() })
		if err != nil {
			return err
		}
		if from != "" && !got.IsZero() {
			return nil
		}
	}
	return violation(kindNotLearned, "after 3 min the starved variant has no borrowed line")
}

// checkSharedLineTruth: the borrowed line is the lender's own line, the
// figure it was fitted to, not a re-fit or a blend.
func checkSharedLineTruth(seed uint64) error {
	r := propRand(seed)
	rig := newSharedRig()
	a := 0.04 + 0.02*r.Float64()
	for range 13 {
		got, from, own, err := rig.cycle(reusePolicy.EnableLearnedStateReuse, a, func() float64 { return 0.005 * r.NormFloat64() })
		if err != nil {
			return err
		}
		if from == "" || got.IsZero() {
			continue
		}
		return holds(got == own, kindBiased, "borrowed A %.5f B %.5f against the lender's A %.5f B %.5f", got.A, got.B, own.A, own.B)
	}
	return violation(kindNotLearned, "no borrowed line in 3 min")
}

// checkSharedLineSwitch: the reuse switch is an edge. Off (the default) the
// starved variant never borrows; on, it does.
func checkSharedLineSwitch(seed uint64) error {
	r := propRand(seed)
	for _, reuse := range []bool{false, reusePolicy.EnableLearnedStateReuse} {
		rig := newSharedRig()
		borrowed := false
		for range 13 {
			_, from, _, err := rig.cycle(reuse, 0.05, func() float64 { return 0.005 * r.NormFloat64() })
			if err != nil {
				return err
			}
			borrowed = borrowed || from != ""
		}
		if borrowed != reuse {
			return violation(kindMoved, "enableLearnedStateReuse %v: borrowed %v", reuse, borrowed)
		}
	}
	return nil
}

// checkSharedLineHeldNotRepublished: a lender whose replicas fall under the
// band keeps pricing on its held slope, and the held slope is never
// published: the line on record for the configuration stays the last one
// fitted, stamped when it was fitted, through every cycle it is held.
func checkSharedLineHeldNotRepublished(seed uint64) error {
	r := propRand(seed)
	rig := newSharedRig()
	jitter := func() float64 { return 0.005 * r.NormFloat64() }
	a := 0.04 + 0.02*r.Float64()
	for range 13 {
		if _, _, _, err := rig.cycle(reusePolicy.EnableLearnedStateReuse, a, jitter); err != nil {
			return err
		}
	}
	published := func() (learnedLine, bool) {
		rig.a.mu.Lock()
		defer rig.a.mu.Unlock()
		for _, l := range rig.a.itlLines {
			if strings.HasPrefix(l.fromVariant, "test-ns|test-model|lender|") {
				return l, true
			}
		}
		return learnedLine{}, false
	}
	if _, ok := published(); !ok {
		return violation(kindNotLearned, "the lender published no line in 3 min")
	}
	rig.below = true
	held := 0
	var fitted learnedLine
	for range 140 { // 35 min: the window's 30 min age-out, and held cycles after it
		at := rig.clock
		if _, _, _, err := rig.cycle(reusePolicy.EnableLearnedStateReuse, a, jitter); err != nil {
			return err
		}
		if !rig.lenderHeld {
			continue
		}
		now, _ := published()
		if held == 0 {
			fitted = now
			if !fitted.learnedAt.Before(at) {
				return violation(kindStale, "the first held cycle stamped the line on record at %s", fitted.learnedAt.Format(time.TimeOnly))
			}
		}
		held++
		if now.model != fitted.model || !now.learnedAt.Equal(fitted.learnedAt) {
			return violation(kindStale, "held cycle %d: the line on record moved to A %.5f at %s, from A %.5f at %s", held,
				now.model.A, now.learnedAt.Format(time.TimeOnly), fitted.model.A, fitted.learnedAt.Format(time.TimeOnly))
		}
	}
	return holds(held > 0, kindNotLearned, "the lender's line was never held under the band")
}

// --- row 4: the borrowed k2 lower bound ---

// borrowRig is variant-b borrowing from variant-a's window at donor, own the
// capacity variant-b has without the bound.
type borrowRig struct {
	a          *SaturationAnalyzer
	clock      time.Time
	own, donor string
}

func newBorrowRig(donorK2 float64, ownKv, donorKv int64) *borrowRig {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v1"), TotalKvCapacityTokens: donorKv}, propStart)
	store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: mtParams(256, "vllm:v1"), TotalKvCapacityTokens: ownKv}, propStart)
	r := &borrowRig{a: NewSaturationAnalyzer(store), clock: propStart}
	r.a.now = func() time.Time { return r.clock }
	r.own = r.a.historyKey("test-model", "test-ns", "variant-b", "H100", 1, domain.RoleBoth, 50, 5)
	r.donor = r.a.historyKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleBoth, 50, 5)
	ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, propStart)
	ra.AddAt(donorK2, propStart)
	r.a.computeCapacityHistory[r.donor] = ra
	return r
}

// at asks for the bound at the rig's clock, as a serving replica of
// variant-b whose live KV total is kv.
func (r *borrowRig) at(own, kv int64) (k2Borrow, bool) {
	return r.a.borrowedK2WithKv(r.own, "test-model", own, mtParams(256, "vllm:v1"), kv, true, true)
}

// serve advances the clock by d in one-minute serving cycles.
func (r *borrowRig) serve(own int64, d time.Duration) {
	for end := r.clock.Add(d); r.clock.Before(end); {
		// The last cycle lands on end exactly, so a threshold is probed at
		// the threshold and not a minute past it.
		r.clock = r.clock.Add(min(time.Minute, end.Sub(r.clock)))
		r.at(own, 0)
	}
}

// checkBorrowLowerOnly: whatever the donor measured and however far the bound
// has relaxed, the k2 a variant is priced on is between the donor's figure
// and its own, never above its own.
func checkBorrowLowerOnly(seed uint64) error {
	r := propRand(seed)
	own := int64(12_000 + r.IntN(4000))
	donor := float64(own) * (0.3 + 0.6*r.Float64())
	rig := newBorrowRig(donor, 0, 0)
	if _, ok := rig.at(own, 0); !ok {
		return violation(kindNotLearned, "variant-b did not borrow %.0f under its own %d", donor, own)
	}
	for range 50 {
		rig.serve(own, time.Duration(10+r.IntN(20))*time.Minute)
		b, ok := rig.at(own, 0)
		if !ok {
			return nil // spent: priced on its own
		}
		if b.k2 > own || float64(b.k2) < donor-1 {
			return violation(kindOverPriced, "after %s a bound of %d against own %d and donor %.0f", b.age, b.k2, own, donor)
		}
	}
	return nil
}

// checkBorrowRelaxesUpward: along serving time the bound only rises toward
// the variant's own figure: a held then rising price, never one that falls.
func checkBorrowRelaxesUpward(seed uint64) error {
	r := propRand(seed)
	own := int64(12_000 + r.IntN(4000))
	rig := newBorrowRig(float64(own)*(0.3+0.4*r.Float64()), 0, 0)
	prev := int64(0)
	for range 80 {
		b, ok := rig.at(own, 0)
		if !ok {
			return holds(prev > 0, kindNotLearned, "never borrowed")
		}
		if b.k2 < prev {
			return violation(kindFell, "at %s the bound fell %d -> %d", b.age, prev, b.k2)
		}
		prev = b.k2
		rig.serve(own, time.Duration(5+r.IntN(10))*time.Minute)
	}
	return nil
}

// checkBorrowPause: a gap with no serving cycle -- the variant parked -- does
// not advance the bound: it reads the same relaxation after the pause as
// before it, up to the one tick gap a pause may add.
func checkBorrowPause(seed uint64) error {
	r := propRand(seed)
	own := int64(14_000)
	rig := newBorrowRig(float64(own)*0.5, 0, 0)
	rig.at(own, 0)
	rig.serve(own, BorrowedK2Hold+time.Duration(30+r.IntN(60))*time.Minute)
	before, ok := rig.at(own, 0)
	if !ok || before.relaxed <= 0 {
		return violation(kindNotLearned, "no relaxing bound before the pause")
	}
	rig.clock = rig.clock.Add(time.Duration(2+r.IntN(20)) * time.Hour)
	after, _ := rig.at(own, 0)
	allow := relaxedAt(before.age+BorrowedK2TickGap) - before.relaxed
	return holds(after.relaxed-before.relaxed <= allow+1e-9, kindMoved,
		"a pause relaxed the bound %.3f -> %.3f (one tick gap allows %.3f)", before.relaxed, after.relaxed, allow)
}

// checkBorrowExpires: a bound served through its hold and relaxation is
// spent: the variant is priced on its own figure.
func checkBorrowExpires(seed uint64) error {
	r := propRand(seed)
	own := int64(14_000)
	rig := newBorrowRig(float64(own)*(0.4+0.4*r.Float64()), 0, 0)
	if _, ok := rig.at(own, 0); !ok {
		return violation(kindNotLearned, "no bound to expire")
	}
	rig.serve(own, BorrowedK2Hold+BorrowedK2Relax+time.Duration(1+r.IntN(30))*time.Minute)
	b, ok := rig.at(own, 0)
	return holds(!ok, kindStale, "after the hold and the relaxation a bound of %d still prices (relaxed %.3f)", b.k2, b.relaxed)
}

// checkBorrowDisabled: with disableK2Borrow the sibling's figure is not
// offered at all, whatever it is.
func checkBorrowDisabled(seed uint64) error {
	r := propRand(seed)
	own := int64(14_000)
	rig := newBorrowRig(float64(own)*(0.3+0.6*r.Float64()), 0, 0)
	b, ok := rig.a.borrowedK2WithKv(rig.own, "test-model", own, mtParams(256, "vllm:v1"), 0, true, !borrowOffPolicy.DisableK2Borrow)
	return holds(!ok, kindMoved, "disableK2Borrow: a sibling's %d was still offered", b.k2)
}

// --- row 8: the fleet shape and its hold ---

// shapeRig drives noteFleetShape on its own clock.
type shapeRig struct {
	a     *SaturationAnalyzer
	clock time.Time
}

func newShapeRig() *shapeRig {
	r := &shapeRig{a: NewSaturationAnalyzer(capacity.NewStore()), clock: propStart}
	r.a.now = func() time.Time { return r.clock }
	return r
}

// note folds one cycle's (in, out) and advances the clock a cycle.
func (r *shapeRig) note(in, out float64, hold time.Duration) (stableOut, stableIn float64, outstanding bool) {
	o, i, h := r.a.noteFleetShape("ns", "m", in, out, 0, false, hold, logr.Discard())
	r.clock = r.clock.Add(propCycle)
	return o, i, h
}

// checkShapeFirst: the first shape a fleet reports, and the first output
// length after cycles of none, raise no change.
func checkShapeFirst(seed uint64) error {
	r := propRand(seed)
	rig := newShapeRig()
	in, out := 500+1000*r.Float64(), 200+2000*r.Float64()
	if _, _, held := rig.note(in, 0, ShapeChangeHoldMax); held {
		return violation(kindAdoptedRaw, "the first prompt length raised a change")
	}
	_, _, held := rig.note(in, out, ShapeChangeHoldMax)
	return holds(!held, kindAdoptedRaw, "the first output length %.0f raised a change", out)
}

// checkShapeMissingAxis: a cycle with no completions reports neither axis;
// it carries the shape, raises no change, and keeps the key.
func checkShapeMissingAxis(seed uint64) error {
	r := propRand(seed)
	rig := newShapeRig()
	in, out := 500+1000*r.Float64(), 200+2000*r.Float64()
	for range 4 {
		rig.note(in, out, ShapeChangeHoldMax)
	}
	for _, miss := range [][2]float64{{0, 0}, {in, 0}, {0, out}} {
		o, i, held := rig.note(miss[0], miss[1], ShapeChangeHoldMax)
		if held || o != out || i != in {
			return violation(kindMoved, "a cycle reading (%.0f, %.0f) gave (%.0f, %.0f), held %v, against (%.0f, %.0f)", miss[0], miss[1], i, o, held, in, out)
		}
	}
	return nil
}

// checkShapeWobble: a shape wobbling (autocorrelated, at most 8 % either
// way) around a fixed point never leaves the 20 % tolerance of its first
// reading, so the key never moves from it.
func checkShapeWobble(seed uint64) error {
	r := propRand(seed)
	rig := newShapeRig()
	in, out := 1000.0, 1000.0
	noise := arNoise(r, 120, 0.04, 0.9)
	// Clamped inside the band: the noise-free shape is (1000, 1000), and no
	// reading leaves the tolerance of it.
	lim := 0.4 * shapeTolerance
	key := 0.0
	for i, e := range noise {
		e = math.Max(-lim, math.Min(lim, e))
		o, _, held := rig.note(in*(1+e), out*(1-e), ShapeChangeHoldMax)
		if i == 0 {
			key = o
			continue
		}
		if held || o != key {
			return violation(kindFlipped, "cycle %d: a %.1f %% wobble moved the key %.0f -> %.0f (held %v)", i, 100*e, key, o, held)
		}
	}
	return nil
}

// checkShapeHoldReleased: a change raises a hold, which the backstop releases
// once its span has passed (shapeChangeHoldSeconds, or 5 min), and not before.
func checkShapeHoldReleased(p *config.ScalingPolicy) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		hold, _ := p.ShapeChangeHold(ShapeChangeHoldMax)
		rig := newShapeRig()
		for range 4 {
			rig.note(1000, 1000, hold)
		}
		if _, _, held := rig.note(1000, 1000*(1.5+r.Float64()), hold); !held {
			return violation(kindNotDetected, "a 50 %% output change raised no hold")
		}
		raised := rig.clock.Add(-propCycle)
		for rig.clock.Sub(raised) < hold {
			if _, _, held := rig.note(1000, 2000, hold); !held {
				return violation(kindMoved, "the hold ended %s after it was raised, against %s", rig.clock.Sub(raised)-propCycle, hold)
			}
		}
		_, _, held := rig.note(1000, 2000, hold)
		return holds(!held, kindStale, "the hold outlived its %s span", hold)
	}
}

// checkShapeHoldDisabled: disableShapeChangeHold tracks the change (the key
// follows) and raises no hold.
func checkShapeHoldDisabled(seed uint64) error {
	r := propRand(seed)
	hold, _ := holdOffPolicy.ShapeChangeHold(ShapeChangeHoldMax)
	rig := newShapeRig()
	for range 4 {
		rig.note(1000, 1000, hold)
	}
	to := 1000 * (1.5 + r.Float64())
	o, _, held := rig.note(1000, to, hold)
	return holds(!held && o == to, kindMoved, "disableShapeChangeHold: held %v, key %.0f against %.0f", held, o, to)
}

// shapeTolerance is the tracker's band, as the cases read it.
const shapeTolerance = shape.DefaultChangeTolerance

// --- row 9: the decode-saturation memory ---

// fullDecode is a decode row at frac of its cache with queue waiting.
func fullDecode(pod string, frac float64, queue int) domain.ReplicaMetrics {
	rm := satRow(pod, int64(frac*float64(propKv)), queue, 6)
	rm.TotalKvCapacityTokens = propKv
	return rm
}

var pdRoles = map[string]string{"v": domain.RoleDecode, "p": domain.RolePrefill}

// checkDecodeMemoryOrder: whether decode is saturated is decided over every
// decode row, whatever order they arrive in.
func checkDecodeMemoryOrder(seed uint64) error {
	r := propRand(seed)
	rows := make([]domain.ReplicaMetrics, 6)
	for i := range rows {
		rows[i] = fullDecode(fmt.Sprintf("d%d", i), 0.2+0.1*r.Float64(), 0)
	}
	rows[r.IntN(len(rows))] = fullDecode("dfull", 0.95, 10)
	want := roleSaturated(rows, pdRoles, domain.RoleDecode, 5, 0.8)
	for range 10 {
		r.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
		if got := roleSaturated(rows, pdRoles, domain.RoleDecode, 5, 0.8); got != want {
			return violation(kindMoved, "the verdict moved %v -> %v with the row order", want, got)
		}
	}
	return holds(want, kindNotDetected, "a full, queued decode replica did not saturate decode")
}

// checkDecodeMemoryMissing: a full, queued decode row that is not Ready, or
// carries no cache size, does not count: it cannot be judged.
func checkDecodeMemoryMissing(seed uint64) error {
	r := propRand(seed)
	frac := 0.85 + 0.1*r.Float64()
	unready := fullDecode("d0", frac, 10)
	unready.Ready = false
	unsized := fullDecode("d1", frac, 10)
	unsized.TotalKvCapacityTokens = 0
	if roleSaturated([]domain.ReplicaMetrics{unready, unsized}, pdRoles, domain.RoleDecode, 5, 0.8) {
		return violation(kindMoved, "an unready or unsized row saturated decode")
	}
	return holds(roleSaturated([]domain.ReplicaMetrics{fullDecode("d2", frac, 10)}, pdRoles, domain.RoleDecode, 5, 0.8),
		kindNotDetected, "the same row Ready and sized did not")
}

// checkDecodeMemoryGatesPrefill: on a P/D model, a saturated prefill replica
// learns no k2 while decode is full and queued, nor for a minute after, and
// learns it once decode has cleared for longer (the memory's span).
func checkDecodeMemoryGatesPrefill(seed uint64) error {
	r := propRand(seed)
	rig := newPropRig()
	states := []domain.VariantReplicaState{decodeStates(1)[0],
		{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill}}
	prefillK2 := func(decodeFull bool) error {
		d := fullDecode("d0", 0.3, 0)
		if decodeFull {
			d = fullDecode("d0", 0.95, 10)
		}
		p := satRow("p0", int64(float64(propKv)*(0.88+0.05*r.Float64())), 10, 6)
		p.VariantName = "p"
		_, err := rig.analyze([]domain.ReplicaMetrics{d, p}, states, 6, propCycle)
		return err
	}
	for range 8 {
		if err := prefillK2(true); err != nil {
			return err
		}
	}
	if k := rig.k2(domain.RolePrefill); k > 0 {
		return violation(kindTransient, "prefill learned k2 %.0f behind a full decode", k)
	}
	for range 3 { // under the minute's memory
		if err := prefillK2(false); err != nil {
			return err
		}
	}
	if k := rig.k2(domain.RolePrefill); k > 0 {
		return violation(kindTransient, "prefill learned k2 %.0f inside the minute decode was remembered full", k)
	}
	for range 10 {
		if err := prefillK2(false); err != nil {
			return err
		}
	}
	return holds(rig.k2(domain.RolePrefill) > 0, kindNotLearned, "prefill never learned k2 once decode cleared")
}
