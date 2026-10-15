package variantmeta

import (
	"reflect"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// podPlacements is the node placement of each distinct pod template of one
// replica, leader first (domain.VariantMetadata.Placements): a LeaderWorkerSet
// whose worker template differs from its leader's carries both. Nil when no
// template constrains its nodes.
func podPlacements(acc scaletarget.ScaleTargetAccessor) []domain.PodPlacement {
	if acc == nil {
		return nil
	}
	var out []domain.PodPlacement
	leader, worker := acc.GetLeaderPodTemplateSpec(), acc.GetWorkerPodTemplateSpec()
	if leader != nil {
		if p := domain.PlacementOf(&leader.Spec); p != nil {
			out = append(out, *p)
		}
	}
	if worker != nil && worker != leader {
		if p := domain.PlacementOf(&worker.Spec); p != nil && !containsPlacement(out, *p) {
			out = append(out, *p)
		}
	}
	return out
}

func containsPlacement(ps []domain.PodPlacement, p domain.PodPlacement) bool {
	for _, q := range ps {
		if reflect.DeepEqual(q, p) {
			return true
		}
	}
	return false
}
