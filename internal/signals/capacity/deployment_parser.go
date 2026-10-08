package capacity

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// ParseEngineArgs parses a scale target's container args using the parser
// appropriate for the given inference engine, returning the shared
// EngineParams. It dispatches to ParseSGLangArgs for SGLang and ParseVLLMArgs
// otherwise (the default), so existing vLLM behavior is unchanged.
func ParseEngineArgs(engine inferenceengine.Engine, scaleTarget scaletarget.ScaleTargetAccessor) EngineParams {
	if engine == inferenceengine.EngineSGLang {
		return ParseSGLangArgs(scaleTarget)
	}
	return ParseVLLMArgs(scaleTarget)
}

// EngineParams holds inference-engine configuration parameters parsed from a
// Deployment/LWS's container args and environment variables. These are used
// to derive compute-bound capacity (k2) when no live metrics are available.
//
// It is the shared engine-params type for all supported engines: ParseVLLMArgs
// populates it from vLLM flags and ParseSGLangArgs populates it from SGLang flags
// (mapped onto the same fields). Field comments note the per-engine flag mapping.
type EngineParams struct {
	// Engine records which inference engine produced these params (set by the
	// parser: EngineVLLM for ParseVLLMArgs, EngineSGLang for ParseSGLangArgs).
	// IsCapacityCompatible compares it so a capacity record learned for one engine
	// is never reused as the zero-replica estimate for a different engine serving
	// the same model on the same hardware.
	Engine inferenceengine.Engine

	// WeightDtype is the dtype the weights are loaded in (--dtype), and
	// Quantization the weight quantization method (--quantization, unset when
	// empty). Both are distinct from KvCacheDtype, which is the KV cache's
	// dtype and says nothing about the weights.
	//
	// They matter twice. They change how much of the GPU the weights occupy,
	// and so how much is left for KV at a given GpuMemoryUtilization -- that
	// is k1. And they dominate the inter-token latency, so any key for a
	// latency model that omits them pools an FP8 and a BF16 serving of one
	// model onto a single ITL line.
	WeightDtype  string // default: "auto"
	Quantization string // default: "" (none)

	GpuMemoryUtilization  float64 // default: 0.9
	BlockSize             int64   // default: 16 (vLLM block size / SGLang page size)
	KvCacheDtype          string  // default: "auto"
	TensorParallelSize    int     // default: 1
	NumGpuBlocksOverride  int64   // default: 0 (not set) — vLLM only
	MaxNumBatchedTokens   int64   // default: 0 (auto)
	MaxNumSeqs            int64   // default: 256 (vLLM --max-num-seqs / SGLang --max-running-requests)
	MaxModelLen           int64   // default: 0 (auto) (vLLM --max-model-len / SGLang --context-length)
	EnforceEager          bool    // default: false (vLLM --enforce-eager / SGLang --disable-cuda-graph)
	IsV1Engine            bool    // VLLM_USE_V1 env detection (default: true since v0.8); always true for SGLang
	ChunkedPrefillEnabled bool    // true for V1, or --enable-chunked-prefill

	// TotalKvTokensOverride is the explicit total KV-cache token capacity, when the
	// engine exposes it as a deployment flag (SGLang --max-total-tokens). 0 = unset.
	// vLLM has no equivalent flag and uses NumGpuBlocksOverride instead.
	TotalKvTokensOverride int64

	// EffectiveMaxBatchedTokens is the resolved per-step token budget used
	// for k2 derivation. It is computed after parsing all other fields.
	EffectiveMaxBatchedTokens int64

	// Unresolved names the flags whose value this parser could NOT read, so
	// the field below them is a DEFAULT standing in for something unknown
	// rather than a measurement of the engine.
	//
	// It exists because that distinction was invisible, and the consequence
	// was measured on a cluster. llm-d/llmdbench Deployments put the flags on
	// the command line with the values as shell variables --
	// `--block-size $VLLM_BLOCK_SIZE` with VLLM_BLOCK_SIZE=128 in the
	// container env. ParseInt fails on "$VLLM_BLOCK_SIZE", the field kept its
	// default of 16, and the capacity derivation ran on 16 for an engine
	// running 128. `--max-model-len $VLLM_MAX_MODEL_LEN` read 0 against a real
	// 16384. Both figures were read back off a running fleet.
	//
	// Why that is worse than merely inaccurate: these fields decide whether
	// two variants count as the same capacity configuration
	// (IsCapacityCompatible). A field that silently defaults makes two
	// genuinely different engines compare EQUAL -- a false equality, which is
	// the dangerous direction, because it licenses one variant to be priced
	// from a figure measured on the other, and an over-stated capacity orders
	// too few replicas.
	//
	// Recording the gap is what lets a later consumer refuse to assert an
	// equality nothing verified. Complete() is that predicate. It has no
	// production caller in this change -- see its own doc comment -- and is
	// included because the mechanism is incoherent without a way to ask the
	// question.
	//
	// Seven causes reach this list, all the same class:
	//   - a variable reference nothing could resolve (not in the container's
	//     literal env, or supplied by valueFrom/envFrom, which this package
	//     cannot read)
	//   - a reference that was STARTED and could not be finished: `${FOO`
	//     with no closing brace, or `${}` with no name. A review found this
	//     one missing from the list and from the code: such a value was
	//     reported resolved, so `--dtype=${FOO` set the field to the literal
	//     "${FOO" and still read as Complete. A lone or trailing `$`, and a
	//     `$` before a byte that cannot begin a name, are deliberately NOT
	//     here -- they are not references at all (see startsDelimitedRef)
	//   - a reference that RESOLVED to a value which is itself a reference.
	//     The kubelet expands `$(OTHER)` between env vars, so the lookup
	//     succeeds and the result is still unusable; this is the one the
	//     mechanism exists for, and it is not the case above -- there the name
	//     was absent, here it was found
	//   - a resolved value that is not a number where a number is required
	//   - a memory fraction rejected by usableFraction (NaN, Inf, out of range)
	//   - an EMPTY resolved value for a string flag (`--dtype=`, or a
	//     reference to an env var set to ""), which has a value token and so
	//     is not the case below
	//   - a flag we map whose value token is missing entirely
	//
	// AND ONE CAUSE OF A DIFFERENT CLASS, which is why the list above says
	// "seven" and this is counted separately: a flag THIS parser does not map
	// that the OTHER engine does. There is no field below it holding a
	// default -- there is no field at all -- and what it records is not "a
	// value I could not read" but "the wrong parser is reading this
	// container", so every field it did not set is a default standing in for
	// something unknown. See mappedValueKeys and parseArgsWith's applyUnknown
	// arm.
	//
	// Sorted and deduplicated, so it is stable enough to hash.
	Unresolved []string
}

