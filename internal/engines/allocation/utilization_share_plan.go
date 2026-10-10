package allocation

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"time"
)

// Planner constants of docs/proposals/utilization-share-optimizer.md §8.4. They
// are not configuration: §6.7's simulation validated the anti-oscillation
// rules at these values, and changing one changes the decide-to-serve latency
// the derived timings assume.
const (
	// ShareConfirmCycles is how many consecutive cycles a role must be
	// actionable before a transfer may be planned for it.
	ShareConfirmCycles = 2
	// ShareMaxReplicasPerCycle bounds the replicas any one role gives or
	// receives in one planning cycle.
	ShareMaxReplicasPerCycle = 2
	// ShareMaxConcurrentTransfers bounds the transfers in flight per group.
	ShareMaxConcurrentTransfers = 2
	// ShareUrgentHeldFraction is the fraction of its need below which a
	// receiver may be funded through the reversal hold, from a donor that
	// keeps its own need (shareReversalExempt). Deep shortfalls only: in the
	// §6.7 simulation of anti-phase swings an exemption for every receiver
	// below its need reversed transfers and ran the 30-minute resonance at
	// 2.95x the shortfall of standing still, where below 0.75 it wasted none
	// and ran at 1.73x (1.86x with no exemption).
	ShareUrgentHeldFraction = 0.75
)

// SharePlanInput is one group's view for one planning cycle.
type SharePlanInput struct {
	// Roles carry this cycle's need, before the swing rule adjusts it.
	Roles []ShareRole
	// Held is each role's GPUs held now, terminating pods included.
	Held map[string]int
	// Thresholds is each role's scale-up threshold.
	Thresholds map[string]float64
	Budget     int
	Tolerance  float64
	// Give and Grow, when set, name the variant each role gives from and grows,
	// and size the move by their replicas (ShareGroup.Give, ShareGroup.Grow). A
	// role absent from a non-nil map cannot give, or cannot grow. Nil maps size
	// every move by the role's ReplicaGPUs.
	Give, Grow map[string]ShareVariant
	// Nodes is the per-node picture for the group's accelerator: each node's
	// free GPUs and labels. Nil without node information, and then donor pods
	// are never combined on a node (§6.5).
	Nodes map[string]ShareNode
	// DonorUnits are, per donor role, the replicas it could give and where
	// their pods run: one Ready, unmarked pod per Deployment replica, the
	// group a LeaderWorkerSet removes next. A role absent gives nothing to
	// a node-aware set.
	DonorUnits map[string][]ShareUnit
	// DomainKey is, per receiver role, the node label all its pods' holes must
	// share (a LeaderWorkerSet's exclusive topology), or "".
	DomainKey map[string]string
	// WakeHeld and PhysicalFree are what the idle fill subtracts from the
	// group's idle quota and caps it by (GPUs held for woken models, the
	// cluster's free GPUs of the type; math.MaxInt when unbounded), so the
	// planner leaves a receiver to the fill only when the fill can fund it.
	// A zero PhysicalFree means none is free.
	WakeHeld, PhysicalFree int
}

// ShareNode is one node's free GPUs and labels.
type ShareNode struct {
	Free   int
	Labels map[string]string
}

// SharePod is one donor pod: its name (namespace/name), node and GPUs.
type SharePod struct {
	Name, Node string
	GPUs       int
}

// ShareUnit is what one donor replica frees when it goes: its pods.
type ShareUnit struct {
	Pods []SharePod
}

// GPUs is the unit's total.
func (u ShareUnit) GPUs() int {
	n := 0
	for _, p := range u.Pods {
		n += p.GPUs
	}
	return n
}

// SharePlan is what one planning cycle decided.
type SharePlan struct {
	// Started are the transfers admitted this cycle, now Releasing.
	Started []ShareTransfer
	// Cancelled are the IDs of transfers cancelled this cycle.
	Cancelled []string
	// Evaluation is the band evaluation of the committed allocation after
	// cancellations and before this cycle's starts.
	Evaluation ShareEvaluation
	// Swinging lists the roles planned on their mean need this cycle.
	Swinging []string
	// ReserveDebt is how far the committed allocation stood above the budget
	// before this cycle's refills: reserve a wake spent, or anything else that
	// put the group over (section 6.2). Refills is how many transfers started
	// to pay it back.
	ReserveDebt int
	Refills     int
	// Unfunded names, for each confirmed receiver that got nothing this cycle
	// because no candidate donor's pods could host its replica, that reason
	// (ShareUnfundedNoCompatibleDonor). A receiver held back by a hold, the
	// pace or admission is not listed: those pass. A receiver with no donor at
	// all is not either: then GPUs are idle, and the idle fill funds it.
	Unfunded map[string]string
	// Withheld counts what was not planned this cycle, by reason:
	// ShareWithheldReversalHold (a pair skipped for rule 4) and
	// ShareWithheldNotActionable (a role out of band with no whole replica to
	// move, section 6.1 step 4).
	Withheld map[string]int
	// Nodes is the node picture this cycle's node-aware sets left: their
	// placements spent, for the idle fill to place into. Nil without node
	// information.
	Nodes map[string]ShareNode
}

// Reasons in SharePlan.Unfunded and SharePlan.Withheld.
const (
	ShareUnfundedNoCompatibleDonor = "no-compatible-donor"
	ShareWithheldReversalHold      = "reversal-hold"
	ShareWithheldNotActionable     = "not-actionable"
)

