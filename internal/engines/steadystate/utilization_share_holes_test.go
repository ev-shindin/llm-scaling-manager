package steadystate

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// A second whole-node receiver while two promised holes are in flight: one
// whole node (n1) released for R1's 8-GPU pod, and one GPU (n2) released for
// M1's 1-GPU pod, both still Pending. n3 is wholly free: the cluster is larger
// than the group's quota, which its roles commit in full. R2, short of a
// second 8-GPU replica, fits n3, its quota paid by 1-GPU donors on n7.
//
//	n1  8 free   R1's hole (promised 8)
//	n2  1 free   M1's hole (promised 1); A x3, B x2, M1 x2
//	n3  8 free   nobody's
//	n4  E x1 (8)   n5  R1 x1 (8)   n6  R2 x1 (8)   n7  A x2, B x2, C x4
//
// known records where each hole is, as marking the donor pods does.
func reserveNodesCase(t *testing.T, known bool) (*allocation.ShareLedger, allocation.SharePlanInput, map[string]decision.NodeGPU, allocation.ShareTimings) {
	t.Helper()
	tm := allocation.ShareTimings{Window: 5 * time.Minute, ReleaseTimeout: time.Hour, FillTimeout: time.Hour,
		ReversalHold: 12 * time.Minute, SwingWindow: 96 * time.Minute}
	snap := map[string]decision.NodeGPU{}
	for n, used := range map[string]int{"n1": 0, "n2": 7, "n3": 0, "n4": 8, "n5": 8, "n6": 8, "n7": 8} {
		snap[n] = decision.NodeGPU{Accelerator: "H100", Capacity: 8, Used: used}
	}
	one := allocation.ShareVariant{Name: "v", GPUs: 1, PodGPUs: []int{1}}
	eight := allocation.ShareVariant{Name: "v", GPUs: 8, PodGPUs: []int{8}}
	roles := []allocation.ShareRole{
		{Key: "R1", Weight: 1, Need: 14, Floor: 8, Ceiling: 64, ReplicaGPUs: 8},
		{Key: "R2", Weight: 1, Need: 14, Floor: 8, Ceiling: 64, ReplicaGPUs: 8},
		{Key: "E", Weight: 1, Need: 4, Floor: 8, Ceiling: 64, ReplicaGPUs: 8},
		{Key: "M1", Weight: 1, Need: 2.4, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
		{Key: "A", Weight: 1, Need: 0.8, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
		{Key: "B", Weight: 1, Need: 0.8, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
		{Key: "C", Weight: 1, Need: 0.8, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
	}
	// Before the releases: E held 16 (one pod on n1), A 6 (one on n2).
	before := map[string]int{"R1": 8, "R2": 8, "E": 16, "M1": 2, "A": 6, "B": 4, "C": 4}
	l := allocation.NewShareLedger()
	t0 := time.Unix(0, 0)
	r1 := l.Start(allocation.ShareTransfer{Donor: "E", Receiver: "R1", GPUs: 8, DonorGPUs: 8, DonorVariant: "v", ReceiverVariant: "v"}, before, t0, tm)
	m1 := l.Start(allocation.ShareTransfer{Donor: "A", Receiver: "M1", GPUs: 1, DonorGPUs: 1, DonorVariant: "v", ReceiverVariant: "v"}, before, t0, tm)
	if known {
		l.RecordHoles(r1.ID, map[string]int{"n1": 8})
		l.RecordHoles(m1.ID, map[string]int{"n2": 1})
	}
	held := map[string]int{"R1": 8, "R2": 8, "E": 8, "M1": 2, "A": 5, "B": 4, "C": 4}
	l.Observe(held, t0.Add(time.Minute), tm)
	if got := l.Promised(); got != 9 {
		t.Fatalf("setup: %d GPUs promised, want 9 (both transfers Filling): %v", got, l.Transfers())
	}
	units := func(role string, nodes ...string) []allocation.ShareUnit {
		out := make([]allocation.ShareUnit, 0, len(nodes))
		for i, n := range nodes {
			out = append(out, allocation.ShareUnit{Pods: []allocation.SharePod{{Name: fmt.Sprintf("ns/%s-%d", role, i), Node: n, GPUs: 1}}})
		}
		return out
	}
	committed := 0
	for _, c := range l.Committed(held) {
		committed += c
	}
	thresholds := map[string]float64{}
	for _, r := range roles {
		thresholds[r.Key] = 0.8
	}
	in := allocation.SharePlanInput{Roles: roles, Held: held, Thresholds: thresholds, Budget: committed, Tolerance: 0.15,
		Give: map[string]allocation.ShareVariant{"A": one, "B": one, "C": one, "E": eight},
		Grow: map[string]allocation.ShareVariant{"R1": eight, "R2": eight, "M1": one},
		DonorUnits: map[string][]allocation.ShareUnit{
			"A": units("A", "n2", "n2", "n2", "n7", "n7"),
			"B": units("B", "n2", "n2", "n7", "n7"),
			"C": units("C", "n7", "n7", "n7", "n7"),
		},
		PhysicalFree: 8,
	}
	return l, in, snap, tm
}

// fundsR2 plans five cycles on nodes and reports whether R2 was funded.
func fundsR2(l *allocation.ShareLedger, in allocation.SharePlanInput, nodes map[string]allocation.ShareNode, tm allocation.ShareTimings) (bool, []allocation.ShareTransfer) {
	var started []allocation.ShareTransfer
	for i := range 5 {
		in.Nodes = nodes
		p := allocation.PlanShareTransfers(l, in, time.Unix(120+int64(30*i), 0), tm)
		started = append(started, p.Started...)
		for _, t := range p.Started {
			if t.Receiver == "R2" {
				return true, started
			}
		}
	}
	return false, started
}

// free is each node's free GPUs in a node picture.
func free(nodes map[string]allocation.ShareNode) map[string]int {
	out := map[string]int{}
	for n, s := range nodes {
		out[n] = s.Free
	}
	return out
}

// The node picture takes each promised hole off the node it is on. Taken off
// the freest instead, the GPU promised to M1 on n2 came off n3, which then
// showed 7 free, and R2 -- whose 8-GPU pod fits n3 -- was never funded.
func TestShareSecondWholeNodeReceiverBesideAPromisedHole(t *testing.T) {
	l, in, snap, tm := reserveNodesCase(t, true)
	nodes := shareFreeNodesAfter(snap, "H100", l.PromisedHoles(time.Unix(60, 0)), nil)
	if got := free(nodes); got["n1"] != 0 || got["n2"] != 0 || got["n3"] != 8 {
		t.Fatalf("node picture %v, want n1=0 n2=0 n3=8: each hole off its own node", got)
	}
	if ok, started := fundsR2(l, in, nodes, tm); !ok {
		t.Errorf("R2 was never funded although its pod fits n3; started %v", started)
	}
}

// A promise whose node the ledger does not know -- a wake held before its
// transfer's pods were marked, a transfer restored from a pod no longer on a
// node -- is taken off the freest nodes, as every promise was before.
func TestShareUnknownHoleUsesTheFreestNodes(t *testing.T) {
	l, _, snap, _ := reserveNodesCase(t, false)
	h := l.PromisedHoles(time.Unix(60, 0))
	if len(h.Nodes) != 0 || h.Unplaced != 9 {
		t.Fatalf("holes %+v, want 9 unplaced", h)
	}
	got := free(shareFreeNodesAfter(snap, "H100", h, nil))
	if want := free(shareFreeNodes(snap, "H100", 9)); !maps.Equal(got, want) {
		t.Fatalf("unknown holes give %v, want the freest-first picture %v", got, want)
	}
	// Freest first, in one piece: n1 (name order among the two 8s) takes 8,
	// then n3 one.
	if got["n1"] != 0 || got["n2"] != 1 || got["n3"] != 7 {
		t.Fatalf("freest-first picture %v, want n1=0 n2=1 n3=7", got)
	}
}

// A hole on a node that no longer shows it free -- something else landed in
// it, or the snapshot predates the release -- is not lost: the receiver's
// pod lands wherever it fits, so what the node cannot absorb is taken off the
// freest nodes, and the picture holds back as much as before.
func TestShareHoleOnANodeSinceFull(t *testing.T) {
	snap := map[string]decision.NodeGPU{
		"n1": {Accelerator: "H100", Capacity: 8, Used: 8},
		"n2": {Accelerator: "H100", Capacity: 8, Used: 4},
		"n3": {Accelerator: "H100", Capacity: 8},
	}
	got := free(shareFreeNodesAfter(snap, "H100", allocation.ShareHoles{Nodes: map[string]int{"n1": 8}}, nil))
	if got["n1"] != 0 || got["n2"] != 4 || got["n3"] != 0 {
		t.Fatalf("picture %v, want n1=0 n2=4 n3=0: the full node's hole taken off n3 in one piece", got)
	}
	// A hole on a node the snapshot no longer has is the same.
	got = free(shareFreeNodesAfter(snap, "H100", allocation.ShareHoles{Nodes: map[string]int{"gone": 4}}, nil))
	if got["n2"] != 4 || got["n3"] != 4 {
		t.Fatalf("picture %v, want n2=4 n3=4: a gone node's hole taken off the freest", got)
	}
}

// Promises on one node beyond its free GPUs take the node to zero, and the
// rest off the freest nodes; the total held back is unchanged.
func TestShareHolesBeyondANodesFree(t *testing.T) {
	snap := map[string]decision.NodeGPU{
		"n1": {Accelerator: "H100", Capacity: 8},
		"n2": {Accelerator: "H100", Capacity: 8, Used: 7},
		"n3": {Accelerator: "H100", Capacity: 8},
	}
	h := allocation.ShareHoles{Nodes: map[string]int{"n2": 3}, Unplaced: 1}
	got := free(shareFreeNodesAfter(snap, "H100", h, nil))
	if got["n1"] != 5 || got["n2"] != 0 || got["n3"] != 8 {
		t.Fatalf("picture %v, want n1=5 n2=0 n3=8", got)
	}
	if before, after := 17, got["n1"]+got["n2"]+got["n3"]; before-after != h.Total() {
		t.Fatalf("held back %d, want %d", before-after, h.Total())
	}
}

// The picture is the same every time: the holes are taken in node order,
// and ties among the freest broken by name, whatever order the maps iterate.
func TestShareHolesDeterministic(t *testing.T) {
	snap := map[string]decision.NodeGPU{}
	for i := range 12 {
		snap[fmt.Sprintf("n%02d", i)] = decision.NodeGPU{Accelerator: "H100", Capacity: 8, Used: i % 3}
	}
	h := allocation.ShareHoles{Nodes: map[string]int{"n01": 9, "n04": 2, "n07": 8, "gone": 3}, Unplaced: 13}
	first := free(shareFreeNodesAfter(snap, "H100", h, nil))
	for range 50 {
		if got := free(shareFreeNodesAfter(snap, "H100", h, nil)); !maps.Equal(got, first) {
			t.Fatalf("picture changed between runs: %v then %v", first, got)
		}
	}
}

// shareHoleNodes is where marked pods run: their GPUs per node, a pod not yet
// on a node left out.
func TestShareHoleNodes(t *testing.T) {
	pod := func(node string, gpus int64) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: corev1.PodSpec{NodeName: node,
			Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				"nvidia.com/gpu": *resource.NewQuantity(gpus, resource.DecimalSI)}}}}}}
	}
	got := shareHoleNodes([]corev1.Pod{pod("a", 4), pod("a", 4), pod("b", 1), pod("", 8)})
	if want := map[string]int{"a": 8, "b": 1}; !maps.Equal(got, want) {
		t.Fatalf("holes %v, want %v", got, want)
	}
	if got := shareHoleNodes([]corev1.Pod{pod("", 8)}); got != nil {
		t.Fatalf("no pod on a node: holes %v, want nil", got)
	}
}

