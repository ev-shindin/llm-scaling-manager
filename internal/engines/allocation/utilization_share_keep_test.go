package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("A donor keeps its own need while the budget covers every need", func() {
	// D's replicas are 8 GPUs, larger than the spare; R's are 1. Raising R's
	// headroom by one of D's replicas would leave D below its own need.
	run := func(rNeed float64, budget int) []ShareTransfer {
		roles := []ShareRole{
			{Key: "D", Weight: 1, Need: 9, Ceiling: 64, ReplicaGPUs: 8},
			{Key: "R", Weight: 1, Need: rNeed, Ceiling: 64, ReplicaGPUs: 1},
		}
		in := SharePlanInput{Roles: roles, Held: map[string]int{"D": 16, "R": 1},
			Thresholds: map[string]float64{"D": 0.8, "R": 0.8}, Budget: budget, Tolerance: 0.15}
		l := NewShareLedger()
		started := make([]ShareTransfer, 0, 4)
		for i := range 5 {
			started = append(started, PlanShareTransfers(l, in, time.Unix(int64(30*i), 0), simTimings()).Started...)
		}
		return started
	}

	It("takes no replica that would leave the donor below its need", func() {
		// Need 9 + 6 = 15 within 17: only headroom may move.
		for _, t := range run(6, 17) {
			Expect(t.Donor).NotTo(Equal("D"), "D was taken from 16 to 8 GPUs against a need of 9")
		}
	})

	It("still moves to the worst off when the budget is short (control)", func() {
		// Need 9 + 12 = 21 over 17: every role is short, and the worst off gains.
		Expect(run(12, 17)).To(ContainElement(HaveField("Donor", "D")))
	})

	It("keeps the floor, and the need rounded up only while covered", func() {
		roles := []ShareRole{{Key: "a", Need: 2.3, Floor: 1}, {Key: "b", Need: 0.5, Floor: 4}}
		Expect(shareKeep(roles, 10)).To(Equal(map[string]int{"a": 3, "b": 4}))
		Expect(shareKeep(roles, 2)).To(Equal(map[string]int{"a": 1, "b": 4}))
	})
})
