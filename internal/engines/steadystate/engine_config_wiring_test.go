package steadystate

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// wva_engine_config is published from one place and deleted from three, and
// every one of those call sites was untested: the emitter has thorough specs of
// its own, so deleting any of the four calls left the whole suite green. These
// drive the engine's own methods instead.
//
// The same shape of gap, in the borrow path, was what let a flag silently stop
// reaching the record -- see TestBorrowedLineReachesTheRecord in the saturation
// package. A well-tested helper nobody calls is the failure this guards.

func engineConfigEngine(t *testing.T) (*Engine, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	return &Engine{
		metricsEmitter:     metrics.NewMetricsEmitter(),
		lastAnalyzerSeries: make(map[string]analyzerSeries),
		capacityStore:      capacity.NewStore(),
	}, registry
}

// deployWithArgs is a scale target carrying engine flags, so the capacity store
// parses real EngineParams rather than a hand-built struct -- the published
// fingerprint must be the one the live path would key on.
func deployWithArgs(args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "vllm",
						Command: []string{"vllm", "serve", "m"},
						Args:    args,
					}},
				},
			},
		},
	}
}

func TestPublishEngineConfig_PublishesTheFingerprintItWouldKeyOn(t *testing.T) {
	e, registry := engineConfigEngine(t)

	st := scaletarget.NewDeploymentAccessor(deployWithArgs(
		"--max-num-seqs=128", "--dtype=bfloat16", "--quantization=fp8"))
	e.capacityStore.LoadFromScaleTarget("ns", "m", "v1", "NVIDIA-H200", 1, st)

	e.publishEngineConfig("ns", "m", "v1", "NVIDIA-H200", 1)

	require.ElementsMatch(t, []string{"v1"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName),
		"the call site must publish exactly one series for the variant")

	// The flags it parsed must be the ones published, or the metric cannot be
	// read back to the configuration it labels.
	assert.ElementsMatch(t, []string{"128"},
		seriesLabels(t, registry, constants.WVAEngineConfig, "max_num_seqs"))
	assert.ElementsMatch(t, []string{"bfloat16"},
		seriesLabels(t, registry, constants.WVAEngineConfig, "weight_dtype"))
	assert.ElementsMatch(t, []string{"fp8"},
		seriesLabels(t, registry, constants.WVAEngineConfig, "quantization"))

	// And the digest must be non-empty: an empty one means the configuration
	// could not be read, and RecordEngineConfig drops the sample.
	fps := seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelFingerprint)
	require.Len(t, fps, 1)
	assert.NotEmpty(t, fps[0])
}

func TestPublishEngineConfig_PublishesNothingWithoutARecord(t *testing.T) {
	// An absent series is the honest signal for "the configuration could not be
	// read". A partial one would be read as a configuration.
	e, registry := engineConfigEngine(t)

	e.publishEngineConfig("ns", "m", "absent", "NVIDIA-H200", 1)

	assert.Empty(t, seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName))
}

func TestPublishEngineConfig_SurvivesAnUnsetStoreAndEmitter(t *testing.T) {
	// The engine's eviction and publication paths run regardless of whether
	// these were wired, so neither may panic.
	bare := &Engine{}
	assert.NotPanics(t, func() { bare.publishEngineConfig("ns", "m", "v1", "a", 1) })

	noStore := &Engine{metricsEmitter: metrics.NewMetricsEmitter()}
	assert.NotPanics(t, func() { noStore.publishEngineConfig("ns", "m", "v1", "a", 1) })
}

func TestEvictStaleAnalyzerSeries_DeletesTheConfigSeriesOfADepartedVariant(t *testing.T) {
	e, registry := engineConfigEngine(t)

	st := scaletarget.NewDeploymentAccessor(deployWithArgs("--max-num-seqs=128"))
	for _, v := range []string{"v1", "v2"} {
		e.capacityStore.LoadFromScaleTarget("ns", "m", v, "NVIDIA-H200", 1, st)
		e.publishEngineConfig("ns", "m", v, "NVIDIA-H200", 1)
	}
	require.ElementsMatch(t, []string{"v1", "v2"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName))

	// Two cycles of analyzer series: the second drops v2, which is what drives
	// the per-variant eviction.
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1", "v2"),
	})
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1"),
	})

	assert.ElementsMatch(t, []string{"v1"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName),
		"a departed variant's config series must go with its analyzer series")
}

func TestPruneAnalyzerSeries_DeletesTheConfigSeriesOfADepartedModel(t *testing.T) {
	e, registry := engineConfigEngine(t)

	st := scaletarget.NewDeploymentAccessor(deployWithArgs("--max-num-seqs=128"))
	for _, m := range []string{"m1", "m2"} {
		e.capacityStore.LoadFromScaleTarget("ns", m, "v", "NVIDIA-H200", 1, st)
		e.publishEngineConfig("ns", m, "v", "NVIDIA-H200", 1)
		e.recordAnalyzerMetrics("ns", m, []allocation.NamedAnalyzerResult{
			namedResult("saturation", nil, 5, "v"),
		})
	}
	require.ElementsMatch(t, []string{"m1", "m2"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelModelName))

	// m2 is no longer reconciled.
	e.pruneAnalyzerSeries(map[string]bool{"ns/m1": true})

	assert.ElementsMatch(t, []string{"m1"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelModelName),
		"a departed model's config series must go with its analyzer series")
}

