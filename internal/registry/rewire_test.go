package registry

import (
	"context"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	rewireAddr  = "wva-external-scaler.wva.svc:9090"
	otherAddr   = "other-scaler.elsewhere.svc:9090"
	rewireSO    = "chat-so"
	rewireGrace = time.Minute
)

// unwiredSO is a ScaledObject created at created, with one external trigger
// per address in addrs.
func unwiredSO(created time.Time, addrs ...string) *kedav1alpha1.ScaledObject {
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         testNamespace,
			Name:              rewireSO,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: kedav1alpha1.ScaledObjectSpec{ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: testTarget}},
	}
	for _, a := range addrs {
		so.Spec.Triggers = append(so.Spec.Triggers, kedav1alpha1.ScaleTriggers{
			Type:     "external",
			Metadata: map[string]string{ScalerAddressKey: a, ModelIDKey: testModel},
		})
	}
	return so
}

// kedaHPA is the HPA KEDA builds for rewireSO, carrying External metrics with
// these names, or only the CPU default when there are none.
func kedaHPA(external ...string) *autoscalingv2.HorizontalPodAutoscaler {
	h := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "keda-hpa-" + rewireSO},
	}
	for _, n := range external {
		h.Spec.Metrics = append(h.Spec.Metrics, autoscalingv2.MetricSpec{
			Type:     autoscalingv2.ExternalMetricSourceType,
			External: &autoscalingv2.ExternalMetricSource{Metric: autoscalingv2.MetricIdentifier{Name: n}},
		})
	}
	if len(external) == 0 {
		h.Spec.Metrics = []autoscalingv2.MetricSpec{{
			Type:     autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{Name: "cpu"},
		}}
	}
	return h
}

// rewireFixture is a Rewirer over a fake cluster holding objs, on a clock the
// test moves, counting nudges.
type rewireFixture struct {
	r      *Rewirer
	c      client.Client
	now    time.Time
	nudges int
}

func newRewireFixture(t *testing.T, now time.Time, objs ...client.Object) *rewireFixture {
	t.Helper()
	s := kedaScheme(t)
	if err := autoscalingv2.AddToScheme(s); err != nil {
		t.Fatalf("adding autoscaling/v2: %v", err)
	}
	f := &rewireFixture{now: now}
	f.c = fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	f.r = NewRewirer(f.c, f.c)
	f.r.Grace = rewireGrace
	f.r.Backoff = time.Minute
	f.r.MaxBackoff = 4 * time.Minute
	f.r.now = func() time.Time { return f.now }
	f.r.Nudged = func(string) { f.nudges++ }
	return f
}

// check reads rewireSO back from the fake cluster, as the Enricher does, and
// hands it to the Rewirer with an entry whose metadata dials rewireAddr.
func (f *rewireFixture) check(t *testing.T) {
	t.Helper()
	so := f.read(t)
	entry := Entry{Namespace: testNamespace, Name: rewireSO,
		Metadata: map[string]string{ScalerAddressKey: rewireAddr, ModelIDKey: testModel}}
	f.r.Check(context.Background(), entry, so)
}

func (f *rewireFixture) read(t *testing.T) *kedav1alpha1.ScaledObject {
	t.Helper()
	var so kedav1alpha1.ScaledObject
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: rewireSO}, &so); err != nil {
		t.Fatalf("reading the ScaledObject: %v", err)
	}
	return &so
}

// rewireValues returns each trigger's RewireKey value, "" where unset.
func (f *rewireFixture) rewireValues(t *testing.T) []string {
	t.Helper()
	so := f.read(t)
	out := make([]string, len(so.Spec.Triggers))
	for i, tr := range so.Spec.Triggers {
		out[i] = tr.Metadata[RewireKey]
	}
	return out
}

