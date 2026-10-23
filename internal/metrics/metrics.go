package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	"github.com/prometheus/client_golang/prometheus"
)

// ControllerInstanceEnvVar is the environment variable name for controller instance label
const ControllerInstanceEnvVar = "CONTROLLER_INSTANCE"

// replicaSeriesAccel tracks the last accelerator_type label emitted for each VA
// (keyed by variant/namespace) on the replica scaling gauges. It lets
// EmitReplicaMetrics evict a stale series only when the accelerator label
// actually changes, instead of delete-and-reset every cycle (which would expose
// a zero-value gap to a concurrent scrape of the scaling signal).
//
// TODO: entries are not pruned on VariantAutoscaling deletion, so the map grows
// by one small entry per (variant, namespace) ever seen. This matches the
// pre-existing behavior of the gauge series themselves (also not deleted on VA
// removal — they expire via Prometheus staleness). Wire a deletion hook if/when
// per-VA metric cleanup is added.
var (
	replicaSeriesMu    sync.Mutex
	replicaSeriesAccel = map[string]string{}
)

// replicaSeriesKey identifies a VA's replica-gauge series independent of
// accelerator_type. controllerInstance is process-global (set once at
// InitMetrics) so it is intentionally not part of the key.
func replicaSeriesKey(variantName, namespace string) string {
	return variantName + "\x00" + namespace
}

var (
	replicaScalingTotal  *prometheus.CounterVec
	desiredReplicas      *prometheus.GaugeVec
	unattributedGPUs     *prometheus.GaugeVec
	unmeasuredQueue      *prometheus.GaugeVec
	variantAtMaxReplicas *prometheus.GaugeVec
	modelScalingBlocked  *prometheus.GaugeVec
	modelReplicas        *prometheus.GaugeVec
	currentReplicas      *prometheus.GaugeVec
	desiredRatio         *prometheus.GaugeVec
	errorsTotal          *prometheus.CounterVec

	optimizationDuration        *prometheus.HistogramVec
	wakeDuration                *prometheus.HistogramVec
	replicaStartSeconds         *prometheus.HistogramVec
	replicaStartSecondsEstimate *prometheus.GaugeVec
	warmPoolFreePods            *prometheus.GaugeVec
	warmPoolBorrows             *prometheus.CounterVec
	warmPoolBridge              *prometheus.HistogramVec
	modelsProcessedGauge        *prometheus.GaugeVec

	// pipeline stage visibility metrics
	decisionsLimitedTotal               *prometheus.CounterVec
	availableGpus                       *prometheus.GaugeVec
	gpuDiscoveryUp                      *prometheus.GaugeVec
	sfzQueueFallbackActive              *prometheus.GaugeVec
	constraintProviderSkipped           *prometheus.GaugeVec
	sfzWakeUnchecked                    *prometheus.CounterVec
	nodeAccessDenied                    *prometheus.GaugeVec
	policyNamespaceUnverified           *prometheus.GaugeVec
	configRejectedEntries               *prometheus.GaugeVec
	configUnknownKeys                   *prometheus.GaugeVec
	configIgnoredFields                 *prometheus.GaugeVec
	podAccessDenied                     *prometheus.GaugeVec
	watchNamespaceUnreadable            *prometheus.GaugeVec
	enforcerModificationsTotal          *prometheus.CounterVec
	optimizerActive                     *prometheus.GaugeVec
	configInfoGauge                     *prometheus.GaugeVec
	configOptimizationIntervalSecsGauge *prometheus.GaugeVec
	metricsCollectionDuration           *prometheus.HistogramVec
	metricsCollectionErrors             *prometheus.CounterVec
	metricsPodsDiscovered               *prometheus.GaugeVec
	metricsFreshnessStatus              *prometheus.GaugeVec
	podMappingMissTotal                 *prometheus.CounterVec
	scalerUndecidedAnswersTotal         *prometheus.CounterVec
	scalerPanicsTotal                   *prometheus.CounterVec

	// Saturation and capacity metrics
	saturationUtilization *prometheus.GaugeVec
	spareCapacity         *prometheus.GaugeVec
	requiredCapacity      *prometheus.GaugeVec
	kvCacheTokensUsed     *prometheus.GaugeVec
	kvCacheTokensCapacity *prometheus.GaugeVec
	saturationMetricsUp   *prometheus.GaugeVec

	// Per-analyzer (demand, target) signals — the common D/P contract exposed so
	// KEDA/HPA and dashboards can consume every analyzer's reasoning.
	analyzerDemand           *prometheus.GaugeVec
	analyzerTarget           *prometheus.GaugeVec
	engineConfig             *prometheus.GaugeVec
	learnedStateBorrowed     *prometheus.GaugeVec
	learnedStateRelaxed      *prometheus.GaugeVec
	learnedStateK2Source     *prometheus.GaugeVec
	learnedStateK2Age        *prometheus.GaugeVec
	learnedStateRestores     *prometheus.CounterVec
	learnedStateErrors       *prometheus.CounterVec
	learnedStateBytes        prometheus.Gauge
	learnedStateBudget       prometheus.Gauge
	learnedStateDropped      prometheus.Gauge
	analyzerObservedReplicas *prometheus.GaugeVec

	// controllerInstance stores the optional controller instance identifier.
	// When set, it's added as a label to all emitted metrics.
	controllerInstance string
)

// GetControllerInstance returns the configured controller instance label value
// Returns empty string if not configured
func GetControllerInstance() string {
	return controllerInstance
}

