package allocation

import (
	"cmp"
	"context"
	"maps"
	"math"
	"slices"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// modelKey is a model's identity for rescale bookkeeping. A model is identified by
// (namespace, ModelID), never ModelID alone: the same ModelID can be served in two
// namespaces, and both can land in the same cluster-scope competition group. Keying
// maps by the bare ModelID would collide those models — overwriting targets, double-
// counting floors, and freezing one namespace's model out of the additive path.
func modelKey(req ModelScalingRequest) string {
	return utils.GetNamespacedKey(req.Namespace, req.ModelID)
}

// RescaleFlags carries the resolved, scope-coupled rescale enablement for one
// optimize cycle. Cluster governs the cluster budget group; ByNamespace[ns]
// governs namespace ns's quota budget group. The zero value disables rescale.
type RescaleFlags struct {
	Cluster     bool
	ByNamespace map[string]bool
}

func (f RescaleFlags) any() bool { return f.Cluster || len(f.ByNamespace) > 0 }

// enabledForScope reports whether rescale is on at a group's budget scope:
// the namespace flag for a namespace-quota group, else the cluster flag.
func (f RescaleFlags) enabledForScope(namespace string, namespaceScoped bool) bool {
	if namespaceScoped {
		return f.ByNamespace[namespace]
	}
	return f.Cluster
}

// rescaleInput is one model's contribution to the priority-weighted water-filling.
// All GPU quantities are whole GPUs; Demand is in saturation's unit for the
// model and is used only inside the weight ratio (priority x demand). Within
// one model that unit cancels; across models it does not: each model's demand
// is work in its own units (its own tokens, its own capacity), so two models'
// weights compare figures that are not the same work.
type rescaleInput struct {
	ID        string
	Priority  float64 // model priority multiplier (>= 0)
	Demand    float64 // model demand for the weight (any unit; ratio only), raised as the cap's is
	FloorGPUs int     // reserved: sum over variants of minReplicas x gpusPerReplica
	CapGPUs   int     // upper bound: sum over roles of min(demand-in-GPUs, the role's maxReplicas GPUs); >= FloorGPUs
}

// computeRescaleTargets distributes `budget` GPUs across models by priority-weighted
// water-filling, returning each model's target GPU allocation.
//
//	target_i = floor_i + (budget - Sum floor) * weight_i / Sum weight     (weight_i = priority_i * demand_i)
//
// A model wanting less than its share is capped at CapGPUs_i and the freed excess
// re-splits over the still-hungry models by the same weights, until none exceeds its
// cap. Floors are always reserved first; when Sum floor > budget the floors cannot be
// met — every model is clamped to its floor and overBudget is true (a Conflict the
// caller surfaces). Fractional shares are rounded to whole GPUs by the largest-remainder
// method, never exceeding a model's cap, so the returned targets sum to at most `budget`.
func computeRescaleTargets(in []rescaleInput, budget int) (targets map[string]int, overBudget bool) {
	targets = make(map[string]int, len(in))

	sumFloor := 0
	for _, m := range in {
		targets[m.ID] = m.FloorGPUs
		sumFloor += m.FloorGPUs
	}
	if sumFloor >= budget {
		// Floors already consume (or exceed) the budget: nothing to redistribute.
		return targets, sumFloor > budget
	}

	pool := budget - sumFloor // redistributable GPUs above the floors

	// Iterative water-fill in fractional space over `pool`, capping at each model's
	// headroom (CapGPUs - FloorGPUs) and re-splitting the freed remainder.
	type active struct {
		id       string
		weight   float64
		headroom int
	}
	act := make([]active, 0, len(in))
	for _, m := range in {
		hr := m.CapGPUs - m.FloorGPUs
		if hr < 0 {
			hr = 0
		}
		// Bounded, so the sum of the weights stays finite: an infinite weight
		// made every share Inf/Inf = NaN.
		w := min(m.Priority*m.Demand, maxShareWeight)
		if w > 0 && hr > 0 {
			act = append(act, active{m.ID, w, hr})
		}
	}

	extra := make(map[string]float64, len(act))
	remaining := float64(pool)
	for len(act) > 0 && remaining > 1e-9 {
		totalW := 0.0
		for _, a := range act {
			totalW += a.weight
		}
		if totalW <= 0 {
			break
		}
		// Cap any model whose proportional share reaches its headroom; if none does,
		// assign the remainder proportionally and finish.
		capped := false
		used := 0.0
		next := act[:0:0]
		for _, a := range act {
			share := remaining * a.weight / totalW
			if share >= float64(a.headroom)-1e-9 {
				extra[a.id] = float64(a.headroom)
				used += float64(a.headroom)
				capped = true
			} else {
				next = append(next, a)
			}
		}
		if capped {
			remaining -= used
			act = next
			continue
		}
		for _, a := range act {
			extra[a.id] = remaining * a.weight / totalW
		}
		remaining = 0
		act = nil
	}

	// Round fractional extras to whole GPUs (largest remainder), respecting each
	// model's headroom, so the integer total does not exceed the fractional total.
	roundExtras(in, extra, targets)
	return targets, false
}

// roundExtras adds each model's fractional above-floor allocation to its target using
// the largest-remainder method: floor everything, then hand out the leftover GPUs one at
// a time to the largest fractional parts, skipping models already at their cap.
func roundExtras(in []rescaleInput, extra map[string]float64, targets map[string]int) {
	capOf := make(map[string]int, len(in))
	for _, m := range in {
		capOf[m.ID] = m.CapGPUs
	}

	remainder := make(map[string]float64, len(extra))
	totalFrac := 0.0
	sumWhole := 0
	// In key order: totalFrac is a float sum, whose last bits depend on the
	// order it is taken in. roundToNano below absorbs those bits before the
	// total is rounded to GPUs; the sort is belt-and-braces, so the sum is
	// also the same from one cycle to the next.
	for _, id := range slices.Sorted(maps.Keys(extra)) {
		e := extra[id]
		whole := int(math.Floor(e))
		targets[id] += whole
		sumWhole += whole
		totalFrac += e
		remainder[id] = e - float64(whole)
	}
	// leftover = round(total above-floor allocation) - the whole GPUs already applied.
	leftover := int(math.Round(roundToNano(totalFrac))) - sumWhole
	apportionLeftover(targets, remainder, leftover, capOf)
}

// roundToNano rounds x to nine decimal places, the precision a GPU total is
// meant at. A float sum carries error in its last bits: 0.1 + 0.35 + 0.05
// sums to 0.49999999999999994, and rounding that to whole GPUs gave 0 where
// the total is a half and rounds to 1. Exact to 1e-9 for any total below
// about 4.5e6 GPUs.
func roundToNano(x float64) float64 {
	return math.Round(x*1e9) / 1e9
}

// apportionLeftover hands out `leftover` whole units one at a time to the keys with
// the largest fractional remainders (deterministic tie-break by key ascending),
// skipping any key already at capOf[key]. A nil capOf means uncapped. It mutates out.
func apportionLeftover(out map[string]int, remainder map[string]float64, leftover int, capOf map[string]int) {
	if leftover <= 0 {
		return
	}
	type kr struct {
		key  string
		frac float64
	}
	order := make([]kr, 0, len(remainder))
	for k, v := range remainder {
		order = append(order, kr{k, v})
	}
	// Largest fractional remainder first; deterministic tie-break by key.
	slices.SortFunc(order, func(a, b kr) int {
		return cmp.Or(cmp.Compare(b.frac, a.frac), cmp.Compare(a.key, b.key))
	})
	for _, o := range order {
		if leftover <= 0 {
			break
		}
		if capOf != nil {
			if c, ok := capOf[o.key]; ok && out[o.key] >= c {
				continue
			}
		}
		out[o.key]++
		leftover--
	}
}

// applyRescale runs the priority-weighted rescale pass for enabled, contended
// (accelerator type, budget scope) groups. It returns the decisions it produced and
// the set of model IDs it handled (which the caller excludes from the additive path).
// Fills consume free GPUs from `available` / `availableByNS` in place; reclaims do not
// free budget this cycle (usage is CurrentReplicas-based), so fills never over-subscribe.
//
// P/D models are handled: a model's GPU target is split across its roles by role
// demand, and reclaim/fill run per role. Multi-accelerator models (variants spanning
// GPU types, incl. P/D across types) are skipped (deferred).
func (o *GreedyByScoreOptimizer) applyRescale(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
	available map[string]int,
	availableByNS map[string]map[string]int,
) ([]domain.VariantDecision, map[string]bool) {
	logger := ctrl.LoggerFrom(ctx).WithName("rescale")
	handled := make(map[string]bool)
	var decisions []domain.VariantDecision

	type groupKey struct {
		accType string
		scope   string // namespace for namespace-scoped groups, "" for cluster
	}
	groups := make(map[groupKey][]ModelScalingRequest)
	for _, req := range requests {
		records := recordsForRequest(req)
		if records == nil {
			continue
		}
		accType, ok := singleAccType(records)
		if !ok {
			continue // multi-accelerator (incl. P/D spanning types): deferred
		}
		// A variant with no accelerator name has GPUs no budget counts, so a
		// model with one is left to the additive path whole, which may shrink
		// it but finds no budget to grow it. Moved by rescale, it was filled
		// with uncounted GPUs; frozen there, it never shrank, and its role's
		// other variants were sized for the demand it already served.
		if slices.ContainsFunc(records, func(vc variantRecord) bool { return vc.AcceleratorName == "" }) {
			continue
		}
		// A namespace present in availableByNS is a closed quota allowlist → namespace scope.
		_, nsScoped := availableByNS[req.Namespace]
		if !o.Rescale.enabledForScope(req.Namespace, nsScoped) {
			continue
		}
		scope := ""
		budgets := available
		if nsScoped {
			scope = req.Namespace
			budgets = availableByNS[req.Namespace]
		}
		// Group under the budget's own spelling of the accelerator, so two models
		// that spell one GPU differently compete for one budget instead of each
		// being handed all of it, and the reads below find it.
		k := groupKey{budgetKey(budgets, accType), scope}
		groups[k] = append(groups[k], req)
	}

	// Process groups in a deterministic order. A namespace fill debits the shared
	// cluster budget, so a cluster-scoped group on the same accelerator type observes
	// the result — a stable order keeps a cycle reproducible for identical input.
	keys := slices.Collect(maps.Keys(groups))
	slices.SortFunc(keys, func(a, b groupKey) int {
		return cmp.Or(cmp.Compare(a.accType, b.accType), cmp.Compare(a.scope, b.scope))
	})

	for _, k := range keys {
		reqs := groups[k]
		free := budgetOf(available, k.accType)
		if k.scope != "" {
			free = availableByNS[k.scope][k.accType]
		}
		// Skip unlimited/unknown budgets — there is nothing to redistribute.
		if free < 0 || free == math.MaxInt {
			continue
		}

		currentUsage := 0
		for _, req := range reqs {
			currentUsage += modelCurrentGPUs(req, k.accType)
		}
		budget := free + currentUsage

		// Contended when the models can take more than the budget: their caps,
		// not their demand. A role at its maxReplicas cannot take the demand
		// beyond it, and read uncapped it set off rescale in a group that fit,
		// which cut a model inside its band while GPUs stayed free.
		inputs, sumWantGPUs := rescaleInputsForGroup(reqs, k.accType, budget)
		if sumWantGPUs <= budget {
			continue // uncontended: leave to the additive path
		}

		targets, overBudget := computeRescaleTargets(inputs, budget)
		if overBudget {
			logger.Info("rescale: minReplicas floors exceed the group budget (Conflict); clamping to floors",
				"accType", k.accType, "scope", k.scope, "budget", budget)
		}

		// Serve highest-priority models first: same-cycle free GPUs are scarce
		// (reclaims free nothing until next cycle), so a lower-priority model earlier
		// in request order must not claim a fill ahead of a higher-priority one.
		// Deterministic tie-break by the full (namespace, ModelID) identity — a bare
		// ModelID ties two same-named models in different namespaces, leaving the winner
		// to randomized request order (flap cycle-to-cycle).
		slices.SortStableFunc(reqs, func(a, b ModelScalingRequest) int {
			return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(modelKey(a), modelKey(b)))
		})

		// GPUs we can physically hand out this cycle. A namespace fill draws from the
		// shared physical pool, so bound it by the cluster's finite free too — filling
		// up to the namespace quota alone would emit scale-ups for GPUs that are not
		// physically free (over-subscription) and drive the cluster budget negative.
		// The water-fill target above intentionally uses the full quota; fills pace
		// toward it over cycles as physical GPUs free up.
		fillable := free
		// The group is keyed by the namespace allowlist's spelling; the cluster
		// pool may spell the accelerator differently.
		clusterKey := budgetKey(available, k.accType)
		if k.scope != "" {
			if cur, ok := clusterBudgetFor(available, k.accType); ok && cur != math.MaxInt {
				fillable = min(fillable, cur)
			}
		}

		freeThisCycle := fillable
		for _, req := range reqs {
			before := freeThisCycle
			d := o.rescaleModelDecisions(ctx, req, constraints, k.accType, targets[modelKey(req)], &freeThisCycle)
			decisions = append(decisions, d...)
			handled[modelKey(req)] = true
			// Recorded per model, as the group's debit below charges it.
			if took := before - freeThisCycle; took > 0 {
				if k.scope != "" {
					o.spend(modelKey(req), k.scope, k.accType, took)
					o.spend(modelKey(req), "", clusterKey, took)
				} else {
					o.spend(modelKey(req), "", k.accType, took)
				}
			}
		}

		if consumed := fillable - freeThisCycle; consumed > 0 {
			if k.scope != "" {
				debitBudget(availableByNS[k.scope], k.accType, consumed)
				// A namespace fill also draws from the shared physical pool; debit the
				// finite cluster budget too (mirroring allocateForModel). Bounded by
				// fillable above, this can never drive the cluster budget negative.
				debitBudget(available, clusterKey, consumed)
			} else {
				debitBudget(available, k.accType, consumed)
			}
		}
	}

	return decisions, handled
}

