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
		DataParallelSize:     1,
		PipelineParallelSize: 1,
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
// SGLang flag key and its string value, returning applyUnusable when it
// recognised the key and could not use the value, applyUnknown when it does
// not map the key, and applyOK otherwise -- the caller records the key in the
// first two cases, so a default standing in for an unreadable value is
// distinguishable from the engine's real setting. The default is still
// preserved either way, matching the vLLM parser.
func applySGLangParam(key, value string, params *EngineParams) applyResult {
	switch key {
	case "mem_fraction_static":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || !usableFraction(v) {
			return applyUnusable
		}
		params.GpuMemoryUtilization = v
	case "page_size":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.BlockSize = v
	case keyDtype:
		if !usableWord(value) {
			return applyUnusable
		}
		params.WeightDtype = value
	case "quantization":
		if !usableWord(value) {
			return applyUnusable
		}
		params.Quantization = value
	case "kv_cache_dtype":
		if !usableWord(value) {
			return applyUnusable
		}
		params.KvCacheDtype = value
	case "tp_size", "tensor_parallel_size", "tp":
		v, err := strconv.Atoi(value)
		if err != nil {
			return applyUnusable
		}
		params.TensorParallelSize = v
	case "max_running_requests":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.MaxNumSeqs = v
		params.MaxNumSeqsSet = true
	case "max_total_tokens":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.TotalKvTokensOverride = v
	case "context_length":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.MaxModelLen = v
	case "max_prefill_tokens":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.MaxNumBatchedTokens = v
	case "chunked_prefill_size":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
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
	case "dp_size", "data_parallel_size":
		return applyParallelSize(value, &params.DataParallelSize)
	case "pp_size", "pipeline_parallel_size":
		return applyParallelSize(value, &params.PipelineParallelSize)
	case "ep_size", "expert_parallel_size", "ep":
		v, err := strconv.Atoi(value)
		if err != nil || v < 1 {
			return applyUnusable
		}
		params.ExpertParallel = v > 1
	case "enable_ep_moe":
		params.ExpertParallel = true
	case "speculative_algorithm", "speculative_num_steps", "speculative_num_draft_tokens",
		"speculative_eagle_topk", "speculative_draft_model_path":
		if !usableWord(value) {
			return applyUnusable
		}
		addSpeculativePart(params, key, value)
	case "disaggregation_transfer_backend":
		if !usableWord(value) {
			return applyUnusable
		}
		params.KVConnector = value
	case "enable_hierarchical_cache":
		params.KVOffloadingBackend = "hierarchical"
	case "hicache_size":
		return applyOffloadSize(value, params)
	case "disable_radix_cache":
		params.PrefixCachingDisabled = true
	case "enable_dp_attention":
		addLayoutPart(params, key, "true")
	case "moe_a2a_backend":
		if !usableWord(value) {
			return applyUnusable
		}
		addLayoutPart(params, key, value)
	case "model_path", "model":
		applyModel(value, &params.ModelPath, params)
	case "revision":
		applyModel(value, &params.ModelRevision, params)
	default:
		// Not a flag this parser maps. parseArgsWith decides whether
		// that is harmless or the wrong-parser signal.
		return applyUnknown
	}
	return applyOK
}
