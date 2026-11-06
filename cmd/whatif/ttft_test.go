package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/prefill"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/whatifsource/promtest"
)

// The prefill-stress goldens the TTFT half is read from. On these recordings
// no run gives both of P1's figures: run 4 (shortRun) learns a and refuses
// V_P, run 2 (saturatedRun) learns V_P and refuses a.
const (
	shortRun     = "../../internal/engines/analyzers/learnreplay/testdata/goldens/ttft-open-loop-short/recording.jsonl.gz"
	saturatedRun = "../../internal/engines/analyzers/learnreplay/testdata/goldens/ttft-saturated/recording.jsonl.gz"
)

// workedTTFT is the page's worked example with its TTFT flags.
func workedTTFT(args ...string) []string {
	return slices.Concat(args, []string{"--recording", olsSweep, "--model", "model-02", "--prefill", "1", "--decode", "1",
		"--assume-vp", "30000"}, workedShape)
}

// The golden outputs of the TTFT half: both questions, in text and in JSON,
// answered and refused. They are regression pins, read back from the code:
// TestTTFT_HandComputedAtCV2 is what holds a figure to the model. go test
// ./cmd/whatif -run TestTTFTGolden -update rewrites them.
func TestTTFTGolden(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"ttft-replicas-worked", slices.Concat(workedTTFT("replicas-to-slo", "--ttft-percentile", "90"),
			[]string{"--tpot-percentile", "90", "--assume-tpot-ratio", "1.3"}), 0},
		{"ttft-replicas-worked-json", slices.Concat(workedTTFT("replicas-to-slo", "--ttft-percentile", "90"), []string{"-o", "json"}), 0},
		{"ttft-rate-worked", workedTTFT("slo-to-rate", "--tpot", "4ms", "--ttft", "200ms@p95"), 0},
		{"ttft-rate-worked-json", slices.Concat(workedTTFT("slo-to-rate", "--tpot", "4ms", "--ttft", "200ms@p95"), []string{"-o", "json"}), 0},
		// Run 4: a learned, V_P assumed, no decode line, so the decode
		// engine's TTFT in its place. Decode's TPOT refuses, and so exit 3.
		{"ttft-replicas-short", []string{"replicas-to-slo", "--recording", shortRun, "--assume-vp", "130000"}, 3},
		{"ttft-replicas-short-json", []string{"replicas-to-slo", "--recording", shortRun, "--assume-vp", "130000", "-o", "json"}, 3},
		// No TPOT asked, so nothing refuses.
		{"ttft-rate-short", []string{"slo-to-rate", "--recording", shortRun, "--assume-vp", "130000", "--ttft", "1s@p90"}, 0},
		// Saturated: at the knee, a refusal and not a number.
		{"ttft-replicas-saturated", []string{"replicas-to-slo", "--recording", shortRun, "--assume-vp", "130000",
			"--rate", "1000"}, 3},
		{"ttft-rate-below-floor", []string{"slo-to-rate", "--recording", shortRun, "--assume-vp", "130000", "--ttft", "1ms"}, 3},
		// Run 4 refuses V_P, and run 2 a: each names the input.
		{"ttft-no-vp", []string{"replicas-to-slo", "--recording", shortRun, "--ttft-percentile", "90"}, 3},
		{"ttft-no-vp-json", []string{"slo-to-rate", "--recording", shortRun, "--ttft", "1s", "-o", "json"}, 3},
		{"ttft-no-a", []string{"replicas-to-slo", "--recording", saturatedRun, "--ttft-percentile", "95"}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { checkGolden(t, c.name, c.code, c.args) })
	}
}

// The text carries the page's words, and the JSON its flags and its note: on
// both questions, UNVALIDATED always and V_P ASSUMED when --assume-vp
// supplied V_P.
func TestTTFT_SaysUnvalidatedAndAssumedInTextAndJSON(t *testing.T) {
	for _, q := range [][]string{workedTTFT("replicas-to-slo", "--ttft-percentile", "90"), workedTTFT("slo-to-rate", "--ttft", "1s@p90")} {
		text, code := runCmd(t, q...)
		require.Equal(t, 0, code, text)
		assert.Contains(t, text, "UNVALIDATED; V_P ASSUMED")
		assert.Contains(t, text, "TTFT is UNVALIDATED until the staircase run (M2) and is not a bound: at the assumed prompt cv 2 it read "+
			"1.1x to 4.1x the router's percentile on the P2 recordings; at the sent spread (cv 0.06) up to 34% under it.")
		assert.Contains(t, text, "UnvalidatedTTFT")
		assert.Contains(t, text, "AssumedVP")
		// Each assumed input is named.
		for _, name := range []string{"V_P 30000 tok/s (--assume-vp)", "prompt cv 2", "Poisson arrivals", "flow control off (no queue series)"} {
			assert.Contains(t, text, name)
		}

		out, code := runCmd(t, slices.Concat(q, []string{"-o", "json"})...)
		require.Equal(t, 0, code, out)
		w := decodeOutput(t, out)
		assert.Contains(t, w.Note, "UNVALIDATED")
		assert.Contains(t, w.Note, "V_P ASSUMED")
		assert.Contains(t, w.Note, "is not a bound")
		i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill })
		require.GreaterOrEqual(t, i, 0, out)
		var flags []string
		if r := w.Roles[i].Results.TTFT; r != nil {
			require.NotNil(t, r.Answer, out)
			flags = r.Answer.Flags
		} else {
			require.NotNil(t, w.Roles[i].Results.Rate.Answer, out)
			flags = w.Roles[i].Results.Rate.Answer.Flags
		}
		for _, fl := range []whatif.Flag{whatif.UnvalidatedTTFT, whatif.AssumedVP, whatif.AssumedPromptSpread, assumedPoisson} {
			assert.Contains(t, flags, string(fl))
		}
		assert.Equal(t, "assumed", w.Roles[i].TTFT.VPSource)
		assert.Contains(t, w.Roles[i].TTFT.Assumed, "V_P 30000 tok/s (--assume-vp)")
	}
}

// The note's direction turns on the spread, and it speaks of an assumed V_P
// only where --assume-vp supplied one.
func TestTTFT_TheNoteFollowsTheSpread(t *testing.T) {
	assumed := func(vp bool) *pricedFleet {
		pf := &pricedFleet{assumedVP: vp}
		pf.fleet.Prefill.PromptCVAssumed = true
		return pf
	}
	want := "TTFT is UNVALIDATED until the staircase run (M2) and is not a bound: at the assumed prompt cv 2 it read " +
		"1.1x to 4.1x the router's percentile on the P2 recordings; at the sent spread (cv 0.06) up to 34% under it."
	assert.Equal(t, want, footerNote([]*pricedFleet{assumed(false)}))
	assert.Equal(t, want+" V_P ASSUMED.", footerNote([]*pricedFleet{assumed(false), assumed(true)}))
	// A refused fleet has no spread of its own to go by.
	assert.Equal(t, want, footerNote([]*pricedFleet{{refused: &whatif.Unanswerable{}}}))
	// A measured spread has only the sent spread's row: no word of 4.1x.
	got := footerNote([]*pricedFleet{{}})
	assert.Equal(t, "TTFT is UNVALIDATED until the staircase run (M2) and is not a bound: at the sent spread (cv 0.06) "+
		"it read up to 34% under the router's percentile on the P2 recordings.", got)
}

