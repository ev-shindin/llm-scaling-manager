package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// TestCompositeIsJudgedAgainstSaturationsBand pins what an analyzer's band
// does (#220): a band set on throughput shapes throughput's own result but
// not the combined decision, which reads throughput's replica need and judges
// it against saturation's band. Saturation's own override does move the
// decision. The config read warns about the first
// (config.InertAnalyzerThresholds).
func TestCompositeIsJudgedAgainstSaturationsBand(t *testing.T) {
	sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: liveEntry(domain.SaturationAnalyzerName, 100, 200).Result}
	tp := &fakeAnalyzerWithResult{analyzerName: "throughput", result: liveEntry("throughput", 300, 100).Result} // needs 3
	e := &Engine{analyzers: testAnalyzers(sat, analyzerEntry{name: "throughput", analyzer: tp})}
	run := func(satUp, tpUp *float64) (allocation.NamedAnalyzerResult, allocation.NamedAnalyzerResult) {
		t.Helper()
		cfg := config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: domain.SaturationAnalyzerName, ScaleUpThreshold: satUp},
				{Name: "throughput", ScaleUpThreshold: tpUp}}}
		named, err := e.runAnalyzers(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
		require.NoError(t, err)
		var own allocation.NamedAnalyzerResult
		for _, nr := range named {
			if nr.Name == "throughput" {
				own = nr
			}
		}
		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
		require.NoError(t, err)
		return own, req.CompositeSignal
	}
	low, lower := 0.5, 0.6

	baseOwn, base := run(nil, nil)
	require.Positive(t, base.RequiredCapacity, "setup: throughput's need of 3 grows the model")

	own, composite := run(nil, &low)
	assert.Greater(t, own.RequiredCapacity, baseOwn.RequiredCapacity, "throughput's own result reads its band")
	assert.InDelta(t, base.RequiredCapacity, composite.RequiredCapacity, 1e-9, "the decision does not")

	_, satBand := run(&lower, nil)
	assert.Greater(t, satBand.RequiredCapacity, base.RequiredCapacity, "saturation's band decides")
}
