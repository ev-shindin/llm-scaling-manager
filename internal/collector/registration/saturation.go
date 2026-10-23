package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// Query name constants for type-safe query references.
const (
	// Saturation queries (per-pod peak metrics over time windows)
	QueryKvCacheUsage = "kv_cache_usage"
	QueryQueueLength  = "queue_length"
	// QueryMaxRankKvCacheUsage and QueryMaxRankQueueLength are the busiest
	// engine rank's figures, beside the pod's mean and sum above.
	QueryMaxRankKvCacheUsage = "max_rank_kv_cache_usage"
	QueryMaxRankQueueLength  = "max_rank_queue_length"

	// V2 queries (token-based capacity analysis)
	QueryCacheConfigInfo = "cache_config_info"
	QueryAvgOutputTokens = "avg_output_tokens"
	// QueryAvgOutputTokensRecent is QueryAvgOutputTokens over a short window.
	// See domain.ReplicaMetrics.AvgOutputTokensRecent for why both exist.
	QueryAvgOutputTokensRecent = "avg_output_tokens_recent"
	QueryAvgInputTokens        = "avg_input_tokens"
	// QueryAvgInputTokensRecent is QueryAvgInputTokens over the same short
	// window as QueryAvgOutputTokensRecent, and is read only together with it.
	// See domain.ReplicaMetrics.AvgInputTokensRecent.
	QueryAvgInputTokensRecent = "avg_input_tokens_recent"
	QueryPrefixCacheHitRate   = "prefix_cache_hit_rate"

	// Scheduler flow control queries (model-level, from inference scheduler)
	QuerySchedulerQueueSize  = "scheduler_queue_size"
	QuerySchedulerQueueBytes = "scheduler_queue_bytes"
)

// Per-replica engine queries are registered NAMESPACE-scoped, not model-scoped.
//
// They carry no model_name matcher; instead model_name is part of the grouping
// key, so one execution per namespace returns the series for every model in it
// and the collector partitions them by model_name in Go (see
// collector.filterResultsToModel). Previously each of these ran once per model
// per cycle over an identical namespace filter, so a namespace hosting M models
// paid M times the round trips to fetch the same set of series.
//
// Consequences to preserve when editing a template below:
//
//   - model_name MUST appear in the by()/grouping clause of any query the
//     collector partitions by model, or every model's slice comes back empty.
//   - A series with no model_name label cannot be attributed to a model and is
//     dropped by the collector — exactly what the removed matcher did.
//   - The rest of the grouping key is instance and pod: instance is IP:port,
//     which distinguishes DP ranks sharing a pod, and pod drives the
//     ownerReference walk that resolves the variant.
//   - engine is NOT in the grouping, and the aggregation that drops it is
//     the merge rule for data-parallel ranks (issue #150). vLLM running
//     several DP ranks behind one API server exports every series once per
//     rank, told apart only by the engine label, from one scrape target. Each
//     query therefore folds the ranks the way collector.collapseToPods folds
//     ranks that run their own API servers, so the two layouts give one
//     answer for the same ranks:
//       - extensive gauges (the waiting queue) are SUMMED over the ranks,
//         after a max per rank so a target scraped twice is not counted twice;
//       - utilization (kv_cache_usage_perc) is AVERAGED, after a max per rank
//         for the same reason: every rank of a pod runs the same model on the
//         same GPUs, so the mean of the ranks' fractions is used tokens over
//         total capacity;
//       - the busiest rank's queue and utilization are read as well, by a max
//         over the ranks (QueryMaxRankQueueLength, QueryMaxRankKvCacheUsage):
//         the sum and the mean price the pod, and the busiest rank says
//         whether one engine of it is saturated;
//       - per-request and per-token means (token lengths, ITL, TTFT, service
//         time, prefix hit rate) are sum(rate(_sum)) / sum(rate(_count)),
//         which weights each rank by its own traffic and leaves an idle rank
//         out;
//       - cache_config_info is COUNTED: one series per rank, so the count is
//         the ranks the instance serves. Its num_gpu_blocks label is already
//         the total over those ranks (the API server sums its engines'
//         blocks), so it is read as is and never summed over the ranks.
//     Taking a max over the ranks instead, as these queries did, priced the
//     pod at its busiest rank: utilization and ITL of the hottest rank
//     against the whole pod's capacity, and the queue of one rank.
//     On an instance with one rank every one of these returns exactly what
//     the max returned.
//   - llm_d_ai_variant is deliberately NOT in the grouping (issue #1263).
//     Neither vLLM nor SGLang emits it — it appears only where an operator
//     relabels the llm-d.ai/variant pod label onto the series — so it was an
//     empty column on every real deployment. Variant identity now comes from
//     the PodLocator owner-walk delivered by PR #1260.

