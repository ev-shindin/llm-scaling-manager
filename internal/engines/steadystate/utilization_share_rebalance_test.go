package steadystate

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// The policy's immediateRebalance settings are the ones the planner and the blocked reasons
// apply.
func TestShareRebalanceComesFromThePolicy(t *testing.T) {
	off := false
	us, err := config.ResolveUtilizationShare(&config.UtilizationShareConfig{
		ImmediateRebalance: &config.UtilizationShareImmediateRebalance{Enabled: &off, ReceiverLoadAtLeast: 1.2, DonorLoadAtMost: 0.4},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := allocation.ShareRebalance{Off: true, ReceiverLoad: 1.2, DonorLoad: 0.4}
	if got := shareRebalance(us); got != want {
		t.Fatalf("shareRebalance = %+v, want %+v", got, want)
	}
}
