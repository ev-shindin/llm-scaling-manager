package saturation

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// waProcess is an analyzer on its own clock, with variant-a running params.
func waProcess(params *capacity.EngineParams) (*SaturationAnalyzer, *capacity.Store, *time.Time) {
	clock := time.Now()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params}, time.Now())
	a := NewSaturationAnalyzer(store)
	a.now = func() time.Time { return clock }
	return a, store, &clock
}

// waRun runs one cycle of variant-a and moves the clock on a cycle.
func waRun(t *testing.T, a *SaturationAnalyzer, clock *time.Time, tokens int64, queue int) domain.VariantCapacity {
	t.Helper()
	vc := mtAnalyze(t, a, tokens, queue, nil)
	*clock = clock.Add(time.Minute)
	return vc
}

func waSetParams(store *capacity.Store, p *capacity.EngineParams) {
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: p}, time.Now())
}

// A rollout that changes the engine's arguments while the controller runs:
// the k2 measured under the old ones is not the variant's own figure any
// more, but it may still hold it -- lower-only, relaxing -- until the variant
// measures itself under the new ones. An image-only rollout changes nothing.
func TestAnArgumentsRolloutTurnsTheOldWindowIntoAPreviousFigure(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity != 4000 || vc.K2Source != capacity.K2SrcHistorical.String() {
		t.Fatalf("precondition: own window at 4000, got %v from %s", vc.PerReplicaCapacity, vc.K2Source)
	}

	waSetParams(store, mtParams(256, "vllm:v2"))
	if vc := waRun(t, a, clock, 1000, 0); vc.K2Source != capacity.K2SrcHistorical.String() {
		t.Errorf("an image-only rollout moved the window: source %s", vc.K2Source)
	}

	waSetParams(store, mtParams(64, "vllm:v2"))
	vc := waRun(t, a, clock, 1000, 0)
	if vc.PerReplicaCapacity != 4000 || vc.K2Source != capacity.K2SrcPrior.String() {
		t.Errorf("after an arguments rollout: %v from %s, want 4000 from P2-prior", vc.PerReplicaCapacity, vc.K2Source)
	}
	if !vc.HasK2Age {
		t.Error("a previous figure is reported with no age")
	}
	a.mu.Lock()
	windows := len(a.computeCapacityHistory)
	a.mu.Unlock()
	if windows != 0 {
		t.Errorf("the old window is still read as the variant's own (%d windows)", windows)
	}

	// The variant measures itself under the new arguments: its own window
	// wins over the previous figure at once.
	waRun(t, a, clock, 3000, 6)
	if vc := waRun(t, a, clock, 1000, 0); vc.PerReplicaCapacity != 3000 || vc.K2Source != capacity.K2SrcHistorical.String() {
		t.Errorf("an own measurement under the new arguments did not displace the previous figure: %v from %s",
			vc.PerReplicaCapacity, vc.K2Source)
	}
}

// A configuration that could not be read completely is still a
// configuration: moving to it from a complete one is a change, and the window
// learned under the complete one becomes a previous figure. Two equal
// incomplete ones are the same engine, as before tags existed, and a variant
// whose configuration is unknown moves nothing.
func TestAnUnreadConfigurationIsAConfiguration(t *testing.T) {
	a, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, a, clock, 4000, 6)
	unread := mtParams(64, "vllm:v1")
	unread.Unresolved = []string{"max_num_seqs"}
	waSetParams(store, unread)
	if vc := waRun(t, a, clock, 1000, 0); vc.K2Source != capacity.K2SrcPrior.String() || vc.PerReplicaCapacity != 4000 {
		t.Errorf("the window learned under a complete configuration was read as the unread one's own: %v from %s",
			vc.PerReplicaCapacity, vc.K2Source)
	}

	waRun(t, a, clock, 3000, 6)
	if vc := waRun(t, a, clock, 1000, 0); vc.K2Source != capacity.K2SrcHistorical.String() || vc.PerReplicaCapacity != 3000 {
		t.Errorf("a window written under the unread configuration is not its own: %v from %s", vc.PerReplicaCapacity, vc.K2Source)
	}

	// The configuration read completely again, with the flag at what the
	// unread one hashed it as: still a change.
	waSetParams(store, mtParams(64, "vllm:v1"))
	if vc := waRun(t, a, clock, 1000, 0); vc.K2Source == capacity.K2SrcHistorical.String() {
		t.Errorf("a window written while the configuration was unread was taken as the complete one's own: %v",
			vc.PerReplicaCapacity)
	}

	waRun(t, a, clock, 4500, 6)
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{}, time.Now())
	key := a.historyKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleBoth, 50, 5)
	a.mu.Lock()
	before := len(a.computeCapacityHistory)
	a.k2WindowUnderLocked(key, windowArgs(nil))
	after := len(a.computeCapacityHistory)
	a.mu.Unlock()
	if before != after {
		t.Errorf("an unknown configuration moved a window (%d -> %d)", before, after)
	}
}