// The asked-for TTFT is priced from the same fleet the page's worked example
// computes by hand. The page assumes cv 1, and a 1.6 ms above P1's: the
// figures are the page's, read 1.6 ms lower as it says they would.
func TestTTFT_TheWorkedExampleFromTheRecording(t *testing.T) {
	st := workedState(t)
	f := answerFlags{question: "replicas-to-slo", set: map[string]bool{"assume-vp": true, "prefill": true, "decode": true,
		"assume-input-tokens": true, "assume-output-tokens": true}, assumeVP: 30000, prefill: 1, decode: 1,
		inputTokens: 1000, outputTokens: 1000}
	variants, err := f.decodeVariants(st)
	require.NoError(t, err)
	require.Len(t, variants, 1)
	fleets, err := f.ttftFleets(st, ttftQuestion{q: 0.9}, variants)
	require.NoError(t, err)
	require.Nil(t, fleets[0].refused)
	fl := fleets[0].fleet
	require.NotNil(t, fl.Decode, "the worked example prices decode by its line")
	near(t, "a", fl.Prefill.FixedLatency, 0.01104, 0.0001)
	assert.False(t, fl.Prefill.VPMargin != 0, "an assumed V_P carries no margin")
	fl.Prefill.PromptCV = 1

	got, ok := whatif.TTFTAt(fl, 11.32, 0.9).(whatif.TTFT)
	require.True(t, ok)
	near(t, "TTFT p90", got.Seconds, 0.160-0.0016, 0.001)
	near(t, "independent", got.Independent, 0.154-0.0016, 0.001)
	near(t, "floor", got.Floor, 0.0916-0.0016, 0.001)
	near(t, "rho", got.PrefillUtilization, 0.377, 0.001)
	got95, ok := whatif.TTFTAt(fl, 11.32, 0.95).(whatif.TTFT)
	require.True(t, ok)
	near(t, "TTFT p95", got95.Seconds, 0.231-0.0016, 0.001)

	rate, ok := whatif.RateForTTFT(fl, whatif.TTFTTarget{Seconds: 0.2, Percentile: 0.95}).(whatif.Answer)
	require.True(t, ok)
	near(t, "the rate at 200 ms p95", rate.Value, 11.10, 0.05)
}

// A figure at the CLI's cv 2, through the command, against a calculation that
// reads no Go. Run 4 at --assume-vp 130000 is a fleet of one pod with no decode
// line, so TTFT is a + q90(W + O) + D:
//
//	I 15974.91866, V_P 130000, so E[S] 0.122883990 s; rate 6.917093 r/s, rho 0.85
//	a 0.011579102 s, D 0.073850679 s (the decode engine's, I under its prompt)
//	O is a gamma of cv 2 and mean E[S]: shape 1/4, scale 4 E[S]
//	W is 0, or with probability rho exponential at r, the smaller of Kingman's
//	2(1 - rho)/(E[S](1 + cv^2)) = 0.488265 and the root of
//	lambda (M(g) - 1) = g, M(g) = (1 - 4 E[S] g)^(-1/4): 0.472663
//	P(W + O > t) = P(O > t) + rho integral_0^t f_O(x) exp(-r(t - x)) dx = 0.1
//	at t = 4.667512539 s, by scipy's quad and brentq
//
// so 4.752942 s, and the floor a + q90(O) + D is 0.454275 s. The agreement is
// the arithmetic's: it says nothing of a real pod.
func TestTTFT_HandComputedAtCV2(t *testing.T) {
	out, code := runCmd(t, "replicas-to-slo", "--recording", shortRun, "--assume-vp", "130000", "-o", "json")
	require.Equal(t, 3, code, out)
	w := decodeOutput(t, out)
	i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill })
	require.GreaterOrEqual(t, i, 0, out)
	in := w.Roles[i].TTFT
	near(t, "the prompt", in.PromptTokens, 15974.91866, 1e-4)
	near(t, "a", in.FixedLatencySeconds, 0.011579102, 1e-8)
	near(t, "cv", in.PromptCV, 2, 0)
	require.NotNil(t, in.Rate)
	near(t, "the rate", *in.Rate, 6.917093, 1e-5)
	ans := w.Roles[i].Results.TTFT.Answer
	require.NotNil(t, ans, out)
	near(t, "the decode term", ans.Parts.DecodeEngine, 0.073850679, 1e-8)
	near(t, "TTFT p90", ans.Value, 4.752942, 1e-5)
	near(t, "floor", ans.Basis.Floor, 0.454275, 1e-5)
	near(t, "rho", ans.Parts.PrefillUtilization, 0.85, 1e-9)
}

// workedState is the worked example's state, at the recording's last cycle.
func workedState(t *testing.T) *learnreplay.State {
	t.Helper()
	f := answerFlags{recording: olsSweep, model: "model-02", cycle: "last", window: captureFlags{}}
	st, err := f.readState(context.Background())
	require.NoError(t, err)
	return st
}

// learnedState is a state whose one prefill variant has both of P1's figures,
// which no committed recording does at one cycle.
func learnedState() *learnreplay.State {
	return &learnreplay.State{Namespace: "ns", ModelID: "m", Cycle: 40, Prefill: []learnreplay.PrefillState{{
		Variant: "pre", Pods: 1, EstimateRead: true, PromptTokens: 2000, PromptFromCycle: 21, PromptToCycle: 40,
		Estimate: prefill.Estimate{VP: 100000, VPSource: prefill.SourceBusyMinutes, VPPromptTokens: 2000, A: 0.01},
	}}}
}

// fleetOf reads the fleet with flags set by name, as check would mark them.
func fleetOf(t *testing.T, st *learnreplay.State, f answerFlags, set ...string) *pricedFleet {
	t.Helper()
	f.set = map[string]bool{}
	for _, name := range set {
		f.set[name] = true
	}
	ps, err := f.prefillState(st)
	require.NoError(t, err)
	return f.fleetFor(st, ps, ttftQuestion{q: 0.9}, nil)
}

// V_P's order is every --assume-* flag's: the flag, else learned, else a
// refusal. It never defaults, and the learned figure is printed beside the
// flag's.
func TestTTFT_VPOrder(t *testing.T) {
	t.Run("learned", func(t *testing.T) {
		pf := fleetOf(t, learnedState(), answerFlags{})
		require.Nil(t, pf.refused)
		assert.Equal(t, 100000.0, pf.fleet.Prefill.VP)
		assert.False(t, pf.fleet.Prefill.VPAssumed)
		assert.Equal(t, whatif.DefaultVPMargin, pf.fleet.Prefill.VPMargin)
		assert.Equal(t, "learned", pf.in.VPSource)
		assert.Equal(t, "UNVALIDATED", pf.verdict())
	})
	t.Run("--assume-vp overrides, with the learned figure beside it", func(t *testing.T) {
		st := learnedState()
		st.Prefill[0].Estimate.VPLowerBound = true
		pf := fleetOf(t, st, answerFlags{assumeVP: 130000}, "assume-vp")
		require.Nil(t, pf.refused)
		assert.Equal(t, 130000.0, pf.fleet.Prefill.VP)
		assert.True(t, pf.assumedVP)
		assert.True(t, pf.fleet.Prefill.VPAssumed)
		assert.False(t, pf.fleet.Prefill.VPLowerBound, "the lower bound was the learned figure's")
		assert.Zero(t, pf.fleet.Prefill.VPMargin)
		require.NotNil(t, pf.in.VPLearned)
		assert.Equal(t, 100000.0, *pf.in.VPLearned)
		assert.Contains(t, pf.inputsText, "V_P 130000 ASSUMED, learned 100000")
		assert.Equal(t, "UNVALIDATED; V_P ASSUMED", pf.verdict())
	})
	refusedVP := func() *learnreplay.State {
		st := learnedState()
		st.Prefill[0].Estimate.VP, st.Prefill[0].Estimate.VPRefused = 0, prefill.NoPrefillCapacity
		return st
	}
	t.Run("assumed where P1 refused it", func(t *testing.T) {
		pf := fleetOf(t, refusedVP(), answerFlags{assumeVP: 5000}, "assume-vp")
		require.Nil(t, pf.refused)
		assert.Equal(t, 5000.0, pf.fleet.Prefill.VP)
		assert.True(t, pf.fleet.Prefill.VPAssumed)
		assert.Zero(t, pf.fleet.Prefill.VPMargin)
		assert.Nil(t, pf.in.VPLearned, "P1 learned none")
		assert.Contains(t, pf.inputsText, "V_P 5000 ASSUMED;")
		assert.Equal(t, "UNVALIDATED; V_P ASSUMED", pf.verdict())
	})
	t.Run("refused with neither", func(t *testing.T) {
		pf := fleetOf(t, refusedVP(), answerFlags{})
		require.NotNil(t, pf.refused)
		assert.Equal(t, whatif.NoPrefillCapacity, pf.refused.Reason)
		assert.Contains(t, pf.refused.Detail, "--assume-vp")
		assert.Equal(t, 3, whatif.ExitCode(*pf.refused))
	})
	t.Run("no estimate at the cycle", func(t *testing.T) {
		st := learnedState()
		st.Prefill[0] = learnreplay.PrefillState{Variant: "pre", Pods: 1}
		pf := fleetOf(t, st, answerFlags{})
		require.NotNil(t, pf.refused)
		assert.Equal(t, whatif.NoPrefillCapacity, pf.refused.Reason)
		assert.Contains(t, pf.refused.Detail, "no P1 estimate")
		// --assume-vp gives V_P, not a.
		pf = fleetOf(t, st, answerFlags{assumeVP: 5000}, "assume-vp")
		require.NotNil(t, pf.refused)
		assert.Equal(t, whatif.NoFixedLatency, pf.refused.Reason)
	})
	t.Run("a has no assumption", func(t *testing.T) {
		st := learnedState()
		st.Prefill[0].Estimate.A, st.Prefill[0].Estimate.ARefused = 0, prefill.TooFewLowLoadMinutes
		pf := fleetOf(t, st, answerFlags{})
		require.NotNil(t, pf.refused)
		assert.Equal(t, whatif.NoFixedLatency, pf.refused.Reason)
	})
}

