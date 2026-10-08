package steadystate

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

const (
	// utilizationShareTransferAnnotation persists a transfer on its donor pods,
	// so a restarted controller rebuilds its ledger from the cluster (§6.3). It
	// uses the pod patch permission WVA already holds.
	utilizationShareTransferAnnotation = "llm-d.ai/utilization-share-transfer"
	// podDeletionCostAnnotation steers which pod a ReplicaSet removes first
	// (§6.5); the warm pool uses the same lever.
	podDeletionCostAnnotation = constants.PodDeletionCostAnnotation
	// donorDeletionCost is below the default of every unmarked sibling.
	donorDeletionCost = "-1000"
)

// shareActuation is what one active cycle of a group did, for the caller to
// apply and publish.
type shareActuation struct {
	overrides   map[string]utilizationShareOverride
	promised    int
	reserveDebt int
	// blocked is each planned model's utilization-share blocked reasons,
	// keyed by namespace/model, an empty list for a model with none.
	blocked map[string][]string
	// withheld counts this cycle's withheld transfers by reason.
	withheld map[string]int
	// claimable are the group's transfers a wake may still claim.
	claimable []decision.ShareClaimable
	timings   allocation.ShareTimings
	sources   allocation.ShareTimingSource
	swinging  []string
	// releasing and filling count the group's transfers in flight.
	releasing, filling int
}

// countInFlight counts a ledger's transfers in flight by state.
func countInFlight(l *allocation.ShareLedger) (releasing, filling int) {
	for _, t := range l.Transfers() {
		switch t.State {
		case allocation.ShareReleasing:
			releasing++
		case allocation.ShareFilling:
			filling++
		}
	}
	return releasing, filling
}

// utilizationShareOverride is a planned variant's target as the optimizer owns
// it, applied over the cycle's decisions.
type utilizationShareOverride struct {
	Target int
	Why    string
}

func shareGroupKey(g allocation.ShareGroup) string { return g.AcceleratorType + "|" + g.Scope }

