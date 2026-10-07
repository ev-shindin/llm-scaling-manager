package decision

import (
	"maps"
	"sync"
	"time"
)

// SharePromisedMaxAge is how old a promised-GPU snapshot may be and still be
// withheld. A promise lasts at most a fill timeout, a few minutes; a snapshot
// older than this belongs to an optimizer that has stopped publishing, and
// withholding it would keep GPUs from wakes and the warm pool for nothing.
const SharePromisedMaxAge = 5 * time.Minute

// SharePromisedStore holds the GPUs the utilization-share optimizer has
// promised to receivers: released by a donor, not yet held by the receiver's
// pods (docs/proposals/utilization-share-optimizer.md, section 6.3). They are
// free on the nodes and in the quota, and the scale-from-zero engine and the
// warm pool must not take them, or the receiver stays Pending until its fill
// timeout.
type SharePromisedStore struct {
	mu sync.RWMutex
	// promised is scope ("" for the cluster group, else a namespace) ->
	// accelerator -> GPUs.
	promised map[string]map[string]int
	at       time.Time
}

// Publish replaces the snapshot. An empty map is a real answer: nothing is
// promised.
func (s *SharePromisedStore) Publish(promised map[string]map[string]int, now time.Time) {
	c := make(map[string]map[string]int, len(promised))
	for scope, perType := range promised {
		c[scope] = maps.Clone(perType)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promised, s.at = c, now
}

// Latest returns a copy of the snapshot, or nil when there is none or it is
// older than maxAge.
func (s *SharePromisedStore) Latest(maxAge time.Duration, now time.Time) map[string]map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.promised == nil || now.Sub(s.at) > maxAge {
		return nil
	}
	c := make(map[string]map[string]int, len(s.promised))
	for scope, perType := range s.promised {
		c[scope] = maps.Clone(perType)
	}
	return c
}

// DefaultSharePromised is the process-wide store, published by the
// steady-state engine after each utilization-share pass.
var DefaultSharePromised = &SharePromisedStore{}

// PublishSharePromised records a snapshot in the default store.
func PublishSharePromised(promised map[string]map[string]int, now time.Time) {
	DefaultSharePromised.Publish(promised, now)
}

// LatestSharePromised reads the default store at SharePromisedMaxAge.
func LatestSharePromised(now time.Time) map[string]map[string]int {
	return DefaultSharePromised.Latest(SharePromisedMaxAge, now)
}
