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
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controllerstatus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// ConfigMapReconciler reconciles ConfigMaps to update the unified configuration.
// Its sole responsibility is to keep the Config object synchronized with ConfigMap changes.
type ConfigMapReconciler struct {
	client.Reader
	Scheme    *runtime.Scheme
	Config    *config.Config
	Datastore datastore.Datastore
	Recorder  record.EventRecorder

	// ThroughputRegistered is the throughput-analyzer registration decision
	// frozen at startup (cmd/main.go). Analyzer registration cannot change
	// without a controller restart; the reconciler compares live config against
	// this to warn when a runtime edit would be silently inert.
	ThroughputRegistered bool

	// Status receives the version of the authoritative policy ConfigMap each
	// time one is applied, for the controller-status ConfigMap. Nil-safe:
	// tests that do not check it leave it unset.
	Status *controllerstatus.Publisher
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles ConfigMap changes and updates the unified configuration.
func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ConfigMap
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, req.NamespacedName, cm); err != nil {
		if apierrors.IsNotFound(err) {
			// ConfigMap was deleted - handle cleanup
			logger.Info("ConfigMap deleted", "name", req.Name, "namespace", req.Namespace)
			r.handleConfigMapDeletion(ctx, req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		errorType := "Failed to get ConfigMap"
		logger.Error(err, errorType, "name", req.Name, "namespace", req.Namespace)
		metrics.RecordError(constants.ComponentController, errorType)
		return ctrl.Result{}, err
	}

	name := cm.GetName()
	namespace := cm.GetNamespace()
	systemNamespace := config.SystemNamespace()

	// Determine if this is a global or namespace-local ConfigMap
	isGlobal := namespace == systemNamespace
	// The policy namespace is a third source, and it is checked before the
	// namespace-local test: when it is separate it is by definition NOT a watched
	// tenant namespace, and treating it as one would file the cluster's limiters
	// under a namespace override.
	isPolicySource := r.Config.PolicyNamespaceIsSeparate() && namespace == r.Config.PolicyNamespace()
	isNamespaceLocal := !isGlobal && !isPolicySource && r.shouldWatchNamespaceLocalConfigMap(ctx, namespace)

	if isPolicySource {
		if slices.Contains(config.ScalingPolicyConfigMapNames(), name) {
			// The error is a startup one only (errUnboundedAtStartup), and the
			// handler has already reported it; a requeue would change nothing.
			_ = r.handleClusterPolicyConfigMap(ctx, cm)
		}
		return ctrl.Result{}, nil
	}

	// Only process if it's global or namespace-local (tracked)
	if !isGlobal && !isNamespaceLocal {
		logger.V(1).Info("Ignoring ConfigMap from untracked namespace", "name", name, "namespace", namespace)
		return ctrl.Result{}, nil
	}

	// Route to appropriate handler based on ConfigMap name. Both scaling-policy
	// names are accepted — the current one and the pre-rename one — so an operator
	// who has not renamed theirs keeps getting live updates. Using the old name is
	// reported once per change by handleScalingPolicyConfigMap.
	if slices.Contains(config.ScalingPolicyConfigMapNames(), name) {
		// As above: the error arises at startup only and is already reported.
		_ = r.handleScalingPolicyConfigMap(ctx, cm, namespace, isGlobal)
	} else {
		logger.V(1).Info("Ignoring unrecognized ConfigMap", "name", name, "namespace", namespace)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(ConfigMapPredicate(r.Datastore, r.Config)).
		Complete(r)
}

// handleConfigMapDeletion handles ConfigMap deletion events.
func (r *ConfigMapReconciler) handleConfigMapDeletion(ctx context.Context, name, namespace string) {
	logger := log.FromContext(ctx)
	systemNamespace := config.SystemNamespace()

	// A deleted ConfigMap has no rejected entries and no unknown keys. Cleared
	// for every scaling-policy ConfigMap, before any of the returns below: the
	// gauges were set wherever one was loaded.
	if slices.Contains(config.ScalingPolicyConfigMapNames(), name) {
		metrics.DeleteConfigEntryGauges(namespace, name)
	}

	// Cluster policy first. The policy namespace holds no workloads, so it is
	// never "tracked", and the tracking check below would return before ever
	// reaching it — meaning an admin who DELETED the limiters left the
	// last-loaded ones in force indefinitely, with the config showing nothing
	// that explained why. Removing a bound has to take effect as reliably as
	// adding one.
	if r.Config.PolicyNamespaceIsSeparate() && namespace == r.Config.PolicyNamespace() {
		if slices.Contains(config.ScalingPolicyConfigMapNames(), name) {
			// An empty policy, not nil: nil would send the limiter accessors back
			// to this controller's own namespace, handing limit-setting to whoever
			// writes that — the party the policy namespace exists to take it from.
			r.Config.UpdateClusterPolicy(&config.ScalingPolicy{})
			r.Status.RecordPolicy(r.Config, namespace, name, "", nil)
			logger.Info("Cluster policy ConfigMap deleted: no limiters or quotas are in force",
				"policyNamespace", namespace, "configMap", name)
		}
		return
	}

	// Only handle namespace-local ConfigMap deletions (not global)
	if namespace == systemNamespace {
		return
	}

	// Check if this namespace should be tracked
	if !r.shouldWatchNamespaceLocalConfigMap(ctx, namespace) {
		return
	}

	// Remove namespace-local config on deletion. One kind now — the separate
	// scale-to-zero ConfigMap folded into this one.
	if name == config.ScalingPolicyConfigMapName() {
		r.Config.RemoveNamespaceConfig(namespace)
		logger.Info("Removed namespace-local saturation config on ConfigMap deletion", "namespace", namespace)
	}
}

// shouldWatchNamespaceLocalConfigMap returns true if a namespace-local ConfigMap should be watched.
// In single-namespace mode (--watch-namespace set), it watches all ConfigMaps in the watched namespace.
// In multi-namespace mode, it checks exclusion first (highest priority), then VA-based tracking (automatic), then opt-in label (explicit).
func (r *ConfigMapReconciler) shouldWatchNamespaceLocalConfigMap(ctx context.Context, namespace string) bool {
	// In single-namespace mode, watch all ConfigMaps in the watched namespace
	// Explicit CLI flag overrides annotation/label-based filtering
	if r.Config != nil {
		watchNamespace := r.Config.WatchNamespace()
		if watchNamespace != "" && namespace == watchNamespace {
			return true
		}
	}

	// Multi-namespace mode: Check exclusion first (highest priority - overrides everything)
	if isNamespaceExcluded(ctx, r.Reader, namespace) {
		return false
	}

	// Check VA-based tracking (automatic)
	if r.Datastore != nil && r.Datastore.IsNamespaceTracked(namespace) {
		return true
	}

	// Check label-based opt-in (explicit)
	return isNamespaceConfigEnabled(ctx, r.Reader, namespace)
}

// supersededByCurrentName reports whether this ConfigMap should be ignored because
// it carries the PRE-RENAME name while a ConfigMap with the current name also
// exists — and logs the deprecation notice when it is the one being used.
//
// Both names are read so an upgrade cannot silently drop a deployment's scaling
// configuration, but "both" needs an order, and bootstrap ordering alone does not
// give one: after startup these arrive as independent watch events, so whichever
// ConfigMap was edited last would win. A stale copy left behind by an upgrade
// would then overwrite the live one at an arbitrary moment — the worst kind of
// configuration bug, because nothing changed at the time it took effect.
//
// So the rule is positional, not temporal: if the current name exists, it is the
// configuration, and the old one is inert.
func (r *ConfigMapReconciler) supersededByCurrentName(ctx context.Context, cm *corev1.ConfigMap, namespace string) bool {
	logger := log.FromContext(ctx)
	current := config.ScalingPolicyConfigMapName()
	if cm.GetName() != config.LegacyScalingPolicyConfigMapName || current == config.LegacyScalingPolicyConfigMapName {
		return false
	}

	var existing corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Name: current, Namespace: namespace}, &existing)
	if err == nil {
		logger.V(1).Info("Ignoring the pre-rename scaling-policy ConfigMap: one with the current "+
			"name exists and takes precedence. Delete the old one once you have confirmed the "+
			"new one carries everything.",
			"ignoring", cm.GetName(), "using", current, "namespace", namespace)
		return true
	}
	if !apierrors.IsNotFound(err) {
		// Could not tell. Proceed rather than drop the only config we can see.
		logger.V(1).Info("Could not check for a current-named scaling-policy ConfigMap; "+
			"using the pre-rename one", "error", err.Error())
	}

	logger.Info("Reading scaling policy from the pre-rename ConfigMap name. It outgrew "+
		"\"saturation\" — it now carries policy tiers, analyzer definitions, limiters and "+
		"scale-to-zero — so rename it; the old name is still read but will not be forever.",
		"using", config.LegacyScalingPolicyConfigMapName, "rename to", current)
	return false
}

