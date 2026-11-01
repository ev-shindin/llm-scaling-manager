package capacity

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

const (
	// StalenessTimeout is the duration after which a stored capacity
	// record is considered stale (IsStale) and should be refreshed from live
	// data.
	StalenessTimeout = 30 * time.Minute

	// EvictionTimeout is the duration after which unused capacity
	// store records are eligible for removal. This is intentionally long
	// because historical capacity knowledge is valuable for zero-replica
	// estimation and cross-variant matching (e.g., a variant may be at
	// zero replicas over a weekend and scale back up Monday).
	EvictionTimeout = 7 * 24 * time.Hour
)

// Record holds cached capacity knowledge for a specific variant.
// This allows the analyzer to make capacity estimates for variants that
// currently have zero replicas, either from their own prior data or from
// a compatible variant via FindCompatible. A record is usable for that when
// it carries EngineParams and either EffectiveCapacity or
// TotalKvCapacityTokens (FindCompatible's rule).
type Record struct {
	// AcceleratorName and GpuCount are the hardware the record was learned
	// on; FindCompatible matches on both.
	AcceleratorName string
	GpuCount        int
	// NumGpuBlocks and BlockSize are the engine's KV-cache geometry (blocks,
	// tokens per block); TotalKvCapacityTokens is their product, the
	// physical KV capacity in tokens (k1 before the threshold).
	NumGpuBlocks          int64
	BlockSize             int64
	TotalKvCapacityTokens int64
	// EffectiveCapacity is the capacity in tokens the analyzer priced the
	// variant at: min(k1, k2) when learned live, a conservative lower bound
	// (the per-step token budget) when derived from a deployment.
	EffectiveCapacity int64
	// PriorLiveCapacity is the EffectiveCapacity the variant was last
	// measured at under engine arguments it no longer runs, kept when
	// LoadFromScaleTarget replaces a live record after a rollout. It is only
	// ever a CEILING on the estimate a parked variant is priced from
	// (estimateStoredCapacity), never a floor and never k1. The next live
	// write builds a fresh record and so drops it. A move to other hardware
	// drops it too: it was measured on hardware the variant no longer runs on.
	PriorLiveCapacity int64
	// PriorLiveAt is when PriorLiveCapacity was measured: the replaced live
	// record's LearnedAt, kept as the ceiling is carried from cycle to cycle.
	// LoadFromScaleTarget rewrites the record every cycle, so its own
	// LearnedAt never ages; this clock does, and the ceiling is dropped once
	// it is older than EvictionTimeout, as the live record itself would have
	// been.
	PriorLiveAt time.Time
	// EngineParams are the parsed deployment params for k2 derivation.
	EngineParams *EngineParams
	// RollingOut is whether the scale target reported a rollout in progress
	// (scaletarget.RollingOut) when LoadFromScaleTarget last read it.
	// While it does, the pods reporting may be a mix of the old engine and
	// the new, and a reading may be an old pod's under arguments the record
	// no longer names. It is read afresh every cycle and remembers nothing:
	// a controller restart, a leader failover, a forget or an undo in
	// mid-rollout reads it exactly as any other cycle does. An image-only
	// rollout is one too -- which pods are old cannot be told from here
	// either, and its readings are only held back until it ends.
	RollingOut bool
	// LearnedFrom is LearnedFromLive for a record written from live metrics,
	// "deployment" for one derived from a scale target's args (LWS included).
	// Update does not set it; the writer does.
	LearnedFrom string
	// LearnedAt is when the record was written; Update and LoadFromScaleTarget
	// set it, whatever the caller passed.
	LearnedAt time.Time
}

// Store is a thread-safe in-memory cache of capacity
// records keyed by "namespace|modelID|variantName". It enables the analyzer
// to estimate capacity for zero-replica variants and newly created
// deployments before any live metrics are available.
//
// For cross-variant estimation, use FindCompatible which searches for records
// from other variants with matching hardware and engine parameters.
type Store struct {
	mu      sync.RWMutex
	records map[string]*Record
	// rec records the engine's writes (FileScaleTarget, EvictStale); nil
	// when recording is off (WithRecorder).
	rec *learnrec.Recorder
}

// NewStore creates an empty capacity store.
func NewStore() *Store {
	return &Store{
		records: make(map[string]*Record),
	}
}

