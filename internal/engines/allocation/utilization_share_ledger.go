package allocation

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
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

// ShareTransferOutcome is how a transfer left the ledger: one of
// constants.UtilizationShareOutcomes, the values its counter is published with.
type ShareTransferOutcome string

const (
	// ShareOutcomeDone counts a transfer whose receiver holds the GPUs, and
	// an idle fill that landed.
	ShareOutcomeDone ShareTransferOutcome = constants.UtilizationShareOutcomeDone
	// ShareOutcomeFillTimeout counts a transfer whose receiver did not take
	// the released GPUs within the fill timeout; it keeps its target.
	ShareOutcomeFillTimeout ShareTransferOutcome = constants.UtilizationShareOutcomeFillTimeout
	// ShareOutcomeCancelled counts a release called off on a clear reversal,
	// while it was still free (section 6.7 rule 3).
	ShareOutcomeCancelled ShareTransferOutcome = constants.UtilizationShareOutcomeCancelled
	// ShareOutcomeAborted counts a release that did not land within the
	// release timeout -- its donor is restored and backs off -- or a member of
	// a donor set that could not complete, which sets no back-off.
	ShareOutcomeAborted ShareTransferOutcome = constants.UtilizationShareOutcomeAborted
	// ShareOutcomeRedirected counts a transfer a wake claimed (section 6.3);
	// it stays in the ledger as a release with no receiver.
	ShareOutcomeRedirected ShareTransferOutcome = constants.UtilizationShareOutcomeRedirected
	// ShareOutcomeWrongPod counts a node-planned transfer whose donor shrank
	// by a different pod than the one marked: the GPUs came free, but not
	// where the plan put the receiver's pod, so the receiver is not raised
	// (section 6.5, "check which pod actually went").
	ShareOutcomeWrongPod ShareTransferOutcome = constants.UtilizationShareOutcomeWrongPod
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
	// Entitled transfers -- owed floors, fixed consumers, reserve refill, a
	// returned spare -- bypass the z admission rule and the holds, and set no
	// hold (§6.2). A Returned transfer is always Entitled.
	Entitled bool
	// Returned marks a transfer that gives a replica above the donor's target
	// back to the group (ShareCapSpare), started while no role was short of
	// its own. It counts against neither the pace nor the concurrency limit.
	// It has no Receiver: it goes to the unassigned GPUs, for the idle fill
	// to raise whoever needs them, and so it is never redirected to a wake.
	Returned bool
	// DonorPodGPUs is the GPUs of each pod of the returned replica, when its
	// shape is known: a return is credited only to a receiver whose pod one
	// of them holds (returnsCover). Empty for any other transfer, and for a
	// return read back from its marks after a restart.
	DonorPodGPUs []int
	// Urgent receivers are below their need (§6.4).
	Urgent bool
	// SkippedWaits marks a move that rebalances a hard imbalance
	// (shareHardImbalance): it waited for neither the reversal hold nor the
	// confirm cycles.
	SkippedWaits bool
	// PlannedPods are the donor pods a node-aware plan chose (namespace/name):
	// the ones to mark, because the holes are planned where they run (§6.5).
	// Empty when any of the donor's pods will do.
	PlannedPods []string
	// HoleNodes is where the donor pods this transfer removes run: their GPUs
	// per node, recorded when they are marked (RecordHoles) or read back with
	// the marks after a restart. The holes open there. Empty when no pod's
	// node is known; its promise then counts against no node in particular
	// (PromisedHoles).
	HoleNodes map[string]int
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
	// setHoles is, on a set's primary, where its released contributors'
	// holes are: their HoleNodes, added as each releases.
	setHoles map[string]int
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
	// DonorReleased is set on an abort that is not the donor's failure when
	// its donor had already released: the pod is gone, and restoring the
	// count starts a replacement.
	DonorReleased bool
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
	// transfer's start, and a cancelled transfer's donor from giving to the
	// same receiver again, measured from the cancellation.
	ReversalHold time.Duration
	// SwingWindow: a role whose transfers change direction SwingReversals
	// times within it is planned on its mean need for the next SwingWindow.
	SwingWindow time.Duration
	// SwingReversals is stabilization.swingReversals: the changes of direction
	// within SwingWindow that make a swing. Zero takes
	// config.DefaultSwingReversals.
	SwingReversals int
	// MoveLatency is how long a move takes from decision to serving: release,
	// model load and the cycles in between. Zero takes SwingWindow/8, what
	// the swing window is derived from when not overridden.
	MoveLatency time.Duration
}

// moveLatency is tm.MoveLatency, or SwingWindow/8 when it is not set.
func (tm ShareTimings) moveLatency() time.Duration {
	if tm.MoveLatency > 0 {
		return tm.MoveLatency
	}
	return tm.SwingWindow / 8
}

// Reversals is the changes of direction a swing takes: tm.SwingReversals, or
// config.DefaultSwingReversals when it is not set.
func (tm ShareTimings) Reversals() int {
	if tm.SwingReversals > 0 {
		return tm.SwingReversals
	}
	return config.DefaultSwingReversals
}

// unpacedHorizon is how much of its burst a receiver must have left for its
// hard imbalance to move unpaced (ShareLedger.Unpaced): a move's latency, and
// a quarter of it again to serve in. Measured in the whole-node simulator, a
// move landing with less of the burst left cost more, at the donor's next
// burst, than it served.
func (tm ShareTimings) unpacedHorizon() time.Duration {
	return tm.moveLatency() * 5 / 4
}

// shareMove is one role's side of a transfer: id names the transfer, so a
// transfer that is cancelled or never took effect removes exactly its own.
type shareMove struct {
	at       time.Time
	received bool
	id       string
	// donorBack marks a give called off because the donor's own demand
	// came back: one turn of its demand (CancelDonorBack).
	donorBack bool
}

