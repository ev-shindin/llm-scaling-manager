package steadystate

import (
	"testing"
	"time"
)

// A model allowed to scale to zero, idle at one replica, is never left at zero
// by a transfer: parking is scale-to-zero's decision, with its retention, not
// the optimizer's.
func TestUtilizationShareNeverGivesTheLastReplica(t *testing.T) {
	f := newShareFleet()
	f.current["C"], f.demand["C"] = 1, 0
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	zero := 0
	for range 40 {
		se.clock = se.clock.Add(30 * time.Second)
		reqs := f.requests()
		for i := range reqs {
			if reqs[i].ModelID == "C" {
				reqs[i].VariantStates[0].MinReplicas = &zero
			}
		}
		o := se.e.evaluateUtilizationShare(se.ctx, reqs, fullQuota(), f.scaleTargets())
		if got, ok := o["ns/C-v"]; ok && got.Target < 1 {
			t.Fatalf("C's last replica given away: target %d", got.Target)
		}
	}
}

// A ledger that has just started is quiet for a fill timeout -- it plans
// nothing -- but it does not freeze the models: with no transfer restored,
// every variant is left to today's optimizer meanwhile.
func TestUtilizationShareQuietPeriodFreezesNothingItDidNotRestore(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	o := se.cycle()
	if len(se.e.utilizationShare.ledgers) != 1 {
		t.Fatal("setup: the ledger did not start")
	}
	if len(o) != 0 {
		t.Fatalf("the quiet period pinned %d variants no transfer moves: %v", len(o), o)
	}
}

// targetOf is the target a cycle leaves a fixture variant at: its override,
// or, with none, the count the share fleet runs -- the variant is left to
// today's optimizer, which these fixtures do not run.
func targetOf(o map[string]utilizationShareOverride, key string) int {
	if v, ok := o[key]; ok {
		return v.Target
	}
	return map[string]int{"ns/A-v": 9, "ns/B-v": 5, "ns/C-v": 2}[key]
}
