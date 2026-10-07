package capacity

import (
	"math"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

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
	// default of 16, and `wva_engine_config` reported block_size=16 for an
	// engine running 128. `--max-model-len $VLLM_MAX_MODEL_LEN` reported 0
	// against a real 16384.
	//
	// Why that is worse than merely inaccurate: these fields key a SHARING
	// decision. A field that silently defaults makes two genuinely different
	// engines hash to one digest -- a false equality, which is the dangerous
	// direction, because it licenses one variant to borrow a latency line
	// measured on the other. Recording the gap is what lets the digest refuse
	// to assert an equality it never verified; see Complete and Fingerprint.
	//
	// Six causes reach this list, all the same class:
	//   - a variable reference nothing could resolve (not in the container's
	//     literal env, or supplied by valueFrom/envFrom, which this package
	//     cannot read)
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

// envValues is one container's literal environment, as a name -> value map for
// resolving variable references in its own args.
//
// Entries using valueFrom are deliberately ABSENT rather than recorded as
// empty: their value lives in a ConfigMap, Secret, field or resource reference
// that this package has no client to read, so a reference to one comes out of
// the resolver unresolved rather than as a guess. envFrom is invisible here
// for the same reason.
//
// IT IS DEFENCE IN DEPTH, NOT AN OBSERVABLE BEHAVIOUR TODAY, and a review
// proved that by reinstating the bug: recording a valueFrom entry as its empty
// .Value left every test green. The reason is that an empty substitution fails
// downstream anyway -- strconv rejects "" for the numeric keys and usableWord
// rejects it for the string ones -- so the key is recorded either way. Two
// tests claimed to pin this exclusion and neither could; see
// TestAnUnreadableReferenceIsRecorded for what they actually prove. The
// exclusion stays because it is correct at the point it is written and the
// first mapped field that tolerates an empty value would make it load-bearing
// with no warning.
func envValues(container *corev1.Container) map[string]string {
	env := make(map[string]string, len(container.Env))
	for _, e := range container.Env {
		if e.ValueFrom != nil {
			continue
		}
		env[e.Name] = e.Value
	}
	return env
}

// resolveRefs substitutes variable references in an argument value against the
// container's own environment, returning the resolved string and whether every
// reference in it could be resolved.
//
// Three forms, because two different things do the expanding and a manifest
// may use either:
//
//   - $(VAR) is Kubernetes' own syntax. The kubelet expands it in command and
//     args from the container's env before the process starts, so the string
//     in the manifest is not what the engine receives.
//   - $VAR and ${VAR} are the shell's. They survive into the manifest whenever
//     the command is `/bin/sh -c "... --block-size $VLLM_BLOCK_SIZE ..."`,
//     which is the shape this parser actually meets in the field, and the
//     shell expands them from the same env.
//
// $$ is a literal dollar: Kubernetes' escape for a reference it should not
// expand, and the sequence a manifest uses when it wants the shell to see a
// single $. It is emitted as one "$" and consumes no name.
//
// NO OPERATOR IS INTERPRETED. `${VAR:-default}`, `${VAR%%suffix}`,
// `${VAR:+x}` and the rest all resolve only if a variable of that whole body
// happens to exist, which it will not -- so they come out unresolved. That is
// deliberate and was once otherwise; see the note at the lookup below for the
// three ways honouring `:-` fabricated values and reported them verified.
//
// A reference to a name the container does not define is NOT substituted with
// an empty string. The whole point is to be able to say "unknown"; silently
// reading an unset variable as zero-length is how `--block-size` became 16.
func resolveRefs(s string, env map[string]string) (string, bool) {
	if !strings.Contains(s, "$") {
		return s, true
	}
	var out strings.Builder
	resolved := true
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			out.WriteByte(s[i])
			continue
		}
		// "$$" -> a literal dollar, consuming both bytes.
		if i+1 < len(s) && s[i+1] == '$' {
			out.WriteByte('$')
			i++
			continue
		}
		name, next, ok := varNameAt(s, i)
		if !ok {
			// A trailing or isolated "$" is not a reference. Keep it, and do
			// not call the value unresolved on its account.
			out.WriteByte('$')
			continue
		}
		// NO SHELL-OPERATOR HANDLING. A brace body is taken as a NAME, and
		// anything that is not one simply does not resolve.
		//
		// A previous revision honoured `${VAR:-default}`, to spare llmdbench
		// fleets a permanent incompleteness. It introduced three ways to
		// fabricate a value and report it verified, which is worse than the
		// incompleteness it avoided, and a review measured all three:
		//
		//   - `env` cannot tell "unset" from "set by something this package
		//     cannot read". So `${VLLM_MAX_MODEL_LEN:-16384}` with the real
		//     value in a ConfigMap resolved to 16384 and reported
		//     Complete() == true -- a fabricated value handed a sharing key,
		//     on the exact manifest shape that motivated the feature, and
		//     flatly against the invariant envValues documents.
		//   - the fallback text was written out without being resolved, so
		//     `${A:-$B}` yielded the literal "$B" as a dtype, complete; and
		//     because varNameAt stops at the first `}`, `${A:-${B}}` yielded
		//     "fp8}".
		//   - the operator was detected by searching the body for "-", so
		//     every hyphen-bearing body was mis-split: `${A:+--quantization}`
		//     became "-quantization" and `${A%-suf}` became "suf".
		//
		// Each is the same defect as the one this file exists to fix, reached
		// through a convenience. An unresolvable reference stays unresolved;
		// the fleet then learns its own ITL line instead of borrowing one,
		// which is exactly what it did before any of this work.
		v, found := env[name]
		// A SUBSTITUTED VALUE THAT IS ITSELF A REFERENCE. The kubelet expands
		// `$(OTHER)` between env vars, so `--dtype $D` with `D="$(E)"`
		// resolves every lookup successfully and still yields "$(E)" -- which
		// was then hashed as a verified dtype.
		//
		// Caught here, at substitution, and not by re-scanning the output:
		// after `a$$b` becomes `a$b` the output is indistinguishable from an
		// unexpanded `$b`, and an output scan rejected the legitimate escape.
		// At this point the two are still separable, because an escape never
		// reaches this branch at all.
		if found && containsVarRef(v) {
			resolved = false
			out.WriteString(v)
			i = next - 1
			continue
		}
		if !found {
			resolved = false
			// Keep the reference in the output so a log or a label shows what
			// could not be read, rather than a misleading blank.
			out.WriteString(s[i:next])
			i = next - 1
			continue
		}
		out.WriteString(v)
		i = next - 1
	}
	return out.String(), resolved
}

