package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// TestWarmPoolSupplyIsInThePressuresFrame (#221): warm-pool supply and
// pressure come from one result, the composite. Saturation rates a and b
// alike, at 200; spy asks for 20 of a and 10 of b, so the role is priced in
// spy's frame, where b is worth 400. Published from saturation's entry, b's
// one bridge read 200 while b's pressure was in spy's frame.
func TestWarmPoolSupplyIsInThePressuresFrame(t *testing.T) {
	decision.DefaultWarmPoolSupply.Reset()
	t.Cleanup(decision.DefaultWarmPoolSupply.Reset)
	decision.DefaultPressure.Reset()
	t.Cleanup(decision.DefaultPressure.Reset)
	fakeSat := &fakeAnalyzerWithResult{
		analyzerName: domain.SaturationAnalyzerName,
		result: &domain.AnalyzerResult{
			TotalDemand: 100,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "a", ReplicaCount: 1, PerReplicaCapacity: 200, TotalDemand: 50, Reason: "P1-obs"},
				{VariantName: "b", ReplicaCount: 1, PerReplicaCapacity: 200, TotalDemand: 50, Reason: "P1-obs",
					WarmPoolReplicas: 1, WarmPoolPerReplicaCapacity: 200},
			},
		},
	}
	spy := &fakeAnalyzerWithResult{
		analyzerName: "spy",
		result: &domain.AnalyzerResult{
			TotalDemand: 4000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "a", ReplicaCount: 1, PerReplicaCapacity: 200, Reason: "measured"},
				{VariantName: "b", ReplicaCount: 1, PerReplicaCapacity: 400, Reason: "measured"},
			},
		},
	}
	e := &Engine{
		analyzers: testAnalyzers(fakeSat,
			analyzerEntry{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
			analyzerEntry{name: "spy", analyzer: spy},
		),
	}
	cfg := config.ScalingPolicy{
		ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
		Analyzers: []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}, {Name: "spy"}},
	}
	req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	b, ok := allocation.FindVariantCapacity(req.CompositeSignal.Result, "b")
	require.True(t, ok)
	require.InDelta(t, 400, b.PerReplicaCapacity, 1e-9, "setup: b is priced in spy's frame")

	supply, ok := decision.WarmPoolSupplyFor("ns", "b", time.Minute, time.Now())
	require.True(t, ok)
	assert.Equal(t, 1, supply.Replicas)
	assert.InDelta(t, 400, supply.Capacity, 1e-9, "the bridge is worth one of b's replicas in the frame b is priced in")
	assert.InDelta(t, b.WarmPoolCapacity, supply.Capacity, 1e-9, "published from the composite")
}

// TestWarmPoolSupplyIsPublishedWhenAnotherAnalyzerFails pins that supply
// still reaches the pool on a cycle another listed analyzer fails: the
// composite is built without that analyzer, so publishing from it loses
// nothing publishing from saturation's entry had.
func TestWarmPoolSupplyIsPublishedWhenAnotherAnalyzerFails(t *testing.T) {
	decision.DefaultWarmPoolSupply.Reset()
	t.Cleanup(decision.DefaultWarmPoolSupply.Reset)
	fakeSat := &fakeAnalyzerWithResult{
		analyzerName: domain.SaturationAnalyzerName,
		result: &domain.AnalyzerResult{
			TotalDemand: 100,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "b", ReplicaCount: 1, PerReplicaCapacity: 200,
				TotalDemand: 100, Reason: "P1-obs", WarmPoolReplicas: 1, WarmPoolPerReplicaCapacity: 200}},
		},
	}
	failing := &fakeAnalyzerWithResult{analyzerName: "spy"} // no result: the run failed
	e := &Engine{
		analyzers: testAnalyzers(fakeSat,
			analyzerEntry{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
			analyzerEntry{name: "spy", analyzer: failing},
		),
	}
	cfg := config.ScalingPolicy{
		ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
		Analyzers: []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}, {Name: "spy"}},
	}
	_, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	supply, ok := decision.WarmPoolSupplyFor("ns", "b", time.Minute, time.Now())
	require.True(t, ok, "published")
	assert.InDelta(t, 200, supply.Capacity, 1e-9)
}
