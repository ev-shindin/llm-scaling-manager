package scalefromzero

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// claimShareTransfer tries to fund a wake that found no capacity by claiming a
// utilization-share transfer still releasing for another receiver
// (docs/proposals/utilization-share-optimizer.md, section 6.3). The claim is
// allowed when the parked model scores lower -- is worse off -- than that
// receiver, and its replica fits the donor pods being released. On success the
// wake proceeds now: its pod waits for the hole, and the steady-state engine
// redirects the transfer on its next pass so the original receiver is not
// raised into it.
//
// The wake claims a transfer for each role it must start: the decode unless
// one is serving, and, for a model that must wake a prefill with its decode,
// the prefill unless one is serving. A P/D pair claims both or neither, on
// one accelerator. Candidates are tried cheapest first, decode before
// prefill.
//
// constraints are the wake's own: a namespace with its own quota claims only in
// its namespace group, any other namespace only in the cluster group.
func (e *Engine) claimShareTransfer(ctx context.Context, group modelGroup, candidates []Candidate,
	constraints []*allocation.ResourceConstraints, covered coverage) ([]Candidate, bool) {
	if e.config == nil {
		return nil, false
	}
	zNamespace, zCluster, ok := e.config.UtilizationShareWakeScores(group.namespace, group.modelID)
	if !ok {
		return nil, false
	}
	scope, z := "", zCluster
	if _, nsScoped := allocation.GPUBudgets(constraints, group.namespace); nsScoped {
		scope, z = group.namespace, zNamespace
	}
	decodes, prefills := splitByRole(candidates)
	sortByCost(decodes)
	sortByCost(prefills)
	// Each role the wake must start is a list of candidates; a role it need
	// not start is one empty slot.
	none := []Candidate{{}}
	if covered.decode {
		decodes = none
	}
	if !e.requirePrefill(group.modelID, group.namespace) || covered.prefill {
		prefills = none
	}
	last, lastAcc := "", ""
	for _, d := range decodes {
		for _, p := range prefills {
			var set []Candidate
			for _, c := range []Candidate{d, p} {
				if c.VariantName != "" {
					set = append(set, c)
				}
			}
			if len(set) == 0 || !sameResolvedAccelerator(set) {
				continue
			}
			acc := set[0].Accelerator
			wakes := make([]decision.ShareWake, 0, len(set))
			for _, c := range set {
				wakes = append(wakes, decision.ShareWake{Pods: c.PodGPUs, GPUs: c.GPUsPerReplica,
					Variant: utils.GetNamespacedKey(group.namespace, c.VariantName)})
			}
			claims, outcome := decision.DefaultShareClaims.ClaimSet(scope, acc, wakes, z,
				utils.GetNamespacedKey(group.namespace, group.modelID), time.Now())
			if outcome == decision.ShareClaimRedirected {
				scope := claims[0].Scope
				if scope == "" {
					scope = constants.UtilizationShareClusterScope
				}
				metrics.CountUtilizationShareClaim(acc, scope, outcome)
				e.claimOutcomeChanged(group.key(), "")
				ids := make([]string, 0, len(claims))
				for _, c := range claims {
					ids = append(ids, c.ID)
				}
				variants := make([]string, 0, len(set))
				for _, c := range set {
					variants = append(variants, c.VariantName)
				}
				ctrl.LoggerFrom(ctx).Info("Scale-from-zero: woke a model by claiming releasing utilization-share transfers",
					"namespace", group.namespace, "modelID", group.modelID, "variants", variants,
					"transfers", ids, "scope", scope)
				return set, true
			}
			last, lastAcc = outcome, acc
		}
	}
	// A refused claim is counted once per change: this loop runs at 10 Hz. Its
	// scope is the wake's own namespace -- which group refused is not known.
	if last != "" && e.claimOutcomeChanged(group.key(), last) {
		metrics.CountUtilizationShareClaim(lastAcc, group.namespace, last)
	}
	return nil, false
}

// sameResolvedAccelerator reports whether every candidate runs on one known
// accelerator: a claim set is made in one group.
func sameResolvedAccelerator(set []Candidate) bool {
	for _, c := range set {
		if !constants.IsAcceleratorResolved(c.Accelerator) || c.Accelerator != set[0].Accelerator {
			return false
		}
	}
	return true
}

// claimOutcomeChanged records a model's last claim outcome and reports whether
// it differs from the one before. "" forgets it.
func (e *Engine) claimOutcomeChanged(key, outcome string) bool {
	e.refusalMu.Lock()
	defer e.refusalMu.Unlock()
	if outcome == "" {
		delete(e.lastClaim, key)
		return true
	}
	if e.lastClaim == nil {
		e.lastClaim = map[string]string{}
	}
	if e.lastClaim[key] == outcome {
		return false
	}
	e.lastClaim[key] = outcome
	return true
}
