package allocation

import (
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// The GPU budget model, shared by the optimizers, the placement check and the
// warm-pool headroom.
//
// Two kinds of bound apply to a decision. A CLUSTER bound is a provider whose
// per-type pools hold for every namespace: the physical inventory and a
// cluster-scope quota. Each is a closed allowlist -- a type it does not list has
// no GPUs under it -- and they are intersected (mergeConstraints). A NAMESPACE
// bound is a namespace-scope quota's per-namespace allowlist (NamespacePools),
// closed for the namespaces it holds and intersected across the providers that
// hold a namespace (mergeNamespaceConstraints). A namespace-scope quota's
// per-type Pools, the sum of its namespace caps, bounds nothing: a namespace it
// excludes bypasses it, and every namespace it holds is bounded by its own cap.
//
// With no cluster bound in force the cluster budget is nil, which reads as
// unbounded for every type: a namespace the quotas exclude scales freely, and a
// namespace with an allowance is bounded by that allowance alone.
//
// Accelerator names form a family/product tree (accelerator.Family). A budget
// key that is a family ("H100") bounds the SUM of what every product under it
// spends; a product key bounds that product alone, and never exceeds its family
// key (clampProducts). A spend under a product therefore also debits its family
// key (debitBudget), and the total a budget holds counts each family once
// (budgetTotal). Workloads find their key with accelerator.FindBudgetKey.

// isClusterBound reports whether c bounds the whole cluster: it is not a
// namespace-scope provider. A provider that carries namespace pools is one,
// whether or not it says so (NamespaceScoped is how one with no active
// namespace says it).
func isClusterBound(c *ResourceConstraints) bool {
	return c != nil && !c.NamespaceScoped && c.NamespacePools == nil
}

// clusterBounds returns the constraints that bound the whole cluster.
func clusterBounds(constraints []*ResourceConstraints) []*ResourceConstraints {
	var out []*ResourceConstraints
	for _, c := range constraints {
		if isClusterBound(c) {
			out = append(out, c)
		}
	}
	return out
}

// mergeConstraints intersects the cluster bounds into one budget per
// accelerator key: the free GPUs every cluster bound allows, at the smallest
// figure any of them gives. Nil when no cluster bound is in force, which is
// unbounded (see the package comment above); an empty, non-nil map denies
// everything. Used by both optimizers, the placement check and the warm-pool
// headroom.
//
// The keys are every spelling a cluster bound uses, spellings that differ only
// in case folded to the lexically smallest. A provider that does not list a
// name bounds it at 0, except the catch-all "unknown", which physical inventory
// never lists and a quota that declares it means. A family name a provider lists
// only through products is bounded by their sum (allowlistBound), so a physical
// pool of {H100: 5} beside a quota of {NVIDIA-H100-80GB-HBM3: 4,
// NVIDIA-H100-SXM5-80GB: 4} merges to {H100: 5, ...HBM3: 4, ...SXM5: 4}, and the
// family key keeps the two products to the 5 GPUs that exist.
//
// The unlimited sentinel (Limit < 0) is an unbounded budget, math.MaxInt, so the
// optimizer allocates the type up to the model's fair-share demand. A finite
// pool from any provider still wins through the minimum.
func mergeConstraints(constraints []*ResourceConstraints) map[string]int {
	cluster := clusterBounds(constraints)
	if len(cluster) == 0 {
		return nil
	}
	var names []string
	for _, c := range cluster {
		names = slices.AppendSeq(names, maps.Keys(c.Pools))
	}
	merged := make(map[string]int, len(names))
	for _, name := range distinctSpellings(names) {
		budget, bounded := 0, false
		for _, c := range cluster {
			b, ok := clusterBound(c.Pools, name)
			if !ok {
				continue
			}
			if !bounded || b < budget {
				budget, bounded = b, true
			}
		}
		if bounded {
			merged[name] = budget
		}
	}
	clampProducts(merged)
	return merged
}

// clusterBound is the budget one cluster bound puts on the accelerator called
// name, and whether it bounds it at all: by allowlistBound, at 0 when it does
// not list the name (a closed allowlist), and not at all for an unlisted
// "unknown".
func clusterBound(pools map[string]ResourcePool, name string) (int, bool) {
	avail, unlimited, listed := allowlistBound(pools, name)
	switch {
	case !listed && name == constants.DefaultAcceleratorName:
		return 0, false
	case !listed:
		return 0, true
	case unlimited:
		return math.MaxInt, true
	}
	return avail, true
}

// allowlistBound is what one allowlist (pools keyed by accelerator) allows on
// name, whether that is unlimited, and whether it lists name at all.
//
//   - A key equal to name ignoring case is its pool; for a product name, a
//     listed family key bounds it too.
//   - A family name listed only through products is bounded by their SUM: an
//     allowlist of {NVIDIA-H100-80GB-HBM3: 4, NVIDIA-H100-SXM5-80GB: 4} allows
//     8 H100s, not 4.
//   - A product name not listed itself falls back to its family key, which
//     admits every product under it (accelerator.FindKey's reading).
//
// Several case spellings of one key take the tightest.
func allowlistBound(pools map[string]ResourcePool, name string) (avail int, unlimited, listed bool) {
	var own, family, products []ResourcePool
	for key, p := range pools {
		switch {
		case strings.EqualFold(key, name):
			own = append(own, p)
		case accelerator.IsProductOf(key, name):
			products = append(products, p)
		case accelerator.IsProductOf(name, key):
			family = append(family, p)
		}
	}
	switch {
	case len(own) > 0:
		avail, unlimited = tightestPool(own)
		if len(family) > 0 {
			fa, fu := tightestPool(family)
			avail, unlimited = tighterBound(avail, unlimited, fa, fu)
		}
	case len(products) > 0:
		for _, p := range products {
			if p.Limit < 0 {
				return 0, true, true
			}
			avail += p.Available()
		}
	case len(family) > 0:
		avail, unlimited = tightestPool(family)
	default:
		return 0, false, false
	}
	return avail, unlimited, true
}

// tightestPool is the smallest free figure among pools, unlimited only when
// every one of them is.
func tightestPool(pools []ResourcePool) (avail int, unlimited bool) {
	unlimited = true
	for _, p := range pools {
		if p.Limit < 0 {
			continue
		}
		avail, unlimited = tighterBound(avail, unlimited, p.Available(), false)
	}
	return avail, unlimited
}

// tighterBound is the tighter of two (free, unlimited) bounds.
func tighterBound(a int, aUnlimited bool, b int, bUnlimited bool) (int, bool) {
	switch {
	case aUnlimited:
		return b, bUnlimited
	case bUnlimited:
		return a, false
	}
	return min(a, b), false
}

// distinctSpellings returns names with spellings that differ only in case
// folded to the lexically smallest, sorted so a merge is deterministic.
func distinctSpellings(names []string) []string {
	slices.Sort(names)
	var out []string
	for _, n := range names {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, n) }) {
			out = append(out, n)
		}
	}
	return out
}

