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
// cannot spend it again -- to the namespace's guaranteed budget (never below 0)
// and its reach, and, for the part past the guaranteed figure (growth into
// nominal a peer holds), to the namespace's cohort pools, clamped at 0.
//
// The cluster budget needs no carry-over: only a namespace-scope provider
// borrows, and a namespace-scope provider is never a cluster bound, so the
// within-nominal copy merges to the same cluster budget. Rescale debits the
// pass's own available in place.
func (o *GreedyByScoreOptimizer) applyRescaleWithinNominal(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
	available map[string]int,
	availableByNS map[string]map[string]int,
	bs *borrowState,
) ([]domain.VariantDecision, map[string]bool) {
	within := WithinNominal(constraints)
	wnByNS := mergeNamespaceConstraints(within)
	beforeByNS := make(map[string]map[string]int, len(wnByNS))
	for ns, perType := range wnByNS {
		beforeByNS[ns] = maps.Clone(perType)
	}

	decisions, handled := o.applyRescale(ctx, requests, within, available, wnByNS)

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
	return decisions, handled
}
