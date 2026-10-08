package steadystate

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// v2Variant is one variant as these integration fixtures state it: the identity
// discovery resolves and the capacity signal the analyzer measures, together in
// one literal. withSatEntryV2 splits them the way the engine does.
type v2Variant struct {
	name        string
	accelerator string
	cost        float64
	role        string
	replicas    int
	prc         float64
}

// v2Variants splits the fixture list into the two halves a ModelScalingRequest
// carries, and returns them ready to attach.
func v2Variants(vs []v2Variant) ([]domain.VariantMetadata, []domain.VariantCapacity) {
	meta := make([]domain.VariantMetadata, 0, len(vs))
	caps := make([]domain.VariantCapacity, 0, len(vs))
	for _, v := range vs {
		meta = append(meta, domain.VariantMetadata{
			VariantName: v.name, AcceleratorName: v.accelerator, Cost: v.cost, Role: v.role,
		})
		caps = append(caps, domain.VariantCapacity{
			VariantName: v.name, Role: v.role, ReplicaCount: v.replicas, PerReplicaCapacity: v.prc,
		})
	}
	return meta, caps
}

// withSatEntryV2 sets req's single-saturation CompositeSignal, along with
// the discovery metadata the optimizer reads variant identity from.
// Mirrors the helper in cost_aware_optimizer_test.go for use in this package.
func withSatEntryV2(rc, sc float64, vs []v2Variant, req allocation.ModelScalingRequest) allocation.ModelScalingRequest {
	meta, caps := v2Variants(vs)
	req.Variants = meta
	req.CompositeSignal = allocation.NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Result: &domain.AnalyzerResult{
			ModelID:           req.ModelID,
			Namespace:         req.Namespace,
			VariantCapacities: caps,
		},
		RequiredCapacity: rc,
		SpareCapacity:    sc,
		Remaining:        rc,
		Spare:            sc,
		Live:             true,
	}
	return req
}

var _ = Describe("V2 Engine Integration", func() {

	Context("CostAwareOptimizer via engine path", func() {

		It("should scale up cheapest variant by cost-efficiency", func() {
			optimizer := allocation.NewCostAwareOptimizer()
			variants := []v2Variant{
				{name: "variant-cheap", accelerator: "A100", cost: 5.0, replicas: 2, prc: 10000},
				{name: "variant-expensive", accelerator: "H100", cost: 15.0, replicas: 1, prc: 20000},
			}
			requests := []allocation.ModelScalingRequest{
				withSatEntryV2(5000, 0, variants, allocation.ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 2},
						{VariantName: "variant-expensive", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			// cost-efficiency: cheap=5/10000=0.0005, expensive=15/20000=0.00075
			// cheap is more cost-efficient, ceil(5000/10000)=1
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(1))
		})

		It("should scale down most expensive variant", func() {
			optimizer := allocation.NewCostAwareOptimizer()
			variants := []v2Variant{
				{name: "variant-cheap", cost: 5.0, replicas: 3, prc: 10000},
				{name: "variant-expensive", cost: 15.0, replicas: 2, prc: 20000},
			}
			requests := []allocation.ModelScalingRequest{
				withSatEntryV2(0, 25000, variants, allocation.ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 3},
						{VariantName: "variant-expensive", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(1))
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
		})

		It("should protect cheapest variant at 1 during scale-down", func() {
			optimizer := allocation.NewCostAwareOptimizer()
			variants := []v2Variant{
				{name: "variant-expensive", cost: 15.0, replicas: 1, prc: 20000},
				{name: "variant-cheap", cost: 5.0, replicas: 1, prc: 10000},
			}
			requests := []allocation.ModelScalingRequest{
				withSatEntryV2(0, 30000, variants, allocation.ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "variant-expensive", CurrentReplicas: 1},
						{VariantName: "variant-cheap", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(0))
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(1))
		})

		It("should not skip variants with pending replicas", func() {
			optimizer := allocation.NewCostAwareOptimizer()
			variants := []v2Variant{
				{name: "variant-cheap", cost: 5.0, replicas: 2, prc: 10000},
				{name: "variant-mid", cost: 10.0, replicas: 1, prc: 15000},
			}
			requests := []allocation.ModelScalingRequest{
				withSatEntryV2(5000, 0, variants, allocation.ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 2, PendingReplicas: 1},
						{VariantName: "variant-mid", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			// cheap has pending but is more cost-efficient → still gets allocation
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["variant-mid"].TargetReplicas).To(Equal(1))
		})
	})
})

var _ = Describe("getRoleFromScaleTarget", func() {

	It("should return 'both' for nil scale target", func() {
		Expect(getRoleFromScaleTarget(nil)).To(Equal("both"))
	})

	It("should return 'both' for scale target without labels", func() {
		deploy := &appsv1.Deployment{}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("both"))
	})

	It("should return 'prefill' for prefill label", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "prefill",
		}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("prefill"))
	})

	It("should return 'decode' for decode label", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "decode",
		}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("decode"))
	})

	It("should return 'both' for unknown role value", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "unknown",
		}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("both"))
	})

	It("should return 'both' when no role label present", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"app": "vllm",
		}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("both"))
	})
})

