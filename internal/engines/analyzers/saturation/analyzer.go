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
	// variantSeenAt is when each variant key was last REPORTED, which is what
	// the learned per-variant state is swept on.
	//
	// It exists because an empty ITL window is not evidence that a variant is
	// gone. Window.Add admits only k in [DefaultMinObservableK,
	// DefaultMaxObservableK], so a healthy, under-utilised fleet below k=0.15
	// offers readings every cycle and holds NONE -- its window is permanently
	// empty while the variant is perfectly live. Keying the sweep on
	// Len() == 0 therefore deleted the learned start estimate and the ITL
	// baseline of exactly the fleets that were doing fine, every cycle; and
	// because startSeenPods is re-stamped for a Pod still being reported,
	// noteReplicaStart could never put the start estimate back. The figure was
	// gone until the Pod was replaced.
	//
	// Stamped by noteITL and by noteReplicaStart, so it covers every role
	// rather than decode alone -- which also bounds the start estimate of a
	// prefill or RoleBoth variant, whose keys the window cascade could never
	// visit and so never swept at all. Both producers stamp their own key,
	// rather than one relying on the other having run first.
	//
	// "REPORTED" means a replica of it was attributable this cycle, which is
	// not the same as existing. A model with no attributable replicas skips
	// analysis entirely (steadystate, the len(replicaMetrics) == 0 branch), so
	// a fleet parked at zero is unstamped for as long as it is parked: past
	// HistoryEvictionTimeout it re-learns its ITL baseline and start estimate
	// on wake, which is the one case this timeout is felt rather than just
	// bounding memory. The capacity STORE is what carries a parked variant --
	// deliberately, on a seven-day timeout whose own comment names the
	// weekend -- so engine params and the capacity record survive the gap and
	// only the two fitted figures are re-measured. The asymmetry between the
	// two timeouts is therefore intended, but it is an asymmetry: 24h of
	// quiet costs the fit, seven days costs the record.
	variantSeenAt map[string]time.Time
	// now is the clock the memory reads; tests set it.
	now func() time.Time
}

// Evicted is what one sweep of the analyzer's learned state removed, counted
// per map rather than summed.
//
// Summed, the number could not be acted on: the per-variant state is swept in
// separate loops, so one departed variant counted three times, while the two
// figures an operator would actually want to see -- MuWindows, because losing
// it makes the demand floor abstain or borrow, and K2History, because losing
// it moves k2 to a derived figure -- were indistinguishable from the start
// estimates and baselines that come and go harmlessly.
//
// Zero value means nothing was evicted, which is the common case; Any reports
// it so the caller can stay silent on the cycles that found nothing.
type Evicted struct {
	// K2History is compute-capacity (k2) windows, and MuWindows the
	// saturated-throughput windows the demand floor prices from.
	K2History int
	MuWindows int
	// ITLWindows and ITLBaselines are the fitted ITL(k) state; StartEstimates
	// and StartOutliers the learned replica start time and its reject
	// counter; Pods the per-Pod records that say a start is already counted.
	ITLWindows     int
	ITLBaselines   int
	StartEstimates int
	StartOutliers  int
	Pods           int
	// Accelerators is the resolved-accelerator memo, DecodeSaturation the
	// per-model decode-saturation memory, FleetShapes the per-model shape memo
	// and VariantStamps the liveness stamps themselves. None is read as a
	// figure; they are counted because an unbounded map is the thing being
	// fixed, and because a deletion nothing counts makes Any() false on a
	// cycle that evicted something -- which silences the one log line this
	// sweep emits. A reviewer found both of the last two uncounted.
	Accelerators     int
	DecodeSaturation int
	FleetShapes      int
	VariantStamps    int
}

// Any reports whether the sweep evicted anything at all.
func (e Evicted) Any() bool {
	return e.K2History > 0 || e.MuWindows > 0 || e.ITLWindows > 0 ||
		e.ITLBaselines > 0 || e.StartEstimates > 0 || e.StartOutliers > 0 ||
		e.Pods > 0 || e.Accelerators > 0 || e.DecodeSaturation > 0 ||
		e.FleetShapes > 0 || e.VariantStamps > 0
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
		variantSeenAt:          make(map[string]time.Time),
		now:                    time.Now,
	}
}

// Name returns the analyzer identifier for logging and result metadata.
// Note: the config value "saturation" (in analyzerName YAML field) selects this analyzer,
// but the descriptive name here is used in AnalyzerResult.AnalyzerName for observability.
func (a *SaturationAnalyzer) Name() string {
	return "saturation-token-based"
}

// noteVariantSeen records that a variant key was reported this cycle. The
// learned state keyed on it is swept on this timestamp and nothing else.
//
// Callers hold a.mu.
func (a *SaturationAnalyzer) noteVariantSeen(key string, now time.Time) {
	a.variantSeenAt[key] = now
}

