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
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	accel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/variantmeta"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpuusage"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// gpuUsageMaxAge bounds how old a GPU-usage snapshot may be and still be used to
// place a wake.
//
// The producer publishes only measured cycles, deliberately leaving the previous
// snapshot alone when collection fails rather than overwriting it with zeros.
// That is right for a blip but needs an upper bound: without one, a saturation
// loop wedged by a long Prometheus outage would leave this engine placing wakes
// against an arbitrarily old picture of the cluster, ten times a second, for the
// duration. Generous relative to the default 15s optimize interval — and to the
// 60s the OpenShift overlay ships — so a slow-but-working cycle is never treated
// as an outage.
const gpuUsageMaxAge = 5 * time.Minute

// variantByName finds a group member by variant name.
func (g modelGroup) variantByName(name string) (wvav1alpha1.VariantAutoscaling, bool) {
	for _, va := range g.variants {
		if va.Name == name {
			return va, true
		}
	}
	return wvav1alpha1.VariantAutoscaling{}, false
}

// buildCandidates turns a model's inactive variants into selection candidates
// and resolves the EPP pool they share.
//
// A variant whose pool cannot be resolved is skipped rather than failing the
// model: that is the normal bootstrap state, and one unresolvable variant must
// not stop its siblings from being woken. The pool returned is the first that
// resolves; a model's variants serve the same model behind the same EPP, so they
// share a queue.
func (e *Engine) buildCandidates(
	ctx context.Context,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	group modelGroup,
) ([]Candidate, *poolutil.EndpointPool, error) {
	logger := log.FromContext(ctx)

	var (
		candidates []Candidate
		pool       *poolutil.EndpointPool
	)

	for _, va := range group.variants {
		scaleTarget, err := e.resolveScaleTarget(ctx, scaleTargets, va)
		if err != nil {
			return nil, nil, err
		}
		podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
		if podTemplateSpec == nil {
			return nil, nil, errors.New("pod template spec is missing for target workload object")
		}
		labels := podTemplateSpec.Labels
		if labels == nil {
			return nil, nil, errors.New("labels are missing for target workload object")
		}

		variantPool, err := e.Datastore.PoolGetFromLabels(va.Namespace, labels)
		if err != nil {
			// Only skip on "not found" errors - return other errors to surface real datastore failures
			if errors.Is(err, datastore.ErrPoolNotSynced) {
				logger.V(logging.DEBUG).Info("Skipping variant, target EPP not found in datastore",
					"variant", va.Name,
					"namespace", va.Namespace,
					"modelID", va.Spec.ModelID)
				continue
			}
			logger.Error(err, "Unexpected error finding target EPP",
				"variant", va.Name,
				"namespace", va.Namespace,
				"modelID", va.Spec.ModelID)
			return nil, nil, err
		}
		if pool == nil {
			pool = variantPool
		}

		candidates = append(candidates, Candidate{
			VariantName:     va.Name,
			ScaleTargetName: va.GetScaleTargetName(),
			Role:            variantmeta.RoleFromScaleTarget(scaleTarget),
			Accelerator:     candidateAccelerator(&va, scaleTarget),
			GPUsPerReplica:  scaleTarget.GetTotalGPUsPerReplica(),
			PodGPUs:         scaletarget.PodGPUs(scaleTarget),
			Cost:            resolveVariantCost(ctx, va),
		})
	}

	if pool == nil {
		return nil, nil, nil
	}
	return candidates, pool, nil
}

// candidateAccelerator returns the variant's accelerator exactly as declared.
//
// It is deliberately NOT normalized here. Pool keys are normalized short names
// while a workload may declare either a full product label
// ("NVIDIA-A100-PCIE-80GB") or an already-short one ("A100"), so the two sides
// have to be reconciled — but doing it by normalizing the candidate is wrong in
// both directions. NormalizeAcceleratorName guesses for names it has no vendor
// prefix for ("Gaudi-2" -> "2", via its `return parts[1]` fallback), so
// normalizing a short name that happens to contain a hyphen produces a value
// matching no pool at all. Reconciliation instead happens at lookup time against
// the pool keys that actually exist — see FitsGPUBudget.
func candidateAccelerator(va *wvav1alpha1.VariantAutoscaling, scaleTarget scaletarget.ScaleTargetAccessor) string {
	return accel.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
}

