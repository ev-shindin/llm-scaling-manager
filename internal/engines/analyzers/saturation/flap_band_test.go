package saturation

import (
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

// The key leaves its bucket only once the length has been past the margin on
// every cycle for K2KeyBucketDwell; a cycle back inside the margin starts the
// dwell again.
func TestK2KeyBucket_LeavesOnlyPastTheMarginForTheDwell(t *testing.T) {
	var m k2KeyMemo
	at := propStart
	step := func(l float64) string {
		at = at.Add(propCycle)
		return classifyOutputLength(m.note(l, at))
	}
	require.Equal(t, "long", step(530), "the first reading sets the bucket")
	assert.Equal(t, "long", step(470), "under 500 but not under 450: held")
	for range 4 {
		assert.Equal(t, "long", step(440), "under 450, inside the dwell: held")
	}
	assert.Equal(t, "long", step(400), "400 is not under 400: the dwell goes on")
	assert.Equal(t, "long", step(460), "back inside the margin: the dwell starts again")
	dwell := int(K2KeyBucketDwell / propCycle)
	for i := range dwell {
		assert.Equal(t, "long", step(440), "%d cycles of the dwell", i)
	}
	assert.Equal(t, "medium", step(440), "under 450 for the whole dwell: the key leaves")
	assert.Equal(t, "medium", step(540), "over 500, under 550: held in medium")
	assert.Equal(t, "long", step(600), "at 600, past 500 by K2KeyBucketJump: at once")
	assert.Equal(t, "medium", step(399), "under 400: at once")
}

// The margins are judged at the boundary of the bucket the key leaves: from
// "long", 1650 and 1800 up, 450 and 400 down. A length that jumps several
// buckets is past it by more, and moves to its own bucket.
func TestK2KeyBucket_TheMarginIsAtTheBoundaryLeft(t *testing.T) {
	after := func(l float64, cycles int) string {
		var m k2KeyMemo
		at := propStart
		m.note(530, at)
		for range cycles {
			at = at.Add(propCycle)
			m.note(l, at)
		}
		return classifyOutputLength(m.length)
	}
	dwell := int(K2KeyBucketDwell/propCycle) + 1
	assert.Equal(t, "long", after(1600, dwell), "1600 is in xlong, under 1500 x 1.1")
	assert.Equal(t, "long", after(1700, 1), "1700 is past 1650: the dwell")
	assert.Equal(t, "xlong", after(1700, dwell))
	assert.Equal(t, "xlong", after(1800, 1), "1800 is past 1500 x 1.2: at once")
	assert.Equal(t, "xxlong", after(4000, 1), "several buckets up: its own bucket, at once")
	assert.Equal(t, "short", after(50, 1), "several buckets down: its own bucket, at once")
	assert.True(t, pastBucketMargin(530, 4000, K2KeyBucketMargin))
	assert.False(t, pastBucketMargin(530, 540, K2KeyBucketMargin))
	assert.True(t, pastBucketMargin(530, 449, K2KeyBucketMargin))
	assert.False(t, pastBucketMargin(530, 451, K2KeyBucketMargin))
	assert.False(t, pastBucketMargin(7000, 1e6, K2KeyBucketJump), "the last bucket has no upper bound")
}

// SwitchK2KeyBucketHeld off: the key reads the tracked length, as before.
// On, it reads the fleet's length, its bucket held.
func TestK2KeyBucket_TheSwitchPicksTheLength(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.now = func() time.Time { return propStart }
	a.fleetShape["ns|m"] = &shapeMemo{}
	assert.Equal(t, 480.0, a.k2KeyLength("ns", "m", 480, 530), "on: the fleet's length")
	assert.Equal(t, 480.0, a.k2KeyLength("ns", "m", 0, 530), "on, no length this cycle: the held one")
	assert.Equal(t, 530.0, a.k2KeyLength("ns", "other", 480, 530), "no shape memo yet: the tracked length")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2KeyBucketHeld: false}))
	assert.Equal(t, 530.0, a.k2KeyLength("ns", "m", 480, 530), "off: the tracked length")
}
