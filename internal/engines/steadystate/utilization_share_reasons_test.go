package steadystate

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/record"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// shortGroup is a group where B is short and actionable and A has plenty: B's
// shortfall is what the per-model reasons explain.
func shortGroup() (allocation.ShareGroup, allocation.ShareEvaluation) {
	g := allocation.ShareGroup{
		Budget: 24,
		Roles: []allocation.ShareRole{
			{Key: "ns/A/both", Weight: 1, Need: 2, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
			{Key: "ns/B/both", Weight: 1, Need: 20, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
			{Key: "ns/C/both", Weight: 1, Need: 2, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
		},
		Committed:  map[string]int{"ns/A/both": 14, "ns/B/both": 4, "ns/C/both": 6},
		Thresholds: map[string]float64{"ns/A/both": 0.8, "ns/B/both": 0.8, "ns/C/both": 0.8},
		Origins: map[string]allocation.ShareRoleOrigin{
			"ns/A/both": {Namespace: "ns", ModelID: "A"}, "ns/B/both": {Namespace: "ns", ModelID: "B"},
			"ns/C/both": {Namespace: "ns", ModelID: "C"},
		},
	}
	return g, allocation.EvaluateShare(g.Roles, g.Committed, g.Thresholds, g.Budget, 0.15)
}

// The reasons a short model sees when something other than the quota keeps it
// from growing, each against the state without that cause.
func TestUtilizationShareBlockedReasonsForAHeldBackReceiver(t *testing.T) {
	tm := allocation.ShareTimings{Window: time.Minute, ReleaseTimeout: time.Hour, FillTimeout: time.Hour,
		ReversalHold: 10 * time.Minute, SwingWindow: time.Hour}
	t0 := time.Unix(0, 0)
	g, ev := shortGroup()
	if v := ev.Roles; !slices.ContainsFunc(v, func(r allocation.ShareRoleVerdict) bool {
		return r.Key == "ns/B/both" && r.Actionable && r.Headroom < 0
	}) {
		t.Fatalf("setup: B is not short and actionable: %+v", v)
	}
	reasons := func(l *allocation.ShareLedger, now time.Time) []string {
		return shareBlockedReasons(l, g, ev, nil, now, tm)["ns/B"]
	}

	if got := reasons(allocation.NewShareLedger(), t0); slices.ContainsFunc(got, func(r string) bool {
		return r == constants.ScalingBlockedReversalHold || r == constants.ScalingBlockedTransferLimit ||
			r == constants.ScalingBlockedSwinging || r == constants.ScalingBlockedDonorNotSteerable
	}) {
		t.Fatalf("a fresh ledger holds B back: %v", got)
	}

	t.Run("reversal-hold", func(t *testing.T) {
		l := allocation.NewShareLedger()
		held := map[string]int{"ns/B/both": 4, "ns/C/both": 6}
		gave := l.Start(allocation.ShareTransfer{Donor: "ns/B/both", Receiver: "ns/C/both", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
		l.Cancel(gave.ID, t0, tm)
		if got := reasons(l, t0.Add(time.Minute)); !slices.Contains(got, constants.ScalingBlockedReversalHold) {
			t.Fatalf("B gave a minute ago: want reversal-hold, got %v", got)
		}
		if got := reasons(l, t0.Add(tm.ReversalHold+time.Minute)); slices.Contains(got, constants.ScalingBlockedReversalHold) {
			t.Fatalf("past the hold: got %v", got)
		}
	})

	t.Run("transfer-limit", func(t *testing.T) {
		l := allocation.NewShareLedger()
		held := map[string]int{"ns/A/both": 14, "ns/C/both": 6}
		for range allocation.ShareMaxConcurrentTransfers {
			l.Start(allocation.ShareTransfer{Donor: "ns/A/both", Receiver: "ns/C/both", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
		}
		if got := reasons(l, t0); !slices.Contains(got, constants.ScalingBlockedTransferLimit) {
			t.Fatalf("the group runs its most transfers: want transfer-limit, got %v", got)
		}
	})

	t.Run("swinging", func(t *testing.T) {
		l := allocation.NewShareLedger()
		held := map[string]int{"ns/B/both": 4, "ns/C/both": 6}
		// B gives, receives, gives: two reversals within the swing window.
		for i, pair := range [][2]string{{"ns/B/both", "ns/C/both"}, {"ns/C/both", "ns/B/both"}, {"ns/B/both", "ns/C/both"}} {
			tr := l.Start(allocation.ShareTransfer{Donor: pair[0], Receiver: pair[1], GPUs: 1, DonorGPUs: 1},
				held, t0.Add(time.Duration(i)*time.Second), tm)
			l.Cancel(tr.ID, t0.Add(time.Duration(i)*time.Second), tm)
		}
		if got := reasons(l, t0.Add(time.Minute)); !slices.Contains(got, constants.ScalingBlockedSwinging) {
			t.Fatalf("B reversed twice: want swinging, got %v", got)
		}
	})

	t.Run("donor-not-steerable, not release-timeout", func(t *testing.T) {
		l := allocation.NewShareLedger()
		l.MarkFailed("ns/B/both", t0, tm)
		got := reasons(l, t0)
		if !slices.Contains(got, constants.ScalingBlockedDonorNotSteerable) ||
			slices.Contains(got, constants.ScalingBlockedReleaseTimeout) {
			t.Fatalf("B could not be marked: want donor-not-steerable alone, got %v", got)
		}
	})
}

// The mode gauge says which mode the optimizer is in -- every mode published,
// the one in force at 1 -- and the in-flight gauge counts transfers by state
// while it acts.
func TestUtilizationSharePublishesModeAndInFlight(t *testing.T) {
	r := freshMetrics(t)
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	mode := func() map[string]float64 {
		out := map[string]float64{}
		for _, m := range family(t, r, constants.WVAUtilizationShareMode) {
			out[label(m, constants.LabelMode)] = m.GetGauge().GetValue()
		}
		return out
	}
	se.untilStarted()
	if got := mode(); got[constants.UtilizationShareModeActive] != 1 || got[constants.UtilizationShareModeShadow] != 0 ||
		len(got) != len(constants.UtilizationShareModes) {
		t.Fatalf("acting: mode series %v", got)
	}
	releasing := 0.0
	for _, m := range family(t, r, constants.WVAUtilizationShareInFlight) {
		if label(m, constants.LabelState) == constants.UtilizationShareStateReleasing {
			releasing += m.GetGauge().GetValue()
		}
	}
	if releasing < 1 {
		t.Fatalf("a transfer is releasing, in_flight{state=releasing} = %v", releasing)
	}

	setShadowPolicy(t, se.e.Config, selectedShadow)
	se.cycle()
	if got := mode(); got[constants.UtilizationShareModeShadow] != 1 || got[constants.UtilizationShareModeActive] != 0 {
		t.Fatalf("shadow: mode series %v", got)
	}
	if n := len(family(t, r, constants.WVAUtilizationShareInFlight)); n != 0 {
		t.Fatalf("%d in-flight series published in shadow mode", n)
	}
	setShadowPolicy(t, se.e.Config, "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    tolerance: 7\n")
	se.cycle()
	if got := mode(); got[constants.UtilizationShareModeInvalid] != 1 {
		t.Fatalf("an invalid block: mode series %v", got)
	}
}

// A transfer is recorded as Events on the scale targets it moves, and an Event
// names the other party only within its own namespace.
func TestUtilizationShareRecordsTransferEvents(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	rec := record.NewFakeRecorder(100)
	se.e.Recorder = rec
	se.untilStarted()
	var events []string
	for len(rec.Events) > 0 {
		events = append(events, <-rec.Events)
	}
	giving := slices.ContainsFunc(events, func(e string) bool {
		return strings.Contains(e, constants.K8SEventUtilizationShareGiving) && strings.Contains(e, "model B")
	})
	receiving := slices.ContainsFunc(events, func(e string) bool {
		return strings.Contains(e, constants.K8SEventUtilizationShareReceiving) && strings.Contains(e, "model A")
	})
	if !giving || !receiving {
		t.Fatalf("want a Giving Event on A naming B and a Receiving Event on B naming A, got %q", events)
	}

	g := allocation.ShareGroup{Origins: map[string]allocation.ShareRoleOrigin{
		"x/A/both": {Namespace: "team-x", ModelID: "A"}, "y/B/both": {Namespace: "team-y", ModelID: "B"}}}
	if p := sharePeer(g, "x/A/both", "y/B/both"); strings.Contains(p, "B") || strings.Contains(p, "team-y") {
		t.Fatalf("an Event in team-x names team-y's model: %q", p)
	}
}

// A cluster quota rolls out namespace by namespace: with clusterNamespaces set,
// a model of another namespace is left to today's optimizer -- it neither
// gives nor receives -- while the listed namespaces are planned.
func TestUtilizationShareClusterNamespacesCanary(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	setShadowPolicy(t, se.e.Config, "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    clusterNamespaces: [elsewhere]\n")
	for range 20 {
		o := se.cycle()
		if len(o) != 0 {
			t.Fatalf("a model outside clusterNamespaces was planned: %v", o)
		}
	}
	if m := markedPods(t, c); len(m) != 0 {
		t.Fatalf("%d pods marked outside the canary", len(m))
	}
	setShadowPolicy(t, se.e.Config, "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    clusterNamespaces: [ns]\n")
	se.untilStarted()
}

// The would-rebalance line is logged when the roles a move would fix change,
// not every cycle: a short role nothing can fund would otherwise repeat it
// forever.
func TestUtilizationShareLogsAWouldBeRebalanceOncePerChange(t *testing.T) {
	f := newShareFleet()
	ctx, logs := observe(t)
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	se.ctx = ctx
	setShadowPolicy(t, se.e.Config, selectedShadow)
	for range 5 {
		se.cycle()
	}
	if n := logs.FilterMessage(shadowMessage).Len(); n != 1 {
		t.Fatalf("logged %d times over five unchanged cycles, want once", n)
	}
	f.demand["B"] = 100 // B no longer short: the set changes
	f.demand["C"] = 9000
	se.cycle()
	if n := logs.FilterMessage(shadowMessage).Len(); n != 2 {
		t.Fatalf("a changed set was not logged: %d lines", n)
	}
}
