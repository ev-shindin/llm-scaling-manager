package saturation

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// A variant parked at zero whose engine was rolled to a configuration that
// changes the KV cache's shape has no measurement under the new arguments, no
// k1 carried across, and usually no sibling running the new flags yet. Its
// first scale-from-zero order is priced from the derived figure, and before
// the prior-live ceiling that figure had no bound at all: ~320,000 tokens per
// replica against a measured 12,800, so the order was ~25x too small. The
// last measurement now caps it until a new pod is priced.
func TestAParkedVariantIsNotPricedAboveItsLastMeasurementAfterARollout(t *testing.T) {
	const (
		ns, model, variant = "test-ns", "test-model", "variant-a"
		accel              = "H100"
		measured           = 12_800
	)
	load := func(store *capacity.Store, args ...string) {
		store.LoadFromScaleTarget(ns, model, variant, accel, 1,
			scaletarget.NewDeploymentAccessor(&appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "vllm", Command: []string{"vllm", "serve", model}, Args: args,
				}}}},
			}}), time.Now())
	}

	for _, tc := range []struct {
		name    string
		rollout []string
	}{
		{"memory share", []string{"--max-num-seqs=256", "--gpu-memory-utilization=0.85"}},
		{"kv cache dtype", []string{"--max-num-seqs=256", "--kv-cache-dtype=fp8"}},
		{"larger batch", []string{"--max-num-seqs=512"}},
		// The stored fallback is the per-step budget, here above the measurement.
		{"larger batch-token budget", []string{"--max-num-seqs=256", "--max-num-batched-tokens=65536"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := capacity.NewStore()
			load(store, "--max-num-seqs=256")
			loaded := store.Get(ns, model, variant)
			require.NotNil(t, loaded)
			store.Update(ns, model, variant, capacity.Record{
				AcceleratorName: accel, GpuCount: 1,
				TotalKvCapacityTokens: 16_000, EffectiveCapacity: measured,
				EngineParams: loaded.EngineParams, LearnedFrom: capacity.LearnedFromLive,
			}, time.Now())
			load(store, tc.rollout...) // the rollout, read while parked
			load(store, tc.rollout...) // and again on the next cycle

			rec := store.Get(ns, model, variant)
			require.NotEqual(t, capacity.LearnedFromLive, rec.LearnedFrom, "precondition: the record was replaced")
			require.Zero(t, rec.TotalKvCapacityTokens, "precondition: no k1 carried")

			a := NewSaturationAnalyzer(store)
			got := a.estimateStoredCapacity(rec, model, ns, variant, accel, 1, 0.8, 1000, 600, logr.Discard())
			require.Greater(t, got, 0.0, "precondition: the estimate path ran")
			require.LessOrEqual(t, got, float64(measured),
				"a parked variant was priced above what it was last measured at")

			// Without workload averages the stored fallback is capped alike.
			got = a.estimateStoredCapacity(rec, model, ns, variant, accel, 1, 0.8, 0, 0, logr.Discard())
			require.LessOrEqual(t, got, float64(measured))
		})
	}
}
