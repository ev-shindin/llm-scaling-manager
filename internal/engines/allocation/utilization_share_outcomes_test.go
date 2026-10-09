package allocation

import (
	"slices"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// The metrics package publishes every outcome at 0 from constants, which it
// can import; the ledger defines them. A new outcome added to one and not the
// other would be counted without ever being published at 0, or the reverse.
func TestShareOutcomesMatchTheConstants(t *testing.T) {
	ledger := []string{
		string(ShareOutcomeDone), string(ShareOutcomeFillTimeout), string(ShareOutcomeCancelled),
		string(ShareOutcomeAborted), string(ShareOutcomeRedirected), string(ShareOutcomeWrongPod),
	}
	published := slices.Clone(constants.UtilizationShareOutcomes)
	slices.Sort(ledger)
	slices.Sort(published)
	if !slices.Equal(ledger, published) {
		t.Fatalf("ledger outcomes %v, constants.UtilizationShareOutcomes %v", ledger, published)
	}
}
