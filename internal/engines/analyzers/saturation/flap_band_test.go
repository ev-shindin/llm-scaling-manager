package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The idle band and the k2 key's bucket band (#396), each with its replay
// switch off as the behaviour before it.

// idleBandRig notes one decode replica's readings, one cycle apart, and
// returns whether it is settled idle room after each.
type idleBandRig struct {
	a   *SaturationAnalyzer
	now time.Time
}

func newIdleBandRig(t *testing.T, band bool) *idleBandRig {
	a := NewSaturationAnalyzer(capacity.NewStore())
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchIdleBand: band}))
	return &idleBandRig{a: a, now: propStart}
}

// read notes the replica at frac of its cache for d, one cycle at a time, and
// returns the last verdict.
func (r *idleBandRig) read(frac float64, d time.Duration) bool {
	rm := satRow("r0", int64(frac*float64(propKv)), 0, 0.5)
	caps := []capacity.ReplicaCapacity{{VariantName: "v", EffectiveCapacity: propKv}}
	t := tuning{AnalyzerTuning: tuningDefault.WithDefaults(), idleKvUsage: config.DefaultIdleKvUsage}
	var v bool
	for end := r.now.Add(d); !r.now.After(end); r.now = r.now.Add(propCycle) {
		v = r.a.noteIdleRuns("ns", "m", caps, []domain.ReplicaMetrics{rm}, map[string]string{"v": domain.RoleDecode}, t, r.now)[0]
	}
	return v
}

func TestIdleBandLevel(t *testing.T) {
	assert.InDelta(t, 0.42, idleBandLevel(0.30), 1e-12, "IdleLeaveRatio times the line")
	assert.InDelta(t, 0.90, idleBandLevel(0.80), 1e-12, "never more than halfway from the line to a full replica")
}

// A run starts under the line, and once started goes on up to the band's top.
func TestIdleBand_ARunStartsUnderTheLineAndEndsAtTheTop(t *testing.T) {
	r := newIdleBandRig(t, true)
	require.False(t, r.read(0.35, 3*time.Minute), "0.35 is over the 0.30 line: no run starts")
	require.True(t, r.read(0.29, IdleSettledFor), "under the line for IdleSettledFor: settled")
	assert.True(t, r.read(0.40, 3*time.Minute), "0.40 is under the band's top (0.42): the run goes on")
	assert.False(t, r.read(0.43, 0), "0.43 is over the band's top: the run ends")
	assert.False(t, r.read(0.40, 3*time.Minute), "a run ended: 0.40 does not start another")
}

// The band does not stretch the dwell: a run that starts under the line still
// waits IdleSettledFor, and a queue ends it whatever its KV.
func TestIdleBand_KeepsTheDwellAndTheQueue(t *testing.T) {
	r := newIdleBandRig(t, true)
	assert.False(t, r.read(0.29, IdleSettledFor-propCycle), "under IdleSettledFor: not yet settled")
	assert.True(t, r.read(0.29, 0))
	rm := satRow("r0", int64(0.10*float64(propKv)), 1, 0.5)
	caps := []capacity.ReplicaCapacity{{VariantName: "v", EffectiveCapacity: propKv}}
	tn := tuning{AnalyzerTuning: tuningDefault.WithDefaults(), idleKvUsage: config.DefaultIdleKvUsage}
	r.now = r.now.Add(propCycle)
	assert.False(t, r.a.noteIdleRuns("ns", "m", caps, []domain.ReplicaMetrics{rm}, map[string]string{"v": domain.RoleDecode}, tn, r.now)[0],
		"a replica with a request waiting is not idle, in a run or not")
}

// SwitchIdleBand off: every reading is held to the line, as before the band.
func TestIdleBand_TheSwitchOffHoldsEveryReadingToTheLine(t *testing.T) {
	r := newIdleBandRig(t, false)
	require.True(t, r.read(0.29, IdleSettledFor))
	assert.False(t, r.read(0.31, 0), "over the line ends the run with the band off")
}

func TestOutputBucketBounds(t *testing.T) {
	for _, c := range []struct{ l, lo, hi float64 }{
		{0, 0, 100}, {99, 0, 100}, {100, 100, 500}, {499, 100, 500}, {500, 500, 1500},
		{2000, 1500, 3000}, {5999, 3000, 6000}, {6000, 6000, 0}, {1e6, 6000, 0},
	} {
		lo, hi := outputBucketBounds(c.l)
		assert.Equal(t, [2]float64{c.lo, c.hi}, [2]float64{lo, hi}, "length %g", c.l)
	}
}

// keyRig steps a k2KeyMemo one cycle at a time and returns the key's bucket.
type keyRig struct {
	m  k2KeyMemo
	at time.Time
}

