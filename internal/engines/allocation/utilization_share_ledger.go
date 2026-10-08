package allocation

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"time"
)

// ShareTransferState is a transfer's place in the state machine of
// docs/proposals/utilization-share-optimizer.md §6.3. "Released" is a step
// inside one cycle, not a state the ledger holds between cycles.
type ShareTransferState int

const (
	// ShareReleasing: the donor's target is lowered; the receiver's is not.
	ShareReleasing ShareTransferState = iota
	// ShareFilling: released; the receiver's target is raised and its pods are
	// not yet scheduled.
	ShareFilling
)

func (s ShareTransferState) String() string {
	switch s {
	case ShareReleasing:
		return "releasing"
	case ShareFilling:
		return "filling"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// ShareTransferOutcome is how a transfer left the ledger.
type ShareTransferOutcome string

const (
	// ShareOutcomeDone counts a transfer whose receiver holds the GPUs, and
	// an idle fill that landed.
	ShareOutcomeDone ShareTransferOutcome = "done"
	// ShareOutcomeFillTimeout counts a transfer whose receiver did not take
	// the released GPUs within the fill timeout; it keeps its target.
	ShareOutcomeFillTimeout ShareTransferOutcome = "fill-timeout"
	// ShareOutcomeCancelled counts a release called off on a clear reversal,
	// while it was still free (section 6.7 rule 3).
	ShareOutcomeCancelled ShareTransferOutcome = "cancelled"
	// ShareOutcomeAborted counts a release that did not land within the
	// release timeout; its donor is restored and backs off.
	ShareOutcomeAborted ShareTransferOutcome = "aborted"
	// ShareOutcomeRedirected counts a transfer a wake claimed (section 6.3);
	// it stays in the ledger as a release with no receiver.
	ShareOutcomeRedirected ShareTransferOutcome = "redirected"
	// ShareOutcomeWrongPod counts a node-planned transfer whose donor shrank
	// by a different pod than the one marked: the GPUs came free, but not
	// where the plan put the receiver's pod, so the receiver is not raised
	// (section 6.5, "check which pod actually went").
	ShareOutcomeWrongPod ShareTransferOutcome = "wrong-pod"
)

// ShareTransfer moves one receiver replica: Donor gives GPUs, Receiver gets
// them once they are released (§6.2). Stage 2 begins with a single donor role
// per transfer; per-pod donor sets (§6.5) extend Donor without changing the
// state machine.
type ShareTransfer struct {
	ID       string
	Donor    string // role key; empty for a fill from idle GPUs
	Receiver string // role key
	// DonorVariant and ReceiverVariant are the variants the transfer acts on
	// (ShareGroup.Give, ShareGroup.Grow); empty when the caller sizes by role.
	DonorVariant, ReceiverVariant string
	// DonorPods are the donor pods marked to be removed (namespace/name), so a
	// cancel can unmark them (§6.5).
	DonorPods []string
	// DonorLowered is true once the donor's target has been lowered for this
	// transfer: when its pods were marked, or on a restart when its marked pod
	// was not yet terminating. Only then does a cancel or abort raise it back.
	DonorLowered bool
	// GPUs is one receiver replica.
	GPUs int
	// DonorGPUs is one donor replica: at least GPUs. Without node information
	// a receiver pod is funded only by one donor pod at least its size (§6.5),
	// and the surplus returns to the idle budget when it is released.
	DonorGPUs int
	// Entitled transfers -- owed floors, fixed consumers, reserve refill --
	// bypass the z admission rule and the holds, and set no hold (§6.2).
	Entitled bool
	// Urgent receivers are below their need (§6.4).
	Urgent bool
	// PlannedPods are the donor pods a node-aware plan chose (namespace/name):
	// the ones to mark, because the holes are planned where they run (§6.5).
	// Empty when any of the donor's pods will do.
	PlannedPods []string
	// SetID links the transfers of one donor set (§6.5): several donor
	// replicas funding one receiver replica, each receiver pod by a donor pod
	// of its own. The primary carries the receiver and its ID is the set's;
	// the contributors carry no receiver. The receiver is raised only when
	// every member has released. Empty for a single-donor transfer.
	SetID string

	State   ShareTransferState
	Started time.Time
	// Deadline is when the current state times out.
	Deadline time.Time
	// donorBase and receiverBase are the donor's held GPUs when the transfer
	// started and the receiver's when it entered Filling; release and fill are
	// observed against them.
	donorBase, receiverBase int
	// fillingSince is when the transfer entered Filling: a fill that
	// completes later is inside its receiverBase only if it landed before.
	fillingSince time.Time
	// receiverBaseUnset is set when the receiver was out of the group as the
	// transfer entered Filling: its held count then was not seen, and a base of
	// zero would complete the fill the moment it returned. The base is taken
	// from the first cycle it is seen.
	receiverBaseUnset bool
	// setReleased is, on a set's primary, the GPUs its contributors have
	// released while it still waits: promised to its receiver. setPending is
	// how many contributors have not released yet. A primary whose pending
	// contributor has left the ledger without releasing -- cancelled,
	// forgotten, aborted -- is aborted too: its receiver's replica will never
	// have all its holes.
	setReleased, setPending int
	// plannedRunning is set, each cycle, while one of PlannedPods still runs;
	// plannedUnknown while one of them could not be read.
	plannedRunning, plannedUnknown bool
}

// IsSetPrimary reports whether t carries a donor set's receiver.
func (t ShareTransfer) IsSetPrimary() bool { return t.SetID != "" && t.SetID == t.ID }

// ShareTransferEnd records a transfer that left the ledger this cycle.
type ShareTransferEnd struct {
	Transfer ShareTransfer
	Outcome  ShareTransferOutcome
}

// ShareTimings are the derived timings the ledger runs with (§8.4).
type ShareTimings struct {
	// Window is the donor's HPA scale-down stabilization window: while a
	// transfer is younger than this, no donor pod has been removed and a cancel
	// is free.
	Window         time.Duration
	ReleaseTimeout time.Duration
	FillTimeout    time.Duration
	// ReversalHold blocks a role from moving the opposite way, measured from a
	// transfer's start or cancellation.
	ReversalHold time.Duration
	// SwingWindow: a role whose transfers change direction twice within it is
	// planned on its mean need for the next SwingWindow.
	SwingWindow time.Duration
}

type shareMove struct {
	at       time.Time
	received bool
}

type shareNeedSample struct {
	at   time.Time
	need float64
}

// ShareLedger holds a group's transfers across cycles, and the per-role
// history the anti-oscillation rules of §6.7 need. It is owned by the engine,
// not by an optimizer instance (§6.6), and is not safe for concurrent use.
type ShareLedger struct {
	transfers []*ShareTransfer
	nextID    int
	// incarnation prefixes the IDs this ledger issues, so a transfer started
	// after a restart never shares an ID with one restored from a mark.
	incarnation string
	// undo is what Start changed in the move history, per transfer, until
	// ConfirmStarted; Forget puts it back.
	undo map[string]shareUndo
	// wakeHolds are the GPUs redirected to woken models (Redirect): free soon
	// on the nodes, but taken by a wake's pod. Idle fills and other wakes must
	// not count them until the hold expires.
	wakeHolds []shareWakeHold
	// aborts counts a donor's consecutive aborted releases, and giveAfter is
	// when it may give again: an abort means something outside WVA -- a
	// PodDisruptionBudget, a stuck finalizer, a raised ScaledObject floor --
	// kept its pods, and retrying at once would loop start/abort forever.
	aborts    map[string]int
	giveAfter map[string]time.Time
	// unsteerable are the donors backing off because their pods could not be
	// marked (MarkFailed), not because a release aborted.
	unsteerable map[string]bool
	// fillBlocked is, per receiver, why its last fill timed out
	// (release-taken or release-shape-mismatch) and until when it is reported.
	fillBlocked map[string]shareFillBlock
	// fillShort are the receivers the last idle fill left below their target:
	// the planner does not leave them to the fill again (SetFillShort).
	fillShort map[string]bool

	lastGave, lastGot map[string]time.Time
	moves             map[string][]shareMove
	swingUntil        map[string]time.Time
	needs             map[string][]shareNeedSample
	confirm           map[string]int
	// releases are the durations of recent completed releases, newest last,
	// for the measured release time (§8.4).
	releases []time.Duration
	// released are the transfers that entered Filling since TakeReleased was
	// last called: the cycle in which each receiver is raised.
	released []ShareTransfer
	// seen is when each role was last in the group or in a live transfer:
	// Retain forgets the history of a role gone longer than any window reads.
	seen map[string]time.Time
}

// shareReleaseSamples is how many completed releases the ledger remembers.
const shareReleaseSamples = 20

// shareUndo is the move history of a transfer's two roles before Start.
type shareUndo struct {
	donor, receiver string
	gave, got       time.Time
	hadGave, hadGot bool
	donorMoves      []shareMove
	receiverMoves   []shareMove
	donorSwing      time.Time
	receiverSwing   time.Time
}

// shareWakeHold is a redirected transfer's GPUs, held for the woken model:
// for as long as the release takes (id names the transfer, until is zero), then
// for hold more, while the wake's pod lands.
type shareWakeHold struct {
	gpus  int
	id    string
	hold  time.Duration
	until time.Time
}

// NewShareLedger returns an empty ledger.
func NewShareLedger() *ShareLedger {
	return &ShareLedger{
		incarnation: newIncarnation(),
		undo:        map[string]shareUndo{},
		aborts:      map[string]int{},
		giveAfter:   map[string]time.Time{},
		fillBlocked: map[string]shareFillBlock{},
		lastGave:    map[string]time.Time{},
		lastGot:     map[string]time.Time{},
		moves:       map[string][]shareMove{},
		swingUntil:  map[string]time.Time{},
		needs:       map[string][]shareNeedSample{},
		confirm:     map[string]int{},
		seen:        map[string]time.Time{},
		unsteerable: map[string]bool{},
	}
}

// Transfers returns the transfers in flight, oldest first.
func (l *ShareLedger) Transfers() []ShareTransfer {
	out := make([]ShareTransfer, 0, len(l.transfers))
	for _, t := range l.transfers {
		out = append(out, *t)
	}
	return out
}

// InFlight is how many transfers count against the concurrency limit: those
// with a donor. A fill from idle GPUs moves nothing and does not count.
func (l *ShareLedger) InFlight() int {
	n := 0
	for _, t := range l.transfers {
		if t.Donor != "" {
			n++
		}
	}
	return n
}

// StartFill records a receiver raised into idle GPUs: no donor, so it starts in
// Filling and ends when the receiver's pods are scheduled. Tracking it keeps
// the next cycle, which does not yet see those pods held, from filling the same
// GPUs again.
func (l *ShareLedger) StartFill(receiver, variant string, gpus int, held map[string]int, now time.Time, tm ShareTimings) ShareTransfer {
	l.nextID++
	t := &ShareTransfer{
		ID: fmt.Sprintf("%s-f%d-%s", l.incarnation, l.nextID, randomHex(4)), Receiver: receiver, ReceiverVariant: variant,
		GPUs: gpus, Entitled: true, State: ShareFilling, Started: now,
		Deadline: now.Add(tm.FillTimeout), receiverBase: held[receiver], fillingSince: now,
	}
	l.transfers = append(l.transfers, t)
	return *t
}

// Restore reinstates a Releasing transfer read back from its donor pods' marks
// after a restart (§6.3). heldDonor is what the donor holds now; its marked
// pods still hold their GPUs, so the release is observed against that.
func (l *ShareLedger) Restore(t ShareTransfer, heldDonor int) {
	if t.SetID == "" && t.DonorGPUs < t.GPUs {
		t.DonorGPUs = t.GPUs
	}
	t.State = ShareReleasing
	t.donorBase = heldDonor
	l.transfers = append(l.transfers, &t)
}

// TakeReleased returns the transfers that were released since the last call
// and forgets them.
func (l *ShareLedger) TakeReleased() []ShareTransfer {
	out := l.released
	l.released = nil
	return out
}

// Committed returns each role's committed allocation Ḡ (§6.1 step 2): what it
// holds, minus what it is still giving, plus what it is promised and does not
// yet hold.
func (l *ShareLedger) Committed(held map[string]int) map[string]int {
	out := make(map[string]int, len(held))
	for k, v := range held {
		out[k] = v
	}
	for _, t := range l.transfers {
		switch t.State {
		case ShareReleasing:
			out[t.Donor] -= t.DonorGPUs
			if t.Receiver != "" { // a reserve refill raises nobody
				out[t.Receiver] += t.GPUs
			}
		case ShareFilling:
			if t.Receiver != "" {
				out[t.Receiver] += t.GPUs
			}
		}
	}
	return out
}

// Promised is P (§6.2): GPUs released for a receiver that it does not yet hold.
func (l *ShareLedger) Promised() int {
	p := 0
	for _, t := range l.transfers {
		switch t.State {
		case ShareFilling:
			p += t.GPUs
		case ShareReleasing:
			p += t.setReleased
		}
	}
	return p
}

// Start records a newly admitted transfer, in Releasing.
func (l *ShareLedger) Start(t ShareTransfer, held map[string]int, now time.Time, tm ShareTimings) ShareTransfer {
	l.nextID++
	t.ID = fmt.Sprintf("%s-t%d-%s", l.incarnation, l.nextID, randomHex(4))
	if t.SetID == "" && t.DonorGPUs < t.GPUs {
		t.DonorGPUs = t.GPUs // a set member gives one donor replica of several
	}
	t.State = ShareReleasing
	t.Started = now
	t.Deadline = now.Add(tm.ReleaseTimeout)
	t.donorBase = held[t.Donor]
	l.transfers = append(l.transfers, &t)
	if !t.Entitled {
		u := shareUndo{
			donor: t.Donor, receiver: t.Receiver,
			donorMoves:    slices.Clone(l.moves[t.Donor]),
			receiverMoves: slices.Clone(l.moves[t.Receiver]),
			donorSwing:    l.swingUntil[t.Donor],
			receiverSwing: l.swingUntil[t.Receiver],
		}
		u.gave, u.hadGave = l.lastGave[t.Donor]
		u.got, u.hadGot = l.lastGot[t.Receiver]
		l.undo[t.ID] = u
		l.recordMove(t.Donor, false, now, tm)
		if t.Receiver != "" {
			l.recordMove(t.Receiver, true, now, tm)
		}
	}
	return t
}

// LinkSet makes primary the receiver-carrying member of a donor set and
// contributors its other members (§6.5).
func (l *ShareLedger) LinkSet(primary string, contributors ...string) {
	for _, t := range l.transfers {
		if t.ID == primary || slices.Contains(contributors, t.ID) {
			t.SetID = primary
		}
	}
	for _, t := range l.transfers {
		if t.ID == primary {
			t.setPending = len(contributors)
		}
	}
}

// Transfer returns the transfer with that ID.
func (l *ShareLedger) Transfer(id string) (ShareTransfer, bool) {
	for _, t := range l.transfers {
		if t.ID == id {
			return *t, true
		}
	}
	return ShareTransfer{}, false
}

// Cancel removes a transfer that is still Releasing and inside the window, so
// no donor pod has moved, and holds the pair (§6.3, §6.7 rule 3). It reports
// whether the transfer was cancelled.
func (l *ShareLedger) Cancel(id string, now time.Time, tm ShareTimings) bool {
	i := slices.IndexFunc(l.transfers, func(t *ShareTransfer) bool { return t.ID == id })
	if i < 0 {
		return false
	}
	t := l.transfers[i]
	if t.State != ShareReleasing || now.Sub(t.Started) >= tm.Window {
		return false
	}
	l.transfers = slices.Delete(l.transfers, i, i+1)
	if !t.Entitled {
		l.lastGave[t.Donor] = now
		l.lastGot[t.Receiver] = now
	}
	return true
}

// Observe advances every transfer from what the cluster shows this cycle and
// returns those that left the ledger. held is each role's GPUs held now,
// terminating pods included.
//
// Several transfers can draw on one donor, or fill one receiver, at once. They
// are released and filled in start order against a common base, so the first
// one completes when one replica's worth has moved, the second when two have.
func (l *ShareLedger) Observe(held map[string]int, now time.Time, tm ShareTimings) []ShareTransferEnd {
	var ended []ShareTransferEnd

	// Releasing -> Filling, per donor in start order. A set's primary waits
	// until every contributor has released too: its receiver is raised only
	// when the whole set's holes are open.
	given := map[string]int{}
	base := map[string]int{}
	releasable := map[*ShareTransfer]bool{}
	for _, t := range l.transfers {
		if t.State != ShareReleasing || t.Donor == "" {
			continue
		}
		if _, seen := held[t.Donor]; !seen {
			// Not planned this cycle -- its collection failed, or it is frozen:
			// nothing shows what it holds, and zero is not an answer. The
			// release timeout still bounds the wait.
			continue
		}
		if _, ok := base[t.Donor]; !ok {
			base[t.Donor] = t.donorBase
		}
		given[t.Donor] += t.DonorGPUs
		releasable[t] = held[t.Donor] <= base[t.Donor]-given[t.Donor]
	}
	// A donor that shrank while a planned pod still runs lost another pod:
	// the planned hole is not open, and never will be by this transfer.
	wrongPod := func(t *ShareTransfer) bool { return releasable[t] && t.plannedRunning }
	// Set IDs with a contributor not yet released -- including one whose
	// planned hole did not open, or cannot be read: its primary must not
	// release on the strength of the others. A wrong-pod contributor leaves
	// the ledger below, and the set is then broken.
	waiting := map[string]bool{}
	for t, ok := range releasable {
		if (!ok || wrongPod(t) || t.plannedUnknown) && t.SetID != "" && !t.IsSetPrimary() {
			waiting[t.SetID] = true
		}
	}
	// A set whose pending contributor is no longer in the ledger is broken:
	// its primary must not release alone (it is aborted below).
	live := map[string]int{}
	for _, t := range l.transfers {
		if t.State == ShareReleasing && t.SetID != "" && !t.IsSetPrimary() {
			live[t.SetID]++
		}
	}
	broken := func(t *ShareTransfer) bool { return t.IsSetPrimary() && t.setPending > live[t.ID] }
	for _, t := range l.transfers {
		// A planned pod that could not be read: neither released nor wrong
		// this cycle. The release timeout still bounds the wait.
		if !releasable[t] || (t.IsSetPrimary() && waiting[t.ID]) || broken(t) || wrongPod(t) || t.plannedUnknown {
			continue
		}
		if t.SetID != "" && !t.IsSetPrimary() {
			for _, p := range l.transfers {
				if p.ID == t.SetID && p.State == ShareReleasing {
					p.setReleased += t.DonorGPUs
					p.setPending--
				}
			}
		}
		l.releases = append(l.releases, now.Sub(t.Started))
		if len(l.releases) > shareReleaseSamples {
			l.releases = l.releases[len(l.releases)-shareReleaseSamples:]
		}
		t.State = ShareFilling
		t.Deadline = now.Add(tm.FillTimeout)
		for i := range l.wakeHolds {
			if h := &l.wakeHolds[i]; h.id == t.ID && h.until.IsZero() {
				h.until = now.Add(h.hold) // the hole is open: the wake's pod has hold to land
			}
		}
		_, seen := held[t.Receiver]
		t.receiverBase = held[t.Receiver]
		t.receiverBaseUnset = t.Receiver != "" && !seen
		t.fillingSince = now
		l.released = append(l.released, *t)
		l.rebaseDonor(t, now)
		delete(l.aborts, t.Donor)
		delete(l.giveAfter, t.Donor)
	}
	// A transfer released this cycle measures its receiver from now; one that
	// was already filling keeps its base. Filling -> Done, per receiver in start
	// order.
	got := map[string]int{}
	rbase := map[string]int{}
	keep := l.transfers[:0]
	var filled, wrong []*ShareTransfer
	for _, t := range l.transfers {
		_, receiverSeen := held[t.Receiver]
		switch {
		case t.State == ShareReleasing && broken(t):
			// Not the donor's failure: no back-off.
			ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeAborted})
			continue
		case t.State == ShareReleasing && wrongPod(t) && !t.plannedUnknown:
			// The donor did give; its GPUs return to the budget for the next
			// plan. No back-off: the donor released, and promptly.
			ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeWrongPod})
			wrong = append(wrong, t)
			continue
		case t.State == ShareReleasing && !now.Before(t.Deadline) && t.IsSetPrimary() && releasable[t]:
			// Its own donor released; a contributor did not. Not this
			// donor's failure: no back-off.
			ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeAborted})
			continue
		case t.State == ShareReleasing && !now.Before(t.Deadline):
			ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeAborted})
			if t.Donor != "" {
				// Back off one release timeout, doubling per consecutive
				// abort up to 16x; a release that lands resets it.
				l.aborts[t.Donor]++
				backoff := tm.ReleaseTimeout << min(l.aborts[t.Donor]-1, 4)
				l.giveAfter[t.Donor] = now.Add(backoff)
				delete(l.unsteerable, t.Donor)
			}
			continue
		case t.State == ShareFilling && (receiverSeen || t.Receiver == ""):
			// A receiver not planned this cycle shows nothing about what it
			// holds: only its fill timeout can end it. A transfer with no
			// receiver -- a contributor, a refill -- completes at its release.
			if t.receiverBaseUnset {
				// Seen for the first time since it entered Filling: measured
				// from now, and judged from the next cycle. A pod that landed
				// while it was unseen ends it at its fill timeout instead,
				// which leaves the receiver's target.
				t.receiverBase, t.receiverBaseUnset, t.fillingSince = held[t.Receiver], false, now
				break
			}
			if _, ok := rbase[t.Receiver]; !ok {
				rbase[t.Receiver] = t.receiverBase
			}
			got[t.Receiver] += t.GPUs
			if held[t.Receiver] >= rbase[t.Receiver]+got[t.Receiver] {
				ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeDone})
				filled = append(filled, t)
				continue
			}
			if !now.Before(t.Deadline) {
				ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeFillTimeout})
				continue
			}
		case t.State == ShareFilling:
			if !now.Before(t.Deadline) {
				ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeFillTimeout})
				continue
			}
		}
		keep = append(keep, t)
	}
	l.transfers = keep
	// After the filtering above, which rewrites the slice in place: a donor
	// that lost a pod other than the planned one did shrink.
	for _, t := range wrong {
		l.rebaseDonor(t, now)
	}
	// A fill that landed is inside the receiver's held count from now on: the
	// receiver's other fills, measured from before it landed, count it in.
	for _, f := range filled {
		for _, u := range l.transfers {
			if u.State == ShareFilling && u.Receiver == f.Receiver && u.Receiver != "" && u.fillingSince.Before(now) {
				u.receiverBase += f.GPUs
			}
		}
	}
	// A set whose primary ended while releasing will never raise its receiver:
	// its contributors still releasing end with it, so their donors are given
	// back instead of shrinking for nobody. Not their donors' failure.
	gone := map[string]bool{}
	for _, e := range ended {
		if e.Transfer.IsSetPrimary() && e.Outcome != ShareOutcomeDone {
			gone[e.Transfer.ID] = true
		}
	}
	if len(gone) > 0 {
		l.transfers = slices.DeleteFunc(l.transfers, func(t *ShareTransfer) bool {
			if t.State == ShareReleasing && gone[t.SetID] && !t.IsSetPrimary() {
				ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeAborted})
				return true
			}
			return false
		})
	}
	return ended
}

