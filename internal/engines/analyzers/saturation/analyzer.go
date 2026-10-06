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

	// itlWindows is one rolling window of (k, ITL) readings PER VARIANT, from
	// which ITL(k) = A*k + B is fitted so mu can be derived for the shape the
	// fleet is serving NOW rather than waiting for it to saturate under it
	// (mu_from_itl.go). Keyed like the throughput windows and swept with them.
	//
	// Per variant, and NOT pooled across the variants that share an engine
	// configuration -- although ITL(k) is largely a property of the weights,
	// the build and the hardware. An earlier version of this file pooled the
	// OBSERVATIONS on that reasoning and it was the wrong mechanism: only A and
	// B ever reach a decision, so what is worth sharing is the fitted LINE, not
	// the data behind it. Sharing the line (itlLines below) is attributable,
	// cannot blend two different traffic shapes into a figure describing
	// neither, and does not make one variant's fit depend on the order the
	// others happened to be iterated in.
	itlWindows map[string]*itl.Window
	// itlLines is the last line each ENGINE CONFIGURATION was measured at,
	// keyed by itlPhysicsKey, with the variant that measured it and when.
	//
	// This is what a variant with no usable fit of its own borrows, so a
	// rename, a second identical variant, or the same deployment in another
	// namespace gets a line on its FIRST cycle instead of paying the ~60
	// cycles a fit takes -- which is the whole saving this was built for. A
	// borrowed line is marked as such all the way to the floor, which may hold
	// the fleet on it but not grow it: the line is evidence about a
	// configuration, not about this variant's own load.
	itlLines map[string]learnedLine
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

// learnedLine is one measured ITL(k) line, with who measured it and when.
//
// fromVariant is the variant key (itlWindowKey) of the fit's owner rather than
// a bare variant name, so "is this my own line" is one comparison and two
// namespaces serving the same deployment are told apart.
type learnedLine struct {
	model       itl.Model
	fromVariant string
	learnedAt   time.Time
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
		itlLines:               make(map[string]learnedLine),
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
	// A shared line ages on a timestamp of its own, because the window that
	// produced it may already be gone -- which is the point of keeping it. It
	// is only ever a starting point for a variant that has nothing of its own,
	// and a line measured a day ago on a configuration nothing runs any more
	// is not one.
	for key, line := range a.itlLines {
		if now.Sub(line.learnedAt) > timeout {
			delete(a.itlLines, key)
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
// It names the ITL window too: the window was never re-keyed, and sharing
// happens on the fitted LINE instead (itlLines, itlPhysicsKey). An earlier
// revision of this file did pool the window on the physics key, and this
// comment described that design; see docs/proposals for why it was replaced.
func (a *SaturationAnalyzer) itlWindowKey(namespace, modelID, variantName, accelerator string, gpuCount int) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", namespace, modelID, variantName,
		a.stableAccelerator(namespace, variantName, accelerator), gpuCount)
}

// publishLine records a variant's OWN fitted line as the line for its engine
// configuration, so a sibling with nothing of its own can borrow it.
//
// Only a variant's own fit is published. A borrowed line is never
// republished, which keeps the record traceable to one measurement instead of
// letting a figure circulate between variants gathering authority it never
// earned.
func (a *SaturationAnalyzer) publishLine(
	physicsKey, variantKey string, model itl.Model, now time.Time,
) {
	if physicsKey == "" || model.IsZero() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.itlLines[physicsKey] = learnedLine{
		model:       model,
		fromVariant: variantKey,
		learnedAt:   now,
	}
}

// borrowLine returns the line last measured for this engine configuration by
// SOME OTHER variant, for a variant whose own window cannot produce one.
//
// It reports the owner so the caller can log what was borrowed and from
// where: a decision made on another variant's measurement has to be visible
// as such. A line this variant measured itself is not a borrow and is not
// returned -- the caller already has it, and calling it borrowed would
// wrongly deny it the right to order.
func (a *SaturationAnalyzer) borrowLine(physicsKey, variantKey string) (itl.Model, string, bool) {
	if physicsKey == "" {
		return itl.Model{}, "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	line, ok := a.itlLines[physicsKey]
	if !ok || line.model.IsZero() || line.fromVariant == variantKey {
		return itl.Model{}, "", false
	}
	return line.model, line.fromVariant, true
}

// itlPhysicsKey names one SHARED LINE record -- not a window -- by what the
// line is a function of:
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
