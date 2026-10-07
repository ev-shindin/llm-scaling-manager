// Package discovery resolves the authoritative per-variant metadata for one
// optimization cycle: variant identity (name, model, namespace, role,
// accelerator, cost) together with replica and GPU state, sourced from the
// managed scale targets and their synthesized VariantAutoscaling records.
//
// It is intended to be the single source of this metadata for both the analyzers
// and the optimizer, replacing the previous scheme in which the saturation
// analyzer laundered identity onto its per-variant capacity output (and cost and
// accelerator name were stamped onto every pod's ReplicaMetrics). Analyzers can
// then reduce to pure (demand, per-replica-capacity) producers.
package variantmeta

import (
	"context"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// RoleLabel is the pod-template label carrying a variant's P/D disaggregation
// role, and the sole signal WVA uses to distinguish roles. Any other value, or
// its absence, means the non-disaggregated role "both".
const RoleLabel = "llm-d.ai/role"

// Discover resolves the domain.VariantMetadata for each VariantAutoscaling. It prefers a
// scale target from the provided map (populated earlier in the cycle) and falls
// back to a live fetch; a VA whose scale target cannot be resolved is skipped
// (logged at DEBUG), matching the previous BuildVariantStates behavior.
// ObserveAccelerators controls whether an unconstrained variant's accelerator is
// resolved by reading the NODES its pods run on.
//
// Nodes are cluster-scoped, so that read is the only thing in the normal path that
// needs cluster-scoped RBAC. It is worth having exactly when something CHARGES a
// variant to an accelerator pool — a physical (gpu-inventory) limiter. With no such
// limiter, an unresolved accelerator is permissive: FitsGPUBudget asks whether ANY
// pool has room, so the answer does not change, and the only cost is that
// accelerator-keyed metrics are withheld for that variant.
//
// So a deployment with no physical limiter should not ask for node permission it
// cannot use, and an install that lacks it is not broken — it is unattributed.
type ObserveAccelerators bool

const (
	// FromNodes reads the pods' nodes to resolve an unconstrained accelerator.
	FromNodes ObserveAccelerators = true
	// DeclaredOnly uses only what the workload declares, and leaves the rest
	// unresolved rather than touching a cluster-scoped object.
	DeclaredOnly ObserveAccelerators = false
)

func Discover(
	ctx context.Context,
	vas []llmdvariant.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
	observe ObserveAccelerators,
) []domain.VariantMetadata {
	logger := ctrl.LoggerFrom(ctx)
	metas := make([]domain.VariantMetadata, 0, len(vas))

	for i := range vas {
		va := &vas[i]

		scaleTarget, ok := resolveScaleTarget(ctx, va, scaleTargets, k8sClient)
		if !ok {
			continue
		}

		currentReplicas := int(scaleTarget.GetStatusReplicas())
		if currentReplicas == 0 && scaleTarget.GetReplicas() != nil {
			currentReplicas = int(*scaleTarget.GetReplicas())
		}

		readyReplicas := int(scaleTarget.GetStatusReadyReplicas())
		pendingReplicas := currentReplicas - readyReplicas
		if pendingReplicas < 0 {
			// readyReplicas exceeding currentReplicas is an unexpected state;
			// surface it and clamp so downstream sizing is not skewed.
			logger.Info("Unexpected state: readyReplicas exceeds currentReplicas, clamping pendingReplicas to 0",
				"variant", va.Name, "currentReplicas", currentReplicas, "readyReplicas", readyReplicas)
			pendingReplicas = 0
		}

		desiredReplicas := 0
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
			desiredReplicas = int(*va.Status.DesiredOptimizedAlloc.NumReplicas)
		}

		var minReplicas *int
		if va.Spec.MinReplicas != nil {
			v := int(*va.Spec.MinReplicas)
			minReplicas = &v
		}
		var maxReplicas *int
		if va.Spec.MaxReplicas > 0 {
			v := int(va.Spec.MaxReplicas)
			maxReplicas = &v
		}

		pendingAges, stuckReplicas := pendingAgeSeconds(ctx, k8sClient, va.Namespace, scaleTarget, time.Now())
		heldReplicas, heldKnown := heldReplicaCount(ctx, k8sClient, va.Namespace, scaleTarget)
		metas = append(metas, domain.VariantMetadata{
			VariantName:     va.Name,
			ModelID:         va.Spec.ModelID,
			Namespace:       va.Namespace,
			Role:            RoleFromScaleTarget(scaleTarget),
			Cost:            costFromVA(ctx, va),
			AcceleratorName: resolveAccelerator(ctx, va, scaleTarget, k8sClient, readyReplicas, observe),
			Engine:          inferenceengine.Detect(scaleTarget).String(),
			GPUsPerReplica:  scaleTarget.GetTotalGPUsPerReplica(),
			CurrentReplicas: currentReplicas,
			DesiredReplicas: desiredReplicas,
			ReadyReplicas:   readyReplicas,
			PendingReplicas: pendingReplicas,
			PendingAges:     pendingAges,
			StuckReplicas:   stuckReplicas,
			HeldReplicas:    heldReplicas,
			HeldKnown:       heldKnown,
			PodGPUs:         scaletarget.PodGPUs(scaleTarget),
			MinReplicas:     minReplicas,
			MaxReplicas:     maxReplicas,
		})
	}

	return metas
}