// isUnboundedBudget reports whether a budget value is one of the two unlimited
// sentinels: math.MaxInt in a cluster or effective budget, a negative value in
// a namespace budget.
func isUnboundedBudget(v int) bool {
	return v < 0 || v == math.MaxInt
}

// clampProducts lowers every product key of budgets to the finite family key
// that covers it: a product never has more free than its family, and an
// unlimited product under a finite family is bounded by it.
func clampProducts(budgets map[string]int) {
	for p, pv := range budgets {
		if accelerator.IsFamily(p) {
			continue
		}
		for f, fv := range budgets {
			if isUnboundedBudget(fv) || !accelerator.IsProductOf(p, f) {
				continue
			}
			if isUnboundedBudget(pv) || pv > fv {
				pv = fv
			}
		}
		budgets[p] = pv
	}
}

// debitBudget spends n GPUs under key: from key itself, from every spelling of
// it that differs only in case, and from the family key that covers it, since
// every GPU a product spends is one of its family's (accelerator.Covers). The
// products under a debited family are then clamped to it (clampProducts). An
// unbounded value (math.MaxInt, or a negative namespace sentinel) is never drawn
// down, and no budget goes below 0. A nil budget is unbounded and is left nil.
func debitBudget(budgets map[string]int, key string, n int) {
	if n <= 0 {
		return
	}
	for other, v := range budgets {
		if !isUnboundedBudget(v) && accelerator.Covers(other, key) {
			budgets[other] = max(0, v-n)
		}
	}
	clampProducts(budgets)
}

