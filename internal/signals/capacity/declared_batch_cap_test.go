package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// annotatedDeployment is a vLLM Deployment whose flags come from a launcher
// script, with the scale target's own annotations set to annots.
func annotatedDeployment(annots map[string]string) scaletarget.ScaleTargetAccessor {
	return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: annots},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "vllm", Image: "vllm/vllm-openai:v0.11.0", Command: []string{"/bin/sh", "-c", "/start.sh"}},
			}},
		}},
	})
}

// The llm-d.ai/max-num-seqs annotation is read into DeclaredBatchCap: a
// positive integer, and nothing else. The pod template's annotations are
// not read, and the flags' own cap is left as the flags state it.
func TestDeclaredBatchCapIsReadFromTheScaleTarget(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"128", 128}, {" 64 ", 64}, {"0", 0}, {"-1", 0}, {"1.5", 0}, {"lots", 0}, {"", 0},
	} {
		value, want := tc.value, tc.want
		p := ParseEngineArgs(inferenceengine.EngineVLLM, annotatedDeployment(map[string]string{annotations.MaxNumSeqs: value}))
		assert.Equal(t, want, p.DeclaredBatchCap, "annotation %q", value)
		assert.Zero(t, p.ExplicitMaxNumSeqs(), "the annotation is not a flag")
	}
	assert.Zero(t, ParseEngineArgs(inferenceengine.EngineVLLM, annotatedDeployment(nil)).DeclaredBatchCap)

	tmpl := annotatedDeployment(nil)
	dep := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: *tmpl.GetLeaderPodTemplateSpec()}}
	dep.Spec.Template.Annotations = map[string]string{annotations.MaxNumSeqs: "128"}
	assert.Zero(t, ParseEngineArgs(inferenceengine.EngineVLLM, scaletarget.NewDeploymentAccessor(dep)).DeclaredBatchCap,
		"the pod template's annotations are the pods', not the scale target's")
}

// The declared cap is not hashed: annotating a variant moves no digest, so
// it keeps its learned state.
func TestDeclaredBatchCapIsNotHashed(t *testing.T) {
	plain := ParseEngineArgs(inferenceengine.EngineVLLM, annotatedDeployment(nil))
	declared := ParseEngineArgs(inferenceengine.EngineVLLM, annotatedDeployment(map[string]string{annotations.MaxNumSeqs: "128"}))
	assert.Equal(t, plain.Fingerprint(), declared.Fingerprint())
	assert.Equal(t, plain.ArgsTag(), declared.ArgsTag())
	assert.False(t, plain.IsCapacityCompatible(&declared), "two variants that declare different caps must not price each other")
}

// Setting the annotation on a variant already measured re-tags its live
// record's parameters, as an image change does, without discarding what was
// measured.
func TestDeclaredBatchCapRetagsALiveRecord(t *testing.T) {
	s := NewStore()
	now := time.Now()
	plain := ParseEngineArgs(inferenceengine.EngineVLLM, annotatedDeployment(nil))
	s.Update("ns", "m", "v", Record{
		AcceleratorName: "H100", GpuCount: 1, EngineParams: &plain,
		EffectiveCapacity: 50_000, LearnedFrom: LearnedFromLive, LearnedAt: now,
	}, now)

	s.LoadFromScaleTarget("ns", "m", "v", "H100", 1, annotatedDeployment(map[string]string{annotations.MaxNumSeqs: "128"}), now)
	rec := s.Get("ns", "m", "v")
	require.NotNil(t, rec)
	assert.Equal(t, LearnedFromLive, rec.LearnedFrom, "the live record is kept")
	assert.Equal(t, int64(50_000), rec.EffectiveCapacity, "what was measured is kept")
	assert.Equal(t, int64(128), rec.EngineParams.DeclaredBatchCap, "the declared cap reaches the record")
}

// An annotation the flags override is not read, and does not split two
// configurations that agree on everything else.
func TestDeclaredBatchCapIsComparedOnlyWhereRead(t *testing.T) {
	a := EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 64, MaxNumSeqsSet: true}
	b := a
	b.DeclaredBatchCap = 128
	assert.True(t, a.IsCapacityCompatible(&b), "the flags state the cap; the annotation is not read")
	a.MaxNumSeqsSet, b.MaxNumSeqsSet = false, false
	assert.False(t, a.IsCapacityCompatible(&b), "with the flags unread, the declared cap is the cap")
}