// rebaseDonor records that t's donor released t's GPUs: the donor's other
// transfers still releasing, measured from before it did, count it out. Without
// this, the next one in line would be measured against a base that still holds
// t's GPUs and release before its own pod went.
func (l *ShareLedger) rebaseDonor(t *ShareTransfer, now time.Time) {
	for _, u := range l.transfers {
		if u != t && u.State == ShareReleasing && u.Donor == t.Donor && u.Started.Before(now) {
			u.donorBase -= t.DonorGPUs
		}
	}
}

// ReceivingHeld reports whether role may not receive now: it gave within the
// reversal hold.
func (l *ShareLedger) ReceivingHeld(role string, now time.Time, tm ShareTimings) bool {
	t, ok := l.lastGave[role]
	return ok && now.Sub(t) < tm.ReversalHold
}

// Redirect hands a Releasing transfer's GPUs to a woken model (section 6.3):
// the release proceeds, nobody is raised at release -- the wake's own pod is
// already waiting for the hole -- and the original receiver, no longer
// committed the GPUs, is planned again. It returns the transfer as it was, and
// false when no transfer of that ID is still Releasing. A claim sets no hold.
//
// A donor set's member is never redirected: its holes fund one receiver
// replica together, and one member's hole is not what a wake needs. The GPUs
// redirected are held for the wake (WakeHeld) until hold after the release
// lands -- not after the redirect: a release takes a whole scale-down window --
// so an idle fill does not raise another receiver into the hole the wake's pod
// waits for. A release that aborts opens no hole, and its hold goes.
func (l *ShareLedger) Redirect(id string, hold time.Duration) (ShareTransfer, bool) {
	for _, t := range l.transfers {
		if t.ID == id && redirectable(t) {
			prev := *t
			t.Receiver, t.ReceiverVariant, t.GPUs, t.Urgent = "", "", 0, false
			l.wakeHolds = append(l.wakeHolds, shareWakeHold{gpus: t.DonorGPUs, id: t.ID, hold: hold})
			return prev, true
		}
	}
	return ShareTransfer{}, false
}

