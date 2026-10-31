package steadystate

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// testAnalyzers is a started registry holding sat as the saturation analyzer
// (when non-nil) and then entries, in order. An entry named saturation is
// skipped: sat is the saturation analyzer.
func testAnalyzers(sat domain.Analyzer, entries ...analyzerEntry) *analyzerRegistry {
	r := newAnalyzerRegistry()
	if sat != nil {
		if err := r.register(domain.SaturationAnalyzerName, sat); err != nil {
			panic(err)
		}
	}
	for _, en := range entries {
		if en.name == domain.SaturationAnalyzerName {
			continue
		}
		if err := r.register(en.name, en.analyzer); err != nil {
			panic(err)
		}
	}
	r.start()
	return r
}

// replaceBuiltin replaces the built-in analyzer registered under name, in
// its place, or appends it when none is. Replacing saturation resolves its
// capabilities again. A test's injection point: production never replaces a
// built-in.
func (r *analyzerRegistry) replaceBuiltin(name string, a domain.Analyzer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	replaced := false
	for i := range r.builtin {
		if r.builtin[i].name == name {
			r.builtin[i].analyzer = a
			replaced = true
		}
	}
	if !replaced {
		r.builtin = append(r.builtin, analyzerEntry{name: name, analyzer: a})
	}
	if name == domain.SaturationAnalyzerName {
		r.saturation = saturationCapabilitiesOf(a)
	}
}

// setSaturation replaces the saturation analyzer of e's registry, creating a
// started registry when e has none.
func (e *Engine) setSaturation(a domain.Analyzer) {
	if e.analyzers == nil {
		e.analyzers = testAnalyzers(nil)
	}
	e.analyzers.replaceBuiltin(domain.SaturationAnalyzerName, a)
}

// saturationAnalyzer is the saturation analyzer registered in e.
func (e *Engine) saturationAnalyzer() domain.Analyzer {
	return e.analyzers.saturationCaps().analyzer
}

// buildInputAndRunSaturation is what runAnalyzers does before the other
// analyzers, for model "m" in namespace "ns" with no rows, states, metadata,
// scheduler queue or arrival rate: the input builder, then the saturation
// analyzer on the input it built.
func (e *Engine) buildInputAndRunSaturation(
	ctx context.Context,
	policy config.ScalingPolicy,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*domain.AnalyzerResult, error) {
	input := e.buildAnalyzerInput(ctx, "m", "ns", nil, policy, nil, nil, scaleTargets, variantAutoscalings, nil, 0)
	return e.runSaturation(ctx, input)
}
