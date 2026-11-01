package steadystate

import (
	"context"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// Issue #372: today's optimizer held utilization-share promises on the freest
// nodes, not on the promised holes' nodes. Whole 8-GPU units. Process-wide
// stores: no t.Parallel.

// A whole-node pod beside a promised hole, through decideV2: one GPU promised
// on a nearly full node (a) and a wholly free node (b). Taken off the freest
// node, the hole made b look 7 free and m's second 8-GPU replica was refused.
func TestAWholeNodePodFitsBesideAPromisedHole(t *testing.T) {
	clock := time.Unix(1000, 0)
	e := &Engine{Config: shadowConfig(t, "")}
	e.utilizationShare.now = func() time.Time { return clock }
	t.Cleanup(func() { decision.PublishSharePromised(nil, time.Time{}) })
	t.Cleanup(decision.DefaultHeadroom.Reset)
	physical := []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
		Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16, Used: 7}}}}
	target := func(promised decision.PromisedHoles) int {
		decision.PublishSharePromised(map[string]map[string]decision.PromisedHoles{"": {"A100": promised}}, clock)
		o := allocation.NewGreedyByScoreOptimizer()
		o.Nodes = allocation.NewNodeView(a100Nodes(1, 8))
		for _, d := range e.decideV2(t.Context(), o, []allocation.ModelScalingRequest{growingA100("ns", "m", 8)}, physical, nil) {
			return d.TargetReplicas
		}
		return 0
	}
	if got := target(decision.PromisedHoles{Nodes: map[string]int{"a": 1}}); got != 2 {
		t.Fatalf("ordered m to %d with one GPU promised on a, want 2: b is wholly free", got)
	}
	// Control: the same GPU with its node unknown comes off b, the freest.
	if got := target(decision.PromisedHoles{Unplaced: 1}); got != 1 {
		t.Fatalf("control: ordered m to %d with the GPU unplaced, want 1", got)
	}
}

// The view mirrors the planner's picture on #371's case: R1's whole-node hole
// on n1 and M1's one-GPU hole on n2, both Filling. The optimizer's view holds
// each on its own node and leaves n3 whole for an 8-GPU pod; freest-first took
// M1's GPU off n3.
func TestTheOptimizerViewHoldsEachHoleOnItsNode(t *testing.T) {
	l, _, snap, _ := reserveNodesCase(t, true)
	h := l.PromisedHoles(time.Unix(60, 0))
	v := allocation.NewNodeView(snap)
	allocation.ReservePromised(v, map[string]map[string]decision.PromisedHoles{"": {"H100": h}})
	if got := v.DebitsExcept(nil); !maps.Equal(got, map[string]int{"n1": 8, "n2": 1}) {
		t.Fatalf("promises held on %v, want n1=8 n2=1", got)
	}
	if h := v.Holes()["H100"].Free; len(h) != 1 || h[0] != 8 {
		t.Fatalf("holes left %v, want [8]: n3 whole", h)
	}
	// And the same as the planner's own picture.
	if got := free(shareFreeNodesAfter(snap, "H100", h, nil)); got["n1"] != 0 || got["n2"] != 0 || got["n3"] != 8 {
		t.Fatalf("planner picture %v disagrees", got)
	}
}

// The share publishes each promise with its hole's node: the donor pods it
// marked are on n1, and the wake and the optimizer's view read it from there.
func TestUtilizationSharePublishesPromisedHoleNodes(t *testing.T) {
	f := newShareFleet()
	clock := time.Unix(0, 0)
	c := sharePods(t, f)
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		p.Spec.Containers = []corev1.Container{{Name: "vllm", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}
		if err := c.Update(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	e := &Engine{Config: shadowConfig(t, activeShare), client: c}
	e.utilizationShare.now = func() time.Time { return clock }
	t.Cleanup(func() { decision.PublishSharePromised(nil, time.Time{}) })
	ctx, _ := observe(t)
	cycle := func() map[string]utilizationShareOverride {
		clock = clock.Add(30 * time.Second)
		return e.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets())
	}
	started := 0
	for range 20 {
		if o := cycle(); o != nil {
			if started = 9 - targetOf(t, o, "ns/A-v"); started > 0 {
				break
			}
		}
	}
	if started == 0 {
		t.Fatal("setup: no transfer started in 20 cycles")
	}
	f.current["A"] -= started
	cycle()
	h := decision.LatestSharePromisedHoles(clock)[""]["A100"]
	if h.Total() != started || h.Nodes["n1"] != started || h.Unplaced != 0 {
		t.Fatalf("published %+v, want %d on n1, none unplaced", h, started)
	}
	if got := decision.LatestSharePromised(clock)[""]["A100"]; got != started {
		t.Fatalf("published count %d, want %d", got, started)
	}
}

// The warm pool's republished headroom withholds only the promises made in
// the cycle, each on the node whose hole grew: a whole node newly promised on
// n1 beside an older one-GPU hole on n0 is held on n1, and a node's growth
// beyond the new total is not held twice.
func TestNewlyPromisedKeepsTheNewHolesNodes(t *testing.T) {
	at := func(unplaced int, nodes map[string]int) map[string]map[string]decision.PromisedHoles {
		return map[string]map[string]decision.PromisedHoles{"": {"H100": {Nodes: nodes, Unplaced: unplaced}}}
	}
	got := newlyPromised(at(0, map[string]int{"n0": 1}), at(0, map[string]int{"n0": 1, "n1": 8}))[""]["H100"]
	if !maps.Equal(got.Nodes, map[string]int{"n1": 8}) || got.Unplaced != 0 {
		t.Fatalf("new promise %+v, want n1=8", got)
	}
	// Two unplaced GPUs found their node (n0 grew by 4): 2 new in all, on n0.
	got = newlyPromised(at(2, nil), at(0, map[string]int{"n0": 4}))[""]["H100"]
	if !maps.Equal(got.Nodes, map[string]int{"n0": 2}) || got.Unplaced != 0 {
		t.Fatalf("new promise %+v, want n0=2", got)
	}
	// Nothing new: nothing withheld.
	if got := newlyPromised(at(0, map[string]int{"n0": 8}), at(8, nil)); len(got) != 0 {
		t.Fatalf("new promises %+v, want none", got)
	}
}