// ShareDebt is how far a committed allocation stands above the budget, or 0.
func ShareDebt(committed map[string]int, budget int) int {
	debt := -budget
	for _, c := range committed {
		debt += c
	}
	return max(0, debt)
}

// PlanShareTransfers runs one planning cycle for a group: the swing rule, the
// cancellations, the band evaluation and the admission of new transfers
// (proposal §6.1-§6.3, §6.7). It changes the ledger and returns what it did;
// the caller has already applied this cycle's observations (ShareLedger.Observe).
//
// A receiver is funded by one donor replica at least its replica's size where
// it can be, which is valid wherever the donor pod ran; otherwise by a donor
// set -- several donor replicas whose pods open one fitting hole together
// (§6.5, fundBySet).
func PlanShareTransfers(l *ShareLedger, in SharePlanInput, now time.Time, tm ShareTimings) SharePlan {
	// Node state is spent as node-aware sets start within the cycle; work on
	// copies, never the caller's.
	in.Nodes, in.DonorUnits = maps.Clone(in.Nodes), maps.Clone(in.DonorUnits)
	p := &sharePlanRun{l: l, in: in, now: now, tm: tm, moved: map[string]int{},
		plan: SharePlan{Unfunded: map[string]string{}, Withheld: map[string]int{}, Nodes: in.Nodes}}
	p.planningRoles()
	p.keep = shareKeep(p.roles, in.Budget)
	p.cont = ContinuousShareTargets(p.roles, float64(in.Budget))
	p.integ = IntegerShareTargets(p.roles, in.Budget)
	p.committed = l.Committed(in.Held)
	p.cancelReversals()

	// Rules 1 and 2: judge the committed allocation; a role is actionable only
	// when a move could fix it.
	p.plan.Evaluation = EvaluateShare(p.roles, p.committed, in.Thresholds, in.Budget, in.Tolerance)
	p.actionable = map[string]bool{}
	for _, v := range p.plan.Evaluation.Roles {
		p.actionable[v.Key] = v.Actionable
		if !v.InBand && !v.Actionable {
			p.plan.Withheld[ShareWithheldNotActionable]++
		}
	}
	p.confirmed = l.Confirm(p.actionable)

	p.refillReserve()
	if !slices.ContainsFunc(slices.Collect(maps.Keys(p.confirmed)), p.isConfirmed) {
		return p.plan
	}

	var receivers, donors []string
	for _, r := range p.roles {
		c := p.committed[r.Key]
		if c < p.integ[r.Key] {
			receivers = append(receivers, r.Key)
		}
		if c > p.integ[r.Key] && c > p.keep[r.Key] {
			donors = append(donors, r.Key)
		}
	}
	slices.SortFunc(receivers, func(a, b string) int {
		return cmp.Or(cmp.Compare(p.z(a, p.committed[a]), p.z(b, p.committed[b])), cmp.Compare(a, b))
	})
	slices.SortFunc(donors, func(a, b string) int {
		return cmp.Or(cmp.Compare(p.z(b, p.committed[b]), p.z(a, p.committed[a])), cmp.Compare(a, b))
	})

	p.work = maps.Clone(p.committed)
	for _, rc := range receivers {
		if !p.fund(rc, donors) {
			break // the concurrency cap is reached
		}
	}
	return p.plan
}

// sharePlanRun is one PlanShareTransfers call: its inputs, the targets and
// allocations it works from, and the plan it builds.
type sharePlanRun struct {
	l   *ShareLedger
	in  SharePlanInput
	now time.Time
	tm  ShareTimings

	plan  SharePlan
	roles []ShareRole
	byKey map[string]ShareRole
	// cont and integ are the continuous and whole-replica targets.
	cont  map[string]float64
	integ map[string]int
	// committed is the allocation net of transfers in flight; work is it
	// with this cycle's rebalance applied, receiver by receiver.
	committed, work map[string]int
	// moved counts each role's replicas moved this cycle -- refills and
	// rebalance alike -- against ShareMaxReplicasPerCycle.
	moved      map[string]int
	actionable map[string]bool
	confirmed  map[string]int
	// keep is the least each role may hold after giving (shareKeep).
	keep map[string]int
}

// shareLimits are what bounds a donor, per role: keep, the least it may hold
// after giving (shareKeep), and integ, its whole-replica target. A donor that
// stays at or above its whole-replica target is in band whatever its
// continuous band says: the target is where the plan means it to be, and a
// continuous band wider than a replica's step can put that target outside it
// -- 24 held as 8-GPU replicas, target 16, continuous 20.2, band 4 -- which
// would starve a short receiver for good.
type shareLimits struct {
	keep, integ map[string]int
}

// shareReversalExempt reports whether a move to receiver rc, holding rcHeld,
// from donor dn, left with dnLeft after giving, is exempt from the reversal
// hold: rc holds less than ShareUrgentHeldFraction of its need and dn stays at
// or above its own. The hold stops
// GPUs ping-ponging for headroom; it must not keep a model short while another
// holds more than it needs. Measured on alternating bursts: GPUs moved to a
// model whose burst had just ended (its draining backlog still read as need)
// as the other's began, and the hold kept them there for 18 minutes -- the
// whole burst served on 3 of 8 replicas. A role that "just gave" is exactly
// the one about to burst when bursts alternate. The cancelled-direction hold,
// back-offs and an exhausted donor still apply (ReceivingBlocked,
// GivingBlocked), and a swinging role is still planned on its mean need.
func shareReversalExempt(rcHeld int, rc ShareRole, dnLeft int, dn ShareRole) bool {
	return rc.Need > 0 && float64(rcHeld) < ShareUrgentHeldFraction*rc.Need && float64(dnLeft) >= dn.Need
}