// redirectable is a transfer still releasing, for a receiver, and not part of
// a donor set.
func redirectable(t *ShareTransfer) bool {
	return t.State == ShareReleasing && t.Receiver != "" && t.SetID == ""
}

// WakeHeld is the GPUs held for woken models now.
func (l *ShareLedger) WakeHeld(now time.Time) int {
	l.wakeHolds = slices.DeleteFunc(l.wakeHolds, func(h shareWakeHold) bool {
		if h.until.IsZero() {
			// Still releasing, or the release ended without landing.
			t, ok := l.Transfer(h.id)
			return !ok || t.State != ShareReleasing
		}
		return !now.Before(h.until)
	})
	n := 0
	for _, h := range l.wakeHolds {
		n += h.gpus
	}
	return n
}

// BackingOff reports whether role is backing off after an aborted release.
func (l *ShareLedger) BackingOff(role string, now time.Time) bool {
	return now.Before(l.giveAfter[role])
}

// GivingHeld reports whether role may not give now: it received within the
// reversal hold, or its last release was aborted and it is backing off.
func (l *ShareLedger) GivingHeld(role string, now time.Time, tm ShareTimings) bool {
	if now.Before(l.giveAfter[role]) {
		return true
	}
	t, ok := l.lastGot[role]
	return ok && now.Sub(t) < tm.ReversalHold
}

