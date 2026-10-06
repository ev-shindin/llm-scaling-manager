package saturation

import (
	"context"
	"fmt"
	"slices"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// The fit the 2026-09-23 shape-swap runs were taken on, and the card they ran
// on: ITL(k) = 30.7 ms*k + 1.6 ms over 70 intervals, 1,163,136 KV tokens,
// max_num_seqs 256.
var (
	tracedModel  = itl.Model{A: 0.0307, B: 0.0016}
	tracedParams = &capacity.EngineParams{MaxNumSeqs: 256}
	tracedKv     = int64(1_163_136)
	// The operating point the analyzer targets: k1 is KvCacheThreshold of
	// physical KV and it scales out at ScaleUpThreshold of k1, so mu is priced
	// at the product -- 0.68 on the defaults, not 0.85.
	tracedK = config.DefaultKvCacheThreshold * config.DefaultScaleUpThreshold
)

// The fit from run P (2026-09-27), the first pass of this trace where the fleet
// was never short of replicas: ITL(k) = 34.4 ms*k + 0.61 ms over the 88
// intervals with k in [0.15, 0.75], mean residual 6.4%. Same card as the
// traced runs above: 1,163,136 KV tokens, max_num_seqs 256.
var runPModel = itl.Model{A: 0.0344, B: 0.00061}

var _ = Describe("pricingK", func() {
	It("is the two thresholds multiplied, because they compound", func() {
		// k1 is KvCacheThreshold of physical KV and the analyzer scales out at
		// ScaleUpThreshold of k1, so a replica is full at the product.
		Expect(pricingK(&config.ScalingPolicy{
			KvCacheThreshold: 0.80, ScaleUpThreshold: 0.85,
		})).To(BeNumerically("~", 0.68, 1e-9))

		Expect(pricingK(&config.ScalingPolicy{
			KvCacheThreshold: 0.80, ScaleUpThreshold: 0.95,
		})).To(BeNumerically("~", 0.76, 1e-9))
	})

	It("stays inside the range the ITL line was fitted over", func() {
		// The line says nothing outside the window's bounds, so a configuration
		// whose product falls outside them is clamped rather than extrapolated.
		Expect(pricingK(&config.ScalingPolicy{
			KvCacheThreshold: 1.0, ScaleUpThreshold: 1.0,
		})).To(Equal(itl.DefaultMaxObservableK))

		Expect(pricingK(&config.ScalingPolicy{
			KvCacheThreshold: 0.1, ScaleUpThreshold: 0.1,
		})).To(Equal(itl.DefaultMinObservableK))

		// Including the fallback: DefaultKSat is 0.85 and the window ends at
		// 0.80, so returning it unclamped would evaluate the line outside the
		// range it was fitted over -- what this function exists to prevent.
		Expect(pricingK(nil)).To(Equal(itl.DefaultMaxObservableK))
	})
})

var _ = Describe("deriveMu against run P", func() {
	const kRunP = 0.80 * 0.85

	It("agrees with the mu the fleet measured, where it measured one", func() {
		// Phase 1 saturated, so the floor had a reading of its own to compare
		// against: it used 1.27 requests a second for the whole phase.
		got := deriveMu(runPModel, tracedParams, tracedKv, shape.New(1000, 6000, 0), kRunP, 6000)
		Expect(got.ok).To(BeTrue())
		Expect(got.rate).To(BeNumerically("~", 1.27, 0.15),
			"a derived mu that disagrees with a measured one is not describing this card")
	})

	It("tracks the shape change the measured mu could not", func() {
		// Phase 2 is 8000/1000: six times shorter generations, so roughly six
		// times the request rate per replica. The floor went on using 1.31 --
		// the phase 1 figure -- for all nineteen minutes, and held 6-7 replicas
		// at 1.2% KV utilisation and an empty queue.
		got := deriveMu(runPModel, tracedParams, tracedKv, shape.New(8000, 1000, 0), kRunP, 1000)
		Expect(got.ok).To(BeTrue())
		Expect(got.rate).To(BeNumerically("~", 3.89, 0.2))

		const measuredArrival = 5.86
		Expect(measuredArrival/got.rate).To(BeNumerically("<", 2.0),
			"the arrival rate over a shape-correct mu is a small fleet")
		Expect(measuredArrival/1.31).To(BeNumerically(">", 4.0),
			"where the stale figure ordered four and a half replicas of capacity")
	})
})

var _ = Describe("deriveMu", func() {
	It("reproduces the mu the fleet measured for itself under the first shape", func() {
		// 1000-token prompts, 6000-token generations. Saturated, the fleet
		// recorded 1.4292 and 1.5429 requests a second across the two runs.
		got := deriveMu(tracedModel, tracedParams, tracedKv, shape.New(1000, 6000, 0), tracedK, 6000)
		Expect(got.ok).To(BeTrue())
		Expect(got.rate).To(BeNumerically("~", 1.49, 0.12),
			"the model has to land where the hardware did, or it is not describing it")
	})

	It("prices the second shape, which the fleet never measured at all", func() {
		// 8000-token prompts, 1000-token generations. The fleet was
		// over-provisioned for this and never saturated under it, so no reading
		// was ever taken -- the floor went on using the first shape's figure
		// for the remaining nineteen minutes of the run.
		got := deriveMu(tracedModel, tracedParams, tracedKv, shape.New(8000, 1000, 0), tracedK, 1000)
		Expect(got.ok).To(BeTrue())
		Expect(got.rate).To(BeNumerically(">", 3.0))
		Expect(6.0/got.rate).To(BeNumerically("<", 2.5),
			"the arrival rate over a shape-correct mu is a small fleet, not the seven that ran")
	})

	It("prices a fully cached workload, whose prompts occupy nothing", func() {
		// ILeff = IL*(1 - hitRate), so a workload served entirely from the
		// prefix cache has an effective prompt length of zero and a footprint
		// of OL/2 alone. That is a real shape, not a missing one, and it holds
		// more sequences per replica than the same workload uncached.
		cached := deriveMu(tracedModel, tracedParams, tracedKv, shape.New(1000, 6000, 1.0), tracedK, 6000)
		plain := deriveMu(tracedModel, tracedParams, tracedKv, shape.New(1000, 6000, 0), tracedK, 6000)
		Expect(cached.ok).To(BeTrue())
		Expect(cached.seqs).To(BeNumerically(">", plain.seqs),
			"a cached prompt leaves room for more resident sequences")
		Expect(cached.rate).To(BeNumerically(">", plain.rate))
	})

	It("divides by the divisor it is given, not by the shape's own output length", func() {
		// The whole point of the parameter. mu = seqs / (ITL * OL), and the two
		// output lengths differ for the few minutes after a shape change: the
		// shape carries the [5m] count-weighted mean, which decayed
		// 3750 -> 2814 -> 2382 -> 2278 -> 2136 -> 250 per decode pod across one
		// 6000 -> 250 switch, while the arriving work was 250 throughout.
		//
		// Measured consequence of getting it wrong: a derived mu of 1.399 req/s
		// and a floor asking 11.56 replicas where the demand implied 1.
		fleet := shape.New(20000, 250, 0)

		stale := deriveMu(tracedModel, tracedParams, tracedKv, fleet, tracedK, 1000)
		fresh := deriveMu(tracedModel, tracedParams, tracedKv, fleet, tracedK, 250)
		Expect(stale.ok).To(BeTrue())
		Expect(fresh.ok).To(BeTrue())

		// Only the divisor moved, so seqs and the token rate are identical and
		// the rates differ by exactly the ratio of the two divisors.
		Expect(fresh.seqs).To(Equal(stale.seqs), "seqs must not depend on the divisor")
		Expect(fresh.tokenSec).To(Equal(stale.tokenSec), "nor the token rate")
		Expect(fresh.rate).To(BeNumerically("~", stale.rate*4, 1e-9),
			"1000/250 = 4x: a divisor 4x too high understates mu 4x, and the floor divides lambda by it")
	})

	It("declines when the divisor is absent, rather than dividing by the shape", func() {
		// A zero divisor must not silently fall back INSIDE deriveMu: the
		// caller is the one that knows whether the short window was empty
		// because the engine does not publish it or because nothing completed,
		// and it substitutes the [5m] figure there (fleetOutputLengthRecent).
		// Deciding it here as well would make the fallback invisible.
		got := deriveMu(tracedModel, tracedParams, tracedKv, shape.New(20000, 250, 0), tracedK, 0)
		Expect(got.ok).To(BeFalse(), "no divisor is no derived mu")
	})

	It("is capped by max_num_seqs, which the trace's second phase sat on", func() {
		// The cache alone implies thousands of resident sequences for a short
		// shape; the engine admits 256, and phase 2 ran at exactly that. The
		// uncapped figure is asserted against the arithmetic rather than a
		// constant, because it moves with the pricing point: it was ~6,591 when
		// mu was priced at 0.85 of physical KV and is ~5,273 at the 0.68 the
		// analyzer actually targets.
		got := deriveMu(tracedModel, tracedParams, tracedKv, shape.New(100, 100, 0), tracedK, 100)
		Expect(got.ok).To(BeTrue())
		Expect(got.seqs).To(Equal(float64(tracedParams.MaxNumSeqs)))

		uncapped := deriveMu(tracedModel, &capacity.EngineParams{}, tracedKv, shape.New(100, 100, 0), tracedK, 100)
		Expect(uncapped.seqs).To(BeNumerically("~", tracedK*float64(tracedKv)/150, 1),
			"uncapped, it is the cache's own number: kPrice x C / KVreq")
		Expect(uncapped.seqs).To(BeNumerically(">", 20*float64(tracedParams.MaxNumSeqs)),
			"and that is a number the engine will not admit")
	})

	// The same mu, priced on one window or two. A swap that raises the
	// generation and drops the prompt has the [1m] divisor arrive before the
	// [5m] shape does, and the mixed pair prices a request larger than either
	// real shape -- fewer sequences in the cache, a lower mu, more replicas
	// ordered. The run is in analyzer.go's muInput comment.
	It("prices a swap above the mixed-window pair", func() {
		const arrivingOutput = 4000
		mixed := deriveMu(tracedModel, tracedParams, tracedKv,
			shape.New(5429, 1800, 0), tracedK, arrivingOutput)
		paired := deriveMu(tracedModel, tracedParams, tracedKv,
			shape.New(1000, arrivingOutput, 0), tracedK, arrivingOutput)

		Expect(mixed.ok).To(BeTrue())
		Expect(paired.ok).To(BeTrue())
		Expect(paired.rate).To(BeNumerically(">", mixed.rate),
			"the paired shape fits more sequences, so one replica completes more per second")
		Expect(paired.tokenSec).To(BeNumerically(">", mixed.tokenSec),
			"and the gain is in the sequence count, not the divisor -- both divide by 4000")
	})

	It("declines rather than guessing", func() {
		Expect(deriveMu(itl.Model{}, tracedParams, tracedKv, shape.New(1000, 6000, 0), tracedK, 6000).ok).To(BeFalse(),
			"no fitted model")
		Expect(deriveMu(tracedModel, tracedParams, 0, shape.New(1000, 6000, 0), tracedK, 6000).ok).To(BeFalse(),
			"no KV capacity")
		Expect(deriveMu(tracedModel, tracedParams, tracedKv, shape.New(1000, 0, 0), tracedK, 0).ok).To(BeFalse(),
			"no generation length is not a decode shape")
		Expect(deriveMu(tracedModel, tracedParams, tracedKv, shape.New(1000, 6000, 0), 0, 6000).ok).To(BeFalse(),
			"a kPrice of zero is not a utilization")
		Expect(deriveMu(tracedModel, tracedParams, tracedKv, shape.New(1000, 6000, 0), -0.1, 6000).ok).To(BeFalse(),
			"nor is a negative one")
		Expect(deriveMu(tracedModel, tracedParams, tracedKv, shape.New(1000, 6000, 0), 1.01, 6000).ok).To(BeFalse(),
			"nor is one above full")
		Expect(deriveMu(tracedModel, tracedParams, tracedKv, shape.Shape{}, tracedK, 0).ok).To(BeFalse(),
			"an empty shape has no footprint to divide the cache by")
		Expect(deriveMu(itl.Model{A: -1, B: 0.5}, tracedParams, tracedKv, shape.New(1000, 6000, 0), tracedK, 6000).ok).To(BeFalse(),
			"a model whose reading at k_sat is not positive")
	})
})

var _ = Describe("the floor's mu, through Analyze", func() {
	const variant = "decode-v"

	// A fleet whose replicas sit at a spread of loads, each reporting the ITL
	// the traced model predicts for its own k, so the fit recovers that model.
	// Ten of them clears the window's sample count and its k-spread in one
	// cycle.
	fleet := func(n int, avgIn, avgOut float64, queue int, tokens int64) domain.AnalyzerInput {
		rms := make([]domain.ReplicaMetrics, 0, n)
		for i := 0; i < n; i++ {
			k := 0.20 + 0.06*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("d%d", i), variant, tokens, tracedKv, queue, avgIn, avgOut)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			rm.RequestRate = 0.6
			rm.GenerationTokenRate = rm.RequestRate * avgOut
			rms = append(rms, rm)
		}
		in := makeAnalyzerInput(rms, []domain.VariantReplicaState{
			{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: "H200",
				CurrentReplicas: n, GPUsPerReplica: 1},
		})
		in.ArrivalRate = 6
		return in
	}

	// What the floor asked for, in replicas: the role's demand over what one
	// replica of it supplies.
	impliedReplicas := func(res *domain.AnalyzerResult) float64 {
		var perReplica float64
		for _, vc := range res.VariantCapacities {
			if vc.VariantName == variant {
				perReplica = vc.PerReplicaCapacity
			}
		}
		Expect(perReplica).To(BeNumerically(">", 0))
		return res.RoleDemand[domain.RoleDecode] / perReplica
	}

	It("prices the shape arriving now, on the first cycle it is seen", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		ctx := context.Background()

		// One cycle of the first shape, saturated, is enough to fit ITL(k) --
		// ten replicas at a spread of loads clears the window in one go.
		_, err := a.Analyze(ctx, fleet(10, 1000, 6000, 10, 900_000))
		Expect(err).NotTo(HaveOccurred())

		// The shape swaps to short generations and long prompts, and the fleet
		// is now far too big for it: nothing is queued and occupancy has
		// collapsed. On the measured path nothing would saturate again, so the
		// floor would go on pricing 6 req/s against the 6000-token figure for
		// the rest of the run -- which is what the fleet did on 2026-09-23,
		// holding seven replicas against a utilization of 0.028.
		res, err := a.Analyze(ctx, fleet(10, 8000, 1000, 0, 17_282))
		Expect(err).NotTo(HaveOccurred())

		Expect(impliedReplicas(res)).To(BeNumerically("<", 2.5),
			"a mu derived for the shape now arriving asks for one or two replicas")
	})

	It("carries the fit across a shape change instead of refitting", func() {
		// The window is not cleared when the shape moves, and this is the spec
		// that holds it to that: cycle 2 brings only three replicas, which on
		// their own are short of DefaultMinSamples and of the k-spread, so a
		// model can only exist here if cycle 1's readings are still in the
		// window. ITL(k) describes the hardware, not the shape, so they are.
		a := NewSaturationAnalyzer(capacity.NewStore())
		ctx := context.Background()

		_, err := a.Analyze(ctx, fleet(10, 1000, 6000, 10, 900_000))
		Expect(err).NotTo(HaveOccurred())

		res, err := a.Analyze(ctx, fleet(3, 8000, 1000, 0, 17_282))
		Expect(err).NotTo(HaveOccurred())
		Expect(impliedReplicas(res)).To(BeNumerically("<", 2.5),
			"three fresh readings cannot fit a model; the carried-over ones can")
	})

	It("falls back to the measured window when no model can be fitted", func() {
		// Ten replicas, so the sample count is not what stops it -- every one
		// of them reports no inter-token latency, which is the exclusion under
		// test. The floor must still bind, on the measured reading, rather than
		// the role quietly losing its floor.
		a := NewSaturationAnalyzer(capacity.NewStore())
		in := fleet(10, 1000, 6000, 10, 900_000)
		for i := range in.ReplicaMetrics {
			in.ReplicaMetrics[i].AvgITL = 0
		}
		res, err := a.Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())

		// The measured mu here is the fleet's own completion rate, 0.6 req/s
		// against 6 arriving, so the floor asks for far more than the derived
		// path would -- which is the point: this is the old answer, and it is
		// still available when the new one is not.
		Expect(impliedReplicas(res)).To(BeNumerically(">", 2.5),
			"with no model the floor is back on the measured reading")
	})
})