// shareReceiveHeld is whether rc may not receive in a move the reversal
// exemption covers (exempt) or not.
func shareReceiveHeld(l *ShareLedger, rc string, exempt bool, now time.Time, tm ShareTimings) bool {
	if exempt {
		return l.ReceivingBlocked(rc, now)
	}
	return l.ReceivingHeld(rc, now, tm)
}

// shareGiveHeld is whether dn may not give in a move the reversal exemption
// covers (exempt) or not.
func shareGiveHeld(l *ShareLedger, dn string, exempt bool, now time.Time, tm ShareTimings) bool {
	if exempt {
		return l.GivingBlocked(dn, now)
	}
	return l.GivingHeld(dn, now, tm)
}

// donorInBand reports whether donor dn holding left GPUs after giving is in its
// band (§6.2): at or above its whole-replica target or its continuous target,
// or within the tolerance band around the latter.
func donorInBand(in SharePlanInput, lim shareLimits, cont map[string]float64, byKey map[string]ShareRole,
	dn string, left int) bool {
	l := float64(left)
	return left >= lim.integ[dn] || l >= cont[dn] || ShareInBand(l, cont[dn], byKey[dn], in.Tolerance, in.Thresholds[dn])
}

// shareKeep is the least each role may hold after giving: its floor, and --
// while whole replicas cover every role's claim (WholeReplicaCoverage) -- its
// claim (need, floor and ceiling applied), rounded up to whole GPUs. Covered, only headroom need move: a role
// short of its need can always be funded from free GPUs or from a donor
// holding a whole replica above its own rounded claim, so no donor need be
// taken below its need for it. Covered only in fractions -- 9 + 6 of 17, with
// the 9 in 8-GPU replicas -- the need cannot be kept without leaving the
// other role far below its own, and moves go to the worst off (§6.2), as they
// do when the budget is short; only the floor holds.
func shareKeep(roles []ShareRole, budget int) map[string]int {
	covered := WholeReplicaCoverage(roles, budget)
	keep := make(map[string]int, len(roles))
	for _, r := range roles {
		keep[r.Key] = r.Floor
		if covered {
			// The claim, not the need: it is capped at the ceiling, which
			// the need is not -- a role needing 20 under a ceiling of 8
			// would otherwise be held at 20 it can never hold, and could
			// neither give down to its ceiling nor repay a debt.
			keep[r.Key] = max(r.Floor, int(math.Ceil(r.Claim()-1e-9)))
		}
	}
	return keep
}

// limits are the run's donor bounds (shareLimits).
func (p *sharePlanRun) limits() shareLimits {
	return shareLimits{keep: p.keep, integ: p.integ}
}

// z is a role's score at g GPUs.
func (p *sharePlanRun) z(k string, g int) float64 {
	r := p.byKey[k]
	return ShareScore(float64(g), r.Need, r.Weight)
}

// isConfirmed reports whether a role has been actionable long enough to move.
func (p *sharePlanRun) isConfirmed(k string) bool { return p.confirmed[k] >= ShareConfirmCycles }

// planningRoles records this cycle's needs and plans a swinging role on its
// mean need (rule 5).
func (p *sharePlanRun) planningRoles() {
	raw := make(map[string]float64, len(p.in.Roles))
	for _, r := range p.in.Roles {
		raw[r.Key] = r.Need
	}
	p.l.RecordNeeds(raw, p.now, p.tm)
	p.roles = slices.Clone(p.in.Roles)
	p.byKey = make(map[string]ShareRole, len(p.roles))
	for i := range p.roles {
		if p.l.Swinging(p.roles[i].Key, p.now) {
			p.roles[i].Need = p.l.PlanningNeed(p.roles[i].Key, p.roles[i].Need, p.now)
			p.plan.Swinging = append(p.plan.Swinging, p.roles[i].Key)
		}
		p.byKey[p.roles[i].Key] = p.roles[i]
	}
}

// cancelReversals cancels a release on a clear reversal, and only while it
// is free (rule 3).
func (p *sharePlanRun) cancelReversals() {
	l, in, now, tm := p.l, p.in, p.now, p.tm
	for _, t := range l.Transfers() {
		if t.State != ShareReleasing || now.Sub(t.Started) >= tm.Window {
			continue
		}
		// A transfer with no receiver is a reserve refill, entitled and not
		// subject to hysteresis, or a set's contributor, which goes only with
		// its primary: cancelling it alone would leave the primary to raise its
		// receiver into a hole that never fully opens.
		if t.Receiver == "" {
			continue
		}
		dn, rc := p.byKey[t.Donor], p.byKey[t.Receiver]
		donorAfter := p.committed[t.Donor] // already net of this transfer
		receiverWithout := p.committed[t.Receiver] - t.GPUs
		donorShort := float64(donorAfter) < p.cont[t.Donor]-2*shareTol(p.cont[t.Donor], dn, in.Tolerance)
		receiverFine := float64(receiverWithout) > p.cont[t.Receiver]+shareTol(p.cont[t.Receiver], rc, in.Tolerance)
		if (donorShort || receiverFine) && l.Cancel(t.ID, now, tm) {
			p.plan.Cancelled = append(p.plan.Cancelled, t.ID)
			// A set's contributors go with its primary, while they still can.
			if t.IsSetPrimary() {
				for _, m := range l.Transfers() {
					if m.SetID == t.ID && l.Cancel(m.ID, now, tm) {
						p.plan.Cancelled = append(p.plan.Cancelled, m.ID)
					}
				}
			}
			p.committed = l.Committed(in.Held)
		}
	}
}

