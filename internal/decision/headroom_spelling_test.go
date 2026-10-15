package decision

import (
	"testing"
	"time"
)

// A namespace allowlist that spells the short name bounds a pool that asks by
// the long one: the two name one accelerator. Matching them by exact string
// read the allowance as 0 and the pool never grew.
func TestReadingMatchesNamespaceAllowlistByIdentity(t *testing.T) {
	s := NewHeadroomStore()
	now := time.Now()
	s.Publish(map[string]map[string]int{"ns": {"H100": 4}}, 0, now)

	if free, state := s.Reading("ns", "NVIDIA-H100-80GB-HBM3", 0, time.Minute, now); state != HeadroomBounded || free != 4 {
		t.Errorf("long spelling against a short allowlist reads (%d, %v), want (4, bounded)", free, state)
	}
	if free, state := s.Reading("ns", "NVIDIA-A100-SXM4-80GB", 0, time.Minute, now); state != HeadroomBounded || free != 0 {
		t.Errorf("an accelerator the allowlist does not name reads (%d, %v), want (0, bounded)", free, state)
	}

	s.Publish(map[string]map[string]int{"ns": {"NVIDIA-H100-80GB-HBM3": -1}}, 0, now)
	if _, state := s.Reading("ns", "H100", 0, time.Minute, now); state != HeadroomUnbounded {
		t.Errorf("short spelling against an unlimited long entry reads %v, want unbounded", state)
	}
}
