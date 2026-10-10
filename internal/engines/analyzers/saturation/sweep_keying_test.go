package saturation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// A memo that RESOLVES a bucket key must outlive the window it keys.
//
// This is the fifth time the same defect has been found on this branch, in a
// different map each time, so it is worth stating as a rule rather than four
// separate fixes: anything whose value becomes part of a bucket key has to age
// on the horizon of the thing that key reaches, and has to be stamped when it
// is READ, not only when it is written. Otherwise the key moves while the
// measurement is still retained, and the measurement becomes unreachable --
// which looks exactly like never having measured, and prices the fleet from a
// formula instead.
//
// Two maps resolve into a bucket key: lastAccelerator and fleetShape. The
// first was found by one reviewer; the second by the next, after the fix for
// the first had been written and reviewed.
var _ = Describe("a key-resolving memo outlives what it keys", func() {
	const (
		ns    = "ns"
		model = "m"
		// Past the per-variant horizon, inside the bucket-retention one --
		// the band both defects lived in.
		inTheBand = capacity.HistoryEvictionTimeout * 2
		pastBoth  = capacity.HistoryRetention * 2
	)

	Describe("the fleet-shape memo, which keys the mu window", func() {
		// classifyInputLength and classifyOutputLength turn this memo's (I, O)
		// into the mu window's bucket. Lose it and the shape reads 0, the key
		// lands in short/ishort, and the floor reads a rate of 0 from a bucket
		// that was never measured -- while the real measurement sits retained
		// under the real key. priceable(0) is false, so the role stops
		// ordering: the floor abstains on the ramp out of a quiet period,
		// which is the one moment it is load-bearing.
		withShape := func(a *SaturationAnalyzer, age time.Duration) {
			a.fleetShape[ns+"|"+model] = &shapeMemo{
				tracker:  shape.NewTracker(shape.DefaultChangeTolerance),
				stable:   shape.Shape{AvgInputTokens: 8000, AvgOutputTokens: 6000},
				lastSeen: time.Now().Add(-age),
			}
		}

		It("survives the band in which the window it keys is still retained", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			withShape(a, inTheBand)

			ev := a.EvictStaleHistory(capacity.HistoryEvictionTimeout, capacity.HistoryRetention)

			Expect(a.fleetShape).To(HaveKey(ns+"|"+model),
				"the memo resolves the mu window's bucket, so expiring it first makes a "+
					"retained measurement unreachable -- the same defect as the accelerator "+
					"memo, one map over")
			Expect(ev.FleetShapes).To(Equal(0))
		})

		It("is still dropped once it is past retention, so it stays bounded", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			withShape(a, pastBoth)

			ev := a.EvictStaleHistory(capacity.HistoryEvictionTimeout, capacity.HistoryRetention)

			Expect(a.fleetShape).To(BeEmpty(), "seven days, not forever")
			Expect(ev.FleetShapes).To(Equal(1))
		})

		It("is stamped by a READ, so an idle model does not age out while being analysed", func() {
			// lastSeen had exactly one writer, after noteFleetShape's early
			// return, so it meant "last cycle with completions or an arriving
			// prompt". A fleet that is idle but still analysed reads the memo
			// through fleetShapeState every cycle and stamped nothing.
			a := NewSaturationAnalyzer(capacity.NewStore())
			base := time.Now()
			a.now = func() time.Time { return base }
			// PAST retention, not just past the per-variant horizon. Aged only
			// into the band, the sweep keeps the memo whether or not the read
			// stamped it, and the spec proves nothing -- which is what my own
			// control caught before this line said pastBoth.
			withShape(a, pastBoth)

			// One read, the way an idle cycle does it.
			out, in, _ := a.fleetShapeState(ns, model)
			Expect(out).To(BeNumerically("==", 6000), "precondition: the memo answered")
			Expect(in).To(BeNumerically("==", 8000))

			// Sweep a minute later. The only thing that can save the memo now
			// is the read having re-stamped it.
			a.now = func() time.Time { return base.Add(time.Minute) }
			ev := a.EvictStaleHistory(capacity.HistoryEvictionTimeout, capacity.HistoryRetention)

			Expect(a.fleetShape).To(HaveKey(ns+"|"+model),
				"a read is use: the memo must have been re-stamped, or an idle fleet "+
					"loses the shape that keys its own measurement")
			Expect(ev.FleetShapes).To(Equal(0))
		})
	})

	Describe("the mu window itself", func() {
		// The k2 half of each of these is pinned in sweep_lastuse_test.go. The
		// mu half was not, and the mu half is the LARGER of the two measured
		// regressions -- 7.9x against k2's 3.85x -- so the untested half was
		// the worse one.
		const key = "m|NVIDIA-H200|1|decode|in1k|medium|q10"

		It("is retained for the bucket horizon, not the per-variant one", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.recordSaturatedThroughput(key, 3.2)
			a.saturatedThroughput[key].TouchAt(time.Now().Add(-inTheBand))

			ev := a.EvictStaleHistory(capacity.HistoryEvictionTimeout, capacity.HistoryRetention)

			Expect(a.saturatedThroughput).To(HaveKey(key),
				"swept on the per-variant horizon the floor loses its rate six days early")
			Expect(ev.MuWindows).To(Equal(0))
		})

		It("starts a fresh window when a new reading follows a long WRITE gap", func() {
			// recordSaturatedThroughput asks WriteGapExceeds, not Stale, for
			// the same reason computeK2 does: the floor reads this window
			// every cycle, so a use-based check never sees the write gap it is
			// asking about, and two saturation episodes a day apart blend into
			// one median.
			a := NewSaturationAnalyzer(capacity.NewStore())
			base := time.Now()
			a.now = func() time.Time { return base }

			// Ten samples at a high rate, spaced so each counts as its own
			// -- and each a DIFFERENT value, because a reading equal to the
			// last one is treated as the same scrape pair read again and only
			// stamps. Ten identical rates give a window of one.
			for i := 0; i < 10; i++ {
				at := base.Add(time.Duration(i) * 2 * ThroughputSampleSpacing)
				a.now = func() time.Time { return at }
				a.recordSaturatedThroughput(key, 36+float64(i))
			}
			Expect(a.saturatedThroughput[key].Len()).To(Equal(10),
				"precondition: ten distinct samples, median 40")
			Expect(a.saturatedThroughput[key].Median()).To(BeNumerically("==", 40))

			// A day with no writes, while the floor keeps reading it.
			a.saturatedThroughput[key].ObservedAt(base.Add(-inTheBand))
			for i := 0; i < 5; i++ {
				a.saturatedThroughputFor(key)
			}
			Expect(a.saturatedThroughput[key].Stale(time.Second)).To(BeFalse(),
				"reads kept it in use, as they should")

			// Today's episode, at a much lower true rate.
			later := base.Add(inTheBand + time.Hour)
			a.now = func() time.Time { return later }
			a.recordSaturatedThroughput(key, 5)

			fresh := a.saturatedThroughput[key]
			Expect(fresh.Len()).To(Equal(1),
				"a new episode must REPLACE the window: blended against ten readings of "+
					"36-45, today's 5 is an eighth of the median and the floor prices the fleet "+
					"from a rate it can no longer sustain -- FEWER replicas, and the "+
					"larger of the two measured regressions at 7.9x")
			Expect(fresh.Median()).To(BeNumerically("==", 5))
		})
	})
})
