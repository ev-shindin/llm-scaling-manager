package saturation

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// acParams is variant-a's engine with --max-num-seqs 256 and, when active is
// non-zero, --max-num-active-seqs active.
func acParams(active int64) *capacity.EngineParams {
	p := mtParams(256, "vllm:v1")
	p.MaxNumActiveSeqs = active
	return p
}

// Issue #334: a rollout that changes only --max-num-active-seqs changes how
// far a replica's batch runs, and so the k2 it saturates at. The k2 measured
// under the old cap is not the variant's own figure under the new one: it
// becomes a lower-only previous figure, as for any change of arguments.
func TestAnActiveCapRolloutTurnsTheOldWindowIntoAPreviousFigure(t *testing.T) {
	a, store, clock := waProcess(acParams(0))
	waRun(t, a, clock, 4000, 6)
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity != 4000 || vc.K2Source != capacity.K2SrcHistorical.String() {
		t.Fatalf("precondition: own window at 4000, got %v from %s", vc.PerReplicaCapacity, vc.K2Source)
	}

	waSetParams(store, acParams(64))
	vc := waRun(t, a, clock, 1000, 0)
	if vc.K2Source != capacity.K2SrcPrior.String() {
		t.Errorf("after an active-cap rollout: %v from %s, want a P2-prior", vc.PerReplicaCapacity, vc.K2Source)
	}
	a.mu.Lock()
	windows := len(a.computeCapacityHistory)
	a.mu.Unlock()
	if windows != 0 {
		t.Errorf("the window measured under the old active cap is still read as the variant's own (%d windows)", windows)
	}
}

// The same change made while the controller was down: the restore does not
// install the old k2 as the variant's own.
func TestAnActiveCapChangeWhileDownRestoresOnlyAPreviousFigure(t *testing.T) {
	a, _, clock := waProcess(acParams(0))
	waRun(t, a, clock, 4000, 6)
	saved := a.ExportLearnedState()

	second, _, _ := waProcess(acParams(64))
	second.RestoreLearnedState(saved)
	r, done := second.installRestored("test-ns", "test-model", "variant-a")
	if !done {
		t.Fatal("precondition: the restore was not checked")
	}
	if r.Installed || r.Outcome != RestoredPrior {
		t.Errorf("a restore across an active-cap change: installed=%v outcome=%s, want outcome %s and nothing installed",
			r.Installed, r.Outcome, RestoredPrior)
	}
}
