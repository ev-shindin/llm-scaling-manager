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

// PromisedHoles is the GPUs of one scope and accelerator the utilization-share
// optimizer has promised, by where their holes are: Nodes on the node a hole
// is known to be on, Unplaced where it is not known.
type PromisedHoles struct {
	Nodes    map[string]int
	Unplaced int
}

// Total is every GPU h holds back, placed or not.
func (h PromisedHoles) Total() int {
	n := h.Unplaced
	for _, g := range h.Nodes {
		n += g
	}
	return n
}

// Merge returns h with o's GPUs added, node by node. A publish has one entry
// per scope and accelerator, as share groups are keyed, so the share merges
// into an empty h; Merge sums all the same, and never aliases either side.
func (h PromisedHoles) Merge(o PromisedHoles) PromisedHoles {
	out := PromisedHoles{Unplaced: h.Unplaced + o.Unplaced}
	if len(h.Nodes)+len(o.Nodes) > 0 {
		out.Nodes = maps.Clone(h.Nodes)
		if out.Nodes == nil {
			out.Nodes = map[string]int{}
		}
		for n, g := range o.Nodes {
			out.Nodes[n] += g
		}
	}
	return out
}

// SharePromisedStore holds the GPUs the utilization-share optimizer has
// promised to receivers: released by a donor, not yet held by the receiver's
// pods (docs/proposals/utilization-share-optimizer.md, section 6.3). They are
// free on the nodes and in the quota, and the scale-from-zero engine and the
// warm pool must not take them, or the receiver stays Pending until its fill
// timeout. Each promise is kept with the nodes its holes are on, so a node
// view holds it there and not on whichever node is freest.
type SharePromisedStore struct {
	mu sync.RWMutex
	// promised is scope ("" for the cluster group, else a namespace) ->
	// accelerator -> holes.
	promised map[string]map[string]PromisedHoles
	at       time.Time
}

// Publish replaces the snapshot. An empty map is a real answer: nothing is
// promised.
func (s *SharePromisedStore) Publish(promised map[string]map[string]PromisedHoles, now time.Time) {
	c := clonePromised(promised)
	if c == nil {
		c = map[string]map[string]PromisedHoles{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promised, s.at = c, now
}

// Latest returns the snapshot's GPUs per scope and accelerator, or nil when
// there is none or it is older than maxAge.
func (s *SharePromisedStore) Latest(maxAge time.Duration, now time.Time) map[string]map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.promised == nil || now.Sub(s.at) > maxAge {
		return nil
	}
	return PromisedTotals(s.promised)
}

// PromisedTotals is the GPUs of each promise in promised, per scope and
// accelerator: what the quota withholds. nil for nil.
func PromisedTotals(promised map[string]map[string]PromisedHoles) map[string]map[string]int {
	if promised == nil {
		return nil
	}
	c := make(map[string]map[string]int, len(promised))
	for scope, perType := range promised {
		c[scope] = make(map[string]int, len(perType))
		for acc, h := range perType {
			c[scope][acc] = h.Total()
		}
	}
	return c
}

// LatestHoles returns a copy of the snapshot with each promise's nodes, or nil
// when there is none or it is older than maxAge.
func (s *SharePromisedStore) LatestHoles(maxAge time.Duration, now time.Time) map[string]map[string]PromisedHoles {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.promised == nil || now.Sub(s.at) > maxAge {
		return nil
	}
	return clonePromised(s.promised)
}

// clonePromised is a deep copy of promised; nil for nil.
func clonePromised(promised map[string]map[string]PromisedHoles) map[string]map[string]PromisedHoles {
	if promised == nil {
		return nil
	}
	c := make(map[string]map[string]PromisedHoles, len(promised))
	for scope, perType := range promised {
		c[scope] = make(map[string]PromisedHoles, len(perType))
		for acc, h := range perType {
			c[scope][acc] = PromisedHoles{Nodes: maps.Clone(h.Nodes), Unplaced: h.Unplaced}
		}
	}
	return c
}

// DefaultSharePromised is the process-wide store, published by the
// steady-state engine after each utilization-share pass.
var DefaultSharePromised = &SharePromisedStore{}

// PublishSharePromised records a snapshot in the default store.
func PublishSharePromised(promised map[string]map[string]PromisedHoles, now time.Time) {
	DefaultSharePromised.Publish(promised, now)
}

// LatestSharePromised reads the default store's GPUs per scope and
// accelerator at SharePromisedMaxAge: what the quota withholds.
func LatestSharePromised(now time.Time) map[string]map[string]int {
	return DefaultSharePromised.Latest(SharePromisedMaxAge, now)
}

// LatestSharePromisedHoles reads the default store with each promise's nodes
// at SharePromisedMaxAge: what a node view holds back.
func LatestSharePromisedHoles(now time.Time) map[string]map[string]PromisedHoles {
	return DefaultSharePromised.LatestHoles(SharePromisedMaxAge, now)
}
