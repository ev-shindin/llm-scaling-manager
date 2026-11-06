package slo

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/prefill"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
)

var ttftNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// ttftEstimate is an estimate the model prices: V_P from busy minutes at the
// prompt the fixtures run, and a.
func ttftEstimate() prefill.Estimate {
	return prefill.Estimate{VP: 100000, VPSource: prefill.SourceBusyMinutes, A: 0.01, VPPromptTokens: 2000}
}

// fixedEstimates serves one estimate, stamped at the analyzer's clock.
type fixedEstimates struct {
	est prefill.Estimate
	now func() time.Time
}

func (f fixedEstimates) PrefillEstimate(string, string, string) (prefill.Estimate, time.Time, bool) {
	return f.est, f.now(), true
}

// prefillRows is pods Ready prefill replicas, each at rate computed tokens/s
// over 2000-token prompts.
func prefillRows(variant string, rates ...float64) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, len(rates))
	for i, r := range rates {
		out = append(out, domain.ReplicaMetrics{
			PodName: fmt.Sprintf("%s-%d", variant, i), VariantName: variant, Ready: true,
			PrefillComputedTokenRate: r, PrefillComputedTokenRateKnown: true,
			RequestRate: r / 2000, RequestRateKnown: true,
		})
	}
	return out
}

// ttftRig is an analyzer with a fixed estimate, on one model of prefill
// variant "pre" and, optionally, decode variant "dec".
type ttftRig struct {
	a   *Analyzer
	now time.Time
	in  domain.AnalyzerInput
}

func newTTFTRig(est prefill.Estimate, rows []domain.ReplicaMetrics) *ttftRig {
	r := &ttftRig{now: ttftNow}
	r.a = New(nil, nil)
	r.a.now = func() time.Time { return r.now }
	r.a.WithPrefillEstimates(fixedEstimates{est: est, now: func() time.Time { return r.now }})
	r.in = domain.AnalyzerInput{
		ModelID: "m", Namespace: "ns", Config: policy(0, 3*time.Second), ArrivalRate: 2, ReplicaMetrics: rows,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "pre", Role: domain.RolePrefill, AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: len(rows)},
		},
	}
	return r
}

func (r *ttftRig) analyze(t *testing.T) (*domain.AnalyzerResult, []metrics.SLOTTFTReading) {
	t.Helper()
	res, j, err := r.a.analyze(context.Background(), r.in)
	require.NoError(t, err)
	return res, j.Published.TTFT
}

func (r *ttftRig) one(t *testing.T) metrics.SLOTTFTReading {
	t.Helper()
	_, readings := r.analyze(t)
	require.Len(t, readings, 1)
	return readings[0]
}

// ttftInputsAt is the inputs of a priced fleet of pods with a learned f.
func ttftInputsAt(pods int, f float64) ttftInputs {
	base, u := whatif.PrefillFromEstimate(ttftEstimate())
	if u != nil {
		panic(u)
	}
	in := ttftInputs{est: ttftEstimate(), base: base, pods: pods, percent: 90, prompt: 2000, spread: promptSpread{cv: 1},
		flow: whatif.FlowControl{State: whatif.FlowControlOff}}
	if f > 0 {
		in.est.FByPods = map[int]float64{pods: f}
	}
	return in
}

var ttftTarget = whatif.TTFTTarget{Seconds: 3, Percentile: 0.9}

// testDecode is a decode role well clear of the prefill side.
func testDecode() *whatif.Decode {
	return &whatif.Decode{Line: itl.Model{A: 0.004438, B: 0.001557}, Band: whatif.DefaultBand(), CapacityTokens: 131072,
		KVPerRequest: 1500, OutputTokens: 1000, ReplicaCap: 64, Replicas: 4}
}

// Test 4 and 6: f is read at its own count, and only there.
func TestTTFTFleet_FIsLearnedAtItsOwnCount(t *testing.T) {
	in := ttftInputsAt(3, 0)
	in.est.FByPods = map[int]float64{3: 1.2}
	fl, _ := ttftFleet(in, 3)
	assert.Equal(t, 1.2, fl.Prefill.Imbalance)
	assert.False(t, fl.Prefill.ImbalanceAssumed)

	// A learned f at 3 is not read at 4: at 4 pods there are too few
	// minutes, and the assumed f is used.
	in.pods = 4
	fl, _ = ttftFleet(in, 4)
	assert.Equal(t, whatif.ImbalanceFloor, fl.Prefill.Imbalance)
	assert.True(t, fl.Prefill.ImbalanceAssumed)
	assert.Equal(t, 4, fl.Prefill.Replicas)

	// P1's F, a median over every count, is never what is priced.
	in.est.F = 3.9
	fl, _ = ttftFleet(in, 4)
	assert.Equal(t, whatif.ImbalanceFloor, fl.Prefill.Imbalance)
}

// Test 5: the assumed f, its floor and its clamp; one pod gives no f.
func TestTTFTFleet_TheAssumedF(t *testing.T) {
	for _, c := range []struct {
		name string
		pods int
		raw  float64
		want float64
	}{
		{"no share reads the floor", 3, 0, 1.5},
		{"a share under the floor reads the floor", 3, 1.2, 1.5},
		{"a share above the floor is taken, not de-biased", 4, 2.1, 2.1},
		{"clamped to the pods", 2, 3.5, 2},
		{"the floor is clamped too", 1, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := ttftInputsAt(c.pods, 0)
			in.shares.largest = c.raw
			fl, _ := ttftFleet(in, c.pods)
			assert.Equal(t, c.want, fl.Prefill.Imbalance)
			assert.Equal(t, c.pods > 1, fl.Prefill.ImbalanceAssumed, "one pod is exact, not assumed")
		})
	}
}

// Test 6: at another count, min(N', max(1.5, f)): never 1 at N' >= 2, and
// exact at one pod.
func TestTTFTFleet_FAtAnotherCount(t *testing.T) {
	for _, c := range []struct {
		name          string
		pods, atPods  int
		learned, want float64
	}{
		{"a learned f below the floor is raised", 3, 5, 1.1, 1.5},
		{"a learned f above the floor is kept", 3, 5, 2.2, 2.2},
		{"clamped to the new count", 4, 2, 3.5, 2},
		{"one pod's exact f does not reach two", 1, 2, 0, 1.5},
		{"one pod's exact f does not reach four", 1, 4, 0, 1.5},
		{"down to one pod it is exact", 3, 1, 2.2, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := ttftInputsAt(c.pods, c.learned)
			fl, _ := ttftFleet(in, c.atPods)
			assert.Equal(t, c.want, fl.Prefill.Imbalance)
			assert.Equal(t, c.atPods, fl.Prefill.Replicas)
			assert.Equal(t, c.atPods > 1, fl.Prefill.ImbalanceAssumed)
		})
	}
}

// Test 5: the raw share is the busiest pod's over the mean, and needs two
// Ready, settled rows and a rate on one of them.
func TestRawShare(t *testing.T) {
	rows := prefillRows("pre", 300, 100, 100)
	share, ok := rawShare(rows, nil, ttftNow)
	require.True(t, ok)
	assert.InDelta(t, 1.8, share, 1e-12)

	_, ok = rawShare(prefillRows("pre", 300), nil, ttftNow)
	assert.False(t, ok, "one pod gives no share")
	_, ok = rawShare(prefillRows("pre", 0, 0), nil, ttftNow)
	assert.False(t, ok, "no rate on any row")

	// A not-Ready row and a bridge are not pods the share is over.
	rows = prefillRows("pre", 300, 100, 500, 500)
	rows[2].Ready = false
	rows[3].FromWarmPool = true
	share, ok = rawShare(rows, nil, ttftNow)
	require.True(t, ok)
	assert.InDelta(t, 1.5, share, 1e-12)

	// A row with no reading counts as a pod that computed nothing: it reads
	// high, the permitted side.
	rows = prefillRows("pre", 300, 100, 100)
	rows[1].PrefillComputedTokenRateKnown = false
	share, ok = rawShare(rows, nil, ttftNow)
	require.True(t, ok)
	assert.InDelta(t, 2.25, share, 1e-12)
}

// Test 5: the window is the largest of the last 5 cycles at the Ready count,
// and resets when the count changes.
func TestNoteShare_TheWindow(t *testing.T) {
	a := New(nil, nil)
	note := func(pods int, share float64) float64 { return a.noteShare("k", pods, share, true, ttftNow).largest }
	assert.Equal(t, 1.9, note(3, 1.9))
	for range 4 {
		assert.Equal(t, 1.9, note(3, 1.2), "held for 5 cycles")
	}
	assert.Equal(t, 1.2, note(3, 1.2), "the sixth cycle has aged it out")
	assert.Equal(t, 1.9, note(3, 1.9))
	assert.Equal(t, 1.3, note(4, 1.3), "a change of the Ready count resets the window")
	assert.Equal(t, 1.3, a.noteShare("k", 4, 0, false, ttftNow).largest, "a cycle with no share leaves the window")
	assert.Equal(t, shareWindow{}, a.noteShare("k2", 4, 0, false, ttftNow), "no share yet")

	// Least, and full only at whatif.TTFTWindowCycles readings.
	var w shareWindow
	for i := range whatif.TTFTWindowCycles {
		assert.False(t, w.full, "after %d", i)
		w = a.noteShare("k3", 2, 1.7+float64(i)*0.01, true, ttftNow)
	}
	assert.True(t, w.full)
	assert.Equal(t, 1.7, w.least)
	assert.Equal(t, 1.74, w.largest)
	w = a.noteShare("k3", 2, 1.1, true, ttftNow)
	assert.True(t, w.full)
	assert.Equal(t, 1.1, w.least)
}

