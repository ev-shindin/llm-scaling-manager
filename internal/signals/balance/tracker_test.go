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

// replicaGraceCycles runs a saturated decode replica beside empty, which the
// tracker first sees on the first cycle, and returns how long after that
// cycle empty first read idle, and how long until the role was reported; -1
// for never within 20 minutes.
func replicaGraceCycles(empty domain.ReplicaMetrics, start time.Time) (idleAfter, reportedAfter time.Duration) {
	tr, c := NewTracker(), &clock{now: start}
	first := time.Time{}
	idleAfter, reportedAfter = -1, -1
	for range int(20 * time.Minute / cycle) {
		now := c.tick()
		if first.IsZero() {
			first = now
		}
		rows := []domain.ReplicaMetrics{row("dec-0", "dec", 0.95, 0), empty}
		r := tr.Observe("ns/m", tr.Measure("ns/m", rows, pd, 5, now), now)[0]
		if r.Idle > 0 && idleAfter < 0 {
			idleAfter = now.Sub(first)
		}
		if r.Sustained {
			reportedAfter = now.Sub(first)
			break
		}
	}
	return idleAfter, reportedAfter
}

// Review r3 (N1): a warm-pool Pod lent to a variant keeps the Ready time it
// had in the pool, an hour before the lend, and the model switch does not
// restart readiness. It read idle on the first cycle it was lent and was
// reported 2m15s after the lend: the fast scale-up path warned that adding
// replicas would not help. The grace runs from the lend, as the tracker first
// sees it.
func TestTrackerGivesALentBridgeTheGraceFromTheLend(t *testing.T) {
	bridge := row("pool-0", "dec", 0, 0)
	bridge.FromWarmPool = true
	bridge.ReadySince = t0.Add(-time.Hour)
	idle, reported := replicaGraceCycles(bridge, t0)
	if idle < IdleAfterReady {
		t.Errorf("a bridge lent at the first cycle read idle %v after the lend, under IdleAfterReady", idle)
	}
	if reported < IdleAfterReady+SustainFor {
		t.Errorf("a bridge lent at the first cycle was reported %v after the lend, under IdleAfterReady+SustainFor", reported)
	}
}

// A Pod the tracker saw as the variant's own replica and then as a bridge, or
// the other way round, is a new replica each time; one it has known for long
// enough is idle at once.
func TestTrackerRestartsTheGraceWhenAPodChangesHands(t *testing.T) {
	tr := NewTracker()
	pod := row("p-0", "dec", 0, 0)
	pod.FromWarmPool = true
	now := t0
	for range int(IdleAfterReady/cycle) + 1 {
		now = now.Add(cycle)
		tr.Measure("ns/m", []domain.ReplicaMetrics{row("dec-0", "dec", 0.95, 0), pod}, pd, 5, now)
	}
	if d := tr.Measure("ns/m", []domain.ReplicaMetrics{row("dec-0", "dec", 0.95, 0), pod}, pd, 5, now.Add(cycle)); d[domain.RoleDecode].Idle != 1 {
		t.Fatalf("a bridge lent for IdleAfterReady did not read idle")
	}
	pod.VariantName = "dec2"
	roles := map[string]string{"dec": domain.RoleDecode, "dec2": domain.RoleDecode}
	if d := tr.Measure("ns/m", []domain.ReplicaMetrics{row("dec-0", "dec", 0.95, 0), pod}, roles, 5, now.Add(2*cycle)); d[domain.RoleDecode].Idle != 0 {
		t.Error("a Pod lent to another variant kept the grace it had served on the first")
	}
}

// Review r3 (N3): a node clock 3 minutes behind the controller's dated a Pod
// that had been Ready for 10 s 3m10s into the controller's past, and it read
// idle at once. The grace runs from the first cycle the tracker saw it Ready
// too, whichever is later. A clock ahead stays conservative: not idle until
// the skew has passed as well.
func TestTrackerDoesNotLetASlowNodeClockSkipTheGrace(t *testing.T) {
	behind := row("dec-new", "dec", 0, 0)
	behind.ReadySince = t0.Add(cycle - 10*time.Second - 3*time.Minute)
	if idle, reported := replicaGraceCycles(behind, t0); idle < IdleAfterReady || reported < IdleAfterReady+SustainFor {
		t.Errorf("node clock 3m behind: idle %v and reported %v after the first sighting, want at least %v and %v",
			idle, reported, IdleAfterReady, IdleAfterReady+SustainFor)
	}
	ahead := row("dec-new", "dec", 0, 0)
	ahead.ReadySince = t0.Add(cycle + 5*time.Minute)
	if idle, _ := replicaGraceCycles(ahead, t0); idle < 5*time.Minute+IdleAfterReady {
		t.Errorf("node clock 5m ahead: idle %v after the first sighting, before the skew and the grace had passed", idle)
	}
}

// Review r3 (N3): the streak was timed, not counted, so two imbalanced
// readings 3 minutes apart with nothing read between (a model skipped without
// being forgotten) reported at once. A streak is of readings at most one
// interval apart, with half an interval of slack.
func TestTrackerSustainNeedsConsecutiveReadings(t *testing.T) {
	tr := NewTracker()
	tr.Observe("ns/m", imbalanced(), t0)
	if r := only(t, tr.Observe("ns/m", imbalanced(), t0.Add(3*time.Minute))); r.Sustained {
		t.Error("two imbalanced readings 3m apart, nothing between, were reported")
	}
	for _, interval := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute} {
		tr := NewTracker()
		tr.SetInterval(interval)
		now, reportedAt := t0, -1
		for i := 1; i <= 20 && reportedAt < 0; i++ {
			if r := only(t, tr.Observe("ns/m", imbalanced(), now)); r.Sustained {
				reportedAt = i
			}
			now = now.Add(interval)
		}
		if want := int(SustainFor/interval) + 1; reportedAt != want {
			t.Errorf("interval %v: reported on reading %d, want %d", interval, reportedAt, want)
		}
	}
	// Once reported, a gap restarts the clear: one balanced reading after it
	// does not clear what the readings before it reported.
	tr = NewTracker()
	c := &clock{now: t0}
	for range sustainCycles {
		tr.Observe("ns/m", imbalanced(), c.tick())
	}
	tr.Observe("ns/m", balanced(), c.tick())
	if r := only(t, tr.Observe("ns/m", balanced(), c.now.Add(ClearAfter))); !r.Sustained {
		t.Error("two balanced readings ClearAfter apart, nothing between, cleared a reported imbalance")
	}
}

// Review r3 (N2): one failed collection forgot the model, so the gauge read 0
// for SustainFor and the alert's for: restarted. Failed keeps the tracker as it
// stands until collection has failed for StaleAfter.
func TestTrackerKeepsItsStateThroughAShortCollectionFailure(t *testing.T) {
	tr, c := NewTracker(), &clock{now: t0}
	for range sustainCycles {
		tr.Observe("ns/m", imbalanced(), c.tick())
	}
	if tr.Failed("ns/m", c.tick()) {
		t.Fatal("one failed collection read stale")
	}
	if r := only(t, tr.Observe("ns/m", imbalanced(), c.tick())); !r.Sustained || r.Changed {
		t.Error("one failed collection cleared a reported imbalance")
	}
	staleAt := 0
	for i := 1; i <= 20 && staleAt == 0; i++ {
		if tr.Failed("ns/m", c.tick()) {
			staleAt = i
		}
	}
	if want := int(StaleAfter/cycle) + 1; staleAt != want {
		t.Errorf("collection failing on every cycle read stale on failure %d, want %d", staleAt, want)
	}
}
