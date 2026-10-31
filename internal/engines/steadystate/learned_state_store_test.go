package steadystate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// stateSpy is an analyzer double for the store alone: it exports a given
// state and records what it was asked to restore or forget.
type stateSpy struct {
	plainAnalyzer
	state    saturation.LearnedState
	restored []saturation.LearnedState
	forgot   int
	// outcomes is what TakeRestoreOutcomes reports next.
	outcomes map[string]int
	// held is every HoldLowerBounds call, in order.
	held []bool
}

func (s *stateSpy) ExportLearnedState() saturation.LearnedState { return s.state }
func (s *stateSpy) RestoreLearnedState(st saturation.LearnedState) saturation.RestoreReport {
	s.restored = append(s.restored, st)
	return saturation.RestoreReport{}
}
func (s *stateSpy) ForgetLearnedState()       { s.forgot++ }
func (s *stateSpy) HoldLowerBounds(held bool) { s.held = append(s.held, held) }
func (s *stateSpy) TakeRestoreOutcomes() map[string]int {
	out := s.outcomes
	s.outcomes = nil
	return out
}

func sampleState(k2 float64) saturation.LearnedState {
	now := time.Now()
	return saturation.LearnedState{
		K2: map[string]capacity.RollingAverageSnapshot{
			"ns|v|m|H100|1|both|short|q5": {Values: []float64{k2}, LastUsed: now, LastWritten: now},
		},
		Config: map[string]string{"ns/v": "abc123"},
	}
}

func snapshotEngine(t *testing.T, c client.Client, spy *stateSpy) *Engine {
	t.Helper()
	cfg := config.NewTestConfig()
	config.SetLearnedStateSnapshotForTest(cfg, true)
	return &Engine{Config: cfg, client: c, analyzers: testAnalyzers(spy)}
}

func storedDoc(t *testing.T, c client.Client) (learnedStateDoc, *corev1.ConfigMap) {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: config.SystemNamespace(), Name: learnedStateConfigMap}, &cm))
	var doc learnedStateDoc
	require.NoError(t, json.Unmarshal([]byte(cm.Data[learnedStateKey()]), &doc))
	return doc, &cm
}

func TestLearnedStateSnapshotIsOffByDefault(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	e := &Engine{Config: config.NewTestConfig(), client: c, analyzers: testAnalyzers(&stateSpy{state: sampleState(4000)})}
	now := time.Now()
	e.restoreLearnedState(context.Background(), now)
	e.writeLearnedState(context.Background(), now)

	var cm corev1.ConfigMap
	err := c.Get(context.Background(), client.ObjectKey{Namespace: config.SystemNamespace(), Name: learnedStateConfigMap}, &cm)
	assert.True(t, apierrors.IsNotFound(err), "a ConfigMap was written with LEARNED_STATE_SNAPSHOT unset")
}

// A restart and a leader hand-off are the same transfer: one process writes,
// another, with nothing in memory, reads what it wrote.
func TestLearnedStateSurvivesARestartOrAHandOff(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()

	old := snapshotEngine(t, c, &stateSpy{state: sampleState(4000)})
	old.restoreLearnedState(ctx, now)
	old.writeLearnedState(ctx, now)
	doc, _ := storedDoc(t, c)
	assert.Equal(t, learnedStateVersion, doc.Version)
	assert.Equal(t, config.SystemNamespace(), doc.Writer.Namespace)
	assert.NotEmpty(t, doc.Writer.Identity, "the document names the controller that wrote it")

	next := &stateSpy{}
	snapshotEngine(t, c, next).restoreLearnedState(ctx, now.Add(time.Minute))
	require.Len(t, next.restored, 1, "the new leader did not restore what the old one wrote")
	assert.Equal(t, []float64{4000}, next.restored[0].K2["ns|v|m|H100|1|both|short|q5"].Values)
	assert.Equal(t, "abc123", next.restored[0].Config["ns/v"], "the configuration identity travels with the state")
}

// Deleting the ConfigMap flushes: the controller forgets rather than writing
// what it knows straight back, which would make the deletion a no-op.
func TestDeletingTheSnapshotFlushes(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	spy := &stateSpy{state: sampleState(4000)}
	e := snapshotEngine(t, c, spy)
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	_, cm := storedDoc(t, c)
	require.NoError(t, c.Delete(ctx, cm))

	spy.state = sampleState(5000)
	e.writeLearnedState(ctx, now.Add(2*time.Minute))
	assert.Equal(t, 1, spy.forgot, "the deleted snapshot was not taken as a flush")
	var again corev1.ConfigMap
	err := c.Get(ctx, client.ObjectKey{Namespace: config.SystemNamespace(), Name: learnedStateConfigMap}, &again)
	assert.True(t, apierrors.IsNotFound(err), "the state was written straight back over the deletion")
}

