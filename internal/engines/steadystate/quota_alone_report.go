package steadystate

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// reportQuotaAloneAllocation is the transitional upgrade log for policies that
// declare both a physical limiter and quota entries. TRANSITIONAL: remove one
// release after the release that builds both.
//
// Until that release such a policy was bounded by its quotas alone -- the
// physical entry was silently never built. It is now bounded by the GPUs that
// exist as well, which can be tighter: the physical view counts every
// GPU-holding pod, training included. So that the change is visible before an
// operator meets it as a model that stopped growing, each cycle runs the
// optimizer a second time, as a shadow that publishes nothing, over the quota
// constraints alone, and logs every variant whose target differs from the
// composed one. Logged when the set of differences changes, not every cycle.
//
// composed is the real pass's decisions, before any utilization-share override;
// constraints are the ones it was given.
func (e *Engine) reportQuotaAloneAllocation(
	ctx context.Context,
	optimizer allocation.ScalingOptimizer,
	requests []allocation.ModelScalingRequest,
	constraints []*allocation.ResourceConstraints,
	composed []domain.VariantDecision,
) {
	if e.Config == nil || !e.Config.HasPhysicalLimiter() || !e.Config.HasQuotaLimiter() {
		return
	}
	greedy, ok := optimizer.(*allocation.GreedyByScoreOptimizer)
	if !ok {
		return // no bound applied this cycle at all
	}
	quotaAlone := quotaConstraints(gpuConstraintProviders(e.currentGPULimiter()), constraints)
	if len(quotaAlone) == 0 || len(quotaAlone) == len(constraints) {
		// Either no quota could be computed, or the physical one could not:
		// the two passes would bound the same way, or not at all.
		return
	}
	diff := quotaAloneDiff(composed, greedy.ShadowOptimize(ctx, requests, quotaAlone))
	if diff == e.lastQuotaAloneDiff {
		return
	}
	e.lastQuotaAloneDiff = diff
	if diff == "" {
		ctrl.LoggerFrom(ctx).Info("Upgrade note (transitional): the physical GPU limiter no longer changes "+
			"any target compared with the quota limiters alone",
			"limiters", e.Config.EffectiveLimiterSummary())
		return
	}
	ctrl.LoggerFrom(ctx).Info("Upgrade note (transitional): this policy declares a physical GPU limiter and "+
		"quotas, and both now bound scaling. Until this release the physical limiter was not built and the "+
		"quotas alone applied. Targets that differ, as namespace/variant: quota-alone -> composed",
		"limiters", e.Config.EffectiveLimiterSummary(), "differences", diff)
}

// quotaConstraints returns the constraints of the quota providers (those fed the
// managed usage basis), matched to providers by name.
func quotaConstraints(providers []allocation.ConstraintProvider, constraints []*allocation.ResourceConstraints) []*allocation.ResourceConstraints {
	quota := map[string]bool{}
	for _, cp := range providers {
		if allocation.UsageBasisOf(cp) == allocation.ManagedUsage {
			quota[cp.Name()] = true
		}
	}
	var out []*allocation.ResourceConstraints
	for _, c := range constraints {
		if c != nil && quota[c.ProviderName] {
			out = append(out, c)
		}
	}
	return out
}

// quotaAloneDiff lists, sorted, every variant whose target differs between the
// two passes as "namespace/variant: alone -> composed", or "" when none does. A
// variant only one pass decided is listed with "-" on the missing side, since
// reading the absence as 0 replicas would mislead.
func quotaAloneDiff(composed, alone []domain.VariantDecision) string {
	targets := func(ds []domain.VariantDecision) map[string]int {
		m := make(map[string]int, len(ds))
		for _, d := range ds {
			m[utils.GetNamespacedKey(d.Namespace, d.VariantName)] = d.TargetReplicas
		}
		return m
	}
	c, a := targets(composed), targets(alone)
	keys := map[string]bool{}
	for k := range c {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	var lines []string
	for k := range keys {
		ct, inC := c[k]
		at, inA := a[k]
		if inC && inA && ct == at {
			continue
		}
		side := func(v int, ok bool) string {
			if !ok {
				return "-"
			}
			return strconv.Itoa(v)
		}
		lines = append(lines, fmt.Sprintf("%s: %s -> %s", k, side(at, inA), side(ct, inC)))
	}
	sort.Strings(lines)
	return strings.Join(lines, ", ")
}
