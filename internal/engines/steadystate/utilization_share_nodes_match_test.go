package steadystate

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// Review minor (whole-node): the share's node picture matched the
// accelerator by exact name and ignored taints, where the node view matches
// by name or family (accelerator.SameName) and refuses a taint the pod does
// not tolerate. A group keyed by its family saw none of its products'
// nodes, and a receiver was planned onto a node a taint keeps it off.
func TestShareFreeNodesCountsAsTheNodeViewDoes(t *testing.T) {
	taint := corev1.Taint{Key: "pool", Value: "batch", Effect: corev1.TaintEffectNoSchedule}
	snap := map[string]decision.NodeGPU{
		"hbm3":    {Accelerator: "NVIDIA-H100-80GB-HBM3", Capacity: 8},
		"tainted": {Accelerator: "NVIDIA-H100-80GB-HBM3", Capacity: 8, Taints: []corev1.Taint{taint}},
		"a100":    {Accelerator: "NVIDIA-A100-SXM4-80GB", Capacity: 8},
	}
	got := shareFreeNodesAfter(snap, "H100", allocation.ShareHoles{}, nil)
	if _, ok := got["hbm3"]; !ok || len(got) != 1 {
		t.Fatalf("family H100 with no toleration: got %v, want the untainted HBM3 node alone", got)
	}
	tolerates := []corev1.Toleration{{Key: "pool", Operator: corev1.TolerationOpEqual, Value: "batch", Effect: corev1.TaintEffectNoSchedule}}
	if got := shareFreeNodesAfter(snap, "H100", allocation.ShareHoles{}, nil, tolerates); len(got) != 2 {
		t.Fatalf("a receiver tolerating the taint: got %v, want both H100 nodes", got)
	}
	if got := shareFreeNodesAfter(snap, "H100", allocation.ShareHoles{}, nil, tolerates, nil); len(got) != 1 {
		t.Fatalf("a second receiver tolerating nothing: got %v, want the untainted node alone", got)
	}
}
