package saturation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The tests here set one analyzerTuning key on the policy Analyze is handed
// and check that the decision it names changes: the key reaches the per-replica
// pricing (computeReplicaCapacity) and the ITL tracker through the policy, not
// only through a tuning value a test builds by hand.

// tunedE1 runs one cycle of the E1 variant at at, on the shipped test policy
// with tune applied, and returns each pod's priced bucket and the cycle's
// refusal reasons. A nil tune is the shipped policy.
func tunedE1(t *testing.T, a *SaturationAnalyzer, at time.Time, rows []e1Row,
	tune func(*config.ScalingPolicy)) (map[string]string, []string) {
	t.Helper()
	a.now = func() time.Time { return at }
	rms := make([]domain.ReplicaMetrics, 0, len(rows))
	for _, r := range rows {
		rms = append(rms, e1Metrics(r))
	}
	in := makeAnalyzerInput(rms, []domain.VariantReplicaState{{
		VariantName: e1Variant, AcceleratorName: "H200", CurrentReplicas: len(rows),
		GPUsPerReplica: 1, Role: domain.RoleDecode,
	}})
	if tune != nil {
		tune(in.Config.(*config.ScalingPolicy))
	}
	ctx, logs := observedCtx(t)
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	buckets := map[string]string{}
	for _, e := range logs.FilterMessage("replica-capacity-decision").All() {
		f := e.ContextMap()
		buckets[fmt.Sprint(f["pod"])] = fmt.Sprint(f["saturatedThroughputBucket"])
	}
	var reasons []string
	for _, e := range logs.FilterMessage("saturated-throughput-refused").All() {
		reasons = append(reasons, fmt.Sprint(e.ContextMap()["reason"]))
	}
	return buckets, reasons
}

// e1Start is 10:00 on the E1 replay's day.
var e1Start = time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

// recordsAfter reports whether four saturated minutes of rows, under tune,
// leave a throughput sample on record.
func recordsAfter(t *testing.T, rows []e1Row, tune func(*config.ScalingPolicy)) (bool, []string) {
	t.Helper()
	a := newE1Analyzer(t, false)
	var reasons []string
	for i := range 4 {
		_, r := tunedE1(t, a, e1Start.Add(time.Duration(i)*time.Minute), rows, tune)
		reasons = append(reasons, r...)
	}
	return lowestThroughputSample(a) > 0, reasons
}

// analyzerTuning.admission.maxTokensOverCompletions reaches the throughput
// admission through Analyze: a saturated replica generating 1.25x the tokens
// its completions account for is filling under the default 1.15, and its rate
// is a sample under 1.30.
func TestTuningMaxTokensOverCompletionsReachesAnalyze(t *testing.T) {
	rows := []e1Row{{"p0", 30, 330_000, 3.0, 1.25 * 3.0 * 498}}
	def, reasons := recordsAfter(t, rows, nil)
	assert.False(t, def, "a replica at 1.25x was recorded under the default 1.15")
	assert.Contains(t, reasons, refusedFilling)
	tuned, reasons := recordsAfter(t, rows, func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.Admission.MaxTokensOverCompletions = 1.30
	})
	assert.True(t, tuned, "maxTokensOverCompletions 1.30 on the policy did not reach the admission; refused: %v", reasons)
}

// analyzerTuning.muChoice.maxRatio is the served bound of the throughput
// admission: a saturated reading of 1.5 beside a replica serving 3.6 is under
// half of it and refused at the default ratio of 2, and recorded at 3.
func TestTuningMaxRatioIsTheServedBound(t *testing.T) {
	rows := []e1Row{{"p0", 30, 330_000, 1.5, 1.5 * 498}, {"p1", 0, 200_000, 3.6, 3.6 * 498}}
	def, reasons := recordsAfter(t, rows, nil)
	assert.False(t, def, "1.5 beside a served 3.6 was recorded under the default maxRatio 2")
	assert.Contains(t, reasons, refusedUnderServed)
	tuned, reasons := recordsAfter(t, rows, func(p *config.ScalingPolicy) { p.AnalyzerTuning.MuChoice.MaxRatio = 3 })
	assert.True(t, tuned, "maxRatio 3 on the policy did not reach the served bound; refused: %v", reasons)
}

