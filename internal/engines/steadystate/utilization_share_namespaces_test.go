package steadystate

import (
	"strings"
	"testing"

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
