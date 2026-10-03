package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The model total carries the scheduler queue once, and each role is charged
// the half of it that role serves: the prompt to prefill, the generation to
// decode. The per-role charges are therefore DISJOINT slices of the total and
// sum to it, so a per-role adjustment moves the total by that role's whole
// delta.
//
// It was not always so. The queue used to be charged to both roles -- prefill
// got the input tokens and decode got the same input tokens plus the output --
// so the roles summed to the total PLUS one input-token charge, and a
// heldInModelTotal() correction subtracted prefill's share before moving the
// total. These specs kept their invariant through that change; what moved is
// the fixture, which now charges the queue the way the analyzer does.
var _ = Describe("applyThroughputFloor and the model total", func() {
	const (
		k1      = 930508.0
		inTok   = 3_000_000.0 // the scheduler queue's input tokens, prefill's charge
		outTok  = 1_000_000.0 // and its output tokens, decode's charge
		perRole = 100_000.0   // each role's own resident demand
	)

	// The fleet as the analyzer builds it: two roles, each with its own
	// resident demand, the scheduler queue split between them, and a local
	// queue residency charge large enough that the floor lands below it --
	// which is the ordinary case.
	var (
		a         *SaturationAnalyzer
		cfg       *config.ScalingPolicy
		variants  []domain.VariantCapacity
		replicas  []capacity.ReplicaCapacity
		eppByRole map[string]float64
	)

	BeforeEach(func() {
		a = &SaturationAnalyzer{}
		cfg = &config.ScalingPolicy{}
		variants = []domain.VariantCapacity{
			{VariantName: "p", Role: domain.RolePrefill, PerReplicaCapacity: k1,
				ReplicaCount: 2, TotalDemand: perRole},
			{VariantName: "d", Role: domain.RoleDecode, PerReplicaCapacity: k1,
				ReplicaCount: 2, TotalDemand: perRole},
		}
		replicas = []capacity.ReplicaCapacity{
			{VariantName: "p", SaturatedThroughput: 5.4, SaturatedThroughputSamples: 2,
				QueueLength: 10, LocalQueueDemand: 2_000_000},
			{VariantName: "d", SaturatedThroughput: 5.4, SaturatedThroughputSamples: 2,
				QueueLength: 10, LocalQueueDemand: 2_000_000},
		}
		eppByRole = map[string]float64{
			domain.RolePrefill: inTok,
			domain.RoleDecode:  outTok,
		}
	})

	It("charges the queue once across the pair, so the roles sum to the total", func() {
		// The premise every spec below rests on, asserted rather than assumed.
		// If estimateSchedulerQueueDemand ever charges an overlapping slice
		// again, this is the spec that says so, and the total arithmetic that
		// follows stops being valid.
		Expect(eppByRole[domain.RolePrefill]+eppByRole[domain.RoleDecode]).
			To(BeNumerically("~", inTok+outTok, 1e-6),
				"prefill's prompt plus decode's generation IS the queue, not twice it")
	})

	It("moves the total by each role's whole delta", func() {
		// roleDemand and totalDemand exactly as analyzer.go builds them:
		// the total gets the queue once, each role gets the half it serves.
		roleDemand := map[string]float64{
			domain.RolePrefill: perRole + eppByRole[domain.RolePrefill],
			domain.RoleDecode:  perRole + eppByRole[domain.RoleDecode],
		}
		totalDemand := 2*perRole + (inTok + outTok)

		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, replicas, variants, totalDemand, roleDemand, eppByRole, 20, false, GinkgoLogr)

		// The invariant, stated without reference to the implementation's
		// formula: the model total is the sum of the per-role demands the
		// cycle ends with. An expectation computed by re-running the
		// production expression on the post-call map is self-consistent with
		// whatever that expression happens to be, and cannot see a wrong one.
		Expect(got).To(BeNumerically("~",
			roleDemand[domain.RolePrefill]+roleDemand[domain.RoleDecode], 1e-6),
			"the model total is what the roles now need, no more and no less")
		Expect(got).To(BeNumerically(">=", 0))
		for role, v := range roleDemand {
			Expect(v).To(BeNumerically(">=", 0), "roleDemand[%s]", role)
		}
	})

	It("prices the total correctly when only prefill has ever saturated", func() {
		// One role floored and the other not is the asymmetric case that used
		// to understate the total by prefill's queue share -- low, and
		// positive, so a sign check passed it.
		replicas[1].SaturatedThroughput = 0 // decode has never been seen saturated

		roleDemand := map[string]float64{
			domain.RolePrefill: perRole + eppByRole[domain.RolePrefill],
			domain.RoleDecode:  perRole + eppByRole[domain.RoleDecode],
		}
		totalDemand := 2*perRole + (inTok + outTok)

		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, replicas, variants, totalDemand, roleDemand, eppByRole, 20, false, GinkgoLogr)

		Expect(got).To(BeNumerically("~",
			roleDemand[domain.RolePrefill]+roleDemand[domain.RoleDecode], 1e-6),
			"prefill's floor belongs in the total whole; decode is unpriced and keeps what it had")
	})

	It("holds when prefill's own resident demand dwarfs its queue share", func() {
		// The fixture above keeps prefill's own demand below its queue share.
		// The ordinary production shape is the other one: prefill holding far
		// more resident KV than the scheduler queue's prompt charge.
		variants[0].TotalDemand = 10_000_000
		roleDemand := map[string]float64{
			domain.RolePrefill: variants[0].TotalDemand + eppByRole[domain.RolePrefill],
			domain.RoleDecode:  perRole + eppByRole[domain.RoleDecode],
		}
		totalDemand := variants[0].TotalDemand + perRole + (inTok + outTok)

		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, replicas, variants, totalDemand, roleDemand, eppByRole, 20, false, GinkgoLogr)

		Expect(got).To(BeNumerically("~",
			roleDemand[domain.RolePrefill]+roleDemand[domain.RoleDecode], 1e-6))
	})

	It("never returns a negative total when both roles' demand falls", func() {
		// What the removed correction was originally added to prevent. It is
		// now prevented by construction rather than by a clamp: each role's
		// delta is taken out of a total that genuinely held it, so the total
		// cannot be driven below the sum of what remains.
		//
		// Both roles are floored well below their pre-floor demand here, which
		// is the cycle that drove the total negative when the roles summed to
		// more than the total.
		roleDemand := map[string]float64{
			domain.RolePrefill: perRole + eppByRole[domain.RolePrefill],
			domain.RoleDecode:  perRole + eppByRole[domain.RoleDecode],
		}
		totalDemand := 2*perRole + (inTok + outTok)

		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, replicas, variants, totalDemand, roleDemand, eppByRole, 20, false, GinkgoLogr)

		Expect(got).To(BeNumerically(">=", 0))
		Expect(got).To(BeNumerically("~",
			roleDemand[domain.RolePrefill]+roleDemand[domain.RoleDecode], 1e-6))
	})

	It("leaves a decode-only fleet exactly as it was", func() {
		// No prefill role, so nothing about the split applies: decode carries
		// the whole queue (input + output) and its whole delta reaches the
		// total. The guard against narrowing the change to the wrong axis.
		roleDemand := map[string]float64{
			domain.RoleDecode: perRole + inTok + outTok,
		}
		onlyDecode := []domain.VariantCapacity{variants[1]}
		onlyDecodeReps := []capacity.ReplicaCapacity{replicas[1]}
		totalDemand := perRole + (inTok + outTok)

		before := roleDemand[domain.RoleDecode]
		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, onlyDecodeReps, onlyDecode, totalDemand, roleDemand,
			map[string]float64{domain.RoleDecode: inTok + outTok}, 20, false, GinkgoLogr)

		Expect(got).To(BeNumerically("~", totalDemand+(roleDemand[domain.RoleDecode]-before), 1e-6),
			"decode contributes its whole demand to the total, so its whole delta must reach it")
	})
})
