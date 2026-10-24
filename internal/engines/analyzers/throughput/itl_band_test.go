package throughput

import (
	"context"
	"testing"

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
	if got := count(&config.ScalingPolicy{ITLMinObservableK: 0.05, ITLMaxObservableK: 0.90}); got != 2 {
		t.Errorf("a band of [0.05, 0.90] kept %d readings, want 2: itlMinObservableK and itlMaxObservableK must reach the window", got)
	}
}