var _ = Describe("resolveScalingPolicy", func() {

	It("should merge model-specific override onto default", func() {
		configMap := map[string]config.ScalingPolicy{
			"default": {
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				AnalyzerName:         "saturation",
			},
			"llama-70b-override": {
				ModelID: "llama-70b", Namespace: "production",
				KvCacheThreshold: 0.85,
				Priority:         5.0,
			},
		}
		cfg := config.ResolveScalingPolicy(configMap, "llama-70b", "production")
		// Overridden fields
		Expect(cfg.KvCacheThreshold).To(Equal(0.85))
		Expect(cfg.Priority).To(Equal(5.0))
		// Inherited from default
		Expect(cfg.QueueLengthThreshold).To(Equal(5.0))
		Expect(cfg.AnalyzerName).To(Equal("saturation"))
	})

	It("should fall back to default config when model-specific not found", func() {
		configMap := map[string]config.ScalingPolicy{
			"default": {
				KvCacheThreshold: 0.80,
				AnalyzerName:     "saturation",
			},
		}
		cfg := config.ResolveScalingPolicy(configMap, "unknown-model", "default")
		Expect(cfg.KvCacheThreshold).To(Equal(0.80))
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
	})

	It("should return V1 defaults when map is empty", func() {
		configMap := map[string]config.ScalingPolicy{}
		cfg := config.ResolveScalingPolicy(configMap, "model-1", "ns-1")
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
		Expect(cfg.KvCacheThreshold).To(Equal(config.DefaultKvCacheThreshold))
		Expect(cfg.QueueLengthThreshold).To(Equal(config.DefaultQueueLengthThreshold))
	})

	It("should apply defaults on model-specific config", func() {
		configMap := map[string]config.ScalingPolicy{
			"model-1-override": {
				ModelID: "model-1", Namespace: "ns-1",
				AnalyzerName: "saturation",
			},
		}
		cfg := config.ResolveScalingPolicy(configMap, "model-1", "ns-1")
		Expect(cfg.ScaleUpThreshold).To(Equal(config.DefaultScaleUpThreshold))
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
		// V1 defaults also applied
		Expect(cfg.KvCacheThreshold).To(Equal(config.DefaultKvCacheThreshold))
	})

	It("should allow partial override with only one field changed", func() {
		configMap := map[string]config.ScalingPolicy{
			"default": {
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
			},
			"model-1-override": {
				ModelID: "model-1", Namespace: "ns-1",
				KvCacheThreshold: 0.90,
			},
		}
		cfg := config.ResolveScalingPolicy(configMap, "model-1", "ns-1")
		Expect(cfg.KvCacheThreshold).To(Equal(0.90))
		Expect(cfg.QueueLengthThreshold).To(Equal(5.0))
		// A V1-style entry stays V1 (selection is decided globally, not here), but the
		// RESOLVED config is calibrated post-merge so that if the global default routes
		// this model to the V2 path it runs with valid thresholds rather than zeros
		// (which would disable the scale-up/scale-down post-step).
		Expect(cfg.IsV2()).To(BeFalse())
		Expect(cfg.ScaleUpThreshold).To(Equal(config.DefaultScaleUpThreshold))
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
	})

	It("should not let a V1-style override clobber a tuned global V2 threshold (production parse order)", func() {
		// Regression guard: entries are ApplyDefaults()'d individually at parse time
		// before storage (see parseScalingPolicyConfig). Build the map that way, then
		// resolve. A V1-style override that omits scaleUpThreshold must INHERIT the
		// operator-tuned global 0.95, not silently revert to the 0.85 default.
		def := config.ScalingPolicy{
			Analyzers:        []config.AnalyzerScoreConfig{{Name: "saturation"}},
			ScaleUpThreshold: 0.95, // operator tuned away from the 0.85 default
			KvCacheThreshold: 0.80,
		}
		def.ApplyDefaults()
		// Identity in the body: the key is arbitrary, and a slashed model ID could
		// never be a legal ConfigMap key.
		override := config.ScalingPolicy{
			ModelID: "meta/llama-70b", Namespace: "production",
			KvCacheThreshold: 0.90, // V1-style, no V2 thresholds
		}
		override.ApplyDefaults()
		configMap := map[string]config.ScalingPolicy{
			"default":        def,
			"llama-override": override,
		}
		cfg := config.ResolveScalingPolicy(configMap, "meta/llama-70b", "production")
		Expect(cfg.KvCacheThreshold).To(Equal(0.90))
		Expect(cfg.ScaleUpThreshold).To(Equal(0.95), "tuned global scaleUpThreshold must survive a V1-style override")
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
	})

	It("should default the sibling V2 threshold post-merge for a fully V1-style namespace map", func() {
		// Namespace-local map that is V1-style end-to-end (no analyzers anywhere), as
		// when a tenant ships their own saturation ConfigMap. Global selection routes
		// these models to V2. The override sets ONLY scaleUpThreshold; the merged
		// config's IsV2() stays false, so ApplyV2ThresholdDefaults() is the ONLY thing
		// that fills the missing scaleDownBoundary — this test fails if that post-merge
		// call is removed (the explicit ApplyDefaults V2-branch never runs here).
		def := config.ScalingPolicy{KvCacheThreshold: 0.80} // V1-style, no analyzers
		def.ApplyDefaults()
		override := config.ScalingPolicy{ScaleUpThreshold: 0.90} // only scaleUp set
		override.ApplyDefaults()
		override.ModelID, override.Namespace = "model-1", "ns-1"
		configMap := map[string]config.ScalingPolicy{
			"default":          def,
			"model-1-override": override,
		}
		cfg := config.ResolveScalingPolicy(configMap, "model-1", "ns-1")
		Expect(cfg.IsV2()).To(BeFalse())
		Expect(cfg.ScaleUpThreshold).To(Equal(0.90), "explicit override must win")
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary), "missing sibling must be defaulted post-merge")
	})

	It("should reset an inverted V2 threshold pair produced by a cross-entry merge", func() {
		// Base scaleUpThreshold 0.85; a V1-style override raises scaleDownBoundary above
		// it. Each entry is valid on its own (so load-time validation passes), but the
		// merged pair is inverted — resolveScalingPolicy must fall back to defaults
		// rather than feed the optimizer scaleUp <= scaleDown.
		def := config.ScalingPolicy{
			Analyzers:        []config.AnalyzerScoreConfig{{Name: "saturation"}},
			KvCacheThreshold: 0.80,
		}
		def.ApplyDefaults() // scaleUp=0.85, scaleDown=0.70
		override := config.ScalingPolicy{ScaleDownBoundary: 0.95}
		override.ApplyDefaults()
		override.ModelID, override.Namespace = "model-1", "ns-1"
		configMap := map[string]config.ScalingPolicy{
			"default":          def,
			"model-1-override": override,
		}
		cfg := config.ResolveScalingPolicy(configMap, "model-1", "ns-1")
		Expect(cfg.ScaleUpThreshold).To(Equal(config.DefaultScaleUpThreshold))
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
		Expect(cfg.ScaleUpThreshold).To(BeNumerically(">", cfg.ScaleDownBoundary))
	})
})

