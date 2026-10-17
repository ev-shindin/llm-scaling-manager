package saturation

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// holdHotRanks keeps the capacity of every replica with a full engine rank
// from being released, and returns the demand it added: in all, and per role.
//
// A replica running several data-parallel ranks is priced on what the pod
// holds -- its ranks' tokens summed, their utilization averaged -- so one rank
// at its ceiling among idle ones reads as a pod with room to spare. As
// demand that is right: the idle ranks can take the next requests. As grounds
// for a release it is not. The full rank is an engine that cannot allocate,
// and a scale-down may remove any pod, this one included; the requests it
// holds then land on what is left. So a replica whose busiest rank is full
// (kvFull) counts, for the release, as at the scale-down boundary: the
// variant's demand is raised to what the other replicas hold plus scaleDown
// times its per-replica capacity for each such replica. The spare that is
// left is the other replicas' spare, which is what the fleet can give back
// without counting on the full rank's pod.
//
// Only ever a raise, and never an order: the role's demand, and the model's,
// stay at or below scaleUp times what the role will have, where the engine's
// required capacity is zero. A demand already at or above the raise is left
// alone, so a hold that ran before this one is not added to.
//
// Only replicas of more than one rank are judged. With one rank the pod's
// demand is the rank's own, a full engine reads as a full replica, and
// nothing is hidden by a mean. Prefill is left out, as busiestQueueLoadByVariant
// leaves it out: its KV is held for decode's pulls, and holdPrefillDemand
// covers decode's back-pressure. A Pod that is not Ready is not routed to, and
// a warm-pool bridge is not the variant's to release.
//
// variants and roleDemand (nil on an aggregated model) are updated in place;
// total is the model's demand before the hold.
func holdHotRanks(metrics []domain.ReplicaMetrics, rolesByVariant map[string]string,
	caps []capacity.ReplicaCapacity, variants []domain.VariantCapacity, roleDemand map[string]float64,
	total, scaleUp, scaleDown, kvCacheThreshold float64,
) (moved float64, byRole map[string]float64) {
	if !(scaleDown > 0) || !(scaleUp > 0) {
		return 0, nil
	}
	type podKey struct{ variant, pod string }
	hot := map[podKey]bool{}
	for _, rm := range metrics {
		role, known := rolesByVariant[rm.VariantName]
		if !known || !rm.Ready || rm.FromWarmPool || rm.Ranks() <= 1 ||
			domain.CanonicalRole(role) == domain.RolePrefill || !kvFull(rm, kvCacheThreshold) {
			continue
		}
		hot[podKey{rm.VariantName, rm.PodName}] = true
	}
	if len(hot) == 0 {
		return 0, nil
	}
	hotCount := map[string]int{}
	restDemand := map[string]float64{}
	for _, rc := range caps {
		if rc.FromWarmPool {
			continue
		}
		if hot[podKey{rc.VariantName, rc.PodName}] {
			hotCount[rc.VariantName]++
			continue
		}
		restDemand[rc.VariantName] += float64(rc.ReplicaDemand)
	}

	// The room under the scale-up threshold, per role and for the model.
	supply := aggregation.AggregateByRole(variants)
	room := map[string]float64{}
	var modelRoom float64
	for role, t := range supply {
		demand := t.TotalDemand
		if roleDemand != nil {
			demand = roleDemand[role]
		}
		room[role] = max(0, scaleUp*t.TotalAnticipatedSupply-demand)
		modelRoom += scaleUp * t.TotalAnticipatedSupply
	}
	modelRoom = max(0, modelRoom-total)

	byRole = map[string]float64{}
	for i := range variants {
		vc := &variants[i]
		n := hotCount[vc.VariantName]
		if n == 0 || !(vc.PerReplicaCapacity > 0) {
			continue
		}
		role := domain.RoleOfVC(*vc)
		target := restDemand[vc.VariantName] + scaleDown*vc.PerReplicaCapacity*float64(n)
		add := min(target-vc.TotalDemand, room[role], modelRoom)
		if !(add > 0) {
			continue
		}
		vc.TotalDemand += add
		if s := float64(vc.ReplicaCount) * vc.PerReplicaCapacity; s > 0 {
			vc.Utilization = vc.TotalDemand / s
		}
		room[role] -= add
		modelRoom -= add
		byRole[role] += add
		moved += add
	}
	if moved == 0 {
		return 0, nil
	}
	if roleDemand != nil {
		for role, add := range byRole {
			roleDemand[role] += add
		}
	}
	return moved, byRole
}
