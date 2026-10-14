package allocation

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// CostAwareOptimizer is a per-model optimizer that minimizes total cost while
// meeting capacity requirements. It processes each model independently:
//
//   - Scale-up: adds replicas to the most cost-efficient variant (lowest cost / perReplicaCapacity)
//   - Scale-down: removes replicas from the most expensive variant (highest absolute cost)
//   - Only the cheapest variant is protected at >=1 replica; others can scale to 0
//   - Variants with pending replicas are skipped for scale-up
//
// This optimizer ignores ResourceConstraints (unlimited mode). For GPU-limited
// environments, use GreedyByScoreOptimizer instead.
type CostAwareOptimizer struct{}

// NewCostAwareOptimizer creates a new CostAwareOptimizer.
func NewCostAwareOptimizer() *CostAwareOptimizer {
	return &CostAwareOptimizer{}
}

// Name returns the optimizer identifier.
func (o *CostAwareOptimizer) Name() string {
	return "cost-aware"
}

// Optimize produces VariantDecisions for all models.
// Constraints are ignored in unlimited mode (CostAwareOptimizer).
func (o *CostAwareOptimizer) Optimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []domain.VariantDecision {
	logger := ctrl.LoggerFrom(ctx).WithName(o.Name())
	var allDecisions []domain.VariantDecision

	for _, req := range requests {
		records := recordsForRequest(req)
		if records == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(records)
		targets := initTargets(req.VariantStates)

		// Unified dispatch: one path for all models via (model, role) math.
		// Non-disaggregated uses synthetic "both" role; disaggregated uses actual roles.
		e := req.CompositeSignal
		roles, ps := initRoleState(&e)
		if anyRoleNeedsScaleUp(ps, roles) {
			allocateForModelPaired(ctx, &e, records, stateMap, nil, targets,
				costGreedyRolePick, ps, roles)
		} else {
			scaleDownRoleIterated(ctx, &e, records, targets, stateMap)
		}

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "cost-aware")
		logger.V(logging.DEBUG).Info("Cost-aware optimizer decisions",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// costGreedyRolePick is a RolePickFn that picks the cheapest-by-cost-efficiency
// variant in the given role. For "both" (non-disaggregated), all variants are
// eligible. For role-tagged roles, only variants with a matching Role are picked.
func costGreedyRolePick(
	role string,
	variants []variantRecord,
	stateMap map[string]domain.VariantReplicaState,
	_ map[string]int,
	targets map[string]int,
) (string, int) {
	roleVCs := variantsForRole(variants, role)
	for _, vc := range sortByCostEfficiencyAsc(roleVCs) {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		state := stateMap[vc.VariantName]
		if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
			headroom := *state.MaxReplicas - targets[vc.VariantName]
			if headroom <= 0 {
				continue
			}
			return vc.VariantName, headroom
		}
		return vc.VariantName, math.MaxInt
	}
	return "", 0
}

// scaleDownVariantSet sheds replicas from sortedVariants (PRE-SORTED cost-desc,
// cheapest last). minReplicas floor and cheapest-at-1 protection are enforced
// here. maxRemovable returns how many replicas of vc the caller permits to remove;
// onRemove is invoked after committing n so the caller can update its spare bookkeeping.
func scaleDownVariantSet(
	ctx context.Context,
	sortedVariants []variantRecord,
	targets map[string]int,
	states map[string]domain.VariantReplicaState,
	maxRemovable func(vc variantRecord) int,
	onRemove func(vc variantRecord, n int),
) {
	logger := ctrl.LoggerFrom(ctx)
	for i, vc := range sortedVariants {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		current := targets[vc.VariantName]
		minReplicas := 0
		if states != nil {
			if st, ok := states[vc.VariantName]; ok && st.MinReplicas != nil {
				minReplicas = *st.MinReplicas
			}
		}
		removable := current - minReplicas
		if removable <= 0 {
			continue
		}
		n := maxRemovable(vc)
		if n > removable {
			n = removable
		}
		// cheapest-at-1: the last (cheapest) variant is protected at 1 only when no
		// more-expensive variant still holds replicas (#1237's positional rule).
		if i == len(sortedVariants)-1 && current-n < 1 && !anyHasReplicas(sortedVariants[:i], targets) {
			n = current - 1
		}
		if n <= 0 {
			continue
		}
		targets[vc.VariantName] = current - n
		onRemove(vc, n)
		logger.V(logging.DEBUG).Info("scale-down: removed replicas",
			"variant", vc.VariantName, "removed", n, "cost", vc.Cost)
	}
}

// sortVariantsForScaleDown orders a role's variants for cost-greedy scale-down:
//  1. Cost descending — shed the most expensive first.
//  2. Tie: score × PRC ascending — Score·PRC[v] (single entry).
//  3. Tie: variant name ascending — full determinism.
func sortVariantsForScaleDown(e NamedAnalyzerResult, roleVCs []variantRecord) []variantRecord {
	weighted := func(name string) float64 {
		if e.Result == nil {
			return 0
		}
		return e.Score * prcForVariant(e.Result, name)
	}
	out := append([]variantRecord(nil), roleVCs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		wi, wj := weighted(out[i].VariantName), weighted(out[j].VariantName)
		if wi != wj {
			return wi < wj
		}
		return out[i].VariantName < out[j].VariantName
	})
	return out
}

