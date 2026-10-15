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

// PhysicalUsageConfigured answers from the declared limiter TYPES, because the
// usage observer needs the answer before any limiter is built. This checks that
// shortcut against the real thing: build the limiter each declaration selects,
// and compare what its providers actually declare. A new limiter kind, or a set
// that mixes physical and quota providers, fails here rather than by silently
// turning the cluster observation off.
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
		{"none (no limiters declared)", withLimiters()},
		{"inventory (explicit gpu-inventory entry)", withLimiters(
			config.QuotaLimiterConfig{Type: "gpu-inventory"})},
		{"quota", withLimiters(config.QuotaLimiterConfig{
			Name: "q", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": 4}})},
		// A quota beside the physical entry used to collapse the set to quota
		// mode, so the observer was not started for a physical limiter that is
		// now built.
		{"gpu-inventory + quota", withLimiters(
			config.QuotaLimiterConfig{Type: "gpu-inventory", Name: "gpu"},
			config.QuotaLimiterConfig{
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

// Matching the built limiter is not enough on its own: while a quota entry
// collapsed the set to quota mode, both answered "no physical provider" for a
// policy that declared one. A declared physical entry must be observed for.
func TestPhysicalUsageConfiguredForAComposedSet(t *testing.T) {
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"default": {Limiters: []config.QuotaLimiterConfig{
			{Type: "gpu-inventory", Name: "gpu"},
			{Name: "q", Type: "quota", Scope: config.QuotaScopeCluster, ClusterQuotas: map[string]int{"A100": 4}},
		}},
	})
	if !PhysicalUsageConfigured(cfg) {
		t.Fatal("a physical entry declared beside a quota must turn the node observation on")
	}
	if !NodeUsageWanted(cfg, false) {
		t.Fatal("NodeUsageWanted must follow PhysicalUsageConfigured")
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