// Complete reports whether every flag this parser maps was actually read.
//
// A false result does not mean the params are unusable -- the defaults are
// still the best available guess, and k2 derivation goes on using them. It
// means they must not be used as an EQUALITY: two incompletely-read
// configurations can agree on every field and still describe different
// engines. Callers that share learned state between variants gate on this.
func (p *EngineParams) Complete() bool {
	return p != nil && len(p.Unresolved) == 0
}

// defaultEngineParams returns EngineParams with vLLM defaults
// as of vLLM v0.8+. If vLLM changes its defaults in a future version,
// these values should be updated accordingly.
func defaultEngineParams() EngineParams {
	return EngineParams{
		Engine:                inferenceengine.EngineVLLM,
		GpuMemoryUtilization:  0.9,
		BlockSize:             16,
		KvCacheDtype:          "auto",
		WeightDtype:           "auto",
		TensorParallelSize:    1,
		MaxNumSeqs:            256,
		IsV1Engine:            true, // default since vLLM v0.8
		ChunkedPrefillEnabled: true, // V1 engine uses chunked prefill by default
	}
}

// ParseVLLMArgs scans a Deployment/LWS's containers for vLLM CLI arguments
// and environment variables, returning the parsed parameters.
//
// It handles:
//   - --key=value and --key value argument formats
//   - Hyphen/underscore normalization (--gpu-memory-utilization = --gpu_memory_utilization)
//   - Shell commands: ["/bin/sh", "-c", "vllm serve model --arg=val"]
//   - Boolean flags: --enforce-eager (no value)
//   - VLLM_USE_V1 environment variable for V1 engine detection
func ParseVLLMArgs(scaleTarget scaletarget.ScaleTargetAccessor) EngineParams {
	params := defaultEngineParams()
	if scaleTarget == nil {
		resolveEffectiveMaxBatchedTokens(&params)
		return params
	}

	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil || len(podTemplateSpec.Spec.Containers) == 0 {
		resolveEffectiveMaxBatchedTokens(&params)
		return params
	}

	// The ENGINE's containers, not every container. A pod that runs the engine
	// beside a routing sidecar or an exporter has several containers carrying
	// flags, and walking all of them let the last one win on any flag name they
	// shared -- publishing and hashing a sidecar's --dtype as the engine's,
	// with no error and no missing series. ConfigContainers documents the rule
	// and its fallback.
	for _, container := range inferenceengine.ConfigContainers(podTemplateSpec, inferenceengine.EngineVLLM) {
		// Check environment variables first
		for _, env := range container.Env {
			if env.Name == "VLLM_USE_V1" {
				if env.Value == "0" {
					params.IsV1Engine = false
					params.ChunkedPrefillEnabled = false // V0 default
				}
				// Any other value (including "1", empty) keeps V1 = true
			}
		}

		// Collect all args from Command + Args, handling shell commands
		allArgs := collectArgs(container.Command, container.Args)

		// Resolved against THIS container's env, not a merged one. A
		// reference in a container's args is expanded from its own
		// environment, and merging two containers' envs would let a sidecar
		// supply a value the engine never sees.
		parseArgs(allArgs, &params, envValues(&container))
	}

	// V1 engine always enables chunked prefill regardless of flag
	if params.IsV1Engine {
		params.ChunkedPrefillEnabled = true
	}

	resolveEffectiveMaxBatchedTokens(&params)
	return params
}

