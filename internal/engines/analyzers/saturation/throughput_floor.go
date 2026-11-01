package saturation

import (
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
)

// The throughput floor: the demand the offered load implies once a replica's
// saturated completion rate (mu) is known, (lambda + backlog / drain) / mu
// replicas per role. The model and its arithmetic are signals/floor (see
// that package's header for the measurements behind it); this file is what
// the saturation analyzer keeps around it -- the window of saturated
// readings per history key, the borrow from a neighbouring bucket, and the
// application of the floor to the demand occupancy measured.

// recordSaturatedThroughput folds one saturated completion-rate reading into
// the window for key. A non-positive rate is not a reading.
//
// The window keeps a MAX, not a mean, and the asymmetry is the point. A
// replica's completion rate while saturated under-reads its capacity while
// the replica is full: in the first minute after it fills, the requests
// completing are the few that were admitted first (a 1m rate on a replica
// that has been full for 20s counts a third of a minute's completions);
// under KV pressure preemption and recompute drop it further. A mean of
// under-reads is an under-read, and the floor divides by it, so an
// under-read mu orders replicas that are not needed. The max of the window
// is the best estimate of what the replica sustains, and a high reading
// errs the way this file already accepts: a floor that is too LOW holds
// back, where occupancy still carries a real shortfall. The exception, and
// it is open, is the drain at the end of an episode -- see below.
//
// A reading counts as a SAMPLE of its own only when it lands
// ThroughputSampleSpacing after the last one that counted and differs from
// it. The rate is rate(...[RequestRateWindow]) evaluated afresh every 15 s
// cycle, so the next cycle reads mostly the same window -- at 30 s scrapes,
// exactly the same two samples, and the same value to the digit -- and one
// saturated moment shows up on several consecutive cycles. Counted each
// time, two cycles cleared MinThroughputSamplesToOrder on the first
// window's under-read: the guard was dead at that scrape interval. Value
// equality alone is not the test either: the rate is an integer count of
// completions over the scrape interval, so two different windows agree to
// the digit a few percent of the time, and two replicas saturating in one
// cycle read two values from the same moment. A minute apart, two readings
// share no samples.
//
// A reading inside the spacing belongs to the last sample's window, and is
// folded into it: the sample becomes the max of what that replica
// read -- the max the window kept when every cycle was added, no
// more. Dropping in-spacing readings instead lost the highest reading of
// an episode exactly when no next window would come, and on two replayed
// passes left mu 10-12 % low for the rest of the pass, on one of them
// ordering a fourth replica the run never needed. The fold is a max over
// re-reads of one replica and the mean across replicas
// (recordSaturatedThroughputFrom): two replicas saturating in one cycle read
// as one moment at the mean of their two readings.
//
// What the max is worth is another matter. A saturated rate under-reads
// while the replica is full (file header),
// and fails in the last minute of an episode that ends by a replica
// landing: the queue gate is a one-minute max and stays up while the rate
// is fresh, and the fresh rate is the batch draining -- sequences admitted
// together finish together, and no new ones come. Measured on the cold
// pass of 2026-09-19 (second): 6.67 req/s on the last saturated cycle,
// 200 completions in 30 s, with token throughput below the plateau the
// replica held while full (~5.0 req/s sustained), and the window carried
// 6.67 for the rest of the phase. The max window has always taken these;
// nothing in it lowers a reading once taken. The fix is upstream of this
// function -- a reading gated on an instantaneous queue, or a mu priced
// from the generation-token rate over the bucket's output length, which
// does not burst on a drain -- and is open.
//
// A reading equal to the last one given, at or past the spacing, is the
// same scrape pair read again across the boundary (a re-read of the
// previous pair when the next scrape has not landed), or a genuine repeat,
// and starts no sample; that costs a cycle or two. Compared to the last
// READING, not to the sample, which the fold may have raised above it. Such
// a repeat is use, not a write (SwitchMuRepeatIsUse): it touches the
// window's use clock and leaves its write clock, so a frozen series does
// not keep an episode alive. The prefill bound keeps the old rule, a write.
//
// Measured on the shape-swap trace: every saturated rate logged over four
// passes is N/30 (103/30 = 3.43, 110/30 = 3.67, 165/30 = 5.5 ...) but a
// fresh replica's first, extrapolated window, and on the first cold pass
// of 2026-09-19 one decode replica's 3.43 stood on four consecutive cycles
// -- two scrape pairs -- and let the floor order on it. The 35 minutes of
// a third replica that followed were the under-read itself, which
// occupancy would have ordered 30 s later and the floor then held either
// way; the sample count is what this fixes, not that.
//
// Same window size and staleness rule as k2 history, and pruned beside it in
// EvictStaleHistory.
func (a *SaturationAnalyzer) recordSaturatedThroughput(key string, rate float64) {
	a.recordSaturatedThroughputFrom(key, "", rate)
}

// muWrite is what recordSaturatedThroughputFrom made of one reading.
type muWrite int

const (
	// muNotWritten: not a reading (non-positive or +Inf).
	muNotWritten muWrite = iota
	// muSampled: the reading started a sample of its own.
	muSampled
	// muFolded: the reading was folded into the sample being read.
	muFolded
	// muRepeated: an exact repeat of the last reading past the spacing,
	// which starts no sample (SwitchMuRepeatIsUse says what it touches).
	muRepeated
)

