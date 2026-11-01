package saturation

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// analyzeConfirmed runs in, and runs it again when the cycle held a k2
// reading (k2_confirm.go), returning the last result: a window's first
// reading waits for a later cycle's, so a test about something else that
// learns k2 from one saturated reading confirms it with the same reading, on
// a second Analyze at the same instant. A cycle that held nothing is run
// once.
func analyzeConfirmed(ctx context.Context, a *SaturationAnalyzer, in domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	res, err := a.Analyze(ctx, in)
	if err != nil {
		return res, err
	}
	a.mu.Lock()
	held := false
	for _, p := range a.k2Pending {
		held = held || p.cycle == a.analyzeSeq
	}
	a.mu.Unlock()
	if !held {
		return res, nil
	}
	return a.Analyze(ctx, in)
}

// analyzeAfterHold runs in once with its log discarded -- the cycle that
// holds the window's first reading (k2_confirm.go) -- and again on ctx,
// returning the second result: a test of what one cycle logs reads the
// confirming cycle's lines alone.
func analyzeAfterHold(ctx context.Context, a *SaturationAnalyzer, in domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if _, err := a.Analyze(logr.NewContext(ctx, logr.Discard()), in); err != nil {
		return nil, err
	}
	return a.Analyze(ctx, in)
}
