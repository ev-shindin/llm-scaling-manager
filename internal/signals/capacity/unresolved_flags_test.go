package capacity

import (
	"slices"
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

// The SGLang fixture's container name and launcher module. Named because each
// recurs, not because the values matter.
const (
	sglangContainer = "sglang"
	sglangLauncher  = "sglang.launch_server"
)

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
			"VLLM_MAX_MODEL_LEN": "16384",
			"VLLM_BLOCK_SIZE":    "128",
			// NOT the parser defaults. A review neutered resolveRefs entirely
			// and these two assertions did not fire, because 0.9 and 256 are
			// also what the struct defaults to -- the same
			// coincidence-with-defaults trap this whole commit is about,
			// left in the test that proves the commit.
			"VLLM_ACCELERATOR_MEM_UTIL": "0.85",
			"VLLM_MAX_NUM_SEQ":          "512",
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
	if p.MaxNumSeqs != 512 {
		t.Errorf("MaxNumSeqs = %d, want 512 from ${VLLM_MAX_NUM_SEQ} -- 256 is the "+
			"default, so this must not be 256", p.MaxNumSeqs)
	}
	if p.TensorParallelSize != 2 {
		t.Errorf("TensorParallelSize = %d, want 2 from $(TP_SIZE)", p.TensorParallelSize)
	}
	if p.GpuMemoryUtilization != 0.85 {
		t.Errorf("GpuMemoryUtilization = %v, want 0.85 -- 0.9 is the default, so "+
			"this must not be 0.9", p.GpuMemoryUtilization)
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
		// NOT "nested references are not chased" -- resolveRefs does not
		// detect nesting as a class at all. It substitutes $OUTER with the
		// literal "$INNER" and reports success, because OUTER *was* found.
		// What then records the key is the value being unusable, and a review
		// showed the original name claimed a general parser property that only
		// held for integer flags: the same shape on --dtype was accepted as
		// the engine's real setting with Complete() true. Both are covered now
		// -- the numeric case here, the string case in
		// TestStringFlagsCanFailToResolve -- and the name says what happens.
		name: "a value that is still a reference after substitution",
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
	// $$ collapses to one literal dollar. This is KUBERNETES' field-expansion
	// rule, not the shell's -- the kubelet collapses $$ unconditionally, and a
	// review confirmed ours matches its algorithm byte for byte.
	p := parseShell("vllm serve m --dtype a$$b", nil)
	if p.WeightDtype != "a$b" {
		t.Errorf("WeightDtype = %q, want %q: $$ is one literal dollar", p.WeightDtype, "a$b")
	}
	if !p.Complete() {
		t.Errorf("Unresolved = %v, want empty: $$ is not a reference", p.Unresolved)
	}

	// THE BARE-DOLLAR HALF, which the name claimed and nothing tested. A
	// review broke the rule -- made a lone "$" count as unresolved -- and the
	// whole package stayed green, because "a$$b" consumes both dollars
	// together and never reaches the isolated-$ path at all. These do.
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"a trailing dollar", "fp8$", "fp8$"},
		{"a dollar before a non-name character", "fp$-8", "fp$-8"},
		{"a dollar before a digit", "fp$8", "fp$8"},
		{"a lone dollar", "$", "$"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := parseShell("vllm serve m --quantization "+tc.value, nil)
			if q.Quantization != tc.want {
				t.Errorf("Quantization = %q, want %q", q.Quantization, tc.want)
			}
			if !q.Complete() {
				t.Errorf("Unresolved = %v, want empty: %q names no variable, so it "+
					"is a literal and not an unreadable reference", q.Unresolved, tc.value)
			}
		})
	}
}

