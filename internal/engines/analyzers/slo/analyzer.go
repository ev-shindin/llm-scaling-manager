// Package slo is the SLO analyzer: it prices each variant's capacity at the
// fill where the variant meets the latency targets its scaling policy declares,
// and judges whether the model meets them as the router measured it.
//
// Phase 0 of docs/proposals/slo-tiers.md. On this release the engine runs the
// analyzer for every model whose policy lists it, publishes its (D, P) as
// wva_analyzer_demand / wva_analyzer_target{analyzer_name="slo"} and its own
// wva_slo_* series, and sizes from the saturation analyzer alone: nothing this
// package returns changes a replica count.
package slo

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// backlogDrainSeconds is how quickly the analyzer asks the fleet to clear the
// requests waiting in the router's queue: each costs O output tokens, spread
// over this many seconds of demand. A queue under a TPOT SLO is already a
// latency miss for someone, so the drain is short.
const backlogDrainSeconds = 60.0

// defaultStateTimeout is how long a variant's learned state -- its ITL window
// and intercept, its accelerator memo -- and a model's transient memo outlive
// the last cycle that analyzed them or reported them present, unless the engine
// sets another (WithStateTimeout). The saturation analyzer's default variant
// timeout.
const defaultStateTimeout = 24 * time.Hour

// RouterLatencySource reads a model's request latency as the router measured
// it. The collector implements it.
type RouterLatencySource interface {
	CollectRouterLatency(ctx context.Context, modelID, namespace string) *domain.RouterLatency
}

// Analyzer is the SLO analyzer. Safe for concurrent use.
type Analyzer struct {
	router RouterLatencySource
	store  *capacity.Store
	now    func() time.Time
	// stateTimeout is how long unused learned state is kept (EvictStale).
	stateTimeout time.Duration
	// slopeHoldFor is how long after it was fitted a line's slope may be held
	// while the window has too few readings to fit (itl.Admission.SlopeHoldFor).
	slopeHoldFor time.Duration

	// lines fits each generating variant's ITL(k), with the same itl.Tracker
	// and so the same rules as the saturation analyzer. Its own instance:
	// saturation fits only the decode role, and an aggregated replica (role
	// "both"), the common deployment, would never get a line there.
	lines *itl.Tracker

	mu sync.Mutex
	// cycle counts the engine's cycles (BeginCycle), so a model can tell it
	// was not analyzed on the one before.
	cycle uint64
	// seen is when each variant key was last analyzed or reported present:
	// its line is evicted on it (EvictStale).
	seen map[string]time.Time
	// args is the engine arguments (capacity.EngineParams.ArgsTag) each
	// variant key's line was last fitted under: a change forgets the line
	// (linesUnderArgs).
	args map[string]string
	// accelerators is the last resolved accelerator per namespace/variant,
	// so a cycle on which the pods disagree about it keys the line where it
	// was (stableAccelerator).
	accelerators map[string]acceleratorMemo
	// transients is each model's recent fleet history, keyed namespace/model,
	// for the transient exclusion (transient.go).
	transients map[string]*transientMemo

	// rec records every call the engine makes, with what it returned
	// (recording.go); nil when recording is off.
	rec *learnrec.Recorder
}

type acceleratorMemo struct {
	name     string
	lastUsed time.Time
}

// New returns an SLO analyzer. router may be nil, in which case the SLO is
// priced but never judged; store may be nil, in which case no engine batch cap
// applies and capacity comes from live readings only.
func New(router RouterLatencySource, store *capacity.Store) *Analyzer {
	return &Analyzer{
		router:       router,
		store:        store,
		now:          clock.RealClock{}.Now,
		stateTimeout: defaultStateTimeout,
		slopeHoldFor: capacity.HistoryRetention,
		lines:        itl.NewTracker(),
		seen:         make(map[string]time.Time),
		args:         make(map[string]string),
		accelerators: make(map[string]acceleratorMemo),
		transients:   make(map[string]*transientMemo),
	}
}

// WithClock sets the clock the analyzer stamps and ages its learned state on:
// the engine passes its cycle clock, which holds one reading per cycle. Call it
// before the analyzer is used.
func (a *Analyzer) WithClock(c clock.PassiveClock) *Analyzer {
	a.now = c.Now
	return a
}

