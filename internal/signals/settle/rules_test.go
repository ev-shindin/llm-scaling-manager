package settle

import (
	"testing"
	"time"
)

// The four settle rules of analyzer-learner.md 4.1, each written against the
// tracker's API (4.3: "before PR 1 merges, all four rules are written
// against the API"). The balance tracker's is production code
// (internal/signals/balance, graceFrom); the other three move onto the
// tracker in their own PRs (k2 and noteReady in 2, ITL in 4a), and these
// sketches show the API carries what each reads. Each keeps its own policy:
// the windows are arguments, never the tracker's.

// itlSettling is the ITL steady trace's rule: a replica settles for steadyFor
// from its last settle event, or from load returning after at least idleRows
// idle rows.
func itlSettling(r Replica, steadyFor time.Duration, idleRows int, now time.Time) bool {
	from := r.Since
	if at, ok := r.ReturnedAfter(idleRows); ok && at.After(from) {
		from = at
	}
	return now.Sub(from) < steadyFor
}

// joined is noteReady's rule (and k2's joined test): a replica joined under
// window ago, counted from the later of its settle event and its Ready time
// as first seen.
func joined(r Replica, window time.Duration, now time.Time) bool {
	from := r.Since
	if rf := r.ReadyFrom(); rf.After(from) {
		from = rf
	}
	return now.Sub(from) < window
}

// k2Settled is the time half of k2's settled-over-window test: there is a
// row at least lookback back, and no gap longer than lookback since it. The
// gauges it compares (the rewritten queue, resident tokens) stay k2's, keyed
// by the sample's time.
func k2Settled(v View, model string, key Key, lookback time.Duration, now time.Time) (time.Time, bool) {
	back, ok := v.Back(model, key, lookback, now)
	if !ok {
		return time.Time{}, false
	}
	last := back.At
	for _, s := range v.Trace(model, key) {
		if s.At.Before(back.At) {
			continue
		}
		if s.At.Sub(last) > lookback {
			return time.Time{}, false
		}
		last = s.At
	}
	return back.At, now.Sub(last) <= lookback
}

func TestTheFourRulesReadTheAPI(t *testing.T) {
	key := Key{Pod: "d", Variant: "dec"}
	rows := append(repeat(busy("d"), 10), repeat(idle("d"), 2)...)
	tr, last := run(append(rows, ptr(busy("d")))...)
	r := replicaOf(t, tr, "d")

	// ITL: settled ten cycles after the join, settling again on the load
	// returning after 2 idle rows, not after a run it would need 3 for.
	if !itlSettling(r, 2*time.Minute, 2, last) || itlSettling(r, 2*time.Minute, 3, last) {
		t.Errorf("ITL: settling after 2 idle rows %v (want true), with idleRows 3 %v (want false)",
			itlSettling(r, 2*time.Minute, 2, last), itlSettling(r, 2*time.Minute, 3, last))
	}
	// steadyFor is the reader's: a minute after the return, settled under a
	// one-minute steadyFor and settling under ten.
	if itlSettling(r, time.Minute, 2, last.Add(time.Minute)) || !itlSettling(r, 10*time.Minute, 3, last) {
		t.Error("ITL: the reader's steadyFor does not set the wait")
	}

	// noteReady: a Pod Ready an hour ago, joined at t0, has not joined
	// under a minute ago at last; a restart under its name has.
	if joined(r, time.Minute, last) {
		t.Error("noteReady: a replica seen for 3 minutes joined under a minute ago")
	}
	restarted := busy("d")
	restarted.ReadySince = last.Add(cycle - 5*time.Second)
	tr.Observe(model, []Row{restarted}, last.Add(cycle))
	if r := replicaOf(t, tr, "d"); !joined(r, time.Minute, last.Add(cycle)) {
		t.Error("noteReady: a restart under the same name did not join again")
	}

	// k2: a row one lookback back, every row since within it.
	tr, last = run(repeat(busy("d"), 8)...)
	if at, ok := k2Settled(tr, model, key, time.Minute, last); !ok || !at.Equal(last.Add(-time.Minute)) {
		t.Errorf("k2: settled %v from %v, want true from %v", ok, at, last.Add(-time.Minute))
	}
	if _, ok := k2Settled(tr, model, key, time.Minute, last.Add(2*time.Minute)); ok {
		t.Error("k2: settled across a gap longer than the lookback")
	}
}
