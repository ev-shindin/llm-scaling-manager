package steadystate

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

const nextGroup = "d-1" // the group LWS "d" removes next in these fixtures

// lwsPod is a pod of LWS "d"'s group, owned by its StatefulSet unless bare.
func lwsPod(name, group string, mods ...func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name,
		Labels:          map[string]string{lwsv1.SetNameLabelKey: "d", lwsv1.GroupIndexLabelKey: group},
		OwnerReferences: lwsStatefulSetOwner("d")}, Spec: corev1.PodSpec{NodeName: "n1"}}
	for _, m := range mods {
		m(&p)
	}
	return p
}

func deleting(p *corev1.Pod)    { p.DeletionTimestamp = &metav1.Time{Time: time.Unix(1, 0)} }
func unscheduled(p *corev1.Pod) { p.Spec.NodeName = "" }
func bare(p *corev1.Pod)        { p.OwnerReferences = nil }

// LWS removes its highest groups first. While a live transfer has marked the
// highest, a second transfer gives the next one down -- it is not "nothing to
// give" and not unsteerable. Groups at or above spec.replicas are already going.
func TestUtilizationShareLWSGivesTheGroupItRemovesNext(t *testing.T) {
	three := []corev1.Pod{lwsPod("d-0", "0"), lwsPod("d-1", "1"), lwsPod("d-2", "2")}
	first := func(p *corev1.Pod) bool { return p.Name == "d-2" }
	got, err := lwsDonorPods("d", 3, three, first)
	if err != nil || len(got) != 1 || got[0].Name != nextGroup {
		t.Fatalf("highest group marked: got %v %v, want the next group d-1", got, err)
	}
	// Above spec.replicas and terminating: a scale-down already under way.
	going := []corev1.Pod{lwsPod("d-0", "0"), lwsPod("d-1", "1"), lwsPod("d-2", "2", deleting)}
	if got, err := lwsDonorPods("d", 2, going, nil); err != nil || len(got) != 1 || got[0].Name != nextGroup {
		t.Fatalf("group above spec.replicas: got %v %v, want d-1", got, err)
	}
	// Below spec.replicas and terminating: a rollout, refused -- and not as exhausted.
	if _, err := lwsDonorPods("d", 2, []corev1.Pod{lwsPod("d-0", "0"), lwsPod("d-1", "1", deleting)}, nil); err == nil ||
		errors.Is(err, errDonorExhausted) {
		t.Fatalf("a terminating group below spec.replicas: err %v, want unsteerable", err)
	}
	// Unscheduled: the next group LWS removes is Pending, refused.
	if _, err := lwsDonorPods("d", 2, []corev1.Pod{lwsPod("d-0", "0"), lwsPod("d-1", "1", unscheduled)}, nil); err == nil {
		t.Fatal("an unscheduled group was offered")
	}
	// A bare pod at a higher index is not the LWS's.
	if got, err := lwsDonorPods("d", -1, []corev1.Pod{lwsPod("d-0", "0"), lwsPod("d-1", "1"), lwsPod("evil", "9", bare)}, nil); err != nil ||
		len(got) != 1 || got[0].Name != nextGroup {
		t.Fatalf("bare pod: got %v %v, want d-1", got, err)
	}
	// Every group already given: exhausted, which is not a failure to steer.
	if _, err := lwsDonorPods("d", 3, three, func(*corev1.Pod) bool { return true }); !errors.Is(err, errDonorExhausted) {
		t.Fatalf("all groups marked: err %v, want errDonorExhausted", err)
	}
}

func costPod(cost string, marked bool) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	if cost != "" {
		p.Annotations[podDeletionCostAnnotation] = cost
	}
	if marked {
		p.Annotations[utilizationShareTransferAnnotation] = `{"id":"x"}`
	}
	return p
}

