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

// What a borrow does around the operator's controls and a restart: forgetting
// the variant, turning the borrow off, a restore that runs late, a borrower
// whose configuration was not read completely, and a slow optimisation loop.
var _ = Describe("a borrow around the operator's controls", func() {
	var (
		a          *SaturationAnalyzer
		store      *capacity.Store
		clock      time.Time
		own, donor string
	)
	params := func() *capacity.EngineParams {
		return &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256}
	}
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	BeforeEach(func() {
		store = capacity.NewStore()
		a = NewSaturationAnalyzer(store)
		clock = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return clock }
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params()}, time.Now())
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params()}, time.Now())
		own = a.historyKey("test-model", "test-ns", "variant-b", "H100", 1, domain.RoleBoth, 50, 5)
		donor = a.historyKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleBoth, 50, 5)
		setDonor(a, donor, 4000)
	})
	borrow := func(p *capacity.EngineParams) (k2Borrow, bool) {
		return a.borrowedK2(own, "test-model", 12800, p, true, true)
	}
	// serve moves the clock d forward step at a time, the variant serving
	// own on every step.
	serve := func(d, step time.Duration) {
		for end := clock.Add(d); clock.Before(end); {
			clock = clock.Add(step)
			a.noteBorrowServing(own, clock, true)
		}
	}

	It("is ended, not re-armed, when the variant is forgotten", func() {
		_, ok := borrow(params())
		Expect(ok).To(BeTrue())
		serve(time.Hour, time.Minute)

		a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
		clock = clock.Add(time.Minute)
		_, ok = borrow(params())
		Expect(ok).To(BeFalse(),
			"forgetting the variant re-armed the sibling's figure at full strength")
		Expect(borrowClocks(a)).To(HaveKey(own), "the ended clock was dropped")

		setDonor(a, donor, 2000)
		b, ok := borrow(params())
		Expect(ok).To(BeTrue(), "new evidence after the forget still re-arms it")
		Expect(b.k2).To(Equal(int64(2000)))
	})

	It("does not run while the model's switch has the borrow off", func() {
		_, ok := borrow(params())
		Expect(ok).To(BeTrue())
		serve(10*time.Minute, time.Minute)

		ownPod := makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50)
		ownPod.Ready = true
		for end := clock.Add(2 * time.Hour); clock.Before(end); {
			clock = clock.Add(time.Minute)
			input := makeAnalyzerInput([]domain.ReplicaMetrics{ownPod}, states[1:])
			input.Config.(*config.ScalingPolicy).DisableK2Borrow = true
			_, err := a.Analyze(context.Background(), input)
			Expect(err).NotTo(HaveOccurred())
		}

		b, ok := borrow(params())
		Expect(ok).To(BeTrue())
		Expect(b.age).To(Equal(10*time.Minute+BorrowedK2TickGap),
			"two hours with the borrow off were counted as serving time")
		Expect(b.k2).To(Equal(int64(4000)), "still inside the hold when it is turned back on")
	})

	Describe("re-arming a spent borrow", func() {
		It("does not count a donor's move made while the borrow ran", func() {
			_, ok := borrow(params())
			Expect(ok).To(BeTrue())
			serve(BorrowedK2Hold+BorrowedK2Relax/2, time.Minute)
			setDonor(a, donor, 3000) // 25% below what it was armed on
			_, ok = borrow(params())
			Expect(ok).To(BeTrue(), "precondition: still relaxing")

			serve(BorrowedK2Relax/2, time.Minute)
			_, ok = borrow(params())
			Expect(ok).To(BeFalse(),
				"a move the borrow ran on re-armed it at full strength on the cycle it ended")
		})

		It("restarts on a configuration change made while the borrow ran, and counts it once", func() {
			_, ok := borrow(params())
			Expect(ok).To(BeTrue())
			serve(BorrowedK2Hold+BorrowedK2Relax/2, time.Minute)
			redeployed := params()
			redeployed.EnforceEager = true
			b, ok := borrow(redeployed)
			Expect(ok).To(BeTrue())
			Expect(b.relaxed).To(BeZero(),
				"the new arguments were handed what was left of a bound armed on the old ones")

			serve(BorrowedK2Hold+BorrowedK2Relax, time.Minute)
			_, ok = borrow(redeployed)
			Expect(ok).To(BeFalse(),
				"the change the bound restarted on re-armed it again on the cycle it ended")
		})
	})

	It("counts serving cycles further apart than five minutes when the loop runs that slowly", func() {
		a.WithOptimizationInterval(10 * time.Minute)
		Expect(a.BorrowTickGap()).To(Equal(30 * time.Minute))
		_, ok := borrow(params())
		Expect(ok).To(BeTrue())
		serve(2*time.Hour, 10*time.Minute)
		b, ok := borrow(params())
		Expect(ok).To(BeTrue())
		Expect(b.age).To(Equal(2*time.Hour),
			"half of every ten-minute cycle was counted as a pause")
	})

	Describe("across a restart", func() {
		It("keeps the clock of a borrower whose configuration was not read completely", func() {
			unread := params()
			unread.Unresolved = []string{"max_num_seqs"}
			store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: unread}, time.Now())
			_, ok := borrow(unread)
			Expect(ok).To(BeTrue(), "precondition: an unread borrower still borrows")
			serve(BorrowedK2Hold+BorrowedK2Relax, time.Minute)
			_, ok = borrow(unread)
			Expect(ok).To(BeFalse(), "precondition: spent")

			second := NewSaturationAnalyzer(store)
			second.now = a.now
			rep := second.RestoreLearnedState(a.ExportLearnedState())
			Expect(rep.BorrowClocks).To(Equal(1))
			Expect(borrowClocks(second)).To(HaveKey(own),
				"the clock was held for a configuration check it can never pass")
			setDonor(second, donor, 4000)
			clock = clock.Add(time.Minute)
			_, ok = second.borrowedK2(own, "test-model", 12800, unread, true, true)
			Expect(ok).To(BeFalse(), "the restart re-armed a spent borrow")
		})

		It("does not undo a forget acted on before the restore ran", func() {
			ownPod := makeReplicaMetrics("pod-b", "variant-b", 4000, 16000, 6, 100, 50)
			ownPod.Ready = true
			_, err := analyzeConfirmed(context.Background(), a, makeAnalyzerInput([]domain.ReplicaMetrics{ownPod}, states[1:]))
			Expect(err).NotTo(HaveOccurred())
			saved := a.ExportLearnedState()
			Expect(saved.K2).To(HaveKey(own), "precondition: variant-b learned a k2")

			// The first read failed; the cycles went on, and the annotation was
			// acted on before the read that succeeded.
			second := NewSaturationAnalyzer(store)
			second.now = a.now
			second.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
			rep := second.RestoreLearnedState(saved)
			Expect(rep.Forgotten).To(BeNumerically(">", 0))
			Expect(second.pendingRestore).NotTo(HaveKey("test-ns/variant-b"),
				"the restore brought back what the forget dropped, and the token is spent")
			Expect(second.ForgottenToken("test-ns", "variant-b")).To(Equal("t1"))
		})

		It("restores a variant whose stored document is already past its forget", func() {
			a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
			// The engine refiles the record from the scale target on the cycle.
			store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params()}, time.Now())
			ownPod := makeReplicaMetrics("pod-b", "variant-b", 4000, 16000, 6, 100, 50)
			ownPod.Ready = true
			_, err := a.Analyze(context.Background(), makeAnalyzerInput([]domain.ReplicaMetrics{ownPod}, states[1:]))
			Expect(err).NotTo(HaveOccurred())
			saved := a.ExportLearnedState()

			second := NewSaturationAnalyzer(store)
			second.now = a.now
			second.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
			second.RestoreLearnedState(saved)
			Expect(second.pendingRestore).To(HaveKey("test-ns/variant-b"),
				"what was learned after the forget, under the same token, was dropped")
		})
	})
})

// setDonor replaces the donor's k2 window with one reading.
func setDonor(a *SaturationAnalyzer, key string, k2 float64) {
	ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
	ra.AddAt(k2, time.Now())
	a.computeCapacityHistory[key] = ra
}
