package saturation

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Specs for the third review of the learned-state redesign: each pins one
// finding.

// A previous figure made at export from a window a coping fleet kept alive by
// reading it -- written days ago, read this minute -- survives a restart.
// Stamped from the window's last write alone, the restore dropped it as aged
// and the variant woke at derived or k1: a k2 raise caused by the restart.
func TestAnExportedPreviousFigureSurvivesARestartWithAnOldWrite(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	key := rrKey(a, "variant-a")
	a.mu.Lock()
	a.computeCapacityHistory[key].ObservedAt(time.Now().Add(-8 * 24 * time.Hour))
	a.computeCapacityHistory[key].TouchAt(time.Now())
	a.mu.Unlock()
	waSetParams(store, mtParams(64, "vllm:v1")) // nothing read the window since
	saved := a.ExportLearnedState()

	b, _, clock2 := waProcess(mtParams(64, "vllm:v1"))
	b.RestoreLearnedState(saved)
	if vc := waRun(t, b, clock2, 1000, 0); vc.K2Source != capacity.K2SrcPrior.String() || vc.PerReplicaCapacity != 4000 {
		t.Errorf("after the restart: %v from %s, want the previous figure 4000", vc.PerReplicaCapacity, vc.K2Source)
	}
}

// A previous figure ages on its last use, as the window it stands in for: one
// a decision read this cycle is kept however long ago it became one, and one
// nothing has read for a bucket retention goes.
func TestAPreviousFigureAgesOnItsLastUse(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	retention := a.horizons.BucketRetention
	a.mu.Lock()
	a.lowerBounds[own] = &lowerBound{prior: 3000, priorSince: clock.Add(-retention - time.Hour),
		priorUsed: clock.Add(-time.Minute), seen: *clock}
	a.mu.Unlock()
	a.EvictStaleHistory()
	a.mu.Lock()
	kept := a.lowerBounds[own] != nil && a.lowerBounds[own].prior == 3000
	a.lowerBounds[own].priorUsed = clock.Add(-retention - time.Minute)
	a.mu.Unlock()
	if !kept {
		t.Error("a previous figure read a minute ago was dropped for having become one a retention ago")
	}
	a.EvictStaleHistory()
	a.mu.Lock()
	defer a.mu.Unlock()
	if lb := a.lowerBounds[own]; lb != nil && lb.prior != 0 {
		t.Errorf("a previous figure nothing read for a retention is still held: %d", lb.prior)
	}
}

// Across a change of arguments the old service rate holds the floor -- as a
// hold-only figure, like a neighbouring bucket's -- until the new arguments
// measure their own. Dropped, the change switched off the one floor that can
// only add replicas.
func TestTheOldServiceRateHoldsTheFloorAcrossAnArgumentsChange(t *testing.T) {
	a, _, _ := waProcess(mtParams(256, "vllm:v1"))
	key := rrKey(a, "variant-a")
	from, to := windowArgs(mtParams(256, "vllm:v1")), windowArgs(mtParams(64, "vllm:v1"))
	a.mu.Lock()
	a.saturatedThroughput[key] = rrWindow(2.5)
	a.throughputWindowArgs[key] = from
	a.throughputWindowUnderLocked(key, to)
	a.mu.Unlock()
	r := a.saturatedThroughputReadingUnder(key, to, true)
	if r.rate != 2.5 || !r.borrowed {
		t.Errorf("under the new arguments: %+v, want the old 2.5 as a hold-only (borrowed) reading", r)
	}
}

// A variant whose configuration is never read completely (flags from envFrom)
// gets its own windows back after a restart, as its own: its windows are
// exported under the unread configuration's tag, and a variant running the
// same unread configuration takes them back.
func TestAnUnreadConfigurationRestoresItsOwnWindow(t *testing.T) {
	unread := mtParams(256, "vllm:v1")
	unread.Unresolved = []string{"gpu_memory_utilization"}
	a, _, clock := waProcess(unread)
	waRun(t, a, clock, 4000, 6)
	saved := a.ExportLearnedState()

	b, _, clock2 := waProcess(unread)
	b.RestoreLearnedState(saved)
	if vc := waRun(t, b, clock2, 1000, 0); vc.K2Source != capacity.K2SrcHistorical.String() || vc.PerReplicaCapacity != 4000 {
		t.Errorf("after the restart: %v from %s, want its own window, 4000 from P2-hist", vc.PerReplicaCapacity, vc.K2Source)
	}
}

// A spent borrow clock restored from a document older than the record of
// what it ran on takes today's figure as its reference, and re-arms on a
// figure more than 20% below it -- where it compared every figure with zero
// and never re-armed.
func TestALegacySpentClockReArmsOnALowerFigure(t *testing.T) {
	a, own, donor, clock := lbProcess(t, 4000)
	a.RestoreLearnedState(LearnedState{K2BorrowedSince: map[string]time.Time{
		own: clock.Add(-(BorrowedK2Hold + BorrowedK2Relax + time.Hour)),
	}})
	p := mtParams(256, "vllm:v1")
	if _, ok := a.borrowedK2(own, "test-model", 12800, p, true, true); ok {
		t.Fatal("precondition: the restored clock is spent")
	}
	lbSetDonor(a, donor, 2000)
	*clock = clock.Add(time.Minute)
	if b, ok := a.borrowedK2(own, "test-model", 12800, p, true, true); !ok || b.k2 != 2000 {
		t.Errorf("the donor fell 4000 -> 2000 and the restored spent clock did not re-arm: %+v (ok %v)", b, ok)
	}
}

