package decision

import (
	"testing"
	"time"
)

// A pool that stops reconciling must stop holding GPUs free: a figure older
// than the bound is not believed. An empty publish clears at once, and the
// store keeps its own copy.
func TestWarmPoolUnheldStore(t *testing.T) {
	s, t0 := &WarmPoolUnheldStore{}, time.Unix(1000, 0)
	in := map[string]int{"A100": 2}
	s.Publish("ns", in, t0)
	in["A100"] = 99
	if got := s.Latest(WarmPoolUnheldMaxAge, t0.Add(WarmPoolUnheldMaxAge))["ns"]["A100"]; got != 2 {
		t.Fatalf("at the bound: got %d, want 2 (and the caller's later write must not reach the store)", got)
	}
	if got := s.Latest(WarmPoolUnheldMaxAge, t0.Add(WarmPoolUnheldMaxAge+time.Nanosecond)); len(got) != 0 {
		t.Fatalf("past the bound: got %v, want nothing", got)
	}
	s.Publish("ns", nil, t0)
	if got := s.Latest(time.Hour, t0); len(got) != 0 {
		t.Fatalf("an empty publish must clear the namespace, got %v", got)
	}
}
