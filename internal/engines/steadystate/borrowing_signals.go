package steadystate

// What on-call sees of the GPU budget and of cohort borrowing
// (the Kueue borrowing proposal, PR #161, Part C): the
// figures the cycle used (metrics and the controller-status limiterBudgets
// key), the borrowing reasons on wva_model_scaling_blocked, unplaced pods,
// waiting lenders, and an Event per release. Everything here reports what the
// cycle decided; nothing decides.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controllerstatus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// publishBorrowSignals publishes the cycle's budgets and borrowing signals.
// Called every cycle that reaches the optimizer, after the releases; with no
// limiter, or no borrowing counted, it clears what an earlier cycle set.
// constraints are the ones the optimizer ran on.
func (e *Engine) publishBorrowSignals(constraints []*allocation.ResourceConstraints,
	requests []allocation.ModelScalingRequest, decisions []domain.VariantDecision, now time.Time) {
	st := &e.borrowing
	borrowing := st.governor != nil && !st.cycle.Now.IsZero()
	var blocked map[string]bool
	if borrowing {
		blocked = st.governor.BlockedCohorts(now)
	}

	budgets := allocation.LimiterBudgets(constraints, blocked)
	if borrowing {
		allocation.MarkPreemptionsUnreadable(budgets, st.cycle.PreemptionUnknown)
	}
	qb := make([]metrics.QuotaBudget, 0, len(budgets))
	sb := make([]controllerstatus.LimiterBudget, 0, len(budgets))
	for _, b := range budgets {
		qb = append(qb, metrics.QuotaBudget{Limiter: b.Limiter, Namespace: b.Namespace, Accelerator: b.Accelerator,
			Guaranteed: b.Guaranteed, Borrowable: b.Borrowable, Reason: b.Reason})
		sb = append(sb, controllerstatus.LimiterBudget{Limiter: b.Limiter, Namespace: b.Namespace,
			Accelerator: b.Accelerator, Guaranteed: b.Guaranteed, Borrowable: b.Borrowable, Used: b.Used, Reason: b.Reason})
	}
	metrics.PublishQuotaBudgets(qb)
	st.budgets = controllerstatus.StampLimiterBudgets(st.budgets, sb, now)
	e.Status.SetLimiterBudgets(st.budgets)

	for _, req := range requests {
		var active []string
		if borrowing {
			active = e.borrowReasons(req, constraints, decisions, now)
		}
		metrics.SetModelScalingBlockedReasons(req.Namespace, req.ModelID, constants.ScalingBlockedReasonsBorrowing, active)
	}

	var unplaced []metrics.UnplacedVariant
	var lenders []metrics.WaitingLender
	if borrowing {
		unplaced = unplacedPastTimeout(constraints, requests, e.borrowSettings().PlacementTimeout, now)
		lenders = waitingLenders(st.governor.WaitingLenders(now),
			allocation.BorrowingCohorts(constraints, st.cycle.Gated), now)
	}
	metrics.PublishKueueUnplacedPods(unplaced)
	metrics.PublishKueueLenderWaiting(lenders)
	metrics.PublishKueueNeverRead(kueueNeverRead(constraints))
}

// kueueNeverRead is the providers whose Kueue source has never been read
// while their static caps leave a type unlimited, sorted, once each.
func kueueNeverRead(constraints []*allocation.ResourceConstraints) []string {
	var out []string
	for _, c := range constraints {
		if c != nil && c.KueueNeverRead && !slices.Contains(out, c.ProviderName) {
			out = append(out, c.ProviderName)
		}
	}
	slices.Sort(out)
	return out
}

// borrowReasons is the borrowing reasons that hold for one model this cycle.
func (e *Engine) borrowReasons(req allocation.ModelScalingRequest, constraints []*allocation.ResourceConstraints,
	decisions []domain.VariantDecision, now time.Time) []string {
	st := &e.borrowing
	var released, unplaced, reclaimed, lentOut bool
	counted := countsBorrowing(constraints, req.Namespace)
	for _, v := range req.VariantStates {
		s := st.governor.VariantStatus(req.Namespace, v.VariantName)
		released = released || s.LenderRelease
		unplaced = unplaced || s.TimeoutRelease || (s.Unplaced && counted)
	}
	if until, ok := st.reclaimedUntil[req.Namespace+"/"+req.ModelID]; ok && now.Before(until) {
		reclaimed = true
	}
	for i := range decisions {
		d := &decisions[i]
		if d.Namespace == req.Namespace && d.ModelID == req.ModelID && d.WasLimited &&
			nominalLentOut(constraints, req.Namespace, d.AcceleratorName) > 0 {
			lentOut = true
		}
	}
	var out []string
	for reason, on := range map[string]bool{
		constants.ScalingBlockedBorrowReleased:  released,
		constants.ScalingBlockedBorrowUnplaced:  unplaced,
		constants.ScalingBlockedLenderReclaimed: reclaimed,
		constants.ScalingBlockedNominalLentOut:  lentOut,
	} {
		if on {
			out = append(out, reason)
		}
	}
	return out
}

// countsBorrowing reports whether some constraint counts ns's borrowing.
func countsBorrowing(constraints []*allocation.ResourceConstraints, ns string) bool {
	for _, c := range constraints {
		if c != nil && c.NamespaceBorrow[ns] != nil {
			return true
		}
	}
	return false
}

