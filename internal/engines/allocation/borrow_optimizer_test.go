package allocation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// h100Node is an 8-GPU H100 node with used GPUs taken.
func h100Node(used int, labels map[string]string, taints ...corev1.Taint) decision.NodeGPU {
	return decision.NodeGPU{Accelerator: "H100", Capacity: 8, Used: used, Labels: labels, Taints: taints}
}

var _ = Describe("Greedy optimizer spending cohort borrowing", func() {
	ctx := context.Background()
	optimize := func(nodes map[string]decision.NodeGPU, requests []ModelScalingRequest, constraints ...*ResourceConstraints) (map[string]int, *GreedyByScoreOptimizer) {
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(nodes)
		return addedGPUs(o.Optimize(ctx, requests, constraints)), o
	}

	It("spends a shared pool once across two borrowers: 8 ordered, not 16", func() {
		got, o := optimize(nil, []ModelScalingRequest{
			borrowModel("a", "ma", 8, 0, 2, nil),
			borrowModel("b", "mb", 8, 0, 2, nil),
		}, physicalH100(64), borrowQuota(8,
			nsBorrow{ns: "a", cap: config.QuotaUnlimited, wn: 0},
			nsBorrow{ns: "b", cap: config.QuotaUnlimited, wn: 0}))
		Expect(got["ma"] + got["mb"]).To(Equal(8))
		Expect(o.BorrowDraws()).To(HaveLen(1))
		Expect(o.BorrowDraws()[0].GPUs).To(Equal(8))
	})

	It("lets a Never queue grow into lent-out nominal only as far as the pool has room: 8, not 16", func() {
		// n: reclaimWithinCohort Never, N=8, U=0, G=0 -> guaranteed 0, its
		// nominal reachable only through the pool (cap 8); b borrows freely.
		got, _ := optimize(nil, []ModelScalingRequest{
			borrowModel("n", "mn", 8, 0, 1, nil),
			borrowModel("b", "mb", 8, 0, 1, nil),
		}, physicalH100(64), borrowQuota(8,
			nsBorrow{ns: "n", cap: 8, wn: 8},
			nsBorrow{ns: "b", cap: config.QuotaUnlimited}))
		Expect(got["mn"] + got["mb"]).To(Equal(8))
	})

	It("grows a reclaiming queue whose nominal is all lent out to nominal, and debits the pool so no one borrows it again", func() {
		// r: N=8 all lent out (a peer borrowed it), so the pool reads 8 free of
		// what r lent... that r takes back; b would borrow the same 8.
		got, _ := optimize(nil, []ModelScalingRequest{
			borrowModel("r", "mr", 8, 0, 4, nil),
			borrowModel("b", "mb", 8, 0, 1, nil),
		}, physicalH100(64), borrowQuota(8,
			nsBorrow{ns: "r", guaranteed: 8, cap: 0, wn: 8, lentOut: 8},
			nsBorrow{ns: "b", cap: config.QuotaUnlimited}))
		Expect(got["mr"]).To(Equal(8), "nominal is guaranteed to a queue that can reclaim")
		Expect(got["mb"]).To(Equal(0), "growth into lent-out nominal took the pool")

		got, _ = optimize(nil, []ModelScalingRequest{borrowModel("r", "mr", 8, 0, 4, nil)},
			physicalH100(64), borrowQuota(0, nsBorrow{ns: "r", guaranteed: 8, cap: 0, wn: 8, lentOut: 8}))
		Expect(got["mr"]).To(Equal(8), "an empty pool does not gate it")
	})

	Describe("node fit", func() {
		quota := func(free int, pools ...SharedPoolChunk) *ResourceConstraints {
			c := borrowQuota(free, nsBorrow{ns: "a", cap: config.QuotaUnlimited})
			if len(pools) > 0 {
				c.SharedPools = pools
			}
			return c
		}

		It("orders nothing when 5 + 5 free GPUs on two nodes face one 8-GPU replica", func() {
			got, _ := optimize(map[string]decision.NodeGPU{
				"n1": h100Node(3, nil), "n2": h100Node(3, nil),
			}, []ModelScalingRequest{borrowModel("a", "ma", 8, 0, 1, nil)}, physicalH100(16), quota(10))
			Expect(got["ma"]).To(Equal(0))
		})

		It("control: orders the replica when one node has 8 free", func() {
			got, _ := optimize(map[string]decision.NodeGPU{
				"n1": h100Node(0, nil), "n2": h100Node(3, nil),
			}, []ModelScalingRequest{borrowModel("a", "ma", 8, 0, 1, nil)}, physicalH100(16), quota(10))
			Expect(got["ma"]).To(Equal(8))
		})

		It("needs two nodes of the flavor for a 2x8 group", func() {
			flavor := SharedPoolChunk{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Free: 16,
				FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: borrowH100, NodeLabels: map[string]string{"pool": "a"}}}
			lws := []ModelScalingRequest{borrowModel("a", "ma", 16, 0, 1, []int{8, 8})}
			got, _ := optimize(map[string]decision.NodeGPU{
				"n1": h100Node(0, map[string]string{"pool": "a"}), "n2": h100Node(0, map[string]string{"pool": "b"}),
			}, lws, physicalH100(32), quota(16, flavor))
			Expect(got["ma"]).To(Equal(0), "one node of the flavor holds one pod of the group")

			got, _ = optimize(map[string]decision.NodeGPU{
				"n1": h100Node(0, map[string]string{"pool": "a"}), "n2": h100Node(0, map[string]string{"pool": "a"}),
			}, lws, physicalH100(32), quota(16, flavor))
			Expect(got["ma"]).To(Equal(16))
		})

		It("respects the flavor's node labels and tolerations", func() {
			taint := corev1.Taint{Key: "dedicated", Value: "train", Effect: corev1.TaintEffectNoSchedule}
			gpuTaint := corev1.Taint{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}
			chunk := func(tols ...corev1.Toleration) SharedPoolChunk {
				return SharedPoolChunk{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Free: 8,
					FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: borrowH100,
						NodeLabels: map[string]string{"pool": "a"}, Tolerations: tols}}
			}
			model := []ModelScalingRequest{borrowModel("a", "ma", 8, 0, 1, nil)}

			got, _ := optimize(map[string]decision.NodeGPU{"n1": h100Node(0, map[string]string{"pool": "b"})},
				model, physicalH100(16), quota(8, chunk()))
			Expect(got["ma"]).To(Equal(0), "the only free node is not the flavor's")

			tainted := map[string]decision.NodeGPU{"n1": h100Node(0, map[string]string{"pool": "a"}, taint, gpuTaint)}
			got, _ = optimize(tainted, model, physicalH100(16), quota(8, chunk()))
			Expect(got["ma"]).To(Equal(0), "the flavor does not tolerate the node's taint")

			got, _ = optimize(tainted, model, physicalH100(16), quota(8, chunk(corev1.Toleration{
				Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "train", Effect: corev1.TaintEffectNoSchedule})))
			Expect(got["ma"]).To(Equal(8), "tolerated; the GPU resource taint is tolerated by every GPU pod")
		})

		It("debits guaranteed and borrowed replicas alike from the node view", func() {
			// g grows within its guaranteed 8 and takes a node; b may borrow 16
			// but only one node is left for it.
			nodes := map[string]decision.NodeGPU{"n1": h100Node(0, nil), "n2": h100Node(0, nil)}
			got, o := optimize(nodes, []ModelScalingRequest{
				borrowModel("g", "mg", 8, 0, 4, nil),
				borrowModel("b", "mb", 8, 0, 2, nil),
			}, physicalH100(64), borrowQuota(16,
				nsBorrow{ns: "g", guaranteed: 8, cap: 0, wn: 8},
				nsBorrow{ns: "b", cap: config.QuotaUnlimited}))
			Expect(got["mg"]).To(Equal(8))
			Expect(got["mb"]).To(Equal(8), "the guaranteed replica's node is not free to borrow onto")
			for name, n := range o.Nodes.Snapshot() {
				Expect(n.Free()).To(Equal(0), name)
			}
		})

		It("still orders a guaranteed replica that fits no node, and debits nothing for it", func() {
			nodes := map[string]decision.NodeGPU{"n1": h100Node(4, nil)}
			got, o := optimize(nodes, []ModelScalingRequest{borrowModel("g", "mg", 8, 0, 1, nil)},
				physicalH100(64), borrowQuota(0, nsBorrow{ns: "g", guaranteed: 8, cap: 0, wn: 8}))
			Expect(got["mg"]).To(Equal(8))
			Expect(o.Nodes.Snapshot()["n1"].Free()).To(Equal(4))
		})

		It("places a P/D pair as one unit: both roles or neither", func() {
			pd := withSatEntry(&satEntryFixture{
				RequiredCapacity: 20000,
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {Role: "decode", RequiredCapacity: 10000, TotalDemand: 10000},
				},
				VariantCapacities: []vcFixture{
					{VariantName: "p", AcceleratorName: "H100", Cost: 5, Role: "prefill", ReplicaCount: 0, PerReplicaCapacity: 10000},
					{VariantName: "d", AcceleratorName: "H100", Cost: 5, Role: "decode", ReplicaCount: 0, PerReplicaCapacity: 10000},
				},
			}, ModelScalingRequest{ModelID: "pd", Namespace: "a", Disaggregated: true, Priority: 1,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "p", GPUsPerReplica: 8, Role: "prefill"},
					{VariantName: "d", GPUsPerReplica: 8, Role: "decode"},
				}})
			// The pool holds 16, but the nodes only one 8-GPU replica.
			got, _ := optimize(map[string]decision.NodeGPU{"n1": h100Node(0, nil), "n2": h100Node(4, nil)},
				[]ModelScalingRequest{pd}, physicalH100(64), quota(16))
			Expect(got["pd"]).To(Equal(0), "one side alone serves nothing")
		})
	})
})

