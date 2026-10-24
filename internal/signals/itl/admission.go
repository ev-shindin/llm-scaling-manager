package itl

import (
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Admission is what decides, per Observe, which replica rows are readings:
// the KV band and the sequence-cap exception above it. The zero value is the
// defaults with no cap known, which admits nothing above the band.
type Admission struct {
	// MinK and MaxK are the band's edges; zero takes DefaultMinObservableK
	// and DefaultMaxObservableK. The policy keys analyzerTuning.itlBand.minObservableK and
	// analyzerTuning.itlBand.maxObservableK set them (config.ScalingPolicy.ITLBand), which
	// validates 0 < MinK < MaxK < 1.
	MinK, MaxK float64
	// MaxNumSeqs is the engine's max_num_seqs, per rank, when it was read
	// from the engine's flag: capacity.EngineParams.ExplicitMaxNumSeqs. Zero
	// when it is unknown or only a default, which admits nothing above MaxK.
	MaxNumSeqs int64
	// CapMaxPreemptionRate is the most preemptions per second a capped
	// replica may show over the ITL's window and still be read above MaxK.
	// The policy key analyzerTuning.itlBand.capMaxPreemptionRate sets it; zero, the default,
	// requires none.
	CapMaxPreemptionRate float64
	// Steady is the steady-state gate: which readings teach the full fit and
	// the pinned one. The policy's analyzerTuning.steadyGate sets it; a zero
	// field takes its default (DefaultSteadiness).
	Steady Steadiness
}

// band is the admission's KV band, with the defaults for an edge left at zero.
func (a Admission) band() (lo, hi float64) {
	lo, hi = a.MinK, a.MaxK
	if !(lo > 0) {
		lo = DefaultMinObservableK
	}
	if !(hi > 0) {
		hi = DefaultMaxObservableK
	}
	return lo, hi
}

// atSequenceCap reports whether the replica's batch is at its sequence cap,
// with no more preemption over the ITL's window than CapMaxPreemptionRate, so
// its reading above the band is a point on its line (issue #287).
//
// The band stops at 0.80 to keep an engine thrashing out of the fit: the
// readings above it steepen A and push B below zero, and preemption tracks
// them (see DefaultMaxObservableK). A replica whose batch is at max_num_seqs
// is not short of memory. It cannot take another sequence however much cache
// is free, and a fill above the band is its sequences' contexts, not
// pressure. Under uneven routing that replica can be the only one with a k
// in reach: on one run a decode replica held 256 running (its max_num_seqs)
// and 768 waiting at k 0.85-0.89 for 20 minutes while three siblings sat
// below 0.15, and no line was ever fitted.
//
// Every condition is required, and an unknown one admits nothing:
//   - MaxNumSeqs is known from the engine's flag, not a default: vLLM V1
//     defaults to 1024 on large accelerators where the parser assumes 256,
//     and a batch of 300 would read as capped while it is still growing.
//   - The running batch is EXACTLY the cap on every rank (max_num_seqs bounds
//     one engine, so the cap is it times Ranks). More than the cap means the
//     cap is not what was read; fewer means memory, not the cap, binds.
//   - Requests are waiting: a batch at the cap with nothing queued is not
//     held there by the cap.
//   - The preemption rate was read and is at most CapMaxPreemptionRate: an
//     engine preempting for memory is the case the band exists for.
func (a Admission) atSequenceCap(rm domain.ReplicaMetrics) bool {
	return a.MaxNumSeqs > 0 &&
		int64(rm.RunningRequests) == a.MaxNumSeqs*int64(rm.Ranks()) &&
		rm.QueueLength > 0 &&
		rm.PreemptionRateKnown && !math.IsNaN(rm.PreemptionRate) &&
		rm.PreemptionRate <= max(a.CapMaxPreemptionRate, 0)
}
