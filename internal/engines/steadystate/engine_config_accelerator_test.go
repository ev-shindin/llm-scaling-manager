package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// wva_engine_config exists to make the learned-state key explicable on a
// dashboard, and the key is composed from the accelerator among other things.
// The publish site was handed the RAW reading from the scale target while the
// key is built from the stabilised one -- the reading the analyzer remembers
// from the last cycle that resolved.
//
// Those two values are equal on a fleet whose pods all report their accelerator,
// which is every fleet the suite had. They differ on exactly the fleet this
// metric is read to understand, and there the consequences are two: a dashboard
// joining variants on the fingerprint groups them under an accelerator no key
// uses, and the series churns between two label sets as the reading flickers,
// each set going stale and back every cycle.

// stabiliserSpy is a domain.Analyzer that reports a fixed stable accelerator,
// standing in for an analyzer that resolved one on an earlier cycle. It records
// what it was asked so the test can tell "consulted" from "coincidence".
type stabiliserSpy struct {
	stable string
	asked  []string
}

func (s *stabiliserSpy) Name() string { return "stabiliser-spy" }

func (s *stabiliserSpy) Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	return nil, nil
}

func (s *stabiliserSpy) StableAcceleratorFor(namespace, variantName, reading string) string {
	s.asked = append(s.asked, namespace+"/"+variantName+"="+reading)
	return s.stable
}

// unresolvedVariant is a VA whose scale target names no accelerator at all: no
// nodeSelector, no affinity, no label. This is the premise of both specs below
// and is asserted rather than assumed.
func unresolvedVariant() (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, scaletarget.ScaleTargetAccessor) {
	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ModelID:        "m",
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "d"},
		},
	}
	return va, scaletarget.NewDeploymentAccessor(deployWithArgs("--max-num-seqs=128"))
}

func runWithAnalyzer(t *testing.T, e *Engine, va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling, st scaletarget.ScaleTargetAccessor) {
	t.Helper()
	_, err := e.buildInputAndRunSaturation(
		context.Background(),
		config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.85,
			ScaleDownBoundary:    0.70,
		},
		map[string]scaletarget.ScaleTargetAccessor{utils.GetNamespacedKey("ns", "d"): st},
		map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{"ns/v1": va},
	)
	require.NoError(t, err)
}

func TestPublishedAcceleratorIsTheOneTheKeyUses(t *testing.T) {
	e, registry := engineConfigEngine(t)
	spy := &stabiliserSpy{stable: "NVIDIA-H200"}
	e.setSaturation(spy)

	va, st := unresolvedVariant()

	// The premise: without the fix there is a different value to publish. If
	// this ever resolves, the spec below proves nothing.
	raw := accelerator.GetAcceleratorNameFromScaleTarget(va, st)
	require.False(t, constants.IsAcceleratorResolved(raw),
		"this scale target was supposed to name no accelerator, but read %q", raw)
	require.NotEqual(t, spy.stable, raw)

	runWithAnalyzer(t, e, va, st)

	assert.ElementsMatch(t, []string{spy.stable},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelAccelerator),
		"the metric published the raw reading, not the accelerator the "+
			"learned-state key is built from")
	assert.Equal(t, []string{"ns/v1=" + raw}, spy.asked,
		"the publish site must ask the analyzer, passing the reading it has")

	// The deployment-derived capacity record is filed under the same value:
	// filed under the raw reading, it was missed by the analyzer's lookups,
	// which go by the stable one.
	rec := e.capacityStore.Get("ns", "m", "v1")
	require.NotNil(t, rec, "precondition: the scale target was loaded into the store")
	assert.Equal(t, spy.stable, rec.AcceleratorName,
		"the capacity record was filed under the raw reading")
}

// TestPublishedAcceleratorFallsBackWithoutAStabiliser pins the other half: an
// analyzer that cannot answer leaves the raw reading, so an injected double --
// which is most of this package's tests -- keeps working and no path loses its
// accelerator label entirely.
func TestPublishedAcceleratorFallsBackWithoutAStabiliser(t *testing.T) {
	e, registry := engineConfigEngine(t)
	e.setSaturation(plainAnalyzer{})

	va, st := unresolvedVariant()
	raw := accelerator.GetAcceleratorNameFromScaleTarget(va, st)

	runWithAnalyzer(t, e, va, st)

	assert.ElementsMatch(t, []string{raw},
		seriesLabels(t, registry, constants.WVAEngineConfig, constants.LabelAccelerator),
		"an analyzer with no stabiliser must leave the reading alone")
}