var _ = Describe("noteITL", func() {
	const variant = "decode-v"

	reading := func(pod string, k, avgITL float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 900_000, tracedKv, 0, 1000, 6000)
		rm.Ready = true
		rm.KvUsageInstant = k
		rm.AvgITL = avgITL
		return rm
	}
	// Ten readings on the traced line, enough to fit it.
	line := func() []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, 10)
		for i := 0; i < 10; i++ {
			k := 0.20 + 0.06*float64(i)
			out = append(out, reading(fmt.Sprintf("d%d", i), k, tracedModel.ITLAt(k)))
		}
		return out
	}
	fit := func(rms []domain.ReplicaMetrics) itl.Model {
		a := NewSaturationAnalyzer(capacity.NewStore())
		return a.noteITL("ns|model|"+variant, "ns|model|"+variant, 1, rms, variant, a.now(), logr.Discard())
	}

	It("fits the line its replicas are reporting", func() {
		got := fit(line())
		Expect(got.IsZero()).To(BeFalse())
		Expect(got.A).To(BeNumerically("~", tracedModel.A, 1e-6))
		Expect(got.B).To(BeNumerically("~", tracedModel.B, 1e-6))
	})

	DescribeTable("leaves out what is not a reading of this variant's own replicas",
		func(spoil func(*domain.ReplicaMetrics)) {
			rms := line()
			for i := range rms {
				spoil(&rms[i])
			}
			Expect(fit(rms).IsZero()).To(BeTrue())
		},
		Entry("another variant's", func(rm *domain.ReplicaMetrics) { rm.VariantName = "other-v" }),
		Entry("a pod still failing readiness", func(rm *domain.ReplicaMetrics) { rm.Ready = false }),
		Entry("a warm-pool bridge on the pool's own settings", func(rm *domain.ReplicaMetrics) { rm.FromWarmPool = true }),
		Entry("no inter-token latency", func(rm *domain.ReplicaMetrics) { rm.AvgITL = 0 }),
		Entry("no utilization to place it at", func(rm *domain.ReplicaMetrics) { rm.KvUsageInstant = 0 }),
		Entry("a load above the band the line was fitted in", func(rm *domain.ReplicaMetrics) {
			rm.KvUsageInstant = itl.DefaultMaxObservableK + 0.05
		}),
	)

	It("fits a balanced fleet, which is the fleet a router produces", func() {
		// Every replica at the same load and the same latency: no spread in k,
		// so the window is never Ready and the two-parameter fit has nothing to
		// work with. This is not a corner case -- it is what a run on a working
		// fleet looks like, and requiring the full fit left the derivation
		// inert on 2026-09-24 with every replica at queue 0.
		rms := make([]domain.ReplicaMetrics, 0, 6)
		for i := 0; i < 6; i++ {
			rms = append(rms, reading(fmt.Sprintf("d%d", i), 0.62, 0.0272))
		}
		// The SPREAD requirement is what a balanced fleet cannot meet and is
		// waived; the sample floor is not, so one cycle of six is not enough
		// and two are. Driven through one analyzer, because the window is what
		// accumulates and `fit` builds a fresh one each call.
		a := NewSaturationAnalyzer(capacity.NewStore())
		key := "ns|model|" + variant
		Expect(a.noteITL(key, key, 1, rms, variant, a.now(), logr.Discard()).IsZero()).To(BeTrue(),
			"six readings is under DefaultMinSamples: nothing is derived yet")
		got := a.noteITL(key, key, 1, rms, variant, a.now(), logr.Discard())
		Expect(got.IsZero()).To(BeFalse(),
			"a balanced fleet still gets a model, with B pinned")
		Expect(got.B).To(Equal(itl.DefaultBaselineSec))
		Expect(got.ITLAt(0.62)).To(BeNumerically("~", 0.0272, 1e-9),
			"and it passes through the load the fleet is actually at")
	})

	It("keeps the good readings when only some are excluded", func() {
		rms := line()
		rms = append(rms, reading("warm", 0.5, 0.02))
		rms[len(rms)-1].FromWarmPool = true
		rms = append(rms, reading("other", 0.5, 0.02))
		rms[len(rms)-1].VariantName = "other-v"

		got := fit(rms)
		Expect(got.IsZero()).To(BeFalse())
		Expect(got.A).To(BeNumerically("~", tracedModel.A, 1e-6),
			"the excluded readings are off the line and would drag the fit")
	})
})

