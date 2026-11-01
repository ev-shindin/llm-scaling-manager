package saturation

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learn"
)

// k2 as a learner on the shared rules (docs/proposals/analyzer-learner.md,
// 6.1 to 6.3): each cycle it fills a learn.Report per variant, logs one
// learner-write line per key whose figure moved, and Forget reaches it
// through learn's list of learners. The analyzer logs; the Report carries
// counts, never a log line.

const (
	// K2WriteBand is how far k2's figure under a key must move from the
	// value it last logged, as a fraction of that value, before
	// learner-write logs it again. The figure is the mean of a ten-reading
	// window, so one admitted reading moves it by a tenth of its distance
	// from the mean: a reading 20 % off the mean moves it 2 %. Under the
	// band, a reading is noise; a slow drift still logs once it has moved
	// the band from the last logged value. Not measured on a cluster run
	// (the recording PR 2 needs is not taken yet).
	K2WriteBand = 0.02
	// LearnerHeartbeat is how long a key that priced or was written may go
	// without a learner-write line before it logs one anyway.
	LearnerHeartbeat = 10 * time.Minute
	// LearnerWriteMaxPods is how many contributing pods one learner-write
	// line lists, lowest reading first; the rest are counted in podsOmitted,
	// so a line stays under the CRI line limit however large the fleet.
	LearnerWriteMaxPods = 16
	// k2TimeBase is the time base of a k2 reading: the busiest rank's
	// resident tokens at its one-minute peak, scaled to the pod.
	k2TimeBase = "busiest-rank-max_over_time[1m]"
)

// k2PodReading is one admitted k2 reading of a cycle: the pod, its reading,
// and the admission tests it passed with their margins.
type k2PodReading struct {
	pod     string
	reading int64
	margin  k2Margin
}

// k2Margin is how a saturated reading passed k2's admission: how long the
// replica had been settled (zero when the joined test did not run on the
// tracker), and which of the settle tests let it through (k2Unsettled):
// "batch" (its queue is the overflow of a full batch), "rate" (its rate and
// residency reach what is on record) or "settled" (flat over a window).
type k2Margin struct {
	settledFor time.Duration
	passedBy   string
	// queueOverThreshold is the busiest rank's queue over the saturation
	// threshold: at least 1 for a saturated reading.
	queueOverThreshold float64
}

// k2VariantCycle is one variant's k2 on one cycle.
type k2VariantCycle struct {
	role   string
	key    string
	source string
	counts map[learn.Reason]int64
	writes int64
	// pods is the cycle's admitted readings, by key.
	pods map[string][]k2PodReading
}

// k2Cycle is one model's k2 on one cycle, by variant, and the margins of the
// readings k2Unsettled admitted, by variant/pod.
type k2Cycle struct {
	variants map[string]*k2VariantCycle
	margins  map[string]k2Margin
}

// k2Logged is the value learner-write last logged for a key, and when.
type k2Logged struct {
	value float64
	at    time.Time
}

// beginK2Cycle starts k2's cycle for a model, at the top of Analyze: what
// an Analyze that failed part way left is dropped.
func (a *SaturationAnalyzer) beginK2Cycle(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.k2Cycles, namespace+"|"+modelID)
}

// k2CycleLocked is model's k2 cycle in progress. Callers hold a.mu.
func (a *SaturationAnalyzer) k2CycleLocked(model string) *k2Cycle {
	c := a.k2Cycles[model]
	if c == nil {
		c = &k2Cycle{variants: map[string]*k2VariantCycle{}, margins: map[string]k2Margin{}}
		a.k2Cycles[model] = c
	}
	return c
}

// noteK2Margin records how a saturated reading passed k2Unsettled.
func (a *SaturationAnalyzer) noteK2Margin(model, pod string, m k2Margin) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.k2CycleLocked(model).margins[pod] = m
}

