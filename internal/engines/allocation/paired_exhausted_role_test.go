package allocation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// pairedFleet builds the smallest two-role model that exercises the joint
// commit: one variant per role, equal cost and per-replica capacity, so the
// only thing separating the roles in these tests is headroom and demand.
//
// Decode's required capacity is fixed: every case here wants decode to have
// real demand it cannot serve, and only prefill's figure varies.
func pairedFleet(
	decodeTarget, decodeMax, prefillTarget, prefillMax int,
	prefillRC float64,
) (NamedAnalyzerResult, []variantRecord, map[string]domain.VariantReplicaState, map[string]int) {
	const prc = 100.0
	const decodeRC = 400.0

	variants := []variantRecord{
		rec("decode", "decode", 1.0, prc),
		rec("prefill", "prefill", 1.0, prc),
	}

	dMax, pMax := decodeMax, prefillMax
	stateMap := map[string]domain.VariantReplicaState{
		"decode":  {VariantName: "decode", CurrentReplicas: decodeTarget, MaxReplicas: &dMax},
		"prefill": {VariantName: "prefill", CurrentReplicas: prefillTarget, MaxReplicas: &pMax},
	}

	e := NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "decode", Role: "decode", PerReplicaCapacity: prc},
				{VariantName: "prefill", Role: "prefill", PerReplicaCapacity: prc},
			},
		},
		RoleCapacities: map[string]domain.RoleCapacity{
			"decode": {
				Role:                   "decode",
				RequiredCapacity:       decodeRC,
				TotalSupply:            float64(decodeTarget) * prc,
				TotalAnticipatedSupply: float64(decodeTarget) * prc,
			},
			"prefill": {
				Role:                   "prefill",
				RequiredCapacity:       prefillRC,
				TotalSupply:            float64(prefillTarget) * prc,
				TotalAnticipatedSupply: float64(prefillTarget) * prc,
			},
		},
		Live: true,
	}

	targets := map[string]int{"decode": decodeTarget, "prefill": prefillTarget}
	return e, variants, stateMap, targets
}

var _ = Describe("allocateForModelPaired with a role at its ceiling", func() {
	// The joint commit exists so a P/D fleet keeps its ratio. A role that has
	// reached its own maxReplicas is finished, not blocking -- it must not
	// prevent its partner from being ordered. Measured on a cluster: decode
	// pinned at 9/9 of max 9 left prefill at 1 replica through a 1119-deep
	// queue with nine replicas of its own headroom unused, in every cycle of
	// the run. See docs/proposals/prefill-starved-by-exhausted-role.md.
	It("still orders prefill when decode is at maxReplicas", func() {
		e, variants, stateMap, targets := pairedFleet(9, 9, 1, 10, 400)

		roles, ps := initRoleState(&e)
		// Roles are the sorted keys, so decode -- the exhausted one -- is
		// picked first. That ordering is what made the old code abandon the
		// pass before prefill was ever committed.
		Expect(roles).To(Equal([]string{"decode", "prefill"}))

		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			nil, targets, costGreedyRolePick, ps, roles)

		Expect(targets["decode"]).To(Equal(9), "decode is at its ceiling and must not exceed it")
		Expect(targets["prefill"]).To(BeNumerically(">", 1), "prefill has headroom and demand and must grow")
	})

	It("does not exceed prefill's own ceiling", func() {
		e, variants, stateMap, targets := pairedFleet(9, 9, 1, 3, 10000)

		roles, ps := initRoleState(&e)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			nil, targets, costGreedyRolePick, ps, roles)

		Expect(targets["prefill"]).To(BeNumerically("<=", 3))
		Expect(targets["prefill"]).To(BeNumerically(">", 1))
	})

	It("terminates when every role is exhausted", func() {
		e, variants, stateMap, targets := pairedFleet(9, 9, 10, 10, 400)

		roles, ps := initRoleState(&e)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			nil, targets, costGreedyRolePick, ps, roles)

		// Unservable demand must be dropped, or anyRoleNeedsScaleUp stays true
		// on it and the loop spins forever.
		Expect(targets["decode"]).To(Equal(9))
		Expect(targets["prefill"]).To(Equal(10))
	})

	// The control: with headroom on both sides the joint commit is unchanged,
	// so the fix cannot be mistaken for "scale every role independently".
	It("still commits both roles together when both have headroom", func() {
		e, variants, stateMap, targets := pairedFleet(1, 10, 1, 10, 400)

		roles, ps := initRoleState(&e)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			nil, targets, costGreedyRolePick, ps, roles)

		Expect(targets["decode"]).To(BeNumerically(">", 1))
		Expect(targets["prefill"]).To(BeNumerically(">", 1))
	})
})
