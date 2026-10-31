package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
)

// SwitchK2WaitsAfterRestart off is the behaviour before PR 2: a replica
// already Ready on the controller's first cycle is established unless its
// Ready time is under a window old. On, it waits a window from that cycle
// (TestRestartSuite_AControllerRestartWaitsAWindowOnce).
func TestSettleView_TheRestartSwitchOffTakesTheFirstCycleAsEstablished(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready time.Duration
		want  string
	}{
		{"Ready an hour", time.Hour, p1Obs},
		{"Ready 10 s", 10 * time.Second, k2ReasonObsUnsettled},
	} {
		r := newRestartRig(t, restartT0)
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchK2WaitsAfterRestart: false}))
		got := r.cycle([]domain.ReplicaMetrics{fullRow("p0", "v", restartT0.Add(-tc.ready))}, decodeVariants("v"))
		assert.Equal(t, []string{tc.want}, got, "%s, on the controller's first cycle, the switch off", tc.name)
	}
}

// With SwitchK2ReadsSettle off, k2's joined test is the memo of Ready times
// from before the tracker: it holds a replica across a gap of up to
// K2SettleMaxGap, where the tracker's absence tolerance (1 min at a 15 s
// interval) makes it a new sighting.
func TestSettleView_TheTrackerSwitchMovesOnlyTheAbsenceRule(t *testing.T) {
	run := func(reads bool) []string {
		r := newRestartRig(t, restartT0)
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchK2ReadsSettle: reads}))
		row := fullRow("p0", "v", restartT0.Add(-time.Hour))
		settled(t, r, func() []domain.ReplicaMetrics { return []domain.ReplicaMetrics{row} }, decodeVariants("v"))
		r.now = r.now.Add(75 * time.Second) // unseen for 90 s
		return r.cycle([]domain.ReplicaMetrics{row}, decodeVariants("v"))
	}
	assert.Equal(t, []string{k2ReasonObsUnsettled}, run(true), "on the tracker: unseen 90 s is a new sighting")
	assert.Equal(t, []string{p1Obs}, run(false), "on the memo: unseen 90 s, under K2SettleMaxGap, is the same one")
}

// Given the engine's tracker, the analyzer reads it and observes nothing of
// its own: the rows are observed once, by the engine.
func TestSettleView_AnAnalyzerGivenATrackerObservesNoneOfItsOwn(t *testing.T) {
	r := newRestartRig(t, restartT0)
	r.cycle([]domain.ReplicaMetrics{fullRow("p0", "v", restartT0.Add(-time.Hour))}, decodeVariants("v"))
	assert.Empty(t, r.a.ownSettled, "the analyzer observed its own tracker beside the engine's")
	_, ok := r.a.ownSettle.Replica(settleModel("test-ns", "test-model"), fullRowKey("p0", "v"))
	assert.False(t, ok)
}

// SetSwitchesForReplay knows PR 2's two switches and refuses any other name,
// setting nothing.
func TestSettleView_TheReplaySwitches(t *testing.T) {
	r := newRestartRig(t, restartT0)
	assert.True(t, r.a.k2ReadsSettle && r.a.k2WaitsAfterRestart, "both on from the constructor")
	require.Error(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchK2ReadsSettle: false, "nope": true}))
	assert.True(t, r.a.k2ReadsSettle, "a refused set changes nothing")
	require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchK2ReadsSettle: false, SwitchK2WaitsAfterRestart: false}))
	assert.False(t, r.a.k2ReadsSettle || r.a.k2WaitsAfterRestart)
}

// fullRowKey is the settle tracker's key of a replica of the variant's own.
func fullRowKey(pod, variant string) settle.Key { return settle.Key{Pod: pod, Variant: variant} }
