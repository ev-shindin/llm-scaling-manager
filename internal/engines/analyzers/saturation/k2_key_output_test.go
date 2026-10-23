package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// k2KeysOverCycles runs one Analyze per output length in outputs over a single
// decode replica and returns the k2-decision historyKey of every cycle and the
// price the variant got on each.
func k2KeysOverCycles(t *testing.T, role string, tokens []int64, queues []int, outputs []float64) ([]string, []float64) {
	t.Helper()
	require.Len(t, tokens, len(outputs))
	require.Len(t, queues, len(outputs))
	analyzer := NewSaturationAnalyzer(capacity.NewStore())
	keys := make([]string, 0, len(outputs))
	prices := make([]float64, 0, len(outputs))
	for i, out := range outputs {
		ctx, logs := observedCtx(t)
		rm := makeReplicaMetrics("pod-k", "variant-k", tokens[i], 376_480, queues[i], 1000, out)
		result, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm},
			[]domain.VariantReplicaState{
				{VariantName: "variant-k", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: role},
			}))
		require.NoError(t, err)
		decisions := logs.FilterMessage("k2-decision").All()
		require.Len(t, decisions, 1, "cycle %d: one k2 decision per replica", i)
		keys = append(keys, stringField(t, decisions[0].ContextMap(), "historyKey"))
		require.Len(t, result.VariantCapacities, 1)
		prices = append(prices, result.VariantCapacities[0].PerReplicaCapacity)
	}
	return keys, prices
}

// A workload whose output length sits on the 500-token medium/long boundary
// keeps ONE k2 window. Keyed on the replica's raw average it alternated
// between two windows cycle by cycle, and the variant's capacity with them:
// on the run this replays, 260 622 and 187 458 on consecutive cycles at
// constant load (util 1.02 <-> 1.42).
func TestK2HistoryKey_DoesNotFlipOnABucketBoundary(t *testing.T) {
	// Two saturated readings land in the two buckets the raw average would
	// pick (the first under 500, the second over), then the replica stops
	// queueing and every later cycle READS the window: P2-hist.
	outputs := []float64{499.8, 500.2, 499.9, 500.1, 499.7, 500.3, 499.8, 500.2}
	tokens := []int64{260_622, 187_458, 255_000, 255_000, 255_000, 255_000, 255_000, 255_000}
	queues := []int{30, 30, 0, 0, 0, 0, 0, 0}
	keys, prices := k2KeysOverCycles(t, domain.RoleDecode, tokens, queues, outputs)

	distinct := map[string]bool{}
	for _, k := range keys {
		distinct[k] = true
	}
	assert.Len(t, distinct, 1, "one window for one workload, got keys %v", keys)
	// One window holding both readings prices every read cycle the same.
	for i := 2; i < len(prices); i++ {
		assert.InDelta(t, prices[2], prices[i], 0.5, "cycle %d priced %v, cycle 2 %v", i, prices[i], prices[2])
	}
	assert.InDelta(t, (260_622+187_458)/2, prices[2], 1, "the window is the mean of both readings")
}
