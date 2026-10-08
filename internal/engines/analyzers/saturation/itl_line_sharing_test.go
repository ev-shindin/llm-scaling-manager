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

const (
	keyNS    = "ns"
	keyModel = "Qwen/Qwen3-8B"
	keyAccel = "NVIDIA-H200"
)

// keyParams builds a configuration, varying only max-num-seqs so two of them
// differ in exactly one hashed field.
func keyParams(maxNumSeqs int64) *capacity.EngineParams {
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

// analyzerWithParams puts one record per variant into the store, so the
// fingerprint resolves exactly as it does on the live path.
func analyzerWithParams(byVariant map[string]*capacity.EngineParams) *SaturationAnalyzer {
	store := capacity.NewStore()
	for variant, p := range byVariant {
		store.Update(keyNS, keyModel, variant, capacity.Record{
			AcceleratorName: keyAccel,
			GpuCount:        1,
			EngineParams:    p,
		})
	}
	return NewSaturationAnalyzer(store)
}

// itlPhysicsKey does NOT key the observation window -- each variant fits its
// own. It names the engine CONFIGURATION, and so decides whose fitted line a
// variant with nothing of its own may borrow.
var _ = Describe("itlPhysicsKey", func() {
	key := func(a *SaturationAnalyzer, variant string) string {
		return a.itlPhysicsKey(keyNS, keyModel, variant, keyAccel, 1,
			a.engineFingerprint(keyNS, keyModel, variant))
	}

	It("gives two variants with one configuration the same key", func() {
		a := analyzerWithParams(map[string]*capacity.EngineParams{
			"decode-a": keyParams(256),
			"decode-b": keyParams(256),
		})
		Expect(key(a, "decode-a")).To(Equal(key(a, "decode-b")),
			"the same weights, hardware and flags may share a measured line")
	})

	It("separates variants whose configuration differs in one hashed field", func() {
		a := analyzerWithParams(map[string]*capacity.EngineParams{
			"decode-a": keyParams(256),
			"decode-b": keyParams(512),
		})
		Expect(key(a, "decode-a")).NotTo(Equal(key(a, "decode-b")),
			"--max-num-seqs changes the compute bound and the derived mu")
	})

	It("gives one deployment the same key across namespaces", func() {
		a := analyzerWithParams(map[string]*capacity.EngineParams{"decode-a": keyParams(256)})
		fp := a.engineFingerprint(keyNS, keyModel, "decode-a")
		Expect(fp).NotTo(BeEmpty())
		Expect(a.itlPhysicsKey(keyNS, keyModel, "decode-a", keyAccel, 1, fp)).
			To(Equal(a.itlPhysicsKey("other-ns", keyModel, "decode-a", keyAccel, 1, fp)))
	})

	It("separates two models that share a configuration", func() {
		// ITL's slope is dominated by parameter count, so a 0.6B and a 32B
		// model must never share a line however identical their flags.
		a := analyzerWithParams(map[string]*capacity.EngineParams{"decode-a": keyParams(256)})
		fp := a.engineFingerprint(keyNS, keyModel, "decode-a")
		Expect(a.itlPhysicsKey(keyNS, "Qwen/Qwen3-0.6B", "decode-a", keyAccel, 1, fp)).
			NotTo(Equal(a.itlPhysicsKey(keyNS, "Qwen/Qwen3-32B", "decode-a", keyAccel, 1, fp)))
	})

	It("separates two accelerators and two GPU counts", func() {
		a := analyzerWithParams(map[string]*capacity.EngineParams{"decode-a": keyParams(256)})
		fp := a.engineFingerprint(keyNS, keyModel, "decode-a")
		base := a.itlPhysicsKey(keyNS, keyModel, "decode-a", keyAccel, 1, fp)
		Expect(a.itlPhysicsKey(keyNS, keyModel, "decode-a", "NVIDIA-H100-80GB-HBM3", 1, fp)).
			NotTo(Equal(base))
		Expect(a.itlPhysicsKey(keyNS, keyModel, "decode-a", keyAccel, 2, fp)).NotTo(Equal(base))
	})

	It("shares nothing when the configuration cannot be read", func() {
		// An absent fingerprint means the configuration is unknown, and
		// treating unknown as a shared identity would let engines with nothing
		// in common lend each other lines.
		a := NewSaturationAnalyzer(capacity.NewStore()) // no records at all
		Expect(a.engineFingerprint(keyNS, keyModel, "decode-a")).To(BeEmpty())
		kA := a.itlPhysicsKey(keyNS, keyModel, "decode-a", keyAccel, 1, "")
		kB := a.itlPhysicsKey(keyNS, keyModel, "decode-b", keyAccel, 1, "")
		Expect(kA).NotTo(Equal(kB), "an unknown configuration must not be shared")
		Expect(kA).To(Equal(a.itlWindowKey(keyNS, keyModel, "decode-a", keyAccel, 1)),
			"the fallback is the variant key")
	})
})

// Only A and B ever reach a decision, so what is shared between variants of one
// engine configuration is the fitted LINE, not the observations behind it. An
// earlier version pooled the observations; these specs pin the properties that
// made that the wrong mechanism.
var _ = Describe("borrowing a fitted ITL line", func() {
	reading := func(pod, variant string, k, avgITL float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 900_000, tracedKv, 0, 1000, 6000)
		rm.Ready = true
		rm.KvUsageInstant = k
		rm.AvgITL = avgITL
		return rm
	}

	// tenOn gives ten readings spanning 0.36 of k: DefaultMinSamples is 10 and
	// DefaultMinKSpread is 0.30, so anything less never reaches Ready and an
	// assertion about the fit would hold vacuously.
	tenOn := func(variant string, kStart float64) []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, 10)
		for i := 0; i < 10; i++ {
			k := kStart + 0.04*float64(i)
			out = append(out, reading(fmt.Sprintf("%s-%d", variant, i), variant, k,
				tracedModel.ITLAt(k)))
		}
		return out
	}

	const physics = "shared-config"

	It("lends a measured line to a variant that has none", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		keyA := a.itlWindowKey(keyNS, keyModel, "decode-a", keyAccel, 1)
		keyB := a.itlWindowKey(keyNS, keyModel, "decode-b", keyAccel, 1)

		fitA := a.noteITL(keyA, tenOn("decode-a", 0.20), "decode-a", a.now(), logr.Discard())
		Expect(fitA.IsZero()).To(BeFalse(), "ten readings over 0.36 of k must fit")
		a.publishLine(physics, keyA, fitA, a.now())

		got, from, ok := a.borrowLine(physics, keyB)
		Expect(ok).To(BeTrue(), "B must be able to start from A's line")
		Expect(from).To(Equal(keyA), "the owner is reported so a log can name it")
		Expect(got).To(Equal(fitA))
	})

	It("does not lend a variant its own line", func() {
		// Borrowing is for a variant with nothing of its own. Handing a variant
		// back its OWN line as borrowed would wrongly deny it the right to
		// order on its own measurement.
		a := NewSaturationAnalyzer(capacity.NewStore())
		keyA := a.itlWindowKey(keyNS, keyModel, "decode-a", keyAccel, 1)

		fitA := a.noteITL(keyA, tenOn("decode-a", 0.20), "decode-a", a.now(), logr.Discard())
		a.publishLine(physics, keyA, fitA, a.now())

		_, _, ok := a.borrowLine(physics, keyA)
		Expect(ok).To(BeFalse(), "a variant's own line is not a borrow")
	})

	It("keeps each variant's observations and baseline to itself", func() {
		// The property pooling broke. Two variants with one configuration still
		// fit their own windows, so neither sees the other's readings and
		// neither inherits the other's measured B.
		a := NewSaturationAnalyzer(capacity.NewStore())
		keyA := a.itlWindowKey(keyNS, keyModel, "decode-a", keyAccel, 1)
		keyB := a.itlWindowKey(keyNS, keyModel, "decode-b", keyAccel, 1)

		Expect(a.noteITL(keyA, tenOn("decode-a", 0.20), "decode-a",
			a.now(), logr.Discard()).IsZero()).To(BeFalse())
		Expect(a.noteITL(keyB, tenOn("decode-b", 0.22), "decode-b",
			a.now(), logr.Discard()).IsZero()).To(BeFalse())

		Expect(a.itlWindows).To(HaveLen(2), "one window per variant, not one shared")
		Expect(a.itlWindows[keyA].Len()).To(Equal(10),
			"A's window holds A's ten readings and only those")
		Expect(a.itlWindows[keyB].Len()).To(Equal(10))

		Expect(a.itlBaseline).To(HaveLen(2))
		Expect(a.itlBaseline).To(HaveKey(keyA))
		Expect(a.itlBaseline).To(HaveKey(keyB))
	})

	It("records the measuring variant as the owner, not the borrower", func() {
		// Otherwise a figure circulates between variants gathering authority no
		// measurement gave it.
		a := NewSaturationAnalyzer(capacity.NewStore())
		keyA := a.itlWindowKey(keyNS, keyModel, "decode-a", keyAccel, 1)
		keyB := a.itlWindowKey(keyNS, keyModel, "decode-b", keyAccel, 1)

		fitA := a.noteITL(keyA, tenOn("decode-a", 0.20), "decode-a", a.now(), logr.Discard())
		a.publishLine(physics, keyA, fitA, a.now())

		_, _, ok := a.borrowLine(physics, keyB)
		Expect(ok).To(BeTrue())
		Expect(a.itlLines[physics].fromVariant).To(Equal(keyA),
			"the recorded owner must still be the variant that measured it")
	})

	It("refuses to publish a zero line", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.publishLine(physics, "k", itl.Model{}, a.now())
		Expect(a.itlLines).To(BeEmpty(), "a zero line is the absence of a line")
	})

	It("lends nothing when the configuration is unknown", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.publishLine("", "k", itl.Model{A: 0.03, B: 0.002}, a.now())
		Expect(a.itlLines).To(BeEmpty())
		_, _, ok := a.borrowLine("", "k")
		Expect(ok).To(BeFalse())
	})

	It("ages a shared line out on its own timestamp", func() {
		// The window that produced it may be long gone -- which is the point of
		// keeping it -- so it cannot age by the window's emptiness.
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.publishLine(physics, "owner", itl.Model{A: 0.03, B: 0.002},
			a.now().Add(-2*time.Hour))
		Expect(a.itlLines).To(HaveKey(physics))

		a.EvictStaleHistory(time.Hour)
		Expect(a.itlLines).NotTo(HaveKey(physics))
	})

	It("keeps a line that is still inside the timeout", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.publishLine(physics, "owner", itl.Model{A: 0.03, B: 0.002}, a.now())
		a.EvictStaleHistory(time.Hour)
		Expect(a.itlLines).To(HaveKey(physics), "a fresh line must survive the sweep")
	})
})