// The cycle's rows reach f through Analyze: the raw share of three pods.
func TestTTFT_AnalyzeReadsTheRawShare(t *testing.T) {
	r := newTTFTRig(ttftEstimate(), prefillRows("pre", 12000, 2000, 2000))
	var lines []string
	logger := funcr.New(func(prefix, args string) {
		lines = append(lines, args)
	}, funcr.Options{Verbosity: 10})
	// Past the pods' settle, which the share is read after.
	for range 8 {
		lines = nil
		r.now = r.now.Add(time.Minute)
		_, _, err := r.a.analyze(ctrl.LoggerInto(context.Background(), logr.New(logger.GetSink())), r.in)
		require.NoError(t, err)
	}
	i := slices.IndexFunc(lines, func(s string) bool { return bytes.Contains([]byte(s), []byte("slo-ttft-variant")) })
	require.GreaterOrEqual(t, i, 0, "the cycle log's line")
	for _, want := range []string{`"f"=2.25`, `"fSource"="assumed"`, `"fRaw"=2.25`, `"fRawWindowFull"=true`, `"reason"="priced"`,
		`"vpSource"="busy-minutes"`, `"cvSource"="assumed"`, `"decodeVariant"="none"`, `"flowControl"="no-series"`,
		`"pods"=3`, `"ttftRefused"=""`} {
		assert.Contains(t, lines[i], want)
	}
}

// A pod still inside its settle gives no share: the first cycle reads the
// floor, however lopsided the rows.
func TestTTFT_AnUnsettledPodGivesNoShare(t *testing.T) {
	r := newTTFTRig(ttftEstimate(), prefillRows("pre", 12000, 2000, 2000))
	r.analyze(t)
	require.Contains(t, r.a.imbalance, "ns|m|pre")
	assert.Empty(t, r.a.imbalance["ns|m|pre"].shares)
}

// Test 7: routing-bound at f / N_p >= 0.8: the prediction stays, the
// per-replica rate does not. One pod is not routing-bound: the rule would
// otherwise hold every single-pod fleet, where f / N_p is 1 by construction.
func TestTTFT_RoutingBound(t *testing.T) {
	for _, c := range []struct {
		name   string
		pods   int
		f      float64
		reason string
	}{
		{"just under", 4, 3.19, constants.SLOTTFTReasonPriced},
		{"at the share", 4, 3.2, constants.SLOTTFTReasonRoutingBound},
		{"above it", 2, 1.9, constants.SLOTTFTReasonRoutingBound},
		{"one pod, f unset", 1, 0, constants.SLOTTFTReasonPriced},
		{"one pod, f 1", 1, 1, constants.SLOTTFTReasonPriced},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := ttftInputsAt(c.pods, c.f)
			p := priceTTFT(in, 1, ttftTarget)
			require.Equal(t, c.reason, p.reason)
			assert.False(t, math.IsNaN(p.published()), "the prediction is published either way")
			assert.Equal(t, c.reason == constants.SLOTTFTReasonPriced, !math.IsNaN(p.perReplica))
		})
	}
}

// Finding T4: a raw share past 0.8 of the pods is not routing-bound until it
// has held over the whole window. Until then the rate is priced at that high
// f, the side that asks for more replicas, and flagged.
func TestTTFT_RoutingBoundMustBeSustained(t *testing.T) {
	window := func(largest, least float64, full bool) ttftInputs {
		in := ttftInputsAt(2, 0)
		in.shares = shareWindow{largest: largest, least: least, full: full}
		return in
	}
	for _, c := range []struct {
		name      string
		in        ttftInputs
		reason    string
		transient bool
	}{
		{"one cycle's chance", window(1.9, 1.2, true), constants.SLOTTFTReasonPriced, true},
		{"held, but not for the whole window", window(1.9, 1.7, false), constants.SLOTTFTReasonPriced, true},
		{"just short of it at the least", window(1.9, 1.59, true), constants.SLOTTFTReasonPriced, true},
		{"held at the share over the window", window(1.9, 1.6, true), constants.SLOTTFTReasonRoutingBound, false},
		{"under the share", window(1.5, 1.2, true), constants.SLOTTFTReasonPriced, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := priceTTFT(c.in, 1, ttftTarget)
			require.Equal(t, c.reason, p.reason)
			assert.Equal(t, c.transient, slices.Contains(p.flags, ImbalanceNotSustained))
			assert.Equal(t, c.reason == constants.SLOTTFTReasonPriced, !math.IsNaN(p.perReplica))
			if c.transient {
				assert.Equal(t, c.in.shares.largest, p.f, "priced at the high f")
			}
		})
	}
}

// Finding T1: the decode role takes the model's whole rate, whichever prefill
// variant sent it. Two equal variants are priced as the fleet they are.
func TestTTFT_TheDecodeSideTakesTheWholeRate(t *testing.T) {
	flow := whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
	variant := ttftInputsAt(1, 0)
	variant.otherPods = 1
	variant.flow, variant.decode = flow, testDecode()
	variant.decode.Replicas = 1
	whole := ttftInputsAt(2, 1)
	whole.flow, whole.decode = flow, variant.decode
	fl, _ := ttftFleet(whole, 2)
	on, ok := whatif.FlowControlOnsets(fl).(whatif.Onsets)
	require.True(t, ok)
	require.Positive(t, on.Decode)

	got := priceTTFT(variant, 0.98*on.Decode, ttftTarget)
	want, ok := whatif.TTFTAt(fl, 0.98*on.Decode, 0.9).(whatif.TTFT)
	require.True(t, ok)
	require.NotNil(t, got.at)
	assert.InDelta(t, want.Seconds, got.at.Seconds, 1e-9, "near the onset the decode wait is the whole model's")
	assert.Greater(t, got.at.Seconds, 3*priceTTFT(ttftInputsAt(1, 0), 1, ttftTarget).at.Seconds, "not one variant's share of it")

	got = priceTTFT(variant, 1.2*on.Decode, ttftTarget)
	assert.Equal(t, constants.SLOTTFTReasonAboveDecodeOnset, got.currentRefusal())
	assert.True(t, got.unmet)
	assert.True(t, math.IsNaN(got.published()))

	// The per-replica rate is the whole rate's, over every prefill replica.
	got = priceTTFT(variant, 1, ttftTarget)
	rate, ok := whatif.RateForTTFT(fl, ttftTarget).(whatif.Answer)
	require.True(t, ok)
	require.NotNil(t, got.rate)
	assert.InDelta(t, rate.Value, got.rate.Value, 1e-9)

	// Priced with no decode side: the rate per replica is over them all, so
	// it is what one pod of either variant carries.
	alone := ttftInputsAt(1, 0)
	alone.otherPods = 1
	p := priceTTFT(alone, 1, ttftTarget)
	require.Equal(t, constants.SLOTTFTReasonPriced, p.reason)
	assert.InDelta(t, p.rate.Value/2, p.perReplica, 1e-9)

	// No Ready pod of this variant is no prefill replicas, however many the
	// other variant has.
	gone := variant
	gone.pods = 0
	assert.Equal(t, constants.SLOTTFTReasonNoPrefillReplicas, priceTTFT(gone, 1, ttftTarget).reason)
}

