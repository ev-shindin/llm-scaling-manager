package steadystate

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// podOf reads a fixture pod.
func podOf(t *testing.T, c client.Client, name string) corev1.Pod {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A Deployment mid-rollout cannot be steered: the Deployment controller splits
// a scale-down across its ReplicaSets, and a cost ranks pods only within one.
func TestUtilizationShareRefusesADonorMidRollout(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	p := podOf(t, c, "A-v-0")
	// A pod of the rollout's new ReplicaSet: its own hash, its own owner.
	p.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "h2"
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet",
		Name: "A-decode-h2", UID: "rs2", Controller: ptr.To(true)}}
	if err := c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	if _, err := se.e.donorPods(se.ctx, f.scaleTargets()["ns/A-v"], "ns"); err == nil ||
		!strings.Contains(err.Error(), "mid-rollout") {
		t.Fatalf("donorPods on a Deployment with pods of two ReplicaSets: %v, want a mid-rollout refusal", err)
	}
}

// Finished pods are not the Deployment's: an evicted pod waiting to be
// collected does not stop its model from giving.
func TestUtilizationShareIgnoresFinishedPods(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	p := podOf(t, c, "A-v-0")
	p.Status.Phase = corev1.PodFailed
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	pods, err := se.e.donorPods(se.ctx, f.scaleTargets()["ns/A-v"], "ns")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range pods {
		if q.Name == "A-v-0" {
			t.Fatal("a failed pod was offered as a donor pod")
		}
	}
}

// The mark's cost is below every sibling's: a sibling a user set to -3000
// would otherwise be the one the ReplicaSet removes.
func TestUtilizationShareMarksBelowAUsersLowCost(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	annotate(t, c, "A-v-3", map[string]string{podDeletionCostAnnotation: "-3000"})
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	m := markedPods(t, c)
	if len(m) != 1 {
		t.Fatalf("setup: %d marked", len(m))
	}
	if got := m[0].Annotations[podDeletionCostAnnotation]; got != "-3001" {
		t.Fatalf("marked %s at cost %s, want -3001, below the user's -3000", m[0].Name, got)
	}
	// Its own cost is the one the unmark checks.
	se.e.unmarkDonorPods(se.ctx, ctrl.LoggerFrom(se.ctx), allocation.ShareTransfer{DonorPods: []string{"ns/" + m[0].Name}})
	if _, ok := podOf(t, c, m[0].Name).Annotations[utilizationShareTransferAnnotation]; ok {
		t.Fatal("the mark was kept")
	}
}

// A mark on a donor pod names its receiver only within the donor's namespace:
// a tenant reading its own pods learns nothing of another tenant's models.
func TestUtilizationShareMarkHidesAReceiverInAnotherNamespace(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	for _, tc := range []struct {
		receiver string
		hidden   bool
	}{{roleB, false}, {"team-y/secret-model/decode", true}} {
		tr := allocation.ShareTransfer{ID: "x-t1-ab", Donor: roleA, DonorVariant: "A-v", Receiver: tc.receiver,
			ReceiverVariant: "rv", GPUs: 1, DonorGPUs: 1, Started: time.Unix(0, 0)}
		pods, err := se.e.markDonorPods(se.ctx, tr, f.scaleTargets()["ns/A-v"], "ns", func(*corev1.Pod) bool { return false })
		if err != nil || len(pods) != 1 {
			t.Fatalf("setup: %v %d", err, len(pods))
		}
		raw := pods[0].Annotations[utilizationShareTransferAnnotation]
		if leaked := strings.Contains(raw, "secret-model") || strings.Contains(raw, "team-y"); leaked != false {
			t.Fatalf("the mark names another tenant's model: %s", raw)
		}
		var m transferMark
		_ = json.Unmarshal([]byte(raw), &m)
		if tc.hidden != (m.Receiver == "") {
			t.Fatalf("receiver %q: mark %s", tc.receiver, raw)
		}
		se.e.unmarkDonorPods(se.ctx, ctrl.LoggerFrom(se.ctx), allocation.ShareTransfer{DonorPods: podKeys(pods)})
	}
}

