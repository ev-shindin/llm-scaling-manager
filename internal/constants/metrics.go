// Package constants provides centralized constant definitions for the autoscaler.
// This file contains metric-related constants (VLLM input metrics, WVA output metrics, and metric label names).
package constants

// VLLM Input Metrics
// These metric names are used to query VLLM (vLLM inference engine) metrics from Prometheus.
// The metrics are emitted by VLLM servers and consumed by the collector to make scaling decisions.
const (
	// VLLMNumRequestRunning tracks the current number of running requests.
	// Used to validate metrics availability.
	VLLMNumRequestRunning = "vllm:num_requests_running"

	// VLLMRequestSuccessTotal tracks the total number of successful requests.
	// Used to calculate arrival rate.
	VLLMRequestSuccessTotal = "vllm:request_success_total"

	// VLLMRequestPromptTokensSum tracks the sum of prompt tokens across all requests.
	// Used with VLLMRequestPromptTokensCount to calculate average output tokens.
	VLLMRequestPromptTokensSum = "vllm:request_prompt_tokens_sum"

	// VLLMRequestPromptTokensCount tracks the count of requests for token generation.
	// Used with VLLMRequestPromptTokensSum to calculate average output tokens.
	VLLMRequestPromptTokensCount = "vllm:request_prompt_tokens_count"

	// VLLMRequestGenerationTokensSum tracks the sum of generated tokens across all requests.
	// Used with VLLMRequestGenerationTokensCount to calculate average output tokens.
	VLLMRequestGenerationTokensSum = "vllm:request_generation_tokens_sum"

	// VLLMRequestGenerationTokensCount tracks the count of requests for token generation.
	// Used with VLLMRequestGenerationTokensSum to calculate average output tokens.
	VLLMRequestGenerationTokensCount = "vllm:request_generation_tokens_count"

	// VLLMGenerationTokensTotal is a counter of generation tokens, incremented as
	// each token is produced.
	//
	// Not interchangeable with VLLMRequestGenerationTokensSum, which carries the
	// same running total but is a HISTOGRAM observed when a request finishes: its
	// rate is zero while a long generation runs and jumps by the whole request at
	// completion. Anything measuring how fast a replica is working needs this one;
	// anything measuring per-request size needs the histogram.
	VLLMGenerationTokensTotal = "vllm:generation_tokens_total"

	// VLLMTimeToFirstTokenSecondsSum tracks the sum of TTFT (Time To First Token) across all requests.
	// Used with VLLMTimeToFirstTokenSecondsCount to calculate TTFT.
	VLLMTimeToFirstTokenSecondsSum = "vllm:time_to_first_token_seconds_sum"

	// VLLMTimeToFirstTokenSecondsCount tracks the count of requests for TTFT.
	// Used with VLLMTimeToFirstTokenSecondsSum to calculate TTFT.
	VLLMTimeToFirstTokenSecondsCount = "vllm:time_to_first_token_seconds_count"

	// VLLMRequestPrefillKVComputedTokensSum is the running total of KV tokens
	// a replica has actually COMPUTED during prefill. vLLM's own help text:
	// "Histogram of new KV tokens computed during prefill (excluding cached
	// tokens)".
	//
	// Its rate is prefill's capacity in the unit prefill is bounded by. A
	// request rate is not: under overload it is the rate the fleet is being
	// served at, which reads the same at one replica and at ten (measured on
	// run PK: 4.75, 4.62 and 4.50 req/s at 1, 2 and 10 replicas, while the
	// token rate went 138,875 to 933,750). Tokens also carry across a shape
	// change, where a request rate learned at one prompt length is worthless
	// at another.
	//
	// "Excluding cached tokens" is doing real work here: a prefix the cache
	// already holds costs prefill nothing, so this is the prompt the replica
	// truly computed, with no hit-rate discount to apply afterwards.
	//
	// SGLang has no equivalent. It publishes TTFT and inter-token latency but
	// no per-stage prefill figure (sgl-project/sglang issue #14303), so a
	// SGLang prefill variant keeps the request-rate reading.
	VLLMRequestPrefillKVComputedTokensSum = "vllm:request_prefill_kv_computed_tokens_sum"

	// VLLMRequestPrefillTimeSeconds is the time a request spent in the PREFILL
	// phase, queue wait excluded (vLLM counts waiting separately, as
	// request_queue_time_seconds). Collected as a diagnostic beside the token
	// rate, not as a capacity: with batching, concurrent requests each report
	// their own prefill duration, so the sum over-counts wall-clock and a
	// per-request figure understates what the replica does.
	VLLMRequestPrefillTimeSeconds = "vllm:request_prefill_time_seconds"

	// VLLMTimePerOutputTokenSecondsSum tracks the sum of time per output token across all requests.
	// Used with VLLMTimePerOutputTokenSecondsCount to calculate ITL (Inter-Token Latency).
	VLLMTimePerOutputTokenSecondsSum = "vllm:time_per_output_token_seconds_sum"

	// VLLMTimePerOutputTokenSecondsCount tracks the count of requests for time per output token.
	// Used with VLLMTimePerOutputTokenSecondsSum to calculate ITL (Inter-Token Latency).
	VLLMTimePerOutputTokenSecondsCount = "vllm:time_per_output_token_seconds_count"

	// VLLMKvCacheUsagePerc tracks the KV cache utilization as a percentage (0.0-1.0).
	// Used by saturation analyzer to detect KV cache saturation and prevent OOM errors.
	VLLMKvCacheUsagePerc = "vllm:kv_cache_usage_perc"

	// VLLMNumRequestsWaiting tracks the number of requests waiting in the queue.
	// Used by saturation analyzer to detect request queue saturation.
	VLLMNumRequestsWaiting = "vllm:num_requests_waiting"

	// VLLMCacheConfigInfo is an info-style gauge that exposes KV cache configuration as labels.
	// Labels include num_gpu_blocks, block_size, cache_dtype, etc.
	// Value is always 1.0. Used by Saturation Analyzer V2 for token capacity computation.
	VLLMCacheConfigInfo = "vllm:cache_config_info"

	// VLLMPrefixCacheHits is a counter of prompt tokens served from the prefix
	// cache. vLLM declares it as vllm:prefix_cache_hits; as a Counter it is
	// exposed with a _total suffix, which is the name Prometheus stores.
	// Used with VLLMPrefixCacheQueries to compute prefix cache hit rate.
	VLLMPrefixCacheHits = "vllm:prefix_cache_hits_total"

	// VLLMPrefixCacheQueries is a counter of prompt tokens looked up in the
	// prefix cache, exposed with a _total suffix like VLLMPrefixCacheHits.
	// Used with VLLMPrefixCacheHits to compute prefix cache hit rate.
	VLLMPrefixCacheQueries = "vllm:prefix_cache_queries_total"
)

// SGLang Input Metrics
// These metric names are used to query SGLang inference engine metrics from Prometheus.
// Names and types were taken from SGLang's metrics collector
// (python/sglang/srt/observability/metrics_collector.py). SGLang labels its metrics
// with model_name, matching the label WVA filters on. See docs/proposals/sglang-backend.md.
const (
	// SGLangNumRunningReqs is the number of running requests (gauge).
	// SGLang equivalent of vllm:num_requests_running.
	SGLangNumRunningReqs = "sglang:num_running_reqs"

	// SGLangNumQueueReqs is the number of requests in the waiting queue (gauge).
	// SGLang equivalent of vllm:num_requests_waiting.
	SGLangNumQueueReqs = "sglang:num_queue_reqs"

	// SGLangTokenUsage is the KV-cache token-pool utilization as a fraction 0.0-1.0 (gauge).
	// SGLang equivalent of vllm:kv_cache_usage_perc.
	SGLangTokenUsage = "sglang:token_usage"

	// SGLangMaxTotalNumTokens is the total KV-cache token capacity (gauge).
	// SGLang exposes capacity directly, unlike vLLM which derives it from
	// vllm:cache_config_info (num_gpu_blocks x block_size).
	SGLangMaxTotalNumTokens = "sglang:max_total_num_tokens"

	// SGLangTimeToFirstTokenSecondsSum is the sum part of the TTFT histogram.
	// SGLang equivalent of vllm:time_to_first_token_seconds_sum.
	SGLangTimeToFirstTokenSecondsSum = "sglang:time_to_first_token_seconds_sum"

	// SGLangTimeToFirstTokenSecondsCount is the count part of the TTFT histogram.
	// SGLang equivalent of vllm:time_to_first_token_seconds_count.
	SGLangTimeToFirstTokenSecondsCount = "sglang:time_to_first_token_seconds_count"

	// SGLangInterTokenLatencySecondsSum is the sum part of the inter-token-latency histogram.
	// SGLang equivalent of vllm:inter_token_latency_seconds_sum.
	SGLangInterTokenLatencySecondsSum = "sglang:inter_token_latency_seconds_sum"

	// SGLangInterTokenLatencySecondsCount is the count part of the inter-token-latency histogram.
	// SGLang equivalent of vllm:inter_token_latency_seconds_count.
	SGLangInterTokenLatencySecondsCount = "sglang:inter_token_latency_seconds_count"

	// SGLangPromptTokensHistogramSum is the sum part of the prompt-token histogram.
	// SGLang equivalent of vllm:request_prompt_tokens_sum.
	SGLangPromptTokensHistogramSum = "sglang:prompt_tokens_histogram_sum"

	// SGLangPromptTokensHistogramCount is the count part of the prompt-token histogram.
	// SGLang equivalent of vllm:request_prompt_tokens_count.
	SGLangPromptTokensHistogramCount = "sglang:prompt_tokens_histogram_count"

	// SGLangGenerationTokensHistogramSum is the sum part of the generation-token histogram.
	// SGLang equivalent of vllm:request_generation_tokens_sum.
	SGLangGenerationTokensHistogramSum = "sglang:generation_tokens_histogram_sum"

	// SGLangGenerationTokensHistogramCount is the count part of the generation-token histogram.
	// SGLang equivalent of vllm:request_generation_tokens_count.
	SGLangGenerationTokensHistogramCount = "sglang:generation_tokens_histogram_count"

	// SGLangCachedTokensTotal is a counter of prompt tokens served from the prefix cache.
	// Used with SGLangPromptTokensTotal to compute the prefix cache hit rate, the
	// unit-safe analog of vllm:prefix_cache_hits_total / vllm:prefix_cache_queries_total.
	SGLangCachedTokensTotal = "sglang:cached_tokens_total"

	// SGLangPromptTokensTotal is a counter of total prompt tokens.
	SGLangPromptTokensTotal = "sglang:prompt_tokens_total"

	// SGLangNumRequestsTotal is a counter of received requests.
	// SGLang equivalent of vllm:request_success_total (used for scale-to-zero).
	SGLangNumRequestsTotal = "sglang:num_requests_total"
)

