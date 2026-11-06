package learnreplay

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/prefill"
)

// The caveat the metric's help and the docs carry is the frozen table: the
// counts and the worst of each, read from it, so a change to the model that
// moves them fails here and not in an operator's reading of the help.
func TestS1_TheCaveatIsTheFrozenTable(t *testing.T) {
	var points, over, under, overP90, overP95, underP90, underP95 int
	var worstOverP90, worstOverP95, worstUnder float64
	for _, c := range ttftFrozen {
		if c.fixture == ttftSaturatedFixture {
			continue // refused, and at saturation; the caveat is light load
		}
		ratio := c.predicted / c.observed
		isOver := strings.HasPrefix(c.verdict, verdictOver)
		isUnder := strings.HasPrefix(c.verdict, verdictUnder)
		switch {
		case c.cv == s1PromptCV:
			points++
			if isOver {
				over++
				if c.q == 0.9 {
					overP90++
					worstOverP90 = max(worstOverP90, ratio)
				} else {
					overP95++
					worstOverP95 = max(worstOverP95, ratio)
				}
			}
		case isUnder:
			under++
			worstUnder = max(worstUnder, 1-ratio)
			if c.q == 0.9 {
				underP90++
			} else {
				underP95++
			}
		}
	}
	require.Equal(t, 14, points)
	t.Logf("over %d (p90 %d, p95 %d), worst %.2fx p90, %.2fx p95; under %d (p90 %d, p95 %d), worst %.1f%%",
		over, overP90, overP95, worstOverP90, worstOverP95, under, underP90, underP95, 100*worstUnder)
	c := constants.SLOTTFTPredictionCaveat
	assert.Contains(t, c, fmt.Sprintf("over the router at %d of %d points", over, points))
	assert.Contains(t, c, fmt.Sprintf("worst %.2fx at p90 and %.2fx at p95", worstOverP90, worstOverP95))
	assert.Contains(t, c, fmt.Sprintf("under at %d of %d, worst %.0f%% under", under, points, 100*worstUnder))
	assert.Equal(t, []int{6, 7, 5, 4}, []int{overP90, overP95, underP90, underP95}, "the docs' split by percentile")
}

// S1 on the recordings. The two goldens slo-published.csv moves in have no
// prefill capacity to price; the prefill-stress goldens were recorded with no
// slo analyzer listed, so their slo-published.csv is empty whatever S1 does.
// These tests drive the analyzer over those recordings' own rows, as the
// engine would have with a TTFT target listed.

// ttftPublished reads a slo-published.csv's TTFT readings, in order.
func ttftPublished(t *testing.T, b []byte) []metrics.SLOTTFTReading {
	t.Helper()
	r := csv.NewReader(bytes.NewReader(b))
	rows, err := r.ReadAll()
	require.NoError(t, err)
	out := make([]metrics.SLOTTFTReading, 0, len(rows))
	for _, row := range rows[1:] {
		var raw struct {
			TTFT []struct {
				Variant, Statistic, VPSource, Reason, Refused string
				Unmet                                         bool
				Flags                                         []string
			}
		}
		require.NoError(t, json.Unmarshal([]byte(row[len(row)-1]), &raw))
		for _, r := range raw.TTFT {
			out = append(out, metrics.SLOTTFTReading{Variant: r.Variant, Statistic: r.Statistic, VPSource: r.VPSource,
				Reason: r.Reason, Refused: r.Refused, Unmet: r.Unmet, Flags: r.Flags})
		}
	}
	return out
}