var _ = Describe("useDerived", func() {
	const enough = floor.MinThroughputSamplesToOrder

	It("prices a shape the fleet has never measured", func() {
		// Nothing recorded under this key at all: the borrowed reading is the
		// neighbouring bucket's, which is the stale figure the derivation
		// exists to replace. On run R that figure was six times wrong.
		Expect(useDerived(true, throughputReading{rate: 1.31, bucket: "xxlong",
			samples: enough, borrowed: true})).To(BeTrue())
	})

	It("yields to the fleet's own measurement of this shape", func() {
		// Letting derived win here is what run R cost: a measured 1.27 req/s
		// was replaced by a derived 0.67, and the floor asked for 42-56
		// replicas where main asked for 4.5.
		Expect(useDerived(true, throughputReading{rate: 1.27, bucket: "xxlong",
			samples: enough})).To(BeFalse())
	})

	It("does not yield to a reading too thin to order on", func() {
		Expect(useDerived(true, throughputReading{rate: 1.27, bucket: "xxlong",
			samples: enough - 1})).To(BeTrue())
	})

	It("does not yield to a zero rate", func() {
		Expect(useDerived(true, throughputReading{rate: 0, bucket: "xxlong",
			samples: enough})).To(BeTrue())
	})

	It("stays out of the way when nothing was derived", func() {
		Expect(useDerived(false, throughputReading{rate: 1.27, bucket: "xxlong",
			samples: enough})).To(BeFalse())
		Expect(useDerived(false, throughputReading{borrowed: true})).To(BeFalse())
	})
})

