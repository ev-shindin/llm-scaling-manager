package alerts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// prometheusRule is the subset of the PrometheusRule this check reads.
type prometheusRule struct {
	Spec struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Alert       string            `json:"alert"`
				Expr        string            `json:"expr"`
				For         string            `json:"for"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"spec"`
}

func loadRule(t *testing.T) prometheusRule {
	t.Helper()
	path := filepath.Join(RepoRoot(), "config", "components", "prometheus-alerts", "prometheusrule.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)

	var rule prometheusRule
	require.NoError(t, yaml.Unmarshal(raw, &rule))
	require.NotEmpty(t, rule.Spec.Groups, "PrometheusRule has no groups")
	return rule
}

// The check that used to run only against a live cluster. An alert naming a
// metric that does not exist matches nothing and fires never, which looks exactly
// like a healthy cluster -- so it is not the sort of mistake that surfaces on its
// own. wva_node_access_denied shipped that way.
func TestAlertsReferenceOnlyDefinedMetrics(t *testing.T) {
	known, err := WVAMetricNames()
	require.NoError(t, err)
	t.Logf("%d wva_ metric constants defined", len(known))

	rule := loadRule(t)
	var alerts int
	for _, group := range rule.Spec.Groups {
		for _, r := range group.Rules {
			if r.Alert == "" {
				continue
			}
			alerts++
			for _, name := range ExtractMetricNames(r.Expr) {
				assert.True(t, IsKnown(name, known),
					"alert %q references unknown metric %q\n  expr: %s\n"+
						"  If this is a new WVA metric, define it in internal/constants/metrics.go.\n"+
						"  If it is an external metric, it needs an explicit allowance here.",
					r.Alert, name, r.Expr)
			}
		}
	}
	assert.NotZero(t, alerts, "no alerts found -- the check would pass vacuously")
}

func TestAlertsHaveRequiredFields(t *testing.T) {
	rule := loadRule(t)
	for _, group := range rule.Spec.Groups {
		for _, r := range group.Rules {
			if r.Alert == "" {
				continue
			}
			assert.NotEmpty(t, r.Expr, "alert %q has no expression", r.Alert)
			assert.NotEmpty(t, r.Labels["severity"], "alert %q has no severity", r.Alert)
			assert.NotEmpty(t, r.Annotations["summary"], "alert %q has no summary", r.Alert)
			assert.NotEmpty(t, r.Annotations["description"],
				"alert %q has no description -- the description is where an operator "+
					"finds out what to DO about it", r.Alert)
		}
	}
}

// The stripping is what makes a label matcher safe to write. Without it the
// contents of reason=~"..." are scanned as identifiers, and four of them are not
// PromQL keywords, so a correct alert fails.
func TestExtractMetricNamesIgnoresLabelMatcherContents(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr string
		want []string
	}{
		{
			name: "regex matcher values are not metric names",
			expr: `wva_model_scaling_blocked{reason=~"variant-floor|policy-forbids-zero"} == 1`,
			want: []string{"wva_model_scaling_blocked"},
		},
		{
			name: "equality matcher values are not metric names",
			expr: `wva_model_scaling_blocked{reason="no-wake-signal"} == 1`,
			want: []string{"wva_model_scaling_blocked"},
		},
		{
			name: "label_replace's string arguments are not metric names",
			expr: `label_replace(wva_current_replicas, "model", "$1", "variant_name", "(.*)-.*")`,
			want: []string{"wva_current_replicas"},
		},
		{
			name: "aggregation and rate survive",
			expr: `sum by (query_type, reason) (rate(wva_metrics_collection_errors_total[5m])) > 0.0833`,
			want: []string{"wva_metrics_collection_errors_total"},
		},
		{
			name: "a subquery's resolution is not a metric name",
			expr: `count_over_time((wva_model_scaling_blocked unless wva_model_scaling_blocked offset 1m)[1h:1m]) > 3`,
			want: []string{"wva_model_scaling_blocked", "wva_model_scaling_blocked"},
		},
		{
			name: "multiple metrics are all found",
			expr: `wva_current_replicas == 0 and on() wva_desired_replicas > 0`,
			want: []string{"wva_current_replicas", "wva_desired_replicas"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractMetricNames(tc.expr))
		})
	}
}

// Deriving the list from the constants is the point: a metric added to
// internal/constants must not need a second edit somewhere else to be usable in
// an alert.
func TestWVAMetricNamesComesFromTheConstants(t *testing.T) {
	known, err := WVAMetricNames()
	require.NoError(t, err)

	assert.True(t, known["wva_model_scaling_blocked"])
	assert.True(t, known["wva_current_replicas"])
	assert.True(t, known["wva_node_access_denied"])
	// Reason slugs and label names live in the same file and are not metrics.
	assert.False(t, known["variant-floor"])
	assert.False(t, known["namespace"])
}
