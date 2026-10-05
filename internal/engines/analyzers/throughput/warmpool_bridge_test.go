package throughput

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// A BRIDGE IS NOT ONE OF THE VARIANT'S REPLICAS, IN THIS ANALYZER EITHER.
//
// The rule is the same one saturation follows, and it has to hold here for the
// same reason: the capacity-build step computes supply as replicas x P, counting
// replicas from the scale target -- which a lent warm pool Pod is not part of. A
// bridge left in this analyzer's per-replica maths would price the counted
// replicas with a reading none of them produced.
//
// This analyzer had no notion of FromWarmPool at all, so the whole of its P was
// measured over own-plus-bridge while its counts came out of the same rows.
var _ = Describe("Throughput analyzer - warm pool bridges", func() {
	// Local rather than the suite's: those are scoped to their own Describe.
	const (
		bridgeModelID   = "default/test-model"
		bridgeNamespace = "default"
	)
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// Fresh analyzers on both sides: the analyzer keeps an observation window, so
	// running both inputs through one instance would compare a first pass with a
	// second rather than comparing the inputs.
	It("produces the same per-variant capacity whether or not a bridge is reporting", func() {
		own := makeMetrics("v1", 2, 0.20, 0.10)

		// The bridge reports under the variant it serves -- correctly, that IS
		// this variant's traffic -- and at a capacity of its own. Half the KV of
		// an ordinary replica here, standing in for the pool's lower
		// --gpu-memory-utilization, so a blend would be plainly visible.
		bridge := makeMetrics("v1", 1, 0.20, 0)[0]
		bridge.PodName = "pool-0"
		bridge.TotalKvCapacityTokens = 32768
		bridge.FromWarmPool = true

		withoutBridge, err := NewThroughputAnalyzer().Analyze(ctx, domain.AnalyzerInput{
			ModelID:        bridgeModelID,
			Namespace:      bridgeNamespace,
			ReplicaMetrics: own,
		})
		Expect(err).NotTo(HaveOccurred())

		withBridge, err := NewThroughputAnalyzer().Analyze(ctx, domain.AnalyzerInput{
			ModelID:        bridgeModelID,
			Namespace:      bridgeNamespace,
			ReplicaMetrics: append(append([]domain.ReplicaMetrics{}, own...), bridge),
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(withBridge.VariantCapacities).To(HaveLen(len(withoutBridge.VariantCapacities)))
		for i := range withBridge.VariantCapacities {
			got, want := withBridge.VariantCapacities[i], withoutBridge.VariantCapacities[i]
			Expect(got.ReplicaCount).To(Equal(want.ReplicaCount),
				"a bridge was counted as one of the variant's own replicas")
			Expect(got.PerReplicaCapacity).To(Equal(want.PerReplicaCapacity),
				"a bridge's reading moved the price of the variant's own replicas")
		}
	})
})
