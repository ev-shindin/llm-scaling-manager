// Package inferenceengine identifies which LLM inference engine (vLLM, SGLang)
// a scale target runs. WVA's metric collection and deployment-argument parsing
// are engine-specific, so the engine must be known before queries are built.
//
// Detection is conservative: a variant is treated as SGLang only when a strong
// signal is present in its pod template. Everything else — including pods with no
// recognizable signal — defaults to vLLM, preserving WVA's historical behavior.
package inferenceengine

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Engine identifies an LLM inference engine.
type Engine string

const (
	// EngineVLLM is the vLLM inference engine. It is the default when no other
	// engine is detected, so existing deployments are unaffected.
	EngineVLLM Engine = "vllm"

	// EngineSGLang is the SGLang inference engine.
	EngineSGLang Engine = "sglang"
)

// String returns the engine identifier (e.g. "vllm").
func (e Engine) String() string {
	return string(e)
}

// Detect inspects a scale target's leader pod template (container images,
// commands, and args) and returns the inference engine it runs. It defaults to
// EngineVLLM when the scale target is nil, has no pod template, or carries no
// SGLang signal.
func Detect(st scaletarget.ScaleTargetAccessor) Engine {
	if st == nil {
		return EngineVLLM
	}
	return detectFromPodTemplate(st.GetLeaderPodTemplateSpec())
}

// detectFromPodTemplate returns the engine implied by a pod template, defaulting
// to EngineVLLM.
func detectFromPodTemplate(tmpl *corev1.PodTemplateSpec) Engine {
	if tmpl == nil {
		return EngineVLLM
	}

	// A LAUNCH COMMAND FIRST, either engine's, before any image is consulted.
	//
	// The image check alone made a `sglang-router` sidecar beside a vLLM
	// engine detect as SGLang, which then sent the SGLang parser looking for
	// an SGLang launch command, finding none, and -- before ConfigContainers
	// grew its cross-engine tier -- selecting the router as the thing whose
	// flags describe the engine. The detection was the root of that, so it is
	// fixed here too and not only worked around downstream.
	//
	// SGLang is still tested before vLLM, preserving the "conservative: only a
	// strong signal makes it SGLang" rule of this package: the two predicates
	// are disjoint in practice, and a pod carrying both launch commands is
	// already beyond what either parser can describe.
	for i := range tmpl.Spec.Containers {
		if sglangLaunchCommand(&tmpl.Spec.Containers[i]) {
			return EngineSGLang
		}
	}
	for i := range tmpl.Spec.Containers {
		if vllmLaunchCommand(&tmpl.Spec.Containers[i]) {
			return EngineVLLM
		}
	}

	// No launch command anywhere: fall back to the image, which is the
	// historical signal and the right one for an engine whose entrypoint is
	// baked in.
	for i := range tmpl.Spec.Containers {
		if isSGLangContainer(&tmpl.Spec.Containers[i]) {
			return EngineSGLang
		}
	}
	return EngineVLLM
}

// isSGLangContainer reports whether a container runs an SGLang server, based on
// its image reference or launch command/args.
func isSGLangContainer(c *corev1.Container) bool {
	if strings.Contains(strings.ToLower(c.Image), "sglang") {
		return true
	}

	// The command is joined into a single lowercase string so both the argv form
	// (["python", "-m", "sglang.launch_server", ...]) and the shell form
	// (["/bin/sh", "-c", "python -m sglang.launch_server ..."]) are caught, and
	// only the documented launch forms are matched. See sglangLaunchCommand,
	// which is this half on its own -- ConfigContainers needs the command
	// signal without the image one, because they are not equally trustworthy.
	return sglangLaunchCommand(c)
}

// isVLLMContainer reports whether a container runs a vLLM server, based on its
// launch command/args or its image reference.
//
// Only the documented launch forms, for the same reason isSGLangContainer
// gives: a bare "-m vllm" prefix would also match "-m vllm_bench" and
// anything else beginning that way.
func isVLLMContainer(c *corev1.Container) bool {
	if vllmLaunchCommand(c) {
		return true
	}
	return strings.Contains(strings.ToLower(c.Image), "vllm")
}

// vllmLaunchCommand reports whether a container's command or args actually
// launch a vLLM server. It is the STRONG signal, kept separate from the image
// check because the two are not equally trustworthy -- see ConfigContainers.
func vllmLaunchCommand(c *corev1.Container) bool {
	cmd := strings.ToLower(strings.Join(slices.Concat(c.Command, c.Args), " "))
	return strings.Contains(cmd, "vllm serve") ||
		strings.Contains(cmd, "vllm.entrypoints")
}

