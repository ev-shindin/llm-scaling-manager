package saturation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Specs for the second review of the learned-state redesign: each pins one
// finding.

// r2Rolling marks variant-a mid-rollout to p, as LoadFromScaleTarget leaves
// the record while the scale target reports one.
func r2Rolling(store *capacity.Store, p *capacity.EngineParams) {
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: p, RollingOut: true}, time.Now())
}

// r2Step is how far apart r2Cycles runs its cycles: inside the tick gap.
const r2Step = 4 * time.Minute

// r2Cycles runs n unsaturated cycles of variant-a (1000 tokens, no queue),
// r2Step apart on the clock -- inside the
// tick gap, so a clock that ran would count every one -- and returns the last.
func r2Cycles(t *testing.T, a *SaturationAnalyzer, clock *time.Time, n int) float64 {
	t.Helper()
	var last float64
	for range n {
		last = mtAnalyze(t, a, 1000, 0, nil).PerReplicaCapacity
		*clock = clock.Add(r2Step)
	}
	return last
}

// A rollout to other arguments that stalls -- a surge pod pending with no
// free node, a LeaderWorkerSet held at a partition -- records no saturated
// reading for as long as it lasts, so the lower bound may not run either:
// the variant would relax toward derived or k1 above what its pods
// demonstrably hold, with nothing measured to correct it. Frozen, the bound
// resumes when the rollout ends, with the full hold. Saturated pods
// mid-rollout are not recorded and price nothing: the fleet stays on the
// previous figure.
func TestAStalledRolloutFreezesTheBound(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	r2Rolling(store, mtParams(64, "vllm:v1"))

	// Eight hours of a stalled rollout, four minutes a cycle.
	if got := r2Cycles(t, a, clock, 120); got != 4000 {
		t.Errorf("after eight hours mid-rollout the variant is priced at %v: the bound ran while nothing could be recorded, want 4000", got)
	}
	// A saturated reading below the previous figure prices its own cycle
	// only (unadmittedPrice), and nothing of it is kept.
	for range 5 {
		if vc := mtAnalyze(t, a, 3000, 6, nil); vc.PerReplicaCapacity != 3000 || vc.K2Source != capacity.K2SrcUnadmitted.String() {
			t.Errorf("a saturated reading of 3000 mid-rollout priced %v from %s, want 3000 for its cycle (P1-obs-unadmitted)", vc.PerReplicaCapacity, vc.K2Source)
		}
		*clock = clock.Add(r2Step)
	}
	if got := r2Cycles(t, a, clock, 1); got != 4000 {
		t.Errorf("the next unsaturated cycle mid-rollout: %v, want the previous figure 4000 -- the readings priced only their own cycles", got)
	}
	a.mu.Lock()
	windows := len(a.computeCapacityHistory)
	a.mu.Unlock()
	if windows != 0 {
		t.Errorf("a reading mid-rollout was recorded (%d windows)", windows)
	}

	// The rollout ends: the new arguments get the whole hold, then relax.
	*clock = clock.Add(4 * time.Minute)
	waSetParams(store, mtParams(64, "vllm:v1"))
	if got := r2Cycles(t, a, clock, 7); got != 4000 {
		t.Errorf("25 minutes after the rollout ended: %v, want the full hold at 4000", got)
	}
	if got := r2Cycles(t, a, clock, 30); got <= 4000 {
		t.Errorf("two hours after the rollout ended: %v, want the relaxation under way", got)
	}
}

// The export makes the previous figure a read would make of a window left
// under other arguments from the document alone, so two exports a minute
// apart are the same document: stamped with now(), every export differed,
// and the store rewrote its ConfigMap every minute for as long as such a
// window existed.
func TestAnExportWithAWindowUnderOtherArgumentsIsStable(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1")) // not read since
	first, err := json.Marshal(a.ExportLearnedState())
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	second, err := json.Marshal(a.ExportLearnedState())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("two exports a minute apart differ:\n%s\n%s", first, second)
	}
	var s LearnedState
	if err := json.Unmarshal(second, &s); err != nil {
		t.Fatal(err)
	}
	if lb := s.LowerBounds[rrKey(a, "variant-a")]; lb.Prior != 4000 {
		t.Errorf("the previous figure was not made at export: %+v", lb)
	}
}

// A running bound whose key gets a previous figure at export goes out
// unarmed, where the live path restarts its clock: restored with the clock it
// had, the new arguments got only what was left of it -- or nothing, from a
// bound already spent.
func TestAnExportedPreviousFigureRestartsItsBound(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	from, to := mtParams(256, "vllm:v1"), mtParams(64, "vllm:v1")
	if _, ok := a.borrowedK2(own, "test-model", 12800, from, true, true); !ok {
		t.Fatal("precondition: variant-b borrows")
	}
	rrServe(a, own, clock, 5*time.Hour)
	a.mu.Lock()
	a.computeCapacityHistory[own] = rrWindow(5000)
	a.k2WindowArgs[own] = windowArgs(from)
	a.mu.Unlock()
	a.capacityStore.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: to}, time.Now())

	lb := a.ExportLearnedState().LowerBounds[own]
	if lb.Prior != 5000 {
		t.Fatalf("precondition: the previous figure made at export, got %+v", lb)
	}
	if lb.Armed || !lb.Since.IsZero() {
		t.Errorf("the bound went out armed from %v, five hours into its relaxation", lb.Since)
	}
}