// storeKey builds the map key for a given namespace, model, and variant.
// The pipe delimiter is safe because Kubernetes resource names follow DNS
// naming rules and cannot contain the "|" character.
func storeKey(namespace, modelID, variantName string) string {
	return fmt.Sprintf("%s|%s|%s", namespace, modelID, variantName)
}

// Update stores or overwrites a capacity record for a specific variant and
// stamps LearnedAt with now. Live data is always authoritative and should
// always be written via Update, with LearnedFrom set to LearnedFromLive by the
// caller: that is what stops LoadFromScaleTarget overwriting it while the
// variant runs the arguments and hardware it was measured on
// (LoadFromScaleTarget replaces it when either changes).
func (s *Store) Update(namespace, modelID, variantName string, record Record, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.LearnedAt = now
	s.records[storeKey(namespace, modelID, variantName)] = &record
}

// InRollout reports a rollout in progress: until it completes, a reading may
// be an old pod's (RollingOut).
func (r *Record) InRollout() bool {
	return r != nil && r.RollingOut
}

// Get returns the stored capacity record for a specific variant, or nil
// if none exists. For cross-variant lookup, use FindCompatible instead.
// The record is the store's own; a caller reads it and writes through
// Update, never into it.
func (s *Store) Get(namespace, modelID, variantName string) *Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.records[storeKey(namespace, modelID, variantName)]
}

// IsStale returns true if the record for the given variant is older than
// StalenessTimeout as of now, or if no record exists. No caller on the reconcile path
// today, and that is deliberate: the read paths have no trust horizon.
//
// StalenessTimeout is 30 minutes and EvictionTimeout is seven days, so the
// store keeps a record 336x longer than this function would call fresh.
//
// The reason to keep serving an old record is NOT that the figure is clamped.
// A record's commonest use on the read path is rec.EngineParams, which feeds
// estimateCapacityFromParams -- the DERIVED k2, the figure the rest of this
// change calls the one that raises capacity. So a stale record there pushes
// capacity UP, the opposite of the k2-history argument, which does not
// transfer to this package. Two further paths set capacity from a record with
// no bound from any current k1 at all:
//
//   - the saturation analyzer's computeReplicaCapacityFallback, taken when
//     vllm:cache_config_info is absent. There is no live k1 on that path at
//     all; the record IS the capacity, scaled by KvCacheThreshold, and is
//     written back out as a synthetic TotalKvCapacityTokens.
//   - estimateStoredCapacity's live branch, on the zero-replica path, which
//     returns rec.EffectiveCapacity directly. That figure was min(k1, k2) when
//     it was written, on the hardware it was written on -- not against
//     anything current. LoadFromScaleTarget replaces a live record when the
//     engine arguments or the hardware change, keeping the old figure only as
//     a ceiling (PriorLiveCapacity); a redeploy that changes neither -- the
//     same flags on a card with less free memory -- is not seen, and does not
//     correct it.
//
// The real reason is what the alternative is. A nil record on the zero-replica
// path falls to lookupCompatibleCapacity and then to perReplicaCapacity = 0,
// which the optimizer skips: the variant stops being scaled at all. An old
// figure is a worse estimate than a fresh one and a far better one than none,
// and scale-from-zero is exactly where no record exists to be fresh.
//
// Kept rather than deleted because an explicit staleness predicate is the
// right thing to have the day a caller wants one -- a diagnostic, or a
// condition on the CR -- and because what it means is now written down. An
// earlier version of this comment reached the same conclusion from two
// premises that were both false.
func (s *Store) IsStale(namespace, modelID, variantName string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[storeKey(namespace, modelID, variantName)]
	if !ok {
		return true
	}
	return now.Sub(rec.LearnedAt) > StalenessTimeout
}

// LoadFromScaleTarget parses the engine args from a scale target (dispatching by
// detected engine via ParseEngineArgs) and stores an estimated capacity record for
// the variant, stamped now. Scale-target-derived data is a fallback only: an existing
// "live" record is kept while the variant runs the arguments and hardware it
// was measured on (an image-only change re-tags its parameters), and replaced
// when either changes.
//
// It parses and files, nothing more: the filing is FileScaleTarget, which is
// what a recording holds and what a replay calls (learnrec).
func (s *Store) LoadFromScaleTarget(namespace, modelID, variantName, accelerator string, gpuCount int, scaleTarget scaletarget.ScaleTargetAccessor, now time.Time) {
	if scaleTarget == nil {
		return
	}
	params := ParseEngineArgs(inferenceengine.Detect(scaleTarget), scaleTarget)
	// Every rollout the scale target reports, read afresh each cycle (see
	// Record.RollingOut).
	rolling := scaletarget.RollingOut(scaleTarget)
	s.FileScaleTarget(namespace, modelID, variantName, accelerator, gpuCount, params, rolling, now)
}

