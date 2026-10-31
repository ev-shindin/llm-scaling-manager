package steadystate

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// The settle tracker (internal/signals/settle, analyzer-learner.md 4.2): the
// engine owns it, one per controller, keyed by model, and writes it once per
// model per cycle at the top of runAnalyzers, before the load-balance
// diagnostic and the input builder read the cycle's rows. Readers take a
// read-only view; the load-balance tracker is the first.

// settleTracker is the engine's settle tracker, set to the configured
// optimize interval, which sets how long a replica may go without a row
// (settle.Tolerance). Built on first use, as the other per-model state here:
// optimize cycles run one at a time, and an Engine built as a literal in a
// test has none.
func (e *Engine) settleTracker() *settle.Tracker {
	if e.settle == nil {
		e.settle = settle.NewTracker()
	}
	e.settle.SetInterval(e.sloMissedAfter())
	return e.settle
}

// observeSettle observes one model's rows into the settle tracker at now,
// and records the observation when a learner recording is open: the model
// and now, the rows being the ones the model's Analyze events record.
func (e *Engine) observeSettle(namespace, modelID string, rows []domain.ReplicaMetrics,
	states []domain.VariantReplicaState, now time.Time) {
	e.learnRec.Record(settle.KindObserve, now, settle.ObserveEvent{Namespace: namespace, ModelID: modelID,
		Interval: e.sloMissedAfter()})
	e.settleTracker().Observe(utils.GetNamespacedKey(namespace, modelID),
		settlerow.Of(rows, variantRoles(states)), now)
}

// holdSettle records a cycle that read no rows of the model, its collection
// failed or returned nothing, at now, and records the hold when a learner
// recording is open. The replicas seen last keep their sighting through it
// (settle.Tracker.Hold): a Prometheus outage is not an absence.
func (e *Engine) holdSettle(namespace, modelID string, now time.Time) {
	e.learnRec.Record(settle.KindHold, now, settle.ObserveEvent{Namespace: namespace, ModelID: modelID,
		Interval: e.sloMissedAfter()})
	e.settleTracker().Hold(utils.GetNamespacedKey(namespace, modelID), now)
}

// modelCollectionFailed is what a cycle whose collection failed for the
// model does to the per-replica state the engine keeps: the settle tracker
// holds its sightings, and the load-balance series stand until collection
// has failed for balance.StaleAfter (loadBalanceCollectionFailed).
func (e *Engine) modelCollectionFailed(namespace, modelID string) {
	now := e.observeClock()
	e.holdSettle(namespace, modelID, now)
	e.loadBalanceCollectionFailed(namespace, modelID, now)
}

// retainSettle forgets the settle record of every model whose namespaced key
// is not in active; nil forgets them all. A model that is still reconciled
// keeps its record through a cycle with no rows or a failed collection
// (holdSettle), until it has gone settle.TraceHorizon without rows.
func (e *Engine) retainSettle(active map[string]bool) {
	if e.settle != nil {
		e.settle.Retain(active)
	}
}

// variantRoles maps each variant to its role as discovery recorded it.
func variantRoles(states []domain.VariantReplicaState) map[string]string {
	roles := make(map[string]string, len(states))
	for _, vs := range states {
		roles[vs.VariantName] = vs.Role
	}
	return roles
}
