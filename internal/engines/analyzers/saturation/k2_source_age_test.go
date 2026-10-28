package saturation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The k2 a variant is priced on carries its source and the age of the
// measurement behind it, for wva_learned_state_k2_source and
// wva_learned_state_k2_age_seconds. The age is the WRITE age: a window is
// touched on every read so it is retained, and a fleet held up by a figure
// measured last week would otherwise read as freshly measured every cycle.
var _ = Describe("the source and age of the k2 a variant is priced on", func() {
	var (
		a     *SaturationAnalyzer
		store *capacity.Store
		ctx   context.Context
		clock time.Time
	)
	// No batch-token budget, so P3 is unavailable and a variant with nothing
	// measured falls to P4-k1.
	params := func() *capacity.EngineParams {
		return &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256}
	}
	BeforeEach(func() {
		store = capacity.NewStore()
		a = NewSaturationAnalyzer(store)
		ctx = context.Background()
		clock = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return clock }
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params()}, time.Now())
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params()}, time.Now())
	})

	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	cycle := func(aTokens int64, aQueue int, bTokens int64, bQueue int) map[string]domain.VariantCapacity {
		result, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", aTokens, 16000, aQueue, 100, 50),
			makeReplicaMetrics("pod-b", "variant-b", bTokens, 16000, bQueue, 100, 50),
		}, states))
		Expect(err).NotTo(HaveOccurred())
		clock = clock.Add(15 * time.Second)
		out := map[string]domain.VariantCapacity{}
		for _, vc := range result.VariantCapacities {
			out[vc.VariantName] = vc
		}
		return out
	}
	keyOf := func(variant string) string {
		return a.historyKey("test-model", "test-ns", variant, "H100", 1, domain.RoleBoth, 50, 5)
	}
	// ageWindow makes variant's k2 window look written `ago` -- and last
	// read then too, which the next cycle's read moves to now.
	ageWindow := func(variant string, ago time.Duration) {
		key := keyOf(variant) // historyKey takes a.mu itself
		a.mu.Lock()
		defer a.mu.Unlock()
		ra, ok := a.computeCapacityHistory[key]
		Expect(ok).To(BeTrue(), "precondition: "+variant+" has a k2 window")
		ra.ObservedAt(clock.Add(-ago))
	}

	It("reports a figure measured this cycle as P1-obs, just written", func() {
		got := cycle(4000, 6, 1000, 0)["variant-a"]
		Expect(got.K2Source).To(Equal("P1-obs"))
		Expect(got.HasK2Age).To(BeTrue(), "a measured figure has an age")
		Expect(got.K2Age).To(BeNumerically("<", time.Minute))
	})

	It("ages an own figure from when it was measured, not from when it was last read", func() {
		cycle(4000, 6, 1000, 0)
		ageWindow("variant-a", 3*time.Hour)
		cycle(1000, 0, 1000, 0) // read, and touched, every cycle from here
		got := cycle(1000, 0, 1000, 0)["variant-a"]

		Expect(got.K2Source).To(Equal("P2-hist"))
		Expect(got.HasK2Age).To(BeTrue())
		Expect(got.K2Age).To(BeNumerically(">=", 3*time.Hour),
			"reading the window made a three-hour-old figure look fresh")
	})

	It("ages a borrowed figure from when the donor measured it", func() {
		cycle(4000, 6, 1000, 0)
		ageWindow("variant-a", 2*time.Hour)
		got := cycle(1000, 0, 1000, 0)["variant-b"]

		Expect(got.K2Source).To(Equal("P2-borrow"), "precondition: variant-b borrows variant-a's k2")
		Expect(got.HasK2Age).To(BeTrue(), "a borrowed figure is a measurement, with an age")
		Expect(got.K2Age).To(BeNumerically(">=", 2*time.Hour),
			"the age is not the donor's")
	})

	It("gives no age to a figure no window holds", func() {
		// Different engines: variant-b may not borrow, so it is on k1.
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{
			EngineParams: &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 128}}, time.Now())
		cycle(4000, 6, 1000, 0)
		got := cycle(1000, 0, 1000, 0)["variant-b"]

		Expect(got.K2Source).To(Equal("P4-k1"))
		Expect(got.HasK2Age).To(BeFalse(), "k1 is no measurement, and has no age")
	})

	// k1 is 0.8 x 16000 = 12800. A saturated replica holding 14000 tokens is
	// a legitimate P1-obs reading above k1, and the replica is then priced at
	// k1: the age of a k2 that prices nothing would send an operator after
	// the wrong figure.
	It("names k1 when the k2 is at or above it, with no age", func() {
		got := cycle(14000, 6, 1000, 0)["variant-a"]

		Expect(got.Reason).To(Equal("P1-obs"), "precondition: the cycle log names the k2's source")
		Expect(got.K2Source).To(Equal("k1-bound"), "a k2 above k1 was reported as the figure the price carries")
		Expect(got.HasK2Age).To(BeFalse(), "a k2 that prices nothing was given an age")

		// The same figure from the window on a later cycle.
		got = cycle(1000, 0, 1000, 0)["variant-a"]
		Expect(got.Reason).To(Equal("P2-hist"))
		Expect(got.K2Source).To(Equal("k1-bound"))
		Expect(got.HasK2Age).To(BeFalse())
	})

	It("reports nothing for a variant with no replica reporting", func() {
		parked := []domain.VariantReplicaState{
			{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
		}
		result, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50),
		}, parked))
		Expect(err).NotTo(HaveOccurred())
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == parked[1].VariantName {
				Expect(vc.K2Source).To(BeEmpty(), "a variant priced from no replica named a k2 source")
				Expect(vc.HasK2Age).To(BeFalse())
			}
		}
	})
})
