package collector

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// The running batch and the preemption rate, from the query result to
// domain.ReplicaMetrics: the ITL fit reads them to take a capped replica's
// reading above its band (issue #287), and a dropped block would leave the
// band closed without a word.
func TestCollectReplicaMetrics_SequenceCap(t *testing.T) {
	podLabels := map[string]string{
		seriesModelLabel: "test-model",
		"pod":            "pod-abc",
		"instance":       "10.0.0.1:8000",
	}
	foreign := map[string]string{
		seriesModelLabel: "other-model",
		"pod":            "pod-abc",
		"instance":       "10.0.0.1:8000",
	}

	collect := func(t *testing.T, results map[string]*source.MetricResult) domain.ReplicaMetrics {
		t.Helper()
		reg := prometheus.NewRegistry()
		if err := metrics.InitMetrics(reg); err != nil {
			t.Fatalf("InitMetrics: %v", err)
		}
		scheme := runtime.NewScheme()
		if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
			t.Fatalf("AddToScheme: %v", err)
		}
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		// Makes the pod discoverable; without it there is no row to assert on.
		results["kv_cache_usage"] = &source.MetricResult{Values: []source.MetricValue{{Labels: podLabels, Value: 0.5}}}
		mockSource := &mockMetricsSource{
			refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
				return results, nil
			},
		}
		c := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil,
			scalerLocator(map[string]string{"pod-abc": "va-1"}))
		got, err := c.CollectReplicaMetrics(context.Background(), "test-model", "test-ns",
			make(map[string]scaletarget.ScaleTargetAccessor),
			make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling), nil)
		if err != nil {
			t.Fatalf("CollectReplicaMetrics: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 replica row, got %d", len(got))
		}
		return got[0]
	}
	series := func(labels map[string]string, v float64) *source.MetricResult {
		return &source.MetricResult{Values: []source.MetricValue{{Labels: labels, Value: v, Timestamp: time.Now()}}}
	}

	t.Run("each reading lands on its field", func(t *testing.T) {
		got := collect(t, map[string]*source.MetricResult{
			registration.QueryRunningRequests: series(podLabels, 256),
			registration.QueryPreemptionRate:  series(podLabels, 0.25),
		})
		if got.RunningRequests != 256 {
			t.Errorf("RunningRequests = %d, want 256", got.RunningRequests)
		}
		if got.PreemptionRate != 0.25 || !got.PreemptionRateKnown {
			t.Errorf("PreemptionRate = %v known %v, want 0.25 known", got.PreemptionRate, got.PreemptionRateKnown)
		}
	})

	t.Run("a zero rate is known, a missing or NaN one is not", func(t *testing.T) {
		if got := collect(t, map[string]*source.MetricResult{
			registration.QueryPreemptionRate: series(podLabels, 0),
		}); !got.PreemptionRateKnown {
			t.Error("a read zero must be known: it is what admits a capped reading")
		}
		if got := collect(t, map[string]*source.MetricResult{}); got.PreemptionRateKnown {
			t.Error("no series (an SGLang fleet) must not read as no preemption")
		}
		if got := collect(t, map[string]*source.MetricResult{
			registration.QueryPreemptionRate: series(podLabels, math.NaN()),
		}); got.PreemptionRateKnown {
			t.Error("a NaN rate must not read as no preemption")
		}
	})

	t.Run("a negative or infinite reading is dropped", func(t *testing.T) {
		for _, v := range []float64{-1, math.Inf(1), math.Inf(-1)} {
			got := collect(t, map[string]*source.MetricResult{
				registration.QueryRunningRequests: series(podLabels, v),
				registration.QueryPreemptionRate:  series(podLabels, v),
			})
			if got.RunningRequests != 0 {
				t.Errorf("running %v read as %d, want 0", v, got.RunningRequests)
			}
			if got.PreemptionRateKnown {
				t.Errorf("a preemption rate of %v must not be known", v)
			}
		}
	})

	t.Run("another model's preemptions are not this one's", func(t *testing.T) {
		got := collect(t, map[string]*source.MetricResult{
			registration.QueryPreemptionRate: series(foreign, 0),
		})
		if got.PreemptionRateKnown {
			t.Error("the preemption query is not narrowed to the model before extraction; the series must be matched here")
		}
	})
}

// A pod of two ranks is at its cap only with both ranks' batches added up,
// and its preemption rate is known only when both ranks reported one.
func TestMergePodInstances_SequenceCap(t *testing.T) {
	rank := func(running int, rate float64, known bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "p", ModelID: "m", RunningRequests: running,
			PreemptionRate: rate, PreemptionRateKnown: known}
	}
	got := collapseToPods([]domain.ReplicaMetrics{rank(256, 0, true), rank(250, 0.5, true)})
	if len(got) != 1 || got[0].RunningRequests != 506 || got[0].PreemptionRate != 0.5 || !got[0].PreemptionRateKnown {
		t.Fatalf("merged = %+v, want running 506, rate 0.5 known", got)
	}
	got = collapseToPods([]domain.ReplicaMetrics{rank(256, 0, true), rank(256, 0, false)})
	if got[0].PreemptionRateKnown {
		t.Error("a rank with no preemption reading must leave the pod's unknown")
	}
}

// A pod of two ranks, each at its cap, collapsed and offered to the ITL fit:
// the cap is max_num_seqs per rank, so the pod is capped at twice it, and one
// rank short of it is not the pod at its cap.
func TestSequenceCapAcrossRanks(t *testing.T) {
	const maxSeqs = 256
	rank := func(running int) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "p", ModelID: "m", VariantName: "v", Ready: true,
			TotalKvCapacityTokens: 100000, KvUsageInstant: 0.87, KvUsageInstantKnown: true,
			AvgITL: 0.08, GenerationTokenRate: 3000, RequestRate: 3,
			RunningRequests: running, QueueLength: 384, PreemptionRateKnown: true}
	}
	adm := itl.Admission{MaxNumSeqs: maxSeqs}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	pods := collapseToPods([]domain.ReplicaMetrics{rank(maxSeqs), rank(maxSeqs)})
	if len(pods) != 1 || pods[0].Ranks() != 2 {
		t.Fatalf("collapsed = %+v, want one pod of two ranks", pods)
	}
	if r := itl.NewTracker().Observe("k", "v", pods, adm, at); r.Capped != 1 || r.AboveBand != 0 {
		t.Errorf("both ranks at the cap: capped %d aboveBand %d, want 1 and 0", r.Capped, r.AboveBand)
	}
	pods = collapseToPods([]domain.ReplicaMetrics{rank(maxSeqs), rank(maxSeqs - 6)})
	if r := itl.NewTracker().Observe("k", "v", pods, adm, at); r.Capped != 0 || r.AboveBand != 1 {
		t.Errorf("one rank short of the cap: capped %d aboveBand %d, want 0 and 1", r.Capped, r.AboveBand)
	}
}
