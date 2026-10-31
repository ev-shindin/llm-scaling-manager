package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// unsized is a live, informative entry that defines no need for "v1": it
// reports the variant without a per-replica capacity.
func unsized(name string) allocation.NamedAnalyzerResult {
	return allocation.NamedAnalyzerResult{
		Name: name,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName:      name,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", Reason: "T1-ols"}},
		},
	}
}

// inGrace is a live entry with no reading this cycle: every variant it
// reports is no-data, so it is not eligible to contribute.
func inGrace(name string) allocation.NamedAnalyzerResult {
	return allocation.NamedAnalyzerResult{
		Name: name,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName:      name,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", Reason: allocation.ReasonNoData}},
		},
	}
}

// TestCompositeHoldsWhatNoListedAnalyzerSizes: with saturation unlisted, a
// model no listed analyzer can size neither grows nor shrinks, whatever
// saturation's own reading says. Sized on saturation, it grew on load
// (demand 800) and shrank when idle (demand 0).
func TestCompositeHoldsWhatNoListedAnalyzerSizes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		demand float64
	}{{"loaded", 800}, {"idle", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			sat := liveEntry(domain.SaturationAnalyzerName, tc.demand, 200)
			other := unsized("throughput")

			listed := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, other},
				[]allocation.NamedAnalyzerResult{sat, other}, 0.85, 0.7)
			require.Nil(t, listed.Holds, "saturation listed: nothing is held")
			require.Positive(t, listed.RequiredCapacity+listed.SpareCapacity, "setup: saturation's reading moves the model")

			held := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, other},
				[]allocation.NamedAnalyzerResult{other}, 0.85, 0.7)
			assert.Equal(t, map[string]allocation.RoleHold{domain.RoleBoth: {NoScaleUp: true, NoScaleDown: true}}, held.Holds)
			assert.Zero(t, held.RequiredCapacity)
			assert.Zero(t, held.SpareCapacity)
			assert.Zero(t, held.Remaining)
			assert.Zero(t, held.Spare)
			assert.False(t, allocation.CompositeHasSignal(held), "reported as no-composite-signal")
		})
	}
}

// TestCompositeGraceBlocksScaleDown: a listed analyzer inside its grace with
// no reading cannot ask for more this cycle, so the model does not shrink on
// the others' smaller need. Out of its grace, it no longer counts.
func TestCompositeGraceBlocksScaleDown(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 0, 200) // idle: would release
	grace := inGrace("throughput")

	held := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, grace},
		[]allocation.NamedAnalyzerResult{sat, grace}, 0.85, 0.7)
	assert.Equal(t, map[string]allocation.RoleHold{domain.RoleBoth: {NoScaleDown: true}}, held.Holds)
	assert.Zero(t, held.SpareCapacity)
	assert.Zero(t, held.Spare)

	grace.Live = false
	free := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, grace},
		[]allocation.NamedAnalyzerResult{sat, grace}, 0.85, 0.7)
	assert.Nil(t, free.Holds)
	assert.Positive(t, free.SpareCapacity)
}

// TestCompositeGraceKeepsScaleUp: the grace blocks only scale-down.
func TestCompositeGraceKeepsScaleUp(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 800, 200)
	grace := inGrace("throughput")

	composite := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, grace},
		[]allocation.NamedAnalyzerResult{sat, grace}, 0.85, 0.7)

	assert.InDelta(t, 800/0.85-600, composite.RequiredCapacity, 1e-9)
}

