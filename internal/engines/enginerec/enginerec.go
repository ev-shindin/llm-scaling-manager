// Package enginerec holds the engine-level events of a learner recording
// (internal/signals/learnrec): what the engine hands the composite, and what
// the composite and the warm pool's figures came out as. Issue #345 step 0
// adds them beside the analyzer events of docs/proposals/analyzer-learner.md,
// 2.4, so a replay can hold the composite to a golden as it holds the
// analyzers.
//
// The engine (internal/engines/steadystate) writes these events and replays
// them; the replay (internal/engines/analyzers/learnreplay) reads them by
// these types, and cannot import the engine, whose tests import it.
package enginerec

import (
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// Owner is the prefix of every engine event's kind.
const Owner = "engine."

// KindComposite is one model's composite in one cycle: its inputs, as the
// engine handed them to it, and what it built.
const KindComposite = Owner + "Composite"

// Kinds is every engine event kind, sorted.
func Kinds() []string { return []string{KindComposite} }

// Owns reports whether kind is an engine event.
func Owns(kind string) bool { return strings.HasPrefix(kind, Owner) }

// CompositeEvent is one model's composite in one cycle.
//
// The inputs are recorded at the composite's boundary, as the analyzer
// events record theirs (analyzer-learner.md, 2.4): what the engine supplied,
// never what a recorded analyzer returned. A replay takes an analyzer's
// result from its own replayed Analyze, and builds each analyzer's entry the
// way the engine does (the capacity-build step), so a change to an analyzer
// reaches the composite in the replay as it would in the controller.
type CompositeEvent struct {
	Namespace, ModelID string
	// Entries is every analyzer entry the cycle built, in the engine's order:
	// saturation first, then each other analyzer that returned a result.
	Entries []CompositeEntry
	// Eligible names the entries the composite may size from, in order:
	// every entry less the slo analyzer, and less saturation when the policy
	// does not list it.
	Eligible []string
	// Grace is the entry, with no result, of each listed analyzer that
	// returned nothing this cycle and is still inside its staleness grace.
	Grace []allocation.NamedAnalyzerResult
	// Metadata is the discovery metadata the capacity-build step joins onto
	// each analyzer's variant capacities; nil when the cycle had none.
	Metadata []domain.VariantMetadata
	// ScaleUp and ScaleDown are saturation's thresholds, an override
	// included: the composite's, and the warm pool's pressure's.
	ScaleUp, ScaleDown float64

	// Composite is what the composite built, its result without AnalyzedAt.
	Composite allocation.NamedAnalyzerResult
	// HoldReasons is, for each role the composite holds, why: the reasons
	// the "composite holds a role" line logs, in the order they were found.
	HoldReasons map[string][]string
	// Supply is what the warm pool reads of each variant's bridges, and
	// Pressure how hard each variant is pushed: what the engine publishes
	// for the warm pool from this composite, in variant order.
	Supply   []WarmPoolSupply
	Pressure []VariantPressure
}

// CompositeEntry is one analyzer entry as the engine supplied it to the
// capacity-build step and the composite.
type CompositeEntry struct {
	Name string
	// Score and the thresholds are the policy's for this analyzer.
	Score, ScaleUp, ScaleDown float64
	// Live is the engine's liveness verdict for the entry this cycle. It
	// reads the analyzer's history in the engine, so it is recorded, not
	// recomputed.
	Live bool
	// Result is the analyzer's result, recorded only for an analyzer the
	// replay does not run (an external one); nil for saturation, slo and
	// throughput, whose own Analyze event the replay reproduces it from.
	Result *domain.AnalyzerResult
}

// WarmPoolSupply is what one variant gets from the warm pool's bridges.
type WarmPoolSupply struct {
	Variant  string
	Replicas int
	Capacity float64
}

// VariantPressure is how hard one variant is pushed: the fraction of its own
// supply left spare, and whether it needs to grow.
type VariantPressure struct {
	Variant       string
	SpareFraction float64
	NeedsScaleUp  bool
}
