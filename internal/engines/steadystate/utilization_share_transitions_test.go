package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// Switching the optimizer between active, shadow and off drops what only
// actuation holds: the transfer ledgers, the targets it overrode, and the
// GPUs it told wakes and the warm pool to keep away from. Switching it back on
// starts a fresh ledger. Each step against the step before, so an empty
// reading means the switch cleared it, not that nothing was ever there.
func TestUtilizationShareTransitionsDropActuationState(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	if len(se.e.utilizationShare.ledgers) == 0 {
		t.Fatal("setup: active, but no ledger")
	}
	first := se.e.utilizationShare.ledgers
	if o := se.cycle(); len(o) == 0 {
		t.Fatal("setup: active, but no target overridden")
	}
	f.current["A"]-- // a transfer released: GPUs promised to its receiver
	se.cycle()
	if len(decision.LatestSharePromised(se.clock)) == 0 {
		t.Fatal("setup: active with a released transfer, but nothing promised")
	}

	for _, step := range []struct {
		name, policy string
	}{
		{"active to shadow", selectedShadow},
		{"shadow to off", ""},
	} {
		setShadowPolicy(t, se.e.Config, step.policy)
		if o := se.cycle(); o != nil {
			t.Fatalf("%s: targets still overridden: %v", step.name, o)
		}
		if n := len(se.e.utilizationShare.ledgers); n != 0 {
			t.Fatalf("%s: %d ledgers kept", step.name, n)
		}
		if p := decision.LatestSharePromised(se.clock); len(p) != 0 {
			t.Fatalf("%s: GPUs still promised: %v", step.name, p)
		}
	}

	setShadowPolicy(t, se.e.Config, activeShare)
	se.cycle()
	for k, l := range se.e.utilizationShare.ledgers {
		if first[k] == l {
			t.Fatalf("back on: group %s kept its old ledger", k)
		}
	}
	if len(se.e.utilizationShare.ledgers) == 0 {
		t.Fatal("back on: no ledger")
	}
}