// f at a count, by S1's decision 1.
func TestTTFT_ImbalanceAt(t *testing.T) {
	learned := prefill.Estimate{FByPods: map[int]float64{3: 1.3}}
	for _, c := range []struct {
		name    string
		ps      learnreplay.PrefillState
		pods    int
		want    float64
		assumed bool
	}{
		{"one pod is exact", learnreplay.PrefillState{Pods: 1}, 1, 0, false},
		{"learned at the count", learnreplay.PrefillState{Pods: 3, Estimate: learned}, 3, 1.3, false},
		{"learned at the count, the raw share unread", learnreplay.PrefillState{Pods: 3, Estimate: learned, RawShare: 2.5}, 3, 1.3, false},
		{"raw share above the floor", learnreplay.PrefillState{Pods: 3, RawShare: 2}, 3, 2, true},
		{"raw share below the floor", learnreplay.PrefillState{Pods: 3, RawShare: 1.1}, 3, 1.5, true},
		{"no computed-token rate", learnreplay.PrefillState{Pods: 3}, 3, 1.5, true},
		{"capped at the pods", learnreplay.PrefillState{Pods: 4, RawShare: 3.9}, 2, 2, true},
		{"a learned f is not carried up unchanged", learnreplay.PrefillState{Pods: 3, Estimate: learned}, 5, 1.5, true},
		{"a learned f is not carried down unchanged", learnreplay.PrefillState{Pods: 3, Estimate: learned}, 2, 1.5, true},
		{"one pod's exact 1 does not reach two", learnreplay.PrefillState{Pods: 1, RawShare: 1}, 2, 1.5, true},
		{"down to one pod is exact", learnreplay.PrefillState{Pods: 3, Estimate: learned}, 1, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, assumed, _ := imbalanceAt(&c.ps, c.pods)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.assumed, assumed)
		})
	}
}

// An assumed f is named, and P3 flags it.
func TestTTFT_AssumedImbalanceIsNamedAndFlagged(t *testing.T) {
	pf := fleetOf(t, learnedState(), answerFlags{prefill: 3}, "prefill")
	require.Nil(t, pf.refused)
	assert.Equal(t, 3, pf.fleet.Prefill.Replicas)
	assert.Equal(t, 1.5, pf.fleet.Prefill.Imbalance)
	assert.Contains(t, pf.in.Assumed, "f 1.5 on 3 pods (none learned at that count)")

	// One learned at the count asked is a reading of that count: unflagged.
	st := learnedState()
	st.Prefill[0].Estimate.FByPods = map[int]float64{3: 1.2}
	pf2 := fleetOf(t, st, answerFlags{prefill: 3}, "prefill")
	assert.Equal(t, 1.2, pf2.fleet.Prefill.Imbalance)
	assert.False(t, pf2.fleet.Prefill.ImbalanceAssumed)
	got := pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 1, 0.9) })
	require.IsType(t, whatif.TTFT{}, got)
	assert.Contains(t, got.(whatif.TTFT).Flags, whatif.AssumedImbalance)
}

// Flow control is on where its series exist, at the upstream thresholds,
// named as assumed; else off, inferred.
func TestTTFT_FlowControl(t *testing.T) {
	st := learnedState()
	pf := fleetOf(t, st, answerFlags{})
	assert.Equal(t, whatif.FlowControlNoSeries, pf.fleet.FlowControl.State)
	assert.Contains(t, pf.in.Assumed, "flow control off (no queue series)")

	st.FlowControlSeries = true
	pf = fleetOf(t, st, answerFlags{})
	assert.Equal(t, whatif.FlowControlOn, pf.fleet.FlowControl.State)
	assert.Equal(t, 5.0, pf.fleet.FlowControl.QueueThreshold)
	assert.Equal(t, 0.8, pf.fleet.FlowControl.KVThreshold)
	assert.Contains(t, pf.in.Assumed, "flow-control thresholds 5 and 0.8 (upstream defaults)")
}

// The decode side: a line that prices, else the engine's measured TTFT, else
// nothing, which P3 flags as left out.
func TestTTFT_DecodeSide(t *testing.T) {
	st := workedState(t)
	f := answerFlags{set: map[string]bool{"assume-vp": true}, assumeVP: 30000}
	variants, err := f.decodeVariants(st)
	require.NoError(t, err)
	ps, err := f.prefillState(st)
	require.NoError(t, err)

	pf := f.fleetFor(st, ps, ttftQuestion{q: 0.9}, &variants[0])
	require.Nil(t, pf.refused, "%v", pf.refused)
	assert.Equal(t, "line", pf.in.DecodeSide)
	require.NotNil(t, pf.fleet.Decode)

	// No line: the engine's TTFT, flagged; the KV pull is in it.
	noLine := variants[0]
	noLine.decode.Line.A, noLine.decode.Line.B = 0, 0
	noLine.state.DecodeTTFT.EngineTTFT = whatif.DecodeTTFT{Seconds: 0.03, PromptTokens: 1000}
	noLine.state.DecodeTTFT.TransferSeconds = 0.01
	pf = f.fleetFor(st, ps, ttftQuestion{q: 0.9}, &noLine)
	assert.Nil(t, pf.fleet.Decode)
	assert.Equal(t, "decode engine", pf.in.DecodeSide)
	assert.Equal(t, "decode engine", pf.in.TransferSource)
	assert.Zero(t, pf.in.TransferSecondsPerToken)
	got := pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 1, 0.9) })
	require.IsType(t, whatif.TTFT{}, got)
	assert.Contains(t, got.(whatif.TTFT).Flags, whatif.DecodeEngineMeasured)

	// Neither: left out, and flagged. The transfer measured still counts.
	noLine.state.DecodeTTFT.EngineTTFT = whatif.DecodeTTFT{}
	pf = f.fleetFor(st, ps, ttftQuestion{q: 0.9}, &noLine)
	assert.Equal(t, "omitted", pf.in.DecodeSide)
	assert.Equal(t, "measured", pf.in.TransferSource)
	got = pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 1, 0.9) })
	require.IsType(t, whatif.TTFT{}, got)
	assert.Contains(t, got.(whatif.TTFT).Flags, whatif.DecodeSideOmitted)
	assert.Contains(t, got.(whatif.TTFT).Flags, whatif.DecodeTTFTUnmeasured)
}

