package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Review probe (round 2, a rejoining pod taken as established). p1's engine
// restarts -- a container restart, or an LWS group restart with stable pod
// names -- so it reports no metrics for 75 s, and comes back handed the
// router's queue while its batch fills. Its Pod became Ready again a few
// seconds ago, and it is as fresh as a new pod: its filling reading (174 327
// tokens, 1.0 req/s, against 340 000 and 6.3 served) is neither k2 nor mu.
//
// The collector does not report the Ready time on this branch
// (replicaReadySince, TODO(#283)); the spec stands in for it with what the
// Pod's Ready condition would say, so it checks the rule, and the
// collector's half lands with #283's ReadySince.
func TestReadySince_ARejoiningPodIsAsFreshAsANewOne(t *testing.T) {
	day := func(clock string) time.Time {
		c, err := time.Parse("15:04:05", clock)
		require.NoError(t, err)
		return time.Date(2026, 10, 23, c.Hour(), c.Minute(), c.Second(), 0, time.UTC)
	}
	// run replays the restart and returns the lowest k2 and mu any window
	// holds afterwards (0 for none). readyKnown reports the Pods' Ready
	// times; otherwise the analyzer has only its own first sight.
	run := func(returning string, readyKnown bool) (k2Low int64, muLow float64) {
		t.Helper()
		a := newE1Analyzer(t, false)
		if readyKnown {
			a.readySince = func(rm domain.ReplicaMetrics) time.Time {
				if rm.PodName == "p0" {
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
		_, lows := replayE1(t, a, cycles)
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

// A Ready time a window old is an established replica, whatever the
// analyzer saw: one first seen on a later cycle is not held back for a
// window when its Pod has been Ready for longer.
func TestReadySince_AKnownOldReadyTimeIsEstablished(t *testing.T) {
	m := newSettleMemo()
	m.pastFirstCycle = true
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	assert.False(t, m.noteReady("v/p", true, now.Add(-2*JoinedReplicaWindow), now), "Ready two windows ago")
	assert.True(t, m.noteReady("v/q", true, now.Add(-time.Second), now), "Ready a second ago")
	assert.True(t, m.noteReady("v/r", true, time.Time{}, now), "unknown: first seen now, after the memo's first cycle")
}
