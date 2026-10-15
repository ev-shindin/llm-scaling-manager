package steadystate

import (
	"context"
	"maps"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// engineSkipReasons are the wva_constraint_provider_skipped reasons the
// steady-state engine owns. node_read_denied belongs to the readiness gate
// (cmd/main.go), which this engine must never clear.
var engineSkipReasons = []string{
	constants.ProviderSkipUnobserved,
	constants.ProviderSkipFailed,
	constants.ProviderSkipNoConstraints,
}

// providerSkipReport publishes which constraint providers the engine left out
// of its last decision.
//
// It used to be one Info line per skipped provider per cycle: a bound that had
// stopped applying was visible only to someone reading the log at the right
// moment, and a provider skipped for a day wrote thousands of identical lines.
// Now the set is a metric, replaced every time it is computed, and the log
// speaks only when the set changes: a provider newly skipped, skipped for a new
// reason, or back in force.
//
// Thread-safe; the zero value is ready to use.
type providerSkipReport struct {
	mu sync.Mutex
	// last maps each skipped provider to the cause it was last reported with.
	last map[string]string
}

// report publishes skipped, the providers left out of the decision just taken.
// An empty skipped clears what was reported before.
func (r *providerSkipReport) report(ctx context.Context, skipped []allocation.SkippedProvider) {
	current := make(map[string]string, len(skipped))
	reasons := make(map[string]string, len(skipped))
	for _, s := range skipped {
		current[s.Name] = s.Cause
		reasons[s.Name] = s.Reason
	}
	metrics.SetConstraintProvidersSkipped(engineSkipReasons, current)

	r.mu.Lock()
	defer r.mu.Unlock()
	if maps.Equal(r.last, current) {
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	for name, cause := range current {
		if prev, ok := r.last[name]; ok && prev == cause {
			continue
		}
		// The decision is still checked against every other provider; silence
		// here would read as "every limiter agreed".
		logger.Info("GPU constraint provider skipped; the other limiters still bound scaling",
			"provider", name, "cause", cause, "reason", reasons[name],
			"metric", constants.WVAConstraintProviderSkipped)
	}
	for name := range r.last {
		if _, still := current[name]; !still {
			logger.Info("GPU constraint provider bounds scaling again", "provider", name)
		}
	}
	r.last = current
}
