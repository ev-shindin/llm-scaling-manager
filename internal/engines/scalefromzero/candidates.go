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
	"strconv"
	"strings"
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
// A provider that cannot be consulted is SKIPPED and the others still bound the
// wake (allocation.ComputeAvailableConstraints): its usage view has not been
// published on the basis it needs, the one that has is too old, or it could not
// compute constraints. Skipping is safe because constraints intersect -- the
// survivors deny only what the full set would deny too. This used to drop
// everything: a quota whose Kueue read failed lifted the physical bound beside
// it, and a physical view not yet observed lifted every quota.
//
// When EVERY provider is unavailable, the wake is checked against this
// namespace's last successful constraints if they are less than
// lastConstraintsMaxAge old: an API blip of a few seconds must not turn a
// placement decision into none at all. A snapshot is a picture of free GPUs
// that nothing refreshes, so every wake admitted against it is debited from it
// (placement.admitted) for the rest of its life: a second wake must not pass on
// the GPUs the first one took. Older than that, or with no limiter
// injected -- in production only when NewLimiterFromConfig itself failed, since
// declaring no limiters builds a NoOpLimiter, which provides no constraints --
// the wake is published without a capacity check.
//
// The check is therefore best-effort by construction: it prevents waking onto an
// accelerator that is known to be full, and stays out of the way when placement
// cannot be reasoned about. The snapshot debit is best-effort too: it is kept
// per namespace, so wakes in two namespaces can still both pass on cluster GPUs
// one snapshot each counted free, and per accelerator name as each wake
// declared it, so two spellings of one GPU are debited apart.
//
// Every one of those exits is REPORTED (see reportUnchecked). "This wake was not
// capacity-checked" is not a detail: it is the difference between a placement
// decision and no decision at all, and it is invisible in the outcome — the wake
// looks identical either way. These used to be V(4) lines, which the shipped
// verbosity discards, so the permissive path left no trace whatsoever.
func (e *Engine) gpuConstraints(ctx context.Context, namespace string) []*allocation.ResourceConstraints {
	return e.placementFor(ctx, namespace).constraints
}

// placement is what one namespace's wakes are judged against this tick.
type placement struct {
	// constraints are the GPU/quota constraints; nil means unchecked.
	constraints []*allocation.ResourceConstraints
	// snapshotTakenAt is set when constraints are the namespace's last
	// successful ones rather than this tick's, and admitted is then what
	// earlier wakes took against that snapshot, by accelerator name.
	snapshotTakenAt time.Time
	admitted        map[string]int
	// unchecked is true when limiters are declared but every provider was
	// unavailable and no young snapshot existed: a wake goes uncapped although
	// the operator asked for a bound.
	unchecked bool
}

// placementFor computes the placement basis for namespace; see gpuConstraints.
func (e *Engine) placementFor(ctx context.Context, namespace string) placement {
	providers := allocation.ConstraintProvidersFrom(e.currentGPULimiter())
	if len(providers) == 0 {
		e.reportUnchecked(ctx, namespace, "no constraint provider is configured")
		return placement{}
	}

	views, missing := e.gpuUsageViews(ctx, namespace, providers)
	constraints, skipped := allocation.ComputeAvailableConstraints(ctx, providers, views, nil)
	// Say WHY a view is missing, not merely that it is: "no snapshot yet" and
	// "a snapshot 6m old" call for different fixes.
	for i, s := range skipped {
		if reason, ok := missing[s.Basis]; ok && s.Unobserved {
			skipped[i].Reason = reason
		}
	}
	now := time.Now()
	var (
		snapshotAge time.Duration
		p           placement
	)
	if len(constraints) == 0 {
		// Every provider unavailable. The usual cause of a failure is the Node
		// API: an inventory limiter's Refresh lists nodes, so losing that
		// permission (or the API) takes the physical check out. Note a
		// SUCCESSFUL discovery that finds no GPU nodes does the opposite —
		// empty pools mean every demanded type is unknown, and FitsGPUBudget
		// denies unknown types outright. Only an error is permissive.
		last, ok := e.lastConstraints(namespace, now)
		if !ok {
			e.reportUnchecked(ctx, namespace, describeSkipped(skipped))
			return placement{unchecked: true}
		}
		constraints, snapshotAge = last.constraints, now.Sub(last.takenAt)
		p.snapshotTakenAt, p.admitted = last.takenAt, last.admitted
	} else {
		e.rememberConstraints(namespace, constraints, now)
	}
	// A wake may not take GPUs the utilization-share optimizer has promised
	// to a receiver: they are released but not yet held, and taking them would
	// leave the receiver Pending until its fill timeout. Claiming them by
	// score (section 6.3 of that proposal) is not built; a wake waits for idle
	// GPUs instead.
	constraints = allocation.WithholdPromised(constraints, decision.LatestSharePromised(now))
	// A wake spends capacity within nominal only, never borrowed capacity: a
	// model woken onto borrowed GPUs can be preempted before it serves (B7).
	// Within nominal, not guaranteed -- an idle queue on Kueue's default
	// (Never) has a guaranteed figure of 0, and must still wake.
	constraints = allocation.WithinNominal(constraints)
	e.logBudgets(ctx, namespace, constraints, views, skipped, snapshotAge)
	p.constraints = constraints
	return p
}

