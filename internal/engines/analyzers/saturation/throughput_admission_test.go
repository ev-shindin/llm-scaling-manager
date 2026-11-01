package saturation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The E1 replay (2026-10-23, two namespaces, Qwen3-32B-FP8, one H200 per
// replica, EPP flow control on). Every figure below is the run's own, read
// from its controller log at --v=4 (replica-capacity-decision, derived-mu,
// itl-gps-mismatch) and its vLLM counters. A row with no completions reports
// no average lengths, as the collector does (0/0 is no series).

const (
	e1Variant = "qwen32b-decode-wva"
	e1Cache   = int64(376_480) // 23 530 blocks of 16; k1 at 0.8 is 301 184
)

// e1Row is one replica on one cycle: its queue, resident tokens, completions
// per second and generated tokens per second over the last minute.
type e1Row struct {
	pod string
	q   int
	tok int64
	rr  float64
	gps float64
}

// e1Cycle is one 15 s cycle, at the wall-clock time the run logged it.
type e1Cycle struct {
	at   string
	rows []e1Row
}

// e1Decision is what one row was priced at: the k2 source and the mu the
// replica-capacity-decision line logged, and the bucket that mu came from.
type e1Decision struct {
	at, pod, k2Src, bucket string
	mu                     float64
	k2                     int64
}

// e1Line is the ITL line sg2 priced from in that minute (itlA 0.16-0.18,
// itlB 0.006): derived mu about 3.6 req/s at 1000/500.
var e1Line = itl.Model{A: 0.16, B: 0.006}

// newE1Analyzer is an analyzer whose variant runs --max-num-seqs 256 and,
// when line is true, has an ITL line to derive mu from.
func newE1Analyzer(t *testing.T, line bool) *SaturationAnalyzer {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", e1Variant, capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: deploymentParams(),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	if line {
		publishE1Line(a)
	}
	return a
}

// publishE1Line gives the variant e1Line. Learned-state reuse is off by
// default, so the variant's physics key is its own window key, and nothing of
// its own has been fitted under it.
func publishE1Line(a *SaturationAnalyzer) {
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", e1Variant, "H200", 1, ""),
		"the run's fitted line", e1Line, time.Now())
}

// e1Metrics is one row as the collector reports it.
func e1Metrics(r e1Row) domain.ReplicaMetrics {
	in, out := 0.0, 0.0
	if r.rr > 0 {
		in, out = 1000, 498 // the run's 495-501: the "medium" bucket
	}
	rm := makeReplicaMetrics(r.pod, e1Variant, r.tok, e1Cache, r.q, in, out)
	rm.RequestRate = r.rr
	rm.GenerationTokenRate = r.gps
	rm.KvUsageInstant, rm.KvUsageInstantKnown = float64(r.tok)/float64(e1Cache), true
	return rm
}

// replayE1 runs the cycles and returns every row's decision, and after each
// cycle the lowest sample any throughput window holds (0 when none has one).
func replayE1(t *testing.T, a *SaturationAnalyzer, cycles []e1Cycle) ([]e1Decision, []float64) {
	t.Helper()
	return replayE1Ready(t, a, cycles, nil)
}

// replayE1Ready is replayE1 with each row's ReadySince set by readySince, as
// the collector sets it from the Pod's Ready condition; nil leaves it zero.
func replayE1Ready(t *testing.T, a *SaturationAnalyzer, cycles []e1Cycle,
	readySince func(pod string) time.Time) ([]e1Decision, []float64) {
	t.Helper()
	out := make([]e1Decision, 0, 3*len(cycles))
	lows := make([]float64, 0, len(cycles))
	for _, c := range cycles {
		at, err := time.Parse("15:04:05", c.at)
		require.NoError(t, err)
		at = time.Date(2026, 10, 23, at.Hour(), at.Minute(), at.Second(), 0, time.UTC)
		a.now = func() time.Time { return at }
		rows := make([]domain.ReplicaMetrics, 0, len(c.rows))
		for _, r := range c.rows {
			rm := e1Metrics(r)
			if readySince != nil {
				rm.ReadySince = readySince(r.pod)
			}
			rows = append(rows, rm)
		}
		ctx, logs := observedCtx(t)
		_, err = a.Analyze(ctx, makeAnalyzerInput(rows, []domain.VariantReplicaState{{
			VariantName: e1Variant, AcceleratorName: "H200", CurrentReplicas: len(rows),
			GPUsPerReplica: 1, Role: domain.RoleDecode,
		}}))
		require.NoError(t, err)
		for _, e := range logs.FilterMessage("replica-capacity-decision").All() {
			f := e.ContextMap()
			d := e1Decision{at: c.at, pod: stringField(t, f, "pod"), k2Src: stringField(t, f, "k2Source"),
				bucket: stringField(t, f, "saturatedThroughputBucket")}
			d.mu, _ = f["saturatedThroughput"].(float64)
			d.k2, _ = f["k2ComputeBound"].(int64)
			out = append(out, d)
			t.Logf("%s %-6s k2Src=%-8s k2=%d mu=%.4f bucket=%s", d.at, d.pod, d.k2Src, d.k2, d.mu, d.bucket)
		}
		lows = append(lows, lowestThroughputSample(a))
	}
	return out, lows
}