// analyzerTuning.admission.siblingMuTolerance reaches the sibling yield: p0
// measured at 3.3 beside three full siblings at 3.6 stands at the default 0.8
// and yields to them at 0.95.
func TestTuningSiblingMuToleranceReachesAnalyze(t *testing.T) {
	last := func(tune func(*config.ScalingPolicy)) string {
		a := newE1Analyzer(t, false)
		var b map[string]string
		for i := range 20 {
			rows := []e1Row{{"p0", 30, 330_000, 3.3, 3.3*498 + float64(i%3)}}
			for _, p := range []string{"p1", "p2", "p3"} {
				rows = append(rows, e1Row{p, 0, 330_000, 3.6, 3.6*498 + float64(i%3)})
			}
			b, _ = tunedE1(t, a, e1Start.Add(time.Duration(i)*15*time.Second), rows, tune)
		}
		return b["p0"]
	}
	assert.NotEqual(t, servedBucket, last(nil), "0.92x its siblings yielded under the default 0.8")
	assert.Equal(t, servedBucket, last(func(p *config.ScalingPolicy) { p.AnalyzerTuning.Admission.SiblingMuTolerance = 0.95 }),
		"siblingMuTolerance 0.95 on the policy did not reach the yield")
}

// analyzerTuning.muChoice.lowHoldFor reaches the measured/derived choice: a
// stale measurement of 1.2 under a derived ~3.6, 17 minutes after it last
// agreed, still prices under the default 30m hold and gives way under 10m.
func TestTuningLowHoldForReachesAnalyze(t *testing.T) {
	last := func(tune func(*config.ScalingPolicy)) string {
		a, _ := muChoiceSeed(t, 600) // last agreeing reading at 10:03
		var b map[string]string
		for i := range 17 {
			b, _ = tunedE1(t, a, e1Start.Add(4*time.Minute+time.Duration(i)*time.Minute),
				[]e1Row{{"p0", 0, 120_000, 1.2, 600}}, tune)
		}
		return b["p0"]
	}
	assert.Equal(t, "medium", last(nil), "the low measurement gave way inside the default 30m hold")
	assert.Equal(t, derivedBucket, last(func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.MuChoice.LowHoldFor = config.PolicyDuration(10 * time.Minute)
	}), "lowHoldFor 10m on the policy did not reach the choice")
}

// analyzerTuning.muChoice.returnRatio is the latch's band: a measurement of
// 1.2 gives way to derived when a replica serves 3.0 (over 2x it), and stays
// given way at a served 2.2 under the default returnRatio 1.6 (1.92). At 1.9
// (2.28) the 2.2 no longer holds it, and the fresh measurement prices again.
func TestTuningReturnRatioReachesAnalyze(t *testing.T) {
	second := func(tune func(*config.ScalingPolicy)) (string, string) {
		a, _ := muChoiceSeed(t, 600)
		first, _ := tunedE1(t, a, e1Start.Add(4*time.Minute), []e1Row{{"p0", 0, 200_000, 3.0, 3.0 * 498}}, tune)
		then, _ := tunedE1(t, a, e1Start.Add(4*time.Minute+15*time.Second), []e1Row{{"p0", 0, 200_000, 2.2, 2.2 * 498}}, tune)
		return first["p0"], then["p0"]
	}
	first, then := second(nil)
	require.Equal(t, derivedBucket, first, "premise: a served 3.0 is over twice the measured 1.2")
	assert.Equal(t, derivedBucket, then, "a served 2.2 is over the default returnRatio 1.6 of 1.2, and keeps the latch")
	first, then = second(func(p *config.ScalingPolicy) { p.AnalyzerTuning.MuChoice.ReturnRatio = 1.9 })
	require.Equal(t, derivedBucket, first, "premise: a served 3.0 is over twice the measured 1.2")
	assert.Equal(t, "medium", then, "returnRatio 1.9 on the policy did not reach the latch")
}

