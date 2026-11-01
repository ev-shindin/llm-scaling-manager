package allocation

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// GreedyByScoreOptimizer is a multi-model optimizer for GPU-constrained
// environments. It uses iterative mean-based fair-sharing to distribute scarce
// GPUs across competing models, ordered by fair-share priority value
// (priority × Score × Σ_role remaining demand, on the one composite entry; see
// fairShareValue).
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

	// pass is what the last pass left of the budgets, for the warm pool's
	// headroom (RepublishHeadroom) and the utilization share (LastPass). Not
	// recorded by a shadow pass; dropped when the next pass starts.
	pass *passHeadroom

	// spends is what this pass took from the budgets, in order, per model:
	// replayed to take back what the pass ordered for a model whose target the
	// utilization share overrides (passHeadroom.without).
	spends []spendOp

	// Nodes is the cycle's per-node picture of free GPUs (set by the engine
	// before Optimize), nil when no physical limiter bounds the cycle. With
	// it, a replica must fit a node, not just the pool: "no node holds a whole
	// replica" is scarcity, a P/D pair needs both roles placed, and every
	// replica ordered is debited from its node for the rest of the pass. A
	// pass consumes it: Optimize clears it when done, so a later pass does not
	// place on the holes an earlier one already took (the engine sets a fresh
	// one every cycle). What the pass left is kept in its headroom.
	//
	// A pass that counts cohort borrowing places through the same view: its
	// placer (modelPlacer) fits every replica of a model, guaranteed or
	// borrowed, and debits it once, charged to the model, so a hole one model
	// takes is gone for every other, borrowing or not, and for the utilization
	// share's idle fill after the pass.
	Nodes *NodeView

	// BorrowBlocked, when set, reports whether a variant may not grow on
	// borrowed capacity this cycle (a placement hold, a model cooldown; see
	// BorrowGovernor.BorrowBlocked). Its guaranteed growth is unaffected.
	BorrowBlocked func(namespace, modelID, variant string) bool

	// BorrowBlockedIn, when set, reports whether a model may not draw on one
	// cohort's shared pools this cycle: its cooldown there, backed off after
	// repeated preemptions or placement timeouts (BorrowGovernor.BorrowBlockedIn).
	BorrowBlockedIn func(namespace, modelID, cohort string) bool

	// ReclaimBlocked, when set, reports whether a model's growth into nominal
	// lent out may not skip the node hole this cycle: its cooldown after a
	// replica ordered without one never placed and was released
	// (BorrowGovernor.CoolingDown, M3). Its growth then needs a real hole.
	ReclaimBlocked func(namespace, modelID string) bool

	// BorrowCredit, when set, is the borrowed draws still in flight
	// (BorrowInFlight.Entries) on cohorts open this cycle: the pools were
	// debited them, and a variant's targets restart from its current
	// replicas, so its own draw is re-placed on its own GPUs rather than
	// counted against it (A4). Without it the target of a variant whose pods
	// are not created yet alternated, 3/2/3, one cycle on the pool, the next
	// refused by its own draw.
	BorrowCredit []BorrowDraw

	// ReclaimCredit, when set, is the growth by Kueue's reclaim still in
	// flight (BorrowInFlight.Reclaims), the steady-state engine's and the
	// wakes': DeductReclaimed took it off its namespace's LentOut so that no
	// other decision counts the same borrowed GPUs again, and a variant's
	// targets restart from its current replicas, so its own entry is
	// re-placed on those GPUs rather than refused by them (NEW-1). Without
	// it, a variant whose reclaimed replica's pods were not created yet was
	// re-placed against a LentOut of 0, node fit refused it, Trim dropped the
	// entry, and the next cycle ordered it again: 3/2/3, with Kueue
	// preempting a borrower for nothing in between.
	ReclaimCredit []BorrowDraw

	// WakeReclaims, when set, is the scale-from-zero wakes' growth into
	// nominal their queue lent out that no pass has yielded for yet
	// (BorrowInFlight.PendingWakeReclaims): Kueue preempts borrowers to admit
	// it, so the pass lowers this controller's own borrowers in the cohort
	// as it does for a reclaim it orders itself (yieldReclaimed). The engine
	// reports them yielded after the pass (YieldedWakeReclaims), so each
	// lowers a borrower once.
	WakeReclaims []BorrowDraw

	// draws is what the last non-shadow pass borrowed (BorrowDraws).
	draws []BorrowDraw
	// reclaims is what the last non-shadow pass grew into by Kueue's
	// preemption of borrowers (ReclaimDraws).
	reclaims []BorrowDraw
	// wakeYielded is the wakes' reclaims the last non-shadow pass yielded
	// for (WakeReclaimsYielded).
	wakeYielded []BorrowDraw
	// flavors is what the last non-shadow pass placed on Kueue flavors'
	// nodes (PlacedFlavors).
	flavors []flavorUse
}

