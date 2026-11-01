package saturation

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learn"
)

// k2's learner-write lines (analyzer-learner.md 6.1): at V(3), at most one
// per key per cycle, when the figure is set for the first time, moves more
// than K2WriteBand from the last logged value, or has not logged for
// LearnerHeartbeat.
func TestK2Learner_WritesOnFirstSetOnAMoveOverTheBandAndOnTheHeartbeat(t *testing.T) {
	r := newRestartRig(t, restartT0)
	writes := func(tokens int64) []map[string]any {
		t.Helper()
		row := fullRow("p0", "v", restartT0.Add(-time.Hour))
		row.TokensInUse = tokens
		row.KvCacheUsage = float64(tokens) / float64(row.TotalKvCapacityTokens)
		ctx, logs := observedCtx(t)
		r.cycleCtx(ctx, []domain.ReplicaMetrics{row}, decodeVariants("v"))
		entries := logs.FilterMessage("learner-write").All()
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.ContextMap())
		}
		return out
	}
	// The controller's first window: refused, nothing written, nothing
	// logged.
	for range waitWindow {
		assert.Empty(t, writes(260_000))
	}
	first := writes(260_000)
	require.Len(t, first, 1, "the first write logs")
	assert.Equal(t, true, first[0]["firstWrite"])
	assert.Equal(t, int64(260_000), first[0]["value"])
	assert.Equal(t, learn.K2, first[0]["learner"])
	assert.Equal(t, p1Obs, first[0]["source"])

	assert.Empty(t, writes(261_000), "a move under the band does not log")
	moved := writes(330_000)
	require.Len(t, moved, 1, "a move over the band logs: mean(260k, 261k, 330k) is 9 % over 260k")
	assert.Equal(t, int64(260_000), moved[0]["previousValue"], "against the last LOGGED value")
	assert.Equal(t, false, moved[0]["firstWrite"])

	for range 3 {
		assert.Empty(t, writes(283_667))
	}
	r.now = r.now.Add(LearnerHeartbeat)
	assert.Len(t, writes(283_667), 1, "a key that has not logged for the heartbeat logs once")
}

// A learner-write line lists at most LearnerWriteMaxPods pods, lowest
// reading first, and counts the rest in podsOmitted; it stays under the CRI
// line limit.
func TestK2Learner_AWriteLineListsSixteenPodsLowestFirst(t *testing.T) {
	r := newRestartRig(t, restartT0)
	const pods = 40
	states := decodeVariants("v")
	states[0].CurrentReplicas = pods
	rows := func() []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, pods)
		for i := range pods {
			row := fullRow(fmt.Sprintf("p%02d", i), "v", restartT0.Add(-time.Hour))
			row.TokensInUse = int64(300_000 - 1000*i)
			row.KvCacheUsage = float64(row.TokensInUse) / float64(row.TotalKvCapacityTokens)
			out = append(out, row)
		}
		return out
	}
	for range waitWindow {
		r.cycle(rows(), states)
	}
	// The first write, logged twice over: to the console encoding the
	// controller ships, for the line's length, and to an observer, for its
	// fields.
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	console := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.Level(int8(-perReplicaLogLevel)))
	observed, logs := observer.New(zapcore.Level(-perReplicaLogLevel))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(zapcore.NewTee(console, observed))))
	r.cycleCtx(ctx, rows(), states)

	lines := logs.FilterMessage("learner-write").All()
	require.Len(t, lines, 1, "one line per key per cycle, however many pods wrote")
	f := lines[0].ContextMap()
	assert.EqualValues(t, pods-LearnerWriteMaxPods, f["podsOmitted"])
	listed, ok := f["pods"].([]map[string]any)
	require.True(t, ok, "pods is a list, got %T", f["pods"])
	require.Len(t, listed, LearnerWriteMaxPods)
	readings := make([]int64, 0, len(listed))
	for _, m := range listed {
		readings = append(readings, m["reading"].(int64))
		assert.Equal(t, k2TimeBase, m["timeBase"])
		assert.Equal(t, "batch", m["passedBy"])
		assert.GreaterOrEqual(t, m["settledForSeconds"].(float64), JoinedReplicaWindow.Seconds())
	}
	assert.IsIncreasing(t, readings, "lowest reading first")
	assert.Equal(t, int64(300_000-1000*(pods-1)), readings[0])

	for _, line := range strings.Split(buf.String(), "\n") {
		assert.Less(t, len(line), criMaxLogLineBytes, "a line over the CRI limit is split by the runtime")
	}
	assert.Contains(t, buf.String(), "learner-write")
}

