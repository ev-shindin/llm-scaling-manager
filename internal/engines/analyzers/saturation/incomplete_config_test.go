package saturation

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// A configuration this controller could not fully READ must not become a
// sharing key, and this is the control for that.
//
// The digest hashes the unresolved set, which stops an incomplete read
// colliding with a complete one. It cannot stop two incomplete reads colliding
// with EACH OTHER: the thing they disagree about is exactly the flag neither
// could be read for. That is the case on a real llm-d fleet, where every
// variant passes `--block-size $VLLM_BLOCK_SIZE` and every one of them keeps
// the default -- so a whole fleet of differently-configured engines hashes
// alike. Only refusing the key prevents a borrow there.
func TestAnUnreadableConfigurationIsNotASharingKey(t *testing.T) {
	const (
		ns      = "test-ns"
		model   = "test-model"
		accel   = "H100"
		variant = "variant-a"
	)

	params := func(unresolved ...string) *capacity.EngineParams {
		return &capacity.EngineParams{
			Engine:                    inferenceengine.EngineVLLM,
			GpuMemoryUtilization:      0.9,
			BlockSize:                 16,
			KvCacheDtype:              "auto",
			WeightDtype:               "auto",
			TensorParallelSize:        1,
			MaxNumSeqs:                256,
			EffectiveMaxBatchedTokens: 8192,
			Unresolved:                unresolved,
		}
	}

	t.Run("a complete configuration gets a fingerprint", func(t *testing.T) {
		store := capacity.NewStore()
		store.Update(ns, model, variant, capacity.Record{
			AcceleratorName: accel, GpuCount: 1, EngineParams: params(),
		})
		a := NewSaturationAnalyzer(store)
		fp := a.engineFingerprint(ns, model, variant)
		require.NotEmpty(t, fp,
			"a fully-read configuration must key on its digest, or sharing never happens")
	})

	t.Run("an incomplete one gets none", func(t *testing.T) {
		store := capacity.NewStore()
		store.Update(ns, model, variant, capacity.Record{
			AcceleratorName: accel, GpuCount: 1, EngineParams: params("block_size"),
		})
		a := NewSaturationAnalyzer(store)
		require.Empty(t, a.engineFingerprint(ns, model, variant),
			"a configuration whose block size could not be read must not be a "+
				"sharing key: every variant of an llm-d fleet would hash alike")
	})

	t.Run("and so falls back to its own per-variant window key", func(t *testing.T) {
		store := capacity.NewStore()
		store.Update(ns, model, variant, capacity.Record{
			AcceleratorName: accel, GpuCount: 1, EngineParams: params("block_size"),
		})
		a := NewSaturationAnalyzer(store)
		own := a.itlWindowKey(ns, model, variant, accel, 1)
		physics := a.itlPhysicsKey(ns, model, variant, accel, 1,
			a.engineFingerprint(ns, model, variant))
		require.Equal(t, own, physics,
			"with no digest the line must be filed under the variant's own key, "+
				"so it neither lends nor borrows")
	})
}

// And end to end: two variants on one unreadable configuration do not share a
// line, where two on the same READABLE configuration do.
func TestTwoUnreadableVariantsDoNotShareALine(t *testing.T) {
	const (
		ns      = "test-ns"
		model   = "test-model"
		accel   = "H100"
		lender  = "variant-lender"
		starved = "variant-starved"
	)

	run := func(t *testing.T, unresolved []string) bool {
		t.Helper()
		store := capacity.NewStore()
		for _, v := range []string{lender, starved} {
			store.Update(ns, model, v, capacity.Record{
				AcceleratorName: accel, GpuCount: 1,
				EngineParams: &capacity.EngineParams{
					Engine:                    inferenceengine.EngineVLLM,
					GpuMemoryUtilization:      0.9,
					BlockSize:                 16,
					KvCacheDtype:              "auto",
					WeightDtype:               "auto",
					TensorParallelSize:        1,
					MaxNumSeqs:                256,
					EffectiveMaxBatchedTokens: 8192,
					Unresolved:                unresolved,
				},
			})
		}
		a := NewSaturationAnalyzer(store)

		states := []domain.VariantReplicaState{
			{VariantName: lender, Role: domain.RoleDecode, AcceleratorName: accel,
				CurrentReplicas: 10, GPUsPerReplica: 1},
			{VariantName: starved, Role: domain.RoleDecode, AcceleratorName: accel,
				CurrentReplicas: 1, GPUsPerReplica: 1},
		}
		rows := make([]domain.ReplicaMetrics, 0, 11)
		for i := 0; i < 10; i++ {
			k := 0.20 + 0.04*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("lender-%d", i), lender, 5000, 16000, 0, 6000, 1000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			rows = append(rows, rm)
		}
		s := makeReplicaMetrics("starved-0", starved, 5000, 16000, 0, 6000, 1000)
		s.Ready = true
		s.KvUsageInstant = 0.30
		s.AvgITL = 0 // nothing of its own to fit: the borrowing case
		rows = append(rows, s)

		ctx, _ := observedCtx(t)
		for i := 0; i < 2; i++ {
			_, err := a.Analyze(ctx, makeAnalyzerInput(rows, states))
			require.NoError(t, err)
		}

		// WHETHER THE STARVED VARIANT BORROWED, not whether itlLines holds
		// anything. The first draft of this asserted the latter and failed:
		// with no digest, publishLine still files the lender's line under the
		// lender's OWN window key, so the map is non-empty and nothing is
		// shared. The map says a line exists; only itlBorrowed says a variant
		// priced itself from someone else's.
		in := makeAnalyzerInput(rows, states)
		pol, ok := in.Config.(*config.ScalingPolicy)
		require.True(t, ok, "the test input must carry a ScalingPolicy")
		c := a.newCycle(ctx, in, pol)
		c.observeFleetShape()
		c.resolvePricing()
		c.fitLines()
		return c.itlBorrowed[starved]
	}

	t.Run("a readable configuration lends its line", func(t *testing.T) {
		require.True(t, run(t, nil),
			"the control must borrow, or the case below proves only that "+
				"borrowing is broken for an unrelated reason")
	})

	t.Run("an unreadable one does not", func(t *testing.T) {
		require.False(t, run(t, []string{"block_size"}),
			"a variant priced itself from a line filed under a digest built on a "+
				"default standing in for an unread flag -- which every sibling "+
				"with the same unread flag shares, whatever they actually run")
	})
}
