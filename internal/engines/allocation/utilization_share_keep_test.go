package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("A donor keeps its own need only where that starves no one", func() {
	run := func(roles []ShareRole, held map[string]int, budget int) []ShareTransfer {
		th := map[string]float64{}
		for _, r := range roles {
			th[r.Key] = 0.8
		}
		in := SharePlanInput{Roles: roles, Held: held, Thresholds: th, Budget: budget, Tolerance: 0.15}
		l := NewShareLedger()
		started := make([]ShareTransfer, 0, 4)
		for i := range 5 {
			started = append(started, PlanShareTransfers(l, in, time.Unix(int64(30*i), 0), simTimings()).Started...)
		}
		return started
	}

	It("moves to a receiver far below its need when only fractions cover the needs", func() {
		// 9 + 6 of 17 fits in fractions, not in whole replicas (16 + 6). D
		// keeping its need would leave R at a sixth of its own.
		roles := []ShareRole{
			{Key: "D", Weight: 1, Need: 9, Ceiling: 64, ReplicaGPUs: 8},
			{Key: "R", Weight: 1, Need: 6, Ceiling: 64, ReplicaGPUs: 1},
		}
		Expect(run(roles, map[string]int{"D": 16, "R": 1}, 17)).To(ContainElement(HaveField("Donor", "D")))
	})

	It("moves between two 8-GPU P/D roles when only fractions cover the needs", func() {
		// 17 + 14 of 32: whole replicas need 24 + 16.
		roles := []ShareRole{
			{Key: "D", Weight: 1, Need: 17, Ceiling: 64, ReplicaGPUs: 8},
			{Key: "R", Weight: 1, Need: 14, Ceiling: 64, ReplicaGPUs: 8},
		}
		Expect(run(roles, map[string]int{"D": 24, "R": 8}, 32)).To(ContainElement(HaveField("Donor", "D")))
	})

	It("keeps the floor, and the need rounded up only while whole replicas cover every claim", func() {
		roles := []ShareRole{{Key: "a", Need: 2.3, Floor: 1, ReplicaGPUs: 1}, {Key: "b", Need: 0.5, Floor: 4, ReplicaGPUs: 1}}
		Expect(shareKeep(roles, 10)).To(Equal(map[string]int{"a": 3, "b": 4}))
		Expect(shareKeep(roles, 2)).To(Equal(map[string]int{"a": 1, "b": 4}))
		big := []ShareRole{{Key: "a", Need: 9, Floor: 0, ReplicaGPUs: 8}, {Key: "b", Need: 6, Floor: 0, ReplicaGPUs: 1}}
		Expect(shareKeep(big, 17)).To(Equal(map[string]int{"a": 0, "b": 0}), "16 + 6 is over 17")
		Expect(shareKeep(big, 22)).To(Equal(map[string]int{"a": 9, "b": 6}))
	})
})