var _ = Describe("Borrowing views", func() {
	It("merges the guaranteed part: quota 12 with 4 borrowable beside physical 10 is 8 within nominal for scale-from-zero", func() {
		constraints := []*ResourceConstraints{
			physicalH100(10),
			borrowQuota(4, nsBorrow{ns: "x", guaranteed: 8, cap: 4, wn: 8}),
		}
		within := WithinNominal(constraints)
		budgets, nsScoped := GPUBudgets(within, "x")
		Expect(nsScoped).To(BeTrue())
		Expect(budgets["H100"]).To(Equal(8))
		Expect(FitsGPUBudget(within, "x", map[string]int{"H100": 8})).To(BeTrue())
		Expect(FitsGPUBudget(within, "x", map[string]int{"H100": 9})).To(BeFalse(), "never a borrowed GPU")

		// The greedy optimizer reaches 10: 8 guaranteed and 2 borrowed, bounded by
		// the physical limiter.
		o := NewGreedyByScoreOptimizer()
		got := addedGPUs(o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("x", "m", 1, 0, 12, nil)}, constraints))
		Expect(got["m"]).To(Equal(10))
	})

	It("wakes an idle Never namespace within nominal although its guaranteed figure is 0", func() {
		constraints := []*ResourceConstraints{borrowQuota(8, nsBorrow{ns: "x", guaranteed: 0, cap: 8, wn: 8})}
		Expect(FitsGPUBudget(WithinNominal(constraints), "x", map[string]int{"H100": 8})).To(BeTrue())
		Expect(FitsGPUBudget(GuaranteedOnly(constraints), "x", map[string]int{"H100": 8})).To(BeFalse())
	})

	It("is the identity without borrowing", func() {
		plain := []*ResourceConstraints{physicalH100(8), {ProviderName: "q", NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: 4}}}}}
		Expect(WithinNominal(plain)[1]).To(BeIdenticalTo(plain[1]))
		Expect(GuaranteedOnly(plain)[1]).To(BeIdenticalTo(plain[1]))
	})

	It("budgets the share at guaranteed plus the borrowed GPUs a group holds, with no debt and no new borrowing", func() {
		// x is guaranteed 8 and holds 12: 4 borrowed. The pool still has 8 free.
		req := shareReq("m", "x", 8000, 12)
		req.CompositeSignal.Live = true
		c := borrowQuota(8, nsBorrow{ns: "x", guaranteed: 8, used: 12, cap: 8, wn: 8})
		c.NamespacePools["x"] = map[string]ResourcePool{"A100": {Limit: 8, Used: 12}}
		c.NamespaceBorrow["x"] = map[string]NamespaceBorrow{"A100": {Cap: 8, WithinNominal: 8}}
		c.Pools = reachAggregate(reachPools(c.NamespacePools, c.NamespaceBorrow))
		groups := BuildShareGroups([]ModelScalingRequest{req}, GuaranteedOnly([]*ResourceConstraints{c}), ShareGroupOptions{})
		Expect(groups).To(HaveLen(1))
		g := groups[0]
		Expect(g.Budget).To(Equal(12), "guaranteed 8 plus the 4 borrowed GPUs it holds; never the pool's 8")
		Expect(ShareDebt(g.Committed, g.Budget)).To(Equal(0), "held borrowed GPUs are not debt")
	})
})

