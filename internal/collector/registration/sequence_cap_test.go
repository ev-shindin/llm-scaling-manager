package registration

import (
	"context"
	"strings"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// The sequence-cap queries read the series each engine publishes, and the
// preemption rate covers the window the ITL mean is taken over: a preemption
// outside it says nothing about the tokens in it.
func TestSequenceCapQueries(t *testing.T) {
	registry := source.NewSourceRegistry()
	src := prometheus.NewPrometheusSource(context.Background(), &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig())
	if err := registry.Register("prometheus", src); err != nil {
		t.Fatalf("Register: %v", err)
	}
	RegisterQueueingModelQueries(registry)
	RegisterAlwaysOnQueries(registry)
	queries := registry.Get("prometheus").QueryList()

	template := func(name string) string {
		t.Helper()
		q := queries.Get(name)
		if q == nil {
			t.Fatalf("query %q is not registered", name)
		}
		return q.Template
	}
	for name, metric := range map[string]string{
		QueryRunningRequests: "vllm:num_requests_running",
		EngineQuery(inferenceengine.EngineSGLang, QueryRunningRequests): "sglang:num_running_reqs",
		QueryPreemptionRate: "vllm:num_preemptions_total",
	} {
		if got := template(name); !strings.Contains(got, metric) {
			t.Errorf("%s reads %q, want %s", name, got, metric)
		}
	}
	window := "[" + ITLWindow + "]"
	for _, name := range []string{QueryPreemptionRate, QueryAvgITL, EngineQuery(inferenceengine.EngineSGLang, QueryAvgITL)} {
		if got := template(name); !strings.Contains(got, window) {
			t.Errorf("%s = %q, want the ITL window %s", name, got, window)
		}
	}
}
