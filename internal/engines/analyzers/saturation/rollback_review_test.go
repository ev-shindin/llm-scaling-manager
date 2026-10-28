package saturation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Rollback and re-roll rules a review of the learned-state follow-ups found
// unpinned: each test fails with the rule it names reverted.

func roundTrip(t *testing.T, s LearnedState) LearnedState {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc LearnedState
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// A restart between the rollback and the re-roll keeps the bound's record of
// the figure it ran its course on, so the re-roll after the restore does not
// re-arm it.
func TestARestartBetweenRollbackAndReRollDoesNotReArmTheSameFigure(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	for range 7 * 60 {
		waRun(t, a, clock, 1000, 0)
	}
	if got := waRun(t, a, clock, 1000, 0); got.PerReplicaCapacity < 12000 {
		t.Fatalf("precondition: the bound has run its course, got %v", got.PerReplicaCapacity)
	}
	waSetParams(store, mtParams(256, "vllm:v1"))
	if got := waRun(t, a, clock, 1000, 0); got.PerReplicaCapacity != 4000 {
		t.Fatalf("precondition: the rollback gets its own window back, got %v", got.PerReplicaCapacity)
	}
	doc := roundTrip(t, a.ExportLearnedState())
	b, storeB, clockB := waProcess(mtParams(256, "vllm:v1"))
	*clockB = *clock
	b.RestoreLearnedState(doc)
	if got := waRun(t, b, clockB, 1000, 0); got.PerReplicaCapacity != 4000 {
		t.Fatalf("precondition: the restored rollback prices from its own window, got %v", got.PerReplicaCapacity)
	}
	waSetParams(storeB, mtParams(64, "vllm:v1"))
	if got := waRun(t, b, clockB, 1000, 0); got.PerReplicaCapacity < 12000 {
		t.Errorf("the re-roll after a restore re-armed the spent figure: priced at %v", got.PerReplicaCapacity)
	}
}

// A window written slightly ahead of this leader's clock is clamped to now on
// restore while the bound keeps its record of it: the same window, not
// written since, is still the same figure, and the re-roll leaves the spent
// bound's clock alone. A document with no record (an older build) restarts
// it, as before.
func TestASkewedWindowClockIsStillTheSameFigure(t *testing.T) {
	probe := func(t *testing.T, offset time.Duration, strip bool) bool {
		t.Helper()
		a, _, clock := waProcess(mtParams(64, "vllm:v1"))
		wall := time.Now()
		*clock = wall
		key := a.historyKey("test-model", "test-ns", "variant-a", "H100", 1, "decode", 1000, 5)
		argsA, argsB := mtParams(256, "vllm:v1").ArgsFingerprint(), mtParams(64, "vllm:v1").ArgsFingerprint()
		w := wall.Add(offset)
		lb := LowerBoundSnapshot{
			Armed: true, Own: true, Since: wall.Add(-10 * time.Hour), Tick: wall.Add(-time.Minute),
			Seen: wall.Add(-time.Minute), FigureWritten: w,
		}
		if strip {
			lb.FigureWritten = time.Time{}
		}
		a.RestoreLearnedState(roundTrip(t, LearnedState{
			K2Stash:     map[string]capacity.RollingAverageSnapshot{stashKey(key, argsA): {Values: []float64{4000}, LastUsed: w, LastWritten: w}},
			LowerBounds: map[string]LowerBoundSnapshot{key: lb},
		}))
		a.mu.Lock()
		defer a.mu.Unlock()
		bound := a.lowerBounds[key]
		if bound == nil {
			t.Fatal("precondition: the bound was installed")
		}
		before := bound.since
		a.k2WindowUnderLocked(key, argsA)
		if _, ok := a.computeCapacityHistory[key]; !ok {
			t.Fatal("precondition: the rollback took the window back")
		}
		a.k2WindowUnderLocked(key, argsB)
		return bound.rearmPending || !bound.since.Equal(before)
	}
	if probe(t, -5*time.Minute, false) {
		t.Error("precondition: a window written in the past is the same figure")
	}
	if probe(t, 30*time.Second, false) {
		t.Error("a window written 30s ahead of this leader's clock re-armed the spent figure")
	}
	if !probe(t, -5*time.Minute, true) {
		t.Error("a bound from a document with no record of its figure did not restart, as it did before")
	}
}

// The throughput window a rollback took back keeps its hold only while it is
// not written since: measured again under the rolled-back arguments, it is a
// newer figure, and the re-roll gives it a fresh hold.
func TestARollbackThatMeasuresAgainGivesTheReRollAFreshHold(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key := a.throughputKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleDecode, 1000, 1000, 5)
	argsA, argsB := mtParams(256, "vllm:v1").ArgsFingerprint(), mtParams(64, "vllm:v1").ArgsFingerprint()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saturatedThroughput[key] = rrWindow(3)
	a.throughputWindowArgs[key] = argsA
	a.throughputWindowUnderLocked(key, argsB)
	st := a.throughputStash[stashKey(key, argsA)]
	st.heldFor = 7 * time.Hour
	a.throughputStash[stashKey(key, argsA)] = st
	a.throughputWindowUnderLocked(key, argsA)
	time.Sleep(time.Millisecond)
	a.saturatedThroughput[key].AddAt(4, time.Now())
	a.throughputWindowUnderLocked(key, argsB)
	if got := a.throughputStash[stashKey(key, argsA)].heldFor; got != 0 {
		t.Errorf("a window measured again under the rollback kept the old hold: %v", got)
	}
}

