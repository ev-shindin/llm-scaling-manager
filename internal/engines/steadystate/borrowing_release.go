package steadystate

// Giving borrowed capacity back, on the engine side
// (docs/proposals/kueue-borrowing-and-limiter-composition.md, B5 and B6): the
// allocation.BorrowGovernor decides; this file feeds it what only the cluster
// knows -- the persisted ledger, Kueue preemptions, cluster-autoscaler events --
// and applies and persists what it decides.

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controllerstatus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/variantmeta"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Cluster-autoscaler event reasons on a pending pod, and its component name.
const (
	eventTriggeredScaleUp  = "TriggeredScaleUp"
	eventNotTriggerScaleUp = "NotTriggerScaleUp"
	clusterAutoscaler      = "cluster-autoscaler"
)

// kueueTerminationTarget is the pod condition Kueue sets on a pod it is about
// to delete, with the reason it stops the pod.
const kueueTerminationTarget = "TerminationTarget"

// borrowSettings are the governor's timings from the policy.
func (e *Engine) borrowSettings() allocation.BorrowSettings {
	s := allocation.DefaultBorrowSettings()
	if e.Config == nil {
		return s
	}
	t := e.Config.KueueBorrowTimings()
	s.PlacementTimeout, s.Cooldown, s.LenderDebounce = t.PlacementTimeout, t.Cooldown, t.LenderDebounce
	return s
}

// governBorrowing runs the governor's half before the optimizer: the
// persisted ledger is read back once, preemptions start model cooldowns, the
// lenders and placement are observed, and the cohorts where nothing new may
// be borrowed have their pools closed.
func (e *Engine) governBorrowing(ctx context.Context, constraints []*allocation.ResourceConstraints,
	requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	gated map[string]map[string]int, now time.Time) []*allocation.ResourceConstraints {
	st := &e.borrowing
	if st.governor == nil {
		st.governor = allocation.NewBorrowGovernor(e.borrowSettings())
	} else {
		st.governor.SetSettings(e.borrowSettings())
	}
	e.loadBorrowLedger(ctx)
	unknown := e.observePreemptions(ctx, constraints, requests, scaleTargets, now)
	st.cycle = allocation.BorrowCycle{
		Now: now, Constraints: constraints, Requests: requests, Gated: gated,
		ScaleUpPending: e.scaleUpPending(ctx, requests, now), PreemptionUnknown: unknown,
	}
	st.governor.Observe(st.cycle)
	return allocation.BlockCohorts(constraints, st.governor.BlockedCohorts(now))
}

// borrowBlocked is the greedy optimizer's per-variant borrowing block for this
// cycle, nil when the cycle borrows nothing.
func (e *Engine) borrowBlocked() func(namespace, modelID, variant string) bool {
	g, now := e.borrowing.governor, e.borrowing.cycle.Now
	if g == nil || now.IsZero() {
		return nil
	}
	return func(namespace, modelID, variant string) bool {
		return g.BorrowBlocked(namespace, modelID, variant, now)
	}
}

