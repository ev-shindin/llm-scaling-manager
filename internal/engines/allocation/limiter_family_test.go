package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
)

// GPU names form a family/product tree: a family key bounds the sum of its
// products, and a namespace-scope quota bounds only the namespaces it holds.
// Every provider below is built the way production builds it.

const (
	famHBM3 = "NVIDIA-H100-80GB-HBM3"
	famSXM5 = "NVIDIA-H100-SXM5-80GB"
	famPCIe = "NVIDIA-H100-PCIE-80GB"
)

func famPhysical(t *testing.T, installed map[string]int) *ResourceConstraints {
	t.Helper()
	nodes := map[string]map[string]gpunodes.AcceleratorModelInfo{}
	for product, count := range installed {
		nodes["node-"+product] = map[string]gpunodes.AcceleratorModelInfo{product: {Count: count}}
	}
	rc, err := NewDefaultLimiter("gpu-limiter", NewTypeInventory("gpu-limiter", &mockDiscovery{inventory: nodes})).
		ComputeConstraints(context.Background(), map[string]int{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func famClusterQuota(t *testing.T, quotas map[string]int) *ResourceConstraints {
	t.Helper()
	rc, err := NewDefaultLimiter("cluster-quota", NewQuotaInventory(config.QuotaLimiterConfig{
		Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster, ClusterQuotas: quotas,
	})).ComputeConstraints(context.Background(), map[string]int{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func famNamespaceQuota(t *testing.T, quotas map[string]map[string]int) *ResourceConstraints {
	t.Helper()
	active := map[string]map[string]int{}
	for ns := range quotas {
		active[ns] = map[string]int{}
	}
	rc, err := NewDefaultLimiter("ns-quota", NewQuotaInventory(config.QuotaLimiterConfig{
		Name: "ns-quota", Type: "quota", Scope: config.QuotaScopeNamespace, NamespaceQuotas: quotas,
	})).ComputeConstraints(context.Background(), map[string]int{}, active)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

// famWanting is one model of one variant on acc, holding no replica, that
// wants `replicas` replicas of gpus GPUs each.
func famWanting(namespace, model, acc string, replicas, gpus int) ModelScalingRequest {
	return withSatEntry(&satEntryFixture{
		ModelID: model, Namespace: namespace, AnalyzedAt: time.Now(),
		RequiredCapacity: float64(replicas) * 10000,
		VariantCapacities: []vcFixture{
			{VariantName: model + "-v", AcceleratorName: acc, Cost: 10, ReplicaCount: 0, PerReplicaCapacity: 10000},
		},
	}, ModelScalingRequest{
		ModelID: model, Namespace: namespace,
		VariantStates: []domain.VariantReplicaState{{VariantName: model + "-v", CurrentReplicas: 0, GPUsPerReplica: gpus}},
	})
}

func famTargets(constraints []*ResourceConstraints, reqs ...ModelScalingRequest) map[string]int {
	out := map[string]int{}
	for _, d := range NewGreedyByScoreOptimizer().Optimize(context.Background(), reqs, constraints) {
		out[d.VariantName] = d.TargetReplicas
	}
	return out
}

// Regression from the review: two namespaces of a namespace-scope quota that
// spell one GPU differently, on a physical pool of 16. The quota's aggregate
// {H100: 8, ...HBM3: 4} used to enter the cluster merge as two keys of one
// GPU, and whichever namespace went first took GPUs from the other: 4/4 or
// 8/0, where main gave 8/4.
func TestNamespaceSpellingsOfOneGPUEachGetTheirCap(t *testing.T) {
	build := func() []*ResourceConstraints {
		return []*ResourceConstraints{
			famPhysical(t, map[string]int{famHBM3: 16}),
			famNamespaceQuota(t, map[string]map[string]int{"x": {"H100": 8}, "y": {famHBM3: 4}}),
		}
	}
	for _, order := range []struct {
		name   string
		xWants int
		yWants int
	}{{"x asks more", 40, 20}, {"y asks more", 20, 40}} {
		for _, swap := range []bool{false, true} {
			reqs := []ModelScalingRequest{
				famWanting("x", "mx", "H100", order.xWants, 1),
				famWanting("y", "my", famHBM3, order.yWants, 1),
			}
			if swap {
				reqs[0], reqs[1] = reqs[1], reqs[0]
			}
			got := famTargets(build(), reqs...)
			if got["mx-v"] != 8 || got["my-v"] != 4 {
				t.Errorf("%s, swapped=%v: x=%d y=%d, want x=8 y=4 (each its own cap, 12 of 16 physical)",
					order.name, swap, got["mx-v"], got["my-v"])
			}
		}
	}
}

// A cluster quota granting two products of one family on 5 physical H100s:
// the products are bounded by their family, together.
func TestTwoProductsOfOneFamilyShareThePhysicalPool(t *testing.T) {
	c := []*ResourceConstraints{
		famPhysical(t, map[string]int{famHBM3: 5}),
		famClusterQuota(t, map[string]int{famHBM3: 4, famSXM5: 4}),
	}
	merged := mergeConstraints(c)
	if merged["H100"] != 5 || merged[famHBM3] != 4 || merged[famSXM5] != 4 {
		t.Errorf("merged = %v, want H100: 5 bounding HBM3: 4 and SXM5: 4", merged)
	}
	if got := budgetTotal(merged); got != 5 {
		t.Errorf("budgetTotal = %d, want 5: the family counts once", got)
	}
	got := famTargets(c, famWanting("a", "ma", famHBM3, 40, 1), famWanting("b", "mb", famSXM5, 40, 1))
	if total := got["ma-v"] + got["mb-v"]; total != 5 {
		t.Errorf("ordered %d GPUs (HBM3 %d + SXM5 %d) against 5 physical H100s, want all 5", total, got["ma-v"], got["mb-v"])
	}
	if got["ma-v"] > 4 || got["mb-v"] > 4 {
		t.Errorf("a product went past its quota of 4: %v", got)
	}
	if FitsGPUBudget(c, "a", map[string]int{famPCIe: 1}) {
		t.Error("a product the quota does not grant must be denied, though its family key is in the budget")
	}
	if got := famTargets(c, famWanting("a", "mp", famPCIe, 40, 1)); got["mp-v"] != 0 {
		t.Errorf("PCIe grew to %d; the quota grants it nothing", got["mp-v"])
	}
}

// A spend under a product also draws down the namespace's family key. Quota
// only: x may take 4 H100s in all, 3 of them HBM3; y's cap keeps the quota's
// aggregate from bounding x.
func TestNamespaceFamilyKeyIsDebitedByAProductSpend(t *testing.T) {
	c := []*ResourceConstraints{
		famNamespaceQuota(t, map[string]map[string]int{"x": {"H100": 4, famHBM3: 3}, "y": {"H100": 8}}),
	}
	got := famTargets(c, famWanting("x", "ml", famHBM3, 40, 1), famWanting("x", "ms", "H100", 40, 1))
	if got["ml-v"] > 3 {
		t.Errorf("HBM3 ordered %d, its cap is 3", got["ml-v"])
	}
	if total := got["ml-v"] + got["ms-v"]; total != 4 {
		t.Errorf("x ordered %d H100s (HBM3 %d + H100 %d), its allowance is 4", total, got["ml-v"], got["ms-v"])
	}
}

// A workload that names no accelerator is charged to no per-type pool. In a
// namespace with a closed allowlist it fits only when the allowlist lists
// "unknown", or lists a single type it is then judged against.
func TestUnnamedAcceleratorInAClosedNamespace(t *testing.T) {
	unknown := constants.DefaultAcceleratorName
	for _, c := range []struct {
		name  string
		quota map[string]int
		need  int
		want  bool
	}{
		{"several types: not chargeable", map[string]int{"H100": 4, "A100": 4}, 1, false},
		{"a single type: judged against it", map[string]int{"H100": 4}, 4, true},
		{"a single type: over it", map[string]int{"H100": 4}, 5, false},
		{"unknown listed", map[string]int{unknown: 2, "H100": 4}, 2, true},
		{"unknown listed: over it", map[string]int{unknown: 2, "H100": 4}, 3, false},
	} {
		cons := []*ResourceConstraints{famNamespaceQuota(t, map[string]map[string]int{"x": c.quota, "y": {"H100": 8}})}
		if got := FitsGPUBudget(cons, "x", map[string]int{unknown: c.need}); got != c.want {
			t.Errorf("%s: FitsGPUBudget(unknown: %d) = %v, want %v", c.name, c.need, got, c.want)
		}
	}
}
