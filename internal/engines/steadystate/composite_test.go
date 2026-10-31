package steadystate

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// satOnlyEngine builds a minimal Engine with only saturation registered,
// returning satResult from Analyze every cycle.
func satOnlyEngine(satResult *domain.AnalyzerResult) *Engine {
	fakeSat := &fakeAnalyzerWithResult{
		analyzerName: domain.SaturationAnalyzerName,
		result:       satResult,
	}
	return &Engine{
		analyzers: testAnalyzers(fakeSat,
			analyzerEntry{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
		),
	}
}

var scaleCfg = config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70}

// Regression guard: with saturation alone, the composite must be numerically
// identical to saturation's own entry (namedResults[0]).
//
// scaleCfg (above) has an empty Analyzers slice: a policy with no analyzers
// section runs saturation alone (config.ScalingPolicy.SaturationListed), so
// saturation is the sole contributor (DecisionSingle). The case where it is
// listed explicitly is covered separately below.
var _ = Describe("buildComposite — sat-only regression, no analyzers section", func() {

	It("is numerically identical to saturation's own entry when the policy lists no analyzers", func() {
		satResult := &domain.AnalyzerResult{
			AnalyzerName: domain.SaturationAnalyzerName,
			ModelID:      "m",
			Namespace:    "ns",
			TotalDemand:  1000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", ReplicaCount: 3, PerReplicaCapacity: 200, Reason: "P1-obs"},
			},
		}
		e := satOnlyEngine(satResult)

		// The baseline: saturation's own entry, namedResults[0] from
		// runAnalyzersAndScore.
		namedResults, err := e.runAnalyzersAndScore(context.Background(), "m", scaleCfg)
		Expect(err).NotTo(HaveOccurred())
		baseline := namedResults[0]

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		composite := req.CompositeSignal

		Expect(composite.Result.VariantCapacities[0].Reason).To(Equal(string(allocation.DecisionSingle)),
			"a policy with no analyzers section lists saturation alone")
		Expect(composite.Holds).To(BeNil())
		Expect(composite.Result.TotalDemand).To(Equal(baseline.Result.TotalDemand))
		Expect(composite.Result.VariantCapacities).To(HaveLen(len(baseline.Result.VariantCapacities)))
		Expect(composite.Result.VariantCapacities[0].PerReplicaCapacity).
			To(Equal(baseline.Result.VariantCapacities[0].PerReplicaCapacity),
				"PRC_com must equal PRC_sat exactly on the sat-only path, no analyzers section")
		Expect(composite.TotalSupply).To(Equal(baseline.TotalSupply))
		Expect(composite.TotalAnticipatedSupply).To(Equal(baseline.TotalAnticipatedSupply))
		Expect(composite.Utilization).To(Equal(baseline.Utilization))
		Expect(composite.RequiredCapacity).To(Equal(baseline.RequiredCapacity))
		Expect(composite.SpareCapacity).To(Equal(baseline.SpareCapacity))
		Expect(composite.Remaining).To(Equal(baseline.Remaining))
		Expect(composite.Spare).To(Equal(baseline.Spare))
		Expect(composite.Live).To(Equal(baseline.Live))
	})

	It("holds even for a zero-demand SO with a real, measured PRC (PRC_com undefined at N_com=0, but supply must not vanish)", func() {
		satResult := &domain.AnalyzerResult{
			AnalyzerName: domain.SaturationAnalyzerName,
			ModelID:      "m",
			Namespace:    "ns",
			TotalDemand:  0, // zero demand: N_sat(v1) = 0/250 = 0, PRC_com formally undefined
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", ReplicaCount: 2, PerReplicaCapacity: 250, Reason: "P1-obs"},
			},
		}
		e := satOnlyEngine(satResult)

		namedResults, err := e.runAnalyzersAndScore(context.Background(), "m", scaleCfg)
		Expect(err).NotTo(HaveOccurred())
		baseline := namedResults[0]

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		composite := req.CompositeSignal

		// demand==0 flows through to the composite as a literal 0, never
		// manufactured into a placeholder.
		Expect(composite.Result.TotalDemand).To(BeZero(),
			"zero demand must flow through to the composite as a literal 0, never manufactured")

		// The critical assertion: PRC must NOT be zeroed just because demand
		// is zero (PRC and demand are independent) -- the
		// composite must fall through to the SO's own measured PRC when
		// PRC_com is undefined, preserving real supply exactly.
		Expect(composite.Result.VariantCapacities[0].PerReplicaCapacity).
			To(Equal(250.0), "a real measured PRC must survive even when N_com is 0")
		Expect(composite.TotalSupply).To(Equal(baseline.TotalSupply))
		Expect(composite.SpareCapacity).To(Equal(baseline.SpareCapacity))
	})

	It("has the composite's own name, not saturation's", func() {
		satResult := &domain.AnalyzerResult{
			TotalDemand:       100,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 100, Reason: "P1-obs"}},
		}
		e := satOnlyEngine(satResult)
		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(req.CompositeSignal.Name).To(Equal(allocation.CompositeSignalName))
		Expect(req.CompositeSignal.Name).ToNot(Equal(domain.SaturationAnalyzerName))
	})
})

