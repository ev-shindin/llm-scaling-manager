package saturation

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Specs for the sixth review of the learned-state redesign: each pins one
// finding.

// A bucket a rollout reaches with no window and no previous figure: its
// saturated reading prices the replica for that cycle, lower-only, and
// nothing of it is kept. Falling through, the replica was priced at derived
// -- or k1 for prefill -- in the very cycle that showed it full, for as long
// as the rollout lasted; main recorded the reading and priced it.
func TestARolloutWithNoFigurePricesASaturatedReadingForItsCycle(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v2"), RollingOut: true}, time.Now())
	key := rrKey(a, "variant-a")

	vc := waRun(t, a, clock, 3000, 6)
	if vc.PerReplicaCapacity != 3000 || vc.K2Source != capacity.K2SrcUnadmitted.String() {
		t.Errorf("mid-rollout, no window, saturated at 3000: priced %v from %s, want 3000 (P1-obs-unadmitted)", vc.PerReplicaCapacity, vc.K2Source)
	}
	a.mu.Lock()
	window, bound, mu := a.computeCapacityHistory[key], a.lowerBounds[key], len(a.saturatedThroughput)
	a.mu.Unlock()
	if (window != nil && window.Len() > 0) || bound != nil || mu != 0 {
		t.Errorf("the reading was kept: window %v, lower bound %v, throughput windows %d", window, bound, mu)
	}
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity <= 3000 {
		t.Errorf("the next cycle, not saturated: priced %v from %s; the reading should have priced only its own cycle", vc.PerReplicaCapacity, vc.K2Source)
	}
}

// A bound the export disarms to carry a new previous figure takes its legacy
// borrow clock with it. A build that reads only K2BorrowedSince and PriorK2
// (main, #151) ran the new figure on the old borrow's clock, five hours into
// its relaxation, and relaxed it early.
func TestAnExportedPreviousFigureLeavesNoLegacyClock(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	from, to := mtParams(256, "vllm:v1"), mtParams(64, "vllm:v1")
	if _, ok := a.borrowedK2(own, "test-model", 12800, from, true, true); !ok {
		t.Fatal("precondition: variant-b borrows")
	}
	rrServe(a, own, clock, 5*time.Hour)
	if _, ok := a.ExportLearnedState().K2BorrowedSince[own]; !ok {
		t.Fatal("precondition: a running sibling borrow is written as a legacy clock")
	}
	a.mu.Lock()
	a.computeCapacityHistory[own] = rrWindow(5000)
	a.k2WindowArgs[own] = windowArgs(from)
	a.mu.Unlock()
	a.capacityStore.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: to}, time.Now())

	s := a.ExportLearnedState()
	if s.LowerBounds[own].Prior != 5000 {
		t.Fatalf("precondition: the previous figure made at export, got %+v", s.LowerBounds[own])
	}
	if since, ok := s.K2BorrowedSince[own]; ok {
		t.Errorf("the disarmed bound's legacy clock went out (%v): an older build relaxes the new previous figure on it", since)
	}
}