// resolveScaleTarget returns the scale target for va, preferring the pre-populated
// map and falling back to a live fetch. Returns ok=false (VA skipped) when no
// scale target can be resolved.
// resolveAccelerator returns the accelerator this variant runs on, preferring
// what the workload CONSTRAINS and falling back to what its pods are OBSERVED to
// be running on.
//
// Both sources are sound, and in that order. A GPU product key in the
// nodeSelector or nodeAffinity binds the scheduler, so it answers even for a
// variant parked at zero. Observation answers only once pods exist, but when it
// does it is the ground truth: it is where the scheduler actually put them.
//
// There is no third source. An inference.optimization/acceleratorName label used
// to fill this gap and was removed as unsound — a workload that constrains
// nothing can be scheduled onto any GPU node, so the label asserted a type
// nothing enforced, and a wrong one silently billed the variant's GPUs to another
// accelerator's pool. Observation replaces it with the same information measured
// rather than declared.
//
// Costs nothing in the resolved case, which is the normal one: it is only reached
// when the workload constrains no accelerator AND has running pods, and both
// reads are served from the informer cache the collector already populates.
func resolveAccelerator(
	ctx context.Context,
	va *llmdvariant.VariantAutoscaling,
	scaleTarget scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
	readyReplicas int,
	observe ObserveAccelerators,
) string {
	declared := accelerator.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
	if constants.IsAcceleratorResolved(declared) || readyReplicas <= 0 || k8sClient == nil {
		return declared
	}
	if !observe {
		// No physical limiter is charging this variant to a pool, so the node read
		// would buy an attribution nobody consumes.
		return declared
	}

	observed, ok := observeAcceleratorFromNodes(ctx, k8sClient, va.Namespace, scaleTarget)
	if !ok {
		return declared
	}
	ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info(
		"Resolved a variant's accelerator from the nodes its pods are running on; "+
			"the workload constrains none itself",
		"variant", va.Name, "namespace", va.Namespace, "accelerator", observed)
	return observed
}

// observeAcceleratorFromNodes reports the accelerator the variant's pods are
// actually scheduled onto, and whether it could be determined.
//
// Requires agreement. An unconstrained workload may legitimately have pods spread
// across different accelerators, and there is no single right answer then — a
// choice would charge one pool for GPUs held on another, which is the failure the
// removed label produced. Disagreement therefore stays unresolved and the GPUs
// stay visibly unattributed (wva_unattributed_gpus).
func observeAcceleratorFromNodes(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	scaleTarget scaletarget.ScaleTargetAccessor,
) (string, bool) {
	podTemplate := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplate == nil || len(podTemplate.Labels) == 0 {
		return "", false
	}

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels(podTemplate.Labels),
	); err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info(
			"Could not list a variant's pods to observe its accelerator",
			"namespace", namespace, "error", err.Error())
		return "", false
	}

	productKeys := accelerator.GetProductKeys()
	found := ""
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
			continue
		}
		var node corev1.Node
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, &node); err != nil {
			continue
		}
		for _, key := range productKeys {
			product, ok := node.Labels[key]
			if !ok || product == "" {
				continue
			}
			if found != "" && found != product {
				return "", false // spread across accelerators; no single answer
			}
			found = product
			break
		}
	}
	return found, found != ""
}

