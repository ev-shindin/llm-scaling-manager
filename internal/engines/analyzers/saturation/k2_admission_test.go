package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// admissionVariant is the one decode variant these cycles run.
const admissionVariant = "variant-d"

// k2AdmissionCycle runs one Analyze over a single decode row that is full and
// queued (queue 30 over the threshold of 5, 600 000 of 1 162 240 tokens
// resident), shaped by mutate, and returns the k2-decision priorities logged
// for the variant.
func k2AdmissionCycle(t *testing.T, analyzer *SaturationAnalyzer, totalKv int64, mutate func(*domain.ReplicaMetrics)) []string {
	t.Helper()
	seen, _ := k2AdmissionAnalyze(t, analyzer, totalKv, mutate)
	return seen
}

// k2AdmissionAnalyze is k2AdmissionCycle that also returns the price the
// variant got.
func k2AdmissionAnalyze(t *testing.T, analyzer *SaturationAnalyzer, totalKv int64, mutate func(*domain.ReplicaMetrics)) ([]string, float64) {
	t.Helper()
	ctx, logs := observedCtx(t)
	rm := makeReplicaMetrics("pod-d", admissionVariant, 600_000, totalKv, 30, 1000, 1000)
	mutate(&rm)
	result, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm},
		[]domain.VariantReplicaState{
			{VariantName: admissionVariant, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
		}))
	require.NoError(t, err)
	var seen []string
	for _, e := range logs.FilterMessage("k2-decision").All() {
		if f := e.ContextMap(); f["variant"] == admissionVariant {
			seen = append(seen, stringField(t, f, "priority"))
		}
	}
	var price float64
	for _, vc := range result.VariantCapacities {
		if vc.VariantName == admissionVariant {
			price = vc.PerReplicaCapacity
		}
	}
	return seen, price
}

func k2Windows(a *SaturationAnalyzer) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.computeCapacityHistory)
}

// A saturated reading is the variant's k2 only from a row the throughput
// write would also take: Ready, and the variant's own. The control is the
// same row Ready and own, which records.
func TestK2Admission_RecordsOnlyReadyOwnReplicas(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.ReplicaMetrics)
		want   []string
	}{
		{"a Ready replica of the variant's own", func(*domain.ReplicaMetrics) {}, []string{"P1-obs"}},
		// Declined, falls through to k1 (no window, no engine parameters), and
		// -- a bridge only -- priced at the 600 000 it holds for this cycle.
		{"a warm-pool bridge lent to the variant", func(rm *domain.ReplicaMetrics) { rm.FromWarmPool = true },
			[]string{k2ReasonObsUnadmitted, "P4-k1", k2ReasonObsUnadmitted}},
		// Declined, and priced at what the chain gives: a Pod that is not
		// Ready may be holding a frozen reading.
		{"a replica that is not Ready", func(rm *domain.ReplicaMetrics) { rm.Ready = false },
			[]string{k2ReasonObsUnadmitted, "P4-k1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			analyzer := NewSaturationAnalyzer(capacity.NewStore())
			seen := k2AdmissionCycle(t, analyzer, 1_162_240, tc.mutate)
			assert.Equal(t, tc.want, seen)
			if tc.want[0] == "P1-obs" {
				assert.Equal(t, 1, k2Windows(analyzer), "the reading opens the variant's k2 window")
				return
			}
			assert.Zero(t, k2Windows(analyzer), "nothing is written to the variant's k2 window")
		})
	}
}

// An unadmitted row still READS the variant's window: it is only the write
// that is refused. A not-Ready replica next to a measured history prices from
// it rather than from k1 -- and not from its own frozen, lower reading.
func TestK2Admission_UnadmittedRowReadsTheWindow(t *testing.T) {
	analyzer := NewSaturationAnalyzer(capacity.NewStore())
	require.Equal(t, []string{"P1-obs"}, k2AdmissionCycle(t, analyzer, 1_162_240, func(*domain.ReplicaMetrics) {}))
	seen, price := k2AdmissionAnalyze(t, analyzer, 1_162_240, func(rm *domain.ReplicaMetrics) {
		rm.Ready = false
		rm.TokensInUse = 300_000 // a frozen, lower reading that would pull the window down
		rm.KvCacheUsage = float64(rm.TokensInUse) / float64(rm.TotalKvCapacityTokens)
	})
	assert.Equal(t, []string{k2ReasonObsUnadmitted, "P2-hist"}, seen, "the window is read")
	assert.InDelta(t, 600_000, price, 0.5, "priced at the window, not at the frozen 300 000")
	analyzer.mu.Lock()
	defer analyzer.mu.Unlock()
	for _, ra := range analyzer.computeCapacityHistory {
		assert.Equal(t, 1, ra.Len(), "the declined reading was not folded in")
		assert.InDelta(t, 600_000, ra.Average(), 0.5)
	}
}

// A bridge's KV cache is the pool engine's. It does not overwrite the
// variant's live capacity record, which a parked variant and its compatible
// siblings are priced from.
func TestK2Admission_BridgeDoesNotOverwriteTheLiveRecord(t *testing.T) {
	store := capacity.NewStore()
	analyzer := NewSaturationAnalyzer(store)
	k2AdmissionCycle(t, analyzer, 1_162_240, func(*domain.ReplicaMetrics) {})
	rec := store.Get("test-ns", "test-model", admissionVariant)
	require.NotNil(t, rec)
	require.Equal(t, capacity.LearnedFromLive, rec.LearnedFrom)
	require.EqualValues(t, 1_162_240, rec.TotalKvCapacityTokens)

	// The pool runs at a lower --gpu-memory-utilization: a smaller cache.
	k2AdmissionCycle(t, analyzer, 700_000, func(rm *domain.ReplicaMetrics) { rm.FromWarmPool = true })
	rec = store.Get("test-ns", "test-model", admissionVariant)
	require.NotNil(t, rec)
	assert.EqualValues(t, 1_162_240, rec.TotalKvCapacityTokens, "the variant's own cache, not the pool engine's")

	// A not-Ready replica's cache is real -- it is the variant's own engine --
	// so its record is still filed.
	k2AdmissionCycle(t, analyzer, 1_100_000, func(rm *domain.ReplicaMetrics) { rm.Ready = false })
	rec = store.Get("test-ns", "test-model", admissionVariant)
	require.NotNil(t, rec)
	assert.EqualValues(t, 1_100_000, rec.TotalKvCapacityTokens)
}

