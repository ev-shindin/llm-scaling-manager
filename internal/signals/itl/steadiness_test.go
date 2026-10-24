package itl

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Observe judges steadiness by the gate it is given. A replica whose k
// moved 0.12 within the window is unsteady under the default drift of 0.10
// and steady under 0.20; a replica that joined 2.5 minutes ago is past the
// default settle of 2 minutes and still inside one of 3.
func TestObserveTunesTheSteadyGate(t *testing.T) {
	start := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	row := func(pod string, k float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: "v", Ready: true, KvUsageInstant: k, AvgITL: 0.02*k + 0.01}
	}
	drift := func(s Steadiness) int {
		tr := NewTracker()
		tr.Observe("key", "v", []domain.ReplicaMetrics{row("p", 0.30)}, Admission{Steady: s}, start)
		return tr.Observe("key", "v", []domain.ReplicaMetrics{row("p", 0.42)}, Admission{Steady: s}, start.Add(15*time.Second)).Unsteady
	}
	if def, wide := drift(DefaultSteadiness()), drift(Steadiness{MaxKDrift: 0.20}); def != 1 || wide != 0 {
		t.Errorf("unsteady readings after a 0.12 move: %d by default, %d at maxKDrift 0.20; want 1 and 0", def, wide)
	}

	settling := func(s Steadiness) int {
		tr := NewTracker()
		tr.Observe("key", "v", []domain.ReplicaMetrics{row("old", 0.30)}, Admission{Steady: s}, start)
		at := start.Add(15 * time.Second)
		tr.Observe("key", "v", []domain.ReplicaMetrics{row("old", 0.30), row("new", 0.30)}, Admission{Steady: s}, at)
		return tr.Observe("key", "v", []domain.ReplicaMetrics{row("old", 0.30), row("new", 0.30)}, Admission{Steady: s}, at.Add(150*time.Second)).Settling
	}
	if def, long := settling(Steadiness{}), settling(Steadiness{For: 3 * time.Minute}); def != 0 || long != 1 {
		t.Errorf("a joiner 2.5m in: %d settling by default, %d at steadyFor 3m; want 0 and 1", def, long)
	}
}
