package steadystate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

const activeShare = "optimizer:\n  type: utilizationShare\n"

// shareFleet is the §5.7 step on a cluster quota of 16 A100s: A (important)
// at 9 replicas, B at 5 with its demand just doubled, C at 2.
type shareFleet struct {
	current map[string]int
	demand  map[string]float64
}

func newShareFleet() *shareFleet {
	return &shareFleet{
		current: map[string]int{"A": 9, "B": 5, "C": 2},
		demand:  map[string]float64{"A": 4000, "B": 6000, "C": 1000},
	}
}

func (f *shareFleet) requests() []allocation.ModelScalingRequest {
	class := map[string]string{"A": "important"}
	out := make([]allocation.ModelScalingRequest, 0, len(f.current))
	for _, id := range []string{"A", "B", "C"} {
		out = append(out, shadowRequest(id, class[id], f.demand[id], f.current[id]))
	}
	return out
}

// scaleTargets keys each variant's Deployment by namespace/variant, as
// optimizeV2 does. The Deployment is named differently from the variant, as it
// is in a KEDA-discovered fleet: a lookup by the wrong name finds nothing.
func (f *shareFleet) scaleTargets() map[string]scaletarget.ScaleTargetAccessor {
	out := map[string]scaletarget.ScaleTargetAccessor{}
	for _, id := range []string{"A", "B", "C"} {
		dep := id + "-decode"
		n := int32(f.current[id])
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: dep},
			Spec: appsv1.DeploymentSpec{
				Replicas: &n,
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": dep}}},
			},
		}
		out["ns/"+id+"-v"] = scaletarget.NewDeploymentAccessor(d)
	}
	return out
}

func sharePods(t *testing.T, f *shareFleet) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, f.current["A"]+f.current["B"]+f.current["C"])
	for _, id := range []string{"A", "B", "C"} {
		for i := range f.current[id] {
			objs = append(objs, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: fmt.Sprintf("%s-v-%d", id, i),
					Labels: map[string]string{"app": id + "-decode", appsv1.DefaultDeploymentUniqueLabelKey: "h1"},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet",
						Name: id + "-decode-h1", UID: "rs", Controller: ptr.To(true)}}},
				Spec: corev1.PodSpec{NodeName: "n1"},
				Status: corev1.PodStatus{Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			})
		}
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func markedPods(t *testing.T, c client.Client) []corev1.Pod {
	t.Helper()
	var list corev1.PodList
	if err := c.List(context.Background(), &list, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if _, ok := p.Annotations[utilizationShareTransferAnnotation]; ok {
			out = append(out, p)
		}
	}
	return out
}

