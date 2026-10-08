package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// countingReader wraps a Reader and counts Gets, so the tests can assert that
// enrichment does NOT re-read on every pass. That is the whole point of the
// freshness window: the read is uncached (a cached one would be served by the
// cluster-wide informer this design removes), so every refresh is a real request.
type countingReader struct {
	client.Reader
	gets int
	err  error
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets++
	if r.err != nil {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func kedaScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding the KEDA scheme: %v", err)
	}
	return s
}

func scaledObject(name, target string, min, max *int32) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef:  &kedav1alpha1.ScaleTarget{Name: target},
			MinReplicaCount: min,
			MaxReplicaCount: max,
		},
	}
}

const (
	// testTargetMaxAge is deliberately an order of magnitude inside testTTL, the
	// same relationship DefaultTargetMaxAge has to DefaultTTL. Enrichment that is
	// not comfortably shorter than the registry TTL never runs: the entry expires
	// first — see TestEnrichmentWindowMustSitInsideTheTTL.
	testTargetMaxAge = testTTL / 10
	// testNamespace is the single namespace these tests operate in; the registry
	// keys on (namespace, name), so entries and objects must agree on it.
	testNamespace = "chat"
	// testTarget is the workload the fixture ScaledObject scales.
	testTarget = "chat-deploy"
)

func newTestEnricher(t *testing.T, now *time.Time, reg *Registry, objs ...client.Object) (*Enricher, *countingReader) {
	t.Helper()
	inner := fake.NewClientBuilder().WithScheme(kedaScheme(t)).WithObjects(objs...).Build()
	reader := &countingReader{Reader: inner}
	e := NewEnricher(reader, reg, testTargetMaxAge)
	e.now = func() time.Time { return *now }
	return e, reader
}

// TestRefreshResolvesTheTarget: the KEDA call names the ScaledObject but carries
// neither the workload to scale nor the envelope KEDA will hold it within.
func TestRefreshResolvesTheTarget(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", map[string]string{ModelIDKey: testModel})

	minR, maxR := int32(0), int32(9)
	e, _ := newTestEnricher(t, &now, reg,
		scaledObject("chat-so", testTarget, &minR, &maxR))

	e.Refresh(context.Background())

	entry, ok := reg.Get(testNamespace, "chat-so")
	if !ok {
		t.Fatal("the entry should still be registered")
	}
	if entry.Target.Name != testTarget {
		t.Errorf("scale target: have %q", entry.Target.Name)
	}
	if entry.Target.Kind != "Deployment" || entry.Target.APIVersion != "apps/v1" {
		t.Errorf("KEDA's own defaults should fill an omitted kind/apiVersion, have %s %s",
			entry.Target.APIVersion, entry.Target.Kind)
	}
	if entry.Target.MinReplicas == nil || *entry.Target.MinReplicas != 0 {
		t.Errorf("minReplicas: have %v", entry.Target.MinReplicas)
	}
	if entry.Target.MaxReplicas == nil || *entry.Target.MaxReplicas != 9 {
		t.Errorf("maxReplicas: have %v", entry.Target.MaxReplicas)
	}
	if entry.TargetAt.IsZero() {
		t.Error("the read should be dated so its staleness can be judged")
	}
}

// TestRefreshDoesNotRereadFreshEntries is the reason the freshness window exists.
// The read is uncached, so re-reading on the scale-from-zero loop's cadence would
// be one API request per variant per 100ms.
func TestRefreshDoesNotRereadFreshEntries(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", nil)

	e, reader := newTestEnricher(t, &now, reg,
		scaledObject("chat-so", testTarget, nil, nil))

	e.Refresh(context.Background())
	if reader.gets != 1 {
		t.Fatalf("the first pass must read, have %d gets", reader.gets)
	}

	for i := 0; i < 10; i++ {
		e.Refresh(context.Background())
	}
	if reader.gets != 1 {
		t.Errorf("a fresh entry must not be re-read, have %d gets", reader.gets)
	}

	now = now.Add(2 * testTargetMaxAge)
	e.Refresh(context.Background())
	if reader.gets != 2 {
		t.Errorf("a stale entry must be re-read, have %d gets", reader.gets)
	}
}