// The same through Analyze: two equal prefill variants and one fitted decode
// line read exactly as one variant of two pods.
func TestTTFT_TheRateIsTheModelsWithADecodeSide(t *testing.T) {
	// Twelve cycles fit the line, with the pricing off: it is the slow part,
	// and only the last cycle's is read.
	fitted := func(rig func(i int) []domain.ReplicaMetrics, states []domain.VariantReplicaState, est prefill.Estimate) *ttftRig {
		r := newTTFTRig(est, rig(0))
		r.a.store = capacity.NewStore()
		r.a.store.Update("ns", "m", "dec", capacity.Record{EngineParams: &capacity.EngineParams{MaxNumSeqs: 64}}, ttftNow)
		r.in.VariantStates = states
		r.in.SchedulerQueue = &domain.SchedulerQueueMetrics{}
		r.a.ttftPrice = false
		for i := range 12 {
			r.now = r.now.Add(time.Minute)
			r.in.ReplicaMetrics = rig(i)
			r.analyze(t)
		}
		r.a.ttftPrice = true
		return r
	}
	read := func(r *ttftRig, arrival float64) metrics.SLOTTFTReading {
		r.in.ArrivalRate = arrival
		_, got := r.analyze(t)
		require.NotEmpty(t, got)
		require.NotEqual(t, constants.SLOTTFTReasonNoDecodeSide, got[0].Reason, "the line must price")
		return got[0]
	}
	state := func(v string) domain.VariantReplicaState {
		return domain.VariantReplicaState{VariantName: v, Role: domain.RolePrefill, AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 1}
	}
	// A small KV, so the decode gate closes well before the prefill knee.
	small := func(i int) []domain.ReplicaMetrics {
		rows := cycleRows("dec", i)
		for j := range rows {
			rows[j].TotalKvCapacityTokens = 8000
		}
		return rows
	}
	two := func(i int) []domain.ReplicaMetrics {
		return append(append(prefillRows("pre", 4000), prefillRows("pre2", 4000)...), small(i)...)
	}
	one := func(i int) []domain.ReplicaMetrics {
		return append(prefillRows("pre", 4000, 4000), small(i)...)
	}
	est := ttftEstimate()
	est.FByPods = map[int]float64{2: 1}
	refused := 0
	splitRig := fitted(two, []domain.VariantReplicaState{state("pre"), state("pre2"), decState}, est)
	joinedRig := fitted(one, []domain.VariantReplicaState{state("pre"), decState}, est)
	for _, arrival := range []float64{0.25, 1, 4} {
		split, joined := read(splitRig, arrival), read(joinedRig, arrival)
		assert.Equal(t, joined.Refused, split.Refused, "at %g r/s", arrival)
		assert.Equal(t, joined.Unmet, split.Unmet, "at %g r/s", arrival)
		assert.Equal(t, joined.Reason, split.Reason, "at %g r/s", arrival)
		if math.IsNaN(joined.Predicted) {
			assert.True(t, math.IsNaN(split.Predicted), "at %g r/s", arrival)
		} else {
			assert.InDelta(t, joined.Predicted, split.Predicted, 1e-9, "at %g r/s", arrival)
		}
		if split.Refused == constants.SLOTTFTReasonAboveDecodeOnset {
			refused++
		}
		t.Logf("%g r/s: %s, refused %q, predicted %.3f s", arrival, split.Reason, split.Refused, split.Predicted)
	}
	assert.Positive(t, refused, "the sweep must cross the decode onset")
}

// Finding T2/T3: a cv above the peak for the percentile, assumed or measured,
// is flagged, at every percentile.
func TestTTFT_ASpreadAboveThePeakIsFlagged(t *testing.T) {
	flagged := func(percent int, spread promptSpread) bool {
		in := ttftInputsAt(1, 0)
		in.percent, in.spread = percent, spread
		_, own := ttftFleet(in, 1)
		return slices.Contains(own, PromptSpreadAbovePeak)
	}
	assert.False(t, flagged(90, promptSpread{cv: 2.22}))
	assert.True(t, flagged(90, promptSpread{cv: 2.23}), "under the old clamp's reach")
	assert.False(t, flagged(90, promptSpread{cv: 2, assumed: true}), "the assumed 2 is under the peak at p90")
	assert.True(t, flagged(85, promptSpread{cv: 2, assumed: true}), "and over it at p85")
	assert.True(t, flagged(70, promptSpread{cv: 0.8}))
	assert.True(t, flagged(50, promptSpread{cv: 0.1}))
	assert.True(t, flagged(90, promptSpread{cv: 3, clamped: true}))
	assert.False(t, flagged(94, promptSpread{cv: 3, clamped: true}), "still rising at the stop")
	assert.False(t, flagged(99, promptSpread{cv: 3}))
	got := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
	got.in.Config.(*config.ScalingPolicy).SLO.TTFT.Percentile = 85
	assert.Contains(t, got.one(t).Flags, string(PromptSpreadAbovePeak))
}

// The table is the gamma's: the prompt's own quantile, as the model prices it
// at no load, peaks where whatif.PromptCVPeak says at each percentile.
func TestTTFT_PromptCVPeakIsTheGammas(t *testing.T) {
	floor := func(cv, q float64) float64 {
		in := ttftInputsAt(1, 0)
		in.base.FixedLatency = 1e-9
		in.spread.cv = cv
		fl, _ := ttftFleet(in, 1)
		at, ok := whatif.TTFTAt(fl, 0, q).(whatif.TTFT)
		require.True(t, ok)
		return at.Floor
	}
	for percent := 50; percent <= 99; percent++ {
		q := float64(percent) / 100
		best, at := 0.0, 0.0
		for cv := 0.0; cv <= 3.0001; cv += 0.02 {
			if f := floor(cv, q); f > best {
				best, at = f, cv
			}
		}
		assert.InDelta(t, at, whatif.PromptCVPeak(float64(percent)/100), 0.03, "p%d", percent)
	}
	assert.Equal(t, 2.22, whatif.PromptCVPeak(0.90))
	assert.Equal(t, whatif.PromptCVStop, whatif.PromptCVPeak(0.94))
	assert.Equal(t, 0.0, whatif.PromptCVPeak(0.40))
}

// Test 8: the prompt V_P was read at and the curve reach the model through
// PrefillFromEstimate, and P3's flags reach the series.
func TestTTFT_VPPromptFlags(t *testing.T) {
	for _, c := range []struct {
		name      string
		vpPrompt  float64
		curve     bool
		on, notOn whatif.Flag
	}{
		{"no curve, past 1.2x", 1600, false, whatif.VPShapeMoved, whatif.VPScaledToPrompt},
		{"no curve, at 1.2x", 2000.0 / 1.2, false, "", whatif.VPShapeMoved},
		{"no curve, within it", 2000, false, "", whatif.VPShapeMoved},
		{"no curve, shorter past 1.2x", 2500, false, whatif.VPShapeMoved, whatif.VPScaledToPrompt},
		{"no curve, unknown", 0, false, whatif.VPShapeMoved, whatif.VPScaledToPrompt},
		{"a curve rescales up", 1600, true, whatif.VPScaledToPrompt, whatif.VPShapeMoved},
		{"a curve holds down", 2500, true, "", whatif.VPScaledToPrompt},
	} {
		t.Run(c.name, func(t *testing.T) {
			est := ttftEstimate()
			est.VPPromptTokens = c.vpPrompt
			if c.curve {
				est.AModel, est.ASlope, est.ACurve = prefill.AModelQuadratic, 7e-7, 2e-10
			}
			got := newTTFTRig(est, prefillRows("pre", 4000)).one(t)
			if c.on != "" {
				assert.Contains(t, got.Flags, string(c.on))
			}
			assert.NotContains(t, got.Flags, string(c.notOn))
		})
	}
}

func bucket(u, r float64) domain.LengthBucket { return domain.LengthBucket{UpperBound: u, Rate: r} }

// Test 9: the measured spread.
func TestReadPromptSpread(t *testing.T) {
	inf := math.Inf(1)
	withBuckets := func(sets ...[]domain.LengthBucket) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, len(sets))
		for i, b := range sets {
			rows[i] = domain.ReplicaMetrics{Ready: true, ComputedTokensBuckets: b}
		}
		return rows
	}
	// 2000-token prompts of which half hit 1800 tokens: I_eff 2000 or 200, a
	// cv of 0.82, where the raw prompts have none.
	got := readPromptSpread(withBuckets([]domain.LengthBucket{bucket(200, 1), bucket(2000, 2), bucket(inf, 2)}), 1100)
	assert.False(t, got.assumed)
	assert.False(t, got.clamped)
	assert.GreaterOrEqual(t, got.cv, 0.818)

	// A bucketed sample reads at or above the true cv.
	r := rand.New(rand.NewPCG(1, 2))
	bounds := []float64{100, 200, 500, 1000, 2000, 5000, 10000, 20000, 50000, 100000}
	for range 200 {
		xs := make([]float64, 0, 400)
		for range 400 {
			xs = append(xs, 100+r.Float64()*r.Float64()*40000)
		}
		var sum, sq float64
		b := make([]domain.LengthBucket, 0, len(bounds)+1)
		for _, x := range xs {
			sum += x
			sq += x * x
		}
		for _, u := range bounds {
			n := 0.0
			for _, x := range xs {
				if x <= u {
					n++
				}
			}
			b = append(b, bucket(u, n))
		}
		b = append(b, bucket(inf, float64(len(xs))))
		n := float64(len(xs))
		mean := sum / n
		truth := math.Sqrt(sq/n/(mean*mean) - 1)
		assert.GreaterOrEqual(t, readPromptSpread(withBuckets(b), mean).cv, truth*(1-1e-12))
	}

	// Pooled over pods: each pod constant, the pool spread.
	a := []domain.LengthBucket{bucket(1000, 1), bucket(10000, 1), bucket(inf, 1)}
	b := []domain.LengthBucket{bucket(1000, 0), bucket(10000, 1), bucket(inf, 1)}
	pooled := readPromptSpread(withBuckets(a, b), 5500)
	assert.InDelta(t, math.Sqrt((1e6+1e8)/2/(5500.0*5500)-1), pooled.cv, 1e-9)

	// A not-Ready pod's histogram is not read.
	rows := withBuckets(a, b)
	rows[1].Ready = false
	assert.InDelta(t, 0, readPromptSpread(rows, 1000).cv, 1e-9)

	// Mass past the last bound has no upper bound.
	clamped := readPromptSpread(withBuckets([]domain.LengthBucket{bucket(1000, 9), bucket(inf, 10)}), 800)
	assert.Equal(t, promptSpread{cv: 3, clamped: true}, clamped)
	// A cv above 3.
	over := readPromptSpread(withBuckets([]domain.LengthBucket{bucket(100, 99), bucket(100000, 100), bucket(inf, 100)}), 1000)
	assert.Equal(t, promptSpread{cv: 3, clamped: true}, over)

	// Constant 3000-token prompts read about 1.3 in the 2000-5000 bucket.
	coarse := readPromptSpread(withBuckets([]domain.LengthBucket{bucket(2000, 0), bucket(5000, 1), bucket(inf, 1)}), 3000)
	assert.InDelta(t, 1.33, coarse.cv, 0.01)
}