// rescaleModelDecisions drives one model to targetGPUs on accType: reclaim (shed
// whole replicas most-expensive-first, respecting each role's floor) or fill (add
// most-cost-efficient first, gated on *freeThisCycle). Reclaim decisions are
// tagged DecisionReasonRescale.
func (o *GreedyByScoreOptimizer) rescaleModelDecisions(
	ctx context.Context,
	req ModelScalingRequest,
	constraints []*ResourceConstraints,
	accType string,
	targetGPUs int,
	freeThisCycle *int,
) []domain.VariantDecision {
	satNamed := req.CompositeSignal
	if satNamed.Result == nil {
		return nil
	}
	all := buildVariantRecords(req, satNamed.Result)
	stateMap := buildStateMap(req.VariantStates)
	vcMap := buildCapacityMap(all)
	targets := initTargets(req.VariantStates)
	// Rescale moves only the variants on accType: the budget, the floors and
	// the demand count no other GPUs. applyRescale admits only models whose
	// variants all name accType; the filter keeps that true for any caller.
	records := variantsOnType(all, accType)

	// Split the model's GPU target across its roles by role demand (a P/D model
	// must keep both roles served), then reclaim/fill each role toward its share.
	// A non-disaggregated model has the single synthetic "both" role.
	roles := modelRolesOnType(records, accType)
	curByRole := make(map[string]int, len(roles))
	demByRole := make(map[string]int, len(roles))
	floorByRole := make(map[string]int, len(roles))
	// A role the composite holds both ways freezes the model here, as the
	// share optimizer freezes it: its demand is a figure the policy does not
	// size on, and shrinking or growing the other roles around it strands one
	// role's output. Taking its GPUs out of the budget instead put the whole
	// cut of a contended model on decode.
	for _, role := range roles {
		curByRole[role] = roleCurrentGPUs(records, stateMap, accType, role)
		demByRole[role] = roleDemandGPUs(satNamed, records, stateMap, accType, role)
		floorByRole[role] = roleFloorGPUs(records, stateMap, accType, role)
		// A role that may not shrink keeps what it has in the split, so the
		// other roles are cut to what is left rather than to a share computed
		// as if this one would shrink.
		if satNamed.Holds[role].NoScaleDown {
			floorByRole[role] = max(floorByRole[role], curByRole[role])
		}
	}
	pinned := pinnedModel(satNamed)
	tgtByRole := curByRole
	if !pinned {
		uncapped := distributeGPUsByWeight(targetGPUs, roles, demByRole, curByRole, floorByRole, nil)
		ceilByRole := roleShareCeilings(records, stateMap, accType, roles, demByRole, uncapped)
		tgtByRole = distributeGPUsByWeight(targetGPUs, roles, demByRole, curByRole, floorByRole, ceilByRole)
		if uniformPods(records, stateMap) {
			roundFillsToReplicas(records, stateMap, roles, tgtByRole, curByRole, demByRole, targets)
			// Each role's share is at or above the floor reserved in the split.
			reclaimWholeReplicas(ctx, satNamed, records, stateMap, roles, tgtByRole, curByRole, targets)
		} else {
			// With more than one replica size the two greedy passes are not
			// monotone in the target (#219): fill and shed are searched together.
			roundExact(satNamed, records, stateMap, roles, tgtByRole, curByRole, demByRole, targets)
		}
	}

	limited := make(gpuLimitTracker)
	// grown is what each role's fill added, per variant, in the order added.
	grown := map[string][]roleGrowth{}
	for _, role := range roles {
		rt, rc := tgtByRole[role], curByRole[role]
		if rt <= rc || satNamed.Holds[role].NoScaleUp {
			continue
		}
		want := rt - rc
		// The role's rescale target needs more GPUs than this cycle can
		// free (reclaims only free capacity for the NEXT cycle), so the
		// fill lands short. That is scarcity, not a user ceiling.
		gpuCapped := want > *freeThisCycle
		if gpuCapped {
			want = *freeThisCycle
		}
		before := maps.Clone(targets)
		*freeThisCycle -= fillRoleExact(records, role, stateMap, targets, want)
		if gpuCapped {
			markRoleGPULimited(records, role, stateMap, targets, limited)
		}
		for _, vc := range sortByCostEfficiencyAsc(variantsForRole(records, role)) {
			if n := targets[vc.VariantName] - before[vc.VariantName]; n > 0 {
				grown[role] = append(grown[role], roleGrowth{variant: vc.VariantName, n: n})
			}
		}
	}
	// With a node view the fills must also land on nodes, as the additive
	// path's do: what does not place is given back to the cycle's free GPUs.
	*freeThisCycle += o.fitRescaleToNodes(req, records, stateMap, targets, roles, grown, limited)

	decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "rescale")
	o.attributeGPULimits(decisions, limited, constraints)
	for i := range decisions {
		if decisions[i].Action == domain.ActionScaleDown {
			decisions[i].SetDecisionReason(domain.ActionScaleDown, domain.DecisionReasonRescale,
				string(domain.DecisionReasonRescale)+" (optimizer: rescale, reclaim)")
		}
	}
	return decisions
}