// Regression guard: the same sat-sole-contributor PRC identity as the block
// above, with saturation listed explicitly: satEnabledCfg lists
// domain.SaturationAnalyzerName in Analyzers, and with no other analyzer
// registered, sat is the loop's sole contributor.
var satEnabledCfg = config.ScalingPolicy{
	ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
	Analyzers: []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}},
}

var _ = Describe("buildComposite — sat-only regression, DecisionSingle path", func() {

	It("is numerically identical to saturation's own entry when saturation is explicitly enabled and the sole analyzer (DecisionSingle)", func() {
		satResult := &domain.AnalyzerResult{
			AnalyzerName: domain.SaturationAnalyzerName,
			ModelID:      "m",
			Namespace:    "ns",
			TotalDemand:  1000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", ReplicaCount: 3, PerReplicaCapacity: 200, Reason: "P1-obs"},
			},
		}
		e := satOnlyEngine(satResult)

		// The baseline: saturation's own entry, namedResults[0] from
		// runAnalyzersAndScore.
		namedResults, err := e.runAnalyzersAndScore(context.Background(), "m", satEnabledCfg)
		Expect(err).NotTo(HaveOccurred())
		baseline := namedResults[0]

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, satEnabledCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		composite := req.CompositeSignal

		Expect(composite.Result.VariantCapacities[0].Reason).To(Equal(string(allocation.DecisionSingle)),
			"sat is enabled and the sole analyzer, so this SO reaches PRC identity through the ordinary contributor loop with exactly one contributor")
		Expect(composite.Result.TotalDemand).To(Equal(baseline.Result.TotalDemand))
		Expect(composite.Result.VariantCapacities).To(HaveLen(len(baseline.Result.VariantCapacities)))
		Expect(composite.Result.VariantCapacities[0].PerReplicaCapacity).
			To(Equal(baseline.Result.VariantCapacities[0].PerReplicaCapacity),
				"PRC_com must equal PRC_sat exactly on the sat-only path, DecisionSingle case")
		Expect(composite.TotalSupply).To(Equal(baseline.TotalSupply))
		Expect(composite.TotalAnticipatedSupply).To(Equal(baseline.TotalAnticipatedSupply))
		Expect(composite.Utilization).To(Equal(baseline.Utilization))
		Expect(composite.RequiredCapacity).To(Equal(baseline.RequiredCapacity))
		Expect(composite.SpareCapacity).To(Equal(baseline.SpareCapacity))
		Expect(composite.Remaining).To(Equal(baseline.Remaining))
		Expect(composite.Spare).To(Equal(baseline.Spare))
		Expect(composite.Live).To(Equal(baseline.Live))
	})
})

