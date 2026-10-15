package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// TestRunV2AnalysisOnly_ForgetsOneVariantOnRequest drives the real call site:
// the annotation on the scale target, read where the engine loads the
// variant's capacity record, acted on once per token.
func TestRunV2AnalysisOnly_ForgetsOneVariantOnRequest(t *testing.T) {
	e, _ := engineConfigEngine(t)
	analyzer := saturation.NewSaturationAnalyzer(e.capacityStore)
	e.saturationV2Analyzer = analyzer

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ModelID:        "m",
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "d"},
		},
	}
	deploy := deployWithArgs("--max-num-seqs=128")
	run := func(token string) {
		if token != "" {
			deploy.Annotations = map[string]string{annotations.ForgetLearnedState: token}
		}
		_, err := e.runV2AnalysisOnly(
			context.Background(), "m", "ns", nil,
			config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, AnalyzerName: "saturation",
				ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70},
			nil,
			map[string]scaletarget.ScaleTargetAccessor{
				utils.GetNamespacedKey("ns", "d"): scaletarget.NewDeploymentAccessor(deploy),
			},
			map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{"ns/v1": va},
			nil, 0,
		)
		require.NoError(t, err)
	}
	// learnLive files a measured live record, as a serving cycle does.
	learnLive := func() {
		rec := e.capacityStore.Get("ns", "m", "v1")
		require.NotNil(t, rec)
		e.capacityStore.Update("ns", "m", "v1", capacity.Record{
			EffectiveCapacity: 4000, TotalKvCapacityTokens: 16000,
			EngineParams: rec.EngineParams, LearnedFrom: capacity.LearnedFromLive,
		})
	}
	isLive := func() bool { return e.capacityStore.Get("ns", "m", "v1").LearnedFrom == capacity.LearnedFromLive }

	run("")
	learnLive()
	run("")
	require.True(t, isLive(), "precondition: with no annotation the live record stands")

	run("2026-10-15T12:00")
	assert.False(t, isLive(), "the annotation did not forget the variant's live capacity")
	assert.Equal(t, "2026-10-15T12:00", analyzer.ForgottenToken("ns", "v1"))

	learnLive()
	run("2026-10-15T12:00")
	assert.True(t, isLive(), "an annotation left in place forgot the variant again on the next cycle")

	run("2026-10-15T13:00")
	assert.False(t, isLive(), "a new token did not ask again")
}
