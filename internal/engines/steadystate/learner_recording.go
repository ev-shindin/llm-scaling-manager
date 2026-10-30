package steadystate

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The learner recorder's wiring (docs/proposals/analyzer-learner.md, 2.4).
// Off unless LearnerRecording names a directory. One recording per
// controller start: it opens when the optimize loop starts, before its first
// cycle, and closes when the loop stops.

// startLearnerRecording opens a recording when LearnerRecording names a
// directory and none is open. A recording that cannot be opened is logged and
// the controller runs without one: a test aid never stops scaling.
//
// Only the first start of the loop opens one. A loop started again in the
// same process (leadership lost and regained) carries the analyzers' learned
// state over, which no start line can describe, so its cycles would not
// replay: it is logged and not recorded.
func (e *Engine) startLearnerRecording() {
	if e.LearnerRecording.Dir == "" || e.learnRec != nil {
		return
	}
	if e.learnRecStarted {
		ctrl.Log.Info("Learner recording not started again: a recording starts only at a controller start; restart the pod for a new one",
			"dir", e.LearnerRecording.Dir)
		return
	}
	rec, err := learnrec.Open(e.LearnerRecording, e.clock.Now())
	if err != nil {
		ctrl.Log.Error(err, "Learner recording not started", "dir", e.LearnerRecording.Dir)
		return
	}
	e.recordLearners(rec)
}

// stopLearnerRecording closes the recording, if one is open, and takes it
// back from the store and the analyzers, so nothing writes to it after.
func (e *Engine) stopLearnerRecording() {
	if e.learnRec == nil {
		return
	}
	rec := e.learnRec
	e.attachLearnerRecorder(nil)
	e.learnRec = nil
	if err := rec.Close(); err != nil {
		ctrl.Log.Error(err, "Closing the learner recording")
	}
}

// attachLearnerRecorder hands rec, nil included, to the capacity store and
// every built-in analyzer registered.
func (e *Engine) attachLearnerRecorder(rec *learnrec.Recorder) {
	if e.capacityStore != nil {
		e.capacityStore.WithRecorder(rec)
	}
	if sat, ok := e.saturationV2Analyzer.(*saturation.SaturationAnalyzer); ok {
		sat.WithRecorder(rec)
	}
	for _, entry := range e.analyzers {
		switch a := entry.analyzer.(type) {
		case *slo.Analyzer:
			a.WithRecorder(rec)
		case *throughput.ThroughputAnalyzer:
			a.WithRecorder(rec)
		}
	}
}

// recordLearners hands rec to the capacity store and to every built-in
// analyzer registered, and writes the recording's first line: how they were
// configured. Called before the first cycle, so the recording starts at a
// controller start, with an empty store.
func (e *Engine) recordLearners(rec *learnrec.Recorder) {
	e.learnRec = rec
	e.learnRecStarted = true
	e.attachLearnerRecorder(rec)
	start := learnrec.Start{Commit: learnrec.Commit()}
	if e.capacityStore != nil {
		start.StoreRecords = e.capacityStore.Len()
	}
	if sat, ok := e.saturationV2Analyzer.(*saturation.SaturationAnalyzer); ok {
		h := sat.Horizons()
		start.Saturation = learnrec.SaturationStart{EpisodeGap: h.EpisodeGap, VariantTimeout: h.VariantTimeout,
			BucketRetention: h.BucketRetention, BorrowTickGap: sat.BorrowTickGap()}
	}
	for _, entry := range e.analyzers {
		switch a := entry.analyzer.(type) {
		case *slo.Analyzer:
			start.SLO = &learnrec.SLOStart{StateTimeout: a.StateTimeout(), SlopeHoldFor: a.SlopeHoldFor(), Judging: a.Judging()}
		case *throughput.ThroughputAnalyzer:
			start.Throughput = true
		}
	}
	rec.Record(learnrec.KindStart, e.clock.Now(), start)
}
