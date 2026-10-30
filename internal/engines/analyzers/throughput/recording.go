package throughput

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The analyzer's recorder (docs/proposals/analyzer-learner.md, 2.4): each
// exported method the engine may call records the call and what it returned,
// and does the unexported method behind it. Analyze observes through the
// unexported observe, so an Analyze is one event. With recording off (a nil
// recorder) each wrapper is one nil test and the call.

// The kinds of event this analyzer records.
const (
	KindAnalyze = "throughput.Analyze"
	KindObserve = "throughput.Observe"
)

// AnalyzeEvent is one Analyze: its input and what it returned. Written
// whether or not it erred. The analyzer reads no capacity store.
type AnalyzeEvent struct {
	// Input is a learnrec.Input, the rows raw.
	Input  learnrec.Raw
	Result *domain.AnalyzerResult
	Error  string
}

// ObserveEvent is Observe's arguments and the reports it returned.
type ObserveEvent struct {
	Now                time.Time
	ModelID, Namespace string
	// Metrics is a []domain.ReplicaMetrics, encoded before the call.
	Metrics learnrec.Raw
	Reports map[string]SanityReport
}

// WithRecorder records the analyzer's calls to rec; nil turns recording off.
// Call it before the analyzer is used.
func (a *ThroughputAnalyzer) WithRecorder(rec *learnrec.Recorder) *ThroughputAnalyzer {
	a.rec = rec
	return a
}

// SetSwitchesForReplay sets the package's replay switches by name: the test
// seam a replay drives one named change through (analyzer-learner.md, 2.5).
// No switch exists yet, so the only set it accepts is the empty one. Never
// called on a production path; a test holds that.
func (a *ThroughputAnalyzer) SetSwitchesForReplay(switches map[string]bool) error {
	return learnrec.UnknownSwitches("throughput", switches)
}

// Analyze implements domain.Analyzer (analyze), and records the call.
func (a *ThroughputAnalyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if !a.rec.Active() {
		return a.analyze(ctx, input)
	}
	now := a.clock.Now()
	in := a.rec.Fragment(learnrec.InputOf(input))
	res, err := a.analyze(ctx, input)
	a.rec.Record(KindAnalyze, now, AnalyzeEvent{Input: in, Result: byVariantName(learnrec.ResultOf(res)), Error: learnrec.ErrorOf(err)})
	return res, err
}

// Observe processes one reconcile cycle for a model (observeDefaultBand), and records
// the call. The engine does not call it today; Analyze observes on its own.
func (a *ThroughputAnalyzer) Observe(
	ctx context.Context,
	now time.Time,
	modelID, namespace string,
	metrics []domain.ReplicaMetrics,
) map[string]SanityReport {
	if !a.rec.Active() {
		return a.observeDefaultBand(ctx, now, modelID, namespace, metrics)
	}
	rows := a.rec.Fragment(metrics)
	reports := a.observeDefaultBand(ctx, now, modelID, namespace, metrics)
	a.rec.Record(KindObserve, a.clock.Now(), ObserveEvent{Now: now, ModelID: modelID, Namespace: namespace,
		Metrics: rows, Reports: sortedReports(reports)})
	return reports
}

// byVariantName is r with its variant capacities sorted by variant name, on
// a copy: Analyze builds them in map order, which no output may depend on.
func byVariantName(r *domain.AnalyzerResult) *domain.AnalyzerResult {
	if r == nil {
		return nil
	}
	r.VariantCapacities = slices.Clone(r.VariantCapacities)
	slices.SortStableFunc(r.VariantCapacities, func(x, y domain.VariantCapacity) int {
		return strings.Compare(x.VariantName, y.VariantName)
	})
	return r
}

// sortedReports is reports with each one's issues sorted, on a copy:
// CheckModelMetrics lists them in map order.
func sortedReports(reports map[string]SanityReport) map[string]SanityReport {
	out := make(map[string]SanityReport, len(reports))
	for v, r := range reports {
		r.Issues = slices.Clone(r.Issues)
		slices.Sort(r.Issues)
		out[v] = r
	}
	return out
}