// refillReserve pays back a reserve debt (section 6.2). Committed GPUs above
// the budget are a debt: a wake took reserve, or a variant was scaled past its
// target from outside. It is an entitled receiver, paid back before any
// rebalance and without the confirmation a rebalance needs, by transfers with
// a donor and no receiver: the donor is lowered and nobody is raised. A donor
// gives only above its whole-replica target and its floor, best-off first.
func (p *sharePlanRun) refillReserve() {
	l, in, now, tm := p.l, p.in, p.now, p.tm
	p.plan.ReserveDebt = ShareDebt(p.committed, in.Budget)
	debt := p.plan.ReserveDebt
	if debt <= 0 {
		return
	}
	var cands []string
	for _, r := range p.roles {
		if c := p.committed[r.Key]; c > p.integ[r.Key] && c > r.Floor {
			cands = append(cands, r.Key)
		}
	}
	zc := func(k string) float64 { return p.z(k, p.committed[k]) }
	slices.SortFunc(cands, func(a, b string) int { return cmp.Or(cmp.Compare(zc(b), zc(a)), cmp.Compare(a, b)) })
	for _, dn := range cands {
		if debt <= 0 || l.InFlight() >= ShareMaxConcurrentTransfers {
			break
		}
		give := ShareVariant{GPUs: p.byKey[dn].ReplicaGPUs}
		if in.Give != nil {
			v, ok := in.Give[dn]
			if !ok {
				continue
			}
			give = v
		}
		gd := max(give.GPUs, 1)
		if l.GivingHeld(dn, now, tm) {
			continue
		}
		// The same pace as a rebalance: up to ShareMaxReplicasPerCycle
		// replicas per role, never below its whole-replica target or floor.
		// Not held to its need (shareKeep): the debt is GPUs the budget does
		// not have, and repaying it is entitled (§6.2).
		left := p.committed[dn]
		for p.moved[dn] < ShareMaxReplicasPerCycle && debt > 0 && l.InFlight() < ShareMaxConcurrentTransfers {
			if left-gd < p.byKey[dn].Floor || left-gd < p.integ[dn] {
				break
			}
			t := l.Start(ShareTransfer{Donor: dn, DonorGPUs: gd, DonorVariant: give.Name, Entitled: true}, in.Held, now, tm)
			p.plan.Started = append(p.plan.Started, t)
			p.plan.Refills++
			p.moved[dn]++
			debt -= gd
			left -= gd
		}
	}
	p.committed = l.Committed(in.Held)
}

// fund funds receiver rc from donors: by single donor replicas first, and by
// a donor set when no single replica fits. It reports false when the
// concurrency cap stops the whole plan.
func (p *sharePlanRun) fund(rc string, donors []string) bool {
	l, in, now, tm := p.l, p.in, p.now, p.tm
	work, moved, byKey := p.work, p.moved, p.byKey
	candidates, misfits, funded := 0, 0, false
	if l.FillHeld(rc, now) {
		return true // its last fill timed out with its pods Pending (HoldFill)
	}
	for _, dn := range donors {
		if l.InFlight() >= ShareMaxConcurrentTransfers {
			return false
		}
		if moved[rc] >= ShareMaxReplicasPerCycle || moved[dn] >= ShareMaxReplicasPerCycle {
			continue
		}
		// A receiver funded up to its whole-replica target takes no more,
		// and a donor down to its own gives no more: past either, the next
		// plan would only move the replica back. Defence in depth: with
		// the surplus above the targets equal to the shortfall below them
		// and two replicas per role per cycle, no fleet reaches it today.
		if work[rc] >= p.integ[rc] {
			break
		}
		if work[dn] <= p.integ[dn] {
			continue
		}
		rr, dr := byKey[rc], byKey[dn]
		grow := ShareVariant{GPUs: rr.ReplicaGPUs}
		give := ShareVariant{GPUs: dr.ReplicaGPUs}
		if in.Grow != nil {
			v, ok := in.Grow[rc]
			if !ok {
				continue
			}
			grow = v
		}
		if in.Give != nil {
			v, ok := in.Give[dn]
			if !ok {
				continue
			}
			give = v
		}
		g, gd := max(grow.GPUs, 1), max(give.GPUs, 1)
		candidates++
		if !ShareCovers(give, grow) {
			misfits++
			continue // the donor's pods cannot host the receiver's (§6.5)
		}
		if l.GivingBusy(dn, now) {
			continue // it has given all it can until its releases land
		}
		// Rule 4: a role that gave cannot receive, and one that received
		// cannot give, within the hold -- unless the receiver is short and the
		// donor has more than it needs (shareReversalExempt).
		exempt := shareReversalExempt(work[rc], rr, work[dn]-gd, dr)
		if shareReceiveHeld(l, rc, exempt, now, tm) || shareGiveHeld(l, dn, exempt, now, tm) {
			p.plan.Withheld[ShareWithheldReversalHold]++
			continue
		}
		if !p.isConfirmed(rc) && !p.isConfirmed(dn) {
			continue // every transfer must fix a confirmed actionable role
		}
		// §6.2 admission: the worst-off role of the pair improves, and the
		// donor is not pushed out of band.
		before := math.Min(p.z(dn, work[dn]), p.z(rc, work[rc]))
		after := math.Min(p.z(dn, work[dn]-gd), p.z(rc, work[rc]+g))
		donorOK := donorInBand(in, p.limits(), p.cont, byKey, dn, work[dn]-gd)
		if !(after > before) || !donorOK || work[dn]-gd < p.keep[dn] {
			continue
		}
		t := l.Start(ShareTransfer{
			Donor: dn, Receiver: rc, GPUs: g, DonorGPUs: gd,
			DonorVariant: in.Give[dn].Name, ReceiverVariant: in.Grow[rc].Name,
			Urgent: float64(work[rc]) < rr.Need,
		}, in.Held, now, tm)
		p.plan.Started = append(p.plan.Started, t)
		work[dn] -= gd
		work[rc] += g
		moved[dn]++
		moved[rc]++
		funded = true
	}
	if !funded && candidates > 0 && misfits == candidates && in.Grow != nil {
		var deferred bool
		funded, deferred = p.fundBySet(rc, donors)
		if deferred {
			return true
		}
	}
	if !funded && p.isConfirmed(rc) && p.actionable[rc] && candidates > 0 && misfits == candidates {
		p.plan.Unfunded[rc] = ShareUnfundedNoCompatibleDonor
	}
	return true
}

