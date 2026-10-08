package allocation

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// A quota and a physical inventory answer different questions, and the answer to
// one is wrong for the other. These pin which measure each asks for, because
// nothing downstream can tell it was handed the wrong one: the constraint still
// computes, it just computes against consumption the provider does not govern.
func TestUsageBasisOfLimiters(t *testing.T) {
	quota := NewDefaultLimiter("quota", NewQuotaInventory(config.QuotaLimiterConfig{
		Name: "quota", Type: "quota", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"A100": 4},
	}))
	if got := UsageBasisOf(quota); got != ManagedUsage {
		t.Errorf("quota limiter basis = %v, want %v; a quota is an allowance granted to WVA and "+
			"may only be charged for what WVA holds", got, ManagedUsage)
	}

	physical := NewDefaultLimiter("gpu", NewTypeInventory("gpu", nil))
	if got := UsageBasisOf(physical); got != PhysicalUsage {
		t.Errorf("type-inventory limiter basis = %v, want %v; the scheduler will not place a "+
			"variant on a device somebody else holds", got, PhysicalUsage)
	}

	// Anything that has not thought about it gets the physical figure: that
	// over-states consumption for a quota, which errs toward refusing, rather than
	// under-stating what the hardware holds, which would place onto a taken device.
	if got := UsageBasisOf(NewNoOpLimiter("noop")); got != PhysicalUsage {
		t.Errorf("undeclared basis = %v, want %v", got, PhysicalUsage)
	}
}

// PhysicalUsageConfigured answers from the limiter MODE, because the usage
// observer needs the answer before any limiter is built. This checks that shortcut
// against the real thing: build the limiter each mode selects, and compare what
// its providers actually declare. A new limiter mode, or a mode that starts mixing
// physical and quota providers, fails here rather than by silently turning the
// cluster observation off.
func TestPhysicalUsageConfiguredMatchesTheBuiltLimiter(t *testing.T) {
	withLimiters := func(limiters ...config.QuotaLimiterConfig) *config.Config {
		cfg := config.NewTestConfig()
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: limiters},
		})
		return cfg
	}

	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{"inventory (the default, no limiters declared)", withLimiters()},
		{"inventory (explicit gpu-inventory entry)", withLimiters(
			config.QuotaLimiterConfig{Type: "gpu-inventory"})},
		{"quota", withLimiters(config.QuotaLimiterConfig{
			Name: "q", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": 4}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter, err := NewLimiterFromConfig(tc.cfg, nil)
			if err != nil {
				t.Fatalf("NewLimiterFromConfig: %v", err)
			}
			built := false
			for _, cp := range ConstraintProvidersFrom(limiter) {
				if UsageBasisOf(cp) == PhysicalUsage {
					built = true
				}
			}
			if got := PhysicalUsageConfigured(tc.cfg); got != built {
				t.Errorf("PhysicalUsageConfigured = %t but the built limiter %q has a physical "+
					"provider = %t; the observer would %s", got, limiter.Name(), built,
					map[bool]string{true: "observe for nothing", false: "stop observing for a consumer that needs it"}[got])
			}
		})
	}
}

func TestGPUUsageViewsRouting(t *testing.T) {
	views := GPUUsageViews{
		PhysicalByType:      map[string]int{"A100": 6},
		PhysicalByNamespace: map[string]map[string]int{"chat": {"A100": 6}},
		ManagedByType:       map[string]int{"A100": 2},
		ManagedByNamespace:  map[string]map[string]int{"chat": {"A100": 2}},
	}

	quota := NewDefaultLimiter("quota", NewQuotaInventory(config.QuotaLimiterConfig{
		Name: "quota", Type: "quota", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"A100": 4},
	}))
	physical := NewDefaultLimiter("gpu", NewTypeInventory("gpu", nil))

	if byType, _ := views.For(quota); byType["A100"] != 2 {
		t.Errorf("quota was routed %v, want the managed view (2)", byType)
	}
	if byType, _ := views.For(physical); byType["A100"] != 6 {
		t.Errorf("physical inventory was routed %v, want the physical view (6)", byType)
	}
	if _, byNS := views.For(quota); byNS["chat"]["A100"] != 2 {
		t.Errorf("quota namespace view = %v, want the managed one", byNS)
	}
}

// MissingBasis must be consulted BEFORE any provider is called. A nil view is
// indistinguishable at the provider from a confident "nothing is in use", and a
// provider told nothing is in use reports its entire capacity free — the opposite
// of the "unknown" the caller meant.
func TestGPUUsageViewsMissingBasis(t *testing.T) {
	quota := NewDefaultLimiter("quota", NewQuotaInventory(config.QuotaLimiterConfig{
		Name: "quota", Type: "quota", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"A100": 4},
	}))
	physical := NewDefaultLimiter("gpu", NewTypeInventory("gpu", nil))
	both := []ConstraintProvider{physical, quota}

	physicalOnly := GPUUsageViews{PhysicalByType: map[string]int{}}
	if basis, missing := physicalOnly.MissingBasis(both); !missing || basis != ManagedUsage {
		t.Errorf("MissingBasis = (%v, %t), want (%v, true)", basis, missing, ManagedUsage)
	}
	if _, missing := physicalOnly.MissingBasis([]ConstraintProvider{physical}); missing {
		t.Error("a physical-only deployment must not wait on a managed view nothing reads")
	}

	managedOnly := GPUUsageViews{ManagedByType: map[string]int{}}
	if _, missing := managedOnly.MissingBasis([]ConstraintProvider{quota}); missing {
		t.Error("a quota-only deployment must not wait on a physical view nothing reads")
	}
	if basis, missing := managedOnly.MissingBasis(both); !missing || basis != PhysicalUsage {
		t.Errorf("MissingBasis = (%v, %t), want (%v, true)", basis, missing, PhysicalUsage)
	}

	// An observed-but-empty view is an answer, not an absence: it says nothing is
	// in use, which is exactly what a parked fleet's managed view reports.
	full := GPUUsageViews{PhysicalByType: map[string]int{}, ManagedByType: map[string]int{}}
	if _, missing := full.MissingBasis(both); missing {
		t.Error("both views were observed; empty is a measurement, not a gap")
	}
}

// The refresher walks nodes for a physical limiter always, and for an active
// utilization-share optimizer only on a cluster-scoped install: a
// namespace-scoped one may not list nodes.
func TestNodeUsageWanted(t *testing.T) {
	policy := func(doc string) *config.Config {
		var p config.ScalingPolicy
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatal(err)
		}
		c := config.NewTestConfig()
		c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: p})
		return c
	}
	quota := "limiters:\n  - type: quota\n    name: q\n    scope: cluster\n    quotas:\n      A100: 16\n"
	share := "optimizer:\n  type: utilizationShare\n"
	shadow := share + "  utilizationShare:\n    shadow: true\n"
	inventory := "limiters:\n  - type: inventory\n    name: gpu\n"
	for _, tc := range []struct {
		name            string
		doc             string
		namespaceScoped bool
		want            bool
	}{
		{"quota alone", quota, false, false},
		{"quota and an active optimizer", quota + share, false, true},
		{"the same, namespace-scoped", quota + share, true, false},
		{"quota and a shadow optimizer", quota + shadow, false, false},
		{"a physical limiter", inventory, false, true},
		{"a physical limiter, namespace-scoped", inventory, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NodeUsageWanted(policy(tc.doc), tc.namespaceScoped); got != tc.want {
				t.Fatalf("NodeUsageWanted = %v, want %v", got, tc.want)
			}
		})
	}
}
