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

package scalefromzero

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/controller-runtime/pkg/log"

	accel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpuusage"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// Constants for condition
const (
	reasonDetails        = ": pending request - scale-up"
	targetEPPMetricLabel = "target_model_name"
	metricNameLabel      = "__name__"
)

// targetEPPMetricNames are the EPP flow-control queue-depth gauges the engine
// reads to decide whether a workload parked at zero has demand waiting, in
// preference order. llm-d's EPP renamed the family to the llm_d_epp_ prefix and
// marks the inference_extension_ name "[Deprecated: Use
// llm_d_epp_flow_control_queue_size]". Both are exported today, so read the new
// name and fall back to the old one rather than pinning either — pinning the
// deprecated name would start failing silently the moment it is dropped.
var targetEPPMetricNames = []string{
	constants.EPPFlowControlQueueSize,
	constants.SchedulerFlowControlQueueSize,
}

// pendingRequestsForModel reports whether the EPP flow-control queue currently
// holds requests for modelID, returning the matching sample.
//
// Only one metric family is consulted: the first name in targetEPPMetricNames
// that the EPP actually exports. The deprecated family mirrors the preferred one,
// so once the preferred family is present its verdict is authoritative and
// falling through to the alias would only double-count.
//
// familyExported separates the two states that look identical in the return
// value and are not the same thing at all. An idle queue reports 0 and the model
// is fine; a queue that does not exist reports nothing and the model can never be
// woken. This function has always distinguished them internally — the `exported`
// flag is the whole reason the loop is shaped this way — and used to discard the
// answer at the boundary, which is how an EPP running without flow control looked
// exactly like an EPP with nothing queued for a week of red e2e runs.
func pendingRequestsForModel(values []source.MetricValue, modelID string) (pending source.MetricValue, hasPending, familyExported bool) {
	for _, name := range targetEPPMetricNames {
		exported := false
		for _, value := range values {
			if value.Labels[metricNameLabel] != name {
				continue
			}
			exported = true
			if value.Value > 0 && value.Labels[targetEPPMetricLabel] == modelID {
				return value, true, true
			}
		}
		if exported {
			return source.MetricValue{}, false, true
		}
	}
	return source.MetricValue{}, false, false
}

