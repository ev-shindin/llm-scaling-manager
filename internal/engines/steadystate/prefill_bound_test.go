package steadystate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Issue #144 through the engine: a prefill fleet whose own window is never
// written -- its queue only ever builds on decode's back-pressure -- is ordered
// by the throughput floor on the router's arrival rate before its queue forms,
// priced on the lower bound it learned while decode metered it.
//
// The figures: one prefill pod with 6000-token prompts, queued 30 deep and
// computing 24,000 tokens/s while two decode pods are full and queued -- 4
// req/s, a lower bound on what it can do. After a restart the router's rate
// is 12 req/s, decode copes, and prefill's queue is empty. 12 / 4 is three
// replicas' worth; the growth cap allows twice the one ready replica, so the
// engine orders one.

const (
	pbDecodeKv  = int64(1_162_240)
	pbPrefillKv = int64(1_149_312)
	pbPrompt    = 6000.0
	pbScaleUp   = 0.85
	pbScaleDown = 0.70
)

func pbPod(name, variant string, kv, tokens int64, queue int, out float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName: name, VariantName: variant, ModelID: "test-model", Namespace: "test-ns", Ready: true,
		KvCacheUsage: float64(tokens) / float64(kv), KvUsageInstant: float64(tokens) / float64(kv),
		QueueLength: queue, NumGpuBlocks: kv / 16, BlockSize: 16, TotalKvCapacityTokens: kv,
		TokensInUse: tokens, AvgInputTokens: pbPrompt, AvgOutputTokens: out,
	}
}

func pbDecode(name string, tokens int64, queue int) domain.ReplicaMetrics {
	rm := pbPod(name, "dec", pbDecodeKv, tokens, queue, 1000)
	rm.RequestRate, rm.GenerationTokenRate = 4.73, 4730
	return rm
}

func pbPrefill(name string, tokens int64, queue int, computed float64) domain.ReplicaMetrics {
	rm := pbPod(name, "pre", pbPrefillKv, tokens, queue, 1)
	rm.RequestRate, rm.PrefillComputedTokenRate = computed/pbPrompt, computed
	return rm
}

func pbStore() *capacity.Store {
	store := capacity.NewStore()
	for _, v := range []string{"dec", "pre"} {
		store.Update("test-ns", "test-model", v, capacity.Record{
			EngineParams: &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256},
		}, time.Now())
	}
	return store
}

// pbCycle runs one analyzer cycle and the engine's capacity build, with the
// prefill bound on or off, and returns the prefill role's required capacity
// in replicas.
func pbCycle(t *testing.T, a *saturation.SaturationAnalyzer, on bool, lambda float64,
	rows ...domain.ReplicaMetrics) float64 {
	t.Helper()
	count := map[string]int{}
	for _, r := range rows {
		count[r.VariantName]++
	}
	meta := map[string]domain.VariantMetadata{}
	states := make([]domain.VariantReplicaState, 0, 2)
	for v, role := range map[string]string{"dec": domain.RoleDecode, "pre": domain.RolePrefill} {
		meta[v] = domain.VariantMetadata{VariantName: v, ModelID: "test-model", Namespace: "test-ns",
			Role: role, CurrentReplicas: count[v], GPUsPerReplica: 1, AcceleratorName: "H200"}
		s := meta[v].ToReplicaState()
		s.Role = role
		states = append(states, s)
	}
	in := domain.AnalyzerInput{ModelID: "test-model", Namespace: "test-ns", ReplicaMetrics: rows,
		VariantStates: states, ArrivalRate: lambda,
		Config: &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, AnalyzerName: "saturation",
			ScaleUpThreshold: pbScaleUp, ScaleDownBoundary: pbScaleDown,
			EnablePrefillThroughputBound: on}}
	ctx := logr.NewContext(context.Background(), logr.Discard())
	res, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	nr := buildNamedResult(ctx, domain.SaturationAnalyzerName, res, config.ScalingPolicy{}, meta, pbScaleUp, pbScaleDown)
	var p float64
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "pre" {
			p = vc.PerReplicaCapacity
		}
	}
	require.Positive(t, p, "prefill is priced")
	rc, ok := nr.RoleCapacities[domain.RolePrefill]
	require.True(t, ok, "a P/D result has a prefill role")
	return rc.RequiredCapacity / p
}

