package slo

import (
	"context"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

type fakeRouter struct{ latency *domain.RouterLatency }

func (f fakeRouter) CollectRouterLatency(context.Context, string, string) *domain.RouterLatency {
	return f.latency
}

func policy(tpot, ttft time.Duration) *config.ScalingPolicy {
	p := &config.ScalingPolicy{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.70,
		Analyzers:         []config.AnalyzerScoreConfig{{Name: "saturation"}, {Name: domain.SLOAnalyzerName}},
		SLO:               &config.SLOPolicy{},
	}
	if tpot > 0 {
		p.SLO.TPOT = &config.SLOObjective{Target: config.SLODuration{Duration: tpot}}
	}
	if ttft > 0 {
		p.SLO.TTFT = &config.SLOObjective{Target: config.SLODuration{Duration: ttft}}
	}
	return p
}

// row is one Ready replica of variant whose mean ITL lies exactly on line at
// fill k: 100k tokens of KV, 1000-token outputs, no prompt, so KVreq = 500.
func row(variant, pod string, k float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: variant, Ready: true,
		KvUsageInstant: k, AvgITL: line.ITLAt(k),
		TotalKvCapacityTokens: 100000, AvgOutputTokens: 1000, RequestRate: 1,
	}
}

// cycleRows is cycle i's readings: two replicas each holding its own fill, so
// five cycles give the window ten readings spanning 0.45 of k.
//
// Each replica holds still because only a steady reading may teach the full
// fit (itl.DefaultSteadyFor): a fill that climbs 0.1 a cycle is a ramp, whose
// k and ITL describe different moments. The fill moves inside
// itl.DefaultMaxKDrift with i, so a cycle is still told apart from the next.
func cycleRows(variant string, i int) []domain.ReplicaMetrics {
	return []domain.ReplicaMetrics{
		row(variant, variant+"-a", 0.20+0.01*float64(i)),
		row(variant, variant+"-b", 0.65+0.01*float64(i)),
	}
}

