package steadystate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// physicalStub is a closed physical constraint provider (the default usage
// basis) with a fixed answer, or a fixed error.
type physicalStub struct {
	pools map[string]allocation.ResourcePool
	err   error
}

func (physicalStub) Name() string { return "gpu-limiter" }
func (p physicalStub) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &allocation.ResourceConstraints{ProviderName: "gpu-limiter", Pools: p.pools}, nil
}

func resetUsageAndHeadroom(t *testing.T) {
	t.Helper()
	reset := func() {
		decision.DefaultGPUUsage.Reset()
		decision.ResetWarmPoolGPUs()
		decision.DefaultHeadroom.Reset()
	}
	reset()
	t.Cleanup(reset)
}

// One missing input never lifts the others. With a physical limiter and a
// quota, a physical view not yet observed used to drop EVERY constraint and run
// the cycle unbounded; the quota must still bound it.
func TestSelectV2OptimizerSkipsAnUnobservedProviderOnly(t *testing.T) {
	resetUsageAndHeadroom(t) // no physical observation

	e := &Engine{
		GPULimiter: allocation.NewCompositeLimiter("gpu-and-quota-limiter", []allocation.Limiter{
			physicalStub{pools: map[string]allocation.ResourcePool{"H100": {Limit: 16}}},
			quotaLimiterFor("tenant", "H100", 2),
		}),
		optimizer: allocation.NewGreedyByScoreOptimizer(),
	}
	opt, constraints := e.selectV2Optimizer(context.Background(), nil)
	if _, ok := opt.(*allocation.GreedyByScoreOptimizer); !ok {
		t.Fatalf("optimizer = %s, want greedy-by-score: the quota can still bound the cycle", opt.Name())
	}
	if len(constraints) != 1 || constraints[0].ProviderName != "test-quota" {
		t.Fatalf("constraints = %+v, want the quota's alone", constraints)
	}
}

// A provider that errors is skipped and the others still bound.
func TestSelectV2OptimizerSkipsAFailingProviderOnly(t *testing.T) {
	resetUsageAndHeadroom(t)
	decision.PublishGPUUsage(map[string]int{}, map[string]map[string]int{})

	e := &Engine{
		GPULimiter: allocation.NewCompositeLimiter("gpu-and-quota-limiter", []allocation.Limiter{
			physicalStub{err: errors.New("nodes is forbidden")},
			quotaLimiterFor("tenant", "H100", 2),
		}),
		optimizer: allocation.NewGreedyByScoreOptimizer(),
	}
	opt, constraints := e.selectV2Optimizer(context.Background(), nil)
	if _, ok := opt.(*allocation.GreedyByScoreOptimizer); !ok || len(constraints) != 1 {
		t.Fatalf("optimizer %s with %d constraints, want greedy-by-score bounded by the quota", opt.Name(), len(constraints))
	}
}

// Only when every provider is unavailable does the cycle go unchecked.
func TestSelectV2OptimizerGoesUncheckedOnlyWhenEveryProviderIs(t *testing.T) {
	resetUsageAndHeadroom(t) // physical unobserved; the physical stub is the only provider

	e := &Engine{
		GPULimiter: physicalStub{pools: map[string]allocation.ResourcePool{"H100": {Limit: 16}}},
		optimizer:  allocation.NewGreedyByScoreOptimizer(),
	}
	opt, constraints := e.selectV2Optimizer(context.Background(), nil)
	if _, ok := opt.(*allocation.CostAwareOptimizer); !ok || constraints != nil {
		t.Fatalf("optimizer %s with constraints %+v, want the unlimited path", opt.Name(), constraints)
	}
}

// The idle-fleet headroom follows the same rule: the warm pool beside a quota
// was told "unbounded" while the physical view was unobserved.
func TestIdleFleetHeadroomSkipsAnUnobservedProviderOnly(t *testing.T) {
	resetUsageAndHeadroom(t)
	// A pool holds one of the two GPUs allowed, which also puts the namespace
	// in the usage the quota materializes its caps for.
	decision.PublishWarmPoolGPUs("tenant", map[string]int{"H100": 1})

	e := &Engine{GPULimiter: allocation.NewCompositeLimiter("gpu-and-quota-limiter", []allocation.Limiter{
		physicalStub{pools: map[string]allocation.ResourcePool{"H100": {Limit: 16}}},
		quotaLimiterFor("tenant", "H100", 2),
	})}
	e.publishHeadroomForIdleFleet(context.Background())

	free, known := decision.GPUHeadroom("tenant", "H100", time.Minute, time.Now())
	if !known || free != 1 {
		t.Fatalf("headroom (%d, %v), want (1, true): the quota still bounds the pool", free, known)
	}
}
