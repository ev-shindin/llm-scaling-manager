package saturation

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// A sibling's k2 is a candidate only when its measured KV cache total is
// within BorrowKvTotalTolerance of the borrower's. Compatible parameters do
// not guarantee the same cache: a flag no field reads, or another checkpoint
// under the same path, sizes it differently, and a k2 is a token count.
func TestASiblingWithAnotherKvTotalIsNotADonor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		own, donorKv  int64 // measured KV totals; 0 = not measured
		wantCandidate bool
	}{
		{"both measured, equal", 100000, 100000, true},
		{"both measured, 2% apart", 100000, 98000, true},
		{"both measured, 3% apart", 100000, 97000, true},
		{"both measured, 5% apart", 100000, 95000, false},
		{"both measured, donor twice the cache", 100000, 200000, false},
		{"borrower not measured", 0, 200000, true},
		{"donor not measured", 100000, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, own, _, _ := lbProcess(t, 3000)
			a.capacityStore.Update("test-ns", "test-model", "variant-a",
				capacity.Record{EngineParams: mtParams(256, "vllm:v1"), TotalKvCapacityTokens: tc.donorKv}, time.Now())
			a.capacityStore.Update("test-ns", "test-model", "variant-b",
				capacity.Record{EngineParams: mtParams(256, "vllm:v1"), TotalKvCapacityTokens: tc.own}, time.Now())
			b, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true)
			if ok != tc.wantCandidate {
				t.Fatalf("borrowed = %v (%+v), want %v", ok, b, tc.wantCandidate)
			}
			if ok && b.donor != "variant-a" {
				t.Errorf("donor = %q, want variant-a", b.donor)
			}
		})
	}
}

// The gate refuses siblings only. The variant's own previous figure was
// measured on its own cache, and the gate does not reach it.
func TestTheKvTotalGateDoesNotReachTheVariantsOwnPreviousFigure(t *testing.T) {
	a, own, donor, _ := lbProcess(t, 3500)
	a.mu.Lock()
	delete(a.computeCapacityHistory, donor)
	a.lowerBounds[own] = &lowerBound{prior: 5000, priorSince: a.now()}
	a.mu.Unlock()
	a.capacityStore.Update("test-ns", "test-model", "variant-b",
		capacity.Record{EngineParams: mtParams(256, "vllm:v1"), TotalKvCapacityTokens: 100000}, time.Now())
	b, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true)
	if !ok || !b.own || b.donorK2 != 5000 {
		t.Fatalf("borrowed = %v %+v, want the variant's own previous figure 5000", ok, b)
	}
}
