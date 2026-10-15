package steadystate

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// One steady-state cycle's stage 2 as optimizeV2 runs it: prepareBorrowing
// (the governor observes the lenders and closes blocked cohorts), the greedy
// optimizer wired with the cycle's node view and borrow block, decideV2, the
// in-flight record and the releases.
var _ = Describe("a waiting reclaiming lender in the steady-state cycle", func() {
	const (
		ns          = "a"
		guaranteed  = 2
		replicaGPUs = 1
	)
	ctx := context.Background()
	t0 := time.Unix(10_000, 0)

	AfterEach(func() { decision.DefaultHeadroom.Reset() })

	// "a" holds its guaranteed 2 GPUs and wants 6 more; its cohort's pool has 8
	// free, lent by a queue that can reclaim them.
	constraints := func(lenderWaiting bool, readAt time.Time) []*allocation.ResourceConstraints {
		return []*allocation.ResourceConstraints{{
			ProviderName:     "namespace-quota",
			Pools:            map[string]allocation.ResourcePool{"H100": {Limit: config.QuotaUnlimited}},
			NamespacePools:   map[string]map[string]allocation.ResourcePool{ns: {"H100": {Limit: guaranteed, Used: guaranteed}}},
			NamespaceBorrow:  map[string]map[string]allocation.NamespaceBorrow{ns: {"H100": {Cap: config.QuotaUnlimited, WithinNominal: guaranteed}}},
			NamespaceCohorts: map[string][]string{ns: {"c"}},
			SharedPools: []allocation.SharedPoolChunk{{
				Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Free: 8, ReadAt: readAt,
				FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"},
				Lenders:         []kueue.LenderState{{Queue: "lender", Waiting: lenderWaiting, CanReclaim: true, LentOut: 8}},
			}},
		}}
	}
	request := func() allocation.ModelScalingRequest {
		return allocation.ModelScalingRequest{
			ModelID: "m", Namespace: ns, Priority: 1,
			CompositeSignal: allocation.NamedAnalyzerResult{
				Name: domain.SaturationAnalyzerName,
				Result: &domain.AnalyzerResult{ModelID: "m", Namespace: ns,
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", ReplicaCount: guaranteed, PerReplicaCapacity: 10000}},
				},
				RequiredCapacity: 60000, Remaining: 60000, Live: true,
			},
			Variants:      []domain.VariantMetadata{{VariantName: "v", AcceleratorName: "H100", Cost: 5}},
			VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: guaranteed, GPUsPerReplica: replicaGPUs}},
		}
	}
	// cycle runs stage 2 at now on a Kueue read taken at now.
	cycle := func(e *Engine, lenderWaiting bool, now time.Time) ([]domain.VariantDecision, []allocation.BorrowDraw) {
		requests := []allocation.ModelScalingRequest{request()}
		optimizer := allocation.NewGreedyByScoreOptimizer()
		c := e.prepareBorrowing(ctx, constraints(lenderWaiting, now), requests, nil, now)
		optimizer.Nodes = e.borrowing.nodes
		optimizer.BorrowBlocked = e.borrowBlocked()
		decisions := e.decideV2(ctx, optimizer, requests, c, nil)
		e.recordBorrowDraws(optimizer)
		e.releaseBorrowed(ctx, decisions, nil, now)
		return decisions, optimizer.BorrowDraws()
	}
	targetOf := func(decisions []domain.VariantDecision) int {
		for _, d := range decisions {
			if d.VariantName == "v" {
				return d.TargetReplicas
			}
		}
		return -1
	}

	It("control: with the lender not waiting, the cycle borrows from the pool", func() {
		e := &Engine{Config: config.NewTestConfig()}
		decisions, draws := cycle(e, false, t0)
		Expect(draws).NotTo(BeEmpty())
		Expect(targetOf(decisions)).To(BeNumerically(">", guaranteed))
	})

	It("orders no borrow and no scale-up past guaranteed once the lender counts as waiting", func() {
		e := &Engine{Config: config.NewTestConfig()}
		debounce := e.borrowSettings().LenderDebounce
		// Two fresh reads, the debounce apart: the lender now counts as waiting.
		cycle(e, true, t0)
		inFlightBefore := e.borrowing.inFlight.Entries()
		decisions, draws := cycle(e, true, t0.Add(debounce))

		Expect(e.borrowing.governor.BlockedCohorts(t0.Add(debounce))).To(HaveKey("c"),
			"premise: a waiting reclaiming lender blocks its cohort")
		Expect(draws).To(BeEmpty(), "nothing is borrowed from a blocked cohort")
		Expect(targetOf(decisions)).To(BeNumerically("<=", guaranteed),
			"no decision scales past the guaranteed figure while the lender waits")
		Expect(e.borrowing.inFlight.Entries()).To(Equal(inFlightBefore),
			"the blocked cycle records no new borrowing in flight")
	})
})
