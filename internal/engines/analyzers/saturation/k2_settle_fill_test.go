package saturation

import (
	"bufio"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// fillRow is one cycle of a simulated replica: seconds from the load step,
// the collector's one-minute peak of resident tokens and of the queue, the
// generated tokens per second over the minute, the shape and the cache.
type fillRow struct {
	t       float64
	tokens  int64
	queue   int
	gps     float64
	in, out float64
	cache   int64
}

// readFill reads a testdata/k2-fill trace. The traces are a toy decode
// engine's (max_num_seqs 256, ITL = 20 ms + 30 ms x k, a closed loop of 416
// clients stepping from idle), scraped every 10 s and read as the collector
// reads it every 15 s; they came from the statistics review of the settle
// test (2026-10-23).
func readFill(t *testing.T, name string) []fillRow {
	t.Helper()
	f, err := os.Open("testdata/k2-fill/" + name)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	var rows []fillRow
	for sc.Scan() {
		p := strings.Split(strings.TrimSpace(sc.Text()), ",")
		require.Len(t, p, 7)
		var r fillRow
		r.t, _ = strconv.ParseFloat(p[0], 64)
		r.tokens, _ = strconv.ParseInt(p[1], 10, 64)
		r.queue, _ = strconv.Atoi(p[2])
		r.gps, _ = strconv.ParseFloat(p[3], 64)
		r.in, _ = strconv.ParseFloat(p[4], 64)
		r.out, _ = strconv.ParseFloat(p[5], 64)
		r.cache, _ = strconv.ParseInt(p[6], 10, 64)
		rows = append(rows, r)
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, rows, "an empty trace replays nothing")
	return rows
}

// A replica whose batch is capped by max_num_seqs fills for minutes after a
// load step on long outputs, and its first k2 must be its filled figure, not
// one taken mid-fill: k2 is never raised. The steady figure is the median of
// its queued readings 300-600 s after the step; the first figure recorded
// must not be under 0.94 of it (before the window it was 0.63 and 0.49 of
// it). The short shape fills at once and is admitted on sight, at +22 s as
// before; its 6 % overshoot is the fill's own peak, which the settle test
// does not judge (its queue is the overflow of a full batch).
//
// The first admitted reading is held for the next (k2_confirm.go), so the
// first figure is recorded a cycle later, and is the mean of the first two
// admitted readings: the figure the window held a cycle after its first write
// before the hold. On the long shapes both sit on the fill's peak, 7-10 %
// over the steady figure (the first alone was within 6 %); the window comes
// down to it as it fills.
func TestK2Settle_ASlowFillIsLearnedFilled(t *testing.T) {
	for _, c := range []struct {
		trace     string
		notBefore float64 // seconds after the step a filled reading can exist
		latest    float64 // seconds after the step it must be recorded by
		maxRatio  float64
	}{
		{"seq-capped-1000-500.csv", 0, 37, 1.07},
		{"seq-capped-1000-4000.csv", 60, 150, 1.10},
		{"seq-capped-1000-6000.csv", 60, 150, 1.10},
	} {
		rows := readFill(t, c.trace)
		var steady []float64
		for _, r := range rows {
			if r.t >= 300 && r.t <= 600 && r.queue >= 5 {
				steady = append(steady, float64(r.tokens))
			}
		}
		require.NotEmpty(t, steady, "%s: no steady readings to compare with", c.trace)
		slices.Sort(steady)
		s := steady[len(steady)/2]

		a := NewSaturationAnalyzer(capacity.NewStore())
		start := time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC)
		first, firstAt := int64(0), math.NaN()
		for _, r := range rows {
			at := start.Add(time.Duration((r.t + 1000) * float64(time.Second)))
			a.now = func() time.Time { return at }
			ctx, logs := observedCtx(t)
			rm := makeReplicaMetrics("pod-s", "variant-s", r.tokens, r.cache, r.queue, r.in, r.out)
			rm.GenerationTokenRate = r.gps
			_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm},
				[]domain.VariantReplicaState{
					{VariantName: "variant-s", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
				}))
			require.NoError(t, err)
			ds := logs.FilterMessage("k2-decision").All()
			if len(ds) == 0 || stringField(t, ds[0].ContextMap(), "priority") != capacity.K2SrcObserved.String() {
				continue
			}
			for _, e := range logs.FilterMessage("replica-capacity-decision").All() {
				first = e.ContextMap()["k2ComputeBound"].(int64)
			}
			firstAt = r.t
			break
		}
		t.Logf("%s: steady %.0f; first k2 recorded at %+.0f s, %.0f = %.3f of steady", c.trace, s, firstAt, float64(first), float64(first)/s)
		if math.IsNaN(firstAt) {
			t.Errorf("%s: no k2 was ever recorded", c.trace)
			continue
		}
		if ratio := float64(first) / s; ratio < 0.94 || ratio > c.maxRatio {
			t.Errorf("%s: first k2 %.0f is %.3f of the steady %.0f: recorded mid-fill", c.trace, float64(first), ratio, s)
		}
		if firstAt < c.notBefore || firstAt > c.latest {
			t.Errorf("%s: first k2 recorded at %+.0f s, want between %+.0f and %+.0f", c.trace, firstAt, c.notBefore, c.latest)
		}
	}
}