// WithStateTimeout sets how long unused learned state is kept: the engine
// passes the saturation analyzer's variant timeout, so
// LEARNED_STATE_VARIANT_TIMEOUT governs both. A non-positive d keeps the
// default. Call it before the analyzer is used.
func (a *Analyzer) WithStateTimeout(d time.Duration) *Analyzer {
	if d > 0 {
		a.stateTimeout = d
	}
	return a
}

// WithSlopeHoldFor sets how long after it was fitted a line's slope may be held
// while the window has too few readings to fit: the engine passes the
// saturation analyzer's bucket retention, so LEARNED_STATE_BUCKET_RETENTION
// bounds the hold in both. A non-positive d keeps the default. Call it before
// the analyzer is used.
func (a *Analyzer) WithSlopeHoldFor(d time.Duration) *Analyzer {
	if d > 0 {
		a.slopeHoldFor = d
	}
	return a
}

// StateTimeout reports how long unused learned state is kept.
func (a *Analyzer) StateTimeout() time.Duration { return a.stateTimeout }

// Name implements domain.Analyzer.
func (a *Analyzer) Name() string { return domain.SLOAnalyzerName }

// Judging reports whether the analyzer can judge an SLO at all: whether it
// was given a source for the router's latency. Without one it still prices
// variants, and every objective stays unjudged.
func (a *Analyzer) Judging() bool { return a.router != nil }

