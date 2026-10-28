package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Two variants serving `$MODEL` agree on an empty checkpoint whichever ones
// they load, so an unread checkpoint is not a sharing key -- while the read
// itself stays complete, so the variant's own restore is not withheld.
func TestAnUnreadCheckpointIsNotASharingKey(t *testing.T) {
	const ns, model, variant = "test-ns", "test-model", "variant-a"
	params := &capacity.EngineParams{
		Engine: inferenceengine.EngineVLLM, GpuMemoryUtilization: 0.9, BlockSize: 16,
		KvCacheDtype: "auto", WeightDtype: "auto", TensorParallelSize: 1,
		MaxNumSeqs: 256, EffectiveMaxBatchedTokens: 8192,
	}

	store := capacity.NewStore()
	store.Update(ns, model, variant, capacity.Record{AcceleratorName: "H100", GpuCount: 1, EngineParams: params}, time.Now())
	require.NotEmpty(t, NewSaturationAnalyzer(store).engineFingerprint(ns, model, variant))

	unread := *params
	unread.ModelUnresolved = true
	require.True(t, unread.Complete(), "an unread checkpoint is not an incomplete read")
	store.Update(ns, model, variant, capacity.Record{AcceleratorName: "H100", GpuCount: 1, EngineParams: &unread}, time.Now())
	require.Empty(t, NewSaturationAnalyzer(store).engineFingerprint(ns, model, variant))
}
