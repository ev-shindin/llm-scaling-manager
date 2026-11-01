package saturation

import (
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

// Mu as a learner (mu_learner.go; analyzer-learner.md 6.1, section 7 PR 3).

// After a controller restart a measured mu waits out the settle k2 waits:
// it is written only beside k2's P1-obs write, so no sample is taken for a
// window from the controller's first sight of the replica. Its first write
// logs learner-write with the pod, its reading and k2's stamp on it -- how
// long the replica had been settled, and which settle test let it through.
func TestMuLearner_AMeasuredMuWaitsOutThePostRestartSettleAndLogsItsFirstSample(t *testing.T) {
	r := newRestartRig(t, restartT0)
	row := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{fullRow("p0", "v", restartT0.Add(-time.Hour))}
	}
	for i := range waitWindow {
		ctx, logs := observedCtx(t)
		r.cycleCtx(ctx, row(), decodeVariants("v"))
		assert.Zero(t, r.a.measuredSamples(), "cycle %d: no mu sample inside the settle", i)
		assert.Empty(t, learnerWrites(logs, learn.Mu), "cycle %d: nothing to log", i)
		rep := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.Mu)
		require.Len(t, rep, 1)
		assert.Empty(t, rep[0].Counts, "cycle %d: a refused k2 reading is not offered to mu", i)
		assert.Empty(t, rep[0].PricingKey)
	}
	written := r.now
	ctx, logs := observedCtx(t)
	r.cycleCtx(ctx, row(), decodeVariants("v"))
	require.Equal(t, 1, r.a.measuredSamples(), "a window on, the sample is written")

	lines := learnerWrites(logs, learn.Mu)
	require.Len(t, lines, 1)
	f := lines[0].ContextMap()
	assert.Equal(t, true, f["firstWrite"])
	assert.Equal(t, domain.ServiceRateSourceMeasured, f["source"])
	assert.InDelta(t, 6.0, f["value"], 1e-9, "3000 generated tokens/s over 500-token outputs")
	pods, ok := f["pods"].([]map[string]any)
	require.True(t, ok, "pods is a list, got %T", f["pods"])
	require.Len(t, pods, 1)
	assert.Equal(t, "p0", pods[0]["pod"])
	assert.Equal(t, muTimeBase, pods[0]["timeBase"])
	assert.Equal(t, "batch", pods[0]["passedBy"], "k2's stamp")
	assert.GreaterOrEqual(t, pods[0]["settledForSeconds"].(float64), JoinedReplicaWindow.Seconds(), "k2's stamp")

	rep := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.Mu)
	require.Len(t, rep, 1)
	assert.Equal(t, map[learn.Reason]int64{muReasonSampled: 1}, rep[0].Counts)
	assert.Equal(t, int64(1), rep[0].Writes)
	assert.Equal(t, learn.Source(domain.ServiceRateSourceMeasured), rep[0].Source)
	assert.Equal(t, 1, rep[0].Samples)
	assert.Equal(t, written, rep[0].LastAdmitted)
	assert.Contains(t, rep[0].PricingKey, "|v|test-model|")
}

// measuredSamples is how many samples the analyzer's own mu windows hold.
func (a *SaturationAnalyzer) measuredSamples() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for k, ra := range a.saturatedThroughput {
		if !isPrefillBoundKey(k) {
			n += ra.Len()
		}
	}
	return n
}