// analyze is Analyze, and what the call read from the router and published,
// for the recorder.
func (a *Analyzer) analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, judged, error) {
	logger := ctrl.LoggerFrom(ctx)
	now := a.now()
	cfg, _ := input.Config.(*config.ScalingPolicy)
	if cfg == nil {
		return nil, judged{}, fmt.Errorf("slo analyzer: config is %T, want *config.ScalingPolicy", input.Config)
	}
	result := &domain.AnalyzerResult{
		AnalyzerName: domain.SLOAnalyzerName,
		ModelID:      input.ModelID,
		Namespace:    input.Namespace,
		AnalyzedAt:   now,
	}
	snap := metrics.SLOModelSnapshot{Namespace: input.Namespace, ModelName: input.ModelID}

	fleet := readFleet(input)
	// An output length nobody has reported yet is not a demand of zero: it is
	// no demand at all, and a combiner must read it as undefined. So the demand
	// is left out, and every generating variant below reports no data.
	shapeKnown := fleet.shape.AvgOutputTokens > 0 && finite(fleet.shape.AvgOutputTokens)
	demand := fleet.demand(input)
	if shapeKnown {
		result.TotalDemand = demand
		if fleet.disaggregated {
			result.RoleDemand = map[string]float64{domain.RoleDecode: demand}
		}
	}

	var tpotTarget float64
	if cfg.SLO != nil && cfg.SLO.TPOT != nil {
		tpotTarget = cfg.SLO.TPOT.Target.Seconds()
	}
	// The threshold the engine publishes this analyzer's target with: the slo
	// entry's own when it sets one, the tier's otherwise.
	scaleUp, _ := cfg.AnalyzerThresholds(domain.SLOAnalyzerName)
	for _, vs := range fleet.variants {
		key := a.variantKey(input.Namespace, input.ModelID, vs)
		vc, reading, d := a.priceVariant(input, vs, key, fleet, tpotTarget, scaleUp, now)
		if !shapeKnown && reading != nil {
			vc.PerReplicaCapacity, vc.Reason = 0, domain.ReasonNoData
			reading.Source = constants.SLOSourceNoOutputLength
			reading.Fill, reading.PredictedITL = math.NaN(), math.NaN()
		}
		if shapeKnown && fleet.generatingReplicas > 0 && isGenerating(vs.Role) {
			vc.TotalDemand = demand * float64(fleet.ready[vs.VariantName]) / float64(fleet.generatingReplicas)
		}
		// Over the Ready replicas the demand was split across, not
		// CurrentReplicas: a replica still starting serves none of it.
		if vc.PerReplicaCapacity > 0 && vc.ObservedReplicas > 0 {
			vc.Utilization = vc.TotalDemand / (float64(vc.ObservedReplicas) * vc.PerReplicaCapacity)
		}
		result.VariantCapacities = append(result.VariantCapacities, vc)
		if reading != nil {
			snap.Variants = append(snap.Variants, *reading)
			logger.V(logging.VERBOSE).Info("slo-variant",
				"modelID", input.ModelID, "namespace", input.Namespace,
				"variant", vs.VariantName, "role", vs.Role, "source", reading.Source,
				"fill", reading.Fill, "predictedITL", reading.PredictedITL,
				"perReplicaTokensPerSec", vc.PerReplicaCapacity, "reason", vc.Reason,
				// Which line, and what it was fitted from: the window's key,
				// the coefficients, and what the window was offered and held.
				"lineKey", key, "lineA", d.line.Model.A, "lineB", d.line.Model.B, "lineTier", d.line.Tier,
				// A held slope reads pinned or pinned-default on the metric
				// like a fit made this cycle; these say it is an old one.
				"lineSlopeHeld", d.line.SlopeHeld, "lineSlopeAge", d.line.SlopeAge(now),
				"considered", d.line.Considered, "held", d.line.Held, "kept", d.line.Offered,
				"belowBand", d.line.BelowBand,
				"aboveBand", d.line.AboveBand, "noK", d.line.NoK, "noITL", d.line.NoITL,
				"notReady", d.line.NotReady, "declined", d.line.Declined,
				// The fill before the clamp and the batch cap, and the cap's inputs.
				"targetFill", d.targetFill, "capacityTokens", d.capTokens, "kvTokensPerRequest", fleet.shape.KVreq,
				"maxNumSeqs", d.maxSeqs, "maxNumActiveSeqs", d.activeSeqs, "engineRanks", d.ranks, "scaleUpThreshold", scaleUp)
		}
	}

	var j judged
	snap.Objectives, j.Latency, j.RouterCalled = a.judge(ctx, input, cfg.SLO)
	// A window that covered a wake or a starting replica is not judged: the
	// requests in it waited out capacity no steady-state sizing could have had
	// ready. The request count is still published, and the reason is.
	window, grace := time.Duration(routerWindowSeconds(snap.Objectives))*time.Second, transientGrace(input.ReplicaMetrics)
	ttftExcluded, tpotExcluded := a.noteTransient(input.Namespace+"/"+input.ModelID, input.VariantStates, window, grace, now)
	if logger.V(logging.VERBOSE).Enabled() {
		t := a.transientDetail(input.Namespace + "/" + input.ModelID)
		// Why a window is not judged, and until when: the mark's reason and
		// time, per scope, and the cycles since the model was last analyzed.
		logger.V(logging.VERBOSE).Info("slo-transient",
			"modelID", input.ModelID, "namespace", input.Namespace,
			"ttftExcluded", ttftExcluded, "tpotExcluded", tpotExcluded,
			"allMark", t.all.reason, "allMarkAt", t.all.at, "generatingMark", t.generating.reason,
			"generatingMarkAt", t.generating.at, "holdFor", window+grace,
			"cyclesSinceAnalyzed", t.cyclesSinceAnalyzed, "pendingSince", t.pendingSince)
	}
	for i := range snap.Objectives {
		o := &snap.Objectives[i]
		o.Excluded = tpotExcluded
		if o.Objective == constants.SLOObjectiveTTFT {
			o.Excluded = ttftExcluded
		}
		if o.Excluded != "" {
			o.Observed, o.Attainment = math.NaN(), math.NaN()
		}
	}
	for _, o := range snap.Objectives {
		logger.V(logging.VERBOSE).Info("slo-model",
			"modelID", input.ModelID, "namespace", input.Namespace,
			"objective", o.Objective, "statistic", o.Statistic,
			"target", o.TargetSeconds, "observed", o.Observed, "attainment", o.Attainment,
			"requests", o.Requests, "excluded", o.Excluded,
			"demandTokensPerSec", demand, "outputTokens", fleet.shape.AvgOutputTokens)
	}
	metrics.PublishSLO(snap, now)
	j.Published = snap
	return result, j, nil
}

// pricingDetail is what a variant was priced from, for its log line.
type pricingDetail struct {
	line       itl.Reading
	targetFill float64
	capTokens  float64
	// maxSeqs is max_num_seqs times ranks, and activeSeqs is
	// max_num_active_seqs times ranks (0 when unset). The replica's batch cap
	// is the lower of the two.
	maxSeqs    int64
	activeSeqs int64
	ranks      int64
}

