package collector

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
)

// The capacity part of the waiting queue (vLLM's
// num_requests_waiting_by_reason{reason="capacity"}, issue #278) rides beside
// the queue: read where the series exists, unknown where it does not.
func TestCollectReplicaMetrics_CapacityQueue(t *testing.T) {
	glm := map[string]string{seriesModelLabel: "glm", "pod": dp8Pod, "instance": "10.0.0.8:8000"}

	t.Run("published", func(t *testing.T) {
		results := singleFrontendResults()
		addValue(results, registration.QueryCapacityQueueLength, glm, 3)
		addValue(results, registration.QueryMaxRankCapacityQueueLength, glm, 2)
		rows := collectRows(t, results, dp8Pod)
		require.Len(t, rows, 1)
		assert.True(t, rows[0].CapacityQueueKnown)
		assert.Equal(t, 3, rows[0].CapacityQueueLength)
		assert.Equal(t, 2, rows[0].BusiestRankCapacityQueue())
		assert.Equal(t, 8, rows[0].QueueLength, "the full queue is unchanged")
	})

	t.Run("not published", func(t *testing.T) {
		rows := collectRows(t, singleFrontendResults(), dp8Pod)
		require.Len(t, rows, 1)
		assert.False(t, rows[0].CapacityQueueKnown, "an older vLLM publishes no reason")
		assert.Zero(t, rows[0].CapacityQueueLength)
	})

	t.Run("another model's series", func(t *testing.T) {
		results := singleFrontendResults()
		other := map[string]string{seriesModelLabel: "other", "pod": dp8Pod, "instance": "10.0.0.8:8000"}
		addValue(results, registration.QueryCapacityQueueLength, other, 3)
		rows := collectRows(t, results, dp8Pod)
		require.Len(t, rows, 1)
		assert.False(t, rows[0].CapacityQueueKnown, "partitioned by model_name like the queue itself")
	})
}

// Ranks with their own API servers merge the capacity queue as the queue
// merges: summed, the busiest rank's max, and known only if every rank says.
func TestCollectReplicaMetrics_CapacityQueueMergesPerPort(t *testing.T) {
	build := func(withReason func(i int) bool) map[string]*source.MetricResult {
		results := perPortResults()
		for i, r := range dp8Ranks {
			if !withReason(i) {
				continue
			}
			labels := map[string]string{seriesModelLabel: "glm", "pod": dp8Pod, "instance": fmt.Sprintf("10.0.0.8:%d", 8000+i)}
			// Half of each rank's queue waits for capacity.
			addValue(results, registration.QueryCapacityQueueLength, labels, float64(r.wait/2))
			addValue(results, registration.QueryMaxRankCapacityQueueLength, labels, float64(r.wait/2))
		}
		return results
	}

	rows := collectRows(t, build(func(int) bool { return true }), dp8Pod)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].CapacityQueueKnown)
	assert.Equal(t, 4, rows[0].CapacityQueueLength, "rank 6's 1 and rank 7's 3")
	assert.Equal(t, 3, rows[0].BusiestRankCapacityQueue(), "rank 7's")

	rows = collectRows(t, build(func(i int) bool { return i != 7 }), dp8Pod)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].CapacityQueueKnown, "rank 7 did not say why its requests wait")
	assert.Zero(t, rows[0].CapacityQueueLength)
	assert.Zero(t, rows[0].BusiestRankCapacityQueue())
}