// handleClusterPolicyConfigMap loads limiters and quotas from the policy
// namespace — the namespace a cluster admin owns, which for a namespace-scoped
// install is not the one the controller runs in.
//
// Only the "default" entry's limiters are taken. Cluster policy is a bound on the
// whole install; per-model thresholds and tiers stay where the workload's own
// operator can edit them, because those are tuning, not entitlement.
//
// It returns an error only at startup, when the "default" entry is invalid and
// no earlier cluster policy is in force: see errUnboundedAtStartup.
func (r *ConfigMapReconciler) handleClusterPolicyConfigMap(ctx context.Context, cm *corev1.ConfigMap) error {
	logger := log.FromContext(ctx)

	configs, _, rejected := parseScalingPolicyConfigRejects(cm.Data, logger)
	r.reportUnknownKeys(ctx, cm)
	policy, ok := configs[config.GlobalDefaultsKey]
	if !ok {
		if rerr := rejected[config.GlobalDefaultsKey]; rerr != nil {
			// A default entry that does not parse is a mistake in a bound, not
			// the removal of one. Keep the policy in force, if there is one: a
			// typo must not lift a quota. Only with nothing in force yet (the
			// first load) does the controller run unbounded, said below.
			if previous, had := r.Config.ClusterPolicy(); had {
				r.reportRejectedEntries(ctx, cm, rejected, map[string]bool{config.GlobalDefaultsKey: true}, true)
				r.Status.RecordPolicy(r.Config, cm.GetNamespace(), cm.GetName(), cm.GetResourceVersion(), rerr)
				// reportRejectedEntries has already logged the rejection as an
				// error. This line only says what stays in force.
				logger.Info("Keeping the cluster policy already in force",
					"policyNamespace", cm.GetNamespace(), "configMap", cm.GetName(),
					"limiters", len(previous.Limiters), "limiterTypes", r.Config.EffectiveLimiterTypes().String())
				return nil
			}
			if !r.Config.ConfigMapsBootstrapComplete() {
				r.reportRejectedEntries(ctx, cm, rejected, nil, true)
				r.Status.RecordPolicy(r.Config, cm.GetNamespace(), cm.GetName(), cm.GetResourceVersion(), rerr)
				return errUnboundedAtStartup(cm, rerr)
			}
		}
		// An absent default entry means no bound was declared. That must clear
		// any previous one rather than leave the last-known limiters in place: a
		// cluster admin who deletes a quota expects it to stop applying.
		//
		// An EMPTY policy, not nil, as on deletion and at bootstrap: nil means
		// "not separated" and sends the limiter accessors back to the
		// controller's own ConfigMap, the one the policy namespace exists to take
		// limit-setting away from.
		r.reportRejectedEntries(ctx, cm, rejected, nil, true)
		r.Config.UpdateClusterPolicy(&config.ScalingPolicy{})
		r.Status.RecordPolicy(r.Config, cm.GetNamespace(), cm.GetName(), cm.GetResourceVersion(),
			rejected[config.GlobalDefaultsKey])
		logger.Info("Policy namespace ConfigMap has no usable default entry: no cluster limiters or quotas are in force",
			"policyNamespace", cm.GetNamespace(), "configMap", cm.GetName())
		return nil
	}

	r.reportRejectedEntries(ctx, cm, rejected, nil, true)
	r.Config.UpdateClusterPolicy(&policy)
	r.Status.RecordPolicy(r.Config, cm.GetNamespace(), cm.GetName(), cm.GetResourceVersion(), nil)
	logger.Info("Loaded cluster policy from the policy namespace",
		"policyNamespace", cm.GetNamespace(), "configMap", cm.GetName(),
		"limiters", len(policy.Limiters), "limiterTypes", r.Config.EffectiveLimiterTypes().String())
	return nil
}

