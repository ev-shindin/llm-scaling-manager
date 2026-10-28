package steadystate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/cycleclock"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// The engine's half of the one clock: optimizeV2 pins the cycle clock once,
// and everything it learns in the cycle is stamped on that instant. The
// analyzers' own specs drive them from a fake clock; only driving the engine
// shows the pin and the wiring are there.

// tickingClock is a base clock that moves on every read, as a wall clock does
// while a cycle runs: each Now is step later than the one before.
type tickingClock struct {
	mu   sync.Mutex
	next time.Time
	step time.Duration
}

func (c *tickingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.next
	c.next = c.next.Add(c.step)
	return t
}

func (c *tickingClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

var _ clock.PassiveClock = (*tickingClock)(nil)

// oneClockFleet is one model with two variants: chat-a reports a replica,
// chat-b has a Deployment and no replica, so its capacity record is the one
// LoadFromScaleTarget files.
func oneClockFleet(t *testing.T, base clock.PassiveClock) (*Engine, map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling) {
	t.Helper()
	const namespace, modelID = "chat", "m"
	variants := []string{"chat-a", "chat-b"}
	objs := make([]client.Object, 0, len(variants))
	vas := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, len(variants))
	for _, v := range variants {
		objs = append(objs, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: v, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: utils.Ptr(int32(1)),
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "server", Image: "vllm/vllm-openai:latest",
						Args: []string{"--max-num-seqs=128"}}},
				}},
			},
			Status: appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1},
		})
		vas = append(vas, llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: v, Namespace: namespace},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ModelID:        modelID,
				MaxReplicas:    4,
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: v, APIVersion: "apps/v1"},
			},
		})
	}
	k8s := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	cfg := config.NewTestConfig()
	config.SetLearnedStateSnapshotForTest(cfg, true)
	cfg.UpdateScalingPolicyConfigForNamespace(namespace, map[string]config.ScalingPolicy{config.GlobalDefaultsKey: {}})
	src := replicaSource{modelID: modelID, pods: []string{"chat-a" + podSuffix}}
	registry := source.NewSourceRegistry()
	registry.MustRegister("prometheus", src)
	e := newEngine(k8s, k8s, clientgoscheme.Scheme, nil, registry, cfg, allocation.NewNoOpLimiter("test"), base)
	e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(src, k8s, nil, nil, variantOfPodLocator{})
	e.ScaleToZeroEnforcer = allocation.NewEnforcer(
		func(context.Context, inferenceengine.Engine, string, string, time.Duration) (float64, error) {
			return 1, nil
		}, nil)
	return e, map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{utils.GetNamespacedKey(namespace, modelID): vas}
}

// One cycle on a base clock that moves 10 s on every read: everything the
// cycle stamps is the pin, the first reading. Without the pin, or with an
// analyzer left on its own clock, each stamp is a different reading.
func TestOneClock_ACycleStampsEverythingAtItsPin(t *testing.T) {
	pin := time.Now().Truncate(time.Second).Add(-time.Hour)
	e, fleet := oneClockFleet(t, &tickingClock{next: pin, step: 10 * time.Second})

	// A record EvictionTimeout old less 5 s at the pin: the sweep keeps it if
	// it reads the pin, and evicts it if it reads any later instant.
	e.capacityStore.Update("chat", "m", "old", capacity.Record{
		EngineParams: &capacity.EngineParams{Engine: inferenceengine.EngineVLLM},
	}, pin.Add(-capacity.EvictionTimeout+5*time.Second))

	e.optimizeV2(context.Background(), fleet, nil)

	assert.Equal(t, pin, e.learnedState.lastTry, "the snapshot restore is stamped at the pin")
	assert.Equal(t, pin, e.learnedState.lastWrite, "the snapshot write is stamped at the pin")
	assert.NotNil(t, e.capacityStore.Get("chat", "m", "old"), "the store's sweep measured age at the pin")
	if rec := e.capacityStore.Get("chat", "m", "chat-b"); assert.NotNil(t, rec, "LoadFromScaleTarget filed chat-b") {
		assert.Equal(t, pin, rec.LearnedAt, "LoadFromScaleTarget stamps the pin")
	}
	if rec := e.capacityStore.Get("chat", "m", "chat-a"); assert.NotNil(t, rec, "chat-a has a record") {
		assert.Equal(t, pin, rec.LearnedAt, "the analyzer's live record stamps the pin")
	}
	lastGood := e.lastGoodAnalysis[utils.GetNamespacedKey("chat", "m")]
	if at, ok := lastGood[domain.SaturationAnalyzerName]; assert.True(t, ok, "the saturation result was informative") {
		assert.Equal(t, pin, at, "the saturation result's AnalyzedAt is the pin")
	}
}

