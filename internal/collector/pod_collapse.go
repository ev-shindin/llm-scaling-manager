package collector

import (
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// collapseToPods merges the per-instance rows of each pod into a single row, so
// a domain.ReplicaMetrics is one *scale-target replica* rather than one engine
// instance.
//
// A vLLM pod running data parallelism with one API server per rank hosts DP
// independently-scraped engine instances, distinguished by the port in the
// instance label; the collector keys them "pod:port". A pod running its ranks
// behind ONE API server is a single instance whose series differ only by vLLM's
// engine label; the per-pod queries fold those ranks in PromQL by the same rules
// as below (registration/saturation.go), so it arrives here as one row already.
// Either way the row's EngineRanks says how many ranks it covers, and the two
// layouts give the same row for the same ranks. Everything downstream, though,
// counts in scale-target units:
// the optimizer's targets, MaxReplicas, GPUsPerReplica, CurrentReplicas and
// PendingReplicas are all pods (or, for LWS, groups). Emitting per-instance rows
// made the analyzers' supply arithmetic disagree with the units its result is
// spent in — a DP=4 variant would have its pod target computed from a per-
// instance capacity. Collapsing here keeps that arithmetic in one unit without
// every consumer having to know a DP factor the cluster never reports.
//
// LWS needs no special handling: worker pods are dropped upstream, so a group's
// leader is already its single row.
//
// Merging is keyed by pod AND MODEL, not by pod alone -- see podModel. A warm
// pool Pod holds several models at once, and summing their capacity into one
// row would credit a variant with cache belonging to a model it is not running.
//
// How each field merges follows from what it measures:
//
//   - Capacities, queue depth, rates and engine ranks are extensive — summed. A
//     pod's KV capacity is the sum of its instances' capacities, and a request
//     served by any instance is a request served by the pod.
//   - Utilization fractions (KvCacheUsage, KvUsageInstant) are recomputed as
//     Σtokens/Σcapacity rather than averaged, so an instance with a larger cache
//     weighs proportionally more. Without capacity to weigh by they fall back to
//     the plain mean.
//   - The busiest rank's queue and utilization (MaxRankQueueLength,
//     MaxRankKvCacheUsage) take the max over the instances: they describe one
//     engine, the fullest, which the saturation guards judge. The capacity
//     part of the queue (CapacityQueueLength, MaxRankCapacityQueueLength)
//     merges the same way, and is known only if every instance reported it.
//   - Every other presence bit (the ...Known fields) is likewise known only if
//     every instance reported it. ITL's three states merge as described at
//     the loop: known if one rank measured a mean, none is absent and no
//     empty rank generated tokens; empty only if every rank is.
//   - Request-shape ratios (input/output tokens, service time, TTFT) are
//     weighted by RequestRate: they describe the average request, so the
//     instance serving more of them should dominate. ITL is a mean per
//     generated token and weighs by GenerationTokenRate; the prefix hit rate
//     is a fraction of prompt tokens and weighs by the prompt-token rate. Each
//     weight is the count the matching PromQL mean divides by, so ranks merged
//     here and ranks folded by the per-pod queries give the same figure. With
//     no traffic anywhere on the pod they fall back to the plain mean.
//   - Metadata takes the worst freshness and the oldest age across the
//     instances, so a pod is only as trustworthy as its stalest rank.
//
// Input order is preserved by first appearance of each pod. Rows are copied;
// the input slice is not modified.
func collapseToPods(instances []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	if len(instances) < 2 {
		return instances
	}

	order := make([]podModel, 0, len(instances))
	byPod := make(map[podModel][]domain.ReplicaMetrics, len(instances))
	for _, m := range instances {
		k := podModel{pod: m.PodName, model: m.ModelID}
		if _, seen := byPod[k]; !seen {
			order = append(order, k)
		}
		byPod[k] = append(byPod[k], m)
	}
	if len(order) == len(instances) {
		return instances // one instance per pod: nothing to merge
	}

	pods := make([]domain.ReplicaMetrics, 0, len(order))
	for _, k := range order {
		pods = append(pods, mergePodInstances(byPod[k]))
	}
	return pods
}

// podModel is the merge key: one pod's ranks OF ONE MODEL.
//
// The pod alone is not enough, and the case that breaks it is the warm pool. An
// ordinary Pod belongs to exactly one variant, so its rows can only be DP ranks
// of the same engine. A POOL Pod holds several models at once -- that is what a
// warm set is -- each on its own port, and the collector keys instances
// "pod:port", so they arrive as several rows for one Pod.
//
// Merging those would sum the KV capacity of unrelated models. On a lent Pod
// every row carries the variant it is lent to, so the sleepers' cache would be
// added to the awake model's supply: the variant is told it has more capacity
// than the engine serving it has, which is the direction that causes a
// scale-DOWN the fleet cannot then serve.
//
// Data parallelism is ranks of the SAME model, so the model separates the two.
type podModel struct {
	pod   string
	model string
}

// mergePodInstances merges one pod's instance rows per the rules documented on
// collapseToPods. group is non-empty.
func mergePodInstances(group []domain.ReplicaMetrics) domain.ReplicaMetrics {
	if len(group) == 1 {
		return group[0]
	}

	// Identity is shared by construction: the rows were grouped by pod, and a
	// pod belongs to one variant of one model.
	pod := group[0]

	pod.NumGpuBlocks = 0
	pod.TotalKvCapacityTokens = 0
	pod.EngineRanks = 0
	pod.TokensInUse = 0
	pod.QueueLength = 0
	pod.MaxRankQueueLength = 0
	pod.MaxRankKvCacheUsage = 0
	pod.CapacityQueueLength = 0
	pod.MaxRankCapacityQueueLength = 0
	pod.RequestRate = 0
	pod.GenerationTokenRate = 0
	pod.RunningRequests = 0
	pod.PreemptionRate = 0

	var weightedKvUsage, weightedKvInstant float64
	// Known only if every rank reported it: a rank with no series would
	// otherwise be averaged in as an empty cache.
	pod.KvUsageInstantKnown = true
	// The same for the capacity part of the queue: a rank without it would
	// count none of its queue as waiting for capacity.
	pod.CapacityQueueKnown = true
	pod.PreemptionRateKnown = true
	pod.KvCacheUsageKnown = true
	pod.PrefillComputedTokenRateKnown = true
	pod.GenerationTokenRateKnown = true
	pod.RequestRateKnown = true
	pod.RunningRequestsKnown = true
	// ITL has three states (domain.ITLState). The pod's mean is known when
	// at least one rank measured one and no rank is absent: an idle rank adds
	// no tokens to the per-token mean, a missing one would hide its own. It
	// is empty only when every rank is.
	//
	// An empty rank that GENERATED tokens (max_tokens=1 requests give a
	// generation rate and no inter-token interval) makes the pod absent:
	// AvgITL below weighs each rank by its generation rate, so that rank adds
	// weight with an ITL of 0 and dilutes the mean. The value is left as it
	// is, since the analyzers read it today; computing the pod's mean over
	// the finite ranks only is #341 4a's named change, and until then the
	// state says the mean is not a measurement.
	itlAnyKnown, itlAllEmpty, itlNoneAbsent, itlEmptyGenerated := false, true, true, false
	for _, m := range group {
		pod.KvUsageInstantKnown = pod.KvUsageInstantKnown && m.KvUsageInstantKnown
		pod.PreemptionRateKnown = pod.PreemptionRateKnown && m.PreemptionRateKnown
		pod.CapacityQueueKnown = pod.CapacityQueueKnown && m.CapacityQueueKnown
		pod.KvCacheUsageKnown = pod.KvCacheUsageKnown && m.KvCacheUsageKnown
		pod.PrefillComputedTokenRateKnown = pod.PrefillComputedTokenRateKnown && m.PrefillComputedTokenRateKnown
		pod.GenerationTokenRateKnown = pod.GenerationTokenRateKnown && m.GenerationTokenRateKnown
		pod.RequestRateKnown = pod.RequestRateKnown && m.RequestRateKnown
		pod.RunningRequestsKnown = pod.RunningRequestsKnown && m.RunningRequestsKnown
		state := m.ITLState()
		itlAnyKnown = itlAnyKnown || state == domain.ITLFinite
		itlAllEmpty = itlAllEmpty && state == domain.ITLEmpty
		itlNoneAbsent = itlNoneAbsent && state != domain.ITLAbsent
		itlEmptyGenerated = itlEmptyGenerated || (state == domain.ITLEmpty && m.GenerationTokenRate > 0)
		pod.CapacityQueueLength += m.CapacityQueueLength
		pod.MaxRankCapacityQueueLength = max(pod.MaxRankCapacityQueueLength, m.BusiestRankCapacityQueue())
		pod.NumGpuBlocks += m.NumGpuBlocks
		pod.TotalKvCapacityTokens += m.TotalKvCapacityTokens
		// Ranks(), not EngineRanks: a row without a cache_config_info series
		// is still one engine, and the pod still has one rank per row.
		pod.EngineRanks += m.Ranks()
		pod.TokensInUse += m.TokensInUse
		pod.QueueLength += m.QueueLength
		// The busiest rank of the pod is the busiest rank of its busiest
		// row: a max, as the per-pod query takes it over ranks behind one
		// API server. Each row's figure is read through the row's own ranks,
		// so a row of one rank gives its queue and utilization as they are.
		pod.MaxRankQueueLength = max(pod.MaxRankQueueLength, m.BusiestRankQueue())
		pod.MaxRankKvCacheUsage = max(pod.MaxRankKvCacheUsage, m.BusiestRankKvUsage())
		pod.RequestRate += m.RequestRate
		pod.GenerationTokenRate += m.GenerationTokenRate
		pod.RunningRequests += m.RunningRequests
		pod.PreemptionRate += m.PreemptionRate

		weightedKvUsage += m.KvCacheUsage * float64(m.TotalKvCapacityTokens)
		weightedKvInstant += m.KvUsageInstant * float64(m.TotalKvCapacityTokens)
	}

	if !pod.CapacityQueueKnown {
		pod.CapacityQueueLength, pod.MaxRankCapacityQueueLength = 0, 0
	}
	pod.AvgITLKnown = itlAnyKnown && itlNoneAbsent && !itlEmptyGenerated
	pod.AvgITLEmpty = itlAllEmpty && !itlEmptyGenerated

	// BlockSize is a per-instance config value, uniform across a pod's ranks;
	// keeping the first preserves NumGpuBlocks × BlockSize == TotalKvCapacityTokens.
	capacity := float64(pod.TotalKvCapacityTokens)
	if capacity > 0 {
		pod.KvCacheUsage = weightedKvUsage / capacity
		pod.KvUsageInstant = weightedKvInstant / capacity
	} else {
		pod.KvCacheUsage = mean(group, func(m domain.ReplicaMetrics) float64 { return m.KvCacheUsage })
		pod.KvUsageInstant = mean(group, func(m domain.ReplicaMetrics) float64 { return m.KvUsageInstant })
	}

	pod.AvgInputTokens = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgInputTokens })
	pod.AvgOutputTokens = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgOutputTokens })
	// Weighted like their [5m] siblings: both are per-request costs, so the
	// instance that served more requests counts for more.
	pod.AvgOutputTokensRecent = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgOutputTokensRecent })
	pod.AvgInputTokensRecent = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgInputTokensRecent })
	// ITL is a per-TOKEN mean, so each instance weighs by the tokens it
	// generated. That is what the per-pod ITL query computes for ranks behind
	// one API server (sum of the ITL sums over sum of the counts, one count
	// per generated token), so both layouts agree. Falls back to the request
	// rate when no instance reports a generation rate.
	pod.AvgITL = weightedBy(group, func(m domain.ReplicaMetrics) float64 { return m.AvgITL },
		func(m domain.ReplicaMetrics) float64 { return m.GenerationTokenRate })
	// Weighted like AvgITL: both are per-request costs, so the engine instance
	// that served more requests should count for more when merging a pod's
	// instances into one replica.
	pod.AvgServiceTime = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgServiceTime })
	pod.AvgTTFT = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgTTFT })
	// SUMMED, not weighted: the engines of one pod each compute part of
	// the prefill work and their token rates add. Averaging them would
	// halve a two-engine replica's capacity.
	pod.PrefillComputedTokenRate = 0
	for _, m := range group {
		pod.PrefillComputedTokenRate += m.PrefillComputedTokenRate
	}
	// The hit rate is a fraction of the prompt tokens looked up, so each
	// instance weighs by its prompt-token rate, as the per-pod query weighs a
	// rank by its prefix_cache_queries_total. Falls back to the request rate.
	pod.PrefixCacheHitRate = weightedBy(group, func(m domain.ReplicaMetrics) float64 { return m.PrefixCacheHitRate },
		func(m domain.ReplicaMetrics) float64 { return m.RequestRate * m.AvgInputTokens })

	pod.Metadata = mergeMetadata(group)
	return pod
}

