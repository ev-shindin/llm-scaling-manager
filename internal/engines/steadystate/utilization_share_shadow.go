package steadystate

import (
	"context"
	"fmt"
	"math"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// utilizationShareState is what the engine remembers between cycles about the
// utilization-share optimizer, so a standing condition is reported once per
// change rather than once per cycle.
type utilizationShareState struct {
	lastConfigErr   string
	lastIgnored     string
	warnedActuation bool
	lastWeightErrs  map[string]string
}

// evaluateUtilizationShare runs stage 1 of the utilization-share optimizer
// (docs/proposals/utilization-share-optimizer.md, section 13): for every
// enabled group it computes the targets, the tolerance band and the roles a
// move could fix, and publishes them as metrics and one log line. It actuates
// nothing -- the decisions this cycle come from today's optimizer either way.
//
// constraints are the GPU constraints this cycle computed. With none there is
// no finite budget to share, and nothing is evaluated.
func (e *Engine) evaluateUtilizationShare(ctx context.Context, requests []allocation.ModelScalingRequest,
	constraints []*allocation.ResourceConstraints) {
	logger := ctrl.LoggerFrom(ctx).WithName("utilization-share")
	st := &e.utilizationShare
	// Shadow evaluation must never cost the cycle it rides on: the decisions
	// are already made, and a bug here should lose this cycle's report, not the
	// controller.
	defer func() {
		if r := recover(); r != nil {
			logger.Error(fmt.Errorf("panic: %v", r), "Utilization-share evaluation failed; skipping it this cycle")
			metrics.PublishUtilizationShare(nil)
		}
	}()

	us, selected, err := e.Config.UtilizationShare()
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if errText != st.lastConfigErr {
		if err != nil {
			logger.Error(err, "Invalid optimizer block; keeping today's optimizer. The limiters are unaffected")
		} else if st.lastConfigErr != "" {
			logger.Info("Optimizer block is valid again")
		}
		st.lastConfigErr = errText
	}
	if ignored := strings.Join(e.Config.IgnoredOptimizerNamespaces(), ","); ignored != st.lastIgnored {
		if ignored != "" {
			logger.Info("WARNING: ignoring optimizer blocks in namespace-local scaling-policy maps; "+
				"the optimizer is configured only beside limiters:", "namespaces", ignored)
		}
		st.lastIgnored = ignored
	}
	if !selected || len(constraints) == 0 {
		metrics.PublishUtilizationShare(nil)
		return
	}
	if !us.Shadow && !st.warnedActuation {
		logger.Info("WARNING: utilizationShare is selected with shadow: false, but actuation is not built yet; " +
			"evaluating in shadow mode only")
		st.warnedActuation = true
	}

	weightErrs := map[string]string{}
	groups := allocation.BuildShareGroups(requests, constraints, allocation.ShareGroupOptions{
		ClusterIsQuota:   e.Config.EffectiveLimiterMode() == config.LimiterTypeQuota,
		PhysicalGroups:   us.PhysicalGroups,
		NamespaceEnabled: us.EnabledForNamespace,
		Weight: func(req allocation.ModelScalingRequest, clusterScope bool) float64 {
			model := utils.GetNamespacedKey(req.Namespace, req.ModelID)
			stated := req.WeightClass != "" || !req.Weight.IsZero()
			if clusterScope && stated && e.Config.NamespaceHasLocalPolicy(req.Namespace) {
				// A weight from a namespace's own map is the tenant's word about
				// itself; in the cluster group it would be weighed against every
				// other tenant's. Only admin-owned weights count there (§8.2).
				weightErrs[model] = "weight set in the namespace's own scaling-policy map is " +
					"ignored in the cluster-wide group; using the default class"
				return us.Classes[us.DefaultClass]
			}
			w, err := us.Weight(req.WeightClass, req.Weight)
			if err != nil {
				weightErrs[model] = err.Error()
			}
			return w
		},
	})
	for model, msg := range weightErrs {
		if st.lastWeightErrs[model] != msg {
			logger.Info("WARNING: invalid weight on a model's scaling-policy entry", "model", model, "error", msg)
		}
	}
	st.lastWeightErrs = weightErrs

	published := make([]metrics.UtilizationShareGroup, 0, len(groups))
	for _, g := range groups {
		ev := allocation.EvaluateShare(g.Roles, g.Committed, g.Thresholds, g.Budget, us.Tolerance)
		scope := g.Scope
		if scope == "" {
			scope = constants.UtilizationShareClusterScope
		}
		pg := metrics.UtilizationShareGroup{
			AcceleratorType: g.AcceleratorType,
			Scope:           scope,
			SpareGPUs:       ev.Spare,
			ReplicasToMove:  ev.ReplicasToMove,
		}
		var table []string
		var actionable []string
		for _, v := range ev.Roles {
			o := g.Origins[v.Key]
			pg.Roles = append(pg.Roles, metrics.UtilizationShareRole{
				Namespace:  o.Namespace,
				ModelName:  o.ModelID,
				Role:       o.Role,
				Headroom:   v.Headroom,
				TargetGPUs: v.Continuous,
				Actionable: v.Actionable,
			})
			table = append(table, fmt.Sprintf("%s held=%d target=%.2f integer=%d headroom=%s band=%t",
				v.Key, v.Committed, v.Continuous, v.Integer, formatHeadroom(v.Headroom), v.InBand))
			if v.Actionable {
				actionable = append(actionable, v.Key)
			}
		}
		published = append(published, pg)

		kv := []any{
			"accelerator", g.AcceleratorType, "scope", scope, "budget", g.Budget,
			"spare", fmt.Sprintf("%.2f", ev.Spare), "wholeReplicaCoverage", ev.WholeReplicaCoverage,
			"replicasToMove", ev.ReplicasToMove, "actionable", actionable, "frozen", g.Frozen,
		}
		// Info only when a move would be planned, so a settled fleet stays quiet.
		if len(actionable) > 0 {
			logger.Info("Shadow: utilization share would rebalance", kv...)
		}
		logger.V(logging.DEBUG).Info("Shadow: utilization share evaluation", append(kv, "roles", table)...)
	}
	metrics.PublishUtilizationShare(published)
}

func formatHeadroom(x float64) string {
	if math.IsNaN(x) {
		return "n/a"
	}
	return fmt.Sprintf("%+.0f%%", 100*x)
}
