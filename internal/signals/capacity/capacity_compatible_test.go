package capacity

import (
	"math"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// IsCapacityCompatible is an EQUALITY, so it has to be reflexive.
//
// The first version of the Unresolved work gated it on Complete(), which made
// a variant whose --gpu-memory-utilization could not be read stop being
// compatible with ITSELF -- and the capacity path reads that predicate to
// decide whether a zero-replica variant may take a sibling's measured figure,
// so a non-reflexive answer there is not a philosophical problem but a
// variant that cannot match its own record.
//
// Nothing pinned it in this package: the spec that forbade it lived beside the
// fingerprint tests, which are not part of this change. So it is written here,
// against the predicate it is actually about.
func TestCapacityCompatibilityStaysReflexive(t *testing.T) {
	base := func() *EngineParams {
		return &EngineParams{
			Engine:                    inferenceengine.EngineVLLM,
			GpuMemoryUtilization:      0.85,
			BlockSize:                 128,
			KvCacheDtype:              "auto",
			WeightDtype:               testWeightDtype,
			Quantization:              testQuantization,
			TensorParallelSize:        2,
			MaxNumSeqs:                512,
			MaxModelLen:               16384,
			EffectiveMaxBatchedTokens: 8192,
		}
	}

	t.Run("a fully-read configuration matches itself", func(t *testing.T) {
		p := base()
		if !p.IsCapacityCompatible(p) {
			t.Fatal("a configuration must be compatible with itself; if this fails " +
				"nothing below proves anything")
		}
	})

	t.Run("an INCOMPLETE configuration still matches itself", func(t *testing.T) {
		// The regression. Re-adding a Complete() gate to the predicate fails
		// exactly here.
		p := base()
		p.Unresolved = []string{"block_size", "gpu_memory_utilization"}
		if p.Complete() {
			t.Fatal("precondition: this fixture must be incomplete")
		}
		if !p.IsCapacityCompatible(p) {
			t.Error("an incompletely-read configuration stopped being compatible " +
				"with itself -- the predicate has been gated on Complete() again, " +
				"which makes the equality non-reflexive")
		}
	})

	t.Run("and Unresolved is not part of the comparison", func(t *testing.T) {
		// Two records that differ ONLY in what could not be read are still the
		// same capacity configuration: every field the predicate reads is
		// equal. This is the honest statement of the residual -- the predicate
		// cannot tell a read 0.9 from a defaulted one -- and it is why the
		// rule is "an unreadable flag withholds only what is NEW".
		read, unread := base(), base()
		unread.Unresolved = []string{"gpu_memory_utilization"}
		if !read.IsCapacityCompatible(unread) {
			t.Error("Unresolved entered the comparison; it must not, or a variant " +
				"stops matching a sibling over a flag neither of them changed")
		}
	})

	// WHAT IT DOES NOT GUARANTEE, asserted so the limit is a recorded fact.
	//
	// The float fields are compared with ==, so a NaN is not equal to itself
	// and a NaN-bearing record is NOT reflexive. The parser cannot produce one
	// -- usableFraction rejects NaN and Inf, and records the flag as unread
	// instead -- so this is only reachable by constructing the struct directly.
	// It is pinned because a future caller that builds EngineParams by hand,
	// or a new float field without that guard, would hit it, and because the
	// obvious "fix" of folding NaN here would change the predicate for every
	// caller rather than at the one place a NaN can enter.
	t.Run("a hand-built NaN is not reflexive, and that is the parser's job to prevent", func(t *testing.T) {
		p := base()
		p.GpuMemoryUtilization = math.NaN()
		if p.IsCapacityCompatible(p) {
			t.Error("NaN compared equal to itself: either == was replaced by a " +
				"NaN-folding comparison, in which case this limit no longer " +
				"applies and the comment above needs re-deriving")
		}
		// And the guarantee that makes it unreachable from a parse.
		if usableFraction(math.NaN()) || usableFraction(math.Inf(1)) {
			t.Error("usableFraction admitted a non-finite value, which is how a " +
				"NaN would reach the struct from a real Deployment")
		}
	})
}