// Test 10: with no histogram, cv 2, and the model flags it.
func TestTTFT_TheAssumedSpread(t *testing.T) {
	assumed := promptSpread{cv: 2, assumed: true}
	assert.Equal(t, assumed, readPromptSpread(nil, 1000))
	assert.Equal(t, assumed, readPromptSpread([]domain.ReplicaMetrics{{Ready: true}}, 1000))
	noTotal := []domain.ReplicaMetrics{{Ready: true, ComputedTokensBuckets: []domain.LengthBucket{bucket(1000, 1)}}}
	assert.Equal(t, assumed, readPromptSpread(noTotal, 1000), "no +Inf bucket: no total to read a share of")
	zero := []domain.ReplicaMetrics{{Ready: true, ComputedTokensBuckets: []domain.LengthBucket{bucket(math.Inf(1), 0)}}}
	assert.Equal(t, assumed, readPromptSpread(zero, 1000), "no request in the window")
	got := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000)).one(t)
	assert.Contains(t, got.Flags, string(whatif.AssumedPromptSpread))
}

// Test 9 through Analyze: a clamped spread is flagged, and a measured one
// lifts the assumed flag.
func TestTTFT_AMeasuredSpreadThroughAnalyze(t *testing.T) {
	rows := prefillRows("pre", 4000)
	rows[0].ComputedTokensBuckets = []domain.LengthBucket{bucket(1000, 1), bucket(math.Inf(1), 2)}
	got := newTTFTRig(ttftEstimate(), rows).one(t)
	assert.Contains(t, got.Flags, string(PromptSpreadClamped))
	assert.NotContains(t, got.Flags, string(whatif.AssumedPromptSpread))
}

// Test 11, narrowed to what holds. From cv 0.3 up to the peak for the
// percentile, a larger cv never prices a shorter TTFT at the current rate, from
// 0.05 of the knee, nor a higher rate at the target, on seeded fleets. Past the
// peak the rate side fails (TestTTFT_TheCVClaimsThatDoNotHold), and so does
// the bottom of the range; PromptSpreadAbovePeak flags the first.
func TestProperty_TTFTIsMonotoneInCV(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 13))
	const draws = 60
	priced := 0
	for range draws {
		in := ttftInputsAt(1+r.IntN(4), 0)
		in.prompt = 500 + 12000*r.Float64()
		in.base.VP = 20000 + 120000*r.Float64()
		in.base.VPPromptTokens = in.prompt
		in.base.FixedLatency = 0.003 + 0.03*r.Float64()
		in.shares.largest = 1 + r.Float64()
		if r.IntN(2) == 0 {
			in.decodeTTFT = whatif.DecodeTTFT{Seconds: 0.05 * r.Float64(), PromptTokens: in.prompt}
		}
		percent := []int{90, 91, 92, 93, 95, 99}[r.IntN(6)]
		q := float64(percent) / 100
		fl, _ := ttftFleet(in, in.pods)
		knee, ok := whatif.PrefillKnee(fl.Prefill).(whatif.Answer)
		require.True(t, ok)
		rate := knee.Value * (0.05 + 0.9*r.Float64())
		var cvs []float64
		for cv := 0.3; cv <= whatif.PromptCVPeak(float64(percent)/100)+1e-9; cv += 0.1 {
			cvs = append(cvs, cv)
		}
		var floor float64
		for _, cv := range cvs {
			in.spread.cv = cv
			fl, _ := ttftFleet(in, in.pods)
			at, ok := whatif.TTFTAt(fl, 0, q).(whatif.TTFT)
			require.True(t, ok)
			floor = max(floor, at.Floor)
		}
		// Half the draws sit just over the highest floor, where a fault shows.
		margin := 1.001
		if r.IntN(2) == 0 {
			margin = 1.1 + 4*r.Float64()
		}
		target := whatif.TTFTTarget{Seconds: floor * margin, Percentile: q}
		prevT, prevR := 0.0, math.Inf(1)
		for _, cv := range cvs {
			in.spread.cv = cv
			fl, _ := ttftFleet(in, in.pods)
			at, ok := whatif.TTFTAt(fl, rate, q).(whatif.TTFT)
			require.True(t, ok)
			require.GreaterOrEqual(t, at.Seconds, prevT*(1-1e-9), "TTFT fell as cv rose to %g: %+v at %g q %g", cv, fl.Prefill, rate, q)
			prevT = at.Seconds
			ans, ok := whatif.RateForTTFT(fl, target).(whatif.Answer)
			require.True(t, ok, "a target above every floor is answered")
			require.LessOrEqual(t, ans.Value, prevR*(1+1e-9), "the rate rose as cv rose to %g: %+v", cv, fl.Prefill)
			prevR = ans.Value
			priced++
		}
	}
	assert.Greater(t, priced, draws*10, "the rate side must be exercised")
}

// The S1 decisions take TTFT as rising with cv over [0, 3] at p90. At the
// floor, no queue, it does not: the gamma prompt's p90 peaks at cv 2.22 and
// falls 8% by cv 3 (the decision's scan started at 0.05 of the knee). So the
// stop at 3 is not the highest p90 at no load, and a target between the two
// floors is refused at cv 2.22 and answered at cv 3. At p95 and p99 the floor
// rises through 3. Pinned so a change to the stop says so.
func TestTTFT_TheP90FloorPeaksBelowCV3(t *testing.T) {
	floor := func(cv, q float64) float64 {
		in := ttftInputsAt(1, 0)
		in.base.FixedLatency = 1e-6 // the prompt's part alone
		in.spread.cv = cv
		fl, _ := ttftFleet(in, 1)
		at, ok := whatif.TTFTAt(fl, 0, q).(whatif.TTFT)
		require.True(t, ok)
		return at.Floor
	}
	assert.InDelta(t, 2.768/3.025, floor(3, 0.9)/floor(2.22, 0.9), 0.002, "8% lower at p90")
	assert.Greater(t, floor(2, 0.9), floor(3, 0.9), "the assumed 2 reads above the stop at p90")
	assert.Greater(t, floor(3, 0.95), floor(2.22, 0.95), "p95 still rises")
	assert.Greater(t, floor(3, 0.99), floor(2.22, 0.99), "p99 still rises")
}

// What the property above leaves out, pinned as false.
func TestTTFT_TheCVClaimsThatDoNotHold(t *testing.T) {
	at := func(in ttftInputs, cv, rate, q float64) float64 {
		in.spread.cv = cv
		fl, _ := ttftFleet(in, in.pods)
		r, ok := whatif.TTFTAt(fl, rate, q).(whatif.TTFT)
		require.True(t, ok)
		return r.Seconds
	}
	// At the bottom of the range: with the worst pod's utilization near 1 - q
	// a little spread prices a shorter p90 than none.
	in := ttftInputsAt(1, 0)
	fl, _ := ttftFleet(in, 1)
	knee, ok := whatif.PrefillKnee(fl.Prefill).(whatif.Answer)
	require.True(t, ok)
	rate := 0.11 * knee.Value
	assert.Less(t, at(in, 0.15, rate, 0.9), 0.96*at(in, 0, rate, 0.9), "more than 4% shorter at cv 0.15")

	// Past the peak: just over the highest floor, the rate at the target rises
	// by multiples between the peak and the stop.
	for _, percent := range []int{90, 92, 93} {
		q := float64(percent) / 100
		peak := whatif.PromptCVPeak(float64(percent) / 100)
		in := ttftInputsAt(1, 0)
		rateAt := func(cv float64, target whatif.TTFTTarget) float64 {
			in.spread.cv = cv
			fl, _ := ttftFleet(in, 1)
			a, ok := whatif.RateForTTFT(fl, target).(whatif.Answer)
			require.True(t, ok)
			return a.Value
		}
		in.spread.cv = peak
		fl, _ := ttftFleet(in, 1)
		top, ok := whatif.TTFTAt(fl, 0, q).(whatif.TTFT)
		require.True(t, ok)
		target := whatif.TTFTTarget{Seconds: 1.001 * top.Floor, Percentile: q}
		ratio := rateAt(3, target) / rateAt(peak, target)
		t.Logf("p%d: rate at cv 3 / rate at the peak %.2f, 1.001x the floor", percent, ratio)
		assert.Greater(t, ratio, 1.5, "p%d", percent)
	}
}