// applyResult is what one flag's value did to the params.
//
// THREE OUTCOMES, NOT TWO. This was a bool, where false meant "recognised the
// flag and could not use the value" and true meant EITHER "recognised and
// used" OR "never heard of this flag". Two opposite outcomes sharing one
// return value is what made the wrong-parser case undetectable: an unmapped
// key fell through apply's switch as a success, so nothing consulted
// mappedValueKeys and `--mem-fraction-static 0.8` read as a complete vLLM
// configuration of defaults. parseArgsWith can now ask the question that
// closes it.
type applyResult uint8

const (
	// applyUnknown: not a flag this parser maps. Harmless for most flags, and
	// the wrong-parser signal when the other engine maps it.
	applyUnknown applyResult = iota
	// applyOK: recognised, and the value was used.
	applyOK
	// applyUnusable: recognised, and the value could not be used -- not a
	// number where one is required, a rejected fraction, an empty string for
	// a flag that needs one.
	applyUnusable
)

// paramApplier sets one flag's value on params and says what happened. Named
// because both parsers implement it and parseArgsWith takes it; spelling the
// signature out at three sites invited them to drift.
type paramApplier func(key, value string, params *EngineParams) applyResult

// noteUnresolved records that a flag we map could not be read, keeping the
// list sorted and free of duplicates so it is stable enough to hash.
//
// Only keys this parser actually maps are recorded. An unresolved
// `--model $MODEL_NAME` says nothing about capacity or latency and would
// otherwise make every deployment's digest incomplete for no gain.
func noteUnresolved(params *EngineParams, key string) {
	if _, ok := mappedValueKeys[key]; !ok {
		return
	}
	if slices.Contains(params.Unresolved, key) {
		return
	}
	params.Unresolved = append(params.Unresolved, key)
	slices.Sort(params.Unresolved)
}

// keyDtype is named because goconst counts "dtype" across both parsers' case
// labels and the key set below. The other case labels stay literal, where a
// constant would read worse than the flag name it stands for.
const keyDtype = "dtype"