// The prompt: assumed, else what the prefill pods computed, else the decode
// variant's; an unread one is the library's NoTraffic.
func TestTTFT_PromptSource(t *testing.T) {
	pf := fleetOf(t, learnedState(), answerFlags{})
	assert.Equal(t, 2000.0, pf.fleet.Prefill.PromptTokens)
	assert.Equal(t, "measured", pf.in.PromptSource)
	assert.Equal(t, 21, pf.in.PromptFromCycle)

	pf = fleetOf(t, learnedState(), answerFlags{inputTokens: 3000}, "assume-input-tokens")
	assert.Equal(t, 3000.0, pf.fleet.Prefill.PromptTokens)
	assert.Contains(t, pf.in.Assumed, "prompt 3000 tokens (--assume-input-tokens)")

	st := learnedState()
	st.Prefill[0].PromptTokens = 0
	pf = fleetOf(t, st, answerFlags{})
	got := pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 1, 0.9) })
	require.IsType(t, whatif.Unanswerable{}, got)
	assert.Equal(t, whatif.NoTraffic, got.(whatif.Unanswerable).Reason)
}

// Aggregated TTFT is M1: refused, and the refusal says so, whether the
// recording holds the aggregated variant alone or beside a prefill one.
func TestTTFT_AggregatedIsRefused(t *testing.T) {
	st := &learnreplay.State{Namespace: "ns", ModelID: "m", Cycle: 3,
		Variants: []learnreplay.VariantState{{Variant: "agg", Role: "both"}}}
	f := answerFlags{set: map[string]bool{"assume-vp": true}, assumeVP: 30000}
	fleets, err := f.ttftFleets(st, ttftQuestion{q: 0.9}, []pricedVariant{{state: st.Variants[0]}})
	require.NoError(t, err)
	require.NotNil(t, fleets[0].refused)
	assert.Equal(t, whatif.NoPrefillReplicas, fleets[0].refused.Reason)
	assert.Contains(t, fleets[0].refused.Detail, "aggregated TTFT is not built")

	// --variant left the aggregated variant out of what is priced.
	assert.Contains(t, noPrefillRole(st).Detail, "aggregated TTFT is not built")
	assert.Contains(t, noPrefillRole(&learnreplay.State{Namespace: "ns", ModelID: "m", Cycle: 3}).Detail, "no prefill variant at cycle 3")

	mixed := learnedState()
	mixed.Variants = st.Variants
	fleets, err = f.ttftFleets(mixed, ttftQuestion{q: 0.9}, []pricedVariant{{state: st.Variants[0]}})
	require.NoError(t, err)
	require.NotNil(t, fleets[0].refused)
	assert.Contains(t, fleets[0].refused.Detail, "aggregated TTFT is not built")

	// Through the command, from a window of aggregated pods.
	srv := sweepServer(t)
	window := sweepWindow(srv.URL)
	i := slices.Index(window, "decode=app=m-dec")
	require.GreaterOrEqual(t, i, 0)
	window[i] = "both=app=m-dec"
	out, code := runCmd(t, slices.Concat([]string{"slo-to-rate"}, window, []string{"--ttft", "1s", "--assume-vp", "30000"})...)
	require.Equal(t, 3, code, out)
	assert.Contains(t, out, "aggregated TTFT is not built")
}

// Several prefill variants are one to name: the CLI prices one fleet.
func TestTTFT_SeveralPrefillVariants(t *testing.T) {
	st := learnedState()
	st.Prefill = append(st.Prefill, learnreplay.PrefillState{Variant: "pre-b"})
	f := answerFlags{set: map[string]bool{}}
	_, err := f.prefillState(st)
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err))
	assert.Contains(t, err.Error(), "pre, pre-b")

	f.prefillVariant = "pre-b"
	ps, err := f.prefillState(st)
	require.NoError(t, err)
	assert.Equal(t, "pre-b", ps.Variant)

	f.prefillVariant = "nope"
	_, err = f.prefillState(st)
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err))
}

// Every usage error is exit 2, whatever the recording would have said, and
// with -o json one error object.
func TestTTFT_UsageErrors(t *testing.T) {
	q2 := []string{"replicas-to-slo", "--recording", shortRun}
	q1 := []string{"slo-to-rate", "--recording", shortRun, "--tpot", "4ms"}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"a percentile of 0", append(slices.Clone(q2), "--ttft-percentile", "0"), "no percentile"},
		{"a percentile of 100", append(slices.Clone(q2), "--ttft-percentile", "100"), "below 100"},
		{"a NaN percentile", append(slices.Clone(q2), "--ttft-percentile", "NaN"), "below 100"},
		{"a percentile that is no number", append(slices.Clone(q2), "--ttft-percentile", "x"), "--ttft-percentile"},
		{"--assume-vp 0", append(slices.Clone(q2), "--assume-vp", "0"), "--assume-vp"},
		{"--assume-vp NaN", append(slices.Clone(q2), "--assume-vp", "NaN"), "--assume-vp"},
		{"--assume-vp Inf", append(slices.Clone(q2), "--assume-vp", "Inf"), "--assume-vp"},
		{"--assume-vp below 0", append(slices.Clone(q2), "--assume-vp", "-1"), "--assume-vp"},
		{"--prefill below 0", append(slices.Clone(q2), "--prefill", "-1"), "--prefill"},
		{"a TTFT target on question 2", append(slices.Clone(q2), "--ttft", "1s"), "for slo-to-rate"},
		{"an unknown variant", append(slices.Clone(q2), "--prefill-variant", "nope"), "no prefill variant"},
		{"a TTFT flag with no target", append(slices.Clone(q1), "--assume-vp", "1000"), "need a --ttft target"},
		{"--prefill with no target", append(slices.Clone(q1), "--prefill", "2"), "need a --ttft target"},
		{"a target that is no duration", append(slices.Clone(q1), "--ttft", "fast"), "--ttft"},
		{"a target of 0", append(slices.Clone(q1), "--ttft", "0s"), "above 0"},
		{"a target below 0", append(slices.Clone(q1), "--ttft", "-1s"), "above 0"},
		{"two percentiles", append(slices.Clone(q1), "--ttft", "1s@p90", "--ttft-percentile", "95"), "give one"},
		{"a percentile written wrongly", append(slices.Clone(q1), "--ttft", "1s@90"), "@p90"},
		{"a target percentile of 100", append(slices.Clone(q1), "--ttft", "1s@p100"), "below 100"},
		{"a per-bucket answer", append(slices.Clone(q1), "--input-bucket", "huge"), "the planner's M1"},
		{"a headroom above 1", append(slices.Clone(q1), "--ttft", "1s", "--headroom", "1.5"), "--headroom"},
		{"a headroom of 0", append(slices.Clone(q2), "--assume-vp", "1000", "--headroom", "0"), "--headroom"},
		{"question 2 at the knee", append(slices.Clone(q2), "--assume-vp", "1000", "--headroom", "1"), "at the knee no TTFT exists"},
		{"no target at all", []string{"slo-to-rate", "--recording", shortRun}, "--tpot is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := run(context.Background(), c.args, io.Discard, io.Discard)
			require.Error(t, err)
			assert.Equal(t, 2, exitCode(err), "%v", err)
			assert.Contains(t, err.Error(), c.want)

			out, code := runCmd(t, slices.Concat(c.args, []string{"-o", "json"})...)
			assert.Equal(t, 2, code, out)
			w := decodeOutput(t, out)
			require.NotNil(t, w.Error, out)
			assert.Contains(t, w.Error.Message, c.want)
		})
	}
}

