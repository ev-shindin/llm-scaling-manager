package allocation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

func sumTargets(t map[string]int) int {
	s := 0
	for _, v := range t {
		s += v
	}
	return s
}

var _ = Describe("computeRescaleTargets", func() {
	DescribeTable("priority-weighted water-filling",
		func(in []rescaleInput, budget int, want map[string]int, overBudget bool) {
			got, over := computeRescaleTargets(in, budget)
			Expect(over).To(Equal(overBudget))
			for id, w := range want {
				Expect(got[id]).To(Equal(w), "target[%s]", id)
			}
			if !overBudget {
				// Invariant: the split never over-allocates the budget.
				Expect(sumTargets(got)).To(BeNumerically("<=", budget))
			}
			for _, m := range in {
				// Invariant: no model exceeds its cap.
				Expect(got[m.ID]).To(BeNumerically("<=", m.CapGPUs), "cap for %s", m.ID)
			}
		},
		// Proposal worked example: A holds the pool, B (higher priority) is starved;
		// rescale reclaims from A to give B its priority-weighted share.
		Entry("proposal: A prio1 dem8, B prio3 dem8, budget 8 -> A2 B6",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 8, CapGPUs: 8}, {ID: "B", Priority: 3, Demand: 8, CapGPUs: 8}},
			8, map[string]int{"A": 2, "B": 6}, false),
		// B wants less than its share (demand 4): its 4.8 caps at 4 and the freed
		// excess flows back to A.
		Entry("proposal: B demand 4 caps and re-splits -> A4 B4",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 8, CapGPUs: 8}, {ID: "B", Priority: 3, Demand: 4, CapGPUs: 4}},
			8, map[string]int{"A": 4, "B": 4}, false),
		Entry("minReplicas floors reserved first",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 10, FloorGPUs: 2, CapGPUs: 10}, {ID: "B", Priority: 1, Demand: 10, CapGPUs: 10}},
			6, map[string]int{"A": 4, "B": 2}, false),
		Entry("fractional shares round by largest remainder (tie by id)",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 1, CapGPUs: 7}, {ID: "B", Priority: 1, Demand: 1, CapGPUs: 7}},
			7, map[string]int{"A": 4, "B": 3}, false),
		Entry("zero-demand model gets floor only",
			[]rescaleInput{{ID: "idle", Priority: 1, Demand: 0, FloorGPUs: 1, CapGPUs: 5}, {ID: "busy", Priority: 1, Demand: 8, CapGPUs: 8}},
			6, map[string]int{"idle": 1, "busy": 5}, false),
		Entry("floors exceed budget -> conflict (clamp to floors)",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 5, FloorGPUs: 4, CapGPUs: 5}, {ID: "B", Priority: 1, Demand: 5, FloorGPUs: 3, CapGPUs: 5}},
			6, map[string]int{"A": 4, "B": 3}, true),
	)

	It("is deterministic across runs", func() {
		in := []rescaleInput{
			{ID: "z", Priority: 1, Demand: 1, CapGPUs: 9},
			{ID: "a", Priority: 1, Demand: 1, CapGPUs: 9},
			{ID: "m", Priority: 1, Demand: 1, CapGPUs: 9},
		}
		first, _ := computeRescaleTargets(in, 7)
		for i := 0; i < 20; i++ {
			got, _ := computeRescaleTargets(in, 7)
			Expect(got).To(Equal(first))
		}
	})
})

var _ = Describe("roundExtras", func() {
	// The water-fill caps at headroom before rounding, so this cap-skip guard is
	// unreachable through computeRescaleTargets; exercise it directly. The model with
	// the largest fractional remainder is already at its cap, so the leftover GPU must
	// skip to the next-largest remainder instead of breaching the cap.
	It("skips a largest-remainder model already at its cap", func() {
		in := []rescaleInput{{ID: "A", CapGPUs: 2}, {ID: "B", CapGPUs: 5}}
		targets := map[string]int{"A": 2, "B": 0} // A already at its cap of 2
		roundExtras(in, map[string]float64{"A": 0.9, "B": 0.4}, targets)
		Expect(targets["A"]).To(Equal(2), "A is at cap; leftover must not push it to 3")
		Expect(targets["B"]).To(Equal(1), "leftover GPU falls to B")
	})

	// 0.1 + 0.35 + 0.05 sums to 0.5 in some orders and to 0.49999999999999994
	// in others, and the sum is rounded to the leftover: taken in a map's
	// random order, the same extras handed out one GPU on some runs and none
	// on others.
	It("hands out the same leftover however the extras map was built", func() {
		in := []rescaleInput{{ID: "A", CapGPUs: 9}, {ID: "B", CapGPUs: 9}, {ID: "C", CapGPUs: 9}}
		ids := []string{"A", "B", "C"}
		figures := map[string]float64{"A": 0.1, "B": 0.35, "C": 0.05}
		run := func(i int) map[string]int {
			extra := make(map[string]float64, len(ids))
			for j := range ids {
				id := ids[(i+j)%len(ids)]
				extra[id] = figures[id]
			}
			targets := map[string]int{"A": 0, "B": 0, "C": 0}
			roundExtras(in, extra, targets)
			return targets
		}
		want := run(0)
		for i := range 1000 {
			Expect(run(i)).To(Equal(want), "run %d: the leftover moved with the map's order", i)
		}
	})
})