// A held mark goes with its window: once the window a rollback took back is
// gone, the sweep drops it.
func TestTheSweepDropsAHeldMarkWithItsWindow(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key := a.throughputKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleDecode, 1000, 1000, 5)
	argsA := mtParams(256, "vllm:v1").ArgsFingerprint()
	a.mu.Lock()
	a.saturatedThroughput[key] = rrWindow(3)
	a.throughputWindowArgs[key] = argsA
	a.throughputHeld[stashKey(key, argsA)] = heldMark{heldFor: time.Hour, written: time.Now()}
	a.throughputHeld[stashKey("gone", argsA)] = heldMark{heldFor: time.Hour, written: time.Now()}
	a.mu.Unlock()
	a.EvictStaleHistory()
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.throughputHeld[stashKey(key, argsA)]; !ok {
		t.Error("the sweep dropped the mark of a live window")
	}
	if _, ok := a.throughputHeld[stashKey("gone", argsA)]; ok {
		t.Error("the mark of a window that is gone was kept")
	}
}

// A variant still pending its first cycle after one restart keeps its
// put-aside ITL baseline through a write and a second restart; a forget
// before the write drops it.
func TestAPendingVariantsPutAsideITLBaselineSurvivesASecondRestart(t *testing.T) {
	first, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key, argsA, argsB := itlStashKeyAndArgs(first)
	first.itlUnderArgs(key, argsA)
	first.itlTracker.SetBaseline(key, 0.0012)
	first.itlUnderArgs(key, argsB)
	first.mu.Lock()
	first.variantSeenAt[key] = time.Now()
	first.mu.Unlock()
	doc := roundTrip(t, first.ExportLearnedState())

	pending, _, _ := waProcess(mtParams(64, "vllm:v1"))
	pending.RestoreLearnedState(doc)
	third, _, _ := waProcess(mtParams(64, "vllm:v1"))
	third.RestoreLearnedState(roundTrip(t, pending.ExportLearnedState()))
	if _, done := third.installRestored("test-ns", "test-model", "variant-a"); !done {
		t.Fatal("precondition: the variant's entries were pending")
	}
	third.itlUnderArgs(key, argsB)
	third.itlUnderArgs(key, argsA)
	if b, ok := third.itlBaselines()[key]; !ok || b != 0.0012 {
		t.Errorf("a second restart before the variant's first cycle lost the put-aside baseline: %v %v", b, ok)
	}

	forgotten, _, _ := waProcess(mtParams(64, "vllm:v1"))
	forgotten.RestoreLearnedState(doc)
	forgotten.ForgetVariant("test-ns", "test-model", "variant-a", "t1")
	for k := range forgotten.ExportLearnedState().ITLBaselineStash {
		t.Errorf("a forgotten variant's put-aside baseline was written back: %s", k)
	}
}
