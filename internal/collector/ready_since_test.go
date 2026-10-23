package collector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPodStatesCarriesReadySince checks that the listing records when a Ready
// Pod became Ready, and records nothing for one that is not Ready: the
// load-balance diagnostic reads zero as "unknown", never as "long ago".
func TestPodStatesCarriesReadySince(t *testing.T) {
	readyAt := time.Date(2026, 10, 23, 11, 58, 0, 0, time.UTC)
	pod := func(name string, status corev1.ConditionStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{
					Type:               corev1.PodReady,
					Status:             status,
					LastTransitionTime: metav1.NewTime(readyAt),
				}},
			},
		}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pod("ready", corev1.ConditionTrue), pod("not-ready", corev1.ConditionFalse)).Build()

	got := podStates(context.Background(), reader, "ns")
	if !got.listed {
		t.Fatal("listing failed")
	}
	if s := got.byName["ready"].readySince; !s.Equal(readyAt) {
		t.Errorf("ready pod readySince = %v, want %v", s, readyAt)
	}
	if s := got.byName["not-ready"].readySince; !s.IsZero() {
		t.Errorf("not-Ready pod readySince = %v, want zero", s)
	}
}