// pendingAgeSeconds is how long each of a variant's STARTING replicas has been
// alive: the Pods that exist and are not Ready, aged from their own
// CreationTimestamp.
//
// The demand floor credits a starting replica with the part of the drain window
// it will be Ready for, which needs its age. PendingReplicas is only a count,
// and a starting Pod reports no metrics, so this is the one place the ages can
// be had -- discovery already lists a variant's Pods by the same labels for the
// accelerator observation beside it.
//
// Returns the ages of the Pods that ARE starting, and how many are not Ready and
// not starting either -- terminal, backing off, or unschedulable. The second is
// what anticipated supply subtracts, and it is deliberately the thing this
// listing can PROVE: an unreadable, stale or skipped listing reports zero stuck,
// which leaves every consumer on the figures it used before.
//
// That asymmetry is the whole design. Being wrong about a stuck Pod costs one
// replica of under-counting; being wrong about a starting Pod told the optimizer
// a fleet was on its way when it was not, and ordered it twice.
//
// Pods being deleted are excluded: their capacity is going away, not arriving.
func pendingAgeSeconds(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	scaleTarget scaletarget.ScaleTargetAccessor,
	now time.Time,
) ([]float64, int) {
	// Both are optional on this path in a way observeAcceleratorFromNodes never
	// had to consider: that one is called behind a config check, this one runs
	// on every variant of every cycle, so a caller without a client -- which the
	// discovery suite is -- must get nil rather than a panic.
	if k8sClient == nil || scaleTarget == nil {
		return nil, 0
	}
	// One replica is not one Pod on a LeaderWorkerSet: ReplicaCount and
	// PendingReplicas are in SCALE-TARGET units (groups), while this lists Pods.
	// A group of four starting Pods would contribute four ages for one pending
	// replica and credit the drain four times over -- and when LeaderTemplate is
	// nil, GetLeaderPodTemplateSpec returns the WORKER template, whose labels
	// match every Pod in the group. Nothing here identifies which group a Pod
	// belongs to, so rather than guess, a multi-Pod replica reports no ages and
	// the floor falls back to its count-based credit, which is already in
	// replica units.
	if scaleTarget.GetGroupSize() > 1 {
		return nil, 0
	}
	podTemplate := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplate == nil || len(podTemplate.Labels) == 0 {
		return nil, 0
	}
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels(podTemplate.Labels),
	); err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info(
			"Could not list a variant's pods to age its starting replicas",
			"namespace", namespace, "error", err.Error())
		return nil, 0
	}
	var ages []float64
	var stuck int
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || podReadyNow(pod) {
			continue
		}
		// "Not Ready" is not "starting". A Pod that is stopped rather than
		// late is counted as STUCK and subtracted from the replicas anticipated
		// to arrive; it contributes no age, because it is not going to serve.
		if !podStarting(pod) {
			stuck++
			continue
		}
		if pod.CreationTimestamp.IsZero() {
			continue
		}
		age := now.Sub(pod.CreationTimestamp.Time).Seconds()
		if age < 0 {
			// Clock skew between the API server and this process. Unknown
			// rather than zero: zero would claim the replica has just been
			// ordered and credit it with the whole window.
			continue
		}
		ages = append(ages, age)
	}
	return ages, stuck
}

