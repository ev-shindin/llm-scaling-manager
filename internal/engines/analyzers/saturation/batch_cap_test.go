package saturation

import (
	"fmt"
	"testing"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// learnCap runs rig through cycles of one aggregated replica of v holding
// perRank sequences with queue 3, and returns the last cycle's price.
func learnCap(t *testing.T, rig *propRig, perRank, cycles int) float64 {
	t.Helper()
	rm := capRow("r0", perRank, 1, 3, false)
	var res *domain.AnalyzerResult
	var err error
	for range cycles {
		if res, err = rig.analyze([]domain.ReplicaMetrics{rm}, aggOne(rm), 12, propCycle); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := vPerReplica(res)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The cap's sources in order: the flags' cap, then the declared one, then
// the learned one only where it is lower than the default, then the default.
func TestPricingBatchCap_Sources(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	learned := func(perRank int64, params *capacity.EngineParams) {
		a.batchCaps[batchCapKey("ns", "m", "v")] = &learnedBatchCap{perRank: perRank, args: windowArgs(params)}
	}
	stated := defaultedParams()
	stated.MaxNumSeqs, stated.MaxNumSeqsSet = 64, true
	declared := defaultedParams()
	declared.DeclaredBatchCap = 96
	both := defaultedParams()
	both.MaxNumSeqs, both.MaxNumSeqsSet, both.DeclaredBatchCap = 64, true, 96

	for _, tc := range []struct {
		name     string
		params   *capacity.EngineParams
		learned  int64
		learning bool
		want     pricedBatchCap
	}{
		{"flags", stated, 0, false, pricedBatchCap{64, batchCapFromFlags}},
		{"flags win over the declared cap", both, 0, false, pricedBatchCap{64, batchCapFromFlags}},
		{"flags win over a lower learned cap", stated, 32, true, pricedBatchCap{64, batchCapFromFlags}},
		{"declared", declared, 0, false, pricedBatchCap{96, batchCapDeclared}},
		{"declared wins over a lower learned cap", declared, 32, true, pricedBatchCap{96, batchCapDeclared}},
		{"default", defaultedParams(), 0, true, pricedBatchCap{256, batchCapDefault}},
		{"learned under the default", defaultedParams(), 120, true, pricedBatchCap{120, batchCapLearned}},
		{"learned above the default is not taken", defaultedParams(), 300, true, pricedBatchCap{256, batchCapDefault}},
		{"learned, learning off", defaultedParams(), 120, false, pricedBatchCap{256, batchCapDefault}},
		{"learned with no params", nil, 120, true, pricedBatchCap{120, batchCapLearned}},
		{"nothing with no params", nil, 0, true, pricedBatchCap{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clear(a.batchCaps)
			if tc.learned > 0 {
				learned(tc.learned, tc.params)
			}
			if got := a.pricingBatchCap("ns", "m", "v", tc.params, tc.learning); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	// A cap learned under other arguments is not this engine's.
	clear(a.batchCaps)
	other := defaultedParams()
	other.EffectiveMaxBatchedTokens = 4096
	learned(120, other)
	if got := a.pricingBatchCap("ns", "m", "v", defaultedParams(), true); got.source != batchCapDefault {
		t.Errorf("a cap learned under other arguments priced the engine: %+v", got)
	}
}

// The derived k2 with no params at all: S sequences of I + O/2, B unknown.
func TestEstimateCapacityAtCap(t *testing.T) {
	if got := estimateCapacityAtCap(nil, 100, 1000, 500); got != 125_000 {
		t.Errorf("no params: %d, want 125000", got)
	}
	// B binds: 2000 x 500/1500 = 666 sequences under a cap of 1000.
	p := &capacity.EngineParams{EffectiveMaxBatchedTokens: 2000}
	nSteady := 2000.0 * 500 / 1500
	if got := estimateCapacityAtCap(p, 1000, 1000, 500); got != int64(nSteady*1250) {
		t.Errorf("B binding: %d", got)
	}
	for _, s := range []int64{0, -1} {
		if got := estimateCapacityAtCap(nil, s, 1000, 500); got != 0 {
			t.Errorf("cap %d: %d, want 0", s, got)
		}
	}
	if got := estimateCapacityAtCap(nil, 100, 1000, 0); got != 0 {
		t.Errorf("no output: %d, want 0", got)
	}
	// Params with no budget price nothing, as before the learned cap.
	if got := estimateCapacityAtCap(&capacity.EngineParams{}, 100, 1000, 500); got != 0 {
		t.Errorf("params with no budget: %d, want 0", got)
	}
}

// The learned cap is the max over plateaus: a later, higher plateau raises
// it, a lower one does not lower it.
func TestBatchCap_LearnedIsTheMaxOverPlateaus(t *testing.T) {
	rig := batchCapRig(defaultedParams(), batchCapPolicy, nil)
	learnCap(t, rig, 80, 4)
	if got := rig.learnedCap(); got != 80 {
		t.Fatalf("after a plateau at 80: %d", got)
	}
	learnCap(t, rig, 120, 4)
	if got := rig.learnedCap(); got != 120 {
		t.Fatalf("after a plateau at 120: %d", got)
	}
	learnCap(t, rig, 60, 4)
	if got := rig.learnedCap(); got != 120 {
		t.Fatalf("a lower plateau moved it: %d", got)
	}
}

// Learning is opt-in: with the policy off nothing is learned, and the price
// is what it was before the learner.
func TestBatchCap_LearningIsOptIn(t *testing.T) {
	rig := batchCapRig(defaultedParams(), nil, nil)
	got := learnCap(t, rig, 120, 13)
	if rig.learnedCap() != 0 {
		t.Fatalf("learned %d with the policy off", rig.learnedCap())
	}
	if want := k1Truth; got != want {
		t.Fatalf("priced %.0f with the policy off, want k1 %.0f", got, want)
	}
}

// SwitchBatchCapLearned: off, a policy that enables learning learns nothing
// and prices nothing from a cap on record.
func TestBatchCapSwitch_LearnedOffLearnsNothing(t *testing.T) {
	on := batchCapRig(defaultedParams(), batchCapPolicy, nil)
	if got := learnCap(t, on, 120, 13); got != float64(120*batchCapPerSeq) {
		t.Fatalf("switch on: priced %.0f, want %d", got, 120*batchCapPerSeq)
	}
	off := batchCapRig(defaultedParams(), batchCapPolicy, func(a *SaturationAnalyzer) {
		if err := a.SetSwitchesForReplay(map[string]bool{SwitchBatchCapLearned: false}); err != nil {
			t.Fatal(err)
		}
	})
	if got := learnCap(t, off, 120, 13); got != k1Truth || off.learnedCap() != 0 {
		t.Fatalf("switch off: priced %.0f, learned %d; want k1 %.0f and nothing", got, off.learnedCap(), k1Truth)
	}
	// A cap already on record is not read either.
	off.a.batchCaps[batchCapKey("test-ns", "test-model", "v")] = &learnedBatchCap{perRank: 120, args: windowArgs(defaultedParams())}
	if got := learnCap(t, off, 120, 1); got != k1Truth {
		t.Fatalf("switch off read a cap on record: %.0f", got)
	}
}

// SwitchBatchCapDeclared: off, the annotation's cap prices nothing.
func TestBatchCapSwitch_DeclaredOffPricesTheDefault(t *testing.T) {
	params := defaultedParams()
	params.DeclaredBatchCap = 120
	on := batchCapRig(params, nil, nil)
	if got := learnCap(t, on, 120, 2); got != float64(120*batchCapPerSeq) {
		t.Fatalf("switch on: priced %.0f, want %d", got, 120*batchCapPerSeq)
	}
	off := batchCapRig(params, nil, func(a *SaturationAnalyzer) {
		if err := a.SetSwitchesForReplay(map[string]bool{SwitchBatchCapDeclared: false}); err != nil {
			t.Fatal(err)
		}
	})
	if got := learnCap(t, off, 120, 2); got != k1Truth {
		t.Fatalf("switch off: priced %.0f, want k1 %.0f", got, k1Truth)
	}
}

// The learned cap is per engine rank: a DP=8 replica holding 8 x 100 learns
// 100, and prices 8 x 100 sequences.
func TestBatchCap_PerRank(t *testing.T) {
	rig := batchCapRig(defaultedParams(), batchCapPolicy, nil)
	rows, states := fleetOf(batchCapTopology{n: 1, ranks: 8}, func(_ int, rm *domain.ReplicaMetrics) {
		*rm = capRow("r0", 100, 8, 2, false)
	})
	var res *domain.AnalyzerResult
	var err error
	for range 4 {
		if res, err = rig.analyze(rows, states, 12, propCycle); err != nil {
			t.Fatal(err)
		}
	}
	if got := rig.learnedCap(); got != 100 {
		t.Fatalf("learned %d per rank, want 100", got)
	}
	got, _, err := vPerReplica(res)
	if err != nil {
		t.Fatal(err)
	}
	if want := float64(8 * 100 * batchCapPerSeq); got != want {
		t.Fatalf("priced %.0f, want %.0f", got, want)
	}
}

// The negative control for #399, through the switches: with both off the
// fleet prices as it did before the fix -- k1, 1.905x the truth at S = 126
// -- on each topology the issue measured.
func TestBatchCap_SwitchesOffReproduceTheIssue(t *testing.T) {
	off := func(a *SaturationAnalyzer) {
		if err := a.SetSwitchesForReplay(map[string]bool{SwitchBatchCapLearned: false, SwitchBatchCapDeclared: false}); err != nil {
			t.Fatal(err)
		}
	}
	for _, topo := range batchCapTopologies {
		// Defaulted params price P3 at 256 sequences; none filed, P4 at k1.
		for _, params := range []*capacity.EngineParams{defaultedParams(), nil} {
			rig := batchCapRig(params, batchCapPolicy, off)
			rows, states := fleetOf(topo, func(i int, rm *domain.ReplicaMetrics) {
				*rm = capRow(fmt.Sprintf("r%d", i), 126, topo.ranks, 3, topo.pd)
			})
			var res *domain.AnalyzerResult
			var err error
			for range 13 {
				if res, err = rig.analyze(rows, states, 12, propCycle); err != nil {
					t.Fatal(err)
				}
			}
			got, reason, err := vPerReplica(res)
			if err != nil {
				t.Fatal(err)
			}
			truth := float64(126 * topo.ranks * batchCapPerSeq)
			t.Logf("%s, params %v, switches off: priced %.0f against a true %.0f, %.3fx (%s)", topo.name, params != nil, got, truth, got/truth, reason)
			if got/truth < 1.8 {
				t.Errorf("%s, params %v: switches off priced %.3fx the truth; the issue's case is 1.905x", topo.name, params != nil, got/truth)
			}
		}
	}
}

// A P/D decode at its cap on vLLM v0.20.0 or later reports its waiting
// requests deferred, none waiting for capacity, so the decode view reads no
// queue and the learner takes no sample: on a P/D decode the declared cap is
// the source.
func TestBatchCap_PDDecodeAtCapLearnsNothing(t *testing.T) {
	rig := batchCapRig(defaultedParams(), batchCapPolicy, nil)
	rows, states := fleetOf(batchCapTopologies[1], func(i int, rm *domain.ReplicaMetrics) {
		*rm = capRow(fmt.Sprintf("r%d", i), 126, 1, 3, true)
	})
	for range 13 {
		if _, err := rig.analyze(rows, states, 12, propCycle); err != nil {
			t.Fatal(err)
		}
	}
	if got := rig.learnedCap(); got != 0 {
		t.Fatalf("a P/D decode at its cap learned %d; the capacity queue reads 0 there", got)
	}
}

// The learned cap ages on its last use (S1): a fleet it prices correctly
// stops queueing, so no plateau confirms it, and it must not be swept for
// that. One nothing uses is swept after the bucket retention.
func TestBatchCap_AgesOnUse(t *testing.T) {
	rig := batchCapRig(defaultedParams(), batchCapPolicy, nil)
	learnCap(t, rig, 120, 4)
	if rig.learnedCap() != 120 {
		t.Fatalf("precondition: learned %d", rig.learnedCap())
	}
	retention := rig.a.horizons.BucketRetention
	// The fleet copes: nothing waits on its pods, and the cap prices them.
	coping := capRow("r0", 120, 1, 0, false)
	for day := range 10 {
		rig.clock = rig.clock.Add(retention / 7)
		res, err := rig.analyze([]domain.ReplicaMetrics{coping}, aggOne(coping), 12, propCycle)
		if err != nil {
			t.Fatal(err)
		}
		if ev := rig.a.evictStaleHistory(); ev.BatchCaps != 0 {
			t.Fatalf("day %d: a cap in use was swept", day)
		}
		if got, _, _ := vPerReplica(res); got != float64(120*batchCapPerSeq) {
			t.Fatalf("day %d: priced %.0f, want the learned %d", day, got, 120*batchCapPerSeq)
		}
	}
	// Unused: learning switched off, so nothing reads it.
	rig.policy = nil
	for range 2 {
		rig.clock = rig.clock.Add(retention/2 + 1)
		if _, err := rig.analyze([]domain.ReplicaMetrics{coping}, aggOne(coping), 12, propCycle); err != nil {
			t.Fatal(err)
		}
	}
	rig.clock = rig.clock.Add(1)
	if ev := rig.a.evictStaleHistory(); ev.BatchCaps != 1 || rig.learnedCap() != 0 {
		t.Fatalf("an unused cap past the retention: evicted %d, left %d", ev.BatchCaps, rig.learnedCap())
	}
}

// A change of engine arguments resets the cap through the live path: the
// old cap prices nothing under the new arguments, and the first plateau
// under them is the cap, however much lower.
func TestBatchCap_ArgsChangeResets(t *testing.T) {
	rig := batchCapRig(defaultedParams(), batchCapPolicy, nil)
	learnCap(t, rig, 120, 4)
	moved := defaultedParams()
	moved.EffectiveMaxBatchedTokens = 16384
	rig.a.capacityStore.Update("test-ns", "test-model", "v", capacity.Record{EngineParams: moved}, rig.clock)
	if got := learnCap(t, rig, 90, 1); got != k1Truth {
		t.Fatalf("under new arguments the old cap priced %.0f; want k1 %.0f", got, k1Truth)
	}
	learnCap(t, rig, 90, 3)
	if got := rig.learnedCap(); got != 90 {
		t.Fatalf("under new arguments: learned %d, want the reset 90", got)
	}
}

// A plateau a cycle has not extended within K2SettleMaxGap is swept, and a
// row after the gap starts a new one.
func TestBatchCap_PlateauSweep(t *testing.T) {
	rig := batchCapRig(defaultedParams(), batchCapPolicy, nil)
	learnCap(t, rig, 120, 2)
	rig.a.mu.Lock()
	n := len(rig.a.batchCapPlateaus)
	rig.a.mu.Unlock()
	if n != 1 {
		t.Fatalf("precondition: %d plateaus", n)
	}
	rig.clock = rig.clock.Add(K2SettleMaxGap + 1)
	rig.a.evictStaleHistory()
	rig.a.mu.Lock()
	n = len(rig.a.batchCapPlateaus)
	rig.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("a plateau past the gap was kept")
	}
	learnCap(t, rig, 120, 1)
	if rig.learnedCap() != 0 {
		t.Fatalf("one cycle after the gap completed a plateau")
	}
}

// Prefill is never priced by P3, so a prefill row learns nothing.
func TestBatchCap_PrefillLearnsNothing(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	rm := capRow("p0", 120, 1, 3, false)
	for range 4 {
		a.observeBatchCap("test-ns", "test-model", domain.RolePrefill, rm, admissionOf(rm), memoryBound(propKv, 0.8), nil, true, logr.Discard())
	}
	if len(a.batchCaps) != 0 || len(a.batchCapPlateaus) != 0 {
		t.Fatalf("a prefill row was learned from")
	}
}