type Engine struct {
	client         client.Client
	executor       executor.Executor
	recorder       record.EventRecorder
	Datastore      datastore.Datastore
	maxConcurrency int
	config         *config.Config // Unified configuration (injected from main.go)
	// gpuLimiter supplies the GPU/quota constraints a wake must fit within. Nil
	// means no capacity check — see gpuConstraints. Read and written only under
	// limiterMu, because refreshLimiter may replace it while a placement is being
	// decided.
	gpuLimiter allocation.Limiter
	// limiterBuilder rebuilds gpuLimiter from the live config, and limiterSig is
	// the config fingerprint it was last built from.
	//
	// This engine used to build its limiter ONCE at startup, so an operator editing
	// the limiters: list changed the saturation engine's behaviour immediately and
	// this engine's not at all — until a restart. The ConfigMap documents that list
	// as applied live, and it is now the sole switch for both, so half of it
	// honouring the edit was a trap: capacity checks silently kept using whatever
	// was configured when the process started.
	limiterMu      sync.Mutex
	limiterBuilder func() (allocation.Limiter, error)
	limiterSig     string
	// UsageRefresher brings the cluster's GPU usage up to date immediately before
	// a placement is decided. A nil pointer is a no-op (EnsureFresh has a nil
	// receiver guard), so the periodic observation is then the only input — which
	// is what tests want and what production must not rely on.
	UsageRefresher *gpuusage.Refresher
	// Variants is the set of workloads KEDA has called WVA about — discovery.
	Variants *registry.Registry
	// VariantEnricher resolves each registered workload's scale target. Refreshed
	// at the top of every cycle, which is a no-op for entries still inside their
	// freshness window — this loop runs at 10Hz and must not read per tick.
	VariantEnricher *registry.Enricher
	// lastRefusal remembers, per model, the last reason a wake was refused, so a
	// verdict repeated every 100ms is logged once rather than continuously.
	refusalMu sync.Mutex
	// lastClaim is each model's last share-claim outcome, so the 10 Hz loop
	// counts a refused claim once per change, not once per tick. Guarded by
	// refusalMu.
	lastClaim   map[string]string
	lastRefusal map[string]SelectionOutcome
	// lastBudgets remembers, per namespace, the GPU budgets last reported for
	// placement, so the picture is logged when it CHANGES rather than on every
	// tick. Guarded by refusalMu: both are per-cycle logging state written from
	// the same loop, and a second mutex would only invite them to be taken in
	// different orders.
	lastBudgets map[string]string
	// lastGood is each namespace's last successful constraints, used to check a
	// wake when every provider is unavailable (see gpuConstraints). Guarded by
	// snapshotMu: it is placement state, not logging state.
	snapshotMu sync.Mutex
	lastGood   map[string]constraintSnapshot
	// QueueFallback reads the flow-control queue from Prometheus when the direct
	// EPP scrape fails. Nil disables it — every method guards its receiver — and
	// the engine then behaves exactly as it did before the fallback existed. See
	// queue_fallback.go for why one metric needs two transports.
	QueueFallback *QueueFallback
	// wakeSignal caches, per EPP pool, whether a flow-control queue is exported at
	// all, and which models that verdict has been reported for. See
	// wake_signal.go: this is reported for SERVING models too, because a model
	// behind an EPP without flow control is parked by the enforcer on a signal
	// that has nothing to do with flow control, and is only then unwakeable.
	wakeSignal wakeSignalState

	// wakeMu makes a wake decision -- the check and what the wake takes --
	// one step, so two models deciding concurrently in one tick cannot both
	// pass on the same free GPUs. tickWakes and tickNodes are this tick's
	// (wake_tick.go), guarded by it.
	wakeMu    sync.Mutex
	tickWakes map[string]map[string]int
	tickNodes tickNodeView
	// tickReclaimed is, per namespace and type, the lent-out nominal this
	// tick's wakes grew into without a node hole (placement.reclaimable).
	tickReclaimed map[string]map[string]allocation.Reclaim
	// wakeBlocked is the models (modelGroup.key) this loop set a refused-wake
	// reason on wva_model_scaling_blocked for (reportWakeBlocked), each as
	// the modelGroup it was set for, so pruneRefusals can clear it.
	wakeBlocked sync.Map
	// layouts remembers the variants whose unknown pod layout this engine's
	// node views have logged, so each is logged once, not once per tick.
	layouts allocation.LayoutLog
	// extraPools remembers, per model (modelGroup.key), the InferencePools
	// its variants resolve to beyond the one used, as last logged
	// (noteExtraPool), so the 100ms loop logs a change, not every tick.
	extraPools sync.Map
}

// SetGPULimiter injects the limiter whose constraints bound a wake, so the
// engine does not wake a variant onto an accelerator with no free GPUs. Optional:
// with no limiter the engine wakes without a capacity check, which is the
// behaviour it had before selection existed.
func (e *Engine) SetGPULimiter(l allocation.Limiter) {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	e.gpuLimiter = l
}

// SetLimiterBuilder installs a builder that rebuilds the limiter from the current
// effective config, so a limiters: edit reaches this engine without a restart —
// matching the saturation engine, which has always worked that way.
//
// The signature is seeded from the config as it stands, so the first cycle does
// not rebuild a limiter that was just constructed from the same config.
func (e *Engine) SetLimiterBuilder(builder func() (allocation.Limiter, error)) {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	e.limiterBuilder = builder
	e.limiterSig = allocation.LimiterSignature(e.config)
}

// refreshLimiter rebuilds the limiter when the effective limiter config changed
// since it was last built. A build error keeps the previous limiter, so a
// transient bad config never silently turns the capacity check off.
//
// Called once per cycle. The comparison is a fingerprint, not a rebuild: this
// loop runs at 10Hz and constructing a limiter per tick would re-create the
// inventory (and its discovery client) ten times a second.
func (e *Engine) refreshLimiter(ctx context.Context) {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	if e.limiterBuilder == nil {
		return
	}
	sig := allocation.LimiterSignature(e.config)
	if sig == e.limiterSig && e.gpuLimiter != nil {
		return
	}
	limiter, err := e.limiterBuilder()
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to rebuild the GPU limiter from config; "+
			"keeping the previous one", "limiters", e.config.EffectiveLimiterSummary())
		return
	}
	e.gpuLimiter = limiter
	e.limiterSig = sig
	e.forgetConstraints()
	log.FromContext(ctx).Info("Scale-from-zero: GPU limiter (re)built from config",
		"limiterTypes", e.config.EffectiveLimiterTypes().String(), "limiters", e.config.EffectiveLimiterSummary(),
		"name", limiter.Name())
}