// llm-d Inference Scheduler Flow Control Metrics
// These metrics come from the Gateway API Inference Extension EPP (Endpoint Picker)
// flow control layer, not from vLLM pods. They are model-scoped (not per-pod).
//
// The EPP does not export a namespace label on these (gateway-api-inference-
// extension issue #2309). The namespace label they carry in Prometheus is added
// at scrape time from the EPP pod's namespace, and the controller's readers of
// them group by or match on it: grouped by model name alone, two namespaces
// serving the same model name would read the sum of both routers' queues.
const (
	// EPPFlowControlQueueSize is the number of requests queued in the inference
	// scheduler's flow control layer. This is the current name for the family
	// below: llm-d's EPP exports both and marks the inference_extension_ one
	// "[Deprecated: Use llm_d_epp_flow_control_queue_size]". Prefer this one and
	// keep the deprecated name only as a fallback.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: namespace is added at scrape time (the EPP pod's), not exported — see above.
	EPPFlowControlQueueSize = "llm_d_epp_flow_control_queue_size"

	// SchedulerFlowControlQueueSize is the deprecated alias of
	// EPPFlowControlQueueSize. Upstream gateway-api-inference-extension still
	// emits only this name (and without the inference_pool/model_name/
	// target_model_name labels llm-d's EPP adds), so it remains the fallback.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: namespace is added at scrape time (the EPP pod's), not exported — see above.
	SchedulerFlowControlQueueSize = "inference_extension_flow_control_queue_size"

	// SchedulerFlowControlQueueBytes is the total bytes of request bodies queued
	// in the inference scheduler's flow control layer.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: namespace is added at scrape time (the EPP pod's), not exported — see above.
	SchedulerFlowControlQueueBytes = "inference_extension_flow_control_queue_bytes"
)

