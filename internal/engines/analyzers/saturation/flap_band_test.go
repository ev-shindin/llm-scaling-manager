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
		return a.keyLengths("ns", model, fleet, 530).k2
	}
	assert.Equal(t, 480.0, length("m", 480), "on: the fleet's length")
	assert.Equal(t, 480.0, length("m", 0), "on, no length this cycle: the held one")
	assert.Equal(t, 530.0, length("other", 480), "no shape memo yet: the tracked length")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2KeyBucketHeld: false}))
	assert.Equal(t, 530.0, length("m", 480), "off: the tracked length")
}

// The throughput key's switch (#415), the same way: on, the band's length,
// the one the k2 key reads; off, the tracked length, and no move or carry.
// Each key reads the band by its own switch, and the band runs while either
// is on.
func TestMuKeyBucket_TheSwitchPicksTheLength(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.now = func() time.Time { return propStart }
	a.fleetShape["ns|m"] = &shapeMemo{}
	got := a.keyLengths("ns", "m", 480, 530)
	assert.Equal(t, 480.0, got.mu, "on: the fleet's length")
	assert.Equal(t, got.k2, got.mu, "on: the length the k2 key reads")
	assert.Equal(t, 480.0, a.keyLengths("ns", "m", 0, 530).mu, "on, no length this cycle: the held one")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuKeyBucketHeld: false}))
	got = a.keyLengths("ns", "m", 480, 530)
	assert.Equal(t, 530.0, got.mu, "off: the tracked length")
	assert.Equal(t, 480.0, got.k2, "off: the k2 key still reads the band")
	assert.Zero(t, got.muLeft, "off: nothing to carry from")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuKeyBucketHeld: true, SwitchK2KeyBucketHeld: false}))
	got = a.keyLengths("ns", "m", 480, 530)
	assert.Equal(t, 480.0, got.mu, "the k2 switch off: the throughput key still reads the band")
	assert.Equal(t, 530.0, got.k2, "the k2 switch off: the tracked length")
}

// The two keys sit in one bucket on every cycle (#415): k2's settle test
// (k2Unsettled) compares a throughput reference with a k2 reference, read
// under the two keys, and from different buckets they are figures of
// different shapes. On the property case's fleet, its output length 6 %
// above the 500-token boundary with noise, the buckets agree on every
// cycle; with SwitchMuKeyBucketHeld off they do not, on some seed.
func TestMuKeyBucket_TheKeysShareABucket(t *testing.T) {
	split := func(edit func(*SaturationAnalyzer)) (int, int) {
		t.Helper()
		splits, seeds := 0, 0
		for seed := uint64(1); seed <= 50; seed++ {
			noise := arNoise(propRand(seed), 120, 0.06, 0.9)
			lengths := make([]float64, len(noise))
			for i, e := range noise {
				lengths[i] = 530 * (1 + e)
			}
			k2, mu, err := selectedKeys(lengths, nil, edit)
			require.NoError(t, err)
			n := 0
			for i := range k2 {
				require.NotEmpty(t, k2[i], "seed %d cycle %d: no k2 key", seed, i)
				require.NotEmpty(t, mu[i], "seed %d cycle %d: no mu key", seed, i)
				if bucketOf(k2[i]) != bucketOf(mu[i]) {
					n++
				}
			}
			splits += n
			if n > 0 {
				seeds++
			}
		}
		return splits, seeds
	}
	splits, seeds := split(nil)
	assert.Zero(t, splits, "on: the k2 and mu keys sat in different buckets on %d cycles of %d seeds", splits, seeds)
	splits, seeds = split(func(a *SaturationAnalyzer) { a.muKeyBucketHeld = false })
	assert.Positive(t, splits, "off: the keys never split, so this test cannot see the switch")
	t.Logf("off: the keys sat in different buckets on %d cycles of %d of 50 seeds", splits, seeds)
}

