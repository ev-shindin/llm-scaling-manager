package saturation

import (
	"sort"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// What one replica can hold: the memory bound k1, the compute bound k2, and
// which of the two applies.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

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
	//
	// Stale() is checked here for the same reason the write path above checks
	// it, and it was missing. Without it this read would return a window the
	// sweep is about to delete, which made wiring EvictStaleHistory up a
	// BEHAVIOUR change rather than the memory fix it was presented as: a k2
	// older than HistoryEvictionTimeout used to be returned as
	// K2SrcHistorical, and after the sweep it falls through to the derived
	// figure instead. A derived k2 above the measured one raises
	// effectiveCapacity and orders FEWER replicas, which is the direction this
	// project has recorded as breaking TTFT irrecoverably. With the guard, a
	// stale entry is ignored whether or not the sweep has reached it yet, so
	// the two agree and the sweep is neutral again.
	a.mu.Lock()
	var histAvg float64
	var histLen int
	if ra, ok := a.computeCapacityHistory[historyKey]; ok &&
		!ra.Stale(capacity.HistoryEvictionTimeout) {
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

// lookupCompatibleCapacity searches the capacity store for a record from
// another variant with matching hardware and engine parameters. This enables
// capacity estimation for zero-replica variants that have no prior data.
// The search is cross-namespace since capacity depends on hardware + config,
// not namespace.
func (a *SaturationAnalyzer) lookupCompatibleCapacity(namespace, modelID, variantName, accelerator string, gpuCount int, reuseDisabled bool) *capacity.Record {
	// Get EngineParams for this variant (from deployment-derived record)
	rec := a.capacityStore.Get(namespace, modelID, variantName)
	if rec == nil || rec.EngineParams == nil {
		return nil
	}
	// Refused outright when reuse is off: this whole function exists to price
	// a variant from a sibling's measurement, which is the thing the switch
	// withholds. The caller's remaining branches fall back to the variant's
	// own figures.
	// Refused outright when reuse is off: this whole function exists to price a
	// variant from a SIBLING's measurement, which is what the switch withholds.
	// The caller's remaining branches fall back to the variant's own figures.
	//
	// This is the only capacity path the switch gates. estimateStoredCapacity's
	// compatible-variant bound is deliberately left alone -- it is a max clamp,
	// and withholding a clamp raises the estimate and orders FEWER replicas.
	if reuseDisabled {
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
			//
			// DisableLearnedStateReuse DELIBERATELY DOES NOT REACH HERE, and a
			// first version of it did, which was a defect.
			//
			// This is a MAX clamp: it only ever LOWERS the estimate. Removing
			// it raises capacity, and an over-stated capacity orders FEWER
			// replicas -- the direction this project has on record as breaking
			// TTFT irrecoverably. store.go measured it when the record was
			// withheld for a different reason: 5,000 became 153,600.
			//
			// So an operator asking "stop pricing this variant from a sibling's
			// measurement" must not silently get "and remove your capacity
			// ceiling" as well. A ceiling is conservatism, not a borrow. The
			// switch gates the two places a sibling's figure BECOMES this
			// variant's answer -- the shared ITL line, and
			// lookupCompatibleCapacity, where the record IS the estimate -- and
			// leaves every clamp alone.
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

// memoryBound is k1: the KV cache times kvCacheThreshold, truncated -- the
// one formula for it, so the gate's "full" is the capacity the replica is
// priced at.
func memoryBound(totalKvCapacityTokens int64, kvCacheThreshold float64) int64 {
	return int64(float64(totalKvCapacityTokens) * kvCacheThreshold)
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
