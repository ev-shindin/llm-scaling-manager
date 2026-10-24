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
	// slopes holds, per key, the A of its last line, which the pinned fit
	// holds while too few readings are past their settle to fit a new one,
	// or the window too few readings at all; with when it was fitted and
	// under which engine arguments, which bound the hold (Admission).
	slopes map[string]heldSlope
}

// heldSlope is a key's last fitted A, when it was fitted, and the engine
// arguments (Admission.Args) its readings were taken under.
type heldSlope struct {
	a        float64
	fittedAt time.Time
	args     string
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		windows:   map[string]*Window{},
		baselines: map[string]float64{},
		steady:    map[string]*Window{},
		traces:    map[string]*keyTraces{},
		slopes:    map[string]heldSlope{},
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
	// for a k below the band's lower edge (an idle replica).
	Considered, NotReady, NoITL, NoK, AboveBand, BelowBand, Offered int
	// Capped counts the Offered readings above the band's upper edge, taken
	// because their replica was at its sequence cap (Admission.atSequenceCap).
	Capped int
	// Unsteady counts the Offered readings kept out of the full fit because
	// their replica's load had moved within DefaultSteadyFor.
	Unsteady int
	// Settling counts the Unsteady readings whose replica was inside its
	// settle -- it joined, or the load came back to it, within
	// DefaultSteadyFor -- and which the pinned fit leaves out too.
	Settling int
	// SlopeHeld is set when the pinned fit had too few readings past their
	// settle to fit, or the window too few readings at all, and Model's A is
	// the key's last one.
	SlopeHeld bool
	// SlopeFittedAt is when the held A was fitted; zero unless SlopeHeld.
	SlopeFittedAt time.Time
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

// SlopeAge is how long ago the held A was fitted, zero when none is held:
// what tells a held line from a fit made this cycle in a log.
func (r Reading) SlopeAge(now time.Time) time.Duration {
	if !r.SlopeHeld || r.SlopeFittedAt.IsZero() {
		return 0
	}
	return now.Sub(r.SlopeFittedAt)
}

// Observe offers one cycle's replica rows of a variant to the key's window and
// fits ITL(k) = A*k + B over it.
//
// A row is a reading when it is the variant's, Ready and not lent by the warm
// pool, with a finite positive ITL and a k inside adm's band (by default
// [DefaultMinObservableK, DefaultMaxObservableK]). A k above the band is a
// reading too when the replica is at its sequence cap (Admission.atSequenceCap);
// the zero Admission knows no cap and admits nothing above it. The band is
// judged per call, so a key's window may hold readings from a band the policy
// has since narrowed; they age out with the window. A full fit is taken when the
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
// pinnedReadings. Taking every reading, it still leaves out those of a replica
// inside its settle -- a joiner's, or one the load came back to -- whose ITL
// covers its warm-up, and while fewer than DefaultMinSettledSamples remain it
// holds the key's last A. It holds the last A too once the window has fewer
// than DefaultMinSamples readings left -- a fleet below the band ages its
// readings out and offers none -- so a key that has fitted keeps a line through
// light load, or above the band (SlopeHeld says so, SlopeFittedAt when the A
// was fitted). Both holds end once the A is older than adm.SlopeHoldFor or was
// fitted under other engine arguments than adm.Args.
func (t *Tracker) Observe(key, variant string, rows []domain.ReplicaMetrics, adm Admission, now time.Time) Reading {
	var r Reading
	lo, hi := adm.band()
	s := adm.Steady.withDefaults()
	t.mu.Lock()
	w, ok := t.windows[key]
	if !ok {
		// The windows span the whole cache: the band is the Admission's,
		// applied below, where a capped replica's reading may pass it.
		w = NewWindow(DefaultWindowMaxSize, DefaultObservationMaxAge, DefaultMinSamples,
			DefaultMinKSpread, 0, 1)
		t.windows[key] = w
	}
	sw, ok := t.steady[key]
	if !ok {
		sw = NewWindow(DefaultSteadyWindowMaxSize, DefaultObservationMaxAge, DefaultMinSamples,
			DefaultMinKSpread, 0, 1)
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
		steady, settling := false, false
		var trace *replicaTrace
		if rm.Ready && !rm.FromWarmPool {
			seen[rm.PodName] = true
			trace = traces.replicas[rm.PodName]
			if trace == nil {
				trace = &replicaTrace{since: now, joined: now.After(traces.since)}
				traces.replicas[rm.PodName] = trace
			}
			trace.serving(rm.AvgITL > 0 && !math.IsInf(rm.AvgITL, 1), traceable(rm.KvUsageInstant), now, s)
			settling = trace.settling(now, s)
			if traceable(rm.KvUsageInstant) {
				steady = trace.steady(rm.KvUsageInstant, now, s)
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
		case rm.KvUsageInstant < lo:
			r.BelowBand++
		case rm.KvUsageInstant > hi && !adm.atSequenceCap(rm):
			r.AboveBand++
		default:
			// Add drops nothing the cases above let through but a k above a
			// full cache, which the collector never reports; counted as
			// above the band all the same.
			if w.add(Observation{K: rm.KvUsageInstant, ITLSec: rm.AvgITL, Timestamp: now, settling: settling}) {
				r.AboveBand++
			} else {
				r.Offered++
				if rm.KvUsageInstant > hi {
					r.Capped++
				}
				switch {
				case steady:
					sw.Add(rm.KvUsageInstant, rm.AvgITL, now)
				case settling:
					r.Unsteady++
					r.Settling++
				default:
					r.Unsteady++
				}
			}
		}
		if trace != nil {
			trace.note(rm.KvUsageInstant, now, s)
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
	cutoff := now.Add(-s.Carry)
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
			// B is what is remembered as the key's: B is the hardware floor,
			// A the contention that moves with the workload, kept only for
			// the pinned fit to hold through a settle. And only a physical,
			// positive floor -- a negative B pinned into the next fit would
			// be worse than the constant it replaced.
			t.mu.Lock()
			if m.B > 0 {
				t.baselines[key] = m.B
			}
			t.slopes[key] = heldSlope{a: m.A, fittedAt: now, args: adm.Args}
			t.mu.Unlock()
			r.Model, r.Tier = m, TierOLS
			return r
		}
	}
	// The sample floor still applies when the spread is waived: the pinned
	// fit answers from a single pair, and a line should not rest on one
	// reading from one replica on one cycle.
	pinned, settled := pinnedReadings(obs, steadyObs)
	if !settled || len(pinned) < s.MinSettledSamples {
		// Too few readings to fit, for one of two reasons, and the last A
		// holds through both.
		//
		// The window is full of replicas still settling: a scale-up's
		// joiners, or a fleet the load just came back to. Their readings
		// are the transient, and what remains is too little to price the
		// fleet, so the last A holds until they settle.
		//
		// Or the window has run short: its readings aged out past
		// DefaultObservationMaxAge and nothing replaced them. That is what
		// light load does -- a replica below the band's lower edge offers no
		// reading at all -- so a fleet that learned its line at load and
		// then sat below the band for half an hour lost the line, the
		// derived mu with it, and was sized by KV occupancy alone, which at
		// light load reads far below the throughput share it serves
		// (issue #284). The key's last A is the newest evidence of its line
		// there is -- the mu derived from it is still priced at the shape
		// arriving now -- and a fresh fit replaces it as soon as the window
		// holds DefaultMinSamples again. A key that never fitted has no A
		// and still gets no line. The same holds above the band: a replica
		// there that is not at its sequence cap offers no reading either.
		//
		// Not for ever. An A fitted longer ago than adm.SlopeHoldFor, or
		// under other engine arguments than adm.Args, is not held: the hold
		// ends where a finished fit is no longer trusted elsewhere, and a
		// different engine is not the one the slope was measured on.
		t.mu.Lock()
		held, have := t.slopes[key]
		t.mu.Unlock()
		if !have || !adm.mayHold(held, now) {
			return r
		}
		r.Model, r.Tier, r.SlopeHeld = Model{A: held.a, B: baseline}, TierPinnedB, true
		r.SlopeFittedAt = held.fittedAt
		return r
	}
	m, ok := FitPinnedB(pinned, baseline)
	if !ok {
		r.Declined = true
		return r
	}
	t.mu.Lock()
	t.slopes[key] = heldSlope{a: m.A, fittedAt: now, args: adm.Args}
	t.mu.Unlock()
	r.Model, r.Tier = m, TierPinnedB
	return r
}

// DefaultMinSettledSamples is the fewest readings, from replicas past their
// settle, the pinned fit is taken from on the every-reading path; below it the
// key's last A holds. The window's DefaultMinSamples still has to be met
// first: this floor is for what is left once the settling replicas' readings
// are set aside, and three is one cycle of a three-replica fleet or three of a
// lone replica -- not one reading of one replica on one cycle.
// It is the default of analyzerTuning.steadyGate.minSettledSamples.
const DefaultMinSettledSamples = 3

// pinnedReadings is what the pinned fit reads: the steady readings the main
// window spans, never more than it holds, reaching back into the carry only
// as far as DefaultMinSamples needs, and every reading of the main window when
// fewer than DefaultMinSamples are steady at all. Both windows are in time
// order.
//
// The carry is the full fit's: it keeps a step's old level until the new one
// settles, which B needs and A does not. A moves with the workload, and
// fitted on the carried window it followed a heavier shape at the same fill
// twice as slowly -- two replicas took 17 cycles to price the new A, against 8
// on the main window alone, and a minute in priced 0.090 for a true 0.12, so
// the derived mu read about 30% high. Through a load step the main
// window's steady readings run short, and the newest DefaultMinSamples, the
// old level's, hold A until the new level settles.
//
// Fewer than DefaultMinSamples are steady on a saturated fleet nearly always:
// k swings between above the band and the dips into it by more than
// DefaultMaxKDrift, and on a two-namespace run the steady readings numbered 0
// on almost every cycle. Taking every reading there took each settling
// replica's warm-up too, so those are set aside by the replica's state when
// the reading was taken, not by its value: see FitPinnedB. ok is false when
// neither window holds DefaultMinSamples, the floor the fit has always had.
func pinnedReadings(obs, steady []Observation) (pinned []Observation, ok bool) {
	if len(steady) < DefaultMinSamples {
		if len(obs) < DefaultMinSamples {
			return nil, false
		}
		pinned = make([]Observation, 0, len(obs))
		for _, o := range obs {
			if !o.settling {
				pinned = append(pinned, o)
			}
		}
		return pinned, true
	}
	start := len(steady)
	for start > 0 && len(obs) > 0 && len(steady)-start < len(obs) &&
		!steady[start-1].Timestamp.Before(obs[0].Timestamp) {
		start--
	}
	return steady[min(start, len(steady)-DefaultMinSamples):], true
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
	// The steady window, the traces and the held slope are part of the
	// window: counted with it, not apart.
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
	for key := range t.slopes {
		if stale(key) {
			delete(t.slopes, key)
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