// Test 12: the transfer term is the mean over the rows that carry it, per
// token; omitted only with none.
func TestTransferPerToken(t *testing.T) {
	rows := []domain.ReplicaMetrics{
		{Ready: true, AvgKVTransferTime: 0.01, AvgKVTransferTimeKnown: true},
		{Ready: true, AvgKVTransferTime: 0.03, AvgKVTransferTimeKnown: true},
		{Ready: true, AvgKVTransferTime: 9},
		{AvgKVTransferTime: 9, AvgKVTransferTimeKnown: true},
	}
	assert.InDelta(t, 0.02/4000, transferPerToken(rows, 4000), 1e-15)
	assert.Equal(t, 0.0, transferPerToken(rows[2:3], 4000))
	assert.Equal(t, 0.0, transferPerToken(rows, 0))

	in := ttftInputsAt(1, 0)
	in.decode = testDecode()
	in.transfer = 2e-6
	fl, _ := ttftFleet(in, 1)
	assert.Equal(t, 2e-6, fl.Prefill.TransferSecondsPerToken)
	assert.NotContains(t, priceTTFT(in, 1, ttftTarget).flags, whatif.TransferOmitted)
	in.transfer = 0
	assert.Contains(t, priceTTFT(in, 1, ttftTarget).flags, whatif.TransferOmitted)
}

// Test 13: declared wins over the series, and a contradiction is flagged.
func TestFlowControlState(t *testing.T) {
	declared := func(v config.SLOFlowControl) *config.SLOPolicy { return &config.SLOPolicy{FlowControl: v} }
	for _, c := range []struct {
		name     string
		policy   *config.SLOPolicy
		series   bool
		state    whatif.FlowControlState
		mismatch bool
	}{
		{"unset, no series", declared(""), false, whatif.FlowControlNoSeries, false},
		{"unset, series", declared(""), true, whatif.FlowControlOn, false},
		{"no policy", nil, false, whatif.FlowControlNoSeries, false},
		{"disabled, no series", declared(config.SLOFlowControlDisabled), false, whatif.FlowControlOff, false},
		{"disabled, series", declared(config.SLOFlowControlDisabled), true, whatif.FlowControlOff, true},
		{"enabled, series", declared(config.SLOFlowControlEnabled), true, whatif.FlowControlOn, false},
		{"enabled, no series", declared(config.SLOFlowControlEnabled), false, whatif.FlowControlOn, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fc, mismatch := flowControlState(c.policy, c.series)
			assert.Equal(t, c.state, fc.State)
			assert.Equal(t, c.mismatch, mismatch)
			if c.state == whatif.FlowControlOn {
				assert.Equal(t, 5.0, fc.QueueThreshold)
				assert.Equal(t, 0.8, fc.KVThreshold)
			}
		})
	}
}

// Test 13 through Analyze: the flags, and the series the state is read from.
func TestTTFT_FlowControlThroughAnalyze(t *testing.T) {
	cfg := func(v config.SLOFlowControl) *config.ScalingPolicy {
		p := policy(0, 3*time.Second)
		p.SLO.FlowControl = v
		return p
	}
	r := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
	assert.Contains(t, r.one(t).Flags, string(whatif.FlowControlInferredOff), "unset, no series")

	r.in.Config = cfg(config.SLOFlowControlDisabled)
	got := r.one(t)
	assert.NotContains(t, got.Flags, string(whatif.FlowControlInferredOff), "declared")
	assert.NotContains(t, got.Flags, string(FlowControlMismatch))

	r.in.SchedulerQueue = &domain.SchedulerQueueMetrics{}
	got = r.one(t)
	assert.Equal(t, constants.SLOTTFTReasonPriced, got.Reason, "priced as declared")
	assert.Contains(t, got.Flags, string(FlowControlMismatch))

	r.in.SchedulerQueue = nil
	r.in.Config = cfg(config.SLOFlowControlEnabled)
	assert.Equal(t, constants.SLOTTFTReasonNoDecodeSide, r.one(t).Reason, "on, with no decode side")
}

// FlowControlPools and the wait histogram are filled only while a learner
// recording is on. The state reads SchedulerQueue alone, so a run recorded and
// the same run unrecorded price alike, which is what lets a replay stand for
// production.
func TestTTFT_TheFlowControlStateDoesNotDependOnARecording(t *testing.T) {
	for _, queue := range []*domain.SchedulerQueueMetrics{nil, {}} {
		r := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
		r.in.SchedulerQueue = queue
		unrecorded := r.one(t)
		r.in.FlowControlPools = []domain.FlowControlPool{{}}
		r.in.RouterLatency = &domain.RouterLatency{FlowControlWaitBuckets: []domain.LatencyBucket{{UpperBound: 1, Rate: 1}}}
		recorded := r.one(t)
		assert.Equal(t, unrecorded.Reason, recorded.Reason)
		assert.Equal(t, unrecorded.Flags, recorded.Flags)
	}
}

// decodeRows are n Ready decode replicas, ITL on line at fill k.
func decodeRows(variant string, n int) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		out = append(out, row(variant, fmt.Sprintf("%s-%d", variant, i), 0.3))
	}
	return out
}

var decState = domain.VariantReplicaState{VariantName: "dec", Role: domain.RoleDecode, AcceleratorName: "H100",
	GPUsPerReplica: 1, CurrentReplicas: 2}

// Test 14: the decode engine's TTFT stands in for the decode side with flow
// control off, measured or left out, and with several decode variants nothing
// does.
func TestTTFT_TheDecodeEngine(t *testing.T) {
	measured := func(rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
		for i := range rows {
			rows[i].AvgTTFT, rows[i].AvgTTFTKnown = 0.2, true
		}
		return rows
	}
	rig := func(dec []domain.ReplicaMetrics) *ttftRig {
		r := newTTFTRig(ttftEstimate(), append(prefillRows("pre", 4000), dec...))
		r.in.VariantStates = append(r.in.VariantStates, decState)
		return r
	}

	r := rig(measured(decodeRows("dec", 2)))
	got := r.one(t)
	assert.Equal(t, constants.SLOTTFTReasonPriced, got.Reason, "no line: priced with the measured TTFT")
	assert.Contains(t, got.Flags, string(whatif.DecodeEngineMeasured))
	assert.NotContains(t, got.Flags, string(whatif.DecodeSideOmitted))
	assert.NotContains(t, got.Flags, string(whatif.TransferOmitted), "the decode engine's TTFT holds the pull")

	got = rig(decodeRows("dec", 2)).one(t)
	assert.Contains(t, got.Flags, string(whatif.DecodeSideOmitted))
	assert.Contains(t, got.Flags, string(whatif.DecodeTTFTUnmeasured))
	assert.NotContains(t, got.Flags, string(whatif.DecodeEngineMeasured))

	// Two decode variants: which one is not known, so neither prices.
	r = rig(append(measured(decodeRows("dec", 2)), measured(decodeRows("dec2", 1))...))
	dec2 := decState
	dec2.VariantName = "dec2"
	r.in.VariantStates = append(r.in.VariantStates, dec2)
	got = r.one(t)
	assert.Contains(t, got.Flags, string(whatif.DecodeSideOmitted))

	// With flow control on there is no price without a decode side.
	r = rig(measured(decodeRows("dec", 2)))
	r.in.SchedulerQueue = &domain.SchedulerQueueMetrics{}
	got = r.one(t)
	assert.Equal(t, constants.SLOTTFTReasonNoDecodeSide, got.Reason)
	assert.True(t, math.IsNaN(got.Predicted))
	assert.True(t, math.IsNaN(got.PerReplica))
	assert.False(t, got.Unmet)
	assert.Empty(t, got.Flags)
}

// A decode line that prices is the decode side, with or without a TPOT target;
// a line pinned to the default intercept is not.
func TestTTFTDecode_OnlyAMeasuredLinePrices(t *testing.T) {
	a := New(nil, nil)
	fleet := fleetReading{ready: map[string]int{"dec": 2}, rows: decodeRows("dec", 2)}
	fleet.shape.KVreq, fleet.shape.AvgOutputTokens = 1500, 1000
	line := func(source string) []ttftDecodeLine {
		d := pricingDetail{source: source, band: whatif.DefaultBand()}
		d.line.Model, d.line.Tier = itl.Model{A: 0.02, B: 0.004}, itl.TierOLS
		return []ttftDecodeLine{{state: decState, detail: d}}
	}
	for source, ok := range map[string]bool{
		constants.SLOSourceOwn: true, constants.SLOSourcePinned: true, constants.SLOSourcePinnedDefault: false,
		constants.SLOSourceNoLine: false, constants.SLOSourceDeclined: false,
	} {
		got := a.ttftDecode(domain.AnalyzerInput{ModelID: "m", Namespace: "ns"}, fleet, line(source))
		assert.Equal(t, ok, got != nil, source)
		if ok {
			assert.Equal(t, 2, got.Replicas)
			assert.Equal(t, 100000.0, got.CapacityTokens)
			assert.Equal(t, 1500.0, got.KVPerRequest)
		}
	}
	bad := line(constants.SLOSourceOwn)
	bad[0].detail.line.Model.B = 0
	assert.Nil(t, a.ttftDecode(domain.AnalyzerInput{}, fleet, bad), "no intercept: a line no engine runs on")
	two := append(line(constants.SLOSourceOwn), line(constants.SLOSourceOwn)...)
	assert.Nil(t, a.ttftDecode(domain.AnalyzerInput{}, fleet, two), "two decode variants")
}

