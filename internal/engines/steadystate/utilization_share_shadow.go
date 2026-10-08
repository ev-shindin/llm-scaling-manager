package steadystate

import (
	"context"
	"fmt"
	"maps"
	"math"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-logr/logr"
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
	// lastUnmatched are the disabled namespaces the last cycle found no
	// namespace quota for, comma-joined: reported once per change.
	lastUnmatched string
	// noBoundSince is when the cycles began to see no constraints at all;
	// zero while they see some. goneSince is when each ledger's group began
	// to be missing from cycles that saw constraints. See shareAbsenceGrace.
	noBoundSince time.Time
	goneSince    map[string]time.Time
	// swept is set once the marks a stopped optimizer left are removed, and
	// cleared while it acts. See sweepShareMarks.
	swept bool
	// floorHeavy are the groups whose floors hold more than half their
	// budget above need: reported once per change (reportFloorHeavy).
	floorHeavy map[string]bool
	// lastActionable is, per group, the actionable roles last logged.
	lastActionable map[string]string

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
	// blockedModels are the models the last active cycle published blocked
	// reasons for (namespace/model), so a model that stops being planned has
	// its reasons cleared.
	blockedModels map[string]bool
	// now is the clock; nil is time.Now. Tests set it.
	now func() time.Time
	// nodes is the per-node GPU picture; nil is decision.DefaultNodeGPUs,
	// which the usage refresher publishes. Tests set it.
	nodes *decision.NodeGPUStore
}

