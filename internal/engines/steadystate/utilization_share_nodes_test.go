package steadystate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// nodeFleet is the share fleet with every pod requesting one GPU, each on a
// node of its own but for A-v-5 and A-v-6, which share node "shared". B grows
// by one replica of a single 2-GPU pod: no donor pod fits it on its own, and
// every node is full, so only the two pods on "shared" together open a hole.
type nodeFleet struct {
	f     *shareFleet
	c     client.Client
	se    *shareEngine
	nodes map[string]decision.NodeGPU
}

func newNodeFleet(t *testing.T) *nodeFleet {
	t.Helper()
	f := newShareFleet()
	c := sharePods(t, f)
	var list corev1.PodList
	if err := c.List(context.Background(), &list, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	nodes := map[string]decision.NodeGPU{}
	for _, p := range list.Items {
		node := "n-" + p.Name
		if p.Name == "A-v-5" || p.Name == "A-v-6" {
			node = "shared"
		}
		// The scheduler frees what a pod requests, and the node picture
		// counts the same: every pod requests its one GPU.
		p.Spec.NodeName = node
		p.Spec.Containers = []corev1.Container{{Name: "vllm", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}
		if err := c.Update(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
		n := nodes[node]
		n.Accelerator, n.Capacity, n.Used = "A100", n.Capacity+1, n.Used+1
		nodes[node] = n
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.e.utilizationShare.nodes = &decision.NodeGPUStore{}
	return &nodeFleet{f: f, c: c, se: se, nodes: nodes}
}

// cycle runs one pass; withNodes publishes the node picture first.
func (nf *nodeFleet) cycle(withNodes bool) map[string]utilizationShareOverride {
	nf.se.clock = nf.se.clock.Add(30 * time.Second)
	if withNodes {
		nf.se.e.utilizationShare.nodes.Publish(nf.nodes, nf.se.clock)
	}
	out := nf.f.requests()
	for i := range out {
		st := &out[i].VariantStates[0]
		if out[i].ModelID == "B" {
			st.GPUsPerReplica, st.PodGPUs = 2, []int{2}
		} else {
			st.PodGPUs = []int{1}
		}
	}
	return nf.se.e.evaluateUtilizationShare(nf.se.ctx, out, fullQuota(), nf.f.scaleTargets())
}

// untilMarked cycles until some pod is marked, and returns the marked names.
func (nf *nodeFleet) untilMarked(t *testing.T, withNodes bool) []string {
	t.Helper()
	var marked []string
	for range 20 {
		nf.cycle(withNodes)
		for _, p := range markedPods(t, nf.c) {
			marked = append(marked, p.Name)
		}
		if len(marked) > 0 {
			break
		}
	}
	slices.Sort(marked)
	return marked
}

// markIDs returns the transfer IDs the marked pods carry.
func markIDs(t *testing.T, c client.Client) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range markedPods(t, c) {
		var m transferMark
		if err := json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m); err != nil {
			t.Fatal(err)
		}
		out[m.ID] = true
	}
	return out
}

func (nf *nodeFleet) deletePods(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		p := &corev1.Pod{}
		if err := nf.c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: n}, p); err != nil {
			t.Fatal(err)
		}
		if err := nf.c.Delete(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
}

// Two of A's pods share node "shared": together they open a 2-GPU hole there,
// and exactly they are marked (section 6.5, with node information). Control:
// without a node snapshot the same fleet funds nothing, because pods on
// unknown nodes are never added up.
func TestUtilizationShareFundsAPodFromDonorsSharingANode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withNodes bool
		want      []string
	}{
		{"with node information", true, []string{"A-v-5", "A-v-6"}},
		{"without node information (control)", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nf := newNodeFleet(t)
			if got := nf.untilMarked(t, tc.withNodes); !slices.Equal(got, tc.want) {
				t.Fatalf("marked %v, want %v", got, tc.want)
			}
		})
	}
}

// The ReplicaSet does not always remove the marked pods: a not-ready sibling
// goes first, wherever it runs. If A shrinks by two pods that are not the
// planned ones, the hole on "shared" never opened, and B must not be raised
// into it; the planned pods are unmarked. Control: when the planned pods are
// the ones that go, B is raised.
func TestUtilizationShareChecksWhichDonorPodWent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gone  []string
		wantB int
	}{
		{"a different pod went", []string{"A-v-0", "A-v-1"}, 5},
		{"the planned pods went (control)", []string{"A-v-5", "A-v-6"}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nf := newNodeFleet(t)
			if got := nf.untilMarked(t, true); !slices.Equal(got, []string{"A-v-5", "A-v-6"}) {
				t.Fatalf("setup: marked %v", got)
			}
			ended := markIDs(t, nf.c)
			nf.deletePods(t, tc.gone...)
			nf.f.current["A"] -= 2
			o := nf.cycle(true)
			if o["ns/B-v"].Target != tc.wantB {
				t.Fatalf("B = %d, want %d", o["ns/B-v"].Target, tc.wantB)
			}
			// The next plan may mark the same pods again, under new IDs.
			for id := range markIDs(t, nf.c) {
				if ended[id] {
					t.Fatalf("a pod still carries the mark of ended transfer %s", id)
				}
			}
		})
	}
}

