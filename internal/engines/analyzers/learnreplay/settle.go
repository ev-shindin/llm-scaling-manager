package learnreplay

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// The settle tracker in a replay (analyzer-learner.md, 4.2). The engine
// observes each model's rows at the top of its cycle and records the model
// and the instant (settle.KindObserve); the rows are the ones the model's
// next Analyze event records. The replay applies the observation as the
// engine did: at the recorded instant, with that Analyze's rows.
//
// A recording made before the engine recorded its observations has none.
// There the replay observes at each saturation Analyze's own clock: the
// cycle clock, which is the instant the engine observes at.
//
// No replayed analyzer reads the tracker yet; the replay counts its verdicts
// per reason (2.5), so a golden set that never reaches one says so.

// settlePathPrefix names the settle tracker's per-path counters: the prefix
// and the reason.
const settlePathPrefix = "settle-"

// settlePathNames is the settle tracker's per-path counters, one per reason.
func settlePathNames() []string {
	out := make([]string, 0, len(settle.Reasons()))
	for _, r := range settle.Reasons() {
		out = append(out, settlePathPrefix+string(r))
	}
	return out
}

// requiredSettlePaths is the settle verdicts the committed recordings
// reach, which the goldens must therefore keep reaching. A rollout under a
// Pod's own name occurs in no recording yet: the settle suite
// (internal/signals/settle) covers it until one does.
func requiredSettlePaths() []string {
	return []string{
		settlePathPrefix + string(settle.Joined),
		settlePathPrefix + string(settle.Rejoined),
		settlePathPrefix + string(settle.Restarted),
		settlePathPrefix + string(settle.Lent),
	}
}

// settleReplay is one replay's settle tracker, and the observations the
// recording has made that wait for their rows.
type settleReplay struct {
	tracker *settle.Tracker
	// pending is, per model, the instant of a recorded observation whose
	// Analyze has not come yet.
	pending map[string]time.Time
	// recorded is set once the recording has shown an observation: from
	// then on the replay observes only where the recording did.
	recorded bool
}

func newSettleReplay() *settleReplay {
	return &settleReplay{tracker: settle.NewTracker(), pending: map[string]time.Time{}}
}

// observe notes a recorded observation of a model at now.
func (s *settleReplay) observe(d settle.ObserveEvent, now time.Time) {
	s.recorded = true
	s.pending[utils.GetNamespacedKey(d.Namespace, d.ModelID)] = now
}

// analyze applies the observation waiting for a saturation Analyze's rows,
// or, in a recording with none, observes them at the Analyze's clock.
func (s *settleReplay) analyze(in domain.AnalyzerInput, now time.Time) {
	model := utils.GetNamespacedKey(in.Namespace, in.ModelID)
	at, ok := s.pending[model]
	switch {
	case ok:
		delete(s.pending, model)
	case s.recorded:
		return
	default:
		at = now
	}
	roles := make(map[string]string, len(in.VariantStates))
	for _, vs := range in.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	s.tracker.Observe(model, settlerow.Of(in.ReplicaMetrics, roles), at)
}

// pathCounts is the settle verdicts per reason, by settlePathNames.
func (s *settleReplay) pathCounts() map[string]int64 {
	out := map[string]int64{}
	for r, n := range s.tracker.Verdicts() {
		out[settlePathPrefix+string(r)] = n
	}
	return out
}