// End-to-end through the real compose path (not a hand-built fixture): a
// composite whose only SO has no usable signal must report
// CompositeHasSignal == false, so the quota guard fires. An analyzer's own
// no-data/error sentinel strings are NOT the composite's decision-path
// markers (DecisionNoSignal == "C4-no-signal"), so reusing the
// analyzer-level informativeness check on the composite's Reason would
// always report "usable" -- silently defeating this gate.
var _ = Describe("buildComposite — no-signal end to end", func() {
	It("reports CompositeHasSignal == false when saturation itself has no-data for the only SO", func() {
		satResult := &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", PerReplicaCapacity: 0, Reason: allocation.ReasonNoData},
			},
		}
		e := satOnlyEngine(satResult)
		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())

		Expect(req.CompositeSignal.Result.VariantCapacities[0].Reason).To(Equal(string(allocation.DecisionNoSignal)))
		Expect(allocation.CompositeHasSignal(req.CompositeSignal)).To(BeFalse())
	})
})

// Deep-copy isolation. Mutating the composite must never reach back into
// saturation's own result.
var _ = Describe("buildComposite — deep-copy isolation", func() {
	It("does not mutate the source saturation entry when the composite's VariantCapacities are edited", func() {
		satResult := &domain.AnalyzerResult{
			TotalDemand: 500,
			RoleDemand:  map[string]float64{"prefill": 500},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p1", Role: "prefill", ReplicaCount: 2, PerReplicaCapacity: 250, Reason: "P1-obs"},
			},
		}
		e := satOnlyEngine(satResult)

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		composite := req.CompositeSignal

		// Mutate the composite's copies.
		composite.Result.VariantCapacities[0].PerReplicaCapacity = 999999
		composite.Result.RoleDemand["prefill"] = 999999
		if composite.RoleCapacities != nil {
			rc := composite.RoleCapacities["prefill"]
			rc.TotalDemand = 999999
			composite.RoleCapacities["prefill"] = rc
		}
		if composite.RoleSpare != nil {
			composite.RoleSpare["prefill"] = 999999
		}

		Expect(satResult.VariantCapacities[0].PerReplicaCapacity).To(Equal(250.0),
			"the source analyzer's own VariantCapacities must be untouched")
		Expect(satResult.RoleDemand["prefill"]).To(Equal(500.0),
			"the source analyzer's own RoleDemand must be untouched")
	})

	It("does not alias RoleCapacities/RoleSpare across two composites built from the same cycle's inputs", func() {
		satResult := &domain.AnalyzerResult{
			TotalDemand: 500,
			RoleDemand:  map[string]float64{"prefill": 500},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p1", Role: "prefill", ReplicaCount: 2, PerReplicaCapacity: 250, Reason: "P1-obs"},
			},
		}
		e := satOnlyEngine(satResult)

		req1, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		req2, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())

		if req1.CompositeSignal.RoleCapacities != nil {
			rc := req1.CompositeSignal.RoleCapacities["prefill"]
			rc.TotalDemand = 42
			req1.CompositeSignal.RoleCapacities["prefill"] = rc
			Expect(req2.CompositeSignal.RoleCapacities["prefill"].TotalDemand).ToNot(Equal(42.0))
		}
	})
})

// Regression guard: Score has no effect on the composite's sizing.
var _ = Describe("buildComposite — Score has no effect", func() {
	It("produces the identical composite for scores 1,1 and for scores 1,5", func() {
		makeEngine := func() *Engine {
			fakeSat := &fakeAnalyzerWithResult{
				analyzerName: domain.SaturationAnalyzerName,
				result: &domain.AnalyzerResult{
					TotalDemand:       1000,
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", ReplicaCount: 2, PerReplicaCapacity: 200, Reason: "P1-obs"}},
				},
			}
			spy := &fakeAnalyzerWithResult{
				analyzerName: "spy",
				result: &domain.AnalyzerResult{
					TotalDemand:       2400, // higher N than sat: N=12 vs sat's N=5
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", ReplicaCount: 2, PerReplicaCapacity: 200, Reason: "measured"}},
				},
			}
			return &Engine{
				analyzers: testAnalyzers(fakeSat,
					analyzerEntry{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
					analyzerEntry{name: "spy", analyzer: spy},
				),
			}
		}

		equalScores := config.ScalingPolicy{
			ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: domain.SaturationAnalyzerName, Score: 1.0},
				{Name: "spy", Score: 1.0},
			},
		}
		skewedScores := config.ScalingPolicy{
			ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: domain.SaturationAnalyzerName, Score: 1.0},
				{Name: "spy", Score: 5.0},
			},
		}

		e1 := makeEngine()
		req1, err := e1.collectV2ModelRequest(context.Background(), "m", "ns", nil, equalScores, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())

		e2 := makeEngine()
		req2, err := e2.collectV2ModelRequest(context.Background(), "m", "ns", nil, skewedScores, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())

		c1, c2 := req1.CompositeSignal, req2.CompositeSignal
		Expect(c1.Result.VariantCapacities[0].PerReplicaCapacity).
			To(Equal(c2.Result.VariantCapacities[0].PerReplicaCapacity),
				"Score must not affect PRC_com (N_com is a pure max, no weighting)")
		Expect(c1.RequiredCapacity).To(Equal(c2.RequiredCapacity))
		Expect(c1.SpareCapacity).To(Equal(c2.SpareCapacity))
		Expect(c1.TotalSupply).To(Equal(c2.TotalSupply))
		// The Score field itself DOES differ (the max over the analyzers'
		// Scores) -- that is the one place Score is allowed to show up, and
		// the per-variant aggregation never reads it.
		Expect(c1.Score).To(Equal(1.0))
		Expect(c2.Score).To(Equal(5.0))
	})
})