// podStarting reports whether a Pod that is not yet Ready is actually on its way
// to being Ready, rather than stopped.
//
// Pending and Running are the two phases a starting replica passes through;
// Succeeded and Failed are terminal. Within those, a Pod is refused when it is
// not going to make progress on its own: unschedulable or scheduling-gated on
// the PodScheduled condition, or any container -- INIT containers included --
// waiting on a backoff, an image that will not pull or a process that will not
// stay up, however long it has existed.
func podStarting(p *corev1.Pod) bool {
	switch p.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return false
	case corev1.PodPending, corev1.PodRunning:
	default:
		return false
	}
	// Unschedulable has no container status to inspect -- the Pod has not been
	// placed, so it has no containers yet. It is phase Pending with an empty
	// ContainerStatuses, which every check below would wave through. This is the
	// GPU-quota case, and it is the one that matters most here: a fleet blocked
	// on quota would otherwise report capacity arriving for as long as the quota
	// stayed spent.
	//
	// SchedulingGated is the same shape and the same story told by Kueue: a
	// gated Pod is Pending with no containers for as long as the gate holds,
	// which on a quota-managed fleet is exactly as long as the quota is spent.
	// Every OTHER PodScheduled=False reason (SchedulerError) is transient and
	// stays "starting".
	for _, c := range p.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse {
			continue
		}
		if c.Reason == corev1.PodReasonUnschedulable ||
			c.Reason == corev1.PodReasonSchedulingGated {
			return false
		}
	}
	// Init containers as well as the main ones. An init container in
	// ImagePullBackOff or CrashLoopBackOff leaves the Pod phase Pending with
	// ContainerStatuses EMPTY and the failure only in InitContainerStatuses --
	// the same blind spot Unschedulable had, and reachable on the engine Pods
	// this repo ships: five benchmark scenarios give prefill and decode
	// templates an init container that fetches weights.
	for _, group := range [][]corev1.ContainerStatus{
		p.Status.InitContainerStatuses, p.Status.ContainerStatuses,
	} {
		for _, cs := range group {
			if cs.State.Waiting == nil {
				continue
			}
			switch cs.State.Waiting.Reason {
			case "ImagePullBackOff", "ErrImagePull", "CrashLoopBackOff",
				"CreateContainerError", "CreateContainerConfigError", "InvalidImageName":
				return false
			}
		}
	}
	return true
}

// podReadyNow reports the Pod's Ready condition, which is what decides whether
// anything routes to it -- the phase alone stays Running while probes fail.
func podReadyNow(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func resolveScaleTarget(
	ctx context.Context,
	va *llmdvariant.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
) (scaletarget.ScaleTargetAccessor, bool) {
	if scaleTargets != nil {
		if st, found := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]; found {
			return st, true
		}
	}

	st, err := scaletarget.FetchScaleTarget(ctx, k8sClient, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
	if err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not get scale target for VA, skipping",
			"variant", va.Name, "error", err)
		return nil, false
	}
	return st, true
}

// costFromVA parses the variant's per-replica cost from its spec, falling back to
// DefaultVariantCost when unset or unparseable. Mirrors the previous engine-side
// cost resolution.
func costFromVA(ctx context.Context, va *llmdvariant.VariantAutoscaling) float64 {
	cost := domain.DefaultVariantCost
	if va.Spec.VariantCost != "" {
		if parsed, err := strconv.ParseFloat(va.Spec.VariantCost, 64); err == nil {
			cost = parsed
		} else {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Failed to parse variant cost, using default",
				"variant", va.Name, "variantCost", va.Spec.VariantCost, "default", cost, "error", err)
		}
	}
	return cost
}

// RoleFromScaleTarget resolves a scale target's P/D role from the llm-d role
// label, returning "prefill", "decode", or "both".
//
// The label is the source of truth. Engine arguments are deliberately NOT
// consulted: the flag differs per engine (SGLang has --disaggregation-mode, vLLM
// configures disaggregation through --kv-transfer-config), so parsing them would
// be an engine-by-engine guess at something llm-d already states declaratively.
// "both" is the default when no role is declared, which is correct for a
// non-disaggregated workload — it serves complete requests on its own.
func RoleFromScaleTarget(scaleTarget scaletarget.ScaleTargetAccessor) string {
	if scaleTarget == nil {
		return domain.RoleBoth
	}

	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil || podTemplateSpec.Labels == nil {
		return domain.RoleBoth
	}
	switch podTemplateSpec.Labels[RoleLabel] {
	case domain.RolePrefill:
		return domain.RolePrefill
	case domain.RoleDecode:
		return domain.RoleDecode
	default:
		return domain.RoleBoth
	}
}
