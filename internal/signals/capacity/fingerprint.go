package capacity

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// FingerprintVersion is hashed into every fingerprint, so changing the set of
// fields below invalidates every stored key rather than silently failing to
// match one.
//
// That is the whole reason it exists. The hash's input set is a wire format the
// moment anything stores a fingerprint, and the breakage from adding a field is
// silent: old keys simply stop matching, with no error and no missing series,
// and the controller relearns while looking healthy. Bump this in the same
// commit as any change to the field list, and treat "every fingerprint starts
// cold once" as the expected cost.
//
// AND IT COVERS MORE THAN THE FIELD LIST. Changing how a hashed value is
// DERIVED changes every digest just as surely as adding a field, and this PR
// did it twice after bumping to 2 -- splitting the unresolved key sets per
// engine, and recording string-flag failures. Two builds both stamped v=2
// therefore produce different digests for the same Deployment. Nothing
// persists a digest across a restart today, so no further bump is owed; the
// rule to carry forward is that the version tracks the field list and the
// values' derivation together.
//
// Version 2 adds the unresolved-flag set. Measured on a cluster: an
// llm-d/llmdbench Deployment passes `--block-size $VLLM_BLOCK_SIZE`, the
// parser could not read it, the field kept its default of 16 against a real
// 128, and the digest asserted an equality it had never verified. The set is
// hashed so an incompletely-read configuration can never collide with a
// fully-read one.
const FingerprintVersion = 2

// fingerprintLength is how much of the digest reaches the label. 16 hex
// characters is 64 bits: for the few hundred distinct engine configurations a
// cluster plausibly runs, a collision is not a risk worth four more
// characters of unreadability.
const fingerprintLength = 16

// Fingerprint returns a stable digest of the engine configuration -- the
// launch flags and nothing else.
//
// What it deliberately does NOT cover: the model, the accelerator and the GPU
// count. Those are part of the KEY that learned state is filed under, as
// labels beside this digest, because an operator queries by them ("what has
// this model learned", "this config on H200 versus H100") and a hash makes
// every such question a join. Composing the key is the caller's job and must
// be done in one place; a fingerprint on its own identifies a configuration,
// never a thing being served.
//
// Relative to IsCapacityCompatible this adds exactly one field, EnforceEager,
// and the asymmetry is deliberate. No CUDA graphs does not change how much KV
// fits or how many sequences run, so a capacity record survives it -- but it
// does change the inter-token latency, and this digest also keys a latency
// model. Including it costs only that two engines differing in --enforce-eager
// each learn their own ITL line; excluding it would pool them onto one line
// that describes neither. The safe direction needs no measurement.
//
// Three EngineParams fields are excluded on one rule: MaxNumBatchedTokens,
// IsV1Engine and ChunkedPrefillEnabled exist only to resolve
// EffectiveMaxBatchedTokens, which IS hashed. Hashing an input beside the
// value it produces would split a key on a distinction the engine has already
// collapsed.
func (p *EngineParams) Fingerprint() string {
	if p == nil {
		return ""
	}

	// Fixed order. Reordering changes every digest, so it is as much a part of
	// the wire format as the field list, and a change to either is a version
	// bump.
	//
	// The names here are internal salt: they keep two fields with the same
	// value from being interchangeable, and nothing reads them. They are
	// deliberately NOT taken from constants.EngineConfigFlagLabels, so that
	// relabelling the published metric cannot silently reshuffle every stored
	// digest. The pairing that must hold is between FingerprintFields and
	// FingerprintValues, which is what labels the metric, and a test pins it.
	fields := []string{
		"v=" + strconv.Itoa(FingerprintVersion),
		"engine=" + string(p.Engine),
		"weight_dtype=" + p.WeightDtype,
		"quantization=" + p.Quantization,
		"gpu_memory_utilization=" + canonicalFloat(p.GpuMemoryUtilization),
		"block_size=" + strconv.FormatInt(p.BlockSize, 10),
		"kv_cache_dtype=" + p.KvCacheDtype,
		"tensor_parallel_size=" + strconv.Itoa(p.TensorParallelSize),
		"num_gpu_blocks_override=" + strconv.FormatInt(p.NumGpuBlocksOverride, 10),
		"total_kv_tokens_override=" + strconv.FormatInt(p.TotalKvTokensOverride, 10),
		"effective_max_batched_tokens=" + strconv.FormatInt(p.EffectiveMaxBatchedTokens, 10),
		"max_num_seqs=" + strconv.FormatInt(p.MaxNumSeqs, 10),
		"max_model_len=" + strconv.FormatInt(p.MaxModelLen, 10),
		"enforce_eager=" + strconv.FormatBool(p.EnforceEager),
		// WHICH FIELDS ABOVE ARE REAL. Every value in this list is either the
		// engine's setting or a default that silently replaced something
		// unreadable, and until this field existed nothing could tell the two
		// apart -- so two engines differing on exactly the flag neither could
		// be read for hashed identically. That is a false equality, and this
		// digest licenses one variant to borrow another's measured latency
		// line, so a false equality is the dangerous direction.
		//
		// Hashing the set is necessary but NOT sufficient, and the difference
		// matters: it stops an incomplete read colliding with a complete one,
		// but two engines with the SAME unreadable flag still agree here while
		// their real values may differ. So the digest alone cannot authorise
		// sharing -- callers gate on Complete() as well. Comma-joined over a
		// sorted, deduplicated list, so it is stable.
		"unresolved=" + strings.Join(p.Unresolved, ","),
	}

	h := sha256.New()
	for _, f := range fields {
		// What actually prevents one value from impersonating a field
		// boundary is the fixed "name=" prefix on every field: a value can
		// contain any bytes it likes and still cannot forge another field's
		// name at the position that field occupies.
		//
		// The NUL is belt and braces on top of that -- it cannot appear in a
		// container arg, so it is unforgeable -- and an earlier comment here
		// credited it with the whole job. That was wrong, and a test written
		// to the wrong claim passed with the NUL removed entirely. The
		// property that is actually tested is boundary-shifting between
		// adjacent fields; see TestFingerprintResistsBoundaryShifting.
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:fingerprintLength]
}