// sglangLaunchCommand is isSGLangContainer's command half, for the same reason.
func sglangLaunchCommand(c *corev1.Container) bool {
	cmd := strings.ToLower(strings.Join(slices.Concat(c.Command, c.Args), " "))
	return strings.Contains(cmd, "sglang.launch_server") ||
		strings.Contains(cmd, "sglang serve")
}

// ConfigContainers returns the containers whose args carry the engine's
// configuration, which is what an argument parser should read and nothing else.
//
// It exists because reading EVERY container is wrong in a way that is silent.
// A pod that runs the engine beside a routing sidecar, a proxy or an exporter
// has several containers with flags, and a parser that walks all of them lets
// the last one win on any flag name they happen to share -- so a sidecar's
// --dtype or --block-size would be published and hashed as the engine's. There
// is no error and no missing series; the configuration is simply someone
// else's.
//
// THE TWO SIGNALS ARE NOT EQUALLY GOOD, and the order matters:
//
//  1. A launch command. `vllm serve ...` or `sglang.launch_server` in the
//     command or args is the engine actually being started, and it is what
//     the deployments in the field carry -- including through a shell wrapper,
//     since the whole `-c` string is searched.
//  2. The image reference. Weaker, because "vllm" appears in the image of
//     things that are not an engine: a vllm-router, a vllm-exporter, a
//     benchmark image built from one. It is the fallback for a container whose
//     command is a bare entrypoint with the launch built in.
//
// Taking them in that order means a sidecar named after the engine cannot
// outvote the container that demonstrably starts it.
//
// When NEITHER signal identifies anything, every container is returned -- the
// historical behaviour. That is deliberate: returning none would leave the
// params entirely at their defaults with nothing recorded as unread, which is
// a configuration of pure defaults presented as a measurement, and that is the
// false equality the unresolved set exists to prevent. A possibly-wrong read
// that the rest of the pipeline can still sanity-check beats a confidently
// empty one.
func ConfigContainers(tmpl *corev1.PodTemplateSpec, engine Engine) []corev1.Container {
	if tmpl == nil {
		return nil
	}
	all := tmpl.Spec.Containers

	launches, images := sglangLaunchCommand, isSGLangContainer
	otherLaunches := vllmLaunchCommand
	if engine != EngineSGLang {
		launches, images = vllmLaunchCommand, isVLLMContainer
		otherLaunches = sglangLaunchCommand
	}

	if sel := matching(all, launches); len(sel) > 0 {
		return sel
	}

	// EITHER ENGINE'S launch command beats an image, and this tier is why.
	//
	// Detect decides which engine a pod runs, and its image tier treats any
	// container whose image contains "sglang" as a strong signal. So a
	// `sglang-router` sidecar beside a vLLM engine makes Detect say SGLang,
	// and asking for SGLang's launch command then matches nothing -- after
	// which an image tier would select the ROUTER and nothing else. Measured:
	// the engine's own --dtype, --block-size and --max-model-len all
	// disappeared and the router's --page-size was hashed as the engine's
	// configuration. That is the exact failure this function exists to
	// prevent, made worse than the loop it replaced, which at least still
	// read the engine's container.
	//
	// A container that demonstrably starts an inference server is the engine
	// whichever server it starts. Taking it here, before any image is
	// considered, means a misdetected engine costs the WRONG PARSER on the
	// right container -- which mis-maps some flags -- rather than the right
	// parser on a sidecar, which invents a configuration. The first is
	// visible in the published flags; the second is not.
	if sel := matching(all, otherLaunches); len(sel) > 0 {
		return sel
	}

	if sel := matching(all, images); len(sel) > 0 {
		return sel
	}
	return all
}

// matching returns the containers satisfying pred, in template order.
func matching(all []corev1.Container, pred func(*corev1.Container) bool) []corev1.Container {
	var out []corev1.Container
	for i := range all {
		if pred(&all[i]) {
			out = append(out, all[i])
		}
	}
	return out
}

// Present returns the deterministically-ordered set of distinct engines detected
// across a collection of scale targets. vLLM is ordered first when present. When
// the input is empty, it returns just EngineVLLM so callers always query at least
// the default engine.
func Present(scaleTargets map[string]scaletarget.ScaleTargetAccessor) []Engine {
	seen := make(map[Engine]bool, 2)
	for _, st := range scaleTargets {
		seen[Detect(st)] = true
	}
	if len(seen) == 0 {
		return []Engine{EngineVLLM}
	}

	// Deterministic order: vLLM first, then SGLang.
	engines := make([]Engine, 0, len(seen))
	for _, e := range []Engine{EngineVLLM, EngineSGLang} {
		if seen[e] {
			engines = append(engines, e)
		}
	}
	return engines
}
