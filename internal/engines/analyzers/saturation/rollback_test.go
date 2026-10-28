package saturation

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Rollbacks, rollouts and what a variant keeps across them.

func rrWindow(v float64) *capacity.RollingAverage {
	ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
	ra.AddAt(v, time.Now())
	return ra
}

// rrKey is variant's k2 key as waRun and lbProcess file it.
func rrKey(a *SaturationAnalyzer, variant string) string {
	return a.historyKey("test-model", "test-ns", variant, "H100", 1, domain.RoleBoth, 50, 5)
}

// rrServe advances the clock d of serving time on key, a minute at a time.
func rrServe(a *SaturationAnalyzer, key string, clock *time.Time, d time.Duration) {
	for end := clock.Add(d); clock.Before(end); {
		*clock = clock.Add(time.Minute)
		a.noteBorrowServing(key, *clock, true)
	}
}

// A rollback to engine arguments the variant ran before gets their window
// back as its own, with full authority: not relaxed away as a previous
// figure, and not priced from the figure of the arguments it was rolled back
// from. Then forward again, the other window comes back too.
func TestARollbackGetsTheOldArgumentsWindowBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bSaturates bool
	}{{"the new arguments never saturated", false}, {"the new arguments saturated higher", true}} {
		t.Run(tc.name, func(t *testing.T) {
			a, store, clock := waProcess(mtParams(256, "vllm:v1"))
			waRun(t, a, clock, 4000, 6)
			waSetParams(store, mtParams(64, "vllm:v1"))
			if vc := waRun(t, a, clock, 1000, 0); vc.K2Source != capacity.K2SrcPrior.String() {
				t.Fatalf("precondition: priced from the previous figure, got %s", vc.K2Source)
			}
			if tc.bSaturates {
				waRun(t, a, clock, 9000, 6)
				if vc := waRun(t, a, clock, 1000, 0); vc.K2Source != capacity.K2SrcHistorical.String() || vc.PerReplicaCapacity != 9000 {
					t.Fatalf("precondition: 64 sequences measured 9000, got %v from %s", vc.PerReplicaCapacity, vc.K2Source)
				}
			}

			waSetParams(store, mtParams(256, "vllm:v1"))
			vc := waRun(t, a, clock, 1000, 0)
			if vc.K2Source != capacity.K2SrcHistorical.String() || vc.PerReplicaCapacity != 4000 {
				t.Errorf("rolled back to 256 sequences: %v from %s, want its own 4000 from P2-hist",
					vc.PerReplicaCapacity, vc.K2Source)
			}
			if priors := previousFigures(a); len(priors) != 0 {
				t.Errorf("the window put back is still held as a previous figure too: %v", priors)
			}

			if tc.bSaturates {
				waSetParams(store, mtParams(64, "vllm:v1"))
				if vc := waRun(t, a, clock, 1000, 0); vc.K2Source != capacity.K2SrcHistorical.String() || vc.PerReplicaCapacity != 9000 {
					t.Errorf("forward to 64 sequences again: %v from %s, want its own 9000", vc.PerReplicaCapacity, vc.K2Source)
				}
			}
		})
	}
}

// The throughput window comes back with the k2 one.
func TestARollbackGetsTheOldArgumentsServiceRateBack(t *testing.T) {
	a, _, _ := waProcess(mtParams(256, "vllm:v1"))
	a256, a64 := mtParams(256, "vllm:v1").ArgsFingerprint(), mtParams(64, "vllm:v1").ArgsFingerprint()
	key := a.throughputKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleDecode, 1000, 1000, 5)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saturatedThroughput[key] = rrWindow(3)
	a.throughputWindowArgs[key] = a256
	a.throughputWindowUnderLocked(key, a64)
	if _, ok := a.saturatedThroughput[key]; ok {
		t.Fatal("precondition: the window is not read under other arguments")
	}
	a.throughputWindowUnderLocked(key, a256)
	ra, ok := a.saturatedThroughput[key]
	if !ok || ra.Median() != 3 || a.throughputWindowArgs[key] != a256 {
		t.Errorf("rolled back: window %v (median %v), tag %q; want the 256-sequence rate back", ok, ra, a.throughputWindowArgs[key])
	}
}