func TestEvictAllAnalyzerSeries_ClearsEveryConfigSeries(t *testing.T) {
	// The path a cycle that analyzes no models at all takes.
	e, registry := engineConfigEngine(t)

	st := scaletarget.NewDeploymentAccessor(deployWithArgs("--max-num-seqs=128"))
	for _, m := range []string{"m1", "m2"} {
		e.capacityStore.LoadFromScaleTarget("ns", m, "v", "NVIDIA-H200", 1, st)
		e.publishEngineConfig("ns", m, "v", "NVIDIA-H200", 1)
		e.recordAnalyzerMetrics("ns", m, []allocation.NamedAnalyzerResult{
			namedResult("saturation", nil, 5, "v"),
		})
	}
	require.Len(t, seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelModelName), 2)

	e.evictAllAnalyzerSeries()

	assert.Empty(t, seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelModelName),
		"an idle cycle must not leave a configuration published with nothing to refresh it")
}

// TestRunV2AnalysisOnly_PublishesEachVariantsConfig drives the real call site.
// Everything above tests publishEngineConfig; this tests that something CALLS
// it, which is the gap that let the borrow flag silently stop reaching the
// record in the sibling package.
func TestRunV2AnalysisOnly_PublishesEachVariantsConfig(t *testing.T) {
	e, registry := engineConfigEngine(t)
	e.saturationV2Analyzer = saturation.NewSaturationAnalyzer(e.capacityStore)

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ModelID:        "m",
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "d"},
		},
	}
	st := scaletarget.NewDeploymentAccessor(deployWithArgs(
		"--max-num-seqs=128", "--dtype=bfloat16"))

	_, err := e.runV2AnalysisOnly(
		context.Background(), "m", "ns",
		nil, // no replica metrics: the publication happens before any analysis
		config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.85,
			ScaleDownBoundary:    0.70,
		},
		nil,
		map[string]scaletarget.ScaleTargetAccessor{
			utils.GetNamespacedKey("ns", "d"): st,
		},
		map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{"ns/v1": va},
		nil, 0,
	)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"v1"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName),
		"the analysis path must publish the configuration of every variant it "+
			"pre-populates the capacity store from")
	assert.ElementsMatch(t, []string{"128"},
		seriesLabels(t, registry, constants.WVAEngineConfig, "max_num_seqs"))

	// The published digest must be the store's own, or the metric labels a
	// configuration the analyzer would not key on.
	rec := e.capacityStore.Get("ns", "m", "v1")
	require.NotNil(t, rec)
	require.NotNil(t, rec.EngineParams)
	assert.ElementsMatch(t, []string{rec.EngineParams.Fingerprint()},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelFingerprint))
}

func TestRunV2AnalysisOnly_PublishesNothingForAVariantWithNoScaleTarget(t *testing.T) {
	// The path that logs "No scale target found" and continues: there is no
	// configuration to publish, and an absent series is the honest signal.
	e, registry := engineConfigEngine(t)
	e.saturationV2Analyzer = saturation.NewSaturationAnalyzer(e.capacityStore)

	// THE SEED IS WHAT MAKES THIS A TEST. publishEngineConfig no-ops when the
	// store holds no record for the variant, so an empty registry is also
	// exactly what DELETING the skip produces -- and what deleting the whole
	// step-1 loop produces. A verification pass proved both against the first
	// draft of this spec, which passed with the feature ripped out: failure
	// shape (b), an absence that holds because the subject never ran.
	//
	// With a complete record seeded the two outcomes finally differ: the skip
	// still publishes nothing, while a publish call reached without the skip
	// finds params and emits a series.
	e.capacityStore.Update("ns", "m", "v1", capacity.Record{
		AcceleratorName: "H100",
		GpuCount:        1,
		EngineParams: &capacity.EngineParams{
			Engine:                    inferenceengine.EngineVLLM,
			GpuMemoryUtilization:      0.9,
			BlockSize:                 16,
			KvCacheDtype:              "auto",
			WeightDtype:               "auto",
			TensorParallelSize:        1,
			MaxNumSeqs:                256,
			EffectiveMaxBatchedTokens: 8192,
		},
	})

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ModelID:        "m",
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "d"},
		},
	}

	_, err := e.runV2AnalysisOnly(
		context.Background(), "m", "ns", nil,
		config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.85,
			ScaleDownBoundary:    0.70,
		},
		nil,
		map[string]scaletarget.ScaleTargetAccessor{}, // none resolvable
		map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{"ns/v1": va},
		nil, 0,
	)
	require.NoError(t, err)

	assert.Empty(t, seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName))
}
