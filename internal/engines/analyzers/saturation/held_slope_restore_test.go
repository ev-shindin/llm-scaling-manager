package saturation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// heldLinePricing is what one cycle priced a light decode variant from: its
// derived-mu line and the utilization it reported.
type heldLinePricing struct {
	ok, held    bool
	rate, a, b  float64
	utilization float64
}

// Issue #316: a line held through light load (issue #284) survives a restart.
// The snapshot carried the ITL baseline and not the held slope, so a
// controller restarted under light load found a window with no readings and
// no slope to hold, had no line of its own -- its own published line is not
// one it may borrow -- and sized the variant by KV occupancy alone.
func TestAHeldITLLineSurvivesARestart(t *testing.T) {
	const variant = "variant-a"
	states := []domain.VariantReplicaState{
		{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	replica := func(k, itlSec float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics("pod-0", variant, int64(k*16000), 16000, 0, 1000, 500)
		rm.KvUsageInstant = k
		rm.AvgITL = itlSec
		return rm
	}
	clock := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	process := func() *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", variant, capacity.Record{EngineParams: mtParams(64, "vllm:v1")}, time.Now())
		a := NewSaturationAnalyzer(store)
		a.now = func() time.Time { return clock }
		return a
	}
	cycle := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics) heldLinePricing {
		t.Helper()
		ctx, logs := observedCtx(t)
		result, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states))
		require.NoError(t, err)
		require.Len(t, result.VariantCapacities, 1)
		entries := logs.FilterMessage("derived-mu").All()
		require.NotEmpty(t, entries, "a decode replica logs its derived mu every cycle")
		m := entries[len(entries)-1].ContextMap()
		var p heldLinePricing
		p.ok, _ = m["ok"].(bool)
		p.held, _ = m["lineHeld"].(bool)
		p.rate, _ = m["rate"].(float64)
		p.a, _ = m["itlA"].(float64)
		p.b, _ = m["itlB"].(float64)
		p.utilization = result.VariantCapacities[0].Utilization
		return p
	}

	first := process()
	for range itl.DefaultMinSamples {
		clock = clock.Add(15 * time.Second)
		cycle(first, replica(0.5, 0.05))
	}
	loadEnd := clock
	var before heldLinePricing
	for clock.Sub(loadEnd) < itl.DefaultObservationMaxAge+2*time.Minute {
		clock = clock.Add(time.Minute)
		before = cycle(first, replica(0.06, 0.01))
	}
	require.True(t, before.ok, "precondition: the line is held through light load")
	require.True(t, before.held, "precondition: the line is a held one, not a fit")

	raw, err := json.Marshal(first.ExportLearnedState())
	require.NoError(t, err)
	var doc LearnedState
	require.NoError(t, json.Unmarshal(raw, &doc))

	// The restart: a new process, the same light load, a minute later; and
	// what the old process would have priced at that minute.
	clock = clock.Add(time.Minute)
	want := cycle(first, replica(0.06, 0.01))
	second := process()
	second.RestoreLearnedState(doc)
	got := cycle(second, replica(0.06, 0.01))

	assert.True(t, got.ok, "the restarted controller derives mu from the held line")
	assert.True(t, got.held, "the restored line is held, as it was before the restart")
	assert.Equal(t, want.a, got.a, "the same slope")
	assert.Equal(t, want.b, got.b, "the same intercept")
	assert.Equal(t, want.rate, got.rate, "the same service rate under the same light load")
	assert.Equal(t, want.utilization, got.utilization, "the same utilization under the same light load")

	// The hold stays bounded from the original fit, not from the restart:
	// past the retention it ends, as it would have without one.
	clock = loadEnd.Add(itl.DefaultObservationMaxAge + second.Horizons().BucketRetention + time.Minute)
	assert.False(t, cycle(second, replica(0.06, 0.01)).ok,
		"a restored slope is held for the retention after it was fitted, no longer")
}