// priceVariant fits the variant's line and prices one replica of it at the
// TPOT target. The reading is nil for a variant the TPOT objective does not
// describe: a prefill role, or a policy with no TPOT target.
func (a *Analyzer) priceVariant(input domain.AnalyzerInput, vs domain.VariantReplicaState, key string,
	fleet fleetReading, tpotTarget, scaleUp float64, now time.Time) (domain.VariantCapacity, *metrics.SLOVariantReading, pricingDetail) {
	d := pricingDetail{targetFill: math.NaN(), capTokens: math.NaN()}
	vc := domain.VariantCapacity{
		VariantName:      vs.VariantName,
		Role:             vs.Role,
		ReplicaCount:     vs.CurrentReplicas,
		PendingReplicas:  vs.PendingReplicas,
		StuckReplicas:    vs.StuckReplicas,
		PendingAges:      vs.PendingAges,
		ObservedReplicas: fleet.ready[vs.VariantName],
		Reason:           domain.ReasonNoData,
	}
	// Prefill is not modelled in phase 0: its TPOT is meaningless (the P/D
	// sidecar asks it for one token), and TTFT is not priced yet. Left as
	// no-data, so a combiner treats it as undefined rather than as zero.
	if !isGenerating(vs.Role) {
		return vc, nil, d
	}
	params := a.engineParams(input.Namespace, input.ModelID, vs.VariantName)
	// Analyze has checked the config's type; a direct caller may pass none,
	// which reads as the defaults.
	cfg, _ := input.Config.(*config.ScalingPolicy)
	adm := admission(cfg, params)
	// A held slope ends on the retention and on a change of engine arguments
	// (itl.Admission), and a change of arguments forgets the window first, so
	// the old engine's readings are neither refitted nor held as the new one's.
	args := params.ArgsTag()
	a.linesUnderArgs(key, args, now)
	adm.SlopeHoldFor, adm.Args = a.slopeHoldFor, args
	// The fill is clamped to the band the line is fitted over.
	band := fillBand{lo: adm.MinK, hi: adm.MaxK}
	line, source, r := a.fitLine(key, input.ReplicaMetrics, vs.VariantName, adm, now)
	d.line = r
	if !(tpotTarget > 0) {
		return vc, nil, d
	}
	reading := newVariantReading(vs.VariantName, source)
	if source == constants.SLOSourceNoLine || source == constants.SLOSourceDeclined {
		return vc, reading, d
	}
	p := priceTPOT(band, line, tpotTarget)
	d.targetFill = p.target
	switch {
	case !p.ok:
		reading.Source = constants.SLOSourceInputInvalid
		vc.Reason = domain.ReasonError
		return vc, reading, d
	case !p.feasible:
		// Abstain, never cap: holding an infeasible variant at its count would
		// freeze a single-variant fleet as load rises.
		reading.Source = constants.SLOSourceInfeasible
		return vc, reading, d
	}
	// The batch cap bounds one engine: max_num_seqs, lowered by
	// max_num_active_seqs when set (issue #335). A replica of several
	// data-parallel ranks admits that many times it, against capacity tokens
	// that are already the whole replica's (issue #150).
	d.ranks = fleet.engineRanks(vs.VariantName)
	if params != nil {
		d.maxSeqs = params.MaxNumSeqs * d.ranks
		d.activeSeqs = params.MaxNumActiveSeqs * d.ranks
	}
	maxSeqs := params.BatchCap() * d.ranks
	capTokens := fleet.capacityTokens(vs.VariantName, params)
	d.capTokens = capTokens
	fill := reachableFill(band, p.fill, capTokens, fleet.shape.KVreq, maxSeqs)
	rate := replicaTokenRate(line, fill, capTokens, fleet.shape.KVreq, maxSeqs)
	if !(rate > 0) {
		reading.Source = constants.SLOSourceInputInvalid
		vc.Reason = domain.ReasonError
		return vc, reading, d
	}
	vc.PerReplicaCapacity = rate
	vc.Reason = "slo-" + source
	reading.Fill = fill
	if k := realisedFill(band, line, fill, scaleUp, capTokens, fleet.shape.KVreq, maxSeqs); finite(k) {
		reading.PredictedITL = line.ITLAt(k)
	}
	return vc, reading, d
}