// lowestThroughputSample is the lowest value any throughput window holds.
func lowestThroughputSample(a *SaturationAnalyzer) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	low := 0.0
	for _, ra := range a.saturatedThroughput {
		for _, v := range ra.Snapshot().Values {
			if low == 0 || v < low {
				low = v
			}
		}
	}
	return low
}

// steadyE1 is the fleet after the step: every replica serving about 6.3 req/s
// with no queue, as it did for the rest of phase B.
func steadyE1(from string, n int, pods ...string) []e1Cycle {
	start, _ := time.Parse("15:04:05", from)
	out := make([]e1Cycle, 0, n)
	for i := range n {
		c := e1Cycle{at: start.Add(time.Duration(i) * 15 * time.Second).Format("15:04:05")}
		for j, p := range pods {
			c.rows = append(c.rows, e1Row{p, j % 2, 340_000, 6.3, 3150 + float64(i%3)})
		}
		out = append(out, c)
	}
	return out
}

// sg2, phase B: kjxzs joins with the router's queue (84) and no completion
// yet. Its 49 generated tokens a second over 496.5 out -- 0.0995 req/s -- was
// recorded as the bucket's saturated throughput, priced 2 req/s as 12-20
// replicas, and, the queue never building again under flow control, stood
// for the rest of the run against a derived 3.1-5.7.
var e1Sg2 = []e1Cycle{
	// The fleet before the step: one replica, as it had been since 14:22.
	{"14:33:16", []e1Row{{"5scc7", 0, 344_607, 5.60, 2800}}},
	{"14:33:31", []e1Row{{"5scc7", 0, 344_607, 6.54, 3270}, {"rx62w", 218, 174_359, 0, 32.68}}},
	{"14:33:47", []e1Row{{"5scc7", 0, 340_606, 6.18, 3090}, {"rx62w", 218, 288_492, 0, 673.69}}},
	{"14:34:02", []e1Row{{"5scc7", 0, 340_606, 5.70, 2850}, {"rx62w", 218, 351_007, 1.02, 2100.86}, {"kjxzs", 19, 9_248, 0, 0}}},
	{"14:34:17", []e1Row{{"5scc7", 0, 335_998, 6.68, 3340}, {"rx62w", 218, 351_007, 3.40, 2514.23}, {"kjxzs", 84, 174_327, 0, 49.40}}},
	// kjxzs's token rate on this cycle is not in the log (no mismatch line);
	// its counters say 1 264 tokens at 14:34:01 and 247 382 at 14:35:12.
	{"14:34:32", []e1Row{{"5scc7", 0, 335_998, 6.34, 3170}, {"rx62w", 1, 351_007, 4.41, 3725.19}, {"kjxzs", 84, 342_335, 0, 1500}}},
	{"14:34:47", []e1Row{{"5scc7", 0, 335_998, 6.34, 3170}, {"rx62w", 1, 351_007, 6.07, 3202.52}, {"kjxzs", 84, 343_807, 1.08, 2382.06}}},
	{"14:35:02", []e1Row{{"5scc7", 0, 331_182, 6.72, 3360}, {"rx62w", 0, 345_151, 6.64, 3043.08}, {"kjxzs", 84, 343_807, 4.45, 2647.91}}},
	{"14:35:17", []e1Row{{"5scc7", 0, 331_182, 5.74, 2870}, {"rx62w", 1, 345_151, 5.12, 3691.20}, {"kjxzs", 1, 343_807, 4.88, 3676.04}}},
}

