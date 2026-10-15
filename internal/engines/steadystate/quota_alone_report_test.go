package steadystate

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

func TestQuotaAloneDiff(t *testing.T) {
	composed := []domain.VariantDecision{
		{Namespace: "ns", VariantName: "a", TargetReplicas: 2},
		{Namespace: "ns", VariantName: "b", TargetReplicas: 3},
		{Namespace: "ns", VariantName: "c", TargetReplicas: 1},
	}
	alone := []domain.VariantDecision{
		{Namespace: "ns", VariantName: "a", TargetReplicas: 4},
		{Namespace: "ns", VariantName: "b", TargetReplicas: 3},
		{Namespace: "ns", VariantName: "d", TargetReplicas: 2},
	}
	got := quotaAloneDiff(composed, alone)
	want := "ns/a, ns/c, ns/d"
	if got != want {
		t.Fatalf("quotaAloneDiff = %q, want %q", got, want)
	}
	if quotaAloneDiff(composed, composed) != "" {
		t.Fatal("identical passes must report no difference")
	}
}

// The upgrade log runs only for a policy that declares both kinds, and says
// what the quota alone would have allowed beside what the composed set allows.
func TestReportQuotaAloneAllocation(t *testing.T) {
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"default": {Limiters: []config.QuotaLimiterConfig{
			{Type: "gpu-inventory", Name: "gpu"},
			{Type: "quota", Name: "test-quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"tenant": {"A100": 8}}},
		}},
	})
	quota := quotaLimiterFor("tenant", "A100", 8)
	e := &Engine{Config: cfg, GPULimiter: allocation.NewCompositeLimiter("gpu-and-quota-limiter", []allocation.Limiter{
		physicalStub{pools: map[string]allocation.ResourcePool{"A100": {Limit: 2}}},
		quota,
	})}
	qc, err := quota.(allocation.ConstraintProvider).ComputeConstraints(context.Background(), map[string]int{}, map[string]map[string]int{"tenant": {}})
	if err != nil {
		t.Fatal(err)
	}
	physical, _ := physicalStub{pools: map[string]allocation.ResourcePool{"A100": {Limit: 2}}}.
		ComputeConstraints(context.Background(), nil, nil)

	var logged []string
	ctx := ctrl.LoggerInto(context.Background(), funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}))

	composed := []domain.VariantDecision{{Namespace: "tenant", VariantName: "v", TargetReplicas: 2}}
	opt := allocation.NewGreedyByScoreOptimizer()

	// No requests: the shadow decides nothing, so "v" differs only by absence.
	e.reportQuotaAloneAllocation(ctx, opt, nil, []*allocation.ResourceConstraints{physical, qc}, composed)
	if len(logged) != 1 || !strings.Contains(logged[0], "Upgrade note") || !strings.Contains(logged[0], "tenant/v") {
		t.Fatalf("logged %q, want one upgrade note naming the difference", logged)
	}
	// Unchanged difference: not logged again.
	e.reportQuotaAloneAllocation(ctx, opt, nil, []*allocation.ResourceConstraints{physical, qc}, composed)
	if len(logged) != 1 {
		t.Fatalf("logged %d lines; an unchanged difference must not be logged every cycle", len(logged))
	}

	// A quota-only policy: nothing to compare.
	quotaOnly := config.NewTestConfig()
	quotaOnly.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"default": {Limiters: []config.QuotaLimiterConfig{{Type: "quota", Name: "test-quota",
			Scope: config.QuotaScopeNamespace, NamespaceQuotas: map[string]map[string]int{"tenant": {"A100": 8}}}}},
	})
	e2 := &Engine{Config: quotaOnly, GPULimiter: quota}
	logged = nil
	e2.reportQuotaAloneAllocation(ctx, opt, nil, []*allocation.ResourceConstraints{qc}, composed)
	if len(logged) != 0 {
		t.Fatalf("logged %q for a quota-only policy", logged)
	}
}
