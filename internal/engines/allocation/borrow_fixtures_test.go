package allocation

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

const borrowH100 = "NVIDIA-H100-80GB-HBM3"

// borrowModel is a single-variant H100 model in ns at current replicas of gpr
// GPUs each, whose demand asks for want more replicas. podGPUs is one
// replica's pod layout (nil: one pod).
func borrowModel(ns, id string, gpr, current, want int, podGPUs []int) ModelScalingRequest {
	v := id + "-v"
	return withSatEntry(&satEntryFixture{
		RequiredCapacity: float64(want) * 10000,
		VariantCapacities: []vcFixture{
			{VariantName: v, AcceleratorName: "H100", Cost: 5, ReplicaCount: current, PerReplicaCapacity: 10000},
		},
	}, ModelScalingRequest{
		ModelID: id, Namespace: ns, Priority: 1,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: v, CurrentReplicas: current, GPUsPerReplica: gpr, PodGPUs: podGPUs},
		},
	})
}

// nsBorrow is one namespace of a borrowing quota provider.
type nsBorrow struct {
	ns                        string
	guaranteed, used, cap, wn int
	lentOut                   int
}

// borrowQuota is a namespace-scope quota provider counting borrowing for nss,
// all in cohort "c", which has one h100 chunk of free GPUs.
func borrowQuota(free int, nss ...nsBorrow) *ResourceConstraints {
	c := &ResourceConstraints{
		ProviderName:     "namespace-quota",
		NamespacePools:   map[string]map[string]ResourcePool{},
		NamespaceBorrow:  map[string]map[string]NamespaceBorrow{},
		NamespaceCohorts: map[string][]string{},
		SharedPools: []SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: borrowH100}, Free: free}},
	}
	for _, n := range nss {
		c.NamespacePools[n.ns] = map[string]ResourcePool{"H100": {Limit: n.guaranteed, Used: n.used}}
		c.NamespaceBorrow[n.ns] = map[string]NamespaceBorrow{"H100": {Cap: n.cap, WithinNominal: n.wn, LentOut: n.lentOut}}
		c.NamespaceCohorts[n.ns] = []string{"c"}
	}
	c.Pools = reachAggregate(reachPools(c.NamespacePools, c.NamespaceBorrow))
	return c
}

func physicalH100(n int) *ResourceConstraints {
	return &ResourceConstraints{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"H100": {Limit: n}}, PoolsClosed: true}
}

// addedGPUs is the GPUs the decisions order past each variant's current count.
func addedGPUs(decisions []domain.VariantDecision) map[string]int {
	out := map[string]int{}
	for _, d := range decisions {
		if d.TargetReplicas > d.CurrentReplicas {
			out[d.ModelID] += (d.TargetReplicas - d.CurrentReplicas) * d.GPUsPerReplica
		}
	}
	return out
}