// WVA Output Metrics
// These metric names are used to emit WVA (Workload Variant Autoscaler) metrics to Prometheus.
// The metrics expose scaling decisions and current state for monitoring and alerting.
const (
	// WVAReplicaScalingTotal is a counter that tracks the total number of scaling operations.
	// Labels: variant_name, namespace, direction (up/down), reason, accelerator_type
	WVAReplicaScalingTotal = "wva_replica_scaling_total"

	// WVADesiredReplicas is a gauge that tracks the desired number of replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVADesiredReplicas = "wva_desired_replicas"

	// WVAUnattributedGPUs counts GPUs held by variants whose accelerator type
	// could not be resolved. Such usage cannot be charged to any accelerator
	// pool, so every pool over-states how much is free by this amount and the
	// budget check lets wakes through that should have been refused. It is
	// exported because the condition is otherwise entirely silent — nothing
	// errors, capacity simply looks larger than it is.
	WVAUnattributedGPUs = "wva_unattributed_gpus"

	// WVACurrentReplicas is a gauge that tracks the current number of replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVACurrentReplicas = "wva_current_replicas"

	// WVADesiredRatio is a gauge that tracks the ratio of desired to current replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVADesiredRatio = "wva_desired_ratio"

	// WVAOptimizationDurationSeconds is a histogram that tracks the duration of each optimization cycle.
	// Labels: status (success, error)
	WVAOptimizationDurationSeconds = "wva_optimization_duration_seconds"

	// WVAReplicaStartSeconds is a histogram of how long each replica took to
	// become Ready: the Pod's Ready condition LastTransitionTime less its
	// CreationTimestamp. One observation per replica started, not per cycle.
	//
	// The demand floor projects the queue forward over this, so it is an INPUT
	// to a sizing decision rather than a report on one.
	WVAReplicaStartSeconds = "wva_replica_start_seconds"

	// WVAReplicaStartSecondsEstimate is the start time the controller is
	// actually sizing with, per variant, carrying a source label of "measured"
	// or "seed".
	//
	// Published beside the histogram rather than derived from it, because the
	// controller holds a running estimate and a quantile over the histogram is
	// not the same number. The histogram says what the cluster did; this says
	// what the controller believed, and a run where they disagree is the one
	// worth opening.
	WVAReplicaStartSecondsEstimate = "wva_replica_start_seconds_estimate"

	// LabelStartSource distinguishes a measured start time from a seeded one on
	// WVAReplicaStartSecondsEstimate.
	LabelStartSource = "source"

	// WVAEngineConfig is a per-variant gauge, always 1, whose labels carry the
	// engine-configuration fingerprint and the flags that produced it.
	//
	// One series per variant: the flag labels are functionally determined by
	// the variant, so they add width and not cardinality. It is published as
	// one wide series rather than as a fingerprint-keyed info metric joined to
	// a variant metric, because the join's natural shape is many-to-one (two
	// models can share a configuration) and a reader cannot then tell which
	// variant a row describes.
	//
	// Nothing reads the METRIC -- no code and no dashboard queries it, and it
	// exists to make the digest observable. The digest itself IS keyed on: it
	// composes the learned-state key that decides whose ITL line a variant
	// borrows (capacity.LearnedStateKey, via the analyzer's itlPhysicsKey).
	// An earlier version of this comment said nothing was keyed on it, which
	// was true when written and stopped being true in the same PR.
	WVAEngineConfig = "wva_engine_config"

	// WVALearnedStateBorrowed is a gauge, 1 while a variant is priced from a
	// figure a sibling variant learned: a k2 below its own capacity, or a
	// shared ITL line. Labels: namespace, model_name, variant_name, quantity
	// (k2 or itl-line), donor, donor_namespace. Absent when it borrows nothing.
	WVALearnedStateBorrowed = "wva_learned_state_borrowed"
	// WVALearnedStateBorrowRelaxed is a gauge of how far a variant's k2 borrow
	// has relaxed back to its own figure: 0 at full strength, 1 fully relaxed.
	// Labels: namespace, model_name, variant_name. Present only while
	// wva_learned_state_borrowed{quantity="k2"} is.
	WVALearnedStateBorrowRelaxed = "wva_learned_state_borrow_relaxed"
	// WVALearnedStateK2Source is a gauge, 1 for the source of the k2 a
	// variant is priced on (P1-obs, P2-hist, P2-borrow, P3-k2, P4-k1), or
	// k1-bound when the k2 is at or above k1 and the KV cache prices it.
	// Labels: namespace, model_name, variant_name, role, source. Absent when
	// no replica of the variant reported this cycle.
	WVALearnedStateK2Source = "wva_learned_state_k2_source"
	// WVALearnedStateK2AgeSeconds is how long ago the k2 a variant is priced
	// on was last measured -- written, not read. Same labels as
	// WVALearnedStateK2Source; present only for P1-obs, P2-hist, P2-borrow
	// and P2-prior, the sources that read a measured window. Like every
	// analyzer series it keeps its last value through a cycle whose
	// collection or analysis failed.
	WVALearnedStateK2AgeSeconds = "wva_learned_state_k2_age_seconds"
	// WVALearnedStateRestoresTotal counts variants whose restored learned
	// state was checked against their engine configuration
	// (LEARNED_STATE_SNAPSHOT), by outcome: whole, args, prior, incomplete or
	// differs (saturation.RestoreOutcome).
	WVALearnedStateRestoresTotal = "wva_learned_state_restores_total"
	// WVALearnedStateSnapshotErrorsTotal counts failed reads and writes of
	// the learned-state snapshot ConfigMap, by operation (read or write).
	WVALearnedStateSnapshotErrorsTotal = "wva_learned_state_snapshot_errors_total"
	// WVALearnedStateSnapshotBytes is the size of the learned-state document
	// last written, WVALearnedStateSnapshotBudgetBytes the bound it had to fit,
	// and WVALearnedStateSnapshotDroppedEntries how many entries, least
	// recently used first, that write left out to fit it.
	WVALearnedStateSnapshotBytes          = "wva_learned_state_snapshot_bytes"
	WVALearnedStateSnapshotBudgetBytes    = "wva_learned_state_snapshot_budget_bytes"
	WVALearnedStateSnapshotDroppedEntries = "wva_learned_state_snapshot_dropped_entries"

	// LabelFingerprint is the engine-configuration digest, and
	// LabelFingerprintVersion the hash's own version -- published beside it
	// because the hash's input set is a wire format, and a bump means every
	// stored key stops matching.
	LabelFingerprint        = "fingerprint"
	LabelFingerprintVersion = "fingerprint_version"

	// LabelAccelerator and LabelGPUsPerReplica complete the key that learned
	// state is filed under: (model, accelerator, gpus, fingerprint). They are
	// labels rather than hashed into the digest so an operator can ask "this
	// configuration on H200 versus H100" without a join.
	LabelAccelerator    = "accelerator"
	LabelGPUsPerReplica = "gpus"

	// WVAModelsProcessed is a gauge that tracks the number of models processed in the last optimization cycle.
	WVAModelsProcessed = "wva_models_processed"

	// WVADecisionsLimitedTotal is a counter that tracks the total number of decisions limited by the limiter.
	// Labels: variant_name, namespace, limiter_name
	WVADecisionsLimitedTotal = "wva_decisions_limited_total"

	// WVAGpuDiscoveryUp is a gauge that indicates whether GPU discovery is on or off.
	WVAGpuDiscoveryUp = "wva_gpu_discovery_up"

	// WVANodeAccessDenied is 1 while a configured physical limiter cannot read
	// nodes, and 0 when it can.
	//
	// This is the one combination that fails silently. A GPU-aware limiter
	// allocates out of per-accelerator pools, so a variant whose accelerator it
	// cannot resolve is charged to no pool, receives no budget, and never scales
	// up — with no error anywhere, because an unresolved accelerator is a normal
	// state when no limiter is configured. Node permission is optional right up
	// until someone turns the limiter on, and then it is load-bearing.
	WVANodeAccessDenied = "wva_node_access_denied"

	// WVAPodAccessDenied is 1 while the GPU usage walk of a namespace-scoped
	// controller may not list Pods cluster-wide and a physical limiter reads
	// that walk, and 0 otherwise. Absent on a
	// cluster-scoped controller, whose cache already holds every Pod. While it is
	// 1 the usage walk counts only the controller's own namespace, so GPUs other
	// namespaces hold on shared nodes read as free.
	WVAPodAccessDenied = "wva_pod_access_denied"

	// WVAWatchNamespaceUnreadable is 1, per namespace and reason, for a
	// namespace a namespace-scoped controller is told to watch and may not read
	// (reason forbidden: its RoleBinding is gone; namespace-not-found). The
	// controller runs on the others and leaves this one unmanaged until it can
	// read it again. Absent for a namespace it reads.
	WVAWatchNamespaceUnreadable = "wva_watch_namespace_unreadable"

	// WVAConfigRejectedEntries is the number of entries of one scaling-policy
	// ConfigMap that were rejected at its last load (labels: namespace,
	// configmap). A rejected entry keeps the previous entry under its key in
	// force when there was one; either way the ConfigMap does not say what is
	// running, so any value above 0 needs fixing.
	WVAConfigRejectedEntries = "wva_config_rejected_entries"

	// WVAConfigUnknownKeys is the number of keys, across the entries of one
	// scaling-policy ConfigMap, that the schema does not define (labels:
	// namespace, configmap). The entries apply with those keys ignored; a key
	// counted here is usually a misspelling of one that was meant to apply.
	WVAConfigUnknownKeys = "wva_config_unknown_keys"

	// WVAConfigIgnoredFields is the number of fields, across the entries of one
	// scaling-policy ConfigMap, whose value could not be used and act as unset
	// (labels: namespace, configmap). The entries still apply; the setting the
	// operator wrote does not.
	WVAConfigIgnoredFields = "wva_config_ignored_fields"

	// WVAPolicyNamespaceUnverified is 1 when the controller fell back to its own
	// ConfigMap for limiters and quotas because reading the Namespaces that decide
	// the policy source was Forbidden, and 0 otherwise. Set once at startup.
	//
	// A 1 means a cluster admin may have published policy (the well-known
	// wva-policy namespace, or a wva.llmd.ai/policy-namespace label) that this
	// controller cannot see, so it may be running under a weaker bound than the
	// admin believes is in force.
	WVAPolicyNamespaceUnverified = "wva_policy_namespace_unverified"

	// WVAConstraintProviderSkipped is 1 while a declared GPU constraint provider
	// -- the physical limiter or a quota entry -- is left out of scaling
	// decisions, by provider and reason (ProviderSkip*). The series is removed
	// when the provider bounds again, so absence means every declared limiter is
	// in force.
	//
	// A skipped provider lifts its bound: the others still apply, because the
	// constraints are intersected, but nothing checks what the skipped one
	// would have denied. Without the series that was one Info line per cycle.
	WVAConstraintProviderSkipped = "wva_constraint_provider_skipped"

	// WVAScaleFromZeroWakeUnchecked counts scale-from-zero wakes published
	// without any GPU capacity check: limiters are declared, every provider is
	// unavailable, and no recent snapshot of their constraints exists.
	WVAScaleFromZeroWakeUnchecked = "wva_scale_from_zero_wake_unchecked_total"

	// ProviderSkipUnobserved: the usage the provider is measured against (all
	// GPUs on the nodes, or WVA's own) has not been observed.
	ProviderSkipUnobserved = "unobserved"
	// ProviderSkipFailed: the provider could not compute its constraints (a
	// Kueue read failed, nodes could not be listed, ...).
	ProviderSkipFailed = "failed"
	// ProviderSkipNoConstraints: the provider returned none.
	ProviderSkipNoConstraints = "no_constraints"
	// ProviderSkipNodeReadDenied: a physical limiter declared beside quota
	// entries, on a controller that may not list nodes. The quota entries still
	// bound; a physical-only policy takes the controller NotReady instead.
	ProviderSkipNodeReadDenied = "node_read_denied"

	// WVAScaleFromZeroQueueFallbackActive is 1 while the scale-from-zero engine is
	// reading the EPP flow-control queue from Prometheus because the direct EPP
	// scrape is failing, and 0 while the direct scrape works. A sustained 1 means
	// wakes still happen but are slower and bounded by the Prometheus scrape
	// interval, and that the EPP metrics path (token, RBAC, NetworkPolicy) is
	// broken and should be fixed.
	WVAScaleFromZeroQueueFallbackActive = "wva_scale_from_zero_queue_fallback_active"

	// WVAAvailableGpus is a gauge that tracks the number of currently available GPUs. If wva_gpu_discovery_up is 1, it shows
	// the number of currently available GPUs. If wva_gpu_discovery_up is 0, it shows the number
	// of GPUs that were available at the last successful discovery.
	// Labels: accelerator_type
	WVAAvailableGpus = "wva_available_gpus"

	// WVAEnforcerModificationsTotal is a counter that tracks the total number of decision modifications made by the enforcer.
	// Labels: policy_type
	WVAEnforcerModificationsTotal = "wva_enforcer_modifications_total"

	// WVAOptimizerActive is a gauge that is 0 when an optimizer is inactive, and 1 when it's active.
	// Labels: optimizer_name
	WVAOptimizerActive = "wva_optimizer_active"
	// WVAErrorsTotal is a counter that tracks the total number of errors by component.
	// Labels: component, error_type
	WVAErrorsTotal = "wva_errors_total"
	// WVAAnalyzerDemand is a gauge exposing each analyzer's demand D (per model,
	// per role). Labels: analyzer_name, namespace, model_name, role.
	WVAAnalyzerDemand = "wva_analyzer_demand"
	// WVAAnalyzerTarget is a gauge exposing each analyzer's per-replica target P
	// (per variant). Labels: analyzer_name, namespace, model_name, variant_name.
	WVAAnalyzerTarget = "wva_analyzer_target"
	// WVAAnalyzerObservedReplicas is a gauge of how many of a variant's
	// replicas REPORTED METRICS in the analyzer's last cycle and were
	// attributed to it -- the live row count, not scale-target status, and not
	// capped at the scale target. 0 when nothing reported, including the cycle
	// that skips analysis because the whole model had no rows. Labels:
	// analyzer_name, namespace, model_name, variant_name.
	//
	// It answers "did the analyzer see the whole fleet this cycle", which no
	// other series can: demand is the greater of occupancy and the arrival
	// floor and so is not proportional to replicas, and the ReplicaCount behind
	// supply is capped at the scale target, which is exactly the number a
	// conceded or otherwise unowned replica exceeds. Read it beside the
	// target's READY replicas -- not wva_current_replicas, which counts pods
	// still starting and is legitimately above this during every scale-up --
	// to separate "a pod's metrics were missing from the cycle" from "replicas
	// are serving that the target does not own".
	WVAAnalyzerObservedReplicas = "wva_analyzer_observed_replicas"
	// WVAConfigInfo is an info-style gauge that exposes WVA configuration as labels.
	// Labels: analyzer_name, limiter_enabled, scale_to_zero_enabled
	WVAConfigInfo = "wva_config_info"

	// WVAConfigOptimizationIntervalSeconds is a gauge that tracks the optimization interval in seconds.
	WVAConfigOptimizationIntervalSeconds = "wva_config_optimization_interval_seconds"
	// WVAMetricsCollectionDurationSeconds is a histogram that tracks the duration of metrics collection operations.
	// Labels: query_type
	WVAMetricsCollectionDurationSeconds = "wva_metrics_collection_duration_seconds"

	// WVAMetricsCollectionErrorsTotal is a counter that tracks the total number of metrics collection errors.
	// Labels: query_type, reason
	WVAMetricsCollectionErrorsTotal = "wva_metrics_collection_errors_total"

	// WVAMetricsPodsDiscovered is a gauge that tracks the number of pods discovered for metrics collection.
	// Labels: namespace
	WVAMetricsPodsDiscovered = "wva_metrics_pods_discovered"

	// WVAMetricsFreshnessStatus is a gauge that tracks the freshness status of metrics for each variant.
	// Labels: variant_name, status
	WVAMetricsFreshnessStatus = "wva_metrics_freshness_status"

	// WVASaturationUtilization is a gauge that tracks per-variant utilization ratio (0.0-1.0).
	// Labels: variant_name, namespace, model_name, accelerator_type
	WVASaturationUtilization = "wva_saturation_utilization"

	// WVASpareCapacity is a gauge that tracks spare capacity; >0 means scale-down
	// headroom (per-role for P/D-disaggregated models, model-level otherwise).
	// The value is a token surplus, carrying unit="continuous".
	// Labels: variant_name, namespace, model_name, unit
	WVASpareCapacity = "wva_spare_capacity"

	// WVARequiredCapacity is a gauge that tracks required capacity; >0 means scale-up
	// needed (per-role for P/D-disaggregated models, model-level otherwise).
	// The value is a token demand, carrying unit="continuous".
	// Labels: variant_name, namespace, model_name, unit
	WVARequiredCapacity = "wva_required_capacity"

	// WVAKvCacheTokensUsed is a gauge that tracks total KV cache tokens currently in use per variant.
	// Labels: variant_name, namespace, model_name
	WVAKvCacheTokensUsed = "wva_kv_cache_tokens_used"

	// WVAKvCacheTokensCapacity is a gauge that tracks total KV cache token capacity per variant.
	// Labels: variant_name, namespace, model_name
	WVAKvCacheTokensCapacity = "wva_kv_cache_tokens_capacity"

	// WVASaturationMetricsUp is a per-VA freshness signal for the five
	// saturation/capacity gauges above. Set to 1.0 in cycles where the
	// optimizer produced a fresh decision for the variant (i.e. the other
	// gauges were just refreshed), and 0.0 in cycles where the analyzer was
	// aware of the variant but no fresh decision was emitted. Lets
	// dashboards distinguish "the system says utilization is X" from "the
	// system has not updated utilization in N minutes and X is the stalest
	// sample" without relying on Prometheus' 5-minute staleness marker.
	// Labels: variant_name, namespace
	WVASaturationMetricsUp = "wva_saturation_metrics_up"

	// WVAPodMappingMissTotal is a counter that tracks pods whose metrics could not be
	// attributed to a managed scaler: the walk up their ownerReferences ended
	// without reaching one. Makes the otherwise-silent skip visible.
	// Labels: namespace, reason
	WVAPodMappingMissTotal = "wva_pod_mapping_miss_total"

	// WVAScalerUndecidedAnswersTotal is a counter of GetMetrics answers the
	// KEDA external scaler gave WITHOUT a WVA decision behind them: the scale
	// target's current replica count, handed back so the HPA holds. A burst
	// after a restart or a lease hand-off is expected; a series that keeps
	// growing names a target WVA is asked about and never decides.
	// Labels: namespace, reason (ScalerUndecided* values)
	WVAScalerUndecidedAnswersTotal = "wva_scaler_undecided_answers_total"

	// WVAScalerUndecidedTargets is a gauge of ScaledObjects the KEDA external
	// scaler is CURRENTLY answering without a WVA decision. A ScaledObject
	// leaves it when a decided answer is given for it, when it is no longer
	// registered, or two minutes after its last undecided answer. Non-zero for
	// a cycle or two after a restart is expected; non-zero for minutes names a
	// target WVA is asked about and does not decide.
	// Labels: namespace
	WVAScalerUndecidedTargets = "wva_scaler_undecided_targets"

	// WVAScalerPanicsTotal is a counter of panics the KEDA external scaler's
	// gRPC recovery interceptors caught and answered with codes.Internal. Any
	// non-zero value is a bug; the stack is in the controller log.
	// Labels: method (the full gRPC method name)
	WVAScalerPanicsTotal = "wva_scaler_panics_total"

	// WVAUnmeasuredQueue is a gauge carrying the number of requests queued for a
	// model that WVA has NO attributed replica to act on.
	//
	// It separates two states the logs used to render identically. "No saturation
	// metrics available, skipping analysis" is the correct and quiet answer for a
	// model that is scaled to zero and idle. It is an emergency for a model that
	// is serving traffic through pods WVA cannot attribute — an FMA topology
	// whose launchers are unscraped, a PodMonitor that selects by a port the pods
	// do not declare, a workload whose ownerReferences reach no scale target.
	// Both produced the same line, so the second was invisible.
	//
	// Non-zero means: requests are queued, and the autoscaler is not going to do
	// anything about them. Sourced from the model-level scheduler flow-control
	// queue, which comes from EPP and is therefore independent of whether any
	// engine pod is scraped.
	//
	// Labels: namespace, model_name
	WVAUnmeasuredQueue = "wva_unmeasured_queue"

	// WVAVariantAtMaxReplicas is a gauge, 1 while a variant's target sits on its
	// own maxReplicas ceiling and 0 otherwise.
	//
	// Distinct from wva_decisions_limited_total, which means GPUs ran out. This
	// one means the operator's own ceiling is the binding constraint, and the
	// remedy is different: raise maxReplicas rather than add accelerators.
	// Conflating them would raise a spurious ResourceConstrained warning for a
	// variant that is simply honouring the bound it was given.
	//
	// Worth alerting on only in combination: at max AND utilization still above
	// the scale-up threshold means demand is going unserved. At max on its own
	// is the normal state of a variant sized exactly right.
	//
	// Kept as its own gauge rather than folded into WVAModelScalingBlocked, which
	// is the reason-labeled metric every other "why isn't this where it wants to
	// be" condition goes on. The two have different SCOPES: this one is per
	// variant, and the reason metric is per model precisely so it can be joined
	// against EPP series and so it can answer "can this model reach zero", a
	// question no single variant can. Adding variant_name there would break both.
	//
	// If a SECOND variant-scoped condition ever appears, convert this into
	// wva_variant_scaling_blocked{variant_name, reason} at that point — the
	// argument in docs/proposals/scale-from-zero-missing-signal.md §4 applies to
	// every scope, but a reason enum with one member is ceremony, not design.
	WVAVariantAtMaxReplicas = "wva_variant_at_max_replicas"

	// WVAModelScalingBlocked is a gauge, present and 1 for each reason a model
	// cannot reach the scaling state its configuration implies.
	//
	// Deliberately one metric with a `reason` label rather than a gauge per
	// condition. The enforcer alone refuses to park a model for three separate
	// reasons and logs each at V(DEBUG), so a metric per condition ends as a
	// metric per discovery — each with its own name, its own alert, and its own
	// entry in wvaMetricNames. This is the same shape wva_decisions_limited_total
	// already uses for `limited_by`, and it is what a status condition would be if
	// WVA owned an API object to write one on: it synthesizes VariantAutoscaling
	// from ScaledObjects, and a ScaledObject's status belongs to KEDA.
	//
	// Presence is the signal: a series exists only while its reason holds, so
	// every emission must clear the model's stale series first (see
	// SetModelScalingBlocked) or a resolved condition alerts forever. Several
	// reasons can hold at once, which emits several series — that is correct, and
	// more useful than picking a winner.
	//
	// Labels: namespace, model_name, reason
	WVAModelScalingBlocked = "wva_model_scaling_blocked"

	// WVAModelReplicas is a gauge carrying the total replicas currently serving a
	// model, summed across its variants.
	//
	// It exists to make one alert expressible. The failure an operator cares about
	// is "my model is parked and requests are being refused", whose two halves are
	// wva_current_replicas and EPP's llm_d_epp_request_error_total — and they
	// cannot be joined. wva_current_replicas is per VARIANT and carries
	// variant_name/namespace/accelerator_type with no model_name; EPP's counter
	// carries model_name and target_model_name, and only the namespace
	// Prometheus adds at scrape time (the router's, which the alert matches to
	// this series' exported_namespace). PromQL can
	// rewrite a label, not derive a model from a variant name, and nothing in
	// Prometheus knows that mapping. WVA is the one component that does.
	//
	// So this is the join key, not a convenience: model_name matches EPP's, and
	// the sum answers "is anything serving this model at all", which is the
	// question a parked model poses. Do NOT add accelerator_type or variant_name —
	// that would put the series back on the per-variant side of the join it exists
	// to bridge.
	//
	// Labels: namespace, model_name
	WVAModelReplicas = "wva_model_replicas"

	// WVAScaleFromZeroWakeSeconds is how long a wake-from-zero took: from the
	// first activation WVA published to the model being observed serving.
	//
	// The distribution is the point, not the average. On FMA the same code path
	// takes roughly 2s when it binds a reusable sleeping instance and roughly 50s
	// when it has to build one, so a bimodal histogram is what a working warm pool
	// looks like and a shift toward the slow mode is how its regression shows up.
	// Nothing else would report that: the wake still succeeds, just slowly.
	WVAScaleFromZeroWakeSeconds = "wva_scale_from_zero_wake_seconds"

	// WVAWarmPoolFreePods is how many warm-pool Pods can serve a wake right
	// now: every instance asleep, none loading. Read against the pool's
	// sleepMinSize, which is a floor on THIS number -- a Pod holding eight
	// sleeping models is one unit of reserve, not eight, because it can serve
	// one wake.
	//
	// Labels: pool
	WVAWarmPoolFreePods = "wva_warmpool_free_pods"

	// WVAWarmPoolBorrowTotal counts attempts to cover a scale-up from the pool,
	// by outcome: hit, blocked or miss.
	//
	// The outcomes name different faults and a hit rate alone shows neither. A
	// MISS means the model was resident nowhere, so the warm set is wrong. A
	// BLOCK means it was resident but every holding Pod was already serving, so
	// the pool is too small. One says raise the warm set, the other says add
	// PODS.
	//
	// Not "raise sleepMinSize", which is what this said and is the opposite of
	// the fix: the reserve is a floor on FREE Pods and admission draws on
	// free-minus-reserve, so raising it makes the pool warm LESS while the
	// blocked borrows continue.
	//
	// Labels: exported_namespace, model_name, outcome
	WVAWarmPoolBorrowTotal = "wva_warmpool_borrow_total"

	// WVAWarmPoolBridgeSeconds is how long a borrowed Pod served before the
	// ordinary replicas took over.
	//
	// This is the honest measure of what the pool is worth: it should track the
	// ordinary start time, measured at ~33-37s on this cluster. Sitting at
	// maxHoldSeconds instead means ordinary scale-ups are failing and the pool
	// is masking it -- which nothing else would report, because the requests
	// are being served.
	//
	// Labels: exported_namespace, model_name
	WVAWarmPoolBridgeSeconds = "wva_warmpool_bridge_seconds"
)

