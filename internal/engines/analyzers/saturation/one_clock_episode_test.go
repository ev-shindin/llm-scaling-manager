package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The EpisodeGap negative controls of the one clock (one_clock_test.go): they
// age a window on the analyzer's clock alone, a.now, which the parent of the
// one clock already had, so they run against it and fail there.

var episodeT0 = time.Date(2026, 10, 23, 11, 18, 0, 0, time.UTC)

// fullReplicaCycle runs one Analyze over one decode replica that is full and
// queued -- the reading the k2 window records on sight -- and returns the
// result.
func fullReplicaCycle(t *testing.T, a *SaturationAnalyzer) *domain.AnalyzerResult {
	t.Helper()
	rm := makeReplicaMetrics("pod-s", "variant-s", 260_000, 376_480, 160, 1000, 500)
	rm.GenerationTokenRate = 3000
	rm.RequestRate = 6
	result, err := a.Analyze(logr.NewContext(context.Background(), logr.Discard()), makeAnalyzerInput([]domain.ReplicaMetrics{rm},
		[]domain.VariantReplicaState{
			{VariantName: "variant-s", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
		}))
	require.NoError(t, err)
	return result
}

// onlyWindow returns the one window in m.
func onlyWindow(t *testing.T, m map[string]*capacity.RollingAverage) *capacity.RollingAverage {
	t.Helper()
	require.Len(t, m, 1, "precondition: one window")
	for _, ra := range m {
		return ra
	}
	return nil
}

// The negative control the one clock exists for: a window written, then a
// fake clock moved past EpisodeGap, then written again, starts a new episode.
// While the window stamped its writes on the wall clock, the fake clock's
// jump was invisible to the gap check and the two episodes blended.
func TestOneClock_EpisodeGapOnTheAnalyzerClockResetsTheK2Window(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := episodeT0
	a.now = func() time.Time { return now }
	for i := range 3 {
		now = episodeT0.Add(time.Duration(i) * 15 * time.Second)
		fullReplicaCycle(t, a)
	}
	require.Equal(t, 3, onlyWindow(t, a.computeCapacityHistory).Len(), "precondition: one sample per full cycle")

	// Unseen that long, the replica is a new sighting on the settle tracker
	// and waits a window before its reading is written; the new window's
	// first reading waits for the next (k2_confirm.go).
	now = now.Add(capacity.EpisodeGap + time.Minute)
	for i := range 6 {
		fullReplicaCycle(t, a)
		if i < 5 {
			now = now.Add(15 * time.Second)
		}
	}
	assert.Equal(t, 2, onlyWindow(t, a.computeCapacityHistory).Len(),
		"a write past EpisodeGap on the analyzer's clock starts a new k2 window")
}

func TestOneClock_EpisodeGapOnTheAnalyzerClockResetsTheMuWindow(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := episodeT0
	a.now = func() time.Time { return now }
	for i := range 5 {
		now = episodeT0.Add(time.Duration(i) * ThroughputSampleSpacing)
		a.recordSaturatedThroughput("k", 3+float64(i)/10)
	}
	require.Equal(t, 5, a.saturatedThroughput["k"].Len(), "precondition: one sample per spacing")

	now = now.Add(capacity.EpisodeGap + time.Minute)
	a.recordSaturatedThroughput("k", 4)
	assert.Equal(t, 1, a.saturatedThroughput["k"].Len(),
		"a write past EpisodeGap on the analyzer's clock starts a new mu window")
	assert.Equal(t, 4.0, a.saturatedThroughput["k"].Median())
}
