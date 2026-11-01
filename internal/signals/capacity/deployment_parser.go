package capacity

import (
	"encoding/json"
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
	var params EngineParams
	if engine == inferenceengine.EngineSGLang {
		params = ParseSGLangArgs(scaleTarget)
	} else {
		params = ParseVLLMArgs(scaleTarget)
	}
	params.Image = engineImage(engine, scaleTarget)
	return params
}

// engineImage is the image of the engine's own container(s), chosen by the
// same rule the flags are read with (inferenceengine.ConfigContainers), so a
// router or exporter sidecar's image is not taken for the engine's. Several
// containers -- the fallback when nothing identifies the engine -- give their
// images sorted and comma-joined, which is stable.
func engineImage(engine inferenceengine.Engine, scaleTarget scaletarget.ScaleTargetAccessor) string {
	if scaleTarget == nil {
		return ""
	}
	var images []string
	for _, c := range inferenceengine.ConfigContainers(scaleTarget.GetLeaderPodTemplateSpec(), engine) {
		if c.Image != "" && !slices.Contains(images, c.Image) {
			images = append(images, c.Image)
		}
	}
	slices.Sort(images)
	return strings.Join(images, ",")
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

	// MaxNumSeqsSet is true when MaxNumSeqs was read from the engine's flag
	// (directly or through a resolved variable reference), false when it is
	// the default. The default is a guess: vLLM V1 picks 1024 on large
	// accelerators, so a replica running 300 sequences may be far below its
	// real cap. See ExplicitMaxNumSeqs.
	MaxNumSeqsSet bool

	// MaxNumActiveSeqs is vLLM's --max-num-active-seqs: how many requests the
	// scheduler admits into the running batch. It lowers the batch cap below
	// MaxNumSeqs without shrinking what MaxNumSeqs sizes (the model runner's
	// buffers and CUDA graphs), and vLLM refuses a value above max_num_seqs.
	// 0 is unset, in which case MaxNumSeqs alone caps the batch. See
	// ExplicitMaxNumSeqs, which folds it into the cap.
	//
	// It is hashed into the fingerprint, so a rollout that changes only the
	// active cap is a change of arguments. A value that cannot be read (an
	// unresolved reference, not a number, below 1) leaves it 0, so the cap
	// falls back to max_num_seqs, still a true upper bound, and is recorded
	// in Unresolved like any other hashed flag. A later unreadable occurrence
	// clears an earlier readable one, since the engine takes the last.
	MaxNumActiveSeqs int64

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

	// Image is the engine container's image as the scale target names it
	// (tag or digest), set by ParseEngineArgs. It is the engine's CODE, which
	// no flag describes: vllm-openai:v0.8.5 and :v0.11.0 with identical flags
	// are different engines, and a canary rollout is the likeliest way to get
	// two variants that differ in nothing else -- with the new, cold one the
	// borrower. Hashed into the fingerprint, which keys latency lines; not
	// compared for capacity, where a tag that changes on every rebuild would
	// refuse every record across a routine rollout.
	Image string

	// The layout and features that decide what one replica of a wide-EP or
	// P/D engine holds and how fast it runs, and that no field above
	// describes. Without them two variants that differ only in, say, MTP on
	// and off, or TP1-DP8 with and without expert parallelism, hashed alike
	// and compared capacity-compatible -- the ordinary canary pair on these
	// fleets. Each is hashed and compared; see IsCapacityCompatible.
	//
	// Only the scale target's LEADER pod template is read (an LWS group's
	// workers are assumed to run the leader's flags).
	DataParallelSize     int  // default 1 (vLLM --data-parallel-size / SGLang --dp-size)
	PipelineParallelSize int  // default 1 (vLLM --pipeline-parallel-size / SGLang --pp-size)
	ExpertParallel       bool // vLLM --enable-expert-parallel / SGLang --ep-size > 1 or --enable-ep-moe
	// SpeculativeConfig is vLLM's --speculative-config, re-encoded so key
	// order and spacing do not split it, or SGLang's --speculative-* flags as
	// sorted key=value pairs. Empty is no speculative decoding.
	SpeculativeConfig string
	// KVConnector is the KV transfer connector: vLLM --kv-transfer-config's
	// kv_connector, or SGLang --disaggregation-transfer-backend. Empty is
	// none.
	KVConnector string
	// KV offloading beyond GPU memory: vLLM --kv-offloading-size (GiB) and
	// --kv-offloading-backend, SGLang --hicache-size and
	// --enable-hierarchical-cache ("hierarchical").
	KVOffloadingSize    float64
	KVOffloadingBackend string
	// PrefixCachingDisabled is vLLM --no-enable-prefix-caching or SGLang
	// --disable-radix-cache. The zero value is the engines' default, on.
	PrefixCachingDisabled bool
	// ModelPath and ModelRevision are the checkpoint served: vLLM's
	// positional model or --model, SGLang's --model-path, and --revision. An
	// FP8 and a BF16 checkpoint of one model ID otherwise hashed alike while
	// their KV caches differ ~2x.
	ModelPath     string
	ModelRevision string
	// ModelUnresolved is true when the checkpoint could not be read -- a
	// `$MODEL` nothing resolved, or a model that is not the token right after
	// `serve` and was not given as --model. It is NOT in Unresolved, and on
	// purpose: Unresolved withholds a variant's own restore after a restart,
	// and a model given as a variable is common enough that withholding on it
	// would start such fleets cold on every restart, the TTFT direction.
	//
	// It withholds what is SHARED and nothing of the variant's own: the
	// shared latency line (engineFingerprint), and a sibling's capacity --
	// IsCapacityCompatible refuses a pair where either side's checkpoint is
	// unread, since two unread checkpoints may be an FP8 and a BF16 build of
	// one model, ~2x apart in KV. Refusing costs at most a lower-only k2
	// borrow, which leaves the variant on its own figure as if it had no
	// sibling. The zero-replica estimate's max clamp is NOT refused: it is
	// found under IsCapacityClampCompatible, which does not read this field.
	ModelUnresolved bool

	// SharingUnresolved lists the flags read for identity whose value could
	// not be interpreted, as key=value with the value as written: a parallel
	// size given as an expression nothing here evaluates. llm-d's wide-EP
	// LeaderWorkerSet passes `--data-parallel-size
	// $((LWS_GROUP_SIZE * DP_SIZE_LOCAL))`, and LWS_GROUP_SIZE is set on the
	// pod, not in its template. A vLLM `--speculative-config.<key>` value
	// that does not resolve lands here too.
	//
	// Like ModelUnresolved it is NOT in Unresolved: putting it there made
	// every such wide-EP variant's read incomplete, so its own learned state
	// was neither exported nor restored -- cold on every restart. It
	// withholds what is shared: the latency line (engineFingerprint) and a
	// sibling's capacity (IsCapacityCompatible refuses a pair where either
	// side has an entry, because 2x8 and 4x8 groups written as the same
	// `$((LWS_GROUP_SIZE*...))` text are different engines). It is hashed
	// with the text as written, so an expression that changes is a
	// configuration that changes for the variant's own restore and KV carry.
	// What it cannot see is an env value the expression reads changing under
	// the same text; a reshape that changes the GPUs per replica changes the
	// learned-state key anyway.
	//
	// Sorted, one entry per key, so it is stable enough to hash.
	SharingUnresolved []string

	// MemoryLayout holds the flags that size or reshape what one replica
	// holds and that no field above describes, as sorted key=value pairs
	// joined by ";" -- the same form as SGLang's speculative flags:
	//
	//   - vLLM --kv-cache-memory-bytes: sets the KV cache outright, in place
	//     of the --gpu-memory-utilization share
	//   - vLLM --enable-eplb, --num-redundant-experts and --eplb-config:
	//     redundant experts take GPU memory the KV cache would otherwise get
	//   - vLLM --all2all-backend and the VLLM_ALL2ALL_BACKEND env var, and
	//     SGLang --moe-a2a-backend: the expert all-to-all's buffers
	//   - SGLang --enable-dp-attention: attention runs data-parallel, which
	//     changes how the KV cache is laid out across the group
	//
	// Kept as one text rather than a field each because none is read for its
	// value -- each only has to tell two engines apart. Hashed, published as
	// the memory_layout label, compared for capacity and for the KV carry.
	// A value that cannot be read lands in SharingUnresolved.
	MemoryLayout string
}