// FileScaleTarget files what the engine read from a variant's scale target --
// its parsed engine arguments, the accelerator and GPU count it runs on, and
// whether it is rolling out -- by LoadFromScaleTarget's rules. Separate from
// the parse so a replay files the values a run recorded on a store of its own,
// and the live record it keeps, and the prior capacity it carries, come from
// that store's own analyzer writes rather than the run's.
func (s *Store) FileScaleTarget(namespace, modelID, variantName, accelerator string, gpuCount int, params EngineParams, rolling bool, now time.Time) {
	if s.rec.Active() {
		s.rec.Record(KindFileScaleTarget, now, FileScaleTargetEvent{
			Namespace: namespace, ModelID: modelID, Variant: variantName,
			Accelerator: accelerator, GpuCount: gpuCount, Params: params, RollingOut: rolling,
		})
	}
	s.fileScaleTarget(namespace, modelID, variantName, accelerator, gpuCount, params, rolling, now)
}

func (s *Store) fileScaleTarget(namespace, modelID, variantName, accelerator string, gpuCount int, params EngineParams, rolling bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := storeKey(namespace, modelID, variantName)

	// Live data is not overwritten -- while it describes the engine that is
	// running. The live write carries the existing EngineParams forward, so
	// without this check the parameters were frozen on the first live record:
	// a rollout that changed the engine's arguments left the record, its
	// fingerprint and every compatibility match describing the old
	// configuration for as long as the variant kept serving. The live
	// capacity figures go too, not just the parameters: they were measured
	// under the old arguments, and a parked variant is priced from them.
	// The next live cycle measures the new configuration.
	//
	// The image is not one of the arguments here. It is part of the
	// fingerprint, so a new release does not share a latency line, but a
	// routine image bump with the same flags is not a new capacity, as
	// IsCapacityCompatible already holds -- and replacing the record on it
	// priced a variant parked at zero from the derived figure on every
	// rollout: 320,000 tokens against a measured 12,800 at the defaults, so
	// its first scale-from-zero order was sized ~25x too small. The
	// PARAMETERS do take the new image: left on the old one, the
	// fingerprint, wva_engine_config and the snapshot's configuration tag
	// kept naming the old release, and the next restart, reading the new
	// image, restored nothing.
	//
	// The hardware is compared too: a variant moved to another GPU product,
	// or to another GPU count, was measured on hardware it no longer runs on.
	// An unresolved accelerator on either side is not a move. A reshape of
	// the data or pipeline parallelism is an argument change and replaces the
	// record like any other; the GPU count itself still comes from the pod's
	// GPU request, so one made through a DRA claim reads as unknown.
	//
	// This function writes the record afresh every cycle, so what it carried
	// from the live record it replaced is carried again from its own record
	// on the next cycle; otherwise the bound held for one cycle only.
	var carriedKv, priorLive int64
	var priorLiveAt time.Time
	existing, ok := s.records[key]
	sameHW := ok && sameHardware(existing, accelerator, gpuCount)
	if ok && existing.LearnedFrom == LearnedFromLive {
		if existing.EngineParams == nil {
			return
		}
		if sameHW && sameArguments(*existing.EngineParams, params) {
			// The declared batch cap (the llm-d.ai/max-num-seqs annotation)
			// re-tags too: it is not an argument the record was measured
			// under, and an operator setting it means it to price the next
			// derived k2.
			if existing.EngineParams.Image != params.Image || existing.RollingOut != rolling ||
				existing.EngineParams.DeclaredBatchCap != params.DeclaredBatchCap {
				// A copy: Get hands out the store's own record.
				swapped := *existing
				swapped.EngineParams = &params
				swapped.RollingOut = rolling
				s.records[key] = &swapped
			}
			return
		}
		// What the variant was last measured at stays a ceiling on what it
		// is priced at until it is measured again. The derived estimate it
		// falls back to otherwise has no bound at all when the cache's
		// geometry changed and no sibling runs the new flags yet. Not across
		// a hardware move: a figure measured on other hardware bounds nothing.
		if sameHW {
			priorLive, priorLiveAt = existing.EffectiveCapacity, existing.LearnedAt
		}
	} else if ok && sameHW && now.Sub(existing.PriorLiveAt) <= EvictionTimeout {
		priorLive, priorLiveAt = existing.PriorLiveCapacity, existing.PriorLiveAt
	}
	if priorLive == 0 {
		priorLiveAt = time.Time{}
	}
	// What the KV cache holds is a property of the memory, not of the
	// scheduler flags. While nothing that shapes it changed, the measured
	// total stays this variant's k1, so the derived estimate a parked variant
	// falls back to is bounded by it (estimateStoredCapacity's own-k1 bound).
	if sameHW && existing.EngineParams != nil && kvTotalCarries(*existing.EngineParams, params) {
		carriedKv = existing.TotalKvCapacityTokens
	}
	record := &Record{
		AcceleratorName:   accelerator,
		GpuCount:          gpuCount,
		EngineParams:      &params,
		PriorLiveCapacity: priorLive,
		PriorLiveAt:       priorLiveAt,
		RollingOut:        rolling,
		LearnedFrom:       "deployment", // same for deployment and LWS
		LearnedAt:         now,
	}

	// k1: the measured KV total carried across a flag change, or, if
	// num_gpu_blocks_override is set (vLLM), an estimate from it.
	switch {
	case carriedKv > 0:
		record.TotalKvCapacityTokens = carriedKv
	case params.NumGpuBlocksOverride > 0:
		record.NumGpuBlocks = params.NumGpuBlocksOverride
		record.BlockSize = params.BlockSize
		record.TotalKvCapacityTokens = params.NumGpuBlocksOverride * params.BlockSize
	case params.TotalKvTokensOverride > 0:
		// SGLang exposes total KV token capacity directly via --max-total-tokens.
		record.TotalKvCapacityTokens = params.TotalKvTokensOverride
	}

	// Provide a conservative capacity estimate so that brand-new variants
	// with no live data or compatible siblings can still be considered for
	// scale-up. EffectiveMaxBatchedTokens (the per-step token budget) is a
	// safe lower bound — real capacity is almost always much higher.
	if record.EffectiveCapacity <= 0 && params.EffectiveMaxBatchedTokens > 0 {
		record.EffectiveCapacity = params.EffectiveMaxBatchedTokens
	}

	s.records[key] = record
}

