package steadystate

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

var _ = Describe("live GPU limiter rebuild", func() {
	ctx := context.Background()

	It("rebuilds the limiter only when the effective limiter config changes", func() {
		cfg := config.NewTestConfig()
		e := &Engine{Config: cfg, GPULimiter: allocation.NewNoOpLimiter("initial")}

		builds := 0
		e.SetLimiterBuilder(func() (allocation.Limiter, error) {
			builds++
			// Name the rebuilt limiter after the declared set so we can observe the swap.
			return allocation.NewNoOpLimiter(cfg.EffectiveLimiterSummary()), nil
		})

		// No config change since SetLimiterBuilder seeded the signature: no rebuild.
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(0))
		Expect(e.GPULimiter.Name()).To(Equal("initial"))

		// Switch to quota mode via inline limiters: the signature changes -> rebuild.
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: []config.QuotaLimiterConfig{{
				Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 8},
			}}},
		})
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(1))
		Expect(e.GPULimiter.Name()).To(Equal("quota/cluster"))

		// No further change: no rebuild.
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(1))
	})

	It("rebuilds when a quota value changes within the same mode", func() {
		cfg := config.NewTestConfig()
		quotaCfg := func(cap int) map[string]config.ScalingPolicy {
			return map[string]config.ScalingPolicy{
				"default": {Limiters: []config.QuotaLimiterConfig{{
					Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": cap},
				}}},
			}
		}
		cfg.UpdateScalingPolicyConfig(quotaCfg(8))
		e := &Engine{Config: cfg, GPULimiter: allocation.NewNoOpLimiter("initial")}

		builds := 0
		e.SetLimiterBuilder(func() (allocation.Limiter, error) {
			builds++
			// Name encodes the current quota so the swap is observable.
			return allocation.NewNoOpLimiter(string(rune('0' + cfg.EffectiveQuotaEntries()[0].ClusterQuotas["H100"]))), nil
		})

		// Same quota mode AND same entries: no rebuild.
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(0))

		// Same mode, changed quota value: signature changes -> rebuild.
		cfg.UpdateScalingPolicyConfig(quotaCfg(9))
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(1))
		Expect(e.GPULimiter.Name()).To(Equal("9"))
	})

	// The signature used to encode the mode and the quota entries only. Adding a
	// physical entry beside a quota left both unchanged -- the mode stayed quota
	// -- so the engine kept a limiter without the physical bound the edit asked
	// for, until a restart.
	It("rebuilds when a physical entry is added beside a quota, and when it is removed", func() {
		cfg := config.NewTestConfig()
		quota := config.QuotaLimiterConfig{
			Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 8},
		}
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: []config.QuotaLimiterConfig{quota}},
		})
		e := &Engine{Config: cfg, GPULimiter: allocation.NewNoOpLimiter("initial")}
		builds := 0
		e.SetLimiterBuilder(func() (allocation.Limiter, error) {
			builds++
			return allocation.NewNoOpLimiter(cfg.EffectiveLimiterSummary()), nil
		})

		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: []config.QuotaLimiterConfig{{Type: "gpu-inventory", Name: "gpu"}, quota}},
		})
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(1))
		Expect(e.GPULimiter.Name()).To(Equal("inventory+quota/gpu,cluster"))

		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: []config.QuotaLimiterConfig{quota}},
		})
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(2))
		Expect(e.GPULimiter.Name()).To(Equal("quota/cluster"))
	})

	It("keeps the previous limiter when a rebuild fails", func() {
		cfg := config.NewTestConfig()
		e := &Engine{Config: cfg, GPULimiter: allocation.NewNoOpLimiter("keep-me")}
		e.SetLimiterBuilder(func() (allocation.Limiter, error) {
			return nil, context.DeadlineExceeded
		})
		// Force a signature change so refresh attempts a rebuild.
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: []config.QuotaLimiterConfig{{
				Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 8},
			}}},
		})
		e.refreshLimiter(ctx)
		Expect(e.GPULimiter.Name()).To(Equal("keep-me"), "a failed rebuild must not drop the limiter")
	})
})
