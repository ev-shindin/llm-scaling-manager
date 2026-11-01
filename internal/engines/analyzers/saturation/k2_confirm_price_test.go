package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// How a cycle that holds a window's first reading (k2_confirm.go) is priced,
// and how the reading after it is judged, through Analyze.

// A cold prefill variant has no derived k2, so the chain past a held reading
// is k1. The held cycle is priced at the reading, lower, not at k1: three
// times the capacity in the cycle that shows the replica full.
func TestK2Hold_AColdPrefillIsPricedAtTheHeldReadingNotK1(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	ctx, logs := observedCtx(t)
	rm := makeReplicaMetrics("pod-p", "variant-p", 300_000, 1_149_312, 30, 1000, 1)
	rm.Ready = true
	res, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill}}))
	require.NoError(t, err)
	decisions := logs.FilterMessage("k2-decision").All()
	prios := make([]string, 0, len(decisions))
	for _, e := range decisions {
		prios = append(prios, stringField(t, e.ContextMap(), "priority"))
	}
	require.Contains(t, prios, k2ReasonObsUnconfirmed, "premise: the reading is held")
	require.Len(t, res.VariantCapacities, 1)
	assert.InDelta(t, 300_000, res.VariantCapacities[0].PerReplicaCapacity, 0.5,
		"priced at the held reading, not at k1 (919 449)")
	assert.Zero(t, k2Windows(a), "and nothing is recorded")
}

// A window whose last write is past the episode gap starts again with the
// next reading, so that reading is held, and the chain past it is the old
// window. The held cycle is priced at today's lower reading, not at the old
// window's figure.
func TestK2Hold_AWindowPastTheEpisodeGapIsNotPricedFromTheOldFigure(t *testing.T) {
	cycles := make([]settleCycle, 0, 10)
	cycles = append(cycles,
		settleCycle{0, 90_000, 30, 3000},
		settleCycle{15 * time.Second, 90_000, 30, 3000},
	)
	gap := 48 * time.Hour
	for i := range 8 {
		cycles = append(cycles, settleCycle{gap + time.Duration(i)*15*time.Second, 10_000, 5, 3000})
	}
	prios, k2s := runSettleCycles(t, cycles)
	held := -1
	for i, p := range prios {
		if i >= 2 && p == k2ReasonObsUnconfirmed {
			held = i
			break
		}
	}
	require.GreaterOrEqual(t, held, 0, "premise: a reading after the gap is held; got %v", prios)
	assert.Equal(t, int64(10_000), k2s[held], "the held cycle is priced at its reading, not the old window's 90 000")
	require.Less(t, held+1, len(prios))
	assert.Equal(t, "P1-obs", prios[held+1])
	assert.Equal(t, int64(10_000), k2s[held+1], "the new window holds today's readings alone")
}

// The reading after a held one is judged against the held readings as the
// k2 on record: a suspect reading (a queue deeper than its batch) completing
// at the rate on record and holding most of the held figure is admitted on
// the rate test, as it would be against a window of them.
func TestK2Hold_TheSettleTestReadsTheHeldReadingsAsOnRecord(t *testing.T) {
	prios, k2s := runSettleCycles(t, []settleCycle{
		{0, 270_000, 30, 3000},                 // admitted on sight and held; mu 6 req/s written
		{15 * time.Second, 200_000, 160, 2900}, // 240 000 waiting behind 200 000, 0.74 of the held 270 000
	})
	assert.Equal(t, []string{k2ReasonObsUnconfirmed, "P1-obs"}, prios)
	assert.Equal(t, int64(235_000), k2s[1], "both readings are written")
}

// After the episode gap the old window still holds its figure, but the
// reading that follows a held one is judged against the hold: a suspect
// reading (a queue deeper than its batch, and 28 % under its reading a minute
// earlier, so not settled) holding 0.72 of the held figure and
// completing at the rate on record passes the rate test, where against the
// old 90 000 it would be refused.
func TestK2Hold_AfterTheGapTheSettleTestReadsTheHoldNotTheOldWindow(t *testing.T) {
	cycles := make([]settleCycle, 0, 8)
	cycles = append(cycles, settleCycle{0, 90_000, 30, 3000}, settleCycle{15 * time.Second, 90_000, 30, 3000})
	gap := 48 * time.Hour
	for i := range 5 {
		cycles = append(cycles, settleCycle{gap + time.Duration(i)*15*time.Second, 10_000, 5, 3000})
	}
	cycles = append(cycles, settleCycle{gap + 75*time.Second, 7_200, 160, 3000}) // 240 000 waiting behind 7 200: 28 % under a minute ago, not settled
	prios, _ := runSettleCycles(t, cycles)
	n := len(prios)
	require.Equal(t, k2ReasonObsUnconfirmed, prios[n-2], "premise: the cycle before is held; got %v", prios)
	assert.Equal(t, "P1-obs", prios[n-1], "judged against the held 10 000, not the old 90 000")
}