// parallelSize is a parallel size as the engine runs it: unset is 1, its
// default, so a configuration built without the field compares equal to one
// parsed from flags that do not set it.
func parallelSize(n int) int {
	if n < 1 {
		return 1
	}
	return n
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

// ExplicitMaxNumSeqs is one engine's batch cap as its flags state it: the
// explicit max_num_seqs, lowered by --max-num-active-seqs when that is set,
// and 0 when neither flag stated a cap, when a flag that bears on it could not
// be read, or when p is nil. A caller that needs the engine's real cap, not an
// estimate of it, reads this: the ITL fit admits a reading above its band
// only for a replica at its cap (itl.Admission), and a P/D decode at its cap
// keeps its full queue (ReplicaMetrics.AtBatchCap).
//
// --max-num-active-seqs alone is enough: vLLM refuses one above
// max_num_seqs, so it is the cap whatever max_num_seqs is, stated or not.
// With both, the lower one is the cap.
//
// An unreadable spelling voids a readable one: `--max-num-seqs 512
// --max-num-seqs ` sets 512, and which of the two the engine takes
// depends on a value nothing here could see.
//
// An unreadable --max-num-active-seqs does NOT void an explicit max_num_seqs:
// the parser leaves MaxNumActiveSeqs at 0 for it (see the field), and this
// falls back to max_num_seqs. That is still a true upper bound on the batch,
// since vLLM refuses an active cap above it; a replica held below it by the
// unread active cap simply never reads as at its cap, the direction that
// admits less.
func (p *EngineParams) ExplicitMaxNumSeqs() int64 {
	if p == nil {
		return 0
	}
	maxSeqs := p.explicitMaxNumSeqsFlag()
	switch active := p.MaxNumActiveSeqs; {
	case active > 0 && maxSeqs > 0:
		return min(active, maxSeqs)
	case active > 0:
		return active
	default:
		return maxSeqs
	}
}

// explicitMaxNumSeqsFlag is MaxNumSeqs when it was read from the engine's
// flag, and 0 when it is a default or any of the flag's spellings could not be
// read.
func (p *EngineParams) explicitMaxNumSeqsFlag() int64 {
	if !p.MaxNumSeqsSet {
		return 0
	}
	for _, key := range maxNumSeqsKeys {
		if slices.Contains(p.Unresolved, key) {
			return 0
		}
	}
	return p.MaxNumSeqs
}

// BatchCap is the most sequences one engine rank runs at once, for pricing:
// ExplicitMaxNumSeqs when the flags state a cap, and MaxNumSeqs, stated or
// defaulted, when they do not. Unlike ExplicitMaxNumSeqs it is never 0 for a
// defaulted max_num_seqs, since it bounds what a replica is priced to serve,
// not which rows read as at the cap. 0 when neither is known.
//
// A stated --max-num-active-seqs is the cap even above the defaulted 256:
// vLLM refuses an active cap above max_num_seqs, so a running engine's real
// max_num_seqs is at least that high.
func (p *EngineParams) BatchCap() int64 {
	if p == nil {
		return 0
	}
	if stated := p.ExplicitMaxNumSeqs(); stated > 0 {
		return stated
	}
	return p.MaxNumSeqs
}

// maxNumSeqsKeys are the normalised flags either parser reads into
// MaxNumSeqs.
var maxNumSeqsKeys = []string{"max_num_seq", "max_num_seqs", "max_running_requests"}

// maxNumActiveSeqsKey is the normalised vLLM flag read into MaxNumActiveSeqs.
const maxNumActiveSeqsKey = "max_num_active_seqs"

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
		DataParallelSize:      1,
		PipelineParallelSize:  1,
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
		applyAll2AllEnv(&container, &params)

		// Collect all args from Command + Args, handling shell commands
		allArgs := collectArgs(container.Command, container.Args)

		// Resolved against THIS container's env, not a merged one. A
		// reference in a container's args is expanded from its own
		// environment, and merging two containers' envs would let a sidecar
		// supply a value the engine never sees.
		env := envValues(&container)
		parseArgs(allArgs, &params, env)
		applyPositionalModel(allArgs, &params, env)
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
	maxNumActiveSeqsKey:       {},
	"data_parallel_size":      {},
	"pipeline_parallel_size":  {},
	"speculative_config":      {},
	"kv_transfer_config":      {},
	"kv_offloading_size":      {},
	"kv_offloading_backend":   {},
	"kv_cache_memory_bytes":   {},
	"num_redundant_experts":   {},
	"eplb_config":             {},
	"all2all_backend":         {},
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
	"dp_size":              {},
	"pp_size":              {},
	"ep_size":              {},
	"ep":                   {},
	"expert_parallel_size": {},
	"hicache_size":         {},

	"speculative_algorithm":           {},
	"speculative_num_steps":           {},
	"speculative_num_draft_tokens":    {},
	"speculative_eagle_topk":          {},
	"speculative_draft_model_path":    {},
	"disaggregation_transfer_backend": {},
	"moe_a2a_backend":                 {},
}

