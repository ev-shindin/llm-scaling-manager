package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Lower bounds, previous figures and argument tags at their edges: the
// restore gates that age or refuse a stored entry, the dates written for an
// older build, and the rules that pick or retire a previous figure.

// A prior-only bound the process dropped on a forget is not brought back by a
// restore that reads a document written before the forget: installed, it
// priced the variant from the figure the operator asked to forget, while the
// token stayed consumed so the same annotation could not forget it again.
func TestARestoreDoesNotBringBackAPriorOnlyBoundAForgetDropped(t *testing.T) {
	priorOnly := func(now time.Time) LowerBoundSnapshot {
		return LowerBoundSnapshot{Prior: 4000, PriorSince: now.Add(-time.Hour), PriorUsed: now.Add(-time.Minute), Seen: now}
	}

	t.Run("forgotten before the restore", func(t *testing.T) {
		a, own, _, clock := lbProcess(t, 9000)
		a.mu.Lock()
		a.lowerBounds[own] = &lowerBound{prior: 4000, priorSince: clock.Add(-time.Hour), priorUsed: clock.Add(-time.Minute), seen: *clock}
		a.mu.Unlock()
		a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
		if _, held := previousFigures(a)[own]; held {
			t.Fatal("precondition: the forget drops a prior-only bound")
		}

		rep := a.RestoreLearnedState(LearnedState{LowerBounds: map[string]LowerBoundSnapshot{own: priorOnly(*clock)}})
		if rep.Forgotten == 0 {
			t.Error("the forgotten variant's bound was not counted as forgotten")
		}
		a.mu.Lock()
		lb := a.lowerBounds[own]
		a.mu.Unlock()
		if lb != nil {
			t.Errorf("the restore brought back the bound the forget dropped: %+v", lb)
		}
		if got := a.ForgottenToken("test-ns", "variant-b"); got != "t1" {
			t.Errorf("forget token %q, want t1 kept", got)
		}
	})

	t.Run("no forget", func(t *testing.T) {
		a, own, _, clock := lbProcess(t, 9000)
		if rep := a.RestoreLearnedState(LearnedState{LowerBounds: map[string]LowerBoundSnapshot{own: priorOnly(*clock)}}); rep.BorrowClocks != 1 {
			t.Errorf("restore report %+v, want the bound installed", rep)
		}
		if got := previousFigures(a)[own]; got != 4000 {
			t.Errorf("previous figure %d, want 4000 installed", got)
		}
	})
}

// A restored bound that holds only a previous figure ages on the figure's last
// use, a bucket retention: one nothing has read for longer is not installed,
// as the sweep would not keep it. Installed, its first offer stamped it used
// and a figure the design says has expired priced a fresh hold.
func TestARestoredPriorOnlyBoundAgesOnItsLastUse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		since   time.Duration // before now
		used    time.Duration // before now; 0 records no last use
		install bool
	}{
		{"last used past the retention", 8 * 24 * time.Hour, capacity.HistoryRetention + time.Hour, false},
		{"no last use, made past the retention", capacity.HistoryRetention + time.Hour, 0, false},
		{"last used inside the retention", 8 * 24 * time.Hour, capacity.HistoryRetention - time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, own, _, clock := lbProcess(t, 9000)
			if a.horizons.BucketRetention != capacity.HistoryRetention {
				t.Fatalf("precondition: default retention, got %v", a.horizons.BucketRetention)
			}
			lb := LowerBoundSnapshot{Prior: 3000, PriorSince: clock.Add(-tc.since), Seen: *clock}
			if tc.used > 0 {
				lb.PriorUsed = clock.Add(-tc.used)
			}
			rep := a.RestoreLearnedState(LearnedState{LowerBounds: map[string]LowerBoundSnapshot{own: lb}})
			got := previousFigures(a)[own]
			switch {
			case tc.install && (got != 3000 || rep.BorrowClocks != 1):
				t.Errorf("installed %d (report %+v), want the previous figure 3000 installed", got, rep)
			case !tc.install && (got != 0 || rep.Aged != 1):
				t.Errorf("installed %d (report %+v), want none, counted as aged", got, rep)
			}
		})
	}
}

// The PriorK2 entry written for an older build is dated from the figure's
// last use, not from when it became one: that build ages it from the date
// against a bucket retention, and a figure still in use but made more than a
// retention ago was dropped on a downgrade, pricing the variant at derived or
// k1.
func TestAnExportedPriorK2IsDatedFromItsLastUse(t *testing.T) {
	a, own, _, clock := lbProcess(t, 9000)
	used := clock.Add(-time.Minute)
	a.mu.Lock()
	a.lowerBounds[own] = &lowerBound{prior: 3000, priorSince: clock.Add(-8 * 24 * time.Hour), priorUsed: used, seen: *clock}
	a.mu.Unlock()

	saved := a.ExportLearnedState()
	p, ok := saved.PriorK2[own]
	if !ok || p.K2 != 3000 {
		t.Fatalf("PriorK2 %+v (written %v), want 3000", p, ok)
	}
	if !p.InstalledAt.Equal(used) {
		t.Errorf("PriorK2 dated %v, want its last use %v", p.InstalledAt, used)
	}

	// What an older build writes back: PriorK2 alone.
	saved.LowerBounds = nil
	b, _, _, clock2 := lbProcess(t, 9000)
	*clock2 = *clock
	rep := b.RestoreLearnedState(saved)
	if got := previousFigures(b)[own]; got != 3000 || rep.Aged != 0 {
		t.Errorf("round trip installed %d (report %+v), want 3000, not aged", got, rep)
	}
}

