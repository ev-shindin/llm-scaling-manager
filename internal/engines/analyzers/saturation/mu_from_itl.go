package saturation

import (
	"math"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// The demand floor's mu is a request rate, and a request rate is not a property
// of a replica -- it is a property of a replica AND a shape. One replica
// generating G tokens a second completes G/O requests a second, so halving the
// generation length doubles mu with nothing about the hardware having changed.
// Measuring that per output-length bucket is a workaround for the units, and it
// cannot be made to work: the reading is only taken while a replica is
// saturated, and a fleet over-provisioned for the shape it has just moved to
// never saturates, so it never prices that shape.
//
// Measured on the 1k/6000 -> 8k/1000 swap of 2026-09-23. Nineteen minutes after
// the switch not one reading had been taken in the bucket the fleet was
// serving, and the floor was still pricing 6 req/s against the 6000-token
// phase's figure. Which figure it inherited was luck: 1.2754 on that run
// against 1.5429 on the one before, implying 4.70 and 3.89 replicas from an
// identical workload. The fleet held 7 while occupancy read a utilization
// of 0.028.
//
// Derived instead, over the model the throughput analyzer already fits and
// signals/itl now carries the arithmetic for:
//
//	muTok = itl.TokenRate(model, kSat, C, KVreq)   tokens/s at saturation
//	muReq = muTok / O                              what the floor prices with
//
// Nothing in it is keyed by a bucket and nothing waits for a saturated cycle:
// (A, B) is fitted from (k, ITL) pairs a replica reports at ANY load, and I
// and O are this cycle's own shape. A shape change reprices the fleet on the
// cycle it is observed rather than whenever the fleet next happens to saturate.
//
// It reproduces a measurement it was never given, which is the reason to
// believe it: on the trace's first shape it derives 1.487 req/s where the fleet
// measured 1.4292 and 1.5429 for itself, and on the second -- which the fleet
// never measured at all -- 4.200 req/s, asking for 1.43 replicas at the 6 req/s
// arriving. Occupancy independently said one.
//
// docs/proposals/shape-shift-treatment.md sets this out in full.

// derivedMu is one variant's mu, priced from its fitted ITL model rather than
// from a saturated reading. ok is false when the variant has no model yet, in
// which case the caller keeps the measured window.
type derivedMu struct {
	rate     float64
	seqs     float64
	tokenSec float64
	ok       bool
}

// capacityTokensFor is the replica's whole KV capacity in tokens: the LIVE
// reading first, the deployment flag only where there is no live reading.
//
// The precedence used to be the other way round, which inverted the capacity
// store's own rule -- LoadFromScaleTarget refuses to overwrite a live record,
// and folds TotalKvTokensOverride in only on the deployment-derived path.
// SGLang's --max-total-tokens is parsed off the Deployment into EngineParams
// for every record, live or not, so as an override it displaced the engine's
// own reported capacity on every cycle of every SGLang variant.
//
// The direction of that error is the one that hurts: the engine clamps
// --max-total-tokens to the memory it actually has, so a flag it could not
// honour over-states C, which over-states the resident sequence count, which
// over-states mu -- and an over-stated mu UNDER-orders replicas. A flag is a
// fallback for a replica reporting nothing at all (a cold variant, or an
// engine that emits no cache_config_info); it is not a measurement.
func capacityTokensFor(params *capacity.EngineParams, liveTokens int64) float64 {
	// math.MaxInt64 is the collector's overflow SENTINEL, not a capacity
	// (internal/collector/attribute.go). Taken literally it makes the resident
	// sequence count astronomical, which prices mu at an absurd figure and --
	// while the check below was a gate -- rejected the replica's line on every
	// cycle forever.
	if liveTokens > 0 && liveTokens < math.MaxInt64 {
		return float64(liveTokens)
	}
	if params != nil && params.TotalKvTokensOverride > 0 {
		return float64(params.TotalKvTokensOverride)
	}
	return 0
}

// noteLineMismatch logs, and only logs, when this replica's observed
// generation-token rate disagrees with what the fitted line predicts at the
// replica's own utilization.
//
// It is a DIAGNOSTIC, not a gate, and that is a decision taken against
// measurement rather than taste.
//
// The proposal asks for a derived mu to be verified against the observed rate
// before it may order. Built as a gate, that verification withheld ordering
// 28 times in run T (2026-09-28) and every one of them fell inside the first
// two minutes of the run -- the phase-1 ramp -- with the predicted rate above
// the observed one nearly every time, and on four cycles every replica of the
// role rejected at once. The fleet still reached 9 so the run stands, but its
// phase-1 TTFT p95 was 64 s against main's 42 s.
//
// The cause is not a threshold that needs tuning. The three signals are
// collected over three different windows -- KvUsageInstant has none,
// GenerationTokenRate is rate[1m], AvgITL is rate[5m] -- so on a ramp k reaches
// its new level in seconds while the 1m rate still reports a fraction of the
// steady state. The disagreement is therefore largest exactly when a scale-up
// is needed, and it moves every replica together, so neither N-consecutive
// cycles nor a majority-of-replicas rule suppresses it.
//
// What it is still worth: it is the line that let run T be diagnosed, and a
// persistent mismatch away from a ramp is real evidence that the shape, and so
// KVreq, does not describe the fleet. When the collector grows a k averaged
// over the same window as the rate, this can become a gate again.
//
// Warm-pool replicas and non-decode roles are skipped rather than logged: a
// bridge runs on the pool's own engine settings (noteITL excludes it from the
// fit for that reason and floor.Estimate prices it not at all), and a prefill
// replica generates about one token per request, so neither disagreement means
// anything about this variant's line.
func (a *SaturationAnalyzer) noteLineMismatch(model itl.Model, params *capacity.EngineParams,
	rm domain.ReplicaMetrics, role string, kvReq float64, logger logr.Logger) {
	if rm.FromWarmPool || canonicalRole(role) != domain.RoleDecode {
		return
	}
	capacityTokens := capacityTokensFor(params, rm.TotalKvCapacityTokens)
	errPct, ok := itl.GPSErrorPct(model, rm.KvUsageInstant, capacityTokens,
		kvReq, rm.GenerationTokenRate)
	if !ok || errPct <= itl.DefaultGPSMismatchThresholdPct {
		return
	}
	logger.V(logging.DEFAULT).Info("itl-gps-mismatch",
		"variant", rm.VariantName, "pod", rm.PodName,
		"k", rm.KvUsageInstant, "observedGPS", rm.GenerationTokenRate,
		"predictedGPS", itl.TokenRate(model, rm.KvUsageInstant, capacityTokens, kvReq),
		"errPct", errPct, "thresholdPct", itl.DefaultGPSMismatchThresholdPct,
		"gates", false)
}

// pricingK is the KV utilization, as a fraction of PHYSICAL capacity, that a
// replica is considered full at -- the point mu is priced for.
//
// It is the product of the two thresholds because they compound: k1, the
// memory-bound capacity, is KvCacheThreshold of physical KV, and the analyzer
// scales out at ScaleUpThreshold of k1. On the defaults (0.80, 0.85) that is
// 0.68 of physical; with ScaleUpThreshold at 0.95 it is 0.76. Both sit inside
// the ITL window's [DefaultMinObservableK, DefaultMaxObservableK] range, so
// the line is evaluated where it was fitted rather than beyond it.
//
// A configuration whose product falls outside that range is clamped to it: the
// model says nothing useful outside the range it was fitted on, and a clamped
// price is a worse answer than an extrapolated one only in theory.
func pricingK(cfg *config.ScalingPolicy) float64 {
	k := itl.DefaultKSat
	if cfg != nil && cfg.KvCacheThreshold*cfg.ScaleUpThreshold > 0 {
		k = cfg.KvCacheThreshold * cfg.ScaleUpThreshold
	}
	// Clamped on every path, including the fallback. DefaultKSat is 0.85 and
	// DefaultMaxObservableK is 0.80, so returning the fallback unclamped
	// evaluated the line 0.05 outside the range it was fitted over -- the
	// exact thing this function exists to prevent.
	return min(max(k, itl.DefaultMinObservableK), itl.DefaultMaxObservableK)
}

// deriveMu prices one replica of this variant at saturation under the shape it
// is serving now.
//
// kvPerRequest is the time-averaged footprint IL + OL/2 (shape.Shape.KVreq):
// sequence ages are spread over [0, OL] in steady state, so the average
// resident sequence carries half its generation.
//
// The cap is the engine's max_num_seqs, and it is not optional: short requests
// imply more resident sequences than the engine will admit, and the trace's
// second phase ran at exactly 256, the configured ceiling. itl.TokenRate does
// not apply it -- it prices a cache, not an engine -- so the sequence count is
// capped here before the division.
//
// C is the engine's whole KV capacity, not k1. k1 is already C times the
// analyzer's KV threshold, and itl.Sequences applies k itself, so passing k1
// would apply a threshold twice.
// avgOutput is the output length mu is divided by, passed in rather than taken
// from fleet: it must be the SHORT-window figure, while the shape keeps the [5m]
// one every other consumer shares. A count-weighted mean over five minutes is
// dominated by the previous shape's long stragglers for five minutes after they
// stop arriving -- measured decaying 3750 -> 250 across one 6000 -> 250 switch --
// and the divisor is where that error reaches mu undamped. KVreq, by contrast,
// is ILeff + OL/2 and barely moves: at a 20k prompt the same error shifts it
// about 1.5%.
func deriveMu(model itl.Model, params *capacity.EngineParams,
	totalKvTokens int64, fleet shape.Shape, kPrice float64, avgOutput float64) derivedMu {
	if model.IsZero() || !(avgOutput > 0) || !(fleet.KVreq > 0) {
		return derivedMu{}
	}
	if !(kPrice > 0) || kPrice > 1 {
		return derivedMu{}
	}
	capacityTokens := capacityTokensFor(params, totalKvTokens)
	// The SHAPE's KVreq, not one re-derived here. shape.New computes
	// ILeff = IL*(1 - prefixHitRate) and KVreq = ILeff + OL/2, so a fleet
	// whose prompts are largely cache hits occupies far less than IL + OL/2
	// and holds correspondingly more sequences. Re-deriving it dropped the
	// hit-rate term, which on a caching workload under-states the resident
	// count and over-orders replicas by the same factor. (It made no
	// difference to the 2026-09-24 run -- those pods export no prefix-cache
	// series at all, so the hit rate was 0 and the two agreed at 4000 -- but
	// that is the workload being uninteresting, not the arithmetic being
	// right.)
	seqs := itl.Sequences(kPrice, capacityTokens, fleet.KVreq)
	if seqs <= 0 {
		return derivedMu{}
	}
	if params != nil && params.MaxNumSeqs > 0 {
		seqs = min(seqs, float64(params.MaxNumSeqs))
	}
	itlSec := model.ITLAt(kPrice)
	if !(itlSec > 0) {
		return derivedMu{}
	}
	tokenSec := seqs / itlSec
	rate := tokenSec / avgOutput
	if !(rate > 0) {
		return derivedMu{}
	}
	return derivedMu{rate: rate, seqs: seqs, tokenSec: tokenSec, ok: true}
}

// noteITL adds this cycle's (k, ITL) readings for one variant to its rolling
// window and returns the model fitted over what the window holds.
//
// Ready replicas of the variant itself only, and only where both halves of the
// pair are present: a pod still failing its readiness probe, or one lent by the
// warm pool, is not a reading of what one of this variant's replicas does.
//
// Why the warm-pool exclusion, stated correctly -- an earlier version of this
// comment said a lent pod runs "the pool's own engine settings", and that is
// false by construction. warmpool.EngineOptionsFrom derives the warm copy's
// command line from the ordinary replicas' own PodSpec precisely so the two
// MATCH (a different --gpu-memory-utilization is a different torch.compile
// cache key), and warmableFlags covers every flag the fingerprint hashes. By
// default a lent pod therefore hashes to the SAME engine configuration, so the
// fingerprint cannot be what tells it apart.
//
// What tells it apart is what it is running: a pool Pod hosts one awake engine
// plus its sleepers, each keeping ~1.4 GiB of GPU residue (demand.go measured
// 4.4 GiB free at 0.95 on an 80 GiB card). Its ITL(k) is the latency of an
// engine sharing a card, which is not the latency of one of this variant's own
// replicas -- and k itself is read against a KV budget the sleepers have
// already eaten into. A pool that does set an explicit GPUMemoryUtilization
// does also change the digest, but the exclusion cannot rest on that: zero
// inherits the workload's value, and zero is the default.
//
// Above itl.DefaultMaxObservableK the
// engine preempts rather than slowing down and the line stops describing it, so
// those readings are left out too.
//
// The window is NOT cleared on a shape change, which is the difference from the
// throughput analyzer's use of it. ITL(k) is a property of the hardware and the
// engine, not of the shape -- that is the whole reason mu can be derived across
// a shape change -- so a fit made under one shape is still the right fit under
// the next, and clearing it would reintroduce exactly the wait this replaces.
func (a *SaturationAnalyzer) noteITL(key string, replicas []domain.ReplicaMetrics,
	variantName string, now time.Time, logger logr.Logger) itl.Model {
	// The lock is analyzer-wide: recordSaturatedThroughput,
	// saturatedThroughputReading and EvictStaleHistory all take it, for every
	// model this instance handles. So it covers the window mutation and
	// nothing else -- the log lines below are emitted after it is dropped,
	// rather than serialising every other model's cycle behind this one's
	// scan, fit and two Info calls.
	a.mu.Lock()
	// Before anything else: this variant has been reported, which is what the
	// sweep ages its learned state on. An empty window is not evidence of a
	// gone variant -- Window.Add admits only a band of k, so a healthy
	// under-utilised fleet holds nothing while reporting every cycle.
	a.noteVariantSeen(key, now)
	w, ok := a.itlWindows[key]
	if !ok {
		w = itl.NewWindow(
			itl.DefaultWindowMaxSize,
			itl.DefaultObservationMaxAge,
			itl.DefaultMinSamples,
			itl.DefaultMinKSpread,
			itl.DefaultMinObservableK,
			itl.DefaultMaxObservableK,
		)
		a.itlWindows[key] = w
	}
	// Logged at DEFAULT, not DEBUG: the deployment passes no -v, so DEBUG (4)
	// never prints and a diagnostic nobody can read is the problem it was
	// written to solve. It is one line per variant per cycle, against the
	// per-REPLICA replica-capacity-decision already logged at this level.
	//
	// Counted per reason, because "the window is empty" has four different
	// causes with four different fixes, and a run that cannot tell them apart
	// costs a rebuild to find out which one it was.
	var considered, notReady, noITL, noK, aboveBand, added int
	for _, rm := range replicas {
		if rm.VariantName != variantName {
			continue
		}
		considered++
		if !rm.Ready || rm.FromWarmPool {
			notReady++
			continue
		}
		if !(rm.AvgITL > 0) {
			noITL++
			continue
		}
		if !(rm.KvUsageInstant > 0) {
			noK++
			continue
		}
		if rm.KvUsageInstant > itl.DefaultMaxObservableK {
			aboveBand++
			continue
		}
		w.Add(rm.KvUsageInstant, rm.AvgITL, now)
		added++
	}
	w.Prune(now)
	// The window's own confidence gate, not itl.Fit's. Fit will draw a line
	// through any two points that are not on top of each other; Ready is
	// what says the points are enough of them and far enough apart in k to
	// mean something (DefaultMinSamples, DefaultMinKSpread). The throughput
	// analyzer gates on it for the same reason, and a derived mu overrides
	// the measured one, so it has to clear a higher bar than two readings.
	obs := w.Observations()
	ready := w.Ready()
	baseline, learned := a.itlBaseline[key]
	a.mu.Unlock()
	if !learned || !(baseline > 0) {
		baseline = itl.DefaultBaselineSec
	}

	// Below DefaultMinObservableK the window drops the reading itself, so
	// `added` counts what was offered and len(obs) what was kept.
	logger.V(logging.DEFAULT).Info("itl-window",
		"variant", variantName, "key", key,
		"replicas", considered, "offered", added, "held", len(obs),
		"notReady", notReady, "noITL", noITL, "noK", noK, "aboveBand", aboveBand,
		"ready", ready, "minSamples", itl.DefaultMinSamples)
	if ready {
		if model, ok := itl.Fit(obs); ok {
			// Remember B, not A. B is the hardware floor and belongs to the
			// card; A is contention and moves with the workload. Only a
			// PHYSICAL floor is worth keeping -- ValidModel admits a negative
			// intercept as long as the line is positive at saturation, and a
			// negative B pinned into the next fit would be worse than the
			// constant it replaced.
			if model.B > 0 {
				a.mu.Lock()
				a.itlBaseline[key] = model.B
				a.mu.Unlock()
			}
			logger.V(logging.DEFAULT).Info("itl-fit", "variant", variantName, "tier", "ols",
				"a", model.A, "b", model.B, "held", len(obs), "baselineLearned", true)
			return model
		}
	}
	// The fleet's replicas are at nearly the same load, so there is no spread
	// in k to fit a slope and an intercept from. That is the NORMAL case, not
	// a degenerate one: the router balances, so it balances k too, and a run
	// on 2026-09-24 sat at queue 0 on every replica but one with the window
	// never once Ready. Requiring the full fit would have left the derivation
	// inert on every fleet that is actually working.
	//
	// So B is pinned to the hardware floor and only A is fitted, which is what
	// the throughput analyzer does for the same reason. It is the weaker
	// answer, and it is still an answer about the shape arriving now, which
	// the measured window it replaces is not.
	//
	// Ready() is two conditions and only one of them is the fleet's fault. The
	// k-SPREAD is what a balanced router cannot supply; the sample COUNT is
	// not, and waiving both was wrong. FitPinnedB answers from a SINGLE pair,
	// and downstream a derived mu both orders replicas (floor.Estimate's
	// mayOrder) and settles the whole variant's shape-change hold
	// (fleetHasMeasuredItself) on sight. Neither should rest on one reading
	// from one replica on one cycle. So the sample floor Ready would have
	// applied still applies here; only the spread requirement is waived.
	//
	// It costs about a cycle: every Ready replica whose k is in range
	// contributes an observation per cycle, so six replicas clear ten in two.
	// It costs nothing in the case this derivation exists for -- once the
	// shape lightens, an over-provisioned fleet sits at a k far below
	// DefaultMinObservableK, contributes no new observations at all, and is
	// priced from the ones the window already holds from when it was working.
	if len(obs) < itl.DefaultMinSamples {
		return itl.Model{}
	}
	// Pinned to what THIS key measured, not to a constant. The slope is fitted
	// against whatever B is pinned, so B decides the line's steepness as much
	// as its offset -- at a pricing point well above the observed k the slope
	// dominates, and a B that is 3x too high halves the ITL the model reports.
	model, ok := itl.FitPinnedB(obs, baseline)
	if !ok {
		logger.V(logging.DEFAULT).Info("itl-fit-declined", "variant", variantName, "held", len(obs))
		return itl.Model{}
	}
	logger.V(logging.DEFAULT).Info("itl-fit", "variant", variantName, "tier", "pinned-B",
		"a", model.A, "b", model.B, "held", len(obs), "baselineLearned", learned)
	return model
}
