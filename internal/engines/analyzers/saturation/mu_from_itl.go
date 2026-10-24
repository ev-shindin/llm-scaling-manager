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
// store's own rule -- LoadFromScaleTarget keeps a live record while the engine
// runs the arguments it was measured on, and folds TotalKvTokensOverride in
// only on the deployment-derived path.
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
//
// ranks is how many engine ranks the replica runs (domain.ReplicaMetrics.Ranks).
// The live reading is already the whole replica's; the flag sizes ONE engine,
// so it is multiplied by the ranks.
func capacityTokensFor(params *capacity.EngineParams, ranks int, liveTokens int64) float64 {
	// math.MaxInt64 is the collector's overflow SENTINEL, not a capacity
	// (internal/collector/attribute.go). Taken literally it makes the resident
	// sequence count astronomical, which prices mu at an absurd figure and --
	// while the check below was a gate -- rejected the replica's line on every
	// cycle forever.
	if liveTokens > 0 && liveTokens < math.MaxInt64 {
		return float64(liveTokens)
	}
	if params != nil && params.TotalKvTokensOverride > 0 {
		return float64(params.TotalKvTokensOverride) * float64(max(ranks, 1))
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
// The cause is not a threshold that needs tuning. The signals are not
// collected over the same window -- KvUsageInstant has none, while
// GenerationTokenRate and AvgITL are both rate[1m] -- so on a ramp k reaches
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
// borrowedFrom is the variant this line was measured on when it is not this
// variant's own, and empty otherwise. Logged because this is the ONLY line
// that fires when a line does not match reality, and the two cases have
// different remedies: a mismatch on a borrowed line says those two
// configurations are not really alike and names who to look at, while a
// mismatch on an own line says this variant's own fit has drifted.
func (a *SaturationAnalyzer) noteLineMismatch(model itl.Model, params *capacity.EngineParams,
	rm domain.ReplicaMetrics, role string, kvReq float64, borrowedFrom string,
	logger logr.Logger) {
	if rm.FromWarmPool || domain.CanonicalRole(role) != domain.RoleDecode {
		return
	}
	capacityTokens := capacityTokensFor(params, rm.Ranks(), rm.TotalKvCapacityTokens)
	errPct, ok := itl.GPSErrorPct(model, rm.KvUsageInstant, capacityTokens,
		kvReq, rm.GenerationTokenRate)
	if !ok || errPct <= itl.DefaultGPSMismatchThresholdPct {
		return
	}
	logger.V(logging.DEFAULT).Info("itl-gps-mismatch",
		"namespace", rm.Namespace, "variant", rm.VariantName, "pod", rm.PodName,
		"k", rm.KvUsageInstant, "observedGPS", rm.GenerationTokenRate,
		"predictedGPS", itl.TokenRate(model, rm.KvUsageInstant, capacityTokens, kvReq),
		"errPct", errPct, "thresholdPct", itl.DefaultGPSMismatchThresholdPct,
		"lineBorrowed", borrowedFrom != "", "from", borrowedFrom,
		"gates", false)
}

// pricingK is the KV utilization, as a fraction of PHYSICAL capacity, that a
// replica is considered full at -- the point mu is priced for.
//
// It is the product of the two thresholds because they compound: k1, the
// memory-bound capacity, is KvCacheThreshold of physical KV, and the analyzer
// scales out at ScaleUpThreshold of k1. On the defaults (0.80, 0.85) that is
// 0.68 of physical; with ScaleUpThreshold at 0.95 it is 0.76. Both sit inside
// the ITL window's band -- the policy's analyzerTuning.itlBand.minObservableK and
// analyzerTuning.itlBand.maxObservableK, 0.15 and 0.80 by default -- so the line is evaluated
// where it was fitted rather than beyond it.
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
	// the default band tops out at 0.80, so returning the fallback unclamped
	// evaluated the line 0.05 outside the range it was fitted over -- the
	// exact thing this function exists to prevent.
	lo, hi := config.DefaultITLMinObservableK, config.DefaultITLMaxObservableK
	if cfg != nil {
		lo, hi = cfg.ITLBand()
	}
	return min(max(k, lo), hi)
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
// max_num_seqs bounds ONE engine, and a data-parallel replica runs ranks of
// them (domain.ReplicaMetrics.Ranks): the replica admits ranks x max_num_seqs.
// The sequence count is the whole replica's, from its whole KV capacity, so the
// cap is scaled to match. Capping a pod of eight ranks at one rank's 256 priced
// it at an eighth of what it serves (issue #150).
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
func deriveMu(model itl.Model, params *capacity.EngineParams, ranks int,
	totalKvTokens int64, fleet shape.Shape, kPrice float64, avgOutput float64) derivedMu {
	if model.IsZero() || !(avgOutput > 0) || !(fleet.KVreq > 0) {
		return derivedMu{}
	}
	if !(kPrice > 0) || kPrice > 1 {
		return derivedMu{}
	}
	capacityTokens := capacityTokensFor(params, ranks, totalKvTokens)
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
		seqs = min(seqs, float64(params.MaxNumSeqs)*float64(max(ranks, 1)))
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

// itlUnderArgs drops key's ITL window and baseline when they were learned
// under engine arguments other than args, the ones its variant runs now, and
// records args for them. A key seen for the first time adopts args, as an
// untagged k2 window does (window_args.go). The baseline is put aside under
// the arguments it was learned under, and a rollback to them gets it back
// (itlBaselineStash); the window is not: the slope refits from new readings
// within it.
func (a *SaturationAnalyzer) itlUnderArgs(key, args string) {
	if args == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tag, ok := a.itlArgs[key]
	a.itlArgs[key] = args
	if !ok || tag == args {
		return
	}
	// The tracker's lock is taken under a.mu here, as EvictStaleHistory takes
	// it: the tracker never calls back into the analyzer.
	if b, ok := a.itlTracker.Baseline(key); ok {
		a.itlBaselineStash[stashKey(key, tag)] = b
	}
	a.itlTracker.Sweep(a.now(), func(k string) bool { return k == key })
	if b, ok := a.itlBaselineStash[stashKey(key, args)]; ok {
		delete(a.itlBaselineStash, stashKey(key, args))
		a.itlTracker.SetBaseline(key, b)
	}
}

// noteITL adds this cycle's (k, ITL) readings for one variant to its rolling
// window and returns the reading: the model fitted over what the window holds,
// and whether it is a slope held for want of readings (SlopeHeld) rather than
// a fit made now.
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
// plus its sleepers, each keeping ~1.4 GiB of GPU residue
// (internal/warmpool/demand.go measured
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
	variantName string, adm itl.Admission, now time.Time, logger logr.Logger) itl.Reading {
	// Stamped here as well as in noteReplicaStart, because this function
	// CREATES learned state (the window, and the baseline the tracker writes
	// on a full fit) and the sweep's rule is that an unstamped key counts as
	// stale. Stamping only in noteReplicaStart made that rule hold by the
	// order two calls happen in a third file -- fitLines calls
	// noteReplicaStart just before the decode guard that reaches this
	// function -- so any early return added to noteReplicaStart, or a
	// reordering of those two calls, would have deleted every learned ITL
	// baseline on the first sweep 24h later, silently, falling back to
	// itl.DefaultBaselineSec. A producer of learned state stamps its own key.
	//
	// The analyzer-wide lock covers the stamp and nothing else: the tracker
	// takes its own around the window, and the scan, the fit and the log
	// lines below are not serialised behind every other model's cycle.
	a.mu.Lock()
	a.noteVariantSeen(key, now)
	a.mu.Unlock()

	// Which rows are readings, the window's confidence gate (Ready: enough
	// samples, far enough apart in k), and the two tiers below are the
	// tracker's: the slo analyzer fits with the same rules.
	r := a.itlTracker.Observe(key, variantName, replicas, adm, now)

	// Logged at DEFAULT, not DEBUG: the deployment passes no -v, so DEBUG (4)
	// never prints and a diagnostic nobody can read is the problem it was
	// written to solve. It is one line per variant per cycle, against the
	// per-REPLICA replica-capacity-decision already logged at this level.
	//
	// Counted per reason, because "the window is empty" has four different
	// causes with four different fixes, and a run that cannot tell them apart
	// costs a rebuild to find out which one it was. Every row is counted
	// under exactly one of them, so they add up to `replicas`. belowBand was
	// the one missing: a single replica at 0.5 to 3 requests/s sat below k
	// 0.15 on nearly every cycle for 24 minutes and logged `offered: 0` with
	// every other count 0, which reads as the window being broken rather than
	// the load being light.
	logger.V(logging.DEFAULT).Info("itl-window",
		"variant", variantName, "key", key,
		"replicas", r.Considered, "offered", r.Offered, "held", r.Held,
		"notReady", r.NotReady, "noITL", r.NoITL, "noK", r.NoK,
		"belowBand", r.BelowBand, "aboveBand", r.AboveBand,
		// Of the offered, the readings above the band taken because their
		// replica was at its sequence cap (itl.Admission); maxNumSeqs is 0
		// when the engine flag did not state it, which admits none.
		"capped", r.Capped, "maxNumSeqs", adm.MaxNumSeqs,
		// Kept, but not for the full fit: the replica's load had just moved,
		// so its k and its ITL describe different moments (itl.DefaultSteadyFor).
		"unsteady", r.Unsteady, "steadyHeld", r.SteadyHeld,
		// Of those, the readings of a replica still inside its settle (it
		// joined, or the load came back to it): left out of the pinned fit
		// as well.
		"settling", r.Settling,
		// Ready needs minSamples steady readings AND this much spread among
		// them; without it a window that never becomes Ready does not say
		// which of the two it lacked.
		"steadySpread", r.SteadySpread, "minKSpread", itl.DefaultMinKSpread,
		"ready", r.Ready, "minSamples", itl.DefaultMinSamples)
	switch {
	case r.Tier == itl.TierOLS:
		logger.V(logging.DEFAULT).Info("itl-fit", "variant", variantName, "tier", "ols",
			"a", r.Model.A, "b", r.Model.B, "held", r.Held, "baselineLearned", true)
	case r.Tier == itl.TierPinnedB:
		// The fleet's replicas are at nearly the same load, so there is no
		// spread in k to fit a slope and an intercept from. That is the
		// NORMAL case, not a degenerate one: the router balances, so it
		// balances k too, and a run on 2026-09-24 sat at queue 0 on every
		// replica but one with the window never once Ready. Requiring the
		// full fit would have left the derivation inert on every fleet that
		// is actually working. So B is pinned to what THIS key measured and
		// only A is fitted -- the weaker answer, and still one about the
		// shape arriving now. The slope is fitted against whatever B is
		// pinned, so B decides the line's steepness as much as its offset.
		//
		// Downstream a derived mu both orders replicas (floor.Estimate's
		// mayOrder) and settles the whole variant's shape-change hold
		// (fleetHasMeasuredItself) on sight, which is why the tracker keeps
		// the sample floor Ready would have applied: only the spread is
		// waived. It costs about a cycle -- six replicas clear ten in two --
		// and nothing in the case this exists for: once the shape lightens,
		// an over-provisioned fleet sits at a k far below
		// DefaultMinObservableK and is priced from the readings the window
		// already holds from when it was working -- and, once those have aged
		// out, from the key's last slope (slopeHeld), so the line outlives a
		// long light stretch (issue #284).
		logger.V(logging.DEFAULT).Info("itl-fit", "variant", variantName, "tier", "pinned-B",
			"a", r.Model.A, "b", r.Model.B, "held", r.Held, "baselineLearned", r.BaselineStored,
			"slopeHeld", r.SlopeHeld, "slopeAge", r.SlopeAge(now))
	case r.Declined:
		logger.V(logging.DEFAULT).Info("itl-fit-declined", "variant", variantName, "held", r.Held)
	}
	return r
}