// InitMetrics registers all custom metrics with the provided registry.
// This function should be called once during application startup from main().
// It reads CONTROLLER_INSTANCE from the environment to optionally add
// controller instance isolation labels to all emitted metrics.
func InitMetrics(registry prometheus.Registerer) error {
	// Read controller instance from environment
	controllerInstance = os.Getenv(ControllerInstanceEnvVar)

	// Fresh registry => fresh gauges; drop any stale replica-series tracking so
	// it never references series from a previous registry.
	replicaSeriesMu.Lock()
	replicaSeriesAccel = map[string]string{}
	replicaSeriesMu.Unlock()

	// Build label sets based on whether controller_instance is configured.
	// Existing replica metrics (baseLabels, scalingLabels) are unchanged.
	// New saturation metrics carry an additional model_name label so dashboards
	// can group/filter by the model a variant serves.
	baseLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelAcceleratorType}
	scalingLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelDirection, constants.LabelReason}
	errorLabels := []string{constants.LabelComponent, constants.LabelErrorType}
	// satAccelLabels: per-variant per-accelerator saturation metrics.
	satAccelLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName, constants.LabelAcceleratorType}
	// satModelLabels: per-variant model-level saturation metrics (no accelerator_type).
	satModelLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName}
	// requiredCapacityLabels: satModelLabels + "unit", naming the continuous
	// token magnitude the wva_required_capacity gauge carries.
	requiredCapacityLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName, constants.LabelUnit}
	// satFreshnessLabels: smallest cardinality shared across the five
	// saturation/capacity gauges. Freshness is a per-VA property, so
	// model_name / accelerator_type / unit don't need to be on this series.
	satFreshnessLabels := []string{constants.LabelVariantName, constants.LabelNamespace}
	// analyzerDemandLabels: per-analyzer demand D, per model instance and role.
	analyzerDemandLabels := []string{constants.LabelAnalyzerName, constants.LabelNamespace, constants.LabelModelName, constants.LabelRole}
	// unmeasuredQueueLabels: model-level, since the whole point is that no
	// variant could be attributed to carry it.
	unmeasuredQueueLabels := []string{constants.LabelNamespace, constants.LabelModelName}
	// Per-variant, unlike the model-level gauge above: the ceiling is configured
	// on the ScaledObject, so one variant of a model can be pinned while a
	// sibling has headroom.
	variantAtMaxLabels := []string{constants.LabelNamespace, constants.LabelVariantName}
	// modelScalingBlockedLabels: model-level, because reaching zero is a property
	// of the model — one variant with a floor keeps the whole model up, so there
	// is no per-variant answer to give. `reason` is a closed enum, so it costs a
	// bounded number of series rather than a metric per condition.
	modelScalingBlockedLabels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelReason}
	// modelReplicasLabels: model_name is deliberately the only identity beyond
	// namespace. This series exists to be joined against EPP metrics, which carry
	// model_name and no namespace; adding variant_name or accelerator_type would
	// put it back on the per-variant side of the join it exists to bridge.
	modelReplicasLabels := []string{constants.LabelNamespace, constants.LabelModelName}
	// analyzerTargetLabels: per-analyzer per-replica target P, per variant.
	analyzerTargetLabels := []string{constants.LabelAnalyzerName, constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName}
	// learnedBorrowLabels: one series per variant and borrowed quantity.
	learnedBorrowLabels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName,
		constants.LabelQuantity, constants.LabelDonor, constants.LabelDonorNamespace}
	learnedRelaxedLabels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName}
	// learnedK2Labels: one series per variant; role and source are
	// determined by the variant this cycle, so they add no cardinality.
	learnedK2Labels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName,
		constants.LabelRole, constants.LabelSource}
	// engineConfigLabels: the key learned state is filed under -- model,
	// accelerator, GPUs per replica, fingerprint -- followed by the engine
	// flags the fingerprint hashes.
	//
	// Wide, and deliberately only one series per variant: every flag label is
	// functionally determined by the variant, so they add width and not
	// cardinality. The flags ride on this series rather than on a separate
	// fingerprint-keyed info metric because that join is many-to-one -- two
	// models can share a configuration -- and a reader of the info series
	// could not then tell which variant a row describes.
	engineConfigLabels := append([]string{
		constants.LabelNamespace,
		constants.LabelModelName,
		constants.LabelVariantName,
		constants.LabelAccelerator,
		constants.LabelGPUsPerReplica,
		constants.LabelFingerprint,
		constants.LabelFingerprintVersion,
	}, constants.EngineConfigFlagLabels...)

	if controllerInstance != "" {
		baseLabels = append(baseLabels, constants.LabelControllerInstance)
		scalingLabels = append(scalingLabels, constants.LabelControllerInstance)
		errorLabels = append(errorLabels, constants.LabelControllerInstance)
		satAccelLabels = append(satAccelLabels, constants.LabelControllerInstance)
		satModelLabels = append(satModelLabels, constants.LabelControllerInstance)
		requiredCapacityLabels = append(requiredCapacityLabels, constants.LabelControllerInstance)
		satFreshnessLabels = append(satFreshnessLabels, constants.LabelControllerInstance)
		analyzerDemandLabels = append(analyzerDemandLabels, constants.LabelControllerInstance)
		analyzerTargetLabels = append(analyzerTargetLabels, constants.LabelControllerInstance)
		engineConfigLabels = append(engineConfigLabels, constants.LabelControllerInstance)
		learnedBorrowLabels = append(learnedBorrowLabels, constants.LabelControllerInstance)
		learnedRelaxedLabels = append(learnedRelaxedLabels, constants.LabelControllerInstance)
		learnedK2Labels = append(learnedK2Labels, constants.LabelControllerInstance)
		unmeasuredQueueLabels = append(unmeasuredQueueLabels, constants.LabelControllerInstance)
		variantAtMaxLabels = append(variantAtMaxLabels, constants.LabelControllerInstance)
		modelScalingBlockedLabels = append(modelScalingBlockedLabels, constants.LabelControllerInstance)
		modelReplicasLabels = append(modelReplicasLabels, constants.LabelControllerInstance)
	}

	replicaScalingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAReplicaScalingTotal,
			Help: "Total number of replica scaling operations",
		},
		scalingLabels,
	)
	desiredReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVADesiredReplicas,
			Help: "Desired number of replicas for each variant",
		},
		baseLabels,
	)
	currentReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVACurrentReplicas,
			Help: "Current number of replicas for each variant",
		},
		baseLabels,
	)
	desiredRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVADesiredRatio,
			Help: "Ratio of the desired number of replicas and the current number of replicas for each variant",
		},
		baseLabels,
	)
	errorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAErrorsTotal,
			Help: "Total number of errors by component",
		},
		errorLabels,
	)
	saturationUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVASaturationUtilization,
			Help: "Per-variant utilization ratio from saturation analysis: TotalDemand / TotalCapacity from the analyzer result. Unbounded above — a value > 1.0 means demand exceeds supply.",
		},
		satAccelLabels,
	)
	spareCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVASpareCapacity,
			Help: fmt.Sprintf("Spare capacity; >0 indicates safe scale-down headroom (per-role for P/D-disaggregated models, model-level otherwise). Carries %s=%q (shared with wva_required_capacity): the token surplus max(0, TotalSupply - TotalDemand/scaleDownBoundary).", constants.LabelUnit, constants.UnitContinuous),
		},
		requiredCapacityLabels,
	)
	requiredCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVARequiredCapacity,
			Help: fmt.Sprintf("Required capacity; >0 indicates scale-up needed (per-role for P/D-disaggregated models, model-level otherwise). Carries %s=%q: the token demand from the analyzer.", constants.LabelUnit, constants.UnitContinuous),
		},
		requiredCapacityLabels,
	)
	kvCacheTokensUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAKvCacheTokensUsed,
			Help: "Total KV cache tokens currently in use across all replicas of a variant (sum of per-replica TokensInUse).",
		},
		satModelLabels,
	)
	kvCacheTokensCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAKvCacheTokensCapacity,
			Help: "Total KV cache token capacity across all replicas of a variant (sum of per-replica TotalKvCapacityTokens).",
		},
		satModelLabels,
	)
	saturationMetricsUp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVASaturationMetricsUp,
			Help: "Per-VA freshness signal for the saturation/capacity gauges: 1.0 in cycles where the optimizer produced a fresh decision for the variant, 0.0 in cycles where the analyzer was aware of the variant but did not refresh those gauges. Pairs with wva_saturation_utilization, wva_spare_capacity, wva_required_capacity, wva_kv_cache_tokens_used, and wva_kv_cache_tokens_capacity so dashboards can gate alerts on this gauge instead of relying on Prometheus' implicit staleness marker.",
		},
		satFreshnessLabels,
	)
	analyzerDemand = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAAnalyzerDemand,
			Help: "Per-analyzer demand D (the total measured signal) for a model instance, in the analyzer's own unit. Role is empty for non-disaggregated models. Paired with wva_analyzer_target as the D/P contract every analyzer exposes.",
		},
		analyzerDemandLabels,
	)
	learnedStateBorrowed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVALearnedStateBorrowed,
			Help: "1 while a variant is priced from a figure a sibling variant learned: quantity=k2 is a measured k2 lower than its own capacity (P2-borrow), quantity=itl-line a shared ITL line. donor and donor_namespace name the sibling. Absent when the variant uses only its own figures.",
		},
		learnedBorrowLabels,
	)
	learnedStateRelaxed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVALearnedStateBorrowRelaxed,
			Help: "How far a variant's k2 borrow has relaxed back to its own figure: 0 at full strength (the first 30 minutes of serving), rising toward 1 over the next 6 hours of serving; an idle, parked or mid-rollout variant holds its place. Present only while the variant borrows k2: when the relaxation completes the borrow ends, and this series goes with it, so it never reads 1.",
		},
		learnedRelaxedLabels,
	)
	learnedStateK2Source = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVALearnedStateK2Source,
			Help: "1 for the source of the k2 a variant is priced on, as the cycle log names it: P1-obs (measured this cycle), P2-hist (the variant's own earlier measurement), P2-borrow (a sibling's, lower than the variant's own), P2-prior (the variant's own, measured under engine arguments it no longer runs, lower than what it would get now), P3-k2 (derived from engine flags), P4-k1 (no k2: the KV cache, or the stored estimate for an engine that publishes no cache config) or P1-obs-unadmitted (a saturated warm-pool bridge, or a saturated Ready replica of a variant mid-rollout, priced at its reading for one cycle, never recorded); k1-bound when the k2 is at or above k1, so the KV cache prices the replica. Taken from the lower-median replica, the one the cycle log's reason names. Absent when no replica of the variant reported this cycle.",
		},
		learnedK2Labels,
	)
	learnedStateK2Age = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVALearnedStateK2AgeSeconds,
			Help: "Seconds since the k2 a variant is priced on was last measured -- written, not read: a figure in use every cycle is read every cycle and still ages. The variant's own window for source P1-obs and P2-hist, the donor's for P2-borrow, the window the previous figure came from for P2-prior; absent for P3-k2, P4-k1, P1-obs-unadmitted and k1-bound, where no measured k2 prices the replica. Keeps its last value through a cycle whose collection or analysis failed.",
		},
		learnedK2Labels,
	)
	learnedStateRestores = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVALearnedStateRestoresTotal,
			Help: "Variants whose restored learned state was checked against their engine configuration after a controller restart (LEARNED_STATE_SNAPSHOT), by outcome: whole (the configuration matched; everything installed), args (the arguments matched on another engine image; everything but the ITL baseline installed), prior (the arguments differed; only the k2 and throughput windows installed, as lower-only figures and a hold-only service rate), incomplete (the configuration could not be read completely; only the k2 and throughput windows installed, as the variant's own under the same unread configuration, and otherwise as lower-only figures and a hold-only service rate; nothing when the document recorded no arguments) or differs (a document from before the arguments were recorded, whose configuration differs; only the k2 windows installed, as lower-only figures).",
		},
		[]string{constants.LabelOutcome},
	)
	learnedStateErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVALearnedStateSnapshotErrorsTotal,
			Help: "Failed reads and writes of the learned-state snapshot ConfigMap (LEARNED_STATE_SNAPSHOT), by operation. A failed write is typically Forbidden -- the wva-learned-state-role or its binding missing -- and keeps nothing learned since for the next restart. A failed read is an API error, or a snapshot that cannot be parsed (counted once, then replaced); it blocks every write until a read succeeds.",
		},
		[]string{constants.LabelOperation},
	)
	learnedStateBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: constants.WVALearnedStateSnapshotBytes,
		Help: "Size in bytes of the learned-state snapshot document last written (LEARNED_STATE_SNAPSHOT). Compare with wva_learned_state_snapshot_budget_bytes: at the bound, every write leaves the least recently used entries out.",
	})
	learnedStateBudget = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: constants.WVALearnedStateSnapshotBudgetBytes,
		Help: "The bound, in bytes, the learned-state snapshot document last written had to fit: the smaller of 700 KiB and 900 KiB less what other versions' documents take of the ConfigMap. 0 until the first write.",
	})
	learnedStateDropped = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: constants.WVALearnedStateSnapshotDroppedEntries,
		Help: "How many entries the last learned-state snapshot write left out to fit its bound, least recently used first: windows of variants idle longest, which a restart then prices at derived or k1 until they saturate again. 0 is the normal case.",
	})
	for _, o := range LearnedStateRestoreOutcomes {
		learnedStateRestores.WithLabelValues(o)
	}
	for _, o := range []string{LearnedStateOpRead, LearnedStateOpWrite} {
		learnedStateErrors.WithLabelValues(o)
	}
	engineConfig = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAEngineConfig,
			Help: "Always 1. Labels carry the engine-configuration fingerprint a variant runs and the launch flags it hashes, so a learned figure keyed on that fingerprint can be read back to the configuration it describes. The unresolved label names any flag the controller could not read, in which case the digest is built partly from defaults and is not used as a key for ITL-line sharing -- though the cross-variant capacity estimate does not consult this digest and is not gated on it; empty is the good case.",
		},
		engineConfigLabels,
	)
	analyzerObservedReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAAnalyzerObservedReplicas,
			Help: "Replicas of a variant that reported metrics in the analyzer's last cycle and were attributed to it, not capped at the scale target. Compare with the target's READY replicas (not wva_current_replicas, which counts pods still starting): lower means a pod's metrics were missing from the cycle, higher means replicas are serving that the scale target does not own; 0 means nothing reported, or the analyzer does not read per-replica metrics.",
		},
		analyzerTargetLabels,
	)
	analyzerTarget = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAAnalyzerTarget,
			Help: "Per-analyzer per-replica target P — the amount of demand one replica of a variant can serve, in the same unit as wva_analyzer_demand, so desired = ceil(demand / target).",
		},
		analyzerTargetLabels,
	)

	optimizationDurationLabels := []string{constants.LabelStatus}
	if controllerInstance != "" {
		optimizationDurationLabels = append(optimizationDurationLabels, constants.LabelControllerInstance)
	}
	optimizationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    constants.WVAOptimizationDurationSeconds,
			Help:    "Duration of optimization loop cycles in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		optimizationDurationLabels,
	)
	replicaStartLabels := []string{constants.LabelVariantName, constants.LabelNamespace}
	// Declared only when it is set, exactly as modelReplicasLabels does it.
	// variantSeriesLabels ADDS controller_instance when the process has one, and
	// a With() carrying a label the collector did not declare panics -- so the
	// declaration and the label set have to agree on this conditionally.
	if controllerInstance != "" {
		replicaStartLabels = append(replicaStartLabels, constants.LabelControllerInstance)
	}
	replicaStartSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: constants.WVAReplicaStartSeconds,
			Help: "Time from a replica Pod being created to its Ready condition",
			// Spread over both modes a start has, like the wake histogram beside
			// it: a small model on a warm node lands near 60-80 s (run T measured
			// 67 and 82), while a large MoE with a cold JIT cache is several
			// hundred. Buckets bunched at either end would collapse the very
			// distinction the demand floor projects over.
			Buckets: []float64{5, 15, 30, 45, 60, 75, 90, 120, 180, 300, 600, 900},
		},
		replicaStartLabels,
	)
	replicaStartSecondsEstimate = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAReplicaStartSecondsEstimate,
			Help: "Replica start time the controller is sizing with, per variant",
		},
		append(append([]string{}, replicaStartLabels...), constants.LabelStartSource),
	)
	wakeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: constants.WVAScaleFromZeroWakeSeconds,
			Help: "Time from publishing a scale-from-zero activation to the model being observed serving",
			// Spread deliberately across both modes an FMA wake has. A warm bind
			// lands near 2s and a cold model load near 50-80s, so buckets bunched at
			// either end would collapse the distinction this metric exists to show.
			Buckets: []float64{1, 2, 3, 5, 8, 15, 30, 45, 60, 90, 120, 300},
		},
		[]string{constants.LabelNamespace, constants.LabelModelName},
	)
	warmPoolFreePods = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAWarmPoolFreePods,
			Help: "Warm-pool Pods that can serve a wake now (every instance asleep, none loading)",
		},
		[]string{constants.LabelPool},
	)
	warmPoolBorrows = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAWarmPoolBorrowTotal,
			Help: "Attempts to cover a scale-up from the warm pool, by outcome: hit, blocked or miss",
		},
		[]string{constants.LabelNamespace, constants.LabelModelName, constants.LabelOutcome},
	)
	warmPoolBridge = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: constants.WVAWarmPoolBridgeSeconds,
			Help: "How long a borrowed Pod served before the ordinary replicas took over",
			// Spread around the ordinary start time this bridges, measured at
			// 33-37s here, with room above it: a bridge sitting near the hold
			// timeout means the scale-up it covers is failing.
			Buckets: []float64{1, 5, 15, 30, 45, 60, 90, 120, 300},
		},
		[]string{constants.LabelNamespace, constants.LabelModelName},
	)
	modelsProcessedLabels := []string{}
	if controllerInstance != "" {
		modelsProcessedLabels = append(modelsProcessedLabels, constants.LabelControllerInstance)
	}
	modelsProcessedGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAModelsProcessed,
			Help: "Number of models processed in the last optimization cycle",
		},
		modelsProcessedLabels,
	)

	unmeasuredQueue = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAUnmeasuredQueue,
			Help: "Requests queued for a model that WVA has no attributed replica to act on. " +
				"Zero is the normal state, including for an idle model scaled to zero. " +
				"Non-zero means traffic is queueing while the autoscaler is blind to the " +
				"pods serving it — most often an FMA topology whose launcher pods are not " +
				"scraped, or a PodMonitor selecting a port the pods do not declare. " +
				"Sourced from the EPP flow-control queue, so it is reported even when no " +
				"engine pod is scraped at all.",
		},
		unmeasuredQueueLabels,
	)

	variantAtMaxReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAVariantAtMaxReplicas,
			Help: "1 while a variant's target sits on its own maxReplicas ceiling, 0 otherwise. " +
				"Not the same as wva_decisions_limited_total, which means GPUs ran out: this " +
				"means the configured ceiling is the binding constraint, so the remedy is to " +
				"raise maxReplicas rather than add accelerators. On its own it is unremarkable " +
				"— a correctly sized variant sits at its ceiling under load. Alert on it only " +
				"together with utilization above the scale-up threshold, which is demand going " +
				"unserved. Also read it when interpreting a benchmark: a run pinned at its " +
				"ceiling measures the ceiling, not the autoscaler.",
		},
		variantAtMaxLabels,
	)

	modelScalingBlocked = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAModelScalingBlocked,
			Help: "1 for each reason a model cannot reach the scaling state its configuration " +
				"implies. Present only while the reason holds, so absence means nothing is " +
				"blocking. Each reason names a CONTRADICTION rather than a setting: " +
				"scale-to-zero disabled alongside a variant floor is consistent and reports " +
				"nothing, whereas either half permitting zero while the other forbids it leaves " +
				"the operator believing something the cluster will never do. variant-floor and " +
				"policy-forbids-zero cost idle accelerators with no other symptom — the model " +
				"is up and serving, exactly as every other metric says it should be.",
		},
		modelScalingBlockedLabels,
	)

	modelReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAModelReplicas,
			Help: "Replicas currently serving a model, summed across its variants. Exists to be " +
				"JOINED: the per-variant wva_current_replicas carries no model_name and EPP's " +
				"request metrics carry no namespace, so \"this model is parked while requests " +
				"are refused\" cannot be expressed without a model-keyed replica count. Zero " +
				"means nothing is serving the model, which is normal for an idle model that " +
				"scaled to zero and a fault only when demand exists.",
		},
		modelReplicasLabels,
	)

	unattributedGPUs = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAUnattributedGPUs,
			Help: "GPUs held by variants whose accelerator type could not be resolved. " +
				"This usage is charged to no accelerator pool, so every pool over-states " +
				"how much is free by this amount and the wake capacity check can allow a " +
				"placement it should refuse. Any non-zero value is a misconfiguration: set " +
				"a GPU product nodeSelector on the workload, or the accelerator label on " +
				"its scaler.",
		},
		modelsProcessedLabels,
	)

	decisionsLimitedLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelLimiterName}
	if controllerInstance != "" {
		decisionsLimitedLabels = append(decisionsLimitedLabels, constants.LabelControllerInstance)
	}
	decisionsLimitedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVADecisionsLimitedTotal,
			Help: "Total number of decisions limited by the limiter",
		},
		decisionsLimitedLabels,
	)

	availableGpusLabels := []string{constants.LabelAcceleratorVendor, constants.LabelAcceleratorModel, constants.LabelAcceleratorType}
	if controllerInstance != "" {
		availableGpusLabels = append(availableGpusLabels, constants.LabelControllerInstance)
	}
	availableGpus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAAvailableGpus,
			Help: "Number of currently available GPUs when wva_gpu_discovery_up is 1. When wva_gpu_discovery_up is 0, shows the number of GPUs that were available at the last successful discovery.",
		},
		availableGpusLabels,
	)

	gpuDiscoveryUpLabels := []string{}
	if controllerInstance != "" {
		gpuDiscoveryUpLabels = append(gpuDiscoveryUpLabels, constants.LabelControllerInstance)
	}
	gpuDiscoveryUp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAGpuDiscoveryUp,
			Help: "Indicates whether GPU discovery is on or off",
		},
		gpuDiscoveryUpLabels,
	)

	nodeAccessDeniedLabels := []string{}
	if controllerInstance != "" {
		nodeAccessDeniedLabels = append(nodeAccessDeniedLabels, constants.LabelControllerInstance)
	}
	nodeAccessDenied = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVANodeAccessDenied,
			Help: "1 while a configured physical GPU limiter cannot read nodes; every variant then gets no GPU budget and stops scaling up",
		},
		nodeAccessDeniedLabels,
	)

	policyNamespaceUnverifiedLabels := []string{}
	if controllerInstance != "" {
		policyNamespaceUnverifiedLabels = append(policyNamespaceUnverifiedLabels, constants.LabelControllerInstance)
	}
	policyNamespaceUnverified = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAPolicyNamespaceUnverified,
			Help: "1 when limiters and quotas fell back to the controller's own ConfigMap because reading the Namespaces that decide the policy source was Forbidden",
		},
		policyNamespaceUnverifiedLabels,
	)

	podAccessDeniedLabels := []string{}
	if controllerInstance != "" {
		podAccessDeniedLabels = append(podAccessDeniedLabels, constants.LabelControllerInstance)
	}
	podAccessDenied = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAPodAccessDenied,
			Help: "1 while a namespace-scoped controller may not list Pods cluster-wide, so GPU usage counts its own namespace only and other namespaces' GPUs on shared nodes read as free",
		},
		podAccessDeniedLabels,
	)

	watchNamespaceUnreadable = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAWatchNamespaceUnreadable,
			Help: "1 for a watched namespace this namespace-scoped controller may not read; it is left unmanaged while the others run",
		},
		append(append([]string{}, podAccessDeniedLabels...), constants.LabelNamespace, constants.LabelReason),
	)

	configRejectedEntriesLabels := []string{constants.LabelNamespace, "configmap"}
	if controllerInstance != "" {
		configRejectedEntriesLabels = append(configRejectedEntriesLabels, constants.LabelControllerInstance)
	}
	configRejectedEntries = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigRejectedEntries,
			Help: "Entries of a scaling-policy ConfigMap rejected at its last load; a rejected entry keeps the previous one under its key, if any",
		},
		configRejectedEntriesLabels,
	)

	configUnknownKeys = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigUnknownKeys,
			Help: "Keys of a scaling-policy ConfigMap's entries that the schema does not define; ignored, the entries still apply",
		},
		configRejectedEntriesLabels,
	)

	configIgnoredFields = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigIgnoredFields,
			Help: "Fields of a scaling-policy ConfigMap's entries whose value cannot be used; they act as unset, the entries still apply",
		},
		configRejectedEntriesLabels,
	)

	sfzQueueFallbackLabels := []string{"pool"}
	if controllerInstance != "" {
		sfzQueueFallbackLabels = append(sfzQueueFallbackLabels, constants.LabelControllerInstance)
	}
	sfzQueueFallbackActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAScaleFromZeroQueueFallbackActive,
			Help: "1 while scale-from-zero reads the EPP flow-control queue from Prometheus because the direct EPP scrape is failing",
		},
		sfzQueueFallbackLabels,
	)

	constraintProviderSkippedLabels := []string{constants.LabelProvider, constants.LabelReason}
	sfzWakeUncheckedLabels := []string{constants.LabelNamespace}
	if controllerInstance != "" {
		constraintProviderSkippedLabels = append(constraintProviderSkippedLabels, constants.LabelControllerInstance)
		sfzWakeUncheckedLabels = append(sfzWakeUncheckedLabels, constants.LabelControllerInstance)
	}
	constraintProviderSkipped = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConstraintProviderSkipped,
			Help: "1 while a declared GPU limiter is left out of scaling decisions, by provider and reason; the others still bound",
		},
		constraintProviderSkippedLabels,
	)
	sfzWakeUnchecked = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAScaleFromZeroWakeUnchecked,
			Help: "Scale-from-zero wakes published without a GPU capacity check: every declared limiter unavailable and no recent snapshot",
		},
		sfzWakeUncheckedLabels,
	)

	enforcerModificationsLabels := []string{constants.LabelPolicyType}
	if controllerInstance != "" {
		enforcerModificationsLabels = append(enforcerModificationsLabels, constants.LabelControllerInstance)
	}
	enforcerModificationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAEnforcerModificationsTotal,
			Help: "Total number of decision modifications made by the enforcer",
		},
		enforcerModificationsLabels,
	)

	optimizerActiveLabels := []string{constants.LabelOptimizerName}
	if controllerInstance != "" {
		optimizerActiveLabels = append(optimizerActiveLabels, constants.LabelControllerInstance)
	}
	optimizerActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAOptimizerActive,
			Help: "1 for active optimizer, 0 for inactive",
		},
		optimizerActiveLabels,
	)

	// Config info metric with labels
	configInfoLabels := []string{constants.LabelAnalyzerName, constants.LabelLimiterEnabled, constants.LabelScaleToZeroEnabled}
	if controllerInstance != "" {
		configInfoLabels = append(configInfoLabels, constants.LabelControllerInstance)
	}
	configInfoGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigInfo,
			Help: "WVA configuration information (value is always 1)",
		},
		configInfoLabels,
	)

	// Config threshold and interval metrics
	configLabels := []string{}
	if controllerInstance != "" {
		configLabels = append(configLabels, constants.LabelControllerInstance)
	}
	configOptimizationIntervalSecsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigOptimizationIntervalSeconds,
			Help: "Optimization interval in seconds",
		},
		configLabels,
	)

	metricsCollectionDurationLabels := []string{constants.LabelQueryType}
	if controllerInstance != "" {
		metricsCollectionDurationLabels = append(metricsCollectionDurationLabels, constants.LabelControllerInstance)
	}
	metricsCollectionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    constants.WVAMetricsCollectionDurationSeconds,
			Help:    "Duration of metrics collection operations in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		},
		metricsCollectionDurationLabels,
	)

	metricsCollectionErrorsLabels := []string{constants.LabelQueryType, constants.LabelReason}
	if controllerInstance != "" {
		metricsCollectionErrorsLabels = append(metricsCollectionErrorsLabels, constants.LabelControllerInstance)
	}
	metricsCollectionErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAMetricsCollectionErrorsTotal,
			Help: "Total number of metrics collection errors",
		},
		metricsCollectionErrorsLabels,
	)

	metricsPodsDiscoveredLabels := []string{constants.LabelNamespace}
	if controllerInstance != "" {
		metricsPodsDiscoveredLabels = append(metricsPodsDiscoveredLabels, constants.LabelControllerInstance)
	}
	metricsPodsDiscovered = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAMetricsPodsDiscovered,
			Help: "Number of pods discovered for a namespace",
		},
		metricsPodsDiscoveredLabels,
	)

	metricsFreshnessStatusLabels := []string{constants.LabelVariantName, constants.LabelStatus}
	if controllerInstance != "" {
		metricsFreshnessStatusLabels = append(metricsFreshnessStatusLabels, constants.LabelControllerInstance)
	}
	metricsFreshnessStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAMetricsFreshnessStatus,
			Help: "Freshness status of metrics for each variant",
		},
		metricsFreshnessStatusLabels,
	)

	podMappingMissLabels := []string{constants.LabelNamespace, constants.LabelReason}
	if controllerInstance != "" {
		podMappingMissLabels = append(podMappingMissLabels, constants.LabelControllerInstance)
	}
	podMappingMissTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAPodMappingMissTotal,
			Help: "Total number of pods whose metrics could not be attributed to a managed scaler",
		},
		podMappingMissLabels,
	)

	scalerUndecidedLabels := []string{constants.LabelNamespace, constants.LabelReason}
	scalerPanicLabels := []string{constants.LabelMethod}
	if controllerInstance != "" {
		scalerUndecidedLabels = append(scalerUndecidedLabels, constants.LabelControllerInstance)
		scalerPanicLabels = append(scalerPanicLabels, constants.LabelControllerInstance)
	}
	scalerUndecidedAnswersTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAScalerUndecidedAnswersTotal,
			Help: "Total number of KEDA GetMetrics answers given without a WVA decision (the scale target's current replica count)",
		},
		scalerUndecidedLabels,
	)
	scalerPanicsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAScalerPanicsTotal,
			Help: "Total number of panics recovered in the KEDA external scaler's gRPC handlers",
		},
		scalerPanicLabels,
	)

	// Register metrics with the registry
	if err := registry.Register(replicaScalingTotal); err != nil {
		return fmt.Errorf("failed to register replicaScalingTotal metric: %w", err)
	}
	if err := registry.Register(desiredReplicas); err != nil {
		return fmt.Errorf("failed to register desiredReplicas metric: %w", err)
	}
	if err := registry.Register(currentReplicas); err != nil {
		return fmt.Errorf("failed to register currentReplicas metric: %w", err)
	}
	if err := registry.Register(desiredRatio); err != nil {
		return fmt.Errorf("failed to register desiredRatio metric: %w", err)
	}
	if err := registry.Register(errorsTotal); err != nil {
		return fmt.Errorf("failed to register errorsTotal metric: %w", err)
	}
	if err := registry.Register(wakeDuration); err != nil {
		return err
	}
	if err := registry.Register(replicaStartSeconds); err != nil {
		return err
	}
	if err := registry.Register(replicaStartSecondsEstimate); err != nil {
		return err
	}
	if err := registry.Register(warmPoolFreePods); err != nil {
		return err
	}
	if err := registry.Register(warmPoolBorrows); err != nil {
		return err
	}
	if err := registry.Register(warmPoolBridge); err != nil {
		return err
	}
	if err := registry.Register(optimizationDuration); err != nil {
		return fmt.Errorf("failed to register optimizationDuration metric: %w", err)
	}
	if err := registry.Register(unattributedGPUs); err != nil {
		return err
	}
	if err := registry.Register(variantAtMaxReplicas); err != nil {
		return fmt.Errorf("failed to register variantAtMaxReplicas metric: %w", err)
	}
	if err := registry.Register(modelScalingBlocked); err != nil {
		return fmt.Errorf("failed to register modelScalingBlocked metric: %w", err)
	}
	if err := registry.Register(modelReplicas); err != nil {
		return fmt.Errorf("failed to register modelReplicas metric: %w", err)
	}
	if err := registry.Register(unmeasuredQueue); err != nil {
		return fmt.Errorf("failed to register unmeasuredQueue metric: %w", err)
	}
	if err := registry.Register(modelsProcessedGauge); err != nil {
		return fmt.Errorf("failed to register modelsProcessedGauge metric: %w", err)
	}
	if err := registry.Register(decisionsLimitedTotal); err != nil {
		return fmt.Errorf("failed to register decisionsLimitedTotal metric: %w", err)
	}
	if err := registry.Register(availableGpus); err != nil {
		return fmt.Errorf("failed to register availableGpus metric: %w", err)
	}
	if err := registry.Register(gpuDiscoveryUp); err != nil {
		return fmt.Errorf("failed to register gpuDiscoveryUp metric: %w", err)
	}
	if err := registry.Register(nodeAccessDenied); err != nil {
		return fmt.Errorf("failed to register nodeAccessDenied metric: %w", err)
	}
	if err := registry.Register(policyNamespaceUnverified); err != nil {
		return fmt.Errorf("failed to register policyNamespaceUnverified metric: %w", err)
	}
	if err := registry.Register(podAccessDenied); err != nil {
		return fmt.Errorf("failed to register podAccessDenied metric: %w", err)
	}
	if err := registry.Register(watchNamespaceUnreadable); err != nil {
		return fmt.Errorf("failed to register watchNamespaceUnreadable metric: %w", err)
	}
	if err := registry.Register(configRejectedEntries); err != nil {
		return fmt.Errorf("failed to register configRejectedEntries metric: %w", err)
	}
	if err := registry.Register(configUnknownKeys); err != nil {
		return fmt.Errorf("failed to register configUnknownKeys metric: %w", err)
	}
	if err := registry.Register(configIgnoredFields); err != nil {
		return fmt.Errorf("failed to register configIgnoredFields metric: %w", err)
	}
	if err := registry.Register(constraintProviderSkipped); err != nil {
		return fmt.Errorf("failed to register constraintProviderSkipped metric: %w", err)
	}
	if err := registry.Register(sfzWakeUnchecked); err != nil {
		return fmt.Errorf("failed to register sfzWakeUnchecked metric: %w", err)
	}
	if err := registry.Register(sfzQueueFallbackActive); err != nil {
		return fmt.Errorf("failed to register sfzQueueFallbackActive metric: %w", err)
	}
	if err := registry.Register(enforcerModificationsTotal); err != nil {
		return fmt.Errorf("failed to register enforcerModificationsTotal metric: %w", err)
	}
	if err := registry.Register(optimizerActive); err != nil {
		return fmt.Errorf("failed to register optimizerActive metric: %w", err)
	}
	if err := registry.Register(configInfoGauge); err != nil {
		return fmt.Errorf("failed to register configInfoGauge metric: %w", err)
	}
	if err := registry.Register(configOptimizationIntervalSecsGauge); err != nil {
		return fmt.Errorf("failed to register configOptimizationIntervalSecsGauge metric: %w", err)
	}
	if err := registry.Register(metricsCollectionDuration); err != nil {
		return fmt.Errorf("failed to register metricsCollectionDuration metric: %w", err)
	}
	if err := registry.Register(metricsCollectionErrors); err != nil {
		return fmt.Errorf("failed to register metricsCollectionErrors metric: %w", err)
	}
	if err := registry.Register(metricsPodsDiscovered); err != nil {
		return fmt.Errorf("failed to register metricsPodsDiscovered metric: %w", err)
	}
	if err := registry.Register(podMappingMissTotal); err != nil {
		return fmt.Errorf("failed to register podMappingMissTotal metric: %w", err)
	}
	if err := registry.Register(scalerUndecidedAnswersTotal); err != nil {
		return fmt.Errorf("failed to register scalerUndecidedAnswersTotal metric: %w", err)
	}
	if err := registry.Register(scalerPanicsTotal); err != nil {
		return fmt.Errorf("failed to register scalerPanicsTotal metric: %w", err)
	}
	if err := registry.Register(newScalerUndecidedTargetsCollector(controllerInstance)); err != nil {
		return fmt.Errorf("failed to register scalerUndecidedTargets metric: %w", err)
	}
	if err := registry.Register(metricsFreshnessStatus); err != nil {
		return fmt.Errorf("failed to register metricsFreshnessStatus metric: %w", err)
	}
	if err := registry.Register(saturationUtilization); err != nil {
		return fmt.Errorf("failed to register saturationUtilization metric: %w", err)
	}
	if err := registry.Register(spareCapacity); err != nil {
		return fmt.Errorf("failed to register spareCapacity metric: %w", err)
	}
	if err := registry.Register(requiredCapacity); err != nil {
		return fmt.Errorf("failed to register requiredCapacity metric: %w", err)
	}
	if err := registry.Register(kvCacheTokensUsed); err != nil {
		return fmt.Errorf("failed to register kvCacheTokensUsed metric: %w", err)
	}
	if err := registry.Register(kvCacheTokensCapacity); err != nil {
		return fmt.Errorf("failed to register kvCacheTokensCapacity metric: %w", err)
	}
	if err := registry.Register(saturationMetricsUp); err != nil {
		return fmt.Errorf("failed to register saturationMetricsUp metric: %w", err)
	}
	if err := registry.Register(analyzerDemand); err != nil {
		return fmt.Errorf("failed to register analyzerDemand metric: %w", err)
	}
	if err := registry.Register(analyzerObservedReplicas); err != nil {
		return fmt.Errorf("failed to register analyzerObservedReplicas metric: %w", err)
	}
	if err := registry.Register(engineConfig); err != nil {
		return fmt.Errorf("failed to register %s: %w", constants.WVAEngineConfig, err)
	}
	for _, c := range []prometheus.Collector{learnedStateBorrowed, learnedStateRelaxed, learnedStateK2Source, learnedStateK2Age,
		learnedStateRestores, learnedStateErrors, learnedStateBytes, learnedStateBudget, learnedStateDropped} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register a learned-state metric: %w", err)
		}
	}
	if err := registry.Register(analyzerTarget); err != nil {
		return fmt.Errorf("failed to register analyzerTarget metric: %w", err)
	}
	if err := registerUtilizationShareMetrics(registry); err != nil {
		return err
	}
	if err := registerServiceRateMetrics(registry); err != nil {
		return err
	}
	if err := registerSLOMetrics(registry); err != nil {
		return err
	}
	if err := registerQuotaBorrowMetrics(registry); err != nil {
		return err
	}
	if err := registerLoadBalanceMetrics(registry); err != nil {
		return err
	}

	return nil
}