// A previous figure from a PR #151 build's document keeps the clock that
// document kept under the same key: started afresh, it was re-armed at full
// strength on every restart.
func TestAnOlderBuildsPriorKeepsItsClock(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	started := clock.Add(-2 * time.Hour)
	a.RestoreLearnedState(LearnedState{
		PriorK2:         map[string]PriorK2Snapshot{key: {K2: 4000, InstalledAt: clock.Add(-2 * time.Hour)}},
		K2BorrowedSince: map[string]time.Time{key: started},
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	lb := a.lowerBounds[key]
	if lb == nil || !lb.armed || !lb.since.Equal(started) || lb.prior != 4000 {
		t.Errorf("restored %+v, want the prior 4000 armed from %v", lb, started)
	}
}

// A window restored under the legacy tag of a document that recorded no
// arguments becomes a previous figure on its first read and is not kept for a
// rollback no configuration can make.
func TestALegacyWindowIsNotKeptForARollback(t *testing.T) {
	src, _, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, src, clock, 4000, 6)
	saved := src.ExportLearnedState()
	saved.Args = nil
	saved.Config["test-ns/variant-a"] = "a fingerprint of another version"

	a, _, clock2 := waProcess(mtParams(256, "vllm:v1"))
	a.RestoreLearnedState(saved)
	waRun(t, a, clock2, 1000, 0)
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.k2Stash); n != 0 {
		t.Errorf("%d legacy windows kept for a rollback", n)
	}
}

// Mid-rollout a saturated reading is not recorded and prices nothing: the
// fleet stays on the window the variant measured. A rollout's readings may
// be an old pod's, and folding them into a figure needed more of them than
// one cycle gives -- held across cycles, they were swept before they
// added up, and the figure they made was dropped the cycle the rollout
// ended.
func TestARolloutReadingPricesOnlyItsOwnCycle(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v2"), RollingOut: true}, time.Now())
	key := rrKey(a, "variant-a")
	a.mu.Lock()
	before := a.computeCapacityHistory[key].Len()
	a.mu.Unlock()
	for range 5 {
		if vc := waRun(t, a, clock, 3000, 6); vc.PerReplicaCapacity != 3000 || vc.K2Source != capacity.K2SrcUnadmitted.String() {
			t.Errorf("a saturated reading of 3000 under a 4000 window, mid-rollout: priced %v from %s, want 3000 for the cycle (P1-obs-unadmitted)", vc.PerReplicaCapacity, vc.K2Source)
		}
	}
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity != 4000 || vc.K2Source != capacity.K2SrcHistorical.String() {
		t.Errorf("the cycle after, not saturated: priced %v from %s, want the window's 4000 (P2-hist): the readings priced only their own cycles", vc.PerReplicaCapacity, vc.K2Source)
	}
	a.mu.Lock()
	after := a.computeCapacityHistory[key].Len()
	_, bound := a.lowerBounds[key]
	a.mu.Unlock()
	if after != before || bound {
		t.Errorf("mid-rollout readings were kept: window %d -> %d readings, lower bound %v", before, after, bound)
	}
}

// A previous figure a parked variant last used three days ago survives a
// restart: it ages on its use, a bucket retention, not on the day a bound
// with a running clock is given.
func TestAParkedVariantsPreviousFigureSurvivesARestart(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	a.mu.Lock()
	a.lowerBounds[key] = &lowerBound{prior: 4000, priorSince: clock.Add(-4 * 24 * time.Hour),
		priorUsed: clock.Add(-3 * 24 * time.Hour), seen: clock.Add(-3 * 24 * time.Hour)}
	a.mu.Unlock()
	saved := a.ExportLearnedState()

	b, _, clock2 := waProcess(mtParams(64, "vllm:v1"))
	b.RestoreLearnedState(saved)
	if vc := waRun(t, b, clock2, 1000, 0); vc.K2Source != capacity.K2SrcPrior.String() || vc.PerReplicaCapacity != 4000 {
		t.Errorf("after the restart: %v from %s, want the previous figure 4000", vc.PerReplicaCapacity, vc.K2Source)
	}
}

// Offering a previous figure to a decision is a use: it is what keeps it.
func TestOfferingAPreviousFigureUsesIt(t *testing.T) {
	a, own, _, clock := lbProcess(t, 9000)
	a.mu.Lock()
	a.lowerBounds[own] = &lowerBound{prior: 3000, priorSince: clock.Add(-time.Hour), priorUsed: clock.Add(-time.Hour), seen: *clock}
	a.mu.Unlock()
	if _, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), false, true); !ok {
		t.Fatal("precondition: the previous figure is offered")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if got := a.lowerBounds[own].priorUsed; !got.Equal(*clock) {
		t.Errorf("offered at %v, last used still %v", *clock, got)
	}
}