// The §5.7 step, actuated: nothing is planned in the quiet period after the
// ledger starts; then A gives, by a lowered target and a marked pod, and B is
// raised only once A's GPUs are released -- scale-down first, scale-up after.
func TestUtilizationShareActuatesScaleDownFirst(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	clock := time.Unix(0, 0)
	e := &Engine{Config: shadowConfig(t, activeShare), client: c}
	e.utilizationShare.now = func() time.Time { return clock }
	ctx, logs := observe(t)

	cycle := func() map[string]utilizationShareOverride {
		clock = clock.Add(30 * time.Second)
		return e.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets())
	}

	// The quiet period: the targets are what runs, and no pod is marked.
	o := cycle()
	if o["ns/A-v"].Target != 9 || o["ns/B-v"].Target != 5 || o["ns/C-v"].Target != 2 {
		t.Fatalf("quiet period must hold the running counts, got %+v", o)
	}
	if logs.FilterMessageSnippet("ledger started").Len() != 1 {
		t.Fatal("want the ledger start logged once")
	}

	started := 0
	for range 20 {
		o = cycle()
		if started = 9 - o["ns/A-v"].Target; started > 0 {
			break
		}
	}
	if started < 1 || started > allocation.ShareMaxConcurrentTransfers {
		t.Fatalf("want A lowered by 1..%d once planning starts, got A=%d", allocation.ShareMaxConcurrentTransfers, o["ns/A-v"].Target)
	}
	if o["ns/B-v"].Target != 5 {
		t.Fatalf("B must not be raised before A's GPUs are released, got B=%d", o["ns/B-v"].Target)
	}
	marked := markedPods(t, c)
	if len(marked) != started {
		t.Fatalf("want %d marked donor pods, got %d", started, len(marked))
	}
	for _, p := range marked {
		if p.Labels["app"] != "A-decode" || p.Annotations[podDeletionCostAnnotation] != donorDeletionCost {
			t.Errorf("pod %s: want an A pod at deletion cost %s, got app=%s cost=%q",
				p.Name, donorDeletionCost, p.Labels["app"], p.Annotations[podDeletionCostAnnotation])
		}
		var m transferMark
		if err := json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m); err != nil || m.Receiver == "" {
			t.Errorf("pod %s: unreadable transfer mark %q", p.Name, p.Annotations[utilizationShareTransferAnnotation])
		}
	}

	// Still releasing: B stays where it is.
	o = cycle()
	if o["ns/B-v"].Target != 5 {
		t.Fatalf("B raised while A still holds its GPUs: B=%d", o["ns/B-v"].Target)
	}

	// A's pods are gone: B is raised by what A gave.
	f.current["A"] -= started
	o = cycle()
	if o["ns/B-v"].Target != 5+started {
		t.Fatalf("want B raised to %d once A released, got %d", 5+started, o["ns/B-v"].Target)
	}
}

// Negative control: the same fleet in shadow mode returns no targets and marks
// no pod.
func TestUtilizationShareShadowActuatesNothing(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	clock := time.Unix(0, 0)
	e := &Engine{Config: shadowConfig(t, selectedShadow), client: c}
	e.utilizationShare.now = func() time.Time { return clock }
	ctx, _ := observe(t)
	for range 20 {
		clock = clock.Add(30 * time.Second)
		if o := e.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets()); o != nil {
			t.Fatalf("shadow mode returned targets: %+v", o)
		}
	}
	if n := len(markedPods(t, c)); n != 0 {
		t.Fatalf("shadow mode marked %d pods", n)
	}
}

// A restarted controller reads its transfers back from the donor pods' marks:
// the donor stays lowered and its pods stay marked. Negative control: once the
// marks are older than the release timeout they are stale -- removed, and the
// donor is not lowered.
func TestUtilizationShareRestoresTransfersFromMarks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		alive bool
	}{
		{"fresh marks", 0, true},
		{"stale marks", 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFleet()
			c := sharePods(t, f)
			clock := time.Unix(0, 0)
			ctx, _ := observe(t)

			first := &Engine{Config: shadowConfig(t, activeShare), client: c}
			first.utilizationShare.now = func() time.Time { return clock }
			started := 0
			for range 20 {
				clock = clock.Add(30 * time.Second)
				o := first.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets())
				if started = 9 - o["ns/A-v"].Target; started > 0 {
					break
				}
			}
			if started == 0 || len(markedPods(t, c)) != started {
				t.Fatalf("setup: want marked donor pods, started=%d marked=%d", started, len(markedPods(t, c)))
			}

			clock = clock.Add(tc.delay)
			restarted := &Engine{Config: shadowConfig(t, activeShare), client: c}
			restarted.utilizationShare.now = func() time.Time { return clock }
			o := restarted.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets())
			want, wantMarked := 9, 0
			if tc.alive {
				want, wantMarked = 9-started, started
			}
			if got := o["ns/A-v"].Target; got != want {
				t.Fatalf("A target after restart = %d, want %d", got, want)
			}
			if n := len(markedPods(t, c)); n != wantMarked {
				t.Fatalf("marked pods after restart = %d, want %d", n, wantMarked)
			}
		})
	}
}