// sharePair is a donor and the receiver it gives to.
type sharePair struct{ donor, receiver string }

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
	// busyUntil holds a donor that has given all it can until its earlier
	// releases land, or one release timeout (HoldGiving). Not a back-off: no
	// abort is counted, no blocked reason reads it, and the withheld count
	// does not take it for a reversal hold.
	busyUntil map[string]time.Time
	// changing holds a donor whose workload is changing -- a rollout, a pod
	// not yet created or scheduled or Ready -- until it reads stable for the
	// confirm cycles, or for at most one release timeout (HoldChanging,
	// ObserveChanging). Like busyUntil, not a back-off and not a fault.
	changing map[string]ShareChangingHold
	// filled is the next Observe's per-role filled GPUs (ReceiverFilled).
	filled map[string]int
	// fillHeld holds a receiver whose last fill timed out from being funded
	// again, until then (HoldFill).
	fillHeld map[string]time.Time
	// fillBlocked is, per receiver, why its last fill timed out
	// (release-taken or release-shape-mismatch) and until when it is reported.
	fillBlocked map[string]shareFillBlock
	// fillShort are the receivers the last idle fill left below their target:
	// the planner does not leave them to the fill again (SetFillShort).
	fillShort map[string]bool

	// lastGave and lastGot are derived from moves (rederive).
	lastGave, lastGot map[string]time.Time
	// noPair holds, until then, a donor from giving to the receiver of a
	// transfer between them that was cancelled, so a start and a cancel
	// cannot loop. Only that pair: the receiver may still take from other
	// donors and the donor give to others -- a hold on either role alone
	// would let one model, by timing its own load, keep another from
	// receiving at all.
	noPair map[sharePair]time.Time
	moves  map[string][]shareMove
	// cancels are the moves each role's cancelled transfers would have made,
	// one per cycle and direction: each counts toward a swing as the move it
	// was (rederive), and holds nothing.
	cancels    map[string][]shareMove
	swingUntil map[string]time.Time
	// stepped marks the roles whose need has stayed on one side of its mean
	// for longer than a move takes (RecordNeeds): a step, not a swing.
	stepped map[string]bool
	needs   map[string][]shareNeedSample
	// needsSince is when each role's need was first recorded: until a swing
	// window has passed since, its peak over the window is not known
	// (PeakKnown).
	needsSince map[string]time.Time
	confirm    map[string]int
	// hard counts each role's consecutive cycles as a hard-imbalance
	// receiver (ConfirmHard).
	hard map[string]int
	// over counts each role's consecutive cycles holding more than its
	// target (MarkOver).
	over map[string]int
	// podBusy is, per role whose busiest pod reads busy, how long the run
	// of busy readings has lasted (NotePodBusy).
	podBusy map[string]SharePodBusy
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

// shareWakeHold is a redirected transfer's GPUs, held for the woken model:
// for as long as the release takes (id names the transfer, until is zero), then
// for hold more, while the wake's pod lands.
type shareWakeHold struct {
	gpus int
	// holes is where the redirected transfer's holes are (HoleNodes).
	holes map[string]int
	id    string
	hold  time.Duration
	until time.Time
}

