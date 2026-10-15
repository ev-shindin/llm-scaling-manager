package saturation

import (
	"fmt"
	"maps"
	"strings"
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
	// keyed by "namespace|variant|modelID|accelerator|gpuCount|role|outputBucket|queueThreshold"
	// (historyKey): one variant's readings per window, never another's.
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
	// k2BorrowedSince is, per own historyKey (variant AND bucket), when the
	// variant first borrowed a sibling's k2 for that bucket (borrowedK2). The
	// relaxation back to the variant's own figure is measured from it, so it
	// must NOT be reset by a donor that comes and goes, which would hold a
	// borrowed figure at full strength again. A bucket the variant has never
	// measured gets its own clock: see k2_borrow.go for why per bucket and not
	// per variant. Moved forward by the time the variant did not serve the
	// bucket, so start-to-now is serving time (noteBorrowServing).
	k2BorrowedSince map[string]time.Time
	// k2BorrowState is, per k2BorrowedSince key, the clock's last serving
	// cycle, when it was last read, and what the borrow last ran on. A spent
	// borrow re-arms only on new evidence (borrowState.rearms), so the clock
	// is kept for as long as its variant exists and swept with it.
	k2BorrowState map[string]*borrowState
	// borrowTickGap is the longest gap between two serving cycles a borrow
	// clock counts as serving time (WithOptimizationInterval); never below
	// BorrowedK2TickGap.
	borrowTickGap time.Duration
	// forgotten is, per namespace/variant, the last forget token acted on
	// (ForgetVariant), so an annotation left in place is not acted on again.
	// Swept with the variant.
	forgotten map[string]forgetMark
	// pendingRestore is learned state restored after a controller restart and
	// not yet installed, keyed namespace/variant: it waits for its variant's
	// first cycle, when the engine configuration it runs is known, and is
	// installed only if that is the configuration it was learned under
	// (installRestored, learned_snapshot.go). Swept a retention after the
	// restore, for a variant that never comes back.
	pendingRestore map[string]*pendingVariant
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
	// namespace gets a line on its FIRST cycle instead of paying the ~28
	// cycles a fit takes -- measured on one benchmark run from load start,
	// which is the figure a controller-boot anchor had inflated to ~60. That
	// is the whole saving this was built for. A
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
	// "REPORTED" means the variant was in the cycle's input, or that the
	// engine still found it -- active, or parked at zero replicas -- and said
	// so through NoteVariantsPresent before the sweep. Before that stamp a
	// fleet parked at zero went unstamped for as long as it was parked, and
	// past VariantTimeout re-learned its ITL baseline and start estimate on
	// wake. A deleted variant is not found, so it still ages out.
	variantSeenAt map[string]time.Time
	// now is the clock the memory reads; tests set it.
	now func() time.Time
	// horizons is how long each kind of learned state is kept. Defaulted
	// from capacity.DefaultHorizons and overridable by configuration --
	// which is the only way any of this is reachable on a cluster, the
	// shortest of them being a day.
	horizons capacity.Horizons
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
	// K2Borrows is the per-variant start of a borrowed k2 (borrowedK2).
	K2Borrows int
	// PendingRestores is restored learned state whose variant never came back.
	PendingRestores int
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
	// ITLLines is the fitted lines shared between variants running the same
	// engine configuration. Counted separately because it is the one map here
	// keyed on a CONFIGURATION rather than a variant or a workload bucket, and
	// a reader asking why a cold variant stopped finding a line to borrow
	// needs to see it move.
	ITLLines int
	// ForgetMarks is the per-variant record of the last forget request
	// acted on (ForgetVariant).
	ForgetMarks int
}

// Any reports whether the sweep evicted anything at all.
func (e Evicted) Any() bool {
	return e.K2History > 0 || e.MuWindows > 0 || e.K2Borrows > 0 || e.PendingRestores > 0 || e.ITLWindows > 0 ||
		e.ITLBaselines > 0 || e.StartEstimates > 0 || e.StartOutliers > 0 ||
		e.Pods > 0 || e.ITLLines > 0 || e.Accelerators > 0 || e.DecodeSaturation > 0 ||
		e.FleetShapes > 0 || e.VariantStamps > 0 || e.ForgetMarks > 0
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
		k2BorrowedSince:        make(map[string]time.Time),
		k2BorrowState:          make(map[string]*borrowState),
		borrowTickGap:          BorrowedK2TickGap,
		forgotten:              make(map[string]forgetMark),
		pendingRestore:         make(map[string]*pendingVariant),
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
		variantSeenAt:          make(map[string]time.Time),
		now:                    time.Now,
		horizons:               capacity.DefaultHorizons(),
	}
}

