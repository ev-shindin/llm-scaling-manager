package saturation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// A restore reports how many k2 windows the document held for the variant,
// so a variant that never measured one of its own -- it borrowed, or never
// saturated -- is not reported as having had none recent enough.
func TestARestoreCountsTheWindowsTheDocumentHeld(t *testing.T) {
	restore := func(windows map[string]capacity.RollingAverageSnapshot) RestoredVariant {
		a, _, _ := waProcess(mtParams(64, "vllm:v1"))
		// A variant with an entry of its own -- its liveness -- and no k2
		// window, as a variant that only borrowed is exported.
		seen := a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1)
		a.RestoreLearnedState(LearnedState{
			Config:        map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").Fingerprint()},
			Args:          map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").ArgsFingerprint()},
			K2:            windows,
			VariantSeenAt: map[string]time.Time{seen: time.Now()},
		})
		r, done := a.installRestored("test-ns", "test-model", "variant-a")
		if !done {
			t.Fatal("precondition: the variant's entries were pending")
		}
		return r
	}
	if r := restore(nil); r.Stored != 0 || r.Outcome != RestoredPrior {
		t.Errorf("a document with no window for the variant: want stored 0 and a prior outcome, got %+v", r)
	}
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	if r := restore(map[string]capacity.RollingAverageSnapshot{key: rrWindow(4000).Snapshot()}); r.Stored != 1 {
		t.Errorf("a document with one window for the variant: want stored 1, got %+v", r)
	}
}

// The restart line tells a variant that had no k2 window to keep from one
// whose windows were too old, or measured on other hardware, to keep -- for a
// changed configuration and for one not read completely alike.
func TestTheRestartLineSaysWhetherTheSnapshotHeldAWindow(t *testing.T) {
	unread := &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 64, Unresolved: []string{"max-num-seqs"}}
	for _, tc := range []struct {
		name   string
		params *capacity.EngineParams
		window bool
		want   string
	}{
		{"changed, none held", mtParams(64, "vllm:v1"), false,
			"configuration changed, and the snapshot held no k2 window of the variant's own to keep"},
		{"changed, one too old", mtParams(64, "vllm:v1"), true,
			"configuration changed, and no k2 window was recent enough"},
		{"unread, none held", unread, false,
			"could not be read completely, and the snapshot held no k2 window of the variant's own to keep"},
		{"unread, one too old", unread, true,
			"could not be read completely, and no k2 window was recent enough"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged []string
			ctx := ctrl.LoggerInto(context.Background(),
				funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}))
			a, _, _ := waProcess(tc.params)
			doc := LearnedState{
				Config:        map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").Fingerprint()},
				Args:          map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").ArgsFingerprint()},
				VariantSeenAt: map[string]time.Time{a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1): time.Now()},
			}
			if tc.window {
				// Last used two months ago: past any bucket retention.
				old := rrWindow(4000).Snapshot()
				old.LastUsed = time.Now().Add(-60 * 24 * time.Hour)
				old.LastWritten = old.LastUsed
				doc.K2 = map[string]capacity.RollingAverageSnapshot{rrKey(a, "variant-a"): old}
			}
			a.RestoreLearnedState(doc)
			in := makeAnalyzerInput([]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50)}, mtOneVariant)
			if _, err := analyzeConfirmed(ctx, a, in); err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			for _, l := range logged {
				if strings.Contains(l, "learned state") && strings.Contains(l, "variant-a") {
					if !strings.Contains(l, tc.want) {
						t.Errorf("restart line %q does not say %q", l, tc.want)
					}
					return
				}
			}
			t.Fatalf("no restart line for variant-a in %d lines", len(logged))
		})
	}
}

// The windows counted are the ones the snapshot held when it was read: the
// sweep before the variant's first cycle prunes one past retention, and a
// window that aged out is not one the variant never had.
func TestTheStoredCountSurvivesTheSweep(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	old := rrWindow(4000).Snapshot()
	old.LastUsed = time.Now().Add(-60 * 24 * time.Hour)
	old.LastWritten = old.LastUsed
	a.RestoreLearnedState(LearnedState{
		Config:        map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").Fingerprint()},
		Args:          map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").ArgsFingerprint()},
		K2:            map[string]capacity.RollingAverageSnapshot{rrKey(a, "variant-a"): old},
		VariantSeenAt: map[string]time.Time{a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1): time.Now()},
	})
	a.NoteVariantsPresent("test-ns", "test-model", []string{"variant-a"})
	a.EvictStaleHistory()
	r, done := a.installRestored("test-ns", "test-model", "variant-a")
	if !done {
		t.Fatal("precondition: the variant's entries were pending")
	}
	if r.Stored != 1 {
		t.Errorf("the window the snapshot held was not counted once the sweep pruned it: stored %d", r.Stored)
	}
}

// A variant pricing on its previous figure when the snapshot was written has
// that figure in its lower bound, its window set aside: restored under yet
// other arguments, the figure prices it again, and the restart line says it
// was restored, not that the snapshot held nothing.
func TestARestoredBoundFigureIsReportedAsRestored(t *testing.T) {
	first, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, first, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	if got := waRun(t, first, clock, 1000, 0); got.PerReplicaCapacity != 4000 {
		t.Fatalf("precondition: priced at the previous figure, got %v", got.PerReplicaCapacity)
	}
	doc := first.ExportLearnedState()

	var logged []string
	ctx := ctrl.LoggerInto(context.Background(),
		funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}))
	second, _, _ := waProcess(mtParams(128, "vllm:v1"))
	second.RestoreLearnedState(doc)
	in := makeAnalyzerInput([]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50)}, mtOneVariant)
	res, err := analyzeConfirmed(ctx, second, in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got := res.VariantCapacities[0].PerReplicaCapacity; got != 4000 {
		t.Fatalf("precondition: the restored previous figure prices the variant, got %v", got)
	}
	for _, l := range logged {
		if strings.Contains(l, "learned state") && strings.Contains(l, "variant-a") {
			if strings.Contains(l, "not restored") || !strings.Contains(l, "kept with their lower bounds") {
				t.Errorf("the restart line denies the figure that prices the variant: %q", l)
			}
			return
		}
	}
	t.Fatal("no restart line for variant-a")
}

// A previous figure held in a bound on hardware the variant no longer runs is
// never looked up, so it is not counted as restored: the line falls through to
// what the variant is priced on.
func TestABoundFigureOnOtherHardwareIsNotCountedAsRestored(t *testing.T) {
	first, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, first, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	waRun(t, first, clock, 1000, 0)
	doc := first.ExportLearnedState()

	count := func(accelerator string) int {
		a, s, _ := waProcess(mtParams(128, "vllm:v1"))
		s.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EngineParams: mtParams(128, "vllm:v1"), AcceleratorName: accelerator, GpuCount: 1,
		}, time.Now())
		a.RestoreLearnedState(doc)
		r, done := a.installRestored("test-ns", "test-model", "variant-a")
		if !done {
			t.Fatalf("precondition: the variant's entries were pending (%s)", accelerator)
		}
		return r.BoundPriors
	}
	if n := count("H100"); n != 1 {
		t.Fatalf("precondition: on the same hardware the bound's figure is counted, got %d", n)
	}
	if n := count("A100"); n != 0 {
		t.Errorf("a bound on hardware the variant left was counted as restored: %d", n)
	}
}
