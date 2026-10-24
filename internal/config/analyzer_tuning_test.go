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
  itlBand: {minObservableK: 0.10, maxObservableK: 0.85, capMaxPreemptionRate: 0}
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
		ITLBand: ITLBandTuning{MinObservableK: 0.10, MaxObservableK: 0.85},
		SteadyGate: SteadyGateTuning{SteadyFor: PolicyDuration(3 * time.Minute), MaxKDrift: 0.05,
			Carry: PolicyDuration(6 * time.Minute), IdleRows: 3, MinSettledSamples: 4},
		K2Settle: K2SettleTuning{RateFraction: 0.8, MaxRise: 0.2},
		MuChoice: MuChoiceTuning{MinMeasuredOfDerived: 0.6, MaxRatio: 2.5, ReturnRatio: 2.0,
			FreshFor: PolicyDuration(4 * time.Minute), LowHoldFor: PolicyDuration(20 * time.Minute)},
		Admission:     AdmissionTuning{MaxTokensOverCompletions: 1.2, SiblingMuTolerance: 0.9},
		FloorTrust:    FloorTrustTuning{LagFraction: 0.4, TrustFraction: 0.9},
		BacklogGrowth: BacklogGrowthTuning{MinRequests: 3, Fraction: 0.2},
	}
	if r := p.AnalyzerTuning.ITLBand.CapMaxPreemptionRate; r == nil || *r != 0 {
		t.Errorf("capMaxPreemptionRate = %v, want an explicit 0", r)
	}
	got := p.AnalyzerTuning
	got.ITLBand.CapMaxPreemptionRate = nil
	if got != want {
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
		// Relations, where the entry sets both keys of the pair.
		"steadyGate.carry (1m0s) must be at least": func(p *ScalingPolicy) {
			p.AnalyzerTuning.SteadyGate.SteadyFor, p.AnalyzerTuning.SteadyGate.Carry = d(2*time.Minute), d(time.Minute)
		},
		"returnRatio (2.5) must be at most": func(p *ScalingPolicy) {
			p.AnalyzerTuning.MuChoice.MaxRatio, p.AnalyzerTuning.MuChoice.ReturnRatio = 2, 2.5
		},
		"lowHoldFor (4m0s) must be at least": func(p *ScalingPolicy) {
			p.AnalyzerTuning.MuChoice.FreshFor, p.AnalyzerTuning.MuChoice.LowHoldFor = d(5*time.Minute), d(4*time.Minute)
		},
		"trustFraction (0.8) must be above": func(p *ScalingPolicy) {
			p.AnalyzerTuning.FloorTrust.LagFraction, p.AnalyzerTuning.FloorTrust.TrustFraction = 0.8, 0.8
		},
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

// An entry that sets no loadImbalance key is not judged by the default idle
// figure: a default entry with kvCacheThreshold at or under 0.30, valid
// before the key existed, keeps its limiters. At run time the idle bound is
// held under the threshold instead.
func TestIdleKvUsageUnsetDoesNotRejectALowKvCacheThreshold(t *testing.T) {
	for _, entry := range []string{
		"kvCacheThreshold: 0.3\nlimiters:\n- type: gpu-inventory\n",
		"kvCacheThreshold: 0.25\nlimiters:\n- type: gpu-inventory\n",
	} {
		p, err := DecodeScalingPolicy(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.ValidateEntry(); err != nil {
			t.Errorf("%q: rejected: %v", entry, err)
		}
		p.ApplyDefaults()
		if got := p.IdleKvUsage(); got != p.KvCacheThreshold {
			t.Errorf("%q: idle bound %g, want kvCacheThreshold %g", entry, got, p.KvCacheThreshold)
		}
	}
	// Set by the entry, idleKvUsage is still judged against the threshold.
	set := ScalingPolicy{KvCacheThreshold: 0.3, LoadImbalance: LoadImbalance{IdleKvUsage: 0.3}}
	if err := set.ValidateEntry(); err == nil {
		t.Error("idleKvUsage 0.3 under kvCacheThreshold 0.3 was accepted")
	}
	// Above the threshold the figure is the key's, or its default.
	for _, tc := range []struct {
		p    *ScalingPolicy
		want float64
	}{
		{nil, DefaultIdleKvUsage},
		{&ScalingPolicy{}, DefaultIdleKvUsage},
		{&ScalingPolicy{KvCacheThreshold: 0.9, LoadImbalance: LoadImbalance{IdleKvUsage: 0.4}}, 0.4},
	} {
		if got := tc.p.IdleKvUsage(); got != tc.want {
			t.Errorf("%+v: idle bound %g, want %g", tc.p, got, tc.want)
		}
	}
}

// One key of a pair set alone is not judged against its partner's default:
// the partner may come from the layer below, which the entry cannot see.
func TestAnalyzerTuningOneKeyOfAPairIsAccepted(t *testing.T) {
	d := func(x time.Duration) PolicyDuration { return PolicyDuration(x) }
	for name, at := range map[string]AnalyzerTuning{
		"steadyFor over the default carry":         {SteadyGate: SteadyGateTuning{SteadyFor: d(10 * time.Minute)}},
		"carry under the default steadyFor":        {SteadyGate: SteadyGateTuning{Carry: d(time.Minute)}},
		"lagFraction over the default trust":       {FloorTrust: FloorTrustTuning{LagFraction: 0.85}},
		"trustFraction under the default lag":      {FloorTrust: FloorTrustTuning{TrustFraction: 0.4}},
		"maxRatio under the default returnRatio":   {MuChoice: MuChoiceTuning{MaxRatio: 1.5}},
		"returnRatio over the default maxRatio":    {MuChoice: MuChoiceTuning{ReturnRatio: 2.5}},
		"freshFor over the default lowHoldFor":     {MuChoice: MuChoiceTuning{FreshFor: d(40 * time.Minute)}},
		"lowHoldFor under the default freshFor":    {MuChoice: MuChoiceTuning{LowHoldFor: d(4 * time.Minute)}},
		"minObservableK over the default maximum":  {ITLBand: ITLBandTuning{MinObservableK: 0.85}},
		"maxObservableK under the default minimum": {ITLBand: ITLBandTuning{MaxObservableK: 0.12}},
	} {
		if err := (ScalingPolicy{AnalyzerTuning: at}).ValidateEntry(); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
}

// Each pair two valid entries cross in the merge is repaired on the resolved
// policy, both keys back to their defaults, so no consumer reads a crossed
// pair; and a pair the merge leaves uncrossed is kept.
func TestAnalyzerTuningCrossedPairsResolveToTheDefaults(t *testing.T) {
	d := func(x time.Duration) PolicyDuration { return PolicyDuration(x) }
	set := ScalingPolicySet{
		GlobalDefaultsKey: {AnalyzerTuning: AnalyzerTuning{
			SteadyGate: SteadyGateTuning{SteadyFor: d(2 * time.Minute), Carry: d(3 * time.Minute)},
			MuChoice: MuChoiceTuning{MaxRatio: 3, ReturnRatio: 2.5,
				FreshFor: d(5 * time.Minute), LowHoldFor: d(10 * time.Minute)},
			FloorTrust: FloorTrustTuning{LagFraction: 0.3, TrustFraction: 0.6},
		}},
		"crossing": {AnalyzerTuning: AnalyzerTuning{
			SteadyGate: SteadyGateTuning{SteadyFor: d(4 * time.Minute)},
			MuChoice:   MuChoiceTuning{MaxRatio: 2, FreshFor: d(15 * time.Minute)},
			FloorTrust: FloorTrustTuning{LagFraction: 0.7},
		}},
		"keeping": {AnalyzerTuning: AnalyzerTuning{
			SteadyGate: SteadyGateTuning{SteadyFor: d(3 * time.Minute)},
			MuChoice:   MuChoiceTuning{MaxRatio: 2.6, FreshFor: d(8 * time.Minute)},
			FloorTrust: FloorTrustTuning{LagFraction: 0.5},
		}},
	}
	for name, p := range set {
		if err := p.ValidateEntry(); err != nil {
			t.Fatalf("premise: entry %s is valid alone: %v", name, err)
		}
	}
	crossed := ResolveScalingPolicyForTier(set, "m", "ns", "crossing").AnalyzerTuning
	if g := crossed.SteadyGate; g.SteadyFor != DefaultSteadyFor || g.Carry != DefaultSteadyCarry {
		t.Errorf("steadyGate = %v/%v, want the defaults", g.SteadyFor.Duration(), g.Carry.Duration())
	}
	if m := crossed.MuChoice; m.MaxRatio != DefaultMeasuredDerivedMaxRatio || m.ReturnRatio != DefaultMeasuredDerivedReturn {
		t.Errorf("muChoice ratios = %g/%g, want the defaults", m.MaxRatio, m.ReturnRatio)
	}
	if m := crossed.MuChoice; m.FreshFor != DefaultMeasuredMuFreshFor || m.LowHoldFor != DefaultMeasuredLowHoldFor {
		t.Errorf("muChoice holds = %v/%v, want the defaults", m.FreshFor.Duration(), m.LowHoldFor.Duration())
	}
	if f := crossed.FloorTrust; f != (FloorTrustTuning{LagFraction: DefaultFloorLagFraction, TrustFraction: DefaultFloorTrustFraction}) {
		t.Errorf("floorTrust = %+v, want the defaults", f)
	}
	resolved := ResolveScalingPolicyForTier(set, "m", "ns", "crossing")
	if err := resolved.Validate(); err != nil {
		t.Errorf("the resolved policy fails its own validation: %v", err)
	}

	kept := ResolveScalingPolicyForTier(set, "m", "ns", "keeping").AnalyzerTuning
	want := AnalyzerTuning{
		SteadyGate: SteadyGateTuning{SteadyFor: d(3 * time.Minute), Carry: d(3 * time.Minute)},
		MuChoice: MuChoiceTuning{MaxRatio: 2.6, ReturnRatio: 2.5,
			FreshFor: d(8 * time.Minute), LowHoldFor: d(10 * time.Minute)},
		FloorTrust: FloorTrustTuning{LagFraction: 0.5, TrustFraction: 0.6},
	}
	if kept.SteadyGate.SteadyFor != want.SteadyGate.SteadyFor || kept.SteadyGate.Carry != want.SteadyGate.Carry ||
		kept.MuChoice.MaxRatio != want.MuChoice.MaxRatio || kept.MuChoice.ReturnRatio != want.MuChoice.ReturnRatio ||
		kept.MuChoice.FreshFor != want.MuChoice.FreshFor || kept.MuChoice.LowHoldFor != want.MuChoice.LowHoldFor ||
		kept.FloorTrust != want.FloorTrust {
		t.Errorf("an uncrossed merge was changed: %+v", kept)
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