// A read that fails is not an empty store: nothing is restored, and nothing is
// written over what could not be read.
func TestAForbiddenReadIsNotAnEmptyStore(t *testing.T) {
	base := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	old := snapshotEngine(t, base, &stateSpy{state: sampleState(4000)})
	old.restoreLearnedState(ctx, now)
	old.writeLearnedState(ctx, now)
	before, _ := storedDoc(t, base)

	forbidden := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == learnedStateConfigMap {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	spy := &stateSpy{state: sampleState(9000)}
	e := snapshotEngine(t, forbidden, spy)
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now.Add(2*time.Minute))
	assert.Empty(t, spy.restored)

	after, _ := storedDoc(t, base)
	assert.Equal(t, before.State.K2, after.State.K2, "a controller that could not read the snapshot overwrote it")
}

// The other half of the same rule: once the read works again -- a Role applied
// late, an API server that was briefly unreachable -- the controller restores
// FIRST. Counting the failed read as a restore would let the first write after
// it replace the stored state with whatever this process learned since it
// started.
func TestAFailedReadIsRetriedBeforeAnythingIsWritten(t *testing.T) {
	base := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	old := snapshotEngine(t, base, &stateSpy{state: sampleState(4000)})
	old.restoreLearnedState(ctx, now)
	old.writeLearnedState(ctx, now)

	denied := true
	flaky := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if denied && key.Name == learnedStateConfigMap {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	spy := &stateSpy{state: sampleState(9000)}
	e := snapshotEngine(t, flaky, spy)
	e.restoreLearnedState(ctx, now) // denied
	denied = false

	e.writeLearnedState(ctx, now.Add(2*time.Minute)) // must not write: nothing restored yet
	after, _ := storedDoc(t, base)
	assert.Equal(t, []float64{4000}, after.State.K2["ns|v|m|H100|1|both|short|q5"].Values,
		"the first write after a failed read replaced the stored state without restoring it")

	e.restoreLearnedState(ctx, now.Add(2*time.Minute))
	require.Len(t, spy.restored, 1, "the read was not retried")
}

// A document of another format version stays where it is, so a controller
// rolled back to that version still finds its own.
func TestAnotherVersionsDocumentIsLeftAlone(t *testing.T) {
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data:       map[string]string{"v0": `{"version":0}`},
	}
	c := fake.NewClientBuilder().WithObjects(foreign).Build()
	ctx := context.Background()
	now := time.Now()
	spy := &stateSpy{state: sampleState(4000)}
	e := snapshotEngine(t, c, spy)
	e.restoreLearnedState(ctx, now)
	assert.Empty(t, spy.restored, "another version's document was read as this one's")
	e.writeLearnedState(ctx, now)

	_, cm := storedDoc(t, c)
	assert.Equal(t, `{"version":0}`, cm.Data["v0"])
}

func TestASnapshotFromAnotherNamespaceIsNotRestored(t *testing.T) {
	raw, err := json.Marshal(learnedStateDoc{
		Version: learnedStateVersion, WrittenAt: time.Now(),
		Writer: learnedStateWriter{Namespace: "somewhere-else", Identity: "pod-x"},
		State:  sampleState(4000),
	})
	require.NoError(t, err)
	copied := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data:       map[string]string{learnedStateKey(): string(raw)},
	}
	spy := &stateSpy{}
	snapshotEngine(t, fake.NewClientBuilder().WithObjects(copied).Build(), spy).
		restoreLearnedState(context.Background(), time.Now())
	assert.Empty(t, spy.restored, "a document written by another namespace's controller was restored")
}

func TestAnUnchangedStateIsNotRewritten(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	e := snapshotEngine(t, c, &stateSpy{state: sampleState(4000)})
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	_, first := storedDoc(t, c)
	e.writeLearnedState(ctx, now.Add(5*time.Minute))
	_, second := storedDoc(t, c)
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion)
}

