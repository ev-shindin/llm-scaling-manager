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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
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
	// divergedSince is when a planned variant's running count first differed
	// from its target with no transfer in flight; see reanchorShareTargets.
	divergedSince map[string]time.Time
	// now is the clock; nil is time.Now. Tests set it.
	now func() time.Time
}

// resetActuation forgets every ledger. A transfer's donor marks stay on its
// pods; when the optimizer is activated again, the rebuild reads them back and
// removes those that are stale.
func (st *utilizationShareState) resetActuation() {
	st.ledgers, st.desired, st.quietUntil, st.divergedSince = nil, nil, nil, nil
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

	now := time.Now()
	if st.now != nil {
		now = st.now()
	}
	// What the active groups have promised, published on every exit: empty
	// when nothing acts, so wakes and the warm pool stop withholding at once.
	promised := map[string]map[string]int{}
	defer func() { decision.PublishSharePromised(promised, now) }()

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
		roleIdx := map[string]int{}
		for _, v := range ev.Roles {
			roleIdx[v.Key] = len(pg.Roles)
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
		mode := "Shadow: utilization share would rebalance"
		if !us.Shadow {
			mode = "Utilization share: rebalancing"
			seen[shareGroupKey(g)] = true
			if overrides == nil {
				overrides = map[string]utilizationShareOverride{}
			}
			act := e.actuateUtilizationShare(ctx, logger, us, g, ev, scaleTargets, now)
			maps.Copy(overrides, act.overrides)
			pg.Active = true
			pg.PromisedGPUs = float64(act.promised)
			if act.promised > 0 {
				if promised[g.Scope] == nil {
					promised[g.Scope] = map[string]int{}
				}
				promised[g.Scope][g.AcceleratorType] += act.promised
			}
			pg.Timings = shareTimingSeries(act.timings, act.sources)
			for _, k := range act.swinging {
				if i, ok := roleIdx[k]; ok {
					pg.Roles[i].Swinging = true
				}
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
			delete(st.divergedSince, k)
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

// shareTimingSeries is a group's derived timings as published series: each
// parameter in seconds, with where its inputs came from (section 8.4) --
// measured when the group's own releases set it, else the ScaledObject or pod
// template when any input was read there, else the cluster defaults.
func shareTimingSeries(tm allocation.ShareTimings, src allocation.ShareTimingSource) []metrics.UtilizationShareTiming {
	derived := func(inputs ...string) string {
		for _, in := range inputs {
			if src[in] == "measured" {
				return "measured"
			}
		}
		for _, in := range inputs {
			if src[in] == "scaledobject" || src[in] == "pod" {
				return src[in]
			}
		}
		return "default"
	}
	return []metrics.UtilizationShareTiming{
		{Param: "window", Source: derived("window"), Seconds: tm.Window.Seconds()},
		{Param: "release_timeout", Source: derived("window", "polling", "grace"), Seconds: tm.ReleaseTimeout.Seconds()},
		{Param: "fill_timeout", Source: derived("polling"), Seconds: tm.FillTimeout.Seconds()},
		{Param: "reversal_hold", Source: derived("release", "window", "polling", "grace"), Seconds: tm.ReversalHold.Seconds()},
		{Param: "swing_window", Source: derived("release", "window", "polling", "grace"), Seconds: tm.SwingWindow.Seconds()},
	}
}
