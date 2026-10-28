package steadystate

// The node picture: what node-aware planning and the idle fill place into,
// and how a receiver left Pending is attributed (proposal section 6.5).

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
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
// holes must share and its pod templates' placement. The nodes, and the units,
// are nil when no fresh node snapshot exists, and the planner then never
// combines pods on a node. A node a LeaderWorkerSet group's pod is on, or
// in a domain this cycle's pass or the in-flight ledger gave a group, is
// marked so a new group lands in no such domain (allocation.ShareNode).
//
// A Deployment donor offers each Ready, unmarked pod as a replica, and offers
// nothing when any of its pods is not Ready: the ReplicaSet removes a not-Ready
// pod first, wherever it runs, so the planned hole would not open. A
// LeaderWorkerSet donor offers the group LWS removes next (donorPods).
func (e *Engine) shareNodeInputs(ctx context.Context, logger logr.Logger, g allocation.ShareGroup,
	accessor func(role, variant string) scaletarget.ScaleTargetAccessor, reserved allocation.ShareHoles,
	marked func(*corev1.Pod) bool, now time.Time) (
	map[string]allocation.ShareNode, map[string][]allocation.ShareUnit, map[string]string, map[string][]domain.PodPlacement) {
	// Domains are known without node information, and a receiver with one
	// is then never funded by the node-blind search. Each receiver's pod
	// templates are judged per pod, leader and workers apart, as the node
	// view judges them.
	domains := map[string]string{}
	placements := map[string][]domain.PodPlacement{}
	for role, grow := range g.Grow {
		if acc := accessor(role, grow.Name); acc != nil {
			if key := scaletarget.ExclusiveTopology(acc); key != "" {
				domains[role] = key
			}
			if p := variantmeta.PodPlacements(acc); len(p) > 0 {
				placements[role] = p
			}
		}
	}
	snap := e.shareNodes(now)
	if snap == nil || e.client == nil {
		return nil, nil, domains, placements
	}
	// Net of what today's optimizer placed this cycle for the models the
	// share does not plan, and of the in-flight ledger's holes: those nodes
	// are about to be taken. The share's own promises are held below
	// (reserved), as they always were.
	st := &e.utilizationShare
	// A node counts only when every receiver's pods may land on it: the
	// node view's rule, tolerations included.
	var tolerations [][]corev1.Toleration
	for _, role := range slices.Sorted(maps.Keys(g.Grow)) {
		if acc := accessor(role, g.Grow[role].Name); acc != nil {
			tolerations = append(tolerations, scaletarget.PodTolerations(acc))
		}
	}
	nodes := shareFreeNodesAfter(snap, g.AcceleratorType, reserved, st.left.NodeDebits(st.planned), tolerations...)
	if len(nodes) == 0 {
		return nil, nil, domains, placements
	}
	if debug := logger.V(logging.DEBUG); debug.Enabled() {
		free := make(map[string]int, len(nodes))
		for name, n := range nodes {
			free[name] = n.Free
		}
		// Why a receiver was or was not placed: the free GPUs the plan saw,
		// and where the in-flight promises were taken from.
		debug.Info("Utilization share: node picture", "accelerator", g.AcceleratorType, "free", free,
			"promisedOnNodes", reserved.Nodes, "promisedUnplaced", reserved.Unplaced)
	}
	// A domain where the pass placed, or the in-flight ledger holds, an
	// exclusive-topology group holds no other group: the node view's rule.
	allocation.ClaimShareDomains(nodes, st.left.TakenDomains(st.planned))
	units := map[string][]allocation.ShareUnit{}
	for role, give := range g.Give {
		acc := accessor(role, give.Name)
		if acc == nil {
			continue
		}
		pods, err := e.donorPods(ctx, acc, g.Origins[role].Namespace, marked)
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
	return nodes, units, domains, placements
}

// shareNodes is the fresh per-node GPU picture, or nil.
func (e *Engine) shareNodes(now time.Time) map[string]decision.NodeGPU {
	store := e.utilizationShare.nodes
	if store == nil {
		store = decision.DefaultNodeGPUs
	}
	return store.Latest(decision.NodeGPUsMaxAge, now)
}

// shareUnfunded is the plan's unfunded receivers, with every short role no
// node can hold a replica of marked allocation.ShareUnfundedNoNodeFits
// (shareNoNodeFits). It is the plan's own map, unchanged, without node
// information, when no role is short, or when a giving role's pods cannot be
// listed: a pod left out would read as a node that cannot be emptied.
func (e *Engine) shareUnfunded(ctx context.Context, logger logr.Logger, l *allocation.ShareLedger, g allocation.ShareGroup,
	ev allocation.ShareEvaluation, unfunded map[string]string, accessor func(role, variant string) scaletarget.ScaleTargetAccessor,
	nodes map[string]allocation.ShareNode, domains map[string]string, placements map[string][]domain.PodPlacement) map[string]string {
	if nodes == nil || !slices.ContainsFunc(ev.Roles, func(v allocation.ShareRoleVerdict) bool { return v.Headroom < 0 }) {
		return unfunded
	}
	pods, err := e.shareGroupPodGPUs(ctx, g, accessor)
	if err != nil {
		logger.V(logging.DEBUG).Info("Utilization share: could not list the group's pods; "+
			"whether a node can hold a short model's replica is not judged this cycle", "error", err.Error())
		return unfunded
	}
	return markNoNodeFits(unfunded, shareNoNodeFits(l, g, ev, nodes, pods, domains, placements))
}

// markNoNodeFits is unfunded with each of roles marked
// allocation.ShareUnfundedNoNodeFits, over any reason the plan gave it: no
// donor and no return can place the role, so neither is what it waits for.
// unfunded itself is not changed.
func markNoNodeFits(unfunded map[string]string, roles []string) map[string]string {
	if len(roles) == 0 {
		return unfunded
	}
	out := maps.Clone(unfunded)
	if out == nil {
		out = map[string]string{}
	}
	for _, r := range roles {
		out[r] = allocation.ShareUnfundedNoNodeFits
	}
	return out
}

// shareGroupPodGPUs is, per giving role, the GPUs its pods hold on each node:
// every pod of the variant it gives that is bound to a node and not finished.
// Marked and terminating pods count -- they hold their GPUs until they go,
// and the node's free GPUs grow by what each one held when it does.
func (e *Engine) shareGroupPodGPUs(ctx context.Context, g allocation.ShareGroup,
	accessor func(role, variant string) scaletarget.ScaleTargetAccessor) (map[string]map[string]int, error) {
	out := map[string]map[string]int{}
	for _, role := range slices.Sorted(maps.Keys(g.Give)) {
		give := g.Give[role]
		acc := accessor(role, give.Name)
		if acc == nil {
			return nil, fmt.Errorf("role %s: no scale target for variant %s", role, give.Name)
		}
		list, err := variantmeta.ListVariantPods(ctx, e.client, g.Origins[role].Namespace, acc)
		if err != nil {
			return nil, fmt.Errorf("role %s: %w", role, err)
		}
		byNode := map[string]int{}
		for i := range list {
			p := &list[i]
			if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			byNode[p.Spec.NodeName] += resources.PodGPURequests(p)
		}
		out[role] = byNode
	}
	return out, nil
}

// shareNoNodeFits lists the roles that are short (below their need), are
// receiving no transfer, and of which no node can hold one replica even if
// every pod of the group's other giving roles on it went
// (allocation.ShareNoNodeFits). pods is each giving role's GPUs by node
// (shareGroupPodGPUs). Such a role cannot be funded by a transfer, a donor set,
// returns or the idle fill, whatever the other roles hold: the cause, and so
// its reason, stays the same while the others give or keep their spare.
func shareNoNodeFits(l *allocation.ShareLedger, g allocation.ShareGroup, ev allocation.ShareEvaluation,
	nodes map[string]allocation.ShareNode, pods map[string]map[string]int, domains map[string]string,
	placements map[string][]domain.PodPlacement) []string {
	receiving := map[string]bool{}
	for _, t := range l.Transfers() {
		if t.Receiver != "" {
			receiving[t.Receiver] = true
		}
	}
	var out []string
	for _, v := range ev.Roles {
		grow, ok := g.Grow[v.Key]
		if v.Headroom >= 0 || !ok || len(grow.PodGPUs) == 0 || receiving[v.Key] {
			continue
		}
		others := map[string]int{}
		for role, byNode := range pods {
			if role == v.Key {
				continue
			}
			for n, gpus := range byNode {
				others[n] += gpus
			}
		}
		fit := allocation.ShareReplicaFit{PodGPUs: grow.PodGPUs, DomainKey: domains[v.Key], Placements: placements[v.Key]}
		if allocation.ShareNoNodeFits(nodes, others, fit) {
			out = append(out, v.Key)
		}
	}
	slices.Sort(out)
	return out
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
// (donorPods: running, scheduled, and for a LeaderWorkerSet the group it
// removes next only). Pods already marked by a transfer are another transfer's. A
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
	// The least protected first: a node-aware plan takes units in this order,
	// so a pod a user protected with a high deletion cost goes last.
	slices.SortFunc(pods, func(a, b corev1.Pod) int {
		return cmp.Or(cmp.Compare(podDeletionCost(&a), podDeletionCost(&b)),
			podReadySince(&b).Compare(podReadySince(&a)), cmp.Compare(a.Name, b.Name))
	})
	toPod := func(p corev1.Pod) allocation.SharePod {
		return allocation.SharePod{Name: utils.GetNamespacedKey(p.Namespace, p.Name), Node: p.Spec.NodeName,
			GPUs: resources.PodGPURequests(&p)}
	}
	if lws {
		u := allocation.ShareUnit{}
		if hash := pods[0].Labels[lwsv1.GroupUniqueHashLabelKey]; hash != "" {
			u.Group = pods[0].Namespace + "/" + hash
		}
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
// free on some node, because a Pending pod has none; where is not known here,
// so they are taken from the nodes with the most free first, which is where
// they would count most. shareFreeNodesAfter takes a hole whose node the
// ledger knows off that node.
func shareFreeNodes(snap map[string]decision.NodeGPU, accelerator string, reserved int) map[string]allocation.ShareNode {
	return shareFreeNodesAfter(snap, accelerator, allocation.ShareHoles{Unplaced: reserved}, nil)
}

// shareFreeNodesAfter is shareFreeNodes with taken GPUs per node (what this
// cycle's optimizer pass and the in-flight ledger hold on each,
// allocation.PassLeft.NodeDebits) no longer free, and the reserved GPUs
// (allocation.ShareLedger.PromisedHoles) taken off the nodes their holes are
// on. A node counts as the node view counts it (allocation.NodeAdmits):
// schedulable, carrying the group's accelerator by name or family, and its
// taints tolerated by each of tolerations (the receivers' pod templates; none
// given: no toleration). Matched by exact name and blind to taints, the share
// placed a receiver on a node of another product of the family, or one a
// taint keeps it off.
func shareFreeNodesAfter(snap map[string]decision.NodeGPU, accelerator string, reserved allocation.ShareHoles,
	taken map[string]int, tolerations ...[]corev1.Toleration) map[string]allocation.ShareNode {
	if len(tolerations) == 0 {
		tolerations = [][]corev1.Toleration{nil}
	}
	nodes := map[string]allocation.ShareNode{}
	for name, n := range snap {
		if !slices.ContainsFunc(tolerations, func(t []corev1.Toleration) bool { return !allocation.NodeAdmits(n, accelerator, t) }) {
			nodes[name] = allocation.ShareNode{Free: max(0, n.Free()-taken[name]), Labels: n.Labels, Groups: n.LWSGroups}
		}
	}
	// A hole whose node is known is taken off that node: the receiver's pod
	// waits for it there. Taken off the freest node instead, a 1-GPU hole on
	// a nearly full node made a wholly free node look short of one, and no
	// whole-node pod was planned onto it. What its node no longer shows free
	// -- something else landed in the hole, or the node is gone -- the
	// receiver's pod finds elsewhere, so it joins the holes of no known node.
	unplaced := reserved.Unplaced
	for _, name := range slices.Sorted(maps.Keys(reserved.Nodes)) {
		g := reserved.Nodes[name]
		n, ok := nodes[name]
		if !ok {
			unplaced += g
			continue
		}
		take := min(n.Free, g)
		n.Free -= take
		nodes[name] = n
		unplaced += g - take
	}
	// The rest are taken in one piece from the freest node, then the next:
	// they are the holes promised transfers opened, whole nodes for
	// whole-node pods. Taken a GPU at a time across nodes, eight promised over
	// two free 8-GPU nodes would leave four on each, and a genuinely free node
	// would stop being placeable until the promise landed.
	for unplaced > 0 {
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
		take := min(n.Free, unplaced)
		n.Free -= take
		nodes[most] = n
		unplaced -= take
	}
	return nodes
}

// shareHoleNodes is where removing pods opens holes: their GPUs per node,
// pods not yet scheduled left out. nil when none is on a node.
func shareHoleNodes(pods []corev1.Pod) map[string]int {
	var out map[string]int
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" {
			continue
		}
		if out == nil {
			out = map[string]int{}
		}
		out[p.Spec.NodeName] += resources.PodGPURequests(p)
	}
	return out
}

// shareFit is the node picture the idle fill places into: the nodes' free
// GPUs, nil without node information, and each receiver's topology domain.
type shareFit struct {
	nodes      map[string]allocation.ShareNode
	domains    map[string]string
	placements map[string][]domain.PodPlacement
}
