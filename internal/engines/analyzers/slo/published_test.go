package slo

import (
	"context"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// gaugeIn returns the value of the first series of name whose labels include
// every pair in match, and whether there is one.
func gaugeIn(registry *prometheus.Registry, name string, match map[string]string) (float64, bool) {
	mfs, err := registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			for k, v := range match {
				found := false
				for _, l := range m.GetLabel() {
					if l.GetName() == k && l.GetValue() == v {
						found = true
					}
				}
				if !found {
					continue series
				}
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// What the analyzer publishes, read back from a registry: the review of #148
// found specs that asserted on the result and never on the snapshot, so a
// published fill, ITL or source could be wrong with every spec green.
var _ = Describe("What Analyze publishes", func() {
	var (
		registry *prometheus.Registry
		clock    time.Time
		agg      = domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth,
			AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 2}
	)
	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
		clock = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	})
	warm := func(a *Analyzer, slo time.Duration, mutate func(*domain.ReplicaMetrics)) *domain.AnalyzerResult {
		a.now = func() time.Time { return clock }
		var res *domain.AnalyzerResult
		for i := range 5 {
			rows := cycleRows("agg", i)
			for j := range rows {
				mutate(&rows[j])
			}
			var err error
			res, err = a.Analyze(context.Background(), domain.AnalyzerInput{
				ModelID: "m", Namespace: "ns", Config: policy(slo, 0), ArrivalRate: 2,
				VariantStates: []domain.VariantReplicaState{agg}, ReplicaMetrics: rows,
			})
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		return res
	}
	noop := func(*domain.ReplicaMetrics) {}
	agg1 := map[string]string{constants.LabelVariantName: "agg"}
	sourceIs := func(want string) {
		for _, s := range constants.SLOSources {
			v, ok := gaugeIn(registry, constants.WVASLOSource,
				map[string]string{constants.LabelVariantName: "agg", constants.LabelSource: s})
			Expect(ok).To(BeTrue(), s)
			if s == want {
				Expect(v).To(Equal(1.0), s)
			} else {
				Expect(v).To(BeZero(), s)
			}
		}
	}

	It("publishes the priced fill, tagged tpot, and the ITL at the realised fill", func() {
		warm(New(nil, nil), 44*time.Millisecond, noop)
		sourceIs(constants.SLOSourceOwn)
		fill, ok := gaugeIn(registry, constants.WVASLOFill,
			map[string]string{constants.LabelVariantName: "agg", constants.LabelObjective: constants.SLOObjectiveTPOT})
		Expect(ok).To(BeTrue())
		Expect(fill).To(BeNumerically("~", 0.6, 1e-9))
		itlAt, ok := gaugeIn(registry, constants.WVASLOPredictedITLSeconds, agg1)
		Expect(ok).To(BeTrue())
		realised := realisedFill(defaultBand, line, 0.6, 0.85, 100000, 500, 0)
		Expect(itlAt).To(BeNumerically("~", line.ITLAt(realised), 1e-9))
		Expect(itlAt).To(BeNumerically("<", line.ITLAt(0.6)), "the headroom must lower the ITL actually run at")
	})

	It("publishes the fill the engine can reach when max_num_seqs binds", func() {
		s := capacity.NewStore()
		s.Update("ns", "m", "agg", capacity.Record{EngineParams: &capacity.EngineParams{MaxNumSeqs: 64}}, time.Now())
		warm(New(nil, s), 44*time.Millisecond, noop)
		fill, _ := gaugeIn(registry, constants.WVASLOFill, agg1)
		Expect(fill).To(BeNumerically("~", 0.32, 1e-9))
	})

	It("publishes no-output-length and no fill when no replica reports an output length", func() {
		warm(New(nil, nil), 44*time.Millisecond, func(rm *domain.ReplicaMetrics) { rm.AvgOutputTokens = 0 })
		sourceIs(constants.SLOSourceNoOutputLength)
		_, ok := gaugeIn(registry, constants.WVASLOFill, agg1)
		Expect(ok).To(BeFalse())
		_, ok = gaugeIn(registry, constants.WVASLOPredictedITLSeconds, agg1)
		Expect(ok).To(BeFalse())
	})

	It("prices nothing and publishes no variant series for a policy with no TPOT target", func() {
		a := New(nil, nil)
		a.now = func() time.Time { return clock }
		var res *domain.AnalyzerResult
		for i := range 5 {
			var err error
			res, err = a.Analyze(context.Background(), domain.AnalyzerInput{
				ModelID: "m", Namespace: "ns", Config: policy(0, 3*time.Second), ArrivalRate: 2,
				VariantStates: []domain.VariantReplicaState{agg}, ReplicaMetrics: cycleRows("agg", i),
			})
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		Expect(res.VariantCapacities[0].Reason).To(Equal(domain.ReasonNoData), "not an error: there is nothing to price")
		_, ok := gaugeIn(registry, constants.WVASLOSource, agg1)
		Expect(ok).To(BeFalse())
		_, ok = gaugeIn(registry, constants.WVASLOTargetSeconds,
			map[string]string{constants.LabelObjective: constants.SLOObjectiveTTFT})
		Expect(ok).To(BeTrue())
	})

	It("passes the variant's identity and replica state through", func() {
		a := New(nil, nil)
		a.now = func() time.Time { return clock }
		v := agg
		v.PendingReplicas, v.StuckReplicas, v.PendingAges = 2, 1, []float64{30, 90}
		res, err := a.Analyze(context.Background(), domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
			VariantStates: []domain.VariantReplicaState{v}, ReplicaMetrics: cycleRows("agg", 0),
		})
		Expect(err).NotTo(HaveOccurred())
		vc := res.VariantCapacities[0]
		Expect(vc.Role).To(Equal(domain.RoleBoth))
		Expect(vc.PendingReplicas).To(Equal(2))
		Expect(vc.StuckReplicas).To(Equal(1))
		Expect(vc.PendingAges).To(Equal([]float64{30, 90}))
	})
})

var _ = Describe("The fleet's shape and capacity", func() {
	It("weights the shape by request rate and discounts the prompt by the prefix hit rate", func() {
		rows := []domain.ReplicaMetrics{
			{VariantName: "v", Ready: true, RequestRate: 3, AvgOutputTokens: 1000, AvgInputTokens: 2000, PrefixCacheHitRate: 0.5},
			{VariantName: "v", Ready: true, RequestRate: 1, AvgOutputTokens: 2000, AvgInputTokens: 2000, PrefixCacheHitRate: 0.5},
			// Neither a pool Pod nor an unready one describes the fleet's requests.
			{VariantName: "v", Ready: true, FromWarmPool: true, RequestRate: 100, AvgOutputTokens: 99999},
			{VariantName: "v", Ready: false, RequestRate: 100, AvgOutputTokens: 99999},
		}
		f := readFleet(domain.AnalyzerInput{
			VariantStates:  []domain.VariantReplicaState{{VariantName: "v", Role: domain.RoleBoth}},
			ReplicaMetrics: rows,
		})
		Expect(f.shape.AvgOutputTokens).To(BeNumerically("~", 1250, 1e-9))
		// ILeff = 2000 * (1 - 0.5) = 1000; KVreq = ILeff + O/2 = 1625.
		Expect(f.shape.KVreq).To(BeNumerically("~", 1625, 1e-9))
		Expect(f.ready["v"]).To(Equal(2))
		Expect(f.requestRate).To(BeNumerically("~", 4, 1e-12))
	})

	It("averages capacity over the variant's own ready replicas only", func() {
		rows := []domain.ReplicaMetrics{
			{VariantName: "v", Ready: true, TotalKvCapacityTokens: 100000},
			{VariantName: "v", Ready: true, TotalKvCapacityTokens: 300000},
			{VariantName: "other", Ready: true, TotalKvCapacityTokens: 9000000},
			{VariantName: "v", Ready: false, TotalKvCapacityTokens: 9000000},
			{VariantName: "v", Ready: true, FromWarmPool: true, TotalKvCapacityTokens: 9000000},
		}
		f := fleetReading{rows: rows}
		Expect(f.capacityTokens("v", nil)).To(BeNumerically("~", 200000, 1e-9))
	})

	It("keys a line by namespace, model, variant, accelerator and GPU count", func() {
		base := domain.VariantReplicaState{VariantName: "v", AcceleratorName: "H100", GPUsPerReplica: 1}
		k := windowKey("ns", "m", base)
		other := base
		other.AcceleratorName = "H200"
		gpus := base
		gpus.GPUsPerReplica = 8
		renamed := base
		renamed.VariantName = "w"
		Expect(map[string]bool{
			k:                             true,
			windowKey("ns2", "m", base):   true,
			windowKey("ns", "m2", base):   true,
			windowKey("ns", "m", other):   true,
			windowKey("ns", "m", gpus):    true,
			windowKey("ns", "m", renamed): true,
		}).To(HaveLen(6))
	})

	It("does not price one model from another model's readings", func() {
		a := New(nil, nil)
		clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return clock }
		v := domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth, CurrentReplicas: 2}
		for i := range 5 {
			_, err := a.Analyze(context.Background(), domain.AnalyzerInput{
				ModelID: "a", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
				VariantStates: []domain.VariantReplicaState{v}, ReplicaMetrics: cycleRows("agg", i),
			})
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		res, err := a.Analyze(context.Background(), domain.AnalyzerInput{
			ModelID: "b", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
			VariantStates: []domain.VariantReplicaState{v}, ReplicaMetrics: cycleRows("agg", 0),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.VariantCapacities[0].Reason).To(Equal(domain.ReasonNoData))
		Expect(math.IsNaN(res.VariantCapacities[0].PerReplicaCapacity)).To(BeFalse())
	})
})
