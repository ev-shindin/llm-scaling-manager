package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// External analyzers are DECLARED on the cluster "default" scaling entry and
// SELECTED by name in policy tiers — "policies select/weight; they don't define"
// (docs/reference/scaling-policy.md, External analyzers). They used to live in a
// ConfigMap of their own (wva-analyzers), two objects away from the policies that
// reference them.
func TestExternalAnalyzerCatalogComesFromTheDefaultEntry(t *testing.T) {
	c := &Config{}
	c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
		GlobalDefaultsKey: {
			AnalyzerDefinitions: ExternalAnalyzerCatalog{
				"ttft-slo": {Engines: map[string]ExternalAnalyzerBody{
					"vllm":   {Query: "q_vllm", Threshold: "0.5"},
					"sglang": {Query: "q_sglang", Threshold: "0.4"},
				}},
				"pool-queue": {Query: "q_agnostic", Threshold: "1.0"},
			},
		},
	})

	cat := c.ExternalAnalyzerCatalog()
	require.Contains(t, cat, "ttft-slo")
	require.Contains(t, cat, "pool-queue")

	// An analyzer is a CONCEPT; the backend is a per-engine query variant of it, so
	// a policy enables "ttft-slo" and never "ttft-slo-vllm".
	assert.Equal(t, "q_vllm", cat["ttft-slo"].Engines["vllm"].Query)
	assert.Equal(t, "0.5", cat["ttft-slo"].Engines["vllm"].Threshold)
	assert.Equal(t, "q_sglang", cat["ttft-slo"].Engines["sglang"].Query)

	// Engine-agnostic analyzers carry a single body.
	assert.Equal(t, "q_agnostic", cat["pool-queue"].Query)
}

// The returned catalog must not alias the stored one, or a consumer mutating what
// it was handed would rewrite the queries WVA runs.
func TestExternalAnalyzerCatalogIsIsolated(t *testing.T) {
	c := &Config{}
	c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
		GlobalDefaultsKey: {
			AnalyzerDefinitions: ExternalAnalyzerCatalog{
				"a": {Engines: map[string]ExternalAnalyzerBody{"vllm": {Query: "original"}}},
			},
		},
	})

	got := c.ExternalAnalyzerCatalog()
	got["a"].Engines["vllm"] = ExternalAnalyzerBody{Query: "mutated"}
	got["injected"] = ExternalAnalyzerDef{Query: "nope"}

	fresh := c.ExternalAnalyzerCatalog()
	assert.Equal(t, "original", fresh["a"].Engines["vllm"].Query,
		"a per-engine body must not be mutable through the returned copy")
	assert.NotContains(t, fresh, "injected")
}

// Definitions are cluster-scope: a tier declaring them is a policy trying to
// define, which the design forbids. Nothing outside the default entry contributes.
func TestOnlyTheDefaultEntryContributesDefinitions(t *testing.T) {
	c := &Config{}
	c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
		GlobalDefaultsKey: {
			AnalyzerDefinitions: ExternalAnalyzerCatalog{"cluster-wide": {Query: "q"}},
		},
		"interactive": {
			AnalyzerDefinitions: ExternalAnalyzerCatalog{"tier-local": {Query: "q"}},
		},
		"m-override": {
			ModelID: "m", Namespace: "ns",
			AnalyzerDefinitions: ExternalAnalyzerCatalog{"model-local": {Query: "q"}},
		},
	})

	cat := c.ExternalAnalyzerCatalog()
	assert.Contains(t, cat, "cluster-wide")
	assert.NotContains(t, cat, "tier-local", "a policy tier must not define analyzers")
	assert.NotContains(t, cat, "model-local", "a per-model override must not define analyzers")
}

func TestExternalAnalyzerCatalogEmptyWhenUndeclared(t *testing.T) {
	c := &Config{}
	c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: {}})
	assert.Empty(t, c.ExternalAnalyzerCatalog())
}
