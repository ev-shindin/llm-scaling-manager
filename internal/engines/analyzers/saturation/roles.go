package saturation

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// P/D role predicates. Small, and together because every file above asks the
// same few questions.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

// rolesFromStates builds the variant-name -> role lookup the per-role helpers
// take, from the variant states an Analyze call carries.
func rolesFromStates(states []domain.VariantReplicaState) map[string]string {
	roles := make(map[string]string, len(states))
	for _, vs := range states {
		roles[vs.VariantName] = vs.Role
	}
	return roles
}

// generatesOutput reports whether a replica is one whose completions carry
// the workload's real output length and service time: every role but prefill.
// A variant with no recorded role is domain.RoleBoth, which generates.
func generatesOutput(rm domain.ReplicaMetrics, rolesByVariant map[string]string) bool {
	return canonicalRole(rolesByVariant[rm.VariantName]) != domain.RolePrefill
}

// canonicalRole normalizes an empty variant role to domain.RoleBoth, matching
// aggregation.AggregateByRole.
// stableAccelerator returns the accelerator to key history under, preferring the
// last one that resolved for this variant over an unresolved reading.
//
// Capacity is a property of the hardware, so it belongs in the key; but "the pods
// currently disagree about it" is not a different accelerator, and treating it as
// one is what makes the fleet oscillate. A resolved reading always wins and is
// remembered; an unresolved one falls back to what was remembered, and only when
// nothing was ever resolved does the unresolved value itself get used, which
// keeps a never-resolvable variant behaving exactly as it does today.
func (a *SaturationAnalyzer) stableAccelerator(namespace, variantName, accelerator string) string {
	key := namespace + "/" + variantName
	a.mu.Lock()
	defer a.mu.Unlock()
	if constants.IsAcceleratorResolved(accelerator) {
		a.lastAccelerator[key] = acceleratorMemo{name: accelerator, lastUsed: time.Now()}
		return accelerator
	}
	if last, ok := a.lastAccelerator[key]; ok {
		// Touched on READ as well as on write: the memo should live as long as the
		// variant is being analysed, not as long as its accelerator keeps
		// resolving -- a variant that stops resolving is exactly the one this
		// exists for. A variant that stops being analysed goes quiet on both, and
		// EvictStaleHistory then ages it out with the k2 history beside it.
		last.lastUsed = time.Now()
		a.lastAccelerator[key] = last
		return last.name
	}
	return accelerator
}

func canonicalRole(role string) string {
	if role == "" {
		return domain.RoleBoth
	}
	return role
}

// roleSaturated reports whether any replica of the given role is full and
// queued this cycle: its local queue at or over the threshold, as computeK2
// admits a P1-obs reading on, AND its resident KV at or over its k1 (the
// cache times kvCacheThreshold) and within the cache's physical ceiling (a
// reading above it is a scrape artifact there, and no more a saturation
// here). The queue alone would not do: a decode request waiting for its
// remote KV sits in vLLM's waiting count, so a slow transfer keeps a queue
// on a decode that is admitting fine. A row with no cache size cannot be
// judged and does not count. Every row counts, bridges included: a
// warm-pool Pod lent to decode that is full and queued is decode saturated
// as much as one of its own replicas is.
func roleSaturated(metrics []domain.ReplicaMetrics, rolesByVariant map[string]string, role string, queueThreshold, kvCacheThreshold float64) bool {
	for _, rm := range metrics {
		if canonicalRole(rolesByVariant[rm.VariantName]) != role {
			continue
		}
		if rm.QueueLength < int(queueThreshold) || rm.TokensInUse <= 0 || rm.TotalKvCapacityTokens <= 0 {
			continue
		}
		if rm.TokensInUse > rm.TotalKvCapacityTokens {
			continue
		}
		if rm.TokensInUse >= memoryBound(rm.TotalKvCapacityTokens, kvCacheThreshold) {
			return true
		}
	}
	return false
}
