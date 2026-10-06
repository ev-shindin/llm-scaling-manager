package saturation

import (
	"fmt"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The ITL window is keyed by what the line is a function of -- the model, the
// hardware and the engine configuration -- and NOT by namespace or variant
// name. The learned baseline and the replica start estimate stay keyed per
// variant, because a hardware floor and a start time are properties of one
// deployment rather than of a configuration.
var _ = Describe("itlPhysicsKey", func() {
	const (
		ns    = "ns"
		model = "Qwen/Qwen3-8B"
		accel = "NVIDIA-H200"
	)

	// params builds a configuration, varying only max-num-seqs so two of them
	// differ in exactly one hashed field.
	params := func(maxNumSeqs int64) *capacity.EngineParams {
		return &capacity.EngineParams{
			Engine:                    inferenceengine.EngineVLLM,
			GpuMemoryUtilization:      0.9,
			BlockSize:                 16,
			KvCacheDtype:              "auto",
			WeightDtype:               "auto",
			TensorParallelSize:        1,
			MaxNumSeqs:                maxNumSeqs,
			EffectiveMaxBatchedTokens: 8192,
		}
	}

	// analyzerWith puts one record per variant into the store, so the
	// fingerprint resolves exactly as it does on the live path.
	analyzerWith := func(byVariant map[string]*capacity.EngineParams) *SaturationAnalyzer {
		store := capacity.NewStore()
		for variant, p := range byVariant {
			store.Update(ns, model, variant, capacity.Record{
				AcceleratorName: accel,
				GpuCount:        1,
				EngineParams:    p,
			})
		}
		return NewSaturationAnalyzer(store)
	}

	key := func(a *SaturationAnalyzer, variant string) string {
		return a.itlPhysicsKey(ns, model, variant, accel, 1,
			a.engineFingerprint(ns, model, variant))
	}

	It("gives two variants with one configuration the same key", func() {
		a := analyzerWith(map[string]*capacity.EngineParams{
			"decode-a": params(256),
			"decode-b": params(256),
		})
		Expect(key(a, "decode-a")).To(Equal(key(a, "decode-b")),
			"the same weights, hardware and flags are one ITL line")
	})

	It("separates variants whose configuration differs in one hashed field", func() {
		a := analyzerWith(map[string]*capacity.EngineParams{
			"decode-a": params(256),
			"decode-b": params(512),
		})
		Expect(key(a, "decode-a")).NotTo(Equal(key(a, "decode-b")),
			"--max-num-seqs changes the compute bound and the derived mu")
	})

	It("gives one deployment the same key across namespaces", func() {
		// The prize: the same deployment under another namespace relearns
		// nothing. The key carries no namespace.
		a := analyzerWith(map[string]*capacity.EngineParams{"decode-a": params(256)})
		fp := a.engineFingerprint(ns, model, "decode-a")
		Expect(fp).NotTo(BeEmpty())
		here := a.itlPhysicsKey(ns, model, "decode-a", accel, 1, fp)
		there := a.itlPhysicsKey("other-ns", model, "decode-a", accel, 1, fp)
		Expect(here).To(Equal(there))
	})

	It("separates two models that share a configuration", func() {
		// The worst mis-keying available here: ITL's slope is dominated by
		// parameter count, so a 0.6B and a 32B model must never share a line
		// however identical their flags.
		a := analyzerWith(map[string]*capacity.EngineParams{"decode-a": params(256)})
		fp := a.engineFingerprint(ns, model, "decode-a")
		small := a.itlPhysicsKey(ns, "Qwen/Qwen3-0.6B", "decode-a", accel, 1, fp)
		large := a.itlPhysicsKey(ns, "Qwen/Qwen3-32B", "decode-a", accel, 1, fp)
		Expect(small).NotTo(Equal(large))
	})

	It("separates two accelerators and two GPU counts", func() {
		a := analyzerWith(map[string]*capacity.EngineParams{"decode-a": params(256)})
		fp := a.engineFingerprint(ns, model, "decode-a")
		base := a.itlPhysicsKey(ns, model, "decode-a", accel, 1, fp)
		Expect(a.itlPhysicsKey(ns, model, "decode-a", "NVIDIA-H100-80GB-HBM3", 1, fp)).
			NotTo(Equal(base))
		Expect(a.itlPhysicsKey(ns, model, "decode-a", accel, 2, fp)).NotTo(Equal(base))
	})

	It("pools nothing when the configuration cannot be read", func() {
		// An absent fingerprint means the configuration is unknown, and
		// treating unknown as a shared identity would pool engines with
		// nothing in common. Falling back to the variant key pays the fit
		// again, which is the lesser cost by a wide margin.
		a := NewSaturationAnalyzer(capacity.NewStore()) // no records at all
		Expect(a.engineFingerprint(ns, model, "decode-a")).To(BeEmpty())
		kA := a.itlPhysicsKey(ns, model, "decode-a", accel, 1, "")
		kB := a.itlPhysicsKey(ns, model, "decode-b", accel, 1, "")
		Expect(kA).NotTo(Equal(kB), "an unknown configuration must not pool")
		Expect(kA).To(Equal(a.itlWindowKey(ns, model, "decode-a", accel, 1)),
			"the fallback is the variant key")
	})
})

var _ = Describe("the pooled ITL window", func() {
	const (
		ns    = "ns"
		model = "Qwen/Qwen3-8B"
		accel = "NVIDIA-H200"
	)

	reading := func(pod, variant string, k, avgITL float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 900_000, tracedKv, 0, 1000, 6000)
		rm.Ready = true
		rm.KvUsageInstant = k
		rm.AvgITL = avgITL
		return rm
	}

	// tenOn returns ten readings for one variant on the traced line, spanning
	// 0.36 of k.
	//
	// Both numbers are load-bearing and an earlier version of this file got
	// them wrong: itl.DefaultMinSamples is 10 and itl.DefaultMinKSpread is
	// 0.30, so five readings over 0.08 left the window never Ready, no OLS fit
	// ran, and the baseline assertions below passed against an EMPTY map --
	// they held with the baseline deliberately mis-keyed.
	tenOn := func(variant string, kStart float64) []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, 10)
		for i := 0; i < 10; i++ {
			k := kStart + 0.04*float64(i)
			out = append(out, reading(fmt.Sprintf("%s-%d", variant, i), variant, k,
				tracedModel.ITLAt(k)))
		}
		return out
	}

	It("holds both variants' readings in one window, and keeps the baseline apart", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		shared := "shared-window-key"
		keyA := a.itlWindowKey(ns, model, "decode-a", accel, 1)
		keyB := a.itlWindowKey(ns, model, "decode-b", accel, 1)
		Expect(keyA).NotTo(Equal(keyB))

		contribA := a.noteITLContributor(shared, ns, "decode-a", a.now())
		Expect(contribA).To(Equal(1))
		fitA := a.noteITL(shared, keyA, contribA, tenOn("decode-a", 0.20), "decode-a",
			a.now(), logr.Discard())
		Expect(fitA.IsZero()).To(BeFalse(), "ten readings over 0.36 of k must fit")

		contribB := a.noteITLContributor(shared, ns, "decode-b", a.now())
		Expect(contribB).To(Equal(2), "a second variant feeding the same window")
		fitB := a.noteITL(shared, keyB, contribB, tenOn("decode-b", 0.22), "decode-b",
			a.now(), logr.Discard())
		Expect(fitB.IsZero()).To(BeFalse())

		Expect(a.itlWindows).To(HaveLen(1), "one window, not one per variant")
		Expect(a.itlWindows[shared].Len()).To(Equal(20),
			"both variants' readings are in it, so the fit sees twenty and not ten")

		// The baseline is per variant. Pooling it would let one variant's
		// measured floor set the steepness of every sibling's pinned fit.
		// Asserted on the CONTENTS, not just on an absence, so the map being
		// empty cannot pass for separation.
		Expect(a.itlBaseline).To(HaveLen(2),
			"one learned baseline per variant, not one per shared window")
		Expect(a.itlBaseline).To(HaveKey(keyA))
		Expect(a.itlBaseline).To(HaveKey(keyB))
		Expect(a.itlBaseline).NotTo(HaveKey(shared),
			"the baseline must not be filed under the shared window key")
	})

	It("grows the window so pooling does not shorten each contributor's history", func() {
		// Add evicts the oldest at capacity whoever contributed it, so four
		// variants on a 20-slot window would hold five cycles each.
		a := NewSaturationAnalyzer(capacity.NewStore())
		shared := "shared"
		for i, variant := range []string{"v1", "v2", "v3", "v4"} {
			n := a.noteITLContributor(shared, ns, variant, a.now())
			Expect(n).To(Equal(i + 1))
			a.noteITL(shared, a.itlWindowKey(ns, model, variant, accel, 1), n,
				tenOn(variant, 0.20+0.01*float64(i)), variant, a.now(), logr.Discard())
		}
		Expect(a.itlWindows[shared].MaxSize()).To(Equal(itl.DefaultWindowMaxSize*4),
			"capacity scales with the number of contributors")
	})

	It("counts one deployment in two namespaces as two contributors", func() {
		// The physics key drops the namespace, which is the pooling this
		// exists to make correct -- so both must be counted.
		a := NewSaturationAnalyzer(capacity.NewStore())
		Expect(a.noteITLContributor("k", "ns-a", "decode", a.now())).To(Equal(1))
		Expect(a.noteITLContributor("k", "ns-b", "decode", a.now())).To(Equal(2))
		Expect(a.noteITLContributor("k", "ns-a", "decode", a.now())).To(Equal(2),
			"the same contributor twice is still one")
	})
})

