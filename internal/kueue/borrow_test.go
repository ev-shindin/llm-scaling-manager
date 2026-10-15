package kueue

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

var gpuKey = ResourceKey{Flavor: "h100", Resource: "nvidia.com/gpu"}

func ptr(n int) *int { return &n }

// member is a ClusterQueue in cohort "c" with status written, holding one
// GPU resource of the h100 flavor.
func member(name string, r QueueResource) QueueState {
	return QueueState{
		Name: name, Cohort: "c", Reclaim: ReclaimNever, HasStatus: true,
		Resources: map[ResourceKey]QueueResource{gpuKey: r},
	}
}

// withReclaim returns q with spec.preemption.reclaimWithinCohort set.
func withReclaim(q QueueState, policy string) QueueState {
	q.Reclaim = policy
	return q
}

func topology(qs ...QueueState) Topology {
	t := Topology{Queues: map[string]QueueState{}, Cohorts: map[string]CohortState{}}
	for _, q := range qs {
		t.Queues[q.Name] = q
	}
	return t
}

// figures returns the only figures of queue name.
func figures(b Borrowing, name string) QueueFigures {
	ExpectWithOffset(1, b.Queues[name]).To(HaveLen(1))
	return b.Queues[name][0]
}

var _ = Describe("ComputeBorrowing", func() {
	It("reproduces Kueue's documented example: team-a N9 B1 beside an idle team-b N12 reaches 10", func() {
		t := topology(
			member("team-a", QueueResource{Nominal: 9, BorrowingLimit: ptr(1)}),
			member("team-b", QueueResource{Nominal: 12}),
		)
		a := figures(ComputeBorrowing(t, false), "team-a")
		Expect(a.Reason).To(BeEmpty())
		Expect(a.CohortFree).To(Equal(21))
		Expect(a.Guaranteed + a.Borrowable).To(Equal(10))
		Expect(a.BorrowCap).To(Equal(1))

		t.Queues["team-a"] = withReclaim(t.Queues["team-a"], ReclaimAny)
		a = figures(ComputeBorrowing(t, false), "team-a")
		Expect(a.Guaranteed).To(Equal(9))
		Expect(a.Borrowable).To(Equal(1))
	})

	Describe("guaranteed under each reclaim policy", func() {
		// q holds nothing of its nominal 8; a peer with no quota borrows all 8.
		lentOut := func(policy string) QueueFigures {
			t := topology(
				withReclaim(member("q", QueueResource{Nominal: 8}), policy),
				member("peer", QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8}),
			)
			return figures(ComputeBorrowing(t, false), "q")
		}

		It("Never: unused nominal a peer borrowed stays with the peer", func() {
			f := lentOut(ReclaimNever)
			Expect(f.CohortFree).To(Equal(0))
			Expect(f.Guaranteed).To(Equal(0))
			Expect(f.Borrowable).To(Equal(0))
			Expect(f.GuaranteedReason).To(Equal(ReasonNominalLentOut))
		})

		It("absent policy is Never", func() {
			Expect(lentOut("")).To(Equal(lentOut(ReclaimNever)))
		})

		It("Any and LowerPriority: nominal is guaranteed, Kueue preempts the peer", func() {
			for _, p := range []string{ReclaimAny, ReclaimLowerPriority} {
				f := lentOut(p)
				Expect(f.Guaranteed).To(Equal(8), p)
				Expect(f.Borrowable).To(Equal(0), p)
				Expect(f.GuaranteedReason).To(BeEmpty(), p)
			}
		})

		It("Never: what the queue already uses is guaranteed", func() {
			t := topology(
				member("q", QueueResource{Nominal: 8, Reserved: 5}),
				member("peer", QueueResource{Nominal: 0, Reserved: 3, Borrowed: 3}),
			)
			f := figures(ComputeBorrowing(t, false), "q")
			Expect(f.Guaranteed).To(Equal(5))
			Expect(f.Borrowable).To(Equal(0))
			Expect(f.GuaranteedReason).To(Equal(ReasonNominalLentOut))
		})

		It("a reclaiming queue whose nominal is all lent out still reaches nominal, and borrows past it only from what is left", func() {
			t := topology(
				withReclaim(member("q", QueueResource{Nominal: 8}), ReclaimAny),
				member("peer", QueueResource{Nominal: 4, Reserved: 12, Borrowed: 8}),
			)
			f := figures(ComputeBorrowing(t, false), "q")
			Expect(f.Guaranteed).To(Equal(8))
			Expect(f.Borrowable).To(Equal(0), "12 lendable, 12 used beyond kept: nothing left")
		})
	})

	Describe("clamps", func() {
		It("a lendingLimit above nominal lends nominal", func() {
			over := topology(
				member("q", QueueResource{Nominal: 0}),
				member("lender", QueueResource{Nominal: 8, LendingLimit: ptr(20)}),
			)
			unset := topology(
				member("q", QueueResource{Nominal: 0}),
				member("lender", QueueResource{Nominal: 8}),
			)
			Expect(figures(ComputeBorrowing(over, false), "q")).To(Equal(figures(ComputeBorrowing(unset, false), "q")))
			Expect(figures(ComputeBorrowing(over, false), "q").Borrowable).To(Equal(8))
		})

		It("lendingLimit 0 lends nothing, and keeps the whole nominal", func() {
			t := topology(
				member("q", QueueResource{Nominal: 0}),
				member("lender", QueueResource{Nominal: 8, LendingLimit: ptr(0)}),
			)
			b := ComputeBorrowing(t, false)
			Expect(figures(b, "q").Borrowable).To(Equal(0))
			lender := figures(b, "lender")
			Expect(lender.Guaranteed).To(Equal(8), "kept quota is guaranteed even under Never")
			Expect(lender.GuaranteedReason).To(BeEmpty())
		})

		It("a partial lendingLimit lends only that much", func() {
			t := topology(
				member("q", QueueResource{Nominal: 0}),
				member("lender", QueueResource{Nominal: 8, LendingLimit: ptr(3)}),
			)
			Expect(figures(ComputeBorrowing(t, false), "q").Borrowable).To(Equal(3))
		})

		It("borrowingLimit 0 caps the reach at nominal", func() {
			t := topology(
				withReclaim(member("q", QueueResource{Nominal: 4, BorrowingLimit: ptr(0)}), ReclaimAny),
				member("lender", QueueResource{Nominal: 8}),
			)
			f := figures(ComputeBorrowing(t, false), "q")
			Expect(f.Guaranteed).To(Equal(4))
			Expect(f.Borrowable).To(Equal(0))
			Expect(f.BorrowCap).To(Equal(0))
		})

		It("an unset borrowingLimit is unlimited", func() {
			t := topology(
				member("q", QueueResource{Nominal: 0}),
				member("lender", QueueResource{Nominal: 16}),
			)
			f := figures(ComputeBorrowing(t, false), "q")
			Expect(f.BorrowCap).To(Equal(config.QuotaUnlimited))
			Expect(f.Borrowable).To(Equal(16))
		})

		It("nominal 0 guarantees nothing and borrows up to its limit", func() {
			t := topology(
				member("q", QueueResource{Nominal: 0, BorrowingLimit: ptr(5)}),
				member("lender", QueueResource{Nominal: 16}),
			)
			f := figures(ComputeBorrowing(t, false), "q")
			Expect(f.Guaranteed).To(Equal(0))
			Expect(f.GuaranteedReason).To(BeEmpty(), "nominal 0 is not below nominal")
			Expect(f.Borrowable).To(Equal(5))
		})
	})

	It("two borrowers see the same free pool, which is one shared chunk", func() {
		t := topology(
			member("lender", QueueResource{Nominal: 8}),
			member("a", QueueResource{Nominal: 0}),
			member("b", QueueResource{Nominal: 0}),
		)
		t.Flavors = map[string]FlavorPlacement{"h100": {
			AcceleratorType: "NVIDIA-H100-80GB-HBM3",
			NodeLabels:      map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3", "pool": "train"},
			Tolerations:     []corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
		}}
		b := ComputeBorrowing(t, false)
		Expect(figures(b, "a").Borrowable).To(Equal(8))
		Expect(figures(b, "b").Borrowable).To(Equal(8))
		Expect(b.Pools).To(HaveLen(1), "8 free GPUs, spent once across both, not 16")
		p := b.Pools[0]
		Expect(p.Cohort).To(Equal("c"))
		Expect(p.ResourceKey).To(Equal(gpuKey))
		Expect(p.Free).To(Equal(8))
		Expect(p.NodeLabels).To(HaveKeyWithValue("pool", "train"))
		Expect(p.Tolerations).To(HaveLen(1))
	})

	It("describes each lender of a pool: waiting, whether it can reclaim, and the nominal it does not hold", func() {
		waiting := member("lender", QueueResource{Nominal: 8, Reserved: 3})
		waiting.Pending = 2
		b := ComputeBorrowing(topology(withReclaim(waiting, ReclaimAny), member("a", QueueResource{Nominal: 0, Reserved: 4})), false)
		Expect(b.Pools).To(HaveLen(1))
		Expect(b.Pools[0].Lenders).To(Equal([]LenderState{
			{Queue: "a", LentOut: 0},
			{Queue: "lender", Waiting: true, CanReclaim: true, LentOut: 5},
		}))
	})

	It("a borrower's own usage past its kept part leaves the pool", func() {
		t := topology(
			member("lender", QueueResource{Nominal: 8}),
			member("a", QueueResource{Nominal: 0, Reserved: 3, Borrowed: 3}),
		)
		b := ComputeBorrowing(t, false)
		Expect(b.Pools[0].Free).To(Equal(5))
		a := figures(b, "a")
		Expect(a.Guaranteed).To(Equal(0))
		Expect(a.Borrowable).To(Equal(8), "what it holds plus what is left")
		Expect(a.KueueBorrowed).To(Equal(3))
	})

	// Kueue's cache puts every ClusterQueue into its cohort and the snapshot
	// clones the cohort's figures before dropping inactive queues, so a held
	// queue's usage and lending still count; it only stops being a borrower.
	It("counts a held queue's borrowed usage against the pool", func() {
		held := member("held", QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8})
		held.Inactive = true
		t := topology(held, member("lender", QueueResource{Nominal: 8}), member("a", QueueResource{Nominal: 0}))
		b := ComputeBorrowing(t, false)
		a := figures(b, "a")
		Expect(a.CohortFree).To(Equal(0), "the held queue's 8 running borrowed GPUs are not free")
		Expect(a.Borrowable).To(Equal(0))
		Expect(b.Pools).To(HaveLen(1))
		Expect(b.Pools[0].Free).To(Equal(0))
		Expect(figures(b, "held").Reason).To(Equal(ReasonInactive))
	})

	It("counts a held queue's lending, and leaves it out as a borrower and as a waiting lender", func() {
		held := member("held", QueueResource{Nominal: 8})
		held.Inactive = true
		held.Pending = 3
		t := topology(held, member("a", QueueResource{Nominal: 0}))
		b := ComputeBorrowing(t, false)
		Expect(figures(b, "a").Borrowable).To(Equal(8))
		Expect(figures(b, "held").Reason).To(Equal(ReasonInactive))
		Expect(b.Pools).To(HaveLen(1))
		Expect(b.Pools[0].WaitingLenders).To(BeEmpty())
		Expect(b.Pools[0].Lenders).To(Equal([]LenderState{{Queue: "a"}}))
	})

	It("withholds borrowing when a held member has no status: its usage is unknown", func() {
		held := member("held", QueueResource{Nominal: 8})
		held.Inactive, held.HasStatus = true, false
		t := topology(held, member("a", QueueResource{Nominal: 0}))
		Expect(figures(ComputeBorrowing(t, false), "a").Reason).To(Equal(ReasonNoStatus))
	})

	Describe("reasons borrowing is not counted", func() {
		base := func() Topology {
			return topology(
				member("q", QueueResource{Nominal: 4}),
				member("lender", QueueResource{Nominal: 8}),
			)
		}
		withheld := func(t Topology, stale bool) QueueFigures {
			b := ComputeBorrowing(t, stale)
			for _, p := range b.Pools {
				ExpectWithOffset(1, p.Free).To(Equal(0), "a withheld pool offers nothing")
			}
			f := figures(b, "q")
			ExpectWithOffset(1, f.Borrowable).To(Equal(0))
			ExpectWithOffset(1, f.Guaranteed).To(Equal(4), "guaranteed falls back to nominal, as today")
			return f
		}

		It("counts a flat cohort, with or without a Cohort object", func() {
			t := base()
			Expect(figures(ComputeBorrowing(t, false), "q").Reason).To(BeEmpty())
			t.Cohorts["c"] = CohortState{Name: "c"}
			Expect(figures(ComputeBorrowing(t, false), "q").Reason).To(BeEmpty())
		})

		It("stale", func() {
			Expect(withheld(base(), true).Reason).To(Equal(ReasonStale))
		})

		It("no-status when any member's status is absent", func() {
			t := base()
			l := t.Queues["lender"]
			l.HasStatus = false
			t.Queues["lender"] = l
			Expect(withheld(t, false).Reason).To(Equal(ReasonNoStatus))
		})

		It("hierarchy-unknown when the Cohort objects could not be listed", func() {
			t := base()
			t.Cohorts, t.CohortsUnreadable = nil, true
			Expect(withheld(t, false).Reason).To(Equal(ReasonHierarchyUnknown))
		})

		It("hierarchical-cohort for a cohort with a parent", func() {
			t := base()
			t.Cohorts["c"] = CohortState{Name: "c", Parent: "org"}
			Expect(withheld(t, false).Reason).To(Equal(ReasonHierarchicalCohort))
		})

		It("hierarchical-cohort for a cohort with its own resourceGroups", func() {
			t := base()
			t.Cohorts["c"] = CohortState{Name: "c", HasResourceGroups: true}
			Expect(withheld(t, false).Reason).To(Equal(ReasonHierarchicalCohort))
		})

		It("hierarchical-cohort for an implicit root another Cohort names as its parent", func() {
			t := base()
			t.Cohorts["child"] = CohortState{Name: "child", Parent: "c"}
			Expect(withheld(t, false).Reason).To(Equal(ReasonHierarchicalCohort))
		})

		It("effective-quotas", func() {
			t := base()
			l := t.Queues["lender"]
			l.EffectiveQuotas = true
			t.Queues["lender"] = l
			Expect(withheld(t, false).Reason).To(Equal(ReasonEffectiveQuotas))
		})

		It("fair-sharing", func() {
			t := base()
			q := t.Queues["q"]
			q.FairSharing = true
			t.Queues["q"] = q
			Expect(withheld(t, false).Reason).To(Equal(ReasonFairSharing))
		})

		It("malformed", func() {
			t := base()
			l := t.Queues["lender"]
			l.Malformed = true
			t.Queues["lender"] = l
			Expect(withheld(t, false).Reason).To(Equal(ReasonMalformed))
		})

		It("no-cohort for a queue in no cohort", func() {
			t := base()
			q := t.Queues["q"]
			q.Cohort = ""
			t.Queues["q"] = q
			f := figures(ComputeBorrowing(t, false), "q")
			Expect(f.Reason).To(Equal(ReasonNoCohort))
			Expect(f.Guaranteed).To(Equal(4))
			Expect(f.Borrowable).To(Equal(0))
		})
	})

	Describe("lender waiting, per read", func() {
		It("holds with pending workloads below nominal, and only then", func() {
			q := member("lender", QueueResource{Nominal: 8, Reserved: 4})
			Expect(LenderWaiting(q, gpuKey)).To(BeFalse(), "nothing pending")
			q.Pending = 2
			Expect(LenderWaiting(q, gpuKey)).To(BeTrue())
			Expect(LenderWaiting(q, ResourceKey{Flavor: "a100", Resource: "nvidia.com/gpu"})).To(BeFalse(), "another flavor")
			full := member("lender", QueueResource{Nominal: 8, Reserved: 8})
			full.Pending = 2
			Expect(LenderWaiting(full, gpuKey)).To(BeFalse(), "at nominal: its own quota is not what holds it")
		})

		It("is listed on the pool", func() {
			lender := member("lender", QueueResource{Nominal: 8, Reserved: 0})
			lender.Pending = 1
			b := ComputeBorrowing(topology(lender, member("a", QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8})), false)
			Expect(b.Pools).To(HaveLen(1))
			Expect(b.Pools[0].WaitingLenders).To(Equal([]string{"lender"}))
		})
	})
})
