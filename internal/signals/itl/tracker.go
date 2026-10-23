package itl

import (
	"maps"
	"math"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The tiers a Tracker's line comes from.
const (
	// TierOLS is a full least-squares fit: the window spans enough of k.
	TierOLS = "ols"
	// TierPinnedB is a fit of A alone, with B pinned to the key's last full
	// fit or, before it has one, to DefaultBaselineSec.
	TierPinnedB = "pinned-B"
)

// Tracker keeps, per key, the rolling window of (k, ITL) readings a line is
// fitted from and the intercept of the key's last full fit. It is the one copy
// of that bookkeeping: which replica rows are readings, when the window is
// enough for a full fit, and what B the fallback pins. Every analyzer that
// fits ITL(k) uses it, so a fix to any of it reaches all of them.
//
// The caller names the key, and stamps and sweeps it on its own clock: what
// counts as "the variant is still there" belongs to the caller.
//
// Safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	windows   map[string]*Window
	baselines map[string]float64
	// steady holds, per key, only the readings taken while their replica's
	// load had held still, and is the only window the full fit -- and so the
	// remembered B -- is taken from. See DefaultSteadyFor.
	steady map[string]*Window
	traces map[string]*keyTraces
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		windows:   map[string]*Window{},
		baselines: map[string]float64{},
		steady:    map[string]*Window{},
		traces:    map[string]*keyTraces{},
	}
}

// Reading is what one Observe saw and fitted, for the caller to log and use.
type Reading struct {
	// Model is the fitted line; zero when there is none (Tier "").
	Model Model
	// Tier is TierOLS, TierPinnedB, or "" when no line could be fitted.
	Tier string
	// Declined is set when the window held enough samples but the pinned fit
	// refused them.
	Declined bool
	// BaselineStored reports whether the key holds an intercept from an
	// earlier full fit. BaselineLearned is that intercept being positive, so
	// the pinned B is this key's measurement rather than DefaultBaselineSec.
	BaselineStored, BaselineLearned bool

	// What the window was offered this cycle, by why a row was turned away.
	// Offered counts the readings the window kept; BelowBand those it dropped
	// for a k below DefaultMinObservableK (an idle replica).
	Considered, NotReady, NoITL, NoK, AboveBand, BelowBand, Offered int
	// Unsteady counts the Offered readings kept out of the full fit because
	// their replica's load had moved within DefaultSteadyFor.
	Unsteady int
	// Held is the window's size after pruning.
	Held int
	// SteadyHeld is how many steady readings the full fit may use, and Ready
	// is the confidence gate judged on those alone: a spread made of a
	// transient is not a spread. It can exceed Held: a steady reading
	// outlives the main window by up to DefaultSteadyCarry.
	SteadyHeld int
	Ready      bool
	// SteadySpread is max k - min k over the steady readings: the other half
	// of Ready, and how far it is from DefaultMinKSpread.
	SteadySpread float64
}