// Test 14: the decode window: the largest of the last 5 cycles, as a pair; a
// cycle with no reading ages the figure out.
func TestNoteDecodeTTFT_TheWindow(t *testing.T) {
	a := New(nil, nil)
	note := func(s, p float64) whatif.DecodeTTFT {
		return a.noteDecodeTTFT("k", whatif.DecodeTTFT{Seconds: s, PromptTokens: p}, ttftNow)
	}
	first := note(0.3, 1000)
	assert.Equal(t, whatif.DecodeTTFT{Seconds: 0.3, PromptTokens: 1000}, first)
	assert.Equal(t, first, note(0.1, 9000), "the pair from one cycle, not the largest of each")
	for range 3 {
		assert.Equal(t, first, note(0, 0))
	}
	assert.Equal(t, whatif.DecodeTTFT{Seconds: 0.1, PromptTokens: 9000}, note(0, 0))
	for range 4 {
		note(0, 0)
	}
	assert.Equal(t, whatif.DecodeTTFT{}, note(0, 0), "unmeasured again, not a stale figure")
}

func TestDecodeEngineTTFT_IsWeightedByRequests(t *testing.T) {
	rows := []domain.ReplicaMetrics{
		{Ready: true, AvgTTFT: 0.1, AvgTTFTKnown: true, RequestRate: 3},
		{Ready: true, AvgTTFT: 0.5, AvgTTFTKnown: true, RequestRate: 1},
		{Ready: true, AvgTTFT: 9, RequestRate: 9},
		{AvgTTFT: 9, AvgTTFTKnown: true, RequestRate: 9},
	}
	got, ok := decodeEngineTTFT(rows)
	require.True(t, ok)
	assert.InDelta(t, 0.2, got, 1e-12)
	rows[0].RequestRate, rows[1].RequestRate = 0, 0
	got, ok = decodeEngineTTFT(rows[:2])
	require.True(t, ok)
	assert.InDelta(t, 0.3, got, 1e-12, "equal weights with no rate")
	_, ok = decodeEngineTTFT(rows[2:3])
	assert.False(t, ok)
}

// decodeBoundAnswer is the fleet's rate answer at the target.
func decodeBoundAnswer(t *testing.T, fl whatif.TTFTFleet, target whatif.TTFTTarget) whatif.Answer {
	t.Helper()
	a, ok := whatif.RateForTTFT(fl, target).(whatif.Answer)
	require.True(t, ok)
	return a
}

// Test 15: decode-bound past 10% and not at it, with the basis as the model
// gives it.
func TestDecodeBound_TheTenPercentRule(t *testing.T) {
	in := ttftInputsAt(1, 0)
	in.decode = testDecode()
	in.decode.Replicas = 1
	fl, _ := ttftFleet(in, 1)
	base := decodeBoundAnswer(t, fl, ttftTarget)
	require.NotNil(t, base.Basis.TTFT)
	at := func(ratio float64) whatif.Answer {
		a := base
		b := *base.Basis.TTFT
		b.PrefillOnlyRate = base.Value * ratio
		b.KneeRate = b.PrefillOnlyRate * 4
		a.Basis.TTFT = &b
		a.Basis.BoundBy = []whatif.Bound{whatif.BoundTTFT, whatif.BoundDecodeAdmission}
		return a
	}
	assert.False(t, decodeBound(fl, at(1.0)))
	assert.False(t, decodeBound(fl, at(1.05)))
	assert.False(t, decodeBound(fl, at(1.1)), "at 10%")
	assert.True(t, decodeBound(fl, at(1.1001)), "past 10%")

	onset := base
	onset.Basis.BoundBy = []whatif.Bound{whatif.BoundDecodeOnset}
	assert.True(t, decodeBound(fl, onset), "the decode onset binds")
}

// Test 15, on a fleet: a decode side that holds the rate is decode-bound: the
// prediction stays, the per-replica figure does not.
func TestTTFT_DecodeBoundFleet(t *testing.T) {
	in := ttftInputsAt(1, 0)
	in.decode = testDecode()
	in.decode.Replicas = 1
	in.decode.ReplicaCap = 4 // few slots: decode's knee is far under prefill's
	p := priceTTFT(in, 0.5, whatif.TTFTTarget{Seconds: 30, Percentile: 0.9})
	require.Equal(t, constants.SLOTTFTReasonDecodeBound, p.reason)
	assert.False(t, math.IsNaN(p.published()))
	assert.True(t, math.IsNaN(p.perReplica))
}

// The onset on a fleet: flow control on, decode's onset the limit.
func TestTTFT_DecodeOnsetBindsTheRate(t *testing.T) {
	in := ttftInputsAt(1, 0)
	in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
	in.decode = testDecode()
	in.decode.Replicas = 1
	p := priceTTFT(in, 0.5, whatif.TTFTTarget{Seconds: 30, Percentile: 0.9})
	require.NotNil(t, p.rate)
	require.Contains(t, p.rate.Basis.BoundBy, whatif.BoundDecodeOnset)
	assert.Equal(t, constants.SLOTTFTReasonDecodeBound, p.reason)
}

// Test 16: a refusal at the current rate blanks the prediction and sets
// unmet; the reason stays RateForTTFT's, and the refusal stays visible.
func TestTTFT_CurrentRateRefusals(t *testing.T) {
	// Past the prefill knee: 100k tok/s over 2000 tokens is 50 r/s a pod.
	in := ttftInputsAt(1, 0)
	p := priceTTFT(in, 60, ttftTarget)
	assert.Equal(t, constants.SLOTTFTReasonPriced, p.reason)
	assert.True(t, math.IsNaN(p.published()))
	assert.False(t, math.IsNaN(p.perReplica), "only the prediction goes")
	assert.True(t, p.unmet)
	require.NotNil(t, p.atRefused)
	assert.Equal(t, whatif.Saturated, p.atRefused.Reason)
	assert.Equal(t, constants.SLOTTFTReasonSaturated, p.currentRefusal())
	assert.Contains(t, constants.SLOTTFTRefusedReasons, p.currentRefusal())

	// At or over the decode onset.
	in = ttftInputsAt(1, 0)
	in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
	in.decode = testDecode()
	in.decode.Replicas = 1
	on, ok := whatif.FlowControlOnsets(func() whatif.TTFTFleet { fl, _ := ttftFleet(in, 1); return fl }()).(whatif.Onsets)
	require.True(t, ok)
	require.Positive(t, on.Decode)
	p = priceTTFT(in, on.Decode*1.01, ttftTarget)
	assert.True(t, math.IsNaN(p.published()))
	assert.True(t, p.unmet)
	assert.Equal(t, constants.SLOTTFTReasonAboveDecodeOnset, p.currentRefusal())
	assert.Equal(t, constants.SLOTTFTReasonDecodeBound, p.reason, "the rate answer's, not the refusal's")

	// Above the prefill onset the model answers, flagged: a miss however low
	// the figure, since the per-pod model is not proven conservative there.
	in = ttftInputsAt(2, 1.5)
	in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: 0.2, KVThreshold: 0.8}
	in.decode = testDecode()
	fl, _ := ttftFleet(in, 2)
	on, ok = whatif.FlowControlOnsets(fl).(whatif.Onsets)
	require.True(t, ok)
	require.Positive(t, on.Prefill)
	require.Less(t, on.Prefill, on.Decode, "precondition: the prefill onset comes first")
	wide := whatif.TTFTTarget{Seconds: 100, Percentile: 0.9}
	p = priceTTFT(in, on.Prefill*1.01, wide)
	require.NotNil(t, p.at)
	require.True(t, p.at.HasFlag(whatif.AboveFlowControlOnset))
	assert.Less(t, p.at.Seconds, wide.Seconds)
	assert.True(t, p.unmet)
	assert.False(t, math.IsNaN(p.published()), "the figure stays: only a refusal blanks it")

	// Under every limit: met, nothing refused.
	p = priceTTFT(ttftInputsAt(1, 0), 1, ttftTarget)
	assert.False(t, p.unmet)
	assert.Empty(t, p.currentRefusal())
}

// The prediction misses the target: unmet with the figure published.
func TestTTFT_APredictionAboveTheTargetIsUnmet(t *testing.T) {
	p := priceTTFT(ttftInputsAt(1, 0), 40, whatif.TTFTTarget{Seconds: 0.2, Percentile: 0.9})
	require.NotNil(t, p.at)
	require.Greater(t, p.at.Seconds, 0.2)
	assert.True(t, p.unmet)
	assert.Equal(t, p.at.Seconds, p.published())
}