// lastConstraintsMaxAge bounds how old the last successful constraints may be
// and still check a wake when every provider is unavailable. Past it the wake
// goes unchecked, as it always did: a stale bound refuses wakes for a reason
// that may no longer hold, and an API outage that long is not a blip.
const lastConstraintsMaxAge = 5 * time.Minute

// constraintSnapshot is one namespace's last successful constraints, before
// promised GPUs are withheld (those are re-read at every use).
type constraintSnapshot struct {
	constraints []*allocation.ResourceConstraints
	takenAt     time.Time
	// admitted is what wakes checked against this snapshot took, by
	// accelerator name as each declared it (debitSnapshot).
	admitted map[string]int
}

// rememberConstraints records namespace's constraints as its last successful
// ones.
func (e *Engine) rememberConstraints(namespace string, constraints []*allocation.ResourceConstraints, now time.Time) {
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()
	if e.lastGood == nil {
		e.lastGood = make(map[string]constraintSnapshot)
	}
	// Entries past the age bound are never used again: drop them, so a
	// namespace that stopped waking leaves nothing behind.
	for ns, s := range e.lastGood {
		if now.Sub(s.takenAt) >= lastConstraintsMaxAge {
			delete(e.lastGood, ns)
		}
	}
	e.lastGood[namespace] = constraintSnapshot{constraints: constraints, takenAt: now}
}

// forgetConstraints drops every remembered snapshot: they were computed by a
// limiter that has just been replaced, and must not check a wake against a
// bound the new configuration no longer declares.
func (e *Engine) forgetConstraints() {
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()
	clear(e.lastGood)
}

// lastConstraints returns namespace's last successful constraints, when they are
// younger than lastConstraintsMaxAge, with a copy of what has been admitted
// against them.
func (e *Engine) lastConstraints(namespace string, now time.Time) (constraintSnapshot, bool) {
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()
	s, ok := e.lastGood[namespace]
	if !ok {
		return constraintSnapshot{}, false
	}
	if now.Sub(s.takenAt) >= lastConstraintsMaxAge {
		delete(e.lastGood, namespace)
		return constraintSnapshot{}, false
	}
	s.admitted = maps.Clone(s.admitted)
	return s, true
}

// debitSnapshot charges a wake's demand to the snapshot it was admitted
// against. The snapshot is a picture of free GPUs that nothing refreshes while
// every provider is unavailable; without the debit, each wake in its lifetime
// passed on the same free GPUs. Only the snapshot taken at takenAt is debited:
// one replaced since is fresher, and a removed one is no longer used.
func (e *Engine) debitSnapshot(namespace string, takenAt time.Time, demand map[string]int) {
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()
	s, ok := e.lastGood[namespace]
	if !ok || !s.takenAt.Equal(takenAt) {
		return
	}
	if s.admitted == nil {
		s.admitted = make(map[string]int, len(demand))
	}
	for accelerator, gpus := range demand {
		s.admitted[accelerator] += gpus
	}
	e.lastGood[namespace] = s
}