// analyzerTuning.muChoice.maxRatio is also how close a saturated reading must
// come to the window's median to keep the measurement fresh. The window holds
// 10 req/s against a derived ~3.6 (2.8x); then the replica reads 4.5 while
// filling, which is refused as a sample but still read for freshness. Under
// the default 2 it disagrees with 10, the measurement goes stale and gives way
// to derived; at 2.5 it agrees, and the measurement, inside 2.5x of derived
// but fresh, keeps pricing.
func TestTuningMaxRatioKeepsAMeasurementFresh(t *testing.T) {
	last := func(tune func(*config.ScalingPolicy)) string {
		a := newE1Analyzer(t, false)
		for i := range 4 {
			tunedE1(t, a, e1Start.Add(time.Duration(i)*time.Minute),
				[]e1Row{{"p0", 30, 330_000, 10, 10*498 + float64(i)}}, tune)
		}
		publishE1Line(a)
		var b map[string]string
		var reasons []string
		for i := range 11 {
			b, reasons = tunedE1(t, a, e1Start.Add(4*time.Minute+time.Duration(i)*time.Minute),
				[]e1Row{{"p0", 30, 330_000, 3.0, 4.5 * 498}}, tune)
		}
		require.Contains(t, reasons, refusedFilling, "premise: the 4.5 reading is refused, so the window stays at 10")
		return b["p0"]
	}
	assert.Equal(t, derivedBucket, last(nil), "a stale 10 at 2.8x derived kept pricing under the default maxRatio 2")
	assert.Equal(t, "medium", last(func(p *config.ScalingPolicy) { p.AnalyzerTuning.MuChoice.MaxRatio = 2.5 }),
		"maxRatio 2.5 on the policy did not keep the measurement fresh")
}

// analyzerTuning.steadyGate reaches the ITL tracker the saturation analyzer
// fits with: a replica whose KV fill moved 0.12 in 15 seconds is unsteady
// under the default maxKDrift of 0.10, and steady under 0.20.
func TestTuningSteadyGateReachesTheITLTracker(t *testing.T) {
	unsteady := func(tune func(*config.ScalingPolicy)) string {
		a := newE1Analyzer(t, false)
		var got string
		for i, k := range []float64{0.30, 0.42} {
			at := e1Start.Add(time.Duration(i) * 15 * time.Second)
			a.now = func() time.Time { return at }
			rm := makeReplicaMetrics("p0", e1Variant, int64(k*float64(e1Cache)), e1Cache, 0, 1000, 498)
			rm.KvUsageInstant, rm.KvUsageInstantKnown = k, true
			rm.AvgITL = 0.02*k + 0.01
			rm.RequestRate, rm.GenerationTokenRate = 1, 498
			in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{{
				VariantName: e1Variant, AcceleratorName: "H200", CurrentReplicas: 1,
				GPUsPerReplica: 1, Role: domain.RoleDecode,
			}})
			if tune != nil {
				tune(in.Config.(*config.ScalingPolicy))
			}
			ctx, logs := observedCtx(t)
			_, err := a.Analyze(ctx, in)
			require.NoError(t, err)
			entries := logs.FilterMessage("itl-window").All()
			require.NotEmpty(t, entries, "premise: the decode variant observes its ITL window")
			got = fmt.Sprint(entries[0].ContextMap()["unsteady"])
		}
		return got
	}
	assert.Equal(t, "1", unsteady(nil), "a 0.12 move is over the default maxKDrift 0.10")
	assert.Equal(t, "0", unsteady(func(p *config.ScalingPolicy) {
		p.AnalyzerTuning.SteadyGate = config.SteadyGateTuning{MaxKDrift: 0.20}
	}), "steadyGate.maxKDrift 0.20 on the policy did not reach the tracker")
	// The tracker's own default is the policy's.
	assert.Equal(t, itl.DefaultMaxKDrift, config.DefaultMaxKDrift)
}
