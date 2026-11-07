package saturation

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learn"
)

// Mu as a learner on the shared rules (docs/proposals/analyzer-learner.md,
// 6.1 to 6.3): the measured service rate (inventory row 5) and the choice
// between it and the figure derived from the ITL line (row 6). Each cycle it
// fills a learn.Report per variant and logs one learner-write line per key
// whose priced figure moved, or whose source flipped, as k2 does
// (k2_learner.go). The analyzer logs; the Report carries counts.
//
// Mu has no joined test of its own: a sample is written only beside a k2
// P1-obs write, so it inherits k2's (k2Unsettled), which reads the settle
// tracker (settle_view.go). Each admitted reading carries k2's stamp -- how
// long the replica had been settled, and which settle test let it through --
// and learner-write shows it beside the reading. So a measured mu waits out
// the settle after a controller restart as k2 does
// (SwitchK2WaitsAfterRestart).

const (
	// MuWriteBand is how far mu's priced figure under a key must move from
	// the value it last logged, as a fraction of that value, before
	// learner-write logs it again. Above the 4 % of noise on the output
	// length that, at a hard edge, flipped the choice 11 times in 20 cycles
	// (analyzer-learner.md, 6.1): a derived figure carries that noise every
	// cycle. A source flip logs whatever the value. Not measured on a
	// cluster run.
	MuWriteBand = 0.05
	// muTimeBase is the time base of a mu reading: the completion rate over
	// the collector's rate window, at the fleet's shape
	// (saturatedCompletionRate).
	muTimeBase = "rate[1m]"
)

// SwitchMuForgetReported is the replay switch for mu on learn's list of
// learners: Forget reaches it by name (forgetMuLocked), what learner-write
// last logged of its keys goes with them, and each key forgotten or evicted
// is reported on the variant's next report. Off, its windows go with the
// variant's other maps (dropVariantLocked), as before: the entries dropped
// are the same, nothing is reported, and the next learner-write reads the
// value logged before the forget as its previous one, or stays silent inside
// the band. On from the constructor; a replay turns it off
// (SetSwitchesForReplay).
const SwitchMuForgetReported = "mu-forget-reported"

// Mu's admission outcomes and yields, the reasons its Report counts.
const (
	muReasonSampled             learn.Reason = "sampled"
	muReasonFolded              learn.Reason = "folded"
	muReasonRepeated            learn.Reason = "repeated"
	muReasonLowConfirmed        learn.Reason = "low-confirmed"
	muReasonRefusedNoCompletion learn.Reason = "refused-no-completion"
	muReasonRefusedFilling      learn.Reason = "refused-filling"
	muReasonRefusedUnderServed  learn.Reason = "refused-under-served"
	muReasonRefusedUnderDerived learn.Reason = "refused-under-derived"
	muReasonYieldServed         learn.Reason = "yield-served"
	muReasonYieldStale          learn.Reason = "yield-stale"
	muReasonYieldSiblings       learn.Reason = "yield-siblings"
	// muReasonSwapPending is a reading not written because a swap is
	// pending on the key's band (muSwapFrozen).
	muReasonSwapPending learn.Reason = "swap-pending"
	// muReasonMarginClamped is a reading clamped to its window's median, or
	// not written for want of one, past the margin toward shorter outputs
	// (marginClamp).
	muReasonMarginClamped learn.Reason = "margin-clamped"
	// muReasonWeighedFleet is a sample that counted the Ready replicas it
	// did not read (weighUnreadReplicas).
	muReasonWeighedFleet learn.Reason = "weighed-fleet"
	// muReasonMinorityExpired is a cycle whose measured mu was released:
	// written by a minority of the fleet, with no line, and stale past
	// MuChoice.LowHoldFor (minorityFigureExpired).
	muReasonMinorityExpired learn.Reason = "minority-expired"
)

// muRefusalReason is the Report's reason for a refusal throughputRefusal
// gave.
func muRefusalReason(why string) learn.Reason {
	switch why {
	case refusedNoCompletion:
		return muReasonRefusedNoCompletion
	case refusedFilling:
		return muReasonRefusedFilling
	case refusedUnderServed:
		return muReasonRefusedUnderServed
	default:
		return muReasonRefusedUnderDerived
	}
}

