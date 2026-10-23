package saturation

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// queueOnlyDemand runs one cycle with 100 requests (and bytes) in the
// scheduler's queue over rows, after the cycles in before, and returns the
// model's demand.
func queueOnlyDemand(t *testing.T, before [][]domain.ReplicaMetrics, rows []domain.ReplicaMetrics, replicas int, bytes int64) float64 {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	ctx := logr.NewContext(t.Context(), logr.Discard())
	states := func(n int) []domain.VariantReplicaState {
		return []domain.VariantReplicaState{{VariantName: "variant-q", AcceleratorName: "H200",
			CurrentReplicas: n, GPUsPerReplica: 1, Role: domain.RoleDecode}}
	}
	for _, b := range before {
		_, err := a.Analyze(ctx, makeAnalyzerInput(b, states(len(b))))
		require.NoError(t, err)
	}
	in := makeAnalyzerInput(rows, states(replicas))
	in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 100, QueueBytes: bytes}
	res, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	return res.TotalDemand
}

// The same scheduler queue with no engine metrics to price it was worth 0,
// 100 000 or 51 200 tokens depending on whether there were zero replicas or
// one without series, and whether the queue reported bytes. Each half of a
// request now falls back on its own -- the prompt to the fleet's tracked
// length (the bytes win when larger), the output to the expected length (512
// by default) -- so the three agree.
func TestSchedulerQueue_PricedTheSameWithNoEngineMetrics(t *testing.T) {
	bare := []domain.ReplicaMetrics{{PodName: "p", VariantName: "variant-q", Ready: true,
		ModelID: "test-model", Namespace: "test-ns"}}
	zero := queueOnlyDemand(t, nil, nil, 0, 0)
	zeroBytes := queueOnlyDemand(t, nil, nil, 0, 400_000)
	one := queueOnlyDemand(t, nil, bare, 1, 0)
	assert.InDelta(t, 100*DefaultExpectedOutputTokens, zero, 1, "zero replicas, no bytes: the output is still owed")
	assert.InDelta(t, 400_000/BytesPerToken+100*DefaultExpectedOutputTokens, zeroBytes, 1, "zero replicas, bytes: prompt and output")
	assert.InDelta(t, zero, one, 1, "one replica without series prices it as zero replicas do")

	// A fleet that has measured its shape and then parked prices the queue at
	// that shape: 100 x (1000 + 500).
	served := [][]domain.ReplicaMetrics{{makeReplicaMetrics("p", "variant-q", 50_000, 1_162_240, 0, 1000, 500)}}
	parked := queueOnlyDemand(t, served, nil, 0, 0)
	assert.InDelta(t, 100*(1000+500), parked, 1)
}
