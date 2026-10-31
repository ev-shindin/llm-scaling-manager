package balance

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
)

// testTracker is a Tracker over a settle tracker of its own, which its
// Measure observes first, as the engine does each cycle.
type testTracker struct {
	*Tracker
	settle *settle.Tracker
}

func newTestTracker() testTracker {
	st := settle.NewTracker()
	return testTracker{Tracker: NewTracker(st), settle: st}
}

// SetInterval sets both trackers' interval, as the engine does.
func (t testTracker) SetInterval(d time.Duration) {
	t.Tracker.SetInterval(d)
	t.settle.SetInterval(d)
}

// Measure observes rows into the settle tracker, then measures them.
func (t testTracker) Measure(key string, rows []domain.ReplicaMetrics, rolesByVariant map[string]string,
	th Thresholds, now time.Time) map[string]Distribution {
	t.settle.Observe(key, settlerow.Of(rows, rolesByVariant), now)
	return t.Tracker.Measure(key, rows, rolesByVariant, th, now)
}

// firstIdle runs a saturated decode replica beside empty for 20 minutes,
// leaving empty out of the rows on the cycles absent names (1 is the first),
// with edit applied to empty from the cycle it names on. It returns how long
// after the first cycle empty first read idle, -1 for never.
func firstIdle(empty domain.ReplicaMetrics, absent map[int]bool, editFrom int, edit func(*domain.ReplicaMetrics)) time.Duration {
	return firstIdleEvery(cycle, empty, absent, editFrom, edit)
}

// firstIdleEvery is firstIdle with the cycles interval apart.
func firstIdleEvery(interval time.Duration, empty domain.ReplicaMetrics, absent map[int]bool, editFrom int,
	edit func(*domain.ReplicaMetrics)) time.Duration {
	tr := newTestTracker()
	tr.SetInterval(interval)
	for i := 1; i <= int(20*time.Minute/interval); i++ {
		now := t0.Add(time.Duration(i-1) * interval)
		if edit != nil && i == editFrom {
			edit(&empty)
		}
		rows := []domain.ReplicaMetrics{row("dec-0", "dec", 0.95, 0)}
		if !absent[i] {
			rows = append(rows, empty)
		}
		if d := tr.Measure("ns/m", rows, pd, policy, now)[domain.RoleDecode]; d.Idle > 0 {
			return now.Sub(t0)
		}
	}
	return -1
}

// cycles is the set of cycles from..to, inclusive.
func cycles(from, to int) map[int]bool {
	out := map[int]bool{}
	for i := from; i <= to; i++ {
		out[i] = true
	}
	return out
}

// Learner PR 1's named change (analyzer-learner.md, 4.2): the grace is read
// from the settle tracker, which keeps a sighting across an absence of up to
// its tolerance (a minute at a 15 s interval). #283's tracker rebuilt its
// sightings from each cycle's rows, so one missed scrape restarted a
// replica's grace and put the role's report back by IdleAfterReady. A
// replica missing for up to two cycles keeps its grace; one missing for
// longer than the tolerance starts again from its return.
//
// On the parent the kept cases fail: any absence restarted the grace.
func TestTrackerKeepsTheGraceThroughAnAbsenceOfUpToThePeriod(t *testing.T) {
	empty := row("dec-new", "dec", 0, 0)
	empty.ReadySince = t0
	if got := firstIdle(empty, nil, 0, nil); got != IdleAfterReady {
		t.Fatalf("precondition: a replica Ready at the first cycle read idle %v after it, want %v", got, IdleAfterReady)
	}
	for _, tc := range []struct {
		name    string
		missing int
		kept    bool
	}{
		{"one missed scrape", 1, true},
		{"two missed scrapes", 2, true},
		{"absent just past the period", int(settle.Period / cycle), false},
		{"absent for two minutes", int(2 * time.Minute / cycle), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Absent from the third cycle; back on the cycle after.
			back := 3 + tc.missing
			want := IdleAfterReady
			if !tc.kept {
				want = time.Duration(back-1)*cycle + IdleAfterReady
			}
			got := firstIdle(empty, cycles(3, back-1), 0, nil)
			if got != want {
				t.Errorf("absent %d cycles (back %v after the last sighting): idle %v after the first cycle, want %v",
					tc.missing, time.Duration(tc.missing+1)*cycle, got, want)
			}
		})
	}
}

