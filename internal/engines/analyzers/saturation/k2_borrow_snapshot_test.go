package saturation

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// A borrow's clock across a controller restart: a spent borrow must stay
// spent in the next process, or every restart re-arms it at full strength.
var _ = Describe("a borrow clock across a restart", func() {
	params := func() *capacity.EngineParams {
		return &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256}
	}
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	var clock time.Time
	newProcess := func() *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params()}, time.Now())
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params()}, time.Now())
		a := NewSaturationAnalyzer(store)
		a.now = func() time.Time { return clock }
		return a
	}
	// cycle runs one Analyze with a Ready own replica of each variant;
	// variant-a saturates at 4000 when aQueue is over the threshold.
	cycle := func(a *SaturationAnalyzer, aTokens int64, aQueue int) float64 {
		ra := makeReplicaMetrics("pod-a", "variant-a", aTokens, 16000, aQueue, 100, 50)
		rb := makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50)
		ra.Ready, rb.Ready = true, true
		result, err := a.Analyze(context.Background(), makeAnalyzerInput([]domain.ReplicaMetrics{ra, rb}, states))
		Expect(err).NotTo(HaveOccurred())
		clock = clock.Add(15 * time.Second)
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == rb.VariantName {
				return vc.PerReplicaCapacity
			}
		}
		Fail("no capacity for variant-b")
		return 0
	}
	carry := func(s LearnedState) LearnedState {
		raw, err := json.Marshal(s)
		Expect(err).NotTo(HaveOccurred())
		var back LearnedState
		Expect(json.Unmarshal(raw, &back)).To(Succeed())
		return back
	}
	var first *SaturationAnalyzer
	var own string
	BeforeEach(func() {
		clock = time.Now()
		first = newProcess()
		cycle(first, 4000, 6) // variant-a measures 4000
		Expect(cycle(first, 1000, 0)).To(Equal(float64(4000)), "precondition: variant-b borrows")
		own = first.historyKey("test-model", "test-ns", "variant-b", "H100", 1, domain.RoleBoth, 50, 5)
		Expect(borrowClocks(first)).To(HaveKey(own), "precondition: the borrow's clock started")
	})

	It("keeps a spent borrow spent in the next process", func() {
		// Spent eight days ago, past the retention that used to drop a clock
		// at restore -- and with it re-arm the borrow at full strength.
		first.lowerBounds[own].since = clock.Add(-8 * 24 * time.Hour)
		first.lowerBounds[own].tick = clock
		Expect(cycle(first, 1000, 0)).To(Equal(float64(12800)), "precondition: spent, on its own k1")

		second := newProcess()
		second.RestoreLearnedState(carry(first.ExportLearnedState()))
		cycle(second, 1000, 0) // installs the restored state
		Expect(borrowClocks(second)).To(HaveKey(own), "the spent clock was not restored")
		cycle(second, 4000, 6) // the donor measures again in the new process
		Expect(cycle(second, 1000, 0)).To(Equal(float64(12800)),
			"the restart re-armed a spent borrow with no new evidence")
	})

	It("does not count the time the controller was down", func() {
		second := newProcess()
		saved := carry(first.ExportLearnedState())
		clock = clock.Add(3 * time.Hour) // down for three hours
		second.RestoreLearnedState(saved)
		cycle(second, 4000, 6)
		cycle(second, 1000, 0)
		Expect(second.lowerBounds).To(HaveKey(own))
		age := second.lowerBounds[own].tick.Sub(second.lowerBounds[own].since)
		Expect(age).To(BeNumerically("<", BorrowedK2Hold), "downtime was counted as serving time")
	})

	It("restores the bound of a variant that has nothing but its clock", func() {
		// No window and no liveness stamp of its own -- which is why it
		// borrows -- so nothing else in the document names its configuration.
		// A bound is restored without one (it can only make the variant more
		// conservative, for a bounded time), so it needs none.
		lone := newProcess()
		donor := lone.historyKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleBoth, 50, 5)
		ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
		ra.AddAt(4000, time.Now())
		lone.computeCapacityHistory[donor] = ra
		_, ok := lone.borrowedK2(own, "test-model", 12800, params(), true, true)
		Expect(ok).To(BeTrue())

		saved := carry(lone.ExportLearnedState())
		second := newProcess()
		rep := second.RestoreLearnedState(saved)
		Expect(rep.Unattributable).To(BeZero())
		Expect(rep.BorrowClocks).To(Equal(1), "a borrow-only variant's bound was not restored")
		Expect(borrowClocks(second)).To(HaveKey(own))
	})

	It("restores a clock from a document written before the state existed, on the old rule", func() {
		saved := carry(first.ExportLearnedState())
		// A document from before LowerBounds: the start of a running borrow
		// (K2BorrowedSince) and nothing else.
		saved.LowerBounds = nil
		Expect(saved.K2BorrowedSince).To(HaveKey(own), "precondition: the start is written for a rollback")
		// Half way through its relaxation: kept, it prices between the
		// donor's figure and k1; dropped, the borrow re-arms at the donor's.
		saved.K2BorrowedSince[own] = clock.Add(-(BorrowedK2Hold + BorrowedK2Relax/2))
		second := newProcess()
		second.RestoreLearnedState(saved)
		cycle(second, 4000, 6)
		Expect(cycle(second, 1000, 0)).To(BeNumerically("~", 8400, 50), "an old document's clock was dropped, and the borrow re-armed at full strength")
		Expect(second.lowerBounds[own].args).To(Equal(params().ArgsFingerprint()),
			"no evidence recorded: today's configuration is what it is armed on from here")
	})
})
