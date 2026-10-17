package saturation

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// What starts, stops and restarts a lower bound's clock.

// A previous figure is one measurement. A change of arguments that leaves no
// newer one behind -- nothing saturated under the arguments just left -- is
// not new evidence about it, so its clock runs on: restarted on every change,
// a figure measured once kept a full hold for as long as the arguments kept
// changing faster than the bound relaxed.
func TestAPreviousFigureIsNotReArmedByAChangeThatMeasuredNothing(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	if got := waRun(t, a, clock, 1000, 0); got.PerReplicaCapacity != 4000 {
		t.Fatalf("precondition: priced at the previous figure, got %v", got.PerReplicaCapacity)
	}
	var last domain.VariantCapacity
	for _, seqs := range []int64{96, 128, 160} {
		for range 120 {
			last = waRun(t, a, clock, 1000, 0)
		}
		waSetParams(store, mtParams(seqs, "vllm:v1"))
	}
	for range 120 {
		last = waRun(t, a, clock, 1000, 0)
	}
	// Eight hours of serving on one figure: fully relaxed to the variant's
	// own capacity. Re-armed at every change it stays near 4000.
	if last.PerReplicaCapacity < 12000 {
		t.Errorf("after 8h of serving on one measurement, priced at %v: the changes of arguments re-armed it", last.PerReplicaCapacity)
	}
}

// One Ready replica with no work -- nothing resident, nothing queued, nothing
// completed -- is not the variant using its bound: an idle night at
// minReplicas does not spend the hold the morning's traffic needs.
func TestAnIdleReplicaDoesNotRunTheBoundsClock(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	if got := waRun(t, a, clock, 1000, 0); got.PerReplicaCapacity != 4000 {
		t.Fatalf("precondition: priced at the previous figure, got %v", got.PerReplicaCapacity)
	}
	for range 7 * 60 {
		waRun(t, a, clock, 0, 0)
	}
	if got := waRun(t, a, clock, 1000, 0); got.PerReplicaCapacity != 4000 {
		t.Errorf("after an idle night the bound priced %v: the idle replica spent its hold", got.PerReplicaCapacity)
	}
}

// A newer previous figure restarts a running bound with its whole hold, even
// where the bound already ran under the arguments the variant comes back to
// (A -> B -> A -> B): there the change of arguments shows nothing, and the
// time to the first serving cycle -- the rollout -- was read as a gap.
func TestANewerPreviousFigureRestartsTheBoundWithItsWholeHold(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	b := mtParams(64, "vllm:v1")
	a.mu.Lock()
	a.lowerBounds[key] = &lowerBound{
		armed: true, since: *clock, tick: *clock, seen: *clock,
		donor: "variant-a", own: true, donorK2: 4000, args: b.ArgsFingerprint(), argsVersion: capacity.FingerprintVersion,
		prior: 4000, priorSince: *clock, priorUsed: *clock,
	}
	a.notePreviousFigureLocked(key, rrWindow(3000))
	a.mu.Unlock()

	*clock = clock.Add(time.Hour) // the rollout to B
	got, ok := a.borrowedK2(key, "test-model", 12800, b, true, true)
	if !ok || got.k2 != 3000 || !got.own {
		t.Fatalf("want the newer previous figure 3000, got %+v ok=%v", got, ok)
	}
	if got.age != 0 {
		t.Errorf("the restarted bound starts %v into its hold, want 0", got.age)
	}
}

// A Pod of the variant's own that is not Ready is priced at the chain, mid-
// rollout as outside one: an old pod draining, or a new one failing its
// readiness probe with its gauges frozen, is not what the variant holds now.
// A Ready replica mid-rollout is priced at its saturated reading.
func TestANotReadyPodMidRolloutIsPricedAtTheChain(t *testing.T) {
	price := func(ready bool) (int64, capacity.K2Source) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		rm := makeReplicaMetrics("pod-a", "variant-a", 3000, 16000, 6, 100, 50)
		rm.Ready = ready
		adm := admissionOf(rm)
		adm.rollout = true
		d := a.computeK2("k", "test-model", "test-ns", "variant-a", 6, 3000, 50, 100, 5,
			nil, 1, 12800, 16000, domain.RoleDecode, false, adm, false, logr.Discard())
		return d.k2, d.src
	}
	if k2, src := price(true); k2 != 3000 || src != capacity.K2SrcUnadmitted {
		t.Fatalf("precondition: a Ready replica mid-rollout is priced at its reading, got %d from %s", k2, src)
	}
	if k2, src := price(false); k2 == 3000 || src == capacity.K2SrcUnadmitted {
		t.Errorf("a not-Ready pod mid-rollout was priced at its frozen reading: %d from %s", k2, src)
	}
}

// Until the stored state is in (HoldLowerBounds), a serving cycle reads a
// bound at full strength but records none: recorded, it was live, and the
// restore kept it over the stored one -- a spent borrow came back at full
// strength for another hold and relaxation.
func TestABoundHeldUntilTheRestoreLosesNothingToIt(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	p := mtParams(256, "vllm:v1")
	a.HoldLowerBounds(true)
	got, ok := a.borrowedK2(own, "test-model", 12800, p, true, true)
	if !ok || got.k2 != 4000 || got.relaxed != 0 {
		t.Fatalf("a held bound is read at full strength, got %+v ok=%v", got, ok)
	}
	if n := len(borrowClocks(a)); n != 0 {
		t.Fatalf("a held serving cycle armed %d bound(s)", n)
	}
	a.RestoreLearnedState(LearnedState{LowerBounds: map[string]LowerBoundSnapshot{own: {
		Armed: true, Since: clock.Add(-7 * time.Hour), Tick: *clock, Seen: *clock,
		Donor: "variant-a", DonorK2: 4000, Offered: 4000,
		Args: p.ArgsFingerprint(), ArgsVersion: capacity.FingerprintVersion,
	}}})
	a.HoldLowerBounds(false)
	if got, ok := a.borrowedK2(own, "test-model", 12800, p, true, true); ok {
		t.Errorf("the stored spent bound lost to one armed before the restore: %+v", got)
	}
}