// A previous figure is the variant's LATEST measurement under the arguments it
// left: two rollouts in a row leave the second's figure, higher or not. Kept
// on the lower of the two, a figure from arguments two generations back won
// every later change and was handed a fresh hold each time. (A rollback is
// the stash's case: TestARollbackGetsTheOldArgumentsWindowBack.)
func TestAPreviousFigureIsTheLatestMeasurement(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	waRun(t, a, clock, 9000, 6)
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity != 9000 {
		t.Fatalf("precondition: 64 sequences measured 9000, got %v", vc.PerReplicaCapacity)
	}
	waSetParams(store, mtParams(128, "vllm:v1"))
	vc := waRun(t, a, clock, 1000, 0)
	if vc.K2Source != capacity.K2SrcPrior.String() || vc.PerReplicaCapacity != 9000 {
		t.Errorf("after a second rollout: %v from %s, want the latest previous figure 9000", vc.PerReplicaCapacity, vc.K2Source)
	}
}

// A change of arguments while a bound runs restarts it: the new arguments get
// a full hold on the previous figure, not what was left of a bound armed on a
// sibling hours ago. Two paths: the arguments recorded as evidence (a
// complete configuration), and the window's own conversion (a configuration
// not read completely records none).
func TestAnArgumentsChangeRestartsARunningBound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to func() *capacity.EngineParams
	}{
		{"read completely", func() *capacity.EngineParams { return mtParams(256, "vllm:v1") },
			func() *capacity.EngineParams { return mtParams(64, "vllm:v1") }},
		{"not read completely", func() *capacity.EngineParams {
			p := mtParams(256, "vllm:v1")
			p.Unresolved = []string{"gpu_memory_utilization"}
			return p
		}, func() *capacity.EngineParams {
			p := mtParams(64, "vllm:v1")
			p.Unresolved = []string{"gpu_memory_utilization"}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, own, _, clock := lbProcess(t, 4000)
			if _, ok := a.borrowedK2(own, "test-model", 12800, tc.from(), true, true); !ok {
				t.Fatal("precondition: variant-b borrows at its first cycle")
			}
			a.mu.Lock()
			a.computeCapacityHistory[own] = rrWindow(5000)
			a.k2WindowArgs[own] = windowArgs(tc.from())
			a.mu.Unlock()
			rrServe(a, own, clock, 5*time.Hour)

			a.mu.Lock()
			a.k2WindowUnderLocked(own, windowArgs(tc.to()))
			a.mu.Unlock()
			b, ok := a.borrowedK2(own, "test-model", 12800, tc.to(), true, true)
			if !ok || !b.own || b.k2 != 5000 || b.relaxed != 0 {
				t.Errorf("first cycle under the new arguments: %+v (ok %v), want the previous figure 5000 at full strength", b, ok)
			}
		})
	}
}

// A spent bound is not re-armed by a figure that was on offer while it ran:
// the variant's previous figure ran in front of a lower sibling, and when it
// ends the sibling is no new evidence.
func TestASpentBoundIsNotReArmedByAFigureOnOfferAllAlong(t *testing.T) {
	a, own, _, clock := lbProcess(t, 3500)
	p := mtParams(256, "vllm:v1")
	a.mu.Lock()
	a.lowerBounds[own] = &lowerBound{prior: 5000, priorSince: *clock, seen: *clock}
	a.mu.Unlock()
	if b, ok := a.borrowedK2(own, "test-model", 12800, p, true, true); !ok || !b.own || b.k2 != 5000 {
		t.Fatalf("precondition: priced from the previous figure 5000, got %+v", b)
	}
	rrServe(a, own, clock, BorrowedK2Hold+BorrowedK2Relax+time.Minute)
	if _, ok := a.borrowedK2(own, "test-model", 12800, p, true, true); ok {
		t.Fatal("precondition: the bound is spent")
	}
	*clock = clock.Add(time.Minute)
	if b, ok := a.borrowedK2(own, "test-model", 12800, p, true, true); ok {
		t.Errorf("re-armed at %d on a sibling's figure that was on offer the whole time", b.k2)
	}
}

