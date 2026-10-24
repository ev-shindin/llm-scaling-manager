package config

import (
	"slices"
	"strings"
)

// Named scaling policies: reusable tiers, selected per variant by the
// `scalingPolicy` key in its scaler's trigger metadata.
//
// A policy is a SCALING TIER — "interactive", "standard", "batch" — carrying
// thresholds, priority, analyzer selection and the scale-to-zero and
// scale-from-zero settings of the models whose variants name it (see
// ModelTiers for a model whose variants name more than one). It is deliberately
// IDENTITY-FREE: no model, no namespace. That is the whole point. Per-(model,
// namespace) entries bind settings to identity, so "loosen the thresholds for
// everything interactive" is an edit per model; a tier makes it one edit, and the
// set of models in the tier is declared by the models themselves.
//
// Policies live in the same ConfigMap as the default entry and per-model
// overrides, told apart like this:
//
//	key "default"              the cluster default entry
//	body sets model_id         a per-model override, key is arbitrary
//	anything else              a named policy tier, keyed by its name
//
// Identity in the BODY, not the key. See PolicyEntryKey for why the original
// {modelID}#{namespace} key form could never work.
//
// See docs/proposals/wva-keda-external-scaler.md §7.5.

// ModelTiers returns the policy tiers a model is resolved under, given the tier
// each of its variants names (one element per variant, "" for none). Sorted
// and distinct, so the result is the same on every cycle.
//
// A variant that names no tier follows the ones that do. Its thresholds already
// did (one tier per model, the smallest named), so its scale-to-zero,
// scale-from-zero and weight follow too: a P/D model with only its decode tagged
// must not be held at its untagged prefill's default. "" is returned only when
// no variant names a tier, and then it is the only element. Empty input is
// treated as one untagged variant.
func ModelTiers(named []string) []string {
	var tiers []string
	for _, tier := range named {
		if tier != "" && !slices.Contains(tiers, tier) {
			tiers = append(tiers, tier)
		}
	}
	if len(tiers) == 0 {
		return []string{""}
	}
	slices.Sort(tiers)
	return tiers
}

// ModelPolicyTier returns the one tier a model's thresholds and weight resolve
// under, given the tier each of its variants names: the lexicographically
// smallest named tier, or "" (the default entry and its defaultPolicy) when
// none names one. Smallest, so a conflict resolves the same way on every cycle
// and every replica.
//
// The steady-state planner and a scale-from-zero wake claim both use it: the
// claim wins back what the planner would give, so both must weigh the model
// under the same tier.
func ModelPolicyTier(named []string) string {
	return ModelTiers(named)[0]
}

// PolicyEntryKey reports whether a ConfigMap entry names a policy tier, rather
// than the default entry or a per-model override.
//
// An override is identified by its BODY — a non-empty modelID — not by the shape
// of its key. It had to be: the key form was {modelID}#{namespace}, and a
// ConfigMap data key may only contain [-._a-zA-Z0-9]. '#' is not in that set and
// neither is the '/' in a namespaced model ID, so the API server rejected every
// per-model override anyone ever tried to write. The feature could not be used at
// all, for any model, which is why no shipped ConfigMap or e2e ever carried one.
//
// The fields were always there for this: ScalingPolicy.ModelID and .Namespace are
// documented as "only used in override entries". The key name is arbitrary and
// model_id + namespace identify the model, so every key an operator chooses is
// legal.
func PolicyEntryKey(key string, entry ScalingPolicy) bool {
	return key != GlobalDefaultsKey && entry.ModelID == ""
}

// FindModelOverride returns the entry overriding settings for one model in one
// namespace, and whether there was one.
//
// Scans rather than looks up, because identity now lives in the body. Ties are
// broken by the lexicographically smallest key so two entries claiming the same
// model resolve the same way on every process and every restart — an allocation
// that changes with map order is worse than either candidate.
func FindModelOverride(configMap map[string]ScalingPolicy, modelID, namespace string) (ScalingPolicy, bool) {
	found, foundKey := ScalingPolicy{}, ""
	for key, entry := range configMap {
		if key == GlobalDefaultsKey || entry.ModelID != modelID || entry.Namespace != namespace {
			continue
		}
		if foundKey == "" || key < foundKey {
			found, foundKey = entry, key
		}
	}
	return found, foundKey != ""
}