// A forget acted on before a delayed restore leaves the stored running bound
// spent, as it would have had the restore come first: dropped, the next
// serving cycle armed the sibling's figure afresh, at full strength.
func TestAForgetBeforeTheRestoreEndsTheStoredBorrow(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	p := mtParams(256, "vllm:v1")
	a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
	a.RestoreLearnedState(LearnedState{LowerBounds: map[string]LowerBoundSnapshot{own: {
		Armed: true, Since: clock.Add(-10 * time.Minute), Tick: *clock, Seen: *clock,
		Donor: "variant-a", DonorK2: 4000, Offered: 4000, Prior: 3000, PriorSince: *clock, PriorUsed: *clock,
		Args: p.ArgsFingerprint(), ArgsVersion: capacity.FingerprintVersion,
	}}})
	if got, ok := a.borrowedK2(own, "test-model", 12800, p, true, true); ok {
		t.Errorf("the forgotten variant borrows again: %+v", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if lb := a.lowerBounds[own]; lb == nil || lb.prior != 0 {
		t.Errorf("want the stored bound kept spent with no previous figure, got %+v", lb)
	}
}

// A restart between a change of arguments and the first serving cycle after
// it keeps the restart pending, so the new arguments still get their whole
// hold.
func TestAPendingRestartSurvivesARestart(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	a.mu.Lock()
	a.lowerBounds[key] = &lowerBound{
		armed: true, since: *clock, tick: *clock, seen: *clock, rearmPending: true,
		donor: "variant-a", own: true, donorK2: 4000, prior: 4000, priorSince: *clock, priorUsed: *clock,
	}
	a.mu.Unlock()
	second, _, _ := waProcess(mtParams(64, "vllm:v1"))
	second.RestoreLearnedState(a.ExportLearnedState())
	second.mu.Lock()
	defer second.mu.Unlock()
	if lb := second.lowerBounds[key]; lb == nil || !lb.rearmPending {
		t.Errorf("the pending restart was lost across a restart: %+v", lb)
	}
}

// The restart completes once: the bound then relaxes as any other.
func TestARestartedBoundRelaxesAfterItsHold(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	b := mtParams(64, "vllm:v1")
	a.mu.Lock()
	a.lowerBounds[key] = &lowerBound{
		armed: true, since: *clock, tick: *clock, seen: *clock,
		donor: "variant-a", own: true, donorK2: 4000, args: b.ArgsFingerprint(), argsVersion: capacity.FingerprintVersion,
		prior: 4000, priorSince: *clock, priorUsed: *clock,
	}
	a.notePreviousFigureLocked(key, rrWindow(3000))
	a.mu.Unlock()
	var got k2Borrow
	for range 120 {
		*clock = clock.Add(time.Minute)
		got, _ = a.borrowedK2(key, "test-model", 12800, b, true, true)
	}
	if got.relaxed <= 0 {
		t.Errorf("two hours after the restart the bound has not relaxed: %+v", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lowerBounds[key].rearmPending {
		t.Error("the restart is still pending after the bound re-armed")
	}
}

// A restart waiting for a previous figure goes with the figure: the variant's
// own window retired it here, by a rollback, and a mark left behind re-armed
// the sibling borrow -- spent again since -- at full strength once the window
// was swept, on no new evidence.
func TestARetiredPreviousFigureLeavesNoPendingRestart(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	lbSpend(t, a, own, clock)
	argsA, argsB := mtParams(256, "vllm:v1").ArgsFingerprint(), mtParams(64, "vllm:v1").ArgsFingerprint()
	a.mu.Lock()
	a.computeCapacityHistory[own] = rrWindow(9000)
	a.k2WindowArgs[own] = argsA
	a.k2WindowUnderLocked(own, argsB)
	if lb := a.lowerBounds[own]; lb == nil || !lb.rearmPending {
		a.mu.Unlock()
		t.Fatal("precondition: the change of arguments left a restart pending")
	}
	a.k2WindowUnderLocked(own, argsA)
	a.mu.Unlock()
	// The change of arguments restarted the sibling borrow: new evidence.
	// It runs its course again while the variant serves on its own window.
	mtServe(a, clock, BorrowedK2Hold+BorrowedK2Relax+time.Minute)
	a.mu.Lock()
	delete(a.computeCapacityHistory, own)
	delete(a.k2WindowArgs, own)
	a.mu.Unlock()
	if got, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); ok {
		t.Errorf("the spent borrow re-armed on a stale pending restart: %+v", got)
	}
}

// The legacy form of a running borrow (a start alone) of a variant forgotten
// before the restore is installed spent too.
func TestAForgetBeforeTheRestoreEndsALegacyBorrowClock(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
	a.RestoreLearnedState(LearnedState{K2BorrowedSince: map[string]time.Time{own: clock.Add(-10 * time.Minute)}})
	if got, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); ok {
		t.Errorf("the forgotten variant borrows again: %+v", got)
	}
}
