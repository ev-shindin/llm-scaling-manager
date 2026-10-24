package config

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// The blocks decode from the ConfigMap's YAML, durations as Go duration
// strings, and an entry with them reports no unknown key.
func TestAnalyzerTuningDecodes(t *testing.T) {
	entry := `
kvCacheThreshold: 0.8
analyzerTuning:
  steadyGate: {steadyFor: 3m, maxKDrift: 0.05, carry: 6m, idleRows: 3, minSettledSamples: 4}
  k2Settle: {rateFraction: 0.8, maxRise: 0.2}
  muChoice: {minMeasuredOfDerived: 0.6, maxRatio: 2.5, returnRatio: 2.0, freshFor: 4m, lowHoldFor: 20m}
  admission: {maxTokensOverCompletions: 1.2, siblingMuTolerance: 0.9}
  floorTrust: {lagFraction: 0.4, trustFraction: 0.9}
  backlogGrowth: {minRequests: 3, fraction: 0.2}
loadImbalance: {idleKvUsage: 0.25, idleAfterReady: 3m, sustainFor: 4m, clearAfter: 90s}
`
	if unknown := UnknownScalingPolicyKeys(entry); len(unknown) != 0 {
		t.Fatalf("unknown keys: %v", unknown)
	}
	p, err := DecodeScalingPolicy(entry)
	if err != nil {
		t.Fatal(err)
	}
	want := AnalyzerTuning{
		SteadyGate: SteadyGateTuning{SteadyFor: PolicyDuration(3 * time.Minute), MaxKDrift: 0.05,
			Carry: PolicyDuration(6 * time.Minute), IdleRows: 3, MinSettledSamples: 4},
		K2Settle: K2SettleTuning{RateFraction: 0.8, MaxRise: 0.2},
		MuChoice: MuChoiceTuning{MinMeasuredOfDerived: 0.6, MaxRatio: 2.5, ReturnRatio: 2.0,
			FreshFor: PolicyDuration(4 * time.Minute), LowHoldFor: PolicyDuration(20 * time.Minute)},
		Admission:     AdmissionTuning{MaxTokensOverCompletions: 1.2, SiblingMuTolerance: 0.9},
		FloorTrust:    FloorTrustTuning{LagFraction: 0.4, TrustFraction: 0.9},
		BacklogGrowth: BacklogGrowthTuning{MinRequests: 3, Fraction: 0.2},
	}
	if p.AnalyzerTuning != want {
		t.Errorf("analyzerTuning = %+v, want %+v", p.AnalyzerTuning, want)
	}
	wantLI := LoadImbalance{IdleKvUsage: 0.25, IdleAfterReady: PolicyDuration(3 * time.Minute),
		SustainFor: PolicyDuration(4 * time.Minute), ClearAfter: PolicyDuration(90 * time.Second)}
	if p.LoadImbalance != wantLI {
		t.Errorf("loadImbalance = %+v, want %+v", p.LoadImbalance, wantLI)
	}
	if err := p.ValidateEntry(); err != nil {
		t.Errorf("valid entry rejected: %v", err)
	}
	if _, err := DecodeScalingPolicy("loadImbalance: {sustainFor: 120}"); err == nil {
		t.Error("a bare number was read as a duration")
	}
}

