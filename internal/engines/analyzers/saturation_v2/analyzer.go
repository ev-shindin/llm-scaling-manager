package saturation_v2

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/fleet"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// SaturationAnalyzer implements the domain.Analyzer interface using a
// token-based capacity model with memory-bound (k1) and compute-bound (k2)
// constraints. It is the sole saturation analysis path.
type SaturationAnalyzer struct {
	// mu protects computeCapacityHistory from concurrent access.
	mu sync.Mutex
	// computeCapacityHistory stores rolling averages of observed k2 values,
	// keyed by "modelID|accelerator|gpuCount|role|outputBucket|queueThreshold".
	// TODO: check if we need to use other model parameters as key in the future.
	computeCapacityHistory map[string]*capacity.RollingAverage
	// lastAccelerator remembers, per variant, the last accelerator that actually
	// RESOLVED.
	//
	// A workload that pins no GPU product has its accelerator observed from the
	// nodes its pods landed on (variantmeta.observeAcceleratorFromNodes), and that
	// observation REQUIRES AGREEMENT: pods on two different products report "no
	// single answer" and the variant reads unresolved. That is the right
	// accounting answer -- charging a spread fleet to one pool is the bug the
	// removed acceleratorName label used to cause -- but it makes the value change
	// with pod PLACEMENT, and the accelerator is part of the k2 history key.
	//
	// On a heterogeneous cluster the result is a self-sustaining oscillation, and
	// this is measured rather than theorised. A kind cluster labelled
	// NVIDIA-H100-SXM5-80GB on one node and NVIDIA-A100-PCIE-80GB on another: at
	// one replica the fleet is single-type and resolves, so k2 comes from history;
	// the scale-up puts the second replica on the other product, resolution
	// reports no single answer, the key moves to an empty bucket, k2 falls back to
	// k1 and capacity jumps 2 -> 8; the variant then scales back DOWN, which
	// restores single-type placement and the cycle repeats. Scaling up is what
	// destroys the deduction; losing the deduction is what causes the scale-down.
	//
	// Keying history on the last resolved accelerator breaks that loop without
	// touching the accounting: this is a cache key for a capacity estimate, not a
	// statement about which pool owns the GPUs. The cost is that a genuinely mixed
	// fleet averages observations from two hardware types under one key, which is
	// coarse -- and still strictly better than alternating between a real estimate
	// and k1 every time a replica lands elsewhere.
	lastAccelerator map[string]acceleratorMemo
	// saturatedThroughput stores rolling averages of the per-replica completion
	// rate (requests/s) observed while the replica's queue was saturated, keyed
	// exactly like computeCapacityHistory. It is what the throughput floor
	// (signals/floor, applied in throughput_floor.go) divides the arrival rate
	// by: k2 says how much KV a
	// saturated replica HOLDS, this says how fast it COMPLETES, and only the
	// latter tells how many replicas a given arrival rate needs.
	saturatedThroughput map[string]*capacity.RollingAverage
	// throughputSampledAt is, per saturatedThroughput key, when the window
	// last took a reading as a sample of its own, and throughputLastRead the
	// last reading it was given, sample or not (recordSaturatedThroughput);
	// both swept with the window.
	throughputSampledAt map[string]time.Time
	throughputLastRead  map[string]float64
	capacityStore       *capacity.Store

	// decodeSaturatedAt is, per namespace|model, the last cycle a decode
	// replica was seen full and queued (roleSaturated). Prefill's readings
	// are treated as decode's for DecodeSaturationMemory after it
	// (rememberDecodeSaturation); swept by EvictStaleHistory beside the
	// history, one time.Time per model that ever saturated.
	decodeSaturatedAt map[string]time.Time
	// fleetShape is the (I, O) bucket pair last seen per namespace|model,
	// and when a change of either was raised and not yet settled. See
	// shape_change.go for what the event is for.
	fleetShape map[string]*shapeMemo

	// itlWindows is one rolling window of (k, ITL) readings per variant, from
	// which ITL(k) = A*k + B is fitted so mu can be derived for the shape the
	// fleet is serving NOW rather than waiting for it to saturate under it
	// (mu_from_itl.go). Keyed like the throughput windows and swept with them.
	itlWindows map[string]*itl.Window
	// itlBaseline is the last B an OLS fit produced for each window key: the
	// zero-contention decode step for THAT model on THAT accelerator. It is
	// what the one-parameter fallback pins, so a fleet that has once been
	// measured never falls back to a constant guessed for another card.
	itlBaseline map[string]float64
	// startSeconds is the running estimate of how long one replica of each
	// variant takes to become Ready, keyed like the ITL windows and swept with
	// them. The demand floor projects the backlog forward over it, so it is a
	// measurement -- see start_est.go for why a constant cannot do.
	startSeconds map[string]float64
	// startSeenPods is the Pods whose start has already been folded into that
	// estimate, with when they were seen.
	//
	// Every cycle sees the same Ready Pod reporting the same StartSeconds, so
	// without this the estimate would converge on whatever the longest-lived
	// replica measured and the histogram would count cycles rather than starts.
	startSeenPods map[string]time.Time
	// startOutliers counts consecutive start-time samples rejected as
	// implausible for a variant, so a genuine change of hardware is eventually
	// admitted rather than rejected forever against a stale estimate.
	startOutliers map[string]int
	// now is the clock the memory reads; tests set it.
	now func() time.Time
}

// acceleratorMemo is the last accelerator that resolved for a variant, with the
// time it was last used -- so it can age out on the same timeout as the k2
// history it keys.
type acceleratorMemo struct {
	name     string
	lastUsed time.Time
}

// NewSaturationAnalyzer creates a new V2 saturation analyzer backed by the
// given capacity store.
func NewSaturationAnalyzer(store *capacity.Store) *SaturationAnalyzer {
	return &SaturationAnalyzer{
		computeCapacityHistory: make(map[string]*capacity.RollingAverage),
		lastAccelerator:        make(map[string]acceleratorMemo),
		saturatedThroughput:    make(map[string]*capacity.RollingAverage),
		throughputSampledAt:    make(map[string]time.Time),
		throughputLastRead:     make(map[string]float64),
		capacityStore:          store,
		decodeSaturatedAt:      make(map[string]time.Time),
		fleetShape:             make(map[string]*shapeMemo),
		itlWindows:             make(map[string]*itl.Window),
		itlBaseline:            make(map[string]float64),
		startSeconds:           make(map[string]float64),
		startSeenPods:          make(map[string]time.Time),
		startOutliers:          make(map[string]int),
		now:                    time.Now,
	}
}

// Name returns the analyzer identifier for logging and result metadata.
// Note: the config value "saturation" (in analyzerName YAML field) selects this analyzer,
// but the descriptive name here is used in AnalyzerResult.AnalyzerName for observability.
func (a *SaturationAnalyzer) Name() string {
	return "saturation-token-based"
}

// EvictStaleHistory removes k2 history entries that have not been updated
// within the given timeout. This prevents unbounded memory growth from
// deleted models or workload buckets that are no longer active.
//
// It prunes the accelerator memo on the same timeout, and here rather than in a
// second sweep so the two cannot drift: both are per-variant state that exists
// only to key or stabilise capacity, and a variant that has gone quiet for the
// timeout has no use for either. The saturated-throughput windows and the
// decode-saturation memory go the same way -- the latter is per
// namespace|model rather than per variant, but a model quiet for the timeout
// has no use for it either. The returned count remains the number of
// HISTORY entries evicted, which is what its callers report.
func (a *SaturationAnalyzer) EvictStaleHistory(timeout time.Duration) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	evicted := 0
	for key, ra := range a.computeCapacityHistory {
		if ra.Stale(timeout) {
			delete(a.computeCapacityHistory, key)
			evicted++
		}
	}
	for key, memo := range a.lastAccelerator {
		if time.Since(memo.lastUsed) > timeout {
			delete(a.lastAccelerator, key)
		}
	}
	// An ITL window ages by its own observations rather than by a timestamp
	// of its own: Prune drops readings past DefaultObservationMaxAge, so a
	// window left empty by that belongs to a variant nothing has reported for
	// at least that long. Without this the map keeps one window per variant
	// that has EVER been seen, including deleted and renamed ones.
	now := a.now()
	for key, w := range a.itlWindows {
		w.Prune(now)
		if w.Len() == 0 {
			delete(a.itlWindows, key)
			// The learned baseline dies with the window that produced it.
			// Kept, it would grow one float per variant/accelerator ever seen
			// and, worse, pin a hardware floor measured before a redeploy onto
			// different hardware into every later fit for that key.
			delete(a.itlBaseline, key)
			// And the start estimate, for the same reason: a figure measured
			// before a redeploy onto different hardware would otherwise size
			// every later projection for that key.
			delete(a.startSeconds, key)
			delete(a.startOutliers, key)
		}
	}
	a.evictStartSeenPods(now, timeout)
	for key, ra := range a.saturatedThroughput {
		if ra.Stale(timeout) {
			delete(a.saturatedThroughput, key)
			delete(a.throughputSampledAt, key)
			delete(a.throughputLastRead, key)
		}
	}
	for key, at := range a.decodeSaturatedAt {
		if time.Since(at) > timeout {
			delete(a.decodeSaturatedAt, key)
		}
	}
	// The fleet-shape memo is per namespace|model and exists only to key and
	// stabilise the capacity figures, so it goes when they do. Without this it
	// is the one map on this struct that grows without bound as models and
	// namespaces come and go.
	//
	// maps.DeleteFunc rather than the hand-rolled loops above: those predate
	// it and two of them cannot use it anyway, one because it counts what it
	// evicts and one because it deletes from three maps on the same key.
	maps.DeleteFunc(a.fleetShape, func(_ string, memo *shapeMemo) bool {
		return time.Since(memo.lastSeen) > timeout
	})
	return evicted
}

// rememberDecodeSaturation records that decode was seen full and queued this
// cycle when it was, and reports whether decode counts as saturated for
// prefill's sake: now, or within DecodeSaturationMemory of the last time.
func (a *SaturationAnalyzer) rememberDecodeSaturation(namespace, modelID string, saturatedNow bool) bool {
	key := namespace + "|" + modelID
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if saturatedNow {
		a.decodeSaturatedAt[key] = now
		return true
	}
	last, ok := a.decodeSaturatedAt[key]
	return ok && now.Sub(last) < DecodeSaturationMemory
}

