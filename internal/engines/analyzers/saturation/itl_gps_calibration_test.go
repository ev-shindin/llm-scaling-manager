package saturation

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The run behind issue #270's second half: Qwen3-32B-FP8 on one H200, vLLM
// v0.26.0, 1000 in / 500 out at 192 concurrent, one replica in evgensh-sg2.
// Each row is one scrape interval of the pod's own counters (vllm-metrics.log,
// 08:11-08:19): mean KV fill, and generation tokens over the interval. The
// replica ran 189-192 requests throughout, so it completed GPS/500 requests a
// second: 6.05 on average.
var runSg2 = []struct{ k, gps float64 }{
	{0.602, 3231}, {0.674, 3069}, {0.680, 2900}, {0.638, 2911},
	{0.605, 2925}, {0.603, 3014}, {0.636, 3136},
}

const (
	runKvTokens = 376480
	runOutput   = 500.0
	// The line the run's ITL histogram produced: the pinned-B fit at the
	// default 6 ms intercept, A as the controller logged it at held=20.
	runA = 0.1025
	runB = 0.006
)

// runReplica is the sg2 replica at fill k generating gps tokens a second, with
// the inter-token latency its histogram reported there: the line itself, which
// is what a fit through those readings reproduces.
func runReplica(k, gps float64) domain.ReplicaMetrics {
	rm := makeReplicaMetrics("qwen32b-decode-st2v9", "qwen32b-decode-wva",
		int64(k*runKvTokens), runKvTokens, 0, 1000, runOutput)
	rm.KvUsageInstant = k
	rm.AvgITL = runA*k + runB
	rm.GenerationTokenRate = gps
	return rm
}

// derivedRate runs the analyzer over the given readings, four 15s cycles per
// row (each row is one minute), and returns the derived mu of the last cycle.
func derivedRate(t *testing.T, rows []struct{ k, gps float64 }) (rate, calibration float64, logs *observer.ObservedLogs) {
	t.Helper()
	states := []domain.VariantReplicaState{{VariantName: "qwen32b-decode-wva",
		Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 10, 23, 8, 11, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	for _, row := range rows {
		for range 4 {
			ctx, l := observedCtx(t)
			_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{runReplica(row.k, row.gps)}, states))
			require.NoError(t, err)
			logs = l
			clock = clock.Add(15 * time.Second)
		}
	}
	entries := logs.FilterMessage("derived-mu").All()
	require.NotEmpty(t, entries)
	fields := entries[len(entries)-1].ContextMap()
	require.Equal(t, true, fields["ok"], "the line must price the replica")
	rate, _ = fields["rate"].(float64)
	calibration, _ = fields["gpsCalibration"].(float64)
	return rate, calibration, logs
}

// The line under-priced the replica by the ratio of its ITL readings to the
// time a sequence actually took per token: 1.13 on average over the run, so a
// derived 5.41 req/s against 6.05 measured -- 11% low, and a floor ordering
// 12% more replicas than the load needed. Calibrated by the replica's own
// token rate, the derived figure has to land within 5% of what it measured.
func TestDerivedMuMatchesTheRunsMeasuredThroughput(t *testing.T) {
	var gps float64
	for _, r := range runSg2 {
		gps += r.gps
	}
	measured := gps / float64(len(runSg2)) / runOutput

	rate, calibration, _ := derivedRate(t, runSg2)

	// What the line alone prices at kPrice 0.68, for the record of how far off
	// it was: seqs = 0.68 x 376480 / 1250, ITL = A x 0.68 + B.
	uncalibrated := (0.68 * runKvTokens / 1250) / (runA*0.68 + runB) / runOutput
	require.InDelta(t, 5.41, uncalibrated, 0.01)
	t.Logf("derived %.3f req/s, measured %.3f, line alone %.3f, factor %.3f",
		rate, measured, uncalibrated, calibration)

	assert.InDelta(t, 1.0, rate/measured, 0.05,
		"derived %.3f req/s against %.3f measured (line alone: %.3f, factor %.3f)",
		rate, measured, uncalibrated, calibration)
	assert.InDelta(t, 1.13, calibration, 0.06,
		"the factor is the run's own ratio of reported to predicted token rate")
}

// A ramp is not evidence. The fill moves faster than the one-minute token rate
// follows it, so the comparison reads the line as wrong on every replica at
// once (run T). A replica whose k moved by more than DefaultMaxKDrift within
// the last two minutes must not move the factor.
func TestCalibrationIgnoresARamp(t *testing.T) {
	ramp := []struct{ k, gps float64 }{
		{0.20, 800}, {0.35, 1500}, {0.50, 2200}, {0.65, 2600},
	}
	_, calibration, logs := derivedRate(t, ramp)
	assert.Empty(t, logs.FilterMessage("itl-gps-calibration").All())
	assert.Equal(t, 1.0, calibration, "a fleet that never settled is priced from the line as fitted")
}

// Once measured, the factor outlives the evidence: an over-provisioned fleet
// sits below DefaultGPSMinKForVerification, where no comparison is made, and
// it is exactly then that the derived mu prices the shape arriving.
func TestCalibrationIsKeptWhenTheFleetGoesQuiet(t *testing.T) {
	rows := append([]struct{ k, gps float64 }{}, runSg2...)
	for range 3 {
		rows = append(rows, struct{ k, gps float64 }{0.20, 900})
	}
	_, calibration, logs := derivedRate(t, rows)
	assert.Empty(t, logs.FilterMessage("itl-gps-calibration").All(),
		"no comparison at k 0.20")
	assert.Greater(t, calibration, 1.05, "the factor measured while the fleet worked is kept")
	assert.False(t, math.IsNaN(calibration))
}