// A throughput key the band moved to a bucket with no window is priced from
// the bucket it left where that is lower than the nearest neighbour
// (carriedThroughput), and the neighbour where it is not. The variant
// measured "short" at 4 and "long" at 1; the band moves the key from "long"
// to "medium". The nearest neighbour, the shorter on a tie, is "short": 4,
// over what the "long" figure of 1 says. Carried, 1; with
// SwitchMuKeyBucketHeld off, nothing is carried and the key is still "long".
func TestMuKeyBucket_AMovedKeyIsCarriedLowerOnly(t *testing.T) {
	const prefix = "ns|v|m|H200|1|decode|ilong|"
	short, medium, long := prefix+"short|q5", prefix+"medium|q5", prefix+"long|q5"
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.now = func() time.Time { return propStart }
	a.recordSaturatedThroughputFrom(short, "p", 4)
	a.recordSaturatedThroughputFrom(long, "p", 1)

	none := a.saturatedThroughputReadingUnder(medium, "", true)
	require.Equal(t, 4.0, none.rate, "precondition: the nearest neighbour is short's")
	got := a.carriedThroughput(medium, long, "", none)
	assert.Equal(t, 1.0, got.rate, "carried from the bucket left, the lower")
	assert.Equal(t, "long", got.bucket)
	assert.True(t, got.borrowed, "a carried figure is borrowed")

	// Lower only: a key that left "short" for "medium" keeps the nearest.
	a2 := NewSaturationAnalyzer(capacity.NewStore())
	a2.now = a.now
	a2.recordSaturatedThroughputFrom(short, "p", 4)
	a2.recordSaturatedThroughputFrom(long, "p", 1)
	near := a2.saturatedThroughputReadingUnder(medium, "", true)
	assert.Equal(t, near, a2.carriedThroughput(medium, short, "", near), "the bucket left is higher: the neighbour stands")

	// A window of its own is never carried over.
	a.recordSaturatedThroughputFrom(medium, "p", 2)
	own := a.saturatedThroughputReadingUnder(medium, "", true)
	assert.Equal(t, own, a.carriedThroughput(medium, long, "", own), "own window: as read")

	// Through the analyzer: the band moves the key, and muLeft names the
	// bucket left; off, the throughput key reads the tracked length and
	// carries nothing.
	b := NewSaturationAnalyzer(capacity.NewStore())
	clock := propStart
	b.now = func() time.Time { return clock }
	b.fleetShape["ns|m"] = &shapeMemo{}
	var keys outputKeys
	for range 10 {
		keys = b.keyLengths("ns", "m", 530, 530)
		clock = clock.Add(propCycle)
	}
	for range int(K2KeyBucketDwell/propCycle) + 2 {
		keys = b.keyLengths("ns", "m", 380, 530)
		clock = clock.Add(propCycle)
	}
	require.Equal(t, "medium", classifyOutputLength(keys.mu), "precondition: a swap moved the band to medium")
	assert.Equal(t, "long", classifyOutputLength(keys.muLeft), "the bucket left")
	require.NoError(t, b.SetSwitchesForReplay(map[string]bool{SwitchMuKeyBucketHeld: false}))
	keys = b.keyLengths("ns", "m", 380, 530)
	assert.Equal(t, 530.0, keys.mu, "off: the tracked length")
	assert.Zero(t, keys.muLeft, "off: nothing carried")
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

// swapRun is one variant measured saturated at 530 output tokens ("long")
// at 3 req/s, then saturated at 5500 tokens at 0.5 req/s: a swap to longer
// outputs. It returns, for each cycle after the swap, whether a swap was
// pending on the band, the "long" window's readings after the cycle, and the
// variant's service-rate source.
type swapCycle struct {
	pending bool
	long    []float64
	source  string
}

func runSwap(t *testing.T, freeze bool) (before []float64, cycles []swapCycle) {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, propStart)
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuSwapFreeze: freeze}))
	clock := propStart
	a.now = func() time.Time { return clock }
	longKey := ""
	readings := func() []float64 {
		a.mu.Lock()
		defer a.mu.Unlock()
		if longKey == "" {
			for k := range a.saturatedThroughput {
				if bucketOf(k) == "long" {
					longKey = k
				}
			}
		}
		ra, ok := a.saturatedThroughput[longKey]
		if !ok {
			return nil
		}
		return ra.Snapshot().Values
	}
	run := func(out, rate float64) string {
		t.Helper()
		rm := makeReplicaMetrics("pod-a", "variant-a", 12000, 16000, 6, 100, out)
		rm.RequestRate, rm.GenerationTokenRate, rm.Ready = rate, rate*out, true
		res, err := a.Analyze(context.Background(), makeAnalyzerInput([]domain.ReplicaMetrics{rm}, mtOneVariant))
		require.NoError(t, err)
		require.Len(t, res.VariantCapacities, 1)
		clock = clock.Add(propCycle)
		if sr := res.VariantCapacities[0].ServiceRate; sr != nil {
			return sr.RateSource
		}
		return ""
	}
	src := ""
	for range 12 {
		src = run(530, 3)
	}
	before = readings()
	require.NotEmpty(t, before, "precondition: the variant measured mu in long")
	require.Equal(t, domain.ServiceRateSourceMeasured, src, "precondition: priced from its own measurement in long")
	for range int(K2KeyBucketDwell/propCycle) + 4 {
		src := run(5500, 0.5)
		a.mu.Lock()
		pending := !a.fleetShape["test-ns|test-model"].k2Key.swapSince.IsZero()
		a.mu.Unlock()
		cycles = append(cycles, swapCycle{pending: pending, long: readings(), source: src})
	}
	return before, cycles
}

// While a swap is pending the throughput window is frozen (muSwapFrozen):
// the arriving shape's readings are not written into the bucket the key is
// leaving, and that bucket's measured mu -- 3 req/s at 530 tokens, six times
// the 0.5 of the shape arriving -- never prices at full authority. With
// SwitchMuSwapFreeze off, the mutant: the old bucket's window takes the new
// shape's readings and prices as measured.
func TestMuKeyBucket_APendingSwapFreezesTheWindow(t *testing.T) {
	before, cycles := runSwap(t, true)
	pending := 0
	for i, c := range cycles {
		if !c.pending {
			continue
		}
		pending++
		assert.Equal(t, before, c.long, "cycle %d, swap pending: the long window was written", i)
		assert.NotEqual(t, domain.ServiceRateSourceMeasured, c.source, "cycle %d, swap pending: the old bucket's mu priced at full authority", i)
	}
	require.Positive(t, pending, "no cycle had a swap pending: the test did not reach the freeze")
	assert.False(t, cycles[len(cycles)-1].pending, "the swap never landed")

	before, cycles = runSwap(t, false)
	wrote, measured := false, false
	for _, c := range cycles {
		if c.pending {
			wrote = wrote || !assert.ObjectsAreEqual(before, c.long)
			measured = measured || c.source == domain.ServiceRateSourceMeasured
		}
	}
	assert.True(t, wrote && measured, "freeze off: the pending swap should write the long window (%v) and price it as measured (%v); the test cannot see the switch", wrote, measured)
}
