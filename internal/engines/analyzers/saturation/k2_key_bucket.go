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
// selected key moved between "long" and "medium", and back, on 17 of 50
// seeds: k2 was read from another window for minutes at a time at constant
// load.
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
// The cost: a real move of the length prices k2 from the old bucket's window
// until the smoothed length crosses -- about 7 min for 530 -> 440 tokens and
// 14 min for 530 -> 470 -- or for K2KeyBucketDwell when it moves by more than
// K2KeyBucketSwap (530 -> 380 or less).

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

// outputBucketBounds is the bounds of the output-length bucket l is in
// (classifyOutputLength). hi is 0 for the last bucket, which has none.
func outputBucketBounds(l float64) (lo, hi float64) {
	bounds := []float64{ShortOutputThreshold, MediumOutputThreshold, LongOutputThreshold,
		ExtraLongOutputThreshold, VeryLongOutputThreshold}
	for _, b := range bounds {
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

// note is this cycle's output length, now, read at t; it returns the length
// the k2 keys are built from. A swap (now past K2KeyBucketSwap for
// K2KeyBucketDwell) moves it to now and restarts the smoothing there.
// Otherwise it is the smoothed length when that is in the held bucket, or
// past its margin with no swap pending, and the held length when not.
func (m *k2KeyMemo) note(now float64, t time.Time) float64 {
	if !(m.length > 0) {
		*m = k2KeyMemo{length: now, smoothed: now, at: t, n: 1}
		return m.length
	}
	switch {
	case !pastBucketMargin(m.length, now, K2KeyBucketSwap):
		m.swapSince = time.Time{}
	case m.swapSince.IsZero():
		m.swapSince = t
	case t.Sub(m.swapSince) >= K2KeyBucketDwell:
		*m = k2KeyMemo{length: now, smoothed: now, at: t, n: 1}
		return m.length
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
	return m.length
}

// k2KeyLength is the output length the model's k2 keys are built from this
// cycle. With SwitchK2KeyBucketHeld on, the fleet's output length now
// (fleetOut), its bucket held (k2KeyMemo.note); a cycle with no length
// carries the held one, and the tracked length (stableOut) stands in until
// there is one. Off, the tracked length, as before.
func (a *SaturationAnalyzer) k2KeyLength(namespace, modelID string, fleetOut, stableOut float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	memo, ok := a.fleetShape[namespace+"|"+modelID]
	if !a.k2KeyBucketHeld || !ok {
		return stableOut
	}
	if !(fleetOut > 0) {
		if memo.k2Key.length > 0 {
			return memo.k2Key.length
		}
		return stableOut
	}
	return memo.k2Key.note(fleetOut, a.now())
}
