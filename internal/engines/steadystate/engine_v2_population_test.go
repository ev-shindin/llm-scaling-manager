package steadystate

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// fakeAnalyzerWithResult is a minimal domain.Analyzer whose Analyze result
// is set at construction time. Used to inject controlled saturation/spy output
// into runAnalyzersAndScore without needing a real SaturationAnalyzer.
type fakeAnalyzerWithResult struct {
	analyzerName string
	result       *domain.AnalyzerResult
}

func (f *fakeAnalyzerWithResult) Name() string { return f.analyzerName }
func (f *fakeAnalyzerWithResult) Analyze(_ context.Context, _ domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	return f.result, nil
}

var _ = Describe("Engine config-population helpers", func() {

	Describe("AnalyzerScore", func() {
		It("returns the configured score when the analyzer is present with a positive score", func() {
			cfg := config.ScalingPolicy{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "saturation", Score: 2.5},
					{Name: "throughput", Score: 0.5},
				},
			}
			Expect(cfg.AnalyzerScore("saturation")).To(Equal(2.5))
			Expect(cfg.AnalyzerScore("throughput")).To(Equal(0.5))
		})

		It("returns 1.0 when the analyzer is absent from config", func() {
			Expect(config.ScalingPolicy{}.AnalyzerScore("unknown")).To(Equal(1.0))
		})

		It("returns 1.0 when the analyzer's Score is zero (field not set in config)", func() {
			cfg := config.ScalingPolicy{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "saturation", Score: 0},
				},
			}
			Expect(cfg.AnalyzerScore("saturation")).To(Equal(1.0))
		})

		It("returns the first matching score when multiple entries share a name", func() {
			cfg := config.ScalingPolicy{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "sat", Score: 3.0},
					{Name: "sat", Score: 7.0}, // duplicate — first wins
				},
			}
			Expect(cfg.AnalyzerScore("sat")).To(Equal(3.0))
		})
	})

	Describe("AnalyzerEnabled", func() {
		It("returns false when the analyzer is absent from config (opt-in)", func() {
			Expect(config.ScalingPolicy{}.AnalyzerEnabled("throughput")).To(BeFalse())
		})

		It("returns false when other analyzers are configured but the target is absent", func() {
			cfg := config.ScalingPolicy{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "other"},
				},
			}
			Expect(cfg.AnalyzerEnabled("throughput")).To(BeFalse())
		})

		It("returns true when Enabled is nil for the matching entry", func() {
			cfg := config.ScalingPolicy{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "throughput"}, // Enabled nil → default true
				},
			}
			Expect(cfg.AnalyzerEnabled("throughput")).To(BeTrue())
		})

		It("returns false when Enabled is explicitly false", func() {
			f := false
			cfg := config.ScalingPolicy{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "throughput", Enabled: &f},
				},
			}
			Expect(cfg.AnalyzerEnabled("throughput")).To(BeFalse())
		})

		It("returns true when Enabled is explicitly true", func() {
			t := true
			cfg := config.ScalingPolicy{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "throughput", Enabled: &t},
				},
			}
			Expect(cfg.AnalyzerEnabled("throughput")).To(BeTrue())
		})
	})

	// These exercise runAnalyzersAndScore directly (Score
	// population/defaulting, per-analyzer threshold override), the layer
	// beneath composite construction: the composite is built after these
	// values are set.
	Describe("runAnalyzersAndScore config-bridge", func() {

		// minEngine builds a minimal Engine suitable for calling runAnalyzersAndScore.
		// satFake is the saturation analyzer; spies are the analyzers after it.
		minEngine := func(satFake domain.Analyzer, spies ...analyzerEntry) *Engine {
			return &Engine{analyzers: testAnalyzers(satFake, spies...)}
		}

		zeroSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{},
		}

		It("populates Score from AnalyzerScoreConfig.Score into the returned slice", func() {
			spy := &fakeAnalyzerWithResult{
				analyzerName: "spy",
				result:       &domain.AnalyzerResult{},
			}
			e := minEngine(zeroSat, analyzerEntry{name: "spy", analyzer: spy})
			cfg := config.ScalingPolicy{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: domain.SaturationAnalyzerName, Score: 2.0},
					{Name: "spy", Score: 0.5},
				},
			}

			results, err := e.runAnalyzersAndScore(context.Background(), "m", cfg)
			Expect(err).NotTo(HaveOccurred())

			byName := namedByName(results)
			Expect(byName[domain.SaturationAnalyzerName].Score).To(Equal(2.0))
			Expect(byName["spy"].Score).To(Equal(0.5))
		})

		It("defaults Score to 1.0 when the analyzer has no Analyzers entry", func() {
			spy := &fakeAnalyzerWithResult{
				analyzerName: "spy",
				result:       &domain.AnalyzerResult{},
			}
			e := minEngine(zeroSat, analyzerEntry{name: "spy", analyzer: spy})
			cfg := config.ScalingPolicy{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				// spy has an entry (so it participates, opt-in) but no Score — defaults to 1.0.
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "spy"},
				},
			}

			results, err := e.runAnalyzersAndScore(context.Background(), "m", cfg)
			Expect(err).NotTo(HaveOccurred())

			byName := namedByName(results)
			Expect(byName[domain.SaturationAnalyzerName].Score).To(Equal(1.0))
			Expect(byName["spy"].Score).To(Equal(1.0))
		})

		It("applies per-analyzer ScaleUpThreshold override into RequiredCapacity", func() {
			// spy returns TotalDemand=100, everything else zero.
			spy := &fakeAnalyzerWithResult{
				analyzerName: "spy",
				result:       &domain.AnalyzerResult{TotalDemand: 100},
			}
			e := minEngine(zeroSat, analyzerEntry{name: "spy", analyzer: spy})

			// Global ScaleUpThreshold=0.85 → RC = 100/0.85 ≈ 117.6
			cfgGlobal := config.ScalingPolicy{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "spy"},
				},
			}
			resultsGlobal, err := e.runAnalyzersAndScore(context.Background(), "m", cfgGlobal)
			Expect(err).NotTo(HaveOccurred())

			// Per-analyzer ScaleUpThreshold=1.10 → RC = 100/1.10 ≈ 90.9
			overrideThreshold := 1.10
			cfgOverride := config.ScalingPolicy{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "spy", ScaleUpThreshold: &overrideThreshold},
				},
			}
			resultsOverride, err := e.runAnalyzersAndScore(context.Background(), "m", cfgOverride)
			Expect(err).NotTo(HaveOccurred())

			rcGlobal := namedByName(resultsGlobal)["spy"].Remaining
			rcOverride := namedByName(resultsOverride)["spy"].Remaining
			Expect(rcGlobal).To(BeNumerically(">", rcOverride),
				"global threshold (0.85) should yield higher RC than override (1.10)")
		})
	})
})

// namedByName indexes a NamedAnalyzerResult slice by Name for easy lookup in tests.
func namedByName(results []allocation.NamedAnalyzerResult) map[string]allocation.NamedAnalyzerResult {
	m := make(map[string]allocation.NamedAnalyzerResult, len(results))
	for _, r := range results {
		m[r.Name] = r
	}
	return m
}