// Test 17: the reasons. Each comes from a fleet, except the three the model
// has but this path cannot reach, held by the map alone: no-imbalance (f is
// always passed), saturated and above-decode-onset (RateForTTFT stays below
// both, and a current rate past them is unmet, not a reason).
func TestTTFT_TheReasonSet(t *testing.T) {
	seen := map[string]bool{}
	fresh := func(mutate func(*ttftRig)) metrics.SLOTTFTReading {
		r := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
		r.in.Config = policy(0, 3*time.Second)
		if mutate != nil {
			mutate(r)
		}
		got := r.one(t)
		seen[got.Reason] = true
		return got
	}
	withEstimate := func(f func(*prefill.Estimate)) func(*ttftRig) {
		return func(r *ttftRig) {
			est := ttftEstimate()
			f(&est)
			r.a.WithPrefillEstimates(fixedEstimates{est: est, now: func() time.Time { return r.now }})
		}
	}
	assert.Equal(t, constants.SLOTTFTReasonPriced, fresh(nil).Reason)
	assert.Equal(t, constants.SLOTTFTReasonNoEstimate, fresh(func(r *ttftRig) { r.a.WithPrefillEstimates(nil) }).Reason)
	assert.Equal(t, constants.SLOTTFTReasonNoDecodeSide, fresh(func(r *ttftRig) { r.in.SchedulerQueue = &domain.SchedulerQueueMetrics{} }).Reason)
	assert.Equal(t, constants.SLOTTFTReasonNoPrefillCapacity, fresh(withEstimate(func(e *prefill.Estimate) {
		e.VP, e.VPRefused = 0, prefill.NoPrefillCapacity
	})).Reason)
	assert.Equal(t, constants.SLOTTFTReasonNoFixedLatency, fresh(withEstimate(func(e *prefill.Estimate) {
		e.A, e.ARefused = 0, prefill.TooFewLowLoadMinutes
	})).Reason)
	assert.Equal(t, constants.SLOTTFTReasonNoPrefillReplicas, fresh(func(r *ttftRig) { r.in.ReplicaMetrics = nil }).Reason)
	assert.Equal(t, constants.SLOTTFTReasonNoTraffic, fresh(func(r *ttftRig) {
		r.in.ReplicaMetrics = []domain.ReplicaMetrics{{PodName: "p", VariantName: "pre", Ready: true}}
	}).Reason)
	assert.Equal(t, constants.SLOTTFTReasonTargetBelowFloor, fresh(func(r *ttftRig) {
		r.in.Config = policy(0, time.Millisecond)
	}).Reason)

	price := func(mutate func(*ttftInputs)) string {
		in := ttftInputsAt(1, 0)
		in.decode = testDecode()
		if mutate != nil {
			mutate(&in)
		}
		p := priceTTFT(in, 1, ttftTarget)
		seen[p.reason] = true
		return p.reason
	}
	assert.Equal(t, constants.SLOTTFTReasonNoDecodeReplicas, price(func(in *ttftInputs) { in.decode.Replicas = 0 }))
	assert.Equal(t, constants.SLOTTFTReasonNoCapacity, price(func(in *ttftInputs) { in.decode.CapacityTokens = 0 }))
	assert.Equal(t, constants.SLOTTFTReasonNoBatchCap, price(func(in *ttftInputs) { in.decode.ReplicaCap = 0 }))
	assert.Equal(t, constants.SLOTTFTReasonTooManySlots, price(func(in *ttftInputs) {
		in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: 5, KVThreshold: 0.8}
		in.decode.ReplicaCap, in.decode.CapacityTokens, in.decode.KVPerRequest = 1<<40, 1e15, 1
	}))
	assert.Equal(t, constants.SLOTTFTReasonInputInvalid, price(func(in *ttftInputs) { in.spread.cv = -1 }))
	assert.Equal(t, constants.SLOTTFTReasonDecodeBound, price(func(in *ttftInputs) {
		in.decode.Replicas, in.decode.ReplicaCap = 1, 4
	}))
	in := ttftInputsAt(2, 1.9)
	p := priceTTFT(in, 1, ttftTarget)
	seen[p.reason] = true
	assert.Equal(t, constants.SLOTTFTReasonRoutingBound, p.reason)

	for _, r := range []whatif.Reason{whatif.NoImbalance, whatif.Saturated, whatif.AboveDecodeOnset} {
		seen[ttftReason(whatif.Unanswerable{Reason: r})] = true
	}
	for _, want := range constants.SLOTTFTReasons {
		assert.True(t, seen[want], "%s has no case", want)
	}
	assert.NotContains(t, constants.SLOTTFTReasons, "flow-control-unknown")
	assert.ElementsMatch(t, slicesOfMap(seen), constants.SLOTTFTReasons, "nothing outside the published set")
	assert.Len(t, ttftReasons, len(constants.SLOTTFTReasons)-5, "every refusal the list names has its mapping: the five others are S1's")

	// A refusal the list does not name never reads as priced.
	assert.Equal(t, constants.SLOTTFTReasonInputInvalid, ttftReason(whatif.Unanswerable{Reason: whatif.PinnedDefaultB}))
	assert.NotContains(t, ttftReasons, whatif.FlowControlUnknown)
}

func slicesOfMap(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Test 18: the flag list holds every flag the path can raise, and each name
// is the model's. A flag not on the list would be dropped from the series.
func TestTTFT_TheFlagListIsTheModels(t *testing.T) {
	flags := []whatif.Flag{whatif.UnvalidatedTTFT, whatif.AssumedVP, whatif.VPLowerBound, whatif.VPAboveSlopeBound,
		whatif.AssumedImbalance, whatif.AssumedPromptSpread, whatif.TransferOmitted, whatif.DecodeSideOmitted,
		whatif.DecodeTTFTUnmeasured, whatif.DecodeEngineMeasured, whatif.VPScaledToPrompt, whatif.VPShapeMoved,
		whatif.FlowControlInferredOff, whatif.AboveFlowControlOnset, whatif.HeldByDecodeAdmission,
		whatif.DecodeOnsetUnchecked, whatif.BelowFittedBand, whatif.KVBoundSlots,
		AssumedPoissonArrivals, PromptSpreadClamped, FlowControlMismatch, PromptSpreadAbovePeak, ImbalanceNotSustained}
	want := make([]string, len(flags))
	for i, f := range flags {
		want[i] = string(f)
	}
	assert.Equal(t, want, constants.SLOTTFTFlags)
	assert.Equal(t, []string{"UnvalidatedTTFT", "FlowControlMismatch"},
		publishedFlags([]whatif.Flag{FlowControlMismatch, "NotOnTheList", whatif.UnvalidatedTTFT}), "list order")
}

// The flags published are the rate answer's and the analyzer's own; with the
// rate refused, the prediction's; with neither, none.
func TestTTFT_WhichFlagsArePublished(t *testing.T) {
	got := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000)).one(t)
	for _, f := range []whatif.Flag{whatif.UnvalidatedTTFT, whatif.AssumedPromptSpread, whatif.TransferOmitted,
		whatif.DecodeSideOmitted, whatif.DecodeTTFTUnmeasured, whatif.FlowControlInferredOff, AssumedPoissonArrivals} {
		assert.Contains(t, got.Flags, string(f))
	}
	assert.NotContains(t, got.Flags, string(whatif.AssumedImbalance), "one pod is exact")
	assert.False(t, slices.Contains(got.Flags, string(whatif.AssumedVP)))
	assert.False(t, slices.Contains(got.Flags, string(whatif.DecodeOnsetUnchecked)))

	got = newTTFTRig(ttftEstimate(), prefillRows("pre", 4000, 4000)).one(t)
	assert.Contains(t, got.Flags, string(whatif.AssumedImbalance))

	est := ttftEstimate()
	est.VPLowerBound, est.VPAboveSlopeBound = true, true
	got = newTTFTRig(est, prefillRows("pre", 4000)).one(t)
	assert.Contains(t, got.Flags, string(whatif.VPLowerBound))
	assert.Contains(t, got.Flags, string(whatif.VPAboveSlopeBound))

	r := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
	r.in.Config = policy(0, time.Millisecond)
	got = r.one(t)
	require.Equal(t, constants.SLOTTFTReasonTargetBelowFloor, got.Reason)
	assert.Contains(t, got.Flags, string(whatif.UnvalidatedTTFT), "the prediction's, with the rate refused")
	assert.True(t, math.IsNaN(got.Predicted), "a refusal reads as a reason, not as a number")
	assert.True(t, got.Unmet, "a target below the floor is missed")

	est = ttftEstimate()
	est.VP, est.VPRefused = 0, prefill.NoPrefillCapacity
	got = newTTFTRig(est, prefillRows("pre", 4000)).one(t)
	assert.Empty(t, got.Flags)
	assert.False(t, got.Unmet)
	assert.Equal(t, constants.SLOTTFTVPNone, got.VPSource)
	assert.Equal(t, constants.SLOTTFTVPNone, vpSource(prefill.Estimate{VPSource: prefill.SourceBusyMinutes,
		VPRefused: prefill.NoPrefillCapacity}), "a refused V_P names no source")
}

// Test 19: no TTFT objective, no entry; an aggregated variant gets none.
func TestTTFT_NoObjectiveNoEntry(t *testing.T) {
	r := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
	r.in.Config = policy(100*time.Millisecond, 0)
	_, got := r.analyze(t)
	assert.Nil(t, got, "a TPOT-only policy")

	r.in.Config = policy(0, 3*time.Second)
	r.in.VariantStates[0].Role = domain.RoleBoth
	_, got = r.analyze(t)
	assert.Nil(t, got, "an aggregated variant waits for M1")
}

// Test 20: with the switch off the snapshot's TTFT is nil, and it encodes as
// it did before the field existed.
func TestTTFT_TheSwitch(t *testing.T) {
	r := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
	_, on := r.analyze(t)
	require.Len(t, on, 1)
	require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTPrice: false}))
	_, off := r.analyze(t)
	assert.Nil(t, off)

	snap := metrics.SLOModelSnapshot{Namespace: "ns", ModelName: "m"}
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	rec.Record("x", ttftNow, snap)
	assert.NotContains(t, buf.String(), "TTFT")
}