// sg1, phase A: s92zt and bpxrs join under the router's queue and no
// completions. The window read 1.75, 0.11 and 3.24 as they filled, and kept
// 4.74 for the rest of the run against the 6.1 req/s they served. No ITL line
// yet, so no derived figure. Token rates are the run's logged mu times its
// output length (generationTokenRate is not on the decision line).
var e1Sg1 = []e1Cycle{
	{"14:19:27", []e1Row{{"wnx8v", 0, 347_631, 6.20, 3100}}},
	{"14:19:42", []e1Row{{"wnx8v", 1, 347_631, 5.40, 2700}, {"s92zt", 197, 58_818, 0, 0}}},
	{"14:19:57", []e1Row{{"wnx8v", 1, 345_823, 6.84, 3420}, {"s92zt", 197, 301_389, 0, 875}, {"bpxrs", 222, 34_129, 0, 0}}},
	{"14:20:12", []e1Row{{"wnx8v", 1, 345_823, 6.24, 3120}, {"s92zt", 197, 358_255, 0, 2174}, {"bpxrs", 222, 190_936, 0, 56.3}}},
	{"14:20:27", []e1Row{{"wnx8v", 1, 345_823, 6.58, 3290}, {"s92zt", 197, 358_255, 3.68, 2300}, {"bpxrs", 222, 350_703, 0, 1608}}},
	{"14:20:42", []e1Row{{"wnx8v", 0, 338_350, 6.78, 3390}, {"s92zt", 52, 358_255, 5.01, 2600}, {"bpxrs", 222, 350_703, 1.32, 2353}}},
	{"14:20:57", []e1Row{{"wnx8v", 0, 338_606, 5.46, 2730}, {"s92zt", 1, 358_767, 6.13, 3065}, {"bpxrs", 69, 350_703, 4.41, 2700}}},
}

// A replica that has completed nothing, or has not served a whole rate window
// since it was first seen Ready, is not a reading of what one replica
// sustains, and a sample far under the derived figure for the same line is
// not one either. Replayed, sg2 records no sample under half the derived
// figure and never prices below it.
func TestE1Replay_AJoiningReplicaIsNotAThroughputSample(t *testing.T) {
	a := newE1Analyzer(t, true)
	cycles := append(append([]e1Cycle{}, e1Sg2...), steadyE1("14:35:32", 40, "5scc7", "rx62w", "kjxzs")...)
	decisions, lows := replayE1(t, a, cycles)

	derived := 0.0
	for _, d := range decisions {
		if d.bucket == derivedBucket {
			derived = d.mu
			break
		}
	}
	require.Greater(t, derived, 3.0, "premise: the variant has a derived mu, as sg2 did (3.1-3.5 req/s)")
	for i, low := range lows {
		if low > 0 {
			assert.GreaterOrEqual(t, low, 0.5*derived,
				"cycle %s: a throughput sample of %.4f is under half the derived %.2f", cycles[i].at, low, derived)
		}
	}
	for _, d := range decisions {
		assert.GreaterOrEqual(t, d.mu, 0.5*derived, "%s %s priced at mu %.4f (%s)", d.at, d.pod, d.mu, d.bucket)
	}
}

// The same joiners' k2. rx62w at 174 359 and 288 492 under 218 waiting, and
// kjxzs at 174 327 under 84, were filling: the run's window settled at 264 265
// while every replica then held 331-351k with no queue. Neither has served a
// window when it reads them; kjxzs a minute in, at 343 807, has. The run's
// trace holds that one admitted reading, and a window's first waits for a
// second (k2_confirm.go): kjxzs reads it again at 14:35:17, as a replica
// still queued there would.
func TestE1Replay_AJoiningReplicaIsNotAK2Sample(t *testing.T) {
	a := newE1Analyzer(t, true)
	confirm := e1Cycle{"14:35:17", []e1Row{{"5scc7", 0, 331_182, 6.72, 3360}, {"rx62w", 0, 345_151, 6.64, 3043.08}, {"kjxzs", 84, 343_807, 4.45, 2647.91}}}
	cycles := append(append([]e1Cycle{}, e1Sg2...), confirm)
	decisions, _ := replayE1(t, a, append(cycles, steadyE1("14:35:32", 4, "5scc7", "rx62w", "kjxzs")...))
	last := decisions[len(decisions)-1]
	require.Equal(t, "P2-hist", last.k2Src, "premise: the fleet is priced from its k2 window")
	assert.GreaterOrEqual(t, last.k2, int64(330_000), "the window holds what a full replica holds, not a fill")
}

