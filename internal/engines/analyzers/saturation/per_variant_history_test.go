package saturation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Two variants of one model on the same hardware, in the same role and output
// bucket: every field the old key had is equal, so the old key gave them one
// window. These specs pin that each now learns only from itself.
var _ = Describe("k2 and throughput windows are per variant", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
		states   []domain.VariantReplicaState
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(capacity.NewStore())
		ctx = context.Background()
		states = []domain.VariantReplicaState{
			{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		}
	})

	capacityOf := func(result *domain.AnalyzerResult, variant string) float64 {
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == variant {
				return vc.PerReplicaCapacity
			}
		}
		Fail("no capacity reported for " + variant)
		return 0
	}

	It("does not let a sibling's higher k2 raise a variant's capacity", func() {
		// k1 = 0.8 x 16000 = 12800 for both, so neither reading is clamped
		// and the k2 each one reads back is what decides its capacity.
		By("variant-b saturating at 4000 tokens: its own window")
		_, err := analyzeConfirmed(ctx, analyzer, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50),
			makeReplicaMetrics("pod-b", "variant-b", 4000, 16000, 6, 100, 50),
		}, states))
		Expect(err).NotTo(HaveOccurred())

		By("variant-a saturating at 12000 tokens: a larger batch on the same model and card")
		_, err = analyzeConfirmed(ctx, analyzer, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", 12000, 16000, 6, 100, 50),
			makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50),
		}, states))
		Expect(err).NotTo(HaveOccurred())

		By("both idle: each reads its history back")
		result, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50),
			makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50),
		}, states))
		Expect(err).NotTo(HaveOccurred())

		// One shared window would hold {4000, 12000} and give both 8000:
		// variant-b would claim twice the KV it was measured holding when
		// saturated, and order half the replicas.
		Expect(capacityOf(result, "variant-b")).To(Equal(float64(4000)),
			"variant-b read a k2 that variant-a measured")
		Expect(capacityOf(result, "variant-a")).To(Equal(float64(12000)),
			"variant-a read a k2 that variant-b measured")
	})

	It("does not let a P1 observation return a blend with a sibling's readings", func() {
		// P1-obs returns the window's average after folding the reading in,
		// so a shared window made even a variant's OWN saturated cycle report
		// a figure partly measured on another variant.
		_, err := analyzeConfirmed(ctx, analyzer, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", 12000, 16000, 6, 100, 50),
			makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50),
		}, states))
		Expect(err).NotTo(HaveOccurred())

		result, err := analyzeConfirmed(ctx, analyzer, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50),
			makeReplicaMetrics("pod-b", "variant-b", 4000, 16000, 6, 100, 50),
		}, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(capacityOf(result, "variant-b")).To(Equal(float64(4000)))
	})

	It("does not borrow a neighbour bucket's throughput from another variant", func() {
		key := func(variant string, avgOutput float64) string {
			return analyzer.throughputKey("test-model", "test-ns", variant, "H100", 1,
				domain.RoleDecode, 100, avgOutput, 5)
		}
		long := float64(LongOutputThreshold - 1)
		xxlong := float64(ExtraLongOutputThreshold)
		analyzer.recordSaturatedThroughput(key("variant-a", long), 4.8)

		mu, bucket := analyzer.saturatedThroughputFor(key("variant-b", xxlong))
		Expect(mu).To(BeZero(), "variant-b borrowed variant-a's rate from a neighbour bucket")
		Expect(bucket).To(BeEmpty())

		By("while a variant still borrows across its OWN buckets")
		mu, bucket = analyzer.saturatedThroughputFor(key("variant-a", xxlong))
		Expect(mu).To(Equal(4.8))
		Expect(bucket).To(Equal("long"))
	})

	It("files the same reading under different keys for different variants or namespaces", func() {
		a := analyzer.historyKey("m", "ns", "variant-a", "H100", 1, domain.RoleDecode, 50, 5)
		Expect(analyzer.historyKey("m", "ns", "variant-b", "H100", 1, domain.RoleDecode, 50, 5)).NotTo(Equal(a))
		Expect(analyzer.historyKey("m", "other-ns", "variant-a", "H100", 1, domain.RoleDecode, 50, 5)).NotTo(Equal(a))
		Expect(bucketOf(a)).To(Equal("short"), "the bucket is still the second-to-last field")
	})
})