// The overlay sets a planned variant's decision under the optimizer's reason,
// and leaves a variant it does not plan alone.
func TestApplyUtilizationShareOverrides(t *testing.T) {
	decisions := []domain.VariantDecision{
		{Namespace: "ns", VariantName: "A-v", CurrentReplicas: 9, TargetReplicas: 9, WasLimited: true},
		{Namespace: "ns", VariantName: "Z-v", CurrentReplicas: 3, TargetReplicas: 4},
	}
	n := applyUtilizationShareOverrides(decisions, map[string]utilizationShareOverride{"ns/A-v": {Target: 8, Why: "utilization share"}})
	if n != 1 {
		t.Fatalf("applied %d, want 1", n)
	}
	a := decisions[0]
	if a.TargetReplicas != 8 || a.Action != domain.ActionScaleDown || a.WasLimited ||
		a.ReasonCategory() != domain.DecisionReasonUtilizationShare {
		t.Errorf("A = target %d action %s limited %t reason %s", a.TargetReplicas, a.Action, a.WasLimited, a.ReasonCategory())
	}
	if decisions[1].TargetReplicas != 4 {
		t.Errorf("an unplanned variant was changed: %d", decisions[1].TargetReplicas)
	}
}

// A variant held off its target by something outside the optimizer -- here a
// replica a ResourceQuota denies -- is re-anchored to what runs once the gap
// has outlasted the release timeout, and not before. A variant a transfer is
// still moving is never re-anchored: the gap is the transfer.
func TestUtilizationShareReanchorsAStuckTarget(t *testing.T) {
	tm := allocation.ShareTimings{Window: time.Minute, ReleaseTimeout: 10 * time.Minute, FillTimeout: 5 * time.Minute}
	g := allocation.ShareGroup{
		Origins:  map[string]allocation.ShareRoleOrigin{"ns/C/": {Namespace: "ns", ModelID: "C"}},
		Variants: map[string][]allocation.ShareVariant{"ns/C/": {{Name: "C-v", Current: 1}}},
	}
	variantKey := func(_, v string) string { return "ns/" + v }
	t0 := time.Unix(0, 0)

	for _, tc := range []struct {
		name     string
		inFlight bool
		want     int
	}{
		{"stuck", false, 1},
		{"a transfer explains the gap", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{}
			e.utilizationShare.desired = map[string]int{"ns/C-v": 2}
			e.utilizationShare.divergedSince = map[string]time.Time{}
			l := allocation.NewShareLedger()
			if tc.inFlight {
				l.StartFill("ns/C/", "C-v", 1, map[string]int{"ns/C/": 1}, t0, tm)
			}
			step := func(at time.Time) int {
				e.reanchorShareTargets(logr.Discard(), l, g, variantKey, at, tm)
				return e.utilizationShare.desired["ns/C-v"]
			}
			step(t0)
			if got := step(t0.Add(tm.ReleaseTimeout - time.Second)); got != 2 {
				t.Fatalf("re-anchored before the release timeout: target %d", got)
			}
			if got := step(t0.Add(tm.ReleaseTimeout)); got != tc.want {
				t.Fatalf("target after the release timeout = %d, want %d", got, tc.want)
			}
		})
	}
}

// stubOptimizer returns fixed decisions.
type stubOptimizer struct{ decisions []domain.VariantDecision }

func (s stubOptimizer) Name() string { return "stub" }
func (s stubOptimizer) Optimize(context.Context, []allocation.ModelScalingRequest, []*allocation.ResourceConstraints) []domain.VariantDecision {
	return append([]domain.VariantDecision(nil), s.decisions...)
}

