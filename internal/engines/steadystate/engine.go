/*
Copyright 2025 The llm-d Authors

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

package steadystate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	accel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	actuator "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controllerstatus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/external"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/variantmeta"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scalingpolicy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/balance"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/cycleclock"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// analyzerEntry binds a registered analyzer to its name. The engine stores
// these in registration order so runAnalyzers iterates analyzers
// deterministically.
type analyzerEntry struct {
	name     string
	analyzer domain.Analyzer
}

type Engine struct {
	client client.Client
	// apiReader reads past the manager's cache: the utilization-share need
	// history ConfigMap is in no namespace or label set the cache holds. Nil in
	// tests, which read through client.
	apiReader client.Reader
	scheme    *runtime.Scheme
	executor  executor.Executor

	// Recorder - use wrapper function recordEvent to limit number of events per va in an optimization cycle
	Recorder record.EventRecorder

	// vaEventTracker tracks whether a K8S event has been issued for a variant in an optimization cycle.
	// Key is namespace/name from utils.GetNamespacedKey.
	vaEventTracker map[string]bool

	// policies reports which scaling policy tier each model resolved to, and the
	// two ways that resolution goes wrong silently — an unknown tier name, and one
	// model's variants naming different tiers. Change-throttled; see
	// scalingpolicy.ChangeReporter.
	policies *scalingpolicy.ChangeReporter

	Config *config.Config // Unified configuration (injected from main.go)

	// Status receives the utilization-share mode each cycle for the
	// controller-status ConfigMap. Nil-safe: tests leave it unset.
	Status *controllerstatus.Publisher

	// Preemptions records Kueue's preemptions of managed pods from the
	// manager's pod informer, between cycles. Nil: preemptions are read only
	// by listing, once per cycle, which misses a pod and Workload that are gone
	// within seconds.
	Preemptions *PreemptionWatch

	// ReplicaMetricsCollector is the collector for replica metrics using the source infrastructure
	ReplicaMetricsCollector *collector.ReplicaMetricsCollector

	// ScaleToZeroEnforcer applies scale-to-zero and minimum replica enforcement
	ScaleToZeroEnforcer *allocation.Enforcer

	// GPULimiter constrains scaling decisions based on available GPU resources.
	// Only applied when EnableLimiter is true in the saturation config. When a
	// limiterBuilder is set (production), it is rebuilt live at the top of each
	// optimize cycle whenever the effective limiter config changes.
	GPULimiter allocation.Limiter

	// limiterBuilder rebuilds GPULimiter from the current effective config. Set by
	// SetLimiterBuilder in production; nil in tests (which inject a static
	// GPULimiter). limiterSig is the signature of the config that built the current
	// GPULimiter, used to skip rebuilds when nothing changed. Both are read and
	// written only from the single optimize goroutine after StartOptimizeLoop, but
	// SetLimiterBuilder may run before it, so limiterMu guards them.
	limiterBuilder func() (allocation.Limiter, error)
	limiterSig     string
	limiterMu      sync.Mutex

	// lastQuotaAloneDiff is the last quota-alone versus composed allocation
	// difference reported (see reportQuotaAloneAllocation), so the transitional
	// upgrade log prints when the difference changes, not every cycle. Written
	// only from the optimize goroutine.
	lastQuotaAloneDiff string
	// quotaAloneCycles counts the cycles reportQuotaAloneAllocation was
	// eligible to run in, so its shadow pass runs once per
	// quotaAloneReportEvery of them. TRANSITIONAL, with it.
	quotaAloneCycles int

	// providerSkips publishes the constraint providers left out of the last
	// decision (wva_constraint_provider_skipped) and logs when that set changes.
	providerSkips providerSkipReport

	// nodeFitSkipped is why the last cycle bounded by the physical limiter
	// fitted replicas to the pool and not to nodes ("" when it did fit them),
	// logged when it changes. Written only from the optimize goroutine.
	nodeFitSkipped string
	// layouts remembers the variants whose unknown pod layout this engine's
	// node views have logged, so each is logged once, not once per cycle.
	layouts allocation.LayoutLog
	// cycleFitted is whether this cycle's optimizer pass was fitted to nodes,
	// and cyclePlanned the models the utilization share planned in it: what
	// admitInFlight needs once the decisions are final. Set by decideV2.
	cycleFitted  bool
	cyclePlanned map[string]bool
	// inFlightMark is what this cycle's provisional admission of the pass's
	// placements replaced in the in-flight ledger (admitPassInFlight), until
	// admitInFlight commits the final targets; nil between cycles.
	inFlightMark allocation.InFlightMark
	// inFlightStore is the in-flight ledger's persistence in the controller
	// status (inflight_persist.go).
	inFlightStore inFlightStore

	// metricsRegistry is used to access metrics sources for request count queries
	metricsRegistry *source.SourceRegistry
	// Variants is the set of workloads KEDA has called WVA about — discovery. Nil
	// falls back to the annotation-sourced listing.
	Variants *registry.Registry
	// VariantEnricher resolves each registered workload's scale target with an
	// uncached read. Refreshed at the top of every cycle; a no-op for entries
	// still inside their freshness window.
	VariantEnricher *registry.Enricher

	// saturationV2Analyzer is the V2 token-based saturation analyzer (initialized once).
	// Also pre-registered in analyzers under domain.SaturationAnalyzerName.
	// Typed as domain.Analyzer to allow injection in tests.
	saturationV2Analyzer domain.Analyzer

	// sloAnalyzer is the built-in slo analyzer, also registered in analyzers
	// under domain.SLOAnalyzerName: held here so the per-cycle sweeps reach its
	// learned state whether or not any model runs it. Nil in tests that build
	// an Engine by hand.
	sloAnalyzer sloStateKeeper
	// sloFailedSince is when the current run of failed cycles began, zero
	// while cycles succeed. Read by beginSLOCycle, written by optimize: both
	// on the executor's goroutine.
	sloFailedSince time.Time
	// reportedCollisions is the external analyzer labels already logged as
	// taken by a built-in, so each is logged once.
	reportedCollisions map[string]bool

	// capacityStore is shared with the V2 analyzer for caching capacity knowledge.
	capacityStore *capacity.Store

	// LearnerRecording turns the learner recorder on when it names a
	// directory: every input the analyzers and the capacity store read, and
	// every result, one recording per controller start
	// (learner_recording.go). Set before StartOptimizeLoop. Off by default.
	LearnerRecording learnrec.Options
	// learnRec is the open recording; nil when recording is off.
	learnRec *learnrec.Recorder

	// clock is the cycle clock: pinned once per optimize cycle (optimizeV2),
	// it is what the analyzers and the capacity store stamp and age their
	// learned state on, so one cycle reads one instant. Nil in an engine a
	// test builds by hand, which reads the wall clock.
	clock *cycleclock.Clock

	// lastGoodAnalysis records, per model (keyed by utils.GetNamespacedKey(namespace,
	// modelID)) and analyzer name, the AnalyzedAt of the most recent informative
	// (non-error, capacity-bearing) result. Used to gate the scale-down veto: an
	// analyzer whose last good analysis for a model is absent or staler than
	// analyzerLivenessStaleCycles cycles does not participate in that model's vote.
	// Keyed per model (not just per analyzer name) because one Engine instance
	// serves all models — a global-by-name map would leak one model's freshness
	// into another's. In-memory only; reset on process restart / leader failover
	// (safe: non-live → no scale-down until refreshed).
	// Unguarded, like vaEventTracker: safe today because PollingExecutor runs
	// optimize cycles sequentially in one goroutine and models within a cycle
	// are processed serially. Parallelizing model processing would need to
	// synchronize this map — the top-level per-model insert would race first.
	lastGoodAnalysis map[string]map[string]time.Time

	// lastAnalyzerSeries records, per model (keyed identically to
	// lastGoodAnalysis), the wva_analyzer_demand / wva_analyzer_target series
	// emitted on the previous cycle. Prometheus gauges have no way to enumerate
	// their own children, so the engine tracks what it published in order to
	// evict series it stops publishing. Absence is meaningful for these metrics
	// — a role that disappears or an analyzer that stops running must drop its
	// series rather than freeze at its last value — see pruneAnalyzerSeries.
	// Unguarded for the same reason as lastGoodAnalysis: optimize cycles run
	// sequentially in one goroutine and models are processed serially.
	lastAnalyzerSeries map[string]analyzerSeries

	// publishedEngineConfig records, per model (keyed identically), the
	// variants whose wva_engine_config series was published since the model's
	// last recordAnalyzerMetrics. That call folds them into the cycle's kept
	// set and clears the entry. Unguarded for the same reason as
	// lastAnalyzerSeries.
	publishedEngineConfig map[string]engineConfigPublished

	// lastDecided records, per scale target (keyed namespace/target), when the
	// optimize loop last DECIDED for it -- as opposed to republished for it --
	// and which incarnation of the target that decision was about. The sticky
	// scale-down trusts a published value only when both still hold: the
	// decision store's own timestamp is refreshed by every publish, including
	// the carry that republishes a held value through a cycle with no metrics,
	// so it cannot tell "decided lately" from "silent, but repeating itself";
	// and a target deleted and re-created under the same name is a different
	// fleet, whose first cycles must not inherit the old one's descent.
	// Entries are swept once they are too old to be trusted.
	lastDecided map[string]decidedMark
	// scaleTargetUIDs is this cycle's identity per scale target, refilled by
	// optimizeV2 from what collection read.
	scaleTargetUIDs map[string]types.UID

	// utilizationShare is the utilization-share optimizer's between-cycle
	// state: what it reported, and the transfer ledgers it acts on unless its
	// policy is in shadow mode; see evaluateUtilizationShare.
	utilizationShare utilizationShareState

	// borrowing is the engine's cohort-borrowing state across cycles: growth
	// ordered on borrowed capacity that Kueue has not reserved yet, and this
	// cycle's node view (borrowing.go).
	borrowing borrowingState

	// learnedState is the learned-state snapshot store's between-cycle state
	// (learned_state_store.go).
	learnedState learnedStateStoreState

	// lastBlockedModels records, keyed identically, every model this engine has
	// published wva_model_scaling_blocked reasons for. Same reason as
	// lastAnalyzerSeries — a GaugeVec cannot enumerate its own children — but the
	// consequence is sharper: a reason series is PRESENT only while it holds, so
	// a model that goes away leaves a series asserting that a workload which no
	// longer exists will never park, and it alerts forever. Nothing else can clear
	// it, because the producer that would clear it never runs for that model
	// again. See pruneBlockedModels.
	lastBlockedModels map[string]blockedModelRef

	// analyzers is the engine's analyzer registry, mutated only during setup
	// (NewEngine + RegisterAnalyzer). After StartOptimizeLoop it is frozen —
	// further RegisterAnalyzer calls return an error. The optimize goroutine reads
	// analyzersSnapshot, never analyzers, so iteration is race-free without
	// runtime locking.
	analyzers []analyzerEntry

	// analyzersSnapshot is the frozen, registration-ordered view that
	// runAnalyzers iterates. Built from analyzers in StartOptimizeLoop
	// before the goroutine launches. Saturation always runs; every other
	// registered analyzer runs when the policy lists it, and the composite
	// combines what they return (buildComposite).
	analyzersSnapshot []analyzerEntry

	// started transitions to true in StartOptimizeLoop. Late RegisterAnalyzer
	// calls return an error so the contract "register before Start" is enforced
	// rather than just documented.
	started bool

	// externalMu guards externalAnalyzers, which — unlike the frozen built-in
	// registry above — may change while the optimize goroutine runs.
	externalMu sync.RWMutex
	// externalAnalyzers holds config-driven external analyzers keyed by name.
	// It is mutated at runtime (UpsertExternalAnalyzer/RemoveExternalAnalyzer)
	// when the analyzer catalog changes, and read under RLock each cycle. This is
	// the "analyzers can be added at run-time" path; the built-in snapshot stays
	// frozen and lock-free.
	externalAnalyzers map[string]domain.Analyzer

	// optimizer is the V2 scaling optimizer that produces VariantDecisions from
	// AnalyzerResults. Selected per-cycle from the declared limiters:
	// CostAwareOptimizer when none is declared, GreedyByScoreOptimizer when one is.
	optimizer allocation.ScalingOptimizer

	metricsEmitter *metrics.MetricsEmitter

	// loadBalance remembers, per model and role, how long the router has kept
	// one replica saturated beside an idle one (observeLoadBalance). Built on
	// first use.
	loadBalance *balance.Tracker
	// loadBalanceNow is the clock observeLoadBalance reads; nil is time.Now.
	// A test sets it to step cycles without waiting for them.
	loadBalanceNow func() time.Time
}

// NewEngine creates a new instance of the saturation engine.
// Config must be non-nil (validated in main.go before engine creation).
// gpuLimiter is the operator-selected limiter built by
// allocation.NewLimiterFromConfig in main.go and must be non-nil — tests
// that do not exercise the limiter path can pass allocation.NewNoOpLimiter.
// Panics if cfg or gpuLimiter is nil to fail fast on programming errors.
func NewEngine(client client.Client, apiReader client.Reader, scheme *runtime.Scheme, recorder record.EventRecorder, metricsRegistry *source.SourceRegistry, cfg *config.Config, gpuLimiter allocation.Limiter) *Engine {
	return newEngine(client, apiReader, scheme, recorder, metricsRegistry, cfg, gpuLimiter, clock.RealClock{})
}

// newEngine is NewEngine with the cycle clock's base clock given: a test runs
// the engine on a fake one.
func newEngine(client client.Client, apiReader client.Reader, scheme *runtime.Scheme, recorder record.EventRecorder, metricsRegistry *source.SourceRegistry, cfg *config.Config, gpuLimiter allocation.Limiter, base clock.PassiveClock) *Engine {
	if cfg == nil {
		panic("config is nil in NewEngine - this should not happen (validated in main.go before engine creation)")
	}
	if gpuLimiter == nil {
		panic("gpuLimiter is nil in NewEngine - production callers must use allocation.NewLimiterFromConfig; tests should pass allocation.NewNoOpLimiter")
	}
	promSource := metricsRegistry.Get("prometheus") // assume prometheus source is registered

	// Create request count function wrapper for scale-to-zero enforcer
	requestCountFunc := func(ctx context.Context, engine inferenceengine.Engine, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
		return registration.CollectModelRequestCountForEngine(ctx, promSource, engine, modelID, namespace, retentionPeriod)
	}

	capacityStore := capacity.NewStore()
	cycleClock := cycleclock.New(base)
	satV2 := saturation.NewSaturationAnalyzer(capacityStore).WithClock(cycleClock)
	// How long the analyzer keeps what it learns. Configured here rather than
	// read per-cycle: these are process-lifetime horizons, and a sweep whose
	// horizon moved underneath it would evict on one age and report another.
	//
	// Logged once, at the only moment the values are chosen, and ALWAYS --
	// not only when they differ from the default. The shortest of them is a
	// day, so nothing an operator can observe in a single session reveals
	// which horizons a process is running; this line is the only record.
	gap, variant, bucket := cfg.LearnedStateHorizons()
	requested := capacity.Horizons{
		EpisodeGap: gap, VariantTimeout: variant, BucketRetention: bucket,
	}
	satV2, replaced := satV2.WithHorizons(requested)
	// A k2 borrow's clock counts serving cycles; how far apart two of them may
	// be is the loop's cadence (k2_borrow.go).
	satV2 = satV2.WithOptimizationInterval(cfg.OptimizationInterval())
	h := satV2.Horizons()
	ctrl.Log.Info("learned-state horizons",
		"episodeGap", h.EpisodeGap,
		"variantTimeout", h.VariantTimeout,
		"bucketRetention", h.BucketRetention,
		"borrowTickGap", satV2.BorrowTickGap(),
		// Whether the snapshot is on: its counters read 0 both when it is off
		// and when it works, so this line is where an operator confirms it.
		"snapshot", cfg.LearnedStateSnapshotEnabled(),
		"note", "set LEARNED_STATE_EPISODE_GAP, LEARNED_STATE_VARIANT_TIMEOUT, LEARNED_STATE_BUCKET_RETENTION to change them")
	// A replaced horizon has to say so, because the line above cannot. After
	// flooring it prints the default, which is byte-identical to what an
	// unconfigured controller prints -- so an operator who mistyped reads
	// their own mistake as confirmation. The requested values are logged
	// alongside: a unit-less "24" arrives here as 24ns, which names the
	// mistake precisely. Error rather than Info, matching
	// sanitizeOptimizationInterval, which makes the same call for
	// GLOBAL_OPT_INTERVAL.
	if len(replaced) > 0 {
		ctrl.Log.Error(
			fmt.Errorf("learned-state horizons below %v were replaced by their defaults", capacity.MinHorizon),
			"Unusable learned-state horizon, using default",
			"replaced", replaced,
			"requestedEpisodeGap", requested.EpisodeGap,
			"requestedVariantTimeout", requested.VariantTimeout,
			"requestedBucketRetention", requested.BucketRetention,
			"note", "a duration without a unit parses as NANOSECONDS: 24 is 24ns, not 24h")
	}

	// Initialize with default optimizer. The actual optimizer is selected
	// per-cycle in optimize() from the ConfigMap's live limiters: list, since
	// config arrives after engine init.
	var scalingOptimizer allocation.ScalingOptimizer = allocation.NewCostAwareOptimizer()

	// Declared before the locator so the closure below can read the field that is
	// wired after construction — see locator.New on why it takes a func.
	var engine Engine

	podLocator, err := locator.New(apiReader, func() *registry.Registry { return engine.Variants })
	if err != nil {
		// locator.New only fails when defaultCacheSize <= 0, which is a
		// programming error we cannot recover from at runtime.
		panic(fmt.Sprintf("locator.New: %v", err))
	}

	// The collector attributes a lent warm-pool Pod to the variant it is lent
	// to; the lending lives in the decision store, handed in here so the
	// collector does not read the store itself.
	replicaCollector := collector.NewReplicaMetricsCollector(promSource, client, apiReader, recorder, podLocator)
	replicaCollector.SetBridgeResolver(decision.DefaultBridges)
	// The slo analyzer keeps its learned state as long as saturation keeps its
	// own: LEARNED_STATE_VARIANT_TIMEOUT governs both, and
	// LEARNED_STATE_BUCKET_RETENTION bounds how long both hold a line's slope.
	sloAnalyzer := slo.New(replicaCollector, capacityStore).
		WithClock(cycleClock).
		WithStateTimeout(satV2.Horizons().VariantTimeout).
		WithSlopeHoldFor(satV2.Horizons().BucketRetention)

	engine = Engine{
		client:                  client,
		apiReader:               apiReader,
		scheme:                  scheme,
		Recorder:                recorder,
		Config:                  cfg,
		ReplicaMetricsCollector: replicaCollector,
		ScaleToZeroEnforcer:     allocation.NewEnforcer(requestCountFunc, cfg),
		GPULimiter:              gpuLimiter,
		policies:                scalingpolicy.NewChangeReporter(),
		metricsRegistry:         metricsRegistry,
		saturationV2Analyzer:    satV2,
		sloAnalyzer:             sloAnalyzer,
		capacityStore:           capacityStore,
		clock:                   cycleClock,
		lastGoodAnalysis:        make(map[string]map[string]time.Time),
		lastAnalyzerSeries:      make(map[string]analyzerSeries),
		lastBlockedModels:       make(map[string]blockedModelRef),
		optimizer:               scalingOptimizer,
		metricsEmitter:          metrics.NewMetricsEmitter(),
		analyzers: []analyzerEntry{
			{name: domain.SaturationAnalyzerName, analyzer: satV2},
			// Built in and always registered: it runs only for a model whose
			// policy lists it under analyzers:, and its router queries run only
			// for such a model, so registering it costs nothing elsewhere, and
			// enabling it needs no restart. Its result is
			// published and, unlike the other listed analyzers, never sizes a
			// model: the composite leaves it out (docs/proposals/slo-tiers.md,
			// phase 0).
			{name: domain.SLOAnalyzerName, analyzer: sloAnalyzer},
		},
		externalAnalyzers: make(map[string]domain.Analyzer),
	}

	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     cfg.OptimizationInterval(),
		RetryBackoff: 100 * time.Millisecond,
	})

	// Register saturation queries in the metrics registry: the base queries
	// (kv_cache_usage, queue_length) plus the token-based analyzer's own
	// (cache_config_info, avg_output_tokens, etc.).
	registration.RegisterSaturationQueries(metricsRegistry)

	// Register scale-to-zero queries in the metrics registry
	registration.RegisterScaleToZeroQueries(metricsRegistry)

	// Register queueing model queries (scheduler dispatch rate per endpoint).
	// These are collected alongside saturation metrics into the shared
	// ReplicaMetrics struct and used by the queueing model analyzer to
	// estimate per-replica arrival rate and model queue behavior.
	registration.RegisterQueueingModelQueries(metricsRegistry)

	// The router latency the slo analyzer judges an SLO on. Run only when that
	// analyzer asks for them.
	registration.RegisterSLOQueries(metricsRegistry)

	return &engine
}

// Clock is the engine's cycle clock, for an analyzer registered from outside
// (RegisterAnalyzer) to stamp its state on the same instant as the built-in
// ones.
func (e *Engine) Clock() clock.PassiveClock {
	return e.clock
}

// RegisterAnalyzer adds an external analyzer to the engine's analyzer
// registry. Returns an error if called after StartOptimizeLoop or if name
// is already registered — callers must check the error. The analyzer is
// appended in registration order.

// addWarmPoolGPUs charges each namespace's warm pools into the managed usage
// about to be published, and returns the version of the pool figure it charged.
//
// Namespaces are ADDED, not merely updated: a namespace whose only WVA
// consumption is a pool has no scaling requests, so it would otherwise be absent
// from the managed figure entirely and its quota would read as untouched while
// the pool holds GPUs inside it.
func addWarmPoolGPUs(byType map[string]int, byNamespace map[string]map[string]int) uint64 {
	figure, version := decision.WarmPoolGPUsWithVersion()
	for namespace, pools := range figure {
		perType, ok := byNamespace[namespace]
		if !ok {
			perType = make(map[string]int)
			byNamespace[namespace] = perType
		}
		for accelerator, gpus := range pools {
			if gpus <= 0 {
				continue
			}
			perType[accelerator] += gpus
			byType[accelerator] += gpus
		}
	}
	return version
}

func (e *Engine) RegisterAnalyzer(name string, a domain.Analyzer) error {
	if e.started {
		return errors.New("RegisterAnalyzer: called after StartOptimizeLoop")
	}
	for i := range e.analyzers {
		if e.analyzers[i].name == name {
			return fmt.Errorf("RegisterAnalyzer: duplicate analyzer name %q", name)
		}
	}
	e.analyzers = append(e.analyzers, analyzerEntry{name: name, analyzer: a})
	return nil
}

// UpsertExternalAnalyzer registers or replaces a config-driven external analyzer
// by name. Unlike RegisterAnalyzer it is safe to call while the optimize loop is
// running (the analyzer participates from the next cycle), so the analyzer
// catalog can change without a restart. A name that collides with a built-in
// analyzer is still stored here but never runs — the built-in wins in
// analyzerRunEntries.
func (e *Engine) UpsertExternalAnalyzer(name string, a domain.Analyzer) {
	e.externalMu.Lock()
	defer e.externalMu.Unlock()
	e.externalAnalyzers[name] = a
}

// RemoveExternalAnalyzer retires a runtime external analyzer. Idempotent.
func (e *Engine) RemoveExternalAnalyzer(name string) {
	e.externalMu.Lock()
	defer e.externalMu.Unlock()
	delete(e.externalAnalyzers, name)
}

// analyzerRunEntries returns the analyzers to run this cycle: the frozen built-in
// snapshot followed by the runtime external analyzers (sorted by name for a
// deterministic order). An external analyzer whose name collides with a built-in
// is skipped — the built-in registry wins name resolution.
func (e *Engine) analyzerRunEntries() []analyzerEntry {
	builtinNames := make(map[string]struct{}, len(e.analyzersSnapshot))
	for _, en := range e.analyzersSnapshot {
		builtinNames[en.name] = struct{}{}
	}

	e.externalMu.RLock()
	defer e.externalMu.RUnlock()

	names := make([]string, 0, len(e.externalAnalyzers))
	for name := range e.externalAnalyzers {
		if _, dup := builtinNames[name]; dup {
			continue // built-in wins name resolution
		}
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]analyzerEntry, 0, len(e.analyzersSnapshot)+len(names))
	entries = append(entries, e.analyzersSnapshot...)
	for _, name := range names {
		entries = append(entries, analyzerEntry{name: name, analyzer: e.externalAnalyzers[name]})
	}
	return entries
}

// reconcileExternalAnalyzers rebuilds the runtime external-analyzer registry from
// the current catalog (Config.ExternalAnalyzerCatalog: analyzerDefinitions on
// the cluster "default" scaling entry, which the ConfigMap reconciler
// refreshes when the scaling-policy ConfigMap changes). Called once per optimize
// cycle, so a catalog edit takes effect without a restart. A catalog label that
// collides with a built-in analyzer is skipped (built-in wins name resolution); a
// definition that fails to build (bad threshold, empty query) is skipped and
// logged, never fatal.
func (e *Engine) reconcileExternalAnalyzers(ctx context.Context) {
	src := e.metricsRegistry.Get("prometheus")
	if src == nil {
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	catalog := e.Config.ExternalAnalyzerCatalog()

	builtin := make(map[string]struct{}, len(e.analyzersSnapshot))
	for _, en := range e.analyzersSnapshot {
		builtin[en.name] = struct{}{}
	}

	desired := make(map[string]struct{}, len(catalog))
	for label, def := range catalog {
		if _, isBuiltin := builtin[label]; isBuiltin {
			// Once per name at the default level, not every cycle at DEBUG:
			// a catalog entry that stopped running because a built-in took
			// its name (slo did) must be visible in a default log.
			if !e.reportedCollisions[label] {
				logger.Info("Skipping external analyzer: its name is a built-in analyzer's, and the built-in runs instead",
					"label", label)
				if e.reportedCollisions == nil {
					e.reportedCollisions = map[string]bool{}
				}
				e.reportedCollisions[label] = true
			}
			continue
		}
		d, err := toExternalDefinition(label, def)
		if err != nil {
			logger.Info("Skipping malformed external analyzer definition", "label", label, "error", err)
			continue
		}
		a, err := external.New(d, src)
		if a != nil {
			// The same Pod gate the builder applies, so every analyzer measures
			// the same population -- see collector.SeriesPodIsGone.
			a.DropSeries = e.ReplicaMetricsCollector.SeriesPodIsGone
			// And the cycle clock, so its AnalyzedAt is on the clock liveness
			// compares it with (updateLivenessAndSetLive).
			a.WithClock(e.Clock())
		}
		if err != nil {
			logger.Info("Skipping invalid external analyzer definition", "label", label, "error", err)
			continue
		}
		e.UpsertExternalAnalyzer(label, a)
		desired[label] = struct{}{}
	}

	// Retire external analyzers no longer present (or no longer valid) in the catalog.
	for _, name := range e.externalAnalyzerNames() {
		if _, ok := desired[name]; !ok {
			e.RemoveExternalAnalyzer(name)
		}
	}
}

// externalAnalyzerNames returns the names of the currently registered runtime
// external analyzers.
func (e *Engine) externalAnalyzerNames() []string {
	e.externalMu.RLock()
	defer e.externalMu.RUnlock()
	names := make([]string, 0, len(e.externalAnalyzers))
	for name := range e.externalAnalyzers {
		names = append(names, name)
	}
	return names
}

// toExternalDefinition converts a catalog entry to an external.Definition,
// parsing the KEDA-style string thresholds to float64. When the entry has no
// engines map it is treated as a single engine-agnostic body.
func toExternalDefinition(label string, def config.ExternalAnalyzerDef) (external.Definition, error) {
	bodies := make(map[string]external.Body)
	if len(def.Engines) > 0 {
		for engine, body := range def.Engines {
			th, err := strconv.ParseFloat(body.Threshold, 64)
			if err != nil {
				return external.Definition{}, fmt.Errorf("engine %q: threshold %q: %w", engine, body.Threshold, err)
			}
			bodies[engine] = external.Body{Query: body.Query, Threshold: th}
		}
	} else {
		th, err := strconv.ParseFloat(def.Threshold, 64)
		if err != nil {
			return external.Definition{}, fmt.Errorf("threshold %q: %w", def.Threshold, err)
		}
		bodies[""] = external.Body{Query: def.Query, Threshold: th}
	}
	return external.Definition{Label: label, Bodies: bodies}, nil
}

// StartOptimizeLoop starts the optimization loop for the saturation engine.
// It runs until the context is cancelled.
//
// Before launching the goroutine, the registered analyzers are snapshotted
// to a frozen slice that runAnalyzers iterates. The started flag is
// flipped so subsequent RegisterAnalyzer calls return an error. The snapshot is the
// natural place to invoke any future per-analyzer Init(ctx) hook.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.analyzersSnapshot = make([]analyzerEntry, len(e.analyzers))
	copy(e.analyzersSnapshot, e.analyzers)
	e.started = true
	// Before the first cycle, once every analyzer is registered.
	e.startLearnerRecording()
	defer e.stopLearnerRecording()

	e.recordActiveOptimizer() // record active optimizer
	metrics.SetConfigOptimizationInterval(float64(e.Config.OptimizationInterval().Seconds()))
	// Read the in-flight holds back before the first cycle, and as early as the
	// scale-from-zero engine starts: a wake builds its node view from the same
	// ledger.
	e.restoreInFlight(ctx, time.Now())
	e.executor.Start(ctx)
	// Start returns once the loop has stopped -- leadership lost or the
	// controller shutting down -- and after its last cycle, so this write
	// races none.
	e.flushLearnedState()
}

func (e *Engine) recordActiveOptimizer() {
	// Record metrics for which optimizer is active
	optimizerNames := []string{"greedy-by-score", "cost-aware"}
	for _, name := range optimizerNames {
		isActive := false // default is false
		if name == e.optimizer.Name() {
			isActive = true // only one active at a time
		}
		e.metricsEmitter.RecordOptimizerActiveMetric(name, isActive)
	}
}

func (e *Engine) recordDefaultConfigMetrics() {
	metrics.SetConfigOptimizationInterval(float64(e.Config.OptimizationInterval().Seconds()))

	globalSatCfgMap := e.Config.ScalingPolicyConfig()
	// record global default config
	//
	// The limiter_enabled label keeps its name and its meaning — "is scaling being
	// limited" — but now comes from whether any limiter is declared, since the
	// enableLimiter flag it used to read is gone. Renaming the label would break
	// the operational dashboard and the zero-allocatable-GPU alert, which both
	// reference it.
	if cfg, ok := globalSatCfgMap["default"]; ok {
		limiterEnabled := e.Config.EffectiveLimiterTypes().Declared()
		metrics.SetConfigInfo(cfg.GetAnalyzerName(), limiterEnabled, e.Config.ScaleToZeroEnabled())
	}
}

// SetLimiterBuilder installs a builder that rebuilds the GPU limiter from the
// current effective config. The engine calls it at the top of each optimize
// cycle and swaps the limiter when the declared limiters (types, quota entries)
// change — so switching the ConfigMap's limiters: list takes effect without a
// restart. Call before StartOptimizeLoop. Passing nil disables live rebuilds
// (the injected GPULimiter is used as-is, e.g. in tests).
func (e *Engine) SetLimiterBuilder(builder func() (allocation.Limiter, error)) {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	e.limiterBuilder = builder
	// Seed the signature from the current config so the first cycle does not
	// rebuild the limiter that was already constructed from the same config.
	e.limiterSig = limiterSignature(e.Config)
}

// refreshLimiter rebuilds GPULimiter when the effective limiter config changed
// since it was last built. A build error is logged and the previous limiter is
// kept, so a transient bad config never leaves the engine without a limiter.
func (e *Engine) refreshLimiter(ctx context.Context) {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	if e.limiterBuilder == nil {
		return
	}
	sig := limiterSignature(e.Config)
	if sig == e.limiterSig && e.GPULimiter != nil {
		return
	}
	limiter, err := e.limiterBuilder()
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to rebuild GPU limiter from config; keeping the previous limiter",
			"limiters", e.Config.EffectiveLimiterSummary())
		return
	}
	e.GPULimiter = limiter
	e.limiterSig = sig
	ctrl.LoggerFrom(ctx).Info("GPU limiter (re)built from config",
		"limiterTypes", e.Config.EffectiveLimiterTypes().String(), "limiters", e.Config.EffectiveLimiterSummary(),
		"name", limiter.Name())
	// Once per build, which is once per change of the limiters: the
	// controller-status key (clusterQuotaPerController) carries it between.
	if e.Config.ClusterQuotaPerController() {
		ctrl.LoggerFrom(ctx).Info(config.ClusterQuotaPerControllerMessage,
			"watchNamespace", e.Config.WatchNamespace(), "limiters", e.Config.EffectiveLimiterSummary())
	}
}

// limiterSignature detects when the limiter config changed. Delegates to the
// shared implementation so this engine and the scale-from-zero engine cannot
// disagree about what a change is.
func limiterSignature(cfg *config.Config) string {
	return allocation.LimiterSignature(cfg)
}

// currentGPULimiter returns the active GPU limiter, read under limiterMu so it is
// safe against a concurrent live rebuild in refreshLimiter (the field is written
// only there, also under the lock).
func (e *Engine) currentGPULimiter() allocation.Limiter {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	return e.GPULimiter
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) (retErr error) {
	start := time.Now()
	var modelsProcessed int
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
			if e.sloFailedSince.IsZero() {
				e.sloFailedSince = start
			}
		}
		metrics.ObserveOptimizationDuration(time.Since(start).Seconds(), status)
		metrics.SetModelsProcessed(modelsProcessed)
	}()

	logger := ctrl.LoggerFrom(ctx)
	e.restoreInFlight(ctx, time.Now()) // once: the in-flight holds a restart would lose, before any node view
	e.refreshLimiter(ctx)              // rebuild the GPU limiter if the ConfigMap changed its type/entries
	e.reconcileExternalAnalyzers(ctx)  // sync the runtime external-analyzer registry with the catalog
	e.recordDefaultConfigMetrics()     // record as soon as possible to reflect any changes in configuration
	e.observeUtilizationShareMode(ctx) // every cycle, even one with no model to plan
	metrics.SweepSLO(time.Now())       // drop the SLO series of a model no longer published

	// Per-cycle progress lines that repeat what scaling-decision and the
	// applied-decision line already say sit at logging.VERBOSE: at the shipped
	// verbosity they were written every cycle, per model or per variant, and
	// carried nothing new. Blocked reasons and warnings are not among them.
	if e.Config.ScaleToZeroEnabled() {
		logger.V(logging.VERBOSE).Info("Scaling to zero is enabled")
	}

	e.VariantEnricher.Refresh(ctx) // resolve the scale target of anything newly registered

	activeVAs, _, err := utils.ActiveVariantAutoscaling(ctx, e.client, e.Variants)
	if err != nil {
		logger.Error(err, "Unable to get active variant autoscalings")
		return err
	}

	if len(activeVAs) == 0 {
		// A cycle, though it analyzes nothing: the models parked through it
		// were not watched, and the slo analyzer must see that it skipped them.
		e.beginSLOCycle()
		logger.Info("No active VariantAutoscalings found, skipping optimization")
		metrics.PublishUtilizationShare(nil) // no group to report: the share gauges go too
		// This cycle analyzes nothing, so no analyzer series gets refreshed and
		// the per-model prune below is never reached. Absence is meaningful for
		// those series: left alone they would hold their last busy-cycle values
		// indefinitely — a scale-to-zero fleet idling overnight would keep
		// reporting the demand it saw at peak, with no other series to
		// contradict it. This is a genuine empty state, not a transient one: a
		// failed list returns above, so reaching here means there really are no
		// active models.
		e.evictAllAnalyzerSeries()
		// Same argument, sharper consequence: a blocked-reason series asserts that
		// a named model will never park. With no models left, every such assertion
		// is about a workload that is gone.
		e.evictAllBlockedModels()

		// The PHYSICAL picture is not published from here — internal/gpuusage
		// discovers it independently, precisely so a parked fleet still has one.
		//
		// The MANAGED picture is, and it is published as ZERO. Unlike the physical
		// figure, "WVA's variants hold no GPUs" is a statement this path can make
		// with confidence: the list above succeeded, and every variant it returned
		// is at zero replicas. It is also the statement that has to be made. A
		// quota is charged only for what WVA holds, so a fleet parked at zero has
		// spent none of its allowance — and withholding that would leave the
		// scale-from-zero engine unable to establish it from anywhere else, refusing
		// every wake under a quota limiter for want of a figure only a running fleet
		// could produce. That is a deadlock: nothing running, so nothing published,
		// so nothing may start.
		//
		// The per-namespace view is empty here, not zeroed per namespace: there is
		// no population to enumerate namespaces from. Consumers materialise the
		// namespace they are deciding about (see scalefromzero gpuConstraints), which
		// is what keeps a namespace-scoped quota applying to a parked namespace.
		decision.PublishManagedGPUUsage(map[string]int{}, map[string]map[string]int{})
		// And what is LEFT of each allowance, which nothing else here would say.
		//
		// Headroom is otherwise published from inside the optimizer, so a
		// namespace with no models never gets an answer -- and the warm pool,
		// which is the only reader, treats no answer as unbounded and grows past
		// the quota it is charged against. A namespace holding only a pool is
		// exactly that case, and it is not a corner: a shared pool namespace has
		// no models by construction.
		e.publishHeadroomForIdleFleet(ctx)
		return nil
	}

	// Initialize vaEventTracker for this optimize cycle
	e.vaEventTracker = make(map[string]bool)

	// Open the collector's cycle so the models grouped below share one execution
	// of each namespace-scoped query instead of one per model.
	if e.ReplicaMetricsCollector != nil {
		e.ReplicaMetricsCollector.BeginCycle()
		defer e.ReplicaMetricsCollector.EndCycle()
	}

	// Collect accelerator inventory (only in limited mode AND only when a physical
	// limiter is declared). With quota entries alone, the controller deliberately
	// runs without consulting physical capacity —
	// listing Nodes here would defeat that contract (and trigger a
	// controller-runtime Node informer for the lifetime of the process). The
	// collected inventory is currently only logged anyway (see comment at
	// internal/collector/collector.go), so skipping it in quota mode loses
	// nothing of operational value.
	if e.Config.LimitedModeEnabled() && scalingpolicy.ShouldCollectClusterInventory(e.Config) {
		inventory, err := collector.CollectInventoryK8S(ctx, e.client)
		if err != nil {
			logger.Error(err, "Failed to collect cluster inventory")
			// do not proceed to optimization if inventory collection fails in limited mode
			return err
		}
		// always print inventory until optimizer consumes it
		logger.Info("Collected cluster accelerator inventory (Limited Mode)", "inventory", inventory)
	}
	// The slo analyzer's cycle starts here, past every early error return: the
	// executor retries a failed cycle within a fraction of a second, and
	// counting the failure and its retry as two cycles read as a skipped one,
	// leaving every SLO model unobserved for a window and its grace.
	e.beginSLOCycle()

	// Group VAs by model for per-model capacity analysis
	modelGroups := utils.GroupVariantAutoscalingByModel(activeVAs)
	modelsProcessed = len(modelGroups)
	logger.V(logging.VERBOSE).Info("Grouped VAs by model",
		"modelCount", len(modelGroups),
		"totalVAs", len(activeVAs))

	// Create VA lookup map for applySaturationDecisions (used to access VA status and update decisions)
	// Use namespace/vaName as key to avoid collisions when multiple namespaces have same VA name
	// Use slice index directly to avoid pointer-to-loop-variable bug
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, len(activeVAs))
	for i := range activeVAs {
		vaMap[utils.GetNamespacedKey(activeVAs[i].Namespace, activeVAs[i].Name)] = &activeVAs[i]
	}

	// Create map to store current allocations populated during metrics collection
	// Keyed by VariantAutoscaling Namespace/Name
	currentAllocations := make(map[string]*domain.Allocation)

	// Read saturation config for analyzer selection.
	globalSatCfgMap := e.Config.ScalingPolicyConfig()
	analyzerName := ""
	if cfg, ok := globalSatCfgMap["default"]; ok {
		cfg.ApplyDefaults()
		analyzerName = cfg.GetAnalyzerName()
	}

	// Select the optimizer from the declared limiters (both are stateless, safe to
	// swap). A declared limiter IS the request to be limited — the separate
	// enableLimiter flag is gone, so a quota can no longer be declared and then
	// silently not enforced, which is what that flag allowed. V2
	// (saturation-token-based) is the sole analysis path and always uses the
	// optimizer pipeline.
	savedOptimizer := e.optimizer
	if !e.Config.EffectiveLimiterTypes().Declared() {
		e.optimizer = allocation.NewCostAwareOptimizer()
	} else {
		e.optimizer = allocation.NewGreedyByScoreOptimizer()
	}
	if savedOptimizer != e.optimizer {
		e.recordActiveOptimizer() // optimizer has changed, record active optimizer
	}
	logger.V(logging.DEBUG).Info("Optimizer selected", "analyzer", analyzerName,
		"optimizer", e.optimizer.Name(), "limiter", e.Config.EffectiveLimiterSummary())

	// V2 (saturation): saturation.Analyzer → AnalyzerResult → Optimizer.Optimize → Enforcer bridge.
	mode := modeLabelForAnalyzer(analyzerName)
	allDecisions := e.optimizeV2(ctx, modelGroups, currentAllocations)

	// STEP 3: Apply decisions and update VA status
	// Always call applySaturationDecisions, even with empty decisions.
	// This function also updates VA.Status.CurrentAlloc with collected metrics
	// and emits HPA metrics, which must happen every reconciliation cycle.
	if len(allDecisions) > 0 {
		logger.V(logging.VERBOSE).Info("Applying scaling decisions",
			"totalDecisions", len(allDecisions))
	} else {
		logger.Info("No scaling decisions to apply, updating VA status with metrics")
	}
	e.applySaturationDecisions(ctx, allDecisions, vaMap, currentAllocations)

	logger.Info("Optimization completed successfully",
		"mode", mode,
		"modelsProcessed", len(modelGroups),
		"decisionsApplied", len(allDecisions))

	return nil
}

// modeLabelForAnalyzer returns the human-readable mode label for the given
// analyzer name, used in the "Optimization completed successfully" log entry.
// V2 is the sole analysis path; the label distinguishes a config that named the
// saturation analyzer from one that left it unset.
func modeLabelForAnalyzer(analyzerName string) string {
	switch analyzerName {
	case domain.SaturationAnalyzerName:
		return domain.SaturationAnalyzerName
	default:
		return "saturation-only"
	}
}

// constrainedMessage is a ResourceConstrained event's message: the decision's
// reason and what held it back (LimitedBy) -- a limiter's name, or
// allocation.NodeFitLimiterName when the pools held the GPUs but no node held
// a whole replica -- with every limiter tied with it (LimitedByAlso).
func constrainedMessage(d *domain.VariantDecision) string {
	if d.LimitedBy == "" {
		return d.Reason()
	}
	return fmt.Sprintf("%s (limited by %s)", d.Reason(), strings.Join(append([]string{d.LimitedBy}, d.LimitedByAlso...), " and "))
}

// recordEvent ensures only one event is recorded per VA in an optimization cycle.
// Exception: K8SEventResourceConstrained events bypass deduplication and can be
// recorded alongside other event types (e.g., ScaledUp + ResourceConstrained).
func (e *Engine) recordEvent(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	eventType, reason, message string,
) {
	if e.Recorder == nil {
		return
	}

	target := llmdVariantAutoscalingV1alpha1.EventTarget(va)

	if reason == constants.K8SEventResourceConstrained {
		// This is the only exception where a variant can have 2 K8S events in an optimize cycle: K8SEventScaledUp & K8SEventResourceConstrained
		e.Recorder.Event(target, eventType, reason, message)
		return
	}

	key := utils.GetNamespacedKey(va.Namespace, va.Name)
	if e.vaEventTracker != nil {
		if _, ok := e.vaEventTracker[key]; ok { // ensures only one event is recorded per VA
			return
		}
	}
	e.Recorder.Event(target, eventType, reason, message)
	if e.vaEventTracker != nil {
		e.vaEventTracker[key] = true
	}
}

func (e *Engine) recordOptimizationFailedEvent(
	variantAutoscalings []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	message string,
) {
	if e.Recorder == nil {
		return
	}
	for _, va := range variantAutoscalings {
		e.recordEvent(&va, corev1.EventTypeWarning, constants.K8SEventOptimizationFailed, message)
	}
}

func (e *Engine) recordScalingEvent(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	action domain.SaturationAction,
	targetReplicas int,
	reason string,
) {
	if e.Recorder == nil {
		return
	}
	switch action {
	case domain.ActionScaleUp:
		e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, reason)
	case domain.ActionScaleDown:
		if targetReplicas == 0 {
			e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledToZero, reason)
		} else {
			e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, reason)
		}
	}
}

// resolveModelPolicy resolves the effective config for a model, layering in the
// policy tier its variants selected via trigger metadata.
//
// The policy is resolved for the MODEL, not per variant, because that is the unit
// WVA scales: the optimizer distributes the model's replicas across its variants
// against one set of thresholds. Variants that disagree are a configuration error,
// reported and resolved deterministically rather than silently half-applied.
func (e *Engine) resolveModelPolicy(
	ctx context.Context,
	configMap map[string]config.ScalingPolicy,
	modelID, namespace string,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) config.ScalingPolicy {
	tier, conflicting := modelPolicy(vas)
	if len(conflicting) > 1 {
		e.policies.ReportPolicyConflict(ctx, namespace, modelID, conflicting, tier)
	}

	// A named-but-absent tier resolves to the default entry, which is the right
	// outcome and the wrong silence — report it against the variants that asked.
	if tier != "" {
		if entry, ok := configMap[tier]; !ok || !config.PolicyEntryKey(tier, entry) {
			known := slices.Sorted(maps.Keys(config.NamedPolicies(configMap)))
			for i := range vas {
				if vas[i].Spec.ScalingPolicy == tier {
					e.policies.ReportUnknownPolicy(ctx, namespace, vas[i].Name, tier, known)
				}
			}
		}
	}

	resolved := config.ResolveScalingPolicyForTier(configMap, modelID, namespace, tier)

	// Fold in the seeds the model's triggers carry. The ConfigMap cannot express
	// these -- they are facts about one workload, not thresholds a class of
	// workloads shares -- and the trigger is the most specific layer there is, so
	// it overlays last.
	if seed, conflicting := e.modelOutputSeed(vas); seed > 0 {
		resolved.DefaultOutputTokens = seed
		if len(conflicting) > 1 {
			e.policies.ReportOutputSeedConflict(ctx, namespace, modelID, conflicting, seed)
		}
	}

	e.policies.ReportEffectivePolicy(ctx, namespace, modelID, tier, resolved)
	e.policies.ReportSLOMisconfiguration(ctx, namespace, modelID, resolved.SLOMisconfiguration())
	return resolved
}

// modelOutputSeed resolves one expected generation length for a model from the
// trigger metadata of its variants, and reports every distinct figure they named
// when they disagree.
//
// The LARGEST wins, not the smallest and not the first. A model under P/D has a
// ScaledObject per role, and only one of those roles generates tokens: a prefill
// trigger carrying nothing must not pull decode's figure down to the built-in
// fallback. The asymmetry is the same one the key exists for -- under-pricing a
// queue is what costs time-to-first-token, and over-pricing it costs a replica.
//
// Reading the registry rather than the synthetic VariantAutoscaling spec is
// deliberate: that CR is being removed, so a spec field added for this would be
// dead on arrival. The ScaledObject names the variant, so va.Name is the registry
// key -- see registrySourcedVariants.
func (e *Engine) modelOutputSeed(
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (int, []int) {
	if e.Variants == nil {
		return 0, nil
	}
	var named []int
	best := 0
	for i := range vas {
		entry, ok := e.Variants.Get(vas[i].Namespace, vas[i].Name)
		if !ok {
			continue
		}
		// ParseMeta, not a direct map read: a trigger whose metadata it rejects
		// has already been skipped everywhere else, and honouring one key off an
		// otherwise unusable block would give this key its own rules.
		meta, err := registry.ParseMeta(entry.Metadata)
		if err != nil || meta.DefaultOutputTokens <= 0 {
			continue
		}
		if !slices.Contains(named, meta.DefaultOutputTokens) {
			named = append(named, meta.DefaultOutputTokens)
		}
		if meta.DefaultOutputTokens > best {
			best = meta.DefaultOutputTokens
		}
	}
	slices.Sort(named)
	return best, named
}

// modelPolicy returns the policy tier a model scales under, plus every distinct
// tier its variants named when they disagree.
//
// Ties break on the lexicographically smallest name so a conflict resolves the
// same way on every cycle and every replica of the controller: an allocation that
// flips between tiers as map order changes would be worse than either tier.
//
// The choice is config.ModelPolicyTier, shared with the scale-from-zero wake
// claim so the two cannot weigh a model under different tiers.
func modelPolicy(vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling) (string, []string) {
	named := make([]string, 0, len(vas))
	for i := range vas {
		named = append(named, vas[i].Spec.ScalingPolicy)
	}
	tier := config.ModelPolicyTier(named)
	if tier == "" {
		return "", nil
	}
	return tier, config.ModelTiers(named)
}

// variantTiers returns every policy tier a model's variants selected
// (config.ModelTiers). A variant that names no tier follows the ones that do,
// as it does for the thresholds (modelPolicy); "" stands for the default entry
// and its defaultPolicy only when no variant names a tier.
//
// It used to keep "" beside the named tiers, so a P/D model with only its
// decode tagged was also folded with the default entry: its thresholds
// followed the decode's tier while a default that disabled scale-to-zero kept
// the whole model, every GPU of it, from ever parking.
func variantTiers(vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling) []string {
	named := make([]string, 0, len(vas))
	for i := range vas {
		named = append(named, vas[i].Spec.ScalingPolicy)
	}
	return config.ModelTiers(named)
}

// scaleToZeroPolicy resolves the scale-to-zero half of a model's policy from
// the tier each of its variants selected.
//
// Parking takes the WHOLE model to zero, so it is resolved more strictly than
// the thresholds, which take one tier for the model (see modelPolicy): a model
// may park only if EVERY tier its variants selected allows scale-to-zero, and it
// is held for the LONGEST retention among them. Parking is the action an
// operator cannot undo from the outside -- a variant on a tier that forbids it
// would otherwise be parked by a sibling on a tier that allows it. When every
// variant names the same tier, as is normal, this is exactly that tier's
// resolution.
//
// The result is the first tier's resolved entry with its scaleToZero envelope
// replaced by the folded verdict; the enforcer and the blocked-reason readout
// read only that envelope, so both see the same answer. disabledBy lists the
// tiers whose resolution disables scale-to-zero ("" for the default entry), so
// a policy-forbids-zero on a model with mixed tiers can be traced to one.
func scaleToZeroPolicy(
	cfg *config.Config,
	configMap map[string]config.ScalingPolicy,
	modelID, namespace string,
	tiers []string,
) (policy config.ScalingPolicy, disabledBy []string) {
	if len(tiers) == 0 {
		tiers = []string{""}
	}
	var out config.ScalingPolicy
	enabled := true
	var retention time.Duration
	for i, tier := range tiers {
		resolved := config.ResolveScalingPolicyForTier(configMap, modelID, namespace, tier)
		if i == 0 {
			out = resolved
		}
		if !config.ResolveScaleToZeroEnabled(cfg, &resolved) {
			enabled = false
			disabledBy = append(disabledBy, tier)
		}
		retention = max(retention, config.ResolveScaleToZeroRetention(&resolved))
	}
	out.ScaleToZero = &config.ScaleToZeroEnvelope{
		Enabled:         &enabled,
		RetentionPeriod: retention.String(),
	}
	return out, disabledBy
}

// scaleToZeroForbiddenMessage is the log line that names the tiers disabling a
// model's scale-to-zero. The alert WVAModelScalingBlocked quotes its start.
const scaleToZeroForbiddenMessage = "Scale-to-zero is disabled by the scaling policy of a tier this model's variants name " +
	"(its per-model entry, if any, included)"

// scaleToZeroForbiddenFix says where to enable scale-to-zero for a model whose
// resolved policy disables it, and whether the model's per-model entry is what
// disables it.
//
// Each tier is resolved with the model's per-model entry merged in, so a
// per-model enabled: false disables every tier at once; naming the tiers then
// sends the operator to the wrong place. disabledBy is spelled by tierNames.
func scaleToZeroForbiddenFix(
	configMap map[string]config.ScalingPolicy,
	modelID, namespace string,
	disabledBy []string,
) (fix string, perModel bool) {
	if override, ok := config.FindModelOverride(configMap, modelID, namespace); ok &&
		override.ScaleToZero != nil && override.ScaleToZero.Enabled != nil && !*override.ScaleToZero.Enabled {
		return "enable scaleToZero on the model's per-model entry", true
	}
	if slices.Equal(disabledBy, []string{config.GlobalDefaultsKey}) {
		return "no variant names a tier: enable scaleToZero on the default entry (or the tier its defaultPolicy names), " +
			"or on the model's per-model entry", false
	}
	return "enable scaleToZero on each tier in disabledBy, or on the model's per-model entry", false
}

// tierNames spells policy tiers for a log line: "" is the default entry.
func tierNames(tiers []string) []string {
	out := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		if tier == "" {
			tier = config.GlobalDefaultsKey
		}
		out = append(out, tier)
	}
	return out
}

// selectV2Optimizer chooses the optimizer and GPU constraints for a V2
// optimization cycle.
//
// When the configured optimizer is GreedyByScore it is GPU-aware and expects
// per-type resource constraints. Those constraints are computed from the GPU
// limiter's inventory. GreedyByScore interprets absent constraints as zero
// available capacity (deny-all), NOT as unlimited — so if it were run with
// empty constraints it would silently suppress all scale-up.
//
// A provider whose usage basis has not been observed, or whose constraints
// cannot be computed (e.g. node objects are not readable on this cluster), is
// SKIPPED and named in the log; the other providers still bound the cycle (see
// allocation.ComputeAvailableConstraints). Only when no provider can supply
// constraints -- the limiter provides none, or every one is unavailable -- do we
// fall back to the cost-aware optimizer, which is the engine's unlimited path
// (the same optimizer used when no limiter is declared), so scale-up proceeds
// unconstrained instead of being blocked. A constraint that is present but
// reports zero GPUs is left intact: that is a genuine "no capacity" signal and
// should still block scale-up.
//
// Providers do not all consume the same usage figure. A physical inventory is fed
// every GPU held on the cluster's nodes; a quota is fed only what WVA's own
// variants hold, because an allowance granted to WVA may only be spent by WVA.
// See allocation.UsageBasis and gpuUsageViews.
func (e *Engine) selectV2Optimizer(
	ctx context.Context,
	requests []allocation.ModelScalingRequest,
) (allocation.ScalingOptimizer, []*allocation.ResourceConstraints) {
	logger := ctrl.LoggerFrom(ctx)

	// Collect constraints from every provider backing the GPU limiter: a single
	// DefaultLimiter, or each constituent of a CompositeLimiter (so multi-entry
	// quota configs are all consulted, and namespace-scoped providers contribute
	// per-namespace caps via NamespacePools).
	//
	// FIRST, before the optimizer guard below. With no limiter declared,
	// optimize() installs the cost-aware optimizer and that guard returns, so a
	// publish placed after it would never run for a limiter-less fleet that has
	// something to optimize -- and the warm pool, which now waits for a
	// snapshot before it grows, would be held at its size for as long as any
	// model was registered.
	providers := gpuConstraintProviders(e.currentGPULimiter())
	if len(providers) == 0 {
		// Nothing bounds anything -- said out loud, because the warm pool waits
		// for a headroom snapshot before it grows and nothing else here would
		// publish one on a cluster with no limiter.
		e.providerSkips.report(ctx, nil)
		allocation.PublishUnboundedHeadroom(decision.WarmPoolGPUsVersion(), time.Now())
		return allocation.NewCostAwareOptimizer(), nil
	}

	// GreedyByScore is currently the only GPU-aware optimizer; any future
	// constraint-consuming optimizer must be added to this guard.
	optimizer := e.optimizer
	if _, ok := optimizer.(*allocation.GreedyByScoreOptimizer); !ok {
		// Providers exist but the optimizer is not GPU-aware: a limiter edit to
		// none whose rebuild failed leaves the previous limiter in place under
		// the cost-aware optimizer. The models scale unbounded; so may the pool.
		allocation.PublishUnboundedHeadroom(decision.WarmPoolGPUsVersion(), time.Now())
		return optimizer, nil
	}

	// Each provider is fed the measure of usage IT asked for: physical inventories
	// the GPUs actually held on the nodes, quotas only what WVA's own variants
	// hold. One figure broadcast to both answers the wrong question for one of
	// them — see allocation.UsageBasis.
	views := gpuUsageViews(requests)
	constraints, skipped := allocation.ComputeAvailableConstraints(ctx, providers, views,
		func(cp allocation.ConstraintProvider, constraint *allocation.ResourceConstraints,
			usageByType map[string]int, usageByNS map[string]map[string]int) {
			// What the limiter decided this cycle, as numbers.
			//
			// Nothing reported this, and the gap is expensive: a quota that fails
			// to bind and a quota that was never declared look identical from
			// outside, and the three things that could be wrong -- the cap, the
			// usage charged against it, and whether the namespace was
			// materialized at all -- are each invisible. Chasing one cost a day
			// of fifteen-minute cluster runs that this line would have answered
			// on the first.
			logger.V(logging.DEBUG).Info("GPU constraints computed",
				"provider", cp.Name(),
				"basis", allocation.UsageBasisOf(cp).String(),
				"usageByType", usageByType,
				"usageByNamespace", usageByNS,
				"pools", constraint.Pools,
				"namespacePools", constraint.NamespacePools,
				"totalLimit", constraint.TotalLimit,
				"totalUsed", constraint.TotalUsed,
				"totalAvail", constraint.TotalAvail)
		})
	// Published as wva_constraint_provider_skipped and logged when the set
	// changes, so a bound that stopped applying is visible without a log line
	// every cycle. A Kueue entry never read is not among them: it is in the
	// decision, its finite caps bound, and wva_kueue_never_read reports it.
	e.providerSkips.report(ctx, skipped)

	// GreedyByScore treats absent constraints as zero available capacity
	// (deny-all), not as unlimited. When no provider could supply constraints
	// -- every one's usage view is unobserved, or every one failed (the usual
	// cause is the Node API, for an inventory limiter without node read) --
	// fall back to the cost-aware optimizer, the engine's unlimited path, so
	// scale-up proceeds instead of being silently blocked. Constraints built on
	// an absent usage figure would not be "unknown", they would be a confident
	// claim that nothing is running, which is why an unobserved basis skips its
	// provider instead of feeding it nothing.
	if len(constraints) == 0 {
		// The models scale unbounded this cycle; the pool follows the same
		// posture rather than being held against a limit nobody could compute.
		allocation.PublishUnboundedHeadroom(views.PoolsVersion, time.Now())
		return allocation.NewCostAwareOptimizer(), nil
	}
	// With the physical limiter among the constraints, a replica must fit a
	// node, not just the pool. A physical limiter that was skipped is not
	// among them, and its node fit goes with it (the skip is reported above).
	nodes, reason := allocation.NodeViewFor(constraints, e.utilizationShare.nodes, time.Now())
	// The pass sizes every variant from its current replicas and re-places
	// the growth whose pods are not created yet: the holes the ledger holds
	// for those replicas are theirs to re-place.
	allocation.DefaultInFlight.ReleaseUncreated(nodes, createdReplicas(requests))
	e.reportNodeFitSkipped(ctx, allocation.PhysicalBounds(constraints), reason)
	if g, ok := optimizer.(*allocation.GreedyByScoreOptimizer); ok {
		g.Nodes = nodes.WithLayoutLog(&e.layouts)
	}
	// What each namespace may still take, for the warm pool, is published by
	// the optimizer once its pass is over -- every pass, a fleet of steady
	// models included -- from what the pass left (decideV2 republishes it
	// with the utilization share's new promises withheld). Published here, it
	// was the figure before the pass, which still counted the GPUs the pass
	// was about to order as free.
	return optimizer, constraints
}

// reportNodeFitSkipped logs, when it changes, that the physical limiter bounds
// the cycle (bounded) but replicas are fitted to the pool and not to nodes,
// and why; and once more when nodes are fitted again. A cycle the physical
// limiter does not bound fits nothing to nodes and says nothing: no physical
// limiter is not "fitted again", and the next skip is reported afresh.
func (e *Engine) reportNodeFitSkipped(ctx context.Context, bounded bool, reason string) {
	if !bounded {
		e.nodeFitSkipped = ""
		return
	}
	if reason == e.nodeFitSkipped {
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	if reason != "" {
		logger.Info("Physical limiter: replicas are not fitted to nodes this cycle", "reason", reason)
	} else {
		logger.Info("Physical limiter: replicas are fitted to nodes again")
	}
	e.nodeFitSkipped = reason
}

// resolveRescaleFlags builds the scope-coupled rescale enablement for this cycle:
// the cluster flag from the global saturation `default` config, plus a per-namespace
// flag from each active namespace's OWN `default` config (never the global fallback,
// so the cluster flag cannot enable rescale on a namespace quota).
func (e *Engine) resolveRescaleFlags(requests []allocation.ModelScalingRequest) allocation.RescaleFlags {
	flags := allocation.RescaleFlags{Cluster: e.Config.RescaleEnabledCluster()}
	seen := make(map[string]bool)
	for _, req := range requests {
		ns := req.Namespace
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		if enabled, hasLocal := e.Config.RescaleEnabledForNamespaceLocal(ns); hasLocal && enabled {
			if flags.ByNamespace == nil {
				flags.ByNamespace = make(map[string]bool)
			}
			flags.ByNamespace[ns] = true
		}
	}
	return flags
}

// optimizeV2 runs the V2 token-based optimizer path (saturation-token-based).
// Collects AnalyzerResults for all models, calls the optimizer once, then applies enforcer per-model.
func (e *Engine) optimizeV2(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*domain.Allocation,
) []domain.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)

	// Prune liveness state for departed models. Any model still present in
	// modelGroups is active this cycle (config-not-loaded or transient-failure
	// models are enumerated but skipped below — they have not departed); keys
	// absent from modelGroups belong to models whose VariantAutoscalings are
	// gone, so their lastGoodAnalysis entries are evicted here. Keyed identically
	// to updateLivenessAndSetLive (utils.GetNamespacedKey(namespace, modelID)).
	activeKeys := make(map[string]bool, len(modelGroups))
	for _, modelVAs := range modelGroups {
		activeKeys[utils.GetNamespacedKey(modelVAs[0].Namespace, modelVAs[0].Spec.ModelID)] = true
	}
	e.pruneLastGoodAnalysis(activeKeys)
	e.pruneAnalyzerSeries(activeKeys)
	e.pruneBlockedModels(activeKeys)
	// evaluated are the models whose scale-to-zero stage 3 resolves this cycle.
	// A model skipped by a cycle -- config not loaded, collection failed, or
	// every model failed and the cycle returned early -- has its "disabled by
	// tier" record forgotten, so the line is said again when it comes back.
	evaluated := make(map[string]bool, len(modelGroups))
	defer func() { e.forgetForbiddenBy(evaluated) }()
	// One clock reading for the cycle: from here until optimizeV2 returns,
	// the analyzers, the capacity store, the learned-state snapshot and the
	// analyzer liveness stamp and age on this one instant (e.clock). The
	// engine's own timers -- the sticky scale-down memory below, in-flight
	// orders, borrowing and the headroom reading -- still read time.Now.
	defer e.clock.Pin()()
	e.pruneLastDecided(e.stickyAge(), time.Now())
	// The learned-state snapshot, restored once before anything is learned or
	// swept, and written after the cycle has learned what it will.
	e.restoreLearnedState(ctx, e.clock.Now())
	defer func() { e.writeLearnedState(ctx, e.clock.Now()) }()

	// Before the sweep, which would otherwise judge a variant on a stamp from
	// before it parked: Analyze stamps too, but only after this point.
	e.notePresentVariants(ctx, modelGroups)
	e.evictStaleLearnedState(ctx)
	e.scaleTargetUIDs = make(map[string]types.UID)

	// Stage 1: Collect ModelScalingRequests for all models
	requests := make([]allocation.ModelScalingRequest, 0, len(modelGroups))
	// modelReplicaMetrics collects per-model replica metrics for KV token enrichment
	modelReplicaMetrics := make(map[string][]domain.ReplicaMetrics)
	// modelScaleTargets carries each model's scale targets into stage 3, where
	// applyScaleToZeroEnforcement needs them to gate the enforcer. Captured here
	// because data.scaleTargets is only in scope during this collection loop.
	modelScaleTargets := make(map[string]map[string]scaletarget.ScaleTargetAccessor)
	// variantScaleTargets is the same accessors keyed by namespace/variant, for
	// the utilization-share optimizer, which plans variants. The model's map
	// above is keyed by scale-target name, and a variant is rarely named after
	// its Deployment (a KEDA-discovered variant is named after its
	// ScaledObject).
	variantScaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	// modelTiers carries the policy tiers each model's variants selected into
	// stage 3, for the same reason: scale-to-zero is resolved from them there.
	modelTiers := make(map[string][]string)

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (V2)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		// Get namespace-aware saturation config
		saturationConfigMap := e.Config.ScalingPolicyConfigForNamespace(namespace)
		if len(saturationConfigMap) == 0 {
			logger.Info("Saturation scaling config not loaded yet for namespace, skipping model",
				"namespace", namespace, "modelID", modelID)
			continue
		}

		scalingPolicyConfig := e.resolveModelPolicy(ctx, saturationConfigMap, modelID, namespace, modelVAs)
		data, err := e.prepareModelData(ctx, modelID, modelVAs, e.client)
		if err != nil {
			msg := "Model data preparation failed"
			logger.Error(err, msg, "modelID", modelID)
			// Nothing measured the load this cycle: the series go once
			// collection has failed for balance.StaleAfter, not on one
			// failed cycle (loadBalanceCollectionFailed).
			e.loadBalanceCollectionFailed(namespace, modelID)
			e.recordOptimizationFailedEvent(modelVAs, msg)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			reportSignalReason(namespace, modelID, nil)
			continue
		}
		if data == nil {
			logger.V(logging.DEBUG).Info("Skipping model: no metrics available", "modelID", modelID)
			reportSignalReason(namespace, modelID, nil)
			continue
		}

		req, err := e.collectV2ModelRequest(ctx, modelID, namespace,
			data.replicaMetrics, scalingPolicyConfig, data.variantStates, data.variantMetadata,
			data.scaleTargets, data.variantAutoscalings, data.schedulerQueue, data.arrivalRate)
		if err != nil {
			msg := "V2 analysis failed"
			logger.Error(err, msg, "modelID", modelID)
			e.recordOptimizationFailedEvent(modelVAs, msg)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, data.scaleTargets)
			reportSignalReason(namespace, modelID, nil)
			continue
		}

		reportSignalReason(namespace, modelID, req)

		requests = append(requests, *req)
		modelReplicaMetrics[modelID] = data.replicaMetrics
		modelScaleTargets[utils.GetNamespacedKey(namespace, modelID)] = data.scaleTargets
		for i := range modelVAs {
			va := &modelVAs[i]
			if t, ok := data.scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]; ok {
				variantScaleTargets[utils.GetNamespacedKey(va.Namespace, va.Name)] = t
			}
		}
		modelTiers[utils.GetNamespacedKey(namespace, modelID)] = variantTiers(modelVAs)
	}

	if len(requests) == 0 {
		// Active variants, none of which produced a request: every model FAILED
		// collection this cycle. Nothing is published for the warm pool here,
		// deliberately, for the reason the managed figure is not published
		// either (below): a view built without the models' usage would credit
		// the namespace with every GPU they hold, and the pool would grow into
		// it. The previous snapshot stands until it ages out, so a transient
		// failure costs the pool nothing; a persistent one holds the pool, as
		// it should, beside models that are not being optimized either.
		return nil
	}

	// The PHYSICAL usage snapshot is NOT published from here any more. It is
	// discovered independently by internal/gpuusage, which is the sole producer, so
	// this path no longer decides what the rest of WVA believes about the cluster —
	// a cycle where every model failed collection can no longer withhold the
	// picture, and a fleet parked at zero (which returns long before this) no longer
	// leaves scale-from-zero with no picture at all.
	//
	// The MANAGED view is published from here, because this population is the only
	// place it exists: it is what WVA's own variants hold, which is the sole
	// consumption an operator-declared quota may be charged for (see
	// decision.DefaultManagedGPUUsage). It is shared for the scale-from-zero
	// engine's benefit — that engine has no population of its own to sum, and a
	// wake still has to be judged against the allowance.
	//
	// Publishing only past the len(requests) == 0 guard above is deliberate. An
	// empty population there means collection FAILED, not that nothing is running,
	// and publishing zeros for it would hand every quota its full allowance back at
	// exactly the moment WVA cannot see what it is holding. The genuinely-empty
	// case is published separately, from the path that can tell the difference.
	managedByType := computeCurrentGPUUsage(requests)
	managedByNamespace := computeCurrentGPUUsageByNamespace(requests)
	// WVA's own warm pools are charged here too. A pool Pod is not a variant, so
	// it is absent from `requests` -- but a quota is an allowance granted to WVA,
	// and a pool exists because WVA asked for it and is sized by what WVA
	// publishes. Left out, a namespace with a 4-GPU quota and a 3-GPU pool placed
	// four more replicas and consumed seven.
	//
	// Folded in HERE rather than published separately, because this store has one
	// producer on purpose: two components summing "what WVA holds" their own way
	// is how the optimizer and the wake path come to disagree about the same
	// cluster.
	addWarmPoolGPUs(managedByType, managedByNamespace)
	decision.PublishManagedGPUUsage(managedByType, managedByNamespace)

	// Report unattributed GPUs from the MANAGED view, which is the only one that
	// can have any: the physical picture attributes usage by the node a pod runs
	// on, so every GPU it counts lands under a resolved type, whereas a variant
	// whose own accelerator cannot be resolved is charged here to the "unknown"
	// placeholder and then to no pool at all. That silently under-states what WVA
	// holds, which is a quota reporting more allowance free than there is.
	//
	// It stays HERE rather than inside selectV2Optimizer, which returns early
	// unless the optimizer is GreedyByScore — reporting from behind that guard is
	// how it went silent on default deployments before.
	reportUnattributedGPUs(ctx, managedByType)

	// Stage 2: Compute GPU constraints and call optimizer
	optimizer, constraints := e.selectV2Optimizer(ctx, requests)
	// Cohort borrowing: what Kueue has not reserved yet leaves the shared
	// pools (borrowing.go). A no-op without borrowing.
	// One clock reading for every borrowing step of the cycle.
	borrowNow := time.Now()
	constraints = e.prepareBorrowing(ctx, constraints, requests, variantScaleTargets, borrowNow)
	// Scope-coupled rescale enablement (cluster + per-namespace) is resolved from
	// config and handed to the GPU-aware optimizer for this cycle.
	if g, ok := optimizer.(*allocation.GreedyByScoreOptimizer); ok {
		g.Rescale = e.resolveRescaleFlags(requests)
		g.BorrowBlocked = e.borrowBlocked()
		g.BorrowBlockedIn = e.borrowBlockedIn()
		g.ReclaimBlocked = e.reclaimBlocked()
		g.BorrowCredit = e.borrowing.credit
		g.ReclaimCredit = e.borrowing.reclaimCredit
		g.WakeReclaims = e.borrowing.wakeYield
	}
	allDecisions := e.decideV2(ctx, optimizer, requests, constraints, variantScaleTargets)
	draws := e.recordBorrowDraws(optimizer, allDecisions, borrowNow)
	logScalingDecisions(ctx, requests, allDecisions, borrowedByModel(draws, constraints))

	logger.V(logging.VERBOSE).Info("V2 optimizer produced decisions",
		"optimizer", optimizer.Name(),
		"decisionCount", len(allDecisions),
		"modelCount", len(requests))

	// Stage 3: Apply enforcer per-model (directly on decisions)
	for _, req := range requests {
		evaluated[utils.GetNamespacedKey(req.Namespace, req.ModelID)] = true
		e.applyScaleToZeroEnforcement(
			ctx, req.ModelID, req.Namespace, optimizer.Name(),
			modelTiers[utils.GetNamespacedKey(req.Namespace, req.ModelID)],
			allDecisions,
			modelScaleTargets[utils.GetNamespacedKey(req.Namespace, req.ModelID)],
			req.VariantStates,
		)
	}

	// Stage 3b: release borrowed capacity a waiting lender or an unplaced
	// replica asks back, last, so nothing above raises it again
	// (borrowing_release.go). A no-op without borrowing.
	e.releaseBorrowed(ctx, requests, allDecisions, variantScaleTargets, borrowNow)
	// A release lowers a target; what was in flight above it is not.
	e.borrowing.inFlight.Trim(decidedTargets(allDecisions))
	// What on-call sees: the budgets the cycle used, the borrowing reasons,
	// unplaced pods and waiting lenders (borrowing_signals.go). Every cycle,
	// so a cleared condition clears its series.
	e.publishBorrowSignals(constraints, requests, allDecisions, borrowNow)

	// The holes this cycle ordered replicas into stay held, from both
	// engines, until the replicas' pods hold their nodes. After the release:
	// a released replica holds no hole.
	if e.cycleFitted {
		e.admitInFlight(requests, allDecisions, e.cyclePlanned, variantScaleTargets, time.Now())
	}
	// Persisted every cycle, fitted or not: a wake's admission and an expired
	// hold change the ledger too.
	e.persistInFlight(ctx, time.Now())

	// Stage 4: Enrich decisions with KV cache token data from replicaMetrics.
	// Utilization, RequiredCapacity, and SpareCapacity are already set by
	// buildDecisionsWithOptimizer from AnalyzerResult.
	enrichDecisionsWithKvTokenData(allDecisions, modelReplicaMetrics)

	return allDecisions
}

// observeAccelerators reports whether this deployment should resolve an
// unconstrained variant's accelerator by reading the nodes its pods run on.
//
// Only a PHYSICAL limiter charges a variant to an accelerator pool, so only a
// physical limiter makes the answer matter. Without one, an unresolved accelerator
// is permissive — FitsGPUBudget asks whether any pool has room — and the node read
// would buy an attribution nothing consumes, at the cost of the only cluster-scoped
// permission WVA needs in the normal path.
//
// This is what lets a namespace-scoped install run with no cluster-scoped RBAC at
// all when no physical limiter is configured.
func observeAccelerators(cfg *config.Config) variantmeta.ObserveAccelerators {
	// ALWAYS FromNodes, for now, and the reasoning that said otherwise was wrong.
	//
	// The argument was: only a physical limiter charges a variant to an accelerator
	// pool, so with no limiter the observation buys nothing and the cluster-scoped
	// node read can be skipped. The budgeting half of that is true. The rest is not
	// — accelerator identity is also how the V2 capacity store keys learned
	// capacity (signals/capacity: FindCompatible matches on AcceleratorName
	// before reusing a record), so leaving it unresolved denies a variant its own
	// prior capacity knowledge and any compatible variant's.
	//
	// Gating it on the limiter made the e2e "per-model override" spec run a
	// workload to maxReplicas instead of settling at 1: with no capacity record the
	// demand-to-capacity ratio has no sane denominator. 53/0 before, 51/1 after.
	//
	// The reduction is still worth having — it is the difference between a
	// namespace-scoped install needing cluster-scoped RBAC and not — but it needs
	// the capacity store to key on something the workload declares, not on an
	// observation of where its pods landed. Until then, correctness wins.
	_ = cfg
	return variantmeta.FromNodes
}

// BuildVariantStates extracts current and desired replica counts from VAs for capacity analysis.
func (e *Engine) BuildVariantStates(
	ctx context.Context,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
) []domain.VariantReplicaState {
	// Variant identity + replica/GPU state is resolved by the discovery step,
	// the single producer of per-variant metadata. BuildVariantStates projects
	// that onto the legacy VariantReplicaState the analyzers consume today; the
	// identity fields discovery additionally resolves (Cost, AcceleratorName,
	// ModelID, ...) are read directly from VariantMetadata by the consumers that
	// need them.
	metas := variantmeta.Discover(ctx, vas, scaleTargets, k8sClient, observeAccelerators(e.Config))
	states := make([]domain.VariantReplicaState, 0, len(metas))
	for _, m := range metas {
		states = append(states, m.ToReplicaState())
	}
	return states
}

// getRoleFromScaleTarget extracts the P/D role from a scale target's pod template labels.
// Returns "prefill", "decode", or "both" (default when no role label is present).
// The resolution now lives in the discovery package (the owner of variant metadata);
// this wrapper is retained for in-package callers and tests.
func getRoleFromScaleTarget(scaleTarget scaletarget.ScaleTargetAccessor) string {
	return variantmeta.RoleFromScaleTarget(scaleTarget)
}

// enrichDecisionsWithKvTokenData sets KvCacheTokensUsed, KvCacheTokensCapacity, and
// RequiredCapacityUnit on decisions from replica metrics aggregated per (model, variant).
// Used by V2 path where Utilization and RequiredCapacity are already set from
// AnalyzerResult.
//
// Aggregation is keyed by (modelID, variantName) — not just variantName — because
// variant names can collide across different models in the same reconcile cycle.
func enrichDecisionsWithKvTokenData(decisions []domain.VariantDecision, modelReplicaMetrics map[string][]domain.ReplicaMetrics) {
	type kvAgg struct {
		kvUsed  int64
		kvTotal int64
	}
	type variantKey struct {
		modelID string
		variant string
	}
	agg := make(map[variantKey]*kvAgg)
	for modelID, metrics := range modelReplicaMetrics {
		for _, rm := range metrics {
			k := variantKey{modelID: modelID, variant: rm.VariantName}
			a, ok := agg[k]
			if !ok {
				a = &kvAgg{}
				agg[k] = a
			}
			a.kvUsed += rm.TokensInUse
			a.kvTotal += rm.TotalKvCapacityTokens
		}
	}

	for i := range decisions {
		d := &decisions[i]
		d.RequiredCapacityUnit = constants.UnitContinuous
		if a, ok := agg[variantKey{modelID: d.ModelID, variant: d.VariantName}]; ok {
			d.KvCacheTokensUsed = a.kvUsed
			d.KvCacheTokensCapacity = a.kvTotal
		}
	}
}

// soleEngineFor returns the one engine a model's scale targets run, and whether
// there is exactly one.
//
// Idleness is measured with a single counter, and the counter is engine-specific:
// vllm:request_success_total or sglang:num_requests_total. Either alone is fine —
// ask for the matching one. A model running BOTH would need both counters summed,
// and asking for one of them would see only part of its traffic and could park a
// model that is still serving through the other engine. That is a real
// (if unusual) topology, so it is refused rather than guessed at.
//
// Present() returns vLLM for an empty set, so a model with no resolvable targets
// yields a single engine and the absent-series guard downstream keeps it safe.
func soleEngineFor(scaleTargets map[string]scaletarget.ScaleTargetAccessor) (inferenceengine.Engine, bool) {
	engines := inferenceengine.Present(scaleTargets)
	if len(engines) != 1 {
		return inferenceengine.EngineVLLM, false
	}
	return engines[0], true
}

// applyScaleToZeroEnforcement runs scale-to-zero / minimum-replica enforcement for a
// single model's decisions, unless a safety gate skips it:
//   - the model runs MORE THAN ONE engine, so no single request counter measures
//     its idleness (see soleEngineFor); or
//   - any variant declares minReplicas > 0 (scalingpolicy.HasMinReplicasAboveZero).
//
// A single non-vLLM engine is now supported. It used to be refused outright,
// because the enforcer asked for vllm:request_success_total whatever the model
// ran, and an SGLang model has no such series — reading as idle and getting
// zeroed. The engine-specific query already existed
// (sglang:num_requests_total, registered beside the vLLM one); it simply was not
// reached. The engine is detected here and passed to the enforcer, which asks for
// the counter that matches.
//
// tiers are the policy tiers the model's variants selected (see variantTiers);
// nil resolves the default entry alone. The scale-to-zero policy is resolved from
// them by scaleToZeroPolicy, the same tier-aware resolution the thresholds use.
//
// Decisions are mutated in place; returns true if the model was scaled to zero.
//
// Both optimize paths (saturation, queueing-model) funnel their enforcement through
// this one method so the gate lives in a single place — a caller cannot accidentally
// invoke the enforcer ungated, and one test (engine_scale_to_zero_enforce_test.go)
// locks the gate down for every path.
func (e *Engine) applyScaleToZeroEnforcement(
	ctx context.Context,
	modelID, namespace, optimizerName string,
	tiers []string,
	decisions []domain.VariantDecision,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantStates []domain.VariantReplicaState,
) bool {
	logger := ctrl.LoggerFrom(ctx)

	// The resolved scaling entry carries the whole scale-to-zero policy: enabled
	// and retention, tiered namespace-local → global, layered with the policy tier
	// the model's variants selected, and merged with this model's override.
	// Resolved before the gates below, not after, because the gates are half of
	// what decides whether this model can ever park and the policy is the other
	// half — reporting one without the other cannot say which is binding.
	satConfig, disabledBy := scaleToZeroPolicy(e.Config, e.Config.ScalingPolicyConfigForNamespace(namespace), modelID, namespace, tiers)
	scaleToZeroEnabled := config.ResolveScaleToZeroEnabled(e.Config, &satConfig)
	modelEngine, engineSupported := soleEngineFor(scaleTargets)

	// Reported before the empty-decision return below, and unconditionally,
	// including with no reasons. This call is what CLEARS a reason that no longer
	// holds, so any path that skips it pins the last bad answer — and a cycle that
	// produced no decisions at all is precisely when a stale "will never park"
	// series is most misleading. The reasons themselves are configuration
	// reconciled against discovered bounds; they do not depend on there being a
	// decision to enforce.
	// Resolved before the reasons are published, not with the gate below, so the
	// hold appears on wva_model_scaling_blocked rather than only in a V(DEBUG)
	// line. A model held here is otherwise indistinguishable from one that will
	// never park.
	retention := config.ResolveScaleToZeroRetention(&satConfig)
	recentlyWoken := decision.WithinActivationRetention(namespace, modelID, retention)

	blockedReasons := scalingpolicy.ScaleToZeroBlockReasons(scaleToZeroEnabled, engineSupported, recentlyWoken, variantStates)
	metrics.SetModelScalingBlockedReasons(namespace, modelID,
		constants.ScalingBlockedReasonsPolicy, blockedReasons)
	if e.recordBlockedModel(namespace, modelID, blockedReasons) {
		scalingpolicy.LogBlockedTransition(ctx, namespace, modelID, blockedReasons)
	}
	// policy-forbids-zero names no tier, and with mixed tiers the operator
	// cannot tell which one disabled it. Said once per change of that set.
	var forbiddenBy []string
	if slices.Contains(blockedReasons, constants.ScalingBlockedPolicyForbidsZero) {
		forbiddenBy = tierNames(disabledBy)
	}
	if e.recordForbiddenBy(namespace, modelID, forbiddenBy) && len(forbiddenBy) > 0 {
		fix, perModel := scaleToZeroForbiddenFix(e.Config.ScalingPolicyConfigForNamespace(namespace),
			modelID, namespace, forbiddenBy)
		logger.Info(scaleToZeroForbiddenMessage,
			"modelID", modelID, "namespace", namespace,
			"tiers", tierNames(tiers), "disabledBy", forbiddenBy,
			"disabledByPerModelEntry", perModel, "fix", fix)
	}

	if len(decisions) == 0 {
		return false
	}

	if !engineSupported {
		logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: model runs more than one inference engine, so no single request counter measures its idleness",
			"modelID", modelID, "optimizer", optimizerName,
			"engines", inferenceengine.Present(scaleTargets))
		return false
	}
	if scalingpolicy.HasMinReplicasAboveZero(variantStates) {
		logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: variant has minReplicas > 0",
			"modelID", modelID, "optimizer", optimizerName)
		return false
	}

	// A model just woken from zero has served nothing yet: the request that woke
	// it is still queued in the EPP while the pod pulls and loads. The enforcer's
	// idle signal is a request counter over the retention window, which reads
	// zero for precisely that model — so without this gate the wake is undone
	// before it can serve the request that asked for it. Hold the model for the
	// same retention period the operator already configures for idleness.
	if recentlyWoken {
		logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: model was recently woken from zero and is inside its retention period",
			"modelID", modelID, "namespace", namespace, "retention", retention, "optimizer", optimizerName)
		return false
	}

	scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicyOnDecisions(
		ctx, modelID, namespace, decisions, &satConfig, optimizerName, modelEngine,
	)
	if scaledToZero {
		logger.Info("Scale-to-zero enforcement applied",
			"modelID", modelID, "optimizer", optimizerName)
	}
	return scaledToZero
}

// normalizeRole maps empty and "both" roles to the same canonical key so that
// non-disaggregated variants are grouped together regardless of whether the
// deployment carries a role label.
func normalizeRole(role string) string {
	if role == "" {
		return domain.RoleBoth
	}
	return role
}

// groupByRole sub-groups variant states by their P/D role.
// Returns a map keyed by normalized role ("both", "prefill", "decode").
// For non-disaggregated models (all "both"/""), the map contains a single entry.
func groupByRole(states []domain.VariantReplicaState) map[string][]domain.VariantReplicaState {
	groups := make(map[string][]domain.VariantReplicaState)
	for _, s := range states {
		key := normalizeRole(s.Role)
		groups[key] = append(groups[key], s)
	}
	return groups
}

// filterReplicaMetricsByVariants returns only the replica metrics whose VariantName
// appears in the given variant state slice. Used to split per-model metrics into
// per-role subsets without re-querying Prometheus.
func filterReplicaMetricsByVariants(metrics []domain.ReplicaMetrics, states []domain.VariantReplicaState) []domain.ReplicaMetrics {
	allowed := make(map[string]struct{}, len(states))
	for _, s := range states {
		allowed[s.VariantName] = struct{}{}
	}
	filtered := make([]domain.ReplicaMetrics, 0, len(metrics))
	for _, m := range metrics {
		if _, ok := allowed[m.VariantName]; ok {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

// filterVAsByVariantStates returns the subset of VAs whose Name appears in the
// given variant states. Used to map a role group back to its source VAs for
// safety-net metric emission.
func filterVAsByVariantStates(
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	states []domain.VariantReplicaState,
) []llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	allowed := make(map[string]struct{}, len(states))
	for _, s := range states {
		allowed[s.VariantName] = struct{}{}
	}
	filtered := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, len(states))
	for _, va := range vas {
		if _, ok := allowed[va.Name]; ok {
			filtered = append(filtered, va)
		}
	}
	return filtered
}

// variantNames returns variant names from states for logging.
func variantNames(states []domain.VariantReplicaState) []string {
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = s.VariantName
	}
	return names
}

// modelData holds the pre-processed data for a model, shared across optimize paths.
type modelData struct {
	modelID             string
	namespace           string
	replicaMetrics      []domain.ReplicaMetrics
	scaleTargets        map[string]scaletarget.ScaleTargetAccessor
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	variantStates       []domain.VariantReplicaState
	variantMetadata     []domain.VariantMetadata
	schedulerQueue      *domain.SchedulerQueueMetrics
	arrivalRate         float64
}

// prepareModelData collects metrics and builds lookup maps for a model's VAs.
// This is shared by the saturation path and the Queueing Model Analyzer engine.
// Returns nil modelData (not error) when no metrics are available — caller should skip the model.
func (e *Engine) prepareModelData(
	ctx context.Context,
	modelID string,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	k8sClient client.Client,
) (*modelData, error) {
	if len(modelVAs) == 0 {
		return nil, fmt.Errorf("no VAs provided for model %s", modelID)
	}

	logger := ctrl.LoggerFrom(ctx)
	namespace := modelVAs[0].Namespace

	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for i := range modelVAs {
		va := &modelVAs[i]
		scaleTarget, err := scaletarget.FetchScaleTarget(ctx, k8sClient, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
		if err != nil {
			logger.V(logging.DEBUG).Info("Could not get scale target for VA",
				"variant", va.Name,
				"scaleTarget", va.GetScaleTargetName(),
				"error", err)
			continue
		}

		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		scaleTargets[key] = scaleTarget
		// Recorded HERE, where the target was read, before anything below can
		// return early: a model skipped for having no metrics this cycle still
		// has a fleet with an identity, and the carry that runs for it must be
		// able to tell that fleet from a re-created one.
		e.noteScaleTargetUID(key, scaleTarget.GetUID())

		variantKey := utils.GetNamespacedKey(va.Namespace, va.Name)
		variantAutoscalings[variantKey] = va
	}

	logger.V(logging.DEBUG).Info("Using source infrastructure for replica metrics",
		"modelID", modelID,
		"namespace", namespace)
	replicaMetrics, err := e.ReplicaMetricsCollector.CollectReplicaMetrics(ctx, modelID, namespace, scaleTargets, variantAutoscalings, e.vaEventTracker)
	if err != nil {
		return nil, fmt.Errorf("failed to collect Saturation metrics for model %s: %w", modelID, err)
	}

	logger.V(logging.DEBUG).Info("Collected saturation metrics",
		"modelID", modelID,
		"namespace", namespace,
		"metricsCount", len(replicaMetrics))

	// How many replicas of each variant are actually SERVING, for the warm pool.
	//
	// A bridge is handed back when the ordinary replicas have arrived, and
	// "arrived" used to mean Status.ReadyReplicas -- the kubelet's probe. A
	// replica that has passed a probe is not necessarily taking requests yet, and
	// handing traffic back to one that is not strands it. A replica the collector
	// can see has registered with the engine and is reporting on its work, which
	// is a much better answer to the same question.
	publishServing(namespace, replicaMetrics)

	if len(replicaMetrics) == 0 {
		// Two very different situations reach this line, and they used to produce
		// the same message.
		//
		// A model scaled to zero with nothing queued is idle, and skipping is the
		// whole point. A model with requests waiting in the scheduler is serving
		// traffic through pods this controller cannot attribute — an FMA topology
		// whose launchers nothing scrapes, a PodMonitor naming a port the pods do
		// not declare, a workload whose ownerReferences reach no scale target. The
		// second is an emergency and was indistinguishable from the first.
		//
		// The scheduler queue answers it, and it can be asked here precisely
		// because it is model-level and comes from EPP: it does not depend on any
		// engine pod being scraped, which is the thing that has gone wrong.
		queued := 0
		if sq := e.ReplicaMetricsCollector.CollectSchedulerQueueMetrics(ctx, modelID, namespace); sq != nil {
			queued = int(sq.QueueSize)
		}
		metrics.SetUnmeasuredQueue(namespace, modelID, queued)

		if queued > 0 {
			logger.Info("Model is serving but no replica could be attributed: requests are queued and nothing will scale",
				"modelID", modelID,
				"namespace", namespace,
				"queuedRequests", queued,
				"hint", "check that the serving pods are scraped and that their ownerReferences reach a scale target; for FMA, that the launcher pods have a PodMonitor (see docs/reference/operations.md, 'FMA launcher pods')")
		} else {
			logger.Info("No saturation metrics available for model, skipping analysis",
				"modelID", modelID,
				"namespace", namespace)
		}
		// The analyzers do not run on this path, so the series they set must
		// be told here that nothing was observed.
		e.onNothingObserved(namespace, modelID)
		return nil, nil // nil modelData signals skip
	}
	// Measurable this cycle. Reset the gauge rather than leaving the last bad
	// value to expire on Prometheus' staleness clock.
	metrics.SetUnmeasuredQueue(namespace, modelID, 0)

	// Discover the authoritative per-variant metadata once, then project it onto
	// the VariantReplicaState the analyzers consume. The full metadata is also
	// threaded to the optimizer (via ModelScalingRequest.Variants) as the source
	// of truth for variant identity/cost/accelerator.
	variantMetadata := variantmeta.Discover(ctx, modelVAs, scaleTargets, k8sClient, observeAccelerators(e.Config))
	variantStates := make([]domain.VariantReplicaState, 0, len(variantMetadata))
	for _, m := range variantMetadata {
		variantStates = append(variantStates, m.ToReplicaState())
	}
	schedulerQueue := e.ReplicaMetricsCollector.CollectSchedulerQueueMetrics(ctx, modelID, namespace)
	arrivalRate := e.ReplicaMetricsCollector.CollectModelArrivalRate(ctx, modelID, namespace)

	return &modelData{
		modelID:             modelID,
		namespace:           namespace,
		replicaMetrics:      replicaMetrics,
		scaleTargets:        scaleTargets,
		variantAutoscalings: variantAutoscalings,
		variantStates:       variantStates,
		variantMetadata:     variantMetadata,
		schedulerQueue:      schedulerQueue,
		arrivalRate:         arrivalRate,
	}, nil
}

// applySaturationDecisions updates VA status and emits metrics based on Saturation decisions.
func (e *Engine) applySaturationDecisions(
	ctx context.Context,
	decisions []domain.VariantDecision,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*domain.Allocation,
) {
	logger := ctrl.LoggerFrom(ctx)
	// Create a map of decisions for O(1) lookup
	// Use namespace/variantName as key to match vaMap and avoid collisions
	decisionMap := make(map[string]domain.VariantDecision)
	for _, d := range decisions {
		decisionMap[utils.GetNamespacedKey(d.Namespace, d.VariantName)] = d
	}

	// What the actuator last PUBLISHED per scale target, read before the loop
	// variable below shadows the decision package. The sticky scale-down judges
	// a fresh target against this, not against the count the target is
	// running: the two differ for the whole of KEDA's stabilization window,
	// which is exactly when a target that has crept back up cancels the descent.
	sticky := e.Config != nil && e.Config.StickyScaleDownEnabled()
	if e.lastDecided == nil {
		e.lastDecided = make(map[string]decidedMark)
	}
	// The published value with the time of the last cycle that DECIDED it, not
	// the time it was last written, and only if that decision was about the
	// incarnation of the target running now -- see lastDecided.
	published := func(namespace, target string) (int, time.Time, bool) {
		key := utils.GetNamespacedKey(namespace, target)
		d, ok := decision.Get(namespace, target)
		mark, decided := e.lastDecided[key]
		// A target this cycle did not read has no identity to compare; the
		// mark is trusted, bounded by its age. Only a target READ this cycle
		// under a different UID is a different fleet.
		uid, known := e.scaleTargetUIDs[key]
		sameFleet := decided && (!known || mark.uid == uid)
		return int(d.DesiredReplicas), mark.at, ok && sameFleet
	}

	// Iterate over ALL active VAs to ensure we update status and trigger reconciliation for everyone
	for vaName, va := range vaMap {
		decision, hasDecision := decisionMap[vaName]

		if hasDecision && sticky {
			p, at, ok := published(va.Namespace, va.GetScaleTargetName())
			var held bool
			if decision, held = scalingpolicy.HoldPublishedScaleDown(decision, p, at, ok, e.stickyAge(), time.Now()); held {
				logger.Info("holding the published scale-down against a fresh target that crept back up",
					"variant", vaName, "published", p, "current", decision.CurrentReplicas,
					"reason", decision.LastStep().Reason)
			}
		}
		{
			key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
			if hasDecision {
				e.lastDecided[key] = decidedMark{at: time.Now(), uid: e.scaleTargetUIDs[key]}
			} else if mark, ok := e.lastDecided[key]; ok {
				mark.missed++
				e.lastDecided[key] = mark
			}
		}

		if hasDecision {
			logger.V(logging.VERBOSE).Info("Processing decision for VA",
				"variant", vaName,
				"action", decision.Action,
				"current", decision.CurrentReplicas,
				"target", decision.TargetReplicas)
		} else {
			logger.V(logging.DEBUG).Info("No scaling decision for VA, but updating status to trigger reconcile",
				"variant", vaName)
		}

		// Variants are synthesized in-memory from annotated HPAs/ScaledObjects;
		// there is no API-server object to fetch, so work on a copy.
		updateVa := *va.DeepCopy()

		// Update CurrentAlloc from local analysis (which has the latest metrics)
		// We use currentAllocations map instead of Status.CurrentAlloc
		if currentAlloc, ok := currentAllocations[vaName]; ok {
			// If we have a decision, attach current alloc to it for cache
			// If we have a decision, attach current alloc to it for cache
			// (Future logic if needed)
			_ = currentAlloc // Used for something?
			// Previously we updated va.Status.CurrentAlloc = currentAlloc
			// Now we just don't update status with it.
		}

		// Determine target replicas and accelerator
		var targetReplicas int
		var acceleratorName string
		var reason string

		if hasDecision {
			targetReplicas = decision.TargetReplicas
			acceleratorName = decision.AcceleratorName
			reason = decision.Reason()
		} else {
			// No change/decision: Keep current target or default to current replicas
			// We effectively explicitly "decide" to keep things as they are if no decision was made
			//
			// resolvedRead says whether targetReplicas below is a count that was
			// actually READ -- from status, from the allocations, or from the
			// scale target -- as opposed to the 0 it starts as. The carry needs
			// the difference: a read 0 is a fleet somebody scaled to zero and
			// must be published as 0; an unread 0 is a fetch that failed.
			resolvedRead := false
			if updateVa.Status.DesiredOptimizedAlloc.NumReplicas != nil && *updateVa.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
				targetReplicas = int(*updateVa.Status.DesiredOptimizedAlloc.NumReplicas)
				resolvedRead = true
			} else if curr, ok := currentAllocations[vaName]; ok {
				targetReplicas = curr.NumReplicas
				resolvedRead = true
			}
			// Keep existing accelerator or use current (skip sentinel values)
			if acc := updateVa.Status.DesiredOptimizedAlloc.Accelerator; constants.IsAcceleratorResolved(acc) {
				acceleratorName = acc
			} else if curr, ok := currentAllocations[vaName]; ok && constants.IsAcceleratorResolved(curr.Accelerator) {
				acceleratorName = curr.Accelerator
			}

			// Fallback for new VAs without prior status or collected metrics:
			// resolve accelerator from deployment nodeSelector/nodeAffinity or VA label,
			// and use current deployment replicas as target to avoid unintended scaling.
			if !constants.IsAcceleratorResolved(acceleratorName) {
				scaleTargetName := updateVa.GetScaleTargetName()
				if scaleTargetName != "" {
					var scaleTarget scaletarget.ScaleTargetAccessor
					var err error
					if scaleTarget, err = scaletarget.FetchScaleTarget(ctx, e.client, va.Name, va.Spec.ScaleTargetRef.Kind, scaleTargetName, va.Namespace); err == nil {
						acceleratorName = accel.GetAcceleratorNameFromScaleTarget(&updateVa, scaleTarget)
						if targetReplicas == 0 && scaleTarget.GetReplicas() != nil {
							targetReplicas = int(*scaleTarget.GetReplicas())
							resolvedRead = true
						}
						// This read is the identity the carry below compares against.
						e.noteScaleTargetUID(utils.GetNamespacedKey(va.Namespace, scaleTargetName), scaleTarget.GetUID())
					} else {
						// If scaleTarget fetch fails, try VA label directly
						acceleratorName = accel.GetAcceleratorNameFromScaleTarget(&updateVa, nil)
					}
				}
			}

			if sticky {
				// AFTER the fallback above has resolved the running count -- a
				// variant synthesized from a ScaledObject has no status and the
				// allocations map is empty, so before it targetReplicas is 0 for
				// every real variant and there would be nothing to carry against.
				// A cycle with no metrics cannot justify raising what the last
				// cycle with metrics lowered; see scalingpolicy.CarryPublished.
				p, at, ok := published(va.Namespace, va.GetScaleTargetName())
				var floor *int
				if va.Spec.MinReplicas != nil {
					f := int(*va.Spec.MinReplicas)
					floor = &f
				}
				missed := e.lastDecided[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())].missed
				if carried := scalingpolicy.CarryPublished(targetReplicas, resolvedRead, p, at, ok, floor, missed, time.Now()); carried != targetReplicas {
					logger.Info("no decision this cycle; republishing the held scale-down rather than the running count",
						"variant", vaName, "published", carried, "running", targetReplicas)
					targetReplicas = carried
				}
			}

			reason = "No scaling decision (optimization loop)"
		}

		// If we still don't have an accelerator name (e.g. new VA, no decision, no current alloc), we can't update status sensibly
		// But we still need to set MetricsAvailable condition via the cache
		if acceleratorName == "" {
			logger.Info("Skipping status update for variant without accelerator info",
				"variant", vaName, "cacheKey.name", va.Name, "cacheKey.namespace", va.Namespace)
		}

		// Emit a K8s event when accelerator cannot be resolved so operators
		// can see the problem without digging through controller logs.
		// The message is a constant string (not built per-cycle via Eventf with
		// formatted args), so each emission produces an identical
		// (involvedObject, source, type, reason, message) tuple — which the K8s
		// API server's event aggregator collapses into a single Event entry with
		// an updated count, rather than creating a new entry each optimization
		// cycle.
		if !constants.IsAcceleratorResolved(acceleratorName) {
			e.emitAcceleratorNotResolvedEvent(&updateVa)
			e.policies.ReportUnresolvedAccelerator(ctx, va.Namespace, vaName, e.Config.EffectiveLimiterSummary())
		}

		// Stage the just-computed decision on the in-memory VA so that
		// act.EmitMetrics below — which reads
		// Status.DesiredOptimizedAlloc.{NumReplicas,Accelerator}
		// (see actuator.EmitMetrics) — publishes the fresh target. This object
		// is local to the engine and never persisted: the decision store that
		// EmitMetrics writes is the only output.
		// Sanitize the sentinel out of the staged value so EmitMetrics never
		// sees it.
		numReplicas := int32(targetReplicas)
		statusAccelerator := acceleratorName
		if !constants.IsAcceleratorResolved(statusAccelerator) {
			statusAccelerator = ""
		}
		updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
			NumReplicas: &numReplicas,
			Accelerator: statusAccelerator,
			LastRunTime: metav1.Now(),
		}
		updateVa.Status.Actuation.Applied = false // Reset applied status until Actuator handles it (if needed)

		// Set condition based on decision characteristics (or lack thereof)
		if hasDecision {
			switch {
			case decision.SafetyOverride:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationSafetyOverride",
					"saturation safety override: "+reason)
			case decision.SaturationOnly:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationOnlyMode",
					fmt.Sprintf("saturation-only decision: %s (target: %d replicas)", reason, targetReplicas))
			default:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
					fmt.Sprintf("Hybrid mode: %s (target: %d replicas)", reason, targetReplicas))
			}
		} else {
			// No active decision (just refreshing)
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
				"Optimization loop ran (no scaling change needed)")
		}

		// Emit metrics for external autoscalers (Important: Actuator emits these)
		// We should emit metrics even if no decision changed, to keep HPA alive
		act := actuator.NewActuator(e.client)
		/*
		   NOTE: emitSafetyNetMetrics handles cases where optimization FAILS.
		   Here we are in the success path (optimization ran, even if no change).
		   We should ensure metrics are emitted for the External Scaler.
		*/

		// Ensure we have a valid SAT/Model decision "SaturationOnly" flag for metric emission context if needed
		// For now we assume if no decision, it's not saturation-only forced override, just normal op.
		// isSaturationOnly := false
		// if hasDecision {
		// 	isSaturationOnly = decision.SaturationOnly
		// }

		// Always emit the replica scaling signal (the HPA/KEDA external metric).
		// EmitReplicaMetrics labels an unresolved accelerator as the bounded
		// "unresolved" value (never the internal sentinel), so the scaling signal
		// is never withheld; the scaler matches on variant/namespace rather than
		// accelerator_type, so it keeps working regardless. Accelerator-dimensioned
		// metrics (saturation/capacity) remain gated on resolution below.
		if err := act.EmitMetrics(ctx, &updateVa); err != nil {
			msg := "Failed to emit metrics for external autoscalers"
			// K8s best practice: events should reference the current resource version
			e.recordOptimizationFailedEvent([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{updateVa}, msg)
			logger.Error(err, msg, "variant", updateVa.Name)
		} else {
			// Only log detail if we had a decision or periodically (to avoid spamming logs on every loop for no-ops)
			if hasDecision {
				// Emit Kubernetes event for observability
				e.recordScalingEvent(&updateVa, decision.Action, decision.TargetReplicas, decision.Reason())
				if decision.WasLimited {
					e.recordEvent(va, corev1.EventTypeWarning, constants.K8SEventResourceConstrained, constrainedMessage(&decision))
				}

				logger.V(logging.VERBOSE).Info("Successfully emitted metrics",
					"variant", updateVa.Name,
					"target", targetReplicas,
					"accelerator", acceleratorName)
			}
			updateVa.Status.Actuation.Applied = true
		}

		// Record saturation and capacity metrics when this cycle produced a
		// fresh decision for the variant. These accelerator-dimensioned series
		// carry the accelerator_type label, so they are emitted only when the
		// type is resolved — otherwise the internal sentinel would leak into a
		// label. When there is no fresh decision the existing series persist with
		// their last-recorded values until Prometheus' staleness marker fires.
		if hasDecision && constants.IsAcceleratorResolved(acceleratorName) {
			act.RecordSaturationMetrics(ctx, decision)
		}

		// The wva_saturation_metrics_up freshness gauge carries only
		// {variant_name, namespace} (no accelerator_type), so it is emitted on
		// every cycle independent of accelerator resolution: 1.0 when a fresh
		// decision was produced for the variant, 0.0 otherwise. Dashboards gate
		// alerts on this gauge instead of relying on Prometheus' 5-minute
		// implicit staleness marker.
		if hasDecision {
			act.RecordSaturationFreshness(ctx, decision.VariantName, decision.Namespace, true)
		} else {
			act.RecordSaturationFreshness(ctx, va.Name, va.Namespace, false)
		}

		// Metric emission above (act.EmitMetrics) is the sole output for
		// annotation-sourced variants: KEDA/HPA reads wva_desired_replicas
		// directly. There is no CRD status to patch, so no cache write or
		// reconcile trigger is needed.

		if hasDecision {
			if decision.Action != domain.ActionNoChange {
				if err := e.metricsEmitter.EmitReplicaScalingMetrics(ctx, &updateVa, decision.Action, decision.ReasonCategory()); err != nil {
					logger.Error(err, "Failed to emit replica scaling metrics")
				}
			}
			logger.Info("Applied saturation decision via shared cache",
				"variant", vaName,
				"namespace", updateVa.Namespace,
				"action", decision.Action,
				"target", targetReplicas,
				"reason", reason)
		}
	}
}

