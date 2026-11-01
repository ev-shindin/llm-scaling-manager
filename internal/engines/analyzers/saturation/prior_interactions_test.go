package saturation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// How a variant's previous figure (P2-prior) gets along with the borrow,
// its controls and its metrics: the image hashed into the fingerprint, a
// parked variant, a forget, disableK2Borrow, the k2 source and the restore
// outcome. The unused previous figure is made by the first read of a window
// kept under the arguments it was learned under (window_args.go).

func mtParams(maxNumSeqs int64, image string) *capacity.EngineParams {
	return &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: maxNumSeqs, Image: image}
}

var mtOneVariant = []domain.VariantReplicaState{
	{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
}

// mtAnalyze runs one cycle of variant-a at tokens/queue and returns its
// capacity record.
func mtAnalyze(t *testing.T, a *SaturationAnalyzer, tokens int64, queue int, mutate func(*config.ScalingPolicy)) domain.VariantCapacity {
	t.Helper()
	in := makeAnalyzerInput([]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", tokens, 16000, queue, 100, 50)}, mtOneVariant)
	if mutate != nil {
		mutate(in.Config.(*config.ScalingPolicy))
	}
	result, err := analyzeConfirmed(context.Background(), a, in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(result.VariantCapacities) != 1 {
		t.Fatalf("want one variant capacity, got %d", len(result.VariantCapacities))
	}
	return result.VariantCapacities[0]
}

// mtPriorProcess is a controller that restarted after variant-a's arguments
// changed (max-num-seqs 256 -> 64) while it was down: the 4000-token k2
// learned under 256 is waiting to be installed as a prior. The returned
// clock pointer drives the process's time.
func mtPriorProcess(t *testing.T) (*SaturationAnalyzer, *time.Time) {
	t.Helper()
	clock := time.Now()
	learnStore := capacity.NewStore()
	learnStore.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, time.Now())
	first := NewSaturationAnalyzer(learnStore)
	first.now = func() time.Time { return clock }
	mtAnalyze(t, first, 4000, 6, nil)
	if got := mtAnalyze(t, first, 1000, 0, nil).PerReplicaCapacity; got != 4000 {
		t.Fatalf("precondition: k2 learned at 4000, got %v", got)
	}
	raw, err := json.Marshal(first.ExportLearnedState())
	if err != nil {
		t.Fatal(err)
	}
	var saved LearnedState
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}

	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(64, "vllm:v1")}, time.Now())
	second := NewSaturationAnalyzer(store)
	second.now = func() time.Time { return clock }
	second.RestoreLearnedState(saved)
	return second, &clock
}

func mtPriorKey(t *testing.T, a *SaturationAnalyzer) string {
	t.Helper()
	priors := previousFigures(a)
	if len(priors) != 1 {
		t.Fatalf("precondition: want one prior installed, got %d", len(priors))
	}
	for k := range priors {
		return k
	}
	return ""
}

// mtConvert moves every k2 window of a written under other arguments out of
// the way, as its first read does, without pricing a cycle: what leaves a
// prior installed and not yet used.
func mtConvert(a *SaturationAnalyzer, p *capacity.EngineParams) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.computeCapacityHistory {
		a.k2WindowUnderLocked(k, p.ArgsFingerprint())
	}
}

// mtServe advances the clock d of serving time, ticking every running borrow
// clock a minute at a time as a Ready own replica does.
func mtServe(a *SaturationAnalyzer, clock *time.Time, d time.Duration) {
	for end := clock.Add(d); clock.Before(end); {
		*clock = clock.Add(time.Minute)
		a.mu.Lock()
		for k := range a.lowerBounds {
			a.noteBorrowServingLocked(k, *clock, true)
		}
		a.mu.Unlock()
	}
}

// Fingerprint hashes the image, and a spent bound restarts when the
// borrower's arguments change. An image-only rollout is not new evidence
// about capacity -- a restore keeps k2 across it -- so it must not re-arm a
// spent borrow at full strength.
func TestAnImageOnlyBumpLeavesASpentBorrowSpent(t *testing.T) {
	clock := time.Now()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, time.Now())
	store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, time.Now())
	a := NewSaturationAnalyzer(store)
	a.now = func() time.Time { return clock }
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	if _, err := analyzeConfirmed(context.Background(), a, makeAnalyzerInput([]domain.ReplicaMetrics{
		makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50),
		makeReplicaMetrics("pod-b", "variant-b", 1000, 16000, 0, 100, 50),
	}, states)); err != nil {
		t.Fatal(err)
	}
	key := a.historyKey("test-model", "test-ns", "variant-b", "H100", 1, domain.RoleBoth, 50, 5)

	if _, ok := a.borrowedK2(key, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); !ok {
		t.Fatal("precondition: variant-b borrows variant-a's 4000")
	}
	mtServe(a, &clock, BorrowedK2Hold+BorrowedK2Relax+time.Minute)
	if _, ok := a.borrowedK2(key, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); ok {
		t.Fatal("precondition: the borrow is spent after its hold and relaxation")
	}

	if b, ok := a.borrowedK2(key, "test-model", 12800, mtParams(256, "vllm:v2"), true, true); ok {
		t.Errorf("an image-only rollout re-armed a spent borrow at k2=%d (relaxed %.2f): an image is not new evidence about capacity", b.k2, b.relaxed)
	}
}

