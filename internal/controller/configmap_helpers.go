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
	"maps"
	"slices"

	"github.com/go-logr/logr"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// parseScalingPolicyConfig parses saturation scaling configuration from ConfigMap data.
// Returns the parsed configs and count of successfully parsed entries.
func parseScalingPolicyConfig(cmData map[string]string, logger logr.Logger) (config.ScalingPolicySet, int) {
	configs := make(config.ScalingPolicySet)
	count := 0
	for key, yamlStr := range cmData {
		var satConfig config.ScalingPolicy
		if err := yaml.Unmarshal([]byte(yamlStr), &satConfig); err != nil {
			errorType := "Failed to parse saturation scaling config entry"
			logger.Error(err, errorType, "key", key)
			metrics.RecordError(constants.ComponentController, errorType)
			continue
		}
		// Fold analyzer plugin parameters into typed fields before defaulting/validation.
		if err := satConfig.Normalize(); err != nil {
			errorType := "Invalid saturation scaling config entry"
			logger.Error(err, errorType, "key", key)
			metrics.RecordError(constants.ComponentController, errorType)
			continue
		}
		// Apply defaults before validation (handles omitempty zero-values like scaleUpThreshold)
		satConfig.ApplyDefaults()
		if err := satConfig.Validate(); err != nil {
			errorType := "Invalid saturation scaling config entry"
			logger.Error(err, errorType, "key", key)
			metrics.RecordError(constants.ComponentController, errorType)
			continue
		}
		// limiters: is a cluster-"default"-scope setting read only from the global
		// default entry (Config.EffectiveLimiterMode). Warn if it appears elsewhere so
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
		// The limiters: list reads like a set of bounds that all apply; it is not.
		// One mode is selected, quota wins, and anything else declared is built as
		// nothing. Silence here is the hazard: the operator reads the config as
		// bounded by real GPUs too, while nothing consults physical capacity and a
		// scale-from-zero wake can land on a full accelerator.
		//
		// Only for the default entry: limiters anywhere else are ignored whole, and
		// the warning above already says so.
		if dropped := satConfig.UnenforcedLimiterTypes(); key == config.GlobalDefaultsKey && len(dropped) > 0 {
			// "WARNING:" rather than a level, because logr has none between Info and
			// Error: V-levels only go more verbose. Same convention as cmd/main.go.
			logger.Info("WARNING: some declared limiters will NOT be enforced; a quota entry "+
				"selects the quota limiter and it is the only one built, so physical "+
				"GPU capacity is not consulted at all. Declare one kind, or track "+
				"issue #1003 for bounding by min(physical, quota)",
				"key", key, "notEnforced", dropped, "enforcing", config.LimiterTypeQuota)
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
		count++
	}
	return configs, count
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
