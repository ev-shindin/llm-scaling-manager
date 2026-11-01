package allocation

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// Issue #372: today's optimizer node view held utilization-share promises on
// the freest nodes, not on the nodes the promised holes are on. Whole 8-GPU
// units throughout. The node view and the package's default stores are
// shared state: no t.Parallel.

// promiseOn is a cluster-scope promise of famHBM3 GPUs: holes on named nodes,
// and unplaced GPUs whose node is not known.
func promiseOn(unplaced int, nodes map[string]int) map[string]map[string]decision.PromisedHoles {
	return map[string]map[string]decision.PromisedHoles{"": {famHBM3: {Nodes: nodes, Unplaced: unplaced}}}
}

// A whole-node pod beside a promised hole: one GPU promised on a nearly full
// node (n00) and a wholly free node beside it (n01). Taken off the freest
// node, the hole made n01 look 7 free, and no 8-GPU replica fit anywhere.
func TestReservePromisedHoldsAHoleOnItsOwnNode(t *testing.T) {
	v := NewNodeView(h100Nodes(1, 8))
	ReservePromised(v, promiseOn(0, map[string]int{"n00": 1}))
	if got := v.DebitsExcept(nil); !maps.Equal(got, map[string]int{"n00": 1}) {
		t.Fatalf("promise held on %v, want n00=1: the hole's own node", got)
	}
	if n := v.fits(replicaFit{acc: famHBM3, pods: []int{8}}, 1); n != 1 {
		t.Fatal("the wholly free node beside the hole does not hold an 8-GPU replica")
	}
}

// An unrelated pod is not placed into a hole's node: a whole node freed for a
// receiver (n01) beside an equally free node (n00). Freest-first took n00,
// the first of the two in name order, and left n01 -- the receiver's -- open.
func TestReservePromisedKeepsAWholeNodeHoleClosed(t *testing.T) {
	v := NewNodeView(h100Nodes(8, 8, 8))
	ReservePromised(v, promiseOn(0, map[string]int{"n01": 8}))
	if got := v.DebitsExcept(nil); !maps.Equal(got, map[string]int{"n01": 8}) {
		t.Fatalf("promise held on %v, want n01=8: the receiver's node", got)
	}
	if n := v.fits(replicaFit{acc: famHBM3, pods: []int{8}}, 3); n != 2 {
		t.Fatalf("%d 8-GPU replicas fit, want 2 (n00 and n02)", n)
	}
}

// A 4-GPU hole on a half-used node beside a free one: the unrelated 4-GPU pod
// goes to the free node, not into the hole.
func TestReservePromisedKeepsAPartialHoleClosed(t *testing.T) {
	v := NewNodeView(h100Nodes(4, 8))
	ReservePromised(v, promiseOn(0, map[string]int{"n00": 4}))
	if got := v.DebitsExcept(nil); !maps.Equal(got, map[string]int{"n00": 4}) {
		t.Fatalf("promise held on %v, want n00=4", got)
	}
	if h := v.Holes()[famHBM3].Free; len(h) != 1 || h[0] != 8 {
		t.Fatalf("holes left %v, want [8]: only n01, whole", h)
	}
}

// Promises the view cannot hold on their node fall back to the freest nodes,
// in one piece, as every promise did before: a node gone from the snapshot, a
// node showing less free than its hole (something else landed there), a node
// of another accelerator, and the unplaced remainder. The total held does not
// change.
func TestReservePromisedFallsBackToTheFreestNodes(t *testing.T) {
	nodes := h100Nodes(2, 8, 8, 8)
	nodes["other"] = decision.NodeGPU{Accelerator: "MI300X", Capacity: 8}
	v := NewNodeView(nodes)
	// n00: 2 of its 4 held there, 2 off the freest; gone: 8 off the freest;
	// other: 8 off the freest (not an HBM3 node); unplaced: 2.
	ReservePromised(v, promiseOn(2, map[string]int{"n00": 4, "gone": 8, "other": 8}))
	got := v.DebitsExcept(nil)
	total := 0
	for _, g := range got {
		total += g
	}
	if total != 22 {
		t.Fatalf("held %d GPUs (%v), want 22: every promised GPU", total, got)
	}
	if got["n00"] != 2 || got["other"] != 0 {
		t.Fatalf("held %v: want n00=2 (all it has free) and nothing on the MI300X node", got)
	}
	// 20 off the freest, each in one piece: n01 and n02 whole, 4 off n03.
	if want := map[string]int{"n00": 2, "n01": 8, "n02": 8, "n03": 4}; !maps.Equal(got, want) {
		t.Fatalf("held %v, want %v", got, want)
	}
}

// The warm pool reads the same view: the headroom republished after the share
// promised a hole holds it on that hole's node. One GPU promised on n00 beside
// a wholly free n01 leaves the pool one 8-GPU Pod, not none.
func TestRepublishedHeadroomHoldsAHoleOnItsOwnNode(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	nodes := h100Nodes(1, 8)
	o := NewGreedyByScoreOptimizer()
	o.Nodes = NewNodeView(nodes)
	o.Optimize(context.Background(), []ModelScalingRequest{famWanting("x", "m", famHBM3, 0, 8)}, physicalOf(nodes))
	units := func() int {
		n, ok := decision.DefaultHeadroom.Units(famHBM3, 8, 0, time.Minute, time.Now())
		if !ok {
			t.Fatal("setup: the headroom carries no node holes")
		}
		return n
	}
	if got := units(); got != 1 {
		t.Fatalf("setup: %d 8-GPU Pods before the promise, want 1", got)
	}
	if !o.RepublishHeadroom(nil, promiseOn(0, map[string]int{"n00": 1}), time.Now()) {
		t.Fatal("setup: no pass to republish")
	}
	if got := units(); got != 1 {
		t.Fatalf("%d 8-GPU Pods after one GPU promised on n00, want 1: n01 is still whole", got)
	}
	// Control: the same promise with its node unknown comes off the freest.
	o.RepublishHeadroom(nil, promiseOn(1, nil), time.Now())
	if got := units(); got != 0 {
		t.Fatalf("control: %d 8-GPU Pods with the GPU unplaced, want 0", got)
	}
}