// A reference that was STARTED and could not be finished is not a literal.
//
// This was a real defect, found by review and confirmed by running it: a
// malformed reference took the same path as a lone "$" -- "not a reference,
// keep the byte, stay resolved" -- so `--dtype=${FOO` set WeightDtype to the
// literal "${FOO" with Unresolved empty and Complete() TRUE. A probe printed
// exactly that. The digest then asserted an equality it had never verified,
// which is the false-equality direction this whole mechanism exists to stop.
//
// Numeric flags happened to be safe, because ParseInt fails on the garbage
// afterwards and that failure is recorded. The string flags had nothing:
// usableWord accepts any non-empty string. So the cases below are deliberately
// string-valued, and the numeric one is included to show it is not the thing
// that was protecting them.
//
// The CONTROLS for this live in TestParserHandlesTheDollarEscapeAndBareDollars,
// which pins the other half -- a lone, trailing or non-name-leading "$" stays
// a literal and must NOT be called unresolved. Both halves have to hold: a fix
// that simply marked every "$" unresolved would pass this test and fail that
// one, which is how the distinction stays honest.
func TestAMalformedReferenceIsNotALiteral(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"an unterminated brace", "${FOO"},
		{"an unterminated paren", "$(FOO"},
		{"an empty brace body", "${}"},
		{"an empty paren body", "$()"},
		{"a malformed reference after real text", "fp8${FOO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := parseShell("vllm serve m --dtype "+tc.value, nil)
			if p.Complete() {
				t.Errorf("Unresolved = %v, want dtype recorded: %q started a reference "+
					"and could not finish it, so the field is a default standing in "+
					"for something unknown -- not a measurement", p.Unresolved, tc.value)
			}
			if !slices.Contains(p.Unresolved, keyDtype) {
				t.Errorf("Unresolved = %v, want it to contain %q", p.Unresolved, keyDtype)
			}
			if p.WeightDtype == tc.value {
				t.Errorf("WeightDtype = %q -- the malformed reference was stored as a "+
					"verified value, which is what this test exists to prevent",
					p.WeightDtype)
			}
		})
	}

	// THE POSITIVE CONTROL for the mechanism, not for the bug: a WELL-FORMED
	// reference that nothing resolves was already recorded correctly. If this
	// ever fails, the cases above are passing for an unrelated reason.
	t.Run("a well-formed unresolvable reference was already recorded", func(t *testing.T) {
		p := parseShell("vllm serve m --dtype ${FOO}", nil)
		if p.Complete() || !slices.Contains(p.Unresolved, keyDtype) {
			t.Errorf("Unresolved = %v, want dtype: this is the pre-existing behaviour "+
				"the malformed cases above were missing", p.Unresolved)
		}
	})

	// And the numeric flag, which was only ever saved by ParseInt rejecting
	// the garbage. It must still be recorded -- now for the reference reason
	// rather than the number reason.
	t.Run("a numeric flag with a malformed reference", func(t *testing.T) {
		p := parseShell("vllm serve m --block-size ${BS", nil)
		if p.Complete() || !slices.Contains(p.Unresolved, "block_size") {
			t.Errorf("Unresolved = %v, want block_size", p.Unresolved)
		}
		if p.BlockSize != 16 {
			t.Errorf("BlockSize = %d, want the default 16", p.BlockSize)
		}
	})
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

