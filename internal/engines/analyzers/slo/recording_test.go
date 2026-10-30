package slo

import (
	"testing"
	"time"
)

// With recording off the recorder costs a nil test: a wrapper allocates
// exactly what the method behind it does.
func TestRecordingOff_CostsNoAllocation(t *testing.T) {
	a := New(nil, nil)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	names := []string{"v"}
	cases := map[string]struct{ wrapper, impl func() }{
		"BeginCycle":          {func() { a.BeginCycle() }, func() { a.beginCycle() }},
		"EvictStale":          {func() { a.EvictStale(at) }, func() { a.evictStale(at) }},
		"NoteVariantsPresent": {func() { a.NoteVariantsPresent("ns", "m", names) }, func() { a.noteVariantsPresent("ns", "m", names) }},
	}
	for name, c := range cases {
		if w, i := testing.AllocsPerRun(100, c.wrapper), testing.AllocsPerRun(100, c.impl); w != i {
			t.Errorf("%s allocates %v with recording off, the method behind it %v", name, w, i)
		}
	}
}