// recordMove notes a role's transfer direction, holds it from reversing, and
// marks it swinging when its moves changed direction twice within the swing
// window (§6.7 rule 5).
func (l *ShareLedger) recordMove(role string, received bool, now time.Time, tm ShareTimings) {
	if received {
		l.lastGot[role] = now
	} else {
		l.lastGave[role] = now
	}
	l.moves[role] = append(l.moves[role], shareMove{now, received})
	ms := slices.DeleteFunc(l.moves[role], func(m shareMove) bool { return now.Sub(m.at) > tm.SwingWindow })
	l.moves[role] = ms
	flips := 0
	for i := 1; i < len(ms); i++ {
		if ms[i].received != ms[i-1].received {
			flips++
		}
	}
	if flips >= 2 {
		l.swingUntil[role] = now.Add(tm.SwingWindow)
	}
}

// Swinging reports whether role is planned on its mean need now.
func (l *ShareLedger) Swinging(role string, now time.Time) bool {
	return now.Before(l.swingUntil[role])
}

// RecordNeeds stores this cycle's needs, keeping a swing window of history.
func (l *ShareLedger) RecordNeeds(needs map[string]float64, now time.Time, tm ShareTimings) {
	for role, n := range needs {
		l.needs[role] = append(l.needs[role], shareNeedSample{now, n})
		l.needs[role] = slices.DeleteFunc(l.needs[role], func(x shareNeedSample) bool { return now.Sub(x.at) > tm.SwingWindow })
	}
}

