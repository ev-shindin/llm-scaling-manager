package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
)

// Limiters are intersected: a decision must fit every declared limiter, and a
// type a closed limiter does not list is denied however generously another
// lists it.
//
// Every provider here is built the way production builds it -- an inventory
// behind a DefaultLimiter -- so what is under test is the whole seam from
// inventory to verdict, not a hand-written ResourceConstraints that might not
// match what a real limiter emits.
var _ = Describe("limiters merge by intersection", func() {
	const (
		ns      = "x"
		longH   = "NVIDIA-H100-80GB-HBM3"
		cluster = "cluster-quota"
	)
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	// physical builds the gpu-inventory limiter's constraints: one node per
	// product label, which TypeInventory normalizes to short names.
	physical := func(installed, used map[string]int) *ResourceConstraints {
		nodes := map[string]map[string]gpunodes.AcceleratorModelInfo{}
		for product, count := range installed {
			nodes["node-"+product] = map[string]gpunodes.AcceleratorModelInfo{product: {Count: count}}
		}
		inv := NewTypeInventory("gpu-limiter", &mockDiscovery{inventory: nodes})
		rc, err := NewDefaultLimiter("gpu-limiter", inv).ComputeConstraints(ctx, used, nil)
		Expect(err).NotTo(HaveOccurred())
		return rc
	}

	clusterQuota := func(name string, quotas map[string]int) *ResourceConstraints {
		inv := NewQuotaInventory(config.QuotaLimiterConfig{
			Name: name, Type: "quota", Scope: config.QuotaScopeCluster, ClusterQuotas: quotas,
		})
		rc, err := NewDefaultLimiter(name, inv).ComputeConstraints(ctx, map[string]int{}, nil)
		Expect(err).NotTo(HaveOccurred())
		return rc
	}

	// namespaceQuota builds a namespace-scope entry; the namespaces of usedByNS
	// are the active ones, as the engine's usage view makes them.
	namespaceQuota := func(name string, quotas map[string]map[string]int, exclude []string,
		usedByNS map[string]map[string]int) *ResourceConstraints {
		inv := NewQuotaInventory(config.QuotaLimiterConfig{
			Name: name, Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: quotas, Exclude: exclude,
		})
		rc, err := NewDefaultLimiter(name, inv).ComputeConstraints(ctx, map[string]int{}, usedByNS)
		Expect(err).NotTo(HaveOccurred())
		return rc
	}

	active := map[string]map[string]int{ns: {}}

	// wanting is one model of one single-GPU variant that holds one replica and
	// wants the given number of replicas' worth of capacity.
	wanting := func(namespace, model, variant, acc string, replicas int) ModelScalingRequest {
		r := &satEntryFixture{
			ModelID:          model,
			Namespace:        namespace,
			AnalyzedAt:       time.Now(),
			RequiredCapacity: float64(replicas) * 10000,
			VariantCapacities: []vcFixture{
				{VariantName: variant, AcceleratorName: acc, Cost: 10, ReplicaCount: 1, PerReplicaCapacity: 10000},
			},
		}
		return withSatEntry(r, ModelScalingRequest{
			ModelID:   model,
			Namespace: namespace,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: variant, CurrentReplicas: 1, GPUsPerReplica: 1},
			},
		})
	}

	// growing wants far more than any budget here.
	growing := func(namespace, model, variant, acc string) ModelScalingRequest {
		return wanting(namespace, model, variant, acc, 40)
	}

	targets := func(constraints []*ResourceConstraints, reqs ...ModelScalingRequest) map[string]int {
		out := map[string]int{}
		for _, d := range NewGreedyByScoreOptimizer().Optimize(ctx, reqs, constraints) {
			out[d.VariantName] = d.TargetReplicas
		}
		return out
	}

	// The five rows of the proposal's union-leak table. Rows 1 and 3 were
	// already right; they stay as the controls the leaking rows are read
	// against.
	Describe("the union leak", func() {
		row2 := func() []*ResourceConstraints {
			return []*ResourceConstraints{
				clusterQuota(cluster, map[string]int{"H100": 8}),
				physical(map[string]int{longH: 16, "NVIDIA-A100-SXM4-80GB": 4}, map[string]int{}),
			}
		}
		row4 := func() []*ResourceConstraints {
			return []*ResourceConstraints{
				namespaceQuota("quota-h100", map[string]map[string]int{ns: {"H100": 4}}, nil, active),
				namespaceQuota("quota-a100", map[string]map[string]int{ns: {"A100": 4}}, nil, active),
			}
		}
		row5 := func() []*ResourceConstraints {
			return []*ResourceConstraints{
				clusterQuota(cluster, map[string]int{"H100": 8}),
				namespaceQuota("quota-a100", map[string]map[string]int{ns: {"A100": 4}}, nil, active),
			}
		}

		DescribeTable("A100 = 2 in x does not fit",
			func(build func() []*ResourceConstraints) {
				Expect(FitsGPUBudget(build(), ns, map[string]int{"A100": 2})).To(BeFalse())
			},
			Entry("row 1: cluster quota {H100: 8} alone", func() []*ResourceConstraints {
				return []*ResourceConstraints{clusterQuota(cluster, map[string]int{"H100": 8})}
			}),
			Entry("row 2: that quota + physical {H100: 16, A100: 4}", row2),
			Entry("row 3: namespace quota x: {H100: 4} alone", func() []*ResourceConstraints {
				return []*ResourceConstraints{
					namespaceQuota("quota-h100", map[string]map[string]int{ns: {"H100": 4}}, nil, active),
				}
			}),
			Entry("row 4: that + a second entry x: {A100: 4}", row4),
			Entry("row 5: cluster quota {H100: 8} + namespace entry x: {A100: 4}", row5),
		)

		It("row 2: merges the type the quota omits as an explicit 0", func() {
			Expect(mergeConstraints(row2())).To(HaveKeyWithValue("A100", 0),
				"the quota is closed: an A100 it never granted is 0, not physical's 4")
		})

		It("row 4: denies in x a type one entry holding x does not list", func() {
			Expect(mergeNamespaceConstraints(row4())[ns]).To(HaveKeyWithValue("A100", 0))
			Expect(mergeNamespaceConstraints(row4())[ns]).To(HaveKeyWithValue("H100", 0))
		})

		It("row 5: a type the cluster bounds do not list is 0 for the namespace", func() {
			Expect(mergeConstraints(row5())).To(Equal(map[string]int{"H100": 8}),
				"the cluster budget holds the cluster bounds' keys only")
			budgets, nsScoped := GPUBudgets(row5(), ns)
			Expect(nsScoped).To(BeTrue())
			Expect(budgets).To(HaveKeyWithValue("A100", 0),
				"every cluster bound is closed: an A100 the quota never granted is 0")
		})

		It("row 2: the optimizer does not grow an A100 model", func() {
			got := targets(row2(), growing(ns, "m", "a100", "A100"))
			Expect(got).To(HaveKeyWithValue("a100", 1))
		})

		It("row 4: the optimizer does not grow an A100 model in x", func() {
			got := targets(row4(), growing(ns, "m", "a100", "A100"))
			Expect(got).To(HaveKeyWithValue("a100", 1))
		})

		It("row 5 (namespace re-entry): the optimizer does not grow an A100 model in x", func() {
			got := targets(row5(), growing(ns, "m", "a100", "A100"))
			Expect(got).To(HaveKeyWithValue("a100", 1))
		})

		It("row 2: credits the limit to the quota, not to the provider that allowed it", func() {
			Expect(bindingProvider(row2(), ns, "A100")).To(Equal(cluster))
		})
	})

	Describe("one GPU, two spellings", func() {
		// A provider keyed by the long product name and one keyed by the short
		// name, both cluster-wide and closed.
		spelled := func() []*ResourceConstraints {
			return []*ResourceConstraints{
				clusterQuota("long-spelling", map[string]int{longH: 8}),
				clusterQuota("short-spelling", map[string]int{"H100": 16}),
			}
		}

		It("merges to the family and the product, both at the smaller budget", func() {
			merged := mergeConstraints(spelled())
			Expect(merged).To(Equal(map[string]int{"H100": 8, longH: 8}),
				"the long-spelling quota bounds the family by the product it grants")
		})

		It("admits no other product of the family", func() {
			Expect(FitsGPUBudget(spelled(), ns, map[string]int{"NVIDIA-H100-SXM5-80GB": 1})).To(BeFalse(),
				"the long-spelling quota grants the HBM3 product only")
		})

		It("judges a wake by that one budget, whichever spelling the workload uses", func() {
			c := spelled()
			Expect(FitsGPUBudget(c, ns, map[string]int{"H100": 8})).To(BeTrue())
			Expect(FitsGPUBudget(c, ns, map[string]int{"H100": 9})).To(BeFalse())
			Expect(FitsGPUBudget(c, ns, map[string]int{longH: 9})).To(BeFalse())
		})

		It("grows both variants against one budget", func() {
			got := targets(spelled(),
				wanting("a", "m-long", "v-long", longH, 7),
				wanting("b", "m-short", "v-short", "H100", 7))
			Expect(got["v-long"]).To(BeNumerically(">", 1))
			Expect(got["v-short"]).To(BeNumerically(">", 1))
			Expect(got["v-long"]+got["v-short"]-2).To(BeNumerically("<=", 8),
				"8 free GPUs fund at most 8 new replicas across both spellings")
		})

		It("grows a variant declaring the long name against a physical pool keyed by the short one", func() {
			c := []*ResourceConstraints{physical(map[string]int{longH: 16}, map[string]int{"H100": 2})}
			got := targets(c,
				wanting("a", "m-long", "v-long", longH, 7),
				wanting("b", "m-short", "v-short", "H100", 7))
			Expect(got["v-long"]).To(BeNumerically(">", 1), "measured before: held at 1")
			Expect(got["v-short"]).To(BeNumerically(">", 1))
			Expect(got["v-long"] + got["v-short"] - 2).To(BeNumerically("<=", 14))
		})

		It("intersects two namespace entries that spell one GPU differently", func() {
			c := []*ResourceConstraints{
				namespaceQuota("short", map[string]map[string]int{ns: {"H100": 4}}, nil, active),
				namespaceQuota("long", map[string]map[string]int{ns: {longH: 2}}, nil, active),
			}
			Expect(mergeNamespaceConstraints(c)[ns]).To(Equal(map[string]int{"H100": 2, longH: 2}),
				"the long entry allows 2 H100s in all, the short one 4")
		})
	})

	It("checks a workload with no named accelerator against namespace caps bounded by the cluster", func() {
		c := []*ResourceConstraints{
			physical(map[string]int{longH: 16}, map[string]int{"H100": 16}),
			namespaceQuota("quota-h100", map[string]map[string]int{ns: {"H100": 4}}, nil, active),
		}
		Expect(FitsGPUBudget(c, ns, map[string]int{constants.DefaultAcceleratorName: 2})).To(BeFalse(),
			"the namespace may use 4 H100s, but the cluster has none free")
	})

	// A reviewer measured an earlier draft stopping this: a namespace a
	// namespace-scope quota excludes must keep scaling. It bypasses the quota,
	// so with no cluster bound in force it is unbounded -- it is no longer
	// bounded by the sum of the other tenants' caps, which were never its own.
	Describe("a namespace excluded from a namespace-scope-only quota", func() {
		excluded := func() []*ResourceConstraints {
			return []*ResourceConstraints{
				namespaceQuota("quota-h100", map[string]map[string]int{ns: {"H100": 4}}, []string{"y"},
					map[string]map[string]int{ns: {}, "y": {}}),
			}
		}

		It("still fits", func() {
			Expect(FitsGPUBudget(excluded(), "y", map[string]int{"H100": 2})).To(BeTrue())
			budgets, nsScoped := GPUBudgets(excluded(), "y")
			Expect(nsScoped).To(BeFalse())
			Expect(budgets).To(BeNil(), "no cluster bound: unbounded")
			Expect(FitsGPUBudget(excluded(), "y", map[string]int{"A100": 64})).To(BeTrue(),
				"the excluded namespace bypasses the quota for any type")
		})

		It("still scales, unbounded", func() {
			got := targets(excluded(), growing("y", "m", "v", "H100"))
			Expect(got["v"]).To(Equal(41), "grows to its demand: nothing bounds it")
		})

		It("is bounded by a physical pool when there is one", func() {
			c := append(excluded(), physical(map[string]int{longH: 6}, map[string]int{"H100": 1}))
			got := targets(c, growing("y", "m", "v", "H100"))
			Expect(got["v"]).To(Equal(6), "5 H100s free")
		})
	})

	It("does not zero an unknown pool a quota declares because physical inventory omits it", func() {
		c := []*ResourceConstraints{
			physical(map[string]int{longH: 16}, map[string]int{}),
			clusterQuota(cluster, map[string]int{constants.DefaultAcceleratorName: 4, "H100": 8}),
		}
		Expect(mergeConstraints(c)).To(HaveKeyWithValue(constants.DefaultAcceleratorName, 4))
	})
})
