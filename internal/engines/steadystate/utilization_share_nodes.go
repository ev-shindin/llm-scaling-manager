package steadystate

// The node picture: what node-aware planning and the idle fill place into,
// and how a receiver left Pending is attributed (proposal section 6.5).

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/variantmeta"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// shareNodeInputs is the node-aware planning input for a group (section 6.5):
// each node of the group's accelerator with its free GPUs and labels, from the
// usage refresher's last snapshot; per donor role, the replicas it could give
// and where their pods run; per receiver role, the exclusive-topology label its
// holes must share. All nil when no fresh node snapshot exists, and the
// planner then never combines pods on a node.
//
// A Deployment donor offers each Ready, unmarked pod as a replica, and offers
// nothing when any of its pods is not Ready: the ReplicaSet removes a not-Ready
// pod first, wherever it runs, so the planned hole would not open. A
// LeaderWorkerSet donor offers its highest-index group, the one LWS removes.
func (e *Engine) shareNodeInputs(ctx context.Context, logger logr.Logger, g allocation.ShareGroup,
	accessor func(role, variant string) scaletarget.ScaleTargetAccessor, reserved int,
	marked func(*corev1.Pod) bool, now time.Time) (
	map[string]allocation.ShareNode, map[string][]allocation.ShareUnit, map[string]string) {
	// Domains are known without node information, and a receiver with one
	// is then never funded by the node-blind search.
	domains := map[string]string{}
	for role, grow := range g.Grow {
		if acc := accessor(role, grow.Name); acc != nil {
			if key := scaletarget.ExclusiveTopology(acc); key != "" {
				domains[role] = key
			}
		}
	}
	snap := e.shareNodes(now)
	if snap == nil || e.client == nil {
		return nil, nil, domains
	}
	nodes := shareFreeNodes(snap, g.AcceleratorType, reserved)
	if len(nodes) == 0 {
		return nil, nil, domains
	}
	units := map[string][]allocation.ShareUnit{}
	for role, give := range g.Give {
		acc := accessor(role, give.Name)
		if acc == nil {
			continue
		}
		pods, err := e.donorPods(ctx, acc, g.Origins[role].Namespace)
		if err != nil {
			// The donor then gives nothing to a node-aware set this cycle.
			logger.V(logging.DEBUG).Info("Utilization share: could not list a donor's pods for node-aware planning",
				"role", role, "error", err.Error())
			continue
		}
		if u := shareDonorUnits(scaletarget.IsLeaderWorkerSet(acc), pods, marked); len(u) > 0 {
			units[role] = u
		}
	}
	return nodes, units, domains
}

// shareNodes is the fresh per-node GPU picture, or nil.
func (e *Engine) shareNodes(now time.Time) map[string]decision.NodeGPU {
	store := e.utilizationShare.nodes
	if store == nil {
		store = decision.DefaultNodeGPUs
	}
	return store.Latest(decision.NodeGPUsMaxAge, now)
}

// shareFillTimeoutCause attributes a transfer whose receiver stayed Pending
// after its donors released (section 6.3), from the receiver's Pending pods'
// GPU requests and the node picture of its accelerator: release-taken when
// fewer GPUs are free than those pods request -- something else took them;
// release-shape-mismatch when enough are free in total but the pods do not fit
// the nodes, placed largest first into the smallest hole that holds each. ""
// when they fit (a pod is Pending for another reason: a taint, an affinity),
// when none is Pending, or without node information.
func shareFillTimeoutCause(accelerator string, pending []int, nodes map[string]decision.NodeGPU) string {
	if nodes == nil || len(pending) == 0 {
		return ""
	}
	free := map[string]int{}
	total := 0
	for name, n := range nodes {
		if n.Accelerator == accelerator && !n.Unschedulable {
			free[name] = n.Free()
			total += n.Free()
		}
	}
	need := slices.Sorted(slices.Values(pending))
	slices.Reverse(need)
	sum, fits := 0, true
	for _, p := range need {
		sum += p
		best := ""
		for name, f := range free {
			if f >= p && (best == "" || f < free[best] || (f == free[best] && name < best)) {
				best = name
			}
		}
		if best == "" {
			fits = false
			continue
		}
		free[best] -= p
	}
	switch {
	case fits:
		return ""
	case total >= sum:
		return constants.ScalingBlockedReleaseShapeMismatch
	default:
		return constants.ScalingBlockedReleaseTaken
	}
}

