package allocation

import (
	"os"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// configFromPolicyText parses a scaling-policy ConfigMap's default entry the
// way the controller does (internal/controller parseScalingPolicyConfig:
// unmarshal, Normalize, ApplyDefaults, Validate) and installs it.
func configFromPolicyText(t *testing.T, defaultEntry string) *config.Config {
	t.Helper()
	var policy config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(defaultEntry), &policy); err != nil {
		t.Fatalf("parse default entry: %v", err)
	}
	if err := policy.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	policy.ApplyDefaults()
	if err := policy.Validate(); err != nil {
		t.Fatalf("the main-era policy must still validate: %v", err)
	}
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": policy})
	return cfg
}

// The upgrade case, from the ConfigMap TEXT a main-era install left behind:
// gpu-inventory beside a quota. Main built the quota alone. After the upgrade
// both are built, and the limiter signature differs from the quota-only one,
// so a running engine rebuilds rather than keeping the quota-only limiter.
func TestMainEraComposedPolicyBuildsBothLimiters(t *testing.T) {
	raw, err := os.ReadFile("testdata/main-era-composed-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	// A CRLF checkout must not decide whether the physical entry is found.
	composedText := strings.ReplaceAll(cm.Data["default"], "\r\n", "\n")
	if !strings.Contains(composedText, "gpu-inventory") || !strings.Contains(composedText, "type: quota") {
		t.Fatalf("fixture lost one of its limiters:\n%s", composedText)
	}

	composed := configFromPolicyText(t, composedText)
	l, err := NewLimiterFromConfig(composed, nil)
	if err != nil {
		t.Fatalf("NewLimiterFromConfig: %v", err)
	}
	c, ok := l.(*CompositeLimiter)
	if !ok {
		t.Fatalf("built %T %q, want a composite of the physical limiter and the quota", l, l.Name())
	}
	names := make([]string, 0, len(c.Constituents()))
	for _, constituent := range c.Constituents() {
		names = append(names, constituent.Name())
	}
	if strings.Join(names, ",") != "gpu-limiter,tenant-quota" {
		t.Fatalf("constituents %v, want [gpu-limiter tenant-quota]: physical first, then the quota", names)
	}

	// The same text without the physical entry: what main effectively ran.
	quotaOnlyText := strings.Replace(composedText, "  - type: gpu-inventory\n", "", 1)
	if quotaOnlyText == composedText {
		t.Fatal("could not strip the physical entry from the fixture")
	}
	quotaOnly := configFromPolicyText(t, quotaOnlyText)
	if LimiterSignature(composed) == LimiterSignature(quotaOnly) {
		t.Fatalf("the composed policy signs like the quota-only one (%s): adding the physical entry would not rebuild a running limiter",
			LimiterSignature(composed))
	}
}
