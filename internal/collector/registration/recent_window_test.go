package registration

import (
	"context"
	"regexp"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// rangeWindows returns every range selector a template contains, sorted, so a
// window can be asserted structurally instead of by substring. A substring test
// for "[1m]" passes on a template that also carries a stray "[5m]", which is
// the mistake these queries are most exposed to.
func rangeWindows(template string) []string {
	m := regexp.MustCompile(`\[([0-9]+[smhd])\]`).FindAllStringSubmatch(template, -1)
	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	sort.Strings(out)
	return out
}

// The short-window queries, and the thing about them that has to stay true.
//
// These templates have no unit test anywhere else, and a wrong one does not
// fail: Prometheus returns no series, the field stays zero, the analyzer falls
// back to its [5m] sibling, and the only symptom is a scaling decision priced
// on the wrong shape. Nothing goes red until a cluster run.
var _ = Describe("the short-window shape queries", func() {
	var registry *source.SourceRegistry

	BeforeEach(func() {
		ctx := context.Background()
		registry = source.NewSourceRegistry()
		metricsSource := prometheus.NewPrometheusSource(ctx, &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig())
		Expect(registry.Register("prometheus", metricsSource)).To(Succeed())
		RegisterSaturationQueries(registry)
		RegisterQueueingModelQueries(registry)
	})

	get := func(engine inferenceengine.Engine, logical string) *source.QueryTemplate {
		return registry.Get("prometheus").QueryList().Get(EngineQuery(engine, logical))
	}

	// The invariant the derived mu's shape pairing rests on. KVreq is
	// ILeff + OL/2, so pricing a request from a [1m] generation length and a
	// [5m] prompt length sums two different workloads: measured on a
	// 6000/1000 -> 1000/4000 swap, that read a request as 7017 tokens when
	// neither shape ever exceeded 6500, halved the derived mu and ordered
	// seven decode replicas where three was right. The two windows must match
	// each other, whatever they are.
	DescribeTable("price one shape, not two: the input and output short windows agree",
		func(engine inferenceengine.Engine) {
			in := get(engine, QueryAvgInputTokensRecent)
			out := get(engine, QueryAvgOutputTokensRecent)
			Expect(in).NotTo(BeNil(), "no short-window prompt query for %s", engine)
			Expect(out).NotTo(BeNil(), "no short-window generation query for %s", engine)

			Expect(rangeWindows(in.Template)).To(Equal(rangeWindows(out.Template)),
				"the short-window pair must share one window, or mu is priced on two timescales")
			Expect(rangeWindows(in.Template)).To(Equal([]string{"1m", "1m"}),
				"both halves of the ratio must be rated over the SHORT window")
		},
		Entry("vLLM", inferenceengine.EngineVLLM),
		Entry("SGLang", inferenceengine.EngineSGLang),
	)

	// A short window that quietly became a long one would make the pairing a
	// no-op and reinstate the straggler bug it exists for, with every test
	// above still green because both halves would still agree.
	DescribeTable("the [5m] siblings stay long",
		func(engine inferenceengine.Engine, logical string) {
			q := get(engine, logical)
			Expect(q).NotTo(BeNil())
			Expect(rangeWindows(q.Template)).To(Equal([]string{"5m", "5m"}),
				"%s on %s must keep the long window its callers expect", logical, engine)
		},
		Entry("vLLM input", inferenceengine.EngineVLLM, QueryAvgInputTokens),
		Entry("vLLM output", inferenceengine.EngineVLLM, QueryAvgOutputTokens),
		Entry("SGLang input", inferenceengine.EngineSGLang, QueryAvgInputTokens),
		Entry("SGLang output", inferenceengine.EngineSGLang, QueryAvgOutputTokens),
	)

	// The short window must measure the SAME quantity as its sibling, only over
	// less time. Reading a different counter would make the two incomparable,
	// and the fallback between them meaningless.
	DescribeTable("each short window reads its sibling's counter",
		func(engine inferenceengine.Engine, recent, long string) {
			r, l := get(engine, recent), get(engine, long)
			Expect(r).NotTo(BeNil())
			Expect(l).NotTo(BeNil())
			base := regexp.MustCompile(`([a-z]+):([a-z_]+?)_(sum|count)\{`)
			names := func(t string) []string {
				seen := map[string]bool{}
				out := []string{}
				for _, g := range base.FindAllStringSubmatch(t, -1) {
					k := g[1] + ":" + g[2]
					if !seen[k] {
						seen[k] = true
						out = append(out, k)
					}
				}
				sort.Strings(out)
				return out
			}
			Expect(names(r.Template)).To(Equal(names(l.Template)),
				"%s must rate the same counter as %s", recent, long)
		},
		Entry("vLLM output", inferenceengine.EngineVLLM, QueryAvgOutputTokensRecent, QueryAvgOutputTokens),
		Entry("vLLM input", inferenceengine.EngineVLLM, QueryAvgInputTokensRecent, QueryAvgInputTokens),
		Entry("SGLang output", inferenceengine.EngineSGLang, QueryAvgOutputTokensRecent, QueryAvgOutputTokens),
		Entry("SGLang input", inferenceengine.EngineSGLang, QueryAvgInputTokensRecent, QueryAvgInputTokens),
	)

	// An engine's template naming the other engine's metrics is the failure the
	// per-engine registration exists to prevent, and it returns no series
	// rather than an error.
	DescribeTable("no template names the other engine's metrics",
		func(engine inferenceengine.Engine, forbidden string, logicals ...string) {
			for _, logical := range logicals {
				q := get(engine, logical)
				Expect(q).NotTo(BeNil(), "%s missing for %s", logical, engine)
				Expect(q.Template).NotTo(ContainSubstring(forbidden),
					"%s on %s names %s", logical, engine, forbidden)
			}
		},
		Entry("vLLM carries no sglang: names", inferenceengine.EngineVLLM, "sglang:",
			QueryAvgInputTokensRecent, QueryAvgOutputTokensRecent, QueryAvgTTFT),
		Entry("SGLang carries no vllm: names", inferenceengine.EngineSGLang, "vllm:",
			QueryAvgInputTokensRecent, QueryAvgOutputTokensRecent, QueryAvgTTFT),
	)
})

// TTFT and the prefill computed-token rate: the two queries the prefill
// pricing path depends on, neither previously asserted anywhere.
var _ = Describe("the prefill pricing queries", func() {
	var registry *source.SourceRegistry

	BeforeEach(func() {
		ctx := context.Background()
		registry = source.NewSourceRegistry()
		metricsSource := prometheus.NewPrometheusSource(ctx, &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig())
		Expect(registry.Register("prometheus", metricsSource)).To(Succeed())
		RegisterQueueingModelQueries(registry)
	})

	get := func(engine inferenceengine.Engine, logical string) *source.QueryTemplate {
		return registry.Get("prometheus").QueryList().Get(EngineQuery(engine, logical))
	}

	It("reads first-token latency, not end-to-end latency", func() {
		q := get(inferenceengine.EngineVLLM, QueryAvgTTFT)
		Expect(q).NotTo(BeNil())
		Expect(q.Template).To(ContainSubstring("vllm:time_to_first_token_seconds_sum"))
		Expect(q.Template).To(ContainSubstring("vllm:time_to_first_token_seconds_count"))
		Expect(q.Template).NotTo(ContainSubstring("e2e_request_latency"),
			"end-to-end latency includes the generation, which is the quantity TTFT must exclude")
	})

	It("reads SGLang's own first-token histogram", func() {
		q := get(inferenceengine.EngineSGLang, QueryAvgTTFT)
		Expect(q).NotTo(BeNil())
		Expect(q.Template).To(ContainSubstring("sglang:time_to_first_token_seconds_sum"))
		Expect(q.Template).To(ContainSubstring("sglang:time_to_first_token_seconds_count"))
	})

	// SUMMED over the engines, not maxed. A pod's engines each compute part of
	// the prefill, so their token rates add; pod_collapse.go sums this field for
	// the same reason. A max over the pod would report a multi-engine replica at
	// a fraction of the work it is really doing. The max is per ENGINE only, so
	// a target scraped twice counts each engine once.
	It("sums the prefill computed-token rate across a pod's engines", func() {
		q := get(inferenceengine.EngineVLLM, QueryPrefillComputedTokenRate)
		Expect(q).NotTo(BeNil())
		Expect(q.Template).To(ContainSubstring("vllm:request_prefill_kv_computed_tokens_sum"))
		Expect(q.Template).To(HavePrefix("sum by"),
			"a pod's engines each compute part of the prefill, so their rates add")
		Expect(q.Template).To(HavePrefix("sum by (model_name, instance, pod) (max by (model_name, instance, pod, engine) ("),
			"the max folds a second scrape of one engine, never two engines")
	})

	// Deliberately NOT an engine-specific pair: SGLang publishes no per-stage
	// prefill figure at all (sgl-project/sglang issue #14303), so the query is
	// registered once under its bare name and simply returns no series there,
	// leaving the field at zero and prefill priced the way it always was.
	// Pinned because the obvious "fix" -- adding it to EngineSpecificQueries --
	// would claim an SGLang variant that does not exist.
	It("is registered once, engine-agnostically, with no SGLang variant", func() {
		Expect(IsEngineSpecific(QueryPrefillComputedTokenRate)).To(BeFalse(),
			"marking it engine-specific asserts an SGLang template nobody can write")
		Expect(EngineQuery(inferenceengine.EngineSGLang, QueryPrefillComputedTokenRate)).
			To(Equal(QueryPrefillComputedTokenRate),
				"an agnostic query keeps its bare name on every engine")
	})
})