// Review r1 (B1): the tolerance follows the optimize interval. With it fixed
// at a minute, every cycle of a 61 s interval was an absence: the replica
// settled again on every cycle and never read idle, so wva_load_imbalanced
// went silent. At each interval the replica reads idle on the first cycle at
// least IdleAfterReady after it was first seen, and one missed scrape keeps
// that.
//
// On 9e8f275ce the 61 s cases fail (never idle), and so do the missed-scrape
// cases at 45 s and 60 s.
func TestTrackerEndsTheGraceAtLongIntervals(t *testing.T) {
	empty := row("dec-new", "dec", 0, 0)
	empty.ReadySince = t0
	for _, interval := range []time.Duration{45 * time.Second, 60 * time.Second, 61 * time.Second} {
		// The first cycle at least IdleAfterReady after the first.
		want := interval
		for want < IdleAfterReady {
			want += interval
		}
		if got := firstIdleEvery(interval, empty, nil, 0, nil); got != want {
			t.Errorf("interval %v: idle %v after the first cycle, want %v", interval, got, want)
		}
		if got := firstIdleEvery(interval, empty, cycles(2, 2), 0, nil); got != want {
			t.Errorf("interval %v, one missed scrape: idle %v after the first cycle, want %v", interval, got, want)
		}
	}
}

// Shipped behaviour (#283, analyzer-learner.md 7, the warm-pool row): a Pod
// lent, handed back and lent again to the same variant within the period
// starts its grace again, though its key and its Ready time are what they
// were. The new lend time tells it apart; a Pod missing for one cycle under
// the same lend keeps its grace.
//
// On the parent the re-lend case passes (any absence restarted the grace) and
// the missed-scrape case fails.
func TestTrackerRestartsTheGraceOfAPodLentAgain(t *testing.T) {
	lentAt := t0.Add(-time.Hour)
	bridge := row("pool-0", "dec", 0, 0)
	bridge.FromWarmPool = true
	bridge.ReadySince = t0.Add(-3 * time.Hour)
	bridge.LentSince = lentAt
	if got := firstIdle(bridge, nil, 0, nil); got != IdleAfterReady {
		t.Fatalf("precondition: a bridge first seen at the first cycle read idle %v after it, want %v", got, IdleAfterReady)
	}

	if got := firstIdle(bridge, cycles(3, 3), 0, nil); got != IdleAfterReady {
		t.Errorf("a bridge missing one cycle under the same lend read idle %v after the first cycle, want %v", got, IdleAfterReady)
	}

	// Handed back on cycle 3 and lent again on cycle 4: the pool published
	// the new lend at cycle 4.
	relend := t0.Add(3 * cycle)
	want := 3*cycle + IdleAfterReady
	got := firstIdle(bridge, cycles(3, 3), 4, func(rm *domain.ReplicaMetrics) { rm.LentSince = relend })
	if got != want {
		t.Errorf("a bridge lent again a cycle after it was handed back read idle %v after the first cycle, want %v (the grace from the new lend)",
			got, want)
	}
}

// Review r2 (N2): the tolerance ends. At 45 s it is 112.5 s; two missed
// scrapes are a 135 s gap, so the replica starts its grace again from its
// return.
func TestTrackerRestartsTheGraceAfterTwoMissedScrapesAt45s(t *testing.T) {
	interval := 45 * time.Second
	empty := row("dec-new", "dec", 0, 0)
	empty.ReadySince = t0
	// Missing on cycles 2 and 3; back on cycle 4, at 135 s.
	back := 3 * interval
	want := back
	for want < back+IdleAfterReady {
		want += interval
	}
	if got := firstIdleEvery(interval, empty, cycles(2, 3), 0, nil); got != want {
		t.Errorf("two missed scrapes at 45 s: idle %v after the first cycle, want %v (the grace from the return)", got, want)
	}
}