var _ = Describe("Analyzer", func() {
	var (
		a     *Analyzer
		clock time.Time
		ctx   = context.Background()
		agg   = domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth,
			AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 2}
	)

	BeforeEach(func() {
		clock = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
		a = New(nil, nil)
		a.now = func() time.Time { return clock }
	})

	analyze := func(cfg *config.ScalingPolicy, states []domain.VariantReplicaState,
		rows []domain.ReplicaMetrics) *domain.AnalyzerResult {
		res, err := a.Analyze(ctx, domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", Config: cfg, ArrivalRate: 2,
			VariantStates: states, ReplicaMetrics: rows,
		})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	warm := func(cfg *config.ScalingPolicy) *domain.AnalyzerResult {
		var res *domain.AnalyzerResult
		for i := range 5 {
			res = analyze(cfg, []domain.VariantReplicaState{agg}, cycleRows("agg", i))
			clock = clock.Add(15 * time.Second)
		}
		return res
	}

	It("prices an aggregated variant at the fill where its mean ITL meets the TPOT target", func() {
		res := warm(policy(44*time.Millisecond, 0))
		Expect(res.AnalyzerName).To(Equal(domain.SLOAnalyzerName))
		// lambda * O = 2 req/s * 1000 tokens.
		Expect(res.TotalDemand).To(BeNumerically("~", 2000, 1e-9))
		Expect(res.RoleDemand).To(BeNil())
		Expect(res.VariantCapacities).To(HaveLen(1))
		vc := res.VariantCapacities[0]
		Expect(vc.Reason).To(Equal("slo-" + constants.SLOSourceOwn))
		// At k = 0.6: 0.6 * 100000 / 500 = 120 sequences, a token each per 44 ms.
		Expect(vc.PerReplicaCapacity).To(BeNumerically("~", 120/0.044, 1e-6))
		Expect(vc.ReplicaCount).To(Equal(2))
		Expect(vc.ObservedReplicas).To(Equal(2))
	})

	It("reports no data until the window holds enough readings to fit", func() {
		res := analyze(policy(44*time.Millisecond, 0), []domain.VariantReplicaState{agg}, cycleRows("agg", 0))
		Expect(res.VariantCapacities[0].Reason).To(Equal(domain.ReasonNoData))
		Expect(res.VariantCapacities[0].PerReplicaCapacity).To(BeZero())
	})

	It("clamps the priced fill to the band the policy sets", func() {
		warm(policy(44*time.Millisecond, 0))
		cfg := policy(44*time.Millisecond, 0)
		cfg.ITLMaxObservableK = 0.50
		// A loose target prices at the top of the band: 0.80 by default.
		_, reading, d := a.priceVariant(domain.AnalyzerInput{Namespace: "ns", ModelID: "m", Config: cfg}, agg,
			windowKey("ns", "m", agg), readFleet(domain.AnalyzerInput{VariantStates: []domain.VariantReplicaState{agg},
				ReplicaMetrics: cycleRows("agg", 5)}),
			1.0, 0.85, clock)
		Expect(reading).NotTo(BeNil())
		Expect(reading.Fill).To(Equal(0.50), "itlMaxObservableK must reach the fill clamp")
		Expect(d.targetFill).To(BeNumerically(">", 0.50))
	})

	It("abstains on an infeasible target rather than capping the variant", func() {
		cfg := policy(44*time.Millisecond, 0)
		warm(cfg)
		vc, reading, _ := a.priceVariant(domain.AnalyzerInput{Namespace: "ns", ModelID: "m"}, agg,
			windowKey("ns", "m", agg), readFleet(domain.AnalyzerInput{VariantStates: []domain.VariantReplicaState{agg},
				ReplicaMetrics: cycleRows("agg", 4)}),
			0.010, 0.85, clock)
		Expect(reading.Source).To(Equal(constants.SLOSourceInfeasible))
		Expect(vc.Reason).To(Equal(domain.ReasonNoData))
		Expect(vc.PerReplicaCapacity).To(BeZero())
		Expect(math.IsNaN(reading.Fill)).To(BeTrue())
	})

	It("names a window the pinned fit refused as declined, not as too few readings", func() {
		// A fast card: ITL flat at 3 ms, under the 6 ms default intercept, so
		// the pinned slope comes out negative. That lasts, and "no-line" --
		// documented as too few readings -- would have had the operator wait.
		flat := func(i int) []domain.ReplicaMetrics {
			rows := cycleRows("agg", i)
			for j := range rows {
				rows[j].KvUsageInstant = 0.5 + 0.01*float64(j)
				rows[j].AvgITL = 0.003
			}
			return rows
		}
		cfg := policy(44*time.Millisecond, 0)
		for i := range 5 {
			analyze(cfg, []domain.VariantReplicaState{agg}, flat(i))
			clock = clock.Add(15 * time.Second)
		}
		vc, reading, d := a.priceVariant(domain.AnalyzerInput{Namespace: "ns", ModelID: "m"}, agg,
			windowKey("ns", "m", agg), readFleet(domain.AnalyzerInput{VariantStates: []domain.VariantReplicaState{agg},
				ReplicaMetrics: flat(5)}),
			0.044, 0.85, clock)
		Expect(reading.Source).To(Equal(constants.SLOSourceDeclined))
		Expect(d.line.Declined).To(BeTrue())
		Expect(d.line.Held).To(BeNumerically(">=", 10), "the window held enough readings")
		Expect(vc.Reason).To(Equal(domain.ReasonNoData))
	})

	It("falls back to the default intercept, labelled as such, when the fleet never spanned enough of k", func() {
		cfg := policy(44*time.Millisecond, 0)
		var res *domain.AnalyzerResult
		for range 5 {
			res = analyze(cfg, []domain.VariantReplicaState{agg},
				[]domain.ReplicaMetrics{row("agg", "agg-a", 0.5), row("agg", "agg-b", 0.52)})
			clock = clock.Add(15 * time.Second)
		}
		Expect(res.VariantCapacities[0].Reason).To(Equal("slo-" + constants.SLOSourcePinnedDefault))
	})

	// Each case feeds readings that alone would fill the window and fit a line,
	// so a filter that let them through would price the variant.
	DescribeTable("fits no line from replicas that do not describe the variant",
		func(exclude func(*domain.ReplicaMetrics)) {
			cfg := policy(44*time.Millisecond, 0)
			var res *domain.AnalyzerResult
			for i := range 5 {
				rows := cycleRows("agg", i)
				for j := range rows {
					exclude(&rows[j])
				}
				// One ordinary replica with no ITL reading, so the variant
				// still has a shape and demand.
				own := row("agg", "agg-own", 0.3)
				own.AvgITL = 0
				res = analyze(cfg, []domain.VariantReplicaState{agg}, append(rows, own))
				clock = clock.Add(15 * time.Second)
			}
			Expect(res.TotalDemand).To(BeNumerically(">", 0))
			Expect(res.VariantCapacities[0].Reason).To(Equal(domain.ReasonNoData))
		},
		Entry("lent by the warm pool", func(rm *domain.ReplicaMetrics) { rm.FromWarmPool = true }),
		Entry("not Ready", func(rm *domain.ReplicaMetrics) { rm.Ready = false }),
		Entry("of another variant", func(rm *domain.ReplicaMetrics) { rm.VariantName = "other" }),
		Entry("above the fitted range", func(rm *domain.ReplicaMetrics) { rm.KvUsageInstant = 0.9 }),
	)

	It("prices decode, leaves prefill unmodelled, and attributes demand to decode on P/D", func() {
		decode := domain.VariantReplicaState{VariantName: "dec", Role: domain.RoleDecode,
			AcceleratorName: "H200", GPUsPerReplica: 8, CurrentReplicas: 2}
		prefill := domain.VariantReplicaState{VariantName: "pre", Role: domain.RolePrefill,
			AcceleratorName: "H200", GPUsPerReplica: 8, CurrentReplicas: 1}
		cfg := policy(44*time.Millisecond, 0)
		var res *domain.AnalyzerResult
		for i := range 5 {
			rows := append(cycleRows("dec", i), domain.ReplicaMetrics{PodName: "pre-a", VariantName: "pre", Ready: true})
			res = analyze(cfg, []domain.VariantReplicaState{prefill, decode}, rows)
			clock = clock.Add(15 * time.Second)
		}
		Expect(res.RoleDemand).To(Equal(map[string]float64{domain.RoleDecode: res.TotalDemand}))
		byName := map[string]domain.VariantCapacity{}
		for _, vc := range res.VariantCapacities {
			byName[vc.VariantName] = vc
		}
		Expect(byName["pre"].Reason).To(Equal(domain.ReasonNoData))
		Expect(byName["pre"].TotalDemand).To(BeZero())
		Expect(byName["pre"].ObservedReplicas).To(Equal(1),
			"its Ready pod is observed; 0 read as a scrape gap")
		Expect(byName["dec"].ObservedReplicas).To(Equal(2))
		Expect(byName["dec"].PerReplicaCapacity).To(BeNumerically(">", 0))
		Expect(byName["dec"].TotalDemand).To(BeNumerically("~", res.TotalDemand, 1e-9))
	})

	It("adds the router backlog to demand, drained over a minute", func() {
		res, err := a.Analyze(ctx, domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
			SchedulerQueue: &domain.SchedulerQueueMetrics{QueueSize: 30},
			VariantStates:  []domain.VariantReplicaState{agg}, ReplicaMetrics: cycleRows("agg", 0),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.TotalDemand).To(BeNumerically("~", 2000+30*1000/backlogDrainSeconds, 1e-9))
	})

	It("falls back to the replicas' completion rate when the router reports no arrivals", func() {
		res, err := a.Analyze(ctx, domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0),
			VariantStates: []domain.VariantReplicaState{agg}, ReplicaMetrics: cycleRows("agg", 0),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.TotalDemand).To(BeNumerically("~", 2*1000, 1e-9))
	})

	It("rejects a config that is not a scaling policy", func() {
		_, err := a.Analyze(ctx, domain.AnalyzerInput{ModelID: "m", Namespace: "ns"})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("judge", func() {
	ttft3s := policy(0, 3*time.Second).SLO

	judgeWith := func(rl *domain.RouterLatency, slo *config.SLOPolicy) []float64 {
		a := New(fakeRouter{latency: rl}, nil)
		out := a.judge(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"}, slo)
		Expect(out).To(HaveLen(1))
		return []float64{out[0].Observed, out[0].Attainment}
	}

	It("reports the TTFT percentile and its attainment from the router's histogram", func() {
		// 1 req/s over 300 s: 300 requests, enough for p90.
		rl := &domain.RouterLatency{WindowSeconds: 300,
			TTFTBuckets: buckets(1, 0.9, 3, 0.95, inf, 1.0)}
		got := judgeWith(rl, ttft3s)
		Expect(got[0]).To(BeNumerically("~", 1.0, 1e-12))
		Expect(got[1]).To(BeNumerically("~", 0.95, 1e-12))
	})

	It("reports nothing from too few requests", func() {
		// 0.2 req/s over 300 s: 60 requests, short of the 100 p90 needs.
		rl := &domain.RouterLatency{WindowSeconds: 300,
			TTFTBuckets: buckets(1, 0.18, 3, 0.19, inf, 0.2)}
		got := judgeWith(rl, ttft3s)
		Expect(math.IsNaN(got[0])).To(BeTrue())
		Expect(math.IsNaN(got[1])).To(BeTrue())
	})

	It("reports nothing when the router exports nothing", func() {
		got := judgeWith(nil, ttft3s)
		Expect(math.IsNaN(got[0])).To(BeTrue())
	})

	It("reports the router's mean TPOT, with no attainment for a mean", func() {
		rl := &domain.RouterLatency{WindowSeconds: 300, TPOTSumRate: 0.08, TPOTCountRate: 1}
		got := judgeWith(rl, policy(100*time.Millisecond, 0).SLO)
		Expect(got[0]).To(BeNumerically("~", 0.08, 1e-12))
		Expect(math.IsNaN(got[1])).To(BeTrue())
	})
})

var _ = Describe("Analyzer with no output length reported", func() {
	It("leaves demand undefined and every generating variant without data", func() {
		a := New(nil, nil)
		clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return clock }
		agg := domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth, CurrentReplicas: 2}
		dec := domain.VariantReplicaState{VariantName: "dec", Role: domain.RoleDecode, CurrentReplicas: 1}
		var res *domain.AnalyzerResult
		for i := range 5 {
			rows := append(cycleRows("agg", i), cycleRows("dec", i)...)
			for j := range rows {
				rows[j].AvgOutputTokens = 0
			}
			var err error
			res, err = a.Analyze(context.Background(), domain.AnalyzerInput{
				ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
				VariantStates: []domain.VariantReplicaState{agg, dec}, ReplicaMetrics: rows,
			})
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		Expect(res.TotalDemand).To(BeZero())
		Expect(res.RoleDemand).To(BeNil(), "a role demand of zero would read as idle, not unknown")
		for _, vc := range res.VariantCapacities {
			Expect(vc.Reason).To(Equal(domain.ReasonNoData), vc.VariantName)
			Expect(vc.PerReplicaCapacity).To(BeZero(), vc.VariantName)
		}
	})
})
