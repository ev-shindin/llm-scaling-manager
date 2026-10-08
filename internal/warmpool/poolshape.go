package warmpool

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
)

// PoolShapes reads pool Deployments for Reconciler.Shape. Read only: it
// configures nothing, and a Deployment it cannot read simply has no shape.
type PoolShapes struct {
	Client client.Client
}

// Read returns what a pool Deployment states about its Pods: the GPUs its
// template asks per Pod, the accelerator its template pins by a vendor's
// product label in its nodeSelector, and the GPUs its scheduled Pods request.
func (p *PoolShapes) Read(ctx context.Context, namespace, deployment string) (PoolShape, bool) {
	var d appsv1.Deployment
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deployment}, &d); err != nil {
		return PoolShape{}, false
	}
	shape := PoolShape{PodGPUs: resources.PodGPURequests(&corev1.Pod{Spec: d.Spec.Template.Spec})}
	for _, res := range constants.VendorResources {
		for _, key := range append([]string{res.ProductLabel}, res.ProductLabelAliases...) {
			if v := d.Spec.Template.Spec.NodeSelector[key]; v != "" && shape.Accelerator == "" {
				shape.Accelerator = v
			}
		}
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return shape, true
	}
	var pods corev1.PodList
	if err := p.Client.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return shape, true
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		shape.ScheduledGPUs += resources.PodGPURequests(pod)
	}
	return shape, true
}