// Analyze computes capacity signals for a model across all its variants.
func (a *SaturationAnalyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	satConfig, ok := input.Config.(*config.ScalingPolicy)
	if !ok {
		return nil, fmt.Errorf("expected *ScalingPolicy, got %T", input.Config)
	}
	logger := ctrl.LoggerFrom(ctx)

	// Build GPU count and P/D role lookups from variant states. The role decides
	// how a waiting request is charged against a replica's KV capacity, so it must
	// be known before per-replica demand is computed.
	// Accelerator joins these two: it is variant-level identity from discovery, not
	// a per-instance measurement, so it is read from the variant state rather than
	// repeated on every ReplicaMetrics record.
	gpusByVariant := make(map[string]int, len(input.VariantStates))
	rolesByVariant := make(map[string]string, len(input.VariantStates))
	accelByVariant := make(map[string]string, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		gpusByVariant[vs.VariantName] = vs.GPUsPerReplica
		rolesByVariant[vs.VariantName] = vs.Role
		accelByVariant[vs.VariantName] = vs.AcceleratorName
	}

	// Whether the decode role is saturated this cycle decides what a
	// saturated PREFILL replica is evidence of (computeK2): a prefill request
	// completes only when decode admits it, so while decode is full and
	// queued, prefill's queue and its held KV are decode's backlog seen from
	// upstream, and neither its occupancy nor its completion rate is a
	// reading of prefill. Decided once, over every decode row, before any
	// replica is priced -- the order the rows arrive in must not matter --
	// and remembered for the collector's row window, because prefill's row
	// is a one-minute max that can outlive decode's
	// (rememberDecodeSaturation).
	decodeSaturated := a.rememberDecodeSaturation(input.Namespace, input.ModelID,
		roleSaturated(input.ReplicaMetrics, rolesByVariant, domain.RoleDecode, satConfig.QueueLengthThreshold, satConfig.KvCacheThreshold))

	// The output length the fleet is serving this cycle, once, for every
	// replica's throughput key (computeReplicaCapacity says why the key is
	// the fleet's shape and not the replica's).
	fleetOutput := fleetOutputLength(input.ReplicaMetrics, rolesByVariant)
	// The prefill side's hit rate, once for the role, for the same reason:
	// it discounts the prompt length that buckets prefill's throughput key,
	// and a per-replica figure would split one window between replicas.
	fleetHitRate := fleetPrefixHitRate(input.ReplicaMetrics, rolesByVariant)
	// The other axis, and the event. The prompt length arriving reads the
	// switch within a scrape of it, where the output half waits for a
	// completion; a change on either says the learned figures describe a
	// workload that is no longer running (shape_change.go).
	fleetInput := servedPromptLength(input.ReplicaMetrics)
	arriving, arrivingOK := arrivingPromptLength(input.SchedulerQueue)
	holdFor, _ := satConfig.ShapeChangeHold(ShapeChangeHoldMax)
	stableOutput, stableInput, shapeChanged := a.noteFleetShape(input.Namespace, input.ModelID,
		fleetInput, fleetOutput, arriving, arrivingOK, holdFor, logger)

	// The derived mu's divisor. The SHORT window is right only WHILE A SHAPE
	// CHANGE IS OUTSTANDING, which is the case it was added for: a [5m]
	// count-weighted mean carries the previous shape's long stragglers for
	// minutes after they stop arriving -- measured decaying 3750 -> 250 across
	// one 6000 -> 250 switch -- and the divisor is where that error reaches mu
	// undamped.
	//
	// On a STEADY shape it is wrong, and expensively so. Phase 1 of the
	// shape-swap trace holds 6000-token generations in flight for about two
	// minutes before any of them completes, so a [1m] mean over COMPLETED
	// requests reads far below 6000 while the fleet ramps. rate =
	// tokenSec/avgOutput then OVER-states mu, and an over-stated mu
	// UNDER-orders replicas (mu_from_itl.go says so explicitly).
	//
	// Bisected over five runs on 2026-10-03, EPP version, EPP config,
	// max_num_seqs and cluster held identical with the WVA image the only
	// variable. The unconditional short window took phase 1 from a 305-request
	// router queue at 33,071 output tok/s to 2,503 at 8,763 -- eight times the
	// queue for a quarter of the throughput. Every build before it measured
	// good.
	//
	// max(recent, [5m]) is NOT the fix, and that is worth recording because it
	// is the obvious one: the recent figure is the LOWER of the two in both
	// cases -- an artefact while ramping, the truth after a switch -- so no
	// magnitude test can separate them. Whether the shape changed can.
	// Keyed on shapeChangedWithin, NOT on the outstanding hold. The hold's flag
	// is zero whenever an operator sets DisableShapeChangeHold, which would
	// leave this reading the short window for the single cycle the tracker
	// declares a change and the [5m] mean for the rest of the straggler window
	// -- reinstating the bug above through a flag documented as only turning
	// off the fleet hold. ShapeChangeWindow says why the two are separate.
	// The divisor falls back the same way the queue's price does, and for the
	// same reason: rate = tokenSec/avgOutput, so a fleet that has completed
	// nothing divides by ZERO and the derived mu reports not-ok. With no mu the
	// demand floor emits nothing at all -- and the floor is where the router
	// queue is projected forward over a replica's start time
	// (floor.backlogAtLanding). So the one window the projection exists for is
	// the one window it could not run in.
	//
	// Measured on run QM (2026-10-03, shape-swap phase 1): the first queued
	// cycle was 14:39:44 and the first throughput-demand-floor line 14:41:59 --
	// 135 s later, by which time the router queue had already peaked at 522 and
	// begun draining. Of 56 not-ok derived-mu cycles, 38 carried a COMPLETE
	// ITL fit (itlA 0.0277, itlB 0.0066, itlAtKPrice 0.0254) and failed on
	// "avgOutputTokens": 0 alone.
	//
	// A measurement still wins, so a warm fleet is unaffected; this only answers
	// where there was otherwise a zero. The error direction is also the safe one
	// for a seed that is too LARGE: a bigger divisor under-states mu, and an
	// under-stated mu over-orders during the cold window rather than
	// under-ordering, which is the failure this is for.
	muDivisor := satConfig.ExpectedOutputTokens(fleetOutput, stableOutput, DefaultExpectedOutputTokens)
	// muInput is the prompt length the same mu is priced at, and it moves to
	// the short window with the divisor or not at all.
	//
	// deriveMu reads the output length twice over: once as the divisor of
	// rate = tokenSec/avgOutput, and once inside the shape, where KVreq =
	// ILeff + OL/2 sets how many sequences fit. Moving only the divisor to the
	// short window leaves the two halves of that division on different
	// timescales, which is at its worst exactly here: a swap that raises the
	// generation and drops the prompt has the divisor reach the new output in
	// about a minute while KVreq still carries the old prompt, so the priced
	// request is larger than either shape ever was and mu collapses.
	//
	// Measured on run QS (2026-10-04, 6000/1000 -> 1000/4000, a scenario whose
	// documented answer is 2 decode replicas then 3): across the switch the
	// divisor went 1088 -> 2897 -> 4000 while kvReq lagged 6251 -> 6030 ->
	// 5896 toward a settled 3000, and the derived mu went 5.47 -> 2.12 -> 1.57
	// against the 2.76 it settled at. The floor read replicasImplied 5.77 and
	// the fleet sat at 7 decode replicas for about seven minutes. The router
	// queue was ZERO on every one of those cycles, so none of it was a backlog
	// response -- it was the price.
	//
	// Both halves or neither. If an engine publishes the short-window output
	// but not the short-window prompt the divisor still moves alone, which is
	// this mismatch -- but the alternative is the straggler bug the short
	// window exists for, and that one is the larger error by an order of
	// magnitude (3750 against 250). vLLM and SGLang both publish the pair, so
	// the single-sided path is the engine-has-no-counter case, not a race.
	muInput, muShortWindow := fleetInput, false
	if a.shapeChangedWithin(input.Namespace, input.ModelID,
		satConfig.ShapeChangeWindow(ShapeChangeHoldMax), time.Now()) {
		if recent := fleetOutputLengthRecent(input.ReplicaMetrics, rolesByVariant); recent > 0 {
			muDivisor = recent
			if recentIn := servedPromptLengthRecent(input.ReplicaMetrics); recentIn > 0 {
				muInput, muShortWindow = recentIn, true
			}
		}
	}

	// One ITL(k) fit per variant per cycle, from readings its replicas report
	// at whatever load they are at. This is what lets mu be priced for the
	// shape arriving now instead of the one the fleet last saturated under
	// (mu_from_itl.go).
	itlModels := make(map[string]itl.Model, len(gpusByVariant))
	for variant := range gpusByVariant {
		// Decode only. ITL is the latency between GENERATED tokens, and
		// deriveMu divides a token rate by an output length; prefill emits
		// about one token per request -- its work is the prompt -- so the
		// same arithmetic would read three orders of magnitude low, which is
		// why saturatedCompletionRate special-cases it on the measured path
		// too (shape_change.go).
		// How long this variant's replicas take to become Ready, folded in from
		// whatever finished starting since the last cycle.
		//
		// BEFORE the decode guard, deliberately. That guard is about ITL -- the
		// latency between generated tokens, which prefill barely has -- while a
		// start time is an image and a node, which prefill has exactly like
		// decode. A P/D fleet projects its prefill backlog too, and leaving
		// prefill out meant it published neither series, so a run could not even
		// be reviewed for it.
		a.noteReplicaStart(a.itlWindowKey(input.Namespace, input.ModelID, variant,
			accelByVariant[variant], gpusByVariant[variant]),
			input.Namespace, variant, input.ReplicaMetrics, logger)

		if canonicalRole(rolesByVariant[variant]) != domain.RoleDecode {
			continue
		}
		// Keyed by what ITL(k) is a property of: the accelerator and how many
		// of them a replica has. Pooling two GPU products under one key is the
		// bug stableAccelerator exists to prevent for k2 (the k1<->k2
		// oscillation of PR #40 on a heterogeneous cluster), and a blended
		// ITL line is meaningless for either product.
		key := a.itlWindowKey(input.Namespace, input.ModelID, variant,
			accelByVariant[variant], gpusByVariant[variant])
		itlModels[variant] = a.noteITL(key, input.ReplicaMetrics, variant, a.now(), logger)
		// How long this variant's replicas take to become Ready, folded in from
		// whatever finished starting since the last cycle. Same key as the ITL
		// window, so it is swept with it.
	}

	// Phase 1: Per-replica capacity computation
	replicaCapacities := make([]capacity.ReplicaCapacity, 0, len(input.ReplicaMetrics))
	for _, rm := range input.ReplicaMetrics {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		gpuCount := gpusByVariant[rm.VariantName]
		role := rolesByVariant[rm.VariantName]
		downstreamSaturated := decodeSaturated && canonicalRole(role) == domain.RolePrefill
		// Priced, then reported on. noteLineMismatch says when the observed
		// generation-token rate disagrees with the line, and does not act on
		// it: see its comment for the run that decided that.
		itlModel := itlModels[rm.VariantName]
		engineParams := engineParamsFor(a, input.Namespace, input.ModelID, rm.VariantName)
		fleetShape := shape.New(fleetInput, fleetOutput, rm.PrefixCacheHitRate)
		// muShape is the shape mu is PRICED at: fleetShape on a steady fleet,
		// and both halves on the short window while a shape change is
		// outstanding (see muInput above). fleetShape stays the fleet's own
		// [5m] reading, because that is the figure noteLineMismatch compares
		// an observed token rate against.
		muShape := fleetShape
		if muShortWindow {
			muShape = shape.New(muInput, muDivisor, rm.PrefixCacheHitRate)
		}
		// Only a generating role is priced from the ITL line. ITL is the gap
		// between two generated tokens, and a prefill replica emits one and
		// hands the KV to decode, so tokenSec/avgOutput does not describe it.
		//
		// This has always held, but only by accident: itlModels above is
		// populated for RoleDecode alone, so a prefill lookup returned the
		// zero Model and deriveMu declined on IsZero() 250 lines away, keyed
		// on the opposite condition. A role-classification slip, or anyone
		// legitimately extending that population to RoleBoth, would have
		// started pricing prefill from decode's physics with nothing to say
		// so. Stated here instead, beside the call it governs.
		var derived derivedMu
		if canonicalRole(role) == domain.RoleDecode {
			kPrice := pricingK(satConfig)
			derived = deriveMu(itlModel, engineParams, rm.TotalKvCapacityTokens,
				muShape, kPrice, muDivisor)
			// Every term, because the result alone cannot be attributed to one.
			// A derived mu that is wrong by 8x looks identical in the log
			// whether the fault is the output length, the sequence count, the
			// ITL line or the pricing point -- and that ambiguity cost four
			// discarded diagnoses in one session. maxNumSeqs is reported
			// separately from seqs so the cap is visible when it binds.
			var maxSeqs int64
			if engineParams != nil {
				maxSeqs = engineParams.MaxNumSeqs
			}
			logger.V(logging.DEFAULT).Info("derived-mu",
				"variant", rm.VariantName, "pod", rm.PodName,
				"ok", derived.ok,
				"rate", derived.rate, "seqs", derived.seqs, "tokenSec", derived.tokenSec,
				"kPrice", kPrice, "itlAtKPrice", itlModel.ITLAt(kPrice),
				"itlA", itlModel.A, "itlB", itlModel.B, "itlZero", itlModel.IsZero(),
				"avgOutputTokens", fleetShape.AvgOutputTokens,
				"muDivisor", muDivisor,
				"avgInputTokens", fleetShape.AvgInputTokens,
				"muInputTokens", muInput,
				"muShortWindow", muShortWindow,
				"kvReqPerSeq", muShape.KVreq,
				"kvReqFleet", fleetShape.KVreq,
				"replicaKvTokens", rm.TotalKvCapacityTokens,
				"maxNumSeqs", maxSeqs)
		}
		a.noteLineMismatch(itlModel, engineParams, rm, role, fleetShape.KVreq, logger)
		rc := a.computeReplicaCapacity(rm, satConfig, input.ModelID, input.Namespace, gpuCount,
			role, accelByVariant[rm.VariantName], stableOutput, fleetOutput, stableInput,
			fleetHitRate, derived, downstreamSaturated, logger)
		if rc != nil {
			replicaCapacities = append(replicaCapacities, *rc)
		}
	}

	// A replica reading a throughput window of its OWN, rather than a
	// neighbouring bucket's, is the fleet having measured itself under the
	// shape now arriving -- which is what an outstanding shape change was
	// waiting for (shape_change.go).
	if shapeChanged {
		measured := fleetHasMeasuredItself(replicaCapacities)
		a.settleFleetShape(input.Namespace, input.ModelID, measured)
		shapeChanged = !measured
	}

	// Phase 2: Per-variant aggregation
	variantCapacities := a.aggregateByVariant(replicaCapacities, input.ReplicaMetrics, input.VariantStates, input.ModelID, input.Namespace, satConfig.KvCacheThreshold, logger)

	// Model-level demand D (the analyzer owns demand attribution). Supply,
	// utilization, and RoleCapacities are assembled downstream by the engine's
	// capacity-build step from the per-variant capacities and RoleDemand, so they
	// are not set here — the analyzer emits only the measured (D, P) signal.
	totalDemand := aggregation.SumTotalDemand(variantCapacities)

	// Track active roles for queue demand attribution.
	activeRoles := make(map[string]bool)
	for _, vc := range variantCapacities {
		activeRoles[canonicalRole(vc.Role)] = true
	}

	// Add scheduler queue demand (requests queued upstream in llm-d flow control).
	// Price the queue at an output length the fleet has, recalls, or was told
	// -- in that order -- so a cold fleet does not value a growing queue at
	// zero. fleetOutput is this cycle's measurement and wins whenever it
	// exists; stableOutput is what this fleet last knew and carries across an
	// idle period; the rest is configuration.
	expectedOutput := satConfig.ExpectedOutputTokens(fleetOutput, stableOutput, DefaultExpectedOutputTokens)
	queueMetrics := withExpectedOutputTokens(input.ReplicaMetrics, rolesByVariant, expectedOutput)
	queueDemand := estimateSchedulerQueueDemand(input.SchedulerQueue, queueMetrics, rolesByVariant, activeRoles,
		fleetHitRate)
	totalDemand += queueDemand.total
	if input.SchedulerQueue != nil {
		logger.Info("scheduler-queue-demand",
			"modelID", input.ModelID, "namespace", input.Namespace,
			"eppQueueSize", input.SchedulerQueue.QueueSize, "eppQueueBytes", input.SchedulerQueue.QueueBytes,
			"estimatedTokens", queueDemand.total, "byRole", queueDemand.byRole)
	}

	// Per-role demand attribution (P/D disaggregation); nil when non-disaggregated.
	// The builder pairs this with the per-role supply it recomputes.
	roleDemand := a.aggregateRoleDemand(variantCapacities, queueDemand.byRole)

	// Floor the demand at what the load requires in THROUGHPUT
	// (signals/floor, applied in throughput_floor.go).
	//
	// Everything above measures occupancy, which falls as capacity rises: a
	// fleet that is keeping up looks idle, and the target follows the signal
	// down. The floor divides the arrival rate by what a saturated replica of
	// each role was seen to complete -- a per-replica constant that does not
	// move when replicas are added -- so it holds the fleet at the size the
	// LOAD implies once occupancy stops implying anything.
	//
	// Strictly a floor: it never lowers demand. It does order -- lambda / mu
	// against a fleet that is short is the earliest signal there is, some 50 s
	// ahead of occupancy on the measured runs -- and it prices a backlog as
	// work to drain within BacklogDrainSeconds rather than as KV that must be
	// resident at once, which is what turned a 350-request queue into five to
	// seven extra replicas.
	//
	// There used to be a second floor here, from Little's law on the service
	// time the engines report. It was retired: service time is ITL x output
	// length and ITL grows with the batch, so the floor priced the same load at
	// 2.5M tokens on two replicas and 450k on four, ordered replicas while the
	// fleet was behind and released them once they arrived -- a positive
	// feedback loop, measured as a 2-4 replica oscillation with a ten-minute
	// period (docs/proposals/backlog-sizing.md, "Phase 2"). Worse, it ordered
	// them BEFORE any replica reached saturation, so the throughput this floor
	// depends on was never observed. The evidence, and what this floor does
	// that one could not, are in docs/developer-guide/saturation-demand-floor.md.
	var eppQueued float64
	if input.SchedulerQueue != nil {
		eppQueued = float64(input.SchedulerQueue.QueueSize)
	}
	totalDemand = a.applyThroughputFloor(input, satConfig, replicaCapacities, variantCapacities,
		totalDemand, roleDemand, queueDemand.byRole, eppQueued, shapeChanged, logger)

	// While decode is saturated, prefill's DEMAND is not a reading of prefill
	// either: the KV it holds and the queue behind it are decode's backlog
	// (computeK2 explains the mechanism), and the gate above only keeps them
	// out of what is LEARNED. Left in the demand they still act on the
	// decision -- against a k2 prefill learned on some earlier, genuine
	// saturation (small, as a compute bound is) the held KV alone reads as
	// several replicas, and against k1 on a fleet of two or more it reads as
	// a release: 537 800 on a supply of 1 839 718 is 29 %, one replica
	// removed while decode is saturated and re-ordered when it recovers. So
	// the role's demand is clamped into the band where the engine neither
	// orders nor releases (RC = 0 and SC = 0 under applyUniversalThreshold):
	// prefill keeps what it has until decode's numbers are its own again:
	// for as long as any decode replica's one-minute peak reads full and
	// queued, plus the memory -- 195 s and two decode starts replayed on
	// the measured cold pass, 135 s and one on the warm; unbounded while
	// decode is capped and cannot grow. The cycle decode recovers, prefill's
	// own occupancy and floor
	// stand. What the floor of the band buys is not a re-order avoided --
	// a released prefill replica would stay released, prefill reads near
	// zero after an episode -- but the burst that ends one: the scheduler's
	// flow control releases what it held in one go (124 requests, 744k
	// prompt tokens on a 919k k1, on the measured run), and it lands on
	// prefill first. Only the disaggregated case has a prefill
	// entry to hold; the model-level total moves by the same amount so
	// RoleDemand and TotalDemand keep moving together.
	if decodeSaturated && roleDemand != nil {
		scaleUp, scaleDown := satConfig.AnalyzerThresholds(domain.SaturationAnalyzerName)
		// The scheduler queue's share is exempt: those requests have been
		// given to no pod and have had no first token, so prefill is what
		// they are waiting for whatever decode is doing.
		undispatched := queueDemand.byRole[domain.RolePrefill]
		if h, held := holdPrefillDemand(roleDemand, variantCapacities, scaleUp, scaleDown, undispatched); held {
			totalDemand += h.after - h.before
			logger.Info("prefill-demand-held",
				"modelID", input.ModelID, "namespace", input.Namespace,
				"demandBefore", h.before, "demandHeld", h.after, "holdFloor", h.lo, "holdCap", h.hi,
				"undispatched", undispatched,
				"reason", "decode saturated: the KV prefill holds and the queue behind it are decode's backlog; prefill is neither ordered nor released on them, but the scheduler queue's share is not held -- no pod has started those")
		}
	}

	// The fleet is not released on figures the switch made stale. A floor,
	// not a clamp: an I-up switch genuinely needs more capacity, and
	// occupancy and the throughput floor still order (shape_change.go).
	if shapeChanged && roleDemand != nil {
		_, scaleDown := satConfig.AnalyzerThresholds(domain.SaturationAnalyzerName)
		if moved, raised := holdFleetFloor(roleDemand, variantCapacities, scaleDown); moved > 0 {
			totalDemand += moved
			logShapeHold(logger, input.ModelID, input.Namespace, moved, raised)
		}
	}

	result := &domain.AnalyzerResult{
		AnalyzerName:      a.Name(),
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        time.Now(),
		VariantCapacities: variantCapacities,
		TotalDemand:       totalDemand,
		RoleDemand:        roleDemand,
	}

	return result, nil
}

