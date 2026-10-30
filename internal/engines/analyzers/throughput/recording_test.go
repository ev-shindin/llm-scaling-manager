package throughput

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// With recording off the recorder costs a nil test: a wrapper allocates
// exactly what the method behind it does.
func TestRecordingOff_CostsNoAllocation(t *testing.T) {
	a := NewThroughputAnalyzer()
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []domain.ReplicaMetrics{{PodName: "p", VariantName: "v", KvUsageInstant: 0.5, AvgITL: 0.01}}
	in := domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ReplicaMetrics: rows}
	cases := map[string]struct{ wrapper, impl func() }{
		"Observe": {func() { a.Observe(ctx, at, "m", "ns", rows) }, func() { a.observeDefaultBand(ctx, at, "m", "ns", rows) }},
		"Analyze": {func() { _, _ = a.Analyze(ctx, in) }, func() { _, _ = a.analyze(ctx, in) }},
	}
	for name, c := range cases {
		if w, i := testing.AllocsPerRun(100, c.wrapper), testing.AllocsPerRun(100, c.impl); w != i {
			t.Errorf("%s allocates %v with recording off, the method behind it %v", name, w, i)
		}
	}
}
