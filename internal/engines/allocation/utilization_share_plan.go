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
			if in.Nodes != nil {
				set = shareNodeSet(l, in, rc, donors, work, moved, byKey, cont, z, isConfirmed, now, tm)
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
	// planned are the donor pods a node-aware set chose (namespace/name).
	planned []string
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
	if len(set) < 2 || l.InFlight()+len(set) > ShareMaxConcurrentTransfers {
		return nil
	}
	// Admission over the whole set.
	g := max(grow.GPUs, 1)
	confirmed := isConfirmed(rc)
	before, after := z(rc, work[rc]), z(rc, work[rc]+g)
	for dn, k := range taken {
		gd := max(in.Give[dn].GPUs, 1)
		before = math.Min(before, z(dn, work[dn]))
		after = math.Min(after, z(dn, work[dn]-k*gd))
		left := float64(work[dn] - k*gd)
		if !(left >= cont[dn] || ShareInBand(left, cont[dn], byKey[dn], in.Tolerance, in.Thresholds[dn])) {
			return nil
		}
		confirmed = confirmed || isConfirmed(dn)
	}
	if !confirmed || !(after > before) {
		return nil
	}
	return set
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
// hole, they never stand in for a donor -- the quota still has to fit. The set
// passes the same admission as shareDonorSet.
func shareNodeSet(l *ShareLedger, in SharePlanInput, rc string, donors []string, work, moved map[string]int,
	byKey map[string]ShareRole, cont map[string]float64, z func(string, int) float64,
	isConfirmed func(string) bool, now time.Time, tm ShareTimings) []shareDonor {
	grow, ok := in.Grow[rc]
	if !ok || len(grow.PodGPUs) == 0 || in.Give == nil || in.DonorUnits == nil {
		return nil
	}
	if l.ReceivingHeld(rc, now, tm) || moved[rc] >= ShareMaxReplicasPerCycle {
		return nil
	}
	need := slices.Sorted(slices.Values(grow.PodGPUs))
	slices.Reverse(need)
	nodes := slices.Sorted(maps.Keys(in.Nodes))
	hole := map[string]int{}
	for n, info := range in.Nodes {
		hole[n] = info.Free
	}
	key, domain := in.DomainKey[rc], ""
	inDomain := func(n string) bool { return key == "" || domain == "" || in.Nodes[n].Labels[key] == domain }

	taken := map[string]int{}
	usedUnit := map[string]map[int]bool{}
	var set []shareDonor
	canGive := func(dn string, extra int) bool {
		give, ok := in.Give[dn]
		if !ok || dn == rc {
			return false
		}
		gd := max(give.GPUs, 1)
		k := taken[dn] + extra
		return moved[dn]+k <= ShareMaxReplicasPerCycle && work[dn]-k*gd >= byKey[dn].Floor &&
			!l.GivingHeld(dn, now, tm) && l.InFlight()+len(set)+extra <= ShareMaxConcurrentTransfers
	}
	for _, p := range need {
		best := ""
		for _, n := range nodes {
			if inDomain(n) && hole[n] >= p && (best == "" || hole[n] < hole[best]) {
				best = n
			}
		}
		if best == "" {
			// Open a hole: per node, the fewest donor units that cover p there.
			type option struct {
				node  string
				units [][2]int // [donor index, unit index]
				left  int
			}
			var opt *option
			for _, n := range nodes {
				if !inDomain(n) {
					continue
				}
				h, extra := hole[n], map[string]int{}
				var units [][2]int
				for di, dn := range donors {
					if h >= p {
						break
					}
					for ui, u := range in.DonorUnits[dn] {
						if h >= p {
							break
						}
						if usedUnit[dn][ui] || !canGive(dn, extra[dn]+1) {
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
						units = append(units, [2]int{di, ui})
						extra[dn]++
						h += on
					}
				}
				if h < p {
					continue
				}
				o := option{node: n, units: units, left: h - p}
				if opt == nil || len(o.units) < len(opt.units) ||
					(len(o.units) == len(opt.units) && o.left < opt.left) {
					opt = &o
				}
			}
			if opt == nil {
				return nil
			}
			for _, du := range opt.units {
				dn, u := donors[du[0]], in.DonorUnits[donors[du[0]]][du[1]]
				if usedUnit[dn] == nil {
					usedUnit[dn] = map[int]bool{}
				}
				usedUnit[dn][du[1]] = true
				taken[dn]++
				d := shareDonor{role: dn, variant: in.Give[dn].Name, gpus: max(in.Give[dn].GPUs, 1)}
				for _, pod := range u.Pods {
					hole[pod.Node] += pod.GPUs
					d.planned = append(d.planned, pod.Name)
				}
				set = append(set, d)
			}
			best = opt.node
		}
		hole[best] -= p
		if key != "" && domain == "" {
			domain = in.Nodes[best].Labels[key]
		}
	}
	if len(set) == 0 {
		return nil // free GPUs alone: the idle fill's to place, within the quota
	}
	gives, g := 0, max(grow.GPUs, 1)
	for _, d := range set {
		gives += d.gpus
	}
	if gives < g {
		return nil
	}
	// Admission over the whole set, as shareDonorSet.
	confirmed := isConfirmed(rc)
	before, after := z(rc, work[rc]), z(rc, work[rc]+g)
	for dn, k := range taken {
		gd := max(in.Give[dn].GPUs, 1)
		before = math.Min(before, z(dn, work[dn]))
		after = math.Min(after, z(dn, work[dn]-k*gd))
		left := float64(work[dn] - k*gd)
		if !(left >= cont[dn] || ShareInBand(left, cont[dn], byKey[dn], in.Tolerance, in.Thresholds[dn])) {
			return nil
		}
		confirmed = confirmed || isConfirmed(dn)
	}
	if !confirmed || !(after > before) {
		return nil
	}
	return set
}