// BorrowDraws returns what the last Optimize pass drew from the cohorts'
// shared pools, one entry per variant and chunk; nil when it borrowed nothing.
// The engine keeps these as in flight until the pods exist.
func (o *GreedyByScoreOptimizer) BorrowDraws() []BorrowDraw {
	return o.draws
}

// ReclaimDraws returns the guaranteed replicas the last Optimize pass placed
// on nominal Kueue reclaims from borrowers (BorrowDraw.Reclaim), one entry per
// replica; nil when it placed none. The engine keeps them in flight, as it
// does the draws, so neither a later cycle nor the wake spends the same
// borrowed GPUs again before a read shows them.
func (o *GreedyByScoreOptimizer) ReclaimDraws() []BorrowDraw {
	return o.reclaims
}

// WakeReclaimsYielded returns the wakes' reclaims (WakeReclaims) the last
// non-shadow pass yielded for, for the engine to report to the record
// (BorrowInFlight.YieldedWakeReclaims); nil when the pass did not reach the
// yield.
func (o *GreedyByScoreOptimizer) WakeReclaimsYielded() []BorrowDraw {
	return o.wakeYielded
}

// spendOp is one debit a pass made: gpus from key of the cluster budget (ns
// "") or of namespace ns's, for model (namespace/modelID).
type spendOp struct {
	model, ns, key string
	gpus           int
}