// End-to-end collectV2ModelRequest -> optimizer with nonzero demand.
var _ = Describe("buildComposite — end-to-end with a higher-demand contributor", func() {
	It("raises the composite's replica sizing above saturation's own when another analyzer demands more", func() {
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				TotalDemand:       1000,
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 200, Reason: "P1-obs"}}, // N=5
			},
		}
		spy := &fakeAnalyzerWithResult{
			analyzerName: "spy",
			result: &domain.AnalyzerResult{
				TotalDemand:       4000,
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 200, Reason: "measured"}}, // N=20
			},
		}
		e := &Engine{
			analyzers: testAnalyzers(fakeSat,
				analyzerEntry{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
				analyzerEntry{name: "spy", analyzer: spy},
			),
		}
		cfg := config.ScalingPolicy{
			ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
			Analyzers: []config.AnalyzerScoreConfig{{Name: "spy"}},
		}

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		composite := req.CompositeSignal

		// N_com = max(5, 20) = 20 -> demand 20 x 200 = 4000 at saturation's
		// own PRC, which is kept.
		Expect(composite.Result.VariantCapacities[0].PerReplicaCapacity).To(Equal(200.0))
		Expect(composite.Result.TotalDemand).To(Equal(4000.0))
		Expect(composite.RequiredCapacity).To(BeNumerically(">", 0))
	})
})

// The composite writes its result back in the SAME layout it read demand in
// (the read half is aggregation.DemandForRole's tests) -- a non-disaggregated
// model's composite has RoleDemand == nil with demand in TotalDemand, never
// a synthetic "both" map key, and a disaggregated model's composite has a
// real RoleDemand map.
var _ = Describe("buildComposite — write-back layout parity", func() {
	It("writes RoleDemand == nil with demand in TotalDemand for a non-disaggregated source", func() {
		satResult := &domain.AnalyzerResult{
			TotalDemand: 500,
			RoleDemand:  nil, // non-disaggregated layout
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", Role: "", ReplicaCount: 2, PerReplicaCapacity: 100, Reason: "P1-obs"},
			},
		}
		e := satOnlyEngine(satResult)
		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		composite := req.CompositeSignal

		Expect(composite.Result.RoleDemand).To(BeNil(),
			"a non-disaggregated composite must not synthesize a RoleDemand map, e.g. a \"both\" key")
		Expect(composite.Result.TotalDemand).To(Equal(500.0))
	})

	It("writes a real RoleDemand map for a disaggregated source", func() {
		satResult := &domain.AnalyzerResult{
			RoleDemand: map[string]float64{"prefill": 400, "decode": 900},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p1", Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 100, Reason: "P1-obs"},
				{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 300, Reason: "P1-obs"},
			},
		}
		e := satOnlyEngine(satResult)
		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		composite := req.CompositeSignal

		Expect(composite.Result.RoleDemand).To(HaveKeyWithValue("prefill", 400.0))
		Expect(composite.Result.RoleDemand).To(HaveKeyWithValue("decode", 900.0))
	})
})
