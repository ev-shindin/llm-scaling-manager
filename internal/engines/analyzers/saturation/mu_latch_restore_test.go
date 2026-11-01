package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Mu's latch is not persisted: a window restored with no choice on record
// has a latch with no evidence behind it, which restores to the safe side
// (SwitchMuLatchRestoresSafe; analyzer-learner.md, 3.5 and the mu choice).

// restoredMuRig is an analyzer holding key's window of rate, its newest
// sample stale (FreshFor and LowHoldFor past), with no choice on record:
// what a controller restart with the snapshot on leaves.
func restoredMuRig(t *testing.T, on bool, rate float64) (*SaturationAnalyzer, *time.Time) {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuLatchRestoresSafe: on}))
	old := now.Add(-MeasuredLowHoldFor - time.Minute)
	ra, ok := capacity.RestoreRollingAverage(capacity.RollingAverageSnapshot{
		Values: []float64{rate, rate, rate}, LastUsed: old, LastWritten: old,
	}, capacity.RollingAverageWindowSize)
	require.True(t, ok)
	a.saturatedThroughput[muRepeatKey] = ra
	a.throughputSampledAt[muRepeatKey] = old
	return a, &now
}

// yields asks the choice with the measurement at ratio times the derived
// figure of 1.0, and served the highest served rate this cycle.
func yields(a *SaturationAnalyzer, now time.Time, ratio, served float64) bool {
	reading := throughputReading{rate: ratio, samples: 3, sampledAt: a.throughputSampledAt[muRepeatKey],
		writtenAt: a.saturatedThroughput[muRepeatKey].LastWritten()}
	return a.measuredYieldsToDerived(muRepeatKey, reading, derivedMu{ok: true, rate: 1.0}, served, tuningOf(nil), now)
}

// A stale measurement 1.8x the derived figure, between the return ratio and
// the ratio: restored unlatched, it priced from the higher, measured figure.
// Restored latched, it yields to the lower one.
func TestMuLatch_ARestoredMeasurementOverDerivedComesBackLatched(t *testing.T) {
	for _, on := range []bool{true, false} {
		a, now := restoredMuRig(t, on, 1.8)
		assert.Equal(t, on, yields(a, *now, 1.8, 0), "switch %v", on)
	}
}

// Under the derived figure the safe side is the lower measurement: restored
// unlatched, so a measurement at 0.55x (inside the return band) keeps
// pricing, and a served rate between the return ratio and the ratio times
// it does not open the served yield.
func TestMuLatch_ARestoredMeasurementUnderDerivedComesBackUnlatched(t *testing.T) {
	for _, on := range []bool{true, false} {
		a, now := restoredMuRig(t, on, 0.55)
		assert.False(t, yields(a, *now, 0.55, 0), "switch %v: stale low", on)
		assert.False(t, yields(a, *now, 0.55, 0.55*1.8), "switch %v: served 1.8x", on)
	}
}

// Once the restored latch has decided a cycle, it is known, and the
// hysteresis runs as it does live: latched it holds past the return ratio,
// and once out it needs the full ratio again.
func TestMuLatch_TheRestoredLatchIsKnownAfterItsFirstChoice(t *testing.T) {
	a, now := restoredMuRig(t, true, 1.8)
	for _, step := range []struct {
		ratio  float64
		yields bool
	}{
		{1.8, true},  // restored latched: yields past the return ratio
		{1.7, true},  // still latched
		{1.5, false}, // back under the return ratio: out
		{1.8, false}, // known unlatched: needs the full ratio
		{2.1, true},
	} {
		*now = now.Add(15 * time.Second)
		assert.Equal(t, step.yields, yields(a, *now, step.ratio, 0), "at %.1fx", step.ratio)
	}
}

// A window this analyzer saw from its first sample has a known latch, and
// a stale measurement 1.8x the derived figure keeps pricing as before.
func TestMuLatch_AWindowSeenFromItsFirstSampleIsKnown(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	tn := tuningOf(nil)
	a.noteSaturatedReading(muRepeatKey, "pod-a", 1.8, false, tn, now)
	a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 1.8)
	now = now.Add(MeasuredMuFreshFor + time.Minute)
	reading := a.saturatedThroughputReading(muRepeatKey)
	assert.False(t, a.measuredYieldsToDerived(muRepeatKey, reading, derivedMu{ok: true, rate: 1.0}, 0, tn, now))
}