// The first restart after upgrading from a release that recorded no
// arguments -- or past a FingerprintVersion bump -- keeps the variant's k2 as
// a lower-only previous figure, where it used to drop every window in the
// cluster at once.
func TestARestoreFromADocumentWithoutArgumentsKeepsAPreviousFigure(t *testing.T) {
	src, _, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, src, clock, 4000, 6)
	saved := src.ExportLearnedState()
	saved.Args = nil
	saved.Config["test-ns/variant-a"] = "a fingerprint of another version"

	a, _, clock2 := waProcess(mtParams(256, "vllm:v1"))
	a.RestoreLearnedState(saved)
	vc := waRun(t, a, clock2, 1000, 0)
	if vc.K2Source != capacity.K2SrcPrior.String() || vc.PerReplicaCapacity != 4000 {
		t.Errorf("after the upgrade: %v from %s, want the previous figure 4000", vc.PerReplicaCapacity, vc.K2Source)
	}
	if got := a.TakeRestoreOutcomes(); got[string(RestoredDiffers)] != 1 {
		t.Errorf("outcomes %v, want one differs", got)
	}
}

// A previous figure from a PR #151 build's document (priorK2) is installed as
// a lower bound holding it, where LowerBounds has none.
func TestARestoreReadsAPreviousFigureFromAnOlderBuild(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	a.RestoreLearnedState(LearnedState{PriorK2: map[string]PriorK2Snapshot{
		key: {K2: 4000, InstalledAt: clock.Add(-time.Hour)},
	}})
	vc := waRun(t, a, clock, 1000, 0)
	if vc.K2Source != capacity.K2SrcPrior.String() || vc.PerReplicaCapacity != 4000 {
		t.Errorf("an older build's previous figure: %v from %s, want 4000 from P2-prior", vc.PerReplicaCapacity, vc.K2Source)
	}
}

// A previous figure goes a bucket retention after it became one, on an armed
// bound too, which keeps its clock: kept for as long as its bound ran, a
// figure from arguments several generations back was still offered weeks on.
func TestAPreviousFigureAgesOnAnArmedBound(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	if _, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); !ok {
		t.Fatal("precondition: variant-b borrows")
	}
	a.mu.Lock()
	lb := a.lowerBounds[own]
	lb.prior, lb.priorSince = 3000, *clock
	a.mu.Unlock()
	*clock = clock.Add(a.horizons.BucketRetention + time.Hour)
	a.mu.Lock()
	lb.seen = *clock // the variant is still around
	a.mu.Unlock()
	a.EvictStaleHistory()
	a.mu.Lock()
	defer a.mu.Unlock()
	got, kept := a.lowerBounds[own]
	if !kept || !got.armed {
		t.Fatalf("the armed bound itself went with its previous figure: %+v", got)
	}
	if got.prior != 0 {
		t.Errorf("a previous figure a bucket retention old is still held: %d", got.prior)
	}
}

// Under a configuration not read completely the bound records no arguments,
// and refreshing the figure it ran on before rearms read it meant a spent
// bound never re-armed, even on a figure half the one it ran on.
func TestASpentBoundUnderAnUnreadConfigurationReArmsOnALowerFigure(t *testing.T) {
	a, own, donor, clock := lbProcess(t, 4000)
	unread := mtParams(256, "vllm:v1")
	unread.Unresolved = []string{"gpu_memory_utilization"}
	if _, ok := a.borrowedK2(own, "test-model", 12800, unread, true, true); !ok {
		t.Fatal("precondition: variant-b borrows")
	}
	rrServe(a, own, clock, BorrowedK2Hold+BorrowedK2Relax+time.Minute)
	if _, ok := a.borrowedK2(own, "test-model", 12800, unread, true, true); ok {
		t.Fatal("precondition: the bound is spent")
	}
	lbSetDonor(a, donor, 2000)
	*clock = clock.Add(time.Minute)
	if b, ok := a.borrowedK2(own, "test-model", 12800, unread, true, true); !ok || b.k2 != 2000 {
		t.Errorf("the donor fell 4000 -> 2000 and the spent bound did not re-arm: %+v (ok %v)", b, ok)
	}
}

// A sibling borrow already running when a rollout to other arguments starts
// waits for it too: every saturated reading is held back for the rollout, so
// a clock that kept ticking spent the borrow with nothing measured to replace
// it.
func TestARolloutFreezesARunningSiblingBorrow(t *testing.T) {
	store := capacity.NewStore()
	params := &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256}
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params}, time.Now())
	store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params}, time.Now())
	a := NewSaturationAnalyzer(store)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	// b's capacity after n cycles four minutes apart, a saturated at 4000.
	run := func(n int) float64 {
		var b float64
		for range n {
			result, err := a.Analyze(context.Background(), makeAnalyzerInput([]domain.ReplicaMetrics{
				makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50),
				makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50),
			}, states))
			if err != nil {
				t.Fatal(err)
			}
			for _, vc := range result.VariantCapacities {
				if vc.VariantName == states[1].VariantName {
					b = vc.PerReplicaCapacity
				}
			}
			clock = clock.Add(4 * time.Minute)
		}
		return b
	}
	if got := run(5); got != 4000 {
		t.Fatalf("precondition: variant-b borrows 4000 in its hold, got %v", got)
	}
	store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params, RollingOut: true}, time.Now())
	if got := run(120); got != 4000 {
		t.Errorf("after eight hours mid-rollout variant-b is priced at %v: its borrow ran with nothing recorded, want 4000", got)
	}
}