// InitMetricsAndEmitter registers metrics with Prometheus and creates a metrics emitter
// This is a convenience function that handles both registration and emitter creation
func InitMetricsAndEmitter(registry prometheus.Registerer) (*MetricsEmitter, error) {
	if err := InitMetrics(registry); err != nil {
		return nil, err
	}
	return NewMetricsEmitter(), nil
}

// MetricsEmitter handles emission of custom metrics
type MetricsEmitter struct{}

// NewMetricsEmitter creates a new metrics emitter
func NewMetricsEmitter() *MetricsEmitter {
	return &MetricsEmitter{}
}

// EmitReplicaScalingMetrics emits metrics related to replica scaling
func (m *MetricsEmitter) EmitReplicaScalingMetrics(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling, direction domain.SaturationAction, reason domain.DecisionReason) error {
	labels := prometheus.Labels{
		constants.LabelVariantName: va.Name,
		constants.LabelNamespace:   va.Namespace,
		constants.LabelDirection:   string(direction),
		constants.LabelReason:      string(reason),
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	// These operations are local and should never fail, but we handle errors for debugging
	if replicaScalingTotal == nil {
		return errors.New("replicaScalingTotal metric not initialized")
	}

	replicaScalingTotal.With(labels).Inc()
	return nil
}

// ObserveOptimizationDuration records the duration of an optimization cycle with the given status.
// Status should be one of: "success", "error".
func ObserveOptimizationDuration(durationSeconds float64, status string) {
	if optimizationDuration == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelStatus: status}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	optimizationDuration.With(labels).Observe(durationSeconds)
}

