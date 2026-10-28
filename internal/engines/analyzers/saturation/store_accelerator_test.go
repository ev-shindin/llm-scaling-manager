package saturation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The capacity store files records under the accelerator, and readers look them
// up by it. On a fleet whose pods sit on two products, discovery reads the
// variant as unresolved, and that flips with placement; the learned-state keys
// absorb the flip through the accelerator memo (stableAccelerator), and these
// specs pin that the store does too, on both sides.
var _ = Describe("the capacity store's accelerator", func() {
	var (
		a     *SaturationAnalyzer
		store *capacity.Store
		ctx   context.Context
	)
	params := &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256}
	BeforeEach(func() {
		store = capacity.NewStore()
		a = NewSaturationAnalyzer(store)
		ctx = context.Background()
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params}, time.Now())
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params}, time.Now())
		// Both variants once ran on H100, so each has its memo.
		Expect(a.stableAccelerator("test-ns", "variant-a", "H100")).To(Equal("H100"))
		Expect(a.stableAccelerator("test-ns", "variant-b", "H100")).To(Equal("H100"))
	})

	It("files a live record under the stable accelerator when this cycle's reading is unresolved", func() {
		_, err := a.Analyze(ctx, makeAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 0, 100, 50)},
			[]domain.VariantReplicaState{{VariantName: "variant-a", AcceleratorName: "", CurrentReplicas: 1, GPUsPerReplica: 1}},
		))
		Expect(err).NotTo(HaveOccurred())
		rec := store.Get("test-ns", "test-model", "variant-a")
		Expect(rec).NotTo(BeNil())
		Expect(rec.LearnedFrom).To(Equal(capacity.LearnedFromLive), "precondition: the cycle wrote a live record")
		Expect(rec.AcceleratorName).To(Equal("H100"),
			"filed under this cycle's unresolved reading, the record is missed by every lookup by product")
	})

	It("finds a sibling's record when the looking variant's own reading is unresolved", func() {
		// variant-a serves on H100 and writes a live record; variant-b is at
		// zero replicas and, with no Pods to observe its product from, reads
		// unresolved. Its lookup must use the product it was learned on.
		_, err := a.Analyze(ctx, makeReusingAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 0, 100, 50)},
			[]domain.VariantReplicaState{
				{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
		))
		Expect(err).NotTo(HaveOccurred())
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params}, time.Now()) // no capacity of its own

		Expect(a.lookupCompatibleCapacity("test-ns", "test-model", "variant-b",
			a.StableAcceleratorFor("test-ns", "variant-b", ""), 1, false)).NotTo(BeNil(),
			"precondition: looked up by the product, the sibling's record is there")

		result, err := a.Analyze(ctx, makeReusingAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 0, 100, 50)},
			[]domain.VariantReplicaState{
				{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				{VariantName: "variant-b", AcceleratorName: "", CurrentReplicas: 0, GPUsPerReplica: 1},
			},
		))
		Expect(err).NotTo(HaveOccurred())
		var b *domain.VariantCapacity
		for i := range result.VariantCapacities {
			if result.VariantCapacities[i].VariantName == "variant-b" {
				b = &result.VariantCapacities[i]
			}
		}
		Expect(b).NotTo(BeNil())
		Expect(b.PerReplicaCapacity).To(BeNumerically(">", 0),
			"looked up by its unresolved reading, variant-b missed the sibling's record and was priced at nothing")
	})

})
