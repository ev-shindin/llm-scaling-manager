package steadystate

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// utilizationShareState is what the engine remembers between cycles about the
// utilization-share optimizer, so a standing condition is reported once per
// change rather than once per cycle.
type utilizationShareState struct {
	lastConfigErr  string
	lastIgnored    string
	lastWeightErrs map[string]string

	// Actuation state (stage 2), dropped whenever the optimizer is not active.
	// ledgers are per group (shareGroupKey); desired is each planned variant's
	// target by namespace/variant; quietUntil is when a group's ledger may
	// plan again after it was (re)built.
	ledgers    map[string]*allocation.ShareLedger
	desired    map[string]int
	quietUntil map[string]time.Time
	// now is the clock; nil is time.Now. Tests set it.
	now func() time.Time
}

// resetActuation forgets every ledger. A transfer's donor marks stay on its
// pods; when the optimizer is activated again, the rebuild reads them back and
// removes those that are stale.
func (st *utilizationShareState) resetActuation() {
	st.ledgers, st.desired, st.quietUntil = nil, nil, nil
}

// evaluateUtilizationShare runs the utilization-share optimizer
// (docs/proposals/utilization-share-optimizer.md, section 13): for every
// enabled group it computes the targets, the tolerance band and the roles a
// move could fix, and publishes them as metrics and one log line.
//
// In shadow mode it actuates nothing and returns nil. With shadow: false it
// runs each group's ledger (stage 2) and returns the target of every variant
// it plans, keyed by namespace/variant, for the caller to apply over today's
// decisions. A model it does not plan -- frozen, multi-accelerator, or in a
// group that is not enabled -- keeps today's decision.
//
// constraints are the GPU constraints this cycle computed. With none there is
// no finite budget to share, and nothing is evaluated. scaleTargets are this
// cycle's scale targets by namespace/model, then variant.
func (e *Engine) evaluateUtilizationShare(ctx context.Context, requests []allocation.ModelScalingRequest,
	constraints []*allocation.ResourceConstraints,
	scaleTargets map[string]map[string]scaletarget.ScaleTargetAccessor) (overrides map[string]utilizationShareOverride) {
	logger := ctrl.LoggerFrom(ctx).WithName("utilization-share")
	st := &e.utilizationShare
	// The optimizer must never cost the cycle it rides on: a bug here loses
	// this cycle's report and leaves today's decisions, not the controller.
	defer func() {
		if r := recover(); r != nil {
			logger.Error(fmt.Errorf("panic: %v", r), "Utilization-share evaluation failed; skipping it this cycle")
			metrics.PublishUtilizationShare(nil)
			overrides = nil
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
		st.resetActuation()
		return nil
	}
	if us.Shadow {
		st.resetActuation()
	}
	now := time.Now()
	if st.now != nil {
		now = st.now()
	}
	seen := map[string]bool{}

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

		mode := "Shadow: utilization share would rebalance"
		if !us.Shadow {
			mode = "Utilization share: rebalancing"
			seen[shareGroupKey(g)] = true
			if overrides == nil {
				overrides = map[string]utilizationShareOverride{}
			}
			maps.Copy(overrides, e.actuateUtilizationShare(ctx, logger, us, g, ev, scaleTargets, now))
		}

		kv := []any{
			"accelerator", g.AcceleratorType, "scope", scope, "budget", g.Budget,
			"spare", fmt.Sprintf("%.2f", ev.Spare), "wholeReplicaCoverage", ev.WholeReplicaCoverage,
			"replicasToMove", ev.ReplicasToMove, "actionable", actionable, "frozen", g.Frozen,
		}
		// Info only when a move would be planned, so a settled fleet stays quiet.
		if len(actionable) > 0 {
			logger.Info(mode, kv...)
		}
		logger.V(logging.DEBUG).Info("Shadow: utilization share evaluation", append(kv, "roles", table)...)
	}
	metrics.PublishUtilizationShare(published)
	// A group that has gone (its last model left, its namespace disabled)
	// takes its ledger with it; its variants return to today's optimizer.
	for k := range st.ledgers {
		if !seen[k] {
			delete(st.ledgers, k)
			delete(st.quietUntil, k)
		}
	}
	for k := range st.desired {
		if _, ok := overrides[k]; !ok {
			delete(st.desired, k)
		}
	}
	return overrides
}

func formatHeadroom(x float64) string {
	if math.IsNaN(x) {
		return "n/a"
	}
	return fmt.Sprintf("%+.0f%%", 100*x)
}