// Reasons a model cannot reach the scaling state its configuration implies
// (values for the `reason` label of WVAModelScalingBlocked).
//
// Each names a CONTRADICTION, not a setting. Scale-to-zero disabled alongside a
// variant floor is a consistent configuration and reports nothing; it is the half
// that permits zero while the other half forbids it that leaves an operator
// believing something the cluster will never do.
const (
	// ScalingBlockedVariantFloor indicates scale-to-zero is enabled for the model
	// but some variant declares minReplicas > 0. A model is at zero only when
	// nothing serves it, so one variant with a floor keeps the whole model up and
	// the scale-to-zero setting is inert — idle accelerators, billed indefinitely,
	// with no symptom to notice because the model is up and serving exactly as
	// every other metric says it should be.
	ScalingBlockedVariantFloor = "variant-floor"

	// ScalingBlockedPolicyForbidsZero indicates every variant permits zero but the
	// model's resolved policy disables scale-to-zero, so the bounds are inert.
	//
	// A valid configuration, reported because the operator's expectation is wrong
	// and nothing else will tell them. It matters most with a single variant,
	// where minReplicas: 0 reads exactly like a deliberate request to park.
	ScalingBlockedPolicyForbidsZero = "policy-forbids-zero"

	// ScalingBlockedEngineUnsupported indicates the model runs MORE THAN ONE
	// inference engine, so no single request counter measures its idleness.
	//
	// vLLM and SGLang are each supported alone: the enforcer asks for the counter
	// matching the detected engine (vllm:request_success_total or
	// sglang:num_requests_total). A model running both would need them summed, and
	// asking for one would see only part of its traffic — possibly parking a model
	// still serving through the other engine. That is refused rather than guessed.
	//
	// This reason previously meant "not vLLM", when the enforcer asked for the vLLM
	// counter whatever the model ran. It is now the narrower, genuinely ambiguous
	// case.
	ScalingBlockedEngineUnsupported = "engine-unsupported"

	// ScalingBlockedActivationRetention indicates the model was recently woken from
	// zero and is being held up for its retention period.
	//
	// TRANSIENT, unlike the other policy reasons: it clears itself once the hold
	// lapses, and it is not an error or a misconfiguration. It is reported because
	// it is otherwise indistinguishable from a model that simply will not park —
	// the enforcement path declines for several reasons and every one of them logs
	// only at V(DEBUG), so at the default verbosity an operator sees a model
	// sitting at one replica with no explanation anywhere.
	//
	// The hold exists because a just-woken model has served nothing yet: the
	// request that woke it is still queued in EPP while the pod pulls and loads,
	// so the idle counter reads zero for exactly the model that has demand waiting
	// on it. Without the hold the wake is undone before it can serve the request
	// that asked for it.
	//
	// Deliberately NOT alerted on — WVAModelWillNotScaleToZero matches only
	// variant-floor and policy-forbids-zero, which are standing configuration
	// contradictions rather than a few minutes of expected behaviour.
	ScalingBlockedActivationRetention = "activation-retention"

	// ScalingBlockedNoWakeSignal indicates EPP exports no flow-control queue at
	// all, so a model parked at zero has nothing that can wake it.
	//
	// Absence of the family, not a queue reading zero — an idle queue reports 0
	// and a queue that does not exist reports nothing, and conflating those is how
	// this failure hid for a week. The usual cause is an EPP pod older than the
	// ConfigMap that enabled flowControl: EPP reads --config-file once at startup.
	ScalingBlockedNoWakeSignal = "no-wake-signal"

	// ScalingBlockedNoCompositeSignal indicates the model's composite analyzer
	// result (every variant on decision path C4-no-signal) carries
	// no usable capacity signal this cycle: no analyzer — saturation included —
	// produced an informative result. The model was not usefully measured, so no
	// decision was made for it; this is that condition surfaced on the existing
	// dashboard rather than only in a log line.
	//
	// Distinct from the policy reasons above: those name a standing
	// configuration contradiction about scale-to-zero specifically, while this
	// names an absence of evidence that blocks ANY decision, scale-to-zero
	// included. Owned separately (ScalingBlockedReasonsSignal) because it is
	// decided at composite-collection time, once per cycle per model, not by
	// the steady-state enforcer.
	ScalingBlockedNoCompositeSignal = "no-composite-signal"
)