func (r *keyRig) step(l float64) string {
	r.at = r.at.Add(propCycle)
	got, _ := r.m.note(l, r.at)
	return classifyOutputLength(got)
}

// steps notes l for n cycles and returns the last bucket.
func (r *keyRig) steps(l float64, n int) string {
	got := ""
	for range n {
		got = r.step(l)
	}
	return got
}

// settledKey is a keyRig that has read l for K2KeyBucketSmoothing.
func settledKey(l float64) *keyRig {
	r := &keyRig{at: propStart}
	r.steps(l, int(K2KeyBucketSmoothing/propCycle))
	return r
}

// dwellCycles is how many cycles past K2KeyBucketSwap keep the bucket: the
// one after them swaps.
const dwellCycles = int(K2KeyBucketDwell / propCycle)

// One early reading does not choose the bucket: the key is judged on the mean
// of the readings so far, and leaves the first reading's bucket when that
// mean is past the margin.
func TestK2KeyBucket_OneEarlyReadingDoesNotChooseTheBucket(t *testing.T) {
	r := &keyRig{at: propStart}
	assert.Equal(t, "medium", r.step(480), "one reading: its own bucket")
	assert.Equal(t, "medium", r.step(540), "the mean, 510, is long's but under 515")
	assert.Equal(t, "long", r.step(540), "the mean, 520, is past 515")
	assert.Equal(t, "long", r.steps(530, 40))
}

// The smoothed length leaves the bucket only once it is past the margin: a
// length that moves to 470 is held in "long" for minutes (about 14), and
// leaves once the smoothed length is under 485.
func TestK2KeyBucket_TheSmoothedLengthLeavesPastTheMargin(t *testing.T) {
	r := settledKey(530)
	assert.Equal(t, "long", r.steps(470, 40), "10 min at 470: the smoothed length is still over 485")
	assert.Equal(t, "medium", r.steps(470, 20), "5 min more: under 485")
	assert.Equal(t, "medium", r.steps(510, 4), "back at 510: under 515, held")
}

// No reading moves the key on its own, however far past the boundary, in
// either direction.
func TestK2KeyBucket_OneReadingNeverMovesIt(t *testing.T) {
	r := settledKey(530)
	assert.Equal(t, "long", r.step(300), "one reading 40 % under 500")
	assert.Equal(t, "long", r.step(530))
	assert.Equal(t, "long", r.step(4000), "one reading several buckets up")
	assert.Equal(t, "long", r.step(530))
	r = settledKey(470)
	assert.Equal(t, "medium", r.step(700), "one reading 40 % over 500")
	assert.Equal(t, "medium", r.step(470))
}

// A swap, the length past the boundary by K2KeyBucketSwap on every cycle for
// K2KeyBucketDwell, moves the key to the length's own bucket, the same way in
// both directions, and is not sticky: the smoothing restarts there.
func TestK2KeyBucket_ASwapMovesItTheSameWayBothWays(t *testing.T) {
	r := settledKey(530)
	assert.Equal(t, "long", r.steps(380, dwellCycles), "380 for under the dwell: held")
	assert.Equal(t, "medium", r.step(380), "380 for the dwell: a swap down")
	r = settledKey(470)
	assert.Equal(t, "medium", r.steps(700, dwellCycles), "700 for under the dwell: held, though the smoothed length is past 515")
	assert.Equal(t, "long", r.step(700), "700 for the dwell: a swap up")
	assert.Equal(t, "long", r.step(300), "one reading after a swap moves nothing")

	r = settledKey(530)
	r.steps(380, dwellCycles+1)
	assert.Equal(t, "long", r.steps(530, 12), "a swap on noise: back at 530, the restarted mean is past 515 within minutes")

	r = settledKey(1000)
	assert.Equal(t, "huge", r.steps(8000, dwellCycles+1), "a swap several buckets up lands in its own bucket, not the next one")
	assert.Equal(t, "short", settledKey(1000).steps(50, dwellCycles+1), "and down")
}

// While a swap is pending the key holds its bucket, even when the smoothed
// length passes the margin of the bucket next door: a swap lands in the
// length's own bucket, and does not stop in the ones between. A fresh memo
// (one reading at 1000) smooths fast, so 3500 takes its mean past 1545 on the
// first cycle, inside the swap's dwell.
func TestK2KeyBucket_APendingSwapHoldsTheBucket(t *testing.T) {
	r := &keyRig{at: propStart}
	r.step(1000)
	for i := range dwellCycles {
		assert.Equal(t, "long", r.step(3500), "cycle %d of a pending swap: held in long, not xlong", i)
	}
	assert.Equal(t, "xxlong", r.step(3500), "the swap lands in the length's own bucket")
}