// anyHasReplicas reports whether any of the given variants has a positive target.
func anyHasReplicas(variants []variantRecord, targets map[string]int) bool {
	for _, vc := range variants {
		if targets[vc.VariantName] > 0 {
			return true
		}
	}
	return false
}

// buildStateMap creates a lookup map from variant name to VariantReplicaState.
func buildStateMap(states []domain.VariantReplicaState) map[string]domain.VariantReplicaState {
	m := make(map[string]domain.VariantReplicaState, len(states))
	for _, s := range states {
		m[s.VariantName] = s
	}
	return m
}

// buildCapacityMap creates a lookup map from variant name to VariantCapacity.
func buildCapacityMap(capacities []variantRecord) map[string]variantRecord {
	m := make(map[string]variantRecord, len(capacities))
	for _, vc := range capacities {
		m[vc.VariantName] = vc
	}
	return m
}

// initTargets creates initial targets from current replica counts.
func initTargets(states []domain.VariantReplicaState) map[string]int {
	targets := make(map[string]int, len(states))
	for _, s := range states {
		targets[s.VariantName] = s.CurrentReplicas
	}
	return targets
}

// sortByCostEfficiencyAsc returns variants sorted by cost/perReplicaCapacity ascending.
func sortByCostEfficiencyAsc(capacities []variantRecord) []variantRecord {
	sorted := make([]variantRecord, len(capacities))
	copy(sorted, capacities)
	sort.Slice(sorted, func(i, j int) bool {
		return costEfficiency(sorted[i]) < costEfficiency(sorted[j])
	})
	return sorted
}

// costEfficiency returns the cost per unit of capacity.
func costEfficiency(vc variantRecord) float64 {
	if vc.PerReplicaCapacity <= 0 {
		return math.MaxFloat64
	}
	return vc.Cost / vc.PerReplicaCapacity
}