// currentGPULimiter reads the active limiter under the lock, so a placement is
// never decided against a limiter being replaced mid-cycle.
func (e *Engine) currentGPULimiter() allocation.Limiter {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	return e.gpuLimiter
}

// NewEngine creates a new instance of the scale-from-zero engine.
// cfg must be non-nil (validated in main.go before engine creation).
func NewEngine(client client.Client, recorder record.EventRecorder, ds datastore.Datastore, cfg *config.Config) (*Engine, error) {
	if cfg == nil {
		return nil, errors.New("config is nil in NewEngine - this should not happen")
	}

	maxConcurrency := cfg.ScaleFromZeroMaxConcurrency()
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("invalid scale-from-zero max concurrency: must be positive, got %d", maxConcurrency)
	}

	engine := Engine{
		client:         client,
		recorder:       recorder,
		Datastore:      ds,
		maxConcurrency: maxConcurrency,
		config:         cfg,
	}

	// TODO: replace by an hybrid, polling and reactive executor when available
	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     100 * time.Millisecond, // frequent polling to quickly detect scale-from-zero opportunities
		RetryBackoff: 100 * time.Millisecond,
	})

	return &engine, nil
}

// StartOptimizeLoop starts the optimization loop for the scale-from-zero engine.
// It runs until the context is cancelled.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.executor.Start(ctx)
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) error {
	logger := log.FromContext(ctx)

	e.refreshLimiter(ctx) // pick up a limiters: edit without a restart
	e.beginTick()
	// Get all inactive (replicas == 0) VAs
	e.VariantEnricher.Refresh(ctx) // resolve the scale target of anything newly registered

	inactiveVAs, scaleTargets, err := utils.InactiveVariantAutoscaling(ctx, e.client, e.Variants)
	if err != nil {
		return err
	}

	logger.V(logging.DEBUG).Info("Found inactive VariantAutoscaling resources", "count", len(inactiveVAs))

	// Which roles a model is already running. A model whose decode is up can
	// serve, so it is the saturation engine's business, not this engine's.
	// Failing to read the active population is not fatal: treat coverage as
	// unknown (nothing covered), which at worst re-publishes an activation for a
	// target that is already awake.
	activeVAs, activeTargets, activeErr := utils.ActiveVariantAutoscaling(ctx, e.client, e.Variants)
	if activeErr != nil {
		logger.V(logging.DEBUG).Info("Could not list active variants; treating every role as uncovered", "error", activeErr)
	}
	coverage := activeRoleCoverage(activeVAs, activeTargets)
	observeCompletedWakes(activeVAs, activeTargets)

	// Wake decisions are made per model, not per variant: a P/D model needs a
	// decode (optionally with a prefill) chosen together against one GPU budget,
	// and every variant of a model shares one EPP queue, so grouping also
	// collapses what used to be one EPP scrape per inactive variant into one per
	// model.
	groups := groupInactiveByModel(inactiveVAs, activeVAs)
	logger.V(logging.DEBUG).Info("Grouped inactive variants by model", "models", len(groups))

	// Keep the refusal bookkeeping to the models still under consideration; it
	// would otherwise grow for the life of the process. A wake reason of a
	// model that left them, or that serves again, is cleared here.
	e.pruneRefusals(groups, coverage)

	var wg sync.WaitGroup
	sem := make(chan struct{}, e.maxConcurrency)
	errorCh := make(chan error, e.maxConcurrency)

	// Start error aggregation in a separate goroutine to prevent deadlock
	var aggregatedErrors []error
	var errorWg sync.WaitGroup
	errorWg.Add(1)
	go func() {
		defer errorWg.Done()
		for err := range errorCh {
			if err != nil {
				aggregatedErrors = append(aggregatedErrors, err)
			}
		}
	}()

modelLoop:
	for _, group := range groups {
		// Check if context is cancelled, but don't return immediately
		select {
		case <-ctx.Done():
			logger.V(logging.DEBUG).Info("Context cancelled, stopping new work")
			break modelLoop
		default:
		}

		logger.V(logging.DEBUG).Info("Processing model", "modelID", group.modelID, "namespace", group.namespace)
		wg.Add(1)

		// This call blocks if the channel is full (concurrency limit reached)
		sem <- struct{}{}
		go func(g modelGroup) {
			defer wg.Done()
			defer func() { <-sem }()

			err := e.processInactiveModel(ctx, scaleTargets, g, coverage[g.key()])
			if err != nil {
				// Info: logr ignores V() on Error. The error reaches the executor,
				// which logs it at ERROR.
				logger.V(logging.DEBUG).Info("Error processing model", "modelID", g.modelID, "namespace", g.namespace, "error", err)
				errorCh <- err
			} else {
				errorCh <- nil
			}
		}(group)
	}

	// Wait for all goroutines to complete, then close error channel
	wg.Wait()
	close(errorCh)

	// Wait for error aggregation to complete
	errorWg.Wait()

	// Whether each model has a wake signal at all, on its own 30-second clock and
	// covering serving models as well as parked ones. Off the per-model path
	// deliberately (see reportWakeSignal), and AFTER the wake loop rather than
	// before it: every pool with something parked has just been scraped, so its
	// verdict is already cached and the sweep pays only for pools that nothing is
	// parked on. Running it first would have cost a second scrape per model per
	// tick, which is exactly the cost the per-model grouping was introduced to
	// remove.
	if ctx.Err() == nil {
		e.reportWakeSignal(ctx, inactiveVAs, scaleTargets, activeVAs, activeTargets, time.Now())
	}

	// After all work is done, if the context was cancelled, return that error
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(aggregatedErrors) > 0 {
		return errors.Join(aggregatedErrors...)
	}
	return nil
}

