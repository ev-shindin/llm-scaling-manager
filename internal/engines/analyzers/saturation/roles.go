package saturation

import (
	"math"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// P/D role predicates. Small, and together because every file above asks the
// same few questions.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

// rolesFromStates builds the variant-name -> role lookup the per-role helpers
// take, from the variant states an Analyze call carries.
func rolesFromStates(states []domain.VariantReplicaState) map[string]string {
	roles := make(map[string]string, len(states))
	for _, vs := range states {
		roles[vs.VariantName] = vs.Role
	}
	return roles
}

// generatesOutput reports whether a replica is one whose completions carry
// the workload's real output length and service time: every role but prefill.
// A variant with no recorded role is domain.RoleBoth, which generates.
func generatesOutput(rm domain.ReplicaMetrics, rolesByVariant map[string]string) bool {
	return domain.CanonicalRole(rolesByVariant[rm.VariantName]) != domain.RolePrefill
}

// stableAccelerator returns the accelerator to key history under, preferring the
// last one that resolved for this variant over an unresolved reading.
//
// Capacity is a property of the hardware, so it belongs in the key; but "the pods
// currently disagree about it" is not a different accelerator, and treating it as
// one is what makes the fleet oscillate. A resolved reading always wins and is
// remembered; an unresolved one falls back to what was remembered, and only when
// nothing was ever resolved does the unresolved value itself get used, which
// keeps a never-resolvable variant behaving exactly as it does today.
func (a *SaturationAnalyzer) stableAccelerator(namespace, variantName, accelerator string) string {
	key := namespace + "/" + variantName
	a.mu.Lock()
	defer a.mu.Unlock()
	if constants.IsAcceleratorResolved(accelerator) {
		a.lastAccelerator[key] = acceleratorMemo{name: accelerator, lastUsed: time.Now()}
		return accelerator
	}
	if last, ok := a.lastAccelerator[key]; ok {
		// Touched on READ as well as on write: the memo should live as long as the
		// variant is being analysed, not as long as its accelerator keeps
		// resolving -- a variant that stops resolving is exactly the one this
		// exists for. NoteVariantsPresent touches it too, so a variant parked at
		// zero keeps it while it is parked; a variant that is gone goes quiet on
		// all three and EvictStaleHistory ages it out. Its k2 and mu windows
		// age on their own clock, so the two need not go together.
		last.lastUsed = time.Now()
		a.lastAccelerator[key] = last
		return last.name
	}
	return accelerator
}

// StableAcceleratorFor reports the accelerator a variant's learned state is
// keyed under, WITHOUT recording or touching anything.
//
// For the metric that explains the key. wva_engine_config was publishing the
// raw reading while the key used the stabilised one, so on a heterogeneous
// fleet -- the only fleet where the difference exists -- the label and the key
// disagreed and the series churned between two label sets every cycle.
//
// Read-only on purpose, and that is the whole reason this is not just a call to
// stableAccelerator: that one writes the memo on a resolved reading and touches
// lastUsed on an unresolved one. lastUsed is the eviction clock, so publishing
// through it would make the memo live as long as the metric publishes rather
// than as long as the variant is analysed -- which is precisely the coupling
// the per-cycle sweep was changed to avoid.
func (a *SaturationAnalyzer) StableAcceleratorFor(namespace, variantName, accelerator string) string {
	if constants.IsAcceleratorResolved(accelerator) {
		return accelerator
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.lastAccelerator[namespace+"/"+variantName]; ok {
		return last.name
	}
	return accelerator
}

// idleKvUsage (tuning.idleKvUsage) is the busiest-rank KV utilization below
// which a Ready replica with nothing waiting reads idle on one row. It is the
// load-imbalance diagnostic's number, read from the same policy key
// (loadImbalance.idleKvUsage, default balance.IdleKvUsage = 0.30), so the two
// cannot disagree. One row is not enough for the analyzer to act on;
// settledIdleDecode says what more it needs.

// replicaIdle reports whether rm reads idle on this cycle's row: Ready, its
// busiest engine rank under idleKvUsage and nothing waiting.
//
// Both figures are one-minute peaks (the collector's max_over_time), but a
// replica Ready for less than that minute has a peak over the seconds since it
// came up, so one idle row is not an idle minute (settledIdleDecode). The KV is the
// larger of the utilization the row reports and its busiest-rank tokens over
// its cache: in production the two are the same number (TokensInUse is
// KvCacheUsage times the cache), and where a row carries only one of them the
// other cannot make it read idle. A reading that is not a finite fraction is
// zero, as the balance package reads it.
func (t tuning) replicaIdle(rm domain.ReplicaMetrics) bool {
	if !rm.Ready || rm.QueueLength > 0 {
		return false
	}
	kv := rm.BusiestRankKvUsage()
	if math.IsNaN(kv) || math.IsInf(kv, 0) || kv < 0 {
		kv = 0
	}
	if rm.TotalKvCapacityTokens > 0 {
		kv = max(kv, float64(rm.BusiestRankTokens())/float64(rm.TotalKvCapacityTokens))
	}
	return kv < t.idleKvUsage
}

// replicaRouting is what the cycle's routing says about how one replica's
// resident KV is charged. Built per row in priceReplicas from what newCycle
// decided over every row, since a replica cannot tell from its own row
// whether a sibling is busy. Its waiting queue is charged in full, whatever
// its siblings are doing (idle_room.go says why).
type replicaRouting struct {
	// kvFromInstant is set when the replica's resident KV is charged at its
	// instant reading rather than its one-minute peak (residentCharge):
	// it is Ready, idle NOW (KvUsageInstant under idleKvUsage) while a
	// sibling of its role is busy now. Its peak is load that has moved to
	// that sibling, and the sibling's own reading already counts it.
	kvFromInstant bool
}

// instantKv is rm's instantaneous KV utilization (KvUsageInstant) and whether
// it is a usable fraction: read from a series (KvUsageInstantKnown), not the
// zero a missing one leaves, and within [0, 1].
func instantKv(rm domain.ReplicaMetrics) (float64, bool) {
	k := rm.KvUsageInstant
	return k, rm.KvUsageInstantKnown && !math.IsNaN(k) && k >= 0 && k <= 1
}

// rolesBusyNow is the set of roles (domain.CanonicalRole) with a Ready replica
// whose instantaneous KV is at or over idleKvUsage this cycle.
//
// The collector's KV and queue rows are one-minute peaks, which is what the
// saturation tests want, and summed per pod they over-read demand when the
// router moves its hot pod within the minute: four pods each full for 15 s
// read as four full pods. A role busy now has its load visible on the
// replicas that carry it now, so a sibling that is idle now (instant KV under
// idleKvUsage) is charged at its instant reading instead of its peak
// (replicaRouting.kvFromInstant). A role with no replica busy now -- light
// everywhere, or a load that has just stopped -- keeps every peak, as does a
// fleet whose instant reading is missing, which has no replica busy now
// either.
func (t tuning) rolesBusyNow(metrics []domain.ReplicaMetrics, rolesByVariant map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, rm := range metrics {
		if k, ok := instantKv(rm); rm.Ready && ok && k >= t.idleKvUsage {
			out[domain.CanonicalRole(rolesByVariant[rm.VariantName])] = true
		}
	}
	return out
}

// kvFromInstant reports whether rm's resident KV is charged at its instant
// reading this cycle (replicaRouting.kvFromInstant): Ready, nothing waiting,
// an instant reading on record and idle by it, its role busy now.
//
// Nothing waiting, because only a replica that has handed its load away has
// a peak that is someone else's. One with requests queued has not: its
// instant KV dips between prefill batches -- a P/D prefill replica's KV is
// transient until decode pulls it -- and on preemption, and charged at the
// dip a uniformly saturated, queued fleet ordered nothing (in review, every
// pod at peak 0.95 queued 20 with instants 0.9/0.2/0.2/0.2 implied 4.00
// replicas on four, against 5.84 with all four at 0.9; P/D prefill at
// 0.4/0.1/0.1/0.1 read a scale-down). A missing series is not a reading of
// zero: the peak stands.
func (t tuning) kvFromInstant(rm domain.ReplicaMetrics, busyNow bool) bool {
	k, ok := instantKv(rm)
	return busyNow && rm.Ready && rm.QueueLength == 0 && ok && k < t.idleKvUsage
}

// roleSaturated reports whether any replica of the given role is full and
// queued this cycle: its local queue at or over the threshold, as computeK2
// admits a P1-obs reading on, AND its resident KV at or over its k1 (the
// cache times kvCacheThreshold) and within the cache's physical ceiling (a
// reading above it is a scrape artifact there, and no more a saturation
// here). The queue alone would not do: a decode request waiting for its
// remote KV sits in vLLM's waiting count, so a slow transfer keeps a queue
// on a decode that is admitting fine. A row with no cache size cannot be
// judged and does not count. Bridges count: a warm-pool Pod lent to decode
// that is full and queued, and Ready, is decode saturated as much as one of
// its own replicas is -- prefill's KV is pulled by whichever decode Pod is
// routed the request. A Pod that is not Ready does not: nothing is routed to
// it, so its queue and resident KV are left over or frozen (the rows
// computeK2 does not record from), and counting it would gate prefill's
// learning and hold prefill's demand for as long as the Pod sat unready.
// Both tests read the replica's busiest engine rank (BusiestRankQueue,
// kvFull), as computeK2 does: the thresholds are one engine's.
//
// One under-loaded decode replica beside a full one does not clear it. The
// back-pressure that makes a prefill reading decode's is the full replica's:
// KV prefilled for it waits on it, whatever its siblings are doing, and a
// prefill k2 learned from that wait is never corrected (k2 is never raised).
// A decode sibling idle for IdleSettledFor relaxes only the hold on
// prefill's DEMAND (applyFloorAndHolds), never what prefill may learn.
func roleSaturated(metrics []domain.ReplicaMetrics, rolesByVariant map[string]string, role string, queueThreshold, kvCacheThreshold float64) bool {
	for _, rm := range metrics {
		if !rm.Ready || domain.CanonicalRole(rolesByVariant[rm.VariantName]) != role {
			continue
		}
		if rm.BusiestRankQueue() >= int(queueThreshold) && kvFull(rm, kvCacheThreshold) {
			return true
		}
	}
	return false
}

// kvFull reports whether rm's busiest engine rank is full: its resident KV,
// scaled to the replica (domain.ReplicaMetrics.BusiestRankTokens), at or
// over the replica's k1 (the cache times kvCacheThreshold) and within the
// cache's physical ceiling. A reading above the ceiling is a scrape artifact,
// and a row with no cache size or no KV in use cannot be judged. A
// kvCacheThreshold at or below zero puts k1 at zero, so any KV in use counts
// as full.
//
// The busiest rank, not the mean: kvCacheThreshold is a fraction of one
// engine's cache, and a pod with one full rank among idle ones has an engine
// that cannot allocate, whatever its mean reads. With one rank it is the
// replica's own KV.
func kvFull(rm domain.ReplicaMetrics, kvCacheThreshold float64) bool {
	tokens := rm.BusiestRankTokens()
	return tokens > 0 && rm.TotalKvCapacityTokens > 0 &&
		tokens <= rm.TotalKvCapacityTokens &&
		tokens >= memoryBound(rm.TotalKvCapacityTokens, kvCacheThreshold)
}

// busiestQueueLoadByVariant is, per decode or aggregated variant, the
// deepest waiting queue on any one engine of a Ready replica as a fraction of
// queueThreshold (domain.AnalyzerResult.VariantBusiestQueueLoad). Bridges
// count, as in roleSaturated. A replica not Ready is not routed to, so its
// queue says nothing about routing. Prefill is left out: its queue is
// decode's back-pressure or the router's choice -- measured, 439 requests
// on one prefill pod while three sat idle -- and keeping a replica relieves
// neither. A decode row counts only while its KV is full (kvFull), since its
// waiting count includes requests waiting for their remote KV; below that
// it reads zero. A variant no role is known for, and every variant when
// there is no positive threshold to measure against, is left out.
func busiestQueueLoadByVariant(metrics []domain.ReplicaMetrics, rolesByVariant map[string]string, queueThreshold, kvCacheThreshold float64) map[string]float64 {
	if !(queueThreshold > 0) {
		return nil
	}
	out := map[string]float64{}
	for _, rm := range metrics {
		role, ok := rolesByVariant[rm.VariantName]
		if !ok || !rm.Ready {
			continue
		}
		v := rm.VariantName
		switch domain.CanonicalRole(role) {
		case domain.RolePrefill:
			continue
		case domain.RoleDecode:
			if !kvFull(rm, kvCacheThreshold) {
				out[v] = max(out[v], 0)
				continue
			}
		}
		// The busiest rank's queue: it is what one engine of the pod holds,
		// as queueThreshold is one engine's. The pod's sum would read eight
		// ranks with one request each as a queue of eight.
		out[v] = max(out[v], float64(max(rm.BusiestRankQueue(), 0))/queueThreshold)
	}
	return out
}
