package saturation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Issue #278. vLLM counts a decode request whose KV is still in flight from
// prefill in num_requests_waiting. The shape: one decode replica at KV 0.5
// with 768 waiting, beside a prefill variant. Read as a backlog, the 768 put
// the decode over queueLengthThreshold (5) -- a saturated reading whose k2 is
// half the cache -- and charged 768 x (6000 + 1000) tokens of queue demand on
// a replica with half its cache free.

const (
	pdPrefillVariant = "prefill-v"
	pdDeferredQueue  = 768
	pdIn, pdOut      = 6000.0, 1000.0
)

// pdDecodeRow is the decode replica: tokens resident of runKvCapacity and
// pdDeferredQueue waiting, completing at a steady rate.
func pdDecodeRow(tokens int64) domain.ReplicaMetrics {
	rm := makeReplicaMetrics("decode-0", decodeVariant, tokens, runKvCapacity, pdDeferredQueue, pdIn, pdOut)
	rm.RequestRate = 4.73
	rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
	return rm
}

// withReason gives rm the split vLLM v0.20.0 and later publish:
// n of its waiting requests wait for scheduling capacity, the rest are deferred.
func withReason(rm domain.ReplicaMetrics, n int) domain.ReplicaMetrics {
	rm.CapacityQueueKnown = true
	rm.CapacityQueueLength = n
	rm.MaxRankCapacityQueueLength = n
	return rm
}

// pdPrefillRow is a lightly loaded prefill replica, nothing waiting.
func pdPrefillRow() domain.ReplicaMetrics {
	rm := makeReplicaMetrics("prefill-0", pdPrefillVariant, 100_000, runKvCapacity, 0, pdIn, 1)
	rm.RequestRate = 4.73
	rm.GenerationTokenRate = rm.RequestRate
	return rm
}

