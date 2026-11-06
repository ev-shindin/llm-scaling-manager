package whatif

import "math"

// What the TTFT model's callers, the slo analyzer's S1 and the what-if CLI,
// share (docs/proposals/slo-tiers.md, "S1 decisions"): the figures they pass
// for an input with no reading, the stop they clamp a measured spread to, and
// the share they raise the routing flags at. The model itself reads none of
// them: it prices what it is given.
const (
	// AssumedPromptCV is the prompt spread with no computed-prompt histogram:
	// not measured, the top of the spread taken as plausible. No committed
	// recording carries that histogram.
	AssumedPromptCV = 2.0
	// PromptCVStop is the most spread a caller passes, and where
	// promptCVPeaks ends. It is a stop, not the value that prices the longest
	// TTFT: at no load that is the peak, 2.22 at p90, and past 3 the p90 falls
	// fast, to 1.96 times the mean by cv 4. It is not maxPromptCV, the model's
	// own bound, past which the gamma says nothing and the call is refused.
	PromptCVStop = 3.0
	// ImbalanceFloor is the least f assumed on more than one pod. Not
	// measured: it is the TTFT model's own synthetic multi-pod case, near P1's
	// 2:1:1:1 example (1.6). The prefill-stress run at three pods read a raw
	// share of at most 1.33 over its loaded cycles, so it held there. At light
	// load the raw share is chance: the same run read 3.00 at 0.06 r/s.
	ImbalanceFloor = 1.5
	// RoutingBoundShare is the f / N_p at which one pod takes the work and
	// more pods would mostly sit idle behind the router. Not measured.
	RoutingBoundShare = 0.8
	// FlowControlQueueThreshold and FlowControlKVThreshold are the EPP
	// detector's upstream defaults, which this repository's install leaves
	// unchanged, assumed with the gate on: nothing recorded reads them.
	FlowControlQueueThreshold = 5.0
	FlowControlKVThreshold    = 0.8
)

// TTFTWindowCycles is how many cycles the callers hold the raw share and the
// decode engine's TTFT over (docs/proposals/slo-tiers.md, "S1 decisions").
const TTFTWindowCycles = 5

// promptCVPeaks is, from p50 to p93, the cv at which a mean-one gamma's quantile
// peaks on [0, 3], to 0.01. From p94 it is still rising at 3. Below p50 the
// figure is a median, under the mean at any cv, so its peak is cv 0.
var promptCVPeaks = [...]float64{
	0, 0.04, 0.08, 0.11, 0.15, 0.19, 0.23, 0.26, 0.30, 0.34,
	0.38, 0.42, 0.46, 0.50, 0.54, 0.58, 0.62, 0.66, 0.70, 0.74,
	0.79, 0.83, 0.87, 0.92, 0.97, 1.02, 1.07, 1.12, 1.18, 1.24,
	1.30, 1.36, 1.43, 1.50, 1.58, 1.66, 1.75, 1.85, 1.96, 2.08,
	2.22, 2.38, 2.56, 2.78,
}

// PromptCVPeak is the cv that prices the longest prompt at percentile q, a
// fraction, at no load, read at the whole percent at or below q: the lower
// peak, which flags sooner. The 1e-9 keeps a whole percent on its own row,
// since 0.57 * 100 is 56.99999999999999.
func PromptCVPeak(q float64) float64 {
	i := int(math.Floor(100*q+1e-9)) - 50
	switch {
	case i < 0:
		return 0
	case i >= len(promptCVPeaks):
		return PromptCVStop
	}
	return promptCVPeaks[i]
}
