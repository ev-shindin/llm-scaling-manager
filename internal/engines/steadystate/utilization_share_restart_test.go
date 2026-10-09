package steadystate

import (
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// reasonModels are the models with reason set on wva_model_scaling_blocked.
func reasonModels(t *testing.T, r *prometheus.Registry, reason string) []string {
	t.Helper()
	var models []string
	for _, m := range family(t, r, constants.WVAModelScalingBlocked) {
		if label(m, constants.LabelReason) == reason && m.GetGauge().GetValue() == 1 {
			models = append(models, label(m, constants.LabelModelName))
		}
	}
	slices.Sort(models)
	return models
}

// A mark older than the release timeout found at a restart is removed and not
// counted: its outcome cannot be told from the mark, and a tenant can write
// such marks on its own pods. Every outcome's series still exists at 0 from
// the new ledger's first cycle.
func TestUtilizationShareRestartRemovesExpiredMarksUncounted(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()

	r := freshMetrics(t)
	restarted := newShareEngine(t, f, c, se.clock.Add(2*time.Hour)) // far past the release timeout
	aborted := map[string]string{constants.LabelOutcome: string(allocation.ShareOutcomeAborted)}
	restarted.cycle()
	series := 0
	for _, m := range family(t, r, constants.WVAUtilizationShareTransfersTotal) {
		if label(m, constants.LabelOutcome) == string(allocation.ShareOutcomeAborted) {
			series++
		}
	}
	if series == 0 {
		t.Fatal("the first cycle created no aborted series at 0")
	}
	if n := len(markedPods(t, c)); n != 0 {
		t.Fatalf("%d expired marks left on the donor's pods", n)
	}
	for i := range 3 {
		if got := counterSum(t, r, aborted); got != 0 {
			t.Fatalf("cycle %d: %v aborts counted for a mark that only expired", i+1, got)
		}
		restarted.cycle()
	}
}

// A ledger's first-cycle outcomes are kept and counted on the next cycle, once:
// their series are created at 0 in that first cycle, and one that appeared at
// 1 would be invisible to increase().
func TestUtilizationShareFirstCycleOutcomesCountOnTheNext(t *testing.T) {
	r := freshMetrics(t)
	var st utilizationShareState
	aborted := map[string]string{constants.LabelOutcome: string(allocation.ShareOutcomeAborted)}
	st.countOutcome("g", "H200", "cluster", true, allocation.ShareOutcomeAborted, false)
	if got := counterSum(t, r, aborted); got != 0 {
		t.Fatalf("a first-cycle outcome was counted at once (%v)", got)
	}
	st.flushUncounted("g", "H200", "cluster")
	if got := counterSum(t, r, aborted); got != 1 {
		t.Fatalf("flushed %v, want 1", got)
	}
	st.flushUncounted("g", "H200", "cluster")
	if got := counterSum(t, r, aborted); got != 1 {
		t.Fatalf("flushed twice: %v, want still 1", got)
	}
	st.countOutcome("g", "H200", "cluster", false, allocation.ShareOutcomeAborted, false)
	if got := counterSum(t, r, aborted); got != 2 {
		t.Fatalf("a later outcome counted %v, want 2 at once", got)
	}
}

// While the marks cannot be read every planned model is held at what it runs,
// possibly for good: a reason of its own says so -- not quiet-period, which
// clears by itself and is documented as not worth an alert.
func TestUtilizationShareUnreadMarksAreAReason(t *testing.T) {
	r := freshMetrics(t)
	f := newShareFleet()
	se := newShareEngine(t, f, failing(sharePods(t, f), func() bool { return true }, nil), time.Unix(0, 0))
	se.cycle()
	if got := reasonModels(t, r, constants.ScalingBlockedMarksUnreadable); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Fatalf("marks-unreadable while the marks cannot be read: %v, want A, B, C", got)
	}
	if got := reasonModels(t, r, constants.ScalingBlockedQuietPeriod); len(got) != 0 {
		t.Fatalf("quiet-period set for unreadable marks on %v", got)
	}
}