// varNameAt reads the variable name of a reference beginning at the "$" at
// position i, returning the name and the index just past the reference.
// It recognises ${NAME}, $(NAME) and bare $NAME.
func varNameAt(s string, i int) (name string, next int, ok bool) {
	if i+1 >= len(s) {
		return "", 0, false
	}
	switch s[i+1] {
	case '{', '(':
		closer := byte('}')
		if s[i+1] == '(' {
			closer = ')'
		}
		for j := i + 2; j < len(s); j++ {
			if s[j] == closer {
				if j == i+2 {
					return "", 0, false // "${}" is not a reference
				}
				return s[i+2 : j], j + 1, true
			}
		}
		return "", 0, false // unterminated
	}
	// Bare $NAME: the shell's name charset, which is what the shell would use
	// to decide where the name ends.
	j := i + 1
	for j < len(s) && (s[j] == '_' ||
		(s[j] >= 'A' && s[j] <= 'Z') ||
		(s[j] >= 'a' && s[j] <= 'z') ||
		(j > i+1 && s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	if j == i+1 {
		return "", 0, false
	}
	return s[i+1 : j], j, true
}

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
// AND IT ONLY CATCHES HALF OF THAT, which is worth stating because the
// sentence it replaced claimed the whole. noteUnresolved runs when
// resolveRefs fails or when apply REJECTS a value. An unmapped key -- which is
// what the other engine's flags are to this parser -- falls through apply's
// switch and returns true, so the set is never consulted. Measured: the vLLM
// parser over `--mem-fraction-static 0.8 --page-size 64` reports
// Complete() == true with every field at a vLLM default.
//
// So the set catches a misparsed pod whose other-engine flags are
// unresolvable REFERENCES, and not one whose flags are literal values. The
// residual is deliberately left: it needs the wrong parser AND literal values
// AND is the same risk class as ConfigContainers' all-containers fallback,
// which is documented and accepted. Three mechanisms added to this PR in
// response to earlier reviews each opened a false-equality hole of their own,
// so a fourth is not the answer -- and `--mem-fraction-static` on a pod that
// Detect reads as vLLM is a configuration no parser here can describe anyway.
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

// collectArgs merges container Command and Args, expanding shell commands.
// If the command is a shell invocation (e.g. ["/bin/sh", "-c", "..."]),
// the shell string is split into tokens.
func collectArgs(command, args []string) []string {
	all := make([]string, 0, len(command)+len(args))
	all = append(all, command...)
	all = append(all, args...)

	// Detect shell invocation: ["/bin/sh", "-c", "cmd ..."] or similar
	for i := 0; i < len(all)-1; i++ {
		base := all[i]
		if (base == "/bin/sh" || base == "/bin/bash" || base == "sh" || base == "bash") && i+1 < len(all) && all[i+1] == "-c" && i+2 < len(all) {
			// Split the shell command string
			shellTokens := splitShellString(all[i+2])
			return shellTokens
		}
	}

	return all
}

// splitShellString performs basic shell-like splitting on a command string.
// It handles simple single/double quoting, and shell line-continuation
// (a backslash immediately followed by a newline, the idiomatic way to write
// a long `vllm serve ...` invocation across multiple lines -- used by every
// scenario in this repo and, in practice, by real deployments generally). It
// is not a full shell parser: escape sequences (\"), variable expansion
// ($VAR), and command substitution are not supported.
//
// Line continuation matters more than it looks: left unhandled, the
// backslash and newline at the end of one line get glued onto the front of
// the next line's first token (e.g. "\\\n--max-num-batched-tokens" instead
// of "--max-num-batched-tokens"). That fails the "--" prefix check in
// parseArgsWith, so every flag after the first physical line of a multi-line
// command was silently skipped -- not a rare edge case, but the normal shape
// of a customCommand block. A bare newline (no preceding backslash, as
// between this function's non-flag preamble lines) is treated the same as a
// space: it ends a token, but isn't itself content.
func splitShellString(s string) []string {
	var tokens []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && !inSingleQuote && !inDoubleQuote && isLineBreakAt(s, i+1):
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
			if s[i+1] == '\r' {
				i++ // CRLF: consume the carriage return too
			}
			i++ // consume the newline
		case ch == '\'' && !inDoubleQuote:
			inSingleQuote = !inSingleQuote
		case ch == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
		case isSpace(ch) && !inSingleQuote && !inDoubleQuote:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// isSpace reports whether ch separates tokens. Tabs and carriage returns
// count: a YAML block scalar indented with tabs, or a manifest authored on
// Windows, otherwise glues the whitespace onto the next token, which then
// fails the "--" prefix check in parseArgsWith and is silently skipped --
// the same failure mode as an unhandled line continuation.
func isSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

// isLineBreakAt reports whether position i begins a newline, LF or CRLF, so a
// trailing backslash is recognised as a line continuation in both.
func isLineBreakAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	return s[i] == '\n' || (s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n')
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
// apply returns false when it recognised the key and could not use the value.
// An unrecognised key returns true: there is nothing to record about a flag
// this parser does not map.
func parseArgsWith(args []string, params *EngineParams, env map[string]string,
	apply func(key, value string, params *EngineParams) bool) {
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
				// define, or something only the API server could read.
				noteUnresolved(params, key)
				continue
			}
			value = resolved
		}

		if !apply(key, value, params) {
			noteUnresolved(params, key)
		}
	}
}

// applyParam sets the corresponding EngineParams field from a normalized key
// and its string value, returning false when it recognised the key and could
// not use the value.
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
func applyParam(key, value string, params *EngineParams) bool {
	switch key {
	case "gpu_memory_utilization":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || !usableFraction(v) {
			return false
		}
		params.GpuMemoryUtilization = v
	case "block_size":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}
		params.BlockSize = v
	case "kv_cache_dtype":
		if !usableWord(value) {
			return false
		}
		params.KvCacheDtype = value
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
	case "tensor_parallel_size":
		v, err := strconv.Atoi(value)
		if err != nil {
			return false
		}
		params.TensorParallelSize = v
	case "num_gpu_blocks_override":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}
		params.NumGpuBlocksOverride = v
	case "max_num_batched_tokens":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
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
			return false
		}
		params.MaxNumSeqs = v
	case "max_model_len":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
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
	}
	return true
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

// containsVarRef reports whether a value still holds something resolveRefs
// would treat as a variable reference.
//
// Reached only AFTER resolution, so a reference here means the substituted
// text itself contained one -- the kubelet expands `$(OTHER)` between env
// vars, so `--dtype $D` with `D="$(E)"` yields `"$(E)"` with every lookup
// having succeeded. It shares varNameAt with the resolver on purpose: a
// second, independently-written notion of "looks like a reference" is how the
// two would drift.
func containsVarRef(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			i++ // an escaped literal dollar, not a reference
			continue
		}
		if _, _, ok := varNameAt(s, i); ok {
			return true
		}
	}
	return false
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
// ITSELF, which TestFingerprintAgreesWithCapacityCompatibilityOnNaN exists to
// forbid. An equality that is not reflexive is the wrong shape for a
// comparison.
//
// The rule that an unreadable flag is an absence of evidence is real, but it
// is NOT enforced here and not at Store.FindCompatible either -- see the long
// note in FindCompatible for why, and for the measured 30x over-estimate that
// came of enforcing it there. The only place it gates anything is
// engineFingerprint, which withholds the SHARING key.
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
