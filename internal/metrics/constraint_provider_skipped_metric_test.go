package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// TestSetConstraintProvidersSkipped: each producer replaces only the series of
// the reasons it owns, so the engine's per-cycle report cannot clear the
// readiness gate's node_read_denied, and a provider back in force loses its
// series.
func TestSetConstraintProvidersSkipped(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	series := func() map[string]string {
		t.Helper()
		families, err := registry.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		out := map[string]string{}
		for _, mf := range families {
			if mf.GetName() != constants.WVAConstraintProviderSkipped {
				continue
			}
			for _, m := range mf.GetMetric() {
				var provider, reason string
				for _, l := range m.GetLabel() {
					switch l.GetName() {
					case constants.LabelProvider:
						provider = l.GetValue()
					case constants.LabelReason:
						reason = l.GetValue()
					}
				}
				if m.GetGauge().GetValue() != 1 {
					t.Errorf("%s/%s = %v, want 1", provider, reason, m.GetGauge().GetValue())
				}
				out[provider+"/"+reason] = reason
			}
		}
		return out
	}
	engine := []string{constants.ProviderSkipUnobserved, constants.ProviderSkipFailed, constants.ProviderSkipNoConstraints}
	gate := []string{constants.ProviderSkipNodeReadDenied}

	SetConstraintProvidersSkipped(gate, map[string]string{"gpu-limiter": constants.ProviderSkipNodeReadDenied})
	SetConstraintProvidersSkipped(engine, map[string]string{
		"gpu-limiter":  constants.ProviderSkipUnobserved,
		"tenant-quota": constants.ProviderSkipFailed,
	})
	got := series()
	for _, want := range []string{"gpu-limiter/node_read_denied", "gpu-limiter/unobserved", "tenant-quota/failed"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing series %s in %v", want, got)
		}
	}

	// The quota recovers and the engine reports only the physical skip: the
	// quota's series goes, the gate's stays.
	SetConstraintProvidersSkipped(engine, map[string]string{"gpu-limiter": constants.ProviderSkipUnobserved})
	got = series()
	if _, ok := got["tenant-quota/failed"]; ok {
		t.Errorf("a recovered provider kept its series: %v", got)
	}
	if _, ok := got["gpu-limiter/node_read_denied"]; !ok {
		t.Errorf("the engine cleared a reason it does not own: %v", got)
	}

	// A producer cannot set a reason it does not own.
	SetConstraintProvidersSkipped(engine, map[string]string{"x": constants.ProviderSkipNodeReadDenied})
	if _, ok := series()["x/node_read_denied"]; ok {
		t.Error("the engine set a reason it does not own")
	}

	SetConstraintProvidersSkipped(gate, nil)
	SetConstraintProvidersSkipped(engine, nil)
	if got := series(); len(got) != 0 {
		t.Errorf("series left after both producers cleared: %v", got)
	}
}
