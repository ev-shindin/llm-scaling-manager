package steadystate

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

const shadowMessage = "Shadow: utilization share would rebalance"

const selectedShadow = "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    shadow: true\n"

// shadowConfig is a cluster-scope quota of 16 A100s, optionally with the
// utilization-share optimizer selected.
func shadowConfig(t *testing.T, optimizer string) *config.Config {
	t.Helper()
	c := config.NewTestConfig()
	setShadowPolicy(t, c, optimizer)
	return c
}

func setShadowPolicy(t *testing.T, c *config.Config, optimizer string) {
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
	c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: p})
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

func fullQuota() []*allocation.ResourceConstraints {
	return []*allocation.ResourceConstraints{{Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16, Used: 16}}}}
}

func observe(t *testing.T) (context.Context, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.InfoLevel)
	return logr.NewContext(t.Context(), zapr.NewLogger(zap.New(core))), logs
}

// gauges returns a family's series keyed by model_name (or scope, for group
// series), after initialising the metrics on a fresh registry.
func freshMetrics(t *testing.T) *prometheus.Registry {
	t.Helper()
	r := prometheus.NewRegistry()
	if err := metrics.InitMetrics(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func family(t *testing.T, r *prometheus.Registry, name string) []*dto.Metric {
	t.Helper()
	mfs, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()
		}
	}
	return nil
}

