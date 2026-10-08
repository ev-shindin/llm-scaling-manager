package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// A pod holds what the scheduler reserves for it: its regular containers'
// GPUs added up, or its largest init container's, whichever is more.
func TestPodGPURequests(t *testing.T) {
	ctr := func(gpus string) corev1.Container {
		return corev1.Container{Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse(gpus)}}}
	}
	for _, tc := range []struct {
		name       string
		init, main []corev1.Container
		want       int
	}{
		{"regular containers add up", nil, []corev1.Container{ctr("2"), ctr("3")}, 5},
		{"a larger init container wins", []corev1.Container{ctr("1"), ctr("8")}, []corev1.Container{ctr("2"), ctr("3")}, 8},
		{"init containers do not add up", []corev1.Container{ctr("3"), ctr("3")}, []corev1.Container{ctr("4")}, 4},
		{"no GPUs", nil, []corev1.Container{{}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &corev1.Pod{Spec: corev1.PodSpec{InitContainers: tc.init, Containers: tc.main}}
			if got := PodGPURequests(p); got != tc.want {
				t.Fatalf("PodGPURequests = %d, want %d", got, tc.want)
			}
		})
	}
}
