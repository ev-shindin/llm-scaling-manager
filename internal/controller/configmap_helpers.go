/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// parseScalingPolicyConfig parses saturation scaling configuration from ConfigMap data.
// Returns the parsed configs and count of successfully parsed entries.
func parseScalingPolicyConfig(cmData map[string]string, logger logr.Logger) (config.ScalingPolicySet, int) {
	configs, count, _, _ := parseScalingPolicyConfigRejects(cmData, logger)
	return configs, count
}

// ignoredFieldErrorType is the error_type a field cleared by
// ResetUnusableFields is counted under, once per field per ConfigMap read.
const ignoredFieldErrorType = "Ignored unusable saturation scaling config field"

// reservedLimiterNameErrorType is the error_type a quota entry named one of
// config.ReservedLimiterNames is counted under, once per entry per ConfigMap
// read, and starts its log line. The entry still applies.
const reservedLimiterNameErrorType = "Quota limiter entry has a reserved name"

// parseScalingPolicyConfigRejects is parseScalingPolicyConfig that also returns,
// by key, why each entry that was dropped was dropped: a YAML error, or a
// Normalize or Validate failure. An entry that parsed is never in it. A key the
// schema does not define drops nothing (config.DecodeScalingPolicy); the
// reconciler reports it (reportUnknownKeys).
//
// ignored holds, by key, the fields of each KEPT entry that were cleared
// because their value could not be used (ScalingPolicy.ResetUnusableFields).
// The entry still applies; only those fields act as unset. A dropped entry is
// reported in rejected only.
func parseScalingPolicyConfigRejects(cmData map[string]string, logger logr.Logger) (
	config.ScalingPolicySet, int, map[string]error, map[string][]config.IgnoredField,
) {
	configs := make(config.ScalingPolicySet)
	rejected := map[string]error{}
	ignored := map[string][]config.IgnoredField{}
	count := 0
	for key, yamlStr := range cmData {
		satConfig, err := config.DecodeScalingPolicy(yamlStr)
		if err != nil {
			errorType := "Failed to parse saturation scaling config entry"
			logger.Error(err, errorType, "key", key)
			metrics.RecordError(constants.ComponentController, errorType)
			rejected[key] = err
			continue
		}
		// Fold analyzer plugin parameters into typed fields before defaulting/validation.
		if err := satConfig.Normalize(); err != nil {
			errorType := "Invalid saturation scaling config entry"
			logger.Error(err, errorType, "key", key)
			metrics.RecordError(constants.ComponentController, errorType)
			rejected[key] = err
			continue
		}
		// An unusable value in one of these fields is ignored, not fatal:
		// rejecting the entry would drop everything else in it, and for the
		// default entry that includes its limiters. Cleared before Validate, so
		// it cannot fail the entry.
		fieldsIgnored := satConfig.ResetUnusableFields()
		// Validated as a defaulted COPY. The stored entry keeps every field it
		// does not set at zero ("unset"), so a tier or override inherits it in
		// the merge; the resolved policy is fully defaulted after it.
		if err := satConfig.ValidateEntry(); err != nil {
			errorType := "Invalid saturation scaling config entry"
			logger.Error(err, errorType, "key", key)
			metrics.RecordError(constants.ComponentController, errorType)
			rejected[key] = err
			continue
		}
		// limiters: is a cluster-"default"-scope setting read only from the global
		// default entry (Config.EffectiveLimiterTypes). Warn if it appears elsewhere so
		// a misplaced block is not silently ignored.
		if key != config.GlobalDefaultsKey && len(satConfig.Limiters) > 0 {
			logger.Info("Ignoring limiters on a non-default saturation config entry; "+
				"the GPU limiter is selected only from the \"default\" entry", "key", key)
		}
		// optimizer: sits beside limiters: and is read from the same place only.
		if key != config.GlobalDefaultsKey && satConfig.Optimizer != nil {
			logger.Info("Ignoring optimizer on a non-default saturation config entry; "+
				"the optimizer is selected only from the \"default\" entry, beside limiters", "key", key)
		}
		// Analyzer DEFINITIONS are tier-level, like limiters. A named policy
		// selects and weights analyzers by name; it does not get to invent the
		// PromQL behind one, because that query runs against the shared Prometheus
		// every cycle and a wrongly-shaped one mis-scales silently. Ignoring it
		// quietly would leave a policy that reads as if it defined an analyzer and
		// an analyzer that does not exist.
		if key != config.GlobalDefaultsKey && len(satConfig.AnalyzerDefinitions) > 0 {
			logger.Info("Ignoring analyzerDefinitions on a non-default saturation config entry; "+
				"policies select and weight analyzers by name but do not define them — "+
				"declare the analyzer on the \"default\" entry and reference it here",
				"key", key, "analyzers", slices.Sorted(maps.Keys(satConfig.AnalyzerDefinitions)))
		}
		configs[key] = satConfig
		if len(fieldsIgnored) > 0 {
			ignored[key] = fieldsIgnored
		}
		count++
	}
	warnAboutTuningFallbacks(logger, configs)
	return configs, count, rejected, ignored
}

