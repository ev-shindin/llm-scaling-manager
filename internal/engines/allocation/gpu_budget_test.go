package allocation

import "testing"

const budgetNS = "chat"

// clusterPools builds a single cluster-scoped provider.
func clusterPools(pools map[string]ResourcePool) *ResourceConstraints {
	return &ResourceConstraints{ProviderName: "gpu-limiter", Pools: pools}
}

// nsPools builds a provider carrying both cluster and namespace pools.
func nsPools(cluster map[string]ResourcePool, ns map[string]map[string]ResourcePool) *ResourceConstraints {
	return &ResourceConstraints{ProviderName: "quota-limiter", Pools: cluster, NamespacePools: ns}
}

func TestFitsGPUBudget(t *testing.T) {
	tests := []struct {
		name        string
		constraints []*ResourceConstraints
		demand      map[string]int
		want        bool
	}{
		{
			name:        "no constraints is unknown, not denied",
			constraints: nil,
			demand:      map[string]int{"H100": 8},
			want:        true,
		},
		{
			name:        "demand fits the free pool",
			constraints: []*ResourceConstraints{clusterPools(map[string]ResourcePool{"H100": {Limit: 8, Used: 4}})},
			demand:      map[string]int{"H100": 4},
			want:        true,
		},
		{
			name:        "demand exceeds the free pool by one",
			constraints: []*ResourceConstraints{clusterPools(map[string]ResourcePool{"H100": {Limit: 8, Used: 4}})},
			demand:      map[string]int{"H100": 5},
			want:        false,
		},
		{
			name:        "a fully used pool denies any demand",
			constraints: []*ResourceConstraints{clusterPools(map[string]ResourcePool{"H100": {Limit: 8, Used: 8}})},
			demand:      map[string]int{"H100": 1},
			want:        false,
		},
		{
			name:        "an unknown accelerator type is denied, not treated as unlimited",
			constraints: []*ResourceConstraints{clusterPools(map[string]ResourcePool{"H100": {Limit: 8}})},
			demand:      map[string]int{"A100": 1},
			want:        false,
		},
		{
			name:        "the unlimited sentinel admits any demand",
			constraints: []*ResourceConstraints{clusterPools(map[string]ResourcePool{"H100": {Limit: -1}})},
			demand:      map[string]int{"H100": 512},
			want:        true,
		},
		{
			name: "the most restrictive provider wins",
			constraints: []*ResourceConstraints{
				clusterPools(map[string]ResourcePool{"H100": {Limit: 16, Used: 0}}),
				clusterPools(map[string]ResourcePool{"H100": {Limit: 4, Used: 2}}),
			},
			demand: map[string]int{"H100": 3},
			want:   false,
		},
		{
			name: "a finite provider beats another's unlimited sentinel",
			constraints: []*ResourceConstraints{
				clusterPools(map[string]ResourcePool{"H100": {Limit: -1}}),
				clusterPools(map[string]ResourcePool{"H100": {Limit: 4, Used: 3}}),
			},
			demand: map[string]int{"H100": 2},
			want:   false,
		},
		{
			// The joint check is the whole point for a P/D pair: prefill and
			// decode can each fit alone while the pair does not.
			name:        "a pair that each fit alone but not together is denied",
			constraints: []*ResourceConstraints{clusterPools(map[string]ResourcePool{"H100": {Limit: 8, Used: 5}})},
			demand:      map[string]int{"H100": 4}, // 2 decode + 2 prefill, 3 free
			want:        false,
		},
		{
			name: "demand spanning two accelerator types must fit in both",
			constraints: []*ResourceConstraints{clusterPools(map[string]ResourcePool{
				"H100": {Limit: 8, Used: 6},
				"L4":   {Limit: 4, Used: 4},
			})},
			demand: map[string]int{"H100": 2, "L4": 1},
			want:   false,
		},
		{
			name: "a namespace cap tighter than the cluster pool is honoured",
			constraints: []*ResourceConstraints{nsPools(
				map[string]ResourcePool{"H100": {Limit: 32, Used: 0}},
				map[string]map[string]ResourcePool{budgetNS: {"H100": {Limit: 2, Used: 1}}},
			)},
			demand: map[string]int{"H100": 2},
			want:   false,
		},
		{
			name: "a namespace allowlist denies a type it does not list",
			constraints: []*ResourceConstraints{nsPools(
				map[string]ResourcePool{"H100": {Limit: 32}, "L4": {Limit: 32}},
				map[string]map[string]ResourcePool{budgetNS: {"H100": {Limit: 8}}},
			)},
			demand: map[string]int{"L4": 1},
			want:   false,
		},
		{
			name: "another namespace's cap does not constrain this one",
			constraints: []*ResourceConstraints{nsPools(
				map[string]ResourcePool{"H100": {Limit: 32, Used: 0}},
				map[string]map[string]ResourcePool{"other-ns": {"H100": {Limit: 1}}},
			)},
			demand: map[string]int{"H100": 8},
			want:   true,
		},
		{
			name:        "zero demand is always satisfiable",
			constraints: []*ResourceConstraints{clusterPools(map[string]ResourcePool{"H100": {Limit: 0, Used: 0}})},
			demand:      map[string]int{"H100": 0},
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FitsGPUBudget(tt.constraints, budgetNS, tt.demand); got != tt.want {
				t.Fatalf("FitsGPUBudget = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFitsGPUBudgetReconcilesAcceleratorNaming covers the two vocabularies a
// workload and the pools can be written in.
//
// Pool keys are normalized short names; a workload may declare either the full
// product label or an already-short name. Both must find their pool — and
// critically, a short name that CONTAINS a hyphen must be matched directly
// rather than normalized, since NormalizeAcceleratorName("Gaudi-2") is "2".
// Normalizing unconditionally denied such a variant a wake it should have got.
func TestFitsGPUBudgetReconcilesAcceleratorNaming(t *testing.T) {
	// Pools as TypeInventory.Refresh would key them.
	constraints := []*ResourceConstraints{clusterPools(map[string]ResourcePool{
		"A100":    {Limit: 8, Used: 0},
		"Gaudi-2": {Limit: 4, Used: 0},
	})}

	tests := []struct {
		name     string
		declared string
		need     int
		want     bool
	}{
		{name: "full product label finds its normalized pool", declared: "NVIDIA-A100-PCIE-80GB", need: 4, want: true},
		{name: "full product label is still bounded by that pool", declared: "NVIDIA-A100-PCIE-80GB", need: 9, want: false},
		{name: "short name matches directly", declared: "A100", need: 4, want: true},
		{name: "hyphenated short name matches directly, not via normalization", declared: "Gaudi-2", need: 2, want: true},
		{name: "hyphenated short name is still bounded", declared: "Gaudi-2", need: 5, want: false},
		{name: "a genuinely unknown type is denied", declared: "TPU-v5", need: 1, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FitsGPUBudget(constraints, budgetNS, map[string]int{tt.declared: tt.need})
			if got != tt.want {
				t.Fatalf("FitsGPUBudget(%s x%d) = %v, want %v", tt.declared, tt.need, got, tt.want)
			}
		})
	}
}

// TestFitsGPUBudgetNamespaceScoped covers the closed-allowlist semantics, where
// the namespace — not the cluster aggregate — decides what may be used.
func TestFitsGPUBudgetNamespaceScoped(t *testing.T) {
	tests := []struct {
		name        string
		constraints []*ResourceConstraints
		demand      map[string]int
		want        bool
	}{
		{
			// An operator granting a namespace UNLIMITED of a type must not have
			// its wakes refused. aggregateNamespacePools omits the -1 sentinel, so
			// the type is absent from the cluster Pools; resolving there first
			// denied it while the optimizer allowed it.
			name: "an unlimited namespace grant is honoured even though the cluster pool omits the type",
			constraints: []*ResourceConstraints{nsPools(
				map[string]ResourcePool{},
				map[string]map[string]ResourcePool{budgetNS: {"A100": {Limit: -1}}},
			)},
			demand: map[string]int{"A100": 16},
			want:   true,
		},
		{
			name: "a finite namespace grant still binds",
			constraints: []*ResourceConstraints{nsPools(
				map[string]ResourcePool{"A100": {Limit: 64}},
				map[string]map[string]ResourcePool{budgetNS: {"A100": {Limit: 2, Used: 1}}},
			)},
			demand: map[string]int{"A100": 2},
			want:   false,
		},
		{
			// A namespace may not be granted more than physically exists.
			name: "an unlimited namespace grant is still bounded by the cluster pool",
			constraints: []*ResourceConstraints{
				{ProviderName: "gpu-limiter", Pools: map[string]ResourcePool{"A100": {Limit: 4, Used: 3}}},
				nsPools(nil, map[string]map[string]ResourcePool{budgetNS: {"A100": {Limit: -1}}}),
			},
			demand: map[string]int{"A100": 2},
			want:   false,
		},
		{
			name: "a type the namespace does not list is denied",
			constraints: []*ResourceConstraints{nsPools(
				map[string]ResourcePool{"A100": {Limit: 64}, "L4": {Limit: 64}},
				map[string]map[string]ResourcePool{budgetNS: {"A100": {Limit: 8}}},
			)},
			demand: map[string]int{"L4": 1},
			want:   false,
		},
		{
			// The product-label spelling must resolve on the namespace side too.
			name: "a product-label demand resolves against the namespace's keys",
			constraints: []*ResourceConstraints{nsPools(
				map[string]ResourcePool{"A100": {Limit: 64}},
				map[string]map[string]ResourcePool{budgetNS: {"A100": {Limit: 2}}},
			)},
			demand: map[string]int{"NVIDIA-A100-PCIE-80GB": 3},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FitsGPUBudget(tt.constraints, budgetNS, tt.demand); got != tt.want {
				t.Fatalf("FitsGPUBudget = %v, want %v", got, tt.want)
			}
		})
	}
}