var _ = Describe("runAnalyzersAndScore call ordering", func() {

	It("calls each enabled non-saturation analyzer exactly once in registration order", func() {
		Skip("WIP single-analyzer refactor: non-saturation analyzer results not yet forwarded to optimizer; rewrite once the multi-analyzer story is redesigned")
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{},
		}
		ta := &spyAnalyzer{name: "throughput"}
		slo := &spyAnalyzer{name: "slo"}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot: []analyzerEntry{
				{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
				{name: "throughput", analyzer: ta},
				{name: "slo", analyzer: slo},
			},
			started: true,
		}
		cfg := config.ScalingPolicy{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: "throughput"},
				{Name: "slo"},
			},
		}

		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(results).NotTo(BeEmpty())
		Expect(ta.callCount).To(Equal(1))
		Expect(slo.callCount).To(Equal(1))
		// saturationV2Analyzer is called via runV2AnalysisOnly, not the loop;
		// the snapshot entry for saturation is skipped by the name guard.
		Expect(fakeSat.Name()).To(Equal(domain.SaturationAnalyzerName)) // sanity
	})
})

var _ = Describe("runAnalyzersAndScore disabled-analyzer gate", func() {

	It("disabled analyzer is not appended and its Analyze is never called", func() {
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{},
		}
		spy := &spyAnalyzer{name: "spy"}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot: []analyzerEntry{
				{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
				{name: "spy", analyzer: spy},
			},
			started: true,
		}
		f := false
		cfg := config.ScalingPolicy{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: "spy", Enabled: &f},
			},
		}

		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)).To(HaveKey(domain.SaturationAnalyzerName), "only saturation entry — disabled spy must not appear")
		Expect(spy.callCount).To(Equal(0), "Analyze must not be called for a disabled analyzer")
	})
})

