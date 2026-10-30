package capacity

import (
	"fmt"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// What the store records (docs/proposals/analyzer-learner.md, 2.4): the
// engine's writes only, as what the engine supplied. The analyzer's own writes
// (Update, Delete) are reproduced by replaying the analyzer, and recording
// them too would apply them twice.

const (
	// KindFileScaleTarget is FileScaleTarget, with FileScaleTargetEvent.
	KindFileScaleTarget = "store.FileScaleTarget"
	// KindEvictStale is EvictStale, with EvictStaleEvent.
	KindEvictStale = "store.EvictStale"
)

// FileScaleTargetEvent is FileScaleTarget's arguments; its now is the event's.
type FileScaleTargetEvent struct {
	Namespace, ModelID, Variant string
	// Accelerator is what the engine passed: the saturation analyzer's
	// stabilised answer, which a replay applies as recorded.
	Accelerator string
	GpuCount    int
	Params      EngineParams
	RollingOut  bool
}

// EvictStaleEvent is EvictStale's timeout and what it removed. A replay
// recomputes the removal; Removed is a check on it.
type EvictStaleEvent struct {
	Timeout time.Duration
	Removed int
}

// WithRecorder records the store's engine-side writes to rec; nil turns
// recording off. Call it before the store is used.
func (s *Store) WithRecorder(rec *learnrec.Recorder) *Store {
	s.rec = rec
	return s
}

// Len is how many records the store holds.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}

// RecordsOf is each named variant's record in store as it stands, null where
// there is none: what an analyzer's recorder writes beside an Analyze, read
// before Analyze can change it. A nil store has no records.
func RecordsOf(store *Store, namespace, modelID string, variants []string) map[string]*Record {
	out := make(map[string]*Record, len(variants))
	for _, v := range variants {
		if store == nil {
			out[v] = nil
			continue
		}
		out[v] = store.Get(namespace, modelID, v)
	}
	return out
}

// EngineSuppliedDiff compares the fields of two records the engine supplies
// (FileScaleTarget) by the store's own rules, not by field equality: the
// engine arguments by ArgsFingerprint and the image, the hardware by
// sameHardware, and RollingOut exactly. It returns what differs, empty when
// nothing does or when the two were not written by the same writer
// (LearnedFrom), where the live record's fields are the analyzer's and not
// the engine's. A nil record matches only a nil record.
func EngineSuppliedDiff(got, want *Record) string {
	switch {
	case got == nil && want == nil:
		return ""
	case got == nil:
		return "no record"
	case want == nil:
		return "a record where the run had none"
	case got.LearnedFrom != want.LearnedFrom:
		return ""
	}
	var diffs []string
	switch {
	case (got.EngineParams == nil) != (want.EngineParams == nil):
		diffs = append(diffs, "EngineParams present on one side only")
	case got.EngineParams != nil:
		if got.EngineParams.ArgsFingerprint() != want.EngineParams.ArgsFingerprint() {
			diffs = append(diffs, "engine arguments (ArgsFingerprint)")
		}
		if got.EngineParams.Image != want.EngineParams.Image {
			diffs = append(diffs, fmt.Sprintf("image %q, want %q", got.EngineParams.Image, want.EngineParams.Image))
		}
	}
	if !sameHardware(got, want.AcceleratorName, want.GpuCount) {
		diffs = append(diffs, fmt.Sprintf("hardware %s x%d, want %s x%d", got.AcceleratorName, got.GpuCount, want.AcceleratorName, want.GpuCount))
	}
	if got.RollingOut != want.RollingOut {
		diffs = append(diffs, fmt.Sprintf("RollingOut %v, want %v", got.RollingOut, want.RollingOut))
	}
	if len(diffs) == 0 {
		return ""
	}
	return strings.Join(diffs, "; ")
}

// SetSwitchesForReplay sets the package's replay switches by name: the test
// seam a replay drives one named change through (analyzer-learner.md, 2.5).
// No switch exists yet, so the only set it accepts is the empty one. Never
// called on a production path; a test holds that.
func (s *Store) SetSwitchesForReplay(switches map[string]bool) error {
	return learnrec.UnknownSwitches("capacity", switches)
}
