package saturation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

var _ = Describe("borrowing a sibling's k2", func() {
	var (
		a     *SaturationAnalyzer
		store *capacity.Store
		ctx   context.Context
		clock time.Time
	)
	// No batch-token budget, so the derived figure (P3) is unavailable and a
	// variant's own figure is k1 = 0.8 x its KV capacity.
	params := func(maxNumSeqs int64) *capacity.EngineParams {
		return &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: maxNumSeqs}
	}
	BeforeEach(func() {
		store = capacity.NewStore()
		a = NewSaturationAnalyzer(store)
		ctx = context.Background()
		clock = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return clock }
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params(256)}, time.Now())
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params(256)}, time.Now())
	})

	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	capacityOf := func(result *domain.AnalyzerResult, variant string) float64 {
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == variant {
				return vc.PerReplicaCapacity
			}
		}
		Fail("no capacity reported for " + variant)
		return 0
	}
	// cycle runs one Analyze with variant-a at aTokens (saturated when
	// aQueue is over the threshold of 5) and variant-b at bTokens.
	cycle := func(aTokens int64, aQueue int, bTokens, bKv int64, bQueue int) *domain.AnalyzerResult {
		result, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-a", "variant-a", aTokens, 16000, aQueue, 100, 50),
			makeReplicaMetrics("pod-b", "variant-b", bTokens, bKv, bQueue, 100, 50),
		}, states))
		Expect(err).NotTo(HaveOccurred())
		clock = clock.Add(15 * time.Second)
		return result
	}
	keyOf := func(variant string) string {
		return a.historyKey("test-model", "test-ns", variant, "H100", 1, domain.RoleBoth, 50, 5)
	}
	// serve moves the clock d forward a minute at a time, the variant
	// serving key on every step as a Ready own replica does every cycle: the
	// borrow clock counts serving time, so a jump with no cycles in it would
	// read as the variant parked.
	serve := func(key string, d time.Duration) {
		for end := clock.Add(d); clock.Before(end); {
			clock = clock.Add(time.Minute)
			a.noteBorrowServing(key, clock, true)
		}
	}

	It("starts a new variant from a compatible sibling's lower k2", func() {
		cycle(4000, 6, 1000, 16000, 0) // held (k2_confirm.go)
		cycle(4000, 6, 1000, 16000, 0) // variant-a measures 4000; variant-b has nothing
		result := cycle(1000, 0, 1000, 16000, 0)

		Expect(capacityOf(result, "variant-b")).To(Equal(float64(4000)),
			"variant-b's own figure is k1 = 12800; the sibling measured 4000 on the same engine")
		Expect(capacityOf(result, "variant-a")).To(Equal(float64(4000)),
			"the donor reads its own window, unchanged by lending it")
		b, ok := a.borrowedK2(keyOf("variant-b"), "test-model", 12800, params(256), true, true)
		Expect(ok).To(BeTrue())
		Expect(b.donor).To(Equal("variant-a"), "the donor is named, so the borrow can be traced")
	})

	It("reports the borrow on the variant's result, with how far it has relaxed", func() {
		borrowedOf := func(result *domain.AnalyzerResult, variant string) []domain.LearnedStateBorrow {
			for _, vc := range result.VariantCapacities {
				if vc.VariantName == variant {
					return vc.Borrowed
				}
			}
			Fail("no capacity reported for " + variant)
			return nil
		}
		cycle(4000, 6, 1000, 16000, 0) // held (k2_confirm.go)
		cycle(4000, 6, 1000, 16000, 0)
		result := cycle(1000, 0, 1000, 16000, 0)
		Expect(borrowedOf(result, "variant-b")).To(Equal([]domain.LearnedStateBorrow{
			{Quantity: domain.BorrowedK2, Donor: "variant-a", DonorNamespace: "test-ns", Relaxed: 0},
		}), "a variant priced from a sibling's k2 does not say so")
		Expect(borrowedOf(result, "variant-a")).To(BeEmpty(), "the donor reported a borrow of its own")

		serve(keyOf("variant-b"), BorrowedK2Hold+BorrowedK2Relax/2)
		got := borrowedOf(cycle(1000, 0, 1000, 16000, 0), "variant-b")
		Expect(got).To(HaveLen(1))
		Expect(got[0].Relaxed).To(BeNumerically("~", 0.5, 0.01), "the relaxation was not carried to the result")
	})

	It("never raises a variant above its own figure", func() {
		// The own figure here is DERIVED, below k1, because that is the only
		// case the rule can be seen in: capacity is min(k1, k2), so a borrowed
		// figure above k1 would be clamped anyway. A 60-token batch budget
		// with 100-in/50-out requests derives 20 sequences x 125 = 2500;
		// the sibling measured 4000; k1 is 12800. Borrowing 4000 would raise
		// variant-b by 60% on a figure measured elsewhere.
		budget := &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256, EffectiveMaxBatchedTokens: 60}
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: budget}, time.Now())
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: budget}, time.Now())
		Expect(estimateCapacityFromParams(budget, 100, 50)).To(Equal(int64(2500)), "precondition: the derived figure")

		cycle(4000, 6, 1000, 16000, 0)
		result := cycle(1000, 0, 1000, 16000, 0)
		Expect(capacityOf(result, "variant-b")).To(Equal(float64(2500)),
			"variant-b was raised to the sibling's 4000, above its own derived 2500")
	})

	It("does not borrow from a variant whose engine is configured differently", func() {
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params(128)}, time.Now())
		cycle(4000, 6, 1000, 16000, 0)
		result := cycle(1000, 0, 1000, 16000, 0)
		Expect(capacityOf(result, "variant-b")).To(Equal(float64(12800)),
			"a different --max-num-seqs is a different k2; the sibling's figure says nothing about it")
	})

	It("is displaced by the variant's own first measurement", func() {
		cycle(4000, 6, 1000, 16000, 0) // held (k2_confirm.go)
		cycle(4000, 6, 1000, 16000, 0)
		Expect(capacityOf(cycle(1000, 0, 1000, 16000, 0), "variant-b")).To(Equal(float64(4000)),
			"precondition: variant-b is borrowing")

		cycle(1000, 0, 9000, 16000, 6) // held (k2_confirm.go)
		cycle(1000, 0, 9000, 16000, 6) // variant-b saturates at 9000
		Expect(capacityOf(cycle(1000, 0, 1000, 16000, 0), "variant-b")).To(Equal(float64(9000)),
			"once variant-b has measured itself, its own reading is what it reads")
	})

	Describe("the expiry that keeps a too-low figure from holding forever", func() {
		// A low k2 orders extra replicas, so nothing saturates and nothing is
		// measured: without an expiry a donor's figure that is wrong for the
		// borrower would stand for as long as the donor exists.
		var own, donor string
		BeforeEach(func() {
			own, donor = keyOf("variant-b"), keyOf("variant-a")
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			ra.AddAt(4000, time.Now())
			a.computeCapacityHistory[donor] = ra
		})
		borrow := func() (k2Borrow, bool) { return a.borrowedK2(own, "test-model", 12800, params(256), true, true) }

		It("holds, then relaxes linearly to the variant's own figure, then stops", func() {
			b, ok := borrow()
			Expect(ok).To(BeTrue())
			Expect(b.k2).To(Equal(int64(4000)), "full strength at the start")

			serve(own, BorrowedK2Hold)
			b, _ = borrow()
			Expect(b.k2).To(Equal(int64(4000)), "and through the hold")

			serve(own, BorrowedK2Relax/2)
			b, _ = borrow()
			Expect(b.k2).To(Equal(int64(8400)), "halfway between 4000 and 12800 halfway through")

			serve(own, BorrowedK2Relax/2)
			_, ok = borrow()
			Expect(ok).To(BeFalse(), "fully relaxed: the variant is on its own figure")
		})

		It("is not restarted by a donor that goes and comes back", func() {
			_, ok := borrow()
			Expect(ok).To(BeTrue())

			saved := a.computeCapacityHistory[donor]
			delete(a.computeCapacityHistory, donor)
			serve(own, BorrowedK2Hold+BorrowedK2Relax/2)
			_, ok = borrow()
			Expect(ok).To(BeFalse(), "precondition: no donor, nothing borrowed")

			a.computeCapacityHistory[donor] = saved
			b, ok := borrow()
			Expect(ok).To(BeTrue())
			Expect(b.relaxed).To(BeNumerically("~", 0.5, 1e-9),
				"a restarted clock would hold the donor's figure at full strength again")
		})

		It("reads the donor's window without keeping it alive", func() {
			a.computeCapacityHistory[donor].TouchAt(time.Now().Add(-time.Hour))
			_, ok := borrow()
			Expect(ok).To(BeTrue())
			Expect(a.computeCapacityHistory[donor].StaleAt(time.Now(), time.Minute)).To(BeTrue(),
				"the borrow touched the donor; a lender would then live as long as anyone borrows from it")
		})

		It("does not borrow across a namespace", func() {
			// The other namespace has its own variant-a, as two teams deploying
			// the same manifests would: a key match that ignored the namespace
			// would read test-ns's variant-a window under other-ns's record.
			store.Update("other-ns", "test-model", "variant-a", capacity.Record{EngineParams: params(256)}, time.Now())
			other := a.historyKey("test-model", "other-ns", "variant-b", "H100", 1, domain.RoleBoth, 50, 5)
			_, ok := a.borrowedK2(other, "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeFalse())
		})

		It("keeps a spent borrow spent while nothing has changed", func() {
			_, ok := borrow()
			Expect(ok).To(BeTrue())
			serve(own, BorrowedK2Hold+BorrowedK2Relax)
			_, ok = borrow()
			Expect(ok).To(BeFalse(), "precondition: fully relaxed")

			// Eight days on its own figure, past the retention that used to
			// drop the clock and re-arm the borrow at full strength.
			for range 8 {
				clock = clock.Add(24 * time.Hour)
				a.computeCapacityHistory[donor].TouchAt(clock) // the donor's owner still reads it
				a.NoteVariantsPresent("test-ns", "test-model", []string{"variant-b"})
				Expect(a.EvictStaleHistory().K2Borrows).To(BeZero(),
					"a spent clock was swept while its variant exists")
			}
			_, ok = borrow()
			Expect(ok).To(BeFalse(), "a spent borrow re-armed at full strength with no new evidence")
		})

		It("ages the donor's window on the analyzer's clock, not the wall clock", func() {
			// The spec above touches the window on the fixture's clock, a fixed
			// date, and the sweep used to measure that stamp against the wall
			// clock: it passed until the wall clock was a retention past the
			// fixture's date, then swept the window and panicked on the next
			// touch. Both dates here are far from any wall clock, either way.
			retention := a.horizons.BucketRetention

			clock = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
			a.computeCapacityHistory[donor].TouchAt(clock)
			a.EvictStaleHistory()
			Expect(a.computeCapacityHistory).To(HaveKey(donor),
				"a window touched this instant was swept: its age was read off the wall clock")

			clock = clock.Add(retention + time.Minute)
			a.EvictStaleHistory()
			Expect(a.computeCapacityHistory).NotTo(HaveKey(donor),
				"a window unread for a retention on the analyzer's clock was kept")

			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			ra.AddAt(4000, time.Now())
			a.computeCapacityHistory[donor] = ra
			clock = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
			ra.TouchAt(clock.Add(-retention - time.Minute))
			a.EvictStaleHistory()
			Expect(a.computeCapacityHistory).NotTo(HaveKey(donor),
				"a window a retention old on the analyzer's clock was kept, because the wall clock is behind it")
		})

		It("re-arms when the donor has measured something materially different", func() {
			_, ok := borrow()
			Expect(ok).To(BeTrue())
			serve(own, BorrowedK2Hold+BorrowedK2Relax)

			a.computeCapacityHistory[donor] = capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			a.computeCapacityHistory[donor].AddAt(3600, time.Now())
			_, ok = borrow()
			Expect(ok).To(BeFalse(), "a 10% move is a donor still measuring, not new evidence")

			a.computeCapacityHistory[donor] = capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			a.computeCapacityHistory[donor].AddAt(2000, time.Now())
			b, ok := borrow()
			Expect(ok).To(BeTrue(), "the donor halved its figure and the spent borrow ignored it")
			Expect(b.k2).To(Equal(int64(2000)), "re-armed from full strength")
			Expect(b.relaxed).To(BeZero())
		})

		It("re-arms when the borrower's engine configuration changed", func() {
			_, ok := borrow()
			Expect(ok).To(BeTrue())
			serve(own, BorrowedK2Hold+BorrowedK2Relax)
			_, ok = borrow()
			Expect(ok).To(BeFalse(), "precondition: spent")

			// --enforce-eager is in the configuration digest and does not
			// change capacity compatibility, so the donor still lends.
			redeployed := params(256)
			redeployed.EnforceEager = true
			b, ok := a.borrowedK2(own, "test-model", 12800, redeployed, true, true)
			Expect(ok).To(BeTrue(), "a redeployed borrower is a new question and was refused a borrow")
			Expect(b.k2).To(Equal(int64(4000)))
		})

		It("goes with its variant, not on its age", func() {
			_, ok := borrow()
			Expect(ok).To(BeTrue())
			clock = clock.Add(a.horizons.VariantTimeout + time.Hour)
			a.computeCapacityHistory[donor].TouchAt(clock)
			Expect(a.EvictStaleHistory().K2Borrows).To(Equal(1),
				"a clock no one read or found present for VariantTimeout was kept")
			Expect(a.lowerBounds).NotTo(HaveKey(own))
		})

		It("gives a bucket the variant has never measured its own hold, and keeps each bucket's clock", func() {
			// Keyed per variant, a variant past its first 6.5 hours jumped
			// straight to its own k1 in every new bucket -- 3.2x the donor's
			// figure, no hold, no gradual rise: the TTFT-direction gap the
			// borrow exists to close. Per bucket, the cost is at most one hold
			// per bucket, in extra replicas.
			longOwn := a.historyKey("test-model", "test-ns", "variant-b", "H100", 1, domain.RoleBoth, 5000, 5)
			longDonor := a.historyKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleBoth, 5000, 5)
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			ra.AddAt(4000, time.Now())
			a.computeCapacityHistory[longDonor] = ra

			_, ok := borrow()
			Expect(ok).To(BeTrue())
			serve(own, BorrowedK2Hold+BorrowedK2Relax)
			_, ok = borrow()
			Expect(ok).To(BeFalse(), "precondition: fully relaxed in the first bucket")

			b, ok := a.borrowedK2(longOwn, "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeTrue(), "a bucket the variant never measured went straight to k1")
			Expect(b.k2).To(Equal(int64(4000)), "from full strength, for the new bucket")

			_, ok = borrow()
			Expect(ok).To(BeFalse(), "the first bucket's clock was restarted by the second's")
		})
	})

	Describe("a clock that counts serving time", func() {
		var own, donor string
		BeforeEach(func() {
			own, donor = keyOf("variant-b"), keyOf("variant-a")
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			ra.AddAt(4000, time.Now())
			a.computeCapacityHistory[donor] = ra
		})

		It("pauses while the variant is parked", func() {
			_, ok := a.borrowedK2(own, "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeTrue())
			serve(own, 10*time.Minute)

			clock = clock.Add(10 * time.Hour) // parked: no cycle serves the bucket
			a.computeCapacityHistory[donor].TouchAt(clock)
			b, ok := a.borrowedK2(own, "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeTrue(), "a canary parked overnight woke with its borrow spent")
			Expect(b.age).To(Equal(10*time.Minute+BorrowedK2TickGap),
				"serving time, plus the gap a missed cycle may leave")
			Expect(b.k2).To(Equal(int64(4000)), "still inside the hold")
		})

		It("is not started by a replica that does not serve the variant", func() {
			b, ok := a.borrowedK2(own, "test-model", 12800, params(256), false, true)
			Expect(ok).To(BeTrue(), "a bridge reads the borrow")
			Expect(b.k2).To(Equal(int64(4000)))
			Expect(borrowClocks(a)).To(BeEmpty(), "a bridge started the variant's clock")
		})

		It("is read by a non-serving replica as it stood at the last serving cycle", func() {
			_, ok := a.borrowedK2(own, "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeTrue())
			serve(own, BorrowedK2Hold+BorrowedK2Relax/2)
			clock = clock.Add(10 * time.Hour)
			a.computeCapacityHistory[donor].TouchAt(clock)

			b, ok := a.borrowedK2(own, "test-model", 12800, params(256), false, true)
			Expect(ok).To(BeTrue(), "a bridge on a parked variant read the borrow as spent")
			Expect(b.relaxed).To(BeNumerically("~", 0.5, 1e-9))
		})

		It("runs on every serving cycle, with or without a donor to read", func() {
			_, ok := a.borrowedK2(own, "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeTrue())
			saved := a.computeCapacityHistory[donor]
			delete(a.computeCapacityHistory, donor)
			ownPod := makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50)
			ownPod.Ready = true
			for end := clock.Add(BorrowedK2Hold + BorrowedK2Relax/2); clock.Before(end); {
				clock = clock.Add(time.Minute)
				_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{ownPod}, states[1:]))
				Expect(err).NotTo(HaveOccurred())
			}
			saved.TouchAt(clock)
			a.computeCapacityHistory[donor] = saved
			b, ok := a.borrowedK2(own, "test-model", 12800, params(256), false, true)
			Expect(ok).To(BeTrue())
			Expect(b.relaxed).To(BeNumerically("~", 0.5, 0.01),
				"three and a half hours of serving with no donor in sight were counted as parked")
		})

		It("starts on a Ready own replica through Analyze, and not on a bridge", func() {
			cycleWith := func(b domain.ReplicaMetrics) *domain.AnalyzerResult {
				result, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50), b,
				}, states))
				Expect(err).NotTo(HaveOccurred())
				clock = clock.Add(15 * time.Second)
				return result
			}
			bridge := makeReplicaMetrics("pool-1", "variant-b", 1000, 16000, 0, 100, 50)
			bridge.Ready, bridge.FromWarmPool = true, true
			Expect(capacityOf(cycleWith(bridge), "variant-b")).To(Equal(float64(4000)),
				"a bridge prices from the borrow")
			Expect(borrowClocks(a)).To(BeEmpty(), "a bridge started the variant's clock")

			ownPod := makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50)
			ownPod.Ready = true
			Expect(capacityOf(cycleWith(ownPod), "variant-b")).To(Equal(float64(4000)))
			Expect(borrowClocks(a)).To(HaveKey(own), "a Ready own replica did not start the clock")
		})
	})

	Describe("the model's switch", func() {
		It("prices a variant on its own figure, and leaves a running clock where it stands", func() {
			own := keyOf("variant-b")
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			ra.AddAt(4000, time.Now())
			a.computeCapacityHistory[keyOf("variant-a")] = ra
			_, ok := a.borrowedK2(own, "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeTrue())
			started := borrowClocks(a)[own]

			ownPod := makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50)
			ownPod.Ready = true
			input := makeAnalyzerInput([]domain.ReplicaMetrics{ownPod}, states[1:])
			input.Config.(*config.ScalingPolicy).DisableK2Borrow = true
			clock = clock.Add(time.Minute)
			result, err := a.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(capacityOf(result, "variant-b")).To(Equal(float64(12800)),
				"disableK2Borrow still priced the variant from its sibling")
			Expect(result.VariantCapacities[0].Reason).NotTo(Equal("P2-borrow"))
			Expect(borrowClocks(a)[own]).To(Equal(started), "the switch moved the clock it stopped")
		})
	})

	Describe("which sibling", func() {
		seed := func(variant string, avgOutput float64, k2 float64) {
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			ra.AddAt(k2, time.Now())
			a.computeCapacityHistory[a.historyKey("test-model", "test-ns", variant, "H100", 1, domain.RoleBoth, avgOutput, 5)] = ra
		}

		It("takes the highest of several lower donors", func() {
			store.Update("test-ns", "test-model", "variant-c", capacity.Record{EngineParams: params(256)}, time.Now())
			seed("variant-a", 50, 3000)
			seed("variant-c", 50, 6000)
			b, ok := a.borrowedK2(keyOf("variant-b"), "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeTrue())
			Expect(b.k2).To(Equal(int64(6000)), "the least far below own: the fewest replicas spent on someone else's figure")
			Expect(b.donor).To(Equal("variant-c"))
		})

		It("does not borrow across an output bucket", func() {
			// A k2 is occupancy at saturation, and how much a saturated replica
			// holds depends on how long its answers are.
			seed("variant-a", 5000, 4000)
			_, ok := a.borrowedK2(keyOf("variant-b"), "test-model", 12800, params(256), true, true)
			Expect(ok).To(BeFalse())
		})
	})

	Describe("against the variant's own capacity, not its own k2", func() {
		// The derived figure can sit ABOVE k1, and capacity is min(k1, k2).
		// 2048-token batches with 100-in/50-out requests derive 256 sequences
		// x 125 = 32000, against a k1 of 12800.
		var budget *capacity.EngineParams
		BeforeEach(func() {
			budget = &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256, EffectiveMaxBatchedTokens: 2048}
			store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: budget}, time.Now())
			store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: budget}, time.Now())
			Expect(estimateCapacityFromParams(budget, 100, 50)).To(Equal(int64(32000)), "precondition: derived above k1")
		})

		It("borrows on the derived path when the donor is below the variant's capacity", func() {
			budget.EffectiveMaxBatchedTokens = 60 // derives 2500, below k1
			ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
			ra.AddAt(2000, time.Now())
			a.computeCapacityHistory[keyOf("variant-a")] = ra

			result := cycle(1000, 0, 1000, 16000, 0)
			Expect(capacityOf(result, "variant-b")).To(Equal(float64(2000)),
				"a P3 variant -- the usual canary, with engine parameters -- must borrow too")
		})

		It("does not report a borrow that changes nothing", func() {
			// The donor saturated at 14000: below the derived 32000, above k1.
			// Capacity is k1 either way, so this is no borrow, and its clock
			// must not start.
			cycle(14000, 6, 1000, 16000, 0)
			result := cycle(1000, 0, 1000, 16000, 0)
			borrower := states[1].VariantName
			for _, vc := range result.VariantCapacities {
				if vc.VariantName == borrower {
					Expect(vc.Reason).NotTo(Equal("P2-borrow"))
				}
			}
			Expect(borrowClocks(a)).To(BeEmpty(), "the hold was spent on a borrow that did nothing")
		})
	})
})
