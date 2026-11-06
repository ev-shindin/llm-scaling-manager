package domain

// LatencyBucket is one cumulative histogram bucket: the per-second rate of
// requests whose latency was at most UpperBound seconds. The +Inf bucket has
// UpperBound = math.Inf(1) and carries the total.
type LatencyBucket struct {
	UpperBound float64
	Rate       float64
}

// RouterLatency is one model's request latency as the router (the llm-d EPP)
// measured it, over WindowSeconds.
//
// The router's time to first token starts when the request reaches the router,
// BEFORE flow-control admission, so it is the one server-side TTFT that
// includes the router's queue -- the number a client's TTFT SLO is judged on.
// Only streaming requests are counted: for a non-streaming response the router
// records the whole request duration as its TTFT.
type RouterLatency struct {
	// WindowSeconds is the rate window the figures below were taken over.
	WindowSeconds float64

	// TTFTBuckets is the cumulative TTFT histogram, as per-second rates,
	// sorted by bound. Nil when the router exports no TTFT series for the
	// model.
	TTFTBuckets []LatencyBucket

	// TPOTSumRate and TPOTCountRate are the rates of the streaming
	// time-per-output-token histogram's _sum and _count. Their ratio is the
	// mean TPOT over the window, request-weighted. Both zero when absent.
	TPOTSumRate   float64
	TPOTCountRate float64

	// TPOTBuckets is the router's per-request streaming TPOT histogram, as
	// per-second bucket rates over the same window, sorted by bound: the
	// distribution behind the mean above. Nil when the router exports no
	// buckets for the model or the query failed. Read by nothing yet
	// (docs/proposals/analyzer-learner.md, 2.9); left out of a learner
	// recording while nil, so a recording made before it existed is written
	// again byte for byte.
	TPOTBuckets []LatencyBucket `learnrec:"omitzero"`

	// FlowControlWaitBuckets is the router's flow-control queue wait
	// histogram, seconds, of this model's dispatched requests, as per-second
	// bucket rates over the same window, sorted by bound: the router-queue
	// part of the TTFT above. Nil with no such series or a failed query, and
	// non-nil does not prove the gate is on (AnalyzerInput.FlowControlSeries).
	// Collected only while a learner recording is on, read by nothing yet,
	// and left out of a recording while nil.
	FlowControlWaitBuckets []LatencyBucket `learnrec:"omitzero"`
}

// Judgeable reports whether r is a reading of the router's latency: a TTFT
// histogram or a TPOT request rate. The two recorded histograms alone
// (TPOTBuckets, FlowControlWaitBuckets) are not one, so the slo analyzer
// treats a struct holding only them as no reading, as it did before they
// were collected. Nil is not judgeable.
func (r *RouterLatency) Judgeable() bool {
	return r != nil && (len(r.TTFTBuckets) > 0 || r.TPOTCountRate != 0)
}

// FlowControlPool is one InferencePool's flow-control layer in the router
// (the llm-d EPP), over WindowSeconds: how saturated the pool read, and how
// requests left it. How long they waited is per model
// (RouterLatency.FlowControlWaitBuckets).
//
// These series carry the pool (inference_pool) and no model, so a pool is
// joined to a model only when the pool serves that one model alone: the
// flow-control queue gauge names both, and a pool it names with two models
// is joined to neither. Numbers only: the pool's name is not kept.
//
// Each figure is behind a presence bit, or is a histogram that is nil when
// absent: a series that is missing, NaN or unreadable is not a zero. Read by
// nothing yet (docs/proposals/analyzer-learner.md, 2.9); a zero field is
// left out of a learner recording (learnrec.OmitZeroTag).
type FlowControlPool struct {
	// WindowSeconds is the rate window of the figures below.
	WindowSeconds float64 `learnrec:"omitzero"`

	// EffectiveSaturation is the pool's saturation score as the gate reads
	// it (stage "effective", the larger of the two below), averaged over the
	// window; PrefillSaturation and DecodeSaturation are the P/D stages' own
	// (stages "prefill" and "decode"). At 1 the gate stops dispatching; a
	// pool with no endpoint reads 1. The highest over the router's
	// replicas. Each Known bit says the series was read finite.
	EffectiveSaturation      float64 `learnrec:"omitzero"`
	EffectiveSaturationKnown bool    `learnrec:"omitzero"`
	PrefillSaturation        float64 `learnrec:"omitzero"`
	PrefillSaturationKnown   bool    `learnrec:"omitzero"`
	DecodeSaturation         float64 `learnrec:"omitzero"`
	DecodeSaturationKnown    bool    `learnrec:"omitzero"`

	// DispatchedRate, RejectedRate and EvictedRate are the requests per
	// second that left the queue by each outcome (the outcome label's
	// Dispatched, Rejected... and Evicted... values); OtherOutcomeRate is any
	// other outcome. OutcomesKnown is set when the outcome series was read.
	DispatchedRate   float64 `learnrec:"omitzero"`
	RejectedRate     float64 `learnrec:"omitzero"`
	EvictedRate      float64 `learnrec:"omitzero"`
	OtherOutcomeRate float64 `learnrec:"omitzero"`
	OutcomesKnown    bool    `learnrec:"omitzero"`
}
