package metrics

import (
	"fmt"
	"math"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var (
	utilizationShareHeadroom       *prometheus.GaugeVec
	utilizationShareTargetGPUs     *prometheus.GaugeVec
	utilizationShareActionable     *prometheus.GaugeVec
	utilizationShareSpareGPUs      *prometheus.GaugeVec
	utilizationShareReplicasToMove *prometheus.GaugeVec
)

// registerUtilizationShareMetrics creates and registers the utilization-share
// optimizer's gauges. Called from InitMetrics, after controllerInstance is set.
func registerUtilizationShareMetrics(registry prometheus.Registerer) error {
	roleLabels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelRole}
	groupLabels := []string{constants.LabelAcceleratorType, constants.LabelScope}
	if controllerInstance != "" {
		roleLabels = append(roleLabels, constants.LabelControllerInstance)
		groupLabels = append(groupLabels, constants.LabelControllerInstance)
	}
	gauge := func(name, help string, labels []string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	}
	utilizationShareHeadroom = gauge(constants.WVAUtilizationShareHeadroom,
		"Utilization-share optimizer: the traffic spike a role absorbs before it must scale, as a "+
			"fraction of its need. Negative when the role is short. Absent for a role with no demand.",
		roleLabels)
	utilizationShareTargetGPUs = gauge(constants.WVAUtilizationShareTargetGPUs,
		"Utilization-share optimizer: the continuous GPU target a role's tolerance band is judged against.",
		roleLabels)
	utilizationShareActionable = gauge(constants.WVAUtilizationShareActionable,
		"Utilization-share optimizer: 1 when a role is out of band and off its whole-replica target, "+
			"so a move could fix it.",
		roleLabels)
	utilizationShareSpareGPUs = gauge(constants.WVAUtilizationShareSpareGPUs,
		"Utilization-share optimizer: a group's budget minus every role's claim (need raised to its "+
			"floor). Negative when the quota is short.",
		groupLabels)
	utilizationShareReplicasToMove = gauge(constants.WVAUtilizationShareReplicasToMove,
		"Utilization-share optimizer: replicas the whole-replica target would move. In shadow mode, "+
			"what would be planned.",
		groupLabels)
	for _, g := range []*prometheus.GaugeVec{
		utilizationShareHeadroom, utilizationShareTargetGPUs, utilizationShareActionable,
		utilizationShareSpareGPUs, utilizationShareReplicasToMove,
	} {
		if err := registry.Register(g); err != nil {
			return fmt.Errorf("failed to register utilization-share metric: %w", err)
		}
	}
	return nil
}

// UtilizationShareRole is one role's published evaluation.
type UtilizationShareRole struct {
	Namespace, ModelName, Role string
	// Headroom is NaN for a role with no demand, which publishes no headroom.
	Headroom   float64
	TargetGPUs float64
	Actionable bool
}

// UtilizationShareGroup is one group's published evaluation.
type UtilizationShareGroup struct {
	AcceleratorType string
	// Scope is a namespace, or constants.UtilizationShareClusterScope.
	Scope          string
	SpareGPUs      float64
	ReplicasToMove int
	Roles          []UtilizationShareRole
}

// PublishUtilizationShare replaces every utilization-share series with this
// cycle's evaluation. Calling it with no groups clears them all, which is what
// a cycle that does not run the optimizer must do: a stale headroom reading
// outliving the optimizer would describe a fleet nothing is sizing.
func PublishUtilizationShare(groups []UtilizationShareGroup) {
	if utilizationShareHeadroom == nil {
		return
	}
	for _, g := range []*prometheus.GaugeVec{
		utilizationShareHeadroom, utilizationShareTargetGPUs, utilizationShareActionable,
		utilizationShareSpareGPUs, utilizationShareReplicasToMove,
	} {
		g.Reset()
	}
	for _, grp := range groups {
		gl := prometheus.Labels{constants.LabelAcceleratorType: grp.AcceleratorType, constants.LabelScope: grp.Scope}
		if controllerInstance != "" {
			gl[constants.LabelControllerInstance] = controllerInstance
		}
		utilizationShareSpareGPUs.With(gl).Set(grp.SpareGPUs)
		utilizationShareReplicasToMove.With(gl).Set(float64(grp.ReplicasToMove))
		for _, r := range grp.Roles {
			rl := prometheus.Labels{
				constants.LabelNamespace: r.Namespace,
				constants.LabelModelName: r.ModelName,
				constants.LabelRole:      r.Role,
			}
			if controllerInstance != "" {
				rl[constants.LabelControllerInstance] = controllerInstance
			}
			if !math.IsNaN(r.Headroom) {
				utilizationShareHeadroom.With(rl).Set(r.Headroom)
			}
			utilizationShareTargetGPUs.With(rl).Set(r.TargetGPUs)
			actionable := 0.0
			if r.Actionable {
				actionable = 1
			}
			utilizationShareActionable.With(rl).Set(actionable)
		}
	}
}