// weightedByRequestRate averages field across the group weighted by each
// instance's RequestRate, falling back to the plain mean when the pod is idle
// (every rate zero) and so carries no traffic to weigh by.
func weightedByRequestRate(group []domain.ReplicaMetrics, field func(domain.ReplicaMetrics) float64) float64 {
	var weighted, totalRate float64
	for _, m := range group {
		weighted += field(m) * m.RequestRate
		totalRate += m.RequestRate
	}
	if totalRate > 0 {
		return weighted / totalRate
	}
	return mean(group, field)
}

// weightedBy averages field across the group weighted by weight, falling back
// to weightedByRequestRate when no instance carries any weight, or a weight is
// not a finite non-negative number.
func weightedBy(group []domain.ReplicaMetrics, field, weight func(domain.ReplicaMetrics) float64) float64 {
	var weighted, total float64
	for _, m := range group {
		w := weight(m)
		if !(w >= 0) || math.IsInf(w, 0) {
			return weightedByRequestRate(group, field)
		}
		weighted += field(m) * w
		total += w
	}
	if total > 0 {
		return weighted / total
	}
	return weightedByRequestRate(group, field)
}

// mean returns the arithmetic mean of field across the group. group is non-empty.
func mean(group []domain.ReplicaMetrics, field func(domain.ReplicaMetrics) float64) float64 {
	var sum float64
	for _, m := range group {
		sum += field(m)
	}
	return sum / float64(len(group))
}

// mergeMetadata reports the pod as only as fresh as its least fresh instance:
// the worst FreshnessStatus and the oldest Age. Returns nil when no instance
// carried metadata.
func mergeMetadata(group []domain.ReplicaMetrics) *domain.ReplicaMetricsMetadata {
	var merged *domain.ReplicaMetricsMetadata
	for _, m := range group {
		if m.Metadata == nil {
			continue
		}
		if merged == nil {
			copied := *m.Metadata
			merged = &copied
			continue
		}
		if m.Metadata.Age > merged.Age {
			merged.Age = m.Metadata.Age
		}
		if freshnessSeverity[m.Metadata.FreshnessStatus] > freshnessSeverity[merged.FreshnessStatus] {
			merged.FreshnessStatus = m.Metadata.FreshnessStatus
		}
		if m.Metadata.CollectedAt.After(merged.CollectedAt) {
			merged.CollectedAt = m.Metadata.CollectedAt
		}
	}
	return merged
}