// sg1 has no ITL line yet, so only the replica's own history can refuse the
// joiners' readings: no completions, or under a minute since first seen Ready.
// The first sample is s92zt's at 14:20:42, a minute after it joined, serving
// 5 req/s.
func TestE1Replay_NoDerivedFigureStillRefusesAJoiningReplica(t *testing.T) {
	a := newE1Analyzer(t, false)
	_, lows := replayE1(t, a, e1Sg1)
	for i, low := range lows {
		if low > 0 {
			assert.GreaterOrEqual(t, low, 4.0, "cycle %s: a joining replica's %.4f is in the window", e1Sg1[i].at, low)
		}
	}
	assert.Positive(t, lows[len(lows)-1], "the replicas that had served a minute were recorded")
}

// A replica that has completed nothing reports no request lengths, and with a
// footprint of zero every queue on it read as the overflow of a batch at least
// as large: the settle test admitted rx62w's 218 waiting on 174 359 resident
// on sight. Judged at the fleet's lengths it is a queue deeper than its batch.
// Both replicas are already serving on the analyzer's first cycle, so the
// joined test does not decide it.
func TestE1_AReplicaWithNoCompletionsIsJudgedAtTheFleetShape(t *testing.T) {
	a := newE1Analyzer(t, false)
	decisions, _ := replayE1(t, a, []e1Cycle{
		{"14:33:31", []e1Row{{"5scc7", 0, 344_607, 6.54, 3270}, {"rx62w", 218, 174_359, 0, 32.68}}},
	})
	for _, d := range decisions {
		if d.pod == "rx62w" {
			assert.NotEqual(t, "P1-obs", d.k2Src, "a fill under a deep queue is not a limit")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	assert.Empty(t, a.computeCapacityHistory, "nothing is written to the k2 window")
}

// A measured mu that nothing refreshes does not hold forever over a derived
// figure it sits more than MeasuredDerivedMaxRatio above: once its bucket
// has not read saturated for MeasuredMuFreshFor, the lower figure prices the
// replica. One far under derived is the lower figure and keeps pricing.
// Within the ratio the measurement keeps winning, however old -- run R's 1.27
// against a derived 0.67 is the case it must keep.
func TestE1_AStaleMeasuredMuYieldsToADisagreeingDerivedFigure(t *testing.T) {
	cases := []struct {
		name      string
		gps       float64 // per-replica tokens/s while saturated, at 500 out
		wantFinal string
	}{
		{"measured far under derived", 600, "medium"},      // 1.2 req/s vs ~3.6: the lower figure
		{"measured far over derived", 4000, derivedBucket}, // 8.0 req/s vs ~3.6
		{"measured within 2x of derived", 1250, "medium"},  // 2.5 req/s vs ~3.6
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No line while the replica saturates: nothing derived to refuse the
			// reading on, so the window is the replica's own measurement.
			a := newE1Analyzer(t, false)
			cycles := make([]e1Cycle, 0, 4)
			start, _ := time.Parse("15:04:05", "10:00:00")
			at := func(d time.Duration) string { return start.Add(d).Format("15:04:05") }
			for i := range 4 {
				cycles = append(cycles, e1Cycle{at(time.Duration(i) * time.Minute),
					[]e1Row{{"p0", 30, 330_000, tc.gps / 500, tc.gps + float64(i)}}})
			}
			_, _ = replayE1(t, a, cycles)
			publishE1Line(a)
			// Then the queue never builds again (flow control holds it upstream).
			quiet := make([]e1Cycle, 0, 12)
			for i := range 12 {
				quiet = append(quiet, e1Cycle{at(4*time.Minute + time.Duration(i)*time.Minute),
					[]e1Row{{"p0", 0, 330_000, tc.gps / 500, tc.gps}}})
			}
			decisions, _ := replayE1(t, a, quiet)
			require.Equal(t, "medium", decisions[0].bucket, "premise: the fresh measurement prices the replica")
			assert.Equal(t, tc.wantFinal, decisions[len(decisions)-1].bucket,
				fmt.Sprintf("after %s with no new sample", quiet[len(quiet)-1].at))
		})
	}
}

