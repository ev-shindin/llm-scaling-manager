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

// BatchCap prices a replica, so it takes max_num_seqs stated or defaulted,
// lowered by --max-num-active-seqs when that is set (issue #335).
func TestBatchCap(t *testing.T) {
	for name, tc := range map[string]struct {
		params EngineParams
		want   int64
	}{
		"neither":              {EngineParams{}, 0},
		"max_num_seqs alone":   {EngineParams{MaxNumSeqs: 256}, 256},
		"active below":         {EngineParams{MaxNumSeqs: 256, MaxNumActiveSeqs: 64}, 64},
		"active equal":         {EngineParams{MaxNumSeqs: 256, MaxNumActiveSeqs: 256}, 256},
		"active above":         {EngineParams{MaxNumSeqs: 256, MaxNumSeqsSet: true, MaxNumActiveSeqs: 512}, 256},
		"active alone":         {EngineParams{MaxNumActiveSeqs: 64}, 64},
		"defaulted max, flag":  {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-active-seqs=32"))), 32},
		"defaulted max, alone": {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--gpu-memory-utilization=0.9"))), 256},
		// vLLM refuses an active cap above max_num_seqs, so a stated one
		// above the defaulted 256 is the engine's real bound.
		"defaulted max, active above": {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-active-seqs=512"))), 512},
		"stated max, active above":    {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-seqs=256", "--max-num-active-seqs=512"))), 256},
		"unreadable max, active":      {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-seqs", "$UNSET_SEQS", "--max-num-active-seqs=512"))), 512},
	} {
		if got := tc.params.BatchCap(); got != tc.want {
			t.Errorf("%s: BatchCap = %d, want %d", name, got, tc.want)
		}
	}
	if got := (*EngineParams)(nil).BatchCap(); got != 0 {
		t.Errorf("nil params: %d, want 0", got)
	}
}

// --max-num-active-seqs (vLLM v0.31+) stops the running batch below
// max_num_seqs, so the batch cap is the lower of the two (issue #308). Read in
// every form max_num_seqs is.
func TestMaxNumActiveSeqsIsTheBatchCap(t *testing.T) {
	parse := func(args ...string) EngineParams {
		return ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment(args...)))
	}
	shell := makeTestDeployment()
	shell.Spec.Template.Spec.Containers[0].Command = []string{
		"/bin/sh", "-c", "vllm serve m --max-num-seqs 512 --max-num-active-seqs 128"}
	for name, tc := range map[string]struct {
		params     EngineParams
		wantActive int64
		wantCap    int64
	}{
		"--x=v alone":                {parse("--max-num-active-seqs=64"), 64, 64},
		"--x v alone":                {parse("--max-num-active-seqs", "64"), 64, 64},
		"underscore spelling":        {parse("--max_num_active_seqs=48"), 48, 48},
		"below max_num_seqs":         {parse("--max-num-seqs=256", "--max-num-active-seqs=64"), 64, 64},
		"flag order does not matter": {parse("--max-num-active-seqs", "64", "--max-num-seqs", "256"), 64, 64},
		"the lower of the two":       {parse("--max-num-seqs=32", "--max-num-active-seqs=64"), 64, 32},
		"through the env": {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeDeploymentWithEnvAndArgs(
			[]corev1.EnvVar{{Name: "ACTIVE", Value: "40"}}, "--max-num-active-seqs", "$ACTIVE"))), 40, 40},
		"in a shell command": {ParseVLLMArgs(scaletarget.NewDeploymentAccessor(shell)), 128, 128},
		// An active cap nothing could read falls back to an explicit
		// max_num_seqs, which vLLM guarantees is at or above it: still a true
		// upper bound on the batch.
		"unreadable, max_num_seqs stated": {parse("--max-num-seqs=256", "--max-num-active-seqs", "$UNSET_ACTIVE"), 0, 256},
		"unreadable alone":                {parse("--max-num-active-seqs", "$UNSET_ACTIVE"), 0, 0},
		// vLLM: Field(default=None, ge=1).
		"zero is not a value vLLM runs": {parse("--max-num-seqs=256", "--max-num-active-seqs=0"), 0, 256},
		"not a number":                  {parse("--max-num-active-seqs=many"), 0, 0},
		"no value token":                {parse("--max-num-seqs=256", "--max-num-active-seqs"), 0, 256},
		"a later unreadable occurrence": {parse("--max-num-seqs=256", "--max-num-active-seqs=64", "--max-num-active-seqs", "$UNSET_ACTIVE"), 0, 256},
		"a later readable occurrence":   {parse("--max-num-seqs=256", "--max-num-active-seqs", "$UNSET_ACTIVE", "--max-num-active-seqs=64"), 64, 64},
		"absent: max_num_seqs alone":    {parse("--max-num-seqs=256"), 0, 256},
	} {
		if got := tc.params.MaxNumActiveSeqs; got != tc.wantActive {
			t.Errorf("%s: MaxNumActiveSeqs = %d, want %d", name, got, tc.wantActive)
		}
		if got := tc.params.ExplicitMaxNumSeqs(); got != tc.wantCap {
			t.Errorf("%s: ExplicitMaxNumSeqs = %d, want %d", name, got, tc.wantCap)
		}
	}

	// An unreadable value is NOT recorded: marking the read incomplete would
	// change the fingerprint and withhold learned-state sharing and restore
	// for a deployment whose only change is that this flag is now read. The
	// read stays complete and hashes exactly as it would without the flag.
	without := parse("--max-num-seqs=256")
	for _, args := range [][]string{
		{"--max-num-seqs=256", "--max-num-active-seqs", "$UNSET_ACTIVE"},
		{"--max-num-seqs=256", "--max-num-active-seqs=0"},
		{"--max-num-seqs=256", "--max-num-active-seqs=many"},
	} {
		p := parse(args...)
		if !p.Complete() {
			t.Errorf("%v reads as incomplete: Unresolved=%v", args, p.Unresolved)
		}
		if p.Fingerprint() != without.Fingerprint() {
			t.Errorf("%v changed the fingerprint against the same flags without it", args)
		}
	}

	// A read cap is hashed (issue #334): a rollout that changes only it is a
	// change of arguments, as IsCapacityCompatible already says.
	read := parse("--max-num-seqs=256", "--max-num-active-seqs=64")
	if !read.Complete() {
		t.Errorf("a read active cap reads as incomplete: Unresolved=%v", read.Unresolved)
	}
	if read.ArgsFingerprint() == without.ArgsFingerprint() {
		t.Error("--max-num-active-seqs=64 left the arguments fingerprint as it is without the flag")
	}
}
