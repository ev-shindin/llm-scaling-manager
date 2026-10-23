package capacity

import "time"

// ReplicaCapacity holds the per-replica capacity breakdown computed by
// the saturation analyzer: what it measured on the replica, what it learned
// about it, and the throughput reading the floor (signals/floor) prices with.
type ReplicaCapacity struct {
	PodName               string
	VariantName           string
	AcceleratorName       string
	TokensInUse           int64
	TotalKvCapacityTokens int64
	MemoryBoundCapacity   int64    // k1: KV-cache-limited capacity
	ComputeBoundCapacity  int64    // k2: compute/scheduling-limited capacity
	K2Priority            K2Source // how k2 was computed
	// K2BorrowedFrom names the sibling variant whose measured k2 this one
	// used, when K2Priority is K2SrcBorrowed; K2BorrowRelaxed is how far that
	// borrow has relaxed back to the variant's own figure, 0 at full strength
	// and 1 fully relaxed. Diagnostics: published as
	// wva_learned_state_borrowed and wva_learned_state_borrow_relaxed.
	K2BorrowedFrom  string
	K2BorrowRelaxed float64
	// K2WriteAge is how long ago the k2 window this replica's figure came
	// from was last WRITTEN: the replica's own window for P1-obs and
	// P2-hist, the donor's for P2-borrow. A read does not reset it -- a
	// window is touched on every read so it is retained, which says nothing
	// about how old its figure is (see RollingAverage). K2WriteAgeKnown is
	// false for P3 and P4, which come from no window. Diagnostics: published
	// as wva_learned_state_k2_age_seconds.
	K2WriteAge        time.Duration
	K2WriteAgeKnown   bool
	EffectiveCapacity int64 // min(k1, k2)
	// ReplicaDemand is the replica's resident KV tokens — TokensInUse on the main
	// path, kvCacheUsage * effectiveCapacity on the fallback path — plus the
	// role-aware waiting-queue footprint: queueLength * avgInputTokens for
	// prefill replicas, and queueLength * (avgInputTokens + avgOutputTokens) for
	// decode/"both". See the saturation analyzer's waitingQueueDemand.
	ReplicaDemand int64
	// QueueLength is the number of requests waiting in this replica's engine
	// queue, and LocalQueueDemand the residency charge waitingQueueDemand put
	// on them, which ReplicaDemand includes. Carried separately so the
	// throughput model (floor.Estimate) can take the residency charge
	// back out and price the same requests as work to be done instead.
	QueueLength      int
	LocalQueueDemand int64
	// QueueRoutedFraction is the share of this replica's engine queue left
	// out of its charge because a sibling of its role has been idle long
	// enough to show it has room for it (the saturation analyzer's
	// routeQueuesToIdleRoom): 0 when nothing was moved, 1 when the idle
	// siblings' room covers every queue of the role. LocalQueueDemand (so
	// ReplicaDemand) already excludes that share, and the throughput floor
	// leaves the same share of QueueLength out of its backlog. Zero on a role
	// with no settled-idle replica, so its queues count in full.
	QueueRoutedFraction float64
	// ServiceRate is the replica's own completion rate this cycle (req/s; for
	// prefill, the prompt tokens it computed over the prompt length), 0 when
	// it has none; ServiceRateOnRecord is the rate its bucket has on record
	// (measured saturated throughput, else the derived one), 0 when none.
	// The saturation analyzer reads the two to tell a prefill replica that is
	// idle from one that is busy at a KV its work does not fill.
	ServiceRate         float64
	ServiceRateOnRecord float64

	// FromWarmPool marks a BRIDGE: a warm pool Pod lent to this variant rather
	// than one of its own replicas. Carried through from the collector so
	// aggregation can put its demand in and keep its capacity out. See
	// domain.ReplicaMetrics.FromWarmPool.
	FromWarmPool bool

	// SaturatedThroughput is the completion rate (requests/s) one replica of
	// this bucket sustains when its queue is saturated, from the history
	// recorded beside k2; 0 when no saturation has been observed for the
	// bucket. Read by floor.Estimate.
	SaturatedThroughput float64
	// SaturatedThroughputSamples is how many readings the window that
	// produced SaturatedThroughput holds, and SaturatedThroughputBorrowed
	// whether that window is a neighbouring bucket's rather than the
	// replica's own (see the saturation analyzer's nearestSaturatedThroughput).
	// The floor orders a
	// replica only on an own window of MinThroughputSamplesToOrder readings;
	// anything less holds.
	SaturatedThroughputSamples  int
	SaturatedThroughputBorrowed bool
	// SaturatedThroughputDerived is set when SaturatedThroughput was
	// DERIVED from the variant's ITL(k) model rather than measured from a
	// saturated cycle. A derived figure is priced for the shape arriving
	// now, so it needs no sample count and is never borrowed from another
	// shape's window -- the two fields above describe the measured window
	// that was NOT used, and are left alone so they stay honest about it.
	SaturatedThroughputDerived bool
	// SaturatedThroughputLineBorrowed is set when the ITL(k) model the derived
	// figure came from was BORROWED from a sibling variant that shares this
	// one's engine configuration, rather than fitted from this variant's own
	// readings.
	//
	// It exists because the sentence above -- a derived figure "is never
	// borrowed from another" -- stopped being true when a variant with no fit
	// of its own was allowed to start from a sibling's line. That is a real
	// saving -- about 28 cycles, measured on run QT from load start, which an
	// earlier anchor on the controller's boot inflated to ~60 -- and it is
	// evidence about a
	// CONFIGURATION rather than about this variant's load: the same line at
	// the same k implies a different service rate for two variants serving
	// different request shapes. So the floor may hold a fleet on it and must
	// not GROW one on it, which is the same rule it already applies to a
	// reading borrowed from a neighbouring shape bucket.
	SaturatedThroughputLineBorrowed bool
	// SaturatedThroughputLowerBound is set when SaturatedThroughput is a
	// prefill replica's LOWER BOUND on its service rate rather than a
	// reading of it: the computed prompt tokens per second it was seen to
	// sustain with its queue saturated while its own window could not
	// record -- in practice, while decode was saturated and metering it.
	//
	// The true rate is at least this, so the error goes one way: a fleet
	// priced on it is over-ordered, never under-ordered. The floor may grow
	// prefill on it, within a growth cap, and only with
	// MinThroughputSamplesToOrder readings; an own window always wins.
	// SaturatedThroughputSamples counts the bound's readings when this is set.
	SaturatedThroughputLowerBound bool
}