// emitAcceleratorNotResolvedEvent records a Warning event on the given
// VariantAutoscaling so operators see at-a-glance that the optimization
// loop ran but could not resolve an accelerator type for it. The message
// is a constant string so the API server's event aggregator collapses
// repeated emissions into a single Event entry with an updated count
// rather than creating a new entry each optimization cycle.
func (e *Engine) emitAcceleratorNotResolvedEvent(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) {
	// A placement constraint is the only thing that resolves this now: the
	// acceleratorName label was removed because a workload that does not constrain
	// placement can be scheduled onto any GPU node, so the label asserted a type
	// nothing enforced. Telling an operator to set one would be telling them to
	// write down a guess.
	e.recordEvent(va, corev1.EventTypeWarning, "AcceleratorNotResolved",
		"The workload constrains no accelerator (no GPU product key in its nodeSelector or "+
			"nodeAffinity), so WVA cannot tell which accelerator it runs on. It is treated as "+
			"placeable on any of them: on a single-accelerator cluster the type is deduced, but "+
			"elsewhere its GPUs are charged to no accelerator pool (see wva_unattributed_gpus) and "+
			"accelerator-specific saturation/capacity metrics are withheld. Replica scaling metrics "+
			"are still emitted with accelerator_type=\"unresolved\" so HPA/KEDA can scale. "+
			"Set a GPU product nodeSelector or nodeAffinity on the workload to resolve it.")
}

