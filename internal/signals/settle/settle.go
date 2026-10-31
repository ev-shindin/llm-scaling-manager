// Package settle is the one record of when each replica of a model last
// started to settle: it joined, came back after an absence, restarted, was
// lent by the warm pool, or rolled out. The engine writes it once per cycle,
// before anything reads the cycle's rows; every reader goes through View, and
// none writes (docs/proposals/analyzer-learner.md, section 4).
//
// The tracker exposes event times and each row's evidence. It decides no
// wait: each reader applies its own window, keyed by its own policy
// (loadImbalance.idleAfterReady for the load-balance diagnostic, the
// steadyGate keys for the ITL line, and so on).
//
// It imports only the standard library (analyzer-learner.md, 6.3); its
// subpackage settlerow reads the collector's rows as the tracker's.
package settle

import (
	"maps"
	"slices"
	"sync"
	"time"
)

// Period is the rule period. A replica missing from a cycle's rows keeps its
// sighting for up to Period: a single missing row is a missed scrape, not a
// replica that left and came back. Two idle rows further apart than Period
// do not continue an idle run. Back compares against a row one Period back,
// not one cycle back: the gauges are one-minute peaks and rates, and two
// consecutive cycles' readings share 45 s of their window.
const Period = time.Minute

// TraceHorizon is how long a replica's rows are kept, and how long a
// replica absent from every cycle is remembered before it is forgotten.
const TraceHorizon = 5 * time.Minute

const (
	// maxTrace bounds a replica's trace whatever the cycle interval.
	maxTrace = 64
	// maxReturns bounds the load returns kept per replica.
	maxReturns = 8
)

// Key names one replica of a model. A warm-pool Pod lent to a variant serves
// that variant from the cycle it is lent, so the variant and the bridge flag
// are part of it: a Pod that changes hands is a new replica.
type Key struct {
	Pod     string
	Variant string
	Bridge  bool
}

// ITL is which of three readings a row's inter-token latency is.
type ITL int

const (
	// ITLAbsent is no usable reading: the series was absent, the query
	// failed, or the Pod was not Ready.
	ITLAbsent ITL = iota
	// ITLEmpty is a series present with an empty window: a replica that
	// generated no token in it.
	ITLEmpty
	// ITLFinite is a measured, finite, positive mean.
	ITLFinite
)

// Row is what the tracker reads of one replica on one cycle. The ...Known
// fields say whether the gauge beside them was read: an unknown gauge is
// neither evidence of idleness nor of load.
type Row struct {
	Key
	// Prefill is set on a prefill-role replica, whose idleness reads the
	// request rate: a prefill Pod's ITL window is empty however busy it is.
	Prefill bool
	// Ready is the Pod's readiness. A row that is not Ready is a sighting,
	// and is no evidence of idleness or load: nothing is routed to it.
	Ready bool
	// ReadySince is when the Pod last became Ready, zero when unknown.
	ReadySince time.Time
	// LentSince is when the warm pool lent the Pod to this variant, zero
	// when unknown or not a bridge.
	LentSince time.Time
	// Revision is the Pod's template revision, "" when unknown.
	Revision string

	// KV is the replica's peak KV cache use over the collector's minute.
	KV      float64
	KVKnown bool
	ITL     ITL

	GenerationRate      float64
	GenerationRateKnown bool
	RequestRate         float64
	RequestRateKnown    bool
}

// Activity is what one row says about a replica's load.
type Activity int

const (
	// Unknown is no evidence either way: a gauge the rule needs was not
	// read, the Pod is not Ready, or the row reads busy without saying it
	// served (KV in use, no ITL and no generation rate). It neither counts
	// toward an idle run nor breaks one.
	Unknown Activity = iota
	// Idle is positive evidence of no work.
	Idle
	// Load is positive evidence the replica served.
	Load
)

// String names the activity.
func (a Activity) String() string {
	switch a {
	case Idle:
		return "idle"
	case Load:
		return "load"
	default:
		return "unknown"
	}
}