// RegisterSaturationQueries registers queries used by the saturation analyzer.
func RegisterSaturationQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// KV cache usage per instance (peak over last minute)
	// Uses max_over_time to catch saturation events between scrapes, per rank,
	// then the mean over the instance's ranks (see the note above
	// RegisterSaturationQueries for the grouping key, namespace scoping and
	// the DP rank rules). The max per rank first, as for the queue: a rank
	// whose gauge has copies counts once. A target scraped twice has two.
	// An older vLLM run with --api-server-count above one can export the
	// gauge in prometheus_client's multiprocess "all" mode: one copy per API
	// server process, told apart by a pid label, and a process that never
	// received the rank's stats holds 0. A plain mean over such copies reads
	// 0.375 for a pod whose two ranks are both at 0.75.
	//
	// A per-pod peak is right for the saturation tests and for k2, and wrong
	// summed as demand when the router moves its hot pod within the minute:
	// every pod it visited keeps a full peak. The saturation analyzer prices
	// such a pod -- idle now beside a sibling busy now -- from
	// QueryKvUsageInstant instead (residentCharge); this query is unchanged.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKvCacheUsage,
		Type:        source.QueryTypePromQL,
		Template:    `avg by (model_name, instance, pod) (max by (model_name, instance, pod, engine) (max_over_time(vllm:kv_cache_usage_perc{namespace="{{.namespace}}"}[1m])))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak KV cache utilization per instance (0.0-1.0) over last minute",
	})

	// Queue length per instance (peak over last minute)
	// Uses max_over_time to catch burst traffic, per rank, then the sum over
	// the instance's ranks: a request waiting on any rank waits on the pod
	// Grouping key, namespace scoping, DP ranks: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryQueueLength,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (max by (model_name, instance, pod, engine) (max_over_time(vllm:num_requests_waiting{namespace="{{.namespace}}"}[1m])))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak queue length per instance over last minute",
	})

	// The busiest rank's KV cache utilization and queue: the largest one-
	// minute peak over the instance's ranks. The two queries above give what
	// the pod holds; these say whether any one engine of it is full and
	// queued, which is what the saturation guards ask (see
	// domain.ReplicaMetrics.MaxRankQueueLength). A max is also idempotent
	// over copies of a series, so a second scrape or a pid copy cannot move
	// it. On an instance with one rank each is the query above.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryMaxRankKvCacheUsage,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(vllm:kv_cache_usage_perc{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak KV cache utilization of the busiest engine rank per instance (0.0-1.0) over last minute",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryMaxRankQueueLength,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(vllm:num_requests_waiting{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak queue length of the busiest engine rank per instance over last minute",
	})

	// --- V2 queries for token-based capacity analysis ---

	// Cache config info per instance (static labels with block size and GPU blocks count)
	// Used by Saturation Analyzer V2 for token capacity computation
	// Preserves instance (IP:port for multi-instance pods), pod (for pod lookup), and the config labels
	//
	// The VALUE is the number of engine ranks behind the instance. vLLM sets
	// one series to 1 per rank it serves, differing only in the engine label,
	// and every one of them carries the same num_gpu_blocks: the API server's
	// CacheConfig, into which it sums the blocks each of its engines reported
	// at startup (vllm/v1/engine/core_client.py, "Sum num_gpu_blocks from all
	// engines in DP case"; the same since at least v0.10). So the blocks label
	// is already the instance's total, and counting the series gives its
	// ranks. The inner max per engine keeps a target scraped twice from
	// counting each rank twice. An instance with one rank counts 1, the value
	// the max used to return.
	//
	// NOTE: vllm:cache_config_info is an info-style metric. Unlike vLLM's regular
	// gauges/counters, it is NOT labeled with model_name — its label set is derived
	// from CacheConfig fields (num_gpu_blocks, block_size, cache_dtype, ...) plus
	// "engine". It therefore cannot be partitioned by model at all: the collector
	// correlates the results to a model's pods by instance key instead, attaching
	// cache config only to instances the KV/queue queries already discovered for
	// that model. Do not add a model_name matcher or grouping label here.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryCacheConfigInfo,
		Type:        source.QueryTypePromQL,
		Template:    `count by (instance, pod, num_gpu_blocks, block_size) (max by (instance, pod, num_gpu_blocks, block_size, engine) (vllm:cache_config_info{namespace="{{.namespace}}"}))`,
		Params:      []string{source.ParamNamespace},
		Description: "KV cache configuration info per instance (num_gpu_blocks and block_size as labels; the value counts the instance's engine ranks)",
	})

	// Average output (generation) tokens per completed request
	// Used for output-length-dependent k2 estimation
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgOutputTokens,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:request_generation_tokens_sum", "vllm:request_generation_tokens_count", "5m"),
		Params:      []string{source.ParamNamespace},
		Description: "Average output tokens per completed request (5m rate)",
	})

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgOutputTokensRecent,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:request_generation_tokens_sum", "vllm:request_generation_tokens_count", "1m"),
		Params:      []string{source.ParamNamespace},
		Description: "Mean output tokens per request per pod over a SHORT window, used only as the derived mu divisor (see AvgOutputTokensRecent)",
	})

	// Average input (prompt) tokens per completed request
	// Used in k2 derivation formula: k2 = N_max × (I + O/2)
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgInputTokens,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:request_prompt_tokens_sum", "vllm:request_prompt_tokens_count", "5m"),
		Params:      []string{source.ParamNamespace},
		Description: "Average input tokens per completed request (5m rate)",
	})

	// The same over the SHORT window AvgOutputTokensRecent uses. The two are
	// read as a PAIR, never one without the other: KVreq is ILeff + OL/2, so a
	// [1m] output length against a [5m] prompt length prices one shape's
	// generation on top of the previous shape's prompt. See
	// domain.ReplicaMetrics.AvgInputTokensRecent for the measurement.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgInputTokensRecent,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:request_prompt_tokens_sum", "vllm:request_prompt_tokens_count", "1m"),
		Params:      []string{source.ParamNamespace},
		Description: "Mean input tokens per request per pod over a SHORT window, read only together with AvgOutputTokensRecent",
	})

	// Prefix cache hit rate per instance (5m rate)
	// Used to reduce estimated input token demand for scheduler-queued requests.
	// Returns 0..1 where 1 means all prefix lookups were cache hits.
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	//
	// vLLM declares both as Counters named vllm:prefix_cache_hits and
	// vllm:prefix_cache_queries; the Prometheus client exposes a Counter with a
	// _total suffix, so the scraped names carry it (issue #232).
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryPrefixCacheHitRate,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:prefix_cache_hits_total", "vllm:prefix_cache_queries_total", "5m"),
		Params:      []string{source.ParamNamespace},
		Description: "Prefix cache hit rate per instance (0.0-1.0, 5m rate)",
	})

	// --- Scheduler flow control queries (model-level) ---
	// These come from the llm-d inference scheduler, not engine pods.
	//
	// Like the engine queries above they carry no model matcher: both model
	// identity labels stay in the grouping key and the collector picks the
	// model per series — target_model_name when set, else model_name (see
	// eppSeriesModel). That replaces the "or" clause the model-scoped versions
	// used to need. One cluster-wide execution per cycle serves every model the
	// controller manages, instead of two per model.
	//
	// The namespace stays in the grouping key, and the collector keeps only its
	// own namespace's series (eppSeriesInNamespace). The EPP does not export a
	// namespace itself (gateway-api-inference-extension #2309), but Prometheus
	// stamps the scraped target's namespace on every series, and the arrival-rate
	// query already matches on that label. Grouped by model name alone, two
	// namespaces serving the same model read the sum of both routers' queues: a
	// model at 32 concurrent requests was charged its saturated twin's queue and
	// read a utilization of 0.95 where its own load gave 0.3.
	//
	// The `or` compares whole label sets, so with the namespace in them it now
	// prefers llm-d's family per namespace: two EPP builds in two namespaces are
	// each read under the name they export.

	// Number of requests queued in the scheduler's flow control layer.
	//
	// Both families are read, preferring llm-d's. The EPP renamed this metric to
	// the llm_d_epp_ prefix and upstream gateway-api-inference-extension kept the
	// inference_extension_ one, so which exists depends on the EPP build — and
	// PromQL `or` is exactly that preference: the left side where it has series,
	// the right side only where it does not. Reading only the deprecated name left
	// this query empty on a newer EPP, which mattered twice over once the
	// scale-from-zero fallback started reading it (queue_fallback.go): no series
	// means no demand means nothing is ever woken.
	registry.MustRegister(source.QueryTemplate{
		Name: QuerySchedulerQueueSize,
		Type: source.QueryTypePromQL,
		Template: `sum by (namespace, model_name, target_model_name) (llm_d_epp_flow_control_queue_size)` +
			` or sum by (namespace, model_name, target_model_name) (inference_extension_flow_control_queue_size)`,
		Description: "Requests queued in scheduler flow control, per namespace and model",
	})

	// Total bytes of request bodies queued in the scheduler's flow control layer
	registry.MustRegister(source.QueryTemplate{
		Name: QuerySchedulerQueueBytes,
		Type: source.QueryTypePromQL,
		Template: `sum by (namespace, model_name, target_model_name) (llm_d_epp_flow_control_queue_bytes)` +
			` or sum by (namespace, model_name, target_model_name) (inference_extension_flow_control_queue_bytes)`,
		Description: "Bytes queued in scheduler flow control, per namespace and model",
	})

	registerSGLangSaturationQueries(registry)
}

// registerSGLangSaturationQueries registers the SGLang variants of the
// engine-specific saturation queries. The scheduler flow-control queries above
// are engine-agnostic (sourced from EPP) and are not duplicated here.
func registerSGLangSaturationQueries(registry *source.QueryList) {
	// KV-cache token-pool utilization per instance (peak over last minute).
	// sglang:token_usage is the 0.0-1.0 fraction equivalent of vllm:kv_cache_usage_perc.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryKvCacheUsage,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(sglang:token_usage{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak KV cache utilization per instance (0.0-1.0) over last minute (SGLang)",
	})

	// Queue length per instance (peak over last minute).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryQueueLength,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(sglang:num_queue_reqs{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak queue length per instance over last minute (SGLang)",
	})

	// The busiest-rank figures are the two queries above: they already take
	// the max per instance.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryMaxRankKvCacheUsage,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(sglang:token_usage{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak KV cache utilization of the busiest rank per instance (0.0-1.0) over last minute (SGLang)",
	})
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryMaxRankQueueLength,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(sglang:num_queue_reqs{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak queue length of the busiest rank per instance over last minute (SGLang)",
	})

	// Total KV-cache token capacity per instance.
	//
	// Structural difference from vLLM: SGLang exposes capacity directly via
	// sglang:max_total_num_tokens (a model-labeled gauge), so — unlike
	// vllm:cache_config_info — this one IS partitionable by model_name and
	// returns the capacity as the value, with no num_gpu_blocks/block_size
	// labels. The collector converts this value into TotalKvCapacityTokens
	// directly (see CollectReplicaMetrics).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryCacheConfigInfo,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (sglang:max_total_num_tokens{namespace="{{.namespace}}"})`,
		Params:      []string{source.ParamNamespace},
		Description: "Total KV cache token capacity per instance (SGLang)",
	})

	// Average output (generation) tokens per completed request (5m rate).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgOutputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:generation_tokens_histogram_sum{namespace="{{.namespace}}"}[5m]) / rate(sglang:generation_tokens_histogram_count{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average output tokens per completed request (5m rate) (SGLang)",
	})

	// The same over a SHORT window, the derived mu's divisor only. A
	// count-weighted mean over [5m] is dominated by the previous shape's long
	// stragglers for five minutes after they stop arriving; see
	// domain.ReplicaMetrics.AvgOutputTokensRecent for the measurement.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgOutputTokensRecent,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:generation_tokens_histogram_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:generation_tokens_histogram_count{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Mean output tokens per request per pod over a SHORT window, used only as the derived mu divisor (SGLang)",
	})

	// Average input (prompt) tokens per completed request (5m rate).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgInputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:prompt_tokens_histogram_sum{namespace="{{.namespace}}"}[5m]) / rate(sglang:prompt_tokens_histogram_count{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average input tokens per completed request (5m rate) (SGLang)",
	})

	// The same over the SHORT window, read only together with
	// QueryAvgOutputTokensRecent; see the vLLM registration above.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgInputTokensRecent,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:prompt_tokens_histogram_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:prompt_tokens_histogram_count{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Mean input tokens per request per pod over a SHORT window (SGLang)",
	})

	// Prefix cache hit rate per instance (5m rate).
	//
	// Structural difference from vLLM: derived from SGLang's token counters
	// (cached prompt tokens / total prompt tokens) rather than hit/query counters.
	// This is unit-safe (0.0-1.0) and parallels the vLLM hits/queries formula.
	// SGLang also exposes sglang:cache_hit_rate directly, but its units are
	// version-dependent (0-1 vs 0-100), so the counter ratio is preferred.
	//
	// Each counter is aggregated with sum by(...) BEFORE the division. The two
	// counters do not share an identical label set — sglang:cached_tokens_total
	// carries an extra cache_source label — so dividing the raw rates would leave
	// the operator with no one-to-one matches and yield an empty vector. Summing
	// each side down to the (model_name, instance, pod) key first drops the
	// differing labels and makes the division well-defined. Both sides must
	// carry model_name for the division to match per model.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryPrefixCacheHitRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (rate(sglang:cached_tokens_total{namespace="{{.namespace}}"}[5m])) / sum by (model_name, instance, pod) (rate(sglang:prompt_tokens_total{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Prefix cache hit rate per instance (0.0-1.0, 5m rate) (SGLang)",
	})
}

// perPodMean is the per-pod mean of a vLLM histogram or counter pair over
// window: sum(rate(sumMetric)) / sum(rate(countMetric)), each summed down to
// (model_name, instance, pod) before the division.
//
// Summing first is the data-parallel rule (see the note above
// RegisterSaturationQueries): it weights each engine rank of a pod by its own
// traffic, and a rank that served nothing adds zero to both sides instead of a
// NaN. On a pod with one rank it is the same
// number as rate(sumMetric) / rate(countMetric). A pod with no traffic at all is
// still 0/0, NaN, which the collector drops as it always has.
func perPodMean(sumMetric, countMetric, window string) string {
	side := func(metric string) string {
		return `sum by (model_name, instance, pod) (rate(` + metric + `{namespace="{{.namespace}}"}[` + window + `]))`
	}
	return side(sumMetric) + ` / ` + side(countMetric)
}