// buildDecisionsWithOptimizer converts targets map into VariantDecision slice.
// optimizerName is included in reason strings for observability.
func buildDecisionsWithOptimizer(
	req ModelScalingRequest,
	stateMap map[string]domain.VariantReplicaState,
	vcMap map[string]variantRecord,
	targets map[string]int,
	optimizerName string,
) []domain.VariantDecision {
	decisions := make([]domain.VariantDecision, 0, len(targets))
	// satNamed carries the model-level RequiredCapacity/SpareCapacity computed by
	// applyUniversalThreshold; per-variant Utilization is on each VariantCapacity.
	// These feed the saturation gauges (utilization/required/spare).
	satNamed := req.CompositeSignal
	for name, target := range targets {
		state := stateMap[name]
		vc := vcMap[name]

		var action domain.SaturationAction
		var decisionReason domain.DecisionReason
		var detailedReason string
		switch {
		case target > state.CurrentReplicas:
			action = domain.ActionScaleUp
			decisionReason = domain.DecisionReasonV2
			detailedReason = fmt.Sprintf("%s (optimizer: %s)", string(decisionReason), optimizerName)
		case target < state.CurrentReplicas:
			action = domain.ActionScaleDown
			decisionReason = domain.DecisionReasonV2
			detailedReason = fmt.Sprintf("%s (optimizer: %s)", string(decisionReason), optimizerName)
		default:
			action = domain.ActionNoChange
			decisionReason = domain.DecisionReasonV2
			detailedReason = string(decisionReason)
		}

		// GPUs the target commits for this variant. Unlike the replica counts it
		// is a resource figure, so it reports the WHOLE footprint at the target,
		// not the delta. An unset GPUsPerReplica reads as the 1-GPU default that
		// the rest of the optimizer assumes (gpusPerReplicaFromState).
		gpusPerReplica := state.GPUsPerReplica
		if gpusPerReplica <= 0 {
			gpusPerReplica = 1
		}

		decision := domain.VariantDecision{
			VariantName:     name,
			ModelID:         req.ModelID,
			Namespace:       req.Namespace,
			AcceleratorName: vc.AcceleratorName,
			Cost:            vc.Cost,
			Role:            state.Role,
			CurrentReplicas: state.CurrentReplicas,
			TargetReplicas:  target,
			GPUsPerReplica:  gpusPerReplica,
			GPUsAllocated:   target * gpusPerReplica,
			MinReplicas:     state.MinReplicas,
			MaxReplicas:     state.MaxReplicas,
		}
		// SetDecisionReason is the single place that sets d.Action (avoids a
		// redundant Action assignment in the struct literal above).
		decision.SetDecisionReason(action, decisionReason, detailedReason)

		// Observability fields consumed by RecordSaturationMetrics
		// (wva_saturation_utilization / wva_required_capacity / wva_spare_capacity).
		// Without these the three V2 gauges read zero. RequiredCapacity and
		// SpareCapacity are the matched scale-up/scale-down token signals from
		// applyUniversalThreshold; Utilization is the per-variant demand/capacity ratio.
		// For P/D-disaggregated models use the variant's per-role capacity; otherwise
		// fall back to the model-level totals.
		decision.Utilization = vc.Utilization
		reqCap, spareCap := satNamed.RequiredCapacity, satNamed.SpareCapacity
		role := state.Role
		if role == "" {
			role = domain.RoleBoth
		}
		if rc, ok := satNamed.RoleCapacities[role]; ok {
			reqCap, spareCap = rc.RequiredCapacity, rc.SpareCapacity
		}
		decision.RequiredCapacity = reqCap
		decision.SpareCapacity = spareCap
		// The demand that PRICED the target, so a later stage re-pricing a
		// different replica count uses the same figure: model-level (or per-role
		// for a disaggregated model), which carries the scheduler-queue estimate
		// the per-variant engine-local sum does not -- taken at THIS variant's
		// share of it, or the other variants' traffic would be priced against
		// this one's replicas and no hold could ever survive on a model with
		// two variants of the same role.
		demand := satNamed.Result.TotalDemand
		if rc, ok := satNamed.RoleCapacities[role]; ok {
			demand = rc.TotalDemand
		}
		if share, ok := variantDemandShare(satNamed.Result.VariantCapacities, vc.VariantName, role); ok {
			decision.TotalDemand = demand * share
		} else {
			decision.TotalDemand = domain.DemandUnpriced
		}
		decision.PerReplicaCapacity = vc.PerReplicaCapacity
		decision.ScaleUpThreshold = satNamed.ScaleUpThreshold

		decisions = append(decisions, decision)
	}
	return decisions
}