// actuateUtilizationShare runs one active cycle for a group (stage 2): observe,
// plan, mark, fill. It returns each planned variant's target, keyed by
// namespace/variant. scaleTargets is this cycle's scale targets by
// namespace/model, then variant.
func (e *Engine) actuateUtilizationShare(ctx context.Context, logger logr.Logger, us config.UtilizationShare,
	g allocation.ShareGroup, ev allocation.ShareEvaluation,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor, now time.Time) shareActuation {
	st := &e.utilizationShare
	if st.ledgers == nil {
		st.ledgers = map[string]*allocation.ShareLedger{}
		st.desired = map[string]int{}
		st.quietUntil = map[string]time.Time{}
		st.divergedSince = map[string]time.Time{}
	}
	key := shareGroupKey(g)
	logger = logger.WithValues("accelerator", g.AcceleratorType, "scope", g.Scope)
	scope := g.Scope
	if scope == "" {
		scope = constants.UtilizationShareClusterScope
	}

	accessor := func(role, variant string) scaletarget.ScaleTargetAccessor {
		return scaleTargets[utils.GetNamespacedKey(g.Origins[role].Namespace, variant)]
	}
	variantKey := func(role, variant string) string {
		return utils.GetNamespacedKey(g.Origins[role].Namespace, variant)
	}
	tm, src := allocation.DeriveShareTimings(e.shareTimingInputs(g, accessor), st.ledgers[key])

	// Every planned variant's desired count starts at what is running.
	for role, vs := range g.Variants {
		for _, v := range vs {
			k := variantKey(role, v.Name)
			if _, ok := st.desired[k]; !ok {
				st.desired[k] = v.Current
			}
		}
	}

	l, ok := st.ledgers[key]
	if !ok {
		l = allocation.NewShareLedger()
		restored, err := e.restoreShareTransfers(ctx, logger, l, g, accessor, variantKey, now, tm)
		if err != nil {
			// Without the marks the ledger would start blind to releases in
			// flight; it is not created, so the read is retried next cycle,
			// and nothing is planned meanwhile.
			logger.Error(err, "utilization share could not read transfer marks; retrying next cycle")
			return shareActuation{overrides: e.shareOverrides(g, variantKey, "reading transfer marks"),
				timings: tm, sources: src}
		}
		st.ledgers[key] = l
		// After a restart a transfer in Filling has no mark left, and its
		// receiver may have no pods yet: plan nothing for one fill timeout.
		st.quietUntil[key] = now.Add(tm.FillTimeout)
		logger.Info("Utilization share: ledger started", "restoredTransfers", restored,
			"planningFrom", st.quietUntil[key], "timings", src)
	}

	held := g.Committed
	// Wake claims first, before Observe: a claim made against a Releasing
	// transfer must be applied before a release turns it into Filling.
	// Each claim is applied on its own. A wake's claims were made all or none
	// (ShareClaimStore.ClaimSet), and the wake has happened by now: its pods
	// exist, and the scheduler gives a hole to whichever Pending pod comes
	// first, not to whoever was promised it. Refusing the redirect of a claim
	// whose partner moved on would not keep the woken pod out of its hole; it
	// would only raise the original receiver into a pod that stays Pending.
	for _, c := range decision.DefaultShareClaims.Take(g.Scope, g.AcceleratorType) {
		prev, ok := l.Redirect(c.ID, now, tm.FillTimeout)
		if !ok {
			logger.Info("Utilization share: a wake claimed a transfer that is no longer releasing; ignored",
				"id", c.ID, "wake", c.Wake, "model", c.Model)
			continue
		}
		metrics.CountUtilizationShareTransfer(g.AcceleratorType, scope, string(allocation.ShareOutcomeRedirected), prev.Urgent)
		e.remarkDonorPods(ctx, logger, l, c.ID)
		logger.Info("Utilization share: transfer redirected to a woken model; its receiver is planned again",
			"id", c.ID, "wake", c.Wake, "model", c.Model, "receiver", prev.Receiver, "donor", prev.Donor)
	}
	l.PlannedRunning(e.plannedRunning(ctx, logger, l))
	l.Retain(slices.Collect(maps.Keys(held)), now, tm)
	for _, end := range l.Observe(held, now, tm) {
		t := end.Transfer
		metrics.CountUtilizationShareTransfer(g.AcceleratorType, scope, string(end.Outcome), t.Urgent)
		e.shareEndedEvent(g, end, tm, accessor)
		// A fill timeout leaves the receiver's target: the scheduler places it
		// when it can, and the transfer simply stops counting as committed
		// (§6.3). An aborted release restores the donor.
		if end.Outcome == allocation.ShareOutcomeAborted && t.DonorVariant != "" {
			if t.DonorLowered {
				st.desired[variantKey(t.Donor, t.DonorVariant)]++
			}
			e.unmarkDonorPods(ctx, logger, t)
		}
		if end.Outcome == allocation.ShareOutcomeDone && t.Receiver != "" {
			l.FillBlocked(t.Receiver, "", now) // the receiver fills again
		}
		if end.Outcome == allocation.ShareOutcomeFillTimeout && t.Receiver != "" {
			pending := e.pendingPodGPUs(ctx, accessor(t.Receiver, t.ReceiverVariant), g.Origins[t.Receiver].Namespace)
			reason := shareFillTimeoutCause(g.AcceleratorType, pending, e.shareNodes(now))
			l.FillBlocked(t.Receiver, reason, now.Add(tm.ReleaseTimeout))
			if reason != "" {
				logger.Info("utilization share: the receiver's pods stayed Pending after its GPUs were released",
					"id", t.ID, "receiver", t.Receiver, "reason", reason)
			}
		}
		if end.Outcome == allocation.ShareOutcomeWrongPod {
			// The donor stays lowered: it did shrink. Its surviving planned
			// pods are unmarked, so a later transfer may choose them again.
			logger.Info("utilization share: the donor lost a pod other than the planned one; "+
				"the receiver is not raised into a hole that did not open",
				"id", t.ID, "donor", t.Donor, "planned", t.PlannedPods)
			e.unmarkDonorPods(ctx, logger, t)
		}
		logger.Info("Utilization share: transfer ended", "id", t.ID, "outcome", end.Outcome,
			"donor", t.Donor, "receiver", t.Receiver)
	}
	for _, t := range l.TakeReleased() {
		if t.Donor != "" {
			metrics.ObserveUtilizationShareRelease(g.AcceleratorType, scope, now.Sub(t.Started))
		}
		if t.Donor != "" && t.Receiver == "" && t.SetID == "" {
			logger.Info("Utilization share: reserve refilled", "id", t.ID, "donor", t.Donor, "gpus", t.DonorGPUs)
		}
		if t.Donor != "" && t.ReceiverVariant != "" {
			st.desired[variantKey(t.Receiver, t.ReceiverVariant)]++
			logger.Info("Utilization share: released, raising the receiver", "id", t.ID,
				"receiver", t.Receiver, "variant", t.ReceiverVariant)
		}
	}

	e.reanchorShareTargets(logger, l, g, variantKey, now, tm)

	out := shareActuation{timings: tm, sources: src}
	if now.Before(st.quietUntil[key]) {
		out.overrides = e.quietOverrides(l, g, variantKey)
		out.promised = l.Promised() + l.WakeHeld(now)
		out.reserveDebt = shareDebtNow(l, held, g.Budget)
		out.blocked = shareBlockedReasons(l, g, ev, nil, now, tm)
		out.claimable = shareClaimable(l, g, held)
		out.releasing, out.filling = countInFlight(l)
		return out
	}

	before := map[string]allocation.ShareTransfer{}
	for _, t := range l.Transfers() {
		before[t.ID] = t
	}
	nodes, units, domains := e.shareNodeInputs(ctx, logger, g, accessor, l.Promised()+l.WakeHeld(now),
		shareMarkOwned(l, nil), now)
	plan := allocation.PlanShareTransfers(l, allocation.SharePlanInput{
		Roles: g.Roles, Held: held, Thresholds: g.Thresholds, Budget: g.Budget, Tolerance: us.Tolerance,
		Give: g.Give, Grow: g.Grow, Nodes: nodes, DonorUnits: units, DomainKey: domains,
		WakeHeld: l.WakeHeld(now), PhysicalFree: g.PhysicalFree,
	}, now, tm)
	for _, id := range plan.Cancelled {
		t := before[id]
		if t.DonorLowered {
			st.desired[variantKey(t.Donor, t.DonorVariant)]++
		}
		e.unmarkDonorPods(ctx, logger, t)
		metrics.CountUtilizationShareTransfer(g.AcceleratorType, scope, string(allocation.ShareOutcomeCancelled), t.Urgent)
		logger.Info("Utilization share: transfer cancelled", "id", id, "donor", t.Donor, "receiver", t.Receiver)
	}
	// A donor set (section 6.5) starts whole or not at all: a primary whose
	// contributor could not be marked would raise its receiver into a hole
	// that never fully opens.
	taken := map[string]bool{}
	ownedMark := shareMarkOwned(l, taken)
	for _, set := range shareStartedSets(plan.Started) {
		marked := map[string][]string{}
		markedPods := map[string][]corev1.Pod{}
		var failed error
		failedDonor := ""
		for _, t := range set {
			pods, err := e.markDonorPods(ctx, t, accessor(t.Donor, t.DonorVariant), g.Origins[t.Donor].Namespace, ownedMark)
			markedPods[t.ID], marked[t.ID] = pods, podKeys(pods)
			for _, p := range marked[t.ID] {
				taken[p] = true
			}
			if err != nil {
				failed, failedDonor = err, t.Donor
				break
			}
		}
		if failed != nil {
			// In reverse start order: each member's Start snapshotted the move
			// history its predecessors had already changed.
			for i := len(set) - 1; i >= 0; i-- {
				t := set[i]
				l.Forget(t.ID)
				// The pods as the mark's patch returned them: the cache may
				// not show the mark yet, and an unmark from it would patch
				// nothing.
				for j := range markedPods[t.ID] {
					if err := e.unmarkPod(ctx, &markedPods[t.ID][j]); err != nil {
						logger.Error(err, "could not unmark a donor pod", "pod", marked[t.ID][j])
					}
				}
			}
			// The donor backs off as after an abort: a pod that cannot be
			// marked -- not Ready, all already given -- is the same next
			// cycle, and its receiver would never try another donor.
			l.MarkFailed(failedDonor, now, tm)
			logger.Error(failed, "utilization share could not mark a donor pod; transfer not started",
				"id", set[0].ID, "donors", len(set), "donor", failedDonor)
			continue
		}
		metrics.ObserveUtilizationShareDonorsPerTransfer(g.AcceleratorType, scope, len(set))
		for _, t := range set {
			l.ConfirmStarted(t.ID, marked[t.ID])
			st.desired[variantKey(t.Donor, t.DonorVariant)]--
			e.shareStartedEvents(g, t, accessor)
			logger.Info("Utilization share: transfer started", "id", t.ID, "set", t.SetID, "donor", t.Donor,
				"receiver", t.Receiver, "donorVariant", t.DonorVariant, "receiverVariant", t.ReceiverVariant,
				"urgent", t.Urgent, "pods", marked[t.ID], "planned", len(t.PlannedPods) > 0)
		}
	}

	e.fillIdleShare(logger, l, g, ev, held, shareFit{nodes: plan.Nodes, domains: domains}, variantKey, now, tm)
	out.overrides = e.shareOverrides(g, variantKey, "utilization share")
	out.promised = l.Promised() + l.WakeHeld(now)
	out.swinging = plan.Swinging
	out.reserveDebt = shareDebtNow(l, held, g.Budget)
	out.blocked = shareBlockedReasons(l, g, ev, plan.Unfunded, now, tm)
	out.withheld = plan.Withheld
	out.claimable = shareClaimable(l, g, held)
	out.releasing, out.filling = countInFlight(l)
	return out
}

