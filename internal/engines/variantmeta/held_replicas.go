package variantmeta

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// heldReplicaCount counts a variant's replicas that hold GPUs: pods that are
// scheduled and have not finished, terminating ones included. A pod being
// deleted keeps its GPUs through its drain hook and grace period, while
// status.replicas stops counting it the moment deletion starts -- so a release
// judged on status.replicas would hand a donor's GPUs to a receiver before they
// are free (docs/proposals/utilization-share-optimizer.md, §6.3).
//
// The count is in scale-target units. On a LeaderWorkerSet a replica is a
// group, so pods are matched by the set's name label and counted by distinct
// group index: a group that still has any pod scheduled still holds GPUs. That
// over-counts a half-drained group, which is the safe direction -- it delays a
// release, never invents one.
//
// known is false when the pods cannot be listed; the caller then falls back to
// CurrentReplicas.
func heldReplicaCount(ctx context.Context, k8sClient client.Client, namespace string,
	scaleTarget scaletarget.ScaleTargetAccessor) (held int, known bool) {
	if k8sClient == nil || scaleTarget == nil {
		return 0, false
	}
	var selector client.MatchingLabels
	group := scaleTarget.GetGroupSize() > 1
	if group {
		selector = client.MatchingLabels{lwsv1.SetNameLabelKey: scaleTarget.GetName()}
	} else {
		tpl := scaleTarget.GetLeaderPodTemplateSpec()
		if tpl == nil || len(tpl.Labels) == 0 {
			return 0, false
		}
		selector = client.MatchingLabels(tpl.Labels)
	}
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), selector); err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not list a variant's pods to count the GPUs it holds",
			"namespace", namespace, "error", err.Error())
		return 0, false
	}
	groups := map[string]struct{}{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !holdsGPUs(p) {
			continue
		}
		if !group {
			held++
			continue
		}
		idx, ok := p.Labels[lwsv1.GroupIndexLabelKey]
		if !ok {
			continue
		}
		groups[idx] = struct{}{}
	}
	if group {
		held = len(groups)
	}
	return held, true
}

// holdsGPUs reports whether a pod holds the GPUs it requested: it is bound to a
// node and its containers have not finished. Deletion does not change that.
func holdsGPUs(p *corev1.Pod) bool {
	if p.Spec.NodeName == "" {
		return false
	}
	switch p.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return false
	}
	return true
}
