package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
)

// A physical pool of {H100: 16} beside a namespace-scope aggregate of
// {NVIDIA-H100-80GB-HBM3: 4} merges to two keys for one GPU. A namespace the
// quota caps spends on the long key, an excluded one on "H100"; both draw on
// the same 16 GPUs, so together they may order at most 16.
func TestOpenAggregateSpellingDoesNotDoubleSpend(t *testing.T) {
	ctx := context.Background()
	const longH = "NVIDIA-H100-80GB-HBM3"

	nodes := map[string]map[string]gpunodes.AcceleratorModelInfo{
		"node-a": {longH: {Count: 16}},
	}
	phys, err := NewDefaultLimiter("gpu-limiter", NewTypeInventory("gpu-limiter", &mockDiscovery{inventory: nodes})).
		ComputeConstraints(ctx, map[string]int{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	quota, err := NewDefaultLimiter("ns-quota", NewQuotaInventory(config.QuotaLimiterConfig{
		Name: "ns-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"x": {longH: 4}},
		Exclude:         []string{"y"},
	})).ComputeConstraints(ctx, map[string]int{}, map[string]map[string]int{"x": {}, "y": {}})
	if err != nil {
		t.Fatal(err)
	}
	constraints := []*ResourceConstraints{phys, quota}

	merged := mergeConstraints(constraints)
	if merged["H100"] != 16 || merged[longH] != 4 {
		t.Fatalf("fixture premise: merged = %v, want H100: 16 and %s: 4 as two keys", merged, longH)
	}

	wanting := func(namespace, model, variant, acc string, replicas int) ModelScalingRequest {
		r := &satEntryFixture{
			ModelID: model, Namespace: namespace, AnalyzedAt: time.Now(),
			RequiredCapacity: float64(replicas) * 10000,
			VariantCapacities: []vcFixture{
				{VariantName: variant, AcceleratorName: acc, Cost: 10, ReplicaCount: 0, PerReplicaCapacity: 10000},
			},
		}
		return withSatEntry(r, ModelScalingRequest{
			ModelID: model, Namespace: namespace,
			VariantStates: []domain.VariantReplicaState{{VariantName: variant, CurrentReplicas: 0, GPUsPerReplica: 1}},
		})
	}

	got := map[string]int{}
	for _, d := range NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{
		wanting("x", "m-long", "v-long", longH, 40),
		wanting("y", "m-short", "v-short", "H100", 40),
	}, constraints) {
		got[d.VariantName] = d.TargetReplicas
	}
	if got["v-long"] > 4 {
		t.Errorf("v-long ordered %d, its namespace cap is 4", got["v-long"])
	}
	if total := got["v-long"] + got["v-short"]; total > 16 {
		t.Errorf("ordered %d GPUs (v-long %d + v-short %d) against 16 physical H100s", total, got["v-long"], got["v-short"])
	}
	if got["v-long"] == 0 || got["v-short"] == 0 {
		t.Errorf("both models should grow: %v", got)
	}
}