// PlanningNeed is the need a role is planned on: its mean over the swing
// window while it is swinging, else its current need.
func (l *ShareLedger) PlanningNeed(role string, current float64, now time.Time) float64 {
	if !l.Swinging(role, now) || len(l.needs[role]) == 0 {
		return current
	}
	sum := 0.0
	for _, s := range l.needs[role] {
		sum += s.need
	}
	return sum / float64(len(l.needs[role]))
}

// Confirm updates each role's run of consecutive actionable cycles and
// returns it. A role absent from actionable is reset.
func (l *ShareLedger) Confirm(actionable map[string]bool) map[string]int {
	for role := range l.confirm {
		if !actionable[role] {
			delete(l.confirm, role)
		}
	}
	for role, a := range actionable {
		if a {
			l.confirm[role]++
		}
	}
	out := make(map[string]int, len(l.confirm))
	for k, v := range l.confirm {
		out[k] = v
	}
	return out
}

// MeasuredRelease is the 90th percentile of recent completed releases, and
// whether at least three have completed: too few say nothing about the tail.
func (l *ShareLedger) MeasuredRelease() (time.Duration, bool) {
	if len(l.releases) < 3 {
		return 0, false
	}
	s := slices.Clone(l.releases)
	slices.Sort(s)
	i := min(len(s)-1, int(float64(len(s))*0.9))
	return s[i], true
}

