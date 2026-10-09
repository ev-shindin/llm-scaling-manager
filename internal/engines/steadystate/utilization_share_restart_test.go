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

// A restored transfer that timed out while the controller was down ends in the
// new ledger's first cycle. Its series must already be at 0 when it is
// counted, or increase() never sees the abort: it is counted on the next cycle.
func TestUtilizationShareRestartCountsTheFirstCycleOnTheNext(t *testing.T) {
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
	if got := counterSum(t, r, constants.WVAUtilizationShareTransfersTotal, aborted); got != 0 {
		t.Fatalf("the first cycle counted %v aborts; its series would appear at 1", got)
	}
	restarted.cycle()
	if got := counterSum(t, r, constants.WVAUtilizationShareTransfersTotal, aborted); got != 1 {
		t.Fatalf("the second cycle counted %v aborts, want the restored transfer's 1", got)
	}
}

// While the marks cannot be read every planned model is held at what it runs,
// possibly for good: a blocked reason says so.
func TestUtilizationShareUnreadMarksAreAReason(t *testing.T) {
	r := freshMetrics(t)
	f := newShareFleet()
	se := newShareEngine(t, f, failing(sharePods(t, f), func() bool { return true }, nil), time.Unix(0, 0))
	se.cycle()
	if got := reasonModels(t, r, constants.ScalingBlockedQuietPeriod); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Fatalf("quiet-period while the marks cannot be read: %v, want A, B, C", got)
	}
}