// Observe offers one cycle's replica rows of a variant to the key's window and
// fits ITL(k) = A*k + B over it.
//
// A row is a reading when it is the variant's, Ready and not lent by the warm
// pool, with a finite positive ITL and a k in (0, DefaultMaxObservableK]; the window
// itself drops a k below DefaultMinObservableK. A full fit is taken when the
// window is Ready, and its B is remembered when positive. Otherwise, with at
// least DefaultMinSamples readings, A alone is fitted with B pinned to the
// remembered intercept, or DefaultBaselineSec before there is one: a fleet the
// router balances spans little of k, so this is the normal case, not a
// degenerate one.
//
// The full fit is judged and taken on steady readings only: those whose
// replica's k had not moved by more than DefaultMaxKDrift within
// DefaultSteadyFor, from a replica that has been serving at least that long.
// In a balanced fleet the only spread in k a window ever sees is a transient
// -- a replica joining, or the load stepping -- and across a transient k and
// ITL describe different moments, so the line through them is not ITL(k).
// Its intercept was remembered and pinned into every fit after it: on one
// run, two identical models in two namespaces kept B = 4.5 ms and 14.8 ms
// from whichever transient each window happened to catch, and priced the
// same replica at 5.3 and 7.7 requests/s (issue #270). The steady readings
// outlive the window by up to DefaultSteadyCarry, so a step's old level is
// still there when the new one settles, and the two give the spread.
//
// The pinned fit takes the steady readings too while there are at least
// DefaultMinSamples of them, and every reading only below that, so a fleet
// that has not settled yet still gets a line. A load step is a transient the
// pinned fit would otherwise take whole: on the issue #270 rerun A jumped from
// 0.077 to 0.207 on the cycle the load stepped, with every new reading
// unsteady and 10 steady ones held, and the derived mu fell from 6.4 to 2.65
// requests/s for two and a half minutes. It does not take the carry: see
// pinnedReadings.
func (t *Tracker) Observe(key, variant string, rows []domain.ReplicaMetrics, now time.Time) Reading {
	var r Reading
	t.mu.Lock()
	w, ok := t.windows[key]
	if !ok {
		w = NewWindow(DefaultWindowMaxSize, DefaultObservationMaxAge, DefaultMinSamples,
			DefaultMinKSpread, DefaultMinObservableK, DefaultMaxObservableK)
		t.windows[key] = w
	}
	sw, ok := t.steady[key]
	if !ok {
		sw = NewWindow(DefaultSteadyWindowMaxSize, DefaultObservationMaxAge, DefaultMinSamples,
			DefaultMinKSpread, DefaultMinObservableK, DefaultMaxObservableK)
		t.steady[key] = sw
	}
	traces, ok := t.traces[key]
	if !ok {
		traces = &keyTraces{since: now, replicas: map[string]*replicaTrace{}}
		t.traces[key] = traces
	}
	seen := make(map[string]bool, len(rows))
	for _, rm := range rows {
		if rm.VariantName != variant {
			continue
		}
		r.Considered++
		// Judged before the switch and noted after it, so a row that is no
		// reading -- k below the band, no ITL yet -- still says where the
		// replica's load is.
		steady := false
		var trace *replicaTrace
		if rm.Ready && !rm.FromWarmPool {
			seen[rm.PodName] = true
			trace = traces.replicas[rm.PodName]
			if trace == nil {
				trace = &replicaTrace{since: now, joined: now.After(traces.since)}
				traces.replicas[rm.PodName] = trace
			}
			if traceable(rm.KvUsageInstant) {
				steady = trace.steady(rm.KvUsageInstant, now)
			} else {
				// Present, so its trace is kept; no k, so nothing is noted.
				trace = nil
			}
		}
		switch {
		case !rm.Ready || rm.FromWarmPool:
			r.NotReady++
		case !(rm.AvgITL > 0) || math.IsInf(rm.AvgITL, 1):
			// +Inf passes "> 0": a rate over a window with no tokens in it.
			r.NoITL++
		case !(rm.KvUsageInstant > 0):
			r.NoK++
		case rm.KvUsageInstant > DefaultMaxObservableK:
			r.AboveBand++
		default:
			// Add reports a reading it dropped: here, only a k below the band.
			if w.Add(rm.KvUsageInstant, rm.AvgITL, now) {
				r.BelowBand++
			} else {
				r.Offered++
				if steady {
					sw.Add(rm.KvUsageInstant, rm.AvgITL, now)
				} else {
					r.Unsteady++
				}
			}
		}
		if trace != nil {
			trace.note(rm.KvUsageInstant, now)
		}
	}
	// A replica missing from this cycle starts over when it returns: what it
	// served in between is unknown.
	for pod := range traces.replicas {
		if !seen[pod] {
			delete(traces.replicas, pod)
		}
	}
	w.Prune(now)
	obs := w.Observations()
	// A steady reading outlives the main window's oldest by at most
	// DefaultSteadyCarry: long enough for the old level to still be there
	// when a step's new level settles, and no longer, because A moves with
	// the workload and a steady window kept for its own sake would keep
	// fitting a line the fleet has since moved off.
	sw.Prune(now)
	cutoff := now.Add(-DefaultSteadyCarry)
	if len(obs) > 0 && obs[0].Timestamp.Before(cutoff) {
		cutoff = obs[0].Timestamp
	}
	sw.dropBefore(cutoff)
	steadyObs := sw.Observations()
	r.Held, r.Ready, r.SteadyHeld, r.SteadySpread = len(obs), sw.Ready(), len(steadyObs), sw.KSpread()
	baseline, stored := t.baselines[key]
	t.mu.Unlock()

	r.BaselineStored = stored
	r.BaselineLearned = stored && baseline > 0
	if !r.BaselineLearned {
		baseline = DefaultBaselineSec
	}
	// The fits run outside the lock: they read a copy of the window.
	if r.Ready {
		if m, ok := Fit(steadyObs); ok {
			// B, not A, is remembered: B is the hardware floor, A the
			// contention that moves with the workload. And only a physical,
			// positive floor -- a negative B pinned into the next fit would
			// be worse than the constant it replaced.
			if m.B > 0 {
				t.mu.Lock()
				t.baselines[key] = m.B
				t.mu.Unlock()
			}
			r.Model, r.Tier = m, TierOLS
			return r
		}
	}
	// The sample floor still applies when the spread is waived: the pinned
	// fit answers from a single pair, and a line should not rest on one
	// reading from one replica on one cycle.
	pinned := pinnedReadings(obs, steadyObs)
	if len(pinned) < DefaultMinSamples {
		return r
	}
	m, ok := FitPinnedB(pinned, baseline)
	if !ok {
		r.Declined = true
		return r
	}
	r.Model, r.Tier = m, TierPinnedB
	return r
}