// optimizeV2's decision step applies the share targets over today's decisions
// once the optimizer acts, and leaves them alone in shadow mode.
func TestDecideV2AppliesUtilizationShareTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
		active bool
	}{
		{"active", activeShare, true},
		{"shadow", selectedShadow, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFleet()
			opt := stubOptimizer{}
			for _, id := range []string{"A", "B", "C"} {
				opt.decisions = append(opt.decisions, domain.VariantDecision{Namespace: "ns", ModelID: id, VariantName: id + "-v",
					CurrentReplicas: f.current[id], TargetReplicas: f.current[id]})
			}
			clock := time.Unix(0, 0)
			e := &Engine{Config: shadowConfig(t, tc.policy), client: sharePods(t, f)}
			e.utilizationShare.now = func() time.Time { return clock }
			ctx, _ := observe(t)
			var a domain.VariantDecision
			for range 20 {
				clock = clock.Add(30 * time.Second)
				a = e.decideV2(ctx, opt, f.requests(), fullQuota(), f.scaleTargets())[0]
				if a.TargetReplicas < 9 {
					break
				}
			}
			if tc.active != (a.TargetReplicas < 9) {
				t.Fatalf("active=%t but A's decision target is %d", tc.active, a.TargetReplicas)
			}
			if tc.active != (a.ReasonCategory() == domain.DecisionReasonUtilizationShare) {
				t.Fatalf("active=%t but A's reason is %q", tc.active, a.ReasonCategory())
			}
		})
	}
}

// An active group publishes what actuation needs to be read: promised GPUs, the
// derived timings with their source, swinging per role, and each release's
// duration. A shadow group publishes none of them -- they would describe
// actuation that is not happening.
func TestUtilizationShareActivePublishesActuationSeries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
		active bool
	}{
		{"active", activeShare, true},
		{"shadow", selectedShadow, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := freshMetrics(t)
			f := newShareFleet()
			clock := time.Unix(0, 0)
			e := &Engine{Config: shadowConfig(t, tc.policy), client: sharePods(t, f)}
			e.utilizationShare.now = func() time.Time { return clock }
			ctx, _ := observe(t)
			cycle := func() map[string]utilizationShareOverride {
				clock = clock.Add(30 * time.Second)
				return e.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets())
			}
			decision.PublishSharePromised(map[string]map[string]int{}, clock)
			started := 0
			for range 20 {
				if o := cycle(); o != nil {
					if started = 9 - o["ns/A-v"].Target; started > 0 {
						break
					}
				}
			}
			if tc.active && started == 0 {
				t.Fatal("setup: no transfer started in 20 cycles")
			}
			f.current["A"] -= started
			cycle()

			want := map[bool][4]int{true: {1, 5, 3, 1}, false: {0, 0, 0, 0}}[tc.active]
			got := [4]int{
				len(family(t, reg, constants.WVAUtilizationSharePromisedGPUs)),
				len(family(t, reg, constants.WVAUtilizationShareEffectiveSeconds)),
				len(family(t, reg, constants.WVAUtilizationShareSwinging)),
				len(family(t, reg, constants.WVAUtilizationShareReleaseSeconds)),
			}
			if got != want {
				t.Fatalf("series (promised, timings, swinging, release) = %v, want %v", got, want)
			}
			if !tc.active {
				return
			}
			for _, m := range family(t, reg, constants.WVAUtilizationShareEffectiveSeconds) {
				if s := label(m, constants.LabelSource); s != "default" {
					t.Errorf("timing %s: source %q, want default (no ScaledObject in the registry)", label(m, constants.LabelParam), s)
				}
			}
			if n := family(t, reg, constants.WVAUtilizationShareReleaseSeconds)[0].GetHistogram().GetSampleCount(); n != uint64(started) {
				t.Errorf("release observations = %d, want %d", n, started)
			}
		})
	}
}