// errUnboundedAtStartup is the error that stops startup when the ConfigMap that
// carries the cluster's limiters has an invalid "default" entry and nothing
// earlier is in force to keep. Running on would run without the GPU bound the
// entry declares, and limiters are the safety boundary: refusing to start is
// visible, running unbounded is not. At runtime the previous entry is kept
// instead, so this arises only at startup.
func errUnboundedAtStartup(cm *corev1.ConfigMap, cause error) error {
	return fmt.Errorf("refusing to start unbounded: the %q entry of ConfigMap %s/%s is invalid and no earlier "+
		"entry is in force to keep; fix the entry and restart the controller: %w",
		config.GlobalDefaultsKey, cm.GetNamespace(), cm.GetName(), cause)
}

// reportUnknownKeys makes keys a scaling-policy entry carries but the schema
// does not define visible: a log line at the default level and a Warning
// ConfigEntryUnknownKeys event per entry, and wva_config_unknown_keys for the
// ConfigMap. The entries themselves still apply, with those keys ignored
// (config.DecodeScalingPolicy says why).
func (r *ConfigMapReconciler) reportUnknownKeys(ctx context.Context, cm *corev1.ConfigMap) {
	logger := log.FromContext(ctx)
	total := 0
	for _, key := range slices.Sorted(maps.Keys(cm.Data)) {
		unknown := config.UnknownScalingPolicyKeys(cm.Data[key])
		if len(unknown) == 0 {
			continue
		}
		total += len(unknown)
		logger.Info("WARNING: scaling-policy entry has keys the schema does not define; they are ignored and "+
			"the rest of the entry applies. Check for a misspelling.",
			"namespace", cm.GetNamespace(), "configMap", cm.GetName(), "entry", key, "unknownKeys", unknown)
		if r.Recorder != nil {
			r.Recorder.Eventf(cm, corev1.EventTypeWarning, constants.K8SEventConfigEntryUnknownKeys,
				"entry %q: unknown keys %s are ignored; the rest of the entry applies", key, strings.Join(unknown, ", "))
		}
	}
	metrics.SetConfigUnknownKeys(cm.GetNamespace(), cm.GetName(), total)
}