// resolveVariantCost reads the variant's declared cost, falling back to the project
// default when it is absent or unparseable.
//
// VariantCost is optional (`variantCost,omitempty`), and domain.DefaultVariantCost
// is what every other consumer assumes for an unset one — see costFromVA in
// internal/engines/discovery. Ranking such a variant behind every well-formed one
// instead would invert that default: a fleet that never sets variantCost would
// have no meaningful cost order at all, and a cheap-but-unset variant would lose
// to a declared-expensive one.
func resolveVariantCost(ctx context.Context, va wvav1alpha1.VariantAutoscaling) float64 {
	if va.Spec.VariantCost == "" {
		return domain.DefaultVariantCost
	}
	cost, err := strconv.ParseFloat(va.Spec.VariantCost, 64)
	if err != nil {
		log.FromContext(ctx).V(logging.DEBUG).Info("Variant has an unparseable cost; using the default",
			"variant", va.Name, "variantCost", va.Spec.VariantCost, "default", domain.DefaultVariantCost)
		return domain.DefaultVariantCost
	}
	return cost
}

// gpuConstraints returns the GPU/quota constraints to place a wake within, or
// nil when they cannot be determined.
//
// Nil means "unknown", which FitsGPUBudget treats as permissive. That is
// deliberate and matches the saturation engine's own fallback when no provider
// can supply constraints: proceed rather than block scaling silently. The first
// tick after a restart has no usage snapshot yet, and that is exactly when a
// queued request is waiting to be served.
//
// Three things make this nil, and each means the wake is published without a
// capacity check:
//
//   - No limiter was injected. In production SetGPULimiter is always called, so
//     this only happens when NewLimiterFromConfig itself failed at startup.
//     Declaring no limiters: list does NOT land here: EffectiveLimiterMode then
//     returns the inventory mode, which builds a DefaultLimiter — and that IS a
//     ConstraintProvider, so the physical check still runs.
//
//   - No usage snapshot has been published on a basis some provider needs, or the
//     one that has is too old. Note this is independent of enableLimiter — the
//     snapshots are published outside the optimizer guard precisely so that a
//     default (CostAware) deployment still feeds this path.
//
//   - A provider could not compute constraints (see below): a partial view would
//     deny types the surviving providers happen not to mention.
//
// The check is therefore best-effort by construction: it prevents waking onto an
// accelerator that is known to be full, and stays out of the way when placement
// cannot be reasoned about.
//
// Every one of those exits is REPORTED (see reportUnchecked). "This wake was not
// capacity-checked" is not a detail: it is the difference between a placement
// decision and no decision at all, and it is invisible in the outcome — the wake
// looks identical either way. These used to be V(4) lines, which the shipped
// verbosity discards, so the permissive path left no trace whatsoever.
func (e *Engine) gpuConstraints(ctx context.Context, namespace string) []*allocation.ResourceConstraints {
	providers := allocation.ConstraintProvidersFrom(e.currentGPULimiter())
	if len(providers) == 0 {
		e.reportUnchecked(ctx, namespace, "no constraint provider is configured")
		return nil
	}

	views, ok := e.gpuUsageViews(ctx, namespace, providers)
	if !ok {
		return nil // gpuUsageViews reported why
	}

	var constraints []*allocation.ResourceConstraints
	for _, cp := range providers {
		usageByType, usageByNamespace := views.For(cp)
		c, err := cp.ComputeConstraints(ctx, usageByType, usageByNamespace)
		if err != nil {
			// A partial view is worse than none. FitsGPUBudget denies any
			// accelerator type its constraints do not mention, so proceeding with
			// the survivors would turn "this provider could not be reached" into
			// "this variant cannot be placed" — refusing a wake with requests
			// already queued, on the strength of a failed lookup. Fall back to
			// unknown, matching the saturation engine, which drops to its
			// unlimited optimizer rather than let scale-up be silently blocked.
			//
			// The usual cause is the Node API: an inventory limiter's Refresh
			// lists nodes, so losing that permission (or the API) turns the whole
			// check off. Note a SUCCESSFUL discovery that finds no GPU nodes does
			// the opposite — empty pools mean every demanded type is unknown, and
			// FitsGPUBudget denies unknown types outright. Only an error is
			// permissive.
			e.reportUnchecked(ctx, namespace, fmt.Sprintf(
				"provider %q could not compute constraints: %v", cp.Name(), err))
			return nil
		}
		constraints = append(constraints, c)
	}
	// A wake may not take GPUs the utilization-share optimizer has promised
	// to a receiver: they are released but not yet held, and taking them would
	// leave the receiver Pending until its fill timeout. Claiming them by
	// score (section 6.3 of that proposal) is not built; a wake waits for idle
	// GPUs instead.
	constraints = allocation.WithholdPromised(constraints, decision.LatestSharePromised(time.Now()))
	e.logBudgets(ctx, namespace, constraints, views)
	return constraints
}

