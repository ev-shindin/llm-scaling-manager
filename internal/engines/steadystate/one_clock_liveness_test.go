package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/cycleclock"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// A result stamped at the pin is fresh however long the cycle ran before
// liveness looked at it. Compared with the wall clock, the 90 s window (three
// 30 s intervals) lost however long the cycle had already taken, and a cycle
// that ran past it marked its own analyzer stale.
func TestOneClock_LivenessJudgesAResultOnTheCycleClock(t *testing.T) {
	wall := time.Now()
	base := clocktesting.NewFakePassiveClock(wall.Add(-100 * time.Second))
	e := &Engine{clock: cycleclock.New(base)}
	release := e.clock.Pin()
	defer release()
	at := e.clock.Now()
	base.SetTime(wall) // the cycle has run 100 s past its pin

	results := []allocation.NamedAnalyzerResult{{
		Name: domain.SaturationAnalyzerName,
		Result: &domain.AnalyzerResult{AnalyzedAt: at, VariantCapacities: []domain.VariantCapacity{
			{VariantName: "v", Reason: "P1-obs"},
		}},
	}}
	e.updateLivenessAndSetLive(context.Background(), "ns", "m", results)
	assert.True(t, results[0].Live, "a result stamped at this cycle's pin is live")

	// And the grace for a listed analyzer that returned nothing this cycle.
	e.lastGoodAnalysis[utils.GetNamespacedKey("ns", "m")]["queueing"] = at
	grace := e.listedInGrace("ns", "m", config.ScalingPolicy{
		Analyzers: []config.AnalyzerScoreConfig{{Name: "queueing"}},
	}, nil)
	require.Len(t, grace, 1, "an analyzer stamped at this cycle's pin is inside its grace")
	assert.True(t, grace[0].Live)
}