// The E1 rerun (2026-10-23, image c0ac4bfd9): sg1's mu sat at 6.70 req/s
// from phase C on, against the 6.0-6.1 its saturated replicas completed
// (request_success_total). The samples behind it came from replicas a minute
// after they joined, still queued: 6.70 at 5.08 completed (rc5bc, 17:12:02)
// and 6.99 at 4.86 (zv8bk, 16:51:28). Their batches were still filling, so
// they generated tokens for sequences not yet completed, and the token rate
// over the output length ran 1.32x and 1.44x ahead of what they served.
// Replayed here: p0 is measured at 6.0 and stays full, unqueued, at 6.0; p1
// reads the two filling minutes, then settles at 6.0. Two filling samples
// beside one honest one were the window's median.
func TestE1_AFillingReplicaDoesNotRaiseMu(t *testing.T) {
	a := newE1Analyzer(t, false)
	const out = 498
	full, light := int64(340_000), int64(60_000)
	cycles := make([]e1Cycle, 0, 16)
	cycles = append(cycles, []e1Cycle{
		{"17:10:01", []e1Row{{"p0", 10, full, 6.0, 6.0 * out}, {"p1", 0, light, 0.5, 0.5 * out}}},
		{"17:10:16", []e1Row{{"p0", 0, full, 6.0, 6.0 * out}, {"p1", 0, light, 0.5, 0.5 * out}}},
		{"17:10:31", []e1Row{{"p0", 0, full, 6.0, 6.0 * out}, {"p1", 0, light, 0.5, 0.5 * out}}},
		{"17:10:46", []e1Row{{"p0", 0, full, 6.0, 6.0 * out}, {"p1", 0, light, 0.5, 0.5 * out}}},
		{"17:11:02", []e1Row{{"p0", 0, full, 6.0, 6.0 * out}, {"p1", 51, 354_644, 5.076, 6.703 * out}}},
		{"17:11:17", []e1Row{{"p0", 0, full, 6.0, 6.0 * out}, {"p1", 51, 354_644, 5.1, 6.70 * out}}},
		{"17:12:03", []e1Row{{"p0", 0, full, 6.0, 6.0 * out}, {"p1", 5, 367_000, 4.858, 6.998 * out}}},
		{"17:12:18", []e1Row{{"p0", 0, full, 6.0, 6.0 * out}, {"p1", 5, 367_000, 4.86, 6.99 * out}}},
	}...)
	for i := range 8 {
		at := time.Date(0, 1, 1, 17, 13, 4+15*i, 0, time.UTC).Format("15:04:05")
		cycles = append(cycles, e1Cycle{at, []e1Row{{"p0", 0, full, 6.0, 6.0 * out}, {"p1", 0, full, 6.0, 6.0 * out}}})
	}
	decisions, _ := replayE1(t, a, cycles)
	for _, d := range decisions {
		if d.mu > 0 {
			assert.LessOrEqual(t, d.mu, 6.0*1.05, "%s %s: mu %.2f from %s, above the 6.0 the replicas complete", d.at, d.pod, d.mu, d.bucket)
		}
	}
}

// A replica whose tokens and completions agree at steady state is not
// filling, whatever the shape; one whose tokens run ahead is, and prefill,
// which generates about one token a request, is never judged.
func TestTokensOutrunCompletions(t *testing.T) {
	rm := func(rr, gps, avgOut float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{RequestRate: rr, GenerationTokenRate: gps, AvgOutputTokens: avgOut}
	}
	assert.False(t, defaultTuning.tokensOutrunCompletions(rm(6.0, 6.0*500, 500), domain.RoleDecode, 500), "steady at 1.0x")
	assert.False(t, defaultTuning.tokensOutrunCompletions(rm(6.0, 1.07*6.0*500, 500), domain.RoleDecode, 500), "1.07x, the most a full replica read on E1")
	assert.True(t, defaultTuning.tokensOutrunCompletions(rm(5.076, 6.703*500, 500), domain.RoleDecode, 500), "1.32x, a replica filling")
	assert.True(t, defaultTuning.tokensOutrunCompletions(rm(5.076, 6.703*500, 0), domain.RoleDecode, 500), "the fleet's output length where the replica has none")
	assert.False(t, defaultTuning.tokensOutrunCompletions(rm(6.0, 6.0*2000, 2000), domain.RoleDecode, 500), "its own output length, not the fleet's")
	assert.False(t, defaultTuning.tokensOutrunCompletions(rm(5.076, 6.703*500, 500), domain.RolePrefill, 500), "prefill is not judged")
	assert.False(t, defaultTuning.tokensOutrunCompletions(rm(0, 6.703*500, 500), domain.RoleDecode, 500), "no completion is refused on its own")
}
