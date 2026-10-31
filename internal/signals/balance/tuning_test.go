package balance

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The policy's loadImbalance figures replace the constants: a replica at 0.40
// is idle under an idleKvUsage of 0.50 and not under the default 0.30, and one
// Ready for 3 minutes is past a 2-minute grace and inside a 5-minute one.
func TestThresholdsTuneTheIdleTest(t *testing.T) {
	rows := []domain.ReplicaMetrics{row("pre-0", "pre", 0.20, 439), row("pre-1", "pre", 0.40, 0)}
	idle := func(th Thresholds) int { return Measure(rows, pd, th, t0)[domain.RolePrefill].Idle }
	if got := idle(policy); got != 0 {
		t.Errorf("default idleKvUsage: %d idle, want 0", got)
	}
	tuned := policy
	tuned.IdleKvUsage = 0.50
	if got := idle(tuned); got != 1 {
		t.Errorf("idleKvUsage 0.50: %d idle, want 1", got)
	}

	recent := []domain.ReplicaMetrics{row("pre-0", "pre", 0.20, 439), row("pre-1", "pre", 0.01, 0)}
	recent[1].ReadySince = t0.Add(-3 * time.Minute)
	grace := func(th Thresholds) int { return Measure(recent, pd, th, t0)[domain.RolePrefill].Idle }
	long := policy
	long.IdleAfterReady = 5 * time.Minute
	if a, b := grace(policy), grace(long); a != 1 || b != 0 {
		t.Errorf("Ready 3m ago: idle %d under the default grace, %d under 5m; want 1 and 0", a, b)
	}
}

// The tracker reports, clears and goes stale on the durations of the
// thresholds the model was last measured with.
func TestTrackerTakesItsTimingsFromMeasure(t *testing.T) {
	th := policy
	th.SustainFor, th.ClearAfter = 4*time.Minute, 3*time.Minute
	measured := func() testTracker {
		tr := newTestTracker()
		tr.Measure("ns/m", nil, pd, th, t0)
		return tr
	}
	// sustainedAfter counts the cycles until the role is reported.
	sustainedAfter := func(tr testTracker) int {
		c := &clock{now: t0}
		for i := 1; i <= 100; i++ {
			if only(t, tr.Observe("ns/m", imbalanced(), c.tick())).Sustained {
				return i
			}
		}
		return -1
	}
	if def, tuned := sustainedAfter(newTestTracker()), sustainedAfter(measured()); def != sustainCycles || tuned != int(4*time.Minute/cycle)+1 {
		t.Errorf("reported after %d cycles by default and %d at sustainFor 4m; want %d and %d",
			def, tuned, sustainCycles, int(4*time.Minute/cycle)+1)
	}

	tr := measured()
	c := &clock{now: t0}
	for range int(4*time.Minute/cycle) + 1 {
		tr.Observe("ns/m", imbalanced(), c.tick())
	}
	cleared := 0
	for i := 1; i <= 100; i++ {
		if !only(t, tr.Observe("ns/m", balanced(), c.tick())).Sustained {
			cleared = i
			break
		}
	}
	if want := int(3*time.Minute/cycle) + 1; cleared != want {
		t.Errorf("cleared after %d balanced cycles at clearAfter 3m, want %d", cleared, want)
	}

	// Stale after sustainFor: 3 minutes of failures is not 4.
	stale := measured()
	if stale.Failed("ns/m", t0) || stale.Failed("ns/m", t0.Add(3*time.Minute)) {
		t.Error("stale before sustainFor (4m)")
	}
	if !stale.Failed("ns/m", t0.Add(4*time.Minute)) {
		t.Error("not stale at sustainFor (4m)")
	}
	if def := newTestTracker(); def.Failed("ns/m", t0) || !def.Failed("ns/m", t0.Add(StaleAfter)) {
		t.Error("the default tracker is not stale at StaleAfter")
	}
}
