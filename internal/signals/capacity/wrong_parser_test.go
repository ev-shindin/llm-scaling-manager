package capacity

import (
	"slices"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// THE WRONG PARSER OVER THE RIGHT CONTAINER, which the parser used to report
// as a complete configuration of defaults.
//
// Detect reads the engine from a container image, and a sidecar's image can
// win -- a `sglang-router` beside a vLLM engine makes Detect say SGLang, and
// ConfigContainers then correctly selects the container that starts an engine.
// So the SGLang parser can be handed vLLM flags, or the reverse. When those
// flags carry LITERAL values nothing fails to resolve, and the measured result
// was:
//
//	the vLLM parser over `--mem-fraction-static 0.8 --page-size 64`
//	  -> Complete() == true, every field at a vLLM default
//
// A configuration of pure defaults, presented as verified, and the previous
// note in this package said it could not be closed from inside the parser.
// It can, and the fix was a return type rather than a new mechanism: applyParam
// returned a bool in which "recognised, unusable" and "never heard of this
// flag" shared one value, so an unmapped key fell through as a success and
// mappedValueKeys was never consulted. With applyResult, parseArgsWith can ask
// whether an unknown key is one the OTHER engine maps -- which is exactly the
// wrong-parser signal.
func TestTheWrongParserDoesNotReportACompleteConfiguration(t *testing.T) {
	t.Run("the vLLM parser over SGLang flags with literal values", func(t *testing.T) {
		// Literal values on purpose: a reference would be recorded by
		// resolveRefs and the test would pass without the fix.
		p := parseShell("vllm serve m --mem-fraction-static 0.8 --page-size 64", nil)

		if p.Complete() {
			t.Errorf("Complete() = true with Unresolved %v: the vLLM parser read two "+
				"SGLang flags, set nothing from either, and reported a verified "+
				"configuration of its own defaults", p.Unresolved)
		}
		for _, want := range []string{"mem_fraction_static", "page_size"} {
			if !slices.Contains(p.Unresolved, want) {
				t.Errorf("Unresolved = %v, want it to contain %q", p.Unresolved, want)
			}
		}
		// And the fields really are untouched defaults, which is what made the
		// false "complete" dangerous rather than merely wrong.
		if p.GpuMemoryUtilization != 0.9 || p.BlockSize != 16 {
			t.Errorf("gpuMemoryUtilization=%v blockSize=%d: the fixture is supposed to "+
				"leave both at their defaults, or it is not testing the case",
				p.GpuMemoryUtilization, p.BlockSize)
		}
	})

	t.Run("and the SGLang parser over vLLM flags", func(t *testing.T) {
		// The mirror, because the two parsers have separate apply functions
		// and a fix to one proves nothing about the other.
		d := shellDeployment("python3 -m sglang.launch_server --block-size 128 --gpu-memory-utilization 0.85", nil)
		d.Spec.Template.Spec.Containers[0].Name = sglangContainer
		p := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(d))

		if p.Complete() {
			t.Errorf("Complete() = true with Unresolved %v: the SGLang parser read two "+
				"vLLM flags and reported a verified configuration", p.Unresolved)
		}
		for _, want := range []string{"block_size", "gpu_memory_utilization"} {
			if !slices.Contains(p.Unresolved, want) {
				t.Errorf("Unresolved = %v, want it to contain %q", p.Unresolved, want)
			}
		}
	})

	// THE LIMIT, recorded rather than left to be discovered. The signal is
	// "a flag belonging to the OTHER engine", so it cannot fire when the
	// misread container's flags fall entirely inside the subset both appliers
	// map: dtype, quantization, kv_cache_dtype, tensor_parallel_size. Every
	// flag then reads fine, nothing is unknown, and the parse reports complete
	// while the UNSHARED fields hold the wrong engine's defaults.
	//
	// A reviewer measured this and it is real. Asserting it keeps the comment
	// at mappedValueKeys honest -- an earlier version of that comment claimed
	// the literal-value case was closed in general, when it is closed only
	// where the two flag sets differ.
	t.Run("but NOT when every flag is one both engines map", func(t *testing.T) {
		d := shellDeployment("vllm serve m --dtype bfloat16 --quantization fp8 "+
			"--kv-cache-dtype fp8 --tensor-parallel-size 2", nil)
		d.Spec.Template.Spec.Containers[0].Name = sglangContainer
		p := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(d))

		if !p.Complete() {
			t.Errorf("Unresolved = %v, want empty: every flag here is mapped by BOTH "+
				"appliers, so nothing is unknown and this limit no longer holds -- "+
				"if that is deliberate, the WHAT REMAINS note at mappedValueKeys "+
				"needs re-deriving", p.Unresolved)
		}
		// And the cost of that silence: a field neither flag set, holding the
		// wrong engine's default.
		if p.BlockSize != 1 {
			t.Errorf("BlockSize = %d, want SGLang's page-size default of 1 -- the "+
				"point of this case is that the misread parser's default stands "+
				"where vLLM's 16 belongs, with no signal", p.BlockSize)
		}
	})

	// THE CONTROL, and the reason the fix is not simply "record every unknown
	// flag". A deployment carries plenty of flags that have nothing to do with
	// capacity, and recording those would make every configuration incomplete
	// and the whole mechanism useless.
	t.Run("a flag neither parser maps is still ignored", func(t *testing.T) {
		p := parseShell("vllm serve m --port 8000 --served-model-name foo "+
			"--trust-remote-code --block-size 128", nil)
		if !p.Complete() {
			t.Errorf("Unresolved = %v, want empty: --port, --served-model-name and "+
				"--trust-remote-code are in neither engine's mapped set, so they say "+
				"nothing about capacity and must not make this configuration "+
				"incomplete", p.Unresolved)
		}
		if p.BlockSize != 128 {
			t.Errorf("BlockSize = %d, want 128: the one flag that IS mapped must still "+
				"be read", p.BlockSize)
		}
	})
}