// WithHorizons overrides how long learned state is kept. It returns the
// analyzer, and the names of any fields it had to replace because they were
// unusable -- see capacity.Horizons.Sanitized.
//
// The names are RETURNED rather than dropped, and the signature is two
// values rather than one, because an earlier revision discarded them with
// `a.horizons, _ =` and five comments went on claiming they were logged.
// What that cost: an operator who writes 24 meaning 24 hours gets 24
// NANOSECONDS from viper, is floored back to the 24h default, and reads a
// startup line identical to the one an unconfigured controller prints. A
// second return value cannot be ignored without `_` at the call site, where
// a reviewer can see it; a dropped tuple element inside the function could
// not. The analyzer has no logger of its own, so the engine does the
// logging -- see NewEngine.
func (a *SaturationAnalyzer) WithHorizons(h capacity.Horizons) (*SaturationAnalyzer, []string) {
	sanitized, replaced := h.Sanitized()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.horizons = sanitized
	return a, replaced
}

// Horizons reports the horizons in force, for the caller that logs them.
//
// Takes a.mu because every other reader of this field does (EvictStaleHistory,
// computeK2, recordSaturatedThroughput). Production writes it once, inside
// NewEngine, before the analyzer is reachable from a reconcile goroutine --
// so today the ordering comes from program order rather than this lock. But
// WithHorizons returns the receiver and reads like a general-purpose builder,
// and nothing in the type stops a later hot-reload path from calling it while
// a cycle is running. Locking here costs nothing and removes the hazard
// instead of documenting it.
func (a *SaturationAnalyzer) Horizons() capacity.Horizons {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.horizons
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

// NoteVariantsPresent refreshes the liveness stamp of every variant named, for
// each key it has learned state under, and its accelerator memo. The engine
// calls it once per cycle, before the sweep, for every variant that still
// exists -- active or parked at zero replicas.
//
// Analyze stamps only the variants in its input, and it runs AFTER the sweep.
// That left two gaps, each of which cost a fleet its ITL baseline and start
// estimate after VariantTimeout and woke it on the defaults:
//   - a variant PARKED at zero is inactive, never in the model groups the
//     engine analyses, so nothing stamped it while any other model kept the
//     engine optimizing;
//   - a whole fleet parked for longer than the timeout stops the engine
//     altogether, and on the cycle it wakes the sweep ran before Analyze, so
//     the stale stamp evicted the state on exactly the cycle it was needed.
//
// By name rather than by the key Analyze would build: a parked variant has no
// Pods to observe its accelerator from, so rebuilding the key could miss the
// one its state was learned under. Matching the namespace|model|variant prefix
// keeps every key that variant has, and only that variant's -- the trailing
// separator keeps "v" from matching "v2". A variant that is deleted stops being
// named, and its state is swept as before.
func (a *SaturationAnalyzer) NoteVariantsPresent(namespace, modelID string, variantNames []string) {
	if len(variantNames) == 0 {
		return
	}
	prefixes := make([]string, 0, len(variantNames))
	for _, v := range variantNames {
		prefixes = append(prefixes, namespace+"|"+modelID+"|"+v+"|")
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.variantSeenAt {
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				a.noteVariantSeen(key, now)
				break
			}
		}
	}
	// A borrow's clock is swept on the same liveness, and keyed the k2 way
	// (namespace|variant|model|...), so it is matched on its own prefixes.
	for key, st := range a.k2BorrowState {
		for _, v := range variantNames {
			if strings.HasPrefix(key, namespace+"|"+v+"|"+modelID+"|") {
				st.seen = now
				break
			}
		}
	}
	for _, v := range variantNames {
		if m, ok := a.forgotten[namespace+"/"+v]; ok {
			m.seen = now
			a.forgotten[namespace+"/"+v] = m
		}
	}
	// The memo ages on the wall clock (stableAccelerator), so it is touched on
	// the wall clock too.
	for _, v := range variantNames {
		if memo, ok := a.lastAccelerator[namespace+"/"+v]; ok {
			memo.lastUsed = time.Now()
			a.lastAccelerator[namespace+"/"+v] = memo
		}
	}
	a.notePendingPresent(namespace, prefixes, variantNames, now)
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
func (a *SaturationAnalyzer) EvictStaleHistory() Evicted {
	a.mu.Lock()
	defer a.mu.Unlock()
	variantTimeout, bucketRetention := a.horizons.VariantTimeout, a.horizons.BucketRetention
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
	// own last-use stamp, because they are keyed by workload BUCKET as well as
	// by variant (namespace|variant|model|accel|gpus|role|bucket|qN): a bucket
	// the variant has stopped serving must still expire while the variant
	// lives, which a per-variant stamp cannot express. Their reads call Touch
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
	// On bucketRetention, with the per-configuration maps, because that is
	// what itlLines is keyed on: the digest of an engine configuration, not a
	// variant. A line is shared precisely so a variant with no history of its
	// own can start from one measured elsewhere, so ageing it on the variant
	// horizon would drop it while the configuration it describes is still
	// running -- the same mistake the accelerator and fleet-shape memos were
	// found making, one map over.
	//
	// learnedAt is last WRITTEN, and that is the one place in this function
	// where the last-read clock does not apply: a learnedLine is a finished
	// fit, not an accumulating window, so a read has nothing to keep alive. A
	// line measured on a configuration nothing runs any more is not a starting
	// point for anything.
	for key, line := range a.itlLines {
		if now.Sub(line.learnedAt) > bucketRetention {
			delete(a.itlLines, key)
			evicted.ITLLines++
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
	// A borrow's clock goes with its variant: when nothing has read it,
	// ticked it or found the variant present for VariantTimeout. Not on its
	// age. Dropping a spent clock is what used to re-arm the borrow at full
	// strength a retention after it ended -- a weekly re-probe that held
	// extra replicas for a variant already living on its own figure, with no
	// new evidence behind it (see k2_borrow.go). Not on the variant's own
	// window appearing either: an own window displaces the borrow already
	// (P2 reads it first), and a kept clock is what stops a variant hovering
	// at full strength by moving between its own buckets.
	//
	// A clock with no state predates it -- restored from an older snapshot
	// and never read since -- and keeps the old rule.
	borrowLife := BorrowedK2Hold + BorrowedK2Relax + bucketRetention
	for key, since := range a.k2BorrowedSince {
		st := a.k2BorrowState[key]
		gone := st == nil && a.now().Sub(since) > borrowLife
		if st != nil {
			gone = a.now().Sub(st.seen) > variantTimeout
		}
		if gone {
			delete(a.k2BorrowedSince, key)
			delete(a.k2BorrowState, key)
			evicted.K2Borrows++
		}
	}
	for key := range a.k2BorrowState {
		if _, ok := a.k2BorrowedSince[key]; !ok {
			delete(a.k2BorrowState, key)
		}
	}
	// A forget mark goes with its variant. Kept past it, a variant deleted
	// and re-created under the same name with the same token would not be
	// forgotten; dropped sooner, a variant parked past the timeout would be
	// forgotten again on waking -- which is why NoteVariantsPresent keeps it.
	marks := len(a.forgotten)
	maps.DeleteFunc(a.forgotten, func(_ string, m forgetMark) bool {
		return a.now().Sub(m.seen) > variantTimeout
	})
	evicted.ForgetMarks = marks - len(a.forgotten)
	// Restored entries for a variant that has not been analysed since the
	// restore. A retention later, any entry in them is past its own horizon
	// too, so none could have been installed.
	// Pending entries go when none of them could be installed any more, on
	// their own clocks -- not on when they were restored, which every restart
	// resets and which a parked variant noted present never moves.
	for key, p := range a.pendingRestore {
		if !p.alive(a.now(), time.Now(), a.horizons) {
			delete(a.pendingRestore, key)
			evicted.PendingRestores++
			continue
		}
		p.pruneWindows(time.Now(), a.horizons)
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
// variant is running, or "" when the capacity store holds no parsed params for
// it OR when what it holds was not fully read.
//
// Read from the store rather than re-parsed, so that when there IS a key it is
// computed from the same record wva_engine_config publishes. It is not the
// same as that label in every case -- see the incompleteness note below.
func (a *SaturationAnalyzer) engineFingerprint(namespace, modelID, variantName string) string {
	if a.capacityStore == nil {
		return ""
	}
	rec := a.capacityStore.Get(namespace, modelID, variantName)
	if rec == nil || rec.EngineParams == nil {
		return ""
	}
	// A configuration this controller could not fully READ does not get a
	// sharing key, and an empty fingerprint is how itlPhysicsKey is told to
	// fall back to the per-variant window key -- so such a variant learns its
	// own line and neither lends nor borrows one.
	//
	// THIS DIVERGES FROM wva_engine_config ON PURPOSE, and the doc comment
	// above used to claim it could not: publishEngineConfig hashes the same
	// record unconditionally, so an incomplete variant HAS a published
	// fingerprint and no learned-state key under it. That asymmetry is the
	// honest one -- the metric's job is to show what was read, including that
	// the read was partial, and its `unresolved` label is non-empty for
	// exactly these variants. Joining the two on `fingerprint` finds nothing
	// for them, which is the correct answer to "what has this configuration
	// learned": nothing, by design.
	//
	// Without this the digest would be doing the one thing it must not: an
	// llm-d Deployment passing `--block-size $VLLM_BLOCK_SIZE` leaves the
	// default on every variant, so a whole fleet of differently-configured
	// engines hashes alike and any of them may price itself from another's
	// measured latency line. Hashing the unresolved SET stops an incomplete
	// read colliding with a complete one; it cannot stop two incomplete reads
	// colliding with each other, because the thing they disagree about is
	// exactly what neither could read. Only refusing the key does that.
	//
	// The cost is that such a fleet gets no cross-variant head start, which is
	// the feature degrading to its pre-existing behaviour rather than
	// misfiring. wva_engine_config's `unresolved` label says why.
	if !rec.EngineParams.Complete() {
		return ""
	}
	return rec.EngineParams.Fingerprint()
}