// fundBySet funds rc from several donors when no single donor replica fits
// (§6.5). With node information, a node's free GPUs and the donor pods on it
// make one hole together, and the holes can be kept in the receiver's
// topology domain. Without it, or when it finds nothing, each donor pod is a
// hole of its own size -- true on any node, but blind to a domain, so not for
// a receiver that has one. deferred is set when the receiver's pods fit free
// GPUs and the quota has room: the idle fill funds it, and no donor shrinks.
func (p *sharePlanRun) fundBySet(rc string, donors []string) (funded, deferred bool) {
	l, in, now, tm := p.l, p.in, p.now, p.tm
	var set []shareDonor
	if in.Nodes != nil {
		var left map[string]int
		set, left, deferred = shareNodeSet(l, in, rc, donors, p.work, p.moved, p.byKey, p.limits(), p.cont, p.z, p.isConfirmed, now, tm)
		if len(set) > 0 {
			withdrawNodeSet(in.Nodes, in.DonorUnits, set, left)
		}
	}
	if deferred {
		return false, true
	}
	if len(set) == 0 && in.DomainKey[rc] == "" {
		set = shareDonorSet(l, in, rc, donors, p.work, p.moved, p.byKey, p.limits(), p.cont, p.z, p.isConfirmed, now, tm)
	}
	if len(set) == 0 {
		return false, false
	}
	started := startShareSet(l, in, rc, set, float64(p.work[rc]) < p.byKey[rc].Need, now, tm)
	p.plan.Started = append(p.plan.Started, started...)
	for _, d := range set {
		p.work[d.role] -= d.gpus
		p.moved[d.role]++
	}
	p.work[rc] += max(in.Grow[rc].GPUs, 1)
	p.moved[rc]++
	return true, false
}

// shareTol is a role's band half-width in GPUs at target (proposal §6.1): the
// configured tolerance of the target, never under half a replica.
func shareTol(target float64, r ShareRole, tolerance float64) float64 {
	return math.Max(tolerance*target, 0.5*float64(max(r.ReplicaGPUs, 1)))
}

// shareDonor is one donor replica in a set.
type shareDonor struct {
	role, variant string
	pods          []int
	gpus          int
	// planned are the donor pods a node-aware set chose (namespace/name):
	// the ones that must go. unit is every pod of the replica the set took,
	// planned or not, so the cycle stops offering it.
	planned, unit []string
}

// shareDonorSet looks for donor replicas that together fund one replica of
// receiver rc (§6.5): each receiver pod, largest first, by a donor pod of its
// own at least its size -- first a spare pod of a replica already in the set,
// then a new replica from the donor with the most to spare. Without pod shapes
// it finds nothing: several replicas' GPUs are never added up. The whole set
// must pass §6.2 admission: the lowest score among the receiver and every
// donor rises, no donor leaves its band or goes below its floor, and the holds,
// the pace and the concurrency limit hold for every member.
func shareDonorSet(l *ShareLedger, in SharePlanInput, rc string, donors []string, work, moved map[string]int,
	byKey map[string]ShareRole, lim shareLimits, cont map[string]float64, z func(string, int) float64,
	isConfirmed func(string) bool, now time.Time, tm ShareTimings) []shareDonor {
	grow, ok := in.Grow[rc]
	if !ok || len(grow.PodGPUs) == 0 || in.Give == nil {
		return nil
	}
	// A short receiver may be funded through its reversal hold, by donors
	// that keep their need; each donor is checked below.
	short := float64(work[rc]) < byKey[rc].Need
	if shareReceiveHeld(l, rc, short, now, tm) || moved[rc] >= ShareMaxReplicasPerCycle {
		return nil
	}
	need := slices.Sorted(slices.Values(grow.PodGPUs))
	slices.Reverse(need)

	var set []shareDonor
	spare := [][]int{} // per set member, its pods not yet assigned
	taken := map[string]int{}
	for _, p := range need {
		// A spare pod of a replica already in the set: the smallest that fits.
		bi, bj := -1, -1
		for i := range spare {
			for j, s := range spare[i] {
				if s >= p && (bi < 0 || s < spare[bi][bj]) {
					bi, bj = i, j
				}
			}
		}
		if bi >= 0 {
			spare[bi] = slices.Delete(spare[bi], bj, bj+1)
			continue
		}
		// A new replica, from the donor with the most to spare.
		added := false
		for _, dn := range donors {
			give, ok := in.Give[dn]
			if !ok || len(give.PodGPUs) == 0 || dn == rc {
				continue
			}
			gd := max(give.GPUs, 1)
			k := taken[dn] + 1
			exempt := shareReversalExempt(work[rc], byKey[rc], work[dn]-k*gd, byKey[dn])
			if moved[dn]+k > shareSetPace(grow) || work[dn]-k*gd < lim.keep[dn] || shareGiveHeld(l, dn, exempt, now, tm) ||
				(!exempt && l.ReceivingHeld(rc, now, tm)) {
				continue
			}
			pods := slices.Sorted(slices.Values(give.PodGPUs))
			j := slices.IndexFunc(pods, func(s int) bool { return s >= p })
			if j < 0 {
				continue
			}
			set = append(set, shareDonor{role: dn, variant: give.Name, pods: give.PodGPUs, gpus: gd})
			spare = append(spare, slices.Delete(pods, j, j+1))
			taken[dn]++
			added = true
			break
		}
		if !added {
			return nil
		}
	}
	if len(set) < 2 || !admitShareSet(l, in, rc, taken, work, byKey, lim, cont, z, isConfirmed) {
		return nil
	}
	return set
}