// SetModelsProcessed sets the gauge to the number of models processed in the last optimization cycle.
// SetUnattributedGPUs records GPUs held by variants whose accelerator could not
// be resolved. Set every cycle, including to zero, so the series reports the
// current state rather than a high-water mark.
func SetUnattributedGPUs(count int) {
	if unattributedGPUs == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	unattributedGPUs.With(labels).Set(float64(count))
}

// SetUnmeasuredQueue records how many requests are queued for a model that WVA
// has no attributed replica to act on. Call it every cycle, with 0 when the model
// is measurable, so the gauge falls back to zero instead of pinning at the last
// bad value until Prometheus' staleness marker expires.
func SetUnmeasuredQueue(namespace, modelName string, queued int) {
	if unmeasuredQueue == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelName,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	unmeasuredQueue.With(labels).Set(float64(queued))
}

// SetVariantAtMaxReplicas records whether a variant's target is sitting on its
// own maxReplicas ceiling. Call it every cycle for every variant, including with
// false, so the gauge drops back to 0 when the ceiling stops binding rather than
// pinning at 1 until Prometheus' staleness marker expires.
func SetVariantAtMaxReplicas(namespace, variantName string, atMax bool) {
	if variantAtMaxReplicas == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelVariantName: variantName,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	value := 0.0
	if atMax {
		value = 1.0
	}
	variantAtMaxReplicas.With(labels).Set(value)
}

