package steadystate

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

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
	podDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
	// donorDeletionCost is below the default of every unmarked sibling.
	donorDeletionCost = "-1000"
)

// transferMark is a transfer as its donor pods record it.
type transferMark struct {
	ID              string    `json:"id"`
	Donor           string    `json:"donor"`
	Receiver        string    `json:"receiver"`
	DonorVariant    string    `json:"donorVariant"`
	ReceiverVariant string    `json:"receiverVariant"`
	GPUs            int       `json:"gpus"`
	DonorGPUs       int       `json:"donorGPUs"`
	Started         time.Time `json:"started"`
}

// shareActuation is what one active cycle of a group did, for the caller to
// apply and publish.
type shareActuation struct {
	overrides map[string]utilizationShareOverride
	promised  int
	timings   allocation.ShareTimings
	sources   allocation.ShareTimingSource
	swinging  []string
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
	scaleTargets map[string]map[string]scaletarget.ScaleTargetAccessor, now time.Time) shareActuation {
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
		o := g.Origins[role]
		return scaleTargets[utils.GetNamespacedKey(o.Namespace, o.ModelID)][variant]
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
		st.ledgers[key] = l
		restored := e.restoreShareTransfers(ctx, logger, l, g, accessor, variantKey, now, tm)
		// After a restart a transfer in Filling has no mark left, and its
		// receiver may have no pods yet: plan nothing for one fill timeout.
		st.quietUntil[key] = now.Add(tm.FillTimeout)
		logger.Info("Utilization share: ledger started", "restoredTransfers", restored,
			"planningFrom", st.quietUntil[key], "timings", src)
	}

	held := g.Committed
	for _, end := range l.Observe(held, now, tm) {
		t := end.Transfer
		metrics.CountUtilizationShareTransfer(g.AcceleratorType, scope, string(end.Outcome), t.Urgent)
		// A fill timeout leaves the receiver's target: the scheduler places it
		// when it can, and the transfer simply stops counting as committed
		// (§6.3). An aborted release restores the donor.
		if end.Outcome == allocation.ShareOutcomeAborted && t.DonorVariant != "" {
			st.desired[variantKey(t.Donor, t.DonorVariant)]++
			e.unmarkDonorPods(ctx, logger, t)
		}
		logger.Info("Utilization share: transfer ended", "id", t.ID, "outcome", end.Outcome,
			"donor", t.Donor, "receiver", t.Receiver)
	}
	for _, t := range l.TakeReleased() {
		if t.Donor != "" {
			metrics.ObserveUtilizationShareRelease(g.AcceleratorType, scope, now.Sub(t.Started))
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
		out.overrides = e.shareOverrides(g, variantKey, "restart quiet period")
		out.promised = l.Promised()
		return out
	}

	before := map[string]allocation.ShareTransfer{}
	for _, t := range l.Transfers() {
		before[t.ID] = t
	}
	plan := allocation.PlanShareTransfers(l, allocation.SharePlanInput{
		Roles: g.Roles, Held: held, Thresholds: g.Thresholds, Budget: g.Budget, Tolerance: us.Tolerance,
		Give: g.Give, Grow: g.Grow,
	}, now, tm)
	for _, id := range plan.Cancelled {
		t := before[id]
		st.desired[variantKey(t.Donor, t.DonorVariant)]++
		e.unmarkDonorPods(ctx, logger, t)
		metrics.CountUtilizationShareTransfer(g.AcceleratorType, scope, string(allocation.ShareOutcomeCancelled), t.Urgent)
		logger.Info("Utilization share: transfer cancelled", "id", id, "donor", t.Donor, "receiver", t.Receiver)
	}
	for _, t := range plan.Started {
		pods, err := e.markDonorPods(ctx, t, accessor(t.Donor, t.DonorVariant), g.Origins[t.Donor].Namespace)
		if err != nil {
			l.Forget(t.ID)
			e.unmarkDonorPods(ctx, logger, allocation.ShareTransfer{DonorPods: pods})
			logger.Info("WARNING: utilization share could not mark a donor pod; transfer not started",
				"id", t.ID, "donor", t.Donor, "error", err.Error())
			continue
		}
		l.SetDonorPods(t.ID, pods)
		st.desired[variantKey(t.Donor, t.DonorVariant)]--
		logger.Info("Utilization share: transfer started", "id", t.ID, "donor", t.Donor, "receiver", t.Receiver,
			"donorVariant", t.DonorVariant, "receiverVariant", t.ReceiverVariant, "urgent", t.Urgent, "pods", pods)
	}

	e.fillIdleShare(logger, l, g, ev, held, variantKey, now, tm)
	out.overrides = e.shareOverrides(g, variantKey, "utilization share")
	out.promised = l.Promised()
	out.swinging = plan.Swinging
	return out
}