// admission is which rows teach the variant's line: the policy's ITL band, and
// the sequence cap only when the engine's flag stated it (itl.Admission). The
// saturation analyzer builds the same from the same two sources, so both
// analyzers read a variant's rows alike.
func admission(cfg *config.ScalingPolicy, params *capacity.EngineParams) itl.Admission {
	if cfg == nil {
		cfg = &config.ScalingPolicy{}
	}
	lo, hi := cfg.ITLBand()
	return itl.Admission{MinK: lo, MaxK: hi, MaxNumSeqs: params.ExplicitMaxNumSeqs(),
		CapMaxPreemptionRate: cfg.ITLCapPreemptionRate(), Steady: cfg.ITLSteadiness()}
}

// newVariantReading is a variant's reading with every figure unknown: NaN,
// which PublishSLO does not publish. The one place readings are made, because a
// field left at Go's zero value is published as a real 0: a fill of zero, or a
// predicted ITL of zero, which reads as a met target.
func newVariantReading(variant, source string) *metrics.SLOVariantReading {
	nan := math.NaN()
	return &metrics.SLOVariantReading{Variant: variant, Source: source, Fill: nan, PredictedITL: nan}
}

// fitLine fits one variant's ITL(k) = A*k + B with the shared itl.Tracker --
// the saturation analyzer's rules, by construction -- and names where the line
// came from: a full fit, B pinned to this key's own last full fit, or B pinned
// to the default floor; or why there is none: the window held enough readings
// and the pinned fit refused them, or it holds too few.
func (a *Analyzer) fitLine(key string, rows []domain.ReplicaMetrics, variant string, adm itl.Admission, now time.Time) (itl.Model, string, itl.Reading) {
	a.mu.Lock()
	a.seen[key] = now
	a.mu.Unlock()
	r := a.lines.Observe(key, variant, rows, adm, now)
	switch {
	case r.Tier == itl.TierOLS:
		return r.Model, constants.SLOSourceOwn, r
	case r.Tier == itl.TierPinnedB && r.BaselineLearned:
		return r.Model, constants.SLOSourcePinned, r
	case r.Tier == itl.TierPinnedB:
		// Not a measurement of this hardware: a constant for another card.
		// Labelled apart, so the line it pins is not read as one.
		return r.Model, constants.SLOSourcePinnedDefault, r
	case r.Declined:
		// Not "too few yet": a steady state. Most often a fast card whose ITL
		// sits below the pinned intercept, so the slope comes out negative.
		return itl.Model{}, constants.SLOSourceDeclined, r
	}
	return itl.Model{}, constants.SLOSourceNoLine, r
}

// routerWindowSeconds is the rate window the router figures were taken over:
// the collector's, carried on the reading, and the collector's default when no
// figure came back.
func routerWindowSeconds(readings []metrics.SLOObjectiveReading) float64 {
	for _, r := range readings {
		if r.WindowSeconds > 0 {
			return r.WindowSeconds
		}
	}
	return defaultRouterWindowSeconds
}

// defaultRouterWindowSeconds matches registration.SLORouterWindowSeconds, the
// window the router queries use. Spelled here so the analyzer does not import
// the collector's registration package for one number.
const defaultRouterWindowSeconds = 300.0

