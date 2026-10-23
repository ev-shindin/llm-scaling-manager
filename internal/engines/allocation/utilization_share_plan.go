package allocation

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
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
	// ShareSkipWaitsReceiverLoad and ShareSkipWaitsDonorLoad are the default hard-imbalance
	// band (ShareSkipWaits; config.DefaultSkipWaitsReceiverLoad and
	// DefaultSkipWaitsDonorLoad): a receiver whose need is at least
	// ShareSkipWaitsReceiverLoad of what it holds -- at or near its scale-up
	// threshold -- and a donor whose need stays at most ShareSkipWaitsDonorLoad of
	// what it keeps. Such a move skips the reversal hold and the confirm
	// cycles. The gap between the two is the hysteresis that keeps it from
	// oscillating: after the move the donor sits at or below ShareSkipWaitsDonorLoad
	// and the receiver below ShareSkipWaitsReceiverLoad, so the reverse move cannot
	// qualify until demand itself has changed by their ratio.
	ShareSkipWaitsReceiverLoad = 0.9
	ShareSkipWaitsDonorLoad    = 0.6
	// ShareUnpacedAfterCycles is how many consecutive cycles a role must be
	// a hard-imbalance receiver before its moves skip the pace and the
	// concurrency limit (sharePlanRun.unpaced).
	ShareUnpacedAfterCycles = 2
	// ShareFastSwingSpread and ShareLongBurstSpread are the least range of a
	// role's need history, as a fraction of its highest need, for its bursts
	// to count (ShareLedger.SwingsFast, ShareLedger.BurstsLong): below them a
	// role's need is steady with noise, not bursting.
	ShareFastSwingSpread = 0.25
	ShareLongBurstSpread = 0.5
)

// ShareFillPace is how many replicas the idle fill raises a receiver by in one
// cycle: every one it is short while it holds less than its need, otherwise
// ShareMaxReplicasPerCycle. A fill drains nobody, so pacing a model below its
// need only makes it wait while GPUs sit idle -- and a donor that funds the
// replicas the pace held back drains a pod for GPUs that were free.
func ShareFillPace(need float64, committed int) int {
	if float64(committed) < need {
		return math.MaxInt
	}
	return ShareMaxReplicasPerCycle
}

// SharePlanInput is one group's view for one planning cycle.
type SharePlanInput struct {
	// Roles carry this cycle's need, before the swing rule adjusts it.
	Roles []ShareRole
	// Held is each role's GPUs held now, terminating pods included.
	Held map[string]int
	// Thresholds is each role's scale-up threshold.
	Thresholds map[string]float64
	// DownThresholds is each role's scale-down boundary. A role without one,
	// or with one at or above its scale-up threshold, has no scale-down band
	// (sharePlanRun.bandKeep).
	DownThresholds map[string]float64
	// BusiestQueueLoad is each role's deepest per-pod waiting queue as a
	// fraction of queueLengthThreshold, the queue at which a replica counts
	// as saturated. returnSpare compares it with the return load
	// (returnSpareWhenLoadAtMost): above it, read on the confirm cycles in a
	// row, a return the fleet's load would let go waits -- at most the
	// donor's scale-down window. A role absent has no reading and never
	// waits on it.
	BusiestQueueLoad map[string]float64
	Budget           int
	Tolerance        float64
	// SkipWaits is when a hard imbalance skips the waits. The zero value is
	// the default.
	SkipWaits ShareSkipWaits
	// ConfirmCycles is how many consecutive cycles a role must be actionable
	// before it moves (stabilization.consecutiveCyclesBeforeMove). Zero takes
	// ShareConfirmCycles.
	ConfirmCycles int
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
	// Placements is, per receiver role, its pod templates' nodeSelector and
	// required node affinity (domain.VariantReplicaState.Placements: one
	// template, or the leader's then the workers'), judged per pod as the
	// node view judges them (podTemplates). A role absent is not judged.
	Placements map[string][]domain.PodPlacement
	// WakeHeld and PhysicalFree are what the idle fill subtracts from the
	// group's idle quota and caps it by (GPUs held for woken models, the
	// cluster's free GPUs of the type; math.MaxInt when unbounded), so the
	// planner leaves a receiver to the fill only when the fill can fund it.
	// A zero PhysicalFree means none is free.
	WakeHeld, PhysicalFree int
	// Headroom limits the spare each role's targets give it, and governs when
	// a role gives back what it holds above them. The zero value spreads the
	// whole spare.
	Headroom ShareHeadroom
	// Origins names each role's model (ShareGroup.Origins), so the roles of
	// one model are paced together (ShareLedger.Unpaced). Nil judges each
	// role alone.
	Origins map[string]ShareRoleOrigin
}

