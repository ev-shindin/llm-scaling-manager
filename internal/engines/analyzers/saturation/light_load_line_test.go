package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// Issue #284: below the ITL band's lower edge a replica offers the window no
// reading, so a decode variant serving light load has no line of its own.
// A variant that learned its line at load must keep it through a light
// stretch longer than the window keeps readings, and one that never learned
// it must say, in the log, that it has no derived mu.
func TestLightLoadKeepsLearnedLineAndNamesMissingOne(t *testing.T) {
	const (
		ns      = "light"
		variant = "qwen32b-decode"
	)
	states := []domain.VariantReplicaState{
		{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	replica := func(k, itlSec float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics("pod-0", variant, int64(k*16000), 16000, 0, 1000, 500)
		rm.Namespace = ns
		rm.KvUsageInstant = k
		rm.AvgITL = itlSec
		return rm
	}

	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	cycle := func(rm domain.ReplicaMetrics) *observer.ObservedLogs {
		t.Helper()
		ctx, logs := observedCtx(t)
		input := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states)
		input.Namespace = ns
		_, err := a.Analyze(ctx, input)
		require.NoError(t, err)
		return logs
	}
	derivedOK := func(logs *observer.ObservedLogs) bool {
		t.Helper()
		entries := logs.FilterMessage("derived-mu").All()
		require.NotEmpty(t, entries, "a decode replica logs its derived mu every cycle")
		ok, _ := entries[len(entries)-1].ContextMap()["ok"].(bool)
		return ok
	}

	// A fresh variant below the band: no line, no derived mu, and a line that
	// says so rather than leaving itl-window's zeros to be read as a fault.
	logs := cycle(replica(0.06, 0.01))
	assert.False(t, derivedOK(logs))
	none := logs.FilterMessage("itl-line-none").All()
	require.Len(t, none, 1, "a variant with no line of its own and none to borrow says so")
	assert.Equal(t, false, none[0].ContextMap()["derivedMu"])
	assert.Equal(t, variant, none[0].ContextMap()["variant"])

	// Load: the window fills and the line is learned.
	for range itl.DefaultMinSamples {
		clock = clock.Add(15 * time.Second)
		logs = cycle(replica(0.5, 0.05))
	}
	require.True(t, derivedOK(logs), "the line is learned at load")
	assert.Empty(t, logs.FilterMessage("itl-line-none").All())

	// Light load for longer than the window keeps a reading. Every reading the
	// line was fitted from ages out; the line must not go with them.
	end := clock.Add(itl.DefaultObservationMaxAge + 2*time.Minute)
	for clock.Before(end) {
		clock = clock.Add(time.Minute)
		logs = cycle(replica(0.06, 0.01))
	}
	lens := a.itlWindowLens()
	require.NotEmpty(t, lens)
	for key, n := range lens {
		assert.Zero(t, n, "%s: the window holds nothing, the premise of the case", key)
	}
	assert.True(t, derivedOK(logs), "a learned line outlives a light stretch (issue #284)")
	assert.Empty(t, logs.FilterMessage("itl-line-none").All())
}
