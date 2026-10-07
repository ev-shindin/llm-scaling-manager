package scalefromzero

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
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
// A claim funds one replica, so a model that must wake a prefill with its
// decode does not claim. Decode candidates are tried cheapest first.
func (e *Engine) claimShareTransfer(ctx context.Context, group modelGroup, candidates []Candidate) (Candidate, bool) {
	if e.config == nil || e.requirePrefill(group.modelID, group.namespace) {
		return Candidate{}, false
	}
	zNamespace, zCluster, ok := e.config.UtilizationShareWakeScores(group.namespace, group.modelID)
	if !ok {
		return Candidate{}, false
	}
	decodes, _ := splitByRole(candidates)
	sortByCost(decodes)
	last, lastAcc := "", ""
	for _, c := range decodes {
		if !constants.IsAcceleratorResolved(c.Accelerator) {
			continue
		}
		claim, outcome := decision.DefaultShareClaims.Claim(group.namespace, c.Accelerator, c.PodGPUs,
			c.GPUsPerReplica, zNamespace, zCluster, utils.GetNamespacedKey(group.namespace, c.VariantName), time.Now())
		if outcome == decision.ShareClaimRedirected {
			scope := claim.Scope
			if scope == "" {
				scope = constants.UtilizationShareClusterScope
			}
			metrics.CountUtilizationShareClaim(c.Accelerator, scope, outcome)
			e.claimOutcomeChanged(group.key(), "")
			ctrl.LoggerFrom(ctx).Info("Scale-from-zero: woke a model by claiming a releasing utilization-share transfer",
				"namespace", group.namespace, "modelID", group.modelID, "variant", c.VariantName,
				"transfer", claim.ID, "scope", scope)
			return c, true
		}
		last, lastAcc = outcome, c.Accelerator
	}
	// A refused claim is counted once per change: this loop runs at 10 Hz. Its
	// scope is the wake's own namespace -- which group refused is not known.
	if last != "" && e.claimOutcomeChanged(group.key(), last) {
		metrics.CountUtilizationShareClaim(lastAcc, group.namespace, last)
	}
	return Candidate{}, false
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
