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
	var plan SharePlan

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
		dn, rc := byKey[t.Donor], byKey[t.Receiver]
		donorAfter := committed[t.Donor] // already net of this transfer
		receiverWithout := committed[t.Receiver] - t.GPUs
		donorShort := float64(donorAfter) < cont[t.Donor]-2*shareTol(cont[t.Donor], dn, in.Tolerance)
		receiverFine := float64(receiverWithout) > cont[t.Receiver]+shareTol(cont[t.Receiver], rc, in.Tolerance)
		if (donorShort || receiverFine) && l.Cancel(t.ID, now, tm) {
			plan.Cancelled = append(plan.Cancelled, t.ID)
			committed = l.Committed(in.Held)
		}
	}

	// Rules 1 and 2: judge the committed allocation; a role is actionable only
	// when a move could fix it.
	plan.Evaluation = EvaluateShare(roles, committed, in.Thresholds, in.Budget, in.Tolerance)
	actionable := map[string]bool{}
	for _, v := range plan.Evaluation.Roles {
		actionable[v.Key] = v.Actionable
	}
	confirmed := l.Confirm(actionable)
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
	moved := map[string]int{}
	for _, rc := range receivers {
		for _, dn := range donors {
			if l.InFlight() >= ShareMaxConcurrentTransfers {
				return plan
			}
			if moved[rc] >= ShareMaxReplicasPerCycle || moved[dn] >= ShareMaxReplicasPerCycle {
				continue
			}
			rr, dr := byKey[rc], byKey[dn]
			g, gd := max(rr.ReplicaGPUs, 1), max(dr.ReplicaGPUs, 1)
			if gd < g {
				continue // one donor pod must cover one receiver pod (§6.5)
			}
			// Rule 4: a role that gave cannot receive, and one that received
			// cannot give, within the hold.
			if l.ReceivingHeld(rc, now, tm) || l.GivingHeld(dn, now, tm) {
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
				Urgent: float64(work[rc]) < rr.Need,
			}, in.Held, now, tm)
			plan.Started = append(plan.Started, t)
			work[dn] -= gd
			work[rc] += g
			moved[dn]++
			moved[rc]++
		}
	}
	return plan
}

// shareTol is a role's band half-width in GPUs at target (proposal §6.1): the
// configured tolerance of the target, never under half a replica.
func shareTol(target float64, r ShareRole, tolerance float64) float64 {
	return math.Max(tolerance*target, 0.5*float64(max(r.ReplicaGPUs, 1)))
}