// mappedValueKeys is every normalized flag key, ACROSS BOTH ENGINES, whose
// VALUE lands in a field that is hashed or compared. Boolean flags are absent:
// they carry no value, so there is nothing to fail to resolve.
//
// ONE SET, AND THE CROSS-ENGINE REACH IS THE POINT. A revision split it per
// engine, to stop the vLLM parser reporting an unresolvable
// `--mem-fraction-static` that applyParam does not map -- a false
// incompleteness. But the split also removed the only signal that the WRONG
// PARSER RAN. Measured: the vLLM parser over a pod's SGLang flags went from
// `Complete()==false` to `Complete()==true` with every field at a vLLM default
// and a published fingerprint -- a configuration of pure defaults presented as
// verified, which then gets a sharing key.
//
// That matters because the wrong parser CAN run: Detect decides the engine
// from any container's image, so a pod with an engine-named sidecar is
// misdetected, and ConfigContainers deliberately prefers the right CONTAINER
// over the right parser.
//
// IT NOW CATCHES THE LITERAL-VALUE HALF TOO, which the comment that stood
// here said was impossible -- so this paragraph is the correction as much as
// the description.
//
// The gap was that noteUnresolved only ran when resolveRefs failed or when
// apply REJECTED a value. An unmapped key fell through apply's switch as a
// success, so this set was never consulted and the case needed literal values
// to hit: `--mem-fraction-static 0.8 --page-size 64` read as a complete vLLM
// configuration of defaults. The old note argued that closing it needed a
// fourth mechanism and that three earlier mechanisms had each opened a hole of
// their own, so it accepted the residual.
//
// It did not need a mechanism. It needed a RETURN TYPE: applyParam's bool gave
// one value to "recognised, unusable" and to "never heard of this flag", which
// is why the second could not be acted on. applyResult separates them, and
// parseArgsWith asks the question that was unaskable -- is this unknown key
// one the OTHER engine maps? -- which is precisely the wrong-parser signal.
// TestTheWrongParserDoesNotReportACompleteConfiguration pins both directions
// and the control that a flag neither engine maps is still ignored.
//
// WHAT REMAINS, stated precisely, because this set only fires when the two
// parsers' flag sets DIFFER:
//
//  1. A misread container whose flags fall entirely inside the SHARED subset
//     -- dtype, quantization, kv_cache_dtype, tensor_parallel_size, which both
//     appliers map. Every flag then reads fine and nothing is unknown, so the
//     parse reports complete while every UNSHARED field silently holds the
//     wrong engine's default. Measured: the SGLang parser over
//     `--dtype bfloat16 --quantization fp8 --kv-cache-dtype fp8
//     --tensor-parallel-size 2` gives Complete() == true with BlockSize 1,
//     SGLang's page-size default, where the vLLM engine's default is 16.
//     TestTheWrongParserDoesNotReportACompleteConfiguration records it as a
//     known limit beside the cases that do fire.
//  2. A misread container carrying NO flag either engine maps -- nothing to
//     catch, and indistinguishable from a deliberately minimal configuration.
//     That is ConfigContainers' fourth-tier risk rather than this set's.
//
// Both are narrower than what this catches, and neither is closable here: the
// signal is "a flag belonging to the other engine", and in both cases there
// is no such flag.
var mappedValueKeys = map[string]struct{}{
	// vLLM
	"gpu_memory_utilization":  {},
	"block_size":              {},
	"kv_cache_dtype":          {},
	keyDtype:                  {},
	"quantization":            {},
	"tensor_parallel_size":    {},
	"num_gpu_blocks_override": {},
	"max_num_batched_tokens":  {},
	"max_num_seq":             {},
	"max_num_seqs":            {},
	"max_model_len":           {},
	// SGLang
	"mem_fraction_static":  {},
	"page_size":            {},
	"tp_size":              {},
	"tp":                   {},
	"max_running_requests": {},
	"max_total_tokens":     {},
	"context_length":       {},
	"max_prefill_tokens":   {},
	"chunked_prefill_size": {},
}

// normalizeKey replaces hyphens with underscores and strips the leading
// dashes so that --gpu-memory-utilization and --gpu_memory_utilization
// both normalize to "gpu_memory_utilization".
func normalizeKey(key string) string {
	key = strings.TrimLeft(key, "-")
	return strings.ReplaceAll(key, "-", "_")
}

// parseArgs walks the argument list and populates params using the vLLM flag
// mapping, resolving variable references against env.
func parseArgs(args []string, params *EngineParams, env map[string]string) {
	parseArgsWith(args, params, env, applyParam)
}