// What the window costs at a true limit whose gauge is noisy: a refused
// reading only waits, but a refusal rate near one would leave a bucket that
// never learns. The resident KV at the limit is a lognormal gauge scraped
// every 10 s, read as its one-minute peak every 15 s, queue standing
// throughout. Measured over 4000 cycles per CV, refusals are 1 / 17 / 55 / 69
// % at CV 0.1 / 0.2 / 0.5 / 0.8, against 0 / 4 / 18 / 24 % comparing a
// cycle back; the first k2 still comes within 12 minutes in every one of 200
// independent starts (the log prints the median and the 95th percentile).
func TestK2Settle_ANoisyLimitStillLearns(t *testing.T) {
	const base = 200_000.0
	rng := rand.New(rand.NewPCG(7, 282))
	readings := func(cv float64, cycles int) []int64 {
		sigma := math.Sqrt(math.Log(1 + cv*cv))
		scrapes := make([]float64, 0, cycles*3/2+8)
		out := make([]int64, 0, cycles)
		for c := 0; c < cycles; c++ {
			at := 70 + 15*float64(c)
			for float64(len(scrapes))*10 <= at {
				scrapes = append(scrapes, base*math.Exp(sigma*rng.NormFloat64()-sigma*sigma/2))
			}
			peak := 0.0
			for i := len(scrapes) - 1; i >= 0 && float64(i)*10 > at-60; i-- {
				peak = max(peak, scrapes[i])
			}
			out = append(out, int64(peak))
		}
		return out
	}
	start := time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC)
	settledAt := func(rs []int64) []bool {
		trace := make([]settleReading, 0, len(rs))
		ok := make([]bool, len(rs))
		for i, tok := range rs {
			now := start.Add(time.Duration(i) * 15 * time.Second)
			ok[i] = defaultTuning.settledOverWindow(trace, now, tok, 5)
			trace = append(trace, settleReading{at: now, queue: 40, tokens: tok})
			for len(trace) > 0 && now.Sub(trace[0].at) > K2SettleMaxGap {
				trace = trace[1:]
			}
		}
		return ok
	}
	for _, c := range []struct {
		cv         float64
		maxRefused float64
	}{
		{0.1, 0.05}, {0.2, 0.30}, {0.5, 0.70}, {0.8, 0.85},
	} {
		ok := settledAt(readings(c.cv, 4000))
		refused := 0
		for _, o := range ok[4:] {
			if !o {
				refused++
			}
		}
		frac := float64(refused) / float64(len(ok)-4)
		const horizon = 48 // cycles: 12 minutes
		firsts := make([]int, 0, 200)
		for range 200 {
			first := slices.Index(settledAt(readings(c.cv, horizon)), true)
			if first < 0 {
				first = horizon
			}
			firsts = append(firsts, first)
		}
		slices.Sort(firsts)
		t.Logf("gauge CV %.1f: %.1f %% of readings at the limit refused; first k2 after %d cycles at the median, %d at p95, %d at worst",
			c.cv, 100*frac, firsts[100], firsts[190], firsts[199])
		if frac > c.maxRefused {
			t.Errorf("gauge CV %.1f: %.1f %% refused, more than %.0f %%", c.cv, 100*frac, 100*c.maxRefused)
		}
		if firsts[199] >= horizon {
			t.Errorf("gauge CV %.1f: a start learned nothing in %d cycles at its limit", c.cv, horizon)
		}
	}
}