// admitShareSet is §6.2 admission over a whole donor set (§6.5): the set fits
// the concurrency limit, the lowest score among the receiver and every donor
// rises, no donor leaves its band, and a confirmed role takes part. taken is
// the replicas each donor gives.
func admitShareSet(l *ShareLedger, in SharePlanInput, rc string, taken map[string]int,
	work map[string]int, byKey map[string]ShareRole, lim shareLimits, cont map[string]float64, z func(string, int) float64,
	isConfirmed func(string) bool) bool {
	if l.InFlight()+1 > ShareMaxConcurrentTransfers {
		return false
	}
	g := max(in.Grow[rc].GPUs, 1)
	confirmed := isConfirmed(rc)
	before, after := z(rc, work[rc]), z(rc, work[rc]+g)
	for dn, k := range taken {
		gd := max(in.Give[dn].GPUs, 1)
		before = math.Min(before, z(dn, work[dn]))
		after = math.Min(after, z(dn, work[dn]-k*gd))
		if !donorInBand(in, lim, cont, byKey, dn, work[dn]-k*gd) {
			return false
		}
		confirmed = confirmed || isConfirmed(dn)
	}
	return confirmed && after > before
}

// startShareSet starts a donor set's transfers: the primary, carrying the
// receiver replica, and one contributor per further donor replica, linked.
func startShareSet(l *ShareLedger, in SharePlanInput, rc string, set []shareDonor, urgent bool,
	now time.Time, tm ShareTimings) []ShareTransfer {
	grow := in.Grow[rc]
	setID := "pending"
	if len(set) == 1 {
		setID = "" // a single donor replica, placed on a node: an ordinary transfer
	}
	primary := l.Start(ShareTransfer{
		Donor: set[0].role, Receiver: rc, GPUs: max(grow.GPUs, 1), DonorGPUs: set[0].gpus,
		DonorVariant: set[0].variant, ReceiverVariant: grow.Name, SetID: setID, Urgent: urgent,
		PlannedPods: set[0].planned,
	}, in.Held, now, tm)
	ids := make([]string, 0, len(set)-1)
	for _, d := range set[1:] {
		c := l.Start(ShareTransfer{Donor: d.role, DonorGPUs: d.gpus, DonorVariant: d.variant, SetID: "pending",
			PlannedPods: d.planned}, in.Held, now, tm)
		ids = append(ids, c.ID)
	}
	if len(set) == 1 {
		return []ShareTransfer{primary}
	}
	l.LinkSet(primary.ID, ids...)
	out := make([]ShareTransfer, 0, len(set))
	for _, id := range append([]string{primary.ID}, ids...) {
		if t, ok := l.Transfer(id); ok {
			out = append(out, t)
		}
	}
	return out
}

