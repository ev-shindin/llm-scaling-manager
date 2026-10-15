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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Cluster-autoscaler event reasons on a pending pod.
const (
	eventTriggeredScaleUp  = "TriggeredScaleUp"
	eventNotTriggerScaleUp = "NotTriggerScaleUp"
)

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
	e.observePreemptions(ctx, constraints, requests, scaleTargets, now)
	st.cycle = allocation.BorrowCycle{
		Now: now, Constraints: constraints, Requests: requests, Gated: gated,
		ScaleUpPending: e.scaleUpPending(ctx, requests, now),
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
// is logged; the ledger is persisted when it changed.
func (e *Engine) releaseBorrowed(ctx context.Context, decisions []domain.VariantDecision,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor) {
	st := &e.borrowing
	if st.governor == nil || st.cycle.Now.IsZero() {
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	for _, r := range st.governor.Releases(st.cycle, decisions) {
		logger.Info("Releasing borrowed capacity",
			"namespace", r.Namespace, "model", r.ModelID, "variant", r.Variant,
			"from", r.From, "to", r.To, "cause", string(r.Cause),
			"lenderClusterQueue", r.Lender.Lender, "cohort", r.Lender.Cohort, "flavor", r.Lender.Flavor)
		acc := scaleTargets[r.Namespace+"/"+r.Variant]
		e.borrowReleasedEvent(r, acc)
		if acc != nil && scaletarget.IsLeaderWorkerSet(acc) {
			e.checkLWSScaleDown(ctx, r, acc)
		}
	}
	e.persistBorrowLedger()
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
func (e *Engine) persistBorrowLedger() {
	st := &e.borrowing
	raw := st.governor.MarshalLedger()
	if raw == st.lastLedger {
		return
	}
	st.lastLedger = raw
	e.Status.SetBorrowReleaseLedger(raw)
}

// observePreemptions starts the cooldown of each model one of whose pods Kueue
// preempted: a Workload with Evicted=True, reason Preempted, owned by a pod of
// one of the model's variants. One Workload list per borrowing namespace.
func (e *Engine) observePreemptions(ctx context.Context, constraints []*allocation.ResourceConstraints,
	requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor, now time.Time) {
	st := &e.borrowing
	if e.client == nil {
		return
	}
	if st.preemptSeen == nil {
		st.preemptSeen = map[types.UID]bool{}
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
	for ns := range namespaces {
		wls, err := kueue.PreemptedWorkloads(ctx, e.client, ns)
		if err != nil {
			continue
		}
		for _, w := range wls {
			if st.preemptSeen[w.UID] {
				continue
			}
			st.preemptSeen[w.UID] = true
			if model := modelOfPods(requests, scaleTargets, ns, w.Owners); model != "" {
				st.governor.MarkPreempted(model, now)
				e.recordReclaim(model, w, now)
				ctrl.LoggerFrom(ctx).Info("Kueue preempted a pod of a borrowing model; it borrows nothing new for the cooldown",
					"model", model, "workload", string(w.UID))
			}
		}
	}
}

// modelOfPods is "namespace/model" of the variant whose scale target owns a
// pod named in owners: a Deployment's pods are named <deployment>-<hash>-<id>,
// a LeaderWorkerSet's <lws>-<group>[-<worker>]. The longest matching name wins.
func modelOfPods(requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	ns string, owners []string) string {
	best, bestLen := "", 0
	for _, req := range requests {
		if req.Namespace != ns {
			continue
		}
		for _, st := range req.VariantStates {
			acc := scaleTargets[ns+"/"+st.VariantName]
			if acc == nil {
				continue
			}
			prefix := acc.GetName() + "-"
			for _, o := range owners {
				if strings.HasPrefix(o, prefix) && len(prefix) > bestLen {
					best, bestLen = ns+"/"+req.ModelID, len(prefix)
				}
			}
		}
	}
	return best
}

// scaleUpPending is, per variant with an unplaced pod old enough for the
// placement timeout to matter, whether a cluster-autoscaler TriggeredScaleUp
// on one of its unplaced pods is not followed by a NotTriggerScaleUp. One
// Event list per such namespace. A scale-up that is never triggered does not
// pause the timeout.
func (e *Engine) scaleUpPending(ctx context.Context, requests []allocation.ModelScalingRequest, now time.Time) map[string]bool {
	if e.client == nil {
		return nil
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
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "EventList"})
		if err := e.client.List(ctx, list, client.InNamespace(ns)); err != nil {
			continue
		}
		triggered, notTriggered := map[string]time.Time{}, map[string]time.Time{}
		for _, ev := range list.Items {
			pod, _, _ := unstructured.NestedString(ev.Object, "involvedObject", "name")
			if _, ok := pods[pod]; !ok {
				continue
			}
			reason, _, _ := unstructured.NestedString(ev.Object, "reason")
			at := eventTime(&ev)
			switch reason {
			case eventTriggeredScaleUp:
				if at.After(triggered[pod]) {
					triggered[pod] = at
				}
			case eventNotTriggerScaleUp:
				if at.After(notTriggered[pod]) {
					notTriggered[pod] = at
				}
			}
		}
		for pod, variant := range pods {
			if t, ok := triggered[pod]; ok && !notTriggered[pod].After(t) {
				out[ns+"/"+variant] = true
			}
		}
	}
	return out
}

// eventTime is an Event's latest occurrence: lastTimestamp, else eventTime,
// else its creation.
func eventTime(ev *unstructured.Unstructured) time.Time {
	for _, f := range []string{"lastTimestamp", "eventTime"} {
		if s, ok, _ := unstructured.NestedString(ev.Object, f); ok && s != "" {
			for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
				if t, err := time.Parse(layout, s); err == nil {
					return t
				}
			}
		}
	}
	return ev.GetCreationTimestamp().Time
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
