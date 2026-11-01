package allocation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// A namespace allowance that lists a family key (H100) under a cluster quota
// that lists only products of it. The cluster bound on the family key is the
// sum of those products, whichever GPU names the cycle's workloads declare. It
// used to be 0 unless some workload named the bare family, so the same
// configuration planned differently from cycle to cycle. Every replica below
// is a whole 8-GPU node.

const famA100 = "NVIDIA-A100-SXM4-80GB"

// familyUnderProducts is a cluster quota of two 8-GPU products of H100 and a
// namespace quota that lists only the family: unlimited in x, 12 in y, and
// nothing in w.
func familyUnderProducts(t *testing.T) []*ResourceConstraints {
	t.Helper()
	return []*ResourceConstraints{
		famClusterQuota(t, map[string]int{famHBM3: 8, famPCIe: 8}),
		famNamespaceQuota(t, map[string]map[string]int{"x": {"H100": -1}, "y": {"H100": 12}, "w": {}}),
	}
}

// fkNamespaceQuota is a namespace-scope quota called name, holding the
// namespaces of quotas and excluding exclude; every one of them is active.
func fkNamespaceQuota(t *testing.T, name string, quotas map[string]map[string]int, exclude ...string) *ResourceConstraints {
	t.Helper()
	active := map[string]map[string]int{}
	for ns := range quotas {
		active[ns] = map[string]int{}
	}
	for _, ns := range exclude {
		active[ns] = map[string]int{}
	}
	rc, err := NewDefaultLimiter(name, NewQuotaInventory(config.QuotaLimiterConfig{
		Name: name, Type: "quota", Scope: config.QuotaScopeNamespace, NamespaceQuotas: quotas, Exclude: exclude,
	})).ComputeConstraints(context.Background(), map[string]int{}, active)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

// fkTargets runs one pass, with rescale on for every scope when rescale is set.
func fkTargets(constraints []*ResourceConstraints, rescale bool, reqs ...ModelScalingRequest) map[string]int {
	o := NewGreedyByScoreOptimizer()
	if rescale {
		o.Rescale = RescaleFlags{Cluster: true, ByNamespace: map[string]bool{"x": true, "y": true, "z": true, "e": true, "w": true}}
	}
	out := map[string]int{}
	for _, d := range o.Optimize(context.Background(), reqs, constraints) {
		out[d.VariantName] = d.TargetReplicas
	}
	return out
}

// fkReadings publishes headroom into a fresh store and reads it back for one
// namespace, per accelerator.
func fkReadings(free map[string]map[string]int, cluster map[string]int, namespace string, accs ...string) map[string]int {
	now := time.Now()
	s := decision.NewHeadroomStore()
	s.PublishBounds(free, cluster, 0, now)
	out := map[string]int{}
	for _, acc := range accs {
		v, state := s.Reading(namespace, acc, 0, time.Minute, now)
		if state != decision.HeadroomBounded {
			v = -1
		}
		out[acc] = v
	}
	return out
}

func TestNamespaceFamilyKeyBoundIgnoresDeclaredNames(t *testing.T) {
	c := familyUnderProducts(t)
	for _, declared := range [][]string{nil, {famHBM3}, {famPCIe}, {"H100"}, {famHBM3, famPCIe}, {famHBM3, "H100"}, {famSXM5}} {
		for ns, want := range map[string]int{"x": 16, "y": 12} {
			budgets, closed := gpuBudgets(c, ns, declared...)
			if !closed || budgets["H100"] != want {
				t.Errorf("%s, declared %v: budgets = %v (closed %v), want H100: %d", ns, declared, budgets, closed, want)
			}
		}
	}
}

func TestNamespaceFamilyKeyFitsWholeNodesOfClusterProducts(t *testing.T) {
	c := familyUnderProducts(t)
	for _, tc := range []struct {
		ns     string
		demand map[string]int
		fits   bool
	}{
		{"x", map[string]int{famHBM3: 8}, true},
		{"x", map[string]int{famPCIe: 8}, true},
		{"x", map[string]int{famHBM3: 8, famPCIe: 8}, true},
		{"x", map[string]int{famHBM3: 16}, false}, // the product's own 8
		{"x", map[string]int{"H100": 16}, true},
		{"x", map[string]int{"H100": 24}, false},
		{"x", map[string]int{famSXM5: 8}, false}, // a product the cluster quota does not list
		{"y", map[string]int{famHBM3: 8}, true},
		{"y", map[string]int{famHBM3: 8, famPCIe: 8}, false}, // the namespace's 12
		{"y", map[string]int{"H100": 16}, false},
	} {
		if got := FitsGPUBudget(c, tc.ns, tc.demand); got != tc.fits {
			t.Errorf("%s %v: fits = %v, want %v", tc.ns, tc.demand, got, tc.fits)
		}
	}
}

// A family the cluster quota covers with no product at all is denied, however
// the namespace lists it.
func TestNamespaceFamilyWithNoClusterProductIsDenied(t *testing.T) {
	c := []*ResourceConstraints{
		famClusterQuota(t, map[string]int{famA100: 16}),
		famNamespaceQuota(t, map[string]map[string]int{"x": {"H100": -1}, "y": {"H100": 8, famSXM5: -1}}),
	}
	for _, ns := range []string{"x", "y"} {
		for _, acc := range []string{"H100", famHBM3, famSXM5} {
			if FitsGPUBudget(c, ns, map[string]int{acc: 8}) {
				t.Errorf("%s %s: 8 fit under a cluster quota of A100 only, want denied", ns, acc)
			}
		}
		if h := namespaceHeadroom(c)[ns]["H100"]; h != 0 {
			t.Errorf("%s: H100 headroom = %d, want 0", ns, h)
		}
	}
	got := fkTargets(c, false, famWanting("x", "mx", "H100", 2, 8), famWanting("y", "my", famSXM5, 2, 8))
	if got["mx-v"] != 0 || got["my-v"] != 0 {
		t.Errorf("targets = %v, want nothing placed", got)
	}
}

// The optimizer sizes 8-GPU replicas alike whether or not another workload in
// the cycle names the bare family. w is allowed nothing, so its model only
// adds the name.
func TestNamespaceFamilyKeyPlansAlikeWhateverNamesTheCycleHolds(t *testing.T) {
	for _, rescale := range []bool{false, true} {
		for _, withBareFamily := range []bool{false, true} {
			name := fmt.Sprintf("rescale=%v bare H100 declared=%v", rescale, withBareFamily)
			reqs := []ModelScalingRequest{
				famWanting("x", "mh", famHBM3, 3, 8),
				famWanting("x", "mp", famPCIe, 3, 8),
				famWanting("y", "yh", famHBM3, 3, 8),
			}
			if withBareFamily {
				reqs = append(reqs, famWanting("w", "mw", "H100", 1, 8))
			}
			got := fkTargets(familyUnderProducts(t), rescale, reqs...)
			if got["mw-v"] != 0 {
				t.Errorf("%s: w took %d replicas with no allowance, want 0", name, got["mw-v"])
			}
			if got["mh-v"]+got["yh-v"] != 1 || got["mp-v"] != 1 {
				t.Errorf("%s: x HBM3=%d x PCIe=%d y HBM3=%d, want one HBM3 replica (8 of 8) and one PCIe replica (8 of 8)",
					name, got["mh-v"], got["mp-v"], got["yh-v"])
			}
		}
	}
}

// The warm pool reads a namespace allowed the family {H100: -1} at each
// product's own cluster figure: 8 HBM3, 8 PCIe, 0 SXM5, not the family's 16 on
// each. A promise to another namespace of the HBM3 GPUs leaves it none of
// them, as FitsGPUBudget says, from the constraints and after a pass.
func TestNamespaceFamilyHeadroomReadsEachProduct(t *testing.T) {
	products := []string{famHBM3, famPCIe, famSXM5}
	build := func(aCap int) []*ResourceConstraints {
		return []*ResourceConstraints{
			famClusterQuota(t, map[string]int{famHBM3: 8, famPCIe: 8}),
			fkNamespaceQuota(t, "ns-quota", map[string]map[string]int{"team-a": {"H100": aCap}, "team-b": {famHBM3: 8}}, "e"),
		}
	}
	promise := map[string]map[string]int{"team-b": {famHBM3: 8}}

	c := build(-1)
	got := fkReadings(namespaceHeadroom(c), clusterHeadroom(c), "team-a", products...)
	if got[famHBM3] != 8 || got[famPCIe] != 8 || got[famSXM5] != 0 {
		t.Errorf("team-a {H100: -1}: headroom = %v, want HBM3 8, PCIe 8, SXM5 0", got)
	}
	w := WithholdPromised(c, promise)
	got = fkReadings(namespaceHeadroom(w), clusterHeadroom(w), "team-a", products...)
	if got[famHBM3] != 0 || got[famPCIe] != 8 {
		t.Errorf("team-a after team-b's HBM3 promise: headroom = %v, want HBM3 0, PCIe 8", got)
	}
	if FitsGPUBudget(w, "team-a", map[string]int{famHBM3: 8}) {
		t.Errorf("team-a after team-b's HBM3 promise: 8 HBM3 fit, want refused (the headroom must agree)")
	}

	c = build(12)
	h := namespaceHeadroom(c)["team-a"]
	if h["H100"] != 12 || h[famHBM3] != 8 || h[famPCIe] != 8 {
		t.Errorf("team-a {H100: 12}: headroom = %v, want H100 12, HBM3 8, PCIe 8", h)
	}

	// The same after a pass that orders nothing, republished with the promise.
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	o := NewGreedyByScoreOptimizer()
	o.Optimize(context.Background(), []ModelScalingRequest{famWanting("e", "me", "H100", 0, 8)}, build(-1))
	read := func() map[string]int {
		now := time.Now()
		out := map[string]int{}
		for _, acc := range products {
			out[acc], _ = decision.GPUHeadroomReading("team-a", acc, 0, time.Minute, now)
		}
		return out
	}
	if got := read(); got[famHBM3] != 8 || got[famPCIe] != 8 || got[famSXM5] != 0 {
		t.Errorf("team-a after a pass: headroom = %v, want HBM3 8, PCIe 8, SXM5 0", got)
	}
	o.RepublishHeadroom(nil, map[string]map[string]decision.PromisedHoles{"team-b": {famHBM3: {Unplaced: 8}}}, time.Now())
	if got := read(); got[famHBM3] != 0 || got[famPCIe] != 8 {
		t.Errorf("team-a after a pass and team-b's HBM3 promise: headroom = %v, want HBM3 0, PCIe 8", got)
	}
}

// Another namespace's allowance key must not change what a namespace the
// quotas exclude (e), or one no quota names (w), reads of the cluster: under a
// cluster bound of {H100: 16} (or a physical pool keyed H100), team-a's
// {...HBM3: 8} once made the cluster figure list HBM3, and then every other
// H100 product read 0 there.
func TestOpenNamespaceHeadroomIgnoresAnotherNamespacesKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster *ResourceConstraints
		want    int
	}{
		{"cluster quota {H100: 16}", famClusterQuota(t, map[string]int{"H100": 16}), 16},
		{"physical 16 HBM3 + 8 SXM5", famPhysical(t, map[string]int{famHBM3: 16, famSXM5: 8}), 24},
	} {
		c := []*ResourceConstraints{tc.cluster, fkNamespaceQuota(t, "ns-quota", map[string]map[string]int{"team-a": {famHBM3: 8}}, "e")}
		for _, ns := range []string{"e", "w"} {
			got := fkReadings(namespaceHeadroom(c), clusterHeadroom(c), ns, famPCIe, famSXM5)
			if got[famPCIe] != tc.want || got[famSXM5] != tc.want {
				t.Errorf("%s, %s: headroom = %v, want %d on PCIe and SXM5", tc.name, ns, got, tc.want)
			}
		}

		decision.DefaultHeadroom.Reset()
		t.Cleanup(decision.DefaultHeadroom.Reset)
		fkTargets(c, false, famWanting("e", "me", "H100", 0, 8))
		now := time.Now()
		for _, acc := range []string{famPCIe, famSXM5} {
			if v, state := decision.GPUHeadroomReading("w", acc, 0, time.Minute, now); state != decision.HeadroomBounded || v != tc.want {
				t.Errorf("%s, w after a pass: %s headroom = %d (%v), want %d", tc.name, acc, v, state, tc.want)
			}
		}
		if got := fkTargets(c, false, famWanting("e", "me", famPCIe, 3, 8)); got["me-v"] != tc.want/8 {
			t.Errorf("%s: e placed %d PCIe replicas, want %d", tc.name, got["me-v"], tc.want/8)
		}
	}
}