// The variant's first measurement under its new arguments retires the
// previous figure of the old ones: kept, it went on being exported as the
// variant's figure for an older build to price from, after the variant had
// replaced it with its own.
func TestAMeasurementUnderTheNewArgumentsRetiresThePreviousFigure(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	key := rrKey(a, "variant-a")
	waRun(t, a, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	if vc := waRun(t, a, clock, 1000, 0); vc.K2Source != capacity.K2SrcPrior.String() {
		t.Fatalf("precondition: priced from the previous figure, got %s", vc.K2Source)
	}

	if vc := waRun(t, a, clock, 5000, 6); vc.K2Source != capacity.K2SrcObserved.String() {
		t.Fatalf("precondition: measured under the new arguments, got %s", vc.K2Source)
	}
	if got, held := previousFigures(a)[key]; held {
		t.Errorf("previous figure %d still held after the variant measured itself", got)
	}
	s := a.ExportLearnedState()
	if p, ok := s.PriorK2[key]; ok {
		t.Errorf("retired previous figure exported for an older build: %+v", p)
	}
	if lb := s.LowerBounds[key]; lb.Prior != 0 {
		t.Errorf("retired previous figure exported: %d", lb.Prior)
	}
}

// A window put aside makes the key's previous figure in place of an older
// one, higher or lower: the latest measurement is the figure, and keeping the
// lower of the two priced the variant on arguments two changes back.
func TestAPutAsideWindowReplacesALowerPreviousFigure(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	from, to := windowArgs(mtParams(256, "vllm:v1")), windowArgs(mtParams(64, "vllm:v1"))
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lowerBounds[key] = &lowerBound{prior: 2000, priorSince: clock.Add(-time.Hour), priorUsed: clock.Add(-time.Hour), seen: *clock}
	a.computeCapacityHistory[key] = rrWindow(4000)
	a.k2WindowArgs[key] = from
	a.k2WindowUnderLocked(key, to)
	if lb := a.lowerBounds[key]; lb == nil || lb.prior != 4000 {
		t.Errorf("previous figure %+v, want the put-aside window's 4000 in place of the older 2000", lb)
	}
}

// With throughput windows put aside under two earlier argument sets, the
// floor holds on the one read most recently: the other is from two changes
// back and holds the fleet that engine needed.
func TestTheMostRecentlyReadPutAsideServiceRateHoldsTheFloor(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	older, newer := windowArgs(mtParams(256, "vllm:v1")), windowArgs(mtParams(128, "vllm:v1"))
	now := windowArgs(mtParams(64, "vllm:v1"))
	window := func(v float64, used time.Time) *capacity.RollingAverage {
		ra := rrWindow(v)
		ra.TouchAt(used)
		return ra
	}
	a.mu.Lock()
	a.throughputStash[stashKey(key, older)] = stashedThroughput{window: window(2.0, clock.Add(-2*time.Hour))}
	a.throughputStash[stashKey(key, newer)] = stashedThroughput{window: window(3.0, clock.Add(-time.Hour))}
	a.mu.Unlock()

	r := a.saturatedThroughputReadingUnder(key, now, true)
	if r.rate != 3.0 || !r.borrowed {
		t.Errorf("held on %+v, want the more recently read 3.0 as a hold-only reading", r)
	}
}

// A borrow clock restored from a document that kept only its start (no tick)
// does not count the time the variant then stays parked as serving time: the
// restore stamps a tick, so the first serving cycle reads the park as a
// pause. Counted, an eight-hour park after a restart left the bound spent.
func TestARestoredLegacyBorrowClockDoesNotCountAParkAsServing(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	rep := a.RestoreLearnedState(LearnedState{K2BorrowedSince: map[string]time.Time{own: clock.Add(-10 * time.Minute)}})
	if rep.BorrowClocks != 1 {
		t.Fatalf("precondition: the legacy clock is installed, report %+v", rep)
	}

	*clock = clock.Add(8 * time.Hour) // parked: no serving cycle
	b, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true)
	if !ok || b.relaxed != 0 || b.k2 != 4000 {
		t.Fatalf("first serving cycle after the park: %+v (ok %v), want the borrow at full strength", b, ok)
	}
	if want := 10*time.Minute + BorrowedK2TickGap; b.age != want {
		t.Errorf("bound age %v, want %v: the park was counted as serving time", b.age, want)
	}
}

