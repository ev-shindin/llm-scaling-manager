package itl

import (
	"math"
	"time"
)

// The window's defaults, as the throughput analyzer builds it.
const (
	// DefaultWindowMaxSize is the maximum number of (k, ITL) observations
	// retained in the rolling window. When the window is full, the oldest
	// observation is evicted on each new Add call.
	DefaultWindowMaxSize = 20

	// DefaultObservationMaxAge is the maximum age of an observation in the
	// window. Observations older than this are dropped by Prune, which the
	// owner calls once per cycle before asking Ready -- nothing in Add,
	// Ready or Observations prunes -- so that stale data from a previous
	// load pattern does not contaminate the current fit.
	DefaultObservationMaxAge = 30 * time.Minute

	// DefaultMinSamples is the minimum number of valid observations required
	// before the window is considered Ready for OLS fitting.
	DefaultMinSamples = 10

	// DefaultMinKSpread is the minimum required spread (max_k - min_k) across
	// observations in the window before the window is considered Ready.
	// A spread of at least 0.30 ensures the linear fit spans a meaningful
	// portion of the KV utilization range and is not extrapolating from a
	// single operating point.
	DefaultMinKSpread = 0.30

	// DefaultMinObservableK is the lower bound on KV utilization for accepted
	// observations. Below this threshold the ITL signal is noisy (few
	// concurrent requests, high variance) and unreliable for fitting.
	DefaultMinObservableK = 0.15

	// DefaultMaxObservableK is the upper bound on KV utilization for accepted
	// observations: a replica at 0.80 counts as saturated, and the linear ITL
	// model holds below that and not above it.
	//
	// Measured on the shape-swap benchmark (run P, 869 intervals over 9 decode
	// replicas), fitting ITL(k) = A·k + B over k in [0.15, hi]:
	//
	//	hi     n    A (ms)   B (ms)   mean resid   p95
	//	0.85   96     40.8    -1.91        11.8%   40.1%
	//	0.80   94     37.2    -0.48         8.3%   25.2%
	//	0.75   88     34.4    +0.61         6.4%   16.3%
	//
	// B is the hardware floor at zero contention and cannot be negative; the
	// fit drives it there only because the top of the range is not linear.
	// Per k-decile the median ITL rises ~3 ms per 0.1 up to 0.8 and then
	// breaks away -- 24.6 ms at 0.75, 30.5 at 0.85, 45.0 at 0.95 -- and
	// preemption tracks it: of 7,537 preemptions in that run, 93% happened at
	// k >= 0.7 and 60% at k >= 0.9. Those points describe an engine thrashing,
	// not a replica's sustainable rate, and including them steepens A and
	// pushes B below zero, which makes every mu derived from the line wrong.
	//
	// This is the window's ceiling only. DefaultKSat (0.85) stays the point a
	// fit is EVALUATED at, mirroring DefaultScaleUpThreshold in the saturation
	// config; evaluating 0.05 above the fitted range costs ~2% against the
	// measured median at that band.
	DefaultMaxObservableK = 0.80
)

// Observation is a single (k, ITL_obs) data point collected from one replica
// during one reconcile cycle. Used to calibrate the linear ITL model: ITL(k) = A·k + B.
type Observation struct {
	// K is the KV cache utilization fraction (0.0–1.0) observed on the replica.
	K float64
	// ITLSec is the observed average inter-token latency in seconds/token.
	ITLSec float64
	// Timestamp is when this observation was collected.
	Timestamp time.Time
}

// Window is a rolling window of (k, ITL_obs) pairs for one variant.
// It accumulates observations from all replicas of the variant across reconcile
// cycles to calibrate the linear ITL model: ITL(k) = A·k + B.
//
// The caller clears the window when the workload shape changes (see the
// throughput analyzer and shape.Tracker) and prunes it by age (Prune).
// Observations outside the valid k range [minK, maxK] are silently ignored.
//
// Not safe for concurrent use: the owner serialises access.
type Window struct {
	observations []Observation
	maxSize      int
	maxAge       time.Duration
	minSamples   int
	minKSpread   float64
	minK         float64
	maxK         float64
}

// NewWindow creates a window with the given configuration.
func NewWindow(maxSize int, maxAge time.Duration, minSamples int, minKSpread, minK, maxK float64) *Window {
	return &Window{
		observations: make([]Observation, 0, maxSize),
		maxSize:      maxSize,
		maxAge:       maxAge,
		minSamples:   minSamples,
		minKSpread:   minKSpread,
		minK:         minK,
		maxK:         maxK,
	}
}

// Add appends a (k, itl) observation if k is a non-NaN value in [minK, maxK]
// and itl > 0. When the window is at capacity, the oldest observation is
// evicted first. Returns true if the observation was dropped (NaN/out-of-range
// k or invalid itl) so the caller can log with a reconcile-scoped logger.
func (w *Window) Add(k, itl float64, ts time.Time) bool {
	if math.IsNaN(k) || k < w.minK || k > w.maxK {
		return true // dropped: NaN or out of range
	}
	if itl <= 0 || math.IsNaN(itl) {
		return true // dropped: invalid
	}
	if len(w.observations) >= w.maxSize {
		w.observations = w.observations[1:]
	}
	w.observations = append(w.observations, Observation{K: k, ITLSec: itl, Timestamp: ts})
	return false
}

// Prune removes observations older than maxAge relative to now.
func (w *Window) Prune(now time.Time) {
	cutoff := now.Add(-w.maxAge)
	i := 0
	for i < len(w.observations) && w.observations[i].Timestamp.Before(cutoff) {
		i++
	}
	w.observations = w.observations[i:]
}

// KSpread returns max_k − min_k over the current observations.
// Returns 0 when the window is empty.
func (w *Window) KSpread() float64 {
	if len(w.observations) == 0 {
		return 0
	}
	minK, maxK := w.observations[0].K, w.observations[0].K
	for _, o := range w.observations[1:] {
		if o.K < minK {
			minK = o.K
		}
		if o.K > maxK {
			maxK = o.K
		}
	}
	return maxK - minK
}

// Ready returns true when the window contains at least minSamples observations
// AND the k-spread is at least minKSpread. Both conditions must hold before the
// window is suitable for OLS fitting.
func (w *Window) Ready() bool {
	return len(w.observations) >= w.minSamples && w.KSpread() >= w.minKSpread
}

// Observations returns a copy of the current window contents.
// The caller may mutate the returned slice without affecting the window.
func (w *Window) Observations() []Observation {
	out := make([]Observation, len(w.observations))
	copy(out, w.observations)
	return out
}

// Len returns the number of observations currently in the window.
func (w *Window) Len() int {
	return len(w.observations)
}

// Clear discards all observations. Called when the workload shape changes.
func (w *Window) Clear() {
	w.observations = w.observations[:0]
}
