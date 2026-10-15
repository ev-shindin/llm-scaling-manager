package allocation

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// GreedyByScoreOptimizer is a multi-model optimizer for GPU-constrained
// environments. It uses iterative mean-based fair-sharing to distribute scarce
// GPUs across competing models, ordered by fair-share priority value
// (priority × Σᵢ(Remainingᵢ × Scoreᵢ) across analyzers).
//
// Key differences from CostAwareOptimizer:
//   - Respects ResourceConstraints (GPU budgets per accelerator type)
//   - Fair-shares GPUs across models (highest-priority model gets GPUs first)
//   - Disaggregated models use paired (n_P, n_D) allocation via the paired helpers
//   - Scale-down uses scaleDownRoleIterated (role-iterated unified path)
type GreedyByScoreOptimizer struct {
	// Rescale carries the resolved, scope-coupled rescale enablement for the
	// current cycle (set by the engine before Optimize). Zero value = off, which
	// keeps the additive fair-share behaviour unchanged.
	Rescale RescaleFlags

	// quiet suppresses the pass's side effects: limit attribution, which
	// publishes warm-pool headroom and contention and counts limited decisions.
	// Set only on the copy ShadowOptimize runs.
	quiet bool

	// Nodes is the cycle's node view, set by the engine before Optimize when a
	// physical limiter is declared and the cycle counts cohort borrowing; nil
	// fits nothing to nodes. A pass debits it in place -- every replica it
	// orders that fits a node -- so the utilization-share idle fill that reads it
	// afterwards cannot place into the same hole (B5). A shadow pass works on a
	// copy.
	Nodes *NodeView

	// BorrowBlocked, when set, reports whether a variant may not grow on
	// borrowed capacity this cycle (a placement hold, a model cooldown; see
	// BorrowGovernor.BorrowBlocked). Its guaranteed growth is unaffected.
	BorrowBlocked func(namespace, modelID, variant string) bool

	// draws is what the last non-shadow pass borrowed (BorrowDraws).
	draws []BorrowDraw
}

// BorrowDraws returns what the last Optimize pass drew from the cohorts'
// shared pools, one entry per variant and chunk; nil when it borrowed nothing.
// The engine keeps these as in flight until the pods exist.
func (o *GreedyByScoreOptimizer) BorrowDraws() []BorrowDraw {
	return o.draws
}

// ShadowOptimize runs the optimizer over constraints for comparison only: it
// returns the decisions a real pass would make, and publishes nothing, counts
// nothing and logs nothing. The engine uses it to show, beside the real
// allocation, what a subset of the limiters alone would have allowed.
func (o *GreedyByScoreOptimizer) ShadowOptimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []domain.VariantDecision {
	shadow := *o
	shadow.quiet = true
	shadow.Nodes = o.Nodes.Clone()
	return shadow.Optimize(ctrl.LoggerInto(ctx, logr.Discard()), requests, constraints)
}

// attributeGPULimits is applyGPULimitAttribution unless the pass is a shadow.
func (o *GreedyByScoreOptimizer) attributeGPULimits(
	decisions []domain.VariantDecision,
	limited gpuLimitTracker,
	constraints []*ResourceConstraints,
) {
	if o.quiet {
		return
	}
	applyGPULimitAttribution(decisions, limited, constraints)
}

// NewGreedyByScoreOptimizer creates a new GreedyByScoreOptimizer.
func NewGreedyByScoreOptimizer() *GreedyByScoreOptimizer {
	return &GreedyByScoreOptimizer{}
}

// Name returns the optimizer identifier.
func (o *GreedyByScoreOptimizer) Name() string {
	return "greedy-by-score"
}

// modelWork tracks per-model allocation state during fair-share iteration.
type modelWork struct {
	req       ModelScalingRequest
	e         *NamedAnalyzerResult // working entry; Remaining/Spare decremented in place
	records   []variantRecord      // engine-built per-variant view (discovery identity + analyzer P)
	ps        RolePairedState      // picker-local per-role demand (from initRoleState)
	roles     []string             // active roles for this model
	remaining float64              // fair-share priority metric (negative = fully satisfied)
	targets   map[string]int       // variant name → target replicas (ALL variants)
	limited   gpuLimitTracker      // variants the GPU budget cut short (see applyGPULimitAttribution)
}

