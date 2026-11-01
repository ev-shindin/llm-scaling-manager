package saturation

import (
	"math"
	"time"
)

// The k2 key's output bucket, held across a boundary (#396).
//
// k2's window was keyed by the fleet's tracked output length. That length
// moves only when the shape tracker declares a change, a move of
// shape.DefaultChangeTolerance (20 %) from its anchor, and the tracker
// declares it on one reading. So a single noisy reading 20 % off moved the key
// into the next bucket for as long as the tracker stayed there. On the
// property case edges/k2/key-near-output-boundary (output length 6 % above
// the 500-token boundary, with 6 % noise correlated over ten cycles) the
// selected key left "long" for "medium" on 13 of 50 seeds (seed 2: twice,
// once out of "long"): k2 was read from another window for minutes at a time
// at constant load.
//
// Judging the bucket on one cycle's length, however wide the band, does not
// fix it: the noise's excursions last as long as a dwell, and one reading
// also chose the starting bucket, which a band then kept (with a 10 % band
// and a 2 min dwell, 2 of 50 seeds stayed in "medium" for the whole run). So
// the key is judged on the fleet's output length smoothed over
// K2KeyBucketSmoothing, and leaves its bucket when that is past the boundary
// by K2KeyBucketMargin. A shape swap moves the smoothed length too slowly
// for its first minutes, and would walk the key through the buckets between
// the two shapes, so a length that has stayed past the boundary by
// K2KeyBucketSwap on every cycle for K2KeyBucketDwell moves the key to its
// own bucket and restarts the smoothing there; while one is pending the key
// holds its bucket, so a swap does not stop in the buckets between. Both
// rules apply the same way in both directions, and neither moves the key on
// one reading.
//
// The cost: after a real move of the length, a fleet that is not saturated is
// priced from the old bucket's window until the key moves -- about 7 min for
// 530 -> 440 tokens and 14 min for 530 -> 470 -- or for K2KeyBucketDwell when
// it moves by more than K2KeyBucketSwap (530 -> 380 or less). A saturated
// fleet is priced from its observation (P1-obs), which also refills the window
// it writes within a few cycles.
//
// When the key does move, the bucket it lands in usually has no window of
// its own, and k2 would fall to k1 (P4-k1): about 60 % more capacity per
// replica on the property rig, until the fleet saturates in the new bucket --
// which a fleet that keeps up never does. The old key did that too, but only
// on a tracker change of 20 %; this one moves on 10-20 % shifts the old key
// ignored. So a move seeds the new bucket's lower bound with the variant's
// figure from the bucket it left (seedMovedK2Key), as the variant's own
// previous figure, which P2-prior already prices an argument change from:
// used only where it is below the variant's own capacity without it, held
// 30 min, relaxed over 6 h, and set aside by the new bucket's first
// measurement.

// K2KeyBucketSmoothing is the time constant of the smoothed output length
// the k2 key is judged on. Until that long after the first reading the
// smoothed length is the mean of every reading so far, so one early reading
// does not choose the starting bucket.
const K2KeyBucketSmoothing = 10 * time.Minute

// K2KeyBucketMargin is how far past a bucket boundary, as a fraction of the
// boundary, the smoothed output length must be for the k2 key to leave its
// bucket: below 485 tokens to leave "long", at or above 515 to leave
// "medium". The boundary is the one of the bucket left.
//
// The case run over 300 seeds, with the length 6 % over 500: at 3 % and a
// 10 min smoothing 3 seeds fail (each into "medium" and back), and none of
// the 50 the case runs. A 5 min smoothing fails 14, a 2 % margin 14, and a
// 5 % margin 5, 4 of them in "medium" for the whole run. A margin this
// close to the noise is a judgement on that case, not a measured optimum.
const K2KeyBucketMargin = 0.03

// K2KeyBucketSwap is how far past the boundary, as a fraction of it, a
// length must stay for K2KeyBucketDwell to move the key at once to its own
// bucket: a shape swap. Under 400 tokens to leave "long", at or above 600 to
// leave "medium". The shape tracker's tolerance
// (shape.DefaultChangeTolerance); on the property case the noise reached 19 %
// for a cycle and never stayed there.
const K2KeyBucketSwap = 0.20

// K2KeyBucketDwell is how long the length must stay past K2KeyBucketSwap,
// cycle after cycle, for a swap. Twice the one-minute window the collector's
// gauges are read over, as IdleSettledFor is.
const K2KeyBucketDwell = 2 * ThroughputSampleSpacing

// SwitchK2KeyBucketHeld is the replay switch for the k2 key's bucket band
// (#396, analyzer-learner.md 2.5). On, the key's output bucket is held as
// k2KeyLength says. Off, as before, the key reads the tracked length.
const SwitchK2KeyBucketHeld = "k2-key-bucket-held"

// k2KeyMemo is one model's k2 key length and the smoothed length it follows.
type k2KeyMemo struct {
	// length is the output length the k2 keys were last built from, 0
	// before the first: its bucket is the key's.
	length float64
	// smoothed is the output length smoothed over K2KeyBucketSmoothing, as
	// of at; n counts the readings in it.
	smoothed float64
	at       time.Time
	n        int
	// swapSince is when the length was first read past K2KeyBucketSwap of
	// length's bucket, on every cycle since; zero when it is not.
	swapSince time.Time
}

