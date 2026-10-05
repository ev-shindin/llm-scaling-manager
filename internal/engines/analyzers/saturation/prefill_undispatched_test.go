package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The hold exists so prefill is neither ordered nor released on decode's
// backlog. What it must NOT hold is the scheduler queue's share: those
// requests have been given to no pod and have had no first token, so prefill
// is exactly what they are waiting for, whatever decode is doing.
//
// Measured on run PM: prefill demand of 550,077 was clamped to 495,529 --
// scaleUp x its own supply, the figure RC = D/scaleUp - anticipated turns
// into exactly zero -- for 19 cycles, while prefill held a 600-deep queue and
// decode sat at its ceiling of 9. No amount of queued work could order a
// replica.
var _ = Describe("holdPrefillDemand and the undispatched share", func() {
	const (
		prc      = 300_000.0
		replicas = 1
	)
	// One prefill variant of one replica, so the band is
	// [scaleDown*supply, scaleUp*supply] = [210_000, 255_000].
	fleet := func() []domain.VariantCapacity {
		// Supply is derived by aggregation from ReplicaCount x
		// PerReplicaCapacity, not carried on the struct, so setting those two
		// is what fixes the band at [0.70*300k, 0.85*300k].
		return []domain.VariantCapacity{{
			VariantName:        "p",
			Role:               domain.RolePrefill,
			ReplicaCount:       replicas,
			PerReplicaCapacity: prc,
		}}
	}

	It("clamps to the band when nothing is undispatched (unchanged)", func() {
		rd := map[string]float64{domain.RolePrefill: 550_000}
		h, held := holdPrefillDemand(rd, fleet(), 0.85, 0.70, 0)
		Expect(held).To(BeTrue())
		Expect(h.after).To(BeNumerically("~", 0.85*prc, 1), "capped at scaleUp x supply, so RC is 0")
		Expect(rd[domain.RolePrefill]).To(BeNumerically("~", 0.85*prc, 1))
	})

	It("does not clamp below work no pod has started", func() {
		// 400,000 sitting in the scheduler queue is above the cap. Held at the
		// cap it orders nothing; left alone it can order, which is the point.
		rd := map[string]float64{domain.RolePrefill: 550_000}
		h, held := holdPrefillDemand(rd, fleet(), 0.85, 0.70, 400_000)
		Expect(held).To(BeTrue())
		Expect(h.after).To(BeNumerically("~", 400_000, 1))
		Expect(h.after).To(BeNumerically(">", 0.85*prc),
			"above the cap, so RC = D/scaleUp - supply is positive and prefill can be ordered")
	})

	It("still holds the part that IS decode's backlog", func() {
		// Only 100,000 is undispatched; the rest is resident KV and the engine
		// queue behind it. The band still applies, because the exemption is
		// the floor and not a bypass.
		rd := map[string]float64{domain.RolePrefill: 550_000}
		h, _ := holdPrefillDemand(rd, fleet(), 0.85, 0.70, 100_000)
		Expect(h.after).To(BeNumerically("~", 0.85*prc, 1),
			"the undispatched share is below the cap, so the cap governs")
	})

	It("never invents demand that was not there", func() {
		// A scheduler-queue estimate larger than the role's own demand cannot
		// raise it past that demand: the exemption declines to HOLD demand, it
		// does not create any.
		rd := map[string]float64{domain.RolePrefill: 120_000}
		holdPrefillDemand(rd, fleet(), 0.85, 0.70, 900_000)
		Expect(rd[domain.RolePrefill]).To(BeNumerically("<=", 210_000+1),
			"at most the band floor, never the 900,000 the queue estimated")
	})

	It("does not pull demand back under the band floor", func() {
		// The floor is there for the burst that ends an episode: the scheduler
		// releases what it held in one go and it lands on prefill first. An
		// exemption written against h.after rather than h.hi would undo that,
		// because a large undispatched figure bounded by a small `before`
		// lands below the floor.
		rd := map[string]float64{domain.RolePrefill: 120_000}
		h, held := holdPrefillDemand(rd, fleet(), 0.85, 0.70, 900_000)
		Expect(held).To(BeTrue())
		Expect(h.after).To(BeNumerically("~", 0.70*prc, 1),
			"still floored at scaleDown x supply, not dropped to the 120,000 that was there")
	})

	It("leaves a fleet with no prefill entry alone", func() {
		rd := map[string]float64{domain.RoleDecode: 1_000_000}
		_, held := holdPrefillDemand(rd, fleet(), 0.85, 0.70, 400_000)
		Expect(held).To(BeFalse())
		Expect(rd).NotTo(HaveKey(domain.RolePrefill))
	})
})
