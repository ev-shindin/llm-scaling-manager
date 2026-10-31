package saturation

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
)

var _ = Describe("prefillBoundKey", func() {
	It("renames the role field of a prefill throughput key", func() {
		Expect(prefillBoundKey("ns|v|m|H200|1|prefill|ilong|noout|q5")).
			To(Equal("ns|v|m|H200|1|prefill-bound|ilong|noout|q5"))
	})
	It("gives no key for another role or another shape", func() {
		Expect(prefillBoundKey("ns|v|m|H200|1|decode|ilong|long|q5")).To(BeEmpty())
		Expect(prefillBoundKey("ns|v|m|H200|1|prefill|noout|q5")).To(BeEmpty())
	})
})

// Issue #144, re-scoped to the capacity side. The fleet: the P/D benchmark's
// two decode replicas and one prefill replica (downstream_saturation_test.go),
// 6000-token prompts. In phase A decode is the saturated role, and prefill
// queues 30 deep on decode's back-pressure while computing 24,000 prompt
// tokens/s -- 4 req/s at 6000 tokens. computeK2 discards that reading for
// prefill's own window, rightly; before this change it was lost, prefill had
// no mu, and the floor gave prefill no term. In phase B decode has recovered,
// prefill's queue is EMPTY, and the router's arrival rate doubles.
var _ = Describe("a prefill lower bound learned while decode meters prefill", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
		clock    time.Time
		// rateSources is every prefill service-rate source closedLoop saw.
		rateSources []string
	)
	const (
		prefillVariant = "prefill-v"
		prefillKv      = int64(1_149_312)
		promptTokens   = 6000.0
		boundRate      = 4.0 // req/s: 24,000 computed tokens/s over 6000-token prompts
		scaleUp        = 0.85
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(capacity.NewStore())
		clock = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
		analyzer.now = func() time.Time { return clock }
		ctx = context.Background()
		rateSources = nil
	})
	decode := func(pod string, tokensInUse int64, queue int) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, decodeVariant, tokensInUse, runKvCapacity, queue, promptTokens, 1000)
		rm.RequestRate = 4.73
		rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
		return rm
	}
	decodeFull := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81)}
	}
	decodeCoping := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{decode("decode-0", 300_000, 0), decode("decode-1", 280_000, 0)}
	}
	// A prefill pod. queue 30 with 357,800 resident is phase A's back-pressure;
	// queue 0 with 20,000 resident is phase B's prefill keeping up.
	prefill := func(pod string, queue int, tokens int64, computed float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, prefillVariant, tokens, prefillKv, queue, promptTokens, 1)
		rm.RequestRate = computed / promptTokens
		rm.PrefillComputedTokenRate = computed
		return rm
	}
	states := func(prefillReady, prefillPending int) []domain.VariantReplicaState {
		return []domain.VariantReplicaState{
			{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
			{VariantName: prefillVariant, Role: domain.RolePrefill, AcceleratorName: "H200",
				CurrentReplicas: prefillReady + prefillPending, PendingReplicas: prefillPending, GPUsPerReplica: 1},
		}
	}
	run := func(rows []domain.ReplicaMetrics, st []domain.VariantReplicaState, lambda float64,
		tweak ...func(*config.ScalingPolicy)) *domain.AnalyzerResult {
		in := makeAnalyzerInput(rows, st)
		in.ArrivalRate = lambda
		// On for these specs; off is the default, and the specs that test it
		// say so with the off tweak.
		in.Config.(*config.ScalingPolicy).EnablePrefillThroughputBound = true
		for _, t := range tweak {
			t(in.Config.(*config.ScalingPolicy))
		}
		res, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	boundKey := func() string {
		analyzer.mu.Lock()
		defer analyzer.mu.Unlock()
		for k := range analyzer.saturatedThroughput {
			if strings.Contains(k, "|"+prefillBoundRole+"|") {
				return k
			}
		}
		return ""
	}
	measuredPrefillKeys := func() int {
		analyzer.mu.Lock()
		defer analyzer.mu.Unlock()
		n := 0
		for k := range analyzer.saturatedThroughput {
			if strings.Contains(k, "|"+domain.RolePrefill+"|") {
				n++
			}
		}
		return n
	}
	prefillP := func(res *domain.AnalyzerResult) float64 {
		for _, vc := range res.VariantCapacities {
			if vc.VariantName == prefillVariant {
				return vc.PerReplicaCapacity
			}
		}
		Fail("no capacity for " + prefillVariant)
		return 0
	}
	// prefillOrder is what the engine orders for prefill from this result:
	// RC = D / scaleUp - anticipated supply, in replicas, rounded up as the
	// optimizer does (steadystate.applyUniversalThreshold).
	prefillOrder := func(res *domain.AnalyzerResult, ready, pending int) int {
		rc := res.RoleDemand[domain.RolePrefill]/(scaleUp*prefillP(res)) - float64(ready+pending)
		if rc <= 1e-9 {
			return 0
		}
		return int(math.Ceil(rc - 1e-9))
	}
	// learn runs phase A for n cycles a sample spacing apart.
	learn := func(n int) {
		for i := 0; i < n; i++ {
			clock = clock.Add(ThroughputSampleSpacing)
			// Each reading a little lower than the last: a reading equal to the
			// previous one is a re-read of the same scrape pair and is not a
			// sample of its own (recordSaturatedThroughput). The max is the first.
			computed := boundRate * promptTokens * (1 - 0.01*float64(i))
			run(append(decodeFull(), prefill("prefill-0", 30, 357_800, computed)), states(1, 0), runLambda)
		}
	}
	// recover moves past the decode-saturation memory, so the next cycle
	// reads decode as having recovered.
	recover := func() { clock = clock.Add(DecodeSaturationMemory + time.Second) }

	It("learns the bound from the reading prefill's own window refuses", func() {
		learn(floor.MinThroughputSamplesToOrder)
		Expect(measuredPrefillKeys()).To(BeZero(), "prefill's own window is still not written under decode's metering")
		k := boundKey()
		Expect(k).NotTo(BeEmpty())
		Expect(analyzer.saturatedThroughput[k].Max()).To(BeNumerically("~", boundRate*promptTokens, 1e-9), "held in computed tokens/s")
		Expect(analyzer.saturatedThroughput[k].Len()).To(Equal(floor.MinThroughputSamplesToOrder))
	})

	It("keeps the max of its readings: each one is a floor on the true rate", func() {
		for _, computed := range []float64{18_000, 24_000, 12_000} {
			clock = clock.Add(ThroughputSampleSpacing)
			run(append(decodeFull(), prefill("prefill-0", 30, 357_800, computed)), states(1, 0), runLambda)
		}
		Expect(analyzer.saturatedThroughput[boundKey()].Max()).To(BeNumerically("~", 24_000.0, 1e-9))
	})

	It("learns nothing from a prefill that is not queued, not Ready, or has no token rate", func() {
		clock = clock.Add(ThroughputSampleSpacing)
		run(append(decodeFull(), prefill("prefill-0", 2, 357_800, 24_000)), states(1, 0), runLambda)
		notReady := prefill("prefill-0", 30, 357_800, 24_000)
		notReady.Ready = false
		clock = clock.Add(ThroughputSampleSpacing)
		run(append(decodeFull(), notReady), states(1, 0), runLambda)
		clock = clock.Add(ThroughputSampleSpacing)
		run(append(decodeFull(), prefill("prefill-0", 30, 357_800, 0)), states(1, 0), runLambda)
		Expect(boundKey()).To(BeEmpty())
	})

	// The closed loop. Phase B: decode has recovered, prefill's queue is
	// empty, and the router's arrival rate doubles to 12 req/s. lambda over
	// the bound is 3 replicas' worth, 3 / 0.85 = 3.53 of supply, against a
	// fleet of one. The engine's order is replayed each cycle: a replica
	// ordered is pending for one cycle and Ready the next, and every prefill
	// row reads an empty queue -- the queue never forms. Returns each cycle's
	// order and the fleet it ends on.
	closedLoop := func(tweak ...func(*config.ScalingPolicy)) ([]int, int) {
		for i := 0; i < floor.MinThroughputSamplesToOrder; i++ {
			clock = clock.Add(ThroughputSampleSpacing)
			computed := boundRate * promptTokens * (1 - 0.01*float64(i))
			run(append(decodeFull(), prefill("prefill-0", 30, 357_800, computed)), states(1, 0), runLambda, tweak...)
		}
		recover()
		lambda := 2 * runLambda
		ready, pending := 1, 0
		var orders []int
		for cycle := 0; cycle < 8; cycle++ {
			clock = clock.Add(15 * time.Second)
			rows := decodeCoping()
			for i := 0; i < ready; i++ {
				// Each ready replica computes its share of what arrives; none queues.
				rows = append(rows, prefill("prefill-"+string(rune('0'+i)), 0, 20_000,
					lambda*promptTokens/float64(ready)))
			}
			res := run(rows, states(ready, pending), lambda, tweak...)
			for _, vc := range res.VariantCapacities {
				if vc.VariantName == prefillVariant && vc.ServiceRate != nil {
					rateSources = append(rateSources, vc.ServiceRate.RateSource)
				}
			}
			order := prefillOrder(res, ready, pending)
			orders = append(orders, order)
			ready += pending
			pending = order
		}
		return orders, ready + pending
	}

	// Off is the default, and off is the behaviour before the bound existed:
	// nothing is learned from the back-pressure episode, prefill has no rate,
	// and the empty queue orders nothing. The same run with the bound on
	// orders prefill on the first cycle of the surge.
	It("is off by default: learns nothing and orders no prefill before its queue forms", func() {
		off := func(c *config.ScalingPolicy) { c.EnablePrefillThroughputBound = false }
		orders, fleet := closedLoop(off)
		Expect(boundKey()).To(BeEmpty(), "nothing recorded with the bound off")
		Expect(rateSources).NotTo(ContainElement(domain.ServiceRateSourceLowerBound), "nothing read with the bound off")
		Expect(orders).To(HaveEach(0), "no prefill ordered on an empty queue")
		Expect(fleet).To(Equal(1))
	})

	It("orders prefill on rising router arrivals before its queue forms, and stops at what the bound implies", func() {
		orders, fleet := closedLoop()
		Expect(rateSources).To(ContainElement(domain.ServiceRateSourceLowerBound))
		Expect(orders[0]).To(Equal(1),
			"the first cycle of the surge orders prefill with its queue at zero, capped at twice the ready fleet")
		// 12 / 4 / 0.85 = 3.53: the fleet settles on four, never more.
		Expect(fleet).To(Equal(4), "converges on lambda / (bound x scaleUp), no ratchet")
		Expect(orders[len(orders)-1]).To(BeZero())
		for _, o := range orders {
			Expect(o).To(BeNumerically("<=", 2), "no cycle more than doubles the ready fleet")
		}
	})

	// Decode saturates again and again at one load, serving all of it. Each
	// episode's readings are decode's admission shared over the prefill fleet
	// the bound had grown, so they read lower than the last episode's; ten of
	// them fill the ring. Were the lowest reading to price prefill, every
	// episode would grow the fleet by 1/scaleUp at least: 5, 6, 8, 10 on this
	// fixture. The bound keeps its highest reading, so the fleet stays put.
	It("does not grow prefill again on each decode episode at the same load", func() {
		lambda := 2 * runLambda
		n := 4
		var fleets []int
		for episode := 0; episode < 4; episode++ {
			for i := 0; i < 10; i++ {
				clock = clock.Add(ThroughputSampleSpacing)
				rows := decodeFull()
				for j := 0; j < n; j++ {
					rows = append(rows, prefill(fmt.Sprintf("prefill-%d", j), 30, 357_800,
						lambda*promptTokens/float64(n)*(1-0.001*float64(i))))
				}
				run(rows, states(n, 0), lambda)
			}
			recover()
			ready, pending := n, 0
			for cycle := 0; cycle < 10; cycle++ {
				clock = clock.Add(15 * time.Second)
				rows := decodeCoping()
				for j := 0; j < ready; j++ {
					rows = append(rows, prefill(fmt.Sprintf("prefill-%d", j), 0, 20_000,
						lambda*promptTokens/float64(ready)))
				}
				order := prefillOrder(run(rows, states(ready, pending), lambda), ready, pending)
				ready += pending
				pending = order
			}
			n = ready + pending
			fleets = append(fleets, n)
		}
		// 12 / 3 / 0.85 = 4.7: five, on the first episode's bound of 3 req/s.
		Expect(fleets).To(Equal([]int{5, 5, 5, 5}))
	})

	It("orders nothing on a bound while decode is still saturated: the prefill hold stands", func() {
		learn(floor.MinThroughputSamplesToOrder)
		res := run(append(decodeFull(), prefill("prefill-0", 0, 20_000, 24_000)), states(1, 0), 2*runLambda)
		Expect(prefillOrder(res, 1, 0)).To(BeZero())
	})

	It("never shrinks prefill: a bound above the load leaves occupancy's demand alone", func() {
		learn(floor.MinThroughputSamplesToOrder)
		recover()
		rows := append(decodeCoping(), prefill("prefill-0", 0, 600_000, 1_000), prefill("prefill-1", 0, 600_000, 1_000))
		with := run(rows, states(2, 0), 0.5)
		without := NewSaturationAnalyzer(capacity.NewStore())
		without.now = analyzer.now
		in := makeAnalyzerInput(rows, states(2, 0))
		in.ArrivalRate = 0.5
		base, err := without.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(with.RoleDemand[domain.RolePrefill]).To(Equal(base.RoleDemand[domain.RolePrefill]))
	})

	It("gives way to prefill's own window once prefill saturates on its own", func() {
		learn(floor.MinThroughputSamplesToOrder)
		recover()
		// Decode coping, prefill queued: prefill's own reading, 6 req/s.
		for i := 0; i < floor.MinThroughputSamplesToOrder; i++ {
			clock = clock.Add(ThroughputSampleSpacing)
			run(append(decodeCoping(), prefill("prefill-0", 30, 357_800, 36_000)), states(1, 0), runLambda)
		}
		Expect(measuredPrefillKeys()).To(Equal(1))
		res := run(append(decodeCoping(), prefill("prefill-0", 0, 20_000, 36_000)), states(1, 0), 2*runLambda)
		for _, vc := range res.VariantCapacities {
			if vc.VariantName == prefillVariant {
				Expect(vc.ServiceRate).NotTo(BeNil())
				Expect(vc.ServiceRate.RateSource).To(Equal(domain.ServiceRateSourceMeasured))
			}
		}
	})

	It("reports the bound as the variant's service-rate source while it prices", func() {
		learn(floor.MinThroughputSamplesToOrder)
		recover()
		res := run(append(decodeCoping(), prefill("prefill-0", 0, 20_000, 24_000)), states(1, 0), 2*runLambda)
		for _, vc := range res.VariantCapacities {
			if vc.VariantName == prefillVariant {
				Expect(vc.ServiceRate).NotTo(BeNil())
				Expect(vc.ServiceRate.RateSource).To(Equal(domain.ServiceRateSourceLowerBound))
				Expect(vc.ServiceRate.Rate).To(BeNumerically("~", boundRate, 1e-9))
			}
		}
	})

	It("is exported with the rest of the learned state, so it survives a restart", func() {
		learn(floor.MinThroughputSamplesToOrder)
		s := analyzer.ExportLearnedState()
		k := boundKey()
		Expect(s.Throughput).To(HaveKey(k))
		Expect(s.Throughput[k].Values).To(HaveLen(floor.MinThroughputSamplesToOrder))
	})
})