// roleGrowth is n replicas a rescale fill added to variant.
type roleGrowth struct {
	variant string
	n       int
}

// fitRescaleToNodes places a rescale fill's replicas (grown, per role) on the
// cycle's node view, as fitPairToNodes places the additive path's: one replica
// at a time, of the role furthest behind its share first, each pod best-fit,
// until every role has its replicas or one does not place. A model filling
// more than one role -- a P/D pair -- grows only if every such role placed at
// least one replica: it never grows half a pair. What places is debited from
// the view, charged to the model; what does not is taken back off targets, its
// variant marked as held back by node fit, and its GPUs returned. Without a
// node view it does nothing and returns 0.
func (o *GreedyByScoreOptimizer) fitRescaleToNodes(req ModelScalingRequest, records []variantRecord,
	stateMap map[string]domain.VariantReplicaState, targets map[string]int, roles []string,
	grown map[string][]roleGrowth, limited gpuLimitTracker) int {
	if o.Nodes == nil || len(grown) == 0 {
		return 0
	}
	fit := nodeFit{nodes: o.Nodes, namespace: req.Namespace, limited: limited}
	filling := make([]string, 0, len(grown))
	want := map[string]int{}
	for _, role := range roles {
		for _, g := range grown[role] {
			want[role] += g.n
		}
		if want[role] > 0 {
			filling = append(filling, role)
		}
	}
	trial := o.Nodes.Clone()
	placed := map[string]int{}        // per role
	placedVariant := map[string]int{} // per variant
	short := ""
	for {
		next := ""
		for _, role := range filling {
			if placed[role] >= want[role] {
				continue
			}
			// The role with the smallest placed/want fraction, compared exactly.
			if next == "" || placed[role]*want[next] < placed[next]*want[role] {
				next = role
			}
		}
		if next == "" {
			break
		}
		// The role's replicas in the order its fill added them.
		v, i := "", placed[next]
		for _, g := range grown[next] {
			if i < g.n {
				v = g.variant
				break
			}
			i -= g.n
		}
		if !trial.place(fit.replica(records, stateMap, v)) {
			short = v
			break
		}
		placed[next]++
		placedVariant[v]++
	}
	if short != "" {
		limited.markNodeFit(short)
	}
	if len(filling) > 1 && slices.ContainsFunc(filling, func(role string) bool { return placed[role] == 0 }) {
		clear(placedVariant) // half a pair is no pair: nothing grows
	} else {
		defer o.Nodes.setOwner(o.Nodes.setOwner(modelKey(req)))
		o.Nodes.adopt(trial)
	}
	// A variant belongs to one role and is grown once per fill.
	refund := 0
	for _, role := range filling {
		for _, g := range grown[role] {
			if back := g.n - placedVariant[g.variant]; back > 0 {
				targets[g.variant] -= back
				refund += back * gpusPerReplicaFromState(stateMap, g.variant)
			}
		}
	}
	return refund
}

