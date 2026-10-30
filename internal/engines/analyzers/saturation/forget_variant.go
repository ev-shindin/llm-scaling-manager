package saturation

import (
	"strings"
	"time"
)

// Forgetting one variant's learned state, on an operator's request.
//
// The only flush there was is deleting the learned-state snapshot, which
// forgets every model in the cluster (ForgetLearnedState) and drops each to
// its derived figure or k1 -- the TTFT direction, fleet-wide -- to correct
// one. ForgetVariant drops one variant's state and nothing else's.
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

// forgetVariant drops everything learned for one variant -- every map
// learnedMapsLocked lists under the variant's keys: its k2 and throughput
// windows and what is kept beside them, its ITL window, baseline and the line
// it published, its start estimate, its accelerator memo, anything pending
// from a restore -- its own figure from previous engine arguments, and its
// capacity record, and records token as acted on. It returns how many
// entries it dropped.
//
// Its lower bounds are ENDED, not dropped (spendLowerBoundsLocked). The
// windows it drops are what a bound falls back on, so a dropped bound
// re-armed a sibling's figure at full strength on the variant's next serving
// cycle: an operator forgetting a variant held large by a borrow got the
// borrow back for another 6.5 serving hours. An ended one re-arms only on new
// evidence, as any spent bound does. A bucket it never borrowed in can still
// borrow a lower compatible sibling's figure; disableK2Borrow stops that.
//
// Siblings keep their own windows, and what they borrowed from it they read
// again from its window each cycle, which is gone. The ITL line it published
// is dropped too, so a sibling that read that line has none next cycle.
func (a *SaturationAnalyzer) forgetVariant(namespace, modelID, variant, token string) int {
	// The two key shapes a variant's state is filed under (variantOfKey).
	// The trailing separator keeps "v" from matching "v2".
	k2Prefix := namespace + "|" + variant + "|" + modelID + "|"
	itlPrefix := namespace + "|" + modelID + "|" + variant + "|"
	name := namespace + "/" + variant

	a.mu.Lock()
	n := a.dropVariantLocked(namespace, modelID, variant)
	// The maps with their own rule (scopeOwnRule).
	a.spendLowerBoundsLocked(k2Prefix, a.now())
	for key, line := range a.itlLines {
		if strings.HasPrefix(line.fromVariant, itlPrefix) {
			delete(a.itlLines, key)
			n++
		}
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

// forgottenToken is the last forget token acted on for a variant, or "".
// Reading it keeps the mark alive: the engine reads it every cycle the
// variant's scale target carries the annotation.
func (a *SaturationAnalyzer) forgottenToken(namespace, variant string) string {
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
