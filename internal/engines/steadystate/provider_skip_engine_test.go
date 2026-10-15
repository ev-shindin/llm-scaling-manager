package steadystate

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// Ten cycles with the physical provider skipped log the skip once, not ten
// times: the skip is a metric now, and the log speaks when the set changes.
func TestSelectV2OptimizerLogsASkippedProviderOnce(t *testing.T) {
	resetUsageAndHeadroom(t) // no physical observation: the physical stub is skipped

	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	ctx := ctrl.LoggerInto(context.Background(), logger)

	e := &Engine{
		GPULimiter: allocation.NewCompositeLimiter("gpu-and-quota-limiter", []allocation.Limiter{
			physicalStub{pools: map[string]allocation.ResourcePool{"H100": {Limit: 16}}},
			quotaLimiterFor("tenant", "H100", 2),
		}),
		optimizer: allocation.NewGreedyByScoreOptimizer(),
	}
	for range 10 {
		e.selectV2Optimizer(ctx, nil)
	}
	n := 0
	for _, l := range lines {
		if strings.Contains(l, `"provider"="gpu-limiter"`) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("ten cycles skipping the same provider logged it %d times, want 1:\n%s", n, strings.Join(lines, "\n"))
	}
}