// muSourceSiblings is mu's source when a measurement yielded to what its
// Ready, full siblings serve (siblingYield). The published service-rate
// source reads it as measured; the learner names it apart.
const muSourceSiblings = servedBucket

// muPodReading is one admitted mu reading of a cycle: the pod, its reading,
// and k2's stamp on it (k2Unsettled), when there is one.
type muPodReading struct {
	pod     string
	reading float64
	stamp   k2Margin
	stamped bool
	// unread is how many Ready replicas the sample counted beside its
	// readers, and unreadAt the rate it counted them at
	// (weighUnreadReplicas); zero when it weighed none.
	unread   int
	unreadAt float64
}

// muVariantCycle is one variant's mu on one cycle.
type muVariantCycle struct {
	role   string
	key    string
	counts map[learn.Reason]int64
	writes int64
	// sibling is set when a replica of the variant priced from what its
	// siblings serve (siblingYield).
	sibling bool
	pods    []muPodReading
}

// muPriced is what priced a variant's mu on a cycle: its throughput key,
// the source, and the rate (the median over its own replicas,
// variantServiceRate). The throughput floor's log line names it.
type muPriced struct {
	key    string
	source string
	rate   float64
}

// muCycle is one model's mu on one cycle, by variant, and, once the cycle
// is closed, what priced each variant.
type muCycle struct {
	variants map[string]*muVariantCycle
	priced   map[string]muPriced
}

// muLogged is what learner-write last logged for a mu key, and when.
type muLogged struct {
	value  float64
	source string
	at     time.Time
}

// beginMuCycle starts mu's cycle for a model, at the top of Analyze.
func (a *SaturationAnalyzer) beginMuCycle(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.muCycles, namespace+"|"+modelID)
}

// muVariantLocked is variant's mu in model's cycle in progress. Callers
// hold a.mu.
func (a *SaturationAnalyzer) muVariantLocked(model, variant string) *muVariantCycle {
	c := a.muCycles[model]
	if c == nil {
		c = &muCycle{variants: map[string]*muVariantCycle{}}
		a.muCycles[model] = c
	}
	v := c.variants[variant]
	if v == nil {
		v = &muVariantCycle{counts: map[learn.Reason]int64{}}
		c.variants[variant] = v
	}
	return v
}

// noteMuKey records the throughput key and role a variant's mu is read and
// written under this cycle. A variant's replicas share one key: it is the
// fleet's shape, not each replica's.
func (a *SaturationAnalyzer) noteMuKey(namespace, modelID, variant, role, key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.muVariantLocked(namespace+"|"+modelID, variant)
	v.role, v.key = role, key
}

// noteMuCount counts one outcome for a variant's mu this cycle.
func (a *SaturationAnalyzer) noteMuCount(namespace, modelID, variant string, reason learn.Reason) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.muVariantLocked(namespace+"|"+modelID, variant)
	v.counts[reason]++
	if reason == muReasonYieldSiblings {
		v.sibling = true
	}
}

// noteMuWrite records what the window made of one admitted reading: a
// sample started or folded, with k2's stamp on it, or an exact repeat, which
// is use and no write (SwitchMuRepeatIsUse). unread and unreadAt are the
// replicas the sample counted beside its reader and their rate
// (weighUnreadReplicas), shown on the reading's learner-write entry.
func (a *SaturationAnalyzer) noteMuWrite(namespace, modelID, variant, pod string, reading float64, w muWrite, unread int, unreadAt float64) {
	model := namespace + "|" + modelID
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.muVariantLocked(model, variant)
	switch w {
	case muSampled, muFolded:
		reason := muReasonSampled
		if w == muFolded {
			reason = muReasonFolded
		}
		v.counts[reason]++
		v.writes++
		r := muPodReading{pod: pod, reading: reading, unread: unread, unreadAt: unreadAt}
		if k2 := a.k2Cycles[model]; k2 != nil {
			r.stamp, r.stamped = k2.margins[variant+"/"+pod]
		}
		v.pods = append(v.pods, r)
	case muRepeated:
		v.counts[muReasonRepeated]++
	}
}