// fillIdleShare raises receivers below their whole-replica target into the
// group's idle GPUs -- budget no role commits, less what is held for a woken
// model, capped by the cluster's free GPUs -- lowest score first, with no
// transfer and no wait (§6.2), and records who it leaves short
// (ShareLedger.SetFillShort).
//
// With node information (fit.nodes: the free GPUs this cycle's node-aware sets
// left), a replica is filled only where its pods fit the nodes -- in its
// domain, for a receiver with one -- and its placement is spent. That is what
// re-plans a receiver after a donor lost the wrong pod: the GPUs that did come
// free are idle, and they fund it only where its pods fit. A receiver whose pod
// shape is unknown is filled by count. The nodes' free GPUs already exclude
// what is promised or held for a wake, so a promised pod bound but not yet
// held is subtracted twice; that only delays a fill until it is held.
func (e *Engine) fillIdleShare(logger logr.Logger, l *allocation.ShareLedger, g allocation.ShareGroup,
	ev allocation.ShareEvaluation, held map[string]int, fit shareFit, variantKey func(string, string) string,
	now time.Time, tm allocation.ShareTimings) {
	committed := l.Committed(held)
	// GPUs redirected to a woken model are its, however idle they look.
	idle := g.Budget - l.WakeHeld(now)
	for _, c := range committed {
		idle -= c
	}
	idle = min(idle, g.PhysicalFree)
	integer := map[string]int{}
	for _, v := range ev.Roles {
		integer[v.Key] = v.Integer
	}
	roles := map[string]allocation.ShareRole{}
	for _, r := range g.Roles {
		roles[r.Key] = r
	}
	var receivers []string
	for _, r := range g.Roles {
		if _, ok := g.Grow[r.Key]; ok && committed[r.Key] < integer[r.Key] {
			receivers = append(receivers, r.Key)
		}
	}
	z := func(k string) float64 {
		return allocation.ShareScore(float64(committed[k]), roles[k].Need, roles[k].Weight)
	}
	slices.SortFunc(receivers, func(a, b string) int { return cmp.Or(cmp.Compare(z(a), z(b)), cmp.Compare(a, b)) })
	// Whoever is still short when the fill is done is told to the planner.
	defer func() {
		short := slices.DeleteFunc(slices.Clone(receivers), func(rc string) bool { return committed[rc] >= integer[rc] })
		l.SetFillShort(short)
	}()
	if idle <= 0 {
		return
	}
	for _, rc := range receivers {
		grow := g.Grow[rc]
		for n := 0; n < allocation.ShareMaxReplicasPerCycle && idle >= grow.GPUs && committed[rc] < integer[rc]; n++ {
			// A receiver whose pod shape is unknown is filled by count, as
			// without node information: guessing one large pod would refuse
			// a multi-pod replica that fits.
			if fit.nodes != nil && len(grow.PodGPUs) > 0 && !allocation.ShareFitPods(fit.nodes, grow.PodGPUs, fit.domains[rc]) {
				logger.V(logging.DEBUG).Info("Utilization share: idle GPUs do not fit the receiver's pods on any node",
					"receiver", rc, "pods", grow.PodGPUs)
				break
			}
			t := l.StartFill(rc, grow.Name, grow.GPUs, held, now, tm)
			e.utilizationShare.desired[variantKey(rc, grow.Name)]++
			idle -= grow.GPUs
			committed[rc] += grow.GPUs
			logger.Info("Utilization share: filling from idle GPUs", "id", t.ID, "receiver", rc, "variant", grow.Name)
		}
	}
}

