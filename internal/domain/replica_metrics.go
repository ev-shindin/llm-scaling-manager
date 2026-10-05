package domain

import "time"

// This file holds the ANALYZER-AGNOSTIC replica signal.
//
// It lived in saturation_analyzer.go, which read as though it belonged to one
// analyzer. It does not: every analyzer that works from measured rows consumes
// ReplicaMetrics (saturation, throughput), the collector produces it, and the
// capacity-build step depends on what it carries. A shared type named after one
// consumer invites exactly the change this split exists to prevent -- a rule
// written into one analyzer and silently absent from the others.

// ReplicaMetrics holds one scale-target replica's measured signal.
//
// It is per *replica*, in the same units as domain.VariantMetadata and as the
// replica targets the optimizer produces: one record per pod, or for LWS per
// group (only leader pods emit engine metrics, so a leader's record is its
// group's). A data-parallel pod exposes one metrics endpoint per DP rank and is
// scraped as several series keyed pod_name:port; the collector merges those
// into this one record (collector.collapseToPods) so that counting these
// records yields a replica count and the capacity derived from one is a
// per-replica capacity.
//
// It carries *signal only*. Variant identity (cost, accelerator, role) belongs
// to the discovery step and reaches consumers via domain.VariantMetadata; it is
// not repeated on every replica record.
type ReplicaMetrics struct {
	PodName      string
	KvCacheUsage float64 // KV cache utilization (0.0-1.0)
	QueueLength  int     // Number of requests waiting
	VariantName  string  // Name of the variant this replica belongs to
	Namespace    string
	ModelID      string // Model ID for grouping variants

	// FromWarmPool marks a record that came from a BRIDGE: a warm pool Pod lent
	// to this variant while it is short, rather than one of its own replicas.
	//
	// Its load is this variant's load and counts toward DEMAND like any other.
	// Its capacity is not this variant's SUPPLY: the Pod is borrowed and goes
	// back when the ordinary replicas arrive, so counting it would tell the
	// optimizer the fleet is already big enough and suppress the scale-up the
	// bridge exists to cover -- after which the replicas that would release the
	// Pod are the ones it just prevented.
	//
	// So the two aggregations treat it differently, and this flag is what lets
	// them. What it is worth is measured separately and published for the
	// retained-pool switching decision; see decision.WarmPoolSupply.
	//
	// A BOOL, NOT A POOL NAME, AND THAT RESTS ON AN INVARIANT: a variant is
	// warmed by AT MOST ONE POOL. Its bridges therefore all come from the same
	// pool, run at that pool's --gpu-memory-utilization, and are directly
	// comparable -- which is what makes one median over them meaningful and what
	// lets decision.WarmPoolSupply key by variant alone.
	//
	// Different variants of one model MAY sit in different pools, including the
	// prefill and decode of the same model: a variant is named by its
	// ScaledObject, role included, and every figure here is per variant, so those
	// aggregate independently and never meet.
	//
	// Warming ONE variant from two pools would break this -- their bridges would
	// carry genuinely different capacities and the median would describe neither
	// -- and the pool layer makes that unreachable rather than merely unlikely.
	// A variant names at most one pool, in the `warmPool` trigger metadata key,
	// and warmpool.VariantsFor hands a variant only to the pool it names (or to
	// the single pool, when a namespace has exactly one and there is nothing to
	// disambiguate). With several pools declared, a variant naming none is
	// Unassignable: it gets no warm copy at all, and is reported.
	FromWarmPool bool

	// Ready is the Pod's own readiness condition, read from the Pod and not
	// inferred from the fact that it answered a scrape.
	//
	// The two are not the same and the gap is not small: an engine serves
	// /metrics as soon as its HTTP server is up, which is BEFORE it passes
	// readiness, so a starting replica reports for some seconds while no Service
	// and no EPP will route to it.
	//
	// That gap matters to exactly one consumer today. The warm pool asks how
	// many of a variant's own replicas are serving, to decide whether the Pod it
	// lent can go home; answering with a replica that is reporting but not yet
	// in the rotation hands the traffic back to nothing. It does NOT bear on
	// capacity: a Pod that is starting still holds its GPU and its KV cache is
	// real, so the analyzer counts it.
	Ready bool

	// StartSeconds is how long this replica took to become Ready: the Pod's
	// Ready condition LastTransitionTime less its CreationTimestamp. Zero when
	// the Pod is not ready, when either timestamp is missing, or when the
	// difference is not credible (see collector.podStartSeconds).
	//
	// It is the dead time the demand floor projects the backlog over. Measured
	// per replica and aggregated per VARIANT by the analyzer, because it is an
	// image, a set of engine flags and a node -- two variants of one model
	// routinely differ.
	StartSeconds float64

	// Metadata contains freshness information (optional)
	Metadata *ReplicaMetricsMetadata `json:"metadata,omitempty"`

	// --- Fields for Saturation Analyzer V2 ---

	// NumGpuBlocks is the total number of KV cache blocks allocated on GPU.
	// Sourced from vllm:cache_config_info label "num_gpu_blocks".
	// Zero value means cache_config_info metric is not available.
	NumGpuBlocks int64

	// BlockSize is the number of tokens per KV cache block.
	// Sourced from vllm:cache_config_info label "block_size".
	// Zero value means cache_config_info metric is not available.
	BlockSize int64

	// TotalKvCapacityTokens is NumGpuBlocks × BlockSize (total token slots).
	// Computed by the collector after parsing cache_config_info labels.
	// Zero value means capacity data is unavailable.
	TotalKvCapacityTokens int64

	// TokensInUse is the derived current token demand on this replica.
	// Computed as KvCacheUsage × TotalKvCapacityTokens.
	// Zero when TotalKvCapacityTokens is unavailable.
	TokensInUse int64

	// AvgOutputTokens is the average generation tokens per request on this replica.
	// Derived from rate(generation_tokens_sum) / rate(generation_tokens_count).
	// Used by saturation V2 for token-demand estimation (k2 derivation) and by
	// the queueing model analyzer for RequestSize and service rate computation.
	// Zero when metrics are unavailable.
	AvgOutputTokens float64

	// AvgOutputTokensRecent is the same quantity over a SHORT window: the mean
	// output tokens per request among requests that completed in roughly the
	// last minute.
	//
	// It exists because AvgOutputTokens is count-weighted over [5m], and a
	// count-weighted mean is the wrong statistic for a bimodal workload: after
	// a 6000 -> 250 output switch, the rare long stragglers dominate the mean
	// and a five-minute window keeps them in it for five minutes after they
	// stop arriving. Measured per decode pod across one such switch, the [5m]
	// figure decayed 3750 -> 2814 -> 2382 -> 2278 -> 2136 -> 250 while the
	// arriving work was 250 throughout; the [1m] form converged in about a
	// minute.
	//
	// Read as the derived mu's divisor (mu = seqs / (ITL * OL)), where the error
	// passes straight through and cost a 4x over-order, and -- paired with
	// AvgInputTokensRecent, never alone -- as the shape mu is priced at while a
	// shape change is outstanding. Everything else keeps AvgOutputTokens: a
	// short window everywhere would only make the shape noisier on a quiet
	// fleet.
	//
	// Zero when the engine does not publish it or the pod completed nothing in
	// the window; the divisor then falls back to AvgOutputTokens.
	AvgOutputTokensRecent float64

	// AvgInputTokensRecent is AvgInputTokens over the SAME short window as
	// AvgOutputTokensRecent, and exists only so the two can be read together.
	//
	// KVreq = ILeff + OL/2 is dominated by the prompt only while the prompt is
	// the large half. At 6000 in / 1000 out, OL/2 is 500 of 6500 -- 8%. At
	// 1000 in / 4000 out it is 2000 of 3000 -- 67%. A shape swap crosses from
	// the first to the second, which is exactly when a one-sided short window
	// is worst: taking the [1m] output while the prompt still carries the [5m]
	// average prices this shape's generation on top of the previous shape's
	// prompt, and that sum is larger than either real shape.
	//
	// Measured across one 6000/1000 -> 1000/4000 switch: the divisor took the
	// [1m] output and reached 4000 within two cycles while KVreq still read
	// 5896 against a settled 3000, so the derived mu read 1.57 req/s against a
	// settled 2.76 and the floor ordered 7 decode replicas where 3 was right.
	//
	// Zero when the engine does not publish it or the pod completed nothing in
	// the window; the caller then falls back to AvgInputTokens.
	AvgInputTokensRecent float64

	// AvgInputTokens is the average prompt tokens per request on this replica.
	// Derived from rate(prompt_tokens_sum) / rate(prompt_tokens_count).
	// Used by saturation V2 for token-demand estimation (k2 derivation) and by
	// the queueing model analyzer for RequestSize and service rate computation.
	// Zero when metrics are unavailable.
	AvgInputTokens float64

	// PrefixCacheHitRate is the fraction of prefix cache queries that were hits (0.0-1.0).
	// Derived from rate(vllm:prefix_cache_hits[5m]) / rate(vllm:prefix_cache_queries[5m]).
	// Used to reduce estimated input token demand for scheduler-queued requests.
	// Zero when prefix caching is disabled or metrics are unavailable.
	PrefixCacheHitRate float64

	// AvgServiceTime is how long a request occupies this replica, in seconds,
	// EXCLUDING time spent waiting in the queue.
	//
	// The distinction is what makes it usable for sizing. End-to-end latency
	// climbs when a replica is behind and falls when it catches up, so it varies
	// with capacity; service time is what one request costs to serve and does
	// not. Zero when the engine does not publish it or nothing has completed.
	AvgServiceTime float64

	// AvgITL is the average inter-token latency on this replica in seconds.
	// Derived from rate(vllm:time_per_output_token_seconds_sum[5m]) / rate(..._count[5m]).
	// Used by queueing model tuner as observed ITL for Kalman filter parameter learning.
	// TA notation: ITL_obs — the (k*, ITL_obs) pair drives OLS calibration of ITL(k) = A·k + B.
	// Zero when metrics are unavailable.
	AvgITL float64

	// AvgTTFT is the average time to first token on this replica in seconds.
	// Derived from rate(vllm:time_to_first_token_seconds_sum[1m]) / rate(..._count[1m]),
	// and the SGLang histogram of the same name.
	//
	// This is the PREFILL side's latency, as AvgITL is decode's: a prefill
	// replica computes the prompt, emits one token and hands the KV on, so on a
	// disaggregated fleet TTFT is most of what it does.
	//
	// IT IS A DIAGNOSTIC. Nothing in the decision path reads it, and that is a
	// measured conclusion rather than an omission: it INCLUDES queue wait, so a
	// TTFT that rises at constant work means the fleet is behind, not that a
	// replica got slower. Fitting TTFT(T) = A·T + B to recover a capacity from
	// it was built and then removed -- the intercept reached 82 seconds because
	// queue wait landed in a term meant to be a fixed per-request cost, and the
	// velocities swung 56,764 to 392,195 tok/s against a measured ~138,000.
	// docs/proposals/prefill-ttft-model.md records that result. Prefill's
	// capacity comes from PrefillComputedTokenRate below, which excludes queue
	// wait by construction and needs no regression at all.
	// Zero when metrics are unavailable.
	AvgTTFT float64

	// PrefillComputedTokenRate is the rate at which this replica computes
	// prefill KV tokens, in tokens/second, excluding tokens the prefix cache
	// already held. Derived from
	// rate(vllm:request_prefill_kv_computed_tokens_sum[1m]).
	//
	// This is PREFILL's capacity in the unit prefill is bounded by, and the
	// figure shape_change.go's saturatedCompletionRate was waiting for. A
	// request rate cannot serve: under overload it is the rate the fleet is
	// being SERVED at, which reads the same at one replica and at ten
	// (measured: 4.75, 4.62 and 4.50 req/s at 1, 2 and 10 replicas while the
	// token rate went 138,875 to 933,750), and a rate learned at one prompt
	// length says nothing about another.
	//
	// Zero on SGLang, which publishes no per-stage prefill counter
	// (sgl-project/sglang issue #14303); those variants keep the request-rate
	// reading.
	PrefillComputedTokenRate float64

	// --- Fields for Throughput Analyzer ---

	// GenerationTokenRate is the observed decode token generation rate on this replica (tokens/sec).
	// Derived from rate(vllm:request_generation_tokens_sum[1m]) per pod.
	// TA notation: μ_dec^obs — directly observable supply proxy; also used as a sanity check
	// against the demand estimate (μ_dec^obs ≈ λ_dec at steady state with no queueing).
	// Zero when metrics are unavailable.
	GenerationTokenRate float64

	// KvUsageInstant is the instantaneous KV cache utilization fraction on this replica (0.0–1.0).
	// Derived from vllm:kv_cache_usage_perc (no max_over_time window).
	// TA notation: k* — the current operating point in the ITL model ITL(k) = A·k + B.
	// Differs from KvCacheUsage which uses max_over_time[1m] for the saturation analyzer.
	// Zero when metrics are unavailable.
	KvUsageInstant float64

	// RequestRate is the engine-side request completion rate on this replica (req/s).
	// Engine-agnostic: derived per pod from rate(vllm:request_generation_tokens_count[1m])
	// for vLLM and rate(sglang:generation_tokens_histogram_count[1m]) for SGLang.
	// TA notation: fallback λ_req — used when ArrivalRate == 0 (EPP not deployed).
	// λ_dec_fallback = sum(RequestRate) × avg(AvgOutputTokens).
	// Measures completed requests only; undercounts when requests queue in the scheduler.
	// Zero when metrics are unavailable.
	RequestRate float64
}

// ReplicaMetricsMetadata contains freshness information for replica metrics
type ReplicaMetricsMetadata struct {
	// CollectedAt is when the metrics were collected
	CollectedAt time.Time
	// Age is the age of the metrics
	Age time.Duration
	// FreshnessStatus indicates freshness: "fresh", "stale", "unavailable"
	FreshnessStatus string
}
