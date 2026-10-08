/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalefromzero

import (
	"fmt"
	"sort"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// Candidate is one inactive variant that could be woken, reduced to what
// selection needs: what it costs, what it would consume, and which half of a
// P/D pair it is.
type Candidate struct {
	// VariantName identifies the variant; ScaleTargetName is the workload the
	// activation decision is keyed by.
	VariantName     string
	ScaleTargetName string
	// Role is the normalized P/D role: "prefill", "decode" or "both".
	Role string
	// Accelerator is the GPU type this variant runs on.
	Accelerator string
	// GPUsPerReplica is what one replica consumes; waking is always one replica.
	GPUsPerReplica int
	// PodGPUs is each pod's GPUs in that replica, leader first; nil when no
	// pod requests a GPU explicitly.
	PodGPUs []int
	// Cost is the variant's declared cost, used to rank equally-feasible options.
	Cost float64
}

// SelectionOutcome explains what selection decided, for logging and metrics.
type SelectionOutcome string

const (
	// OutcomePair woke a decode and a prefill variant together.
	OutcomePair SelectionOutcome = "decode+prefill"
	// OutcomeDecodeOnly woke decode alone: either the model is not
	// disaggregated, or no prefill could be placed and requirePrefill is false.
	OutcomeDecodeOnly SelectionOutcome = "decode-only"
	// OutcomeAlreadyServing means the model already has a running decode and
	// nothing is left to wake. A queue building on a serving model is a
	// saturation problem, not an activation one. Distinct from the refusals
	// below because it is the steady state for every serving model and must not
	// be reported as a failure to wake.
	OutcomeAlreadyServing SelectionOutcome = "already-serving"
	// OutcomePrefillCatchUp woke a prefill variant for a model whose decode is
	// already up. It is not a cold start — the model can serve — but it is the
	// only path by which a P/D model that came up decode-first ever completes.
	OutcomePrefillCatchUp SelectionOutcome = "prefill-catchup"
	// OutcomeNoDecodeCandidate means the model has no inactive decode variant to
	// wake at all.
	OutcomeNoDecodeCandidate SelectionOutcome = "no-decode-candidate"
	// OutcomeNoCapacity means nothing could be placed within the GPU budget.
	OutcomeNoCapacity SelectionOutcome = "no-capacity"
	// OutcomePrefillRequired means a decode variant would fit, but no prefill
	// could be placed alongside it and the policy requires one.
	OutcomePrefillRequired SelectionOutcome = "prefill-required"
)

// SelectionInput is everything selectServingSet needs to choose a serving set.
type SelectionInput struct {
	// Namespace scopes the GPU budget lookup.
	Namespace string
	// Candidates are the model's inactive variants.
	Candidates []Candidate
	// DecodeCovered reports whether the model already has a running decode (or
	// "both") variant. When true the model can already serve and scale-from-zero
	// has nothing to do — a saturated-but-serving model is the saturation
	// engine's business, not this engine's.
	DecodeCovered bool
	// PrefillCovered reports whether a prefill variant is already running, in
	// which case waking another adds nothing to reachability.
	PrefillCovered bool
	// Constraints are the GPU/quota constraints to place within. Empty means
	// unknown, which FitsGPUBudget treats as permissive.
	Constraints []*allocation.ResourceConstraints
	// RequirePrefill refuses a decode-only wake for a disaggregated model.
	RequirePrefill bool
}

// selectServingSet chooses the cheapest set of variants to wake so the model can
// serve, or returns nothing with the reason why.
//
// Decode is mandatory and prefill is optional, which follows from how the llm-d
// router behaves rather than from a preference: with no prefill endpoint
// selected it sends the request to a decode endpoint, which runs both stages
// locally. Disaggregation is a TTFT/throughput optimization the router already
// declines on its own for short prompts and high cache hits. So a decode woken
// alone serves, and refusing to wake because prefill will not fit would trade a
// slower model for no model at all.
//
// The pair search is exhaustive rather than greedy. Picking the cheapest decode
// and the cheapest prefill independently can fail on the joint budget while some
// costlier pairing fits, and reporting "no capacity" when capacity exists is the
// failure this whole path is meant to prevent. Roles are at most two and
// candidates per role are a handful, so the cross-product is small enough to
// search exactly.
func selectServingSet(in SelectionInput) ([]Candidate, SelectionOutcome) {
	if in.DecodeCovered {
		// Serving, but not necessarily finished. A P/D model whose two halves
		// become visible on different ticks — which is the normal case, since each
		// is discovered when KEDA first calls about it — wakes decode alone, and
		// from the next tick onwards this branch would return here forever. The
		// prefill half is then stranded at zero permanently: this engine has
		// declared itself done, and the saturation engine only scales what is
		// already running, so nothing else will ever wake it.
		//
		// The model keeps serving throughout, which is why this went unnoticed —
		// the cost is a disaggregated deployment silently running degraded, with
		// the router doing prefill on the decode worker for the life of the model.
		//
		// Waking prefill here is not "waking prefill alone", the one P/D outcome
		// that is always wrong: decode is up by definition on this branch.
		if prefill, ok := prefillCatchUp(in); ok {
			return prefill, OutcomePrefillCatchUp
		}
		return nil, OutcomeAlreadyServing
	}

	decodes, prefills := splitByRole(in.Candidates)
	if len(decodes) == 0 {
		return nil, OutcomeNoDecodeCandidate
	}

	// Cheapest first, so the first feasible option found is the cheapest one and
	// ties resolve deterministically.
	sortByCost(decodes)
	sortByCost(prefills)

	// A model with no prefill variants at all, or one whose prefill is already
	// up, is not waiting on a pair.
	pairWanted := len(prefills) > 0 && !in.PrefillCovered

	if pairWanted {
		if pair, ok := cheapestFeasiblePair(in, decodes, prefills); ok {
			return pair, OutcomePair
		}
		if in.RequirePrefill {
			// A decode might well fit on its own, but the operator has said a
			// degraded-prefill model is worse than an absent one.
			return nil, OutcomePrefillRequired
		}
	}

	for _, d := range decodes {
		if fits(in, []Candidate{d}) {
			return []Candidate{d}, OutcomeDecodeOnly
		}
	}
	return nil, OutcomeNoCapacity
}

// hasPrefillCandidate reports whether any candidate is a prefill-only variant.
func hasPrefillCandidate(candidates []Candidate) bool {
	_, prefills := splitByRole(candidates)
	return len(prefills) > 0
}

// prefillCatchUp returns the cheapest prefill that can be placed for a model
// whose decode is already running and whose prefill is not.
//
// Judged on its own demand, not a pair's: the decode is already holding its GPUs
// and is counted as used, so charging for it again would refuse a prefill that
// fits.
func prefillCatchUp(in SelectionInput) ([]Candidate, bool) {
	if in.PrefillCovered {
		return nil, false
	}
	_, prefills := splitByRole(in.Candidates)
	if len(prefills) == 0 {
		return nil, false
	}
	sortByCost(prefills)
	for _, p := range prefills {
		if set := []Candidate{p}; fits(in, set) {
			return set, true
		}
	}
	return nil, false
}

// cheapestFeasiblePair returns the lowest-combined-cost (decode, prefill) pair
// that fits the budget together. Both slices must already be sorted by cost.
func cheapestFeasiblePair(in SelectionInput, decodes, prefills []Candidate) ([]Candidate, bool) {
	var (
		best  []Candidate
		found bool
		// bestCost is only read when found is true.
		bestCost float64
	)
	for _, d := range decodes {
		for _, p := range prefills {
			cost := d.Cost + p.Cost
			if found && cost >= bestCost {
				// Prefills are cost-sorted, so every remaining p in this row is
				// at least this expensive.
				break
			}
			set := []Candidate{d, p}
			if !fits(in, set) {
				continue
			}
			best, bestCost, found = set, cost, true
			break // cheapest prefill for this decode; costlier ones cannot win
		}
	}
	return best, found
}

// fits reports whether the set can be placed within the GPU budget, testing the
// combined demand rather than each member alone.
func fits(in SelectionInput, set []Candidate) bool {
	return allocation.FitsGPUBudget(in.Constraints, in.Namespace, demandOf(set))
}

// demandOf sums one replica of each candidate, keyed by accelerator type. Two
// candidates on the same accelerator therefore contend for the same pool, which
// is exactly the case a per-variant check would miss.
//
// A candidate whose accelerator could not be resolved contributes nothing. Its
// demand cannot be charged to any pool, and charging it to the placeholder would
// be worse than useless: no provider publishes a pool named "unknown", so the
// budget check would deny every such variant outright. Accelerator resolution
// depends on a nodeSelector or label that a workload need not carry, so that
// would turn an annotation gap into "this model can never wake". Unknown
// placement is treated the same way as an unknown budget — do not block.
//
// Resolution is tested with constants.IsAcceleratorResolved, the project-wide
// predicate, rather than an empty-string check:
// accelerator.GetAcceleratorNameFromScaleTarget never returns "" — it falls back
// to the DefaultAcceleratorName ("unknown") sentinel — and the "unresolved"
// label placeholder can flow back in from metrics. An == "" check would silently
// never fire.
func demandOf(set []Candidate) map[string]int {
	demand := make(map[string]int, len(set))
	for _, c := range set {
		if !constants.IsAcceleratorResolved(c.Accelerator) {
			continue
		}
		gpus := c.GPUsPerReplica
		if gpus <= 0 {
			// Mirrors scaletarget.GetTotalGPUsPerReplica's floor: a workload with
			// no explicit GPU request still occupies one.
			gpus = 1
		}
		demand[c.Accelerator] += gpus
	}
	return demand
}

// candidateDemand renders what each candidate would ask for, keyed by variant
// name, for the refusal log.
//
// It goes through demandOf rather than reading GPUsPerReplica directly, so the
// reported figure is the one the budget check used. That matters most for the
// candidate demandOf drops: an unresolved accelerator contributes NOTHING, so a
// variant that looks like it was refused for capacity was in fact never weighed
// at all — a distinction the verdict alone cannot express.
func candidateDemand(candidates []Candidate) map[string]string {
	demand := make(map[string]string, len(candidates))
	for _, c := range candidates {
		perType := demandOf([]Candidate{c})
		if len(perType) == 0 {
			demand[c.VariantName] = "none (accelerator unresolved)"
			continue
		}
		for accType, gpus := range perType {
			demand[c.VariantName] = fmt.Sprintf("%s=%d", accType, gpus)
		}
	}
	return demand
}

// splitByRole separates candidates into decode-capable and prefill-only. A
// "both" variant counts as decode: it serves complete requests on its own, which
// is what decode-capable means here.
func splitByRole(candidates []Candidate) (decodes, prefills []Candidate) {
	for _, c := range candidates {
		switch c.Role {
		case domain.RolePrefill:
			prefills = append(prefills, c)
		default:
			decodes = append(decodes, c)
		}
	}
	return decodes, prefills
}

// sortByCost orders candidates cheapest first, breaking ties on name so
// selection is stable across ticks and does not flap between equal-cost
// variants.
func sortByCost(candidates []Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Cost != candidates[j].Cost {
			return candidates[i].Cost < candidates[j].Cost
		}
		return candidates[i].VariantName < candidates[j].VariantName
	})
}
