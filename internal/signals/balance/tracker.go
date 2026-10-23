package balance

import (
	"sort"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// StaleAfter is how long a model's collection must fail, on every cycle,
// before Tracker.Failed reports its figures stale. One failed cycle in a
// persistent imbalance must not drop the series the alert's for: timer reads,
// but a model whose collection stays broken must not keep a page standing on
// figures nothing measures any more.
const StaleAfter = 2 * time.Minute

// DefaultInterval is the optimize interval a Tracker assumes until
// SetInterval is called: the controller's default GLOBAL_OPT_INTERVAL.
const DefaultInterval = 15 * time.Second

// Reading is one role's distribution this cycle together with what the
// tracker remembers about it.
type Reading struct {
	Distribution
	// Sustained is true once the role has read imbalanced on every cycle for
	// SustainFor, and stays true until it has read balanced on every cycle
	// for ClearAfter.
	Sustained bool
	// Changed is true on the cycle Sustained flipped, either way: the one
	// cycle a warning (or its all-clear) is logged.
	Changed bool
}

// Tracker remembers, per model and role, since when the role has read
// imbalanced, and since when a role reported imbalanced has read balanced. It
// is safe for concurrent use.
type Tracker struct {
	mu       sync.Mutex
	interval time.Duration
	models   map[string]*modelState
}

type modelState struct {
	roles map[string]roleState
	// observedAt is the cycle Observe last read the model, zero before the
	// first.
	observedAt time.Time
	// seen is, per replica (replicaKey), the Ready time Measure last read on
	// it and the first cycle it read that Ready time.
	seen map[replicaKey]sighting
	// failedSince is the first cycle of the current run of failed
	// collections, zero outside one.
	failedSince time.Time
}

// replicaKey names one replica of a model as Measure grades it. A warm-pool
// Pod lent to a variant is a replica of that variant from the cycle it is lent,
// so the variant and the bridge flag are part of it.
type replicaKey struct {
	pod, variant string
	bridge       bool
}

type sighting struct {
	readySince time.Time
	firstSeen  time.Time
}

type roleState struct {
	// imbalancedSince is the first cycle of the current imbalanced streak,
	// zero outside one.
	imbalancedSince time.Time
	// balancedSince is, while sustained, the first cycle of the current
	// balanced streak, zero outside one.
	balancedSince time.Time
	sustained     bool
}

// NewTracker returns an empty Tracker that assumes DefaultInterval.
func NewTracker() *Tracker {
	return &Tracker{interval: DefaultInterval, models: map[string]*modelState{}}
}

// SetInterval sets the optimize interval the tracker's readings are taken at.
// Two readings further apart than maxGap are not consecutive. At or below
// zero it is ignored.
func (t *Tracker) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.interval = d
}

// maxGap is how far apart two readings may be and still be consecutive: one
// interval, with half an interval for a slow cycle.
func (t *Tracker) maxGap() time.Duration {
	return t.interval + t.interval/2
}

func (t *Tracker) model(key string) *modelState {
	m := t.models[key]
	if m == nil {
		m = &modelState{}
		t.models[key] = m
	}
	return m
}

// Measure is the package Measure for the model named by key, with each
// replica's IdleAfterReady grace run from the later of its ReadySince and the
// first cycle the tracker saw that ReadySince on it. A replica absent from
// rows is forgotten, so one that returns, a warm-pool Pod lent again among
// them, starts its grace again.
func (t *Tracker) Measure(key string, rows []domain.ReplicaMetrics, rolesByVariant map[string]string,
	queueThreshold float64, now time.Time) map[string]Distribution {
	t.mu.Lock()
	m := t.model(key)
	seen := make(map[replicaKey]sighting, len(rows))
	graceFrom := make(map[replicaKey]time.Time, len(rows))
	for _, rm := range rows {
		if rm.ReadySince.IsZero() {
			continue
		}
		k := replicaKey{pod: rm.PodName, variant: rm.VariantName, bridge: rm.FromWarmPool}
		s, ok := m.seen[k]
		if !ok || !s.readySince.Equal(rm.ReadySince) {
			s = sighting{readySince: rm.ReadySince, firstSeen: now}
		}
		seen[k] = s
		graceFrom[k] = rm.ReadySince
		if s.firstSeen.After(rm.ReadySince) {
			graceFrom[k] = s.firstSeen
		}
	}
	m.seen = seen
	t.mu.Unlock()
	return measure(rows, rolesByVariant, queueThreshold, now, func(rm domain.ReplicaMetrics) time.Time {
		return graceFrom[replicaKey{pod: rm.PodName, variant: rm.VariantName, bridge: rm.FromWarmPool}]
	})
}

// Observe folds the distributions of one cycle at now for the model named by
// key and returns a Reading per role, sorted by role. A role absent this
// cycle, one with no ready replica, is forgotten: when it returns it starts
// again from zero, so a parked role does not come back reporting the
// imbalance it had before it parked.
//
// Before a role is reported, one balanced cycle restarts its streak: an
// imbalance has to hold for SustainFor without a break. Once reported, it
// clears only after ClearAfter of balanced cycles, so a dip of one cycle in a
// persistent imbalance does not clear and re-raise it.
//
// A streak is of consecutive readings. When the model was last read more than
// one interval ago (SetInterval), with half an interval of slack, nothing
// was read in between and the streak, either way, restarts at this reading;
// a role already reported stays reported. Observe also ends a run of failed
// collections (Failed).
func (t *Tracker) Observe(key string, dists map[string]Distribution, now time.Time) []Reading {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.model(key)
	gap := !m.observedAt.IsZero() && now.Sub(m.observedAt) > t.maxGap()
	m.observedAt = now
	m.failedSince = time.Time{}
	next := make(map[string]roleState, len(dists))
	out := make([]Reading, 0, len(dists))
	for role, d := range dists {
		was := m.roles[role]
		s := was
		if gap {
			s.imbalancedSince, s.balancedSince = time.Time{}, time.Time{}
		}
		switch {
		case d.Imbalanced():
			s.balancedSince = time.Time{}
			if s.imbalancedSince.IsZero() {
				s.imbalancedSince = now
			}
			if now.Sub(s.imbalancedSince) >= SustainFor {
				s.sustained = true
			}
		case s.sustained:
			if s.balancedSince.IsZero() {
				s.balancedSince = now
			}
			if now.Sub(s.balancedSince) >= ClearAfter {
				s = roleState{}
			}
		default:
			s.imbalancedSince = time.Time{}
		}
		out = append(out, Reading{Distribution: d, Sustained: s.sustained, Changed: s.sustained != was.sustained})
		next[role] = s
	}
	m.roles = next
	sort.Slice(out, func(i, j int) bool { return out[i].Role < out[j].Role })
	return out
}

// Failed records that the model named by key could not be collected at now,
// and reports whether its collection has now failed on every cycle for
// StaleAfter. Until then the tracker keeps what it remembers, so a failed
// cycle neither restarts a streak nor clears a report; the next Observe ends
// the run. The caller forgets the model once Failed reports true.
func (t *Tracker) Failed(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.model(key)
	if m.failedSince.IsZero() {
		m.failedSince = now
	}
	return now.Sub(m.failedSince) >= StaleAfter
}

// Forget drops what the tracker remembers about the model named by key.
func (t *Tracker) Forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.models, key)
}

// Retain forgets every model whose key is not in active, so an empty set
// forgets them all.
func (t *Tracker) Retain(active map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.models {
		if !active[key] {
			delete(t.models, key)
		}
	}
}
