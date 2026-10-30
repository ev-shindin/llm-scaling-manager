package capacity

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// With recording off the recorder costs a nil test: a wrapper allocates
// exactly what the method behind it does.
func TestRecordingOff_CostsNoAllocation(t *testing.T) {
	s := NewStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64}
	cases := map[string]struct{ wrapper, impl func() }{
		"FileScaleTarget": {func() { s.FileScaleTarget("ns", "m", "v", "H100", 1, p, false, at) },
			func() { s.fileScaleTarget("ns", "m", "v", "H100", 1, p, false, at) }},
		"EvictStale": {func() { s.EvictStale(at, time.Hour) }, func() { s.evictStale(at, time.Hour) }},
	}
	for name, c := range cases {
		if w, i := testing.AllocsPerRun(100, c.wrapper), testing.AllocsPerRun(100, c.impl); w != i {
			t.Errorf("%s allocates %v with recording off, the method behind it %v", name, w, i)
		}
	}
}

// EngineSuppliedDiff compares by the store's own rules, and only records one
// writer wrote.
func TestEngineSuppliedDiff(t *testing.T) {
	p := &EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, Image: "a"}
	rec := func(mut func(*Record)) *Record {
		r := &Record{AcceleratorName: "H100", GpuCount: 1, EngineParams: p, LearnedFrom: "deployment"}
		if mut != nil {
			mut(r)
		}
		return r
	}
	cases := []struct {
		name      string
		got, want *Record
		diff      string
	}{
		{"both nil", nil, nil, ""},
		{"none here", nil, rec(nil), "no record"},
		{"one here only", rec(nil), nil, "a record where"},
		{"the same", rec(nil), rec(nil), ""},
		{"another writer", rec(func(r *Record) { r.LearnedFrom = LearnedFromLive; r.RollingOut = true }), rec(nil), ""},
		{"an analyzer field", rec(func(r *Record) { r.EffectiveCapacity = 9 }), rec(nil), ""},
		{"rolling out", rec(func(r *Record) { r.RollingOut = true }), rec(nil), "RollingOut"},
		{"image", rec(func(r *Record) { q := *p; q.Image = "b"; r.EngineParams = &q }), rec(nil), "image"},
		{"arguments", rec(func(r *Record) { q := *p; q.MaxNumSeqs = 32; r.EngineParams = &q }), rec(nil), "ArgsFingerprint"},
		{"params on one side", rec(func(r *Record) { r.EngineParams = nil }), rec(nil), "one side"},
		{"hardware", rec(func(r *Record) { r.AcceleratorName = "A100" }), rec(nil), "hardware"},
		{"unknown hardware is no move", rec(func(r *Record) { r.GpuCount = 0 }), rec(nil), ""},
	}
	for _, c := range cases {
		got := EngineSuppliedDiff(c.got, c.want)
		if c.diff == "" && got != "" || c.diff != "" && !contains(got, c.diff) {
			t.Errorf("%s: %q, want %q", c.name, got, c.diff)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