// PlannedRunning records, for this cycle, which node-planned transfers still
// have a planned donor pod running (IDs in running) and which have one that
// could not be read (IDs in unknown). Observe reads it.
func (l *ShareLedger) PlannedRunning(running, unknown map[string]bool) {
	for _, t := range l.transfers {
		t.plannedRunning, t.plannedUnknown = running[t.ID], unknown[t.ID]
	}
}

// ConfirmStarted records that a transfer took effect: its donor pods are
// marked and the donor's target is lowered.
func (l *ShareLedger) ConfirmStarted(id string, pods []string) {
	for _, t := range l.transfers {
		if t.ID == id {
			t.DonorPods = pods
			t.DonorLowered = true
		}
	}
	delete(l.undo, id)
}

// Forget drops a transfer that never took effect -- its donor pod could not be
// marked -- and puts back the move history Start recorded for it: nothing
// moved, so it must neither hold its roles nor count toward a swing.
func (l *ShareLedger) Forget(id string) {
	l.transfers = slices.DeleteFunc(l.transfers, func(t *ShareTransfer) bool { return t.ID == id })
	u, ok := l.undo[id]
	if !ok {
		return
	}
	delete(l.undo, id)
	restore := func(m map[string]time.Time, k string, v time.Time, had bool) {
		if had {
			m[k] = v
		} else {
			delete(m, k)
		}
	}
	restore(l.lastGave, u.donor, u.gave, u.hadGave)
	restore(l.lastGot, u.receiver, u.got, u.hadGot)
	l.moves[u.donor] = u.donorMoves
	l.moves[u.receiver] = u.receiverMoves
	restore(l.swingUntil, u.donor, u.donorSwing, !u.donorSwing.IsZero())
	restore(l.swingUntil, u.receiver, u.receiverSwing, !u.receiverSwing.IsZero())
}

