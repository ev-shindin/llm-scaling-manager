package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// The gpu-policy readiness gate arms whenever a physical limiter is declared,
// alone or beside quotas. It used to ask for the inventory MODE, which a quota
// entry suppressed: adding a quota beside the physical entry silenced the gate
// while the physical limiter, now built, failed every cycle without node read.
func TestGPUPolicyReadyArmsForEveryPhysicalDeclaration(t *testing.T) {
	forbidden := errors.New("nodes is forbidden")
	probed := 0
	probe := func(context.Context) error { probed++; return forbidden }

	quota := config.QuotaLimiterConfig{Type: "quota", Name: "q", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"H100": 8}}
	for _, tc := range []struct {
		name     string
		limiters []config.QuotaLimiterConfig
		armed    bool
	}{
		{"none", nil, false},
		{"quota only", []config.QuotaLimiterConfig{quota}, false},
		{"physical only", []config.QuotaLimiterConfig{{Type: "gpu-inventory"}}, true},
		{"physical and quota", []config.QuotaLimiterConfig{{Type: "gpu-inventory"}, quota}, true},
		{"quota and the inventory alias", []config.QuotaLimiterConfig{quota, {Type: "inventory"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": {Limiters: tc.limiters}})
			probed = 0
			err := gpuPolicyReady(context.Background(), cfg, probe)
			if tc.armed {
				assert.ErrorIs(t, err, forbidden, "a physical limiter without node read must take the controller out of service")
				assert.Equal(t, 1, probed)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, 0, probed, "no physical limiter: the gate must not list nodes")
			}
		})
	}
}
