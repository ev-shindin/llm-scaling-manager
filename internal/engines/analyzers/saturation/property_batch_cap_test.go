package saturation

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The batch cap's property cases (batch_cap.go, issue #399): a
// sequence-bound engine whose flags state no cap, under flow control.
//
// Every row holds its batch at the true cap S per engine rank, with S x 1250
// tokens resident (the rig's 1000-token prompts and 500-token outputs, so
// I + O/2 = 1250): about 0.42 of its KV at S = 126, under k1 = 0.80. The
// router holds the rest of the queue (a scheduler queue of 40); the engine's
// own queue stays under the k2 gate of 5, so no P1 reading is ever taken.
// The truth is S x 1250 x ranks per replica. Before the fix every such
// replica was priced at k1: 1.905x the truth at S = 126.

const keyBatchCapLearning = "enableBatchCapLearning"

// batchCapPolicy is the policy the learned cap's cases run.
var batchCapPolicy = &config.ScalingPolicy{EnableBatchCapLearning: true}

// batchCapPerSeq is one sequence's resident tokens at the rig's shape.
const batchCapPerSeq = 1000 + 500/2

// flagsBatchCap is the batch cap computeK2 is handed for params when nothing
// but the flags (or their default) price it, as before the declared and
// learned caps.
func flagsBatchCap(params *capacity.EngineParams) pricedBatchCap {
	return (&SaturationAnalyzer{}).pricingBatchCap("", "", "", params, false)
}

// defaultedParams is an engine whose flags could not be read: max_num_seqs
// is the parser's default of 256, not stated.
func defaultedParams() *capacity.EngineParams {
	return &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256}
}

// batchCapTopology is one fleet shape of #399's measurement.
type batchCapTopology struct {
	name  string
	n     int  // replicas of variant v
	ranks int  // data-parallel ranks per replica
	pd    bool // decode of a P/D model, beside one prefill replica
}

var batchCapTopologies = []batchCapTopology{
	{name: "aggregated N=2", n: 2, ranks: 1},
	{name: "P/D decode N=2", n: 2, ranks: 1, pd: true},
	{name: "aggregated DP=8", n: 1, ranks: 8},
}

// learnableTopologies are the topologies a learned cap covers: a P/D
// decode at its cap reads no capacity queue, so it teaches nothing
// (TestBatchCap_PDDecodeAtCapLearnsNothing); the declared cap covers it.
var learnableTopologies = []batchCapTopology{batchCapTopologies[0], batchCapTopologies[2]}

// capRow is one replica of v holding perRank sequences on each of its
// ranks, with queue waiting on each rank, the ranks loaded alike.
//
// pd is a decode of a P/D model on vLLM v0.20.0 or later, at its cap: vLLM
// reports every waiting request deferred, none waiting for capacity
// (pd_decode_queue.go, as #287 measured), so the capacity queue reads 0.
func capRow(pod string, perRank, ranks, queue int, pd bool) domain.ReplicaMetrics {
	running := perRank * ranks
	rm := makeReplicaMetrics(pod, "v", int64(running*batchCapPerSeq), propKv*int64(ranks), queue, 1000, 500)
	rm.KvUsageInstant, rm.KvUsageInstantKnown = rm.KvCacheUsage, true
	rm.RequestRate = 6
	rm.GenerationTokenRate = 6 * 500
	rm.PrefillComputedTokenRate = 6 * 1000
	rm.EngineRanks = ranks
	rm.QueueLength = queue * ranks
	rm.MaxRankQueueLength = queue
	if ranks > 1 {
		rm.MaxRankKvCacheUsage = rm.KvCacheUsage
	}
	rm.RunningRequests, rm.RunningRequestsKnown = running, true
	rm.PreemptionRate, rm.PreemptionRateKnown = 0, true
	if pd {
		rm.CapacityQueueKnown = true
	}
	return rm
}

