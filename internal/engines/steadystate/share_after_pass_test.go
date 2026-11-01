package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// Review round 2 (whole-node owner, MAJOR 2): the utilization share's idle
// fill planned on the raw constraints, so it counted free the GPUs today's
// optimizer had just ordered for a model the share does not plan, and both
// were ordered into them. Here G, outside clusterNamespaces, is left to
// today's optimizer and takes the quota's 2 free GPUs; the share must not fill
// its receiver into them too. Every cycle, the fleet's growth stays within
// what is free.
//
// The engine's stores are process-wide: no t.Parallel.
func TestShareIdleFillPlansOnWhatThePassLeft(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	setShadowPolicy(t, se.e.Config, "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    clusterNamespaces: [ns]\n")
	ctx, logs := observe(t)
	se.ctx = ctx
	const free = 2
	cons := []*allocation.ResourceConstraints{{ProviderName: "q", Pools: map[string]allocation.ResourcePool{
		"A100": {Limit: 16 + free, Used: 16}}}}
	// G asks today's optimizer for 3 more replicas of one GPU.
	g := shadowRequest("G", "", 8000, 1)
	g.Namespace, g.Priority = "other", 100
	g.CompositeSignal.Remaining, g.CompositeSignal.Score = 3000, 1
	for cycle := range 40 {
		se.clock = se.clock.Add(30 * time.Second)
		reqs := append(f.requests(), g)
		current := map[string]int{}
		for _, r := range reqs {
			current[r.Namespace+"/"+r.VariantStates[0].VariantName] = r.VariantStates[0].CurrentReplicas
		}
		grown := 0
		for _, d := range se.e.decideV2(se.ctx, allocation.NewGreedyByScoreOptimizer(), reqs, cons, f.scaleTargets()) {
			grown += d.TargetReplicas - current[d.Namespace+"/"+d.VariantName]
			if d.VariantName == "G-v" && d.TargetReplicas != 1+free {
				t.Fatalf("setup: G ordered to %d, want %d (the quota's free GPUs)", d.TargetReplicas, 1+free)
			}
		}
		if grown > free {
			t.Fatalf("cycle %d: the fleet grew by %d GPUs on %d free (idle fills so far: %d)",
				cycle, grown, free, logs.FilterMessage("Utilization share: filling from idle GPUs").Len())
		}
	}
}

// Review round 2 (whole-node owner, MAJOR 2): the warm pool's headroom kept
// what today's optimizer ordered for a model the share plans, though the
// share overrides that model's target. In the share's quiet period it holds
// B at what it runs, so the 2 GPUs the optimizer ordered for B are free.
func TestHeadroomDropsWhatThePassOrderedForAPlannedModel(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	cons := []*allocation.ResourceConstraints{{ProviderName: "q", Pools: map[string]allocation.ResourcePool{
		"A100": {Limit: 18, Used: 16}}}}
	se.clock = se.clock.Add(30 * time.Second)
	// B asks today's optimizer for 2 more replicas of one GPU.
	reqs := f.requests()
	reqs[1].CompositeSignal.Remaining, reqs[1].CompositeSignal.Score = 2000, 1
	ordered := 0
	for _, d := range allocation.NewGreedyByScoreOptimizer().Optimize(se.ctx, reqs, cons) {
		if d.VariantName == "B-v" {
			ordered = d.TargetReplicas - f.current["B"]
		}
	}
	if ordered != 2 {
		t.Fatalf("setup: today's optimizer ordered %d for B, want 2", ordered)
	}
	decision.DefaultHeadroom.Reset()
	for _, d := range se.e.decideV2(se.ctx, allocation.NewGreedyByScoreOptimizer(), reqs, cons, f.scaleTargets()) {
		if d.VariantName == "B-v" && d.TargetReplicas != f.current["B"] {
			t.Fatalf("setup: B at %d in the quiet period, want held at %d", d.TargetReplicas, f.current["B"])
		}
	}
	h, ok := decision.DefaultHeadroom.Get()
	if !ok {
		t.Fatal("no headroom published")
	}
	if got := h.Cluster["A100"]; got != 2 {
		t.Fatalf("published cluster headroom %d, want 2: B was held, so what the optimizer ordered for it (%d) is free",
			got, ordered)
	}
}

// growingA100 is a one-variant model on A100 at one replica of gpus GPUs,
// asking today's optimizer for one more.
func growingA100(ns, model string, gpus int) allocation.ModelScalingRequest {
	v := model + "-v"
	return allocation.ModelScalingRequest{
		ModelID: model, Namespace: ns, Priority: 1,
		CompositeSignal: allocation.NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName, Remaining: 1000, Score: 1,
			Result: &domain.AnalyzerResult{
				TotalDemand:       2000,
				VariantCapacities: []domain.VariantCapacity{{VariantName: v, PerReplicaCapacity: 1000}},
			},
		},
		Variants:      []domain.VariantMetadata{{VariantName: v, AcceleratorName: "A100", Cost: 5}},
		VariantStates: []domain.VariantReplicaState{{VariantName: v, CurrentReplicas: 1, GPUsPerReplica: gpus}},
	}
}

// a100Nodes is 8-GPU A100 nodes with the given free GPUs.
func a100Nodes(free ...int) map[string]decision.NodeGPU {
	out := map[string]decision.NodeGPU{}
	for i, f := range free {
		out[string(rune('a'+i))] = decision.NodeGPU{Accelerator: "A100", Capacity: 8, Used: 8 - f}
	}
	return out
}

// Whole-node m1 / Go 3: a node the utilization share freed and holds for a
// receiver whose pod is still Pending looked free to today's optimizer, which
// ordered another model into it. Promised GPUs are held on the nodes too.
// 16 free as 8+4+4 with 8 promised: the pool still holds an 8-GPU replica,
// but the 8-GPU node is the receiver's.
func TestAPromisedNodeIsNoHoleForTheOptimizer(t *testing.T) {
	clock := time.Unix(1000, 0)
	e := &Engine{Config: shadowConfig(t, "")}
	e.utilizationShare.now = func() time.Time { return clock }
	t.Cleanup(func() { decision.PublishSharePromised(nil, time.Time{}) })
	t.Cleanup(decision.DefaultHeadroom.Reset)
	physical := []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
		Pools: map[string]allocation.ResourcePool{"A100": {Limit: 24, Used: 8}}}}
	target := func(promised map[string]map[string]decision.PromisedHoles) int {
		decision.PublishSharePromised(promised, clock)
		o := allocation.NewGreedyByScoreOptimizer()
		o.Nodes = allocation.NewNodeView(a100Nodes(8, 4, 4))
		for _, d := range e.decideV2(t.Context(), o, []allocation.ModelScalingRequest{growingA100("ns", "m", 8)}, physical, nil) {
			return d.TargetReplicas
		}
		return 0
	}
	if got := target(nil); got != 2 {
		t.Fatalf("setup: nothing promised, ordered %d, want 2 (the 8-GPU node is free)", got)
	}
	if got := target(map[string]map[string]decision.PromisedHoles{"": {"A100": {Unplaced: 8}}}); got != 1 {
		t.Fatalf("ordered m to %d into the node promised to a receiver, want it held at 1", got)
	}
}