// judge reads the router's latency for the model and reports each objective:
// the TPOT mean, and the TTFT percentile with its attainment. A figure from too
// few requests is NaN, which is not published. It returns, for the recorder, the
// latency it read and whether it asked the router at all.
func (a *Analyzer) judge(ctx context.Context, input domain.AnalyzerInput, slo *config.SLOPolicy) ([]metrics.SLOObjectiveReading, *domain.RouterLatency, bool) {
	if slo.Empty() {
		return nil, nil, false
	}
	var rl *domain.RouterLatency
	if a.router != nil {
		rl = a.router.CollectRouterLatency(ctx, input.ModelID, input.Namespace)
	}
	var out []metrics.SLOObjectiveReading
	if slo.TPOT != nil {
		r := metrics.SLOObjectiveReading{
			Objective: constants.SLOObjectiveTPOT, Statistic: "mean",
			TargetSeconds: slo.TPOT.Target.Seconds(), Observed: math.NaN(), Attainment: math.NaN(),
			Requests: math.NaN(),
		}
		if rl != nil {
			r.WindowSeconds = rl.WindowSeconds
			r.Requests = rl.TPOTCountRate * rl.WindowSeconds
			// A mean of 0 is no reading: it would count as met.
			if r.Requests >= minSamples(0) && rl.TPOTSumRate > 0 {
				r.Observed = rl.TPOTSumRate / rl.TPOTCountRate
			}
		}
		out = append(out, r)
	}
	if slo.TTFT != nil {
		p := slo.TTFTPercentile()
		r := metrics.SLOObjectiveReading{
			Objective: constants.SLOObjectiveTTFT, Statistic: "p" + strconv.Itoa(p),
			TargetSeconds: slo.TTFT.Target.Seconds(), Observed: math.NaN(), Attainment: math.NaN(),
			ObjectiveRatio: float64(p) / 100, Requests: math.NaN(),
		}
		if rl != nil {
			r.WindowSeconds = rl.WindowSeconds
			if h, ok := newHistogram(rl.TTFTBuckets); ok {
				r.Requests = h.total * rl.WindowSeconds
				if r.Requests >= minSamples(p) {
					r.Observed = h.quantile(float64(p) / 100)
					r.Attainment, _ = h.fractionWithin(r.TargetSeconds)
				}
			} else if slices.ContainsFunc(rl.TTFTBuckets, func(b domain.LatencyBucket) bool { return math.IsInf(b.UpperBound, 1) }) &&
				!slices.ContainsFunc(rl.TTFTBuckets, func(b domain.LatencyBucket) bool { return b.Rate != 0 }) {
				// The router reported the histogram and nobody was served: a
				// count of 0, not an absent one. Absent reads as "the router's
				// series are missing", which an idle model's are not.
				r.Requests = 0
			}
		}
		out = append(out, r)
	}
	return out, rl, a.router != nil
}

// linesUnderArgs forgets key's line -- its window, intercept and held slope --
// when the variant now runs under other engine arguments than the ones the line
// was learned under, as the saturation analyzer's itlUnderArgs does. Without
// it the window kept the old engine's readings for up to
// itl.DefaultObservationMaxAge, the pinned fit refitted A from them each cycle
// and stamped it with the new arguments, and the old engine's slope was then
// held as the new one's for the whole slope hold (issue #284). Unknown
// arguments change nothing. There is no stash: a rollback relearns its line.
func (a *Analyzer) linesUnderArgs(key, args string, now time.Time) {
	if args == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, ok := a.args[key]
	a.args[key] = args
	if !ok || prev == args {
		return
	}
	// The tracker's lock is taken under a.mu, as EvictStale takes it.
	a.lines.Sweep(now, func(k string) bool { return k == key })
}

func (a *Analyzer) engineParams(namespace, modelID, variant string) *capacity.EngineParams {
	if a.store == nil {
		return nil
	}
	if rec := a.store.Get(namespace, modelID, variant); rec != nil {
		return rec.EngineParams
	}
	return nil
}

// evictStale forgets the learned state of variants and models not analyzed or
// reported present for the state timeout. The engine calls it with the
// saturation analyzer's sweep on every cycle that has a model to plan, whether
// or not any model lists this analyzer, so each key is visited once per cycle
// rather than once per analyzed model. A cycle with no active model does not
// sweep (the engine returns before it), so the timeout counts reconciled time,
// as the saturation analyzer's does.
func (a *Analyzer) evictStale(now time.Time) Evicted {
	a.mu.Lock()
	defer a.mu.Unlock()
	stale := func(key string) bool {
		at, ok := a.seen[key]
		return !ok || now.Sub(at) > a.stateTimeout
	}
	var ev Evicted
	ev.Lines, ev.Baselines = a.lines.Sweep(now, stale)
	for key := range a.seen {
		if stale(key) {
			delete(a.seen, key)
		}
	}
	for key := range a.args {
		if stale(key) {
			delete(a.args, key)
		}
	}
	for key, m := range a.transients {
		if now.Sub(m.seenAt) > a.stateTimeout {
			delete(a.transients, key)
			ev.Transients++
		}
	}
	for key, m := range a.accelerators {
		if now.Sub(m.lastUsed) > a.stateTimeout {
			delete(a.accelerators, key)
			ev.Accelerators++
		}
	}
	return ev
}