var (
	pdStates = []domain.VariantReplicaState{
		{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: pdPrefillVariant, Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	// The same decode variant with no prefill beside it: it serves whole
	// requests, and its queue is a backlog.
	decodeOnlyStates = []domain.VariantReplicaState{
		{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	aggregatedStates = []domain.VariantReplicaState{
		{VariantName: decodeVariant, Role: domain.RoleBoth, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
)

// analyzePD runs four cycles of rows under states and returns the last
// result and the analyzer.
func analyzePD(t *testing.T, rows []domain.ReplicaMetrics, states []domain.VariantReplicaState) (*SaturationAnalyzer, *domain.AnalyzerResult) {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	in := makeAnalyzerInput(rows, states)
	var res *domain.AnalyzerResult
	for range 4 {
		var err error
		res, err = a.Analyze(context.Background(), in)
		require.NoError(t, err)
		clock = clock.Add(30 * time.Second)
	}
	return a, res
}

// decodeDemand is the decode variant's demand in tokens.
func decodeDemand(t *testing.T, res *domain.AnalyzerResult) float64 {
	t.Helper()
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == decodeVariant {
			return vc.TotalDemand
		}
	}
	t.Fatalf("no capacity for %s", decodeVariant)
	return 0
}

// recordedK2 reports whether a k2 observation was recorded for the decode
// variant: the saturation test read it saturated.
func recordedK2(a *SaturationAnalyzer) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.computeCapacityHistory {
		if strings.Contains(k, "|"+decodeVariant+"|") {
			return true
		}
	}
	return false
}

const halfCache = runKvCapacity / 2 // KV 0.5
const queueCharge = pdDeferredQueue * (pdIn + pdOut)

func TestPDDecodeDeferredQueueIsNotSaturationOrDemand(t *testing.T) {
	// vLLM v0.20.0+: all 768 deferred, none waiting for capacity.
	a, res := analyzePD(t, []domain.ReplicaMetrics{withReason(pdDecodeRow(halfCache), 0), pdPrefillRow()}, pdStates)

	assert.False(t, recordedK2(a),
		"a P/D decode at KV 0.5 is not saturated by requests waiting for their KV from prefill; no k2 may be recorded at half its cache")
	assert.InDelta(t, float64(halfCache), decodeDemand(t, res), 1,
		"its demand is what it holds; the 768 waiting on prefill's KV add no queue demand")
}

func TestPDDecodeChargesOnlyTheCapacityQueue(t *testing.T) {
	// The capacity series is read as it stands, KV or not: 40 waiting for
	// capacity below a full cache is a decode at its sequence cap.
	a, res := analyzePD(t, []domain.ReplicaMetrics{withReason(pdDecodeRow(halfCache), 40), pdPrefillRow()}, pdStates)
	assert.True(t, recordedK2(a), "40 waiting for capacity is over the threshold of 5")
	assert.InDelta(t, float64(halfCache)+40*(pdIn+pdOut), decodeDemand(t, res), 1,
		"only the 40 waiting for capacity are charged")
}

// Older vLLM publishes no reason. Which of the 768 are in flight cannot be
// told, and dropping a real queue under-provisions, so the whole queue counts,
// as it did before #278: saturated, k2 recorded, every request charged.
func TestPDDecodeWithoutReasonKeepsTheFullQueue(t *testing.T) {
	a, res := analyzePD(t, []domain.ReplicaMetrics{pdDecodeRow(halfCache), pdPrefillRow()}, pdStates)
	assert.True(t, recordedK2(a), "768 waiting is over the threshold of 5: saturated")
	assert.InDelta(t, float64(halfCache)+queueCharge, decodeDemand(t, res), 1, "the whole queue is charged")
}

// No cliff at kvCacheThreshold (0.8) without the reason series: the queue's
// charge is the same just under and just over it.
func TestPDDecodeWithoutReasonHasNoCliffAtTheKvThreshold(t *testing.T) {
	for _, tokens := range []int64{runKvCapacity * 79 / 100, runKvCapacity * 81 / 100} {
		_, res := analyzePD(t, []domain.ReplicaMetrics{pdDecodeRow(tokens), pdPrefillRow()}, pdStates)
		assert.InDelta(t, queueCharge, decodeDemand(t, res)-float64(tokens), 1,
			"at %d resident tokens the queue's charge is the full queue", tokens)
	}
}

// Control: the same row on a model with no prefill variant. Its queue is a
// backlog of whole requests, and it reads exactly as before, reason or not.
func TestDecodeWithoutPrefillAndAggregatedKeepTheirQueue(t *testing.T) {
	for name, states := range map[string][]domain.VariantReplicaState{
		"decode without prefill": decodeOnlyStates,
		"aggregated":             aggregatedStates,
	} {
		t.Run(name, func(t *testing.T) {
			a, res := analyzePD(t, []domain.ReplicaMetrics{withReason(pdDecodeRow(halfCache), 0)}, states)
			assert.True(t, recordedK2(a), "768 waiting is over the threshold of 5: saturated")
			assert.InDelta(t, float64(halfCache)+queueCharge, decodeDemand(t, res), 1,
				"the whole queue is charged")
		})
	}
}

func TestDecodeCapacityView(t *testing.T) {
	multi := domain.ReplicaMetrics{
		VariantName: "d",
		QueueLength: 800, MaxRankQueueLength: 300, EngineRanks: 8,
		CapacityQueueKnown: true, CapacityQueueLength: 24, MaxRankCapacityQueueLength: 9,
	}
	v := decodeCapacityView(multi, domain.RoleDecode)
	assert.Equal(t, 24, v.QueueLength)
	assert.Equal(t, 9, v.BusiestRankQueue(), "the busiest rank's capacity queue, not the pod's")

	for _, role := range []string{domain.RolePrefill, domain.RoleBoth, ""} {
		assert.Equal(t, multi, decodeCapacityView(multi, role), "role %q keeps its row", role)
	}

	rows := []domain.ReplicaMetrics{multi}
	assert.Equal(t, rows, decodeCapacityViews(rows, map[string]string{"d": domain.RoleDecode}),
		"decode with no prefill variant keeps its row")
	pd := map[string]string{"d": domain.RoleDecode, "p": domain.RolePrefill}
	assert.Equal(t, 24, decodeCapacityViews(rows, pd)[0].QueueLength)

	unknown := multi
	unknown.CapacityQueueKnown = false
	assert.Equal(t, unknown, decodeCapacityView(unknown, domain.RoleDecode), "no reason series: the full queue")
}