// TestRefreshKeepsTheLastTargetOnError: the workload has not changed shape just
// because one read failed, and dropping the target would take the variant out of
// the fleet on the strength of a transient API error.
func TestRefreshKeepsTheLastTargetOnError(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", nil)

	e, reader := newTestEnricher(t, &now, reg,
		scaledObject("chat-so", testTarget, nil, nil))
	e.Refresh(context.Background())

	reader.err = errors.New("apiserver unreachable")
	now = now.Add(2 * testTargetMaxAge)
	e.Refresh(context.Background())

	entry, ok := reg.Get(testNamespace, "chat-so")
	if !ok {
		t.Fatal("a failed read must not deregister the workload")
	}
	if entry.Target.Name != testTarget {
		t.Errorf("the last known target must survive a failed read, have %q", entry.Target.Name)
	}
}

// TestRefreshForgetsADeletedScaledObject: NotFound is authoritative in a way the
// absence of calls is not. Waiting out the TTL would leave a deleted workload in
// the fleet the optimizer balances against.
func TestRefreshForgetsADeletedScaledObject(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", nil)

	e, reader := newTestEnricher(t, &now, reg)
	reader.err = apierrors.NewNotFound(
		schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}, "chat-so")

	e.Refresh(context.Background())

	if _, ok := reg.Get(testNamespace, "chat-so"); ok {
		t.Error("a deleted ScaledObject must drop its entry immediately, not at TTL")
	}
	if reg.Len() != 0 {
		t.Error("Forget must remove the entry outright")
	}
}

// TestOneBadReadDoesNotStopTheOthers — refresh is a sweep, not a transaction.
func TestOneBadReadDoesNotStopTheOthers(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "a-so", nil)
	reg.Observe(testNamespace, "b-so", nil)

	// Only b exists; a is missing, so its read fails.
	e, _ := newTestEnricher(t, &now, reg,
		scaledObject("b-so", "b-deploy", nil, nil))

	e.Refresh(context.Background())

	entry, ok := reg.Get(testNamespace, "b-so")
	if !ok {
		t.Fatal("b must still be registered")
	}
	if entry.Target.Name != "b-deploy" {
		t.Errorf("b must have been enriched despite a's failure, have %q", entry.Target.Name)
	}
}

// TestSetTargetIgnoresAnUnregisteredEntry: a read can land after KEDA has stopped
// asking, and reviving the entry here would put a variant back in the fleet that
// no call justifies.
func TestSetTargetIgnoresAnUnregisteredEntry(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)

	reg.SetTarget(testNamespace, "never-registered", Target{Name: "ghost"})

	if reg.Len() != 0 {
		t.Error("SetTarget must not create an entry")
	}
}

// TestEnrichmentDiesWithItsEntry: expiry drops the target read too, so a workload
// that comes back is re-read rather than resuming against a stale envelope.
func TestEnrichmentDiesWithItsEntry(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", nil)

	minR, maxR := int32(1), int32(4)
	e, _ := newTestEnricher(t, &now, reg,
		scaledObject("chat-so", testTarget, &minR, &maxR))
	e.Refresh(context.Background())

	now = now.Add(2 * testTTL)
	reg.Snapshot() // evicts

	reg.Observe(testNamespace, "chat-so", nil)
	entry, ok := reg.Get(testNamespace, "chat-so")
	if !ok {
		t.Fatal("the workload came back and should be registered again")
	}
	if !entry.TargetAt.IsZero() || entry.Target.Name != "" {
		t.Error("a re-registered workload must start unenriched, not resume its old target")
	}
}

// TestTargetFromScaledObjectDefaultsMaxReplicas: an omitted maxReplicaCount must
// read as the ceiling KEDA will actually enforce, not as "unbounded".
func TestTargetFromScaledObjectDefaultsMaxReplicas(t *testing.T) {
	so := scaledObject("chat-so", testTarget, nil, nil)
	target := TargetFromScaledObject(so)

	if target.MaxReplicas == nil {
		t.Fatal("maxReplicas must resolve to KEDA's default, not nil")
	}
	if *target.MaxReplicas != so.GetHPAMaxReplicas() {
		t.Errorf("want KEDA's default %d, have %d", so.GetHPAMaxReplicas(), *target.MaxReplicas)
	}
}

