package balance

import (
	"sort"
	"sync"
	"time"
)

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
	mu     sync.Mutex
	models map[string]map[string]roleState
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

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{models: map[string]map[string]roleState{}}
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
func (t *Tracker) Observe(key string, dists map[string]Distribution, now time.Time) []Reading {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.models[key]
	next := make(map[string]roleState, len(dists))
	out := make([]Reading, 0, len(dists))
	for role, d := range dists {
		was := prev[role]
		s := was
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
	if len(next) == 0 {
		delete(t.models, key)
	} else {
		t.models[key] = next
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Role < out[j].Role })
	return out
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
