package saturation

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// Forgetting one variant on request: its own state goes, everyone else's
// stays, and the request is not acted on twice.
var _ = Describe("forgetting one variant", func() {
	var (
		a     *SaturationAnalyzer
		store *capacity.Store
		clock time.Time
	)
	params := func() *capacity.EngineParams { return &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256} }
	variants := []string{"variant-a", "variant-b", "variant-b2"}
	BeforeEach(func() {
		store = capacity.NewStore()
		a = NewSaturationAnalyzer(store)
		clock = time.Now()
		a.now = func() time.Time { return clock }
		states := make([]domain.VariantReplicaState, 0, len(variants))
		rows := make([]domain.ReplicaMetrics, 0, len(variants))
		for _, v := range variants {
			store.Update("test-ns", "test-model", v, capacity.Record{EngineParams: params()})
			states = append(states, domain.VariantReplicaState{VariantName: v, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1})
			r := makeReplicaMetrics("pod-"+v, v, 4000, 16000, 6, 100, 50)
			r.Ready = true
			rows = append(rows, r)
		}
		_, err := a.Analyze(context.Background(), makeAnalyzerInput(rows, states))
		Expect(err).NotTo(HaveOccurred())
		for _, v := range variants {
			a.itlLines["line-of-"+v] = learnedLine{
				model:       itl.Model{A: 0.01, B: 0.01},
				fromVariant: a.itlWindowKey("test-ns", "test-model", v, "H100", 1),
				learnedAt:   clock,
			}
		}
	})
	keysOf := func(v string) int {
		n := 0
		for k := range a.computeCapacityHistory {
			if strings.HasPrefix(k, "test-ns|"+v+"|") {
				n++
			}
		}
		return n
	}

	It("drops the variant's own state and nothing else", func() {
		for _, v := range variants {
			Expect(keysOf(v)).To(BeNumerically(">", 0), "precondition: %s learned a k2", v)
		}
		Expect(a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")).To(BeNumerically(">", 0))

		Expect(keysOf("variant-b")).To(BeZero(), "the variant's k2 window survived")
		Expect(store.Get("test-ns", "test-model", "variant-b")).To(BeNil(), "its capacity record survived")
		Expect(a.itlLines).NotTo(HaveKey("line-of-variant-b"), "the line it published survived")

		Expect(keysOf("variant-a")).To(BeNumerically(">", 0), "a sibling's window was forgotten")
		Expect(keysOf("variant-b2")).To(BeNumerically(">", 0), "variant-b2 matched variant-b's prefix")
		Expect(store.Get("test-ns", "test-model", "variant-b2")).NotTo(BeNil())
		Expect(a.itlLines).To(HaveKey("line-of-variant-a"))
		Expect(a.itlLines).To(HaveKey("line-of-variant-b2"))
		Expect(a.ForgottenToken("test-ns", "variant-b")).To(Equal("t1"))
	})

	It("drops what a restore was holding for the variant", func() {
		saved := a.ExportLearnedState()
		second := NewSaturationAnalyzer(capacity.NewStore())
		second.now = a.now
		second.RestoreLearnedState(saved)
		Expect(second.pendingRestore).To(HaveKey("test-ns/variant-b"), "precondition")
		second.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
		Expect(second.pendingRestore).NotTo(HaveKey("test-ns/variant-b"),
			"a forgotten variant's restored state would be installed on its first cycle")
		Expect(second.pendingRestore).To(HaveKey("test-ns/variant-a"))
	})

	It("carries the token across a restart, so a standing annotation is not acted on again", func() {
		a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
		raw, err := json.Marshal(a.ExportLearnedState())
		Expect(err).NotTo(HaveOccurred())
		var back LearnedState
		Expect(json.Unmarshal(raw, &back)).To(Succeed())

		second := NewSaturationAnalyzer(capacity.NewStore())
		second.now = a.now
		second.RestoreLearnedState(back)
		Expect(second.ForgottenToken("test-ns", "variant-b")).To(Equal("t1"))
	})

	It("keeps the token while the variant exists, and lets it go when it does not", func() {
		a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
		for range 3 {
			clock = clock.Add(a.horizons.VariantTimeout / 2)
			a.NoteVariantsPresent("test-ns", "test-model", []string{"variant-b"})
			a.EvictStaleHistory()
		}
		Expect(a.forgotten).To(HaveKey("test-ns/variant-b"), "a parked variant's token was swept")

		clock = clock.Add(a.horizons.VariantTimeout + time.Minute)
		Expect(a.EvictStaleHistory().ForgetMarks).To(Equal(1))
		Expect(a.ForgottenToken("test-ns", "variant-b")).To(BeEmpty())
	})
})
