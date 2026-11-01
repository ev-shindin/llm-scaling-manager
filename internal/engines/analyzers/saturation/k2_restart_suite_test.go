package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The restart suite (analyzer-learner.md, section 7, PR 2): k2's joined test
// on the settle tracker, through Analyze, with the tracker observed as the
// engine observes it (restartRig). A replica waits a rate window
// (JoinedReplicaWindow) from the later of its Pod's Ready time and the
// controller's first sight of that Ready time; while it waits, a saturated
// reading is refused as P1-obs-unsettled and writes nothing.

var restartT0 = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// fullRow is one decode replica full and queued -- a reading k2 records on
// sight once the replica has settled -- Ready since readySince.
func fullRow(pod, variant string, readySince time.Time) domain.ReplicaMetrics {
	rm := makeReplicaMetrics(pod, variant, 260_000, 376_480, 160, 1000, 500)
	rm.GenerationTokenRate = 3000
	rm.RequestRate = 6
	rm.ReadySince = readySince
	return rm
}

// decodeVariants is one decode variant state per name, one replica each.
func decodeVariants(names ...string) []domain.VariantReplicaState {
	out := make([]domain.VariantReplicaState, 0, len(names))
	for _, n := range names {
		out = append(out, domain.VariantReplicaState{VariantName: n, AcceleratorName: "H200", CurrentReplicas: 1,
			GPUsPerReplica: 1, Role: domain.RoleDecode})
	}
	return out
}

// waitWindow is the cycles a replica waits: refused on each cycle under a
// window from its start, and admitted on the cycle a window on.
var waitWindow = int(JoinedReplicaWindow / restartCycle)

// The controller restarts: every replica, however long it has been Ready,
// is first seen now, and waits a window once. With the snapshot off no k2 is
// on record, so the fleet prices at k1 for that minute (the SLO side the
// proposal names, 4.4). Fails on the parent, which took a replica already
// Ready on its first cycle as established.
func TestRestartSuite_AControllerRestartWaitsAWindowOnce(t *testing.T) {
	r := newRestartRig(t, restartT0)
	row := fullRow("p0", "v", restartT0.Add(-time.Hour))
	got := make([]string, 0, waitWindow+2)
	for range waitWindow + 2 {
		got = append(got, r.cycle([]domain.ReplicaMetrics{row}, decodeVariants("v"))...)
	}
	want := make([]string, 0, waitWindow+2)
	for range waitWindow {
		want = append(want, k2ReasonObsUnsettled)
	}
	want = append(want, p1Obs, p1Obs)
	assert.Equal(t, want, got, "a replica Ready an hour waits a window from the controller's first sight of it")
}

// A pod restarts under its own name: the engine's container restarts, the
// Pod leaves the rotation for two cycles and comes back with a new Ready
// time. It waits a window from the first sight of that Ready time, as #288
// made it, and is admitted after.
func TestRestartSuite_ARestartUnderTheSameNameWaitsAgain(t *testing.T) {
	r := newRestartRig(t, restartT0)
	ready := restartT0.Add(-time.Hour)
	settled(t, r, func() []domain.ReplicaMetrics { return []domain.ReplicaMetrics{fullRow("p0", "v", ready)} }, decodeVariants("v"))

	down := fullRow("p0", "v", time.Time{})
	down.Ready = false
	for range 2 {
		r.cycle([]domain.ReplicaMetrics{down}, decodeVariants("v"))
	}
	back := r.now.Add(-5 * time.Second)
	assert.Equal(t, waitThenAdmit(), runCycles(r, waitWindow+1, func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{fullRow("p0", "v", back)}
	}, decodeVariants("v")), "back under the same name with a new Ready time")
}

// An LWS group restart (#361: the workload patch recreates the leader and its
// workers, names kept). The collector reports one row per group, the
// leader's, with the latest Ready time over the leader and its Ready workers
// (attribute.go drops a worker's row; TestLWSGroupReadyTimeIsTheLatestOfItsPods
// pins it), so k2 sees one Ready time per group, here the row's own. The
// restarted group's leader waits a window from the later of its own Ready
// time and the first sight of it; a node clock ahead holds it from its Ready
// time. The group the patch has not reached yet keeps its Ready time and
// shows no wait: the negative control, a group whose Ready time the restart
// does not change.
func TestRestartSuite_AnLWSGroupRestartWithMixedReadyTimes(t *testing.T) {
	r := newRestartRig(t, restartT0)
	old := restartT0.Add(-time.Hour)
	states := decodeVariants("lws")
	states[0].CurrentReplicas = 3
	leaders := func(g0, g1, g2 time.Time, ready0, ready2 bool) []domain.ReplicaMetrics {
		a, b, c := fullRow("lws-0", "lws", g0), fullRow("lws-1", "lws", g1), fullRow("lws-2", "lws", g2)
		a.Ready, c.Ready = ready0, ready2
		return []domain.ReplicaMetrics{a, b, c}
	}
	settled(t, r, func() []domain.ReplicaMetrics { return leaders(old, old, old, true, true) }, states)

	// The patch recreates groups 0 and 2. Their leaders read not Ready
	// while their workers come up (worker Ready times: group 0's at +10 s
	// and +40 s; group 2's at +5 s), then Ready: group 0's leader 20 s
	// before the controller sees it Ready, group 2's 30 s after, by a node
	// clock ahead of the controller's. Group 1 is not reached.
	r.cycle(leaders(time.Time{}, old, time.Time{}, false, false), states)
	r.cycle(leaders(time.Time{}, old, time.Time{}, false, false), states)
	seen := r.now
	g0, g2 := seen.Add(-20*time.Second), seen.Add(30*time.Second)
	got := make([][]string, 0, waitWindow+3)
	for range waitWindow + 3 {
		got = append(got, r.cycle(leaders(g0, old, g2, true, true), states))
	}
	for i, d := range got {
		at := time.Duration(i) * restartCycle
		require.Len(t, d, 3, "one decision per leader at +%v", at)
		assert.Equal(t, verdictAt(seen.Add(at), seen), d[0], "group 0, Ready 20 s before first seen, at +%v: waits from its first sight", at)
		assert.Equal(t, p1Obs, d[1], "group 1, its Ready time unchanged, at +%v: no wait", at)
		assert.Equal(t, verdictAt(seen.Add(at), g2), d[2], "group 2, Ready 30 s after first seen, at +%v: waits from its Ready time", at)
	}
}