// Activity reads the row by its role.
//
// Decode and aggregated: idle when the peak KV is known and 0 and either the
// ITL window is known empty or the generation rate is known and 0; load on a
// finite ITL or a known generation rate above 0.
//
// Prefill: idle when the peak KV is known and 0 and the request rate is
// known and 0; load on a known request rate above 0. A prefill Pod's ITL is
// empty on every row (it emits one token per request), and the prompt-token
// rate is absent on SGLang, so neither is read.
func (r Row) Activity() Activity {
	if !r.Ready {
		return Unknown
	}
	kvEmpty := r.KVKnown && !(r.KV > 0)
	if r.Prefill {
		switch {
		case r.RequestRateKnown && r.RequestRate > 0:
			return Load
		case kvEmpty && r.RequestRateKnown:
			return Idle
		}
		return Unknown
	}
	switch {
	case r.ITL == ITLFinite || r.GenerationRateKnown && r.GenerationRate > 0:
		return Load
	case kvEmpty && (r.ITL == ITLEmpty || r.GenerationRateKnown):
		return Idle
	}
	return Unknown
}

// Reason is the event that started a replica's current settle.
type Reason string

const (
	// Joined is a replica seen for the first time.
	Joined Reason = "joined"
	// Rejoined is a replica back after more than Period with no row.
	Rejoined Reason = "rejoined"
	// Restarted is a new Ready time under a key already seen: an engine
	// restart under the same Pod name.
	Restarted Reason = "restarted"
	// Lent is a warm-pool lend: a bridge seen for the first time, or a new
	// lend time under a bridge already seen (a Pod handed back and lent
	// again to the same variant).
	Lent Reason = "lent"
	// RolledOut is a new template revision under a key already seen.
	RolledOut Reason = "rolled-out"
)

// Reasons is every reason, in a fixed order.
func Reasons() []Reason {
	return []Reason{Joined, Rejoined, Restarted, Lent, RolledOut}
}

// Return is one return of load after an idle run.
type Return struct {
	// At is the first row of load after the run.
	At time.Time
	// IdleRows is how many idle rows the run held, and IdleFrom its first.
	IdleRows int
	IdleFrom time.Time
}

// Sample is one row of a replica's trace, with when it was read and what it
// says.
type Sample struct {
	At       time.Time
	Activity Activity
	Row      Row
}

// Replica is what the tracker knows of one replica.
type Replica struct {
	Key
	// Since is when the current settle began, and Reason the event that
	// began it.
	Since  time.Time
	Reason Reason
	// FirstSeen and LastSeen bound the current sighting.
	FirstSeen time.Time
	LastSeen  time.Time
	// ReadySince is the Pod's Ready time last read, zero when none has
	// been, and ReadySeen the first cycle it was read.
	ReadySince time.Time
	ReadySeen  time.Time
	// LentSince is the warm pool's lend time last read, zero when none has
	// been, and LentSeen the first cycle the current lend was seen: the
	// first sight of the bridge, or of a new lend time under it.
	LentSince time.Time
	LentSeen  time.Time
	// Revision is the template revision last read.
	Revision string
	// IdleRows is the length of the current idle run, IdleFrom its first
	// row and IdleLast its last; zero outside one.
	IdleRows int
	IdleFrom time.Time
	IdleLast time.Time
	// Returns is the recent returns of load after an idle run, oldest
	// first.
	Returns []Return
}

// ReadyFrom is the later of the Pod's Ready time and the first cycle that
// Ready time was read, zero when no Ready time has been. A node clock behind
// the controller's dates the Ready time into the controller's past, and a
// warm-pool lend keeps the Ready time the Pod had in the pool; the first
// sight covers both.
func (r Replica) ReadyFrom() time.Time {
	if r.ReadySince.IsZero() {
		return time.Time{}
	}
	if r.ReadySeen.After(r.ReadySince) {
		return r.ReadySeen
	}
	return r.ReadySince
}