// parseArgsWith walks the argument list and applies each normalized --key/value
// pair via the supplied apply function. It is shared by the vLLM and SGLang
// parsers, which differ only in their per-flag mapping (applyParam vs
// applySGLangParam). Boolean flags (no following value) are passed with an empty
// value string.
//
// Two things happen to a value before it is applied. It is resolved against
// env, because the value in a manifest is routinely a variable reference that
// something else expands (see resolveRefs). And if either the resolution or
// the apply fails, the key is recorded in params.Unresolved -- the field then
// holds a default, and the digest has to know that it does.
//
// apply reports which of three things happened (applyResult), and the switch
// below acts on each:
//
//   - applyUnusable -- recognised the key, could not use the value. Recorded.
//   - applyUnknown  -- not a flag this parser maps. Recorded ONLY when the
//     other engine maps it, because that means the wrong parser is reading
//     this container. An earlier version of this comment said an unrecognised
//     key had "nothing to record", which is the proposition the three-state
//     result exists to falsify.
//   - applyOK       -- recognised and used.
func parseArgsWith(args []string, params *EngineParams, env map[string]string,
	apply paramApplier) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			continue
		}

		var key, value string
		hasValue := false
		if idx := strings.Index(arg, "="); idx >= 0 {
			// --key=value format
			key = normalizeKey(arg[:idx])
			value = arg[idx+1:]
			hasValue = true
		} else {
			key = normalizeKey(arg)
			// Check if next token is the value (not another flag)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				value = args[i+1]
				hasValue = true
				i++ // consume the value
			}
			// Otherwise it's a boolean flag (no value)
		}

		if hasValue {
			resolved, ok := resolveRefs(value, env)
			if !ok {
				// The reference named something the container does not
				// define literally -- so either nothing defines it, or it
				// comes from valueFrom/envFrom.
				//
				// NOT an RBAC limit: the controller already holds
				// configmaps get/list/watch. It is a layering one -- this
				// package takes a pod template and has no client - so
				// resolving a configMapKeyRef would mean giving it one. That
				// is a deliberate choice and a reviewable change, not a
				// permission to grant.
				noteUnresolved(params, key)
				continue
			}
			value = resolved
		}

		switch apply(key, value, params) {
		case applyUnusable:
			// Recognised the flag and could not use the value.
			noteUnresolved(params, key)
		case applyUnknown:
			// NOT a flag this parser maps -- and that is the wrong-parser
			// signal, when the OTHER engine's parser maps it.
			//
			// This is what the bool could not express. `false` meant
			// "recognised, unusable" and `true` meant EITHER "recognised and
			// used" OR "never heard of it", so an unmapped key fell through
			// apply's switch as a success and mappedValueKeys was never
			// consulted. Measured: the vLLM parser over
			// `--mem-fraction-static 0.8 --page-size 64` -- both SGLang flags
			// with literal values, so nothing fails to resolve -- reported
			// Complete() == true with every field at a vLLM default. A
			// configuration of pure defaults, presented as verified.
			//
			// A key in mappedValueKeys that THIS parser does not map is a key
			// the other engine maps. That is not a flag we can ignore: it
			// means the wrong parser is reading this container, so every
			// field it did not set is a default standing in for something
			// unknown. Recording it is exactly what Unresolved is for.
			//
			// A key in NEITHER set is genuinely nothing to do with capacity --
			// `--port`, `--served-model-name`, a bench flag -- and is ignored
			// as before. noteUnresolved already filters on mappedValueKeys, so
			// it is the whole decision; checking the set here as well would be
			// the same condition written twice.
			noteUnresolved(params, key)
		case applyOK:
			// Recognised and used.
		}
	}
}