// resolveScaleTarget returns the VA's scale target from the pre-populated map,
// falling back to a live fetch.
func (e *Engine) resolveScaleTarget(ctx context.Context, scaleTargets map[string]scaletarget.ScaleTargetAccessor, va wvav1alpha1.VariantAutoscaling) (scaletarget.ScaleTargetAccessor, error) {
	objName := va.GetScaleTargetName()
	if target, found := scaleTargets[utils.GetNamespacedKey(va.Namespace, objName)]; found {
		return target, nil
	}
	return scaletarget.FetchScaleTarget(ctx, e.client, va.Name, va.GetScaleTargetKind(), objName, va.Namespace)
}

// processInactiveModel decides whether a model with no running capacity should
// be woken, and if so which of its variants to wake.
//
// It replaces a per-variant loop that woke every inactive variant of the model.
// That both over-allocated (several variants for one model's demand) and
// under-served: a variant whose accelerator had no free GPUs was woken anyway,
// its pods sat Pending, and the request that asked for it timed out — a wake
// that looks like progress and delivers none.
func (e *Engine) processInactiveModel(
	ctx context.Context,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	group modelGroup,
	covered coverage,
) error {
	logger := log.FromContext(ctx)

	// Check if inferencepool datastore is empty: this can happen during bootstrapping
	if len(e.Datastore.PoolList()) == 0 {
		logger.Info("Inferencepool datastore is empty - skipping processing inactive model", "modelID", group.modelID)
		return nil
	}

	candidates, pool, err := e.buildCandidates(ctx, scaleTargets, group)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		// Every variant was skipped (no resolvable pool yet, typically during
		// bootstrap); buildCandidates has already logged why.
		return nil
	}

	// Use EPP source from registry
	namespacedPoolName := pool.Namespace + "/" + pool.Name
	eppSource := e.Datastore.PoolGetMetricsSource(namespacedPoolName)
	if eppSource == nil {
		// This is unexpected - pool exists but metrics source is missing
		err := fmt.Errorf("EPP metrics source not found in registry for pool %s", namespacedPoolName)
		logger.Error(err, "Datastore inconsistency detected",
			"modelID", group.modelID,
			"namespace", group.namespace,
			"pool", namespacedPoolName)
		return err
	}

	// One scrape per model per tick. Every variant of a model shares this queue,
	// so the old per-variant scrape re-fetched identical data N times through a
	// mutex-serialized HTTP call on a 100ms loop.
	startTime := time.Now()
	results, err := eppSource.Refresh(ctx, source.RefreshSpec{})
	duration := time.Since(startTime).Seconds()
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeQueueLength)

	// A failed scrape is still recorded as a collection error even when the
	// fallback answers: the direct path IS broken, and a fallback that silenced
	// the error would hide the outage it exists to survive.
	var fallbackPending source.MetricValue
	var fallbackHasPending, onFallback bool
	if err != nil {
		reason := prometheus.CategorizePrometheusError(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeQueueLength, reason)
		fallbackPending, fallbackHasPending, onFallback = e.QueueFallback.PendingAfterScrapeFailure(
			ctx, namespacedPoolName, pool.Namespace, group.modelID, err)
		if !onFallback {
			// No second transport, not yet past the failure threshold, or
			// Prometheus is down too. Surface the scrape error, as before.
			return err
		}
	} else {
		e.QueueFallback.ScrapeSucceeded(ctx, namespacedPoolName)
	}

	// A P/D model whose decode is up but whose prefill is still parked has to be
	// reconsidered even with an empty queue — and its queue IS empty, because the
	// decode it is serving from drained it. Gating that on demand would make the
	// completion depend on a queue that only exists while the model cannot serve,
	// which is precisely the window it has already left.
	catchUpWanted := covered.decode && !covered.prefill && hasPrefillCandidate(candidates)

	// Check for pending requests using EPP flowcontrol queue size metrics
	pending, pendingRequestExist := fallbackPending, fallbackHasPending
	// On the fallback path Prometheus answered, which is itself proof the family
	// exists somewhere; only the direct scrape can testify that EPP exports none.
	queueFamilyExported := true
	if !onFallback {
		result := results["all_metrics"]
		pending, pendingRequestExist, queueFamilyExported = pendingRequestsForModel(result.Values, group.modelID)
	}

	// Hand the verdict to the wake-signal cache rather than reporting it here.
	// This scrape has already proved the answer, so any pool with something parked
	// keeps its verdict current for free and the periodic sweep only pays for pools
	// nothing is parked on. Reporting from here instead would be skipped by every
	// early return above it — which is exactly the bug this replaced.
	if !onFallback {
		e.recordPoolWakeVerdict(namespacedPoolName, queueFamilyExported, time.Now())
	}
	if !pendingRequestExist && !catchUpWanted {
		// Scale-from-zero loop runs every 100ms; log at DEBUG to avoid flooding.
		logger.V(logging.DEBUG).Info("Scale-from-zero: skipping model, no pending requests in flow control queue",
			"namespace", group.namespace,
			"modelID", group.modelID)
		// No demand left: a wake refused earlier is no longer being refused.
		e.reportWakeBlocked(group, "")
		return nil
	}
	if pendingRequestExist {
		// DEBUG, not Info: this fires on every 100ms tick for as long as ANY
		// request is queued, including for models that are already serving fine
		// and for refusals whose reason has not changed. The events worth an Info
		// line are the outcomes below — a wake, or a refusal — not the demand.
		logger.V(logging.DEBUG).Info(
			"Target workload has pending requests",
			"metricName", pending.Labels[metricNameLabel],
			"metric", pending.Labels, "value", pending.Value)
	}

	place, selected, outcome := e.wakeVerdict(ctx, group, candidates, covered)
	constraints := place.constraints
	if len(selected) == 0 {
		if outcome == OutcomeAlreadyServing {
			// The steady state for every serving model with a queue. Not a
			// refusal, and on a 100ms loop it must never reach Info.
			//
			// Clear any remembered refusal too: the model is no longer in a
			// refused state, so if it parks again and is refused for the same
			// reason later, that is news and must be reported rather than
			// suppressed as unchanged.
			e.clearRefusal(group.key())
			logger.V(logging.DEBUG).Info("Scale-from-zero: model already has a running decode, leaving it to the saturation engine",
				"namespace", group.namespace, "modelID", group.modelID)
			return nil
		}
		// A genuine refusal: demand exists and nothing was woken for it. Logged
		// only when the verdict CHANGES, because this loop runs every 100ms and
		// an unchanged refusal repeated at Info would bury the log at 10 lines a
		// second for as long as the demand lasts.
		if e.refusalChanged(group.key(), outcome) {
			fields := []any{
				"namespace", group.namespace,
				"modelID", group.modelID,
				"reason", string(outcome),
			}
			if outcome == OutcomeNoCapacity || outcome == OutcomeNominalLentOut {
				// The one reason that cannot be diagnosed from the verdict alone: it
				// says a set did not fit, not what it was measured against nor what
				// it asked for. Both halves are needed — a budget of zero and a
				// demand of zero produce opposite verdicts for the same log line.
				budgets, nsScoped := allocation.GPUBudgets(constraints, group.namespace)
				fields = append(fields,
					"gpuBudgets", budgets,
					"namespaceScoped", nsScoped,
					"demand", candidateDemand(candidates))
			}
			logger.Info("Scale-from-zero: no variant woken for a model with pending requests", fields...)
		}
		return nil
	}
	e.clearRefusal(group.key())

	// Publishing cannot fail — see publishActivation, which deliberately has no
	// error path so that a wake can never be left half-applied. That matters most
	// for a P/D pair: the two members were validated against the JOINT budget, so
	// publishing one without the other would put capacity behind a set that was
	// never approved.
	for _, c := range selected {
		va, ok := group.variantByName(c.VariantName)
		if !ok {
			// Unreachable: candidates are built from group.variants, and variant
			// names are unique per namespace. Logged rather than skipped silently
			// so that if the two ever diverge it is visible.
			logger.Error(nil, "Selected variant is not in its own model group; skipping",
				"variant", c.VariantName, "namespace", group.namespace, "modelID", group.modelID)
			continue
		}
		e.publishActivation(ctx, scaleTargets, va, pool, 1, outcome)
	}
	return nil
}