// closeMuCycle ends mu's cycle for a model, after its last replica is
// priced: it files a learn.Report per variant beside k2's (LearnerReports),
// keeps what priced each variant for the throughput floor's log line
// (muPricedFor), and logs a learner-write line per key whose priced figure
// was set for the first time, moved MuWriteBand from what was last logged,
// changed source -- a flip between measured, derived, served-siblings,
// neighbour, borrowed-line and lower-bound -- or has not logged for
// LearnerHeartbeat. At most one line per key per cycle, at V(3).
func (a *SaturationAnalyzer) closeMuCycle(ctx context.Context, namespace, modelID string, caps []capacity.ReplicaCapacity) {
	now := a.now()
	model := namespace + "|" + modelID
	type line struct {
		variant  string
		v        *muVariantCycle
		p        muPriced
		last     muLogged
		logged   bool
		samples  int
		restored bool
	}
	var lines []line
	a.mu.Lock()
	c := a.muCycles[model]
	if c == nil {
		c = &muCycle{variants: map[string]*muVariantCycle{}}
		a.muCycles[model] = c
	}
	c.priced = map[string]muPriced{}
	pending := a.muForgotten[model]
	delete(a.muForgotten, model)
	variants := make([]string, 0, len(c.variants)+len(pending))
	for name := range c.variants {
		variants = append(variants, name)
	}
	for name := range pending {
		if _, ok := c.variants[name]; !ok {
			variants = append(variants, name)
		}
	}
	slices.Sort(variants)
	for _, variant := range variants {
		v := c.variants[variant]
		if v == nil {
			// Forgotten with nothing priced this cycle: filed to say so.
			a.learnerReports[model] = append(a.learnerReports[model], learn.Report{Learner: learn.Mu,
				Namespace: namespace, ModelID: modelID, Variant: variant, Forgotten: pending[variant]})
			continue
		}
		r := learn.Report{Learner: learn.Mu, Namespace: namespace, ModelID: modelID, Variant: variant,
			Role: v.role, Counts: v.counts, Writes: v.writes, Forgotten: pending[variant]}
		rate, source := variantServiceRate(variant, caps)
		if source == "" || !(rate > 0) {
			a.learnerReports[model] = append(a.learnerReports[model], r)
			continue
		}
		if source == domain.ServiceRateSourceMeasured && v.sibling {
			source = muSourceSiblings
		}
		p := muPriced{key: v.key, source: source, rate: rate}
		c.priced[variant] = p
		r.Source = learn.Source(source)
		samples, restored := 0, false
		if ra, ok := a.saturatedThroughput[v.key]; ok && ra.Len() > 0 {
			r.PricingKey, r.Samples, r.LastAdmitted = v.key, ra.Len(), ra.LastWritten()
			samples, restored = ra.Len(), ra.Restored()
		}
		a.learnerReports[model] = append(a.learnerReports[model], r)
		last, logged := a.muLogged[v.key]
		if logged && last.source == source && math.Abs(rate-last.value) <= MuWriteBand*last.value &&
			now.Sub(last.at) < LearnerHeartbeat {
			continue
		}
		lines = append(lines, line{variant: variant, v: v, p: p, last: last, logged: logged, samples: samples, restored: restored})
		a.muLogged[v.key] = muLogged{value: rate, source: source, at: now}
	}
	a.mu.Unlock()

	logger := ctrl.LoggerFrom(ctx)
	for _, l := range lines {
		pods, omitted := muWritePods(l.v.pods)
		logger.V(perReplicaLogLevel).Info("learner-write",
			"learner", learn.Mu, "modelID", modelID, "namespace", namespace, "variant", l.variant,
			"role", l.v.role, "key", l.p.key, "value", l.p.rate, "previousValue", l.last.value,
			"firstWrite", !l.logged, "source", l.p.source, "previousSource", l.last.source,
			"samples", l.samples, "restored", l.restored, "pods", pods, "podsOmitted", omitted)
	}
}

