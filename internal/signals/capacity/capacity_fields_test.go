package capacity

import (
	"reflect"
	"slices"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// Every EngineParams field is either COMPARED by IsCapacityCompatible or
// listed here as deliberately ignored, with the reason.
//
// The predicate authorises one variant to be priced from another's measured
// capacity, so a field added to the struct and silently left out of it is a
// false equality waiting to happen -- two configurations that differ in the
// new field compare equal and one lends the other a figure measured under
// different conditions. Nothing caught that: the predicate is a hand-written
// conjunction, and adding a field to the struct does not break it.
//
// Reflection over the struct is what makes forgetting impossible. Add a field
// and this test fails until it is put in one list or the other.
var (
	capacityCompared = []string{
		"Engine",
		"GpuMemoryUtilization",
		"BlockSize",
		"KvCacheDtype",
		"TensorParallelSize",
		"NumGpuBlocksOverride",
		"TotalKvTokensOverride",
		"EffectiveMaxBatchedTokens",
		"MaxNumSeqs",
		"MaxModelLen",
		"WeightDtype",
		"Quantization",
	}
	// Each of these needs its reason, because "ignored" is the dangerous half
	// of the list and an unexplained entry here is how a field gets dropped.
	capacityIgnored = map[string]string{
		// Not a capacity input at all: it changes LATENCY, not how many tokens
		// fit. The latency key (the sharing fingerprint, in a later change)
		// does compare it, which is why the two equalities deliberately differ.
		"EnforceEager": "affects latency, not token capacity",
		// Resolved INTO EffectiveMaxBatchedTokens, which is compared. Comparing
		// both would reject two configurations that resolve to the same budget
		// by different routes.
		"MaxNumBatchedTokens": "folded into EffectiveMaxBatchedTokens",
		// Same: these two decide how EffectiveMaxBatchedTokens is resolved and
		// are not independently meaningful once it is.
		"IsV1Engine":            "decides how EffectiveMaxBatchedTokens resolves",
		"ChunkedPrefillEnabled": "decides how EffectiveMaxBatchedTokens resolves",
		// Provenance, not configuration. Two records that differ only in what
		// could not be READ describe the same configuration as far as every
		// compared field goes; see TestCapacityCompatibilityStaysReflexive for
		// why including it would make the equality non-reflexive.
		"Unresolved": "provenance; including it breaks reflexivity",
	}
)

func TestCapacityCompatibilityCoversEveryEngineParamsField(t *testing.T) {
	typ := reflect.TypeOf(EngineParams{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		_, ignored := capacityIgnored[name]
		compared := slices.Contains(capacityCompared, name)
		switch {
		case compared && ignored:
			t.Errorf("%s is in both lists; it is one or the other", name)
		case !compared && !ignored:
			t.Errorf("EngineParams.%s is in neither list. Add it to "+
				"IsCapacityCompatible and to capacityCompared, or to "+
				"capacityIgnored with the reason it cannot change how many "+
				"tokens fit on a replica. Leaving it out silently makes two "+
				"different configurations compare EQUAL, which licenses one "+
				"variant to be priced from the other's measurement", name)
		}
	}

	// And the lists do not name fields that no longer exist.
	for _, name := range capacityCompared {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("capacityCompared names %q, which EngineParams does not have", name)
		}
	}
	for name := range capacityIgnored {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("capacityIgnored names %q, which EngineParams does not have", name)
		}
	}
}

// The list is only worth anything if each named field actually reaches the
// predicate. A hand-maintained list can claim a field the conjunction forgot.
func TestEveryComparedFieldChangesTheAnswer(t *testing.T) {
	base := func() *EngineParams {
		return &EngineParams{
			Engine:                    inferenceengine.EngineVLLM,
			GpuMemoryUtilization:      0.85,
			BlockSize:                 128,
			KvCacheDtype:              "auto",
			WeightDtype:               testWeightDtype,
			Quantization:              testQuantization,
			TensorParallelSize:        2,
			NumGpuBlocksOverride:      1024,
			TotalKvTokensOverride:     200000,
			EffectiveMaxBatchedTokens: 8192,
			MaxNumSeqs:                512,
			MaxModelLen:               16384,
		}
	}
	// One differing value per compared field, each genuinely different from
	// base() so a no-op mutation cannot pass.
	diff := map[string]func(*EngineParams){
		"Engine":                    func(p *EngineParams) { p.Engine = inferenceengine.EngineSGLang },
		"GpuMemoryUtilization":      func(p *EngineParams) { p.GpuMemoryUtilization = 0.9 },
		"BlockSize":                 func(p *EngineParams) { p.BlockSize = 16 },
		"KvCacheDtype":              func(p *EngineParams) { p.KvCacheDtype = "fp8" },
		"TensorParallelSize":        func(p *EngineParams) { p.TensorParallelSize = 4 },
		"NumGpuBlocksOverride":      func(p *EngineParams) { p.NumGpuBlocksOverride = 2048 },
		"TotalKvTokensOverride":     func(p *EngineParams) { p.TotalKvTokensOverride = 300000 },
		"EffectiveMaxBatchedTokens": func(p *EngineParams) { p.EffectiveMaxBatchedTokens = 16384 },
		"MaxNumSeqs":                func(p *EngineParams) { p.MaxNumSeqs = 256 },
		"MaxModelLen":               func(p *EngineParams) { p.MaxModelLen = 8192 },
		"WeightDtype":               func(p *EngineParams) { p.WeightDtype = "float16" },
		"Quantization":              func(p *EngineParams) { p.Quantization = "awq" },
	}

	for _, name := range capacityCompared {
		mutate, ok := diff[name]
		if !ok {
			t.Errorf("no differing value defined for compared field %q; add one or "+
				"this field's membership of capacityCompared is unverified", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			a, b := base(), base()
			mutate(b)
			if reflect.DeepEqual(a, b) {
				t.Fatalf("the mutation for %s changed nothing, so the assertion "+
					"below would pass whatever the predicate does", name)
			}
			if a.IsCapacityCompatible(b) {
				t.Errorf("two configurations differing only in %s compared equal: "+
					"the field is in capacityCompared but does not reach "+
					"IsCapacityCompatible", name)
			}
		})
	}
}