// A bridge-only variant with no record yet gets none from the bridge.
func TestK2Admission_BridgeDoesNotCreateALiveRecord(t *testing.T) {
	store := capacity.NewStore()
	analyzer := NewSaturationAnalyzer(store)
	k2AdmissionCycle(t, analyzer, 700_000, func(rm *domain.ReplicaMetrics) { rm.FromWarmPool = true })
	if rec := store.Get("test-ns", "test-model", admissionVariant); rec != nil {
		assert.NotEqual(t, capacity.LearnedFromLive, rec.LearnedFrom)
	}
}

// A variant only a bridge is carrying is priced at what the saturated bridge
// holds, not at k1: the reading is the only one there is, and the cycle that
// showed the bridge full is the one that must not price it higher. Nothing is
// recorded from it.
func TestK2Admission_ABridgeAloneIsPricedAtWhatItHolds(t *testing.T) {
	store := capacity.NewStore()
	analyzer := NewSaturationAnalyzer(store)
	seen, price := k2AdmissionAnalyze(t, analyzer, 1_162_240, func(rm *domain.ReplicaMetrics) { rm.FromWarmPool = true })
	assert.Equal(t, []string{k2ReasonObsUnadmitted, "P4-k1", k2ReasonObsUnadmitted}, seen)
	assert.InDelta(t, 600_000, price, 0.5, "priced at the bridge's resident tokens, not at k1 (929 792)")
	assert.Zero(t, k2Windows(analyzer), "the reading is not written to the variant's window")
	if rec := store.Get("test-ns", "test-model", admissionVariant); rec != nil {
		assert.NotEqual(t, capacity.LearnedFromLive, rec.LearnedFrom, "nor filed as its live capacity")
	}
}

// Lower-only: a bridge's reading ABOVE what the chain gives does not price
// the row up. The window measured 600 000; a bridge holding 800 000 stays at
// the window's figure.
func TestK2Admission_AnUnadmittedReadingNeverPricesAboveTheChain(t *testing.T) {
	analyzer := NewSaturationAnalyzer(capacity.NewStore())
	require.Equal(t, []string{"P1-obs"}, k2AdmissionCycle(t, analyzer, 1_162_240, func(*domain.ReplicaMetrics) {}))
	seen, price := k2AdmissionAnalyze(t, analyzer, 1_162_240, func(rm *domain.ReplicaMetrics) {
		rm.FromWarmPool = true
		rm.TokensInUse = 800_000
		rm.KvCacheUsage = float64(rm.TokensInUse) / float64(rm.TotalKvCapacityTokens)
	})
	assert.Equal(t, []string{k2ReasonObsUnadmitted, "P2-hist"}, seen)
	assert.InDelta(t, 600_000, price, 0.5)
}

// A not-Ready Pod is priced at the chain and files its cache, which is the
// variant's own, at the chain's figure. Its saturated reading prices nothing:
// it may be a hang with its gauges frozen, and every cycle would re-read it.
func TestK2Admission_ANotReadyPodIsPricedAndFiledAtTheChain(t *testing.T) {
	store := capacity.NewStore()
	analyzer := NewSaturationAnalyzer(store)
	k1 := memoryBound(1_162_240, 0.8)
	for range 3 { // a hang: the same frozen reading, cycle after cycle
		_, price := k2AdmissionAnalyze(t, analyzer, 1_162_240, func(rm *domain.ReplicaMetrics) { rm.Ready = false })
		assert.InDelta(t, float64(k1), price, 0.5, "priced at the chain (k1 here), not at the frozen 600 000")
	}
	rec := store.Get("test-ns", "test-model", admissionVariant)
	require.NotNil(t, rec)
	assert.Equal(t, capacity.LearnedFromLive, rec.LearnedFrom)
	assert.EqualValues(t, k1, rec.EffectiveCapacity, "the record holds the chain's figure, not the reading")
}

// A bridge reads a sibling's lower k2 like any row, but does not start the
// variant's borrow clock: the hold is spent by the variant's own replicas.
func TestK2Admission_ABridgeDoesNotStartTheBorrowClock(t *testing.T) {
	store := capacity.NewStore()
	analyzer := NewSaturationAnalyzer(store)
	params := &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256}
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params}, time.Now())
	store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params}, time.Now())
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	run := func(b domain.ReplicaMetrics) {
		ctx, _ := observedCtx(t)
		_, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50), // variant-a measures 4000
			b,
		}, states))
		require.NoError(t, err)
	}
	clocks := func() int {
		n := 0
		for k := range borrowClocks(analyzer) {
			if ns, v, _, ok := splitVariantKey(k); ok && ns == "test-ns" && v == states[1].VariantName {
				n++
			}
		}
		return n
	}
	bridge := makeReplicaMetrics("pool-0", "variant-b", 1000, 16000, 0, 100, 50)
	bridge.FromWarmPool = true
	run(bridge)
	run(bridge)
	assert.Zero(t, clocks(), "a bridge read the borrow without starting variant-b's clock")

	run(makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50))
	assert.Equal(t, 1, clocks(), "the variant's own replica starts it")
}