// ShareModelRoles lists the roles of role's model in origins -- its prefill
// and decode, or role alone -- sorted; nil when origins does not name role.
func ShareModelRoles(origins map[string]ShareRoleOrigin, role string) []string {
	o, ok := origins[role]
	if !ok {
		return nil
	}
	var out []string
	for k, x := range origins {
		if x.Namespace == o.Namespace && x.ModelID == o.ModelID {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// ShareHeadroom is utilizationShare.maxHeadroomReplicas and
// stabilization.returnSpareWhenLoadAtMost in force.
type ShareHeadroom struct {
	// Capped limits each role's targets to its claim, rounded up to whole
	// replicas, plus ShareHeadroomReplicas(weight, Replicas) replicas
	// (ShareCapSpare).
	Capped   bool
	Replicas int
	// ReturnLoad is the load, on what it would keep one spare replica lower,
	// at or below which a role gives back what it holds above its target
	// (returnSpare). Zero takes ShareSkipWaitsDonorLoad.
	ReturnLoad float64
}

// returnLoad is the return load in force.
func (h ShareHeadroom) returnLoad() float64 {
	if h.ReturnLoad > 0 {
		return h.ReturnLoad
	}
	return ShareSkipWaitsDonorLoad
}

// spareReplicas is the replicas of headroom the targets give role r: its
// weight's share of Replicas, or one when the spare is not capped -- the
// replica the return's calm rule keeps in hand.
func (h ShareHeadroom) spareReplicas(r ShareRole) int {
	if !h.Capped {
		return 1
	}
	return ShareHeadroomReplicas(r.Weight, h.Replicas)
}

// ShareNode is one node's free GPUs and labels, and whether its topology
// domain already holds a LeaderWorkerSet group: an exclusive-topology
// receiver's group lands in no such domain, as the node view judges it
// (NodeView.domainTaken).
type ShareNode struct {
	Free   int
	Labels map[string]string
	// Groups is the LeaderWorkerSet groups with a pod on the node
	// (decision.NodeGPU.LWSGroups). A donor set that takes every such group
	// of a domain frees it for the receiver's group.
	Groups []string
	// Claimed is the topology label keys for which this cycle already placed
	// an exclusive-topology group in the node's domain -- the optimizer's pass
	// or an in-flight hold (NodeView.TakenDomains), or the share itself
	// (ShareFitPods, a donor set) -- so no other group lands in that domain.
	// Per key, as NodeView.domains: a claim on "zone" refuses no "rack".
	Claimed []string
}

// SharePod is one donor pod: its name (namespace/name), node and GPUs.
type SharePod struct {
	Name, Node string
	GPUs       int
}

// ShareUnit is what one donor replica frees when it goes: its pods.
type ShareUnit struct {
	Pods []SharePod
	// Group names a LeaderWorkerSet group as ShareNode.Groups does; "" for
	// a unit that is no group. Once it goes, its pods' nodes hold no pod of
	// it.
	Group string
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
	// SwingNeeds is, for each role in Swinging, the need it was planned on
	// (its mean over the swing window) and the need it read this cycle, in
	// GPUs: what the swing rule changed, for the log line that names it.
	SwingNeeds map[string]ShareSwingNeed
	// ReserveDebt is how far the committed allocation stood above the budget
	// before this cycle's refills: reserve a wake spent, or anything else that
	// put the group over (section 6.2). Refills is how many transfers started
	// to pay it back.
	ReserveDebt int
	Refills     int
	// Returns is how many transfers started this cycle to give a role's
	// replicas above its target back to the unassigned GPUs (returnSpare).
	Returns int
	// HeadroomCapped is set when the spare was capped this cycle: the quota
	// could not cover every role at its recent peak (contended).
	HeadroomCapped bool
	// PeakClaimGPUs is what contention was judged on: the sum of every role's
	// claim at its peak need over the swing window, in whole replicas. Zero
	// when the cap is off by configuration.
	PeakClaimGPUs int
	// MeanCapped are the roles whose headroom was capped on their mean need,
	// below their reading, as fast swingers beside a long burster
	// (sharePlanRun.fastSwingers).
	MeanCapped []string
	// UnknownPeaks are the roles counted at their ceiling in PeakClaimGPUs
	// because no role's peak is known yet (peakClaims).
	UnknownPeaks []string
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

// ShareSwingNeed is a swinging role's need two ways, in GPUs: Mean, its mean
// over the swing window, which it is planned on, and Current, its reading.
type ShareSwingNeed struct {
	Mean, Current float64
}

// Reasons in SharePlan.Unfunded and SharePlan.Withheld.
const (
	ShareUnfundedNoCompatibleDonor = "no-compatible-donor"
	// ShareUnfundedAwaitingReturn marks a receiver whose replica spans
	// several pods credited with returns still releasing (returnsCover).
	ShareUnfundedAwaitingReturn = "awaiting-return"
	// ShareUnfundedNoNodeFits marks a receiver no node can hold a replica of
	// (ShareNoNodeFits). The plan does not set it: the caller does, from the
	// pods the group's roles run, which the plan does not see.
	ShareUnfundedNoNodeFits    = "no-node-fits"
	ShareWithheldReversalHold  = "reversal-hold"
	ShareWithheldNotActionable = "not-actionable"
	// ShareWithheldReturnNotCalm counts a role above its target that keeps
	// its spare because it would not stay calm one replica lower.
	ShareWithheldReturnNotCalm = "return-not-calm"
	// ShareWithheldReturnRoleShort counts a role above its target that keeps
	// its spare because another role is short of its need (returnSpare).
	ShareWithheldReturnRoleShort = "return-role-short"
	// ShareWithheldDonorPeak counts a headroom move not made because it
	// would leave its donor below its own recent peak while the quota holds
	// every peak (fundFrom).
	ShareWithheldDonorPeak = "donor-peak"
	// ShareWithheldAwaitingReturn counts a receiver whose replica spans
	// several pods left to the idle fill, not a donor set, because returns
	// still releasing will free at least one replica of it (returnsCover).
	ShareWithheldAwaitingReturn = "awaiting-return"
	// ShareWithheldScaleDownBand counts a return not made because it would
	// take its role into its scale-down band (sharePlanRun.bandKeep).
	ShareWithheldScaleDownBand = "scale-down-band"
	// ShareWithheldReturnPodBusy counts a role above its target whose fleet
	// load would let a return go, kept because one of its pods has held a
	// waiting queue above the return load of the saturation queue for the
	// confirm cycles in a row -- for at most its scale-down window
	// (returnSpare).
	ShareWithheldReturnPodBusy = "return-pod-busy"
	// ShareWithheldReturnUnplaceable counts a role above its target that
	// keeps its spare because every role below its own target is one no node
	// can hold a replica of (returnSpare): returned, the GPUs would sit
	// unassigned.
	ShareWithheldReturnUnplaceable = "return-receiver-unplaceable"
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
	var peakSum int
	p.peaks, peakSum = p.peakClaims()
	if in.Headroom.Capped {
		p.plan.PeakClaimGPUs = peakSum
		if peakSum > in.Budget {
			p.capHeadroom(peakSum)
		}
	}
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
	p.hard = l.ConfirmHard(p.hardReceivers())

	p.refillReserve()
	p.idleNodes = maps.Clone(in.Nodes)
	p.work = maps.Clone(p.committed)
	if !slices.ContainsFunc(slices.Collect(maps.Keys(p.confirmed)), p.isConfirmed) && !p.hardImbalance() {
		p.returnSpare() // its own clock: a role above its target is not actionable when nearly idle
		return p.plan
	}

	var receivers, donors []string
	for _, r := range p.roles {
		c := p.committed[r.Key]
		// A role still giving is not a receiver, as the fill does not raise
		// it (ShareLedger.Giving): a target that crosses a replica boundary
		// would otherwise fill it while its own release drains, and a move
		// that skips the waits would send GPUs back to it mid-drain when an
		// alternation is shorter than a release.
		if c < p.integ[r.Key] && !p.l.Giving(r.Key) {
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

	for _, rc := range receivers {
		p.fund(rc, donors)
	}
	p.returnSpare()
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
	// hard is each role's consecutive cycles as a hard-imbalance receiver
	// (ShareLedger.ConfirmHard).
	hard map[string]int
	// keep is the least each role may hold after giving (shareKeep).
	keep map[string]int
	// idleNodes is the node picture the idle fill's credit (fromIdle) has
	// spent this cycle; nil without node information.
	idleNodes map[string]ShareNode
	// peaks is each role's claim at its peak need (peakClaims).
	peaks map[string]int
	// creditedReturns are the returns this cycle already credited to a
	// receiver whose replica spans several pods (returnsCover).
	creditedReturns map[string]bool
}

// shareReplicaPods is the GPUs of each pod of one replica of v: its pods'
// when known, else one pod of the whole replica.
func shareReplicaPods(v ShareVariant) []int {
	if len(v.PodGPUs) > 0 {
		return slices.Clone(v.PodGPUs)
	}
	return []int{max(v.GPUs, 1)}
}

// variantOf is the variant role k gives from or grows in m, or, when m is
// nil, one replica of gpus GPUs. A role absent from a non-nil m has none.
func variantOf(m map[string]ShareVariant, k string, gpus int) (ShareVariant, bool) {
	if m == nil {
		return ShareVariant{GPUs: gpus}, true
	}
	v, ok := m[k]
	return v, ok
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

// shareHardImbalance reports whether a move to receiver rc, holding rcHeld,
// from donor dn, left with dnLeft after giving, rebalances a hard imbalance:
// rc's load is at or near its scale-up threshold (ShareSkipWaitsReceiverLoad)
// while dn's stays well below its own (ShareSkipWaitsDonorLoad). Load is what
// the need serves over what the GPUs held serve (ShareRole.serves):
// utilization as a fraction of the scale-up threshold. Such a move does not wait: the
// reversal hold and the confirm cycles exist to stop GPUs ping-ponging for
// headroom, and must not keep a model at its threshold while another idles.
// Measured on alternating bursts, the hold kept a rising model at 3 of 8
// replicas for its whole 18-minute burst while the other, idle, held 7. A
// cancelled pair's hold, back-offs and an exhausted donor still apply
// (PairHeld, GivingBlocked), as do §6.2 admission and the donor's band.
func shareHardImbalance(b ShareSkipWaits, rcHeld int, rc ShareRole, dnLeft int, dn ShareRole) bool {
	return rc.Need > 0 && b.Urgent(rc, rcHeld) && b.Calm(dn, dnLeft)
}

// ShareSkipWaits is the band shareHardImbalance applies
// (config.UtilizationShareSkipWaits). The zero value is the default.
type ShareSkipWaits struct {
	// Off makes every move wait for the holds and the confirm cycles.
	Off bool
	// ReceiverLoad is the receiver's load -- need over the GPUs it holds --
	// at or above which it is at its threshold. Zero takes
	// ShareSkipWaitsReceiverLoad.
	ReceiverLoad float64
	// DonorLoad is the donor's load on the GPUs it keeps, at or below which
	// it is calm. Zero takes ShareSkipWaitsDonorLoad.
	DonorLoad float64
}

// band returns the receiver and donor loads in force.
func (b ShareSkipWaits) band() (urgent, calm float64) {
	urgent, calm = b.ReceiverLoad, b.DonorLoad
	if urgent == 0 {
		urgent = ShareSkipWaitsReceiverLoad
	}
	if calm == 0 {
		calm = ShareSkipWaitsDonorLoad
	}
	return urgent, calm
}

// Urgent reports whether role r, holding held, is at or near its scale-up
// threshold under the band. Its load is what its need serves over what held
// serves (ShareRole.serves).
func (b ShareSkipWaits) Urgent(r ShareRole, held int) bool {
	urgent, _ := b.band()
	return !b.Off && r.serves(r.Need) >= urgent*r.serves(float64(held))
}

// Calm reports whether donor r, keeping kept, is well below its threshold
// under the band.
func (b ShareSkipWaits) Calm(r ShareRole, kept int) bool {
	_, calm := b.band()
	return !b.Off && r.serves(r.Need) <= calm*r.serves(float64(kept))
}

// shareMoveHeld reports whether donor dn may not give to receiver rc in a
// move the reversal exemption covers (exempt) or not: the reversal hold of
// either role unless exempt, and the pair's cancel hold, a back-off or an
// exhausted donor always.
func shareMoveHeld(l *ShareLedger, dn, rc string, exempt bool, now time.Time, tm ShareTimings) bool {
	if l.PairHeld(dn, rc, now) || l.GivingBlocked(dn, now) {
		return true
	}
	return !exempt && (l.ReceivingHeld(rc, now, tm) || l.GivingHeld(dn, now, tm))
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
	return p.byKey[k].Score(float64(g))
}

// need is a role's need for judging whether it is calm: the higher of this
// cycle's reading and the mean a swinging role is planned on (rule 5). A
// swinging role whose demand is rising reads lower on its mean, and judged
// calm on it, it would give or return GPUs it is about to need.
func (p *sharePlanRun) need(k string) float64 {
	return math.Max(p.byKey[k].Need, p.rawNeed(k))
}

// rawNeed is a role's need as this cycle read it, before the swing rule: what
// the idle fill, which never sees the swing rule, sizes its pace by.
func (p *sharePlanRun) rawNeed(k string) float64 {
	for _, r := range p.in.Roles {
		if r.Key == k {
			return r.Need
		}
	}
	return 0
}

// isConfirmed reports whether a role has been actionable long enough to move.
func (p *sharePlanRun) isConfirmed(k string) bool {
	return p.confirmed[k] >= p.confirmCycles()
}

// confirmCycles is how many consecutive cycles a condition must hold before
// it moves anything (SharePlanInput.ConfirmCycles, else ShareConfirmCycles).
func (p *sharePlanRun) confirmCycles() int {
	if p.in.ConfirmCycles > 0 {
		return p.in.ConfirmCycles
	}
	return ShareConfirmCycles
}

// hardImbalance reports whether an actionable role and another role could
// make a hard imbalance (shareHardImbalance) with one replica moved: the
// move fund then makes without waiting for the confirm cycles.
func (p *sharePlanRun) hardImbalance() bool {
	return len(p.hardReceivers()) > 0
}

// hardReceivers are the actionable roles that make a hard imbalance with
// another role (hardImbalance).
func (p *sharePlanRun) hardReceivers() map[string]bool {
	out := map[string]bool{}
	for _, rc := range p.roles {
		if !p.actionable[rc.Key] {
			continue
		}
		for _, dn := range p.roles {
			left := p.committed[dn.Key] - max(dn.ReplicaGPUs, 1)
			if dn.Key != rc.Key && left >= 0 && shareHardImbalance(p.in.SkipWaits, p.committed[rc.Key], p.needView(rc), left, p.donorViewFor(dn, rc.Key)) {
				out[rc.Key] = true
				break
			}
		}
	}
	return out
}

// donorView is donor role r judged on the higher of its need and its peak
// over its scale-down window, as a scale-down is: judged on one reading, a
// calm donor read in a trough of its normal demand is drained in two cycles.
func (p *sharePlanRun) donorView(r ShareRole) ShareRole {
	r.Need = math.Max(p.need(r.Key), r.gpusFor(p.l.RecentPeak(r.Key, p.now, p.tm.Window)))
	return r
}

// donorPeak is what donor dn keeps under the donor-peak rule: its claim at
// its peak need over its scale-down window, in whole replicas, as donorView
// judges a donor. Not its peak over the swing window (peakClaims) for a
// donor that burst once: one whose load fell would keep that peak for the
// whole window, about two hours by default, and a model at its need beside
// it waited that long for its headroom replica, as if the load had swung
// back. A donor that has burst at least twice within the window has a peak
// that comes back, and keeps it (peakClaims): judged on its last scale-down
// window, a donor peaking every 15 to 90 minutes gave its spare away between
// peaks and waited a release at each one. A dip shorter than the scale-down
// window does not split a burst in two (needBurstsJoined): judged on its
// recent peak, the donor never left that peak during it, and counting it
// held a donor whose one busy hour had a one-minute dip at its old peak for
// most of a swing window, as before the rule.
func (p *sharePlanRun) donorPeak(dn string) int {
	if p.l.needBurstsJoined(dn, p.tm.Window).completed >= 2 {
		return p.peaks[dn]
	}
	r := p.byKey[dn]
	g := max(r.ReplicaGPUs, 1)
	r.Need = r.gpusFor(p.l.RecentPeak(dn, p.now, p.tm.Window))
	return g * int(math.Ceil(r.Claim()/float64(g)-1e-9))
}

// donorViewFor is donor r judged for receiver rc: on its recent peak
// (donorView) unless rc's bursts outlast a move (ShareLedger.LongBursts).
// Then a move lands inside the burst it was made for, and holding it back a
// scale-down window at every handover only made the burst wait: whole-node
// bursts of 40 minutes lost 1 to 2% more of their demand.
func (p *sharePlanRun) donorViewFor(r ShareRole, rc string) ShareRole {
	if p.l.LongBursts(rc, p.tm.moveLatency()) {
		return p.needView(r)
	}
	return p.donorView(r)
}

// needView is role r judged on its need (sharePlanRun.need): a receiver's
// urgency and the headroom cap.
// A receiver's urgency is judged on it too: on its mean, a swinging role at
// the crest of a burst would never be a hard imbalance.
func (p *sharePlanRun) needView(r ShareRole) ShareRole {
	r.Need = p.need(r.Key)
	return r
}

// bandKeep is the least role dn, judged as v, keeps when it returns GPUs to
// the quota: the GPUs at which its demand sits where today's scale-down
// leaves it, a quarter of the way up from its scale-down boundary to its
// scale-up threshold, in whole replicas and no more than its own ceiling --
// not the one the headroom cap sets, which is the spare a return gives back.
// A return is today's scale-down, done while nobody waits for the GPUs, and
// goes no lower than it would. Transfers do not keep it: a receiver at its need whose demand is
// still rising needs the next replica within a move's latency, and a P/D
// role held back from it lost up to 0.055 more of its demand in the
// whole-node simulator. Zero, no band, without a boundary below the
// scale-up threshold.
func (p *sharePlanRun) bandKeep(dn string, v ShareRole) int {
	up, down := p.in.Thresholds[dn], p.in.DownThresholds[dn]
	if !(down > 0) || !(up > down) || !(v.Need > 0) {
		return 0
	}
	// Where today's scale-down leaves a role: a quarter of the way up its
	// band (releaseTargetUtilization), not at the boundary. Kept at the
	// boundary, a P/D role of 8-GPU replicas at a need of 20 kept 4 replicas
	// where today's scale-down keeps 3.
	target := down + (up-down)/4
	g := max(v.ReplicaGPUs, 1)
	keep := g * int(math.Ceil(v.gpusFor(v.serves(v.Need)*up/target)/float64(g)-1e-9))
	for _, r := range p.in.Roles {
		if r.Key == dn && r.Ceiling > 0 {
			keep = min(keep, r.Ceiling)
		}
	}
	return keep
}

// capHeadroom caps each role's headroom (ShareCapSpare,
// maxHeadroomReplicas by weight) because the quota is contended: peakSum,
// the sum of every role's claim at its peak need over the swing window
// (peakClaims), is more than the budget. Only then is the spare capped. While
// it is not, no burst
// can find the GPUs it needs parked on another, and returning headroom only
// makes the next burst cold-start what a spread spare held warm: in
// simulation, alternating bursts whose peaks fit together lost 4 to 8% of
// demand capped against under 1% spread, and whole-node bursts in a slack
// quota lost 6 to 43% against none. When the peaks do not fit together, a
// role that bursts must take GPUs from the others as it bursts, and spare
// parked on them -- whose pods may take minutes to drain -- is what made a
// bursting model wait 18 minutes for 8 replicas on a cluster.
//
// A role is capped on the higher of its reading and its planning need
// (needView): on the reading, a swinging role in its trough would be held
// below the mean it is planned on; on the mean, one at its crest would be
// held below its reading, beside GPUs only it could use. It is never capped
// below what the other roles' peaks leave of the budget
// (shareRoomForOthers): GPUs no other role's recent peak could use would sit
// unassigned until its own next burst cold-started them again.
func (p *sharePlanRun) capHeadroom(peakSum int) {
	p.plan.HeadroomCapped = true
	views := make([]ShareRole, len(p.roles))
	fast := p.fastSwingers()
	for i, r := range p.roles {
		views[i] = p.needView(r)
		if fast[r.Key] {
			mean := r.gpusFor(p.l.MeanNeed(r.Key, r.serves(r.Need)))
			onMean := views[i]
			onMean.Need = mean
			// Reported only where the mean lowers the cap the role ends up
			// with: in whole replicas it often does not, an 8-GPU role above
			// all, and the room the others' peaks leave can lift both.
			if c := ShareCapSpare([]ShareRole{views[i], onMean}, p.in.Headroom.Replicas); p.capWithRoom(c[1], peakSum) < p.capWithRoom(c[0], peakSum) {
				p.plan.MeanCapped = append(p.plan.MeanCapped, r.Key)
			}
			views[i] = onMean
		}
	}
	for _, c := range ShareCapSpare(views, p.in.Headroom.Replicas) {
		r := p.byKey[c.Key]
		r.Ceiling = p.capWithRoom(c, peakSum)
		p.byKey[c.Key] = r
	}
	for i := range p.roles {
		p.roles[i] = p.byKey[p.roles[i].Key]
	}
}

// capWithRoom is the ceiling capHeadroom gives role c, capped by
// ShareCapSpare: no lower than what the other roles' peaks leave of the budget
// (shareRoomForOthers). A role without a ceiling keeps its own.
func (p *sharePlanRun) capWithRoom(c ShareRole, peakSum int) int {
	r := p.byKey[c.Key]
	if c.Ceiling <= 0 {
		return r.Ceiling
	}
	return max(c.Ceiling, shareRoomForOthers(p.in.Budget, peakSum-p.peaks[c.Key], max(r.ReplicaGPUs, 1), r.Ceiling))
}

// fastSwingers are the roles capped on their mean need (capHeadroom): those
// whose need swings faster than a move can follow (ShareLedger.SwingsFast),
// while another role bursts for longer than a move (ShareLedger.BurstsLong)
// and its peak needs more than its floor holds -- a burster its floor covers
// waits on no release, and capping others for it only costs them.
// Capped on its reading, a fast swinger takes idle GPUs at each crest; they
// start serving as the crest ends, and parked on it -- a donor that is never
// calm, often with long requests to drain -- they are what the long burst
// then waits a release for. In simulation, a model bursting for 20 minutes
// an hour beside one swinging every few minutes left 0.33 to 0.35 of its
// demand unserved, against 0.20 to 0.21 capped on the swinger's mean, which
// cost the swinger 0.02 to 0.04. Without a long burster nothing waits on the
// swinger's GPUs, so it is capped on its reading: a model re-bursting every
// 12 minutes beside a steady one lost up to 0.03 more capped on its mean.
func (p *sharePlanRun) fastSwingers() map[string]bool {
	lat := p.tm.moveLatency()
	if !slices.ContainsFunc(p.roles, func(r ShareRole) bool { return p.peaks[r.Key] > r.Floor && p.l.BurstsLong(r.Key, lat) }) {
		return nil
	}
	out := map[string]bool{}
	for _, r := range p.roles {
		if p.l.SwingsFast(r.Key, lat) {
			out[r.Key] = true
		}
	}
	return out
}

// shareRoomForOthers is what the others' peak claims leave of budget, in
// whole replicas of g GPUs, and no more than the role's own ceiling (0: none).
func shareRoomForOthers(budget, others, g, ceiling int) int {
	room := max(budget-others, 0) / g * g
	if ceiling > 0 {
		room = min(room, ceiling)
	}
	return room
}

// peakClaims is each role's claim at its peak need over the swing window,
// rounded up to whole replicas, and their sum. While no role's peak is known
// (ShareLedger.PeakKnown) -- a new group, or a restart with no stored history
// -- each is taken to peak at its ceiling, or the whole budget when it has
// none: judged on the history it has, the cap stayed off until a model had
// burst inside the window, and the first burst found the spare parked on
// another model. A role joining a group whose history is known claims what
// it holds instead (peakClaims' established case).
func (p *sharePlanRun) peakClaims() (map[string]int, int) {
	out := make(map[string]int, len(p.roles))
	sum := 0
	established := p.l.AnyPeakKnown(p.now, p.tm)
	for _, r := range p.roles {
		g := max(r.ReplicaGPUs, 1)
		peak := r
		peak.Need = r.gpusFor(p.l.PeakNeed(r.Key, r.serves(r.Need)))
		switch {
		case p.l.PeakKnown(r.Key, p.now, p.tm):
		case established:
			// A role joining a group whose history is known claims what it
			// holds: at its ceiling, any new model -- or one renamed to look
			// new -- would cap every other model's headroom for a window.
			peak.Need = math.Max(peak.Need, float64(p.in.Held[r.Key]))
		default:
			peak.Need = math.Max(peak.Need, math.Min(r.ceiling(), float64(p.in.Budget)))
			p.plan.UnknownPeaks = append(p.plan.UnknownPeaks, r.Key)
		}
		peak.Need = math.Min(peak.Need, float64(p.in.Budget))
		out[r.Key] = g * int(math.Ceil(peak.Claim()/float64(g)-1e-9))
		sum += out[r.Key]
	}
	return out, sum
}

// unpaced reports whether a hard-imbalance move to rc may skip the pace and
// the concurrency limit: rc has been a hard-imbalance receiver for
// ShareUnpacedAfterCycles consecutive cycles, and its burst is not expected
// to end before a move lands (ShareLedger.Unpaced). The first cycle moves at
// the pace, so a single reading -- a scrape glitch, a forged metric -- moves
// at most ShareMaxReplicasPerCycle replicas, which the next cycle cancels
// while they are free; the cycle after takes the rest of the shortfall.
func (p *sharePlanRun) unpaced(rc string) bool {
	return p.l.Unpaced(rc, ShareModelRoles(p.in.Origins, rc), p.tm)
}

// planningRoles records this cycle's needs and plans a swinging role on its
// mean need (rule 5). Needs are recorded in what they serve (ShareRole.serves)
// and read back on the role's curve of the cycle reading them: in GPUs, the
// same demand reads fewer once a dense replica serves part of it, and a
// history mixing curves would overstate a role that just gained one, and
// average a swinging role's readings on different curves.
func (p *sharePlanRun) planningRoles() {
	raw := make(map[string]float64, len(p.in.Roles))
	for _, r := range p.in.Roles {
		raw[r.Key] = r.serves(r.Need)
	}
	p.l.RecordNeeds(raw, p.now, p.tm)
	p.roles = slices.Clone(p.in.Roles)
	p.byKey = make(map[string]ShareRole, len(p.roles))
	for i := range p.roles {
		if r := p.roles[i]; p.l.Swinging(r.Key, p.now) {
			p.roles[i].Need = r.gpusFor(p.l.PlanningNeed(r.Key, r.serves(r.Need), p.now))
			p.plan.Swinging = append(p.plan.Swinging, p.roles[i].Key)
			if p.plan.SwingNeeds == nil {
				p.plan.SwingNeeds = map[string]ShareSwingNeed{}
			}
			p.plan.SwingNeeds[r.Key] = ShareSwingNeed{Mean: p.roles[i].Need, Current: r.Need}
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
		dn, planned := p.byKey[t.Donor]
		donorAfter := p.committed[t.Donor] // already net of this transfer
		// A reversal is judged against both targets: the continuous one, for
		// its band, and the whole-replica one, which admission started the
		// move against. The continuous split shares the spare by need in
		// GPUs, the whole-replica targets by what the GPUs serve; judged on
		// the first alone, a move the second had just started was cancelled
		// the next cycle, and started again.
		//
		// Twice the band below, or more, not strictly more: the band is at
		// least half a replica, so twice it is a whole one, and a donor one
		// whole replica short wants back exactly the replica it is giving --
		// the move reversed. Strictly more, a one-replica move was never
		// called off on its donor: measured, a busy model that read no demand
		// for two cycles as its load generator was swapped started giving a
		// replica to an idle one, wanted it back 15s later, and lost it six
		// minutes later, when its scale-down window let the pod go.
		//
		// Only a donor planned this cycle is judged: one out of the group --
		// frozen, or not collected -- has no target, and what it holds reads
		// as zero, net of its transfer below it.
		donorShort := planned &&
			float64(donorAfter) <= p.cont[t.Donor]-2*shareTol(p.cont[t.Donor], dn, in.Tolerance)+shareCancelSlack &&
			donorAfter < p.integ[t.Donor]
		// A returned spare is called off, while it is free, when its role's
		// demand came back: the GPUs would otherwise drain from it only for
		// the fill to raise it again. Back means below its target, or busier
		// than the calm load on what it keeps -- one replica above the bar
		// the return was started under, so the two do not alternate.
		g := float64(max(dn.ReplicaGPUs, 1))
		bar := float64(donorAfter) - float64(in.Headroom.spareReplicas(dn))*g + g
		back := donorAfter < p.integ[t.Donor] || !p.returnCalm(dn, bar)
		if t.Returned && back && l.Cancel(t.ID, now, tm) {
			p.plan.Cancelled = append(p.plan.Cancelled, t.ID)
			p.committed = l.Committed(in.Held)
			continue
		}
		// Any other transfer with no receiver is a reserve refill, entitled
		// and not subject to hysteresis, or a set's contributor, which goes
		// only with its primary: cancelling it alone would leave the primary
		// to raise its receiver into a hole that never fully opens.
		if t.Receiver == "" {
			continue
		}
		rc := p.byKey[t.Receiver]
		receiverWithout := p.committed[t.Receiver] - t.GPUs
		receiverFine := float64(receiverWithout) > p.cont[t.Receiver]+shareTol(p.cont[t.Receiver], rc, in.Tolerance) &&
			receiverWithout >= p.integ[t.Receiver]
		// Called off for the donor, the cancel is a turn of the donor's own
		// demand, a change of direction at its second turn inside the swing
		// window (CancelDonorBack); for the receiver, the move it would have
		// made.
		cancel := l.Cancel
		if donorShort {
			cancel = l.CancelDonorBack
		}
		if (donorShort || receiverFine) && cancel(t.ID, now, tm) {
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
		give, ok := variantOf(in.Give, dn, p.byKey[dn].ReplicaGPUs)
		if !ok {
			continue
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
// a donor set when no single replica fits. What the idle fill gives it this
// cycle is counted first, so no donor shrinks for GPUs that are free.
func (p *sharePlanRun) fund(rc string, donors []string) {
	l, in, now := p.l, p.in, p.now
	candidates, misfits, funded := 0, 0, false
	if l.FillHeld(rc, now) {
		return // its last fill timed out with its pods Pending (HoldFill)
	}
	p.fromIdle(rc)
	// Returns still releasing that will free whole replicas of the receiver
	// land in the unassigned GPUs, where the idle fill places its pods: they
	// are credited, and only the rest is funded, which would otherwise drain a
	// donor for GPUs already on their way.
	if n := p.returnsCover(rc); n > 0 {
		p.work[rc] += n
		p.plan.Withheld[ShareWithheldAwaitingReturn]++
		p.plan.Unfunded[rc] = ShareUnfundedAwaitingReturn
	}
	for _, dn := range donors {
		started, step, cand, misfit := p.fundFrom(rc, dn)
		funded = funded || started > 0
		if cand {
			candidates++
			if misfit {
				misfits++
			}
		}
		if step == fundFull {
			break
		}
	}
	if !funded && candidates > 0 && misfits == candidates && in.Grow != nil {
		var deferred bool
		funded, deferred = p.fundBySet(rc, donors)
		if deferred {
			return
		}
		// Unpaced, a hard imbalance takes set after set while it lasts, as
		// the single-donor path takes replica after replica. Bounded by the
		// receiver's target in any case; the bound only stops a loop.
		for range MaxShareBudgetGPUs {
			if !funded || !p.unpacedNow(rc) {
				break
			}
			if funded, _ = p.fundBySet(rc, donors); !funded {
				funded = true // a set started above
				break
			}
		}
	}
	if !funded && p.plan.Unfunded[rc] == "" && p.isConfirmed(rc) && p.actionable[rc] && candidates > 0 && misfits == candidates {
		p.plan.Unfunded[rc] = ShareUnfundedNoCompatibleDonor
	}
}

// unpacedNow reports whether receiver rc's moves skip the pace and the
// concurrency limit this cycle: a hard imbalance confirmed for
// ShareUnpacedAfterCycles (unpaced), and rc still below its need -- the
// replica of headroom above it is an ordinary, paced move.
func (p *sharePlanRun) unpacedNow(rc string) bool {
	return p.unpaced(rc) && p.work[rc] < p.integ[rc] && float64(p.work[rc]) < p.need(rc)
}

// fromIdle counts what the idle fill gives receiver rc this cycle -- the
// group's unassigned GPUs, raised without a transfer or a wait, at the fill's
// pace (ShareFillPace) -- into the run's allocation, so donors fund only the
// rest. With node information only where rc's pods fit free GPUs, in its
// domain, as the fill places them. Not a receiver the last fill left short
// (FillShort): whatever kept it short, the fill sees and the planner does not.
func (p *sharePlanRun) fromIdle(rc string) {
	if p.l.FillShort(rc) || p.work[rc] >= p.integ[rc] {
		return
	}
	grow, ok := variantOf(p.in.Grow, rc, p.byKey[rc].ReplicaGPUs)
	if !ok {
		return
	}
	g := max(grow.GPUs, 1)
	pace := ShareFillPace(p.rawNeed(rc), p.work[rc])
	for n := 0; n < pace && shareIdle(p.l, p.in, p.work) >= g && p.work[rc] < p.integ[rc]; n++ {
		if p.idleNodes != nil && len(grow.PodGPUs) > 0 && !ShareFitPods(p.idleNodes, p.in.receiverFit(rc, grow)) {
			return
		}
		p.work[rc] += g
	}
}

// returnsCover is the GPUs, in whole replicas of receiver rc, that returns
// still releasing will free for it, up to its target. A return is never
// handed to a receiver: it reaches rc through the idle fill once released.
// Only returned pods at least as large as rc's smallest pod count -- its
// whole replica, when its pods are not known: two 4-GPU pods do not hold an
// 8-GPU one, and a return whose shape is not known counts for nothing. Each
// return is credited to one receiver per cycle.
func (p *sharePlanRun) returnsCover(rc string) int {
	grow, ok := variantOf(p.in.Grow, rc, p.byKey[rc].ReplicaGPUs)
	if !ok {
		return 0
	}
	g := max(grow.GPUs, 1)
	smallest := g
	if len(grow.PodGPUs) > 0 {
		smallest = slices.Min(grow.PodGPUs)
	}
	want := p.integ[rc] - p.work[rc]
	var ids []string
	gpus := 0
	for _, t := range p.l.Transfers() {
		if !t.Returned || t.State != ShareReleasing || t.Donor == rc || p.creditedReturns[t.ID] {
			continue
		}
		fits := 0
		for _, pod := range t.DonorPodGPUs {
			if pod >= smallest {
				fits += pod
			}
		}
		if fits == 0 {
			continue
		}
		ids = append(ids, t.ID)
		if gpus += fits; gpus/g*g >= want {
			break
		}
	}
	covered := min(gpus/g*g, max(want, 0)/g*g)
	if covered == 0 {
		return 0
	}
	if p.creditedReturns == nil {
		p.creditedReturns = map[string]bool{}
	}
	for _, id := range ids {
		p.creditedReturns[id] = true
	}
	return covered
}

// fundStep is how fundFrom left a donor.
type fundStep int

const (
	// fundNext: try the next donor.
	fundNext fundStep = iota
	// fundFull: the receiver is at its whole-replica target.
	fundFull
)

// fundFrom moves replicas from donor dn to receiver rc: one, or -- while the
// move is unpaced (unpacedNow) -- as many as rc is short, until dn is no
// longer calm or reaches its target or keep. It returns how many it started,
// whether rc is full, and whether dn is a candidate (it has a variant to give
// and rc one to grow), and a misfit (its pods cannot host rc's).
//
// An unpaced move skips the per-cycle replica limit and the concurrency
// limit (proposal §6.7). Every other bound -- the targets, the keep, the band,
// the holds a skip does not skip -- still applies, and the ratio between the
// two loads still stops the reverse move.
func (p *sharePlanRun) fundFrom(rc, dn string) (started int, step fundStep, candidate, misfit bool) {
	l, in, now, tm := p.l, p.in, p.now, p.tm
	work, moved, byKey := p.work, p.moved, p.byKey
	rr, dr := byKey[rc], byKey[dn]
	grow, okGrow := variantOf(in.Grow, rc, rr.ReplicaGPUs)
	give, okGive := variantOf(in.Give, dn, dr.ReplicaGPUs)
	for {
		// A receiver funded up to its whole-replica target takes no more, and a
		// donor down to its own gives no more: past either, the next plan would
		// only move the replica back.
		if work[rc] >= p.integ[rc] {
			return started, fundFull, candidate, misfit
		}
		if work[dn] <= p.integ[dn] || !okGrow || !okGive {
			return started, fundNext, candidate, misfit
		}
		g, gd := max(grow.GPUs, 1), max(give.GPUs, 1)
		// Rule 4: a role that gave cannot receive, and one that received
		// cannot give, within the hold -- unless the move rebalances a hard
		// imbalance, which waits for neither the hold nor the confirm cycles.
		exempt := shareHardImbalance(in.SkipWaits, work[rc], p.needView(rr), work[dn]-gd, p.donorViewFor(dr, rc))
		free := exempt && p.unpacedNow(rc)
		if !free && (started > 0 || l.InFlight() >= ShareMaxConcurrentTransfers ||
			moved[rc] >= ShareMaxReplicasPerCycle || moved[dn] >= ShareMaxReplicasPerCycle) {
			return started, fundNext, candidate, misfit
		}
		candidate = true
		if !ShareCovers(give, grow) {
			return started, fundNext, candidate, true // the donor's pods cannot host the receiver's (§6.5)
		}
		if l.GivingBusy(dn, now) {
			return started, fundNext, candidate, misfit // it has given all it can until its releases land
		}
		if shareMoveHeld(l, dn, rc, exempt, now, tm) {
			p.plan.Withheld[ShareWithheldReversalHold]++
			return started, fundNext, candidate, misfit
		}
		if !exempt && !p.isConfirmed(rc) && !p.isConfirmed(dn) {
			return started, fundNext, candidate, misfit // every other transfer must fix a confirmed actionable role
		}
		// Spread, a receiver already at its need takes no headroom that would
		// leave its donor below the donor's own recent peak: the split covers
		// both, and the move -- from a donor that may drain for minutes --
		// lands after the receiver's burst and just before the donor's own.
		// The peak is the donor's recent one (donorPeak), not its peak over
		// the swing window.
		if !p.plan.HeadroomCapped && float64(work[rc]) >= p.need(rc) && work[dn]-gd < p.donorPeak(dn) {
			p.plan.Withheld[ShareWithheldDonorPeak]++
			return started, fundNext, candidate, misfit
		}
		// §6.2 admission: the worst-off role of the pair improves, and the
		// donor keeps at least its whole-replica target and its keep -- what
		// it keeps after giving, which a donor replica larger than its
		// target's step can take below the target.
		before := math.Min(p.z(dn, work[dn]), p.z(rc, work[rc]))
		after := math.Min(p.z(dn, work[dn]-gd), p.z(rc, work[rc]+g))
		if !(after > before) || work[dn]-gd < p.integ[dn] || work[dn]-gd < p.keep[dn] {
			return started, fundNext, candidate, misfit
		}
		t := l.Start(ShareTransfer{
			Donor: dn, Receiver: rc, GPUs: g, DonorGPUs: gd,
			DonorVariant: give.Name, ReceiverVariant: grow.Name,
			Urgent: float64(work[rc]) < rr.Need, SkippedWaits: exempt,
		}, in.Held, now, tm)
		p.plan.Started = append(p.plan.Started, t)
		work[dn] -= gd
		work[rc] += g
		moved[dn]++
		moved[rc]++
		started++
	}
}

// returnSpare gives what a role holds above its whole-replica target back to
// the group's unassigned GPUs, by donor-only transfers (Returned; proposal
// §6.2), where the idle fill raises whoever needs it next without a drain to
// wait for. It is today's scale-down, done while nobody waits for the GPUs:
//   - only after the role has held more than its target for the confirm
//     cycles; its replica then drains after the scale-down window, and a
//     burst that comes back within it calls the return off (cancelReversals)
//     and finds its replica warm;
//   - only while no role that can grow is short of its claim, rounded up to
//     whole replicas -- with one short, the GPUs go to it by a transfer,
//     which the holds govern; a role short only of its headroom, or held
//     from filling, does not block;
//   - not while every role below its target is one no node can hold a
//     replica of, even with the returning roles' pods gone (unplaceable):
//     the GPUs would sit unassigned, and the role would lose its headroom
//     for nothing;
//   - never below its target, floor or keep, and not while held from giving;
//   - and only while it stays calm one replica lower: what its need serves at
//     most the return load of what it would keep less its headroom serves
//     (returnCalm). The fill raises a role once its need passes what it holds
//     less its headroom, so a returned replica is filled again only once
//     need has grown by the inverse of the return load.
//
// What is returned goes in one cycle, as a scale-down would.
func (p *sharePlanRun) returnSpare() {
	l, in, now, tm := p.l, p.in, p.now, p.tm
	over := map[string]bool{}
	for _, r := range p.roles {
		// Only a capped spare is returned: spread, the targets give it all
		// away by weight, and what moves between roles moves by transfer.
		// And only above its target on what it holds, not on what the idle
		// fill or returns still releasing raise it by this cycle (fromIdle,
		// returnsCover): a replica of its grow variant can take it past its
		// target, and it would give back what it holds for GPUs it does not
		// have yet -- a role holding 5 of a target of 8, credited an 8-GPU
		// replica, returned a 2-GPU one, and the return was called off the
		// next cycle, every cycle.
		over[r.Key] = p.plan.HeadroomCapped && p.work[r.Key] > p.integ[r.Key] && p.committed[r.Key] > p.integ[r.Key]
	}
	cycles := l.MarkOver(over)
	load := in.Headroom.returnLoad()
	busy := map[string]bool{}
	for _, r := range p.roles {
		busy[r.Key] = in.BusiestQueueLoad[r.Key] > load
	}
	// Recorded every cycle, before anything below can end it early: a
	// cycle not read is a cycle the run of busy readings did not break.
	podBusy := l.NotePodBusy(busy, now)
	for _, r := range p.roles {
		g := max(r.ReplicaGPUs, 1)
		claim := g * int(math.Ceil(r.Claim()/float64(g)-1e-9))
		if p.work[r.Key] < claim && !l.FillHeld(r.Key, now) && p.plan.Unfunded[r.Key] == "" {
			if _, grows := variantOf(in.Grow, r.Key, g); grows {
				for k, o := range over {
					if o && k != r.Key {
						p.plan.Withheld[ShareWithheldReturnRoleShort]++
					}
				}
				return
			}
		}
	}
	if p.onlyUnplaceableShort(over) {
		for _, o := range over {
			if o {
				p.plan.Withheld[ShareWithheldReturnUnplaceable]++
			}
		}
		return
	}
	for _, r := range p.roles {
		dn := r.Key
		// It waits the confirm cycles, as any move does, not its scale-down
		// window: the release that follows waits that window again, and
		// is called off while it does if the role's demand comes back
		// (cancelReversals). Waiting both, a return took two windows where
		// a scale-down takes one, and a burst on another model found the
		// spare still parked.
		if !over[dn] || cycles[dn] < p.confirmCycles() || l.GivingHeld(dn, now, tm) {
			continue
		}
		give, ok := variantOf(in.Give, dn, r.ReplicaGPUs)
		if !ok {
			continue
		}
		gd := max(give.GPUs, 1)
		spare := float64(in.Headroom.spareReplicas(r) * max(r.ReplicaGPUs, 1))
		floor := max(p.integ[dn], r.Floor, p.keep[dn])
		// A return is today's scale-down, and goes no lower than it would.
		band := p.bandKeep(dn, p.needView(r))
		// Calm is judged on the busiest pod as well as the fleet: the fleet
		// read calm on a cluster run while one pod held 235 queued requests
		// and its seven siblings sat idle, and a give-back started (and was
		// cancelled) during it. A replica given back then is one that queue
		// could have been routed to. The queue is the load balancer's
		// routing, though, and a pod queued for good would park the spare
		// for good, so the hold needs the busy reading on the confirm
		// cycles in a row and lasts at most the donor's scale-down window,
		// after which the return goes. Only a return the fleet's load would
		// let go is held, and counted, here; any other keeps its own reason.
		fleetCalm := p.work[dn]-gd >= max(floor, band) && p.returnCalm(r, float64(p.work[dn]-gd)-spare)
		if b, ok := podBusy[dn]; ok && fleetCalm && b.cycles >= p.confirmCycles() && now.Sub(b.since) < tm.Window {
			p.plan.Withheld[ShareWithheldReturnPodBusy]++
			continue
		}
		returned := 0
		for p.work[dn]-gd >= max(floor, band) && p.returnCalm(r, float64(p.work[dn]-gd)-spare) {
			t := l.Start(ShareTransfer{Donor: dn, DonorGPUs: gd, DonorVariant: give.Name, Entitled: true, Returned: true,
				DonorPodGPUs: shareReplicaPods(give)}, in.Held, now, tm)
			p.plan.Started = append(p.plan.Started, t)
			p.plan.Returns++
			p.moved[dn]++
			p.work[dn] -= gd
			returned++
		}
		switch {
		case returned > 0 || p.work[dn]-gd < floor:
		case p.work[dn]-gd < band && p.returnCalm(r, float64(p.work[dn]-gd)-spare):
			// The band withheld it only if the load would have let it go.
			p.plan.Withheld[ShareWithheldScaleDownBand]++
		default:
			p.plan.Withheld[ShareWithheldReturnNotCalm]++
		}
	}
}

// returnCalm reports whether role r is calm on g GPUs at the return load:
// what its need serves is at most the return load of what g serves
// (ShareRole.serves). Read as need over GPUs, a role whose sparse replicas
// sit at the end of its curve read calm while busier: 1.639 needed on 4 GPUs
// of a curve of 2 dense and 12 sparse is 0.41 in GPUs and 0.625 of what they
// serve.
func (p *sharePlanRun) returnCalm(r ShareRole, g float64) bool {
	return r.serves(p.need(r.Key)) <= p.in.Headroom.returnLoad()*r.serves(g)
}

// onlyUnplaceableShort reports whether there are roles below their
// whole-replica target that can grow and are not held from filling -- the
// roles the GPUs a return frees would go to -- and every one of them is
// unplaceable, even with the returns of the roles in over.
func (p *sharePlanRun) onlyUnplaceableShort(over map[string]bool) bool {
	short := false
	for _, r := range p.roles {
		if p.work[r.Key] >= p.integ[r.Key] || p.l.FillHeld(r.Key, p.now) {
			continue
		}
		if _, grows := variantOf(p.in.Grow, r.Key, r.ReplicaGPUs); !grows {
			continue
		}
		if !p.unplaceable(r.Key, over) {
			return false
		}
		short = true
	}
	return short
}

// unplaceable reports whether no node can hold a replica of receiver rc, even
// once the roles in over -- the ones a return would shrink -- gave back every
// pod they could give: no donor could fund it (ShareUnfundedNoCompatibleDonor)
// while the planner knew the nodes, rc's pods and the donors' pods, and no
// node holds rc's pods in its free GPUs plus those roles' pods on it
// (ShareNoNodeFits, as the no-node-fits reason counts the giving roles' pods).
//
// No donor funding rc is not enough. A donor set pays a receiver's whole
// replica from donor replicas and never from the idle quota, so a receiver a
// node could hold once a neighbour's pods went is still unfunded when the
// neighbour cannot pay all of it: a large model that lent a node to a small
// one, and bursts, gets the node back only by the small model returning its
// pods there and the idle fill raising it. Without node information, or
// without the donors' pods, the idle fill places blind, and a return may
// still serve rc.
func (p *sharePlanRun) unplaceable(rc string, over map[string]bool) bool {
	grow, ok := p.in.Grow[rc]
	if !ok || len(grow.PodGPUs) == 0 || p.in.Nodes == nil || p.in.DonorUnits == nil ||
		p.plan.Unfunded[rc] != ShareUnfundedNoCompatibleDonor {
		return false
	}
	returning := map[string]int{}
	for dn, o := range over {
		if !o || dn == rc {
			continue
		}
		for _, u := range p.in.DonorUnits[dn] {
			for _, pod := range u.Pods {
				returning[pod.Node] += pod.GPUs
			}
		}
	}
	return ShareNoNodeFits(p.in.Nodes, returning, p.in.receiverFit(rc, grow))
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
	rule := shareSetRule{hard: p.hard[rc] > 0, free: p.unpacedNow(rc), need: p.needView,
		donorNeed: func(r ShareRole) ShareRole { return p.donorViewFor(r, rc) }}
	// As fundFrom: spread, a receiver at its need takes no donor below its own
	// recent peak (donor-peak), by a set any more than by a single move.
	peakHeld := false
	if !p.plan.HeadroomCapped && float64(p.work[rc]) >= p.need(rc) {
		rule.keepsPeak = func(dn string, left int) bool {
			if left < p.donorPeak(dn) {
				peakHeld = true
				return true
			}
			return false
		}
	}
	defer func() {
		if !funded && peakHeld {
			p.plan.Withheld[ShareWithheldDonorPeak]++
		}
	}()
	var set []shareDonor
	if in.Nodes != nil {
		// Placed on the free GPUs the idle credit has not already spent
		// (idleNodes): placed on in.Nodes, a set -- or its deferral to the
		// fill -- would count a node the credit gave away a second time.
		// What the set takes of a node's free GPUs comes off both pictures;
		// in.Nodes is the one the fill places into.
		view := in
		view.Nodes = p.idleNodes
		var left map[string]int
		var claimed string
		set, left, claimed, deferred = shareNodeSet(l, view, rc, donors, p.work, p.moved, p.byKey, p.limits(), p.cont, p.z, p.isConfirmed, rule, now, tm)
		if len(set) > 0 {
			for n, info := range p.idleNodes {
				if used := info.Free - min(info.Free, left[n]); used > 0 {
					fill := in.Nodes[n]
					fill.Free = max(0, fill.Free-used)
					in.Nodes[n] = fill
				}
			}
			withdrawNodeSet(p.idleNodes, in.DonorUnits, set, left)
			// The receiver's group takes its domain in both pictures: no
			// other group is planned or filled into it this cycle.
			if claimed != "" {
				claimDomain(p.idleNodes, in.DomainKey[rc], claimed)
				claimDomain(in.Nodes, in.DomainKey[rc], claimed)
			}
		}
	}
	if deferred {
		return false, true
	}
	if len(set) == 0 && in.DomainKey[rc] == "" {
		set = shareDonorSet(l, in, rc, donors, p.work, p.moved, p.byKey, p.limits(), p.cont, p.z, p.isConfirmed, rule, now, tm)
	}
	if len(set) == 0 {
		return false, false
	}
	started := startShareSet(l, in, rc, set, float64(p.work[rc]) < p.byKey[rc].Need, rule.hard, now, tm)
	p.plan.Started = append(p.plan.Started, started...)
	for _, d := range set {
		p.work[d.role] -= d.gpus
		p.moved[d.role]++
	}
	p.work[rc] += max(in.Grow[rc].GPUs, 1)
	p.moved[rc]++
	return true, false
}

// shareSetRule is how a hard imbalance relaxes a donor set's admission
// (§6.5, §6.7): hard, a receiver in one this cycle, needs no confirmed role;
// free, one confirmed for ShareUnpacedAfterCycles, skips the pace and the
// concurrency limit. need and donorNeed judge the receiver and each donor as
// a single move judges them (needView, donorView): on its mean, a swinging
// donor whose demand is rising would be drained to its keep by a set a single
// move from it is refused.
type shareSetRule struct {
	hard, free      bool
	need, donorNeed func(ShareRole) ShareRole
	// keepsPeak, when set, reports whether donor dn, left with left GPUs,
	// would fall below its own recent peak for a receiver already at its need
	// (the donor-peak guard).
	keepsPeak func(dn string, left int) bool
}

// withholds reports whether the rule withholds donor dn left with left GPUs
// (keepsPeak).
func (rule shareSetRule) withholds(dn string, left int) bool {
	return rule.keepsPeak != nil && rule.keepsPeak(dn, left)
}

// receiver is receiver role r as the rule judges it.
func (rule shareSetRule) receiver(r ShareRole) ShareRole {
	if rule.need == nil {
		return r
	}
	return rule.need(r)
}

// donor is donor role r as the rule judges it.
func (rule shareSetRule) donor(r ShareRole) ShareRole {
	if rule.donorNeed == nil {
		return r
	}
	return rule.donorNeed(r)
}

// shareCancelSlack absorbs floating-point error when a donor's shortfall is
// compared with a whole number of replicas: a continuous target of exactly 3
// can arrive as 2.9999999999999996.
const shareCancelSlack = 1e-9

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
	isConfirmed func(string) bool, rule shareSetRule, now time.Time, tm ShareTimings) []shareDonor {
	grow, ok := in.Grow[rc]
	if !ok || len(grow.PodGPUs) == 0 || in.Give == nil {
		return nil
	}
	// A receiver in its reversal hold may still be exempt from it, per donor
	// (shareHardImbalance); only one that is not short is passed over here.
	short := float64(work[rc]) < byKey[rc].Need
	if (!short && l.ReceivingHeld(rc, now, tm)) || (!rule.free && moved[rc] >= ShareMaxReplicasPerCycle) {
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
			exempt := shareHardImbalance(in.SkipWaits, work[rc], rule.receiver(byKey[rc]), work[dn]-k*gd, rule.donor(byKey[dn]))
			if (!rule.free && moved[dn]+k > shareSetPace(grow)) || work[dn]-k*gd < lim.keep[dn] || shareMoveHeld(l, dn, rc, exempt, now, tm) ||
				rule.withholds(dn, work[dn]-k*gd) {
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
	if len(set) < 2 || !admitShareSet(l, in, rc, taken, work, byKey, lim, cont, z, isConfirmed, rule) {
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
	isConfirmed func(string) bool, rule shareSetRule) bool {
	if !rule.free && l.InFlight()+1 > ShareMaxConcurrentTransfers {
		return false
	}
	g := max(in.Grow[rc].GPUs, 1)
	confirmed := rule.hard || isConfirmed(rc)
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
func startShareSet(l *ShareLedger, in SharePlanInput, rc string, set []shareDonor, urgent, skipped bool,
	now time.Time, tm ShareTimings) []ShareTransfer {
	grow := in.Grow[rc]
	setID := "pending"
	if len(set) == 1 {
		setID = "" // a single donor replica, placed on a node: an ordinary transfer
	}
	primary := l.Start(ShareTransfer{
		Donor: set[0].role, Receiver: rc, GPUs: max(grow.GPUs, 1), DonorGPUs: set[0].gpus,
		DonorVariant: set[0].variant, ReceiverVariant: grow.Name, SetID: setID, Urgent: urgent,
		SkippedWaits: skipped, PlannedPods: set[0].planned,
	}, in.Held, now, tm)
	ids := make([]string, 0, len(set)-1)
	for _, d := range set[1:] {
		c := l.Start(ShareTransfer{Donor: d.role, DonorGPUs: d.gpus, DonorVariant: d.variant, SetID: "pending",
			SkippedWaits: skipped, PlannedPods: d.planned}, in.Held, now, tm)
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
	isConfirmed func(string) bool, rule shareSetRule, now time.Time, tm ShareTimings) ([]shareDonor, map[string]int, string, bool) {
	grow, ok := in.Grow[rc]
	if !ok || len(grow.PodGPUs) == 0 || in.Give == nil || in.DonorUnits == nil {
		return nil, nil, "", false
	}
	// As in shareDonorSet: the exemption is decided per donor (canGive).
	short := float64(work[rc]) < byKey[rc].Need
	if (!short && l.ReceivingHeld(rc, now, tm)) || (!rule.free && moved[rc] >= ShareMaxReplicasPerCycle) {
		return nil, nil, "", false
	}
	// Each domain is tried in turn: fixing it at the first placement would
	// refuse receivers another domain fits. A group (an exclusive topology,
	// more than one pod) lands only in a domain holding no other group, as
	// the node view places it: none this cycle claimed, and every group pod
	// already there leaving with a donor replica the set takes.
	fit := in.receiverFit(rc, grow)
	key, exclusive := fit.domainKey()
	for _, d := range shareDomains(in.Nodes, key) {
		in1 := shareNodesInDomain(in.Nodes, key, d)
		if exclusive && domainClaimed(in.Nodes, in1, key) {
			continue
		}
		search := shareNodeSearch{l: l, in: in, rc: rc, donors: donors, work: work, moved: moved, byKey: byKey, keep: lim.keep,
			now: now, tm: tm, taken: map[string]int{}, used: map[shareUnitRef]bool{}, rule: rule}
		hole, ok := search.place(grow.PodGPUs, in1)
		if !ok || (exclusive && !search.freesGroups(in1)) {
			continue
		}
		if len(search.set) == 0 && shareIdle(l, in, work) >= max(grow.GPUs, 1) && !l.FillShort(rc) {
			// The pods fit free GPUs and the quota has room: the idle fill
			// funds this receiver without shrinking anyone -- unless the last
			// fill left it short, for a reason only the fill sees. Not
			// another domain, nor the node-blind search: either would shrink
			// donors for GPUs nobody needed.
			return nil, nil, "", true
		}
		// The placement may need fewer donor replicas than the receiver's
		// quota -- or none, where its pods fit free GPUs. Free GPUs place a
		// pod; they never pay for it: donor replicas from any node make up
		// the quota, their pods' GPUs coming free wherever they run.
		if !search.topUp(max(grow.GPUs, 1), hole) ||
			!admitShareSet(l, in, rc, search.taken, work, byKey, lim, cont, z, isConfirmed, rule) {
			continue
		}
		claimed := ""
		if exclusive {
			claimed = d
		}
		return search.set, hole, claimed, false
	}
	return nil, nil, "", false
}

// place puts every receiver pod, largest first, on one of nodes: into the
// smallest hole that already fits, otherwise into a hole opened on the node
// needing the fewest donor replicas, then leaving the smallest hole. Each pod
// goes only to the nodes its template admits (sharePodNodes). It returns the
// holes left on every node, and whether every pod was placed.
func (s *shareNodeSearch) place(podGPUs []int, nodes []string) (map[string]int, bool) {
	hole := map[string]int{}
	for n, info := range s.in.Nodes {
		hole[n] = info.Free
	}
	for _, pod := range sharePodNodes(s.in.Nodes, nodes, ShareReplicaFit{PodGPUs: podGPUs, Placements: s.in.Placements[s.rc]}) {
		p, nodes := pod.gpus, pod.nodes
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
	// rule is how a hard imbalance relaxes the search (shareSetRule).
	rule shareSetRule
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
	exempt := shareHardImbalance(s.in.SkipWaits, s.work[s.rc], s.rule.receiver(s.byKey[s.rc]), left, s.rule.donor(s.byKey[dn]))
	return (s.rule.free || s.moved[dn]+k <= shareSetPace(s.in.Grow[s.rc])) && left >= s.keep[dn] &&
		!shareMoveHeld(s.l, dn, s.rc, exempt, s.now, s.tm) && !s.rule.withholds(dn, left) &&
		(s.rule.free || s.l.InFlight()+1 <= ShareMaxConcurrentTransfers)
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
// woken models and what donor-only transfers have not released yet
// (ShareLedger.Unreleased), capped by the cluster's free GPUs.
func shareIdle(l *ShareLedger, in SharePlanInput, work map[string]int) int {
	idle := in.Budget - in.WakeHeld - l.Unreleased()
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
func shareNodesInDomain(nodes map[string]ShareNode, key, value string) []string {
	var out []string
	for _, n := range slices.Sorted(maps.Keys(nodes)) {
		if key == "" || nodes[n].Labels[key] == value {
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