// TestTargetFromScaledObjectWithoutScaleTargetRef must not panic; KEDA rejects
// such an object, but WVA can read one mid-write.
func TestTargetFromScaledObjectWithoutScaleTargetRef(t *testing.T) {
	so := &kedav1alpha1.ScaledObject{ObjectMeta: metav1.ObjectMeta{Namespace: "chat", Name: "so"}}
	if target := TargetFromScaledObject(so); target.Name != "" {
		t.Errorf("expected no target name, have %q", target.Name)
	}
}

// TestEnrichmentWindowMustSitInsideTheTTL pins the relationship between the two
// durations, which is not obvious and is silently destructive to get wrong.
//
// If the enrichment window is not comfortably shorter than the registry TTL, an
// entry reaches "stale enough to re-read" only around the time it reaches
// "expired", so Snapshot evicts it in the same pass that would have refreshed it
// — and on a poll trigger it is then re-registered unenriched on the next call,
// forever. The failure mode is a fleet that looks discovered and never resolves
// a scale target.
func TestEnrichmentWindowMustSitInsideTheTTL(t *testing.T) {
	if DefaultTargetMaxAge >= DefaultTTL {
		t.Fatalf("DefaultTargetMaxAge (%s) must be shorter than DefaultTTL (%s)",
			DefaultTargetMaxAge, DefaultTTL)
	}
	// A margin, not merely "shorter": several refresh passes have to fit inside
	// one TTL for a transiently failing read to have another go before the entry
	// ages out.
	if DefaultTTL < 4*DefaultTargetMaxAge {
		t.Errorf("DefaultTTL (%s) leaves too few refresh attempts per lifetime at a %s window",
			DefaultTTL, DefaultTargetMaxAge)
	}

	// And a caller cannot opt out of it: an over-long window is clamped rather
	// than honoured, so a misconfiguration degrades to "refreshes more often
	// than asked" instead of "never refreshes".
	reg := New(time.Minute)
	if got := NewEnricher(nil, reg, time.Hour).MaxAge; got > reg.TTL()/4 {
		t.Errorf("an over-long window must be clamped to TTL/4 (%s), have %s", reg.TTL()/4, got)
	}
}

// TestRefreshIsANoOpWithoutAReader guards the wiring: an Enricher built without
// an uncached reader must do nothing rather than panic.
func TestRefreshIsANoOpWithoutAReader(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", nil)

	(&Enricher{Registry: reg}).Refresh(context.Background())

	if entry, _ := reg.Get(testNamespace, "chat-so"); !entry.TargetAt.IsZero() {
		t.Error("nothing should have been enriched")
	}
}

// TestChangedTriggerMetadataInvalidatesTheTargetRead is how WVA learns about an
// edited ScaledObject without watching one.
//
// KEDA rebuilds its scaler cache when a ScaledObject's generation changes — it
// re-issues GetMetricSpec and re-opens StreamIsActive — so a call carrying
// different metadata is evidence that the OBJECT changed, not just that time
// passed. Waiting out the freshness window there would serve a stale envelope
// for no reason.
func TestChangedTriggerMetadataInvalidatesTheTargetRead(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", map[string]string{ModelIDKey: testModel})

	e, reader := newTestEnricher(t, &now, reg,
		scaledObject("chat-so", testTarget, nil, nil))
	e.Refresh(context.Background())
	if reader.gets != 1 {
		t.Fatalf("expected the first read, have %d", reader.gets)
	}

	// Same metadata, well inside the window: no re-read.
	reg.Observe(testNamespace, "chat-so", map[string]string{ModelIDKey: testModel})
	e.Refresh(context.Background())
	if reader.gets != 1 {
		t.Errorf("an unchanged trigger must not force a read, have %d", reader.gets)
	}

	// Changed metadata: KEDA only re-sends this because the object changed.
	reg.Observe(testNamespace, "chat-so", map[string]string{
		ModelIDKey: testModel, VariantCostKey: "20.0",
	})
	e.Refresh(context.Background())
	if reader.gets != 2 {
		t.Errorf("a changed trigger must force a re-read, have %d", reader.gets)
	}
}