// shareNodeSet looks for donor replicas that open, node by node, a hole for
// every pod of one replica of receiver rc (§6.5, with node information). A
// hole on a node is its free GPUs plus the GPUs of the chosen donor pods on
// it, so donor pods too small on their own can fund a pod together where they
// share a node, and a node's free GPUs can complete a hole.
//
// Receiver pods are placed largest first. A pod goes into an existing hole
// where one fits, the smallest that does; otherwise a hole is opened on the
// node that needs the fewest further donor replicas, taking the donors with the
// most to spare first, and on a tie the one that leaves the smallest hole. A
// receiver with an exclusive topology keeps every hole in one domain.
//
// The donors must give at least the receiver's replica: free GPUs complete a
// hole, they never stand in for a donor -- the quota still has to fit. Where
// the placement needs fewer donor replicas than that -- none at all when the
// receiver's pods fit free GPUs, as on a cluster whose quota is smaller than
// its nodes -- donor replicas from any node make up the quota. The set passes
// the same admission as shareDonorSet.
func shareNodeSet(l *ShareLedger, in SharePlanInput, rc string, donors []string, work, moved map[string]int,
	byKey map[string]ShareRole, lim shareLimits, cont map[string]float64, z func(string, int) float64,
	isConfirmed func(string) bool, now time.Time, tm ShareTimings) ([]shareDonor, map[string]int, bool) {
	grow, ok := in.Grow[rc]
	if !ok || len(grow.PodGPUs) == 0 || in.Give == nil || in.DonorUnits == nil {
		return nil, nil, false
	}
	short := float64(work[rc]) < byKey[rc].Need
	if shareReceiveHeld(l, rc, short, now, tm) || moved[rc] >= ShareMaxReplicasPerCycle {
		return nil, nil, false
	}
	// Each domain is tried in turn: fixing it at the first placement would
	// refuse receivers another domain fits.
	key := in.DomainKey[rc]
	for _, domain := range shareDomains(in.Nodes, key) {
		in1 := shareNodesInDomain(in.Nodes, key, domain)
		search := shareNodeSearch{l: l, in: in, rc: rc, donors: donors, work: work, moved: moved, byKey: byKey, keep: lim.keep,
			now: now, tm: tm, taken: map[string]int{}, used: map[shareUnitRef]bool{}}
		hole, ok := search.place(grow.PodGPUs, in1)
		if !ok {
			continue
		}
		if len(search.set) == 0 && shareIdle(in, work) >= max(grow.GPUs, 1) && !l.FillShort(rc) {
			// The pods fit free GPUs and the quota has room: the idle fill
			// funds this receiver without shrinking anyone -- unless the last
			// fill left it short, for a reason only the fill sees. Not
			// another domain, nor the node-blind search: either would shrink
			// donors for GPUs nobody needed.
			return nil, nil, true
		}
		// The placement may need fewer donor replicas than the receiver's
		// quota -- or none, where its pods fit free GPUs. Free GPUs place a
		// pod; they never pay for it: donor replicas from any node make up
		// the quota, their pods' GPUs coming free wherever they run.
		if !search.topUp(max(grow.GPUs, 1), hole) ||
			!admitShareSet(l, in, rc, search.taken, work, byKey, lim, cont, z, isConfirmed) {
			continue
		}
		return search.set, hole, false
	}
	return nil, nil, false
}

// place puts every receiver pod, largest first, on one of nodes: into the
// smallest hole that already fits, otherwise into a hole opened on the node
// needing the fewest donor replicas, then leaving the smallest hole. It
// returns the holes left on every node, and whether every pod was placed.
func (s *shareNodeSearch) place(podGPUs []int, nodes []string) (map[string]int, bool) {
	hole := map[string]int{}
	for n, info := range s.in.Nodes {
		hole[n] = info.Free
	}
	for _, p := range largestFirst(podGPUs) {
		best := shareBestFit(hole, nodes, p)
		if best == "" {
			var units []shareUnitRef
			left := 0
			for _, n := range nodes {
				u, h, ok := s.open(n, hole[n], p)
				if ok && (best == "" || len(u) < len(units) || (len(u) == len(units) && h-p < left)) {
					best, units, left = n, u, h-p
				}
			}
			if best == "" {
				return nil, false
			}
			for _, r := range units {
				s.take(r, hole, true)
			}
		}
		hole[best] -= p
	}
	return hole, true
}

// shareUnitRef names one donor replica: a donor role and its unit's index.
type shareUnitRef struct {
	donor string
	unit  int
}

// shareNodeSearch is the state of one shareNodeSet search: the donor
// replicas chosen so far, in order, and how many each donor gives.
type shareNodeSearch struct {
	l           *ShareLedger
	in          SharePlanInput
	rc          string
	donors      []string
	work, moved map[string]int
	byKey       map[string]ShareRole
	keep        map[string]int
	now         time.Time
	tm          ShareTimings
	set         []shareDonor
	taken       map[string]int
	used        map[shareUnitRef]bool
}

// canGive reports whether donor dn may give extra replicas more than it
// already does in the set: its keep, the set pace, its holds and the
// concurrency limit, which the whole set counts against once.
func (s *shareNodeSearch) canGive(dn string, extra int) bool {
	give, ok := s.in.Give[dn]
	if !ok || dn == s.rc {
		return false
	}
	k := s.taken[dn] + extra
	left := s.work[dn] - k*max(give.GPUs, 1)
	exempt := shareReversalExempt(s.work[s.rc], s.byKey[s.rc], left, s.byKey[dn])
	return s.moved[dn]+k <= shareSetPace(s.in.Grow[s.rc]) && left >= s.keep[dn] &&
		!shareGiveHeld(s.l, dn, exempt, s.now, s.tm) && (exempt || !s.l.ReceivingHeld(s.rc, s.now, s.tm)) &&
		s.l.InFlight()+1 <= ShareMaxConcurrentTransfers
}

// shareSetPace is how many replicas one donor may give a donor set in a cycle:
// the pace, or as many as the receiver replica has pods -- one whole-node
// donor pod per receiver pod is the only way to fund an LWS group of three or
// more 8-GPU pods from 8-GPU donors, and the set moves one receiver replica.
func shareSetPace(grow ShareVariant) int {
	return max(ShareMaxReplicasPerCycle, len(grow.PodGPUs))
}

// open finds the donor replicas that, added to free, make a hole of at least
// p GPUs on node n: donors in order of how much they have to spare, each
// replica with a pod on n. It returns them, the hole they make, and whether
// it reaches p.
func (s *shareNodeSearch) open(n string, free, p int) ([]shareUnitRef, int, bool) {
	h := free
	extra := map[string]int{}
	var units []shareUnitRef
	for _, dn := range s.donors {
		for ui, u := range s.in.DonorUnits[dn] {
			if h >= p {
				return units, h, true
			}
			r := shareUnitRef{dn, ui}
			if s.used[r] || !s.canGive(dn, extra[dn]+1) {
				continue
			}
			on := 0
			for _, pod := range u.Pods {
				if pod.Node == n {
					on += pod.GPUs
				}
			}
			if on == 0 {
				continue
			}
			units = append(units, r)
			extra[dn]++
			h += on
		}
	}
	return units, h, h >= p
}

