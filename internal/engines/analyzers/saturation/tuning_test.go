package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/balance"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// defaultTuning is the tuning of a policy that sets none of it.
var defaultTuning = tuningOf(nil)

// A policy that sets no tuning runs on exactly the figures the analyzer, the
// ITL gate and the load-imbalance diagnostic were calibrated with before they
// were configurable. The literals are those constants as they stood, so a
// default that drifts fails here and not on a cluster.
func TestTuningDefaultsAreTheCalibratedConstants(t *testing.T) {
	want := tuning{
		AnalyzerTuning: config.AnalyzerTuning{
			SteadyGate: config.SteadyGateTuning{
				SteadyFor: config.PolicyDuration(2 * time.Minute), MaxKDrift: 0.10,
				Carry: config.PolicyDuration(5 * time.Minute), IdleRows: 2, MinSettledSamples: 3,
			},
			K2Settle: config.K2SettleTuning{RateFraction: 0.7, MaxRise: 0.25},
			MuChoice: config.MuChoiceTuning{
				MinMeasuredOfDerived: 0.5, MaxRatio: 2.0, ReturnRatio: 1.6,
				FreshFor: config.PolicyDuration(5 * time.Minute), LowHoldFor: config.PolicyDuration(30 * time.Minute),
			},
			Admission:     config.AdmissionTuning{MaxTokensOverCompletions: 1.15, SiblingMuTolerance: 0.8},
			FloorTrust:    config.FloorTrustTuning{LagFraction: 0.5, TrustFraction: 0.8},
			BacklogGrowth: config.BacklogGrowthTuning{MinRequests: 2, Fraction: 0.10},
		},
		idleKvUsage: 0.30,
	}
	resolved := config.ScalingPolicy{}
	resolved.ApplyDefaults()
	for name, got := range map[string]tuning{
		"nil policy":      tuningOf(nil),
		"empty policy":    tuningOf(&config.ScalingPolicy{}),
		"resolved policy": tuningOf(&resolved),
	} {
		assert.Equal(t, want, got, name)
	}

	// The packages' own constants, which the existing tests are written
	// against, are the same figures.
	assert.Equal(t, itl.DefaultSteadiness(), defaultTuning.steadiness())
	assert.Equal(t, itl.Steadiness{For: 2 * time.Minute, MaxKDrift: 0.10, Carry: 5 * time.Minute, IdleRows: 2,
		MinSettledSamples: 3}, itl.DefaultSteadiness())
	assert.Equal(t, balance.IdleKvUsage, defaultTuning.idleKvUsage)
	li := resolved.LoadImbalance
	assert.Equal(t, balance.IdleKvUsage, li.IdleKvUsage)
	assert.Equal(t, balance.IdleAfterReady, li.IdleAfterReady.Duration())
	assert.Equal(t, balance.SustainFor, li.SustainFor.Duration())
	assert.Equal(t, balance.ClearAfter, li.ClearAfter.Duration())
	assert.Equal(t, 2*time.Minute, balance.StaleAfter, "stale after is sustain for, which is unchanged")
	assert.Equal(t, 0.7, K2SettleRateFraction)
	assert.Equal(t, 0.25, K2SettleMaxRise)
	assert.Equal(t, time.Minute, K2SettleLookback)
	assert.Equal(t, 2*time.Minute, K2SettleMaxGap)
	assert.Equal(t, 2*time.Minute, IdleSettledFor)
	assert.Equal(t, time.Minute, IdleMaxGap)
	assert.Equal(t, 0.5, MinMeasuredOfDerived)
	assert.Equal(t, 2.0, MeasuredDerivedMaxRatio)
	assert.Equal(t, 1.6, MeasuredDerivedReturnRatio)
	assert.Equal(t, 5*time.Minute, MeasuredMuFreshFor)
	assert.Equal(t, 30*time.Minute, MeasuredLowHoldFor)
	assert.Equal(t, 1.15, MaxTokensOverCompletions)
	assert.Equal(t, 0.8, SiblingMuTolerance)
	assert.Equal(t, 0.5, FloorLagFraction)
	assert.Equal(t, 0.8, FloorTrustFraction)
	assert.Equal(t, 2.0, BacklogGrowthMinRequests)
	assert.Equal(t, 0.10, BacklogGrowthFraction)
}

// policyWith is the shipped test policy with tune applied to it.
func policyWith(tune func(*config.ScalingPolicy)) *config.ScalingPolicy {
	p := makeAnalyzerInput(nil, nil).Config.(*config.ScalingPolicy)
	tune(p)
	return p
}