// Idle reports whether the replica's current idle run holds at least
// idleRows rows and is still running at now: its last idle row is at most
// Period old.
func (r Replica) Idle(idleRows int, now time.Time) bool {
	return r.IdleRows > 0 && r.IdleRows >= idleRows && now.Sub(r.IdleLast) <= Period
}

// ReturnedAfter is the latest return of load after an idle run of at least
// idleRows rows, and whether there is one since the current settle began.
func (r Replica) ReturnedAfter(idleRows int) (time.Time, bool) {
	for i := len(r.Returns) - 1; i >= 0; i-- {
		if r.Returns[i].IdleRows >= idleRows {
			return r.Returns[i].At, true
		}
	}
	return time.Time{}, false
}

// View reads the tracker. It never writes.
type View interface {
	// Replica is what the tracker knows of the replica key of model, and
	// whether it knows it at all.
	Replica(model string, key Key) (Replica, bool)
	// Trace is the replica's rows over the last TraceHorizon, oldest first.
	Trace(model string, key Key) []Sample
	// Back is the replica's newest row at least d before now, and whether
	// there is one.
	Back(model string, key Key, d time.Duration, now time.Time) (Sample, bool)
}

// replica is one replica's state and trace.
type replica struct {
	Replica
	trace []Sample
}

// Tracker is the settle record of every model. The engine writes it, once
// per model per cycle (Observe); readers use it as a View. It is safe for
// concurrent use.
type Tracker struct {
	mu       sync.RWMutex
	models   map[string]map[Key]*replica
	verdicts map[Reason]int64
}

var _ View = (*Tracker)(nil)

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{models: map[string]map[Key]*replica{}, verdicts: map[Reason]int64{}}
}

// Observe records one cycle of model's rows, read at now.
//
// A row is a sighting whatever its gauges: a row whose every gauge is
// unknown (a failed scrape, a Prometheus brownout) keeps the replica's
// settle, and a run of them, however long, does not restart it while the
// Pod stays listed. A replica missing from rows keeps its sighting for up
// to Period; back after longer it settles again (Rejoined). One absent
// TraceHorizon is forgotten.
func (t *Tracker) Observe(model string, rows []Row, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.models[model]
	if m == nil {
		m = map[Key]*replica{}
		t.models[model] = m
	}
	for _, row := range rows {
		r, ok := m[row.Key]
		switch {
		case !ok:
			r = &replica{}
			m[row.Key] = r
			t.start(r, row, firstReason(row, Joined), now)
		case now.Sub(r.LastSeen) > Period:
			*r = replica{}
			t.start(r, row, firstReason(row, Rejoined), now)
		default:
			t.note(r, row, now)
		}
		r.LastSeen = now
		t.activity(r, row, now)
	}
	maps.DeleteFunc(m, func(_ Key, r *replica) bool { return now.Sub(r.LastSeen) > TraceHorizon })
	if len(m) == 0 {
		delete(t.models, model)
	}
}

// firstReason is why a replica first seen (or seen again) starts to settle:
// a bridge is a lend.
func firstReason(row Row, otherwise Reason) Reason {
	if row.Bridge {
		return Lent
	}
	return otherwise
}

// start begins a replica's sighting at now.
func (t *Tracker) start(r *replica, row Row, reason Reason, now time.Time) {
	r.Key = row.Key
	r.FirstSeen = now
	r.ReadySince, r.LentSince, r.Revision = row.ReadySince, row.LentSince, row.Revision
	if !row.ReadySince.IsZero() {
		r.ReadySeen = now
	}
	if row.Bridge {
		r.LentSeen = now
	}
	t.settle(r, reason, now)
}

