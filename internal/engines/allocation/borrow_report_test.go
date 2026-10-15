package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

var _ = Describe("what on-call sees of borrowing", func() {
	t0 := time.Unix(20000, 0)

	Describe("LimiterBudgets", func() {
		byNS := func(bs []LimiterBudget) map[string]LimiterBudget {
			out := map[string]LimiterBudget{}
			for _, b := range bs {
				out[b.Namespace] = b
			}
			return out
		}

		It("reports guaranteed, used, and borrowable as min(cap, the free pool)", func() {
			c := borrowQuota(6,
				nsBorrow{ns: "a", guaranteed: 8, used: 10, cap: 4},
				nsBorrow{ns: "u", guaranteed: 2, cap: config.QuotaUnlimited},
				nsBorrow{ns: "z", guaranteed: 2, cap: 0})
			got := byNS(LimiterBudgets([]*ResourceConstraints{c}, nil))
			Expect(got["a"]).To(Equal(LimiterBudget{Limiter: "namespace-quota", Namespace: "a", Accelerator: "H100",
				Guaranteed: 8, Borrowable: 4, Used: 10}))
			Expect(got["u"].Borrowable).To(Equal(6), "an unlimited cap is bounded by the pool")
			Expect(got["z"].Borrowable).To(Equal(0))
			Expect(got["z"].Reason).To(Equal(BorrowReasonNoBorrowCap))
		})

		DescribeTable("names why borrowable is 0",
			func(mutate func(*ResourceConstraints), blocked map[string]bool, want string) {
				c := borrowQuota(0, nsBorrow{ns: "a", guaranteed: 8, cap: 4})
				mutate(c)
				b := LimiterBudgets([]*ResourceConstraints{c}, blocked)[0]
				Expect(b.Borrowable).To(Equal(0))
				Expect(b.Reason).To(Equal(want))
			},
			Entry("an empty pool", func(*ResourceConstraints) {}, nil, BorrowReasonPoolEmpty),
			Entry("a cohort closed for a waiting lender", func(*ResourceConstraints) {}, map[string]bool{"c": true}, BorrowReasonLenderWaiting),
			Entry("a stale read", func(c *ResourceConstraints) { c.SharedPools[0].Stale = true }, nil, string(kueue.ReasonStale)),
			Entry("a pool of another accelerator", func(c *ResourceConstraints) {
				c.SharedPools[0].AcceleratorType = "NVIDIA-A100-SXM4-80GB"
				c.SharedPools[0].Free = 16
			}, nil, BorrowReasonPoolEmpty),
		)

		It("reports a withheld namespace's reason and a non-Kueue quota with none", func() {
			c := borrowQuota(16, nsBorrow{ns: "a", guaranteed: 8, cap: 4})
			c.NamespacePools["w"] = map[string]ResourcePool{"H100": {Limit: 8, Used: 8}}
			c.BorrowWithheld = map[string]string{"w": string(kueue.ReasonNotKueueManaged)}
			plain := &ResourceConstraints{ProviderName: "static", NamespacePools: map[string]map[string]ResourcePool{
				"p": {"H100": {Limit: 4, Used: 1}}}}
			got := LimiterBudgets([]*ResourceConstraints{plain, c, physicalH100(64)}, nil)
			Expect(got).To(HaveLen(3), "a physical limiter has no namespace budget")
			Expect(got[0].Limiter).To(Equal("namespace-quota"), "sorted by limiter, then namespace")
			Expect(byNS(got)["w"]).To(Equal(LimiterBudget{Limiter: "namespace-quota", Namespace: "w", Accelerator: "H100",
				Guaranteed: 8, Used: 8, Reason: string(kueue.ReasonNotKueueManaged)}))
			Expect(byNS(got)["p"]).To(Equal(LimiterBudget{Limiter: "static", Namespace: "p", Accelerator: "H100",
				Guaranteed: 4, Used: 1}))
		})
	})

	Describe("the governor's status of a variant", func() {
		lender := kueue.LenderState{Queue: "lender", Waiting: true, LentOut: 8}
		cycle := func(g *BorrowGovernor, now time.Time, reqs []ModelScalingRequest, used int) []BorrowRelease {
			c := BorrowCycle{Now: now, Constraints: govWorld{readAt: now, lenders: []kueue.LenderState{lender}, used: used}.constraints(),
				Requests: reqs}
			g.Observe(c)
			return g.Releases(c, decisionsOf(reqs))
		}

		It("names a lender release, and the lender waiting since its first read", func() {
			g := NewBorrowGovernor(DefaultBorrowSettings())
			reqs := []ModelScalingRequest{govModel("m", 4, 8)}
			var rel []BorrowRelease
			for _, d := range []time.Duration{0, 30 * time.Second, 61 * time.Second} {
				rel = cycle(g, t0.Add(d), reqs, 32)
			}
			Expect(rel).To(HaveLen(1))
			Expect(g.VariantStatus("b", "m-v")).To(Equal(VariantBorrowStatus{LenderRelease: true}))
			Expect(g.VariantStatus("b", "other")).To(Equal(VariantBorrowStatus{}))
			Expect(g.WaitingLenders(t0.Add(61 * time.Second))).To(Equal([]LenderWait{{
				LenderKey: LenderKey{Cohort: "c", Lender: "lender", Flavor: "h100", Resource: "nvidia.com/gpu"}, Since: t0}}))
			Expect(g.WaitingLenders(t0.Add(30*time.Second))).To(BeEmpty(), "inside the debounce")
		})

		It("names the placement hold and a placement-timeout release", func() {
			g := NewBorrowGovernor(DefaultBorrowSettings())
			r := govModel("m", 2, 8)
			r.VariantStates[0].UnplacedReplicas, r.VariantStates[0].UnplacedSince = 1, t0
			g.Observe(BorrowCycle{Now: t0, Requests: []ModelScalingRequest{r}})
			Expect(g.VariantStatus("b", "m-v")).To(Equal(VariantBorrowStatus{Unplaced: true}))

			c := BorrowCycle{Now: t0.Add(6 * time.Minute), Constraints: govWorld{used: 16}.constraints(), Requests: []ModelScalingRequest{r}}
			g.Observe(c)
			Expect(g.Releases(c, decisionsOf(c.Requests))).NotTo(BeEmpty())
			Expect(g.VariantStatus("b", "m-v")).To(Equal(VariantBorrowStatus{TimeoutRelease: true, Unplaced: true}))
		})
	})

	It("marks the budgets of a namespace whose preemptions could not be read, keeping a reason that holds anyway", func() {
		budgets := []LimiterBudget{
			{Namespace: "a", Borrowable: 8},
			{Namespace: "a", Accelerator: "A100", Reason: BorrowReasonPoolEmpty},
			{Namespace: "a", Accelerator: "L4", Reason: BorrowReasonNoBorrowCap},
			{Namespace: "a", Accelerator: "T4", Reason: string(kueue.ReasonNotKueueManaged)},
			{Namespace: "b", Borrowable: 4},
		}
		MarkPreemptionsUnreadable(budgets, map[string]bool{"a": true})
		Expect(budgets).To(Equal([]LimiterBudget{
			{Namespace: "a", Reason: BorrowReasonPreemptionsUnreadable},
			{Namespace: "a", Accelerator: "A100", Reason: BorrowReasonPreemptionsUnreadable},
			{Namespace: "a", Accelerator: "L4", Reason: BorrowReasonNoBorrowCap},
			{Namespace: "a", Accelerator: "T4", Reason: string(kueue.ReasonNotKueueManaged)},
			{Namespace: "b", Borrowable: 4},
		}))
	})

	It("finds the cohorts a governed namespace borrows in now", func() {
		Expect(BorrowingCohorts(govWorld{used: 32}.constraints(), nil)).To(Equal(map[string]bool{"c": true}))
		Expect(BorrowingCohorts(govWorld{used: 32}.constraints(), map[string]map[string]int{"b": {"": 32}})).To(BeEmpty(),
			"gated pods hold no reservation")
		Expect(BorrowingCohorts(govWorld{g: 8, used: 8}.constraints(), nil)).To(BeEmpty())
	})

	It("reports a stale read with no earlier figures as the reason, through the limiter", func() {
		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{
			Namespace: map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 8}}},
			Borrow: map[string][]config.ExternalBorrow{"team-a": {{Queue: "q", Cohort: "c", Flavor: "h100",
				Resource: "nvidia.com/gpu", Accelerator: "H100", Nominal: 8, Guaranteed: 8, BorrowCap: 8, WithinNominal: 8}}},
			ObservedAt: t0,
		}, nil)
		e := config.QuotaLimiterConfig{Name: "kq", Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
			Kueue:           &config.KueueQuotaSource{Enabled: true}}
		inv := NewQuotaInventoryWithSource(e, src)
		inv.now = func() time.Time { return t0.Add(time.Hour) }
		inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
		c, err := NewDefaultLimiter("kq", inv).ComputeConstraints(context.Background(), nil, map[string]map[string]int{"team-a": {}})
		Expect(err).NotTo(HaveOccurred())
		Expect(c.NamespaceBorrow).To(BeNil())
		Expect(c.BorrowWithheld).To(Equal(map[string]string{"team-a": string(kueue.ReasonStale)}))
		Expect(LimiterBudgets([]*ResourceConstraints{c}, nil)[0].Reason).To(Equal(string(kueue.ReasonStale)))
	})
})
