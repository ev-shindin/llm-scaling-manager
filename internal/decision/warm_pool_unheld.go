package decision

import (
	"maps"
	"sync"
	"time"
)

// WarmPoolUnheldMaxAge is how long a namespace's published pool target is
// believed. The pools publish every reconcile pass; a figure older than this is
// from a pool that stopped reconciling, and carving GPUs out for it would hold
// them free for nothing.
const WarmPoolUnheldMaxAge = 5 * time.Minute

// WarmPoolUnheldStore holds, per namespace and accelerator, the GPUs the warm
// pools want and do not hold yet: the part of their target the
// utilization-share optimizer sets aside so the pools can grow into it
// (docs/proposals/utilization-share-optimizer.md, section 7.2).
type WarmPoolUnheldStore struct {
	mu sync.RWMutex
	ns map[string]warmPoolUnheld
}

type warmPoolUnheld struct {
	byType map[string]int
	at     time.Time
}

// Publish replaces a namespace's figure. An empty one clears it.
func (s *WarmPoolUnheldStore) Publish(namespace string, byType map[string]int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(byType) == 0 {
		delete(s.ns, namespace)
		return
	}
	if s.ns == nil {
		s.ns = map[string]warmPoolUnheld{}
	}
	s.ns[namespace] = warmPoolUnheld{byType: maps.Clone(byType), at: now}
}

// Latest returns every namespace's figure no older than maxAge.
func (s *WarmPoolUnheldStore) Latest(maxAge time.Duration, now time.Time) map[string]map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]int{}
	for ns, u := range s.ns {
		if now.Sub(u.at) <= maxAge {
			out[ns] = maps.Clone(u.byType)
		}
	}
	return out
}

// DefaultWarmPoolUnheld is the process-wide store, published by the warm-pool
// reconcilers and read by the utilization-share optimizer.
var DefaultWarmPoolUnheld = &WarmPoolUnheldStore{}
