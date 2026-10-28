package saturation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The ITL window and baseline follow the engine arguments, as the k2 and
// throughput windows do.

// A change of arguments drops the ITL window and baseline learned under the
// old ones: the line is a property of the engine, and the old engine's priced
// the new one's service rate.
func TestAChangeOfArgumentsDropsTheITLBaseline(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	const key = "test-ns|test-model|decode-a|H100|1"
	a.itlUnderArgs(key, "args-a")
	a.itlTracker.SetBaseline(key, 0.011)
	a.itlUnderArgs(key, "args-a")
	if _, ok := a.itlBaselines()[key]; !ok {
		t.Fatal("precondition: the same arguments keep the baseline")
	}
	a.itlUnderArgs(key, "args-b")
	if b, ok := a.itlBaselines()[key]; ok {
		t.Errorf("the baseline learned under the old arguments survived the change: %v", b)
	}
}

// Mid-rollout the ITL window takes no readings: one may be an old pod's.
func TestTheITLWindowTakesNoReadingsMidRollout(t *testing.T) {
	window := func(rolling bool) int {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", "decode-a", capacity.Record{
			EngineParams: mtParams(256, "vllm:v1"), RollingOut: rolling,
		}, time.Now())
		a := NewSaturationAnalyzer(store)
		rows := make([]domain.ReplicaMetrics, 0, 10)
		for i := range 10 {
			k := 0.20 + 0.04*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("pod-%d", i), "decode-a", int64(k*16000), 16000, 0, 100, 50)
			rm.KvUsageInstant = k
			rm.AvgITL = 0.01 + 0.02*k
			rows = append(rows, rm)
		}
		in := makeAnalyzerInput(rows, []domain.VariantReplicaState{{
			VariantName: "decode-a", AcceleratorName: "H100", CurrentReplicas: 10, GPUsPerReplica: 1, Role: domain.RoleDecode,
		}})
		if _, err := a.Analyze(context.Background(), in); err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		return a.itlWindowLen(a.itlWindowKey("test-ns", "test-model", "decode-a", "H100", 1))
	}
	if n := window(false); n == 0 {
		t.Fatal("precondition: outside a rollout the window takes the readings")
	}
	if n := window(true); n != 0 {
		t.Errorf("mid-rollout the ITL window took %d readings", n)
	}
}

// A rollback to the arguments a baseline was learned under gets it back: the
// slope refits within a window, the baseline only on a full fit.
func TestARollbackGetsTheITLBaselineBack(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	const key = "test-ns|test-model|decode-a|H100|1"
	a.itlUnderArgs(key, "args-a")
	a.itlTracker.SetBaseline(key, 0.0012)
	a.itlUnderArgs(key, "args-b")
	a.itlUnderArgs(key, "args-a")
	if b, ok := a.itlBaselines()[key]; !ok || b != 0.0012 {
		t.Errorf("the rollback lost the baseline its arguments measured: %v %v", b, ok)
	}
}

// The cycle checks the ITL state against the arguments the variant runs.
func TestACycleDropsTheITLBaselineOfArgumentsTheVariantLeft(t *testing.T) {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "decode-a", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, time.Now())
	a := NewSaturationAnalyzer(store)
	in := makeAnalyzerInput([]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "decode-a", 4000, 16000, 0, 100, 50)},
		[]domain.VariantReplicaState{{VariantName: "decode-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode}})
	key := a.itlWindowKey("test-ns", "test-model", "decode-a", "H100", 1)
	if _, err := a.Analyze(context.Background(), in); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	a.itlTracker.SetBaseline(key, 0.011)
	store.Update("test-ns", "test-model", "decode-a", capacity.Record{EngineParams: mtParams(64, "vllm:v1")}, time.Now())
	if _, err := a.Analyze(context.Background(), in); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if b, ok := a.itlBaselines()[key]; ok {
		t.Errorf("the cycle kept the baseline of the arguments the variant left: %v", b)
	}
}