// TestRewireLeavesAWiredHPAAlone is the negative control: an HPA that carries
// the trigger's External metric is never patched.
func TestRewireLeavesAWiredHPAAlone(t *testing.T) {
	now := time.Now()
	f := newRewireFixture(t, now, unwiredSO(now.Add(-10*time.Minute), rewireAddr), kedaHPA("s0-wva"))
	f.check(t)
	if v := f.rewireValues(t); v[0] != "" || f.nudges != 0 {
		t.Fatalf("a wired HPA was re-wired: %s=%q, %d nudges", RewireKey, v[0], f.nudges)
	}
}

// TestRewirePatchesAnUnwiredHPA: KEDA left the HPA on the CPU default, so the
// trigger naming this scaler gets a new RewireKey, and only that trigger.
func TestRewirePatchesAnUnwiredHPA(t *testing.T) {
	now := time.Now()
	f := newRewireFixture(t, now, unwiredSO(now.Add(-10*time.Minute), otherAddr, rewireAddr), kedaHPA())
	f.check(t)
	v := f.rewireValues(t)
	if v[1] == "" {
		t.Fatalf("the trigger naming this scaler was not re-wired: %v", v)
	}
	if v[0] != "" {
		t.Errorf("a trigger naming another scaler was patched: %v", v)
	}
	if f.nudges != 1 {
		t.Errorf("nudges: have %d, want 1", f.nudges)
	}
}

// TestRewirePatchesOnlyTheUnwiredTrigger: with two triggers on this scaler,
// the one whose metric is on the HPA is left alone.
func TestRewirePatchesOnlyTheUnwiredTrigger(t *testing.T) {
	now := time.Now()
	f := newRewireFixture(t, now, unwiredSO(now.Add(-10*time.Minute), rewireAddr, rewireAddr), kedaHPA("s0-wva"))
	f.check(t)
	if v := f.rewireValues(t); v[0] != "" || v[1] == "" {
		t.Fatalf("want only trigger 1 re-wired, have %v", v)
	}
}

// TestRewireWaitsOutTheGrace: KEDA builds the HPA and asks for the metric spec
// within seconds of a create; a young ScaledObject is not judged yet.
func TestRewireWaitsOutTheGrace(t *testing.T) {
	now := time.Now()
	f := newRewireFixture(t, now, unwiredSO(now.Add(-rewireGrace/2), rewireAddr), kedaHPA())
	f.check(t)
	if v := f.rewireValues(t); v[0] != "" || f.nudges != 0 {
		t.Fatalf("a ScaledObject inside the grace was re-wired: %v, %d nudges", v, f.nudges)
	}
	f.now = now.Add(rewireGrace)
	f.check(t)
	if v := f.rewireValues(t); v[0] == "" {
		t.Fatalf("past the grace it should be re-wired: %v", v)
	}
}

// TestRewireBacksOff: a KEDA that keeps failing is nudged at most once per
// backoff, which doubles up to MaxBackoff.
func TestRewireBacksOff(t *testing.T) {
	now := time.Now()
	f := newRewireFixture(t, now, unwiredSO(now.Add(-10*time.Minute), rewireAddr), kedaHPA())
	// at is the time since the first check.
	steps := []struct {
		at   time.Duration
		want int
	}{
		{0, 1},
		{30 * time.Second, 1}, // inside the first backoff (1m)
		{time.Minute, 2},      // first backoff over; the next is 2m
		{2 * time.Minute, 2},  // 1m into the second
		{3 * time.Minute, 3},  // second over; the next is 4m
		{6 * time.Minute, 3},  // 3m into the third
		{7 * time.Minute, 4},  // third over; 8m capped at 4m
		{10 * time.Minute, 4}, // 3m into the capped one
		{11 * time.Minute, 5}, // capped one over
	}
	for i, s := range steps {
		f.now = now.Add(s.at)
		f.check(t)
		if f.nudges != s.want {
			t.Fatalf("step %d (at +%s): %d nudges, want %d", i, s.at, f.nudges, s.want)
		}
	}
}

