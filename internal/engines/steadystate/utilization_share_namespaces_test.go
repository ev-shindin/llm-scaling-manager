package steadystate

import (
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// A namespace the optimizer's namespaces block disables but no namespace quota
// names -- a misspelling, or a quota since removed -- is reported once, not
// silently ignored, and not on every cycle.
func TestUtilizationShareReportsUnmatchedDisabledNamespaces(t *testing.T) {
	us, err := config.ResolveUtilizationShare(&config.UtilizationShareConfig{
		Namespaces: map[string]config.UtilizationShareNamespace{
			"team-a":  {Enabled: ptr.To(false)},
			"tema-b":  {Enabled: ptr.To(false)}, // the quota is for team-b
			"team-on": {Enabled: ptr.To(true)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	constraints := []*allocation.ResourceConstraints{{
		ProviderName: "quota",
		NamespacePools: map[string]map[string]allocation.ResourcePool{
			"team-a": {"A100": {Limit: 4}},
			"team-b": {"A100": {Limit: 4}},
		},
	}}
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	st := &utilizationShareState{}

	st.reportUnmatchedNamespaces(logger, us, constraints)
	if len(lines) != 1 || !strings.Contains(lines[0], `"tema-b"`) {
		t.Fatalf("want one report naming tema-b only, got %q", lines)
	}
	st.reportUnmatchedNamespaces(logger, us, constraints)
	if len(lines) != 1 {
		t.Fatalf("reported again with nothing changed: %q", lines)
	}
}

// The report follows the set: a new misspelling is reported, and a set that
// empties is not reported at all, yet resets so the next misspelling is.
func TestUtilizationShareReportsEachChangeOfUnmatchedNamespaces(t *testing.T) {
	resolve := func(names ...string) config.UtilizationShare {
		ns := map[string]config.UtilizationShareNamespace{}
		for _, n := range names {
			ns[n] = config.UtilizationShareNamespace{Enabled: ptr.To(false)}
		}
		us, err := config.ResolveUtilizationShare(&config.UtilizationShareConfig{Namespaces: ns})
		if err != nil {
			t.Fatal(err)
		}
		return us
	}
	constraints := []*allocation.ResourceConstraints{{ProviderName: "quota",
		NamespacePools: map[string]map[string]allocation.ResourcePool{"team-a": {"A100": {Limit: 4}}}}}
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	st := &utilizationShareState{}

	st.reportUnmatchedNamespaces(logger, resolve("tema-a"), constraints)
	st.reportUnmatchedNamespaces(logger, resolve("taem-a"), constraints)
	if len(lines) != 2 || !strings.Contains(lines[1], `"taem-a"`) {
		t.Fatalf("a different misspelling was not reported: %q", lines)
	}
	st.reportUnmatchedNamespaces(logger, resolve("team-a"), constraints) // fixed
	if len(lines) != 2 {
		t.Fatalf("a fixed block was reported: %q", lines)
	}
	st.reportUnmatchedNamespaces(logger, resolve("tema-a"), constraints)
	if len(lines) != 3 {
		t.Fatalf("the misspelling coming back was not reported: %q", lines)
	}
}

// Floors holding more than half a group's budget above need are reported
// once, and again only after the group has recovered and relapsed.
func TestUtilizationShareReportsFloorHeavyGroupsOnce(t *testing.T) {
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	st := &utilizationShareState{}
	st.reportFloorHeavy(logger, "g", 5, 10) // exactly half: not heavy
	if len(lines) != 0 {
		t.Fatalf("half the budget reported: %q", lines)
	}
	st.reportFloorHeavy(logger, "g", 6, 10)
	st.reportFloorHeavy(logger, "g", 7, 10)
	if len(lines) != 1 || !strings.Contains(lines[0], "floorExcessGPUs") {
		t.Fatalf("want one report, got %q", lines)
	}
	st.reportFloorHeavy(logger, "g", 1, 10)
	st.reportFloorHeavy(logger, "g", 6, 10)
	if len(lines) != 2 {
		t.Fatalf("a relapse was not reported: %q", lines)
	}
}

// The evaluation sums the floors' excess over need per group and reports it
// against the group's budget: a fleet whose floors hold most of the quota is
// logged once, across cycles.
func TestUtilizationShareEvaluationReportsAFloorHeavyGroup(t *testing.T) {
	f := newShareFleet()
	f.demand = map[string]float64{"A": 100, "B": 100, "C": 100} // all near idle
	ctx, logs := observe(t)
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	se.ctx = ctx
	setShadowPolicy(t, se.e.Config, selectedShadow)
	reqs := f.requests()
	floor := 6 // three floors of 6 on a 16-GPU quota: far above the idle models' need
	for i := range reqs {
		reqs[i].VariantStates[0].MinReplicas = &floor
	}
	for range 3 {
		se.clock = se.clock.Add(30 * time.Second)
		se.e.evaluateUtilizationShare(se.ctx, reqs, fullQuota(), f.scaleTargets())
	}
	if n := logs.FilterMessageSnippet("floors hold more than half").Len(); n != 1 {
		t.Fatalf("floor-heavy group logged %d times over three cycles, want once", n)
	}
}
