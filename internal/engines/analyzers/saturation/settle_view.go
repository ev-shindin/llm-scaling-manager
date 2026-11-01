package saturation

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// k2 reads when each replica joined from the settle tracker
// (internal/signals/settle, analyzer-learner.md 4.2): the engine writes it
// once per model per cycle, before any analyzer runs, and the analyzer reads
// it through a read-only view (WithSettleView). It writes nothing to it.
//
// An analyzer given no view (a test that drives Analyze without an engine)
// keeps a tracker of its own and observes each Analyze's rows into it at the
// top of the call, at the cycle's clock: the instant the engine observes at.
// One of the two observes, never both.

// SwitchK2ReadsSettle is the replay switch for k2's joined test on the settle
// tracker (analyzer-learner.md, 2.5 and section 7, PR 2). On, the test reads
// the engine's tracker (k2Joined). Off, it reads the analyzer's own memo of
// each Ready time (settleMemo.noteReady), as before the tracker. The rule is
// the same (#288); two things move. How long a replica may go without a row
// before it is a new sighting: the tracker's absence tolerance
// (settle.Tolerance: 1 min, or 2.5 optimize intervals), not the memo's
// K2SettleMaxGap (2 min). And a Ready row whose Ready time reads zero (the
// collector missed the Pod): the tracker takes it as unknown and keeps the
// settle, where the memo took it, and the old Ready time coming back, as two
// new sightings, and the replica waited again.
const SwitchK2ReadsSettle = "k2-reads-the-settle-tracker"

// SwitchK2WaitsAfterRestart is the replay switch for k2's wait after a
// controller restart (analyzer-learner.md, 2.5 and section 7, PR 2). On, a
// replica first seen on the analyzer's first cycle for its model waits a
// window from that cycle, as one first seen later does: the controller's
// first sight is the later of the two times the joined test reads. Off, as
// before the settle tracker, a replica already Ready on the first cycle is
// taken as established unless its Pod's Ready time is under a window old.
const SwitchK2WaitsAfterRestart = "k2-waits-after-controller-restart"

// WithSettleView sets the settle tracker k2's joined test reads: the
// engine's, which it observes at the top of every cycle. Call it before the
// analyzer is used. Without it the analyzer observes a tracker of its own.
func (a *SaturationAnalyzer) WithSettleView(v settle.View) *SaturationAnalyzer {
	a.settle = v
	return a
}

// ownSettleInterval is the interval the analyzer's own tracker is judged at
// until WithOptimizationInterval sets the controller's: one at which a
// replica keeps its sighting across a gap of K2SettleMaxGap
// (settle.Tolerance is 2.5 intervals), as the memo of Ready times before the
// tracker did. The engine's tracker is set to the controller's interval
// instead.
const ownSettleInterval = K2SettleMaxGap * 2 / 5

// newOwnSettle is the analyzer's own tracker, at ownSettleInterval.
func newOwnSettle() *settle.Tracker {
	t := settle.NewTracker()
	t.SetInterval(ownSettleInterval)
	return t
}

// settleView is the tracker the analyzer reads: the engine's when it was
// given one, else its own.
func (a *SaturationAnalyzer) settleView() settle.View {
	if a.settle != nil {
		return a.settle
	}
	return a.ownSettle
}

