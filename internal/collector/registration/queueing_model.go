// This file provides queueing model analyzer metrics collection using the source
// infrastructure with registered query templates.
package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// Query name constants for queueing model analyzer metrics.
const (
	// QueryAvgITL is the query name for average inter-token latency per pod (in seconds).
	// Source: vllm:inter_token_latency_seconds histogram
	QueryAvgITL = "avg_itl"

	// QueryAvgServiceTime is the query name for how long a request occupies a
	// replica, EXCLUDING time spent waiting in the queue (seconds).
	//
	// The exclusion is the whole point. End-to-end latency rises when a fleet is
	// behind and falls when it catches up, so anything built on it varies with
	// capacity -- which is the property that makes occupancy unusable for sizing.
	// Service time does not: it is what one request costs to serve.
	//
	// vLLM publishes it directly as request_inference_time_seconds. SGLang has no
	// single equivalent but publishes both halves, so it is reconstructed there as
	// e2e minus queue.
	QueryAvgServiceTime = "avg_service_time"

	// QueryAvgTTFT is the query name for average time-to-first-token per pod
	// (seconds). Source: vllm:time_to_first_token_seconds /
	// sglang:time_to_first_token_seconds.
	//
	// This is PREFILL's latency. A prefill replica computes the prompt, emits
	// one token and hands the KV to decode, so on a disaggregated fleet TTFT is
	// essentially the whole of what that replica does -- where ITL, the metric
	// beside it, is what a decode replica does. Collected for the same reason
	// ITL is: to fit a line against it and price a replica from the fit rather
	// than from having to watch it saturate.
	//
	// It includes queue wait, which is why the model fitted from it regresses
	// on the tokens resident in the replica rather than taking TTFT as a
	// capacity on its own: a rising TTFT at constant work means the fleet is
	// behind, which is the property that makes end-to-end latency unusable for
	// sizing (see QueryAvgServiceTime above).
	QueryAvgTTFT = "avg_ttft"

	// QueryPrefillComputedTokenRate is the query name for the rate at which a
	// replica computes prefill KV tokens (tokens/second).
	// Source: vllm:request_prefill_kv_computed_tokens_sum.
	//
	// This is prefill's capacity in prefill's own unit. shape_change.go's
	// saturatedCompletionRate already names it as the figure prefill should
	// be priced by -- "prefill's own counterpart is prompt tokens per second
	// ... which the collector does not gather today" -- and this gathers it.
	// The computed-token counter is better than the prompt-token one that
	// comment names, because it already excludes a cached prefix.
	//
	// A SUM across the pod's engines, not a max: the two halves of a
	// multi-engine replica each compute part of the work, and their rates add.
	// The latency queries above take a max because a latency does not.
	//
	// vLLM only. SGLang publishes no per-stage prefill counter
	// (sgl-project/sglang issue #14303), so there is deliberately no SGLang
	// registration below and an SGLang prefill variant keeps the request-rate
	// reading it has today.
	QueryPrefillComputedTokenRate = "prefill_computed_token_rate"
)