// Evicted counts what one EvictStale forgot, per map.
type Evicted struct {
	Lines, Baselines, Transients, Accelerators int
}

// Any reports whether anything was evicted.
func (e Evicted) Any() bool {
	return e.Lines+e.Baselines+e.Transients+e.Accelerators > 0
}

// beginCycle marks the start of an engine cycle. The engine calls it once per
// cycle, before any model is analyzed and whether or not any is: a model not
// analyzed on the cycle before the one it is analyzed in was skipped, and its
// window is not judged (unobserved).
func (a *Analyzer) beginCycle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cycle++
}

// noteVariantsPresent stamps the learned state of a model's variants that
// were discovered this cycle, analyzed or not: a variant parked at zero is
// not analyzed, and must not lose its line for it.
func (a *Analyzer) noteVariantsPresent(namespace, modelID string, variantNames []string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range variantNames {
		// The accelerator memo too, as saturation's presence does: a variant
		// parked past the state timeout would otherwise wake with no memo, and a
		// first cycle of unresolved readings would key a new, empty line.
		if m, ok := a.accelerators[namespace+"/"+name]; ok {
			m.lastUsed = now
			a.accelerators[namespace+"/"+name] = m
		}
		prefix := namespace + "|" + modelID + "|" + name + "|"
		for key := range a.seen {
			if strings.HasPrefix(key, prefix) {
				a.seen[key] = now
			}
		}
	}
	if m, ok := a.transients[namespace+"/"+modelID]; ok {
		m.seenAt = now
	}
}

// variantKey is windowKey with the stable accelerator in it.
func (a *Analyzer) variantKey(namespace, modelID string, vs domain.VariantReplicaState) string {
	vs.AcceleratorName = a.stableAccelerator(namespace, vs.VariantName, vs.AcceleratorName)
	return windowKey(namespace, modelID, vs)
}

// stableAccelerator is the accelerator to key a variant's line under: the
// reading when it resolves, else the last one that did. The pods of one
// variant disagreeing about it for a cycle is not new hardware, and keying on
// the raw reading opened a fresh, empty window every time they did. The same
// rule as the saturation analyzer's, kept per analyzer like the lines.
func (a *Analyzer) stableAccelerator(namespace, variant, accelerator string) string {
	key := namespace + "/" + variant
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if constants.IsAcceleratorResolved(accelerator) {
		a.accelerators[key] = acceleratorMemo{name: accelerator, lastUsed: now}
		return accelerator
	}
	if last, ok := a.accelerators[key]; ok {
		last.lastUsed = now
		a.accelerators[key] = last
		return last.name
	}
	return accelerator
}

// windowKey names one variant's line. The accelerator and GPU count are part
// of it, because ITL(k) is a property of the hardware a replica runs on.
func windowKey(namespace, modelID string, vs domain.VariantReplicaState) string {
	return namespace + "|" + modelID + "|" + vs.VariantName + "|" + vs.AcceleratorName + "|" +
		strconv.Itoa(vs.GPUsPerReplica)
}

func isGenerating(role string) bool {
	return role != domain.RolePrefill
}

// fleetReading is what the analyzer reads off one model's replicas once per
// cycle.
type fleetReading struct {
	variants      []domain.VariantReplicaState
	disaggregated bool
	// ready counts each variant's Ready, non-pool replica rows, every role
	// included: it is the variant's observed replicas, and a prefill variant
	// reported as 0 read as a scrape gap.
	ready map[string]int
	// generatingReplicas is the total of ready over decode and aggregated
	// variants.
	generatingReplicas int
	// shape is the request shape the generating replicas are serving.
	shape shape.Shape
	// requestRate is the generating replicas' summed completion rate, the
	// fallback when the router reports no arrival rate.
	requestRate float64
	rows        []domain.ReplicaMetrics
}

