package saturation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Specs for the fourth review of the learned-state redesign: each pins one
// finding.

// r4Rolling marks variant-a mid-rollout, or not, on params p.
func r4Rolling(store *capacity.Store, p *capacity.EngineParams, rolling bool) {
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: p, RollingOut: rolling}, time.Now())
}

// r4Windows is how many k2 windows a holds.
func r4Windows(a *SaturationAnalyzer) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.computeCapacityHistory)
}

// A variant whose own replicas report two pod template revisions is
// mid-rollout whatever its scale target's status says -- a LeaderWorkerSet
// status names no generation -- so their saturated readings are held back
// from its window. One revision, an unknown one, or a warm-pool bridge's own
// is not a second revision.
func TestReplicasOfTwoRevisionsHoldTheVariantsReadings(t *testing.T) {
	states := []domain.VariantReplicaState{{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 2, GPUsPerReplica: 1}}
	run := func(revA, revB string, bridgeB bool) int {
		a, _, _ := waProcess(mtParams(256, "vllm:v1"))
		ra := makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)
		ra.Revision = revA
		rb := makeReplicaMetrics("pod-b", "variant-a", 4000, 16000, 6, 100, 50)
		rb.Revision, rb.FromWarmPool = revB, bridgeB
		if _, err := a.Analyze(context.Background(), makeAnalyzerInput([]domain.ReplicaMetrics{ra, rb}, states)); err != nil {
			t.Fatal(err)
		}
		return r4Windows(a)
	}
	if n := run("rev-1", "rev-2", false); n != 0 {
		t.Errorf("replicas of two revisions: %d windows recorded, want none", n)
	}
	if n := run("rev-1", "rev-1", false); n != 1 {
		t.Errorf("replicas of one revision: %d windows, want the reading recorded", n)
	}
	if n := run("rev-1", "", false); n != 1 {
		t.Errorf("a replica with no revision read: %d windows, want the reading recorded", n)
	}
	if n := run("rev-1", "pool-rev", true); n != 1 {
		t.Errorf("a warm-pool bridge of the pool's revision: %d windows, want the variant's own reading recorded", n)
	}
}

// The variant's first measurement under the arguments it runs retires its
// previous figure: kept, it priced every later rollout from a measurement the
// variant had since replaced -- 2000 against a measured 8000.
func TestAMeasurementRetiresThePreviousFigure(t *testing.T) {
	a, store, clock := waProcess(mtParams(64, "vllm:v1"))
	waRun(t, a, clock, 2000, 6)
	waSetParams(store, mtParams(256, "vllm:v1"))
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity != 2000 || vc.K2Source != capacity.K2SrcPrior.String() {
		t.Fatalf("precondition: the 64-sequence window as a previous figure, got %v from %s", vc.PerReplicaCapacity, vc.K2Source)
	}
	waRun(t, a, clock, 8000, 6)
	r4Rolling(store, mtParams(256, "vllm:v2"), true)
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity != 8000 || vc.K2Source != capacity.K2SrcHistorical.String() {
		t.Errorf("an image rollout after the variant measured 8000: priced %v from %s, want its own 8000", vc.PerReplicaCapacity, vc.K2Source)
	}
}

// The service rate from before an arguments change holds the floor for at
// most BorrowedK2Hold + BorrowedK2Relax of serving, counted as a bound's
// clock (a pause counts one tick gap), and is read without Touch: touched and
// unbounded, a change that doubled the service rate kept the old engine's
// fleet for as long as the load was steady.
func TestTheOldServiceRateHoldsTheFloorForABoundedServingTime(t *testing.T) {
	a, _, clock := waProcess(mtParams(256, "vllm:v1"))
	key := rrKey(a, "variant-a")
	from, to := windowArgs(mtParams(256, "vllm:v1")), windowArgs(mtParams(64, "vllm:v1"))
	a.mu.Lock()
	a.saturatedThroughput[key] = rrWindow(2.5)
	a.throughputWindowArgs[key] = from
	a.throughputWindowUnderLocked(key, to)
	sk := stashKey(key, from)
	usedBefore := a.throughputStash[sk].window.Snapshot().LastUsed
	a.mu.Unlock()
	serve := func(d time.Duration) bool {
		held := true
		for end := clock.Add(d); clock.Before(end); *clock = clock.Add(4 * time.Minute) {
			if r := a.saturatedThroughputReadingUnder(key, to, true); r.rate != 2.5 || !r.borrowed {
				held = false
			}
		}
		return held
	}
	if !serve(3 * time.Hour) {
		t.Fatal("the old rate stopped holding within three hours of serving")
	}
	*clock = clock.Add(2 * time.Hour) // parked
	if !serve(3 * time.Hour) {
		t.Error("a two-hour pause was counted as serving time: the old rate stopped holding before 6.5 hours of serving")
	}
	a.mu.Lock()
	usedAfter := a.throughputStash[sk].window.Snapshot().LastUsed
	a.mu.Unlock()
	if !usedAfter.Equal(usedBefore) {
		t.Errorf("reading the old rate touched its window: last used %v, was %v", usedAfter, usedBefore)
	}
	serve(time.Hour)
	if r := a.saturatedThroughputReadingUnder(key, to, true); r.rate == 2.5 {
		t.Errorf("after 7 hours of serving the old rate still holds the floor: %+v", r)
	}
}

// r4Parked is variant-a with a 4000 window under 256 sequences whose
// arguments changed to 64 while nothing read it, holding bound lb.
func r4Parked(t *testing.T, lb *lowerBound) (*SaturationAnalyzer, string) {
	t.Helper()
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	key := rrKey(a, "variant-a")
	if lb != nil {
		a.mu.Lock()
		lb.seen = *clock
		a.lowerBounds[key] = lb
		a.mu.Unlock()
	}
	waSetParams(store, mtParams(64, "vllm:v1"))
	return a, key
}