// With no cluster bound in force the pass publishes no cluster figure, and a
// namespace no quota holds reads unbounded, as before.
func TestNoClusterBoundLeavesOpenNamespacesUnbounded(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	c := []*ResourceConstraints{famNamespaceQuota(t, map[string]map[string]int{"x": {"H100": 8}})}
	fkTargets(c, false, famWanting("x", "mx", famHBM3, 1, 8))
	if _, state := decision.GPUHeadroomReading("w", famHBM3, 0, time.Minute, time.Now()); state != decision.HeadroomUnbounded {
		t.Errorf("w after a pass with no cluster bound: %v, want unbounded", state)
	}
}

func fkShareReq(id, ns, acc string) ModelScalingRequest {
	v := id + "-v"
	req := withSatEntry(&satEntryFixture{
		ModelID: id, Namespace: ns, TotalDemand: 20000,
		VariantCapacities: []vcFixture{{VariantName: v, AcceleratorName: acc, Cost: 5, ReplicaCount: 0, PerReplicaCapacity: 1000}},
	}, ModelScalingRequest{
		ModelID: id, Namespace: ns,
		VariantStates: []domain.VariantReplicaState{{VariantName: v, CurrentReplicas: 0, GPUsPerReplica: 8, MinReplicas: ptrInt(0)}},
	})
	req.CompositeSignal.ScaleUpThreshold = 0.8
	return req
}

