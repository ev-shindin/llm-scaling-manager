package saturation

import (
	"strings"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// A lower bound on prefill's service rate, for the fleet whose prefill never
// saturates on its own account (issue #144).
//
// The throughput floor sizes every role at (lambda + backlog/drain) / mu, with
// lambda the router's arrival rate. That is already "prompt tokens arriving
// per second over prompt tokens one replica computes per second" for prefill
// (saturatedCompletionRate prices prefill's mu from computed tokens at the
// effective prompt length). What prefill lacks is the mu: its window is written
// only when a prefill replica saturates while decode does NOT, and on a P/D
// fleet prefill's queue follows decode's back-pressure, so computeK2 discards
// those readings (k2ReasonObsDownstream). Measured on two runs: P4-k1 on every
// prefill cycle, saturatedThroughput 0, and the floor priced decode alone while
// prefill queued 820 deep. With no mu, prefill scales only once its queue and
// occupancy say so.
//
// A discarded reading is still evidence, in one direction. A prefill replica
// whose queue is over the threshold is computing as fast as something lets it
// -- its own compute, or decode's admission -- so the computed-token rate it
// shows then is AT MOST what it can sustain. That is a lower bound on mu, and
// the max of lower bounds is the tightest one; so this window reads its MAX,
// where the measured window reads a median, and a lower reading never
// displaces a higher one (notePrefillBound). The max is over minutes, not
// over pods: one minute's readings fold into one sample as mu's do, at the
// mean across pods, since a fleet serves the sum of its pods' rates and the
// fastest pod's reading over-states what each one serves.
//
// The window holds computed TOKENS per second, not requests per second, and
// is converted at the prompt length being served when it is read. Its key's
// input bucket spans a factor of two, and the top one ("huge", 6000 tokens
// and up) has no upper end, so readings in one window come from different
// prompt lengths. In requests per second, the max that never yields kept the
// shortest prompt's figure, and priced a 12,000-token prompt at what the
// replica did for 6000-token ones: twice its rate, half the prefill. A
// replica's token rate moves far less with prompt length.
//
// The error goes one way. The floor divides by mu, so a bound under-reading
// the true rate by a factor f over-orders prefill by f. What bounds it:
//
//   - an own reading always wins: the bound prices a replica only when its
//     own window, under the arguments it runs, holds nothing;
//   - MinThroughputSamplesToOrder spaced readings before it may grow prefill;
//     fewer only hold (heldWhy lower-bound);
//   - floor.LowerBoundMaxGrowth caps the floor at twice the READY prefill
//     supply, so one replica start can at most double the fleet;
//   - the floor only raises demand, never lowers it; and while decode is
//     saturated the prefill hold (holdPrefillDemand) still clamps prefill, so
//     the bound cannot order prefill for decode's shortfall -- the 33
//     GPU-minutes computeK2's comment records came of exactly that;
//   - it is off unless enablePrefillThroughputBound turns it on; off,
//     nothing is recorded or read.
//
// Without a bound on record nothing changes: no window, no term, the floor
// prices prefill as it did.

// prefillBoundRole is the role field of a lower bound's window key. The bound
// lives in the same saturatedThroughput map as the measured windows, so it is
// persisted, aged, swept, put aside on an argument change and forgotten with
// the variant by the code that already does those things for them; a distinct
// role field keeps the two from ever being read for one another, including by
// the neighbour-bucket borrow, which never crosses the role field.
const prefillBoundRole = "prefill-bound"

// lowerBoundBucket is the bucket label a replica's throughput carries in the
// replica-capacity-decision line when the lower bound priced it.
const lowerBoundBucket = "lower-bound"

// prefillBoundKey is the lower bound's window key for a prefill throughput
// key: the same key with the role field replaced. The throughput key has nine
// |-separated fields (keys.go) and the role is the sixth; no field contains
// "|". A key of any other shape has no bound.
func prefillBoundKey(throughputKey string) string {
	f := strings.Split(throughputKey, "|")
	if len(f) != 9 || f[5] != domain.RolePrefill {
		return ""
	}
	f[5] = prefillBoundRole
	return strings.Join(f, "|")
}

// isPrefillBoundKey reports whether key is a lower bound's window key, one
// prefillBoundKey made.
func isPrefillBoundKey(key string) bool {
	f := strings.Split(key, "|")
	return len(f) == 9 && f[5] == prefillBoundRole
}

// prefillBoundReading is what the bound offers one replica: a rate, how many
// spaced readings the window holds, and whether it is usable at all.
type prefillBoundReading struct {
	rate    float64
	samples int
}

// notePrefillBound records a prefill replica's lower bound when its queue is
// saturated, and returns the bound on record for its key. Called for every
// prefill replica the analyzer prices; a no-op returning nothing when the
// model has not turned the bound on, the role is not prefill, or the key has no
// bound shape.
//
// Recorded only from a SERVING row -- Ready, the variant's own, not mid-
// rollout -- for the reasons admissionOf gives for every learned window, and
// only while the busiest rank's queue is over the threshold with tokens
// resident: the condition computeK2 calls a saturated reading. Readings the
// measured window also takes are recorded here too; it costs nothing, since
// the measured window wins wherever it exists.
//
// The window holds computed tokens/s (see the file header); ileff, the
// effective prompt length the throughput key was built with this cycle,
// converts it on the way out, so the bound returned is in the same requests/s,
// at the shape being served, as the measured window it stands in for.
func (a *SaturationAnalyzer) notePrefillBound(rm domain.ReplicaMetrics, cfg *config.ScalingPolicy,
	role, throughputKey, args string, ileff float64, adm admission, logger logr.Logger) prefillBoundReading {
	if !cfg.EnablePrefillThroughputBound || domain.CanonicalRole(role) != domain.RolePrefill {
		return prefillBoundReading{}
	}
	key := prefillBoundKey(throughputKey)
	if key == "" {
		return prefillBoundReading{}
	}
	a.mu.Lock()
	a.throughputWindowUnderLocked(key, args)
	a.mu.Unlock()
	saturated := rm.BusiestRankQueue() >= int(cfg.QueueLengthThreshold) && rm.BusiestRankTokens() > 0
	if saturated && adm.serving() && rm.PrefillComputedTokenRate > 0 && ileff > 0 {
		tokens := rm.PrefillComputedTokenRate
		a.mu.Lock()
		var prior float64
		if ra, ok := a.saturatedThroughput[key]; ok {
			prior = ra.Max()
		}
		sampledAt, sampled := a.throughputSampledAt[key]
		a.mu.Unlock()
		// Under the pod's name, so the readings of one minute fold as mu's
		// do: a max over one pod's re-reads and the mean across pods
		// (recordSaturatedThroughputFrom). Under one name for every pod, the
		// fold kept the fastest pod's reading, and one pod read at twice its
		// rate -- a doubled series -- or one fast pod set the whole fleet's
		// bound (#395).
		a.recordSaturatedThroughputFrom(key, rm.PodName, tokens)
		a.mu.Lock()
		if ra, ok := a.saturatedThroughput[key]; ok {
			if args != "" {
				a.throughputWindowArgs[key] = args
			}
			// The bound only rises. A reading behind a full decode is
			// decode's admission shared over the prefill fleet, so one taken
			// after the bound grew that fleet is lower by the growth, and
			// letting it evict the higher reading (the ring holds ten, and a
			// write after EpisodeGap starts a new window) re-priced prefill
			// on the smaller figure: every decode episode at the same load
			// grew prefill again, by at least 1/scaleUp. Each reading is a
			// lower bound on the same rate, so the highest one stays true.
			//
			// It rises against the minutes BEFORE this one: the window's max
			// when this minute's sample started, kept with the sample. Judged
			// against the max now, which holds the sample itself, it restored
			// the sample to its first pod's reading and undid the fold.
			reads := a.throughputPodReads[key]
			if at, ok := a.throughputSampledAt[key]; ok && reads.at.Equal(at) && (!sampled || !at.Equal(sampledAt)) {
				reads.floor = prior
				a.throughputPodReads[key] = reads
			}
			if ra.Last() < reads.floor {
				ra.RaiseLastAt(reads.floor, a.now())
			}
		}
		a.mu.Unlock()
		logger.V(logging.DEFAULT).Info("prefill-throughput-bound-reading",
			"variant", rm.VariantName, "pod", rm.PodName, "key", key,
			"computedTokenRate", rm.PrefillComputedTokenRate, "effectiveInputTokens", ileff,
			"reading", tokens/ileff, "queueLength", rm.BusiestRankQueue(),
			"queueThreshold", cfg.QueueLengthThreshold)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[key]
	if !ok || ra.Len() == 0 || !(ileff > 0) {
		return prefillBoundReading{}
	}
	// Read counts as use, as it does for the measured window: a bound the
	// floor prices from every cycle must not age out for want of a new
	// saturation, which is the one thing a fleet the bound is working for
	// stops producing.
	ra.TouchAt(a.now())
	return prefillBoundReading{rate: ra.Max() / ileff, samples: ra.Len()}
}