// loadImbalance.idleKvUsage is the analyzer's idle test as well as the
// diagnostic's, read through Analyze. Four pods peak at 0.95 and three read
// 0.40 now: under the default 0.30 they are busy now and every peak is
// charged; at 0.50 they are idle now beside a busy sibling and are charged
// what they hold now.
func TestTuningIdleKvUsageReachesAnalyze(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "agg-v", AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1},
	}
	run := func(idle float64) float64 {
		t.Helper()
		a, _ := imbAnalyzer()
		rows := make([]domain.ReplicaMetrics, 0, 4)
		for i, k := range []float64{0.95, 0.40, 0.40, 0.40} {
			rm := imbRow("r"+string(rune('0'+i)), "agg-v", 0.95, 0, 1000, 1000, 0.5)
			rm.KvUsageInstant = k
			rows = append(rows, rm)
		}
		in := makeAnalyzerInput(rows, states)
		in.Config.(*config.ScalingPolicy).LoadImbalance.IdleKvUsage = idle
		r, err := a.Analyze(context.Background(), in)
		require.NoError(t, err)
		return r.TotalDemand
	}
	peaks := 4 * float64(int64(0.95*float64(imbKv)))
	def, tuned := run(0), run(0.50)
	t.Logf("demand: default idleKvUsage %.0f, idleKvUsage 0.50 %.0f, every peak %.0f", def, tuned, peaks)
	assert.Equal(t, peaks, def, "at the default the 0.40 pods are busy now")
	assert.Less(t, tuned, def, "at 0.50 the 0.40 pods are idle now and charged what they hold")
}

// analyzerTuning.floorTrust reaches the throughput floor. At lambda 0.8 the
// arrival half is 0.13 of the resident KV: under the default ramp (0.5-0.8)
// the floor may not lower demand; with the ramp at 0.05-0.10 it trusts the
// drain price in full.
func TestTuningFloorTrustReachesTheFloor(t *testing.T) {
	const resident, charge = 260_123.0, 239_323.0
	floor := func(cfg *config.ScalingPolicy) float64 {
		a := NewSaturationAnalyzer(capacity.NewStore())
		in := makeAnalyzerInput(nil, nil)
		in.ArrivalRate = 0.8
		rcs := []capacity.ReplicaCapacity{{
			VariantName: "v", PodName: "p", QueueLength: 160, LocalQueueDemand: int64(charge),
			SaturatedThroughput: 6, SaturatedThroughputSamples: 2,
		}}
		vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 260_622}}
		return a.applyThroughputFloor(in, cfg, rcs, vcs, resident+charge, nil, nil, 0, false, logr.Discard())
	}
	def := floor(policyWith(func(*config.ScalingPolicy) {}))
	tuned := floor(policyWith(func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.FloorTrust = config.FloorTrustTuning{LagFraction: 0.05, TrustFraction: 0.10}
	}))
	assert.InDelta(t, resident+charge, def, 1, "the default ramp does not trust a lagging lambda")
	assert.Less(t, tuned, def-1, "a ramp under 0.13 trusts it")
}

// analyzerTuning.backlogGrowth sets the band a backlog must grow past. 100 ->
// 105 over a window is inside the default max(2, 10 %), and outside max(1, 2 %).
func TestTuningBacklogGrowthBand(t *testing.T) {
	grew := func(cfg *config.ScalingPolicy) bool {
		a, clock := imbAnalyzer()
		tune := tuningOf(cfg)
		a.noteBacklogGrowth("ns", "m", map[string]float64{"decode": 100}, tune)
		*clock = clock.Add(BacklogGrowthLookback)
		return a.noteBacklogGrowth("ns", "m", map[string]float64{"decode": 105}, tune)["decode"]
	}
	assert.False(t, grew(policyWith(func(*config.ScalingPolicy) {})))
	assert.True(t, grew(policyWith(func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.BacklogGrowth = config.BacklogGrowthTuning{MinRequests: 1, Fraction: 0.02}
	})))
}