// Reason ownership. Two engines write WVAModelScalingBlocked — the steady-state
// enforcer and the scale-from-zero loop — so each declares the reasons it owns
// and clears only those. Without this a 10Hz producer and a per-interval producer
// would take turns deleting each other's series.
var (
	// ScalingBlockedReasonsPolicy are decided by the steady-state enforcer, from
	// configuration reconciled against discovered replica bounds.
	ScalingBlockedReasonsPolicy = []string{
		ScalingBlockedVariantFloor,
		ScalingBlockedPolicyForbidsZero,
		ScalingBlockedEngineUnsupported,
		ScalingBlockedActivationRetention,
	}

	// ScalingBlockedReasonsWake are decided by the scale-from-zero loop, from
	// what EPP actually exports.
	ScalingBlockedReasonsWake = []string{
		ScalingBlockedNoWakeSignal,
	}

	// ScalingBlockedReasonsSignal are decided at composite-collection time, from
	// whether the model's composite analyzer result carries a usable signal.
	ScalingBlockedReasonsSignal = []string{
		ScalingBlockedNoCompositeSignal,
	}

	// ScalingBlockedReasonsUtilizationShare are decided by the
	// utilization-share optimizer while it acts
	// (docs/proposals/utilization-share-optimizer.md, section 9).
	ScalingBlockedReasonsUtilizationShare = []string{
		ScalingBlockedAwaitingRelease,
		ScalingBlockedQuotaShort,
		ScalingBlockedFloorPinned,
		ScalingBlockedFloorsExceedQuota,
		ScalingBlockedDonorsAtFloor,
		ScalingBlockedNoCompatibleDonor,
		ScalingBlockedNoNodeFits,
		ScalingBlockedReleaseTimeout,
		ScalingBlockedReleaseTaken,
		ScalingBlockedReleaseShapeMismatch,
		ScalingBlockedReversalHold,
		ScalingBlockedSwinging,
		ScalingBlockedTransferLimit,
		ScalingBlockedBurstEnding,
		ScalingBlockedFastSwingCapped,
		ScalingBlockedDonorNotSteerable,
		ScalingBlockedDonorChanging,
		ScalingBlockedQuietPeriod,
		ScalingBlockedMarksUnreadable,
		ScalingBlockedWholeReplicaShort,
	}
)