var _ = Describe("throughputKey", func() {
	a := NewSaturationAnalyzer(capacity.NewStore())
	const (
		model = "m"
		ns    = "n"
		v     = "decode-v"
		accel = "H200"
	)

	It("separates two shapes that differ only in prompt length", func() {
		// historyKey carries the OUTPUT bucket only. Without an input
		// dimension an input-only shape change -- prompts 1000 -> 8000,
		// generations unchanged -- left the key byte-identical, so the window
		// recorded under the old prompts read back as an OWN, non-borrowed
		// reading and useDerived handed it precedence over a derived figure
		// that had priced the change correctly through KVreq = ILeff + OL/2.
		short := a.throughputKey(model, ns, v, accel, 1, domain.RoleDecode, 1000, 6000, 5)
		long := a.throughputKey(model, ns, v, accel, 1, domain.RoleDecode, 8000, 6000, 5)
		Expect(short).NotTo(Equal(long))
	})

	It("keeps one key while only the prompt length wobbles inside a bucket", func() {
		// The counterpart risk: a key that moved on every jitter would split
		// one window in two. The caller passes the TRACKED input for this
		// reason, and the bucket boundaries absorb the rest.
		Expect(a.throughputKey(model, ns, v, accel, 1, domain.RoleDecode, 6100, 6000, 5)).
			To(Equal(a.throughputKey(model, ns, v, accel, 1, domain.RoleDecode, 9000, 6000, 5)))
	})

	It("still separates two shapes that differ only in generation length", func() {
		Expect(a.throughputKey(model, ns, v, accel, 1, domain.RoleDecode, 1000, 6000, 5)).
			NotTo(Equal(a.throughputKey(model, ns, v, accel, 1, domain.RoleDecode, 1000, 1000, 5)))
	})

	// Prefill completes a request after one token and hands the KV to decode,
	// so the answer's length is not work it does. Keying its throughput by the
	// fleet's output length split one population of readings across unrelated
	// buckets: measured on the 1k/6000 -> 30k/250 trace, prefill walked
	// xlong -> xxlong -> xlong -> medium inside a single phase whose prompt
	// length never moved, re-learning mu in each and being held to one replica
	// per cycle while the floor asked for nine.
	It("keys prefill by prompt length alone, whatever the fleet is generating", func() {
		const pv = "prefill-v"
		Expect(a.throughputKey(model, ns, pv, accel, 1, domain.RolePrefill, 30000, 6000, 5)).
			To(Equal(a.throughputKey(model, ns, pv, accel, 1, domain.RolePrefill, 30000, 250, 5)))
	})

	It("still separates two prefill shapes that differ in prompt length", func() {
		const pv = "prefill-v"
		Expect(a.throughputKey(model, ns, pv, accel, 1, domain.RolePrefill, 1000, 250, 5)).
			NotTo(Equal(a.throughputKey(model, ns, pv, accel, 1, domain.RolePrefill, 30000, 250, 5)))
	})

	It("does not collapse decode's output dimension along with prefill's", func() {
		// The control: the change is scoped to one role. If this ever passes,
		// decode has lost the bucketing its capacity genuinely depends on.
		Expect(a.throughputKey(model, ns, v, accel, 1, domain.RoleDecode, 30000, 6000, 5)).
			NotTo(Equal(a.throughputKey(model, ns, v, accel, 1, domain.RoleDecode, 30000, 250, 5)))
	})
})