// GPUs released for a receiver and not yet held are published as promised, so
// wakes and the warm pool withhold them; a shadow pass promises nothing.
func TestUtilizationSharePublishesPromisedGPUs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
		active bool
	}{
		{"active", activeShare, true},
		{"shadow", selectedShadow, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFleet()
			clock := time.Unix(0, 0)
			e := &Engine{Config: shadowConfig(t, tc.policy), client: sharePods(t, f)}
			e.utilizationShare.now = func() time.Time { return clock }
			ctx, _ := observe(t)
			cycle := func() map[string]utilizationShareOverride {
				clock = clock.Add(30 * time.Second)
				return e.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets())
			}
			decision.PublishSharePromised(map[string]map[string]int{}, clock)
			started := 0
			for range 20 {
				if o := cycle(); o != nil {
					if started = 9 - o["ns/A-v"].Target; started > 0 {
						break
					}
				}
			}
			if tc.active && started == 0 {
				t.Fatal("setup: no transfer started in 20 cycles")
			}
			if got := decision.LatestSharePromised(clock)[""]["A100"]; got != 0 {
				t.Fatalf("promised %d before any release", got)
			}
			f.current["A"] -= started
			cycle()
			want := 0
			if tc.active {
				want = started
			}
			if got := decision.LatestSharePromised(clock)[""]["A100"]; got != want {
				t.Fatalf("promised after the release = %d, want %d", got, want)
			}
		})
	}
}

// shareEngine is an active engine on the section 5.7 fleet with its own clock.
type shareEngine struct {
	t     *testing.T
	f     *shareFleet
	c     client.Client
	e     *Engine
	clock time.Time
	ctx   context.Context
}

func newShareEngine(t *testing.T, f *shareFleet, c client.Client, clock time.Time) *shareEngine {
	t.Helper()
	se := &shareEngine{t: t, f: f, c: c, clock: clock}
	se.e = &Engine{Config: shadowConfig(t, activeShare), client: c}
	se.e.utilizationShare.now = func() time.Time { return se.clock }
	se.ctx, _ = observe(t)
	return se
}

func (se *shareEngine) cycle() map[string]utilizationShareOverride {
	se.clock = se.clock.Add(30 * time.Second)
	return se.e.evaluateUtilizationShare(se.ctx, se.f.requests(), fullQuota(), se.f.scaleTargets())
}

// untilStarted cycles until A gives, and returns how many replicas it gave.
func (se *shareEngine) untilStarted() int {
	se.t.Helper()
	for range 20 {
		if started := 9 - se.cycle()["ns/A-v"].Target; started > 0 {
			return started
		}
	}
	se.t.Fatal("setup: no transfer started in 20 cycles")
	return 0
}

func annotate(t *testing.T, c client.Client, name string, ann map[string]string) {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	p.Annotations = ann
	if err := c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
}

// A release that never lands -- a PodDisruptionBudget, a stuck finalizer -- is
// aborted: the donor's target comes back, its pods are unmarked so a restart
// cannot resurrect the transfer, and the donor backs off instead of being
// asked again at once, which would loop start/abort forever (section 6.3).
func TestUtilizationShareAbortRestoresTheDonorAndBacksOff(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	started := se.untilStarted()
	if len(markedPods(t, c)) != started {
		t.Fatalf("setup: %d marked, want %d", len(markedPods(t, c)), started)
	}
	start := se.clock
	var o map[string]utilizationShareOverride
	for range 60 { // A's pods never go: well past the release timeout
		o = se.cycle()
		if len(markedPods(t, c)) == 0 {
			break
		}
	}
	aborted := se.clock
	if n := len(markedPods(t, c)); n != 0 {
		t.Fatalf("%d donor pods still marked 30 minutes after the transfer started", n)
	}
	if o["ns/A-v"].Target != 9 {
		t.Fatalf("A target after the abort = %d, want 9 restored", o["ns/A-v"].Target)
	}
	// The back-off is at least one release timeout, which is how long the
	// aborted attempt took.
	for se.clock.Before(aborted.Add(aborted.Sub(start) - time.Minute)) {
		if o = se.cycle(); o["ns/A-v"].Target != 9 || len(markedPods(t, c)) != 0 {
			t.Fatalf("A asked to give again %s after an abort: target %d, %d marked",
				se.clock.Sub(aborted), o["ns/A-v"].Target, len(markedPods(t, c)))
		}
	}
}