// A replica that may not arm a bound -- a warm-pool bridge, a Pod that is not
// Ready -- reads a fresh one where there is new evidence, not the spent or
// half-relaxed bound the variant last served.
func TestABridgeReadsAFreshBoundWhenThereIsNewEvidence(t *testing.T) {
	t.Run("a lower figure after the bound was spent", func(t *testing.T) {
		a, own, donor, clock := lbProcess(t, 4000)
		lbSpend(t, a, own, clock)
		lbSetDonor(a, donor, 3000)
		b, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), false, true)
		if !ok || b.k2 != 3000 || b.relaxed != 0 {
			t.Errorf("a bridge read %+v (ok %v), want the new lower figure at full strength", b, ok)
		}
	})
	t.Run("other arguments while the bound ran", func(t *testing.T) {
		a, own, _, clock := lbProcess(t, 4000)
		if _, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); !ok {
			t.Fatal("precondition: variant-b borrows")
		}
		rrServe(a, own, clock, 3*time.Hour)
		redeployed := mtParams(256, "vllm:v1")
		redeployed.EnforceEager = true
		b, ok := a.borrowedK2(own, "test-model", 12800, redeployed, false, true)
		if !ok || b.relaxed != 0 {
			t.Errorf("a bridge read %+v (ok %v) under new arguments, want a fresh bound", b, ok)
		}
	})
}

// A sibling's window still tagged with arguments the sibling no longer runs
// describes an engine nobody runs, and is not lent.
func TestASiblingWindowFromArgumentsItNoLongerRunsIsNotLent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tag   string
		lends bool
	}{
		{"tagged with what it runs", mtParams(256, "vllm:v1").ArgsFingerprint(), true},
		{"untagged", "", true},
		{"tagged with what it ran before", mtParams(64, "vllm:v1").ArgsFingerprint(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, own, donor, _ := lbProcess(t, 4000)
			a.mu.Lock()
			if tc.tag != "" {
				a.k2WindowArgs[donor] = tc.tag
			}
			a.mu.Unlock()
			if _, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); ok != tc.lends {
				t.Errorf("lent = %v, want %v", ok, tc.lends)
			}
		})
	}
}

// The cache gate compares the replica's live KV total, not the store's
// record, which on the cycle that arms a bound is still the one derived from
// the deployment, with no total.
func TestTheCacheGateReadsTheReplicasLiveTotal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		live  int64
		lends bool
	}{{"a live total twice the donor's", 16000, false}, {"a live total like the donor's", 32000, true}} {
		t.Run(tc.name, func(t *testing.T) {
			a, own, _, _ := lbProcess(t, 4000)
			a.capacityStore.Update("test-ns", "test-model", "variant-a", capacity.Record{
				EngineParams: mtParams(256, "vllm:v1"), TotalKvCapacityTokens: 32000, LearnedFrom: capacity.LearnedFromLive,
			}, time.Now())
			_, ok := a.borrowedK2WithKv(own, "test-model", 12800, mtParams(256, "vllm:v1"), tc.live, true, true)
			if ok != tc.lends {
				t.Errorf("lent = %v, want %v", ok, tc.lends)
			}
		})
	}
}

// A bound's arguments hashed under another FingerprintVersion differ from
// today's whatever the engine runs: no evidence, so a spent bound is not
// re-armed by a controller upgrade.
func TestAStaleFingerprintVersionIsNoEvidence(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	lbSpend(t, a, own, clock)
	a.mu.Lock()
	a.lowerBounds[own].args = "hashed-under-the-previous-version"
	a.lowerBounds[own].argsVersion = capacity.FingerprintVersion - 1
	a.mu.Unlock()
	*clock = clock.Add(time.Minute)
	if b, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); ok {
		t.Errorf("a fingerprint from another version re-armed the bound at %d", b.k2)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if got := a.lowerBounds[own]; got.args != mtParams(256, "vllm:v1").ArgsFingerprint() || got.argsVersion != capacity.FingerprintVersion {
		t.Errorf("the bound kept the other version's arguments as evidence: %q v%d", got.args, got.argsVersion)
	}
}

