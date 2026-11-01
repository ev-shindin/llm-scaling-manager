package learnreplay

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
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
// cycle clock, which is the instant the engine observes at, at the default
// optimize interval. A cycle whose collection failed (settle.KindHold) is in
// no such recording, so the fallback cannot hold through one.
//
// The saturation analyzer's k2 reads the replay's tracker (its joined test),
// as it read the engine's. A recording is an engine's: one made by driving
// the analyzer alone gives it a tracker of the caller's, observed as the
// engine does, or its replay reads otherwise from the first cycle (the
// analyzer alone takes its first cycle's replicas as already watched,
// saturation.observeOwnSettle).
//
// A known gap: an observation applies only with the next saturation Analyze
// of its model. A cycle the engine observed and then did not analyze (an
// error between the observation and the Analyze) leaves its observation
// pending, and the next one replaces it, so the replay's tracker misses
// rows the engine's saw, and k2 reads what the replay's holds. None of the
// committed recordings has such a cycle.
//
// The replay counts the tracker's verdicts per reason (2.5), so a golden set
// that never reaches one says so.

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

// requiredSettlePaths is the settle verdicts the goldens must keep
// reaching: a join, a warm-pool lend, and a restart. The pd-restart cluster
// golden restarts a decode engine under its own Pod name, so a restart is
// required from a real one, not only from a generator. The committed
// recordings reach a rejoin too, but only through their synthetic
// generators (clocks that jump), so requiring it would pin the generators,
// not the tracker; a rollout under a Pod's own name is in no recording. The
// settle suite (internal/signals/settle) covers both.
func requiredSettlePaths() []string {
	return []string{
		settlePathPrefix + string(settle.Joined),
		settlePathPrefix + string(settle.Lent),
		settlePathPrefix + string(settle.Restarted),
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
	t := settle.NewTracker()
	t.SetInterval(config.DefaultOptimizationInterval)
	return &settleReplay{tracker: t, pending: map[string]time.Time{}}
}

// observe notes a recorded observation of a model at now.
func (s *settleReplay) observe(d settle.ObserveEvent, now time.Time) {
	s.recorded = true
	s.tracker.SetInterval(d.Interval)
	s.pending[utils.GetNamespacedKey(d.Namespace, d.ModelID)] = now
}

// hold applies a recorded hold of a model at now.
func (s *settleReplay) hold(d settle.ObserveEvent, now time.Time) {
	s.recorded = true
	s.tracker.SetInterval(d.Interval)
	s.tracker.Hold(utils.GetNamespacedKey(d.Namespace, d.ModelID), now)
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
