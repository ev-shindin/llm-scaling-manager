package steadystate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// modelOfPod and preemptedPod decide which model a preempted pod's cooldown
// starts for, and whether the pod was preempted at all.
func TestModelOfPodAndPreemptedPod(t *testing.T) {
	requests := []allocation.ModelScalingRequest{
		{ModelID: "chat", Namespace: "b", VariantStates: []domain.VariantReplicaState{{VariantName: "chat-v"}}},
		{ModelID: "code", Namespace: "b", VariantStates: []domain.VariantReplicaState{{VariantName: "code-v"}}},
	}
	targets := map[string]scaletarget.ScaleTargetAccessor{
		"b/chat-v": scaletarget.NewDeploymentAccessor(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "b"}}),
		"b/code-v": scaletarget.NewLWSAccessor(&lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "code", Namespace: "b"}}),
	}
	at := metav1.NewTime(time.Unix(5000, 0))
	terminating := func(reason, message string) []corev1.PodCondition {
		return []corev1.PodCondition{{Type: kueueTerminationTarget, Status: corev1.ConditionTrue, Reason: reason,
			Message: message, LastTransitionTime: at}}
	}
	for _, tc := range []struct {
		name      string
		pod       corev1.Pod
		model     string
		preempted bool
		preemptAt time.Time
	}{
		{name: "a Deployment's pod, matched by name prefix, preempted",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "chat-7d9f-abc"},
				Status: corev1.PodStatus{Conditions: terminating("WorkloadEvictedDueToPreempted", "")}},
			model: "b/chat", preempted: true, preemptAt: at.Time},
		{name: "an LWS pod, matched by its set-name label, whatever its name",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-0", Labels: map[string]string{lwsv1.SetNameLabelKey: "code"}},
				Status: corev1.PodStatus{Conditions: terminating("Evicted", "Preempted to accommodate a workload")}},
			model: "b/code", preempted: true, preemptAt: at.Time},
		{name: "an LWS label naming no model's set matches nothing, not a name prefix",
			pod:   corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "chat-1", Labels: map[string]string{lwsv1.SetNameLabelKey: "other"}}},
			model: ""},
		{name: "no owner, no matching prefix: no model",
			pod:   corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "batch-job-xyz"}},
			model: ""},
		{name: "a pod terminated for another reason is not preempted",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "chat-7d9f-def"},
				Status: corev1.PodStatus{Conditions: terminating("WorkloadEvictedDueToPodsReadyTimeout", "")}},
			model: "b/chat"},
		{name: "a TerminationTarget condition that is not True is not preempted",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "chat-x"}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: kueueTerminationTarget, Status: corev1.ConditionFalse, Reason: "WorkloadEvictedDueToPreempted"}}}},
			model: "b/chat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelOfPod(requests, targets, "b", &tc.pod); got != tc.model {
				t.Errorf("modelOfPod = %q, want %q", got, tc.model)
			}
			gotAt, preempted := preemptedPod(&tc.pod)
			if preempted != tc.preempted || !gotAt.Equal(tc.preemptAt) {
				t.Errorf("preemptedPod = %v, %t; want %v, %t", gotAt, preempted, tc.preemptAt, tc.preempted)
			}
		})
	}
	if got := modelOfPod(requests, targets, "other-ns", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "chat-1"}}); got != "" {
		t.Errorf("a pod of another namespace matched %q", got)
	}
}

// eventTime prefers lastTimestamp, then the events.k8s.io eventTime, then
// the object's creation.
func TestEventTime(t *testing.T) {
	last, event, created := time.Unix(300, 0), time.Unix(200, 0), time.Unix(100, 0)
	base := func() *corev1.Event {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)}}
	}
	onlyLast := base()
	onlyLast.LastTimestamp = metav1.NewTime(last)
	onlyEvent := base()
	onlyEvent.EventTime = metav1.NewMicroTime(event)
	both := base()
	both.LastTimestamp, both.EventTime = metav1.NewTime(last), metav1.NewMicroTime(event)
	for name, tc := range map[string]struct {
		ev   *corev1.Event
		want time.Time
	}{
		"lastTimestamp only":  {onlyLast, last},
		"eventTime only":      {onlyEvent, event},
		"both: lastTimestamp": {both, last},
		"neither: creation":   {base(), created},
	} {
		if got := eventTime(tc.ev); !got.Equal(tc.want) {
			t.Errorf("%s: eventTime = %v, want %v", name, got, tc.want)
		}
	}
}

// checkLWSScaleDown warns when the group a LeaderWorkerSet removes is not the
// newest one, and says nothing otherwise.
func TestCheckLWSScaleDown(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	pod := func(name, group string, age time.Duration) client.Object {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "b", CreationTimestamp: metav1.NewTime(now.Add(-age)),
			Labels: map[string]string{lwsv1.SetNameLabelKey: "serve", lwsv1.GroupIndexLabelKey: group}}}
	}
	acc := scaletarget.NewLWSAccessor(&lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "serve", Namespace: "b"}})
	release := allocation.BorrowRelease{Namespace: "b", ModelID: "m", Variant: "serve", From: 2, To: 1}
	logged := func(e *Engine) string {
		var lines []string
		ctx := logr.NewContext(context.Background(), funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{}))
		e.checkLWSScaleDown(ctx, release, acc)
		return strings.Join(lines, "\n")
	}

	recreated := &Engine{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		pod("serve-0", "0", time.Minute), pod("serve-1", "1", time.Hour)).Build()}
	if got := logged(recreated); !strings.Contains(got, `"highestGroup"=1`) || !strings.Contains(got, `"newestGroup"=0`) {
		t.Errorf("group 0 recreated after a preemption: want a warning naming groups 1 and 0, got %q", got)
	}
	inOrder := &Engine{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		pod("serve-0", "0", time.Hour), pod("serve-1", "1", time.Minute)).Build()}
	if got := logged(inOrder); got != "" {
		t.Errorf("highest group is the newest: want no log, got %q", got)
	}
	if got := logged(&Engine{}); got != "" {
		t.Errorf("no client: want no log, got %q", got)
	}
}
