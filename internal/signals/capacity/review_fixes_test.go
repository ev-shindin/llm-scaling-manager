package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Controls for an adversarial review of the parser work. Every case here was a
// defect the review proved by execution, so each test is written against the
// measured wrong answer rather than against the intent.

// A string-valued flag had no way to fail, so several inputs were taken as the
// engine's setting with the configuration reported COMPLETE.
//
// The cases divide by WHERE they are caught, and the table below mixes them on
// purpose because the observable is the same: usableWord rejects the empty
// ones, and resolveRefs rejects "a value the kubelet would still expand" --
// `$(OTHER)` is expanded BETWEEN env vars, so the lookup succeeds and the
// result is still a reference. WeightDtype and Quantization are the two fields
// EngineParams' own comment calls out as dominating inter-token latency, which
// is why a false equality on them is the worst kind.
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
	if ok.WeightDtype != testWeightDtype || !ok.Complete() {
		t.Fatalf("a usable dtype became %q (Unresolved=%v): the guard is too strict",
			ok.WeightDtype, ok.Unresolved)
	}
}

// NO SHELL OPERATOR IS INTERPRETED, and this is the control for the three ways
// honouring `:-` fabricated a value and reported it verified.
//
// A revision supported `${VAR:-default}` so llmdbench fleets would not be
// permanently incomplete. A review measured all three holes it opened, and
// each is the same defect this file exists to prevent, reached through a
// convenience -- so the support is gone and an unresolvable reference stays
// unresolved. The fleet then learns its own line instead of borrowing one,
// which is what it did before any of this work.
func TestNoShellOperatorIsInterpreted(t *testing.T) {
	// Every one of these resolved at the revision under test, with
	// Complete() == true.
	for _, tc := range []struct {
		name string
		body string
		env  map[string]string
	}{
		{"a default for an unset name", "${MML:-16384}", nil},
		{"a default when the name is set", "${MML:-16384}",
			map[string]string{"MML": "32768"}},
		{"a bare-hyphen default", "${MML-16384}", nil},
		// The fallback text was written out WITHOUT being resolved, so this
		// yielded the literal "$B" as a value.
		{"a default that is itself a reference", "${MML:-$B}",
			map[string]string{"B": "4096"}},
		// varNameAt stops at the first brace, so this yielded "4096}".
		{"a braced default", "${MML:-${B}}", map[string]string{"B": "4096"}},
		// The operator was found by searching the body for "-", so every
		// hyphen-bearing body was mis-split.
		{"an alternate-value operator", "${MML:+--max-model-len}",
			map[string]string{"MML": "1"}},
		{"a suffix-strip operator", "${MML%%-suf}",
			map[string]string{"MML": "16384-suf"}},
		{"a substring operator", "${MML:1-2}", map[string]string{"MML": "16384"}},
		{"an assign-default operator", "${MML:=16384}", nil},
		{"an error-if-unset operator", "${MML:?no-value}", nil},
		{"a length operator", "${#MML}", map[string]string{"MML": "16384"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := parseShell("vllm serve m --max-model-len "+tc.body, tc.env)
			if p.Complete() {
				t.Errorf("Complete() is true and MaxModelLen=%d. Guessing at shell "+
					"semantics is how a parser invents a configuration; an honest "+
					"\"could not read\" is what the set is for", p.MaxModelLen)
			}
			if p.MaxModelLen != 0 {
				t.Errorf("MaxModelLen=%d, want the default 0 -- a value was "+
					"fabricated from the operator", p.MaxModelLen)
			}

			// THE PER-CASE POSITIVE CONTROL, and it is the point. Every
			// assertion above is satisfied by the field's own default, so a
			// review made resolveRefs reject EVERYTHING containing a dollar
			// and all eleven subtests still passed -- each one proved that
			// parsing had failed, not that ITS operator was the reason. The
			// same env must still resolve a plain name, in the same subtest,
			// or the rejection is not specific to the operator under test.
			ctl := parseShell("vllm serve m --max-model-len ${MML}",
				map[string]string{"MML": "16384"})
			if ctl.MaxModelLen != 16384 || !ctl.Complete() {
				t.Fatalf("the control failed: a plain ${MML} gave %d (Unresolved=%v), "+
					"so this case proves only that resolution is broken outright",
					ctl.MaxModelLen, ctl.Unresolved)
			}
		})
	}
}