// modelScalingBlockedLabels builds the label set for one (model, reason) series.
func modelScalingBlockedSeries(namespace, modelName, reason string) prometheus.Labels {
	labels := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelName,
		constants.LabelReason:    reason,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	return labels
}

// SetModelScalingBlockedReasons records which of the reasons a producer OWNS
// currently hold for a model: every reason in owned is either set to 1 or
// deleted, and reasons outside owned are left alone.
//
// The ownership split is not ceremony. Two engines write this metric — the
// steady-state enforcer decides the policy reasons, the scale-from-zero loop
// decides the wake reasons — and they run at different rates. A "delete every
// series for this model, then set mine" implementation would have them take turns
// erasing each other's answers, at 10Hz, producing a metric that flaps for
// reasons having nothing to do with the cluster.
//
// Presence is the signal, so a reason that no longer holds is DELETED rather than
// set to 0. Setting it to 0 would satisfy a `== 1` alert but leave a series per
// reason ever seen on every model, forever; deleting keeps the surface
// proportional to what is actually wrong. That is also why callers must invoke
// this every cycle even when nothing is blocking — this call is what clears a
// reason, so skipping it on the healthy path pins the last bad answer.
func SetModelScalingBlockedReasons(namespace, modelName string, owned, active []string) {
	if modelScalingBlocked == nil {
		return
	}
	activeSet := make(map[string]struct{}, len(active))
	for _, reason := range active {
		activeSet[reason] = struct{}{}
	}
	for _, reason := range owned {
		labels := modelScalingBlockedSeries(namespace, modelName, reason)
		if _, on := activeSet[reason]; on {
			modelScalingBlocked.With(labels).Set(1)
			continue
		}
		modelScalingBlocked.Delete(labels)
	}
}

// ClearModelScalingBlocked removes every reason series for a model, whoever set
// them. For a model that has gone away entirely — no producer will run for it
// again, so nothing else can clear it, and its last reason would otherwise alert
// forever about a workload that no longer exists.
func ClearModelScalingBlocked(namespace, modelName string) {
	if modelScalingBlocked == nil {
		return
	}
	match := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelName,
	}
	if controllerInstance != "" {
		match[constants.LabelControllerInstance] = controllerInstance
	}
	modelScalingBlocked.DeletePartialMatch(match)
}

// SetModelReplicas records how many replicas are serving a model in total.
//
// Call it every cycle for every model, INCLUDING with zero. Zero is the whole
// point of the series -- a model at zero while requests are being refused is the
// failure this exists to make expressible -- so a caller that skips the zero case
// removes exactly the sample the alert reads.
func SetModelReplicas(namespace, modelName string, replicas int) {
	if modelReplicas == nil {
		return
	}
	modelReplicas.With(modelSeriesLabels(namespace, modelName)).Set(float64(replicas))
}

// ObserveReplicaStartSeconds records one replica's observed time to Ready.
//
// Once per replica, not once per cycle: the caller keeps the Pods it has already
// counted, so a fleet that stays up does not keep contributing the same sample
// and flatten the distribution onto its longest-lived replica.
func ObserveReplicaStartSeconds(namespace, variantName string, seconds float64) {
	if replicaStartSeconds == nil || !(seconds > 0) {
		return
	}
	replicaStartSeconds.With(variantSeriesLabels(namespace, variantName)).Observe(seconds)
}

// SetReplicaStartSecondsEstimate publishes the start time sizing is using for a
// variant, and whether it was measured or seeded.
//
// Both label values are published for the same variant over its life -- seed
// until the first replica starts under observation, measured after -- so a query
// reading the series without the label sees two. That is intentional: the
// transition is the interesting moment, and collapsing it would hide whether a
// run sized against a measurement at all.
func SetReplicaStartSecondsEstimate(namespace, variantName string, seconds float64, measured bool) {
	if replicaStartSecondsEstimate == nil {
		return
	}
	source := "seed"
	if measured {
		source = "measured"
	}
	labels := variantSeriesLabels(namespace, variantName)
	labels[constants.LabelStartSource] = source
	replicaStartSecondsEstimate.With(labels).Set(seconds)

	// The other source child is DELETED, not left behind. Both values appear for
	// a variant over its life -- seed until the first replica starts, measured
	// after -- and a child is scraped until it is removed, so keeping the stale
	// one makes sum by (variant_name) return seed + measured. A panel or alert
	// written without a source selector would read a variant with a 300 s
	// measured start as 370.
	stale := variantSeriesLabels(namespace, variantName)
	stale[constants.LabelStartSource] = "measured"
	if measured {
		stale[constants.LabelStartSource] = "seed"
	}
	replicaStartSecondsEstimate.Delete(stale)
}

// ObserveWakeDuration records how long a wake-from-zero took.
//
// Called once per completed wake, not once per poll: the caller closes the
// episode as it reports it, so a model that stays up does not keep contributing
// samples and skew the distribution toward whatever its steady state is.
func ObserveWakeDuration(namespace, modelName string, seconds float64) {
	if wakeDuration == nil {
		return
	}
	wakeDuration.With(modelSeriesLabels(namespace, modelName)).Observe(seconds)
}

// SetWarmPoolFreePods records the reserve: Pods that could serve a wake at the
// moment of observation.
func SetWarmPoolFreePods(poolName string, free int) {
	if warmPoolFreePods == nil {
		return
	}
	warmPoolFreePods.With(prometheus.Labels{constants.LabelPool: poolName}).Set(float64(free))
}

// CountWarmPoolBorrow records one attempt to cover a scale-up from the pool.
//
// Outcome is deliberately a label rather than three metrics: hit, blocked and
// miss partition the same event, and separating them into gauges would let a
// reader add up a number that is not a total.
func CountWarmPoolBorrow(namespace, modelName, outcome string) {
	if warmPoolBorrows == nil {
		return
	}
	warmPoolBorrows.With(prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelName,
		constants.LabelOutcome:   outcome,
	}).Inc()
}

// ObserveWarmPoolBridge records how long a borrowed Pod served before handing
// back. Observed once per bridge, when it ends.
func ObserveWarmPoolBridge(namespace, modelName string, seconds float64) {
	if warmPoolBridge == nil {
		return
	}
	warmPoolBridge.With(prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelName,
	}).Observe(seconds)
}

// ClearModelReplicas drops a model's replica series, for a model that has gone
// away. Distinct from setting it to zero, which asserts the model exists and is
// parked -- and would keep the symptom alert live for a deleted workload.
func ClearModelReplicas(namespace, modelName string) {
	if modelReplicas == nil {
		return
	}
	modelReplicas.Delete(modelSeriesLabels(namespace, modelName))
}

// variantSeriesLabels builds the label set for a variant-keyed series.
func variantSeriesLabels(namespace, variantName string) prometheus.Labels {
	labels := prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelVariantName: variantName,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	return labels
}

// modelSeriesLabels builds the label set for a model-keyed series.
func modelSeriesLabels(namespace, modelName string) prometheus.Labels {
	labels := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelName,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	return labels
}

func SetModelsProcessed(count int) {
	if modelsProcessedGauge == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	modelsProcessedGauge.With(labels).Set(float64(count))
}

// EmitReplicaMetrics emits current and desired replica metrics — the scaling
// signal consumed by HPA/KEDA. These gauges are emitted regardless of whether
// the accelerator type is resolved: the signal must never be withheld just
// because the accelerator is unknown. When the type is unresolved the
// accelerator_type label carries the bounded UnresolvedAcceleratorType value
// (never the internal "unknown" sentinel).
//
// The HPA/KEDA query selects a VA by variant_name + namespace, NOT by
// accelerator_type, so scaling must not depend on that label. To keep that true
// across an accelerator transition (e.g. unresolved -> a real type) without
// exposing a gap on the steady-state path, we set the current series first and
// then — only when the accelerator label actually changed since the last emit —
// delete the now-stale prior series. Steady state (no label change) is therefore
// a plain idempotent Set: no gap, no per-cycle churn, exactly one series. Only
// during an accelerator transition do the old and new series briefly coexist
// (set-before-delete), which is preferable to a zero-value gap.
func (m *MetricsEmitter) EmitReplicaMetrics(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling, current, desired int32, acceleratorType string) error {
	// These operations are local and should never fail, but we handle errors for debugging
	if currentReplicas == nil || desiredReplicas == nil || desiredRatio == nil {
		return errors.New("replica metrics not initialized")
	}

	if !constants.IsAcceleratorResolved(acceleratorType) {
		acceleratorType = constants.UnresolvedAcceleratorType
	}

	labelsFor := func(accel string) prometheus.Labels {
		l := prometheus.Labels{
			constants.LabelVariantName:     va.Name,
			constants.LabelNamespace:       va.Namespace,
			constants.LabelAcceleratorType: accel,
		}
		if controllerInstance != "" {
			l[constants.LabelControllerInstance] = controllerInstance
		}
		return l
	}

	// Set the current series first so a concurrent scrape never sees a gap.
	baseLabels := labelsFor(acceleratorType)
	currentReplicas.With(baseLabels).Set(float64(current))
	desiredReplicas.With(baseLabels).Set(float64(desired))
	// Avoid division by 0 if current replicas is zero: set the ratio to the desired replicas.
	// Going 0 -> N is treated by using `desired_ratio = N`.
	if current == 0 {
		desiredRatio.With(baseLabels).Set(float64(desired))
	} else {
		desiredRatio.With(baseLabels).Set(float64(desired) / float64(current))
	}

	// If the accelerator label changed since the last emit for this VA, evict the
	// stale prior series (done after Set, so there is no zero-value window).
	key := replicaSeriesKey(va.Name, va.Namespace)
	replicaSeriesMu.Lock()
	prev, had := replicaSeriesAccel[key]
	replicaSeriesAccel[key] = acceleratorType
	replicaSeriesMu.Unlock()
	if had && prev != acceleratorType {
		stale := labelsFor(prev)
		currentReplicas.Delete(stale)
		desiredReplicas.Delete(stale)
		desiredRatio.Delete(stale)
	}
	return nil
}

