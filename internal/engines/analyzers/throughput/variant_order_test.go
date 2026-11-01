package throughput

import (
	"context"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Analyze walks the model's variants in name order. It sums floats across
// them and returns the capacities that later code sums again, and a walk in
// map order left the last bit of each sum, and the order of the capacities,
// to chance from one cycle -- or one replay -- to the next.
var _ = Describe("Analyze variant order", func() {
	const (
		modelID   = "llama3-8b"
		namespace = "default"
		il        = 5000.0
		ol        = 200.0
		prefix    = 0.1
		kvMax     = int64(1024000)
		b         = 0.006
	)
	kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}
	variants := []string{"v-a", "v-b", "v-c", "v-d"}

	It("returns the capacities in variant-name order on every run", func() {
		ctx := context.Background()
		analyzer := NewThroughputAnalyzer()
		rows := make([]domain.ReplicaMetrics, 0, len(variants))
		for _, v := range variants {
			injectWindowObs(analyzer, ctx, modelID, namespace, v, il, ol, prefix, kvMax, b, kValues)
			rows = append(rows, domain.ReplicaMetrics{
				VariantName: v, KvCacheUsage: 0.5, KvUsageInstant: 0.5, AvgITL: 0.073*0.5 + b,
				AvgInputTokens: il, AvgOutputTokens: ol, PrefixCacheHitRate: prefix,
				TotalKvCapacityTokens: kvMax, RequestRate: 5,
			})
		}
		input := domain.AnalyzerInput{ModelID: modelID, Namespace: namespace, ReplicaMetrics: rows, ArrivalRate: 20}

		for i := range 50 {
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			names := make([]string, 0, len(result.VariantCapacities))
			for _, vc := range result.VariantCapacities {
				names = append(names, vc.VariantName)
			}
			Expect(names).To(HaveLen(len(variants)), "the premise: every variant is priced")
			Expect(slices.IsSorted(names)).To(BeTrue(), "run %d: capacities came back in %v", i, names)
		}
	})
})