// A saturated fleet is a refusal on both exit paths, never a number.
func TestTTFT_SaturatedIsARefusal(t *testing.T) {
	out, code := runCmd(t, "replicas-to-slo", "--recording", shortRun, "--assume-vp", "130000", "--rate", "1000", "-o", "json")
	require.Equal(t, 3, code, out)
	w := decodeOutput(t, out)
	i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill })
	require.GreaterOrEqual(t, i, 0, out)
	r := w.Roles[i].Results.TTFT
	require.NotNil(t, r)
	assert.Nil(t, r.Answer, "a number at the knee")
	require.NotNil(t, r.Refused)
	assert.Equal(t, string(whatif.Saturated), r.Refused.Reason)

	text, code := runCmd(t, "replicas-to-slo", "--recording", shortRun, "--assume-vp", "130000", "--rate", "1000")
	require.Equal(t, 3, code, text)
	assert.Contains(t, text, "refused: Saturated")
	assert.NotContains(t, text, " ms   ", "a TTFT row at the knee")
}

// Question 2's operating rate is the fleet's: the prefill knee sets it where
// it is the lower, and the TPOT is read at that rate.
func TestTTFT_ThePrefillKneeBindsTheOperatingRate(t *testing.T) {
	low := workedTTFT("replicas-to-slo", "--tpot-percentile", "90", "--assume-tpot-ratio", "1.3")
	low[slices.Index(low, "30000")] = "8000" // a prefill knee of 8 r/s, under decode's 13.31
	out, code := runCmd(t, slices.Concat(low, []string{"-o", "json"})...)
	require.Equal(t, 0, code, out)
	w := decodeOutput(t, out)
	require.Len(t, w.Roles, 2)
	rate := w.Roles[0].Results.OperatingRate.Answer
	require.NotNil(t, rate, out)
	assert.InDelta(t, 6.80, rate.Value, 1e-9)
	assert.Nil(t, rate.Band, "a rate the prefill knee set has no band")
	require.NotNil(t, w.Roles[1].TTFT.Rate)
	assert.InDelta(t, 6.80, *w.Roles[1].TTFT.Rate, 1e-9)
	assert.Equal(t, sourcePrefillKnee, w.Roles[1].TTFT.RateSource)

	// A figure read at that rate is a figure of an assumed V_P, and says so.
	in := w.Roles[0].Inputs
	require.NotNil(t, in.Rate)
	assert.InDelta(t, 6.80, *in.Rate, 1e-9)
	assert.Equal(t, sourcePrefillKnee, in.RateSource)
	mean := w.Roles[0].Results.MeanTPOT.Answer
	require.NotNil(t, mean, out)
	assert.InDelta(t, 0.00238, mean.Value, 0.00001)
	for _, r := range []*wireAnswer{rate, mean, w.Roles[0].Results.PercentileTPOT.Answer} {
		assert.Contains(t, r.Flags, string(whatif.UnvalidatedTTFT))
		assert.Contains(t, r.Flags, string(whatif.AssumedVP))
	}
	assert.NotContains(t, w.Roles[0].Results.Knee.Answer.Flags, string(whatif.UnvalidatedTTFT), "decode's own knee rests on no V_P")

	text, _ := runCmd(t, low...)
	assert.Contains(t, text, "0.85 x prefill knee (scaleUpThreshold); UNVALIDATED; V_P ASSUMED")
	var tpotRows int
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "mean TPOT") || strings.HasPrefix(line, "TPOT p90") {
			assert.Contains(t, line, "at the prefill knee's rate; UNVALIDATED; V_P ASSUMED")
			tpotRows++
		}
	}
	assert.Equal(t, 2, tpotRows, text)

	// With the decode knee the lower, decode's figures are its own.
	high := workedTTFT("replicas-to-slo")
	out, code = runCmd(t, slices.Concat(high, []string{"-o", "json"})...)
	require.Equal(t, 0, code, out)
	w = decodeOutput(t, out)
	assert.Equal(t, "headroom x decode knee", w.Roles[1].TTFT.RateSource)
	assert.Nil(t, w.Roles[0].Inputs.Rate)
	assert.Empty(t, w.Roles[0].Inputs.RateSource)
	assert.NotContains(t, w.Roles[0].Results.MeanTPOT.Answer.Flags, string(whatif.UnvalidatedTTFT))
	text, _ = runCmd(t, high...)
	assert.NotContains(t, text, "at the prefill knee's rate")
}

// The knee row spells the knee, margin and all, at the V_P priced.
func TestTTFT_TheKneeRowSpellsTheKnee(t *testing.T) {
	st := learnedState()
	st.Prefill[0].PromptTokens = 4000
	st.Prefill[0].Estimate.AModel = prefill.AModelQuadratic
	st.Prefill[0].Estimate.ASlope, st.Prefill[0].Estimate.ACurve = 1e-6, 1e-10
	pf := fleetOf(t, st, answerFlags{})
	k, ok := pf.knee().(whatif.Answer)
	require.True(t, ok)
	// s(2000) / s(4000) = 1.2e-6 / 1.4e-6, so V_P 85714 at the prompt.
	assert.Equal(t, "N_p 1 x V_P 85714 x (1 - 0.05 margin) / (f 1 x 4000 tokens); UNVALIDATED", pf.kneeBasis(k.Value))
	assert.InDelta(t, 100000*(1.2/1.4)*0.95/4000, k.Value, 1e-9)
}

// S1's flag on the spread: at the assumed cv 2, flagged at p88 and below,
// where the peak is under it, and not at p89 and above.
func TestTTFT_PromptSpreadAbovePeak(t *testing.T) {
	for _, c := range []struct {
		q    float64
		peak float64
	}{{0.70, 0.79}, {0.80, 1.30}, {0.85, 1.66}, {0.88, 1.96}, {0.90, 2.22}, {0.92, 2.56}, {0.93, 2.78}, {0.94, 3}, {0.999, 3}, {0.4, 0}} {
		assert.Equal(t, c.peak, whatif.PromptCVPeak(c.q), "q %g", c.q)
	}
	flagged := func(q float64) bool {
		f := answerFlags{set: map[string]bool{}}
		st := learnedState()
		ps, err := f.prefillState(st)
		require.NoError(t, err)
		pf := f.fleetFor(st, ps, ttftQuestion{q: q}, nil)
		require.Nil(t, pf.refused)
		return slices.Contains(pf.flags, promptSpreadAbovePeak)
	}
	assert.True(t, flagged(0.5))
	assert.True(t, flagged(0.88))
	assert.False(t, flagged(0.89))
	assert.False(t, flagged(0.95))
	assert.False(t, flagged(0), "a mean has no peak")

	out, code := runCmd(t, "replicas-to-slo", "--recording", shortRun, "--assume-vp", "130000", "--ttft-percentile", "80")
	require.Equal(t, 3, code, out)
	assert.Contains(t, out, string(promptSpreadAbovePeak))
}

// S1's routing flags: at 80% of the pods, RoutingBound when it held (a learned
// f, or the whole window of raw shares), else ImbalanceNotSustained; priced at
// that f in either case.
func TestTTFT_RoutingFlags(t *testing.T) {
	read := func(ps learnreplay.PrefillState, extra answerFlags, set ...string) *pricedFleet {
		st := learnedState()
		est := st.Prefill[0].Estimate
		est.FByPods = ps.Estimate.FByPods
		ps.Variant, ps.EstimateRead, ps.Estimate, ps.PromptTokens = "pre", true, est, 2000
		st.Prefill[0] = ps
		return fleetOf(t, st, extra, set...)
	}
	for _, c := range []struct {
		name string
		ps   learnreplay.PrefillState
		want []whatif.Flag
		f    float64
	}{
		{"one pod has no router to blame", learnreplay.PrefillState{Pods: 1}, nil, 0},
		{"under the share", learnreplay.PrefillState{Pods: 4, RawShare: 2}, nil, 2},
		{"a raw share not held over the window",
			learnreplay.PrefillState{Pods: 2, RawShare: 1.9, RawShareLeast: 1.2, RawShareFull: true}, []whatif.Flag{imbalanceNotSustained}, 1.9},
		{"a window not full", learnreplay.PrefillState{Pods: 2, RawShare: 1.9, RawShareLeast: 1.9}, []whatif.Flag{imbalanceNotSustained}, 1.9},
		{"a raw share held over the whole window",
			learnreplay.PrefillState{Pods: 2, RawShare: 1.9, RawShareLeast: 1.7, RawShareFull: true}, []whatif.Flag{routingBound}, 1.9},
		{"a learned f has held over its minutes",
			learnreplay.PrefillState{Pods: 2, Estimate: prefill.Estimate{FByPods: map[int]float64{2: 1.8}}}, []whatif.Flag{routingBound}, 1.8},
	} {
		t.Run(c.name, func(t *testing.T) {
			pf := read(c.ps, answerFlags{})
			require.Nil(t, pf.refused)
			assert.Equal(t, c.f, pf.fleet.Prefill.Imbalance)
			for _, fl := range []whatif.Flag{routingBound, imbalanceNotSustained} {
				assert.Equal(t, slices.Contains(c.want, fl), slices.Contains(pf.flags, fl), string(fl))
			}
			got := pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 0.1, 0.9) })
			require.IsType(t, whatif.TTFT{}, got)
			for _, fl := range c.want {
				assert.True(t, got.(whatif.TTFT).HasFlag(fl))
			}
		})
	}
	// Another count than the window's reads no flag, though f / N' reaches it.
	pf := read(learnreplay.PrefillState{Pods: 3, RawShare: 2.9, RawShareLeast: 2.8, RawShareFull: true}, answerFlags{prefill: 2}, "prefill")
	assert.Equal(t, 2.0, pf.fleet.Prefill.Imbalance)
	assert.NotContains(t, pf.flags, routingBound)
	assert.NotContains(t, pf.flags, imbalanceNotSustained)
}