// The mark sits below every unmarked sibling and above every live mark of the
// same donor, or reports that no cost does.
func TestShareMarkCost(t *testing.T) {
	minInt32 := strconv.Itoa(math.MinInt32)
	for _, tc := range []struct {
		name     string
		unmarked []corev1.Pod
		live     []corev1.Pod
		want     string
		ok       bool
	}{
		{"no siblings", nil, nil, "-1000", true},
		{"default siblings", []corev1.Pod{costPod("", false)}, nil, "-1000", true},
		{"user cost below", []corev1.Pod{costPod("-3000", false)}, nil, "-3001", true},
		{"after a live mark", []corev1.Pod{costPod("", false)}, []corev1.Pod{costPod("-1000", true)}, "-999", true},
		{"after a live mark below a user's", []corev1.Pod{costPod("-3000", false)}, []corev1.Pod{costPod("-3001", true)}, "-3001", false},
		{"live mark not visible yet", []corev1.Pod{costPod("", false)}, []corev1.Pod{costPod("", false)}, "-999", true},
		{"a sibling at the int32 minimum", []corev1.Pod{costPod(minInt32, false)}, nil, minInt32, false},
	} {
		got, ok := markCost(tc.unmarked, tc.live)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: markCost = %s, %t; want %s, %t", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// A set is private when it crosses a namespace anywhere -- including through a
// contributor, whose mark would otherwise carry the primary's transfer ID.
func TestSharePrivate(t *testing.T) {
	tr := func(donor, receiver string) allocation.ShareTransfer {
		return allocation.ShareTransfer{Donor: donor, Receiver: receiver}
	}
	for _, tc := range []struct {
		name string
		set  []allocation.ShareTransfer
		want bool
	}{
		{"same namespace", []allocation.ShareTransfer{tr("a/m/both", "a/n/both")}, false},
		{"cross namespace", []allocation.ShareTransfer{tr("a/m/both", "b/n/both")}, true},
		{"refill", []allocation.ShareTransfer{tr("a/m/both", "")}, false},
		{"set in one namespace", []allocation.ShareTransfer{tr("a/m/both", "a/n/both"), tr("a/k/both", "")}, false},
		{"contributor elsewhere", []allocation.ShareTransfer{tr("a/m/both", "a/n/both"), tr("b/k/both", "")}, true},
	} {
		if got := sharePrivate(tc.set); got != tc.want {
			t.Errorf("%s: sharePrivate = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// A redirect rewrites a mark and keeps its own cost: dropping it made a later
// unmark read our cost as the user's and leave it on the pod.
func TestUtilizationShareRemarkKeepsTheMarksOwnCost(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	for i := range f.current["A"] {
		annotate(t, c, "A-v-"+string(rune('0'+i)), map[string]string{podDeletionCostAnnotation: "-3000"})
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	var tr allocation.ShareTransfer
	for _, l := range se.e.utilizationShare.ledgers {
		for _, t := range l.Transfers() {
			tr = t
			se.e.remarkDonorPods(se.ctx, ctrl.LoggerFrom(se.ctx), l, t.ID)
		}
	}
	marked := markedPods(t, c)
	if tr.ID == "" || len(marked) != 1 {
		t.Fatalf("setup: id %q, %d marked", tr.ID, len(marked))
	}
	var m transferMark
	if err := json.Unmarshal([]byte(marked[0].Annotations[utilizationShareTransferAnnotation]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Cost != "-3001" {
		t.Fatalf("remarked with cost %q, want the mark's own -3001", m.Cost)
	}
	se.e.unmarkDonorPods(se.ctx, ctrl.LoggerFrom(se.ctx), allocation.ShareTransfer{ID: tr.ID, DonorPods: podKeys(marked)})
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(&marked[0]), &p); err != nil {
		t.Fatal(err)
	}
	if got := p.Annotations[podDeletionCostAnnotation]; got != "-3000" {
		t.Fatalf("deletion cost after unmark %q, want the user's -3000 back", got)
	}
}

// An abort that is not the donor's own failure -- its set could not complete --
// sets no back-off, and the Warning must not invent one.
func TestUtilizationShareAbortedEventSaysWhetherTheDonorBacksOff(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	rec := record.NewFakeRecorder(10)
	se.e.Recorder = rec
	accessor := func(_, variant string) scaletarget.ScaleTargetAccessor { return f.scaleTargets()["ns/"+variant] }
	g := allocation.ShareGroup{Origins: map[string]allocation.ShareRoleOrigin{
		roleA: {Namespace: "ns", ModelID: "A", Role: "both"}, roleB: {Namespace: "ns", ModelID: "B", Role: "both"}}}
	tr := allocation.ShareTransfer{ID: "x", Donor: roleA, DonorVariant: "A-v", Receiver: roleB, ReceiverVariant: "B-v",
		GPUs: 1, DonorGPUs: 1, Started: time.Unix(1000, 0)}
	tm := allocation.ShareTimings{ReleaseTimeout: 10 * time.Minute}
	end := allocation.ShareTransferEnd{Transfer: tr, Outcome: allocation.ShareOutcomeAborted}

	se.e.shareEndedEvent(g, end, tm, time.Time{}, accessor)
	if got := <-rec.Events; !strings.Contains(got, "donor set could not complete") || strings.Contains(got, "0001-01-01") {
		t.Fatalf("no back-off: event %q", got)
	}
	se.e.shareEndedEvent(g, end, tm, time.Unix(2200, 0), accessor)
	if got := <-rec.Events; !strings.Contains(got, "not asked to give again before") {
		t.Fatalf("back-off: event %q", got)
	}
}

// An idle fill that lands says so on the receiver: nothing else explains a
// model growing with no load to grow it.
func TestUtilizationShareIdleFillLandingIsAnEvent(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	rec := record.NewFakeRecorder(10)
	se.e.Recorder = rec
	accessor := func(_, variant string) scaletarget.ScaleTargetAccessor { return f.scaleTargets()["ns/"+variant] }
	g := allocation.ShareGroup{Origins: map[string]allocation.ShareRoleOrigin{roleB: {Namespace: "ns", ModelID: "B", Role: "both"}}}
	fill := allocation.ShareTransfer{ID: "f", Receiver: roleB, ReceiverVariant: "B-v", GPUs: 1}
	se.e.shareEndedEvent(g, allocation.ShareTransferEnd{Transfer: fill, Outcome: allocation.ShareOutcomeDone},
		allocation.ShareTimings{}, time.Time{}, accessor)
	select {
	case got := <-rec.Events:
		if !strings.HasPrefix(got, "Normal "+constants.K8SEventUtilizationShareReceived) || !strings.Contains(got, "spare GPUs") {
			t.Fatalf("idle fill event %q", got)
		}
	default:
		t.Fatal("an idle fill landed with no Event")
	}
}

// The restart quiet period is a blocked reason on every planned model: a model
// under load that stops scaling for one fill timeout is otherwise unexplained.
func TestUtilizationShareQuietPeriodIsAReason(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	se.cycle() // the ledger starts, and the quiet period with it
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, mf := range mfs {
		if mf.GetName() != constants.WVAModelScalingBlocked {
			continue
		}
		for _, m := range mf.GetMetric() {
			reason, model := "", ""
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case constants.LabelReason:
					reason = l.GetValue()
				case constants.LabelModelName:
					model = l.GetValue()
				}
			}
			if reason == constants.ScalingBlockedQuietPeriod && m.GetGauge().GetValue() == 1 {
				models = append(models, model)
			}
		}
	}
	slices.Sort(models)
	if !slices.Equal(models, []string{"A", "B", "C"}) {
		t.Fatalf("quiet-period reported for %v, want every planned model A, B, C", models)
	}
}

// Released by count when a different pod of the donor went: the marked pod
// survives, and its mark must go -- a restart inside the release timeout would
// otherwise restore the transfer and move a second replica.
func TestUtilizationShareReleaseUnmarksASurvivingPod(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	if started := se.untilStarted(); started != 1 {
		t.Skipf("fixture started %d transfers; this test needs exactly one", started)
	}
	m := markedPods(t, c)[0]
	var original transferMark
	if err := json.Unmarshal([]byte(m.Annotations[utilizationShareTransferAnnotation]), &original); err != nil {
		t.Fatal(err)
	}
	var other corev1.Pod
	for i := range f.current["A"] {
		if name := "A-v-" + string(rune('0'+i)); name != m.Name {
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, &other); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if err := c.Delete(context.Background(), &other); err != nil {
		t.Fatal(err)
	}
	f.current["A"]-- // the donor shrank, by the wrong pod
	for range 5 {
		se.cycle()
	}
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(&m), &p); err != nil {
		t.Fatal(err)
	}
	// A later transfer may mark the pod again; the released one's mark must go.
	var now transferMark
	if raw, ok := p.Annotations[utilizationShareTransferAnnotation]; ok && json.Unmarshal([]byte(raw), &now) == nil &&
		now.ID == original.ID {
		t.Fatalf("pod %s still carries released transfer %s's mark", p.Name, original.ID)
	}
}

// A rollout that changed a template label: the old ReplicaSet's pods no longer
// match the current template, but the Deployment's selector still selects
// them, and the Deployment controller still splits a scale-down across both.
func TestUtilizationShareRolloutSeenThroughTheSelector(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f) // A's pods: app=A-decode, ReplicaSet A-decode-h1
	annotateLabels := func(name string, labels map[string]string) {
		var p corev1.Pod
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, &p); err != nil {
			t.Fatal(err)
		}
		for k, v := range labels {
			p.Labels[k] = v
		}
		if err := c.Update(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
	}
	for i := range f.current["A"] {
		annotateLabels("A-v-"+string(rune('0'+i)), map[string]string{"version": "2"})
	}
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "A-old",
		Labels: map[string]string{"app": "A-decode", "version": "1", appsv1.DefaultDeploymentUniqueLabelKey: "h0"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet",
			Name: "A-decode-h0", UID: "rs0", Controller: ptr.To(true)}}},
		Spec: corev1.PodSpec{NodeName: "n1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if err := c.Create(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	n := int32(f.current["A"])
	d := func(selector *metav1.LabelSelector) scaletarget.ScaleTargetAccessor {
		return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "A-decode"},
			Spec: appsv1.DeploymentSpec{Replicas: &n, Selector: selector,
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "A-decode", "version": "2"}}}},
		})
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	_, err := se.e.donorPods(se.ctx, d(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "A-decode"}}), "ns", nil)
	if err == nil || !strings.Contains(err.Error(), "mid-rollout") {
		t.Fatalf("selector sees both ReplicaSets: err %v, want mid-rollout", err)
	}
	// The control: by the template's labels alone the old pod is invisible.
	if _, err := se.e.donorPods(se.ctx, d(nil), "ns", nil); err != nil {
		t.Fatalf("control without a selector: %v, want the old pod unseen", err)
	}
}