// Utilization-share reasons (values for the `reason` label of
// WVAModelScalingBlocked). Each is set on the model of the role it describes.
const (
	// ScalingBlockedQuietPeriod indicates the group is in the quiet period
	// that follows a controller restart, a leader change, or the optimizer
	// starting to act: every planned model holds what it runs for one fill
	// timeout, so none scales, up or down.
	ScalingBlockedQuietPeriod = "quiet-period"
	// ScalingBlockedMarksUnreadable indicates the controller cannot read the
	// transfer marks on the group's donor pods, so it cannot start the group's
	// ledger: every planned model holds what it runs until it can. Unlike the
	// quiet period it does not clear by itself -- the Error log line
	// "could not read transfer marks" names the cause.
	ScalingBlockedMarksUnreadable = "marks-unreadable"
	// ScalingBlockedWholeReplicaShort indicates the role holds less than its
	// need although the needs add up to less than the quota: whole replicas
	// cannot give every role its need (each need rounded up to its replica
	// size does not fit), so moves go to whichever role is worse off.
	ScalingBlockedWholeReplicaShort = "whole-replica-short"
	// ScalingBlockedAwaitingRelease indicates the role receives a transfer
	// whose donor has not released its GPUs yet.
	ScalingBlockedAwaitingRelease = "awaiting-release"
	// ScalingBlockedQuotaShort indicates the role holds less than its need and
	// the group as a whole is short, so no rebalance can cover it.
	ScalingBlockedQuotaShort = "quota-short"
	// ScalingBlockedFloorPinned indicates the role's floor holds at least a
	// replica more than its need: GPUs the share would otherwise give others.
	ScalingBlockedFloorPinned = "floor-pinned"
	// ScalingBlockedFloorsExceedQuota indicates the floors of the group's
	// roles add up to more than its budget.
	ScalingBlockedFloorsExceedQuota = "floors-exceed-quota"
	// ScalingBlockedDonorsAtFloor indicates the role is short and out of band,
	// and no other role is above its whole-replica target and floor to give.
	ScalingBlockedDonorsAtFloor = "donors-at-floor"
	// ScalingBlockedNoCompatibleDonor indicates the role is short and out of
	// band, and no donor replica's pods can host one of its replicas
	// (section 6.5).
	ScalingBlockedNoCompatibleDonor = "no-compatible-donor"
	// ScalingBlockedNoNodeFits indicates the role is short and no node can
	// hold one of its replicas, even if every pod the group's other models
	// could give on that node went: pods the scaling manager does not plan
	// fill the rest of every node. No transfer or return can place it, so
	// none is planned. While it holds, the donor reasons (donors-at-floor,
	// no-compatible-donor) and the receive reasons (reversal-hold,
	// transfer-limit, burst-ending) are not reported for the role: they
	// change as the other models give or keep their spare, and this cause
	// does not. Needs node information.
	ScalingBlockedNoNodeFits = "no-node-fits"
	// ScalingBlockedReleaseTimeout indicates the role's last release was
	// aborted, and it is backing off before it is asked to give again.
	ScalingBlockedReleaseTimeout = "release-timeout"
	// ScalingBlockedReleaseTaken indicates the role's last transfer timed out
	// filling, and the GPUs its donors released are no longer free: a pod WVA
	// did not place took them. It needs node information.
	ScalingBlockedReleaseTaken = "release-taken"
	// ScalingBlockedReleaseShapeMismatch indicates the role's last transfer
	// timed out filling with enough GPUs free in total, but on no node enough
	// for the role's largest pod. It needs node information.
	ScalingBlockedReleaseShapeMismatch = "release-shape-mismatch"
	// ScalingBlockedReversalHold indicates the role is short but gave GPUs
	// within the reversal hold, so it may not receive yet (section 6.7 rule 4).
	ScalingBlockedReversalHold = "reversal-hold"
	// ScalingBlockedSwinging indicates the role reversed direction
	// stabilization.swingReversals times (three by default) within the swing
	// window and is planned on its mean need, not its current one (section
	// 6.7 rule 5).
	ScalingBlockedSwinging = "swinging"
	// ScalingBlockedTransferLimit indicates the role is short while its group
	// already has the most transfers in flight it may run at once.
	ScalingBlockedTransferLimit = "transfer-limit"
	// ScalingBlockedBurstEnding indicates the role is short with a hard
	// imbalance confirmed, but its model's burst is expected to end before a
	// move would land and serve, so it receives at the pace instead of its
	// whole shortfall at once.
	ScalingBlockedBurstEnding = "burst-ending"
	// ScalingBlockedFastSwingCapped indicates the role's need swings faster
	// than a move can follow while another model bursts for longer than one,
	// so its headroom is capped on its mean need, below its current reading.
	ScalingBlockedFastSwingCapped = "fast-swing-capped"
	// ScalingBlockedDonorNotSteerable indicates the role's pods could not be
	// marked when it was asked to give (one was not Ready or not scheduled, or
	// the patch failed), and it is backing off before it is asked again.
	ScalingBlockedDonorNotSteerable = "donor-not-steerable"
	// ScalingBlockedDonorChanging indicates a donor is held because its
	// workload is changing -- a rollout, or a pod not yet created, scheduled
	// or Ready -- so the pod a lower count removes cannot be steered. Set on
	// the held donor while it is not short itself, and on each role whose
	// transfer from it was refused, while that role stays short, is not
	// receiving, and no reversal-hold or transfer-limit holds it. The hold
	// ends once the donor's workload reads stable for
	// stabilization.consecutiveCyclesBeforeMove cycles, or after one release
	// timeout at the latest. Routine, not a fault: no back-off follows.
	ScalingBlockedDonorChanging = "donor-changing"
)

// Kueue cohort borrowing reasons (values for the `reason` label of
// WVAModelScalingBlocked), decided by the steady-state engine each cycle
// (the Kueue borrowing proposal, PR #161, Part C). They
// answer "why did my model drop": Kueue preempted a replica, or a replica did
// not place and the scaling manager released it. A model that dropped with
// none of them present dropped because its demand fell.
const (
	// ScalingBlockedLenderReclaimed indicates Kueue preempted a pod of the
	// model for a lender reclaiming its nominal quota. Present for the borrow
	// cooldown after the preemption, while the model borrows nothing new.
	ScalingBlockedLenderReclaimed = "lender-reclaimed"
	// ScalingBlockedBorrowUnplaced indicates a variant of the model has
	// replicas that did not place: it grows on no borrowed capacity, and its
	// borrowed replicas still unplaced after the placement timeout are
	// released.
	ScalingBlockedBorrowUnplaced = "borrow-unplaced"
	// ScalingBlockedNominalLentOut indicates the model was capped this cycle
	// while part of its namespace's nominal quota is held by a peer its
	// ClusterQueue cannot reclaim from (reclaimWithinCohort: Never), so its
	// guaranteed figure sits below nominal. A parked model refused a wake for
	// the same cause carries ScalingBlockedWakeNominalLentOut instead.
	ScalingBlockedNominalLentOut = "nominal-lent-out"
	// ScalingBlockedReleasedToLender indicates the scaling manager lowered
	// the model to hand borrowed capacity back to a waiting lender Kueue does
	// not reclaim for (kueue.releaseToNonReclaimingLenders). Present while
	// the release is in flight, until it lands.
	ScalingBlockedReleasedToLender = "released-to-lender"
)

// Scale-from-zero refusal reasons (values for the `reason` label of
// WVAModelScalingBlocked): a model at zero with requests queued was not woken
// because the limiters left no room for it. Set every tick the wake is
// refused, and cleared once it wakes, is refused for another reason, or its
// queue empties.
const (
	// ScalingBlockedWakeNoCapacity indicates a wake was refused because no
	// serving set fit the GPU budget or the nodes (the scale-from-zero
	// outcomes no-capacity and prefill-required). The log line "Scale-from-zero:
	// no variant woken for a model with pending requests" names the budgets it
	// was measured against and the demand.
	ScalingBlockedWakeNoCapacity = "wake-no-capacity"
	// ScalingBlockedWakeNominalLentOut is ScalingBlockedWakeNoCapacity in a
	// namespace whose ClusterQueue lent part of its nominal quota to a peer it
	// cannot reclaim from: the wake was checked against its guaranteed figure
	// plus the cohort's free pool, not its whole nominal.
	ScalingBlockedWakeNominalLentOut = "wake-nominal-lent-out"
)

