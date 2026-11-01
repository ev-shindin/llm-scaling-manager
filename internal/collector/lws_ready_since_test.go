package collector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
)

// Where an LWS group's Ready time comes from (analyzer-learner.md 4.3 and
// section 7, PR 2; the group restart #361's workload patch issues: the
// leader and its workers recreated, their names kept, their Ready times
// apart). The collector reports one row per group, the leader's: a worker's
// instance is dropped before its row is built (isLWSWorker). The row's
// Ready time is the latest over the leader and its live, Ready workers
// (groupReadyTimes), matched by the LWS set-name and group-index labels, so
// k2 ages the group from the later of it and the controller's first sight
// of it.
//
// The case that needs the workers: a restart that recreates only a worker
// and leaves the leader Ready (an LWS whose restartPolicy is None). The
// leader's Ready time does not move; the worker's does, and the group must
// wait on it. Under the default restartPolicy, RecreateGroupOnPodRestart,
// any worker's restart deletes the leader (lws pod_controller.go,
// handleRestartPolicy), and the recreated leader has a new Ready time too.
//
// A worker missing or not Ready adds nothing: it never makes the group's
// time zero (unknown), and the worker's own Ready time moves it once the
// worker is Ready. A worker that drops while it holds the latest time moves
// the group back to the next latest Pod's, which the settle tracker also
// reads as a restart: the group waits from the drop and again on return.
func TestLWSGroupReadyTimeIsTheLatestOfItsPods(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	labels := map[string]map[string]string{
		"lws-0":   {lwsv1.SetNameLabelKey: "lws", lwsv1.GroupIndexLabelKey: "0", lwsv1.WorkerIndexLabelKey: "0"},
		"lws-0-1": {lwsv1.SetNameLabelKey: "lws", lwsv1.GroupIndexLabelKey: "0", lwsv1.WorkerIndexLabelKey: "1"},
		"lws-0-2": {lwsv1.SetNameLabelKey: "lws", lwsv1.GroupIndexLabelKey: "0", lwsv1.WorkerIndexLabelKey: "2"},
		"lws-1":   {lwsv1.SetNameLabelKey: "lws", lwsv1.GroupIndexLabelKey: "1", lwsv1.WorkerIndexLabelKey: "0"},
		"lws-1-1": {lwsv1.SetNameLabelKey: "lws", lwsv1.GroupIndexLabelKey: "1", lwsv1.WorkerIndexLabelKey: "1"},
		"other-0-1": {lwsv1.SetNameLabelKey: "other", lwsv1.GroupIndexLabelKey: "0",
			lwsv1.WorkerIndexLabelKey: "1"},
	}
	// pod is the named Pod Ready at readyAt, or Running and not Ready when
	// readyAt is zero. A not-Ready Pod's condition changed after every Ready
	// time here (a probe failing now), so counting it would show.
	pod := func(name string, readyAt time.Time) *corev1.Pod {
		cond := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(readyAt)}
		if readyAt.IsZero() {
			cond = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(t0.Add(3 * time.Hour))}
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels[name]},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{cond}},
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
	// rows attributes an instance for every listed Pod and returns the rows
	// it built, by Pod: only leaders build one.
	rows := func(pods ...*corev1.Pod) map[string]time.Time {
		t.Helper()
		objs := make([]client.Object, len(pods))
		for i, p := range pods {
			objs[i] = p
		}
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		c := NewReplicaMetricsCollector(nil, nil, reader, nil, loc)
		out := map[string]time.Time{}
		for _, p := range pods {
			data := &podMetricData{podName: p.Name, vaName: "lws", hasKv: true, kvUsage: 0.5}
			if m, _, ok := c.attributeInstance(ctx, "m", "ns", p.Name+":8000", data, t0.Add(time.Hour), nil,
				map[string]map[string]int{}); ok {
				out[m.PodName] = m.ReadySince
			}
		}
		return out
	}

	// The group's Pods became Ready a minute apart: one row, the leader's,
	// with the latest of the three.
	before := rows(pod("lws-0", t0), pod("lws-0-1", t0.Add(-time.Minute)), pod("lws-0-2", t0.Add(time.Minute)))
	if len(before) != 1 {
		t.Fatalf("rows %v: want one, the leader's (a worker's instance is no row)", before)
	}
	if got, want := before["lws-0"], t0.Add(time.Minute); !got.Equal(want) {
		t.Errorf("the group's Ready time is %v, want its latest Pod's %v", got, want)
	}

	// The patch recreates the group, names kept: the workers Ready 10 s and
	// 40 s after the leader's new Ready time.
	restarted := t0.Add(30 * time.Minute)
	after := rows(pod("lws-0", restarted), pod("lws-0-1", restarted.Add(10*time.Second)),
		pod("lws-0-2", restarted.Add(40*time.Second)))
	if got, want := after["lws-0"], restarted.Add(40*time.Second); !got.Equal(want) {
		t.Errorf("the group's Ready time after the restart is %v, want its last worker's %v", got, want)
	}

	// restartPolicy None: a worker alone recreated, the leader still Ready
	// from t0. The group's Ready time moves to the worker's.
	workerOnly := rows(pod("lws-0", t0), pod("lws-0-1", restarted), pod("lws-0-2", t0.Add(time.Minute)))
	if got := workerOnly["lws-0"]; !got.Equal(restarted) {
		t.Errorf("a worker-only restart left the group's Ready time at %v; want the worker's %v", got, restarted)
	}

	// The recreated worker not Ready yet, or not created yet: the group reads
	// the latest Ready time among the rest, not zero.
	notReady := rows(pod("lws-0", t0), pod("lws-0-1", time.Time{}), pod("lws-0-2", t0.Add(time.Minute)))
	if got, want := notReady["lws-0"], t0.Add(time.Minute); !got.Equal(want) {
		t.Errorf("with a worker not Ready the group's Ready time is %v, want %v", got, want)
	}
	missing := rows(pod("lws-0", t0), pod("lws-0-2", t0.Add(time.Minute)))
	if got, want := missing["lws-0"], t0.Add(time.Minute); !got.Equal(want) {
		t.Errorf("with a worker missing the group's Ready time is %v, want %v", got, want)
	}

	// The worker holding the latest time drops (a probe fails): the group
	// moves back to the next latest Pod's, 10 s after the leader.
	dropped := rows(pod("lws-0", restarted), pod("lws-0-1", restarted.Add(10*time.Second)),
		pod("lws-0-2", time.Time{}))
	if got, want := dropped["lws-0"], restarted.Add(10*time.Second); !got.Equal(want) {
		t.Errorf("with its latest worker dropped the group's Ready time is %v, want the next latest %v", got, want)
	}

	// A leader not Ready is a group not Ready: zero, whatever its workers say.
	leaderDown := rows(pod("lws-0", time.Time{}), pod("lws-0-1", restarted))
	if got := leaderDown["lws-0"]; !got.IsZero() {
		t.Errorf("a not-Ready leader's group reads Ready since %v; want zero", got)
	}

	// Another group's worker, or another set's with the same group index,
	// does not move this group; a worker being deleted is leaving the group.
	leaving := pod("lws-0-1", restarted.Add(time.Hour))
	leaving.Finalizers = []string{"test/hold"}
	leaving.DeletionTimestamp = &metav1.Time{Time: restarted}
	others := rows(pod("lws-0", t0), leaving, pod("lws-1", t0), pod("lws-1-1", restarted),
		pod("other-0-1", restarted))
	if got := others["lws-0"]; !got.Equal(t0) {
		t.Errorf("group 0's Ready time is %v, moved by a Pod outside it or leaving it; want %v", got, t0)
	}
	if got := others["lws-1"]; !got.Equal(restarted) {
		t.Errorf("group 1's Ready time is %v, want its worker's %v", got, restarted)
	}
}
