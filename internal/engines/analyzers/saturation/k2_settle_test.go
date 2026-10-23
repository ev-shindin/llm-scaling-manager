package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// settleCycle is one cycle of one decode replica: what it holds, its queue
// and its generated tokens per second over the last minute.
type settleCycle struct {
	at     time.Duration
	tokens int64
	queue  int
	gps    float64
}

// runSettleCycles runs the cycles over one decode replica of a 376 480-token
// cache at 1000 in / 500 out, and returns each cycle's k2-decision priority
// and the k2 it priced the replica at.
func runSettleCycles(t *testing.T, cycles []settleCycle) ([]string, []int64) {
	t.Helper()
	analyzer := NewSaturationAnalyzer(capacity.NewStore())
	start := time.Date(2026, 10, 23, 11, 18, 0, 0, time.UTC)
	prios := make([]string, 0, len(cycles))
	k2s := make([]int64, 0, len(cycles))
	for _, c := range cycles {
		at := start.Add(c.at)
		analyzer.now = func() time.Time { return at }
		ctx, logs := observedCtx(t)
		rm := makeReplicaMetrics("pod-s", "variant-s", c.tokens, 376_480, c.queue, 1000, 500)
		rm.GenerationTokenRate = c.gps
		rm.RequestRate = c.gps / 500 // completing at the rate it generates
		_, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm},
			[]domain.VariantReplicaState{
				{VariantName: "variant-s", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
			}))
		require.NoError(t, err)
		decisions := logs.FilterMessage("k2-decision").All()
		require.NotEmpty(t, decisions)
		prios = append(prios, stringField(t, decisions[0].ContextMap(), "priority"))
		var k2 int64
		for _, e := range logs.FilterMessage("replica-capacity-decision").All() {
			k2 = e.ContextMap()["k2ComputeBound"].(int64)
		}
		k2s = append(k2s, k2)
	}
	return prios, k2s
}

// The rerun's 11:19:39 step on sg1: the bucket held 270 539 from a saturated
// replica completing 6 req/s; the replica is then handed a load step and reads
// queue 160 at 31 713 resident tokens (8 % of its cache) with the idle
// minute's 0.95 tokens/s, then 260 123 under the same queue a cycle later.
// Recorded, the step made the window mean(270 539, 31 713) = 151 126 and then
// 187 458, and the replica, holding 252-266k with no queue, read util 1.34-1.42.
func TestK2Settle_ALoadStepIsNotALimit(t *testing.T) {
	prios, k2s := runSettleCycles(t, []settleCycle{
		{0, 270_539, 30, 3000},                // a replica full and queued: 6 req/s at 500 out
		{65 * time.Second, 12_000, 0, 1.2},    // the load goes; the replica idles
		{80 * time.Second, 31_713, 160, 0.95}, // the step: a deep queue on an empty batch
		{95 * time.Second, 260_123, 160, 3000},
	})
	assert.Equal(t, []string{"P1-obs", "P2-hist", "P1-obs-unsettled", "P1-obs"}, prios)
	assert.Equal(t, int64(270_539), k2s[2], "the step's reading is not folded in; the window prices the replica")
	assert.Equal(t, int64((270_539+260_123)/2), k2s[3], "the next, settled reading is")
}

// The same step's mu: the idle minute's 0.0019 req/s was recorded as a
// saturated throughput beside the step's k2 reading. Refused with it.
func TestK2Settle_ALoadStepRecordsNoThroughput(t *testing.T) {
	analyzer := NewSaturationAnalyzer(capacity.NewStore())
	at := time.Date(2026, 10, 23, 11, 19, 39, 0, time.UTC)
	analyzer.now = func() time.Time { return at }
	// A rate on record, so the step's own rate has something to fall short of.
	rm := makeReplicaMetrics("pod-s", "variant-s", 270_539, 376_480, 30, 1000, 500)
	rm.GenerationTokenRate = 3000
	rm.RequestRate = 6
	states := []domain.VariantReplicaState{{VariantName: "variant-s", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode}}
	_, err := analyzer.Analyze(t.Context(), makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states))
	require.NoError(t, err)
	at = at.Add(65 * time.Second)
	idle := makeReplicaMetrics("pod-s", "variant-s", 12_000, 376_480, 0, 1000, 500)
	idle.GenerationTokenRate = 1.2
	_, err = analyzer.Analyze(t.Context(), makeAnalyzerInput([]domain.ReplicaMetrics{idle}, states))
	require.NoError(t, err)
	at = at.Add(15 * time.Second)
	step := makeReplicaMetrics("pod-s", "variant-s", 31_713, 376_480, 160, 1000, 500)
	step.GenerationTokenRate = 0.95
	_, err = analyzer.Analyze(t.Context(), makeAnalyzerInput([]domain.ReplicaMetrics{step}, states))
	require.NoError(t, err)
	analyzer.mu.Lock()
	defer analyzer.mu.Unlock()
	require.Len(t, analyzer.saturatedThroughput, 1)
	for _, ra := range analyzer.saturatedThroughput {
		assert.Equal(t, 1, ra.Len(), "one reading, the saturated replica's; the step added none")
		assert.InDelta(t, 6.0, ra.Median(), 1e-9)
	}
}

