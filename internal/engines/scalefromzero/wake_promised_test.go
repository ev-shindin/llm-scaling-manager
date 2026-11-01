package scalefromzero

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// Review round 2 (whole-node m1 / Go 3): a node the utilization share freed
// and holds for a receiver whose pod is still Pending looked free to a wake.
// 16 free as 8+4+4 with 8 promised: the pool still holds an 8-GPU replica,
// but the 8-GPU node is the receiver's. Process-wide stores: no t.Parallel.
func TestAWakeDoesNotTakeAPromisedNode(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8, 4, 4)
	t.Cleanup(func() { decision.PublishSharePromised(nil, time.Time{}) })
	wake := func() SelectionOutcome {
		e := physicalEngine(16)
		e.beginTick()
		return wakeOnce(t, e, "a", "m", cand("m-decode", "decode", wakeHBM3, 8, 1))
	}
	decision.PublishSharePromised(nil, time.Now())
	if got := wake(); got == OutcomeNoCapacity {
		t.Fatalf("setup: nothing promised, the wake was refused (%s)", got)
	}
	decision.PublishSharePromised(map[string]map[string]decision.PromisedHoles{"": {wakeHBM3: {Unplaced: 8}}}, time.Now())
	if got := wake(); got != OutcomeNoCapacity {
		t.Fatalf("woke into the node promised to a receiver: %s, want %s", got, OutcomeNoCapacity)
	}
}

// Issue #372: the wake's node view held a promise on the freest node, not on
// the node its hole is on. One GPU promised on a nearly full node (n0) beside
// a wholly free one (n1): an 8-GPU wake fits n1. Taken off the freest node,
// the hole made n1 look 7 free and the wake was refused.
func TestAWakeFitsBesideAPromisedHole(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 1, 8)
	t.Cleanup(func() { decision.PublishSharePromised(nil, time.Time{}) })
	wake := func(h decision.PromisedHoles) SelectionOutcome {
		decision.PublishSharePromised(map[string]map[string]decision.PromisedHoles{"": {wakeHBM3: h}}, time.Now())
		e := physicalEngine(9)
		e.beginTick()
		return wakeOnce(t, e, "a", "m", cand("m-decode", "decode", wakeHBM3, 8, 1))
	}
	if got := wake(decision.PromisedHoles{Nodes: map[string]int{"n0": 1}}); got == OutcomeNoCapacity {
		t.Fatalf("the wake was refused beside a one-GPU hole on n0 (%s): n1 is wholly free", got)
	}
	// Control: the same GPU with its node unknown comes off n1, the freest.
	if got := wake(decision.PromisedHoles{Unplaced: 1}); got != OutcomeNoCapacity {
		t.Fatalf("control: woke with the GPU unplaced: %s, want %s", got, OutcomeNoCapacity)
	}
}
