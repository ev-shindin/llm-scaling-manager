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
func (e *Engine) startLearnerRecording() {
	if e.LearnerRecording.Dir == "" || e.learnRec != nil {
		return
	}
	rec, err := learnrec.Open(e.LearnerRecording, e.clock.Now())
	if err != nil {
		ctrl.Log.Error(err, "Learner recording not started", "dir", e.LearnerRecording.Dir)
		return
	}
	e.recordLearners(rec)
}

// stopLearnerRecording closes the recording, if one is open.
func (e *Engine) stopLearnerRecording() {
	if e.learnRec == nil {
		return
	}
	if err := e.learnRec.Close(); err != nil {
		ctrl.Log.Error(err, "Closing the learner recording")
	}
}

// recordLearners hands rec to the capacity store and to every built-in
// analyzer registered, and writes the recording's first line: how they were
// configured. Called before the first cycle, so the recording starts at a
// controller start, with an empty store.
func (e *Engine) recordLearners(rec *learnrec.Recorder) {
	e.learnRec = rec
	var start learnrec.Start
	if e.capacityStore != nil {
		start.StoreRecords = e.capacityStore.Len()
		e.capacityStore.WithRecorder(rec)
	}
	if sat, ok := e.saturationV2Analyzer.(*saturation.SaturationAnalyzer); ok {
		sat.WithRecorder(rec)
		h := sat.Horizons()
		start.Saturation = learnrec.SaturationStart{EpisodeGap: h.EpisodeGap, VariantTimeout: h.VariantTimeout,
			BucketRetention: h.BucketRetention, BorrowTickGap: sat.BorrowTickGap()}
	}
	for _, entry := range e.analyzers {
		switch a := entry.analyzer.(type) {
		case *slo.Analyzer:
			a.WithRecorder(rec)
			start.SLO = &learnrec.SLOStart{StateTimeout: a.StateTimeout(), SlopeHoldFor: a.SlopeHoldFor(), Judging: a.Judging()}
		case *throughput.ThroughputAnalyzer:
			a.WithRecorder(rec)
			start.Throughput = true
		}
	}
	rec.Record(learnrec.KindStart, e.clock.Now(), start)
}