// What k2 exists for must still be learned: a compute bound reached at low
// occupancy. A replica whose queue stands while its resident KV stays flat at
// 12 % of the cache -- long prompts behind a prefill-bound engine, no rate on
// record yet -- is admitted a window after it first shows, and its low figure
// is kept.
func TestK2Settle_AComputeBoundAtLowOccupancyIsLearned(t *testing.T) {
	prios, k2s := runSettleCycles(t, []settleCycle{
		{0, 45_000, 40, 800},
		{15 * time.Second, 46_000, 40, 800},
		{30 * time.Second, 45_500, 40, 800},
		{45 * time.Second, 45_000, 40, 800},
		{60 * time.Second, 46_000, 40, 800},
		{75 * time.Second, 45_500, 40, 800},
	})
	assert.Equal(t, []string{
		"P1-obs-unsettled", "P1-obs-unsettled", "P1-obs-unsettled", "P1-obs-unsettled", "P1-obs", "P1-obs",
	}, prios)
	assert.Equal(t, int64(45_750), k2s[5], "the low bound is the variant's k2")
}

// Flat cycle to cycle is not settled. Two readings 15 s apart share 45 s of
// their one-minute window, so a replica filling by 20 % a cycle -- a batch
// capped by max_num_seqs growing toward prompt plus half the output -- reads
// flat on every pair while it doubles in a minute. Compared a window back it
// is still filling, and nothing is recorded until it stops.
func TestK2Settle_AFillIsNotFlatCycleToCycle(t *testing.T) {
	prios, k2s := runSettleCycles(t, []settleCycle{
		{0, 100_000, 200, 0},
		{15 * time.Second, 120_000, 200, 0},
		{30 * time.Second, 144_000, 200, 0},
		{45 * time.Second, 172_800, 200, 0},
		{60 * time.Second, 207_360, 200, 0},
		{75 * time.Second, 230_000, 200, 0},
		{90 * time.Second, 240_000, 200, 0},
		{105 * time.Second, 244_000, 200, 0},
		{120 * time.Second, 245_000, 200, 0},
		{135 * time.Second, 245_000, 200, 0},
	})
	for i, p := range prios[:8] {
		assert.Equal(t, "P1-obs-unsettled", p, "cycle %d is mid-fill", i)
	}
	assert.Equal(t, "P1-obs", prios[8], "a window after the fill slowed to under 25 %% a minute")
	assert.Equal(t, int64(245_000), k2s[8], "the first figure recorded is the filled one")
}

// A reading whose queue is the overflow of a batch at least as large is a
// replica at its limit however it got there, and is recorded on sight.
func TestK2Settle_AFullReplicaIsRecordedOnSight(t *testing.T) {
	prios, _ := runSettleCycles(t, []settleCycle{{0, 260_000, 160, 0}})
	assert.Equal(t, []string{"P1-obs"}, prios, "160 x 1500 = 240 000 waiting behind 260 000 resident")
}

// A suspect reading whose service rate has reached the rate on record, and
// whose residency has reached most of the k2 on record, is admitted at once:
// the replica is completing at its limit, holding what a full one holds.
func TestK2Settle_ARateAtTheRecordIsALimit(t *testing.T) {
	prios, _ := runSettleCycles(t, []settleCycle{
		{0, 270_000, 30, 3000},
		{65 * time.Second, 12_000, 0, 1.2},
		{80 * time.Second, 200_000, 160, 2900}, // 240 000 waiting behind 200 000, 0.74 of k2
	})
	assert.Equal(t, []string{"P1-obs", "P2-hist", "P1-obs"}, prios)
}

