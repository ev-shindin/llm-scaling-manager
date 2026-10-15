package allocation

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// benchNodes is a large GPU cluster: n nodes of 8 H100s, each carrying the
// given number of labels (the flavor's pool label among them) and one
// tolerated taint.
func benchNodes(n, labels int) map[string]decision.NodeGPU {
	out := make(map[string]decision.NodeGPU, n)
	for i := range n {
		l := map[string]string{"pool": "h100", "zone": fmt.Sprintf("z%d", i%4), "rdma": "true"}
		for j := len(l); j < labels; j++ {
			l[fmt.Sprintf("example.com/label-%02d", j)] = fmt.Sprintf("value-%d-%d", i%7, j)
		}
		out[fmt.Sprintf("node-%05d", i)] = decision.NodeGPU{
			Accelerator: borrowH100, Capacity: 8, Used: 8 * (i % 2), Labels: l,
			Taints: []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}},
		}
	}
	return out
}

// benchTemplates is a pod template that pins RDMA nodes in two zones.
var benchTemplates = []domain.PodPlacement{{
	NodeSelector: map[string]string{"rdma": "true"},
	RequiredNodeAffinity: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"z0", "z1"}},
			{Key: "example.com/label-10", Operator: corev1.NodeSelectorOpExists},
		},
	}}},
	Tolerations: []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gpu", Effect: corev1.TaintEffectNoSchedule}},
}}

// A clone shares the nodes and the eligibility cache but not the debits: what
// one view places is invisible to the other, and a probe debits nothing.
func TestNodeViewCloneSharesNodesNotDebits(t *testing.T) {
	v := NewNodeView(benchNodes(4, 5)) // node-00000 and node-00002 are free
	c := v.Clone()
	if !c.place("H100", []int{8}, nil, nil, true) {
		t.Fatal("the clone placed nothing")
	}
	if got := v.Snapshot()["node-00000"].Used; got != 0 {
		t.Fatalf("the original sees the clone's debit: node-00000 used %d", got)
	}
	if got := c.Snapshot()["node-00000"].Used; got != 8 {
		t.Fatalf("the clone lost its debit: node-00000 used %d", got)
	}
	if !v.place("H100", []int{8, 8}, nil, nil, false) || v.place("H100", []int{8, 8, 8}, nil, nil, false) {
		t.Fatal("the original's free nodes are wrong")
	}
	if got := v.Snapshot()["node-00002"].Used; got != 0 {
		t.Fatalf("a probe debited: node-00002 used %d", got)
	}
	if !c.place("H100", []int{8}, nil, nil, true) || c.place("H100", []int{8}, nil, nil, false) {
		t.Fatal("after two commits the clone must have no free node left")
	}
}

// BenchmarkBorrowPlacementCycle is one optimizer pass's borrowed placement on
// a 2000-node cluster with 80 labels per node: 50 models, each saving the
// state before a tentative unit (a NodeView clone) and placing 4 borrowed
// 8-GPU replicas on the cohort's flavor nodes; every third unit is rolled back.
func BenchmarkBorrowPlacementCycle(b *testing.B) {
	snap := benchNodes(2000, 80)
	c := borrowQuota(1<<20, nsBorrow{ns: "b", guaranteed: 0, used: 0, cap: -1})
	c.SharedPools[0].NodeLabels = map[string]string{"pool": "h100"}
	c.SharedPools[0].Tolerations = benchTemplates[0].Tolerations
	constraints := []*ResourceConstraints{c}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		bs := newBorrowState(constraints, NewNodeView(snap))
		for m := range 50 {
			p := bs.newModelPlacer("b", fmt.Sprintf("m%d", m), map[string]int{"H100": 0}, map[string]int{})
			saved := bs.save(p.guaranteed)
			for range 4 {
				if !p.placeReplica("v", "H100", 8, []int{8}, benchTemplates, 1) {
					b.Fatal("a replica did not place")
				}
			}
			if m%3 == 0 {
				bs.restore(saved, p.guaranteed)
			}
		}
	}
}
