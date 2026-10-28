package steadystate

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
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
	e.capacityStore.LoadFromScaleTarget("ns", "m", "v1", "NVIDIA-H200", 1, st, time.Now())

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
		e.capacityStore.LoadFromScaleTarget("ns", "m", v, "NVIDIA-H200", 1, st, time.Now())
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

// sharedTargetCycle runs one cycle the way collectV2ModelRequest does: the
// analysis path publishes each enumerated VA's configuration, then one record
// covers every analyzer's result.
func sharedTargetCycle(t *testing.T, e *Engine, vaNames []string, results []allocation.NamedAnalyzerResult) {
	t.Helper()
	st := scaletarget.NewDeploymentAccessor(deployWithArgs("--max-num-seqs=128"))
	vas := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
	for _, name := range vaNames {
		vas["ns/"+name] = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ModelID:        "m",
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "d"},
			},
		}
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
		map[string]scaletarget.ScaleTargetAccessor{utils.GetNamespacedKey("ns", "d"): st},
		vas, nil, 0,
	)
	require.NoError(t, err)
	e.recordAnalyzerMetrics("ns", "m", results)
}

// Issue #172: the config series was evicted per (analyzer, variant), so a
// second analyzer dropping v2 for one cycle unpublished v2's configuration
// while saturation, and its VariantAutoscaling, still kept it.
func TestEngineConfig_SurvivesOneAnalyzerDroppingAVariant(t *testing.T) {
	e, registry := engineConfigEngine(t)
	e.saturationV2Analyzer = saturation.NewSaturationAnalyzer(e.capacityStore)

	second := [][]string{{"v1", "v2"}, {"v1"}, {"v1", "v2"}, {"v1", "v2"}}
	got := make([][]string, 0, len(second))
	for _, variants := range second {
		sharedTargetCycle(t, e, []string{"v1", "v2"}, []allocation.NamedAnalyzerResult{
			namedResult("saturation", nil, 5, "v1", "v2"),
			namedResult("throughput", nil, 5, variants...),
		})
		published := seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName)
		slices.Sort(published)
		got = append(got, published)
	}
	assert.Equal(t, [][]string{{"v1", "v2"}, {"v1", "v2"}, {"v1", "v2"}, {"v1", "v2"}}, got,
		"a variant the model still runs lost its configuration for a cycle")
}

func TestEngineConfig_SurvivesEveryAnalyzerDroppingAVariantItsVAKeeps(t *testing.T) {
	// The VA is still enumerated and its configuration published: no
	// analyzer reporting it does not mean it left.
	e, registry := engineConfigEngine(t)
	e.saturationV2Analyzer = saturation.NewSaturationAnalyzer(e.capacityStore)

	sharedTargetCycle(t, e, []string{"v1", "v2"}, []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1", "v2"),
	})
	sharedTargetCycle(t, e, []string{"v1", "v2"}, []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1"),
	})
	assert.ElementsMatch(t, []string{"v1", "v2"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName))
}

func TestEngineConfig_GoesWhenTheVariantLeaves(t *testing.T) {
	// v2's VA is gone and no analyzer reports it: its configuration goes in
	// the same cycle, through the real publish path.
	e, registry := engineConfigEngine(t)
	e.saturationV2Analyzer = saturation.NewSaturationAnalyzer(e.capacityStore)

	sharedTargetCycle(t, e, []string{"v1", "v2"}, []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1", "v2"),
		namedResult("throughput", nil, 5, "v1", "v2"),
	})
	sharedTargetCycle(t, e, []string{"v1"}, []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1"),
		namedResult("throughput", nil, 5, "v1"),
	})
	assert.ElementsMatch(t, []string{"v1"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelVariantName),
		"a departed variant's configuration stayed published")
}

func TestPruneAnalyzerSeries_DeletesAConfigPublishedWithoutARecord(t *testing.T) {
	// A cycle that published a configuration and then failed before
	// recordAnalyzerMetrics leaves the series tracked only as published.
	e, registry := engineConfigEngine(t)

	st := scaletarget.NewDeploymentAccessor(deployWithArgs("--max-num-seqs=128"))
	for _, m := range []string{"m1", "m2"} {
		e.capacityStore.LoadFromScaleTarget("ns", m, "v", "NVIDIA-H200", 1, st, time.Now())
		e.publishEngineConfig("ns", m, "v", "NVIDIA-H200", 1)
	}
	e.pruneAnalyzerSeries(map[string]bool{"ns/m1": true})
	assert.ElementsMatch(t, []string{"m1"},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelModelName))

	e.evictAllAnalyzerSeries()
	assert.Empty(t, seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelModelName))
}

func TestPruneAnalyzerSeries_DeletesTheConfigSeriesOfADepartedModel(t *testing.T) {
	e, registry := engineConfigEngine(t)

	st := scaletarget.NewDeploymentAccessor(deployWithArgs("--max-num-seqs=128"))
	for _, m := range []string{"m1", "m2"} {
		e.capacityStore.LoadFromScaleTarget("ns", m, "v", "NVIDIA-H200", 1, st, time.Now())
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
		e.capacityStore.LoadFromScaleTarget("ns", m, "v", "NVIDIA-H200", 1, st, time.Now())
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

	// The accelerator label is published under the stabilised reading, reached
	// through an assertion on this field. The compile-time var beside the
	// interface pins the concrete TYPE; this pins the FIELD, which is what the
	// assertion actually tests -- engine.go builds the analyzer through
	// WithHorizons, and a wrapper returned from there would satisfy the var and
	// fail here. It is the only spec that CHECKS the field:
	// PublishesNothingForAVariantWithNoScaleTarget below also wires the real
	// analyzer, but its scale-target map is empty so the loop continues before
	// reaching this path at all.
	stabiliser, stabilises := e.saturationV2Analyzer.(acceleratorStabiliser)
	require.True(t, stabilises,
		"the real analyzer stopped satisfying acceleratorStabiliser, so the "+
			"metric is publishing the raw accelerator and nothing else noticed")

	// Compared against the stabiliser MEASURED, not a GPU name retyped here:
	// this fixture names no accelerator, so a hardcoded name asserts a value
	// the path cannot produce.
	//
	// Be clear about what this does NOT prove. No memo is ever seeded here, so
	// the stabiliser falls through both branches and returns the reading
	// unchanged -- stable EQUALS raw -- and this comparison would therefore
	// still pass if the publish site regressed to the raw reading, which is
	// the very bug the series exists to fix. What has teeth in this spec is
	// the field assertion above. The divergence case needs a warmed memo and
	// is covered by TestPublishedAcceleratorIsTheOneTheKeyUses.
	raw := accelerator.GetAcceleratorNameFromScaleTarget(va, st)
	assert.ElementsMatch(t, []string{stabiliser.StableAcceleratorFor("ns", "v1", raw)},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelAccelerator),
		"the accelerator label must be what the stabiliser reports for the "+
			"reading this path computed")
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
	}, time.Now())

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
