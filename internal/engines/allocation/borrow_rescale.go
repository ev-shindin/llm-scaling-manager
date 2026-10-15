package allocation

import (
	"context"
	"maps"
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// applyRescaleWithinNominal is applyRescale for a pass that counts cohort
// borrowing. Rescale spends capacity within nominal only, never borrowed
// capacity (B7): it runs over the within-nominal copy of the constraints, and
// what it fills is then charged to the pass's own budgets so the additive path
// cannot spend it again -- to the cluster budget, to the namespace's guaranteed
// budget (never below 0) and its reach, and, for the part past the guaranteed
// figure (growth into nominal a peer holds), to the namespace's cohort pools,
// clamped at 0.
func (o *GreedyByScoreOptimizer) applyRescaleWithinNominal(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
	available map[string]int,
	availableByNS map[string]map[string]int,
	bs *borrowState,
) ([]domain.VariantDecision, map[string]bool) {
	within := WithinNominal(constraints)
	wnAvail := mergeConstraints(within)
	wnByNS := mergeNamespaceConstraints(within)
	beforeAvail := maps.Clone(wnAvail)
	beforeByNS := make(map[string]map[string]int, len(wnByNS))
	for ns, perType := range wnByNS {
		beforeByNS[ns] = maps.Clone(perType)
	}

	decisions, handled := o.applyRescale(ctx, requests, within, wnAvail, wnByNS)

	for ns, perType := range beforeByNS {
		for accType, before := range perType {
			consumed := before - wnByNS[ns][accType]
			if before < 0 || consumed <= 0 {
				continue
			}
			g := availableByNS[ns]
			key := budgetKey(g, accType)
			guaranteed := g[key]
			if g != nil && guaranteed >= 0 {
				g[key] = max(0, guaranteed-consumed)
				if past := consumed - guaranteed; past > 0 {
					bs.debitPool(ns, accType, past)
				}
			}
			if r := bs.reachFor(ns); r != nil {
				if k := budgetKey(r, accType); r[k] >= 0 && r[k] != math.MaxInt {
					r[k] = max(0, r[k]-consumed)
				}
			}
		}
	}
	// The cluster budget: everything rescale filled, namespace fills included,
	// was debited from the within-nominal cluster figure; carry it over.
	for accType, before := range beforeAvail {
		consumed := before - wnAvail[accType]
		if consumed <= 0 || before == math.MaxInt {
			continue
		}
		key := budgetKey(available, accType)
		if cur, ok := available[key]; ok && cur != math.MaxInt {
			available[key] = max(0, cur-consumed)
		}
	}
	return decisions, handled
}