// variantDemandShare is the fraction of a model's (or role's) demand that
// belongs to the named variant: its engine-local demand over the engine-local
// demand of every variant of that role. One variant means 1. When nothing
// reported, the share is even, so a variant is never priced at zero for want
// of rows. When its siblings reported and it did not -- a rolling restart,
// or the router pinning the traffic elsewhere -- its share is unknown, not
// zero: a zero share would price any replica count at zero utilization and
// nothing could ever release a hold on it. Reports ok=false in that case.
func variantDemandShare(vcs []domain.VariantCapacity, variantName, role string) (float64, bool) {
	var mine, all float64
	var n int
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = domain.RoleBoth
		}
		if r != role {
			continue
		}
		n++
		all += vc.TotalDemand
		if vc.VariantName == variantName {
			mine = vc.TotalDemand
		}
	}
	switch {
	case n <= 1:
		return 1, true
	case all == 0:
		return 1 / float64(n), true
	case mine == 0:
		return 0, false
	default:
		return mine / all, true
	}
}

// mergeConstraints combines the per-type GPU budgets of every provider into one
// budget per accelerator: the INTERSECTION of what they allow, at the smallest
// figure any of them gives. Used by GreedyByScoreOptimizer and the placement
// check; lives here since CostAwareOptimizer owns the shared helpers.
//
// A type is allowed only if every provider with a view on it allows it. A
// provider whose Pools are closed (ResourceConstraints.PoolsClosed: physical
// inventory, a cluster-scope quota) has a view on every type, so a type it does
// not list is merged as an explicit 0. Before this, the minimum was taken only
// over the providers that listed a type, so a type one provider omitted passed
// whenever another listed it: a cluster quota of {H100: 8} beside a physical
// pool of {H100: 16, A100: 4} let A100 through. An open provider (a
// namespace-scope quota's aggregate of its namespace caps) bounds only the types
// it lists and denies nothing, exactly as before. The catch-all "unknown" pool
// is never zeroed, because physical inventory never lists it and a quota that
// declares it means it.
//
// The types considered are every key of every provider's Pools AND of its
// NamespacePools. Without the namespace keys, a type a closed provider denied
// would simply be missing here, and the namespace check reads a missing cluster
// key as "no cluster bound" -- letting a namespace entry of {A100: 4} re-admit
// the A100 a cluster quota of {H100: 8} denies.
//
// Types are matched by accelerator identity (accelerator.FindKey), not exact
// string: physical keys are short names ("H100") and quota keys are whatever the
// operator wrote ("NVIDIA-H100-80GB-HBM3"). The result carries one key per
// accelerator wherever the providers agree on its budget; see collapseSpellings
// for which spelling is kept.
//
// The unlimited sentinel (Limit < 0) is an unbounded budget, math.MaxInt, so the
// optimizer allocates the type up to the model's fair-share demand. Leaving it
// absent would let fairShareRolePick read a 0 budget and silently deny the type,
// inverting the -1 = unlimited semantic. A finite pool from any provider still
// wins through the minimum.
func mergeConstraints(constraints []*ResourceConstraints) map[string]int {
	names := constraintTypeNames(constraints)
	merged := make(map[string]int, len(names))
	for _, name := range names {
		budget, bounded := 0, false
		for _, c := range constraints {
			if c == nil {
				continue
			}
			b, ok := clusterBound(c, name)
			if !ok {
				continue
			}
			if !bounded || b < budget {
				budget, bounded = b, true
			}
		}
		// A name only a namespace allowlist spells, with no closed provider in
		// play, has no cluster bound at all: absent, as it always was.
		if bounded {
			merged[name] = budget
		}
	}
	return collapseSpellings(merged, func(name string) bool {
		for _, c := range constraints {
			if c == nil || !c.PoolsClosed {
				continue
			}
			if _, ok := c.Pools[name]; ok {
				return true
			}
		}
		return false
	})
}