// ScalingBlockedReasonsBorrowing are decided by the steady-state engine from
// cohort borrowing, every cycle; with no borrowing counted, none holds.
var ScalingBlockedReasonsBorrowing = []string{
	ScalingBlockedLenderReclaimed,
	ScalingBlockedBorrowUnplaced,
	ScalingBlockedNominalLentOut,
	ScalingBlockedReleasedToLender,
}

// ScalingBlockedReasonsWakeCapacity are decided by the scale-from-zero loop
// when the limiters refuse a wake. They are its own, not the steady-state
// engine's nominal-lent-out: two producers sharing one reason delete each
// other's series.
var ScalingBlockedReasonsWakeCapacity = []string{
	ScalingBlockedWakeNoCapacity,
	ScalingBlockedWakeNominalLentOut,
}

// ScalingBlockedReasonOwners is every producer's reason set, by producer. No
// reason may appear in two of them: SetModelScalingBlockedReasons deletes
// every reason its caller owns that does not hold.
var ScalingBlockedReasonOwners = map[string][]string{
	"policy":            ScalingBlockedReasonsPolicy,
	"wake":              ScalingBlockedReasonsWake,
	"wake-capacity":     ScalingBlockedReasonsWakeCapacity,
	"utilization-share": ScalingBlockedReasonsUtilizationShare,
	"borrowing":         ScalingBlockedReasonsBorrowing,
}

// Kueue cohort borrowing metrics
// (the Kueue borrowing proposal, PR #161, Part C). Every
// other borrowing condition is a reason on WVAModelScalingBlocked.
const (
	// WVAQuotaLimitGPUs is a gauge: a namespace-scope quota entry's cap for one
	// namespace and accelerator, by tier. tier="guaranteed" is the cap without
	// borrowing; tier="borrowable" is what the namespace may draw past it now,
	// were no other namespace to draw first -- min(its borrow cap, the free
	// GPUs of its cohorts' shared pools serving the type) -- and 0 where
	// borrowing is not counted. An unlimited guaranteed figure publishes no
	// series. Labels: limiter_name, namespace, accelerator, tier.
	WVAQuotaLimitGPUs = "wva_quota_limit_gpus"

	// WVAQuotaBorrowWithheld is a gauge, 1 for the reason a namespace's
	// borrowable figure is 0, for an entry that reads Kueue borrowing. Present
	// only while that reason holds: a namespace that borrows, or whose reason
	// changed, has its old series deleted. Labels: limiter_name, namespace,
	// accelerator, reason.
	WVAQuotaBorrowWithheld = "wva_quota_borrow_withheld"

	// WVAKueueUnplacedPods is a gauge: per variant in a namespace that counts
	// borrowing, the pods that have not placed for longer than the placement
	// timeout (Unschedulable, or held at Kueue's admission gate). Absent
	// otherwise. Labels: namespace, variant_name.
	WVAKueueUnplacedPods = "wva_kueue_unplaced_pods"

	// WVAKueuePreemptedPodsTotal is a counter of preempted Workloads of a
	// model: Kueue preemptions (a Workload evicted with reason Preempted) in a
	// namespace that counts borrowing, one per Workload, so a LeaderWorkerSet
	// or pod group counts once. The name predates that and is kept for the
	// alert and dashboards that read it. Labels: namespace, model_name.
	WVAKueuePreemptedPodsTotal = "wva_kueue_preempted_pods_total"

	// WVABorrowReleasesTotal is a counter of the variant targets the scaling
	// manager lowered to give borrowed Kueue capacity back, one per release
	// record (a P/D unit counts once per variant it lowers). The namespace is
	// the model's, scraped as exported_namespace. Labels: namespace,
	// model_name, cause (placement-timeout: a borrowed replica did not place
	// in time).
	WVABorrowReleasesTotal = "wva_borrow_releases_total"

	// WVAKueueLenderWaitingSeconds is a gauge: how long a lender ClusterQueue
	// has waited for capacity it lent (debounced), published only while some
	// namespace this controller governs still borrows in the lender's cohort.
	// Above the borrow cooldown it means Kueue has not taken back what the
	// lender lent. Labels: cohort, cluster_queue.
	WVAKueueLenderWaitingSeconds = "wva_kueue_lender_waiting_seconds"

	// WVAKueueBorrowDriftGPUs is a gauge, the conformance check of the
	// borrowing figures: per ClusterQueue in a cohort, the GPUs by which the
	// borrowed figure computed from its reservation (usage past nominal, per
	// flavor and GPU resource) differs from Kueue's own
	// status.flavorsReservation[].borrowed, summed as absolute differences. 0
	// while Kueue and the scaling manager agree. Labels: cluster_queue.
	WVAKueueBorrowDriftGPUs = "wva_kueue_borrow_drift_gpus"

	// WVAKueueNeverRead is a gauge, 1 for a quota entry that reads Kueue,
	// leaves some accelerator type unlimited (-1) in its static caps, and has
	// never read Kueue successfully since the controller started: nothing
	// bounds that type. Absent otherwise. Labels: limiter_name.
	WVAKueueNeverRead = "wva_kueue_never_read"

	// WVAKueueControllerVersionKnown is a gauge, 1 when the Kueue controller's
	// Deployment was read and its image names a version, 0 when not (a
	// digest-only image, or a Deployment that cannot be read). Published on
	// each Kueue read that checks the controller (a cluster with a cohort).
	// While 0, borrowing is withheld (kueue-version-unknown, a digest-only
	// image) or frozen (kueue-controller-down, a Deployment that cannot be
	// read). No labels beyond the controller instance.
	WVAKueueControllerVersionKnown = "wva_kueue_controller_version_known"

	// WVAKueueBorrowCooldownSeconds is a gauge: the borrowCooldown in force,
	// in seconds, while a quota entry counts Kueue borrowing; the threshold
	// the lender-waiting alert compares against. Absent otherwise.
	WVAKueueBorrowCooldownSeconds = "wva_kueue_borrow_cooldown_seconds"

	// WVAKueueBorrowReleaseLatencySeconds is a gauge, per cohort whose pool
	// opts in to kueue.releaseToNonReclaimingLenders: how long a release of
	// borrowed capacity to a waiting lender there takes to land -- the
	// slowest KEDA scale-down window of the models a release may lower, plus
	// the HPA sync, the KEDA poll and the termination grace, capped at 30
	// minutes. The lender-waiting alert adds it to the cooldown for that
	// cohort alone. Absent for every other cohort.
	WVAKueueBorrowReleaseLatencySeconds = "wva_kueue_borrow_release_latency_seconds"

	// WVAKueueLenderReleaseBlocked is a gauge, 1 per cohort, lender
	// ClusterQueue and reason while a waiting lender Kueue does not reclaim
	// for is handed nothing (kueue.releaseToNonReclaimingLenders): reason
	// unservable (what this controller could hand back would not admit its
	// next Workload), taken-by-another-borrower (a release landed and
	// another borrower took it) or kueue-preempting (Workloads Kueue
	// preempted still hold their quota while their pods stop). Absent
	// otherwise.
	WVAKueueLenderReleaseBlocked = "wva_kueue_lender_release_blocked"

	// WVAKueueBorrowLedgerUnread is a gauge, 1 with a reason while the
	// persisted borrowing release ledger cannot be read and some quota
	// entry sets kueue.releaseToNonReclaimingLenders: no release to a lender
	// is decided until it is. Reasons: forbidden, read-error, newer-version.
	WVAKueueBorrowLedgerUnread = "wva_kueue_borrow_ledger_unread"

	// WVAKueueBorrowedGPUs is a gauge: per cohort and flavor, the GPUs the
	// namespaces this controller governs hold past their guaranteed figures,
	// as the last cycle counted them. Absent where nothing is borrowed.
	// Labels: cohort, flavor.
	WVAKueueBorrowedGPUs = "wva_kueue_borrowed_gpus"

	// LabelFlavor is a Kueue ResourceFlavor name.
	LabelFlavor = "flavor"

	// LabelTier is the tier of WVAQuotaLimitGPUs: guaranteed or borrowable.
	LabelTier = "tier"
	// LabelCause is why borrowed capacity was released, on
	// WVABorrowReleasesTotal.
	LabelCause = "cause"
	// LabelCohort is a Kueue cohort name.
	LabelCohort = "cohort"
	// LabelClusterQueue is a Kueue ClusterQueue name.
	LabelClusterQueue = "cluster_queue"

	// QuotaTierGuaranteed and QuotaTierBorrowable are the values of LabelTier.
	QuotaTierGuaranteed = "guaranteed"
	QuotaTierBorrowable = "borrowable"
)

