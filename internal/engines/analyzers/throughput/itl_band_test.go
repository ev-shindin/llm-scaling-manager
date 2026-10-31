package throughput

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The observation window reads the policy's ITL band, as the saturation and
// slo analyzers do: replicas at k 0.85 and 0.10 are outside the default band
// and inside one of [0.05, 0.90].
func TestObservationWindowReadsThePolicyBand(t *testing.T) {
	count := func(cfg *config.ScalingPolicy) int {
		a := NewThroughputAnalyzer()
		in := domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ReplicaMetrics: makeMetrics("v1", 2, 0.10, 0.75)}
		if cfg != nil {
			in.Config = cfg
		}
		if _, err := a.Analyze(context.Background(), in); err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		st, _ := a.VariantState("m", "ns", "v1")
		return st.SampleCount
	}
	if got := count(nil); got != 0 {
		t.Errorf("default band kept %d readings at k 0.10 and 0.85, want 0", got)
	}
	if got := count(&config.ScalingPolicy{AnalyzerTuning: config.AnalyzerTuning{ITLBand: config.ITLBandTuning{MinObservableK: 0.05, MaxObservableK: 0.90}}}); got != 2 {
		t.Errorf("a band of [0.05, 0.90] kept %d readings, want 2: analyzerTuning.itlBand must reach the window", got)
	}
}

// A replica's ITL is a minute's rate, so the window, its band and the tier-2
// fit pair it with the KV fill averaged over that minute when the collector
// read it (issue #403): a replica whose instant fill is 0.95, above the
// default band, and whose minute mean is 0.45 teaches the line at 0.45. Without
// the mean the instant fill is all there is, and the replica is out of band.
func TestITLPairsTheMinuteMeanKVFill(t *testing.T) {
	const instant, mean = 0.95, 0.45
	replica := func(known bool) domain.ReplicaMetrics {
		m := makeMetrics("v1", 1, instant, 0)[0]
		m.AvgITL = 0.030 + 0.05*mean
		m.KvUsageMean, m.KvUsageMeanKnown = mean, known
		return m
	}
	ctx := context.Background()
	now := time.Now()

	for _, tc := range []struct {
		name    string
		known   bool
		samples int
		k       float64
	}{
		{"mean known", true, 1, mean},
		{"mean unknown", false, 0, instant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := replica(tc.known)
			a := NewThroughputAnalyzer()
			a.observeDefaultBand(ctx, now, "m", "ns", []domain.ReplicaMetrics{m})
			st, _ := a.VariantState("m", "ns", "v1")
			if st.SampleCount != tc.samples {
				t.Errorf("window kept %d readings, want %d", st.SampleCount, tc.samples)
			}

			// Tier 2 pins B and fits A over the same k: A = (ITL - B) / k.
			model, _, ok := a.resolveITLModel(ctx, a.getOrCreateVariantState(variantKey("ns", "m", "v1")), []domain.ReplicaMetrics{m}, "ns", "m", "v1")
			if !ok {
				t.Fatal("tier-2 fit failed")
			}
			if want := (m.AvgITL - DefaultBaselineITLSec) / tc.k; math.Abs(model.A-want) > 1e-12 {
				t.Errorf("tier-2 A = %g, want %g (k %g)", model.A, want, tc.k)
			}
		})
	}
}
