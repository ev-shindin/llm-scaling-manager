package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("saturatedCompletionRate for prefill", func() {
	// Prefill is bounded by the prompt tokens it has to compute, so its mu is
	// a token rate converted at the shape arriving -- not the request rate it
	// used to take.
	//
	// Why it matters, measured on run PK: at 1, 2 and 10 prefill replicas the
	// request rate read 4.75, 4.62 and 4.50 req/s while the computed-token
	// rate went 138,875 -> 933,750. A request rate under overload is the rate
	// the fleet is being SERVED at, so dividing by it sizes the fleet by its
	// own current size, and a figure learned at one prompt length is worthless
	// at another.
	prefillReplica := func(tokenRate, reqRate float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			VariantName:              "p",
			PodName:                  "p-1",
			Ready:                    true,
			RequestRate:              reqRate,
			PrefillComputedTokenRate: tokenRate,
		}
	}

	It("prices prefill from the token rate at the effective prompt length", func() {
		// 140,000 computed tokens/s over 30,000-token prompts is 4.67 req/s,
		// regardless of what the request rate happens to read.
		mu, ok := saturatedCompletionRate(prefillReplica(140000, 4.5), domain.RolePrefill, 6000, 30000)
		Expect(ok).To(BeTrue())
		Expect(mu).To(BeNumerically("~", 140000.0/30000.0, 1e-9))
	})

	It("gives a higher request rate for shorter prompts at the same velocity", func() {
		// The property a request rate cannot express: the same replica serves
		// more short requests than long ones.
		long, _ := saturatedCompletionRate(prefillReplica(140000, 4.5), domain.RolePrefill, 6000, 30000)
		short, _ := saturatedCompletionRate(prefillReplica(140000, 4.5), domain.RolePrefill, 6000, 1000)
		Expect(short).To(BeNumerically(">", long))
	})

	It("falls back to the request rate when no token rate is published", func() {
		// SGLang has no per-stage prefill counter at all
		// (sgl-project/sglang issue #14303), and neither does an older vLLM.
		// The request rate is what this always used and is better than no
		// window, which leaves the role with no floor.
		mu, ok := saturatedCompletionRate(prefillReplica(0, 4.5), domain.RolePrefill, 6000, 30000)
		Expect(ok).To(BeTrue())
		Expect(mu).To(BeNumerically("~", 4.5, 1e-9))
	})

	It("falls back when the prompt length is unknown, rather than dividing by zero", func() {
		mu, ok := saturatedCompletionRate(prefillReplica(140000, 4.5), domain.RolePrefill, 6000, 0)
		Expect(ok).To(BeTrue())
		Expect(mu).To(BeNumerically("~", 4.5, 1e-9))
	})

	It("records nothing when neither figure is available", func() {
		// The contract the 2026-09-22 outage established: a half-priced window
		// would take the max of two different definitions of mu.
		_, ok := saturatedCompletionRate(prefillReplica(0, 0), domain.RolePrefill, 6000, 30000)
		Expect(ok).To(BeFalse())
	})

	It("leaves decode on the generation-token path", func() {
		// The control: the change is scoped to prefill. Decode is bounded by
		// resident KV and its mu stays tokens/output-length.
		rm := domain.ReplicaMetrics{
			VariantName: "d", Ready: true,
			GenerationTokenRate: 12000, PrefillComputedTokenRate: 999999,
		}
		mu, ok := saturatedCompletionRate(rm, domain.RoleDecode, 6000, 30000)
		Expect(ok).To(BeTrue())
		Expect(mu).To(BeNumerically("~", 2.0, 1e-9), "12000/6000, not the prefill token rate")
	})
})