// Test 22, the pairwise switch run: P1's observation off with the TTFT
// pricing on moves exactly the outputs the table names from all on, and every
// prefill variant reads no-estimate in place of its reason: no-prefill-capacity
// on the two ols-sweep-slo goldens, and whichever a staircase cycle gives.
func TestGoldens_TTFTPriceReadsTheObservation(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	m, err := ReadManifest(goldenDir)
	require.NoError(t, err)
	off := replaySets(t, m, []map[string]bool{{saturation.SwitchPrefillCapacityObserve: false}})[0]
	var moved []string
	for f, b := range off {
		committed, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(f)))
		require.NoError(t, err)
		d, err := compareOutput(f, committed, b)
		require.NoError(t, err)
		if d != nil {
			moved = append(moved, f)
		}
	}
	require.ElementsMatch(t, replaySwitches[slo.SwitchTTFTPrice], moved)
	for _, f := range moved {
		committed, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(f)))
		require.NoError(t, err)
		on, without := ttftPublished(t, committed), ttftPublished(t, off[f])
		require.NotEmpty(t, on)
		require.Len(t, without, len(on), f)
		for i := range on {
			if !strings.HasPrefix(f, "ttft-staircase-") {
				assert.Equal(t, constants.SLOTTFTReasonNoPrefillCapacity, on[i].Reason, f)
				assert.Equal(t, constants.SLOTTFTVPNone, on[i].VPSource, f)
			}
			assert.Equal(t, constants.SLOTTFTReasonNoEstimate, without[i].Reason, f)
		}
	}
}

// stitched serves one estimate for every variant, stamped with the replay's
// clock: V_P from one run and a from another, as P2's validation prices them.
type stitched struct {
	est prefill.Estimate
	now func() time.Time
}

func (s stitched) PrefillEstimate(string, string, string) (prefill.Estimate, time.Time, bool) {
	return s.est, s.now(), true
}

// shadowCycle is one cycle of a shadow replay.
type shadowCycle struct {
	at      time.Time
	input   domain.AnalyzerInput
	reading metrics.SLOTTFTReading
}

