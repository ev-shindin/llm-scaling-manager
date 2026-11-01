package saturation

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
)

// A replica's mu waits a rate window from when its load returned (#392).
//
// The joined test (k2Unsettled) ages a replica from when its Pod became
// Ready. A replica Ready for longer than a rate window, but routed to only
// partway through the minute -- an idle fleet hit by a burst, or a release of
// the scheduler's flow-control queue -- passes it, and its first saturated
// rate[1m] covers only the part of the minute it served. Served for half the
// minute or more, the reading is at least half the derived mu and passes
// muChoice.minMeasuredOfDerived, so it is recorded low: by up to about 25 %
// across two samples on the property case first-sample/mu/joiner-rate-
// timebase (51 of 100 seeds admitted such a reading). A low mu over-orders:
// money, not SLO.
//
// So a reading is not a sample while the replica's load returned, after an
// idle run, under JoinedReplicaWindow ago. The settle tracker reads both
// (settle.Row.Activity): an idle row is positive evidence of no work over the
// minute before it -- no KV held at the minute's peak and no token generated
// (a prefill replica: no request completed) -- and the return is the first
// row of load after it (settle.Replica.ReturnedAfter). The return's own
// minute may hold the first of the work, so the window counts from the
// return, not from the idle row before it. A replica the tracker read no idle
// row of is unchanged: the joined test is the only one that ages it.
//
// The k2 reading is not refused: max_over_time[1m] of a batch that has filled
// is what the replica holds, and k2's own settle tests judge it.

// SwitchMuReturnWaits is the replay switch for the wait (#392): on from the
// constructor; a replay turns it off (SetSwitchesForReplay), and a replica
// whose load returned is admitted as before, on the joined test alone.
const SwitchMuReturnWaits = "mu-waits-a-window-after-load-returns"

// refusedReturned is throughputRefusal's sibling reason for a reading from a
// replica whose load returned under a rate window ago (muReturnedUnderWindow).
const refusedReturned = "its load returned after an idle run under a rate window ago: the rate window still holds part of the idle minute, so the rate reads low"

// muReturnedUnderWindow reports whether rm's load returned, after at least
// one idle row, under JoinedReplicaWindow before now, as the settle tracker
// read it.
func (a *SaturationAnalyzer) muReturnedUnderWindow(namespace, modelID string, rm domain.ReplicaMetrics, now time.Time) bool {
	if !a.muReturnWaits {
		return false
	}
	r, ok := a.settleView().Replica(settleModel(namespace, modelID), settlerow.Key(rm))
	if !ok {
		return false
	}
	at, ok := r.ReturnedAfter(1)
	return ok && now.Sub(at) < JoinedReplicaWindow
}