// Delete removes one variant's record, and reports whether there was one. It
// is for an operator's request to forget what a variant has learned: the next
// LoadFromScaleTarget files a fresh record from the scale target.
func (s *Store) Delete(namespace, modelID, variantName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey(namespace, modelID, variantName)
	_, ok := s.records[key]
	delete(s.records, key)
	return ok
}

// EvictStale removes capacity records that have not been updated within the
// given timeout as of now. This prevents unbounded memory growth from deleted or
// long-unused variants. Use a long timeout (EvictionTimeout, seven days)
// since historical capacity data is valuable for zero-replica estimation.
// Called once per cycle from steadystate.evictStaleLearnedState.
//
// Recorded with what it removed, as a check: a replay recomputes the removal
// from its own records.
func (s *Store) EvictStale(now time.Time, timeout time.Duration) int {
	n := s.evictStale(now, timeout)
	if s.rec.Active() {
		s.rec.Record(KindEvictStale, now, EvictStaleEvent{Timeout: timeout, Removed: n})
	}
	return n
}

func (s *Store) evictStale(now time.Time, timeout time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	evicted := 0
	for key, rec := range s.records {
		if now.Sub(rec.LearnedAt) > timeout {
			delete(s.records, key)
			evicted++
		}
	}
	return evicted
}

// FindCompatible searches across all namespaces for a capacity record from
// another variant with matching configuration: same model, accelerator type,
// GPU count, and compatible engine parameters (as defined by IsCapacityCompatible).
// Capacity is a property of hardware + engine config, not namespace, so
// cross-namespace matching is intentional.
//
// Returns the best match (preferring live records over deployment-derived
// ones), or nil if no compatible record exists. As with Get, the record is
// the store's own.
func (s *Store) FindCompatible(modelID, accelerator string, gpuCount int, params *EngineParams) *Record {
	return s.findCompatible(modelID, accelerator, gpuCount, params, (*EngineParams).IsCapacityCompatible)
}