// A warm-pool Pod lent to the variant keeps the Ready time it had in the
// pool, hours old. Its rows are a bridge's: k2 records no reading from one,
// whatever its age (P1-obs-unadmitted), so the lend writes nothing, and the
// variant's own replica, already settled, is not moved by it. Admission
// refuses a bridge before the settle test is read, so the lend half guards
// admission, not the tracker's lend event (that is the settle suite's,
// internal/signals/settle). What reaches the tracker is its key: the same
// Pod in a row of the variant's own (the bridge flag off) is a new key, first
// seen now, so it waits a window although its Ready time is hours old. The
// memo before the tracker keyed by variant and pod alone, and took it as the
// replica it had seen lent.
func TestRestartSuite_ABridgeLendWritesNothing(t *testing.T) {
	r := newRestartRig(t, restartT0)
	own := func() domain.ReplicaMetrics { return fullRow("own-0", "v", restartT0.Add(-time.Hour)) }
	settled(t, r, func() []domain.ReplicaMetrics { return []domain.ReplicaMetrics{own()} }, decodeVariants("v"))
	states := decodeVariants("v")
	states[0].CurrentReplicas = 2
	for i := range waitWindow + 2 {
		lent := fullRow("pool-0", "v", restartT0.Add(-3*time.Hour))
		lent.FromWarmPool = true
		lent.LentSince = restartT0
		d := r.cycle([]domain.ReplicaMetrics{own(), lent}, states)
		require.Len(t, d, 2)
		assert.Equal(t, p1Obs, d[0], "the variant's own replica, cycle %d of the lend", i)
		assert.Equal(t, k2ReasonObsUnadmitted, d[1], "the bridge, cycle %d of the lend", i)
	}
	for i := range waitWindow + 1 {
		d := r.cycle([]domain.ReplicaMetrics{own(), fullRow("pool-0", "v", restartT0.Add(-3*time.Hour))}, states)
		require.Len(t, d, 2)
		assert.Equal(t, waitThenAdmit()[i], d[1], "the Pod in a row of the variant's own, cycle %d", i)
	}
}

// A node clock 90 s behind the controller's dates a replica that has just
// become Ready 90 s into the controller's past. It still waits a window,
// from the controller's first sight of that Ready time (#288).
func TestRestartSuite_ANodeClockBehindStillWaitsAWindow(t *testing.T) {
	r := newRestartRig(t, restartT0)
	old := restartT0.Add(-time.Hour)
	settled(t, r, func() []domain.ReplicaMetrics { return []domain.ReplicaMetrics{fullRow("p0", "v", old)} }, decodeVariants("v"))
	joined := r.now.Add(-90 * time.Second)
	states := decodeVariants("v")
	states[0].CurrentReplicas = 2
	for i := range waitWindow + 1 {
		d := r.cycle([]domain.ReplicaMetrics{fullRow("p0", "v", old), fullRow("p1", "v", joined)}, states)
		require.Len(t, d, 2)
		assert.Equal(t, p1Obs, d[0], "the settled replica, cycle %d", i)
		assert.Equal(t, waitThenAdmit()[i], d[1], "the new replica, its Ready time 90 s old by a clock behind, cycle %d", i)
	}
}

// settled runs a window of cycles, and one more, over rows: past the wait a
// controller start begins with, so every replica in them has settled.
func settled(t *testing.T, r *restartRig, rows func() []domain.ReplicaMetrics, states []domain.VariantReplicaState) {
	t.Helper()
	var last []string
	for range waitWindow + 1 {
		last = r.cycle(rows(), states)
	}
	for _, p := range last {
		require.Equal(t, p1Obs, p, "premise: settled after a window of cycles")
	}
}

// runCycles runs n cycles over rows and returns the first decision of each.
func runCycles(r *restartRig, n int, rows func() []domain.ReplicaMetrics, states []domain.VariantReplicaState) []string {
	out := make([]string, 0, n)
	for range n {
		d := r.cycle(rows(), states)
		require.NotEmpty(r.t, d)
		out = append(out, d[0])
	}
	return out
}

// waitThenAdmit is a window of refusals, then an admission.
func waitThenAdmit() []string {
	out := make([]string, 0, waitWindow+1)
	for range waitWindow {
		out = append(out, k2ReasonObsUnsettled)
	}
	return append(out, p1Obs)
}

// verdictAt is what k2 decides at now for a replica that started to wait at
// since: refused under a window, admitted from it.
func verdictAt(now, since time.Time) string {
	if now.Sub(since) < JoinedReplicaWindow {
		return k2ReasonObsUnsettled
	}
	return p1Obs
}
