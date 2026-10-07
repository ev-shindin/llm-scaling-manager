package capacity

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// A value this parser cannot use becomes a DEFAULT, and until the Unresolved
// set existed nothing could tell that default from the engine's real setting.
//
// Measured on a Kubernetes cluster rather than reasoned about: an
// llm-d/llmdbench Deployment runs the engine through a shell wrapper and puts
// the flag VALUES in the container env --
// `--block-size $VLLM_BLOCK_SIZE` with `VLLM_BLOCK_SIZE=128`. ParseInt failed
// on the literal "$VLLM_BLOCK_SIZE", BlockSize kept its default of 16, and
// `wva_engine_config` reported block_size=16 for an engine running 128.
// `--max-model-len $VLLM_MAX_MODEL_LEN` reported 0 against a real 16384.
//
// Why that is a defect and not an inaccuracy: these fields key a SHARING
// decision. A silently-defaulted field makes two different engines hash to one
// digest, and that digest licenses one variant to price itself from a latency
// line the other measured.

// shellDeployment is the shape the cluster actually runs: the engine invoked
// from `/bin/sh -c`, flag values as shell variable references, real values in
// the container env.
func shellDeployment(cmd string, env map[string]string) *appsv1.Deployment {
	vars := make([]corev1.EnvVar, 0, len(env))
	for k, v := range env {
		vars = append(vars, corev1.EnvVar{Name: k, Value: v})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "vllm",
						Command: []string{"/bin/sh", "-c", cmd},
						Env:     vars,
					}},
				},
			},
		},
	}
}

func parseShell(cmd string, env map[string]string) EngineParams {
	return ParseVLLMArgs(scaletarget.NewDeploymentAccessor(shellDeployment(cmd, env)))
}

func TestParserResolvesTheClusterFixture(t *testing.T) {
	// Verbatim shape from the cluster, reduced to the hashed flags.
	p := parseShell(
		"vllm serve Qwen/Qwen3-0.6B "+
			"--max-model-len $VLLM_MAX_MODEL_LEN "+
			"--block-size $VLLM_BLOCK_SIZE "+
			"--gpu-memory-utilization $VLLM_ACCELERATOR_MEM_UTIL "+
			"--max-num-seqs ${VLLM_MAX_NUM_SEQ} "+
			"--tensor-parallel-size $(TP_SIZE) "+
			"--max-num-batched-tokens 8192 "+
			"--dtype bfloat16",
		map[string]string{
			"VLLM_MAX_MODEL_LEN":        "16384",
			"VLLM_BLOCK_SIZE":           "128",
			"VLLM_ACCELERATOR_MEM_UTIL": "0.9",
			"VLLM_MAX_NUM_SEQ":          "256",
			"TP_SIZE":                   "2",
		})

	// The two the cluster got wrong, which is the whole point.
	if p.MaxModelLen != 16384 {
		t.Errorf("MaxModelLen = %d, want 16384 (it read 0 on the cluster)", p.MaxModelLen)
	}
	if p.BlockSize != 128 {
		t.Errorf("BlockSize = %d, want 128 (it read the default 16 on the cluster)", p.BlockSize)
	}
	// ${VAR} is the shell's brace form and $(VAR) is Kubernetes' own, which the
	// kubelet expands from the same env. Both have to resolve or a manifest
	// using either is read wrong.
	if p.MaxNumSeqs != 256 {
		t.Errorf("MaxNumSeqs = %d, want 256 from ${VLLM_MAX_NUM_SEQ}", p.MaxNumSeqs)
	}
	if p.TensorParallelSize != 2 {
		t.Errorf("TensorParallelSize = %d, want 2 from $(TP_SIZE)", p.TensorParallelSize)
	}
	if p.GpuMemoryUtilization != 0.9 {
		t.Errorf("GpuMemoryUtilization = %v, want 0.9", p.GpuMemoryUtilization)
	}
	if !p.Complete() {
		t.Errorf("Unresolved = %v, want empty: every flag here is resolvable", p.Unresolved)
	}
}

func TestParserRecordsWhatItCouldNotRead(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		env  map[string]string
		want string // the key that must be recorded
	}{{
		name: "a reference the container does not define",
		cmd:  "vllm serve m --block-size $VLLM_BLOCK_SIZE",
		env:  map[string]string{"SOMETHING_ELSE": "128"},
		want: "block_size",
	}, {
		name: "a resolved value that is not a number",
		cmd:  "vllm serve m --max-model-len sixteen-thousand",
		env:  nil,
		want: "max_model_len",
	}, {
		name: "a memory fraction usableFraction rejects",
		cmd:  "vllm serve m --gpu-memory-utilization NaN",
		env:  nil,
		want: "gpu_memory_utilization",
	}, {
		name: "a reference resolving through another reference is not chased",
		cmd:  "vllm serve m --max-num-seqs $OUTER",
		env:  map[string]string{"OUTER": "$INNER", "INNER": "512"},
		want: "max_num_seqs",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseShell(tc.cmd, tc.env)
			found := false
			for _, k := range p.Unresolved {
				if k == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("Unresolved = %v, want it to contain %q -- otherwise the "+
					"default below it is indistinguishable from the engine's real setting",
					p.Unresolved, tc.want)
			}
			if p.Complete() {
				t.Error("Complete() is true with an unreadable flag on record")
			}
		})
	}
}