// The utilization share splits a cluster key's free GPUs between the groups
// that draw on it. A namespace's family key is not one of the cluster's keys:
// split first, it took team-a's PCIe group's share of the free PCIe node.
func TestShareGroupsDoNotSplitANamespaceFamilyKey(t *testing.T) {
	for _, aCap := range []int{12, 16} {
		c := []*ResourceConstraints{
			famClusterQuota(t, map[string]int{famHBM3: 8, famPCIe: 8}),
			fkNamespaceQuota(t, "ns-quota", map[string]map[string]int{"team-a": {"H100": aCap}, "team-b": {famHBM3: 8}}, "e"),
		}
		groups := BuildShareGroups([]ModelScalingRequest{
			fkShareReq("ma", "team-a", famHBM3),
			fkShareReq("mp", "team-a", famPCIe),
			fkShareReq("mb", "team-b", famHBM3),
			fkShareReq("me", "e", famHBM3),
		}, c, ShareGroupOptions{ClusterIsQuota: true})
		found := false
		for _, g := range groups {
			if g.Scope == "team-a" && g.AcceleratorType == famPCIe {
				found = true
				if g.ClusterFree != 8 {
					t.Errorf("team-a {H100: %d}: PCIe group ClusterFree = %d, want 8 (the PCIe node no other group draws on)", aCap, g.ClusterFree)
				}
			}
		}
		if !found {
			t.Errorf("team-a {H100: %d}: no PCIe group in %+v", aCap, groups)
		}
	}
}

