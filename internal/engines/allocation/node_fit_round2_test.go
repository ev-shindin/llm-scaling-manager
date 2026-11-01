package allocation

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Review round 2 of node fit, every case in whole 8-GPU units. The node view
// and the package's default stores are shared state: these tests do not run
// in parallel (no t.Parallel).

// clusterQuota is a cluster-scope quota of free GPUs of acc.
func clusterQuota(acc string, free int) *ResourceConstraints {
	return &ResourceConstraints{ProviderName: "cluster-quota", Pools: map[string]ResourcePool{acc: {Limit: free}}}
}

// withDemand sets the demand the rescale pre-pass weighs and sizes a model by,
// at priority 1.
func withDemand(req ModelScalingRequest, demand float64) ModelScalingRequest {
	req.CompositeSignal.Result.TotalDemand = demand
	req.Priority = 1
	return req
}

// decideOnNodes runs the greedy optimizer over constraints with a node view of
// nodes and returns the decisions by variant.
func decideOnNodes(o *GreedyByScoreOptimizer, nodes map[string]decision.NodeGPU, constraints []*ResourceConstraints,
	requests ...ModelScalingRequest) map[string]domain.VariantDecision {
	o.Nodes = NewNodeView(nodes)
	out := map[string]domain.VariantDecision{}
	for _, d := range o.Optimize(context.Background(), requests, constraints) {
		out[d.VariantName] = d
	}
	return out
}

// Architect/SRE M1: when node fit is the binding cut, the decision named the
// provider with the smallest pool. A quota of 16 free beside 24 free
// physical GPUs as 8+4+4+4+4 holds two 8-GPU replicas by pool and one by
// node: node fit cut it, and the quota did not.
func TestANodeFitCutIsNamedNodeFit(t *testing.T) {
	nodes := h100Nodes(8, 4, 4, 4, 4)
	got := decideOnNodes(NewGreedyByScoreOptimizer(), nodes,
		append(physicalOf(nodes), clusterQuota(famHBM3, 16)), famWanting("x", "m", famHBM3, 3, 8))
	d := got["m-v"]
	if d.TargetReplicas != 1 || !d.WasLimited || d.LimitedBy != NodeFitLimiterName {
		t.Fatalf("pools hold 2, nodes hold 1: target %d limited %v by %q, want 1, limited by %q",
			d.TargetReplicas, d.WasLimited, d.LimitedBy, NodeFitLimiterName)
	}
	// Control: the quota binding (8 free) is still named as the quota.
	got = decideOnNodes(NewGreedyByScoreOptimizer(), nodes,
		append(physicalOf(nodes), clusterQuota(famHBM3, 8)), famWanting("x", "m", famHBM3, 3, 8))
	if d := got["m-v"]; d.TargetReplicas != 1 || d.LimitedBy != "cluster-quota" {
		t.Fatalf("control: target %d limited by %q, want 1, limited by cluster-quota", d.TargetReplicas, d.LimitedBy)
	}
}

// The same for a P/D pair: 16 free as 4+4+8 under a quota of 16 hold the pair
// by pool and not by node, so a role is held back by node fit.
func TestAPairTheNodesRefuseIsNamedNodeFit(t *testing.T) {
	nodes := h100Nodes(4, 4, 8)
	got := decideOnNodes(NewGreedyByScoreOptimizer(), nodes,
		append(physicalOf(nodes), clusterQuota(famHBM3, 16)), pdWanting("x", "pd", 3))
	named := 0
	for _, v := range []string{"pd-prefill", "pd-decode"} {
		if got[v].LimitedBy == NodeFitLimiterName {
			named++
		}
		if got[v].LimitedBy != "" && got[v].LimitedBy != NodeFitLimiterName {
			t.Errorf("%s limited by %q: the pools held the pair", v, got[v].LimitedBy)
		}
	}
	if named == 0 {
		t.Fatalf("no role of the refused pair is limited by %s: %+v", NodeFitLimiterName, got)
	}
}