// topUp adds donor replicas, from any node, until the set gives at least g
// GPUs of quota, and reports whether it does.
func (s *shareNodeSearch) topUp(g int, hole map[string]int) bool {
	gives := func() int {
		n := 0
		for _, d := range s.set {
			n += d.gpus
		}
		return n
	}
	for _, dn := range s.donors {
		for ui := range s.in.DonorUnits[dn] {
			if gives() >= g {
				return true
			}
			r := shareUnitRef{dn, ui}
			if s.used[r] || !s.canGive(dn, 1) {
				continue
			}
			s.take(r, hole, false)
		}
	}
	return gives() >= g
}

// shareIdle is the GPUs the idle fill could place now, measured as the fill
// measures them: the group's quota no role commits, less what is held for
// woken models, capped by the cluster's free GPUs.
func shareIdle(in SharePlanInput, work map[string]int) int {
	idle := in.Budget - in.WakeHeld
	for _, w := range work {
		idle -= w
	}
	return min(idle, in.PhysicalFree)
}

// take adds a donor replica to the set: every node its pods run on gains
// their GPUs as a hole. planned marks its pods as the ones that must go --
// true for a replica that opens a hole, false for one that only pays quota,
// where any of the donor's pods will do and the which-pod check would only
// abort a transfer for nothing.
func (s *shareNodeSearch) take(r shareUnitRef, hole map[string]int, planned bool) {
	s.used[r] = true
	s.taken[r.donor]++
	give := s.in.Give[r.donor]
	d := shareDonor{role: r.donor, variant: give.Name, gpus: max(give.GPUs, 1)}
	for _, pod := range s.in.DonorUnits[r.donor][r.unit].Pods {
		hole[pod.Node] += pod.GPUs
		d.unit = append(d.unit, pod.Name)
		if planned {
			d.planned = append(d.planned, pod.Name)
		}
	}
	s.set = append(s.set, d)
}

// withdrawNodeSet spends a started node-aware set within the planning cycle:
// each node keeps as free only what the set left unplaced and was free before
// -- a donor's surplus is not free until it is released, and a released hole
// is the receiver's -- and the set's donor pods are no longer offered.
func withdrawNodeSet(nodes map[string]ShareNode, units map[string][]ShareUnit, set []shareDonor, left map[string]int) {
	for n, info := range nodes {
		info.Free = max(0, min(info.Free, left[n]))
		nodes[n] = info
	}
	gone := map[string]bool{}
	for _, d := range set {
		for _, p := range d.unit {
			gone[p] = true
		}
	}
	for role, us := range units {
		units[role] = slices.DeleteFunc(slices.Clone(us), func(u ShareUnit) bool {
			return slices.ContainsFunc(u.Pods, func(p SharePod) bool { return gone[p.Name] })
		})
	}
}

// ShareFitPods places one replica's pods, largest first, each into the node
// with the smallest free count that holds it, and spends what it placed. A
// receiver with an exclusive topology (domainKey) is placed in one domain,
// each tried in turn; a node without the label is in none. It reports
// whether every pod fitted, and spends nothing when not. It places exactly as
// the node-aware set search does where no donor is needed.
func ShareFitPods(nodes map[string]ShareNode, podGPUs []int, domainKey string) bool {
	for _, domain := range shareDomains(nodes, domainKey) {
		names := shareNodesInDomain(nodes, domainKey, domain)
		free := map[string]int{}
		for _, n := range names {
			free[n] = nodes[n].Free
		}
		fits := true
		for _, p := range largestFirst(podGPUs) {
			best := shareBestFit(free, names, p)
			if best == "" {
				fits = false
				break
			}
			free[best] -= p
		}
		if fits {
			for n, f := range free {
				info := nodes[n]
				info.Free = f
				nodes[n] = info
			}
			return true
		}
	}
	return false
}

// shareDomains is the domains a receiver may be placed in, in order: every
// value of key on the nodes, or the one domain "" -- all nodes -- when key is
// "". A node without the label is in no domain.
func shareDomains(nodes map[string]ShareNode, key string) []string {
	if key == "" {
		return []string{""}
	}
	var out []string
	for _, n := range nodes {
		if v := n.Labels[key]; v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// shareNodesInDomain is the nodes of one domain, by name.
func shareNodesInDomain(nodes map[string]ShareNode, key, domain string) []string {
	var out []string
	for _, n := range slices.Sorted(maps.Keys(nodes)) {
		if key == "" || nodes[n].Labels[key] == domain {
			out = append(out, n)
		}
	}
	return out
}

// shareBestFit is the node of names with the smallest hole that holds p
// GPUs, the first by name on a tie, or "".
func shareBestFit(hole map[string]int, names []string, p int) string {
	best := ""
	for _, n := range names {
		if h, ok := hole[n]; ok && h >= p && (best == "" || h < hole[best]) {
			best = n
		}
	}
	return best
}

// largestFirst is a replica's pod sizes, largest first.
func largestFirst(podGPUs []int) []int {
	out := slices.Sorted(slices.Values(podGPUs))
	slices.Reverse(out)
	return out
}