// Every range Validate checks, one bad value at a time; the error names the
// key. Each value sits just outside its range.
func TestAnalyzerTuningValidateRanges(t *testing.T) {
	d := func(x time.Duration) PolicyDuration { return PolicyDuration(x) }
	cases := map[string]func(*ScalingPolicy){
		"analyzerTuning.steadyGate.steadyFor":               func(p *ScalingPolicy) { p.AnalyzerTuning.SteadyGate.SteadyFor = d(59 * time.Second) },
		"analyzerTuning.steadyGate.maxKDrift":               func(p *ScalingPolicy) { p.AnalyzerTuning.SteadyGate.MaxKDrift = 1 },
		"analyzerTuning.steadyGate.carry":                   func(p *ScalingPolicy) { p.AnalyzerTuning.SteadyGate.Carry = d(25 * time.Hour) },
		"analyzerTuning.steadyGate.idleRows":                func(p *ScalingPolicy) { p.AnalyzerTuning.SteadyGate.IdleRows = -1 },
		"analyzerTuning.steadyGate.minSettledSamples":       func(p *ScalingPolicy) { p.AnalyzerTuning.SteadyGate.MinSettledSamples = MaxTuningCount + 1 },
		"analyzerTuning.k2Settle.rateFraction":              func(p *ScalingPolicy) { p.AnalyzerTuning.K2Settle.RateFraction = 1.01 },
		"analyzerTuning.k2Settle.maxRise":                   func(p *ScalingPolicy) { p.AnalyzerTuning.K2Settle.MaxRise = -0.1 },
		"analyzerTuning.muChoice.minMeasuredOfDerived":      func(p *ScalingPolicy) { p.AnalyzerTuning.MuChoice.MinMeasuredOfDerived = 1.5 },
		"analyzerTuning.muChoice.maxRatio":                  func(p *ScalingPolicy) { p.AnalyzerTuning.MuChoice.MaxRatio = 0.9 },
		"analyzerTuning.muChoice.returnRatio":               func(p *ScalingPolicy) { p.AnalyzerTuning.MuChoice.ReturnRatio = 0.5 },
		"analyzerTuning.muChoice.freshFor":                  func(p *ScalingPolicy) { p.AnalyzerTuning.MuChoice.FreshFor = d(-time.Minute) },
		"analyzerTuning.muChoice.lowHoldFor":                func(p *ScalingPolicy) { p.AnalyzerTuning.MuChoice.LowHoldFor = d(25 * time.Hour) },
		"analyzerTuning.admission.maxTokensOverCompletions": func(p *ScalingPolicy) { p.AnalyzerTuning.Admission.MaxTokensOverCompletions = 0.9 },
		"analyzerTuning.admission.siblingMuTolerance":       func(p *ScalingPolicy) { p.AnalyzerTuning.Admission.SiblingMuTolerance = 1.1 },
		"analyzerTuning.floorTrust.lagFraction":             func(p *ScalingPolicy) { p.AnalyzerTuning.FloorTrust.LagFraction = 1 },
		"analyzerTuning.floorTrust.trustFraction":           func(p *ScalingPolicy) { p.AnalyzerTuning.FloorTrust.TrustFraction = 1.2 },
		"analyzerTuning.backlogGrowth.minRequests":          func(p *ScalingPolicy) { p.AnalyzerTuning.BacklogGrowth.MinRequests = -1 },
		"analyzerTuning.backlogGrowth.fraction":             func(p *ScalingPolicy) { p.AnalyzerTuning.BacklogGrowth.Fraction = 1 },
		"loadImbalance.idleKvUsage":                         func(p *ScalingPolicy) { p.LoadImbalance.IdleKvUsage = 1 },
		"loadImbalance.idleAfterReady":                      func(p *ScalingPolicy) { p.LoadImbalance.IdleAfterReady = d(30 * time.Second) },
		"loadImbalance.sustainFor":                          func(p *ScalingPolicy) { p.LoadImbalance.SustainFor = d(30 * time.Second) },
		"loadImbalance.clearAfter":                          func(p *ScalingPolicy) { p.LoadImbalance.ClearAfter = d(-time.Second) },
		// Relations, with the partner at its default.
		"steadyGate.carry (1m0s) must be at least": func(p *ScalingPolicy) { p.AnalyzerTuning.SteadyGate.Carry = d(time.Minute) },
		"returnRatio (2.5) must be at most":        func(p *ScalingPolicy) { p.AnalyzerTuning.MuChoice.ReturnRatio = 2.5 },
		"lowHoldFor (4m0s) must be at least":       func(p *ScalingPolicy) { p.AnalyzerTuning.MuChoice.LowHoldFor = d(4 * time.Minute) },
		"trustFraction (0.8) must be above":        func(p *ScalingPolicy) { p.AnalyzerTuning.FloorTrust.LagFraction = 0.8 },
		"idleKvUsage (0.5) must be below kvCacheThreshold (0.4)": func(p *ScalingPolicy) {
			p.LoadImbalance.IdleKvUsage, p.KvCacheThreshold = 0.5, 0.4
		},
	}
	for want, set := range cases {
		p := ScalingPolicy{}
		set(&p)
		err := p.ValidateEntry()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	// The edges of each range are accepted.
	ok := ScalingPolicy{
		AnalyzerTuning: AnalyzerTuning{
			SteadyGate: SteadyGateTuning{SteadyFor: MinSteadyFor, MaxKDrift: 0.99, Carry: MaxTuningDuration,
				IdleRows: 1, MinSettledSamples: MaxTuningCount},
			K2Settle:      K2SettleTuning{RateFraction: 1, MaxRise: 0.01},
			MuChoice:      MuChoiceTuning{MinMeasuredOfDerived: 1, MaxRatio: 1, ReturnRatio: 1, FreshFor: PolicyDuration(time.Second), LowHoldFor: PolicyDuration(time.Second)},
			Admission:     AdmissionTuning{MaxTokensOverCompletions: 1, SiblingMuTolerance: 1},
			FloorTrust:    FloorTrustTuning{LagFraction: 0.01, TrustFraction: 1},
			BacklogGrowth: BacklogGrowthTuning{MinRequests: 0.5, Fraction: 0.99},
		},
		LoadImbalance: LoadImbalance{IdleKvUsage: 0.79, IdleAfterReady: MinImbalanceWindow, SustainFor: MinImbalanceWindow,
			ClearAfter: PolicyDuration(time.Second)},
	}
	if err := ok.ValidateEntry(); err != nil {
		t.Errorf("edges rejected: %v", err)
	}
}

// A per-model override that tunes one figure keeps every other figure of the
// default entry: the blocks merge field by field.
func TestAnalyzerTuningMergesFieldByField(t *testing.T) {
	set := ScalingPolicySet{
		GlobalDefaultsKey: {
			AnalyzerTuning: AnalyzerTuning{
				K2Settle: K2SettleTuning{RateFraction: 0.8, MaxRise: 0.2},
				MuChoice: MuChoiceTuning{FreshFor: PolicyDuration(4 * time.Minute)},
			},
			LoadImbalance: LoadImbalance{IdleKvUsage: 0.25, SustainFor: PolicyDuration(4 * time.Minute)},
		},
		"tier": {AnalyzerTuning: AnalyzerTuning{Admission: AdmissionTuning{SiblingMuTolerance: 0.9}}},
		"one-model": {ModelID: "m", Namespace: "ns",
			AnalyzerTuning: AnalyzerTuning{K2Settle: K2SettleTuning{MaxRise: 0.3}},
			LoadImbalance:  LoadImbalance{SustainFor: PolicyDuration(6 * time.Minute)}},
	}
	got := ResolveScalingPolicyForTier(set, "m", "ns", "tier")
	at, li := got.AnalyzerTuning, got.LoadImbalance
	if at.K2Settle != (K2SettleTuning{RateFraction: 0.8, MaxRise: 0.3}) {
		t.Errorf("k2Settle = %+v, want the default's rateFraction and the model's maxRise", at.K2Settle)
	}
	if at.MuChoice.FreshFor != PolicyDuration(4*time.Minute) || at.Admission.SiblingMuTolerance != 0.9 {
		t.Errorf("lost the default's freshFor or the tier's tolerance: %+v", at)
	}
	if at.MuChoice.LowHoldFor != DefaultMeasuredLowHoldFor || at.SteadyGate.SteadyFor != DefaultSteadyFor {
		t.Errorf("unset figures not defaulted after resolution: %+v", at)
	}
	if li.IdleKvUsage != 0.25 || li.SustainFor != PolicyDuration(6*time.Minute) || li.ClearAfter != DefaultImbalanceClearAfter {
		t.Errorf("loadImbalance = %+v", li)
	}

	other := ResolveScalingPolicyForTier(set, "other", "ns", "")
	if other.AnalyzerTuning.K2Settle.MaxRise != 0.2 || other.LoadImbalance.SustainFor != PolicyDuration(4*time.Minute) {
		t.Errorf("another model took the override: %+v %+v", other.AnalyzerTuning.K2Settle, other.LoadImbalance)
	}
}

// A floor-trust pair crossed by the merge, which no single entry's Validate
// sees, falls back to the default pair rather than dividing by a difference
// at or below zero.
func TestAnalyzerTuningCrossedFloorTrustFallsBack(t *testing.T) {
	got := AnalyzerTuning{FloorTrust: FloorTrustTuning{LagFraction: 0.7, TrustFraction: 0.6}}.WithDefaults().FloorTrust
	if got != (FloorTrustTuning{LagFraction: DefaultFloorLagFraction, TrustFraction: DefaultFloorTrustFraction}) {
		t.Errorf("floorTrust = %+v, want the default pair", got)
	}
}

// A NaN or infinite tuning figure is cleared and reported, and the entry still
// validates.
func TestAnalyzerTuningClearsNonFiniteNumbers(t *testing.T) {
	want := []string{
		"analyzerTuning.steadyGate.maxKDrift", "analyzerTuning.k2Settle.rateFraction", "analyzerTuning.k2Settle.maxRise",
		"analyzerTuning.muChoice.minMeasuredOfDerived", "analyzerTuning.muChoice.maxRatio", "analyzerTuning.muChoice.returnRatio",
		"analyzerTuning.admission.maxTokensOverCompletions", "analyzerTuning.admission.siblingMuTolerance",
		"analyzerTuning.floorTrust.lagFraction", "analyzerTuning.floorTrust.trustFraction",
		"analyzerTuning.backlogGrowth.minRequests", "analyzerTuning.backlogGrowth.fraction",
		"loadImbalance.idleKvUsage",
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := ScalingPolicy{
			AnalyzerTuning: AnalyzerTuning{
				SteadyGate:    SteadyGateTuning{MaxKDrift: v},
				K2Settle:      K2SettleTuning{RateFraction: v, MaxRise: v},
				MuChoice:      MuChoiceTuning{MinMeasuredOfDerived: v, MaxRatio: v, ReturnRatio: v},
				Admission:     AdmissionTuning{MaxTokensOverCompletions: v, SiblingMuTolerance: v},
				FloorTrust:    FloorTrustTuning{LagFraction: v, TrustFraction: v},
				BacklogGrowth: BacklogGrowthTuning{MinRequests: v, Fraction: v},
			},
			LoadImbalance: LoadImbalance{IdleKvUsage: v},
		}
		ignored := p.ResetUnusableFields()
		fields := make([]string, 0, len(ignored))
		for _, f := range ignored {
			fields = append(fields, f.Field)
			if f.Reason != errNotFinite {
				t.Errorf("%v: %s reason %v", v, f.Field, f.Reason)
			}
		}
		if !slices.Equal(fields, want) {
			t.Errorf("%v: ignored %v, want %v", v, fields, want)
		}
		if p.AnalyzerTuning != (AnalyzerTuning{}) || p.LoadImbalance != (LoadImbalance{}) {
			t.Errorf("%v: not every field cleared: %+v %+v", v, p.AnalyzerTuning, p.LoadImbalance)
		}
		if err := p.ValidateEntry(); err != nil {
			t.Errorf("%v: entry rejected after reset: %v", v, err)
		}
	}
}