// Two cluster bounds that list the same products in different numbers: only
// the GPUs of each product that fit under both are free, 8 HBM3 and 8 PCIe.
// The family used to read the smaller of the two sums, 24, and a workload
// naming the bare H100 took three nodes beside nothing else.
func TestClusterFamilyIsTheSumOfWhatFitsUnderEveryBound(t *testing.T) {
	q := func(quotas map[string]int) *ResourceConstraints { return famClusterQuota(t, quotas) }
	for _, tc := range []struct {
		name   string
		bounds []*ResourceConstraints
		want   int
	}{
		{"products swapped", []*ResourceConstraints{q(map[string]int{famHBM3: 16, famPCIe: 8}), q(map[string]int{famHBM3: 8, famPCIe: 16})}, 16},
		{"a family key beside products", []*ResourceConstraints{q(map[string]int{"H100": 24, famHBM3: 8}), q(map[string]int{famHBM3: 16, famPCIe: 8})}, 16},
		{"a family key bounds the sum", []*ResourceConstraints{q(map[string]int{"H100": 8}), q(map[string]int{famHBM3: 16, famPCIe: 8})}, 8},
		{"family keys alone", []*ResourceConstraints{q(map[string]int{"H100": 24}), q(map[string]int{"h100": 16, famHBM3: 8})}, 16},
		{"one product unlimited in each", []*ResourceConstraints{q(map[string]int{famHBM3: -1, famPCIe: 8}), q(map[string]int{famHBM3: 8, famPCIe: -1})}, 16},
	} {
		if got := mergeConstraints(tc.bounds, "H100")["H100"]; got != tc.want {
			t.Errorf("%s: merged H100 = %d, want %d", tc.name, got, tc.want)
		}
		if !FitsGPUBudget(tc.bounds, "w", map[string]int{"H100": tc.want}) || FitsGPUBudget(tc.bounds, "w", map[string]int{"H100": tc.want + 8}) {
			t.Errorf("%s: a bare H100 is not bounded at %d", tc.name, tc.want)
		}
		for _, rescale := range []bool{false, true} {
			got := fkTargets(tc.bounds, rescale, famWanting("w", "bare", "H100", 4, 8), famWanting("w", "hbm", famHBM3, 4, 8))
			if gpus := 8 * (got["bare-v"] + got["hbm-v"]); gpus > tc.want || got["hbm-v"] > 1 {
				t.Errorf("%s, rescale=%v: bare H100 %d and HBM3 %d replicas, want at most %d GPUs and one HBM3 node", tc.name, rescale, got["bare-v"], got["hbm-v"], tc.want)
			}
		}
	}
}

