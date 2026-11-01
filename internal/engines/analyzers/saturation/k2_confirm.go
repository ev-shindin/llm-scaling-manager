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
// (k2Pending) and writes nothing to the window: the reading is labelled
// P1-obs-unconfirmed. The next cycle with an admitted reading under the key
// writes the held readings, at the time they were read, and its own. That is
// averaging, not a test of agreement: the window is two readings wide before
// it prices, so one reading is half of it, not all. The second reading must
// come from a later cycle -- a later Analyze of the model, whatever the clock
// reads: a load step lands on every replica at once, so a sibling's reading
// in the same cycle is not a second look. A later cycle's one-minute peak
// still shares most of its minute with the first's (45 s at a 15 s cycle), so
// the second is partly a resample. Held readings count while the window they
// would start could still take them: within the episode gap
// (capacity.Horizons.EpisodeGap) of when they were read, and under the engine
// arguments they were read under; a reading held under arguments the variant
// no longer runs is dropped (window_args.go says why a window is per
// arguments).
//
// The held cycle is priced no higher than the reading: at the lower of the
// reading and what the chain gives (unadmittedPrice): the chain past a held reading may be k1, or a
// window from before the episode gap, far above what the replica holds while
// it is full. And the settle test reads usable held readings as the k2 on
// record, ahead of any window (heldOnRecordLocked): a window that is fresh
// because of the episode gap still holds its old figure, and the reading
// that follows the hold is judged against the hold, as it would be against a
// window of it.
//
// Held readings are not in the learned-state snapshot: a controller restart
// during the held cycle drops them, and the variant's next admitted reading
// is held again, one more cycle.
//
// Nothing else waits. The held reading passed every admission test a P1-obs
// one does, so the mu beside it is judged as on any P1-obs cycle: the hold is
// k2's. A window already holding readings, or restored from the learned-state
// snapshot, takes each reading at once, as before.
//
// The cost: a variant with no k2 for its bucket -- a cold start, a new
// bucket, a controller restart without the learned-state snapshot, or a
// window past the episode gap -- records its k2 from its second admitted
// cycle, not its first, and its first is priced at most at its reading.

// SwitchK2FirstReadingWaits is the replay switch for the wait (#392): on from
// the constructor; a replay turns it off (SetSwitchesForReplay), and the
// first reading starts the window at once, as before.
const SwitchK2FirstReadingWaits = "k2-first-reading-waits-for-a-second"

// k2ReasonObsUnconfirmed labels a saturated reading held for a later cycle's:
// the first of a window that has none. Not a capacity.K2Source: the chain
// falls through, and the cycle is priced at most at the reading.
const k2ReasonObsUnconfirmed = "P1-obs-unconfirmed"

// k2PendingReadings is the readings of the first cycle that would have
// started a k2 window, held until a later cycle's admitted reading writes
// them.
type k2PendingReadings struct {
	// at is when they were read, cycle the Analyze call that read them
	// (SaturationAnalyzer.analyzeSeq), and args the engine arguments they
	// were read under (windowArgs).
	at       time.Time
	cycle    uint64
	args     string
	readings []int64
}

// k2HoldFirstLocked reports whether a saturated reading of key at now, under
// engine arguments args, is held rather than written: the switch is on and
// the window it would start has nothing on record (fresh). It holds the
// reading, and returns false with the readings held from an earlier cycle
// when this one confirms them: the caller writes them, then its own. Readings
// held past the episode gap, or under other arguments, are replaced, not
// written, and returned as dropped for the caller to log. Callers hold a.mu.
func (a *SaturationAnalyzer) k2HoldFirstLocked(key string, reading int64, fresh bool, args string, now time.Time) (held bool, confirmed, dropped k2PendingReadings) {
	if !a.k2FirstReadingWaits || !fresh {
		delete(a.k2Pending, key)
		return false, k2PendingReadings{}, k2PendingReadings{}
	}
	p, ok := a.k2Pending[key]
	usable := ok && p.usable(args, now, a.horizons.EpisodeGap)
	if usable && p.cycle != a.analyzeSeq {
		delete(a.k2Pending, key)
		return false, *p, k2PendingReadings{}
	}
	if !usable {
		if ok {
			dropped = *p
		}
		p = &k2PendingReadings{at: now, cycle: a.analyzeSeq, args: args}
		a.k2Pending[key] = p
	}
	p.readings = append(p.readings, reading)
	return true, k2PendingReadings{}, dropped
}

// usable reports whether readings held at p.at under p.args may still start
// the window of a reading under args at now: the same arguments, within the
// episode gap.
func (p *k2PendingReadings) usable(args string, now time.Time, gap time.Duration) bool {
	return p.args == args && now.Sub(p.at) <= gap
}

// heldOnRecordLocked is the mean of the readings held for key from an earlier
// cycle, usable under args at now, zero when there are none: the k2 on record
// for the settle test while a window's first readings are held. A sibling's
// reading held this cycle is not on record for this cycle's readings.
// Callers hold a.mu.
func (a *SaturationAnalyzer) heldOnRecordLocked(key, args string, now time.Time) float64 {
	p, ok := a.k2Pending[key]
	if !ok || p.cycle == a.analyzeSeq || !p.usable(args, now, a.horizons.EpisodeGap) || len(p.readings) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range p.readings {
		sum += float64(r)
	}
	return sum / float64(len(p.readings))
}

// sweepK2PendingLocked drops held readings past the episode gap: no reading
// can confirm them any more. Called by evictStaleHistory; callers hold a.mu.
func (a *SaturationAnalyzer) sweepK2PendingLocked(now time.Time) {
	maps.DeleteFunc(a.k2Pending, func(_ string, p *k2PendingReadings) bool {
		return now.Sub(p.at) > a.horizons.EpisodeGap
	})
}
