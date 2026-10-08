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
	// highest-index group of a LeaderWorkerSet. A role absent gives nothing to
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
// It plans single-donor transfers: one donor replica at least the receiver
// replica's size funds it, which is valid wherever the donor pod ran (§6.5).
// Per-node donor sets extend the donor choice, not this procedure.
func PlanShareTransfers(l *ShareLedger, in SharePlanInput, now time.Time, tm ShareTimings) SharePlan {
	plan := SharePlan{Unfunded: map[string]string{}, Withheld: map[string]int{}}
	// Node state is spent as node-aware sets start within the cycle; work on
	// copies, never the caller's.
	in.Nodes, in.DonorUnits = maps.Clone(in.Nodes), maps.Clone(in.DonorUnits)
	plan.Nodes = in.Nodes

	// Rule 5: a swinging role is planned on its mean need.
	raw := make(map[string]float64, len(in.Roles))
	for _, r := range in.Roles {
		raw[r.Key] = r.Need
	}
	l.RecordNeeds(raw, now, tm)
	roles := slices.Clone(in.Roles)
	byKey := make(map[string]ShareRole, len(roles))
	for i := range roles {
		if l.Swinging(roles[i].Key, now) {
			roles[i].Need = l.PlanningNeed(roles[i].Key, roles[i].Need, now)
			plan.Swinging = append(plan.Swinging, roles[i].Key)
		}
		byKey[roles[i].Key] = roles[i]
	}
	cont := ContinuousShareTargets(roles, float64(in.Budget))
	integ := IntegerShareTargets(roles, in.Budget)

	// Rule 3: cancel only on a clear reversal, and only while it is free.
	committed := l.Committed(in.Held)
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
		dn, rc := byKey[t.Donor], byKey[t.Receiver]
		donorAfter := committed[t.Donor] // already net of this transfer
		receiverWithout := committed[t.Receiver] - t.GPUs
		donorShort := float64(donorAfter) < cont[t.Donor]-2*shareTol(cont[t.Donor], dn, in.Tolerance)
		receiverFine := float64(receiverWithout) > cont[t.Receiver]+shareTol(cont[t.Receiver], rc, in.Tolerance)
		if (donorShort || receiverFine) && l.Cancel(t.ID, now, tm) {
			plan.Cancelled = append(plan.Cancelled, t.ID)
			// A set's contributors go with its primary, while they still can.
			if t.IsSetPrimary() {
				for _, m := range l.Transfers() {
					if m.SetID == t.ID && l.Cancel(m.ID, now, tm) {
						plan.Cancelled = append(plan.Cancelled, m.ID)
					}
				}
			}
			committed = l.Committed(in.Held)
		}
	}

	// Rules 1 and 2: judge the committed allocation; a role is actionable only
	// when a move could fix it.
	plan.Evaluation = EvaluateShare(roles, committed, in.Thresholds, in.Budget, in.Tolerance)
	actionable := map[string]bool{}
	for _, v := range plan.Evaluation.Roles {
		actionable[v.Key] = v.Actionable
		if !v.InBand && !v.Actionable {
			plan.Withheld[ShareWithheldNotActionable]++
		}
	}
	confirmed := l.Confirm(actionable)

	// Reserve refill (section 6.2). Committed GPUs above the budget are a debt:
	// a wake took reserve, or a variant was scaled past its target from
	// outside. It is an entitled receiver, paid back before any rebalance and
	// without the confirmation a rebalance needs, by transfers with a donor and
	// no receiver: the donor is lowered and nobody is raised. A donor gives
	// only above its whole-replica target and its floor, best-off first.
	plan.ReserveDebt = ShareDebt(committed, in.Budget)
	// moved counts each role's replicas moved this cycle -- refills and
	// rebalance alike -- against ShareMaxReplicasPerCycle.
	moved := map[string]int{}
	if debt := plan.ReserveDebt; debt > 0 {
		var cands []string
		for _, r := range roles {
			if c := committed[r.Key]; c > integ[r.Key] && c > r.Floor {
				cands = append(cands, r.Key)
			}
		}
		zc := func(k string) float64 {
			r := byKey[k]
			return ShareScore(float64(committed[k]), r.Need, r.Weight)
		}
		slices.SortFunc(cands, func(a, b string) int { return cmp.Or(cmp.Compare(zc(b), zc(a)), cmp.Compare(a, b)) })
		for _, dn := range cands {
			if debt <= 0 || l.InFlight() >= ShareMaxConcurrentTransfers {
				break
			}
			give := ShareVariant{GPUs: byKey[dn].ReplicaGPUs}
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
			left := committed[dn]
			for moved[dn] < ShareMaxReplicasPerCycle && debt > 0 && l.InFlight() < ShareMaxConcurrentTransfers {
				if left-gd < byKey[dn].Floor || left-gd < integ[dn] {
					break
				}
				t := l.Start(ShareTransfer{Donor: dn, DonorGPUs: gd, DonorVariant: give.Name, Entitled: true}, in.Held, now, tm)
				plan.Started = append(plan.Started, t)
				plan.Refills++
				moved[dn]++
				debt -= gd
				left -= gd
			}
		}
		committed = l.Committed(in.Held)
	}

	isConfirmed := func(k string) bool { return confirmed[k] >= ShareConfirmCycles }
	if !slices.ContainsFunc(slices.Collect(maps.Keys(confirmed)), isConfirmed) {
		return plan
	}

	z := func(k string, g int) float64 { r := byKey[k]; return ShareScore(float64(g), r.Need, r.Weight) }
	var receivers, donors []string
	for _, r := range roles {
		c := committed[r.Key]
		if c < integ[r.Key] {
			receivers = append(receivers, r.Key)
		}
		if c > integ[r.Key] && c > r.Floor {
			donors = append(donors, r.Key)
		}
	}
	slices.SortFunc(receivers, func(a, b string) int {
		return cmp.Or(cmp.Compare(z(a, committed[a]), z(b, committed[b])), cmp.Compare(a, b))
	})
	slices.SortFunc(donors, func(a, b string) int {
		return cmp.Or(cmp.Compare(z(b, committed[b]), z(a, committed[a])), cmp.Compare(a, b))
	})

	work := maps.Clone(committed)
	for _, rc := range receivers {
		candidates, misfits, funded := 0, 0, false
		for _, dn := range donors {
			if l.InFlight() >= ShareMaxConcurrentTransfers {
				return plan
			}
			if moved[rc] >= ShareMaxReplicasPerCycle || moved[dn] >= ShareMaxReplicasPerCycle {
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
			// Rule 4: a role that gave cannot receive, and one that received
			// cannot give, within the hold.
			if l.ReceivingHeld(rc, now, tm) || l.GivingHeld(dn, now, tm) {
				plan.Withheld[ShareWithheldReversalHold]++
				continue
			}
			if !isConfirmed(rc) && !isConfirmed(dn) {
				continue // every transfer must fix a confirmed actionable role
			}
			// §6.2 admission: the worst-off role of the pair improves, and the
			// donor is not pushed out of band.
			before := math.Min(z(dn, work[dn]), z(rc, work[rc]))
			after := math.Min(z(dn, work[dn]-gd), z(rc, work[rc]+g))
			donorAfter := float64(work[dn] - gd)
			donorOK := donorAfter >= cont[dn] || ShareInBand(donorAfter, cont[dn], dr, in.Tolerance, in.Thresholds[dn])
			if !(after > before) || !donorOK || work[dn]-gd < dr.Floor {
				continue
			}
			t := l.Start(ShareTransfer{
				Donor: dn, Receiver: rc, GPUs: g, DonorGPUs: gd,
				DonorVariant: in.Give[dn].Name, ReceiverVariant: in.Grow[rc].Name,
				Urgent: float64(work[rc]) < rr.Need,
			}, in.Held, now, tm)
			plan.Started = append(plan.Started, t)
			work[dn] -= gd
			work[rc] += g
			moved[dn]++
			moved[rc]++
			funded = true
		}
		if !funded && candidates > 0 && misfits == candidates && in.Grow != nil {
			// No single donor replica fits. Several may (§6.5).
			// With node information, a node's free GPUs and the donor pods on it
			// make one hole together, and the holes can be kept in the
			// receiver's topology domain (§6.5). Without it, or when it finds
			// nothing, each donor pod is a hole of its own size -- true on any
			// node, but blind to a domain, so not for a receiver that has one.
			var set []shareDonor
			deferred := false
			if in.Nodes != nil {
				var left map[string]int
				set, left, deferred = shareNodeSet(l, in, rc, donors, work, moved, byKey, cont, z, isConfirmed, now, tm)
				if len(set) > 0 {
					withdrawNodeSet(in.Nodes, in.DonorUnits, set, left)
				}
			}
			if deferred {
				// Its pods fit free GPUs and the quota has room: the idle
				// fill funds it, and no donor shrinks for it.
				continue
			}
			if len(set) == 0 && in.DomainKey[rc] == "" {
				set = shareDonorSet(l, in, rc, donors, work, moved, byKey, cont, z, isConfirmed, now, tm)
			}
			if len(set) > 0 {
				started := startShareSet(l, in, rc, set, float64(work[rc]) < byKey[rc].Need, now, tm)
				plan.Started = append(plan.Started, started...)
				for _, d := range set {
					work[d.role] -= d.gpus
					moved[d.role]++
				}
				work[rc] += max(in.Grow[rc].GPUs, 1)
				moved[rc]++
				funded = true
			}
		}
		if !funded && isConfirmed(rc) && actionable[rc] && candidates > 0 && misfits == candidates {
			plan.Unfunded[rc] = ShareUnfundedNoCompatibleDonor
		}
	}
	return plan
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
	byKey map[string]ShareRole, cont map[string]float64, z func(string, int) float64,
	isConfirmed func(string) bool, now time.Time, tm ShareTimings) []shareDonor {
	grow, ok := in.Grow[rc]
	if !ok || len(grow.PodGPUs) == 0 || in.Give == nil {
		return nil
	}
	if l.ReceivingHeld(rc, now, tm) || moved[rc] >= ShareMaxReplicasPerCycle {
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
			if moved[dn]+k > ShareMaxReplicasPerCycle || work[dn]-k*gd < byKey[dn].Floor || l.GivingHeld(dn, now, tm) {
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
	if len(set) < 2 || !admitShareSet(l, in, rc, set, taken, work, byKey, cont, z, isConfirmed) {
		return nil
	}
	return set
}

// admitShareSet is §6.2 admission over a whole donor set (§6.5): the set fits
// the concurrency limit, the lowest score among the receiver and every donor
// rises, no donor leaves its band, and a confirmed role takes part. taken is
// the replicas each donor gives.
func admitShareSet(l *ShareLedger, in SharePlanInput, rc string, set []shareDonor, taken map[string]int,
	work map[string]int, byKey map[string]ShareRole, cont map[string]float64, z func(string, int) float64,
	isConfirmed func(string) bool) bool {
	if l.InFlight()+len(set) > ShareMaxConcurrentTransfers {
		return false
	}
	g := max(in.Grow[rc].GPUs, 1)
	confirmed := isConfirmed(rc)
	before, after := z(rc, work[rc]), z(rc, work[rc]+g)
	for dn, k := range taken {
		gd := max(in.Give[dn].GPUs, 1)
		before = math.Min(before, z(dn, work[dn]))
		after = math.Min(after, z(dn, work[dn]-k*gd))
		left := float64(work[dn] - k*gd)
		if !(left >= cont[dn] || ShareInBand(left, cont[dn], byKey[dn], in.Tolerance, in.Thresholds[dn])) {
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
	byKey map[string]ShareRole, cont map[string]float64, z func(string, int) float64,
	isConfirmed func(string) bool, now time.Time, tm ShareTimings) ([]shareDonor, map[string]int, bool) {
	grow, ok := in.Grow[rc]
	if !ok || len(grow.PodGPUs) == 0 || in.Give == nil || in.DonorUnits == nil {
		return nil, nil, false
	}
	if l.ReceivingHeld(rc, now, tm) || moved[rc] >= ShareMaxReplicasPerCycle {
		return nil, nil, false
	}
	// Each domain is tried in turn: fixing it at the first placement would
	// refuse receivers another domain fits.
	key := in.DomainKey[rc]
	for _, domain := range shareDomains(in.Nodes, key) {
		in1 := shareNodesInDomain(in.Nodes, key, domain)
		search := shareNodeSearch{l: l, in: in, rc: rc, donors: donors, work: work, moved: moved, byKey: byKey,
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
			!admitShareSet(l, in, rc, search.set, search.taken, work, byKey, cont, z, isConfirmed) {
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
	now         time.Time
	tm          ShareTimings
	set         []shareDonor
	taken       map[string]int
	used        map[shareUnitRef]bool
}

// canGive reports whether donor dn may give extra replicas more than it
// already does in the set, with pending more replicas about to join the set
// from any donor: its floor, the pace, its holds and the concurrency limit.
func (s *shareNodeSearch) canGive(dn string, extra, pending int) bool {
	give, ok := s.in.Give[dn]
	if !ok || dn == s.rc {
		return false
	}
	k := s.taken[dn] + extra
	return s.moved[dn]+k <= ShareMaxReplicasPerCycle && s.work[dn]-k*max(give.GPUs, 1) >= s.byKey[dn].Floor &&
		!s.l.GivingHeld(dn, s.now, s.tm) && s.l.InFlight()+len(s.set)+pending <= ShareMaxConcurrentTransfers
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
			if s.used[r] || !s.canGive(dn, extra[dn]+1, len(units)+1) {
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
			if s.used[r] || !s.canGive(dn, 1, 1) {
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