// FindCompatible deliberately does NOT refuse an incomplete read, and this
// pins that -- because an earlier version of this very test asserted the
// opposite and the gate it demanded was a regression.
//
// The result is used by estimateZeroReplicaCapacity as a MAX CLAMP on a
// derived estimate (`if compatible.EffectiveCapacity < bounded`). Refusing the
// record does not substitute something smaller; it REMOVES THE CEILING.
// Measured by a review: 5,000 became 153,600, a 30x over-estimate of
// per-replica capacity -- and an over-stated capacity under-orders replicas,
// which this project has on record as breaking TTFT irrecoverably.
//
// The rule that came out of it: an unreadable flag withholds the NEW thing it
// would authorise, and never changes a path that already existed.
// IsCapacityCompatible was always a heuristic over parsed fields, and an
// unresolved value defaulted before this work exactly as it does now.
func TestFindCompatibleDoesNotGateOnCompleteness(t *testing.T) {
	unread := parseShell("vllm serve m --block-size $NOPE", nil)
	if unread.Complete() {
		t.Fatal("the fixture must be an incomplete read, or this proves nothing")
	}

	s := NewStore()
	s.Update("ns", "m", "donor", Record{
		AcceleratorName:   "H200",
		GpuCount:          1,
		EffectiveCapacity: 5000,
		EngineParams:      &unread,
		LearnedFrom:       LearnedFromLive,
	})

	got := s.FindCompatible("m", "H200", 1, &unread)
	if got == nil {
		t.Fatal("FindCompatible refused an incomplete read. At its only caller the " +
			"record is a CEILING on a derived estimate, so refusing it removes the " +
			"ceiling and over-states per-replica capacity -- the direction that " +
			"under-orders replicas and breaks TTFT irrecoverably.")
	}
	if got.EffectiveCapacity != 5000 {
		t.Errorf("EffectiveCapacity = %d, want the donor's 5000", got.EffectiveCapacity)
	}
}

func TestSGLangParserResolvesReferencesToo(t *testing.T) {
	// The resolution lives in the shared parse loop, so SGLang gets it. Pinned
	// because the two parsers have drifted before.
	d := shellDeployment(
		"python3 -m "+sglangLauncher+" --model-path m "+
			"--mem-fraction-static $SG_MEM --page-size $SG_PAGE",
		map[string]string{"SG_MEM": "0.8", "SG_PAGE": "64"})
	d.Spec.Template.Spec.Containers[0].Name = sglangContainer
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
		"python3 -m "+sglangLauncher+" --model-path m --page-size $SG_PAGE", nil)
	bad.Spec.Template.Spec.Containers[0].Name = sglangContainer
	q := ParseEngineArgs(inferenceengine.EngineSGLang, scaletarget.NewDeploymentAccessor(bad))
	if q.Complete() {
		t.Error("an unresolvable SGLang page size was not recorded")
	}
}