func readFleet(input domain.AnalyzerInput) fleetReading {
	f := fleetReading{ready: make(map[string]int), rows: input.ReplicaMetrics}
	f.variants = append(f.variants, input.VariantStates...)
	sort.Slice(f.variants, func(i, j int) bool { return f.variants[i].VariantName < f.variants[j].VariantName })
	roles := make(map[string]string, len(f.variants))
	for _, vs := range f.variants {
		roles[vs.VariantName] = vs.Role
		if vs.Role == domain.RolePrefill || vs.Role == domain.RoleDecode {
			f.disaggregated = true
		}
	}
	// Request-rate weighted, so a replica serving most of the traffic decides
	// the shape; equal weights when no replica reports a rate.
	var in, out, hit, weight float64
	for _, rm := range input.ReplicaMetrics {
		if !rm.Ready || rm.FromWarmPool {
			continue
		}
		f.ready[rm.VariantName]++
		if !isGenerating(roles[rm.VariantName]) {
			continue
		}
		f.generatingReplicas++
		if finite(rm.RequestRate) && rm.RequestRate > 0 {
			f.requestRate += rm.RequestRate
		}
	}
	for _, rm := range input.ReplicaMetrics {
		if !rm.Ready || rm.FromWarmPool || !isGenerating(roles[rm.VariantName]) {
			continue
		}
		w := 1.0
		if f.requestRate > 0 {
			w = rm.RequestRate
		}
		// A row with any non-finite shape figure is no reading of the shape: one
		// NaN summed in would turn the whole fleet's shape into NaN.
		if !(w > 0) || !finite(w) || !finite(rm.AvgOutputTokens) || !(rm.AvgOutputTokens > 0) ||
			!finite(rm.AvgInputTokens) || !finite(rm.PrefixCacheHitRate) {
			continue
		}
		in += w * rm.AvgInputTokens
		out += w * rm.AvgOutputTokens
		hit += w * rm.PrefixCacheHitRate
		weight += w
	}
	if weight > 0 {
		f.shape = shape.New(in/weight, out/weight, hit/weight)
	}
	return f
}

// demand is the decode work arriving, in output tokens per second: the arrival
// rate times the output length, plus the router's backlog drained over
// backlogDrainSeconds. Zero when the output length is unknown.
func (f fleetReading) demand(input domain.AnalyzerInput) float64 {
	o := f.shape.AvgOutputTokens
	if !(o > 0) || !finite(o) {
		return 0
	}
	lambda := input.ArrivalRate
	if !(lambda > 0) || !finite(lambda) {
		lambda = f.requestRate
	}
	d := lambda * o
	if q := input.SchedulerQueue; q != nil && q.QueueSize > 0 {
		d += float64(q.QueueSize) * o / backlogDrainSeconds
	}
	return d
}

// capacityTokens is the mean KV capacity of the variant's ready replicas, in
// tokens: the live reading where there is one, the engine's configured
// override otherwise, the same order the saturation analyzer reads them in.
func (f fleetReading) capacityTokens(variant string, params *capacity.EngineParams) float64 {
	var sum float64
	var n int
	for _, rm := range f.rows {
		if rm.VariantName != variant || !rm.Ready || rm.FromWarmPool {
			continue
		}
		c := 0.0
		// math.MaxInt64 is the collector's overflow sentinel, not a capacity.
		if rm.TotalKvCapacityTokens > 0 && rm.TotalKvCapacityTokens < math.MaxInt64 {
			c = float64(rm.TotalKvCapacityTokens)
		} else if params != nil && params.TotalKvTokensOverride > 0 {
			// The flag sizes one engine; the row may run several.
			c = float64(params.TotalKvTokensOverride) * float64(rm.Ranks())
		}
		if c > 0 {
			sum += c
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// engineRanks is the data-parallel engine ranks one of the variant's ready
// replicas runs, over the rows capacityTokens reads (ready, not a warm-pool
// bridge): the rounded mean of their counts, and one when there is no such
// row. A row with no count is one rank. Every replica of a variant
// runs one layout, so the mean is that layout's count; the rounding only keeps
// a row caught mid-rollout from making the cap fractional.
func (f fleetReading) engineRanks(variant string) int64 {
	var sum, n int
	for _, rm := range f.rows {
		if rm.VariantName != variant || !rm.Ready || rm.FromWarmPool {
			continue
		}
		sum += rm.Ranks()
		n++
	}
	if n == 0 {
		return 1
	}
	return max(int64(math.Round(float64(sum)/float64(n))), 1)
}