var _ = Describe("collectV2ModelRequest Disaggregated flag", func() {

	It("sets Disaggregated=true when any variant has a non-both role", func() {
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{},
		}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot: []analyzerEntry{
				{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
			},
			started: true,
		}
		cfg := config.ScalingPolicy{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		variantStates := []domain.VariantReplicaState{
			{VariantName: "prefill-v1", Role: "prefill"},
			{VariantName: "decode-v1", Role: "decode"},
		}

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, variantStates, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(req.Disaggregated).To(BeTrue())
	})

	It("sets Disaggregated=false when all variants have role 'both' or empty", func() {
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{},
		}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot: []analyzerEntry{
				{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
			},
			started: true,
		}
		cfg := config.ScalingPolicy{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		variantStates := []domain.VariantReplicaState{
			{VariantName: "v1", Role: domain.RoleBoth},
			{VariantName: "v2", Role: ""},
		}

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, variantStates, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(req.Disaggregated).To(BeFalse())
	})
})

// A dropped assignment here would give every model the default weight, and
// nothing else would notice: the shadow evaluation builds on these fields.
var _ = Describe("collectV2ModelRequest weight", func() {
	collect := func(cfg config.ScalingPolicy) *allocation.ModelScalingRequest {
		fakeSat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot:    []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: fakeSat}},
			started:              true,
		}
		cfg.ScaleUpThreshold, cfg.ScaleDownBoundary = 0.85, 0.70
		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg,
			[]domain.VariantReplicaState{{VariantName: "v1"}}, nil, nil, nil, nil, 0)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return req
	}

	It("carries the model's weight class", func() {
		Expect(collect(config.ScalingPolicy{WeightClass: "critical"}).WeightClass).To(Equal("critical"))
	})

	It("carries the model's numeric weight", func() {
		Expect(collect(config.ScalingPolicy{Weight: config.NewModelWeight(3)}).Weight).To(Equal(config.NewModelWeight(3)))
	})
})

func decisionsByVariant(decisions []domain.VariantDecision) map[string]domain.VariantDecision {
	m := make(map[string]domain.VariantDecision, len(decisions))
	for _, d := range decisions {
		m[d.VariantName] = d
	}
	return m
}