// A bound's clock runs on serving time, so it pauses while the variant is
// parked, and a previous figure must not expire on the wall clock instead.
// A prior used for two hours and then parked for five must still be there,
// at the fraction it had relaxed to, when the variant wakes.
func TestAParkedVariantKeepsItsPriorAtTheSameRelaxedFraction(t *testing.T) {
	a, clock := mtPriorProcess(t)
	if got := mtAnalyze(t, a, 1000, 0, nil).PerReplicaCapacity; got != 4000 {
		t.Fatalf("precondition: held at the prior, got %v", got)
	}
	key := mtPriorKey(t, a)
	mtServe(a, clock, BorrowedK2Hold+BorrowedK2Relax/4)
	before, ok := a.borrowedK2(key, "test-model", 12800, mtParams(64, "vllm:v1"), true, true)
	if !ok {
		t.Fatal("precondition: the prior is in use")
	}

	*clock = clock.Add(5 * time.Hour) // parked: no serving cycle
	a.EvictStaleHistory()

	_, kept := previousFigures(a)[key]
	if !kept {
		t.Fatalf("the prior was swept during a park: it had relaxed to %.2f and the variant wakes on its own figure", before.relaxed)
	}
	after, ok := a.borrowedK2(key, "test-model", 12800, mtParams(64, "vllm:v1"), true, true)
	if !ok {
		t.Fatal("the prior is not in use after the park")
	}
	if d := after.relaxed - before.relaxed; d < -0.01 || d > 0.03 {
		t.Errorf("relaxed %.3f before the park, %.3f after: a park is not serving time", before.relaxed, after.relaxed)
	}
}

// Forgetting a variant must drop its previous figure too. An installed one
// not yet used has no clock, so a forget that only ended clocks would leave
// it to arm at full strength on the next cycle.
func TestForgetDropsAnUnusedPrior(t *testing.T) {
	a, _ := mtPriorProcess(t)
	if _, done := a.installRestored("test-ns", "test-model", "variant-a"); !done {
		t.Fatal("precondition: the restore was installed")
	}
	mtConvert(a, mtParams(64, "vllm:v1"))
	key := mtPriorKey(t, a)
	a.ForgetVariant("test-ns", "test-model", "variant-a", "t1")

	if b, ok := a.borrowedK2(key, "test-model", 12800, mtParams(64, "vllm:v1"), true, true); ok {
		t.Errorf("a forgotten variant was priced from its prior at k2=%d (P2-borrow)", b.k2)
	}
	if n := len(previousFigures(a)); n != 0 {
		t.Errorf("ForgetVariant left %d prior(s)", n)
	}
}

// disableK2Borrow turns off borrowing a SIBLING's
// figure. The prior is the variant's own figure under its previous
// arguments; switching the sibling borrow off must not drop the variant to
// its own k1 (the TTFT direction).
func TestDisableK2BorrowKeepsThePrior(t *testing.T) {
	a, _ := mtPriorProcess(t)
	got := mtAnalyze(t, a, 1000, 0, func(p *config.ScalingPolicy) { p.DisableK2Borrow = true })
	if got.PerReplicaCapacity != 4000 {
		t.Errorf("with disableK2Borrow the variant priced at %v, not its prior 4000", got.PerReplicaCapacity)
	}
}

// A prior is reported as its own k2 source, with an
// age, not as a sibling borrow with no age.
func TestAPriorIsReportedAsItsOwnSourceWithAnAge(t *testing.T) {
	a, _ := mtPriorProcess(t)
	vc := mtAnalyze(t, a, 1000, 0, nil)
	if vc.PerReplicaCapacity != 4000 {
		t.Fatalf("precondition: priced at the prior, got %v", vc.PerReplicaCapacity)
	}
	if vc.K2Source == capacity.K2SrcBorrowed.String() {
		t.Errorf("a prior is reported as %q: indistinguishable from a sibling's borrowed k2", vc.K2Source)
	}
	if !vc.HasK2Age {
		t.Errorf("a prior is reported with no k2 age (source %q)", vc.K2Source)
	}
}

// restores_total counts a restore that kept only a previous figure as such
// (prior), not as rejected.
func TestAPriorRestoreIsNotCountedAsRejected(t *testing.T) {
	a, _ := mtPriorProcess(t)
	mtAnalyze(t, a, 1000, 0, nil)
	got := a.TakeRestoreOutcomes()
	if got[string(RestoredPrior)] != 1 || len(got) != 1 {
		t.Errorf("a prior restore was not counted as its own outcome: %v", got)
	}
}
