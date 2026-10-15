package allocation

import (
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// NodeView is one cycle's per-node picture of free GPUs, consumed in order
// (docs/proposals/kueue-borrowing-and-limiter-composition.md, B5): the greedy
// optimizer places first and debits every replica it orders that fits a node,
// and the utilization-share optimizer's idle fill then reads the same view with
// those debits applied, so the two cannot place into the same hole.
//
// It counts GPUs only. CPU or memory exhaustion on a node is not seen here.
type NodeView struct {
	nodes map[string]decision.NodeGPU
	names []string
}

// NewNodeView builds a view over a node snapshot (decision.NodeGPUStore.Latest),
// or returns nil when there is none: without node information nothing is fitted.
func NewNodeView(snap map[string]decision.NodeGPU) *NodeView {
	if snap == nil {
		return nil
	}
	v := &NodeView{nodes: make(map[string]decision.NodeGPU, len(snap))}
	for name, n := range snap {
		n.Labels = maps.Clone(n.Labels)
		n.Taints = slices.Clone(n.Taints)
		v.nodes[name] = n
	}
	v.names = slices.Sorted(maps.Keys(v.nodes))
	return v
}

// Clone returns an independent copy, for a pass that must not consume the
// cycle's view (a shadow pass, a tentative placement).
func (v *NodeView) Clone() *NodeView {
	if v == nil {
		return nil
	}
	return NewNodeView(v.nodes)
}

// Snapshot returns the view with every debit applied, in the shape the node
// store hands out.
func (v *NodeView) Snapshot() map[string]decision.NodeGPU {
	if v == nil {
		return nil
	}
	return NewNodeView(v.nodes).nodes
}

// place fits pods, given as GPUs per pod, first-fit onto the schedulable nodes
// of accelerator acc, and debits them when commit is set. With a placement
// only that flavor's nodes count: every node label the flavor names, and every
// taint tolerated by its tolerations. A taint keyed by a GPU resource name is
// not held against a flavor: the ExtendedResourceToleration admission plugin
// adds its toleration to every pod requesting that resource. Without a placement
// (a guaranteed replica, whose flavor is Kueue's choice) any node of the
// accelerator counts. Reports whether every pod fit; nothing is debited
// otherwise.
func (v *NodeView) place(acc string, pods []int, placement *kueue.FlavorPlacement, commit bool) bool {
	if v == nil {
		return true
	}
	free := map[string]int{}
	for _, name := range v.names {
		n := v.nodes[name]
		if v.eligible(n, acc, placement) {
			free[name] = n.Free()
		}
	}
	taken := map[string]int{}
	for _, need := range pods {
		if need <= 0 {
			continue
		}
		placed := false
		for _, name := range v.names {
			if f, ok := free[name]; ok && f >= need {
				free[name] -= need
				taken[name] += need
				placed = true
				break
			}
		}
		if !placed {
			return false
		}
	}
	if commit {
		for name, g := range taken {
			n := v.nodes[name]
			n.Used += g
			v.nodes[name] = n
		}
	}
	return true
}

// eligible reports whether a pod of accelerator acc, pinned to placement when
// it is non-nil, may land on n.
func (v *NodeView) eligible(n decision.NodeGPU, acc string, placement *kueue.FlavorPlacement) bool {
	if n.Unschedulable {
		return false
	}
	if acc != "" && acc != constants.DefaultAcceleratorName && !accelerator.SameName(n.Accelerator, acc) {
		return false
	}
	if placement == nil {
		return true
	}
	for k, val := range placement.NodeLabels {
		if n.Labels[k] != val {
			return false
		}
	}
	for i := range n.Taints {
		t := &n.Taints[i]
		if isGPUResourceTaint(t) {
			continue
		}
		if !slices.ContainsFunc(placement.Tolerations, func(tol corev1.Toleration) bool { return tol.ToleratesTaint(t) }) {
			return false
		}
	}
	return true
}

// isGPUResourceTaint reports whether a taint is keyed by a GPU resource name.
func isGPUResourceTaint(t *corev1.Taint) bool {
	for _, v := range constants.VendorResources {
		if t.Key == v.ResourceName {
			return true
		}
	}
	return false
}

// replicaPods is one replica's pods as GPUs per pod: the variant's measured
// layout when it sums to the replica, else one pod holding the whole replica.
func replicaPods(podGPUs []int, gpusPerReplica int) []int {
	sum := 0
	for _, g := range podGPUs {
		sum += g
	}
	if len(podGPUs) > 0 && sum == gpusPerReplica {
		return podGPUs
	}
	return []int{gpusPerReplica}
}