// The engine records where the pods it marks run, and a restart reads it back
// from the marked pods: the two pods planned on node "shared" put both their
// GPUs' holes there, before the restart and after.
func TestUtilizationShareRecordsWhereHolesOpen(t *testing.T) {
	nf := newNodeFleet(t)
	if got := nf.untilMarked(t, true); !slices.Equal(got, []string{"A-v-5", "A-v-6"}) {
		t.Fatalf("setup: marked %v", got)
	}
	holes := func() map[string]int {
		out := map[string]int{}
		for _, l := range nf.se.e.utilizationShare.ledgers {
			for _, tr := range l.Transfers() {
				for n, g := range tr.HoleNodes {
					out[n] += g
				}
			}
		}
		return out
	}
	if got, want := holes(), map[string]int{"shared": 2}; !maps.Equal(got, want) {
		t.Fatalf("holes recorded at the mark %v, want %v", got, want)
	}
	restarted := newShareEngine(t, nf.f, nf.c, nf.se.clock)
	restarted.e.utilizationShare.nodes = &decision.NodeGPUStore{}
	nf.se = restarted
	nf.cycle(true) // restores from the marks
	if got, want := holes(), map[string]int{"shared": 2}; !maps.Equal(got, want) {
		t.Fatalf("holes restored from the marks %v, want %v", got, want)
	}
}