// shareOverrides returns each planned variant's desired count, clamped to its
// replica bounds.
func (e *Engine) shareOverrides(g allocation.ShareGroup, variantKey func(string, string) string,
	why string) map[string]utilizationShareOverride {
	out := map[string]utilizationShareOverride{}
	for role, vs := range g.Variants {
		for _, v := range vs {
			k := variantKey(role, v.Name)
			d := max(e.utilizationShare.desired[k], v.Min)
			if v.Max > 0 {
				d = min(d, v.Max)
			}
			e.utilizationShare.desired[k] = d
			out[k] = utilizationShareOverride{Target: d, Why: why}
		}
	}
	return out
}

// quietOverrides are a group's targets while its ledger is quiet after a
// (re)start: the variants a restored transfer moves keep the targets the
// restore set, and every other variant is left to today's optimizer, its
// desired count following what it runs -- a model must not be frozen for a
// fill timeout because the controller restarted.
func (e *Engine) quietOverrides(l *allocation.ShareLedger, g allocation.ShareGroup,
	variantKey func(string, string) string) map[string]utilizationShareOverride {
	touched := map[string]bool{}
	for _, t := range l.Transfers() {
		touched[variantKey(t.Donor, t.DonorVariant)] = true
		if t.Receiver != "" {
			touched[variantKey(t.Receiver, t.ReceiverVariant)] = true
		}
	}
	all := e.shareOverrides(g, variantKey, "utilization share: restored transfer in flight")
	out := map[string]utilizationShareOverride{}
	for role, vs := range g.Variants {
		for _, v := range vs {
			k := variantKey(role, v.Name)
			if touched[k] {
				out[k] = all[k]
			} else {
				e.utilizationShare.desired[k] = v.Current
			}
		}
	}
	return out
}