// computeReplicaCapacity computes the capacity breakdown for a single replica.
// The role argument is the replica's P/D role, which determines how requests
// waiting in the local engine queue are charged (see waitingQueueDemand).
// An empty role is treated as domain.RoleBoth. downstreamSaturated says the
// role this replica hands its requests to is saturated this cycle, which
// makes the replica's own saturation not a reading of it (computeK2); it is
// only ever true for prefill.
// Returns nil if the replica has no V2 capacity data (TotalKvCapacityTokens == 0).
func (a *SaturationAnalyzer) computeReplicaCapacity(
	rm domain.ReplicaMetrics,
	config *config.ScalingPolicy,
	modelID, namespace string,
	gpuCount int,
	role string,
	accelerator string,
	// shapeKeyOutput is the TRACKED output length: hysteretic, so the window's
	// bucket does not flip when the fleet's average wobbles across a boundary.
	shapeKeyOutput float64,
	// fleetOutput is the output length the fleet is serving RIGHT NOW. A
	// bucket label wants hysteresis; a physical quantity does not, and mu is
	// divided by this one.
	fleetOutput float64,
	// shapeKeyInput is the TRACKED prompt length, hysteretic for the same
	// reason shapeKeyOutput is: it buckets the throughput key, and a fleet
	// whose average wobbles across a boundary must not split its window in two.
	shapeKeyInput float64,
	// fleetHitRate is the PREFILL ROLE's prefix-cache hit rate, one figure for
	// the whole role this cycle. It discounts shapeKeyInput into the prompt
	// length prefill actually computes. The replica's own rate is deliberately
	// not used: it would put two replicas of one variant in different input
	// buckets in the same cycle (fleetPrefixHitRate).
	fleetHitRate float64,
	// derived is mu priced from this variant's fitted ITL model, which needs
	// no saturated cycle and no bucket. It is preferred over the measured
	// window when present (mu_from_itl.go).
	derived derivedMu,
	downstreamSaturated bool,
	logger logr.Logger,
) *capacity.ReplicaCapacity {
	if rm.TotalKvCapacityTokens <= 0 {
		// TODO: implement proper demand estimation when vllm:cache_config_info is absent.
		// Currently we fall back to percentage-based demand using the deployment-derived
		// capacity from the capacity store. A better approach would be to estimate
		// TotalKvCapacityTokens from deployment args (num_gpu_blocks_override, block_size)
		// or use a dedicated percentage-based demand signal.
		return a.computeReplicaCapacityFallback(rm, config, modelID, namespace, role, accelerator, logger)
	}

	// Compute demand: tokens already resident in KV cache plus the role-aware
	// footprint of requests still waiting in the local engine queue.
	localQueueDemand := waitingQueueDemand(rm, role)
	replicaDemand := rm.TokensInUse + localQueueDemand

	// k1: memory-bound capacity
	k1 := memoryBound(rm.TotalKvCapacityTokens, config.KvCacheThreshold)

	// k2: compute-bound capacity
	var engineParams *capacity.EngineParams
	if rec := a.capacityStore.Get(namespace, modelID, rm.VariantName); rec != nil {
		engineParams = rec.EngineParams
	}
	historyKey := a.historyKey(modelID, namespace, rm.VariantName, accelerator, gpuCount, role,
		rm.AvgOutputTokens, config.QueueLengthThreshold)
	k2, k2Priority := a.computeK2(
		historyKey,
		modelID, namespace, rm.VariantName,
		rm.QueueLength, rm.TokensInUse,
		rm.AvgOutputTokens, rm.AvgInputTokens,
		config.QueueLengthThreshold,
		engineParams,
		k1,
		rm.TotalKvCapacityTokens,
		role,
		downstreamSaturated,
		logger,
	)
	// The same saturated moment that yields a k2 observation yields the
	// replica's saturated THROUGHPUT: with the queue over the threshold the
	// engine is completing requests as fast as it can, so its completion rate
	// in that window is what one replica can sustain for this shape. Recorded
	// under the same key, and read back below for the throughput floor.
	//
	// Ready pods only. A pod that is still failing its readiness probe can
	// report completions -- the collector drops its timing for exactly that
	// reason (collector/attribute.go) but leaves its completion rate, which other
	// consumers sum as real work. A per-replica RATE from such a pod is not a
	// capacity, and the floor divides by it.
	//
	// Own replicas only, too. A warm-pool bridge is recorded under the
	// VARIANT it is lent to (same VariantName, same history key), and it runs
	// its engine on the pool's terms -- a lower --gpu-memory-utilization, a
	// different batch ceiling -- so its saturated rate is not a reading of
	// what one of this variant's replicas does. The window keeps a max, so
	// one such reading would price the whole variant for the rest of the
	// window; the read side already leaves bridges out
	// (floor.Estimate), and the write side has to match it.
	//
	// Recorded and read under the FLEET's output-length bucket, not this
	// replica's. The k2 key above is the replica's own, and rightly: its
	// occupancy is its own. Its throughput is priced per role, as the median
	// over the role's replicas (floor.Estimate), and a median over
	// readings from different buckets is a reading of nothing. A replica's
	// own average output length is a few minutes of its own completions,
	// and at a shape switch that is noise: a fresh replica's first
	// completions are the short requests (they finish first), so with the
	// fleet on 6000-token outputs it classified as `long` and read the
	// 1000-token shape's mu of its own -- 3.08 against the 1.42 the replicas
	// classified `xxlong` read. Measured on the 1000/6000 shape-swap trace
	// (2026-09-20, cycles 17:32-17:34): the median came out 2.94, the floor
	// said two replicas as the switch's batch drained, the fleet released
	// 10 -> 3 where 4-5 is what the shape needs at that arrival rate, and
	// the queue that built from 34 min cost a p95 TTFT of 32 s at 36-38 min.
	// One bucket per role per cycle -- the fleet's, weighted by request rate
	// so a fresh replica barely moves it (fleetOutputLength) -- gives every
	// replica of the role the same shape, and the median a meaning.
	// The INPUT bucket as well as the output one. historyKey carries output
	// only, so an input-only shape change left the key identical and the
	// pre-change window read back as an own, non-borrowed reading -- which
	// useDerived then preferred over a derived figure that had priced the
	// change correctly. k2 keeps historyKey unchanged; this is the throughput
	// key alone, as the shape-shift proposal scopes it.
	// Prefill is keyed on the prompt tokens it actually has to compute: a
	// prefix the cache already holds is not work, so the discount that
	// shape.New applies for KVreq applies to prefill's throughput too. Raw
	// prompt length would put the same replica in different buckets purely
	// because the hit rate moved. Decode keeps the raw figure it has always
	// used -- its capacity is about resident KV, which the shape's KVreq
	// already discounts on its own path.
	keyInput := shapeKeyInput
	if canonicalRole(role) == domain.RolePrefill {
		keyInput = shape.New(shapeKeyInput, shapeKeyOutput, fleetHitRate).ILeff
	}
	throughputKey := a.throughputKey(modelID, namespace, rm.VariantName, accelerator, gpuCount, role,
		keyInput, shapeKeyOutput, config.QueueLengthThreshold)
	if k2Priority == capacity.K2SrcObserved && rm.Ready && !rm.FromWarmPool {
		// keyInput is the effective prompt length this key was built with, so
		// the token rate is converted to requests/s at the same figure the
		// window is keyed by -- a rate divided by one shape and stored under
		// another is the fault the key exists to prevent.
		if mu, ok := saturatedCompletionRate(rm, role, fleetOutput, keyInput); ok {
			a.recordSaturatedThroughput(throughputKey, mu)
		} else {
			// The replica is full and queued -- the one moment its throughput can
			// be learned -- and nothing can price it. Every cycle that reaches
			// here is a cycle the demand floor will not have a mu for, and a role
			// with no mu gets no floor at all.
			//
			// Said out loud because the failure is otherwise invisible: on
			// 2026-09-22 a build whose GenerationTokenRate was never collected
			// (the query was registered only by the opt-in throughput analyzer)
			// ran a whole 40-minute benchmark with no floor for any role, and the
			// only trace in the log was an empty string where a bucket name
			// should have been. The same shape of bug had already been found once
			// for the arrival rate. A line here would have named it in seconds.
			logger.V(logging.DEFAULT).Info("saturated-throughput-not-priced",
				"modelID", modelID, "namespace", namespace, "variant", rm.VariantName,
				"pod", rm.PodName, "role", role,
				"generationTokenRate", rm.GenerationTokenRate, "requestRate", rm.RequestRate,
				"fleetOutputTokens", fleetOutput,
				"reason", "saturated, but no throughput could be priced: the role's demand floor has no mu and will not bind")
		}
	}
	reading := a.saturatedThroughputReading(throughputKey)
	saturatedThroughput, throughputBucket := reading.rate, reading.bucket
	// A derived figure prices the shape the fleet has NOT measured -- that is
	// the whole of its job. Where the fleet HAS measured this shape, under this
	// key, for itself, the measurement wins.
	//
	// Letting derived win unconditionally is what run R cost. In phase 1 the
	// fleet had its own reading of 1.27 req/s and the derivation replaced it
	// with 0.67, so the floor asked for 42-56 replicas against phase 1's usual
	// 4.5 and spent 32% more replica-minutes than main for a decode TTFT p95 of
	// 55.6 s against 0.215. Phase 2, where no reading for the arriving shape
	// exists and the borrowed one is six times wrong, is where derived belongs:
	// there it took the fleet from the 6-7 replicas main holds to 1-2.
	//
	// "Measured this shape" is precise: an OWN reading (not borrowed from a
	// neighbouring output bucket, which is exactly the stale figure the
	// derivation exists to replace) with enough samples to order on. A borrowed
	// or thin reading loses to derived, as before.
	throughputSamples := reading.samples
	if useDerived(derived.ok, reading) {
		saturatedThroughput = derived.rate
		throughputBucket = derivedBucket
		// Not the stale measured count, which describes a different figure
		// entirely and only ever reached a log line as a confusing number.
		throughputSamples = MinDerivedThroughputSamples
	}

	effectiveCapacity := k1
	bound := "k1-memory"
	if k2 < k1 {
		effectiveCapacity = k2
		bound = "k2-compute"
	}
	// Per-replica, per-cycle diagnostics: two lines per replica per optimize
	// cycle, the highest-volume logging in the controller. V(logging.DEFAULT) is
	// the shipped verbosity (cmd/main.go defaults -v to logging.DEFAULT), so
	// hack/benchmark/dump_k2_decisions.py still sees them out of the box, while an
	// operator who does not want them can drop to -v=1 without losing the V(1)
	// diagnostics elsewhere. replica-capacity-skipped is deliberately not among
	// them: it reports a degradation rather than a decision.
	logger.V(logging.DEFAULT).Info("replica-capacity-decision",
		"modelID", modelID, "namespace", namespace, "variant", rm.VariantName, "pod", rm.PodName,
		"k1MemoryBound", k1, "k2ComputeBound", k2, "k2Source", k2Priority.String(),
		"effectiveCapacity", effectiveCapacity, "boundBy", bound,
		"tokensInUse", rm.TokensInUse, "localQueueDemand", localQueueDemand, "replicaDemand", replicaDemand,
		"queueLength", rm.QueueLength, "queueThreshold", config.QueueLengthThreshold,
		"requestRate", rm.RequestRate,
		"saturatedThroughput", saturatedThroughput, "saturatedThroughputBucket", throughputBucket)

	// Update capacity store with live data, preserving EngineParams from any
	// existing record (parsed from deployment args and needed for FindCompatible).
	var existingParams *capacity.EngineParams
	if existing := a.capacityStore.Get(namespace, modelID, rm.VariantName); existing != nil && existing.EngineParams != nil {
		existingParams = existing.EngineParams
	}
	a.capacityStore.Update(namespace, modelID, rm.VariantName, capacity.Record{
		AcceleratorName:       accelerator,
		GpuCount:              gpuCount,
		NumGpuBlocks:          rm.NumGpuBlocks,
		BlockSize:             rm.BlockSize,
		TotalKvCapacityTokens: rm.TotalKvCapacityTokens,
		EffectiveCapacity:     effectiveCapacity,
		EngineParams:          existingParams,
		LearnedFrom:           capacity.LearnedFromLive,
	})

	return &capacity.ReplicaCapacity{
		PodName:                     rm.PodName,
		VariantName:                 rm.VariantName,
		AcceleratorName:             accelerator,
		TokensInUse:                 rm.TokensInUse,
		QueueLength:                 rm.QueueLength,
		LocalQueueDemand:            localQueueDemand,
		TotalKvCapacityTokens:       rm.TotalKvCapacityTokens,
		MemoryBoundCapacity:         k1,
		ComputeBoundCapacity:        k2,
		K2Priority:                  k2Priority,
		EffectiveCapacity:           effectiveCapacity,
		ReplicaDemand:               replicaDemand,
		FromWarmPool:                rm.FromWarmPool,
		SaturatedThroughput:         saturatedThroughput,
		SaturatedThroughputSamples:  throughputSamples,
		SaturatedThroughputBorrowed: reading.borrowed,
		SaturatedThroughputDerived:  throughputBucket == derivedBucket,
	}
}