// keepPreviousEntries puts back into configs, for each rejected key, the entry
// previous returns for it, and reports which keys it kept that way.
func keepPreviousEntries(configs config.ScalingPolicySet, rejected map[string]error,
	previous func(key string) (config.ScalingPolicy, bool)) map[string]bool {
	kept := map[string]bool{}
	for key := range rejected {
		if old, ok := previous(key); ok {
			configs[key] = old
			kept[key] = true
		}
	}
	return kept
}

// reportRejectedEntries makes rejected scaling-policy entries visible beyond the
// error log: a Warning event on the ConfigMap per rejected key, saying whether
// the previous entry stays in force, and wva_config_rejected_entries for the
// ConfigMap, which returns to 0 once every entry parses.
//
// boundSource says cm is the ConfigMap the cluster's limiters are read from.
// Its "default" entry is the one an invalid value stops startup over
// (errUnboundedAtStartup), so a rejected default there is reported as such:
// at startup the controller is refusing to start, and at runtime, where the
// previous entry is kept, it will refuse on its next restart. The log line
// carries the same text as the event, because at startup the process exits
// right after and the event may never be sent.
func (r *ConfigMapReconciler) reportRejectedEntries(ctx context.Context, cm *corev1.ConfigMap,
	rejected map[string]error, kept map[string]bool, boundSource bool) {
	metrics.SetConfigRejectedEntries(cm.GetNamespace(), cm.GetName(), len(rejected))
	if len(rejected) == 0 {
		return
	}
	logger := log.FromContext(ctx)
	for _, key := range slices.Sorted(maps.Keys(rejected)) {
		outcome := rejectedEntryOutcome(key, kept[key], boundSource, r.Config.ConfigMapsBootstrapComplete())
		msg := fmt.Sprintf("entry %q rejected (%v); %s", key, rejected[key], outcome)
		if boundSource && key == config.GlobalDefaultsKey {
			logger.Error(rejected[key], "Scaling-policy entry rejected: "+msg,
				"namespace", cm.GetNamespace(), "configMap", cm.GetName(), "entry", key)
		} else {
			logger.Info("WARNING: scaling-policy entry rejected: "+msg,
				"namespace", cm.GetNamespace(), "configMap", cm.GetName(), "entry", key)
		}
		if r.Recorder != nil {
			r.Recorder.Event(cm, corev1.EventTypeWarning, constants.K8SEventConfigEntryRejected, msg)
		}
	}
}