// noteK2 records what k2 made of one replica this cycle: the key and source
// that priced it, and, for a saturated reading, whether it was admitted
// (P1-obs, a write) or why not (the P1-obs-* label).
func (a *SaturationAnalyzer) noteK2(namespace, modelID, variant, role, key, pod string,
	d k2Decision, reading int64) {
	model := namespace + "|" + modelID
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.k2CycleLocked(model)
	v := c.variants[variant]
	if v == nil {
		v = &k2VariantCycle{counts: map[learn.Reason]int64{}, pods: map[string][]k2PodReading{}}
		c.variants[variant] = v
	}
	// A variant's replicas share one key (the fleet's tracked output length
	// keys it, not each replica's), so the last replica's key and source
	// are the variant's.
	v.role, v.key, v.source = role, key, d.src.String()
	switch {
	case d.src == capacity.K2SrcObserved:
		v.counts[learn.Reason(capacity.K2SrcObserved.String())]++
		v.writes++
		v.pods[key] = append(v.pods[key], k2PodReading{pod: pod, reading: reading, margin: c.margins[variant+"/"+pod]})
	case d.refused != "":
		v.counts[learn.Reason(d.refused)]++
	}
}

// closeK2Cycle ends k2's cycle for a model, after its last replica is
// priced: it files a learn.Report per variant (LearnerReports) and logs a
// learner-write line per key that priced or was written and whose figure
// was set for the first time, moved K2WriteBand from what was last logged,
// or has not logged for LearnerHeartbeat. At most one line per key per
// cycle, at V(3).
func (a *SaturationAnalyzer) closeK2Cycle(ctx context.Context, namespace, modelID string) {
	now := a.now()
	model := namespace + "|" + modelID
	type line struct {
		variant  string
		key      string
		v        *k2VariantCycle
		value    float64
		previous float64
		first    bool
		samples  int
	}
	var lines []line
	a.mu.Lock()
	c := a.k2CycleLocked(model)
	delete(a.k2Cycles, model)
	pending := a.k2Forgotten[model]
	delete(a.k2Forgotten, model)
	reports := make([]learn.Report, 0, len(c.variants)+len(pending))
	variants := make([]string, 0, len(c.variants))
	for v := range c.variants {
		variants = append(variants, v)
	}
	for v := range pending {
		if _, ok := c.variants[v]; !ok {
			variants = append(variants, v)
		}
	}
	sort.Strings(variants)
	for _, variant := range variants {
		v := c.variants[variant]
		r := learn.Report{Learner: learn.K2, Namespace: namespace, ModelID: modelID, Variant: variant,
			Forgotten: pending[variant]}
		if v != nil {
			r.Role, r.Source, r.Counts, r.Writes = v.role, learn.Source(v.source), v.counts, v.writes
			if ra, ok := a.computeCapacityHistory[v.key]; ok && ra.Len() > 0 {
				r.PricingKey, r.Samples, r.LastAdmitted = v.key, ra.Len(), ra.LastWritten()
			}
			keys := []string{v.key}
			for k := range v.pods {
				if k != v.key {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys[1:])
			for _, key := range keys {
				ra, ok := a.computeCapacityHistory[key]
				if !ok || ra.Len() == 0 {
					continue
				}
				value := ra.Average()
				last, logged := a.k2Logged[key]
				if logged && math.Abs(value-last.value) <= K2WriteBand*last.value && now.Sub(last.at) < LearnerHeartbeat {
					continue
				}
				lines = append(lines, line{variant: variant, key: key, v: v, value: value, previous: last.value,
					first: !logged, samples: ra.Len()})
				a.k2Logged[key] = k2Logged{value: value, at: now}
			}
		}
		reports = append(reports, r)
	}
	a.learnerReports[model] = reports
	a.mu.Unlock()

	logger := ctrl.LoggerFrom(ctx)
	for _, l := range lines {
		pods, omitted := k2WritePods(l.v.pods[l.key])
		logger.V(perReplicaLogLevel).Info("learner-write",
			"learner", learn.K2, "modelID", modelID, "namespace", namespace, "variant", l.variant,
			"role", l.v.role, "key", l.key, "value", int64(l.value), "previousValue", int64(l.previous),
			"firstWrite", l.first, "source", l.v.source, "samples", l.samples,
			"pods", pods, "podsOmitted", omitted)
	}
}

// k2WritePods is a learner-write line's pods: at most LearnerWriteMaxPods,
// lowest reading first, each with its reading, its time base and the
// admission tests it passed with their margins, and how many were left out.
func k2WritePods(rs []k2PodReading) ([]map[string]any, int) {
	rs = slices.Clone(rs)
	slices.SortStableFunc(rs, func(x, y k2PodReading) int {
		if x.reading != y.reading {
			return int(min(max(x.reading-y.reading, -1), 1))
		}
		return strings.Compare(x.pod, y.pod)
	})
	omitted := max(len(rs)-LearnerWriteMaxPods, 0)
	rs = rs[:len(rs)-omitted]
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		out = append(out, map[string]any{
			"pod": r.pod, "reading": r.reading, "timeBase": k2TimeBase,
			"settledForSeconds": r.margin.settledFor.Seconds(), "passedBy": r.margin.passedBy,
			"queueOverThreshold": r.margin.queueOverThreshold,
		})
	}
	return out, omitted
}

