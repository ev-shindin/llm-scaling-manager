package collector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
)

// Where an LWS group's Ready time comes from (analyzer-learner.md 4.3 and
// section 7, PR 2; the group restart #361's workload patch issues: the
// leader and its workers recreated, their names kept, their Ready times
// apart). The collector reports one row per group, the leader's: a worker's
// instance is dropped before its row is built (isLWSWorker), and the
// leader's row carries the leader Pod's own Ready condition
// (podReadySince). So a group's Ready time is its leader's, whatever its
// workers' are, and k2 ages the group from the later of it and the
// controller's first sight of it.
//
// What that leaves out: a restart that recreates only a worker and leaves
// the leader Ready (an LWS whose restartPolicy is None) changes no Ready
// time on the row, and no settle starts. Under the default
// restartPolicy, RecreateGroupOnPodRestart, any worker's restart deletes
// the leader (lws pod_controller.go, handleRestartPolicy), and the
// recreated leader has a new Ready time.
func TestLWSGroupReadyTimeIsTheLeaders(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	labels := map[string]map[string]string{
		"lws-0":   {lwsv1.SetNameLabelKey: "lws", lwsv1.GroupIndexLabelKey: "0", lwsv1.WorkerIndexLabelKey: "0"},
		"lws-0-1": {lwsv1.SetNameLabelKey: "lws", lwsv1.GroupIndexLabelKey: "0", lwsv1.WorkerIndexLabelKey: "1"},
		"lws-0-2": {lwsv1.SetNameLabelKey: "lws", lwsv1.GroupIndexLabelKey: "0", lwsv1.WorkerIndexLabelKey: "2"},
	}
	pod := func(name string, readyAt time.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels[name]},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(readyAt)}},
			},
		}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	loc := &mockLocator{
		locateFunc: func(context.Context, string, string) (*locator.ManagedScaler, error) {
			return &locator.ManagedScaler{Namespace: "ns", Name: "lws"}, nil
		},
		getPodLabelsFunc: func(_ context.Context, _, name string) map[string]string { return labels[name] },
	}
	// rows attributes the group's three instances against a listing of its
	// Pods Ready at the given times, and returns the rows it built.
	rows := func(leader, worker1, worker2 time.Time) map[string]time.Time {
		t.Helper()
		reader := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(pod("lws-0", leader), pod("lws-0-1", worker1), pod("lws-0-2", worker2)).Build()
		c := NewReplicaMetricsCollector(nil, nil, reader, nil, loc)
		out := map[string]time.Time{}
		for _, name := range []string{"lws-0", "lws-0-1", "lws-0-2"} {
			data := &podMetricData{podName: name, vaName: "lws", hasKv: true, kvUsage: 0.5}
			if m, _, ok := c.attributeInstance(ctx, "m", "ns", name+":8000", data, t0.Add(time.Hour), nil,
				map[string]map[string]int{}); ok {
				out[m.PodName] = m.ReadySince
			}
		}
		return out
	}

	// Before the patch: the group's Pods became Ready a minute apart.
	before := rows(t0, t0.Add(-time.Minute), t0.Add(time.Minute))
	if len(before) != 1 {
		t.Fatalf("rows %v: want one, the leader's (a worker's instance is no row)", before)
	}
	if got := before["lws-0"]; !got.Equal(t0) {
		t.Errorf("the group's Ready time before the restart is %v, want the leader's %v", got, t0)
	}

	// The patch recreates the group, names kept: the workers Ready 10 s and
	// 40 s after the leader's new Ready time.
	restarted := t0.Add(30 * time.Minute)
	after := rows(restarted, restarted.Add(10*time.Second), restarted.Add(40*time.Second))
	if got := after["lws-0"]; !got.Equal(restarted) {
		t.Errorf("the group's Ready time after the restart is %v, want the recreated leader's %v, not a worker's", got, restarted)
	}

	// A worker alone recreated, the leader still Ready: the row's Ready
	// time does not move.
	workerOnly := rows(t0, t0.Add(30*time.Minute), t0.Add(time.Minute))
	if got := workerOnly["lws-0"]; !got.Equal(t0) {
		t.Errorf("a worker's restart moved the group's Ready time to %v; want the leader's %v", got, t0)
	}
}