// TestInvalidationKeepsServingTheLastEnvelope: an edit must not drop the variant
// out of the fleet while the fresh read is pending. Clearing the target would be
// a scaling gap caused by an edit that may not have touched the scale target at
// all.
func TestInvalidationKeepsServingTheLastEnvelope(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", map[string]string{ModelIDKey: testModel})

	e, _ := newTestEnricher(t, &now, reg,
		scaledObject("chat-so", testTarget, nil, nil))
	e.Refresh(context.Background())

	reg.Observe(testNamespace, "chat-so", map[string]string{
		ModelIDKey: testModel, VariantCostKey: "20.0",
	})

	entry, ok := reg.Get(testNamespace, "chat-so")
	if !ok {
		t.Fatal("the workload must stay registered across an edit")
	}
	if entry.Target.Name != testTarget {
		t.Errorf("the last known target must keep serving until the re-read lands, have %q",
			entry.Target.Name)
	}
	if entry.Fresh(now, testTargetMaxAge) {
		t.Error("but it must read as stale, so the next pass re-reads it")
	}
}

// fakeTracker records what the namespace sync hands the datastore.
type fakeTracker struct{ tracked map[string]string }

func newFakeTracker() *fakeTracker { return &fakeTracker{tracked: map[string]string{}} }

func (f *fakeTracker) NamespaceTrack(_, name, namespace string) {
	f.tracked[namespace+"/"+name] = namespace
}
func (f *fakeTracker) NamespaceUntrack(_, name, namespace string) {
	delete(f.tracked, namespace+"/"+name)
}

// TestRefreshTracksNamespaces is what replaces the ScaledObject reconciler.
//
// Namespace tracking scopes which namespaces WVA loads configuration from. It
// used to come from watching ScaledObjects — and a watch is the cluster-wide
// LIST+WATCH this design removes. The registry knows the same thing for a better
// reason: these are the namespaces WVA has actually been called about.
func TestRefreshTracksNamespaces(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "a-so", map[string]string{ModelIDKey: testModel})
	reg.Observe("batch", "b-so", map[string]string{ModelIDKey: testModel})

	e, _ := newTestEnricher(t, &now, reg)
	tracker := newFakeTracker()
	e.Tracker = tracker

	e.Refresh(context.Background())

	if _, ok := tracker.tracked[testNamespace+"/a-so"]; !ok {
		t.Error("a registered workload's namespace must be tracked")
	}
	if _, ok := tracker.tracked["batch/b-so"]; !ok {
		t.Error("every registered namespace must be tracked, not just the first")
	}
}

// TestRefreshUntracksDepartedNamespaces: a workload KEDA stops calling about must
// stop pinning its namespace, or configuration keeps loading for a namespace WVA
// no longer has anything in.
func TestRefreshUntracksDepartedNamespaces(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "a-so", map[string]string{ModelIDKey: testModel})

	e, _ := newTestEnricher(t, &now, reg)
	tracker := newFakeTracker()
	e.Tracker = tracker
	e.Refresh(context.Background())

	if len(tracker.tracked) != 1 {
		t.Fatalf("expected one tracked workload, have %d", len(tracker.tracked))
	}

	// Nobody has called about it for a whole TTL, so it is gone.
	now = now.Add(2 * testTTL)
	e.Refresh(context.Background())

	if len(tracker.tracked) != 0 {
		t.Errorf("an expired workload must release its namespace, have %v", tracker.tracked)
	}
}

// TestSyncNamespacesWithoutATrackerIsANoOp guards the optional wiring.
func TestSyncNamespacesWithoutATrackerIsANoOp(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "a-so", nil)

	e, _ := newTestEnricher(t, &now, reg)
	e.Refresh(context.Background()) // must not panic
}

