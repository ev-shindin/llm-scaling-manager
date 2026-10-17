package collector

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// One pod of eight data-parallel ranks, the layout of issue #150, measured two
// ways. These are the per-rank figures of dp8-0 in the promtool fixture
// registration/testdata/per-pod-queries.test.yaml: rank 7 is hot, rank 5 idle.
type dpRank struct {
	kv, rps, out, in, itl, ttft, svc, hit float64
	wait                                  int
}

var dp8Ranks = func() []dpRank {
	normal := dpRank{kv: 0.125, rps: 1, out: 512, in: 1024, itl: 1.0 / 64, ttft: 0.25, svc: 8, hit: 0.5}
	ranks := make([]dpRank, 8)
	for i := range ranks {
		ranks[i] = normal
	}
	ranks[5] = dpRank{} // idle: no traffic, an empty cache, no means to report
	ranks[6].wait = 2
	ranks[7] = dpRank{kv: 0.875, wait: 6, rps: 2, out: 2048, in: 4096, itl: 1.0 / 16, ttft: 1, svc: 32, hit: 0.25}
	return ranks
}()

const (
	dp8BlocksPerRank = 10000
	dp8BlockSize     = 64
	dp8Pod           = "dp8-0"
)

// What the per-pod queries return for dp8-0 behind one API server: the
// fixture's expected samples, which promtool evaluates from the rank series.
var dp8Folded = map[string]float64{
	registration.QueryKvCacheUsage:   0.203125,
	registration.QueryKvUsageInstant: 0.203125,
	registration.QueryQueueLength:    8,
	// The busiest rank, rank 7.
	registration.QueryMaxRankKvCacheUsage:   0.875,
	registration.QueryMaxRankQueueLength:    6,
	registration.QueryAvgOutputTokens:       896,
	registration.QueryAvgOutputTokensRecent: 896,
	registration.QueryAvgInputTokens:        1792,
	registration.QueryAvgInputTokensRecent:  1792,
	registration.QueryPrefixCacheHitRate:    0.35714285714285715,
	registration.QueryAvgITL:                0.04241071428571429,
	registration.QueryAvgTTFT:               0.4375,
	registration.QueryAvgServiceTime:        14,
	// Already per-pod sums before issue #150, unchanged by it.
	registration.QueryRequestRate:         8,
	registration.QueryGenerationTokenRate: 7168,
}

// collectRows runs CollectReplicaMetrics over the given query results, every
// pod attributed to va-dp.
func collectRows(t *testing.T, results map[string]*source.MetricResult, pods ...string) []domain.ReplicaMetrics {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(reg))
	scheme := runtime.NewScheme()
	require.NoError(t, llmdVariantAutoscalingV1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	scalers := map[string]string{}
	for _, p := range pods {
		scalers[p] = "va-dp"
	}
	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return results, nil
		},
	}
	c := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil, scalerLocator(scalers))
	rows, err := c.CollectReplicaMetrics(context.Background(), "glm", "llm",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling), nil)
	require.NoError(t, err)
	return rows
}

func addValue(results map[string]*source.MetricResult, query string, labels map[string]string, v float64) {
	if results[query] == nil {
		results[query] = &source.MetricResult{QueryName: query}
	}
	results[query].Values = append(results[query].Values, source.MetricValue{Labels: labels, Value: v, Timestamp: time.Now()})
}

// singleFrontendResults is dp8-0 as Prometheus returns it with all eight ranks
// behind one API server: one row per query, already folded, and a
// cache_config_info row counting eight ranks whose num_gpu_blocks label is the
// API server's total.
func singleFrontendResults() map[string]*source.MetricResult {
	labels := map[string]string{seriesModelLabel: "glm", "pod": dp8Pod, "instance": "10.0.0.8:8000"}
	results := map[string]*source.MetricResult{}
	for q, v := range dp8Folded {
		addValue(results, q, labels, v)
	}
	addValue(results, registration.QueryCacheConfigInfo, map[string]string{
		"pod": dp8Pod, "instance": "10.0.0.8:8000",
		"num_gpu_blocks": strconv.Itoa(8 * dp8BlocksPerRank), "block_size": strconv.Itoa(dp8BlockSize),
	}, 8)
	return results
}

