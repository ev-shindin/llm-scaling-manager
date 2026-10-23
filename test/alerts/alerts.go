// Package alerts validates the shipped PrometheusRule against the metrics WVA
// actually defines.
//
// It lives here, outside test/e2e, for one reason: `make test` runs
// `go list ./... | grep -v /e2e`, so the check that alert expressions name only
// real metrics used to run ONLY against a live cluster. A typo in an alert shipped
// and was caught days later, if at all — which is how wva_node_access_denied
// shipped broken and stayed hidden behind a runtime skip. The same helpers back
// the e2e spec, which still validates the rule as DEPLOYED; this package
// validates it as WRITTEN, at unit-test speed.
package alerts

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// labelMatchers and quotedStrings are stripped from an expression before metric
// names are extracted. Their CONTENTS are values, not metric names, and an
// identifier scan cannot tell the difference:
// reason=~"variant-floor|policy-forbids-zero" otherwise yields "variant",
// "policy", "forbids" and "zero" as four unknown "metrics" and fails an alert
// that is perfectly correct.
var (
	labelMatchers = regexp.MustCompile(`\{[^}]*\}`)
	quotedStrings = regexp.MustCompile(`"[^"]*"`)
	// rangeSelectors are [5m] ranges and [30m:1m] subqueries. A subquery step
	// reads as an identifier, ":1m", because a metric name may hold a colon.
	rangeSelectors = regexp.MustCompile(`\[[^\]]*\]`)
	// identifiers matches Prometheus metric naming: alphanumerics, underscores,
	// and colons for recording rules.
	identifiers = regexp.MustCompile(`\b([a-zA-Z_:][a-zA-Z0-9_:]*)\b`)
)

// promQLKeywords are the function names, operators and label names that appear in
// expressions and are not metrics.
var promQLKeywords = map[string]bool{
	// Functions
	"rate": true, "irate": true, "sum": true, "avg": true, "min": true, "max": true,
	"count": true, "count_values": true, "stddev": true, "stdvar": true, "group": true,
	"topk": true, "bottomk": true, "quantile": true,
	"max_over_time": true, "min_over_time": true, "avg_over_time": true,
	"sum_over_time": true, "count_over_time": true, "quantile_over_time": true,
	"stddev_over_time": true, "stdvar_over_time": true,
	"last_over_time": true, "present_over_time": true,
	"absent": true, "absent_over_time": true,
	"increase": true, "delta": true, "idelta": true, "deriv": true,
	"changes": true, "resets": true, "predict_linear": true, "holt_winters": true,
	"histogram_quantile": true, "label_replace": true, "label_join": true,
	"vector": true, "scalar": true, "time": true, "timestamp": true,
	"sort": true, "sort_desc": true, "clamp": true, "clamp_min": true, "clamp_max": true,
	"round": true, "ceil": true, "floor": true, "abs": true, "sgn": true,
	"exp": true, "ln": true, "log2": true, "log10": true, "sqrt": true,
	// Keywords
	"by": true, "without": true, "and": true, "or": true, "unless": true,
	"on": true, "ignoring": true, "group_left": true, "group_right": true,
	"bool": true, "offset": true,
	// Label names
	"namespace": true, "exported_namespace": true, "variant_name": true, "model_name": true,
	"component": true, "error_type": true, "status": true,
	"query_type": true, "reason": true, "direction": true, "limiter_name": true,
	"accelerator_type": true, "accelerator_vendor": true, "accelerator_model": true,
	"controller_instance": true, "deployment": true, "service": true, "le": true,
	"objective": true, "statistic": true,
	"cohort": true, "cluster_queue": true,
	"operation": true, "role": true,
}

