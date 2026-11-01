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
//
// The throughput (mu) key reads the same band (#415). It read the tracked
// length, which moved on one reading 20 % off its anchor, so on the same
// property case (edges/mu/key-near-output-boundary) it left "long" for
// "medium" on 13 of 50 seeds: measured mu was read from, and written to,
// another bucket's window at constant load, and k2's settle test
// (k2Unsettled) compared a throughput reference from one bucket with a k2
// reference from another. One band puts both keys in one bucket on every
// cycle. When the band moves the throughput key to a bucket with no window,
// it borrows a neighbour's as it always has (nearestSaturatedThroughput),
// and the bucket it left where that is lower (carriedThroughput).

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
// keyLengths says. Off, as before, the key reads the tracked length.
const SwitchK2KeyBucketHeld = "k2-key-bucket-held"

// SwitchMuKeyBucketHeld is the replay switch for the throughput (mu) key's
// output bucket (#415, analyzer-learner.md 2.5). On, the key reads the k2
// key's band (keyLengths), so the two keys sit in one bucket, and a key
// that moves to a bucket with no window is priced from the bucket it left
// where that is lower (carriedThroughput). Off, as before, the key reads the
// tracked length.
const SwitchMuKeyBucketHeld = "mu-key-bucket-held"

// k2KeyMemo is one model's output-key band: the length the keys are built
// from and the smoothed length it follows. The k2 key reads it, and the
// throughput key too (SwitchMuKeyBucketHeld).
type k2KeyMemo struct {
	// length is the output length the k2 keys were last built from, 0
	// before the first: its bucket is the key's.
	length float64
	// left is the length the keys were built from before the bucket last
	// moved, 0 before the first move: its bucket is the one the keys left.
	// It does not expire: it stands until the next move, so a key whose new
	// window is later swept may be carried from a bucket left hours ago.
	// The carry is lower only and borrowed (carriedThroughput), so that is
	// never the risky side.
	left float64
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
		*m = k2KeyMemo{length: now, left: was, smoothed: now, at: t, n: 1}
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
		m.left = was
		return m.length, k2KeyMove{from: was}
	}
	return m.length, k2KeyMove{}
}

// outputKeys is the output length a model's keys are built from on one
// cycle (keyLengths).
type outputKeys struct {
	// k2 is the length the k2 keys are built from, and mu the length the
	// throughput keys are.
	k2, mu float64
	// k2Move is the move of the k2 key's bucket this cycle, if one happened
	// (seedMovedK2Key), and muMove the throughput key's: the band's one move,
	// for each key that reads the band.
	k2Move, muMove k2KeyMove
	// muLeft is the length the throughput keys were built from before the
	// band last moved, 0 when they do not read the band or it has not moved:
	// the bucket a key with no window of its own is carried from
	// (carriedThroughput).
	muLeft float64
	// muSwapPending is a swap pending on the band (k2KeyMemo.swapSince) when
	// the throughput keys read it: the length is past K2KeyBucketSwap of the
	// key's bucket, and the key has not moved yet. The key's window is then
	// frozen (muSwapFrozen).
	muSwapPending bool
	// muShorterPastMargin is this cycle's length past K2KeyBucketMargin
	// under the lower bound of the key's bucket, when the throughput keys
	// read the band: shorter outputs than the key's bucket, whose readings
	// are clamped to the window's median (marginClamp).
	muShorterPastMargin bool
}

