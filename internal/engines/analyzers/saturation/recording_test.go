package saturation

import (
	"bytes"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// With recording off the recorder costs a nil test: a wrapper allocates
// exactly what the method behind it does, and nothing is written.
func TestRecordingOff_CostsNoAllocation(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	names := []string{"v"}
	cases := map[string]struct{ wrapper, impl func() }{
		"HoldLowerBounds": {func() { a.HoldLowerBounds(true) }, func() { a.holdLowerBounds(true) }},
		"StableAcceleratorFor": {func() { a.StableAcceleratorFor("ns", "v", "H100") },
			func() { a.stableAcceleratorFor("ns", "v", "H100") }},
		"NoteVariantsPresent": {func() { a.NoteVariantsPresent("ns", "m", names) },
			func() { a.noteVariantsPresent("ns", "m", names) }},
		"ForgottenToken":      {func() { a.ForgottenToken("ns", "v") }, func() { a.forgottenToken("ns", "v") }},
		"TakeRestoreOutcomes": {func() { a.TakeRestoreOutcomes() }, func() { a.takeRestoreOutcomes() }},
	}
	for name, c := range cases {
		wrapper := testing.AllocsPerRun(100, c.wrapper)
		impl := testing.AllocsPerRun(100, c.impl)
		if wrapper != impl {
			t.Errorf("%s allocates %v with recording off, the method behind it %v", name, wrapper, impl)
		}
	}
	if got := testing.AllocsPerRun(100, func() { a.HoldLowerBounds(false) }); got != 0 {
		t.Errorf("HoldLowerBounds allocates %v with recording off, want 0", got)
	}
}

// The same calls with a recorder write one event each; with none, nothing.
func TestRecordingOff_WritesNothing(t *testing.T) {
	var buf bytes.Buffer
	on := NewSaturationAnalyzer(capacity.NewStore()).WithRecorder(learnrec.New(&buf, 0))
	off := NewSaturationAnalyzer(capacity.NewStore())
	for _, a := range []*SaturationAnalyzer{on, off} {
		a.HoldLowerBounds(true)
		a.NoteVariantsPresent("ns", "m", []string{"v"})
		a.EvictStaleHistory()
	}
	events, err := learnrec.Read(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("the recorded analyzer wrote %d events, want 3", len(events))
	}
	if off.rec != nil {
		t.Fatal("an analyzer built without a recorder has one")
	}
}