// learner-start: one line per model, at the default level, on the
// analyzer's first cycle for it, and never again; settleUntil is a window
// after it while the controller-restart wait is on.
func TestK2Learner_LearnerStartIsOneLinePerModelAtTheDefaultLevel(t *testing.T) {
	r := newRestartRig(t, restartT0)
	starts := make([]map[string]any, 0, 1)
	for range 3 {
		core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
		ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
		r.cycleCtx(ctx, []domain.ReplicaMetrics{fullRow("p0", "v", restartT0.Add(-time.Hour))}, decodeVariants("v"))
		for _, e := range logs.FilterMessage("learner-start").All() {
			starts = append(starts, e.ContextMap())
		}
	}
	require.Len(t, starts, 1, "one learner-start per model, at the shipped verbosity")
	assert.Equal(t, restartT0.Add(JoinedReplicaWindow).Format(time.RFC3339), starts[0]["settleUntil"])
	assert.Equal(t, map[string]int{learn.K2: 0}, starts[0]["restoredKeys"])
	assert.Equal(t, map[string]int{learn.K2: 1}, starts[0]["coldVariants"])
}

// The #303 budget, the first-write burst included (6.1: the first PR that
// adds learner-write checks the budget test catches it): on the cycle every
// replica writes its first k2, the shipped verbosity writes the same lines
// at 8 replicas as at 64, learner-write is not among them, and at V(3) it is.
func TestK2Learner_TheFirstWriteBurstStaysOffTheShippedLog(t *testing.T) {
	burst := func(verbosity, pods int) string {
		var buf bytes.Buffer
		enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
		core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.Level(int8(-verbosity)))
		ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
		rows := make([]domain.ReplicaMetrics, 0, pods)
		for i := range pods {
			rows = append(rows, fullRow(fmt.Sprintf("decode-%d", i), "variant-d", time.Time{}))
		}
		states := decodeVariants("variant-d")
		states[0].CurrentReplicas = pods
		// The analyzer alone takes its first cycle's replicas as watched
		// already, so this first cycle writes every replica's first k2.
		_, err := NewSaturationAnalyzer(capacity.NewStore()).Analyze(ctx, makeAnalyzerInput(rows, states))
		require.NoError(t, err)
		return buf.String()
	}
	shipped8, shipped64 := burst(logging.DEFAULT, 8), burst(logging.DEFAULT, 64)
	verbose64 := burst(perReplicaLogLevel, 64)
	require.Equal(t, 1, linesWith(verbose64, "learner-write"), "premise: the burst writes, at V(3)")
	assert.Zero(t, linesWith(shipped64, "learner-write"))
	assert.Equal(t, 1, linesWith(shipped64, "learner-start"))
	assert.Equal(t, lineCount(shipped8), lineCount(shipped64),
		"lines per cycle at the shipped verbosity must not grow with the replica count, first writes included")
}

// Forget reaches every learner on learn's list, by name (6.2): each is
// forgotten by the analyzer's own code, and its keys are reported forgotten
// by the operator on the variant's next report. A key k2 evicts is reported
// forgotten by eviction.
func TestK2Learner_ForgetReachesEveryLearnerOnTheList(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.mu.Lock()
	for _, name := range learn.Learners() {
		_, reached := a.forgetLearnerLocked(name, "ns", "m", "v", learn.ByOperator)
		assert.True(t, reached, "the learner %q is on learn's list and the analyzer does not forget it", name)
	}
	_, reached := a.forgetLearnerLocked("no-such-learner", "ns", "m", "v", learn.ByOperator)
	a.mu.Unlock()
	assert.False(t, reached, "negative control: a name the analyzer does not know is not reached")

	r := newRestartRig(t, restartT0)
	row := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{fullRow("p0", "v", restartT0.Add(-time.Hour))}
	}
	settled(t, r, row, decodeVariants("v"))
	key := firstKey(r.a)
	require.NotEmpty(t, key, "premise: k2 holds a window")
	r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
	down := fullRow("p0", "v", restartT0.Add(-time.Hour))
	down.QueueLength = 0 // not saturated: nothing written again this cycle
	r.cycle([]domain.ReplicaMetrics{down}, decodeVariants("v"))
	reports := r.a.LearnerReports()["test-ns|test-model"]
	require.Len(t, reports, 1)
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, reports[0].Forgotten)
	assert.Empty(t, reports[0].PricingKey, "no key remains")

	r.cycle([]domain.ReplicaMetrics{down}, decodeVariants("v"))
	assert.Empty(t, r.a.LearnerReports()["test-ns|test-model"][0].Forgotten, "reported once")

	// Eviction: a window unused past its retention, its model still served.
	settled(t, r, row, decodeVariants("v"))
	key = firstKey(r.a)
	r.a.mu.Lock()
	aged := r.now.Add(-r.a.horizons.BucketRetention - time.Hour)
	r.a.computeCapacityHistory[key].ObservedAt(aged)
	r.a.computeCapacityHistory[key].TouchAt(aged)
	r.a.mu.Unlock()
	r.a.EvictStaleHistory()
	r.cycle([]domain.ReplicaMetrics{down}, decodeVariants("v"))
	got := r.a.LearnerReports()["test-ns|test-model"]
	require.Len(t, got, 1)
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByEvicted}}, got[0].Forgotten)
}

