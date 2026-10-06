package saturation

import (
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The analyzer itself: its state, its lifecycle, and the lookups that belong
// to neither a decision nor a measurement.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

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

	// itlWindows is one rolling window of (k, ITL) readings, from which
	// ITL(k) = A*k + B is fitted so mu can be derived for the shape the fleet
	// is serving NOW rather than waiting for it to saturate under it
	// (mu_from_itl.go).
	//
	// Keyed by itlPhysicsKey -- model, accelerator, GPUs per replica and the
	// ENGINE-CONFIGURATION fingerprint -- and so NOT by namespace or variant
	// name. ITL(k) is a property of the weights, the engine build and the
	// hardware, which is the same reason the window is not cleared on a shape
	// change. The consequence is deliberate: a renamed variant, a second
	// identical variant, and the same deployment in another namespace all
	// share one window instead of each paying the fit again.
	itlWindows map[string]*itl.Window
	// itlContributors is which variants have fed each itlWindows key, by
	// namespace|variant, with when each was last seen feeding it. Pooling
	// needs it for one reason: Add evicts the oldest observation at capacity
	// whoever contributed it, so N variants sharing a 20-slot window would
	// hold 20/N cycles each. The window is grown to DefaultWindowMaxSize per
	// contributor (itl.Window.GrowMaxSize).
	//
	// The timestamp is not decoration. The first version held a bool and was
	// only ever cleared wholesale when its window went empty, which meant a
	// window kept alive by ONE variant remembered every variant that had ever
	// shared it -- the same "one entry per variant that has EVER been seen"
	// leak EvictStaleHistory exists to stop, reintroduced in a new map. Worse
	// than the memory: len() of this set is what drives GrowMaxSize, so dead
	// contributors permanently inflated a live window's capacity.
	itlContributors map[string]map[string]time.Time
	// itlBaseline is the last B an OLS fit produced, keyed by itlWindowKey --
	// namespace, model, VARIANT, accelerator, GPUs -- and deliberately NOT by
	// the physics key the window above uses.
	//
	// B is the zero-contention decode step measured on one variant's own
	// hardware, and it is what the one-parameter fallback pins. Pooling it
	// would let one variant's measured floor decide the steepness of every
	// sibling's fit, which is the hazard the eviction comment has always
	// described: "pin a hardware floor measured before a redeploy onto
	// different hardware into every later fit for that key".
	itlBaseline map[string]float64
	// startSeconds is the running estimate of how long one replica of each
	// variant takes to become Ready, keyed by itlWindowKey. The demand floor
	// projects the backlog forward over it, so it is a measurement -- see
	// start_est.go for why a constant cannot do.
	//
	// Variant-keyed for a stronger reason than the baseline: a start time is
	// an image pull, a node and a PVC, and is NOT a function of the engine
	// configuration at all. startEstimates takes the MINIMUM over a role, so
	// on the physics key the fastest-starting pod anywhere sharing a
	// fingerprint would set the horizon T for every fleet that shares it, and
	// under-projecting the backlog under-orders.
	startSeconds map[string]float64
	// variantSeenAt is, per itlWindowKey, the last cycle that variant was
	// seen. It is what ages itlBaseline, startSeconds and startOutliers now
	// that they no longer share a key with the window whose emptiness used to
	// evict them (EvictStaleHistory).
	variantSeenAt map[string]time.Time
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
		itlContributors:        make(map[string]map[string]time.Time),
		variantSeenAt:          make(map[string]time.Time),
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
			delete(a.itlContributors, key)
		}
	}
	// A window kept alive by one variant must still forget the variants that
	// have STOPPED feeding it. Without this, the loop above only ever clears a
	// contributor set wholesale, so a renamed or deleted variant stayed in the
	// set of every window it had ever shared -- and because len(set) is what
	// GrowMaxSize scales by, a dead contributor permanently inflated a live
	// window's capacity.
	//
	// The window's own size does NOT shrink back, deliberately: a contributor
	// that goes quiet for a cycle must not discard the history of the ones
	// still reporting, and Prune already bounds the window by
	// DefaultObservationMaxAge whatever its capacity, so an over-sized window
	// holds stale observations for no longer than a right-sized one. What this
	// fixes is that growth is driven by LIVE contributors from here on.
	for key, set := range a.itlContributors {
		for contributor, seen := range set {
			if now.Sub(seen) > timeout {
				delete(set, contributor)
			}
		}
		if len(set) == 0 {
			delete(a.itlContributors, key)
		}
	}
	// The baseline, the start estimate and its outlier count are keyed per
	// VARIANT and no longer share a key with the window above, so the window
	// going empty can no longer evict them. They age on the variant's own
	// last-seen instead, which is a timestamp of its own rather than an
	// inference from someone else's emptiness.
	//
	// The reasons they must still age are unchanged and are the ones the old
	// comment here gave: a learned hardware floor would otherwise pin a figure
	// measured before a redeploy onto different hardware into every later fit,
	// and a stale start estimate would size every later projection.
	for key, seen := range a.variantSeenAt {
		if now.Sub(seen) <= timeout {
			continue
		}
		delete(a.variantSeenAt, key)
		delete(a.itlBaseline, key)
		delete(a.startSeconds, key)
		delete(a.startOutliers, key)
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

// engineParamsFor is the engine configuration recorded for one variant, or
// nil when the capacity store has not seen it yet.
func engineParamsFor(a *SaturationAnalyzer, namespace, modelID, variantName string) *capacity.EngineParams {
	if rec := a.capacityStore.Get(namespace, modelID, variantName); rec != nil {
		return rec.EngineParams
	}
	return nil
}

// itlWindowKey names one VARIANT's per-key state: the learned ITL baseline,
// the replica start estimate and its outlier count. It carries the
// accelerator (through stableAccelerator, which absorbs the flapping a
// heterogeneous fleet reports) and the GPU count, and no shape dimension at
// all.
//
// It no longer names the ITL window itself -- see itlPhysicsKey. The two are
// separate because a window is a property of the configuration while a
// hardware floor and a start time are properties of a deployment.
func (a *SaturationAnalyzer) itlWindowKey(namespace, modelID, variantName, accelerator string, gpuCount int) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", namespace, modelID, variantName,
		a.stableAccelerator(namespace, variantName, accelerator), gpuCount)
}

