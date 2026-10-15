package alerts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The two node-read outcomes page differently. A gpu-inventory limiter declared
// alone without node read scales nothing up: wva_node_access_denied, critical.
// Beside a quota the physical limiter is only skipped and the quota still
// bounds; the inventory no longer raises wva_node_access_denied there, so the
// skip needs an alert of its own, and it must not page as critical.
func TestNodeReadDenialPagesCriticalOnlyForAPhysicalLimiterAlone(t *testing.T) {
	rule := loadRule(t)
	severities := map[string][]string{}
	for _, group := range rule.Spec.Groups {
		for _, r := range group.Rules {
			if r.Alert == "" {
				continue
			}
			for _, name := range ExtractMetricNames(r.Expr) {
				severities[name] = append(severities[name], r.Alert+"="+r.Labels["severity"])
			}
		}
	}

	denied := severities["wva_node_access_denied"]
	assert.NotEmpty(t, denied, "no alert on wva_node_access_denied")
	for _, s := range denied {
		assert.Contains(t, s, "=critical", "a physical limiter alone without node read scales nothing up: %s", s)
	}

	skipped := severities["wva_constraint_provider_skipped"]
	assert.NotEmpty(t, skipped,
		"no alert on wva_constraint_provider_skipped: a gpu-inventory limiter skipped beside a quota would go unalerted")
	for _, s := range skipped {
		assert.False(t, strings.HasSuffix(s, "=critical"),
			"a skipped provider leaves the other limiters bounding; it must not page as critical: %s", s)
	}
}