// RecordOptimizerActiveMetric records which optimizer is currently active.
// Only one optimizer should be active at a time (isActive=true), while others are inactive (isActive=false).
func (m *MetricsEmitter) RecordOptimizerActiveMetric(optimizerName string, isActive bool) {
	if optimizerActive == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelOptimizerName: optimizerName,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	v := float64(0)
	if isActive {
		v = 1
	}
	optimizerActive.With(labels).Set(v)
}

// RecordError records an error metric for the specified component and error type.
// Callers MUST invoke InitMetrics before this method (the package-level
// metric vars are nil otherwise, and the Set calls below would panic).
// InitMetricsAndEmitter is the recommended construction path because it
// performs the registration before returning the emitter.
func RecordError(component, errorType string) {
	if errorsTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelComponent: component,
		constants.LabelErrorType: errorType,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	errorsTotal.With(labels).Inc()
}

// RecordEnforcerMetric records a decision modification made by the enforcer.
// The policyType identifies which enforcement policy (e.g., "scale_to_zero", "minimum_replicas") was applied.
func (m *MetricsEmitter) RecordEnforcerMetric(policyType string) {
	if enforcerModificationsTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelPolicyType: policyType,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	enforcerModificationsTotal.With(labels).Inc()
}

var (
	nodeAccessDeniedMu    sync.RWMutex
	nodeAccessDeniedState bool
)

// IsNodeAccessDenied reports the last observed node-discovery outcome, for
// callers that must act on it rather than publish it — the readiness gate, which
// takes the controller out of service when cluster policy demands a physical
// limiter this controller cannot serve.
func IsNodeAccessDenied() bool {
	nodeAccessDeniedMu.RLock()
	defer nodeAccessDeniedMu.RUnlock()
	return nodeAccessDeniedState
}

// SetNodeAccessDenied records whether a configured physical limiter is unable to
// read nodes. Published on every refresh, so the series exists while a physical
// limiter is configured and absence means no limiter is asking.
func SetNodeAccessDenied(denied bool) {
	// Mirrored into a plain variable as well as the gauge. The readiness gate has
	// to answer this question in-process, and reading it back out of a Prometheus
	// collector means scraping ourselves — an absurd amount of machinery for one
	// bool, and one that would report "not denied" whenever the metric had not been
	// registered yet.
	nodeAccessDeniedMu.Lock()
	nodeAccessDeniedState = denied
	nodeAccessDeniedMu.Unlock()

	if nodeAccessDenied == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	value := 0.0
	if denied {
		value = 1.0
	}
	nodeAccessDenied.With(labels).Set(value)
}

// SetPolicyNamespaceUnverified records whether the policy-namespace resolution
// fell back to the controller's own ConfigMap because a Namespace read was
// Forbidden. The resolution runs once, before InitMetrics, so the caller
// publishes its outcome after registration; a call before then is a no-op.
func SetPolicyNamespaceUnverified(unverified bool) {
	if policyNamespaceUnverified == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	value := 0.0
	if unverified {
		value = 1.0
	}
	policyNamespaceUnverified.With(labels).Set(value)
}

// SetPodAccessDenied records whether the GPU usage walk was refused a
// cluster-wide Pod list.
func SetPodAccessDenied(denied bool) {
	if podAccessDenied == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	value := 0.0
	if denied {
		value = 1.0
	}
	podAccessDenied.With(labels).Set(value)
}

// SetWatchNamespacesUnreadable replaces the set of watched namespaces this
// controller may not read: one series per namespace, labelled with why.
func SetWatchNamespacesUnreadable(blocked map[string]string) {
	if watchNamespaceUnreadable == nil {
		return
	}
	watchNamespaceUnreadable.Reset()
	for ns, reason := range blocked {
		labels := prometheus.Labels{constants.LabelNamespace: ns, constants.LabelReason: reason}
		if controllerInstance != "" {
			labels[constants.LabelControllerInstance] = controllerInstance
		}
		watchNamespaceUnreadable.With(labels).Set(1)
	}
}

// SetConfigRejectedEntries records how many entries of the scaling-policy
// ConfigMap namespace/name were rejected at its last load. Set on every load,
// so a fixed ConfigMap reads 0.
func SetConfigRejectedEntries(namespace, name string, rejected int) {
	if configRejectedEntries == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelNamespace: namespace, "configmap": name}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configRejectedEntries.With(labels).Set(float64(rejected))
}

// SetConfigUnknownKeys records how many keys the schema does not define the
// scaling-policy ConfigMap namespace/name carried at its last load.
func SetConfigUnknownKeys(namespace, name string, unknown int) {
	if configUnknownKeys == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelNamespace: namespace, "configmap": name}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configUnknownKeys.With(labels).Set(float64(unknown))
}

// SetConfigIgnoredFields records how many fields of the scaling-policy
// ConfigMap namespace/name could not be used at its last load. Set on every
// load, so a fixed ConfigMap reads 0.
func SetConfigIgnoredFields(namespace, name string, ignored int) {
	if configIgnoredFields == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelNamespace: namespace, "configmap": name}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configIgnoredFields.With(labels).Set(float64(ignored))
}

// DeleteConfigEntryGauges removes the wva_config_rejected_entries,
// wva_config_unknown_keys and wva_config_ignored_fields series of the
// scaling-policy ConfigMap namespace/name. Called when the ConfigMap is
// deleted: a deleted ConfigMap has no entries, and its last count would
// otherwise be reported for as long as the controller runs.
func DeleteConfigEntryGauges(namespace, name string) {
	labels := prometheus.Labels{constants.LabelNamespace: namespace, "configmap": name}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	if configRejectedEntries != nil {
		configRejectedEntries.Delete(labels)
	}
	if configUnknownKeys != nil {
		configUnknownKeys.Delete(labels)
	}
	if configIgnoredFields != nil {
		configIgnoredFields.Delete(labels)
	}
}

// SetConstraintProvidersSkipped records which GPU constraint providers are
// skipped for the reasons a producer OWNS. Every series carrying one of the
// owned reasons is replaced by active (provider name -> reason): a provider
// absent from active, or listed under a reason the producer does not own, has
// no series of that producer's afterwards.
//
// Ownership is by reason, as SetModelScalingBlockedReasons does it, because two
// producers publish here: the steady-state engine (unobserved, failed,
// no_constraints) and the readiness gate (node_read_denied). Neither may clear
// what the other set.
func SetConstraintProvidersSkipped(owned []string, active map[string]string) {
	if constraintProviderSkipped == nil {
		return
	}
	for _, reason := range owned {
		match := prometheus.Labels{constants.LabelReason: reason}
		if controllerInstance != "" {
			match[constants.LabelControllerInstance] = controllerInstance
		}
		constraintProviderSkipped.DeletePartialMatch(match)
	}
	for provider, reason := range active {
		if !slices.Contains(owned, reason) {
			continue
		}
		labels := prometheus.Labels{constants.LabelProvider: provider, constants.LabelReason: reason}
		if controllerInstance != "" {
			labels[constants.LabelControllerInstance] = controllerInstance
		}
		constraintProviderSkipped.With(labels).Set(1)
	}
}

// CountScaleFromZeroWakeUnchecked counts one scale-from-zero wake published
// without a GPU capacity check in namespace: limiters are declared, but every
// provider was unavailable and no recent snapshot of their constraints existed.
func CountScaleFromZeroWakeUnchecked(namespace string) {
	if sfzWakeUnchecked == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelNamespace: namespace}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	sfzWakeUnchecked.With(labels).Inc()
}

// SetScaleFromZeroQueueFallbackActive records whether scale-from-zero is reading
// the EPP flow-control queue from Prometheus (true) or scraping the EPP directly
// (false), per pool. It is a gauge rather than a counter because what matters is
// whether the condition is CURRENT: a fallback that engaged for ten seconds last
// week is history, one that has been up for an hour is an unfixed EPP metrics
// path silently slowing every wake.
func SetScaleFromZeroQueueFallbackActive(pool string, active bool) {
	if sfzQueueFallbackActive == nil {
		return
	}
	labels := prometheus.Labels{"pool": pool}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	value := 0.0
	if active {
		value = 1.0
	}
	sfzQueueFallbackActive.With(labels).Set(value)
}

// SetGpuDiscoveryUp sets the GPU discovery status metric.
// status should be 1 when GPU discovery is enabled, 0 when it is disabled.
func SetGpuDiscoveryUp(status float64) {
	if gpuDiscoveryUp == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	gpuDiscoveryUp.With(labels).Set(status)
}

// RecordAvailableGPUsMetric records the number of available GPUs for a given accelerator type. acceleratorModel is
// accelerator full name, acceleratorType is short name.
// This metric is updated during GPU discovery and reflects cluster GPU capacity.
func (m *MetricsEmitter) RecordAvailableGPUsMetric(vendor, acceleratorModel, acceleratorType string, count int32) {
	if availableGpus == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelAcceleratorVendor: vendor,
		constants.LabelAcceleratorModel:  acceleratorModel,
		constants.LabelAcceleratorType:   acceleratorType,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	availableGpus.With(labels).Set(float64(count))
}

// RecordDecisionsLimitedTotalMetric records when a scaling decision was constrained by a limiter.
// This tracks how often the limiter prevents scaling actions due to resource constraints.
func (m *MetricsEmitter) RecordDecisionsLimitedTotalMetric(variantName, namespace, limiterName string) {
	if decisionsLimitedTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
		constants.LabelLimiterName: limiterName,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	decisionsLimitedTotal.With(labels).Inc()
}

// SetConfigInfo sets the config info metric with the given analyzer name and feature flags.
// The metric value is always set to 1 (info-style metric).
func SetConfigInfo(analyzerName string, limiterEnabled, scaleToZeroEnabled bool) {
	if configInfoGauge == nil {
		return
	}
	configInfoGauge.Reset()
	labels := prometheus.Labels{
		constants.LabelAnalyzerName:       analyzerName,
		constants.LabelLimiterEnabled:     strconv.FormatBool(limiterEnabled),
		constants.LabelScaleToZeroEnabled: strconv.FormatBool(scaleToZeroEnabled),
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configInfoGauge.With(labels).Set(1)
}

// SetConfigOptimizationInterval sets the optimization interval in seconds.
func SetConfigOptimizationInterval(intervalSeconds float64) {
	if configOptimizationIntervalSecsGauge == nil {
		return
	}
	configOptimizationIntervalSecsGauge.Reset()
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configOptimizationIntervalSecsGauge.With(labels).Set(intervalSeconds)
}

// ObserveMetricsCollectionDuration records the duration of a metrics collection operation.
func ObserveMetricsCollectionDuration(durationSeconds float64, queryType string) {
	if metricsCollectionDuration == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelQueryType: queryType}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsCollectionDuration.With(labels).Observe(durationSeconds)
}

