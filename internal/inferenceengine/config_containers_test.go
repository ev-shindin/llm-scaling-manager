package inferenceengine

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Reading every container in a pod template is wrong in a way that produces no
// error: a pod running the engine beside a routing sidecar has several
// containers with flags, and a parser walking all of them lets the last one
// win on any flag name they share. A sidecar's --dtype then gets published and
// hashed as the engine's configuration.
//
// The fixtures here are the real shapes from a Kubernetes cluster: the engine
// container is named `vllm`, runs `docker.io/vllm/vllm-openai`, and starts the
// server from inside a long `/bin/bash -c` wrapper that ends in `vllm serve`.
// The llm-d routing sidecar is `ghcr.io/llm-d/llm-d-router-disagg-sidecar`.

func container(name, image string, cmd ...string) corev1.Container {
	return corev1.Container{Name: name, Image: image, Command: cmd}
}

func tmplOf(cs ...corev1.Container) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: cs}}
}

func names(cs []corev1.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("containers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("containers = %v, want %v", got, want)
		}
	}
}

func TestConfigContainersPrefersTheLaunchCommand(t *testing.T) {
	// The cluster's decode pod, with the routing sidecar promoted from an init
	// container to a regular one -- which is how it runs in other llm-d
	// layouts, and the case the old "walk every container" loop got wrong.
	engineCmd := "/bin/bash -c . /shared-config/llmdbench_env.sh && " +
		"vllm serve /model-cache/$MODEL_PATH --block-size $VLLM_BLOCK_SIZE"

	tmpl := tmplOf(
		container("routing-proxy", "ghcr.io/llm-d/llm-d-router-disagg-sidecar:v0.9.0",
			"/app/proxy", "--block-size", "8", "--dtype", "float16"),
		container("vllm", "docker.io/vllm/vllm-openai:v0.26.0", "/bin/bash", "-c", engineCmd),
	)

	eq(t, names(ConfigContainers(tmpl, EngineVLLM)), "vllm")
}

func TestConfigContainersIsNotFooledByAnEngineNamedSidecar(t *testing.T) {
	// "vllm" appears in the image of plenty of things that are not an engine.
	// The image signal alone would return both and let the router's flags win
	// if it sorted last.
	engineCmd := "/bin/sh -c exec vllm serve m --block-size 128"
	tmpl := tmplOf(
		container("vllm-router", "ghcr.io/acme/vllm-router:1.2", "/router"),
		container("engine", "docker.io/vllm/vllm-openai:v0.26.0", "/bin/sh", "-c", engineCmd),
		container("vllm-exporter", "ghcr.io/acme/vllm-metrics-exporter:0.1", "/exporter"),
	)

	eq(t, names(ConfigContainers(tmpl, EngineVLLM)), "engine")
}

func TestConfigContainersFallsBackToTheImage(t *testing.T) {
	// A container whose entrypoint is baked into the image has no launch
	// command to match, which is a legitimate way to ship an engine.
	tmpl := tmplOf(
		container("istio-proxy", "docker.io/istio/proxyv2:1.20", "/usr/local/bin/pilot-agent"),
		container("vllm", "docker.io/vllm/vllm-openai:v0.26.0"),
	)

	eq(t, names(ConfigContainers(tmpl, EngineVLLM)), "vllm")
}

func TestConfigContainersFallsBackToEverythingWhenNothingIdentifies(t *testing.T) {
	// Deliberately NOT "return none". An empty selection leaves every field at
	// its default with nothing recorded as unread, which is a configuration of
	// pure defaults presented as a measurement -- the false equality the
	// unresolved set exists to prevent. A possibly-wrong read the rest of the
	// pipeline can sanity-check beats a confidently empty one.
	tmpl := tmplOf(
		container("server", "registry.internal/our-own-build:7", "/serve", "--block-size", "64"),
		container("sidecar", "registry.internal/sidecar:2", "/side"),
	)

	eq(t, names(ConfigContainers(tmpl, EngineVLLM)), "server", "sidecar")
}

func TestConfigContainersSelectsTheSGLangServer(t *testing.T) {
	sgCmd := "python3 -m sglang.launch_server --model-path m --page-size 64"
	tmpl := tmplOf(
		container("routing-proxy", "ghcr.io/llm-d/llm-d-router-disagg-sidecar:v0.9.0",
			"/app/proxy", "--page-size", "1"),
		container("sglang", "lmsysorg/sglang:latest", "/bin/sh", "-c", sgCmd),
	)

	eq(t, names(ConfigContainers(tmpl, EngineSGLang)), "sglang")

	// And asking for the wrong engine must not silently return the other one's
	// container: there is no vLLM here, so the fallback applies and the caller
	// gets everything rather than a confident mis-selection.
	got := names(ConfigContainers(tmpl, EngineVLLM))
	if len(got) != 2 {
		t.Fatalf("vLLM selection over an SGLang pod = %v, want the fallback to "+
			"all containers rather than a guess", got)
	}
}

func TestConfigContainersHandlesAnEmptyTemplate(t *testing.T) {
	if got := ConfigContainers(nil, EngineVLLM); got != nil {
		t.Fatalf("nil template returned %v, want nil", got)
	}
	if got := ConfigContainers(tmplOf(), EngineVLLM); len(got) != 0 {
		t.Fatalf("empty template returned %v, want nothing", got)
	}
}

func TestDetectStillWorksThroughTheSplitHelpers(t *testing.T) {
	// isSGLangContainer was refactored to call sglangLaunchCommand, so Detect's
	// behaviour has to be pinned against the forms it documents.
	for _, tc := range []struct {
		name string
		c    corev1.Container
		want Engine
	}{
		{"sglang by image", container("x", "lmsysorg/sglang:v0.4"), EngineSGLang},
		{"sglang by launch module", container("x", "registry/custom:1",
			"python3", "-m", "sglang.launch_server"), EngineSGLang},
		{"sglang by serve verb", container("x", "registry/custom:1",
			"/bin/sh", "-c", "sglang serve m"), EngineSGLang},
		{"a bare -m sglang prefix is deliberately not a match",
			container("x", "registry/custom:1", "python3", "-m", "sglang_bench"), EngineVLLM},
		{"vllm is the default", container("x", "docker.io/vllm/vllm-openai:v0.26.0"), EngineVLLM},
		{"nothing recognisable defaults to vllm",
			container("x", "registry/custom:1", "/serve"), EngineVLLM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectFromPodTemplate(tmplOf(tc.c)); got != tc.want {
				t.Errorf("detect = %q, want %q", got, tc.want)
			}
		})
	}
}
