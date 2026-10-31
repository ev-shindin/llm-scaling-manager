package decision

import (
	"maps"
	"sync"
	"time"
)

// BridgeStore records which warm pool Pods are currently LENT, and to which
// variant.
//
// It exists so the collector can attribute a bridge's engine metrics. A pool Pod
// is owned by the POOL's workload, so the ownerReference walk that attributes
// every other Pod reaches the pool's scale target and not the model's -- the Pod
// is serving a variant's traffic while belonging, structurally, to something
// else entirely. Without this the metrics are dropped as unattributed, and the
// load the bridge is carrying is invisible to the analyzer that decides how much
// capacity the model needs.
//
// Which is the wrong way round. A bridge exists precisely because a variant is
// short, so its traffic is the traffic that most needs counting; dropping it
// makes demand look lowest exactly when it is highest, and makes the demand
// appear from nowhere the moment the bridge is handed back.
//
// The pool publishes this because the pool is the only component that knows it.
// Lending is a decision it makes and records in Pod labels, and reading those
// labels back out would mean the collector re-deriving a fact the controller
// already has -- against an InferencePool selector, which is a join the
// collector has no reason to carry.
type BridgeStore struct {
	mu sync.RWMutex
	// by is namespace → the whole lending map for that namespace.
	by map[string]bridgeSnapshot
}

// bridgeSnapshot is one namespace's lending, as of one pass.
type bridgeSnapshot struct {
	// lentTo is pod name → the VARIANT the Pod is lent to, named by its
	// ScaledObject, which is what the collector resolves for every other Pod
	// and what the analyzer keys its rows by. NOT the scale target: that is the
	// Deployment beneath the ScaledObject, a different string on any real
	// deployment, and a lending published under it matches no analyzer row.
	lentTo map[string]string
	// since is pod name → when its current lend began: the first pass that
	// published the Pod lent to that variant, carried across passes that
	// keep publishing it so.
	since map[string]time.Time
	at    time.Time
}

// NewBridgeStore returns an empty store.
func NewBridgeStore() *BridgeStore {
	return &BridgeStore{by: map[string]bridgeSnapshot{}}
}

// DefaultBridges is the store the warm pool writes and the collector reads.
var DefaultBridges = NewBridgeStore()

// Publish replaces a namespace's lending wholesale.
//
// Wholesale, and an EMPTY map is a real answer: a Pod handed back has stopped
// being a bridge, and a store that only ever learned of new lendings would go on
// attributing a returned Pod's metrics to a variant it no longer serves. The
// pool publishes on every pass in which it could observe itself, and publishes
// nothing on a pass where it could not -- "could not see" must not read as
// "nothing is lent".
//
// A Pod published lent to the same variant as on the namespace's previous
// pass keeps the time its lend began; any other lent Pod's lend begins at
// at. So a Pod handed back and lent again reads a new lend time, though its
// name, its variant and its Ready time are what they were (LendFor).
func (s *BridgeStore) Publish(namespace string, lentTo map[string]string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]bridgeSnapshot{}
	}
	prev := s.by[namespace]
	since := make(map[string]time.Time, len(lentTo))
	for pod, variant := range lentTo {
		since[pod] = at
		if was, ok := prev.since[pod]; ok && prev.lentTo[pod] == variant {
			since[pod] = was
		}
	}
	s.by[namespace] = bridgeSnapshot{lentTo: maps.Clone(lentTo), since: since, at: at}
}

// VariantFor returns the variant a Pod is lent to, and whether it is a bridge at
// all.
//
// A reading older than maxAge answers false. The pool republishes every pass, so
// a stale map means the pool's reconciler has stopped -- and attributing a Pod
// to a variant on the strength of a lending that may since have ended would add
// demand for load nobody is carrying.
func (s *BridgeStore) VariantFor(namespace, pod string, maxAge time.Duration, now time.Time) (string, bool) {
	variant, _, lent := s.LendFor(namespace, pod, maxAge, now)
	return variant, lent
}

// LendFor is VariantFor with the time the Pod's current lend began
// (Publish).
func (s *BridgeStore) LendFor(namespace, pod string, maxAge time.Duration, now time.Time) (string, time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.by[namespace]
	if !ok {
		return "", time.Time{}, false
	}
	if maxAge > 0 && now.Sub(snapshot.at) > maxAge {
		return "", time.Time{}, false
	}
	variant, lent := snapshot.lentTo[pod]
	if !lent || variant == "" {
		return "", time.Time{}, false
	}
	return variant, snapshot.since[pod], true
}

// Reset drops every lending. For tests, and for a controller that has lost its
// leadership.
func (s *BridgeStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = map[string]bridgeSnapshot{}
}

// PublishBridges records a namespace's lending in the default store.
func PublishBridges(namespace string, lentTo map[string]string, at time.Time) {
	DefaultBridges.Publish(namespace, lentTo, at)
}

// BridgeVariant reads a lending from the default store.
func BridgeVariant(namespace, pod string, maxAge time.Duration, now time.Time) (string, bool) {
	return DefaultBridges.VariantFor(namespace, pod, maxAge, now)
}