// wakeVerdict is decideWake with the refusal named: a no-capacity refusal in a
// namespace whose queue lent nominal it cannot reclaim is nominal-lent-out
// (B10). A wake the limiters refuse is reported on wva_model_scaling_blocked
// (wakeBlockedReason).
func (e *Engine) wakeVerdict(ctx context.Context, group modelGroup, candidates []Candidate,
	covered coverage) (placement, []Candidate, SelectionOutcome) {
	place, selected, outcome := e.decideWake(ctx, group, candidates, covered)
	if len(selected) == 0 && outcome == OutcomeNoCapacity && place.nominalLentOut {
		// The budget it missed is within nominal, short of nominal because a
		// peer holds what the queue lent and need not give it back (B10).
		outcome = OutcomeNominalLentOut
	}
	reason := ""
	if len(selected) == 0 {
		reason = wakeBlockedReason(outcome)
	}
	e.reportWakeBlocked(group, reason)
	return place, selected, outcome
}

// wakeBlockedReason is the wva_model_scaling_blocked reason for a refused
// wake's outcome, or "" when the limiters did not refuse it.
func wakeBlockedReason(outcome SelectionOutcome) string {
	switch outcome {
	case OutcomeNoCapacity, OutcomePrefillRequired:
		return constants.ScalingBlockedWakeNoCapacity
	case OutcomeNominalLentOut:
		return constants.ScalingBlockedWakeNominalLentOut
	}
	return ""
}

