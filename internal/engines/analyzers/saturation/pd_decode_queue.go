package saturation

import (
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// disaggregated reports whether rolesByVariant names a prefill variant: the
// model is split into prefill and decode, and its decode replicas' waiting
// counts hold requests waiting for their KV from prefill. Whether the prefill
// variant has a Ready replica does not matter; the split is the model's shape.
// The balance package reads the same test (balance.hasPrefill).
func disaggregated(rolesByVariant map[string]string) bool {
	for _, role := range rolesByVariant {
		if domain.CanonicalRole(role) == domain.RolePrefill {
			return true
		}
	}
	return false
}

// decodeCapacityView returns rm, a replica of a disaggregated model, with its
// waiting queue as decode capacity reads it if it is a decode replica; any
// other role's row is returned as it is. decodeCapacityViews decides whether
// the model is disaggregated.
//
// vLLM counts a decode request whose KV is still in flight from prefill in
// num_requests_waiting. That request has its blocks and is waiting on the
// network, not on this replica, so it is not a backlog this replica's
// capacity drains: read as one, a decode at KV 0.5 with 768 such requests
// tripped the saturation test, recorded a k2 at half its cache, and was
// charged the 768 as queue demand and floor backlog.
//
//   - Where vLLM says why each request waits (CapacityQueueKnown, v0.20.0
//     and later), the queue is the part waiting for scheduling capacity
//     (reason="capacity"). A request that cannot get blocks for its pull
//     waits there, so a full decode still reads queued.
//   - Where it does not, the decode is judged on its KV, as the load-
//     imbalance diagnostic judges it (internal/signals/balance): its queue
//     counts only while its busiest rank is full (decodeKvFull), when the
//     queue is a replica unable to allocate. Below that it reads zero. A
//     decode bound by its sequence ceiling before its KV then reads
//     unqueued; that case needs the capacity series to be seen.
//
// Aggregated replicas, prefill, and decode of a model with no prefill variant
// serve whole requests, and their queue stays the full count. Every reader
// of the per-replica queue in pricing goes through this view -- the
// saturation test and k2 record (computeK2), the k2 settle gate, the
// throughput admission's unqueued test (servedBounds), the local queue demand
// (waitingQueueDemand) and the floor's backlog (ReplicaCapacity.QueueLength)
// -- while the idle tests keep the raw count: a request in flight to a
// replica is load on it.
func decodeCapacityView(rm domain.ReplicaMetrics, role string, kvCacheThreshold float64) domain.ReplicaMetrics {
	if domain.CanonicalRole(role) != domain.RoleDecode {
		return rm
	}
	switch {
	case rm.CapacityQueueKnown:
		rm.MaxRankQueueLength = rm.BusiestRankCapacityQueue()
		rm.QueueLength = rm.CapacityQueueLength
	case decodeKvFull(rm, kvCacheThreshold):
	default:
		rm.QueueLength, rm.MaxRankQueueLength = 0, 0
	}
	return rm
}

// decodeCapacityViews is decodeCapacityView over rows when the model is
// disaggregated, and rows as they are when it is not.
func decodeCapacityViews(rows []domain.ReplicaMetrics, rolesByVariant map[string]string, kvCacheThreshold float64) []domain.ReplicaMetrics {
	if !disaggregated(rolesByVariant) {
		return rows
	}
	out := make([]domain.ReplicaMetrics, len(rows))
	for i, rm := range rows {
		out[i] = decodeCapacityView(rm, rolesByVariant[rm.VariantName], kvCacheThreshold)
	}
	return out
}

// decodeKvFull reports whether rm's busiest engine rank is full: kvFull where
// the row carries its cache size, and otherwise its busiest rank's KV
// utilization at or over kvCacheThreshold (a row priced from the capacity
// store has no token counts). A reading that is not a finite number is not
// full.
func decodeKvFull(rm domain.ReplicaMetrics, kvCacheThreshold float64) bool {
	if rm.TotalKvCapacityTokens > 0 {
		return kvFull(rm, kvCacheThreshold)
	}
	kv := rm.BusiestRankKvUsage()
	return !math.IsNaN(kv) && !math.IsInf(kv, 0) && kv > 0 && kv >= kvCacheThreshold
}