// fleetOf is topology t's rows (edit applies to each of v's) and states.
func fleetOf(t batchCapTopology, edit func(i int, rm *domain.ReplicaMetrics)) ([]domain.ReplicaMetrics, []domain.VariantReplicaState) {
	rows := make([]domain.ReplicaMetrics, 0, t.n+1)
	for i := range t.n {
		rm := domain.ReplicaMetrics{}
		edit(i, &rm)
		rows = append(rows, rm)
	}
	gpus := t.ranks
	if !t.pd {
		return rows, []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: t.n, GPUsPerReplica: gpus}}
	}
	p := satRow("p0", 20_000, 0, 6)
	p.VariantName = "p"
	rows = append(rows, p)
	return rows, []domain.VariantReplicaState{
		{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: t.n, GPUsPerReplica: gpus, Role: domain.RoleDecode},
		{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
	}
}

// batchCapRig is a rig for v with params filed (none when nil), the
// router holding 40, policy pol and mut applied to its analyzer.
func batchCapRig(params *capacity.EngineParams, pol *config.ScalingPolicy, mut func(*SaturationAnalyzer)) *propRig {
	rig := newPropRig()
	if params != nil {
		rig.a.capacityStore.Update("test-ns", "test-model", "v", capacity.Record{EngineParams: params}, rig.clock)
	}
	if pol != nil {
		rig.policy = func(p *config.ScalingPolicy) { p.EnableBatchCapLearning = pol.EnableBatchCapLearning }
	}
	rig.input = func(in *domain.AnalyzerInput) { in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 40} }
	if mut != nil {
		mut(rig.a)
	}
	return rig
}

// vPerReplica is the capacity v is priced at in res.
func vPerReplica(res *domain.AnalyzerResult) (float64, string, error) {
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "v" {
			return vc.PerReplicaCapacity, vc.Reason, nil
		}
	}
	return 0, "", errors.New("no capacity for v")
}

// learnedCap is v's learned batch cap per rank, 0 when none.
func (r *propRig) learnedCap() int64 {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	if lc := r.a.batchCaps[batchCapKey("test-ns", "test-model", "v")]; lc != nil {
		return lc.perRank
	}
	return 0
}

// pricedNear judges a capacity against the truth: over it is over-priced
// (the side that orders too few), under it biased.
func pricedNear(what string, got, truth float64) error {
	switch {
	case got > truth*(1+propBand):
		return violation(kindOverPriced, "%s: priced %.0f against a true %.0f, %.3fx", what, got, truth, got/truth)
	case got < truth*(1-propBand):
		return violation(kindBiased, "%s: priced %.0f against a true %.0f, %.3fx", what, got, truth, got/truth)
	}
	return nil
}

// checkBatchCapSeqBound is #399's case: on each learnable topology, with the engine's
// args unread (defaulted, or none filed at all) and its own queue at 1-4,
// the learned cap prices the replica at its truth once its pods have held
// their batch for a plateau.
func checkBatchCapSeqBound(mut func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		s := 110 + r.IntN(31)
		for _, t := range learnableTopologies {
			for _, params := range []*capacity.EngineParams{defaultedParams(), nil} {
				rig := batchCapRig(params, batchCapPolicy, mut)
				q := 1 + r.IntN(4)
				rows, states := fleetOf(t, func(i int, rm *domain.ReplicaMetrics) {
					*rm = capRow(fmt.Sprintf("r%d", i), s, t.ranks, q, t.pd)
				})
				var res *domain.AnalyzerResult
				var err error
				for range 13 {
					if res, err = rig.analyze(rows, states, 12, propCycle); err != nil {
						return err
					}
				}
				got, reason, err := vPerReplica(res)
				if err != nil {
					return err
				}
				what := fmt.Sprintf("%s, params %v, S=%d, engine queue %d (%s)", t.name, params != nil, s, q, reason)
				if e := pricedNear(what, got, float64(s*t.ranks*batchCapPerSeq)); e != nil {
					return e
				}
			}
		}
		return nil
	}
}