// gpuUsageViews assembles the usage measures this namespace's providers need,
// reporting and returning false if one of them has not been observed.
//
// Only the measures some provider actually asks for are gathered. A quota-only
// deployment must not be held up waiting for a physical observation it never
// consults, and a physical-only one — the default — must not be held up waiting
// for a saturation cycle to publish a managed figure nothing reads.
//
// A missing view cannot be substituted with zeros. Absent means "unknown", which
// this engine treats as permissive; an empty map is a confident claim that
// nothing is in use, and a provider handed one reports its entire capacity free.
// The two are opposite answers, and only one of them is honest here.
func (e *Engine) gpuUsageViews(
	ctx context.Context,
	namespace string,
	providers []allocation.ConstraintProvider,
) (allocation.GPUUsageViews, bool) {
	var views allocation.GPUUsageViews
	needed := make(map[allocation.UsageBasis]bool, 2)
	for _, cp := range providers {
		needed[allocation.UsageBasisOf(cp)] = true
	}

	if needed[allocation.PhysicalUsage] {
		// Observe before deciding. A wake is considered the instant demand appears,
		// which is routinely within a second of the cluster changing, so the periodic
		// observation alone can be a whole interval out of date at exactly the moment
		// it is consulted — and a workload that has just started holding GPUs is
		// precisely what a placement must not miss.
		e.UsageRefresher.EnsureFresh(ctx, gpuusage.DecisionMaxAge)

		usage, ok := e.freshUsage(ctx, namespace, decision.LatestGPUUsage,
			"no cluster GPU-usage snapshot has been published yet")
		if !ok {
			return views, false
		}
		views.PhysicalByType = usage.ByType
		views.PhysicalByNamespace = withNamespace(usage.ByNamespace, namespace)
	}

	if needed[allocation.ManagedUsage] {
		// Published by the saturation engine, which is the only place the managed
		// population exists — including as an explicit zero when its fleet is
		// entirely parked, so a quota can still be evaluated for the very workloads
		// this engine wakes.
		usage, ok := e.freshUsage(ctx, namespace, decision.LatestManagedGPUUsage,
			"the saturation engine has published no WVA-managed GPU-usage snapshot yet (no completed cycle)")
		if !ok {
			return views, false
		}
		views.ManagedByType = usage.ByType
		views.ManagedByNamespace = withNamespace(usage.ByNamespace, namespace)
	}

	return views, true
}

// freshUsage reads a snapshot and bounds how old it may be, reporting the wake as
// unchecked and returning false when it is missing or stale.
//
// The bound matters because a producer publishes only measured passes, deliberately
// leaving the last snapshot alone when one fails. That is right for a blip, but an
// unbounded one would have this engine placing wakes against an arbitrarily old
// picture of the cluster, at 10Hz, for as long as the outage lasts.
func (e *Engine) freshUsage(
	ctx context.Context,
	namespace string,
	read func() (*decision.GPUUsage, bool),
	missingReason string,
) (*decision.GPUUsage, bool) {
	usage, ok := read()
	if !ok {
		e.reportUnchecked(ctx, namespace, missingReason)
		return nil, false
	}
	if age := time.Since(usage.TakenAt); age > gpuUsageMaxAge {
		e.reportUnchecked(ctx, namespace, fmt.Sprintf(
			"a GPU-usage snapshot is %s old, past the %s bound", age.Truncate(time.Second), gpuUsageMaxAge))
		return nil, false
	}
	return usage, true
}

// withNamespace returns byNamespace with an entry for namespace, adding an empty
// one if absent. The stored snapshot must not be mutated (GPUUsageStore.Get
// documents it as shared), so it copies when it has to.
//
// A namespace-scoped quota is only materialised for namespaces present in the map
// handed to the provider, and both producers build theirs from what is currently
// HOLDING GPUs — so a namespace whose fleet is entirely parked, which is every
// namespace this engine asks about, would be absent and its quota simply would not
// apply. The wake would then be judged against the cluster aggregate instead: it
// could exceed the namespace's own cap, or be refused because a DIFFERENT namespace
// is full. Present with zero usage is the accurate statement.
func withNamespace(byNamespace map[string]map[string]int, namespace string) map[string]map[string]int {
	if _, present := byNamespace[namespace]; present {
		return byNamespace
	}
	out := make(map[string]map[string]int, len(byNamespace)+1)
	for ns, perType := range byNamespace {
		out[ns] = perType
	}
	out[namespace] = map[string]int{}
	return out
}