// The node picture the planner gets, through the engine: once the pods
// planned on node "shared" are gone and B is raised into the hole they left,
// the next cycle takes B's promise off "shared" -- not off the freest node,
// "a-spare", which is nobody's and stays whole. The engine logs that picture
// at V(DEBUG).
func TestUtilizationSharePlansOnTheHoleItsPromiseIsOn(t *testing.T) {
	nf := newNodeFleet(t)
	if got := nf.untilMarked(t, true); !slices.Equal(got, []string{"A-v-5", "A-v-6"}) {
		t.Fatalf("setup: marked %v", got)
	}
	nf.deletePods(t, "A-v-5", "A-v-6")
	nf.f.current["A"] -= 2
	nf.nodes["shared"] = decision.NodeGPU{Accelerator: "A100", Capacity: 2}
	raised := false
	for range 6 {
		if nf.cycle(true)["ns/B-v"].Target > 5 {
			raised = true
			break
		}
	}
	if !raised {
		t.Fatal("setup: B was not raised once its planned holes opened")
	}
	core, logs := observer.New(zapcore.Level(-logging.DEBUG))
	nf.se.ctx = logr.NewContext(t.Context(), zapr.NewLogger(zap.New(core)))
	nf.nodes["a-spare"] = decision.NodeGPU{Accelerator: "A100", Capacity: 2}
	nf.cycle(true)
	lines := logs.FilterMessage("Utilization share: node picture").All()
	if len(lines) == 0 {
		t.Fatal("no node picture logged")
	}
	fields := lines[len(lines)-1].ContextMap()
	got, _ := fields["free"].(map[string]int)
	if got["shared"] != 0 || got["a-spare"] != 2 {
		t.Fatalf("node picture %v (promised on nodes %v, unplaced %v): want shared=0 a-spare=2, B's promise off its own hole",
			got, fields["promisedOnNodes"], fields["promisedUnplaced"])
	}
}