// Whole-node MAJOR 1: the rescale pre-pass filled by pool only. With rescale
// on, 16 free as 4+4+8 ordered two 8-GPU replicas; one fits.
func TestRescaleFillsOnlyWhatTheNodesHold(t *testing.T) {
	nodes := h100Nodes(4, 4, 8)
	o := NewGreedyByScoreOptimizer()
	o.Rescale = RescaleFlags{Cluster: true}
	got := decideOnNodes(o, nodes, physicalOf(nodes), withDemand(famWanting("x", "m", famHBM3, 3, 8), 30000))
	if d := got["m-v"]; d.TargetReplicas != 1 || d.LimitedBy != NodeFitLimiterName {
		t.Fatalf("rescale on 4+4+8: target %d limited by %q, want 1, limited by node fit", d.TargetReplicas, d.LimitedBy)
	}
}

// A P/D pair the rescale fill grows needs both roles placed: on 4+4+8 one
// 8-GPU hole holds a prefill or a decode, never the pair, so neither grows.
func TestRescaleNeverGrowsHalfAPair(t *testing.T) {
	nodes := h100Nodes(4, 4, 8)
	o := NewGreedyByScoreOptimizer()
	o.Rescale = RescaleFlags{Cluster: true}
	got := decideOnNodes(o, nodes, physicalOf(nodes), withDemand(pdWanting("x", "pd", 3), 80000))
	if got["pd-prefill"].TargetReplicas != 1 || got["pd-decode"].TargetReplicas != 1 {
		t.Fatalf("rescale on 4+4+8: prefill %d decode %d, want 1 and 1 (no pair fits)",
			got["pd-prefill"].TargetReplicas, got["pd-decode"].TargetReplicas)
	}
	// Control: on 8+8 the pair grows by one of each.
	nodes = h100Nodes(8, 8)
	got = decideOnNodes(o, nodes, physicalOf(nodes), withDemand(pdWanting("x", "pd", 3), 80000))
	if got["pd-prefill"].TargetReplicas != 2 || got["pd-decode"].TargetReplicas != 2 {
		t.Fatalf("control, rescale on 8+8: prefill %d decode %d, want 2 and 2",
			got["pd-prefill"].TargetReplicas, got["pd-decode"].TargetReplicas)
	}
}

// What the rescale fill places is debited from the view: the additive path
// that follows does not order another model into the same hole. Model a is
// rescaled in its namespace's quota of 8; model b, in an open namespace, is
// not. 16 GPUs are free by pool, as 8+4+4: one 8-GPU hole, which the rescale
// fill takes, and the pool still holds 8 for b.
func TestRescaleDebitsTheHolesItFills(t *testing.T) {
	nodes := h100Nodes(8, 4, 4)
	nsQuota := &ResourceConstraints{ProviderName: "ns-quota", NamespaceScoped: true,
		NamespacePools: map[string]map[string]ResourcePool{"a": {famHBM3: {Limit: 8}}}}
	o := NewGreedyByScoreOptimizer()
	o.Rescale = RescaleFlags{ByNamespace: map[string]bool{"a": true}}
	got := decideOnNodes(o, nodes, append(physicalOf(nodes), nsQuota),
		withDemand(famWanting("a", "ma", famHBM3, 3, 8), 30000), famWanting("b", "mb", famHBM3, 1, 8))
	if a, b := got["ma-v"].TargetReplicas, got["mb-v"].TargetReplicas; a != 1 || b != 0 {
		t.Fatalf("one 8-GPU hole: rescaled ma %d, additive mb %d, want 1 and 0 (the rescale fill took it first)", a, b)
	}
}

// Placement is best-fit: a 4-GPU pod goes to the 4-GPU hole, leaving the
// whole node for the 8-GPU pod. First-fit put it on the whole node, and the
// 8-GPU pod then fit nowhere.
func TestPlacementIsBestFit(t *testing.T) {
	v := NewNodeView(map[string]decision.NodeGPU{
		"n0": {Accelerator: famHBM3, Capacity: 8},
		"n1": {Accelerator: famHBM3, Capacity: 8, Used: 4},
	})
	if !v.PlaceReplicas([]ReplicaShape{{Accelerator: famHBM3, GPUs: 4}, {Accelerator: famHBM3, GPUs: 8}}, true) {
		t.Fatal("8 and 4 free: a 4-GPU replica then an 8-GPU one did not fit")
	}
}

