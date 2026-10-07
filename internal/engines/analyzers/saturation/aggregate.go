package saturation

import (
	"sort"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Per-replica figures summed into the variant and role totals the optimizer
// reads.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

// aggregateByVariant groups replica capacities by variant and computes
// per-variant capacity metrics.
func (a *SaturationAnalyzer) aggregateByVariant(
	replicaCapacities []capacity.ReplicaCapacity,
	inputMetrics []domain.ReplicaMetrics,
	variantStates []domain.VariantReplicaState,
	modelID, namespace string,
	kvCacheThreshold float64,
	// reuseDisabled is config.ScalingPolicy.DisableLearnedStateReuse, passed
	// as the resolved value the way kvCacheThreshold is rather than as the
	// policy, so this function keeps taking only what it reads.
	reuseDisabled bool,
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
				kvCacheThreshold, modelAvgInput, modelAvgOutput, reuseDisabled, logger)
			capacityLabel = satReasonP0Store
		} else if rec := a.lookupCompatibleCapacity(namespace, modelID, vs.VariantName, accelerator, vs.GPUsPerReplica, reuseDisabled); rec != nil {
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