// emitSafetyNetMetrics emits fallback metrics when saturation analysis fails.
func (e *Engine) emitSafetyNetMetrics(
	ctx context.Context,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*domain.Allocation,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) {
	logger := ctrl.LoggerFrom(ctx)
	act := actuator.NewActuator(e.client)

	for _, va := range modelVAs {
		// Determine desired replicas
		var desiredReplicas, currentReplicas int32
		var fallbackSource string
		var scaleTarget scaletarget.ScaleTargetAccessor
		var err error
		if scaleTargets != nil {
			if target, ok := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]; ok {
				scaleTarget = target
				// Get current replicas for metric emission.
				currentReplicas, err = act.GetCurrentScaleTargetReplicasFromScaleTarget(&va, scaleTarget)
				if err != nil {
					logger.Error(err, "Safety net: failed to get current replicas from scale target for metrics, using cached allocation",
						"variant", va.Name)
					if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
						currentReplicas = int32(curr.NumReplicas)
					}
				}
			}
		}

		// Strategy 1: Use previous desired replicas if available
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil && *va.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
			desiredReplicas = *va.Status.DesiredOptimizedAlloc.NumReplicas
			fallbackSource = "previous-desired"
		} else {
			desiredReplicas = currentReplicas
			fallbackSource = "current-replicas"
		}

		// Determine accelerator - try status first, then labels, skip if unavailable
		// TODO: remove this checks when we will move to a new version of the CRD
		// with required accelerator field
		accelerator := va.Status.DesiredOptimizedAlloc.Accelerator
		if accelerator == "" {
			if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
				accelerator = curr.Accelerator
			}
		}
		if !constants.IsAcceleratorResolved(accelerator) {
			// Try to get accelerator name from scale target nodeSelector/nodeAffinity or VA labels
			if scaleTarget == nil {
				logger.V(logging.DEBUG).Info("Safety net: no scale target found for VA",
					"variant", va.Name)
			} else {
				accelerator = accel.GetAcceleratorNameFromScaleTarget(&va, scaleTarget)
			}
		}
		if !constants.IsAcceleratorResolved(accelerator) {
			// Do NOT withhold the scaling signal: EmitReplicaMetrics labels an
			// unresolved accelerator with the bounded "unresolved" value, so the
			// safety-net signal still reaches HPA/KEDA (which match on
			// variant/namespace, not accelerator_type). Mirrors the main path.
			logger.V(logging.DEBUG).Info("Safety net: accelerator unresolved, emitting scaling signal with 'unresolved' accelerator_type",
				"variant", va.Name)
		}

		// Emit safety net metrics
		if err := act.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			&va,
			currentReplicas,
			desiredReplicas,
			accelerator,
		); err != nil {
			logger.Error(err, "Safety net: failed to emit metrics",
				"variant", va.Name)
			continue
		}

		logger.Info("Safety net activated: emitted fallback metrics",
			"variant", va.Name,
			"currentReplicas", currentReplicas,
			"desiredReplicas", desiredReplicas,
			"accelerator", accelerator,
			"fallbackSource", fallbackSource)
	}
}