// perPortResults is the same eight ranks with one API server each: eight
// instances of one pod, each reporting its own rank's figures and one rank.
func perPortResults() map[string]*source.MetricResult {
	results := map[string]*source.MetricResult{}
	for i, r := range dp8Ranks {
		inst := fmt.Sprintf("10.0.0.8:%d", 8000+i)
		labels := map[string]string{seriesModelLabel: "glm", "pod": dp8Pod, "instance": inst}
		addValue(results, registration.QueryKvCacheUsage, labels, r.kv)
		addValue(results, registration.QueryKvUsageInstant, labels, r.kv)
		addValue(results, registration.QueryQueueLength, labels, float64(r.wait))
		// One rank per instance: its busiest rank is itself.
		addValue(results, registration.QueryMaxRankKvCacheUsage, labels, r.kv)
		addValue(results, registration.QueryMaxRankQueueLength, labels, float64(r.wait))
		addValue(results, registration.QueryRequestRate, labels, r.rps)
		addValue(results, registration.QueryGenerationTokenRate, labels, r.rps*r.out)
		if r.rps > 0 {
			// An idle rank's means are 0/0 and the collector drops them.
			for q, v := range map[string]float64{
				registration.QueryAvgOutputTokens:       r.out,
				registration.QueryAvgOutputTokensRecent: r.out,
				registration.QueryAvgInputTokens:        r.in,
				registration.QueryAvgInputTokensRecent:  r.in,
				registration.QueryPrefixCacheHitRate:    r.hit,
				registration.QueryAvgITL:                r.itl,
				registration.QueryAvgTTFT:               r.ttft,
				registration.QueryAvgServiceTime:        r.svc,
			} {
				addValue(results, q, labels, v)
			}
		}
		addValue(results, registration.QueryCacheConfigInfo, map[string]string{
			"pod": dp8Pod, "instance": inst,
			"num_gpu_blocks": strconv.Itoa(dp8BlocksPerRank), "block_size": strconv.Itoa(dp8BlockSize),
		}, 1)
	}
	return results
}

// A pod of eight ranks behind one API server is one row whose capacity is the
// whole pod's, whose utilization is the mean over the ranks, and which says it
// covers eight ranks.
func TestCollectReplicaMetrics_SingleFrontendDPPod(t *testing.T) {
	rows := collectRows(t, singleFrontendResults(), dp8Pod)
	require.Len(t, rows, 1)
	pod := rows[0]

	assert.Equal(t, 8, pod.EngineRanks, "one cache_config_info series per rank")
	assert.Equal(t, 8, pod.Ranks())
	assert.Equal(t, int64(80000), pod.NumGpuBlocks,
		"the blocks label is already the API server's total and must not be multiplied by the ranks")
	assert.Equal(t, int64(80000*64), pod.TotalKvCapacityTokens, "eight ranks' capacity, not one")
	assert.InDelta(t, 0.203125, pod.KvCacheUsage, 1e-12, "the mean over the ranks, not the busiest rank's 0.875")
	assert.Equal(t, int64(1040000), pod.TokensInUse, "used tokens summed over the ranks")
	assert.Equal(t, 8, pod.QueueLength, "every rank's waiting requests")
	assert.InDelta(t, 0.04241071428571429, pod.AvgITL, 1e-15, "the token-weighted mean, not the slowest rank's 0.0625")
	// Beside the sum and the mean, the busiest rank: what the saturation
	// guards judge.
	assert.Equal(t, 6, pod.MaxRankQueueLength, "rank 7's queue, of the pod's 8")
	assert.Equal(t, 6, pod.BusiestRankQueue())
	assert.Equal(t, 0.875, pod.MaxRankKvCacheUsage, "rank 7's cache")
	assert.Equal(t, 0.875, pod.BusiestRankKvUsage())
	assert.Equal(t, int64(4480000), pod.BusiestRankTokens(), "0.875 of the pod's 5,120,000 tokens: one full rank, scaled to the pod")
}

