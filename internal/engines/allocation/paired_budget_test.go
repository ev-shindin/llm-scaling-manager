package allocation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// pdOnAccelerators builds a two-role model whose prefill and decode replicas
// each take 8 GPUs of the named accelerators, both at one replica.
// Decode wants three more replicas; prefill wants prefillRC/100 replicas in all.
func pdOnAccelerators(prefillAcc, decodeAcc string, prefillRC float64,
	decodeTarget, decodeMax int) (NamedAnalyzerResult, []variantRecord, map[string]domain.VariantReplicaState, map[string]int) {
	e, variants, stateMap, targets := pairedFleet(decodeTarget, decodeMax, 1, 10, prefillRC)
	for i := range variants {
		if variants[i].Role == "prefill" {
			variants[i].AcceleratorName = prefillAcc
		} else {
			variants[i].AcceleratorName = decodeAcc
		}
	}
	for name, st := range stateMap {
		st.GPUsPerReplica = 8
		stateMap[name] = st
	}
	return e, variants, stateMap, targets
}

// orderPair runs one paired allocation through the greedy pick against budget,
// with a fair-share target large enough that only the budget binds.
func orderPair(e NamedAnalyzerResult, variants []variantRecord, stateMap map[string]domain.VariantReplicaState,
	targets map[string]int, budget map[string]int) {
	roles, ps := initRoleState(&e)
	allocateForModelPaired(context.Background(), &e, variants, stateMap, budget, targets,
		fairShareRolePick(1e6, roles, gpuLimitTracker{}), ps, roles)
}

// A P/D pair's roles are each sized against the whole budget, and the budget is
// debited only after both are committed. With one accelerator under both roles,
// each saw all of it (docs/proposals/kueue-borrowing-and-limiter-composition.md,
// A2).
var _ = Describe("a P/D pair commits only what fits together", func() {
	It("orders nothing when one replica of each does not fit (budget 8, 8+8 GPUs)", func() {
		e, variants, stateMap, targets := pdOnAccelerators("H100", "H100", 400, 1, 10)
		budget := map[string]int{"H100": 8}
		orderPair(e, variants, stateMap, targets, budget)
		Expect(targets).To(Equal(map[string]int{"prefill": 1, "decode": 1}),
			"8 GPUs cannot hold a prefill and a decode replica of 8 each; one side alone serves nothing")
		Expect(budget["H100"]).To(Equal(8))
	})

	It("orders one of each when exactly that fits (budget 16)", func() {
		e, variants, stateMap, targets := pdOnAccelerators("H100", "H100", 400, 1, 10)
		budget := map[string]int{"H100": 16}
		orderPair(e, variants, stateMap, targets, budget)
		Expect(targets).To(Equal(map[string]int{"prefill": 2, "decode": 2}))
		Expect(budget["H100"]).To(Equal(0))
	})

	It("scales an uneven pair down jointly and keeps its ratio (budget 24, decode wants 3, prefill 1)", func() {
		e, variants, stateMap, targets := pdOnAccelerators("H100", "H100", 200, 1, 10)
		budget := map[string]int{"H100": 24}
		orderPair(e, variants, stateMap, targets, budget)
		// The k math asks 1 prefill + 3 decode = 32 GPUs; scaled to 24 it is 1 + 2.
		Expect(targets).To(Equal(map[string]int{"prefill": 2, "decode": 3}))
		Expect(budget["H100"]).To(BeNumerically(">=", 0))
	})

	It("treats two spellings of one GPU as one budget", func() {
		e, variants, stateMap, targets := pdOnAccelerators("NVIDIA-H100-80GB-HBM3", "H100", 400, 1, 10)
		budget := map[string]int{"H100": 8}
		orderPair(e, variants, stateMap, targets, budget)
		Expect(targets).To(Equal(map[string]int{"prefill": 1, "decode": 1}))
	})

	// Controls: what must not change.
	It("grows both roles when each fits its own accelerator's pool", func() {
		e, variants, stateMap, targets := pdOnAccelerators("H100", "A100", 400, 1, 10)
		budget := map[string]int{"A100": 8, "H100": 8}
		orderPair(e, variants, stateMap, targets, budget)
		Expect(targets).To(Equal(map[string]int{"prefill": 2, "decode": 2}))
	})

	It("still grows prefill when decode is at its ceiling", func() {
		e, variants, stateMap, targets := pdOnAccelerators("H100", "H100", 400, 9, 9)
		budget := map[string]int{"H100": 16}
		orderPair(e, variants, stateMap, targets, budget)
		Expect(targets["decode"]).To(Equal(9))
		Expect(targets["prefill"]).To(Equal(3), "decode is finished, so prefill alone takes the 16 GPUs")
	})

	It("holds the namespace budget through the optimizer", func() {
		r := &satEntryFixture{
			RequiredCapacity: 80000,
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {Role: "prefill", RequiredCapacity: 40000, TotalDemand: 40000},
				"decode":  {Role: "decode", RequiredCapacity: 40000, TotalDemand: 40000},
			},
			VariantCapacities: []vcFixture{
				{VariantName: "prefill-v", AcceleratorName: "H100", Cost: 5, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
				{VariantName: "decode-v", AcceleratorName: "H100", Cost: 5, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
			},
		}
		requests := []ModelScalingRequest{withSatEntry(r, ModelScalingRequest{
			ModelID: "pd", Namespace: "x", Disaggregated: true, Priority: 1,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 8, Role: "prefill"},
				{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 8, Role: "decode"},
			},
		})}
		constraints := []*ResourceConstraints{
			{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"H100": {Limit: 64}}},
			{ProviderName: "namespace-quota", Pools: map[string]ResourcePool{"H100": {Limit: 16}},
				NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: 16}}}},
		}
		dm := decisionMap(NewGreedyByScoreOptimizer().Optimize(context.Background(), requests, constraints))
		added := (dm["prefill-v"].TargetReplicas - 1 + dm["decode-v"].TargetReplicas - 1) * 8
		Expect(added).To(BeNumerically("<=", 16), "the namespace may take 16 GPUs")
		Expect(dm["prefill-v"].TargetReplicas).To(Equal(2))
		Expect(dm["decode-v"].TargetReplicas).To(Equal(2))
	})
})