var _ = Describe("fleetPrefixHitRate", func() {
	roles := map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode}
	pre := func(pod string, hit, rate float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName: pod, VariantName: "p", PrefixCacheHitRate: hit, RequestRate: rate,
		}
	}

	// The reason this passes fleet.ZeroIsAReading() rather than taking
	// fleet.Mean's default. That default reads a value of zero as absent, and a
	// hit rate of zero is a reading: a fleet with prefix caching off reports 0
	// everywhere, and skipping those would hand the mean to whichever replica
	// happened to report something.
	It("counts a zero hit rate as a reading, not as missing", func() {
		rms := []domain.ReplicaMetrics{pre("a", 0.0, 1), pre("b", 0.8, 1)}
		Expect(fleetPrefixHitRate(rms, roles)).To(BeNumerically("~", 0.4, 1e-9))
	})

	It("is one figure for the role, so two replicas cannot split the bucket", func() {
		// The fault this exists to prevent: with the replica's own rate, these
		// two land either side of an input-bucket boundary in the SAME cycle.
		rms := []domain.ReplicaMetrics{pre("a", 0.70, 1), pre("b", 0.80, 1)}
		rate := fleetPrefixHitRate(rms, roles)
		Expect(rate).To(BeNumerically("~", 0.75, 1e-9))
	})

	It("weights by request rate when there is one", func() {
		rms := []domain.ReplicaMetrics{pre("a", 0.0, 3), pre("b", 1.0, 1)}
		Expect(fleetPrefixHitRate(rms, roles)).To(BeNumerically("~", 0.25, 1e-9))
	})

	It("ignores decode replicas: this keys PREFILL's window", func() {
		rms := []domain.ReplicaMetrics{
			pre("a", 0.5, 1),
			{PodName: "d1", VariantName: "d", PrefixCacheHitRate: 1.0, RequestRate: 99},
		}
		Expect(fleetPrefixHitRate(rms, roles)).To(BeNumerically("~", 0.5, 1e-9))
	})

	It("returns 0 when no prefill replica reports, so ILeff is the raw prompt", func() {
		Expect(fleetPrefixHitRate(nil, roles)).To(Equal(0.0))
	})
})