// modelKeys are the flags that name the checkpoint. They are not in
// mappedValueKeys: one that cannot be read sets ModelUnresolved instead of
// Unresolved (see the field).
var modelKeys = map[string]struct{}{
	"model":      {},
	"model_path": {},
	"revision":   {},
}

// sharingOnlyKeys are the parallel sizes: one that cannot be read lands in
// SharingUnresolved, withholding sharing, rather than in Unresolved,
// withholding the variant's own restore. A parallel size is the value a
// multi-node group most often computes at start-up (see the field).
var sharingOnlyKeys = map[string]struct{}{
	"data_parallel_size":     {},
	"pipeline_parallel_size": {},
	"dp_size":                {},
	"pp_size":                {},
	"ep_size":                {},
	"ep":                     {},
	"expert_parallel_size":   {},
	// The memory-layout flags (EngineParams.MemoryLayout): an unreadable one
	// withholds sharing, not the variant's own restore.
	"kv_cache_memory_bytes": {},
	"num_redundant_experts": {},
	"eplb_config":           {},
	"all2all_backend":       {},
	"moe_a2a_backend":       {},
	envAll2AllKey:           {},
}

// envAll2AllKey is the key VLLM_ALL2ALL_BACKEND is recorded under, in
// MemoryLayout or SharingUnresolved.
const envAll2AllKey = "env.VLLM_ALL2ALL_BACKEND"