// noteVariantSeen stamps a variant key as seen this cycle, which is what ages
// the variant-keyed learned state in EvictStaleHistory.
func (a *SaturationAnalyzer) noteVariantSeen(key string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.variantSeenAt[key] = now
}

// noteITLContributor records that a variant feeds a window, and returns how
// many distinct variants now do. The count scales the window's capacity: see
// itlContributors and itl.Window.GrowMaxSize.
//
// Counted by namespace|variant rather than by variant name alone, because the
// physics key drops the namespace and two namespaces can serve the same
// deployment -- which is the pooling this exists to make correct, so they must
// count as two contributors and not one.
func (a *SaturationAnalyzer) noteITLContributor(
	windowKey, namespace, variantName string, now time.Time,
) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	set, ok := a.itlContributors[windowKey]
	if !ok {
		set = make(map[string]time.Time, 2)
		a.itlContributors[windowKey] = set
	}
	set[namespace+"|"+variantName] = now
	return len(set)
}

// itlPhysicsKey names one ITL(k) window, by what the line is a function of:
// the model, the hardware, and the engine configuration that produced it.
// Namespace and variant name are deliberately absent.
//
// An EMPTY fingerprint falls back to the variant key, which pools nothing.
// That is the safe direction and it is not a detail: a fingerprint is absent
// when the configuration could not be read, and treating "unknown" as a
// shared identity would pool engines that have nothing in common -- including
// a 0.6B and a 32B model, the worst mis-keying available here. Paying the fit
// again is the lesser cost by a wide margin.
func (a *SaturationAnalyzer) itlPhysicsKey(
	namespace, modelID, variantName, accelerator string, gpuCount int, fingerprint string,
) string {
	if fingerprint == "" {
		return a.itlWindowKey(namespace, modelID, variantName, accelerator, gpuCount)
	}
	return capacity.LearnedStateKey(modelID,
		a.stableAccelerator(namespace, variantName, accelerator), gpuCount, fingerprint)
}

// engineFingerprint returns the fingerprint of the engine configuration a
// variant is running, or "" when the capacity store holds no parsed params
// for it. Read from the store rather than re-parsed so the key matches the
// digest published as wva_engine_config.
func (a *SaturationAnalyzer) engineFingerprint(namespace, modelID, variantName string) string {
	if a.capacityStore == nil {
		return ""
	}
	rec := a.capacityStore.Get(namespace, modelID, variantName)
	if rec == nil || rec.EngineParams == nil {
		return ""
	}
	return rec.EngineParams.Fingerprint()
}