// recordSaturatedThroughputFrom is recordSaturatedThroughput for a reading
// from one named replica (pod).
//
// The window is per bucket, not per replica, and every saturated replica of
// the variant reads into it each cycle. A reading inside the spacing is folded
// into the last sample, and the fold is a max over re-reads of ONE replica's
// rate window -- the same moment read again, whose highest reading is the
// best one -- and the MEAN across replicas, whose readings are independent.
// Folded by a max across replicas too, a sample was the hottest of N
// readings and the window's median an order statistic whose expectation
// rose with N: measured with 6 % reading noise on a true 6.0 req/s, +5.5 % at
// one saturated replica and +13.6 % at sixteen. An over-stated mu orders too
// few replicas. Per replica, the cross-replica part is gone and what is left
// is the one-replica re-read max the window always kept.
//
// The mean, not the median: a fleet serves the sum of its replicas' rates,
// so what one replica serves on average is their mean. A lower median
// ignores a slow replica once three or more read: four at 1, 1, 1 and 0.5
// read 1.0 against the 0.875 they serve, 14 % high, the side that orders too
// few (review, round 5). The median was there against outliers; the
// readings that reach this fold have passed the settle, joined, filling and
// served-bound tests, so what is left is what a replica serves.
//
// The per-replica readings of the sample belong to the sample: they are kept
// with its time (throughputPodReads) and start over with the next sample, and
// a sample with none on record -- restored, or put aside and brought back --
// counts its own value as the reading of the replica that reads next.
func (a *SaturationAnalyzer) recordSaturatedThroughputFrom(key, pod string, rate float64) muWrite {
	// `!(rate > 0)` rejects a NaN, which `rate <= 0` would admit. +Inf is
	// excluded too, to match floor.priceable: it passes `rate > 0`, and left in
	// it would take one of the window's ten slots from a real reading and be
	// logged verbatim as a saturated rate.
	if !(rate > 0) || math.IsInf(rate, 1) {
		return muNotWritten
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[key]
	// An exact repeat is use, not a write (SwitchMuRepeatIsUse), so it is
	// judged before the episode: a frozen series past EpisodeGap neither
	// keeps the old episode nor starts a new one with its frozen value.
	if ok && a.muRepeatIsUse && !isPrefillBoundKey(key) && a.exactRepeatLocked(key, rate, now) {
		ra.TouchAt(now)
		return muRepeated
	}
	// WriteGapExceedsAt, not StaleAt, for the reason given at the k2 history
	// write in replica_capacity.go: the floor reads this window every
	// cycle, so a use-based check never sees the write gap it is asking
	// about.
	if !ok || ra.WriteGapExceedsAt(now, a.horizons.EpisodeGap) {
		ra = capacity.NewRollingAverage(capacity.RollingAverageWindowSize, now)
		a.saturatedThroughput[key] = ra
		delete(a.throughputSampledAt, key)
		delete(a.throughputLastRead, key)
		delete(a.throughputPodReads, key)
		// A new window's freshness is its own; a replica's run of low
		// readings is about the replica, and stands.
		if c, ok := a.throughputMuChoice[key]; ok {
			c.saturatedAt, c.yielded, c.sibling = time.Time{}, false, 0
			c.latchKnown = true // a new window has given way to nothing
		}
	}
	lastRead, read := a.throughputLastRead[key]
	a.throughputLastRead[key] = rate
	if last, sampled := a.throughputSampledAt[key]; sampled {
		if now.Sub(last) < ThroughputSampleSpacing {
			// The last sample's window, still being read.
			reads := a.throughputPodReads[key]
			if reads.at != last || reads.pods == nil {
				reads = podReads{at: last, pods: map[string]float64{pod: ra.Last()}}
			}
			reads.pods[pod] = max(reads.pods[pod], rate)
			a.throughputPodReads[key] = reads
			ra.SetLastAt(meanOf(reads.pods), now)
			return muFolded
		}
		if read && rate == lastRead {
			// The same pair across the boundary, or a repeat; for mu with
			// SwitchMuRepeatIsUse on, judged above.
			ra.ObservedAt(now)
			return muRepeated
		}
	}
	ra.AddAt(rate, now)
	if pod != "" && !isPrefillBoundKey(key) {
		// A replica's rate started a sample of mu's window; the prefill
		// bound writes its own window through this fold and is not mu.
		a.paths.add(pathMuWritten, 1)
	}
	a.throughputSampledAt[key] = now
	a.throughputPodReads[key] = podReads{at: now, pods: map[string]float64{pod: rate}}
	return muSampled
}

// exactRepeatLocked reports whether rate, read now for key, is an exact
// repeat of the last reading recorded: at or past the sample spacing, at
// exactly the value last read (recordSaturatedThroughputFrom). Callers hold
// a.mu.
func (a *SaturationAnalyzer) exactRepeatLocked(key string, rate float64, now time.Time) bool {
	last, sampled := a.throughputSampledAt[key]
	lastRead, read := a.throughputLastRead[key]
	return sampled && read && rate == lastRead && now.Sub(last) >= ThroughputSampleSpacing
}

// podReads is one throughput sample's readings per replica, and the time of
// the sample they belong to (recordSaturatedThroughputFrom).
type podReads struct {
	at   time.Time
	pods map[string]float64
	// floor is the prefill bound's window max when the sample started: the
	// figure the sample may not fall below (notePrefillBound). Zero for mu.
	floor float64
}

// meanOf is the mean of the readings, zero when there are none.
func meanOf(reads map[string]float64) float64 {
	if len(reads) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range reads {
		sum += v
	}
	return sum / float64(len(reads))
}

// lowerMedian is the lower median of the readings, the order statistic the
// window itself reads (capacity.RollingAverage.Median).
func lowerMedian(reads map[string]float64) float64 {
	vs := slices.Sorted(maps.Values(reads))
	if len(vs) == 0 {
		return 0
	}
	return vs[(len(vs)-1)/2]
}

// saturatedThroughputFor returns the saturated completion rate on record for
// key, and the bucket it came from. When the key's own bucket has no reading
// it borrows the nearest output-length bucket's under the same key prefix
// (see nearestSaturatedThroughput); the returned bucket names which, so the
// log can say the figure is borrowed. Returns 0 and "" when no bucket has one.
func (a *SaturationAnalyzer) saturatedThroughputFor(key string) (float64, string) {
	r := a.saturatedThroughputReading(key)
	return r.rate, r.bucket
}

// throughputReading is what the floor knows about a key's saturated
// throughput: the figure, the bucket it came from, how many readings that
// bucket's window holds, and whether the bucket is a neighbour's.
type throughputReading struct {
	rate     float64
	bucket   string
	samples  int
	borrowed bool
	// sampledAt is when the key's own window took its newest sample
	// (throughputSampledAt); zero for any other reading, or when unknown.
	sampledAt time.Time
	// writtenAt is when the key's own window last took a reading (its
	// write clock), never earlier than sampledAt; zero for any other
	// reading.
	writtenAt time.Time
}

// saturatedThroughputReading is saturatedThroughputFor with the window's
// size and provenance, which decide whether the floor may order on it.
//
// Touch() on the way out, for the reason spelled out at the k2 history read in
// replica_capacity.go: recordSaturatedThroughput is the only writer and it
// needs a saturated queue, so a window swept on "last written" is swept on
// "last SATURATED" -- which deletes the floor's input from exactly the fleets
// that are coping. The floor then either abstains (priceable(0) is false, so
// mayOrder goes false for the role) or borrows a neighbour bucket at a rate
// measured 2.97x too high. Aged on "last read" instead, a bucket the floor
// prices from every cycle stays, and one nothing reads still goes.
func (a *SaturationAnalyzer) saturatedThroughputReading(key string) throughputReading {
	return a.saturatedThroughputReadingUnder(key, "", true)
}

// saturatedThroughputReadingUnder is saturatedThroughputReading for a variant
// running the engine arguments args (windowArgs): the key's own window has
// been checked against them already, and a neighbouring bucket's window
// written under other arguments is not borrowed (window_args.go). serving
// is admission.serving for the row asking: only a serving row advances the
// hold clock of a put-aside window, as only one runs a lower bound's.
func (a *SaturationAnalyzer) saturatedThroughputReadingUnder(key, args string, serving bool) throughputReading {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ra, ok := a.saturatedThroughput[key]; ok {
		ra.TouchAt(a.now())
		return throughputReading{rate: ra.Median(), bucket: bucketOf(key), samples: ra.Len(),
			sampledAt: a.throughputSampledAt[key], writtenAt: ra.LastWritten()}
	}
	// The key's own window from before the variant's arguments changed: a
	// hold-only figure, with the same authority as a neighbouring bucket's
	// (borrowed) -- the floor may hold the fleet on it, never grow it --
	// until the new arguments measure their own. Dropped instead, the change
	// switched off the one floor that can only add replicas, for as long as
	// the new arguments took to saturate.
	//
	// For a bounded time: BorrowedK2Hold + BorrowedK2Relax of serving, the
	// life a k2 lower bound gets, counted as its clock is (a gap past the
	// tick gap counts as one tick gap). And read without Touch, so it ages
	// from the change. Touched and unbounded, it held the fleet the old
	// engine needed for as long as the load was steady: a change that
	// doubled the service rate (--max-num-seqs 64 -> 256) left the replicas
	// at half load, so none saturated, the new rate was never measured, and
	// the read kept the old one alive -- 2x the replicas with no end date.
	if args != "" {
		if k, st, ok := a.previousThroughputLocked(key); ok {
			// Only a serving row -- admitted, not mid-rollout -- counts: a
			// bridge, a Pod that is not Ready or a rollout reads the window
			// as it stands. Counted on every read, a rollout spent the hold
			// the new arguments were to get, as the k2 bound's did before its
			// clock was made serving-only.
			if serving {
				now := a.now()
				if !st.heldTick.IsZero() {
					st.heldFor += min(now.Sub(st.heldTick), max(a.borrowTickGap, BorrowedK2TickGap))
				}
				st.heldTick = now
				a.throughputStash[k] = st
			}
			if st.heldFor <= BorrowedK2Hold+BorrowedK2Relax {
				return throughputReading{rate: st.window.Median(), bucket: bucketOf(key), samples: st.window.Len(), borrowed: true}
			}
		}
	}
	rate, bucket, samples := a.nearestSaturatedThroughput(key, args)
	return throughputReading{rate: rate, bucket: bucket, samples: samples, borrowed: rate > 0}
}

// previousThroughputLocked is key's throughput window put aside under other
// arguments, the most recently read if there are several, with its stash key;
// ok is false when there is none. Callers hold a.mu.
func (a *SaturationAnalyzer) previousThroughputLocked(key string) (string, stashedThroughput, bool) {
	prefix := key + "#"
	var bestKey string
	var best stashedThroughput
	var bestUsed time.Time
	for k, st := range a.throughputStash {
		if !strings.HasPrefix(k, prefix) || st.window == nil || st.window.Len() == 0 {
			continue
		}
		if used := st.window.Snapshot().LastUsed; bestKey == "" || used.After(bestUsed) {
			bestKey, best, bestUsed = k, st, used
		}
	}
	return bestKey, best, bestKey != ""
}

// nearestSaturatedThroughput finds the reading in the output-length bucket
// closest to key's, on the same model, accelerator, GPU count, role and queue
// threshold. Caller holds a.mu.
//
// A shape the fleet has not yet been seen saturated under has no throughput
// of its own, and without this the floor would simply vanish on the first
// cycle of a new shape -- which is the moment a floor is for. Measured on the
// shape-swap benchmark: the switch from 1000 to 4000 output tokens landed in
// an empty bucket, occupancy read 400k at three replicas, and the target went
// from 3 towards 1 in one cycle, where the shared bucket it replaced would
// at least have held two.
//
// The neighbour's figure is wrong in a known direction. Throughput falls with
// output length, so a shorter shape's mu is too high and holds too few
// replicas (the fleet then saturates and learns its own, exactly once); a
// longer shape's mu is too low and holds too many, which the cap bounds at
// the fleet's own size. Either is better than no floor. The borrowed figure
// is used only until the bucket has a reading of its own: a fresh window
// takes over the first cycle it exists, so it is never masked by the
// neighbour's max.
//
// Ties between an equally distant shorter and longer bucket go to the shorter
// one -- the under-hold, which occupancy corrects, rather than the over-hold,
// which only the cap does.
func (a *SaturationAnalyzer) nearestSaturatedThroughput(key, args string) (float64, string, int) {
	prefix, bucket, suffix, ok := splitHistoryKey(key)
	if !ok {
		return 0, "", 0
	}
	own := slices.Index(outputBuckets, bucket)
	if own < 0 {
		return 0, "", 0
	}
	for dist := 1; dist < len(outputBuckets); dist++ {
		for _, i := range []int{own - dist, own + dist} {
			if i < 0 || i >= len(outputBuckets) {
				continue
			}
			neighbour := prefix + outputBuckets[i] + suffix
			if ra, found := a.saturatedThroughput[neighbour]; found && a.throughputWindowUsable(neighbour, args) {
				// A borrowed bucket is still a bucket a decision reads, so it
				// ages on this read like the key's own window does.
				ra.TouchAt(a.now())
				return ra.Median(), outputBuckets[i], ra.Len()
			}
		}
	}
	return 0, "", 0
}

// splitHistoryKey takes a key of the form built by historyKey --
// namespace|variant|model|accelerator|gpus|role|bucket|qN -- apart around its
// OUTPUT bucket, the second-to-last |-field; the queue threshold is the last.
// No field contains "|" (Kubernetes names cannot, and model IDs do not), so
// counting from the right is safe however many fields lead.
//
// Everything left of the bucket lands in prefix, and a neighbour key is
// re-formed from prefix+bucket+suffix, so a borrow keeps all of it. That
// includes the namespace and variant -- a borrow walks the variant's OWN other
// buckets, never another variant's -- and, in a throughput key, the input
// bucket ahead of the output one: a borrow crosses output buckets, never input
// ones.
func splitHistoryKey(key string) (prefix, bucket, suffix string, ok bool) {
	last := strings.LastIndexByte(key, '|')
	if last < 0 {
		return "", "", "", false
	}
	prev := strings.LastIndexByte(key[:last], '|')
	if prev < 0 {
		return "", "", "", false
	}
	return key[:prev+1], key[prev+1 : last], key[last:], true
}

// bucketOf returns the output-length bucket a history key was built with.
func bucketOf(key string) string {
	_, bucket, _, ok := splitHistoryKey(key)
	if !ok {
		return ""
	}
	return bucket
}

// applyThroughputFloor raises roleDemand (in place) and returns the raised
// totalDemand wherever the throughput model exceeds what occupancy measured,
// after taking the queues' residency charge out of the measurement for every
// role the model can price.
//
// On a disaggregated fleet each role is floored on its own: the scheduler's
// arrival RATE is every request, and every request passes through both roles,
// so each must keep up with all of it. The queue STANDING at the scheduler is
// a different quantity and is charged to prefill alone -- see the backlog build
// below. The model-level total moves
// with them, by what each role CONTRIBUTES to it -- the role's own figure for
// every role but prefill, whose scheduler-queue share the total never carried
// (contributionToTotal). On a non-disaggregated fleet there is no RoleDemand
// and the single "both" floor lands on the total directly.
//
// eppByRole is the residency charge estimateSchedulerQueueDemand put on the
// scheduler queue per role, and eppQueued the requests in it. For a role with
// a mu, both that charge and the engines' own queue charge (LocalQueueDemand)
// come back out; the engines' own queued requests go into the floor as that
// role's backlog, and the scheduler's standing queue goes in as prefill's. For
// prefill without a mu, the scheduler-queue charge is dropped (file header),
// and that is logged at the per-replica verbosity when it changes the figure:
// it is the normal state of a P/D fleet, so an INFO line every cycle would be
// noise, but a reader comparing scheduler-queue-demand's byRole with
// RoleDemand needs to find the gap somewhere.
//
// Logged at INFO when it binds, with the terms: a floor that changes a
// decision is worth a line, and which of lambda, mu, P and the backlog moved
// is the first question anyone will ask of it. A role with no throughput on
// record is silent -- that is the normal state of a fleet that has never been
// saturated, not a gap.
func (a *SaturationAnalyzer) applyThroughputFloor(
	input domain.AnalyzerInput,
	cfg *config.ScalingPolicy,
	replicas []capacity.ReplicaCapacity,
	variants []domain.VariantCapacity,
	totalDemand float64,
	roleDemand map[string]float64,
	eppByRole map[string]float64,
	eppQueued float64,
	// staleShape is set while a fleet-shape change is outstanding: every
	// mu on record was learned under a shape the fleet has left, so the
	// floor may hold on one but not order on it (shape_change.go).
	staleShape bool,
	logger logr.Logger,
) float64 {
	tune := tuningOf(cfg)
	roleOf := make(map[string]string, len(variants))
	for _, vc := range variants {
		roleOf[vc.VariantName] = domain.CanonicalRole(vc.Role)
	}
	// The projection is handed start times ONLY when lambda is a true arrival
	// rate. offeredArrivalRate falls back to the replicas' COMPLETION rate when
	// EPP reports nothing, and its own doc says that understates lambda exactly
	// when demand is highest -- so on that path arrived = lambda*T is computed at
	// one rate while served = mu*(...) is computed at another, the two cancel,
	// and the projection becomes a pure subtraction that under-orders. Better no
	// projection than one built on two different rates. Read before the
	// backlog, whose engine queues are capped over the same drain horizon.
	var startSeconds map[string]float64
	if input.ArrivalRate > 0 {
		startSeconds = a.startSecondsByRole(input, roleOf)
	}
	// Per role: the requests waiting in the engines' own queues, and the
	// residency charge on them. Own replicas only -- a bridge's queue is
	// counted toward the variant's demand by aggregation, but it is not this
	// variant's backlog to size for.
	backlog := make(map[string]float64)
	// queued is the same backlog uncapped: what noteBacklogGrowth compares.
	queued := make(map[string]float64)
	residency := make(map[string]float64)
	for _, rc := range replicas {
		if rc.FromWarmPool {
			continue
		}
		role := roleOf[rc.VariantName]
		// Every queue counts, whichever replica it stands on. The backlog is
		// requests to drain at mu, and a sibling's KV room says nothing about
		// whether the fleet has the throughput to drain them: in the floor's
		// own model a replica's mu is already spent on its share of lambda.
		// Leaving out the share an idle sibling had room for read three
		// replicas queued 60 each beside a fourth never routed to at 5.88
		// replicas, against the 9.41 the fleet needed with perfect routing.
		//
		// But a request waiting in one engine's queue can only be served by
		// that engine: replicas added take new arrivals and nothing else. So a
		// replica's own queue counts for at most what the replica itself
		// drains in the floor's drain time, mu x T (localQueueBacklog), which
		// is one replica's worth: the floor then asks for lambda / mu plus the
		// replicas that are queued. Charged in full, in review, a queue of
		// 1000 pinned on one of four replicas (mu 1, lambda 2) ordered 21.96
		// replicas where at most 3.53 can help, and three replicas each queued
		// 600 at lambda 5 ordered 41.2 against the 9.41 of use. A queue each
		// replica drains inside T is untouched, so the short fleet above still
		// reads 9.41. The router's queue (eppQueued, below) is not capped: the
		// router can send it to any replica, new ones included.
		backlog[role] += localQueueBacklog(rc, floor.DrainHorizon(floor.BacklogDrainSeconds, startSeconds[role]))
		queued[role] += float64(rc.QueueLength)
		residency[role] += float64(rc.LocalQueueDemand)
	}
	// The residency charge is per role -- estimateSchedulerQueueDemand already
	// splits the queue's tokens by the role that serves them.
	//
	// The REQUEST COUNT is not, and must not be. A standing queue is a backlog
	// for the role that serves it FIRST, which on a disaggregated fleet is
	// prefill: its prompts reach decode only once prefill has prefilled them,
	// at prefill's throughput, so decode can neither drain them within
	// drainSeconds nor be sized as though it could. The ongoing arrival is
	// already charged to every role -- lambda is model-wide and floor.Estimate
	// applies it to each -- so adding the standing queue on top of it for a
	// role that cannot receive it yet is a double count, not a safety margin.
	//
	// Measured on a P/D run over all 24 loaded cycles: decode's own engines
	// held 1-8 queued requests while this line charged it 1294-3476, the gap
	// identical on both roles. At the worst cycle decode's ask was 15.0
	// replicas, 37 of whose 46 req/s came from a 2224-request queue it held 6
	// of; its own queue gives 2.98. Across the run the measured demand implied
	// a mean of 2.18 replicas against the floored figure's 8.36.
	//
	// Nothing is left unowned by this. Prefill's measured demand already
	// carries the queue's tokens through aggregation -- on that run it implied
	// 481 replicas before the floor cut it to 2 -- so the queue is if anything
	// over-visible on prefill, and the floor's job there is the cap.
	queueOwner := domain.RolePrefill
	if _, split := eppByRole[domain.RolePrefill]; !split {
		// No prefill role: one role serves the queue end to end and takes it.
		queueOwner = ""
	}
	for role, tokens := range eppByRole {
		if queueOwner == "" || role == queueOwner {
			backlog[role] += eppQueued
			queued[role] += eppQueued
		}
		residency[role] += tokens
	}
	// Recorded every cycle, priced or not, so the next has this one to
	// compare with. Uncapped: a queue that grows past what its replica
	// drains in T is still arrivals above what the fleet drains.
	grew := a.noteBacklogGrowth(input.Namespace, input.ModelID, queued, tune)
	// The cap above is right for a STANDING queue only. Requests waiting in
	// an engine can be served by that engine alone, but a queue that grows
	// is arrivals above what the fleet serves, and those arrivals are still
	// to come: replicas added can take them. Lambda may not count them yet
	// -- the router's rate is a one-minute rate that reads a step late, and
	// with no router rate lambda falls back to completions, which on a full
	// fleet are n x mu whatever arrives -- so the growing queue is the only
	// evidence of them, and capped it is lost. While the role's backlog grows
	// (noteBacklogGrowth) the engine queues are charged in full, as the
	// growth test reads them. In review (mu ~1, four full replicas, flow
	// control off): a step from 4 to 20 req/s read 18.82 replicas at 30 s,
	// under the 23.53 the arrivals alone need, and with no router rate a true
	// 12 req/s read 9.41 for two minutes against 14.12. Charged in full they
	// read 23.53 by 30 s and by 120 s. grew lingers for up to
	// BacklogGrowthLookback after the growth stops, so the full charge
	// outlives it by up to a minute: an over-order, the cheap direction.
	for role, q := range queued {
		if grew[role] {
			backlog[role] = q
		}
	}

	// The hold cap is measured against the threshold the ENGINE sizes RC
	// with, which is the saturation analyzer's own (config.AnalyzerThresholds):
	// a cap drawn at the policy-level figure while the engine divides by a
	// per-analyzer override would leave a gap that orders a replica.
	scaleUp, _ := cfg.AnalyzerThresholds(domain.SaturationAnalyzerName)
	tf := floor.Estimate(offeredArrivalRate(input), replicas, variants, backlog,
		floor.BacklogDrainSeconds, scaleUp, staleShape, eppQueued, startSeconds)

	// Prefill with no mu: the scheduler queue's prompts are not resident work
	// for prefill (file header). Only the disaggregated case has a prefill
	// entry to correct.
	//
	// THE MODEL TOTAL MOVES WITH IT, and that is a change. While the queue was
	// charged to both roles the total carried it once as decode's input +
	// output, so prefill's input-only share was never in the total and
	// dropping it from the role alone kept the two consistent. Now each role
	// is charged the half it serves: the total's input tokens ARE prefill's
	// share, and no other role is carrying them. Leaving them in the total
	// while no role claims them makes the total exceed the sum of the roles by
	// exactly the queue's prompt -- 1.2M tokens on the fixture that caught
	// this -- which is a model-level demand nothing is responsible for
	// serving.
	if roleDemand != nil {
		if _, priced := tf.ByRole[domain.RolePrefill]; !priced {
			if share, ok := eppByRole[domain.RolePrefill]; ok && share > 0 {
				if before, ok := roleDemand[domain.RolePrefill]; ok {
					roleDemand[domain.RolePrefill] = before - share
					totalDemand = max(totalDemand-share, 0)
					logger.V(logging.DEFAULT).Info("scheduler-queue-prefill-share-dropped",
						"modelID", input.ModelID, "namespace", input.Namespace,
						"eppQueueSize", eppQueued, "droppedTokens", share,
						"prefillDemandBefore", before, "prefillDemandAfter", before-share,
						"modelTotalAfter", totalDemand)
				}
			}
		}
	}
	if len(tf.ByRole) == 0 {
		return totalDemand
	}
	// Roles in a stable order, so a two-role fleet logs the same way each cycle.
	roles := slices.Sorted(maps.Keys(tf.ByRole))
	modelTotalBefore := totalDemand

	for _, role := range roles {
		tokens := tf.ByRole[role]
		term := tf.Terms[role]
		var measured float64
		if roleDemand != nil {
			if _, ok := roleDemand[role]; !ok {
				// A role the analyzer attributed no demand to is one no variant
				// serves this cycle (aggregateRoleDemand); nothing to hold.
				continue
			}
			measured = roleDemand[role]
		} else {
			measured = totalDemand
		}
		// What occupancy measured with the queues' residency charge removed:
		// the resident KV alone. The queued requests are in the floor as a
		// backlog now, and must not be counted twice.
		resident := max(measured-residency[role], 0)
		// The swap -- the queue priced as work to drain instead of as KV
		// resident at once -- may LOWER the role's demand, and so it is made
		// only where the floor's figure can be trusted to carry the queue:
		// a term that may order and is not held, whose arrival rate is not
		// lagging (FloorLagFraction). Anywhere else the floor only raises:
		// want = max(measured, floor).
		//
		// At a load step lambda lags -- a [1m] rate, or the completion
		// fallback, still reading the minute before -- while the queue is
		// already standing. The replicas say so themselves: what is resident
		// arrived within one residence time, and at saturation a replica
		// holding P tokens completes mu, so the resident KV implies an arrival
		// rate of about resident / cost, and the floor's arrival half
		// (lambda x cost, ArrivalDemand) about the resident KV. A lambda that
		// accounts for less than FloorLagFraction of it is the minute before
		// the step, and cannot price the queue behind it. Measured on the
		// two-namespace rerun
		// (11:20:09): lambda 0.8 req/s with 160 queued, demand 499 446 cut to
		// the 260 123 resident, the analyzer's rc falling 312 936 -> 47 265 in
		// one cycle; at 07:46:12 a term that could not order (mayOrder=false)
		// took 356 392 to 274 556.
		//
		// Blended, not switched (floorTrust). A hard switch at
		// FloorLagFraction made demand fall as lambda rose: in review, the
		// replica above went 499 446 -> 260 123 between 2.95 and 3.0 req/s,
		// so a fleet sitting near the line flipped between the two demands
		// cycle to cycle.
		//
		// And not while the role's backlog is growing (backlogGrew). The
		// lag test reads lambda against the resident KV, which on a saturated
		// fleet sits at about P per replica whatever the arrival rate, so it
		// reads lambda / (n x mu): full trust once a lagging rate reads 80 %
		// of the fleet's capacity, even with the true rate at 150 %. A queue
		// that grew past a noise band over the last minute
		// (noteBacklogGrowth) is arrivals above what the fleet drains, whatever lambda reads, and the swap would price it below
		// the occupancy that holds it. In review, a step from 3 to 9 req/s on
		// one replica of mu 6 read 319 763 before it and 280 704, 354 736
		// under occupancy's 387 263 and 454 573, for the three cycles the
		// queue tripled.
		want := max(measured, tokens)
		if term.MayOrder && !term.Held && !grew[role] {
			trust := 1.0
			if resident > 0 {
				trust = tune.floorTrust(term.ArrivalDemand / resident)
			}
			want -= trust * (want - max(resident, tokens))
		}
		if want == measured {
			continue
		}
		logger.Info("throughput-demand-floor",
			"modelID", input.ModelID, "namespace", input.Namespace, "role", role,
			"demandBeforeFloor", measured, "residentDemand", resident, "flooredTo", want,
			"arrivalRate", tf.Lambda, "backlogRequests", term.Backlog, "drainSeconds", tf.DrainSeconds,
			// backlogRequests with each engine queue capped at what its
			// replica drains in the drain time; this is the queues uncapped.
			"queuedRequests", queued[role],
			"saturatedThroughput", term.Mu, "perReplicaCapacity", term.PerReplica,
			"replicasImplied", term.Replicas, "heldAtFleet", term.Held, "heldWhy", term.HeldWhy,
			"orderedBehindQueue", term.OrderedBehindQueue,
			// The two gates behind heldAtFleet. The hold is a CAP, so
			// heldAtFleet=false means either "mu was orderable" or "it was not
			// and the cap sat above an already-small floor" -- two different
			// states that a run testing the borrow path has to tell apart, and
			// could not.
			"mayOrder", term.MayOrder, "borrowedOnly", term.BorrowedOnly,
			// mu was a prefill lower bound (prefill_bound.go), and whether
			// the growth cap on it bound this cycle.
			"lowerBound", term.LowerBound, "growthCapped", term.GrowthCapped,
			// How many replicas the standing queue was worth when the release
			// fired. Run T showed orderedBehindQueue=true on five consecutive
			// cycles and no way to tell whether the cap granted one replica
			// because that was all the queue justified or because one was all
			// it ever granted.
			"queueJustifiedReplicas", term.QueueJustifiedReplicas,
			// Both, because they are different questions. backlogRequests is
			// what was measured; projectedBacklog is what the floor priced,
			// and a run that shows only one cannot tell an empty queue from a
			// queue the fleet is projected to outrun.
			"projectedBacklog", term.ProjectedBacklog,
			// The backlog grew past the noise band over the last minute
			// (noteBacklogGrowth), so the swap was not made: arrivals
			// exceed what the fleet drains.
			"backlogGrew", grew[role])
		if roleDemand != nil {
			roleDemand[role] = want
		}
		totalDemand += want - measured
	}

	// What the floor did to the MODEL total, which is otherwise emitted
	// nowhere on a disaggregated fleet: wva_analyzer_demand carries the
	// model level only when a result has no role capacities
	// (steadystate.recordAnalyzerSeries), and a P/D fleet always has them.
	// The per-role lines above do not add up to this figure -- that is the
	// whole reason contributionToTotal exists -- so a reader checking the
	// fleet's target against the floor has no way to derive it.
	if totalDemand != modelTotalBefore {
		logger.Info("throughput-demand-floor-total",
			"modelID", input.ModelID, "namespace", input.Namespace,
			"modelTotalBefore", modelTotalBefore, "modelTotalAfter", totalDemand,
			"roles", roles)
	}
	return totalDemand
}

// A NOTE ON WHAT THE MODEL TOTAL HOLDS, and why there is no correction here.
//
// There used to be a heldInModelTotal() at this point, subtracting prefill's
// scheduler-queue share before moving the model total by a role's delta. It
// existed because the queue was charged to BOTH roles -- prefill got the input
// tokens and decode got the same input tokens plus the output -- while the
// model total carried the queue once. The per-role demands therefore summed to
// the total PLUS one input-token charge, and moving the total by each role's
// full delta took out tokens it had never held, driving it below zero whenever
// both roles' demand fell.
//
// estimateSchedulerQueueDemand now charges each half of a queued request to the
// role that serves it: the prompt to prefill, the generation to decode. The
// per-role charges are disjoint slices of the same total, so they sum to it and
// a role's full delta is exactly what the total held for that role. The
// correction is not merely unnecessary, it would be wrong -- it would subtract
// a share the total now genuinely carries.
//
// PRE-floor `measured` is still the right thing to subtract, and that part has
// not changed: it comes from aggregateRoleDemand as DemandByRole plus the
// queue's per-role charge, while the post-floor `want` is an independent
// (lambda + backlog / drain) x P / mu with no queue term at all.

// FloorLagFraction is the least share of the resident KV the floor's arrival
// half (floor.Term.ArrivalDemand) must account for before the floor may price
// the queues as drain rather than residency, and so lower a role's demand
// (applyThroughputFloor). Half, not one: the resident KV of a saturated
// replica can stand above P -- k1 is kvCacheThreshold of the cache and a full
// replica holds more -- and a fleet draining a backlog with lambda right reads
// 0.8-0.9 of it. A load step whose lambda still reads the minute before reads
// 0.13 (0.8 req/s against a replica holding 260 123 tokens at mu 6).
// The default of analyzerTuning.floorTrust.lagFraction.
const FloorLagFraction = config.DefaultFloorLagFraction

// FloorTrustFraction is the share of the resident KV the arrival half must
// account for before the floor's drain price replaces the queues' residency
// charge in full. Between FloorLagFraction and it the two are blended in
// proportion (floorTrust); 0.8 is the bottom of what a fleet draining a
// backlog with lambda right reads (0.8-0.9), so such a fleet is fully trusted.
// The default of analyzerTuning.floorTrust.trustFraction.
const FloorTrustFraction = config.DefaultFloorTrustFraction

// A role's backlog has grown (noteBacklogGrowth) when it exceeds the backlog
// BacklogGrowthLookback ago by more than max(BacklogGrowthMinRequests,
// BacklogGrowthFraction of it).
//
// Not "larger than on the previous cycle". The engine queue is a one-minute
// max read every 15 s, so successive readings overlap, and with the
// scheduler's queue beside it a fleet draining steadily reads a request or two
// either way from cycle to cycle. Counted strictly, each +1 cycle skipped the
// swap and the next made it: in review a queue reading 160, 161, 160, 161 at
// 4.75 req/s moved demand 326 185 <-> 500 942 every cycle -- the cycle-to-cycle
// flip floorTrust exists to remove, brought back on a noisier input. A step
// grows the queue by tens of requests a cycle and clears the band at once.
const (
	// BacklogGrowthLookback is how far back the backlog a cycle's is compared
	// with was read: one rate window (ThroughputSampleSpacing), so the two
	// one-minute maxima share no samples. Until a reading that old exists the
	// oldest on record stands in.
	BacklogGrowthLookback = ThroughputSampleSpacing
	// BacklogGrowthMinRequests is the least growth, in requests, that counts.
	// The default of analyzerTuning.backlogGrowth.minRequests.
	BacklogGrowthMinRequests = config.DefaultBacklogGrowthMinRequests
	// BacklogGrowthFraction is the least growth, as a share of the backlog it
	// is compared with, that counts. A standing queue's one-minute max moves
	// by a few percent from minute to minute; at 5 %, in review, four queues
	// alternating 100 / 106 cleared the band every other minute, lifted the
	// engine-queue cap and flipped the order 7.06 <-> 10.67 (round 5). 10 %
	// is above that jitter, and a queue really growing -- tens of requests a
	// cycle -- clears it within a cycle or two. What it costs is a queue that
	// grows slower than 10 % a minute: the cap stays on it and the floor
	// reads it as standing.
	// The default of analyzerTuning.backlogGrowth.fraction.
	BacklogGrowthFraction = config.DefaultBacklogGrowthFraction
)

// maxBacklogReadings bounds the backlog readings a model's memo keeps, for a
// clock that does not advance: BacklogGrowthLookback at 15 s cycles is 4.
const maxBacklogReadings = 16

// backlogReading is the throughput floor's backlog per role on one cycle.
type backlogReading struct {
	at     time.Time
	byRole map[string]float64
}

// noteBacklogGrowth records this cycle's floor backlog per role in the
// model's memo and reports the roles whose backlog has grown past the band
// above since BacklogGrowthLookback ago. A role with no earlier reading has
// not grown: there is nothing to compare with.
func (a *SaturationAnalyzer) noteBacklogGrowth(namespace, modelID string, backlog map[string]float64, t tuning) map[string]bool {
	model := namespace + "|" + modelID
	var now time.Time
	if a.now != nil {
		now = a.now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.k2Settle == nil {
		a.k2Settle = make(map[string]*settleMemo)
	}
	memo, ok := a.k2Settle[model]
	if !ok {
		memo = newSettleMemo(now)
		a.k2Settle[model] = memo
	}
	// The newest reading at least BacklogGrowthLookback old, else the oldest;
	// everything older than it is no longer needed.
	ref := 0
	for i, r := range memo.backlogs {
		if now.Sub(r.at) >= BacklogGrowthLookback {
			ref = i
		}
	}
	memo.backlogs = memo.backlogs[ref:]
	grew := make(map[string]bool, len(backlog))
	if len(memo.backlogs) > 0 {
		then := memo.backlogs[0].byRole
		for role, b := range backlog {
			if prev, seen := then[role]; seen && b > prev+max(t.BacklogGrowth.MinRequests, t.BacklogGrowth.Fraction*prev) {
				grew[role] = true
			}
		}
	}
	memo.backlogs = append(memo.backlogs, backlogReading{at: now, byRole: maps.Clone(backlog)})
	if n := len(memo.backlogs); n > maxBacklogReadings {
		memo.backlogs = memo.backlogs[n-maxBacklogReadings:]
	}
	if a.now != nil {
		memo.seen = now
	}
	return grew
}

// localQueueBacklog is what one replica's own engine queue adds to its role's
// floor backlog: the queue, but no more than the replica drains itself in
// horizon seconds at its saturated throughput (applyThroughputFloor). A
// replica with no throughput that can price it adds its whole queue; its role
// is priced by its siblings' mu, if at all.
func localQueueBacklog(rc capacity.ReplicaCapacity, horizon float64) float64 {
	q := float64(rc.QueueLength)
	if mu := rc.SaturatedThroughput; mu > 0 && !math.IsInf(mu, 1) && horizon > 0 {
		return min(q, mu*horizon)
	}
	return q
}

// floorTrust is how far the floor's drain price may replace the queues'
// residency charge, from 0 at FloorLagFraction of the resident KV or less to
// 1 at FloorTrustFraction or more, given ratio = ArrivalDemand / resident.
// Linear between, so the demand moves continuously with lambda rather than
// dropping by the whole queue at one point.
func (t tuning) floorTrust(ratio float64) float64 {
	lag, full := t.FloorTrust.LagFraction, t.FloorTrust.TrustFraction
	if !(ratio > lag) {
		return 0
	}
	return min((ratio-lag)/(full-lag), 1)
}

// offeredArrivalRate is the model-level arrival rate: the scheduler's, or the
// completion rate of the replicas that generate output when the scheduler
// reports none.
//
// The EPP is the only source of a model-level arrival rate. Without it, fall
// back to what the engines completed: at steady state a queue that is neither
// growing nor shrinking makes completion rate equal arrival rate. That
// equality fails exactly when a queue is building, and completions are then
// capped by capacity -- so this understates lambda precisely when demand is
// highest. Tolerable because a building queue is the case OCCUPANCY reads
// well, so the floor is not what carries that decision.
//
// The fallback sums the generating replicas only. A P/D request completes on
// its prefill replica AND on its decode replica, so summing every replica's
// completion rate counts each request twice.
func offeredArrivalRate(input domain.AnalyzerInput) float64 {
	if input.ArrivalRate > 0 {
		return input.ArrivalRate
	}
	var lambda float64
	for _, rm := range generatingReplicas(input) {
		lambda += rm.RequestRate
	}
	return lambda
}

// generatingReplicas returns the replicas whose completions describe the
// workload -- every role but prefill (see generatesOutput). On a P/D fleet a
// prefill replica finishes each request after one token and hands it off, so
// its completion rate, output length and service time are properties of the
// role, not of the workload. When that leaves nothing, which happens only on a
// fleet whose decode side reports no metrics this cycle, it returns the full
// set rather than an empty one: a prefill reading is a poor estimate, but no
// reading at all would make the floor silently decline on a fleet that is
// demonstrably serving.
func generatingReplicas(input domain.AnalyzerInput) []domain.ReplicaMetrics {
	roles := rolesFromStates(input.VariantStates)
	out := make([]domain.ReplicaMetrics, 0, len(input.ReplicaMetrics))
	for _, rm := range input.ReplicaMetrics {
		if generatesOutput(rm, roles) {
			out = append(out, rm)
		}
	}
	if len(out) == 0 {
		return input.ReplicaMetrics
	}
	return out
}
