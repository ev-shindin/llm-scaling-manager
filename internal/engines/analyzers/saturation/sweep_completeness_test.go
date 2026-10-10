package saturation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// What EvictStaleHistory must and must not delete, beyond the figures a
// decision reads. Each spec here exists because a mutation of the thing it
// pins left the whole suite green: the sweep is a function whose only job is
// deletion, so anything it deletes silently is untested by construction.
var _ = Describe("the sweep's own completeness", func() {
	// The horizons, named so a reader does not have to look them up: a
	// per-variant stamp expires in a day, a bucket-keyed window is kept a
	// week, and the band between them is where these specs live.
	const (
		inTheBand = capacity.HistoryEvictionTimeout * 2 // past a day, inside a week
		pastBoth  = capacity.HistoryRetention * 2
	)

	Describe("the accelerator memo, which keys the k2 window", func() {
		// lastAccelerator is per variant, so it looks like it belongs on the
		// per-variant horizon -- and it was there. But its VALUE is what
		// resolves into the k2 history key, so expiring it ahead of the window
		// it keys moves the key: the accelerator reads unresolved, the lookup
		// misses a measurement that is still retained, and k2 falls through to
		// the derived figure. That is precisely what retaining the window past
		// its last read exists to prevent, defeated from the side. A reviewer
		// found it; on the short horizon there was a six-day band in which the
		// measurement existed and could not be reached.
		It("outlives the window it keys, so a retained measurement stays reachable", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
			ra.Add(50000)
			a.computeCapacityHistory["m|NVIDIA-H200|1|decode|huge|q5"] = ra
			a.lastAccelerator["ns/v1"] = acceleratorMemo{
				name:     "NVIDIA-H200",
				lastUsed: time.Now().Add(-inTheBand),
			}

			ev := a.EvictStaleHistory()

			Expect(a.lastAccelerator).To(HaveKey("ns/v1"),
				"the memo must age on the same horizon as the k2 history it keys, which is "+
					"what its own doc comment claims: expired first, the key moves to "+
					"'unresolved' and the retained window becomes unreachable")
			Expect(ev.Accelerators).To(Equal(0))
			Expect(a.computeCapacityHistory).To(HaveLen(1), "and the window it keys is still here")
		})

		It("is still dropped once it is past retention, so it stays bounded", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.lastAccelerator["ns/v1"] = acceleratorMemo{
				name:     "NVIDIA-H200",
				lastUsed: time.Now().Add(-pastBoth),
			}

			ev := a.EvictStaleHistory()

			Expect(a.lastAccelerator).To(BeEmpty())
			Expect(ev.Accelerators).To(Equal(1))
		})
	})

	Describe("every deletion is counted", func() {
		// Any() decides whether the sweep logs at all, so a map deleted
		// without a counter makes the one diagnostic this sweep emits silent
		// on a cycle that evicted something. Two maps were in that state.
		It("reports a fleet-shape memo it evicted", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.fleetShape["ns|m"] = &shapeMemo{lastSeen: time.Now().Add(-pastBoth)}

			ev := a.EvictStaleHistory()

			Expect(a.fleetShape).To(BeEmpty(), "precondition: it was evicted")
			Expect(ev.FleetShapes).To(Equal(1))
			Expect(ev.Any()).To(BeTrue(),
				"an eviction nothing counts makes Any() false, and then the sweep "+
					"deletes state and logs nothing at all")
		})

		It("reports a liveness stamp it evicted", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.variantSeenAt["ns|m|v1|NVIDIA-H200|1"] = time.Now().Add(-pastBoth)

			ev := a.EvictStaleHistory()

			Expect(a.variantSeenAt).To(BeEmpty(), "precondition: it was evicted")
			Expect(ev.VariantStamps).To(Equal(1))
			Expect(ev.Any()).To(BeTrue())
		})
	})

	Describe("the mu window's two side maps", func() {
		// throughputSampledAt and throughputLastRead are written only beside
		// saturatedThroughput[key], so they cannot orphan -- but that is an
		// invariant of one function's body, not of their types, and removing
		// both deletes from the sweep left all 301 specs green. The same shape
		// of hole the startOutliers belt-and-braces loop exists for.
		It("goes with the window, so neither can outlive it", func() {
			a := NewSaturationAnalyzer(capacity.NewStore())
			key := "m|NVIDIA-H200|1|decode|in1k|medium|q10"
			a.recordSaturatedThroughput(key, 3.2)
			Expect(a.saturatedThroughput).To(HaveLen(1))
			Expect(a.throughputSampledAt).To(HaveLen(1), "precondition: the side maps were written")
			Expect(a.throughputLastRead).To(HaveLen(1))

			a.saturatedThroughput[key].TouchAt(time.Now().Add(-pastBoth))
			a.EvictStaleHistory()

			Expect(a.saturatedThroughput).To(BeEmpty())
			Expect(a.throughputSampledAt).To(BeEmpty(),
				"the sample stamp must go with its window, or it is a map nothing bounds")
			Expect(a.throughputLastRead).To(BeEmpty())
		})
	})
})
