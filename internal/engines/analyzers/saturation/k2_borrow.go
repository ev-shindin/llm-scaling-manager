package saturation

import (
	"math"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Borrowing a sibling's k2: a variant with no saturated reading of its own
// starting from one a compatible variant measured. See
// docs/proposals/learned-state-sharing-by-similarity.md.
//
// Two rules make it safe, and each answers one direction of being wrong.
//
// LOWER ONLY. The borrowed figure is used only where it is below what the
// variant would otherwise get (P3 derived, or P4 k1), so it can never raise
// capacity: whatever the donor measured, the variant orders at least as many
// replicas as it would without borrowing. A too-HIGH k2 is the expensive
// error -- fewer replicas, a saturated fleet, a TTFT tail that does not recover
// -- and this rule means borrowing cannot introduce one.
//
// IT EXPIRES. A too-LOW k2 does not correct itself. k2 is measured only at
// saturation (P1-obs needs the queue over its threshold), and a low k2 orders
// extra replicas, so each runs light, nothing saturates, nothing is measured,
// and the low figure also blocks the scale-down that would let a replica
// saturate. A donor's figure can be right for the donor and too low for the
// borrower, so it may not hold forever: after BorrowedK2Hold it relaxes
// linearly to the variant's own figure over BorrowedK2Relax. As it rises the
// fleet thins until one replica saturates and the variant measures k2 for
// itself, which displaces the borrow at once (P1, then P2 on its own window).
// The worst case of the relaxation is therefore the variant's no-borrow
// behaviour, reached hours later rather than at once.
//
// The clock is per BUCKET: one per own history key. A variant whose traffic
// moves to an output bucket it has never measured gets that bucket's own hold
// and relaxation, from the donor's figure FOR that bucket. Keyed per variant
// instead, a variant past its first 6.5 hours jumped straight to its own k1 in
// every new bucket -- measured in review at 3.2x the donor's figure, with no
// hold and no gradual rise -- which is the TTFT-direction gap this borrow
// exists to close. The cost of keying per bucket is the other direction: at
// most one hold per output bucket a variant passes through, extra replicas
// for a bounded time, which the k2 asymmetry ranks as the cheaper error.
//
// THE CLOCK COUNTS SERVING TIME. It advances only on cycles where a Ready
// replica of the variant's own serves the bucket (noteBorrowServing). A
// variant parked at zero, or whose traffic is in another bucket, does not use
// the borrowed figure, so the hold and the relaxation wait for it: measured on
// the wall clock, a canary parked overnight woke fully relaxed and spent none
// of the protection it was given. A warm-pool bridge, or a Pod that is not
// Ready, reads a borrow but neither starts its clock nor moves it -- neither
// is the variant serving.
//
// A SPENT BORROW STAYS SPENT. A clock is not reset by a donor that goes and
// comes back, and it is no longer dropped a retention after the relaxation
// ended: that drop used to re-arm the borrow at full strength once a week, for
// a variant that had already lived on its own figure for days -- a periodic
// cost with no new evidence behind it, about 42 GPU-hours per replica per
// bucket on whole 8-GPU nodes. The clock now keeps what the borrow last ran
// on -- the donor's figure and the borrower's engine configuration on its
// last serving cycle -- and re-arms only when one of them has changed since
// it was spent (borrowState.rearms): a donor that has measured something
// materially different, or a borrower redeployed with other flags, is a new
// question; the passage of time is not. Compared with what the borrow was
// ARMED on instead, a donor still filling its window when the borrow started,
// or a flag changed in its third hour, re-armed it at full strength on the
// cycle it ended. The clock goes when its variant does (EvictStaleHistory, on
// the variant's liveness). A controller restart keeps it through the
// learned-state snapshot, if that is on; so does forgetting the variant on
// request (ForgetVariant), which ends a running borrow instead of dropping it.

const (
	// BorrowedK2Hold is how long a borrowed k2 is used at full strength
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
	// at most one allowance of serving time to the borrow.
	BorrowedK2TickGap = 5 * time.Minute
	// BorrowedK2RearmChange is how far a donor's figure must have moved, as a
	// fraction of the figure the borrow last ran on, before a spent borrow
	// re-arms. Well above a window's cycle-to-cycle drift, so a donor that is
	// merely still measuring does not restart a borrow.
	BorrowedK2RearmChange = 0.2
)

// borrowState is what a borrow's clock keeps beside its start
// (k2BorrowedSince): when the variant last served the bucket, when anything
// last read the clock or found the variant present, and what the borrow last
// ran on.
type borrowState struct {
	// tick is the last serving cycle; the start is moved forward by any gap
	// past BorrowedK2TickGap, so start-to-tick is serving time only.
	tick time.Time
	// seen is when the clock was last read, ticked or its variant noted
	// present. The clock is swept on it, with the variant's other state.
	seen time.Time
	// donorK2 and args are the evidence the borrow last ran on: the donor's
	// figure and the borrower's engine configuration (Fingerprint), set when
	// it is armed and refreshed on every serving cycle while it runs.
	donorK2 int64
	args    string
}

// rearms says whether a spent borrow may start again: the donor's figure has
// moved by more than BorrowedK2RearmChange, or the borrower's engine
// configuration is not the one the borrow last ran under.
func (st *borrowState) rearms(donorK2 int64, args string) bool {
	if args != st.args {
		return true
	}
	return math.Abs(float64(donorK2-st.donorK2)) > BorrowedK2RearmChange*float64(st.donorK2)
}

// k2Borrow is the outcome of borrowedK2 for one replica's decision.
type k2Borrow struct {
	k2      int64         // the figure to use: between the donor's and own
	donor   string        // the lending variant's name
	donorK2 int64         // what the donor measured
	relaxed float64       // 0 at full strength, 1 when fully relaxed to own
	age     time.Duration // how long this variant has been borrowing, in serving time
}

// relaxedAt is how far a borrow of the given age has relaxed: 0 through the
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
// the borrow clock counts as serving time to the controller's optimisation
// interval: three intervals, never less than BorrowedK2TickGap. With the
// constant alone, an interval above it read every gap between two cycles as a
// pause, counted only five minutes of each, and stretched a 6.5-hour borrow
// over many more hours of wall time.
func (a *SaturationAnalyzer) WithOptimizationInterval(interval time.Duration) *SaturationAnalyzer {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.borrowTickGap = max(BorrowedK2TickGap, 3*interval)
	return a
}

// BorrowTickGap reports the longest gap between two serving cycles a borrow
// clock counts as serving time, for the caller that logs it.
func (a *SaturationAnalyzer) BorrowTickGap() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return max(a.borrowTickGap, BorrowedK2TickGap)
}