// The dotted spelling of vLLM's JSON flags, `--speculative-config.method
// eagle`: one key of the JSON per flag.
const (
	speculativeDotted = "speculative_config."
	kvConnectorDotted = "kv_transfer_config.kv_connector"
)

// isSharingOnlyKey reports whether an unreadable value for key withholds only
// sharing: see sharingOnlyKeys and SharingUnresolved.
func isSharingOnlyKey(key string) bool {
	if _, ok := sharingOnlyKeys[key]; ok {
		return true
	}
	return strings.HasPrefix(key, speculativeDotted) || key == kvConnectorDotted
}

// noteSharingUnresolved records key's value as written, replacing an earlier
// entry for the key: the last occurrence of a flag is the one the engine
// takes.
func noteSharingUnresolved(params *EngineParams, key, value string) {
	clearSharingUnresolved(params, key)
	params.SharingUnresolved = append(params.SharingUnresolved, key+"="+value)
	slices.Sort(params.SharingUnresolved)
}

// clearSharingUnresolved drops key's entry, for a later occurrence of the flag
// that did read.
func clearSharingUnresolved(params *EngineParams, key string) {
	params.SharingUnresolved = slices.DeleteFunc(params.SharingUnresolved, func(e string) bool {
		return strings.HasPrefix(e, key+"=")
	})
	if len(params.SharingUnresolved) == 0 {
		params.SharingUnresolved = nil
	}
}

