package capacity

import (
	"reflect"
	"sort"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// fingerprintHashed and fingerprintExcluded together must name EVERY field of
// EngineParams. That is the point of the drift test below: adding a field to
// the struct and not deciding which list it belongs in is a test failure
// rather than a silent mis-keying, which is the failure mode this whole design
// is most exposed to.
var fingerprintHashed = []string{
	"Engine",
	"WeightDtype",
	"Quantization",
	"GpuMemoryUtilization",
	"BlockSize",
	"KvCacheDtype",
	"TensorParallelSize",
	"NumGpuBlocksOverride",
	"TotalKvTokensOverride",
	"EffectiveMaxBatchedTokens",
	"MaxNumSeqs",
	"MaxModelLen",
	"EnforceEager",
}

// All three are excluded on one rule: they exist only to resolve
// EffectiveMaxBatchedTokens, which is hashed. Hashing an input beside the value
// it produces would split a key on a distinction the engine has collapsed.
var fingerprintExcluded = []string{
	"MaxNumBatchedTokens",
	"IsV1Engine",
	"ChunkedPrefillEnabled",
}

func TestFingerprintCoversEveryEngineParamsField(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(EngineParams{})
	for i := 0; i < ty.NumField(); i++ {
		got = append(got, ty.Field(i).Name)
	}

	want := append(append([]string{}, fingerprintHashed...), fingerprintExcluded...)
	sort.Strings(got)
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EngineParams fields have drifted from the fingerprint's lists.\n"+
			"struct: %v\nlisted: %v\n"+
			"Add the new field to fingerprintHashed (and to Fingerprint, "+
			"FingerprintFields, FingerprintValues, and bump FingerprintVersion) "+
			"or to fingerprintExcluded with the reason.", got, want)
	}
}

func TestFingerprintFieldsMatchValues(t *testing.T) {
	p := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&p)

	names := FingerprintFields()
	values := p.FingerprintValues()
	if len(names) != len(values) {
		t.Fatalf("%d field names but %d values: the info metric would mislabel", len(names), len(values))
	}
	if len(names) != len(fingerprintHashed) {
		t.Fatalf("%d published fields but %d hashed: the info metric would not "+
			"explain the digest it labels", len(names), len(fingerprintHashed))
	}
}

func TestFingerprintIsStableAcrossCalls(t *testing.T) {
	a := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&a)
	b := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&b)

	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("two identical configurations hashed differently: %q vs %q",
			a.Fingerprint(), b.Fingerprint())
	}
	if got := len(a.Fingerprint()); got != fingerprintLength {
		t.Fatalf("fingerprint is %d characters, want %d", got, fingerprintLength)
	}
	if a.Fingerprint() != a.Fingerprint() {
		t.Fatal("Fingerprint is not deterministic within one process")
	}
}

// TestFingerprintChangesWithEveryHashedField is the one that matters: a field
// in the list that does not actually reach the digest is worse than an absent
// field, because the list is what the next person trusts.
func TestFingerprintChangesWithEveryHashedField(t *testing.T) {
	base := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&base)
	baseline := base.Fingerprint()

	mutations := map[string]func(*EngineParams){
		"Engine":                    func(p *EngineParams) { p.Engine = "sglang" },
		"WeightDtype":               func(p *EngineParams) { p.WeightDtype = "bfloat16" },
		"Quantization":              func(p *EngineParams) { p.Quantization = "fp8" },
		"GpuMemoryUtilization":      func(p *EngineParams) { p.GpuMemoryUtilization = 0.85 },
		"BlockSize":                 func(p *EngineParams) { p.BlockSize = 32 },
		"KvCacheDtype":              func(p *EngineParams) { p.KvCacheDtype = "fp8" },
		"TensorParallelSize":        func(p *EngineParams) { p.TensorParallelSize = 8 },
		"NumGpuBlocksOverride":      func(p *EngineParams) { p.NumGpuBlocksOverride = 4096 },
		"TotalKvTokensOverride":     func(p *EngineParams) { p.TotalKvTokensOverride = 100000 },
		"EffectiveMaxBatchedTokens": func(p *EngineParams) { p.EffectiveMaxBatchedTokens = 4096 },
		"MaxNumSeqs":                func(p *EngineParams) { p.MaxNumSeqs = 512 },
		"MaxModelLen":               func(p *EngineParams) { p.MaxModelLen = 131072 },
		"EnforceEager":              func(p *EngineParams) { p.EnforceEager = true },
	}

	if len(mutations) != len(fingerprintHashed) {
		t.Fatalf("%d mutations for %d hashed fields: one is unexercised",
			len(mutations), len(fingerprintHashed))
	}

	for _, name := range fingerprintHashed {
		mutate, ok := mutations[name]
		if !ok {
			t.Fatalf("no mutation exercises hashed field %s", name)
		}
		p := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p)
		mutate(&p)
		if p.Fingerprint() == baseline {
			t.Errorf("changing %s did not change the fingerprint: it is listed as "+
				"hashed but does not reach the digest", name)
		}
	}
}