// The ReplicaSet removes a not-Ready pod before it consults deletion cost, so
// a donor with one cannot be steered: no transfer starts from it and no pod is
// left marked.
func TestUtilizationShareRefusesADonorWithANotReadyPod(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "A-v-4"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	for range 20 {
		if a := se.cycle()["ns/A-v"].Target; a != 9 {
			t.Fatalf("A lowered to %d although one of its pods is not Ready", a)
		}
	}
	for _, m := range markedPods(t, c) {
		if m.Labels["app"] == "A-decode" {
			t.Fatalf("A pod %s left marked", m.Name)
		}
	}
}

// roleA and roleB are the share role keys of the fleet's models.
var (
	roleA = "ns/A/" + domain.RoleBoth
	roleB = "ns/B/" + domain.RoleBoth
)

// A mark is read back only when it is one this controller would have written:
// a tenant who can edit its own pods must not be able to steer a restart.
func TestUtilizationShareRestoreRejectsForgedMarks(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark transferMark
	}{
		{"huge transfer", transferMark{ID: "x", Donor: roleA, Receiver: roleB, DonorVariant: "A-v", ReceiverVariant: "B-v", GPUs: 500, DonorGPUs: 500}},
		{"negative transfer", transferMark{ID: "x", Donor: roleA, Receiver: roleB, DonorVariant: "A-v", ReceiverVariant: "B-v", GPUs: -4, DonorGPUs: -4}},
		{"unknown receiver", transferMark{ID: "x", Donor: roleA, Receiver: "victim/Z/" + domain.RoleBoth, DonorVariant: "A-v", ReceiverVariant: "Z-v", GPUs: 1, DonorGPUs: 1}},
		{"wrong donor variant", transferMark{ID: "x", Donor: roleA, Receiver: roleB, DonorVariant: "C-v", ReceiverVariant: "B-v", GPUs: 1, DonorGPUs: 1}},
		{"starts in the future", transferMark{ID: "x", Donor: roleA, Receiver: roleB, DonorVariant: "A-v", ReceiverVariant: "B-v", GPUs: 1, DonorGPUs: 1,
			Started: time.Unix(1<<40, 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFleet()
			c := sharePods(t, f)
			clock := time.Unix(100000, 0)
			if tc.mark.Started.IsZero() {
				tc.mark.Started = clock.Add(-time.Minute)
			}
			raw, _ := json.Marshal(tc.mark)
			annotate(t, c, "A-v-0", map[string]string{utilizationShareTransferAnnotation: string(raw), podDeletionCostAnnotation: donorDeletionCost})

			se := newShareEngine(t, f, c, clock)
			o := se.cycle()
			if o["ns/A-v"].Target != 9 || o["ns/B-v"].Target != 5 {
				t.Fatalf("a forged mark moved targets: A=%d B=%d", o["ns/A-v"].Target, o["ns/B-v"].Target)
			}
			if n := len(markedPods(t, c)); n != 0 {
				t.Fatalf("the forged mark was left on the pod")
			}
			if p := se.e.utilizationShare.ledgers; len(p) != 1 {
				t.Fatalf("want one ledger, got %d", len(p))
			}
			for _, l := range se.e.utilizationShare.ledgers {
				if n := len(l.Transfers()); n != 0 {
					t.Fatalf("forged mark restored as %d transfers", n)
				}
			}
		})
	}
	// Control: the same shape with valid fields is restored.
	f := newShareFleet()
	c := sharePods(t, f)
	clock := time.Unix(100000, 0)
	raw, _ := json.Marshal(transferMark{ID: "x", Donor: roleA, Receiver: roleB, DonorVariant: "A-v", ReceiverVariant: "B-v",
		GPUs: 1, DonorGPUs: 1, Started: clock.Add(-time.Minute)})
	annotate(t, c, "A-v-0", map[string]string{utilizationShareTransferAnnotation: string(raw), podDeletionCostAnnotation: donorDeletionCost})
	if a := newShareEngine(t, f, c, clock).cycle()["ns/A-v"].Target; a != 8 {
		t.Fatalf("control: a valid mark must be restored and lower A to 8, got %d", a)
	}

	// A reserve refill has no receiver; its mark is restored too, and one that
	// claims to move GPUs to nobody is not.
	for _, tc := range []struct {
		name string
		gpus int
		want int
	}{{"refill", 0, 8}, {"forged refill moving GPUs", 4, 9}} {
		f := newShareFleet()
		c := sharePods(t, f)
		raw, _ := json.Marshal(transferMark{ID: "r", Donor: roleA, DonorVariant: "A-v", GPUs: tc.gpus, DonorGPUs: 1,
			Started: clock.Add(-time.Minute)})
		annotate(t, c, "A-v-0", map[string]string{utilizationShareTransferAnnotation: string(raw), podDeletionCostAnnotation: donorDeletionCost})
		if a := newShareEngine(t, f, c, clock).cycle()["ns/A-v"].Target; a != tc.want {
			t.Fatalf("%s: A = %d, want %d", tc.name, a, tc.want)
		}
	}
}