// shortFlags are vLLM's single-dash aliases for flags this parser maps. No
// other single-dash token is read as a flag.
var shortFlags = map[string]string{
	"-tp": "--tensor-parallel-size",
	"-pp": "--pipeline-parallel-size",
	"-dp": "--data-parallel-size",
}

// longFlag returns tok as a long flag, a short alias spelled out; ok is false
// for a token that is not a flag this parser reads.
func longFlag(tok string) (string, bool) {
	if strings.HasPrefix(tok, "--") {
		return tok, true
	}
	name, rest, hasEq := strings.Cut(tok, "=")
	long, ok := shortFlags[name]
	switch {
	case !ok:
		return "", false
	case hasEq:
		return long + "=" + rest, true
	}
	return long, true
}

// isFlagToken reports whether tok is a flag rather than the value of the one
// before it. A negative number (`--chunked-prefill-size -1`) is a value.
func isFlagToken(tok string) bool {
	_, ok := longFlag(tok)
	return ok
}

// maxArithmeticTokens bounds how far joinArithmetic looks for the end of an
// expansion.
const maxArithmeticTokens = 16

// joinArithmetic rejoins a `$(( ... ))` expansion that the shell splitter cut
// at its spaces, so the value is the expression as written; i is the index of
// the last token read and is returned moved past what was consumed. An
// expansion with no closing `))` nearby is left as it was.
func joinArithmetic(value string, args []string, i int) (string, int) {
	if !strings.Contains(value, "$((") || strings.Contains(value, "))") {
		return value, i
	}
	parts := []string{value}
	for j := i + 1; j < len(args) && j <= i+maxArithmeticTokens; j++ {
		if isFlagToken(args[j]) {
			break
		}
		parts = append(parts, args[j])
		if strings.Contains(args[j], "))") {
			return strings.Join(parts, " "), j
		}
	}
	return value, i
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
		arg, isFlag := longFlag(args[i])
		if !isFlag {
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
			if i+1 < len(args) && !isFlagToken(args[i+1]) {
				value = args[i+1]
				hasValue = true
				i++ // consume the value
			}
			// Otherwise it's a boolean flag (no value)
		}

		if hasValue {
			value, i = joinArithmetic(value, args, i)
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
				if isSharingOnlyKey(key) {
					noteSharingUnresolved(params, key, value)
					continue
				}
				if key == maxNumActiveSeqsKey {
					// Unknown, so the batch cap falls back to max_num_seqs
					// (see EngineParams.MaxNumActiveSeqs); recorded below.
					params.MaxNumActiveSeqs = 0
				}
				if _, ok := modelKeys[key]; ok {
					params.ModelUnresolved = true
				}
				noteUnresolved(params, key)
				continue
			}
			value = resolved
		}

		switch apply(key, value, params) {
		case applyUnusable:
			// Recognised the flag and could not use the value.
			if isSharingOnlyKey(key) {
				noteSharingUnresolved(params, key, value)
			} else {
				noteUnresolved(params, key)
			}
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
			// Recognised and used -- over an earlier occurrence that did not
			// read, for a key that records one.
			clearSharingUnresolved(params, key)
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
		params.MaxNumSeqsSet = true
	case maxNumActiveSeqsKey:
		// vLLM v0.31+: caps the running batch below max_num_seqs. vLLM
		// requires at least 1, so anything less is not a value it runs.
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil || v < 1 {
			// Recorded in Unresolved; the cap falls back to max_num_seqs.
			// See MaxNumActiveSeqs.
			params.MaxNumActiveSeqs = 0
			return applyUnusable
		}
		params.MaxNumActiveSeqs = v
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
	case "data_parallel_size":
		return applyParallelSize(value, &params.DataParallelSize)
	case "pipeline_parallel_size":
		return applyParallelSize(value, &params.PipelineParallelSize)
	case "enable_expert_parallel":
		params.ExpertParallel = true
	case "no_enable_expert_parallel":
		params.ExpertParallel = false
	case "speculative_config":
		if !usableWord(value) {
			return applyUnusable
		}
		params.SpeculativeConfig = canonicalJSON(value)
	case "kv_transfer_config":
		if !usableWord(value) {
			return applyUnusable
		}
		params.KVConnector = kvConnectorOf(value)
	case "kv_offloading_size":
		return applyOffloadSize(value, params)
	case "kv_offloading_backend":
		if !usableWord(value) {
			return applyUnusable
		}
		params.KVOffloadingBackend = value
	case "enable_prefix_caching":
		params.PrefixCachingDisabled = false
	case "no_enable_prefix_caching":
		params.PrefixCachingDisabled = true
	case "model":
		applyModel(value, &params.ModelPath, params)
	case "revision":
		applyModel(value, &params.ModelRevision, params)
	case kvConnectorDotted:
		if !usableWord(value) {
			return applyUnusable
		}
		params.KVConnector = value
	case "kv_cache_memory_bytes", "all2all_backend":
		if !usableWord(value) {
			return applyUnusable
		}
		addLayoutPart(params, key, value)
	case "num_redundant_experts":
		v, err := strconv.Atoi(value)
		if err != nil || v < 0 {
			return applyUnusable
		}
		addLayoutPart(params, key, strconv.Itoa(v))
	case "eplb_config":
		if !usableWord(value) {
			return applyUnusable
		}
		addLayoutPart(params, key, canonicalJSON(value))
	case "enable_eplb":
		addLayoutPart(params, key, "true")
	case "no_enable_eplb":
		removeLayoutPart(params, "enable_eplb")
	default:
		// `--speculative-config.<key> <value>`, one JSON key per flag: kept
		// as sorted key=value pairs, as SGLang's --speculative-* flags are.
		// The same configuration written as one JSON object hashes
		// differently, the safe direction.
		if sub, ok := strings.CutPrefix(key, speculativeDotted); ok && sub != "" {
			if !usableWord(value) {
				return applyUnusable
			}
			addSpeculativePart(params, key, value)
			return applyOK
		}
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

// applyParallelSize reads a data- or pipeline-parallel size: a positive
// integer.
func applyParallelSize(value string, field *int) applyResult {
	v, err := strconv.Atoi(value)
	if err != nil || v < 1 {
		return applyUnusable
	}
	*field = v
	return applyOK
}

// applyOffloadSize reads a KV offloading size in GiB: finite and not negative.
func applyOffloadSize(value string, params *EngineParams) applyResult {
	v, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return applyUnusable
	}
	params.KVOffloadingSize = v
	return applyOK
}