// releaseBorrowed runs the governor's half after every stage that sets a
// target: releases lower the decisions and stay in force until they land. Each
// is logged, counted and recorded as an Event; the ledger is persisted when it
// changed. In a cycle that counts no borrowing nothing new is decided, but the
// releases already in flight still hold (BorrowGovernor.HoldReleases): a
// transient read failure must not let the demand floor raise them again.
func (e *Engine) releaseBorrowed(ctx context.Context, requests []allocation.ModelScalingRequest,
	decisions []domain.VariantDecision, scaleTargets map[string]scaletarget.ScaleTargetAccessor, now time.Time) {
	st := &e.borrowing
	if st.governor == nil && !e.restoreHeldReleases(ctx) {
		return
	}
	if st.cycle.Now.IsZero() {
		st.governor.HoldReleases(now, requests, decisions)
		e.persistBorrowLedger(ctx)
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	for _, r := range st.governor.Releases(st.cycle, decisions) {
		logger.Info("Releasing borrowed capacity",
			"namespace", r.Namespace, "modelID", r.ModelID, "variant", r.Variant,
			"from", r.From, "to", r.To, "cause", string(r.Cause),
			"lenderClusterQueue", r.Lender.Lender, "cohort", r.Lender.Cohort, "flavor", r.Lender.Flavor)
		if r.GuaranteedGPUs > 0 {
			logger.Info("A borrowed-capacity release goes below the namespace's guaranteed figure: "+
				"the smallest unit that keeps the model serviceable is larger than what it borrows",
				"namespace", r.Namespace, "modelID", r.ModelID, "cause", string(r.Cause), "guaranteedGPUs", r.GuaranteedGPUs)
		}
		metrics.CountBorrowRelease(r.Namespace, r.ModelID, string(r.Cause))
		acc := scaleTargets[r.Namespace+"/"+r.Variant]
		e.borrowReleasedEvent(r, acc)
		if acc != nil && scaletarget.IsLeaderWorkerSet(acc) {
			e.checkLWSScaleDown(ctx, r, acc)
		}
	}
	e.persistBorrowLedger(ctx)
}

// restoreHeldReleases is for a process whose first cycles count no borrowing:
// once, it reads the persisted ledger, and keeps a governor only when that
// ledger pins something (a release in flight or a placement-timeout hold), so
// those pins, and the window after which they are dropped, survive a restart.
// Otherwise it leaves no governor and the ledger unloaded, for the first cycle
// that counts borrowing to load. Reports whether a governor now exists.
func (e *Engine) restoreHeldReleases(ctx context.Context) bool {
	st := &e.borrowing
	if st.holdChecked {
		return false
	}
	st.holdChecked = true
	st.governor = allocation.NewBorrowGovernor(e.borrowSettings())
	e.loadBorrowLedger(ctx)
	if l := st.governor.Ledger(); len(l.Entries) > 0 || len(l.TimeoutHeld) > 0 {
		return true
	}
	st.governor, st.ledgerLoaded, st.lastLedger = nil, false, ""
	return false
}

// loadBorrowLedger reads the persisted ledger back, once. Until it succeeds
// the governor treats every waiting lender as having a release in flight until
// the next fresh read, which is what it would do with nothing persisted.
func (e *Engine) loadBorrowLedger(ctx context.Context) {
	st := &e.borrowing
	if st.ledgerLoaded {
		return
	}
	var reader client.Reader = e.client
	if e.apiReader != nil {
		reader = e.apiReader
	}
	if reader == nil {
		st.ledgerLoaded = true
		return
	}
	raw, found, err := controllerstatus.ReadKey(ctx, reader, config.SystemNamespace(), controllerstatus.KeyBorrowReleaseLedger)
	if err != nil {
		ctrl.LoggerFrom(ctx).Info("Cannot read the persisted borrowing release ledger yet; waiting lenders are treated as having a release in flight until their next fresh read",
			"error", err.Error())
		return
	}
	st.ledgerLoaded = true
	if !found {
		return
	}
	l, err := allocation.UnmarshalBorrowLedger(raw)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Discarding an unreadable borrowing release ledger")
		return
	}
	st.governor.Restore(l)
	st.lastLedger = raw
}

// persistBorrowLedger hands the ledger to the status publisher when changed.
func (e *Engine) persistBorrowLedger(ctx context.Context) {
	st := &e.borrowing
	raw, err := st.governor.MarshalLedger()
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot persist the borrowing release ledger; the last persisted one stays")
		return
	}
	if raw == st.lastLedger {
		return
	}
	st.lastLedger = raw
	e.Status.SetBorrowReleaseLedger(raw)
}