// noteBorrowServing advances the borrow clock of a history key on a cycle
// where a Ready replica of the variant's own serves it. A key with no clock is
// left alone: serving does not start a borrow, borrowing does.
func (a *SaturationAnalyzer) noteBorrowServing(key string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.noteBorrowServingLocked(key, now)
}

// noteBorrowServingLocked is noteBorrowServing for a caller holding a.mu.
func (a *SaturationAnalyzer) noteBorrowServingLocked(key string, now time.Time) {
	since, ok := a.k2BorrowedSince[key]
	if !ok {
		return
	}
	st := a.k2BorrowState[key]
	if st == nil {
		// A clock restored from a snapshot written before this state
		// existed: no serving time is known, so the next gap counts from now.
		st = &borrowState{tick: now}
		a.k2BorrowState[key] = st
	}
	tickGap := max(a.borrowTickGap, BorrowedK2TickGap)
	if gap := now.Sub(st.tick); !st.tick.IsZero() && gap > tickGap {
		a.k2BorrowedSince[key] = since.Add(gap - tickGap)
	}
	st.tick = now
	st.seen = now
}

// borrowedK2 returns a sibling's measured k2 for a replica whose own window
// (ownKey) is empty, if one applies: a variant in the same namespace, on the
// same model, accelerator, GPU count, role, bucket and queue threshold, whose
// engine parameters are capacity-compatible (EngineParams.IsCapacityCompatible:
// the twelve fields that decide a replica's capacity, max-num-seqs and the
// batch-token budget among them), and whose figure is below own. Among
// several, the highest is taken: it is the least far below own, so the least
// replicas spent on a figure that may be someone else's.
//
// The donor's window is read without Touch: borrowing must not keep a donor's
// measurement alive past its own owner's use of it.
//
// own is the CAPACITY the variant would have without borrowing: min(k1, its
// own k2). Compared against its own k2 alone, a donor between k1 and a derived
// k2 above k1 was "lower", was reported as P2-borrow and started the clock,
// while capacity -- clamped to k1 either way -- did not move. ok is false when
// there is nothing to borrow, or the relaxation has run its course and nothing
// has changed since.
//
// serving says whether the replica asking is a Ready replica of the variant's
// own. Only such a replica starts a clock, advances it, or re-arms a spent
// one; any other reads the borrow as the clock stood at the variant's last
// serving cycle, at full strength if it never served.
func (a *SaturationAnalyzer) borrowedK2(ownKey, modelID string, own int64, ownParams *capacity.EngineParams, serving bool) (k2Borrow, bool) {
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
	}
	var candidates []candidate
	a.mu.Lock()
	for key, ra := range a.computeCapacityHistory {
		ns, v, r, ok := splitVariantKey(key)
		if !ok || ns != namespace || v == variant || r != rest || ra.Len() == 0 {
			continue
		}
		if k2 := int64(ra.Average()); k2 > 0 && k2 < own {
			candidates = append(candidates, candidate{variant: v, k2: k2})
		}
	}
	a.mu.Unlock()

	// The store has its own lock; checked outside a.mu.
	best := candidate{}
	for _, c := range candidates {
		rec := a.capacityStore.Get(namespace, modelID, c.variant)
		if rec == nil || !ownParams.IsCapacityCompatible(rec.EngineParams) {
			continue
		}
		if c.k2 > best.k2 || (c.k2 == best.k2 && c.variant < best.variant) {
			best = c
		}
	}
	if best.k2 == 0 {
		return k2Borrow{}, false
	}

	at := func(age time.Duration) k2Borrow {
		f := relaxedAt(age)
		return k2Borrow{
			k2:      best.k2 + int64(f*float64(own-best.k2)),
			donor:   best.variant,
			donorK2: best.k2,
			relaxed: f,
			age:     age,
		}
	}
	now := a.now()
	args := ownParams.Fingerprint()
	a.mu.Lock()
	defer a.mu.Unlock()
	arm := func() (k2Borrow, bool) {
		a.k2BorrowedSince[ownKey] = now
		a.k2BorrowState[ownKey] = &borrowState{tick: now, seen: now, donorK2: best.k2, args: args}
		return at(0), true
	}
	since, started := a.k2BorrowedSince[ownKey]
	if !started {
		if !serving {
			return at(0), true
		}
		return arm()
	}
	if serving {
		a.noteBorrowServingLocked(ownKey, now)
		since = a.k2BorrowedSince[ownKey]
	}
	st := a.k2BorrowState[ownKey]
	if st == nil {
		st = &borrowState{}
		a.k2BorrowState[ownKey] = st
	}
	st.seen = now
	if st.args == "" {
		// Restored from a snapshot that did not record what the borrow was
		// armed on: today's figures are the evidence from here on.
		st.donorK2, st.args = best.k2, args
	}
	end := now
	if !serving && !st.tick.IsZero() {
		end = st.tick
	}
	age := end.Sub(since)
	if relaxedAt(age) < 1 {
		if serving {
			// What a spent borrow is later judged against is what it last
			// ran on, not what it was armed on: a change that happened while
			// it ran is not new evidence once it ends.
			st.donorK2, st.args = best.k2, args
		}
		return at(age), true
	}
	if serving && st.rearms(best.k2, args) {
		return arm()
	}
	return k2Borrow{}, false
}

