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

// TestCompositeFreezesGrowthOfBothPDRolesWhenOneIsHeld: with saturation
// unlisted and a listed analyzer that sizes decode only, prefill holds both
// ways and decode does not grow either. Grown against a frozen prefill,
// decode is capacity prefill cannot feed; the share optimizer already froze
// such a model whole, so cost-aware and greedy grew decode alone.
func TestCompositeFreezesGrowthOfBothPDRolesWhenOneIsHeld(t *testing.T) {
	sat := pdEntry(domain.SaturationAnalyzerName,
		map[string]float64{domain.RolePrefill: 400, domain.RoleDecode: 900}, 100)
	other := pdEntry("throughput", map[string]float64{domain.RoleDecode: 900}, 100)

	composite := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, other},
		[]allocation.NamedAnalyzerResult{other}, 0.85, 0.7)

	assert.Equal(t, map[string]allocation.RoleHold{
		domain.RolePrefill: {NoScaleUp: true, NoScaleDown: true},
		domain.RoleDecode:  {NoScaleUp: true},
	}, composite.Holds)
	assert.Zero(t, composite.RoleCapacities[domain.RoleDecode].RequiredCapacity, "decode 900 against 600 would grow")
	assert.Zero(t, composite.RequiredCapacity)

	// Both roles sized: nothing is held.
	both := pdEntry("throughput", map[string]float64{domain.RolePrefill: 400, domain.RoleDecode: 900}, 100)
	sized := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, both},
		[]allocation.NamedAnalyzerResult{both}, 0.85, 0.7)
	assert.Nil(t, sized.Holds)
}

// TestCompositeDropsAHoldTheOthersOverride: when another analyzer asks for at
// least saturation's held figure, saturation's hold no longer sets the
// demand, and the role is not reported as held: the share optimizer gives
// nothing from a held role.
func TestCompositeDropsAHoldTheOthersOverride(t *testing.T) {
	sat := pdEntry(domain.SaturationAnalyzerName,
		map[string]float64{domain.RolePrefill: 500, domain.RoleDecode: 900}, 100)
	sat.Result.HeldRoleDemand = map[string]float64{domain.RolePrefill: 300}
	for _, demand := range []float64{500, 600} {
		other := pdEntry("slo", map[string]float64{domain.RolePrefill: demand}, 100)
		named := []allocation.NamedAnalyzerResult{sat, other}
		got := buildComposite(context.Background(), named, named, 0.85, 0.7).Result
		assert.Equal(t, demand, got.RoleDemand[domain.RolePrefill])
		assert.NotContains(t, got.HeldRoleDemand, domain.RolePrefill, "other demand %v", demand)
	}
}

// TestVariantPressureFollowsTheComposite: the retained pool reads the
// optimizer's view. Published from saturation, a variant another analyzer is
// growing read as comfortable, and the pool could switch its GPUs away.
func TestVariantPressureFollowsTheComposite(t *testing.T) {
	decision.DefaultPressure.Reset()
	t.Cleanup(decision.DefaultPressure.Reset)
	fakeSat := &fakeAnalyzerWithResult{
		analyzerName: domain.SaturationAnalyzerName,
		result: &domain.AnalyzerResult{
			TotalDemand:       100,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 200, TotalDemand: 100, Utilization: 0.5, Reason: "P1-obs"}},
		},
	}
	spy := &fakeAnalyzerWithResult{
		analyzerName: "spy",
		result: &domain.AnalyzerResult{
			TotalDemand:       4000,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 200, Reason: "measured"}},
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
	_, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	p, ok := decision.PressureFor("ns", "v1", time.Minute, time.Now())
	require.True(t, ok)
	assert.True(t, p.NeedsScaleUp, "spy needs 20 replicas, 1 is running")
}

// TestVariantPressureHonoursAHold: a role held against growth is not being
// grown, so it does not ask the pool for capacity.
func TestVariantPressureHonoursAHold(t *testing.T) {
	decision.DefaultPressure.Reset()
	t.Cleanup(decision.DefaultPressure.Reset)
	nr := allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{VariantCapacities: []domain.VariantCapacity{
			{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: 100, Utilization: 3},
			{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 100, Utilization: 3},
		}},
		Holds: map[string]allocation.RoleHold{domain.RolePrefill: {NoScaleUp: true}},
	}
	publishVariantPressure("ns", nr, 0.85)
	prefill, _ := decision.PressureFor("ns", "p", time.Minute, time.Now())
	decode, _ := decision.PressureFor("ns", "d", time.Minute, time.Now())
	assert.False(t, prefill.NeedsScaleUp)
	assert.True(t, decode.NeedsScaleUp)
}

