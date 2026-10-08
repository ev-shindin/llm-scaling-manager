package saturation

import (
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// How long a replica of this variant takes to become Ready, which the demand
// floor projects the arriving queue forward over.
//
// It has to be a measurement. Run T measured 67 s for four pods and 82 s for
// five while the controller ordered one replica per cycle for a minute and the
// queue reached 191 -- a projection over a guessed dead time is wrong by whatever
// the guess was, multiplied by the arrival rate.
//
// Per VARIANT, not per model: it is an image, a set of engine flags and a node,
// and two variants of one model routinely differ. Keyed exactly like the ITL
// windows so the two agree about what a variant is.

const (
	// DefaultReplicaStartSeconds is the fallback for a variant with no
	// measurement and no seed on its ScaledObject.
	//
	// 70 s, the middle of run T's measured 67-82 s for a 0.6B model on an H200.
	// Deliberately not generous: too long over-states the backlog that will
	// accumulate and over-orders. A GLM-5.2 cold start with a cold JIT cache is
	// nearer 500 s, which is why registry.ReplicaStartSecondsKey exists rather
	// than this number being asked to serve both.
	DefaultReplicaStartSeconds = 70.0

	// startSecondsAlpha weights a new measurement against the running estimate.
	//
	// 0.3 -- slow enough that one unlucky start does not move it far, fast
	// enough that a real change of image or node class is absorbed within a
	// handful of starts. Run T's own spread, 67 then 82, gives 71.5.
	startSecondsAlpha = 0.3

	// maxStartOutlierFactor bounds how far one sample may exceed the running
	// estimate before it is treated as something other than a start.
	//
	// The Ready condition's LastTransitionTime is re-stamped on EVERY
	// False->True transition, not only the first, so a readiness blip 45 minutes
	// into a pod's life reports 2700 s as its "start". Folded in at alpha 0.3
	// that moves a 67 s estimate to about 857 s, and the projection then
	// over-orders by an order of magnitude for the rest of the run. Nothing in
	// the Pod object distinguishes a first transition from a later one, so the
	// guard is on the value instead.
	maxStartOutlierFactor = 3.0

	// maxConsecutiveStartOutliers is how many rejections in a row are taken as
	// the world having changed rather than the samples being wrong.
	//
	// Without it a variant redeployed onto genuinely slower hardware would have
	// every sample rejected forever against an estimate that no longer describes
	// it -- the guard above would become the bug it was added to prevent.
	maxConsecutiveStartOutliers = 3
)

// noteReplicaStart folds this cycle's observed start times into the per-variant
// estimate, publishes both the observation and the estimate, and forgets the
// replicas that have gone.
//
// Warm-pool bridges are skipped. A bridged Pod reaching Ready in a second is a
// wake, not a start, and says nothing about how long this variant's own replicas
// take -- the same exclusion noteITL makes for the fit and floor.Estimate makes
// for the price.
//
// An observation is counted ONCE per replica. Every cycle sees the same Ready Pod
// reporting the same StartSeconds, so folding it in repeatedly would drive the
// estimate to whatever the longest-lived replica measured and make the histogram
// a count of cycles rather than of starts.
//
// The bookkeeping for that is bounded HERE, by forgetting Pods this variant no
// longer reports. EvictStaleHistory sweeps it too, as a backstop for a variant
// that disappears entirely -- it DOES have a caller on the reconcile path now
// (steadystate.evictStaleLearnedState, once per cycle), which is why the
// already-counted branch below re-stamps rather than leaving an entry to
// expire under a live Pod. A set keyed per Pod is the fastest-growing state on this
// struct, so it cannot be left to a sweep that does not run.
func (a *SaturationAnalyzer) noteReplicaStart(
	key, namespace, variantName string,
	replicas []domain.ReplicaMetrics,
	logger logr.Logger,
) {
	type outlier struct {
		pod     string
		sample  float64
		against float64
	}
	var rejected []outlier
	observed := 0
	present := make(map[string]struct{}, len(replicas))

	a.mu.Lock()
	// This runs for every role, so it is what bounds the start estimate of a
	// prefill or RoleBoth variant: noteITL stamps decode keys only, and the
	// sweep that used to reach these maps through the ITL window map could
	// never visit a key that has no window.
	a.noteVariantSeen(key, a.now())
	for _, rm := range replicas {
		if rm.VariantName != variantName || rm.FromWarmPool || rm.PodName == "" {
			continue
		}
		// Qualified by the variant's window key AND the namespace. A bare Pod
		// name is not unique: a StatefulSet or LeaderWorkerSet reuses
		// deterministic names across incarnations, and the same release in two
		// namespaces gives two Pods the same name -- either of which would have
		// one variant's measurement suppress another's silently. Including the
		// window key also means a variant whose key changes (its accelerator
		// resolving differently, its GPU count changing) re-measures from the
		// Pods it can already see instead of falling back to the seed for the
		// life of those Pods.
		seen := key + "|" + rm.Namespace + "|" + rm.PodName
		present[seen] = struct{}{}

		if !(rm.StartSeconds > 0) {
			continue
		}
		if _, counted := a.startSeenPods[seen]; counted {
			// Re-stamp. The entry is what says "this Pod's start is already in
			// the estimate", and it used to be written once and never
			// refreshed -- which was safe only while evictStartSeenPods had no
			// caller. With the per-cycle sweep wired up, a Pod Ready for
			// longer than the timeout lost this record and its start was
			// folded into the EWMA a second time, then once per timeout for
			// the rest of its life. That breaks the invariant above: the
			// estimate would drift toward whatever the longest-lived replica
			// measured, and wva_replica_start_seconds would count cycles
			// rather than starts.
			//
			// Refreshing keeps the sweep for the case its own comment names --
			// a variant that disappears entirely and is never reported again
			// -- while a Pod still being reported never ages out.
			a.startSeenPods[seen] = a.now()
			continue
		}
		cur, measured := a.startSeconds[key]
		if measured && cur > 0 && rm.StartSeconds > maxStartOutlierFactor*cur &&
			a.startOutliers[key] < maxConsecutiveStartOutliers {
			a.startOutliers[key]++
			rejected = append(rejected, outlier{rm.PodName, rm.StartSeconds, cur})
			// NOT recorded as seen: if the next cycle still reports it, it is
			// counted toward the consecutive run above rather than forgotten.
			continue
		}
		a.startOutliers[key] = 0
		a.startSeenPods[seen] = a.now()
		observed++

		if measured && cur > 0 {
			a.startSeconds[key] = (1-startSecondsAlpha)*cur + startSecondsAlpha*rm.StartSeconds
		} else {
			// The first measurement REPLACES the seed rather than being averaged
			// with it. The seed is a guess and the measurement is not, so giving
			// the guess most of the weight of the first real figure would keep a
			// wrong number in force for several starts.
			a.startSeconds[key] = rm.StartSeconds
		}
		metrics.ObserveReplicaStartSeconds(namespace, variantName, rm.StartSeconds)
	}

	// Forget this variant's Pods that no longer report. Scoped by the key
	// prefix so one variant's cycle cannot drop another's bookkeeping.
	prefix := key + "|"
	for k := range a.startSeenPods {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if _, still := present[k]; !still {
			delete(a.startSeenPods, k)
		}
	}
	est, measured := a.startSeconds[key]
	a.mu.Unlock()

	if !measured || !(est > 0) {
		est = DefaultReplicaStartSeconds
	}
	metrics.SetReplicaStartSecondsEstimate(namespace, variantName, est, measured)
	for _, o := range rejected {
		logger.V(logging.DEFAULT).Info("replica-start-outlier-rejected",
			"variant", variantName, "pod", o.pod,
			"sampleSeconds", o.sample, "estimateSeconds", o.against,
			"factor", maxStartOutlierFactor,
			"reason", "the Ready condition re-transitions, so a probe blip reports the pod's age")
	}
	if observed > 0 {
		logger.V(logging.DEFAULT).Info("replica-start-measured",
			"variant", variantName, "observations", observed,
			"estimateSeconds", est, "source", startSource(measured))
	}
}

// startSecondsByRole is how long one replica of each role takes to become Ready,
// from the per-variant estimates.
//
// The MINIMUM over a role's variants, not the maximum or the mean: it is the
// first relief to arrive, and the projection is what gets ordered on. Taking the
// slowest would project more arrivals and order more replicas on a role whose
// variants differ -- the wrong direction to err, given the failure this whole
// file is chasing is a fleet that over-orders and cannot come back down.
//
// A role with no measured variant is absent from the result, which leaves the
// floor on its observed backlog for that role.
func (a *SaturationAnalyzer) startSecondsByRole(
	input domain.AnalyzerInput, roleOf map[string]string,
) map[string]float64 {
	accel := make(map[string]string, len(input.VariantStates))
	gpus := make(map[string]int, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		accel[vs.VariantName] = vs.AcceleratorName
		gpus[vs.VariantName] = vs.GPUsPerReplica
	}

	// Keys FIRST, outside the lock. itlWindowKey reaches stableAccelerator,
	// which takes a.mu -- and sync.Mutex is not reentrant, so resolving a key
	// while holding it deadlocks the whole analyzer. noteITL documents the same
	// hazard where it drops the lock before logging; this is that hazard.
	keys := make(map[string]string, len(roleOf))
	for variant := range roleOf {
		keys[variant] = a.itlWindowKey(input.Namespace, input.ModelID, variant,
			accel[variant], gpus[variant])
	}

	out := make(map[string]float64, 2)
	a.mu.Lock()
	defer a.mu.Unlock()
	for variant, role := range roleOf {
		est, ok := a.startSeconds[keys[variant]]
		if !ok || !(est > 0) {
			continue
		}
		if cur, seen := out[role]; !seen || est < cur {
			out[role] = est
		}
	}
	return out
}

// startSource labels the estimate for the log and the metric, so a run says
// whether it sized against a measurement or a guess.
func startSource(measured bool) string {
	if measured {
		return "measured"
	}
	return "seed"
}

// evictStartSeenPods drops bookkeeping older than the timeout.
//
// A backstop only: noteReplicaStart already forgets a Pod the cycle it stops
// reporting, which is what actually bounds the map. This exists for the one
// case that cannot reach -- a variant which disappears entirely, never
// reported again, so never pruned by its own cycle. EvictStaleHistory now has
// a caller (steadystate.evictStaleLearnedState), so this runs every cycle, and
// noteReplicaStart re-stamps a Pod it still sees precisely so that a live Pod
// is never swept out from under its own "already counted" record.
func (a *SaturationAnalyzer) evictStartSeenPods(now time.Time, timeout time.Duration) int {
	dropped := 0
	for pod, seen := range a.startSeenPods {
		if now.Sub(seen) > timeout {
			delete(a.startSeenPods, pod)
			dropped++
		}
	}
	return dropped
}
