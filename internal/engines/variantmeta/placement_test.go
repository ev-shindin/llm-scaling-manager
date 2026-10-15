package variantmeta

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// The unplaced count is narrow: only a pod the scheduler cannot place, or one
// Kueue holds at its gate for more than a cycle. A pod pulling its image or
// loading weights has a node and is placed.
func TestCountUnplacedIsNarrow(t *testing.T) {
	now := time.Unix(10000, 0)
	pod := func(name string, mutate func(*corev1.Pod)) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
			Status: corev1.PodStatus{Phase: corev1.PodPending}}
		mutate(&p)
		return p
	}
	unschedulable := func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Minute))}}
	}
	gated := func(age time.Duration) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.CreationTimestamp = metav1.NewTime(now.Add(-age))
			p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: kueue.AdmissionGate}}
		}
	}
	pulling := func(p *corev1.Pod) {
		p.Spec.NodeName = "n1"
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
	}

	n, since, pods, byPod := countUnplaced([]corev1.Pod{
		pod("a", unschedulable),
		pod("b", gated(5*time.Minute)),
		pod("c", gated(5*time.Second)),
		pod("d", pulling),
		pod("e", func(p *corev1.Pod) { p.Spec.NodeName = "n1" }),
	}, false, now)
	if n != 2 || len(pods) != 2 || pods[0] != "a" || pods[1] != "b" {
		t.Fatalf("unplaced = %d %v, want 2 [a b]: a fresh gate, a pulling image and a loading pod are not unplaced", n, pods)
	}
	if !since.Equal(now.Add(-5 * time.Minute)) {
		t.Fatalf("since = %v, want the newest unplaced pod (b, gated 5m), not the oldest (a, unschedulable 10m)", since)
	}
	if len(byPod) != 2 || !byPod["b"].Equal(now.Add(-5*time.Minute)) || byPod["a"].IsZero() {
		t.Fatalf("byPod = %v, want each unplaced pod with its own time", byPod)
	}
}

// A LeaderWorkerSet group is one replica, unplaced while any of its pods is.
func TestCountUnplacedCountsLWSGroups(t *testing.T) {
	now := time.Unix(10000, 0)
	mk := func(name, group string, placed bool) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{lwsv1.GroupIndexLabelKey: group}},
			Status: corev1.PodStatus{Phase: corev1.PodPending}}
		if placed {
			p.Spec.NodeName = "n"
		} else {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
		}
		return p
	}
	n, _, _, _ := countUnplaced([]corev1.Pod{mk("g0-a", "0", true), mk("g0-b", "0", true), mk("g1-a", "1", true), mk("g1-b", "1", false),
		mk("g2-a", "2", false), mk("g2-b", "2", false)}, true, now)
	if n != 2 {
		t.Fatalf("unplaced groups = %d, want 2: the hold lifts only when the whole group placed", n)
	}
}

// Review finding (whole-node): the placement timeout counted the pods of a
// LeaderWorkerSet as replicas, so two unplaced pods of one 2x8 group released
// two groups, one of them only a minute unplaced. Each group is one entry,
// timed from when its newest unplaced pod became so.
func TestCountUnplacedTimesLWSGroups(t *testing.T) {
	now := time.Unix(10000, 0)
	mk := func(name, group string, unplacedFor time.Duration) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{lwsv1.GroupIndexLabelKey: group}},
			Status: corev1.PodStatus{Phase: corev1.PodPending}}
		if unplacedFor == 0 {
			p.Spec.NodeName = "n"
			return p
		}
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, LastTransitionTime: metav1.NewTime(now.Add(-unplacedFor))}}
		return p
	}
	n, _, pods, at := countUnplaced([]corev1.Pod{
		mk("g0-a", "0", 0), mk("g0-b", "0", 0), // placed
		mk("g1-a", "1", 10*time.Minute), mk("g1-b", "1", 8*time.Minute), // past the timeout
		mk("g2-a", "2", time.Minute), mk("g2-b", "2", time.Minute), // fresh
		mk("g3-a", "3", 0), mk("g3-b", "3", 10*time.Minute), // leader placed, worker past the timeout
	}, true, now)
	if n != 3 || len(pods) != 5 {
		t.Fatalf("unplaced = %d groups, %d pods, want 3 groups of 5 pods", n, len(pods))
	}
	want := map[string]time.Time{"1": now.Add(-8 * time.Minute), "2": now.Add(-time.Minute), "3": now.Add(-10 * time.Minute)}
	if len(at) != len(want) {
		t.Fatalf("at = %v, want one entry per unplaced group: %v", at, want)
	}
	for g, w := range want {
		if !at[g].Equal(w) {
			t.Fatalf("group %s unplaced at %v, want %v: the newest unplaced pod of the group", g, at[g], w)
		}
	}
}
