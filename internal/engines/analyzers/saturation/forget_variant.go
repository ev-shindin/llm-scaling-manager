package saturation

import (
	"maps"
	"strings"
	"time"
)

// Forgetting one variant's learned state, on an operator's request.
//
// The only flush there was is deleting the learned-state snapshot, which
// forgets every model in the cluster (ForgetLearnedState) and drops each to
// its derived figure or k1 -- the TTFT direction, fleet-wide -- to correct
// one. Redeploying the variant does not reset it either: the k2 and
// throughput windows are not keyed by engine configuration. ForgetVariant
// drops one variant's state and nothing else's.
//
// It is requested by an annotation on the variant's scale target
// (annotations.ForgetLearnedState) whose value is a token: any string, acted
// on once. The analyzer remembers the last token it acted on per variant, and
// the snapshot carries it, so the annotation can stay in place without
// forgetting again on every cycle or after every restart. Changing the value
// asks again.

// forgetMark is the last forget token acted on for a variant, with when it
// was last read or the variant found present; swept with the variant.
type forgetMark struct {
	token string
	seen  time.Time
}

// ForgetMarkSnapshot is a forgetMark as the snapshot carries it.
type ForgetMarkSnapshot struct {
	Token string    `json:"token"`
	Seen  time.Time `json:"seen"`
}

// dropPrefix deletes every key of m starting with prefix, and returns how
// many it deleted.
func dropPrefix[V any](m map[string]V, prefix string) int {
	n := len(m)
	maps.DeleteFunc(m, func(k string, _ V) bool { return strings.HasPrefix(k, prefix) })
	return n - len(m)
}

// ForgetVariant drops everything learned for one variant -- its k2 and
// throughput windows, its borrow clocks, its ITL window, baseline and the
// line it published, its start estimate, its accelerator memo, anything
// pending from a restore, and its capacity record -- and records token as
// acted on. Other variants, including ones that borrowed from it, keep what
// they have. It returns how many entries it dropped.
func (a *SaturationAnalyzer) ForgetVariant(namespace, modelID, variant, token string) int {
	// The two key shapes a variant's state is filed under (variantOfKey).
	// The trailing separator keeps "v" from matching "v2".
	k2Prefix := namespace + "|" + variant + "|" + modelID + "|"
	itlPrefix := namespace + "|" + modelID + "|" + variant + "|"
	name := namespace + "/" + variant

	a.mu.Lock()
	n := dropPrefix(a.computeCapacityHistory, k2Prefix) +
		dropPrefix(a.saturatedThroughput, k2Prefix) +
		dropPrefix(a.k2BorrowedSince, k2Prefix) +
		dropPrefix(a.itlWindows, itlPrefix) +
		dropPrefix(a.itlBaseline, itlPrefix) +
		dropPrefix(a.startSeconds, itlPrefix)
	dropPrefix(a.throughputSampledAt, k2Prefix)
	dropPrefix(a.throughputLastRead, k2Prefix)
	dropPrefix(a.k2BorrowState, k2Prefix)
	dropPrefix(a.startOutliers, itlPrefix)
	dropPrefix(a.startSeenPods, itlPrefix)
	dropPrefix(a.variantSeenAt, itlPrefix)
	for key, line := range a.itlLines {
		if strings.HasPrefix(line.fromVariant, itlPrefix) {
			delete(a.itlLines, key)
			n++
		}
	}
	if _, ok := a.lastAccelerator[name]; ok {
		delete(a.lastAccelerator, name)
		n++
	}
	if p, ok := a.pendingRestore[name]; ok {
		n += p.size()
		delete(a.pendingRestore, name)
	}
	a.forgotten[name] = forgetMark{token: token, seen: a.now()}
	a.mu.Unlock()

	// The store has its own lock. The engine reloads a record from the
	// scale target on the same cycle, so what is gone is the live figure.
	if a.capacityStore.Delete(namespace, modelID, variant) {
		n++
	}
	return n
}

// ForgottenToken is the last forget token acted on for a variant, or "".
// Reading it keeps the mark alive: the engine reads it every cycle the
// variant's scale target carries the annotation.
func (a *SaturationAnalyzer) ForgottenToken(namespace, variant string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.forgotten[namespace+"/"+variant]
	if !ok {
		return ""
	}
	m.seen = a.now()
	a.forgotten[namespace+"/"+variant] = m
	return m.token
}
