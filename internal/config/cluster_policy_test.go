package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Cluster policy is the bound a tenant must not be able to lift.
//
// The threat is specific and it is not hypothetical: a namespace-scoped install
// runs the controller in the TENANT's namespace, which makes the tenant the owner
// of POD_NAMESPACE and therefore of the ConfigMap that declares the limiter and
// the quota. Every assertion here is about the same property — once policy is
// sourced from elsewhere, nothing written in the controller's own namespace can
// change what bounds it.
var _ = Describe("Config cluster policy", func() {
	quotaOf := func(n int) []QuotaLimiterConfig {
		return []QuotaLimiterConfig{{
			Type: "quota", Name: "cluster-h100", Scope: QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": n},
		}}
	}

	It("uses the global entry when policy is not separated", func() {
		// The default and the cluster-scoped case: the controller's namespace
		// belongs to the admin already, so nothing changes.
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: quotaOf(32)},
		})
		Expect(c.ClusterPolicyIsSeparate()).To(BeFalse())
		Expect(c.EffectiveLimiterTypes()).To(Equal([]LimiterType{LimiterTypeQuota}))
		Expect(c.EffectiveQuotaEntries()[0].ClusterQuotas["H100"]).To(Equal(32))
	})

	It("takes limiters from cluster policy once it is set", func() {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{"default": {}})
		c.UpdateClusterPolicy(&ScalingPolicy{Limiters: quotaOf(8)})

		Expect(c.ClusterPolicyIsSeparate()).To(BeTrue())
		Expect(c.EffectiveLimiterTypes()).To(Equal([]LimiterType{LimiterTypeQuota}))
		Expect(c.EffectiveQuotaEntries()[0].ClusterQuotas["H100"]).To(Equal(8))
	})

	// The whole point, stated as a test: the tenant writes a bigger number into
	// the namespace they control, and it does not move the bound.
	It("does not let the controller's own ConfigMap raise the quota", func() {
		c := &Config{}
		c.UpdateClusterPolicy(&ScalingPolicy{Limiters: quotaOf(8)})

		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: quotaOf(4096)},
		})

		Expect(c.EffectiveQuotaEntries()).To(HaveLen(1))
		Expect(c.EffectiveQuotaEntries()[0].ClusterQuotas["H100"]).To(Equal(8),
			"a quota a tenant can raise is not a quota")
	})

	// Removing the limiter is the other half of the same attack, and the cheaper
	// one: if an empty local list won, the tenant would not need to out-declare
	// the admin, only to out-blank them.
	It("does not let the controller's own ConfigMap remove the limiter", func() {
		c := &Config{}
		c.UpdateClusterPolicy(&ScalingPolicy{Limiters: quotaOf(8)})

		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: []QuotaLimiterConfig{}},
		})

		Expect(c.EffectiveLimiterTypes()).To(Equal([]LimiterType{LimiterTypeQuota}))
	})

	It("reports no limiter when cluster policy declares none", func() {
		// An admin who declares no limiter means none. This must not fall back to
		// the local entry, or "the admin removed the quota" would silently hand
		// control back to the tenant.
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: quotaOf(4096)},
		})
		c.UpdateClusterPolicy(&ScalingPolicy{})

		Expect(c.EffectiveLimiterTypes()).To(BeEmpty())
		Expect(c.EffectiveQuotaEntries()).To(BeEmpty())
	})

	It("restores the global entry as the source when cluster policy is cleared", func() {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: quotaOf(32)},
		})
		c.UpdateClusterPolicy(&ScalingPolicy{Limiters: quotaOf(8)})
		Expect(c.EffectiveQuotaEntries()[0].ClusterQuotas["H100"]).To(Equal(8))

		c.UpdateClusterPolicy(nil)

		Expect(c.ClusterPolicyIsSeparate()).To(BeFalse())
		Expect(c.EffectiveQuotaEntries()[0].ClusterQuotas["H100"]).To(Equal(32))
	})

	// The mode and the entries are read through one accessor precisely so they
	// cannot disagree. A split — mode saying "quota" while the entries came from
	// somewhere else — would present as a quota limiter enforcing nothing, with
	// nothing in the logs to distinguish it from a correctly empty quota.
	It("keeps the limiter mode and the quota entries reading the same source", func() {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}},
		})
		c.UpdateClusterPolicy(&ScalingPolicy{Limiters: quotaOf(8)})

		Expect(c.EffectiveLimiterTypes()).To(Equal([]LimiterType{LimiterTypeQuota}))
		Expect(c.EffectiveQuotaEntries()).To(HaveLen(1))
	})

	It("copies the limiters it is given, so a later mutation cannot rewrite the bound", func() {
		c := &Config{}
		limiters := quotaOf(8)
		c.UpdateClusterPolicy(&ScalingPolicy{Limiters: limiters})

		limiters[0].ClusterQuotas["H100"] = 4096

		Expect(c.EffectiveQuotaEntries()[0].ClusterQuotas["H100"]).To(Equal(8))
	})
})