// fillIdleShare raises receivers below their whole-replica target into GPUs
// nobody holds or has been promised, with no transfer and no wait (§6.2).
func (e *Engine) fillIdleShare(logger logr.Logger, l *allocation.ShareLedger, g allocation.ShareGroup,
	ev allocation.ShareEvaluation, held map[string]int, variantKey func(string, string) string,
	now time.Time, tm allocation.ShareTimings) {
	committed := l.Committed(held)
	idle := g.Budget
	for _, c := range committed {
		idle -= c
	}
	idle = min(idle, g.PhysicalFree)
	if idle <= 0 {
		return
	}
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
	for _, rc := range receivers {
		grow := g.Grow[rc]
		for n := 0; n < allocation.ShareMaxReplicasPerCycle && idle >= grow.GPUs && committed[rc] < integer[rc]; n++ {
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

// donorPods lists the pods a donor variant would release: on a Deployment, its
// ready pods not being deleted; on a LeaderWorkerSet, the pods of its
// highest-index group, which LWS removes first and does not steer by cost.
func (e *Engine) donorPods(ctx context.Context, acc scaletarget.ScaleTargetAccessor, namespace string) ([]corev1.Pod, error) {
	var sel client.MatchingLabels
	lws := acc.GetGroupSize() > 1
	if lws {
		sel = client.MatchingLabels{lwsv1.SetNameLabelKey: acc.GetName()}
	} else {
		tpl := acc.GetLeaderPodTemplateSpec()
		if tpl == nil || len(tpl.Labels) == 0 {
			return nil, fmt.Errorf("variant %s has no pod template labels", acc.GetName())
		}
		sel = client.MatchingLabels(tpl.Labels)
	}
	var list corev1.PodList
	if err := e.client.List(ctx, &list, client.InNamespace(namespace), sel); err != nil {
		return nil, err
	}
	var pods []corev1.Pod
	maxGroup := -1
	for _, p := range list.Items {
		if p.DeletionTimestamp != nil || p.Spec.NodeName == "" {
			continue
		}
		if lws {
			if i, err := strconv.Atoi(p.Labels[lwsv1.GroupIndexLabelKey]); err == nil && i > maxGroup {
				maxGroup = i
			}
		}
		pods = append(pods, p)
	}
	if lws {
		pods = slices.DeleteFunc(pods, func(p corev1.Pod) bool {
			return p.Labels[lwsv1.GroupIndexLabelKey] != strconv.Itoa(maxGroup)
		})
	}
	return pods, nil
}

// markDonorPods marks the pod a transfer's donor gives: the lowest deletion
// cost, so the ReplicaSet removes it and not a sibling, and the transfer
// annotation, so a restarted controller finds it (§6.3, §6.5). It prefers a
// Ready pod: the ReplicaSet removes not-ready pods before it consults the
// cost, so a donor with one cannot be steered.
func (e *Engine) markDonorPods(ctx context.Context, t allocation.ShareTransfer,
	acc scaletarget.ScaleTargetAccessor, namespace string) ([]string, error) {
	if e.client == nil || acc == nil {
		return nil, fmt.Errorf("no client or scale target for donor variant %q", t.DonorVariant)
	}
	pods, err := e.donorPods(ctx, acc, namespace)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("donor variant %q has no pod to give", t.DonorVariant)
	}
	if acc.GetGroupSize() <= 1 {
		for _, p := range pods {
			if !podReady(&p) {
				return nil, fmt.Errorf("donor variant %q has a pod that is not ready; its victim cannot be steered", t.DonorVariant)
			}
		}
		slices.SortFunc(pods, func(a, b corev1.Pod) int { return cmp.Compare(a.Name, b.Name) })
		pods = pods[:1]
	}
	raw, err := json.Marshal(transferMark{ID: t.ID, Donor: t.Donor, Receiver: t.Receiver, DonorVariant: t.DonorVariant,
		ReceiverVariant: t.ReceiverVariant, GPUs: t.GPUs, DonorGPUs: t.DonorGPUs, Started: t.Started})
	if err != nil {
		return nil, err
	}
	var marked []string
	for i := range pods {
		p := &pods[i]
		patch := client.MergeFrom(p.DeepCopy())
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[podDeletionCostAnnotation] = donorDeletionCost
		p.Annotations[utilizationShareTransferAnnotation] = string(raw)
		if err := e.client.Patch(ctx, p, patch); err != nil {
			return marked, err
		}
		marked = append(marked, utils.GetNamespacedKey(p.Namespace, p.Name))
	}
	return marked, nil
}

// unmarkDonorPods removes a cancelled or aborted transfer's marks, so the pods
// are no longer first in line and no restart resurrects the transfer.
func (e *Engine) unmarkDonorPods(ctx context.Context, logger logr.Logger, t allocation.ShareTransfer) {
	if e.client == nil {
		return
	}
	for _, key := range t.DonorPods {
		ns, name, _ := strings.Cut(key, "/")
		var p corev1.Pod
		if err := e.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
			continue // gone: nothing to unmark
		}
		patch := client.MergeFrom(p.DeepCopy())
		delete(p.Annotations, podDeletionCostAnnotation)
		delete(p.Annotations, utilizationShareTransferAnnotation)
		if err := e.client.Patch(ctx, &p, patch); err != nil {
			logger.Info("WARNING: could not unmark a donor pod", "pod", key, "error", err.Error())
		}
	}
}

// restoreShareTransfers rebuilds a group's Releasing transfers from its donor
// pods' marks after a restart (§6.3). A mark older than the release timeout
// is stale and removed. A donor whose marked pod is not yet terminating is
// still counted in status.replicas, so its desired count is lowered to keep
// the release going.
func (e *Engine) restoreShareTransfers(ctx context.Context, logger logr.Logger, l *allocation.ShareLedger,
	g allocation.ShareGroup, accessor func(string, string) scaletarget.ScaleTargetAccessor,
	variantKey func(string, string) string, now time.Time, tm allocation.ShareTimings) int {
	if e.client == nil {
		return 0
	}
	restored := 0
	seen := map[string]bool{}
	for role, give := range g.Give {
		acc := accessor(role, give.Name)
		if acc == nil {
			continue
		}
		var list corev1.PodList
		if err := e.client.List(ctx, &list, client.InNamespace(g.Origins[role].Namespace)); err != nil {
			continue
		}
		for _, p := range list.Items {
			raw, ok := p.Annotations[utilizationShareTransferAnnotation]
			if !ok {
				continue
			}
			var m transferMark
			if json.Unmarshal([]byte(raw), &m) != nil || m.Donor != role || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			if now.Sub(m.Started) >= tm.ReleaseTimeout {
				e.unmarkDonorPods(ctx, logger, allocation.ShareTransfer{DonorPods: []string{utils.GetNamespacedKey(p.Namespace, p.Name)}})
				continue
			}
			l.Restore(allocation.ShareTransfer{
				ID: m.ID, Donor: m.Donor, Receiver: m.Receiver, DonorVariant: m.DonorVariant,
				ReceiverVariant: m.ReceiverVariant, GPUs: m.GPUs, DonorGPUs: m.DonorGPUs, Started: m.Started,
				Deadline: m.Started.Add(tm.ReleaseTimeout), DonorPods: []string{utils.GetNamespacedKey(p.Namespace, p.Name)},
			}, g.Committed[m.Donor])
			if p.DeletionTimestamp == nil {
				e.utilizationShare.desired[variantKey(m.Donor, m.DonorVariant)]--
			}
			restored++
		}
	}
	return restored
}

// podReady reports whether a pod's Ready condition is true.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
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
				logger.Info("WARNING: utilization share target re-anchored to the running count; "+
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
	scaleTargets map[string]map[string]scaletarget.ScaleTargetAccessor) []domain.VariantDecision {
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
		if p := decision.LatestSharePromised(now); len(p) > 0 {
			allocation.PublishNamespaceHeadroom(allocation.WithholdPromised(constraints, p), now)
		}
	}
	return decisions
}