// outputBucketUppers is the upper bound of every output-length bucket but the
// last, in order (classifyOutputLength).
var outputBucketUppers = [...]float64{ShortOutputThreshold, MediumOutputThreshold, LongOutputThreshold,
	ExtraLongOutputThreshold, VeryLongOutputThreshold}

// outputBucketBounds is the bounds of the output-length bucket l is in
// (classifyOutputLength). hi is 0 for the last bucket, which has none.
func outputBucketBounds(l float64) (lo, hi float64) {
	for _, b := range outputBucketUppers {
		if l < b {
			return lo, b
		}
		lo = b
	}
	return lo, 0
}

// pastBucketMargin reports whether now is past the bucket of held by margin:
// above its upper bound, or below its lower bound, by that fraction of the
// bound.
func pastBucketMargin(held, now, margin float64) bool {
	lo, hi := outputBucketBounds(held)
	return (hi > 0 && now >= hi*(1+margin)) || now < lo*(1-margin)
}

// k2KeyMove is a move of the k2 key's output bucket: from is the length the
// key was built from before it, 0 when the bucket did not move, and swap says
// the swap rule moved it.
type k2KeyMove struct {
	from float64
	swap bool
}

// note is this cycle's output length, now, read at t; it returns the length
// the k2 keys are built from, and the move of its bucket if there was one. A
// swap (now past K2KeyBucketSwap for K2KeyBucketDwell) moves it to now and
// restarts the smoothing there. Otherwise it is the smoothed length when that
// is in the held bucket, or past its margin with no swap pending, and the
// held length when not.
func (m *k2KeyMemo) note(now float64, t time.Time) (float64, k2KeyMove) {
	if !(m.length > 0) {
		*m = k2KeyMemo{length: now, smoothed: now, at: t, n: 1}
		return m.length, k2KeyMove{}
	}
	was := m.length
	switch {
	case !pastBucketMargin(m.length, now, K2KeyBucketSwap):
		m.swapSince = time.Time{}
	case m.swapSince.IsZero():
		m.swapSince = t
	case t.Sub(m.swapSince) >= K2KeyBucketDwell:
		*m = k2KeyMemo{length: now, smoothed: now, at: t, n: 1}
		return m.length, k2KeyMove{from: was, swap: true}
	}
	// The mean of every reading until K2KeyBucketSmoothing has passed, an
	// exponential average over it after.
	m.n++
	w := 1 / float64(m.n)
	if dt := t.Sub(m.at); dt > 0 {
		w = max(w, 1-math.Exp(-float64(dt)/float64(K2KeyBucketSmoothing)))
	}
	m.smoothed += w * (now - m.smoothed)
	m.at = t
	if classifyOutputLength(m.smoothed) == classifyOutputLength(m.length) ||
		(m.swapSince.IsZero() && pastBucketMargin(m.length, m.smoothed, K2KeyBucketMargin)) {
		m.length = m.smoothed
	}
	if classifyOutputLength(m.length) != classifyOutputLength(was) {
		return m.length, k2KeyMove{from: was}
	}
	return m.length, k2KeyMove{}
}

// k2KeyLength is the output length the model's k2 keys are built from this
// cycle, and the move of its bucket if there was one. With
// SwitchK2KeyBucketHeld on, the fleet's output length now (fleetOut), its
// bucket held (k2KeyMemo.note); a cycle with no length carries the held one,
// and the tracked length (stableOut) stands in until there is one. Off, the
// tracked length, as before, and never a move. The memo is not persisted: a
// controller restart chooses the bucket again from its first readings.
func (a *SaturationAnalyzer) k2KeyLength(namespace, modelID string, fleetOut, stableOut float64) (float64, k2KeyMove) {
	a.mu.Lock()
	defer a.mu.Unlock()
	memo, ok := a.fleetShape[namespace+"|"+modelID]
	if !a.k2KeyBucketHeld || !ok {
		return stableOut, k2KeyMove{}
	}
	if !(fleetOut > 0) {
		if memo.k2Key.length > 0 {
			return memo.k2Key.length, k2KeyMove{}
		}
		return stableOut, k2KeyMove{}
	}
	return memo.k2Key.note(fleetOut, a.now())
}

// seedMovedK2Key prices a k2 key the bucket moved to (to) from the one it left
// (from) until it has a window of its own: the window of from becomes the
// previous figure of to (notePreviousFigureLocked), which P2-prior prices
// lower only, holds and relaxes. Nothing is seeded when to already has a
// window, or from has none.
func (a *SaturationAnalyzer) seedMovedK2Key(from, to string) {
	if from == to {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if ra, ok := a.computeCapacityHistory[to]; ok && ra.Len() > 0 {
		return
	}
	ra, ok := a.computeCapacityHistory[from]
	if !ok || ra.Len() == 0 {
		return
	}
	a.notePreviousFigureLocked(to, ra)
}
