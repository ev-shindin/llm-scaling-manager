package saturation

import (
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The engine arguments a k2 or throughput window was written under.
//
// The window keys carry no configuration: a variant redeployed with other
// flags kept reading the k2 and service rate it measured under the old ones,
// with full authority and no end date -- a capacity rise left the fleet
// holding replicas it no longer needed, a fall priced it too high until
// enough saturated cycles diluted the window. A restart was the only reset,
// and an accidental one.
//
// So each window records the arguments it was written under
// (ArgsFingerprint: the configuration without the image, which a routine
// release changes and capacity does not follow), BESIDE the key rather than
// in it -- a new key would have started every window and every snapshot cold
// once more. When a window is next read or written under other arguments:
//
//   - it is put aside (the stash), under the arguments it was written under,
//     for as long as a window is retained. A variant rolled back to those
//     arguments gets it back as its own window, k2 and service rate, with
//     full authority: a rollback is not a new engine, and treating it as one
//     relaxed the variant's own measurement away, or priced it from the
//     figure of the arguments it was rolled back FROM.
//   - a k2 window's figure becomes the bucket's lower bound, as the variant's
//     own previous figure (lowerBound.prior): used only below what the
//     variant would otherwise get, relaxing to it on serving time, the same
//     two rules as a sibling's borrowed k2. It REPLACES an older previous
//     figure, lower or not: it is what the variant measured last, under the
//     arguments it just left, and an older one is from arguments further
//     back. Kept on the lower of the two, a figure from several generations
//     back won every later change and was handed a fresh hold each time --
//     2.25x the replicas where the last measurement said otherwise. A
//     rollback is not this case: the stash puts the window measured under
//     the arguments rolled back to in front of any previous figure. The
//     change of arguments restarts the bound's clock: it is new evidence,
//     and a bound that had been running for hours on something else must
//     not hand the new arguments only what was left of its relaxation. A
//     previous figure ages on its last USE, as the window it stands in for
//     ages on its last read, and goes a bucket retention after a decision
//     last read it (EvictStaleHistory). The variant's first measurement under
//     the arguments it runs retires it (Priority 1): from then on its own
//     window is the figure.
//   - a throughput window is not borrowed from a sibling, but the key's own
//     from before the change holds the floor, hold-only -- the floor may
//     keep the fleet on it, never grow it -- until the new arguments measure
//     their own, for at most BorrowedK2Hold + BorrowedK2Relax of serving
//     and without being touched (saturatedThroughputReadingUnder). Dropped,
//     the change switched off the one floor that can only add replicas;
//     unbounded, a change that raised the service rate kept the fleet the
//     old engine needed for as long as the load was steady.
//
// The same rule covers a restart. A restore installs each window with the
// arguments it was written under, and its first read here decides, so a
// window learned before a live rollout and exported after it is not restored
// as a figure of the new arguments.
//
// A configuration that was not read completely is still a configuration: its
// windows are tagged with its fingerprint behind incompletePrefix, so moving
// from it to a complete one, from a complete one to it, or between two that
// differ in what WAS read, is a change like any other. Only two equal
// incomplete fingerprints are taken as the same engine, as before tags
// existed. A window with no tag at all -- learned before tags existed -- is
// taken as written under the arguments the variant runs now, and adopts them.
// A variant whose configuration is unknown (no record yet) changes nothing.

// incompletePrefix marks a tag taken from a configuration that was not read
// completely (EngineParams.Complete): its fingerprint hashes an unread flag
// as its default, so it never equals a complete configuration's.
const incompletePrefix = capacity.IncompleteArgsPrefix

// windowArgs is the arguments a variant's windows are checked and written
// under: its ArgsFingerprint, behind incompletePrefix when its configuration
// was not read completely, or "" when it is unknown.
func windowArgs(params *capacity.EngineParams) string {
	return params.ArgsTag()
}

// stashKey is where a window written under args is put aside for key.
func stashKey(key, args string) string { return key + "#" + args }

// sameFigure says whether a window last written at written is still the
// figure a bound recorded as written at figure: not written since. Not an
// equality: a restore clamps a window clock written slightly ahead of this
// leader's to its now (notFuture), and leaves the bound's record of it, so
// the same window then reads as written BEFORE its figure -- which only an
// unwritten window can be -- and was taken for a newer one.
func sameFigure(figure, written time.Time) bool {
	return !figure.IsZero() && !written.IsZero() && !written.After(figure)
}

// stashedThroughput is a throughput window put aside with what travels with it.
type stashedThroughput struct {
	window    *capacity.RollingAverage
	sampledAt time.Time
	lastRead  float64
	// heldFor is how long the floor has held a fleet on this window since it
	// was put aside, counted in serving time; heldTick is when it last did
	// (throughput_floor.go, saturatedThroughputReadingUnder).
	heldFor  time.Duration
	heldTick time.Time
}

// heldMark is how long a put-aside throughput window had held the floor when
// a rollback took it back, and when the window was last written then.
type heldMark struct {
	heldFor time.Duration
	written time.Time
}

// k2WindowUnderLocked checks the k2 window of key against args (above): a
// window written under other arguments is put aside and its figure becomes
// the key's previous figure; one put aside under args is put back. Callers
// hold a.mu.
func (a *SaturationAnalyzer) k2WindowUnderLocked(key, args string) {
	if args == "" {
		return
	}
	ra, ok := a.computeCapacityHistory[key]
	tag, tagged := a.k2WindowArgs[key]
	switch {
	case !ok:
		delete(a.k2WindowArgs, key)
	case !tagged || tag == "":
		a.k2WindowArgs[key] = args
		return
	case tag == args:
		return
	default:
		// A window restored from a document that recorded no arguments is
		// tagged with what no configuration runs (legacyArgsPrefix): it
		// becomes a previous figure and is not kept for a rollback that
		// cannot come.
		if !strings.HasPrefix(tag, legacyArgsPrefix) {
			a.k2Stash[stashKey(key, tag)] = ra
		}
		a.notePreviousFigureLocked(key, ra)
		delete(a.computeCapacityHistory, key)
		delete(a.k2WindowArgs, key)
	}
	if back, ok := a.k2Stash[stashKey(key, args)]; ok {
		delete(a.k2Stash, stashKey(key, args))
		a.computeCapacityHistory[key] = back
		a.k2WindowArgs[key] = args
		// The variant has its own window under the arguments it runs again,
		// with its full authority: a previous figure -- of these arguments,
		// or of the ones just left -- is not a figure for them any more.
		if lb := a.lowerBounds[key]; lb != nil {
			lb.clearPrior()
		}
	}
}

// notePreviousFigureLocked makes window ra, written under tag, the key's
// previous figure in place of any older one, and restarts a running bound's
// clock: the arguments changed. The restart is completed by the bound's next
// serving cycle, which arms it afresh (rearmPending), so the time to then --
// the rollout -- is not read as a gap. The window the bound's own figure
// already came from, not written since (lowerBound.figureWritten), is not a
// newer figure, and leaves the clock where it stands. Callers hold a.mu.
func (a *SaturationAnalyzer) notePreviousFigureLocked(key string, ra *capacity.RollingAverage) {
	now := a.now()
	lb := a.lowerBounds[key]
	written := ra.LastWritten()
	same := lb != nil && lb.armed && lb.own && sameFigure(lb.figureWritten, written)
	if lb != nil && lb.armed && !same {
		lb.since, lb.tick, lb.seen = now, now, now
		lb.rearmPending = true
	}
	if ra.Len() == 0 {
		return
	}
	v := int64(ra.Average())
	if v <= 0 {
		return
	}
	if lb == nil {
		lb = &lowerBound{}
		a.lowerBounds[key] = lb
	}
	lb.prior, lb.priorSince, lb.priorAge, lb.priorUsed, lb.seen = v, now, ra.WriteAgeAt(now), now, now
	lb.figureWritten = written
}

// throughputWindowUnderLocked checks the throughput window of key against
// args (above): one written under other arguments is put aside, and one put
// aside under args is put back. Callers hold a.mu.
func (a *SaturationAnalyzer) throughputWindowUnderLocked(key, args string) {
	if args == "" {
		return
	}
	ra, ok := a.saturatedThroughput[key]
	tag, tagged := a.throughputWindowArgs[key]
	switch {
	case !ok:
		delete(a.throughputWindowArgs, key)
	case !tagged || tag == "":
		a.throughputWindowArgs[key] = args
		return
	case tag == args:
		return
	default:
		if !strings.HasPrefix(tag, legacyArgsPrefix) {
			st := stashedThroughput{
				window: ra, sampledAt: a.throughputSampledAt[key], lastRead: a.throughputLastRead[key],
			}
			// The same window, not written since a rollback took it back,
			// keeps the hold it had already given: a fresh one on every
			// undo and redo held the fleet on the same service rate again.
			if m, ok := a.throughputHeld[stashKey(key, tag)]; ok && m.written.Equal(ra.LastWritten()) {
				st.heldFor = m.heldFor
			}
			delete(a.throughputHeld, stashKey(key, tag))
			a.throughputStash[stashKey(key, tag)] = st
		}
		delete(a.saturatedThroughput, key)
		delete(a.throughputSampledAt, key)
		delete(a.throughputLastRead, key)
		delete(a.throughputPodReads, key)
		delete(a.throughputMuChoice, key)
		delete(a.throughputWindowArgs, key)
	}
	if back, ok := a.throughputStash[stashKey(key, args)]; ok {
		delete(a.throughputStash, stashKey(key, args))
		if back.heldFor > 0 {
			a.throughputHeld[stashKey(key, args)] = heldMark{heldFor: back.heldFor, written: back.window.LastWritten()}
		}
		a.saturatedThroughput[key] = back.window
		a.throughputWindowArgs[key] = args
		if !back.sampledAt.IsZero() {
			a.throughputSampledAt[key] = back.sampledAt
		}
		if back.lastRead > 0 {
			a.throughputLastRead[key] = back.lastRead
		}
	}
}

// throughputWindowUsable says whether a neighbouring bucket's throughput
// window may be read under args: one written under other arguments is not a
// reading of the engine the variant runs. Callers hold a.mu.
func (a *SaturationAnalyzer) throughputWindowUsable(key, args string) bool {
	tag := a.throughputWindowArgs[key]
	return args == "" || tag == "" || tag == args
}

// donorWindowCurrent says whether a sibling's k2 window, written under tag,
// describes the engine the sibling runs now (donorParams): a window it has
// not read since its arguments changed -- parked, serving another bucket, or
// restored from before a rollout -- is a figure of an engine nobody runs. An
// untagged window is taken as current, as its owner would take it.
func donorWindowCurrent(tag string, donorParams *capacity.EngineParams) bool {
	if tag == "" {
		return true
	}
	cur := windowArgs(donorParams)
	return cur != "" && tag == cur
}
