package saturation

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Carrying learned state across a controller restart (issue #137): a restart,
// a changed engine configuration and an aged entry. A leader hand-off is the
// same transfer between two processes, and is pinned at the engine, which
// owns the store.
var _ = Describe("learned state across a restart", func() {
	params := func(maxNumSeqs int64) *capacity.EngineParams {
		return &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: maxNumSeqs}
	}
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	// newProcess is one controller process: its own store, loaded from the
	// scale target as the engine does before every cycle, and its analyzer.
	newProcess := func(p *capacity.EngineParams) *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: p}, time.Now())
		return NewSaturationAnalyzer(store)
	}
	decisionOf := func(a *SaturationAnalyzer) domain.VariantCapacity {
		result, err := a.Analyze(context.Background(), makeAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50)}, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		return result.VariantCapacities[0]
	}
	capacityOf := func(a *SaturationAnalyzer) float64 { return decisionOf(a).PerReplicaCapacity }
	// learn saturates variant-a at 4000 tokens, against a k1 of 12800, so a
	// process that remembers it prices the replica at 4000 and one that does
	// not prices it at k1.
	learn := func(a *SaturationAnalyzer) LearnedState {
		// Twice: a window's first reading waits for a later cycle's
		// (k2_confirm.go).
		for range 2 {
			_, err := a.Analyze(context.Background(), makeAnalyzerInput(
				[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)}, states))
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(capacityOf(a)).To(Equal(float64(4000)), "precondition: the first process learned k2")
		return a.ExportLearnedState()
	}
	// carry is the store's round trip: the state as JSON, as the ConfigMap
	// holds it.
	carry := func(s LearnedState) LearnedState {
		raw, err := json.Marshal(s)
		Expect(err).NotTo(HaveOccurred())
		var back LearnedState
		Expect(json.Unmarshal(raw, &back)).To(Succeed())
		return back
	}

	It("restores k2 into a new process running the same configuration", func() {
		first := newProcess(params(256))
		saved := carry(learn(first))
		Expect(saved.Config).To(HaveKey("test-ns/variant-a"), "the variant's configuration travels with it")

		second := newProcess(params(256))
		Expect(capacityOf(NewSaturationAnalyzer(capacity.NewStore()))).To(Equal(float64(12800)),
			"the control: a process with nothing restored prices the replica at k1")
		rep := second.RestoreLearnedState(saved)
		Expect(rep.PendingVariants).To(Equal(1))
		Expect(capacityOf(second)).To(Equal(float64(4000)),
			"the restart forgot k2 and priced the replica at k1 -- three times the capacity")
	})

	It("restores into a variant whose engine configuration changed only a lower-only previous figure", func() {
		first := newProcess(params(256))
		saved := carry(learn(first))
		saved.Args = nil // a document from before the arguments were recorded

		second := newProcess(params(64))
		second.RestoreLearnedState(saved)
		vc := decisionOf(second)
		Expect(vc.K2Source).To(Equal(capacity.K2SrcPrior.String()),
			"a k2 measured under --max-num-seqs=256 was taken as the 64-sequence engine's own, or dropped")
		Expect(vc.PerReplicaCapacity).To(Equal(float64(4000)))
		Expect(second.TakeRestoreOutcomes()).To(Equal(map[string]int{string(RestoredDiffers): 1}), "the outcome was not counted")
		Expect(previousFigures(second)).To(HaveLen(1))
		second.mu.Lock()
		defer second.mu.Unlock()
		Expect(second.pendingRestore).To(BeEmpty(), "checked once, and dropped")
		Expect(second.computeCapacityHistory).To(BeEmpty(), "an older document's window was installed as the variant's own")
	})

	It("counts each installed restore once", func() {
		saved := carry(learn(newProcess(params(256))))
		second := newProcess(params(256))
		second.RestoreLearnedState(saved)
		capacityOf(second)
		capacityOf(second) // a second cycle installs nothing more
		Expect(second.TakeRestoreOutcomes()).To(Equal(map[string]int{string(RestoredWhole): 1}))
		Expect(second.TakeRestoreOutcomes()).To(BeEmpty(), "the counts were not reset when taken")
	})

	It("does not restore an entry past its horizon", func() {
		first := newProcess(params(256))
		saved := carry(learn(first))
		for k, w := range saved.K2 {
			w.LastUsed = time.Now().Add(-2 * capacity.HistoryRetention)
			saved.K2[k] = w
		}

		second := newProcess(params(256))
		second.RestoreLearnedState(saved)
		Expect(capacityOf(second)).To(Equal(float64(12800)),
			"a window unread for longer than the retention came back as if it had been read")
	})

	It("keeps each entry's clocks, so a restore does not make a figure younger", func() {
		first := newProcess(params(256))
		saved := learn(first)
		var used time.Time
		for k, w := range saved.K2 {
			used = time.Now().Add(-time.Hour)
			w.LastUsed = used
			saved.K2[k] = w
		}

		second := newProcess(params(256))
		second.RestoreLearnedState(carry(saved))
		second.mu.Lock()
		key := ""
		for k := range saved.K2 {
			key = k
		}
		Expect(second.computeCapacityHistory).To(BeEmpty(), "precondition: pending until the variant's cycle")
		second.mu.Unlock()
		_, done := second.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		second.mu.Lock()
		defer second.mu.Unlock()
		Expect(second.computeCapacityHistory).To(HaveKey(key))
		Expect(second.computeCapacityHistory[key].StaleAt(time.Now(), 30*time.Minute)).To(BeTrue(),
			"restored as last read now, not an hour ago")
	})

	It("does not restore a variant whose configuration was not known when it was saved", func() {
		first := newProcess(params(256))
		saved := carry(learn(first))
		saved.Config = nil

		second := newProcess(params(256))
		rep := second.RestoreLearnedState(saved)
		Expect(rep.PendingVariants).To(BeZero())
		Expect(rep.Unattributable).To(BeNumerically(">", 0))
		Expect(capacityOf(second)).To(Equal(float64(12800)))
	})

	It("never overwrites what the new process has already learned", func() {
		first := newProcess(params(256))
		saved := carry(learn(first))

		second := newProcess(params(256))
		_, err := analyzeConfirmed(context.Background(), second, makeAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 9000, 16000, 6, 100, 50)}, states))
		Expect(err).NotTo(HaveOccurred())
		second.RestoreLearnedState(saved)
		Expect(capacityOf(second)).To(Equal(float64(9000)), "the restore replaced a reading taken since the restart")
	})

	It("forgets everything, restored or learned, when the store is flushed", func() {
		first := newProcess(params(256))
		saved := carry(learn(first))
		first.RestoreLearnedState(saved)
		first.ForgetLearnedState()
		Expect(capacityOf(first)).To(Equal(float64(12800)))
		first.mu.Lock()
		defer first.mu.Unlock()
		Expect(first.pendingRestore).To(BeEmpty())
	})

	It("tags what it learned after a rollout with the arguments it was learned under", func() {
		// The scale target as the engine reads it every cycle.
		load := func(store *capacity.Store, maxNumSeqs string) {
			store.LoadFromScaleTarget("test-ns", "test-model", "variant-a", "H100", 1,
				scaletarget.NewDeploymentAccessor(&appsv1.Deployment{Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "vllm", Command: []string{"vllm", "serve", "test-model"},
						Args: []string{"--max-num-seqs", maxNumSeqs},
					}}}},
				}}), time.Now())
		}
		saturate := func(a *SaturationAnalyzer) {
			_, err := a.Analyze(context.Background(), makeAnalyzerInput(
				[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)}, states))
			Expect(err).NotTo(HaveOccurred())
		}
		store := capacity.NewStore()
		load(store, "256")
		first := NewSaturationAnalyzer(store)
		saturate(first)
		before := carry(first.ExportLearnedState()).Config["test-ns/variant-a"]
		Expect(before).NotTo(BeEmpty(), "precondition: the configuration was recorded")

		load(store, "64") // the rollout, read on the next cycle
		saturate(first)
		saved := carry(first.ExportLearnedState())
		Expect(saved.Config["test-ns/variant-a"]).NotTo(Equal(before),
			"the snapshot tagged figures learned after the rollout with the arguments it replaced")

		restarted := capacity.NewStore()
		load(restarted, "64")
		second := NewSaturationAnalyzer(restarted)
		second.RestoreLearnedState(saved)
		Expect(capacityOf(second)).To(Equal(float64(4000)),
			"a restart on the arguments the figures were learned under restored nothing")
	})

	It("tags what it learned after an image-only rollout with the new image", func() {
		load := func(store *capacity.Store, image string) {
			store.LoadFromScaleTarget("test-ns", "test-model", "variant-a", "H100", 1,
				scaletarget.NewDeploymentAccessor(&appsv1.Deployment{Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "vllm", Image: image, Command: []string{"vllm", "serve", "test-model"},
						Args: []string{"--max-num-seqs", "256"},
					}}}},
				}}), time.Now())
		}
		saturate := func(a *SaturationAnalyzer) {
			_, err := a.Analyze(context.Background(), makeAnalyzerInput(
				[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)}, states))
			Expect(err).NotTo(HaveOccurred())
		}
		store := capacity.NewStore()
		load(store, "registry.example/vllm:v0.11.0")
		first := NewSaturationAnalyzer(store)
		saturate(first)

		load(store, "registry.example/vllm:v0.11.1")
		saturate(first)
		saved := carry(first.ExportLearnedState())

		restarted := capacity.NewStore()
		load(restarted, "registry.example/vllm:v0.11.1")
		second := NewSaturationAnalyzer(restarted)
		second.RestoreLearnedState(saved)
		Expect(capacityOf(second)).To(Equal(float64(4000)),
			"the snapshot named the replaced release, and a restart on the new one restored nothing")
	})

	It("writes back what is still pending, so a second restart does not lose it", func() {
		// The first write after a restart is a minute in; a model parked at
		// zero has not been analysed by then.
		saved := carry(learn(newProcess(params(256))))
		second := newProcess(params(256))
		second.RestoreLearnedState(saved)
		again := carry(second.ExportLearnedState())
		Expect(again.Config).To(HaveKeyWithValue("test-ns/variant-a", saved.Config["test-ns/variant-a"]))

		third := newProcess(params(256))
		third.RestoreLearnedState(again)
		Expect(capacityOf(third)).To(Equal(float64(4000)),
			"the second restart lost k2 that was still pending at the first write")
	})

	It("does not take a configuration it could not read completely as a match", func() {
		// --max-num-seqs $VLLM_MAX_NUM_SEQS from an envFrom: both processes
		// hash the default, whatever the value is. Not a match: what was
		// learned comes back only as a lower-only previous figure (P2-prior),
		// never as the variant's own window (P2-hist).
		unread := func() *capacity.EngineParams {
			p := params(256)
			p.Unresolved = []string{"--max-num-seqs"}
			return p
		}
		first := newProcess(params(256))
		saved := carry(learn(first))
		second := newProcess(unread())
		second.RestoreLearnedState(saved)
		Expect(decisionOf(second).K2Source).To(Equal(capacity.K2SrcPrior.String()),
			"k2 installed as its own under a configuration this process could not read")

		incomplete := newProcess(unread())
		_, err := incomplete.Analyze(context.Background(), makeAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)}, states))
		Expect(err).NotTo(HaveOccurred())
		// Exported under it, tagged as unread: it restores only into the
		// same unread configuration, never as a complete one's.
		Expect(incomplete.ExportLearnedState().Args).To(HaveKeyWithValue("test-ns/variant-a", HavePrefix(incompletePrefix)),
			"an incompletely read configuration exported as a complete one")

		// A document that does carry such a fingerprint -- hand-edited, or
		// written by a build without the check above -- equal on both sides.
		forged := carry(learn(newProcess(params(256))))
		forged.Config["test-ns/variant-a"] = unread().Fingerprint()
		both := newProcess(unread())
		both.RestoreLearnedState(forged)
		Expect(decisionOf(both).K2Source).To(Equal(capacity.K2SrcPrior.String()),
			"two unread configurations with equal fingerprints were taken as the same")
	})

	It("does not restore a clock from the future, and takes a slightly early one as now", func() {
		saved := carry(learn(newProcess(params(256))))
		for k, w := range saved.K2 {
			w.LastUsed = time.Now().Add(100 * 365 * 24 * time.Hour)
			saved.K2[k] = w
		}
		far := newProcess(params(256))
		far.RestoreLearnedState(saved)
		Expect(capacityOf(far)).To(Equal(float64(12800)),
			"a window whose clock is a century ahead was restored: it would never go stale")

		saved = carry(learn(newProcess(params(256))))
		for k, w := range saved.K2 {
			w.LastUsed = time.Now().Add(maxClockSkew / 2)
			saved.K2[k] = w
		}
		near := newProcess(params(256))
		near.RestoreLearnedState(saved)
		Expect(capacityOf(near)).To(Equal(float64(4000)), "slight clock skew between nodes lost a figure")
	})

	It("checks the accelerator memo against the configuration like any per-variant entry", func() {
		saved := carry(learn(newProcess(params(256))))
		Expect(saved.Accelerators).To(HaveKey("test-ns/variant-a"), "precondition: the memo was exported")
		memo := func(a *SaturationAnalyzer) bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			_, ok := a.lastAccelerator["test-ns/variant-a"]
			return ok
		}

		// Other arguments on the same hardware: the restored windows are
		// installed as previous figures under the stable accelerator, and
		// the memo with them, so a cycle that reads no accelerator finds
		// them.
		changed := newProcess(params(64))
		changed.RestoreLearnedState(saved)
		Expect(memo(changed)).To(BeFalse(), "a memo installed before its variant's configuration was checked")
		_, done := changed.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(memo(changed)).To(BeTrue(), "the memo left out where the previous figures were installed")

		// Other hardware: neither the windows nor the memo describe it.
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{AcceleratorName: "A100", EngineParams: params(64)}, time.Now())
		moved := NewSaturationAnalyzer(store)
		moved.RestoreLearnedState(saved)
		_, done = moved.installRestored("test-ns", "test-model", "variant-a")
		Expect(done).To(BeTrue())
		Expect(memo(moved)).To(BeFalse(), "a memo naming H100 installed for a variant that runs on A100")
	})

	It("keeps a parked variant's pending entries past the bucket retention while it is noted present", func() {
		saved := carry(learn(newProcess(params(256))))
		second := newProcess(params(256))
		clock := time.Now()
		second.now = func() time.Time { return clock }
		second.RestoreLearnedState(saved)
		for range 8 {
			clock = clock.Add(24 * time.Hour)
			second.NoteVariantsPresent("test-ns", "test-model", []string{"variant-a"})
			second.EvictStaleHistory()
		}
		Expect(second.pendingRestore).To(HaveKey("test-ns/variant-a"),
			"a parked variant's restored state was swept a retention after the restart")
	})

	It("lets a deleted variant's pending entries go once none of them could be installed", func() {
		saved := carry(learn(newProcess(params(256))))
		// Only the per-variant liveness: entries that age on the analyzer's
		// clock, which the spec can move.
		saved.K2, saved.Throughput, saved.Accelerators, saved.K2BorrowedSince = nil, nil, nil, nil
		Expect(saved.VariantSeenAt).NotTo(BeEmpty(), "precondition")
		second := newProcess(params(256))
		clock := time.Now()
		second.now = func() time.Time { return clock }
		second.RestoreLearnedState(saved)
		Expect(second.pendingRestore).To(HaveKey("test-ns/variant-a"))

		clock = clock.Add(second.horizons.VariantTimeout + time.Hour)
		Expect(second.EvictStaleHistory().PendingRestores).To(Equal(1),
			"a deleted variant's entries, all past their horizon, were kept to be written back again")

		third := newProcess(params(256))
		third.now = func() time.Time { return clock }
		rep := third.RestoreLearnedState(saved)
		Expect(rep.PendingVariants).To(BeZero(), "entries past every horizon were held as pending at restore")
		Expect(rep.Aged).To(BeNumerically(">", 0))
	})

	It("does not keep pending entries alive on a clock from the future", func() {
		saved := carry(learn(newProcess(params(256))))
		saved.K2, saved.Throughput, saved.Accelerators, saved.K2BorrowedSince = nil, nil, nil, nil
		for k := range saved.VariantSeenAt {
			saved.VariantSeenAt[k] = time.Now().Add(100 * 365 * 24 * time.Hour)
		}
		rep := newProcess(params(256)).RestoreLearnedState(saved)
		Expect(rep.PendingVariants).To(BeZero(),
			"an entry that can never be installed was held, to be written back for a century")
	})

	It("prunes expired windows from a variant kept pending by its liveness", func() {
		saved := carry(learn(newProcess(params(256))))
		Expect(saved.K2).NotTo(BeEmpty(), "precondition")
		for k, w := range saved.K2 {
			w.LastUsed = time.Now().Add(-8 * 24 * time.Hour)
			saved.K2[k] = w
		}
		second := newProcess(params(256))
		second.RestoreLearnedState(saved)
		second.EvictStaleHistory()
		Expect(second.pendingRestore).To(HaveKey("test-ns/variant-a"), "precondition: still alive by its liveness")
		Expect(second.pendingRestore["test-ns/variant-a"].state.K2).To(BeEmpty(),
			"a window install would drop was kept, to be written back with every document")
	})

	It("does not export what was learned with no configuration known under a restored fingerprint", func() {
		saved := carry(learn(newProcess(params(256))))
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{}, time.Now()) // scale target not read yet
		second := NewSaturationAnalyzer(store)
		second.RestoreLearnedState(saved)
		_, err := second.Analyze(context.Background(), makeAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 9000, 16000, 6, 100, 50)}, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(second.pendingRestore).To(HaveKey("test-ns/variant-a"), "precondition: still pending")

		out := second.ExportLearnedState()
		for k, w := range out.K2 {
			Expect(w.Values).NotTo(ContainElement(float64(9000)),
				"k2 learned with no configuration known went out as checked, under %s", k)
		}
		Expect(out.K2).NotTo(BeEmpty(), "the restored entries were dropped with the unchecked ones")
	})

	It("keeps a parked variant's pending liveness fresh while it stays parked", func() {
		saved := carry(learn(newProcess(params(256))))
		second := newProcess(params(256))
		clock := time.Now()
		second.now = func() time.Time { return clock }
		second.RestoreLearnedState(saved)

		clock = clock.Add(second.horizons.VariantTimeout + time.Hour)
		second.NoteVariantsPresent("test-ns", "test-model", []string{"variant-a"})
		second.mu.Lock()
		p := second.pendingRestore["test-ns/variant-a"]
		second.mu.Unlock()
		Expect(p).NotTo(BeNil())
		Expect(p.state.VariantSeenAt).NotTo(BeEmpty())
		for _, seen := range p.state.VariantSeenAt {
			Expect(seen).To(Equal(clock), "a parked variant's liveness aged while it was parked")
		}
	})
})