// A window learned before windows recorded their arguments is taken as
// written under the ones the variant runs, and adopts them.
func TestAWindowWithNoArgumentsAdoptsTheVariants(t *testing.T) {
	a, _, clock := waProcess(mtParams(256, "vllm:v1"))
	key := a.historyKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleBoth, 50, 5)
	ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
	ra.AddAt(4000, time.Now())
	a.computeCapacityHistory[key] = ra
	if vc := waRun(t, a, clock, 1000, 0); vc.K2Source != capacity.K2SrcHistorical.String() {
		t.Fatalf("an untagged window was not read as the variant's own: %s", vc.K2Source)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if got := a.k2WindowArgs[key]; got != mtParams(256, "vllm:v1").ArgsFingerprint() {
		t.Errorf("the window adopted %q, not the variant's arguments", got)
	}
}

// A service rate is never carried across arguments: the window is dropped,
// and a neighbouring bucket's window from other arguments is not borrowed.
func TestAThroughputWindowFromOtherArgumentsIsNotRead(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	old, cur := mtParams(256, "vllm:v1").ArgsFingerprint(), mtParams(64, "vllm:v1").ArgsFingerprint()
	key := a.throughputKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleDecode, 1000, 1000, 5)
	prefix, bucket, suffix, ok := splitHistoryKey(key)
	if !ok {
		t.Fatalf("precondition: a throughput key, got %q", key)
	}
	i := slices.Index(outputBuckets, bucket)
	if i < 0 || i+1 >= len(outputBuckets) {
		t.Fatalf("precondition: a bucket with a longer neighbour, got %q", bucket)
	}
	neighbour := prefix + outputBuckets[i+1] + suffix
	window := func(rate float64) *capacity.RollingAverage {
		ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
		ra.AddAt(rate, time.Now())
		return ra
	}

	a.mu.Lock()
	a.saturatedThroughput[neighbour] = window(2)
	a.throughputWindowArgs[neighbour] = old
	a.mu.Unlock()
	if r := a.saturatedThroughputReadingUnder(key, cur, true); r.rate != 0 {
		t.Errorf("a neighbour's rate from other arguments was borrowed: %v", r.rate)
	}
	if r := a.saturatedThroughputReadingUnder(key, "", true); r.rate != 2 {
		t.Errorf("the control: with no arguments known the neighbour is read, got %v", r.rate)
	}

	a.mu.Lock()
	a.saturatedThroughput[key] = window(3)
	a.throughputSampledAt[key] = time.Now()
	a.throughputLastRead[key] = 3
	a.throughputWindowArgs[key] = old
	a.throughputWindowUnderLocked(key, cur)
	_, kept := a.saturatedThroughput[key]
	_, sampled := a.throughputSampledAt[key]
	a.mu.Unlock()
	if kept || sampled {
		t.Errorf("a throughput window from other arguments was kept (window %v, side state %v)", kept, sampled)
	}
}

// A window learned before a rollout the controller saw, exported after it,
// goes out under the arguments it was learned under -- not the variant's new
// ones -- and the next process moves it out of the way on its first read.
func TestARestoredWindowIsCheckedAgainstItsOwnArguments(t *testing.T) {
	first, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, first, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1")) // rolled out; no cycle read the window since
	raw, err := json.Marshal(first.ExportLearnedState())
	if err != nil {
		t.Fatal(err)
	}
	var saved LearnedState
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Args["test-ns/variant-a"] != mtParams(64, "vllm:v1").ArgsFingerprint() {
		t.Fatal("precondition: the variant is exported under the arguments it runs now")
	}

	second, _, clock2 := waProcess(mtParams(64, "vllm:v1"))
	second.RestoreLearnedState(saved)
	vc := waRun(t, second, clock2, 1000, 0)
	if vc.K2Source != capacity.K2SrcPrior.String() || vc.PerReplicaCapacity != 4000 {
		t.Errorf("a window learned under 256 sequences was restored as the 64-sequence engine's own: %v from %s",
			vc.PerReplicaCapacity, vc.K2Source)
	}
	if got := second.TakeRestoreOutcomes(); got[string(RestoredWhole)] != 1 {
		t.Errorf("the variant's configuration matched; outcomes %v", got)
	}
}

// disableK2Borrow turns off a sibling's figure, and pauses a bound running on
// one; a bound running on the variant's own previous figure is not a borrow
// and keeps running.
func TestTheSwitchPausesASiblingsBoundAndNotTheVariantsOwn(t *testing.T) {
	a, _, clock := waProcess(mtParams(256, "vllm:v1"))
	sibling, own := "test-ns|variant-a|test-model|H100|1|both|s|q5", "test-ns|variant-a|test-model|H100|1|both|m|q5"
	a.lowerBounds[sibling] = &lowerBound{armed: true, since: *clock, tick: *clock, donor: "variant-b"}
	a.lowerBounds[own] = &lowerBound{armed: true, since: *clock, tick: *clock, donor: "variant-a", own: true, prior: 4000}
	*clock = clock.Add(time.Minute)
	a.noteBorrowServing(sibling, *clock, false)
	a.noteBorrowServing(own, *clock, false)
	if !a.lowerBounds[sibling].tick.Equal(clock.Add(-time.Minute)) {
		t.Error("the switch did not pause a bound running on a sibling's figure")
	}
	if !a.lowerBounds[own].tick.Equal(*clock) {
		t.Error("the switch paused the variant's own previous figure")
	}
}
