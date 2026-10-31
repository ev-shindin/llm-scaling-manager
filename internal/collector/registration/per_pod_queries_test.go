package registration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
)

// perPodQueryFixture is the promtool unit test that evaluates the per-pod vLLM
// queries on per-rank series (hack/check-collector-queries.sh runs it).
const perPodQueryFixture = "testdata/per-pod-queries.test.yaml"

// perPodVLLMQueries are the vLLM per-pod queries that read a series vLLM
// exports once per data-parallel rank, and so must fold the ranks of a pod
// (issue #150). Each is evaluated by the promtool fixture.
//
// The three rates summed per pod (request rate, generation token rate,
// prefill computed-token rate) are listed too: a sum folds the ranks, but
// summed straight it also counts a target scraped twice twice, so each takes
// a max per rank first, as the gauges do.
var perPodVLLMQueries = []string{
	QueryKvCacheUsage,
	QueryKvUsageInstant,
	QueryKvUsageMean,
	QueryQueueLength,
	QueryMaxRankKvCacheUsage,
	QueryMaxRankQueueLength,
	QueryCapacityQueueLength,
	QueryMaxRankCapacityQueueLength,
	QueryCacheConfigInfo,
	QueryAvgOutputTokens,
	QueryAvgOutputTokensRecent,
	QueryAvgInputTokens,
	QueryAvgInputTokensRecent,
	QueryPrefixCacheHitRate,
	QueryAvgITL,
	QueryAvgTTFT,
	QueryAvgServiceTime,
	QueryRequestRate,
	QueryGenerationTokenRate,
	QueryPrefillComputedTokenRate,
}

// builtPerPodQueries returns the query the registry builds for each of
// perPodVLLMQueries in namespace ns, keyed by query name.
func builtPerPodQueries(t *testing.T, ns string) map[string]string {
	t.Helper()
	reg := source.NewSourceRegistry()
	if err := reg.Register("prometheus", prometheus.NewPrometheusSource(
		context.Background(), &mockPrometheusAPI{},
		prometheus.DefaultPrometheusSourceConfig())); err != nil {
		t.Fatalf("registering the prometheus source: %v", err)
	}
	RegisterSaturationQueries(reg)
	RegisterQueueingModelQueries(reg)
	RegisterAlwaysOnQueries(reg)

	built := make(map[string]string, len(perPodVLLMQueries))
	for _, name := range perPodVLLMQueries {
		q, err := reg.Get("prometheus").QueryList().Build(name, map[string]string{source.ParamNamespace: ns})
		if err != nil {
			t.Fatalf("building %s: %v", name, err)
		}
		built[name] = q
	}
	return built
}

// The promtool fixture evaluates copies of the queries, so a copy that drifts
// from the registry tests nothing. Every per-pod vLLM query must appear in the
// fixture exactly as the registry builds it, and the fixture may hold nothing
// else.
func TestPerPodQueriesMatchPromtoolFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(perPodQueryFixture))
	if err != nil {
		t.Fatalf("reading %s: %v", perPodQueryFixture, err)
	}
	var fixture struct {
		Tests []struct {
			PromQLExprTest []struct {
				Expr       string           `yaml:"expr"`
				ExpSamples []map[string]any `yaml:"exp_samples"`
			} `yaml:"promql_expr_test"`
		} `yaml:"tests"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing %s: %v", perPodQueryFixture, err)
	}
	inFixture := map[string]int{}
	for _, tc := range fixture.Tests {
		for _, e := range tc.PromQLExprTest {
			if len(e.ExpSamples) == 0 {
				t.Errorf("fixture case %q expects no samples, which passes against any query", e.Expr)
			}
			inFixture[e.Expr]++
		}
	}
	// An empty parse would make every check below vacuous.
	if len(inFixture) == 0 {
		t.Fatalf("%s holds no promql_expr_test cases", perPodQueryFixture)
	}

	built := builtPerPodQueries(t, "llm")
	registered := map[string]bool{}
	for name, q := range built {
		registered[q] = true
		if inFixture[q] != 1 {
			t.Errorf("%s: the registry builds\n  %s\nwhich the fixture evaluates %d times, not once", name, q, inFixture[q])
		}
	}
	for expr := range inFixture {
		if !registered[expr] {
			t.Errorf("the fixture evaluates a query the registry does not build:\n  %s", expr)
		}
	}
}

// The structural rule for each query, stated where a reader of the registry
// finds it. The fixture proves the numbers; this names the mistake: a max over
// a pod's ranks priced the busiest rank as the whole pod (issue #150). The two
// busiest-rank queries are the exception by name, and the only ones.
func TestPerPodQueriesFoldDataParallelRanks(t *testing.T) {
	built := builtPerPodQueries(t, "llm")
	const perPod = "by (model_name, instance, pod) ("
	// A max per rank first, so copies of one rank's series -- a second
	// scrape, a per-process pid copy -- count once.
	const perRank = "max by (model_name, instance, pod, engine) ("
	want := map[string]string{
		QueryKvCacheUsage:             "avg " + perPod + perRank,
		QueryKvUsageInstant:           "avg " + perPod + perRank,
		QueryKvUsageMean:              "avg " + perPod + perRank + "avg_over_time(",
		QueryQueueLength:              "sum " + perPod + perRank,
		QueryRequestRate:              "sum " + perPod + perRank + "rate(",
		QueryGenerationTokenRate:      "sum " + perPod + perRank + "rate(",
		QueryPrefillComputedTokenRate: "sum " + perPod + perRank + "rate(",
		QueryMaxRankKvCacheUsage:      "max " + perPod + "max_over_time(",
		QueryMaxRankQueueLength:       "max " + perPod + "max_over_time(",
		QueryCacheConfigInfo: "count by (instance, pod, num_gpu_blocks, block_size) " +
			"(max by (instance, pod, num_gpu_blocks, block_size, engine) (",
		// The waiting count by reason folds as the queue does.
		QueryCapacityQueueLength:        "sum " + perPod + perRank,
		QueryMaxRankCapacityQueueLength: "max " + perPod + "max_over_time(",
	}
	for name, q := range built {
		if prefix, ok := want[name]; ok {
			if !strings.HasPrefix(q, prefix) {
				t.Errorf("%s must start %q, got:\n  %s", name, prefix, q)
			}
			continue
		}
		// Every other one is a mean: summed per pod on both sides of the
		// division, so each rank weighs by its own traffic.
		if !strings.HasPrefix(q, "sum "+perPod+"rate(") || strings.Count(q, "sum "+perPod) != 2 {
			t.Errorf("%s must be sum(rate(_sum)) / sum(rate(_count)) per pod, got:\n  %s", name, q)
		}
		if strings.Contains(q, "max by") {
			t.Errorf("%s takes a max, which folds a pod's ranks to its busiest:\n  %s", name, q)
		}
	}
}