// A reference this package cannot read is recorded, whatever supplies it.
//
// THIS TEST IS DELIBERATELY NARROWER THAN THE ONE IT REPLACES. Two earlier
// tests claimed to pin envValues' valueFrom/envFrom exclusion, and a review
// proved neither could: reinstating the bug -- recording a valueFrom entry as
// its empty .Value -- left both green, because an empty substitution fails
// downstream anyway (strconv for the numeric keys, usableWord for the string
// ones), so the key is recorded either way. The exclusion is defence in depth
// with no observable difference today; claiming a test pins it was the lie.
//
// What IS provable, and what this asserts: a `${NAME:-default}` body is not an
// operator to this parser, so the whole body is looked up, found nowhere, and
// recorded -- including when a valueFrom entry of that name exists. That
// covers the regression the `:-` support introduced, which is the thing worth
// pinning.
func TestAnUnreadableReferenceIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(*corev1.Container)
	}{{
		name: "valueFrom configMapKeyRef",
		mk: func(c *corev1.Container) {
			c.Env = []corev1.EnvVar{{
				Name: "VLLM_MAX_MODEL_LEN",
				ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "engine-cfg"},
					Key:                  "max-model-len",
				}},
			}}
		},
	}, {
		name: "envFrom configMapRef",
		mk: func(c *corev1.Container) {
			c.EnvFrom = []corev1.EnvFromSource{{
				ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "engine-cfg"},
				},
			}}
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			d := shellDeployment("vllm serve m --max-model-len ${VLLM_MAX_MODEL_LEN:-16384}", nil)
			tc.mk(&d.Spec.Template.Spec.Containers[0])
			p := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(d))

			if p.MaxModelLen == 16384 {
				t.Fatal("the default was taken for a name whose real value lives in a " +
					"ConfigMap. That is a fabricated configuration reported as the " +
					"engine's, and it hands out a sharing key.")
			}
			if p.MaxModelLen != 0 {
				t.Errorf("MaxModelLen=%d, want the default 0", p.MaxModelLen)
			}
			if p.Complete() {
				t.Errorf("Complete() is true; Unresolved=%v", p.Unresolved)
			}
			// And the control, for the same reason as above: a plain name in
			// the same container must still resolve, so this proves the body
			// was rejected and not that resolution is broken.
			d2 := shellDeployment("vllm serve m --max-model-len ${PLAIN}",
				map[string]string{"PLAIN": "4096"})
			if q := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(d2)); q.MaxModelLen != 4096 {
				t.Fatalf("the control failed: a plain ${PLAIN} gave %d", q.MaxModelLen)
			}
		})
	}
}

// The cross-engine key set catches a misparsed pod whose other-engine flags
// are unresolvable REFERENCES -- and only that half, which is why the set is
// not split per engine and why the name says "references".
//
// A revision split it per engine, to stop the vLLM parser reporting an
// unresolvable SGLang flag it does not map. But the wrong parser CAN run
// (Detect takes the engine from any container's image, and ConfigContainers
// deliberately prefers the right container over the right parser), and the
// split made that case report pure defaults as VERIFIED.
func TestTheCrossEngineSetFlagsUnreadableOtherEngineReferences(t *testing.T) {
	// The vLLM parser over a pod whose flags are SGLang's, as references.
	v := parseShell("python3 -m sglang.launch_server --model-path m "+
		"--mem-fraction-static $SG_MEM --page-size $SG_PAGE", nil)
	if v.Complete() {
		t.Errorf("Complete() is true over SGLang flags the vLLM parser does not "+
			"map, so a pure-defaults read claims to be verified and gets a "+
			"sharing key. Unresolved=%v", v.Unresolved)
	}

	// THE HALF IT DOES NOT CATCH, asserted so the limit is a fact and not a
	// hope. With LITERAL values the unmapped key never reaches noteUnresolved
	// at all -- apply falls through its switch and returns true -- so the pod
	// reads as a complete vLLM configuration of pure defaults. A review
	// measured this; the residual is accepted and argued in
	// mappedValueKeys' comment. If this ever starts failing, the set grew a
	// reach it did not have and the comment needs re-deriving.
	lit := parseShell("python3 -m sglang.launch_server --model-path m "+
		"--mem-fraction-static 0.8 --page-size 64", nil)
	if !lit.Complete() {
		t.Errorf("literal other-engine flags are now recorded (Unresolved=%v). That "+
			"is an improvement, but mappedValueKeys' comment says they are not -- "+
			"update it", lit.Unresolved)
	}
	if lit.GpuMemoryUtilization != 0.9 || lit.BlockSize != 16 {
		t.Errorf("the pure-defaults read changed: gpuUtil=%v blockSize=%d",
			lit.GpuMemoryUtilization, lit.BlockSize)
	}

	// And the converse, so the set is not merely recording everything.
	ok := parseShell("vllm serve m --block-size 128 --max-model-len 16384", nil)
	if !ok.Complete() {
		t.Errorf("a fully-read vLLM configuration reported incomplete: %v", ok.Unresolved)
	}
}

