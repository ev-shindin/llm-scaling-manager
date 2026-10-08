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

func weightsByModel(w map[string]float64) func(ModelScalingRequest, bool) float64 {
	return func(req ModelScalingRequest, _ bool) float64 {
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

	It("holds the reserve out of the budget (§7.2)", func() {
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9)}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 12}}}}
		without := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		with := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true, ReserveGPUs: 2})[0]
		Expect(without.Budget - with.Budget).To(Equal(2))
	})

	It("carves the warm pools' unheld target out of the budget of the group they plan in (§7.2)", func() {
		reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 9)}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 12}}}}
		without := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})[0]
		with := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true,
			PoolUnheld: map[string]map[string]int{"pools": {"A100": 3, "H100": 5}}})[0]
		Expect(without.Budget-with.Budget).To(Equal(3), "only the group's accelerator is carved")
		Expect(with.PoolCarve).To(Equal(3))
	})

	It("charges a namespace with its own quota group its pools' carve-out, not the cluster group", func() {
		Expect(poolCarve(map[string]map[string]int{"own": {"A100": 2}, "other": {"A100": 3}}, "A100", "own",
			func(string) bool { return true })).To(Equal(2))
		Expect(poolCarve(map[string]map[string]int{"own": {"A100": 2}, "other": {"A100": 3}}, "A100", "",
			func(ns string) bool { return ns == "own" })).To(Equal(3), "the cluster group carves only namespaces without a group")
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

// shareVariant is one variant of a multi-variant fixture.
type shareVariant struct {
	name          string
	gpus, current int
	cost, prc     float64
	min, max      *int
}

// shareReqVariants builds model "m" in namespace "ns" from several variants.
func shareReqVariants(demand float64, vs ...shareVariant) ModelScalingRequest {
	fx := &satEntryFixture{ModelID: "m", Namespace: "ns", TotalDemand: demand}
	req := ModelScalingRequest{ModelID: "m", Namespace: "ns"}
	for _, v := range vs {
		fx.VariantCapacities = append(fx.VariantCapacities, vcFixture{
			VariantName: v.name, AcceleratorName: "A100", Cost: v.cost, ReplicaCount: v.current, PerReplicaCapacity: v.prc,
		})
		req.VariantStates = append(req.VariantStates, domain.VariantReplicaState{
			VariantName: v.name, CurrentReplicas: v.current, GPUsPerReplica: v.gpus, MinReplicas: v.min, MaxReplicas: v.max,
		})
	}
	req = withSatEntry(fx, req)
	req.CompositeSignal.ScaleUpThreshold = 0.8
	return req
}

var _ = Describe("BuildShareGroups: floors, ceilings and replica sizes", func() {
	roomy := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 64, Used: 0}}}}

	only := func(req ModelScalingRequest) ShareRole {
		g := BuildShareGroups([]ModelScalingRequest{req}, roomy, ShareGroupOptions{ClusterIsQuota: true})
		ExpectWithOffset(1, g).To(HaveLen(1))
		ExpectWithOffset(1, g[0].Roles).To(HaveLen(1))
		return g[0].Roles[0]
	}

	It("sums floors and ceilings over a role's variants, each at its own replica size", func() {
		r := only(shareReqVariants(4000,
			shareVariant{name: "big", gpus: 4, cost: 5, prc: 4000, min: ptrInt(1), max: ptrInt(2)},
			shareVariant{name: "small", gpus: 1, cost: 2, prc: 1000, min: ptrInt(2), max: ptrInt(3)},
		))
		Expect(r.Floor).To(Equal(1*4 + 2*1))
		Expect(r.Ceiling).To(Equal(2*4 + 3*1))
		// The most cost-efficient variant (5/4000 < 2/1000) sizes the role:
		// one replica is 4 GPUs, and a GPU of it serves 1000.
		Expect(r.ReplicaGPUs).To(Equal(4))
		Expect(r.Need).To(BeNumerically("~", 4000/0.8/1000, 1e-9))
	})

	It("leaves a role unbounded when any of its variants has no ceiling", func() {
		r := only(shareReqVariants(4000,
			shareVariant{name: "big", gpus: 4, cost: 5, prc: 4000, max: ptrInt(2)},
			shareVariant{name: "small", gpus: 1, cost: 2, prc: 1000},
		))
		Expect(r.Ceiling).To(BeZero())
		Expect(r.Floor).To(BeZero())
	})

	It("freezes an aggregated model with demand but no measured capacity", func() {
		req := shareReqVariants(4000, shareVariant{name: "v", gpus: 1, cost: 5, prc: 0, current: 2})
		g := BuildShareGroups([]ModelScalingRequest{req}, roomy, ShareGroupOptions{ClusterIsQuota: true})
		Expect(g).To(HaveLen(1))
		Expect(g[0].Roles).To(BeEmpty())
		Expect(g[0].Frozen).To(ConsistOf(ContainSubstring("no measured capacity")))
	})

	It("plans a model with no demand and no capacity reading at its floor", func() {
		r := only(shareReqVariants(0, shareVariant{name: "v", gpus: 2, cost: 5, prc: 0, min: ptrInt(1)}))
		Expect(r.Need).To(BeZero())
		Expect(r.Floor).To(Equal(2))
		Expect(r.ReplicaGPUs).To(Equal(2))
	})
})

var _ = Describe("BuildShareGroups: scopes and bounds", func() {
	It("builds a namespace-quota group beside the cluster group, each with its own budget", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4), shareReq("B", "other", 3000, 8)}
		cons := []*ResourceConstraints{{
			Pools:          map[string]ResourcePool{"A100": {Limit: 16, Used: 12}},
			NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 8, Used: 4}}},
		}}
		groups := BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})
		Expect(groups).To(HaveLen(2))
		Expect(groups[0].Scope).To(BeEmpty())
		Expect(groups[0].Budget).To(Equal(4 + 8)) // the cluster's free 4 + B's 8
		Expect(groups[1].Scope).To(Equal("team-a"))
		Expect(groups[1].Budget).To(Equal(4 + 4)) // team-a's free 4 + A's 4
	})

	It("builds a namespace-quota group even when the cluster budget is the physical inventory", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4)}
		cons := []*ResourceConstraints{{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 8, Used: 4}}}}}
		Expect(BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: false})).To(HaveLen(1))
	})

	It("skips a namespace whose quota does not list the model's accelerator, rather than borrowing the cluster pool", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4)}
		cons := []*ResourceConstraints{{
			Pools:          map[string]ResourcePool{"A100": {Limit: 16, Used: 4}},
			NamespacePools: map[string]map[string]ResourcePool{"team-a": {"H100": {Limit: 8, Used: 0}}},
		}}
		Expect(BuildShareGroups(reqs, cons, ShareGroupOptions{ClusterIsQuota: true})).To(BeEmpty())
	})

	It("skips an unlimited namespace pool and a budget no cluster could hold", func() {
		reqs := []ModelScalingRequest{shareReq("A", "team-a", 4000, 4)}
		unlimited := []*ResourceConstraints{{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: -1}}}}}
		Expect(BuildShareGroups(reqs, unlimited, ShareGroupOptions{})).To(BeEmpty())

		huge := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: MaxShareBudgetGPUs + 1, Used: 0}}}}
		Expect(BuildShareGroups([]ModelScalingRequest{shareReq("A", "ns", 4000, 4)}, huge, ShareGroupOptions{ClusterIsQuota: true})).To(BeEmpty())
	})
})