// applyParam sets the corresponding EngineParams field from a normalized key
// and its string value, returning applyUnusable when it recognised the key and
// could not use the value, applyUnknown when it does not map the key at all,
// and applyOK otherwise.
//
// The default is still preserved on failure -- that part is unchanged, and is
// the right graceful degradation for an operator-controlled arg. What changed
// is that the failure is no longer SILENT: the caller records the key, so a
// default standing in for an unreadable value can be told apart from a default
// that is genuinely the engine's setting. The two were indistinguishable, and
// the digest built on them asserted equalities it had not verified.
//
// A string-valued key cannot fail: any token is a legitimate dtype as far as
// this parser is concerned, and the engine is the thing that validates it.
//
// The negative boolean forms were missing. vLLM renders several booleans with
// argparse's BooleanOptionalAction, so `--no-<flag>` is how an operator turns
// one off, and both negative forms fell through the same path as a genuinely
// unknown flag.
//
// For chunked prefill that only changes anything on V0: ParseVLLMArgs forces
// ChunkedPrefillEnabled true for V1 whatever the args said, which is
// pre-existing and reflects the engine rather than this parser. Where the flag
// does apply it matters more than a label, because it moves
// EffectiveMaxBatchedTokens from the chunked default to max(MaxModelLen, 2048),
// and that figure is hashed and derives k2.
//
// `--no-enforce-eager` has no such override and applies on either engine. Both
// forms are plain assignments, so the LAST occurrence wins, which is what
// argparse does.
func applyParam(key, value string, params *EngineParams) applyResult {
	switch key {
	case "gpu_memory_utilization":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || !usableFraction(v) {
			return applyUnusable
		}
		params.GpuMemoryUtilization = v
	case "block_size":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.BlockSize = v
	case "kv_cache_dtype":
		if !usableWord(value) {
			return applyUnusable
		}
		params.KvCacheDtype = value
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
	case "tensor_parallel_size":
		v, err := strconv.Atoi(value)
		if err != nil {
			return applyUnusable
		}
		params.TensorParallelSize = v
	case "num_gpu_blocks_override":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.NumGpuBlocksOverride = v
	case "max_num_batched_tokens":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.MaxNumBatchedTokens = v
	case "max_num_seq", "max_num_seqs":
		// vLLM accepts both --max-num-seq and --max-num-seqs (confirmed live:
		// this repo's own scenarios invoke the singular form, and vLLM starts
		// normally with it), but only the plural form was recognized here --
		// the singular one fell through to the same "unrecognized flag" path
		// as an actually-unknown flag, silently leaving MaxNumSeqs at its
		// struct default.
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.MaxNumSeqs = v
	case "max_model_len":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return applyUnusable
		}
		params.MaxModelLen = v
	case "enforce_eager":
		params.EnforceEager = true
	case "no_enforce_eager":
		params.EnforceEager = false
	case "enable_chunked_prefill":
		params.ChunkedPrefillEnabled = true
	case "no_enable_chunked_prefill":
		params.ChunkedPrefillEnabled = false
	default:
		// Not a flag this parser maps. parseArgsWith decides whether
		// that is harmless or the wrong-parser signal.
		return applyUnknown
	}
	return applyOK
}

// usableFraction reports whether a parsed memory fraction is a value the rest
// of the pipeline can divide by: finite, and strictly inside (0, 1].
//
// It exists because ParseFloat accepts "NaN" and "Inf" -- a flag of
// --gpu-memory-utilization=NaN parses without error, and the value then
// defeats every comparison downstream, since NaN is not equal to itself and
// NaN <= 0 is false. Two concrete consequences: IsCapacityCompatible would
// report a variant as incompatible with ITSELF, and the engine fingerprint
// would hash two NaN configurations to the same digest while that predicate
// called them different. Rejecting it here, so the struct default survives, is
// what keeps the two consistent -- and matches this parser's existing
// contract that an unusable value leaves the default in place.
//
// The finiteness half duplicates utils.CheckValue, deliberately and not by
// oversight: internal/utils also pulls in internal/domain, zapcore and yaml,
// which is a poor trade for one predicate in a package that imports none of
// them. If this file ever needs utils for another reason, collapse the two.
func usableFraction(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 && v <= 1
}

// usableWord reports whether a STRING-valued flag's value is something to
// believe, as opposed to something that only looks like a value.
//
// The numeric keys get this for free: strconv fails and the key is recorded as
// unresolved. The string keys -- dtype, quantization, kv_cache_dtype -- had no
// way to fail at all, so THREE inputs were taken as the engine's setting with
// the configuration reported COMPLETE, and this is the guard for all three:
//
//	--dtype              with no value token  ->  ""
//	--dtype=             explicitly empty     ->  ""
//	--dtype $D  with D=""                     ->  ""
//
// A fourth case of the same family -- `--dtype $D` where D is itself `$(E)`,
// which happens because the kubelet expands references BETWEEN env vars -- does
// NOT reach this function. resolveRefs catches it at substitution and records
// the key, so applyParam is never called; an earlier version of this comment
// listed it here, which would send someone hardening usableWord against `$(`
// to write a check that can never fire.
//
// These fields matter most because EngineParams' own comment calls dtype and
// quantization the two that dominate inter-token latency -- the pair a false
// equality hurts worst.
func usableWord(v string) bool {
	return v != ""
}