// computeReplicaCapacityFallback handles the case where vllm:cache_config_info
// is not available (TotalKvCapacityTokens == 0). It uses the deployment-derived
// capacity from the capacity store and estimates demand from KvCacheUsage percentage.
// This allows V2 to work with model servers that don't emit cache_config_info
// (e.g., the llm-d-inference-sim).
func (a *SaturationAnalyzer) computeReplicaCapacityFallback(
	rm domain.ReplicaMetrics,
	cfg *config.ScalingPolicy,
	modelID, namespace string,
	role string,
	accelerator string,
	logger logr.Logger,
) *capacity.ReplicaCapacity {
	rec := a.capacityStore.Get(namespace, modelID, rm.VariantName)
	if rec == nil || rec.EffectiveCapacity <= 0 {
		// Not a routine decision: this replica contributes no capacity at all,
		// so supply is under-counted and the controller over-scales. Unlike the
		// other per-replica lines it stays at Info, where -v cannot hide it.
		logger.Info("replica-capacity-skipped",
			"modelID", modelID, "namespace", namespace, "variant", rm.VariantName, "pod", rm.PodName,
			"reason", "no vllm:cache_config_info and no usable capacity-store record")
		return nil
	}

	// Apply KvCacheThreshold to match the main path (where k1 = totalTokens * threshold).
	// For deployment-derived records, EffectiveCapacity is the raw estimate; the threshold
	// reduces it to the usable portion, consistent with the normal code path.
	//
	// Floor at one token rather than letting the product truncate to zero: rec.EffectiveCapacity
	// is already known positive, and a zero here does not mean "no capacity", it means
	// "less than one token of capacity". Reporting zero makes the variant unsizable —
	// the engine sees a shortfall it cannot divide by a per-replica capacity, so it asks
	// for capacity and can never act on the answer.
	effectiveCapacity := int64(float64(rec.EffectiveCapacity) * cfg.KvCacheThreshold)
	if effectiveCapacity <= 0 {
		effectiveCapacity = 1
	}

	// Estimate demand from KV cache usage percentage applied to the record's RAW
	// capacity, not the thresholded one. Charging usage against the thresholded
	// capacity would put KvCacheThreshold on both sides of demand/supply, where it
	// cancels: utilization would collapse to KvCacheUsage and the threshold would
	// have no effect on the scaling decision at all. Against the raw capacity the
	// ratio is KvCacheUsage/KvCacheThreshold, which is what the main path computes
	// (TokensInUse / (TotalKvCapacityTokens × threshold)) — utilization reaches 1.0
	// exactly when observed KV occupancy reaches the configured ceiling.
	//
	// This is a coarse approximation — KvCacheUsage reflects memory pressure, not
	// exact token demand — but it's sufficient when token-level metrics are absent.
	kvUsageDemand := int64(rm.KvCacheUsage * float64(rec.EffectiveCapacity))

	// Add the role-aware footprint of requests waiting in the local engine queue,
	// matching the main path.
	//
	// Caveat, pre-existing and not introduced here: for a deployment-derived
	// record, EffectiveCapacity is EffectiveMaxBatchedTokens — a *per-step* token
	// budget the store itself calls "a safe lower bound" — while this addend is in
	// absolute KV tokens. The two are not the same unit, so on that record a deep
	// queue can push replicaDemand past effectiveCapacity and report saturation
	// that the replica's actual KV occupancy does not support. Raising the
	// per-request charge lowers the queue depth at which that happens. Tracked
	// separately; fixing it means pairing the fallback's demand and capacity units,
	// not adjusting this line.
	localQueueDemand := waitingQueueDemand(rm, role)
	replicaDemand := kvUsageDemand + localQueueDemand

	logger.V(logging.DEFAULT).Info("replica-capacity-store-fallback",
		"modelID", modelID, "namespace", namespace, "variant", rm.VariantName, "pod", rm.PodName,
		"reason", "no vllm:cache_config_info; using capacity-store record",
		"storeEffectiveCapacity", rec.EffectiveCapacity, "storeLearnedFrom", rec.LearnedFrom,
		"kvCacheUsagePct", rm.KvCacheUsage, "effectiveCapacity", effectiveCapacity,
		"kvUsageDemand", kvUsageDemand, "localQueueDemand", localQueueDemand, "replicaDemand", replicaDemand)

	return &capacity.ReplicaCapacity{
		PodName:               rm.PodName,
		VariantName:           rm.VariantName,
		AcceleratorName:       accelerator,
		TokensInUse:           replicaDemand,
		QueueLength:           rm.QueueLength,
		LocalQueueDemand:      localQueueDemand,
		TotalKvCapacityTokens: effectiveCapacity, // synthetic: store-derived
		MemoryBoundCapacity:   effectiveCapacity,
		ComputeBoundCapacity:  effectiveCapacity,
		K2Priority:            capacity.K2SrcFallback,
		EffectiveCapacity:     effectiveCapacity,
		ReplicaDemand:         replicaDemand,
		FromWarmPool:          rm.FromWarmPool,
	}
}

// useDerived reports whether the derived figure should price this replica.
//
// It should where the fleet has no measurement of the shape now arriving --
// which is the whole of its job -- and not where it has one. "Has one" is
// precise: an OWN reading, in this key's own output bucket, with enough
// samples to order on. A BORROWED reading is the stale figure from a
// neighbouring bucket that the derivation exists to replace, and a thin one is
// not yet evidence, so both lose to derived.
func useDerived(derivedOK bool, reading throughputReading) bool {
	if !derivedOK {
		return false
	}
	ownMeasured := !reading.borrowed && reading.rate > 0 &&
		reading.samples >= floor.MinThroughputSamplesToOrder
	return !ownMeasured
}

// derivedBucket is the sentinel the bucket label carries when mu was derived
// from the ITL model rather than measured. classifyOutputLength can never
// produce it, so it cannot be spoofed by a real output bucket.
const derivedBucket = "derived"

// prefillOutputBucket is the output bucket every prefill throughput reading is
// recorded under, whatever the fleet is generating.
//
// A prefill replica completes its request after the first token and hands the
// KV to decode, so how long the ANSWER turns out to be is not work it does.
// Keying its saturated throughput by the fleet's output length therefore
// splits one population of readings across unrelated buckets, and a shape swap
// walks it through several of them as the old requests drain.
//
// Measured on the 1k/6000 -> 30k/250 trace (run PJ, 2026-09-29): within a
// single phase whose prompt length never moved, prefill's bucket read xlong,
// then xxlong, then xlong, then medium, with mu re-learned in each. It had no
// mu at all for the first nine minutes of traffic, and when the floor wanted
// about nine replicas it was held to one per cycle -- 1->2->3->4 over two and
// a half minutes -- until a window finally had samples of its own, then
// jumped to 10.
//
// Not in outputBuckets, so nearestSaturatedThroughput declines to borrow
// across it. That is the intent: borrowing exists to cover the empty bucket an
// OUTPUT switch leaves behind, which prefill no longer has. The INPUT bucket
// stays in the key and still partitions these readings, because prompt length
// is what a prefill replica's throughput actually depends on -- so an input
// switch still, correctly, makes prefill re-learn.
const prefillOutputBucket = "noout"

// throughputKey is historyKey with the fleet's input bucket COMPOSED INTO it --
// not appended, see the body: a saturated throughput is a property of a replica
// AND the (I, O) it was measured under, so a reading recorded at one input
// length is not an own reading for another.
func (a *SaturationAnalyzer) throughputKey(
	modelID, namespace, variantName, accelerator string,
	gpuCount int,
	role string,
	avgInput, avgOutput float64,
	queueThreshold float64,
) string {
	// Composed, not appended. splitHistoryKey reads the output bucket as the
	// second-to-last field and the queue threshold as the last; an input
	// bucket tacked on the end made it read "q5" as the output bucket, which
	// is in no bucket table, so nearestSaturatedThroughput returned nothing
	// for every key and the neighbour-bucket borrow silently died. Putting the
	// input bucket ahead of the output one keeps that parse intact, and makes
	// a borrow walk output buckets WITHIN an input bucket -- which is what
	// borrowing should mean anyway.
	// Prefill is keyed by prompt length alone: the output bucket is a constant
	// for it, because output length is not work a prefill replica does. See
	// prefillOutputBucket.
	outBucket := classifyOutputLength(avgOutput)
	if canonicalRole(role) == domain.RolePrefill {
		outBucket = prefillOutputBucket
	}
	return fmt.Sprintf("%s|%s|%d|%s|i%s|%s|q%g",
		modelID, a.stableAccelerator(namespace, variantName, accelerator),
		gpuCount, canonicalRole(role), classifyInputLength(avgInput),
		outBucket, queueThreshold)
}

// historyKey is the bucket a replica's saturated observations (k2, and the
// throughput recorded beside it) are stored and read under.
//
// Scoped by role, not just model/accelerator/bucket: prefill's own
// avgOutputTokens is always ~0-1 (it hands off to decode before
// generating anything), so it always lands in the "short" bucket -- the
// same bucket a cold/fresh decode replica lands in before it's served
// real traffic. Without the role in the key, one role's P1-obs seeds
// history the other role then reads back via P2-hist, silently reusing
// an unrelated role's occupancy reading as its own capacity estimate.
//
// The queue threshold is in the key because it DEFINES what a P1 observation
// means: k2 is recorded as the occupancy seen when the queue was considered
// saturated, so a reading taken at threshold 2 is not a capacity estimate at
// threshold 100. Nothing else invalidates history -- EvictStaleHistory is
// age-based and knows nothing about policy -- so without this an operator
// retuning queueLengthThreshold keeps being sized by observations recorded
// under the old one. Measured: a k2 of 2 learned under a low threshold kept
// a variant at utilization 1.0 under a threshold of 100, where P1 could not
// fire at all.
func (a *SaturationAnalyzer) historyKey(
	modelID, namespace, variantName, accelerator string,
	gpuCount int,
	role string,
	avgOutput float64,
	queueThreshold float64,
) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|q%g",
		modelID, a.stableAccelerator(namespace, variantName, accelerator),
		gpuCount, canonicalRole(role), classifyOutputLength(avgOutput), queueThreshold)
}

