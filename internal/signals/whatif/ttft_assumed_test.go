package whatif

import "testing"

// The callers' stop sits inside the model's bound, so a spread clamped to it
// is priced and never refused.
func TestPromptCVStop_IsInsideTheModelsBound(t *testing.T) {
	stop, bound := float64(PromptCVStop), float64(maxPromptCV)
	if stop >= bound {
		t.Errorf("PromptCVStop %g, want below the model's bound %g", stop, bound)
	}
}

// A whole percent, however it is written as a fraction, reads its own row: S1
// holds a percent and the CLI a fraction, and both call this.
func TestPromptCVPeak_AWholePercentReadsItsOwnRow(t *testing.T) {
	for percent := range 101 {
		want := float64(PromptCVStop)
		switch i := percent - 50; {
		case i < 0:
			want = 0
		case i < len(promptCVPeaks):
			want = promptCVPeaks[i]
		}
		if got := PromptCVPeak(float64(percent) / 100); got != want {
			t.Errorf("p%d: PromptCVPeak = %g, want %g", percent, got, want)
		}
	}
}

// Between whole percents it reads the one below, the lower peak.
func TestPromptCVPeak_BetweenPercentsReadsTheOneBelow(t *testing.T) {
	if got := PromptCVPeak(0.905); got != 2.22 {
		t.Errorf("p90.5: PromptCVPeak = %g, want 2.22", got)
	}
}