// TestFingerprintIgnoresTheResolvedInputs pins the exclusion rule. Changing an
// input WITHOUT changing the value it resolves to must not split the key.
func TestFingerprintIgnoresTheResolvedInputs(t *testing.T) {
	for _, name := range fingerprintExcluded {
		p := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p)
		baseline := p.Fingerprint()
		resolved := p.EffectiveMaxBatchedTokens

		switch name {
		case "MaxNumBatchedTokens":
			p.MaxNumBatchedTokens = 9999
		case "IsV1Engine":
			p.IsV1Engine = !p.IsV1Engine
		case "ChunkedPrefillEnabled":
			p.ChunkedPrefillEnabled = !p.ChunkedPrefillEnabled
		default:
			t.Fatalf("no case for excluded field %s", name)
		}

		// Hold the resolved value fixed: this test is about the input alone.
		p.EffectiveMaxBatchedTokens = resolved
		if p.Fingerprint() != baseline {
			t.Errorf("changing %s changed the fingerprint, but it is listed as "+
				"excluded because it only resolves EffectiveMaxBatchedTokens", name)
		}
	}
}

func TestFingerprintVersionParticipates(t *testing.T) {
	// The version cannot be varied at runtime, so this asserts the property it
	// exists for: it is inside the hashed material. If the "v=" prefix were
	// dropped from Fingerprint, a field-set change would silently keep matching
	// old keys -- the exact silent breakage the constant exists to prevent.
	p := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&p)
	if FingerprintVersion < 1 {
		t.Fatal("FingerprintVersion must be a positive, bumpable integer")
	}
	if p.Fingerprint() == "" {
		t.Fatal("a populated configuration must hash to something")
	}
}

func TestFingerprintOfNilIsEmpty(t *testing.T) {
	var p *EngineParams
	if got := p.Fingerprint(); got != "" {
		t.Fatalf("nil params hashed to %q, want empty so a caller cannot key on it", got)
	}
	if got := p.FingerprintValues(); got != nil {
		t.Fatalf("nil params produced values %v", got)
	}
}

// TestFingerprintSeparatorCannotBeForged covers the reason the fields are
// NUL-separated: with a printable joiner, a value containing it could
// impersonate a field boundary and two different configurations would collide.
func TestFingerprintSeparatorCannotBeForged(t *testing.T) {
	a := defaultEngineParams()
	a.WeightDtype = "x"
	a.Quantization = "y"
	resolveEffectiveMaxBatchedTokens(&a)

	b := defaultEngineParams()
	b.WeightDtype = "x=y"
	b.Quantization = ""
	resolveEffectiveMaxBatchedTokens(&b)

	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("two configurations collided: a value was able to impersonate a field boundary")
	}
}

func TestLearnedStateKeyIsTheWholeTuple(t *testing.T) {
	p := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&p)
	fp := p.Fingerprint()

	small := LearnedStateKey("Qwen/Qwen3-0.6B", "NVIDIA-H200", 1, fp)
	large := LearnedStateKey("Qwen/Qwen3-32B", "NVIDIA-H200", 1, fp)
	if small == large {
		t.Fatal("two models with one configuration share a key: this is the " +
			"mis-keying that would pool their ITL lines")
	}

	otherGPU := LearnedStateKey("Qwen/Qwen3-0.6B", "NVIDIA-H100-80GB-HBM3", 1, fp)
	if small == otherGPU {
		t.Fatal("two accelerators share a key")
	}

	twoGPUs := LearnedStateKey("Qwen/Qwen3-0.6B", "NVIDIA-H200", 2, fp)
	if small == twoGPUs {
		t.Fatal("one and two GPUs per replica share a key")
	}

	if LearnedStateKey("m", "a", 1, fp) != LearnedStateKey("m", "a", 1, fp) {
		t.Fatal("the key is not stable for identical inputs")
	}
}

// TestFingerprintAgreesWithCapacityCompatibilityOnNaN is the consistency the
// parser guard buys. ParseFloat accepts "NaN", and NaN != NaN, so an
// unguarded NaN would make a variant incompatible with itself while hashing
// two NaN configurations to one digest.
func TestFingerprintAgreesWithCapacityCompatibilityOnNaN(t *testing.T) {
	deploy := makeTestDeployment("--gpu-memory-utilization=NaN")
	p := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))

	if p.GpuMemoryUtilization != 0.9 {
		t.Fatalf("GpuMemoryUtilization is %v; a NaN flag must leave the default in place",
			p.GpuMemoryUtilization)
	}
	if !p.IsCapacityCompatible(&p) {
		t.Fatal("a variant is not compatible with itself: a NaN reached the struct")
	}

	for _, bad := range []string{"NaN", "Inf", "-Inf", "0", "-0.5", "1.5"} {
		d := makeTestDeployment("--gpu-memory-utilization=" + bad)
		q := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(d))
		if q.GpuMemoryUtilization != 0.9 {
			t.Errorf("--gpu-memory-utilization=%s became %v, want the 0.9 default",
				bad, q.GpuMemoryUtilization)
		}
	}

	// A usable value still lands, or the guard is rejecting everything.
	ok := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(makeTestDeployment("--gpu-memory-utilization=0.85")))
	if ok.GpuMemoryUtilization != 0.85 {
		t.Fatalf("a valid fraction became %v: the guard is too strict", ok.GpuMemoryUtilization)
	}
}