var _ = Describe("EvictStaleHistory with split keys", func() {
	const (
		ns    = "ns"
		model = "m"
		accel = "NVIDIA-H200"
	)

	It("ages the variant-keyed state on the variant's own last-seen", func() {
		// The window's emptiness used to evict these, because they shared its
		// key. They no longer do, so they need a timestamp of their own --
		// otherwise a learned hardware floor would pin a figure measured
		// before a redeploy into every later fit, which is what the old
		// comment here warned about.
		a := NewSaturationAnalyzer(capacity.NewStore())
		key := a.itlWindowKey(ns, model, "decode-a", accel, 1)

		a.itlBaseline[key] = 0.011
		a.startSeconds[key] = 67
		a.startOutliers[key] = 2
		a.noteVariantSeen(key, a.now())

		Expect(a.EvictStaleHistory(time.Hour)).To(Equal(0))
		Expect(a.itlBaseline).To(HaveKey(key), "a variant seen this cycle keeps its state")
		Expect(a.startSeconds).To(HaveKey(key))

		// Seen two hours ago, swept on a one-hour timeout.
		a.noteVariantSeen(key, a.now().Add(-2*time.Hour))
		a.EvictStaleHistory(time.Hour)
		Expect(a.itlBaseline).NotTo(HaveKey(key))
		Expect(a.startSeconds).NotTo(HaveKey(key))
		Expect(a.startOutliers).NotTo(HaveKey(key))
		Expect(a.variantSeenAt).NotTo(HaveKey(key))
	})

	It("forgets a contributor that stopped feeding a window that is still alive", func() {
		// The leak the first version had: the contributor set was only ever
		// cleared wholesale when its window went empty, so a window kept alive
		// by one variant remembered every variant that had ever shared it --
		// and len(set) is what GrowMaxSize scales by, so a dead contributor
		// permanently inflated a live window's capacity.
		a := NewSaturationAnalyzer(capacity.NewStore())
		const shared = "physics"
		now := a.now()

		Expect(a.noteITLContributor(shared, ns, "decode-a", now.Add(-2*time.Hour))).To(Equal(1))
		Expect(a.noteITLContributor(shared, ns, "decode-b", now)).To(Equal(2))

		// Keep the window alive: a non-empty window must NOT be deleted, or
		// this test would pass for the wrong reason.
		w := itl.NewWindow(itl.DefaultWindowMaxSize, itl.DefaultObservationMaxAge,
			itl.DefaultMinSamples, itl.DefaultMinKSpread,
			itl.DefaultMinObservableK, itl.DefaultMaxObservableK)
		Expect(w.Add(0.30, 0.01, now)).To(BeFalse())
		a.itlWindows[shared] = w

		a.EvictStaleHistory(time.Hour)

		Expect(a.itlWindows).To(HaveKey(shared), "the window itself must survive")
		Expect(a.itlContributors[shared]).To(HaveLen(1),
			"the contributor last seen two hours ago must be gone")
		Expect(a.itlContributors[shared]).To(HaveKey(ns + "|decode-b"))
		Expect(a.itlContributors[shared]).NotTo(HaveKey(ns + "|decode-a"))

		// And the count that drives GrowMaxSize is now the LIVE count.
		Expect(a.noteITLContributor(shared, ns, "decode-b", now)).To(Equal(1),
			"growth must be driven by live contributors, not by history")
	})

	It("drops an empty contributor set so the outer map does not leak keys", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		now := a.now()
		a.noteITLContributor("gone", ns, "decode-a", now.Add(-2*time.Hour))
		// No window for this key at all, so only the contributor sweep can
		// remove it.
		a.EvictStaleHistory(time.Hour)
		Expect(a.itlContributors).NotTo(HaveKey("gone"))
	})

	It("drops a pooled window's contributor set with the window", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteITLContributor("physics", ns, "decode-a", a.now())
		a.itlWindows["physics"] = itl.NewWindow(
			itl.DefaultWindowMaxSize, itl.DefaultObservationMaxAge,
			itl.DefaultMinSamples, itl.DefaultMinKSpread,
			itl.DefaultMinObservableK, itl.DefaultMaxObservableK)

		// An empty window is one whose observations have all aged out.
		a.EvictStaleHistory(time.Hour)
		Expect(a.itlWindows).NotTo(HaveKey("physics"))
		Expect(a.itlContributors).NotTo(HaveKey("physics"),
			"the contributor set would otherwise outlive every window it describes")
	})
})
