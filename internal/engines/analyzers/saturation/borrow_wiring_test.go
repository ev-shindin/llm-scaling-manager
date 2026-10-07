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

// Borrowing a line is wired through three places that unit tests cannot see
// together: fitLines resolves the line, records which variant borrowed it, and
// priceReplicas stamps that onto the ReplicaCapacity the floor reads. Each
// piece has its own spec, and removing the one line that joins them --
// rc.SaturatedThroughputLineBorrowed = c.itlBorrowed[...] -- was measured to
// leave every one of those specs green.
//
// So this drives the real cycle, twice, and reads the flag off the record.
func TestBorrowedLineReachesTheRecord(t *testing.T) {
	const (
		// Must match makeAnalyzerInput: engineFingerprint looks the record up
		// by the INPUT's namespace and model, so a store keyed on anything
		// else resolves to an empty fingerprint and nothing is shared.
		ns      = "test-ns"
		model   = "test-model"
		accel   = "H100"
		lender  = "variant-lender"
		starved = "variant-starved"
	)

	// One configuration, so both variants hash to one physics key and may
	// share a line.
	params := func() *capacity.EngineParams {
		return &capacity.EngineParams{
			Engine:                    inferenceengine.EngineVLLM,
			GpuMemoryUtilization:      0.9,
			BlockSize:                 16,
			KvCacheDtype:              "auto",
			WeightDtype:               "auto",
			TensorParallelSize:        1,
			MaxNumSeqs:                256,
			EffectiveMaxBatchedTokens: 8192,
		}
	}
	newAnalyzer := func() *SaturationAnalyzer {
		store := capacity.NewStore()
		for _, v := range []string{lender, starved} {
			store.Update(ns, model, v, capacity.Record{
				AcceleratorName: accel, GpuCount: 1, EngineParams: params(),
			})
		}
		return NewSaturationAnalyzer(store)
	}

	states := []domain.VariantReplicaState{
		{VariantName: lender, Role: domain.RoleDecode, AcceleratorName: accel,
			CurrentReplicas: 10, GPUsPerReplica: 1},
		{VariantName: starved, Role: domain.RoleDecode, AcceleratorName: accel,
			CurrentReplicas: 1, GPUsPerReplica: 1},
	}

	// The lender reports ten readings spread across k, which fits a line. The
	// starved variant reports one reading with no ITL at all, so its own
	// window can never produce one -- the cold-start case borrowing exists
	// for.
	lenderRows := func() []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, 10)
		for i := 0; i < 10; i++ {
			k := 0.20 + 0.04*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("lender-%d", i), lender, 5000, 16000, 0, 6000, 1000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			out = append(out, rm)
		}
		return out
	}
	starvedRow := func() domain.ReplicaMetrics {
		rm := makeReplicaMetrics("starved-0", starved, 5000, 16000, 0, 6000, 1000)
		rm.Ready = true
		rm.KvUsageInstant = 0.30
		rm.AvgITL = 0 // no ITL reported: nothing of its own to fit
		return rm
	}

	ctx, _ := observedCtx(t)
	a := newAnalyzer()

	// Cycle 1: the lender measures and publishes. An earlier version of this
	// comment claimed the starved variant has nothing to borrow yet "because
	// borrowing reads what a PREVIOUS cycle left" -- which is not true, and
	// not true of this fixture in particular: fitLines publishes and borrows
	// in one sorted loop, and `variant-lender` < `variant-starved`, so the
	// borrow fires in this very cycle. Nothing here asserted that claim, so it
	// sat wrong without failing anything. See fitLines on what the sort order
	// does and does not decide.
	rows := append(lenderRows(), starvedRow())
	_, err := a.Analyze(ctx, makeAnalyzerInput(rows, states))
	require.NoError(t, err)
	require.NotEmpty(t, a.itlLines, "the lender's fit must have been published")

	// Cycle 2: the starved variant borrows.
	res, err := a.Analyze(ctx, makeAnalyzerInput(rows, states))
	require.NoError(t, err)
	require.NotNil(t, res)

	// Re-run the stages that produce the records, so the flag can be read off
	// them: Analyze returns demand, not the per-replica capacities.
	in := makeAnalyzerInput(rows, states)
	pol, ok := in.Config.(*config.ScalingPolicy)
	require.True(t, ok, "the test input must carry a ScalingPolicy")
	caps, err := func() ([]capacity.ReplicaCapacity, error) {
		c := a.newCycle(ctx, in, pol)
		c.observeFleetShape()
		c.resolvePricing()
		c.fitLines()
		return c.priceReplicas(ctx)
	}()
	require.NoError(t, err)

	var sawStarved, sawLender bool
	for _, rc := range caps {
		switch rc.VariantName {
		case starved:
			sawStarved = true
			require.True(t, rc.SaturatedThroughputLineBorrowed,
				"the starved variant priced on a line it did not measure, so the "+
					"record must say so -- otherwise the floor grows a fleet on a "+
					"sibling's measurement")
		case lender:
			sawLender = true
			require.False(t, rc.SaturatedThroughputLineBorrowed,
				"the lender fitted its own line and must keep the right to order")
		}
	}
	require.True(t, sawStarved, "no record for the starved variant: the test proves nothing")
	require.True(t, sawLender, "no record for the lender: the test proves nothing")
}
