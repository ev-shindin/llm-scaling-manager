package itl

import (
	"fmt"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("Tracker on a replica at its sequence cap", func() {
	const (
		cycle   = 15 * time.Second
		maxSeqs = 256
	)
	var (
		t   *Tracker
		now time.Time
	)
	capped := Admission{MaxNumSeqs: maxSeqs}
	BeforeEach(func() {
		t = NewTracker()
		now = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	})

	// The shape of issue #287, cycle by cycle: one decode replica took all the
	// traffic, at its max_num_seqs of 256 running with 768 waiting and k
	// between 0.86 and 0.89, reading ITL 73-85 ms, while three siblings sat
	// idle below the band.
	hot := func(i int) domain.ReplicaMetrics {
		ks := []float64{0.86, 0.87, 0.89, 0.88, 0.87}
		itls := []float64{0.073, 0.077, 0.085, 0.081, 0.078}
		return domain.ReplicaMetrics{
			PodName: "decode-hot", VariantName: "v", Ready: true,
			KvUsageInstant: ks[i%len(ks)], KvUsageInstantKnown: true, AvgITL: itls[i%len(itls)],
			RunningRequests: maxSeqs, QueueLength: 768,
			PreemptionRate: 0, PreemptionRateKnown: true,
		}
	}
	idle := func(n int) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName: fmt.Sprintf("decode-idle-%d", n), VariantName: "v", Ready: true,
			KvUsageInstant: 0.004, KvUsageInstantKnown: true, AvgITL: 0.012,
			RunningRequests: 1, PreemptionRateKnown: true,
		}
	}
	// replay runs 20 cycles -- five minutes -- of the shape, with change
	// applied to the hot replica's row, and returns the last cycle's reading.
	replay := func(adm Admission, change func(*domain.ReplicaMetrics)) Reading {
		var r Reading
		for i := range 20 {
			h := hot(i)
			if change != nil {
				change(&h)
			}
			rows := []domain.ReplicaMetrics{h, idle(0), idle(1), idle(2)}
			r = t.Observe("key", "v", rows, adm, now)
			now = now.Add(cycle)
		}
		return r
	}

	It("fits a line from the capped replica's readings above the band", func() {
		r := replay(capped, nil)
		Expect(r.AboveBand).To(BeZero(), "a replica at its sequence cap is not thrashing")
		Expect(r.Capped).To(Equal(1))
		Expect(r.BelowBand).To(Equal(3), "the idle siblings still teach nothing")
		Expect(r.Tier).To(Equal(TierPinnedB), "the window must hold a line, or derived mu never exists")
		// One replica spans no k, so B is pinned and the line passes through
		// the readings: 79 ms at k 0.874, their means.
		Expect(r.Model.ITLAt(0.874)).To(BeNumerically("~", 0.079, 0.002))
	})

	It("counts the cap per rank", func() {
		r := replay(capped, func(h *domain.ReplicaMetrics) {
			h.EngineRanks = 2
			h.RunningRequests = 2 * maxSeqs
		})
		Expect(r.Tier).To(Equal(TierPinnedB))
		r = NewTracker().Observe("key", "v", []domain.ReplicaMetrics{{
			PodName: "dp", VariantName: "v", Ready: true, KvUsageInstant: 0.87, AvgITL: 0.08,
			EngineRanks: 2, RunningRequests: maxSeqs, QueueLength: 768, PreemptionRateKnown: true,
		}}, capped, now)
		Expect(r.AboveBand).To(Equal(1), "one of two ranks at its cap is not the pod at its cap")
	})

	It("takes the preemption allowance the policy sets", func() {
		r := replay(Admission{MaxNumSeqs: maxSeqs, CapMaxPreemptionRate: 0.1},
			func(h *domain.ReplicaMetrics) { h.PreemptionRate = 0.05 })
		Expect(r.Capped).To(Equal(1))
		Expect(r.Tier).To(Equal(TierPinnedB))
	})

	DescribeTable("keeps the band whenever the cap is not shown",
		func(adm Admission, change func(*domain.ReplicaMetrics)) {
			r := replay(adm, change)
			Expect(r.AboveBand).To(Equal(1))
			Expect(r.Capped).To(BeZero())
			Expect(r.Tier).To(BeEmpty())
		},
		Entry("max_num_seqs unknown", Admission{}, nil),
		Entry("the batch below the cap: memory, not the cap, binds", capped,
			func(h *domain.ReplicaMetrics) { h.RunningRequests = maxSeqs - 1 }),
		// The parser assumes 256 when no flag is set, while vLLM V1 defaults
		// to 1024 on large accelerators: 300 running is a batch still growing
		// past a cap that was never the engine's.
		Entry("more running than the cap: the cap read is not the engine's", capped,
			func(h *domain.ReplicaMetrics) { h.RunningRequests = 300; h.KvUsageInstant = 0.92 }),
		Entry("nothing waiting: the cap is not what holds the batch", capped,
			func(h *domain.ReplicaMetrics) { h.QueueLength = 0 }),
		Entry("the engine preempted in the ITL's window", capped,
			func(h *domain.ReplicaMetrics) { h.PreemptionRate = 0.05 }),
		Entry("a NaN preemption rate", capped,
			func(h *domain.ReplicaMetrics) { h.PreemptionRate = math.NaN() }),
		Entry("no preemption reading", capped,
			func(h *domain.ReplicaMetrics) { h.PreemptionRateKnown = false }),
		Entry("a NaN preemption allowance", Admission{MaxNumSeqs: maxSeqs, CapMaxPreemptionRate: math.NaN()}, nil),
	)

	It("reads the band the policy sets", func() {
		// A replica at 0.12 is below the default band and inside one from 0.10.
		rows := []domain.ReplicaMetrics{{PodName: "p", VariantName: "v", Ready: true, KvUsageInstant: 0.12, AvgITL: 0.02}}
		Expect(NewTracker().Observe("key", "v", rows, Admission{}, now).BelowBand).To(Equal(1))
		Expect(NewTracker().Observe("key", "v", rows, Admission{MinK: 0.10, MaxK: 0.80}, now).Offered).To(Equal(1))
		// And a replica at 0.85 is above the default band and inside one to 0.90.
		rows[0].KvUsageInstant = 0.85
		Expect(NewTracker().Observe("key", "v", rows, Admission{}, now).AboveBand).To(Equal(1))
		r := NewTracker().Observe("key", "v", rows, Admission{MinK: 0.15, MaxK: 0.90}, now)
		Expect(r.Offered).To(Equal(1))
		Expect(r.Capped).To(BeZero(), "inside the band is not the cap's exception")
	})
})
