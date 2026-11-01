package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/cycleclock"
)

// One clock: every stamp and every age in the analyzer's learned state reads
// the analyzer's clock, and the engine pins that clock once per cycle. These
// specs drive the analyzer from a fake clock alone, which is only meaningful
// once nothing in it reads the wall clock behind that fake's back.

var oneClockT0 = time.Date(2026, 10, 23, 11, 18, 0, 0, time.UTC)

// pinnedCycle is one engine cycle: the base clock reads at when the cycle
// pins, then moves to during while the cycle runs, as a wall clock does.
// With pin false the cycle runs on the bare base clock, which is what every
// read of time.Now was before.
func pinnedCycle(base *clocktesting.FakePassiveClock, c *cycleclock.Clock, at, during time.Time, pin bool, run func()) {
	base.SetTime(at)
	if pin {
		defer c.Pin()()
	}
	base.SetTime(during)
	run()
}

// One reading per cycle moves a comparison at ThroughputSampleSpacing by up
// to one cycle, and that is the one output change of the one clock. The first
// cycle pins at t0 and runs until t0+59s; the second pins at t0+60s. Pinned,
// the spacing between the two is exactly 60s, a new sample. On the bare base
// clock the first sample was stamped at t0+59s, so the second reads 1s later
// and is folded into it.
func TestOneClock_ThroughputSampleSpacingIsJudgedOnThePinnedInstant(t *testing.T) {
	for _, tc := range []struct {
		pin       bool
		samples   int
		sampledAt time.Time
	}{
		{pin: true, samples: 2, sampledAt: oneClockT0.Add(ThroughputSampleSpacing)},
		{pin: false, samples: 1, sampledAt: oneClockT0.Add(59 * time.Second)},
	} {
		base := clocktesting.NewFakePassiveClock(oneClockT0)
		c := cycleclock.New(base)
		a := NewSaturationAnalyzer(capacity.NewStore()).WithClock(c)

		pinnedCycle(base, c, oneClockT0, oneClockT0.Add(59*time.Second), tc.pin, func() {
			a.recordSaturatedThroughput("k", 3)
		})
		if tc.pin {
			assert.Equal(t, oneClockT0, a.throughputSampledAt["k"], "the sample is stamped at the pin, not when the write ran")
		}
		pinnedCycle(base, c, oneClockT0.Add(ThroughputSampleSpacing), oneClockT0.Add(ThroughputSampleSpacing+time.Second), tc.pin, func() {
			a.recordSaturatedThroughput("k", 3.2)
		})
		assert.Equal(t, tc.samples, a.saturatedThroughput["k"].Len(), "pinned=%v", tc.pin)
		assert.Equal(t, tc.sampledAt, a.throughputSampledAt["k"], "pinned=%v", tc.pin)
	}
}

// The same boundary at K2SettleLookback: a replica at 12 % of its cache with a
// deep queue is admitted once its reading a window back is flat
// (TestK2Settle_AComputeBoundAtLowOccupancyIsLearned). Pinned, the cycle at
// t0+60s compares against the reading stamped at t0 and admits it. On the bare
// base clock the reading was stamped at t0+59s, when the first cycle's settle
// test ran, and the second is refused a cycle longer.
func TestOneClock_K2SettleLookbackIsJudgedOnThePinnedInstant(t *testing.T) {
	for _, tc := range []struct {
		pin  bool
		want []string
	}{
		// Admitted, and held as the window's first reading (k2_confirm.go).
		{pin: true, want: []string{"P1-obs-unsettled", k2ReasonObsUnconfirmed}},
		{pin: false, want: []string{"P1-obs-unsettled", "P1-obs-unsettled"}},
	} {
		base := clocktesting.NewFakePassiveClock(oneClockT0)
		c := cycleclock.New(base)
		a := NewSaturationAnalyzer(capacity.NewStore()).WithClock(c)
		var prios []string
		cycle := func() {
			ctx, logs := observedCtx(t)
			rm := makeReplicaMetrics("pod-s", "variant-s", 45_000, 376_480, 40, 1000, 500)
			rm.GenerationTokenRate = 800
			rm.RequestRate = 800.0 / 500
			_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm},
				[]domain.VariantReplicaState{
					{VariantName: "variant-s", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
				}))
			require.NoError(t, err)
			decisions := logs.FilterMessage("k2-decision").All()
			require.NotEmpty(t, decisions)
			prios = append(prios, stringField(t, decisions[0].ContextMap(), "priority"))
		}
		pinnedCycle(base, c, oneClockT0, oneClockT0.Add(59*time.Second), tc.pin, cycle)
		pinnedCycle(base, c, oneClockT0.Add(K2SettleLookback), oneClockT0.Add(K2SettleLookback+time.Second), tc.pin, cycle)
		assert.Equal(t, tc.want, prios, "pinned=%v", tc.pin)
	}
}

