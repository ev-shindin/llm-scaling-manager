package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// An exact repeat of mu's last reading past the sample spacing is use, not a
// write (SwitchMuRepeatIsUse; analyzer-learner.md, 3.4).

const muRepeatKey = "ns|v|m|H200|1|decode|imedium|medium|q5"

// muRepeatRig is an analyzer on a clock the test moves, with
// SwitchMuRepeatIsUse set to on.
func muRepeatRig(t *testing.T, on bool) (*SaturationAnalyzer, *time.Time) {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuRepeatIsUse: on}))
	return a, &clock
}

func TestMuRepeat_IsUseNotAWrite(t *testing.T) {
	for _, on := range []bool{true, false} {
		a, clock := muRepeatRig(t, on)
		t0 := *clock
		require.Equal(t, muSampled, a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 3.4))
		*clock = t0.Add(ThroughputSampleSpacing)
		require.Equal(t, muRepeated, a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 3.4))
		ra := a.saturatedThroughput[muRepeatKey]
		assert.Equal(t, 1, ra.Len(), "switch %v: a repeat starts no sample", on)
		assert.False(t, ra.StaleAt(*clock, 0), "switch %v: a repeat is use", on)
		want := t0
		if !on {
			want = *clock // as before the switch: taken as a write
		}
		assert.Equal(t, want, ra.LastWritten(), "switch %v: the window's write clock", on)
	}
}

// A frozen series repeats one value for good. Taken as a write it kept the
// window's episode alive however long it went on; taken as use, the episode
// ends EpisodeGap after the last real reading, and the next reading starts a
// window of its own.
func TestMuRepeat_AFrozenSeriesLetsTheEpisodeEnd(t *testing.T) {
	for _, on := range []bool{true, false} {
		a, clock := muRepeatRig(t, on)
		t0 := *clock
		a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 3.4)
		for *clock = t0.Add(ThroughputSampleSpacing); clock.Sub(t0) <= capacity.EpisodeGap+time.Hour; *clock = clock.Add(15 * time.Minute) {
			a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 3.4)
		}
		a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 2.0)
		ra := a.saturatedThroughput[muRepeatKey]
		if on {
			assert.Equal(t, 1, ra.Len(), "a new episode: the frozen figure is not carried")
			assert.Equal(t, 2.0, ra.Median())
		} else {
			assert.Equal(t, 2, ra.Len(), "as before the switch: the repeats kept the episode")
		}
	}
}

// Over the derived figure an exact repeat does not keep the measurement
// fresh: freshness is the last saturated reading that agreed with it, a
// repeat is not a reading of saturation, and stale over the derived figure
// the measurement yields to the lower one, the safe side.
func TestMuRepeat_OverDerivedDoesNotKeepTheMeasurementFresh(t *testing.T) {
	tn := tuningOf(nil)
	for _, on := range []bool{true, false} {
		a, clock := muRepeatRig(t, on)
		t0 := *clock
		a.noteSaturatedReading(muRepeatKey, "pod-a", 3.4, false, 3.0, tn, *clock)
		a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 3.4)
		*clock = t0.Add(5 * time.Minute)
		a.noteSaturatedReading(muRepeatKey, "pod-a", 3.4, false, 3.0, tn, *clock)
		got := a.throughputMuChoice[muRepeatKey].saturatedAt
		if on {
			assert.True(t, got.IsZero(), "a repeat is not agreement: the first reading had no window to agree with")
		} else {
			assert.Equal(t, *clock, got, "as before the switch: a repeat refreshed the measurement")
		}
		// A reading that differs, and agrees, still refreshes it.
		*clock = t0.Add(6 * time.Minute)
		a.noteSaturatedReading(muRepeatKey, "pod-a", 3.5, false, 3.0, tn, *clock)
		assert.Equal(t, *clock, a.throughputMuChoice[muRepeatKey].saturatedAt, "switch %v", on)
	}
}

// Under the derived figure an exact repeat still counts as agreement:
// stale, a real low measurement whose quantized rate repeats would give way
// to the higher derived figure after MeasuredLowHoldFor, the side that
// under-orders. Repeated for longer than the hold, it keeps pricing.
func TestMuRepeat_UnderDerivedStillKeepsTheMeasurementFresh(t *testing.T) {
	tn := tuningOf(nil)
	derived := derivedMu{ok: true, rate: 3.0}
	for _, on := range []bool{true, false} {
		a, clock := muRepeatRig(t, on)
		t0 := *clock
		a.noteSaturatedReading(muRepeatKey, "pod-a", 1.2, false, derived.rate, tn, *clock)
		a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 1.2)
		for *clock = t0.Add(ThroughputSampleSpacing); clock.Sub(t0) <= MeasuredLowHoldFor+5*time.Minute; *clock = clock.Add(time.Minute) {
			a.noteSaturatedReading(muRepeatKey, "pod-a", 1.2, false, derived.rate, tn, *clock)
			a.recordSaturatedThroughputFrom(muRepeatKey, "pod-a", 1.2)
		}
		assert.Equal(t, clock.Add(-time.Minute), a.throughputMuChoice[muRepeatKey].saturatedAt, "switch %v", on)
		reading := a.saturatedThroughputReading(muRepeatKey)
		assert.False(t, a.measuredYieldsToDerived(muRepeatKey, reading, derived, 0, tn, *clock),
			"switch %v: the low measurement keeps pricing", on)
	}
}