// note reads a row of a replica already seen: a new Ready time, lend time or
// revision starts a settle. A zero or empty field is unknown and changes
// nothing.
func (t *Tracker) note(r *replica, row Row, now time.Time) {
	var reason Reason
	if !row.ReadySince.IsZero() && !row.ReadySince.Equal(r.ReadySince) {
		if !r.ReadySince.IsZero() {
			reason = Restarted
		}
		r.ReadySince, r.ReadySeen = row.ReadySince, now
	}
	if row.Revision != "" && row.Revision != r.Revision {
		if r.Revision != "" {
			reason = RolledOut
		}
		r.Revision = row.Revision
	}
	if !row.LentSince.IsZero() && !row.LentSince.Equal(r.LentSince) {
		if !r.LentSince.IsZero() {
			reason = Lent
		}
		r.LentSince, r.LentSeen = row.LentSince, now
	}
	if reason != "" {
		t.settle(r, reason, now)
	}
}

// settle starts a settle at now: the idle run and the returns before it no
// longer say anything about the replica.
func (t *Tracker) settle(r *replica, reason Reason, now time.Time) {
	r.Since, r.Reason = now, reason
	r.IdleRows, r.IdleFrom, r.IdleLast = 0, time.Time{}, time.Time{}
	r.Returns = nil
	t.verdicts[reason]++
}

// activity folds the row's evidence into the idle run and the trace.
//
// An idle row continues the run when the run's last idle row is at most
// Period old, and begins a new one otherwise. A load row after a run that is
// still running (its last idle row at most Period old) is a return; one
// after a longer gap is not: what the replica did in between was not read.
// An unknown row changes neither.
func (t *Tracker) activity(r *replica, row Row, now time.Time) {
	a := row.Activity()
	running := r.IdleRows > 0 && now.Sub(r.IdleLast) <= Period
	switch a {
	case Idle:
		if !running {
			r.IdleRows, r.IdleFrom = 0, now
		}
		r.IdleRows++
		r.IdleLast = now
	case Load:
		if running {
			r.Returns = append(r.Returns, Return{At: now, IdleRows: r.IdleRows, IdleFrom: r.IdleFrom})
			if len(r.Returns) > maxReturns {
				r.Returns = r.Returns[len(r.Returns)-maxReturns:]
			}
		}
		r.IdleRows, r.IdleFrom, r.IdleLast = 0, time.Time{}, time.Time{}
	}
	r.trace = append(r.trace, Sample{At: now, Activity: a, Row: row})
	cut := 0
	for cut < len(r.trace) && (now.Sub(r.trace[cut].At) > TraceHorizon || len(r.trace)-cut > maxTrace) {
		cut++
	}
	r.trace = r.trace[cut:]
}

// Replica implements View.
func (t *Tracker) Replica(model string, key Key) (Replica, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.models[model][key]
	if !ok {
		return Replica{}, false
	}
	out := r.Replica
	out.Returns = slices.Clone(r.Returns)
	return out, true
}

// Trace implements View.
func (t *Tracker) Trace(model string, key Key) []Sample {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if r, ok := t.models[model][key]; ok {
		return slices.Clone(r.trace)
	}
	return nil
}

// Back implements View.
func (t *Tracker) Back(model string, key Key, d time.Duration, now time.Time) (Sample, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.models[model][key]
	if !ok {
		return Sample{}, false
	}
	for i := len(r.trace) - 1; i >= 0; i-- {
		if now.Sub(r.trace[i].At) >= d {
			return r.trace[i], true
		}
	}
	return Sample{}, false
}

// Verdicts is how many settles each reason has started since the tracker
// was made.
func (t *Tracker) Verdicts() map[Reason]int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[Reason]int64, len(Reasons()))
	for _, r := range Reasons() {
		out[r] = t.verdicts[r]
	}
	return out
}

// Forget drops what the tracker knows of model.
func (t *Tracker) Forget(model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.models, model)
}

// Retain forgets every model not in active; an empty set forgets them all.
func (t *Tracker) Retain(active map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	maps.DeleteFunc(t.models, func(model string, _ map[Key]*replica) bool { return !active[model] })
}