type shareFillBlock struct {
	reason string
	until  time.Time
}

// FillBlocked records why role's fill timed out, reported until until. An
// empty reason clears it.
func (l *ShareLedger) FillBlocked(role, reason string, until time.Time) {
	if reason == "" {
		delete(l.fillBlocked, role)
		return
	}
	l.fillBlocked[role] = shareFillBlock{reason, until}
}

// FillBlockedReason is the reason recorded for role's last timed-out fill,
// while it is still reported, or "".
func (l *ShareLedger) FillBlockedReason(role string, now time.Time) string {
	b, ok := l.fillBlocked[role]
	if !ok {
		return ""
	}
	if !now.Before(b.until) {
		delete(l.fillBlocked, role) // expired: a role that is gone is not kept
		return ""
	}
	return b.reason
}

// SetFillShort records the receivers the idle fill left below their target
// this cycle, replacing the last record. The planner leaves a receiver whose
// pods fit free GPUs to the fill only when the fill did not leave it short:
// the fill checks what the planner does not (GPUs held for a wake, the
// cluster's physical free GPUs, the nodes other sets just spent), and a
// receiver deferred to a fill that keeps refusing it would never be funded.
func (l *ShareLedger) SetFillShort(roles []string) {
	l.fillShort = map[string]bool{}
	for _, r := range roles {
		l.fillShort[r] = true
	}
}