// A fill timeout is attributed from the receiver's Pending pods and the node
// picture: something took the released GPUs, or they are free but the pods do
// not fit the nodes.
func TestShareFillTimeoutCause(t *testing.T) {
	nodes := func(free ...int) map[string]decision.NodeGPU {
		out := map[string]decision.NodeGPU{"other": {Accelerator: "H100", Capacity: 8},
			"cordoned": {Accelerator: "A100", Capacity: 8, Unschedulable: true}}
		for i, f := range free {
			out[fmt.Sprint("n", i)] = decision.NodeGPU{Accelerator: "A100", Capacity: 8, Used: 8 - f}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		pending []int
		nodes   map[string]decision.NodeGPU
		want    string
	}{
		{"taken: fewer free than the Pending pods request", []int{4, 4}, nodes(2, 3), constants.ScalingBlockedReleaseTaken},
		{"shape: enough in total, no node holds a pod", []int{4, 4}, nodes(3, 3, 3), constants.ScalingBlockedReleaseShapeMismatch},
		{"shape: one pod fits, the other does not", []int{4, 4}, nodes(4, 2, 2), constants.ScalingBlockedReleaseShapeMismatch},
		{"the pods fit: not a GPU cause", []int{4, 4}, nodes(4, 4), ""},
		{"only the Pending part counts: a placed leader frees nothing", []int{4}, nodes(4), ""},
		{"other accelerators and cordoned nodes do not count", []int{1}, nodes(0), constants.ScalingBlockedReleaseTaken},
		{"nothing Pending", nil, nodes(0), ""},
		{"no node information", []int{4}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shareFillTimeoutCause("A100", tc.pending, tc.nodes); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The reason is reported on the receiver's model for one release timeout,
// then cleared.
func TestShareFillBlockedReasonIsPublishedAndExpires(t *testing.T) {
	l := allocation.NewShareLedger()
	t0 := time.Unix(0, 0)
	g := allocation.ShareGroup{
		Roles:   []allocation.ShareRole{{Key: "B", Need: 1}, {Key: "A", Need: 1}},
		Origins: map[string]allocation.ShareRoleOrigin{"B": {Namespace: "ns", ModelID: "b"}, "A": {Namespace: "ns", ModelID: "a"}},
		Budget:  16,
	}
	l.FillBlocked("B", constants.ScalingBlockedReleaseTaken, t0.Add(time.Minute))
	got := shareBlockedReasons(l, g, allocation.ShareEvaluation{}, nil, t0.Add(30*time.Second))
	if !slices.Contains(got["ns/b"], constants.ScalingBlockedReleaseTaken) {
		t.Fatalf("reasons %v, want release-taken on ns/b", got)
	}
	if slices.Contains(got["ns/a"], constants.ScalingBlockedReleaseTaken) {
		t.Fatal("the reason belongs to the receiver only")
	}
	got = shareBlockedReasons(l, g, allocation.ShareEvaluation{}, nil, t0.Add(time.Minute))
	if slices.Contains(got["ns/b"], constants.ScalingBlockedReleaseTaken) {
		t.Fatal("the reason outlived its report window")
	}
}

// End to end in the engine: A releases, B is raised, B's new pod never lands.
// When the fill times out, B's model is reported release-taken if every node
// is full -- something took the GPUs -- and carries no release reason when a
// node has room (control: B is Pending for another cause).
func TestUtilizationShareAttributesAFillTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		free int
		want bool
	}{
		{"every node full", 0, true},
		{"a node has room (control)", 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := freshMetrics(t)
			f := newShareFleet()
			se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
			se.e.utilizationShare.nodes = &decision.NodeGPUStore{}
			nodes := map[string]decision.NodeGPU{"n1": {Accelerator: "A100", Capacity: 20, Used: 20 - tc.free}}
			cycle := func() {
				se.clock = se.clock.Add(30 * time.Second)
				se.e.utilizationShare.nodes.Publish(nodes, se.clock)
				se.e.evaluateUtilizationShare(se.ctx, f.requests(), fullQuota(), f.scaleTargets())
			}
			started := 0
			for range 20 {
				cycle()
				if started = len(markedPods(t, se.c)); started > 0 {
					break
				}
			}
			if started == 0 {
				t.Fatal("setup: no transfer started")
			}
			f.current["A"] -= started // released; B's new pod waits for a node
			pending := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "B-v-new",
					Labels: map[string]string{"app": "B-decode", appsv1.DefaultDeploymentUniqueLabelKey: "h1"},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet",
						Name: "B-decode-h1", UID: "rs", Controller: ptr.To(true)}}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}},
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			}
			if err := se.c.Create(context.Background(), pending); err != nil {
				t.Fatal(err)
			}
			taken := false
			for range 40 {
				cycle()
				for _, m := range family(t, reg, constants.WVAModelScalingBlocked) {
					if label(m, constants.LabelModelName) == "B" && label(m, constants.LabelReason) == constants.ScalingBlockedReleaseTaken {
						taken = true
					}
				}
				if taken {
					break
				}
			}
			if taken != tc.want {
				t.Fatalf("release-taken on B: %v, want %v", taken, tc.want)
			}
		})
	}
}