var _ = Describe("the learned ITL baseline", func() {
	const variant = "decode-v"

	// A fleet spread across k, each replica reporting the ITL the traced model
	// predicts for its own load, so an OLS fit recovers that model.
	spread := func(n int) []domain.ReplicaMetrics {
		rms := make([]domain.ReplicaMetrics, 0, n)
		for i := 0; i < n; i++ {
			k := 0.20 + 0.06*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("d%d", i), variant, 400_000, tracedKv, 10, 1000, 6000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			rms = append(rms, rm)
		}
		return rms
	}

	// The same fleet balanced at one load: no spread, so the window is never
	// Ready and only the one-parameter fit can answer.
	flat := func() []domain.ReplicaMetrics {
		const n = 6
		const k = 0.35
		rms := make([]domain.ReplicaMetrics, 0, n)
		for i := 0; i < n; i++ {
			rm := makeReplicaMetrics(fmt.Sprintf("f%d", i), variant, 400_000, tracedKv, 10, 1000, 6000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			rms = append(rms, rm)
		}
		return rms
	}

	It("pins what this card measured, not a constant meant for another", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		key := "ns|model|" + variant

		// One spread cycle: OLS fits and its B is remembered.
		ols := a.noteITL(key, key, 1, spread(10), variant, a.now(), logr.Discard())
		Expect(ols.IsZero()).To(BeFalse())
		Expect(ols.B).To(BeNumerically("~", tracedModel.B, 5e-4),
			"the fit has to recover the card's floor before it can be reused")

		// The fleet balances. Two flat cycles clear DefaultMinSamples with no
		// spread at all, so the fallback answers -- pinned to the learned B.
		for i := 0; i < 2; i++ {
			a.noteITL(key, key, 1, flat(), variant, a.now(), logr.Discard())
		}
		pinned := a.noteITL(key, key, 1, flat(), variant, a.now(), logr.Discard())
		Expect(pinned.IsZero()).To(BeFalse())
		Expect(pinned.B).To(BeNumerically("~", ols.B, 1e-9),
			"the fallback pins the measured floor")
		Expect(pinned.B).NotTo(BeNumerically("~", itl.DefaultBaselineSec, 1e-9),
			"and not the global constant, which on this card is several times too high")
	})

	It("falls back to the constant only for a key that never fitted", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		for i := 0; i < 2; i++ {
			a.noteITL("fresh|key|"+variant, "fresh|key|"+variant, 1, flat(), variant, a.now(), logr.Discard())
		}
		got := a.noteITL("fresh|key|"+variant, "fresh|key|"+variant, 1, flat(), variant, a.now(), logr.Discard())
		Expect(got.IsZero()).To(BeFalse())
		Expect(got.B).To(BeNumerically("~", itl.DefaultBaselineSec, 1e-9),
			"nothing has been measured for this key, so the bootstrap stands")
	})
})