// A variant's own flags reach question 2's TTFT, as they reach every Answer.
func TestTTFT_VariantFlagsReachTheTTFT(t *testing.T) {
	st := workedState(t)
	f := answerFlags{set: map[string]bool{"assume-vp": true}, assumeVP: 30000}
	variants, err := f.decodeVariants(st)
	require.NoError(t, err)
	variants[0].flags = append(variants[0].flags, whatif.BorrowedLine, whatif.HeldLine)
	fleets, err := f.ttftFleets(st, ttftQuestion{q: 0.9}, variants)
	require.NoError(t, err)
	got := fleets[0].priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 1, 0.9) })
	require.IsType(t, whatif.TTFT{}, got)
	assert.Contains(t, got.(whatif.TTFT).Flags, whatif.BorrowedLine)
	assert.Contains(t, got.(whatif.TTFT).Flags, whatif.HeldLine)

	// With no line priced, the line's flags are not the answer's.
	variants[0].decode.Line.A, variants[0].decode.Line.B = 0, 0
	fleets, err = f.ttftFleets(st, ttftQuestion{q: 0.9}, variants)
	require.NoError(t, err)
	got = fleets[0].priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 1, 0.9) })
	require.IsType(t, whatif.TTFT{}, got)
	assert.NotContains(t, got.(whatif.TTFT).Flags, whatif.BorrowedLine)
}

// The refusal for no V_P through the command, on the text path too: a TTFT
// row that says so, the input named, no figure.
func TestTTFT_NoVPOnTheTextPath(t *testing.T) {
	text, code := runCmd(t, "replicas-to-slo", "--recording", shortRun, "--ttft-percentile", "90")
	require.Equal(t, 3, code, text)
	var row string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "TTFT p90") {
			row = line
		}
	}
	assert.Contains(t, row, "refused: NoPrefillCapacity")
	assert.Contains(t, row, "pass --assume-vp for a flagged answer")
	assert.NotContains(t, row, " ms")
	assert.NotContains(t, text, "prefill knee")

	text, code = runCmd(t, "slo-to-rate", "--recording", shortRun, "--ttft", "1s")
	require.Equal(t, 3, code, text)
	assert.Contains(t, text, "prefill  refused: NoPrefillCapacity")
	assert.Contains(t, text, "fleet    refused: NoPrefillCapacity")
}

// The fleet row is the smallest of the role rates and the operating point of
// the lower knee, each named; on a tie, every one, the cap first.
func TestFleetRate(t *testing.T) {
	q := ttftQuestion{q: 0.95}
	ttft := func(v, knee float64) whatif.Answer {
		return whatif.Answer{Value: v, Flags: []whatif.Flag{whatif.UnvalidatedTTFT},
			Basis: whatif.Basis{TTFT: &whatif.TTFTBasis{KneeRate: knee}}}
	}
	decode := whatif.Answer{Value: 9, Flags: []whatif.Flag{whatif.HeldLine}, Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundTPOT}}}
	for _, c := range []struct {
		name   string
		decode whatif.Result
		ttft   whatif.Answer
		want   float64
		by     []string
	}{
		{"the target", nil, ttft(4, 10), 4, []string{"TTFT p95, end to end"}},
		{"the cap", nil, ttft(9.5, 10), 8.5, []string{"operating-point cap"}},
		{"decode", decode, ttft(9.5, 20), 9, []string{"decode TPOT"}},
		{"a tie", nil, ttft(8.5, 10), 8.5, []string{"operating-point cap", "TTFT p95, end to end"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := fleetRate(q, 0.85, c.decode, c.ttft).(whatif.Answer)
			require.True(t, ok)
			assert.Equal(t, c.want, got.Value)
			by := make([]string, 0, len(got.Basis.BoundBy))
			for _, b := range got.Basis.BoundBy {
				by = append(by, string(b))
			}
			assert.Equal(t, c.by, by)
			assert.True(t, got.HasFlag(whatif.UnvalidatedTTFT))
		})
	}
	u := whatif.Unanswerable{Reason: whatif.NoLine}
	assert.Equal(t, u, fleetRate(q, 0.85, u, ttft(4, 10)))
	assert.Equal(t, u, fleetRate(q, 0.85, decode, u))
}

// The percentile of question 2: the flag, else the policy's, else 90.
func TestTTFT_PercentileOrder(t *testing.T) {
	st := workedState(t)
	f := answerFlags{set: map[string]bool{"assume-vp": true}}
	q, err := f.replicasTTFT()
	require.NoError(t, err)
	require.NotNil(t, q)
	q.policyPercentile(st)
	assert.Equal(t, 0.9, q.q)

	st.Policy = &config.ScalingPolicy{SLO: &config.SLOPolicy{TTFT: &config.SLOObjective{Percentile: 95}}}
	q = &ttftQuestion{}
	q.policyPercentile(st)
	assert.Equal(t, 0.95, q.q)

	f = answerFlags{set: map[string]bool{"ttft-percentile": true}, ttftPercentile: "p99.5"}
	q, err = f.replicasTTFT()
	require.NoError(t, err)
	q.policyPercentile(st)
	assert.InDelta(t, 0.995, q.q, 1e-12)
	assert.Equal(t, "TTFT p99.5", q.label())

	f = answerFlags{set: map[string]bool{}}
	q, err = f.replicasTTFT()
	require.NoError(t, err)
	assert.Nil(t, q, "no TTFT flag asks no TTFT")
	q.policyPercentile(st) // a nil question has none to fill
}

// A bad percentile is refused before any replay: the recording is not read.
func TestTTFT_APercentileIsCheckedFirst(t *testing.T) {
	out, code := runCmd(t, "replicas-to-slo", "--recording", "/no/such/recording", "--ttft-percentile", "150", "-o", "json")
	require.Equal(t, 2, code, out)
	assert.Contains(t, decodeOutput(t, out).Error.Message, "below 100")
}

// The label holds no float noise.
func TestTTFT_TheLabelIsWhatWasAsked(t *testing.T) {
	for _, pct := range []string{"57", "90", "99.9", "99.99", "7", "55.5"} {
		q, err := parseTTFTPercent("--ttft-percentile", pct)
		require.NoError(t, err)
		assert.Equal(t, "TTFT p"+pct, ttftQuestion{q: q}.label())
	}
	assert.Equal(t, "mean TTFT", ttftQuestion{}.label())
}