// computeK2 determines the compute-bound capacity using a priority chain:
// 1. Observed (queue saturated) → use tokensInUse as k2
// 2. Historical → rolling average from previous observations
// 3. Derived (from deployment args) → formula-based estimate
// 4. Fallback → k1 (memory-bound only)
// Returns the k2 value and the priority level (1–4) that produced it.
// historyKey is the replica's bucket from historyKey(); modelID, namespace
// and variantName are for the log lines only.
func (a *SaturationAnalyzer) computeK2(
	historyKey string,
	modelID, namespace, variantName string,
	queueLen int, tokensInUse int64,
	avgOutput, avgInput float64,
	queueThreshold float64,
	engineParams *capacity.EngineParams,
	k1 int64,
	kvCeiling int64,
	role string,
	downstreamSaturated bool,
	logger logr.Logger,
) (int64, capacity.K2Source) {
	// Priority 1: Observed (queue saturated)
	//
	// A reading above the KV cache's PHYSICAL ceiling is a scrape artifact
	// (e.g. a mid-cycle admission/eviction race between the metrics snapshot
	// and the cache accounting), not real demand: accepting it would seed the
	// rolling average with a value every subsequent P2-hist cycle inherits,
	// long after the artifact itself is gone.
	//
	// The bound is kvCeiling, NOT k1. k1 is TotalKvCapacityTokens x
	// KvCacheThreshold (0.80 by default), so occupancy between k1 and the
	// ceiling is entirely legitimate -- and with the queue saturated it is the
	// most informative reading there is. Discarding that band would throw away
	// exactly the observations P1-obs exists to capture, and leave the analyzer
	// reporting a capacity ABOVE what the replica is demonstrably holding.
	// Such a reading cannot inflate this cycle either: effectiveCapacity is
	// min(k1, k2), so k1 still bounds it.
	//
	// Falls through to Priority 2 rather than returning k1 directly, so a real
	// historical/derived signal still wins over an untimely fallback-to-k1.
	//
	// The value returned is the rolling average AFTER folding this observation
	// in, not the raw sample: tokensInUse under a saturated queue reflects
	// whatever happens to be admitted this instant, which churns with
	// admission/eviction independently of the backlog it's meant to size for
	// (a replica can shed most of its resident batch while its wait-list stays
	// pinned). Returning it raw lets one noisy cycle single-handedly swing
	// capacity -- and everything downstream that divides by it -- long before
	// the next cycle can correct it. Blending it into the same window P2-hist
	// already reads gives a lone outlier 1/RollingAverageWindowSize weight
	// instead of 100%, while a real, sustained shift still dominates the
	// average once the window has turned over.
	//
	// The window is per VARIANT, not per replica -- historyKey carries no pod
	// identity -- and computeK2 runs once per replica per cycle. So a variant
	// with R replicas writes R samples per cycle and the window spans
	// RollingAverageWindowSize/R cycles: a sustained shift converges in ~2-3
	// cycles at R=4 and takes the full window at R=1. Damping is therefore
	// deepest at one replica, which is where scale-up latency matters most.
	// That is a property to keep in mind when tuning the window, not a bug:
	// outlier weight is 1/N regardless of R, and it is only the time constant
	// that moves.
	//
	// A window that has gone stale is reset rather than blended into. Without
	// that, a variant returning after a quiet period would have its first
	// genuine observation diluted to 1/N against samples describing behaviour
	// from before the gap -- an exposure this smoothing creates and the raw
	// return did not have.
	//
	// A saturated PREFILL replica is a reading of prefill only while decode
	// is not saturated. A prefill request is done when decode admits it and
	// pulls its KV, so with decode over its queue threshold what prefill
	// shows is metered by decode: the KV of finished prompts it holds until
	// the pull, the arrivals the scheduler's flow control releases to it in
	// bursts, and a completion rate that is decode's admission rate. That is
	// decode's saturation seen from upstream. Recorded as prefill's, it
	// persists: measured on the shape-swap P/D benchmark (2026-09-18, cold
	// pass), a single prefill replica showed queue 30 and 357 800 resident
	// tokens for four cycles at +220..+265 s -- the cycles both decode
	// replicas were over the threshold at ~1.0M resident -- and was priced
	// at k2 = 357 800 (k1 was 919 859) and mu = 5.57 req/s. Its own
	// occupancy then read 100 % of that k2 and ordered a second prefill
	// replica; the mu, below the 6 req/s offered, held both for the rest
	// of the run (lambda / mu = 1.08 replicas at the median) while their
	// resident KV read near zero -- 33 GPU-minutes, and no later cycle could
	// correct either figure, because a prefill fleet of two never saturates
	// again. The correlation is what was measured; which path carried it
	// (held blocks, flow-control bursts) is not settled -- prefill's KV was
	// at 31 % of its cache on the gated rows, so it was not block-starved.
	// The reading is left unrecorded: k2 falls through to history or k1,
	// and the throughput floor records no mu (computeReplicaCapacity keys
	// that on capacity.K2SrcObserved). A prefill bottleneck reduces decode's
	// arrivals, so the two saturate at once only when decode is short at
	// prefill's completion rate; prefill's reading then waits for decode to
	// recover.
	//
	// The decode test is a replica full AND queued (roleSaturated), not the
	// queue alone: vLLM counts a decode request waiting for its remote KV in
	// num_requests_waiting, so a fleet whose transfer keeps as many in
	// flight as the threshold would read decode saturated every cycle on
	// the queue alone, and prefill would never record and -- with the hold
	// on its demand (Analyze) -- never be ordered. Full is decode unable to
	// allocate the blocks a pull needs, which the transfer pipeline does not
	// produce. The test is remembered for the collector's row window
	// (rememberDecodeSaturation): on the measured episode decode's
	// occupancy dropped under k1 on the fourth cycle while its queue and
	// prefill's stale row had not moved, and that row would otherwise have
	// recorded. A decode bound by its sequence ceiling before its KV does
	// not gate; that is a prefill mislearned low, which costs money, where
	// a prefill that cannot be ordered costs latency.
	if queueLen >= int(queueThreshold) && tokensInUse > 0 && downstreamSaturated {
		logger.V(logging.DEFAULT).Info("k2-decision",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"priority", k2ReasonObsDownstream, "historyKey", historyKey,
			"queueLength", queueLen, "queueThreshold", queueThreshold,
			"reason", "queue saturated while the decode role is; a prefill completes only when decode admits it, so this is decode's saturation seen from prefill; not recorded",
			"tokensInUse", tokensInUse, "k1", k1)
	} else if queueLen >= int(queueThreshold) && tokensInUse > 0 {
		k2Observed := tokensInUse
		if kvCeiling > 0 && k2Observed > kvCeiling {
			logger.V(logging.DEFAULT).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", k2ReasonObsImplausible, "historyKey", historyKey,
				"queueLength", queueLen, "queueThreshold", queueThreshold,
				"reason", "observed tokensInUse exceeds the KV cache's physical ceiling; discarding as implausible",
				"k2Observed", k2Observed, "k1", k1, "kvCeiling", kvCeiling)
		} else {
			a.mu.Lock()
			ra, ok := a.computeCapacityHistory[historyKey]
			if !ok || ra.Stale(capacity.HistoryEvictionTimeout) {
				ra = capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
				a.computeCapacityHistory[historyKey] = ra
			}
			ra.Add(float64(k2Observed))
			k2Smoothed := int64(ra.Average())
			historyLen := ra.Len()
			a.mu.Unlock()
			logger.V(logging.DEFAULT).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", capacity.K2SrcObserved.String(), "historyKey", historyKey,
				"queueLength", queueLen, "queueThreshold", queueThreshold,
				"k2Observed", k2Observed, "k2", k2Smoothed, "historyWindowLen", historyLen)
			return k2Smoothed, capacity.K2SrcObserved
		}
	}

	// Priority 2: Historical — lock must cover Average() since Add() mutates
	// the same slice from Priority 1 under the same lock.
	a.mu.Lock()
	var histAvg float64
	var histLen int
	if ra, ok := a.computeCapacityHistory[historyKey]; ok {
		histAvg = ra.Average()
		histLen = ra.Len()
	}
	a.mu.Unlock()
	if histAvg > 0 {
		logger.V(logging.DEFAULT).Info("k2-decision",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"priority", capacity.K2SrcHistorical.String(), "historyKey", historyKey,
			"queueLength", queueLen, "queueThreshold", queueThreshold,
			"k2", int64(histAvg), "historyWindowLen", histLen)
		return int64(histAvg), capacity.K2SrcHistorical
	}

	// Priority 3: Derived from deployment args
	//
	// The formula assumes avgOutput is a genuine per-request output-token
	// count feeding an iterative decode-batching steady-state estimate.
	// Prefill's own vLLM instance reports avgOutput~0-1 (it hands off to
	// decode before generating anything), which collapses the formula's O
	// terms and returns ~EffectiveMaxBatchedTokens regardless of prefill's
	// real per-replica behavior -- not a derived signal at all, just the
	// batch-token budget echoed back. Skip straight to the k1 fallback for
	// prefill rather than report a number that looks derived but isn't.
	//
	// The batch-token budget alone (a prior revision reported it directly as
	// P3-prefill) isn't a substitute: every other capacity figure in this
	// chain is a product -- k1 is capacity x threshold, decode's own P3 is
	// concurrency x per-request footprint -- scaled to the same order of
	// magnitude as demand. The raw per-step budget has no such scaling and
	// disagreed with this replica's own directly observed behavior: at
	// EffectiveMaxBatchedTokens=65536, vllm:num_requests_waiting read 0 across
	// every sample of a full benchmark run, yet P3-prefill's ~65536 read as
	// 5-10x under water against demand snapshots in the hundreds of thousands
	// -- capacity that looked starved while the replica was never once
	// observed to queue. k1 over-states capacity by the same 1-2 orders of
	// magnitude in the other direction, but at least it bounds something real
	// (the KV cache this replica actually has) rather than a per-step figure
	// compared against an accumulated one.
	isPrefill := canonicalRole(role) == domain.RolePrefill
	if !isPrefill {
		if k2Derived := estimateCapacityFromParams(engineParams, avgInput, avgOutput); k2Derived > 0 {
			logger.V(logging.DEFAULT).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", capacity.K2SrcDerived.String(), "historyKey", historyKey,
				"avgInputTokens", avgInput, "avgOutputTokens", avgOutput,
				"engineParams", engineParams, "k2", k2Derived)
			return k2Derived, capacity.K2SrcDerived
		}
	}

	// Priority 4: Fallback to k1
	reason := "no observed/historical/derived k2; capacity is memory-bound only"
	if isPrefill {
		reason = "prefill role: derived-from-args formula assumes decode-style output length; skipped"
	}
	logger.V(logging.DEFAULT).Info("k2-decision",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"priority", capacity.K2SrcFallback.String(), "historyKey", historyKey,
		"reason", reason,
		"k1", k1)
	return k1, capacity.K2SrcFallback
}

