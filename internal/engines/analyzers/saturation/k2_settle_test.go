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
// record yet -- is admitted the cycle after it first shows, and its low figure
// is kept.
func TestK2Settle_AComputeBoundAtLowOccupancyIsLearned(t *testing.T) {
	prios, k2s := runSettleCycles(t, []settleCycle{
		{0, 45_000, 40, 800},
		{15 * time.Second, 46_000, 40, 800},
		{30 * time.Second, 45_500, 40, 800},
	})
	assert.Equal(t, []string{"P1-obs-unsettled", "P1-obs", "P1-obs"}, prios)
	assert.Equal(t, int64(45_750), k2s[2], "the low bound is the variant's k2")
}

// A reading whose queue is the overflow of a batch at least as large is a
// replica at its limit however it got there, and is recorded on sight.
func TestK2Settle_AFullReplicaIsRecordedOnSight(t *testing.T) {
	prios, _ := runSettleCycles(t, []settleCycle{{0, 260_000, 160, 0}})
	assert.Equal(t, []string{"P1-obs"}, prios, "160 x 1500 = 240 000 waiting behind 260 000 resident")
}

// A suspect reading whose service rate has reached the rate on record is
// admitted at once: the replica is completing at its limit.
func TestK2Settle_ARateAtTheRecordIsALimit(t *testing.T) {
	prios, _ := runSettleCycles(t, []settleCycle{
		{0, 270_000, 30, 3000},
		{65 * time.Second, 12_000, 0, 1.2},
		{80 * time.Second, 60_000, 160, 2900},
	})
	assert.Equal(t, []string{"P1-obs", "P2-hist", "P1-obs"}, prios)
}