// RegisterQueueingModelQueries registers queries used by the queueing model analyzer.
func RegisterQueueingModelQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// Average inter-token latency per instance (seconds).
	// Uses histogram rate(sum[1m]) / rate(count[1m]) over a 1m sliding window.
	// Used by queueing model tuner as the observed ITL for Kalman filter updates.
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:     QueryAvgITL,
		Type:     source.QueryTypePromQL,
		Template: `max by (model_name, instance, pod) (rate(vllm:inter_token_latency_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(vllm:inter_token_latency_seconds_count{namespace="{{.namespace}}"}[1m]))`,
		Params:   []string{source.ParamNamespace},
		Description: "Average inter-token latency per instance (seconds), " +
			"used by queueing model tuner for parameter learning",
	})

	// Average per-request service time (seconds), queue wait excluded.
	// vLLM publishes this directly; SGLang has no equivalent and reconstructs it
	// from two histograms (see the SGLang registration below).
	registry.MustRegister(source.QueryTemplate{
		Name:     QueryAvgServiceTime,
		Type:     source.QueryTypePromQL,
		Template: `max by (model_name, instance, pod) (rate(vllm:request_inference_time_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(vllm:request_inference_time_seconds_count{namespace="{{.namespace}}"}[1m]))`,
		Params:   []string{source.ParamNamespace},
		Description: "Average per-request service time excluding queue wait (seconds), " +
			"used to size demand from the offered load",
	})

	// Average time-to-first-token per instance (seconds), 1m sliding window.
	// The prefill side's latency, and the regressand of the prefill capacity
	// model (docs/proposals/prefill-ttft-model.md). Same window as ITL beside
	// it, so the two roles' models are fitted over comparable intervals.
	registry.MustRegister(source.QueryTemplate{
		Name:     QueryAvgTTFT,
		Type:     source.QueryTypePromQL,
		Template: `max by (model_name, instance, pod) (rate(vllm:time_to_first_token_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(vllm:time_to_first_token_seconds_count{namespace="{{.namespace}}"}[1m]))`,
		Params:   []string{source.ParamNamespace},
		Description: "Average time to first token per instance (seconds), " +
			"the prefill capacity model's regressand",
	})

	// Prefill computed-token rate per instance (tokens/second), 1m window.
	// Prefill's capacity in the unit it is bounded by; see the constant.
	registry.MustRegister(source.QueryTemplate{
		Name:     QueryPrefillComputedTokenRate,
		Type:     source.QueryTypePromQL,
		Template: `sum by (model_name, instance, pod) (rate(vllm:request_prefill_kv_computed_tokens_sum{namespace="{{.namespace}}"}[1m]))`,
		Params:   []string{source.ParamNamespace},
		Description: "Prefill KV tokens computed per second per instance, " +
			"excluding cached prefix; prefill's capacity unit",
	})

	registerSGLangQueueingModelQueries(registry)
}

// registerSGLangQueueingModelQueries registers the SGLang variants of the
// engine-specific queueing-model queries. The scheduler dispatch-rate query above
// is engine-agnostic (sourced from EPP) and is not duplicated here.
func registerSGLangQueueingModelQueries(registry *source.QueryList) {
	// Average inter-token latency per instance (seconds), 1m sliding window.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgITL,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:inter_token_latency_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:inter_token_latency_seconds_count{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average inter-token latency per instance (seconds) (SGLang)",
	})

	// Average time-to-first-token per instance (seconds), 1m sliding window.
	// SGLang publishes the same histogram shape under its own prefix, so this
	// is a straight rename rather than a reconstruction like service time.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgTTFT,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:time_to_first_token_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:time_to_first_token_seconds_count{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average time to first token per instance (seconds) (SGLang)",
	})

	// Service time: SGLang has no single metric for it, but publishes both
	// halves, so subtract the queue wait from end-to-end. clamp_min guards the
	// window where the two rates disagree -- they cover the same interval but
	// count different request populations, so their difference can dip below
	// zero without either being wrong.
	//
	// The two histograms are NOT labelled identically upstream: e2e carries an
	// extra is_streaming label that queue_time does not (sglang's
	// python/sglang/srt/observability/metrics_collector.py builds e2e with
	// labelnames + ["is_streaming"] where queue_time uses labelnames alone). `max by (model_name,
	// instance, pod)` drops it on both sides so the subtraction still matches
	// series, but it means the e2e term is the max ACROSS streaming and
	// non-streaming rather than a pooled average, which biases W upward. That is
	// the safe direction for a floor -- it over-provisions rather than under --
	// and it is why this uses max rather than avg, consistent with the ITL query
	// above.
	//
	// Unverified against a live SGLang: whether queue_time is observed for every
	// request or only for requests that actually queued. If the latter, its mean
	// is taken over a biased subset and the subtraction systematically
	// understates the wait, which would inflate W further -- again the safe
	// direction, but worth confirming before anyone relies on the magnitude.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:     QueryAvgServiceTime,
		Type:     source.QueryTypePromQL,
		Template: `clamp_min(max by (model_name, instance, pod) (rate(sglang:e2e_request_latency_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:e2e_request_latency_seconds_count{namespace="{{.namespace}}"}[1m])) - max by (model_name, instance, pod) (rate(sglang:queue_time_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:queue_time_seconds_count{namespace="{{.namespace}}"}[1m])), 0)`,
		Params:   []string{source.ParamNamespace},
		Description: "Average per-request service time excluding queue wait (seconds) " +
			"(SGLang, reconstructed as e2e minus queue)",
	})

}
