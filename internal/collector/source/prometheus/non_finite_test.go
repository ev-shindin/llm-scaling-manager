package prometheus

import (
	"math"
	"testing"

	"github.com/prometheus/common/model"

	sourcepkg "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
)

// Each parser keeps Value as it always was (0 for a non-finite sample) and
// says which non-finite value it replaced, so a consumer can tell a present
// NaN from an absent series without any Value changing.
func TestParseFlagsNonFinite(t *testing.T) {
	p := &PrometheusSource{}
	cases := []struct {
		name      string
		in        float64
		wantValue float64
		wantNaN   bool
		wantInf   bool
	}{
		{"finite", 0.25, 0.25, false, false},
		{"zero", 0, 0, false, false},
		{"NaN", math.NaN(), 0, true, false},
		{"+Inf", math.Inf(1), 0, false, true},
		{"-Inf", math.Inf(-1), 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sample := model.SampleValue(tc.in)
			parsed := map[string][]sourcepkg.MetricValue{
				"vector": p.parseVector(model.Vector{&model.Sample{Value: sample}}),
				"scalar": p.parseScalar(&model.Scalar{Value: sample}),
				"matrix": p.parseMatrix(model.Matrix{&model.SampleStream{Values: []model.SamplePair{{Value: sample}}}}),
			}
			for kind, values := range parsed {
				if len(values) != 1 {
					t.Fatalf("%s: got %d values, want 1", kind, len(values))
				}
				v := values[0]
				if v.Value != tc.wantValue || v.NaN != tc.wantNaN || v.Inf != tc.wantInf {
					t.Errorf("%s: Value %v NaN %v Inf %v, want %v %v %v",
						kind, v.Value, v.NaN, v.Inf, tc.wantValue, tc.wantNaN, tc.wantInf)
				}
				if v.Finite() == (tc.wantNaN || tc.wantInf) {
					t.Errorf("%s: Finite() = %v", kind, v.Finite())
				}
				if v.IsNaN() != tc.wantNaN {
					t.Errorf("%s: IsNaN() = %v, want %v", kind, v.IsNaN(), tc.wantNaN)
				}
			}
		})
	}
}
