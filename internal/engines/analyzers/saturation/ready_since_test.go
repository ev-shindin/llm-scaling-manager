package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
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

// A Ready time a window old is an established replica when the memo has seen
// it that long, or when it was already Ready on the memo's first cycle. A
// Ready time first seen now is held for a window from now, however old it
// reads: the Ready time is the node's clock, and a node behind the
// controller's dates a replica that has just joined into the past (review:
// one 60 s behind skipped the window, and its filling reading became k2).
func TestReadySince_AKnownOldReadyTimeIsEstablished(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	old := now.Add(-2 * JoinedReplicaWindow)

	first := newSettleMemo()
	assert.False(t, joinedOf(first.noteReady("v/p", true, old, now)), "Ready two windows ago, on the memo's first cycle")
	assert.True(t, joinedOf(first.noteReady("v/q", true, now.Add(-time.Second), now)), "Ready a second ago, on the memo's first cycle")
	assert.False(t, joinedOf(first.noteReady("v/u", true, time.Time{}, now)), "unknown on the memo's first cycle: taken as established")

	m := newSettleMemo()
	m.pastFirstCycle = true
	assert.True(t, joinedOf(m.noteReady("v/p", true, old, now)), "a Ready time two windows old, first seen now: a node clock behind")
	assert.True(t, joinedOf(m.noteReady("v/p", true, old, now.Add(JoinedReplicaWindow/2))), "the same Ready time half a window later")
	assert.False(t, joinedOf(m.noteReady("v/p", true, old, now.Add(JoinedReplicaWindow))), "the same Ready time a window after it was first seen")
	assert.True(t, joinedOf(m.noteReady("v/p", true, now.Add(JoinedReplicaWindow), now.Add(JoinedReplicaWindow))), "a new Ready time: back from a restart")
	assert.True(t, joinedOf(m.noteReady("v/f", true, now.Add(time.Minute), now)), "a Ready time in the future: a node clock ahead")
	assert.True(t, joinedOf(m.noteReady("v/f", true, now.Add(time.Minute), now.Add(JoinedReplicaWindow))), "held a window from that Ready time, not from first sight")
	assert.True(t, joinedOf(m.noteReady("v/r", true, time.Time{}, now)), "unknown: first seen now, after the memo's first cycle")
}

// joinedOf is noteReady's first result.
func joinedOf(joined, _ bool) bool { return joined }