// Equal deletion costs: the ReplicaSet removes the most recently Ready pod, so
// the mark goes there -- marking an older one would steer the ReplicaSet away
// from its own choice and cost the warm cache it keeps.
func TestUtilizationShareMarksTheMostRecentlyReadyPod(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	for i := range f.current["A"] {
		var p corev1.Pod
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "A-v-" + string(rune('0'+i))}, &p); err != nil {
			t.Fatal(err)
		}
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: time.Unix(int64(100*(i+1)), 0)}}}
		if err := c.Status().Update(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	tr := allocation.ShareTransfer{ID: "x-t1-ab", Donor: roleA, DonorVariant: "A-v", Receiver: roleB,
		ReceiverVariant: "B-v", GPUs: 1, DonorGPUs: 1, Started: time.Unix(0, 0)}
	pods, err := se.e.markDonorPods(se.ctx, tr, f.scaleTargets()["ns/A-v"], "ns", func(*corev1.Pod) bool { return false }, false)
	newest := "A-v-" + string(rune('0'+f.current["A"]-1))
	if err != nil || len(pods) != 1 || pods[0].Name != newest {
		t.Fatalf("marked %v (%v), want the most recently Ready %s", podKeys(pods), err, newest)
	}
}

// A Deployment donor whose every pod a live transfer holds is exhausted, not
// unsteerable: it backs off without a Warning.
func TestUtilizationShareDeploymentDonorExhausted(t *testing.T) {
	f := newShareFleet()
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	tr := allocation.ShareTransfer{ID: "x-t1-ab", Donor: roleA, DonorVariant: "A-v", Receiver: roleB,
		ReceiverVariant: "B-v", GPUs: 1, DonorGPUs: 1, Started: time.Unix(0, 0)}
	_, err := se.e.markDonorPods(se.ctx, tr, f.scaleTargets()["ns/A-v"], "ns", func(*corev1.Pod) bool { return true }, false)
	if !errors.Is(err, errDonorExhausted) {
		t.Fatalf("every pod given: err %v, want errDonorExhausted", err)
	}
}
