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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learn"
)

// analyzeConfirmed runs in, and runs it again when the cycle held a k2
// reading (k2_confirm.go), returning the last result: a window's first
// reading waits for a later cycle's, so a test about something else that
// learns k2 from one saturated reading confirms it with the same reading, on
// a second Analyze at the same instant. A cycle that held nothing is run
// once.
func analyzeConfirmed(ctx context.Context, a *SaturationAnalyzer, in domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	res, err := a.Analyze(ctx, in)
	if err != nil {
		return res, err
	}
	a.mu.Lock()
	held := false
	for _, p := range a.k2Pending {
		held = held || p.cycle == a.analyzeSeq
	}
	a.mu.Unlock()
	if !held {
		return res, nil
	}
	return a.Analyze(ctx, in)
}

// analyzeAfterHold runs in once with its log discarded -- the cycle that
// holds the window's first reading (k2_confirm.go) -- and again on ctx,
// returning the second result: a test of what one cycle logs reads the
// confirming cycle's lines alone.
func analyzeAfterHold(ctx context.Context, a *SaturationAnalyzer, in domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if _, err := a.Analyze(logr.NewContext(ctx, logr.Discard()), in); err != nil {
		return nil, err
	}
	return a.Analyze(ctx, in)
}

// holdRig is an analyzer whose k2 hold is driven directly, cycle by cycle.
type holdRig struct {
	a   *SaturationAnalyzer
	now time.Time
}

func newHoldRig() *holdRig {
	r := &holdRig{a: NewSaturationAnalyzer(capacity.NewStore()), now: propStart}
	r.a.now = func() time.Time { return r.now }
	return r
}

// cycle starts the next Analyze call, step after the last.
func (r *holdRig) cycle(step time.Duration) {
	r.now = r.now.Add(step)
	r.a.analyzeSeq++
}

func (r *holdRig) hold(key string, reading int64, args string) (bool, k2PendingReadings) {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	return r.a.k2HoldFirstLocked(key, reading, true, args, r.now)
}

func (r *holdRig) pending(key string) []int64 {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	if p, ok := r.a.k2Pending[key]; ok {
		return p.readings
	}
	return nil
}

// A sibling's reading in the same cycle is held beside the first, not taken
// as its confirmation; the next cycle's writes both.
func TestK2Hold_ASiblingInTheSameCycleDoesNotConfirm(t *testing.T) {
	r := newHoldRig()
	r.cycle(0)
	held, _ := r.hold("k", 30_000, "")
	require.True(t, held)
	held, _ = r.hold("k", 50_000, "")
	assert.True(t, held, "a reading in the same cycle is held too")
	assert.Equal(t, []int64{30_000, 50_000}, r.pending("k"))
	r.cycle(propCycle)
	held, confirmed := r.hold("k", 40_000, "")
	assert.False(t, held)
	assert.Equal(t, []int64{30_000, 50_000}, confirmed.readings)
	assert.Equal(t, propStart, confirmed.at, "written at when they were read")
	assert.Empty(t, r.pending("k"))
}

// Held readings past the episode gap, or under engine arguments the variant
// no longer runs, are replaced by the new reading, not written with it.
func TestK2Hold_ReadingsPastTheGapOrUnderOtherArgumentsAreReplaced(t *testing.T) {
	r := newHoldRig()
	r.cycle(0)
	_, _ = r.hold("k", 10_000, "args-a")
	r.cycle(propCycle)
	held, confirmed := r.hold("k", 40_000, "args-b")
	assert.True(t, held, "a reading under other arguments does not confirm")
	assert.Empty(t, confirmed.readings)
	assert.Equal(t, []int64{40_000}, r.pending("k"), "the reading held under args-a is dropped")

	r.cycle(r.a.horizons.EpisodeGap + time.Minute)
	held, _ = r.hold("k", 20_000, "args-b")
	assert.True(t, held, "past the episode gap nothing confirms")
	assert.Equal(t, []int64{20_000}, r.pending("k"))
}

// A window that is not fresh takes the reading at once, and drops anything
// held for the key.
func TestK2Hold_AWindowOnRecordIsNotHeld(t *testing.T) {
	r := newHoldRig()
	r.cycle(0)
	_, _ = r.hold("k", 10_000, "")
	r.a.mu.Lock()
	held, _ := r.a.k2HoldFirstLocked("k", 30_000, false, "", r.now)
	_, kept := r.a.k2Pending["k"]
	r.a.mu.Unlock()
	assert.False(t, held)
	assert.False(t, kept)
}

// The sweep drops held readings past the episode gap, and a forget drops
// the variant's.
func TestK2Hold_TheSweepAndAForgetDropHeldReadings(t *testing.T) {
	r := newHoldRig()
	key := "ns|v|m|H200|1|decode|short|q5"
	r.cycle(0)
	_, _ = r.hold(key, 10_000, "")
	r.cycle(time.Hour)
	r.a.mu.Lock()
	r.a.sweepK2PendingLocked(r.now)
	r.a.mu.Unlock()
	require.NotEmpty(t, r.pending(key), "inside the gap the sweep keeps it")
	r.cycle(r.a.horizons.EpisodeGap)
	r.a.mu.Lock()
	r.a.sweepK2PendingLocked(r.now)
	r.a.mu.Unlock()
	assert.Empty(t, r.pending(key), "past the gap the sweep drops it")

	_, _ = r.hold(key, 10_000, "")
	r.a.mu.Lock()
	r.a.forgetK2Locked("ns", "m", "v", learn.ByOperator)
	r.a.mu.Unlock()
	assert.Empty(t, r.pending(key), "a forget drops it")
}