// ExtractMetricNames returns the metric names an expression references.
func ExtractMetricNames(expr string) []string {
	expr = labelMatchers.ReplaceAllString(expr, "")
	expr = quotedStrings.ReplaceAllString(expr, "")
	expr = rangeSelectors.ReplaceAllString(expr, "")

	var out []string
	for _, name := range identifiers.FindAllString(expr, -1) {
		if promQLKeywords[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ExternalMetrics are metrics WVA does not emit but deliberately alerts on.
//
// Kept as an explicit, short list rather than a blanket exemption for anything
// non-wva_. Each entry is a dependency on another component's metric surface: if
// EPP renames or drops one, our alert silently matches nothing and fires never,
// which is indistinguishable from a healthy cluster. Naming them here at least
// makes the dependency greppable.
var ExternalMetrics = map[string]bool{
	// EPP's request error counter. Labels observed on a real EPP scrape:
	// {error_code, fairness_id, model_name, priority, target_model_name} — note
	// there is no namespace, which is why the join partner has to be a
	// model-keyed WVA series (see constants.WVAModelReplicas).
	"llm_d_epp_request_error_total": true,
	// EPP's TTFT histogram, the series the slo analyzer judges TTFT from.
	// WVAModelSLOUnjudged reads its streaming request count to tell clients
	// without include_usage from an idle model. Like the error counter, the
	// EPP's own exposition has no namespace label; the join uses the one
	// Prometheus adds from the scrape target, which the collector's router
	// queries already select on, so it holds wherever the analyzer judges at
	// all.
	"llm_d_epp_request_ttft_seconds": true,
}

// IsKnown reports whether name is a defined metric, allowing for the suffixes the
// Prometheus client library appends to counters and histograms.
func IsKnown(name string, known map[string]bool) bool {
	if known[name] || ExternalMetrics[name] {
		return true
	}
	for _, suffix := range []string{"_total", "_count", "_sum", "_bucket"} {
		if base, found := strings.CutSuffix(name, suffix); found && ExternalMetrics[base] {
			return true
		}
	}
	for _, suffix := range []string{"_total", "_count", "_sum", "_bucket"} {
		if base, found := strings.CutSuffix(name, suffix); found && known[base] {
			return true
		}
	}
	return false
}

// RulePath is the shipped PrometheusRule the installer applies.
func RulePath() string {
	return filepath.Join(RepoRoot(), "config", "components", "prometheus-alerts", "prometheusrule.yaml")
}

// AlertNames returns the alert names in the shipped PrometheusRule.
//
// The e2e spec compares the alerts DEPLOYED on a cluster against this rather than
// against a list written out by hand. A hand-written list is a third place to
// remember an alert exists, and it had already drifted — it asserted five names
// against a manifest that had shipped six since the commit that added both, so
// the spec could never have passed. Reading the manifest makes the assertion
// "the cluster has what we ship", which is the thing actually worth checking.
func AlertNames() ([]string, error) {
	raw, err := os.ReadFile(RulePath())
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", RulePath(), err)
	}

	var rule struct {
		Spec struct {
			Groups []struct {
				Rules []struct {
					Alert string `json:"alert"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &rule); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", RulePath(), err)
	}

	var names []string
	for _, group := range rule.Spec.Groups {
		for _, r := range group.Rules {
			if r.Alert != "" {
				names = append(names, r.Alert)
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no alerts found in %s", RulePath())
	}
	return names, nil
}

// RepoRoot returns the repository root, resolved from this file's own location so
// it does not depend on the working directory a test is run from.
func RepoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// .../test/alerts/alerts.go -> repo root
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// WVAMetricNames returns every metric name WVA defines, read from the constants
// that declare them.
//
// Derived rather than listed on purpose. The e2e spec used to carry a
// hand-maintained slice, which is a second place to remember: a metric added to
// constants but not to the list made any alert naming it fail, and the error
// pointed at the alert rather than at the list.
//
// Every non-test file of internal/constants, not metrics.go alone: the
// utilization-share and SLO metrics are declared in files of their own, and an
// alert on one of them was otherwise reported as naming an unknown metric.
func WVAMetricNames() (map[string]bool, error) {
	dir := filepath.Join(RepoRoot(), "internal", "constants")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	names := make(map[string]bool)
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		collectMetricNames(parsed, names)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no wva_ metric constants found in %s", dir)
	}
	return names, nil
}

// collectMetricNames adds every wva_ string constant in file to names.
func collectMetricNames(file *ast.File, names map[string]bool) {
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, value := range spec.Values {
			lit, ok := value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			// Metric names only. Label names, reason slugs and query names are
			// declared in the same file and are not metrics.
			if strings.HasPrefix(unquoted, "wva_") {
				names[unquoted] = true
			}
		}
		return true
	})
}
