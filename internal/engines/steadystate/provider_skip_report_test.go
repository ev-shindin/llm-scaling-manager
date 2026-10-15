package steadystate

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// A skipped provider is logged when the skipped set changes, not every cycle:
// the review found one Info line per provider per cycle, so a provider skipped
// for a day wrote thousands of identical lines and nothing else said so.
func TestProviderSkipReportLogsOnChange(t *testing.T) {
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	ctx := ctrl.LoggerInto(context.Background(), logger)

	physical := allocation.SkippedProvider{Name: "gpu-limiter", Reason: "no physical GPU-usage observation yet",
		Unobserved: true, Cause: constants.ProviderSkipUnobserved}
	failed := physical
	failed.Cause, failed.Reason = constants.ProviderSkipFailed, "could not compute constraints: forbidden"

	var r providerSkipReport
	count := func(substr string) int {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, substr) {
				n++
			}
		}
		return n
	}

	for range 10 {
		r.report(ctx, []allocation.SkippedProvider{physical})
	}
	if got := count("provider skipped"); got != 1 {
		t.Fatalf("ten cycles with the same skip logged %d lines, want 1: %v", got, lines)
	}
	r.report(ctx, []allocation.SkippedProvider{failed})
	if got := count("provider skipped"); got != 2 {
		t.Fatalf("a new cause for the same provider must be logged: %d lines, want 2", got)
	}
	r.report(ctx, nil)
	r.report(ctx, nil)
	if got := count("bounds scaling again"); got != 1 {
		t.Fatalf("recovery logged %d times, want 1: %v", got, lines)
	}
}