// ModelOverrideKey suggests a readable, legal ConfigMap key for a per-model
// override. It is a CONVENIENCE, not the lookup: the key may be anything, and
// what binds the entry to a model is the modelID/namespace in its body. Illegal
// characters become '_' so the suggestion is always writable.
func ModelOverrideKey(modelID, namespace string) string {
	return sanitizeConfigMapKey(modelID) + modelOverrideKeySeparator + sanitizeConfigMapKey(namespace)
}

// sanitizeConfigMapKey replaces every character Kubernetes disallows in a
// ConfigMap data key with '_'.
func sanitizeConfigMapKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '.', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// modelOverrideKeySeparator joins modelID and namespace in a suggested key.
const modelOverrideKeySeparator = "."

// ScalingPolicies returns the named policy tiers declared in a resolved config
// map, keyed by tier name.
func NamedPolicies(configMap map[string]ScalingPolicy) map[string]ScalingPolicy {
	policies := make(map[string]ScalingPolicy)
	for key, entry := range configMap {
		if PolicyEntryKey(key, entry) {
			policies[key] = entry
		}
	}
	return policies
}

// ResolveScalingPolicy resolves config for a model.
// Starts from the "default" entry (or zero-value), then merges the model-specific
// override (an entry whose body names this model) on top. This allows per-model
// overrides to specify only the fields they want to change.
// After merging, ApplyDefaults fills remaining zero-valued fields, then
// ApplyV2ThresholdDefaults calibrates the V2 thresholds on the final config and an
// inconsistent (inverted) threshold pair is reset to the defaults.
func ResolveScalingPolicy(
	configMap map[string]ScalingPolicy,
	modelID, namespace string,
) ScalingPolicy {
	return ResolveScalingPolicyForTier(configMap, modelID, namespace, "")
}

// ResolveScalingPolicyForTier resolves the effective entry, layering the
// named policy tier the variant selected between the cluster default and any
// per-model override:
//
//	default entry            fleet-wide fallback
//	  â–¼ overridden by
//	named policy tier        the variant's scalingPolicy, or defaultPolicy
//	  â–¼ overridden by
//	model override entry     an entry whose body names this model, most specific
//
// Most specific wins, per docs/proposals/wva-keda-external-scaler.md Â§7.5. The
// per-model override stays the innermost layer so existing configs keep working
// while tiers are adopted: a fleet moves to policies model by model, not all at
// once.
//
// An unknown policy name resolves to the default entry â€” the same outcome as
// naming none, which is the right behaviour but a bad silence, so the caller
// reports it (see policyResolution).
func ResolveScalingPolicyForTier(
	configMap map[string]ScalingPolicy,
	modelID, namespace, policy string,
) ScalingPolicy {
	// Start with default config as base
	base := ScalingPolicy{}
	if defaultCfg, ok := configMap[GlobalDefaultsKey]; ok {
		base = defaultCfg
	}
	// Overlay the named policy tier, falling back to the cluster's defaultPolicy.
	// Read from the DEFAULT entry only: a fleet-wide fallback chosen by a policy or
	// a per-model entry would be that entry choosing for everyone but itself.
	if policy == "" {
		policy = base.DefaultPolicy
	}
	if policy != "" {
		if tier, ok := configMap[policy]; ok && PolicyEntryKey(policy, tier) {
			base.Merge(tier)
		}
	}
	// Overlay model-specific override if present (non-zero fields win)
	if override, ok := FindModelOverride(configMap, modelID, namespace); ok {
		base.Merge(override)
	}
	base.ApplyDefaults()
	// Calibrate V2 thresholds on the final merged config. Analyzer selection is
	// global, so this entry may run on the V2 path even when written V1-style;
	// applied here (post-merge) rather than in ApplyDefaults so a V1-style override
	// cannot clobber a tuned global threshold during Merge.
	base.ApplyV2ThresholdDefaults()
	// Per-entry V2 thresholds are range-validated at load, but a merge can still
	// produce an inconsistent pair across entries (e.g. an override raises
	// scaleDownBoundary above the base scaleUpThreshold). Rather than feed the
	// optimizer an inverted pair, fall back to the defaults for both.
	if base.ScaleUpThreshold <= base.ScaleDownBoundary {
		base.ScaleUpThreshold = DefaultScaleUpThreshold
		base.ScaleDownBoundary = DefaultScaleDownBoundary
	}
	return base
}