// checkBatchCapDeclared: the issue's case as stated, with the engine queue at
// 0 -- no pod shows it is full, so nothing can be learned -- passes on the
// declared cap alone, with learning off.
func checkBatchCapDeclared(seed uint64) error {
	r := propRand(seed)
	s := 110 + r.IntN(31)
	for _, t := range batchCapTopologies {
		params := defaultedParams()
		params.DeclaredBatchCap = int64(s)
		rig := batchCapRig(params, nil, nil)
		rows, states := fleetOf(t, func(i int, rm *domain.ReplicaMetrics) {
			*rm = capRow(fmt.Sprintf("r%d", i), s, t.ranks, 0, t.pd)
		})
		var res *domain.AnalyzerResult
		var err error
		for range 13 {
			if res, err = rig.analyze(rows, states, 12, propCycle); err != nil {
				return err
			}
		}
		got, reason, err := vPerReplica(res)
		if err != nil {
			return err
		}
		if e := pricedNear(fmt.Sprintf("%s, declared %d (%s)", t.name, s, reason), got, float64(s*t.ranks*batchCapPerSeq)); e != nil {
			return e
		}
	}
	return nil
}

// checkBatchCapQueueZero: with the engine queue at 0 no pod shows it is
// full -- the router judges the pool, not the pod -- so the learner takes no
// sample, and the replica stays priced where it was (k1). Not learnable per
// pod; the declared cap is the source for it.
func checkBatchCapQueueZero(seed uint64) error {
	r := propRand(seed)
	s := 110 + r.IntN(31)
	for _, t := range batchCapTopologies {
		rig := batchCapRig(defaultedParams(), batchCapPolicy, nil)
		rows, states := fleetOf(t, func(i int, rm *domain.ReplicaMetrics) {
			*rm = capRow(fmt.Sprintf("r%d", i), s, t.ranks, 0, t.pd)
		})
		for range 13 {
			if _, err := rig.analyze(rows, states, 12, propCycle); err != nil {
				return err
			}
		}
		if got := rig.learnedCap(); got != 0 {
			return violation(kindMoved, "%s: engine queue 0 learned a batch cap of %d", t.name, got)
		}
	}
	return nil
}

// unevenDPRow is a DP=8 replica with cap 128: one rank full at 128 with 3
// waiting, seven at 64 with none.
func unevenDPRow() domain.ReplicaMetrics {
	rm := capRow("r0", 0, 8, 0, false)
	running := 128 + 7*64
	rm.RunningRequests = running
	rm.TokensInUse = int64(running * batchCapPerSeq)
	rm.KvCacheUsage = float64(rm.TokensInUse) / float64(rm.TotalKvCapacityTokens)
	rm.KvUsageInstant = rm.KvCacheUsage
	rm.MaxRankKvCacheUsage = float64(128*batchCapPerSeq) / float64(propKv)
	rm.QueueLength, rm.MaxRankQueueLength = 3, 3
	return rm
}

// batchCapLow is the running count per rank a row that is NOT at its cap
// holds flat in the refusal cases: well under any S they draw.
const batchCapLow = 40

// checkBatchCapNeverUnder runs v's rows cycle by cycle (rowsAt gives cycle
// c's) with learning on, and holds every cycle's price at or over the truth
// x (1 - propBand): a refused row's low count must never become the cap.
// truth is the true per-replica capacity.
func checkBatchCapNeverUnder(name string, mut func(*SaturationAnalyzer),
	rowsAt func(c int, s int) ([]domain.ReplicaMetrics, []domain.VariantReplicaState, float64)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		s := 110 + r.IntN(31)
		rig := batchCapRig(defaultedParams(), batchCapPolicy, mut)
		for c := range 13 {
			rows, states, truth := rowsAt(c, s)
			res, err := rig.analyze(rows, states, 12, propCycle)
			if err != nil {
				return err
			}
			got, reason, err := vPerReplica(res)
			if err != nil {
				return err
			}
			if got < truth*(1-propBand) {
				return violation(kindBiased, "%s, cycle %d: priced %.0f under a true %.0f (%s), learned cap %d",
					name, c, got, truth, reason, rig.learnedCap())
			}
		}
		return nil
	}
}