// aggregateByVariant groups replica capacities by variant and computes
// per-variant capacity metrics.
func (a *SaturationAnalyzer) aggregateByVariant(
	replicaCapacities []capacity.ReplicaCapacity,
	inputMetrics []domain.ReplicaMetrics,
	variantStates []domain.VariantReplicaState,
	modelID, namespace string,
	kvCacheThreshold float64,
	logger logr.Logger,
) []domain.VariantCapacity {
	// Group replicas by variant
	byVariant := make(map[string][]capacity.ReplicaCapacity)
	for _, rc := range replicaCapacities {
		byVariant[rc.VariantName] = append(byVariant[rc.VariantName], rc)
	}

	// Compute model-level workload averages from live replica metrics.
	// Used for capacity estimation of zero-replica variants with deployment-derived params.
	modelAvgInput, modelAvgOutput, _ := computeModelWorkloadAverages(inputMetrics, rolesFromStates(variantStates))

	result := make([]domain.VariantCapacity, 0, len(variantStates))
	for _, vs := range variantStates {
		replicas := byVariant[vs.VariantName]

		var perReplicaCapacity float64
		var totalDemand float64
		// Bridges serving this variant, counted apart from its own replicas.
		var warmPoolReplicas int
		// P for a bridge, measured from the bridges themselves. Kept apart from
		// perReplicaCapacity because the pool runs its engines at a lower
		// --gpu-memory-utilization than the workload, so the two are not
		// interchangeable readings. See the split in the loop below.
		var bridgePerReplica float64
		// accelerator is an analyzer input (discovery-resolved, on VariantReplicaState),
		// used below for cross-variant capacity lookup. It is NOT emitted on the output;
		// the capacity builder fills per-variant identity from discovery.
		accelerator := vs.AcceleratorName

		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}

		// replicaCount sets VariantCapacity.ReplicaCount, which aggregation.go uses
		// to recompute supply totals, and divides totalDemand for the per-variant
		// utilization. It is in scale-target units (pods, or LWS groups) on every
		// branch: readyCount is scale-target status, and len(replicas) counts the
		// collector's per-pod rows, which merge a pod's engine instances into one
		// replica (see collector.collapseToPods). pendingCount shares that unit,
		// as SumTotalAnticipatedSupply adds the two.
		replicaCount := readyCount
		pendingCount := vs.PendingReplicas
		// What this cycle's rows attributed to the variant, own replicas only,
		// and 0 when none reported. Never readyCount: that is scale-target
		// status, and the point of this number is to be the thing status is
		// not -- see domain.VariantCapacity.ObservedReplicas.
		observedReplicas := 0

		var capacityLabel string
		if len(replicas) > 0 {
			// BRIDGES count toward demand and not toward supply.
			//
			// A bridge is a warm pool Pod lent to this variant while it is short.
			// The traffic it is serving is this variant's traffic, so its demand
			// belongs in the total like any replica's -- leave it out and demand
			// reads lowest exactly while a bridge is covering the shortfall, then
			// appears from nowhere when the Pod goes back.
			//
			// Its capacity is a different matter. The Pod is borrowed and returns
			// when the ordinary replicas arrive, so counting it as supply would
			// tell the optimizer the fleet is already big enough and suppress the
			// scale-up the bridge exists to bridge. The pool would then hold the
			// Pod indefinitely: the replicas that would release it are the ones
			// it talked the optimizer out of creating.
			//
			// Its capacity IS measured, and carried out separately for the
			// retained-pool switching decision, where the pool is the capacity
			// and there are no ordinary replicas coming.
			// P IS MEASURED OVER OWN REPLICAS, AND A BRIDGE IS PRICED APART.
			//
			// Both are measurements, so both are the analyzer's to emit; what is
			// DERIVED from them (supply, and the bridges' worth) belongs to the
			// capacity-build step and is not computed here.
			//
			// The two readings differ for a structural reason: the pool runs its
			// engines at a lower --gpu-memory-utilization than the workload does,
			// because it fits several sleepers in a Pod where a workload replica
			// has the GPU to itself. Less of the GPU is KV cache, so a bridge's
			// memory-bound capacity is genuinely smaller. See
			// domain.VariantCapacity.PerReplicaCapacity.
			ownCapacities := make([]int64, 0, len(replicas))
			bridgeCapacities := make([]int64, 0, len(replicas))
			ownRows := make([]capacity.ReplicaCapacity, 0, len(replicas))
			ownReplicas := 0
			for _, rc := range replicas {
				// Demand is summed over EVERY row, bridges included: the traffic a
				// bridge is serving is this variant's traffic. Only capacity splits.
				totalDemand += float64(rc.ReplicaDemand)
				if rc.FromWarmPool {
					warmPoolReplicas++
					bridgeCapacities = append(bridgeCapacities, rc.EffectiveCapacity)
					continue
				}
				ownReplicas++
				ownCapacities = append(ownCapacities, rc.EffectiveCapacity)
				ownRows = append(ownRows, rc)
			}
			bridgePerReplica = float64(median(bridgeCapacities))
			if len(ownCapacities) > 0 {
				perReplicaCapacity = float64(median(ownCapacities))
				capacityLabel = k2SourceLabel(ownRows)
			} else {
				// Only bridges reported. The variant's own per-replica capacity is
				// unmeasured this cycle, and the bridge's reading is the closest
				// thing to it that was actually observed -- lower than the truth,
				// which asks for one replica too many rather than one too few.
				// Zero would be worse than either: the optimizer skips a variant
				// whose per-replica capacity is <= 0 (cost_aware_optimizer.go), so
				// a variant a bridge is currently carrying would stop being scaled
				// at all -- exactly while it is short.
				perReplicaCapacity = bridgePerReplica
				capacityLabel = k2SourceLabel(replicas)
			}
			// Prefer the live count over readyCount: it is what actually reported
			// capacity this cycle, where readyCount is (lagging) scale-target status.
			replicaCount = ownReplicas
			observedReplicas = ownReplicas
			// And keep the ARRIVING count in step with it. Pending is everything
			// the scale target owns that did not report this cycle -- not just
			// the pods the target itself calls not-ready. A replica that turned
			// Ready between the last scrape and this optimize cycle is in neither
			// set: it is ready, so the target's PendingReplicas excludes it, and
			// it has no metrics row yet, so ownReplicas excludes it too. Counting
			// it nowhere drops it from anticipated supply, which is sized as
			// (ReplicaCount + PendingReplicas) x P, and the shortfall that
			// remains orders a replica that already exists.
			//
			// Measured on the shape-swap P/D run (biran-20260915-102548-571,
			// 07:30:23): the target reported 4 replicas, 2 Ready, and one row
			// scraped -- the second Ready pod had passed its probe one second
			// earlier. Anticipated supply counted 3, the decode role was sized
			// to 7 instead of 6, and the extra replica stayed: the next cycle
			// took the 7 as its starting point and nothing asked for it back
			// until spare capacity did, minutes later.
			//
			// CurrentReplicas - ownReplicas covers both cases with one figure:
			// the target's own not-ready pods, and its ready-but-unscraped ones.
			// It is never negative in effect -- replicas still reporting after
			// an in-flight scale-down make it so, and the engine's clamp
			// (steadystate.clampReplicaCountToScaleTarget) already caps
			// ReplicaCount at CurrentReplicas for exactly that case, so pending
			// is zero there rather than the target's stale not-ready count.
			pendingCount = max(0, vs.CurrentReplicas-ownReplicas)
		} else if rec := a.capacityStore.Get(namespace, modelID, vs.VariantName); rec != nil && rec.EffectiveCapacity > 0 {
			// No ready replicas — use stored capacity, enhanced with k2 derivation
			// for deployment-derived records when workload data is available.
			perReplicaCapacity = a.estimateStoredCapacity(rec, modelID, namespace, vs.VariantName, accelerator, vs.GPUsPerReplica,
				kvCacheThreshold, modelAvgInput, modelAvgOutput, logger)
			capacityLabel = satReasonP0Store
		} else if rec := a.lookupCompatibleCapacity(namespace, modelID, vs.VariantName, accelerator, vs.GPUsPerReplica); rec != nil {
			// No own record — try cross-variant estimation from a compatible variant
			perReplicaCapacity = float64(rec.EffectiveCapacity)
			capacityLabel = satReasonP0Store
			logger.Info("variant-capacity-source",
				"modelID", modelID, "namespace", namespace, "variant", vs.VariantName,
				"reason", "no own capacity record; borrowed from a compatible variant",
				"perReplicaCapacity", perReplicaCapacity, "engineParamsSource", rec.LearnedFrom)
		} else {
			capacityLabel = satReasonNoData
			logger.Info("variant-capacity-source",
				"modelID", modelID, "namespace", namespace, "variant", vs.VariantName,
				"reason", "no live replicas, no own capacity-store record, no compatible variant found")
		}

		totalCapacity := float64(replicaCount) * perReplicaCapacity

		var utilization float64
		if totalCapacity > 0 {
			utilization = totalDemand / totalCapacity
		}

		// Per-variant identity (Cost, AcceleratorName) is intentionally not set here:
		// the capacity builder fills it from the discovery step, so the analyzer's
		// output is the measured capacity signal, not laundered identity.
		result = append(result, domain.VariantCapacity{
			VariantName:      vs.VariantName,
			Role:             vs.Role,
			ReplicaCount:     replicaCount,
			ObservedReplicas: observedReplicas,
			PendingReplicas:  pendingCount,
			PendingAges:      vs.PendingAges,
			StuckReplicas:    vs.StuckReplicas,
			WarmPoolReplicas: warmPoolReplicas,
			// Both readings are MEASURED, so both are the analyzer's to emit, and
			// they are kept apart because they are different numbers. What they
			// are worth in total -- WarmPoolCapacity -- is derived, so the
			// capacity-build step owns it and nothing is set for it here.
			WarmPoolPerReplicaCapacity: bridgePerReplica,
			PerReplicaCapacity:         perReplicaCapacity,
			TotalDemand:                totalDemand,
			Utilization:                utilization,
			Reason:                     capacityLabel,
		})
		if warmPoolReplicas > 0 {
			logger.Info("warm-pool-bridge-supply",
				"modelID", modelID, "namespace", namespace, "variant", vs.VariantName,
				"bridges", warmPoolReplicas, "ownReplicas", replicaCount,
				// Both prices, because the whole point of measuring them apart is
				// that they differ -- a reader given only one cannot tell whether
				// the split is working, or whether the pool is running the engine
				// on the same terms as the workload.
				"bridgePerReplica", bridgePerReplica,
				"ownPerReplicaCapacity", perReplicaCapacity,
				"totalDemand", totalDemand,
				"note", "bridge demand is counted, bridge capacity is not supply")
		}
	}

	return result
}

// aggregateRoleDemand groups the variant capacities' demand by role and returns
// the analyzer's per-role demand attribution — the demand half of the (D, P)
// contract. Returns nil when no disaggregation is active (all variants are role
// "both" or empty). The queueDemandByRole map adds scheduler queue demand
// attributed to each role (nil when there's no queue demand); a role that no
// variant serves is ignored, so queue demand is never charged to a role with no
// supply behind it.
//
// Per-role supply is deliberately not computed here: the engine's capacity-build
// step recomputes it from the same VariantCapacities and pairs it with this map.
func (a *SaturationAnalyzer) aggregateRoleDemand(
	variantCapacities []domain.VariantCapacity,
	queueDemandByRole map[string]float64,
) map[string]float64 {
	if !aggregation.IsDisaggregated(variantCapacities) {
		return nil
	}

	demand := aggregation.DemandByRole(variantCapacities)

	// Add scheduler queue demand attributed to each role.
	for role, qd := range queueDemandByRole {
		if _, ok := demand[role]; ok {
			demand[role] += qd
		}
	}
	return demand
}

// lookupCompatibleCapacity searches the capacity store for a record from
// another variant with matching hardware and engine parameters. This enables
// capacity estimation for zero-replica variants that have no prior data.
// The search is cross-namespace since capacity depends on hardware + config,
// not namespace.
func (a *SaturationAnalyzer) lookupCompatibleCapacity(namespace, modelID, variantName, accelerator string, gpuCount int) *capacity.Record {
	// Get EngineParams for this variant (from deployment-derived record)
	rec := a.capacityStore.Get(namespace, modelID, variantName)
	if rec == nil || rec.EngineParams == nil {
		return nil
	}
	return a.capacityStore.FindCompatible(modelID, accelerator, gpuCount, rec.EngineParams)
}

// estimateStoredCapacity returns a capacity estimate for a zero-replica variant
// using its stored capacity.Record. For live records (from a previously running
// pod), the stored EffectiveCapacity is authoritative. For "deployment" records,
// it tries to compute a better estimate using the k2 derivation formula with
// model-level workload averages, bounded by:
//  1. A compatible variant's live EffectiveCapacity (already min(k1,k2))
//  2. Own k1 if TotalKvCapacityTokens is known (from num_gpu_blocks_override)
//
// Falls back to stored EffectiveCapacity (EffectiveMaxBatchedTokens) when no
// workload data is available.
//
// accelerator and gpuCount are the variant's hardware keys and come from the
// discovery metadata, NOT from rec. A stored record's own copies can be empty or
// stale — a deployment-derived record is written before any pod has reported —
// and keying the compatibility search off them silently finds no match, which
// reads as "no comparable variant exists" rather than "we looked with a blank
// key". These are the same keys the record is written under, so read and write
// agree by construction.
func (a *SaturationAnalyzer) estimateStoredCapacity(
	rec *capacity.Record,
	modelID, namespace, variantName string,
	accelerator string,
	gpuCount int,
	kvCacheThreshold float64,
	modelAvgInput, modelAvgOutput float64,
	logger logr.Logger,
) float64 {
	if rec == nil {
		return 0
	}

	// Live records have observed capacity — use directly
	if rec.LearnedFrom == capacity.LearnedFromLive {
		logger.Info("zero-replica-capacity-estimate",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"source", "stored-live", "reason", "prior live observation reused while replica count is zero",
			"perReplicaCapacity", rec.EffectiveCapacity)
		return float64(rec.EffectiveCapacity)
	}

	// For deployment-derived records, try k2 derivation with workload data
	if rec.EngineParams != nil && modelAvgOutput > 0 {
		if derived := estimateCapacityFromParams(rec.EngineParams, modelAvgInput, modelAvgOutput); derived > 0 {
			bounded := derived
			boundedBy := "derived"

			// Bound by own k1 if TotalKvCapacityTokens is known (num_gpu_blocks_override)
			if rec.TotalKvCapacityTokens > 0 && kvCacheThreshold > 0 {
				k1 := int64(float64(rec.TotalKvCapacityTokens) * kvCacheThreshold)
				if k1 > 0 && k1 < bounded {
					bounded = k1
					boundedBy = "own-k1"
				}
			}

			// Bound by compatible variant's live EffectiveCapacity (already min(k1,k2))
			if compatible := a.capacityStore.FindCompatible(modelID, accelerator, gpuCount, rec.EngineParams); compatible != nil && compatible.LearnedFrom == capacity.LearnedFromLive && compatible.EffectiveCapacity > 0 {
				if compatible.EffectiveCapacity < bounded {
					bounded = compatible.EffectiveCapacity
					boundedBy = "compatible-variant-live"
				}
			}

			logger.Info("zero-replica-capacity-estimate",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"source", "deployment-derived", "boundedBy", boundedBy,
				"derivedCapacity", derived, "perReplicaCapacity", bounded,
				"modelAvgInputTokens", modelAvgInput, "modelAvgOutputTokens", modelAvgOutput)
			return float64(bounded)
		}
	}

	// Fallback: stored EffectiveCapacity (EffectiveMaxBatchedTokens from LoadFromDeployment)
	logger.Info("zero-replica-capacity-estimate",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"source", "deployment-stored-fallback",
		"reason", "no workload averages or derivation available; using raw stored EffectiveMaxBatchedTokens",
		"perReplicaCapacity", rec.EffectiveCapacity)
	return float64(rec.EffectiveCapacity)
}

// estimateCapacityFromParams computes a capacity estimate using the k2 derivation
// formula: N_steady = min(B * O / (I + O), S), capacity = N_steady * (I + O/2).
// Used by computeK2 (Priority 3) for per-replica estimation and by
// estimateStoredCapacity for zero-replica variants with model-level workload averages.
// Returns 0 if estimation is not possible.
func estimateCapacityFromParams(params *capacity.EngineParams, avgInput, avgOutput float64) int64 {
	if params == nil || params.EffectiveMaxBatchedTokens <= 0 || avgOutput <= 0 {
		return 0
	}

	B := float64(params.EffectiveMaxBatchedTokens)
	S := float64(params.MaxNumSeqs)
	I := avgInput
	O := avgOutput

	nSteady := B * O / (I + O)
	if nSteady > S {
		nSteady = S
	}
	k2Derived := int64(nSteady * (I + O/2))
	if k2Derived > 0 {
		return k2Derived
	}
	return 0
}

// withExpectedOutputTokens returns replicaMetrics with expected filled in as
// the output length of every OUTPUT-GENERATING replica that reports none.
//
// It exists so the scheduler queue can be priced on the first cycle of load.
// estimateSchedulerQueueDemand charges the queue Q x avgOutput, and avgOutput
// is an average over replicas that have COMPLETED something; on a cold fleet
// none has, so the queue is worth nothing to the role that will generate it.
// Measured on run QL: decode's share read {"decode":0} for the first two
// cycles with 141 requests already queued.
//
// A COPY, never a mutation of the caller's slice. The input is the analyzer's
// own AnalyzerInput, read by several other steps in the same cycle, and a
// replica whose reported output length was quietly rewritten would change what
// every one of them measured -- including the throughput keys and the shape
// tracker, which must follow what the fleet actually served.
//
// Only replicas that generate output, and only those reporting zero: a prefill
// replica completes about one token per request, so filling it in would move a
// figure that is already correct, and a replica with a real reading is not
// improved by a default.
//
// Returns the input unchanged when expected is not positive or nothing needs
// filling, so the common case allocates nothing.
func withExpectedOutputTokens(replicaMetrics []domain.ReplicaMetrics,
	rolesByVariant map[string]string, expected float64) []domain.ReplicaMetrics {
	if !(expected > 0) {
		return replicaMetrics
	}
	needed := slices.ContainsFunc(replicaMetrics, func(rm domain.ReplicaMetrics) bool {
		return !(rm.AvgOutputTokens > 0) && generatesOutput(rm, rolesByVariant)
	})
	if !needed {
		return replicaMetrics
	}
	out := slices.Clone(replicaMetrics)
	for i := range out {
		if !(out[i].AvgOutputTokens > 0) && generatesOutput(out[i], rolesByVariant) {
			out[i].AvgOutputTokens = expected
		}
	}
	return out
}