// oneClockScenario runs a scripted fleet history from start on a pinned
// fake clock and returns each cycle's result, with AnalyzedAt made relative to
// start: a load that saturates, a quiet stretch, and saturation again past
// EpisodeGap.
func oneClockScenario(t *testing.T, start time.Time) []domain.AnalyzerResult {
	t.Helper()
	base := clocktesting.NewFakePassiveClock(start)
	c := cycleclock.New(base)
	a := NewSaturationAnalyzer(capacity.NewStore()).WithClock(c)
	type step struct {
		at     time.Duration
		tokens int64
		queue  int
		gps    float64
	}
	steps := []step{
		{0, 45_000, 40, 800},
		{15 * time.Second, 46_000, 40, 800},
		{60 * time.Second, 46_000, 40, 800},
		{75 * time.Second, 260_000, 160, 3000},
		{3 * time.Minute, 260_000, 160, 3000},
		{10 * time.Minute, 12_000, 0, 1.2},
		{2 * time.Hour, 12_000, 0, 1.2},
		{capacity.EpisodeGap + 3*time.Hour, 250_000, 160, 2900},
		{capacity.EpisodeGap + 3*time.Hour + time.Minute, 255_000, 160, 2950},
	}
	out := make([]domain.AnalyzerResult, 0, len(steps))
	for _, s := range steps {
		base.SetTime(start.Add(s.at))
		release := c.Pin()
		base.SetTime(start.Add(s.at + 3*time.Second)) // the cycle takes time
		rm := makeReplicaMetrics("pod-s", "variant-s", s.tokens, 376_480, s.queue, 1000, 500)
		rm.GenerationTokenRate = s.gps
		rm.RequestRate = s.gps / 500
		result, err := a.Analyze(logr.NewContext(context.Background(), logr.Discard()), makeAnalyzerInput([]domain.ReplicaMetrics{rm},
			[]domain.VariantReplicaState{
				{VariantName: "variant-s", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
			}))
		require.NoError(t, err)
		a.EvictStaleHistory()
		release()
		r := *result
		require.Equal(t, start.Add(s.at), r.AnalyzedAt, "the result is stamped at the pin")
		r.AnalyzedAt = time.Time{}.Add(r.AnalyzedAt.Sub(start))
		out = append(out, r)
	}
	return out
}

// The same script on the same fake clock gives the same results, whenever it
// runs and however often: run three times in one process, and again 30 days
// later on the fake clock. A wall-clock read anywhere in the learned state --
// a window's write age, a gap check -- makes them differ.
func TestOneClock_AScriptedHistoryIsDeterministic(t *testing.T) {
	want := oneClockScenario(t, oneClockT0)
	for i := range 3 {
		assert.Equal(t, want, oneClockScenario(t, oneClockT0), "run %d at t0", i)
		assert.Equal(t, want, oneClockScenario(t, oneClockT0.Add(30*24*time.Hour)), "run %d at t0+30d", i)
	}
}
