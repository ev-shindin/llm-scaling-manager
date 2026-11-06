package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/prefill"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
)

// goldenState is the state at a golden's cycle, 0 for the last.
func goldenState(t *testing.T, fixture string, cycle int) *State {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, fixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	st, err := StateAt(context.Background(), raw, Options{}, StateQuery{Cycle: cycle})
	require.NoError(t, err)
	return st
}

// The figures P2's comparison reads from each run's analyzer log, read here
// through the replay's state: V_P from run 2's busy minutes, a from run 4,
// each run refusing the other. The frozen figures are the TTFT page's
// ("What P1 learns on main"); P1's own log line is the same estimate.
func TestStateAt_ReadsP1Through(t *testing.T) {
	st := goldenState(t, ttftSaturatedFixture, 0)
	require.Len(t, st.Prefill, 1)
	ps := st.Prefill[0]
	require.True(t, ps.EstimateRead, "no estimate stamped with the cycle")
	assert.Equal(t, prefill.SourceBusyMinutes, ps.Estimate.VPSource)
	assert.InDelta(t, 134775, ps.Estimate.VP, 1)
	assert.Zero(t, ps.Estimate.A)
	assert.NotEmpty(t, ps.Estimate.ARefused)
	assert.Equal(t, 1, ps.Pods)
	assert.False(t, st.FlowControlSeries)

	st = goldenState(t, ttftOpenShortFixture, 0)
	require.Len(t, st.Prefill, 1)
	ps = st.Prefill[0]
	require.True(t, ps.EstimateRead)
	assert.InDelta(t, 0.01158, ps.Estimate.A, 0.00001)
	assert.NotEmpty(t, ps.Estimate.VPRefused)
	assert.Equal(t, prefill.AModelQuadratic, ps.Estimate.AModel)
	assert.Positive(t, ps.PromptTokens)
	assert.Positive(t, ps.PromptCycles)
	assert.LessOrEqual(t, ps.PromptCycles, shapeCycles)

	var decode *VariantState
	for i := range st.Variants {
		if domain.CanonicalRole(st.Variants[i].Role) == domain.RoleDecode {
			decode = &st.Variants[i]
		}
	}
	require.NotNil(t, decode, "no decode variant")
	assert.Positive(t, decode.DecodeTTFT.EngineTTFT.Seconds)
	assert.GreaterOrEqual(t, decode.DecodeTTFT.EngineTTFT.PromptTokens, whatif.MinDecodeTTFTPrompt)
	assert.Positive(t, decode.DecodeTTFT.TransferSeconds)
}

// A cycle before P1 learned a figure reads the estimate as it stood then.
func TestStateAt_TheEstimateIsTheCycles(t *testing.T) {
	early := goldenState(t, ttftOpenShortFixture, 3)
	late := goldenState(t, ttftOpenShortFixture, 0)
	require.Len(t, early.Prefill, 1)
	require.True(t, early.Prefill[0].EstimateRead)
	assert.Zero(t, early.Prefill[0].Estimate.A, "a learned by the third cycle")
	assert.Positive(t, late.Prefill[0].Estimate.A)
}

func prefillRow(pod string, computed, requests float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{VariantName: "pre", PodName: pod, Ready: true,
		PrefillComputedTokenRate: computed, PrefillComputedTokenRateKnown: true,
		RequestRate: requests, RequestRateKnown: true}
}

func decodeRow(pod string, ttft, requests float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{VariantName: "dec", PodName: pod, Ready: true, AvgTTFT: ttft, AvgTTFTKnown: true,
		RequestRate: requests, RequestRateKnown: true}
}

func pdInput(rows ...domain.ReplicaMetrics) domain.AnalyzerInput {
	return domain.AnalyzerInput{ReplicaMetrics: rows, VariantStates: []domain.VariantReplicaState{
		{VariantName: "pre", Role: domain.RolePrefill}, {VariantName: "dec", Role: domain.RoleDecode}}}
}

// The prompt is computed tokens over requests across the cycles, and a cycle
// with no traffic does not read as a prompt of nothing.
func TestTTFTSamples_Prompt(t *testing.T) {
	s := newTTFTSamples()
	s.note(pdInput(prefillRow("p0", 1000, 1)), 1)
	s.note(pdInput(prefillRow("p0", 9000, 3)), 2)
	s.note(pdInput(prefillRow("p0", 0, 0)), 3)
	tokens, from, to, cycles := promptOf(s.prompts["pre"])
	assert.InDelta(t, 2500, tokens, 1e-9)
	assert.Equal(t, []int{1, 2, 2}, []int{from, to, cycles})

	got, _, _, n := promptOf(nil)
	assert.Zero(t, got)
	assert.Zero(t, n)
}