// resetActuation forgets every ledger. A transfer's donor marks stay on its
// pods; when the optimizer is activated again, the rebuild reads them back and
// removes those that are stale.
func (st *utilizationShareState) resetActuation() {
	st.ledgers, st.desired, st.quietUntil, st.divergedSince, st.goneSince = nil, nil, nil, nil, nil
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
	scaleTargets map[string]scaletarget.ScaleTargetAccessor) (overrides map[string]utilizationShareOverride) {
	logger := ctrl.LoggerFrom(ctx).WithName("utilization-share")
	st := &e.utilizationShare
	// The optimizer must never cost the cycle it rides on: a bug here loses
	// this cycle's report and leaves today's decisions, not the controller.
	defer func() {
		if r := recover(); r != nil {
			logger.Error(fmt.Errorf("panic: %v\n%s", r, debug.Stack()),
				"Utilization share: evaluation failed; skipping it this cycle")
			metrics.PublishUtilizationShare(nil)
			overrides = nil
		}
	}()

	now := st.clock()
	// What the active groups have promised, published on every exit: empty
	// when nothing acts, so wakes and the warm pool stop withholding at once.
	promised := map[string]map[string]int{}
	defer func() { decision.PublishSharePromised(promised, now) }()
	// Blocked reasons are published only while the optimizer acts: in shadow
	// mode "blocked by the optimizer" would be false. Every exit clears what
	// this cycle did not set.
	blocked := map[string][]string{}
	defer func() { st.publishBlocked(blocked) }()
	// What a wake may claim, republished every pass: empty when nothing acts.
	claimable := map[string][]decision.ShareClaimable{}
	defer func() { decision.DefaultShareClaims.Publish(claimable, now) }()

	us, selected, err := e.Config.UtilizationShare()
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	switch {
	case err != nil:
		metrics.SetUtilizationShareMode(constants.UtilizationShareModeInvalid)
	case !selected:
		metrics.SetUtilizationShareMode(constants.UtilizationShareModeOff)
	case us.Shadow:
		metrics.SetUtilizationShareMode(constants.UtilizationShareModeShadow)
	default:
		metrics.SetUtilizationShareMode(constants.UtilizationShareModeActive)
	}
	if errText != st.lastConfigErr {
		if err != nil {
			logger.Error(err, "Utilization share: invalid optimizer block; keeping today's optimizer. The limiters are unaffected")
		} else if st.lastConfigErr != "" {
			logger.Info("Utilization share: the optimizer block is valid again")
		}
		st.lastConfigErr = errText
	}
	if ignored := strings.Join(e.Config.IgnoredOptimizerNamespaces(), ","); ignored != st.lastIgnored {
		if ignored != "" {
			logger.Info("Utilization share: ignoring optimizer blocks in namespace-local scaling-policy maps; "+
				"the optimizer is configured only beside limiters:", "namespaces", ignored)
		}
		st.lastIgnored = ignored
	}
	if !selected {
		// Switched off: our marks go with the ledgers.
		metrics.PublishUtilizationShare(nil)
		e.dropShareActuation(ctx, logger)
		return nil
	}
	if len(constraints) == 0 {
		// No bound this cycle -- possibly a failed read. For a grace the
		// ledgers stay and the variants their transfers move keep their
		// targets: a fill has no mark to be restored from. Past it the bound
		// is gone, not unread, and there is nothing to share: as when the
		// optimizer is switched off, its marks go with its ledgers.
		metrics.PublishUtilizationShare(nil)
		if st.noBoundSince.IsZero() {
			st.noBoundSince = now
		}
		if !us.Shadow && now.Sub(st.noBoundSince) < shareAbsenceGrace {
			return st.holdInFlight(nil, "utilization share: no bound read this cycle; transfer in flight")
		}
		e.dropShareActuation(ctx, logger)
		return nil
	}
	st.noBoundSince = time.Time{}
	if us.Shadow {
		e.dropShareActuation(ctx, logger)
	} else {
		st.swept = false
	}
	st.reportUnmatchedNamespaces(logger, us, constraints)
	seen := map[string]bool{}

	weightErrs := map[string]string{}
	groups := allocation.BuildShareGroups(requests, constraints, allocation.ShareGroupOptions{
		ClusterIsQuota:   e.Config.EffectiveLimiterMode() == config.LimiterTypeQuota,
		PhysicalGroups:   us.PhysicalGroups,
		ReserveGPUs:      us.ReserveGPUs,
		PoolUnheld:       decision.DefaultWarmPoolUnheld.Latest(decision.WarmPoolUnheldMaxAge, now),
		NamespaceEnabled: us.EnabledForNamespace,
		Weight: func(req allocation.ModelScalingRequest, _ bool) float64 {
			w, err := us.Weight(req.WeightClass, req.Weight)
			if err != nil {
				weightErrs[utils.GetNamespacedKey(req.Namespace, req.ModelID)] = err.Error()
			}
			return w
		},
		// A namespace with its own scaling-policy map sets every input its
		// models are sized by -- weight, scale-up threshold, KV threshold -- and
		// that map may be the tenant's. In the cluster group those inputs would
		// be weighed against every other tenant's, so such a model is not
		// planned there; today's optimizer keeps it. In its own namespace's
		// quota group, a budget the tenant owns whole, it is planned (§8.2).
		Exclude: func(req allocation.ModelScalingRequest, clusterScope bool) string {
			if clusterScope && e.Config.NamespaceHasLocalPolicy(req.Namespace) {
				return "namespace has its own scaling-policy map; not planned in the cluster-wide group"
			}
			if clusterScope && !us.InClusterGroup(req.Namespace) {
				return "namespace not in utilizationShare.clusterNamespaces"
			}
			return ""
		},
	})
	for model, msg := range weightErrs {
		if st.lastWeightErrs[model] != msg {
			logger.Info("Utilization share: invalid weight on a model's scaling-policy entry", "model", model, "error", msg)
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
		floorExcess := 0.0
		roleIdx := map[string]int{}
		roleByKey := map[string]allocation.ShareRole{}
		for _, r := range g.Roles {
			roleByKey[r.Key] = r
		}
		for _, v := range ev.Roles {
			roleIdx[v.Key] = len(pg.Roles)
			r := roleByKey[v.Key]
			actual := math.NaN()
			if r.Need > 0 && v.Committed > 0 {
				actual = r.Need * g.Thresholds[v.Key] / float64(v.Committed)
			}
			o := g.Origins[v.Key]
			excess := math.Max(0, float64(r.Floor)-r.Need)
			pg.Roles = append(pg.Roles, metrics.UtilizationShareRole{
				Namespace:   o.Namespace,
				ModelName:   o.ModelID,
				Role:        o.Role,
				Headroom:    v.Headroom,
				TargetGPUs:  v.Continuous,
				Actionable:  v.Actionable,
				Actual:      actual,
				FloorExcess: excess,
			})
			floorExcess += excess
			table = append(table, fmt.Sprintf("%s held=%d target=%.2f integer=%d headroom=%s band=%t",
				v.Key, v.Committed, v.Continuous, v.Integer, formatHeadroom(v.Headroom), v.InBand))
			if v.Actionable {
				actionable = append(actionable, v.Key)
			}
		}
		st.reportFloorHeavy(logger, shareGroupKey(g), floorExcess, g.Budget)
		mode := "Utilization share: would rebalance (shadow)"
		if !us.Shadow {
			mode = "Utilization share: rebalancing"
			seen[shareGroupKey(g)] = true
			if overrides == nil {
				overrides = map[string]utilizationShareOverride{}
			}
			act := e.actuateUtilizationShare(ctx, logger, us, g, ev, scaleTargets, now)
			maps.Copy(overrides, act.overrides)
			pg.Active = true
			maps.Copy(blocked, act.blocked)
			if len(act.claimable) > 0 {
				claimable[decision.ShareGroupKey(g.Scope, g.AcceleratorType)] = act.claimable
			}
			for reason, n := range act.withheld {
				metrics.CountUtilizationShareWithheld(g.AcceleratorType, scope, reason, n)
			}
			pg.PromisedGPUs = float64(act.promised)
			pg.ReserveDebtGPUs = float64(act.reserveDebt)
			pg.Releasing, pg.Filling = act.releasing, act.filling
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
			"accelerator", g.AcceleratorType, "scope", scope, "budget", g.Budget, "poolCarve", g.PoolCarve,
			"spare", fmt.Sprintf("%.2f", ev.Spare), "wholeReplicaCoverage", ev.WholeReplicaCoverage,
			"replicasToMove", ev.ReplicasToMove, "actionable", actionable, "frozen", g.Frozen,
		}
		// Info when the roles a move would fix change, so a settled fleet -- or
		// one whose short roles nothing can fund -- stays quiet.
		if joined := strings.Join(actionable, ","); joined != st.lastActionable[shareGroupKey(g)] {
			if st.lastActionable == nil {
				st.lastActionable = map[string]string{}
			}
			st.lastActionable[shareGroupKey(g)] = joined
			if joined != "" {
				logger.Info(mode, kv...)
			}
		}
		logger.V(logging.DEBUG).Info("Utilization share: evaluation", append(kv, "roles", table)...)
	}
	metrics.PublishUtilizationShare(published)
	e.dropVanishedGroups(ctx, logger, seen, now)
	return st.holdInFlight(overrides, "utilization share: transfer in flight")
}

// dropVanishedGroups drops the ledger of a group that has gone (its last model
// left, its namespace disabled) and its marks: its variants return to today's
// optimizer, with pods that carry no deletion cost of ours. A group missing for
// less than shareAbsenceGrace -- a failed collection -- keeps both, and the
// variants its transfers move keep their targets. seen are the groups this
// cycle acted on.
func (e *Engine) dropVanishedGroups(ctx context.Context, logger logr.Logger, seen map[string]bool, now time.Time) {
	st := &e.utilizationShare
	if st.goneSince == nil {
		st.goneSince = map[string]time.Time{}
	}
	for k, l := range st.ledgers {
		if seen[k] {
			delete(st.goneSince, k)
			continue
		}
		if _, ok := st.goneSince[k]; !ok {
			st.goneSince[k] = now
		}
		if now.Sub(st.goneSince[k]) < shareAbsenceGrace {
			continue
		}
		failed := 0
		for _, t := range l.Transfers() {
			failed += e.unmarkDonorPods(ctx, logger, t)
		}
		if failed > 0 {
			continue // the ledger still names the marks: retried next cycle
		}
		delete(st.ledgers, k)
		delete(st.quietUntil, k)
		delete(st.goneSince, k)
	}
}

// shareAbsenceGrace is how long cycles that read no constraint, or miss a
// group, keep the transfers in flight: long enough to ride out a failed read,
// short enough that a bound or group really removed releases the variants
// promptly.
const shareAbsenceGrace = 2 * time.Minute

// holdInFlight adds to overrides the targets of the variants a live transfer
// still moves and this cycle did not plan, and forgets every other target.
// Such a variant keeps its target while its model is out of the group for a
// cycle -- frozen, or not collected -- or no bound was read: dropping it would
// lose the transfer's decrement, and the donor would never shrink.
func (st *utilizationShareState) holdInFlight(overrides map[string]utilizationShareOverride,
	why string) map[string]utilizationShareOverride {
	touched := map[string]bool{}
	for _, l := range st.ledgers {
		for _, t := range l.Transfers() {
			if k := transferVariantKey(t.Donor, t.DonorVariant); k != "" {
				touched[k] = true
			}
			if k := transferVariantKey(t.Receiver, t.ReceiverVariant); k != "" {
				touched[k] = true
			}
		}
	}
	for k, d := range st.desired {
		_, planned := overrides[k]
		switch {
		case planned:
		case touched[k]:
			if overrides == nil {
				overrides = map[string]utilizationShareOverride{}
			}
			overrides[k] = utilizationShareOverride{Target: d, Why: why}
		default:
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
		{Param: constants.UtilizationShareParamWindow, Source: derived("window"), Seconds: tm.Window.Seconds()},
		{Param: constants.UtilizationShareParamReleaseTimeout, Source: derived("window", "polling", "grace"),
			Seconds: tm.ReleaseTimeout.Seconds()},
		{Param: constants.UtilizationShareParamFillTimeout, Source: derived("polling"), Seconds: tm.FillTimeout.Seconds()},
		{Param: constants.UtilizationShareParamReversalHold, Source: derived("release", "window", "polling", "grace"),
			Seconds: tm.ReversalHold.Seconds()},
		{Param: constants.UtilizationShareParamSwingWindow, Source: derived("release", "window", "polling", "grace"),
			Seconds: tm.SwingWindow.Seconds()},
	}
}

// publishBlocked sets this cycle's utilization-share blocked reasons, and
// clears them for models that were published last cycle and are not now.
func (st *utilizationShareState) publishBlocked(blocked map[string][]string) {
	owned := constants.ScalingBlockedReasonsUtilizationShare
	for key, reasons := range blocked {
		ns, model, _ := strings.Cut(key, "/")
		metrics.SetModelScalingBlockedReasons(ns, model, owned, reasons)
	}
	for key := range st.blockedModels {
		if _, ok := blocked[key]; !ok {
			ns, model, _ := strings.Cut(key, "/")
			metrics.SetModelScalingBlockedReasons(ns, model, owned, nil)
		}
	}
	st.blockedModels = map[string]bool{}
	for key := range blocked {
		st.blockedModels[key] = true
	}
}

// transferVariantKey is the namespace/variant key of a transfer's role and
// variant (role keys are namespace/model/role), or "" for none.
func transferVariantKey(role, variant string) string {
	ns, _, ok := strings.Cut(role, "/")
	if !ok || variant == "" {
		return ""
	}
	return utils.GetNamespacedKey(ns, variant)
}

// reportUnmatchedNamespaces logs, once per change, the namespaces the
// optimizer's namespaces block disables but no namespace quota names: a
// misspelled name, or a namespace whose quota was removed, disables nothing.
func (st *utilizationShareState) reportUnmatchedNamespaces(logger logr.Logger, us config.UtilizationShare,
	constraints []*allocation.ResourceConstraints) {
	var unmatched []string
	for _, ns := range us.DisabledNamespaces() {
		if _, nsScoped := allocation.GPUBudgets(constraints, ns); !nsScoped {
			unmatched = append(unmatched, ns)
		}
	}
	joined := strings.Join(unmatched, ",")
	if joined == st.lastUnmatched {
		return
	}
	st.lastUnmatched = joined
	if joined != "" {
		logger.Info("Utilization share: the namespaces block disables namespaces no namespace quota names; it has no effect on them",
			"namespaces", joined)
	}
}

// reportFloorHeavy logs, once per change, a group whose floors hold more than
// half its budget above their models' need: the optimizer can share only what
// floors leave, and the floor_excess series shows whose floors they are
// (proposal section 5.5).
func (st *utilizationShareState) reportFloorHeavy(logger logr.Logger, group string, excess float64, budget int) {
	heavy := budget > 0 && excess > float64(budget)/2
	if st.floorHeavy[group] == heavy {
		return
	}
	if st.floorHeavy == nil {
		st.floorHeavy = map[string]bool{}
	}
	st.floorHeavy[group] = heavy
	if heavy {
		logger.Info("Utilization share: floors hold more than half the group's budget above their models' need; "+
			"see wva_utilization_share_floor_excess_gpus for whose", "group", group,
			"floorExcessGPUs", excess, "budget", budget)
	}
}

// clock is the optimizer's time: now, or the test's clock.
func (st *utilizationShareState) clock() time.Time {
	if st.now != nil {
		return st.now()
	}
	return time.Now()
}
