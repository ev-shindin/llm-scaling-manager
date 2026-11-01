package saturation

import (
	"sort"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
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
	// tune is config's tuning, the cycle's (tuningOf), built once per cycle
	// rather than once per replica.
	tune tuning,
	modelID, namespace string,
	gpuCount int,
	role string,
	accelerator string,
	// shapeKeyOutput is the TRACKED output length: hysteretic, so the window's
	// bucket does not flip when the fleet's average wobbles across a boundary.
	shapeKeyOutput float64,
	// k2KeyOut is the output length the k2 key is built from: the fleet's,
	// its bucket held across a boundary (k2KeyLength).
	k2KeyOut float64,
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
	// served is what the variant's Ready replicas served this cycle
	// (servedBounds), in the same requests/s mu is measured in: the highest
	// rate, a lower bound on what one replica sustains, and the median of
	// those at their batch limit.
	served servedRates,
	downstreamSaturated bool,
	// revisionsMixed is the variant's replicas reporting more than one pod
	// template revision this cycle (revisionsMixed): mid-rollout, whatever
	// its scale target's status says.
	revisionsMixed bool,
	// routing is what the cycle's routing says about this replica's demand
	// (replicaRouting): whether its resident KV is charged at its instant
	// reading.
	routing replicaRouting,
	logger logr.Logger,
) *capacity.ReplicaCapacity {
	if rm.TotalKvCapacityTokens <= 0 {
		// TODO: implement proper demand estimation when vllm:cache_config_info is absent.
		// Currently we fall back to percentage-based demand using the deployment-derived
		// capacity from the capacity store. A better approach would be to estimate
		// TotalKvCapacityTokens from deployment args (num_gpu_blocks_override, block_size)
		// or use a dedicated percentage-based demand signal.
		return a.computeReplicaCapacityFallback(rm, config, modelID, namespace, role, accelerator, routing, logger)
	}

	// Compute demand: tokens already resident in KV cache plus the role-aware
	// footprint of requests still waiting in the local engine queue, in full
	// whatever the replica's siblings are doing (idle_room.go says why).
	// The resident half is the one-minute peak, or the instant reading where
	// that peak is load the router has since moved to a sibling
	// (residentCharge).
	localQueueDemand := waitingQueueDemand(rm, role)
	residentDemand := residentCharge(rm, rm.TokensInUse, rm.TotalKvCapacityTokens, routing)
	if _, ok := instantKv(rm); ok && routing.kvFromInstant && rm.TotalKvCapacityTokens > 0 {
		a.paths.add(pathInstantKV, 1)
	}
	replicaDemand := residentDemand + localQueueDemand

	// k1: memory-bound capacity
	k1 := memoryBound(rm.TotalKvCapacityTokens, config.KvCacheThreshold)

	// k2: compute-bound capacity
	var engineParams *capacity.EngineParams
	rec := a.capacityStore.Get(namespace, modelID, rm.VariantName)
	if rec != nil {
		engineParams = rec.EngineParams
	}
	historyKey := a.historyKey(modelID, namespace, rm.VariantName, accelerator, gpuCount, role,
		k2KeyOutput(role, k2KeyOut, rm.AvgOutputTokens), config.QueueLengthThreshold)
	// The rows whose saturated reading may become the variant's k2 are the
	// rows whose saturated throughput may become its mu, below: Ready, and
	// the variant's own. A warm-pool bridge is filed under the variant it is
	// lent to but runs its engine on the pool's terms (a lower
	// --gpu-memory-utilization, a different batch ceiling), so its occupancy
	// at saturation is the pool engine's, not this variant's. A Pod that is
	// not Ready is not routed to; a queue on it is what it held when it was,
	// or a hang, and its resident KV is frozen with it. Either one recorded
	// lowers the window, and a low k2 never corrects itself: it orders
	// replicas, so nothing saturates to write a higher one, and every read
	// keeps the window alive (Priority 2's Touch). Unadmitted rows still read
	// the window; they only do not write it. admissionOf is the one place the
	// rule is spelled: the k2 write, the throughput write below, the borrow
	// clock and the capacity-store update all read adm.
	adm := admissionOf(rm)
	// Mid-rollout, a reading may be an old pod's, and the record -- and so the
	// tag a window would be written under -- already names the new
	// arguments: recorded, it would price the new engine at the old one's
	// figure as its own, with full authority. Measured in review: a decode
	// fleet rolled 256 -> 64 sequences, its old pods saturating while the
	// groups were replaced, priced the 64-sequence pods at the 256-sequence k2
	// once the rollout was done. So nothing is written to a window while the
	// scale target reports ANY rollout. The fleet is priced from what the
	// variant already holds: its own window under the arguments it runs,
	// or -- after the arguments changed -- its previous figure on a bound
	// frozen for the rollout, which keeps its whole hold for the first
	// serving cycle after; a sibling's figure only below either, as ever.
	// A rollout's readings price nothing: a minimum of single readings gave
	// one outlier all the weight, and a median of a few needed readings the
	// per-cycle sweep did not keep. The gate is the rollout the scale target reports, read afresh
	// every cycle, with no memory of where a rollout began: an undo, a
	// restart or a failover in mid-rollout are rollouts like any other. The
	// cost is that an image-only rollout, whose old pods run the same
	// arguments, records nothing until it ends either.
	//
	// Or while the variant's own replicas report more than one pod template
	// revision: that is a rollout whatever the status says. A LeaderWorkerSet
	// status names no generation, so a template change its controller has
	// acted on for some groups but not yet reported -- or never will, while
	// the controller is down -- reads as settled there. Which revision is the
	// new one the pods cannot say, so the whole variant is held, as for a
	// rollout the status reports. A scale-up is not this: a new group is
	// created from the template every other group runs.
	adm.rollout = rec.InRollout() || revisionsMixed
	// An admitted row not mid-rollout is the variant serving this bucket,
	// which is the only time a lower bound's clock runs (k2_borrow.go;
	// admission.serving). With the borrow turned off for the model, a bound
	// running on a sibling's figure is not in use, so its hold and relaxation
	// wait, and turning the borrow back on resumes it where it stood (the gap
	// then reads as a pause). One running on the variant's own previous
	// figure is not a borrow, and runs.
	if adm.serving() {
		a.noteBorrowServing(historyKey, a.now(), !config.DisableK2Borrow)
	}
	// The saturated reading is judged, and recorded, on the BUSIEST engine
	// rank, not on the pod's sum and mean (domain.ReplicaMetrics.
	// MaxRankQueueLength). queueLengthThreshold is a queue on one engine: a
	// pod of eight ranks with one request waiting on each sums to eight and
	// is not saturated, and recording its 30 % occupancy as k2 priced it at
	// half its capacity for as long as the window lived. And a k2 is one
	// engine's occupancy at saturation scaled by the ranks
	// (BusiestRankTokens): the tokens one full rank holds plus seven idle
	// ones' nothing is not what the pod holds when it is full. With one rank
	// both are the pod's own queue and tokens, as before.
	//
	// Prefill's throughput is keyed on the prompt tokens it actually has to
	// compute: a prefix the cache already holds is not work, so the discount
	// that shape.New applies for KVreq applies to prefill's throughput too.
	// Raw prompt length would put the same replica in different buckets
	// purely because the hit rate moved. Decode keeps the raw figure it has
	// always used -- its capacity is about resident KV, which the shape's
	// KVreq already discounts on its own path. Why the throughput key is the
	// fleet's shape at all is said where it is written, below.
	keyInput := throughputKeyInput(role, shapeKeyInput, shapeKeyOutput, fleetHitRate)
	throughputKey := a.throughputKey(modelID, namespace, rm.VariantName, accelerator, gpuCount, role,
		keyInput, shapeKeyOutput, config.QueueLengthThreshold)
	a.noteMuKey(namespace, modelID, rm.VariantName, role, throughputKey)
	// A window written under engine arguments the variant no longer runs is
	// not its service rate (window_args.go).
	args := windowArgs(engineParams)
	a.mu.Lock()
	a.throughputWindowUnderLocked(throughputKey, args)
	ref := a.serviceRateOnRecordLocked(throughputKey)
	k2Ref := a.k2OnRecordLocked(historyKey)
	a.mu.Unlock()
	if ref <= 0 && derived.ok {
		ref = derived.rate
	}
	// Whether a saturated reading is the replica at its limit or a load step
	// it has just been handed (k2_settle.go). Judged on the busiest rank, as
	// the reading is.
	serviceRate, serviceRateOK := saturatedCompletionRate(rm, role, fleetOutput, keyInput)
	adm.unsettled = a.k2Unsettled(k2SettleInput{
		namespace: namespace, modelID: modelID, variant: rm.VariantName, pod: rm.PodName,
		queue: rm.BusiestRankQueue(), tokens: rm.BusiestRankTokens(), ranks: rm.Ranks(),
		perRequest: k2SettlePerRequest(rm, role, shapeKeyInput, fleetOutput), threshold: config.QueueLengthThreshold,
		rate: serviceRate, rateOK: serviceRateOK, ref: ref, k2Ref: k2Ref, ready: rm.Ready, readyAt: a.readySince(rm), bridge: rm.FromWarmPool,
		tune: tune,
	})
	d := a.computeK2(
		historyKey,
		modelID, namespace, rm.VariantName,
		rm.BusiestRankQueue(), rm.BusiestRankTokens(),
		rm.AvgOutputTokens, rm.AvgInputTokens,
		config.QueueLengthThreshold,
		engineParams,
		rm.Ranks(),
		k1,
		rm.TotalKvCapacityTokens,
		role,
		downstreamSaturated,
		adm,
		config.DisableK2Borrow,
		logger,
	)
	k2, k2Priority, k2Recordable, k2Borrowed := d.k2, d.src, d.recordable, d.borrowed
	a.noteK2(namespace, modelID, rm.VariantName, role, historyKey, rm.PodName, d, rm.BusiestRankTokens())
	k2Age, k2AgeKnown := a.k2WriteAge(historyKey, k2Priority, k2Borrowed)
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
	// Recorded and read under the FLEET's tracked output-length bucket, not
	// this replica's. The k2 key above is the fleet's too (k2KeyOutput), held
	// by its own band, so the two can be in different buckets for minutes
	// after the length crosses a boundary. Its throughput is
	// priced per role, as the median
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
	// key alone, as the shape-shift proposal scopes it. (throughputKey and
	// keyInput are built above, before computeK2, whose settle test reads the
	// rate on record under the key.)
	if k2Priority == capacity.K2SrcObserved && adm.admitted() {
		// keyInput is the effective prompt length this key was built with, so
		// the token rate is converted to requests/s at the same figure the
		// window is keyed by -- a rate divided by one shape and stored under
		// another is the fault the key exists to prevent.
		if mu, ok := saturatedCompletionRate(rm, role, fleetOutput, keyInput); ok {
			// Not every admitted k2 reading is a service rate: a replica that
			// has completed nothing, generates more tokens than its
			// completions account for, or completes a fraction of what its
			// ITL line prices, is filling (throughput_admission.go).
			// Nor is one under half of what a Ready replica of the variant
			// served this cycle (served): a served rate is a lower bound on
			// what one replica sustains.
			why := tune.throughputRefusal(rm.RequestRate, mu, derived, served.max,
				tune.tokensOutrunCompletions(rm, role, fleetOutput))
			// A low reading that repeats on every saturated cycle for
			// MeasuredLowConfirmFor is a real low rate, not a fill -- unless
			// a sibling's served rate contradicts it (refusedUnderServed),
			// which no repetition confirms.
			derivedRate := 0.0
			if derived.ok {
				derivedRate = derived.rate
			}
			if a.noteSaturatedReading(throughputKey, rm.PodName, mu, why == refusedUnderDerived, derivedRate, tune, a.now()) {
				logger.V(logging.DEFAULT).Info("saturated-throughput-low-confirmed",
					"modelID", modelID, "namespace", namespace, "variant", rm.VariantName,
					"pod", rm.PodName, "role", role, "rate", mu, "derivedRate", derived.rate,
					"confirmedFor", MeasuredLowConfirmFor.String(),
					"reason", "under half the derived mu on every saturated cycle for MeasuredLowConfirmFor: the line is the wrong figure, and the reading is recorded")
				why = ""
				a.noteMuCount(namespace, modelID, rm.VariantName, muReasonLowConfirmed)
			}
			if why != "" {
				a.noteMuCount(namespace, modelID, rm.VariantName, muRefusalReason(why))
				logger.V(logging.DEFAULT).Info("saturated-throughput-refused",
					"modelID", modelID, "namespace", namespace, "variant", rm.VariantName,
					"pod", rm.PodName, "role", role, "rate", mu, "requestRate", rm.RequestRate,
					"derivedOK", derived.ok, "derivedRate", derived.rate, "servedBound", served.max, "reason", why)
			} else {
				// The reading is admitted beside k2's P1-obs write, so it
				// carries k2's stamp (noteMuWrite).
				w := a.recordSaturatedThroughputFrom(throughputKey, rm.PodName, mu)
				a.noteMuWrite(namespace, modelID, rm.VariantName, rm.PodName, mu, w)
				if args != "" {
					a.mu.Lock()
					if _, ok := a.saturatedThroughput[throughputKey]; ok {
						a.throughputWindowArgs[throughputKey] = args
					}
					a.mu.Unlock()
				}
			}
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
	reading := a.saturatedThroughputReadingUnder(throughputKey, args, adm.serving())
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
	// or thin reading loses to derived, as before. So does one more than 2x
	// above derived whose bucket has not read saturated in agreement with it
	// for a while: under flow control the queue that writes a sample rarely
	// builds, and a stale high reading must not under-order the fleet. A
	// stale pair that disagrees prices the lower figure, latched so the
	// choice does not flip with derived's noise (measuredYieldsToDerived).
	throughputSamples := reading.samples
	yield := muYieldNone
	if !useDerived(derived.ok, reading) {
		yield = a.measuredYield(throughputKey, reading, derived, served.max, tune, a.now())
		switch yield {
		case muYieldServed:
			a.noteMuCount(namespace, modelID, rm.VariantName, muReasonYieldServed)
		case muYieldStale:
			a.noteMuCount(namespace, modelID, rm.VariantName, muReasonYieldStale)
		}
	}
	if useDerived(derived.ok, reading) || yield != muYieldNone {
		saturatedThroughput = derived.rate
		throughputBucket = derivedBucket
		// Not the stale measured count, which describes a different figure
		// entirely and only ever reached a log line as a confusing number.
		throughputSamples = MinDerivedThroughputSamples
	} else if rate, ok := a.siblingYield(throughputKey, reading, served, tune); ok {
		a.paths.add(pathYieldSibling, 1)
		a.noteMuCount(namespace, modelID, rm.VariantName, muReasonYieldSiblings)
		// A measurement under what the variant's Ready, full replicas serve
		// is one slow replica's, not the variant's (siblingYield).
		logger.V(logging.DEFAULT).Info("saturated-throughput-yields-to-siblings",
			"modelID", modelID, "namespace", namespace, "variant", rm.VariantName,
			"pod", rm.PodName, "role", role, "measuredRate", reading.rate, "siblingRate", rate,
			"siblingRateNow", served.saturated, "tolerance", tune.Admission.SiblingMuTolerance)
		saturatedThroughput = rate
		throughputBucket = servedBucket
	}
	// Prefill with no window of its own under the arguments it runs is
	// priced on its lower bound when it has one (prefill_bound.go). Only
	// then: any own reading, however thin, and a previous figure from other
	// arguments are both measurements, and a measurement outranks a bound.
	prefillBound := a.notePrefillBound(rm, config, role, throughputKey, args, keyInput, adm, logger)
	lowerBound := false
	if reading.rate <= 0 && throughputBucket != derivedBucket && prefillBound.rate > 0 {
		saturatedThroughput = prefillBound.rate
		throughputBucket = lowerBoundBucket
		throughputSamples = prefillBound.samples
		lowerBound = true
	}

	effectiveCapacity := k1
	bound := "k1-memory"
	if k2 < k1 {
		effectiveCapacity = k2
		bound = "k2-compute"
	}
	// Per-replica, per-cycle diagnostics: two lines per replica per optimize
	// cycle, the highest-volume logging in the controller, so they sit at
	// perReplicaLogLevel, one above the shipped verbosity (replica_log.go).
	// replica-capacity-summary says what each replica was priced at, per
	// variant, at the default; hack/benchmark/dump_k2_decisions.py needs the
	// controller at --v=3. replica-capacity-skipped is deliberately not among them: it reports a
	// degradation rather than a decision.
	logger.V(perReplicaLogLevel).Info("replica-capacity-decision",
		"modelID", modelID, "namespace", namespace, "variant", rm.VariantName, "pod", rm.PodName,
		"k1MemoryBound", k1, "k2ComputeBound", k2, "k2Source", k2Priority.String(),
		"effectiveCapacity", effectiveCapacity, "boundBy", bound,
		"tokensInUse", rm.TokensInUse, "localQueueDemand", localQueueDemand, "replicaDemand", replicaDemand,
		"queueLength", rm.QueueLength, "queueThreshold", config.QueueLengthThreshold,
		"residentDemand", residentDemand, "kvFromInstant", routing.kvFromInstant, "kvUsageInstant", rm.KvUsageInstant,
		"engineRanks", rm.Ranks(), "busiestRankQueue", rm.BusiestRankQueue(),
		"busiestRankKvUsage", rm.BusiestRankKvUsage(),
		"requestRate", rm.RequestRate,
		"saturatedThroughput", saturatedThroughput, "saturatedThroughputBucket", throughputBucket)

	// Update capacity store with live data, preserving EngineParams from any
	// existing record (parsed from deployment args and needed for FindCompatible).
	//
	// Not from a warm-pool bridge. Its KV cache and capacity are the pool
	// engine's, and a record filed live under the variant it is lent to
	// would price that variant -- once parked, and as a compatible sibling's
	// clamp -- on an engine it does not run.
	//
	// A not-Ready Pod files its cache, which is the variant's own engine's
	// -- so the gate here is adm.bridge, not adm.admitted() -- but at the
	// capacity the chain gave, never at a reading it may not record.
	//
	// During a rollout the record already carries the NEW arguments
	// (LoadFromScaleTarget runs first in the cycle) while old pods still
	// report, so a figure measured on an old pod is filed under the new
	// arguments and fingerprint. The new pods' readings replace it as they
	// are priced; only a variant parked before any new pod reported keeps it.
	if !adm.bridge {
		var existingParams *capacity.EngineParams
		var rolling bool
		if existing := a.capacityStore.Get(namespace, modelID, rm.VariantName); existing != nil {
			existingParams, rolling = existing.EngineParams, existing.RollingOut
		}
		// Filed under the STABLE accelerator, as the k2 and throughput keys are
		// and as every reader of the store looks it up: the raw reading flips to
		// unresolved with pod placement on a fleet spread over two products, and
		// a record filed under whichever value the last cycle saw was missed by
		// the next lookup that saw the other.
		//
		// The cost, the same compromise the k2 keys make: on a variant whose pods
		// sit on two products, the record holds whichever replica was priced last
		// under the product last RESOLVED, so a compatible sibling parked on one
		// product can be bounded or estimated from the other's capacity. It needs
		// a mixed fleet and a compatible parked sibling on the same model; filed
		// under the raw reading instead, the record matched no sibling at all.
		a.capacityStore.Update(namespace, modelID, rm.VariantName, capacity.Record{
			AcceleratorName:       a.stableAcceleratorFor(namespace, rm.VariantName, accelerator),
			GpuCount:              gpuCount,
			NumGpuBlocks:          rm.NumGpuBlocks,
			BlockSize:             rm.BlockSize,
			TotalKvCapacityTokens: rm.TotalKvCapacityTokens,
			EffectiveCapacity:     min(k1, k2Recordable),
			EngineParams:          existingParams,
			RollingOut:            rolling,
			LearnedFrom:           capacity.LearnedFromLive,
		}, a.now())
	}

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
		K2BorrowedFrom:              k2Borrowed.donor,
		K2BorrowRelaxed:             k2Borrowed.relaxed,
		K2WriteAge:                  k2Age,
		K2WriteAgeKnown:             k2AgeKnown,
		K2NotRecorded:               d.refused,
		EffectiveCapacity:           effectiveCapacity,
		ReplicaDemand:               replicaDemand,
		FromWarmPool:                rm.FromWarmPool,
		SaturatedThroughput:         saturatedThroughput,
		SaturatedThroughputSamples:  throughputSamples,
		SaturatedThroughputBorrowed: reading.borrowed,
		SaturatedThroughputDerived:  throughputBucket == derivedBucket,
		// Set only where the bound priced the replica, so a measured or
		// derived figure is never read as one.
		SaturatedThroughputLowerBound: lowerBound,
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
	routing replicaRouting,
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
	// The peak, or the instant reading where the peak has moved to a sibling,
	// as on the main path (residentCharge).
	kvUsageDemand := residentCharge(rm, int64(rm.KvCacheUsage*float64(rec.EffectiveCapacity)),
		rec.EffectiveCapacity, routing)

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
		"kvUsageDemand", kvUsageDemand, "localQueueDemand", localQueueDemand, "replicaDemand", replicaDemand,
		"kvFromInstant", routing.kvFromInstant)

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

// servedBucket is the bucket name logged for a measured mu that yielded to
// what the variant's Ready, full replicas serve (siblingYield).
const servedBucket = "served-siblings"

// computeK2 determines the compute-bound capacity using a priority chain:
// 1. Observed (queue saturated) → use tokensInUse as k2
// 2. Historical → rolling average from previous observations
// 3. Derived (from deployment args) → formula-based estimate
// 4. Fallback → k1 (memory-bound only)
// Returns the k2 the replica is priced on this cycle, the source that produced
// it, the k2 the chain gave -- the figure the capacity store may keep -- and
// the lower bound it was priced from when that is P2-borrow or P2-prior (zero
// otherwise). The first and third differ only for an unadmitted row
// (unadmittedPrice), which is then priced from no bound.
// historyKey is the replica's bucket from historyKey(); modelID, namespace
// and variantName are for the log lines only. adm is the row's admission
// (admissionOf): a row that is not admitted has its saturated reading logged
// as P1-obs-unadmitted, and the chain falls through, as for a prefill reading
// under a saturated decode.
func (a *SaturationAnalyzer) computeK2(
	historyKey string,
	modelID, namespace, variantName string,
	queueLen int, tokensInUse int64,
	avgOutput, avgInput float64,
	queueThreshold float64,
	engineParams *capacity.EngineParams,
	// ranks is the engine ranks the replica runs (domain.ReplicaMetrics.Ranks);
	// the derived k2 (P3) prices one engine and is scaled by it.
	ranks int,
	k1 int64,
	kvCeiling int64,
	role string,
	downstreamSaturated bool,
	adm admission,
	// borrowDisabled is the model's disableK2Borrow: no P2-borrow at all.
	borrowDisabled bool,
	logger logr.Logger,
) k2Decision {
	var refused string
	chain, chainSrc, borrowed := a.k2Chain(historyKey, modelID, namespace, variantName, queueLen, tokensInUse,
		avgOutput, avgInput, queueThreshold, engineParams, ranks, k1, kvCeiling, role, downstreamSaturated,
		adm, borrowDisabled, &refused, logger)
	k2, src, recordable := a.unadmittedPrice(chain, chainSrc, historyKey, modelID, namespace, variantName,
		queueLen, tokensInUse, queueThreshold, kvCeiling, downstreamSaturated, adm, logger)
	if src != chainSrc {
		// Priced at the unadmitted reading, not at the chain's lower bound.
		borrowed = k2Borrow{}
	}
	return k2Decision{k2: k2, src: src, recordable: recordable, borrowed: borrowed, refused: refused}
}

// k2Decision is what computeK2 decided for one replica: the k2 it is priced on
// this cycle and the source that produced it, the k2 the chain gave -- the
// figure the capacity store may keep -- and the lower bound it was priced from
// (zero unless the source is P2-borrow or P2-prior). One value rather than
// four results, so a caller cannot drop the bound by position.
type k2Decision struct {
	k2         int64
	src        capacity.K2Source
	recordable int64
	borrowed   k2Borrow
	// refused is the P1-obs-* label of a saturated reading k2Chain did not
	// record this cycle, empty when there was none.
	refused string
}

// unadmittedPrice returns the k2 a row is priced on this cycle, its source,
// and the k2 the chain gave, which is what the capacity store may keep.
//
// The two differ only for a saturated warm-pool bridge, or a saturated row of
// a variant mid-rollout. Neither reading may be written -- a bridge's
// describes the pool's engine, and mid-rollout the pod may run the old one --
// but it is what the replica is holding NOW. For a variant a bridge alone is
// carrying, or a bucket a rollout reaches with no window and no previous
// figure, it is the only reading there is: falling through to derived, or to
// k1 for prefill, priced the replica higher in the very cycle that showed it
// full, so fewer replicas were ordered while it was saturated -- for as long
// as the rollout lasted. So the row is priced at its reading for this cycle,
// and only where that is LOWER than the chain: a reading may only make one
// cycle more conservative, never a record. Nothing is written -- not the
// window, not the throughput (written only for P1), not the lower bound --
// so the next cycle prices from the chain again unless it is saturated too.
//
// A Pod of the variant's own that is not Ready is priced at the chain, mid-rollout
// or not. Its reading is not what it is holding now but what it held when it
// stopped being routed to -- an old pod draining, a new one failing its
// readiness probe -- or a hang with its gauges frozen, and a frozen reading
// would price it low for as long as the hang lasts: every cycle re-reads it,
// and a rollout stalled on that pod lasts as long.
func (a *SaturationAnalyzer) unadmittedPrice(
	chain int64, chainSrc capacity.K2Source,
	historyKey, modelID, namespace, variantName string,
	queueLen int, tokensInUse int64,
	queueThreshold float64,
	kvCeiling int64,
	downstreamSaturated bool,
	adm admission,
	logger logr.Logger,
) (k2 int64, src capacity.K2Source, recordable int64) {
	saturated := queueLen >= int(queueThreshold) && tokensInUse > 0
	plausible := kvCeiling <= 0 || tokensInUse <= kvCeiling
	// A bridge, or a Ready replica of the variant's own mid-rollout.
	atReading := adm.bridge || (adm.rollout && adm.admitted())
	if !atReading || downstreamSaturated || !saturated || !plausible || tokensInUse >= chain {
		return chain, chainSrc, chain
	}
	why := "a warm-pool bridge"
	if !adm.bridge {
		why = "mid-rollout"
	}
	logger.V(perReplicaLogLevel).Info("k2-decision",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"priority", capacity.K2SrcUnadmitted.String(), "historyKey", historyKey,
		"reason", "priced this cycle at the saturated reading, lower than the chain; not recorded ("+why+")",
		"k2", tokensInUse, "chainK2", chain, "chainPriority", chainSrc.String())
	return tokensInUse, capacity.K2SrcUnadmitted, chain
}

// k2Chain is the priority chain itself: P1 (a saturated reading, recorded),
// P2 (the variant's window), P3 (derived) and P4 (k1), with the lower-only
// borrow from a compatible sibling after P3 and P4. A row that is not
// admitted never writes the window and never starts the variant's borrow
// clock: a bridge serving a variant whose own Pods are pending would
// otherwise spend the borrow's hold before the variant ran a replica of its
// own.
func (a *SaturationAnalyzer) k2Chain(
	historyKey string,
	modelID, namespace, variantName string,
	queueLen int, tokensInUse int64,
	avgOutput, avgInput float64,
	queueThreshold float64,
	engineParams *capacity.EngineParams,
	ranks int,
	k1 int64,
	kvCeiling int64,
	role string,
	downstreamSaturated bool,
	adm admission,
	// borrowDisabled is the model's disableK2Borrow: no sibling's figure.
	// The variant's own previous figure (P2-prior) is not a borrow and stays.
	borrowDisabled bool,
	// refused receives the P1-obs-* label of a saturated reading this call
	// does not record, for replica-capacity-summary at the default level.
	refused *string,
	logger logr.Logger,
) (int64, capacity.K2Source, k2Borrow) {
	// adm.serving() (Ready, not a bridge, not mid-rollout) is the one
	// predicate for the lower bound: only such a row arms, advances, re-arms
	// or ends it (borrowedK2WithKv). Any other reads it as it stood at the
	// variant's last serving cycle.
	serving := adm.serving()
	// The window is checked against the arguments the variant runs before
	// anything reads or writes it: one written under others becomes the
	// variant's previous figure (window_args.go).
	args := windowArgs(engineParams)
	a.mu.Lock()
	a.k2WindowUnderLocked(historyKey, args)
	a.mu.Unlock()
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
	saturatedReading := queueLen >= int(queueThreshold) && tokensInUse > 0
	switch {
	case saturatedReading && downstreamSaturated:
		*refused = k2ReasonObsDownstream
		logger.V(perReplicaLogLevel).Info("k2-decision",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"priority", k2ReasonObsDownstream, "historyKey", historyKey,
			"queueLength", queueLen, "queueThreshold", queueThreshold,
			"reason", "queue saturated while the decode role is; a prefill completes only when decode admits it, so this is decode's saturation seen from prefill; not recorded",
			"tokensInUse", tokensInUse, "k1", k1)
	case saturatedReading && !adm.admitted():
		*refused = k2ReasonObsUnadmitted
		logger.V(perReplicaLogLevel).Info("k2-decision",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"priority", k2ReasonObsUnadmitted, "historyKey", historyKey,
			"queueLength", queueLen, "queueThreshold", queueThreshold,
			"reason", adm.why+"; not recorded",
			"tokensInUse", tokensInUse, "k1", k1)
	case saturatedReading && adm.rollout:
		*refused = k2ReasonObsRollout
		logger.V(perReplicaLogLevel).Info("k2-decision",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"priority", k2ReasonObsRollout, "historyKey", historyKey,
			"queueLength", queueLen, "queueThreshold", queueThreshold,
			"reason", "the variant is mid-rollout and this pod may run the old engine; not recorded",
			"tokensInUse", tokensInUse, "k1", k1)
	case saturatedReading && adm.unsettled != "":
		*refused = k2ReasonObsUnsettled
		logger.V(perReplicaLogLevel).Info("k2-decision",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"priority", k2ReasonObsUnsettled, "historyKey", historyKey,
			"queueLength", queueLen, "queueThreshold", queueThreshold,
			"reason", adm.unsettled,
			"tokensInUse", tokensInUse, "k1", k1)
	case saturatedReading:
		k2Observed := tokensInUse
		if kvCeiling > 0 && k2Observed > kvCeiling {
			*refused = k2ReasonObsImplausible
			logger.V(perReplicaLogLevel).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", k2ReasonObsImplausible, "historyKey", historyKey,
				"queueLength", queueLen, "queueThreshold", queueThreshold,
				"reason", "observed tokensInUse exceeds the KV cache's physical ceiling; discarding as implausible",
				"k2Observed", k2Observed, "k1", k1, "kvCeiling", kvCeiling)
		} else {
			now := a.now()
			a.mu.Lock()
			ra, ok := a.computeCapacityHistory[historyKey]
			// WriteGapExceedsAt, not StaleAt: this asks whether the last
			// OBSERVATION is old enough that today's belongs to a new
			// episode. StaleAt tracks use, and Priority 2 below reads this
			// window every cycle, so asking it could never report a gap for
			// a bucket the fleet is still serving -- two episodes weeks
			// apart would blend, and a stale high reading would hold
			// capacity up against today's lower, truer one.
			if !ok || ra.WriteGapExceedsAt(now, a.horizons.EpisodeGap) {
				ra = capacity.NewRollingAverage(capacity.RollingAverageWindowSize, now)
				a.computeCapacityHistory[historyKey] = ra
			}
			ra.AddAt(float64(k2Observed), now)
			a.paths.add(pathK2Obs, 1)
			if args != "" {
				a.k2WindowArgs[historyKey] = args
			}
			// The variant has measured itself under the arguments it runs:
			// a previous figure of the arguments it left is not a figure for
			// them.
			if lb := a.lowerBounds[historyKey]; lb != nil && lb.prior > 0 {
				lb.clearPrior()
			}
			k2Smoothed := int64(ra.Average())
			historyLen := ra.Len()
			a.mu.Unlock()
			logger.V(perReplicaLogLevel).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", capacity.K2SrcObserved.String(), "historyKey", historyKey,
				"queueLength", queueLen, "queueThreshold", queueThreshold,
				"k2Observed", k2Observed, "k2", k2Smoothed, "historyWindowLen", historyLen)
			return k2Smoothed, capacity.K2SrcObserved, k2Borrow{}
		}
	}

	// Priority 2: Historical — lock must cover Average() since Add() mutates
	// the same slice from Priority 1 under the same lock.
	//
	// NO staleness check. A retained window answers however old its figure
	// is, and the reason is at HistoryRetention: the measurement is the
	// lowest capacity figure available for this bucket, so refusing it can
	// only raise capacity. An intermediate revision guarded this read and
	// fell through to k1; it cost between 1.8x and 18x the replicas.
	//
	// TouchAt is what keeps the window alive to be read. The ONLY writer is
	// Priority 1 above, which needs a saturated queue, so without the touch
	// the window ages on "last saturated" -- and a comfortably provisioned
	// fleet does not saturate. Measured: a live variant serving happily for
	// 24h lost its measured k2 of 50,000 to a derived 332,800, for no reason
	// but the clock. Touching on the read ages it on "last USED", which is
	// the same correction variantSeenAt makes for the ITL maps. A window
	// nothing reads still ages out at HistoryRetention, so the leak closes;
	// one a decision depends on every cycle does not.
	//
	// The write path above asks WriteGapExceedsAt, not StaleAt, precisely because
	// these reads touch: a use-based check could never see the write gap it
	// is asking about. Two clocks, two questions -- see RollingAverage.
	a.mu.Lock()
	var histAvg float64
	var histLen int
	var histAge time.Duration
	if ra, ok := a.computeCapacityHistory[historyKey]; ok {
		histAvg = ra.Average()
		histLen = ra.Len()
		now := a.now()
		histAge = ra.WriteAgeAt(now)
		ra.TouchAt(now)
	}
	a.mu.Unlock()
	if histAvg > 0 {
		// historyAgeSeconds because nothing else carries it. The read no
		// longer refuses a window on age, so a figure measured an hour ago and
		// one measured last Tuesday reach this line identically, and the age
		// has no ceiling: a bucket read every cycle is retained indefinitely.
		// wva_analyzer_target publishes the capacity with no provenance at
		// all, and Reason carries only "P2-hist" -- so without this field an
		// operator chasing a replica count cannot tell a live measurement from
		// a stale one. Same line, one more field, no new cardinality.
		logger.V(perReplicaLogLevel).Info("k2-decision",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"priority", capacity.K2SrcHistorical.String(), "historyKey", historyKey,
			"queueLength", queueLen, "queueThreshold", queueThreshold,
			"k2", int64(histAvg), "historyWindowLen", histLen,
			"historyAgeSeconds", int64(histAge.Seconds()))
		return int64(histAvg), capacity.K2SrcHistorical, k2Borrow{}
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
	//
	// Reached only when Priority 2 found NO window. A bucket this fleet has
	// measured keeps answering from its measurement for as long as the window
	// is retained, however old the figure is, and that is deliberate.
	//
	// The reason is narrow, so state it narrowly. The revision this replaced
	// refused a measurement past 24h and fell through to PRIORITY 4, which
	// returns k1 as k2 -- and since effectiveCapacity = min(k1, k2), k1 is the
	// largest value that expression can take. So refusing a measurement in
	// favour of k1 can only raise capacity and order fewer replicas: measured
	// at 1.8x to 18x depending on k1.
	//
	// NOT because every alternative is larger than a measurement. Derived is
	// not: when --max-num-seqs binds, estimateCapacityFromParams lands at
	// (I + O/2)/(I + O) of the engine's physical occupancy ceiling -- 57% at
	// I=1000, O=6000 -- so a saturated measurement can exceed it, and
	// retaining one can then order FEWER replicas than falling to derived
	// would have. 95,000 measured against 64,000 derived at max_num_seqs=16.
	// An earlier version of this comment claimed the general rule and a
	// reviewer built the counterexample. k1 is the bound that always holds.
	// borrow is the key's lower bound: P2-prior, or P2-borrow unless the
	// model switched the sibling borrow off, when a bound running on a
	// sibling's figure is not consulted and its clock does not move.
	borrow := func(own int64) (k2Borrow, bool) {
		return a.borrowedK2WithKv(historyKey, modelID, own, engineParams, kvCeiling, serving, !borrowDisabled)
	}
	isPrefill := domain.CanonicalRole(role) == domain.RolePrefill
	if !isPrefill {
		// The formula prices ONE engine: B and S are --max-num-batched-tokens
		// and --max-num-seqs, which bound a single rank. A replica of several
		// data-parallel ranks runs that many engines side by side, and k1
		// beside it is already the whole replica's KV, so the derived figure
		// is scaled to the replica. Left per rank, min(k1, k2) priced a pod of
		// eight ranks at about one (issue #150).
		if k2Derived := estimateCapacityFromParams(engineParams, avgInput, avgOutput) * int64(max(ranks, 1)); k2Derived > 0 {
			if b, ok := borrow(min(k1, k2Derived)); ok {
				logBorrowedK2(logger, b, modelID, namespace, variantName, historyKey, min(k1, k2Derived), capacity.K2SrcDerived)
				return b.k2, b.source(), b
			}
			logger.V(perReplicaLogLevel).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", capacity.K2SrcDerived.String(), "historyKey", historyKey,
				"avgInputTokens", avgInput, "avgOutputTokens", avgOutput,
				"engineParams", engineParams, "engineRanks", max(ranks, 1), "k2", k2Derived)
			return k2Derived, capacity.K2SrcDerived, k2Borrow{}
		}
	}

	// Priority 4: Fallback to k1
	if b, ok := borrow(k1); ok {
		logBorrowedK2(logger, b, modelID, namespace, variantName, historyKey, k1, capacity.K2SrcFallback)
		return b.k2, b.source(), b
	}
	reason := "no observed/historical/derived k2; capacity is memory-bound only"
	if isPrefill {
		reason = "prefill role: derived-from-args formula assumes decode-style output length; skipped"
	}
	logger.V(perReplicaLogLevel).Info("k2-decision",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"priority", capacity.K2SrcFallback.String(), "historyKey", historyKey,
		"reason", reason,
		"k1", k1)
	return k1, capacity.K2SrcFallback, k2Borrow{}
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
//  2. Own k1 if TotalKvCapacityTokens is known (from num_gpu_blocks_override,
//     or the measured total carried across a flag change)
//  3. Own PriorLiveCapacity: what the variant was last measured at before a
//     rollout changed its arguments (boundedBy=prior-live)
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

			// Bound by what this variant was last measured at, under
			// arguments a rollout has since replaced: a ceiling only, so a
			// parked variant whose cache changed shape is never priced above
			// its last measurement, and never held below a smaller derivation.
			if rec.PriorLiveCapacity > 0 && rec.PriorLiveCapacity < bounded {
				bounded = rec.PriorLiveCapacity
				boundedBy = "prior-live"
			}

			// Bound by compatible variant's live EffectiveCapacity (already min(k1,k2))
			//
			// The reuse switch DELIBERATELY DOES NOT REACH HERE, and a
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
			//
			// For the same reason the clamp is found under the looser equality
			// (FindClampBound): the borrow's equality compares the parallel
			// layout, speculative decoding, the KV connector, offloading, prefix
			// caching and the checkpoint, and refusing a sibling on those removed
			// the ceiling rather than tightening it.
			if compatible := a.capacityStore.FindClampBound(namespace, modelID, accelerator, gpuCount, rec.EngineParams); compatible != nil && compatible.LearnedFrom == capacity.LearnedFromLive && compatible.EffectiveCapacity > 0 {
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

	// Fallback: stored EffectiveCapacity (EffectiveMaxBatchedTokens from
	// LoadFromDeployment), under the same prior-live ceiling.
	fallback := rec.EffectiveCapacity
	if rec.PriorLiveCapacity > 0 && rec.PriorLiveCapacity < fallback {
		fallback = rec.PriorLiveCapacity
	}
	logger.Info("zero-replica-capacity-estimate",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"source", "deployment-stored-fallback",
		"reason", "no workload averages or derivation available; using raw stored EffectiveMaxBatchedTokens",
		"perReplicaCapacity", fallback)
	return float64(fallback)
}

// estimateCapacityFromParams computes a capacity estimate using the k2 derivation
// formula: N_steady = min(B * O / (I + O), S), capacity = N_steady * (I + O/2).
// Used by computeK2 (Priority 3) for per-replica estimation and by
// estimateStoredCapacity for zero-replica variants with model-level workload averages.
// Returns 0 if estimation is not possible.
//
// The figure is ONE engine's: B and S bound a single data-parallel rank.
// computeK2 multiplies it by the ranks the live replica reports. The
// zero-replica path has no live replica to count and keeps the one-engine
// figure, which can only under-price a data-parallel variant and order more
// on its way up from zero; its first live replica then prices it in full.
func estimateCapacityFromParams(params *capacity.EngineParams, avgInput, avgOutput float64) int64 {
	if params == nil || params.EffectiveMaxBatchedTokens <= 0 || avgOutput <= 0 {
		return 0
	}

	B := float64(params.EffectiveMaxBatchedTokens)
	// S is the engine's batch cap: max_num_seqs, or a lower
	// --max-num-active-seqs, the most sequences vLLM runs at once (#335).
	S := float64(params.BatchCap())
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
	row := medianK2Row(replicas)
	if row == nil {
		return ""
	}
	if label := row.K2Priority.String(); label != "" {
		return label
	}
	return domain.ReasonError
}

// k2SourceK1Bound is the k2 source published for a variant whose k2, from
// whichever source, is at or above k1: the KV cache prices the replica, and
// the k2's own source and age say nothing about the price. The cycle log's
// Reason still names the k2 source.
const k2SourceK1Bound = "k1-bound"

// k2DoesNotBind reports whether row's k2 is at or above its k1, so that k1
// is what prices it. A P4-k1 row is its own answer: its k2 is k1 by
// construction, and on the store fallback both are the stored estimate.
func k2DoesNotBind(row *capacity.ReplicaCapacity) bool {
	return row.K2Priority != capacity.K2SrcFallback &&
		row.ComputeBoundCapacity >= row.MemoryBoundCapacity
}

// medianK2Row is the replica k2SourceLabel names: the lower median by
// EffectiveCapacity, from a sorted copy. Nil when replicas is empty. With an
// even count the variant's price averages the central pair, so this row is
// the lower of the two figures it averages.
func medianK2Row(replicas []capacity.ReplicaCapacity) *capacity.ReplicaCapacity {
	if len(replicas) == 0 {
		return nil
	}
	sorted := make([]capacity.ReplicaCapacity, len(replicas))
	copy(sorted, replicas)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].EffectiveCapacity < sorted[j].EffectiveCapacity
	})
	return &sorted[(len(sorted)-1)/2]
}
