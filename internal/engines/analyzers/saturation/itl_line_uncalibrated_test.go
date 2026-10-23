package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The derived mu is the line's own figure, whatever token rate the replicas
// report. A per-variant factor fitted to that rate was tried and removed: on
// the rerun of issue #270 its steady-state ratio was 0.988 (median of 107
// cycles), so the 13% it was built to correct had come from lines fitted on
// transients, which the steady gate now keeps out. What the factor did measure
// was the transients themselves: a first ratio of 1.42, taken whole on a ramp
// whose one-minute rates had not settled, priced the replica 22-40% high for
// 27 minutes after the line had been refitted.
//
// Here the replica sits at one fill for three minutes -- long enough for any
// settled-replica rule -- reporting 1.4 times the tokens the line predicts.
// The rate must stay the line's, and itl-gps-mismatch must report the gap
// against the line as fitted.
func TestDerivedMuIsTheLinesOwnFigure(t *testing.T) {
	const (
		kvTokens = 376480
		output   = 500.0
		k        = 0.64
		a, b     = 0.1025, itl.DefaultBaselineSec
	)
	line := itl.Model{A: a, B: b}
	kvReq := 1000 + output/2
	predicted := itl.TokenRate(line, k, kvTokens, kvReq)

	states := []domain.VariantReplicaState{{VariantName: "qwen32b-decode-wva",
		Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}
	an := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 10, 23, 10, 53, 0, 0, time.UTC)
	an.now = func() time.Time { return clock }

	rm := makeReplicaMetrics("qwen32b-decode-st2v9", "qwen32b-decode-wva",
		int64(240947), kvTokens, 0, 1000, output) // k x kvTokens
	rm.KvUsageInstant = k
	rm.AvgITL = line.ITLAt(k)
	rm.GenerationTokenRate = 1.4 * predicted

	var fields map[string]any
	var mismatch map[string]any
	for range 12 {
		ctx, logs := observedCtx(t)
		_, err := an.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states))
		require.NoError(t, err)
		entries := logs.FilterMessage("derived-mu").All()
		require.NotEmpty(t, entries)
		fields = entries[len(entries)-1].ContextMap()
		if m := logs.FilterMessage("itl-gps-mismatch").All(); len(m) > 0 {
			mismatch = m[len(m)-1].ContextMap()
		}
		assert.Empty(t, logs.FilterMessage("itl-gps-calibration").All())
		clock = clock.Add(15 * time.Second)
	}

	require.Equal(t, true, fields["ok"], "the line must price the replica")
	want := (0.68 * kvTokens / kvReq) / line.ITLAt(0.68) / output
	rate, _ := fields["rate"].(float64)
	assert.InDelta(t, want, rate, 0.01*want,
		"derived %.3f req/s; the line prices %.3f (factor %v)", rate, want, fields["gpsCalibration"])
	_, has := fields["gpsCalibration"]
	assert.False(t, has, "derived-mu carries no calibration factor")

	require.NotNil(t, mismatch, "a 1.4x gap is reported")
	got, _ := mismatch["predictedGPS"].(float64)
	assert.InDelta(t, predicted, got, 0.01*predicted, "compared against the line as fitted")
}
