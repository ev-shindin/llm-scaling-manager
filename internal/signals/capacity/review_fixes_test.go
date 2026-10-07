package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Controls for an adversarial review of the parser work. Every case here was a
// defect the review proved by execution, so each test is written against the
// measured wrong answer rather than against the intent.

// A string-valued flag had no way to fail, so three inputs were taken as the
// engine's setting with the configuration reported COMPLETE. The last row is
// the dangerous one: envValues reads env literally, but the kubelet expands
// `$(OTHER)` references BETWEEN env vars, so a legal manifest yielded
// weight_dtype="$(E)" hashed as verified. WeightDtype and Quantization are the
// two fields EngineParams' own comment calls out as dominating inter-token
// latency.
func TestStringFlagsCanFailToResolve(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		env  map[string]string
		key  string
	}{
		{"no value token", "vllm serve m --dtype", nil, "dtype"},
		{"explicitly empty", "vllm serve m --dtype=", nil, "dtype"},
		{"an empty env value", "vllm serve m --dtype $D",
			map[string]string{"D": ""}, "dtype"},
		{"a value the kubelet would still expand", "vllm serve m --dtype $D",
			map[string]string{"D": "$(E)"}, "dtype"},
		{"the next flag mistaken for a value", "vllm serve m --dtype --enforce-eager",
			nil, "dtype"},
		{"quantization too", "vllm serve m --quantization", nil, "quantization"},
		{"and kv-cache-dtype", "vllm serve m --kv-cache-dtype=", nil, "kv_cache_dtype"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseShell(tc.cmd, tc.env)
			if p.Complete() {
				t.Fatalf("Complete() is true; Unresolved=%v. The field below it is a "+
					"DEFAULT and nothing says so", p.Unresolved)
			}
			found := false
			for _, k := range p.Unresolved {
				if k == tc.key {
					found = true
				}
			}
			if !found {
				t.Errorf("Unresolved=%v, want %q", p.Unresolved, tc.key)
			}
			// And the default must survive, as it does for the numeric keys.
			if tc.key == "dtype" && p.WeightDtype != "auto" {
				t.Errorf("WeightDtype=%q, want the default %q", p.WeightDtype, "auto")
			}
		})
	}

	// A real dtype still lands, or the guard is rejecting everything.
	ok := parseShell("vllm serve m --dtype bfloat16", nil)
	if ok.WeightDtype != "bfloat16" || !ok.Complete() {
		t.Fatalf("a usable dtype became %q (Unresolved=%v): the guard is too strict",
			ok.WeightDtype, ok.Unresolved)
	}
}

// `${VAR:-default}` is routine in llmdbench manifests. Taking the whole brace
// body as a name made every such flag unresolvable, which would have left a
// real fleet permanently incomplete -- and incompleteness switches off line
// sharing.
func TestShellDefaultExpansionsResolve(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		env  map[string]string
		want int64
		done bool
	}{
		{"unset takes the default", "vllm serve m --max-model-len ${MML:-16384}",
			nil, 16384, true},
		{"set wins over the default", "vllm serve m --max-model-len ${MML:-16384}",
			map[string]string{"MML": "32768"}, 32768, true},
		{"empty takes the default under :-", "vllm serve m --max-model-len ${MML:-16384}",
			map[string]string{"MML": ""}, 16384, true},
		{"empty does NOT take it under a bare -", "vllm serve m --max-model-len ${MML-16384}",
			map[string]string{"MML": ""}, 0, false},
		{"unset takes it under a bare -", "vllm serve m --max-model-len ${MML-16384}",
			nil, 16384, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseShell(tc.cmd, tc.env)
			if p.MaxModelLen != tc.want {
				t.Errorf("MaxModelLen=%d, want %d (Unresolved=%v)",
					p.MaxModelLen, tc.want, p.Unresolved)
			}
			if p.Complete() != tc.done {
				t.Errorf("Complete()=%v, want %v (Unresolved=%v)",
					p.Complete(), tc.done, p.Unresolved)
			}
		})
	}

	// A Kubernetes env var name may contain a hyphen, so an env that defines
	// the literal body must win over reading it as "name or else default".
	lit := parseShell("vllm serve m --dtype ${FOO-BAR}",
		map[string]string{"FOO-BAR": "bfloat16", "FOO": "float16"})
	if lit.WeightDtype != "bfloat16" {
		t.Errorf("WeightDtype=%q, want bfloat16: the literal env name must win",
			lit.WeightDtype)
	}

	// Every other shell operator stays unresolved on purpose.
	for _, body := range []string{"${MML:4}", "${#MML}", "${MML%x}", "${MML/a/b}"} {
		p := parseShell("vllm serve m --max-model-len "+body,
			map[string]string{"MML": "16384"})
		if p.Complete() {
			t.Errorf("%s was resolved; guessing at substring/length/strip semantics "+
				"is how a parser invents a configuration", body)
		}
	}
}