// applyModel sets a checkpoint field, or marks the checkpoint unread when the
// flag has no value.
func applyModel(value string, field *string, params *EngineParams) {
	if !usableWord(value) {
		params.ModelUnresolved = true
		return
	}
	*field = value
}

// applyPositionalModel reads vLLM's positional model, `vllm serve <model>`,
// which wins over --model as it does in vLLM. A reference nothing resolves
// marks the checkpoint unread.
//
// So does a flag right after `serve` with no --model: the model is then a
// positional after the flags, where a boolean flag before it may have taken
// it as its value (`serve --enable-expert-parallel org/m`). Read as absent it
// would compare equal to every other checkpoint read as absent.
func applyPositionalModel(args []string, params *EngineParams, env map[string]string) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "serve" {
			continue
		}
		if strings.HasPrefix(args[i+1], "-") {
			if params.ModelPath == "" {
				params.ModelUnresolved = true
			}
			return
		}
		v, ok := resolveRefs(args[i+1], env)
		if !ok {
			params.ModelUnresolved = true
			return
		}
		applyModel(v, &params.ModelPath, params)
		return
	}
}

// canonicalJSON re-encodes a JSON value so that key order and spacing do not
// change it (encoding/json writes object keys sorted). A value that is not
// JSON is kept with its whitespace removed: still a stable text, and two
// configurations that differ only in formatting it then split, the safe
// direction.
func canonicalJSON(value string) string {
	var v any
	if err := json.Unmarshal([]byte(value), &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return strings.Join(strings.Fields(value), "")
}

// kvConnectorOf is the connector a --kv-transfer-config names. A config that
// is not JSON, or names none, is kept whole (canonicalJSON): it still tells
// two configurations apart.
func kvConnectorOf(value string) string {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(value), &cfg); err == nil {
		if c, ok := cfg["kv_connector"].(string); ok && c != "" {
			return c
		}
	}
	return canonicalJSON(value)
}