// learner-write for mu logs the priced figure of a key at most once a
// cycle: when it is set for the first time, moves MuWriteBand from what was
// last logged, flips source -- measured, derived, served-siblings -- or has
// not logged for LearnerHeartbeat.
func TestMuLearner_LogsAChoiceFlipAMoveOverTheBandAndTheHeartbeat(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := restartT0
	a.now = func() time.Time { return now }
	const key = "ns|v|m|H200|1|decode|imedium|medium|q5"
	step := func(rate float64, derived, siblings bool) []map[string]any {
		t.Helper()
		a.beginMuCycle("ns", "m")
		a.noteMuKey("ns", "m", "v", domain.RoleDecode, key)
		if siblings {
			a.noteMuCount("ns", "m", "v", muReasonYieldSiblings)
		}
		caps := []capacity.ReplicaCapacity{{PodName: "p0", VariantName: "v", SaturatedThroughput: rate,
			SaturatedThroughputDerived: derived}}
		ctx, logs := observedCtx(t)
		a.closeMuCycle(ctx, "ns", "m", caps)
		now = now.Add(restartCycle)
		entries := learnerWrites(logs, learn.Mu)
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.ContextMap())
		}
		return out
	}
	first := step(3.0, false, false)
	require.Len(t, first, 1)
	assert.Equal(t, true, first[0]["firstWrite"])
	assert.Equal(t, domain.ServiceRateSourceMeasured, first[0]["source"])
	assert.Equal(t, key, first[0]["key"])

	flip := step(3.0, true, false)
	require.Len(t, flip, 1, "a source flip logs, whatever the value")
	assert.Equal(t, domain.ServiceRateSourceDerived, flip[0]["source"])
	assert.Equal(t, domain.ServiceRateSourceMeasured, flip[0]["previousSource"])

	assert.Empty(t, step(3.1, true, false), "a move under the band does not log")
	siblings := step(3.0, false, true)
	require.Len(t, siblings, 1)
	assert.Equal(t, muSourceSiblings, siblings[0]["source"], "a sibling yield is a source of its own")
	back := step(3.0, false, false)
	require.Len(t, back, 1)
	assert.Equal(t, domain.ServiceRateSourceMeasured, back[0]["source"])

	moved := step(3.4, false, false)
	require.Len(t, moved, 1, "13 % over the last logged value")
	assert.Equal(t, 3.0, moved[0]["previousValue"])
	for range 3 {
		assert.Empty(t, step(3.4, false, false))
	}
	now = now.Add(LearnerHeartbeat)
	assert.Len(t, step(3.4, false, false), 1, "a key that has not logged for the heartbeat logs once")
}

// throughput-demand-floor names the learned figures its term priced from:
// the mu key and source of the role's variant, and the start time the
// backlog was projected over, measured or none.
func TestMuLearner_TheFloorNamesItsMuAndItsStartTime(t *testing.T) {
	for _, start := range []float64{0, 90} {
		t.Run(fmt.Sprintf("start %v s", start), func(t *testing.T) {
			rig, err := floorRig()
			require.NoError(t, err)
			ctx, logs := observedCtx(t)
			rig.ctx = ctx
			rows := []domain.ReplicaMetrics{
				imbRow("r0", "v", 0.95, 300, 1000, 1000, 1.0),
				imbRow("r1", "v", 0.10, 0, 1000, 1000, 0.33),
				imbRow("r2", "v", 0.10, 0, 1000, 1000, 0.33),
				imbRow("r3", "v", 0.10, 0, 1000, 1000, 0.34),
			}
			for i := range rows {
				rows[i].StartSeconds = start
			}
			for range 8 {
				_, err := rig.analyze(rows, aggStates(len(rows)), 2.5, propCycle)
				require.NoError(t, err)
			}
			floors := logs.FilterMessage("throughput-demand-floor").All()
			require.NotEmpty(t, floors, "premise: the floor binds")
			f := floors[len(floors)-1].ContextMap()
			key, _ := f["muKey"].(string)
			assert.True(t, strings.HasPrefix(key, "test-ns|v|test-model|"), "muKey %q", key)
			assert.Equal(t, domain.ServiceRateSourceMeasured, f["muSource"])
			if start > 0 {
				assert.Equal(t, start, f["startEstimateSeconds"])
				assert.Equal(t, startSource(true), f["startSource"])
			} else {
				assert.Equal(t, 0.0, f["startEstimateSeconds"])
				assert.Equal(t, "none", f["startSource"], "no start measured: the floor projected none")
			}
		})
	}
}

// A model's learner-start counts mu's restored keys and cold variants
// beside k2's.
func TestMuLearner_LearnerStartCountsMu(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := restartT0
	a.now = func() time.Time { return now }
	ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, now.Add(-time.Hour))
	ra.AddAt(3.0, now.Add(-time.Hour))
	a.saturatedThroughput["test-ns|v|test-model|H200|1|decode|imedium|medium|q5"] = ra
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{fullRow("p0", "v", now.Add(-time.Hour))},
		decodeVariants("v", "w")))
	require.NoError(t, err)
	starts := logs.FilterMessage("learner-start").All()
	require.Len(t, starts, 1)
	f := starts[0].ContextMap()
	assert.Equal(t, map[string]int{learn.K2: 0, learn.Mu: 1}, f["restoredKeys"])
	assert.Equal(t, map[string]int{learn.K2: 2, learn.Mu: 1}, f["coldVariants"])
	assert.InDelta(t, time.Hour.Seconds(), f["oldestAdmittedAgeSeconds"], 1e-9)
}
