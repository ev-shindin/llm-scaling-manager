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
