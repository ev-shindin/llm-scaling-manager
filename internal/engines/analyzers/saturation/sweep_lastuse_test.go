package saturation

import (
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Three reviewers found the same shape of defect in the first version of this
// change, twice over: a window swept on "last WRITTEN" whose only writer is
// the saturated path, so it expires on exactly the fleets that are coping.
// Both windows now Touch on the read and age on last USE.
//
// Each spec is paired: the live case must survive, and the genuinely-unread
// case must still go. Without the second half a Touch that never expired
// anything would pass the first, and the leak would be back.
//
// Note on the clock: RollingAverage.Stale reads wall-clock time.Since, which
// no test can inject. So "a cycle later" is staged by aging the window with
// TouchAt and then asserting against a one-second timeout -- the window was
// either refreshed by the read just now or it was not.
var _ = Describe("learned state ages on last use, not last write", func() {
	const (
		ns      = "ns"
		model   = "m"
		variant = "decode-v"
		// Nearly the whole timeout since the last WRITE, which is the normal
		// state of a variant that has not saturated recently.
		sinceWrite = capacity.HistoryEvictionTimeout - time.Minute
	)

	Describe("the k2 history window", func() {
		// The only writer is Priority 1, which needs a saturated queue, so
		// nothing writes this window for as long as the fleet is comfortable.
		// Two separate defects came out of that, both found in review:
		//
		//   - swept on last WRITE, the window expired on a healthy fleet, so
		//     k2 left its measurement. Fixed by Touch on the read.
		//   - a "trust horizon" on the read refused an old measurement and
		//     fell through. That was MY fix and it was backwards: since
		//     effectiveCapacity = min(k1, k2), a measured k2 can only lower
		//     capacity below k1, so refusing it can only RAISE capacity and
		//     order fewer replicas. Measured at up to 48x. There is no trust
		//     horizon any more: a retained measurement always answers.
		const historyKey = "hist-key"
		k2With := func(a *SaturationAnalyzer, params *capacity.EngineParams, k1 int64) (int64, capacity.K2Source) {
			return a.computeK2(historyKey, model, ns, variant,
				0,    // queueLen: not saturated, so Priority 1 cannot fire
				0,    // tokensInUse
				600,  // avgOutput
				1000, // avgInput
				10,   // queueThreshold
				params,
				k1,
				k1, // kvCeiling
				domain.RoleDecode, false, logr.Discard())
		}
		withHistory := func(k2 float64) (*SaturationAnalyzer, *capacity.RollingAverage) {
			a := NewSaturationAnalyzer(capacity.NewStore())
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
			ra.Add(k2)
			a.computeCapacityHistory[historyKey] = ra
			return a, ra
		}
		// Engine params that derive a figure ABOVE k1 in the small-k1 regime
		// and below it in the large one.
		params := &capacity.EngineParams{EffectiveMaxBatchedTokens: 65536, MaxNumSeqs: 256}

		It("refreshes the window on the read, so a live variant keeps its measured k2", func() {
			a, ra := withHistory(50000)
			ra.TouchAt(time.Now().Add(-sinceWrite))
			Expect(ra.Stale(time.Second)).To(BeTrue(),
				"precondition: nothing has written this window for nearly the whole day")

			k2, src := k2With(a, nil, 90000)
			Expect(src).To(Equal(capacity.K2SrcHistorical))
			Expect(k2).To(BeNumerically("==", 50000))

			Expect(ra.Stale(time.Second)).To(BeFalse(),
				"the read is a USE and must refresh the window: aged on last-write it "+
					"expires and k2 leaves the measurement, for a variant that is "+
					"merely not saturating")
		})

		// The direction property, across BOTH k1 regimes. Pinning only
		// k1 = 90,000 is what let the earlier fall-through-to-k1 change look
		// harmless: there, min(k1, derived) and min(k1, k1) are the same
		// number, so the spec passed on the clamp rather than on the fix.
		DescribeTable("a retained measurement answers, whatever its age and whatever k1 is",
			func(k1 int64, p *capacity.EngineParams) {
				a, ra := withHistory(50000)
				ra.TouchAt(time.Now().Add(-2 * capacity.HistoryEvictionTimeout))

				k2, src := k2With(a, p, k1)
				Expect(src).To(Equal(capacity.K2SrcHistorical),
					"a measurement this fleet made is the LOWEST capacity figure available "+
						"for the bucket; anything else it could fall through to is >= it, so "+
						"refusing it can only order fewer replicas")
				Expect(k2).To(BeNumerically("==", 50000))
			},
			Entry("small k1, derived above it", int64(90000), params),
			Entry("large k1, derived below it", int64(1000000), params),
			Entry("small k1, no engine params at all", int64(90000), nil),
			Entry("large k1, no engine params at all", int64(1000000), nil),
		)

		It("still uses the derived figure for a bucket it has never measured", func() {
			// The cold path is untouched: a genuinely new shape has no window,
			// and derived is the right answer for it.
			a := NewSaturationAnalyzer(capacity.NewStore())
			_, src := k2With(a, params, 90000)
			Expect(src).To(Equal(capacity.K2SrcDerived),
				"a bucket with no history at all is what Priority 3 exists for")
		})

		It("keeps a measurement past the point it stops being written, up to retention", func() {
			a, ra := withHistory(50000)
			ra.TouchAt(time.Now().Add(-2 * capacity.HistoryEvictionTimeout))

			a.EvictStaleHistory()

			Expect(a.computeCapacityHistory).To(HaveKey(historyKey),
				"retention is seven days and the window is one day old; a shape that "+
					"leaves a bucket for a day and comes back must find its measurement")
		})

		It("drops it once it is past the retention horizon", func() {
			a, ra := withHistory(50000)
			ra.TouchAt(time.Now().Add(-2 * capacity.HistoryRetention))

			ev := a.EvictStaleHistory()

			Expect(a.computeCapacityHistory).To(BeEmpty(),
				"retention is seven days, not forever -- the leak must still close")
			Expect(ev.K2History).To(Equal(1))
		})

		// The write path's own question, which is NOT the read's. Reads touch
		// the window every cycle, so asking Stale here could never see a gap
		// for a bucket still being served: two saturation episodes weeks apart
		// blended into one average instead of the second standing alone.
		It("starts a fresh window when a new observation follows a long write gap", func() {
			a, ra := withHistory(90000)
			// Nine more, so a blend would be visible and a reset would not be
			// confusable with it.
			for i := 0; i < 9; i++ {
				ra.Add(90000)
			}
			Expect(ra.Len()).To(Equal(10))

			// A day since the last WRITE, but read every cycle throughout --
			// which is what a bucket the fleet is serving looks like.
			ra.ObservedAt(time.Now().Add(-2 * capacity.HistoryEvictionTimeout))
			for i := 0; i < 5; i++ {
				k2With(a, nil, 1000000)
			}
			Expect(ra.Stale(time.Second)).To(BeFalse(), "reads kept it in use, as they should")
			Expect(ra.WriteGapExceeds(capacity.HistoryEvictionTimeout)).To(BeTrue(),
				"but nothing has been WRITTEN for a day, and that is the write path's question")

			// Now the queue saturates and today's true figure is much lower.
			_, src := a.computeK2(historyKey, model, ns, variant,
				50,    // queueLen over threshold: Priority 1 fires
				10000, // tokensInUse -> k2Observed well below the stored 90,000
				600, 1000, 10,
				nil,
				1000000, 1000000,
				domain.RoleDecode, false, logr.Discard())
			Expect(src).To(Equal(capacity.K2SrcObserved))

			// Re-read from the MAP: a reset stores a new window, so the handle
			// taken before the write still points at the old one.
			fresh := a.computeCapacityHistory[historyKey]
			Expect(fresh).NotTo(BeIdenticalTo(ra), "the window must have been replaced, not appended to")
			Expect(fresh.Len()).To(Equal(1),
				"a new episode must REPLACE the window, not blend into it: averaged "+
					"against nine readings from before the gap, today's lower, truer "+
					"figure is diluted to a tenth and capacity stays high -- fewer "+
					"replicas, which is the direction that breaks TTFT")
			Expect(fresh.Average()).To(BeNumerically("<", 90000),
				"and the surviving figure must be today's, not the pre-gap one")
		})
	})

	Describe("the mu window the demand floor prices from", func() {
		// recordSaturatedThroughput is the only writer and it needs a
		// saturated queue too. Aged on last-write, the floor lost the rate it
		// prices from on a fleet that was coping: priceable(0) is false, so
		// mayOrder goes false for the role, or a neighbour bucket is borrowed
		// at a rate measured 2.97x too high.
		const (
			prefix = model + "|NVIDIA-H200|1|decode|in1k|"
			suffix = "|q10"
			own    = prefix + "medium" + suffix
		)

		It("refreshes the window on the read, so a live floor keeps its rate", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.recordSaturatedThroughput(own, 3.2)
			ra := a.saturatedThroughput[own]
			ra.TouchAt(time.Now().Add(-sinceWrite))
			Expect(ra.Stale(time.Second)).To(BeTrue(), "precondition: unwritten for nearly the timeout")

			rate, bucket := a.saturatedThroughputFor(own)
			Expect(rate).To(Equal(3.2))
			Expect(bucket).To(Equal("medium"), "the key's own bucket, not a borrowed one")

			Expect(ra.Stale(time.Second)).To(BeFalse(),
				"the floor read this bucket, so the sweep must not take it on the next cycle")
		})

		It("refreshes a BORROWED window too, since a borrowed rate is still one a decision reads", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.recordSaturatedThroughput(own, 3.2)
			ra := a.saturatedThroughput[own]
			ra.TouchAt(time.Now().Add(-sinceWrite))

			// A shape the fleet has not saturated under yet reads its
			// neighbour's bucket (nearestSaturatedThroughput).
			rate, bucket := a.saturatedThroughputFor(prefix + "long" + suffix)
			Expect(rate).To(Equal(3.2))
			Expect(bucket).To(Equal("medium"), "borrowed from the nearest bucket with a reading")

			Expect(ra.Stale(time.Second)).To(BeFalse(),
				"the only reading on the fleet is this one; expiring it leaves the floor "+
					"nothing to borrow and no rate at all")
		})

		It("still sweeps a bucket nothing reads, and counts it", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.recordSaturatedThroughput(own, 3.2)
			a.saturatedThroughput[own].TouchAt(time.Now().Add(-2 * capacity.HistoryRetention))

			ev := a.EvictStaleHistory()

			Expect(a.saturatedThroughput).To(BeEmpty(), "a bucket no decision reads is what the sweep is for")
			Expect(ev.MuWindows).To(Equal(1),
				"and it must be counted under its own name: 'the floor just lost its rate' is "+
					"the one thing an operator needs from this log line, and the summed "+
					"counter it replaced did not count this map at all")
		})
	})

	Describe("a producer of learned state stamps its own key", func() {
		// variantIsStale treats an UNSTAMPED key as stale, so a producer that
		// does not stamp has its output deleted by the very next sweep --
		// zero seconds later, not after a timeout.
		//
		// noteITL creates the ITL window and writes the learned baseline and
		// stamped neither. It was harmless only by the order of two calls in
		// a third file: fitLines calls noteReplicaStart, which stamps, just
		// before the decode guard that reaches noteITL. So an early return
		// added to noteReplicaStart, or a reordering of those two calls,
		// would have deleted every learned ITL baseline on the first sweep --
		// silently, falling back to itl.DefaultBaselineSec, the direction this
		// project has recorded as collapsing a fleet.
		//
		// This spec calls noteITL ALONE, which is what no existing spec does
		// and why the gap was invisible: driven through fitLines the suite
		// stays green with the stamp removed.
		It("keeps the window noteITL created, without noteReplicaStart having run", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			key := "ns|m|decode-v|NVIDIA-H200|1"
			now := time.Now()

			a.noteITL(key, nil, variant, now, logr.Discard())
			Expect(a.itlWindows).To(HaveKey(key), "precondition: noteITL creates the window")

			a.EvictStaleHistory()

			Expect(a.itlWindows).To(HaveKey(key),
				"noteITL must stamp the key it creates state under, rather than relying on "+
					"noteReplicaStart having been called first by a third file")
		})
	})

	Describe("a counted Pod whose start time stops reporting", func() {
		// collector.podStartSeconds returns 0 for a Pod that is not Ready,
		// for one whose Pod list could not be read, and for one whose Ready
		// transition is further than maxCredibleStartSeconds from creation --
		// and a readiness re-transition rewrites that timestamp, so a live Pod
		// can enter the last case. The re-stamp used to sit BELOW the
		// StartSeconds > 0 gate, so such a Pod kept its record (the prefix
		// cleanup sees it in `present`) and never refreshed it: the sweep
		// expired it under a live Pod, and the next good reading folded the
		// same Pod into the EWMA a second time.
		const (
			key = "ns|m|decode-v|NVIDIA-H200|1"
			pod = "decode-v-0"
		)
		reporting := func(start float64) []domain.ReplicaMetrics {
			return []domain.ReplicaMetrics{{
				VariantName:  variant,
				Namespace:    ns,
				PodName:      pod,
				StartSeconds: start,
			}}
		}

		It("is re-stamped while it is still reported, so its start is counted once", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			base := time.Now()
			a.now = func() time.Time { return base }

			a.noteReplicaStart(key, ns, variant, reporting(70), logr.Discard())
			Expect(a.startSeconds[key]).To(BeNumerically("==", 70))
			Expect(a.startSeenPods).To(HaveLen(1))

			// Nearly a timeout later the same Pod is still reported, but its
			// start time now reads 0. This is the cycle that must re-stamp.
			quiet := base.Add(capacity.HistoryEvictionTimeout - time.Minute)
			a.now = func() time.Time { return quiet }
			a.noteReplicaStart(key, ns, variant, reporting(0), logr.Discard())

			// A sweep past the ORIGINAL stamp. Re-stamped, the record is two
			// minutes old and survives; gated below StartSeconds > 0 it is
			// still stamped at base, so the sweep expires it under a live Pod.
			swept := base.Add(capacity.HistoryEvictionTimeout + time.Minute)
			a.now = func() time.Time { return swept }
			a.EvictStaleHistory()
			Expect(a.startSeenPods).To(HaveLen(1),
				"the record of a Pod still being reported must survive the sweep whatever "+
					"its StartSeconds reads this cycle")

			// It reports a start time again -- a different one, so a second
			// fold is visible rather than hidden by an EWMA onto itself.
			a.noteReplicaStart(key, ns, variant, reporting(130), logr.Discard())
			Expect(a.startSeconds[key]).To(BeNumerically("==", 70),
				"one Pod's start is one sample however many cycles report it; folded in "+
					"twice the estimate drifts toward whatever the longest-lived replica "+
					"measured, and wva_replica_start_seconds counts cycles rather than starts")
		})
	})
})
