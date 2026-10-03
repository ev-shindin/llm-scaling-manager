package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// A queued request is one backlog. It used to be charged in full to BOTH roles
// of a disaggregated fleet -- prefill got the input tokens, decode got the same
// input tokens plus the output -- so the fleet was sized for roughly twice the
// work that existed, and the half charged to decode was work decode does not do.
//
// The charge is right without disaggregation, and right at a steady shape even
// with it (a fixed over-count divides out of demand/mu). It breaks when the
// shape turns input-heavy: at 1000 in / 6000 out the charge is output-dominated,
// which is decode's real work; at 30000 in / 250 out the input term is 120x the
// output. Measured on that trace, decode's demand was 96.65% gateway charge
// against 2.74% resident KV, and it held eight replicas at 2% KV with one
// request waiting while prefill queued 249.
var _ = Describe("the scheduler queue is charged to the role that serves it", func() {
	const (
		queued = 100
		hiIn   = 30_000.0 // phase 2 of the shape-swap trace
		loOut  = 250.0
		loIn   = 1_000.0 // phase 1
		hiOut  = 6_000.0
	)
	// One replica per role, no prefix caching, so the arithmetic below is the
	// charge and nothing else.
	fleet := func(in, out float64) ([]domain.ReplicaMetrics, map[string]string) {
		return []domain.ReplicaMetrics{
			{
				PodName: "p-0", VariantName: "p", Ready: true,
				TokensInUse: 1, TotalKvCapacityTokens: 100_000,
				AvgInputTokens: in, AvgOutputTokens: 1,
			},
			{
				PodName: "d-0", VariantName: "d", Ready: true,
				TokensInUse: 1, TotalKvCapacityTokens: 100_000,
				AvgInputTokens: in, AvgOutputTokens: out,
			},
		}, map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode}
	}
	sq := func() *domain.SchedulerQueueMetrics {
		return &domain.SchedulerQueueMetrics{QueueSize: queued}
	}
	pd := map[string]bool{domain.RolePrefill: true, domain.RoleDecode: true}

	It("charges decode what it generates, not the prompt it never computes", func() {
		metrics, roles := fleet(hiIn, loOut)
		got := estimateSchedulerQueueDemand(sq(), metrics, roles, pd, 0)

		Expect(got.byRole[domain.RoleDecode]).To(BeNumerically("~", queued*loOut, 1e-6),
			"the output tokens decode must generate")
		Expect(got.byRole[domain.RolePrefill]).To(BeNumerically("~", queued*hiIn, 1e-6),
			"the prompt tokens prefill must compute")
	})

	It("stops sizing decode by an input-heavy prompt it cannot hold yet", func() {
		// The regression this exists to prevent, at the ratio that exposed it.
		metrics, roles := fleet(hiIn, loOut)
		got := estimateSchedulerQueueDemand(sq(), metrics, roles, pd, 0)

		old := queued * (hiIn + loOut) // what the branch used to charge
		Expect(got.byRole[domain.RoleDecode]).To(BeNumerically("<", old/100),
			"120x the output used to swamp it")
		Expect(old / got.byRole[domain.RoleDecode]).To(BeNumerically("~", 121.0, 0.5))
	})

	It("barely moves decode's charge at an output-heavy shape", func() {
		// Phase 1 of the same trace. The control for the claim that this is
		// about the RATIO and not about decode in general: where output
		// dominates, the old and new charges are within a few percent.
		metrics, roles := fleet(loIn, hiOut)
		got := estimateSchedulerQueueDemand(sq(), metrics, roles, pd, 0)

		old := queued * (loIn + hiOut)
		Expect(got.byRole[domain.RoleDecode]).To(BeNumerically("~", queued*hiOut, 1e-6))
		Expect(got.byRole[domain.RoleDecode]/old).To(BeNumerically(">", 0.85),
			"output-heavy: the old charge was already nearly all decode's own work")
	})

	It("leaves an aggregated fleet completely alone", func() {
		// No prefill role: a RoleBoth pod computes the prompt AND generates
		// from it, so the whole request is its work and the charge is right.
		metrics := []domain.ReplicaMetrics{{
			PodName: "b-0", VariantName: "b", Ready: true,
			TokensInUse: 1, TotalKvCapacityTokens: 100_000,
			AvgInputTokens: hiIn, AvgOutputTokens: loOut,
		}}
		roles := map[string]string{"b": domain.RoleBoth}
		got := estimateSchedulerQueueDemand(sq(), metrics, roles,
			map[string]bool{domain.RoleBoth: true}, 0)
		Expect(got.byRole[domain.RoleBoth]).To(BeNumerically("~", queued*(hiIn+loOut), 1e-6))
	})

	It("leaves a decode role running WITHOUT a prefill side alone", func() {
		// A decode-labelled variant with no prefill peer still serves whole
		// requests. Keying off the label alone would have starved it.
		metrics, roles := fleet(hiIn, loOut)
		got := estimateSchedulerQueueDemand(sq(), metrics, roles,
			map[string]bool{domain.RoleDecode: true}, 0)
		Expect(got.byRole[domain.RoleDecode]).To(BeNumerically("~", queued*(hiIn+loOut), 1e-6),
			"no prefill role active, so decode is not disaggregated")
	})

	It("keeps the model-level total unchanged", func() {
		// Every other consumer reads total; splitting the roles must not move
		// the figure the model is sized by.
		metrics, roles := fleet(hiIn, loOut)
		got := estimateSchedulerQueueDemand(sq(), metrics, roles, pd, 0)
		Expect(got.total).To(BeNumerically("~", queued*(hiIn+loOut), 1e-6))
	})

	It("no longer charges the one queue twice across the pair", func() {
		// The property in one line: prefill's share plus decode's share is the
		// queue, not two of them.
		metrics, roles := fleet(hiIn, loOut)
		got := estimateSchedulerQueueDemand(sq(), metrics, roles, pd, 0)
		sum := got.byRole[domain.RolePrefill] + got.byRole[domain.RoleDecode]
		Expect(sum).To(BeNumerically("~", got.total, 1e-6))
	})
})
