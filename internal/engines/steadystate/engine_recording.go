package steadystate

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The engine-level layer of the learner recording (issue #345, step 0):
// each composite's inputs, at the composite's boundary, and what it built,
// with the warm pool's figures published from it. The analyzer events
// (docs/proposals/analyzer-learner.md, 2.4) stop at each Analyze; this layer
// carries the recording on to the composite. Off unless a learner recording
// is open (learner_recording.go); with it off nothing here runs.
//
// It records only what the replay cannot rebuild: the liveness verdicts and
// the grace entries, which read the engine's history of each analyzer; an
// external analyzer's result; and what the capacity-build step reads of the
// discovery metadata. What the engine derives from the cycle's policy -- the
// eligible entries, each analyzer's score and thresholds, saturation's
// thresholds -- the replay derives again with the same functions
// (cycleEntry, eligibleEntries, compositeThresholds), from the policy the
// Analyze events recorded.

// compositeInputs is what collectV2ModelRequest hands the composite that
// the recording keeps.
type compositeInputs struct {
	namespace, modelID string
	// named is every entry runAnalyzers built; grace the entries of the
	// listed analyzers inside their staleness grace with no result.
	named, grace []allocation.NamedAnalyzerResult
	metadata     []domain.VariantMetadata
}

// recordComposite records one composite: its inputs and what it built.
// scaleUp is the threshold the warm pool's pressure was read at.
func (e *Engine) recordComposite(in compositeInputs, composite allocation.NamedAnalyzerResult,
	holdReasons map[string][]string, scaleUp float64) {
	e.learnRec.Record(enginerec.KindComposite, e.clock.Now(),
		compositeEvent(in, composite, holdReasons, scaleUp, e.replayedAnalyzers()))
}

// replayedAnalyzers is the names of the analyzers whose Analyze the learner
// recording carries (attachLearnerRecorder): a replay reproduces their
// results, so the composite event does not record them again.
func (e *Engine) replayedAnalyzers() map[string]bool {
	out := map[string]bool{}
	if _, ok := e.saturationV2Analyzer.(*saturation.SaturationAnalyzer); ok {
		out[domain.SaturationAnalyzerName] = true
	}
	for _, entry := range e.analyzers {
		switch entry.analyzer.(type) {
		case *slo.Analyzer, *throughput.ThroughputAnalyzer:
			out[entry.name] = true
		}
	}
	return out
}

// compositeEvent is the event for one composite. An entry's result is kept
// only when the replay cannot reproduce it (replayed does not name it).
// The production path and the replay build the event here, so the two
// cannot drift.
func compositeEvent(in compositeInputs, composite allocation.NamedAnalyzerResult, holdReasons map[string][]string,
	scaleUp float64, replayed map[string]bool) enginerec.CompositeEvent {
	ev := enginerec.CompositeEvent{
		Namespace:   in.namespace,
		ModelID:     in.modelID,
		Grace:       in.grace,
		Metadata:    enginerec.MetaOf(in.metadata),
		HoldReasons: holdReasons,
		Supply:      warmPoolSupply(composite),
		Pressure:    variantPressure(composite, scaleUp),
	}
	for _, nr := range in.named {
		entry := enginerec.CompositeEntry{Name: nr.Name, Live: nr.Live}
		if !replayed[nr.Name] {
			entry.Result = learnrec.ResultOf(nr.Result)
		}
		ev.Entries = append(ev.Entries, entry)
	}
	ev.Composite = composite
	ev.Composite.Result = learnrec.ResultOf(composite.Result)
	return ev
}

// The engine replay's per-path counters (analyzer-learner.md, 2.5): how often
// each composite path ran in a replay. A golden set that never reaches a path
// pins nothing about it.
const (
	// PathCompositeBuilt counts composites built.
	PathCompositeBuilt = "composite-built"
	// PathCompositeRoleCapacities counts composites of a P/D model, priced
	// per role.
	PathCompositeRoleCapacities = "composite-role-capacities"
	// PathCompositeHold counts composites that hold a role.
	PathCompositeHold = "composite-hold"
	// PathCompositeReleaseBound counts composites in which another
	// analyzer's frame bounds a role's release.
	PathCompositeReleaseBound = "composite-release-bound"
	// PathCompositeExternalResult counts composites built with an external
	// analyzer's result, read from the recording.
	PathCompositeExternalResult = "composite-external-result"
	// PathCompositeGrace counts composites built with a listed analyzer
	// inside its staleness grace and no result.
	PathCompositeGrace = "composite-grace"
	// PathCompositeReplicaClamped counts composites in which the
	// capacity-build step clamped an analyzer's replica count of a variant
	// to the scale target's current replicas (clampReplicaCountToScaleTarget).
	PathCompositeReplicaClamped = "composite-replica-clamped"
	// PathWarmPoolSupply counts composites with a variant a bridge serves.
	PathWarmPoolSupply = "warm-pool-supply"
	// PathPressureScaleUp counts composites with a variant pushed past its
	// scale-up threshold.
	PathPressureScaleUp = "pressure-needs-scale-up"
)

// LearnerReplayPathNames is every per-path counter LearnerReplay.PathCounts
// reports.
func LearnerReplayPathNames() []string {
	return []string{PathCompositeBuilt, PathCompositeRoleCapacities, PathCompositeHold, PathCompositeReleaseBound,
		PathCompositeExternalResult, PathCompositeGrace, PathCompositeReplicaClamped, PathWarmPoolSupply,
		PathPressureScaleUp}
}

// LearnerReplay replays the engine events of a learner recording: test
// support for the replay (internal/engines/analyzers/learnreplay), which
// cannot import this package. cmd/learnergolden and the replay's tests use
// it; nothing in the controller does.
//
// It rebuilds each composite from the analyzers' replayed results, the
// policy their Analyze events recorded, and what the event recorded of the
// engine's history, with the engine's own functions, and records it with the
// replay's recorder.
type LearnerReplay struct {
	paths map[string]int64
}

// NewLearnerReplay returns an engine replay with its counters at zero. One
// replay of one recording uses one.
func NewLearnerReplay() *LearnerReplay {
	return &LearnerReplay{paths: map[string]int64{}}
}

// Owns reports whether kind is an engine event this replay applies.
func (r *LearnerReplay) Owns(kind string) bool { return enginerec.Owns(kind) }

// PathCounts is how often each engine path ran in this replay, keyed by
// LearnerReplayPathNames.
func (r *LearnerReplay) PathCounts() map[string]int64 {
	out := make(map[string]int64, len(r.paths))
	for _, p := range LearnerReplayPathNames() {
		out[p] = r.paths[p]
	}
	return out
}

// Apply replays one engine event, at its clock, and records what it built
// with rec. replayed is what the replay has reproduced before it.
func (r *LearnerReplay) Apply(ctx context.Context, ev learnrec.RawEvent, replayed enginerec.Replayed,
	rec *learnrec.Recorder) error {
	if ev.Kind == enginerec.KindComposite {
		var d enginerec.CompositeEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		return r.composite(ctx, d, replayed, rec, ev)
	}
	return fmt.Errorf("an engine event the replay does not know: %s", ev.Kind)
}

func (r *LearnerReplay) composite(ctx context.Context, d enginerec.CompositeEvent, replayed enginerec.Replayed,
	rec *learnrec.Recorder, ev learnrec.RawEvent) error {
	policy, ok := replayed.Policy(d.Namespace, d.ModelID)
	if !ok || policy == nil {
		return errors.New("a composite with no Analyze of its model before it to read the policy from")
	}
	metadata := enginerec.Metadata(d.Metadata)
	metaByVariant := metadataByVariant(metadata)
	fromReplay := map[string]bool{}
	named := make([]allocation.NamedAnalyzerResult, 0, len(d.Entries))
	clamped := false
	for _, entry := range d.Entries {
		res := entry.Result
		if res == nil {
			fromReplay[entry.Name] = true
			var ok bool
			if res, ok = replayed.Result(entry.Name, d.Namespace, d.ModelID); !ok {
				if entry.Name == domain.SaturationAnalyzerName {
					// The model fails the cycle before any composite
					// (runAnalyzers): there is none to record.
					return nil
				}
				// An analyzer that failed leaves no entry (runRegisteredAnalyzer).
				continue
			}
		}
		before := make([]int, len(res.VariantCapacities))
		for i, vc := range res.VariantCapacities {
			before[i] = vc.ReplicaCount
		}
		nr := cycleEntry(ctx, entry.Name, res, *policy, metaByVariant)
		nr.Live = entry.Live
		for i, vc := range nr.Result.VariantCapacities {
			clamped = clamped || i < len(before) && vc.ReplicaCount < before[i]
		}
		named = append(named, nr)
	}
	eligible := append(eligibleEntries(named, *policy), d.Grace...)
	scaleUp, scaleDown := compositeThresholds(*policy)
	composite, reasons := composeSignal(ctx, named, eligible, scaleUp, scaleDown)
	in := compositeInputs{namespace: d.Namespace, modelID: d.ModelID, named: named, grace: d.Grace, metadata: metadata}
	out := compositeEvent(in, composite, reasons, scaleUp, fromReplay)
	r.count(out)
	if clamped {
		r.paths[PathCompositeReplicaClamped]++
	}
	rec.Record(enginerec.KindComposite, ev.Now, out)
	return nil
}

// count adds one composite to the path counters.
func (r *LearnerReplay) count(ev enginerec.CompositeEvent) {
	r.paths[PathCompositeBuilt]++
	if len(ev.Composite.RoleCapacities) > 0 {
		r.paths[PathCompositeRoleCapacities]++
	}
	if len(ev.Composite.Holds) > 0 {
		r.paths[PathCompositeHold]++
	}
	if len(ev.Composite.ReleaseBound) > 0 {
		r.paths[PathCompositeReleaseBound]++
	}
	if slices.ContainsFunc(ev.Entries, func(e enginerec.CompositeEntry) bool { return e.Result != nil }) {
		r.paths[PathCompositeExternalResult]++
	}
	if len(ev.Grace) > 0 {
		r.paths[PathCompositeGrace]++
	}
	if slices.ContainsFunc(ev.Supply, func(s enginerec.WarmPoolSupply) bool { return s.Replicas > 0 }) {
		r.paths[PathWarmPoolSupply]++
	}
	if slices.ContainsFunc(ev.Pressure, func(p enginerec.VariantPressure) bool { return p.NeedsScaleUp }) {
		r.paths[PathPressureScaleUp]++
	}
}