// computeModelWorkloadAverages computes the model-level average input tokens,
// output tokens, and prefix cache hit rate from replica metrics across all
// variants. These averages enable capacity estimation for zero-replica variants
// using the k2 derivation formula, and scheduler queue demand estimation.
//
// The output length is averaged over the replicas that GENERATE output, which
// on a P/D fleet excludes prefill. A prefill replica completes every request
// after one token (it hands off to decode), so its own
// vllm:request_generation_tokens averages ~1 -- not a measurement of the
// workload's output length but a property of the role. Folding it into an
// unweighted mean halves the model's output length at one prefill per decode
// replica and quarters it at three, and everything priced per queued request
// downstream (the scheduler queue's output-token charge, the zero-replica k2
// derivation) shrinks with it. Measured on a P/D run: 213 requests queued at
// the scheduler were charged 500 output tokens each against a 1000-token
// workload, because the one prefill replica's ~1 averaged against the one
// decode replica's 1000.
//
// Input tokens and the prefix-cache hit rate are still averaged over every
// replica: both roles see the same prompts, and in a P/D deployment the prefix
// cache that a queued prompt can hit lives on the prefill side.
//
// rolesByVariant maps variant name to its P/D role; a variant absent from it
// is treated as domain.RoleBoth, so a non-disaggregated fleet averages over
// every replica exactly as before.
func computeModelWorkloadAverages(replicaMetrics []domain.ReplicaMetrics, rolesByVariant map[string]string) (avgInput, avgOutput, avgHitRate float64) {
	var count, outputCount int
	for _, rm := range replicaMetrics {
		if rm.AvgInputTokens > 0 || rm.AvgOutputTokens > 0 {
			avgInput += rm.AvgInputTokens
			avgHitRate += rm.PrefixCacheHitRate
			count++
			if generatesOutput(rm, rolesByVariant) {
				avgOutput += rm.AvgOutputTokens
				outputCount++
			}
		}
	}
	if count > 0 {
		avgInput /= float64(count)
		avgHitRate /= float64(count)
	}
	if outputCount > 0 {
		avgOutput /= float64(outputCount)
	}
	return avgInput, avgOutput, avgHitRate
}

// fleetOutputLength is the output length the fleet is serving this cycle: the
// generating replicas' average output tokens, weighted by their request rate
// (fleetAverage).
func fleetOutputLength(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.AvgOutputTokens },
		func(rm domain.ReplicaMetrics) bool { return generatesOutput(rm, rolesByVariant) })
}

// fleetOutputLengthRecent is fleetOutputLength over the SHORT window, and is
// the derived mu's divisor (deriveMu) WHILE A SHAPE CHANGE IS OUTSTANDING only.
//
// Zero when no generating replica reports the short-window figure -- an engine
// that does not publish the counter, or a fleet that completed nothing in the
// last minute.
//
// The caller falls back to fleetOutputLength then, and also whenever the shape
// is steady. That gate is not a precaution; it is a measured requirement. This
// window averages over requests that have COMPLETED, so on a fleet ramping into
// long generations it reads far below the length being served, which over-states
// mu and under-orders replicas. The caller's comment carries the bisect. The
// [5m] figure is wrong only for the few minutes after a shape change, which is
// exactly when this one is used.
func fleetOutputLengthRecent(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.AvgOutputTokensRecent },
		func(rm domain.ReplicaMetrics) bool { return generatesOutput(rm, rolesByVariant) })
}

// fleetPrefixHitRate is the prefill side's prefix-cache hit rate as ONE figure
// for the whole role, request-rate weighted.
//
// It is not fleetAverage: that helper skips a value of zero as absent, and a
// hit rate of zero is a reading, not a missing one -- a fleet with prefix
// caching off reports 0 on every replica, and skipping those would leave the
// mean to whichever replica happened to report something.
//
// One figure per role per cycle, for the same reason the output length is one
// figure: it buckets a key. rm.PrefixCacheHitRate is per REPLICA, so using it
// directly let two replicas of one variant land in different input buckets in
// the same cycle and split the window the bucket exists to hold together --
// the exact fault prefillOutputBucket was added to remove, on a new axis.
//
// Still not hysteretic: shape.Tracker tracks IL and OL, not this. A fleet
// whose hit rate drifts across a bucket boundary can therefore still move
// prefill's window, just not split it between replicas within a cycle.
func fleetPrefixHitRate(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.PrefixCacheHitRate },
		func(rm domain.ReplicaMetrics) bool {
			return canonicalRole(rolesByVariant[rm.VariantName]) == domain.RolePrefill
		},
		// A hit rate of zero is a reading -- a fleet with prefix caching off
		// reports it on every replica, and skipping those would leave the mean
		// to whichever replica happened to report something. Bounded because a
		// hit rate is a fraction.
		fleet.ZeroIsAReading(), fleet.Within(0, 1))
}

// rolesFromStates builds the variant-name -> role lookup the per-role helpers
// take, from the variant states an Analyze call carries.
func rolesFromStates(states []domain.VariantReplicaState) map[string]string {
	roles := make(map[string]string, len(states))
	for _, vs := range states {
		roles[vs.VariantName] = vs.Role
	}
	return roles
}

// generatesOutput reports whether a replica is one whose completions carry
// the workload's real output length and service time: every role but prefill.
// A variant with no recorded role is domain.RoleBoth, which generates.
func generatesOutput(rm domain.ReplicaMetrics, rolesByVariant map[string]string) bool {
	return canonicalRole(rolesByVariant[rm.VariantName]) != domain.RolePrefill
}

// canonicalRole normalizes an empty variant role to domain.RoleBoth, matching
// aggregation.AggregateByRole.
// stableAccelerator returns the accelerator to key history under, preferring the
// last one that resolved for this variant over an unresolved reading.
//
// Capacity is a property of the hardware, so it belongs in the key; but "the pods
// currently disagree about it" is not a different accelerator, and treating it as
// one is what makes the fleet oscillate. A resolved reading always wins and is
// remembered; an unresolved one falls back to what was remembered, and only when
// nothing was ever resolved does the unresolved value itself get used, which
// keeps a never-resolvable variant behaving exactly as it does today.
func (a *SaturationAnalyzer) stableAccelerator(namespace, variantName, accelerator string) string {
	key := namespace + "/" + variantName
	a.mu.Lock()
	defer a.mu.Unlock()
	if constants.IsAcceleratorResolved(accelerator) {
		a.lastAccelerator[key] = acceleratorMemo{name: accelerator, lastUsed: time.Now()}
		return accelerator
	}
	if last, ok := a.lastAccelerator[key]; ok {
		// Touched on READ as well as on write: the memo should live as long as the
		// variant is being analysed, not as long as its accelerator keeps
		// resolving -- a variant that stops resolving is exactly the one this
		// exists for. A variant that stops being analysed goes quiet on both, and
		// EvictStaleHistory then ages it out with the k2 history beside it.
		last.lastUsed = time.Now()
		a.lastAccelerator[key] = last
		return last.name
	}
	return accelerator
}

func canonicalRole(role string) string {
	if role == "" {
		return domain.RoleBoth
	}
	return role
}

// roleSaturated reports whether any replica of the given role is full and
// queued this cycle: its local queue at or over the threshold, as computeK2
// admits a P1-obs reading on, AND its resident KV at or over its k1 (the
// cache times kvCacheThreshold) and within the cache's physical ceiling (a
// reading above it is a scrape artifact there, and no more a saturation
// here). The queue alone would not do: a decode request waiting for its
// remote KV sits in vLLM's waiting count, so a slow transfer keeps a queue
// on a decode that is admitting fine. A row with no cache size cannot be
// judged and does not count. Every row counts, bridges included: a
// warm-pool Pod lent to decode that is full and queued is decode saturated
// as much as one of its own replicas is.
func roleSaturated(metrics []domain.ReplicaMetrics, rolesByVariant map[string]string, role string, queueThreshold, kvCacheThreshold float64) bool {
	for _, rm := range metrics {
		if canonicalRole(rolesByVariant[rm.VariantName]) != role {
			continue
		}
		if rm.QueueLength < int(queueThreshold) || rm.TokensInUse <= 0 || rm.TotalKvCapacityTokens <= 0 {
			continue
		}
		if rm.TokensInUse > rm.TotalKvCapacityTokens {
			continue
		}
		if rm.TokensInUse >= memoryBound(rm.TotalKvCapacityTokens, kvCacheThreshold) {
			return true
		}
	}
	return false
}

// memoryBound is k1: the KV cache times kvCacheThreshold, truncated -- the
// one formula for it, so the gate's "full" is the capacity the replica is
// priced at.
func memoryBound(totalKvCapacityTokens int64, kvCacheThreshold float64) int64 {
	return int64(float64(totalKvCapacityTokens) * kvCacheThreshold)
}

// roleHold is what holdPrefillDemand did to prefill's demand: the figure it
// found, the figure it left, and the band it clamped into.
type roleHold struct {
	before, after, lo, hi float64
}

// holdPrefillDemand clamps roleDemand[prefill] into the band where the
// engine neither orders nor releases -- [scaleDown x supply, scaleUp x
// anticipated supply], the demands at which applyUniversalThreshold's RC
// and SC are both zero -- and reports whether it moved. No prefill demand
// entry, no prefill supply anticipated, or a demand already inside the band
// leaves it alone. When the band is empty the cap wins -- a hold that
// cannot avoid both errors must not order -- though with the config
// refusing a scale-down boundary at or above the scale-up threshold and
// each variant's starting term clamped at zero (aggregation.startingReplicas),
// anticipated supply is never below supply and the band is never empty in
// practice. The
// variants' own demand and utilization are moved with the role figure.
// undispatched is the scheduler queue's share of prefill demand: requests the
// gateway is holding that have been given to NO pod. It is the one part of
// prefill's demand the hold must not clamp, and the distinction is physical.
//
// A request whose KV sits on a prefill replica awaiting transfer has already
// been prefilled and has already produced its first token; what it waits for
// is decode, and ordering prefill for it buys nothing. A request in the
// scheduler's queue has been prefilled by nobody. Prefill capacity is exactly
// what it is waiting for, whatever decode is doing, and until it gets some it
// has no first token at all.
//
// Holding both together is what left prefill at one replica through a
// 600-deep queue while decode sat at its ceiling: the clamp lands on
// scaleUp x supply, which is the figure RC = D/scaleUp - anticipated turns
// into exactly zero, so no amount of queued work could order a replica.
// Measured on run PM: demand 550,077 clamped to 495,529, RC 0, for 19 cycles.
func holdPrefillDemand(roleDemand map[string]float64, variants []domain.VariantCapacity, scaleUp, scaleDown, undispatched float64) (roleHold, bool) {
	const role = domain.RolePrefill
	before, ok := roleDemand[role]
	if !ok || scaleUp <= 0 || scaleDown <= 0 {
		return roleHold{}, false
	}
	rc, ok := aggregation.AggregateByRole(variants)[role]
	if !ok || rc.TotalAnticipatedSupply <= 0 {
		return roleHold{}, false
	}
	h := roleHold{before: before, lo: scaleDown * rc.TotalSupply, hi: scaleUp * rc.TotalAnticipatedSupply}
	h.after = min(before, h.hi)
	if h.lo <= h.hi {
		h.after = max(h.after, h.lo)
	}
	// Never below the undispatched share. The band exists to stop prefill
	// being ordered or released on DECODE's backlog; work no pod has started
	// is not that, and clamping it away is what made the queue unable to
	// order anything. Bounded by `before` so this can only decline to hold
	// demand that was already there -- it never invents any.
	if undispatched > h.hi {
		h.after = max(h.after, min(undispatched, before))
	}
	if h.after == before {
		return h, false
	}
	roleDemand[role] = h.after
	// The per-variant figures follow, or the hold does not survive the split:
	// the optimizer prices each variant of a role by its share of the ROLE
	// demand (allocation.variantDemandShare), taken from the variants' own
	// TotalDemand, and the sticky scale-down tests a variant's release
	// against that priced figure. Left raw, the variant whose replica showed
	// decode's backlog would carry nearly the whole held total, and a sibling
	// idling on a genuine reading nearly none. Shared out by anticipated
	// supply instead -- the hold's point is that prefill's measurement is
	// nobody's this cycle, so every variant reads the band's figure at its
	// own size. Utilization moves with it, so what reads it (the warm pool's
	// pressure) sees the held figure too.
	for i := range variants {
		vc := &variants[i]
		if canonicalRole(vc.Role) != role {
			continue
		}
		// Through aggregation, not by hand: rc.TotalAnticipatedSupply is the
		// sum of exactly this term, and a numerator computed from
		// PendingReplicas while the denominator subtracted StuckReplicas made
		// the shares sum to more than 1 and over-distributed the held demand.
		anticipated := aggregation.AnticipatedSupply(*vc)
		vc.TotalDemand = h.after * anticipated / rc.TotalAnticipatedSupply
		vc.Utilization = 0
		if supply := float64(vc.ReplicaCount) * vc.PerReplicaCapacity; supply > 0 {
			vc.Utilization = vc.TotalDemand / supply
		}
	}
	return h, true
}