// shareTimingInputs collects the slowest donor configuration of a group: the
// longest scale-down window, polling interval and termination grace among the
// variants that can give, from their ScaledObjects and pod templates.
func (e *Engine) shareTimingInputs(g allocation.ShareGroup,
	accessor func(role, variant string) scaletarget.ScaleTargetAccessor) allocation.ShareTimingInputs {
	in := allocation.ShareTimingInputs{Cycle: e.optimizeInterval()}
	maxDur := func(cur **time.Duration, d time.Duration) {
		if *cur == nil || d > **cur {
			v := d
			*cur = &v
		}
	}
	for role, give := range g.Give {
		acc := accessor(role, give.Name)
		if acc == nil {
			continue
		}
		if acc.GetGroupSize() > 1 {
			in.LWS = true
		}
		for _, tpl := range []*corev1.PodTemplateSpec{acc.GetLeaderPodTemplateSpec(), acc.GetWorkerPodTemplateSpec()} {
			if tpl != nil && tpl.Spec.TerminationGracePeriodSeconds != nil {
				maxDur(&in.TerminationGrace, time.Duration(*tpl.Spec.TerminationGracePeriodSeconds)*time.Second)
			}
		}
		if e.Variants == nil {
			continue
		}
		ns := g.Origins[role].Namespace
		entry, ok := e.Variants.FindByScaleTarget(ns, constants.DeploymentKind, acc.GetName())
		if !ok {
			entry, ok = e.Variants.FindByScaleTarget(ns, constants.LeaderWorkerSetKind, acc.GetName())
		}
		if !ok {
			continue
		}
		if w := entry.Target.ScaleDownWindowSeconds; w != nil {
			maxDur(&in.ScaleDownWindow, time.Duration(*w)*time.Second)
		}
		if p := entry.Target.PollingIntervalSeconds; p != nil {
			maxDur(&in.PollingInterval, time.Duration(*p)*time.Second)
		}
	}
	return in
}