// reportWakeBlocked sets reason (one of ScalingBlockedReasonsWakeCapacity) on
// wva_model_scaling_blocked for a model at zero whose wake the limiters
// refuse, every tick it is refused, and clears what it set once the model
// wakes, is refused for another reason or has no demand left (reason "").
// These reasons are this loop's alone, so clearing them deletes no other
// producer's series.
func (e *Engine) reportWakeBlocked(group modelGroup, reason string) {
	key := group.key()
	if reason != "" {
		e.wakeBlocked.Store(key, modelGroup{namespace: group.namespace, modelID: group.modelID})
		metrics.SetModelScalingBlockedReasons(group.namespace, group.modelID,
			constants.ScalingBlockedReasonsWakeCapacity, []string{reason})
		return
	}
	if _, set := e.wakeBlocked.LoadAndDelete(key); set {
		metrics.SetModelScalingBlockedReasons(group.namespace, group.modelID,
			constants.ScalingBlockedReasonsWakeCapacity, nil)
	}
}

// publishActivation records the wake for one selected variant: it publishes the
// activation decision KEDA acts on, then updates the variant's status and
// records the event.
//
// It returns nothing on purpose. Everything after the decision.Set below is
// bookkeeping, and an early return there would leave the target woken with no
// status or event — a half-applied state the 100ms loop would then repeat
// forever.
func (e *Engine) publishActivation(
	ctx context.Context,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	va wvav1alpha1.VariantAutoscaling,
	pool *poolutil.EndpointPool,
	targetWorkloadReplicas int,
	outcome SelectionOutcome,
) {
	logger := log.FromContext(ctx)
	objName := va.GetScaleTargetName()

	// 1. Publish the activation decision. Writing the decision store is the whole
	// actuation: it flips the external scaler's IsActive predicate to true and
	// wakes any open StreamIsActive, so KEDA scales the target off zero. WVA does
	// NOT patch the scale subresource itself — KEDA/HPA is the single writer, and
	// a direct patch here would fight the HPA that owns the same field.
	// TODO: Right now we are scaling all the VA for the same target model. We need to scale only the VA that has the lowest cost.
	//
	// Read before the write so the event below fires on the wake itself, not on
	// every 100ms tick that republishes the same activation while KEDA catches up.
	prev, hadPrev := decision.Get(va.Namespace, objName)
	changed := !hadPrev || prev.DesiredReplicas != int32(targetWorkloadReplicas)
	decision.Set(va.Namespace, objName, int32(targetWorkloadReplicas))
	// Acknowledge the wake so scale-to-zero leaves the model alone for its
	// retention period. The model has served nothing yet — the request that
	// caused this is still queued in the EPP — so the idle request counter the
	// enforcer reads is zero and would zero the replica straight back out from
	// under the request that asked for it.
	decision.MarkActivated(va.Namespace, va.Spec.ModelID)
	logger.Info("Published scale-from-zero activation for Target Workload",
		"variant", va.Name, "target VA model", va.Spec.ModelID,
		"inferencepool", pool.EndpointPicker.ServiceName, "servingSet", string(outcome))

	// 2. Determine accelerator - try status first, then labels
	va.Status.Actuation.Applied = false
	var accelerator string
	accelerator = va.Status.DesiredOptimizedAlloc.Accelerator
	if accelerator == "" {
		// Try to get from deployment/LWS nodeSelector/nodeAffinity, or VA labels
		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		if scaleTarget, found := scaleTargets[key]; found {
			accelerator = accel.GetAcceleratorNameFromScaleTarget(&va, scaleTarget)
		} else {
			// Deployment/LWS not cached, fall back to VA label via nil deployment/LWS
			accelerator = accel.GetAcceleratorNameFromScaleTarget(&va, nil)
		}
	}

	// 3. Updates VA status.
	numReplicas := int32(targetWorkloadReplicas)
	va.Status.DesiredOptimizedAlloc = wvav1alpha1.OptimizedAlloc{
		NumReplicas: &numReplicas,
		LastRunTime: metav1.Now(),
		Accelerator: accelerator,
	}

	// Set condition based on decision characteristics
	wvav1alpha1.SetCondition(&va,
		wvav1alpha1.TypeOptimizationReady,
		metav1.ConditionTrue,
		"ScaleFromZeroMode",
		"scalefromzero decision: "+string(domain.DecisionReasonScaleFromZero)+reasonDetails)

	// Record event just before Actuation.Applied = true. Only when this call
	// changed the published decision: the loop republishes the same activation
	// every tick until KEDA has scaled the target off zero. A wake always
	// publishes at least one replica (the only caller passes 1), so a changed
	// decision is always a scale-up.
	if changed {
		e.recorder.Eventf(&va, corev1.EventTypeNormal, constants.K8SEventScaledUp, string(domain.DecisionReasonScaleFromZero)+reasonDetails)
	}
	va.Status.Actuation.Applied = true

	// Log scaling decision for E2E and operators (mirrors saturation engine "Applied ... via shared cache").
	logger.Info("Scale-from-zero decision written to cache",
		"va", va.Name,
		"namespace", va.Namespace,
		"targetReplicas", targetWorkloadReplicas,
		"reason", string(domain.DecisionReasonScaleFromZero)+reasonDetails)
}