// A rate alone shows a replica is busy, not that it is at its limit. One
// completing at the rate on record but holding a fifth of the k2 on record
// under a queue deeper than its batch is filling, and waits for the window.
func TestK2Settle_ARateWithoutTheResidencyIsNotALimit(t *testing.T) {
	prios, k2s := runSettleCycles(t, []settleCycle{
		{0, 270_000, 30, 3000},
		{65 * time.Second, 12_000, 0, 1.2},
		{80 * time.Second, 60_000, 160, 2900},
	})
	assert.Equal(t, []string{"P1-obs", "P2-hist", "P1-obs-unsettled"}, prios)
	assert.Equal(t, int64(270_000), k2s[2], "the filling reading is not folded in")
}

// The router moves the hot queue onto a sibling that was already busy: it
// completes at 75 % of the rate on record, above the 70 % the rate test asks,
// and reads 25 waiting (175 000 of footprint) on 160 000 resident while it
// fills. The k2 on record is 250 000, and the reading must not pull it down.
func TestK2Settle_ABusySiblingHandedTheQueueIsNotALimit(t *testing.T) {
	const cache = int64(1_000_000)
	const key = "test-ns|both-v|test-model|H200|1|both|long|q5"
	states := []domain.VariantReplicaState{{VariantName: "both-v", AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1}}
	row := func(pod string, tok int64, q int, gtr float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, "both-v", tok, cache, q, 6000, 1000)
		rm.RequestRate = gtr / 1000
		rm.GenerationTokenRate = gtr
		rm.KvUsageInstant, rm.KvUsageInstantKnown = float64(tok)/float64(cache), true
		return rm
	}
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	// r0 at its limit (250 000, queued, 2.0 req/s); r1 busy at 60 % of that,
	// serving 1.5 req/s, nothing waiting.
	for range 4 {
		_, err := a.Analyze(t.Context(), makeAnalyzerInput([]domain.ReplicaMetrics{
			row("r0", 250_000, 20, 2000), row("r1", 150_000, 0, 1500)}, states))
		require.NoError(t, err)
		clock = clock.Add(ThroughputSampleSpacing)
	}
	a.mu.Lock()
	before := a.computeCapacityHistory[key].Average()
	a.mu.Unlock()
	_, err := a.Analyze(t.Context(), makeAnalyzerInput([]domain.ReplicaMetrics{
		row("r0", 250_000, 0, 2000), row("r1", 160_000, 25, 1500)}, states))
	require.NoError(t, err)
	a.mu.Lock()
	after := a.computeCapacityHistory[key].Average()
	a.mu.Unlock()
	t.Logf("k2 mean %.0f -> %.0f", before, after)
	assert.InDelta(t, 250_000, before, 1, "the seed is r0's limit")
	assert.InDelta(t, before, after, 1, "a busy sibling filling under the moved queue is not its limit")
}

// Settled is flat, either way. A compute bound at low occupancy may drift
// down a little over a window and is still admitted; a fall past
// K2SettleMaxRise is traffic moved off the replica, not a limit, and is not.
// No rate on record (no generated tokens), so the flat test alone decides.
func TestK2Settle_AFallIsNotSettling(t *testing.T) {
	prios, k2s := runSettleCycles(t, []settleCycle{
		{0, 45_000, 40, 0},
		{15 * time.Second, 46_000, 40, 0},
		{30 * time.Second, 45_500, 40, 0},
		{45 * time.Second, 45_000, 40, 0},
		{60 * time.Second, 46_000, 40, 0},
		{75 * time.Second, 36_000, 40, 0}, // 22 % under 15 s's 46 000: still flat
		{90 * time.Second, 20_000, 40, 0}, // 56 % under 30 s's 45 500: moved off
	})
	assert.Equal(t, []string{
		"P1-obs-unsettled", "P1-obs-unsettled", "P1-obs-unsettled", "P1-obs-unsettled",
		"P1-obs", "P1-obs", "P1-obs-unsettled",
	}, prios)
	assert.Equal(t, int64((46_000+36_000)/2), k2s[6], "the fall is not folded in")
}
