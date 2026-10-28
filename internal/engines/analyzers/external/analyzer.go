// Package external implements a built-in analyzer that turns a config-driven
// PromQL definition into a domain.Analyzer, so WVA can be extended with new
// scaling signals without writing Go (issue #1455).
//
// An external analyzer mirrors KEDA scaler vocabulary: a demand query (the total
// signal D) and a per-replica target (threshold P), such that desired replicas =
// ceil(D / P). Like the collector's registerForEngine, the query body is
// per-engine: a Definition carries one Body per inference engine (vllm, sglang),
// plus an optional engine-agnostic body; Analyze selects the body matching the
// model's engine. It runs the selected query each cycle via the shared metrics
// source and reduces the result to D; P is the body's constant threshold.
//
// Per-variant identity (cost, accelerator) is filled downstream by the capacity
// builder from the discovery step, so the wrapper emits only the measured (D, P)
// signal — the same contract the internal analyzers follow.
package external

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/utils/clock"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// queryNamePrefix namespaces external-analyzer queries in the shared QueryList so
// they cannot collide with the built-in template names.
const queryNamePrefix = "external:"

// reasonConstantTarget labels a per-replica capacity that came from a constant
// threshold rather than a measured query.
const reasonConstantTarget = "E1-const"

// engineAgnostic is the Bodies key for a body that applies to any engine. It is
// used when no engine-specific body matches the model's engine.
const engineAgnostic = ""

// Body is one engine's query and per-replica target for an external analyzer.
// The field names mirror KEDA's Prometheus scaler.
//
// # Units
//
// D and P must agree, and that agreement is the operator's to get right: it
// cannot be checked here, since PromQL semantics are not inspectable.
//
// Query returns D, the model's TOTAL demand — every series it returns is summed
// into one number. Threshold is P, what ONE scale-target replica (a pod, or an
// LWS group) can serve. desired = ceil(D / P), so P must be the whole pod's
// figure and not one engine instance's: a data-parallel pod runs several.
//
// A sum-shaped query needs nothing further, whatever granularity the underlying
// series have — summing across a pod's DP ranks and summing across pods give
// the same total. What breaks is a query whose value depends on how many series
// exist: count() counts DP ranks and avg() averages over them, so both measure
// in instances while P is in pods. Collapse to pod granularity first:
//
//	count(count by (pod) (...))
//	avg(avg by (pod) (...))
//
// The built-in analyzers get this for free: their per-replica figures come from
// domain.ReplicaMetrics, which the collector has already merged to one record
// per pod (collector.collapseToPods). Query runs straight against the metrics
// source and never passes through that merge.
type Body struct {
	// Query is a PromQL expression for the total demand D. It may reference
	// {{.namespace}} and {{.modelID}}; the metrics source escapes both before
	// substitution. All returned series are summed, so it must resolve to the
	// model's total demand and not to a per-replica breakdown of it.
	Query string
	// Threshold is the per-replica target P — the demand a single scale-target
	// replica can serve (see the Units note above). desired = ceil(D / threshold).
	// Must be > 0.
	Threshold float64
}

// Definition describes a config-driven external analyzer with per-engine query
// bodies. A body under the engineAgnostic ("") key applies to any engine.
type Definition struct {
	// Label is the analyzer's unique name, used for policy selection and metrics.
	Label string
	// Bodies maps an inference engine ("vllm", "sglang") to its query body; the
	// empty-string key is the engine-agnostic body. At least one is required.
	Bodies map[string]Body
}

// Analyzer is the built-in wrapper that runs a Definition as a domain.Analyzer.
// SeriesGate rejects a series whose Pod is gone, so an external analyzer counts
// the same Pods the collector's builder would.
type SeriesGate func(ctx context.Context, namespace string, labels map[string]string) bool

type Analyzer struct {
	def    Definition
	source source.MetricsSource
	// queryNames maps an engine key to the query name registered for its body.
	queryNames map[string]string

	// DropSeries applies the collector's Pod gate to this analyzer's own query
	// results, so a deleted Pod stops contributing demand here as it does there.
	//
	// A field rather than a constructor argument because it is optional and
	// layered: the engine wires the collector's gate in after construction, and
	// an analyzer built without one sums everything, which is what it did before.
	DropSeries SeriesGate

	// clock stamps AnalyzedAt: the engine's cycle clock (WithClock), the wall
	// clock by default.
	clock clock.PassiveClock
}

// New constructs an external analyzer from def and registers each body's query
// into src's shared QueryList. Registration uses Upsert, so re-initialization
// (e.g. a catalog reload) is safe.
func New(def Definition, src source.MetricsSource) (*Analyzer, error) {
	if def.Label == "" {
		return nil, errors.New("external analyzer: label is required")
	}
	if len(def.Bodies) == 0 {
		return nil, fmt.Errorf("external analyzer %q: at least one query body is required", def.Label)
	}
	if src == nil {
		return nil, fmt.Errorf("external analyzer %q: metrics source is required", def.Label)
	}

	queryNames := make(map[string]string, len(def.Bodies))
	for engineKey, body := range def.Bodies {
		if body.Query == "" {
			return nil, fmt.Errorf("external analyzer %q (%s): query is required", def.Label, engineLabel(engineKey))
		}
		if body.Threshold <= 0 {
			return nil, fmt.Errorf("external analyzer %q (%s): threshold must be > 0, got %v", def.Label, engineLabel(engineKey), body.Threshold)
		}
		name := queryName(def.Label, engineKey)
		if err := src.QueryList().Upsert(source.QueryTemplate{
			Name:        name,
			Type:        source.QueryTypePromQL,
			Template:    body.Query,
			Params:      []string{source.ParamNamespace, source.ParamModelID},
			Description: fmt.Sprintf("demand (D) for external analyzer %q (%s)", def.Label, engineLabel(engineKey)),
		}); err != nil {
			return nil, fmt.Errorf("external analyzer %q (%s): registering demand query: %w", def.Label, engineLabel(engineKey), err)
		}
		queryNames[engineKey] = name
	}

	return &Analyzer{def: def, source: src, queryNames: queryNames, clock: clock.RealClock{}}, nil
}