// NewShareLedger returns an empty ledger.
func NewShareLedger() *ShareLedger {
	return &ShareLedger{
		incarnation: newIncarnation(),
		aborts:      map[string]int{},
		giveAfter:   map[string]time.Time{},
		fillBlocked: map[string]shareFillBlock{},
		lastGave:    map[string]time.Time{},
		lastGot:     map[string]time.Time{},
		noPair:      map[sharePair]time.Time{},
		moves:       map[string][]shareMove{},
		cancels:     map[string][]shareMove{},
		swingUntil:  map[string]time.Time{},
		stepped:     map[string]bool{},
		needs:       map[string][]shareNeedSample{},
		needsSince:  map[string]time.Time{},
		confirm:     map[string]int{},
		hard:        map[string]int{},
		over:        map[string]int{},
		podBusy:     map[string]SharePodBusy{},
		seen:        map[string]time.Time{},
		unsteerable: map[string]bool{},
		busyUntil:   map[string]time.Time{},
		changing:    map[string]ShareChangingHold{},
		fillHeld:    map[string]time.Time{},
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
// with a donor, a donor set counting once -- it funds one receiver replica,
// and counting its members would make an LWS receiver of three or more
// whole-node pods unfundable. A fill from idle GPUs moves nothing and does
// not count.
func (l *ShareLedger) InFlight() int {
	seen := map[string]bool{}
	for _, t := range l.transfers {
		// A returned spare does not count: counted, a calm model's returns
		// would hold every ordinary move for a release. A move that skipped
		// the waits does: the unpaced moves of a hard imbalance pass the
		// limit without consulting it, but an ordinary move started beside
		// them would drain the same donor for headroom, and land after the
		// burst it was not needed for.
		if t.Donor == "" || t.Returned {
			continue
		}
		k := t.ID
		if t.SetID != "" {
			k = t.SetID
		}
		seen[k] = true
	}
	return len(seen)
}

// Unreleased is the GPUs releasing transfers took off their donors'
// commitment and gave no receiver: all of a reserve refill's or a return's,
// and whatever a transfer or a set takes beyond its receiver's replica (an
// 8-GPU replica given to a 4-GPU receiver). The donors' pods still run on them
// and the quota still counts them, so they are not idle until released.
// Counted idle, the fill would raise a model into a full quota, its pods would
// be refused at admission, and it would be raised again at every fill timeout.
func (l *ShareLedger) Unreleased() int {
	excess := map[string]int{}
	for _, t := range l.transfers {
		if t.State != ShareReleasing {
			continue
		}
		k := t.ID
		if t.SetID != "" {
			k = t.SetID
		}
		excess[k] += t.DonorGPUs
		if t.Receiver != "" {
			excess[k] -= t.GPUs
		}
	}
	n := 0
	for _, v := range excess {
		n += max(v, 0)
	}
	return n
}

// StartFill records a receiver raised into idle GPUs: no donor, so it starts in
// Filling and ends when the receiver's pods are scheduled. Tracking it keeps
// the next cycle, which does not yet see those pods held, from filling the same
// GPUs again. urgent marks a receiver raised below its need, as Urgent does a
// transfer's.
func (l *ShareLedger) StartFill(receiver, variant string, gpus int, urgent bool, held map[string]int, now time.Time,
	tm ShareTimings) ShareTransfer {
	l.nextID++
	t := &ShareTransfer{
		ID: fmt.Sprintf("%s-f%d-%s", l.incarnation, l.nextID, randomHex(4)), Receiver: receiver, ReceiverVariant: variant,
		GPUs: gpus, Entitled: true, Urgent: urgent, State: ShareFilling, Started: now,
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

// ShareHoles is the GPUs promised to receivers or held for wakes and not yet
// taken (Promised, WakeHeld), by where their holes are: Nodes on the node a
// hole is known to be on, Unplaced where it is not known.
type ShareHoles struct {
	Nodes    map[string]int
	Unplaced int
}

// Total is every GPU h holds back, placed or not.
func (h ShareHoles) Total() int {
	n := h.Unplaced
	for _, g := range h.Nodes {
		n += g
	}
	return n
}

// add places gpus on holes, the nodes in name order, each up to what it
// opened; what no known hole covers is unplaced.
func (h *ShareHoles) add(gpus int, holes ...map[string]int) {
	if gpus <= 0 {
		return
	}
	at := map[string]int{}
	for _, m := range holes {
		for n, g := range m {
			at[n] += g
		}
	}
	for _, n := range slices.Sorted(maps.Keys(at)) {
		take := min(gpus, at[n])
		if take <= 0 {
			continue
		}
		if h.Nodes == nil {
			h.Nodes = map[string]int{}
		}
		h.Nodes[n] += take
		gpus -= take
	}
	h.Unplaced += gpus
}

// PromisedHoles is Promised and WakeHeld together, each GPU on the node its
// hole is on when the ledger knows it (HoleNodes): a receiver's pod, or a
// wake's, waits for that hole. Its Total is Promised() + WakeHeld(now).
func (l *ShareLedger) PromisedHoles(now time.Time) ShareHoles {
	var h ShareHoles
	for _, t := range l.transfers {
		switch t.State {
		case ShareFilling:
			h.add(t.GPUs, t.HoleNodes, t.setHoles)
		case ShareReleasing:
			h.add(t.setReleased, t.setHoles)
		}
	}
	l.WakeHeld(now) // drops the expired holds
	for _, w := range l.wakeHolds {
		h.add(w.gpus, w.holes)
	}
	return h
}

// RecordHoles records where transfer id's holes open: the GPUs of the donor
// pods it marked, per node (HoleNodes).
func (l *ShareLedger) RecordHoles(id string, holes map[string]int) {
	for _, t := range l.transfers {
		if t.ID == id {
			t.HoleNodes = maps.Clone(holes)
		}
	}
	for i := range l.wakeHolds {
		if l.wakeHolds[i].id == id {
			l.wakeHolds[i].holes = maps.Clone(holes)
		}
	}
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
		l.recordMove(t.Donor, false, t.ID, now, tm)
		if t.Receiver != "" {
			l.recordMove(t.Receiver, true, t.ID, now, tm)
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
// no donor pod has moved, and holds its donor from giving to its receiver
// again (§6.3, §6.7 rule 3). It reports whether the transfer was cancelled.
func (l *ShareLedger) Cancel(id string, now time.Time, tm ShareTimings) bool {
	return l.cancel(id, false, now, tm)
}

// CancelDonorBack is Cancel for a transfer called off because its donor's own
// demand came back: the donor's demand went down, so it gave, and up again,
// so it wants the replica back: one turn of its demand. The turn is counted
// as a change of direction -- the give it would have made, then the receive
// its demand turned to -- only when it is the donor's second inside the swing
// window: an earlier turn was called off before this transfer started, so the
// donor gave again after its demand came back once and wanted the replica
// back again. That is a donor chasing a load that swaps faster than a release.
// Counted as the give alone, such a donor never changed direction, never
// reached a swing, and started a transfer at every swap. Counted as a change
// of direction at every turn, a single wobble -- a busy model that read no
// demand for a cycle or two -- was one more change of direction, and a single
// swap back after it was held on the mean: measured over 74 wobbles, twice as
// many held, and 22 swap backs slower than with the rule off. Counted at the
// second turn also when the first was a landed move against the one before it,
// a model that had given its spare and been raised again was held on its mean
// by a single wobble: 28 of the 74 held, 22 slower.
//
// Every transfer the turn calls off counts once: a transfer started before
// the donor's demand came back in a cancel already counted (a second replica
// started the cycle after the first, called off a cycle later) is the same
// turn, and records nothing more for the donor. Recorded as a give again, it
// reversed the receive just counted. A spare return (Entitled) still counts
// for nothing, and the receiver still counts the receive it would have made.
func (l *ShareLedger) CancelDonorBack(id string, now time.Time, tm ShareTimings) bool {
	return l.cancel(id, true, now, tm)
}

func (l *ShareLedger) cancel(id string, donorBack bool, now time.Time, tm ShareTimings) bool {
	i := slices.IndexFunc(l.transfers, func(t *ShareTransfer) bool { return t.ID == id })
	if i < 0 {
		return false
	}
	t := l.transfers[i]
	if t.State != ShareReleasing || now.Sub(t.Started) >= tm.Window {
		return false
	}
	// Nothing moved: the move history Start recorded is put back, as for a
	// transfer that never took effect. Recorded, it held the donor from
	// receiving and the receiver from giving -- exactly the reverse move the
	// demand that cancelled it calls for -- and counted toward a swing as a
	// move, which plans a role on its mean need and under-reads a rising one.
	// Measured: a model whose burst began as it was about to give waited 16
	// minutes. Only the cancelled pair is held, so start and cancel cannot
	// loop.
	//
	// The cancel still counts toward a swing as the move it would have been
	// (recordCancel): a load that swaps faster than the release window has
	// every transfer cancelled before it moves anything, and counting only
	// the moves that land, it never reached a swing -- a transfer started and
	// was called off at every swap. It counts in its direction, so it is a
	// change of direction only when it reverses the role's move before it:
	// counted as one whatever its direction, a single wobble called off made
	// a single swap back a swing. And only a transfer that recorded a move
	// counts: a spare return (Entitled) has no counterpart, and counted, a
	// bursty model whose returns were called off as its load came back was
	// planned on its mean without having traded a GPU with anyone.
	rc := t.Receiver
	if rc == "" {
		rc = l.setReceiver(t.SetID)
	}
	l.Forget(t.ID, tm)
	if !t.Entitled {
		switch {
		case !donorBack:
			l.recordCancel(t.Donor, false, now, tm)
		case !l.turnedSince(t.Donor, t.Started):
			again := l.turnedBefore(t.Donor, t.Started, now, tm)
			l.recordCancel(t.Donor, false, now, tm)
			l.markDonorBack(t.Donor, now)
			if again {
				l.recordCancel(t.Donor, true, now, tm)
			}
		}
		if rc != "" {
			l.recordCancel(rc, true, now, tm)
		}
	}
	if !t.Entitled && rc != "" {
		l.noPair[sharePair{t.Donor, rc}] = now.Add(tm.ReversalHold)
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
	// A receiver is judged by its filled replicas when the caller gave them
	// (ReceiverFilled): an LWS group whose leader alone is bound serves
	// nothing, and ending its fill would free its other holes for anyone.
	rheld := held
	if l.filled != nil {
		rheld = l.filled
		l.filled = nil
	}
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
				if p.ID != t.SetID {
					continue
				}
				if p.State == ShareReleasing {
					p.setReleased += t.DonorGPUs
					p.setPending--
				}
				// Also when the primary released earlier in this pass: its
				// receiver's replica fills this hole too.
				for n, g := range t.HoleNodes {
					if p.setHoles == nil {
						p.setHoles = map[string]int{}
					}
					p.setHoles[n] += g
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
		_, seen := rheld[t.Receiver]
		t.receiverBase = rheld[t.Receiver]
		t.receiverBaseUnset = t.Receiver != "" && !seen
		t.fillingSince = now
		l.released = append(l.released, *t)
		l.rebaseDonor(t, now)
		delete(l.aborts, t.Donor)
		delete(l.giveAfter, t.Donor)
		delete(l.busyUntil, t.Donor)
	}
	// A transfer released this cycle measures its receiver from now; one that
	// was already filling keeps its base. Filling -> Done, per receiver in start
	// order.
	got := map[string]int{}
	rbase := map[string]int{}
	keep := l.transfers[:0]
	abortedNow := map[string]bool{}
	var filled, wrong []*ShareTransfer
	for _, t := range l.transfers {
		_, receiverSeen := rheld[t.Receiver]
		switch {
		case t.State == ShareReleasing && broken(t):
			// Not the donor's failure: no back-off.
			// Released by count, not by the planned pod: that pod still runs.
			ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeAborted,
				DonorReleased: releasable[t] && !t.plannedRunning})
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
			ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeAborted, DonorReleased: true})
			continue
		case t.State == ShareReleasing && !now.Before(t.Deadline):
			ended = append(ended, ShareTransferEnd{Transfer: *t, Outcome: ShareOutcomeAborted})
			if t.Donor != "" && !abortedNow[t.Donor] {
				// Back off one release timeout, doubling per consecutive
				// abort up to 16x; a release that lands resets it. One
				// cycle's aborts of a donor count once: transfers started
				// together share a deadline, and the five a hard imbalance
				// starts at once would otherwise reach the 16x back-off on
				// their first stuck release.
				abortedNow[t.Donor] = true
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
				t.receiverBase, t.receiverBaseUnset, t.fillingSince = rheld[t.Receiver], false, now
				break
			}
			if _, ok := rbase[t.Receiver]; !ok {
				rbase[t.Receiver] = t.receiverBase
			}
			got[t.Receiver] += t.GPUs
			if rheld[t.Receiver] >= rbase[t.Receiver]+got[t.Receiver] {
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

// PairHeld reports whether donor may not give to receiver now: a transfer
// between them was cancelled within the reversal hold, and starting it again
// would repeat it.
func (l *ShareLedger) PairHeld(donor, receiver string, now time.Time) bool {
	return now.Before(l.noPair[sharePair{donor, receiver}])
}

// setReceiver is the receiver of the set whose primary is id, while the
// primary is in the ledger.
func (l *ShareLedger) setReceiver(id string) string {
	for _, t := range l.transfers {
		if t.ID == id {
			return t.Receiver
		}
	}
	return ""
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
			l.wakeHolds = append(l.wakeHolds, shareWakeHold{gpus: t.DonorGPUs, holes: maps.Clone(t.HoleNodes), id: t.ID, hold: hold})
			return prev, true
		}
	}
	return ShareTransfer{}, false
}

// redirectable is a transfer still releasing, for a receiver, not part of a
// donor set, and not a return.
func redirectable(t *ShareTransfer) bool {
	return t.State == ShareReleasing && t.Receiver != "" && t.SetID == "" && !t.Returned
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
// reversal hold, or GivingBlocked.
func (l *ShareLedger) GivingHeld(role string, now time.Time, tm ShareTimings) bool {
	if l.GivingBlocked(role, now) {
		return true
	}
	t, ok := l.lastGot[role]
	return ok && now.Sub(t) < tm.ReversalHold
}

// GivingBlocked reports whether role may not give now for a reason no
// shortfall lifts: its last release was aborted and it is backing off, or it
// has given all it can (GivingBusy).
func (l *ShareLedger) GivingBlocked(role string, now time.Time) bool {
	return now.Before(l.giveAfter[role]) || l.GivingBusy(role, now)
}

// recordMove notes a role's transfer direction, holds it from reversing, and
// marks it swinging when its moves changed direction SwingReversals times
// within the swing window (§6.7 rule 5).
func (l *ShareLedger) recordMove(role string, received bool, id string, now time.Time, tm ShareTimings) {
	// Kept for as long as anything derived from them can still matter: the
	// reversal hold, and a swing (SwingWindow) begun by moves up to a
	// SwingWindow before it.
	keep := max(tm.ReversalHold, 2*tm.SwingWindow)
	l.moves[role] = append(slices.DeleteFunc(l.moves[role], func(m shareMove) bool { return now.Sub(m.at) > keep }),
		shareMove{at: now, received: received, id: id})
	l.rederive(role, tm)
}

// recordCancel notes that a transfer of role, which would have had it receive
// or give, was cancelled on a clear reversal: an attempted move toward a
// swing, once per cycle and direction however many transfers went (a set's
// contributors go with its primary).
func (l *ShareLedger) recordCancel(role string, received bool, now time.Time, tm ShareTimings) {
	cs := slices.DeleteFunc(l.cancels[role], func(m shareMove) bool { return now.Sub(m.at) > 2*tm.SwingWindow })
	if slices.ContainsFunc(cs, func(m shareMove) bool { return m.at.Equal(now) && m.received == received }) {
		l.cancels[role] = cs
		return
	}
	l.cancels[role] = append(cs, shareMove{at: now, received: received})
	l.rederive(role, tm)
}

// markDonorBack marks role's give cancelled this cycle as one turn of its
// demand (CancelDonorBack).
func (l *ShareLedger) markDonorBack(role string, now time.Time) {
	for i, m := range l.cancels[role] {
		if m.at.Equal(now) && !m.received {
			l.cancels[role][i].donorBack = true
		}
	}
}

// turnedSince reports whether role's demand turned back after started: a
// give of role was called off for it (CancelDonorBack) since then.
func (l *ShareLedger) turnedSince(role string, started time.Time) bool {
	return slices.ContainsFunc(l.cancels[role], func(m shareMove) bool { return m.donorBack && m.at.After(started) })
}

// turnedBefore reports whether role's demand turned back inside the swing
// window before now, and no later than started: a give of role was called off
// for it (CancelDonorBack) before the transfer that started then.
func (l *ShareLedger) turnedBefore(role string, started, now time.Time, tm ShareTimings) bool {
	return slices.ContainsFunc(l.cancels[role], func(m shareMove) bool {
		return m.donorBack && now.Sub(m.at) <= tm.SwingWindow && !m.at.After(started)
	})
}

// unrecord removes transfer id's moves from role's history, as if it never
// started.
func (l *ShareLedger) unrecord(role, id string, tm ShareTimings) {
	if _, ok := l.moves[role]; !ok {
		return
	}
	l.moves[role] = slices.DeleteFunc(l.moves[role], func(m shareMove) bool { return m.id == id })
	l.rederive(role, tm)
}

// rederive recomputes role's last move each way and its swing from its moves:
// a swing begins at a move or cancel where the changes of direction within
// SwingWindow before it -- between consecutive moves, landed or cancelled
// (recordCancel), in time order -- number SwingReversals
// (config.DefaultSwingReversals unless the policy sets
// stabilization.swingReversals), and lasts SwingWindow. The last move each way
// is of landed moves only: a cancel holds nothing.
func (l *ShareLedger) rederive(role string, tm ShareTimings) {
	delete(l.lastGave, role)
	delete(l.lastGot, role)
	delete(l.swingUntil, role)
	for _, m := range l.moves[role] {
		if m.received {
			l.lastGot[role] = m.at
		} else {
			l.lastGave[role] = m.at
		}
	}
	ms := slices.SortedStableFunc(slices.Values(slices.Concat(l.moves[role], l.cancels[role])),
		func(a, b shareMove) int { return a.at.Compare(b.at) })
	reversals := func(at time.Time) int {
		n := 0
		for i := 1; i < len(ms) && !ms[i].at.After(at); i++ {
			if at.Sub(ms[i-1].at) <= tm.SwingWindow && ms[i].received != ms[i-1].received {
				n++
			}
		}
		return n
	}
	var until time.Time
	need := tm.Reversals()
	mark := func(at time.Time) {
		if reversals(at) >= need && at.Add(tm.SwingWindow).After(until) {
			until = at.Add(tm.SwingWindow)
		}
	}
	for _, m := range ms {
		mark(m.at)
	}
	if !until.IsZero() {
		l.swingUntil[role] = until
	}
}

// Swinging reports whether role is planned on its mean need now: its moves,
// landed or cancelled, changed direction SwingReversals times within the swing
// window, and its
// need has not since settled away from its mean for longer than a move takes
// (stepped).
func (l *ShareLedger) Swinging(role string, now time.Time) bool {
	return now.Before(l.swingUntil[role]) && !l.stepped[role]
}

// ShareStepSpread is how far from its mean, as a fraction of it, a swinging
// role's need must stay for a move's latency before it counts as a step and
// the role is planned on its current need again (ShareLedger.Swinging).
const ShareStepSpread = 0.25

// steppedAway reports whether role's need has stayed more than
// ShareStepSpread above its mean over the swing window, or as far below it,
// for at least d up to its latest reading, and for longer than any earlier
// run on either side within the window. A swing crosses its mean every half
// period; a step stays on one side until the mean catches up. Planned on the
// mean after a step, a model whose load moved for good was held halfway for
// most of a swing window: two models swapping 3:1 for 1:3 sat at 2:2 for 85
// minutes, the busy one past its threshold. A run no longer than the swing's
// own earlier ones is the swing, not a step: a load swapping every 16 to 45
// minutes -- longer than a move, shorter than its window -- counted as a
// step at the end of every half period, and chasing it started three times
// the transfers and left more demand unserved than planning on its mean.
func (l *ShareLedger) steppedAway(role string, d time.Duration) bool {
	s := l.needs[role]
	if len(s) < 2 || d <= 0 {
		return false
	}
	mean := l.MeanNeed(role, 0)
	side := func(n float64) int {
		switch {
		case n > mean*(1+ShareStepSpread):
			return 1
		case n < mean*(1-ShareStepSpread):
			return -1
		}
		return 0
	}
	// Runs of readings on one side of the mean, the last one under way. The
	// first may have begun before the window: its length is a lower bound,
	// and still counts toward the longest.
	var earlier, cur time.Duration
	run, start := side(s[0].need), s[0].at
	for i := 1; i <= len(s); i++ {
		if i < len(s) && side(s[i].need) == run {
			continue
		}
		if run != 0 {
			if i == len(s) {
				cur = s[i-1].at.Sub(start)
			} else {
				earlier = max(earlier, s[i-1].at.Sub(start))
			}
		}
		if i < len(s) {
			run, start = side(s[i].need), s[i].at
		}
	}
	return cur >= d && cur > earlier
}

// RecordNeeds stores this cycle's needs, keeping a swing window of history.
// The planner records each need as what it serves (ShareRole.serves), so the
// history, and every peak, mean and burst read from it, is in GPUs of the
// role's best variant, whatever mix of variants the role held at the time.
func (l *ShareLedger) RecordNeeds(needs map[string]float64, now time.Time, tm ShareTimings) {
	for role, n := range needs {
		l.needs[role] = slices.DeleteFunc(l.needs[role], func(x shareNeedSample) bool { return now.Sub(x.at) > tm.SwingWindow })
		// A role whose history has all aged out -- new, or back after a gap
		// longer than the window -- starts its history again: kept, its
		// first-seen time would call a peak known from one sample. So does a
		// role with samples but no start: a history restored across a gap
		// (RestoreNeedHistory) keeps its peaks without one.
		if _, ok := l.needsSince[role]; !ok || len(l.needs[role]) == 0 {
			l.needsSince[role] = now
		}
		l.needs[role] = append(l.needs[role], shareNeedSample{now, n})
		l.stepped[role] = l.steppedAway(role, tm.moveLatency())
	}
}

// PeakNeed is the highest need two readings in a row reached for role over
// the swing window; current while fewer than two are recorded.
func (l *ShareLedger) PeakNeed(role string, current float64) float64 {
	s := l.needs[role]
	if len(s) < 2 {
		return current
	}
	// A peak counts only once two readings in a row reach it: one reading
	// -- a scrape glitch, or a tenant's forged metric -- would otherwise cap
	// every other model's headroom for a whole window.
	peak := 0.0
	for i := 1; i < len(s); i++ {
		peak = math.Max(peak, math.Min(s[i-1].need, s[i].need))
	}
	return peak
}

// RecentPeak is role's highest need over the last d that two readings in a
// row reached, as PeakNeed counts a peak: one reading -- a glitch, or one
// forged by a tenant once a window -- would keep a donor's GPUs from a short
// neighbour for the whole window.
func (l *ShareLedger) RecentPeak(role string, now time.Time, d time.Duration) float64 {
	s := l.needs[role]
	peak := 0.0
	for i := 1; i < len(s); i++ {
		if now.Sub(s[i].at) <= d {
			peak = math.Max(peak, math.Min(s[i-1].need, s[i].need))
		}
	}
	return peak
}

// LongBursts reports whether role's longest burst -- a run of need above the
// midpoint of its recorded range, completed or under way (needBursts,
// sofar) -- lasted at least d.
func (l *ShareLedger) LongBursts(role string, d time.Duration) bool {
	return l.needBursts(role).sofar() >= max(d, 1)
}

// BurstsLong reports whether role bursts (LongBursts) over a range of at
// least ShareLongBurstSpread of its highest need: a burst, not a steady need
// drifting across the middle of a narrow range.
func (l *ShareLedger) BurstsLong(role string, d time.Duration) bool {
	b := l.needBursts(role)
	return b.sofar() >= max(d, 1) && b.spread >= ShareLongBurstSpread
}

// SwingsFast reports whether role's need swings faster than d: at least two
// bursts completed, none -- the one under way included -- lasting d, over a
// range of at least ShareFastSwingSpread of its highest need.
func (l *ShareLedger) SwingsFast(role string, d time.Duration) bool {
	b := l.needBursts(role)
	return b.completed >= 2 && b.sofar() < d && b.spread >= ShareFastSwingSpread
}

// BurstEndsWithin reports whether role's burst is expected to end within d:
// what is left of its last completed burst, after as long as the one under
// way has lasted (none: all of it), is shorter than d. The last, not the
// longest: one long burst would otherwise make every short one after it look
// long for a whole window. There is no expected end -- the role's moves are
// not held at the pace -- for a role with no completed burst on record, for
// one whose need does not swing across ShareFastSwingSpread of its peak (a
// steady need with noise crosses its own midpoint every few readings), and
// for one whose burst under way has already outlasted the last: the history
// does not predict it.
func (l *ShareLedger) BurstEndsWithin(role string, d time.Duration) bool {
	b := l.needBursts(role)
	if b.completed == 0 || b.spread < ShareFastSwingSpread || b.ongoing >= b.last {
		return false
	}
	return b.last-b.ongoing < d
}

// ModelBurstEnds reports whether the burst of role's model is expected to end
// within an unpaced move's horizon (unpacedHorizon; BurstEndsWithin). The
// model is role and the roles in model (role included or not; nil judges role
// alone), and they are judged as one: every role whose need swings across
// ShareFastSwingSpread of its peak and is in a burst must be expected to end
// it, and while none is in one, every swinging role's last burst must be. A
// role whose need does not swing is not judged -- a steady need with noise
// crosses its midpoint every few readings, and counted as bursting with no
// end it would unpace its partner's every short burst. The answer is the
// same for each swinging role of a model: the roles of one model -- prefill
// and decode -- burst together but not for equally long; judged apart, one
// could take its whole shortfall at once while its partner moves at the
// pace, and the model would run the burst at a skewed ratio. A role whose own
// need does not swing has no burst to end: its shortfall lasts, and it is
// never held at the pace for its partner's.
func (l *ShareLedger) ModelBurstEnds(role string, model []string, tm ShareTimings) bool {
	if l.needBursts(role).spread < ShareFastSwingSpread {
		return false
	}
	h := tm.unpacedHorizon()
	roles := append([]string{role}, model...)
	var swinging, bursting []string
	for _, r := range roles {
		if slices.Contains(swinging, r) {
			continue
		}
		b := l.needBursts(r)
		if b.spread < ShareFastSwingSpread {
			continue
		}
		swinging = append(swinging, r)
		if b.ongoing > 0 {
			bursting = append(bursting, r)
		}
	}
	judged := bursting
	if len(judged) == 0 {
		judged = swinging
	}
	if len(judged) == 0 {
		return false
	}
	for _, r := range judged {
		if !l.BurstEndsWithin(r, h) {
			return false
		}
	}
	return true
}

// shareBursts summarises a role's need history (ShareLedger.needBursts).
type shareBursts struct {
	// longest is the longest burst that has ended: a completed one, or one
	// already under way when the history starts, whose length is a lower
	// bound. A run still going is in ongoing, and sofar reads the two. last
	// is the most recent completed burst; completed counts them.
	longest, last time.Duration
	completed     int
	// ongoing is how long the burst under way has lasted, zero when none is.
	ongoing time.Duration
	// spread is the history's range as a fraction of its highest need (of
	// one GPU, at least).
	spread float64
}

// sofar is the longest burst including the one under way. A model in its
// first long burst -- already longer than a move, still running -- is a long
// burster now, not once it ends: read on completed bursts alone, it did not
// cap a fast swinger beside it on its mean, and its own short bursts before
// it kept it reading as a fast swinger for as long as the long one lasted.
func (b shareBursts) sofar() time.Duration {
	return max(b.longest, b.ongoing)
}

// needBursts reads role's need history as bursts: runs of need above the
// midpoint of its recorded range. Like PeakNeed it trusts no single reading:
// the range runs from the lowest level two readings in a row stayed at or
// below to the highest two reached, and a run starts and ends only on two
// readings in a row on its new side. One reading -- a scrape glitch, or a
// tenant's forged metric -- would otherwise move the midpoint above every
// real burst, or end a burst and log a short one in its place. A run still
// going is recorded as ongoing, not in longest; the predicates that ask how
// long a role bursts read both (sofar). One already under way when the history
// starts is not a completed burst -- its start is not known, and taken as
// the history's it would be counted short, predicting an early end and
// making a long burster look like a fast swinger -- but its length is a
// lower bound, so it still counts toward the longest: a burster whose one
// long burst straddles the window's start is still a long burster.
func (l *ShareLedger) needBursts(role string) shareBursts {
	return l.needBurstsJoined(role, 0)
}

// needBurstsJoined is needBursts with two runs counted as one burst when the
// dip between them -- from the first reading of one at or below the midpoint
// to the first of the next above it -- is shorter than join. A dip of a
// minute or two inside one busy stretch, from noise in the need, would
// otherwise count the stretch as two bursts.
func (l *ShareLedger) needBurstsJoined(role string, join time.Duration) shareBursts {
	s := l.needs[role]
	if len(s) < 3 {
		return shareBursts{}
	}
	hi, lo := math.Inf(-1), math.Inf(1)
	for i := 1; i < len(s); i++ {
		hi = math.Max(hi, math.Min(s[i-1].need, s[i].need))
		lo = math.Min(lo, math.Max(s[i-1].need, s[i].need))
	}
	if hi <= lo {
		// No level held for two readings above another: no bursts.
		return shareBursts{}
	}
	mid := (hi + lo) / 2
	out := shareBursts{spread: (hi - lo) / math.Max(hi, 1)}
	// A run that ended is counted once the next one starts after a dip of
	// at least join, or once the history ends without one: until then the
	// next run may continue it.
	var start, end time.Time
	in, cut, ended := false, false, false
	count := func() {
		d := end.Sub(start)
		out.longest = max(out.longest, d)
		if !cut {
			out.completed++
			out.last = d
		}
	}
	for i := 1; i < len(s); i++ {
		a, b := s[i-1].need, s[i].need
		switch {
		case !in && a > mid && b > mid:
			in = true
			if ended && s[i-1].at.Sub(end) < join {
				ended = false
				continue
			}
			if ended {
				count()
				ended = false
			}
			cut, start = i == 1, s[i-1].at
		case in && a <= mid && b <= mid:
			in, ended, end = false, true, s[i-1].at
		}
	}
	if ended {
		count()
	}
	if in {
		out.ongoing = s[len(s)-1].at.Sub(start)
	}
	return out
}

// AnyPeakKnown reports whether any role's peak is known (PeakKnown): the
// ledger has a window of history, so a role whose peak is not known joined
// since, rather than every role being unknown after a restart.
func (l *ShareLedger) AnyPeakKnown(now time.Time, tm ShareTimings) bool {
	for role := range l.needsSince {
		if l.PeakKnown(role, now, tm) {
			return true
		}
	}
	return false
}

// PeakKnown reports whether role's need has been recorded for a whole swing
// window, so PeakNeed is its peak over the window. A role first seen less
// than a window ago -- every role after a restart, a new model -- may yet
// peak at anything up to its ceiling.
func (l *ShareLedger) PeakKnown(role string, now time.Time, tm ShareTimings) bool {
	since, ok := l.needsSince[role]
	return ok && now.Sub(since) >= tm.SwingWindow
}

// Giving reports whether role is the donor of a transfer still releasing --
// to a receiver, back to the quota, or to the reserve. Such a role is not
// raised: raised while its pod drains, it would never shrink, and the
// release would abort and restore its count one above where it was meant to
// be.
func (l *ShareLedger) Giving(role string) bool {
	for _, t := range l.transfers {
		if t.State == ShareReleasing && t.Donor == role {
			return true
		}
	}
	return false
}

// PlanningNeed is the need a role is planned on: its mean over the swing
// window while it is swinging, else its current need.
func (l *ShareLedger) PlanningNeed(role string, current float64, now time.Time) float64 {
	if !l.Swinging(role, now) {
		return current
	}
	return l.MeanNeed(role, current)
}

// MeanNeed is role's mean need over the swing window, or current with no
// history.
func (l *ShareLedger) MeanNeed(role string, current float64) float64 {
	if len(l.needs[role]) == 0 {
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

// ConfirmHard records which roles are hard-imbalance receivers this cycle
// and returns, per role, how many consecutive cycles it has been one. A
// role that stops being one starts again from zero.
func (l *ShareLedger) ConfirmHard(hard map[string]bool) map[string]int {
	for role := range l.hard {
		if !hard[role] {
			delete(l.hard, role)
		}
	}
	for role, h := range hard {
		if h {
			l.hard[role]++
		}
	}
	return maps.Clone(l.hard)
}

// HardCycles is how many consecutive cycles role has been a hard-imbalance
// receiver (ConfirmHard).
func (l *ShareLedger) HardCycles(role string) int { return l.hard[role] }

// Unpaced reports whether role's hard-imbalance moves skip the pace and the
// concurrency limit: its hard imbalance has held ShareUnpacedAfterCycles
// consecutive cycles, and its model's burst is not expected to end within
// unpacedHorizon (ModelBurstEnds; model lists the roles of role's model,
// ShareModelRoles). Moves taken all at once for a burst that ends first land
// after it, or serve it for minutes: they drain their donors, often just
// before the donors' own bursts, so such a role moves at the pace.
func (l *ShareLedger) Unpaced(role string, model []string, tm ShareTimings) bool {
	return l.hard[role] >= ShareUnpacedAfterCycles && !l.ModelBurstEnds(role, model, tm)
}

// MarkOver records which roles hold more than their target this cycle and
// returns, per role still over, for how many consecutive cycles it has been:
// what a return waits on (returnSpare). A role that is not over starts again.
func (l *ShareLedger) MarkOver(over map[string]bool) map[string]int {
	for role := range l.over {
		if !over[role] {
			delete(l.over, role)
		}
	}
	for role, o := range over {
		if o {
			l.over[role]++
		}
	}
	return maps.Clone(l.over)
}

// SharePodBusy is a run of consecutive cycles in which a role's busiest pod
// read busy: since the first of them, and how many.
type SharePodBusy struct {
	since  time.Time
	cycles int
}

// NotePodBusy records which roles' busiest pod reads busy this cycle, and
// returns, per role still busy, the run of busy cycles it is in. A role not
// busy this cycle starts again from nothing.
func (l *ShareLedger) NotePodBusy(busy map[string]bool, now time.Time) map[string]SharePodBusy {
	for role := range l.podBusy {
		if !busy[role] {
			delete(l.podBusy, role)
		}
	}
	for role, b := range busy {
		if !b {
			continue
		}
		run, ok := l.podBusy[role]
		if !ok {
			run.since = now
		}
		run.cycles++
		l.podBusy[role] = run
	}
	return maps.Clone(l.podBusy)
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
}

// Forget drops a transfer that never took effect -- its donor pod could not be
// marked, or it was cancelled before any pod moved -- and removes the moves
// Start recorded for it: nothing moved, so it must neither hold its roles nor
// count toward a swing. Only its own moves go, in any order and after
// ConfirmStarted too, so other transfers between the same roles keep theirs.
func (l *ShareLedger) Forget(id string, tm ShareTimings) {
	i := slices.IndexFunc(l.transfers, func(t *ShareTransfer) bool { return t.ID == id })
	if i < 0 {
		return
	}
	t := l.transfers[i]
	l.transfers = slices.Delete(l.transfers, i, i+1)
	l.unrecord(t.Donor, id, tm)
	if t.Receiver != "" {
		l.unrecord(t.Receiver, id, tm)
	}
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

// GivingBusy reports whether role is held by HoldGiving: within the hold,
// and still giving to a live transfer. A transfer of its that ended -- a
// release that landed, a cancel, an abort -- may have left it a pod to give,
// so the hold ends with the last of them.
func (l *ShareLedger) GivingBusy(role string, now time.Time) bool {
	if l.Changing(role, now) {
		return true
	}
	if !now.Before(l.busyUntil[role]) {
		return false
	}
	for _, t := range l.transfers {
		if t.Donor == role && t.State == ShareReleasing {
			return true
		}
	}
	return false
}

// ReceiverFilled gives the next Observe the GPUs of each role's replicas
// every pod of which is scheduled, to judge receivers' fills by.
func (l *ShareLedger) ReceiverFilled(filled map[string]int) { l.filled = filled }

// HoldFill keeps receiver from being funded again -- by a transfer or by the
// idle fill -- until until: its last fill timed out with its pods Pending, and
// funding it again would take another whole replica of GPUs for a pod that
// cannot be placed either.
func (l *ShareLedger) HoldFill(receiver string, until time.Time) {
	if until.After(l.fillHeld[receiver]) {
		l.fillHeld[receiver] = until
	}
}

// FillHeld reports whether receiver is held by HoldFill.
func (l *ShareLedger) FillHeld(receiver string, now time.Time) bool {
	return now.Before(l.fillHeld[receiver])
}

// ShareChangingHold is a donor held by HoldChanging.
type ShareChangingHold struct {
	// Donor is the held role, and Variant the donor variant whose workload
	// was changing.
	Donor, Variant string
	// Receivers are the roles whose transfers from it were refused while it
	// holds (a spare return has none).
	Receivers []string
	// Reason is why its pods could not be steered, as the refusal said.
	Reason string
	// Since is when the hold began, and Until when it ends at the latest.
	Since, Until time.Time
	// SteadyCycles counts the cycles in a row its workload has read stable
	// since (ObserveChanging).
	SteadyCycles int
}

// HoldChanging keeps donor from being asked again until its workload is stable
// (ObserveChanging), or for at most one release timeout: its workload is
// changing -- a rollout, a pod not yet created, scheduled or Ready -- and which
// pod a lower count removes cannot be steered until it settles. variant is the
// donor variant that was refused, receiver the role the refused transfer was
// for (empty for a spare return), and reason why. Routine, not a fault: no
// abort, no back-off, not unsteerable, so a rollout cannot ratchet the
// back-off the next real abort starts from.
func (l *ShareLedger) HoldChanging(donor, variant, receiver, reason string, now time.Time, tm ShareTimings) {
	if donor == "" {
		return
	}
	h, held := l.changing[donor]
	if !held || !now.Before(h.Until) {
		h = ShareChangingHold{Donor: donor, Since: now}
	}
	h.Variant, h.Reason, h.SteadyCycles = variant, reason, 0
	if receiver != "" && !slices.Contains(h.Receivers, receiver) {
		h.Receivers = append(slices.Clone(h.Receivers), receiver)
	}
	if until := now.Add(tm.ReleaseTimeout); until.After(h.Until) {
		h.Until = until
	}
	l.changing[donor] = h
}

// Changing reports whether role is held by HoldChanging.
func (l *ShareLedger) Changing(role string, now time.Time) bool {
	h, ok := l.changing[role]
	return ok && now.Before(h.Until)
}

// ChangingBlocks reports whether a donor HoldChanging holds now refused a
// transfer to receiver.
func (l *ShareLedger) ChangingBlocks(receiver string, now time.Time) bool {
	for d, h := range l.changing {
		if d != receiver && now.Before(h.Until) && slices.Contains(h.Receivers, receiver) {
			return true
		}
	}
	return false
}

// ChangingHolds lists the donors HoldChanging holds now, by donor.
func (l *ShareLedger) ChangingHolds(now time.Time) []ShareChangingHold {
	var out []ShareChangingHold
	for _, h := range l.changing {
		if now.Before(h.Until) {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b ShareChangingHold) int { return strings.Compare(a.Donor, b.Donor) })
	return out
}

// ObserveChanging records one cycle's look at a held donor's workload: stable,
// or still changing for reason. The hold ends once the workload has read
// stable for confirm cycles in a row (at least one) -- a pod that became
// Ready, a rollout that finished -- rather than at the release timeout, which
// left a donor whose idle-fill pod became Ready in four minutes held for ten.
// A changing reading starts the count again. It returns the hold as updated,
// and whether it ended.
func (l *ShareLedger) ObserveChanging(donor string, stable bool, reason string, confirm int) (ShareChangingHold, bool) {
	h, ok := l.changing[donor]
	if !ok {
		return h, false
	}
	if !stable {
		h.SteadyCycles = 0
		if reason != "" {
			h.Reason = reason
		}
		l.changing[donor] = h
		return h, false
	}
	h.SteadyCycles++
	if h.SteadyCycles >= max(confirm, 1) {
		delete(l.changing, donor)
		return h, true
	}
	l.changing[donor] = h
	return h, false
}

// HoldGiving keeps donor from being asked again until one of its releases
// lands, or for one release timeout: every pod it could give is already given
// to a live transfer. Its receiver tries another donor meanwhile. Nothing
// failed, so unlike MarkFailed it counts no abort, sets no back-off and is not
// unsteerable.
func (l *ShareLedger) HoldGiving(donor string, now time.Time, tm ShareTimings) {
	if donor == "" {
		return
	}
	if until := now.Add(tm.ReleaseTimeout); until.After(l.busyUntil[donor]) {
		l.busyUntil[donor] = until
	}
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
		maps.Keys(l.lastGave), maps.Keys(l.lastGot), maps.Keys(l.moves), maps.Keys(l.cancels), maps.Keys(l.swingUntil), maps.Keys(l.needs)}
	for _, roles := range recorded {
		for r := range roles {
			if _, ok := l.seen[r]; !ok {
				l.seen[r] = now
			}
		}
	}
	horizon := max(tm.ReversalHold, 2*tm.SwingWindow, tm.ReleaseTimeout<<4)
	maps.DeleteFunc(l.noPair, func(_ sharePair, until time.Time) bool { return !now.Before(until) })
	for r, at := range l.seen {
		if now.Sub(at) <= horizon {
			continue
		}
		delete(l.seen, r)
		delete(l.aborts, r)
		delete(l.unsteerable, r)
		delete(l.busyUntil, r)
		delete(l.changing, r)
		delete(l.fillHeld, r)
		delete(l.giveAfter, r)
		delete(l.fillBlocked, r)
		delete(l.lastGave, r)
		delete(l.lastGot, r)
		delete(l.moves, r)
		delete(l.cancels, r)
		delete(l.swingUntil, r)
		delete(l.stepped, r)
		delete(l.needs, r)
		delete(l.needsSince, r)
		delete(l.hard, r)
		delete(l.over, r)
		delete(l.podBusy, r)
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