func label(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// §5.7's step: B's demand doubles while the fleet still holds 9 / 5 / 2. The
// shadow evaluation must say a rebalance would move three replicas and that B
// is the role a move would fix, at Info so it is seen at the default verbosity,
// and publish the same in metrics.
func TestUtilizationShareShadowReportsAWouldBeRebalance(t *testing.T) {
	reg := freshMetrics(t)
	ctx, logs := observe(t)
	e := &Engine{Config: shadowConfig(t, selectedShadow)}
	reqs := []allocation.ModelScalingRequest{
		shadowRequest("A", "important", 4000, 9),
		shadowRequest("B", "", 6000, 5),
		shadowRequest("C", "", 1000, 2),
	}

	e.evaluateUtilizationShare(ctx, reqs, fullQuota())

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
	// B is short of its target and A, holding 9 against 6.2, is the donor a
	// move would take from: both are actionable. C holds 2 against 1.4, but 2 is
	// its whole-replica target, so no move could fix it.
	if a, ok := fields["actionable"].([]any); !ok || len(a) != 2 || a[0] != "ns/A/both" || a[1] != "ns/B/both" {
		t.Errorf("actionable = %v, want [ns/A/both ns/B/both]", fields["actionable"])
	}

	actionable := map[string]float64{}
	for _, m := range family(t, reg, constants.WVAUtilizationShareActionable) {
		actionable[label(m, constants.LabelModelName)] = m.GetGauge().GetValue()
	}
	if len(actionable) != 3 || actionable["A"] != 1 || actionable["B"] != 1 || actionable["C"] != 0 {
		t.Errorf("actionable series = %v, want A=1, B=1, C=0", actionable)
	}
	move := family(t, reg, constants.WVAUtilizationShareReplicasToMove)
	if len(move) != 1 || move[0].GetGauge().GetValue() != 3 || label(move[0], constants.LabelScope) != constants.UtilizationShareClusterScope {
		t.Errorf("replicas_to_move = %v, want one cluster series at 3", move)
	}
}

// A settled fleet stays quiet at Info, and the evaluation still ran: every role
// is published, none actionable. Without the positive control an early return
// would pass this test too.
func TestUtilizationShareShadowIsQuietWhenSettled(t *testing.T) {
	reg := freshMetrics(t)
	ctx, logs := observe(t)
	e := &Engine{Config: shadowConfig(t, "optimizer:\n  type: utilizationShare\n")}
	reqs := []allocation.ModelScalingRequest{
		shadowRequest("A", "important", 4000, 9),
		shadowRequest("B", "", 3000, 5),
		shadowRequest("C", "", 1000, 2),
	}

	e.evaluateUtilizationShare(ctx, reqs, fullQuota())

	if n := logs.FilterMessage(shadowMessage).Len(); n != 0 {
		t.Fatalf("a settled fleet wrote %d would-rebalance lines", n)
	}
	series := family(t, reg, constants.WVAUtilizationShareActionable)
	if len(series) != 3 {
		t.Fatalf("want 3 published roles, got %d -- did the evaluation run?", len(series))
	}
	for _, m := range series {
		if m.GetGauge().GetValue() != 0 {
			t.Errorf("%s is actionable in a settled fleet", label(m, constants.LabelModelName))
		}
	}
}

// Not selected: nothing is evaluated and nothing is published. An invalid block
// is reported once, not every cycle, evaluates nothing, and fixing it is
// reported once too.
func TestUtilizationShareShadowOnlyWhenSelected(t *testing.T) {
	reqs := []allocation.ModelScalingRequest{shadowRequest("B", "", 6000, 5), shadowRequest("A", "", 4000, 11)}

	reg := freshMetrics(t)
	ctx, logs := observe(t)
	e := &Engine{Config: shadowConfig(t, "")}
	e.evaluateUtilizationShare(ctx, reqs, fullQuota())
	if n := logs.FilterMessage(shadowMessage).Len(); n != 0 {
		t.Fatalf("an unselected optimizer wrote %d lines", n)
	}
	if n := len(family(t, reg, constants.WVAUtilizationShareTargetGPUs)); n != 0 {
		t.Fatalf("an unselected optimizer published %d series", n)
	}

	ctx, logs = observe(t)
	c := shadowConfig(t, "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    tolerance: 2\n")
	e = &Engine{Config: c}
	for range 3 {
		e.evaluateUtilizationShare(ctx, reqs, fullQuota())
	}
	if n := logs.FilterMessage("Invalid optimizer block; keeping today's optimizer. The limiters are unaffected").Len(); n != 1 {
		t.Fatalf("want the invalid block reported once across three cycles, got %d", n)
	}
	if n := logs.FilterMessage(shadowMessage).Len(); n != 0 {
		t.Fatalf("an invalid block still evaluated: %d lines", n)
	}
	setShadowPolicy(t, c, selectedShadow)
	for range 2 {
		e.evaluateUtilizationShare(ctx, reqs, fullQuota())
	}
	if n := logs.FilterMessage("Optimizer block is valid again").Len(); n != 1 {
		t.Fatalf("want the fix reported once, got %d", n)
	}
}

// A cycle that stops selecting the optimizer clears what the last one published:
// a stale headroom reading would describe a fleet nothing is sizing.
func TestUtilizationShareShadowClearsWhenDeselected(t *testing.T) {
	reg := freshMetrics(t)
	ctx, _ := observe(t)
	c := shadowConfig(t, selectedShadow)
	e := &Engine{Config: c}
	reqs := []allocation.ModelScalingRequest{shadowRequest("A", "", 4000, 9)}

	e.evaluateUtilizationShare(ctx, reqs, fullQuota())
	if n := len(family(t, reg, constants.WVAUtilizationShareTargetGPUs)); n != 1 {
		t.Fatalf("want one series while selected, got %d", n)
	}
	setShadowPolicy(t, c, "")
	e.evaluateUtilizationShare(ctx, reqs, fullQuota())
	if n := len(family(t, reg, constants.WVAUtilizationShareTargetGPUs)); n != 0 {
		t.Fatalf("want no series once deselected, got %d", n)
	}
}

// Standing conditions are reported once per change, not once per cycle: the
// shadow-only warning when shadow is off, and a model's bad weight.
func TestUtilizationShareShadowReportsStandingConditionsOnce(t *testing.T) {
	ctx, logs := observe(t)
	e := &Engine{Config: shadowConfig(t, "optimizer:\n  type: utilizationShare\n")}
	reqs := []allocation.ModelScalingRequest{shadowRequest("A", "gold", 4000, 9), shadowRequest("B", "", 3000, 5)}
	for range 3 {
		e.evaluateUtilizationShare(ctx, reqs, fullQuota())
	}
	if n := logs.FilterMessageSnippet("actuation is not built yet").Len(); n != 1 {
		t.Errorf("want the shadow-only warning once across three cycles, got %d", n)
	}
	weight := logs.FilterMessageSnippet("invalid weight").All()
	if len(weight) != 1 {
		t.Fatalf("want the bad weight reported once across three cycles, got %d", len(weight))
	}
	if weight[0].ContextMap()["model"] != "ns/A" {
		t.Errorf("weight warning names %v, want ns/A", weight[0].ContextMap()["model"])
	}
}

// §8.2 and the PR #121 security review: a weight a tenant writes in its own
// namespace's map must not count in the cluster group, where it would be
// weighed against every other tenant's. In that namespace's own quota group --
// a budget the tenant owns whole -- it counts.
func TestUtilizationShareShadowScopesTenantWeights(t *testing.T) {
	c := shadowConfig(t, selectedShadow)
	c.UpdateScalingPolicyConfigForNamespace("ns", map[string]config.ScalingPolicy{
		config.GlobalDefaultsKey: {WeightClass: "critical"},
	})
	reqs := []allocation.ModelScalingRequest{shadowRequest("A", "critical", 4000, 9), shadowRequest("B", "", 3000, 5)}

	ctx, logs := observe(t)
	e := &Engine{Config: c}
	e.evaluateUtilizationShare(ctx, reqs, fullQuota())
	ignored := logs.FilterMessageSnippet("invalid weight").All()
	if len(ignored) != 1 || ignored[0].ContextMap()["model"] != "ns/A" {
		t.Fatalf("want the tenant's weight on ns/A reported as ignored in the cluster group, got %v", ignored)
	}

	ctx, logs = observe(t)
	e = &Engine{Config: c}
	nsQuota := []*allocation.ResourceConstraints{{
		NamespacePools: map[string]map[string]allocation.ResourcePool{"ns": {"A100": {Limit: 16, Used: 14}}},
	}}
	e.evaluateUtilizationShare(ctx, reqs, nsQuota)
	if n := logs.FilterMessageSnippet("invalid weight").Len(); n != 0 {
		t.Fatalf("the tenant's weight must count in its own quota group, got %d warnings", n)
	}
}
