package saturation

import (
	"maps"
	"time"
)

// k2's first reading waits for a second (#392).
//
// A k2 window that is empty -- a bucket never measured, or one whose last
// write is past the episode gap, so the next write starts it again -- was
// priced from its first admitted reading alone: one reading was the whole
// mean, and the next moved it by half its difference. Measured on the
// property case first-sample/k2/second-reading: 296 552 from the first
// reading, 264 778 after a second, 11 %. A k2 that is too low never corrects
// itself (it orders replicas, so nothing saturates again), so a figure one
// reading wide is the one most worth not adopting.
//
// So the first cycle that would start a window holds its readings aside
// (k2Pending) and writes nothing: the reading is refused as
// P1-obs-unconfirmed, and k2 falls through the chain as for any refused
// reading. The next cycle with an admitted reading under the key writes the
// held readings and its own together. The confirming reading must come from
// a later cycle -- a later Analyze of the model, whatever the clock reads: a
// load step lands on every replica at once, so a sibling's reading in the
// same cycle is no confirmation of a limit. Held readings
// count while the window they would start could still take them: within the
// episode gap (capacity.Horizons.EpisodeGap) of when they were read.
//
// Nothing else waits. The held reading passed every admission test a P1-obs
// one does, so the mu beside it is judged as on any P1-obs cycle: the hold is
// k2's, and mu's own first-sample rules are its own (mu_return.go). A window
// already holding readings, or restored from the learned-state snapshot,
// takes each reading at once, as before.
//
// The cost is one cycle: a variant with no k2 for its bucket -- a cold start,
// a new bucket, or a controller restart without the learned-state snapshot --
// is priced from the chain's next source (P2-borrow, P2-prior, P3-k2 or P4-k1)
// for one more cycle after its first saturated reading.

// SwitchK2FirstReadingWaits is the replay switch for the wait (#392): on from
// the constructor; a replay turns it off (SetSwitchesForReplay), and the
// first reading starts the window at once, as before.
const SwitchK2FirstReadingWaits = "k2-first-reading-waits-for-a-second"

// k2ReasonObsUnconfirmed labels a saturated reading held until a later
// cycle's reading confirms it: the first of a window that has none. Not a
// capacity.K2Source: the chain falls through.
const k2ReasonObsUnconfirmed = "P1-obs-unconfirmed"

// k2PendingReadings is the readings of the first cycle that would have
// started a k2 window, held until a later cycle confirms them.
type k2PendingReadings struct {
	// at is when they were read, and cycle the Analyze call that read them
	// (SaturationAnalyzer.analyzeSeq).
	at       time.Time
	cycle    uint64
	readings []int64
}

// k2HoldFirstLocked reports whether a saturated reading of key at now is
// held rather than written: the switch is on and the window it would start
// has nothing on record (fresh). It holds the reading, and returns false with
// the readings held from an earlier cycle when this one confirms them: the
// caller writes them, then its own. Callers hold a.mu.
func (a *SaturationAnalyzer) k2HoldFirstLocked(key string, reading int64, fresh bool, now time.Time) (held bool, confirmed []int64) {
	if !a.k2FirstReadingWaits || !fresh {
		delete(a.k2Pending, key)
		return false, nil
	}
	p, ok := a.k2Pending[key]
	if ok && p.cycle != a.analyzeSeq && now.Sub(p.at) <= a.horizons.EpisodeGap {
		delete(a.k2Pending, key)
		return false, p.readings
	}
	if !ok || p.cycle != a.analyzeSeq {
		// None held, or held too long ago to start this window.
		p = &k2PendingReadings{at: now, cycle: a.analyzeSeq}
		a.k2Pending[key] = p
	}
	p.readings = append(p.readings, reading)
	return true, nil
}

// sweepK2PendingLocked drops held readings past the episode gap: no reading
// can confirm them any more. Called by evictStaleHistory; callers hold a.mu.
func (a *SaturationAnalyzer) sweepK2PendingLocked(now time.Time) {
	maps.DeleteFunc(a.k2Pending, func(_ string, p *k2PendingReadings) bool {
		return now.Sub(p.at) > a.horizons.EpisodeGap
	})
}