// warnAboutTuningFallbacks logs one WARNING per tuning figure a kept entry
// sets that the analyzer will not run with (config.TuningFallbacks): a pair
// the entry crosses with a key the default entry sets or leaves at its
// default, which the resolved policy puts back to both defaults, or an
// idleKvUsage held under kvCacheThreshold. Each entry is resolved over the
// "default" entry, as a model with no tier is. Logged once per ConfigMap
// read, not per cycle: the entry is accepted, so nothing else says the
// setting is ignored.
func warnAboutTuningFallbacks(logger logr.Logger, configs config.ScalingPolicySet) {
	var base *config.ScalingPolicy
	if d, ok := configs[config.GlobalDefaultsKey]; ok {
		base = &d
	}
	for _, key := range slices.Sorted(maps.Keys(configs)) {
		over := base
		if key == config.GlobalDefaultsKey {
			over = nil
		}
		for _, w := range config.TuningFallbacks(over, configs[key]) {
			logger.Info("WARNING: tuning figure not used: "+w, "key", key)
		}
	}
}

// warnAboutLimiters logs the findings about the limiters of policy, the
// "default" entry the cluster's limiters are read from. The caller calls it
// only for that entry: limiters anywhere else are ignored whole, and a
// warning about them would describe a bound nothing enforces.
//
// "WARNING:" rather than a level, because logr has none between Info and
// Error: V-levels only go more verbose. Same convention as cmd/main.go.
func warnAboutLimiters(logger logr.Logger, policy config.ScalingPolicy) {
	key := config.GlobalDefaultsKey
	// A second physical entry is accepted (rejecting it would drop the whole
	// policy) but only one physical limiter is built; say which were ignored.
	if dups := policy.IgnoredPhysicalLimiters(); len(dups) > 0 {
		logger.Info(config.IgnoredPhysicalLimitersMessage, "key", key, "ignoredIndexes", dups)
	}
	// Quota findings that do not reject the config, such as a namespace in
	// exclude that nothing bounds. Logged once per config change.
	for _, w := range policy.LimiterWarnings() {
		logger.Info("WARNING: "+w, "key", key)
	}
	// A quota entry with a reserved name is accepted in this release
	// (config.ReservedLimiterNames): rejecting it would drop the default
	// entry, and at startup stop the controller mid-upgrade. Warned on
	// every read, counted, and listed in the controller status
	// (limiterWarnings) while it is in force.
	for _, w := range config.ReservedLimiterNameWarnings(policy.Limiters) {
		logger.Info("WARNING: "+reservedLimiterNameErrorType+"; it applies in this release, rename it before a later one rejects it",
			"key", key, "warning", w)
		metrics.RecordError(constants.ComponentController, reservedLimiterNameErrorType)
	}
}

// maxIgnoredFieldsReported is how many ignored fields of one ConfigMap are
// logged and sent as Events on each read; the rest are summed in one line.
// A ConfigMap is read on every edit and at every start, and a tenant writes
// how many entries it has.
const maxIgnoredFieldsReported = 10

