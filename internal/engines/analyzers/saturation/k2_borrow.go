package saturation

import (
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The lower bound a k2 history key may be priced below its own figure: a
// compatible sibling's measured k2, or the variant's own k2 measured under
// engine arguments it no longer runs. See
// docs/proposals/learned-state-sharing-by-similarity.md, "One lower bound per
// key".
//
// ONE RECORD PER KEY. Each own historyKey (variant AND bucket) has at most
// one lowerBound, whatever its figure comes from. There used to be three
// mechanisms meaning the same thing -- a borrow clock, a restored prior and
// the state beside the clock -- and forgetting a variant, turning the borrow
// off, trimming the snapshot and checking the image each knew some of them
// and not the others. Every such gap was a bug once the three met.
//
// Two rules make it safe, and each answers one direction of being wrong.
//
// LOWER ONLY. The figure is used only where it is below what the variant
// would otherwise get (P3 derived, or P4 k1), so it can never raise capacity:
// whatever the donor measured, the variant orders at least as many replicas
// as it would without it. A too-HIGH k2 is the expensive error -- fewer
// replicas, a saturated fleet, a TTFT tail that does not recover -- and this
// rule means a lower bound cannot introduce one.
//
// IT EXPIRES. A too-LOW k2 does not correct itself. k2 is measured only at
// saturation (P1-obs needs the queue over its threshold), and a low k2 orders
// extra replicas, so each runs light, nothing saturates, nothing is measured,
// and the low figure also blocks the scale-down that would let a replica
// saturate. A donor's figure can be right for the donor and too low for the
// borrower, and the variant's own figure from other arguments can be too low
// for the arguments it runs now, so neither may hold forever: after
// BorrowedK2Hold it relaxes linearly to the variant's own figure over
// BorrowedK2Relax. As it rises the fleet thins until one replica saturates
// and the variant measures k2 for itself, which displaces the bound at once
// (P1, then P2 on its own window). The worst case of the relaxation is
// therefore the variant's behaviour without a bound, reached hours later
// rather than at once.
//
// The record is per BUCKET. A variant whose traffic moves to an output bucket
// it has never measured gets that bucket's own hold and relaxation, from the
// donor's figure FOR that bucket. Keyed per variant instead, a variant past
// its first 6.5 hours jumped straight to its own k1 in every new bucket --
// measured in review at 3.2x the donor's figure, with no hold and no gradual
// rise. The cost of keying per bucket is at most one hold per output bucket a
// variant passes through, extra replicas for a bounded time, which the k2
// asymmetry ranks as the cheaper error.
//
// THE CLOCK COUNTS SERVING TIME. It advances only on cycles where a Ready
// replica of the variant's own serves the bucket (noteBorrowServing). A
// variant parked at zero, or whose traffic is in another bucket, does not use
// the figure, so the hold and the relaxation wait for it: measured on the
// wall clock, a canary parked overnight woke fully relaxed and spent none of
// the protection it was given. A warm-pool bridge, or a Pod that is not
// Ready, reads a bound but neither starts its clock nor moves it -- neither
// is the variant serving. With disableK2Borrow on, a bound running on a
// sibling's figure is not in use, so its clock waits too; one running on the
// variant's own previous figure is in use, and runs.
//
// A SPENT BOUND STAYS SPENT until there is new evidence (lowerBound.rearms):
// the variant's engine ARGUMENTS changed -- ArgsFingerprint, so an image-only
// rollout is not evidence about capacity -- or a figure now on offer is more
// than BorrowedK2RearmChange below what the bound last ran on. A change of
// arguments is new evidence about a SIBLING's figure, which is measured
// afresh; it is not about the variant's OWN previous figure, which is the
// same measurement whatever the variant runs next. That one restarts only
// when a newer figure replaces it (rearmPending): restarted on every change,
// a figure measured once kept a full hold for as long as the arguments kept
// changing faster than the bound relaxed. A donor that
// measured higher, a donor that went and came back, and the passage of time
// are not new evidence: dropping a spent clock used to re-arm the borrow at
// full strength once a week, about 42 GPU-hours per replica per bucket on
// whole 8-GPU nodes. The record goes when its variant does
// (EvictStaleHistory, on the variant's liveness); a controller restart keeps
// it through the learned-state snapshot, if that is on.
//
// THE VARIANT'S OWN PREVIOUS FIGURE is what a k2 window written under other
// engine arguments becomes (k2WindowUnderLocked), whether the arguments
// changed while the controller ran or while it was down: a restore installs
// each window with the arguments it was written under, and the first read
// moves it here. It takes the same two rules, and it is not a borrow:
// disableK2Borrow does not reach it, and forgetting the variant drops it.

const (
	// BorrowedK2Hold is how long a lower bound is used at full strength
	// before it starts to relax: long enough to cover a canary's first hours,
	// when it most needs a measured figure and is least likely to saturate.
	BorrowedK2Hold = 30 * time.Minute
	// BorrowedK2Relax is how long the relaxation to the variant's own figure
	// takes once the hold is over. Slow on purpose: capacity rises by a
	// fraction of the gap per cycle, so the fleet thins a replica at a time
	// and the first saturation is a short, measured one rather than a cliff.
	BorrowedK2Relax = 6 * time.Hour
	// BorrowedK2TickGap is the least the longest gap between two serving
	// cycles the clock counts as serving time can be; the analyzer uses three
	// optimisation intervals when that is longer (WithOptimizationInterval).
	// Room for a missed cycle or two. A longer gap -- the variant parked, or
	// serving another bucket -- is paused time, except for the allowance
	// itself: the clock counts up to this much of any gap, so each pause adds
	// at most one allowance of serving time to the bound.
	BorrowedK2TickGap = 5 * time.Minute
	// BorrowedK2RearmChange is how far below the figure a spent bound last
	// ran on a figure must be before the bound re-arms on it. Well above a
	// window's cycle-to-cycle drift, so a donor that is merely still
	// measuring does not restart a bound. Downward only: a higher figure can
	// only make the variant less conservative, which its own relaxation
	// already did.
	BorrowedK2RearmChange = 0.2
	// BorrowKvTotalTolerance is how far apart a sibling's measured KV cache
	// total and the borrower's may be, as a fraction of the larger, for the
	// sibling's k2 to be a candidate. Two engines the parameters call
	// compatible can still size their caches differently -- a flag no field
	// reads, another checkpoint under the same path, another driver -- and a
	// k2 is a token count: borrowed across caches ~2x apart it describes
	// neither. Equal parameters on the same hardware land within a fraction
	// of a percent, so 3% refuses a different cache without refusing a
	// re-profiled one.
	BorrowKvTotalTolerance = 0.03
)

// lowerBound is the one lower bound a k2 history key keeps (see above): its
// clock, what it last ran on, and the variant's own figure from engine
// arguments it no longer runs, if it has one.
type lowerBound struct {
	// armed says the bound has priced a serving cycle, so since and tick run.
	// A record that only holds a previous figure is not armed until a Ready
	// replica of the variant's own is priced from it.
	armed bool
	// since is the start of the hold, moved forward by every gap past the
	// tick gap, so tick-since is serving time only.
	since time.Time
	// tick is the last serving cycle.
	tick time.Time
	// seen is when the record was last read, ticked or its variant noted
	// present. It is swept on it, with the variant's other state.
	seen time.Time
	// donor is the variant the bound last ran on: a sibling's name, or the
	// variant's own when own is true.
	donor string
	own   bool
	// donorK2 and args are the evidence the bound last ran on: the figure
	// and the variant's engine arguments (boundArgs), set when it is armed
	// and refreshed on every serving cycle while it runs. argsVersion is the
	// FingerprintVersion args was hashed under: a fingerprint from another
	// version differs whatever the engine runs, so it is no evidence.
	donorK2     int64
	args        string
	argsVersion int
	// offered is the lowest figure on offer while the bound ran -- the one it
	// ran on, or a sibling's it did not take because a higher one was there.
	// A spent bound is judged against it: a figure that was available all
	// along is not new evidence when the one in front of it goes.
	offered int64
	// prior is the variant's own k2 measured under engine arguments it no
	// longer runs, 0 when there is none. priorSince is when it became one, and priorAge how
	// long ago its window had last been written then; the figure's age is
	// priorAge plus the time since priorSince.
	prior      int64
	priorSince time.Time
	priorAge   time.Duration
	// priorUsed is when the previous figure was last offered to a decision:
	// it ages on it, as the window it stands in for ages on its last read.
	priorUsed time.Time
	// figureWritten is when the window the variant's own previous figure
	// came from was last written, kept after the figure is cleared: a
	// rollback puts that window back as the variant's own, and a re-roll
	// sets the same window aside again. Not written since, it is the figure
	// the bound already ran on, not a newer one, and does not restart it
	// (notePreviousFigureLocked) -- each undo and redo otherwise re-armed a
	// spent figure at full strength on no new measurement.
	figureWritten time.Time
	// rearmPending says a newer previous figure replaced the one the bound
	// ran on, or the arguments changed under a running bound
	// (notePreviousFigureLocked): its next serving cycle arms it afresh,
	// with its whole hold. Restarted in place instead, the first serving
	// cycle read the time since the restart as a gap and moved the start
	// forward by all of it but one tick gap -- half the hold on a five-minute
	// interval.
	rearmPending bool
}

// clearPrior drops the variant's previous figure, and the restart it was
// waiting to complete (rearmPending): the variant's own window retired it, or
// a forget did, and the mark left behind re-armed a spent sibling borrow at
// full strength the next time the bucket's window was empty -- on no new
// evidence.
func (lb *lowerBound) clearPrior() {
	lb.prior, lb.priorSince, lb.priorAge, lb.priorUsed = 0, time.Time{}, 0, time.Time{}
	lb.rearmPending = false
}

// boundArgs is the arguments a lower bound records as evidence: the
// configuration's ArgsFingerprint when it was read completely, and "" -- no
// evidence either way -- when it was not or is unknown.
func boundArgs(params *capacity.EngineParams) string {
	if !params.Complete() {
		return ""
	}
	return params.ArgsFingerprint()
}

// argsChanged says whether args, the variant's arguments now, are evidence
// against what the bound last ran under: both known, hashed under this
// FingerprintVersion, and different.
func (lb *lowerBound) argsChanged(args string) bool {
	return args != "" && lb.args != "" && lb.argsVersion == capacity.FingerprintVersion && args != lb.args
}

// newArgs says whether args, the variant's arguments now, are new evidence for
// a bound offered a figure that is, or is not, the variant's own: they changed
// (argsChanged), and the figure is a sibling's, or the bound last ran on one.
// The variant's own previous figure is the same measurement under any
// arguments the variant runs next; only a newer figure replacing it is new
// (rearmPending).
func (lb *lowerBound) newArgs(args string, own bool) bool {
	return lb.argsChanged(args) && (!own || !lb.own)
}

// rearms says whether a spent bound may start again on a figure of donorK2
// under the variant's arguments args: the arguments are new evidence
// (newArgs), or the figure is more than BorrowedK2RearmChange below the
// lowest it had on offer -- a donor that measured lower, or a new, lower
// donor. A figure already on offer while it ran is not new.
func (lb *lowerBound) rearms(donorK2 int64, args string, own bool) bool {
	if lb.newArgs(args, own) {
		return true
	}
	ref := lb.donorK2
	if lb.offered > 0 && lb.offered < ref {
		ref = lb.offered
	}
	return float64(donorK2) < (1-BorrowedK2RearmChange)*float64(ref)
}

// tickLocked advances the clock to a serving cycle at now, moving the start
// forward by any gap longer than tickGap. Callers hold a.mu.
func (lb *lowerBound) tickLocked(now time.Time, tickGap time.Duration) {
	if gap := now.Sub(lb.tick); !lb.tick.IsZero() && gap > tickGap {
		lb.since = lb.since.Add(gap - tickGap)
	}
	lb.tick, lb.seen = now, now
}

// k2Borrow is the outcome of borrowedK2 for one replica's decision.
type k2Borrow struct {
	k2      int64         // the figure to use: between the bound's and own
	donor   string        // the lending variant's name; the variant's own when own
	donorK2 int64         // what the donor measured
	relaxed float64       // 0 at full strength, 1 when fully relaxed to own
	age     time.Duration // how long the bound has run, in serving time
	// own is true when the figure is the variant's own, measured under
	// engine arguments it no longer runs (lowerBound.prior): reported as
	// P2-prior, not as a borrow.
	own bool
	// figureAge is how long ago the figure was last measured, for own;
	// a sibling's is read from its window (k2WriteAge).
	figureAge time.Duration
}

// source is the k2 source a replica priced from b reports.
func (b k2Borrow) source() capacity.K2Source {
	if b.own {
		return capacity.K2SrcPrior
	}
	return capacity.K2SrcBorrowed
}

// relaxedAt is how far a bound of the given age has relaxed: 0 through the
// hold, then linearly to 1 over the relaxation.
func relaxedAt(age time.Duration) float64 {
	if age <= BorrowedK2Hold {
		return 0
	}
	return float64(age-BorrowedK2Hold) / float64(BorrowedK2Relax)
}

// splitVariantKey splits a historyKey into its namespace, variant and the rest
// (model|accelerator|gpus|role|bucket|qN). No field contains "|", so the first
// two separators are the variant's.
func splitVariantKey(key string) (namespace, variant, rest string, ok bool) {
	parts := strings.SplitN(key, "|", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// WithOptimizationInterval sizes the longest gap between two serving cycles
// the clock counts as serving time to the controller's optimisation interval:
// three intervals, never less than BorrowedK2TickGap. With the constant alone,
// an interval above it read every gap between two cycles as a pause, counted
// only five minutes of each, and stretched a 6.5-hour bound over many more
// hours of wall time.
func (a *SaturationAnalyzer) WithOptimizationInterval(interval time.Duration) *SaturationAnalyzer {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.borrowTickGap = max(BorrowedK2TickGap, 3*interval)
	// The analyzer's own settle tracker judges absence at the same cadence
	// the engine's does (settle.Tolerance).
	if a.ownSettle != nil {
		a.ownSettle.SetInterval(interval)
	}
	return a
}

// BorrowTickGap reports the longest gap between two serving cycles a clock
// counts as serving time, for the caller that logs it.
func (a *SaturationAnalyzer) BorrowTickGap() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return max(a.borrowTickGap, BorrowedK2TickGap)
}

// holdLowerBounds holds every lower bound as it stands while held is true: a
// serving cycle reads a bound, but does not arm, advance, re-arm or end one.
// The engine holds them from its first cycle until the learned-state snapshot
// has been read: a cycle that ran while the first read failed armed a bound
// at full strength, and the restore then kept that one -- it was live -- over
// the document's, spent or half relaxed, for another whole hold and
// relaxation of extra replicas. A bound read while held is priced as an
// unarmed one, at full strength: the conservative side, for as long as the
// engine holds them.
func (a *SaturationAnalyzer) holdLowerBounds(held bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.boundsHeld = held
}

// noteBorrowServing advances the clock of a history key's lower bound on a
// cycle where a Ready replica of the variant's own serves it. A key with no
// armed bound is left alone: serving does not start a bound, pricing from one
// does. siblings is false when the model has the borrow turned off: a bound
// running on a sibling's figure is then not in use and waits.
//
// Nor does a bound on the variant's own previous figure while the key has a
// window again: a bound is read only when there is none (computeK2). A
// rollback puts the variant's window back with that bound still armed;
// ticked through the hours it served on the window, the bound was spent by
// a re-roll that had run on it for minutes, and the re-roll -- the same
// figure, so it does not restart the bound (notePreviousFigureLocked) --
// was priced at k1. A bound on a sibling's figure runs on: a re-roll is new
// evidence for it and re-arms it (lowerBound.newArgs).
func (a *SaturationAnalyzer) noteBorrowServing(key string, now time.Time, siblings bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.noteBorrowServingLocked(key, now, siblings)
}

// noteBorrowServingLocked is noteBorrowServing for a caller holding a.mu.
func (a *SaturationAnalyzer) noteBorrowServingLocked(key string, now time.Time, siblings bool) {
	lb := a.lowerBounds[key]
	if lb == nil || !lb.armed || (!siblings && !lb.own) || a.boundsHeld {
		return
	}
	if ra, ok := a.computeCapacityHistory[key]; lb.own && ok && ra.Len() > 0 {
		return
	}
	lb.tickLocked(now, max(a.borrowTickGap, BorrowedK2TickGap))
}

// borrowedK2WithKv returns the lower bound for a replica whose own window (ownKey)
// is empty, if one applies: the variant's own previous figure, or -- unless
// siblings is false (disableK2Borrow) -- a sibling's measured k2 from a
// variant in the same namespace, on the same model, accelerator, GPU count,
// role, bucket and queue threshold, whose engine parameters are
// capacity-compatible (EngineParams.IsCapacityCompatible). Only a figure
// below own is a candidate. Among several the highest is taken: it is the
// least far below own, so the least replicas spent on a figure that may not
// describe the variant. The variant's own previous figure wins a tie.
//
// The donor's window is read without Touch: borrowing must not keep a donor's
// measurement alive past its own owner's use of it.
//
// own is the CAPACITY the variant would have without the bound: min(k1, its
// own k2). Compared against its own k2 alone, a donor between k1 and a derived
// k2 above k1 was "lower", was reported as P2-borrow and started the clock,
// while capacity -- clamped to k1 either way -- did not move. ok is false when
// there is nothing below own, or the bound has run its course and there is no
// new evidence.
//
// serving says whether the replica asking is a Ready replica of the variant's
// own, not mid-rollout (admission.serving; every rollout counts). Only
// such a replica arms a bound, advances it, re-arms a spent one or ends it;
// any other reads it as it stood at the variant's last serving cycle, at full
// strength if it never served or if there is new evidence it has not acted on
// yet.
//
// ownKv is the replica's live KV cache total, 0 when it reported none; the
// cache gate compares a sibling's against it, and against the store's record
// only when it is 0. The record is still the one derived from the deployment
// on a new variant's first cycle, and on the first cycle after a rollout that
// changed the cache, with no total or the old one -- and that is the cycle
// that arms a bound.
func (a *SaturationAnalyzer) borrowedK2WithKv(ownKey, modelID string, own int64, ownParams *capacity.EngineParams, ownKv int64, serving, siblings bool) (k2Borrow, bool) {
	if own <= 0 || ownParams == nil {
		return k2Borrow{}, false
	}
	namespace, variant, rest, ok := splitVariantKey(ownKey)
	if !ok {
		return k2Borrow{}, false
	}

	type candidate struct {
		variant string
		k2      int64
		own     bool
		age     time.Duration
		tag     string
	}
	var siblingFigures []candidate
	best := candidate{}
	now := a.now()
	a.mu.Lock()
	if siblings {
		for key, ra := range a.computeCapacityHistory {
			ns, v, r, ok := splitVariantKey(key)
			if !ok || ns != namespace || v == variant || r != rest || ra.Len() == 0 {
				continue
			}
			if k2 := int64(ra.Average()); k2 > 0 && k2 < own {
				siblingFigures = append(siblingFigures, candidate{variant: v, k2: k2, tag: a.k2WindowArgs[key]})
			}
		}
	}
	if lb := a.lowerBounds[ownKey]; lb != nil && lb.prior > 0 && lb.prior < own {
		best = candidate{variant: variant, k2: lb.prior, own: true, age: lb.priorAge + now.Sub(lb.priorSince)}
		lb.priorUsed = now
	}
	a.mu.Unlock()

	// The store has its own lock; checked outside a.mu.
	ownTotal := ownKv
	if ownTotal <= 0 && len(siblingFigures) > 0 {
		if ownRec := a.capacityStore.Get(namespace, modelID, variant); ownRec != nil {
			ownTotal = ownRec.TotalKvCapacityTokens
		}
	}
	// lowest is the lowest figure on offer: what a spent bound is judged
	// against (lowerBound.offered).
	lowest := best.k2
	for _, c := range siblingFigures {
		rec := a.capacityStore.Get(namespace, modelID, c.variant)
		if rec == nil || !ownParams.IsCapacityCompatible(rec.EngineParams) || !kvTotalsAgree(ownTotal, rec) ||
			!donorWindowCurrent(c.tag, rec.EngineParams) {
			continue
		}
		if lowest == 0 || c.k2 < lowest {
			lowest = c.k2
		}
		if c.k2 > best.k2 || (c.k2 == best.k2 && !best.own && c.variant < best.variant) {
			best = c
		}
	}
	if best.k2 == 0 {
		return k2Borrow{}, false
	}

	at := func(age time.Duration) k2Borrow {
		f := relaxedAt(age)
		return k2Borrow{
			k2:        best.k2 + int64(f*float64(own-best.k2)),
			donor:     best.variant,
			donorK2:   best.k2,
			relaxed:   f,
			age:       age,
			own:       best.own,
			figureAge: best.age,
		}
	}
	args := boundArgs(ownParams)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.boundsHeld {
		// The stored lower bounds are not in yet (HoldLowerBounds): read as
		// an unarmed bound, at full strength, but record nothing a restore
		// would then lose to.
		serving = false
	}
	lb := a.lowerBounds[ownKey]
	runOn := func() {
		lb.donor, lb.own, lb.donorK2 = best.variant, best.own, best.k2
		lb.args, lb.argsVersion = args, capacity.FingerprintVersion
		if lb.offered <= 0 || lowest < lb.offered {
			lb.offered = lowest
		}
	}
	arm := func() (k2Borrow, bool) {
		if lb == nil {
			lb = &lowerBound{}
			a.lowerBounds[ownKey] = lb
		}
		lb.armed, lb.since, lb.tick, lb.seen = true, now, now, now
		lb.offered, lb.rearmPending = 0, false
		runOn()
		return at(0), true
	}
	if lb == nil || !lb.armed {
		if !serving {
			return at(0), true
		}
		return arm()
	}
	lb.seen = now
	if lb.rearmPending || lb.newArgs(args, best.own) {
		// A newer previous figure, or arguments that changed while the bound
		// ran on a sibling's figure or since it was spent: new evidence, and
		// a fresh bound -- not what is left of one armed on another engine
		// hours ago. A replica that may not arm reads it as the fresh bound
		// it is about to be. The variant's own figure, unchanged, runs on
		// across a change of arguments (newArgs).
		if serving {
			return arm()
		}
		return at(0), true
	}
	if serving {
		lb.tickLocked(now, max(a.borrowTickGap, BorrowedK2TickGap))
	}
	if serving && (lb.args == "" || lb.argsVersion != capacity.FingerprintVersion) {
		// Restored from a document that did not record what the bound ran
		// on, or recorded it under another FingerprintVersion, or armed
		// under a configuration not read completely: today's arguments are
		// the evidence from here on, not a reason to re-arm. Only the
		// arguments: the figure the bound ran on is what a spent bound is
		// judged against, and refreshed here before rearms read it, a
		// figure 50% lower never re-armed a bound with no arguments
		// recorded. Under a configuration still not read completely args is
		// "" again, so this does no more than it did.
		lb.args, lb.argsVersion = args, capacity.FingerprintVersion
	}
	end := now
	if !serving {
		end = lb.tick
	}
	age := end.Sub(lb.since)
	if relaxedAt(age) < 1 {
		if serving {
			// What a spent bound is later judged against is what it last
			// ran on, not what it was armed on: a change that happened while
			// it ran is not new evidence once it ends.
			runOn()
		}
		return at(age), true
	}
	if serving && lb.donorK2 <= 0 && lb.offered <= 0 {
		// Spent with no record of the figure it ran on -- a clock restored
		// from a document older than that record: today's figure is the
		// reference from here on, not new evidence against one. Without it a
		// spent bound compared every figure with zero and never re-armed.
		lb.donor, lb.own, lb.donorK2, lb.offered = best.variant, best.own, best.k2, lowest
		lb.clearPrior()
		return k2Borrow{}, false
	}
	if lb.rearms(best.k2, args, best.own) {
		if serving {
			return arm()
		}
		return at(0), true
	}
	if serving {
		// Spent: a previous figure has run its course and is not offered
		// again. A later change of arguments makes a new one.
		lb.clearPrior()
	}
	return k2Borrow{}, false
}

// kvTotalsAgree reports whether a sibling's measured KV cache total is within
// BorrowKvTotalTolerance of the borrower's (own, its measured total). A total
// either side has not measured -- no record, or one derived from the
// deployment before a pod reported -- does not refuse: the parameters'
// compatibility is then all there is, as before the gate.
func kvTotalsAgree(own int64, donor *capacity.Record) bool {
	if own <= 0 || donor == nil || donor.TotalKvCapacityTokens <= 0 {
		return true
	}
	o, d := float64(own), float64(donor.TotalKvCapacityTokens)
	return max(o, d)-min(o, d) <= BorrowKvTotalTolerance*max(o, d)
}

// spendLowerBoundsLocked ends every lower bound whose key starts with prefix
// and drops the variant's own previous figures: an armed bound is marked as
// if its relaxation had run its course, keeping what it last ran on, and one
// that was never armed goes. A spent bound re-arms only on new evidence
// (lowerBound.rearms), where a dropped one re-armed a sibling's figure at full
// strength on the variant's next serving cycle. Callers hold a.mu.
func (a *SaturationAnalyzer) spendLowerBoundsLocked(prefix string, now time.Time) {
	spentAt := now.Add(-(BorrowedK2Hold + BorrowedK2Relax))
	for key, lb := range a.lowerBounds {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		lb.clearPrior()
		if !lb.armed {
			delete(a.lowerBounds, key)
			continue
		}
		if lb.since.After(spentAt) {
			lb.since = spentAt
		}
		lb.tick, lb.seen = now, now
	}
}

// k2WriteAge is how long ago the figure a k2 came from was last measured: the
// variant's own window for P1-obs and P2-hist, the donor's for P2-borrow, and
// the window the variant's previous figure came from for P2-prior. It reads
// WriteAge and never touches: asking how old a figure is must not make it look
// used. ok is false for P3 and P4, which read no window, and for a window that
// is gone.
func (a *SaturationAnalyzer) k2WriteAge(historyKey string, src capacity.K2Source, b k2Borrow) (time.Duration, bool) {
	key := historyKey
	switch src {
	case capacity.K2SrcObserved, capacity.K2SrcHistorical:
	case capacity.K2SrcPrior:
		return b.figureAge, b.own
	case capacity.K2SrcBorrowed:
		namespace, _, rest, ok := splitVariantKey(historyKey)
		if !ok || b.donor == "" {
			return 0, false
		}
		key = namespace + "|" + b.donor + "|" + rest
	default:
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.computeCapacityHistory[key]
	if !ok || ra.Len() == 0 {
		return 0, false
	}
	return ra.WriteAgeAt(a.now()), true
}

// logBorrowedK2 is the k2-decision line for a lower bound. It names the donor
// and what the variant would have used instead, because the bound is standing
// state, not a one-cycle event: a fleet holding more replicas than its load
// needs is explained by this line for as long as the bound lasts.
func logBorrowedK2(logger logr.Logger, b k2Borrow, modelID, namespace, variantName, historyKey string, own int64, ownSource capacity.K2Source) {
	logger.V(logging.DEFAULT).Info("k2-decision",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"priority", b.source().String(), "historyKey", historyKey,
		"donor", b.donor, "donorK2", b.donorK2,
		"ownCapacity", own, "ownPriority", ownSource.String(),
		"relaxed", b.relaxed, "borrowAgeSeconds", int64(b.age.Seconds()),
		"k2", b.k2)
}