// Reading a figure moves its last-used clock every cycle; that alone is not
// worth a write a minute. The refresh writes the clocks anyway, in time for
// horizons of days.
func TestAStateThatOnlyAgedIsRewrittenOnTheRefreshNotEveryInterval(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	learned := sampleState(4000)
	spy := &stateSpy{state: learned}
	e := snapshotEngine(t, c, spy)
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	_, first := storedDoc(t, c)

	// The same figures, read since: only the last-used clock moved.
	touched := func(at time.Time) saturation.LearnedState {
		s := cloneLearnedState(learned)
		for k, w := range s.K2 {
			w.LastUsed = at
			s.K2[k] = w
		}
		return s
	}
	spy.state = touched(now.Add(5 * time.Minute))
	e.writeLearnedState(ctx, now.Add(5*time.Minute))
	_, second := storedDoc(t, c)
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion, "a clock moved by a read was a write")

	spy.state = touched(now.Add(learnedStateRefresh))
	e.writeLearnedState(ctx, now.Add(learnedStateRefresh))
	doc, third := storedDoc(t, c)
	assert.NotEqual(t, first.ResourceVersion, third.ResourceVersion, "the clocks were never refreshed")
	for _, w := range doc.State.K2 {
		assert.WithinDuration(t, now.Add(learnedStateRefresh), w.LastUsed, time.Second)
	}
}

// Another writer moving first is retried, not taken as written.
func TestAConflictIsRetriedNextInterval(t *testing.T) {
	base := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	first := snapshotEngine(t, base, &stateSpy{state: sampleState(4000)})
	first.restoreLearnedState(ctx, now)
	first.writeLearnedState(ctx, now)

	conflict := true
	racing := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if conflict {
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), nil)
			}
			return cl.Update(ctx, obj, opts...)
		},
	})
	e := snapshotEngine(t, racing, &stateSpy{state: sampleState(9000)})
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	conflict = false
	e.writeLearnedState(ctx, now.Add(learnedStateInterval))
	doc, _ := storedDoc(t, base)
	assert.Equal(t, []float64{9000}, doc.State.K2["ns|v|m|H100|1|both|short|q5"].Values,
		"a write that lost a conflict was taken as written, and not retried")
}

// A flush is noticed within an interval even when nothing new was learned:
// with the digest unchanged, nothing else would read the ConfigMap until the
// refresh.
func TestAFlushIsNoticedWithinAnIntervalWhenNothingChanged(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	spy := &stateSpy{state: sampleState(4000)}
	e := snapshotEngine(t, c, spy)
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	_, cm := storedDoc(t, c)
	require.NoError(t, c.Delete(ctx, cm))

	e.writeLearnedState(ctx, now.Add(learnedStateInterval))
	assert.Equal(t, 1, spy.forgot, "a flush with nothing new learned waited for the refresh")
}

// The store is working again once a read works, not only once a write lands:
// with nothing new learned, the next write is a refresh away.
func TestARecoveredReadClearsTheErrorWithoutAWrite(t *testing.T) {
	base := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	denied := false
	flaky := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if denied && key.Name == learnedStateConfigMap {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	e := snapshotEngine(t, flaky, &stateSpy{state: sampleState(4000)})
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)

	denied = true
	e.writeLearnedState(ctx, now.Add(learnedStateInterval))
	require.NotEmpty(t, e.learnedState.lastReadErr, "precondition: the failed read was reported")
	denied = false
	e.writeLearnedState(ctx, now.Add(2*learnedStateInterval))
	assert.Empty(t, e.learnedState.lastReadErr,
		"a read that works left the error standing until the next write; the same error again would not be logged")
}

// counterValue reads one series of a counter family by one label.
func counterValue(t *testing.T, registry *prometheus.Registry, name, label, value string) float64 {
	t.Helper()
	mfs, err := registry.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("no %s{%s=%q}", name, label, value)
	return 0
}