// reclaimWholeReplicas sheds the roles whose share tgt is below what they have
// cur, in whole replicas. Each role's cut alone is rounded down to its replica
// size, so on 8-GPU pods a model target of 24 from 32, split as a cut of 4 from
// each role, shed nothing and the model kept all 32: the 8 GPUs the water-fill
// gave another model never reached it. The cuts are summed instead and shed as
// whole replicas: first what fits within each role's own cut, then what is
// left of the sum, from the role with the most of its cut still unshed first,
// each role no further than its cut rounded up to one replica. A role counts
// toward the sum only what it can actually give: a cut no replica can meet (a
// variant with no capacity reading, the cheapest held at one) made the other
// role pay for it, and a role with a share does not count its last replica, so
// the second pass never takes it: with two equal variants the cheapest-at-one
// rule does not protect a role's one pod, and a role with demand went to zero.
// The second pass runs only when every role cut has one replica size. The model
// sheds no more than the summed
// cut, a role under a scale-down hold is not cut at all, and reclaimRole keeps
// every variant at its minReplicas. It mutates targets. rescaleModelDecisions
// calls it only for a model of one replica size; roundExact rounds the rest.
func reclaimWholeReplicas(ctx context.Context, e NamedAnalyzerResult, records []variantRecord,
	stateMap map[string]domain.VariantReplicaState, roles []string, tgt, cur, targets map[string]int) {
	cut := make(map[string]int, len(roles))
	total := 0
	trialCtx := ctrl.LoggerInto(ctx, logr.Discard())
	for _, role := range roles {
		if tgt[role] < cur[role] && !e.Holds[role].NoScaleDown {
			cut[role] = cur[role] - tgt[role]
			// A role with a share keeps its last replica, so that is not
			// something it can give either.
			keep := 0
			if tgt[role] > 0 {
				keep = max(roleReplicaStepGPUs(records, role, stateMap), 1)
			}
			can := reclaimRole(trialCtx, e, records, role, stateMap, maps.Clone(targets), max(cur[role]-keep, 0))
			total += min(cut[role], can)
		}
	}
	if total == 0 {
		return
	}
	shed := make(map[string]int, len(roles))
	for _, role := range roles {
		if cut[role] > 0 {
			shed[role] = reclaimRole(ctx, e, records, role, stateMap, targets, cut[role])
			total -= shed[role]
		}
	}
	// With more than one replica size in a role, the per-role rounding of the
	// first pass and the summed second pass can disagree between adjacent
	// targets, so a larger target left fewer GPUs; such a model is rounded
	// per role only, as before.
	for _, role := range roles {
		if cut[role] > 0 && !uniformReplicaGPUs(records, role, stateMap) {
			return
		}
	}
	order := slices.Clone(roles)
	slices.SortStableFunc(order, func(a, b string) int {
		return cmp.Compare(cut[b]-shed[b], cut[a]-shed[a])
	})
	for _, role := range order {
		left := cut[role] - shed[role]
		if total <= 0 || left <= 0 {
			continue
		}
		limit := ceilToReplica(left, roleReplicaStepGPUs(records, role, stateMap))
		n := reclaimRole(ctx, e, records, role, stateMap, targets, min(total, limit))
		shed[role] += n
		total -= n
	}
}

