package steadystate

import (
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/balance"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// loadImbalancedWarning is the line logged on the cycle a role is reported
// imbalanced. Each claim in it holds by construction: the idle replica has
// been Ready for balance.IdleAfterReady with nothing waiting, beside a
// saturated one, on every cycle for balance.SustainFor. So the role already
// has a replica the router is not sending work to; a new one would most
// likely be passed over the same way. It says "unlikely", not "cannot": a
// replica holding long-running requests can stay full while new ones go
// elsewhere, and more replicas do then add room.
const loadImbalancedWarning = "WARNING: load imbalanced across a role's replicas: one is saturated while another, " +
	"Ready for at least 2m with nothing waiting, has sat idle beside it for 2m. The router is not using a replica " +
	"the role already has, so adding replicas is unlikely to relieve it. Check the router's scorer weights " +
	"(queue-scorer, active-request-scorer, kv-cache-utilization-scorer against prefix-cache-scorer)"

// observeLoadBalance measures how evenly the router spreads this model's load
// over each role's ready replicas, publishes it (wva_load_*), and logs a
// warning on the cycle a role is reported imbalanced and a line on the cycle
// the report clears.
//
// Observe only. It reads the rows the analyzers were given and changes none of
// them, and nothing it computes reaches a decision: capacity is priced per
// replica and demand per fleet, and a role whose router piles load on one
// replica is fixed in the router's scorer configuration, not by a replica
// count.
func (e *Engine) observeLoadBalance(logger logr.Logger, namespace, modelID string,
	rows []domain.ReplicaMetrics, states []domain.VariantReplicaState, queueThreshold float64) {
	// Lazily, as the other per-model maps here: optimize cycles run one at a
	// time, and an Engine built as a literal in a test has none.
	if e.loadBalance == nil {
		e.loadBalance = balance.NewTracker()
	}
	now := time.Now()
	if e.loadBalanceNow != nil {
		now = e.loadBalanceNow()
	}
	roles := make(map[string]string, len(states))
	for _, vs := range states {
		roles[vs.VariantName] = vs.Role
	}
	readings := e.loadBalance.Observe(utils.GetNamespacedKey(namespace, modelID),
		balance.Measure(rows, roles, queueThreshold, now), now)
	metrics.PublishLoadBalance(namespace, modelID, readings)
	for _, r := range readings {
		if !r.Changed {
			continue
		}
		kvRatio, _ := r.KvMaxToMean()
		fields := []any{"namespace", namespace, "modelID", modelID, "role", r.Role,
			"readyReplicas", r.Ready, "saturatedReplicas", r.Saturated, "idleReplicas", r.Idle,
			"busiestPod", r.BusiestPod, "idlePods", r.IdlePods,
			"kvUsageMax", r.MaxKvUsage, "kvUsageMean", r.MeanKvUsage, "kvMaxToMean", kvRatio,
			"queueMax", r.MaxQueue, "queueMean", r.MeanQueue}
		if r.Sustained {
			logger.Info(loadImbalancedWarning, fields...)
			continue
		}
		logger.Info("load balanced again across a role's replicas", fields...)
	}
}

// retainLoadBalance deletes the load-distribution series, and the tracker's
// memory, of every model whose namespaced key is not in active; nil deletes
// them all.
func (e *Engine) retainLoadBalance(active map[string]bool) {
	metrics.RetainLoadBalanceModels(func(namespace, modelID string) bool {
		return active[utils.GetNamespacedKey(namespace, modelID)]
	})
	if e.loadBalance != nil {
		e.loadBalance.Retain(active)
	}
}

// forgetLoadBalance deletes one model's load-distribution series and what the
// tracker remembers about it.
func (e *Engine) forgetLoadBalance(namespace, modelID string) {
	metrics.DeleteLoadBalanceModel(namespace, modelID)
	if e.loadBalance != nil {
		e.loadBalance.Forget(utils.GetNamespacedKey(namespace, modelID))
	}
}
