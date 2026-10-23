package itl

import "time"

// The line's token rate, corrected by the rate the replica reports.
//
// TokenRate prices a replica from two readings that are not the same
// measurement: Sequences comes from the KV fill, ITL(k) from the
// inter-token-latency histogram. On a real-GPU run the two disagreed by a
// steady 13-17%, always the same way (issue #270, Qwen3-32B-FP8 on one H200,
// vLLM v0.26.0, 1000 in / 500 out, 192 concurrent). The replica ran 190-192
// requests and generated 2,900-3,230 tokens/s, so a sequence advanced every
// 0.059-0.066 s (running / GPS). The line fitted to the histogram read
// 0.068-0.078 s at the same fill -- 1.13x on average over 17 one-minute
// intervals in two namespaces -- while k x C / KVreq matched the running
// count to within 1% on average. The sequence count was right and the latency
// was not, so the line under-priced every replica by the same factor: derived
// mu 5.33 and 5.41 req/s against 5.99 and 6.05 measured.
//
// The ratio of the reported rate to the predicted one, taken where the
// replica is settled and folded into a per-key factor, removes it: 1.129 and
// 1.125 on those intervals, and derived mu 6.02 and 6.09. The factor is a
// property of the readings rather than of a particular fit -- every line
// fitted to the same histogram carries the same bias -- so it outlives a refit
// and is applied to whatever line prices the key.

const (
	// DefaultMinGPSCalibration and DefaultMaxGPSCalibration bound the ratio
	// one cycle may contribute. Outside them the line does not describe the
	// replica at all, which is itl-gps-mismatch's to report, not a bias to
	// correct: a factor of three would hide the fault behind a number.
	DefaultMinGPSCalibration = 0.5
	DefaultMaxGPSCalibration = 2.0

	// DefaultGPSCalibrationWeight is the weight a cycle's ratio gets against
	// the factor already held. A quarter is a time constant of about four
	// cycles: one-minute rates move 5-10% between scrapes on a steady
	// replica, and the factor should not.
	DefaultGPSCalibrationWeight = 0.25
)

// CalibrationRatio returns observedGPS / predictedGPS, the factor that makes the
// line's token rate at k the one the replica reports there.
//
// ok is false when the pair is no evidence: no observed or predicted rate, a k
// below DefaultGPSMinKForVerification (a handful of resident sequences), or a
// ratio outside [DefaultMinGPSCalibration, DefaultMaxGPSCalibration].
func CalibrationRatio(observedGPS, predictedGPS, k float64) (float64, bool) {
	if !(observedGPS > 0) || !(predictedGPS > 0) || k < DefaultGPSMinKForVerification {
		return 0, false
	}
	r := observedGPS / predictedGPS
	if !(r >= DefaultMinGPSCalibration && r <= DefaultMaxGPSCalibration) {
		return 0, false
	}
	return r, true
}

// NoteGPSRatio folds one cycle's ratio into the key's calibration factor and
// returns the factor. The first ratio a key sees is taken as it is.
func (t *Tracker) NoteGPSRatio(key string, ratio float64) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.calibration[key]
	if !ok {
		c = ratio
	} else {
		c += DefaultGPSCalibrationWeight * (ratio - c)
	}
	t.calibration[key] = c
	return c
}

// GPSCalibration returns the key's calibration factor; ok is false when the key
// has none, and the caller then prices from the line as it stands (factor 1).
func (t *Tracker) GPSCalibration(key string) (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.calibration[key]
	return c, ok
}

// Settled reports whether the replica pod, as of the key's last Observe at now,
// has been seen under the key for at least DefaultSteadyFor without a gap and
// its k has stayed within DefaultMaxKDrift of its latest reading throughout.
//
// Stricter than the steadiness the full fit asks for, which takes a replica
// present at the key's first Observe as steady on no history. A calibration
// compares the replica's one-minute token rate with its instantaneous k, and on
// a ramp the rate trails the fill by up to a minute -- predicted above
// observed on every replica at once, which is how run T's gate fired 28 times
// in the first two minutes. Here the history has to exist.
func (t *Tracker) Settled(key, pod string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	traces, ok := t.traces[key]
	if !ok {
		return false
	}
	rt, ok := traces.replicas[pod]
	if !ok || len(rt.samples) == 0 || now.Sub(rt.since) < DefaultSteadyFor {
		return false
	}
	last := rt.samples[len(rt.samples)-1]
	return rt.steady(last.k, last.at)
}