// Pod-mapping miss reasons (values for the `reason` label of WVAPodMappingMissTotal).
const (
	// PodMappingMissUnresolved indicates a scraped pod resolved to no managed scaler:
	// the locator found no owning ScaledObject above it. A pod owned by something
	// else entirely — an FMA launcher, owned by a LauncherConfig — counts here.
	PodMappingMissUnresolved = "unresolved"

	// PodMappingMissUnboundLauncher indicates an FMA server-providing pod with no
	// current binding: a launcher carrying no dual-pods pairing label.
	//
	// Expected and benign. An unbound launcher is a warm spare, serving nothing,
	// and declining to attribute it is correct. It is separated from
	// "unresolved" so a pool of spares — which can be large, and is permanent —
	// does not bury a genuine attribution bug in the same counter.
	//
	// Rarely seen where the shipped PodMonitor is in use, and that is by design:
	// classification happens only for pods that were scraped, and that monitor
	// drops launchers with no bound instance before a target is generated. This
	// reason therefore reports the case where something ELSE scrapes launchers —
	// a monitor with no such rule, or one inherited from another guide — which is
	// exactly when a warm spare could otherwise be mistaken for idle capacity.
	PodMappingMissUnboundLauncher = "unbound_launcher"

	// PodMappingMissPairingUnresolved indicates a launcher that DID declare a
	// pairing, whose partner still did not resolve to a managed scaler.
	//
	// Unlike the reason above this one is worth investigating: the partner pod
	// may be gone, its ownerReferences may reach no ScaledObject, or the two
	// halves may disagree on the model they serve.
	//
	// The proposal called this `foreign_model_instance`, after the third case.
	// Renamed because the collector cannot tell the three apart from here, and
	// naming one of them would repeat the mistake this investigation started
	// with — a log line that asserted "possible pod/pod_name label mismatch" for
	// a condition with several possible causes, and sent readers hunting the
	// wrong one.
	PodMappingMissPairingUnresolved = "pairing_unresolved"

	// PodMappingMissOtherModelVariant indicates a pod that resolved to a real
	// managed scaler which this model's pass does not own.
	//
	// The pass selects series by model_name, so the series says one model while
	// the pod's current pairing leads to another model's variant. An FMA launcher
	// rebound from one model to another produces exactly this: the pod IP and
	// port stay the same, FMA repoints the pairing and model labels at the new
	// requester, and samples from before the rebind are still inside the query
	// window.
	//
	// It is not an error. The row is ignored rather than charged to a variant it
	// does not belong to, which is the safe direction. It is counted because the
	// alternative is silence: the pairing DID resolve, so `pairing_unresolved`
	// never fires, and a rebind would otherwise leave no trace at all while the
	// old model reads slightly small for one query window.
	//
	// Sustained counts mean something else: a launcher whose pairing points at a
	// variant of a model it does not serve, which is the multi-instance case the
	// singular pairing label cannot express.
	PodMappingMissOtherModelVariant = "other_model_variant"
)

// Undecided-answer reasons (values for the `reason` label of
// WVAScalerUndecidedAnswersTotal).
const (
	// ScalerUndecidedNoDecision indicates a target WVA manages and has not
	// decided yet -- normally the first cycle after a restart or a lease
	// hand-off. Expected to stop growing within a cycle or two.
	ScalerUndecidedNoDecision = "no-decision"

	// ScalerUndecidedNotManaged indicates a target WVA will never decide: its
	// trigger metadata does not parse (registry.ParseMeta rejects it), so
	// every engine skips the workload. It holds at its current count until
	// the ScaledObject is fixed.
	ScalerUndecidedNotManaged = "not-managed"

	// ScalerUndecidedStale indicates a target whose last positive decision is
	// older than the scaler's decision TTL (at least 60 s, and at least three
	// optimize intervals): no engine is republishing it. The scaler answers
	// the current count, as IsActive already does for such a decision.
	ScalerUndecidedStale = "stale"
)

// Metric Label Names
// Common label names used across metrics for consistency.
const (
	LabelModelName          = "model_name"
	LabelNamespace          = "namespace"
	LabelComponent          = "component"
	LabelVariantName        = "variant_name"
	LabelDirection          = "direction"
	LabelReason             = "reason"
	LabelMethod             = "method"
	LabelAcceleratorVendor  = "accelerator_vendor"
	LabelAcceleratorModel   = "accelerator_model"
	LabelAcceleratorType    = "accelerator_type"
	LabelControllerInstance = "controller_instance"
	LabelProvider           = "provider"
	LabelRole               = "role"
	LabelStatus             = "status"
	LabelLimiterName        = "limiter_name"
	LabelPolicyType         = "policy_type"
	LabelOptimizerName      = "optimizer_name"
	LabelPool               = "pool"
	// LabelOutcome distinguishes a borrow that hit, one blocked by an exhausted
	// reserve, and one that missed because the model was resident nowhere.
	LabelOutcome = "outcome"
	// LabelQuantity names what a variant borrowed from a sibling (k2 or
	// itl-line), and LabelDonor the sibling it borrowed from.
	LabelQuantity = "quantity"
	LabelDonor    = "donor"
	// LabelDonorNamespace is the donor variant's namespace.
	LabelDonorNamespace = "donor_namespace"
	// LabelOperation names the operation a store error happened in.
	LabelOperation          = "operation"
	LabelErrorType          = "error_type"
	LabelAnalyzerName       = "analyzer_name"
	LabelLimiterEnabled     = "limiter_enabled"
	LabelScaleToZeroEnabled = "scale_to_zero_enabled"
	LabelQueryType          = "query_type"
	// LabelUnit names the unit of a metric value. Applied to
	// wva_required_capacity and wva_spare_capacity, whose values are a
	// "continuous" token magnitude. It is retained as a stable part of those
	// series' identity so existing queries and alerts keep matching.
	LabelUnit = "unit"
)

// Metric Label Values for query_type
// These values are used as the query_type label in metrics collection metrics.
const (
	QueryTypeKVCache      = "kv_cache"
	QueryTypeQueueLength  = "queue_length"
	QueryTypeRequestCount = "request_count"
	QueryTypeCacheConfig  = "cache_config"
	QueryTypeArrivalRate  = "arrival_rate"
	// QueryTypePodList is the collector's per-namespace Pod listing, which
	// supplies readiness and deletion state. Not a Prometheus query: a failure
	// is a Kubernetes API error (reason forbidden, unavailable or other), and
	// the collector then treats every Pod as Ready and present for the cycle.
	QueryTypePodList = "pod_list"
	// QueryTypeRouterLatency is the router TTFT/TPOT read by the slo analyzer.
	QueryTypeRouterLatency = "router_latency"
)

// Value for the LabelUnit Prometheus label: the metric carries an absolute
// quantity rather than a normalized ratio.
const (
	UnitContinuous = "continuous"
)

// EngineConfigFlagLabels are the engine-flag labels on WVAEngineConfig, in the
// order their values arrive from EngineParams.FingerprintValues.
//
// It lives here, in a leaf package, so that capacity (which pairs values to
// it) and metrics (which publishes it) read one list instead of two that can
// drift. The order is POSITIONAL against FingerprintValues, and
// TestFingerprintLabelsDescribeTheirValues pins that pairing.
//
// REORDERING THIS DOES NOT CHANGE ANY DIGEST, and an earlier version of this
// comment said it did -- which would have sent a maintainer to bump
// FingerprintVersion, flushing every learned key on the cluster, for a pure
// relabel. Fingerprint() hashes its own `name=` list in fingerprint.go and
// deliberately does not read these names, exactly so that relabelling the
// metric cannot reshuffle stored digests. Getting this list wrong mislabels
// the metric; it does not mis-key anything.
var EngineConfigFlagLabels = []string{
	"engine",
	"weight_dtype",
	"quantization",
	"gpu_memory_utilization",
	"block_size",
	"kv_cache_dtype",
	"tensor_parallel_size",
	"num_gpu_blocks_override",
	"total_kv_tokens_override",
	"effective_max_batched_tokens",
	"max_num_seqs",
	// vLLM's --max-num-active-seqs; 0 when unset or unreadable.
	"max_num_active_seqs",
	"max_model_len",
	"enforce_eager",
	// Which of the labels above are the engine's real settings and which
	// are defaults that replaced something this controller could not read.
	// Empty is the good case. Published because the gap is otherwise
	// invisible: a block_size of 16 reads identically whether the engine
	// runs 16 or the flag was "$VLLM_BLOCK_SIZE" and nothing resolved it.
	"unresolved",
	// The engine container's image: the engine's code, which no flag
	// describes. One value per configuration, so no cardinality beyond the
	// configurations themselves.
	"image",
	"data_parallel_size",
	"pipeline_parallel_size",
	"expert_parallel",
	"speculative_config",
	"kv_connector",
	"kv_offloading_size",
	"kv_offloading_backend",
	"prefix_caching_disabled",
	// The checkpoint served, and whether it could be read: a model given as
	// an unresolved variable is not used as a key for sharing.
	"model",
	"model_revision",
	"model_unresolved",
	// Flags read for identity whose value could not be interpreted, as
	// written: a parallel size computed at start-up. A variant with anything
	// here is not used as a key for sharing a latency line.
	"sharing_unresolved",
	// Flags that size or reshape what one replica holds and that no label
	// above names (--kv-cache-memory-bytes, redundant experts, the expert
	// all-to-all backend, SGLang's DP attention), as sorted key=value pairs.
	"memory_layout",
}
