package inferenceengine

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Detect takes the engine from ANY container's image, and nothing pinned that.
//
// A revision reordered it to try launch commands first. It fixed one shape and
// broke the mirror, and a review reproduced both; the reorder is reverted, so
// detection is once again a zero-delta from main. But the whole existing suite
// stayed green under the reorder -- every fixture in engine_test.go and
// config_containers_test.go is SINGLE-CONTAINER, and the regression only shows
// with several. So the restored property had no test at all, which is how it
// would be reverted again by someone reading the reorder as an improvement.
//
// These are multi-container on purpose.
func TestDetectPrefersAnImageOverAnyLaunchCommand(t *testing.T) {
	engineByImage := corev1.Container{
		Name:  "sglang",
		Image: "lmsysorg/sglang:v0.4.3",
		// No command: the launcher is the image's entrypoint, which is a
		// legitimate way to ship an engine and the reason the image signal
		// exists at all.
	}
	// A sidecar that merely MENTIONS the other engine. The whole joined
	// command string is searched, so a bench image, a warmup probe or an echo
	// in a wrapper script all qualify -- which is what made the reorder flip a
	// genuine SGLang pod to vLLM.
	benchSidecar := corev1.Container{
		Name:    "bench",
		Image:   "ghcr.io/llm-d/llm-d-benchmark:v0.7.8",
		Command: []string{"/bin/sh", "-c", "echo starting; vllm serve --dry-run"},
	}

	t.Run("an sglang image beats a sidecar's vllm launch command", func(t *testing.T) {
		got := detectFromPodTemplate(tmplOf(engineByImage, benchSidecar))
		if got != EngineSGLang {
			t.Fatalf("Detect = %q, want sglang. A sidecar that mentions `vllm serve` "+
				"must not decide the pod's engine -- Detect also builds the "+
				"collector's queries, so a flip scrapes the wrong metrics as well "+
				"as running the wrong parser", got)
		}
	})

	t.Run("and in either container order", func(t *testing.T) {
		if got := detectFromPodTemplate(tmplOf(benchSidecar, engineByImage)); got != EngineSGLang {
			t.Errorf("Detect = %q with the sidecar listed first, want sglang", got)
		}
	})

	t.Run("a vllm pod with an sglang-named sidecar still reads as sglang", func(t *testing.T) {
		// The imprecision this leaves, asserted so it is a recorded fact
		// rather than a surprise. ConfigContainers is where it is contained:
		// its cross-engine launch tier still selects the real engine's
		// container, so the cost is the wrong PARSER on the right container
		// and not a configuration invented from a router's flags.
		vllmEngine := corev1.Container{
			Name:    "vllm",
			Image:   "docker.io/vllm/vllm-openai:v0.26.0",
			Command: []string{"/bin/sh", "-c", "vllm serve /model-cache/m --block-size 128"},
		}
		router := corev1.Container{
			Name:  "sglang-router",
			Image: "ghcr.io/acme/sglang-router:1.0",
		}
		tmpl := tmplOf(vllmEngine, router)

		if got := detectFromPodTemplate(tmpl); got != EngineSGLang {
			t.Errorf("Detect = %q, want sglang: this is the known imprecision, and "+
				"if it has been fixed, ConfigContainers' cross-engine tier and its "+
				"comment both need re-deriving", got)
		}
		// ... and the containment: the ENGINE is still the container selected.
		sel := names(ConfigContainers(tmpl, EngineSGLang))
		if len(sel) != 1 || sel[0] != "vllm" {
			t.Errorf("ConfigContainers = %v, want [vllm]. The misdetection is only "+
				"tolerable because selection still finds the container that starts "+
				"an engine", sel)
		}
	})

	t.Run("no signal anywhere still defaults to vllm", func(t *testing.T) {
		a := corev1.Container{Name: "a", Image: "registry/internal:1", Command: []string{"/serve"}}
		b := corev1.Container{Name: "b", Image: "registry/sidecar:2"}
		if got := detectFromPodTemplate(tmplOf(a, b)); got != EngineVLLM {
			t.Errorf("Detect = %q, want the vllm default", got)
		}
	})
}
