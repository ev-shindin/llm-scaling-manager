package steadystate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	if targetOf(o, "ns/A-v") != 9 || targetOf(o, "ns/B-v") != 5 || targetOf(o, "ns/C-v") != 2 {
		t.Fatalf("quiet period must hold the running counts, got %+v", o)
	}
	if logs.FilterMessageSnippet("ledger started").Len() != 1 {
		t.Fatal("want the ledger start logged once")
	}

	started := 0
	for range 20 {
		o = cycle()
		if started = 9 - targetOf(o, "ns/A-v"); started > 0 {
			break
		}
	}
	if started < 1 || started > allocation.ShareMaxConcurrentTransfers {
		t.Fatalf("want A lowered by 1..%d once planning starts, got A=%d", allocation.ShareMaxConcurrentTransfers, targetOf(o, "ns/A-v"))
	}
	if targetOf(o, "ns/B-v") != 5 {
		t.Fatalf("B must not be raised before A's GPUs are released, got B=%d", targetOf(o, "ns/B-v"))
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
	if targetOf(o, "ns/B-v") != 5 {
		t.Fatalf("B raised while A still holds its GPUs: B=%d", targetOf(o, "ns/B-v"))
	}

	// A's pods are gone: B is raised by what A gave.
	f.current["A"] -= started
	o = cycle()
	if targetOf(o, "ns/B-v") != 5+started {
		t.Fatalf("want B raised to %d once A released, got %d", 5+started, targetOf(o, "ns/B-v"))
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
				if started = 9 - targetOf(o, "ns/A-v"); started > 0 {
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
			if got := targetOf(o, "ns/A-v"); got != want {
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
					if started = 9 - targetOf(o, "ns/A-v"); started > 0 {
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
					if started = 9 - targetOf(o, "ns/A-v"); started > 0 {
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
		if started := 9 - targetOf(se.cycle(), "ns/A-v"); started > 0 {
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
	r := freshMetrics(t)
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	started := se.untilStarted()
	if len(markedPods(t, c)) != started {
		t.Fatalf("setup: %d marked, want %d", len(markedPods(t, c)), started)
	}
	start := se.clock
	var o map[string]utilizationShareOverride
	ids := map[string]bool{}
	seen := func() {
		for _, l := range se.e.utilizationShare.ledgers {
			for _, tr := range l.Transfers() {
				ids[tr.ID] = true
			}
		}
	}
	seen()
	for range 60 { // A's pods never go: well past the release timeout
		o = se.cycle()
		seen()
		if len(markedPods(t, c)) == 0 {
			break
		}
	}
	aborted := se.clock
	if n := len(markedPods(t, c)); n != 0 {
		t.Fatalf("%d donor pods still marked 30 minutes after the transfer started", n)
	}
	if targetOf(o, "ns/A-v") != 9 {
		t.Fatalf("A target after the abort = %d, want 9 restored", targetOf(o, "ns/A-v"))
	}
	if got := counterSum(t, r, constants.WVAUtilizationShareTransfersTotal,
		map[string]string{constants.LabelOutcome: string(allocation.ShareOutcomeAborted)}); got != float64(len(ids)) {
		t.Fatalf("aborted transfers counted %v, want each of the %d that started once", got, len(ids))
	}
	// The back-off is at least one release timeout, which is how long the
	// aborted attempt took.
	for se.clock.Before(aborted.Add(aborted.Sub(start) - time.Minute)) {
		if o = se.cycle(); targetOf(o, "ns/A-v") != 9 || len(markedPods(t, c)) != 0 {
			t.Fatalf("A asked to give again %s after an abort: target %d, %d marked",
				se.clock.Sub(aborted), targetOf(o, "ns/A-v"), len(markedPods(t, c)))
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
		if a := targetOf(se.cycle(), "ns/A-v"); a != 9 {
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
			if targetOf(o, "ns/A-v") != 9 || targetOf(o, "ns/B-v") != 5 {
				t.Fatalf("a forged mark moved targets: A=%d B=%d", targetOf(o, "ns/A-v"), targetOf(o, "ns/B-v"))
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
	if a := targetOf(newShareEngine(t, f, c, clock).cycle(), "ns/A-v"); a != 8 {
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
		if a := targetOf(newShareEngine(t, f, c, clock).cycle(), "ns/A-v"); a != tc.want {
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
	if a := targetOf(restarted.cycle(), "ns/A-v"); a != 8 {
		t.Fatalf("A after restart = %d, want 8: the terminating pod is already out of the count", a)
	}
	// Every cycle, through the abort and after it: a raise right at the
	// abort would otherwise be undone by the re-anchor before a final check.
	for i := range 60 {
		if a := targetOf(restarted.cycle(), "ns/A-v"); a > 8 {
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
		given := 9 - targetOf(se.cycle(), "ns/A-v")
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

// While a transfer releases, its receiver's model carries awaiting-release on
// wva_model_scaling_blocked; switched to shadow mode, the optimizer blocks
// nothing and the reason is cleared.
func TestUtilizationSharePublishesBlockedReasons(t *testing.T) {
	reg := freshMetrics(t)
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	reasons := func() map[string][]string {
		out := map[string][]string{}
		for _, m := range family(t, reg, constants.WVAModelScalingBlocked) {
			out[label(m, constants.LabelModelName)] = append(out[label(m, constants.LabelModelName)], label(m, constants.LabelReason))
		}
		return out
	}
	if got := reasons(); !slices.Contains(got["B"], constants.ScalingBlockedAwaitingRelease) {
		t.Fatalf("want B awaiting-release while A releases, got %v", got)
	}

	setShadowPolicy(t, se.e.Config, selectedShadow)
	se.cycle()
	for model, rs := range reasons() {
		for _, r := range rs {
			if slices.Contains(constants.ScalingBlockedReasonsUtilizationShare, r) {
				t.Fatalf("shadow mode left %s blocked by %s", model, r)
			}
		}
	}
}

// The blocked reasons a group's state implies (section 9), each against the
// state that does not imply it.
func TestUtilizationShareBlockedReasonsFromGroupState(t *testing.T) {
	build := func(aHeld, budget int) map[string][]string {
		g := allocation.ShareGroup{
			Budget: budget,
			Roles: []allocation.ShareRole{
				{Key: "ns/A/both", Weight: 1, Need: 2, Floor: 8, Ceiling: 64, ReplicaGPUs: 1},
				{Key: "ns/B/both", Weight: 1, Need: 20, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
			},
			Committed:  map[string]int{"ns/A/both": aHeld, "ns/B/both": 2},
			Thresholds: map[string]float64{"ns/A/both": 0.8, "ns/B/both": 0.8},
			Origins: map[string]allocation.ShareRoleOrigin{
				"ns/A/both": {Namespace: "ns", ModelID: "A"}, "ns/B/both": {Namespace: "ns", ModelID: "B"},
			},
		}
		ev := allocation.EvaluateShare(g.Roles, g.Committed, g.Thresholds, g.Budget, 0.15)
		return shareBlockedReasons(allocation.NewShareLedger(), g, ev, nil, time.Unix(0, 0), allocation.ShareTimings{})
	}

	got := build(8, 10)
	for _, want := range []string{constants.ScalingBlockedQuotaShort, constants.ScalingBlockedDonorsAtFloor} {
		if !slices.Contains(got["ns/B"], want) {
			t.Errorf("B short with A at its floor: want %s, got %v", want, got["ns/B"])
		}
	}
	if !slices.Contains(got["ns/A"], constants.ScalingBlockedFloorPinned) {
		t.Errorf("A's floor holds 6 GPUs above its need: want floor-pinned, got %v", got["ns/A"])
	}
	if slices.Contains(got["ns/A"], constants.ScalingBlockedFloorsExceedQuota) {
		t.Errorf("floors 9 fit a budget of 10: got %v", got["ns/A"])
	}

	// Controls: A above its floor can give; floors above the budget block both.
	if got := build(9, 11); slices.Contains(got["ns/B"], constants.ScalingBlockedDonorsAtFloor) {
		t.Errorf("A holds 9 over a floor of 8, so B has a donor: got %v", got["ns/B"])
	}
	got = build(8, 8)
	for _, m := range []string{"ns/A", "ns/B"} {
		if !slices.Contains(got[m], constants.ScalingBlockedFloorsExceedQuota) {
			t.Errorf("floors 9 over a budget of 8: want floors-exceed-quota on %s, got %v", m, got[m])
		}
	}
}

// A wake that claims a releasing transfer (section 6.3) takes its GPUs: at
// release the original receiver is not raised -- the wake's own pod is
// waiting for the hole -- and the donor's mark is rewritten so a restart
// cannot hand the GPUs back to the receiver. Control: without the claim the
// receiver is raised at release (TestUtilizationShareActuatesScaleDownFirst).
func TestUtilizationShareWakeClaimRedirectsATransfer(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	decision.DefaultShareClaims.Take("", "A100") // claims from other tests
	started := se.untilStarted()

	// A parked standard model wakes: z = -1, below B's (short, weight 1).
	claims, outcome := decision.DefaultShareClaims.ClaimSet("", "A100",
		[]decision.ShareWake{{GPUs: 1, Variant: "ns/W-v"}}, -1, "ns/W", se.clock)
	if outcome != decision.ShareClaimRedirected || len(claims) != 1 {
		t.Fatalf("want the wake to claim a releasing transfer, got %q", outcome)
	}
	se.cycle() // applies the claim
	var remarked bool
	for _, p := range markedPods(t, c) {
		var m transferMark
		if json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m) == nil && m.ID == claims[0].ID {
			remarked = m.Receiver == "" && m.GPUs == 0
		}
	}
	if !remarked {
		t.Fatalf("the claimed transfer's donor mark still names its receiver")
	}

	f.current["A"] -= started
	o := se.cycle()
	if want := 5 + started - 1; targetOf(o, "ns/B-v") != want {
		t.Fatalf("B = %d after the release, want %d: the claimed replica's GPUs went to the wake", targetOf(o, "ns/B-v"), want)
	}
}

// A receiver replica of two pods, when every donor replica has one: no single
// donor can host it, and two are marked together as one donor set (section
// 6.5). The receiver is raised only once BOTH have released -- one hole of two
// is not a replica.
func TestUtilizationShareFundsAReplicaFromADonorSet(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	requests := func() []allocation.ModelScalingRequest {
		out := f.requests()
		for i := range out {
			st := &out[i].VariantStates[0]
			if out[i].ModelID == "B" {
				st.GPUsPerReplica, st.PodGPUs = 2, []int{1, 1}
			} else {
				st.PodGPUs = []int{1}
			}
		}
		return out
	}
	cycle := func() map[string]utilizationShareOverride {
		se.clock = se.clock.Add(30 * time.Second)
		return se.e.evaluateUtilizationShare(se.ctx, requests(), fullQuota(), f.scaleTargets())
	}
	var marked []corev1.Pod
	for range 20 {
		cycle()
		if marked = markedPods(t, c); len(marked) > 0 {
			break
		}
	}
	if len(marked) != 2 {
		t.Fatalf("want two donor pods marked as one set, got %d", len(marked))
	}
	sets := map[string]bool{}
	for _, p := range marked {
		var m transferMark
		if err := json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m); err != nil || m.SetID == "" {
			t.Fatalf("pod %s: mark carries no set: %q", p.Name, p.Annotations[utilizationShareTransferAnnotation])
		}
		sets[m.SetID] = true
	}
	if len(sets) != 1 {
		t.Fatalf("want both marks in one set, got %v", sets)
	}
	donorOf := func(p corev1.Pod) string { return strings.TrimSuffix(p.Labels["app"], "-decode") }

	f.current[donorOf(marked[0])]--
	if o := cycle(); targetOf(o, "ns/B-v") != 5 {
		t.Fatalf("B raised to %d with one of its two holes open", targetOf(o, "ns/B-v"))
	}
	f.current[donorOf(marked[1])]--
	if o := cycle(); targetOf(o, "ns/B-v") != 6 {
		t.Fatalf("B = %d once both donors released, want 6", targetOf(o, "ns/B-v"))
	}
}

// A donor set restores whole or not at all: a primary mark that names a
// receiver larger than what its set's donors give would book GPUs nobody gave
// up -- a debt the refill would then take from other tenants. Control: the
// same primary with a contributor that covers the receiver is restored.
func TestUtilizationShareRestoresDonorSetsWhole(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contributor bool
		wantA       int
	}{
		{"forged primary alone", false, 9},
		{"whole set", true, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFleet()
			c := sharePods(t, f)
			clock := time.Unix(100000, 0)
			mark := func(pod string, m transferMark) {
				m.Started = clock.Add(-time.Minute)
				raw, _ := json.Marshal(m)
				annotate(t, c, pod, map[string]string{utilizationShareTransferAnnotation: string(raw), podDeletionCostAnnotation: donorDeletionCost})
			}
			mark("A-v-0", transferMark{ID: "x", SetID: "x", Donor: roleA, Receiver: roleB, DonorVariant: "A-v",
				ReceiverVariant: "B-v", GPUs: 2, DonorGPUs: 1})
			if tc.contributor {
				mark("A-v-1", transferMark{ID: "y", SetID: "x", Donor: roleA, DonorVariant: "A-v", DonorGPUs: 1})
			}
			e := &Engine{Config: shadowConfig(t, activeShare), client: c}
			e.utilizationShare.now = func() time.Time { return clock }
			ctx, _ := observe(t)
			reqs := f.requests()
			for i := range reqs {
				st := &reqs[i].VariantStates[0]
				if reqs[i].ModelID == "B" {
					st.GPUsPerReplica, st.PodGPUs = 2, []int{1, 1}
				} else {
					st.PodGPUs = []int{1}
				}
			}
			o := e.evaluateUtilizationShare(ctx, reqs, fullQuota(), f.scaleTargets())
			if got := targetOf(o, "ns/A-v"); got != tc.wantA {
				t.Fatalf("A = %d after restart, want %d", got, tc.wantA)
			}
		})
	}
}