// IncMetricsCollectionErrors increments the metrics collection error counter.
func IncMetricsCollectionErrors(queryType, reason string) {
	if metricsCollectionErrors == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelQueryType: queryType,
		constants.LabelReason:    reason,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsCollectionErrors.With(labels).Inc()
}

// IncPodMappingMiss increments the pod-to-managed-scaler mapping miss counter.
func IncPodMappingMiss(namespace, reason string) {
	if podMappingMissTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelReason:    reason,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	podMappingMissTotal.With(labels).Inc()
}

// IncScalerUndecidedAnswer counts one GetMetrics answer the KEDA external
// scaler gave without a WVA decision behind it. reason is one of the
// constants.ScalerUndecided* values.
func IncScalerUndecidedAnswer(namespace, reason string) {
	if scalerUndecidedAnswersTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelReason:    reason,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	scalerUndecidedAnswersTotal.With(labels).Inc()
}

var (
	// scalerUndecidedTargetsMu guards scalerUndecidedTargetsFn.
	scalerUndecidedTargetsMu sync.Mutex
	// scalerUndecidedTargetsFn reports, per namespace, how many ScaledObjects
	// the external scaler is currently answering without a decision. Read at
	// scrape time, so the gauge never outlives the state it reports.
	scalerUndecidedTargetsFn func() map[string]int
)

// SetScalerUndecidedTargetsSource sets the function wva_scaler_undecided_targets
// is read from at scrape time (scaler.Handler.UndecidedTargets). Nil reports
// nothing.
func SetScalerUndecidedTargetsSource(fn func() map[string]int) {
	scalerUndecidedTargetsMu.Lock()
	defer scalerUndecidedTargetsMu.Unlock()
	scalerUndecidedTargetsFn = fn
}

// scalerUndecidedTargetsCollector emits wva_scaler_undecided_targets from
// scalerUndecidedTargetsFn on every scrape. A collector rather than a GaugeVec:
// the set is pruned when read, so a namespace whose last undecided target is
// deleted drops out of the scrape instead of keeping a series set by a call
// that will never come again.
type scalerUndecidedTargetsCollector struct {
	desc     *prometheus.Desc
	instance string
}

func newScalerUndecidedTargetsCollector(instance string) *scalerUndecidedTargetsCollector {
	labels := []string{constants.LabelNamespace}
	if instance != "" {
		labels = append(labels, constants.LabelControllerInstance)
	}
	return &scalerUndecidedTargetsCollector{
		desc: prometheus.NewDesc(constants.WVAScalerUndecidedTargets,
			"Number of ScaledObjects the KEDA external scaler is currently answering without a WVA decision (with the scale target's current replica count)",
			labels, nil),
		instance: instance,
	}
}

// Describe implements prometheus.Collector.
func (c *scalerUndecidedTargetsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector.
func (c *scalerUndecidedTargetsCollector) Collect(ch chan<- prometheus.Metric) {
	scalerUndecidedTargetsMu.Lock()
	fn := scalerUndecidedTargetsFn
	scalerUndecidedTargetsMu.Unlock()
	if fn == nil {
		return
	}
	for ns, n := range fn() {
		values := []string{ns}
		if c.instance != "" {
			values = append(values, c.instance)
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), values...)
	}
}

// IncScalerPanic counts one panic recovered in a KEDA external scaler gRPC
// handler. method is the full gRPC method name.
func IncScalerPanic(method string) {
	if scalerPanicsTotal == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelMethod: method}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	scalerPanicsTotal.With(labels).Inc()
}

// SetMetricsPodsDiscovered sets the number of pods discovered in a namespace.
func SetMetricsPodsDiscovered(namespace string, count int) {
	if metricsPodsDiscovered == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelNamespace: namespace}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsPodsDiscovered.With(labels).Set(float64(count))
}

// SetMetricsFreshnessStatus sets the freshness status count for a variant's metrics.
// status should be one of: "fresh", "stale", "missing", "unavailable".
// count is the number of metrics with this status for the variant.
func SetMetricsFreshnessStatus(variantName, status string, count int) {
	if metricsFreshnessStatus == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelStatus:      status,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	metricsFreshnessStatus.With(labels).Set(float64(count))
}

// RecordSaturationMetrics records saturation analysis and KV cache capacity
// metrics. The "Record" naming reflects the fact that the controller does not
// actively push these metrics — Prometheus scrapes them.
//
// modelID is exposed as the model_name label so dashboards can group/filter by
// the model a variant serves. requiredCapacityUnit is used as the "unit" label
// on both wva_required_capacity and wva_spare_capacity (they share the same
// label set) to name the unit the value is expressed in.
//
// Callers MUST invoke InitMetrics before this method (the package-level
// metric vars are nil otherwise, and the Set calls below would panic).
// InitMetricsAndEmitter is the recommended construction path because it
// performs the registration before returning the emitter.
func (m *MetricsEmitter) RecordSaturationMetrics(
	ctx context.Context,
	variantName, namespace, modelID, acceleratorType, requiredCapacityUnit string,
	utilization, spare, required float64,
	kvTokensUsed, kvTokensCapacity int64,
) {
	accelLabels := prometheus.Labels{
		constants.LabelVariantName:     variantName,
		constants.LabelNamespace:       namespace,
		constants.LabelModelName:       modelID,
		constants.LabelAcceleratorType: acceleratorType,
	}
	modelLabels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
	}
	// capacityLabels are shared by wva_required_capacity and wva_spare_capacity: both
	// are model/role-level signals carrying the unit label (no accelerator_type).
	capacityLabels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelUnit:        requiredCapacityUnit,
	}

	if controllerInstance != "" {
		accelLabels[constants.LabelControllerInstance] = controllerInstance
		modelLabels[constants.LabelControllerInstance] = controllerInstance
		capacityLabels[constants.LabelControllerInstance] = controllerInstance
	}

	saturationUtilization.With(accelLabels).Set(utilization)
	spareCapacity.With(capacityLabels).Set(spare)
	requiredCapacity.With(capacityLabels).Set(required)
	kvCacheTokensUsed.With(modelLabels).Set(float64(kvTokensUsed))
	kvCacheTokensCapacity.With(modelLabels).Set(float64(kvTokensCapacity))
}

// RecordAnalyzerDemand publishes an analyzer's demand D for a model instance and
// role ("" for non-disaggregated). Nil-guarded so tests that never call
// InitMetrics are a no-op.
func (m *MetricsEmitter) RecordAnalyzerDemand(analyzer, namespace, modelID, role string, demand float64) {
	if analyzerDemand == nil {
		return
	}
	analyzerDemand.With(analyzerDemandLabelsFor(analyzer, namespace, modelID, role)).Set(demand)
}

// RecordAnalyzerTarget publishes an analyzer's per-replica target P for a variant.
// Nil-guarded like RecordAnalyzerDemand.
func (m *MetricsEmitter) RecordAnalyzerTarget(analyzer, namespace, modelID, variantName string, target float64) {
	if analyzerTarget == nil {
		return
	}
	analyzerTarget.With(analyzerTargetLabelsFor(analyzer, namespace, modelID, variantName)).Set(target)
}

