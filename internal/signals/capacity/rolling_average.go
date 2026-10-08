package capacity

import (
	"slices"
	"time"
)

const (
	// RollingAverageWindowSize is the number of samples retained for compute
	// capacity (k2) history per workload bucket, and for the saturated
	// completion-rate (mu) windows recorded beside it.
	RollingAverageWindowSize = 10

	// HistoryEvictionTimeout is how long a k2 or mu reading is TRUSTED: past
	// it the read refuses the window, because workload patterns shift and a
	// stale observation from a very different workload can mislead scaling
	// decisions.
	//
	// It is a trust horizon, not a retention horizon. The two were the same
	// constant once, and that made refusing a reading indistinguishable from
	// never having had one -- see HistoryRetention.
	HistoryEvictionTimeout = 24 * time.Hour

	// HistoryRetention is how long a bucket-keyed window is KEPT after it was
	// last read, which is seven times longer than it is trusted.
	//
	// The gap is the point. k2 falls back through history -> derived -> k1,
	// and the DERIVED figure is typically well above the measured one: 50,000
	// measured against 332,800 derived on one variant. Since
	// effectiveCapacity = min(k1, k2), falling to derived RAISES capacity and
	// orders fewer replicas, which this project has recorded as breaking TTFT
	// irrecoverably. So "I measured this bucket and no longer trust the
	// figure" must not fall to derived -- it falls to k1, which bounds
	// something real.
	//
	// That is only possible while the window still EXISTS to be refused. The
	// sweep runs at the top of the cycle, before any capacity is computed, so
	// a window evicted on the trust horizon is gone milliseconds before the
	// read that would have refused it, and the fall-through to k1 would never
	// fire. Keeping the window for seven days lets its existence carry the one
	// bit that matters -- this bucket has been measured -- long after the
	// figure stops being usable. Seven days also matches the capacity store's
	// EvictionTimeout, and for the same reason: a variant may be quiet over a
	// weekend and come back on Monday.
	HistoryRetention = 7 * 24 * time.Hour
)

// RollingAverage maintains a fixed-size sliding window of float64 values.
// The saturation analyzer keeps one per history key for two readings: the
// compute-bound capacity (k2), read through Average, and the saturated
// throughput (mu) the demand floor prices from, read through Median. Each
// read's own doc says why it is the right one for what that window holds.
//
// Not safe for concurrent use: the owner serialises access (the saturation
// analyzer holds its own mutex around every window).
// TWO timestamps, because three different questions were being asked of one
// and they do not have the same answer:
//
//	lastUsed    -- when a decision last READ this window. Governs retention:
//	               a window nothing reads is what the sweep is for.
//	lastWritten -- when a value was last PUT IN. Governs whether a new
//	               observation joins this window or starts a fresh one.
//
// Collapsing them is a live defect, found in review. The windows the analyzer
// keeps are written only on a saturated queue but READ every cycle, so one
// field made every read look like a write: the write path's "a long gap means
// a new episode, so reset rather than blend" check could never fire for any
// bucket still being queried. Two saturation episodes weeks apart then blended
// into one rolling average, and a stale high reading held capacity up against
// today's lower, truer one.
type RollingAverage struct {
	values      []float64
	maxSize     int
	lastUsed    time.Time
	lastWritten time.Time
}

// NewRollingAverage creates a RollingAverage with the given window size.
func NewRollingAverage(maxSize int) *RollingAverage {
	now := time.Now()
	return &RollingAverage{
		values:      make([]float64, 0, maxSize),
		maxSize:     maxSize,
		lastUsed:    now,
		lastWritten: now,
	}
}

// Add appends a value, evicting the oldest entry if the window is full.
func (r *RollingAverage) Add(value float64) {
	if len(r.values) >= r.maxSize {
		r.values = r.values[1:]
	}
	r.values = append(r.values, value)
	now := time.Now()
	r.lastUsed = now
	r.lastWritten = now
}