// The SGLang parser's container selection, which had NO test.
//
// A review reverted ParseSGLangArgs to walk every container -- reintroducing,
// for SGLang alone, the exact "last one wins" bug the commit message says it
// fixes -- and the whole tree stayed green. The vLLM side was pinned from the
// parser, the SGLang side was not, which is the same "a well-identified helper
// nobody calls" shape the wiring tests exist to catch, half-applied.
func TestTheSGLangParserAlsoReadsOnlyTheEngineContainer(t *testing.T) {
	engine := corev1.Container{
		Name:    sglangContainer,
		Image:   "lmsysorg/sglang:v0.4.3",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{"python3 -m " + sglangLauncher + " --model-path m " +
			"--page-size $SG_PAGE --context-length $SG_CTX --dtype bfloat16"},
		Env: []corev1.EnvVar{
			{Name: "SG_PAGE", Value: "64"},
			{Name: "SG_CTX", Value: "8192"},
		},
	}
	sidecar := corev1.Container{
		Name:    "routing-proxy",
		Image:   "ghcr.io/llm-d/llm-d-router-disagg-sidecar:v0.9.0",
		Command: []string{"/app/proxy"},
		// SGLang's own flag names, different values.
		Args: []string{"--page-size", "1", "--context-length", "512",
			"--dtype", "float16"},
	}

	d := shellDeployment("unused", nil)
	d.Spec.Template.Spec.Containers = []corev1.Container{engine, sidecar}
	p := ParseEngineArgs(inferenceengine.EngineSGLang, scaletarget.NewDeploymentAccessor(d))

	if p.BlockSize != 64 {
		t.Errorf("BlockSize = %d, want 64 from the engine -- 1 is the sidecar's",
			p.BlockSize)
	}
	if p.MaxModelLen != 8192 {
		t.Errorf("MaxModelLen = %d, want 8192 from the engine -- 512 is the sidecar's",
			p.MaxModelLen)
	}
	if p.WeightDtype != testWeightDtype {
		t.Errorf("WeightDtype = %q, want bfloat16 -- float16 is the sidecar's",
			p.WeightDtype)
	}
	if !p.Complete() {
		t.Errorf("Unresolved = %v, want empty: the engine's own env resolves both "+
			"its references", p.Unresolved)
	}
	if p.Engine != inferenceengine.EngineSGLang {
		t.Errorf("Engine = %q, want sglang", p.Engine)
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

// TestTheParserReadsOnlyTheEngineContainer is the wiring, not the rule.
//
// ConfigContainers has its own specs in internal/inferenceengine, and all of
// them passed while the parsers still walked every container -- a
// well-identified helper nobody called. This drives the parser and reads the
// result, which is the only thing that shows the selection is in use.
//
// The fixture is the cluster's decode pod with the llm-d routing sidecar as a
// regular container (it runs as one in other llm-d layouts) and given flag
// names it shares with the engine. Sorted last by nothing in particular: the
// old loop simply let whichever came later win.
func TestTheParserReadsOnlyTheEngineContainer(t *testing.T) {
	engine := corev1.Container{
		Name:    "vllm",
		Image:   "docker.io/vllm/vllm-openai:v0.26.0",
		Command: []string{"/bin/bash", "-c"},
		Args: []string{". /shared-config/llmdbench_env.sh && vllm serve /model-cache/m " +
			"--block-size $VLLM_BLOCK_SIZE --max-model-len $VLLM_MAX_MODEL_LEN " +
			"--dtype bfloat16"},
		Env: []corev1.EnvVar{
			{Name: "VLLM_BLOCK_SIZE", Value: "128"},
			{Name: "VLLM_MAX_MODEL_LEN", Value: "16384"},
		},
	}
	sidecar := corev1.Container{
		Name:    "routing-proxy",
		Image:   "ghcr.io/llm-d/llm-d-router-disagg-sidecar:v0.9.0",
		Command: []string{"/app/proxy"},
		// The same flag NAMES, different values. Nothing stops a sidecar
		// having them; what must stop is them being read as the engine's.
		Args: []string{"--block-size", "8", "--max-model-len", "512", "--dtype", "float16"},
	}

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{engine, sidecar}},
			},
		},
	}
	p := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(d))

	if p.BlockSize != 128 {
		t.Errorf("BlockSize = %d, want 128 from the engine -- 8 is the sidecar's",
			p.BlockSize)
	}
	if p.MaxModelLen != 16384 {
		t.Errorf("MaxModelLen = %d, want 16384 from the engine -- 512 is the sidecar's",
			p.MaxModelLen)
	}
	if p.WeightDtype != testWeightDtype {
		t.Errorf("WeightDtype = %q, want bfloat16 from the engine -- float16 is the sidecar's",
			p.WeightDtype)
	}
	if !p.Complete() {
		t.Errorf("Unresolved = %v, want empty: the engine's own env resolves both "+
			"its references", p.Unresolved)
	}

	// And the order must not matter, which is the actual defect: the old loop
	// let the last container win.
	rev := d.DeepCopy()
	rev.Spec.Template.Spec.Containers = []corev1.Container{sidecar, engine}
	q := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(rev))
	// Asserted on the parsed fields rather than on the digest: the digest
	// arrives with the sharing feature, and the property under test here --
	// that container ORDER does not change what was parsed -- is the parser's.
	if q.MaxModelLen != p.MaxModelLen || q.BlockSize != p.BlockSize ||
		q.WeightDtype != p.WeightDtype {
		t.Errorf("swapping the container order changed what was parsed: "+
			"maxModelLen %d vs %d, blockSize %d vs %d, dtype %q vs %q",
			p.MaxModelLen, q.MaxModelLen, p.BlockSize, q.BlockSize,
			p.WeightDtype, q.WeightDtype)
	}
}