// variantIsStale reports whether nothing has reported a variant key within the
// timeout. A key with no stamp at all counts as stale: every producer of
// learned state stamps one, so an unstamped key is left over from before a key
// changed shape and is exactly what the sweep is for.
//
// Callers hold a.mu.
func (a *SaturationAnalyzer) variantIsStale(key string, now time.Time, timeout time.Duration) bool {
	seen, ok := a.variantSeenAt[key]
	return !ok || now.Sub(seen) > timeout
}

// EvictStaleHistory removes learned state that is past its horizon, which
// prevents unbounded memory growth from deleted models, renamed variants and
// workload buckets that are no longer active.
//
// Two horizons, because the maps are keyed differently and "no longer needed"
// means different things:
//
//   - variantTimeout governs the PER-VARIANT state -- ITL window and baseline,
//     start estimate and its outlier counter -- measured from when the variant
//     was last REPORTED (variantSeenAt), plus the per-model memos beside them.
//   - bucketRetention governs the BUCKET-KEYED windows, the k2 history and the
//     mu windows, measured from when they were last READ. It is the longer of
//     the two because a retained measurement is the LOWEST capacity figure
//     available for its bucket: dropping it hands the decision to a formula,
//     and the fleet is repriced upward. See capacity.HistoryRetention for the
//     measured cost. Nothing refuses a window on age any more -- an earlier
//     revision did, and this comment used to describe it.
//
// Everything is pruned here rather than in a second sweep so the horizons
// cannot drift, which has happened twice: both the accelerator memo and the
// fleet-shape memo RESOLVE INTO a bucket key, so each has to outlive the
// window its value reaches, and each was found expiring six days early. Both
// are on bucketRetention for that reason -- see the two loops below, which say
// so where a reader will be standing. The decode-saturation memory is per
// namespace|model and stays on variantTimeout: nothing keys off it, and
// DecodeSaturationMemory is minutes, so the horizon cannot affect what it is
// for.
//
// The return is a per-map breakdown rather than one total, because a single
// number could not be read: the per-variant state is swept in separate loops
// (window-emptiness stopped being the liveness test), so one departed variant
// incremented a shared counter three times, and the one figure a total could
// have meant -- "the demand floor just lost its mu" -- was not counted at all.
// Each field names the map it came from, and the caller in steadystate logs
// them under those names.
func (a *SaturationAnalyzer) EvictStaleHistory(variantTimeout, bucketRetention time.Duration) Evicted {
	a.mu.Lock()
	defer a.mu.Unlock()
	var evicted Evicted
	for key, ra := range a.computeCapacityHistory {
		if ra.Stale(bucketRetention) {
			delete(a.computeCapacityHistory, key)
			evicted.K2History++
		}
	}
	// bucketRetention, NOT variantTimeout, although this memo is per
	// variant. It is the value that RESOLVES into the k2 history key, so
	// expiring it ahead of the window it keys silently moves the key: the
	// accelerator reads unresolved, the lookup misses a window that is
	// still retained, and k2 falls through to the derived figure -- which
	// is exactly what retaining the window past its last read is meant to
	// prevent. A reviewer found this; on the 24h horizon there was a
	// six-day band where the measurement existed and could not be reached.
	//
	// One residual, named because the window for it widened from one day to
	// seven. The THROUGHPUT key resolves through stableAccelerator too, and
	// the mu it keys drives the demand floor as lambda/mu with no min()
	// protecting it -- so a mu learned on larger hardware is too high and
	// orders FEWER replicas. It needs an unresolvable accelerator on a
	// variant that went quiet and came back on different hardware, and the
	// mu window already ages on bucketRetention, so this aligns the memo with
	// the window it keys rather than creating the exposure. Capacity itself is
	// safe either way: k2 from the wrong hardware is still clamped by the
	// live k1.
	for key, memo := range a.lastAccelerator {
		if time.Since(memo.lastUsed) > bucketRetention {
			delete(a.lastAccelerator, key)
			evicted.Accelerators++
		}
	}
	// The per-VARIANT maps below age on variantSeenAt: the time the variant
	// was last REPORTED. Two other clocks are in play in this function and
	// neither is a leftover. The k2 history and the mu windows age on their
	// own last-use stamp, because they are keyed by workload BUCKET
	// (model|accel|gpus|role|bucket|qN -- no namespace, no variant name), so
	// they cannot be joined to a variant key at all; their reads call Touch
	// so that "last use" is what it says. The accelerator memo, the
	// decode-saturation memory and the fleet shape are per model, not per
	// variant, and age on wall-clock time.Since.
	//
	// The window is still pruned every sweep,
	// because stale observations must not reach a fit, but an empty window is
	// no longer taken as evidence that the variant is gone -- see
	// variantSeenAt's own comment for why it is not, and for what that cost.
	//
	// The intent is unchanged and is the one the first version of this loop
	// stated: without a sweep the maps keep one entry per variant that has
	// EVER been seen, including deleted and renamed ones, and a baseline or a
	// start time measured before a redeploy onto different hardware would size
	// every later fit and projection for that key. Only the liveness test
	// changed, from "its window is empty" to "nothing has reported it".
	now := a.now()
	for _, w := range a.itlWindows {
		w.Prune(now)
	}
	for key := range a.itlWindows {
		if a.variantIsStale(key, now, variantTimeout) {
			delete(a.itlWindows, key)
			evicted.ITLWindows++
		}
	}
	// Separate loops, not one pass over itlWindows: noteReplicaStart runs for
	// every role while noteITL runs for decode only, so a prefill or RoleBoth
	// variant has a start estimate and no window at all. Sweeping these
	// through the window map left those keys unbounded -- the very leak the
	// sweep was wired up to close.
	for key := range a.itlBaseline {
		if a.variantIsStale(key, now, variantTimeout) {
			delete(a.itlBaseline, key)
			evicted.ITLBaselines++
		}
	}
	for key := range a.startSeconds {
		if a.variantIsStale(key, now, variantTimeout) {
			delete(a.startSeconds, key)
			delete(a.startOutliers, key)
			evicted.StartEstimates++
		}
	}
	// Belt and braces, and deliberately kept as such. Today
	// keys(startOutliers) is a strict SUBSET of keys(startSeconds) -- the
	// counter is only ever incremented under `measured` (so the estimate
	// exists) or written to 0 beside an estimate write -- and the loop above
	// deletes both on the same key, so this loop cannot currently fire. It
	// stays because it is what makes startOutliers bounded no matter who
	// writes it next: the subset property is an invariant of two call sites in
	// start_est.go, not of this map's type, and an unbounded per-variant map
	// is the defect this whole function exists to prevent. An earlier version
	// of this comment claimed the loop was reachable, which was wrong.
	for key := range a.startOutliers {
		if a.variantIsStale(key, now, variantTimeout) {
			delete(a.startOutliers, key)
			evicted.StartOutliers++
		}
	}
	for key, seen := range a.variantSeenAt {
		if now.Sub(seen) > variantTimeout {
			delete(a.variantSeenAt, key)
			evicted.VariantStamps++
		}
	}
	for key, ra := range a.saturatedThroughput {
		if ra.Stale(bucketRetention) {
			delete(a.saturatedThroughput, key)
			delete(a.throughputSampledAt, key)
			delete(a.throughputLastRead, key)
			evicted.MuWindows++
		}
	}
	for key, at := range a.decodeSaturatedAt {
		if time.Since(at) > variantTimeout {
			delete(a.decodeSaturatedAt, key)
			evicted.DecodeSaturation++
		}
	}
	// The fleet-shape memo is per namespace|model and exists only to key and
	// stabilise the capacity figures, so it goes when they do. Without this it
	// is the one map on this struct that grows without bound as models and
	// namespaces come and go.
	//
	// maps.DeleteFunc rather than the hand-rolled loops above: those predate
	// it and two of them cannot use it anyway, one because it needs the key
	// to consult another map and one because it deletes from three maps on
	// the same key. Counting is not the obstacle -- the length difference
	// gives it, which is how FleetShapes is filled. An earlier version of
	// this comment said counting was the obstacle and left these evictions
	// unreported, so Any() could be false on a cycle that evicted something
	// and the one log line this sweep emits stayed silent.
	// bucketRetention, and on a.now() rather than time.Since -- for the same
	// reason as the accelerator memo above, which a reviewer had to find twice
	// because this map is the OTHER one that resolves into a bucket key.
	//
	// The (I, O) pair this memo holds is what classifyInputLength and
	// classifyOutputLength turn into the mu window's bucket (see
	// throughputKey). Expiring it ahead of that window moves the key: the
	// shape reads 0, the key lands in the short/ishort bucket, and the
	// retained measurement at the real key cannot be reached -- for up to the
	// six days between the two horizons. nearestSaturatedThroughput cannot
	// rescue it either, because it borrows across OUTPUT buckets within one
	// input bucket and the input bucket moved too. The floor then reads 0,
	// priceable(0) is false, and it abstains for the role.
	//
	// Which lands on the ramp out of a quiet period -- the one moment the
	// floor is load-bearing, and the exact case HistoryRetention's comment
	// names: quiet over a weekend, back on Monday.
	//
	// a.now() because the fake clock is how a spec reaches this at all; the
	// hand-rolled per-variant loops above already use it.
	before := len(a.fleetShape)
	maps.DeleteFunc(a.fleetShape, func(_ string, memo *shapeMemo) bool {
		return now.Sub(memo.lastSeen) > bucketRetention
	})
	evicted.FleetShapes = before - len(a.fleetShape)
	evicted.Pods = a.evictStartSeenPods(now, variantTimeout)
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
// all. It is also the key the per-cycle sweep ages all four of those maps on,
// through variantSeenAt.
func (a *SaturationAnalyzer) itlWindowKey(namespace, modelID, variantName, accelerator string, gpuCount int) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", namespace, modelID, variantName,
		a.stableAccelerator(namespace, variantName, accelerator), gpuCount)
}