var _ = Describe("the throughput key and the borrow", func() {
	a := NewSaturationAnalyzer(capacity.NewStore())
	key := func(in, out float64) string {
		return a.throughputKey("m", "ns", "decode-v", "H200", 1, domain.RoleDecode, in, out, 5)
	}

	It("parses to the OUTPUT bucket, so the neighbour borrow still works", func() {
		// Appending the input bucket to the end of the key broke this:
		// splitHistoryKey reads the second-to-last field, so it returned the
		// queue threshold as the bucket, slices.Index found nothing in
		// outputBuckets, and nearestSaturatedThroughput returned nothing for
		// every key. The borrow died silently and reading.borrowed could never
		// be true again.
		k := key(1000, 6000)
		_, bucket, suffix, ok := splitHistoryKey(k)
		Expect(ok).To(BeTrue())
		Expect(bucket).To(Equal(classifyOutputLength(6000)))
		Expect(suffix).To(Equal("|q5"))
		Expect(slices.Index(outputBuckets, bucket)).To(BeNumerically(">=", 0),
			"the bucket has to be one the neighbour walk can index")
	})

	It("borrows across output buckets but never across input ones", func() {
		// A reading recorded for one shape is borrowable for a different
		// GENERATION length on the same prompts, and is a different key
		// entirely for different prompts.
		prefixOf := func(k string) string { p, _, _, _ := splitHistoryKey(k); return p }
		Expect(prefixOf(key(1000, 6000))).To(Equal(prefixOf(key(1000, 1000))),
			"same prompts: one borrow neighbourhood")
		Expect(prefixOf(key(1000, 6000))).NotTo(Equal(prefixOf(key(8000, 6000))),
			"different prompts: not a neighbour, not an own reading")
	})
})

var _ = Describe("capacityTokensFor", func() {
	It("prefers the replica's live reading to the deployment flag", func() {
		flagged := &capacity.EngineParams{TotalKvTokensOverride: 4 * tracedKv}
		Expect(capacityTokensFor(flagged, tracedKv)).To(Equal(float64(tracedKv)),
			"the engine's own reported capacity outranks a flag it may not have honoured")
	})

	It("falls back to the flag only where there is no live reading", func() {
		flagged := &capacity.EngineParams{TotalKvTokensOverride: tracedKv}
		Expect(capacityTokensFor(flagged, 0)).To(Equal(float64(tracedKv)),
			"a cold variant, or an engine that emits no cache_config_info")
		Expect(capacityTokensFor(nil, 0)).To(BeZero())
		Expect(capacityTokensFor(&capacity.EngineParams{}, 0)).To(BeZero())
	})

	It("leaves a derived mu on the measured capacity", func() {
		// The precedence was inverted, and SGLang parses --max-total-tokens
		// into EngineParams on every record, live or not -- so the flag
		// displaced the measured capacity on every cycle of every SGLang
		// variant. The engine clamps that flag to the memory it actually has,
		// so a flag it could not honour over-states C, which over-states the
		// resident count, which over-states mu, which UNDER-orders replicas.
		//
		// 4x the capacity puts the resident count past max_num_seqs, so the
		// two answers differ by more than rounding.
		fleet := shape.New(1000, 6000, 0)
		flagged := &capacity.EngineParams{MaxNumSeqs: 256, TotalKvTokensOverride: 4 * tracedKv}
		got := deriveMu(runPModel, flagged, tracedKv, fleet, tracedK, fleet.AvgOutputTokens)
		want := deriveMu(runPModel, tracedParams, tracedKv, fleet, tracedK, fleet.AvgOutputTokens)
		Expect(got.ok).To(BeTrue())
		Expect(got.rate).To(Equal(want.rate))
		Expect(got.seqs).To(BeNumerically("<", 256),
			"the live capacity holds fewer sequences than the cap; the flag would have hit it")
	})
})

