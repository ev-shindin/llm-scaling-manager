package gpunodes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

func TestDiscoverUsageWithNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	node := func(name string, gpus string, rack string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
				"amd.com/gpu.product-name": "AMD-MI300X-192G", "rack": rack}},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{"amd.com/gpu": resource.MustParse(gpus)}},
		}
	}
	pod := func(name, ns, nodeName, gpus string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{
				Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"amd.com/gpu": resource.MustParse(gpus)}},
			}}},
			Status: corev1.PodStatus{Phase: phase},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		node("n1", "8", "r1"), node("n2", "8", "r2"),
		pod("a", "x", "n1", "2", corev1.PodRunning),
		pod("b", "y", "n1", "3", corev1.PodRunning),
		pod("c", "y", "n2", "1", corev1.PodRunning),
		pod("done", "y", "n2", "4", corev1.PodSucceeded),
	).Build()

	byType, byNamespace, nodes, err := NewK8sWithGpuOperator(c).DiscoverUsageWithNodes(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 6, byType["AMD-MI300X-192G"])
	assert.Equal(t, 2, byNamespace["x"]["AMD-MI300X-192G"])
	assert.Equal(t, 4, byNamespace["y"]["AMD-MI300X-192G"])
	require.Len(t, nodes, 2)
	assert.Equal(t, decision.NodeGPU{Accelerator: "AMD-MI300X-192G", Capacity: 8, Used: 5,
		Labels: map[string]string{"amd.com/gpu.product-name": "AMD-MI300X-192G", "rack": "r1"}}, nodes["n1"])
	assert.Equal(t, 1, nodes["n2"].Used, "a finished pod holds no GPU")

	// The per-node sum is the per-type sum: one walk, both views.
	sum := 0
	for _, n := range nodes {
		sum += n.Used
	}
	assert.Equal(t, byType["AMD-MI300X-192G"], sum)
}

// A cordoned node is reported as such, so a placement does not count on its
// free GPUs.
func TestDiscoverUsageWithNodesMarksCordonedNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	mk := func(name string, cordoned bool) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"amd.com/gpu.product-name": "AMD-MI300X-192G"}},
			Spec:       corev1.NodeSpec{Unschedulable: cordoned},
			Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{"amd.com/gpu": resource.MustParse("8")}},
		}
	}
	tainted := mk("tainted", false)
	tainted.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}
	preferred := mk("preferred", false)
	preferred.Spec.Taints = []corev1.Taint{{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule}}
	notReady := mk("not-ready", false)
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	ready := mk("ready", false)
	ready.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(mk("open", false), mk("cordoned", true),
		tainted, preferred, notReady, ready).Build()
	_, _, nodes, err := NewK8sWithGpuOperator(c).DiscoverUsageWithNodes(context.Background())
	require.NoError(t, err)
	assert.False(t, nodes["open"].Unschedulable)
	assert.True(t, nodes["cordoned"].Unschedulable)
	assert.True(t, nodes["tainted"].Unschedulable, "a NoSchedule taint refuses new pods")
	assert.False(t, nodes["preferred"].Unschedulable, "PreferNoSchedule does not")
	assert.True(t, nodes["not-ready"].Unschedulable)
	assert.False(t, nodes["ready"].Unschedulable)
}