// A namespace that may use only SXM5 under a cluster quota that denies SXM5
// gets no H100 of any kind, whatever name its workload uses; the cluster's
// HBM3 and PCIe GPUs are not its to take.
func TestBareFamilyInANamespaceIsBoundedByItsProducts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster map[string]int
		allow   map[string]int
		want    int
	}{
		{"only SXM5, which the cluster denies", map[string]int{famHBM3: 8, famPCIe: 8}, map[string]int{famSXM5: -1}, 0},
		{"only SXM5, which the cluster allows 8", map[string]int{famHBM3: 8, famSXM5: 8}, map[string]int{famSXM5: -1}, 8},
		{"SXM5 and HBM3, at 8 each", map[string]int{famHBM3: 8, famPCIe: 8}, map[string]int{famSXM5: 8, famHBM3: 8}, 8},
		{"the family itself", map[string]int{famHBM3: 8, famPCIe: 8}, map[string]int{"H100": -1, famSXM5: 8}, 16},
	} {
		c := []*ResourceConstraints{famClusterQuota(t, tc.cluster), famNamespaceQuota(t, map[string]map[string]int{"x": tc.allow})}
		if !FitsGPUBudget(c, "x", map[string]int{"H100": tc.want}) || FitsGPUBudget(c, "x", map[string]int{"H100": tc.want + 8}) {
			t.Errorf("%s: a bare H100 in x is not bounded at %d", tc.name, tc.want)
		}
		for _, rescale := range []bool{false, true} {
			if got := fkTargets(c, rescale, famWanting("x", "m", "H100", 3, 8)); 8*got["m-v"] != tc.want {
				t.Errorf("%s, rescale=%v: bare H100 got %d replicas, want %d GPUs", tc.name, rescale, got["m-v"], tc.want)
			}
		}
	}

	// With no cluster bound, only SXM5 at -1 is no bound on a bare H100.
	c := []*ResourceConstraints{famNamespaceQuota(t, map[string]map[string]int{"x": {famSXM5: -1}})}
	if b, _ := gpuBudgets(c, "x", "H100"); b["H100"] != -1 {
		t.Errorf("only SXM5 at -1, no cluster bound: budgets = %v, want H100 unlimited (-1)", b)
	}
	if got := fkTargets(c, false, famWanting("x", "m", "H100", 3, 8)); got["m-v"] != 3 {
		t.Errorf("only SXM5 at -1, no cluster bound: bare H100 got %d replicas, want all 3", got["m-v"])
	}
}