// TestCompositeGraceCoversOnlyItsRoles: on P/D, an analyzer in its grace that
// reports decode variants only blocks decode's scale-down, not prefill's.
func TestCompositeGraceCoversOnlyItsRoles(t *testing.T) {
	sat := allocation.NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName: domain.SaturationAnalyzerName,
			RoleDemand:   map[string]float64{domain.RolePrefill: 0, domain.RoleDecode: 0},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 2, PerReplicaCapacity: 100, Reason: "measured"},
				{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 300, Reason: "measured"},
			},
		},
	}
	grace := allocation.NamedAnalyzerResult{
		Name: "throughput",
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName:      "throughput",
			VariantCapacities: []domain.VariantCapacity{{VariantName: "d", Role: domain.RoleDecode, Reason: allocation.ReasonNoData}},
		},
	}

	composite := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, grace},
		[]allocation.NamedAnalyzerResult{sat, grace}, 0.85, 0.7)

	assert.Equal(t, map[string]allocation.RoleHold{domain.RoleDecode: {NoScaleDown: true}}, composite.Holds)
	assert.Zero(t, composite.RoleCapacities[domain.RoleDecode].SpareCapacity)
	assert.Positive(t, composite.RoleCapacities[domain.RolePrefill].SpareCapacity)
}

// TestCompositeSaturationsOwnGraceIsUnchanged: saturation inside its own
// grace does not block scale-down; that is decided as it was before the
// composite existed.
func TestCompositeSaturationsOwnGraceIsUnchanged(t *testing.T) {
	sat := inGrace(domain.SaturationAnalyzerName)

	composite := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat},
		[]allocation.NamedAnalyzerResult{sat}, 0.85, 0.7)

	assert.Nil(t, composite.Holds)
}

// TestCompositeGraceCoversAnAnalyzerThatReturnedNothing: an analyzer whose
// run failed leaves no result, and still blocks scale-down while its last
// informative result is inside the staleness window.
func TestCompositeGraceCoversAnAnalyzerThatReturnedNothing(t *testing.T) {
	sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: liveEntry(domain.SaturationAnalyzerName, 0, 200).Result}
	tp := &fakeAnalyzerWithResult{analyzerName: "throughput", result: liveEntry("throughput", 0, 200).Result}
	e := &Engine{analyzers: testAnalyzers(sat, analyzerEntry{name: "throughput", analyzer: tp})}
	cfg := config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
		Analyzers: []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}, {Name: "throughput"}}}
	cycle := func() *allocation.ModelScalingRequest {
		t.Helper()
		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
		require.NoError(t, err)
		return req
	}

	require.Positive(t, cycle().CompositeSignal.SpareCapacity, "setup: both analyzers agree the model is idle")

	tp.result = nil // the run fails: no entry this cycle
	req := cycle()
	assert.Equal(t, map[string]allocation.RoleHold{domain.RoleBoth: {NoScaleDown: true}}, req.CompositeSignal.Holds)
	assert.Zero(t, req.CompositeSignal.SpareCapacity)

	e.lastGoodAnalysis[utils.GetNamespacedKey("ns", "m")]["throughput"] = time.Now().Add(-time.Hour)
	req = cycle()
	assert.Nil(t, req.CompositeSignal.Holds, "out of its grace, it no longer counts")
	assert.Positive(t, req.CompositeSignal.SpareCapacity)
}

// TestCompositeNoAnalyzersSectionIsSaturation: a policy with no analyzers
// section runs saturation alone, so nothing is held.
func TestCompositeNoAnalyzersSectionIsSaturation(t *testing.T) {
	e := satOnlyEngine(liveEntry(domain.SaturationAnalyzerName, 800, 200).Result)

	req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	assert.Nil(t, req.CompositeSignal.Holds)
	assert.Positive(t, req.CompositeSignal.RequiredCapacity)
}

// TestCompositeScoreIsTheContributorsOnly: an analyzer that sized nothing
// cannot raise the composite's score, which the cross-model fair share reads.
func TestCompositeScoreIsTheContributorsOnly(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 800, 200)
	sat.Score = 1
	other := unsized("throughput")
	other.Score = 5
	named := []allocation.NamedAnalyzerResult{sat, other}

	assert.Equal(t, 1.0, buildComposite(context.Background(), named, named, 0.85, 0.7).Score)

	sizing := liveEntry("throughput", 60, 10)
	sizing.Score = 5
	named = []allocation.NamedAnalyzerResult{sat, sizing}
	assert.Equal(t, 5.0, buildComposite(context.Background(), named, named, 0.85, 0.7).Score)
}

