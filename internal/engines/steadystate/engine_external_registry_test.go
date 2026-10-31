package steadystate

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

var _ = Describe("external analyzer runtime registry", func() {
	makeEngine := func() *Engine {
		sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
		return &Engine{analyzers: testAnalyzers(sat)}
	}

	cfgWithExt := config.ScalingPolicy{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.70,
		Analyzers: []config.AnalyzerScoreConfig{
			{Name: domain.SaturationAnalyzerName},
			{Name: "ext-demand"},
		},
	}

	extAnalyzer := func() *fakeAnalyzerWithResult {
		return &fakeAnalyzerWithResult{analyzerName: "ext-demand", result: &domain.AnalyzerResult{AnalyzerName: "ext-demand"}}
	}

	names := func(rs []allocation.NamedAnalyzerResult) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return out
	}

	run := func(e *Engine, cfg config.ScalingPolicy) []allocation.NamedAnalyzerResult {
		results, err := e.runAnalyzersAndScore(context.Background(), "m", cfg)
		Expect(err).NotTo(HaveOccurred())
		return results
	}

	It("runs an external analyzer upserted at runtime when it is enabled in config", func() {
		Skip("WIP single-analyzer refactor: non-saturation analyzer results not yet forwarded to optimizer; rewrite once the multi-analyzer story is redesigned")
		e := makeEngine()
		e.UpsertExternalAnalyzer("ext-demand", extAnalyzer())
		Expect(names(run(e, cfgWithExt))).To(ContainElement("ext-demand"))
	})

	It("stops running it after RemoveExternalAnalyzer", func() {
		e := makeEngine()
		e.UpsertExternalAnalyzer("ext-demand", extAnalyzer())
		e.RemoveExternalAnalyzer("ext-demand")
		Expect(names(run(e, cfgWithExt))).NotTo(ContainElement("ext-demand"))
	})

	It("does not run an upserted analyzer that is absent from config (opt-in)", func() {
		e := makeEngine()
		e.UpsertExternalAnalyzer("ext-demand", extAnalyzer())
		cfgNoExt := config.ScalingPolicy{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			Analyzers:         []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}},
		}
		Expect(names(run(e, cfgNoExt))).NotTo(ContainElement("ext-demand"))
	})

	It("does not duplicate a built-in when an external name collides with it", func() {
		e := makeEngine()
		e.UpsertExternalAnalyzer(domain.SaturationAnalyzerName, extAnalyzer())
		count := 0
		for _, n := range names(run(e, cfgWithExt)) {
			if n == domain.SaturationAnalyzerName {
				count++
			}
		}
		Expect(count).To(Equal(1))
	})
})

var _ = Describe("toExternalDefinition", func() {
	It("builds per-engine bodies with parsed thresholds", func() {
		d, err := toExternalDefinition("ttft", config.ExternalAnalyzerDef{
			Engines: map[string]config.ExternalAnalyzerBody{
				"vllm":   {Query: "qv", Threshold: "0.5"},
				"sglang": {Query: "qs", Threshold: "0.4"},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Label).To(Equal("ttft"))
		Expect(d.Bodies["vllm"].Query).To(Equal("qv"))
		Expect(d.Bodies["vllm"].Threshold).To(Equal(0.5))
		Expect(d.Bodies["sglang"].Threshold).To(Equal(0.4))
	})

	It("builds an engine-agnostic body from top-level query/threshold", func() {
		d, err := toExternalDefinition("pool", config.ExternalAnalyzerDef{Query: "q", Threshold: "1.0"})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Bodies).To(HaveKey(""))
		Expect(d.Bodies[""].Threshold).To(Equal(1.0))
	})

	It("errors on an unparseable threshold", func() {
		_, err := toExternalDefinition("bad", config.ExternalAnalyzerDef{Query: "q", Threshold: "not-a-number"})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("reconcileExternalAnalyzers", func() {
	var (
		e   *Engine
		cfg *config.Config
	)

	BeforeEach(func() {
		reg := source.NewSourceRegistry()
		Expect(reg.Register("prometheus", prometheus.NewPrometheusSource(context.Background(), nil, prometheus.DefaultPrometheusSourceConfig()))).To(Succeed())
		cfg = config.NewTestConfig()
		sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
		e = &Engine{
			Config:          cfg,
			metricsRegistry: reg,
			analyzers:       testAnalyzers(sat),
		}
	})

	It("registers a catalog analyzer and retires it when it leaves the catalog", func() {
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			config.GlobalDefaultsKey: {AnalyzerDefinitions: config.ExternalAnalyzerCatalog{
				"ttft-slo": {Query: "q", Threshold: "0.5"},
			}},
		})
		e.reconcileExternalAnalyzers(context.Background())
		Expect(e.externalAnalyzerNames()).To(ContainElement("ttft-slo"))

		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			config.GlobalDefaultsKey: {AnalyzerDefinitions: config.ExternalAnalyzerCatalog{}},
		})
		e.reconcileExternalAnalyzers(context.Background())
		Expect(e.externalAnalyzerNames()).NotTo(ContainElement("ttft-slo"))
	})

	It("skips a catalog label that collides with a built-in", func() {
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			config.GlobalDefaultsKey: {AnalyzerDefinitions: config.ExternalAnalyzerCatalog{
				domain.SaturationAnalyzerName: {Query: "q", Threshold: "0.5"},
			}},
		})
		e.reconcileExternalAnalyzers(context.Background())
		Expect(e.externalAnalyzerNames()).NotTo(ContainElement(domain.SaturationAnalyzerName))
	})

	It("says so once, at the default level, when a built-in takes a catalog entry's name", func() {
		// slo became a built-in: a catalog that already had an analyzer of that
		// name stops running it, and that must be in a default log.
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			config.GlobalDefaultsKey: {AnalyzerDefinitions: config.ExternalAnalyzerCatalog{
				domain.SaturationAnalyzerName: {Query: "q", Threshold: "0.5"},
			}},
		})
		var lines []string
		ctx := logr.NewContext(context.Background(), funcr.New(func(_, args string) {
			lines = append(lines, args)
		}, funcr.Options{Verbosity: 0}))
		for range 3 {
			e.reconcileExternalAnalyzers(ctx)
		}
		n := 0
		for _, l := range lines {
			if strings.Contains(l, "the built-in runs instead") {
				n++
			}
		}
		Expect(n).To(Equal(1), "%v", lines)
	})

	It("skips a malformed definition (unparseable threshold)", func() {
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			config.GlobalDefaultsKey: {AnalyzerDefinitions: config.ExternalAnalyzerCatalog{
				"bad": {Query: "q", Threshold: "nope"},
			}},
		})
		e.reconcileExternalAnalyzers(context.Background())
		Expect(e.externalAnalyzerNames()).NotTo(ContainElement("bad"))
	})
})