// aggOne is one aggregated replica of v, at k1 priced capacity's truth: its
// rows below never reach their cap, so the default prices them.
func aggOne(rows ...domain.ReplicaMetrics) []domain.VariantReplicaState {
	return []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: len(rows), GPUsPerReplica: 1}}
}

// lowRow is an aggregated row holding batchCapLow sequences flat, queue 3:
// at its cap but for the one input edit makes.
func lowRow(edit func(*domain.ReplicaMetrics)) domain.ReplicaMetrics {
	rm := capRow("r0", batchCapLow, 1, 3, false)
	if edit != nil {
		edit(&rm)
	}
	return rm
}

// k1Truth is a replica the cases below do not let reach its cap: the
// capacity it is truly priced at is the default P3 or k1, whichever is
// lower, and with 256 sequences of 1250 tokens that is k1.
var k1Truth = float64(memoryBound(propKv, 0.8))

// The refusal cases: each puts a row that is not at its cap where the
// learner would read it, and must leave the price at k1. Each is named by
// the admission rule it holds (TestBatchCapMutants drops the rule).
func batchCapRefusalChecks(mut func(*SaturationAnalyzer)) map[string]func(uint64) error {
	low := func(edit func(*domain.ReplicaMetrics)) func(int, int) ([]domain.ReplicaMetrics, []domain.VariantReplicaState, float64) {
		return func(int, int) ([]domain.ReplicaMetrics, []domain.VariantReplicaState, float64) {
			rm := lowRow(edit)
			return []domain.ReplicaMetrics{rm}, aggOne(rm), k1Truth
		}
	}
	return map[string]func(uint64) error{
		// A warm-pool bridge runs the pool's engine.
		"not-serving": checkBatchCapNeverUnder("bridge", mut, low(func(rm *domain.ReplicaMetrics) { rm.FromWarmPool = true })),
		// A joined replica fills to 40 for 45 s, then to its cap.
		"joined": checkBatchCapNeverUnder("joined", mut, func(c, s int) ([]domain.ReplicaMetrics, []domain.VariantReplicaState, float64) {
			per := batchCapLow
			if c >= 4 {
				per = s
			}
			rm := capRow("r0", per, 1, 3, false)
			rm.ReadySince = propStart.Add(-10e9)
			return []domain.ReplicaMetrics{rm}, aggOne(rm), float64(s * batchCapPerSeq)
		}),
		// One pod loaded past k1, one held by the router with nothing
		// waiting on it: #399's 1.68x over-order.
		"no-waiting": checkBatchCapNeverUnder("router-held idle pod", mut, func(int, int) ([]domain.ReplicaMetrics, []domain.VariantReplicaState, float64) {
			loaded := capRow("r0", 0, 1, 0, false)
			loaded.TokensInUse = int64(0.9 * float64(propKv))
			loaded.KvCacheUsage, loaded.KvUsageInstant = 0.9, 0.9
			loaded.RunningRequests = 270
			idle := capRow("r1", 10, 1, 0, false)
			return []domain.ReplicaMetrics{loaded, idle}, aggOne(loaded, idle), k1Truth
		}),
		// A P/D decode whose queue may be pulls in flight.
		"decode-queue-unknown": checkBatchCapNeverUnder("decode queue unknown", mut, func(int, int) ([]domain.ReplicaMetrics, []domain.VariantReplicaState, float64) {
			rows, states := fleetOf(batchCapTopology{n: 1, ranks: 1, pd: true}, func(_ int, rm *domain.ReplicaMetrics) {
				*rm = capRow("r0", batchCapLow, 1, 3, false)
			})
			return rows, states, k1Truth
		}),
		"running-unknown": checkBatchCapNeverUnder("running unknown", mut, low(func(rm *domain.ReplicaMetrics) { rm.RunningRequestsKnown = false })),
		// Full by KV: the count is what fits, and k1 prices it.
		"kv-bound": checkBatchCapNeverUnder("kv-bound", mut, low(func(rm *domain.ReplicaMetrics) {
			rm.TokensInUse = int64(0.85 * float64(propKv))
			rm.KvCacheUsage, rm.KvUsageInstant = 0.85, 0.85
		})),
		"preempting": checkBatchCapNeverUnder("preempting", mut, low(func(rm *domain.ReplicaMetrics) { rm.PreemptionRate = 0.5 })),
		// DP=8, cap 128: one rank full and queued, seven at 64. The mean per
		// rank, 72, is not the cap.
		"uneven-ranks": checkBatchCapNeverUnder("uneven DP ranks", mut, func(int, int) ([]domain.ReplicaMetrics, []domain.VariantReplicaState, float64) {
			rows, states := fleetOf(batchCapTopology{n: 1, ranks: 8}, func(_ int, rm *domain.ReplicaMetrics) {
				*rm = unevenDPRow()
			})
			return rows, states, float64(8 * 128 * batchCapPerSeq)
		}),
		// A fill ramps through counts with requests waiting; only the
		// count it holds is its cap.
		"not-flat": checkBatchCapNeverUnder("fill ramp", mut, func(c, s int) ([]domain.ReplicaMetrics, []domain.VariantReplicaState, float64) {
			per := min(s, 20+c*20)
			rm := capRow("r0", per, 1, 3, false)
			return []domain.ReplicaMetrics{rm}, aggOne(rm), float64(s * batchCapPerSeq)
		}),
	}
}

