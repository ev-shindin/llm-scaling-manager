package saturation

import (
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// An empty ITL window is not evidence that a variant is gone, and the sweep
// used to treat it as exactly that.
//
// Window.Add admits only k in [DefaultMinObservableK, DefaultMaxObservableK].
// A healthy, under-utilised fleet sits BELOW 0.15, offers a reading every
// cycle and holds none -- so its window is permanently empty while the variant
// reports normally. Keying the sweep on Len() == 0 therefore deleted the
// learned start estimate and the learned ITL baseline of the fleets that were
// doing fine, every cycle, and the re-stamp that keeps a live Pod counted then
// stopped noteReplicaStart from putting the estimate back. The figure was gone
// until the Pod was replaced.
//
// What it costs when it goes: the floor loses the landing projection and its
// horizon reverts from max(drain, startSeconds) to drain;
// wva_replica_start_seconds_estimate reports source="seed"; and the next
// pinned-B fit pins to itl.DefaultBaselineSec instead of the measured B, which
// is the direction recorded as collapsing a fleet.
//
// The sweep now ages every map on variantSeenAt -- when the variant was last
// REPORTED -- which is the question it was always asking.
var _ = Describe("the sweep's liveness test", func() {
	const (
		ns      = "ns"
		model   = "m"
		decodeV = "decode-v"
		accel   = "NVIDIA-H200"
	)

	// Below the admissible band, and reported as Ready with a start time: the
	// ordinary state of a fleet with capacity to spare.
	belowBand := func(pod, variant string, startSeconds float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 900_000, tracedKv, 0, 1000, 6000)
		rm.Ready = true
		rm.StartSeconds = startSeconds
		rm.AvgITL = 0.02
		rm.KvUsageInstant = itl.DefaultMinObservableK / 2
		return rm
	}

	It("keeps the learned state of a live variant whose window admits nothing", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		base := time.Now()
		a.now = func() time.Time { return base }
		key := a.itlWindowKey(ns, model, decodeV, accel, 1)
		rms := []domain.ReplicaMetrics{belowBand("pod-a", decodeV, 70)}

		a.noteReplicaStart(key, ns, decodeV, rms, logr.Discard())
		a.noteITL(key, rms, decodeV, base, logr.Discard())
		a.itlBaseline[key] = 0.011 // as an OLS fit would have left it earlier

		measured := a.startSeconds[key]
		Expect(measured).To(BeNumerically(">", 0),
			"the fixture must have produced a start estimate, or this proves nothing")
		Expect(a.itlWindows).To(HaveKey(key))
		Expect(a.itlWindows[key].Len()).To(BeZero(),
			"the fixture must leave the window EMPTY -- that is the case under test")

		// Cycles in realistic steps, each sweeping before the analysis
		// re-stamps, exactly as optimizeV2 orders them.
		step := capacity.HistoryEvictionTimeout / 4
		for i := 1; i <= 10; i++ {
			at := base.Add(time.Duration(i) * step)
			a.now = func() time.Time { return at }
			a.EvictStaleHistory(capacity.HistoryEvictionTimeout)
			a.noteReplicaStart(key, ns, decodeV, rms, logr.Discard())
			a.noteITL(key, rms, decodeV, at, logr.Discard())
		}

		Expect(a.startSeconds).To(HaveKey(key),
			"the start estimate of a reporting variant was swept because its "+
				"window holds nothing -- and the re-stamp means noteReplicaStart "+
				"cannot put it back while the Pod lives")
		Expect(a.startSeconds[key]).To(Equal(measured),
			"and it must be the figure that was measured, not a reseeded one")
		Expect(a.itlBaseline).To(HaveKey(key),
			"the learned baseline would otherwise be replaced by "+
				"itl.DefaultBaselineSec in the next pinned-B fit")
	})

	It("still evicts a variant nothing reports any more", func() {
		// The sweep's own purpose, which the fix must not weaken: the maps
		// keep one entry per variant EVER seen without it.
		a := NewSaturationAnalyzer(capacity.NewStore())
		base := time.Now()
		a.now = func() time.Time { return base }
		key := a.itlWindowKey(ns, model, decodeV, accel, 1)
		rms := []domain.ReplicaMetrics{belowBand("pod-a", decodeV, 70)}

		a.noteReplicaStart(key, ns, decodeV, rms, logr.Discard())
		a.noteITL(key, rms, decodeV, base, logr.Discard())
		a.itlBaseline[key] = 0.011
		Expect(a.startSeconds).To(HaveKey(key))

		// The variant is deleted: nothing reports it again, so nothing
		// re-stamps it.
		gone := base.Add(2 * capacity.HistoryEvictionTimeout)
		a.now = func() time.Time { return gone }
		a.EvictStaleHistory(capacity.HistoryEvictionTimeout)

		Expect(a.startSeconds).NotTo(HaveKey(key))
		Expect(a.startOutliers).NotTo(HaveKey(key))
		Expect(a.itlBaseline).NotTo(HaveKey(key))
		Expect(a.itlWindows).NotTo(HaveKey(key))
		Expect(a.variantSeenAt).NotTo(HaveKey(key),
			"the stamp itself must age out, or it is the unbounded map now")
	})

	It("bounds a role the ITL window never covers", func() {
		// noteITL runs for decode only, so a prefill variant has a start
		// estimate and no window at all. Sweeping these maps THROUGH the
		// window map could never visit such a key: the leak the sweep was
		// wired up to close stayed open for every non-decode role.
		a := NewSaturationAnalyzer(capacity.NewStore())
		base := time.Now()
		a.now = func() time.Time { return base }
		key := a.itlWindowKey(ns, model, "prefill-v", accel, 1)

		a.noteReplicaStart(key, ns, "prefill-v",
			[]domain.ReplicaMetrics{belowBand("pod-p", "prefill-v", 40)}, logr.Discard())
		Expect(a.startSeconds).To(HaveKey(key))
		Expect(a.itlWindows).NotTo(HaveKey(key),
			"a prefill variant has no ITL window, which is the point")

		gone := base.Add(2 * capacity.HistoryEvictionTimeout)
		a.now = func() time.Time { return gone }
		a.EvictStaleHistory(capacity.HistoryEvictionTimeout)

		Expect(a.startSeconds).NotTo(HaveKey(key),
			"a non-decode key has no window to be swept through, so it needs a "+
				"liveness stamp of its own or it is never bounded")
	})
})
