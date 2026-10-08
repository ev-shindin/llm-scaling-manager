package fixtures

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
)

// fillerImage holds GPUs and does nothing. Never docker.io.
const fillerImage = "registry.k8s.io/pause:3.9"

// FreeGPUsOnNode is a node's allocatable GPUs of resourceName less what the
// pods scheduled to it request, as the scheduler accounts them.
func FreeGPUsOnNode(ctx context.Context, k8sClient *kubernetes.Clientset, node *corev1.Node, resourceName string) (int, error) {
	alloc, ok := node.Status.Allocatable[corev1.ResourceName(resourceName)]
	if !ok {
		return 0, nil
	}
	pods, err := k8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node.Name})
	if err != nil {
		return 0, fmt.Errorf("list pods on %s: %w", node.Name, err)
	}
	used := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		used += resources.PodGPURequests(p)
	}
	return int(alloc.Value()) - used, nil
}

// FillGPUNodes takes every GPU still free on the nodes labelled key=product
// with a pause pod per node, pinned by node name, in namespace. It returns the
// pods' names and how many GPUs they took. The pods are not WVA's: no limiter
// or optimizer counts them as a model's, but the node picture does, which is
// what a spec needing full nodes wants.
func FillGPUNodes(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, key, product, resourceName string) ([]string, int, error) {
	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: key + "=" + product})
	if err != nil {
		return nil, 0, fmt.Errorf("list %s nodes: %w", product, err)
	}
	var names []string
	total := 0
	for i := range nodes.Items {
		n := &nodes.Items[i]
		free, err := FreeGPUsOnNode(ctx, k8sClient, n, resourceName)
		if err != nil {
			return names, total, err
		}
		if free <= 0 {
			continue
		}
		qty := *resource.NewQuantity(int64(free), resource.DecimalSI)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "gpu-filler-", Namespace: namespace,
				Labels: map[string]string{"app": "gpu-filler"}},
			Spec: corev1.PodSpec{
				NodeName: n.Name,
				// Tolerate the control-plane taint: a product labelled only there
				// is still a node the picture counts.
				Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				Containers: []corev1.Container{{Name: "pause", Image: fillerImage,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceName(resourceName): qty},
						Limits:   corev1.ResourceList{corev1.ResourceName(resourceName): qty},
					}}},
			},
		}
		created, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			return names, total, fmt.Errorf("create filler on %s: %w", n.Name, err)
		}
		names = append(names, created.Name)
		total += free
	}
	return names, total, nil
}
