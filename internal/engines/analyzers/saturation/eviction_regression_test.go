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
		// Wiring the sweep up moved k2 off the measured figure for buckets
		// the sweep deleted, and the fall-through is "usually larger" --
		// larger k2 raises effectiveCapacity and orders FEWER replicas, which
		// this project has recorded as breaking TTFT irrecoverably.
		//
		// The fix is retention, not a guard on the read, and the reason is
		// narrower than an earlier version of this comment claimed.
		//
		// The guard that was removed fell through to PRIORITY 4, which returns
		// k1 as k2 -- and since effectiveCapacity = min(k1, k2), k1 is the
		// largest value that expression can take. Refusing a measurement in
		// favour of k1 can therefore only raise capacity and order fewer
		// replicas: measured at 1.8x to 18x depending on k1.
		//
		// It is NOT true that every alternative exceeds a measurement. Derived
		// does not: see "prefers a retained measurement to a LOWER derived
		// figure" below, which is the counterexample a reviewer built after
		// this comment asserted the general rule. k1 is the bound that holds.
		const historyKey = "hist-key"
		k2From := func(a *SaturationAnalyzer) (int64, capacity.K2Source) {
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

		It("reports a retained measurement as historical however old the figure is", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
			ra.Add(50000)
			a.computeCapacityHistory[historyKey] = ra

			// Fresh: the historical figure is what a decision reads.
			k2, src := k2From(a)
			Expect(src).To(Equal(capacity.K2SrcHistorical),
				"a fresh window must answer Priority 2")
			Expect(k2).To(BeNumerically("==", 50000))

			// A day past its last write, which is what a bucket the fleet left
			// and came back to looks like. The figure still answers: it is the
			// most conservative one available, and Priority 1 replaces it the
			// first saturated cycle either way.
			ra.ObservedAt(time.Now().Add(-2 * capacity.HistoryEvictionTimeout))
			staleK2, staleSrc := k2From(a)
			Expect(staleSrc).To(Equal(capacity.K2SrcHistorical),
				"an old measurement is still a measurement, and refusing it can only "+
					"raise capacity: min(k1, k2) means every alternative is >= it")
			Expect(staleK2).To(BeNumerically("==", 50000))
		})

		It("gives the same answer before and after the sweep", func() {
			// The property the eviction commit claimed and did not have.
			//
			// Weak on its own, and worth saying so: on the revision this was
			// written against it passed too, because that revision answered
			// P4-k1 both before and after. It pins "the sweep changes
			// nothing", not "the measurement is used" -- the DescribeTable in
			// sweep_lastuse_test.go is what pins the latter.
			a := NewSaturationAnalyzer(capacity.NewStore())
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
			ra.Add(50000)
			ra.ObservedAt(time.Now().Add(-2 * capacity.HistoryEvictionTimeout))
			a.computeCapacityHistory[historyKey] = ra

			k2Before, srcBefore := k2From(a)
			a.EvictStaleHistory(capacity.HistoryEvictionTimeout, capacity.HistoryRetention)
			k2After, srcAfter := k2From(a)

			Expect(srcAfter).To(Equal(srcBefore),
				"the sweep must not change which priority answers")
			Expect(k2After).To(Equal(k2Before),
				"the sweep must not change the capacity a decision reads")
		})

		// The case the rest of this file cannot reach: k2From passes nil
		// engineParams, so Priority 3 never fires in any of its specs, while
		// the header above used to assert a rule ABOUT Priority 3.
		//
		// Derived is not bounded below by a measurement. nSteady is
		// min(B*O/(I+O), S), so when --max-num-seqs binds, derived lands at
		// nSteady*(I + O/2) -- which is (I + O/2)/(I + O) of the engine's
		// physical occupancy ceiling, 57% at I=1000 O=6000. Any saturated
		// measurement above that sits in the band where retaining it orders
		// FEWER replicas than falling to derived would.
		//
		// Retaining it anyway is still the right call -- it is a measurement
		// and derived is a formula -- but the reason is not that it is always
		// the smaller number, and nothing in the suite said so until here.
		It("prefers a retained measurement to a LOWER derived figure", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
			ra.Add(95000)
			ra.ObservedAt(time.Now().Add(-30 * 24 * time.Hour))
			a.computeCapacityHistory[historyKey] = ra

			// max_num_seqs=16 binds hard: derived is 16*(1000+3000) = 64,000,
			// below the 95,000 on record.
			params := &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 16}
			k2, src := a.computeK2(historyKey, model, ns, variant,
				0, 0, 6000, 1000, 10,
				params,
				919859, // k1: large, so it clamps neither figure
				919859,
				domain.RoleDecode, false, logr.Discard())

			Expect(src).To(Equal(capacity.K2SrcHistorical))
			Expect(k2).To(BeNumerically("==", 95000),
				"a month-old measurement still answers, and here it is HIGHER than the "+
					"derived figure it displaces -- so the retention rule cannot be "+
					"justified by 'the measurement is always the lowest option'")

			// And the figure it displaced, to pin the direction rather than
			// assert it: the same call with no history reaches Priority 3.
			fresh := NewSaturationAnalyzer(capacity.NewStore())
			derived, derivedSrc := fresh.computeK2(historyKey, model, ns, variant,
				0, 0, 6000, 1000, 10, params, 919859, 919859,
				domain.RoleDecode, false, logr.Discard())
			Expect(derivedSrc).To(Equal(capacity.K2SrcDerived))
			Expect(derived).To(BeNumerically("<", k2),
				"derived below measured is what makes the general claim false")
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
				a.EvictStaleHistory(capacity.HistoryEvictionTimeout, capacity.HistoryRetention)
				a.noteReplicaStart(key, ns, variant,
					[]domain.ReplicaMetrics{readyPod("pod-a", 2700)}, logr.Discard())
			}
			Expect(base.Add(10*step).Sub(base)).To(BeNumerically(">",
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