// canonicalFloat renders a float so that equal values always produce equal
// text. 'f' with precision -1 gives the shortest form that round-trips, so 0.9
// is "0.9" and not "0.90000000000000002".
//
// NaN and the infinities are folded to fixed strings for a reason worth
// stating: the parser rejects them (applyParam), so they should never reach
// here. If one ever does, a stable digest is still better than a digest that
// varies, and the fold keeps this function total. It does leave one honest
// asymmetry -- IsCapacityCompatible compares with ==, under which NaN does not
// equal itself, so two NaN configurations would share a fingerprint while the
// predicate called them incompatible. Guarding at the parser is what keeps the
// two consistent.
func canonicalFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "+inf"
	case math.IsInf(f, -1):
		return "-inf"
	default:
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
}

// FingerprintFields returns the published label names for the hashed fields,
// positionally paired with FingerprintValues. That pairing is what labels
// wva_engine_config, so getting it wrong misreports a configuration rather
// than mis-keying one; TestFingerprintLabelsDescribeTheirValues pins it.
func FingerprintFields() []string {
	return append([]string{}, constants.EngineConfigFlagLabels...)
}

// FingerprintValues returns the hashed values, in the same order as
// FingerprintFields, as the info metric publishes them. Paired with the field
// list so the two cannot drift apart.
func (p *EngineParams) FingerprintValues() []string {
	if p == nil {
		return nil
	}
	return []string{
		string(p.Engine),
		p.WeightDtype,
		p.Quantization,
		canonicalFloat(p.GpuMemoryUtilization),
		strconv.FormatInt(p.BlockSize, 10),
		p.KvCacheDtype,
		strconv.Itoa(p.TensorParallelSize),
		strconv.FormatInt(p.NumGpuBlocksOverride, 10),
		strconv.FormatInt(p.TotalKvTokensOverride, 10),
		strconv.FormatInt(p.EffectiveMaxBatchedTokens, 10),
		strconv.FormatInt(p.MaxNumSeqs, 10),
		strconv.FormatInt(p.MaxModelLen, 10),
		strconv.FormatBool(p.EnforceEager),
		strings.Join(p.Unresolved, ","),
	}
}

// LearnedStateKey composes the key that learned per-configuration state is
// filed under. Every caller must use this rather than the fingerprint alone:
// the fingerprint describes a configuration, and two different models can
// share one. Keying on it by itself would pool a 0.6B and a 32B model onto a
// single ITL line, which is the worst mis-keying available here.
func LearnedStateKey(modelID, accelerator string, gpusPerReplica int, fingerprint string) string {
	return strings.Join([]string{
		modelID,
		accelerator,
		strconv.Itoa(gpusPerReplica),
		fingerprint,
	}, "|")
}