// sweepLearnerMapsLocked drops what k2 as a learner, and the analyzer's own
// settle tracker, keep for a model the sweep has let go of: its reports, its
// pending forgotten keys, its learner-start mark and its own-tracker sighting
// go with its settle memo (k2Settle, swept at VariantTimeout unseen), and
// what learner-write last logged of a key goes with the key's window. A model
// that comes back is a model seen for the first time: it logs learner-start,
// and each key logs its first write. Called at the end of evictStaleHistory;
// callers hold a.mu.
func (a *SaturationAnalyzer) sweepLearnerMapsLocked() {
	gone := func(model string) bool { _, ok := a.k2Settle[model]; return !ok }
	maps.DeleteFunc(a.learnerReports, func(m string, _ []learn.Report) bool { return gone(m) })
	// A cycle an Analyze left part way (priceReplicas returned early, and
	// closeK2Cycle never ran) goes with its model too.
	maps.DeleteFunc(a.k2Cycles, func(m string, _ *k2Cycle) bool { return gone(m) })
	maps.DeleteFunc(a.k2Forgotten, func(m string, _ map[string][]learn.Forgotten) bool { return gone(m) })
	maps.DeleteFunc(a.learnerStarted, func(m string, _ bool) bool { return gone(m) })
	maps.DeleteFunc(a.ownSettled, func(m string, _ bool) bool {
		if !gone(m) {
			return false
		}
		if ns, model, ok := strings.Cut(m, "|"); ok && a.ownSettle != nil {
			a.ownSettle.Forget(settleModel(ns, model))
		}
		return true
	})
	maps.DeleteFunc(a.k2Logged, func(key string, _ k2Logged) bool {
		_, ok := a.computeCapacityHistory[key]
		return !ok
	})
}

// LearnerReports is each learner's report of each variant the model priced
// on its last cycle, by namespace|model: overwritten per model every cycle,
// so it stays bounded. A copy; reading it changes nothing.
func (a *SaturationAnalyzer) LearnerReports() map[string][]learn.Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string][]learn.Report, len(a.learnerReports))
	for m, rs := range a.learnerReports {
		cp := make([]learn.Report, len(rs))
		for i, r := range rs {
			cp[i] = r.Clone()
		}
		out[m] = cp
	}
	return out
}

// forgetLearnerLocked forgets one learner's state for a variant, on an
// operator's request, and returns how many entries it dropped: the learners
// named in learn.Learners are forgotten by name, here, not through a
// registry. A name it does not know is a learner put on the list without
// the code that forgets it, and panics; a test walks the list. Callers hold
// a.mu.
func (a *SaturationAnalyzer) forgetLearnerLocked(name, namespace, modelID, variant string, by learn.By) int {
	if name == learn.K2 {
		return a.forgetK2Locked(namespace, modelID, variant, by)
	}
	panic("saturation: no forget for the learner " + name + " on learn.Learners")
}