// pinnedReadings is what the pinned fit reads: the steady readings the main
// window spans, never more than it holds, reaching back into the carry only
// as far as DefaultMinSamples needs, and every reading of the main window when
// fewer than DefaultMinSamples are steady at all. Both windows are in time
// order.
//
// The carry is the full fit's: it keeps a step's old level until the new one
// settles, which B needs and A does not. A moves with the workload, and
// fitted on the carried window it followed a heavier shape at the same fill
// twice as slowly -- two replicas take 9 cycles to price the new A, against 5
// on the main window alone. Under least squares it was 17 against 8, and a
// minute in priced 0.090 for a true 0.12, so the derived mu read about 30%
// high. Through a load step the main
// window's steady readings run short, and the newest DefaultMinSamples, the
// old level's, hold A until the new level settles.
func pinnedReadings(obs, steady []Observation) []Observation {
	if len(steady) < DefaultMinSamples {
		return obs
	}
	start := len(steady)
	for start > 0 && len(obs) > 0 && len(steady)-start < len(obs) &&
		!steady[start-1].Timestamp.Before(obs[0].Timestamp) {
		start--
	}
	return steady[min(start, len(steady)-DefaultMinSamples):]
}

// Sweep prunes every window and forgets the window and intercept of each key
// stale reports true for. It returns how many of each it forgot.
//
// stale runs under the tracker's lock: it must be cheap and must not call back
// into the tracker. What it reads of the caller's state is the caller's to
// guard -- both analyzers call Sweep holding their own lock.
func (t *Tracker) Sweep(now time.Time, stale func(key string) bool) (windows, baselines int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, w := range t.windows {
		w.Prune(now)
	}
	for _, w := range t.steady {
		w.Prune(now)
	}
	for key := range t.windows {
		if stale(key) {
			delete(t.windows, key)
			windows++
		}
	}
	// The steady window and the traces are part of the window: counted with
	// it, not apart.
	for key := range t.steady {
		if stale(key) {
			delete(t.steady, key)
		}
	}
	for key := range t.traces {
		if stale(key) {
			delete(t.traces, key)
		}
	}
	// Apart from the windows: a key can hold an intercept with no window, one
	// restored from a snapshot before the variant was seen again.
	for key := range t.baselines {
		if stale(key) {
			delete(t.baselines, key)
			baselines++
		}
	}
	return windows, baselines
}

// Baselines returns a copy of every key's remembered intercept.
func (t *Tracker) Baselines() map[string]float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return maps.Clone(t.baselines)
}

// Baseline returns the key's remembered intercept.
func (t *Tracker) Baseline(key string) (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.baselines[key]
	return b, ok
}

// RestoreBaseline sets the key's intercept unless it already holds one: what
// was learned since the process started wins over what a snapshot carried.
func (t *Tracker) RestoreBaseline(key string, b float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, have := t.baselines[key]; !have {
		t.baselines[key] = b
	}
}

// SetBaseline sets the key's intercept.
func (t *Tracker) SetBaseline(key string, b float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.baselines[key] = b
}

// WindowLen is how many readings the window filed under key holds, 0 when there
// is none.
func (t *Tracker) WindowLen(key string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if w, ok := t.windows[key]; ok {
		return w.Len()
	}
	return 0
}

// WindowLens returns, for every key holding a window, how many readings it
// holds.
func (t *Tracker) WindowLens() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.windows))
	for key, w := range t.windows {
		out[key] = w.Len()
	}
	return out
}

// ForgetBaselines drops every remembered intercept and keeps the windows: the
// readings are this process's own, the intercepts may have been restored.
func (t *Tracker) ForgetBaselines() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.baselines)
}