// WithClock sets the clock AnalyzedAt is stamped on: the engine passes its
// cycle clock. Call it before the analyzer is used.
func (a *Analyzer) WithClock(c clock.PassiveClock) *Analyzer {
	a.clock = c
	return a
}

// Name returns the analyzer's label.
func (a *Analyzer) Name() string { return a.def.Label }

// Analyze selects the query body for the model's engine, runs it, and produces a
// (D, P) result: D is the summed query value, P is the body's constant threshold.
// When no body matches the model's engine the analyzer does not apply to this
// model and returns a nil result (the engine skips it — the not-defined state,
// distinct from a present-but-zero result).
func (a *Analyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	body, qName, ok := a.selectBody(modelEngine(input.VariantStates))
	if !ok {
		return nil, nil
	}

	demand, err := a.queryDemand(ctx, qName, input.ModelID, input.Namespace)
	if err != nil {
		return nil, err
	}

	vcs := make([]domain.VariantCapacity, 0, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}
		// Cost/AcceleratorName are intentionally unset: the capacity builder fills
		// per-variant identity from discovery. The wrapper emits only P.
		vcs = append(vcs, domain.VariantCapacity{
			VariantName:        vs.VariantName,
			Role:               vs.Role,
			ReplicaCount:       readyCount,
			PendingReplicas:    vs.PendingReplicas,
			PerReplicaCapacity: body.Threshold,
			Reason:             reasonConstantTarget,
		})
	}

	return &domain.AnalyzerResult{
		AnalyzerName:      a.def.Label,
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        a.clock.Now(),
		VariantCapacities: vcs,
		TotalDemand:       demand,
		// Supply, utilization, and RoleCapacities are assembled downstream by the
		// engine's capacity-build step; the wrapper emits only the (D, P) signal.
		// RequiredCapacity/SpareCapacity are written by the engine post-step.
		// RoleDemand is nil: this MVP treats demand as model-level.
	}, nil
}

// selectBody returns the body (and its query name) for engine, falling back to
// the engine-agnostic body. ok is false when neither matches.
func (a *Analyzer) selectBody(engine string) (Body, string, bool) {
	if b, ok := a.def.Bodies[engine]; ok {
		return b, a.queryNames[engine], true
	}
	if b, ok := a.def.Bodies[engineAgnostic]; ok {
		return b, a.queryNames[engineAgnostic], true
	}
	return Body{}, "", false
}

// queryDemand runs the given query name for (modelID, namespace) and returns the
// sum of all returned series — the model-level total demand.
//
// A failed query is an error, never zero demand: an outage is not a
// measurement. The engine discards an analyzer result that errors
// (runRegisteredAnalyzer), so this analyzer reports nothing for the model this
// cycle and its wva_analyzer_demand series is dropped rather than set to 0.
// A query that returned no result at all, which a source does only for a query
// it did not run, still reads as zero.
func (a *Analyzer) queryDemand(ctx context.Context, qName, modelID, namespace string) (float64, error) {
	results, err := a.source.Refresh(ctx, source.RefreshSpec{
		Queries: []string{qName},
		Params: map[string]string{
			source.ParamModelID:   modelID,
			source.ParamNamespace: namespace,
		},
	})
	if err != nil {
		return 0, fmt.Errorf("external analyzer %q: demand query: %w", a.def.Label, err)
	}

	res := results[qName]
	if res == nil {
		return 0, nil
	}
	// Prometheus already returns an error above when this, its only query,
	// failed. This covers a source that reports a query's failure only in the
	// result.
	if res.HasError() {
		return 0, fmt.Errorf("external analyzer %q: demand query: %w", a.def.Label, res.Error)
	}
	var demand float64
	for _, v := range res.Values {
		// The SAME Pod gate the collector's builder applies, so an external
		// analyzer measures the same population as a built-in one.
		//
		// It has to be applied here because this analyzer never builds a per-Pod
		// record: it runs the operator's query and sums the series itself. Without
		// this, a per-Pod body counts deleted Pods for as long as Prometheus keeps
		// their series -- the same defect the builder prevents, on a path the
		// builder cannot see.
		//
		// An already-aggregated body has no Pod label left to judge and is summed
		// untouched; see collector.SeriesPodIsGone.
		if a.DropSeries != nil && a.DropSeries(ctx, namespace, v.Labels) {
			continue
		}
		demand += v.Value
	}
	return demand, nil
}

// modelEngine returns the model's inference engine from its variant states.
// MVP: it uses the first variant that declares an engine (models are assumed
// engine-homogeneous); mixed-engine models are a follow-up.
func modelEngine(states []domain.VariantReplicaState) string {
	for _, vs := range states {
		if vs.Engine != "" {
			return vs.Engine
		}
	}
	return ""
}

// queryName builds the registered query name for an analyzer label and engine
// key. The engine-agnostic body has no engine suffix.
func queryName(label, engineKey string) string {
	if engineKey == engineAgnostic {
		return queryNamePrefix + label + ":demand"
	}
	return queryNamePrefix + label + ":demand:" + engineKey
}

// engineLabel renders an engine key for error messages.
func engineLabel(engineKey string) string {
	if engineKey == engineAgnostic {
		return "engine-agnostic"
	}
	return "engine=" + engineKey
}