// A row that is not serving is not read: not Ready, or a warm-pool bridge.
func TestTTFTSamples_ReadsServingRowsOnly(t *testing.T) {
	s := newTTFTSamples()
	notReady := prefillRow("p1", 100000, 1)
	notReady.Ready = false
	bridge := prefillRow("p2", 100000, 1)
	bridge.FromWarmPool = true
	s.note(pdInput(prefillRow("p0", 2000, 1), notReady, bridge), 1)
	tokens, _, _, _ := promptOf(s.prompts["pre"])
	assert.InDelta(t, 2000, tokens, 1e-9)
	require.NotNil(t, s.shares["pre"])
	assert.Equal(t, 1, s.shares["pre"].pods)
}

// f_raw is the busiest pod over the mean, a pod with no rate counting as one
// that computed nothing; the window is the largest and least over the cycles
// that read one, and restarts at another pod count.
func TestTTFTSamples_RawShare(t *testing.T) {
	share := func(rows ...domain.ReplicaMetrics) (float64, float64, bool) {
		s := newTTFTSamples()
		s.note(pdInput(rows...), 1)
		return s.shares["pre"].read()
	}
	// Four Ready pods, one unread: 3000 over 5000/4.
	unread := prefillRow("p3", 0, 1)
	unread.PrefillComputedTokenRateKnown = false
	got, _, _ := share(prefillRow("p0", 3000, 1), prefillRow("p1", 1000, 1), prefillRow("p2", 1000, 1), unread)
	assert.InDelta(t, 2.4, got, 1e-9)
	got, _, _ = share(prefillRow("p0", 0, 1), prefillRow("p1", 0, 1))
	assert.Zero(t, got, "no rate on any row")
	got, _, _ = share(prefillRow("p0", 500, 1))
	assert.Zero(t, got, "one pod has no share")

	s := newTTFTSamples()
	s.note(pdInput(prefillRow("p0", 900, 1), prefillRow("p1", 100, 1)), 1) // 1.8, on two pods
	s.note(pdInput(prefillRow("p0", 500, 1)), 2)                           // one pod: restarts
	s.note(pdInput(prefillRow("p0", 300, 1), prefillRow("p1", 200, 1)), 3) // two pods: restarts, 1.2
	s.note(pdInput(prefillRow("p0", 400, 1), prefillRow("p1", 200, 1)), 4) // 1.33
	largest, least, full := s.shares["pre"].read()
	assert.InDelta(t, 4.0/3, largest, 1e-9)
	assert.InDelta(t, 1.2, least, 1e-9)
	assert.False(t, full)

	// An idle tail forgets nothing: S1 appends only the cycles that read one.
	for c := 5; c < 5+2*whatif.TTFTWindowCycles; c++ {
		s.note(pdInput(prefillRow("p0", 0, 0), prefillRow("p1", 0, 0)), c)
	}
	largest, _, full = s.shares["pre"].read()
	assert.InDelta(t, 4.0/3, largest, 1e-9)
	assert.False(t, full)

	for c := 20; c < 20+whatif.TTFTWindowCycles; c++ {
		s.note(pdInput(prefillRow("p0", 300, 1), prefillRow("p1", 200, 1)), c)
	}
	largest, least, full = s.shares["pre"].read()
	assert.InDelta(t, 1.2, largest, 1e-9)
	assert.InDelta(t, 1.2, least, 1e-9)
	assert.True(t, full)
}

// The decode engine's TTFT is a pair: the largest cycle's mean, with that
// cycle's prompt, each mean weighted by the rows' request rates.
func TestTTFTSamples_EngineTTFTIsAPair(t *testing.T) {
	s := newTTFTSamples()
	s.note(pdInput(prefillRow("p0", 8000, 1), decodeRow("d0", 0.1, 1), decodeRow("d1", 0.3, 3)), 1) // 0.25 at 8000
	s.note(pdInput(prefillRow("p0", 2000, 1), decodeRow("d0", 0.05, 1)), 2)                         // 0.05 at 2000
	require.Len(t, s.engineTTFT["dec"], 2)
	assert.InDelta(t, 0.25, s.engineTTFT["dec"][0].Seconds, 1e-9)
	assert.InDelta(t, 8000, s.engineTTFT["dec"][0].PromptTokens, 1e-9)

	// No prompt in the cycle: nothing to pair it with, an empty slot.
	s.note(pdInput(prefillRow("p0", 0, 0), decodeRow("d0", 9, 1)), 3)
	require.Len(t, s.engineTTFT["dec"], 3)
	assert.Zero(t, s.engineTTFT["dec"][2])

	// Rows with no request rate read equally.
	u := newTTFTSamples()
	d0, d1 := decodeRow("d0", 0.1, 0), decodeRow("d1", 0.3, 0)
	u.note(pdInput(prefillRow("p0", 2000, 1), d0, d1), 1)
	assert.InDelta(t, 0.2, u.engineTTFT["dec"][0].Seconds, 1e-9)

	// The last 5 cycles, not the last 5 readings: idle cycles age a figure out.
	idle := func(c int) { s.note(pdInput(prefillRow("p0", 0, 0), decodeRow("d0", 0, 0)), c) }
	idle(10)
	idle(11)
	idle(12)
	assert.InDelta(t, 0.05, largestTTFT(s.engineTTFT["dec"]).Seconds, 1e-12, "the 0.25 has aged out")
	idle(13)
	assert.Len(t, s.engineTTFT["dec"], whatif.TTFTWindowCycles)
	assert.Zero(t, largestTTFT(s.engineTTFT["dec"]))
}

