package allocation

import (
	"maps"
	"slices"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// NodeView is one cycle's per-node picture of free GPUs, consumed in order
// (docs/proposals/kueue-borrowing-and-limiter-composition.md, B5): the greedy
// optimizer places first and debits every replica it orders that fits a node,
// and the utilization-share optimizer's idle fill then reads the same view with
// those debits applied, so the two cannot place into the same hole.
//
// It counts GPUs only. CPU or memory exhaustion on a node is not seen here.
//
// A NodeView is not safe for concurrent use: one optimizer pass owns it. Every
// method accepts a nil receiver, which is "no node information": nothing is
// fitted, every placement succeeds, and the copies are nil.
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
	return &NodeView{nodes: v.Snapshot(), names: slices.Clone(v.names)}
}

// Snapshot returns a copy of the view with every debit applied, in the shape
// the node store hands out.
func (v *NodeView) Snapshot() map[string]decision.NodeGPU {
	if v == nil {
		return nil
	}
	out := make(map[string]decision.NodeGPU, len(v.nodes))
	for name, n := range v.nodes {
		n.Labels = maps.Clone(n.Labels)
		n.Taints = slices.Clone(n.Taints)
		out[name] = n
	}
	return out
}

// place fits pods, given as GPUs per pod, first-fit onto the schedulable nodes
// of accelerator acc, and debits them when commit is set.
//
// A node must satisfy every pod template's own placement (templates): its
// nodeSelector and required node affinity, which the scheduler enforces
// whatever flavor Kueue adds. With a flavor placement, only that flavor's nodes
// count as well: every node label the flavor names, and every taint tolerated
// by the flavor's tolerations or the template's own (the pod carries both). A
// taint keyed by a GPU resource name is not held against a pod: the
// ExtendedResourceToleration admission plugin adds its toleration to every pod
// requesting that resource. Without a flavor placement (a guaranteed replica,
// whose flavor is Kueue's choice and whose tolerations are therefore unknown)
// taints are not judged. Reports whether every pod fit; nothing is debited
// otherwise.
func (v *NodeView) place(acc string, pods []int, templates []domain.PodPlacement, placement *kueue.FlavorPlacement, commit bool) bool {
	if v == nil {
		return true
	}
	free := map[string]int{}
	for _, name := range v.names {
		n := v.nodes[name]
		if v.eligible(n, acc, templates, placement) {
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

// eligible reports whether a pod of accelerator acc, with the given template
// placements and pinned to placement when it is non-nil, may land on n.
func (v *NodeView) eligible(n decision.NodeGPU, acc string, templates []domain.PodPlacement, placement *kueue.FlavorPlacement) bool {
	if n.Unschedulable {
		return false
	}
	if acc != "" && acc != constants.DefaultAcceleratorName && !accelerator.SameName(n.Accelerator, acc) {
		return false
	}
	for i := range templates {
		if !templateAdmits(&templates[i], n.Labels) {
			return false
		}
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
		if !tolerated(t, placement.Tolerations, templates) {
			return false
		}
	}
	return true
}

// tolerated reports whether every pod template, carrying its own tolerations
// and the flavor's, tolerates t.
func tolerated(t *corev1.Taint, flavor []corev1.Toleration, templates []domain.PodPlacement) bool {
	tolerates := func(tol corev1.Toleration) bool { return tol.ToleratesTaint(t) }
	if slices.ContainsFunc(flavor, tolerates) {
		return true
	}
	if len(templates) == 0 {
		return false
	}
	for i := range templates {
		if !slices.ContainsFunc(templates[i].Tolerations, tolerates) {
			return false
		}
	}
	return true
}

// templateAdmits reports whether a node with these labels satisfies a pod
// template's nodeSelector and required node affinity, as the scheduler judges
// them: every nodeSelector pair, and at least one affinity term whose every
// match expression holds. matchFields (which name the node, not its labels)
// are not judged here: a term carrying only them admits the node, so the view
// stays as permissive as it was before templates were read.
func templateAdmits(p *domain.PodPlacement, labels map[string]string) bool {
	for k, val := range p.NodeSelector {
		if got, ok := labels[k]; !ok || got != val {
			return false
		}
	}
	if p.RequiredNodeAffinity == nil || len(p.RequiredNodeAffinity.NodeSelectorTerms) == 0 {
		return true
	}
	for _, term := range p.RequiredNodeAffinity.NodeSelectorTerms {
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue // an empty term matches no node
		}
		if termAdmits(term.MatchExpressions, labels) {
			return true
		}
	}
	return false
}

// termAdmits reports whether every requirement of one node selector term holds
// for labels.
func termAdmits(reqs []corev1.NodeSelectorRequirement, labels map[string]string) bool {
	for _, r := range reqs {
		if !requirementHolds(r, labels) {
			return false
		}
	}
	return true
}

// requirementHolds evaluates one node selector requirement against labels.
func requirementHolds(r corev1.NodeSelectorRequirement, labels map[string]string) bool {
	val, has := labels[r.Key]
	switch r.Operator {
	case corev1.NodeSelectorOpIn:
		return has && slices.Contains(r.Values, val)
	case corev1.NodeSelectorOpNotIn:
		return !has || !slices.Contains(r.Values, val)
	case corev1.NodeSelectorOpExists:
		return has
	case corev1.NodeSelectorOpDoesNotExist:
		return !has
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		if len(r.Values) != 1 || !has {
			return false
		}
		want, err1 := strconv.ParseInt(r.Values[0], 10, 64)
		got, err2 := strconv.ParseInt(val, 10, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if r.Operator == corev1.NodeSelectorOpGt {
			return got > want
		}
		return got < want
	}
	return false
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
// The fallback places a multi-node group (a LeaderWorkerSet whose layout is
// unknown) as one pod, which no 8-GPU node holds; for a replica of more than
// one GPU it is logged once per variant (variant is "namespace/name"), so a
// borrowed group that never places can be traced to it.
func replicaPods(variant string, podGPUs []int, gpusPerReplica int) []int {
	sum := 0
	for _, g := range podGPUs {
		sum += g
	}
	if len(podGPUs) > 0 && sum == gpusPerReplica {
		return podGPUs
	}
	if gpusPerReplica > 1 {
		if _, seen := unknownLayoutLogged.LoadOrStore(variant, true); !seen {
			ctrl.Log.WithName("allocation").V(1).Info(
				"Pod layout of a replica unknown: placing it on nodes as one pod",
				"variant", variant, "gpusPerReplica", gpusPerReplica, "podGPUs", podGPUs)
		}
	}
	return []int{gpusPerReplica}
}

// unknownLayoutLogged is the variants whose unknown pod layout replicaPods
// has logged.
var unknownLayoutLogged sync.Map
