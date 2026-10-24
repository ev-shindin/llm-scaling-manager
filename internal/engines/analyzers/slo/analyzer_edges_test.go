package slo

import (
	"context"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

var _ = Describe("Analyzer state and inputs", func() {
	var (
		clock time.Time
		ctx   = context.Background()
		agg   = domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth,
			AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 2}
	)
	BeforeEach(func() {
		clock = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	})
	newAt := func(store *capacity.Store) *Analyzer {
		a := New(nil, store)
		a.now = func() time.Time { return clock }
		return a
	}
	// warmWith runs five cycles of rows whose fills span 0.45 of k, so the
	// window fits a full line, with mutate applied to every row.
	warmWith := func(a *Analyzer, mutate func(*domain.ReplicaMetrics)) *domain.AnalyzerResult {
		var res *domain.AnalyzerResult
		for i := range 5 {
			rows := cycleRows("agg", i)
			for j := range rows {
				mutate(&rows[j])
			}
			var err error
			res, err = a.Analyze(ctx, domain.AnalyzerInput{
				ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
				VariantStates: []domain.VariantReplicaState{agg}, ReplicaMetrics: rows,
			})
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		return res
	}
	noop := func(*domain.ReplicaMetrics) {}

	It("forgets a variant's line once it has not been seen for the timeout", func() {
		a := newAt(nil)
		warmWith(a, noop)
		key := windowKey("ns", "m", agg)
		Expect(a.lines.WindowLens()).To(HaveKey(key))
		Expect(a.lines.Baselines()).To(HaveKey(key))

		// Evicted by the engine's per-cycle sweep, not by another model's
		// Analyze: once no model lists the analyzer, nothing would call that.
		ev := a.EvictStale(clock.Add(defaultStateTimeout + time.Minute))
		Expect(ev.Lines).To(Equal(1))
		Expect(ev.Baselines).To(Equal(1))
		Expect(a.lines.WindowLens()).NotTo(HaveKey(key))
		Expect(a.lines.Baselines()).NotTo(HaveKey(key))
		Expect(a.seen).NotTo(HaveKey(key))
	})

	It("keeps a line seen within the timeout", func() {
		a := newAt(nil)
		warmWith(a, noop)
		Expect(a.EvictStale(clock.Add(defaultStateTimeout - time.Minute)).Any()).To(BeFalse())
		Expect(a.lines.WindowLens()).To(HaveKey(windowKey("ns", "m", agg)))
	})

	It("keeps a parked variant's line while it is reported present", func() {
		a := newAt(nil)
		warmWith(a, noop)
		key := windowKey("ns", "m", agg)
		clock = clock.Add(defaultStateTimeout - time.Minute)
		a.NoteVariantsPresent("ns", "m", []string{"agg"})
		Expect(a.EvictStale(clock.Add(2*time.Minute)).Lines).To(BeZero(),
			"parked, so not analyzed, but present")
		Expect(a.lines.Baselines()).To(HaveKey(key))
		stamped := clock
		clock = clock.Add(defaultStateTimeout)
		a.NoteVariantsPresent("ns", "m", []string{"ag"})
		Expect(a.EvictStale(stamped.Add(defaultStateTimeout+2*time.Minute)).Lines).To(Equal(1),
			`"ag" must not keep "agg" alive`)
	})

	It("keys the line on the last resolved accelerator while the pods disagree", func() {
		a := newAt(nil)
		warmWith(a, noop)
		resolved := windowKey("ns", "m", agg)
		unresolved := agg
		unresolved.AcceleratorName = constants.UnresolvedAcceleratorType
		Expect(a.variantKey("ns", "m", unresolved)).To(Equal(resolved))
		moved := agg
		moved.AcceleratorName = "H200"
		Expect(a.variantKey("ns", "m", moved)).NotTo(Equal(resolved), "a resolved change is new hardware")
		fresh := New(nil, nil)
		Expect(fresh.variantKey("ns", "m", unresolved)).To(Equal(windowKey("ns", "m", unresolved)),
			"never resolved: the reading itself")
	})

	It("keeps a parked variant's accelerator memo while it is reported present", func() {
		a := newAt(nil)
		warmWith(a, noop)
		resolved := windowKey("ns", "m", agg)
		clock = clock.Add(defaultStateTimeout - time.Minute)
		a.NoteVariantsPresent("ns", "m", []string{"agg"})
		a.EvictStale(clock.Add(2 * time.Minute))
		unresolved := agg
		unresolved.AcceleratorName = constants.UnresolvedAcceleratorType
		Expect(a.variantKey("ns", "m", unresolved)).To(Equal(resolved),
			"parked past the timeout but present: it wakes keyed where its line is")
	})

	It("forgets an accelerator memo unused for the timeout, and counts it", func() {
		a := newAt(nil)
		warmWith(a, noop)
		ev := a.EvictStale(clock.Add(defaultStateTimeout + time.Minute))
		Expect(ev.Accelerators).To(Equal(1))
		Expect(Evicted{Accelerators: 1}.Any()).To(BeTrue(), "an accelerator memo alone is an eviction to log")
		unresolved := agg
		unresolved.AcceleratorName = constants.UnresolvedAcceleratorType
		Expect(a.variantKey("ns", "m", unresolved)).To(Equal(windowKey("ns", "m", unresolved)),
			"the memo is gone: an unresolved reading keys on itself again")
	})

	It("treats a line no cycle ever stamped as stale", func() {
		a := newAt(nil)
		a.lines.Observe("ns|m|stray|H100|1", "agg", cycleRows("agg", 0), itl.Admission{}, clock)
		Expect(a.EvictStale(clock).Lines).To(Equal(1), "a key with no stamp is not one anything still uses")
	})

	Context("engine params from the capacity store", func() {
		store := func(params capacity.EngineParams) *capacity.Store {
			s := capacity.NewStore()
			s.Update("ns", "m", "agg", capacity.Record{EngineParams: &params})
			return s
		}

		// Issue #287: the line takes a replica at its sequence cap above the
		// band, so priceVariant must hand fitLine the engine's cap -- only
		// when its flag stated it -- and the policy's band.
		It("hands the line the policy's band and the cap the engine's flag stated", func() {
			capped := func() []domain.ReplicaMetrics {
				r := row("agg", "agg-hot", 0.87)
				r.RunningRequests, r.QueueLength, r.PreemptionRateKnown = 64, 768, true
				return []domain.ReplicaMetrics{r}
			}
			price := func(a *Analyzer, cfg *config.ScalingPolicy) itl.Reading {
				in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m", Config: cfg,
					VariantStates: []domain.VariantReplicaState{agg}, ReplicaMetrics: capped()}
				_, _, d := a.priceVariant(in, agg, windowKey("ns", "m", agg), readFleet(in), 0.044, 0.85, clock)
				return d.line
			}
			stated := price(newAt(store(capacity.EngineParams{MaxNumSeqs: 64, MaxNumSeqsSet: true})), policy(44*time.Millisecond, 0))
			Expect(stated.AboveBand).To(BeZero())
			Expect(stated.Capped).To(Equal(1))
			defaulted := price(newAt(store(capacity.EngineParams{MaxNumSeqs: 64})), policy(44*time.Millisecond, 0))
			Expect(defaulted.AboveBand).To(Equal(1), "a parser default is not the engine's cap")
			wide := policy(44*time.Millisecond, 0)
			wide.AnalyzerTuning.ITLBand.MaxObservableK = 0.90
			banded := price(newAt(store(capacity.EngineParams{MaxNumSeqs: 64})), wide)
			Expect(banded.AboveBand).To(BeZero(), "analyzerTuning.itlBand.maxObservableK must reach the line")
			Expect(banded.Offered).To(Equal(1))
		})

		It("caps the resident sequences at max_num_seqs and prices the fill the engine can reach", func() {
			res := warmWith(newAt(store(capacity.EngineParams{MaxNumSeqs: 64})), noop)
			// k = 0.6 implies 120 sequences; the engine admits 64, so the cache
			// never fills past 64 * 500 / 100000 = 0.32, and 64 sequences
			// advance at ITL(0.32) = 32.8 ms, not ITL(0.6).
			Expect(res.VariantCapacities[0].PerReplicaCapacity).To(BeNumerically("~", 64/line.ITLAt(0.32), 1e-6))
		})

		It("reads a replica that says it runs one rank exactly as one that says nothing", func() {
			res := warmWith(newAt(store(capacity.EngineParams{MaxNumSeqs: 64})),
				func(rm *domain.ReplicaMetrics) { rm.EngineRanks = 1 })
			Expect(res.VariantCapacities[0].PerReplicaCapacity).To(BeNumerically("~", 64/line.ITLAt(0.32), 1e-6))
		})

		// Issue #150: max_num_seqs bounds one engine. A replica of eight
		// data-parallel ranks admits eight times it, against KV that is
		// already the whole replica's.
		It("caps a data-parallel replica at max_num_seqs per rank, not per replica", func() {
			res := warmWith(newAt(store(capacity.EngineParams{MaxNumSeqs: 16})),
				func(rm *domain.ReplicaMetrics) { rm.EngineRanks = 8 })
			// k = 0.6 implies 120 sequences; eight ranks admit 8 x 16 = 128,
			// so the cap does not bind. Capped at one rank's 16, the fill
			// would stop at 16 * 500 / 100000 = 0.08 and the replica be priced
			// at 16 sequences.
			Expect(res.VariantCapacities[0].PerReplicaCapacity).To(BeNumerically("~", 120/0.044, 1e-6))
		})

		It("scales the configured KV override, which sizes one engine, by the replica's ranks", func() {
			res := warmWith(newAt(store(capacity.EngineParams{TotalKvTokensOverride: 100000})),
				func(rm *domain.ReplicaMetrics) { rm.TotalKvCapacityTokens = 0; rm.EngineRanks = 2 })
			// Two ranks of 100000: 0.6 * 200000 / 500 = 240 sequences.
			Expect(res.VariantCapacities[0].PerReplicaCapacity).To(BeNumerically("~", 240/0.044, 1e-6))
		})

		It("reads the configured KV override where the live reading is absent or the overflow sentinel", func() {
			for _, live := range []int64{0, math.MaxInt64} {
				res := warmWith(newAt(store(capacity.EngineParams{TotalKvTokensOverride: 200000})),
					func(rm *domain.ReplicaMetrics) { rm.TotalKvCapacityTokens = live })
				// 0.6 * 200000 / 500 = 240 sequences.
				Expect(res.VariantCapacities[0].PerReplicaCapacity).To(BeNumerically("~", 240/0.044, 1e-6),
					"live capacity %d", live)
			}
		})

		It("averages the capacity over the variant's ready replicas", func() {
			res := warmWith(newAt(nil), func(rm *domain.ReplicaMetrics) {
				if rm.PodName == "agg-b" {
					rm.TotalKvCapacityTokens = 300000
				}
			})
			// The mean of 100k and 300k: 0.6 * 200000 / 500 = 240 sequences.
			Expect(res.VariantCapacities[0].PerReplicaCapacity).To(BeNumerically("~", 240/0.044, 1e-6))
		})
	})

	It("is input-invalid, an error and not no-data, when the variant has no KV capacity at all", func() {
		a := newAt(nil)
		res := warmWith(a, func(rm *domain.ReplicaMetrics) { rm.TotalKvCapacityTokens = 0 })
		Expect(res.VariantCapacities[0].Reason).To(Equal(domain.ReasonError))
		Expect(res.VariantCapacities[0].PerReplicaCapacity).To(BeZero())

		zeroCap := row("agg", "agg-a", 0.5)
		zeroCap.TotalKvCapacityTokens = 0
		_, reading, _ := a.priceVariant(domain.AnalyzerInput{Namespace: "ns", ModelID: "m"}, agg,
			windowKey("ns", "m", agg), readFleet(domain.AnalyzerInput{VariantStates: []domain.VariantReplicaState{agg},
				ReplicaMetrics: []domain.ReplicaMetrics{zeroCap}}),
			0.044, 0.85, clock)
		Expect(reading.Source).To(Equal(constants.SLOSourceInputInvalid))
	})

	It("holds B at the last full fit's intercept once the fleet stops spanning k", func() {
		a := newAt(nil)
		// The true intercept here is 30 ms, far from the 6 ms default floor.
		steep := func(k float64) float64 { return 0.040*k + 0.030 }
		key := windowKey("ns", "m", agg)
		var src string
		for i := range 5 {
			rows := cycleRows("agg", i)
			for j := range rows {
				rows[j].AvgITL = steep(rows[j].KvUsageInstant)
			}
			_, src, _ = a.fitLine(key, rows, "agg", itl.Admission{}, clock)
			clock = clock.Add(15 * time.Second)
		}
		Expect(src).To(Equal(constants.SLOSourceOwn))
		b, _ := a.lines.Baseline(key)
		Expect(b).To(BeNumerically("~", 0.030, 1e-9))

		// Readings at nearly one fill, for longer than a steady reading may
		// outlive the window (itl.DefaultSteadyCarry), push the spread-out
		// ones out, so only a pinned fit is possible.
		var m itl.Model
		for range int(itl.DefaultSteadyCarry/(15*time.Second)) + 2 {
			rows := []domain.ReplicaMetrics{row("agg", "agg-a", 0.50), row("agg", "agg-b", 0.52)}
			for j := range rows {
				rows[j].AvgITL = steep(rows[j].KvUsageInstant)
			}
			m, src, _ = a.fitLine(key, rows, "agg", itl.Admission{}, clock)
			clock = clock.Add(15 * time.Second)
		}
		Expect(src).To(Equal(constants.SLOSourcePinned))
		Expect(m.B).To(BeNumerically("~", 0.030, 1e-9), "the learned intercept, not itl.DefaultBaselineSec")
	})

	It("splits demand across generating variants by their ready replicas, and reports utilization", func() {
		a := newAt(nil)
		two := domain.VariantReplicaState{VariantName: "agg2", Role: domain.RoleBoth,
			AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 1}
		var res *domain.AnalyzerResult
		for i := range 5 {
			rows := append(cycleRows("agg", i), row("agg2", "agg2-a", 0.20+0.1*float64(i)))
			var err error
			res, err = a.Analyze(ctx, domain.AnalyzerInput{
				ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 3,
				VariantStates: []domain.VariantReplicaState{agg, two}, ReplicaMetrics: rows,
			})
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		Expect(res.TotalDemand).To(BeNumerically("~", 3000, 1e-9))
		byName := map[string]domain.VariantCapacity{}
		for _, vc := range res.VariantCapacities {
			byName[vc.VariantName] = vc
		}
		Expect(byName["agg"].TotalDemand).To(BeNumerically("~", 2000, 1e-9))
		Expect(byName["agg2"].TotalDemand).To(BeNumerically("~", 1000, 1e-9))
		vc := byName["agg"]
		Expect(vc.PerReplicaCapacity).To(BeNumerically(">", 0))
		Expect(vc.Utilization).To(BeNumerically("~", vc.TotalDemand/(2*vc.PerReplicaCapacity), 1e-12))
	})

	It("reports utilization over the Ready replicas, not those still starting", func() {
		a := newAt(nil)
		starting := agg
		starting.CurrentReplicas, starting.PendingReplicas = 3, 1 // two Ready rows, one Pod starting
		var res *domain.AnalyzerResult
		for i := range 5 {
			var err error
			res, err = a.Analyze(ctx, domain.AnalyzerInput{
				ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
				VariantStates: []domain.VariantReplicaState{starting}, ReplicaMetrics: cycleRows("agg", i),
			})
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		vc := res.VariantCapacities[0]
		Expect(vc.ObservedReplicas).To(Equal(2))
		Expect(vc.PerReplicaCapacity).To(BeNumerically(">", 0))
		Expect(vc.Utilization).To(BeNumerically("~", vc.TotalDemand/(2*vc.PerReplicaCapacity), 1e-12))
	})

	It("prices the realised fill at the slo entry's own threshold when it sets one", func() {
		predicted := func(cfg *config.ScalingPolicy) float64 {
			registry := prometheus.NewRegistry()
			Expect(metrics.InitMetrics(registry)).To(Succeed())
			a := newAt(nil)
			for i := range 5 {
				_, err := a.Analyze(ctx, domain.AnalyzerInput{
					ModelID: "m", Namespace: "ns", Config: cfg, ArrivalRate: 2,
					VariantStates: []domain.VariantReplicaState{agg}, ReplicaMetrics: cycleRows("agg", i),
				})
				Expect(err).NotTo(HaveOccurred())
				clock = clock.Add(15 * time.Second)
			}
			v, ok := gaugeIn(registry, constants.WVASLOPredictedITLSeconds, map[string]string{constants.LabelVariantName: "agg"})
			Expect(ok).To(BeTrue())
			return v
		}
		tier := predicted(policy(44*time.Millisecond, 0))
		own := policy(44*time.Millisecond, 0)
		half := 0.5
		own.Analyzers[1].ScaleUpThreshold = &half
		Expect(predicted(own)).To(BeNumerically("<", tier),
			"a lower threshold runs the fleet emptier, so its predicted ITL is lower")
	})

	It("reads the shape from the rows whose figures are all finite", func() {
		good := row("agg", "agg-a", 0.5)
		bad := row("agg", "agg-b", 0.5)
		bad.AvgInputTokens = math.NaN()
		f := readFleet(domain.AnalyzerInput{VariantStates: []domain.VariantReplicaState{agg},
			ReplicaMetrics: []domain.ReplicaMetrics{good, bad}})
		Expect(finite(f.shape.AvgInputTokens)).To(BeTrue(), "one NaN row must not make the fleet's shape NaN")
		Expect(f.shape.AvgOutputTokens).To(Equal(good.AvgOutputTokens))
	})

	It("does not weigh the shape by a request rate that is not finite", func() {
		good := row("agg", "agg-a", 0.5)
		bad := row("agg", "agg-b", 0.5)
		bad.RequestRate, bad.AvgOutputTokens = math.Inf(1), 3000
		f := readFleet(domain.AnalyzerInput{VariantStates: []domain.VariantReplicaState{agg},
			ReplicaMetrics: []domain.ReplicaMetrics{good, bad}})
		Expect(f.shape.AvgOutputTokens).To(Equal(good.AvgOutputTokens), "an infinite weight made the shape NaN")
	})

	It("keeps state for the timeout it is given, not the default", func() {
		a := newAt(nil).WithStateTimeout(72 * time.Hour)
		Expect(a.StateTimeout()).To(Equal(72 * time.Hour))
		warmWith(a, noop)
		Expect(a.EvictStale(clock.Add(defaultStateTimeout+time.Hour)).Any()).To(BeFalse(),
			"LEARNED_STATE_VARIANT_TIMEOUT of 72h keeps a line a day old")
		Expect(a.EvictStale(clock.Add(72*time.Hour + time.Minute)).Lines).To(Equal(1))
		Expect(New(nil, nil).WithStateTimeout(0).StateTimeout()).To(Equal(defaultStateTimeout))
	})
})

var _ = Describe("judge's sample minimums and labels", func() {
	judge := func(rl *domain.RouterLatency, slo *config.SLOPolicy) []metrics.SLOObjectiveReading {
		return New(fakeRouter{latency: rl}, nil).judge(context.Background(),
			domain.AnalyzerInput{ModelID: "m", Namespace: "ns"}, slo)
	}

	It("reports no TPOT mean from fewer than 50 requests, and none from zero", func() {
		for _, rate := range []float64{0.1, 0} { // 30 requests, then none
			out := judge(&domain.RouterLatency{WindowSeconds: 300, TPOTSumRate: rate * 0.08, TPOTCountRate: rate},
				policy(100*time.Millisecond, 0).SLO)
			Expect(math.IsNaN(out[0].Observed)).To(BeTrue(), "rate %v", rate)
		}
	})

	It("counts 0 TTFT requests for an idle model, and none for a malformed histogram", func() {
		// The router reported its histogram and served nobody: a count of 0.
		// Absent would read as "the router's series are missing", which fires
		// WVAModelSLOUnjudged on every idle night.
		idle := judge(&domain.RouterLatency{WindowSeconds: 300, TTFTBuckets: buckets(1, 0, 3, 0, inf, 0)},
			policy(0, 3*time.Second).SLO)
		Expect(idle[0].Requests).To(Equal(0.0))
		Expect(math.IsNaN(idle[0].Observed)).To(BeTrue())
		// No +Inf bucket: unreadable, not idle.
		bad := judge(&domain.RouterLatency{WindowSeconds: 300, TTFTBuckets: buckets(1, 0, 3, 0)},
			policy(0, 3*time.Second).SLO)
		Expect(math.IsNaN(bad[0].Requests)).To(BeTrue())
	})

	It("reports no TPOT mean from a count with no time behind it", func() {
		// 300 requests and a sum of 0: half a reading, which as a mean of 0
		// would count as a met TPOT.
		out := judge(&domain.RouterLatency{WindowSeconds: 300, TPOTCountRate: 1}, policy(100*time.Millisecond, 0).SLO)
		Expect(math.IsNaN(out[0].Observed)).To(BeTrue())
		Expect(out[0].Requests).To(BeNumerically("~", 300, 1e-9))
	})

	It("labels each objective with its statistic", func() {
		slo := policy(100*time.Millisecond, 3*time.Second).SLO
		slo.TTFT.Percentile = 99
		out := judge(nil, slo)
		Expect(out).To(HaveLen(2))
		Expect(out[0].Objective).To(Equal(constants.SLOObjectiveTPOT))
		Expect(out[0].Statistic).To(Equal("mean"))
		Expect(out[1].Objective).To(Equal(constants.SLOObjectiveTTFT))
		Expect(out[1].Statistic).To(Equal("p99"))
		Expect(out[1].ObjectiveRatio).To(Equal(0.99), "the attainment alert compares with it")
		Expect(out[0].ObjectiveRatio).To(BeZero(), "a mean promises no share")
	})

	It("asks a p99 for a thousand requests", func() {
		slo := policy(0, 3*time.Second).SLO
		slo.TTFT.Percentile = 99
		short := judge(&domain.RouterLatency{WindowSeconds: 300, // 900 requests
			TTFTBuckets: buckets(1, 2.9, 3, 2.97, inf, 3.0)}, slo)
		Expect(math.IsNaN(short[0].Observed)).To(BeTrue())
		enough := judge(&domain.RouterLatency{WindowSeconds: 300, // 1200 requests
			TTFTBuckets: buckets(1, 3.9, 3, 3.96, inf, 4.0)}, slo)
		Expect(enough[0].Observed).To(BeNumerically("~", 3.0, 1e-9))
		Expect(enough[0].Attainment).To(BeNumerically("~", 0.99, 1e-12))
	})
})

var _ = Describe("judge's request counts", func() {
	It("reports how many requests the router saw, even below the minimum", func() {
		slo := policy(100*time.Millisecond, 3*time.Second).SLO
		out := New(fakeRouter{latency: &domain.RouterLatency{WindowSeconds: 300,
			TPOTSumRate: 0.008, TPOTCountRate: 0.1, TTFTBuckets: buckets(1, 0.18, 3, 0.19, inf, 0.2)}}, nil).
			judge(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"}, slo)
		Expect(out[0].Requests).To(BeNumerically("~", 30, 1e-9))
		Expect(math.IsNaN(out[0].Observed)).To(BeTrue())
		Expect(out[1].Requests).To(BeNumerically("~", 60, 1e-9))
		Expect(math.IsNaN(out[1].Observed)).To(BeTrue())
	})

	It("reports no request count when the router reported nothing", func() {
		out := New(fakeRouter{}, nil).judge(context.Background(),
			domain.AnalyzerInput{ModelID: "m", Namespace: "ns"}, policy(0, 3*time.Second).SLO)
		Expect(math.IsNaN(out[0].Requests)).To(BeTrue())
	})
})
