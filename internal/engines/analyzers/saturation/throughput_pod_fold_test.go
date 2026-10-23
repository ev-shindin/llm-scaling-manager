package saturation

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// measuredMuBias runs ten minutes of 15 s cycles in which n replicas of one
// decode variant are saturated, each completing a true 6.0 req/s read with
// independent 6 % noise, and returns the mean over trials of the window's
// median, relative to the truth.
func measuredMuBias(t *testing.T, n, trials int) float64 {
	t.Helper()
	const trueMu, sigma, out = 6.0, 0.06, 500.0
	states := []domain.VariantReplicaState{{VariantName: "variant-m", AcceleratorName: "H200",
		CurrentReplicas: n, GPUsPerReplica: 1, Role: domain.RoleDecode}}
	var sum float64
	for trial := range trials {
		rng := rand.New(rand.NewSource(int64(trial*100 + n)))
		a := NewSaturationAnalyzer(capacity.NewStore())
		start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		for c := range 40 {
			at := start.Add(time.Duration(c) * 15 * time.Second)
			a.now = func() time.Time { return at }
			rows := make([]domain.ReplicaMetrics, n)
			for i := range rows {
				rows[i] = makeReplicaMetrics(fmt.Sprintf("pod-%d", i), "variant-m", 600_000, 1_162_240, 30, 1000, out)
				rows[i].GenerationTokenRate = trueMu * out * (1 + sigma*rng.NormFloat64())
			}
			_, err := a.Analyze(logr.NewContext(t.Context(), logr.Discard()), makeAnalyzerInput(rows, states))
			require.NoError(t, err)
		}
		a.mu.Lock()
		require.Len(t, a.saturatedThroughput, 1)
		for _, ra := range a.saturatedThroughput {
			sum += ra.Median()
		}
		a.mu.Unlock()
	}
	return sum/float64(trials)/trueMu - 1
}

// A saturated throughput sample was the MAX across every replica that read
// into it, so the measured mu rose with the number of saturated replicas: the
// probe on the review tree read +5.5 % at one and +13.6 % at sixteen. An
// over-stated mu orders too few replicas, more so the bigger the fleet. The
// sample now folds re-reads of one replica by their max and replicas by their
// median, so the bias is the one-replica figure at any fleet size.
func TestMeasuredMu_DoesNotGrowWithTheSaturatedFleet(t *testing.T) {
	one := measuredMuBias(t, 1, 60)
	sixteen := measuredMuBias(t, 16, 60)
	t.Logf("window median over truth: 1 replica %+.1f %%, 16 replicas %+.1f %%", 100*one, 100*sixteen)
	assert.Less(t, sixteen-one, 0.02, "the bias does not grow with the fleet")
	assert.Less(t, sixteen, 0.08)
}

// learnedUnder runs three sample windows of a decode variant of four
// replicas, the first `saturated` of them full and queued (600 000 tokens,
// queue 30, 6.0 req/s at 500 out) and the rest at low load (50 000 tokens, no
// queue, 0.6 req/s), and returns the measured mu, the k2 window and the price.
func learnedUnder(t *testing.T, saturated int) (mu, k2, price float64) {
	t.Helper()
	states := []domain.VariantReplicaState{{VariantName: "variant-m", AcceleratorName: "H200",
		CurrentReplicas: 4, GPUsPerReplica: 1, Role: domain.RoleDecode}}
	a := NewSaturationAnalyzer(capacity.NewStore())
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for c := range 3 {
		at := start.Add(time.Duration(c) * ThroughputSampleSpacing)
		a.now = func() time.Time { return at }
		rows := make([]domain.ReplicaMetrics, 4)
		for i := range rows {
			if i < saturated {
				rows[i] = makeReplicaMetrics(fmt.Sprintf("pod-%d", i), "variant-m", 600_000, 1_162_240, 30, 1000, 500)
				rows[i].GenerationTokenRate = 6.0 * 500
			} else {
				rows[i] = makeReplicaMetrics(fmt.Sprintf("pod-%d", i), "variant-m", 50_000, 1_162_240, 0, 1000, 500)
				rows[i].GenerationTokenRate = 0.6 * 500
			}
		}
		res, err := a.Analyze(logr.NewContext(t.Context(), logr.Discard()), makeAnalyzerInput(rows, states))
		require.NoError(t, err)
		require.Len(t, res.VariantCapacities, 1)
		price = res.VariantCapacities[0].PerReplicaCapacity
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	require.Len(t, a.saturatedThroughput, 1)
	for _, ra := range a.saturatedThroughput {
		mu = ra.Median()
	}
	require.Len(t, a.computeCapacityHistory, 1)
	for _, ra := range a.computeCapacityHistory {
		k2 = ra.Average()
	}
	return mu, k2, price
}

// The router can load replicas unevenly. What one saturated replica measures
// is what a replica of the variant does, whatever its siblings are doing: the
// lightly loaded ones are not saturated readings, so they enter neither the
// throughput sample nor the k2 window, and do not pull either down.
func TestMeasuredMuAndK2_IgnoreTheIdleReplicasOfAnImbalancedFleet(t *testing.T) {
	balancedMu, balancedK2, balancedPrice := learnedUnder(t, 4)
	mu, k2, price := learnedUnder(t, 1)
	assert.InDelta(t, 6.0, balancedMu, 1e-9)
	assert.InDelta(t, 600_000, balancedK2, 1e-6)
	assert.InDelta(t, balancedMu, mu, 1e-9, "1 of 4 saturated measures the balanced fleet's mu")
	assert.InDelta(t, balancedK2, k2, 1e-6, "and its k2")
	assert.InDelta(t, balancedPrice, price, 1e-6, "and prices every replica the same")
}
