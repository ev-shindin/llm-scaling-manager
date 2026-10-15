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

// One model whose roles name one GPU under two spellings: prefill on "H100",
// decode on "NVIDIA-H100-80GB-HBM3". A merged budget can hold both keys (a
// physical pool keyed by the family beside a cluster quota granting the
// product), and every GPU spent under the long key is one of the short key's
// GPUs too.
var _ = Describe("one model's roles on two spellings of one GPU", func() {
	const longH = "NVIDIA-H100-80GB-HBM3"

	It("fits the pair against the broader key: 8+8 GPUs do not fit 8 H100s", func() {
		e, variants, stateMap, targets := pdOnAccelerators("H100", longH, 400, 1, 10)
		budget := map[string]int{"H100": 8, longH: 12}
		orderPair(e, variants, stateMap, targets, budget)
		Expect(targets).To(Equal(map[string]int{"prefill": 1, "decode": 1}),
			"the long key's GPUs are H100s: one replica of each needs 16 of the 8")
	})

	It("debits the broader key for a spend under the long one", func() {
		e, variants, stateMap, targets := pdOnAccelerators("H100", longH, 400, 1, 10)
		budget := map[string]int{"H100": 16, longH: 12}
		orderPair(e, variants, stateMap, targets, budget)
		Expect(targets).To(Equal(map[string]int{"prefill": 2, "decode": 2}))
		Expect(budget).To(Equal(map[string]int{"H100": 0, longH: 0}),
			"16 H100s spent, 8 of them under the long key")
	})

	// Through the optimizer, against the merged cluster budget, which keeps
	// both keys.
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
			ModelID: "pd", Namespace: "y", AnalyzedAt: time.Now(),
			RequiredCapacity: 80000,
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {Role: "prefill", RequiredCapacity: 40000, TotalDemand: 40000},
				"decode":  {Role: "decode", RequiredCapacity: 40000, TotalDemand: 40000},
			},
			VariantCapacities: []vcFixture{
				{VariantName: "prefill-v", AcceleratorName: "H100", Cost: 5, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
				{VariantName: "decode-v", AcceleratorName: longH, Cost: 5, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
			},
		}
		requests := []ModelScalingRequest{withSatEntry(r, ModelScalingRequest{
			ModelID: "pd", Namespace: "y", Disaggregated: true, Priority: 1,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 8, Role: "prefill"},
				{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 8, Role: "decode"},
			},
		})}
		dm := decisionMap(NewGreedyByScoreOptimizer().Optimize(ctx, requests, constraints))
		return map[string]int{"prefill": dm["prefill-v"].TargetReplicas - 1, "decode": dm["decode-v"].TargetReplicas - 1}
	}

	It("orders no pair through the optimizer when only 8 H100s exist", func() {
		added := optimizeExcluded(8)
		Expect((added["prefill"]+added["decode"])*8).To(BeNumerically("<=", 8),
			"ordered %v replicas of 8 GPUs against 8 physical H100s", added)
	})

	It("control: orders one pair when 16 H100s exist", func() {
		Expect(optimizeExcluded(16)).To(Equal(map[string]int{"prefill": 1, "decode": 1}))
	})
})