// FindClampBound returns the live record whose measured capacity bounds a
// zero-replica estimate from above, under the looser equality a max clamp
// takes (EngineParams.IsCapacityClampCompatible): a sibling that differs in
// the layout or feature flags the borrow's equality compares is still a
// ceiling.
//
// Among several, the LOWEST live EffectiveCapacity is taken, from namespace's
// own variants when any qualify and from every namespace otherwise; ties go
// to the smaller key. It took the first match in map order, so with several
// candidates the ceiling changed from cycle to cycle. The lowest is the
// tightest ceiling, and a ceiling only lowers an estimate.
func (s *Store) FindClampBound(namespace, modelID, accelerator string, gpuCount int, params *EngineParams) *Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best, bestOwnNS *Record
	var bestKey, bestOwnNSKey string
	lower := func(rec *Record, key string, cur *Record, curKey string) bool {
		return cur == nil || rec.EffectiveCapacity < cur.EffectiveCapacity ||
			(rec.EffectiveCapacity == cur.EffectiveCapacity && key < curKey)
	}
	for key, rec := range s.records {
		parts := strings.SplitN(key, "|", 3)
		if len(parts) < 3 || parts[1] != modelID {
			continue
		}
		if rec.AcceleratorName != accelerator || rec.GpuCount != gpuCount {
			continue
		}
		if rec.LearnedFrom != LearnedFromLive || rec.EffectiveCapacity <= 0 {
			continue
		}
		if rec.EngineParams == nil || !rec.EngineParams.IsCapacityClampCompatible(params) {
			continue
		}
		if lower(rec, key, best, bestKey) {
			best, bestKey = rec, key
		}
		if parts[0] == namespace && lower(rec, key, bestOwnNS, bestOwnNSKey) {
			bestOwnNS, bestOwnNSKey = rec, key
		}
	}
	if bestOwnNS != nil {
		return bestOwnNS
	}
	return best
}

func (s *Store) findCompatible(modelID, accelerator string, gpuCount int, params *EngineParams,
	compatible func(*EngineParams, *EngineParams) bool) *Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *Record
	for key, rec := range s.records {
		// Parse key: "namespace|modelID|variantName"
		parts := strings.SplitN(key, "|", 3)
		if len(parts) < 3 || parts[1] != modelID {
			continue
		}

		// Must match accelerator type and GPU count
		if rec.AcceleratorName != accelerator || rec.GpuCount != gpuCount {
			continue
		}

		// Must have compatible engine parameters.
		//
		// COMPLETENESS IS DELIBERATELY NOT CHECKED. Two revisions got this
		// wrong in opposite ways, so both are recorded.
		//
		// The first ADDED a Complete() gate here. That is a regression,
		// because of how one of the two callers uses the result:
		// estimateStoredCapacity (saturation/replica_capacity.go) takes it as
		// a MAX CLAMP on a derived estimate, `if compatible.EffectiveCapacity
		// < bounded`. Refusing the record does not substitute something
		// smaller; it removes the ceiling. Measured: 5,000 became 153,600, a
		// 30x over-estimate of per-replica capacity -- and an over-stated
		// capacity UNDER-orders replicas, which this project has on record as
		// breaking TTFT irrecoverably.
		//
		// The second was the comment that replaced it, which justified the
		// removal by calling that the ONLY caller. It is not. The other is
		// lookupCompatibleCapacity (same file), consumed in aggregate.go,
		// where the record IS the estimate --
		// `perReplicaCapacity = float64(rec.EffectiveCapacity)` -- for a
		// variant with no record of its own. There, refusing would not remove
		// a ceiling; it would fall through to satReasonNoData. So "refusing
		// only ever removes a ceiling" was false, and the conclusion needs a
		// different reason.
		//
		// THE REASON THAT HOLDS: an unreadable flag withholds the NEW thing it
		// would authorise and never changes a path that already existed.
		// Cross-variant capacity borrowing predates this work;
		// IsCapacityCompatible was always a heuristic over parsed fields, and
		// an unresolved value defaulted before the set existed exactly as it
		// does now. Recording the gap made the uncertainty visible, it did not
		// create it, and making it newly fatal would be this work changing a
		// decision it did not improve. The one gate is engineFingerprint,
		// where refusing means a variant learns its own ITL line -- the
		// behaviour that preceded line sharing.
		//
		// What the borrow at the second call site DOES get is a log line
		// naming the donor's unresolved set, so the uncertainty is visible
		// where it is acted on: the `donorUnresolved` field on
		// variant-capacity-source in aggregate.go.
		//
		// That field is named here because this comment asserted it before it
		// existed. A review checked and found the log line carried only
		// LearnedFrom, which made the one place the uncertainty is acted on
		// the one place it was invisible -- so the mitigation this paragraph
		// offers for not gating on completeness was not actually there.
		if rec.EngineParams == nil || !compatible(rec.EngineParams, params) {
			continue
		}

		// Must have useful capacity data
		if rec.EffectiveCapacity <= 0 && rec.TotalKvCapacityTokens <= 0 {
			continue
		}

		// Prefer live data over deployment/lws-derived
		if best == nil || (best.LearnedFrom != "live" && rec.LearnedFrom == LearnedFromLive) {
			best = rec
		}
	}

	return best
}