// addSpeculativePart records one SGLang --speculative-* flag in
// SpeculativeConfig, kept as sorted key=value pairs so flag order does not
// change it.
func addSpeculativePart(params *EngineParams, key, value string) {
	key = strings.TrimPrefix(key, "speculative_")
	var parts []string
	if params.SpeculativeConfig != "" {
		parts = strings.Split(params.SpeculativeConfig, ";")
	}
	parts = slices.DeleteFunc(parts, func(p string) bool { return strings.HasPrefix(p, key+"=") })
	parts = append(parts, key+"="+value)
	slices.Sort(parts)
	params.SpeculativeConfig = strings.Join(parts, ";")
}

// addLayoutPart records one memory-layout flag in MemoryLayout, replacing an
// earlier occurrence of the same key (the engine takes the last), kept as
// sorted key=value pairs so flag order does not change it.
func addLayoutPart(params *EngineParams, key, value string) {
	parts := layoutParts(params, key)
	parts = append(parts, key+"="+value)
	slices.Sort(parts)
	params.MemoryLayout = strings.Join(parts, ";")
}

// removeLayoutPart drops key from MemoryLayout, for a negative boolean form.
func removeLayoutPart(params *EngineParams, key string) {
	params.MemoryLayout = strings.Join(layoutParts(params, key), ";")
}

// layoutParts is MemoryLayout split into its pairs, without key's.
func layoutParts(params *EngineParams, key string) []string {
	var parts []string
	if params.MemoryLayout != "" {
		parts = strings.Split(params.MemoryLayout, ";")
	}
	return slices.DeleteFunc(parts, func(p string) bool { return strings.HasPrefix(p, key+"=") })
}