// observeCompletedWakes reports how long each finished wake took.
//
// This is the only place a wake's END can be seen. The engine stops publishing
// once a model is serving, so nothing downstream of publishActivation ever
// learns that the pod came up — and the 30s wake-signal sweep, which does see
// ready replicas, is far too coarse: it cannot separate a 2s warm FMA bind from
// a 50s cold load, which is the entire distinction the metric exists to make.
// This runs on the 100ms optimize loop, so the resolution matches the question.
//
// A model is counted only once ready replicas exist, not merely because its
// spec left zero. A wake that has scheduled a pod but not yet loaded the model
// has not finished the thing being measured — that gap IS the cold path.
func observeCompletedWakes(
	activeVAs []wvav1alpha1.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) {
	// Several variants of one model can go ready in the same pass (a P/D serving
	// set), and the wake is per model. CompleteWake deletes the episode as it
	// reports it, so the second variant finds nothing open and the model
	// contributes one observation rather than one per variant.
	for i := range activeVAs {
		va := &activeVAs[i]
		st, found := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
		if !found || st.GetStatusReadyReplicas() < 1 {
			continue
		}
		if took, ok := decision.CompleteWake(va.Namespace, va.Spec.ModelID); ok {
			metrics.ObserveWakeDuration(va.Namespace, va.Spec.ModelID, took.Seconds())
		}
	}
}