// pendingPodGPUs is the GPU request of each of a variant's pods that is
// waiting for a node. Nil when they cannot be listed.
func (e *Engine) pendingPodGPUs(ctx context.Context, acc scaletarget.ScaleTargetAccessor, namespace string) []int {
	if e.client == nil || acc == nil {
		return nil
	}
	pods, err := variantmeta.ListVariantPods(ctx, e.client, namespace, acc)
	if err != nil {
		return nil
	}
	var out []int
	for _, p := range pods {
		if p.Spec.NodeName == "" && p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodPending {
			out = append(out, resources.PodGPURequests(&p))
		}
	}
	return out
}

// shareDonorUnits is what a donor could give a node-aware set, from its pods
// (donorPods: running, scheduled, and for a LeaderWorkerSet its highest-index
// group only). Pods already marked by a transfer are another transfer's. A
// LeaderWorkerSet gives that group, all its pods at once, as one unit. A
// Deployment gives each pod as a unit, and nothing while any pod is not
// Ready: the ReplicaSet removes a not-Ready pod first, wherever it runs, so
// the planned hole would not open.
func shareDonorUnits(lws bool, pods []corev1.Pod, marked func(*corev1.Pod) bool) []allocation.ShareUnit {
	if !lws && slices.ContainsFunc(pods, func(p corev1.Pod) bool { return !variantmeta.PodReady(&p) }) {
		return nil
	}
	pods = slices.DeleteFunc(slices.Clone(pods), func(p corev1.Pod) bool { return marked(&p) })
	if len(pods) == 0 {
		return nil
	}
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return cmp.Compare(a.Name, b.Name) })
	toPod := func(p corev1.Pod) allocation.SharePod {
		return allocation.SharePod{Name: utils.GetNamespacedKey(p.Namespace, p.Name), Node: p.Spec.NodeName,
			GPUs: resources.PodGPURequests(&p)}
	}
	if lws {
		u := allocation.ShareUnit{}
		for _, p := range pods {
			u.Pods = append(u.Pods, toPod(p))
		}
		return []allocation.ShareUnit{u}
	}
	out := make([]allocation.ShareUnit, 0, len(pods))
	for _, p := range pods {
		out = append(out, allocation.ShareUnit{Pods: []allocation.SharePod{toPod(p)}})
	}
	return out
}

// shareFreeNodes is the node picture a node-aware plan may use: the schedulable
// nodes of the accelerator, each with its free GPUs. reserved GPUs -- released
// to a receiver whose pods are still Pending, or held for a wake -- show as
// free on some node, because a Pending pod has none; where is not known, so
// they are taken from the nodes with the most free first, which is where they
// would count most.
func shareFreeNodes(snap map[string]decision.NodeGPU, accelerator string, reserved int) map[string]allocation.ShareNode {
	nodes := map[string]allocation.ShareNode{}
	for name, n := range snap {
		if n.Accelerator == accelerator && !n.Unschedulable {
			nodes[name] = allocation.ShareNode{Free: n.Free(), Labels: n.Labels}
		}
	}
	for reserved > 0 {
		most := ""
		for name, n := range nodes {
			if n.Free > 0 && (most == "" || n.Free > nodes[most].Free || (n.Free == nodes[most].Free && name < most)) {
				most = name
			}
		}
		if most == "" {
			break
		}
		n := nodes[most]
		n.Free--
		nodes[most] = n
		reserved--
	}
	return nodes
}

// shareFit is the node picture the idle fill places into: the nodes' free
// GPUs, nil without node information, and each receiver's topology domain.
type shareFit struct {
	nodes   map[string]allocation.ShareNode
	domains map[string]string
}