func TestParserWillNotGuessAtAValueFromAConfigMap(t *testing.T) {
	// valueFrom cannot be read without an API client, and reading it as the
	// empty string would be a guess -- the exact thing that made block_size 16.
	d := shellDeployment("vllm serve m --block-size $VLLM_BLOCK_SIZE", nil)
	d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{
		Name: "VLLM_BLOCK_SIZE",
		ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "engine-cfg"},
				Key:                  "block-size",
			},
		},
	}}
	p := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(d))

	if p.BlockSize != 16 {
		t.Errorf("BlockSize = %d; an unreadable value must leave the default", p.BlockSize)
	}
	if p.Complete() {
		t.Fatalf("Unresolved = %v, want block_size: a ConfigMap reference is not "+
			"a value this package can read, and must not be treated as one", p.Unresolved)
	}
}

func TestParserHandlesTheDollarEscapeAndBareDollars(t *testing.T) {
	// $$ is Kubernetes' escape for a reference it must not expand, and the
	// sequence a manifest uses when it wants the shell to see one dollar.
	// Neither it nor a lone trailing dollar is an unresolved reference.
	p := parseShell("vllm serve m --dtype a$$b --quantization 100%", nil)
	if p.WeightDtype != "a$b" {
		t.Errorf("WeightDtype = %q, want %q: $$ is one literal dollar", p.WeightDtype, "a$b")
	}
	if !p.Complete() {
		t.Errorf("Unresolved = %v, want empty: neither value is a reference", p.Unresolved)
	}
}

func TestParserReadsTheNegativeBooleanForms(t *testing.T) {
	// vLLM renders these with argparse's BooleanOptionalAction, so --no-<flag>
	// is how an operator turns one off, and neither negative form was mapped:
	// both fell through the same path as a genuinely unknown flag.
	//
	// ON V0, where the flag has an effect. ParseVLLMArgs forces
	// ChunkedPrefillEnabled true for V1 whatever the args said -- that is
	// pre-existing and reflects the engine (V1 does not run unchunked), so the
	// negative form is correctly inert there and this test does not pretend
	// otherwise. V0 is selected the way a deployment selects it, with
	// VLLM_USE_V1=0.
	v0 := map[string]string{"VLLM_USE_V1": "0"}

	on := parseShell("vllm serve m --max-model-len 32768 --enable-chunked-prefill", v0)
	if !on.ChunkedPrefillEnabled {
		t.Fatal("the fixture is wrong: --enable-chunked-prefill must enable it on V0")
	}
	if on.EffectiveMaxBatchedTokens != 2048 {
		t.Fatalf("baseline EffectiveMaxBatchedTokens = %d, want the 2048 V0 chunked default",
			on.EffectiveMaxBatchedTokens)
	}

	off := parseShell("vllm serve m --max-model-len 32768 --no-enable-chunked-prefill", v0)
	if off.ChunkedPrefillEnabled {
		t.Error("--no-enable-chunked-prefill left chunked prefill enabled on V0")
	}
	if off.EffectiveMaxBatchedTokens != 32768 {
		t.Errorf("EffectiveMaxBatchedTokens = %d, want 32768: unchunked prefill "+
			"budgets a whole sequence", off.EffectiveMaxBatchedTokens)
	}
	if on.Fingerprint() == off.Fingerprint() {
		t.Error("chunked and unchunked prefill hash the same, so one could lend " +
			"the other its latency line -- and they have different per-step budgets")
	}

	// --no-enforce-eager has no such override, so it works on either engine.
	eager := parseShell("vllm serve m --enforce-eager --no-enforce-eager", nil)
	if eager.EnforceEager {
		t.Error("--no-enforce-eager did not win as the later flag")
	}
	stillEager := parseShell("vllm serve m --no-enforce-eager --enforce-eager", nil)
	if !stillEager.EnforceEager {
		t.Error("the later --enforce-eager did not win: argument order must decide")
	}
}

// THE SAFETY PROPERTY, and the reason the set is hashed rather than only
// logged: an incomplete read must never be mistaken for a complete one, and
// must never license sharing.
func TestAnIncompleteReadCannotPassForAMeasuredOne(t *testing.T) {
	// Same flags; one deployment can be read and one cannot.
	read := parseShell("vllm serve m --block-size 16", nil)
	unread := parseShell("vllm serve m --block-size $NOPE", nil)

	if read.BlockSize != unread.BlockSize {
		t.Fatalf("the fixture is wrong: both must end up at BlockSize 16 "+
			"(got %d and %d), or this test is not about the unresolved set",
			read.BlockSize, unread.BlockSize)
	}
	if read.Fingerprint() == unread.Fingerprint() {
		t.Error("a configuration whose block size could not be read hashes the " +
			"same as one genuinely running the default: the digest asserts an " +
			"equality it never verified")
	}

	// And hashing alone is NOT enough, which is why Complete() exists: two
	// deployments with the SAME unreadable flag agree on the digest while
	// their real values may differ.
	otherUnread := parseShell("vllm serve m --block-size $ALSO_NOPE", nil)
	if unread.Fingerprint() != otherUnread.Fingerprint() {
		t.Log("note: two unreadable configs hash differently here only because " +
			"the recorded KEY is the same; that is fine either way")
	}
	if unread.Complete() || otherUnread.Complete() {
		t.Error("Complete() must be false for both, so neither may lend the other a line")
	}
}