func batchCapCases() []propCase {
	refusals := batchCapRefusalChecks(nil)
	refusal := func(class propClass, name, rule string) propCase {
		return propCase{class: class, learner: "batch-cap", name: name, family: familyDirect, tuning: tunedPolicy,
			policy: batchCapPolicy, check: refusals[rule]}
	}
	return []propCase{
		{class: classKnownTruth, learner: "batch-cap", name: "seq-bound-flow-control", family: familyDirect, tuning: tunedPolicy,
			policy: batchCapPolicy, check: checkBatchCapSeqBound(nil)},
		{class: classKnownTruth, learner: "batch-cap", name: "declared-engine-queue-0", family: familyDirect, tuning: tunedDefault,
			check: checkBatchCapDeclared},
		{class: classMissing, learner: "batch-cap", name: "engine-queue-0-learns-nothing", family: familyDirect, tuning: tunedPolicy,
			policy: batchCapPolicy, check: checkBatchCapQueueZero},
		refusal(classHeterogeneity, "bridge-row", "not-serving"),
		refusal(classTransient, "joined-fill", "joined"),
		refusal(classHeterogeneity, "router-holds-idle-pod", "no-waiting"),
		refusal(classMissing, "decode-queue-unknown", "decode-queue-unknown"),
		refusal(classMissing, "running-unknown", "running-unknown"),
		refusal(classHeterogeneity, "kv-bound-pod", "kv-bound"),
		refusal(classHeterogeneity, "preempting-pod", "preempting"),
		refusal(classTransient, "fill-ramp", "not-flat"),
		refusal(classHeterogeneity, "uneven-dp-ranks", "uneven-ranks"),
	}
}

// TestBatchCapMutants drops each admission rule (and, for not-flat, the
// plateau) and requires the case that holds it to fail: a rule no case
// needs is a rule nothing tests.
func TestBatchCapMutants(t *testing.T) {
	names := make([]string, 0, len(batchCapRules())+1)
	for _, r := range batchCapRules() {
		names = append(names, r.name)
	}
	names = append(names, "not-flat")
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			mut := func(a *SaturationAnalyzer) {
				if name == "not-flat" {
					a.batchCapPlateauCycles = 1
					return
				}
				a.batchCapAdmission = slices.DeleteFunc(batchCapRules(), func(r batchCapRule) bool { return r.name == name })
			}
			check, ok := batchCapRefusalChecks(mut)[name]
			if !ok {
				t.Fatalf("no refusal case holds the rule %q", name)
			}
			if err := check(1); err == nil {
				t.Fatalf("with %q dropped, its case still passes: the rule is untested", name)
			} else {
				t.Logf("with %q dropped: %v", name, err)
			}
			if err := batchCapRefusalChecks(nil)[name](1); err != nil {
				t.Fatalf("with every rule, the case fails: %v", err)
			}
		})
	}
}
