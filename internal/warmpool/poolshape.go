package warmpool

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// PoolShapes reads pool Deployments for Reconciler.Shape. Read only: it
// configures nothing. A Deployment that is not found has no shape; one that
// cannot be read whole -- the Deployment, its selector or its Pods -- has none
// either, with the error logged: a half-read shape would count a scheduled Pod
// as unheld.
type PoolShapes struct {
	Client client.Client
}

// Read returns what a pool Deployment states about its Pods: the GPUs its
// template asks per Pod, the accelerator its template requires (by a
// vendor's product label, in its nodeSelector or required nodeAffinity), and
// the GPUs its scheduled Pods request.
func (p *PoolShapes) Read(ctx context.Context, namespace, deployment string) (PoolShape, bool) {
	logger := log.FromContext(ctx).WithValues("namespace", namespace, "deployment", deployment)
	var d appsv1.Deployment
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deployment}, &d); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.V(logging.DEBUG).Info("could not read a warm pool Deployment's shape", "error", err.Error())
		}
		return PoolShape{}, false
	}
	// The accelerator as the template requires it: by nodeSelector, or by the
	// required nodeAffinity term llm-d modelservice writes instead of one.
	shape := PoolShape{
		PodGPUs:     resources.PodGPURequests(&corev1.Pod{Spec: d.Spec.Template.Spec}),
		Accelerator: pool.AcceleratorRequiredBy(&d.Spec.Template.Spec),
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		logger.V(logging.DEBUG).Info("a warm pool Deployment's selector does not parse", "error", err.Error())
		return PoolShape{}, false
	}
	var pods corev1.PodList
	if err := p.Client.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		logger.V(logging.DEBUG).Info("could not list a warm pool Deployment's Pods", "error", err.Error())
		return PoolShape{}, false
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
