package saturation

import "time"

// The k2 key's output bucket, held across a boundary (#396).
//
// k2's window was keyed by the fleet's tracked output length, which moves
// only when the shape tracker declares a change: a move of
// shape.DefaultChangeTolerance (20 %) from its anchor. That keeps a small
// wobble from moving the key, but the tracker declares a change on one
// reading, so a noisy reading 20 % off moved the key into the next bucket
// for as long as the tracker stayed there. On the property case
// edges/k2/key-near-output-boundary (output length 6 % above the 500-token
// boundary, with 6 % noise correlated over ten cycles) the selected key moved
// between "long" and "medium", and back, on 17 of 50 seeds: k2 was read from
// another window for minutes at a time at constant load. A margin on the
// tracked length alone does not fix it: the tracker's jumps are as large as
// the noise's excursions, and a 20 % margin still failed 3 of 50 seeds.
//
// So the key's bucket follows the fleet's output length through a band and a
// dwell: it leaves its bucket only once the length has been past the
// boundary by K2KeyBucketMargin on every cycle for K2KeyBucketDwell. An
// excursion of the noise is short; a shape that has moved stays moved. A
// length past the boundary by K2KeyBucketJump leaves at once: a shape swap
// is not noise, and its k2 is not the old shape's.

// K2KeyBucketMargin is how far past a bucket boundary, as a fraction of the
// boundary, the output length must be for the k2 key to leave its bucket:
// below 450 tokens to leave "long" for "medium", at or above 550 to leave
// "medium" for "long". The boundary is the one nearest the bucket left, so a
// length that jumps several buckets passes it by more.
const K2KeyBucketMargin = 0.10

// K2KeyBucketDwell is how long the output length must stay past the margin,
// cycle after cycle, before the k2 key leaves its bucket. Twice the
// one-minute window the collector's gauges are read over, as IdleSettledFor
// is: one reading past the margin is one minute of it.
const K2KeyBucketDwell = 2 * ThroughputSampleSpacing

// K2KeyBucketJump is how far past the boundary, as a fraction of it, a length
// leaves the bucket on its first reading, without the dwell: under 400 tokens
// to leave "long", at or above 600 to leave "medium". The shape tracker's
// tolerance (shape.DefaultChangeTolerance), the move it calls a change of
// shape: on the property case above, 6 % noise correlated over ten cycles
// reached 19 % below the length and never 24.5 %.
const K2KeyBucketJump = 0.20

// SwitchK2KeyBucketHeld is the replay switch for the k2 key's bucket band
// (#396, analyzer-learner.md 2.5). On, the key's output bucket is held as
// k2KeyLength says. Off, as before, the key reads the tracked length.
const SwitchK2KeyBucketHeld = "k2-key-bucket-held"

// k2KeyMemo is one model's k2 key length and the crossing it is waiting out.
type k2KeyMemo struct {
	// length is the output length the k2 keys were last built from, 0
	// before the first: its bucket is the key's.
	length float64
	// pastSince is when the output length was first read past the margin of
	// length's bucket, on every cycle since; zero when it is not.
	pastSince time.Time
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
// the k2 keys are built from: now, when it is in the held bucket, past its
// margin for K2KeyBucketDwell, or past K2KeyBucketJump; the held length
// otherwise.
func (m *k2KeyMemo) note(now float64, t time.Time) float64 {
	switch {
	case !(m.length > 0) || classifyOutputLength(m.length) == classifyOutputLength(now) ||
		pastBucketMargin(m.length, now, K2KeyBucketJump):
		m.length, m.pastSince = now, time.Time{}
	case !pastBucketMargin(m.length, now, K2KeyBucketMargin):
		m.pastSince = time.Time{}
	case m.pastSince.IsZero():
		m.pastSince = t
	case t.Sub(m.pastSince) >= K2KeyBucketDwell:
		m.length, m.pastSince = now, time.Time{}
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
