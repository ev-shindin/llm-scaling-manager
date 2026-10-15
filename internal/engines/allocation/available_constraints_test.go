package allocation

import (
	"context"
	"errors"
	"math"
	"testing"
)

// physicalLimiterName is the name NewLimiterFromConfig gives the physical limiter.
const physicalLimiterName = "gpu-limiter"

// stubProvider is a constraint provider with a fixed answer, a declared basis,
// and a call counter, so a test can assert it was skipped without being asked.
type stubProvider struct {
	name  string
	basis UsageBasis
	pools map[string]ResourcePool
	err   error
	calls *int
}

func (s stubProvider) Name() string           { return s.name }
func (s stubProvider) UsageBasis() UsageBasis { return s.basis }
func (s stubProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*ResourceConstraints, error) {
	if s.calls != nil {
		*s.calls++
	}
	if s.err != nil {
		return nil, s.err
	}
	return &ResourceConstraints{ProviderName: s.name, Pools: s.pools}, nil
}

// One missing input never lifts the others. Both engines used to drop every
// constraint when any one provider's view was unobserved or any one provider
// failed; the providers that could still answer must bound the decision.
func TestComputeAvailableConstraintsSkipsOnlyTheUnavailable(t *testing.T) {
	ctx := context.Background()
	physicalCalls := 0
	physical := stubProvider{name: physicalLimiterName, basis: PhysicalUsage,
		pools: map[string]ResourcePool{"H100": {Limit: 16}}, calls: &physicalCalls}
	quota := stubProvider{name: "tenant-quota", basis: ManagedUsage,
		pools: map[string]ResourcePool{"H100": {Limit: 4}}}
	both := []ConstraintProvider{physical, quota}

	t.Run("an unobserved basis skips its provider without calling it", func(t *testing.T) {
		managedOnly := GPUUsageViews{ManagedByType: map[string]int{}}
		got, skipped := ComputeAvailableConstraints(ctx, both, managedOnly, nil)
		if len(got) != 1 || got[0].ProviderName != "tenant-quota" {
			t.Fatalf("constraints = %+v, want the quota alone to still bound", got)
		}
		if len(skipped) != 1 || skipped[0].Name != physicalLimiterName || !skipped[0].Unobserved || skipped[0].Basis != PhysicalUsage {
			t.Fatalf("skipped = %+v, want the physical limiter named as unobserved", skipped)
		}
		if physicalCalls != 0 {
			t.Fatal("a provider handed no view reports its whole capacity free; it must not be called")
		}
	})

	t.Run("a failing provider is skipped and the other still bounds", func(t *testing.T) {
		failing := quota
		failing.err = errors.New("kueue unreachable")
		full := GPUUsageViews{PhysicalByType: map[string]int{}, ManagedByType: map[string]int{}}
		got, skipped := ComputeAvailableConstraints(ctx, []ConstraintProvider{physical, failing}, full, nil)
		if len(got) != 1 || got[0].ProviderName != physicalLimiterName {
			t.Fatalf("constraints = %+v, want the physical limiter to still bound", got)
		}
		if len(skipped) != 1 || skipped[0].Name != "tenant-quota" || skipped[0].Unobserved {
			t.Fatalf("skipped = %+v, want the failing quota named", skipped)
		}
	})

	t.Run("an observed-but-empty view is an answer, not a gap", func(t *testing.T) {
		full := GPUUsageViews{PhysicalByType: map[string]int{}, ManagedByType: map[string]int{}}
		got, skipped := ComputeAvailableConstraints(ctx, both, full, nil)
		if len(got) != 2 || len(skipped) != 0 {
			t.Fatalf("got %d constraints, skipped %+v; both views were observed", len(got), skipped)
		}
	})

	t.Run("every provider unavailable yields nothing to check against", func(t *testing.T) {
		got, skipped := ComputeAvailableConstraints(ctx, both, GPUUsageViews{}, nil)
		if len(got) != 0 || len(skipped) != 2 {
			t.Fatalf("got %+v, skipped %+v; want none and both named", got, skipped)
		}
	})

	t.Run("skipping one only ever widens the merged budget", func(t *testing.T) {
		full := GPUUsageViews{PhysicalByType: map[string]int{}, ManagedByType: map[string]int{}}
		all, _ := ComputeAvailableConstraints(ctx, both, full, nil)
		some, _ := ComputeAvailableConstraints(ctx, both, GPUUsageViews{ManagedByType: map[string]int{}}, nil)
		if a, s := mergeConstraints(all)["H100"], mergeConstraints(some)["H100"]; s < a {
			t.Fatalf("H100 budget %d with a provider skipped, %d with all; skipping must never deny more", s, a)
		}
	})
}

// ClusterFree is the tightest CLOSED cluster figure. A namespace-scope quota's
// aggregate is open (the sum of its namespace caps) and bounds a namespace
// group through its own budget, not as a cluster figure.
func TestShareGroupClusterFreeReadsClosedPoolsOnly(t *testing.T) {
	reqs := []ModelScalingRequest{shareReq("A", "ns", 4000, 4)}
	nsQuota := &ResourceConstraints{ProviderName: "ns-quota",
		Pools:          map[string]ResourcePool{"A100": {Limit: 16, Used: 4}},
		NamespacePools: map[string]map[string]ResourcePool{"ns": {"A100": {Limit: 16, Used: 4}}}}

	groups := BuildShareGroups(reqs, []*ResourceConstraints{nsQuota}, ShareGroupOptions{ClusterIsQuota: true})
	if len(groups) != 1 || groups[0].ClusterFree != math.MaxInt {
		t.Fatalf("groups %+v: with no closed provider the cluster figure is unbounded", groups)
	}

	physical := &ResourceConstraints{ProviderName: physicalLimiterName,
		Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 13}}}
	groups = BuildShareGroups(reqs, []*ResourceConstraints{physical, nsQuota}, ShareGroupOptions{ClusterIsQuota: true})
	if len(groups) != 1 || groups[0].ClusterFree != 3 {
		t.Fatalf("groups %+v: ClusterFree must be the physical pool's 3 free", groups)
	}
}