// Every TTFT flag asks about TTFT on question 2.
func TestTTFT_EachFlagAsks(t *testing.T) {
	for _, flag := range [][]string{{"--ttft-percentile", "90"}, {"--assume-vp", "30000"}, {"--prefill", "1"},
		{"--prefill-variant", "variant-02"}} {
		text, _ := runCmd(t, slices.Concat([]string{"replicas-to-slo", "--recording", shortRun}, flag)...)
		assert.Contains(t, text, "TTFT p90", "%v", flag)
		assert.Contains(t, text, "UNVALIDATED", "%v", flag)
	}
}

// A P/D window for the mode equivalence: the sweep's decode variant and one
// prefill pod. Its prompt alternates, so P1 fits a from its low-load minutes.
const (
	pdPromptHold = 20
	pdFixed      = 0.010
	pdTokenCost  = 0.00001
	pdRate       = 0.5
)

func pdPrompt(i int) float64 {
	if (i/pdPromptHold)%2 == 0 {
		return 1000
	}
	return 4000
}

func pdService(i int) float64 { return pdFixed + pdPrompt(i)*pdTokenCost }

// pdServer is sweepServer with one prefill pod.
func pdServer(t *testing.T) *promtest.Server {
	t.Helper()
	srv := sweepServer(t)
	addPrefillPod(t, srv, "m-pre-7c9d-0", "10.0.1.1:8000", pdRate)
	return srv
}

// addPrefillPod adds one pod serving rate requests/s: kv, rates and service
// time, the series a prefill row reads.
func addPrefillPod(t *testing.T, srv *promtest.Server, pod, instance string, rate float64) {
	t.Helper()
	q := sweepQueries(t, inferenceengine.EngineVLLM)
	labels := map[string]string{"model_name": "m", "pod": pod, "instance": instance, "namespace": sweepNS}
	add := func(name string, f func(i int) float64) {
		srv.Add(q(name), promtest.Series{Labels: labels, Value: stepped(f)})
	}
	c := func(x float64) func(int) float64 { return func(int) float64 { return x } }
	add(registration.QueryKvCacheUsage, c(0.01))
	add(registration.QueryMaxRankKvCacheUsage, c(0.01))
	add(registration.QueryQueueLength, c(0))
	add(registration.QueryMaxRankQueueLength, c(0))
	add(registration.QueryCapacityQueueLength, c(0))
	add(registration.QueryMaxRankCapacityQueueLength, c(0))
	add(registration.QueryAvgOutputTokens, c(1))
	add(registration.QueryAvgOutputTokensRecent, c(1))
	add(registration.QueryAvgInputTokens, pdPrompt)
	add(registration.QueryAvgInputTokensRecent, pdPrompt)
	add(registration.QueryPrefixCacheHitRate, c(0))
	add(registration.QueryRunningRequests, c(0))
	add(registration.QueryRequestRate, c(rate))
	add(registration.QueryPrefillComputedTokenRate, func(i int) float64 { return rate * pdPrompt(i) })
	add(registration.QueryAvgServiceTime, pdService)
	add(registration.QueryAvgTTFT, func(i int) float64 { return pdService(i) + 0.002 })
	add(registration.QueryKvUsageInstant, c(0.01))
	add(registration.QueryKvUsageMean, c(0.01))
	add(registration.QueryPreemptionRate, c(0))
}

// stepped is f on the sweep's grid.
func stepped(f func(i int) float64) func(time.Time) (float64, bool) {
	return func(ts time.Time) (float64, bool) {
		i, ok := sweepStepOf(ts)
		if !ok {
			return 0, false
		}
		return f(i), true
	}
}

// The second window's inputs: two prefill pods at 3:1, so a raw share of 1.5,
// each decode pod's KV transfer, and flow-control series for the model.
const (
	pdFastRate = 0.75
	pdSlowRate = 0.25
	pdTransfer = 0.02
)

// pdRichServer is the P/D window with what the first leaves out.
func pdRichServer(t *testing.T) *promtest.Server {
	t.Helper()
	srv := sweepServer(t)
	addPrefillPod(t, srv, "m-pre-7c9d-0", "10.0.1.1:8000", pdFastRate)
	addPrefillPod(t, srv, "m-pre-7c9d-1", "10.0.1.2:8000", pdSlowRate)
	q := sweepQueries(t, inferenceengine.EngineVLLM)
	for j := range sweepPods {
		srv.Add(q(registration.QueryAvgKVTransferTime), promtest.Series{Value: stepped(func(int) float64 { return pdTransfer }),
			Labels: map[string]string{"model_name": "m", "pod": fmt.Sprintf("m-dec-7c9d-%d", j),
				"instance": fmt.Sprintf("10.0.0.%d:8000", j+1), "namespace": sweepNS}})
	}
	srv.Add(q(registration.QueryFlowControlPoolModels), promtest.Series{Value: stepped(func(int) float64 { return 0 }),
		Labels: map[string]string{"inference_pool": "pool-m", "model_name": "m", "target_model_name": "m"}})
	// The same gauge, by model: what says the gate is on.
	srv.Add(q(registration.QuerySchedulerQueueSize), promtest.Series{Value: stepped(func(int) float64 { return 0 }),
		Labels: map[string]string{"model_name": "m", "target_model_name": "m", "namespace": sweepNS}})
	return srv
}

// pdWindow is the --prometheus flags of the P/D window.
func pdWindow(prom string) []string {
	window := sweepWindow(prom)
	return slices.Insert(window, slices.Index(window, "--from"), "--role-of", "prefill=app=m-pre")
}

// The TTFT answer from --prometheus is the one capture, generate and
// --recording give, byte for byte, in text and in JSON, as every other flag's
// is. The window must answer: a refusal on both paths would hold the identity
// and say nothing.
func TestPrometheus_TheTTFTAnswerIsTheFilesAnswer(t *testing.T) {
	srv := pdServer(t)
	window := pdWindow(srv.URL)
	for _, c := range []struct {
		name string
		args []string
		code int
	}{
		{"replicas-to-slo", []string{"replicas-to-slo", "--assume-vp", "100000"}, 0},
		{"replicas-to-slo at a rate", []string{"replicas-to-slo", "--assume-vp", "100000", "--rate", "2", "--ttft-percentile", "95"}, 0},
		{"replicas-to-slo, more pods", []string{"replicas-to-slo", "--assume-vp", "100000", "--prefill", "3"}, 0},
		{"slo-to-rate", []string{"slo-to-rate", "--assume-vp", "100000", "--tpot", "15ms", "--ttft", "2s@p90"}, 0},
		{"slo-to-rate, TTFT alone", []string{"slo-to-rate", "--assume-vp", "100000", "--ttft", "2s"}, 0},
		{"refused: no V_P", []string{"replicas-to-slo", "--ttft-percentile", "90"}, 3},
		{"refused: saturated", []string{"replicas-to-slo", "--assume-vp", "100000", "--rate", "5000"}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, output := range []string{outputJSON, outputText} {
				q := slices.Concat(c.args, []string{"-o", output})
				files, fileCode := filesAnswer(t, window, q)
				prom, promCode := runCmd(t, slices.Concat(q[:1], window, q[1:])...)
				require.Equal(t, c.code, fileCode, "the files' exit code\n%s", files)
				require.Equal(t, fileCode, promCode, "exit codes differ\nfiles:\n%s\nprometheus:\n%s", files, prom)
				if output == outputText {
					assert.Equal(t, files, withoutSourceLine(t, prom), "the text differs")
					continue
				}
				if d := sameAnswer(t, files, prom); d != "" {
					t.Errorf("the --prometheus answer is not the files' answer:\n%s", d)
				}
			}
		})
	}

	// The window answers TTFT, from a learned a.
	out, code := runCmd(t, slices.Concat([]string{"slo-to-rate"}, window, []string{"--assume-vp", "100000", "--ttft", "2s@p90", "-o", "json"})...)
	require.Equal(t, 0, code, out)
	w := decodeOutput(t, out)
	i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill })
	require.GreaterOrEqual(t, i, 0, out)
	require.NotNil(t, w.Roles[i].Results.Rate.Answer, "no answer to compare:\n%s", out)
	assert.Equal(t, "learned", w.Roles[i].TTFT.FixedLatencySource)
	near(t, "a", w.Roles[i].TTFT.FixedLatencySeconds, pdFixed+0.002, 0.001)
	assert.Positive(t, w.Roles[i].TTFT.PromptTokens)
}