// clusterBound is the budget one provider's per-type pools put on the
// accelerator called name, and whether it bounds it at all.
//
// A closed provider bounds every name: by its matching pool, or at 0 when it has
// none (except "unknown"; see mergeConstraints). It matches by identity in both
// directions, which is the rule the placement check has always used: its
// "NVIDIA-H100-80GB-HBM3" pool answers for an "H100", and its "H100" pool for
// the long name.
//
// An open provider bounds only the names it lists, and only through a key at
// least as broad as name (coversName). Its aggregate of {NVIDIA-H100-80GB-HBM3: 4}
// bounds that product and nothing else; reading it as a bound on every "H100"
// would cap an excluded namespace's other H100 products at a figure that was
// never theirs.
func clusterBound(c *ResourceConstraints, name string) (int, bool) {
	var (
		key    string
		listed bool
	)
	if c.PoolsClosed {
		key, listed = accelerator.FindKey(c.Pools, name)
	} else {
		key, listed = coveringKey(c.Pools, name)
	}
	if !listed {
		if c.PoolsClosed && name != constants.DefaultAcceleratorName {
			return 0, true
		}
		return 0, false
	}
	pool := c.Pools[key]
	if pool.Limit < 0 {
		return math.MaxInt, true
	}
	return pool.Available(), true
}