// One input bucket spans a factor of two, and the top one ("huge", 6000 tokens
// and up) has no upper end. A bound held in requests/s kept the shortest
// prompt's figure, since the max never yields: learned at 6000-token prompts,
// it priced 12,000-token prompts at twice what the replica does for them. The
// window holds computed tokens/s and converts at the prompt being served.
var _ = Describe("a prefill lower bound across prompt lengths in one input bucket", func() {
	It("prices the prompt length being served, not the shortest one it was learned at", func() {
		analyzer := NewSaturationAnalyzer(capacity.NewStore())
		clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
		analyzer.now = func() time.Time { return clock }
		const prefillVariant = "prefill-v"
		states := []domain.VariantReplicaState{
			{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
			{VariantName: prefillVariant, Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
		}
		rows := func(prompt float64, decodeFull bool, queue int, tokens int64, computed float64) []domain.ReplicaMetrics {
			out := make([]domain.ReplicaMetrics, 0, 3)
			for i, kv := range []int64{970_475, 1_039_474} {
				q := 40
				if !decodeFull {
					kv, q = 300_000, 0
				}
				rm := makeReplicaMetrics(fmt.Sprintf("decode-%d", i), decodeVariant, kv, runKvCapacity, q, prompt, 1000)
				rm.RequestRate = 4.73
				rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
				out = append(out, rm)
			}
			p := makeReplicaMetrics("prefill-0", prefillVariant, tokens, 1_149_312, queue, prompt, 1)
			p.RequestRate = computed / prompt
			p.PrefillComputedTokenRate = computed
			return append(out, p)
		}
		run := func(r []domain.ReplicaMetrics, lambda float64) *domain.AnalyzerResult {
			in := makeAnalyzerInput(r, states)
			in.ArrivalRate = lambda
			in.Config.(*config.ScalingPolicy).EnablePrefillThroughputBound = true
			res, err := analyzer.Analyze(context.Background(), in)
			Expect(err).NotTo(HaveOccurred())
			return res
		}
		// Behind a full decode at 6000-token prompts, then hours later at
		// 12,000: the replica computes 24,000 tokens/s both times, 4 req/s
		// and then 2.
		for _, prompt := range []float64{6000, 12000} {
			clock = clock.Add(3 * time.Hour)
			for i := 0; i < 2; i++ {
				clock = clock.Add(ThroughputSampleSpacing)
				run(rows(prompt, true, 30, 357_800, 24_000*(1-0.01*float64(i))), runLambda)
			}
		}
		clock = clock.Add(DecodeSaturationMemory + time.Second)
		res := run(rows(12000, false, 0, 20_000, 24_000), 2*runLambda)
		var priced *domain.ServiceRateSignal
		for _, vc := range res.VariantCapacities {
			if vc.VariantName == prefillVariant {
				priced = vc.ServiceRate
			}
		}
		Expect(priced).NotTo(BeNil())
		Expect(priced.RateSource).To(Equal(domain.ServiceRateSourceLowerBound))
		Expect(priced.Rate).To(BeNumerically("~", 2.0, 1e-9),
			"24,000 tokens/s at 12,000-token prompts, not the 4 req/s of the 6000-token episode")
	})
})

// Issue #395. The bound wrote every prefill pod's reading under one name, so
// mu's fold kept the fastest pod's within a minute: one pod read at twice its
// rate -- a series doubled inside PromQL -- set the bound for the whole
// fleet, and the floor ordered half the prefill the load needed. Within a
// minute the pods fold by their mean, as mu's do; across minutes the bound
// still only rises.
var _ = Describe("notePrefillBound across the pods of one minute", func() {
	const (
		key    = "ns|prefill-v|m|H200|1|prefill|ihuge|noout|q5"
		prompt = 6000.0
	)
	var (
		analyzer *SaturationAnalyzer
		clock    time.Time
		cfg      *config.ScalingPolicy
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(capacity.NewStore())
		clock = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
		analyzer.now = func() time.Time { return clock }
		cfg = &config.ScalingPolicy{EnablePrefillThroughputBound: true, QueueLengthThreshold: 5}
	})
	// minute reads each pod once, saturated, a second apart, and returns the
	// bound in prompt tokens/s as notePrefillBound prices it after the last.
	minute := func(computed ...float64) float64 {
		var got prefillBoundReading
		for i, c := range computed {
			rm := makeReplicaMetrics(fmt.Sprintf("prefill-%d", i), "prefill-v", 300_000, 1_149_312, 30, prompt, 1)
			rm.PrefillComputedTokenRate = c
			got = analyzer.notePrefillBound(rm, cfg, domain.RolePrefill, key, "", prompt, admission{}, GinkgoLogr)
			clock = clock.Add(time.Second)
		}
		return got.rate * prompt
	}

	It("prices the mean of four pods when one reads at twice its rate, not the doubled pod", func() {
		Expect(minute(4000, 4000, 4000, 8000)).To(BeNumerically("~", 5000, 1e-6))
	})
	It("prices the mean of four pods when one is genuinely slow, not the fastest", func() {
		Expect(minute(4000, 4000, 4000, 2000)).To(BeNumerically("~", 3500, 1e-6))
	})
	It("still only rises across minutes: a later, lower minute keeps the earlier figure", func() {
		Expect(minute(4000, 4000)).To(BeNumerically("~", 4000, 1e-6))
		clock = clock.Add(ThroughputSampleSpacing)
		Expect(minute(3000, 3200)).To(BeNumerically("~", 4000, 1e-6))
		clock = clock.Add(ThroughputSampleSpacing)
		Expect(minute(4400, 4800)).To(BeNumerically("~", 4600, 1e-6))
	})
	It("keeps the earlier figure through a minute that starts a new window after EpisodeGap", func() {
		Expect(minute(4000, 4000)).To(BeNumerically("~", 4000, 1e-6))
		clock = clock.Add(capacity.EpisodeGap + time.Minute)
		Expect(minute(3000, 3200)).To(BeNumerically("~", 4000, 1e-6))
	})
})