// observeOwnSettle observes input's rows into the analyzer's own tracker at
// now, when the engine gave it none. The Ready time each row carries is read
// through the readySince seam, which tests stub.
//
// The analyzer alone stands for a controller that has watched the fleet for
// as long as the caller says, not for one that has just started: the first
// time it observes a model, it observes the same rows a window earlier too,
// so a replica on its first cycle is aged from its Ready time alone, as
// before the tracker. A controller restart is the engine's tracker's first
// cycle (SwitchK2WaitsAfterRestart); a test of it gives the analyzer a
// tracker of its own making (WithSettleView).
func (a *SaturationAnalyzer) observeOwnSettle(input domain.AnalyzerInput, now time.Time) {
	if a.settle != nil {
		return
	}
	roles := make(map[string]string, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	rows := settlerow.Of(input.ReplicaMetrics, roles)
	for i := range rows {
		rows[i].ReadySince = a.readySince(input.ReplicaMetrics[i])
	}
	model := settleModel(input.Namespace, input.ModelID)
	a.mu.Lock()
	// Keyed namespace|model, as the settle memo the sweep reads
	// (sweepLearnerMapsLocked).
	first := !a.ownSettled[input.Namespace+"|"+input.ModelID]
	a.ownSettled[input.Namespace+"|"+input.ModelID] = true
	a.mu.Unlock()
	if first {
		a.ownSettle.Observe(model, rows, now.Add(-JoinedReplicaWindow))
	}
	a.ownSettle.Observe(model, rows, now)
}

// settleModel is the settle tracker's name for a model: the engine's
// namespaced key.
func settleModel(namespace, modelID string) string {
	return utils.GetNamespacedKey(namespace, modelID)
}

// k2Joined reports whether the replica in has joined the fleet under
// JoinedReplicaWindow ago, and whether its Pod's Ready time decided that
// (byReadyAt). The rule is #288's, read from the settle tracker:
//
//   - The replica is aged from the later of its Pod's Ready time and the
//     first cycle that Ready time was seen (settle.Replica.ReadyFrom). A
//     node clock behind the controller's dates the Ready time into the
//     past, and a warm-pool lend keeps the Ready time the Pod had in the
//     pool; the first sight covers both. A new Ready time under the same
//     name -- an engine restart, or an LWS group recreated with its pod
//     names kept -- is seen anew, so the replica waits again.
//   - With no Ready time known, from the tracker's first sight of the
//     replica.
//   - A row that is not Ready since then ends the sighting: the replica is
//     aged from its first Ready row after the last one that was not. A Pod
//     that left the rotation and came back with no Ready time to say so
//     has restarted as far as anything can tell.
//
// With SwitchK2WaitsAfterRestart off, a first sight on the analyzer's first
// cycle for the model does not count, as before the tracker: a controller
// restart then holds only the replicas whose Ready time is under a window
// old.
//
// A replica the tracker does not know -- never observed -- is first seen
// now, with the Ready time its row carries. The engine observes every row
// before the analyzer runs, so only a caller that skips the observation
// meets one. A row that is not Ready this cycle has not joined: nothing
// routes to it,
// and the admission refuses it on its own (P1-obs-unadmitted).
//
// readyKnown reports whether this cycle's row carried a Ready time, for the
// per-path counter (PathRowsReadySince), and since is when the replica's
// wait began, zero when it has none to wait.
func (a *SaturationAnalyzer) k2Joined(in k2SettleInput, firstCycle, now time.Time) (joined, byReadyAt, readyKnown bool, since time.Time) {
	view := a.settleView()
	model := settleModel(in.namespace, in.modelID)
	// The engine's spelling of the key, so the two cannot drift apart.
	key := settlerow.Key(domain.ReplicaMetrics{PodName: in.pod, VariantName: in.variant, FromWarmPool: in.bridge})
	trace := view.Trace(model, key)
	if n := len(trace); n > 0 && trace[n-1].At.Equal(now) {
		readyKnown = !trace[n-1].Row.ReadySince.IsZero()
	}
	if !in.ready {
		return false, false, readyKnown, time.Time{}
	}
	r, ok := view.Replica(model, key)
	if !ok {
		// Never observed: first seen now, or, by the analyzer alone, a
		// window ago, as observeOwnSettle backdates a model's first rows.
		seen := now
		if a.settle == nil {
			seen = now.Add(-JoinedReplicaWindow)
		}
		r = settle.Replica{Key: key, FirstSeen: seen, ReadySince: in.readyAt}
		if !in.readyAt.IsZero() {
			r.ReadySeen = seen
		}
	}
	readyAt, first := r.ReadySince, r.ReadySeen
	if readyAt.IsZero() {
		first = r.FirstSeen
	}
	if back := readyAgain(trace); back.After(first) {
		first = back
	}
	if !a.k2WaitsAfterRestart && !first.After(firstCycle) {
		first = time.Time{}
	}
	since = first
	if readyAt.After(since) {
		since = readyAt
	}
	joined = !since.IsZero() && now.Sub(since) < JoinedReplicaWindow
	// The Ready time decided the join when it is the later of the two, or
	// when a new one under a name already seen is what began the sighting.
	restarted := !readyAt.IsZero() && r.ReadySeen.After(r.FirstSeen)
	return joined, joined && !readyAt.IsZero() && (since.Equal(readyAt) || restarted), readyKnown, since
}

// readyAgain is the first Ready row after the last row of trace that was not
// Ready, zero when every row is Ready or none after it is.
func readyAgain(trace []settle.Sample) time.Time {
	for i := len(trace) - 1; i >= 0; i-- {
		if trace[i].Row.Ready {
			continue
		}
		if i+1 < len(trace) {
			return trace[i+1].At
		}
		return time.Time{}
	}
	return time.Time{}
}