// reclaimRole sheds up to deltaGPUs worth of a role's replicas, most-expensive-first,
// respecting minReplicas and the cheapest-at-1 protection, via scaleDownVariantSet,
// and returns the GPUs it shed.
func reclaimRole(
	ctx context.Context,
	e NamedAnalyzerResult,
	variants []variantRecord,
	role string,
	stateMap map[string]domain.VariantReplicaState,
	targets map[string]int,
	deltaGPUs int,
) int {
	remaining := deltaGPUs
	sorted := sortVariantsForScaleDown(e, variantsForRole(variants, role))
	scaleDownVariantSet(ctx, sorted, targets, stateMap,
		func(vc variantRecord) int {
			g := gpusPerReplicaFromState(stateMap, vc.VariantName)
			if remaining <= 0 || g <= 0 {
				return 0
			}
			return remaining / g // whole replicas that fit in the remaining GPU delta
		},
		func(vc variantRecord, n int) {
			remaining -= n * gpusPerReplicaFromState(stateMap, vc.VariantName)
		},
	)
	return deltaGPUs - remaining
}

// fillRole adds up to wantGPUs worth of a role's replicas, most-cost-efficient-first,
// respecting maxReplicas. Returns the GPUs actually consumed.
func fillRole(
	variants []variantRecord,
	role string,
	stateMap map[string]domain.VariantReplicaState,
	targets map[string]int,
	wantGPUs int,
) int {
	spent := 0
	for _, vc := range sortByCostEfficiencyAsc(variantsForRole(variants, role)) {
		if wantGPUs-spent <= 0 {
			break
		}
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		g := gpusPerReplicaFromState(stateMap, vc.VariantName)
		if g <= 0 {
			continue
		}
		st := stateMap[vc.VariantName]
		for wantGPUs-spent >= g {
			if st.MaxReplicas != nil && *st.MaxReplicas > 0 && targets[vc.VariantName] >= *st.MaxReplicas {
				break
			}
			targets[vc.VariantName]++
			spent += g
		}
	}
	return spent
}

// markRoleGPULimited records the variant that would have received the GPUs a
// short fill could not get: the cheapest role candidate still under its
// MaxReplicas ceiling, i.e. the one fillRole would grow next given more budget.
// Called after the fill so `targets` reflects what was actually placed.
//
// Candidates already at their ceiling are skipped — they are bounded by user
// intent, not by scarcity, and crediting them would be a false alarm.
func markRoleGPULimited(
	variants []variantRecord,
	role string,
	stateMap map[string]domain.VariantReplicaState,
	targets map[string]int,
	limited gpuLimitTracker,
) {
	for _, vc := range sortByCostEfficiencyAsc(variantsForRole(variants, role)) {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		st := stateMap[vc.VariantName]
		if st.MaxReplicas != nil && *st.MaxReplicas > 0 && targets[vc.VariantName] >= *st.MaxReplicas {
			continue
		}
		limited.mark(vc.VariantName)
		return
	}
}