// The analyzer's restore outcomes reach the counter on the cycle they happen,
// not at the next write, and a failed read is counted.
func TestRestoreOutcomesAndReadErrorsAreCounted(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	base := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()

	spy := &stateSpy{state: sampleState(4000)}
	e := snapshotEngine(t, base, spy)
	e.metricsEmitter = metrics.NewMetricsEmitter()
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	spy.outcomes = map[string]int{"whole": 2, "prior": 1}
	e.writeLearnedState(ctx, now.Add(time.Second)) // inside the write interval
	assert.Equal(t, 2.0, counterValue(t, registry, constants.WVALearnedStateRestoresTotal,
		constants.LabelOutcome, "whole"))
	assert.Equal(t, 1.0, counterValue(t, registry, constants.WVALearnedStateRestoresTotal,
		constants.LabelOutcome, "prior"))

	forbidden := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
		},
	})
	denied := snapshotEngine(t, forbidden, &stateSpy{})
	denied.metricsEmitter = metrics.NewMetricsEmitter()
	denied.restoreLearnedState(ctx, now)
	assert.Equal(t, 1.0, counterValue(t, registry, constants.WVALearnedStateSnapshotErrorsTotal,
		constants.LabelOperation, metrics.LearnedStateOpRead), "a Forbidden read was not counted")

	// A document that cannot be parsed is a read that failed too.
	garbled := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data:       map[string]string{learnedStateKey(): "{not json"},
	}
	unreadable := snapshotEngine(t, fake.NewClientBuilder().WithObjects(garbled).Build(), &stateSpy{})
	unreadable.metricsEmitter = metrics.NewMetricsEmitter()
	unreadable.restoreLearnedState(ctx, now)
	assert.Equal(t, 2.0, counterValue(t, registry, constants.WVALearnedStateSnapshotErrorsTotal,
		constants.LabelOperation, metrics.LearnedStateOpRead), "an unparseable snapshot was not counted")
}

// Every other version's document counts, not only the largest.
func TestTwoOtherVersionsCountAgainstTheBoundTogether(t *testing.T) {
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data: map[string]string{
			"v0": strings.Repeat("x", learnedStateMaxData/2-50),
			"v2": strings.Repeat("y", learnedStateMaxData/2-50),
		},
	}
	c := fake.NewClientBuilder().WithObjects(foreign).Build()
	ctx := context.Background()
	now := time.Now()
	e := snapshotEngine(t, c, &stateSpy{state: sampleState(4000)})
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: config.SystemNamespace(), Name: learnedStateConfigMap}, &cm))
	total := 0
	for k, v := range cm.Data {
		total += len(k) + len(v)
	}
	assert.LessOrEqual(t, total, learnedStateMaxData, "the data went past its bound with two other versions")
	assert.NotEmpty(t, e.learnedState.lastWriteErr, "a snapshot that does not fit was not reported")

	e.writeLearnedState(ctx, now.Add(learnedStateInterval))
	assert.NotEmpty(t, e.learnedState.lastWriteErr, "the failure stays reported, once, rather than logged every minute")
}

// The API server bounds the whole object: another version's document takes
// from this one's bound.
func TestAnotherVersionsDocumentCountsAgainstTheBound(t *testing.T) {
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data:       map[string]string{"v0": strings.Repeat("x", learnedStateMaxData-100)},
	}
	c := fake.NewClientBuilder().WithObjects(foreign).Build()
	ctx := context.Background()
	now := time.Now()
	e := snapshotEngine(t, c, &stateSpy{state: sampleState(4000)})
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: config.SystemNamespace(), Name: learnedStateConfigMap}, &cm))
	assert.NotContains(t, cm.Data, learnedStateKey(), "a document written past what the other version left")
	assert.Len(t, cm.Data["v0"], learnedStateMaxData-100, "the other version's document was touched")
}

func TestTheSizeBoundLeavesOutTheLeastRecentlyUsedFirst(t *testing.T) {
	now := time.Now()
	s := saturation.LearnedState{K2: map[string]capacity.RollingAverageSnapshot{}}
	for i := range 200 {
		at := now.Add(-time.Duration(i) * time.Minute)
		s.K2[string(rune('a'+i%26))+string(rune('A'+i/26))] = capacity.RollingAverageSnapshot{
			Values: []float64{4000, 4100, 4200}, LastUsed: at, LastWritten: at,
		}
	}
	full, _, err := encodeLearnedState(s, now, 1<<30)
	require.NoError(t, err)
	raw, dropped, err := encodeLearnedState(s, now, len(full)/2)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(raw), len(full)/2)
	assert.Greater(t, dropped, 0)

	var doc learnedStateDoc
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	oldestKept := now
	for _, w := range doc.State.K2 {
		if w.LastUsed.Before(oldestKept) {
			oldestKept = w.LastUsed
		}
	}
	left := 0
	for k, w := range s.K2 {
		if _, kept := doc.State.K2[k]; kept {
			continue
		}
		left++
		assert.False(t, w.LastUsed.After(oldestKept),
			"%s, used more recently than an entry that was kept, was left out", k)
	}
	assert.Equal(t, dropped, left)
}