// applyAll2AllEnv reads VLLM_ALL2ALL_BACKEND from the container's env into
// MemoryLayout. A value set by valueFrom, or a literal that references
// something unresolvable, is not readable here: it lands in
// SharingUnresolved, which withholds sharing and not the variant's own
// restore. A value supplied by envFrom is not seen at all -- a known limit.
func applyAll2AllEnv(container *corev1.Container, params *EngineParams) {
	env := envValues(container)
	for _, e := range container.Env {
		if e.Name != "VLLM_ALL2ALL_BACKEND" {
			continue
		}
		if e.ValueFrom != nil {
			noteSharingUnresolved(params, envAll2AllKey, "valueFrom")
			continue
		}
		v, ok := resolveRefs(e.Value, env)
		if !ok || !usableWord(v) {
			noteSharingUnresolved(params, envAll2AllKey, e.Value)
			continue
		}
		clearSharingUnresolved(params, envAll2AllKey)
		addLayoutPart(params, envAll2AllKey, v)
	}
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
//   - MaxNumActiveSeqs (--max-num-active-seqs) caps the running batch below
//     max_num_seqs without changing the KV the engine profiles, so it moves
//     the saturated throughput a sibling measured, not k1. A figure measured
//     under one active cap is not taken as another's.
//   - MaxModelLen changes the KV a single sequence can occupy. It reaches
//     EffectiveMaxBatchedTokens only when chunked prefill is off (see
//     resolveEffectiveMaxBatchedTokens), so on the V1 path -- where chunked
//     prefill is the default -- it is otherwise invisible to this predicate.
//
// The parallel layout, expert parallelism, speculative decoding, the KV
// connector, KV offloading, prefix caching, the memory layout and the
// checkpoint are compared: each changes what one replica holds or how fast it
// drains, so a figure measured with one is not taken as the other's.
//
// AN UNREAD IDENTITY IS NOT AN EQUALITY. A pair where either side has an
// unread checkpoint (ModelUnresolved) or an identity flag it could not
// interpret (SharingUnresolved) is refused, even a configuration compared
// with itself. Two `$((LWS_GROUP_SIZE*8))` texts describe a 2x8 group and a
// 4x8 group alike, and two unread checkpoints an FP8 and a BF16 build of one
// model; comparing them as equal licensed a borrow across engines up to ~2x
// apart. The refusal costs a lower-only borrow, which leaves the variant on
// its own figure as if it had no sibling -- never a ceiling, which is found
// under IsCapacityClampCompatible. The reflexivity this gives up is the
// point: the predicate is "known to be the same engine", and these fields
// say it is not known. Unresolved keeps reflexivity (see
// TestCapacityCompatibilityStaysReflexive): it is the variant's own
// incomplete read, gated where sharing is decided.
//
// That makes this the equality for taking a sibling's figure AS this
// variant's -- a borrow, or a record that becomes the estimate. It is not the
// one for a CEILING: see IsCapacityClampCompatible, which estimateStoredCapacity
// uses so that comparing these fields did not remove a max clamp.
//
// EnforceEager is deliberately NOT compared: no CUDA graphs changes the
// inter-token latency, and so the ITL line, but it does not change how much
// KV fits or how many sequences run. A capacity record stays reusable across
// it. Keying a latency model on these params is a different equality, and it
// needs this one plus EnforceEager. The fingerprint also hashes
// MaxNumActiveSeqs, as this predicate compares it, so a rollout that changes
// only the active cap is a change of arguments (fingerprint.go).
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
	if p == nil || other == nil || !p.identityRead() || !other.identityRead() {
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
		p.MaxNumActiveSeqs == other.MaxNumActiveSeqs &&
		p.MaxModelLen == other.MaxModelLen &&
		p.WeightDtype == other.WeightDtype &&
		p.Quantization == other.Quantization &&
		p.MemoryLayout == other.MemoryLayout &&
		parallelSize(p.DataParallelSize) == parallelSize(other.DataParallelSize) &&
		parallelSize(p.PipelineParallelSize) == parallelSize(other.PipelineParallelSize) &&
		p.ExpertParallel == other.ExpertParallel &&
		p.SpeculativeConfig == other.SpeculativeConfig &&
		p.KVConnector == other.KVConnector &&
		p.KVOffloadingSize == other.KVOffloadingSize &&
		p.KVOffloadingBackend == other.KVOffloadingBackend &&
		p.PrefixCachingDisabled == other.PrefixCachingDisabled &&
		p.ModelPath == other.ModelPath &&
		p.ModelRevision == other.ModelRevision
}

// identityRead reports whether the fields that identify an engine for
// sharing were all read: the checkpoint, and every identity flag.
func (p *EngineParams) identityRead() bool {
	return !p.ModelUnresolved && len(p.SharingUnresolved) == 0
}

// IsCapacityClampCompatible is the looser equality the zero-replica estimate
// takes a sibling's live capacity under as a MAX clamp: the twelve fields
// IsCapacityCompatible compared before the layout, speculative decoding, the
// KV connector, offloading, prefix caching and the checkpoint were added to it.
//
// A clamp only lowers an estimate, so refusing one raises capacity and orders
// fewer replicas. Tightening the clamp's equality with the borrow's took the
// ceiling away from a parked variant whose live sibling differs only in, say,
// how its checkpoint path is spelled, or prefix caching -- and left it on the
// raw derived figure. A sibling that differs in those fields is a looser
// ceiling than an identical one, never a missing one.
//
// MaxNumActiveSeqs is not compared either, for the same reason. A sibling
// whose active cap is higher bounds this variant by a figure it may not reach,
// a looser ceiling; one whose cap is lower bounds it below what it may reach,
// which orders more replicas, the safe direction. Refusing it would leave the
// variant on the raw derived figure, which is the unsafe one.
func (p *EngineParams) IsCapacityClampCompatible(other *EngineParams) bool {
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