// TestRewireForgetsTheBackoffOnceWired: a ScaledObject KEDA wired starts over.
func TestRewireForgetsTheBackoffOnceWired(t *testing.T) {
	now := time.Now()
	f := newRewireFixture(t, now, unwiredSO(now.Add(-10*time.Minute), rewireAddr), kedaHPA())
	f.check(t)
	if f.nudges != 1 {
		t.Fatalf("nudges: have %d, want 1", f.nudges)
	}
	// KEDA wires it, and later loses it again (its operator restarted while
	// the scaler was down).
	wired := kedaHPA("s0-wva")
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := f.c.Get(context.Background(), client.ObjectKeyFromObject(wired), hpa); err != nil {
		t.Fatal(err)
	}
	hpa.Spec.Metrics = wired.Spec.Metrics
	if err := f.c.Update(context.Background(), hpa); err != nil {
		t.Fatal(err)
	}
	f.now = now.Add(10 * time.Second)
	f.check(t)
	hpa.Spec.Metrics = kedaHPA().Spec.Metrics
	if err := f.c.Update(context.Background(), hpa); err != nil {
		t.Fatal(err)
	}
	f.now = now.Add(20 * time.Second)
	f.check(t)
	if f.nudges != 2 {
		t.Fatalf("a ScaledObject unwired again after being wired should be re-wired at once: %d nudges, want 2", f.nudges)
	}
}

// TestRewireLeavesAnotherScalersScaledObjectAlone: the registry entry dials
// this scaler, but the ScaledObject's triggers name another one.
func TestRewireLeavesAnotherScalersScaledObjectAlone(t *testing.T) {
	now := time.Now()
	f := newRewireFixture(t, now, unwiredSO(now.Add(-10*time.Minute), otherAddr), kedaHPA())
	f.check(t)
	if v := f.rewireValues(t); v[0] != "" || f.nudges != 0 {
		t.Fatalf("a ScaledObject of another scaler was patched: %v, %d nudges", v, f.nudges)
	}
}

// TestRewireWithoutAnHPA: KEDA has not built the HPA (or the ScaledObject is
// paused); nothing to judge, nothing patched.
func TestRewireWithoutAnHPA(t *testing.T) {
	now := time.Now()
	f := newRewireFixture(t, now, unwiredSO(now.Add(-10*time.Minute), rewireAddr))
	f.check(t)
	if v := f.rewireValues(t); v[0] != "" || f.nudges != 0 {
		t.Fatalf("re-wired without an HPA: %v, %d nudges", v, f.nudges)
	}
}

// TestRewireSkipsAPausedScaledObject: KEDA is told not to scale it.
func TestRewireSkipsAPausedScaledObject(t *testing.T) {
	now := time.Now()
	so := unwiredSO(now.Add(-10*time.Minute), rewireAddr)
	so.Annotations = map[string]string{kedav1alpha1.PausedReplicasAnnotation: "0"}
	f := newRewireFixture(t, now, so, kedaHPA())
	f.check(t)
	if v := f.rewireValues(t); v[0] != "" || f.nudges != 0 {
		t.Fatalf("a paused ScaledObject was re-wired: %v, %d nudges", v, f.nudges)
	}
}

// TestRefreshRewires: the Enricher hands each ScaledObject it reads to its
// Rewirer, so the re-wire runs on the enrichment cadence.
func TestRefreshRewires(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, rewireSO, map[string]string{ScalerAddressKey: rewireAddr, ModelIDKey: testModel})
	f := newRewireFixture(t, now, unwiredSO(now.Add(-10*time.Minute), rewireAddr), kedaHPA())
	e := NewEnricher(f.c, reg, testTargetMaxAge)
	e.now = func() time.Time { return now }
	e.Rewirer = f.r

	e.Refresh(context.Background())

	if v := f.rewireValues(t); v[0] == "" || f.nudges != 1 {
		t.Fatalf("Refresh did not re-wire an unwired ScaledObject: %v, %d nudges", v, f.nudges)
	}
}
