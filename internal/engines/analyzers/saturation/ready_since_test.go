package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
)

// The analyzer ages a replica from the Ready time the collector puts on its
// row, so a pod back from a restart under the same name is fresh again; zero
// stays unknown.
func TestReadySince_TheAnalyzerReadsTheCollectorsReadyTime(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	since := time.Date(2026, 10, 23, 10, 1, 55, 0, time.UTC)
	assert.Equal(t, since, a.readySince(domain.ReplicaMetrics{PodName: "p1", Ready: true, ReadySince: since}))
	assert.True(t, a.readySince(domain.ReplicaMetrics{PodName: "p1", Ready: true}).IsZero())
}

// Review probe (round 2, a rejoining pod taken as established). p1's engine
// restarts -- a container restart, or an LWS group restart with stable pod
// names -- so it reports no metrics for 75 s, and comes back handed the
// router's queue while its batch fills. Its Pod became Ready again a few
// seconds ago, and it is as fresh as a new pod: its filling reading (174 327
// tokens, 1.0 req/s, against 340 000 and 6.3 served) is neither k2 nor mu.
//
// The Ready times ride on the rows, as the collector sets them, and the
// analyzer reads them through its production hook.
func TestReadySince_ARejoiningPodIsAsFreshAsANewOne(t *testing.T) {
	day := func(clock string) time.Time {
		c, err := time.Parse("15:04:05", clock)
		require.NoError(t, err)
		return time.Date(2026, 10, 23, c.Hour(), c.Minute(), c.Second(), 0, time.UTC)
	}
	// run replays the restart and returns the lowest k2 and mu any window
	// holds afterwards (0 for none). readyKnown reports the Pods' Ready
	// times on the rows; otherwise the analyzer has only its own first sight.
	run := func(returning string, readyKnown bool) (k2Low int64, muLow float64) {
		t.Helper()
		a := newE1Analyzer(t, false)
		var readySince func(pod string) time.Time
		if readyKnown {
			readySince = func(pod string) time.Time {
				if pod == "p0" {
					return day("09:00:00")
				}
				return day("10:01:55") // back from its restart
			}
		}
		steady := e1Row{"p0", 0, 340_000, 6.3, 3150}
		cycles := []e1Cycle{
			{"10:00:00", []e1Row{steady, {"p1", 0, 340_000, 6.3, 3150}}},
			{"10:00:15", []e1Row{steady, {"p1", 0, 340_000, 6.3, 3150}}},
			{"10:00:30", []e1Row{steady, {"p1", 0, 340_000, 6.3, 3150}}},
			{"10:00:45", []e1Row{steady}},
			{"10:01:00", []e1Row{steady}},
			{"10:01:15", []e1Row{steady}},
			{"10:01:30", []e1Row{steady}},
			{"10:01:45", []e1Row{steady}},
			{"10:02:00", []e1Row{steady, {returning, 84, 174_327, 1.0, 500}}},
		}
		_, lows := replayE1Ready(t, a, cycles, readySince)
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, ra := range a.computeCapacityHistory {
			for _, v := range ra.Snapshot().Values {
				if k2Low == 0 || int64(v) < k2Low {
					k2Low = int64(v)
				}
			}
		}
		return k2Low, lows[len(lows)-1]
	}
	newK2, newMu := run("p2", true)
	backK2, backMu := run("p1", true)
	blindK2, blindMu := run("p1", false)
	t.Logf("filling reading from a new pod: k2 low %d, mu low %.3f", newK2, newMu)
	t.Logf("from p1 back after 75 s, its Ready time known: k2 low %d, mu low %.3f", backK2, backMu)
	t.Logf("from p1 back after 75 s, no Ready time (the analyzer's first sight only): k2 low %d, mu low %.3f", blindK2, blindMu)
	require.Zero(t, newK2, "premise: a new pod's filling reading is refused by the joined test")
	assert.Zero(t, backK2, "a pod back from an engine restart wrote its filling reading as k2, which is never raised")
	assert.Zero(t, backMu, "and its 1.0 req/s (against 6.3 served) became the bucket's mu")
}

