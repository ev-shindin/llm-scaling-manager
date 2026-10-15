package steadystate

// Cohort borrowing on the engine side
// (docs/proposals/kueue-borrowing-and-limiter-composition.md, B4 and B5): what
// the Kueue reader cannot see -- growth ordered on borrowed capacity that Kueue
// has not reserved yet, and managed pods held at its admission gate -- is taken
// out of the shared pools before the optimizer runs, and one node view per cycle
// is built for the optimizer to fit borrowed replicas into.

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controllerstatus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// borrowingState is the steady-state engine's cohort-borrowing state.
type borrowingState struct {
	// inFlight is borrowed growth ordered and not yet visible as pods. The
	// release steps of the proposal extend it.
	inFlight *allocation.BorrowInFlight
	// nodes is this cycle's node view, nil when the cycle counts no borrowing
	// or no physical limiter is declared. The greedy optimizer debits it and
	// the utilization-share idle fill then reads it (shareNodes).
	nodes *allocation.NodeView
	// nodeStore is the node snapshot the view is built from; nil is
	// decision.DefaultNodeGPUs.
	nodeStore *decision.NodeGPUStore

	// governor decides the placement hold, the cooldowns and the releases of
	// borrowed capacity (borrowing_release.go); nil until a cycle borrows.
	governor *allocation.BorrowGovernor
	// ledgerLoaded is set once the persisted release ledger was read back.
	ledgerLoaded bool
	// lastLedger is the ledger last handed to the status publisher.
	lastLedger string
	// preemptSeen is the preempted Workloads already counted, by UID.
	preemptSeen map[types.UID]bool
	// cycle is this cycle's input to the governor, kept for the releases
	// after the optimizer; zero when the cycle borrows nothing.
	cycle allocation.BorrowCycle
	// reclaimedUntil is, per "namespace/model", until when the
	// lender-reclaimed reason holds: a recent Kueue preemption plus the
	// cooldown.
	reclaimedUntil map[string]time.Time
	// budgets is the limiterBudgets value last handed to the status
	// publisher, whose asOf stamps the next one keeps.
	budgets controllerstatus.LimiterBudgets
}

// prepareBorrowing readies one cycle's borrowing: it drops the in-flight draws
// that resolved, takes what is in flight and what managed pods hold at Kueue's
// admission gate out of the shared pools, and builds the cycle's node view.
// Without borrowing in the constraints it returns them as they are and the
// cycle has no node view, exactly as before borrowing was counted.
func (e *Engine) prepareBorrowing(ctx context.Context, constraints []*allocation.ResourceConstraints,
	requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	now time.Time) []*allocation.ResourceConstraints {
	st := &e.borrowing
	st.nodes = nil
	st.cycle = allocation.BorrowCycle{}
	if st.inFlight == nil {
		st.inFlight = allocation.NewBorrowInFlight()
	}
	st.inFlight.Observe(observeVariants(requests))
	if !allocation.HasBorrowing(constraints) {
		return constraints
	}
	if e.Config != nil && e.Config.HasPhysicalLimiter() {
		store := st.nodeStore
		if store == nil {
			store = decision.DefaultNodeGPUs
		}
		st.nodes = allocation.NewNodeView(store.Latest(decision.NodeGPUsMaxAge, now))
	}
	gated := e.gatedGPUs(ctx, constraints)
	constraints = allocation.DeductUnreserved(constraints, st.inFlight.Entries(), gated)
	return e.governBorrowing(ctx, constraints, requests, scaleTargets, gated, now)
}

// recordBorrowDraws keeps what the optimizer borrowed this cycle as in flight.
func (e *Engine) recordBorrowDraws(optimizer allocation.ScalingOptimizer) {
	g, ok := optimizer.(*allocation.GreedyByScoreOptimizer)
	if !ok || e.borrowing.inFlight == nil {
		return
	}
	e.borrowing.inFlight.Record(g.BorrowDraws())
}

// observeVariants is each variant's replica counts, keyed "namespace/variant".
func observeVariants(requests []allocation.ModelScalingRequest) map[string]allocation.VariantObservation {
	out := map[string]allocation.VariantObservation{}
	for _, req := range requests {
		for _, s := range req.VariantStates {
			out[req.Namespace+"/"+s.VariantName] = allocation.VariantObservation{Current: s.CurrentReplicas, Desired: s.DesiredReplicas}
		}
	}
	return out
}

// gatedGPUs is, per namespace some borrowing constraint maps to a cohort, the
// GPUs of pods held at Kueue's admission gate, per the accelerator their node
// selector names ("" when none). They hold no reservation yet, so the cohort's
// free pool still counts what Kueue is about to admit them onto. Read from the
// manager's cached pod informer: one list per such namespace, no API call.
func (e *Engine) gatedGPUs(ctx context.Context, constraints []*allocation.ResourceConstraints) map[string]map[string]int {
	if e.client == nil {
		return nil
	}
	namespaces := map[string]bool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns := range c.NamespaceCohorts {
			namespaces[ns] = true
		}
	}
	out := map[string]map[string]int{}
	for ns := range namespaces {
		var pods corev1.PodList
		if err := e.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
			continue
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.DeletionTimestamp != nil || !kueue.PodGated(p) {
				continue
			}
			g := resources.PodGPURequests(p)
			if g <= 0 {
				continue
			}
			if out[ns] == nil {
				out[ns] = map[string]int{}
			}
			out[ns][accelerator.ProductFromNodeSelector(p.Spec.NodeSelector)] += g
		}
	}
	return out
}
