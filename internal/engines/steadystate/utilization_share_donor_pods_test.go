package steadystate

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// A Deployment with fewer pods than spec.replicas -- one the quota would not
// let it create -- frees nothing when its count is lowered: the ReplicaSet
// removes only pods above the count.
func TestUtilizationShareRefusesADeploymentMissingAPod(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	p := podOf(t, c, "A-v-0")
	if err := c.Delete(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	_, err := se.e.donorPods(se.ctx, f.scaleTargets()["ns/A-v"], "ns", nil)
	if err == nil || !strings.Contains(err.Error(), "would remove none") || errors.Is(err, errDonorExhausted) {
		t.Fatalf("donorPods with 8 of 9 pods: %v, want an unsteerable refusal", err)
	}
}

// An LWS with no pod of a group below spec.replicas, or whose groups are of two
// revisions (a rolling update, surge groups included), cannot be steered.
func TestUtilizationShareLWSRefusesAMissingGroupOrARollout(t *testing.T) {
	if _, err := lwsDonorPods("d", 3, []corev1.Pod{lwsPod("d-0", "0"), lwsPod("d-2", "2")}, nil); err == nil ||
		!strings.Contains(err.Error(), "group 1") {
		t.Fatalf("group 1 missing: %v, want a refusal naming it", err)
	}
	rev := func(h string) func(*corev1.Pod) { return func(p *corev1.Pod) { p.Labels[lwsv1.RevisionKey] = h } }
	if _, err := lwsDonorPods("d", 2, []corev1.Pod{lwsPod("d-0", "0", rev("a")), lwsPod("d-1", "1", rev("a")),
		lwsPod("d-2", "2", rev("b"))}, nil); err == nil || !strings.Contains(err.Error(), "mid-rollout") {
		t.Fatalf("a surge group of a new revision: %v, want a rollout refusal", err)
	}
	if got, err := lwsDonorPods("d", 2, []corev1.Pod{lwsPod("d-0", "0", rev("a")), lwsPod("d-1", "1", rev("a"))}, nil); err != nil ||
		len(got) != 1 || got[0].Name != nextGroup {
		t.Fatalf("control, one revision: %v (%v), want %s", got, err, nextGroup)
	}
}

// A sibling a user pinned at the lowest cost there is leaves the mark no room
// below it on a Deployment: refused as unsteerable, not as exhausted.
func TestUtilizationShareMarkRefusesWhenASiblingLeavesNoRoom(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	pin := strconv.Itoa(math.MinInt32)
	annotate(t, c, "A-v-1", map[string]string{podDeletionCostAnnotation: pin})
	annotate(t, c, "A-v-2", map[string]string{podDeletionCostAnnotation: pin})
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	tr := allocation.ShareTransfer{ID: "x-t1-ab", Donor: roleA, DonorVariant: "A-v", Receiver: roleB,
		ReceiverVariant: "B-v", GPUs: 1, DonorGPUs: 1, Started: time.Unix(0, 0)}
	_, err := se.e.markDonorPods(se.ctx, tr, f.scaleTargets()["ns/A-v"], "ns", func(*corev1.Pod) bool { return false }, false)
	if err == nil || errors.Is(err, errDonorExhausted) || !strings.Contains(err.Error(), "no room below it") {
		t.Fatalf("two pods at the int32 minimum: %v, want an unsteerable refusal", err)
	}
	if len(markedPods(t, c)) != 0 {
		t.Fatal("a pod was marked")
	}
}

// Through the actuator: a donor whose every pod a live transfer holds is held,
// not failed -- no back-off, not unsteerable, no Warning.
func TestUtilizationShareExhaustedDonorIsHeldNotFailed(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	rec := record.NewFakeRecorder(100)
	se.e.Recorder = rec
	se.untilStarted()
	m := markedPods(t, c)[0]
	var mark transferMark
	if err := json.Unmarshal([]byte(m.Annotations[utilizationShareTransferAnnotation]), &mark); err != nil {
		t.Fatal(err)
	}
	// Every other pod of A now carries the live transfer's mark: A has
	// nothing left to give.
	for i := range f.current["A"] {
		if name := "A-v-" + string(rune('0'+i)); name != m.Name {
			annotate(t, c, name, map[string]string{podDeletionCostAnnotation: mark.Cost,
				utilizationShareTransferAnnotation: m.Annotations[utilizationShareTransferAnnotation]})
		}
	}
	busy := false
	for range 6 {
		se.cycle()
		for _, l := range se.e.utilizationShare.ledgers {
			if l.Unsteerable(roleA, se.clock) || l.BackingOff(roleA, se.clock) {
				t.Fatal("an exhausted donor backs off or is unsteerable")
			}
			busy = busy || l.GivingBusy(roleA, se.clock)
		}
	}
	if !busy {
		t.Fatal("setup: the planner never asked A again, so the exhausted path did not run")
	}
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.HasPrefix(e, corev1.EventTypeWarning) {
			t.Fatalf("an exhausted donor got a Warning: %s", e)
		}
	}
}