// Average returns the arithmetic mean of all stored values, or 0 if empty.
func (r *RollingAverage) Average() float64 {
	if len(r.values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range r.values {
		sum += v
	}
	return sum / float64(len(r.values))
}

// Touch marks the window as USED now, without adding to it and without
// counting as a write. A decision that reads a window is still depending on
// it, which is what retention asks about -- but reading cannot make the data
// newer, so Touch deliberately leaves lastWritten alone. Writers call Observe.
func (r *RollingAverage) Touch() {
	r.TouchAt(time.Now())
}

// TouchAt is Touch at a given time; Touch is TouchAt(time.Now()). A test ages
// a window's USE by it, which is what the sweep reads.
func (r *RollingAverage) TouchAt(t time.Time) {
	r.lastUsed = t
}

// Observe records that the window's own producer saw its reading again without
// a new value to store -- the same sample across a spacing boundary, or a
// repeat. It is a write for gap purposes even though nothing is appended: the
// producer only runs while the condition it measures holds, so seeing the same
// reading again is evidence the episode is still going.
func (r *RollingAverage) Observe() {
	now := time.Now()
	r.lastUsed = now
	r.lastWritten = now
}

// ObservedAt is Observe at a given time; a test ages a window's WRITE by it.
func (r *RollingAverage) ObservedAt(t time.Time) {
	r.lastUsed = t
	r.lastWritten = t
}

// RaiseLast lifts the most recent value to v when v is higher, and counts as a
// write either way. A reading that belongs to the last sample's window is
// folded into that sample this way rather than counted or dropped.
func (r *RollingAverage) RaiseLast(v float64) {
	if n := len(r.values); n > 0 && v > r.values[n-1] {
		r.values[n-1] = v
	}
	r.Observe()
}

// Last returns the most recent value, or 0 if empty. No production caller;
// the tests read it.
func (r *RollingAverage) Last() float64 {
	if n := len(r.values); n > 0 {
		return r.values[n-1]
	}
	return 0
}

// Len returns the number of values currently stored.
func (r *RollingAverage) Len() int {
	return len(r.values)
}

// Max returns the largest stored value, or 0 if empty.
//
// The saturated throughput window used to read this, on the argument that a
// COMPLETION rate under saturation under-reads while the replica is full, so
// the largest reading is the best estimate of what it sustains. That argument
// did not survive the move to a token rate, which bursts rather than
// under-reads; the window reads Median. See "The mu window reads the median,
// not the maximum" in docs/developer-guide/analyzer-evidence.md.
func (r *RollingAverage) Max() float64 {
	if len(r.values) == 0 {
		return 0
	}
	return slices.Max(r.values)
}

// Median returns the middle value of the window, the lower of the two middle
// values on an even count, or 0 when the window is empty.
//
// Where Max is the right read for a figure whose error is one-sided -- a
// saturated COMPLETION rate under-reads while a replica fills, so the largest
// reading is the best estimate of what it sustains -- Median is the right read
// for one that bursts, and a generation-token rate is the latter. Read with
// Max, the window ratcheted to a mu that asked for a fifth of the replicas the
// fleet needed; see "The mu window reads the median, not the maximum" in
// docs/developer-guide/analyzer-evidence.md.
func (r *RollingAverage) Median() float64 {
	if len(r.values) == 0 {
		return 0
	}
	sorted := make([]float64, len(r.values))
	copy(sorted, r.values)
	slices.Sort(sorted)
	return sorted[(len(sorted)-1)/2]
}

// Stale reports whether nothing has USED the window within the timeout:
// creation, Add, RaiseLast, Observe and Touch all count, so a window created
// and never added to is fresh for one timeout. This is the retention question,
// and the sweep is its caller. For "has a long gap passed since anything was
// written", which is a different question with a different answer, see
// WriteGapExceeds.
//
// The saturation analyzer's EvictStaleHistory sweeps whole entries on the same
// measure, once per cycle from steadystate.evictStaleLearnedState. Readers
// check it anyway: a window can go stale between sweeps, and a read that
// depended on the sweep having run would give two answers for one state. An
// average carried over a long gap is worse than no average, because it looks
// like data and is weighted like data.
//
// Which makes WHAT counts as a touch load-bearing. A window whose only writer
// is a saturated-queue path ages on "last saturated" unless its readers Touch
// it, and a fleet that is coping does not saturate -- so both the k2 history
// and the mu windows touch on the read, and age on last USE. See the Priority
// 2 read in the analyzer's replica_capacity.go for what that cost when they
// did not.
func (r *RollingAverage) Stale(timeout time.Duration) bool {
	return time.Since(r.lastUsed) > timeout
}

// WriteGapExceeds reports whether more than the given gap has passed since a
// value was last written to this window. Reads do not count, by design.
//
// It answers the producer's question: is this new reading a continuation of
// what the window already holds, or the first of a new episode? The windows
// the analyzer keeps are written only while the condition they measure holds
// -- a saturated queue -- so a long write gap means the fleet stopped
// saturating and has started again, and the figures either side of the gap
// describe different eras. Blending them is worse than either, because a
// rolling average weights the old readings like evidence. The producers reset
// the window on this rather than adding to it.
//
// Asking Stale instead is the defect this method exists to prevent: Stale
// tracks USE, every cycle reads these windows, so it never reports a gap for
// any bucket the fleet is still serving.
func (r *RollingAverage) WriteGapExceeds(gap time.Duration) bool {
	return time.Since(r.lastWritten) > gap
}
