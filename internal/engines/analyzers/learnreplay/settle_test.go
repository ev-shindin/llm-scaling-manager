package learnreplay

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
)

// A replay observes the settle tracker where the engine did: at the recorded
// instant, with the rows of the model's next saturation Analyze. A recording
// made before the engine recorded its observations is observed at each
// saturation Analyze's own clock; one that recorded them is observed only
// where it did.
func TestSettleReplay_ObservesWhereTheRecordingDid(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m",
		ReplicaMetrics: []domain.ReplicaMetrics{{PodName: "p", VariantName: "v", Ready: true}}}
	key := settle.Key{Pod: "p", Variant: "v"}

	old := newSettleReplay()
	old.analyze(in, at)
	if r, ok := old.tracker.Replica("ns/m", key); !ok || !r.FirstSeen.Equal(at) {
		t.Errorf("a recording with no observation: %+v %v, want the replica first seen at the Analyze's %v", r, ok, at)
	}

	rec := newSettleReplay()
	rec.observe(settle.ObserveEvent{Namespace: "ns", ModelID: "m"}, at.Add(-time.Second))
	rec.analyze(in, at)
	if r, ok := rec.tracker.Replica("ns/m", key); !ok || !r.FirstSeen.Equal(at.Add(-time.Second)) {
		t.Errorf("a recorded observation: %+v %v, want the replica first seen at the observation's %v", r, ok, at.Add(-time.Second))
	}
	other := in
	other.ModelID = "other"
	rec.analyze(other, at)
	if _, ok := rec.tracker.Replica("ns/other", key); ok {
		t.Error("an Analyze the recording observed nothing before was observed")
	}
	assert.Equal(t, int64(1), rec.pathCounts()[settlePathPrefix+string(settle.Joined)])
}

// The required settle paths are settle paths, and every settle verdict has a
// counter.
func TestSettleReplay_RequiredPathsAreCounted(t *testing.T) {
	counted := map[string]bool{}
	for p := range newSettleReplay().pathCounts() {
		counted[p] = true
	}
	assert.ElementsMatch(t, settlePathNames(), slices.Collect(maps.Keys(counted)))
	for _, p := range requiredSettlePaths() {
		assert.True(t, counted[p], "%s is required and not counted", p)
	}
}

// Review r2 (S2): a recorded hold is applied as the engine applied it. Five
// failed 15 s cycles between two observations are a 90 s gap, past the
// minute's tolerance; held, the replica keeps its join.
func TestSettleReplay_AppliesARecordedHold(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m",
		ReplicaMetrics: []domain.ReplicaMetrics{{PodName: "p", VariantName: "v", Ready: true}}}
	ev := settle.ObserveEvent{Namespace: "ns", ModelID: "m", Interval: 15 * time.Second}
	key := settle.Key{Pod: "p", Variant: "v"}
	for _, held := range []bool{true, false} {
		s := newSettleReplay()
		s.observe(ev, at)
		s.analyze(in, at)
		if held {
			for i := 1; i <= 5; i++ {
				s.hold(ev, at.Add(time.Duration(i)*15*time.Second))
			}
		}
		s.observe(ev, at.Add(90*time.Second))
		s.analyze(in, at.Add(90*time.Second))
		r, ok := s.tracker.Replica("ns/m", key)
		want := settle.Rejoined
		if held {
			want = settle.Joined
		}
		if !ok || r.Reason != want {
			t.Errorf("held %v: settle %s, want %s", held, r.Reason, want)
		}
	}
}
