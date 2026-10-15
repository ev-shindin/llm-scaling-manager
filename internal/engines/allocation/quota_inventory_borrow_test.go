package allocation

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// fakeEligibility answers the engine-side B1 conditions from a table; a
// namespace it does not list is managed, with readable ResourceQuotas.
type fakeEligibility struct {
	byNS  map[string]kueue.NamespaceEligibility
	calls int
}

func (f *fakeEligibility) Evaluate(_ context.Context, namespaces []string) map[string]kueue.NamespaceEligibility {
	f.calls++
	out := map[string]kueue.NamespaceEligibility{}
	for _, ns := range namespaces {
		out[ns] = f.byNS[ns]
	}
	return out
}

var _ = Describe("QuotaInventory counting Kueue borrowing", func() {
	observed := time.Unix(1000, 0)
	fresh := func() time.Time { return observed.Add(10 * time.Second) }
	ctx := context.Background()

	entry := func(countBorrowable *bool) config.QuotaLimiterConfig {
		return config.QuotaLimiterConfig{
			Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
			Exclude:         []string{"ops"},
			Kueue:           &config.KueueQuotaSource{Enabled: true, CountBorrowable: countBorrowable},
		}
	}
	// team-a: reclaimWithinCohort Never, N=8, borrowingLimit 8, idle, beside an
	// idle lender: guaranteed 0, within nominal 8, reach 16.
	row := config.ExternalBorrow{
		Queue: "team-a-cq", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
		Accelerator: "H100", Nominal: 8, Guaranteed: 0, Borrowable: 16, BorrowCap: 8, WithinNominal: 8,
		Reason: "nominal-lent-out",
	}
	snapshotOf := func(r config.ExternalBorrow) config.ExternalQuotas {
		return config.ExternalQuotas{
			Namespace:              map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 8}}},
			NamespaceGuaranteed:    map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": r.Guaranteed}}},
			NamespaceWithinNominal: map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": r.WithinNominal}}},
			Borrow:                 map[string][]config.ExternalBorrow{"team-a": {r}},
			SharedPools: []config.ExternalSharedPool{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				Accelerator: "H100", Free: 16}},
			ObservedAt: observed,
		}
	}
	snapshot := snapshotOf(row)

	type built struct {
		inv *QuotaInventory
		lim *DefaultLimiter
	}
	build := func(e config.QuotaLimiterConfig, elig *fakeEligibility, physical bool, snaps ...config.ExternalQuotas) built {
		src := &fakeQuotaSource{}
		for _, s := range snaps {
			src.push(s, nil)
		}
		inv := NewQuotaInventoryWithSource(e, src)
		inv.now = fresh
		if elig != nil {
			inv.EnableBorrowing(BorrowOptions{Physical: physical, Eligibility: elig})
		}
		return built{inv, NewDefaultLimiter(e.Name, inv)}
	}
	constraintsOf := func(b built) *ResourceConstraints {
		c, err := b.lim.ComputeConstraints(ctx, nil, map[string]map[string]int{"team-a": {}})
		Expect(err).NotTo(HaveOccurred())
		return c
	}

	It("caps a counted namespace at its guaranteed figure and carries the borrowing figures", func() {
		b := build(entry(nil), &fakeEligibility{}, true, snapshot)
		c := constraintsOf(b)
		Expect(b.inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 0}))
		Expect(c.NamespacePools["team-a"]["H100"].Limit).To(Equal(0))
		Expect(c.NamespaceBorrow).To(Equal(map[string]map[string]NamespaceBorrow{
			"team-a": {"H100": {Cap: 16, WithinNominal: 8}},
		}))
		Expect(c.SharedPools).To(HaveLen(1))
		Expect(c.SharedPools[0].Free).To(Equal(16))
		Expect(c.NamespaceCohorts).To(Equal(map[string][]string{"team-a": {"c"}}))
		Expect(c.Pools["H100"].Limit).To(Equal(16), "the open aggregate is the reach, not the guaranteed sum")
	})

	It("bounds the borrow cap by the static entry", func() {
		e := entry(nil)
		e.NamespaceQuotas = map[string]map[string]int{"default": {"H100": 12}}
		c := constraintsOf(build(e, &fakeEligibility{}, true, snapshot))
		Expect(c.NamespaceBorrow["team-a"]["H100"].Cap).To(Equal(12))
	})

	// Each B1 condition withholds borrowing, and the constraints are then
	// exactly those of an entry that never counted it.
	headOf := func(e config.QuotaLimiterConfig, snap config.ExternalQuotas) *ResourceConstraints {
		return constraintsOf(build(e, nil, false, snap))
	}
	DescribeTable("withholds borrowing, identical to the nominal cap",
		func(elig kueue.NamespaceEligibility, physical bool, snap config.ExternalQuotas) {
			b := build(entry(nil), &fakeEligibility{byNS: map[string]kueue.NamespaceEligibility{"team-a": elig}}, physical, snap)
			got := constraintsOf(b)
			Expect(got.NamespaceBorrow).To(BeNil())
			Expect(got).To(Equal(headOf(entry(nil), snap)))
			Expect(b.inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 8}))
		},
		Entry("not-kueue-managed", kueue.NamespaceEligibility{Reason: kueue.ReasonNotKueueManaged}, true, snapshot),
		Entry("resourcequota-unreadable", kueue.NamespaceEligibility{Reason: kueue.ReasonResourceQuotaUnreadable}, true, snapshot),
		Entry("no-gpu-bound: no physical limiter, no ResourceQuota", kueue.NamespaceEligibility{}, false, snapshot),
		Entry("no-gpu-bound: a ResourceQuota above nominal plus borrowingLimit proves nothing",
			kueue.NamespaceEligibility{GPUQuota: map[string]int{"nvidia.com/gpu": 1000}}, false, snapshot),
		Entry("no-gpu-bound: borrowingLimit unset, only the physical limiter qualifies",
			kueue.NamespaceEligibility{GPUQuota: map[string]int{"nvidia.com/gpu": 16}}, false,
			snapshotOf(func() config.ExternalBorrow { r := row; r.BorrowCap = config.QuotaUnlimited; return r }())),
		Entry("stale (source reason)", kueue.NamespaceEligibility{}, true, snapshotOf(row.Withheld("stale"))),
		Entry("no-status", kueue.NamespaceEligibility{}, true, snapshotOf(row.Withheld("no-status"))),
		Entry("hierarchical-cohort", kueue.NamespaceEligibility{}, true, snapshotOf(row.Withheld("hierarchical-cohort"))),
		Entry("hierarchy-unknown", kueue.NamespaceEligibility{}, true, snapshotOf(row.Withheld("hierarchy-unknown"))),
		Entry("effective-quotas", kueue.NamespaceEligibility{}, true, snapshotOf(row.Withheld("effective-quotas"))),
		Entry("fair-sharing", kueue.NamespaceEligibility{}, true, snapshotOf(row.Withheld("fair-sharing"))),
	)

	It("counts a ResourceQuota at most nominal plus borrowingLimit as the GPU bound, and caps the reach with it", func() {
		elig := &fakeEligibility{byNS: map[string]kueue.NamespaceEligibility{"team-a": {GPUQuota: map[string]int{"nvidia.com/gpu": 12}}}}
		c := constraintsOf(build(entry(nil), elig, false, snapshot))
		Expect(c.NamespaceBorrow["team-a"]["H100"].Cap).To(Equal(12))
	})

	It("with countBorrowable: false is exactly the nominal cap, through the optimizer too (golden)", func() {
		off := false
		b := build(entry(&off), &fakeEligibility{}, true, snapshot)
		got := constraintsOf(b)
		head := headOf(entry(&off), snapshot)
		Expect(got).To(Equal(head))
		reqs := []ModelScalingRequest{
			borrowModel("team-a", "m1", 4, 1, 6, nil),
			borrowModel("team-a", "m2", 8, 0, 2, nil),
		}
		golden := NewGreedyByScoreOptimizer().Optimize(ctx, reqs, []*ResourceConstraints{physicalH100(64), head})
		Expect(NewGreedyByScoreOptimizer().Optimize(ctx, reqs, []*ResourceConstraints{physicalH100(64), got})).To(ConsistOf(golden))
		sum := 0
		for _, g := range addedGPUs(golden) {
			sum += g
		}
		Expect(sum).To(Equal(8), "nominal 8 bounds the namespace")
	})

	It("freezes on a stale read: the figures stay, nothing new is borrowed", func() {
		src := &fakeQuotaSource{}
		src.push(snapshot, nil)
		src.push(snapshot, errors.New("api down"))
		inv := NewQuotaInventoryWithSource(entry(nil), src)
		inv.now = fresh
		inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
		lim := NewDefaultLimiter("namespace-quota", inv)

		c, _ := lim.ComputeConstraints(ctx, nil, map[string]map[string]int{"team-a": {}})
		Expect(c.SharedPools[0].Free).To(Equal(16))
		c, _ = lim.ComputeConstraints(ctx, nil, map[string]map[string]int{"team-a": {}})
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 0}), "never shrunk back to nominal, never grown")
		Expect(c.NamespaceBorrow["team-a"]["H100"].Cap).To(Equal(16))
		Expect(c.SharedPools[0].Free).To(Equal(0), "no new borrowing while stale")
	})

	It("reads a snapshot older than twice the refresh interval as stale", func() {
		src := &fakeQuotaSource{}
		src.push(snapshot, nil)
		inv := NewQuotaInventoryWithSource(entry(nil), src)
		inv.now = func() time.Time { return observed.Add(2*config.DefaultKueueRefreshInterval + time.Second) }
		inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 8}),
			"stale with no earlier figures: the nominal cap")
	})

	It("logs what it counts and why it withholds, once per change", func() {
		var lines []string
		lctx := logr.NewContext(ctx, funcr.NewJSON(func(obj string) { lines = append(lines, obj) }, funcr.Options{}))
		count := func() int {
			n := 0
			for _, l := range lines {
				if strings.Contains(l, "Kueue borrowing counted") {
					n++
				}
			}
			return n
		}
		elig := &fakeEligibility{byNS: map[string]kueue.NamespaceEligibility{"team-a": {Reason: kueue.ReasonNotKueueManaged}}}
		b := build(entry(nil), elig, true, snapshot)
		Expect(b.inv.Refresh(lctx)).To(Succeed())
		Expect(b.inv.Refresh(lctx)).To(Succeed())
		Expect(count()).To(Equal(1))
		Expect(strings.Join(lines, "\n")).To(ContainSubstring(`"reason":"not-kueue-managed"`))
	})

	It("lets an idle Never namespace wake within nominal on a default setup", func() {
		c := constraintsOf(build(entry(nil), &fakeEligibility{}, true, snapshot))
		within := WithinNominal([]*ResourceConstraints{c})
		Expect(FitsGPUBudget(within, "team-a", map[string]int{"H100": 8})).To(BeTrue(),
			"guaranteed is 0 under Never; within nominal is what Kueue would admit now")
		Expect(FitsGPUBudget(within, "team-a", map[string]int{"H100": 9})).To(BeFalse())
	})
})