// waitingQueueDemand estimates the KV-token demand of the requests waiting in a
// replica's local engine queue (vllm:num_requests_waiting / sglang:num_queue_reqs).
// Their footprint is projected from the replica's average request shape, since
// the queue metric carries no per-request token counts.
//
// Note these requests are not uniformly blockless: on the decode side a
// transfer-pending request (vLLM's WAITING_FOR_REMOTE_KVS) has already had
// blocks allocated, so part of its prompt KV is also inside KvCacheUsage and is
// counted twice. The overlap shrinks toward zero under KV pressure, when block
// allocation starts failing — i.e. in the saturated regime where the scaling
// decision is actually made.
//
// The per-request cost is role-aware, because a replica only pays for the KV it
// actually materializes:
//
//	Prefill:      AvgInputTokens                   (prompt KV only; output is generated elsewhere)
//	Decode/Both:  AvgInputTokens + AvgOutputTokens (holds prompt KV and grows it per generated token)
//
// Charging decode replicas for input alone understates demand for
// long-generation workloads, which are precisely the ones whose KV pressure is
// output-driven. That is the under-reporting this addresses. It mirrors the role
// attribution estimateSchedulerQueueDemand already applies model-wide.
//
// # Why the full output length, and how it relates to the capacity side
//
// I+O is a request's KV footprint at its LAST decode step, not its mean. It is
// deliberately a peak, no-preemption planning charge: once admitted, a decode
// request's KV grows monotonically and the engine cannot shed it without
// preemption and recompute, so a replica expected to host the request needs room
// for its final size.
//
// I+O is this analyzer's demand-side unit. It is deliberately not the unit used
// by the time-averaged models elsewhere, and the distinction matters when reading
// the two together:
//
//   - Saturation V2 (here, demand): I+O — peak footprint. Sizes for what a
//     replica must be able to hold, not what it holds on average.
//   - Throughput analyzer (shape.Shape.KVreq): ILeff + O/2 —
//     time-averaged. A SEPARATE analyzer with its own model and its own
//     supply/demand pairing; it does not constrain this term and is not
//     inconsistent with it.
//
// One asymmetry is genuinely internal to this analyzer and should not be confused
// with the above: estimateCapacityFromParams prices a *concurrent* request at
// I + O/2 when deriving k2, so a queued request is charged more than it will
// occupy on average, and its charge drops once it is admitted and starts being
// measured through KvCacheUsage instead. Both effects bias this term toward
// scale-up.
//
// Note also that this term and the resident term are both derived from 1-minute
// maxima (max_over_time on kv_cache_usage_perc and num_requests_waiting), whose
// peaks need not coincide, so their sum can exceed any demand the replica
// actually saw at a single instant. That is a pre-existing property of the
// collector's queries, not of this function, but it compounds the bias above.
//
// That trade is chosen on purpose: under-provisioning decode capacity causes
// preemption and recompute thrash, which costs more than a spare replica.
// Revisiting it means changing the demand/capacity pair together, not this
// function alone.
//
// Returns 0 when the queue is empty, or when token metrics are absent or
// non-finite.
func waitingQueueDemand(rm domain.ReplicaMetrics, role string) int64 {
	if rm.QueueLength <= 0 {
		return 0
	}

	tokensPerRequest := rm.AvgInputTokens
	if canonicalRole(role) != domain.RolePrefill {
		tokensPerRequest += rm.AvgOutputTokens
	}
	// Compute in float64 and validate before converting, so one check covers
	// NaN, infinities, and finite values too large for an int64. Converting an
	// out-of-range float64 to int64 is implementation-defined in Go (amd64
	// yields math.MinInt64, arm64 saturates), and either way the result is
	// garbage that reads downstream as "idle, remove replicas" or as enormous
	// demand. Note `x <= 0` would NOT catch NaN, since every NaN comparison is
	// false. The collector filters NaN/Inf on the token averages, so this is a
	// guard rather than a live bug; it does not filter QueueLength, which is a
	// bare int conversion, but a garbage value there is caught either by the
	// QueueLength <= 0 check above or by the range bound below.
	//
	// Multiplying before truncating also drops the per-request rounding the
	// previous truncate-then-multiply form accumulated: at queue 3 and an average
	// of 100.9 tokens this yields 302 rather than 300.
	// The bound is >=, not >: float64(math.MaxInt64) rounds up to 2^63, which is
	// one past the largest representable int64, so a demand of exactly 2^63 would
	// pass a > check and then overflow to MinInt64.
	demand := float64(rm.QueueLength) * tokensPerRequest
	if !(demand > 0) || demand >= float64(math.MaxInt64) {
		return 0
	}

	return int64(demand)
}

// schedulerQueueDemand holds the estimated token demand from scheduler-queued
// requests, broken down by P/D role for disaggregated models.
type schedulerQueueDemand struct {
	total  float64            // model-level total (inputTokens + outputTokens)
	byRole map[string]float64 // per-role demand: "prefill", "decode", "both"
}

// estimateSchedulerQueueDemand estimates the token demand from requests queued
// in the llm-d inference scheduler's flow control layer, with per-role
// attribution for P/D disaggregated models.
//
// These requests have not yet reached any engine pod, so we estimate their
// token footprint using two independent signals:
//
//	inputTokens = max(queueBytes / BytesPerToken, queueSize * avgInputTokens)
//	             * (1 - prefixCacheHitRate)
//	outputTokens = queueSize * avgOutputTokens
//
// Role attribution:
//   - Prefill: inputTokens, discounted by PREFILL's own hit rate (see below)
//   - Decode:  outputTokens when a prefill role exists, inputTokens +
//     outputTokens otherwise (see "charging a queue to the role that serves it")
//   - Both:    inputTokens + outputTokens (handles full request lifecycle)
//   - Model-level total: inputTokens + outputTokens, EXCEPT where a prefill
//     role is active, in which case it is prefillInputTokens + outputTokens so
//     that the per-role charges sum back to it. See the comment above the
//     `total` assignment; "unchanged for backward compat" is what this used to
//     say, and it is the premise the split disproved.
//
// The prefix cache hit rate reduces expected input token KV demand because
// a fraction of prompt tokens will hit the prefix cache and reuse existing
// KV blocks. This does NOT apply to the local engine queue
// (vllm:num_requests_waiting / sglang:num_queue_reqs) because those requests
// have not yet had prefix cache lookup performed.
//
// prefillHitRate is the PREFILL role's own hit rate (fleetPrefixHitRate), and
// the prefill role's charge is discounted by it rather than by the model-wide
// average, BECAUSE THE DIVISOR IS. saturatedCompletionRate prices a prefill
// replica as PrefillComputedTokenRate / ILeff, where ILeff carries exactly this
// factor; the quotient is a replica count only if the dividend carries it too.
// The model-wide average is a different figure over a different set -- every
// replica with token activity, decode included, unweighted -- so on a P/D fleet
// the two diverge with the role ratio. At one prefill replica reading 0.8 and
// nine decode replicas reading 0.0 the average is 0.08: the charge would keep
// 92% of the prompt while the divisor kept 20% of it, inflating mu fivefold
// against the demand it is divided into and under-ordering prefill by the same
// factor -- worse the larger decode grows, which is the fleet this attribution
// exists for. Passing the one variable to both sides is what makes them cancel;
// its VALUE does not have to be right for the quotient to be a replica count,
// and when there is no prefill reading at all both sides fall back to 0
// together and no discount is taken on either.
//
// # Charging a queue to the role that serves it
//
// A queued request is ONE backlog, and it used to be charged in full to both
// roles: prefill got its input tokens and decode got the same input tokens plus
// the output. That is right in two cases and wrong in a third.
//
// It is right with NO DISAGGREGATION. A RoleBoth pod computes the prompt and
// generates from it, so the whole request is its work and the charge is the
// work. This branch is unchanged for that case, and for an unknown role.
//
// It is also right AT A STEADY SHAPE, even disaggregated -- not because the
// figure is accurate but because it is consistently inaccurate. mu is learned
// at the same shape the demand is charged at, so a fixed over-count divides out
// of demand/mu and the replica count survives it, exactly as the hit rate above
// does.
//
// It breaks on a DISAGGREGATED fleet when the shape turns input-heavy, because
// then the over-count stops being fixed. At 1000 in / 6000 out the charge is
// dominated by output, which is decode's real work. After the trace flips to
// 30000 in / 250 out the input term is 120x the output and swamps it, so decode
// is sized by prompts it does not compute and cannot hold until prefill has
// handed them over. Measured on that trace: decode's demand was 96.65% gateway
// queue charge against 2.74% resident KV, and at the peak cycle it was charged
// 1.32e8 tokens while holding 3.63e5 -- a factor of 366. It sat at eight
// replicas whose KV cache was 2% full, with one request waiting, beside a
// prefill side queueing 249.
//
// So the two roles are separated: prefill is charged the tokens it must
// compute, decode the tokens it must generate. Decode's share of the prompt is
// not dropped, it is DEFERRED to where it is real -- the resident KV that
// aggregateRoleDemand already counts from the engines, which rises as prefill
// actually delivers. The queue is charged once, to the role the queue is
// waiting on.
func estimateSchedulerQueueDemand(
	sq *domain.SchedulerQueueMetrics,
	replicaMetrics []domain.ReplicaMetrics,
	rolesByVariant map[string]string,
	activeRoles map[string]bool,
	prefillHitRate float64,
) schedulerQueueDemand {
	if sq == nil || (sq.QueueSize == 0 && sq.QueueBytes == 0) {
		return schedulerQueueDemand{}
	}

	// Compute model-level averages from replica metrics. The output length
	// comes from the replicas that generate output (see
	// computeModelWorkloadAverages): a prefill replica's ~1 must not dilute the
	// per-request charge below.
	avgInput, avgOutput, avgHitRate := computeModelWorkloadAverages(replicaMetrics, rolesByVariant)

	// Estimate input tokens from two signals, take the max for robustness
	tokensFromBytes := float64(sq.QueueBytes) / BytesPerToken
	tokensFromCount := float64(sq.QueueSize) * avgInput
	inputTokens := tokensFromBytes
	if tokensFromCount > inputTokens {
		inputTokens = tokensFromCount
	}

	// Apply prefix cache hit rate reduction to input tokens only
	inputTokensRaw := inputTokens
	inputTokens *= (1 - avgHitRate)

	// The same reduction at the PREFILL role's own hit rate, for the prefill
	// charge alone. Clamped the way shape.New clamps the figure the divisor is
	// built from, so the two cannot disagree about a reading out of range.
	prefillDiscount := prefillHitRate
	if math.IsNaN(prefillDiscount) || prefillDiscount < 0 {
		prefillDiscount = 0
	}
	if prefillDiscount > 1 {
		prefillDiscount = 1
	}
	prefillInputTokens := inputTokensRaw * (1 - prefillDiscount)

	// Estimate output tokens (no cache reduction — output must be generated)
	outputTokens := float64(sq.QueueSize) * avgOutput

	// On a DISAGGREGATED fleet the model total is what the two roles are
	// charged between them, and it has to be, because the roles are charged
	// disjoint slices of it and everything downstream now relies on their
	// summing back to it -- see the note in throughput_floor.go where
	// heldInModelTotal() used to stand.
	//
	// The two prompt figures are not the same number. The total's prompt is
	// discounted at the model-wide mean hit rate and prefill's charge at
	// PREFILL's own, which the fleet that motivated this diverges sharply on:
	// one prefill replica reading 0.8 beside nine decode replicas reading 0.0
	// gives a mean of 0.08. Left on the mean the total would carry 920,000
	// tokens of prompt where prefill is charged 200,000 -- a 720,000-token
	// shortfall between the total and the sum of its roles, owned by nothing,
	// which is the exact fault the split was made to remove.
	//
	// So the total follows the charge, not the other way round. An aggregated
	// fleet keeps the model-wide figure: there is one role, it is charged the
	// whole request, and no split has happened to be consistent with.
	//
	// The condition is PREFILL ALONE, not prefill-and-decode. It was the pair
	// at first, and that left the same divergence behind on a smaller fleet:
	// with a prefill role active and decode absent from activeRoles -- decode
	// scaled to zero, or simply carrying no VariantCapacity this cycle --
	// outputTokens is 0 (generatesOutput excludes prefill replicas, so there is
	// nothing to average), and the total fell back to the mean-discounted
	// prompt while prefill was still charged the rate-weighted one. The two
	// figures are then the SAME replicas averaged two different ways, which is
	// the fault e3218ce3 exists to remove. Two prefill replicas reading 0.9 at
	// 10 req/s and 0.1 at 1 req/s give a plain mean of 0.50 against a weighted
	// 0.83: on a 100-request queue of 10,000-token prompts the total read
	// 500,000 and prefill was charged 172,727, leaving 327,273 tokens -- 65% of
	// it -- owned by no role.
	//
	// Keyed on prefill alone the identity is exact in every shape: with decode
	// present the total is both charges, with decode absent outputTokens is 0
	// and the total IS prefill's charge. Decode-only is untouched, because
	// prefill not being active is what selects the model-wide figure.
	total := inputTokens + outputTokens
	if activeRoles[domain.RolePrefill] {
		total = prefillInputTokens + outputTokens
	}

	// Build per-role attribution
	byRole := make(map[string]float64)
	if len(activeRoles) > 0 {
		for role := range activeRoles {
			switch role {
			case domain.RolePrefill:
				byRole[domain.RolePrefill] = prefillInputTokens
			case domain.RoleDecode:
				// Disaggregated only when a prefill role is actually active:
				// a decode-labelled variant running alone still serves whole
				// requests, so it keeps the full charge.
				if activeRoles[domain.RolePrefill] {
					byRole[domain.RoleDecode] = outputTokens
				} else {
					byRole[domain.RoleDecode] = inputTokens + outputTokens
				}
			default: // domain.RoleBoth or unknown
				byRole[role] = total
			}
		}
	}

	return schedulerQueueDemand{total: total, byRole: byRole}
}

// k2SourceLabel returns the K2Priority label for the lower-median replica by
// EffectiveCapacity. Sorts a copy and picks index (n-1)/2, which always
// resolves to an actual replica — no average is taken, so even-length slices
// never produce a value that matches no element.
// Returns "" when replicas is empty.
func k2SourceLabel(replicas []capacity.ReplicaCapacity) string {
	if len(replicas) == 0 {
		return ""
	}
	sorted := make([]capacity.ReplicaCapacity, len(replicas))
	copy(sorted, replicas)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].EffectiveCapacity < sorted[j].EffectiveCapacity
	})
	medIdx := (len(sorted) - 1) / 2
	if label := sorted[medIdx].K2Priority.String(); label != "" {
		return label
	}
	return domain.ReasonError
}

// median returns the median value from a sorted slice of int64 values.
// Returns 0 if the slice is empty.
//
// Averages the central pair on an even count: this blends learned
// per-replica capacities, where every reading is trusted and the midpoint is
// the better estimate. The floor's median follows the same convention for
// the same reason.
func median(values []int64) int64 {
	n := len(values)
	if n == 0 {
		return 0
	}

	sorted := make([]int64, n)
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// engineParamsFor is the engine configuration recorded for one variant, or
// nil when the capacity store has not seen it yet.
func engineParamsFor(a *SaturationAnalyzer, namespace, modelID, variantName string) *capacity.EngineParams {
	if rec := a.capacityStore.Get(namespace, modelID, variantName); rec != nil {
		return rec.EngineParams
	}
	return nil
}

// itlWindowKey names one ITL(k) window. ITL is a property of the
// accelerator and the engine, so the key carries both the accelerator
// (through stableAccelerator, which absorbs the flapping a heterogeneous
// fleet reports) and the GPU count, and no shape dimension at all.
func (a *SaturationAnalyzer) itlWindowKey(namespace, modelID, variantName, accelerator string, gpuCount int) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", namespace, modelID, variantName,
		a.stableAccelerator(namespace, variantName, accelerator), gpuCount)
}
