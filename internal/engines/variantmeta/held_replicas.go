package variantmeta

import (
	"context"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
//
// filled counts the replicas every pod of which holds GPUs: held for a
// Deployment, and for a LeaderWorkerSet the groups with all of their group
// size scheduled. A receiver's fill is judged by it, the safe direction there.
func heldReplicaCount(ctx context.Context, k8sClient client.Client, namespace string,
	scaleTarget scaletarget.ScaleTargetAccessor) (held, filled int, known bool) {
	if k8sClient == nil || scaleTarget == nil {
		return 0, 0, false
	}
	group := scaletarget.IsLeaderWorkerSet(scaleTarget)
	pods, err := ListVariantPods(ctx, k8sClient, namespace, scaleTarget)
	if err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not list a variant's pods to count the GPUs it holds",
			"namespace", namespace, "error", err.Error())
		return 0, 0, false
	}
	groups := map[string]int{}
	for i := range pods {
		p := &pods[i]
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
		groups[idx]++
	}
	if !group {
		return held, held, true
	}
	size := max(int(scaleTarget.GetGroupSize()), 1)
	for _, n := range groups {
		if n >= size {
			filled++
		}
	}
	return len(groups), filled, true
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

// ListVariantPods lists the pods of one scale target. A LeaderWorkerSet's pods
// carry its name in the set-name label. A Deployment's are matched by its pod
// template labels AND by ownership -- controlled by one of its ReplicaSets,
// which are named <deployment>-<pod-template-hash> -- because two variants of
// one model routinely share template labels, and a label match alone would
// count, or mark, the other variant's pods.
func ListVariantPods(ctx context.Context, c client.Client, namespace string,
	acc scaletarget.ScaleTargetAccessor) ([]corev1.Pod, error) {
	if scaletarget.IsLeaderWorkerSet(acc) {
		var list corev1.PodList
		if err := c.List(ctx, &list, client.InNamespace(namespace),
			client.MatchingLabels{lwsv1.SetNameLabelKey: acc.GetName()}); err != nil {
			return nil, err
		}
		return list.Items, nil
	}
	tpl := acc.GetLeaderPodTemplateSpec()
	if tpl == nil || len(tpl.Labels) == 0 {
		return nil, fmt.Errorf("scale target %s has no pod template labels", acc.GetName())
	}
	var list corev1.PodList
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels(tpl.Labels)); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list.Items, func(p corev1.Pod) bool {
		return !ownedByDeployment(&p, acc.GetName())
	}), nil
}

// ListDeploymentPods lists every pod of a Deployment, of every ReplicaSet
// generation: matched by its spec.selector, which is immutable, and by
// ownership. ListVariantPods matches the CURRENT pod template's labels, so a
// rollout that changed or added one leaves the old ReplicaSet's pods out --
// the pods a mid-rollout check exists to see.
func ListDeploymentPods(ctx context.Context, c client.Client, namespace, name string,
	selector *metav1.LabelSelector) ([]corev1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s selector: %w", namespace, name, err)
	}
	var list corev1.PodList
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list.Items, func(p corev1.Pod) bool {
		return !ownedByDeployment(&p, name)
	}), nil
}

// ownedByDeployment reports whether a pod is controlled by a ReplicaSet of the
// named Deployment.
func ownedByDeployment(p *corev1.Pod, deployment string) bool {
	hash := p.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	if hash == "" {
		return false
	}
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "ReplicaSet" && o.Name == deployment+"-"+hash {
			return true
		}
	}
	return false
}