// A node-aware plan breaks equal costs by the most recently Ready, then by
// name -- not by name alone, and a not-Ready pod refuses the donor.
func TestUtilizationShareDonorUnitsNewestReadyFirst(t *testing.T) {
	mk := func(name string, readySince int64) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}, Spec: corev1.PodSpec{NodeName: "n1"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady,
				Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: time.Unix(readySince, 0)}}}}}
	}
	units := shareDonorUnits(false, []corev1.Pod{mk("a", 100), mk("b", 300), mk("c", 200)},
		func(*corev1.Pod) bool { return false })
	order := make([]string, 0, len(units))
	for _, u := range units {
		for _, p := range u.Pods {
			order = append(order, p.Name)
		}
	}
	if strings.Join(order, ",") != "ns/b,ns/c,ns/a" {
		t.Fatalf("unit order %v, want the most recently Ready first", order)
	}
	notReady := mk("d", 400)
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	if got := shareDonorUnits(false, []corev1.Pod{mk("a", 100), notReady}, func(*corev1.Pod) bool { return false }); got != nil {
		t.Fatalf("a not-Ready pod: units %v, want none", got)
	}
}

// A set abort after the donor already released says the pod is gone and a
// replacement is starting: "the count is restored" alone hides a cold start.
func TestUtilizationShareSetAbortAfterReleaseNamesThePod(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	rec := record.NewFakeRecorder(10)
	se.e.Recorder = rec
	accessor := func(_, variant string) scaletarget.ScaleTargetAccessor { return f.scaleTargets()["ns/"+variant] }
	g := allocation.ShareGroup{Origins: map[string]allocation.ShareRoleOrigin{
		roleA: {Namespace: "ns", ModelID: "A", Role: "both"}, roleB: {Namespace: "ns", ModelID: "B", Role: "both"}}}
	tr := allocation.ShareTransfer{ID: "x", Donor: roleA, DonorVariant: "A-v", Receiver: roleB, ReceiverVariant: "B-v",
		GPUs: 1, DonorGPUs: 1, Started: time.Unix(1000, 0), DonorPods: []string{"ns/A-v-7"}}
	tm := allocation.ShareTimings{ReleaseTimeout: 10 * time.Minute}
	end := allocation.ShareTransferEnd{Transfer: tr, Outcome: allocation.ShareOutcomeAborted, DonorReleased: true}
	se.e.shareEndedEvent(g, end, tm, time.Time{}, accessor)
	if got := <-rec.Events; !strings.Contains(got, "already released A-v-7") || !strings.Contains(got, "replacement pod") {
		t.Fatalf("set abort after the release: event %q", got)
	}
}

// A Deployment whose status says some pods are not of its current template --
// a new ReplicaSet whose pods do not exist yet included -- is mid-rollout,
// however its pods read.
func TestUtilizationShareRefusesADeploymentRollingOutByStatus(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	n := int32(f.current["A"])
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "A-decode"},
		Spec: appsv1.DeploymentSpec{Replicas: &n, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": "A-decode"}}}},
		Status: appsv1.DeploymentStatus{Replicas: n, UpdatedReplicas: 0}}
	if _, err := se.e.donorPods(se.ctx, scaletarget.NewDeploymentAccessor(d), "ns", nil); err == nil ||
		!strings.Contains(err.Error(), "mid-rollout") {
		t.Fatalf("a Deployment whose new ReplicaSet has no pods yet: %v, want a rollout refusal", err)
	}
	d.Status.UpdatedReplicas = n
	if _, err := se.e.donorPods(se.ctx, scaletarget.NewDeploymentAccessor(d), "ns", nil); err != nil {
		t.Fatalf("control, every pod updated: %v", err)
	}
}