// Two namespace quotas hold x: one lists the family, the other two products.
// x may use only what both allow: HBM3 up to 8, PCIe up to 4, 12 in all.
func TestTwoNamespaceQuotasListingOverlappingKeys(t *testing.T) {
	c := []*ResourceConstraints{
		famClusterQuota(t, map[string]int{famHBM3: 8, famPCIe: 8}),
		fkNamespaceQuota(t, "family-quota", map[string]map[string]int{"x": {"H100": -1}}),
		fkNamespaceQuota(t, "product-quota", map[string]map[string]int{"x": {famHBM3: 8, famPCIe: 4}}),
	}
	for _, tc := range []struct {
		demand map[string]int
		fits   bool
	}{
		{map[string]int{famHBM3: 8}, true},
		{map[string]int{famPCIe: 8}, false},
		{map[string]int{famSXM5: 8}, false},
		{map[string]int{"H100": 12}, true},
		{map[string]int{"H100": 16}, false},
	} {
		if got := FitsGPUBudget(c, "x", tc.demand); got != tc.fits {
			t.Errorf("x %v: fits = %v, want %v", tc.demand, got, tc.fits)
		}
	}
	h := namespaceHeadroom(c)["x"]
	if h[famHBM3] != 8 || h[famPCIe] != 4 || h["H100"] != 12 {
		t.Errorf("x headroom = %v, want HBM3 8, PCIe 4, H100 12", h)
	}
	for _, rescale := range []bool{false, true} {
		got := fkTargets(c, rescale, famWanting("x", "mh", famHBM3, 3, 8), famWanting("x", "mp", famPCIe, 3, 8))
		if got["mh-v"] != 1 || got["mp-v"] != 0 {
			t.Errorf("rescale=%v: HBM3 %d, PCIe %d replicas, want 1 and 0", rescale, got["mh-v"], got["mp-v"])
		}
	}
}

// With no cluster quota, the physical pool keyed by family bounds a namespace
// family key and a namespace product key alike.
func TestPhysicalPoolBoundsNamespaceFamilyAndProductKeys(t *testing.T) {
	c := []*ResourceConstraints{
		famPhysical(t, map[string]int{famHBM3: 16, famPCIe: 8}),
		famNamespaceQuota(t, map[string]map[string]int{"x": {"H100": 12}, "y": {famHBM3: 8}}),
	}
	for ns, want := range map[string]map[string]int{"x": {"H100": 12}, "y": {famHBM3: 8}} {
		for key, v := range want {
			if b, _ := gpuBudgets(c, ns); b[key] != v {
				t.Errorf("%s: budgets = %v, want %s: %d", ns, b, key, v)
			}
		}
	}
	for _, rescale := range []bool{false, true} {
		got := fkTargets(c, rescale, famWanting("x", "mx", famHBM3, 3, 8), famWanting("y", "my", famHBM3, 3, 8))
		if got["mx-v"] != 1 || got["my-v"] != 1 {
			t.Errorf("rescale=%v: x %d, y %d replicas, want 1 each", rescale, got["mx-v"], got["my-v"])
		}
	}
}

