package scaletarget

import (
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
)

// ScaleTargetAccessor provides a uniform interface to extract scaling-relevant
// information from any supported scale target kind (Deployment, LeaderWorkerSet).
type ScaleTargetAccessor interface {
	// GetName returns the name of the scale target.
	GetName() string

	// GetNamespace returns the namespace of the scale target.
	GetNamespace() string

	// GetReplicas returns current spec replicas.
	GetReplicas() *int32
	GetDeletionTimestamp() *v1.Time
	// GetUID identifies this incarnation of the scale target: a target deleted
	// and re-created under the same name is a different fleet.
	GetUID() types.UID

	// GetStatusReplicas returns status replicas (actual running).
	GetStatusReplicas() int32
	GetStatusReadyReplicas() int32

	// GetTotalGPUsPerReplica returns total GPU count across all pods in a replica.
	// For Deployment: GPUs from the single pod template.
	// For LWS: leader_GPUs + (Size - 1) * worker_GPUs.
	GetTotalGPUsPerReplica() int

	// GetLeaderPodTemplateSpec returns the pod template for the leader/primary pod.
	// For Deployment: the single pod template.
	// For LWS: the leader template (falls back to worker template if not set).
	// Use this for: engine args extraction (leader starts the API server),
	// metrics port discovery, pod label matching.
	GetLeaderPodTemplateSpec() *corev1.PodTemplateSpec

	// GetWorkerPodTemplateSpec returns the pod template for worker pods.
	// For Deployment: same as GetLeaderPodTemplateSpec() (single template).
	// For LWS: the worker template.
	// Use this for: GPU resource extraction when workers differ from leader.
	GetWorkerPodTemplateSpec() *corev1.PodTemplateSpec

	// GetGroupSize returns the number of pods per replica.
	// For Deployment: always 1.
	// For LWS: spec.leaderWorkerTemplate.size (1 leader + N-1 workers).
	GetGroupSize() int32
}

// PodGPUs returns the GPUs each pod of one replica requests, leader first: one
// entry for a Deployment, GetGroupSize entries for a LeaderWorkerSet, whose
// leader runs the worker template when it has none of its own. It returns nil
// when no pod requests a GPU explicitly: the replica's default of one GPU says
// nothing about how it is split across pods.
func PodGPUs(acc ScaleTargetAccessor) []int {
	if acc == nil {
		return nil
	}
	gpus := func(t *corev1.PodTemplateSpec) int {
		if t == nil {
			return 0
		}
		return resources.GetContainersGPUs(t.Spec.Containers)
	}
	size := max(int(acc.GetGroupSize()), 1)
	out := make([]int, 0, size)
	out = append(out, gpus(acc.GetLeaderPodTemplateSpec()))
	worker := gpus(acc.GetWorkerPodTemplateSpec())
	for range size - 1 {
		out = append(out, worker)
	}
	for _, g := range out {
		if g > 0 {
			return out
		}
	}
	return nil
}
