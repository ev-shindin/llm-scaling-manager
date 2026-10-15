package steadystate

import (
	"context"
	"sort"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// reportQuotaAloneAllocation is the transitional upgrade log for policies that
// declare both a physical limiter and quota entries.
//
// TODO(#164): TRANSITIONAL -- remove this file, quotaAloneReportEvery, the
// two quotaAlone* Engine fields, its call in optimize and
// GreedyByScoreOptimizer.ShadowOptimize one release after the first release
// that composes limiters (the release after v1.0.0). The upgrade notes promise
// that removal ("Limiters", docs/reference/upgrade-and-uninstall.md).
//
// Until that release such a policy was bounded by its quotas alone -- the
// physical entry was silently never built. It is now bounded by the GPUs that
// exist as well, which can be tighter: the physical view counts every
// GPU-holding pod, training included. So that the change is visible before an
// operator meets it as a model that stopped growing, each cycle runs the
// optimizer a second time, as a shadow that publishes nothing, over the quota
// constraints alone, and logs the variants whose target differs from the
// composed one. The pass runs once per quotaAloneReportEvery eligible cycles,
// not every cycle, and logs only when the SET of differing variants changes:
// the targets themselves move with load almost every cycle, and logging them
// made the "on change" log fire nearly every cycle under load.
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
	if e.Config == nil {
		return
	}
	if types := e.Config.EffectiveLimiterTypes(); !types.HasPhysical() || !types.HasQuota() {
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
	run := e.quotaAloneCycles%quotaAloneReportEvery == 0
	e.quotaAloneCycles++
	if !run {
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
		"quotas alone applied. Variants whose target differs (namespace/variant)",
		"limiters", e.Config.EffectiveLimiterSummary(), "differences", diff)
}

// quotaAloneReportEvery is how many eligible cycles pass between two shadow
// passes of reportQuotaAloneAllocation. The pass is a second optimizer run, and
// the note it feeds is an upgrade hint, not a signal anyone acts on within a
// cycle. TRANSITIONAL: removed with reportQuotaAloneAllocation.
const quotaAloneReportEvery = 10

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

// quotaAloneDiff lists, sorted, every variant ("namespace/variant") whose
// target differs between the two passes, or "" when none does. A variant only
// one pass decided differs. The targets are left out: they change with load,
// and the set of variants the physical limiter bounds is what the note is for.
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
		lines = append(lines, k)
	}
	sort.Strings(lines)
	return strings.Join(lines, ", ")
}