// recordWake accounts for a wake about to be published against place.
//
// Admitted against a snapshot nothing refreshes, it is charged to the snapshot,
// so the next wake in its lifetime does not pass on the GPUs this one takes.
// Admitted with no check at all although limiters are declared -- every
// provider unavailable, no young snapshot -- it is counted and logged, one per
// wake, so an uncapped wake is never silent.
func (e *Engine) recordWake(ctx context.Context, group modelGroup, place placement, selected []Candidate) {
	prior := maps.Clone(e.tickWakes[group.namespace])
	if !place.snapshotTakenAt.IsZero() {
		e.debitSnapshot(group.namespace, place.snapshotTakenAt, demandOf(selected))
	} else if place.constraints != nil {
		e.debitTick(group.namespace, demandOf(selected))
	}
	// What the wake takes on the nodes, for the next wake in this tick, and in
	// the in-flight ledger for every later decision of either engine until its
	// pods hold a node. A set that passed the node check (fits) places by
	// construction. Two kinds of wake bypass that check, and place and hold
	// only what fits now: a claimed share transfer (claimShareTransfer funds
	// the wake from a donor's pods still releasing, which the snapshot shows
	// in use -- the transfer, not the ledger, holds that hole); and a wake
	// checked against a remembered snapshot, or not at all, which has no node
	// view (decideWake builds one for this tick's constraints only).
	// A wake into lent-out nominal places without a hole (fits); what it took
	// of the namespace's reclaimable nominal is gone for this tick's next wake.
	placed, took := place.nodes.PlaceReplicasReclaiming(replicaShapes(group.namespace, selected, place.queueFlavor), true, place.reclaimable)
	if placed && place.nodes != nil {
		e.admitInFlight(group.namespace, selected, place.queueFlavor)
	}
	e.debitReclaimed(group.namespace, took)
	// What the wake takes of its cohort's free pool, and what Kueue reclaims
	// for it from borrowers, is in no Kueue read until its pods are admitted:
	// the shared record holds it, so the steady-state engine does not borrow
	// the same GPUs meanwhile, nor a later wake reclaim them again (N3).
	if placed && place.snapshotTakenAt.IsZero() && place.raw != nil {
		allocation.DefaultBorrowInFlight.RecordWake(allocation.WakeDraws(place.raw, group.namespace, group.modelID,
			wakeReplicas(selected), prior, place.reclaimable), time.Now())
	}
	if place.unchecked {
		metrics.CountScaleFromZeroWakeUnchecked(group.namespace)
		log.FromContext(ctx).Info("Scale-from-zero: woke WITHOUT a GPU capacity check; every declared limiter is unavailable and no recent snapshot exists",
			"namespace", group.namespace, "modelID", group.modelID, "metric", constants.WVAScaleFromZeroWakeUnchecked)
	}
}

// selectOrClaim selects the variants a wake starts. When no idle GPU and no
// reserve can host them, a transfer still releasing for another receiver may
// be claimed instead (utilization-share optimizer, section 6.3); outcome is
// the selection's either way.
func (e *Engine) selectOrClaim(ctx context.Context, group modelGroup, candidates []Candidate, covered coverage,
	place placement) ([]Candidate, SelectionOutcome) {
	selected, outcome := selectServingSet(SelectionInput{
		Namespace:      group.namespace,
		Candidates:     candidates,
		DecodeCovered:  covered.decode,
		PrefillCovered: covered.prefill,
		Constraints:    place.constraints,
		Admitted:       place.admitted,
		Nodes:          place.nodes,
		Reclaimable:    place.reclaimable,
		QueueFlavor:    place.queueFlavor,
		RequirePrefill: e.requirePrefill(group),
	})
	if len(selected) == 0 && outcome == OutcomeNoCapacity {
		if set, ok := e.claimShareTransfer(ctx, group, candidates, place.constraints, covered); ok {
			selected = set
		}
	}
	return selected, outcome
}
