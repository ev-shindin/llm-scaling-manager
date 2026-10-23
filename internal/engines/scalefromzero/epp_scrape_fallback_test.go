package scalefromzero

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/pod"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// failingEPPSource is the real pod-scraping source over one Ready EPP pod whose
// metrics endpoint answers 500, as the EPP did in the run that found this.
func failingEPPSource(t *testing.T, ctx context.Context, ns string) *pod.PodScrapingSource {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	var port int32
	_, err := fmt.Sscanf(server.URL, "http://127.0.0.1:%d", &port)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	selector := map[string]string{"app": "epp"}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "epp", Namespace: ns},
			Spec:       corev1.ServiceSpec{Selector: selector},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "epp-0", Namespace: ns, Labels: selector},
			Status: corev1.PodStatus{
				PodIP:      "127.0.0.1",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		},
	).Build()

	src, err := pod.NewPodScrapingSource(ctx, k8s, pod.PodScrapingSourceConfig{
		ServiceName:      "epp",
		ServiceNamespace: ns,
		MetricsPort:      port,
		ScrapeTimeout:    time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(src.Close)
	return src
}

// TestEveryEPPScrapeFailingWakesThroughThePrometheusFallback wires the real
// pod-scraping source, not a stub with a synthetic error, into the wake path.
// Every EPP pod answers 500 and the queue is in Prometheus: the model must wake
// once the fallback threshold is reached, and the pool must not be reported as
// exporting no queue. The real source used to return an empty result with a nil
// error here, so the fallback never engaged and the model stayed parked behind
// a no-wake-signal reason that pointed at the EPP's configuration instead of at
// the HTTP 500.
func TestEveryEPPScrapeFailingWakesThroughThePrometheusFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))

	f := newWakeFixture(t, "eppdown")
	ds := f.engine.Datastore.(stubSourceDatastore)
	ds.src = failingEPPSource(t, ctx, f.ns)
	f.engine.Datastore = ds
	prom := newFakeQueueSource([]source.MetricValue{queued(4, f.model, time.Second)}, nil)
	f.engine.QueueFallback = NewQueueFallback(prom)

	for i := 1; i < f.engine.QueueFallback.Threshold; i++ {
		require.Error(t, f.engine.optimize(ctx),
			"scrape %d of every EPP pod failed; below the threshold that must surface", i)
		_, published, _ := f.woken()
		require.False(t, published, "woken below the fallback threshold")
	}
	require.NoError(t, f.engine.optimize(ctx), "at the threshold the fallback answers")

	replicas, published, held := f.woken()
	require.True(t, published, "a queue of 4 in Prometheus must wake the model while every EPP scrape fails")
	assert.Equal(t, int32(1), replicas)
	assert.True(t, held)
	assert.Equal(t, 1, prom.refreshes, "the fallback read Prometheus")

	assert.NotEqual(t, constants.ScalingBlockedNoWakeSignal, wakeReasons(t, registry)[f.model],
		"a pool that could not be read is not a pool that exports no queue")
}

// noReadyEPPSource is the real pod-scraping source over an EPP Service that
// selects no Ready pod: one pod NotReady (a crash loop, a rollout, a failing
// readiness probe), or no pod at all (the EPP scaled to zero).
func noReadyEPPSource(t *testing.T, ctx context.Context, ns string, withPod bool) *pod.PodScrapingSource {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	selector := map[string]string{"app": "epp"}
	objs := []runtime.Object{&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "epp", Namespace: ns},
		Spec:       corev1.ServiceSpec{Selector: selector},
	}}
	if withPod {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "epp-0", Namespace: ns, Labels: selector},
			Status: corev1.PodStatus{
				PodIP:      "127.0.0.1",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			},
		})
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	src, err := pod.NewPodScrapingSource(ctx, k8s, pod.PodScrapingSourceConfig{
		ServiceName:      "epp",
		ServiceNamespace: ns,
		MetricsPort:      9090,
		ScrapeTimeout:    time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(src.Close)
	return src
}

// TestNoReadyEPPPodWakesThroughThePrometheusFallback is the zero-Ready case of
// the test above. With no Ready EPP pod there is nothing to scrape, which is a
// failure to look, not an EPP that exports no queue. The source used to return
// an empty answer with a nil error, so Prometheus was never asked, no error was
// counted, and the model stayed parked behind no-wake-signal.
//
// Waking on the Prometheus queue here is deliberate. With no EPP serving, the
// last sample can be up to DefaultFallbackMaxAge (90 s) old. A wrong wake
// costs one replica until scale to zero parks it again; a missed wake parks a
// model with real demand for as long as the EPP stays down.
func TestNoReadyEPPPodWakesThroughThePrometheusFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		withPod bool
	}{
		{"every EPP pod NotReady", true},
		{"no EPP pod at all", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			registry := prometheus.NewRegistry()
			require.NoError(t, metrics.InitMetrics(registry))

			f := newWakeFixture(t, "eppnoready")
			ds := f.engine.Datastore.(stubSourceDatastore)
			ds.src = noReadyEPPSource(t, ctx, f.ns, tc.withPod)
			f.engine.Datastore = ds
			prom := newFakeQueueSource([]source.MetricValue{queued(4, f.model, time.Second)}, nil)
			f.engine.QueueFallback = NewQueueFallback(prom)

			for i := 1; i < f.engine.QueueFallback.Threshold; i++ {
				require.Error(t, f.engine.optimize(ctx),
					"tick %d found no Ready EPP pod; below the threshold that must surface", i)
			}
			require.NoError(t, f.engine.optimize(ctx), "at the threshold the fallback answers")

			_, published, _ := f.woken()
			require.True(t, published, "a queue of 4 in Prometheus must wake the model while no EPP pod is Ready")
			assert.Equal(t, 1, prom.refreshes, "the fallback read Prometheus")
			assert.NotEqual(t, constants.ScalingBlockedNoWakeSignal, wakeReasons(t, registry)[f.model],
				"a pool with no Ready EPP pod is not a pool that exports no queue")
		})
	}
}