// Transfer IDs carry a random part of their own: one read off a tenant's pod
// does not predict the next.
func TestShareTransferIDsAreNotPredictable(t *testing.T) {
	l := allocation.NewShareLedger()
	held := map[string]int{"A": 4, "B": 0}
	tm := allocation.ShareTimings{Window: time.Minute, ReleaseTimeout: time.Hour, FillTimeout: time.Hour}
	a := l.Start(allocation.ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, held, time.Unix(0, 0), tm)
	b := l.Start(allocation.ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, DonorGPUs: 1}, held, time.Unix(0, 0), tm)
	shape := regexp.MustCompile(`^[0-9a-f]+-t[0-9]+-[0-9a-f]{8}$`)
	if !shape.MatchString(a.ID) || !shape.MatchString(b.ID) {
		t.Fatalf("IDs %q, %q lack a random suffix", a.ID, b.ID)
	}
	if a.ID[strings.LastIndex(a.ID, "-"):] == b.ID[strings.LastIndex(b.ID, "-"):] {
		t.Fatalf("two IDs share their random part: %q %q", a.ID, b.ID)
	}
}

// Every way a transfer ends is told to the side it is about, at the right
// severity, and an Event never names another namespace's model.
func TestUtilizationShareEventsForEveryOutcome(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	rec := record.NewFakeRecorder(100)
	se.e.Recorder = rec
	accessor := func(_, variant string) scaletarget.ScaleTargetAccessor { return f.scaleTargets()["ns/"+variant] }
	g := allocation.ShareGroup{Origins: map[string]allocation.ShareRoleOrigin{
		roleA: {Namespace: "ns", ModelID: "A", Role: "both"}, roleB: {Namespace: "ns", ModelID: "B", Role: "both"}}}
	tr := allocation.ShareTransfer{ID: "x", Donor: roleA, DonorVariant: "A-v", Receiver: roleB, ReceiverVariant: "B-v",
		GPUs: 1, DonorGPUs: 1}
	tm := allocation.ShareTimings{ReleaseTimeout: 10 * time.Minute, FillTimeout: 3 * time.Minute}
	drain := func() []string {
		var out []string
		for len(rec.Events) > 0 {
			out = append(out, <-rec.Events)
		}
		return out
	}
	for _, tc := range []struct {
		outcome allocation.ShareTransferOutcome
		want    string // "Type Reason"
	}{
		{allocation.ShareOutcomeAborted, "Warning " + constants.K8SEventUtilizationShareReleaseAborted},
		{allocation.ShareOutcomeWrongPod, "Warning " + constants.K8SEventUtilizationShareWrongPod},
		{allocation.ShareOutcomeFillTimeout, "Warning " + constants.K8SEventUtilizationShareFillTimedOut},
		{allocation.ShareOutcomeDone, "Normal " + constants.K8SEventUtilizationShareReceived},
	} {
		se.e.shareEndedEvent(g, allocation.ShareTransferEnd{Transfer: tr, Outcome: tc.outcome}, tm, time.Unix(600, 0),
			accessor)
		got := drain()
		if len(got) != 1 || !strings.HasPrefix(got[0], tc.want) {
			t.Fatalf("%s: events %q, want one %q", tc.outcome, got, tc.want)
		}
	}
	se.e.shareCancelledEvents(tr, accessor)
	if got := drain(); len(got) != 2 || !strings.HasPrefix(got[0], "Normal "+constants.K8SEventUtilizationShareCancelled) {
		t.Fatalf("cancelled: %q, want one on each side", got)
	}
	se.e.shareRedirectedEvent(tr, accessor)
	if got := drain(); len(got) != 1 || !strings.HasPrefix(got[0], "Normal "+constants.K8SEventUtilizationShareRedirected) {
		t.Fatalf("redirected: %q", got)
	}
	// A set member is told whom its set funds, not "the reserve".
	member := allocation.ShareTransfer{ID: "m", SetID: "x", Donor: roleA, DonorVariant: "A-v", DonorGPUs: 1}
	se.e.shareStartedEvents(g, member, []allocation.ShareTransfer{tr, member}, []string{"ns/A-v-0"}, accessor)
	got := drain()
	if len(got) != 1 || !strings.Contains(got[0], "model B") || strings.Contains(got[0], "reserve") ||
		!strings.Contains(got[0], "A-v-0") {
		t.Fatalf("a set member's Giving Event: %q, want it to name model B and the pod", got)
	}
	if strings.Contains(got[0], "(both)") {
		t.Fatalf("an aggregated model's role printed: %q", got)
	}
}

