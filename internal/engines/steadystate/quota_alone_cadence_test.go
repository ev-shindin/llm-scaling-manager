package steadystate

import (
	"context"
	"testing"

	"github.com/go-logr/logr/funcr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// The transitional shadow pass runs once per 10 eligible cycles, and its note
// fires when the set of differing variants changes -- not when their targets
// move with load, which under load is nearly every cycle.
func TestReportQuotaAloneAllocationCadence(t *testing.T) {
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"default": {Limiters: []config.QuotaLimiterConfig{
			{Type: "gpu-inventory", Name: "gpu"},
			{Type: "quota", Name: "test-quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"tenant": {"A100": 8}}},
		}},
	})
	quota := quotaLimiterFor("tenant", "A100", 8)
	stub := physicalStub{pools: map[string]allocation.ResourcePool{"A100": {Limit: 2}}}
	e := &Engine{Config: cfg, GPULimiter: allocation.NewCompositeLimiter("gpu-and-quota-limiter", []allocation.Limiter{stub, quota})}
	qc, err := quota.(allocation.ConstraintProvider).ComputeConstraints(context.Background(), map[string]int{}, map[string]map[string]int{"tenant": {}})
	if err != nil {
		t.Fatal(err)
	}
	physical, _ := stub.ComputeConstraints(context.Background(), nil, nil)
	constraints := []*allocation.ResourceConstraints{physical, qc}

	var logged []string
	ctx := ctrl.LoggerInto(context.Background(), funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}))
	opt := allocation.NewGreedyByScoreOptimizer()
	at := func(target int) []domain.VariantDecision {
		if target < 0 {
			return nil
		}
		return []domain.VariantDecision{{Namespace: "tenant", VariantName: "v", TargetReplicas: target}}
	}
	cycle := func(target int) { e.reportQuotaAloneAllocation(ctx, opt, nil, constraints, at(target)) }

	cycle(2) // cycle 0: runs, "tenant/v" differs
	if len(logged) != 1 {
		t.Fatalf("cycle 0 logged %d lines, want 1", len(logged))
	}
	for i := 1; i < 10; i++ {
		cycle(-1) // would report "no longer changes" if the pass ran
	}
	if len(logged) != 1 {
		t.Fatalf("cycles 1-9 logged %d lines in all, want 1: the pass runs once per 10 cycles", len(logged))
	}
	cycle(-1) // cycle 10: runs, nothing differs any more
	if len(logged) != 2 {
		t.Fatalf("cycle 10 logged %d lines in all, want 2", len(logged))
	}
	for i := 11; i < 20; i++ {
		cycle(-1)
	}
	cycle(3) // cycle 20: runs, "tenant/v" differs again
	for i := 21; i < 30; i++ {
		cycle(-1)
	}
	cycle(7) // cycle 30: runs, same set at a different target
	if len(logged) != 3 {
		t.Fatalf("logged %d lines in all, want 3: a target that moved within the same set is not a change", len(logged))
	}
}