// observePreemptions starts the cooldown of each model one of whose pods Kueue
// preempted, from when it did, and returns the namespaces whose preemptions
// could not be read: their models borrow nothing new this cycle. Three
// sources:
//   - the pods the pod informer saw marked for a preemption since the last
//     cycle (Engine.Preemptions): the one that does not miss a preempted pod
//     and Workload that are gone within seconds, as they usually are;
//   - Workloads with Evicted=True, reason Preempted (one API list). A
//     LeaderWorkerSet's Workload is owned by the LeaderWorkerSet itself and is
//     matched by its name and UID; a pod's by the pod.
//   - Pods carrying Kueue's TerminationTarget condition for a preemption (the
//     cached pod informer). Kueue sets it before it deletes the pod, and a
//     pod's Workload goes with the pod, often between two cycles.
//
// A preemption older than the cooldown starts nothing; the UIDs counted are
// kept only while still listed, judged only in a namespace where both lists
// succeeded.
func (e *Engine) observePreemptions(ctx context.Context, constraints []*allocation.ResourceConstraints,
	requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor, now time.Time) map[string]bool {
	st := &e.borrowing
	if e.client == nil {
		return nil
	}
	if st.preemptSeen == nil {
		st.preemptSeen = map[types.UID]string{}
	}
	if st.preemptErrLogged == nil {
		st.preemptErrLogged = map[string]bool{}
	}
	if st.watchSeen == nil {
		st.watchSeen = map[types.UID]time.Time{}
	}
	logger := ctrl.LoggerFrom(ctx)
	cooldown := e.borrowSettings().Cooldown
	for uid, at := range st.watchSeen {
		if now.Sub(at) > cooldown {
			delete(st.watchSeen, uid)
		}
	}
	namespaces := map[string]bool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns := range c.NamespaceBorrow {
			namespaces[ns] = true
		}
	}
	var unknown map[string]bool
	listed := map[types.UID]bool{}
	read := map[string]bool{}
	mark := func(ns, model string, uids []types.UID, w kueue.PreemptedWorkload, key string) {
		fresh := false
		for _, uid := range uids {
			listed[uid] = true
			_, watched := st.watchSeen[uid]
			if _, ok := st.preemptSeen[uid]; !ok && !watched {
				fresh = true
				st.preemptSeen[uid] = ns
			}
		}
		if !fresh || model == "" {
			return
		}
		at := w.At
		if at.IsZero() || at.After(now) {
			at = now
		}
		if now.Sub(at) > cooldown {
			return // found again after a restart: its cooldown is over
		}
		st.governor.MarkPreempted(model, at)
		e.recordReclaim(model, w, now, key)
		logger.Info("Kueue preempted a pod of a borrowing model; it borrows nothing new for the cooldown",
			"modelID", model, "uid", string(uids[0]), "preemptedAt", at)
	}
	observed, dropped := e.Preemptions.drain()
	if dropped > 0 {
		logger.Info("The preemption watch dropped preemptions past its bound between two cycles; the Workload and pod reads still see what remains",
			"dropped", dropped)
	}
	// Every namespace, not only those counting borrowing this cycle: the
	// preempted pod itself can withhold its namespace for as long as it
	// terminates (measured on kind: 30s of not-kueue-managed, its Workload
	// already gone), and a preemption dropped then is never seen again. A pod
	// of no known model matches nothing.
	for _, o := range observed {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: o.Name, UID: o.UID,
			Labels: o.Labels, OwnerReferences: o.OwnerRefs}}
		model := modelOfPod(requests, scaleTargets, o.Namespace, p)
		mark(o.Namespace, model, []types.UID{o.UID},
			kueue.PreemptedWorkload{UID: o.UID, Owners: []string{o.Name}, At: o.At},
			preemptionKey(o.Namespace, o.UID, o.Labels))
		if _, ok := st.watchSeen[o.UID]; !ok {
			st.watchSeen[o.UID] = now
		}
	}
	for ns := range namespaces {
		wls, err := kueue.PreemptedWorkloads(ctx, e.client, ns)
		if err != nil {
			if unknown == nil {
				unknown = map[string]bool{}
			}
			unknown[ns] = true
			if !st.preemptErrLogged[ns] {
				st.preemptErrLogged[ns] = true
				logger.Error(err, "Cannot read Kueue preemptions; the namespace borrows nothing new until they can be read",
					"namespace", ns)
			}
			continue
		}
		delete(st.preemptErrLogged, ns)
		for _, w := range wls {
			model, uids := modelOfOwners(requests, scaleTargets, ns, w.OwnerRefs)
			if len(uids) == 0 {
				uids = []types.UID{w.UID}
			}
			mark(ns, model, uids, w, workloadPreemptionKey(ns, w))
		}
		// Pruning needs both lists: a UID seen only through its pod's
		// TerminationTarget condition, pruned in a cycle whose pod list
		// failed, would be counted again the next cycle.
		var pods corev1.PodList
		if err := e.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
			continue
		}
		read[ns] = true
		for i := range pods.Items {
			p := &pods.Items[i]
			at, ok := preemptedPod(p)
			if !ok {
				continue
			}
			model := modelOfPod(requests, scaleTargets, ns, p)
			mark(ns, model, []types.UID{p.UID}, kueue.PreemptedWorkload{UID: p.UID, Owners: []string{p.Name}, At: at},
				preemptionKey(ns, p.UID, p.Labels))
		}
	}
	for uid, ns := range st.preemptSeen {
		if read[ns] && !listed[uid] {
			delete(st.preemptSeen, uid)
		}
	}
	return unknown
}

