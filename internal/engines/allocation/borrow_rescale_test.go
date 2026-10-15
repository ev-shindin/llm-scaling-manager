package allocation

import (
	"context"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// demanding sets the model's TotalDemand, which rescale sizes a group by, to
// replicas single-GPU replicas' worth.
func demanding(req ModelScalingRequest, replicas int) ModelScalingRequest {
	res := *req.CompositeSignal.Result
	res.TotalDemand = float64(replicas) * 10000
	req.CompositeSignal.Result = &res
	return req
}

// Rescale in a pass that counts cohort borrowing spends within nominal only
// (B7), and what it fills is charged to the pass's budgets so the additive path
// cannot order the same GPUs again (applyRescaleWithinNominal).
//
// Namespace "a" has rescale on and two single-GPU models wanting 5 GPUs each:
// a contended group. Its guaranteed figure is 2 and its within-nominal figure
// 4, so a fill of 4 goes 2 past guaranteed, into nominal a peer holds. "b" is a
// pure borrower (guaranteed 0, cap unlimited) in the same cohort, not
// rescaled: it spends through the additive path.
func TestGreedyRescaleCountingBorrowing(t *testing.T) {
	ctx := context.Background()
	rescaled := func() *GreedyByScoreOptimizer {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{ByNamespace: map[string]bool{"a": true}}
		return o
	}
	requests := func(withB bool) []ModelScalingRequest {
		r := []ModelScalingRequest{
			demanding(borrowModel("a", "ma1", 1, 0, 5, nil), 5),
			demanding(borrowModel("a", "ma2", 1, 0, 5, nil), 5),
		}
		if withB {
			r = append(r, borrowModel("b", "mb", 1, 0, 40, nil))
		}
		return r
	}
	quota := func(poolFree int) *ResourceConstraints {
		return borrowQuota(poolFree,
			nsBorrow{ns: "a", guaranteed: 2, cap: config.QuotaUnlimited, wn: 4},
			nsBorrow{ns: "b", cap: config.QuotaUnlimited})
	}
	run := func(physical, poolFree int, withB bool) (map[string]int, *GreedyByScoreOptimizer) {
		o := rescaled()
		got := addedGPUs(o.Optimize(ctx, requests(withB), []*ResourceConstraints{physicalH100(physical), quota(poolFree)}))
		return got, o
	}

	t.Run("fill never exceeds within nominal, even with a pool free", func(t *testing.T) {
		got, o := run(64, 32, false)
		if a := got["ma1"] + got["ma2"]; a != 4 {
			t.Errorf("rescale filled %d GPUs in a, want exactly its within-nominal 4 (pool had 32 free)", a)
		}
		for _, d := range o.BorrowDraws() {
			if d.Namespace == "a" {
				t.Errorf("rescale borrowed: %+v", d)
			}
		}
	})

	t.Run("the additive path cannot order the fill's GPUs again", func(t *testing.T) {
		// 6 physical H100s: a's fill takes 4, so b may order 2, not 6.
		got, _ := run(6, 32, true)
		a := got["ma1"] + got["ma2"]
		if a != 4 {
			t.Fatalf("premise: rescale filled %d in a, want 4", a)
		}
		if a+got["mb"] > 6 {
			t.Errorf("ordered %d GPUs (a %d + b %d) against 6 physical", a+got["mb"], a, got["mb"])
		}
		if got["mb"] != 2 {
			t.Errorf("b ordered %d, want the 2 physical GPUs left", got["mb"])
		}
	})

	t.Run("a fill past guaranteed debits the pool", func(t *testing.T) {
		// Pool 3 free; a's fill goes 2 past its guaranteed 2, so b borrows 1.
		got, _ := run(64, 3, true)
		if a := got["ma1"] + got["ma2"]; a != 4 {
			t.Fatalf("premise: rescale filled %d in a, want 4", a)
		}
		if got["mb"] != 1 {
			t.Errorf("b borrowed %d, want 1: the pool's 3 less the 2 a grew past guaranteed", got["mb"])
		}
	})

	t.Run("the pool debit clamps at 0", func(t *testing.T) {
		got, _ := run(64, 1, true)
		if a := got["ma1"] + got["ma2"]; a != 4 {
			t.Errorf("rescale filled %d in a, want 4: growth into lent-out nominal is not gated", a)
		}
		if got["mb"] != 0 {
			t.Errorf("b borrowed %d from a pool the fill emptied", got["mb"])
		}
	})

	t.Run("control: without borrowing the rescale path fills to the namespace quota", func(t *testing.T) {
		c := &ResourceConstraints{
			ProviderName:   "namespace-quota",
			NamespacePools: map[string]map[string]ResourcePool{"a": {"H100": {Limit: 4}}},
		}
		c.Pools = aggregateNamespacePools(c.NamespacePools)
		o := rescaled()
		got := addedGPUs(o.Optimize(ctx, requests(false), []*ResourceConstraints{physicalH100(64), c}))
		if a := got["ma1"] + got["ma2"]; a != 4 {
			t.Errorf("rescale filled %d in a, want 4", a)
		}
		if o.BorrowDraws() != nil {
			t.Errorf("a pass without borrowing recorded draws: %v", o.BorrowDraws())
		}
	})
}