// fairShareValue computes the fair-share priority metric for one model.
// Phase 3: reads picker-local role-remaining (Score × Σ_role pickerState[role])
// so the metric reflects actual per-role demand remaining rather than the
// P-anchor model-level scalar.
//
//	fsv = priority × Score × Σ_role pickerState[role]
//
// Falls back to max remaining demand when the weighted result is zero.
func fairShareValue(priority float64, e NamedAnalyzerResult, ps RolePairedState, roles []string) float64 {
	if e.Result != nil {
		roleSum := 0.0
		for _, role := range roles {
			roleSum += ps[role]
		}
		if fsv := priority * roleSum * e.Score; fsv > 0 {
			return fsv
		}
	}
	// Fallback: max remaining demand across roles when Score=0 or priority=0.
	maxDemand := 0.0
	for _, role := range roles {
		if d := ps[role]; d > maxDemand {
			maxDemand = d
		}
	}
	return maxDemand
}

// Optimize produces VariantDecisions for all models, fair-sharing GPUs across
// models that need to scale up. Scale-down models are handled independently.
func (o *GreedyByScoreOptimizer) Optimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []domain.VariantDecision {
	logger := ctrl.LoggerFrom(ctx).WithName(o.Name())
	available := mergeConstraints(constraints)
	availableByNS := mergeNamespaceConstraints(constraints)
	if len(constraints) == 0 {
		// No constraints at all is not "no cluster bound": this optimizer denies
		// rather than guesses. The engine never runs it so -- with no provider
		// available it falls back to the cost-aware optimizer, its unlimited path.
		available = map[string]int{}
	}
	// Cohort borrowing, when any constraint counts it: what each namespace may
	// still draw past its guaranteed figure, the shared pools, the node view.
	// Nil otherwise, and every path below is then exactly as before.
	bs := newBorrowState(constraints, o.Nodes)
	if bs != nil {
		bs.blocked = o.BorrowBlocked
	}
	if !o.quiet {
		o.draws = nil
		defer func() { o.draws = bs.drawsByVariant() }()
	}

	// Rescale pre-pass: for enabled, contended (type, budget-scope) groups, compute
	// priority-weighted targets and produce reclaim/fill decisions, consuming free
	// GPUs from `available`/`availableByNS` so the additive path below sees the
	// reduced budget. Models it handles are excluded from the additive path. When
	// rescale is off or no group is contended, `handled` is empty and behaviour is
	// unchanged.
	var rescaleDecisions []domain.VariantDecision
	var handled map[string]bool
	if o.Rescale.any() {
		if bs == nil {
			rescaleDecisions, handled = o.applyRescale(ctx, requests, constraints, available, availableByNS)
		} else {
			rescaleDecisions, handled = o.applyRescaleWithinNominal(ctx, requests, constraints, available, availableByNS, bs)
		}
	}

	var scaleUpWork []*modelWork
	var otherRequests []ModelScalingRequest

	for _, req := range requests {
		if handled[modelKey(req)] {
			continue
		}
		records := recordsForRequest(req)
		if records == nil {
			continue
		}

		e := req.CompositeSignal
		roles, ps := initRoleState(&e)
		fsv := fairShareValue(req.Priority, e, ps, roles)
		if anyRoleNeedsScaleUp(ps, roles) || fsv > 0 {
			w := o.buildScaleUpWork(req, records, &e, ps, roles, fsv)
			if w != nil {
				scaleUpWork = append(scaleUpWork, w)
			}
		} else {
			otherRequests = append(otherRequests, req)
		}
	}

	o.fairShareScaleUp(ctx, scaleUpWork, available, availableByNS, bs)

	allDecisions := make([]domain.VariantDecision, 0, len(scaleUpWork))

	for _, w := range scaleUpWork {
		stateMap := buildStateMap(w.req.VariantStates)
		vcMap := buildCapacityMap(w.records)
		decisions := buildDecisionsWithOptimizer(w.req, stateMap, vcMap, w.targets, "greedy-by-score")
		o.attributeGPULimits(decisions, w.limited, constraints)
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (scale-up)",
			"modelID", w.req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	for _, req := range otherRequests {
		records := recordsForRequest(req)
		if records == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(records)
		targets := initTargets(req.VariantStates)

		// Unified scale-down path via scaleDownRoleIterated.
		e := req.CompositeSignal
		_, _ = initRoleState(&e) // populates RoleSpare for all roles
		scaleDownRoleIterated(ctx, &e, records, targets, stateMap)

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (other)",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	allDecisions = append(allDecisions, rescaleDecisions...)
	return allDecisions
}

// buildScaleUpWork creates a single work unit for a scale-up request.
func (o *GreedyByScoreOptimizer) buildScaleUpWork(req ModelScalingRequest, records []variantRecord, e *NamedAnalyzerResult, ps RolePairedState, roles []string, fsv float64) *modelWork {
	if fsv <= 0 {
		return nil
	}
	return &modelWork{
		req:       req,
		e:         e,
		records:   records,
		ps:        ps,
		roles:     roles,
		remaining: fsv,
		targets:   initTargets(req.VariantStates),
		limited:   make(gpuLimitTracker),
	}
}

// fairShareScaleUp implements the iterative mean-based fair-sharing algorithm.
func (o *GreedyByScoreOptimizer) fairShareScaleUp(
	ctx context.Context,
	work []*modelWork,
	available map[string]int,
	availableByNS map[string]map[string]int,
	bs *borrowState,
) {
	logger := ctrl.LoggerFrom(ctx)

	for {
		active := filterActive(work)
		if len(active) == 0 {
			break
		}

		// Nothing left for any model still asking. Per model, because the
		// cluster budget alone does not say: with no cluster bound in force it
		// is nil (unbounded) and a namespace's allowance is what runs out.
		if !anyBudgetLeft(active, available, availableByNS, bs) {
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs remaining, stopping fair-share")
			break
		}

		mean := computeMean(active)
		logger.V(logging.DEBUG).Info("GreedyByScore: iteration",
			"activeModels", len(active), "meanRemaining", mean)

		sortByRemainingDesc(active)
		w := active[0]

		allocationMean := mean
		if len(active) == 1 {
			allocationMean = 0
		} else if w.remaining <= mean {
			allocationMean = mean - (w.remaining / float64(len(active)))
		}

		allocated := o.allocateForModel(ctx, w, allocationMean, available, availableByNS, bs)

		if !allocated {
			w.remaining = -1
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs available for model, removing",
				"model", w.req.ModelID)
			continue
		}

		if w.remaining > mean {
			logger.V(logging.DEBUG).Info("GreedyByScore: model still above mean, removing",
				"model", w.req.ModelID, "remaining", w.remaining, "mean", mean)
			w.remaining = -1
		}
	}
}

// allocateForModel allocates replicas to bring the model's remaining score below
// the mean. Dispatches to the paired path for disaggregated models.
// After allocation, w.remaining is recomputed from the working slice.
func (o *GreedyByScoreOptimizer) allocateForModel(
	ctx context.Context,
	w *modelWork,
	mean float64,
	available map[string]int,
	availableByNS map[string]map[string]int,
	bs *borrowState,
) bool {
	target := w.remaining - mean
	if target <= 0 {
		return false
	}

	stateMap := buildStateMap(w.req.VariantStates)
	oldRemaining := w.remaining

	// Re-initialize picker-state from current e.Remaining each call so
	// multi-iteration fair-sharing sees the correct post-allocation demand.
	// Cap at target so the loop exits when the fair-share budget is exhausted.
	_, ps := initRoleState(w.e)
	for _, role := range w.roles {
		if ps[role] > target {
			ps[role] = target
		}
	}

	// This model belongs to one namespace, so its per-type budget is the
	// minimum of the cluster-wide budget and this namespace's quota. The shared
	// allocateForModelPaired only understands a flat per-type budget, so we pass
	// the effective copy and reconcile consumption back to both budgets
	// afterwards. A non-nil nsBudget means a closed namespace-quota allowlist
	// (see effectiveAvailable); nil means the namespace is open (cluster-scope
	// quota or an excluded namespace).
	//
	// nsBudget is a reference into the cycle-wide availableByNS map, shared by
	// every model in this namespace; the reconcile below decrements it in place,
	// so later same-namespace models correctly see the reduced remaining budget.
	nsBudget := availableByNS[w.req.Namespace]
	// A namespace that borrows is sized against its reach -- guaranteed plus
	// borrow cap -- and the placer decides, replica by replica, what of it the
	// guaranteed budget, the shared pools and the nodes actually hold.
	reach := bs.reachFor(w.req.Namespace)
	sizing := nsBudget
	if reach != nil {
		sizing = reach
	}
	effAvail := effectiveAvailable(available, sizing)

	// Unified path: fairShareRolePick behind the RolePickFn interface.
	// α logic removed in commit 3.
	pick := fairShareRolePick(target, w.roles, w.limited)
	spent := allocateForModelPairedPlaced(ctx, w.e, w.records, stateMap, effAvail,
		w.targets, pick, ps, w.roles, bs.newModelPlacer(w.req.Namespace, w.req.ModelID, nsBudget, w.targets))

	// Reconcile: apply what was spent to the cluster-wide budget and, where
	// this namespace caps the type, to the namespace budget. What was spent, not
	// the change in effAvail: the in-loop debit also drew down every key that
	// covers the one spent, so a GPU spent under a long spelling would be
	// counted again under its short name. Only decrement the cluster budget for
	// types it actually constrains (a type present in `available`); a type
	// bounded solely by the namespace must not drive the cluster budget
	// negative and pollute the loop's totalGPUs check. Sorted, so the clamps in
	// debitBudget do not depend on map order.
	for _, accType := range slices.Sorted(maps.Keys(spent)) {
		consumed := spent[accType]
		if consumed <= 0 {
			continue
		}
		// Decrement only a FINITE cluster budget. An unbounded (math.MaxInt)
		// budget marks an unlimited-quota type and must stay exactly math.MaxInt:
		// depleting it to MaxInt-consumed would (a) be meaningless (you cannot
		// draw down infinity) and (b) defeat the fairShareScaleUp stop-check,
		// whose `== math.MaxInt` guard would then miss it and let the totalGPUs
		// sum overflow with two or more unlimited types.
		// accType is the namespace allowlist's spelling when the namespace is
		// closed, which need not be the cluster pool's: resolve by identity.
		// debitBudget also debits the family key that covers this one, and
		// leaves a nil (unbounded) cluster budget nil.
		debitBudget(available, budgetKey(available, accType), consumed)
		// The namespace budget the same way: accType is its own key, and a
		// family key the namespace also lists draws down with it. The
		// unlimited sentinel (negative) is never drawn down, and nothing goes
		// below 0: for a borrowing namespace the guaranteed budget is spent
		// first, and what went past it was borrowed, which the placer has
		// already debited from the shared pools.
		debitBudget(nsBudget, accType, consumed)
		if reach != nil {
			debitBudget(reach, accType, consumed)
		}
	}

	// Recompute w.remaining for fair-share ordering.
	// For "both" (non-disag): use fresh ps so applyAllocation-decremented
	// e.Remaining is read (budget-capped ps is already 0).
	// For P/D: use local capped ps which correctly reaches 0 when both roles served.
	if len(w.roles) == 1 && w.roles[0] == domain.RoleBoth {
		_, freshPs := initRoleState(w.e)
		w.remaining = fairShareValue(w.req.Priority, *w.e, freshPs, w.roles)
	} else {
		w.remaining = fairShareValue(w.req.Priority, *w.e, ps, w.roles)
	}
	return w.remaining < oldRemaining
}

// effectiveAvailable returns the per-type budget the optimizer may spend on a
// model in this namespace, given the cluster-wide budget and the namespace's
// quota. A budget value < 0 in nsBudget is the "unlimited" sentinel for that
// (namespace, type); all other values are finite GPU counts.
//
// nsBudget == nil → the namespace is OPEN (a cluster-scope quota, or an
// excluded namespace): the model is bound only by the cluster budget, so the
// result is a copy of `available` -- nil, unbounded, when no cluster bound is
// in force.
//
// nsBudget != nil → the namespace is a CLOSED allowlist (namespace-scope
// quota): the model may use ONLY the accelerator types the namespace lists,
// as NamespaceResourcePools materialized them. The result is therefore built
// from nsBudget alone — a type the namespace does not list is absent, and the
// optimizer's gpusAvail==0 check denies it (no fall-through to the cluster
// aggregate, which previously let one namespace draw on another's quota). For a
// listed type: a finite cap binds at min(cluster, cap); an unlimited cap binds
// at the cluster budget for that type, or is unbounded (math.MaxInt) when the
// cluster does not constrain it. The cluster budget is found by accelerator
// identity (clusterBudgetFor), since the namespace allowlist and the cluster
// pools may spell one GPU differently, and a type a non-nil cluster budget does
// not hold is 0 there: every cluster bound is a closed allowlist.
func effectiveAvailable(available, nsBudget map[string]int) map[string]int {
	if nsBudget == nil {
		return maps.Clone(available)
	}
	eff := make(map[string]int, len(nsBudget))
	for accType, nsAvail := range nsBudget {
		cv, clusterCapped := clusterBudgetFor(available, accType)
		if nsAvail < 0 { // unlimited for this (namespace, type)
			if clusterCapped {
				eff[accType] = cv
			} else {
				eff[accType] = math.MaxInt
			}
			continue
		}
		eff[accType] = nsAvail
		if clusterCapped && cv < nsAvail {
			eff[accType] = cv
		}
	}
	return eff
}

// fairShareRolePick returns a RolePickFn for the unified allocateForModelPaired loop.
// Each role receives the same target fair-share budget. The joint Δ_util commit
// inside allocateForModelPaired enforces P/D coupling — α is no longer needed.
//
// limited collects the variants the GPU budget cut short, so the decisions built
// afterwards can carry WasLimited / LimitedBy. A variant stopped by its own
// MaxReplicas ceiling is never recorded: that is user intent, not scarcity.
func fairShareRolePick(target float64, roles []string, limited gpuLimitTracker) RolePickFn {
	_ = roles // roles available for future per-role budget splitting
	return func(
		role string,
		variants []variantRecord,
		stateMap map[string]domain.VariantReplicaState,
		available map[string]int,
		targets map[string]int,
	) (string, int) {
		roleVCs := variantsForRole(variants, role)
		// Variants the GPU budget could not fit even one replica of. They only
		// count as constrained if the role goes unserved below — a variant
		// skipped while a cheaper sibling still satisfies the role's demand was
		// not the thing scarcity held back.
		var gpuBlocked []string
		for _, vc := range sortByCostEfficiencyAsc(roleVCs) {
			if vc.PerReplicaCapacity <= 0 {
				continue
			}
			state := stateMap[vc.VariantName]
			gpusPR := state.GPUsPerReplica
			if gpusPR <= 0 {
				gpusPR = 1
			}
			gpusAvail := budgetOf(available, vc.AcceleratorName)
			if gpusAvail < gpusPR {
				gpuBlocked = append(gpuBlocked, vc.VariantName)
				continue
			}
			fairShareCap := int(math.Ceil(target / vc.PerReplicaCapacity))
			gpuCap := gpusAvail / gpusPR
			capN := min(fairShareCap, gpuCap)
			if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
				headroom := *state.MaxReplicas - targets[vc.VariantName]
				if headroom <= 0 {
					continue
				}
				capN = min(capN, headroom)
			}
			if capN > 0 {
				// The variant grew, but short of the fair share it asked for
				// because the GPU budget ran out first. capN != gpuCap means
				// MaxReplicas was the tighter bound, which is not scarcity.
				if gpuCap < fairShareCap && capN == gpuCap {
					limited.mark(vc.VariantName)
				}
				return vc.VariantName, capN
			}
		}
		// The role goes unserved this pass: whatever the GPU budget locked out
		// really was held back by scarcity.
		for _, v := range gpuBlocked {
			limited.mark(v)
		}
		return "", 0
	}
}

