package steadystate

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/variantmeta"
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
	// SetID links the members of a donor set (section 6.5).
	SetID string `json:"setID,omitempty"`
}

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
			logger.Info("WARNING: utilization share could not read transfer marks; retrying next cycle",
				"error", err.Error())
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
	for _, c := range decision.DefaultShareClaims.Take(g.Scope, g.AcceleratorType) {
		prev, ok := l.Redirect(c.ID)
		if !ok {
			logger.Info("Utilization share: a wake claimed a transfer that is no longer releasing; ignored",
				"id", c.ID, "wake", c.Wake)
			continue
		}
		metrics.CountUtilizationShareTransfer(g.AcceleratorType, scope, string(allocation.ShareOutcomeRedirected), prev.Urgent)
		e.remarkDonorPods(ctx, logger, l, c.ID)
		logger.Info("Utilization share: transfer redirected to a woken model; its receiver is planned again",
			"id", c.ID, "wake", c.Wake, "receiver", prev.Receiver, "donor", prev.Donor)
	}
	for _, end := range l.Observe(held, now, tm) {
		t := end.Transfer
		metrics.CountUtilizationShareTransfer(g.AcceleratorType, scope, string(end.Outcome), t.Urgent)
		// A fill timeout leaves the receiver's target: the scheduler places it
		// when it can, and the transfer simply stops counting as committed
		// (§6.3). An aborted release restores the donor.
		if end.Outcome == allocation.ShareOutcomeAborted && t.DonorVariant != "" {
			if t.DonorLowered {
				st.desired[variantKey(t.Donor, t.DonorVariant)]++
			}
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
		out.overrides = e.shareOverrides(g, variantKey, "restart quiet period")
		out.promised = l.Promised()
		out.reserveDebt = shareDebtNow(l, held, g.Budget)
		out.blocked = shareBlockedReasons(l, g, ev, nil, now)
		out.claimable = shareClaimable(l, g, held)
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
	for _, set := range shareStartedSets(plan.Started) {
		marked := map[string][]string{}
		var failed error
		for _, t := range set {
			pods, err := e.markDonorPods(ctx, t, accessor(t.Donor, t.DonorVariant), g.Origins[t.Donor].Namespace)
			marked[t.ID] = pods
			if err != nil {
				failed = err
				break
			}
		}
		if failed != nil {
			for _, t := range set {
				l.Forget(t.ID)
				e.unmarkDonorPods(ctx, logger, allocation.ShareTransfer{ID: t.ID, DonorPods: marked[t.ID]})
			}
			logger.Info("WARNING: utilization share could not mark a donor pod; transfer not started",
				"id", set[0].ID, "donors", len(set), "error", failed.Error())
			continue
		}
		metrics.ObserveUtilizationShareDonorsPerTransfer(g.AcceleratorType, scope, len(set))
		for _, t := range set {
			l.ConfirmStarted(t.ID, marked[t.ID])
			st.desired[variantKey(t.Donor, t.DonorVariant)]--
			logger.Info("Utilization share: transfer started", "id", t.ID, "set", t.SetID, "donor", t.Donor,
				"receiver", t.Receiver, "donorVariant", t.DonorVariant, "receiverVariant", t.ReceiverVariant,
				"urgent", t.Urgent, "pods", marked[t.ID])
		}
	}

	e.fillIdleShare(logger, l, g, ev, held, variantKey, now, tm)
	out.overrides = e.shareOverrides(g, variantKey, "utilization share")
	out.promised = l.Promised()
	out.swinging = plan.Swinging
	out.reserveDebt = shareDebtNow(l, held, g.Budget)
	out.blocked = shareBlockedReasons(l, g, ev, plan.Unfunded, now)
	out.withheld = plan.Withheld
	out.claimable = shareClaimable(l, g, held)
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
	lws := scaletarget.IsLeaderWorkerSet(acc)
	list, err := variantmeta.ListVariantPods(ctx, e.client, namespace, acc)
	if err != nil {
		return nil, err
	}
	var pods []corev1.Pod
	maxGroup := -1
	for _, p := range list {
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
		if maxGroup < 0 {
			return nil, fmt.Errorf("no pod of %s carries a group index", acc.GetName())
		}
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
	if !scaletarget.IsLeaderWorkerSet(acc) {
		for _, p := range pods {
			if !variantmeta.PodReady(&p) {
				return nil, fmt.Errorf("donor variant %q has a pod that is not ready; its victim cannot be steered", t.DonorVariant)
			}
		}
	}
	// A pod another transfer has marked is that transfer's: a second
	// concurrent transfer from the same donor gives a different pod, or none.
	pods = slices.DeleteFunc(pods, func(p corev1.Pod) bool {
		_, marked := p.Annotations[utilizationShareTransferAnnotation]
		return marked
	})
	if len(pods) == 0 {
		return nil, fmt.Errorf("donor variant %q has no unmarked pod to give", t.DonorVariant)
	}
	if !scaletarget.IsLeaderWorkerSet(acc) {
		slices.SortFunc(pods, func(a, b corev1.Pod) int { return cmp.Compare(a.Name, b.Name) })
		pods = pods[:1]
	}
	raw, err := json.Marshal(transferMark{ID: t.ID, Donor: t.Donor, Receiver: t.Receiver, DonorVariant: t.DonorVariant,
		ReceiverVariant: t.ReceiverVariant, GPUs: t.GPUs, DonorGPUs: t.DonorGPUs, Started: t.Started, SetID: t.SetID})
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
			if !apierrors.IsNotFound(err) {
				// Not "gone": the mark stays, and a restart inside the
				// release timeout would restore the transfer. Say so.
				logger.Info("WARNING: could not read a donor pod to unmark it", "pod", key, "error", err.Error())
			}
			continue
		}
		if t.ID != "" && !markedFor(&p, t.ID) {
			continue // marked for another transfer since: not this one's to clear
		}
		patch := client.MergeFrom(p.DeepCopy())
		delete(p.Annotations, podDeletionCostAnnotation)
		delete(p.Annotations, utilizationShareTransferAnnotation)
		if err := e.client.Patch(ctx, &p, patch); err != nil {
			logger.Info("WARNING: could not unmark a donor pod", "pod", key, "error", err.Error())
		}
	}
}

