package saturation

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// Issue #150. --max-num-seqs and --max-num-batched-tokens bound ONE engine. A
// replica of eight data-parallel ranks (a pod of eight ranks behind one API
// server, or eight API servers merged into one row) runs eight engines, and its
// KV capacity is already all eight ranks'. Every per-engine setting priced
// against that capacity is scaled by the ranks the row reports.

// The derived mu caps the resident sequences at max_num_seqs per rank.
func TestDeriveMuCapsSequencesPerRank(t *testing.T) {
	short := shape.New(100, 100, 0) // a shape whose cache implies far more than 256 sequences

	one := deriveMu(tracedModel, tracedParams, 1, tracedKv, short, tracedK, 100)
	require.True(t, one.ok)
	require.Equal(t, 256.0, one.seqs, "precondition: one rank sits on its cap")

	eight := deriveMu(tracedModel, tracedParams, 8, 8*tracedKv, short, tracedK, 100)
	require.True(t, eight.ok)
	assert.Equal(t, 8*256.0, eight.seqs, "eight ranks admit eight times max_num_seqs")
	assert.InDelta(t, 8*one.rate, eight.rate, 1e-9*one.rate,
		"eight ranks of one engine's cache serve eight times its rate")

	capped := deriveMu(tracedModel, tracedParams, 1, 8*tracedKv, short, tracedK, 100)
	assert.Equal(t, 256.0, capped.seqs,
		"the defect: eight ranks' cache capped at one rank's 256 sequences")

	unknown := deriveMu(tracedModel, tracedParams, 0, tracedKv, short, tracedK, 100)
	assert.Equal(t, one, unknown, "an unknown rank count is one rank, as before")
}

// The KV flag sizes one engine; the live reading is already the replica's.
func TestCapacityTokensForScalesOnlyTheFlag(t *testing.T) {
	flagged := &capacity.EngineParams{TotalKvTokensOverride: tracedKv}
	assert.Equal(t, float64(8*tracedKv), capacityTokensFor(flagged, 8, 0),
		"with no live reading, eight ranks hold eight times the flag")
	assert.Equal(t, float64(tracedKv), capacityTokensFor(flagged, 8, tracedKv),
		"a live reading is the whole replica's and is never multiplied")
	assert.Equal(t, capacityTokensFor(flagged, 1, 0), capacityTokensFor(flagged, 0, 0))
}

// The derived k2 (P3) prices one engine and is scaled to the replica.
func TestDerivedK2ScalesWithRanks(t *testing.T) {
	params := &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256}
	// N_steady = min(8192 * 1000 / 2000, 256) = 256; k2 = 256 * (1000 + 500).
	perEngine := estimateCapacityFromParams(params, 1000, 1000)
	require.Equal(t, int64(384000), perEngine, "precondition: one engine's derived figure")

	k2 := func(ranks int) (int64, capacity.K2Source) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		const k1 = 100_000_000 // far above either figure, so it cannot be the answer
		d := a.computeK2("dp-key", "m", "ns", "decode-v",
			0, 0, // not saturated, no history: the chain reaches P3
			1000, 1000, 10,
			params, ranks,
			k1, k1,
			domain.RoleDecode, false, admission{}, false, logr.Discard())
		return d.k2, d.src
	}
	got1, src1 := k2(1)
	require.Equal(t, capacity.K2SrcDerived, src1)
	assert.Equal(t, perEngine, got1, "one rank: unchanged")
	got8, src8 := k2(8)
	require.Equal(t, capacity.K2SrcDerived, src8)
	assert.Equal(t, 8*perEngine, got8, "eight ranks: eight engines' figure")
	got0, _ := k2(0)
	assert.Equal(t, got1, got0, "an unknown rank count is one rank")
}