// pbLearnAndRestart runs the back-pressure episode on one analyzer, and hands
// what it learned to a fresh one, as a controller restart does through the
// learned-state snapshot. The episode is one cycle; its reading is entered a
// second time, a little lower, to stand for the second rate window a minute
// later -- the analyzer's own clock is not reachable from this package, and
// the floor needs two spaced readings before it may order.
func pbLearnAndRestart(t *testing.T, on bool) *saturation.SaturationAnalyzer {
	t.Helper()
	first := saturation.NewSaturationAnalyzer(pbStore())
	pbCycle(t, first, on, 6,
		pbDecode("d-0", 970_475, 36), pbDecode("d-1", 1_039_474, 81),
		pbPrefill("p-0", 357_800, 30, 4*pbPrompt))
	s := first.ExportLearnedState()
	var key string
	for k := range s.Throughput {
		if strings.Contains(k, "|prefill-bound|") {
			key = k
		}
	}
	// Not a require: on a build with no bound the restart carries nothing,
	// and the spec's own assertion is what should report it.
	if w, ok := s.Throughput[key]; ok && len(w.Values) > 0 {
		w.Values = append(w.Values, w.Values[0]*0.99)
		s.Throughput[key] = w
	} else {
		t.Log("the back-pressure episode left no prefill bound")
	}

	second := saturation.NewSaturationAnalyzer(pbStore())
	second.RestoreLearnedState(s)
	return second
}

func TestPrefillBoundOrdersPrefillBeforeItsQueueForms(t *testing.T) {
	a := pbLearnAndRestart(t, true)
	required := pbCycle(t, a, true, 12,
		pbDecode("d-0", 300_000, 0), pbDecode("d-1", 280_000, 0),
		pbPrefill("p-0", 20_000, 0, 12*pbPrompt))
	// D = 0.85 x 2 x P (the growth cap), RC = D / 0.85 - 1 = 1.
	assert.InDelta(t, 1.0, required, 1e-6, "one prefill replica ordered, with prefill's queue at zero")
}

func TestPrefillBoundOrdersNothingAtTheLoadItWasLearnedAt(t *testing.T) {
	// 6 req/s on two prefill replicas against a bound of 4: 1.5 replicas'
	// worth, 1.76 of supply -- inside the two the fleet has.
	a := pbLearnAndRestart(t, true)
	required := pbCycle(t, a, true, 6,
		pbDecode("d-0", 300_000, 0), pbDecode("d-1", 280_000, 0),
		pbPrefill("p-0", 20_000, 0, 3*pbPrompt), pbPrefill("p-1", 20_000, 0, 3*pbPrompt))
	assert.Zero(t, required)
}

func TestPrefillBoundWithoutABoundOrdersNothing(t *testing.T) {
	// The same surge on a controller that learned nothing: prefill has no mu,
	// the floor has no prefill term, and an empty queue orders nothing. This
	// is the behaviour without router-priced prefill, and with the bound off.
	a := saturation.NewSaturationAnalyzer(pbStore())
	required := pbCycle(t, a, true, 12,
		pbDecode("d-0", 300_000, 0), pbDecode("d-1", 280_000, 0),
		pbPrefill("p-0", 20_000, 0, 12*pbPrompt))
	assert.Zero(t, required)
}

// TestPrefillBoundOffIsTheDefaultAndOrdersNothing: the same back-pressure
// episode, restart and surge with the bound off -- its default -- order no
// prefill on the empty queue, which is the behaviour before the bound
// existed. TestPrefillBoundOrdersPrefillBeforeItsQueueForms is the same run on.
func TestPrefillBoundOffIsTheDefaultAndOrdersNothing(t *testing.T) {
	require.False(t, config.ScalingPolicy{}.EnablePrefillThroughputBound, "the bound is opt-in")
	a := pbLearnAndRestart(t, false)
	required := pbCycle(t, a, false, 12,
		pbDecode("d-0", 300_000, 0), pbDecode("d-1", 280_000, 0),
		pbPrefill("p-0", 20_000, 0, 12*pbPrompt))
	assert.Zero(t, required)
}