// The previous figure made at export is written in the form a build before
// LowerBounds reads, for a rollback to it.
func TestAnExportedPreviousFigureRollsBack(t *testing.T) {
	a, key := r4Parked(t, nil)
	s := a.ExportLearnedState()
	lb := s.LowerBounds[key]
	if lb.Prior != 4000 {
		t.Fatalf("precondition: exported previous figure %d, want 4000", lb.Prior)
	}
	p, ok := s.PriorK2[key]
	if !ok || p.K2 != 4000 || !p.InstalledAt.Equal(lb.PriorUsed) {
		t.Errorf("rollback copy %+v (present %v), want 4000 dated from its last use %v", p, ok, lb.PriorUsed)
	}
}

// A window restored under a legacy tag -- from a document that recorded no
// arguments -- is not kept aside at export for a rollback that cannot come:
// its k2 becomes the previous figure, and its service rate is not carried to
// the next restart as a hold-only figure of a configuration nothing runs.
func TestALegacyWindowIsNotExportedAside(t *testing.T) {
	a, _, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	key := rrKey(a, "variant-a")
	saved := a.ExportLearnedState()
	saved.Args = nil
	saved.Config = map[string]string{"test-ns/variant-a": "a configuration nothing runs"}
	saved.Throughput = map[string]ThroughputSnapshot{key: {RollingAverageSnapshot: rrWindow(2.5).Snapshot(), SampledAt: *clock}}

	b, _, _ := waProcess(mtParams(256, "vllm:v1"))
	b.RestoreLearnedState(saved)
	if r, ok := b.installRestored("test-ns", "test-model", "variant-a"); !ok || r.Outcome != RestoredDiffers {
		t.Fatalf("precondition: restored %+v (%v), want the differs outcome", r, ok)
	}
	s := b.ExportLearnedState()
	for k := range s.K2Stash {
		if strings.Contains(k, "#"+legacyArgsPrefix) {
			t.Errorf("a legacy k2 window was exported aside: %s", k)
		}
	}
	for k := range s.ThroughputStash {
		if strings.Contains(k, "#"+legacyArgsPrefix) {
			t.Errorf("a legacy throughput window was exported aside: %s", k)
		}
	}
	if lb := s.LowerBounds[key]; lb.Prior != 4000 {
		t.Errorf("the legacy k2 window's previous figure %d, want 4000", lb.Prior)
	}
}

// A window an older build put aside under a legacy tag is not restored into
// the stash: no configuration runs it, so it could only come back as a
// hold-only service rate nothing measured.
func TestALegacyWindowPutAsideByAnOlderBuildIsNotRestored(t *testing.T) {
	a, _, clock := waProcess(mtParams(256, "vllm:v1"))
	key := rrKey(a, "variant-a")
	snap := rrWindow(4000).Snapshot()
	snap.LastUsed, snap.LastWritten = *clock, *clock
	a.RestoreLearnedState(LearnedState{
		K2Stash:         map[string]capacity.RollingAverageSnapshot{stashKey(key, legacyArgsPrefix+"fp"): snap},
		ThroughputStash: map[string]ThroughputSnapshot{stashKey(key, legacyArgsPrefix+"fp"): {RollingAverageSnapshot: snap, SampledAt: *clock}},
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.k2Stash) != 0 || len(a.throughputStash) != 0 {
		t.Errorf("legacy windows restored aside: %d k2, %d throughput", len(a.k2Stash), len(a.throughputStash))
	}
}

// A variant running the same unread configuration it was exported under gets
// its windows back as its own, and the restore says so: counted among the
// entries, not as previous figures, and not reported as a change.
func TestAnUnreadConfigurationsOwnWindowsAreNotCountedAsPriors(t *testing.T) {
	unread := mtParams(256, "vllm:v1")
	unread.Unresolved = []string{"gpu_memory_utilization"}
	a, _, clock := waProcess(unread)
	waRun(t, a, clock, 4000, 6)
	saved := a.ExportLearnedState()

	b, _, _ := waProcess(unread)
	b.RestoreLearnedState(saved)
	r, ok := b.installRestored("test-ns", "test-model", "variant-a")
	if !ok || r.Outcome != RestoredIncomplete {
		t.Fatalf("precondition: restored %+v (%v), want the incomplete outcome", r, ok)
	}
	if !r.Installed || r.Entries != 1 || r.Priors != 0 {
		t.Errorf("restored %+v: want its one window installed as its own, no previous figures", r)
	}
}

// A restart in the middle of a rollout that changed the arguments keeps the
// previous figure the change made, on the bound the rollout froze.
func TestARestartMidRolloutKeepsThePreviousFigure(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	r4Rolling(store, mtParams(64, "vllm:v1"), true)
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity != 4000 || vc.K2Source != capacity.K2SrcPrior.String() {
		t.Fatalf("precondition: mid-rollout on the previous figure, got %v from %s", vc.PerReplicaCapacity, vc.K2Source)
	}
	saved := a.ExportLearnedState()

	b, store2, clock2 := waProcess(mtParams(64, "vllm:v1"))
	*clock2 = clock.Add(time.Minute)
	b.RestoreLearnedState(saved)
	r4Rolling(store2, mtParams(64, "vllm:v1"), true)
	if vc := waRun(t, b, clock2, 1000, 0); vc.PerReplicaCapacity != 4000 || vc.K2Source != capacity.K2SrcPrior.String() {
		t.Errorf("a minute after a restart mid-rollout: %v from %s, want the previous figure, 4000", vc.PerReplicaCapacity, vc.K2Source)
	}
}
