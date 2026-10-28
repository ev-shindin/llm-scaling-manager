package source

import (
	"math"
	"testing"
)

// Finite and IsNaN read a non-finite Value even when the source set no flag,
// as a source that hands a sample over as read does.
func TestMetricValueFiniteness(t *testing.T) {
	cases := []struct {
		name       string
		v          MetricValue
		wantFinite bool
		wantNaN    bool
	}{
		{"finite", MetricValue{Value: 0.5}, true, false},
		{"zero", MetricValue{}, true, false},
		{"raw NaN, no flag", MetricValue{Value: math.NaN()}, false, true},
		{"raw +Inf, no flag", MetricValue{Value: math.Inf(1)}, false, false},
		{"raw -Inf, no flag", MetricValue{Value: math.Inf(-1)}, false, false},
		{"NaN flag, Value 0", MetricValue{NaN: true}, false, true},
		{"Inf flag, Value 0", MetricValue{Inf: true}, false, false},
	}
	for _, tc := range cases {
		if got := tc.v.Finite(); got != tc.wantFinite {
			t.Errorf("%s: Finite() = %v, want %v", tc.name, got, tc.wantFinite)
		}
		if got := tc.v.IsNaN(); got != tc.wantNaN {
			t.Errorf("%s: IsNaN() = %v, want %v", tc.name, got, tc.wantNaN)
		}
	}
}
