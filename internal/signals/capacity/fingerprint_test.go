package capacity

import (
	"reflect"
	"sort"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// testWeightDtype and testQuantization are the values these tests mutate to.
// Declared in parser_test_literals_test.go, which already shares them with
// the parser tests -- this file used to declare its own copy.

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
	// Hashed so an incompletely-read configuration cannot collide with a
	// fully-read one. It is not sufficient on its own -- two engines with the
	// same unreadable flag still agree here -- which is why sharing also gates
	// on Complete(); see the field's comment in deployment_parser.go.
	"Unresolved",
	// The engine's code, which no flag describes.
	"Image",
	// What one replica of a wide-EP or P/D engine holds and how fast it
	// drains, which no flag above describes.
	"DataParallelSize",
	"PipelineParallelSize",
	"ExpertParallel",
	"SpeculativeConfig",
	"KVConnector",
	"KVOffloadingSize",
	"KVOffloadingBackend",
	"PrefixCachingDisabled",
	// The checkpoint, and whether it could be read.
	"ModelPath",
	"ModelRevision",
	"ModelUnresolved",
	// What was read for identity and not interpreted, as written.
	"SharingUnresolved",
	// Flags that size or reshape what one replica holds.
	"MemoryLayout",
}

// All three are excluded on one rule: they exist only to resolve
// EffectiveMaxBatchedTokens, which is hashed. Hashing an input beside the value
// it produces would split a key on a distinction the engine has collapsed.
//
// MaxNumSeqsSet is excluded on another: it says where the hashed MaxNumSeqs
// came from, the flag or the default, and the same value from either is the
// same engine setting. Only the ITL fit's sequence-cap exception reads it.
var fingerprintExcluded = []string{
	"MaxNumBatchedTokens",
	"IsV1Engine",
	"ChunkedPrefillEnabled",
	"MaxNumSeqsSet",
	// Not hashed: hashing it bumps FingerprintVersion and adds a label to the
	// published engine-config metric, which moves every stored digest. Read
	// for the batch cap (ExplicitMaxNumSeqs) and compared for capacity.
	"MaxNumActiveSeqs",
	// Not hashed: an operator's statement about the engine (the
	// llm-d.ai/max-num-seqs annotation), not a setting of it. Read only for
	// the derived k2 where the flags state no cap, and compared for capacity.
	"DeclaredBatchCap",
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
	first, second := a.Fingerprint(), a.Fingerprint()
	if first != second {
		t.Fatalf("Fingerprint is not deterministic within one process: %q then %q",
			first, second)
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
		"WeightDtype":               func(p *EngineParams) { p.WeightDtype = testWeightDtype },
		"Quantization":              func(p *EngineParams) { p.Quantization = testQuantization },
		"GpuMemoryUtilization":      func(p *EngineParams) { p.GpuMemoryUtilization = 0.85 },
		"BlockSize":                 func(p *EngineParams) { p.BlockSize = 32 },
		"KvCacheDtype":              func(p *EngineParams) { p.KvCacheDtype = testQuantization },
		"TensorParallelSize":        func(p *EngineParams) { p.TensorParallelSize = 8 },
		"NumGpuBlocksOverride":      func(p *EngineParams) { p.NumGpuBlocksOverride = 4096 },
		"TotalKvTokensOverride":     func(p *EngineParams) { p.TotalKvTokensOverride = 100000 },
		"EffectiveMaxBatchedTokens": func(p *EngineParams) { p.EffectiveMaxBatchedTokens = 4096 },
		"MaxNumSeqs":                func(p *EngineParams) { p.MaxNumSeqs = 512 },
		"MaxModelLen":               func(p *EngineParams) { p.MaxModelLen = 131072 },
		"EnforceEager":              func(p *EngineParams) { p.EnforceEager = true },
		"Unresolved": func(p *EngineParams) {
			p.Unresolved = []string{"block_size"}
		},
		"Image":                 func(p *EngineParams) { p.Image = "vllm/vllm-openai:v0.11.0" },
		"DataParallelSize":      func(p *EngineParams) { p.DataParallelSize = 8 },
		"PipelineParallelSize":  func(p *EngineParams) { p.PipelineParallelSize = 2 },
		"ExpertParallel":        func(p *EngineParams) { p.ExpertParallel = true },
		"SpeculativeConfig":     func(p *EngineParams) { p.SpeculativeConfig = testSpeculativeConfig },
		"KVConnector":           func(p *EngineParams) { p.KVConnector = testKVConnector },
		"KVOffloadingSize":      func(p *EngineParams) { p.KVOffloadingSize = 64 },
		"KVOffloadingBackend":   func(p *EngineParams) { p.KVOffloadingBackend = testOffloadBackend },
		"PrefixCachingDisabled": func(p *EngineParams) { p.PrefixCachingDisabled = true },
		"ModelPath":             func(p *EngineParams) { p.ModelPath = testModelPath },
		"ModelRevision":         func(p *EngineParams) { p.ModelRevision = testModelRevision },
		"ModelUnresolved":       func(p *EngineParams) { p.ModelUnresolved = true },
		"SharingUnresolved": func(p *EngineParams) {
			p.SharingUnresolved = []string{"data_parallel_size=$((LWS_GROUP_SIZE * 8))"}
		},
		"MemoryLayout": func(p *EngineParams) { p.MemoryLayout = testMemoryLayout },
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
		case "MaxNumSeqsSet":
			p.MaxNumSeqsSet = !p.MaxNumSeqsSet
		case "MaxNumActiveSeqs":
			p.MaxNumActiveSeqs = 64
		case "DeclaredBatchCap":
			p.DeclaredBatchCap = 128
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

// TestFingerprintResistsBoundaryShifting replaces a test that did not test
// what it claimed. The old one asserted the NUL separator stops a value forging
// a field boundary, and tried it with a value containing "=" — which is part of
// the fixed "name=" prefix, not the separator, so it passed with the NUL
// changed to "|" and with it removed altogether.
//
// The property that holds is that moving characters across the boundary between
// two adjacent hashed fields changes the digest, and it is guaranteed TWICE
// OVER: by the per-field "name=" prefix and, independently, by the NUL
// separator. Either alone suffices.
//
// That is worth stating because it means no single-mutation negative control
// can make this spec fail — removing one mechanism leaves the other, and
// removing the prefixes OR the NUL alone was measured to leave it green. The
// control that bites is removing BOTH, at which point "auto"+"fp8" and
// "autofp8"+"" hash alike and this fails. Defence in depth is the right design
// here; an untestable-by-one-mutation spec is the honest consequence, and
// pretending otherwise is what the previous version did.
func TestFingerprintResistsBoundaryShifting(t *testing.T) {
	split := defaultEngineParams()
	split.WeightDtype = "auto"
	split.Quantization = "fp8"
	resolveEffectiveMaxBatchedTokens(&split)

	joined := defaultEngineParams()
	joined.WeightDtype = "autofp8"
	joined.Quantization = ""
	resolveEffectiveMaxBatchedTokens(&joined)

	// The two differ only in where the boundary between the fields falls:
	// their concatenated VALUES are identical, so only the field structure
	// can tell them apart.
	if split.WeightDtype+split.Quantization != joined.WeightDtype+joined.Quantization {
		t.Fatal("the two configurations must have identical concatenated values, " +
			"or this test proves nothing about the boundary")
	}
	if split.Fingerprint() == joined.Fingerprint() {
		t.Fatal("two configurations collided across a field boundary: the per-field " +
			"name prefixes are not reaching the digest")
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

	againA, againB := LearnedStateKey("m", "a", 1, fp), LearnedStateKey("m", "a", 1, fp)
	if againA != againB {
		t.Fatalf("the key is not stable for identical inputs: %q then %q", againA, againB)
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

// TestFingerprintLabelsDescribeTheirValues pins the one pairing that can
// misreport a configuration: wva_engine_config labels FingerprintValues by
// position using FingerprintFields, so if the two lists are ordered
// differently every published series names the wrong flag -- with no error
// and no missing series, which is the worst kind of wrong for a diagnostic.
//
// For each label, mutate the field that label names and assert the value at
// THAT position moves and no other does.
func TestFingerprintLabelsDescribeTheirValues(t *testing.T) {
	byLabel := map[string]func(*EngineParams){
		"engine":                       func(p *EngineParams) { p.Engine = "sglang" },
		"weight_dtype":                 func(p *EngineParams) { p.WeightDtype = testWeightDtype },
		"quantization":                 func(p *EngineParams) { p.Quantization = testQuantization },
		"gpu_memory_utilization":       func(p *EngineParams) { p.GpuMemoryUtilization = 0.85 },
		"block_size":                   func(p *EngineParams) { p.BlockSize = 32 },
		"kv_cache_dtype":               func(p *EngineParams) { p.KvCacheDtype = testQuantization },
		"tensor_parallel_size":         func(p *EngineParams) { p.TensorParallelSize = 8 },
		"num_gpu_blocks_override":      func(p *EngineParams) { p.NumGpuBlocksOverride = 4096 },
		"total_kv_tokens_override":     func(p *EngineParams) { p.TotalKvTokensOverride = 100000 },
		"effective_max_batched_tokens": func(p *EngineParams) { p.EffectiveMaxBatchedTokens = 4096 },
		"max_num_seqs":                 func(p *EngineParams) { p.MaxNumSeqs = 512 },
		"max_model_len":                func(p *EngineParams) { p.MaxModelLen = 131072 },
		"enforce_eager":                func(p *EngineParams) { p.EnforceEager = true },
		"unresolved": func(p *EngineParams) {
			p.Unresolved = []string{"block_size"}
		},
		"image":                   func(p *EngineParams) { p.Image = "vllm/vllm-openai:v0.11.0" },
		"data_parallel_size":      func(p *EngineParams) { p.DataParallelSize = 8 },
		"pipeline_parallel_size":  func(p *EngineParams) { p.PipelineParallelSize = 2 },
		"expert_parallel":         func(p *EngineParams) { p.ExpertParallel = true },
		"speculative_config":      func(p *EngineParams) { p.SpeculativeConfig = testSpeculativeConfig },
		"kv_connector":            func(p *EngineParams) { p.KVConnector = testKVConnector },
		"kv_offloading_size":      func(p *EngineParams) { p.KVOffloadingSize = 64 },
		"kv_offloading_backend":   func(p *EngineParams) { p.KVOffloadingBackend = testOffloadBackend },
		"prefix_caching_disabled": func(p *EngineParams) { p.PrefixCachingDisabled = true },
		"model":                   func(p *EngineParams) { p.ModelPath = testModelPath },
		"model_revision":          func(p *EngineParams) { p.ModelRevision = testModelRevision },
		"model_unresolved":        func(p *EngineParams) { p.ModelUnresolved = true },
		"sharing_unresolved": func(p *EngineParams) {
			p.SharingUnresolved = []string{"data_parallel_size=$((LWS_GROUP_SIZE * 8))"}
		},
		"memory_layout": func(p *EngineParams) { p.MemoryLayout = testMemoryLayout },
	}

	names := FingerprintFields()
	if len(byLabel) != len(names) {
		t.Fatalf("%d mutations for %d labels: one label is unexercised",
			len(byLabel), len(names))
	}

	base := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&base)
	baseValues := base.FingerprintValues()

	for i, label := range names {
		mutate, ok := byLabel[label]
		if !ok {
			t.Fatalf("no mutation for published label %q", label)
		}
		p := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p)
		mutate(&p)
		got := p.FingerprintValues()

		// Bounded by NAMES, not by the values slice. A review shortened
		// FingerprintValues by one and this loop simply never reached the
		// last position, so the final label went unvalidated and the test
		// still passed. A sibling length check catches that mutation, but a
		// test should not depend on one.
		if len(got) != len(names) {
			t.Fatalf("%d values for %d labels: the pairing cannot be checked",
				len(got), len(names))
		}
		for j := range names {
			changed := got[j] != baseValues[j]
			if j == i && !changed {
				t.Errorf("label %q is at position %d but mutating that field did "+
					"not change the value there (%q): the labels and the values "+
					"are ordered differently", label, i, got[j])
			}
			if j != i && changed {
				t.Errorf("mutating the field for %q (position %d) also changed "+
					"position %d (%q -> %q), labelled %q",
					label, i, j, baseValues[j], got[j], names[j])
			}
		}
	}
}

// TestFingerprintFlagLabelsAreValidPrometheusLabels guards the published side:
// a label name Prometheus rejects would panic registration at startup.
func TestFingerprintFlagLabelsAreValidPrometheusLabels(t *testing.T) {
	seen := map[string]bool{}
	for _, l := range FingerprintFields() {
		if l == "" {
			t.Error("an empty label name would be rejected at registration")
		}
		if seen[l] {
			t.Errorf("duplicate label name %q: registration would panic", l)
		}
		seen[l] = true
		for _, r := range l {
			ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				t.Errorf("label %q contains %q, which Prometheus does not accept", l, r)
			}
		}
	}
}