var _ = Describe("Warm-pool headroom across namespace providers", func() {
	It("denies a type only some providers holding the namespace list", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "q1", NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: 4}}}},
			{ProviderName: "q2", NamespacePools: map[string]map[string]ResourcePool{"x": {"A100": {Limit: 4}}}},
		}
		Expect(namespaceHeadroom(constraints)).To(Equal(map[string]map[string]int{"x": {"H100": 0, "A100": 0}}),
			"the optimizer allows neither (mergeNamespaceConstraints); the pool must not grow on either")
	})

	It("keeps the tightest figure where every provider lists the type", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "q1", NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: 4, Used: 1}}}},
			{ProviderName: "q2", NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: -1}}}},
		}
		Expect(namespaceHeadroom(constraints)).To(Equal(map[string]map[string]int{"x": {"H100": 3}}))
	})
})

var _ = Describe("Borrowed growth Kueue has not reserved yet", func() {
	draw := BorrowDraw{Namespace: "a", Variant: "v", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", GPUs: 16, Replicas: 2, Target: 3}

	It("stays deducted from its chunk until the pods exist, shrinking as they appear", func() {
		f := NewBorrowInFlight()
		f.Record([]BorrowDraw{draw})
		c := []*ResourceConstraints{borrowQuota(24, nsBorrow{ns: "a", cap: config.QuotaUnlimited})}

		f.Observe(map[string]VariantObservation{"a/v": {Current: 1, Desired: 3}})
		Expect(DeductUnreserved(c, f.Entries(), nil)[0].SharedPools[0].Free).To(Equal(8))

		f.Observe(map[string]VariantObservation{"a/v": {Current: 2, Desired: 3}})
		Expect(DeductUnreserved(c, f.Entries(), nil)[0].SharedPools[0].Free).To(Equal(16), "one replica's pods exist")

		f.Observe(map[string]VariantObservation{"a/v": {Current: 3, Desired: 3}})
		Expect(f.Entries()).To(BeEmpty())
		Expect(c[0].SharedPools[0].Free).To(Equal(24), "the input is never modified")
	})

	It("is dropped when the desired count goes back down", func() {
		f := NewBorrowInFlight()
		f.Record([]BorrowDraw{draw})
		f.Observe(map[string]VariantObservation{"a/v": {Current: 1, Desired: 2}})
		Expect(f.Entries()).To(BeEmpty())
	})

	It("takes gated pods' GPUs out of the namespace's cohort pools, clamped at 0", func() {
		c := []*ResourceConstraints{borrowQuota(8, nsBorrow{ns: "a", cap: config.QuotaUnlimited})}
		got := DeductUnreserved(c, nil, map[string]map[string]int{"a": {borrowH100: 6}, "elsewhere": {"": 8}})
		Expect(got[0].SharedPools[0].Free).To(Equal(2))
		got = DeductUnreserved(c, nil, map[string]map[string]int{"a": {"": 20}})
		Expect(got[0].SharedPools[0].Free).To(Equal(0))
	})
})