// preemptedPod reports whether Kueue marked a pod for termination because its
// Workload was preempted, and when: the TerminationTarget condition, reason
// WorkloadEvictedDueToPreempted (or an older Kueue's generic reason with the
// eviction message naming the preemption).
func preemptedPod(p *corev1.Pod) (time.Time, bool) {
	for _, c := range p.Status.Conditions {
		if string(c.Type) != kueueTerminationTarget || c.Status != corev1.ConditionTrue {
			continue
		}
		if strings.Contains(c.Reason, "Preempted") || strings.Contains(c.Message, "Preempted") {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

// modelOfOwners is "namespace/model" of the variant whose scale target owns a
// preempted Workload, and the UIDs that identify the preemption: the pods it
// is owned by, so a pod seen through its TerminationTarget condition is not
// counted twice. A LeaderWorkerSet owner matches its scale target by name and
// UID; a pod by its name, <deployment>-<hash>-<id> or <lws>-<group>, the
// longest scale-target prefix winning.
func modelOfOwners(requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	ns string, owners []metav1.OwnerReference) (string, []types.UID) {
	model := ""
	var pods []types.UID
	for _, o := range owners {
		if o.Kind == kindPod {
			pods = append(pods, o.UID)
			if m := modelByPodName(requests, scaleTargets, ns, o.Name); m != "" {
				model = m
			}
			continue
		}
		forEachTarget(requests, scaleTargets, ns, func(m string, acc scaletarget.ScaleTargetAccessor) {
			if acc.GetName() == o.Name && (o.UID == "" || acc.GetUID() == "" || acc.GetUID() == o.UID) {
				model = m
			}
		})
	}
	return model, pods
}

// modelOfPod is "namespace/model" of the variant whose scale target owns a
// pod: a LeaderWorkerSet's pods by their set-name label, a Deployment's by
// name prefix.
func modelOfPod(requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	ns string, p *corev1.Pod) string {
	if set := p.Labels[lwsv1.SetNameLabelKey]; set != "" {
		model := ""
		forEachTarget(requests, scaleTargets, ns, func(m string, acc scaletarget.ScaleTargetAccessor) {
			if scaletarget.IsLeaderWorkerSet(acc) && acc.GetName() == set {
				model = m
			}
		})
		return model
	}
	return modelByPodName(requests, scaleTargets, ns, p.Name)
}

// modelByPodName matches a pod name to the longest "<scale target>-" prefix.
func modelByPodName(requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	ns, pod string) string {
	best, bestLen := "", 0
	forEachTarget(requests, scaleTargets, ns, func(m string, acc scaletarget.ScaleTargetAccessor) {
		prefix := acc.GetName() + "-"
		if strings.HasPrefix(pod, prefix) && len(prefix) > bestLen {
			best, bestLen = m, len(prefix)
		}
	})
	return best
}

// forEachTarget calls f with "namespace/model" and the scale target of every
// variant in ns that has one.
func forEachTarget(requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	ns string, f func(model string, acc scaletarget.ScaleTargetAccessor)) {
	for _, req := range requests {
		if req.Namespace != ns {
			continue
		}
		for _, st := range req.VariantStates {
			if acc := scaleTargets[ns+"/"+st.VariantName]; acc != nil {
				f(ns+"/"+req.ModelID, acc)
			}
		}
	}
}

// scaleUpPending is, per variant with an unplaced pod old enough for the
// placement timeout to matter, whether a cluster-autoscaler TriggeredScaleUp
// on one of its unplaced pods is not followed by a NotTriggerScaleUp. Two
// Event lists per such namespace, straight from the API server and narrowed
// by reason, so neither every Event of the namespace nor a cluster-wide Event
// informer is read; only Events on a Pod reported by the cluster autoscaler
// count. A scale-up that is never triggered does not pause the timeout.
//
// An Event list that fails is logged once per namespace (until one succeeds)
// and reads as no scale-up pending: the timeout keeps running. Pausing on an
// unreadable list would let an Events outage hold a borrowed replica that
// cannot place for three timeouts; running it at worst releases a replica a
// new node was about to take, which the next cycle may grow back.
func (e *Engine) scaleUpPending(ctx context.Context, requests []allocation.ModelScalingRequest, now time.Time) map[string]bool {
	var reader client.Reader = e.client
	if e.apiReader != nil {
		reader = e.apiReader
	}
	if reader == nil {
		return nil
	}
	st := &e.borrowing
	if st.eventsErrLogged == nil {
		st.eventsErrLogged = map[string]bool{}
	}
	timeout := e.borrowSettings().PlacementTimeout
	byNS := map[string]map[string]string{} // ns -> pod -> variant
	for _, req := range requests {
		for _, st := range req.VariantStates {
			if st.UnplacedReplicas == 0 || now.Sub(st.UnplacedSince) < timeout {
				continue
			}
			if byNS[req.Namespace] == nil {
				byNS[req.Namespace] = map[string]string{}
			}
			for _, p := range st.UnplacedPods {
				byNS[req.Namespace][p] = st.VariantName
			}
		}
	}
	out := map[string]bool{}
	for ns, pods := range byNS {
		latest := map[string]map[string]time.Time{} // reason -> pod -> time
		var failed error
		for _, reason := range []string{eventTriggeredScaleUp, eventNotTriggerScaleUp} {
			var list corev1.EventList
			if err := reader.List(ctx, &list, client.InNamespace(ns), client.MatchingFields{"reason": reason, "involvedObject.kind": kindPod}); err != nil {
				failed = err
				break
			}
			latest[reason] = map[string]time.Time{}
			for i := range list.Items {
				ev := &list.Items[i]
				if ev.InvolvedObject.Kind != kindPod || !fromClusterAutoscaler(ev) {
					continue
				}
				if _, ok := pods[ev.InvolvedObject.Name]; !ok {
					continue
				}
				if at := eventTime(ev); at.After(latest[reason][ev.InvolvedObject.Name]) {
					latest[reason][ev.InvolvedObject.Name] = at
				}
			}
		}
		if failed != nil {
			if !st.eventsErrLogged[ns] {
				st.eventsErrLogged[ns] = true
				ctrl.LoggerFrom(ctx).Error(failed, "Cannot read cluster-autoscaler Events; the placement timeout runs without waiting for a scale-up",
					"namespace", ns)
			}
			continue
		}
		delete(st.eventsErrLogged, ns)
		for pod, variant := range pods {
			if t, ok := latest[eventTriggeredScaleUp][pod]; ok && !latest[eventNotTriggerScaleUp][pod].After(t) {
				out[ns+"/"+variant] = true
			}
		}
	}
	return out
}

// fromClusterAutoscaler reports whether the cluster autoscaler reported ev.
func fromClusterAutoscaler(ev *corev1.Event) bool {
	return ev.Source.Component == clusterAutoscaler || ev.ReportingController == clusterAutoscaler
}

// eventTime is an Event's latest occurrence: lastTimestamp, else eventTime,
// else its creation.
func eventTime(ev *corev1.Event) time.Time {
	switch {
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

// checkLWSScaleDown logs when the group a LeaderWorkerSet removes on scale-down
// -- the highest group index, LeaderWorkerSet's behaviour, not Kueue's -- is
// not the newest group, the one that borrowed.
func (e *Engine) checkLWSScaleDown(ctx context.Context, r allocation.BorrowRelease, acc scaletarget.ScaleTargetAccessor) {
	if e.client == nil {
		return
	}
	pods, err := variantmeta.ListVariantPods(ctx, e.client, r.Namespace, acc)
	if err != nil {
		return
	}
	if highest, newest, ok := lwsHighestIsNewest(pods); !ok {
		ctrl.LoggerFrom(ctx).Info("A LeaderWorkerSet scales down its highest group index, which is not its newest group; the release frees a group that may not be the borrowed one",
			"namespace", r.Namespace, "variant", r.Variant, "highestGroup", highest, "newestGroup", newest)
	}
}

// lwsHighestIsNewest reports whether the highest group index of a
// LeaderWorkerSet's pods is also its most recently created group, judged by
// each group's oldest pod.
func lwsHighestIsNewest(pods []corev1.Pod) (highest, newest int, ok bool) {
	created := map[int]time.Time{}
	for i := range pods {
		idx, err := strconv.Atoi(pods[i].Labels[lwsv1.GroupIndexLabelKey])
		if err != nil {
			continue
		}
		at := pods[i].CreationTimestamp.Time
		if prev, seen := created[idx]; !seen || at.Before(prev) {
			created[idx] = at
		}
	}
	if len(created) == 0 {
		return 0, 0, true
	}
	idxs := make([]int, 0, len(created))
	for i := range created {
		idxs = append(idxs, i)
	}
	slices.Sort(idxs)
	highest = idxs[len(idxs)-1]
	newest = highest
	for _, i := range idxs {
		if created[i].After(created[newest]) {
			newest = i
		}
	}
	return highest, newest, !created[newest].After(created[highest])
}