// A window still tagged with arguments other than the ones its variant is
// exported under goes out put aside, with the previous figure a read would
// have made of it -- not among the variant's windows, where a controller from
// before the tags would install it as its own. A window under the variant's
// arguments carries no tag of its own.
func TestAnExportedWindowIsSortedByItsArguments(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	key := rrKey(a, "variant-a")

	s := a.ExportLearnedState()
	if _, ok := s.K2[key]; !ok {
		t.Fatal("precondition: the window is exported")
	}
	if tag, ok := s.K2Args[key]; ok {
		t.Errorf("a window under the variant's own arguments repeats them: %q", tag)
	}

	waSetParams(store, mtParams(64, "vllm:v1")) // rolled out; nothing read the window since
	s = a.ExportLearnedState()
	if _, ok := s.K2[key]; ok {
		t.Error("a window learned under 256 sequences is exported among the 64-sequence variant's own")
	}
	if _, ok := s.K2Stash[stashKey(key, mtParams(256, "vllm:v1").ArgsFingerprint())]; !ok {
		t.Error("the window is not exported put aside under the arguments it was written under")
	}
	if got := s.LowerBounds[key]; got.Prior != 4000 {
		t.Errorf("exported previous figure %d, want 4000", got.Prior)
	}
}

// A window put aside survives a restart: rolled back after it, the variant
// gets it back as its own.
func TestAStashedWindowSurvivesARestart(t *testing.T) {
	first, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, first, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	waRun(t, first, clock, 1000, 0)

	second, store2, clock2 := waProcess(mtParams(64, "vllm:v1"))
	if rep := second.RestoreLearnedState(first.ExportLearnedState()); rep.Stashed != 1 {
		t.Fatalf("precondition: one window restored put aside, report %+v", rep)
	}
	waRun(t, second, clock2, 1000, 0)
	waSetParams(store2, mtParams(256, "vllm:v1"))
	if vc := waRun(t, second, clock2, 1000, 0); vc.K2Source != capacity.K2SrcHistorical.String() || vc.PerReplicaCapacity != 4000 {
		t.Errorf("rolled back after a restart: %v from %s, want its own 4000", vc.PerReplicaCapacity, vc.K2Source)
	}
}

// Flushing the learned state leaves the working memories a flush is not
// about -- decode's recent saturation, the fleet's bucket hysteresis, the ITL
// samples not yet fitted, the pods whose start is counted.
func TestAFlushLeavesTheWorkingMemories(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	v := reflect.ValueOf(a).Elem()
	a.mu.Lock()
	for _, m := range a.learnedMapsLocked() {
		f := v.FieldByName(m.name)
		f = reflect.NewAt(f.Type(), f.Addr().UnsafePointer()).Elem()
		f.SetMapIndex(reflect.ValueOf("k"), reflect.Zero(f.Type().Elem()))
	}
	a.mu.Unlock()
	a.itlTracker.SetBaseline("k", 0.01)
	a.ForgetLearnedState()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.learnedMapsLocked() {
		n := v.FieldByName(m.name).Len()
		if m.flushed && n != 0 {
			t.Errorf("%s survived a flush", m.name)
		}
		if !m.flushed && n != 1 {
			t.Errorf("a flush cleared the working memory %s", m.name)
		}
	}
	if n := len(a.itlTracker.Baselines()); n != 0 {
		t.Errorf("%d ITL baselines survived a flush", n)
	}
	for _, name := range []string{"decodeSaturatedAt", "fleetShape", "startSeenPods", "startOutliers"} {
		if v.FieldByName(name).Len() != 1 {
			t.Errorf("a flush cleared %s", name)
		}
	}
}

// A restore whose arguments differ installs the throughput windows too, each
// under the arguments it was written under: one written under the arguments
// the variant runs now -- rolled back to them while the controller was down --
// is its own service rate.
func TestAPriorRestoreKeepsAServiceRateOfTheCurrentArguments(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key := a.throughputKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleDecode, 1000, 1000, 5)
	s := LearnedState{
		Config:         map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").Fingerprint()},
		Args:           map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").ArgsFingerprint()},
		Throughput:     map[string]ThroughputSnapshot{key: {RollingAverageSnapshot: rrWindow(3).Snapshot()}},
		ThroughputArgs: map[string]string{key: mtParams(64, "vllm:v1").ArgsFingerprint()},
	}
	a.RestoreLearnedState(s)
	out, ok := a.installRestored("test-ns", "test-model", "variant-a")
	if !ok || out.Outcome != RestoredPrior {
		t.Fatalf("precondition: the arguments differ, outcome %v (ok %v)", out.Outcome, ok)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.saturatedThroughput[key]; !ok {
		t.Error("a service rate measured under the arguments the variant runs was dropped")
	}
}

