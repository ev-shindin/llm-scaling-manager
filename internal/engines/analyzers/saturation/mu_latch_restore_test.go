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
	a.noteSaturatedReading(muRepeatKey, "pod-a", 1.8, false, 1.0, tn, now)
	a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 1.8)
	now = now.Add(MeasuredMuFreshFor + time.Minute)
	reading := a.saturatedThroughputReading(muRepeatKey)
	assert.False(t, a.measuredYieldsToDerived(muRepeatKey, reading, derivedMu{ok: true, rate: 1.0}, 0, tn, now))
}

// A window installed through the real restore path after a refused reading
// already made the key a choice memo: the memo says nothing of the window,
// whose latch is unknown, so a stale restored measurement 1.8x the derived
// figure yields to the lower figure, as it does with no memo at all.
func TestMuLatch_AWindowRestoredAfterARefusedReadingComesBackLatched(t *testing.T) {
	params := mtParams(64, "vllm:v1")
	for _, on := range []bool{true, false} {
		a, _, clock := waProcess(params)
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuLatchRestoresSafe: on}))
		const key = "test-ns|variant-a|test-model|H100|1|decode|imedium|medium|q5"
		old := clock.Add(-MeasuredLowHoldFor - time.Minute)
		a.RestoreLearnedState(LearnedState{
			Config: map[string]string{"test-ns/variant-a": params.Fingerprint()},
			Args:   map[string]string{"test-ns/variant-a": params.ArgsFingerprint()},
			Throughput: map[string]ThroughputSnapshot{key: {
				RollingAverageSnapshot: capacity.RollingAverageSnapshot{Values: []float64{1.8, 1.8, 1.8}, LastUsed: old, LastWritten: old},
				SampledAt:              old,
			}},
		})
		// A refused reading on the cycle before the variant's record lets the
		// restore install: a memo, with no window on record.
		a.noteSaturatedReading(key, "pod-a", 0.1, false, 1.0, tuningOf(nil), *clock)
		_, done := a.installRestored("test-ns", "test-model", "variant-a")
		require.True(t, done, "premise: the restore installs")
		reading := a.saturatedThroughputReading(key)
		require.InDelta(t, 1.8, reading.rate, 1e-9, "premise: the restored window prices")
		got := a.measuredYieldsToDerived(key, reading, derivedMu{ok: true, rate: 1.0}, 0, tuningOf(nil), *clock)
		assert.Equal(t, on, got, "switch %v", on)
	}
}

// A window brought back from under other engine arguments has an unknown
// latch too, whatever memo the key made meanwhile.
func TestMuLatch_AWindowBroughtBackFromTheStashComesBackLatched(t *testing.T) {
	for _, on := range []bool{true, false} {
		a := NewSaturationAnalyzer(capacity.NewStore())
		now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return now }
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuLatchRestoresSafe: on}))
		old := now.Add(-MeasuredLowHoldFor - time.Minute)
		ra, ok := capacity.RestoreRollingAverage(capacity.RollingAverageSnapshot{
			Values: []float64{1.8, 1.8, 1.8}, LastUsed: old, LastWritten: old,
		}, capacity.RollingAverageWindowSize)
		require.True(t, ok)
		a.throughputStash[stashKey(muRepeatKey, "args-a")] = stashedThroughput{window: ra, sampledAt: old}
		a.noteSaturatedReading(muRepeatKey, "pod-a", 0.1, false, 1.0, tuningOf(nil), now)
		a.mu.Lock()
		a.throughputWindowUnderLocked(muRepeatKey, "args-a")
		a.mu.Unlock()
		reading := a.saturatedThroughputReading(muRepeatKey)
		require.InDelta(t, 1.8, reading.rate, 1e-9, "premise: the window is back")
		got := a.measuredYieldsToDerived(muRepeatKey, reading, derivedMu{ok: true, rate: 1.0}, 0, tuningOf(nil), now)
		assert.Equal(t, on, got, "switch %v", on)
	}
}

// The same through installPreviousK2Locked: a document from other engine
// arguments installs its throughput window as the variant's previous
// figure, after a refused reading made the key a memo.
func TestMuLatch_AWindowRestoredAsAPriorAfterARefusedReadingComesBackLatched(t *testing.T) {
	stored := mtParams(256, "vllm:v1")
	for _, on := range []bool{true, false} {
		a, _, clock := waProcess(mtParams(64, "vllm:v1"))
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuLatchRestoresSafe: on}))
		const key = "test-ns|variant-a|test-model|H100|1|decode|imedium|medium|q5"
		old := clock.Add(-MeasuredLowHoldFor - time.Minute)
		a.RestoreLearnedState(LearnedState{
			Config: map[string]string{"test-ns/variant-a": stored.Fingerprint()},
			Args:   map[string]string{"test-ns/variant-a": stored.ArgsFingerprint()},
			Throughput: map[string]ThroughputSnapshot{key: {
				RollingAverageSnapshot: capacity.RollingAverageSnapshot{Values: []float64{1.8, 1.8, 1.8}, LastUsed: old, LastWritten: old},
				SampledAt:              old,
			}},
		})
		a.noteSaturatedReading(key, "pod-a", 0.1, false, 1.0, tuningOf(nil), *clock)
		r, done := a.installRestored("test-ns", "test-model", "variant-a")
		require.True(t, done, "premise: the restore runs")
		require.Equal(t, RestoredPrior, r.Outcome, "premise: through installPreviousK2Locked")
		require.Equal(t, 1, r.Throughput, "premise: the throughput window is installed")
		reading := a.saturatedThroughputReading(key)
		require.InDelta(t, 1.8, reading.rate, 1e-9)
		got := a.measuredYieldsToDerived(key, reading, derivedMu{ok: true, rate: 1.0}, 0, tuningOf(nil), *clock)
		assert.Equal(t, on, got, "switch %v", on)
	}
}