// FillShort reports whether the last idle fill left role below its target.
func (l *ShareLedger) FillShort(role string) bool { return l.fillShort[role] }

// MarkFailed backs donor off as an abort does: its pods could not be marked
// for a transfer, and planning it again next cycle would fail the same way
// while its receiver never tries another donor.
func (l *ShareLedger) MarkFailed(donor string, now time.Time, tm ShareTimings) {
	if donor == "" {
		return
	}
	l.aborts[donor]++
	l.giveAfter[donor] = now.Add(tm.ReleaseTimeout << min(l.aborts[donor]-1, 4))
	l.unsteerable[donor] = true
}

// Unsteerable reports whether role is backing off because its pods could not
// be marked, rather than because a release aborted.
func (l *ShareLedger) Unsteerable(role string, now time.Time) bool {
	return l.BackingOff(role, now) && l.unsteerable[role]
}

// Retain forgets the per-role history of roles that have been out of the group
// -- and out of every live transfer -- for longer than any window that reads
// it: the reversal hold, the swing window and its mean, the longest abort
// back-off. A role absent for a cycle or two (frozen, not collected) keeps its
// history; a deleted model's is dropped instead of kept for the life of the
// controller.
func (l *ShareLedger) Retain(present []string, now time.Time, tm ShareTimings) {
	for _, r := range present {
		l.seen[r] = now
	}
	for _, t := range l.transfers {
		l.seen[t.Donor] = now
		if t.Receiver != "" {
			l.seen[t.Receiver] = now
		}
	}
	// A role recorded before Retain first ran starts its absence now.
	recorded := []iter.Seq[string]{maps.Keys(l.aborts), maps.Keys(l.giveAfter), maps.Keys(l.fillBlocked),
		maps.Keys(l.lastGave), maps.Keys(l.lastGot), maps.Keys(l.moves), maps.Keys(l.swingUntil), maps.Keys(l.needs)}
	for _, roles := range recorded {
		for r := range roles {
			if _, ok := l.seen[r]; !ok {
				l.seen[r] = now
			}
		}
	}
	horizon := max(tm.ReversalHold, 2*tm.SwingWindow, tm.ReleaseTimeout<<4)
	for r, at := range l.seen {
		if now.Sub(at) <= horizon {
			continue
		}
		delete(l.seen, r)
		delete(l.aborts, r)
		delete(l.unsteerable, r)
		delete(l.giveAfter, r)
		delete(l.fillBlocked, r)
		delete(l.lastGave, r)
		delete(l.lastGot, r)
		delete(l.moves, r)
		delete(l.swingUntil, r)
		delete(l.needs, r)
	}
}

// newIncarnation is a random prefix for a ledger's transfer IDs, so an ID
// never repeats across restarts.
func newIncarnation() string {
	if s := randomHex(8); s != "" {
		return s
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// randomHex is n random bytes in hex, or "" if the system has none. Every
// transfer ID ends in one: an ID a tenant could predict from the one on its own
// pod could be forged onto its pods to collide with another tenant's transfer,
// which a restart then drops as a duplicate.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// GiveAfter is when role may be asked to give again after a back-off; the zero
// time when it is not backing off.
func (l *ShareLedger) GiveAfter(role string) time.Time { return l.giveAfter[role] }