// The result is what it was: a prefill variant stays no-data and the role
// demand gains no prefill, priced or not, so no replica count can move.
func TestTTFT_TheResultDoesNotMove(t *testing.T) {
	r := newTTFTRig(ttftEstimate(), append(prefillRows("pre", 4000), decodeRows("dec", 2)...))
	r.in.VariantStates = append(r.in.VariantStates, decState)
	on, readings := r.analyze(t)
	require.Equal(t, constants.SLOTTFTReasonPriced, readings[0].Reason)
	r2 := newTTFTRig(ttftEstimate(), append(prefillRows("pre", 4000), decodeRows("dec", 2)...))
	r2.in.VariantStates = append(r2.in.VariantStates, decState)
	require.NoError(t, r2.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTPrice: false}))
	off, _ := r2.analyze(t)
	assert.Equal(t, off, on)
	assert.NotContains(t, on.RoleDemand, domain.RolePrefill)
	for _, vc := range on.VariantCapacities {
		if vc.VariantName == "pre" {
			assert.Equal(t, domain.ReasonNoData, vc.Reason)
			assert.Zero(t, vc.PerReplicaCapacity)
		}
	}
}

// The rate is the model's arrivals shared over the Ready prefill replicas.
func TestTTFT_TheRateIsTheVariantsShare(t *testing.T) {
	rows := append(prefillRows("pre", 4000, 4000, 4000), prefillRows("pre2", 4000)...)
	r := newTTFTRig(ttftEstimate(), rows)
	r.in.VariantStates = append(r.in.VariantStates, domain.VariantReplicaState{VariantName: "pre2", Role: domain.RolePrefill,
		AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 1})
	r.in.ArrivalRate = 40
	_, got := r.analyze(t)
	require.Len(t, got, 2)
	assert.Equal(t, "pre", got[0].Variant)
	assert.Equal(t, "pre2", got[1].Variant)
	assert.Equal(t, "p90", got[0].Statistic)
	assert.False(t, got[0].Unmet, "30 r/s over 3 pods, the knee 95 at f 1.5 and the margin")
	assert.False(t, math.IsNaN(got[1].Predicted), "the one pod at 10 r/s")
	// 75 and 25 r/s each stand under their knees, 95 and 47.5; 100 on either
	// would not.
	r.in.ArrivalRate = 100
	_, got = r.analyze(t)
	assert.Empty(t, got[0].Refused)
	assert.Empty(t, got[1].Refused)
	r.in.ArrivalRate = 140
	_, got = r.analyze(t)
	assert.True(t, got[0].Unmet, "105 r/s over 3 pods is past the knee")
	assert.Equal(t, constants.SLOTTFTReasonSaturated, got[0].Refused)
	assert.Equal(t, constants.SLOTTFTReasonPriced, got[0].Reason, "the reason is the answer at the target")
	assert.True(t, math.IsNaN(got[0].Predicted))
}

// The unmet contract: a target below the floor is unmet with no prediction, at
// any rate, the model's answer there or not.
func TestTTFT_ATargetBelowTheFloorIsUnmetAtAnyRate(t *testing.T) {
	for _, arrival := range []float64{0.01, 10, 500} {
		r := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
		r.in.Config = policy(0, time.Millisecond)
		r.in.ArrivalRate = arrival
		got := r.one(t)
		assert.Equal(t, constants.SLOTTFTReasonTargetBelowFloor, got.Reason)
		assert.True(t, got.Unmet, "at %g r/s", arrival)
		assert.True(t, math.IsNaN(got.Predicted), "at %g r/s", arrival)
		if arrival == 500 {
			assert.Equal(t, constants.SLOTTFTReasonSaturated, got.Refused, "past the knee too, with the rate refused")
		}
	}
	// With the target exactly at the floor the prediction does not miss it,
	// and the floor still does: no prompt spread, no load, so the two are equal.
	in := ttftInputsAt(1, 0)
	in.spread.cv = 0
	fl, _ := ttftFleet(in, 1)
	at, ok := whatif.TTFTAt(fl, 0, 0.9).(whatif.TTFT)
	require.True(t, ok)
	require.Equal(t, at.Floor, at.Seconds)
	p := priceTTFT(in, 0, whatif.TTFTTarget{Seconds: at.Floor, Percentile: 0.9})
	require.Equal(t, constants.SLOTTFTReasonTargetBelowFloor, p.reason)
	assert.True(t, p.unmet)

	// A refusal that is not about the rate is no refused rate.
	none := ttftInputsAt(1, 0)
	none.base.VP = 0
	p = priceTTFT(none, 1, ttftTarget)
	require.NotNil(t, p.atRefused)
	assert.Empty(t, p.currentRefusal())
}

// With no arrival rate from the router, the generating replicas' rate stands
// in, as the demand's does.
func TestTTFT_TheFallbackArrivalRate(t *testing.T) {
	r := newTTFTRig(ttftEstimate(), append(prefillRows("pre", 4000), decodeRows("dec", 2)...))
	r.in.VariantStates = append(r.in.VariantStates, decState)
	r.in.ArrivalRate = 0
	got := r.one(t)
	require.Equal(t, constants.SLOTTFTReasonPriced, got.Reason)
	r.in.ArrivalRate = 2 // the two decode rows' 1 r/s each
	assert.Equal(t, r.one(t).Predicted, got.Predicted)
}

// The windows are forgotten with the variant, evicted with the state timeout,
// and kept while the variant is present.
func TestTTFT_TheWindowsAreForgottenAndEvicted(t *testing.T) {
	rig := func() *ttftRig {
		rows := append(prefillRows("pre", 4000, 2000), decodeRows("dec", 2)...)
		r := newTTFTRig(ttftEstimate(), rows)
		r.in.VariantStates = append(r.in.VariantStates, decState)
		r.analyze(t)
		require.Len(t, r.a.imbalance, 1)
		require.Len(t, r.a.decodeTTFT, 1)
		return r
	}
	r := rig()
	n, _ := r.a.forgetVariant("ns", "m", "pre")
	assert.Positive(t, n)
	assert.Empty(t, r.a.imbalance)
	assert.Len(t, r.a.decodeTTFT, 1, "another variant's stays")
	r.a.forgetVariant("ns", "m", "dec")
	assert.Empty(t, r.a.decodeTTFT)

	r = rig()
	r.now = r.now.Add(r.a.stateTimeout + time.Minute)
	ev := r.a.evictStale(r.now)
	assert.Equal(t, 2, ev.TTFTWindows)
	assert.True(t, ev.Any())
	assert.Empty(t, r.a.imbalance)
	assert.Empty(t, r.a.decodeTTFT)

	r = rig()
	r.now = r.now.Add(r.a.stateTimeout - time.Minute)
	r.a.noteVariantsPresent("ns", "m", []string{"pre", "dec"})
	r.now = r.now.Add(2 * time.Minute)
	assert.Zero(t, r.a.evictStale(r.now).TTFTWindows, "a variant parked at zero is still present")
	assert.Len(t, r.a.imbalance, 1)
}

// Test 3: saturation cycles and TTFT reads together, under -race: no slo.mu
// is held across the read. Each goroutine analyzes its own model.
func TestTTFT_ConcurrentWithSaturation(t *testing.T) {
	p := newStashPair()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := stashInput(fmt.Sprintf("m%d", i))
			in.Config.(*config.ScalingPolicy).SLO = &config.SLOPolicy{TTFT: &config.SLOObjective{Target: config.SLODuration{Duration: time.Second}}}
			for range 50 {
				_, err := p.sat.Analyze(context.Background(), in)
				assert.NoError(t, err)
				_, err = p.slo.Analyze(context.Background(), in)
				assert.NoError(t, err)
			}
		}(i)
	}
	wg.Wait()
	p.slo.mu.Lock()
	defer p.slo.mu.Unlock()
	assert.Len(t, p.slo.imbalance, 4)
}

// Test 2, through Analyze: slo before saturation reads no-estimate, and
// saturation first reads P1's own figure, the reason a replay's order is
// load-bearing.
func TestTTFT_SLOBeforeSaturationReadsNoEstimate(t *testing.T) {
	p := newStashPair()
	in := stashInput("m")
	in.Config.(*config.ScalingPolicy).SLO = &config.SLOPolicy{TTFT: &config.SLOObjective{Target: config.SLODuration{Duration: time.Second}}}
	reason := func() string {
		_, j, err := p.slo.analyze(context.Background(), in)
		require.NoError(t, err)
		require.Len(t, j.Published.TTFT, 1)
		return j.Published.TTFT[0].Reason
	}
	assert.Equal(t, constants.SLOTTFTReasonNoEstimate, reason())
	_, err := p.sat.Analyze(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, constants.SLOTTFTReasonNoPrefillCapacity, reason())
	p.clk.SetTime(stashAt.Add(time.Minute))
	assert.Equal(t, constants.SLOTTFTReasonNoEstimate, reason(), "the last cycle's estimate is not this one's")
}

// The prompt is P1's reading, pooled by requests; a variant with none is
// no-traffic and not a zero.
func TestTTFT_ThePromptIsPooledByRequests(t *testing.T) {
	rows := prefillRows("pre", 4000, 12000)
	rows[1].RequestRate = 2 // 6000-token prompts at 2 r/s; 2000-token at 2 r/s
	got, ok := prefill.MeanEffectivePrompt(rows, false)
	require.True(t, ok)
	assert.InDelta(t, 4000, got, 1e-9)
}