// shadowReplay replays a golden and, after each saturation Analyze, runs the
// slo analyzer over the same input with a TTFT target listed, flow control
// declared off as it was on every run, as the engine does in that order. It
// returns each cycle. est, when set, stands in for P1.
func shadowReplay(t *testing.T, fixture string, target time.Duration, est *prefill.Estimate) []shadowCycle {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, fixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	var cycles []shadowCycle
	opts := Options{Mode: Normal, stop: func(r *replayer, ev learnrec.RawEvent) bool {
		if ev.Kind != saturation.KindAnalyze {
			return false
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		in, err := decodeInput(d.Input)
		require.NoError(t, err)
		cfg := *in.Config.(*config.ScalingPolicy)
		cfg.SLO = &config.SLOPolicy{TTFT: &config.SLOObjective{Target: config.SLODuration{Duration: target}},
			FlowControl: config.SLOFlowControlDisabled}
		in.Config = &cfg
		if est != nil {
			r.slo.WithPrefillEstimates(stitched{est: *est, now: r.clock.Now})
		}
		_, err = r.slo.Analyze(context.Background(), in)
		require.NoError(t, err)
		cycles = append(cycles, shadowCycle{at: ev.Now, input: in})
		return false
	}}
	rep, err := Replay(context.Background(), raw, opts)
	require.NoError(t, err)
	rec, err := learnrec.Read(rep.Recording)
	require.NoError(t, err)
	n := 0
	for _, ev := range rec.Events {
		if ev.Kind != slo.KindAnalyze {
			continue
		}
		var d slo.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		require.Empty(t, d.Error)
		require.NotNil(t, d.Published)
		require.Len(t, d.Published.TTFT, 1, "one prefill variant a cycle")
		require.Less(t, n, len(cycles))
		cycles[n].reading = d.Published.TTFT[0]
		n++
	}
	require.Equal(t, len(cycles), n)
	require.NotEmpty(t, cycles)
	return cycles
}

// As P1 learns them, no run alone gives the model a fleet: each lacks V_P or a
// (the P2 table). So S1's item 7, run 2 priced from busy minutes, is not met:
// run 2 reads busy-minutes and refuses for a. That is a reason, never a number.
func TestS1_OnThePrefillStressGoldensAsP1LearnsThem(t *testing.T) {
	t.Parallel() // reads the goldens, writes only to its own copies
	for _, c := range []struct {
		fixture, reason, vpSource string
	}{
		{ttftSaturatedFixture, constants.SLOTTFTReasonNoFixedLatency, constants.SLOTTFTVPBusyMinutes},
		{ttftThreePodsFixture, constants.SLOTTFTReasonNoFixedLatency, constants.SLOTTFTVPPeakUnderQueue},
		{ttftOpenLongFixture, constants.SLOTTFTReasonNoPrefillCapacity, constants.SLOTTFTVPNone},
		{ttftOpenShortFixture, constants.SLOTTFTReasonNoPrefillCapacity, constants.SLOTTFTVPNone},
	} {
		got := shadowReplay(t, c.fixture, 3*time.Second, nil)
		last := got[len(got)-1].reading
		assert.Equal(t, c.reason, last.Reason, c.fixture)
		assert.Equal(t, c.vpSource, last.VPSource, c.fixture)
		reasons := map[string]int{}
		for _, cy := range got {
			r := cy.reading
			reasons[r.Reason]++
			require.Contains(t, constants.SLOTTFTReasons, r.Reason)
			if r.Reason != constants.SLOTTFTReasonPriced {
				assert.True(t, math.IsNaN(r.Predicted) && math.IsNaN(r.PerReplica), "%s: %s carries a number", c.fixture, r.Reason)
			}
		}
		t.Logf("%s: %d cycles, %v, last vp_source %s", c.fixture, len(got), reasons, last.VPSource)
	}
}

// With V_P from run 2 and a from run 4, as P2 prices them, S1 prices run 4's
// own rows through Analyze: the path from the rows to the published figure.
// It is at S1's assumed spread (no recording carries the computed-token
// histogram), where the model reads over the router's p90, so the logged
// ratios are not the 34% under that the sent spread gives. The test holds the
// path and the flags, not the accuracy: the accuracy table is
// TestTTFTValidation_AgainstTheRouter.
func TestS1_PricedOnRecordedRows(t *testing.T) {
	t.Parallel() // reads the goldens, writes only to its own copies
	vp, _ := replayP1(t, ttftSaturatedFixture)
	a, _ := replayP1(t, ttftOpenShortFixture)
	est := a.estimate()
	est.VP, est.VPSource, est.VPRefused, est.VPPromptTokens = vp.VP, prefill.SourceBusyMinutes, "", vp.VPPrompt
	est.VPLowerBound = false
	cycles := shadowReplay(t, ttftOpenShortFixture, 3*time.Second, &est)

	for _, lv := range ttftLevels[ttftOpenShortFixture] {
		var last *shadowCycle
		for i := range cycles {
			c := &cycles[i]
			if rl := c.input.RouterLatency; !c.at.Before(lv.from) && !c.at.After(lv.to) && rl != nil && len(rl.TTFTBuckets) > 0 {
				last = c
			}
		}
		require.NotNil(t, last, lv.name)
		r := last.reading
		require.Equal(t, constants.SLOTTFTReasonPriced, r.Reason, lv.name)
		require.Equal(t, constants.SLOTTFTVPBusyMinutes, r.VPSource, lv.name)
		for _, f := range []string{"UnvalidatedTTFT", "AssumedPromptSpread", "DecodeEngineMeasured", "AssumedPoissonArrivals"} {
			assert.True(t, slices.Contains(r.Flags, f), "%s: not flagged %s: %v", lv.name, f, r.Flags)
		}
		assert.False(t, math.IsNaN(r.Predicted), lv.name)
		assert.Positive(t, r.PerReplica, lv.name)
		assert.False(t, r.Unmet, "%s: light load against a 3 s target", lv.name)
		obs, _, _ := bucketQuantile(last.input.RouterLatency.TTFTBuckets, 0.9)
		t.Logf("%s: predicted %.3f s, router p90 %.3f s, ratio %.2f, per replica %.2f r/s", lv.name, r.Predicted, obs, r.Predicted/obs, r.PerReplica)
	}
}