// A Ready time a window old is an established replica when the controller
// has seen it that long. A Ready time first seen now is held for a window
// from now, however old it reads: the Ready time is the node's clock, and a
// node behind the controller's dates a replica that has just joined into the
// past (review: one 60 s behind skipped the window, and its filling reading
// became k2). On the controller's first cycle the first sight is that cycle,
// so every replica waits a window once (SwitchK2WaitsAfterRestart); with the
// switch off, a replica already Ready then is aged from its Ready time
// alone, as before the settle tracker.
func TestReadySince_AKnownOldReadyTimeIsEstablished(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	old := now.Add(-2 * JoinedReplicaWindow)
	// joined observes rows into a fresh tracker as the engine does, one
	// cycle per call, and asks k2 whether pod has joined at the last one.
	type cycle struct {
		at      time.Time
		readyAt time.Time
	}
	joined := func(waits bool, pod string, cycles ...cycle) bool {
		t.Helper()
		tr := settle.NewTracker()
		a := NewSaturationAnalyzer(capacity.NewStore()).WithSettleView(tr)
		a.k2WaitsAfterRestart = waits
		var got bool
		for _, c := range cycles {
			tr.Observe(settleModel("ns", "m"), []settle.Row{{Key: settle.Key{Pod: pod, Variant: "v"}, Ready: true, ReadySince: c.readyAt}}, c.at)
			got, _, _, _ = a.k2Joined(k2SettleInput{namespace: "ns", modelID: "m", variant: "v", pod: pod, ready: true}, cycles[0].at, c.at)
		}
		return got
	}
	half, window := now.Add(JoinedReplicaWindow/2), now.Add(JoinedReplicaWindow)

	// The controller's first cycle, the switch off: as before the tracker.
	assert.False(t, joined(false, "p", cycle{now, old}), "switch off: Ready two windows ago, on the first cycle")
	assert.True(t, joined(false, "q", cycle{now, now.Add(-time.Second)}), "switch off: Ready a second ago, on the first cycle")
	assert.False(t, joined(false, "u", cycle{now, time.Time{}}), "switch off: unknown on the first cycle, taken as established")
	// The switch on: the first cycle is the controller's first sight.
	assert.True(t, joined(true, "p", cycle{now, old}), "Ready two windows ago, first seen on the first cycle: waits a window")
	assert.True(t, joined(true, "u", cycle{now, time.Time{}}), "unknown on the first cycle: waits a window from first sight")
	assert.False(t, joined(true, "p", cycle{now, old}, cycle{window, old}), "the same Ready time a window after the first cycle")

	// After the first cycle, with either setting: the replica watched every
	// cycle for two windows before now (within the tracker's absence
	// tolerance, so no case is a rejoin), Ready since readyAt.
	watched := func(readyAt time.Time, then ...cycle) []cycle {
		var out []cycle
		for at := now.Add(-2 * JoinedReplicaWindow); at.Before(now); at = at.Add(15 * time.Second) {
			out = append(out, cycle{at, readyAt})
		}
		return append(out, then...)
	}
	for _, waits := range []bool{false, true} {
		assert.False(t, joined(waits, "p", watched(old, cycle{now, old})...), "control: the same Ready time, watched two windows")
		assert.True(t, joined(waits, "p", watched(time.Time{}, cycle{now, old})...), "a Ready time two windows old, first seen now: a node clock behind")
		assert.True(t, joined(waits, "p", watched(old, cycle{now, old.Add(time.Second)}, cycle{half, old.Add(time.Second)})...), "a new Ready time half a window later")
		assert.False(t, joined(waits, "p", watched(old, cycle{now, old}, cycle{half, old})...), "control: the same Ready time half a window later")
		assert.False(t, joined(waits, "p", watched(time.Time{}, cycle{now, old}, cycle{window, old})...), "the same Ready time a window after it was first seen")
		assert.True(t, joined(waits, "p", watched(old, cycle{now, old}, cycle{window, window})...), "a new Ready time: back from a restart")
		assert.True(t, joined(waits, "f", watched(old, cycle{now, now.Add(time.Minute)})...), "a Ready time in the future: a node clock ahead")
		assert.True(t, joined(waits, "f", watched(old, cycle{now, now.Add(time.Minute)}, cycle{window, now.Add(time.Minute)})...), "held a window from that Ready time, not from first sight")
	}
}
