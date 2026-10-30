package slo

import (
	"context"
	"fmt"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The analyzer's recorder (docs/proposals/analyzer-learner.md, 2.4): each
// exported method the engine calls records the call and what it returned, and
// does the unexported method of the same name. With recording off (a nil
// recorder) each wrapper is one nil test and the call.

// The kinds of event this analyzer records.
const (
	KindAnalyze             = "slo.Analyze"
	KindEvictStale          = "slo.EvictStale"
	KindBeginCycle          = "slo.BeginCycle"
	KindNoteVariantsPresent = "slo.NoteVariantsPresent"
)

// judged is what one Analyze read from the router and published, beside its
// result.
type judged struct {
	// Latency is what the router returned; RouterCalled is whether it was
	// asked at all (a policy with no objective does not ask).
	Latency      *domain.RouterLatency
	RouterCalled bool
	// Published is the snapshot handed to metrics.PublishSLO: the judged
	// objectives and each variant's pricing, an output like the result.
	Published metrics.SLOModelSnapshot
}

// AnalyzeEvent is one Analyze: its input, the capacity records it read, the
// router latency it was served, and what it returned and published. Written
// whether or not it erred.
type AnalyzeEvent struct {
	// Input is a learnrec.Input, the rows raw.
	Input learnrec.Raw
	// Records is each variant's capacity.Record before Analyze read it, by
	// variant name (a map[string]*capacity.Record). The analyzer reads only
	// their EngineParams.
	Records learnrec.Raw
	// RouterCalled is whether Analyze asked the router; RouterLatency is
	// what it answered, which a replay serves back instead of a query.
	RouterCalled  bool
	RouterLatency *domain.RouterLatency
	// Result is the result without AnalyzedAt; Published the SLO snapshot it
	// published; Error the error, if any.
	Result    *domain.AnalyzerResult
	Published *metrics.SLOModelSnapshot
	Error     string
}

// EvictStaleEvent is EvictStale's argument and what it evicted.
type EvictStaleEvent struct {
	Now     time.Time
	Evicted Evicted
}

// PresentEvent is NoteVariantsPresent's arguments.
type PresentEvent struct {
	Namespace, ModelID string
	Variants           []string
}

// WithRecorder records the analyzer's calls to rec; nil turns recording off.
// Call it before the analyzer is used.
func (a *Analyzer) WithRecorder(rec *learnrec.Recorder) *Analyzer {
	a.rec = rec
	return a
}

// SlopeHoldFor reports how long a line's slope may be held (WithSlopeHoldFor).
func (a *Analyzer) SlopeHoldFor() time.Duration { return a.slopeHoldFor }

// SetSwitchesForReplay sets the package's replay switches by name: the test
// seam a replay drives one named change through (analyzer-learner.md, 2.5).
// No switch exists yet, so the only set it accepts is the empty one. Never
// called on a production path; a test holds that.
func (a *Analyzer) SetSwitchesForReplay(switches map[string]bool) error {
	for name := range switches {
		return fmt.Errorf("slo: no replay switch %q", name)
	}
	return nil
}

// Analyze implements domain.Analyzer (analyze), and records the call.
func (a *Analyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if a.rec == nil {
		res, _, err := a.analyze(ctx, input)
		return res, err
	}
	now := a.now()
	in := a.rec.Fragment(learnrec.InputOf(input))
	records := a.rec.Fragment(capacity.RecordsOf(a.store, input.Namespace, input.ModelID, learnrec.VariantNames(input)))
	res, j, err := a.analyze(ctx, input)
	ev := AnalyzeEvent{Input: in, Records: records, RouterCalled: j.RouterCalled, RouterLatency: j.Latency,
		Result: learnrec.ResultOf(res), Error: learnrec.ErrorOf(err)}
	if err == nil {
		ev.Published = &j.Published
	}
	a.rec.Record(KindAnalyze, now, ev)
	return res, err
}

// EvictStale forgets the learned state not analyzed or reported present for
// the state timeout (evictStale), and records the call.
func (a *Analyzer) EvictStale(now time.Time) Evicted {
	ev := a.evictStale(now)
	if a.rec != nil {
		a.rec.Record(KindEvictStale, a.now(), EvictStaleEvent{Now: now, Evicted: ev})
	}
	return ev
}

// BeginCycle marks the start of an engine cycle (beginCycle), and records
// the call.
func (a *Analyzer) BeginCycle() {
	if a.rec != nil {
		a.rec.Record(KindBeginCycle, a.now(), struct{}{})
	}
	a.beginCycle()
}

// NoteVariantsPresent stamps the learned state of a model's variants that
// were discovered this cycle (noteVariantsPresent), and records the call.
func (a *Analyzer) NoteVariantsPresent(namespace, modelID string, variantNames []string) {
	if a.rec != nil {
		a.rec.Record(KindNoteVariantsPresent, a.now(), PresentEvent{Namespace: namespace, ModelID: modelID, Variants: variantNames})
	}
	a.noteVariantsPresent(namespace, modelID, variantNames)
}