// A restored armed bound, and a restored forget mark, go with their variant:
// one whose variant was last seen more than VariantTimeout ago is not
// installed. A variant re-created under the same name would otherwise inherit
// the old variant's spent bound, or its forget token, so the operator reusing
// that token forgot nothing.
func TestARestoreDropsABoundAndAForgetMarkOfAVariantPastItsTimeout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seen  time.Duration // before now
		fresh bool
	}{
		{"seen past the variant timeout", capacity.HistoryEvictionTimeout + time.Hour, false},
		{"seen inside the variant timeout", time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, own, _, clock := lbProcess(t, 4000)
			if a.horizons.VariantTimeout != capacity.HistoryEvictionTimeout {
				t.Fatalf("precondition: default variant timeout, got %v", a.horizons.VariantTimeout)
			}
			seen := clock.Add(-tc.seen)
			rep := a.RestoreLearnedState(LearnedState{
				LowerBounds: map[string]LowerBoundSnapshot{own: {
					Armed: true, Since: seen.Add(-time.Hour), Tick: seen, Seen: seen,
					Donor: "variant-a", DonorK2: 4000,
				}},
				Forgotten: map[string]ForgetMarkSnapshot{"test-ns/variant-b": {Token: "t1", Seen: seen}},
			})
			_, armed := borrowClocks(a)[own]
			token := a.ForgottenToken("test-ns", "variant-b")
			if tc.fresh {
				if !armed || token != "t1" || rep.BorrowClocks != 1 {
					t.Errorf("bound installed %v, token %q (report %+v), want both restored", armed, token, rep)
				}
				return
			}
			if armed || rep.Aged != 1 {
				t.Errorf("bound installed %v (report %+v), want none, counted as aged", armed, rep)
			}
			if token != "" {
				t.Errorf("forget token %q restored for a variant past its timeout, want none", token)
			}
		})
	}
}

// A row priced at its own saturated reading mid-rollout, below the chain,
// reports no borrow: the bound priced nothing on that cycle, and naming its
// donor and relaxation sent an operator chasing a borrow that was not used.
func TestARowPricedAtItsRolloutReadingReportsNoBorrow(t *testing.T) {
	a, own, _, _ := lbProcess(t, 4000)
	params := mtParams(256, "vllm:v1")
	k2 := func(queue int, tokens int64, adm admission) k2Decision {
		return a.computeK2(own, "test-model", "test-ns", "variant-b",
			queue, tokens, 50, 100, 5, params, flagsBatchCap(params), 1, 12800, 16000,
			domain.RoleBoth, false, adm, false, logr.Discard())
	}
	const donor = "variant-a"
	if d := k2(0, 1000, admission{}); d.src != capacity.K2SrcBorrowed || d.borrowed.donor != donor {
		t.Fatalf("precondition: a serving row borrows from variant-a, got %v from %+v", d.src, d.borrowed)
	}
	if d := k2(0, 1000, admission{rollout: true}); d.src != capacity.K2SrcBorrowed || d.borrowed.donor != donor {
		t.Fatalf("precondition: mid-rollout the chain still gives the borrow, got %v from %+v", d.src, d.borrowed)
	}

	d := k2(6, 3000, admission{rollout: true})
	if d.src != capacity.K2SrcUnadmitted || d.k2 != 3000 {
		t.Fatalf("precondition: priced at the reading 3000 (P1-obs-unadmitted), got %d from %v", d.k2, d.src)
	}
	if d.borrowed != (k2Borrow{}) {
		t.Errorf("a row priced at its reading reports the borrow it did not use: %+v", d.borrowed)
	}
}

// A throughput window is tagged with the arguments it was written under on
// the cycle it is written. Untagged, it was taken as written under whatever
// the variant ran when it was next read or exported, so a change of arguments
// in between priced the new engine from the old one's service rate as its own.
func TestAThroughputWindowIsTaggedWithItsArgumentsWhenWritten(t *testing.T) {
	from := mtParams(256, "vllm:v1")
	a, store, _ := waProcess(from)
	rm := makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)
	rm.RequestRate = 2
	rm.GenerationTokenRate = 100
	if _, err := analyzeConfirmed(context.Background(), a, makeAnalyzerInput([]domain.ReplicaMetrics{rm}, mtOneVariant)); err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	a.mu.Lock()
	keys := make([]string, 0, len(a.saturatedThroughput))
	for k := range a.saturatedThroughput {
		keys = append(keys, k)
	}
	var tag string
	if len(keys) == 1 {
		tag = a.throughputWindowArgs[keys[0]]
	}
	a.mu.Unlock()
	if len(keys) != 1 {
		t.Fatalf("precondition: one throughput window written, got %v", keys)
	}
	if want := windowArgs(from); tag != want {
		t.Errorf("throughput window tagged %q, want %q on the cycle it was written", tag, want)
	}

	waSetParams(store, mtParams(64, "vllm:v1"))
	s := a.ExportLearnedState()
	if _, own := s.Throughput[keys[0]]; own {
		t.Error("exported as the window of arguments it was not written under")
	}
	if _, put := s.ThroughputStash[stashKey(keys[0], windowArgs(from))]; !put {
		t.Error("not put aside under the arguments it was written under")
	}
}