// singleAccType returns the accelerator type shared by all variants, or false if
// they span more than one (multi-accelerator, deferred) or none is set. Variants
// that spell one accelerator differently share it (accelerator.SameName); the
// first spelling is returned, and callers resolve it against their budget. A
// variant with no accelerator name is skipped here; applyRescale leaves a
// model with one to the additive path.
func singleAccType(vcs []variantRecord) (string, bool) {
	accType := ""
	for _, vc := range vcs {
		if vc.AcceleratorName == "" {
			continue
		}
		if accType == "" {
			accType = vc.AcceleratorName
		} else if !accelerator.SameName(accType, vc.AcceleratorName) {
			return "", false
		}
	}
	return accType, accType != ""
}

// modelCurrentGPUs sums CurrentReplicas x GPUsPerReplica over the model's variants
// on accType.
func modelCurrentGPUs(req ModelScalingRequest, accType string) int {
	records := recordsForRequest(req)
	if records == nil {
		return 0
	}
	stateMap := buildStateMap(req.VariantStates)
	total := 0
	for _, vc := range records {
		if !accelerator.SameName(vc.AcceleratorName, accType) {
			continue
		}
		total += stateMap[vc.VariantName].CurrentReplicas * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return total
}

// rescaleInputsForGroup builds the water-filling inputs for a group's models and
// returns them with the GPUs the models can take, their caps before the budget
// bounds them, for the contention check.
func rescaleInputsForGroup(reqs []ModelScalingRequest, accType string, budget int) ([]rescaleInput, int) {
	inputs := make([]rescaleInput, 0, len(reqs))
	sumWantGPUs := 0
	for _, req := range reqs {
		satNamed := req.CompositeSignal
		if satNamed.Result == nil {
			continue
		}
		records := buildVariantRecords(req, satNamed.Result)
		stateMap := buildStateMap(req.VariantStates)

		floorGPUs, maxGPUs, maxBounded := 0, 0, true
		for _, vc := range records {
			if !accelerator.SameName(vc.AcceleratorName, accType) {
				continue
			}
			g := gpusPerReplicaFromState(stateMap, vc.VariantName)
			st := stateMap[vc.VariantName]
			if st.MinReplicas != nil && *st.MinReplicas > 0 {
				floorGPUs += *st.MinReplicas * g
			}
			if st.MaxReplicas != nil && *st.MaxReplicas > 0 {
				maxGPUs += *st.MaxReplicas * g
			} else {
				maxBounded = false
			}
		}

		// A role that may not shrink keeps what it has in the water-fill too:
		// weighed as if it would shrink, the model was given a target it never
		// reached, and the GPUs it kept were missing from the others'.
		heldFloor := 0
		for _, role := range modelRolesOnType(records, accType) {
			rf := roleFloorGPUs(records, stateMap, accType, role)
			if satNamed.Holds[role].NoScaleDown {
				rf = max(rf, roleCurrentGPUs(records, stateMap, accType, role))
			}
			heldFloor += rf
		}
		floorGPUs = max(floorGPUs, heldFloor)

		// Each role is capped at its own ceiling before the sum: a P/D role at
		// its maxReplicas cannot take GPUs for the demand beyond it, and since
		// the role split no longer reclaims the other role to pay for it, the
		// GPUs sized on that demand sat free while another model was short.
		capGPUs := 0
		for _, role := range modelRolesOnType(records, accType) {
			d := roleDemandGPUs(satNamed, records, stateMap, accType, role)
			if c := roleCeilingGPUs(variantsForRole(variantsOnType(records, accType), role), stateMap); c > 0 {
				d = min(d, c)
			}
			capGPUs += d
		}
		if maxBounded {
			capGPUs = min(capGPUs, maxGPUs)
		}
		// What the model can take, for the contention check: its cap before
		// the group budget bounds it, so one model wanting more than the
		// budget still contends for it.
		wantGPUs := max(capGPUs, floorGPUs)
		if !maxBounded {
			capGPUs = min(capGPUs, budget) // unbounded max: cap at the group budget
		}
		capGPUs = max(capGPUs, floorGPUs)
		// A model with a role held both ways is frozen by rescaleModelDecisions,
		// so it holds exactly what it has: the water-fill gives it that and
		// shares the rest. Weighed on its demand, it was set a share it never
		// took, and the GPUs sat idle while another model stayed short.
		if pinnedModel(satNamed) {
			cur := 0
			for _, role := range modelRolesOnType(records, accType) {
				cur += roleCurrentGPUs(records, stateMap, accType, role)
			}
			floorGPUs, capGPUs, wantGPUs = cur, cur, cur
		}

		inputs = append(inputs, rescaleInput{
			ID:        modelKey(req),
			Priority:  req.Priority,
			Demand:    modelWeightDemand(satNamed, records, accType),
			FloorGPUs: floorGPUs,
			CapGPUs:   capGPUs,
		})
		sumWantGPUs += wantGPUs
	}
	return inputs, sumWantGPUs
}

// pinnedModel reports whether the composite holds any of the model's roles
// both ways, which freezes the model's rescale.
func pinnedModel(nr NamedAnalyzerResult) bool {
	for _, h := range nr.Holds {
		if h.NoScaleUp && h.NoScaleDown {
			return true
		}
	}
	return false
}

// modelWeightDemand is the demand a model weighs in the water-fill: each
// role's demand on accType raised to what another analyzer keeps there at
// full utilization (rescaleRoleDemand), summed. The cap is sized on the same raised
// demand (roleDemandGPUs); weighed on its own TotalDemand alone, a model whose
// other frame kept 800 against its own 400 was given 3 of 12 GPUs, and 4 when
// saturation alone read the 800.
func modelWeightDemand(nr NamedAnalyzerResult, records []variantRecord, accType string) float64 {
	total := 0.0
	for _, role := range modelRolesOnType(records, accType) {
		total += rescaleRoleDemand(nr, role)
	}
	return total
}

// rescaleRoleDemand is role's demand raised to what another analyzer keeps at full
// utilization, rescale's sizing point.
func rescaleRoleDemand(nr NamedAnalyzerResult, role string) float64 {
	return max(demandForRoleOrModel(nr, role), nr.OtherFrameDemand(role, 1))
}

// roleDemandGPUs converts a role's token demand to a GPU count via the role's most
// cost-efficient variant's per-replica capacity. The synthetic "both" role uses the
// model-level TotalDemand; a P/D role uses its RoleCapacities demand (via
// demandForRoleOrModel).
func roleDemandGPUs(satNamed NamedAnalyzerResult, records []variantRecord, stateMap map[string]domain.VariantReplicaState, accType, role string) int {
	// Rescale sizes at full utilization (whole replicas of the demand), so
	// another analyzer keeps what it needs there.
	demand := rescaleRoleDemand(satNamed, role)
	best, bestGPUs, ok := bestVariantForRole(records, stateMap, accType, role)
	if !ok {
		return 0
	}
	return replicasForDemand(demand, best.PerReplicaCapacity) * bestGPUs
}

// bestVariantForRole returns a role's most cost-efficient variant on accType
// that carries a capacity reading, with its GPUs per replica. ok is false when
// no variant of the role has one: nothing can size a replica of it.
func bestVariantForRole(records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	accType, role string) (best variantRecord, gpusPerReplica int, ok bool) {
	for _, vc := range sortByCostEfficiencyAsc(variantsForRole(variantsOnType(records, accType), role)) {
		if hasCapacityReading(vc.PerReplicaCapacity) {
			return vc, gpusPerReplicaFromState(stateMap, vc.VariantName), true
		}
	}
	return variantRecord{}, 0, false
}

// variantsOnType filters variants to those on accType.
func variantsOnType(vcs []variantRecord, accType string) []variantRecord {
	out := make([]variantRecord, 0, len(vcs))
	for _, vc := range vcs {
		if accelerator.SameName(vc.AcceleratorName, accType) {
			out = append(out, vc)
		}
	}
	return out
}

// modelRolesOnType returns the distinct roles among a model's variants on accType,
// sorted for determinism. A variant with no role is the synthetic "both".
func modelRolesOnType(vcs []variantRecord, accType string) []string {
	return rolesOf(variantsOnType(vcs, accType))
}

// roleCurrentGPUs sums CurrentReplicas x GPUsPerReplica over a role's variants on accType.
func roleCurrentGPUs(records []variantRecord, stateMap map[string]domain.VariantReplicaState, accType, role string) int {
	total := 0
	for _, vc := range variantsForRole(variantsOnType(records, accType), role) {
		total += stateMap[vc.VariantName].CurrentReplicas * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return total
}

// roleFloorGPUs sums minReplicas x GPUsPerReplica over a role's variants on accType —
// the GPUs that must stay allocated to the role regardless of the weighted split.
func roleFloorGPUs(records []variantRecord, stateMap map[string]domain.VariantReplicaState, accType, role string) int {
	total := 0
	for _, vc := range variantsForRole(variantsOnType(records, accType), role) {
		st := stateMap[vc.VariantName]
		if st.MinReplicas != nil && *st.MinReplicas > 0 {
			total += *st.MinReplicas * gpusPerReplicaFromState(stateMap, vc.VariantName)
		}
	}
	return total
}

// roundFillsToReplicas lowers each growing role's share in tgt, up from cur,
// to what fillRole can actually place as whole replicas from targets (cost
// order, maxReplicas, every variant's own replica size), and spends the GPUs
// that left on whole replicas of the roles cut most. The GPU split ignores
// replica size, so with 8-GPU pods a model given 8 more GPUs split them +1
// and +7 between its roles, neither a replica, and the GPUs sat free every
// cycle. A role takes from the leftover no more than its demand dem still
// asks for, or its own cut rounded up to one replica: the leftover also
// holds what a role at its maxReplicas could not place, and given to the
// other role that was pods beyond its demand. It fills a copy of targets;
// the real fill follows tgt. rescaleModelDecisions calls it only for a model
// of one replica size; roundExact rounds the rest.
func roundFillsToReplicas(records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	roles []string, tgt, cur, dem, targets map[string]int) {
	trial := maps.Clone(targets)
	cut := make(map[string]int, len(roles))
	leftover := 0
	for _, role := range roles {
		want := tgt[role] - cur[role]
		if want <= 0 {
			continue
		}
		placed := fillRole(records, role, stateMap, trial, want)
		cut[role] = want - placed
		tgt[role] -= cut[role]
		leftover += cut[role]
	}
	order := slices.Clone(roles)
	slices.SortStableFunc(order, func(a, b string) int { return cmp.Compare(cut[b], cut[a]) })
	for _, role := range order {
		// A role whose share landed on whole pods (no cut) still takes what
		// its demand asks for: the leftover is also what a role at its
		// maxReplicas could not place.
		if leftover <= 0 {
			break
		}
		limit := max(dem[role]-tgt[role], ceilToReplica(cut[role], roleReplicaStepGPUs(records, role, stateMap)))
		extra := fillRole(records, role, stateMap, trial, min(leftover, limit))
		tgt[role] += extra
		leftover -= extra
	}
}

// roleReplicaStepGPUs is the fewest GPUs one replica of any of role's
// fillable variants takes, or 0 when none is.
func roleReplicaStepGPUs(variants []variantRecord, role string, stateMap map[string]domain.VariantReplicaState) int {
	least := 0
	for _, vc := range variantsForRole(variants, role) {
		if g := gpusPerReplicaFromState(stateMap, vc.VariantName); vc.PerReplicaCapacity > 0 && g > 0 && (least == 0 || g < least) {
			least = g
		}
	}
	return least
}

// uniformReplicaGPUs reports whether every variant of role that can be sized
// (a capacity reading and a GPU count) takes the same GPUs per replica.
func uniformReplicaGPUs(variants []variantRecord, role string, stateMap map[string]domain.VariantReplicaState) bool {
	size := 0
	for _, vc := range variantsForRole(variants, role) {
		g := gpusPerReplicaFromState(stateMap, vc.VariantName)
		if vc.PerReplicaCapacity <= 0 || g <= 0 {
			continue
		}
		if size != 0 && g != size {
			return false
		}
		size = g
	}
	return true
}

// ceilToReplica rounds gpus up to a whole number of g-GPU replicas; g <= 0
// leaves it as is.
func ceilToReplica(gpus, g int) int {
	if g <= 0 {
		return gpus
	}
	return (gpus + g - 1) / g * g
}

// roleShareCeilings is the most of a model's GPU target each role may be given
// in the split: the GPUs its maxReplicas allow (none when any of its variants
// has no maximum), and, when the split is by demand, no more than the larger of
// its demand dem and its share uncapped, the split made without ceilings.
// The demand bound limits only what a role takes of the share another role at
// its maximum could not hold: without it, that share went to the other role as
// pods beyond its demand. A role's own share is never lowered by it, so with no
// role at its maximum the split is the uncapped one. When no role has demand
// the split falls back to what the roles hold, and only the maxReplicas
// ceilings apply.
func roleShareCeilings(records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	accType string, roles []string, dem, uncapped map[string]int) map[string]int {
	byDemand := false
	for _, role := range roles {
		byDemand = byDemand || dem[role] > 0
	}
	out := make(map[string]int, len(roles))
	for _, role := range roles {
		if c := roleCeilingGPUs(variantsForRole(variantsOnType(records, accType), role), stateMap); c > 0 {
			out[role] = c
		}
		if byDemand {
			bound := max(dem[role], uncapped[role])
			if c, ok := out[role]; !ok || bound < c {
				out[role] = bound
			}
		}
	}
	return out
}

// distributeGPUsByWeight splits total GPUs across roles: each role is first reserved
// its floor (minReplicas GPUs), then the remainder is split proportional to weight
// (falling back to `fallback` when all weights are zero), rounded by largest remainder
// for determinism. Reserving floors first keeps a role's minReplicas from being
// undercut by the split — e.g. a cold-start P/D model with zero demand and zero current
// on both roles must still keep each role's floor, not dump everything on one role. The
// caller reserves the model-level floor before splitting, so total >= sum of role floors.
//
// A role is never given more than its ceiling (absent is unbounded, and a
// ceiling below the floor is the floor): what
// its share would have exceeded re-splits over the other roles by the same
// weights, as computeRescaleTargets re-splits a capped model's excess. Uncapped,
// a role at its maxReplicas took a share it could not fill, and the other role
// was reclaimed to pay for it: prefill at its max of two 8-GPU pods with demand
// for 70 was given 35 GPUs of a 40-GPU target and decode 5, and decode went
// from 2 pods to 1 with 64 GPUs free. When every role reaches its ceiling the rest of total
// is left unassigned, so the roles sum to less than total.
func distributeGPUsByWeight(total int, roles []string, weight, fallback, floor, ceiling map[string]int) map[string]int {
	out := make(map[string]int, len(roles))
	if len(roles) <= 1 {
		if len(roles) == 1 {
			out[roles[0]] = total
		}
		return out
	}

	sumFloor := 0
	for _, r := range roles {
		sumFloor += floor[r]
	}
	remaining := total - sumFloor
	// capOf is each role's ceiling, never below its floor; unbounded is the
	// floor plus everything above the floors, which no share exceeds.
	capOf := make(map[string]int, len(roles))
	for _, r := range roles {
		capOf[r] = floor[r] + max(remaining, 0)
		if c, ok := ceiling[r]; ok {
			capOf[r] = max(min(c, capOf[r]), floor[r])
		}
	}

	w, sum := weight, 0
	for _, r := range roles {
		sum += w[r]
	}
	if sum == 0 {
		w, sum = fallback, 0
		for _, r := range roles {
			sum += w[r]
		}
	}
	if sum == 0 {
		// Nothing to weight by: deterministic fallback, in role order, each
		// role up to its ceiling.
		for _, r := range roles {
			out[r] = floor[r]
		}
		for _, r := range roles {
			give := min(max(remaining, 0), capOf[r]-out[r])
			out[r] += give
			remaining -= give
		}
		return out
	}

	// The floors are reserved first and the rest is water-filled by weight
	// under each role's ceiling, rounded by largest remainder: the model-level
	// split, applied to the roles.
	in := make([]rescaleInput, 0, len(roles))
	for _, r := range roles {
		in = append(in, rescaleInput{ID: r, Priority: 1, Demand: float64(w[r]), FloorGPUs: floor[r], CapGPUs: capOf[r]})
	}
	split, _ := computeRescaleTargets(in, total)
	maps.Copy(out, split)
	return out
}