// Go minor 2: a pass kept the node view it consumed, and the next pass, given
// none, placed on it again. A view belongs to one pass.
func TestAPassDoesNotReuseTheLastPassView(t *testing.T) {
	nodes := h100Nodes(8, 8)
	o := NewGreedyByScoreOptimizer()
	if got := decideOnNodes(o, nodes, physicalOf(nodes), famWanting("x", "m", famHBM3, 2, 8)); got["m-v"].TargetReplicas != 2 {
		t.Fatalf("setup: ordered %d, want 2", got["m-v"].TargetReplicas)
	}
	// The engine sets no view this cycle (no physical limiter in force, say):
	// pools alone.
	out := o.Optimize(context.Background(), []ModelScalingRequest{famWanting("x", "m", famHBM3, 2, 8)}, physicalOf(nodes))
	if len(out) != 1 || out[0].TargetReplicas != 2 {
		t.Fatalf("second pass without a view: %+v, want 2 by pools (the last pass's view is spent)", out)
	}
}

// Whole-node minor: a node tainted for one workload looked free to every
// replica, which then sat Pending. A replica lands there only if its pods
// tolerate the taint.
func TestATaintedNodeHoldsOnlyTolerantReplicas(t *testing.T) {
	taint := corev1.Taint{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectNoSchedule}
	nodes := map[string]decision.NodeGPU{
		"n0": {Accelerator: famHBM3, Capacity: 8, Taints: []corev1.Taint{taint}},
		"n1": {Accelerator: famHBM3, Capacity: 8, Used: 4},
	}
	req := famWanting("x", "m", famHBM3, 1, 8)
	if got := decideOnNodes(NewGreedyByScoreOptimizer(), nodes, physicalOf(nodes), req); got["m-v"].TargetReplicas != 0 {
		t.Fatalf("only the tainted node holds 8: ordered %d, want 0", got["m-v"].TargetReplicas)
	}
	req.VariantStates[0].Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual,
		Value: "x", Effect: corev1.TaintEffectNoSchedule}}
	if got := decideOnNodes(NewGreedyByScoreOptimizer(), nodes, physicalOf(nodes), req); got["m-v"].TargetReplicas != 1 {
		t.Fatalf("tolerating the taint: ordered %d, want 1", got["m-v"].TargetReplicas)
	}
}

// Whole-node minor: a wide-EP LeaderWorkerSet group was placed pod by pod
// across topology domains. Its pods share one rack: two 4-GPU pods over two
// racks with 4 free each do not fit; over one rack with 4+4 they do.
func TestAnExclusiveTopologyReplicaLandsInOneDomain(t *testing.T) {
	rack := func(r string, free int) decision.NodeGPU {
		return decision.NodeGPU{Accelerator: famHBM3, Capacity: 8, Used: 8 - free, Labels: map[string]string{"rack": r}}
	}
	req := famWanting("x", "m", famHBM3, 1, 8)
	req.VariantStates[0].PodGPUs = []int{4, 4}
	req.VariantStates[0].TopologyKey = "rack"
	split := map[string]decision.NodeGPU{"n0": rack("a", 4), "n1": rack("b", 4)}
	if got := decideOnNodes(NewGreedyByScoreOptimizer(), split, physicalOf(split), req); got["m-v"].TargetReplicas != 0 {
		t.Fatalf("4 free on each of two racks: ordered %d, want 0", got["m-v"].TargetReplicas)
	}
	one := map[string]decision.NodeGPU{"n0": rack("a", 4), "n1": rack("b", 4), "n2": rack("c", 4), "n3": rack("c", 4)}
	if got := decideOnNodes(NewGreedyByScoreOptimizer(), one, physicalOf(one), req); got["m-v"].TargetReplicas != 1 {
		t.Fatalf("4+4 free in rack c: ordered %d, want 1", got["m-v"].TargetReplicas)
	}
}

// Whole-node m1 / Go 3: a node freed and held for a share receiver is not a
// hole: ReservePromised takes the freest node whole.
func TestReservePromisedHoldsTheFreestNodeWhole(t *testing.T) {
	v := NewNodeView(h100Nodes(8, 4, 4))
	ReservePromised(v, map[string]map[string]decision.PromisedHoles{"": {famHBM3: {Unplaced: 8}}})
	if n := v.fits(replicaFit{acc: famHBM3, pods: []int{8}}, 1); n != 0 {
		t.Fatal("the promised node still holds an 8-GPU replica")
	}
	if n := v.fits(replicaFit{acc: famHBM3, pods: []int{4}}, 2); n != 2 {
		t.Fatalf("the two 4-GPU holes hold %d 4-GPU replicas, want 2", n)
	}
}
