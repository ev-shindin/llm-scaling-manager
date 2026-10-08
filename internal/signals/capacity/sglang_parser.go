package capacity

import (
	"strconv"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// defaultSGLangEngineParams returns EngineParams with SGLang defaults.
// Flag defaults were taken from SGLang's server_args.py.
func defaultSGLangEngineParams() EngineParams {
	return EngineParams{
		Engine:               inferenceengine.EngineSGLang,
		GpuMemoryUtilization: 0.9, // --mem-fraction-static default
		BlockSize:            1,   // --page-size default
		KvCacheDtype:         "auto",
		WeightDtype:          "auto",
		TensorParallelSize:   1,
		// SGLang auto-derives --max-running-requests from available memory when
		// unset; 256 is a conservative placeholder that underestimates capacity
		// (safe: it biases toward scale-up rather than overload).
		MaxNumSeqs:            256,
		IsV1Engine:            true, // SGLang has a single (V1-style) engine
		ChunkedPrefillEnabled: true, // chunked prefill is on by default
	}
}

// ParseSGLangArgs scans a Deployment/LWS's containers for SGLang CLI arguments
// and returns the parsed parameters. It mirrors ParseVLLMArgs:
//   - --key=value and --key value argument formats
//   - hyphen/underscore normalization
//   - shell commands: ["/bin/sh", "-c", "python -m sglang.launch_server ..."]
//   - boolean flags: --disable-cuda-graph (no value)
func ParseSGLangArgs(scaleTarget scaletarget.ScaleTargetAccessor) EngineParams {
	params := defaultSGLangEngineParams()
	if scaleTarget == nil {
		resolveEffectiveMaxBatchedTokens(&params)
		return params
	}

	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil || len(podTemplateSpec.Spec.Containers) == 0 {
		resolveEffectiveMaxBatchedTokens(&params)
		return params
	}

	// The engine's containers only, for the reason given at the vLLM call site.
	for _, container := range inferenceengine.ConfigContainers(podTemplateSpec, inferenceengine.EngineSGLang) {
		// collectArgs, variable resolution and the --key/--key=value parsing
		// loop are shared with the vLLM parser; only the per-flag mapping
		// (applySGLangParam) differs. The env is this container's own, for the
		// reason given at the vLLM call site.
		allArgs := collectArgs(container.Command, container.Args)
		parseArgsWith(allArgs, &params, envValues(&container), applySGLangParam)
	}

	resolveEffectiveMaxBatchedTokens(&params)
	return params
}

// applySGLangParam sets the corresponding EngineParams field from a normalized
// SGLang flag key and its string value, returning false when it recognised the
// key and could not use the value -- the caller then records the key, so a
// default standing in for an unreadable value is distinguishable from the
// engine's real setting. The default is still preserved either way, matching
// the vLLM parser.
func applySGLangParam(key, value string, params *EngineParams) bool {
	switch key {
	case "mem_fraction_static":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || !usableFraction(v) {
			return false
		}
		params.GpuMemoryUtilization = v
	case "page_size":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}
		params.BlockSize = v
	case keyDtype:
		if !usableWord(value) {
			return false
		}
		params.WeightDtype = value
	case "quantization":
		if !usableWord(value) {
			return false
		}
		params.Quantization = value
	case "kv_cache_dtype":
		if !usableWord(value) {
			return false
		}
		params.KvCacheDtype = value
	case "tp_size", "tensor_parallel_size", "tp":
		v, err := strconv.Atoi(value)
		if err != nil {
			return false
		}
		params.TensorParallelSize = v
	case "max_running_requests":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}
		params.MaxNumSeqs = v
	case "max_total_tokens":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}
		params.TotalKvTokensOverride = v
	case "context_length":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}
		params.MaxModelLen = v
	case "max_prefill_tokens":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}
		params.MaxNumBatchedTokens = v
	case "chunked_prefill_size":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}
		if v > 0 {
			params.MaxNumBatchedTokens = v
			params.ChunkedPrefillEnabled = true
		} else {
			// SGLang uses --chunked-prefill-size=-1 to disable chunked prefill.
			params.ChunkedPrefillEnabled = false
		}
	case "disable_cuda_graph":
		params.EnforceEager = true
	}
	return true
}