// Both directions of the key-set contract, over the ONE set. A key listed but
// unhandled over-reports; a key handled by EITHER apply function and missing
// here under-reports, which is the silent default the mechanism exists to
// catch.
//
// KNOWN LIMIT, measured rather than assumed: the `handled` list below is
// hand-maintained, not derived from the switches, so a key added to an apply
// function AND to neither list is invisible to this test -- a review proved it
// by adding one. Deriving the case labels needs AST inspection, which is more
// machinery than the risk warrants; the size equality at the end is what makes
// a one-sided drift fail. A second limit: the empty-value probe requires only
// that ONE apply function reject the key, so one engine's function gaining a
// permissive case for the OTHER engine's key also slips through.
func TestTheValueKeySetMatchesBothApplyFunctions(t *testing.T) {
	for key := range mappedValueKeys {
		// A value no numeric or string key can accept. One of the two apply
		// functions must own the key and report failure on it.
		pv := defaultEngineParams()
		ps := defaultSGLangEngineParams()
		if applyParam(key, "", &pv) && applySGLangParam(key, "", &ps) {
			t.Errorf("%q is listed as a value key but NEITHER apply function "+
				"rejected an empty value, so an unreadable one is never recorded", key)
		}
	}

	handled := []string{
		// vLLM
		"gpu_memory_utilization", "block_size", "kv_cache_dtype", "dtype",
		"quantization", "tensor_parallel_size", "num_gpu_blocks_override",
		"max_num_batched_tokens", "max_num_seq", "max_num_seqs", "max_model_len",
		// SGLang
		"mem_fraction_static", "page_size", "tp_size", "tp",
		"max_running_requests", "max_total_tokens", "context_length",
		"max_prefill_tokens", "chunked_prefill_size",
	}
	for _, k := range handled {
		if _, ok := mappedValueKeys[k]; !ok {
			t.Errorf("an apply function handles %q but it is not in mappedValueKeys, "+
				"so an unresolvable value for it is silently dropped", k)
		}
	}
	if len(mappedValueKeys) != len(handled) {
		t.Errorf("mappedValueKeys has %d keys, the handled list %d: one of them has "+
			"drifted", len(mappedValueKeys), len(handled))
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

	ab := two(eng("a", testWeightDtype), eng("b", "float16"))
	ba := two(eng("b", "float16"), eng("a", testWeightDtype))

	// This is the CURRENT behaviour, asserted so a future change to it is a
	// deliberate one: two containers that both launch an engine are both
	// selected, and the later wins.
	if ab.WeightDtype != "float16" || ba.WeightDtype != testWeightDtype {
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

// containsVarRef had no direct test. It is the guard that catches a value which
// resolved successfully and is STILL a reference -- the kubelet expands
// `$(OTHER)` between env vars, so the lookup succeeds and the result is
// unusable -- and it has to tell that apart from a literal dollar, because
// rejecting every `$` made `fp8$` unreadable.
//
// Exercised directly, since reaching every branch through the parser needs a
// contrived env for each.
func TestContainsVarRef(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		// References.
		{"$B", true},
		{"${B}", true},
		{"$(B)", true},
		{"a$b", true},
		{"a${b}c", true},
		{"a$(b)c", true},
		{"$$$B", true}, // an escape then a reference
		// Not references: no name follows the dollar.
		{"", false},
		{"fp8", false},
		{"$", false},
		{"a$", false},
		{"fp8$", false},
		{"fp$-8", false},
		{"fp$8", false}, // a digit cannot open a name
		{"$$", false},   // the escape, which must not read as a reference
		{"a$$b", false},
		{"a$$", false},
		{"$$$", false},
		{"${}", false},
		{"$()", false},
		{"a${", false},
		{"a$(b", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := containsVarRef(tc.in); got != tc.want {
				t.Errorf("containsVarRef(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// And the escape through the PARSER, in a substituted value rather than in the
// arg. The existing dollar tests only cover the arg's own path, so nothing
// reached containsVarRef with an escaped dollar in it.
func TestAnEscapedDollarInASubstitutedValueIsNotAReference(t *testing.T) {
	p := parseShell("vllm serve m --quantization $Q", map[string]string{"Q": "a$$b"})
	if !p.Complete() {
		t.Errorf("Unresolved=%v: an env VALUE containing $$ names no variable, so "+
			"the substitution is usable", p.Unresolved)
	}
	// Not collapsed: neither expander re-processes a value it has already
	// substituted, so what the engine receives is what the env held.
	if p.Quantization != "a$$b" {
		t.Errorf("Quantization=%q, want %q unchanged -- a substituted value is not "+
			"re-expanded", p.Quantization, "a$$b")
	}

	// The case it must still catch, for contrast.
	q := parseShell("vllm serve m --quantization $Q", map[string]string{"Q": "$(E)"})
	if q.Complete() {
		t.Errorf("Unresolved=%v: a value that is still a reference must be recorded",
			q.Unresolved)
	}
}