// anyBudgetLeft reports whether some model in active could still be granted a
// GPU: its effective budget (effectiveAvailable) is not empty. A model in a
// namespace with an allowance is bounded by it -- by its reach when the
// namespace borrows, since its guaranteed allowance may be spent while the
// shared pools still hold GPUs; one without reads the cluster budget, which is
// unbounded when nil.
func anyBudgetLeft(active []*modelWork, available map[string]int, availableByNS map[string]map[string]int, bs *borrowState) bool {
	for _, w := range active {
		sizing := availableByNS[w.req.Namespace]
		if reach := bs.reachFor(w.req.Namespace); reach != nil {
			sizing = reach
		}
		if budgetTotal(effectiveAvailable(available, sizing)) > 0 {
			return true
		}
	}
	return false
}

// filterActive returns modelWork entries that still have remaining > 0.
func filterActive(work []*modelWork) []*modelWork {
	var active []*modelWork
	for _, w := range work {
		if w.remaining > 0 {
			active = append(active, w)
		}
	}
	return active
}

// computeMean returns the average remaining across active models.
func computeMean(active []*modelWork) float64 {
	if len(active) == 0 {
		return 0
	}
	total := 0.0
	for _, w := range active {
		total += w.remaining
	}
	return total / float64(len(active))
}

// sortByRemainingDesc sorts active models by remaining descending.
func sortByRemainingDesc(active []*modelWork) {
	sort.Slice(active, func(i, j int) bool {
		return active[i].remaining > active[j].remaining
	})
}

// prcFromVCs returns the PerReplicaCapacity for variant v from a slice of VCs.
func prcFromVCs(vcs []variantRecord, v string) float64 {
	for _, vc := range vcs {
		if vc.VariantName == v {
			return vc.PerReplicaCapacity
		}
	}
	return 0
}

// accFromVCs returns the AcceleratorName for variant v from a slice of VCs.
func accFromVCs(vcs []variantRecord, v string) string {
	for _, vc := range vcs {
		if vc.VariantName == v {
			return vc.AcceleratorName
		}
	}
	return ""
}

// gpusPerReplicaFromState returns GPUsPerReplica for variant v, defaulting to 1.
func gpusPerReplicaFromState(stateMap map[string]domain.VariantReplicaState, v string) int {
	if state, ok := stateMap[v]; ok && state.GPUsPerReplica > 0 {
		return state.GPUsPerReplica
	}
	return 1
}

// Ensure GreedyByScoreOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*GreedyByScoreOptimizer)(nil)