// The restore report says why a lower bound was not installed.
func TestARestoreReportSaysWhyABoundWasNotInstalled(t *testing.T) {
	a, _, clock := waProcess(mtParams(256, "vllm:v1"))
	key := rrKey(a, "variant-a")
	live := rrKey(a, "variant-z")
	a.mu.Lock()
	a.lowerBounds[live] = &lowerBound{armed: true, since: *clock, tick: *clock, seen: *clock}
	a.mu.Unlock()
	rep := a.RestoreLearnedState(LearnedState{
		LowerBounds: map[string]LowerBoundSnapshot{
			key:  {Armed: true, Since: clock.Add(time.Hour), Seen: *clock},
			live: {Armed: true, Since: *clock, Seen: *clock},
		},
		K2BorrowedSince: map[string]time.Time{rrKey(a, "variant-y"): clock.Add(time.Hour)},
	})
	if rep.Unusable != 2 || rep.Aged != 0 || rep.BorrowClocks != 0 {
		t.Errorf("report %+v: want the two clocks in the future as unusable, the live one not counted", rep)
	}
}

// rrAnalyzeTwo runs one cycle of variant-a on two saturated replicas.
func rrAnalyzeTwo(t *testing.T, a *SaturationAnalyzer, tokens int64, queue int) {
	t.Helper()
	states := []domain.VariantReplicaState{{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 2, GPUsPerReplica: 1}}
	in := makeAnalyzerInput([]domain.ReplicaMetrics{
		makeReplicaMetrics("pod-a", "variant-a", tokens, 16000, queue, 100, 50),
		makeReplicaMetrics("pod-b", "variant-a", tokens, 16000, queue, 100, 50),
	}, states)
	if _, err := a.Analyze(context.Background(), in); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
}

// Mid-rollout to other engine arguments, a saturated reading may be an old
// pod's under arguments the record no longer names, and is not recorded --
// on any replica of the cycle, the second included, which reads the record
// the first one's live write left behind. Once the rollout completes it is.
func TestAReadingMidRolloutIsNotRecorded(t *testing.T) {
	a, store, clock := waProcess(mtParams(64, "vllm:v1"))
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{
		EngineParams: mtParams(64, "vllm:v1"), RollingOut: true,
	}, time.Now())
	rrAnalyzeTwo(t, a, 4000, 6)
	*clock = clock.Add(time.Minute)
	a.mu.Lock()
	windows := len(a.computeCapacityHistory)
	a.mu.Unlock()
	if windows != 0 {
		t.Errorf("a reading mid-rollout was recorded (%d windows)", windows)
	}

	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(64, "vllm:v1")}, time.Now())
	rrAnalyzeTwo(t, a, 4000, 6)
	a.mu.Lock()
	windows = len(a.computeCapacityHistory)
	a.mu.Unlock()
	if windows != 1 {
		t.Errorf("the control: with the rollout done the reading is recorded, got %d windows", windows)
	}
}

// A bound armed under a configuration not read completely recorded no
// arguments: the configuration read completely later is no evidence against
// it, and a spent bound is not re-armed by the parse changing.
func TestAnUnreadConfigurationIsNoEvidenceForABound(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	unread := mtParams(256, "vllm:v1")
	unread.Unresolved = []string{"gpu_memory_utilization"}
	if _, ok := a.borrowedK2(own, "test-model", 12800, unread, true, true); !ok {
		t.Fatal("precondition: variant-b borrows")
	}
	rrServe(a, own, clock, BorrowedK2Hold+BorrowedK2Relax+time.Minute)
	if _, ok := a.borrowedK2(own, "test-model", 12800, unread, true, true); ok {
		t.Fatal("precondition: the bound is spent")
	}
	read := mtParams(256, "vllm:v1")
	read.EnforceEager = true
	*clock = clock.Add(time.Minute)
	if b, ok := a.borrowedK2(own, "test-model", 12800, read, true, true); ok {
		t.Errorf("a configuration read completely after an unread one re-armed the bound at %d", b.k2)
	}
}
