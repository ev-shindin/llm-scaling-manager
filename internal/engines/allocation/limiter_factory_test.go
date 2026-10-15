package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// configWithLimiters builds a test Config whose global "default" saturation
// entry declares the given inline limiters — the sole source of limiter
// selection that NewLimiterFromConfig consults (via EffectiveLimiterTypes /
// EffectiveQuotaEntries).
func configWithLimiters(limiters ...config.QuotaLimiterConfig) *config.Config {
	GinkgoHelper()
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"default": {Limiters: limiters},
	})
	return cfg
}

var _ = Describe("NewLimiterFromConfig", func() {

	// Declaring no limiter must produce no limiting, not a hidden default one. A
	// NoOpLimiter provides no constraints, so the optimizer runs unconstrained and
	// the scale-from-zero check reports the wake as unchecked — both visible, both
	// what the operator asked for by declaring nothing.
	It("returns a NoOp limiter when no limiters are declared", func() {
		l, err := NewLimiterFromConfig(config.NewTestConfig(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).NotTo(BeNil())
		Expect(ConstraintProvidersFrom(l)).To(BeEmpty(),
			"an undeclared limiter must not bound scaling")
	})

	It("returns an inventory limiter for an explicit inventory entry", func() {
		cfg := configWithLimiters(config.QuotaLimiterConfig{Type: "inventory"})
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Name()).To(Equal("gpu-limiter"))
		_, ok := l.(*DefaultLimiter)
		Expect(ok).To(BeTrue(), "inventory mode should produce a DefaultLimiter")
	})

	It("returns an inventory limiter for an inline gpu-inventory entry", func() {
		cfg := configWithLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory"})
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Name()).To(Equal("gpu-limiter"))
	})

	It("returns a single DefaultLimiter for one inline quota entry", func() {
		cfg := configWithLimiters(config.QuotaLimiterConfig{
			Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 16},
		})
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).NotTo(BeNil())
		Expect(l.Name()).To(Equal("cluster-quota"))
		_, ok := l.(*DefaultLimiter)
		Expect(ok).To(BeTrue(), "single-entry quota should produce a DefaultLimiter")
	})

	It("wraps multiple inline quota entries in a CompositeLimiter", func() {
		cfg := configWithLimiters(
			config.QuotaLimiterConfig{
				Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 16},
			},
			config.QuotaLimiterConfig{
				Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 8}},
			},
		)
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())

		comp, ok := l.(*CompositeLimiter)
		Expect(ok).To(BeTrue(), "multi-entry quota should produce a CompositeLimiter")
		Expect(comp.Name()).To(Equal("quota-limiter"))
		Expect(comp.Constituents()).To(HaveLen(2))
		Expect(comp.Constituents()[0].Name()).To(Equal("cluster-quota"))
		Expect(comp.Constituents()[1].Name()).To(Equal("namespace-quota"))
	})

	// A quota entry used to select quota mode outright, and the physical entry
	// beside it was never built: the policy read as bounded by real GPUs while
	// nothing consulted them.
	It("builds the physical limiter AND every quota entry when both are declared", func() {
		for _, physical := range []string{"gpu-inventory", "inventory"} {
			cfg := configWithLimiters(
				config.QuotaLimiterConfig{
					Name: "tenant-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
					NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 8}},
				},
				config.QuotaLimiterConfig{Type: physical, Name: "gpu"},
				config.QuotaLimiterConfig{
					Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": 16},
				},
			)
			l, err := NewLimiterFromConfig(cfg, nil)
			Expect(err).NotTo(HaveOccurred())

			comp, ok := l.(*CompositeLimiter)
			Expect(ok).To(BeTrue(), "a composed set should produce a CompositeLimiter")
			providers := ConstraintProvidersFrom(comp)
			names := make([]string, 0, len(providers))
			bases := make([]UsageBasis, 0, len(providers))
			for _, cp := range providers {
				names = append(names, cp.Name())
				bases = append(bases, UsageBasisOf(cp))
			}
			Expect(names).To(Equal([]string{"gpu-limiter", "tenant-quota", "cluster-quota"}),
				"physical first, then every quota entry in declared order")
			Expect(bases).To(Equal([]UsageBasis{PhysicalUsage, ManagedUsage, ManagedUsage}),
				"the physical limiter counts every GPU on the nodes; a quota only the scaling manager's pods")
		}
	})
})

// A policy with two physical entries installed before the limiters composed.
// It must still validate and build one physical limiter beside the quota.
var _ = Describe("NewLimiterFromConfig with a duplicate physical entry", func() {
	It("builds one physical limiter and the quota", func() {
		cfg := configWithLimiters(
			config.QuotaLimiterConfig{Type: "gpu-inventory"},
			config.QuotaLimiterConfig{
				Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 16},
			},
			config.QuotaLimiterConfig{Type: "inventory"},
		)
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		providers := ConstraintProvidersFrom(l)
		names := make([]string, 0, len(providers))
		for _, cp := range providers {
			names = append(names, cp.Name())
		}
		Expect(names).To(Equal([]string{"gpu-limiter", "cluster-quota"}))
	})

	It("passes the controller's parse-time validation", func() {
		p := config.ScalingPolicy{Limiters: []config.QuotaLimiterConfig{
			{Type: "gpu-inventory"}, {Type: "inventory"},
			{Name: "q", Type: "quota", Scope: config.QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
		}}
		p.ApplyDefaults()
		Expect(p.Validate()).To(Succeed())
	})
})

var _ = Describe("LimiterSignature", func() {
	quota := config.QuotaLimiterConfig{
		Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"H100": 16},
	}

	It("changes when a physical entry is added beside a quota, or removed", func() {
		quotaOnly := LimiterSignature(configWithLimiters(quota))
		composed := LimiterSignature(configWithLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory"}, quota))
		Expect(composed).NotTo(Equal(quotaOnly))
		Expect(LimiterSignature(configWithLimiters(quota))).To(Equal(quotaOnly), "removing it restores the signature")
	})

	// The physical entry takes no fields and its name only labels the summary,
	// so renaming it (or spelling it inventory) builds the same limiter: no
	// rebuild, and no reason to keep the entry in the signature.
	It("does not change when only the physical entry's name or spelling changes", func() {
		Expect(LimiterSignature(configWithLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory", Name: "a"}, quota))).
			To(Equal(LimiterSignature(configWithLimiters(config.QuotaLimiterConfig{Type: "inventory", Name: "b"}, quota))))
	})

	It("is stable for equal configs", func() {
		Expect(LimiterSignature(configWithLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory"}, quota))).
			To(Equal(LimiterSignature(configWithLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory"}, quota))))
	})
})