// spend records a debit the pass made, for model.
func (o *GreedyByScoreOptimizer) spend(model, ns, key string, gpus int) {
	if !o.quiet {
		o.spends = append(o.spends, spendOp{model: model, ns: ns, key: key, gpus: gpus})
	}
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
	// The shadow is what the quota alone would allow: no physical limiter,
	// so no node fit, and the cycle's view is not consumed.
	shadow.Nodes = nil
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

// maxShareWeight bounds a model's weight in the fair share and in rescale, so
// a sum of weights over any number of models stays finite. Configuration
// already bounds priority and score (config.MaxPrecedence); this covers a
// policy built in code, and a demand large enough to overflow the product.
const maxShareWeight = 1e300

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
			// Bounded, so the mean across models stays finite: an infinite
			// value made the model's target Inf - Inf = NaN and it got nothing.
			return min(fsv, maxShareWeight)
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
	// One pass consumes one node view; what it left is in o.pass. A shadow
	// pass runs on a copy and touches neither.
	defer func() { o.Nodes = nil }()
	if !o.quiet {
		o.pass, o.spends = nil, nil
	}
	declared := declaredAccelerators(requests)
	// The cluster budget holds every key a namespace allowance lists, so the
	// bound on each is the same whatever names the workloads declare; the warm
	// pool's cluster figure leaves those keys out (passHeadroom.clusterKeys).
	// The namespace budgets hold the cluster's products, so the warm pool
	// reads each product a namespace may use at its own figure.
	available := mergeWithAllowanceKeys(constraints, declared...)
	clusterKeys := slices.Collect(maps.Keys(mergeConstraints(constraints, declared...)))
	availableByNS := mergeNamespaceConstraints(constraints, slices.Concat(declared, clusterProducts(constraints))...)
	if len(constraints) == 0 {
		// No constraints at all is not "no cluster bound": this optimizer denies
		// rather than guesses. The engine never runs it so -- with no provider
		// available it falls back to the cost-aware optimizer, its unlimited path.
		available = map[string]int{}
	}
	// The budgets as the pass found them, to replay its spends on (spends).
	start, startByNS := maps.Clone(available), cloneNamespaceBudgets(availableByNS)
	// Cohort borrowing, when any constraint counts it: what each namespace may
	// still draw past its guaranteed figure, the shared pools, the node view.
	// Nil otherwise, and every path below is then exactly as before -- unless
	// a Kueue-backed namespace carries nominal co-resource headroom: then the
	// state holds that alone (counts false), only that namespace's models are
	// placed by a placer that checks it, and rescale and every other model
	// take the paths they take without borrowing.
	bs := newBorrowState(constraints, o.Nodes)
	if bs != nil {
		bs.blocked = o.BorrowBlocked
		bs.blockedIn = o.BorrowBlockedIn
		bs.reclaimBlocked = o.ReclaimBlocked
		bs.setCredit(o.BorrowCredit)
		bs.setReclaimCredit(o.ReclaimCredit)
		bs.wakeReclaims = o.WakeReclaims
	}
	if !o.quiet {
		o.draws, o.flavors, o.reclaims, o.wakeYielded = nil, nil, nil, nil
		defer func() {
			o.draws = bs.drawsByVariant()
			if bs != nil {
				o.flavors = bs.flavors
				o.reclaims = bs.reclaims
			}
		}()
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
		if bs == nil || !bs.counts {
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
	// A reclaim the pass ordered takes borrowed GPUs a managed model holds:
	// that model gives them back in the same pass (KU3).
	if lowered := bs.yieldReclaimed(allDecisions, requests); len(lowered) > 0 && !o.quiet {
		logger.Info(ReclaimYieldLogMessage, "replicasByVariant", lowered)
	}
	if !o.quiet && bs != nil {
		o.wakeYielded = bs.wakeReclaims
	}

	// The warm pool's headroom, from what this pass left: every replica it
	// ordered is debited, from the budgets and from the nodes.
	if !o.quiet && len(constraints) > 0 {
		o.pass = &passHeadroom{available: available, byNamespace: availableByNS, nodes: o.Nodes,
			version: poolsVersionOf(constraints), start: start, startByNS: startByNS, spends: o.spends,
			clusterKeys: clusterKeys}
		o.pass.publish(time.Now())
	}
	return allDecisions
}

// RepublishHeadroom publishes the last pass's headroom again, less what the
// pass ordered for the models in planned (namespace/modelID: the ones the
// utilization share plans, whose targets it overrides), with promised GPUs
// (scope "" for the cluster, else a namespace -> accelerator -> holes) also
// withheld, from the pools and, held on the nodes their holes are on, from the
// node holes: those the utilization-share optimizer promised after the pass,
// which a warm pool must not take before their receiver does. Reports false
// when there is no pass to republish.
func (o *GreedyByScoreOptimizer) RepublishHeadroom(planned map[string]bool, promised map[string]map[string]decision.PromisedHoles, now time.Time) bool {
	if o.pass == nil {
		return false
	}
	o.pass.without(planned).withheld(promised).publish(now)
	return true
}

// LastPass is what the last pass left, for the utilization share's step that
// follows it; nil when there was none (or it was a shadow).
func (o *GreedyByScoreOptimizer) LastPass() *PassLeft {
	if o.pass == nil {
		return nil
	}
	return &PassLeft{h: o.pass}
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
			// The models still asking never reach their pick, which is what
			// marks a variant the budget locked out: mark them here, or the
			// model the last GPUs did not reach is held back in silence.
			markBudgetExhausted(active)
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
	// The cluster bounds count, for a namespace growing into nominal its
	// reclaiming queues lent out, the GPUs the borrowers hold there as free:
	// Kueue preempts them to admit it (withLent).
	effAvail := effectiveAvailable(bs.withLent(w.req.Namespace, stateVariants(w.req), available), sizing)

	// Unified path: fairShareRolePick behind the RolePickFn interface.
	// α logic removed in commit 3.
	fit := nodeFit{nodes: o.Nodes, namespace: w.req.Namespace, limited: w.limited}
	// Whatever this model places on the nodes is charged to it, so a later
	// reader can take back the placements of a model whose target the
	// utilization share overrides. The borrowing placer places on the same
	// view, so its placements are charged to the model too.
	defer o.Nodes.setOwner(o.Nodes.setOwner(modelKey(w.req)))
	placer := bs.newModelPlacer(w.req.Namespace, w.req.ModelID, nsBudget, w.targets, w.limited)
	pickFit := fit
	if placer != nil {
		// The placer fits the nodes itself, per replica: a borrowed one on
		// its flavor's nodes, growth into lent-out nominal whether or not a
		// hole shows; a replica it refuses for want of a node is marked as
		// held back by node fit. The pick then sizes against the pools only.
		pickFit.nodes = nil
	}
	pick := fairShareRolePickOn(target, w.roles, w.limited, pickFit)
	spent := allocateForModelPairedOn(ctx, w.e, w.records, stateMap, effAvail,
		w.targets, pick, ps, w.roles, fit, placer)

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
		key := budgetKey(available, accType)
		debitBudget(available, key, consumed)
		o.spend(modelKey(w.req), "", key, consumed)
		// The namespace budget the same way: accType is its own key, and a
		// family key the namespace also lists draws down with it. The
		// unlimited sentinel (negative) is never drawn down, and nothing goes
		// below 0: for a borrowing namespace the guaranteed budget is spent
		// first, and what went past it was borrowed, which the placer has
		// already debited from the shared pools.
		debitBudget(nsBudget, accType, consumed)
		if nsBudget != nil {
			o.spend(modelKey(w.req), w.req.Namespace, accType, consumed)
		}
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
	return fairShareRolePickOn(target, roles, limited, nodeFit{})
}

// fairShareRolePickOn is fairShareRolePick that also caps a variant at the
// replicas the nodes hold (fit.nodes): a variant no node holds a whole replica
// of is held back by scarcity, as one the pool cannot fit is.
func fairShareRolePickOn(target float64, roles []string, limited gpuLimitTracker, fit nodeFit) RolePickFn {
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
		// not the thing scarcity held back. nodeBlocked are those the pools
		// held but no node did: node fit is then the cause named.
		var gpuBlocked, nodeBlocked []string
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
			nodeCut := false
			if fit.nodes != nil && capN > 0 {
				// The nodes may hold fewer whole replicas than the pool has
				// GPUs for; that is scarcity too, of nodes rather than GPUs.
				onNodes := fit.nodes.fits(fit.replica(variants, stateMap, vc.VariantName), capN)
				if onNodes == 0 {
					nodeBlocked = append(nodeBlocked, vc.VariantName)
					continue
				}
				if onNodes < capN {
					capN, gpuCap, nodeCut = onNodes, onNodes, true
				}
			}
			if capN > 0 {
				// The variant grew, but short of the fair share it asked for
				// because the GPU budget ran out first. capN != gpuCap means
				// MaxReplicas was the tighter bound, which is not scarcity.
				if gpuCap < fairShareCap && capN == gpuCap {
					if nodeCut {
						limited.markNodeFit(vc.VariantName)
					} else {
						limited.mark(vc.VariantName)
					}
				}
				return vc.VariantName, capN
			}
		}
		// The role goes unserved this pass: whatever the GPU budget locked out
		// really was held back by scarcity.
		for _, v := range gpuBlocked {
			limited.mark(v)
		}
		for _, v := range nodeBlocked {
			limited.markNodeFit(v)
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
		if budgetTotal(effectiveAvailable(bs.withLent(w.req.Namespace, stateVariants(w.req), available), sizing)) > 0 {
			return true
		}
	}
	return false
}

// markBudgetExhausted marks, for each model in active, every variant that would
// have grown had any budget been left: a variant serving a role that still has
// demand, short of its own MaxReplicas ceiling. The pick marks these itself
// when it runs (fairShareRolePickOn's gpuBlocked); the fair-share loop stops
// before it runs once no model has budget left, so the models still asking
// must be marked here. A variant an earlier cut already marked keeps that
// cause. applyGPULimitAttribution then names the binding limiter
// (bindingProvider).
func markBudgetExhausted(active []*modelWork) {
	for _, w := range active {
		if w.e == nil {
			continue
		}
		_, ps := initRoleState(w.e)
		stateMap := buildStateMap(w.req.VariantStates)
		for _, role := range w.roles {
			if ps[role] <= 0 {
				continue
			}
			for _, vc := range variantsForRole(w.records, role) {
				if vc.PerReplicaCapacity <= 0 {
					continue
				}
				if _, marked := w.limited[vc.VariantName]; marked {
					continue
				}
				st := stateMap[vc.VariantName]
				if st.MaxReplicas != nil && *st.MaxReplicas > 0 && w.targets[vc.VariantName] >= *st.MaxReplicas {
					continue
				}
				w.limited.mark(vc.VariantName)
			}
		}
	}
}

// stateVariants is the names of a request's variants, as its states list them.
func stateVariants(req ModelScalingRequest) []string {
	out := make([]string, 0, len(req.VariantStates))
	for _, s := range req.VariantStates {
		out = append(out, s.VariantName)
	}
	return out
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