// The GPS check is a diagnostic now, not a gate, so what matters is that the
// line is emitted when it should be and suppressed when the comparison would be
// meaningless. See noteLineMismatch for the run that decided it does not gate.
func TestLineMismatchIsLoggedNotEnforced(t *testing.T) {
	const variant = "decode-v"
	const k = 0.5
	fleet := shape.New(1000, 6000, 0)
	predicted := itl.TokenRate(runPModel, k, float64(tracedKv), fleet.KVreq)

	replica := func(observedGPS float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics("d0", variant, 400_000, tracedKv, 10, 1000, 6000)
		rm.Ready = true
		rm.KvUsageInstant = k
		rm.GenerationTokenRate = observedGPS
		return rm
	}
	// noteLineMismatch takes a logger directly rather than a context, so the
	// observer core is wired straight into one at the shipped verbosity.
	notice := func(rm domain.ReplicaMetrics, role string) *observer.ObservedLogs {
		core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteLineMismatch(runPModel, tracedParams, rm, role, fleet.KVreq,
			zapr.NewLogger(zap.New(core)))
		return logs
	}

	t.Run("a contradicted line is reported, and says it does not gate", func(t *testing.T) {
		logs := notice(replica(predicted/2), domain.RoleDecode)
		entries := logs.FilterMessage("itl-gps-mismatch").All()
		require.Len(t, entries, 1)
		fields := entries[0].ContextMap()
		assert.Equal(t, false, fields["gates"],
			"the field is what tells an operator this line withheld nothing")
		assert.Greater(t, fields["errPct"], 15.0)
	})

	t.Run("a sound line is silent", func(t *testing.T) {
		logs := notice(replica(predicted), domain.RoleDecode)
		assert.Empty(t, logs.FilterMessage("itl-gps-mismatch").All())
	})

	t.Run("an unmeasurable comparison is silent, not a mismatch", func(t *testing.T) {
		logs := notice(replica(0), domain.RoleDecode)
		assert.Empty(t, logs.FilterMessage("itl-gps-mismatch").All())
	})

	t.Run("a warm-pool bridge is skipped", func(t *testing.T) {
		rm := replica(predicted / 2)
		rm.FromWarmPool = true
		logs := notice(rm, domain.RoleDecode)
		assert.Empty(t, logs.FilterMessage("itl-gps-mismatch").All(),
			"a bridge runs the pool's engine settings; its rate says nothing about this line")
	})

	t.Run("a prefill replica is skipped", func(t *testing.T) {
		logs := notice(replica(predicted/2), domain.RolePrefill)
		assert.Empty(t, logs.FilterMessage("itl-gps-mismatch").All(),
			"prefill generates about one token per request")
	})
}

var _ = Describe("the one-parameter fallback's own floor", func() {
	const variant = "decode-v"

	// n replicas balanced at one load, each reporting the ITL the traced model
	// predicts there: no spread, so the window is never Ready and only the
	// one-parameter fit can answer.
	flatN := func(n int) []domain.ReplicaMetrics {
		const k = 0.35
		rms := make([]domain.ReplicaMetrics, 0, n)
		for i := 0; i < n; i++ {
			rm := makeReplicaMetrics(fmt.Sprintf("f%d", i), variant, 400_000, tracedKv, 10, 1000, 6000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			rms = append(rms, rm)
		}
		return rms
	}

	It("declines one reading short of DefaultMinSamples and answers on it", func() {
		// The exact edge. FitPinnedB answers from a single pair, and a derived
		// mu both orders replicas and settles a shape-change hold on sight, so
		// where this floor sits is the whole of what stops one reading from one
		// replica on one cycle doing both.
		a := NewSaturationAnalyzer(capacity.NewStore())
		key := "boundary|" + variant
		short := a.noteITL(key, key, 1, flatN(itl.DefaultMinSamples-1), variant, a.now(), logr.Discard())
		Expect(short.IsZero()).To(BeTrue(), "one short of the floor is not an answer")

		onMore := a.noteITL(key, key, 1, flatN(1), variant, a.now(), logr.Discard())
		Expect(onMore.IsZero()).To(BeFalse(), "the tenth reading is the one that makes it a fit")
	})

	It("catches a Ready window whose OLS line comes out inverted", func() {
		// Ready() and Fit() can disagree: the window admits a fleet with
		// enough samples and enough k-spread, and OLS over it still produces a
		// slope ValidModel rejects -- a noisier replica at a low k, a quieter
		// one high, and the line runs downhill. The fallthrough to the
		// one-parameter fit is what keeps that cycle from going unpriced, and
		// nothing asserted it.
		a := NewSaturationAnalyzer(capacity.NewStore())
		rms := make([]domain.ReplicaMetrics, 0, itl.DefaultMinSamples)
		for i := 0; i < itl.DefaultMinSamples; i++ {
			k := 0.20 + 0.06*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("i%d", i), variant, 400_000, tracedKv, 10, 1000, 6000)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = 0.030 - 0.01*k
			rms = append(rms, rm)
		}
		got := a.noteITL("inverted|"+variant, "inverted|"+variant, 1, rms, variant, a.now(), logr.Discard())
		Expect(got.IsZero()).To(BeFalse(), "an inverted OLS line must not leave the cycle unpriced")
		Expect(got.A).To(BeNumerically(">", 0), "the pinned fit answers with a positive slope")
		Expect(got.B).To(BeNumerically("~", itl.DefaultBaselineSec, 1e-9),
			"and pins the bootstrap, because no OLS fit ever succeeded for this key")
	})
})