// A zero is no reading of a latency: it does not pull a mean low.
func TestTTFTSamples_ZeroReadingsAreSkipped(t *testing.T) {
	s := newTTFTSamples()
	zero := decodeRow("d1", 0, 1)
	zero.AvgKVTransferTime, zero.AvgKVTransferTimeKnown = 0, true
	d0 := decodeRow("d0", 0.1, 1)
	d0.AvgKVTransferTime, d0.AvgKVTransferTimeKnown = 0.02, true
	s.note(pdInput(prefillRow("p0", 2000, 1), d0, zero), 1)
	assert.InDelta(t, 0.1, s.engineTTFT["dec"][0].Seconds, 1e-12)
	assert.InDelta(t, 0.02, s.transfers["dec"][0], 1e-12)
}

// The figures the CLI's closing note quotes, from the frozen table: the ratio
// of predicted to observed at the assumed spread, and the most under at the
// sent one. A move of the table fails here before the note goes stale.
func TestTTFTFrozen_TheNotesFigures(t *testing.T) {
	var assumed []float64
	under := 1.0
	for _, c := range ttftFrozen {
		if c.refused != "" {
			continue
		}
		r := c.predicted / c.observed
		if c.cv == s1PromptCV {
			assumed = append(assumed, r)
		} else {
			under = min(under, r)
		}
	}
	require.Len(t, assumed, 14)
	assert.InDelta(t, 1.1, slices.Min(assumed), 0.05)
	assert.InDelta(t, 4.1, slices.Max(assumed), 0.05)
	assert.InDelta(t, 0.34, 1-under, 0.005)
	assert.InDelta(t, 0.06, knownPromptCV, 0.005)
	over := 0
	for _, r := range assumed {
		if r > 1.25 {
			over++
		}
	}
	assert.Equal(t, 13, over, "reads over at all but one level")
}

// The largest TTFT comes with its own cycle's prompt.
func TestLargestTTFT(t *testing.T) {
	assert.Equal(t, whatif.DecodeTTFT{Seconds: 0.3, PromptTokens: 500}, largestTTFT([]whatif.DecodeTTFT{
		{Seconds: 0.1, PromptTokens: 9000}, {Seconds: 0.3, PromptTokens: 500}, {Seconds: 0.2, PromptTokens: 7000}}))
	assert.Zero(t, largestTTFT(nil))
}

// An estimate stashed at another cycle is not this cycle's.
func TestEstimateAt(t *testing.T) {
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	stash := func(at time.Time, ok bool) func(string, string, string) (prefill.Estimate, time.Time, bool) {
		return func(ns, model, v string) (prefill.Estimate, time.Time, bool) {
			assert.Equal(t, []string{"ns", "m", "v"}, []string{ns, model, v})
			return prefill.Estimate{VP: 7}, at, ok
		}
	}
	est, ok := estimateAt(stash(now, true), "ns", "m", "v", now)
	assert.True(t, ok)
	assert.Equal(t, 7.0, est.VP)
	est, ok = estimateAt(stash(now.Add(-15*time.Second), true), "ns", "m", "v", now)
	assert.False(t, ok)
	assert.Zero(t, est.VP)
	_, ok = estimateAt(stash(now, false), "ns", "m", "v", now)
	assert.False(t, ok)
}

// Cycle 5 of the short run holds a wait histogram at rate 0 and no queue, on a
// run goldens.yaml describes as flow control off. The state reads no queue
// series there, as S1 does.
func TestStateAt_AZeroRateWaitHistogramIsNoFlowControl(t *testing.T) {
	assert.False(t, goldenState(t, ttftOpenShortFixture, 5).FlowControlSeries)
}

// The acceptance run holds a queue for one model of its three, and the state
// says so for that model and not for the others.
func TestStateAt_AQueueIsFlowControl(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join(goldenDir, "acceptance", RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	for model, want := range map[string]bool{"model-01": true, "model-03": false} {
		st, err := StateAt(context.Background(), raw, Options{}, StateQuery{ModelID: model})
		require.NoError(t, err)
		assert.Equal(t, want, st.FlowControlSeries, model)
	}
}

// The transfer is the mean over the rows that read one, and an unread row is
// not a zero.
func TestTTFTSamples_Transfer(t *testing.T) {
	s := newTTFTSamples()
	d0 := decodeRow("d0", 0.1, 1)
	d0.AvgKVTransferTime, d0.AvgKVTransferTimeKnown = 0.02, true
	d1 := decodeRow("d1", 0.1, 1)
	s.note(pdInput(prefillRow("p0", 2000, 1), d0, d1), 1)
	require.Len(t, s.transfers["dec"], 1)
	assert.InDelta(t, 0.02, s.transfers["dec"][0], 1e-12)
}
