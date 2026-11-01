package allocation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// pdWholeNodeRequest is pdRescaleRequest on 8-GPU pods: pods replicas per
// role, prefill demand prefill and decode demand decode at 100 per replica.
func pdWholeNodeRequest(pods int, prefill, decode float64) ModelScalingRequest {
	req := pdRescaleRequest(nil)
	req.CompositeSignal.Result.RoleDemand = map[string]float64{domain.RolePrefill: prefill, domain.RoleDecode: decode}
	supply := float64(pods) * 100
	req.CompositeSignal.RoleCapacities = map[string]domain.RoleCapacity{
		domain.RolePrefill: {Role: domain.RolePrefill, TotalDemand: prefill, TotalSupply: supply},
		domain.RoleDecode:  {Role: domain.RoleDecode, TotalDemand: decode, TotalSupply: supply},
	}
	for i := range req.CompositeSignal.Result.VariantCapacities {
		req.CompositeSignal.Result.VariantCapacities[i].ReplicaCount = pods
	}
	for i := range req.VariantStates {
		req.VariantStates[i].GPUsPerReplica, req.VariantStates[i].CurrentReplicas = 8, pods
	}
	return req
}

// TestRescaleCapsARoleAtItsMaxReplicas: a role at its maxReplicas is not
// given a share it cannot fill. Prefill is at its max of two 8-GPU pods with
// demand for 70, decode at two with demand for 10. A model target of 40 split
// by demand was 35 for prefill and 5 for decode, so decode was cut from 2 pods
// to 1 while prefill could not grow and all 64 free GPUs stayed free. Capped
// at its 16, prefill leaves decode 24: three pods.
func TestRescaleCapsARoleAtItsMaxReplicas(t *testing.T) {
	two := 2
	req := pdWholeNodeRequest(2, 7000, 1000)
	req.VariantStates[0].MaxReplicas = &two
	got := rescaleTargets(req, 40, 64)
	assert.Equal(t, 2, got["p"], "prefill is at its max")
	assert.Equal(t, 3, got["d"], "decode takes what prefill cannot hold")
}

// TestRescaleReclaimsWholePods: a model's cut is shed as whole pods. On 8-GPU
// pods, two per role, a target of 24 from 32 split equally is a cut of 4 from
// each role; each cut alone is less than a pod, so nothing was shed and the
// model kept 32. Summed, the cut is one pod.
func TestRescaleReclaimsWholePods(t *testing.T) {
	got := rescaleTargets(pdWholeNodeRequest(2, 100, 100), 24, 0)
	assert.Equal(t, 3, got["p"]+got["d"], "one pod of the 8-GPU cut is shed")
	assert.GreaterOrEqual(t, got["p"], 1)
	assert.GreaterOrEqual(t, got["d"], 1)
}

// TestRescaleReclaimsWholePodsFromBothRoles: on 8-GPU pods, four per role, a
// target of 32 from 64 weighed 1:2 by demand is 11 for prefill and 21 for
// decode, a cut of 21 and 11. Each cut rounded down alone shed 2 prefill pods
// and 1 decode pod and the model kept 40; the 5 and 3 left over make one more
// pod, taken from prefill, whose cut is the larger.
func TestRescaleReclaimsWholePodsFromBothRoles(t *testing.T) {
	got := rescaleTargets(pdWholeNodeRequest(4, 100, 200), 32, 0)
	assert.Equal(t, 1, got["p"])
	assert.Equal(t, 3, got["d"])
}

// TestRescaleReclaimsNoPodBelowAFloor: the rounded cut never takes a role
// below its floor. Prefill, with no demand, would be cut furthest, but its
// floor is its two 8-GPU pods -- minReplicas 2, or a scale-down hold -- so at
// a target of 24 the one pod the cut buys comes from decode. The split
// reserves the floor and reclaimRole keeps minReplicas, so this pins that the
// summed cut is taken from the role that can give it.
func TestRescaleReclaimsNoPodBelowAFloor(t *testing.T) {
	two := 2
	req := pdWholeNodeRequest(2, 0, 100)
	req.VariantStates[0].MinReplicas = &two
	got := rescaleTargets(req, 24, 0)
	assert.Equal(t, 2, got["p"], "prefill's minReplicas stops its pod")
	assert.Equal(t, 1, got["d"], "the pod comes from decode")

	req = pdWholeNodeRequest(2, 0, 100)
	req.CompositeSignal.Holds = map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}}
	got = rescaleTargets(req, 24, 0)
	assert.Equal(t, 2, got["p"], "a role that may not shrink keeps its pods")
	assert.Equal(t, 1, got["d"], "the pod comes from decode")
}

