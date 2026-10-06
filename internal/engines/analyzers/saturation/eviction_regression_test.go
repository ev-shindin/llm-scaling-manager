package saturation

import (
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Wiring EvictStaleHistory into the reconcile loop was presented as a pure
// memory fix. It was not: two read paths depended on entries the sweep now
// deletes. These pin both, because the sweep's own tests cannot see either --
// they assert that eviction HAPPENS, not that the figures a decision reads
// are unchanged by it.
var _ = Describe("what the per-cycle sweep must not change", func() {
	const (
		ns      = "ns"
		model   = "m"
		variant = "decode-v"
		accel   = "NVIDIA-H200"
	)

	Describe("the k2 historical read", func() {
		// Priority 2 read a RollingAverage with no staleness check while the
		// write path beside it had one. So a k2 older than the timeout used to
		// be returned as K2SrcHistorical, and once the sweep deleted it the
		// source fell through to the derived figure -- a different, usually
		// larger, capacity. Larger k2 raises effectiveCapacity and orders
		// FEWER replicas, which this project has recorded as breaking TTFT
		// irrecoverably.
		//
		// The guard makes the read agree with the sweep: a stale entry is
		// ignored whether or not the sweep has reached it yet.
		k2From := func(a *SaturationAnalyzer, historyKey string) (int64, capacity.K2Source) {
			return a.computeK2(historyKey, model, ns, variant,
				0,     // queueLen: not saturated, so Priority 1 cannot fire
				0,     // tokensInUse
				600,   // avgOutput
				1000,  // avgInput
				10,    // queueThreshold
				nil,   // engineParams: nil so Priority 3 has nothing but the k1 fallback
				90000, // k1
				90000, // kvCeiling
				domain.RoleDecode, false, logr.Discard())
		}

		It("does not report a stale window as historical", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			key := "hist-key"

			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
			ra.Add(50000)
			a.computeCapacityHistory[key] = ra

			// Fresh: the historical figure is what a decision reads.
			k2, src := k2From(a, key)
			Expect(src).To(Equal(capacity.K2SrcHistorical),
				"a fresh window must still answer Priority 2")
			Expect(k2).To(BeNumerically("==", 50000))

			// Aged past the timeout, WITHOUT running the sweep. The read must
			// already ignore it, or the sweep changes the answer.
			ra.TouchAt(time.Now().Add(-2 * capacity.HistoryEvictionTimeout))
			_, staleSrc := k2From(a, key)
			Expect(staleSrc).NotTo(Equal(capacity.K2SrcHistorical),
				"a stale window must not be read as historical: otherwise the "+
					"sweep deleting it silently changes k2, and with it the replica count")
		})

		It("gives the same answer before and after the sweep", func() {
			// The property the eviction commit claimed and did not have.
			a := NewSaturationAnalyzer(capacity.NewStore())
			key := "hist-key"
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
			ra.Add(50000)
			ra.TouchAt(time.Now().Add(-2 * capacity.HistoryEvictionTimeout))
			a.computeCapacityHistory[key] = ra

			k2Before, srcBefore := k2From(a, key)
			a.EvictStaleHistory(capacity.HistoryEvictionTimeout)
			k2After, srcAfter := k2From(a, key)

			Expect(srcAfter).To(Equal(srcBefore),
				"the sweep must not change which priority answers")
			Expect(k2After).To(Equal(k2Before),
				"the sweep must not change the capacity a decision reads")
		})
	})

	Describe("the replica-start bookkeeping", func() {
		// startSeenPods records that a Pod's start is already folded into the
		// estimate, and the already-counted branch returned before writing, so
		// the stamp was never refreshed. Safe only while evictStartSeenPods had
		// no caller -- which its own comment said. With the sweep wired up, a
		// Pod Ready for longer than the timeout lost its record and had its
		// start folded in AGAIN, then once per timeout for life, driving the
		// estimate toward whatever the longest-lived replica measured.
		readyPod := func(pod string, startSeconds float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, variant, 900_000, tracedKv, 0, 1000, 6000)
			rm.Ready = true
			rm.StartSeconds = startSeconds
			return rm
		}

		It("refreshes the already-counted stamp for a Pod it still sees", func() {
			// This asserts the MECHANISM, not a downstream number, and that is
			// deliberate: an earlier version of this spec folded one Pod's
			// unchanged StartSeconds in twice and asserted the estimate had not
			// moved -- but 0.7*70 + 0.3*70 is 70, so it passed with the fix
			// removed. The observable difference is the stamp.
			a := NewSaturationAnalyzer(capacity.NewStore())
			base := time.Now()
			a.now = func() time.Time { return base }

			key := a.itlWindowKey(ns, model, variant, accel, 1)
			rms := []domain.ReplicaMetrics{readyPod("pod-a", 70)}

			a.noteReplicaStart(key, ns, variant, rms, logr.Discard())
			Expect(a.startSeenPods).To(HaveLen(1))
			var firstStamp time.Time
			for _, ts := range a.startSeenPods {
				firstStamp = ts
			}
			Expect(firstStamp).To(Equal(base))

			// A later cycle, same Pod, still Ready and already counted.
			later := base.Add(2 * capacity.HistoryEvictionTimeout)
			a.now = func() time.Time { return later }
			a.noteReplicaStart(key, ns, variant, rms, logr.Discard())

			Expect(a.startSeenPods).To(HaveLen(1))
			for _, ts := range a.startSeenPods {
				Expect(ts).To(Equal(later),
					"the stamp was not refreshed, so the per-cycle sweep will "+
						"evict it under a live Pod and the start will be folded "+
						"in again -- once per timeout for the Pod's whole life")
			}
		})

		It("keeps a live Pod counted across the sweep that now runs every cycle", func() {
			// The consequence of the above, through the sweep itself: the
			// histogram must see one observation per start, not one per
			// timeout. Asserted on the fold count via the outlier path, which
			// only an uncounted Pod can reach.
			a := NewSaturationAnalyzer(capacity.NewStore())
			base := time.Now()
			a.now = func() time.Time { return base }
			key := a.itlWindowKey(ns, model, variant, accel, 1)

			a.noteReplicaStart(key, ns, variant,
				[]domain.ReplicaMetrics{readyPod("pod-a", 70)}, logr.Discard())
			settled := a.startSeconds[key]
			Expect(settled).To(BeNumerically(">", 0))

			// Cycles advance in realistic steps, and the ORDER matters: the
			// engine sweeps in optimizeV2 before the analysis that re-stamps,
			// so each iteration sweeps first. The run spans well past the
			// timeout in total while no single gap approaches it -- which is
			// what a 15s reconcile over a day actually looks like, and is why
			// refreshing the stamp is sufficient.
			//
			// An earlier version of this spec jumped the clock by two whole
			// timeouts PER iteration. That is not a cycle gap, it is a
			// 48-hour stall between cycles, and under it no amount of
			// re-stamping can help: the entry is already stale when the sweep
			// runs. It failed for that reason and the fix was not at fault.
			step := capacity.HistoryEvictionTimeout / 4
			for i := 1; i <= 10; i++ {
				a.now = func() time.Time { return base.Add(time.Duration(i) * step) }
				a.EvictStaleHistory(capacity.HistoryEvictionTimeout)
				a.noteReplicaStart(key, ns, variant,
					[]domain.ReplicaMetrics{readyPod("pod-a", 2700)}, logr.Discard())
			}
			Expect(base.Add(10 * step).Sub(base)).To(BeNumerically(">",
				capacity.HistoryEvictionTimeout),
				"the run must span more than the timeout, or the sweep is never tested")

			Expect(a.startSeconds[key]).To(Equal(settled),
				"a Pod already folded in must never be folded in again, however "+
					"many sweeps pass: its record is refreshed, not expired")
		})

		It("still forgets a Pod that stops being reported", func() {
			// The bound must survive the fix, or the map grows without limit.
			a := NewSaturationAnalyzer(capacity.NewStore())
			base := time.Now()
			a.now = func() time.Time { return base }
			key := a.itlWindowKey(ns, model, variant, accel, 1)

			a.noteReplicaStart(key, ns, variant,
				[]domain.ReplicaMetrics{readyPod("pod-a", 70)}, logr.Discard())
			Expect(a.startSeenPods).NotTo(BeEmpty())

			// A cycle in which the variant reports a different Pod entirely.
			a.noteReplicaStart(key, ns, variant,
				[]domain.ReplicaMetrics{readyPod("pod-b", 72)}, logr.Discard())
			for seen := range a.startSeenPods {
				Expect(seen).NotTo(ContainSubstring("pod-a"),
					"a Pod the variant no longer reports must be forgotten")
			}
		})
	})
})
