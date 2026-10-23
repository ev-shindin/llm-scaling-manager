package balance

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// cycle is the controller's default optimize interval (GLOBAL_OPT_INTERVAL).
const cycle = 15 * time.Second

func imbalanced() map[string]Distribution {
	return map[string]Distribution{domain.RolePrefill: {Role: domain.RolePrefill, Ready: 4, Saturated: 1, Idle: 3}}
}

func balanced() map[string]Distribution {
	return map[string]Distribution{domain.RolePrefill: {Role: domain.RolePrefill, Ready: 4}}
}

func only(t *testing.T, rs []Reading) Reading {
	t.Helper()
	if len(rs) != 1 {
		t.Fatalf("%d readings, want 1", len(rs))
	}
	return rs[0]
}

// clock steps one cycle per call.
type clock struct{ now time.Time }

func (c *clock) tick() time.Time {
	c.now = c.now.Add(cycle)
	return c.now
}

// sustainCycles and clearCycles are how many cycles span SustainFor and
// ClearAfter, counting the first.
var (
	sustainCycles = int(SustainFor/cycle) + 1
	clearCycles   = int(ClearAfter/cycle) + 1
)

// An imbalance is reported once it has held on every cycle for SustainFor,
// once; it clears once it has read balanced on every cycle for ClearAfter,
// once.
func TestTrackerWarnsOncePerChangeAfterSustainFor(t *testing.T) {
	tr, c := NewTracker(), &clock{now: t0}
	sustainAt := sustainCycles
	for i := 1; i <= sustainAt+2; i++ {
		r := only(t, tr.Observe("ns/m", imbalanced(), c.tick()))
		if want := i >= sustainAt; r.Sustained != want || r.Changed != (i == sustainAt) {
			t.Fatalf("imbalanced cycle %d: sustained/changed = %v/%v, want %v/%v", i, r.Sustained, r.Changed, want, i == sustainAt)
		}
	}
	clearAt := clearCycles
	for i := 1; i <= clearAt+2; i++ {
		r := only(t, tr.Observe("ns/m", balanced(), c.tick()))
		if want := i < clearAt; r.Sustained != want || r.Changed != (i == clearAt) {
			t.Fatalf("balanced cycle %d: sustained/changed = %v/%v, want %v/%v", i, r.Sustained, r.Changed, want, i == clearAt)
		}
	}
}

// Probe (review r1): the debounce was two cycles, 15 s, against KV and queue
// figures that are one-minute peaks, so one burst, or a scale-up whose old
// replicas are full beside a new empty one, warned within two cycles. Two
// cycles, or any streak shorter than SustainFor, must not report.
func TestTrackerTwoCyclesAreNotSustained(t *testing.T) {
	if SustainFor <= time.Minute {
		t.Fatalf("SustainFor %v does not exceed the one-minute window the figures are peaks over", SustainFor)
	}
	tr, c := NewTracker(), &clock{now: t0}
	for i := 1; i < sustainCycles; i++ {
		if r := only(t, tr.Observe("ns/m", imbalanced(), c.tick())); r.Sustained {
			t.Fatalf("reported imbalanced after %d cycles (%v), under SustainFor", i, time.Duration(i-1)*cycle)
		}
	}
}

// Before it is reported, one balanced cycle restarts the streak.
func TestTrackerResetsAnUnreportedStreakOnABalancedCycle(t *testing.T) {
	tr, c := NewTracker(), &clock{now: t0}
	half := sustainCycles - 2
	for round := 0; round < 3; round++ {
		for i := 0; i < half; i++ {
			if r := only(t, tr.Observe("ns/m", imbalanced(), c.tick())); r.Sustained {
				t.Fatalf("round %d: an imbalance broken by a balanced cycle was reported", round)
			}
		}
		tr.Observe("ns/m", balanced(), c.tick())
	}
}

// Probe (review r1): once reported, one balanced cycle (the saturated
// replica's peak dipping to 0.89) cleared it, and the gauge's drop reset the
// alert's 10-minute timer: a persistent imbalance paged, resolved and paged
// again, or never paged. A dip shorter than ClearAfter keeps it reported.
func TestTrackerKeepsAReportedImbalanceThroughAShortDip(t *testing.T) {
	tr, c := NewTracker(), &clock{now: t0}
	for i := 0; i < sustainCycles; i++ {
		tr.Observe("ns/m", imbalanced(), c.tick())
	}
	for round := 0; round < 3; round++ {
		r := only(t, tr.Observe("ns/m", balanced(), c.tick()))
		if !r.Sustained || r.Changed {
			t.Fatalf("round %d: one balanced cycle cleared a reported imbalance", round)
		}
		for i := 0; i < 4; i++ {
			if r := only(t, tr.Observe("ns/m", imbalanced(), c.tick())); !r.Sustained || r.Changed {
				t.Fatalf("round %d: the imbalance resumed and was reported again", round)
			}
		}
	}
}

// A role that parks and returns starts from zero, and so does a forgotten
// model; models are tracked apart.
func TestTrackerForgetsAbsentRolesAndModels(t *testing.T) {
	sustain := func(tr *Tracker, c *clock, key string) {
		for i := 0; i < sustainCycles; i++ {
			tr.Observe(key, imbalanced(), c.tick())
		}
	}
	tr, c := NewTracker(), &clock{now: t0}
	sustain(tr, c, "ns/m")
	if rs := tr.Observe("ns/m", nil, c.tick()); len(rs) != 0 {
		t.Fatalf("readings for a model with no ready replica: %v", rs)
	}
	if r := only(t, tr.Observe("ns/m", imbalanced(), c.tick())); r.Sustained {
		t.Error("a role back from zero kept its old streak")
	}
	sustain(tr, c, "ns/other")
	tr.Retain(map[string]bool{"ns/other": true})
	if r := only(t, tr.Observe("ns/m", imbalanced(), c.tick())); r.Sustained {
		t.Error("Retain kept a model it was not given")
	}
	if r := only(t, tr.Observe("ns/other", imbalanced(), c.tick())); !r.Sustained {
		t.Error("Retain dropped a model it was given")
	}
	tr.Forget("ns/other")
	if r := only(t, tr.Observe("ns/other", imbalanced(), c.tick())); r.Sustained {
		t.Error("Forget kept the model's streak")
	}
}