// spendBorrowsLocked ends every borrow clock whose key starts with prefix, as
// if its relaxation had run its course, and keeps what it last ran on. A
// spent clock re-arms only on new evidence (borrowState.rearms), where a
// dropped one re-armed at full strength on the variant's next serving cycle.
// Callers hold a.mu.
func (a *SaturationAnalyzer) spendBorrowsLocked(prefix string, now time.Time) {
	spent := now.Add(-(BorrowedK2Hold + BorrowedK2Relax))
	for key, since := range a.k2BorrowedSince {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if since.After(spent) {
			a.k2BorrowedSince[key] = spent
		}
		st := a.k2BorrowState[key]
		if st == nil {
			// No record of what it ran on: borrowedK2 takes the next
			// cycle's figures as the evidence from there.
			st = &borrowState{}
			a.k2BorrowState[key] = st
		}
		st.tick, st.seen = now, now
	}
}

// logBorrowedK2 is the k2-decision line for a borrowed figure. It names the
// donor and what the variant would have used instead, because the borrow is
// standing state, not a one-cycle event: a fleet holding more replicas than its
// load needs is explained by this line for as long as the borrow lasts.
func logBorrowedK2(logger logr.Logger, b k2Borrow, modelID, namespace, variantName, historyKey string, own int64, ownSource capacity.K2Source) {
	logger.V(logging.DEFAULT).Info("k2-decision",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"priority", capacity.K2SrcBorrowed.String(), "historyKey", historyKey,
		"donor", b.donor, "donorK2", b.donorK2,
		"ownCapacity", own, "ownPriority", ownSource.String(),
		"relaxed", b.relaxed, "borrowAgeSeconds", int64(b.age.Seconds()),
		"k2", b.k2)
}
