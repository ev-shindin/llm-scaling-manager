package saturation

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

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
//     (reason="capacity"). A request that cannot get blocks for its pull, or
//     waits at the sequence cap, waits there, so a full or capped decode
//     still reads queued.
//   - Where it does not, the queue is the full count, as before. Which part
//     of it is in flight cannot be told, and reading too little of it is the
//     direction that breaks SLOs: a decode capped by --max-num-seqs below a
//     full cache queues for real, and dropping that queue would record no k2
//     and leave its capacity at k1.
//
// Which readers take the view, and which keep the raw row, follows the same
// asymmetry: each takes the reading that errs toward stricter learning and
// toward keeping capacity.
//
//   - The view prices the replica's own capacity and demand -- the saturation
//     test and k2 record (computeK2), the k2 settle gate, the throughput
//     admission's unqueued test (servedBounds), the local queue demand
//     (waitingQueueDemand), the floor's backlog (ReplicaCapacity.QueueLength)
//     and the idle admission (admissionOf), where a row reading idle records
//     nothing.
//   - The raw row is kept where a deferred request counted as waiting only
//     holds more: decode's saturation as prefill sees it (roleSaturated,
//     which gates prefill's learning and holds its demand), the busiest-queue
//     load that keeps a replica from being given away
//     (busiestQueueLoadByVariant), and the idle tests (replicaIdle,
//     kvFromInstant), since a request in flight to a replica is load on it.
//
// Aggregated replicas, prefill, and decode of a model with no prefill variant
// serve whole requests, and their queue stays the full count.
func decodeCapacityView(rm domain.ReplicaMetrics, role string) domain.ReplicaMetrics {
	if domain.CanonicalRole(role) != domain.RoleDecode || !rm.CapacityQueueKnown {
		return rm
	}
	rm.MaxRankQueueLength = rm.BusiestRankCapacityQueue()
	rm.QueueLength = rm.CapacityQueueLength
	return rm
}

// decodeCapacityViews is decodeCapacityView over rows when the model is
// disaggregated (domain.HasPrefill), and rows as they are when it is not.
func decodeCapacityViews(rows []domain.ReplicaMetrics, rolesByVariant map[string]string) []domain.ReplicaMetrics {
	if !domain.HasPrefill(rolesByVariant) {
		return rows
	}
	out := make([]domain.ReplicaMetrics, len(rows))
	for i, rm := range rows {
		out[i] = decodeCapacityView(rm, rolesByVariant[rm.VariantName])
	}
	return out
}