// markedFor reports whether a pod's transfer mark names the transfer id.
func markedFor(p *corev1.Pod, id string) bool {
	var m transferMark
	return json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m) == nil && m.ID == id
}

// restoreShareTransfers rebuilds a group's Releasing transfers from its donor
// pods' marks after a restart (§6.3).
//
// A mark sits on a pod in the donor's namespace, which its tenant may be able
// to write, so it is read only from the donor variant's own pods and is
// believed only when it matches what this controller would have written for
// that group now: the donor role and variant it is found under, a receiver
// role and variant of the same group, the replica sizes of both, and a start
// in the past. Anything else is removed with a WARN. A mark older than the
// release timeout is stale and removed quietly.
//
// The pods of one transfer -- every pod of an LWS group -- are restored as one
// transfer. If any of them is not yet terminating, the donor is still counted
// in status.replicas and its target is lowered again to keep the release
// going; DonorLowered records that, so a later cancel or abort raises only
// what was lowered.
//
// An error listing a donor's pods is returned before anything is applied.
func (e *Engine) restoreShareTransfers(ctx context.Context, logger logr.Logger, l *allocation.ShareLedger,
	g allocation.ShareGroup, accessor func(string, string) scaletarget.ScaleTargetAccessor,
	variantKey func(string, string) string, now time.Time, tm allocation.ShareTimings) (int, error) {
	if e.client == nil {
		return 0, nil
	}
	type found struct {
		mark transferMark
		pods []string
		live bool
	}
	byID := map[string]*found{}
	var order []string
	var invalid []string
	for _, role := range slices.Sorted(maps.Keys(g.Give)) {
		give := g.Give[role]
		acc := accessor(role, give.Name)
		if acc == nil {
			continue
		}
		pods, err := variantmeta.ListVariantPods(ctx, e.client, g.Origins[role].Namespace, acc)
		if err != nil {
			return 0, fmt.Errorf("list the pods of donor variant %s: %w", give.Name, err)
		}
		for _, p := range pods {
			raw, ok := p.Annotations[utilizationShareTransferAnnotation]
			if !ok {
				continue
			}
			key := utils.GetNamespacedKey(p.Namespace, p.Name)
			var m transferMark
			if why := validTransferMark(raw, &m, role, give, g, now); why != "" {
				logger.Info("WARNING: utilization share removed a transfer mark it did not write", "pod", key, "reason", why)
				invalid = append(invalid, key)
				continue
			}
			if now.Sub(m.Started) >= tm.ReleaseTimeout {
				invalid = append(invalid, key)
				continue
			}
			id := role + "|" + m.ID
			f, ok := byID[id]
			if !ok {
				f = &found{mark: m}
				byID[id] = f
				order = append(order, id)
			}
			f.pods = append(f.pods, key)
			f.live = f.live || p.DeletionTimestamp == nil
		}
	}
	e.unmarkDonorPods(ctx, logger, allocation.ShareTransfer{DonorPods: invalid})
	for _, id := range order {
		f := byID[id]
		m := f.mark
		l.Restore(allocation.ShareTransfer{
			ID: m.ID, Donor: m.Donor, Receiver: m.Receiver, DonorVariant: m.DonorVariant,
			ReceiverVariant: m.ReceiverVariant, GPUs: m.GPUs, DonorGPUs: m.DonorGPUs, Started: m.Started,
			Deadline: m.Started.Add(tm.ReleaseTimeout), DonorPods: f.pods, DonorLowered: f.live, SetID: m.SetID,
		}, g.Committed[m.Donor])
		if f.live {
			e.utilizationShare.desired[variantKey(m.Donor, m.DonorVariant)]--
		}
	}
	return len(order), nil
}