// The same physical pod with one API server per rank gives the same row: the
// collector's merge and the queries' fold follow the same rules.
func TestCollectReplicaMetrics_DPLayoutsAgree(t *testing.T) {
	single := collectRows(t, singleFrontendResults(), dp8Pod)
	perPort := collectRows(t, perPortResults(), dp8Pod)
	require.Len(t, single, 1)
	require.Len(t, perPort, 1, "the eight instances of one pod collapse to one replica")
	a, b := single[0], perPort[0]

	assert.Equal(t, a.EngineRanks, b.EngineRanks, "EngineRanks")
	assert.Equal(t, a.NumGpuBlocks, b.NumGpuBlocks, "NumGpuBlocks")
	assert.Equal(t, a.TotalKvCapacityTokens, b.TotalKvCapacityTokens, "TotalKvCapacityTokens")
	assert.Equal(t, a.TokensInUse, b.TokensInUse, "TokensInUse")
	assert.Equal(t, a.QueueLength, b.QueueLength, "QueueLength")
	assert.Equal(t, a.MaxRankQueueLength, b.MaxRankQueueLength, "MaxRankQueueLength")
	assert.Equal(t, a.BusiestRankTokens(), b.BusiestRankTokens(), "BusiestRankTokens")
	for _, f := range []struct {
		name string
		get  func(domain.ReplicaMetrics) float64
	}{
		{"KvCacheUsage", func(m domain.ReplicaMetrics) float64 { return m.KvCacheUsage }},
		{"KvUsageInstant", func(m domain.ReplicaMetrics) float64 { return m.KvUsageInstant }},
		{"MaxRankKvCacheUsage", func(m domain.ReplicaMetrics) float64 { return m.MaxRankKvCacheUsage }},
		{"RequestRate", func(m domain.ReplicaMetrics) float64 { return m.RequestRate }},
		{"GenerationTokenRate", func(m domain.ReplicaMetrics) float64 { return m.GenerationTokenRate }},
		{"AvgOutputTokens", func(m domain.ReplicaMetrics) float64 { return m.AvgOutputTokens }},
		{"AvgOutputTokensRecent", func(m domain.ReplicaMetrics) float64 { return m.AvgOutputTokensRecent }},
		{"AvgInputTokens", func(m domain.ReplicaMetrics) float64 { return m.AvgInputTokens }},
		{"AvgInputTokensRecent", func(m domain.ReplicaMetrics) float64 { return m.AvgInputTokensRecent }},
		{"PrefixCacheHitRate", func(m domain.ReplicaMetrics) float64 { return m.PrefixCacheHitRate }},
		{"AvgITL", func(m domain.ReplicaMetrics) float64 { return m.AvgITL }},
		{"AvgTTFT", func(m domain.ReplicaMetrics) float64 { return m.AvgTTFT }},
		{"AvgServiceTime", func(m domain.ReplicaMetrics) float64 { return m.AvgServiceTime }},
	} {
		assert.InDelta(t, f.get(a), f.get(b), 1e-12, "%s: one API server %v, one per rank %v", f.name, f.get(a), f.get(b))
	}
}

// An ordinary pod reads exactly as it did: one rank, and every field the value
// its query returned.
func TestCollectReplicaMetrics_SingleRankPodUnchanged(t *testing.T) {
	labels := map[string]string{seriesModelLabel: "glm", "pod": "solo-0", "instance": "10.0.1.1:8000"}
	results := map[string]*source.MetricResult{}
	addValue(results, registration.QueryKvCacheUsage, labels, 0.5)
	addValue(results, registration.QueryQueueLength, labels, 3)
	addValue(results, registration.QueryAvgITL, labels, 1.0/64)
	addValue(results, registration.QueryCacheConfigInfo, map[string]string{
		"pod": "solo-0", "instance": "10.0.1.1:8000", "num_gpu_blocks": "10000", "block_size": "64",
	}, 1)
	rows := collectRows(t, results, "solo-0")
	require.Len(t, rows, 1)
	pod := rows[0]
	assert.Equal(t, 1, pod.EngineRanks)
	assert.Equal(t, int64(640000), pod.TotalKvCapacityTokens)
	assert.Equal(t, int64(320000), pod.TokensInUse)
	assert.Equal(t, 0.5, pod.KvCacheUsage)
	assert.Equal(t, 3, pod.QueueLength)
	assert.Equal(t, 1.0/64, pod.AvgITL)

	// Without a cache_config_info row the count is unknown and reads as one.
	delete(results, registration.QueryCacheConfigInfo)
	rows = collectRows(t, results, "solo-0")
	require.Len(t, rows, 1)
	assert.Equal(t, 0, rows[0].EngineRanks)
	assert.Equal(t, 1, rows[0].Ranks())
}

// Rows merged by collapseToPods add their ranks, and a row with no count is
// one rank.
func TestCollapseToPods_SumsEngineRanks(t *testing.T) {
	pods := collapseToPods([]domain.ReplicaMetrics{
		{PodName: "p", ModelID: "m", EngineRanks: 4},
		{PodName: "p", ModelID: "m", EngineRanks: 4},
		{PodName: "q", ModelID: "m"},
		{PodName: "q", ModelID: "m"},
		{PodName: "solo", ModelID: "m", EngineRanks: 1},
	})
	require.Len(t, pods, 3)
	assert.Equal(t, 8, pods[0].EngineRanks, "two API servers of four ranks each")
	assert.Equal(t, 2, pods[1].EngineRanks, "two instances without a count are two ranks")
	assert.Equal(t, 1, pods[2].EngineRanks, "a pod with one row is left as it is")
}