// describeSkipped joins why each provider was skipped, for the unchecked report.
func describeSkipped(skipped []allocation.SkippedProvider) string {
	reasons := make([]string, 0, len(skipped))
	for _, s := range skipped {
		reasons = append(reasons, fmt.Sprintf("provider %q: %s", s.Name, s.Reason))
	}
	return "every constraint provider is unavailable and no constraints younger than " +
		lastConstraintsMaxAge.String() + " are on hand: " + strings.Join(reasons, "; ")
}

// gpuUsageViews assembles the usage measures this namespace's providers need.
// A measure that has not been observed, or is too old, is left out of the views
// and its reason returned under its basis; the providers that need it are then
// skipped, and the rest still bound the wake.
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
) (allocation.GPUUsageViews, map[allocation.UsageBasis]string) {
	var views allocation.GPUUsageViews
	missing := map[allocation.UsageBasis]string{}
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

		if usage, reason := freshUsage(decision.LatestGPUUsage,
			"no cluster GPU-usage snapshot has been published yet"); reason != "" {
			missing[allocation.PhysicalUsage] = reason
		} else {
			views.PhysicalByType = usage.ByType
			views.PhysicalByNamespace = withNamespace(usage.ByNamespace, namespace)
		}
	}

	if needed[allocation.ManagedUsage] {
		// Published by the saturation engine, which is the only place the managed
		// population exists — including as an explicit zero when its fleet is
		// entirely parked, so a quota can still be evaluated for the very workloads
		// this engine wakes.
		if usage, reason := freshUsage(decision.LatestManagedGPUUsage,
			"the saturation engine has published no WVA-managed GPU-usage snapshot yet (no completed cycle)"); reason != "" {
			missing[allocation.ManagedUsage] = reason
		} else {
			views.ManagedByType = usage.ByType
			views.ManagedByNamespace = withNamespace(usage.ByNamespace, namespace)
		}
	}

	return views, missing
}

// freshUsage reads a snapshot and bounds how old it may be, returning why it is
// unusable ("" when it is usable).
//
// The bound matters because a producer publishes only measured passes, deliberately
// leaving the last snapshot alone when one fails. That is right for a blip, but an
// unbounded one would have this engine placing wakes against an arbitrarily old
// picture of the cluster, at 10Hz, for as long as the outage lasts.
func freshUsage(
	read func() (*decision.GPUUsage, bool),
	missingReason string,
) (*decision.GPUUsage, string) {
	usage, ok := read()
	if !ok {
		return nil, missingReason
	}
	if age := time.Since(usage.TakenAt); age > gpuUsageMaxAge {
		return nil, fmt.Sprintf(
			"a GPU-usage snapshot is %s old, past the %s bound", age.Truncate(time.Second), gpuUsageMaxAge)
	}
	return usage, ""
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
//
// A provider left out (skipped) is named, and a check against the last
// successful constraints says how old they are: a bound that stopped applying,
// or that is being read from memory, must not look like one that was consulted.
func (e *Engine) logBudgets(ctx context.Context, namespace string, constraints []*allocation.ResourceConstraints,
	views allocation.GPUUsageViews, skipped []allocation.SkippedProvider, snapshotAge time.Duration) {
	budgets, nsScoped := allocation.GPUBudgets(constraints, namespace)
	skippedNames := make([]string, 0, len(skipped))
	for _, s := range skipped {
		skippedNames = append(skippedNames, s.Name)
	}
	// The snapshot's age is not part of the change key -- it moves every tick --
	// only whether one is in use.
	if !e.placementBasisChanged(namespace, fmt.Sprintf("ok|%v|%t|%v|%v|%v|%t",
		budgets, nsScoped, views.PhysicalByType, views.ManagedByType, skippedNames, snapshotAge > 0)) {
		return
	}
	kv := []any{
		"namespace", namespace,
		"gpuBudgets", budgets,
		"namespaceScoped", nsScoped,
	}
	if len(skipped) > 0 {
		reasons := make([]string, 0, len(skipped))
		for _, s := range skipped {
			reasons = append(reasons, s.String())
		}
		kv = append(kv, "providersSkipped", reasons)
	}
	if snapshotAge > 0 {
		kv = append(kv, "fromLastConstraintsAge", snapshotAge.Truncate(time.Second).String())
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