// A cluster quota and a namespace allowance that spell one GPU in different
// case are one key: one budget, spent once.
func TestNamespaceKeyInAnotherCaseIsTheClusterKey(t *testing.T) {
	const lower = "nvidia-h100-80gb-hbm3"
	for _, tc := range []struct {
		cluster map[string]int
		allow   string
		key     string
		want    int
	}{
		{map[string]int{famHBM3: 8, famPCIe: 8}, lower, famHBM3, 8},
		{map[string]int{"H100": 16}, "h100", "H100", 16},
	} {
		c := []*ResourceConstraints{famClusterQuota(t, tc.cluster), famNamespaceQuota(t, map[string]map[string]int{"x": {tc.allow: -1}})}
		merged := mergeWithAllowanceKeys(c)
		if len(merged) != len(tc.cluster) || merged[tc.key] != tc.want {
			t.Errorf("namespace %s under %v: merged = %v, want the cluster's keys only, %s: %d", tc.allow, tc.cluster, merged, tc.key, tc.want)
		}
		if b, _ := gpuBudgets(c, "x"); b[tc.allow] != tc.want {
			t.Errorf("namespace %s: budgets = %v, want %s: %d", tc.allow, b, tc.allow, tc.want)
		}
		got := fkTargets(c, false, famWanting("x", "a", tc.allow, 3, 8), famWanting("x", "b", tc.key, 3, 8))
		if 8*(got["a-v"]+got["b-v"]) != tc.want {
			t.Errorf("namespace %s: %d + %d replicas, want %d GPUs in all", tc.allow, got["a-v"], got["b-v"], tc.want)
		}
	}
}

func TestHasKeyIgnoresCase(t *testing.T) {
	pools := []map[string]ResourcePool{{famPCIe: {}}, {famHBM3: {}}}
	for name, want := range map[string]bool{
		famHBM3: true, "nvidia-h100-80gb-hbm3": true, "NVIDIA-H100-PCIE-80GB": true,
		"H100": false, famSXM5: false, "": false,
	} {
		if got := hasKey(pools, name); got != want {
			t.Errorf("hasKey(%q) = %v, want %v", name, got, want)
		}
	}
	if hasKey(nil, famHBM3) {
		t.Errorf("hasKey over no allowlist = true, want false")
	}
}

// A namespace allowance of "unknown" adds no budget key: a workload that
// names no accelerator is bounded by its namespace's allowance alone, as
// before, and not denied by the cluster quota that does not list it.
func TestNamespaceUnknownAddsNoClusterKey(t *testing.T) {
	c := []*ResourceConstraints{
		famClusterQuota(t, map[string]int{famHBM3: 8}),
		fkNamespaceQuota(t, "ns-quota", map[string]map[string]int{"x": {"unknown": 8, "H100": -1}}, "e"),
	}
	for _, merged := range []map[string]int{mergeWithAllowanceKeys(c), mergeWithAllowanceKeys(c, "unknown")} {
		if _, ok := merged["unknown"]; ok || len(merged) != 2 {
			t.Errorf("merged = %v, want HBM3 and H100 only", merged)
		}
	}
	if !FitsGPUBudget(c, "x", map[string]int{"unknown": 8}) || FitsGPUBudget(c, "x", map[string]int{"unknown": 16}) {
		t.Errorf("x unknown: not bounded at the namespace's 8")
	}
	if !FitsGPUBudget(c, "e", map[string]int{"unknown": 8}) || FitsGPUBudget(c, "e", map[string]int{"unknown": 16}) {
		t.Errorf("e unknown: not bounded by the one pool it may land on, HBM3's 8")
	}
}

// The warm pool's headroom names no workload at all, so it read every such key
// as 0. A product key under a cluster quota that lists its family and a sibling
// product reads the family's figure, as a workload naming it does.
func TestNamespaceKeyHeadroomReadsTheClusterBound(t *testing.T) {
	got := namespaceHeadroom(familyUnderProducts(t))
	if got["x"]["H100"] != 16 || got["y"]["H100"] != 12 {
		t.Errorf("family key under products: headroom = %v, want x H100: 16, y H100: 12", got)
	}

	c := []*ResourceConstraints{
		famClusterQuota(t, map[string]int{"H100": 16, famPCIe: 8}),
		famNamespaceQuota(t, map[string]map[string]int{"x": {famHBM3: 8}}),
	}
	got = namespaceHeadroom(c)
	if got["x"][famHBM3] != 8 {
		t.Errorf("product key under a family and a sibling: headroom = %v, want x %s: 8", got, famHBM3)
	}
	if !FitsGPUBudget(c, "x", map[string]int{famHBM3: 8}) {
		t.Errorf("x %s: 8 refused, want fits (the family's 16, the namespace's 8)", famHBM3)
	}
}
