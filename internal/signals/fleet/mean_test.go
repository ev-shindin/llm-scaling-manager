package fleet

import (
	"math"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

func rm(value, rate float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{AvgOutputTokens: value, RequestRate: rate}
}

func outputTokens(m domain.ReplicaMetrics) float64 { return m.AvgOutputTokens }
func all(domain.ReplicaMetrics) bool               { return true }

func TestMeanWeightsByRequestRate(t *testing.T) {
	// The replica serving most of the traffic decides the figure. 1000 at rate
	// 9 and 100 at rate 1 is 910, not the plain mean of 550 -- which is the
	// whole reason this is weighted.
	got := Mean([]domain.ReplicaMetrics{rm(1000, 9), rm(100, 1)}, outputTokens, all)
	if got != 910 {
		t.Errorf("Mean = %v, want 910", got)
	}
}

func TestMeanFallsBackToThePlainMean(t *testing.T) {
	// No replica reports a rate: a fleet under no load still has a shape, and
	// reporting zero would price its queue at nothing.
	got := Mean([]domain.ReplicaMetrics{rm(1000, 0), rm(100, 0)}, outputTokens, all)
	if got != 550 {
		t.Errorf("Mean = %v, want 550 (plain mean)", got)
	}
}

func TestMeanIgnoresRatelessReplicasWhenAnyRateExists(t *testing.T) {
	// A replica with completions but no rate contributes to neither sum once a
	// weighted answer is available. Pinned because the two accumulators run in
	// the same loop and it is easy to make the plain one leak into the result.
	got := Mean([]domain.ReplicaMetrics{rm(1000, 5), rm(1, 0)}, outputTokens, all)
	if got != 1000 {
		t.Errorf("Mean = %v, want 1000", got)
	}
}

func TestMeanIsZeroWhenNothingQualifies(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []domain.ReplicaMetrics
	}{
		{"no replicas", nil},
		{"all zero", []domain.ReplicaMetrics{rm(0, 3), rm(0, 4)}},
		{"all excluded", []domain.ReplicaMetrics{rm(100, 3)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			include := all
			if tc.name == "all excluded" {
				include = func(domain.ReplicaMetrics) bool { return false }
			}
			if got := Mean(tc.in, outputTokens, include); got != 0 {
				t.Errorf("Mean = %v, want 0", got)
			}
		})
	}
}

// Zero is the one real disagreement between the callers this replaced, so it
// is the one thing an option controls.
func TestMeanZeroIsAReading(t *testing.T) {
	in := []domain.ReplicaMetrics{rm(1000, 1), rm(0, 1)}

	if got := Mean(in, outputTokens, all); got != 1000 {
		t.Errorf("default: Mean = %v, want 1000 -- a zero token length is a replica that completed nothing", got)
	}
	if got := Mean(in, outputTokens, all, ZeroIsAReading()); got != 500 {
		t.Errorf("ZeroIsAReading: Mean = %v, want 500 -- a zero hit rate is caching being off", got)
	}
}

// The guard that `v <= 0` cannot express, in either mode. This is the defect
// that existed in fleetAverage and was absent from fleetPrefixHitRate.
func TestMeanRejectsNonFiniteReadings(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []domain.ReplicaMetrics{rm(1000, 1), rm(tc.bad, 1)}

			got := Mean(in, outputTokens, all)
			if got != 1000 {
				t.Errorf("default: Mean = %v, want 1000 -- %s must not reach the accumulators", got, tc.name)
			}
			// And with the option on, because `v < 0` does not reject NaN
			// either, so the two branches need separate cover.
			got = Mean(in, outputTokens, all, ZeroIsAReading())
			if got != 1000 {
				t.Errorf("ZeroIsAReading: Mean = %v, want 1000 -- %s must not reach the accumulators", got, tc.name)
			}
		})
	}
}

func TestMeanRejectsNegativeReadings(t *testing.T) {
	in := []domain.ReplicaMetrics{rm(1000, 1), rm(-5, 1)}
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"default", nil},
		{"ZeroIsAReading", []Option{ZeroIsAReading()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Mean(in, outputTokens, all, tc.opts...); got != 1000 {
				t.Errorf("Mean = %v, want 1000 -- a negative reading is not a quantity", got)
			}
		})
	}
}

func TestMeanWithin(t *testing.T) {
	// A hit rate is a fraction; 1.4 is an artefact, not an unusual fleet.
	in := []domain.ReplicaMetrics{rm(0.5, 1), rm(1.4, 1)}
	got := Mean(in, outputTokens, all, ZeroIsAReading(), Within(0, 1))
	if got != 0.5 {
		t.Errorf("Mean = %v, want 0.5 -- the out-of-range reading must be dropped", got)
	}
	// The bound admits its own endpoints.
	got = Mean([]domain.ReplicaMetrics{rm(0, 1), rm(1, 1)}, outputTokens, all, ZeroIsAReading(), Within(0, 1))
	if got != 0.5 {
		t.Errorf("Mean = %v, want 0.5 -- 0 and 1 are both inside [0,1]", got)
	}
}

func TestMeanRespectsInclude(t *testing.T) {
	// The role filter the callers use: only prefill replicas carry a prefix
	// cache, so a decode replica's reading must not enter the mean.
	in := []domain.ReplicaMetrics{
		{AvgOutputTokens: 1000, RequestRate: 1, VariantName: "decode"},
		{AvgOutputTokens: 100, RequestRate: 1, VariantName: "prefill"},
	}
	got := Mean(in, outputTokens,
		func(m domain.ReplicaMetrics) bool { return m.VariantName == "prefill" })
	if got != 100 {
		t.Errorf("Mean = %v, want 100", got)
	}
}
