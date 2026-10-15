package allocation

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// nodeReadDeniedClient is a client whose every List is Forbidden, as for a
// namespace-scoped install that was never granted node read.
func nodeReadDeniedClient() client.Client {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no ClusterRole"))
		},
	}).Build()
}

// Review of PR #164: wva_node_access_denied pages the critical
// WVANodeAccessDenied alert, "no variant scales up". That is true of a physical
// limiter declared alone. Beside a quota the physical provider is only skipped
// and the quota still bounds, yet the inventory raised the same signal, so a
// namespace-scoped install that observes usage but cannot list nodes paged a
// critical alert for a controller that was scaling.
func TestNodeAccessDeniedIsCriticalOnlyForAPhysicalLimiterAlone(t *testing.T) {
	t.Cleanup(func() { metrics.SetNodeAccessDenied(false) })
	quota := config.QuotaLimiterConfig{
		Name: "tenant-quota", Type: "quota", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"H100": 8},
	}
	cases := []struct {
		name     string
		limiters []config.QuotaLimiterConfig
		want     bool
	}{
		{"gpu-inventory alone", []config.QuotaLimiterConfig{{Type: "gpu-inventory"}}, true},
		{"gpu-inventory beside a quota", []config.QuotaLimiterConfig{{Type: "gpu-inventory"}, quota}, false},
		{"the inventory alias beside a quota", []config.QuotaLimiterConfig{quota, {Type: "inventory"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.SetNodeAccessDenied(!tc.want)
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": {Limiters: tc.limiters}})
			l, err := NewLimiterFromConfig(cfg, nodeReadDeniedClient())
			if err != nil {
				t.Fatalf("NewLimiterFromConfig: %v", err)
			}
			var physical ConstraintProvider
			for _, cp := range ConstraintProvidersFrom(l) {
				if cp.Name() == PhysicalLimiterName {
					physical = cp
				}
			}
			if physical == nil {
				t.Fatalf("no %s provider was built", PhysicalLimiterName)
			}
			if _, err := physical.ComputeConstraints(context.Background(), map[string]int{}, nil); !apierrors.IsForbidden(err) {
				t.Fatalf("ComputeConstraints error = %v, want the node list's Forbidden: the provider must still be skipped", err)
			}
			if got := metrics.IsNodeAccessDenied(); got != tc.want {
				t.Errorf("wva_node_access_denied = %v, want %v", got, tc.want)
			}
		})
	}
}
