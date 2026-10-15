package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// Review finding: the fallback snapshot was not debited per wake, so while
// every provider was unavailable each wake in the snapshot's five-minute life
// passed on the same free GPUs. Two 8-GPU wakes against 8 free H100s both woke.
// Each admitted wake is now charged to the snapshot, and the second is refused.
func TestSnapshotIsDebitedPerWake(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 0}, nil)

	e := &Engine{gpuLimiter: failingProvider{name: "gpu-limiter"}}
	e.rememberConstraints("chat", []*allocation.ResourceConstraints{{ProviderName: "gpu-limiter",
		Pools: map[string]allocation.ResourcePool{"H100": {Limit: 8}}}}, time.Now().Add(-time.Minute))

	wake := func(model string) SelectionOutcome {
		t.Helper()
		place := e.placementFor(ctx, "chat")
		if place.snapshotTakenAt.IsZero() {
			t.Fatalf("%s: the young snapshot was not used", model)
		}
		group := modelGroup{namespace: "chat", modelID: model}
		selected, outcome := e.selectOrClaim(ctx, group,
			[]Candidate{cand(model+"-decode", "decode", "H100", 8, 1)}, coverage{}, place)
		if len(selected) > 0 {
			e.recordWake(ctx, group, place, selected)
		}
		return outcome
	}

	if got := wake("first"); got == OutcomeNoCapacity {
		t.Fatalf("first wake refused (%s); 8 H100s are free in the snapshot", got)
	}
	if got := wake("second"); got != OutcomeNoCapacity {
		t.Fatalf("second wake = %s, want %s: the first took the snapshot's 8 free H100s", got, OutcomeNoCapacity)
	}

	// The debit belongs to that snapshot: a fresh one starts from what it measures.
	e.rememberConstraints("chat", []*allocation.ResourceConstraints{{ProviderName: "gpu-limiter",
		Pools: map[string]allocation.ResourcePool{"H100": {Limit: 16}}}}, time.Now())
	if p := e.placementFor(ctx, "chat"); len(p.admitted) != 0 {
		t.Fatalf("a new snapshot inherited the old one's debit: %v", p.admitted)
	}
}

// With limiters declared, every provider unavailable and no snapshot, the
// placement says so, and recordWake counts the uncapped wake.
func TestPlacementMarksAnUncheckedWake(t *testing.T) {
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 0}, nil)

	e := &Engine{gpuLimiter: failingProvider{name: "gpu-limiter"}}
	if p := e.placementFor(context.Background(), "chat"); !p.unchecked || p.constraints != nil {
		t.Fatalf("placement = %+v, want unchecked with no constraints", p)
	}
	e = &Engine{}
	if p := e.placementFor(context.Background(), "chat"); p.unchecked {
		t.Fatal("no limiter declared is not an unchecked wake: nothing asked for a bound")
	}
}