// The first window could pass by construction: one pod, and inputs the path
// never touches. This one puts on the path everything the rows and the input
// carry beyond it: two pods at 3:1, a KV transfer series and flow-control
// series. So the identity here says that capture carries those series into
// the capture file, generate into the recording, and the replay reads them
// back the same in both modes, and the assertions after it say what they read
// is the fake's: f 1.5, a transfer of 20 ms, the gate on. It says nothing of
// whether a real engine's series mean what the model takes them to.
func TestPrometheus_TheTTFTInputsCrossTheCapture(t *testing.T) {
	srv := pdRichServer(t)
	window := pdWindow(srv.URL)
	asks := [][]string{
		{"replicas-to-slo", "--assume-vp", "100000"},
		{"slo-to-rate", "--assume-vp", "100000", "--tpot", "15ms", "--ttft", "2s@p90"},
	}
	for _, q := range asks {
		for _, output := range []string{outputJSON, outputText} {
			args := slices.Concat(q, []string{"-o", output})
			files, fileCode := filesAnswer(t, window, args)
			prom, promCode := runCmd(t, slices.Concat(args[:1], window, args[1:])...)
			require.Equal(t, 0, fileCode, files)
			require.Equal(t, fileCode, promCode, "exit codes differ\nfiles:\n%s\nprometheus:\n%s", files, prom)
			if output == outputText {
				assert.Equal(t, files, withoutSourceLine(t, prom), "the text differs")
			} else if d := sameAnswer(t, files, prom); d != "" {
				t.Errorf("the --prometheus answer is not the files' answer:\n%s", d)
			}
		}
	}

	out, code := runCmd(t, slices.Concat(asks[0][:1], window, asks[0][1:], []string{"-o", "json"})...)
	require.Equal(t, 0, code, out)
	w := decodeOutput(t, out)
	i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill })
	require.GreaterOrEqual(t, i, 0, out)
	in := w.Roles[i].TTFT
	assert.Equal(t, 2, in.Replicas)
	assert.Equal(t, "assumed", in.ImbalanceSource, "minutes of 100 requests a pod are needed to learn f")
	near(t, "f, the raw share", in.Imbalance, 1.5, 1e-9)
	assert.Equal(t, "on", in.FlowControl)
	assert.Contains(t, in.Assumed, "flow-control thresholds 5 and 0.8 (upstream defaults)")
	assert.Equal(t, "line", in.DecodeSide)
	assert.Equal(t, "measured", in.TransferSource)
	near(t, "transfer", in.TransferSecondsPerToken*in.PromptTokens, pdTransfer, 1e-9)
	require.NotNil(t, w.Roles[i].Results.TTFT.Answer, out)
	assert.NotNil(t, w.Roles[i].Results.TTFT.Answer.Parts.Onsets, "the gate on places onsets")

	// The negative control: the same question without those series differs.
	bare := pdServer(t)
	other, code := runCmd(t, slices.Concat(asks[0][:1], pdWindow(bare.URL), asks[0][1:], []string{"-o", "json"})...)
	require.Equal(t, 0, code, other)
	j := slices.IndexFunc(decodeOutput(t, other).Roles, func(r wireRole) bool { return r.Role == rolePrefill })
	require.GreaterOrEqual(t, j, 0)
	assert.NotEqual(t, in.TransferSource, decodeOutput(t, other).Roles[j].TTFT.TransferSource)
	assert.NotEqual(t, in.FlowControl, decodeOutput(t, other).Roles[j].TTFT.FlowControl)
}

// A window of two decode variants and two prefill variants: the labels, the
// names of the whole-rate assumptions, and -o json on the fleets.
func TestTTFT_SeveralVariants(t *testing.T) {
	srv := pdServer(t)
	addPrefillPod(t, srv, "m-prb-7c9d-0", "10.0.2.1:8000", pdRate)
	addPrefillPod(t, srv, "m-dc2-7c9d-0", "10.0.3.1:8000", pdRate)
	window := slices.Concat(pdWindow(srv.URL), []string{"--role-of", "prefill=app=m-prb", "--role-of", "decode=app=m-dc2"})

	// Several prefill variants, none named: a usage error, as one object.
	out, code := runCmd(t, slices.Concat([]string{"slo-to-rate"}, window, []string{"--assume-vp", "100000", "--ttft", "2s", "-o", "json"})...)
	require.Equal(t, 2, code, out)
	assert.Contains(t, decodeOutput(t, out).Error.Message, "--prefill-variant")

	ask := slices.Concat([]string{"slo-to-rate"}, window, []string{"--assume-vp", "100000", "--ttft", "2s", "--prefill-variant", "m-pre"})
	text, _ := runCmd(t, ask...)
	for _, row := range []string{"prefill/m-dec", "fleet/m-dec", "prefill/m-dc2", "fleet/m-dc2"} {
		assert.Contains(t, text, row)
	}
	assert.Contains(t, text, "the whole rate on prefill variant m-pre (--prefill-variant; the model has 2)")
	assert.Contains(t, text, "the whole rate on decode variant m-dec (the model has 2)")

	out, _ = runCmd(t, slices.Concat(ask, []string{"-o", "json"})...)
	w := decodeOutput(t, out)
	roles := make([]string, 0, len(w.Roles))
	for _, r := range w.Roles {
		roles = append(roles, r.Role+":"+r.Variant)
	}
	assert.Equal(t, []string{"prefill:m-pre", "fleet:m-dc2", "prefill:m-pre", "fleet:m-dec"}, roles)
	assert.Contains(t, w.Roles[0].TTFT.Assumed, "the whole rate on prefill variant m-pre (--prefill-variant; the model has 2)")
	assert.Equal(t, "m-dc2", w.Roles[0].TTFT.DecodeVariant)

	// Question 2 labels by decode variant and repeats the prefill role.
	out, _ = runCmd(t, slices.Concat([]string{"replicas-to-slo"}, window, []string{"--assume-vp", "100000", "--prefill-variant", "m-prb", "-o", "json"})...)
	w = decodeOutput(t, out)
	prefills := 0
	for _, r := range w.Roles {
		if r.Role == rolePrefill {
			prefills++
			assert.Equal(t, "m-prb", r.Variant)
		}
	}
	assert.Equal(t, 2, prefills, out)
}

// -o json carries the aggregated refusal as a result, not an error.
func TestTTFT_AggregatedInJSON(t *testing.T) {
	srv := sweepServer(t)
	window := sweepWindow(srv.URL)
	window[slices.Index(window, "decode=app=m-dec")] = "both=app=m-dec"
	out, code := runCmd(t, slices.Concat([]string{"replicas-to-slo"}, window, []string{"--assume-vp", "30000", "-o", "json"})...)
	require.Equal(t, 3, code, out)
	w := decodeOutput(t, out)
	assert.Nil(t, w.Error)
	i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill })
	require.GreaterOrEqual(t, i, 0, out)
	require.NotNil(t, w.Roles[i].Results.TTFT.Refused, out)
	assert.Equal(t, string(whatif.NoPrefillReplicas), w.Roles[i].Results.TTFT.Refused.Reason)
	assert.Contains(t, w.Roles[i].Results.TTFT.Refused.Detail, "the planner's M1")
}

// A fleet refused before pricing prints no input it did not read.
func TestTTFT_ARefusedFleetPrintsNoFigure(t *testing.T) {
	st := learnedState()
	st.Prefill = nil
	pf := fleetOf(t, st, answerFlags{})
	require.NotNil(t, pf.refused)
	var b strings.Builder
	pf.writeInputs(&b, "", "")
	assert.Empty(t, b.String())
	assert.Zero(t, pf.in.VP)
}
