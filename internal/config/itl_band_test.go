package config

import (
	"math"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The policy's defaults are the ITL package's: an unset key must behave as
// the code did before the key existed.
func TestITLBandDefaultsMatchTheTracker(t *testing.T) {
	if DefaultITLMinObservableK != itl.DefaultMinObservableK || DefaultITLMaxObservableK != itl.DefaultMaxObservableK {
		t.Errorf("config band [%g, %g] differs from itl [%g, %g]", DefaultITLMinObservableK, DefaultITLMaxObservableK,
			itl.DefaultMinObservableK, itl.DefaultMaxObservableK)
	}
	var p ScalingPolicy
	p.ApplyDefaults()
	if p.ITLMinObservableK != DefaultITLMinObservableK || p.ITLMaxObservableK != DefaultITLMaxObservableK {
		t.Errorf("ApplyDefaults band = [%g, %g]", p.ITLMinObservableK, p.ITLMaxObservableK)
	}
	if got := p.ITLCapPreemptionRate(); got != DefaultITLCapMaxPreemptionRate {
		t.Errorf("unset itlCapMaxPreemptionRate = %g, want %g", got, DefaultITLCapMaxPreemptionRate)
	}
	var none *ScalingPolicy
	if lo, hi := none.ITLBand(); lo != DefaultITLMinObservableK || hi != DefaultITLMaxObservableK || none.ITLCapPreemptionRate() != 0 {
		t.Errorf("a nil policy reads band [%g, %g], want the defaults", lo, hi)
	}
}

func TestITLKeysValidate(t *testing.T) {
	rate := func(v float64) *float64 { return &v }
	for name, tc := range map[string]struct {
		p  ScalingPolicy
		ok bool
	}{
		"unset":                 {ScalingPolicy{}, true},
		"a band":                {ScalingPolicy{ITLMinObservableK: 0.10, ITLMaxObservableK: 0.90}, true},
		"one edge":              {ScalingPolicy{ITLMaxObservableK: 0.85}, true},
		"min negative":          {ScalingPolicy{ITLMinObservableK: -0.1}, false},
		"max at one":            {ScalingPolicy{ITLMaxObservableK: 1}, false},
		"min at one":            {ScalingPolicy{ITLMinObservableK: 1}, false},
		"inverted":              {ScalingPolicy{ITLMinObservableK: 0.5, ITLMaxObservableK: 0.4}, false},
		"equal":                 {ScalingPolicy{ITLMinObservableK: 0.5, ITLMaxObservableK: 0.5}, false},
		"zero preemption rate":  {ScalingPolicy{ITLCapMaxPreemptionRate: rate(0)}, true},
		"some preemption":       {ScalingPolicy{ITLCapMaxPreemptionRate: rate(0.2)}, true},
		"negative preemption":   {ScalingPolicy{ITLCapMaxPreemptionRate: rate(-0.1)}, false},
		"band edge below other": {ScalingPolicy{ITLMinObservableK: 0.85}, false},
	} {
		// As resolved: an edge set alone meets the other's default.
		p := tc.p
		p.ApplyDefaults()
		if err := p.Validate(); (err == nil) != tc.ok {
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
		GlobalDefaultsKey: {ITLCapMaxPreemptionRate: &some},
		"wide":            {ITLMinObservableK: 0.10, ITLMaxObservableK: 0.90},
		"one-model":       {ModelID: "m", Namespace: "ns", ITLCapMaxPreemptionRate: &zero},
		"low-top":         {ITLMaxObservableK: 0.12},
	}
	tier := ResolveScalingPolicyForTier(set, "other", "ns", "wide")
	if lo, hi := tier.ITLBand(); lo != 0.10 || hi != 0.90 {
		t.Errorf("tier band = [%g, %g], want [0.10, 0.90]", lo, hi)
	}
	if got := tier.ITLCapPreemptionRate(); got != some {
		t.Errorf("inherited itlCapMaxPreemptionRate = %g, want %g", got, some)
	}
	model := ResolveScalingPolicyForTier(set, "m", "ns", "wide")
	if got := model.ITLCapPreemptionRate(); got != 0 {
		t.Errorf("an override's explicit 0 = %g, want 0 over the default's %g", got, some)
	}
	inverted := ResolveScalingPolicyForTier(set, "other", "ns", "low-top")
	if lo, hi := inverted.ITLBand(); lo != DefaultITLMinObservableK || hi != DefaultITLMaxObservableK {
		t.Errorf("an inverted merged band = [%g, %g], want the defaults", lo, hi)
	}
	if inverted.ITLMinObservableK != DefaultITLMinObservableK || inverted.ITLMaxObservableK != DefaultITLMaxObservableK {
		t.Errorf("the resolved policy holds [%g, %g]; resolution must reset an inverted pair, as it does the scaling band",
			inverted.ITLMinObservableK, inverted.ITLMaxObservableK)
	}
}

// A non-finite value is reset, so it reads as unset, rather than passing every
// range check as NaN does.
func TestITLKeysNonFinite(t *testing.T) {
	nan := math.NaN()
	p := ScalingPolicy{ITLMinObservableK: math.Inf(1), ITLMaxObservableK: nan, ITLCapMaxPreemptionRate: &nan}
	ignored := p.ResetUnusableFields()
	if len(ignored) != 3 {
		t.Fatalf("ignored %v, want the three ITL keys", ignored)
	}
	if p.ITLMinObservableK != 0 || p.ITLMaxObservableK != 0 || p.ITLCapMaxPreemptionRate != nil {
		t.Errorf("after reset: %+v", p)
	}
	nanBand := ScalingPolicy{ITLMinObservableK: nan, ITLMaxObservableK: nan}
	if lo, hi := nanBand.ITLBand(); lo != DefaultITLMinObservableK || hi != DefaultITLMaxObservableK {
		t.Errorf("ITLBand of NaN edges = [%g, %g], want the defaults", lo, hi)
	}
}