// rejectedEntryOutcome says what a rejected entry under key means for the
// controller. See reportRejectedEntries for boundSource.
func rejectedEntryOutcome(key string, kept, boundSource, bootstrapComplete bool) string {
	outcome := "there is no previous entry under this key, so it is not in force"
	if kept {
		outcome = "the previous entry under this key stays in force"
	}
	if !boundSource || key != config.GlobalDefaultsKey {
		return outcome
	}
	if !bootstrapComplete {
		if kept {
			return outcome
		}
		return "the controller refuses to start while the bounding \"default\" entry is invalid"
	}
	return outcome + "; the controller refuses to start while the bounding \"default\" entry is invalid, " +
		"so on its next restart it will refuse to start until the entry is fixed"
}

// handleScalingPolicyConfigMap handles updates to the scaling-policy ConfigMap.
// Supports both global and namespace-local ConfigMaps.
//
// It returns an error only at startup, when this is the ConfigMap the cluster's
// limiters are read from and its "default" entry is invalid with no earlier
// entry to keep (errUnboundedAtStartup). Everything else in the ConfigMap is
// still applied first, so what is recorded matches what was read.
func (r *ConfigMapReconciler) handleScalingPolicyConfigMap(ctx context.Context, cm *corev1.ConfigMap, namespace string, isGlobal bool) error {
	logger := log.FromContext(ctx)

	if r.supersededByCurrentName(ctx, cm, namespace) {
		return nil
	}

	// Parse scaling policy entries
	configs, count, rejected := parseScalingPolicyConfigRejects(cm.Data, logger)
	// The surviving entries replace the live set wholesale, so a rejected
	// key would otherwise vanish: a bad "default" took every limiter and tuned
	// threshold with it. The entry previously in force under that key stays.
	setNamespace := namespace
	if isGlobal {
		setNamespace = ""
	}
	kept := keepPreviousEntries(configs, rejected, func(key string) (config.ScalingPolicy, bool) {
		return r.Config.ScalingPolicyEntry(setNamespace, key)
	})
	r.reportRejectedEntries(ctx, cm, rejected, kept, isGlobal && !r.Config.PolicyNamespaceIsSeparate())
	r.reportUnknownKeys(ctx, cm)

	// Only the ConfigMap limiters are read from can leave the controller
	// unbounded: the global one while no policy namespace is separate.
	var startupErr error
	if rerr := rejected[config.GlobalDefaultsKey]; rerr != nil && !kept[config.GlobalDefaultsKey] &&
		isGlobal && !r.Config.PolicyNamespaceIsSeparate() && !r.Config.ConfigMapsBootstrapComplete() {
		startupErr = errUnboundedAtStartup(cm, rerr)
	}

	// Update global or namespace-local config
	if isGlobal {
		// When cluster policy is sourced elsewhere, limiters here carry no
		// authority — otherwise whoever can write the controller's namespace could
		// raise their own quota, which is the entire thing WVA_POLICY_NAMESPACE
		// exists to prevent. Say so rather than dropping it silently: a limiter
		// block that reads as enforcing and enforces nothing is the worst outcome.
		if r.Config.PolicyNamespaceIsSeparate() {
			for key, satConfig := range configs {
				if len(satConfig.Limiters) > 0 {
					logger.Info("Ignoring limiters in the controller's own ConfigMap: cluster policy "+
						"is read from the policy namespace, which this deployment does not own. "+
						"Ask a cluster admin to change it there.",
						"entry", key, "policyNamespace", r.Config.PolicyNamespace(),
						"systemNamespace", config.SystemNamespace())
				}
			}
		}
		r.Config.UpdateScalingPolicyConfig(configs)
		// This ConfigMap is the cluster policy only when no policy namespace is
		// separate; otherwise that one's version is the one recorded, and this
		// edit cannot move the limiter or the optimizer, which are read there.
		if r.Config.PolicyNamespaceIsSeparate() {
			r.Status.ObserveConfig(r.Config)
		} else {
			r.Status.RecordPolicy(r.Config, namespace, cm.GetName(), cm.GetResourceVersion(),
				rejected[config.GlobalDefaultsKey])
		}
		logger.Info("Updated global scaling policy from ConfigMap", "entries", count)
		r.warnIfThroughputRegistrationDiverged(logger, cm)
	} else {
		// Limiters and quotas are CLUSTER-scope: EffectiveLimiterTypes reads them only
		// from the system namespace's ConfigMap, so a tenant cannot grant themselves
		// capacity by declaring a limiter — or raise a quota by declaring a looser
		// one — in their own namespace. That is the point, but silently dropping the
		// block is the same trap enableLimiter was: the config reads as bounded and
		// nothing is bounding it. Say so.
		for key, satConfig := range configs {
			if len(satConfig.Limiters) > 0 {
				logger.Info("Ignoring limiters in a namespace-local saturation ConfigMap: "+
					"limiters and quotas are cluster-scope and are read only from the "+
					"system namespace's ConfigMap. Move this block there, or it has no effect.",
					"namespace", namespace, "entry", key, "systemNamespace", config.SystemNamespace())
			}
			// Same scope as limiters, for a different reason: analyzer definitions
			// are PromQL run against the shared Prometheus, and the analyzer
			// registry is process-wide, so two namespaces defining the same name
			// would collide. A per-namespace WVA install can still declare its own,
			// because that install's own namespace IS its system namespace.
			if len(satConfig.AnalyzerDefinitions) > 0 {
				logger.Info("Ignoring analyzerDefinitions in a namespace-local saturation ConfigMap: "+
					"analyzer definitions are cluster-scope and are read only from the system "+
					"namespace's ConfigMap. A namespace may still SELECT and weight them in its "+
					"policies.",
					"namespace", namespace, "entry", key, "systemNamespace", config.SystemNamespace())
			}
		}
		r.Config.UpdateScalingPolicyConfigForNamespace(namespace, configs)
		logger.Info("Updated namespace-local saturation config from ConfigMap", "namespace", namespace, "entries", count)
	}
	return startupErr
}

