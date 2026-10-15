package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
)

// One model whose variants name one GPU under two spellings: v-long on
// "NVIDIA-H100-80GB-HBM3", v-short on "H100". A merged budget can hold both
// keys (a physical pool keyed by the family beside a cluster quota granting the
// product), and every GPU spent under the long key is one of the short key's
// GPUs too, so a spend on one variant must shrink what the other reads within
// the same pass.
var _ = Describe("one model's variants on two spellings of one GPU", func() {
	const longH = "NVIDIA-H100-80GB-HBM3"

	optimizeExcluded := func(physical int) map[string]int {
		ctx := context.Background()
		nodes := map[string]map[string]gpunodes.AcceleratorModelInfo{"node-a": {longH: {Count: physical}}}
		phys, err := NewDefaultLimiter("gpu-limiter", NewTypeInventory("gpu-limiter", &mockDiscovery{inventory: nodes})).
			ComputeConstraints(ctx, map[string]int{}, nil)
		Expect(err).NotTo(HaveOccurred())
		// A cluster quota granting the product, beside the physical pool keyed by
		// its family: the merge keeps both keys, the family bounding the sum.
		quota, err := NewDefaultLimiter("cluster-quota", NewQuotaInventory(config.QuotaLimiterConfig{
			Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{longH: 16},
		})).ComputeConstraints(ctx, map[string]int{}, nil)
		Expect(err).NotTo(HaveOccurred())
		constraints := []*ResourceConstraints{phys, quota}
		merged := mergeConstraints(constraints)
		Expect(merged).To(HaveKeyWithValue("H100", physical), "fixture premise: two keys")
		Expect(merged).To(HaveKeyWithValue(longH, physical), "fixture premise: two keys")

		r := &satEntryFixture{
			ModelID: "m", Namespace: "y", AnalyzedAt: time.Now(),
			RequiredCapacity: 400000,
			VariantCapacities: []vcFixture{
				{VariantName: "v-long", AcceleratorName: longH, Cost: 5, ReplicaCount: 0, PerReplicaCapacity: 10000},
				{VariantName: "v-short", AcceleratorName: "H100", Cost: 10, ReplicaCount: 0, PerReplicaCapacity: 10000},
			},
		}
		requests := []ModelScalingRequest{withSatEntry(r, ModelScalingRequest{
			ModelID: "m", Namespace: "y",
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v-long", CurrentReplicas: 0, GPUsPerReplica: 8},
				{VariantName: "v-short", CurrentReplicas: 0, GPUsPerReplica: 8},
			},
		})}
		dm := decisionMap(NewGreedyByScoreOptimizer().Optimize(ctx, requests, constraints))
		return map[string]int{"v-long": dm["v-long"].TargetReplicas, "v-short": dm["v-short"].TargetReplicas}
	}

	It("orders no more GPUs than exist: 8 H100s hold one 8-GPU replica", func() {
		got := optimizeExcluded(8)
		Expect((got["v-long"]+got["v-short"])*8).To(Equal(8),
			"ordered %v replicas of 8 GPUs against 8 physical H100s", got)
	})

	It("control: 16 H100s hold two", func() {
		got := optimizeExcluded(16)
		Expect((got["v-long"] + got["v-short"]) * 8).To(Equal(16))
	})
})

var _ = Describe("spellings that differ only in case", func() {
	It("are counted once by budgetTotal", func() {
		Expect(budgetTotal(map[string]int{"H100": 16, "h100": 4})).To(Equal(16),
			"one accelerator under two spellings holds 16 GPUs, not 20")
	})

	It("are debited together", func() {
		b := map[string]int{"H100": 16, "h100": 4}
		debitBudget(b, "h100", 4)
		Expect(b).To(Equal(map[string]int{"H100": 12, "h100": 0}),
			"4 GPUs spent under h100 are 4 of the H100s")
		b = map[string]int{"H100": 16, "h100": 4}
		debitBudget(b, "H100", 14)
		Expect(b).To(Equal(map[string]int{"H100": 2, "h100": 0}))
	})
})