// logBudgets reports the GPU budgets this namespace's wakes are being judged
// against, once per change.
//
// A wake that is ALLOWED logs no reason, so an over-permissive capacity check is
// invisible from the outside: the only symptom is a variant coming up on an
// accelerator that was supposed to be full, which looks exactly like correct
// behaviour. This is the line that distinguishes them — it says what WVA believed
// was free, and the measured usage it derived that from.
// Both usage measures are reported when both were consulted: a budget that looks
// wrong is almost always one of the two being fed where the other belongs, and
// that is only visible if the line says which is which.
func (e *Engine) logBudgets(ctx context.Context, namespace string, constraints []*allocation.ResourceConstraints, views allocation.GPUUsageViews) {
	budgets, nsScoped := allocation.GPUBudgets(constraints, namespace)
	if !e.placementBasisChanged(namespace, fmt.Sprintf("ok|%v|%t|%v|%v",
		budgets, nsScoped, views.PhysicalByType, views.ManagedByType)) {
		return
	}
	kv := []any{
		"namespace", namespace,
		"gpuBudgets", budgets,
		"namespaceScoped", nsScoped,
	}
	if views.Has(allocation.PhysicalUsage) {
		kv = append(kv, "gpusInUse", views.PhysicalByType)
	}
	if views.Has(allocation.ManagedUsage) {
		kv = append(kv, "gpusInUseByWVA", views.ManagedByType)
	}
	log.FromContext(ctx).Info("Scale-from-zero: GPU budgets available for placement", kv...)
}

// reportUnchecked records that wakes in this namespace are being published with
// NO capacity check, and why.
//
// Reported at Info, at the same level as the budgets themselves, because the two
// are the same fact: what this wake was placed against. Silence here reads as
// "the check passed" when it means "there was no check", and that misreading is
// expensive — it is the reason an unbounded wake onto a full accelerator can look
// like correct behaviour for as long as it lasts.
func (e *Engine) reportUnchecked(ctx context.Context, namespace, reason string) {
	if !e.placementBasisChanged(namespace, "none|"+reason) {
		return
	}
	log.FromContext(ctx).Info("Scale-from-zero: waking WITHOUT a GPU capacity check",
		"namespace", namespace, "reason", reason)
}

// placementBasisChanged reports whether what this namespace's wakes are judged
// against differs from what was last reported, recording it either way.
//
// Gated on a CHANGE rather than a tick: gpuConstraints runs at 10Hz for every
// model with pending requests, but the basis only moves when the fleet does. That
// makes the log rate proportional to real events, which is what earns these lines
// Info. It also means the transition that matters most — checked becoming
// unchecked, or the reverse — is always reported, since the summaries differ.
//
// Guarded by refusalMu; see lastBudgets.
func (e *Engine) placementBasisChanged(namespace, summary string) bool {
	e.refusalMu.Lock()
	defer e.refusalMu.Unlock()
	if e.lastBudgets == nil {
		e.lastBudgets = make(map[string]string)
	}
	if e.lastBudgets[namespace] == summary {
		return false
	}
	e.lastBudgets[namespace] = summary
	return true
}

// requirePrefill reports whether the model refuses a decode-only wake.
//
// Resolution goes through ScalingPolicy.Merge — the same overlay every
// other per-entry setting uses — rather than a hand-rolled precedence check, so
// there is one implementation of "model-specific entry wins over default" and it
// cannot drift from the rest of the config. Only the merge is run, not
// ApplyDefaults/ApplyV2ThresholdDefaults, which calibrate scaling thresholds that
// have no bearing on this field.
func (e *Engine) requirePrefill(modelID, namespace string) bool {
	if e.config == nil {
		return false
	}
	entries := e.config.ScalingPolicyConfigForNamespace(namespace)
	resolved := entries["default"] // zero value when absent, which reads as false
	if override, ok := entries[modelID+"#"+namespace]; ok {
		resolved.Merge(override)
	}
	return resolved.RequirePrefillOnScaleFromZero()
}
