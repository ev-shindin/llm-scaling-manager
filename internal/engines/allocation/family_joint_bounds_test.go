package allocation

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
)

// Bounds that only hold together: several cluster quotas that each list
// products of one family, and a namespace that lists the family and denies one
// of its products. Every replica below is a whole 8-GPU node.

// jbPhysical is the physical inventory of whole 8-GPU nodes, installed GPUs per
// product.
func jbPhysical(t *testing.T, installed map[string]int) *ResourceConstraints {
	t.Helper()
	nodes := map[string]map[string]gpunodes.AcceleratorModelInfo{}
	for product, count := range installed {
		for i := 0; i < count/8; i++ {
			nodes[fmt.Sprintf("node-%s-%d", product, i)] = map[string]gpunodes.AcceleratorModelInfo{product: {Count: 8}}
		}
	}
	rc, err := NewDefaultLimiter("gpu-limiter", NewTypeInventory("gpu-limiter", &mockDiscovery{inventory: nodes})).
		ComputeConstraints(context.Background(), map[string]int{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

// jbClusterQuota is a cluster-scope quota called name, with used GPUs already
// held under it.
func jbClusterQuota(t *testing.T, name string, quotas, used map[string]int) *ResourceConstraints {
	t.Helper()
	if used == nil {
		used = map[string]int{}
	}
	rc, err := NewDefaultLimiter(name, NewQuotaInventory(config.QuotaLimiterConfig{
		Name: name, Type: "quota", Scope: config.QuotaScopeCluster, ClusterQuotas: quotas,
	})).ComputeConstraints(context.Background(), used, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

// jbDecisions runs one pass, with rescale on for every scope when rescale is
// set.
func jbDecisions(constraints []*ResourceConstraints, rescale bool, reqs ...ModelScalingRequest) map[string]domain.VariantDecision {
	o := NewGreedyByScoreOptimizer()
	if rescale {
		o.Rescale = RescaleFlags{Cluster: true, ByNamespace: map[string]bool{"x": true}}
	}
	out := map[string]domain.VariantDecision{}
	for _, d := range o.Optimize(context.Background(), reqs, constraints) {
		out[d.VariantName] = d
	}
	return out
}

// Two cluster quotas that swap their products: cq0 allows 32 HBM3 and 8 PCIe,
// cq1 8 HBM3 and 32 PCIe. Each alone allows 40 H100s, as the 40 installed do,
// but only 8 of each product fit under both: 16. The quotas together held the
// decision back, and LimitedBy names them, not the physical pool.
func TestJointClusterQuotaBoundNamesTheQuotas(t *testing.T) {
	build := func(installed map[string]int) []*ResourceConstraints {
		return []*ResourceConstraints{
			jbPhysical(t, installed),
			jbClusterQuota(t, "cq0", map[string]int{famHBM3: 32, famPCIe: 8}, nil),
			jbClusterQuota(t, "cq1", map[string]int{famHBM3: 8, famPCIe: 32}, nil),
		}
	}
	for _, tc := range []struct {
		name      string
		installed map[string]int
		by        string
		also      []string
	}{
		{"40 installed: the quotas bind together", map[string]int{famHBM3: 24, famPCIe: 16}, "cq0", []string{"cq1"}},
		{"16 installed: the physical pool binds alone", map[string]int{famHBM3: 8, famPCIe: 8}, PhysicalLimiterName, nil},
	} {
		c := build(tc.installed)
		if got := bindingProviders(c, "x", "H100"); !slices.Equal(got, append([]string{tc.by}, tc.also...)) {
			t.Errorf("%s: binding providers = %v, want %s then %v", tc.name, got, tc.by, tc.also)
		}
		for _, rescale := range []bool{false, true} {
			d := jbDecisions(c, rescale, famWanting("x", "bare", "H100", 6, 8))["bare-v"]
			if d.TargetReplicas != 2 || !d.WasLimited || d.LimitedBy != tc.by || !slices.Equal(d.LimitedByAlso, tc.also) {
				t.Errorf("%s, rescale=%v: target %d, limited %v by %q also %v; want 2, limited by %q also %v",
					tc.name, rescale, d.TargetReplicas, d.WasLimited, d.LimitedBy, d.LimitedByAlso, tc.by, tc.also)
			}
		}
	}
}

// A namespace that lists the family and denies one of its products may use
// only the other products. A workload that names the bare family is bounded
// by those, at the cluster's figure for each, not by the whole family.
func TestBareFamilyInANamespaceThatDeniesAProduct(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster []*ResourceConstraints
		allow   map[string]int
		want    int
	}{
		{
			"the family, but no SXM5; the cluster has 8 HBM3 and 24 SXM5",
			[]*ResourceConstraints{jbClusterQuota(t, "cq0", map[string]int{famHBM3: 8, famSXM5: 24}, nil)},
			map[string]int{"H100": -1, famSXM5: 0}, 8,
		},
		{
			"16 of the family, but no HBM3, the only product the quota lists",
			[]*ResourceConstraints{
				jbPhysical(t, map[string]int{famPCIe: 24}),
				jbClusterQuota(t, "cq0", map[string]int{famHBM3: 16, famA100: 24}, nil),
			},
			map[string]int{"H100": 16, famHBM3: 0}, 0,
		},
		{
			"8 of the family, but no SXM5; the cluster has 16 HBM3 and 24 SXM5",
			[]*ResourceConstraints{jbClusterQuota(t, "cq0", map[string]int{famHBM3: 16, famSXM5: 24}, nil)},
			map[string]int{"H100": 8, famSXM5: 0}, 8,
		},
		{
			"the family, and SXM5 at 8, which the cluster does not list",
			[]*ResourceConstraints{jbClusterQuota(t, "cq0", map[string]int{famHBM3: 8, famPCIe: 8}, nil)},
			map[string]int{"H100": -1, famSXM5: 8}, 16,
		},
	} {
		c := append(slices.Clone(tc.cluster), famNamespaceQuota(t, map[string]map[string]int{"x": tc.allow}))
		if !FitsGPUBudget(c, "x", map[string]int{"H100": tc.want}) || FitsGPUBudget(c, "x", map[string]int{"H100": tc.want + 8}) {
			t.Errorf("%s: a bare H100 in x does not fit exactly %d GPUs", tc.name, tc.want)
		}
		if got := namespaceHeadroom(c)["x"]["H100"]; got != tc.want {
			t.Errorf("%s: x's H100 headroom = %d, want %d", tc.name, got, tc.want)
		}
		for _, rescale := range []bool{false, true} {
			if got := fkTargets(c, rescale, famWanting("x", "m", "H100", 3, 8)); 8*got["m-v"] != tc.want {
				t.Errorf("%s, rescale=%v: bare H100 got %d replicas, want %d GPUs", tc.name, rescale, got["m-v"], tc.want)
			}
		}
	}
}

// A share's promise of 8 H100s to namespace a, which may use the family, under
// a cluster quota of 8 HBM3 and 8 PCIe. The warm pool of a namespace the
// quotas exclude reads each product at what is left of the family: 8 each
// with 8 promised, 0 each with 16 promised. Taking either 8 leaves the
// promise its 8, on the other product, and the next reading of the other is 0.
func TestClusterHeadroomHoldsAFamilyPromise(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	build := func(usedHBM3 int) []*ResourceConstraints {
		return []*ResourceConstraints{
			jbClusterQuota(t, "cq0", map[string]int{famHBM3: 8, famPCIe: 8}, map[string]int{famHBM3: usedHBM3}),
			fkNamespaceQuota(t, "ns-quota", map[string]map[string]int{"a": {"H100": -1}}, "e"),
		}
	}
	read := func(c []*ResourceConstraints, promised int) map[string]int {
		o := NewGreedyByScoreOptimizer()
		o.Optimize(context.Background(), []ModelScalingRequest{famWanting("a", "ma", famHBM3, 0, 8), famWanting("e", "me", famPCIe, 0, 8)}, c)
		o.RepublishHeadroom(nil, map[string]map[string]decision.PromisedHoles{"a": {"H100": {Unplaced: promised}}}, time.Now())
		out := map[string]int{}
		for _, acc := range []string{famHBM3, famPCIe} {
			v, state := decision.GPUHeadroomReading("e", acc, 0, time.Minute, time.Now())
			if state != decision.HeadroomBounded {
				v = -1
			}
			out[acc] = v
		}
		return out
	}

	if got := read(build(0), 8); got[famHBM3] != 8 || got[famPCIe] != 8 {
		t.Errorf("8 H100 promised: e reads %v, want 8 HBM3 and 8 PCIe, either one alone", got)
	}
	if got := read(build(0), 16); got[famHBM3] != 0 || got[famPCIe] != 0 {
		t.Errorf("16 H100 promised: e reads %v, want 0 of each", got)
	}
	// e's pool took the 8 HBM3 it read.
	c := build(8)
	if got := read(c, 8); got[famHBM3] != 0 || got[famPCIe] != 0 {
		t.Errorf("8 H100 promised, 8 HBM3 taken: e reads %v, want 0 of each", got)
	}
	if !FitsGPUBudget(c, "a", map[string]int{"H100": 8}) {
		t.Errorf("8 HBM3 taken: the 8 H100 promised to a no longer fit")
	}
}