// muMapsLocked lists what mu keeps per throughput key, the maps its forget
// reaches. The prefill bound files its own windows in the same maps and is
// not mu: isMuKey leaves them to the variant's other maps. Callers hold a.mu.
func (a *SaturationAnalyzer) muMapsLocked() []learnedMap {
	return []learnedMap{
		registered("saturatedThroughput", scopeK2Key, a.saturatedThroughput),
		registered("throughputSampledAt", scopeK2Key, a.throughputSampledAt),
		registered("throughputLastRead", scopeK2Key, a.throughputLastRead),
		registered("throughputPodReads", scopeK2Key, a.throughputPodReads),
		registered("throughputMuChoice", scopeK2Key, a.throughputMuChoice),
		registered("throughputWindowArgs", scopeK2Key, a.throughputWindowArgs),
		registered("throughputStash", scopeK2Key, a.throughputStash),
		registered("throughputHeld", scopeK2Key, a.throughputHeld),
	}
}

// forgetMuLocked forgets mu's state for one variant: its windows under every
// key, what is kept beside them, and what learner-write last logged of them,
// so the next line is a first write. Each window's key is reported forgotten,
// by by, on the variant's next report. It returns how many entries it
// dropped, the logged values not counted, as the variant's forget counted
// them before mu was on learn's list. With SwitchMuForgetReported off it
// forgets nothing here. Callers hold a.mu.
func (a *SaturationAnalyzer) forgetMuLocked(namespace, modelID, variant string, by learn.By) int {
	if !a.muForgetReported {
		return 0
	}
	prefix := namespace + "|" + variant + "|" + modelID + "|"
	isMuKey := func(k string) bool { return strings.HasPrefix(k, prefix) && !isPrefillBoundKey(k) }
	var keys []string
	for key := range a.saturatedThroughput {
		if isMuKey(key) {
			keys = append(keys, key)
		}
	}
	// A window put aside under other arguments is the key's too: the key may
	// be the one last published, and nothing else would name it.
	for key := range a.throughputStash {
		if isMuKey(key) {
			keys = append(keys, stashedKey(key))
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	n := 0
	for _, m := range a.muMapsLocked() {
		n += m.dropIf(isMuKey)
	}
	for key := range a.muLogged {
		if isMuKey(key) {
			delete(a.muLogged, key)
		}
	}
	fileForgotten(a.muForgotten, namespace+"|"+modelID, variant, keys, by)
	return n
}

// muWritePods is a learner-write line's pods for mu: at most
// LearnerWriteMaxPods, lowest reading first, each with its reading, its
// time base and k2's stamp on it -- how long the replica had been settled
// and which settle test let the reading through -- and how many were left
// out.
func muWritePods(rs []muPodReading) ([]map[string]any, int) {
	rs = slices.Clone(rs)
	slices.SortStableFunc(rs, func(x, y muPodReading) int {
		if x.reading != y.reading {
			if x.reading < y.reading {
				return -1
			}
			return 1
		}
		return strings.Compare(x.pod, y.pod)
	})
	omitted := max(len(rs)-LearnerWriteMaxPods, 0)
	rs = rs[:len(rs)-omitted]
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		m := map[string]any{"pod": r.pod, "reading": r.reading, "timeBase": muTimeBase}
		if r.stamped {
			m["settledForSeconds"] = r.stamp.settledFor.Seconds()
			m["passedBy"] = r.stamp.passedBy
		}
		if r.unread > 0 {
			m["unreadReplicas"], m["unreadAt"] = r.unread, r.unreadAt
		}
		out = append(out, m)
	}
	return out, omitted
}

// muPricedFor is what priced the variant of role whose figure is nearest mu
// on model's last closed cycle: the key and source the throughput floor's
// term for the role priced from (its mu is the median over the role's
// replicas, which may span variants). Ties go to the variant first by name.
// ok is false when no variant of the role was priced.
func (a *SaturationAnalyzer) muPricedFor(namespace, modelID, role string, roleOf map[string]string, mu float64) (muPriced, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.muCycles[namespace+"|"+modelID]
	if c == nil {
		return muPriced{}, false
	}
	var best muPriced
	var bestName string
	found := false
	for variant, p := range c.priced {
		if roleOf[variant] != role {
			continue
		}
		d, bd := math.Abs(p.rate-mu), math.Abs(best.rate-mu)
		if !found || d < bd || (d == bd && variant < bestName) {
			best, bestName, found = p, variant, true
		}
	}
	return best, found
}