// TestRescaleSecondPassKeepsARolesLastPod: the summed second pass never takes
// a role's last replica. Prefill holds three 8-GPU pods, decode one, on the
// first of two equal decode variants, where the cheapest-at-one rule does not
// protect it; demand weighs a target of 16 at 12 and 4. Decode, with demand,
// went to zero pods.
func TestRescaleSecondPassKeepsARolesLastPod(t *testing.T) {
	req := pdWholeNodeRequest(3, 300, 100)
	req.CompositeSignal.Result.VariantCapacities[1].ReplicaCount = 1
	req.VariantStates[1].CurrentReplicas = 1
	req.CompositeSignal.Result.VariantCapacities = append(req.CompositeSignal.Result.VariantCapacities,
		domain.VariantCapacity{VariantName: "d-b", Role: domain.RoleDecode, PerReplicaCapacity: 100})
	req.VariantStates = append(req.VariantStates,
		domain.VariantReplicaState{VariantName: "d-b", Role: domain.RoleDecode, GPUsPerReplica: 8})
	req.Variants = append(req.Variants,
		domain.VariantMetadata{VariantName: "d-b", Role: domain.RoleDecode, AcceleratorName: "A100", Cost: 1})
	got := rescaleTargets(req, 16, 0)
	assert.Equal(t, 1, got["d"]+got["d-b"], "decode keeps its one pod: %v", got)
	assert.Equal(t, 2, got["p"], "prefill gives the pod its own cut buys: %v", got)
}

// TestRescaleCountsOnlyACutARoleCanGive: a cut no replica can meet does not
// count toward the summed cut. Decode's variant has no capacity reading, so
// none of its 32 GPUs can be shed; counted anyway, prefill shed it for decode
// and went from 4 pods to 2 at a target of 20, and to 3 at 28.
func TestRescaleCountsOnlyACutARoleCanGive(t *testing.T) {
	req := pdWholeNodeRequest(4, 300, 100)
	req.CompositeSignal.Result.VariantCapacities[1].PerReplicaCapacity = 0
	assert.Equal(t, 3, rescaleTargets(req, 20, 0)["p"], "prefill sheds its own cut, rounded down")
	assert.Equal(t, 4, rescaleTargets(req, 28, 0)["p"])
	assert.Equal(t, 4, rescaleTargets(req, 20, 0)["d"], "decode cannot shed")
}

// TestRescaleMixedPodSizesStayMonotone: with more than one replica size the
// cut is shed by an exact pod search, not a summed pass, which made a larger
// target leave fewer GPUs. Prefill holds one 8-GPU and one 4-GPU pod, decode
// two 8-GPU pods: summed, a target of 18 left 20 GPUs where 17 left 24.
func TestRescaleMixedPodSizesStayMonotone(t *testing.T) {
	req := pdWholeNodeRequest(2, 100, 100)
	req.CompositeSignal.Result.VariantCapacities[0].ReplicaCount = 1
	req.CompositeSignal.Result.VariantCapacities[0].PerReplicaCapacity = 200
	req.CompositeSignal.Result.VariantCapacities[1].PerReplicaCapacity = 200
	req.VariantStates[0].CurrentReplicas = 1
	req.Variants[0].Cost, req.Variants[1].Cost = 8, 8
	req.CompositeSignal.Result.VariantCapacities = append(req.CompositeSignal.Result.VariantCapacities,
		domain.VariantCapacity{VariantName: "p4", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: 100})
	req.VariantStates = append(req.VariantStates,
		domain.VariantReplicaState{VariantName: "p4", Role: domain.RolePrefill, CurrentReplicas: 1, GPUsPerReplica: 4})
	req.Variants = append(req.Variants,
		domain.VariantMetadata{VariantName: "p4", Role: domain.RolePrefill, AcceleratorName: "A100", Cost: 4})
	gpus := func(target int) int {
		got := rescaleTargets(req, target, 0)
		return 8*got["p"] + 4*got["p4"] + 8*got["d"]
	}
	for target := 16; target < 28; target++ {
		assert.LessOrEqual(t, gpus(target), gpus(target+1), "target %d gives more GPUs than %d", target, target+1)
	}
}
