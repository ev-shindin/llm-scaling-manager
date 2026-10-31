// Package enginerec holds the engine-level events of a learner recording
// (internal/signals/learnrec): what the engine hands the composite that it
// did not compute from the cycle's policy and results, and what the
// composite and the warm pool's figures came out as. Issue #345 step 0 adds
// them beside the analyzer events of docs/proposals/analyzer-learner.md,
// 2.4, so a replay can hold the composite to a golden as it holds the
// analyzers.
//
// The engine (internal/engines/steadystate) writes these events and replays
// them; the replay (internal/engines/analyzers/learnreplay) reads them by
// these types, and cannot import the engine, whose tests import it.
package enginerec

import (
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// Owner is the prefix of every engine event's kind.
const Owner = "engine."

// KindComposite is one model's composite in one cycle: the inputs only the
// engine's history can supply, and what it built.
const KindComposite = Owner + "Composite"

// Kinds is every engine event kind, sorted.
func Kinds() []string { return []string{KindComposite} }

// Owns reports whether kind is an engine event.
func Owns(kind string) bool { return strings.HasPrefix(kind, Owner) }

// Why the composite holds a role: the reasons a CompositeEvent's HoldReasons
// holds, and the "composite holds a role" line logs. The engine defines them
// here, so the export that must recognise them reads the same words.
const (
	// HoldUnsized: saturation is not listed and no listed analyzer sizes the
	// role, so it neither grows nor shrinks.
	HoldUnsized = "no-listed-analyzer-sizes-role"
	// HoldOtherRoleUnsized: on P/D, another role is held for HoldUnsized,
	// so this one does not grow either.
	HoldOtherRoleUnsized = "other-role-cannot-grow"
	// HoldAnalyzerInGrace is the prefix of the reason a listed analyzer
	// inside its staleness grace with no reading holds a role against
	// scale-down; the analyzer's name follows it.
	HoldAnalyzerInGrace = "analyzer-in-grace:"
	// HoldSaturationHolds: saturation holds the role (HeldRoleDemand) and
	// another analyzer's demand replaced its held figure, so the role does
	// not shrink.
	HoldSaturationHolds = "saturation-holds-role"
)

// HoldReasons is every hold reason that is a whole word:
// HoldAnalyzerInGrace is a prefix, and not among them.
func HoldReasons() []string {
	return []string{HoldUnsized, HoldOtherRoleUnsized, HoldSaturationHolds}
}

// Words is every word the composite writes into a recording that the
// export must keep as it is: the composite's name, its decision paths, and
// the hold reasons that are whole words. The export reads them here,
// beside the code that writes them, rather than retyping them.
func Words() []string {
	return append([]string{allocation.CompositeSignalName, string(allocation.DecisionAgree),
		string(allocation.DecisionSingle), string(allocation.DecisionNoSignal)}, HoldReasons()...)
}

// Replayed is what a replay has reproduced when it reaches an engine event:
// each analyzer's last result for a model, and the policy the model's last
// Analyze received.
type Replayed interface {
	// Result is the replay's own result of an analyzer's last Analyze of a
	// model, false when that Analyze failed or never ran.
	Result(analyzer, namespace, modelID string) (*domain.AnalyzerResult, bool)
	// Policy is the policy the model's last saturation Analyze received,
	// false when none has run.
	Policy(namespace, modelID string) (*config.ScalingPolicy, bool)
}

// CompositeEvent is one model's composite in one cycle.
//
// The inputs are recorded at the composite's boundary, as the analyzer
// events record theirs (analyzer-learner.md, 2.4), and only those the replay
// cannot rebuild. Everything the engine derives from the cycle's policy (the
// eligible entries, each analyzer's score and thresholds, saturation's
// thresholds) the replay derives again, with the engine's own functions,
// from the policy the Analyze events recorded; each analyzer's result it
// takes from its own replayed Analyze. A change to any of them reaches the
// composite in the replay as it would in the controller.
type CompositeEvent struct {
	Namespace, ModelID string
	// Entries is every analyzer entry the cycle built, in the engine's order:
	// saturation first, then each other analyzer that returned a result.
	Entries []CompositeEntry
	// Grace is the entry, with no result, of each listed analyzer that
	// returned nothing this cycle and is still inside its staleness grace:
	// read from the engine's history of the analyzer.
	Grace []allocation.NamedAnalyzerResult
	// Metadata is what the capacity-build step reads of the discovery
	// metadata it joins onto each analyzer's variant capacities; nil when
	// the cycle had none.
	Metadata []VariantMeta

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

// CompositeEntry is one analyzer entry: what the engine knew of it that the
// cycle's policy and results do not say.
type CompositeEntry struct {
	Name string
	// Live is the engine's liveness verdict for the entry this cycle. It
	// reads the analyzer's history in the engine, so it is recorded, not
	// recomputed.
	Live bool
	// Result is the analyzer's result, recorded only for an analyzer the
	// replay does not run (an external one); nil for saturation, slo and
	// throughput, whose own Analyze event the replay reproduces it from.
	Result *domain.AnalyzerResult
}

// VariantMeta is the part of a variant's discovery metadata the
// capacity-build step reads: its name, its role, and its current replicas.
type VariantMeta struct {
	VariantName, Role string
	CurrentReplicas   int
}

// MetaOf is what a recording keeps of metadata.
func MetaOf(metadata []domain.VariantMetadata) []VariantMeta {
	if metadata == nil {
		return nil
	}
	out := make([]VariantMeta, 0, len(metadata))
	for _, m := range metadata {
		out = append(out, VariantMeta{VariantName: m.VariantName, Role: m.Role, CurrentReplicas: m.CurrentReplicas})
	}
	return out
}

// Metadata is the discovery metadata a recording's VariantMeta stands for,
// with every field the recording does not keep at its zero value.
func Metadata(meta []VariantMeta) []domain.VariantMetadata {
	if meta == nil {
		return nil
	}
	out := make([]domain.VariantMetadata, 0, len(meta))
	for _, m := range meta {
		out = append(out, domain.VariantMetadata{VariantName: m.VariantName, Role: m.Role, CurrentReplicas: m.CurrentReplicas})
	}
	return out
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