var _ = Describe("distributeGPUsByWeight", func() {
	It("splits proportionally to weight, not evenly", func() {
		// 4 GPUs with role demand 6:2 must split 3:1 — a 50/50 split (2:2) or a
		// swapped/ignored weight map would fail here.
		out := distributeGPUsByWeight(4, []string{"decode", "prefill"},
			map[string]int{"prefill": 6, "decode": 2}, nil, nil, nil)
		Expect(out["prefill"]).To(Equal(3))
		Expect(out["decode"]).To(Equal(1))
	})

	It("falls back to the fallback weights when all weights are zero", func() {
		out := distributeGPUsByWeight(4, []string{"decode", "prefill"},
			map[string]int{"prefill": 0, "decode": 0},
			map[string]int{"prefill": 3, "decode": 1}, nil, nil)
		Expect(out["prefill"]).To(Equal(3))
		Expect(out["decode"]).To(Equal(1))
	})

	It("gives a single role the whole total", func() {
		out := distributeGPUsByWeight(5, []string{"both"}, map[string]int{"both": 0}, nil, nil, nil)
		Expect(out["both"]).To(Equal(5))
	})

	It("reserves each role's floor before the weighted split", func() {
		// Cold-start P/D: zero demand and zero current on both roles, but each role has
		// a floor of 1. Without floor reservation the whole total lands on the
		// alphabetically-first role (decode), starving prefill's minReplicas.
		out := distributeGPUsByWeight(2, []string{"decode", "prefill"},
			map[string]int{"prefill": 0, "decode": 0}, // demand
			map[string]int{"prefill": 0, "decode": 0}, // current
			map[string]int{"prefill": 1, "decode": 1}, // floor
			nil) // ceiling
		Expect(out["prefill"]).To(Equal(1), "prefill floor honored")
		Expect(out["decode"]).To(Equal(1), "decode floor honored")
	})

	It("gives the weighted remainder on top of reserved floors", func() {
		// 6 GPUs, floors 1:1 (reserve 2), remainder 4 split by demand 3:1 → prefill 3+1,
		// decode 1+1.
		out := distributeGPUsByWeight(6, []string{"decode", "prefill"},
			map[string]int{"prefill": 3, "decode": 1},
			map[string]int{"prefill": 0, "decode": 0},
			map[string]int{"prefill": 1, "decode": 1}, nil)
		Expect(out["prefill"]).To(Equal(4))
		Expect(out["decode"]).To(Equal(2))
	})

	It("caps a role at its ceiling and re-splits the excess", func() {
		// 40 GPUs by demand 560:80 is 35:5; prefill can reach only 16, so
		// decode takes the other 24. A ceiling below the floor is the floor.
		out := distributeGPUsByWeight(40, []string{"decode", "prefill"},
			map[string]int{"prefill": 560, "decode": 80}, nil, nil,
			map[string]int{"prefill": 16})
		Expect(out).To(Equal(map[string]int{"prefill": 16, "decode": 24}))

		out = distributeGPUsByWeight(40, []string{"decode", "prefill"},
			map[string]int{"prefill": 560, "decode": 80}, nil,
			map[string]int{"prefill": 20}, map[string]int{"prefill": 16, "decode": 8})
		Expect(out).To(Equal(map[string]int{"prefill": 20, "decode": 8}), "both capped: the rest is unassigned")
	})
})

var _ = Describe("rescaleModelDecisions", func() {
	// Nil-guard: no upstream invariant actually guarantees CompositeSignal carries a
	// saturation result (rescaleInputsForGroup, the sibling that builds this function's
	// caller's groups, already guards this exact case). Without the guard,
	// buildVariantRecords(req, satNamed.Result) would nil-deref on satNamed.
	It("returns nil instead of panicking when CompositeSignal has no result", func() {
		o := NewGreedyByScoreOptimizer()
		req := ModelScalingRequest{
			ModelID:   "A",
			Namespace: "default",
			Priority:  1,
			// CompositeSignal deliberately left zero-valued (Result == nil).
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "A-v", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
		}
		freeThisCycle := 0
		var got []domain.VariantDecision
		Expect(func() {
			got = o.rescaleModelDecisions(context.Background(), req, nil, "A100", 1, &freeThisCycle)
		}).NotTo(Panic())
		Expect(got).To(BeEmpty())
	})
})

var _ = Describe("roleDemandGPUs", func() {
	It("rounds token demand up to whole replicas (ceil)", func() {
		// 8500 tokens / 1000 per-replica capacity = 8.5 -> ceil = 9 replicas (9 GPUs).
		// Guards against a regression to floor (which would under-count demand at 8).
		sat := NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName,
			Result: &domain.AnalyzerResult{
				ModelID:     "A",
				TotalDemand: 8500,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "A-v", PerReplicaCapacity: 1000},
				},
			},
		}
		stateMap := map[string]domain.VariantReplicaState{
			"A-v": {VariantName: "A-v", GPUsPerReplica: 1},
		}
		req := ModelScalingRequest{
			Variants: []domain.VariantMetadata{
				{VariantName: "A-v", AcceleratorName: "A100"},
			},
		}
		records := buildVariantRecords(req, sat.Result)
		Expect(roleDemandGPUs(sat, records, stateMap, "A100", domain.RoleBoth)).To(Equal(9))
	})
})
