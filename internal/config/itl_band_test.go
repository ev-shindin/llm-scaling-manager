package config

import (
	"math"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// band is a policy that sets only analyzerTuning.itlBand.
func band(b ITLBandTuning) ScalingPolicy {
	return ScalingPolicy{AnalyzerTuning: AnalyzerTuning{ITLBand: b}}
}

// The policy's defaults are the ITL package's: an unset key must behave as
// the code did before the key existed.
func TestITLBandDefaultsMatchTheTracker(t *testing.T) {
	if DefaultITLMinObservableK != itl.DefaultMinObservableK || DefaultITLMaxObservableK != itl.DefaultMaxObservableK {
		t.Errorf("config band [%g, %g] differs from itl [%g, %g]", DefaultITLMinObservableK, DefaultITLMaxObservableK,
			itl.DefaultMinObservableK, itl.DefaultMaxObservableK)
	}
	var p ScalingPolicy
	p.ApplyDefaults()
	if b := p.AnalyzerTuning.ITLBand; b.MinObservableK != DefaultITLMinObservableK || b.MaxObservableK != DefaultITLMaxObservableK {
		t.Errorf("ApplyDefaults band = [%g, %g]", b.MinObservableK, b.MaxObservableK)
	}
	if got := p.ITLCapPreemptionRate(); got != DefaultITLCapMaxPreemptionRate {
		t.Errorf("unset capMaxPreemptionRate = %g, want %g", got, DefaultITLCapMaxPreemptionRate)
	}
	var none *ScalingPolicy
	if lo, hi := none.ITLBand(); lo != DefaultITLMinObservableK || hi != DefaultITLMaxObservableK || none.ITLCapPreemptionRate() != 0 {
		t.Errorf("a nil policy reads band [%g, %g], want the defaults", lo, hi)
	}
	// And the steady gate: unset or nil, it is the tracker's own.
	if got := none.ITLSteadiness(); got != itl.DefaultSteadiness() {
		t.Errorf("a nil policy's steady gate = %+v, want %+v", got, itl.DefaultSteadiness())
	}
	if got := p.ITLSteadiness(); got != itl.DefaultSteadiness() {
		t.Errorf("a resolved empty policy's steady gate = %+v, want %+v", got, itl.DefaultSteadiness())
	}
}

func TestITLKeysValidate(t *testing.T) {
	rate := func(v float64) *float64 { return &v }
	for name, tc := range map[string]struct {
		p  ScalingPolicy
		ok bool
	}{
		"unset":                {ScalingPolicy{}, true},
		"a band":               {band(ITLBandTuning{MinObservableK: 0.10, MaxObservableK: 0.90}), true},
		"one edge":             {band(ITLBandTuning{MaxObservableK: 0.85}), true},
		"min negative":         {band(ITLBandTuning{MinObservableK: -0.1}), false},
		"max at one":           {band(ITLBandTuning{MaxObservableK: 1}), false},
		"min at one":           {band(ITLBandTuning{MinObservableK: 1}), false},
		"inverted":             {band(ITLBandTuning{MinObservableK: 0.5, MaxObservableK: 0.4}), false},
		"equal":                {band(ITLBandTuning{MinObservableK: 0.5, MaxObservableK: 0.5}), false},
		"zero preemption rate": {band(ITLBandTuning{CapMaxPreemptionRate: rate(0)}), true},
		"some preemption":      {band(ITLBandTuning{CapMaxPreemptionRate: rate(0.2)}), true},
		"negative preemption":  {band(ITLBandTuning{CapMaxPreemptionRate: rate(-0.1)}), false},
		"band edge past other": {band(ITLBandTuning{MinObservableK: 0.85}), true},
	} {
		// As stored: an edge set alone is not judged against the other's
		// default, which a layer below may replace; the resolved policy
		// repairs a pair the merge inverts (TestITLKeysMerge).
		if err := tc.p.ValidateEntry(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v, want ok=%t", name, err, tc.ok)
		}
	}
}

// Each key carries through a tier and a per-model override, and a merge that
// inverts the band resolves to the defaults rather than a band that admits
// nothing.
func TestITLKeysMerge(t *testing.T) {
	zero, some := 0.0, 0.3
	set := ScalingPolicySet{
		GlobalDefaultsKey: band(ITLBandTuning{CapMaxPreemptionRate: &some}),
		"wide":            band(ITLBandTuning{MinObservableK: 0.10, MaxObservableK: 0.90}),
		"one-model":       {ModelID: "m", Namespace: "ns", AnalyzerTuning: AnalyzerTuning{ITLBand: ITLBandTuning{CapMaxPreemptionRate: &zero}}},
		"low-top":         band(ITLBandTuning{MaxObservableK: 0.12}),
	}
	tier := ResolveScalingPolicyForTier(set, "other", "ns", "wide")
	if lo, hi := tier.ITLBand(); lo != 0.10 || hi != 0.90 {
		t.Errorf("tier band = [%g, %g], want [0.10, 0.90]", lo, hi)
	}
	if got := tier.ITLCapPreemptionRate(); got != some {
		t.Errorf("inherited capMaxPreemptionRate = %g, want %g", got, some)
	}
	model := ResolveScalingPolicyForTier(set, "m", "ns", "wide")
	if got := model.ITLCapPreemptionRate(); got != 0 {
		t.Errorf("an override's explicit 0 = %g, want 0 over the default's %g", got, some)
	}
	inverted := ResolveScalingPolicyForTier(set, "other", "ns", "low-top")
	if lo, hi := inverted.ITLBand(); lo != DefaultITLMinObservableK || hi != DefaultITLMaxObservableK {
		t.Errorf("an inverted merged band = [%g, %g], want the defaults", lo, hi)
	}
	if b := inverted.AnalyzerTuning.ITLBand; b.MinObservableK != DefaultITLMinObservableK || b.MaxObservableK != DefaultITLMaxObservableK {
		t.Errorf("the resolved policy holds [%g, %g]; resolution must reset an inverted pair, as it does the scaling band",
			b.MinObservableK, b.MaxObservableK)
	}
}

// A non-finite value is reset, so it reads as unset, rather than passing every
// range check as NaN does.
func TestITLKeysNonFinite(t *testing.T) {
	nan := math.NaN()
	p := band(ITLBandTuning{MinObservableK: math.Inf(1), MaxObservableK: nan, CapMaxPreemptionRate: &nan})
	ignored := p.ResetUnusableFields()
	if len(ignored) != 3 {
		t.Fatalf("ignored %v, want the three ITL keys", ignored)
	}
	if b := p.AnalyzerTuning.ITLBand; b.MinObservableK != 0 || b.MaxObservableK != 0 || b.CapMaxPreemptionRate != nil {
		t.Errorf("after reset: %+v", b)
	}
	nanBand := band(ITLBandTuning{MinObservableK: nan, MaxObservableK: nan})
	if lo, hi := nanBand.ITLBand(); lo != DefaultITLMinObservableK || hi != DefaultITLMaxObservableK {
		t.Errorf("ITLBand of NaN edges = [%g, %g], want the defaults", lo, hi)
	}
}