// constraintTypeNames is every accelerator spelling any provider uses, in its
// cluster pools or in any namespace's, sorted so the merge is deterministic.
func constraintTypeNames(constraints []*ResourceConstraints) []string {
	seen := map[string]bool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for name := range c.Pools {
			seen[name] = true
		}
		for _, perType := range c.NamespacePools {
			for name := range perType {
				seen[name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// coversName reports whether a pool keyed key bounds an accelerator called name:
// the same name ignoring case, or key is a short name that name reduces to. A
// long key never covers a short name, nor a different long name, since "H100"
// may be any of several products and only one of them is key.
func coversName(key, name string) bool {
	if strings.EqualFold(key, name) {
		return true
	}
	return accelerator.NormalizeAcceleratorName(key) == key && accelerator.SameName(key, name)
}

// coveringKey returns the key of pools that covers name (coversName): name
// itself when present, else the lexically smallest covering key, so the answer
// does not depend on map order.
func coveringKey[V any](pools map[string]V, name string) (string, bool) {
	if _, ok := pools[name]; ok {
		return name, true
	}
	var matches []string
	for key := range pools {
		if coversName(key, name) {
			matches = append(matches, key)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	return slices.Min(matches), true
}

// collapseSpellings leaves one key per accelerator where providers spelled it
// differently but agree on its budget, so a consumer cannot spend the same GPUs
// once under "H100" and again under "NVIDIA-H100-80GB-HBM3".
//
// The spelling kept is the most specific one: a long product name over a short
// name, then the lexically smallest. Every consumer looks a workload's
// accelerator up by identity (accelerator.FindKey), under which a short key
// answers for every product that reduces to it while a long key answers only for
// itself and its short name. Keeping "H100" where a closed quota granted only
// "NVIDIA-H100-80GB-HBM3" would therefore admit an "NVIDIA-H100-SXM5-80GB"
// workload the quota never granted; keeping the long name cannot. Physical keys
// are always short names, so in practice this keeps the operator's spelling.
//
// A short name is folded into a longer one only when that longer spelling is a
// key of a closed provider (closedKey): only then is the narrower product all
// there is. An open provider's long key bounds that product alone, and the
// short name stays to answer for the others. Spellings that differ only in case
// are always one accelerator and always folded. Where budgets differ the
// spellings stay apart, because they genuinely name different allowances.
func collapseSpellings(budgets map[string]int, closedKey func(string) bool) map[string]int {
	out := make(map[string]int, len(budgets))
	for name, budget := range budgets {
		folded := false
		for other, b := range budgets {
			if other == name || b != budget || !moreSpecific(other, name) || !accelerator.SameName(other, name) {
				continue
			}
			if strings.EqualFold(other, name) || closedKey(other) {
				folded = true
				break
			}
		}
		if !folded {
			out[name] = budget
		}
	}
	return out
}

// moreSpecific orders accelerator spellings for collapseSpellings: a long
// product name before a short name, then lexically. It is a strict total order,
// so of two spellings exactly one is kept.
func moreSpecific(a, b string) bool {
	aLong := accelerator.NormalizeAcceleratorName(a) != a
	bLong := accelerator.NormalizeAcceleratorName(b) != b
	if aLong != bLong {
		return aLong
	}
	return a < b
}

// mergeNamespaceConstraints merges the per-(namespace, accelerator type) pools
// across providers into available-GPU budgets. Returns nil when no provider
// carries namespace pools, so the per-type-only path is unaffected.
//
// A namespace present in any provider's NamespacePools is materialized in the
// result even when its inner map is empty — its presence marks a CLOSED
// allowlist (deny-all when empty) that the optimizer enforces by allocating
// only the listed types.
//
// Several providers holding one namespace are intersected, like the cluster
// pools: a type is allowed only if EVERY provider that holds the namespace lists
// it (by accelerator identity), at the tightest figure among them, and is an
// explicit 0 otherwise. Taking the minimum only over the providers that listed a
// type let a second entry of x: {A100: 4} re-admit the A100 that x: {H100: 4}
// denies. A provider that does not hold the namespace adds no rule for it.
//
// An unlimited (-1 sentinel) pool is carried through as a negative budget so
// the optimizer can distinguish "unlimited" from a finite cap; tighterBudget
// treats it as +infinity when taking the minimum.
func mergeNamespaceConstraints(constraints []*ResourceConstraints) map[string]map[string]int {
	holders := map[string][]map[string]ResourcePool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespacePools {
			holders[ns] = append(holders[ns], perType)
		}
	}
	if len(holders) == 0 {
		return nil
	}
	merged := make(map[string]map[string]int, len(holders))
	for ns, pools := range holders {
		names := map[string]bool{}
		for _, perType := range pools {
			for name := range perType {
				names[name] = true
			}
		}
		inner := make(map[string]int, len(names))
		for name := range names {
			for i, perType := range pools {
				budget := 0 // not listed by this holder: denied
				if key, listed := accelerator.FindKey(perType, name); listed {
					budget = nsPoolBudget(perType[key])
				}
				if i == 0 {
					inner[name] = budget
				} else {
					inner[name] = tighterBudget(inner[name], budget)
				}
			}
		}
		// Every holder's allowlist is closed for this namespace, so any spelling
		// one of them lists is a closed key.
		merged[ns] = collapseSpellings(inner, func(name string) bool {
			for _, perType := range pools {
				if _, ok := perType[name]; ok {
					return true
				}
			}
			return false
		}) // present (even if empty) marks a closed namespace
	}
	return merged
}

// nsPoolBudget returns a namespace ResourcePool's remaining budget for the
// optimizer — the available (not total) GPU count. A negative Limit is the
// "unlimited" sentinel and is preserved as -1; any other Limit yields the
// finite available count (Limit - Used, clamped to 0).
func nsPoolBudget(pool ResourcePool) int {
	if pool.Limit < 0 {
		return -1
	}
	return pool.Available()
}

// tighterBudget returns the more restrictive of two namespace budgets, treating
// a negative (unlimited) budget as +infinity. The result is unlimited only when
// both inputs are unlimited.
func tighterBudget(a, b int) int {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

// scaleDownRoleIterated removes replicas role-by-role using the generalized
// scaleDownVariantSet primitive. Roles are sorted for determinism.
// Arity-1 (roles=["both"]) handles non-disaggregated models.
func scaleDownRoleIterated(
	ctx context.Context,
	e *NamedAnalyzerResult,
	variants []variantRecord,
	targets map[string]int,
	stateMap ...map[string]domain.VariantReplicaState,
) {
	var states map[string]domain.VariantReplicaState
	if len(stateMap) > 0 {
		states = stateMap[0]
	}
	roles := rolesOf(variants)

	for _, role := range roles {
		if !needsScaleDownForRole(*e, role) {
			continue
		}
		roleVCs := variantsForRole(variants, role)
		if len(roleVCs) == 0 {
			continue
		}
		sorted := sortVariantsForScaleDown(*e, roleVCs)
		scaleDownVariantSet(ctx, sorted, targets, states,
			func(vc variantRecord) int {
				return safeRemovalReplicasForRole(*e, vc.VariantName, role)
			},
			func(vc variantRecord, n int) {
				applyDeallocationForRole(e, vc.VariantName, role, n)
			},
		)
	}
}

// Ensure CostAwareOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*CostAwareOptimizer)(nil)
