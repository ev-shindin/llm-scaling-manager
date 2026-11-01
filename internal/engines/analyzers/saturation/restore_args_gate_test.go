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

// The restore gate by how much of the configuration still holds: the whole
// of it, the arguments without the image, or neither.
var _ = Describe("learned state restored by engine arguments", func() {
	params := func(maxNumSeqs int64, image string) *capacity.EngineParams {
		return &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: maxNumSeqs, Image: image}
	}
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	newProcess := func(p *capacity.EngineParams) *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: p}, time.Now())
		return NewSaturationAnalyzer(store)
	}
	capacityOf := func(a *SaturationAnalyzer) float64 {
		result, err := a.Analyze(context.Background(), makeAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50)}, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		return result.VariantCapacities[0].PerReplicaCapacity
	}
	// learn saturates variant-a at 4000 tokens against a k1 of 12800 and
	// returns the snapshot as the ConfigMap would hold it, with an ITL baseline
	// for each liveness key so the latency figure's fate can be read.
	learn := func(p *capacity.EngineParams) LearnedState {
		a := newProcess(p)
		// Twice: a window's first reading waits for a later cycle's
		// (k2_confirm.go).
		for range 2 {
			_, err := a.Analyze(context.Background(), makeAnalyzerInput(
				[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)}, states))
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(capacityOf(a)).To(Equal(float64(4000)), "precondition: k2 learned")
		s := a.ExportLearnedState()
		Expect(s.VariantSeenAt).NotTo(BeEmpty(), "precondition: liveness exported")
		for k := range s.VariantSeenAt {
			s.ITLBaseline[k] = 0.05
		}
		raw, err := json.Marshal(s)
		Expect(err).NotTo(HaveOccurred())
		var back LearnedState
		Expect(json.Unmarshal(raw, &back)).To(Succeed())
		Expect(back.Args).To(HaveKey("test-ns/variant-a"), "the arguments travel with the configuration")
		return back
	}
	// serve advances the clock by d of SERVING time: a cycle a minute, each
	// ticking every running lower bound, as the variant's own Ready replica
	// does (noteBorrowServing). The clock counts serving time only, so a bare
	// jump of hours reads as a park.
	serve := func(a *SaturationAnalyzer, clock *time.Time, d time.Duration) {
		for end := clock.Add(d); clock.Before(end); {
			*clock = clock.Add(time.Minute)
			a.mu.Lock()
			for k := range a.lowerBounds {
				a.noteBorrowServingLocked(k, *clock, true)
			}
			a.mu.Unlock()
		}
	}
	// convert moves the variant's restored k2 windows out of the way as their
	// first read would (window_args.go), without pricing a cycle, and returns
	// the key of the last one.
	convert := func(a *SaturationAnalyzer, p *capacity.EngineParams) string {
		a.mu.Lock()
		defer a.mu.Unlock()
		var key string
		for k := range a.computeCapacityHistory {
			key = k
		}
		for k := range a.computeCapacityHistory {
			a.k2WindowUnderLocked(k, p.ArgsFingerprint())
		}
		return key
	}
	itlBaselines := func(a *SaturationAnalyzer) int {
		return len(a.itlBaselines())
	}

	It("restores everything under the same arguments and image", func() {
		second := newProcess(params(256, "vllm:v1"))
		second.RestoreLearnedState(learn(params(256, "vllm:v1")))
		r, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(r.Installed).To(BeTrue())
		Expect(r.ImageChanged).To(BeFalse())
		Expect(itlBaselines(second)).To(BeNumerically(">", 0), "the control: the ITL baseline comes back")
		Expect(capacityOf(second)).To(Equal(float64(4000)))
	})

	It("restores capacity across an image change made while the controller was down, without the ITL baseline", func() {
		second := newProcess(params(256, "vllm:v2"))
		second.RestoreLearnedState(learn(params(256, "vllm:v1")))
		r, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(r.Installed).To(BeTrue(), "a routine image bump made the restart start cold")
		Expect(r.ImageChanged).To(BeTrue())
		Expect(itlBaselines(second)).To(BeZero(), "a latency figure crossed an engine release")
		Expect(capacityOf(second)).To(Equal(float64(4000)))
	})

	It("keeps the old k2 only as a lower-only, expiring prior when the arguments changed", func() {
		saved := learn(params(256, "vllm:v1"))
		second := newProcess(params(64, "vllm:v1"))
		clock := time.Now()
		second.now = func() time.Time { return clock }
		second.RestoreLearnedState(saved)
		r, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(r.Installed).To(BeFalse())
		Expect(r.Outcome).To(Equal(RestoredPrior))
		Expect(r.Priors).To(Equal(1))

		second.mu.Lock()
		Expect(second.k2WindowArgs).To(HaveLen(1), "the k2 window was not installed under the arguments it was learned under")
		for _, args := range second.k2WindowArgs {
			Expect(args).To(Equal(params(256, "vllm:v1").ArgsFingerprint()),
				"a k2 under other arguments was installed as the variant's own")
		}
		Expect(second.saturatedThroughput).To(BeEmpty(), "a throughput under other arguments was installed")
		Expect(second.startSeconds).To(BeEmpty(), "a start time under other arguments was installed")
		second.mu.Unlock()

		Expect(capacityOf(second)).To(Equal(float64(4000)), "held at the prior")
		serve(second, &clock, BorrowedK2Hold+BorrowedK2Relax/2)
		Expect(capacityOf(second)).To(Equal(float64(8400)), "relaxing half way to own")
		serve(second, &clock, BorrowedK2Relax/2)
		Expect(capacityOf(second)).To(Equal(float64(12800)), "the prior outlived its relaxation")
		Expect(previousFigures(second)).To(BeEmpty(), "a spent previous figure is still offered")

		// Spent, it stays spent: the record is kept, as any spent bound's
		// is, so the figure cannot come back without new evidence.
		clock = clock.Add(time.Minute)
		evicted := second.EvictStaleHistory()
		Expect(evicted.K2Borrows+evicted.K2Priors).To(BeZero(), "a spent bound was swept while its variant lives")
		Expect(capacityOf(second)).To(Equal(float64(12800)))
	})

	// The prior's hold and relaxation run on the borrow clock, which starts
	// when the bucket is first priced. Swept on when it was installed, a
	// bucket first priced five hours after the restart lost its prior an hour
	// and a half into the relaxation, and capacity jumped to own at once.
	It("keeps a prior first used hours after the restart until its own relaxation ends", func() {
		saved := learn(params(256, "vllm:v1"))
		second := newProcess(params(64, "vllm:v1"))
		clock := time.Now()
		second.now = func() time.Time { return clock }
		second.RestoreLearnedState(saved)
		_, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())

		clock = clock.Add(5 * time.Hour)
		Expect(second.EvictStaleHistory().K2Priors).To(BeZero(), "an unused prior went before the bucket retention")
		Expect(capacityOf(second)).To(Equal(float64(4000)), "first use, five hours in: the hold starts now")

		serve(second, &clock, BorrowedK2Hold+BorrowedK2Relax/2)
		Expect(second.EvictStaleHistory().K2Priors).To(BeZero(), "the prior went part way through its relaxation")
		Expect(capacityOf(second)).To(Equal(float64(8400)), "relaxing half way to own")

		serve(second, &clock, BorrowedK2Relax/2+time.Minute)
		Expect(capacityOf(second)).To(Equal(float64(12800)), "the prior outlived its own relaxation")
		Expect(previousFigures(second)).To(BeEmpty())
	})

	It("lets a prior never used go a bucket retention after it became one", func() {
		saved := learn(params(256, "vllm:v1"))
		second := newProcess(params(64, "vllm:v1"))
		clock := time.Now()
		second.now = func() time.Time { return clock }
		second.RestoreLearnedState(saved)
		_, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		convert(second, params(64, "vllm:v1"))
		Expect(previousFigures(second)).To(HaveLen(1), "precondition: the window became a previous figure")

		clock = clock.Add(second.horizons.BucketRetention - time.Minute)
		Expect(second.EvictStaleHistory().K2Priors).To(BeZero())
		clock = clock.Add(2 * time.Minute)
		Expect(second.EvictStaleHistory().K2Priors).To(Equal(1))
	})

	// A second restart inside the prior's life: the prior and its clock are
	// exported, so the relaxation resumes where it was instead of the variant
	// dropping to its own figure at once.
	It("keeps a prior and its clock across a second restart", func() {
		saved := learn(params(256, "vllm:v1"))
		second := newProcess(params(64, "vllm:v1"))
		clock := time.Now()
		second.now = func() time.Time { return clock }
		second.RestoreLearnedState(saved)
		_, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(capacityOf(second)).To(Equal(float64(4000)), "precondition: the prior in use")

		serve(second, &clock, BorrowedK2Hold+BorrowedK2Relax/2)
		Expect(capacityOf(second)).To(Equal(float64(8400)), "precondition: half way")
		raw, err := json.Marshal(second.ExportLearnedState())
		Expect(err).NotTo(HaveOccurred())
		var again LearnedState
		Expect(json.Unmarshal(raw, &again)).To(Succeed())
		Expect(again.LowerBounds).To(HaveLen(1), "the prior was not exported")
		for _, lb := range again.LowerBounds {
			Expect(lb.Prior).To(Equal(int64(4000)), "the prior's figure was not exported with its clock")
		}

		third := newProcess(params(64, "vllm:v1"))
		third.now = func() time.Time { return clock }
		third.RestoreLearnedState(again)
		r, done := third.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(r.Outcome).To(Equal(RestoredWhole))
		Expect(capacityOf(third)).To(Equal(float64(8400)), "the relaxation did not resume where it was")
	})

	It("says when the arguments changed and no prior could be kept", func() {
		saved := learn(params(256, "vllm:v1"))
		// The variant now runs on two GPUs per replica: its k2 keys were
		// filed under one, and are never looked up again.
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			AcceleratorName: "H100", GpuCount: 2, EngineParams: params(64, "vllm:v1"),
		}, time.Now())
		second := NewSaturationAnalyzer(store)
		second.RestoreLearnedState(saved)
		r, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(r.Outcome).To(Equal(RestoredPrior))
		Expect(r.Priors).To(BeZero(), "a prior was kept under a key the variant can no longer read")
	})

	It("names an incomplete read as the reason nothing was restored", func() {
		saved := learn(params(256, "vllm:v1"))
		unread := params(256, "vllm:v1")
		unread.Unresolved = []string{"max_num_seqs"}
		second := newProcess(unread)
		second.RestoreLearnedState(saved)
		r, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(r.Outcome).To(Equal(RestoredIncomplete))
	})

	// llm-d's wide-EP LeaderWorkerSet computes its data-parallel size at
	// start-up. Read as an incomplete configuration, such a variant's own
	// learned state was neither exported nor restored.
	It("restores a variant whose parallel size is computed at start-up", func() {
		wideEP := func() *capacity.EngineParams {
			p := params(256, "vllm:v1")
			p.SharingUnresolved = []string{"data_parallel_size=$((LWS_GROUP_SIZE * DP_SIZE_LOCAL))"}
			return p
		}
		saved := learn(wideEP())
		second := newProcess(wideEP())
		second.RestoreLearnedState(saved)
		r, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(r.Outcome).To(Equal(RestoredWhole))
		Expect(capacityOf(second)).To(Equal(float64(4000)))
		Expect(second.engineFingerprint("test-ns", "test-model", "variant-a")).To(BeEmpty(),
			"a layout read as text is not a key for sharing a latency line")
	})

	It("never takes a prior above what the variant would get without it", func() {
		saved := learn(params(256, "vllm:v1"))
		second := newProcess(params(64, "vllm:v1"))
		second.RestoreLearnedState(saved)
		_, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		ownParams := params(64, "vllm:v1")
		key := convert(second, ownParams)
		Expect(previousFigures(second)).To(HaveLen(1))

		// The control: a prior below what the variant would get is taken.
		b, ok := second.borrowedK2(key, "test-model", 5000, ownParams, true, true)
		Expect(ok).To(BeTrue())
		Expect(b.k2).To(Equal(int64(4000)))
		Expect(b.own).To(BeTrue())

		// Above it, not: 4000 against an own capacity of 3000 would raise it.
		_, ok = second.borrowedK2(key, "test-model", 3000, ownParams, true, true)
		Expect(ok).To(BeFalse(), "a prior raised capacity")
	})

	It("takes a configuration it could not read completely as other arguments, not as a match", func() {
		// The live path tags an incomplete configuration's windows
		// incomplete:<fingerprint>, so a window from the complete arguments
		// becomes a lower-only previous figure on its first read; a restore
		// does the same, where it used to install nothing.
		saved := learn(params(256, "vllm:v1"))
		unread := params(64, "vllm:v1")
		unread.Unresolved = []string{"max_num_seqs"}
		second := newProcess(unread)
		second.RestoreLearnedState(saved)
		r, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(r.Outcome).To(Equal(RestoredIncomplete))
		Expect(r.Priors).To(Equal(1))
		Expect(capacityOf(second)).To(Equal(float64(4000)))
		Expect(previousFigures(second)).To(HaveLen(1), "taken as the variant's own window, not as a previous figure")
	})

	It("writes back the arguments of entries still pending", func() {
		saved := learn(params(256, "vllm:v1"))
		second := newProcess(params(256, "vllm:v1"))
		second.RestoreLearnedState(saved)
		again := second.ExportLearnedState()
		Expect(again.Args).To(HaveKeyWithValue("test-ns/variant-a", saved.Args["test-ns/variant-a"]))
	})
})