// analyzerDemandLabelsFor builds the full label set for one wva_analyzer_demand
// series, so Record and Delete cannot disagree about it (Delete needs an exact
// match, including the optional controller_instance label).
func analyzerDemandLabelsFor(analyzer, namespace, modelID, role string) prometheus.Labels {
	labels := prometheus.Labels{
		constants.LabelAnalyzerName: analyzer,
		constants.LabelNamespace:    namespace,
		constants.LabelModelName:    modelID,
		constants.LabelRole:         role,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	return labels
}

// analyzerTargetLabelsFor is analyzerDemandLabelsFor's counterpart for
// wva_analyzer_target.
func analyzerTargetLabelsFor(analyzer, namespace, modelID, variantName string) prometheus.Labels {
	labels := prometheus.Labels{
		constants.LabelAnalyzerName: analyzer,
		constants.LabelNamespace:    namespace,
		constants.LabelModelName:    modelID,
		constants.LabelVariantName:  variantName,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	return labels
}

// RecordEngineConfig publishes the engine-configuration fingerprint a variant
// is running, with the flags that produced it.
//
// fingerprintVersion and flagValues come from the capacity package --
// capacity.FingerprintVersion and EngineParams.FingerprintValues() -- passed in
// rather than imported, so this infrastructure leaf gains no edge to a
// pipeline package. flagValues is
// positionally paired with constants.EngineConfigFlagLabels. A length
// mismatch means the two drifted, and this drops the sample rather than
// publishing a configuration described by the wrong labels -- a mislabelled
// diagnostic is worse than an absent one.
//
// Any previous series for this variant is removed first. A variant whose
// configuration changes would otherwise leave its old label combination
// behind for ever, because changing a label value creates a new series rather
// than updating the old one.
func (m *MetricsEmitter) RecordEngineConfig(
	namespace, modelID, variantName, accelerator string,
	gpusPerReplica int,
	fingerprint string,
	fingerprintVersion int,
	flagValues []string,
) {
	if engineConfig == nil || fingerprint == "" {
		return
	}
	if len(flagValues) != len(constants.EngineConfigFlagLabels) {
		return
	}

	m.DeleteEngineConfig(namespace, modelID, variantName)

	labels := prometheus.Labels{
		constants.LabelNamespace:          namespace,
		constants.LabelModelName:          modelID,
		constants.LabelVariantName:        variantName,
		constants.LabelAccelerator:        accelerator,
		constants.LabelGPUsPerReplica:     strconv.Itoa(gpusPerReplica),
		constants.LabelFingerprint:        fingerprint,
		constants.LabelFingerprintVersion: strconv.Itoa(fingerprintVersion),
	}
	for i, l := range constants.EngineConfigFlagLabels {
		labels[l] = flagValues[i]
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	engineConfig.With(labels).Set(1)
}

// DeleteEngineConfig removes every wva_engine_config series for one variant of
// one model, whatever configuration it was last seen running. Partial match
// because the flag labels are part of the series identity and the caller does
// not know which values are currently published.
//
// modelID is part of the match and the omission of it was a bug. A variant
// name is unique within a model, not within a namespace, so matching on
// namespace and variant alone made two models that share a namespace AND a
// variant name delete each other's series -- and because RecordEngineConfig
// deletes before it sets, publishing the second model's configuration silently
// unpublished the first. Found by the test that drives the engine's own
// publication path.
func (m *MetricsEmitter) DeleteEngineConfig(namespace, modelID, variantName string) {
	if engineConfig == nil {
		return
	}
	engineConfig.DeletePartialMatch(prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelVariantName: variantName,
	})
}

// DeleteEngineConfigForModel removes every wva_engine_config series for a
// model instance, used when a model stops being reconciled entirely.
func (m *MetricsEmitter) DeleteEngineConfigForModel(namespace, modelID string) {
	if engineConfig == nil {
		return
	}
	engineConfig.DeletePartialMatch(prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelID,
	})
}

// The outcomes of a learned-state restore, and the operations of the
// snapshot store, as the counters label them. Each is published at zero from
// registration, so rate() over a fresh controller reads 0 rather than nothing.
const (
	LearnedStateOpRead  = "read"
	LearnedStateOpWrite = "write"
)

// LearnedStateRestoreOutcomes are the outcomes a restore is counted under,
// as the analyzer names them (saturation.RestoreOutcome).
var LearnedStateRestoreOutcomes = []string{"whole", "args", "prior", "incomplete", "differs"}

// RecordLearnedStateBorrows publishes what a variant borrowed this cycle, and
// removes whatever it published before that is not borrowed now -- a borrow
// that ended, or whose donor changed, must not leave its series behind. The
// new series are set before the old ones go, so a borrow still in effect is
// never absent from a concurrent scrape.
func (m *MetricsEmitter) RecordLearnedStateBorrows(namespace, modelID, variantName string, borrows []domain.LearnedStateBorrow) {
	if learnedStateBorrowed == nil || learnedStateRelaxed == nil {
		return
	}
	variantLabels := func() prometheus.Labels {
		l := prometheus.Labels{
			constants.LabelNamespace:   namespace,
			constants.LabelModelName:   modelID,
			constants.LabelVariantName: variantName,
		}
		if controllerInstance != "" {
			l[constants.LabelControllerInstance] = controllerInstance
		}
		return l
	}
	keep := map[string]bool{}
	relaxedSet := false
	for _, b := range borrows {
		labels := variantLabels()
		labels[constants.LabelQuantity] = b.Quantity
		labels[constants.LabelDonor] = b.Donor
		labels[constants.LabelDonorNamespace] = b.DonorNamespace
		learnedStateBorrowed.With(labels).Set(1)
		keep[b.Quantity+"|"+b.DonorNamespace+"|"+b.Donor] = true
		if b.Quantity == domain.BorrowedK2 {
			learnedStateRelaxed.With(variantLabels()).Set(b.Relaxed)
			relaxedSet = true
		}
	}
	learnedBorrowsMu.Lock()
	key := namespace + "|" + modelID + "|" + variantName
	prev := learnedBorrowsPublished[key]
	learnedBorrowsPublished[key] = borrows
	learnedBorrowsMu.Unlock()
	for _, b := range prev {
		if keep[b.Quantity+"|"+b.DonorNamespace+"|"+b.Donor] {
			continue
		}
		labels := variantLabels()
		labels[constants.LabelQuantity] = b.Quantity
		labels[constants.LabelDonor] = b.Donor
		labels[constants.LabelDonorNamespace] = b.DonorNamespace
		learnedStateBorrowed.Delete(labels)
	}
	if !relaxedSet {
		learnedStateRelaxed.Delete(variantLabels())
	}
}

// learnedBorrowsPublished is what RecordLearnedStateBorrows last published per
// namespace|model|variant, so the next call can remove exactly what ended.
var (
	learnedBorrowsMu        sync.Mutex
	learnedBorrowsPublished = map[string][]domain.LearnedStateBorrow{}
)

// RecordLearnedStateK2Source publishes the source of the k2 a variant is
// priced on and, when hasAge, how long ago that figure was last measured; an
// empty source publishes nothing. Whatever it published for the variant
// before under another role or source is removed after the new series is
// set, so a variant priced every cycle is never absent from a concurrent
// scrape.
func (m *MetricsEmitter) RecordLearnedStateK2Source(namespace, modelID, variantName, role, source string, age time.Duration, hasAge bool) {
	if learnedStateK2Source == nil || learnedStateK2Age == nil {
		return
	}
	labels := func(role, source string) prometheus.Labels {
		l := prometheus.Labels{
			constants.LabelNamespace:   namespace,
			constants.LabelModelName:   modelID,
			constants.LabelVariantName: variantName,
			constants.LabelRole:        role,
			constants.LabelSource:      source,
		}
		if controllerInstance != "" {
			l[constants.LabelControllerInstance] = controllerInstance
		}
		return l
	}
	now := learnedK2Series{role: role, source: source, hasAge: hasAge && source != ""}
	if source != "" {
		learnedStateK2Source.With(labels(role, source)).Set(1)
		if now.hasAge {
			learnedStateK2Age.With(labels(role, source)).Set(age.Seconds())
		}
	}
	key := namespace + "|" + modelID + "|" + variantName
	learnedBorrowsMu.Lock()
	prev, had := learnedK2Published[key]
	if source != "" {
		learnedK2Published[key] = now
	} else {
		delete(learnedK2Published, key)
	}
	learnedBorrowsMu.Unlock()
	if !had {
		return
	}
	if source == "" || prev.role != role || prev.source != source {
		learnedStateK2Source.Delete(labels(prev.role, prev.source))
		learnedStateK2Age.Delete(labels(prev.role, prev.source))
	} else if prev.hasAge && !now.hasAge {
		learnedStateK2Age.Delete(labels(prev.role, prev.source))
	}
}

// learnedK2Series is the k2-source series RecordLearnedStateK2Source last
// published for a variant; learnedK2Published holds one per
// namespace|model|variant, guarded by learnedBorrowsMu.
type learnedK2Series struct {
	role, source string
	hasAge       bool
}

var learnedK2Published = map[string]learnedK2Series{}

// DeleteLearnedStateBorrows removes one variant's borrow and k2-source series,
// whatever it borrowed and from whom. modelID is part of the match: a variant
// name is unique within a model, not within a namespace (see
// DeleteEngineConfig).
func (m *MetricsEmitter) DeleteLearnedStateBorrows(namespace, modelID, variantName string) {
	match := prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelVariantName: variantName,
	}
	for _, g := range []*prometheus.GaugeVec{learnedStateBorrowed, learnedStateRelaxed, learnedStateK2Source, learnedStateK2Age} {
		if g != nil {
			g.DeletePartialMatch(match)
		}
	}
	learnedBorrowsMu.Lock()
	delete(learnedBorrowsPublished, namespace+"|"+modelID+"|"+variantName)
	delete(learnedK2Published, namespace+"|"+modelID+"|"+variantName)
	learnedBorrowsMu.Unlock()
}

// DeleteLearnedStateBorrowsForModel removes every borrow and k2-source series
// of a model: one that stops being reconciled, or has no replica reporting.
func (m *MetricsEmitter) DeleteLearnedStateBorrowsForModel(namespace, modelID string) {
	match := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelID}
	for _, g := range []*prometheus.GaugeVec{learnedStateBorrowed, learnedStateRelaxed, learnedStateK2Source, learnedStateK2Age} {
		if g != nil {
			g.DeletePartialMatch(match)
		}
	}
	learnedBorrowsMu.Lock()
	for k := range learnedBorrowsPublished {
		if strings.HasPrefix(k, namespace+"|"+modelID+"|") {
			delete(learnedBorrowsPublished, k)
		}
	}
	for k := range learnedK2Published {
		if strings.HasPrefix(k, namespace+"|"+modelID+"|") {
			delete(learnedK2Published, k)
		}
	}
	learnedBorrowsMu.Unlock()
}

// AddLearnedStateRestores counts restored variants by outcome.
func (m *MetricsEmitter) AddLearnedStateRestores(byOutcome map[string]int) {
	if learnedStateRestores == nil {
		return
	}
	for outcome, n := range byOutcome {
		if n > 0 {
			learnedStateRestores.WithLabelValues(outcome).Add(float64(n))
		}
	}
}

// RecordLearnedStateSnapshotSize publishes the size of the snapshot document
// just written, the bound it had to fit, and how many entries it left out.
func (m *MetricsEmitter) RecordLearnedStateSnapshotSize(bytes, budget, dropped int) {
	if learnedStateBytes == nil {
		return
	}
	learnedStateBytes.Set(float64(bytes))
	learnedStateBudget.Set(float64(budget))
	learnedStateDropped.Set(float64(dropped))
}

// IncLearnedStateSnapshotError counts one failed read or write of the
// snapshot store.
func (m *MetricsEmitter) IncLearnedStateSnapshotError(operation string) {
	if learnedStateErrors == nil {
		return
	}
	learnedStateErrors.WithLabelValues(operation).Inc()
}

// DeleteAnalyzerDemand removes one wva_analyzer_demand series. Absence is
// meaningful for the analyzer metrics — a series that is no longer emitted must
// disappear rather than freeze at its last value — so the engine evicts the
// series it stops publishing. Nil-guarded like the Record methods.
func (m *MetricsEmitter) DeleteAnalyzerDemand(analyzer, namespace, modelID, role string) {
	if analyzerDemand == nil {
		return
	}
	analyzerDemand.Delete(analyzerDemandLabelsFor(analyzer, namespace, modelID, role))
}

// RecordAnalyzerObservedReplicas publishes how many replicas the analyzer
// attributed to a variant this cycle, before the scale-target clamp. Nil-guarded
// like RecordAnalyzerTarget.
func (m *MetricsEmitter) RecordAnalyzerObservedReplicas(analyzer, namespace, modelID, variantName string, observed int) {
	if analyzerObservedReplicas == nil {
		return
	}
	analyzerObservedReplicas.With(analyzerTargetLabelsFor(analyzer, namespace, modelID, variantName)).Set(float64(observed))
}

// DeleteAnalyzerObservedReplicas removes one wva_analyzer_observed_replicas
// series. Shares analyzerTarget's label shape and its eviction bookkeeping, so
// the two are always added and removed together.
func (m *MetricsEmitter) DeleteAnalyzerObservedReplicas(analyzer, namespace, modelID, variantName string) {
	if analyzerObservedReplicas == nil {
		return
	}
	analyzerObservedReplicas.Delete(analyzerTargetLabelsFor(analyzer, namespace, modelID, variantName))
}

// DeleteAnalyzerTarget removes one wva_analyzer_target series.
func (m *MetricsEmitter) DeleteAnalyzerTarget(analyzer, namespace, modelID, variantName string) {
	if analyzerTarget == nil {
		return
	}
	analyzerTarget.Delete(analyzerTargetLabelsFor(analyzer, namespace, modelID, variantName))
}

// DeleteAnalyzerSeriesForModel removes every wva_analyzer_demand and
// wva_analyzer_target series for a model instance, across all analyzers, roles
// and variants. Used when a model stops being reconciled entirely, where the
// per-series bookkeeping the engine keeps for live models no longer applies.
func (m *MetricsEmitter) DeleteAnalyzerSeriesForModel(namespace, modelID string) {
	match := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelID,
	}
	if analyzerDemand != nil {
		analyzerDemand.DeletePartialMatch(match)
	}
	if analyzerTarget != nil {
		analyzerTarget.DeletePartialMatch(match)
	}
	if analyzerObservedReplicas != nil {
		analyzerObservedReplicas.DeletePartialMatch(match)
	}
}

// RecordSaturationFreshness publishes the per-VA freshness signal for the
// five saturation/capacity gauges recorded by RecordSaturationMetrics.
//
// fresh=true means the optimizer just produced a fresh decision for the
// variant this cycle (the other gauges have just been refreshed). fresh=false
// means the analyzer was aware of the variant but did not refresh those
// gauges this cycle, so dashboards see an explicit "stale" signal rather
// than relying on Prometheus' 5-minute implicit staleness marker.
//
// Unlike RecordSaturationMetrics, this method runs on every variant every
// cycle (not gated on hasDecision), so it must tolerate tests that haven't
// called InitMetrics — nil-guarded the same way as SetMetricsFreshnessStatus.
// In production InitMetrics runs at startup from main.go and the guard is a
// no-op fast path.
func (m *MetricsEmitter) RecordSaturationFreshness(ctx context.Context, variantName, namespace string, fresh bool) {
	if saturationMetricsUp == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	value := 0.0
	if fresh {
		value = 1.0
	}
	saturationMetricsUp.With(labels).Set(value)
}
