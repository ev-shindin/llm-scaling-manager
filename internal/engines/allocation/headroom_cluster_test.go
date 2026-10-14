package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// The warm pool's headroom takes the closed cluster-scope bounds the optimizer
// takes. With only a physical limiter the pool used to read "unbounded" -- no
// provider had namespace pools -- and asked for Pods the nodes could not hold.
func TestWarmPoolHeadroomIsBoundedByAPhysicalPool(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	now := time.Now()

	physical := &ResourceConstraints{ProviderName: "gpu-limiter", PoolsClosed: true,
		Pools: map[string]ResourcePool{"H100": {Limit: 8, Used: 6}}}
	PublishNamespaceHeadroom([]*ResourceConstraints{physical}, now)

	free, state := decision.GPUHeadroomReading("tenant", "NVIDIA-H100-80GB-HBM3", 0, time.Minute, now)
	if state != decision.HeadroomBounded || free != 2 {
		t.Fatalf("headroom (%d, %v), want (2, bounded): the physical pool has 2 free", free, state)
	}
	if free, state := decision.GPUHeadroomReading("tenant", "A100", 0, time.Minute, now); state != decision.HeadroomBounded || free != 0 {
		t.Fatalf("A100 headroom (%d, %v), want (0, bounded): a closed pool that lists no A100 has none", free, state)
	}

	// A namespace's own allowance is bounded by the cluster figure too.
	quota := NewDefaultLimiter("tenant-quota", NewQuotaInventory(config.QuotaLimiterConfig{
		Name: "tenant-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"tenant": {"H100": 4}},
	}))
	qc, err := quota.ComputeConstraints(context.Background(), map[string]int{}, map[string]map[string]int{"tenant": {}})
	if err != nil {
		t.Fatal(err)
	}
	PublishNamespaceHeadroom([]*ResourceConstraints{physical, qc}, now)
	if free, state := decision.GPUHeadroomReading("tenant", "H100", 0, time.Minute, now); state != decision.HeadroomBounded || free != 2 {
		t.Fatalf("headroom (%d, %v), want (2, bounded): a 4-GPU allowance on a cluster with 2 free leaves 2", free, state)
	}
}

// A namespace allowance of -1 (unlimited) for a type is unbounded headroom for
// a warm pool, not zero. It was skipped, which left the type absent from a
// namespace that is present -- and an absent type reads as denied, so a pool
// under an unlimited allowance never grew. A closed cluster bound still caps it.
func TestWarmPoolHeadroomForAnUnlimitedAllowance(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	now := time.Now()

	quota := NewDefaultLimiter("tenant-quota", NewQuotaInventory(config.QuotaLimiterConfig{
		Name: "tenant-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"tenant": {"H100": config.QuotaUnlimited, "A100": 2}},
	}))
	qc, err := quota.ComputeConstraints(context.Background(), map[string]int{}, map[string]map[string]int{"tenant": {}})
	if err != nil {
		t.Fatal(err)
	}
	PublishNamespaceHeadroom([]*ResourceConstraints{qc}, now)
	if free, state := decision.GPUHeadroomReading("tenant", "H100", 0, time.Minute, now); state != decision.HeadroomUnbounded {
		t.Fatalf("H100 headroom (%d, %v), want unbounded: the allowance is unlimited", free, state)
	}
	if free, state := decision.GPUHeadroomReading("tenant", "A100", 0, time.Minute, now); state != decision.HeadroomBounded || free != 2 {
		t.Fatalf("A100 headroom (%d, %v), want (2, bounded)", free, state)
	}
	if _, state := decision.GPUHeadroomReading("tenant", "L4", 0, time.Minute, now); state != decision.HeadroomBounded {
		t.Fatalf("L4 is not on the allowlist and must stay denied, read %v", state)
	}

	physical := &ResourceConstraints{ProviderName: "gpu-limiter", PoolsClosed: true,
		Pools: map[string]ResourcePool{"H100": {Limit: 8, Used: 5}, "A100": {Limit: 4}}}
	PublishNamespaceHeadroom([]*ResourceConstraints{physical, qc}, now)
	if free, state := decision.GPUHeadroomReading("tenant", "H100", 0, time.Minute, now); state != decision.HeadroomBounded || free != 3 {
		t.Fatalf("H100 headroom (%d, %v), want (3, bounded): unlimited, capped by the 3 GPUs free on the nodes", free, state)
	}
}