// publishServing records, per variant, how many distinct Pods of its OWN
// replicas reported engine metrics.
//
// Counted by POD, not by metric: an engine that spans several processes reports
// more than once for one replica, and the warm pool is comparing this against a
// replica count. Empty variant names are skipped rather than collapsed into one
// bucket -- a metric WVA cannot attribute says nothing about any particular
// variant's readiness to take traffic back.
//
// BRIDGES ARE EXCLUDED, and that is the whole point of the count. The pool asks
// this to decide whether the ordinary replicas have arrived and its lent Pod can
// go home. A bridge is attributed to the variant it serves -- correctly, because
// its DEMAND is the variant's -- so counting it here answers "yes" one replica
// early: the pool hands back the very Pod that was carrying the traffic, to a
// fleet one replica short of covering it. Demand yes, supply no; this is a
// supply-side count.
//
// A variant whose only rows are bridges still publishes, as zero. That is not
// the same as publishing nothing: no reading falls back to the scale target's
// Ready count, while zero says its own replicas are demonstrably not serving --
// which is exactly when a bridge must be kept.
func publishServing(namespace string, metrics []domain.ReplicaMetrics) {
	byVariant := map[string]map[string]bool{}
	for _, m := range metrics {
		if m.VariantName == "" || m.PodName == "" {
			continue
		}
		if byVariant[m.VariantName] == nil {
			byVariant[m.VariantName] = map[string]bool{}
		}
		if m.FromWarmPool {
			continue // seen, so it publishes a count; not counted in it
		}
		// REPORTING IS NOT SERVING. An engine answers /metrics as soon as its
		// HTTP server is up, which is before the Pod passes readiness, so a
		// starting replica reports for some seconds while no Service and no EPP
		// will route to it. Handing a bridge back to one strands the traffic it
		// was carrying -- the same failure as returning it too early, arrived at
		// from the other side.
		//
		// Only this count is narrowed. The analyzer still prices a starting Pod:
		// it holds its GPU and its KV cache is real, so it is capacity even
		// while it is not yet in the rotation.
		if !m.Ready {
			continue
		}
		byVariant[m.VariantName][m.PodName] = true
	}
	now := time.Now()
	for variant, pods := range byVariant {
		decision.PublishServing(namespace, variant, len(pods), now)
	}
}

// reportSignalReason sets the no-composite-signal reason when req's composite
// defines no replica need for any variant, and clears it otherwise. A nil req
// (this cycle built no composite for the model) clears it, so the reason never
// outlives the cycle that found it. With no signal the optimizer sizes on
// saturation's own capacity.
func reportSignalReason(namespace, modelID string, req *allocation.ModelScalingRequest) {
	var reasons []string
	if req != nil && !allocation.CompositeHasSignal(req.CompositeSignal) {
		reasons = []string{constants.ScalingBlockedNoCompositeSignal}
	}
	metrics.SetModelScalingBlockedReasons(namespace, modelID, constants.ScalingBlockedReasonsSignal, reasons)
}