// TestCompositeSaysWhenNothingListedCanSize: a policy that leaves saturation
// out and lists only slo, which observes, holds every model; the log says so,
// not only the blocked-reason metric.
func TestCompositeSaysWhenNothingListedCanSize(t *testing.T) {
	const msg = "no listed analyzer can size this model this cycle, so it holds where it is"
	sat := liveEntry(domain.SaturationAnalyzerName, 800, 200)
	ctx, logs := zapObserverCtx(t)
	held := buildComposite(ctx, []allocation.NamedAnalyzerResult{sat}, nil, 0.85, 0.7)
	assert.Len(t, logs.FilterMessage(msg).All(), 1)
	assert.Equal(t, allocation.RoleHold{NoScaleUp: true, NoScaleDown: true}, held.Holds[domain.RoleBoth])

	ctx, logs = zapObserverCtx(t)
	buildComposite(ctx, []allocation.NamedAnalyzerResult{sat}, []allocation.NamedAnalyzerResult{sat}, 0.85, 0.7)
	assert.Empty(t, logs.FilterMessage(msg).All())
}

// TestCompositeSaysWhenANeedIsDropped: a listed analyzer's need on a variant
// saturation has no per-replica capacity for sizes nothing, and is logged.
func TestCompositeSaysWhenANeedIsDropped(t *testing.T) {
	const msg = "a listed analyzer's need is dropped: saturation has no per-replica capacity for the variant"
	named := []allocation.NamedAnalyzerResult{liveEntry(domain.SaturationAnalyzerName, 800, 0), liveEntry("throughput", 60, 10)}
	ctx, logs := zapObserverCtx(t)
	buildComposite(ctx, named, named, 0.85, 0.7)
	if lines := logs.FilterMessage(msg).All(); assert.Len(t, lines, 1) {
		assert.Equal(t, "v1", lines[0].ContextMap()["variant"])
	}

	named[0] = liveEntry(domain.SaturationAnalyzerName, 800, 200)
	ctx, logs = zapObserverCtx(t)
	buildComposite(ctx, named, named, 0.85, 0.7)
	assert.Empty(t, logs.FilterMessage(msg).All())
}

// TestCompositeSignalsUseTheBandItIsGiven: RC and SC, which the optimizers
// consume, come from the band buildComposite is given, not only the fields
// that report it.
func TestCompositeSignalsUseTheBandItIsGiven(t *testing.T) {
	named := []allocation.NamedAnalyzerResult{liveEntry(domain.SaturationAnalyzerName, 450, 200)} // 0.75 of 600
	at := func(up, down float64) allocation.NamedAnalyzerResult {
		return buildComposite(context.Background(), named, named, up, down)
	}
	assert.InDelta(t, 450/0.6-600, at(0.6, 0.5).RequiredCapacity, 1e-9)
	assert.Zero(t, at(0.85, 0.7).RequiredCapacity)
	assert.InDelta(t, 600-450/0.8, at(0.9, 0.8).SpareCapacity, 1e-9)
	assert.Zero(t, at(0.85, 0.7).SpareCapacity)
}

// TestBuildCompositeIgnoresANegativeNeed: a negative need is no reading.
// Alone, it would set a negative role demand.
func TestBuildCompositeIgnoresANegativeNeed(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 800, 200)
	other := liveEntry("throughput", -60, 10)
	named := []allocation.NamedAnalyzerResult{sat, other}

	unlisted := buildComposite(context.Background(), named, []allocation.NamedAnalyzerResult{other}, 0.85, 0.7)
	assert.Equal(t, string(allocation.DecisionNoSignal), unlisted.Result.VariantCapacities[0].Reason)
	assert.Equal(t, allocation.RoleHold{NoScaleUp: true, NoScaleDown: true}, unlisted.Holds[domain.RoleBoth])

	listed := buildComposite(context.Background(), named, named, 0.85, 0.7)
	assert.Equal(t, 800.0, listed.Result.TotalDemand)
}

// TestBuildCompositeDropsSaturationsServiceRate: saturation's service-rate
// signal is published from its own entry; the composite does not price on
// it and must not carry it.
func TestBuildCompositeDropsSaturationsServiceRate(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 800, 200)
	sat.Result.VariantCapacities[0].ServiceRate = &domain.ServiceRateSignal{}
	for _, named := range [][]allocation.NamedAnalyzerResult{
		{sat},
		{sat, liveEntry("throughput", 60, 10)},
	} {
		composite := buildComposite(context.Background(), named, named, 0.85, 0.7)
		assert.Nil(t, composite.Result.VariantCapacities[0].ServiceRate)
	}
	assert.NotNil(t, sat.Result.VariantCapacities[0].ServiceRate, "saturation's own is kept")
}

// TestBuildCompositeClonesEveryMap: writing the composite's maps never
// reaches saturation's result.
func TestBuildCompositeClonesEveryMap(t *testing.T) {
	sat := pdEntry(domain.SaturationAnalyzerName,
		map[string]float64{domain.RolePrefill: 400, domain.RoleDecode: 900}, 100)
	sat.Result.RoleStartSeconds = map[string]float64{domain.RolePrefill: 120}
	sat.Result.VariantBusiestQueueLoad = map[string]float64{"p": 7}
	named := []allocation.NamedAnalyzerResult{sat}
	got := buildComposite(context.Background(), named, named, 0.85, 0.7).Result

	got.RoleStartSeconds[domain.RolePrefill] = 0
	got.VariantBusiestQueueLoad["p"] = 0
	assert.Equal(t, 120.0, sat.Result.RoleStartSeconds[domain.RolePrefill])
	assert.Equal(t, 7.0, sat.Result.VariantBusiestQueueLoad["p"])
}