// keyLengths is the output length the model's k2 and throughput keys are
// built from this cycle. The band is one for both keys: the fleet's output
// length now (fleetOut), its bucket held (k2KeyMemo.note); a cycle with no
// length carries the held one, and the tracked length (stableOut) stands in
// until there is one. A key reads the band with its switch on
// (SwitchK2KeyBucketHeld, SwitchMuKeyBucketHeld), and the tracked length,
// as before, with it off, and never a move. The memo is not persisted: a
// controller restart chooses the bucket again from its first readings.
func (a *SaturationAnalyzer) keyLengths(namespace, modelID string, fleetOut, stableOut float64) outputKeys {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := outputKeys{k2: stableOut, mu: stableOut}
	memo, ok := a.fleetShape[namespace+"|"+modelID]
	if !ok || (!a.k2KeyBucketHeld && !a.muKeyBucketHeld) {
		return out
	}
	var held float64
	var move k2KeyMove
	switch {
	case fleetOut > 0:
		held, move = memo.k2Key.note(fleetOut, a.now())
	case memo.k2Key.length > 0:
		held = memo.k2Key.length
	default:
		return out
	}
	if a.k2KeyBucketHeld {
		out.k2, out.k2Move = held, move
	}
	if a.muKeyBucketHeld {
		out.mu, out.muMove, out.muLeft = held, move, memo.k2Key.left
		out.muSwapPending = a.muSwapFreeze && !memo.k2Key.swapSince.IsZero()
		lo, _ := outputBucketBounds(held)
		out.muShorterPastMargin = a.muMarginClamp && fleetOut > 0 && fleetOut < lo*(1-K2KeyBucketMargin)
	}
	return out
}

// SwitchMuSwapFreeze is the replay switch for the throughput window's freeze
// while a swap is pending (#415). On, as muSwapFrozen says. Off, the key's
// window takes writes and prices at full authority through the swap dwell.
const SwitchMuSwapFreeze = "mu-swap-freeze"

// muSwapFrozen is the throughput reading for a key whose band has a swap
// pending (outputKeys.muSwapPending), and the key's window takes no write
// while it is (computeReplicaCapacity).
//
// The band holds the key in its bucket through K2KeyBucketDwell of a swap, so
// that one reading does not move it. For the throughput window that hold is
// not neutral. A swap to longer outputs keeps the key in the shorter bucket,
// whose measured mu is the higher: on the 1000 -> 5500-token shape-swap spec
// (throughput_floor_test.go) the first swapped cycle priced the fleet at the
// old bucket's 3.08 against the new shape's 1.40, at full authority, and the
// new shape's readings went into the old bucket's window (median 1.64 of
// both shapes) -- mu's too-high side, which breaks the SLO
// (analyzer-learner.md 3.4). The tracker-keyed throughput key moved at once
// and did neither.
//
// So while a swap is pending the window is frozen: its readings are not
// written, and the figure it holds counts as borrowed. The derived mu prices
// instead where there is one (useDerived), and the floor may hold the fleet
// on the window but not grow it. The keys keep one bucket. Prefill is never
// frozen: its key has no output bucket (prefillOutputBucket), so an output
// swap leaves it naming its own window.
//
// The cost: up to K2KeyBucketDwell on the derived figure at a real swap, and
// the same under noise, which is correlated over minutes, so an excursion
// past the swap line can last the whole dwell. Measured on k2KeyMemo.note
// over 50 seeds of 480 cycles at 15 s (noise with a 0.9 one-cycle
// correlation): at 6 % noise around 530 tokens no cycle is frozen; at 10 %,
// 0.95 % of cycles; at 15 %, 5.4 % (6.4 % around 490, 3.9 % around 110, 2.9 %
// around 1400), the longest run the 8 cycles of the dwell. An excursion that
// lasts the dwell lands a swap on noise -- 2 in 50 runs at 10 %, 36 at 15 %
// around 530 -- and moves the throughput key with the k2 key.
//
// A move by the margin is not frozen; its writes are clamped instead
// (marginClamp).
//
// Nor is k2's settle test: k2Unsettled reads the frozen window as its
// throughput reference (serviceRateOnRecordLocked) at full weight, as it did
// before, so on a swap to shorter outputs a reading's rate is compared with
// the bucket being left. That is the k2 window's own hold (#396).
func muSwapFrozen(r throughputReading, pending bool) throughputReading {
	if pending && r.rate > 0 {
		r.borrowed = true
	}
	return r
}