// IsCapacityCompatible checks whether two EngineParams configurations
// would produce equivalent per-replica capacity (both k1 and k2).
// Used by Store.FindCompatible to identify variants
// whose stored capacity can be reused for zero-replica estimation.
//
// MaxNumSeqs and MaxModelLen are compared even though neither enters k1,
// because both enter k2:
//
//   - MaxNumSeqs is S, which caps N_steady in the k2 derivation and caps the
//     sequence count the derived mu prices at. Two engines differing only in
//     --max-num-seqs have a different compute bound AND a different mu.
//   - MaxModelLen changes the KV a single sequence can occupy. It reaches
//     EffectiveMaxBatchedTokens only when chunked prefill is off (see
//     resolveEffectiveMaxBatchedTokens), so on the V1 path -- where chunked
//     prefill is the default -- it is otherwise invisible to this predicate.
//
// EnforceEager is deliberately NOT compared: no CUDA graphs changes the
// inter-token latency, and so the ITL line, but it does not change how much
// KV fits or how many sequences run. A capacity record stays reusable across
// it. Keying a latency model on these params is a different equality, and it
// needs this one plus EnforceEager.
// Completeness is deliberately NOT checked here, and the reason is worth
// stating because the first version of this change got it wrong. Gating the
// predicate on Complete() makes it non-reflexive: a variant whose
// --gpu-memory-utilization could not be read stops being compatible with
// ITSELF. An equality that is not reflexive is the wrong shape for a
// comparison, and TestCapacityCompatibilityStaysReflexive forbids it.
//
// The rule that an unreadable flag is an absence of evidence is real, and it
// is deliberately enforced NOWHERE in this change: not here, and not at
// Store.FindCompatible, where refusing a record was measured to turn 5,000
// into 153,600 -- a 30x OVER-estimate of per-replica capacity, and an
// over-stated capacity orders FEWER replicas. The rule is that an unreadable
// flag withholds only what is NEW; it never changes a path that already
// existed. Unresolved is therefore recorded and not yet acted on. Its first
// consumer is the learned-state sharing key, which is not in this change.
func (p *EngineParams) IsCapacityCompatible(other *EngineParams) bool {
	if p == nil || other == nil {
		return false
	}
	return p.Engine == other.Engine &&
		p.GpuMemoryUtilization == other.GpuMemoryUtilization &&
		p.BlockSize == other.BlockSize &&
		p.KvCacheDtype == other.KvCacheDtype &&
		p.TensorParallelSize == other.TensorParallelSize &&
		p.NumGpuBlocksOverride == other.NumGpuBlocksOverride &&
		p.TotalKvTokensOverride == other.TotalKvTokensOverride &&
		p.EffectiveMaxBatchedTokens == other.EffectiveMaxBatchedTokens &&
		p.MaxNumSeqs == other.MaxNumSeqs &&
		p.MaxModelLen == other.MaxModelLen &&
		p.WeightDtype == other.WeightDtype &&
		p.Quantization == other.Quantization
}

// resolveEffectiveMaxBatchedTokens computes the per-step token budget
// based on parsed parameters. This is the value used for k2 derivation.
//
// Priority:
//  1. Explicitly set --max-num-batched-tokens → use that
//  2. V1 engine with chunked prefill → 8192 (vLLM V1 default since v0.8)
//  3. V0 engine with chunked prefill → 2048 (vLLM V0 default since v0.6.5)
//  4. Unchunked prefill → max(MaxModelLen, 2048)
//  5. Fallback → 2048
func resolveEffectiveMaxBatchedTokens(params *EngineParams) {
	if params.MaxNumBatchedTokens > 0 {
		params.EffectiveMaxBatchedTokens = params.MaxNumBatchedTokens
		return
	}

	if params.ChunkedPrefillEnabled {
		if params.IsV1Engine {
			params.EffectiveMaxBatchedTokens = 8192
		} else {
			params.EffectiveMaxBatchedTokens = 2048
		}
		return
	}

	// Unchunked prefill
	if params.MaxModelLen > 2048 {
		params.EffectiveMaxBatchedTokens = params.MaxModelLen
		return
	}

	params.EffectiveMaxBatchedTokens = 2048
}
