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
	held, confirmed, _ := r.holdDropping(key, reading, args)
	return held, confirmed
}

func (r *holdRig) holdDropping(key string, reading int64, args string) (bool, k2PendingReadings, k2PendingReadings) {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	return r.a.k2HoldFirstLocked(key, reading, true, args, r.now)
}

// onRecord is what is held for key "k" on record for a reading under args.
func (r *holdRig) onRecord(args string) float64 {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	return r.a.heldOnRecordLocked("k", args, r.now)
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
	assert.InDelta(t, 10_000, r.onRecord("args-a"), 0.5, "on record for a reading under the same arguments")
	assert.Zero(t, r.onRecord("args-b"), "and not for one under other arguments")
	held, confirmed, dropped := r.holdDropping("k", 40_000, "args-b")
	assert.True(t, held, "a reading under other arguments does not confirm")
	assert.Empty(t, confirmed.readings)
	assert.Equal(t, []int64{10_000}, dropped.readings, "the reading held under args-a is dropped, for the caller to log")
	assert.Equal(t, []int64{40_000}, r.pending("k"))

	r.cycle(r.a.horizons.EpisodeGap + time.Minute)
	assert.Zero(t, r.onRecord("args-b"), "past the episode gap nothing held is on record")
	held, _, dropped = r.holdDropping("k", 20_000, "args-b")
	assert.True(t, held, "past the episode gap nothing confirms")
	assert.Equal(t, []int64{40_000}, dropped.readings)
	assert.Equal(t, []int64{20_000}, r.pending("k"))
	assert.Zero(t, r.onRecord("args-b"), "a reading held this cycle is not on record for this cycle's")
}

// A window that is not fresh takes the reading at once, and drops anything
// held for the key.
func TestK2Hold_AWindowOnRecordIsNotHeld(t *testing.T) {
	r := newHoldRig()
	r.cycle(0)
	_, _ = r.hold("k", 10_000, "")
	r.a.mu.Lock()
	held, _, _ := r.a.k2HoldFirstLocked("k", 30_000, false, "", r.now)
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

// With SwitchK2FirstReadingWaits off, a window's first reading is written
// and prices at once, as before the hold.
func TestK2Hold_TheSwitchOffWritesTheFirstReadingAtOnce(t *testing.T) {
	for _, on := range []bool{true, false} {
		a := NewSaturationAnalyzer(capacity.NewStore())
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2FirstReadingWaits: on}))
		_, err := a.Analyze(context.Background(), makeAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)},
			[]domain.VariantReplicaState{{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1}}))
		require.NoError(t, err)
		want := 1
		if on {
			want = 0
		}
		assert.Equal(t, want, k2Windows(a), "switch on=%v: windows written by the first saturated cycle", on)
	}
}

// A k2 reading held as its window's first (k2_confirm.go) while a mu key swap
// is pending is frozen like a P1-obs one (#415's muSwapFrozen): its mu is not
// written under the bucket the key is leaving, and the refusal is counted.
func TestK2Hold_AHeldReadingDuringAPendingSwapLeavesTheMuWindowFrozen(t *testing.T) {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, propStart)
	a := NewSaturationAnalyzer(store)
	clock := propStart
	a.now = func() time.Time { return clock }
	run := func(out, rate float64) {
		t.Helper()
		rm := makeReplicaMetrics("pod-a", "variant-a", 12000, 16000, 6, 100, out)
		rm.RequestRate, rm.GenerationTokenRate, rm.Ready = rate, rate*out, true
		_, err := a.Analyze(context.Background(), makeAnalyzerInput([]domain.ReplicaMetrics{rm}, mtOneVariant))
		require.NoError(t, err)
		clock = clock.Add(propCycle)
	}
	long := func() []float64 {
		a.mu.Lock()
		defer a.mu.Unlock()
		for k, ra := range a.saturatedThroughput {
			if bucketOf(k) == longBucket {
				return ra.Snapshot().Values
			}
		}
		return nil
	}
	for range 12 {
		run(530, 3)
	}
	before := long()
	require.NotEmpty(t, before, "precondition: mu measured in long")
	// The shape moves, and the swap is pending from its first cycle. Every
	// k2 window is gone, so that cycle's saturated reading is held.
	a.mu.Lock()
	clear(a.computeCapacityHistory)
	a.mu.Unlock()
	run(5500, 0.5)
	a.mu.Lock()
	pending := !a.fleetShape["test-ns|test-model"].k2Key.swapSince.IsZero()
	_, held := a.k2Pending[firstPendingKey(a)]
	a.mu.Unlock()
	require.True(t, pending, "premise: the swap is pending")
	require.True(t, held, "premise: the reading is held")
	assert.Equal(t, before, long(), "the held reading's mu was written under the bucket being left")
	var counted int64
	for _, r := range a.LearnerReports()["test-ns|test-model"] {
		if r.Learner == learn.Mu {
			counted += r.Counts[muReasonSwapPending]
		}
	}
	assert.Equal(t, int64(1), counted, "the frozen write is counted")
}

// firstPendingKey is a key with readings held, "" when none. Callers hold a.mu.
func firstPendingKey(a *SaturationAnalyzer) string {
	for k := range a.k2Pending {
		return k
	}
	return ""
}
