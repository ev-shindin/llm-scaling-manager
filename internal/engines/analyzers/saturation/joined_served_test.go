package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
)

// lateRow is r0 on the time base: Ready an hour before the run, every gauge
// known, holding tokens with queue waiting and completing at rate.
func lateRow(ready time.Time, tokens int64, queue int, rate float64) domain.ReplicaMetrics {
	rm := satRow("r0", tokens, queue, rate)
	rm.ReadySince = ready
	rm.KvCacheUsageKnown, rm.GenerationTokenRateKnown, rm.RequestRateKnown = true, true, true
	if tokens == 0 {
		rm.AvgITLEmpty = true
	}
	return rm
}

// A full, queued replica that has completed nothing and generated nothing
// over the minute is not idle: it holds KV. So its first completion is not a
// return, and its readings are not held as a late-routed replica's.
func TestJoinedServed_AFullQueuedReplicaWithNoCompletionIsNotIdle(t *testing.T) {
	rm := lateRow(propStart.Add(-time.Hour), int64(0.95*float64(propKv)), 10, 0)
	rm.GenerationTokenRate = 0
	rows := settlerow.Of([]domain.ReplicaMetrics{rm}, map[string]string{"v": domain.RoleDecode})
	require.NotEqual(t, settle.Idle, rows[0].Activity(), "a replica holding KV is not idle, whatever its rates read")

	rig := newPropRig()
	ready := rig.clock.Add(-time.Hour)
	full := propKv * 95 / 100
	for i := range 12 {
		rate := 0.0
		if i >= 4 {
			rate = 0.2
		}
		rm := lateRow(ready, full, 10, rate)
		if rate == 0 {
			rm.GenerationTokenRate = 0
		}
		_, err := rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 0.2, propCycle)
		require.NoError(t, err)
		r, ok := rig.a.settleView().Replica(settleModel("test-ns", "test-model"), settlerow.Key(rm))
		require.True(t, ok)
		require.Empty(t, r.Returns, "cycle %d: a full replica's first completion is no return", i)
	}
	require.Positive(t, rig.k2(""), "the full replica's readings were recorded")
}

// A replica Ready for an hour, idle, then routed to: neither k2 nor mu is
// written for a rate window from its first served row. Refusing mu alone
// would write k2 there. With the switch off, k2 is written inside the window.
func TestJoinedServed_ALateRoutedReplicaWritesNoK2ForAWindow(t *testing.T) {
	for _, on := range []bool{true, false} {
		rig := newPropRig()
		rig.a.joinedFromFirstServed = on
		ready := rig.clock.Add(-time.Hour)
		const load = 125
		rate := make([]float64, 6*60)
		for s := load; s < len(rate); s++ {
			rate[s] = 0.2
		}
		full := propKv * 92 / 100
		firstServed := -1
		k2InWindow, muInWindow := false, false
		for i, rr := range sampleTimeBase(rate).Rate1m {
			at := 60 + i*int(propCycle/time.Second)
			rm := lateRow(ready, 0, 0, rr)
			if at >= load {
				rm = lateRow(ready, full, 10, rr)
			}
			if rr > 0 && firstServed < 0 {
				firstServed = at
			}
			_, err := rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 0.2, propCycle)
			require.NoError(t, err)
			if firstServed >= 0 && at-firstServed < int(JoinedReplicaWindow/time.Second) {
				k2InWindow = k2InWindow || rig.k2("") > 0 || rig.k2Admitted()
				muInWindow = muInWindow || rig.measuredMu() > 0
			}
		}
		if on {
			require.False(t, k2InWindow, "on: k2 written within a rate window of the first served row")
			require.False(t, muInWindow, "on: mu written within a rate window of the first served row")
			require.Positive(t, rig.k2(""), "on: k2 is recorded once the window has passed")
			require.Positive(t, rig.measuredMu(), "on: mu is recorded once the window has passed")
		} else {
			require.True(t, k2InWindow, "off: k2 is written inside the window, as before")
		}
	}
}

// Only the first return of a settle holds: a replica that served, went idle
// and was routed to again under the same settle is admitted at once.
func TestJoinedServed_OnlyTheFirstReturnOfASettleHolds(t *testing.T) {
	rig := newPropRig()
	ready := rig.clock.Add(-time.Hour)
	full := propKv * 92 / 100
	step := func(tokens int64, queue int, rate float64) {
		_, err := rig.analyze([]domain.ReplicaMetrics{lateRow(ready, tokens, queue, rate)}, decodeStates(1), 0.2, propCycle)
		require.NoError(t, err)
	}
	step(0, 0, 0)
	step(0, 0, 0)
	in := k2SettleInput{namespace: "test-ns", modelID: "test-model", variant: "v", pod: "r0", ready: true}
	step(full, 10, 0.2) // the first return
	require.True(t, rig.a.firstReturnUnderWindow(in, rig.clock.Add(-propCycle)), "the first return holds")
	for range 8 {
		step(full, 10, 0.2)
	}
	require.False(t, rig.a.firstReturnUnderWindow(in, rig.clock), "a rate window on, it holds no more")
	step(0, 0, 0)
	step(0, 0, 0)
	step(full, 10, 0.2) // a second return, same settle
	r, ok := rig.a.settleView().Replica(settleModel("test-ns", "test-model"), settlerow.Key(lateRow(ready, 0, 0, 0)))
	require.True(t, ok)
	require.Len(t, r.Returns, 2, "the tracker saw both returns")
	require.False(t, rig.a.firstReturnUnderWindow(in, rig.clock.Add(-propCycle)), "the second return does not hold")
}

// A prefill replica is read by its role: it holds no KV and reads no ITL, and
// only its request rate says it served. One Ready for an hour, idle, then
// routed to is held for a rate window from that row. Read as decode, its
// serving rows (KV empty, no token generated) would be idle, and nothing
// would hold it.
func TestJoinedServed_ALateRoutedPrefillReplicaIsHeld(t *testing.T) {
	rig := newPropRig()
	ready := rig.clock.Add(-time.Hour)
	states := []domain.VariantReplicaState{
		{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
	}
	row := func(rate float64) domain.ReplicaMetrics {
		rm := satRow("r0", 0, 0, rate)
		rm.ReadySince = ready
		rm.GenerationTokenRate = 0
		rm.KvCacheUsageKnown, rm.GenerationTokenRateKnown, rm.RequestRateKnown = true, true, true
		return rm
	}
	in := k2SettleInput{namespace: "test-ns", modelID: "test-model", variant: "v", pod: "r0", ready: true}
	for range 3 {
		_, err := rig.analyze([]domain.ReplicaMetrics{row(0)}, states, 0.5, propCycle)
		require.NoError(t, err)
	}
	served := rig.clock
	for i := range 6 {
		_, err := rig.analyze([]domain.ReplicaMetrics{row(0.5)}, states, 0.5, propCycle)
		require.NoError(t, err)
		at := served.Add(time.Duration(i) * propCycle)
		require.Equal(t, at.Sub(served) < JoinedReplicaWindow, rig.a.firstReturnUnderWindow(in, at),
			"%s after the prefill replica's first served row", at.Sub(served))
	}
}
