package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// The placement-timeout exemption ("within guaranteed: Kueue reclaims for it")
// rests on the guaranteed figure the reader derives from reclaimWithinCohort.
// Kueue's LowerPriority preempts only lower-priority borrowers, so with serving
// above batch it never reclaims for the serving replica: the figure must be the
// Never one, and a gated replica below nominal is not exempt.
var _ = Describe("BorrowGovernor under each reclaim policy", func() {
	t0 := time.Unix(30000, 0)
	settings := DefaultBorrowSettings()
	key := kueue.ResourceKey{Flavor: "h100", Resource: "nvidia.com/gpu"}

	// Queue a: nominal 2, one replica of 1 GPU admitted; peer b borrows the
	// other GPU of a's nominal. The namespace holds 2 replicas, 1 gated.
	figuresFor := func(policy string) kueue.QueueFigures {
		t := kueue.Topology{Queues: map[string]kueue.QueueState{
			"a": {Name: "a", Cohort: "c", Reclaim: policy, HasStatus: true,
				Resources: map[kueue.ResourceKey]kueue.QueueResource{key: {Nominal: 2, Reserved: 1}}},
			"b": {Name: "b", Cohort: "c", Reclaim: kueue.ReclaimNever, HasStatus: true,
				Resources: map[kueue.ResourceKey]kueue.QueueResource{key: {Nominal: 0, Reserved: 1, Borrowed: 1}}},
		}}
		figs := kueue.ComputeBorrowing(t, false).Queues["a"]
		Expect(figs).To(HaveLen(1))
		return figs[0]
	}
	releases := func(policy string) []BorrowRelease {
		f := figuresFor(policy)
		r := govModel("m", 2, 1)
		r.VariantStates[0].UnplacedReplicas = 1
		r.VariantStates[0].UnplacedSince = t0
		c := BorrowCycle{Now: t0.Add(settings.PlacementTimeout + time.Second),
			Constraints: []*ResourceConstraints{borrowQuota(0, nsBorrow{ns: "b", guaranteed: f.Guaranteed, used: 2,
				cap: config.QuotaUnlimited, wn: f.WithinNominal, lentOut: f.LentOut})},
			Requests: []ModelScalingRequest{r},
			Gated:    map[string]map[string]int{"b": {"H100": 1}}}
		g := NewBorrowGovernor(settings)
		g.Observe(c)
		return g.Releases(c, decisionsOf(c.Requests))
	}

	It("Any: the gated replica within nominal is exempt, Kueue reclaims for it", func() {
		Expect(releases(kueue.ReclaimAny)).To(BeEmpty())
	})

	It("LowerPriority: the gated replica is past guaranteed and is released after the timeout", func() {
		rel := releases(kueue.ReclaimLowerPriority)
		Expect(rel).To(HaveLen(1), "Kueue preempts only lower priorities; it does not reclaim for serving")
		Expect(rel[0].Cause).To(Equal(ReleasePlacementTimeout))
		f := figuresFor(kueue.ReclaimLowerPriority)
		Expect(f.Guaranteed).To(Equal(1))
		Expect(f.LentOut).To(Equal(0))
	})
})
