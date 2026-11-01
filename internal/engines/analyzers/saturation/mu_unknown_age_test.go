package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// A measured mu whose age is unknown resolves to the safe side
// (SwitchMuUnknownAgeSafe; analyzer-learner.md, 3.4).

// unknownAgeRig is an analyzer holding key's window of rate, whose last
// write is the given age, with no sample time and no choice on record: what
// a restore from a document with no sample time leaves.
func unknownAgeRig(t *testing.T, on bool, rate float64, written time.Duration) (*SaturationAnalyzer, time.Time) {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuUnknownAgeSafe: on}))
	ra, ok := capacity.RestoreRollingAverage(capacity.RollingAverageSnapshot{
		Values: []float64{rate, rate, rate}, LastUsed: now.Add(-written), LastWritten: now.Add(-written),
	}, capacity.RollingAverageWindowSize)
	require.True(t, ok)
	a.saturatedThroughput[muRepeatKey] = ra
	return a, now
}

// Under the derived figure, the safe side is the lower measurement: it keeps
// pricing until its own last write is MeasuredLowHoldFor old. Taken as stale
// as can be, it gave way to a derived figure three times it on sight.
func TestMuUnknownAge_UnderDerivedKeepsTheLowHold(t *testing.T) {
	tn := tuningOf(nil)
	derived := derivedMu{ok: true, rate: 3.0}
	for _, tc := range []struct {
		written time.Duration
		on      bool
		yields  bool
	}{
		{10 * time.Minute, true, false},
		{MeasuredLowHoldFor + time.Minute, true, true},
		{10 * time.Minute, false, true}, // as before the switch
	} {
		a, now := unknownAgeRig(t, tc.on, 1.0, tc.written)
		reading := a.saturatedThroughputReading(muRepeatKey)
		require.True(t, reading.sampledAt.IsZero(), "the sample time is unknown")
		got := a.measuredYieldsToDerived(muRepeatKey, reading, derived, 0, tn, now)
		assert.Equal(t, tc.yields, got, "written %s ago, switch %v", tc.written, tc.on)
	}
}

// With no write time either, the measurement under the derived figure is
// held: nothing says it is old. Off, as before, it gave way at once.
func TestMuUnknownAge_NoTimeAtAllIsHeld(t *testing.T) {
	for _, on := range []bool{true, false} {
		a, now := unknownAgeRig(t, on, 1.0, time.Minute)
		got := a.measuredYieldsToDerived(muRepeatKey, throughputReading{rate: 1.0, samples: 3},
			derivedMu{ok: true, rate: 3.0}, 0, tuningOf(nil), now)
		assert.Equal(t, !on, got, "switch %v", on)
	}
}

// Over the derived figure, stale is the safe side: the measurement yields to
// the lower derived figure whatever the switch, however recent its last
// write. A write time is not an agreeing reading, so it never makes a high
// measurement fresh.
func TestMuUnknownAge_OverDerivedIsStillStale(t *testing.T) {
	for _, on := range []bool{true, false} {
		a, now := unknownAgeRig(t, on, 3.0, time.Minute)
		reading := a.saturatedThroughputReading(muRepeatKey)
		got := a.measuredYieldsToDerived(muRepeatKey, reading, derivedMu{ok: true, rate: 1.0}, 0, tuningOf(nil), now)
		assert.True(t, got, "switch %v", on)
	}
}
