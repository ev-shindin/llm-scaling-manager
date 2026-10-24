package metrics

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/balance"
)

var (
	loadReadyReplicas       *prometheus.GaugeVec
	loadKvUsageMax          *prometheus.GaugeVec
	loadKvUsageMean         *prometheus.GaugeVec
	loadKvMaxToMean         *prometheus.GaugeVec
	loadQueueMaxToMean      *prometheus.GaugeVec
	loadIdleWhileSaturated  *prometheus.GaugeVec
	loadImbalanced          *prometheus.GaugeVec
	loadBalancePublishedMu  sync.Mutex
	loadBalancePublishedFor = map[string]map[string]prometheus.Labels{}
)

// registerLoadBalanceMetrics creates and registers the load-distribution
// gauges. Called from InitMetrics, after controllerInstance is set. The label
// keys are the ones the other per-model families use, so the series join with
// wva_model_replicas and wva_analyzer_demand on (namespace, model_name).
func registerLoadBalanceMetrics(registry prometheus.Registerer) error {
	labels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelRole}
	if controllerInstance != "" {
		labels = append(labels, constants.LabelControllerInstance)
	}
	gauge := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	}
	loadReadyReplicas = gauge(constants.WVALoadReadyReplicas,
		"Ready replicas of a model's role that the load-distribution gauges are over, warm-pool bridges included.")
	loadKvUsageMax = gauge(constants.WVALoadKvUsageMax,
		"Highest KV cache utilization (0-1) of any ready replica of the role, on its busiest engine rank, one-minute peak.")
	loadKvUsageMean = gauge(constants.WVALoadKvUsageMean,
		"Mean over the role's ready replicas of the same per-replica KV cache utilization.")
	loadKvMaxToMean = gauge(constants.WVALoadKvMaxToMeanRatio,
		"wva_load_kv_usage_max / wva_load_kv_usage_mean: 1 when the router spreads the role's load evenly, up to "+
			"the replica count when one replica holds all of it. Absent with fewer than two ready replicas or a mean of 0.")
	loadQueueMaxToMean = gauge(constants.WVALoadQueueMaxToMeanRatio,
		"Deepest waiting queue on one ready replica of the role over the mean queue: up to the replica count when "+
			"every waiting request is on one replica. Absent with fewer than two ready replicas or nothing waiting.")
	loadIdleWhileSaturated = gauge(constants.WVALoadIdleReplicasWhileSaturated,
		"Ready replicas of the role that are idle (KV under 0.30, nothing waiting, Ready for at least 2m) while another is saturated "+
			"(KV at or over kvCacheThreshold, or a queue at queueLengthThreshold except on the decode role of a model "+
			"with a prefill variant). 0 otherwise.")
	loadImbalanced = gauge(constants.WVALoadImbalanced,
		"1 once the role has had an idle replica beside a saturated one on every cycle for 2m, until it has read "+
			"balanced on every cycle for 2m; else 0. "+
			"A routing diagnostic: nothing scales on it.")

	loadBalancePublishedMu.Lock()
	loadBalancePublishedFor = map[string]map[string]prometheus.Labels{}
	loadBalancePublishedMu.Unlock()

	for name, c := range map[string]prometheus.Collector{
		constants.WVALoadReadyReplicas:              loadReadyReplicas,
		constants.WVALoadKvUsageMax:                 loadKvUsageMax,
		constants.WVALoadKvUsageMean:                loadKvUsageMean,
		constants.WVALoadKvMaxToMeanRatio:           loadKvMaxToMean,
		constants.WVALoadQueueMaxToMeanRatio:        loadQueueMaxToMean,
		constants.WVALoadIdleReplicasWhileSaturated: loadIdleWhileSaturated,
		constants.WVALoadImbalanced:                 loadImbalanced,
	} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register %s: %w", name, err)
		}
	}
	return nil
}

func loadBalanceModelKey(namespace, modelID string) string {
	return namespace + "\x00" + modelID
}

func loadBalanceLabels(namespace, modelID, role string) prometheus.Labels {
	l := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelID,
		constants.LabelRole:      role,
	}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	return l
}

// PublishLoadBalance makes one model's load-distribution series exactly these
// readings: every role in readings is set, and a role the model published
// before but not now (it has no ready replica this cycle) is deleted. A ratio
// that says nothing this cycle (balance.Distribution.KvMaxToMean) is deleted
// rather than published as 0. A no-op before InitMetrics.
func PublishLoadBalance(namespace, modelID string, readings []balance.Reading) {
	if loadImbalanced == nil {
		return
	}
	loadBalancePublishedMu.Lock()
	defer loadBalancePublishedMu.Unlock()
	key := loadBalanceModelKey(namespace, modelID)
	current := make(map[string]prometheus.Labels, len(readings))
	for _, r := range readings {
		l := loadBalanceLabels(namespace, modelID, r.Role)
		current[r.Role] = l
		loadReadyReplicas.With(l).Set(float64(r.Ready))
		loadKvUsageMax.With(l).Set(r.MaxKvUsage)
		loadKvUsageMean.With(l).Set(r.MeanKvUsage)
		if v, ok := r.KvMaxToMean(); ok {
			loadKvMaxToMean.With(l).Set(v)
		} else {
			loadKvMaxToMean.Delete(l)
		}
		if v, ok := r.QueueMaxToMean(); ok {
			loadQueueMaxToMean.With(l).Set(v)
		} else {
			loadQueueMaxToMean.Delete(l)
		}
		loadIdleWhileSaturated.With(l).Set(float64(r.IdleWhileSaturated()))
		imbalanced := 0.0
		if r.Sustained {
			imbalanced = 1
		}
		loadImbalanced.With(l).Set(imbalanced)
	}
	for role, l := range loadBalancePublishedFor[key] {
		if _, ok := current[role]; !ok {
			deleteLoadBalanceSeries(l)
		}
	}
	if len(current) == 0 {
		delete(loadBalancePublishedFor, key)
		return
	}
	loadBalancePublishedFor[key] = current
}

// DeleteLoadBalanceModel deletes every load-distribution series of one model.
func DeleteLoadBalanceModel(namespace, modelID string) {
	if loadImbalanced == nil {
		return
	}
	loadBalancePublishedMu.Lock()
	defer loadBalancePublishedMu.Unlock()
	key := loadBalanceModelKey(namespace, modelID)
	for _, l := range loadBalancePublishedFor[key] {
		deleteLoadBalanceSeries(l)
	}
	delete(loadBalancePublishedFor, key)
}

// RetainLoadBalanceModels deletes every load-distribution series of each model
// keep does not keep.
func RetainLoadBalanceModels(keep func(namespace, modelID string) bool) {
	if loadImbalanced == nil {
		return
	}
	loadBalancePublishedMu.Lock()
	defer loadBalancePublishedMu.Unlock()
	for key, roles := range loadBalancePublishedFor {
		namespace, modelID, _ := strings.Cut(key, "\x00")
		if keep(namespace, modelID) {
			continue
		}
		for _, l := range roles {
			deleteLoadBalanceSeries(l)
		}
		delete(loadBalancePublishedFor, key)
	}
}

func deleteLoadBalanceSeries(l prometheus.Labels) {
	for _, g := range []*prometheus.GaugeVec{loadReadyReplicas, loadKvUsageMax, loadKvUsageMean,
		loadKvMaxToMean, loadQueueMaxToMean, loadIdleWhileSaturated, loadImbalanced} {
		g.Delete(l)
	}
}
