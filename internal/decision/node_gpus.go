package decision

import (
	"maps"
	"sync"
	"time"
)

// NodeGPU is one GPU node as the usage refresher last saw it: its accelerator,
// capacity, the GPUs requested by pods scheduled to it, its labels, and
// whether it is cordoned.
type NodeGPU struct {
	Accelerator   string
	Capacity      int
	Used          int
	Labels        map[string]string
	Unschedulable bool
}

// Free is the node's GPUs no scheduled pod requests.
func (n NodeGPU) Free() int { return max(0, n.Capacity-n.Used) }

// NodeGPUsMaxAge is how old a node snapshot may be and still be used to place
// pods. Older, the holes it shows may have been filled.
const NodeGPUsMaxAge = 2 * time.Minute

// NodeGPUStore holds the latest per-node GPU snapshot.
type NodeGPUStore struct {
	mu    sync.RWMutex
	nodes map[string]NodeGPU
	at    time.Time
}

// Publish replaces the snapshot.
func (s *NodeGPUStore) Publish(nodes map[string]NodeGPU, now time.Time) {
	c := make(map[string]NodeGPU, len(nodes))
	for k, n := range nodes {
		n.Labels = maps.Clone(n.Labels)
		c[k] = n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes, s.at = c, now
}

// Latest returns a copy of the snapshot, or nil when there is none or it is
// older than maxAge.
func (s *NodeGPUStore) Latest(maxAge time.Duration, now time.Time) map[string]NodeGPU {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.nodes == nil || now.Sub(s.at) > maxAge {
		return nil
	}
	c := make(map[string]NodeGPU, len(s.nodes))
	for k, n := range s.nodes {
		n.Labels = maps.Clone(n.Labels)
		c[k] = n
	}
	return c
}

// DefaultNodeGPUs is the process-wide store, published by the usage refresher.
var DefaultNodeGPUs = &NodeGPUStore{}
