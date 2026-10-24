package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// ExplicitMaxNumSeqs is the engine's cap only when its flag stated it. With
// no flag the parser assumes 256, and vLLM V1 picks 1024 on large
// accelerators: the ITL fit must not read a replica at 256 as capped there.
func TestExplicitMaxNumSeqs(t *testing.T) {
	for name, tc := range map[string]struct {
		params EngineParams
		want   int64
	}{
		"no flag":         {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--gpu-memory-utilization=0.9"))), 0},
		"vLLM flag":       {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-seqs=128"))), 128},
		"singular flag":   {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-seq", "64"))), 64},
		"unreadable flag": {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-seqs", "$UNSET_SEQS"))), 0},
		"a readable and an unreadable flag": {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment(
			"--max-num-seqs", "512", "--max-num-seqs", "$UNSET_SEQS"))), 0},
		"through the env": {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeDeploymentWithEnvAndArgs(
			[]corev1.EnvVar{{Name: "SEQS", Value: "96"}}, "--max-num-seqs", "$SEQS"))), 96},
	} {
		if got := tc.params.ExplicitMaxNumSeqs(); got != tc.want {
			t.Errorf("%s: ExplicitMaxNumSeqs = %d, want %d (MaxNumSeqs %d)", name, got, tc.want, tc.params.MaxNumSeqs)
		}
	}
	if got := (*EngineParams)(nil).ExplicitMaxNumSeqs(); got != 0 {
		t.Errorf("nil params: %d, want 0", got)
	}
}