// reportIgnoredFields makes each field parseScalingPolicyConfigRejects cleared
// as visible as a rejected entry or an unknown key: a log line carrying the
// ignoredFieldErrorType text and the ConfigMap's identity, a Warning
// ConfigFieldIgnored Event on the ConfigMap, a count in wva_errors_total under
// ignoredFieldErrorType, and wva_config_ignored_fields for the ConfigMap. Only
// kept entries have ignored fields: a dropped one is reported as rejected, and
// nothing in it applies.
//
// Each value is already truncated (config.TruncateIgnoredValue). At most
// maxIgnoredFieldsReported fields are listed per read; one more line says how
// many were not. Every field is counted in both metrics.
//
// The caller decides whether to report: the policy namespace's ConfigMap is
// parsed with the same function, but only its limiters and optimizer are read,
// so a field cleared there would be reported for a value nothing uses.
func (r *ConfigMapReconciler) reportIgnoredFields(ctx context.Context, cm *corev1.ConfigMap,
	ignored map[string][]config.IgnoredField) {
	logger := log.FromContext(ctx)
	total := 0
	for _, key := range slices.Sorted(maps.Keys(ignored)) {
		for _, f := range ignored[key] {
			total++
			metrics.RecordError(constants.ComponentController, ignoredFieldErrorType)
			if total > maxIgnoredFieldsReported {
				continue
			}
			logger.Info("WARNING: "+ignoredFieldErrorType+"; the field is treated as unset and the rest of the entry applies",
				"namespace", cm.GetNamespace(), "configMap", cm.GetName(), "entry", key,
				"field", f.Field, "value", f.Value, "reason", f.Reason.Error())
			if r.Recorder != nil {
				r.Recorder.Eventf(cm, corev1.EventTypeWarning, constants.K8SEventConfigFieldIgnored,
					"entry %q: %s; the field acts as unset, the rest of the entry applies", key, f.String())
			}
		}
	}
	if more := total - maxIgnoredFieldsReported; more > 0 {
		logger.Info("WARNING: "+ignoredFieldErrorType+fmt.Sprintf("; +%d more not listed", more),
			"namespace", cm.GetNamespace(), "configMap", cm.GetName(), "ignoredFields", total)
		if r.Recorder != nil {
			r.Recorder.Eventf(cm, corev1.EventTypeWarning, constants.K8SEventConfigFieldIgnored,
				"+%d more fields act as unset (%d in all); see wva_config_ignored_fields", more, total)
		}
	}
	metrics.SetConfigIgnoredFields(cm.GetNamespace(), cm.GetName(), total)
}

// isNamespaceConfigEnabled checks if a namespace has the opt-in label for namespace-local ConfigMaps.
// This allows namespaces to opt-in for ConfigMap watching even before VAs are created.
// Package-level function so it can be used by both reconcilers.
func isNamespaceConfigEnabled(ctx context.Context, c client.Reader, namespace string) bool {
	if namespace == "" {
		return false
	}

	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		// If namespace doesn't exist or we can't read it, default to not enabled
		// This is safe - we'll proceed with normal logic
		return false
	}

	labels := ns.GetLabels()
	if labels == nil {
		return false
	}

	value, exists := labels[constants.NamespaceConfigEnabledLabelKey]
	return exists && value == constants.AnnotationValueTrue
}

// isNamespaceExcluded checks if a namespace has the exclude annotation.
// Excluded namespaces are not watched for ConfigMaps or reconciled for VAs.
// Thread-safe (reads namespace object from API server).
// Package-level function so it can be used by both reconcilers.
func isNamespaceExcluded(ctx context.Context, c client.Reader, namespace string) bool {
	if namespace == "" {
		return false
	}

	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		// If namespace doesn't exist or we can't read it, default to not excluded
		// This is safe - we'll proceed with normal logic
		return false
	}

	annotations := ns.GetAnnotations()
	if annotations == nil {
		return false
	}

	value, exists := annotations[constants.NamespaceExcludeAnnotationKey]
	return exists && value == constants.AnnotationValueTrue
}