// nominalLentOut is the nominal of ns's queues serving acc that a peer holds
// and they cannot reclaim, summed over the constraints; acc "" sums every type.
func nominalLentOut(constraints []*allocation.ResourceConstraints, ns, acc string) int {
	n := 0
	for _, c := range constraints {
		if c == nil {
			continue
		}
		perType := c.NamespaceBorrow[ns]
		if acc == "" {
			for _, b := range perType {
				n += b.NominalLentOut
			}
			continue
		}
		if key, ok := accelerator.FindKey(perType, acc); ok {
			n += perType[key].NominalLentOut
		}
	}
	return n
}

// unplacedPastTimeout is, per variant in a namespace that counts borrowing,
// its pods unplaced for longer than the placement timeout.
func unplacedPastTimeout(constraints []*allocation.ResourceConstraints, requests []allocation.ModelScalingRequest,
	timeout time.Duration, now time.Time) []metrics.UnplacedVariant {
	var out []metrics.UnplacedVariant
	for _, req := range requests {
		if !countsBorrowing(constraints, req.Namespace) {
			continue
		}
		for _, s := range req.VariantStates {
			if s.UnplacedReplicas <= 0 || s.UnplacedSince.IsZero() || now.Sub(s.UnplacedSince) <= timeout {
				continue
			}
			pods := len(s.UnplacedPods)
			if pods == 0 {
				pods = s.UnplacedReplicas
			}
			out = append(out, metrics.UnplacedVariant{Namespace: req.Namespace, Variant: s.VariantName, Pods: pods})
		}
	}
	return out
}

// waitingLenders is each waiting lender in a cohort some governed namespace
// borrows in, once per ClusterQueue, with its longest wait.
func waitingLenders(waits []allocation.LenderWait, borrowing map[string]bool, now time.Time) []metrics.WaitingLender {
	type key struct{ cohort, queue string }
	since := map[key]time.Time{}
	var order []key
	for _, w := range waits {
		if !borrowing[w.Cohort] {
			continue
		}
		k := key{w.Cohort, w.Lender}
		prev, seen := since[k]
		if !seen {
			order = append(order, k)
		}
		if !seen || w.Since.Before(prev) {
			since[k] = w.Since
		}
	}
	out := make([]metrics.WaitingLender, 0, len(order))
	for _, k := range order {
		out = append(out, metrics.WaitingLender{Cohort: k.cohort, ClusterQueue: k.queue, Seconds: now.Sub(since[k]).Seconds()})
	}
	return out
}

// recordReclaim counts a Kueue preemption of a model's pods -- once per
// preempted Workload, by key (preemptionKey), so a LeaderWorkerSet or pod
// group whose pods arrive one by one counts once -- and holds the
// lender-reclaimed reason for the cooldown after it. A preemption older than
// the cooldown, one a restart finds again, counts nothing.
func (e *Engine) recordReclaim(modelKey string, w kueue.PreemptedWorkload, now time.Time, key string) {
	cooldown := e.borrowSettings().Cooldown
	at := w.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	if now.Sub(at) > cooldown {
		return
	}
	st := &e.borrowing
	if st.reclaimedUntil == nil {
		st.reclaimedUntil = map[string]time.Time{}
	}
	if until := at.Add(cooldown); until.After(st.reclaimedUntil[modelKey]) {
		st.reclaimedUntil[modelKey] = until
	}
	for k, until := range st.reclaimedUntil {
		if !now.Before(until) {
			delete(st.reclaimedUntil, k)
		}
	}
	if st.preemptCounted == nil {
		st.preemptCounted = map[string]time.Time{}
	}
	for k, seen := range st.preemptCounted {
		if now.Sub(seen) > cooldown {
			delete(st.preemptCounted, k)
		}
	}
	if _, counted := st.preemptCounted[key]; counted && key != "" {
		return
	}
	st.preemptCounted[key] = now
	ns, model, _ := strings.Cut(modelKey, "/")
	metrics.CountKueuePreemptedPods(ns, model, 1)
}

// borrowReleasedEvent records, on the released variant's scale target, that
// the scaling manager lowered it to give borrowed capacity back, and why. The
// recorder aggregates repeats.
func (e *Engine) borrowReleasedEvent(r allocation.BorrowRelease, acc scaletarget.ScaleTargetAccessor) {
	switch r.Cause {
	case allocation.ReleaseLenderWaiting:
		e.shareEvent(acc, corev1.EventTypeNormal, constants.K8SEventBorrowedCapacityReleased,
			fmt.Sprintf("Released borrowed Kueue capacity: replicas lowered from %d to %d (cause %s). "+
				"ClusterQueue %s in cohort %s lent these GPUs, is waiting for them, and cannot reclaim them itself "+
				"(reclaimWithinCohort: Never)", r.From, r.To, r.Cause, r.Lender.Lender, r.Lender.Cohort))
	default:
		e.shareEvent(acc, corev1.EventTypeWarning, constants.K8SEventBorrowedCapacityReleased,
			fmt.Sprintf("Released borrowed Kueue capacity: replicas lowered from %d to %d (cause %s). "+
				"Replicas on borrowed GPUs did not place within the placement timeout; this model borrows "+
				"nothing new for the cooldown", r.From, r.To, r.Cause))
	}
}