// After a restart, a marked pod that is already terminating is out of
// status.replicas: the donor is not lowered again, and an abort does not raise
// it either -- it was never lowered by this controller.
func TestUtilizationShareRestoredTerminatingMarkIsNotRestoredTwice(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	first := newShareEngine(t, f, c, time.Unix(0, 0))
	if started := first.untilStarted(); started != 1 {
		t.Skipf("fixture started %d transfers; this test needs exactly one", started)
	}
	m := markedPods(t, c)[0]
	m.Finalizers = []string{"drain"}
	if err := c.Update(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	f.current["A"] = 8 // status.replicas no longer counts the terminating pod

	restarted := newShareEngine(t, f, c, first.clock)
	if a := restarted.cycle()["ns/A-v"].Target; a != 8 {
		t.Fatalf("A after restart = %d, want 8: the terminating pod is already out of the count", a)
	}
	// Every cycle, through the abort and after it: a raise right at the
	// abort would otherwise be undone by the re-anchor before a final check.
	for i := range 60 {
		if a := restarted.cycle()["ns/A-v"].Target; a > 8 {
			t.Fatalf("cycle %d: A raised to %d by the abort of a transfer that never lowered it", i, a)
		}
	}
}

// Two transfers in flight from one Deployment donor give two different pods:
// each is steered, and ending one cannot clear the other's mark.
func TestUtilizationShareConcurrentTransfersMarkDistinctPods(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	most := 0
	for i := range 15 { // inside the release timeout: nothing has ended yet
		given := 9 - se.cycle()["ns/A-v"].Target
		if marked := len(markedPods(t, c)); marked != given {
			t.Fatalf("cycle %d: A gave %d replicas but %d distinct pods are marked", i, given, marked)
		}
		most = max(most, given)
	}
	if most < 2 {
		t.Skipf("fixture never had two transfers in flight (most %d); nothing to check", most)
	}
}

// An LWS donor gives its highest-index group, every pod of it.
func TestUtilizationShareLWSDonorGivesItsHighestGroup(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, 6)
	for g := range 3 {
		for w := range 2 {
			objs = append(objs, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: fmt.Sprintf("d-%d-%d", g, w), Labels: map[string]string{
					lwsv1.SetNameLabelKey: "d", lwsv1.GroupIndexLabelKey: strconv.Itoa(g)}},
				Spec: corev1.PodSpec{NodeName: "n1"},
			})
		}
	}
	e := &Engine{client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()}
	lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "d"},
		Spec: lwsv1.LeaderWorkerSetSpec{LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{Size: ptr.To[int32](2)}}}
	pods, err := e.donorPods(context.Background(), scaletarget.NewLWSAccessor(lws), "ns")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"d-2-0", "d-2-1"}) {
		t.Fatalf("donor pods = %v, want the whole highest group d-2-*", names)
	}
}