// LWS removes its highest group whatever its pods' state: when that group is
// terminating, the next group down is not the one that goes, so the donor is
// refused rather than marked on the wrong group.
func TestUtilizationShareRefusesAnLWSWhoseHighestGroupIsTerminating(t *testing.T) {
	pod := func(name, group string, deleting bool) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name,
			Labels:          map[string]string{lwsv1.SetNameLabelKey: "d", lwsv1.GroupIndexLabelKey: group},
			OwnerReferences: lwsStatefulSetOwner("d")}, Spec: corev1.PodSpec{NodeName: "n1"}}
		if deleting {
			p.DeletionTimestamp = &metav1.Time{Time: time.Unix(1, 0)}
		}
		return p
	}
	if _, err := lwsDonorPods("d", []corev1.Pod{pod("d-0", "0", false), pod("d-1", "1", true)}); err == nil {
		t.Fatal("the highest group is terminating, yet the donor was offered group 0")
	}
	got, err := lwsDonorPods("d", []corev1.Pod{pod("d-0", "0", false), pod("d-1", "1", false)})
	if err != nil || len(got) != 1 || got[0].Name != "d-1" {
		t.Fatalf("control: %v %v, want the highest group d-1", got, err)
	}
}

// A new mark keeps the user's earlier cost only while the pod still carries
// the cost an old mark of ours wrote: a cost the user set since is theirs.
func TestUtilizationShareRemarkRecordsACostSetSinceTheOldMark(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	old, _ := json.Marshal(transferMark{ID: "old-t1-aa", Donor: roleA, PrevCost: ptr.To("7")})
	for i := range f.current["A"] {
		annotate(t, c, "A-v-"+string(rune('0'+i)), map[string]string{
			utilizationShareTransferAnnotation: string(old), podDeletionCostAnnotation: "-5000"})
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	tr := allocation.ShareTransfer{ID: "new-t1-bb", Donor: roleA, DonorVariant: "A-v", Receiver: roleB,
		ReceiverVariant: "B-v", GPUs: 1, DonorGPUs: 1, Started: time.Unix(0, 0)}
	pods, err := se.e.markDonorPods(se.ctx, tr, f.scaleTargets()["ns/A-v"], "ns", func(*corev1.Pod) bool { return false })
	if err != nil || len(pods) != 1 {
		t.Fatalf("setup: %v %d", err, len(pods))
	}
	var m transferMark
	_ = json.Unmarshal([]byte(pods[0].Annotations[utilizationShareTransferAnnotation]), &m)
	if m.PrevCost == nil || *m.PrevCost != "-5000" {
		t.Fatalf("prevCost %v, want the user's -5000 set since the old mark", m.PrevCost)
	}
}

// A node-aware plan takes the least protected pods first.
func TestUtilizationShareDonorUnitsLeastProtectedFirst(t *testing.T) {
	ready := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	mk := func(name, cost string) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}, Spec: corev1.PodSpec{NodeName: "n1"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: ready}}
		if cost != "" {
			p.Annotations = map[string]string{podDeletionCostAnnotation: cost}
		}
		return p
	}
	units := shareDonorUnits(false, []corev1.Pod{mk("a", "100"), mk("b", ""), mk("c", "-5")},
		func(*corev1.Pod) bool { return false })
	order := make([]string, 0, len(units))
	for _, u := range units {
		for _, p := range u.Pods {
			order = append(order, p.Name)
		}
	}
	if strings.Join(order, ",") != "ns/c,ns/b,ns/a" {
		t.Fatalf("unit order %v, want the lowest cost first", order)
	}
}

// The mode is published on every cycle, also one with no model to plan.
func TestUtilizationShareModeWithoutModels(t *testing.T) {
	r := freshMetrics(t)
	e := &Engine{Config: shadowConfig(t, selectedShadow)}
	e.observeUtilizationShareMode(context.Background())
	for _, m := range family(t, r, constants.WVAUtilizationShareMode) {
		want := 0.0
		if label(m, constants.LabelMode) == constants.UtilizationShareModeShadow {
			want = 1
		}
		if m.GetGauge().GetValue() != want {
			t.Fatalf("mode %s = %v with no models planned", label(m, constants.LabelMode), m.GetGauge().GetValue())
		}
	}
	if n := len(family(t, r, constants.WVAUtilizationShareMode)); n != len(constants.UtilizationShareModes) {
		t.Fatalf("%d mode series, want %d", n, len(constants.UtilizationShareModes))
	}
	e = &Engine{Config: shadowConfig(t, "")}
	e.observeUtilizationShareMode(context.Background())
	for _, m := range family(t, r, constants.WVAUtilizationShareMode) {
		if label(m, constants.LabelMode) == constants.UtilizationShareModeOff && m.GetGauge().GetValue() != 1 {
			t.Fatal("no optimizer block: mode off not set")
		}
	}
}
