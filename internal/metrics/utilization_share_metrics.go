package metrics

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var (
	utilizationShareHeadroom       *prometheus.GaugeVec
	utilizationShareTargetGPUs     *prometheus.GaugeVec
	utilizationShareActionable     *prometheus.GaugeVec
	utilizationShareSpareGPUs      *prometheus.GaugeVec
	utilizationShareReplicasToMove *prometheus.GaugeVec
	utilizationShareTransfers      *prometheus.CounterVec
	utilizationSharePromisedGPUs   *prometheus.GaugeVec
	utilizationShareReserveDebt    *prometheus.GaugeVec
	utilizationShareActual         *prometheus.GaugeVec
	utilizationShareFloorExcess    *prometheus.GaugeVec
	utilizationShareWithheld       *prometheus.CounterVec
	utilizationShareEffective      *prometheus.GaugeVec
	utilizationShareSwinging       *prometheus.GaugeVec
	utilizationShareRelease        *prometheus.HistogramVec
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
	utilizationSharePromisedGPUs = gauge(constants.WVAUtilizationSharePromisedGPUs,
		"Utilization-share optimizer: GPUs released for a receiver and not yet held by it. "+
			"Published only while the optimizer acts.",
		groupLabels)
	utilizationShareActual = gauge(constants.WVAUtilizationShareActual,
		"Utilization-share optimizer: a role's utilization at the GPUs it holds, on the scale of its "+
			"scale-up threshold. Absent for a role with no demand or no GPUs.",
		roleLabels)
	utilizationShareFloorExcess = gauge(constants.WVAUtilizationShareFloorExcessGPUs,
		"Utilization-share optimizer: GPUs a role's floor holds above its need.",
		roleLabels)
	utilizationShareWithheld = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: constants.WVAUtilizationShareWithheldTotal,
		Help: "Utilization-share optimizer: transfers not planned, by reason (reversal-hold, not-actionable).",
	}, append(slices.Clone(groupLabels), constants.LabelReason))
	if err := registry.Register(utilizationShareWithheld); err != nil {
		return fmt.Errorf("failed to register utilization-share metric: %w", err)
	}
	utilizationShareReserveDebt = gauge(constants.WVAUtilizationShareReserveDebtGPUs,
		"Utilization-share optimizer: reserve GPUs spent and not yet refilled. Published only while "+
			"the optimizer acts.",
		groupLabels)
	utilizationShareEffective = gauge(constants.WVAUtilizationShareEffectiveSeconds,
		"Utilization-share optimizer: a derived timing in force, in seconds, and where its inputs came "+
			"from (scaledobject, pod, measured, default). Published only while the optimizer acts.",
		append(slices.Clone(groupLabels), constants.LabelParam, constants.LabelSource))
	utilizationShareSwinging = gauge(constants.WVAUtilizationShareSwinging,
		"Utilization-share optimizer: 1 while a role is planned on its mean need because it reversed "+
			"direction twice within the swing window. Published only while the optimizer acts.",
		roleLabels)
	utilizationShareRelease = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    constants.WVAUtilizationShareReleaseSeconds,
		Help:    "Utilization-share optimizer: time from a transfer's start to its donor's GPUs being released.",
		Buckets: []float64{30, 60, 120, 240, 360, 480, 600, 900, 1200, 1800},
	}, groupLabels)
	if err := registry.Register(utilizationShareRelease); err != nil {
		return fmt.Errorf("failed to register utilization-share metric: %w", err)
	}
	utilizationShareTransfers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: constants.WVAUtilizationShareTransfersTotal,
		Help: "Utilization-share optimizer: transfers that left the ledger, by outcome.",
	}, append(slices.Clone(groupLabels), constants.LabelOutcome, constants.LabelUrgent))
	if err := registry.Register(utilizationShareTransfers); err != nil {
		return fmt.Errorf("failed to register utilization-share metric: %w", err)
	}
	for _, g := range []*prometheus.GaugeVec{
		utilizationShareHeadroom, utilizationShareTargetGPUs, utilizationShareActionable,
		utilizationShareSpareGPUs, utilizationShareReplicasToMove,
		utilizationSharePromisedGPUs, utilizationShareEffective, utilizationShareSwinging,
		utilizationShareReserveDebt, utilizationShareActual, utilizationShareFloorExcess,
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
	// Swinging is published only for an active group.
	Swinging bool
	// Actual is u_r; NaN publishes no series. FloorExcess is in GPUs.
	Actual      float64
	FloorExcess float64
}

// UtilizationShareTiming is one derived timing of an active group.
type UtilizationShareTiming struct {
	Param, Source string
	Seconds       float64
}