// budgetTotal is the number of GPUs a budget still holds, saturating at
// math.MaxInt for an unbounded key or a nil (unbounded) budget. Used for the
// optimizer's "nothing left" stop check.
//
// Each family is counted once: by its family key when the budget has one --
// every product under it draws on it -- else by the sum of its product keys.
// Spellings that differ only in case are one key and count at the larger
// figure: every spend debits both, so the stop check, which must not stop while
// anything can still be spent, reads the one that has more left.
func budgetTotal(budgets map[string]int) int {
	if budgets == nil {
		return math.MaxInt
	}
	type family struct {
		family    int
		hasFamily bool
		products  map[string]int
	}
	families := map[string]*family{}
	for key, v := range budgets {
		if isUnboundedBudget(v) {
			v = math.MaxInt
		}
		name := strings.ToLower(accelerator.Family(key))
		f := families[name]
		if f == nil {
			f = &family{products: map[string]int{}}
			families[name] = f
		}
		if accelerator.IsFamily(key) {
			f.family, f.hasFamily = max(f.family, v), true
			continue
		}
		p := strings.ToLower(key)
		f.products[p] = max(f.products[p], v)
	}
	total := 0
	for _, f := range families {
		held := f.family
		if !f.hasFamily {
			held = 0
			for _, v := range f.products {
				held = saturatingAdd(held, v)
			}
		}
		total = saturatingAdd(total, held)
		if total == math.MaxInt {
			return total
		}
	}
	return total
}

// saturatingAdd adds two non-negative budgets, saturating at math.MaxInt.
func saturatingAdd(a, b int) int {
	if a == math.MaxInt || b == math.MaxInt || a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// budgetKey is the key of budgets that the accelerator a workload declares is
// spent from (accelerator.FindBudgetKey), or the declared name itself when
// budgets holds none, so a read finds no budget and a debit touches nothing.
// Every budget read and debit in the optimizers goes through it.
func budgetKey(budgets map[string]int, declared string) string {
	if key, ok := accelerator.FindBudgetKey(budgets, declared); ok {
		return key
	}
	return declared
}

// budgetOf is what budgets allows a workload declaring name: unbounded
// (math.MaxInt) for a nil budget, the value of its key, and 0 when budgets holds
// no key for it -- a non-nil budget is a closed allowlist.
func budgetOf(budgets map[string]int, name string) int {
	if budgets == nil {
		return math.MaxInt
	}
	if key, ok := accelerator.FindBudgetKey(budgets, name); ok {
		return budgets[key]
	}
	return 0
}

// clusterBudgetFor is the cluster bound on an accelerator a namespace allowance
// lists, and whether there is one: none for a nil (unbounded) cluster budget, the
// value of its key, and 0 for a type the cluster bounds do not list, since each
// of them is a closed allowlist. "unknown" is exempt, as in mergeConstraints:
// no physical inventory lists it, so a namespace that grants it is bounded by
// its own allowance alone.
func clusterBudgetFor(available map[string]int, name string) (int, bool) {
	if available == nil {
		return 0, false
	}
	if key, ok := accelerator.FindBudgetKey(available, name); ok {
		return available[key], true
	}
	if name == constants.DefaultAcceleratorName {
		return 0, false
	}
	return 0, true
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
// bounds: a type is allowed only if EVERY provider that holds the namespace
// allows it (allowlistBound, which reads the family/product tree), at the
// tightest figure among them, and is an explicit 0 otherwise. A provider that
// does not hold the namespace adds no rule for it.
//
// An unlimited (-1 sentinel) pool is carried through as a negative budget so
// the optimizer can distinguish "unlimited" from a finite cap; tighterBudget
// treats it as +infinity when taking the minimum.
func mergeNamespaceConstraints(constraints []*ResourceConstraints) map[string]map[string]int {
	holders := namespaceHolders(constraints)
	if len(holders) == 0 {
		return nil
	}
	merged := make(map[string]map[string]int, len(holders))
	for ns, pools := range holders {
		var names []string
		for _, perType := range pools {
			names = slices.AppendSeq(names, maps.Keys(perType))
		}
		inner := make(map[string]int, len(names))
		for _, name := range distinctSpellings(names) {
			for i, perType := range pools {
				budget := 0 // not allowed by this holder: denied
				if avail, unlimited, listed := allowlistBound(perType, name); listed {
					budget = avail
					if unlimited {
						budget = -1
					}
				}
				if i == 0 {
					inner[name] = budget
				} else {
					inner[name] = tighterBudget(inner[name], budget)
				}
			}
		}
		clampProducts(inner)
		merged[ns] = inner // present (even if empty) marks a closed namespace
	}
	return merged
}

// namespaceHolders groups every provider's allowlist for each namespace it
// holds.
func namespaceHolders(constraints []*ResourceConstraints) map[string][]map[string]ResourcePool {
	holders := map[string][]map[string]ResourcePool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespacePools {
			holders[ns] = append(holders[ns], perType)
		}
	}
	return holders
}