// sameArguments reports whether two parses describe the same engine arguments,
// leaving out the image: see LoadFromScaleTarget. It is ArgsFingerprint
// equality, the one definition the restore's arguments gate uses too -- a
// change the live path keeps a record across is a change a restart restores
// across. It was a DeepEqual over the struct, which also compared the inputs
// EffectiveMaxBatchedTokens is resolved from, so writing the default budget
// out explicitly replaced a live record that a restart would have restored.
func sameArguments(a, b EngineParams) bool {
	return a.ArgsFingerprint() == b.ArgsFingerprint()
}

// sameHardware reports whether a record was learned on the hardware a
// variant now runs on. An unresolved accelerator, or an unknown GPU count, on
// either side is not taken as a difference: the reading flips with pod
// placement, and a move is only a move when both ends are known.
func sameHardware(rec *Record, accelerator string, gpuCount int) bool {
	if constants.IsAcceleratorResolved(rec.AcceleratorName) && constants.IsAcceleratorResolved(accelerator) &&
		rec.AcceleratorName != accelerator {
		return false
	}
	return rec.GpuCount <= 0 || gpuCount <= 0 || rec.GpuCount == gpuCount
}

// kvTotalCarries reports whether a KV total measured under prev still bounds
// an engine started with cur, so it may stand as the new engine's k1.
//
// The cache must be sized alike: the engine, the checkpoint and its dtypes and
// quantization, the memory share, the block size, the parallel layout
// (tensor, data and pipeline, and expert parallelism, which moves the
// weights), speculative decoding (a draft model's weights and cache), CUDA
// graphs, and the overrides that set it outright. The image is not
// compared: a release can move the cache a little either way, and the
// carried total is then off by as much -- the same allowance
// IsCapacityCompatible makes.
//
// The per-step budgets are compared one way. vLLM sizes the cache from what
// its profiling run leaves free, and that run's peak grows with the batch-token
// budget and, through the CUDA graphs it captures, with --max-num-seqs. A
// smaller budget leaves the cache no smaller, so the old total under-states
// it and orders more replicas, not fewer; a larger one can shrink it, and the
// old total would over-state it, so it is not carried. The context length
// decides how the cache is used, not how big it is.
func kvTotalCarries(prev, cur EngineParams) bool {
	return prev.Engine == cur.Engine &&
		prev.WeightDtype == cur.WeightDtype && prev.Quantization == cur.Quantization &&
		prev.GpuMemoryUtilization == cur.GpuMemoryUtilization && prev.BlockSize == cur.BlockSize &&
		prev.KvCacheDtype == cur.KvCacheDtype && prev.TensorParallelSize == cur.TensorParallelSize &&
		prev.NumGpuBlocksOverride == cur.NumGpuBlocksOverride && prev.TotalKvTokensOverride == cur.TotalKvTokensOverride &&
		prev.EnforceEager == cur.EnforceEager &&
		parallelSize(prev.DataParallelSize) == parallelSize(cur.DataParallelSize) &&
		parallelSize(prev.PipelineParallelSize) == parallelSize(cur.PipelineParallelSize) &&
		prev.ExpertParallel == cur.ExpertParallel && prev.SpeculativeConfig == cur.SpeculativeConfig &&
		prev.ModelPath == cur.ModelPath && prev.ModelRevision == cur.ModelRevision &&
		slices.Equal(prev.SharingUnresolved, cur.SharingUnresolved) && prev.MemoryLayout == cur.MemoryLayout &&
		cur.EffectiveMaxBatchedTokens <= prev.EffectiveMaxBatchedTokens && cur.MaxNumSeqs <= prev.MaxNumSeqs
}
