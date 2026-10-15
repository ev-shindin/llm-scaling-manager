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
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params()})
		store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: params()})
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
			if vc.VariantName == "variant-b" {
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
		Expect(first.k2BorrowedSince).To(HaveKey(own), "precondition: the borrow's clock started")
	})

	It("keeps a spent borrow spent in the next process", func() {
		// Spent eight days ago, past the retention that used to drop a clock
		// at restore -- and with it re-arm the borrow at full strength.
		first.k2BorrowedSince[own] = clock.Add(-8 * 24 * time.Hour)
		first.k2BorrowState[own].tick = clock
		Expect(cycle(first, 1000, 0)).To(Equal(float64(12800)), "precondition: spent, on its own k1")

		second := newProcess()
		second.RestoreLearnedState(carry(first.ExportLearnedState()))
		cycle(second, 1000, 0) // installs the restored state
		Expect(second.k2BorrowedSince).To(HaveKey(own), "the spent clock was not restored")
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
		Expect(second.k2BorrowState).To(HaveKey(own))
		age := second.k2BorrowState[own].tick.Sub(second.k2BorrowedSince[own])
		Expect(age).To(BeNumerically("<", BorrowedK2Hold), "downtime was counted as serving time")
	})

	It("names the configuration of a variant that has nothing but its clock", func() {
		// No window and no liveness stamp of its own -- which is why it
		// borrows -- so nothing else in the document names its configuration.
		lone := newProcess()
		donor := lone.historyKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleBoth, 50, 5)
		ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
		ra.Add(4000)
		lone.computeCapacityHistory[donor] = ra
		_, ok := lone.borrowedK2(own, "test-model", 12800, params(), true)
		Expect(ok).To(BeTrue())

		saved := carry(lone.ExportLearnedState())
		Expect(saved.Config).To(HaveKey("test-ns/variant-b"),
			"a borrow-only variant's clock went out with no configuration, and is unattributable at restore")
		second := newProcess()
		Expect(second.RestoreLearnedState(saved).Unattributable).To(BeZero())
	})

	It("restores a clock from a document written before the state existed, on the old rule", func() {
		saved := carry(first.ExportLearnedState())
		saved.K2BorrowState = nil
		second := newProcess()
		second.RestoreLearnedState(saved)
		cycle(second, 4000, 6)
		Expect(cycle(second, 1000, 0)).To(Equal(float64(4000)), "an old document's young clock was dropped")
		Expect(second.k2BorrowState[own].args).To(Equal(params().Fingerprint()),
			"no evidence recorded: today's configuration is what it is armed on from here")
	})
})
