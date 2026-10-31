package steadystate

import (
	"context"
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

// compositeInputs is what collectV2ModelRequest hands the composite.
type compositeInputs struct {
	namespace, modelID string
	// named is every entry runAnalyzers built; eligible the entries the
	// composite may size from, grace last; grace the entries of the listed
	// analyzers inside their staleness grace with no result.
	named, eligible, grace []allocation.NamedAnalyzerResult
	metadata               []domain.VariantMetadata
	scaleUp, scaleDown     float64
}

// recordComposite records one composite: its inputs and what it built.
func (e *Engine) recordComposite(in compositeInputs, composite allocation.NamedAnalyzerResult, holdReasons map[string][]string) {
	e.learnRec.Record(enginerec.KindComposite, e.clock.Now(),
		compositeEvent(in, composite, holdReasons, e.replayedAnalyzers()))
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
	replayed map[string]bool) enginerec.CompositeEvent {
	ev := enginerec.CompositeEvent{
		Namespace:   in.namespace,
		ModelID:     in.modelID,
		Grace:       in.grace,
		Metadata:    in.metadata,
		ScaleUp:     in.scaleUp,
		ScaleDown:   in.scaleDown,
		HoldReasons: holdReasons,
		Supply:      warmPoolSupply(composite),
		Pressure:    variantPressure(composite, in.scaleUp),
	}
	for _, nr := range in.named {
		entry := enginerec.CompositeEntry{Name: nr.Name, Score: nr.Score, ScaleUp: nr.ScaleUpThreshold,
			ScaleDown: nr.ScaleDownBoundary, Live: nr.Live}
		if !replayed[nr.Name] {
			entry.Result = learnrec.ResultOf(nr.Result)
		}
		ev.Entries = append(ev.Entries, entry)
	}
	for _, nr := range in.eligible[:len(in.eligible)-len(in.grace)] {
		ev.Eligible = append(ev.Eligible, nr.Name)
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
		PathWarmPoolSupply, PathPressureScaleUp}
}

// LearnerReplay replays the engine events of a learner recording: test
// support for the replay (internal/engines/analyzers/learnreplay), which
// cannot import this package. cmd/learnergolden and the replay's tests use
// it; nothing in the controller does.
//
// It rebuilds each composite from the inputs the engine supplied and the
// analyzers' results the replay reproduced, with the engine's own functions,
// and records it with the replay's recorder.
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
// with rec. results is the replay's own result of an analyzer's last Analyze
// of a model, false when that Analyze failed or never ran.
func (r *LearnerReplay) Apply(ctx context.Context, ev learnrec.RawEvent,
	results func(analyzer, namespace, modelID string) (*domain.AnalyzerResult, bool), rec *learnrec.Recorder) error {
	if ev.Kind == enginerec.KindComposite {
		var d enginerec.CompositeEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		return r.composite(ctx, d, results, rec, ev)
	}
	return fmt.Errorf("an engine event the replay does not know: %s", ev.Kind)
}

func (r *LearnerReplay) composite(ctx context.Context, d enginerec.CompositeEvent,
	results func(analyzer, namespace, modelID string) (*domain.AnalyzerResult, bool), rec *learnrec.Recorder,
	ev learnrec.RawEvent) error {
	metaByVariant := metadataByVariant(d.Metadata)
	replayed := map[string]bool{}
	named := make([]allocation.NamedAnalyzerResult, 0, len(d.Entries))
	for _, entry := range d.Entries {
		res := entry.Result
		if res == nil {
			replayed[entry.Name] = true
			var ok bool
			if res, ok = results(entry.Name, d.Namespace, d.ModelID); !ok {
				if entry.Name == domain.SaturationAnalyzerName {
					// The model fails the cycle before any composite
					// (runAnalyzers): there is none to record.
					return nil
				}
				// An analyzer that failed leaves no entry (runRegisteredAnalyzer).
				continue
			}
		}
		nr := namedResultScored(ctx, entry.Name, res, entry.Score, metaByVariant, entry.ScaleUp, entry.ScaleDown)
		nr.Live = entry.Live
		named = append(named, nr)
	}
	eligible := make([]allocation.NamedAnalyzerResult, 0, len(d.Eligible)+len(d.Grace))
	for _, name := range d.Eligible {
		if i := slices.IndexFunc(named, func(nr allocation.NamedAnalyzerResult) bool { return nr.Name == name }); i >= 0 {
			eligible = append(eligible, named[i])
		}
	}
	eligible = append(eligible, d.Grace...)
	in := compositeInputs{namespace: d.Namespace, modelID: d.ModelID, named: named, eligible: eligible,
		grace: d.Grace, metadata: d.Metadata, scaleUp: d.ScaleUp, scaleDown: d.ScaleDown}
	composite, reasons := composeSignal(ctx, named, eligible, d.ScaleUp, d.ScaleDown)
	out := compositeEvent(in, composite, reasons, replayed)
	r.count(out)
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
	if slices.ContainsFunc(ev.Supply, func(s enginerec.WarmPoolSupply) bool { return s.Replicas > 0 }) {
		r.paths[PathWarmPoolSupply]++
	}
	if slices.ContainsFunc(ev.Pressure, func(p enginerec.VariantPressure) bool { return p.NeedsScaleUp }) {
		r.paths[PathPressureScaleUp]++
	}
}