// TestStreamOpenInvalidatesTheTargetRead closes the gap metadata comparison
// cannot: scaleTargetRef and min/maxReplicaCount are not carried in the trigger,
// so an edit touching only those leaves the metadata identical and would
// otherwise wait out the whole freshness window.
//
// KEDA re-opens StreamIsActive when it rebuilds its scaler cache, which it does
// on a generation change — so a fresh stream is evidence the object was edited.
func TestStreamOpenInvalidatesTheTargetRead(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	meta := map[string]string{ModelIDKey: testModel}
	reg.Observe(testNamespace, "chat-so", meta)

	e, reader := newTestEnricher(t, &now, reg,
		scaledObject("chat-so", testTarget, nil, nil))
	e.Refresh(context.Background())
	if reader.gets != 1 {
		t.Fatalf("expected the first read, have %d", reader.gets)
	}

	// Well inside the freshness window, and the metadata has NOT changed — the
	// shape of a scaleTargetRef or min/max edit.
	release := reg.Hold(testNamespace, "chat-so", meta)
	defer release()

	e.Refresh(context.Background())
	if reader.gets != 2 {
		t.Errorf("a stream re-open must force a re-read, have %d gets", reader.gets)
	}
}

// TestStreamOpenKeepsServingTheLastEnvelope: as with a metadata change, only the
// date is zeroed. An edit must not drop the variant out of the fleet while the
// fresh read is pending.
func TestStreamOpenKeepsServingTheLastEnvelope(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", nil)

	e, _ := newTestEnricher(t, &now, reg,
		scaledObject("chat-so", testTarget, nil, nil))
	e.Refresh(context.Background())

	release := reg.Hold(testNamespace, "chat-so", nil)
	defer release()

	entry, ok := reg.Get(testNamespace, "chat-so")
	if !ok {
		t.Fatal("the workload must stay registered across a reconnect")
	}
	if entry.Target.Name != testTarget {
		t.Errorf("the last known target must keep serving, have %q", entry.Target.Name)
	}
	if entry.Fresh(now, testTargetMaxAge) {
		t.Error("but it must read as stale, so the next pass re-reads it")
	}
}

// TestTargetCarriesScaledObjectLabels — the enricher is where labels enter the
// registry, and everything downstream that reads them is silent when they are
// missing (see TestScaledObjectLabelsReachTheVariant in internal/utils).
func TestTargetCarriesScaledObjectLabels(t *testing.T) {
	so := scaledObject("chat-so", testTarget, nil, nil)
	so.Labels = map[string]string{"inference.optimization/acceleratorName": "A100"}

	target := TargetFromScaledObject(so)
	if got := target.Labels["inference.optimization/acceleratorName"]; got != "A100" {
		t.Errorf("labels must be carried onto the target, got %q", got)
	}

	// Copied, not aliased: the ScaledObject is a decoded API object whose
	// lifetime is the read, while the entry outlives it.
	so.Labels["inference.optimization/acceleratorName"] = "mutated"
	if target.Labels["inference.optimization/acceleratorName"] != "A100" {
		t.Error("the target must not alias the object's label map")
	}
}

// TestTargetFromScaledObjectCarriesReleaseTiming: the scale-down window and the
// polling interval are what a release takes, and the utilization-share
// optimizer derives its timings from them. Unset, they stay nil, so the
// consumer applies the HPA's and KEDA's defaults rather than a zero.
func TestTargetFromScaledObjectCarriesReleaseTiming(t *testing.T) {
	so := scaledObject("chat-so", testTarget, nil, nil)
	if target := TargetFromScaledObject(so); target.ScaleDownWindowSeconds != nil || target.PollingIntervalSeconds != nil {
		t.Fatalf("unset timings must stay nil, have window %v polling %v",
			target.ScaleDownWindowSeconds, target.PollingIntervalSeconds)
	}

	window, polling := int32(60), int32(15)
	so.Spec.PollingInterval = &polling
	so.Spec.Advanced = &kedav1alpha1.AdvancedConfig{
		HorizontalPodAutoscalerConfig: &kedav1alpha1.HorizontalPodAutoscalerConfig{
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleDown: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: &window},
			},
		},
	}
	target := TargetFromScaledObject(so)
	if target.ScaleDownWindowSeconds == nil || *target.ScaleDownWindowSeconds != 60 {
		t.Errorf("window = %v, want 60", target.ScaleDownWindowSeconds)
	}
	if target.PollingIntervalSeconds == nil || *target.PollingIntervalSeconds != 15 {
		t.Errorf("polling = %v, want 15", target.PollingIntervalSeconds)
	}
}