// forgetK2Locked forgets k2's state for one variant: its windows under every
// key, their arguments and stashes, and what learner-write last logged of
// them; and it ends, not drops, the variant's lower bounds
// (spendLowerBoundsLocked: a dropped bound would re-arm a sibling's figure
// at full strength). Each window's key is reported forgotten, by by, on the
// variant's next report. It returns how many entries it dropped, the
// logged values not counted. Callers hold a.mu.
func (a *SaturationAnalyzer) forgetK2Locked(namespace, modelID, variant string, by learn.By) int {
	prefix := namespace + "|" + variant + "|" + modelID + "|"
	n := 0
	var keys []string
	for key := range a.computeCapacityHistory {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		delete(a.computeCapacityHistory, key)
		n++
	}
	for key := range a.k2WindowArgs {
		if strings.HasPrefix(key, prefix) {
			delete(a.k2WindowArgs, key)
			n++
		}
	}
	for key := range a.k2Stash {
		if strings.HasPrefix(key, prefix) {
			delete(a.k2Stash, key)
			n++
		}
	}
	for key := range a.k2Logged {
		if strings.HasPrefix(key, prefix) {
			delete(a.k2Logged, key)
		}
	}
	a.spendLowerBoundsLocked(prefix, a.now())
	a.noteForgottenLocked(namespace, modelID, variant, keys, by)
	return n
}

// noteForgottenLocked files keys of a variant as forgotten, by by, for the
// variant's next report. Callers hold a.mu.
func (a *SaturationAnalyzer) noteForgottenLocked(namespace, modelID, variant string, keys []string, by learn.By) {
	if len(keys) == 0 {
		return
	}
	model := namespace + "|" + modelID
	if a.k2Forgotten[model] == nil {
		a.k2Forgotten[model] = map[string][]learn.Forgotten{}
	}
	for _, k := range keys {
		a.k2Forgotten[model][variant] = append(a.k2Forgotten[model][variant], learn.Forgotten{Key: k, By: by})
	}
}

// logLearnerStart logs the learner-start line for a model on the analyzer's
// first cycle for it, at the default level, and only then: how many k2 keys
// of the model were restored and how many of its variants start cold, the
// oldest restored key's last admitted reading, and settleUntil, when the
// fleet-wide wait a controller start begins with ends. It is the line that
// says every replica waits again.
func (a *SaturationAnalyzer) logLearnerStart(ctx context.Context, namespace, modelID string, variants []string) {
	now := a.now()
	model := namespace + "|" + modelID
	a.mu.Lock()
	if a.learnerStarted[model] {
		a.mu.Unlock()
		return
	}
	a.learnerStarted[model] = true
	restored, oldest := 0, time.Duration(-1)
	warm := map[string]bool{}
	for key, ra := range a.computeCapacityHistory {
		name, model, ok := variantOfKey(key)
		ns, variant, _ := strings.Cut(name, "/")
		if !ok || ns != namespace || model != modelID || ra.Len() == 0 {
			continue
		}
		restored++
		warm[variant] = true
		if age := now.Sub(ra.LastWritten()); age > oldest {
			oldest = age
		}
	}
	waits := a.k2ReadsSettle && a.k2WaitsAfterRestart
	a.mu.Unlock()
	cold := 0
	for _, v := range variants {
		if !warm[v] {
			cold++
		}
	}
	settleUntil := now
	reason := "the analyzer's first cycle for this model, after a controller start or as the model first appears: replicas already Ready keep their settle"
	if waits {
		settleUntil = now.Add(JoinedReplicaWindow)
		reason = "the analyzer's first cycle for this model, after a controller start or as the model first appears: each replica waits its settle, k2 a rate window from the controller's first sight of it, so by settleUntil at the latest"
	}
	oldestSeconds := -1.0
	if oldest >= 0 {
		oldestSeconds = oldest.Seconds()
	}
	ctrl.LoggerFrom(ctx).V(logging.DEFAULT).Info("learner-start",
		"modelID", modelID, "namespace", namespace,
		"restoredKeys", map[string]int{learn.K2: restored}, "coldVariants", map[string]int{learn.K2: cold},
		"oldestAdmittedAgeSeconds", oldestSeconds, "settleUntil", settleUntil.UTC().Format(time.RFC3339),
		"reason", reason)
}