// What k2 keeps as a learner goes when the sweep lets its model go: a model
// unseen past VariantTimeout leaves no report, no pending forgotten key, no
// learner-start mark, no own-tracker sighting and, its windows gone, no
// logged value. Back, it is a model seen for the first time.
func TestK2Learner_TheSweepLetsAGoneModelsLearnerStateGo(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := restartT0
	a.now = func() time.Time { return now }
	row := fullRow("p0", "v", restartT0.Add(-time.Hour))
	for range 3 {
		_, err := a.Analyze(context.Background(), makeAnalyzerInput([]domain.ReplicaMetrics{row}, decodeVariants("v")))
		require.NoError(t, err)
		now = now.Add(restartCycle)
	}
	a.mu.Lock()
	require.NotEmpty(t, a.k2Logged, "premise: a write was logged")
	require.NotEmpty(t, a.learnerReports)
	require.NotEmpty(t, a.learnerStarted)
	require.NotEmpty(t, a.ownSettled)
	a.mu.Unlock()

	now = now.Add(a.Horizons().BucketRetention + time.Hour)
	a.EvictStaleHistory()
	a.mu.Lock()
	assert.Empty(t, a.k2Settle, "premise: the settle memo is swept")
	assert.Empty(t, a.k2Logged)
	assert.Empty(t, a.learnerReports)
	assert.Empty(t, a.learnerStarted)
	assert.Empty(t, a.ownSettled)
	assert.Empty(t, a.k2Forgotten, "the eviction report of a model that will not report again")
	a.mu.Unlock()
	_, ok := a.ownSettle.Replica(settleModel("test-ns", "test-model"), fullRowKey("p0", "v"))
	assert.False(t, ok, "the own tracker forgot the model")

	ctx, logs := observedCtx(t)
	_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{row}, decodeVariants("v")))
	require.NoError(t, err)
	assert.Equal(t, 1, logs.FilterMessage("learner-start").Len(), "back, the model logs learner-start again")
	writes := logs.FilterMessage("learner-write").All()
	require.Len(t, writes, 1)
	assert.Equal(t, true, writes[0].ContextMap()["firstWrite"], "and its key's first write is a first write")
}

// A report carries the cycle's admission outcomes by reason, the key that
// priced the variant, its window's size and the time of its newest reading.
func TestK2Learner_TheReportCountsTheCycle(t *testing.T) {
	r := newRestartRig(t, restartT0)
	r.cycle([]domain.ReplicaMetrics{fullRow("p0", "v", restartT0.Add(-time.Hour))}, decodeVariants("v"))
	rep := r.a.LearnerReports()["test-ns|test-model"]
	require.Len(t, rep, 1)
	assert.Equal(t, map[learn.Reason]int64{learn.Reason(k2ReasonObsUnsettled): 1}, rep[0].Counts)
	assert.Zero(t, rep[0].Writes)
	assert.Empty(t, rep[0].PricingKey, "no window yet")

	settled(t, r, func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{fullRow("p0", "v", restartT0.Add(-time.Hour))}
	}, decodeVariants("v"))
	rep = r.a.LearnerReports()["test-ns|test-model"]
	require.Len(t, rep, 1)
	assert.Equal(t, map[learn.Reason]int64{p1Obs: 1}, rep[0].Counts)
	assert.Equal(t, int64(1), rep[0].Writes)
	assert.Equal(t, firstKey(r.a), rep[0].PricingKey)
	assert.Equal(t, learn.Source(p1Obs), rep[0].Source)
	assert.Equal(t, r.now.Add(-restartCycle), rep[0].LastAdmitted)
	assert.Positive(t, rep[0].Samples)
}

// firstKey is the analyzer's one k2 key, "" when it holds none.
func firstKey(a *SaturationAnalyzer) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.computeCapacityHistory {
		return k
	}
	return ""
}