func (e *Engine) optimizeInterval() time.Duration {
	if e.Config != nil {
		if d := e.Config.OptimizationInterval(); d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// applyUtilizationShareOverrides sets each planned variant's decision to the
// utilization-share target, under the optimizer's own reason so the sticky
// scale-down hold stands down for it (§6.3). A planned variant this cycle's
// optimizer made no decision for is left alone: there is nothing to carry the
// target, and the next cycle will have one.
func applyUtilizationShareOverrides(decisions []domain.VariantDecision, overrides map[string]utilizationShareOverride) int {
	applied := 0
	for i := range decisions {
		d := &decisions[i]
		o, ok := overrides[utils.GetNamespacedKey(d.Namespace, d.VariantName)]
		if !ok {
			continue
		}
		action := domain.ActionNoChange
		switch {
		case o.Target > d.CurrentReplicas:
			action = domain.ActionScaleUp
		case o.Target < d.CurrentReplicas:
			action = domain.ActionScaleDown
		}
		was := d.TargetReplicas
		d.TargetReplicas = o.Target
		d.WasLimited = false
		d.SetDecisionReason(action, domain.DecisionReasonUtilizationShare, o.Why)
		d.AddDecisionStep("utilization-share", fmt.Sprintf("%s: target %d (today's optimizer: %d)", o.Why, o.Target, was), false)
		applied++
	}
	return applied
}

// reanchorShareTargets resets a planned variant's target to what is running
// when the two have disagreed, with no transfer in flight to explain it, for
// longer than the release timeout. A target only moves by the ledger's own
// steps, so without this anything that keeps a variant off its target -- a
// ResourceQuota that denies the pod, a ScaledObject ceiling below it, a
// controller that scales it to zero -- would leave the optimizer planning on a
// count that does not exist, for as long as it is selected.
func (e *Engine) reanchorShareTargets(logger logr.Logger, l *allocation.ShareLedger, g allocation.ShareGroup,
	variantKey func(string, string) string, now time.Time, tm allocation.ShareTimings) {
	st := &e.utilizationShare
	inFlight := map[string]bool{}
	for _, t := range l.Transfers() {
		if t.DonorVariant != "" {
			inFlight[variantKey(t.Donor, t.DonorVariant)] = true
		}
		if t.ReceiverVariant != "" {
			inFlight[variantKey(t.Receiver, t.ReceiverVariant)] = true
		}
	}
	for role, vs := range g.Variants {
		for _, v := range vs {
			k := variantKey(role, v.Name)
			if inFlight[k] || st.desired[k] == v.Current {
				delete(st.divergedSince, k)
				continue
			}
			since, ok := st.divergedSince[k]
			if !ok {
				st.divergedSince[k] = now
				continue
			}
			if now.Sub(since) >= tm.ReleaseTimeout {
				logger.Info("utilization share target re-anchored to the running count; "+
					"something outside the optimizer kept the variant off its target",
					"variant", k, "target", st.desired[k], "running", v.Current, "since", since)
				st.desired[k] = v.Current
				delete(st.divergedSince, k)
			}
		}
	}
}

// decideV2 is optimizeV2's decision step: today's optimizer decides, and the
// utilization-share optimizer runs beside it. In shadow mode it publishes what
// it would do and those decisions stand; active, it owns the targets of the
// variants it plans and today's decisions keep the rest.
func (e *Engine) decideV2(ctx context.Context, optimizer allocation.ScalingOptimizer,
	requests []allocation.ModelScalingRequest, constraints []*allocation.ResourceConstraints,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor) []domain.VariantDecision {
	decisions := optimizer.Optimize(ctx, requests, constraints)
	if overrides := e.evaluateUtilizationShare(ctx, requests, constraints, scaleTargets); len(overrides) > 0 {
		applied := applyUtilizationShareOverrides(decisions, overrides)
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Utilization share set planned targets", "variants", applied)
	}
	// Republish the warm pool's headroom with this pass's promises withheld,
	// so a transfer that started filling this cycle is not open to the pool
	// until the next one (section 6.3).
	if len(constraints) > 0 {
		now := time.Now()
		allocation.PublishNamespaceHeadroom(allocation.WithholdPromised(constraints, decision.LatestSharePromised(now)), now)
	}
	return decisions
}

// shareDebtNow is the reserve debt the ledger's committed allocation carries
// against a budget: allocation.ShareDebt over this cycle's commitments.
func shareDebtNow(l *allocation.ShareLedger, held map[string]int, budget int) int {
	return allocation.ShareDebt(l.Committed(held), budget)
}

// shareBlockedReasons is each planned model's utilization-share blocked
// reasons (section 9), keyed by namespace/model. Every planned model has an
// entry, empty when nothing holds it back, so a reason that stops holding is
// cleared.
func shareBlockedReasons(l *allocation.ShareLedger, g allocation.ShareGroup, ev allocation.ShareEvaluation,
	unfunded map[string]string, now time.Time, tm allocation.ShareTimings) map[string][]string {
	out := map[string][]string{}
	model := func(role string) string {
		o := g.Origins[role]
		return utils.GetNamespacedKey(o.Namespace, o.ModelID)
	}
	add := func(role, reason string) {
		k := model(role)
		if !slices.Contains(out[k], reason) {
			out[k] = append(out[k], reason)
		}
	}
	floors := 0
	for _, r := range g.Roles {
		out[model(r.Key)] = out[model(r.Key)][:0:0]
		floors += r.Floor
	}
	// donorsAtFloor: every role other than skip holds no more than its floor.
	donorsAtFloor := func(skip string) bool {
		for _, o := range g.Roles {
			if o.Key != skip && g.Committed[o.Key] > o.Floor {
				return false
			}
		}
		return len(g.Roles) > 1
	}
	verdict := map[string]allocation.ShareRoleVerdict{}
	for _, v := range ev.Roles {
		verdict[v.Key] = v
	}
	receiving := map[string]bool{}
	for _, t := range l.Transfers() {
		if t.Receiver != "" {
			receiving[t.Receiver] = true
		}
	}
	for _, r := range g.Roles {
		if floors > g.Budget {
			add(r.Key, constants.ScalingBlockedFloorsExceedQuota)
		}
		if float64(r.Floor)-r.Need >= float64(max(r.ReplicaGPUs, 1)) {
			add(r.Key, constants.ScalingBlockedFloorPinned)
		}
		v := verdict[r.Key]
		if v.Headroom < 0 && ev.Spare < 0 {
			add(r.Key, constants.ScalingBlockedQuotaShort)
		}
		// Not "out of band": the targets already absorb floors, so a role
		// whose GPUs are all pinned elsewhere sits in band at its shortfall.
		// What the operator needs is the cause -- short, and nobody can give.
		if v.Headroom < 0 && donorsAtFloor(r.Key) {
			add(r.Key, constants.ScalingBlockedDonorsAtFloor)
		}
		switch {
		case l.Unsteerable(r.Key, now):
			add(r.Key, constants.ScalingBlockedDonorNotSteerable)
		case l.BackingOff(r.Key, now):
			add(r.Key, constants.ScalingBlockedReleaseTimeout)
		}
		if l.Swinging(r.Key, now) {
			add(r.Key, constants.ScalingBlockedSwinging)
		}
		// What keeps a short role from receiving, when something does.
		if v.Headroom < 0 && v.Actionable && !receiving[r.Key] {
			switch {
			case l.ReceivingHeld(r.Key, now, tm):
				add(r.Key, constants.ScalingBlockedReversalHold)
			case l.InFlight() >= allocation.ShareMaxConcurrentTransfers:
				add(r.Key, constants.ScalingBlockedTransferLimit)
			}
		}
		if reason := l.FillBlockedReason(r.Key, now); reason != "" {
			add(r.Key, reason)
		}
		if unfunded[r.Key] == allocation.ShareUnfundedNoCompatibleDonor {
			add(r.Key, constants.ScalingBlockedNoCompatibleDonor)
		}
	}
	for _, t := range l.Transfers() {
		if t.State == allocation.ShareReleasing && t.Receiver != "" {
			if _, planned := g.Origins[t.Receiver]; planned {
				add(t.Receiver, constants.ScalingBlockedAwaitingRelease)
			}
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

// shareClaimable lists the group's transfers a wake may claim: still
// releasing, for a receiver, each with the receiver's score at what it holds
// now and the shape of the donor pods it releases (section 6.3).
func shareClaimable(l *allocation.ShareLedger, g allocation.ShareGroup, held map[string]int) []decision.ShareClaimable {
	roles := map[string]allocation.ShareRole{}
	for _, r := range g.Roles {
		roles[r.Key] = r
	}
	var out []decision.ShareClaimable
	for _, t := range l.Transfers() {
		// A set member's hole funds its receiver together with the others.
		if t.State != allocation.ShareReleasing || t.Receiver == "" || t.SetID != "" {
			continue
		}
		r, ok := roles[t.Receiver]
		if !ok {
			continue
		}
		var pods []int
		for _, v := range g.Variants[t.Donor] {
			if v.Name == t.DonorVariant {
				pods = v.PodGPUs
			}
		}
		out = append(out, decision.ShareClaimable{
			ID:           t.ID,
			ReceiverZ:    allocation.ShareScore(float64(held[t.Receiver]), r.Need, r.Weight),
			DonorPodGPUs: pods,
			DonorGPUs:    t.DonorGPUs,
		})
	}
	return out
}

// shareStartedSets groups this cycle's started transfers into what must start
// together: each donor set's members, primary first, and each single transfer
// alone.
func shareStartedSets(started []allocation.ShareTransfer) [][]allocation.ShareTransfer {
	var out [][]allocation.ShareTransfer
	index := map[string]int{}
	for _, t := range started {
		if t.SetID == "" {
			out = append(out, []allocation.ShareTransfer{t})
			continue
		}
		i, ok := index[t.SetID]
		if !ok {
			i = len(out)
			index[t.SetID] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], t)
	}
	for _, set := range out {
		// The primary first; the contributors keep their start order.
		slices.SortStableFunc(set, func(a, b allocation.ShareTransfer) int {
			switch {
			case a.IsSetPrimary() == b.IsSetPrimary():
				return 0
			case a.IsSetPrimary():
				return -1
			default:
				return 1
			}
		})
	}
	return out
}
