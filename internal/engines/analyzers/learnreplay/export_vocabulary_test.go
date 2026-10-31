package learnreplay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// A run under the utilization-share optimizer records the policy's optimizer
// type, config.OptimizerTypeUtilizationShare, in every saturation Analyze.
// It is the code's own word, so the export keeps it and the export replays.
// Before the word was in the vocabulary, every recording of such a run was
// refused ("Optimizer.Type in saturation.Analyze holds a value that is not
// allowed after the export"), the E1 benchmark scenario among them.
func TestExport_KeepsTheUtilizationShareOptimizerType(t *testing.T) {
	pol := &config.ScalingPolicy{
		KvCacheThreshold: 0.8,
		Optimizer: &config.OptimizerConfig{
			Type:             config.OptimizerTypeUtilizationShare,
			UtilizationShare: &config.UtilizationShareConfig{Shadow: false},
		},
	}
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, pol, nil)
	require.Contains(t, string(rec), `"Type":"`+config.OptimizerTypeUtilizationShare+`"`, "precondition: the recording holds the optimizer type")

	out, _, err := Export(rec, ExportOptions{Key: []byte("k")})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"Type":"`+config.OptimizerTypeUtilizationShare+`"`, "the optimizer type is kept as it is")

	r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, r.Failed())
	assert.Empty(t, r.Mismatches, "the export does not replay")
}
