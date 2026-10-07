package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// shareReq builds a live single-variant model on A100 (1 GPU per replica, a
// per-replica capacity of 1000) with a scale-up threshold of 0.8, so a demand
// of D gives a need of D / 800 GPUs.
func shareReq(id, ns string, demand float64, current int) ModelScalingRequest {
	v := id + "-v"
	req := withSatEntry(&satEntryFixture{
		ModelID:     id,
		Namespace:   ns,
		TotalDemand: demand,
		VariantCapacities: []vcFixture{
			{VariantName: v, AcceleratorName: "A100", Cost: 5, ReplicaCount: current, PerReplicaCapacity: 1000},
		},
	}, ModelScalingRequest{
		ModelID:   id,
		Namespace: ns,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: v, CurrentReplicas: current, GPUsPerReplica: 1, MinReplicas: ptrInt(1)},
		},
	})
	req.CompositeSignal.ScaleUpThreshold = 0.8
	return req
}

func ptrInt(v int) *int { return &v }

func weightsByModel(w map[string]float64) func(ModelScalingRequest) float64 {
	return func(req ModelScalingRequest) float64 {
		if v, ok := w[req.ModelID]; ok {
			return v
		}
		return 1
	}
}

var _ = Describe("BuildShareGroups", func() {
	full16 := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 16}}}}

	It("builds §5.1's fleet as one cluster group whose need is demand / threshold in GPUs", func() {
		reqs := []ModelScalingRequest{
			shareReq("A", "ns", 4000, 9),
			shareReq("B", "ns", 3000, 5),
			shareReq("C", "ns", 1000, 2),
		}
		groups := BuildShareGroups(reqs, full16, ShareGroupOptions{
			ClusterIsQuota: true,
			Weight:         weightsByModel(map[string]float64{"A": 2}),
		})
		Expect(groups).To(HaveLen(1))
		g := groups[0]
		Expect(g.AcceleratorType).To(Equal("A100"))
		Expect(g.Scope).To(BeEmpty())
		Expect(g.Budget).To(Equal(16))
		needs := map[string]float64{}
		for _, r := range g.Roles {
			needs[r.Key] = r.Need
			Expect(r.Floor).To(Equal(1))
		}
		Expect(needs["ns/A/both"]).To(BeNumerically("~", 5, 1e-9))
		Expect(needs["ns/B/both"]).To(BeNumerically("~", 3.75, 1e-9))
		Expect(needs["ns/C/both"]).To(BeNumerically("~", 1.25, 1e-9))

		// Held exactly at §5.7's integer target: nothing is actionable.
		ev := EvaluateShare(g.Roles, g.Committed, g.Thresholds, g.Budget, 0.15)
		Expect(ev.ReplicasToMove).To(BeZero())
		for _, v := range ev.Roles {
			Expect(v.Actionable).To(BeFalse(), v.Key)
		}
	})

	It("sees B's doubled demand as three replicas to move (§5.7)", func() {
		reqs := []ModelScalingRequest{
			shareReq("A", "ns", 4000, 9),
			shareReq("B", "ns", 6000, 5),
			shareReq("C", "ns", 1000, 2),
		}
		g := BuildShareGroups(reqs, full16, ShareGroupOptions{
			ClusterIsQuota: true,
			Weight:         weightsByModel(map[string]float64{"A": 2}),
		})[0]
		ev := EvaluateShare(g.Roles, g.Committed, g.Thresholds, g.Budget, 0.15)
		Expect(ev.ReplicasToMove).To(Equal(3))
	})

	It("builds no group on the physical inventory unless asked to (§7.3)", func() {
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9)}
		Expect(BuildShareGroups(reqs, full16, ShareGroupOptions{ClusterIsQuota: false})).To(BeEmpty())
		Expect(BuildShareGroups(reqs, full16, ShareGroupOptions{ClusterIsQuota: false, PhysicalGroups: true})).To(HaveLen(1))
	})

	It("freezes a model whose signal is not live and keeps its GPUs out of the budget (§6.1)", func() {
		dark := shareReq("D", "ns", 2000, 3)
		dark.CompositeSignal.Live = false
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9), dark}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 12}}}}
		g := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		Expect(g.Roles).To(HaveLen(1))
		Expect(g.Frozen).To(ConsistOf(ContainSubstring("ns/D")))
		// 4 free + A's 9: D's 3 are neither free nor planned.
		Expect(g.Budget).To(Equal(13))
	})

	It("skips a disabled namespace-quota group and keeps the others", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4), shareReq("B", "team-b", 3000, 4)}
		cons := []*ResourceConstraints{{
			NamespacePools: map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 8, Used: 4}},
				"team-b": {"A100": {Limit: 8, Used: 4}},
			},
		}}
		groups := BuildShareGroups(reqs, cons, ShareGroupOptions{
			NamespaceEnabled: func(ns string) bool { return ns != "team-b" },
		})
		Expect(groups).To(HaveLen(1))
		Expect(groups[0].Scope).To(Equal("team-a"))
		Expect(groups[0].Budget).To(Equal(8))
	})

	It("builds both roles of a P/D model from their role demands", func() {
		req := withSatEntry(&satEntryFixture{
			ModelID:     "pd",
			Namespace:   "ns",
			TotalDemand: 30000,
			VariantCapacities: []vcFixture{
				{VariantName: "pd-p", AcceleratorName: "H200", Role: domain.RolePrefill, Cost: 5, ReplicaCount: 2, PerReplicaCapacity: 8000},
				{VariantName: "pd-d", AcceleratorName: "H200", Role: domain.RoleDecode, Cost: 5, ReplicaCount: 3, PerReplicaCapacity: 8000},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				domain.RolePrefill: {Role: domain.RolePrefill, TotalDemand: 6400},
				domain.RoleDecode:  {Role: domain.RoleDecode, TotalDemand: 12800},
			},
		}, ModelScalingRequest{
			ModelID:       "pd",
			Namespace:     "ns",
			Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "pd-p", CurrentReplicas: 2, GPUsPerReplica: 8, Role: domain.RolePrefill, MinReplicas: ptrInt(1)},
				{VariantName: "pd-d", CurrentReplicas: 3, GPUsPerReplica: 8, Role: domain.RoleDecode, MinReplicas: ptrInt(1)},
			},
		})
		req.CompositeSignal.ScaleUpThreshold = 0.8
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"H200": {Limit: 64, Used: 40}}}}
		g := BuildShareGroups([]ModelScalingRequest{req}, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		byRole := map[string]ShareRole{}
		for _, r := range g.Roles {
			byRole[g.Origins[r.Key].Role] = r
		}
		// prefill: 6400 / 0.8 / (8000 / 8 GPUs) = 8 GPUs; decode: 16 GPUs.
		Expect(byRole[domain.RolePrefill].Need).To(BeNumerically("~", 8, 1e-9))
		Expect(byRole[domain.RoleDecode].Need).To(BeNumerically("~", 16, 1e-9))
		Expect(byRole[domain.RolePrefill].ReplicaGPUs).To(Equal(8))
		Expect(byRole[domain.RolePrefill].Floor).To(Equal(8))
		Expect(g.Committed[byRole[domain.RoleDecode].Key]).To(Equal(24))
		Expect(g.Budget).To(Equal(24 + 40))
	})

	It("freezes a P/D model whole when one role has no demand reading", func() {
		req := withSatEntry(&satEntryFixture{
			ModelID:     "pd",
			Namespace:   "ns",
			TotalDemand: 30000,
			VariantCapacities: []vcFixture{
				{VariantName: "pd-p", AcceleratorName: "H200", Role: domain.RolePrefill, Cost: 5, ReplicaCount: 2, PerReplicaCapacity: 8000},
				{VariantName: "pd-d", AcceleratorName: "H200", Role: domain.RoleDecode, Cost: 5, ReplicaCount: 3, PerReplicaCapacity: 8000},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				domain.RoleDecode: {Role: domain.RoleDecode, TotalDemand: 12800},
			},
		}, ModelScalingRequest{
			ModelID:   "pd",
			Namespace: "ns",
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "pd-p", CurrentReplicas: 2, GPUsPerReplica: 8, Role: domain.RolePrefill},
				{VariantName: "pd-d", CurrentReplicas: 3, GPUsPerReplica: 8, Role: domain.RoleDecode},
			},
		})
		req.CompositeSignal.ScaleUpThreshold = 0.8
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"H200": {Limit: 64, Used: 40}}}}
		g := BuildShareGroups([]ModelScalingRequest{req}, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		Expect(g.Roles).To(BeEmpty())
		Expect(g.Frozen).To(ConsistOf(ContainSubstring("prefill")))
	})

	It("returns nothing without constraints, and skips an unlimited budget", func() {
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9)}
		Expect(BuildShareGroups(reqs, nil, ShareGroupOptions{ClusterIsQuota: true})).To(BeEmpty())
		unlimited := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: -1}}}}
		Expect(BuildShareGroups(reqs, unlimited, ShareGroupOptions{ClusterIsQuota: true})).To(BeEmpty())
	})
})