func TestFindCompatibleRefusesAnIncompleteRead(t *testing.T) {
	// The reuse decision is the one that must not act on an absence of
	// evidence: both records agree on every compared field, and both agree
	// only because neither block size could be read.
	unread := parseShell("vllm serve m --block-size $NOPE", nil)
	if unread.Complete() {
		t.Fatal("the fixture must be an incomplete read, or this proves nothing")
	}

	s := NewStore()
	s.Update("ns", "m", "donor", Record{
		AcceleratorName:   "H200",
		GpuCount:          1,
		EffectiveCapacity: 900_000,
		EngineParams:      &unread,
		LearnedFrom:       LearnedFromLive,
	})

	if got := s.FindCompatible("m", "H200", 1, &unread); got != nil {
		t.Error("FindCompatible reused a record whose engine configuration could " +
			"not be read, on the strength of two identical defaults")
	}

	// The same donor, fully read, IS reusable -- or the gate above is just
	// breaking the feature.
	complete := parseShell("vllm serve m --block-size 128", nil)
	if !complete.Complete() {
		t.Fatalf("the control fixture is not complete: %v", complete.Unresolved)
	}
	s2 := NewStore()
	s2.Update("ns", "m", "donor", Record{
		AcceleratorName:   "H200",
		GpuCount:          1,
		EffectiveCapacity: 900_000,
		EngineParams:      &complete,
		LearnedFrom:       LearnedFromLive,
	})
	if got := s2.FindCompatible("m", "H200", 1, &complete); got == nil {
		t.Error("a fully-read configuration must still find its compatible record")
	}
}

func TestSGLangParserResolvesReferencesToo(t *testing.T) {
	// The resolution lives in the shared parse loop, so SGLang gets it. Pinned
	// because the two parsers have drifted before.
	d := shellDeployment(
		"python3 -m sglang.launch_server --model-path m "+
			"--mem-fraction-static $SG_MEM --page-size $SG_PAGE",
		map[string]string{"SG_MEM": "0.8", "SG_PAGE": "64"})
	d.Spec.Template.Spec.Containers[0].Name = "sglang"
	p := ParseEngineArgs(inferenceengine.EngineSGLang, scaletarget.NewDeploymentAccessor(d))

	if p.GpuMemoryUtilization != 0.8 {
		t.Errorf("GpuMemoryUtilization = %v, want 0.8", p.GpuMemoryUtilization)
	}
	if p.BlockSize != 64 {
		t.Errorf("BlockSize = %d, want 64 from $SG_PAGE", p.BlockSize)
	}
	if !p.Complete() {
		t.Errorf("Unresolved = %v, want empty", p.Unresolved)
	}

	bad := shellDeployment(
		"python3 -m sglang.launch_server --model-path m --page-size $SG_PAGE", nil)
	bad.Spec.Template.Spec.Containers[0].Name = "sglang"
	q := ParseEngineArgs(inferenceengine.EngineSGLang, scaletarget.NewDeploymentAccessor(bad))
	if q.Complete() {
		t.Error("an unresolvable SGLang page size was not recorded")
	}
}

func TestUnresolvedIsSortedAndDeduplicated(t *testing.T) {
	// It is hashed, so an unstable order would give one configuration two
	// digests. Two containers both failing on the same flag must not record it
	// twice either.
	d := shellDeployment("vllm serve m --max-model-len $A --block-size $B", nil)
	d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers,
		corev1.Container{
			Name:    "vllm-again",
			Command: []string{"/bin/sh", "-c", "vllm serve m --block-size $B"},
		})
	p := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(d))

	want := []string{"block_size", "max_model_len"}
	if len(p.Unresolved) != len(want) {
		t.Fatalf("Unresolved = %v, want exactly %v", p.Unresolved, want)
	}
	for i := range want {
		if p.Unresolved[i] != want[i] {
			t.Fatalf("Unresolved = %v, want %v (sorted, deduplicated)", p.Unresolved, want)
		}
	}
}

func TestUnresolvedIgnoresFlagsTheParserDoesNotMap(t *testing.T) {
	// An unresolvable --model or --served-model-name says nothing about
	// capacity or latency. Recording it would make every deployment incomplete
	// and switch off sharing everywhere for no gain.
	p := parseShell("vllm serve $MODEL_NAME --served-model-name $ALIAS "+
		"--download-dir $CACHE --block-size 128", nil)
	if !p.Complete() {
		t.Errorf("Unresolved = %v, want empty: none of those flags is hashed or compared",
			p.Unresolved)
	}
	if p.BlockSize != 128 {
		t.Errorf("BlockSize = %d, want 128", p.BlockSize)
	}
}
