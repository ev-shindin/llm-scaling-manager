package allocation

import (
	"maps"
	"math"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// FitsGPUBudget reports whether allocating demand — GPUs keyed by accelerator
// type — is permitted in namespace under the given constraints.
//
// It answers the question the scale-from-zero engine has to ask before waking a
// variant: would this actually get GPUs, or would its pods sit Pending while the
// request that asked for it times out anyway. Waking something that cannot be
// placed is worse than not waking it, because it looks like progress.
//
// The budget semantics are the optimizer's, reused rather than restated so the
// two cannot drift (see the budget model in budget_merge.go):
//
//   - The cluster bounds (physical inventory, cluster-scope quotas) are
//     intersected, with the Limit < 0 sentinel meaning unlimited; a type they do
//     not list is denied. With no cluster bound in force the cluster is
//     unbounded.
//   - A namespace that appears in any provider's NamespacePools is a CLOSED
//     allowlist: an accelerator type it does not list is denied outright, and a
//     listed type is capped at the tighter of its cluster and namespace budgets
//     (see mergeNamespaceConstraints and GPUBudgets).
//
// No constraints at all means "unknown", which returns true. That mirrors the
// engine's own fallback when no provider can supply constraints: proceed rather
// than block scaling silently. Denying instead would keep a model down for the
// first cycle after every restart — exactly when a queued request is waiting on
// it.
//
// A workload that names no accelerator ("unknown") can be placed on any GPU, so
// on the cluster path it fits if any pool does (anyPoolFits). In a namespace
// with a closed allowlist that would let it draw on a quota it is charged to
// nowhere: the quota tracks usage per type, and an unnamed workload's GPUs land
// in no type. So there it fits only when the allowlist lists "unknown" itself,
// or lists exactly one type, which it is then checked (and, its pods landing
// there, charged) against; an allowlist of several types denies it. The
// alternative, charging the largest remaining type, would guess a type the
// scheduler is free to contradict.
func FitsGPUBudget(constraints []*ResourceConstraints, namespace string, demand map[string]int) bool {
	if len(constraints) == 0 {
		return true
	}

	// One map for every branch below, so a workload with no named accelerator
	// is judged against the same figures as one that names its GPU.
	budgets, nsScoped := GPUBudgets(constraints, namespace)
	if budgets == nil && !nsScoped {
		return true // no cluster bound, and no allowance for this namespace
	}

	for rawType, need := range demand {
		if need <= 0 {
			continue
		}
		key, known := accelerator.FindBudgetKey(budgets, rawType)
		if rawType == constants.DefaultAcceleratorName && !known {
			if !nsScoped {
				// A workload that constrains no accelerator can be placed on any
				// of them, so it fits if any pool does. See anyPoolFits.
				if !anyPoolFits(budgets, need) {
					return false
				}
				continue
			}
			if len(budgets) != 1 {
				return false // not chargeable to the namespace's allowance
			}
			key, known = slices.Collect(maps.Keys(budgets))[0], true
		}
		// In a closed namespace the map is the allowlist, so an unlisted type is
		// denied; on the cluster path it is the merged cluster bounds, closed too.
		if !known {
			return false
		}
		budget := budgets[key]

		// A negative budget survives tighterBudget only when every contributing
		// budget was unlimited.
		if isUnboundedBudget(budget) {
			continue
		}
		if need > budget {
			return false
		}
	}

	return true
}

// GPUBudgets reports the per-accelerator-type budgets an allocation in namespace
// is judged against, and whether that namespace is a closed allowlist (in which
// case a type absent from the map is denied outright). On the cluster path a
// type absent from a non-nil map is denied too, and a nil map is unbounded: no
// cluster bound is in force. A negative budget is the unlimited sentinel.
//
// It exists so the budgets can be REPORTED with the same resolution FitsGPUBudget
// applies. Nothing else exposes what the check actually compared against: the
// pools carry Limit and Used, the merge takes the minimum across providers, and a
// namespace allowlist overrides the cluster figures — so a log line that summed
// the pools itself would be free to disagree with the verdict it purports to
// explain, which is worse than no line at all.
func GPUBudgets(constraints []*ResourceConstraints, namespace string) (map[string]int, bool) {
	if len(constraints) == 0 {
		return nil, false
	}

	perType := mergeConstraints(constraints)
	nsBudgets, nsScoped := mergeNamespaceConstraints(constraints)[namespace]
	if !nsScoped {
		return perType, false
	}

	// The allowlist decides what the namespace may use, still bounded by the
	// cluster bounds: a namespace may not be granted more than they allow, and
	// a type they do not list is 0 (clusterBudgetFor).
	budgets := make(map[string]int, len(nsBudgets))
	for accType, budget := range nsBudgets {
		if cv, bounded := clusterBudgetFor(perType, accType); bounded && cv != math.MaxInt {
			budget = tighterBudget(budget, cv)
		}
		budgets[accType] = budget
	}
	return budgets, true
}

// anyPoolFits reports whether some pool in budgets can take need GPUs.
//
// This answers demand from a workload that constrains nothing. Its pod spec names
// no accelerator, so the scheduler may place it on any GPU node — "any accelerator
// can serve this" is the literal meaning of the configuration, not a guess about
// it. The placement question is therefore whether ANY pool has room, and asking it
// that way is both more correct and more permissive than the alternatives: naming
// one pool would assert something the scheduler can contradict, and denying
// outright would refuse a workload that several accelerators could have served.
//
// It subsumes the single-type case. With one pool, "any pool has room" and "the
// sole pool has room" are the same statement, which is why there is no separate
// homogeneous branch here.
//
// Attribution is a different question and is NOT answered here: which pool such a
// workload should be CHARGED to is genuinely unknown until its pods are scheduled.
// TypeInventory.SetUsed deduces it on a single-type cluster and leaves it
// unattributed otherwise, reported as wva_unattributed_gpus.
func anyPoolFits(budgets map[string]int, need int) bool {
	for _, budget := range budgets {
		if budget < 0 || need <= budget {
			return true
		}
	}
	return false
}