// warnIfThroughputRegistrationDiverged compares the live global config's
// throughput-analyzer enablement against the registration decision frozen at
// controller startup. Analyzer registration cannot change without a restart,
// so a divergence means this ConfigMap edit is silently inert; it emits a
// Warning event on cm plus a log line telling the operator to restart.
//
// It runs only after the initial ConfigMap bootstrap has completed. The frozen
// decision (ThroughputRegistered) is captured just after bootstrap returns
// (cmd/main.go); during the synchronous bootstrap pass it is still its zero
// value, so comparing against it would emit a spurious "restart required"
// warning on every healthy startup. The manager — and therefore all runtime
// reconciles — starts only after the decision is frozen, so gating on bootstrap
// completion leaves genuine runtime edits fully covered.
//
// Namespace-local config does not participate: startup registration reads only
// the global saturation config, so a namespace-local edit can never cause this
// kind of drift.
func (r *ConfigMapReconciler) warnIfThroughputRegistrationDiverged(logger logr.Logger, cm *corev1.ConfigMap) {
	if !r.Config.ConfigMapsBootstrapComplete() {
		// Bootstrap pass: the registration decision has not been frozen yet.
		return
	}

	want := r.Config.ThroughputAnalyzerEnabled()
	if want == r.ThroughputRegistered {
		return
	}

	logger.Info("Throughput analyzer registration diverged from live config; controller restart required to apply",
		"wantEnabled", want, "registered", r.ThroughputRegistered, "configMap", cm.Name)
	if r.Recorder != nil {
		msg := fmt.Sprintf(
			"Throughput analyzer enablement in config (%t) differs from the registration "+
				"frozen at controller startup (%t); analyzer registration cannot change at "+
				"runtime. Restart the wva-controller-manager to apply.",
			want, r.ThroughputRegistered)
		r.Recorder.Event(cm, corev1.EventTypeWarning, constants.K8SEventThroughputAnalyzerRestartRequired, msg)
	}
}
