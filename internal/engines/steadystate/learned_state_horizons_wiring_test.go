package steadystate

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The last link in the chain: Config -> NewEngine -> the analyzer's horizons.
//
// Config is tested to read the three keys, and the analyzer is tested to act on
// the horizons it holds. Neither notices if NewEngine stops handing one to the
// other -- the analyzer would quietly run on capacity.DefaultHorizons and every
// other spec in both packages would still pass, which is the whole failure mode
// this commit exists to prevent. So this asserts on the object the engine
// actually built.
var _ = Describe("learned-state horizons reach the analyzer the engine builds", func() {
	newEngineWith := func(cfg *config.Config) *saturation.SaturationAnalyzer {
		sourceRegistry := source.NewSourceRegistry()
		Expect(sourceRegistry.Register("prometheus", source.NewNoOpSource())).To(Succeed())
		engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), nil, sourceRegistry, cfg,
			allocation.NewNoOpLimiter("test"))
		sat, ok := engine.saturationV2Analyzer.(*saturation.SaturationAnalyzer)
		Expect(ok).To(BeTrue(), "the V2 analyzer is the one that owns the horizons")
		return sat
	}

	It("passes the configured horizons through", func() {
		cfg := config.NewTestConfig()
		config.SetLearnedStateHorizonsForTest(cfg, 90*time.Minute, 72*time.Hour, 240*time.Hour)

		Expect(newEngineWith(cfg).Horizons()).To(Equal(capacity.Horizons{
			EpisodeGap:      90 * time.Minute,
			VariantTimeout:  72 * time.Hour,
			BucketRetention: 240 * time.Hour,
		}), "none of these is a default, so a dropped WithHorizons call cannot pass")
	})

	It("falls back to the defaults when nothing is configured", func() {
		Expect(newEngineWith(config.NewTestConfig()).Horizons()).
			To(Equal(capacity.DefaultHorizons()))
	})

	It("floors one unusable value without taking the other two with it", func() {
		cfg := config.NewTestConfig()
		// 24 is what viper returns for a unit-less "24": 24 nanoseconds.
		config.SetLearnedStateHorizonsForTest(cfg, 24, 72*time.Hour, 240*time.Hour)

		h := newEngineWith(cfg).Horizons()
		Expect(h.EpisodeGap).To(Equal(capacity.DefaultHorizons().EpisodeGap))
		Expect(h.VariantTimeout).To(Equal(72 * time.Hour))
		Expect(h.BucketRetention).To(Equal(240 * time.Hour))
	})

	It("does not need a nil-config branch, because NewEngine refuses one first", func() {
		// The horizon block originally guarded cfg != nil. It was dead code:
		// NewEngine panics on a nil config twenty-five lines earlier, so the
		// guard only made it look as though the default could be reached that
		// way. This pins the reason it is gone.
		sourceRegistry := source.NewSourceRegistry()
		Expect(sourceRegistry.Register("prometheus", source.NewNoOpSource())).To(Succeed())
		Expect(func() {
			NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), nil, sourceRegistry, nil,
				allocation.NewNoOpLimiter("test"))
		}).To(PanicWith(ContainSubstring("config is nil in NewEngine")))
	})
})