// SwitchMuMarginClamp is the replay switch for the clamp on readings written
// past the margin toward shorter outputs (#415). On, as marginClamp says.
// Off, those readings are written as read.
const SwitchMuMarginClamp = "mu-margin-clamp"

// marginClamp is the reading written to the throughput window under key: mu,
// or, when clamp (outputKeys.muShorterPastMargin), the lower of mu and the
// window's median; ok is false when clamp is set and the window has no
// median, and nothing is written.
//
// A length past the margin but short of the swap line moves the key only
// once the smoothed length crosses the margin: 5 min 15 s for 530 -> 420
// tokens, 13 min 45 s for 530 -> 470 (the 400-485 band out of "long"). Until
// then the key names the longer bucket, and a shorter shape's readings --
// about 1.26x the requests/s at 420 -- would raise its window at full
// authority, to price the fleet too high when it returns to the longer
// shape: mu's side that breaks the SLO. The tracker-keyed key moved at once
// on a shift 20 % off its anchor, and never on a smaller one. Clamped to the
// median, a reading never raises the window by being appended: one at or
// under the median, in place of any value, leaves the median where it was or
// lower. A sample still being folded across pods (recordSaturatedThroughputFrom
// keeps a mean of the pods' readings within ThroughputSampleSpacing) can
// still move by the pods already in it: a review probe over 180 000 clamped
// writes on three pods saw the median rise over the previous step on 0.17 %
// of them, and never over its value when the clamping began. Toward longer
// outputs the readings are lower than the window's, the safe side, and are
// written as read; so is every reading when the length is within the margin.
// The median is read under one lock and the write takes another: a write
// that starts a new episode (EpisodeGap) is clamped to the old episode's
// median, which is lower only.
//
// Not a freeze: freezing on this cycle's length past the margin, in either
// direction and with pending swaps counted, froze 8.6 % of cycles at 6 %
// noise around 530 tokens, in runs of up to 26 cycles, and 22-33 % at
// 10-15 %.
func (a *SaturationAnalyzer) marginClamp(key string, mu float64, clamp bool) (float64, bool) {
	if !clamp {
		return mu, true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[key]
	if !ok || ra.Len() == 0 {
		return 0, false
	}
	return min(mu, ra.Median()), true
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

// carriedThroughput is the throughput reading for key, whose band left the
// bucket of left (outputKeys.muLeft): r, the reading key has
// (saturatedThroughputReadingUnder), unless key has no window of its own and
// left's window, usable under args, is lower than what r borrows -- then
// left's, borrowed. Lower only, as the k2 key's carry (seedMovedK2Key) is:
// mu is the learner whose too-high side breaks the SLO (analyzer-learner.md
// 3.4), and r's neighbour is the nearest bucket, the shorter on a tie, whose
// mu is the higher. So a key that moved from "long" to "medium", with
// "short" measured too, is priced from "long", not "short". A key that
// moved to a longer bucket is carried no lower than its neighbour: that is
// the bucket it left, whose mu over-states the new one, as a neighbour's
// always did (nearestSaturatedThroughput). Either is a borrowed figure: the
// derived mu, where there is one, prices instead (useDerived), and the floor
// may hold the fleet on it but not grow it.
func (a *SaturationAnalyzer) carriedThroughput(key, left, args string, r throughputReading) throughputReading {
	// r's bucket is key's own when key has a window, or one put aside under
	// other arguments (saturatedThroughputReadingUnder): not carried over.
	if left == "" || left == key || r.bucket == bucketOf(key) {
		return r
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[left]
	if !ok || ra.Len() == 0 || !a.throughputWindowUsable(left, args) {
		return r
	}
	// r found the nearest window of all, so with left's usable it has a rate.
	rate := ra.Median()
	if !(rate > 0) || !(rate < r.rate) {
		return r
	}
	ra.TouchAt(a.now())
	return throughputReading{rate: rate, bucket: bucketOf(left), samples: ra.Len(), borrowed: true}
}