// validTransferMark parses a mark into m and reports why it is not one this
// controller would have written for donor role and its giving variant, or "".
func validTransferMark(raw string, m *transferMark, role string, give allocation.ShareVariant,
	g allocation.ShareGroup, now time.Time) string {
	if err := json.Unmarshal([]byte(raw), m); err != nil {
		return "unparseable"
	}
	grow, ok := g.Grow[m.Receiver]
	refill := m.Receiver == "" && m.ReceiverVariant == "" && m.GPUs == 0
	switch {
	case m.ID == "":
		return "no id"
	case m.Donor != role || m.DonorVariant != give.Name:
		return "donor is not the variant the mark is on"
	case refill:
		if m.DonorGPUs != max(give.GPUs, 1) {
			return "replica sizes do not match the variants"
		}
		if m.Started.After(now) {
			return "starts in the future"
		}
		return ""
	case !ok || m.Receiver == role || m.ReceiverVariant != grow.Name:
		return "receiver is not a variant of this group that can grow"
	case m.GPUs != max(grow.GPUs, 1) || (m.SetID == "" && m.DonorGPUs != max(give.GPUs, 1, m.GPUs)) ||
		(m.SetID != "" && m.DonorGPUs != max(give.GPUs, 1)):
		// A single donor gives at least the receiver replica; a set member
		// gives one donor replica of several.
		return "replica sizes do not match the variants"
	case m.Started.After(now):
		return "starts in the future"
	}
	return ""
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
	unfunded map[string]string, now time.Time) map[string][]string {
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
		if l.BackingOff(r.Key, now) {
			add(r.Key, constants.ScalingBlockedReleaseTimeout)
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
		if t.State != allocation.ShareReleasing || t.Receiver == "" {
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

// remarkDonorPods rewrites a redirected transfer's marks on its donor pods, so
// a restart restores it as the release-with-no-receiver it now is, not as the
// transfer to its original receiver.
func (e *Engine) remarkDonorPods(ctx context.Context, logger logr.Logger, l *allocation.ShareLedger, id string) {
	if e.client == nil {
		return
	}
	i := slices.IndexFunc(l.Transfers(), func(t allocation.ShareTransfer) bool { return t.ID == id })
	if i < 0 {
		return
	}
	t := l.Transfers()[i]
	raw, err := json.Marshal(transferMark{ID: t.ID, Donor: t.Donor, DonorVariant: t.DonorVariant,
		DonorGPUs: t.DonorGPUs, Started: t.Started})
	if err != nil {
		return
	}
	for _, key := range t.DonorPods {
		ns, name, _ := strings.Cut(key, "/")
		var p corev1.Pod
		if err := e.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
			if !apierrors.IsNotFound(err) {
				logger.Info("WARNING: could not read a donor pod to re-mark it", "pod", key, "error", err.Error())
			}
			continue
		}
		if !markedFor(&p, id) {
			continue
		}
		patch := client.MergeFrom(p.DeepCopy())
		p.Annotations[utilizationShareTransferAnnotation] = string(raw)
		if err := e.client.Patch(ctx, &p, patch); err != nil {
			logger.Info("WARNING: could not re-mark a donor pod", "pod", key, "error", err.Error())
		}
	}
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
		slices.SortStableFunc(set, func(a, b allocation.ShareTransfer) int {
			return cmp.Compare(boolInt(!a.IsSetPrimary()), boolInt(!b.IsSetPrimary()))
		})
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