// End to end through computeReplicaCapacity: the row's EngineRanks reaches the
// derived k2, so a pod of eight ranks is priced at eight engines' capacity and
// not at min(k1 of eight ranks, k2 of one).
func TestReplicaCapacityPricesEveryRank(t *testing.T) {
	params := &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256}
	price := func(ranks int) *capacity.ReplicaCapacity {
		store := capacity.NewStore()
		store.Update("ns", "m", "decode-v", capacity.Record{EngineParams: params}, time.Now())
		a := NewSaturationAnalyzer(store)
		// Eight ranks of 640,000 tokens; 20% used, nothing waiting.
		rm := makeReplicaMetrics("dp8-0", "decode-v", 1_024_000, 5_120_000, 0, 1000, 1000)
		rm.Ready = true
		rm.EngineRanks = ranks
		cfg := &config.ScalingPolicy{KvCacheThreshold: 0.8, ScaleUpThreshold: 0.85, QueueLengthThreshold: 10}
		return a.computeReplicaCapacity(rm, cfg, tuningOf(cfg), "m", "ns", 8, domain.RoleDecode, "H200",
			outputKeys{k2: 1000, mu: 1000}, 1000, 1000, 0, derivedMu{}, servedRates{}, false, false, replicaRouting{}, logr.Discard())
	}

	eight := price(8)
	require.NotNil(t, eight)
	assert.Equal(t, capacity.K2SrcDerived, eight.K2Priority)
	assert.Equal(t, int64(8*384000), eight.ComputeBoundCapacity)
	assert.Equal(t, int64(8*384000), eight.EffectiveCapacity,
		"min(k1 = 4,096,000, k2 = 3,072,000): the eight engines' figure")

	one := price(1)
	require.NotNil(t, one)
	assert.Equal(t, int64(384000), one.EffectiveCapacity,
		"the same row read as one engine is priced at one engine: the issue's 8x")
}

// The restart suite's 8-GPU case (analyzer-learner.md, section 7, PR 2): a
// whole-node pod of eight data-parallel ranks, one row, restarts under its
// own name. Its one Ready time stands for all eight ranks, so the pod waits
// a window as a one-rank replica does, and once settled k2 records the
// busiest rank's occupancy scaled to the pod (BusiestRankTokens), not the
// mean over the ranks.
func TestRestartSuite_AnEightRankPodRestartedUnderItsNameWaitsAWindow(t *testing.T) {
	const ranks, perRank = 8, int64(376_480)
	row := func(ready time.Time) domain.ReplicaMetrics {
		rm := makeReplicaMetrics("dp8-0", "decode-v", 4*perRank, ranks*perRank, 160, 1000, 500)
		rm.EngineRanks = ranks
		rm.MaxRankQueueLength = 20
		rm.MaxRankKvCacheUsage = 260_000.0 / float64(perRank)
		rm.GenerationTokenRate = ranks * 3000
		rm.RequestRate = ranks * 6
		rm.ReadySince = ready
		return rm
	}
	r := newRestartRig(t, restartT0)
	old := restartT0.Add(-time.Hour)
	settled(t, r, func() []domain.ReplicaMetrics { return []domain.ReplicaMetrics{row(old)} }, decodeVariants("decode-v"))
	require.Equal(t, int64(ranks*260_000), row(old).BusiestRankTokens(), "premise: the busiest rank's occupancy, scaled to the pod")

	down := row(time.Time{})
	down.Ready = false
	r.cycle([]domain.ReplicaMetrics{down}, decodeVariants("decode-v"))
	back := r.now.Add(-5 * time.Second)
	assert.Equal(t, waitThenAdmit(), runCycles(r, waitWindow+1, func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{row(back)}
	}, decodeVariants("decode-v")), "the eight-rank pod back under its own name with a new Ready time")

	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for _, ra := range r.a.computeCapacityHistory {
		for _, v := range ra.Snapshot().Values {
			assert.InDelta(t, float64(ranks*260_000), v, 1, "every k2 reading is the busiest rank's, scaled to the pod")
		}
	}
}
