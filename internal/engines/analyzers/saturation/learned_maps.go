package saturation

import (
	"maps"
	"strings"
)

// Every map the analyzer keeps per key, in one list.
//
// Forgetting a variant (ForgetVariant) and flushing everything
// (ForgetLearnedState) used to name the maps they cleared by hand, and each
// new map was a chance to miss one: a forget once left a previous figure
// behind, and a trim kept one without its clock. learnedMapsLocked lists them all,
// with how their keys name a variant, and TestEveryAnalyzerMapIsRegistered
// fails when a map field of SaturationAnalyzer is not in it. The ITL windows
// and baselines are not maps of the analyzer's but the tracker's
// (itl.Tracker), keyed like scopeITLKey: dropVariantLocked and
// ForgetLearnedState reach them through it.

// keyScope is how a map's keys name the variant an entry belongs to.
type keyScope int

const (
	// scopeK2Key: namespace|variant|model|... -- the k2 and throughput
	// windows and everything kept beside them.
	scopeK2Key keyScope = iota
	// scopeITLKey: namespace|model|variant|... -- the start estimate and
	// liveness, keyed like the tracker's ITL windows and baselines.
	scopeITLKey
	// scopeVariantName: namespace/variant.
	scopeVariantName
	// scopeOwnRule: per variant, but ForgetVariant treats it by its own rule
	// (lowerBounds is ended rather than dropped, itlLines is matched on the
	// variant that published a line, forgotten is what records the forget).
	scopeOwnRule
	// scopeShared: not per variant (per model or per configuration) and not
	// learned from a variant's capacity -- short memories the per-cycle gates
	// read (decode's recent saturation, the fleet's bucket hysteresis). A
	// forget leaves it, and so does a flush of the learned state.
	scopeShared
)

// learnedMap is one per-key map: its field name, its key scope, how to drop
// entries from it, and whether a flush of the learned state takes it.
type learnedMap struct {
	name   string
	scope  keyScope
	dropIf func(func(key string) bool) int
	clear  func()
	// flushed says ForgetLearnedState clears it: the state the snapshot
	// carries, which an operator deleting the snapshot means to drop. A
	// working memory of the current cycles (the ITL samples not yet fitted,
	// the pods whose start is already counted, a shared gate's memory) is not
	// what they deleted, and clearing it changes the next cycles' gates.
	flushed bool
}

func registered[V any](name string, scope keyScope, m map[string]V) learnedMap {
	return learnedMap{
		name:  name,
		scope: scope,
		dropIf: func(f func(string) bool) int {
			n := len(m)
			maps.DeleteFunc(m, func(k string, _ V) bool { return f(k) })
			return n - len(m)
		},
		clear:   func() { clear(m) },
		flushed: scope != scopeShared,
	}
}

// working marks a map a flush leaves (learnedMap.flushed).
func (m learnedMap) working() learnedMap {
	m.flushed = false
	return m
}

// learnedMapsLocked lists every per-key map. Callers hold a.mu.
func (a *SaturationAnalyzer) learnedMapsLocked() []learnedMap {
	return []learnedMap{
		registered("computeCapacityHistory", scopeK2Key, a.computeCapacityHistory),
		registered("saturatedThroughput", scopeK2Key, a.saturatedThroughput),
		registered("throughputSampledAt", scopeK2Key, a.throughputSampledAt),
		registered("throughputLastRead", scopeK2Key, a.throughputLastRead),
		registered("throughputPodReads", scopeK2Key, a.throughputPodReads),
		registered("throughputMuChoice", scopeK2Key, a.throughputMuChoice),
		registered("k2WindowArgs", scopeK2Key, a.k2WindowArgs),
		registered("throughputWindowArgs", scopeK2Key, a.throughputWindowArgs),
		registered("k2Stash", scopeK2Key, a.k2Stash),
		registered("throughputStash", scopeK2Key, a.throughputStash),
		registered("throughputHeld", scopeK2Key, a.throughputHeld),
		registered("lowerBounds", scopeOwnRule, a.lowerBounds),
		registered("itlArgs", scopeITLKey, a.itlArgs),
		registered("itlBaselineStash", scopeITLKey, a.itlBaselineStash),
		registered("startSeconds", scopeITLKey, a.startSeconds),
		registered("startSeenPods", scopeITLKey, a.startSeenPods).working(),
		registered("startOutliers", scopeITLKey, a.startOutliers).working(),
		registered("variantSeenAt", scopeITLKey, a.variantSeenAt),
		registered("lastAccelerator", scopeVariantName, a.lastAccelerator),
		registered("pendingRestore", scopeVariantName, a.pendingRestore),
		registered("itlLines", scopeOwnRule, a.itlLines),
		registered("forgotten", scopeOwnRule, a.forgotten),
		registered("decodeSaturatedAt", scopeShared, a.decodeSaturatedAt),
		registered("hotRankHeld", scopeShared, a.hotRankHeld),
		registered("k2Settle", scopeShared, a.k2Settle),
		registered("fleetShape", scopeShared, a.fleetShape),
		registered("ownSettled", scopeShared, a.ownSettled),
		registered("k2Cycles", scopeShared, a.k2Cycles),
		registered("k2Logged", scopeShared, a.k2Logged),
		registered("k2Forgotten", scopeShared, a.k2Forgotten),
		registered("learnerReports", scopeShared, a.learnerReports),
		registered("learnerStarted", scopeShared, a.learnerStarted),
	}
}

// dropVariantLocked drops one variant's entries from every map whose keys
// name it plainly, and returns how many it dropped. The maps with their own
// rule (scopeOwnRule) and the shared ones are left to the caller. Callers hold
// a.mu.
func (a *SaturationAnalyzer) dropVariantLocked(namespace, modelID, variant string) int {
	// The trailing separator keeps "v" from matching "v2".
	k2Prefix := namespace + "|" + variant + "|" + modelID + "|"
	itlPrefix := namespace + "|" + modelID + "|" + variant + "|"
	name := namespace + "/" + variant
	n := 0
	for _, m := range a.learnedMapsLocked() {
		switch m.scope {
		case scopeK2Key:
			n += m.dropIf(func(k string) bool { return strings.HasPrefix(k, k2Prefix) })
		case scopeITLKey:
			n += m.dropIf(func(k string) bool { return strings.HasPrefix(k, itlPrefix) })
		case scopeVariantName:
			n += m.dropIf(func(k string) bool { return k == name })
		}
	}
	windows, baselines := a.itlTracker.Sweep(a.now(), func(k string) bool { return strings.HasPrefix(k, itlPrefix) })
	n += windows + baselines
	return n
}