// A combined key set recorded keys the running parser does not map, reporting
// a FALSE incompleteness over a configuration that was read perfectly well.
func TestEachParserRecordsOnlyItsOwnKeys(t *testing.T) {
	v := parseShell("vllm serve m --mem-fraction-static $NOPE --max-total-tokens $NOPE "+
		"--block-size 128", nil)
	if !v.Complete() {
		t.Errorf("vLLM: Unresolved=%v, want empty -- applyParam maps neither SGLang flag",
			v.Unresolved)
	}
	if v.BlockSize != 128 {
		t.Errorf("BlockSize=%d, want 128", v.BlockSize)
	}

	d := shellDeployment("python3 -m "+sglangLauncher+" --model-path m "+
		"--max-model-len $NOPE --num-gpu-blocks-override $NOPE --page-size 64", nil)
	d.Spec.Template.Spec.Containers[0].Name = sglangContainer
	s := ParseEngineArgs(inferenceengine.EngineSGLang, scaletarget.NewDeploymentAccessor(d))
	if !s.Complete() {
		t.Errorf("SGLang: Unresolved=%v, want empty -- applySGLangParam maps neither "+
			"vLLM flag", s.Unresolved)
	}
	if s.BlockSize != 64 {
		t.Errorf("BlockSize=%d, want 64 from --page-size", s.BlockSize)
	}
}

// Both directions of the key-set contract. A key listed but unhandled
// over-reports; a key handled but unlisted under-reports, which is the silent
// default the whole mechanism exists to catch.
func TestValueKeySetsMatchTheApplyFunctions(t *testing.T) {
	probe := func(apply func(string, string, *EngineParams) bool,
		eng inferenceengine.Engine, keys map[string]struct{}, label string) {
		for key := range keys {
			p := defaultEngineParams()
			p.Engine = eng
			// A value no numeric or string key can accept. If the apply
			// function handles this key at all it must report failure.
			if apply(key, "", &p) {
				t.Errorf("%s: %q is listed as a value key but apply() accepted an "+
					"empty value, so an unreadable one would never be recorded", label, key)
			}
		}
	}
	probe(applyParam, inferenceengine.EngineVLLM, vllmValueKeys, "vllm")
	probe(applySGLangParam, inferenceengine.EngineSGLang, sglangValueKeys, "sglang")

	// And the converse: a key the apply function handles must be listed, or a
	// failure on it is dropped. Walk the known flag names per engine.
	vllmHandled := []string{"gpu_memory_utilization", "block_size", "kv_cache_dtype",
		"dtype", "quantization", "tensor_parallel_size", "num_gpu_blocks_override",
		"max_num_batched_tokens", "max_num_seq", "max_num_seqs", "max_model_len"}
	for _, k := range vllmHandled {
		if _, ok := vllmValueKeys[k]; !ok {
			t.Errorf("vllm: applyParam handles %q but it is not in vllmValueKeys, so "+
				"an unresolvable value for it is silently dropped", k)
		}
	}
	sgHandled := []string{"mem_fraction_static", "page_size", "dtype", "quantization",
		"kv_cache_dtype", "tp_size", "tensor_parallel_size", "tp",
		"max_running_requests", "max_total_tokens", "context_length",
		"max_prefill_tokens", "chunked_prefill_size"}
	for _, k := range sgHandled {
		if _, ok := sglangValueKeys[k]; !ok {
			t.Errorf("sglang: applySGLangParam handles %q but it is not in "+
				"sglangValueKeys", k)
		}
	}
}

// THE ORDER-INDEPENDENCE PROPERTY, pinned where it is NOT vacuous.
//
// The earlier assertion compared two digests over a pod whose selection
// reduces to ONE container, so swapping them was a no-op and the test could
// not fail. Two shapes where the selection returns several containers are
// genuinely order-dependent, and the last one still wins. That is a real
// residual limitation, recorded here as the shape it takes rather than left
// for the next reader to rediscover.
func TestSelectionIsStillOrderDependentWhenSeveralContainersMatch(t *testing.T) {
	two := func(a, b corev1.Container) EngineParams {
		d := shellDeployment("unused", nil)
		d.Spec.Template.Spec.Containers = []corev1.Container{a, b}
		return ParseVLLMArgs(scaletarget.NewDeploymentAccessor(d))
	}
	eng := func(name, dtype string) corev1.Container {
		return corev1.Container{
			Name:    name,
			Image:   "quay.io/acme/engine:1",
			Command: []string{"/bin/sh", "-c", "vllm serve m --dtype " + dtype},
		}
	}

	ab := two(eng("a", "bfloat16"), eng("b", "float16"))
	ba := two(eng("b", "float16"), eng("a", "bfloat16"))

	// This is the CURRENT behaviour, asserted so a future change to it is a
	// deliberate one: two containers that both launch an engine are both
	// selected, and the later wins.
	if ab.WeightDtype != "float16" || ba.WeightDtype != "bfloat16" {
		t.Fatalf("expected last-wins over two launching containers, got %q then %q",
			ab.WeightDtype, ba.WeightDtype)
	}
	if ab.Fingerprint() == ba.Fingerprint() {
		t.Error("the two orders hashed alike, so this test no longer describes the " +
			"behaviour it was written for -- re-derive it")
	}
	t.Log("two engine containers in one pod is not a shape llm-d produces, and " +
		"neither parser can describe it; the limitation is recorded, not fixed")
}
