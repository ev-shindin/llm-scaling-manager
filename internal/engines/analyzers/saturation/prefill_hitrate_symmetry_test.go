package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// Prefill's floor is a quotient: the scheduler queue's charge divided by a mu
// that saturatedCompletionRate built as PrefillComputedTokenRate / ILeff. Both
// halves carry a (1 - prefixHitRate) factor, and the quotient is a replica
// count only while it is the SAME factor.
//
// It was not. The divisor takes fleetPrefixHitRate -- prefill replicas only,
// request-rate weighted -- and the charge took computeModelWorkloadAverages,
// which averages over every replica with token activity, decode included and
// unweighted. On a P/D fleet those diverge with the role ratio, and the
// direction is the one this whole file exists to prevent: mu inflated against
// the demand it is divided into, so prefill is under-ordered, worse the larger
// decode grows.
var _ = Describe("prefill's charge and prefill's divisor use one hit rate", func() {
	// One prefill replica reading 0.8, nine decode replicas reading 0.0: the
	// shape of the fleet the role attribution exists for.
	const (
		prefillHit = 0.8
		avgInput   = 10_000.0
		avgOutput  = 1_000.0
		queued     = 100
	)
	fleet := func() ([]domain.ReplicaMetrics, map[string]string) {
		out := []domain.ReplicaMetrics{{
			PodName: "p-0", VariantName: "p", Ready: true,
			TokensInUse: 1, TotalKvCapacityTokens: 100_000,
			AvgInputTokens: avgInput, AvgOutputTokens: 1,
			PrefixCacheHitRate: prefillHit, RequestRate: 4,
		}}
		for i := 0; i < 9; i++ {
			out = append(out, domain.ReplicaMetrics{
				PodName: "d-" + string(rune('0'+i)), VariantName: "d", Ready: true,
				TokensInUse: 1, TotalKvCapacityTokens: 100_000,
				AvgInputTokens: avgInput, AvgOutputTokens: avgOutput,
				PrefixCacheHitRate: 0, RequestRate: 4,
			})
		}
		roles := map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode}
		return out, roles
	}

	It("charges prefill at the rate its divisor was built from", func() {
		metrics, roles := fleet()
		// The figure the divisor is built from, read from the same helper
		// Analyze passes in -- not a constant retyped here.
		divisorHit := fleetPrefixHitRate(metrics, roles)
		Expect(divisorHit).To(BeNumerically("~", prefillHit, 1e-9),
			"the divisor reads PREFILL's rate, not the fleet mean")

		sq := &domain.SchedulerQueueMetrics{QueueSize: queued}
		activeRoles := map[string]bool{domain.RolePrefill: true, domain.RoleDecode: true}
		got := estimateSchedulerQueueDemand(sq, metrics, roles, activeRoles, divisorHit)

		// ILeff is what saturatedCompletionRate divides by, so the charge is
		// the queue's count at exactly that per-request figure.
		ileff := shape.New(avgInput, avgOutput, divisorHit).ILeff
		Expect(got.byRole[domain.RolePrefill]).To(BeNumerically("~", queued*ileff, 1e-6))
	})

	It("does not charge prefill at the fleet mean, which decode dominates", func() {
		// The negative control, and the measured size of the defect: with ten
		// replicas reading 0.8 once and 0.0 nine times the mean is 0.08, so the
		// charge would keep 92% of the prompt while the divisor kept 20% of it.
		metrics, roles := fleet()
		_, _, mean := computeModelWorkloadAverages(metrics, roles)
		Expect(mean).To(BeNumerically("~", 0.08, 1e-9))

		sq := &domain.SchedulerQueueMetrics{QueueSize: queued}
		activeRoles := map[string]bool{domain.RolePrefill: true, domain.RoleDecode: true}
		got := estimateSchedulerQueueDemand(sq, metrics, roles, activeRoles,
			fleetPrefixHitRate(metrics, roles))

		atMean := queued * avgInput * (1 - mean)
		Expect(got.byRole[domain.RolePrefill]).To(BeNumerically("<", atMean),
			"the mean would over-charge, which reads as under-capacity once mu divides it")
		// 0.92/0.20: the factor prefill was under-ordered by.
		Expect(atMean / got.byRole[domain.RolePrefill]).To(BeNumerically("~", 4.6, 0.01))
	})

	It("keeps the roles summing to the total when the two hit rates diverge", func() {
		// The invariant everything downstream rests on, asserted in the ONE
		// fleet shape that can break it. The queue-charge specs next door all
		// run at a hit rate of zero, where prefill's discount and the model-wide
		// mean are the same number by construction and the sum holds for free.
		//
		// Here they are 0.8 and 0.08. With the total left on the mean it read
		// 1,020,000 against roles summing to 300,000: a 720,000-token shortfall
		// that no role was responsible for serving, which is what deleting
		// heldInModelTotal() assumed could not happen.
		metrics, roles := fleet()
		activeRoles := map[string]bool{domain.RolePrefill: true, domain.RoleDecode: true}
		got := estimateSchedulerQueueDemand(&domain.SchedulerQueueMetrics{QueueSize: queued},
			metrics, roles, activeRoles, fleetPrefixHitRate(metrics, roles))

		_, _, mean := computeModelWorkloadAverages(metrics, roles)
		Expect(mean).To(BeNumerically("~", 0.08, 1e-9))
		Expect(fleetPrefixHitRate(metrics, roles)).To(BeNumerically("~", prefillHit, 1e-9),
			"the two discounts really do diverge in this fixture")

		Expect(got.byRole[domain.RolePrefill]+got.byRole[domain.RoleDecode]).
			To(BeNumerically("~", got.total, 1e-6),
				"nothing in the model total is unowned by a role")
	})

	It("takes the model total off the mean once the roles are split", func() {
		// The total follows the CHARGE on a split fleet, not the model-wide
		// mean, which is what makes the sum above hold. Stated as the two
		// figures rather than as their sum, so a change that moved both
		// consistently in the wrong direction still fails here.
		//
		// Decode's charge is the queue's OUTPUT tokens and carries no hit rate
		// at all -- the discount never applied to output, and the prompt half
		// is prefill's charge now (see decode_queue_charge_test.go).
		metrics, roles := fleet()
		_, _, mean := computeModelWorkloadAverages(metrics, roles)
		sq := &domain.SchedulerQueueMetrics{QueueSize: queued}
		activeRoles := map[string]bool{domain.RolePrefill: true, domain.RoleDecode: true}
		got := estimateSchedulerQueueDemand(sq, metrics, roles, activeRoles,
			fleetPrefixHitRate(metrics, roles))

		wantOut := queued * avgOutput
		wantIn := queued * avgInput * (1 - prefillHit)
		Expect(got.total).To(BeNumerically("~", wantIn+wantOut, 1e-6))
		Expect(got.byRole[domain.RoleDecode]).To(BeNumerically("~", wantOut, 1e-6))

		// And explicitly NOT the mean-discounted figure it used to be.
		atMean := queued*avgInput*(1-mean) + wantOut
		Expect(got.total).To(BeNumerically("<", atMean),
			"prefill's own 0.8 discounts more prompt than the fleet mean's 0.08")
	})

	It("keeps the identity when decode is absent from activeRoles", func() {
		// The gap the pair-condition left behind. With decode missing --
		// scaled to zero, or carrying no VariantCapacity this cycle --
		// outputTokens is 0, so the total is the prompt alone and it MUST be
		// prefill's prompt, not the model-wide one. Both are averages over the
		// same replicas; one is request-rate weighted and the other is not.
		//
		// Heterogeneous on purpose: equal replicas make the plain mean and the
		// weighted mean the same number, and the spec would pass either way.
		ms := []domain.ReplicaMetrics{
			{PodName: "p-0", VariantName: "p", Ready: true, TokensInUse: 1,
				TotalKvCapacityTokens: 100_000, AvgInputTokens: avgInput, AvgOutputTokens: 1,
				PrefixCacheHitRate: 0.9, RequestRate: 10},
			{PodName: "p-1", VariantName: "p", Ready: true, TokensInUse: 1,
				TotalKvCapacityTokens: 100_000, AvgInputTokens: avgInput, AvgOutputTokens: 1,
				PrefixCacheHitRate: 0.1, RequestRate: 1},
		}
		roles := map[string]string{"p": domain.RolePrefill}
		_, _, mean := computeModelWorkloadAverages(ms, roles)
		weighted := fleetPrefixHitRate(ms, roles)
		Expect(mean).To(BeNumerically("~", 0.50, 1e-9))
		Expect(weighted).To(BeNumerically("~", 0.8273, 1e-4),
			"the two averages really do diverge in this fixture")

		got := estimateSchedulerQueueDemand(&domain.SchedulerQueueMetrics{QueueSize: queued},
			ms, roles, map[string]bool{domain.RolePrefill: true}, weighted)

		Expect(got.byRole[domain.RolePrefill]).To(BeNumerically("~", got.total, 1e-6),
			"with no decode role the total IS prefill's charge")
		// And it is the weighted figure, not the plain mean: 327,273 tokens of
		// the 500,000 total were unowned when this read the mean.
		Expect(got.total).To(BeNumerically("~", queued*avgInput*(1-weighted), 1e-6))
	})

	It("leaves a decode-only fleet on the model-wide figure", func() {
		// The control for the condition being keyed on prefill: with no prefill
		// role the split has not happened, decode carries the whole request,
		// and the total must stay the model-wide inputTokens + outputTokens.
		metrics, roles := fleet()
		got := estimateSchedulerQueueDemand(&domain.SchedulerQueueMetrics{QueueSize: queued},
			metrics, roles, map[string]bool{domain.RoleDecode: true},
			fleetPrefixHitRate(metrics, roles))
		_, _, mean := computeModelWorkloadAverages(metrics, roles)
		want := queued*avgInput*(1-mean) + queued*avgOutput
		Expect(got.total).To(BeNumerically("~", want, 1e-6))
		Expect(got.byRole[domain.RoleDecode]).To(BeNumerically("~", got.total, 1e-6))
	})

	It("takes no discount on either side when prefill publishes no rate", func() {
		// A fleet whose prefill replicas report nothing readable: both sides
		// fall back to 0 together, so the quotient is still a replica count.
		// Consistency is the invariant, not the value.
		metrics, roles := fleet()
		for i := range metrics {
			metrics[i].PrefixCacheHitRate = -1 // out of range, so unreadable
		}
		divisorHit := fleetPrefixHitRate(metrics, roles)
		Expect(divisorHit).To(Equal(0.0))

		sq := &domain.SchedulerQueueMetrics{QueueSize: queued}
		activeRoles := map[string]bool{domain.RolePrefill: true}
		got := estimateSchedulerQueueDemand(sq, metrics, roles, activeRoles, divisorHit)
		Expect(got.byRole[domain.RolePrefill]).To(BeNumerically("~", queued*avgInput, 1e-6))
	})

	It("clamps a rate out of range the way the divisor's shape does", func() {
		// shape.New clamps to [0,1]; so does the charge, or a caller handing in
		// 1.5 would make the charge negative while ILeff stayed at zero.
		metrics, roles := fleet()
		sq := &domain.SchedulerQueueMetrics{QueueSize: queued}
		activeRoles := map[string]bool{domain.RolePrefill: true}

		hi := estimateSchedulerQueueDemand(sq, metrics, roles, activeRoles, 1.5)
		Expect(hi.byRole[domain.RolePrefill]).To(Equal(0.0))
		Expect(shape.New(avgInput, avgOutput, 1.5).ILeff).To(Equal(0.0))

		lo := estimateSchedulerQueueDemand(sq, metrics, roles, activeRoles, -0.5)
		Expect(lo.byRole[domain.RolePrefill]).To(BeNumerically("~", queued*avgInput, 1e-6))
	})
})