// NewEngine hands every built-in analyzer the engine's clock, and Clock
// returns it for the one cmd/main.go registers. A pinned clock and a base
// moved an hour on shows which clock each one reads.
func TestOneClock_NewEngineSharesTheClockWithItsAnalyzers(t *testing.T) {
	pin := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	base := clocktesting.NewFakePassiveClock(pin)
	e, _ := oneClockFleet(t, base)
	require.Same(t, e.clock, e.Clock().(*cycleclock.Clock), "Clock is the engine's cycle clock")

	release := e.clock.Pin()
	defer release()
	base.SetTime(pin.Add(time.Hour))
	input := domain.AnalyzerInput{ModelID: "m", Namespace: "chat", Config: &config.ScalingPolicy{}}

	seen := map[string]bool{}
	for _, a := range e.analyzers {
		res, err := a.analyzer.Analyze(context.Background(), input)
		require.NoError(t, err, a.name)
		assert.Equal(t, pin, res.AnalyzedAt, "the %s analyzer reads the engine's clock", a.name)
		seen[a.name] = true
	}
	assert.Equal(t, map[string]bool{domain.SaturationAnalyzerName: true, domain.SLOAnalyzerName: true}, seen,
		"precondition: both built-in analyzers ran")
}

// oneValueSource answers every query with one series of value 1.
type oneValueSource struct{ ql *source.QueryList }

func (s oneValueSource) QueryList() *source.QueryList { return s.ql }

func (s oneValueSource) Refresh(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	out := make(map[string]*source.MetricResult, len(spec.Queries))
	for _, q := range spec.Queries {
		out[q] = &source.MetricResult{Values: []source.MetricValue{{Value: 1}}}
	}
	return out, nil
}

func (s oneValueSource) Get(string, map[string]string) *source.CachedValue { return nil }

// A catalog analyzer is built with the engine's clock too: liveness compares
// its AnalyzedAt with that clock (updateLivenessAndSetLive).
func TestOneClock_AnExternalAnalyzerReadsTheEngineClock(t *testing.T) {
	pin := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	base := clocktesting.NewFakePassiveClock(pin)
	src := oneValueSource{ql: source.NewQueryList()}
	registry := source.NewSourceRegistry()
	registry.MustRegister("prometheus", src)
	k8s := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		config.GlobalDefaultsKey: {AnalyzerDefinitions: config.ExternalAnalyzerCatalog{
			"ttft-slo": {Query: "q", Threshold: "0.5"},
		}},
	})
	e := &Engine{
		Config:                  cfg,
		metricsRegistry:         registry,
		externalAnalyzers:       make(map[string]domain.Analyzer),
		ReplicaMetricsCollector: collector.NewReplicaMetricsCollector(src, k8s, nil, nil, variantOfPodLocator{}),
		clock:                   cycleclock.New(base),
	}
	e.reconcileExternalAnalyzers(context.Background())
	a, ok := e.externalAnalyzers["ttft-slo"]
	require.True(t, ok, "precondition: the catalog analyzer was built")

	release := e.clock.Pin()
	defer release()
	base.SetTime(pin.Add(time.Hour))
	res, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "chat"})
	require.NoError(t, err)
	assert.Equal(t, pin, res.AnalyzedAt, "the external analyzer reads the engine's clock")
}
