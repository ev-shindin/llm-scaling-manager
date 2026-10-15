package capacity

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
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
	// EngineParams are the parsed deployment params for k2 derivation.
	EngineParams *EngineParams
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
// stamps LearnedAt. Live data is always authoritative and should always be
// written via Update, with LearnedFrom set to LearnedFromLive by the caller:
// that is what stops LoadFromScaleTarget overwriting it.
func (s *Store) Update(namespace, modelID, variantName string, record Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.LearnedAt = time.Now()
	s.records[storeKey(namespace, modelID, variantName)] = &record
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
// StalenessTimeout, or if no record exists. No caller on the reconcile path
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
//     anything current -- and LoadFromScaleTarget refuses to overwrite a live
//     record, so a redeploy onto a smaller cache does not correct it.
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
func (s *Store) IsStale(namespace, modelID, variantName string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[storeKey(namespace, modelID, variantName)]
	if !ok {
		return true
	}
	return time.Since(rec.LearnedAt) > StalenessTimeout
}

// LoadFromScaleTarget parses the engine args from a scale target (dispatching by
// detected engine via ParseEngineArgs) and stores an estimated capacity record for
// the variant. It does NOT overwrite an existing "live" record — scale
// target-derived data is a fallback only.
func (s *Store) LoadFromScaleTarget(namespace, modelID, variantName, accelerator string, gpuCount int, scaleTarget scaletarget.ScaleTargetAccessor) {
	if scaleTarget == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := storeKey(namespace, modelID, variantName)

	// Don't overwrite live data
	if existing, ok := s.records[key]; ok && existing.LearnedFrom == LearnedFromLive {
		return
	}

	params := ParseEngineArgs(inferenceengine.Detect(scaleTarget), scaleTarget)
	record := &Record{
		AcceleratorName: accelerator,
		GpuCount:        gpuCount,
		EngineParams:    &params,
		LearnedFrom:     "deployment", // same for deployment and LWS
		LearnedAt:       time.Now(),
	}

	// If num_gpu_blocks_override is set (vLLM), we can estimate k1.
	if params.NumGpuBlocksOverride > 0 {
		record.NumGpuBlocks = params.NumGpuBlocksOverride
		record.BlockSize = params.BlockSize
		record.TotalKvCapacityTokens = params.NumGpuBlocksOverride * params.BlockSize
	} else if params.TotalKvTokensOverride > 0 {
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
// given timeout. This prevents unbounded memory growth from deleted or
// long-unused variants. Use a long timeout (EvictionTimeout, seven days)
// since historical capacity data is valuable for zero-replica estimation.
// Called once per cycle from steadystate.evictStaleLearnedState.
func (s *Store) EvictStale(timeout time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	evicted := 0
	for key, rec := range s.records {
		if time.Since(rec.LearnedAt) > timeout {
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
		if rec.EngineParams == nil || !rec.EngineParams.IsCapacityCompatible(params) {
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
