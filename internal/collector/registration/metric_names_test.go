package registration

import (
	"context"
	"regexp"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
)

// engineMetricName matches an engine metric name inside a PromQL template.
var engineMetricName = regexp.MustCompile(`\b(?:vllm|sglang):[A-Za-z0-9_]+`)

// engineGauges are the engine metrics the collector reads that are Gauges,
// and so are exposed under their declared name with no suffix. Every other
// engine metric a template reads must be a Counter (exposed with _total) or a
// histogram part (_sum, _count, _bucket). A metric added to a template that is
// none of these needs its type checked against the engine's source before it
// goes on this list.
//
// vLLM: vllm/v1/metrics/loggers.py. SGLang:
// python/sglang/srt/observability/metrics_collector.py.
var engineGauges = map[string]bool{
	"vllm:num_requests_running":   true,
	"vllm:num_requests_waiting":   true,
	"vllm:kv_cache_usage_perc":    true,
	"vllm:cache_config_info":      true,
	"sglang:num_running_reqs":     true,
	"sglang:num_queue_reqs":       true,
	"sglang:token_usage":          true,
	"sglang:max_total_num_tokens": true,
	// A gauge with a reason label, since vLLM v0.20.0.
	"vllm:num_requests_waiting_by_reason": true,
}

// exposedAsDeclared reports whether name is one the engine actually exposes:
// a Counter's _total, a histogram part, or a listed gauge. A Counter read
// under its declared name (vllm:prefix_cache_hits) matches no series, since
// the Prometheus client appends _total to every Counter it exposes.
func exposedAsDeclared(name string) bool {
	for _, suffix := range []string{"_total", "_sum", "_count", "_bucket"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return engineGauges[name]
}

var _ = Describe("Engine metric names in query templates", func() {
	var queries *source.QueryList

	BeforeEach(func() {
		registry := source.NewSourceRegistry()
		metricsSource := prometheus.NewPrometheusSource(context.Background(), &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig())
		Expect(registry.Register("prometheus", metricsSource)).To(Succeed())
		RegisterSaturationQueries(registry)
		RegisterScaleToZeroQueries(registry)
		RegisterQueueingModelQueries(registry)
		RegisterSLOQueries(registry)
		RegisterAlwaysOnQueries(registry)
		queries = registry.Get("prometheus").QueryList()
	})

	It("reads every engine metric under the name the engine exposes", func() {
		seen := map[string]bool{}
		names := queries.List()
		sort.Strings(names)
		for _, query := range names {
			for _, metric := range engineMetricName.FindAllString(queries.Get(query).Template, -1) {
				seen[metric] = true
				Expect(exposedAsDeclared(metric)).To(BeTrue(),
					"query %q reads %q, which is neither a Counter's _total, a histogram part, nor a known gauge", query, metric)
			}
		}
		// An empty scan passes the loop above trivially; make sure it scanned.
		Expect(seen).To(HaveKey("vllm:kv_cache_usage_perc"))
		Expect(seen).To(HaveKey("sglang:token_usage"))
		Expect(len(seen)).To(BeNumerically(">=", 20), "scanned names: %v", seen)
	})

	It("reads vLLM's prefix-cache counters with the _total suffix (issue #232)", func() {
		tmpl := queries.Get(QueryPrefixCacheHitRate).Template
		Expect(tmpl).To(ContainSubstring("vllm:prefix_cache_hits_total{"))
		Expect(tmpl).To(ContainSubstring("vllm:prefix_cache_queries_total{"))
	})

	It("rejects a Counter read under its declared name", func() {
		Expect(exposedAsDeclared("vllm:prefix_cache_hits")).To(BeFalse())
		Expect(exposedAsDeclared("vllm:generation_tokens")).To(BeFalse())
		Expect(exposedAsDeclared("vllm:prefix_cache_hits_total")).To(BeTrue())
		Expect(exposedAsDeclared("vllm:request_prompt_tokens_sum")).To(BeTrue())
		Expect(exposedAsDeclared("vllm:kv_cache_usage_perc")).To(BeTrue())
	})
})
