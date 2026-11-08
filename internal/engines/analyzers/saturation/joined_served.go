package saturation

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
)

// SwitchJoinedFromFirstServed is the replay switch for the clock that also
// starts at a replica's first return of load (#392); on from the constructor.
// A replay turns it off (SetSwitchesForReplay), and the joined test ages a
// replica from its Ready time and first sight alone, as before.
//
// The joined test (k2Unsettled) aged a replica from when its Pod became Ready,
// or from when the controller first saw it. A replica Ready for longer than a
// rate window but routed to only partway through a minute -- an idle fleet
// hit by a burst, or a release of the scheduler's flow-control queue -- passed
// it, and its first saturated rate[1m] covers only the part of the minute it
// served. That reading went into mu's window as a service rate. On the
// property case first-sample/mu/joiner-rate-timebase, a replica routed to 10 s
// before a cycle recorded 0.0377 req/s against a truth of 0.2055 (seed 1), and
// three minutes on its mu still read 0.1918. With an ITL line, a reading that
// served 30-50 s of the minute passes muChoice.minMeasuredOfDerived
// (first-sample/mu/joiner-rate-timebase-line). A low mu over-orders: money,
// not SLO.
//
// k2 is refused with mu, not mu alone. Mu is written only beside a k2 write,
// and k2's settle rate test reads the mu on record: refusing mu alone left
// that test with no mu, and the k2 readings it admitted then wrote ramp
// readings as mu (f32a32ed7). The cost is valid k2 readings: a
// max_over_time[1m] KV peak is not lowered by the idle part of the minute.
// On the e1 golden, model-02's variant-01 saturates only in the minute after
// its first return (holding 363 871 tokens), so it never records an observed
// k2 and prices from the derived figure; its capacity does not move. In time,
// a late-routed replica records its first k2 and mu a rate window after its
// first served cycle (4 cycles at 15 s), and k2's first reading of a fresh
// window waits one more (k2_confirm.go).
const SwitchJoinedFromFirstServed = "joined-clock-starts-at-first-served"

// k2UnsettledRoutedLate is the settle test's reason for a saturated reading
// from a replica whose load first returned, since its settle began, under a
// rate window ago (SwitchJoinedFromFirstServed).
const k2UnsettledRoutedLate = "the replica's load first returned after an idle run under a rate window ago: its one-minute rates still hold the idle part of the minute before it was routed to; not recorded"

// firstReturnUnderWindow reports whether the replica in is Ready and the
// first return of load after an idle run in its current settle is under
// JoinedReplicaWindow before now, as the settle tracker reads it.
//
// The tracker reads activity by role (settle.Row.Activity): a row is idle
// only with no KV held at the minute's peak and no token generated (a prefill
// replica: no request completed), so a full, queued replica with no
// completion yet is unknown, not idle. A return is the first row of load
// after an idle run (settle.Replica.Returns), and the tracker keeps the
// returns of the current settle only: one that began at the Pod's Ready time,
// a restart, a rejoin, a rollout or a lend. The routing began at most one
// cycle before the return's row, so a window from that row holds no idle
// second.
//
// Only the FIRST return of a settle holds: a replica is protected once per
// settle, so one that idles every night and is hit by a morning burst is
// held on its first morning and not after. Every return leaves idle seconds
// in the window that follows it, so the narrower rule -- hold a return whose
// idle run reaches into the reading's window -- is the rule that holds every
// return. Measured on the pd-restart golden, where decode Pods idle and
// return as the load steps: holding every return left decode's mu unpriced
// from 12:27:02 to 12:28:32, then 5.06 and 4.65 req/s until 12:46:50 against
// the 6.21 the first-return rule prices (-25 %), and its k2 at 48 005
// against 55 212; it also moved e1-tuned and ttft-staircase-b-off. The
// first-return rule holds one of the two decode Pods there (pod-01; pod-07
// had returned at 12:13:44) and moves decode's mu toward its steady figure.
// The tracker keeps the last
// eight returns; past eight, the first kept is minutes old, as each return
// needs an idle row before it.
func (a *SaturationAnalyzer) firstReturnUnderWindow(in k2SettleInput, now time.Time) bool {
	if !a.joinedFromFirstServed || !in.ready {
		return false
	}
	key := settlerow.Key(domain.ReplicaMetrics{PodName: in.pod, VariantName: in.variant, FromWarmPool: in.bridge})
	r, ok := a.settleView().Replica(settleModel(in.namespace, in.modelID), key)
	if !ok {
		return false
	}
	at, ok := firstReturn(r)
	return ok && now.Sub(at) < JoinedReplicaWindow
}

// firstReturn is the first return of load after an idle run in r's current
// settle, and whether there is one.
func firstReturn(r settle.Replica) (time.Time, bool) {
	if len(r.Returns) == 0 {
		return time.Time{}, false
	}
	return r.Returns[0].At, true
}