// note reports a move of the bucket, from the length it left, and whether a
// swap made it.
func TestK2KeyBucket_NoteReportsTheMove(t *testing.T) {
	r := settledKey(530)
	at := r.at
	for range dwellCycles {
		at = at.Add(propCycle)
		_, mv := r.m.note(380, at)
		require.Zero(t, mv.from, "no move inside the dwell")
	}
	at = at.Add(propCycle)
	got, mv := r.m.note(380, at)
	assert.Equal(t, 380.0, got)
	assert.Equal(t, "long", classifyOutputLength(mv.from), "the move is from long")
	assert.True(t, mv.swap, "a swap made it")
	at = at.Add(propCycle)
	_, mv = r.m.note(380, at)
	assert.Zero(t, mv.from, "one move, reported once")
}

// The margins are judged at the boundary of the bucket the key leaves.
func TestK2KeyBucket_TheMarginIsAtTheBoundaryLeft(t *testing.T) {
	assert.True(t, pastBucketMargin(530, 4000, K2KeyBucketMargin))
	assert.False(t, pastBucketMargin(530, 1540, K2KeyBucketMargin), "under 1500 x 1.03")
	assert.True(t, pastBucketMargin(530, 1545, K2KeyBucketMargin))
	assert.True(t, pastBucketMargin(530, 484, K2KeyBucketMargin))
	assert.False(t, pastBucketMargin(530, 486, K2KeyBucketMargin))
	assert.True(t, pastBucketMargin(530, 399, K2KeyBucketSwap))
	assert.False(t, pastBucketMargin(530, 400, K2KeyBucketSwap))
	assert.False(t, pastBucketMargin(7000, 1e6, K2KeyBucketSwap), "the last bucket has no upper bound")
}

// SwitchK2KeyBucketHeld off: the key reads the tracked length, as before.
// On, it reads the fleet's length, its bucket held.
func TestK2KeyBucket_TheSwitchPicksTheLength(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.now = func() time.Time { return propStart }
	a.fleetShape["ns|m"] = &shapeMemo{}
	length := func(model string, fleet float64) float64 {
		l, _ := a.k2KeyLength("ns", model, fleet, 530)
		return l
	}
	assert.Equal(t, 480.0, length("m", 480), "on: the fleet's length")
	assert.Equal(t, 480.0, length("m", 0), "on, no length this cycle: the held one")
	assert.Equal(t, 530.0, length("other", 480), "no shape memo yet: the tracked length")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2KeyBucketHeld: false}))
	assert.Equal(t, 530.0, length("m", 480), "off: the tracked length")
}

// A key that moves to a bucket with no window of its own is priced from the
// bucket it left, lower only (seedMovedK2Key, P2-prior), not at k1. The fleet
// learns k2 saturated at 530 output tokens ("long"), then keeps up, unsaturated,
// at 440: the key moves to "medium" within minutes, and for the 20 minutes the
// test runs no cycle prices the replica above what it learned.
func TestK2KeyBucket_AMovedKeyIsPricedFromTheBucketItLeft(t *testing.T) {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, propStart)
	a := NewSaturationAnalyzer(store)
	clock := propStart
	a.now = func() time.Time { return clock }
	run := func(tokens int64, queue int, out float64) domain.VariantCapacity {
		t.Helper()
		in := makeAnalyzerInput([]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", tokens, 16000, queue, 100, out)}, mtOneVariant)
		res, err := a.Analyze(context.Background(), in)
		require.NoError(t, err)
		require.Len(t, res.VariantCapacities, 1)
		clock = clock.Add(propCycle)
		return res.VariantCapacities[0]
	}
	for range 10 {
		run(8000, 6, 530)
	}
	learned := run(1000, 0, 530).PerReplicaCapacity
	k1 := 16000 * 0.8
	require.True(t, learned > 0 && learned < k1, "precondition: a k2 under k1 learned in long, got %v", learned)
	moved := -1
	for i := range 80 {
		vc := run(1000, 0, 440)
		a.mu.Lock()
		bucket := classifyOutputLength(a.fleetShape["test-ns|test-model"].k2Key.length)
		a.mu.Unlock()
		if bucket == "medium" && moved < 0 {
			moved = i
		}
		assert.LessOrEqual(t, vc.PerReplicaCapacity, learned, "cycle %d at 440 (key in %s): priced above the k2 learned in long", i, bucket)
	}
	require.GreaterOrEqual(t, moved, 0, "the key never moved to medium: the test did not reach the move")
	t.Logf("the key moved to medium on cycle %d at 440; learned k2 %v, k1 %v", moved, learned, k1)
}
