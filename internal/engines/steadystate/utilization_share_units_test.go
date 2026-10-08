package steadystate

import (
	"encoding/json"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

func unitPod(name, node string, gpus int64, ready bool, marked bool) corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "vllm",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				"nvidia.com/gpu": *resource.NewQuantity(gpus, resource.DecimalSI)}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}}},
	}
	if marked {
		p.Annotations = map[string]string{utilizationShareTransferAnnotation: "{}"}
	}
	return p
}

// What a donor offers a node-aware set: a Deployment each Ready, unmarked pod,
// and nothing while any pod is not Ready -- the ReplicaSet removes a not-Ready
// pod first, wherever it runs; a LeaderWorkerSet its group as one unit.
func TestShareDonorUnits(t *testing.T) {
	names := func(lws bool, pods ...corev1.Pod) [][]string {
		units := shareDonorUnits(lws, pods, annotated)
		out := make([][]string, 0, len(units))
		for _, u := range units {
			n := make([]string, 0, len(u.Pods))
			for _, p := range u.Pods {
				n = append(n, p.Name+"@"+p.Node)
			}
			out = append(out, n)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		lws  bool
		pods []corev1.Pod
		want [][]string
	}{
		{"deployment: one unit per Ready pod, by name", false,
			[]corev1.Pod{unitPod("b", "n2", 1, true, false), unitPod("a", "n1", 1, true, false)},
			[][]string{{"ns/a@n1"}, {"ns/b@n2"}}},
		{"deployment: a marked pod is another transfer's", false,
			[]corev1.Pod{unitPod("a", "n1", 1, true, true), unitPod("b", "n2", 1, true, false)},
			[][]string{{"ns/b@n2"}}},
		{"deployment: nothing while a pod is not Ready", false,
			[]corev1.Pod{unitPod("a", "n1", 1, true, false), unitPod("b", "n2", 1, false, false)},
			nil},
		{"lws: the group is one unit, Ready or not", true,
			[]corev1.Pod{unitPod("g-1", "n2", 4, false, false), unitPod("g-0", "n1", 4, true, false)},
			[][]string{{"ns/g-0@n1", "ns/g-1@n2"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := names(tc.lws, tc.pods...)
			if !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Fatalf("units %v, want %v", got, tc.want)
			}
		})
	}
	if u := shareDonorUnits(false, []corev1.Pod{unitPod("a", "n1", 2, true, false)}, annotated); u[0].Pods[0].GPUs != 2 {
		t.Fatalf("a unit counts its pod's requested GPUs, got %d", u[0].Pods[0].GPUs)
	}
}

// The node picture a plan may use: one accelerator, no cordoned node, and the
// GPUs already promised or held for a wake taken from the nodes with the most
// free first.
func TestShareFreeNodes(t *testing.T) {
	snap := map[string]decision.NodeGPU{
		"a": {Accelerator: "A100", Capacity: 8, Used: 2},
		"b": {Accelerator: "A100", Capacity: 8, Used: 4},
		"c": {Accelerator: "A100", Capacity: 8, Unschedulable: true},
		"h": {Accelerator: "H100", Capacity: 8},
	}
	got := shareFreeNodes(snap, "A100", 3)
	if len(got) != 2 {
		t.Fatalf("want nodes a and b only, got %v", got)
	}
	// Free 6 and 4; three reserved come off the larger first: 6->5->4, then a tie at 4 breaks by name.
	if got["a"].Free != 3 || got["b"].Free != 4 {
		t.Fatalf("free after the reserve: a=%d b=%d, want 3 and 4", got["a"].Free, got["b"].Free)
	}
	if all := shareFreeNodes(snap, "A100", 100); all["a"].Free != 0 || all["b"].Free != 0 {
		t.Fatalf("a reserve larger than every free GPU empties them: %v", all)
	}
}

// A node-planned transfer keeps its check across a restart: the mark says
// "planned", the restore rebuilds PlannedPods from the marked pods, and a
// donor that then loses another pod still ends wrong-pod instead of raising
// the receiver.
func TestUtilizationSharePlannedMarkSurvivesARestart(t *testing.T) {
	nf := newNodeFleet(t)
	if got := nf.untilMarked(t, true); !slices.Equal(got, []string{"A-v-5", "A-v-6"}) {
		t.Fatalf("setup: marked %v", got)
	}
	for _, p := range markedPods(t, nf.c) {
		var m transferMark
		if err := json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m); err != nil || !m.Planned {
			t.Fatalf("pod %s: mark does not say planned: %q", p.Name, p.Annotations[utilizationShareTransferAnnotation])
		}
	}

	restarted := newShareEngine(t, nf.f, nf.c, nf.se.clock)
	restarted.e.utilizationShare.nodes = &decision.NodeGPUStore{}
	nf.se = restarted
	nf.cycle(true) // restores from the marks
	nf.deletePods(t, "A-v-0", "A-v-1")
	nf.f.current["A"] -= 2
	// Not raised: held at its 5, or -- the transfer gone and the ledger quiet
	// -- left to today's optimizer.
	if b, ok := nf.cycle(true)["ns/B-v"]; ok && b.Target != 5 {
		t.Fatalf("B = %d after a restart and a wrong pod, want 5", b.Target)
	}
}

// annotated treats any transfer annotation as another transfer's.
func annotated(p *corev1.Pod) bool {
	_, ok := p.Annotations[utilizationShareTransferAnnotation]
	return ok
}
