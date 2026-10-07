package steadystate

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

const shadowMessage = "Shadow: utilization share would rebalance"

// shadowConfig is a cluster-scope quota of 16 A100s, optionally with the
// utilization-share optimizer selected.
func shadowConfig(t *testing.T, optimizer string) *config.Config {
	t.Helper()
	doc := `
limiters:
  - type: quota
    name: q
    scope: cluster
    quotas:
      A100: 16
` + optimizer
	var p config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	c := config.NewTestConfig()
	c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: p})
	return c
}

// shadowRequest is a live single-variant model on A100: a per-replica capacity of
// 1000 at a threshold of 0.8, so a demand of D needs D / 800 GPUs.
func shadowRequest(id, class string, demand float64, current int) allocation.ModelScalingRequest {
	v := id + "-v"
	one := 1
	return allocation.ModelScalingRequest{
		ModelID:     id,
		Namespace:   "ns",
		WeightClass: class,
		CompositeSignal: allocation.NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName,
			Result: &domain.AnalyzerResult{
				TotalDemand:       demand,
				VariantCapacities: []domain.VariantCapacity{{VariantName: v, PerReplicaCapacity: 1000}},
			},
			ScaleUpThreshold: 0.8,
			Live:             true,
		},
		Variants:      []domain.VariantMetadata{{VariantName: v, AcceleratorName: "A100", Cost: 5}},
		VariantStates: []domain.VariantReplicaState{{VariantName: v, CurrentReplicas: current, GPUsPerReplica: 1, MinReplicas: &one}},
	}
}

func observe(t *testing.T) (context.Context, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.InfoLevel)
	return logr.NewContext(t.Context(), zapr.NewLogger(zap.New(core))), logs
}

// §5.7's step: B's demand doubles while the fleet still holds 9 / 5 / 2. The
// shadow evaluation must say a rebalance would move three replicas, at Info so
// it is seen at the default verbosity, and change nothing else.
func TestUtilizationShareShadowReportsAWouldBeRebalance(t *testing.T) {
	ctx, logs := observe(t)
	e := &Engine{Config: shadowConfig(t, "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    shadow: true\n")}
	reqs := []allocation.ModelScalingRequest{
		shadowRequest("A", "important", 4000, 9),
		shadowRequest("B", "", 6000, 5),
		shadowRequest("C", "", 1000, 2),
	}
	cons := []*allocation.ResourceConstraints{{Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16, Used: 16}}}}

	e.evaluateUtilizationShare(ctx, reqs, cons)

	got := logs.FilterMessage(shadowMessage).All()
	if len(got) != 1 {
		t.Fatalf("want one would-rebalance line, got %d", len(got))
	}
	fields := got[0].ContextMap()
	if fields["replicasToMove"] != int64(3) {
		t.Errorf("replicasToMove = %v, want 3", fields["replicasToMove"])
	}
	if fields["budget"] != int64(16) {
		t.Errorf("budget = %v, want 16", fields["budget"])
	}
}

// A settled fleet stays quiet at Info: held at its integer target, nothing is
// actionable and no would-rebalance line is written.
func TestUtilizationShareShadowIsQuietWhenSettled(t *testing.T) {
	ctx, logs := observe(t)
	e := &Engine{Config: shadowConfig(t, "optimizer:\n  type: utilizationShare\n")}
	reqs := []allocation.ModelScalingRequest{
		shadowRequest("A", "important", 4000, 9),
		shadowRequest("B", "", 3000, 5),
		shadowRequest("C", "", 1000, 2),
	}
	cons := []*allocation.ResourceConstraints{{Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16, Used: 16}}}}

	e.evaluateUtilizationShare(ctx, reqs, cons)

	if n := logs.FilterMessage(shadowMessage).Len(); n != 0 {
		t.Fatalf("a settled fleet wrote %d would-rebalance lines", n)
	}
}

// Not selected: nothing is evaluated. An invalid block is reported once, not
// every cycle, and evaluates nothing.
func TestUtilizationShareShadowOnlyWhenSelected(t *testing.T) {
	reqs := []allocation.ModelScalingRequest{shadowRequest("B", "", 6000, 5), shadowRequest("A", "", 4000, 11)}
	cons := []*allocation.ResourceConstraints{{Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16, Used: 16}}}}

	ctx, logs := observe(t)
	e := &Engine{Config: shadowConfig(t, "")}
	e.evaluateUtilizationShare(ctx, reqs, cons)
	if n := logs.FilterMessage(shadowMessage).Len(); n != 0 {
		t.Fatalf("an unselected optimizer wrote %d lines", n)
	}

	ctx, logs = observe(t)
	e = &Engine{Config: shadowConfig(t, "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    tolerance: 2\n")}
	for range 3 {
		e.evaluateUtilizationShare(ctx, reqs, cons)
	}
	if n := logs.FilterMessage("Invalid optimizer block; keeping today's optimizer. The limiters are unaffected").Len(); n != 1 {
		t.Fatalf("want the invalid block reported once across three cycles, got %d", n)
	}
	if n := logs.FilterMessage(shadowMessage).Len(); n != 0 {
		t.Fatalf("an invalid block still evaluated: %d lines", n)
	}
}