// TestCompositeLiveFromContributorsOnly: the composite is live through an
// analyzer that contributed a need, not one that is merely live. With no
// contributor it is as live as saturation.
func TestCompositeLiveFromContributorsOnly(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 800, 200)
	sat.Live = false
	other := unsized("throughput")
	named := []allocation.NamedAnalyzerResult{sat, other}

	assert.False(t, buildComposite(context.Background(), named, []allocation.NamedAnalyzerResult{other}, 0.85, 0.7).Live)

	sizing := liveEntry("throughput", 60, 10)
	named = []allocation.NamedAnalyzerResult{sat, sizing}
	assert.True(t, buildComposite(context.Background(), named, []allocation.NamedAnalyzerResult{sizing}, 0.85, 0.7).Live)

	sat.Live = true
	named = []allocation.NamedAnalyzerResult{sat, other}
	assert.True(t, buildComposite(context.Background(), named, []allocation.NamedAnalyzerResult{other}, 0.85, 0.7).Live)
}

// TestCompositeKeepsSaturationsHoldWhenAnotherFramePrices: a role saturation
// holds keeps a scale-down hold when another analyzer's frame prices it and
// the held figure is dropped. Prefill holds 10 replicas at 100, saturation
// holds it at 300 with demand 700, and throughput reads 750: priced in
// throughput's frame, prefill went from 10 GPUs to 5 under contention while
// saturation alone kept all 10. Saturation alone adds no hold, and an
// unlisted saturation's hold does not count.
func TestCompositeKeepsSaturationsHoldWhenAnotherFramePrices(t *testing.T) {
	mk := func(name string, prefill float64, vcs ...domain.VariantCapacity) allocation.NamedAnalyzerResult {
		e := variantsEntry(name, prefill+300, vcs...)
		e.Result.RoleDemand = map[string]float64{domain.RolePrefill: prefill, domain.RoleDecode: 300}
		return e
	}
	sat := mk(domain.SaturationAnalyzerName, 700,
		domain.VariantCapacity{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 10, PerReplicaCapacity: 100, TotalDemand: 700},
		domain.VariantCapacity{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 4, PerReplicaCapacity: 100, TotalDemand: 300})
	sat.Result.HeldRoleDemand = map[string]float64{domain.RolePrefill: 300}
	thr := mk("throughput", 750,
		domain.VariantCapacity{VariantName: "p", Role: domain.RolePrefill, PerReplicaCapacity: 100},
		domain.VariantCapacity{VariantName: "d", Role: domain.RoleDecode, PerReplicaCapacity: 100})
	ctx := context.Background()

	alone := buildComposite(ctx, []allocation.NamedAnalyzerResult{sat}, []allocation.NamedAnalyzerResult{sat}, 0.85, 0.7)
	assert.Contains(t, alone.Result.HeldRoleDemand, domain.RolePrefill, "setup: saturation alone keeps its held figure")
	assert.False(t, alone.Holds[domain.RolePrefill].NoScaleDown, "saturation alone decides as before")

	named := []allocation.NamedAnalyzerResult{sat, thr}
	both := buildComposite(ctx, named, named, 0.85, 0.7)
	assert.NotContains(t, both.Result.HeldRoleDemand, domain.RolePrefill, "setup: priced in throughput's frame")
	assert.True(t, both.Holds[domain.RolePrefill].NoScaleDown)
	assert.False(t, both.Holds[domain.RolePrefill].NoScaleUp, "growth is unaffected")
	assert.False(t, both.Holds[domain.RoleDecode].NoScaleDown, "a role saturation does not hold is not held")

	unlisted := buildComposite(ctx, named, []allocation.NamedAnalyzerResult{thr}, 0.85, 0.7)
	assert.False(t, unlisted.Holds[domain.RolePrefill].NoScaleDown, "an unlisted saturation does not hold")

	stale := sat
	stale.Live = false
	named = []allocation.NamedAnalyzerResult{stale, thr}
	notEligible := buildComposite(ctx, named, named, 0.85, 0.7)
	assert.False(t, notEligible.Holds[domain.RolePrefill].NoScaleDown, "a saturation that is not eligible blocks nothing")
}