// UtilizationShareGroup is one group's published evaluation.
type UtilizationShareGroup struct {
	AcceleratorType string
	// Scope is a namespace, or constants.UtilizationShareClusterScope.
	Scope          string
	SpareGPUs      float64
	ReplicasToMove int
	Roles          []UtilizationShareRole
	// Active is true when the optimizer acts on this group; the fields below
	// are published only then.
	Active          bool
	PromisedGPUs    float64
	ReserveDebtGPUs float64
	Timings         []UtilizationShareTiming
}

// utilizationSharePublished is the series the last PublishUtilizationShare set,
// per gauge, so the next call can delete only those that are gone. It is
// written by the single optimize loop only.
var utilizationSharePublished = map[*prometheus.GaugeVec]map[string]prometheus.Labels{}

func seriesKey(l prometheus.Labels) string {
	parts := make([]string, 0, len(l))
	for k, v := range l {
		parts = append(parts, k+"="+v)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// PublishUtilizationShare makes the utilization-share series exactly this
// cycle's evaluation. It sets every current series first and only then deletes
// the ones that are gone, so a scrape never lands on an empty set -- a gap the
// actionable gauge's alerts would read as "nothing to do". Calling it with no
// groups clears them all, which is what a cycle that does not run the optimizer
// must do: a stale headroom reading would describe a fleet nothing is sizing.
func PublishUtilizationShare(groups []UtilizationShareGroup) {
	if utilizationShareHeadroom == nil {
		return
	}
	current := map[*prometheus.GaugeVec]map[string]prometheus.Labels{}
	set := func(g *prometheus.GaugeVec, l prometheus.Labels, v float64) {
		g.With(l).Set(v)
		if current[g] == nil {
			current[g] = map[string]prometheus.Labels{}
		}
		current[g][seriesKey(l)] = l
	}
	defer func() {
		for g, prev := range utilizationSharePublished {
			for k, l := range prev {
				if _, ok := current[g][k]; !ok {
					g.Delete(l)
				}
			}
		}
		utilizationSharePublished = current
	}()
	for _, grp := range groups {
		gl := prometheus.Labels{constants.LabelAcceleratorType: grp.AcceleratorType, constants.LabelScope: grp.Scope}
		if controllerInstance != "" {
			gl[constants.LabelControllerInstance] = controllerInstance
		}
		set(utilizationShareSpareGPUs, gl, grp.SpareGPUs)
		set(utilizationShareReplicasToMove, gl, float64(grp.ReplicasToMove))
		if grp.Active {
			set(utilizationSharePromisedGPUs, gl, grp.PromisedGPUs)
			set(utilizationShareReserveDebt, gl, grp.ReserveDebtGPUs)
			for _, tm := range grp.Timings {
				tl := maps.Clone(gl)
				tl[constants.LabelParam] = tm.Param
				tl[constants.LabelSource] = tm.Source
				set(utilizationShareEffective, tl, tm.Seconds)
			}
		}
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
				set(utilizationShareHeadroom, rl, r.Headroom)
			}
			set(utilizationShareTargetGPUs, rl, r.TargetGPUs)
			actionable := 0.0
			if r.Actionable {
				actionable = 1
			}
			set(utilizationShareActionable, rl, actionable)
			if !math.IsNaN(r.Actual) {
				set(utilizationShareActual, rl, r.Actual)
			}
			set(utilizationShareFloorExcess, rl, r.FloorExcess)
			if grp.Active {
				swinging := 0.0
				if r.Swinging {
					swinging = 1
				}
				set(utilizationShareSwinging, rl, swinging)
			}
		}
	}
}

// CountUtilizationShareTransfer counts one transfer leaving a group's ledger.
func CountUtilizationShareTransfer(acceleratorType, scope, outcome string, urgent bool) {
	if utilizationShareTransfers == nil {
		return
	}
	l := prometheus.Labels{
		constants.LabelAcceleratorType: acceleratorType,
		constants.LabelScope:           scope,
		constants.LabelOutcome:         outcome,
		constants.LabelUrgent:          strconv.FormatBool(urgent),
	}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	utilizationShareTransfers.With(l).Inc()
}

// ObserveUtilizationShareRelease records how long one donor took to release.
func ObserveUtilizationShareRelease(acceleratorType, scope string, d time.Duration) {
	if utilizationShareRelease == nil {
		return
	}
	l := prometheus.Labels{constants.LabelAcceleratorType: acceleratorType, constants.LabelScope: scope}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	utilizationShareRelease.With(l).Observe(d.Seconds())
}

// CountUtilizationShareWithheld counts n transfers not planned for reason.
func CountUtilizationShareWithheld(acceleratorType, scope, reason string, n int) {
	if utilizationShareWithheld == nil || n <= 0 {
		return
	}
	l := prometheus.Labels{
		constants.LabelAcceleratorType: acceleratorType,
		constants.LabelScope:           scope,
		constants.LabelReason:          reason,
	}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	utilizationShareWithheld.With(l).Add(float64(n))
}
