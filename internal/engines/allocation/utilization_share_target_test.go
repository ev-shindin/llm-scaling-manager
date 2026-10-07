package allocation

import (
	"math"
	"math/rand"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The worked examples are those of docs/proposals/utilization-share-optimizer.md,
// with need N = d / k at k = 0.8 and one GPU per replica unless stated.

func role(key string, w, need float64, floor, ceiling, g int) ShareRole {
	return ShareRole{Key: key, Weight: w, Need: need, Floor: floor, Ceiling: ceiling, ReplicaGPUs: g}
}

// threeModels is §5.1's fleet: weights 2 / 1 / 1, demands 4 / 3 / 1, k = 0.8.
func threeModels() []ShareRole {
	return []ShareRole{
		role("A", 2, 5, 1, 0, 1),
		role("B", 1, 3.75, 1, 0, 1),
		role("C", 1, 1.25, 1, 0, 1),
	}
}

func sumInts(m map[string]int) int {
	s := 0
	for _, v := range m {
		s += v
	}
	return s
}

var _ = Describe("ContinuousShareTargets", func() {
	It("shares the spare as headroom proportional to weight (§5.1)", func() {
		t := ContinuousShareTargets(threeModels(), 16)
		Expect(t["A"]).To(BeNumerically("~", 9.0, 1e-9))
		Expect(t["B"]).To(BeNumerically("~", 5.25, 1e-9))
		Expect(t["C"]).To(BeNumerically("~", 1.75, 1e-9))
		// A's headroom is exactly twice B's and C's: 0.8 against 0.4.
		Expect(t["A"]/5 - 1).To(BeNumerically("~", 2*(t["B"]/3.75-1), 1e-9))
	})

	It("gives every role exactly its need when there is nothing spare", func() {
		t := ContinuousShareTargets(threeModels(), 10)
		Expect(t["A"]).To(BeNumerically("~", 5, 1e-9))
		Expect(t["B"]).To(BeNumerically("~", 3.75, 1e-9))
		Expect(t["C"]).To(BeNumerically("~", 1.25, 1e-9))
	})

	It("cuts a short quota by inverse weight (§5.1, 8 GPUs)", func() {
		// §5.1's arithmetic has no floors, so neither does this fixture.
		roles := threeModels()
		for i := range roles {
			roles[i].Floor = 0
		}
		t := ContinuousShareTargets(roles, 8)
		// t = 2 / (5/2 + 3.75 + 1.25) = 0.2667: A is cut 13 %, B and C 27 %.
		Expect(1 - t["A"]/5).To(BeNumerically("~", 0.1333, 1e-3))
		Expect(1 - t["B"]/3.75).To(BeNumerically("~", 0.2667, 1e-3))
		Expect(1 - t["C"]/1.25).To(BeNumerically("~", 0.2667, 1e-3))
	})

	It("pins a role whose inverse-weight cut would take it below its floor", func() {
		// With floors of 1, C's cut (1.25 × (1 − 0.267) = 0.92) is below its
		// floor: C holds 1, and A and B share the remaining 7 by inverse weight.
		t := ContinuousShareTargets(threeModels(), 8)
		Expect(t["C"]).To(Equal(1.0))
		Expect(t["A"] + t["B"]).To(BeNumerically("~", 7, 1e-9))
		Expect(1 - t["A"]/5).To(BeNumerically("<", 1-t["B"]/3.75))
	})

	It("pins a floor above the share and shares the rest", func() {
		roles := threeModels()
		roles[2].Floor = 8 // §5.5 case 3: C at minReplicas 8
		t := ContinuousShareTargets(roles, 16)
		Expect(t["C"]).To(Equal(8.0))
		Expect(t["A"] + t["B"]).To(BeNumerically("~", 8, 1e-9))
	})

	It("caps at a ceiling and re-shares what the ceiling refused", func() {
		roles := threeModels()
		roles[0].Ceiling = 6
		t := ContinuousShareTargets(roles, 16)
		Expect(t["A"]).To(Equal(6.0))
		Expect(t["A"] + t["B"] + t["C"]).To(BeNumerically("~", 16, 1e-9))
	})

	It("holds a role with no demand at its floor", func() {
		roles := append(threeModels(), role("D", 1, 0, 2, 0, 1))
		t := ContinuousShareTargets(roles, 18)
		Expect(t["D"]).To(Equal(2.0))
	})
})

var _ = Describe("IntegerShareTargets", func() {
	DescribeTable("reproduces the proposal's worked examples",
		func(roles []ShareRole, budget int, want map[string]int) {
			Expect(IntegerShareTargets(roles, budget)).To(Equal(want))
		},
		Entry("weights 2/1/1, 16 GPUs (§5.7)", threeModels(), 16,
			map[string]int{"A": 9, "B": 5, "C": 2}),
		Entry("everyone at weight 1 (§5.7)", []ShareRole{
			role("A", 1, 5, 1, 0, 1), role("B", 1, 3.75, 1, 0, 1), role("C", 1, 1.25, 1, 0, 1),
		}, 16, map[string]int{"A": 8, "B": 6, "C": 2}),
		Entry("B's demand doubles (§5.7)", []ShareRole{
			role("A", 2, 5, 1, 0, 1), role("B", 1, 7.5, 1, 0, 1), role("C", 1, 1.25, 1, 0, 1),
		}, 16, map[string]int{"A": 6, "B": 8, "C": 2}),
		Entry("a short quota (§5.7)", []ShareRole{
			role("A", 2, 10, 1, 0, 1), role("B", 1, 7.5, 1, 0, 1), role("C", 1, 2.5, 1, 0, 1),
		}, 16, map[string]int{"A": 9, "B": 5, "C": 2}),
		Entry("a P/D model beside an aggregated one (§5.7)", []ShareRole{
			role("A/prefill", 2, 2.5, 1, 0, 1), role("A/decode", 2, 5, 1, 0, 1), role("B", 1, 3.75, 1, 0, 1),
		}, 16, map[string]int{"A/prefill": 4, "A/decode": 7, "B": 5}),
		Entry("a large minReplicas (§5.5 case 3)", []ShareRole{
			role("A", 2, 5, 1, 0, 1), role("B", 1, 3.75, 1, 0, 1), role("C", 1, 1.25, 8, 0, 1),
		}, 16, map[string]int{"A": 5, "B": 3, "C": 8}),
		Entry("two P/D models on 8-GPU replicas (§5.7)", []ShareRole{
			role("A/prefill", 2, 10, 8, 0, 8), role("A/decode", 2, 18, 8, 0, 8),
			role("B/prefill", 1, 6, 8, 0, 8), role("B/decode", 1, 12, 8, 0, 8),
		}, 64, map[string]int{"A/prefill": 16, "A/decode": 24, "B/prefill": 8, "B/decode": 16}),
		Entry("the same, after A shifts to long outputs (§5.7)", []ShareRole{
			role("A/prefill", 2, 6, 8, 0, 8), role("A/decode", 2, 26, 8, 0, 8),
			role("B/prefill", 1, 6, 8, 0, 8), role("B/decode", 1, 12, 8, 0, 8),
		}, 64, map[string]int{"A/prefill": 8, "A/decode": 32, "B/prefill": 8, "B/decode": 16}),
	)

	It("can leave a role one replica below its need when whole replicas do not allow it (§5.1)", func() {
		roles := []ShareRole{role("X", 1, 10, 0, 0, 8), role("Y", 1, 10, 0, 0, 8)}
		Expect(ShareSpare(roles, 24)).To(BeNumerically(">", 0))
		Expect(WholeReplicaCoverage(roles, 24)).To(BeFalse())
		t := IntegerShareTargets(roles, 24)
		Expect(sumInts(t)).To(Equal(24))
		Expect([]int{t["X"], t["Y"]}).To(ConsistOf(16, 8))
		Expect(WholeReplicaCoverage(roles, 32)).To(BeTrue())
	})

	It("respects ceilings and leaves what nobody can take unallocated", func() {
		roles := []ShareRole{role("A", 1, 2, 1, 3, 1), role("B", 1, 2, 1, 4, 1)}
		t := IntegerShareTargets(roles, 16)
		Expect(t).To(Equal(map[string]int{"A": 3, "B": 4}))
	})

	It("gives a role with no demand nothing above its floor", func() {
		roles := append(threeModels(), role("D", 4, 0, 2, 0, 1))
		Expect(IntegerShareTargets(roles, 18)["D"]).To(Equal(2))
	})

	It("terminates with mixed replica sizes and a remainder smaller than any replica", func() {
		roles := []ShareRole{role("big", 1, 20, 0, 0, 8), role("mid", 1, 5, 0, 0, 3)}
		t := IntegerShareTargets(roles, 30)
		Expect(t["big"] % 8).To(BeZero())
		Expect(t["mid"] % 3).To(BeZero())
		Expect(30 - sumInts(t)).To(BeNumerically("<", 3))
	})

	Describe("properties", func() {
		randomRoles := func(rnd *rand.Rand) ([]ShareRole, int) {
			n := 2 + rnd.Intn(5)
			roles := make([]ShareRole, n)
			floors := 0
			for i := range roles {
				g := []int{1, 1, 2, 4, 8}[rnd.Intn(5)]
				floor := g * rnd.Intn(2)
				floors += floor
				roles[i] = role(string(rune('a'+i)), []float64{0.5, 1, 2, 4}[rnd.Intn(4)],
					rnd.Float64()*20, floor, 0, g)
			}
			return roles, floors + rnd.Intn(64)
		}

		It("never exceeds the budget, keeps floors, and is deterministic under reordering", func() {
			rnd := rand.New(rand.NewSource(1))
			for range 500 {
				roles, budget := randomRoles(rnd)
				t := IntegerShareTargets(roles, budget)
				Expect(sumInts(t)).To(BeNumerically("<=", budget))
				for _, r := range roles {
					Expect(t[r.Key]).To(BeNumerically(">=", r.Floor))
					Expect(t[r.Key] % max(r.ReplicaGPUs, 1)).To(BeZero())
				}
				shuffled := append([]ShareRole(nil), roles...)
				rnd.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
				Expect(IntegerShareTargets(shuffled, budget)).To(Equal(t))
			}
		})

		It("allocates identically when every weight is multiplied by 1000 (§5.3)", func() {
			rnd := rand.New(rand.NewSource(2))
			for range 300 {
				roles, budget := randomRoles(rnd)
				scaled := append([]ShareRole(nil), roles...)
				for i := range scaled {
					scaled[i].Weight *= 1000
				}
				Expect(IntegerShareTargets(scaled, budget)).To(Equal(IntegerShareTargets(roles, budget)))
			}
		})

		It("leaves no candidate that could still take a replica", func() {
			rnd := rand.New(rand.NewSource(3))
			for range 500 {
				roles, budget := randomRoles(rnd)
				t := IntegerShareTargets(roles, budget)
				left := budget - sumInts(t)
				for _, r := range roles {
					if r.Need > 0 {
						Expect(max(r.ReplicaGPUs, 1)).To(BeNumerically(">", left),
							"role %s could still take a replica", r.Key)
					}
				}
			}
		})
	})
})

var _ = Describe("ShareScore", func() {
	It("discounts headroom by weight and amplifies shortfall by it", func() {
		Expect(ShareScore(9, 5, 2)).To(BeNumerically("~", 0.4, 1e-9))  // +80 % at weight 2
		Expect(ShareScore(4, 5, 2)).To(BeNumerically("~", -0.4, 1e-9)) // −20 % at weight 2
		Expect(ShareScore(5, 5, 1)).To(BeZero())
		Expect(math.IsInf(ShareScore(3, 0, 1), 1)).To(BeTrue())
	})
})

var _ = Describe("ShareInBand", func() {
	const k = 0.8
	It("is judged in GPUs and never narrower than half a replica (§6.1)", func() {
		r := role("decode", 1, 10, 8, 0, 8)
		// Ĝ = 12 on 8-GPU replicas: 8 and 16 are both 4 GPUs away, inside ½ · 8.
		Expect(ShareInBand(8, 12, r, 0.15, k)).To(BeTrue())
		Expect(ShareInBand(16, 12, r, 0.15, k)).To(BeTrue())
		Expect(ShareInBand(0, 12, r, 0.15, k)).To(BeFalse())
	})

	It("is not fooled by the idle floor when the role is past its threshold", func() {
		// Large spare: target 40 GPUs for a need of 5 (û = 0.1). Holding 4 GPUs
		// puts u = 1.0, past the threshold: out of band whatever the idle floor.
		r := role("A", 1, 5, 0, 0, 1)
		Expect(ShareInBand(4, 40, r, 0.15, k)).To(BeFalse())
	})

	It("lets an idle role off on a large relative miss of no consequence", func() {
		// need 0.25, target 10 → û = 0.02; holding 4 → u = 0.05: 6 GPUs off, but
		// both utilizations are tiny and below the threshold.
		r := role("idle", 1, 0.25, 0, 0, 1)
		Expect(ShareInBand(4, 10, r, 0.15, k)).To(BeTrue())
	})
})

var _ = Describe("EvaluateShare", func() {
	It("marks a role actionable only when out of band and off its integer target (§6.1)", func() {
		roles := threeModels()
		thresholds := map[string]float64{"A": 0.8, "B": 0.8, "C": 0.8}
		// At the integer target: nothing to do, whatever the continuous miss.
		ev := EvaluateShare(roles, map[string]int{"A": 9, "B": 5, "C": 2}, thresholds, 16, 0.15)
		for _, v := range ev.Roles {
			Expect(v.Actionable).To(BeFalse(), v.Key)
		}
		Expect(ev.ReplicasToMove).To(BeZero())
		Expect(ev.WholeReplicaCoverage).To(BeTrue())

		// B's demand doubles while B still holds 5 (§5.7): B is out of band and
		// below its integer target of 8; three replicas would move.
		roles[1].Need = 7.5
		ev = EvaluateShare(roles, map[string]int{"A": 9, "B": 5, "C": 2}, thresholds, 16, 0.15)
		byKey := map[string]ShareRoleVerdict{}
		for _, v := range ev.Roles {
			byKey[v.Key] = v
		}
		Expect(byKey["B"].Actionable).To(BeTrue())
		Expect(byKey["B"].Continuous).To(BeNumerically("~", 8.4, 1e-9))
		Expect(byKey["B"].Integer).To(Equal(8))
		Expect(ev.ReplicasToMove).To(Equal(3))
	})

	It("does not call a role off target only through granularity actionable (§6.7 rule 2)", func() {
		// C holds 2 against a continuous target of 1.4; 2 is its integer target.
		roles := []ShareRole{role("A", 2, 5, 1, 0, 1), role("B", 1, 7.5, 1, 0, 1), role("C", 1, 1.25, 1, 0, 1)}
		thresholds := map[string]float64{"A": 0.8, "B": 0.8, "C": 0.8}
		ev := EvaluateShare(roles, map[string]int{"A": 6, "B": 8, "C": 2}, thresholds, 16, 0.15)
		for _, v := range ev.Roles {
			Expect(v.Actionable).To(BeFalse(), v.Key)
		}
	})
})

var _ = Describe("share targets: edge cases", func() {
	It("re-shares what a ceiling refuses by weight, not equally", func() {
		// A capped at 6; B (weight 2) and C (weight 1) share the rest so that
		// B's headroom is twice C's.
		roles := []ShareRole{role("A", 1, 5, 0, 6, 1), role("B", 2, 4, 0, 0, 1), role("C", 1, 4, 0, 0, 1)}
		t := ContinuousShareTargets(roles, 24)
		Expect(t["A"]).To(Equal(6.0))
		Expect(t["B"] + t["C"]).To(BeNumerically("~", 18, 1e-9))
		Expect(t["B"]/4 - 1).To(BeNumerically("~", 2*(t["C"]/4-1), 1e-9))
	})

	It("pins a ceiling in the short regime too", func() {
		roles := []ShareRole{role("A", 4, 10, 0, 3, 1), role("B", 1, 10, 0, 0, 1)}
		t := ContinuousShareTargets(roles, 10)
		Expect(t["A"]).To(Equal(3.0))
		Expect(t["B"]).To(BeNumerically("~", 7, 1e-9))
	})

	It("holds every role at its floor when the floors exceed the budget, without going negative", func() {
		roles := []ShareRole{role("A", 1, 5, 4, 0, 1), role("B", 1, 5, 4, 0, 1)}
		Expect(IntegerShareTargets(roles, 6)).To(Equal(map[string]int{"A": 4, "B": 4}))
		for _, v := range ContinuousShareTargets(roles, 6) {
			Expect(v).To(BeNumerically(">=", 0))
		}
	})

	It("allocates nothing above the floors when no role has demand", func() {
		roles := []ShareRole{role("A", 1, 0, 1, 0, 1), role("B", 2, 0, 2, 0, 1)}
		Expect(IntegerShareTargets(roles, 16)).To(Equal(map[string]int{"A": 1, "B": 2}))
		Expect(ContinuousShareTargets(roles, 16)).To(Equal(map[string]float64{"A": 1, "B": 2}))
	})
})