// analyzerTuning.k2Settle: a reading 30 % over its reading a window back has
// not settled under the default 25 % band and has under a 40 % one; and a
// suspect reading at 0.6 of the rate and k2 on record is admitted at a rate
// fraction of 0.5, not at the default 0.7.
func TestTuningK2Settle(t *testing.T) {
	start := time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC)
	trace := []settleReading{
		{at: start, queue: 40, tokens: 100_000},
		{at: start.Add(30 * time.Second), queue: 40, tokens: 115_000},
	}
	now := start.Add(time.Minute)
	wide := tuningOf(policyWith(func(p *config.ScalingPolicy) { p.AnalyzerTuning.K2Settle.MaxRise = 0.40 }))
	assert.False(t, defaultTuning.settledOverWindow(trace, now, 130_000, 5))
	assert.True(t, wide.settledOverWindow(trace, now, 130_000, 5))

	unsettled := func(tune tuning) string {
		a, _ := imbAnalyzer()
		return a.k2Unsettled(k2SettleInput{
			namespace: "ns", modelID: "m", variant: "v", pod: "p",
			queue: 100, tokens: 60_000, ranks: 1, perRequest: 2000, threshold: 5,
			rate: 1.2, rateOK: true, ref: 2.0, k2Ref: 100_000, tune: tune,
		})
	}
	loose := tuningOf(policyWith(func(p *config.ScalingPolicy) { p.AnalyzerTuning.K2Settle.RateFraction = 0.5 }))
	assert.NotEmpty(t, unsettled(defaultTuning), "0.6 of what is on record is short of 0.7")
	assert.Empty(t, unsettled(loose), "and reaches 0.5")
}

// analyzerTuning.admission and analyzerTuning.muChoice reach the throughput
// admission and the measured/derived choice.
func TestTuningAdmissionAndMuChoice(t *testing.T) {
	rm := domain.ReplicaMetrics{RequestRate: 5.0, GenerationTokenRate: 1.25 * 5.0 * 500, AvgOutputTokens: 500}
	lenient := tuningOf(policyWith(func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.Admission.MaxTokensOverCompletions = 1.30
	}))
	assert.True(t, defaultTuning.tokensOutrunCompletions(rm, domain.RoleDecode, 500), "1.25x is over the default 1.15")
	assert.False(t, lenient.tokensOutrunCompletions(rm, domain.RoleDecode, 500), "and under 1.30")

	derived := derivedMu{rate: 4.0, ok: true}
	strict := tuningOf(policyWith(func(p *config.ScalingPolicy) { p.AnalyzerTuning.MuChoice.MinMeasuredOfDerived = 0.8 }))
	assert.Empty(t, defaultTuning.throughputRefusal(1, 2.4, derived, 0, false), "0.6 of derived passes the default half")
	assert.Equal(t, refusedUnderDerived, strict.throughputRefusal(1, 2.4, derived, 0, false), "and not 0.8")

	// A stale measurement at 1.8x the derived figure keeps pricing under the
	// default ratio of 2 and gives way under 1.5.
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	reading := throughputReading{rate: 7.2, sampledAt: now.Add(-10 * time.Minute)}
	tight := tuningOf(policyWith(func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.MuChoice.MaxRatio = 1.5
		p.AnalyzerTuning.MuChoice.ReturnRatio = 1.2
	}))
	a := NewSaturationAnalyzer(capacity.NewStore())
	assert.False(t, a.measuredYieldsToDerived("k1", reading, derived, 0, defaultTuning, now))
	assert.True(t, a.measuredYieldsToDerived("k2", reading, derived, 0, tight, now))
	// And it is fresh for longer than ten minutes when freshFor says so.
	fresh := tuningOf(policyWith(func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.MuChoice.MaxRatio = 1.5
		p.AnalyzerTuning.MuChoice.ReturnRatio = 1.2
		p.AnalyzerTuning.MuChoice.FreshFor = config.PolicyDuration(15 * time.Minute)
	}))
	assert.False(t, a.measuredYieldsToDerived("k3", reading, derived, 0, fresh, now))
}

// analyzerTuning.steadyGate reaches the ITL tracker the analyzer fits with.
func TestTuningSteadyGateIsTheTrackers(t *testing.T) {
	p := policyWith(func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.SteadyGate = config.SteadyGateTuning{
			SteadyFor: config.PolicyDuration(3 * time.Minute), MaxKDrift: 0.05,
			Carry: config.PolicyDuration(10 * time.Minute), IdleRows: 4, MinSettledSamples: 6,
		}
	})
	assert.Equal(t, itl.Steadiness{For: 3 * time.Minute, MaxKDrift: 0.05, Carry: 10 * time.Minute, IdleRows: 4,
		MinSettledSamples: 6}, tuningOf(p).steadiness())
}
