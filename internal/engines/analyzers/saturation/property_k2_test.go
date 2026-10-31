package saturation

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Property cases for k2 (inventory row 3) and the fleet demand priced from
// the same rows, all through SaturationAnalyzer.Analyze.

func k2Cases() []propCase {
	return []propCase{
		{class: classTransient, learner: "k2", name: "load-step", family: familyDirect, tuning: tunedDefault, check: checkK2LoadStep(nil)},
		{class: classTransient, learner: "k2", name: "restart-same-name", family: familyDirect, tuning: tunedDefault, check: checkK2Restart(nil)},
		{class: classTransient, learner: "k2", name: "slow-fill", family: familyTimeBase, tuning: tunedDefault, check: checkK2Fill(nil)},
		{class: classFirstSample, learner: "k2", name: "second-reading", family: familyDirect, tuning: tunedDefault, check: checkK2FirstSample},
		{class: classStale, learner: "k2", name: "converges-to-truth", family: familyDirect, tuning: tunedDefault, check: checkK2Tracks},
		{class: classMissing, learner: "k2", name: "unknown-gauges", family: familyDirect, tuning: tunedDefault, check: checkK2UnknownGauges},
		{class: classRedistribution, learner: "k2", name: "queue-moved", family: familyDirect, tuning: tunedDefault, check: checkK2Redistribution(nil)},
		{class: classRedistribution, learner: "demand", name: "alternating-pods", family: familyTimeBase, tuning: tunedDefault, check: checkDemandAlternating(nil)},
		{class: classKnownTruth, learner: "k2", name: "priced-per-replica", family: familyDirect, tuning: tunedDefault, check: checkK2PricedTruth},
		{class: classScale, learner: "k2", name: "block-size-and-cycle", family: familyDirect, tuning: tunedDefault, check: checkK2Scale(nil)},
		{class: classLiveness, learner: "k2", name: "n2-aggregated", family: familyDirect, tuning: tunedDefault, check: checkK2Liveness("aggregated")},
		{class: classLiveness, learner: "k2", name: "pd-decode", family: familyDirect, tuning: tunedDefault, check: checkK2Liveness("pd")},
		{class: classLiveness, learner: "k2", name: "flow-control-on", family: familyDirect, tuning: tunedDefault, check: checkK2Liveness("flow-control")},
		{class: classEdges, learner: "k2", name: "key-near-output-boundary", family: familyDirect, tuning: tunedDefault, seeds: 50, check: checkK2KeyEdge},
		{class: classEdges, learner: "k2", name: "key-follows-output-length", family: familyDirect, tuning: tunedDefault, check: checkK2KeyFollows(nil)},
	}
}

// k2Run is one replica r0 of variant v cycled through readings: tokens held,
// queue, completions a second, and the time to the next cycle.
type k2Reading struct {
	tokens int64
	queue  int
	rate   float64
	step   time.Duration
}

func (r *propRig) runK2(readings ...k2Reading) error {
	for _, k := range readings {
		if _, err := r.analyze([]domain.ReplicaMetrics{satRow("r0", k.tokens, k.queue, k.rate)}, decodeStates(1), 6, k.step); err != nil {
			return err
		}
	}
	return nil
}

// checkK2LoadStep is TestK2Settle_ALoadStepIsNotALimit with seeded levels: a
// deep queue lands on an idle replica, which reads 8 % of its cache. The
// step's reading is refused.
//
// It assumes one saturated reading is admitted: the k2 on record before the
// step comes from a single reading. The fix for first-sample/k2/second-reading
// (withhold until a second reading) breaks that premise, and this case then
// fails as not-learned. The owner of that ledger row reworks this case in the
// same PR, with two saturated readings before the step.
func checkK2LoadStep(policy func(*config.ScalingPolicy)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		rig := newPropRig()
		rig.policy = policy
		full := int64(270_000 * (1 + 0.05*r.Float64()))
		if err := rig.runK2(k2Reading{full, 30, 6, 65 * time.Second}, k2Reading{12_000, 0, 0.0024, propCycle}); err != nil {
			return err
		}
		before := rig.k2("")
		if !(before > 0) {
			return violation(kindNotLearned, "no k2 on record before the step: the saturated reading was not admitted")
		}
		if err := rig.runK2(k2Reading{int64(float64(propKv) * (0.07 + 0.02*r.Float64())), 160, 0.0019, propCycle}); err != nil {
			return err
		}
		after := rig.k2("")
		return holds(relErr(after, before) <= 1e-9, kindTransient, "a load step's reading moved k2 %.0f -> %.0f", before, after)
	}
}

// checkK2Restart: a replica restarted under the same name (a new Ready time)
// reads a steady, plausible figure from its first cycle. Nothing is admitted
// within a minute of its Ready time.
func checkK2Restart(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return checkK2RestartOn(wrap, nil)
}

// checkK2RestartOn is checkK2Restart with edit applied to the rig's analyzer
// first (a mutant), and counts, when it is not nil, the analyzer's path counts
// at the end of each seed's run.
func checkK2RestartOn(wrap func(*domain.ReplicaMetrics), edit func(*SaturationAnalyzer), counts ...func(map[string]int64)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		rig := newPropRig()
		rig.wrap = wrap
		if edit != nil {
			edit(rig.a)
		}
		for _, c := range counts {
			defer func() { c(rig.a.PathCounts()) }()
		}
		before := int64(200_000 * (1 + 0.05*r.Float64()))
		ready := propStart.Add(-time.Hour)
		cycle := func(tokens int64) error {
			rm := satRow("r0", tokens, 10, 6)
			rm.ReadySince = ready
			_, err := rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 6, propCycle)
			return err
		}
		for range 12 {
			if err := cycle(before); err != nil {
				return err
			}
		}
		held := rig.k2("")
		if !(held > 0) {
			return violation(kindNotLearned, "no k2 on record before the restart")
		}
		ready = rig.clock.Add(-5 * time.Second)
		for i := range 3 {
			if err := cycle(int64(float64(before) * 0.78)); err != nil {
				return err
			}
			if got := rig.k2(""); relErr(got, held) > 1e-9 {
				return violation(kindTransient, "%s after its Ready time, the restarted replica's reading moved k2 %.0f -> %.0f",
					time.Duration(i)*propCycle+5*time.Second, held, got)
			}
		}
		return nil
	}
}

// checkK2Fill: a replica idles for two minutes, then the load lands -- a
// standing queue -- and its batch fills toward the plateau with a one-minute
// time constant; its KV reaches the analyzer as the max_over_time[1m] peak. No
// reading under 0.85 of the plateau is admitted.
func checkK2Fill(policy func(*config.ScalingPolicy)) func(uint64) error {
	return checkK2FillOn(func() k2FillLearner {
		rig := newPropRig()
		rig.policy = policy
		return rigFill{rig}
	})
}

// k2FillLearner is k2 as checkK2FillOn drives it: one replica's reading a
// cycle, and the figure on record.
type k2FillLearner interface {
	reading(tokens int64, queue int) error
	figure() float64
}

// rigFill is the production k2, through Analyze.
type rigFill struct{ rig *propRig }

func (f rigFill) reading(tokens int64, queue int) error {
	return f.rig.runK2(k2Reading{tokens, queue, 0, propCycle})
}
func (f rigFill) figure() float64 { return f.rig.k2("") }

// toyF1Settle is #282 F1's settle: a saturated reading whose queue is deeper
// than its resident batch (queue x footprint over the tokens held) is a fill
// suspect, and is refused once; the next is admitted whatever it reads. The
// figure is the admitted readings' mean.
type toyF1Settle struct {
	refused   bool
	sum       float64
	n         int
	threshold int
}

// toyFootprint is a waiting request's KV at satRow's shape: 1000 in, half of
// 500 out.
const toyFootprint = 1250

func (t *toyF1Settle) reading(tokens int64, queue int) error {
	if queue < t.threshold {
		return nil
	}
	if suspect := float64(queue)*toyFootprint > float64(tokens); suspect && !t.refused {
		t.refused = true
		return nil
	}
	t.refused = false
	t.sum += float64(tokens)
	t.n++
	return nil
}

func (t *toyF1Settle) figure() float64 {
	if t.n == 0 {
		return 0
	}
	return t.sum / float64(t.n)
}

// checkK2FillOn is checkK2Fill on the learner newL makes.
func checkK2FillOn(newL func() k2FillLearner) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		plateau := 240_000 * (1 + 0.05*r.Float64())
		const idleFor = 120
		path := make([]float64, 7*60)
		for s := range path {
			path[s] = 12_000
			if s >= idleFor {
				path[s] = plateau - (plateau-12_000)*math.Exp(-float64(s-idleFor)/60)
			}
		}
		tb := sampleTimeBase(path)
		l := newL()
		for i, tok := range tb.peak1m {
			queue := 0
			if 60+i*int(propCycle/time.Second) >= idleFor {
				queue = 200
			}
			if err := l.reading(int64(tok), queue); err != nil {
				return err
			}
			if got := l.figure(); got > 0 && got < 0.85*plateau {
				return violation(kindTransient, "cycle %d: k2 %.0f admitted while the replica filled toward %.0f (%.0f%%)", i, got, plateau, 100*got/plateau)
			}
		}
		return holds(l.figure() > 0, kindNotLearned, "the filled replica was never admitted")
	}
}

// checkK2FirstSample: a first admitted reading 12 % off the truth, then one
// 12 % off the other way. The figure the first set moves by at most the
// band. A first reading withheld until a second is a pass.
func checkK2FirstSample(seed uint64) error {
	r := propRand(seed)
	sign := 1.0
	if r.IntN(2) == 0 {
		sign = -1
	}
	truth := 250_000.0 * (1 + 0.1*r.Float64())
	rig := newPropRig()
	if err := rig.runK2(k2Reading{int64(truth * (1 + 0.12*sign)), 10, 6, propCycle}); err != nil {
		return err
	}
	first := rig.k2("")
	if err := rig.runK2(k2Reading{int64(truth * (1 - 0.12*sign)), 10, 6, propCycle}); err != nil {
		return err
	}
	second := rig.k2("")
	if !(second > 0) {
		return violation(kindNotLearned, "two saturated readings and no k2")
	}
	if !(first > 0) {
		return nil // withheld until a second reading
	}
	return holds(relErr(second, first) <= propBand, kindAdoptedRaw, "k2 %.0f from its first reading, %.0f after a second (%.0f%%)", first, second, 100*relErr(second, first))
}

// checkK2Tracks: the first admitted reading is 20 % low; then a dozen readings
// at the truth with 1 % noise. k2 ends within the band of the truth: a k2
// frozen at its first reading fails.
func checkK2Tracks(seed uint64) error {
	r := propRand(seed)
	truth := 250_000.0 * (1 + 0.1*r.Float64())
	rig := newPropRig()
	readings := make([]k2Reading, 0, 13)
	readings = append(readings, k2Reading{int64(truth * 0.8), 10, 6, propCycle})
	for range 12 {
		readings = append(readings, k2Reading{int64(truth * (1 + 0.01*r.NormFloat64())), 10, 6, propCycle})
	}
	if err := rig.runK2(readings...); err != nil {
		return err
	}
	got := rig.k2("")
	if !(got > 0) {
		return violation(kindNotLearned, "no k2 after 13 saturated readings")
	}
	return holds(relErr(got, truth) <= propBand, kindStale, "k2 %.0f after a dozen readings at %.0f", got, truth)
}

// checkK2PricedTruth: a replica saturated at the truth for 3 min prices a
// per-replica capacity no higher than the truth x (1 + epsilon), read from the
// AnalyzerResult.
func checkK2PricedTruth(seed uint64) error {
	r := propRand(seed)
	truth := 250_000.0 * (1 + 0.1*r.Float64())
	rig := newPropRig()
	var res *domain.AnalyzerResult
	for range 13 {
		var err error
		if res, err = rig.analyze([]domain.ReplicaMetrics{satRow("r0", int64(truth*(1+0.01*r.NormFloat64())), 10, 6)}, decodeStates(1), 6, propCycle); err != nil {
			return err
		}
	}
	per := math.NaN()
	for _, vc := range res.VariantCapacities {
		per = vc.PerReplicaCapacity
	}
	if !(per > 0) {
		return violation(kindNotLearned, "no per-replica capacity priced")
	}
	return holds(per <= truth*(1+propTruthTol), kindOverPriced, "per-replica capacity %.0f against a replica holding %.0f at saturation", per, truth)
}

// checkK2UnknownGauges: a cycle whose row has every gauge unknown -- a failed
// scrape, the collector's zeros -- with a scheduler queue standing leaves k2
// where it was.
func checkK2UnknownGauges(seed uint64) error {
	r := propRand(seed)
	rig := newPropRig()
	tok := int64(260_000 * (1 + 0.05*r.Float64()))
	for range 6 {
		if err := rig.runK2(k2Reading{tok, 10, 6, propCycle}); err != nil {
			return err
		}
	}
	held := rig.k2("")
	if !(held > 0) {
		return violation(kindNotLearned, "no k2 on record before the failed scrape")
	}
	blank := domain.ReplicaMetrics{PodName: "r0", VariantName: "v", Ready: true, ModelID: "test-model", Namespace: "test-ns"}
	rig.input = func(in *domain.AnalyzerInput) { in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 200} }
	if _, err := rig.analyze([]domain.ReplicaMetrics{blank}, decodeStates(1), 6, propCycle); err != nil {
		return err
	}
	return holds(relErr(rig.k2(""), held) <= 1e-9, kindMoved, "a failed scrape moved k2 %.0f -> %.0f", held, rig.k2(""))
}

// checkK2Redistribution: four replicas at the same full cache; the queue
// spread over all four, or moved onto one with the others draining. The k2
// learned is the same within the transient tolerance.
func checkK2Redistribution(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		full := int64(float64(propKv) * (0.9 + 0.05*r.Float64()))
		run := func(queues []int, tokens []int64) (float64, error) {
			rig := newPropRig()
			rig.wrap = wrap
			for range 8 {
				rows := make([]domain.ReplicaMetrics, 4)
				for p := range rows {
					rows[p] = satRow(fmt.Sprintf("r%d", p), tokens[p], queues[p], 6)
				}
				if _, err := rig.analyze(rows, decodeStates(4), 24, propCycle); err != nil {
					return 0, err
				}
			}
			return rig.k2(""), nil
		}
		spread, err := run([]int{10, 10, 10, 10}, []int64{full, full, full, full})
		if err != nil {
			return err
		}
		if !(spread > 0) {
			return violation(kindNotLearned, "no k2 with the queue spread")
		}
		half := full / 2
		moved, err := run([]int{40, 0, 0, 0}, []int64{full, half, half, half})
		if err != nil {
			return err
		}
		return holds(relErr(moved, spread) <= propTransientTol, kindMoved, "k2 %.0f with the queue spread, %.0f with it moved onto one replica", spread, moved)
	}
}

// checkDemandAlternating: two replicas share a constant load, either evenly or
// alternating every 30 s; Prometheus serves each pod's one-minute peak and its
// instant reading. Fleet demand is the same within 10 %.
func checkDemandAlternating(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		total := 0.6 + 0.2*r.Float64()
		const secs = 5 * 60
		even, alt0, alt1 := make([]float64, secs), make([]float64, secs), make([]float64, secs)
		for s := range secs {
			even[s] = total / 2
			if (s/30)%2 == 0 {
				alt0[s], alt1[s] = total, 0.02
			} else {
				alt0[s], alt1[s] = 0.02, total
			}
		}
		demand := func(p0, p1 []float64) (float64, error) {
			tb0, tb1 := sampleTimeBase(p0), sampleTimeBase(p1)
			rig := newPropRig()
			rig.wrap = wrap
			var res *domain.AnalyzerResult
			for i := range tb0.instant {
				var err error
				if res, err = rig.analyze([]domain.ReplicaMetrics{tbRow("r0", tb0, i), tbRow("r1", tb1, i)}, decodeStates(2), 0, propCycle); err != nil {
					return 0, err
				}
			}
			return res.TotalDemand, nil
		}
		de, err := demand(even, even)
		if err != nil {
			return err
		}
		if !(de > 0) {
			return violation(kindNotLearned, "no demand with the load even")
		}
		da, err := demand(alt0, alt1)
		if err != nil {
			return err
		}
		return holds(relErr(da, de) <= 0.10, kindMoved, "fleet demand %.0f with the load even, %.0f with it alternating (%+.0f%%)", de, da, 100*(da/de-1))
	}
}

// tbRow is a decode replica whose KV is sampled from tb at cycle i: the peak
// as KvCacheUsage and TokensInUse, the instant as KvUsageInstant.
func tbRow(pod string, tb timeBase, i int) domain.ReplicaMetrics {
	rm := satRow(pod, int64(tb.peak1m[i]*float64(propKv)), 0, 1)
	rm.KvUsageInstant, rm.KvUsageInstantKnown = tb.instant[i], true
	return rm
}

// checkK2Scale: the same fleet described in 16- and 32-token blocks, and
// cycled at 15 s and 30 s, learns the same k2 and prices the same per-replica
// capacity.
func checkK2Scale(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		tok := int64(float64(propKv) * (0.85 + 0.1*r.Float64()))
		run := func(block int64, cycle time.Duration) (float64, float64, error) {
			rig := newPropRig()
			rig.wrap = wrap
			var res *domain.AnalyzerResult
			for range 8 {
				rm := satRow("r0", tok, 10, 6)
				rm.BlockSize, rm.NumGpuBlocks = block, propKv/block
				var err error
				if res, err = rig.analyze([]domain.ReplicaMetrics{rm}, decodeStates(1), 6, cycle); err != nil {
					return 0, 0, err
				}
			}
			per := math.NaN()
			for _, vc := range res.VariantCapacities {
				per = vc.PerReplicaCapacity
			}
			return rig.k2(""), per, nil
		}
		k16, p16, err := run(16, propCycle)
		if err != nil {
			return err
		}
		if !(k16 > 0 && p16 > 0) {
			return violation(kindNotLearned, "no k2 (%.0f) or per-replica capacity (%.0f) at 16 / 15 s", k16, p16)
		}
		for _, alt := range []struct {
			block int64
			cycle time.Duration
		}{{32, propCycle}, {16, 2 * propCycle}} {
			k, p, err := run(alt.block, alt.cycle)
			if err != nil {
				return err
			}
			if relErr(k, k16) > 1e-9 || relErr(p, p16) > 1e-9 {
				return violation(kindMoved, "block %d, cycle %s: k2 %.0f and per-replica %.0f, against %.0f and %.0f at 16 / 15 s",
					alt.block, alt.cycle, k, p, k16, p16)
			}
		}
		return nil
	}
}

// checkK2Liveness: a saturated fleet under each topology learns a measured k2
// within 3 min, within the band of what its replicas hold.
func checkK2Liveness(topology string) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		truth := float64(propKv) * (0.85 + 0.1*r.Float64())
		rig := newPropRig()
		var rows []domain.ReplicaMetrics
		var states []domain.VariantReplicaState
		role := domain.RoleDecode
		switch topology {
		case "aggregated":
			rows = []domain.ReplicaMetrics{satRow("r0", int64(truth), 10, 6), satRow("r1", int64(truth), 10, 6)}
			states, role = aggStates(2), ""
		case "pd":
			rows = make([]domain.ReplicaMetrics, 0, 3)
			rows = append(rows, satRow("d0", int64(truth), 10, 6), satRow("d1", int64(truth), 10, 6))
			for j := range rows {
				rows[j].CapacityQueueKnown, rows[j].CapacityQueueLength = true, 10
			}
			p := satRow("p0", 20_000, 0, 6)
			p.VariantName = "p"
			rows = append(rows, p)
			states = []domain.VariantReplicaState{decodeStates(2)[0],
				{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill}}
		case "flow-control":
			// The queue stands at the scheduler; the engines dispatch what
			// they take, so their own queues read empty.
			rows = []domain.ReplicaMetrics{satRow("r0", int64(truth), 0, 6), satRow("r1", int64(truth), 0, 6)}
			states = decodeStates(2)
			rig.input = func(in *domain.AnalyzerInput) { in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 40} }
		default:
			return fmt.Errorf("no topology %q", topology)
		}
		for range 13 {
			if _, err := rig.analyze(rows, states, 12, propCycle); err != nil {
				return err
			}
		}
		got := rig.k2(role)
		if !(got > 0) {
			return violation(kindNotLearned, "%s: after 3 min saturated at %.0f tokens, no k2", topology, truth)
		}
		return holds(relErr(got, truth) <= propBand, kindBiased, "%s: after 3 min saturated at %.0f tokens, k2 %.0f", topology, truth, got)
	}
}

// selectedK2Keys runs a two-replica saturated fleet through one cycle per
// output length, and returns the k2 key the analyzer selected each cycle, as
// its k2-decision line names it (historyKey, in logContract). wrap edits each
// row (a mutant).
func selectedK2Keys(lengths []float64, wrap func(*domain.ReplicaMetrics)) ([]string, error) {
	rig := newPropRig()
	rig.wrap = wrap
	core, logs := observer.New(zapcore.Level(-perReplicaLogLevel))
	rig.ctx = logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	keys := make([]string, 0, len(lengths))
	for _, l := range lengths {
		rows := make([]domain.ReplicaMetrics, 2)
		for p := range rows {
			rows[p] = satRow(fmt.Sprintf("r%d", p), 330_000, 10, 6)
			rows[p].AvgOutputTokens = l
		}
		if _, err := rig.analyze(rows, decodeStates(2), 12, propCycle); err != nil {
			return nil, err
		}
		key := ""
		for _, entry := range logs.TakeAll() {
			if entry.Message == "k2-decision" {
				if k, ok := entry.ContextMap()["historyKey"].(string); ok {
					key = k
				}
			}
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// keySettle is two minutes of cycles: the fleet's tracked length exists by
// then, so key changes are counted from it.
const keySettle = 8

// keyChanges counts the selected key's changes after keySettle, and is a
// not-learned violation when a cycle selected none.
func keyChanges(keys []string) (int, error) {
	changes := 0
	for i := keySettle + 1; i < len(keys); i++ {
		if keys[i] == "" || keys[i-1] == "" {
			return 0, violation(kindNotLearned, "cycle %d selected no k2 key", i)
		}
		if keys[i] != keys[i-1] {
			changes++
		}
	}
	return changes, nil
}

// checkK2KeyEdge: a saturated fleet whose output length sits 6 % above the
// 500-token medium/long boundary, with 6 % autocorrelated noise. The
// noise-free length never crosses, so the k2 key the analyzer selects --
// counted once it has selected one for two minutes -- never changes.
func checkK2KeyEdge(seed uint64) error {
	noise := arNoise(propRand(seed), 120, 0.06, 0.9)
	lengths := make([]float64, len(noise))
	for i, e := range noise {
		lengths[i] = 530 * (1 + e)
	}
	keys, err := selectedK2Keys(lengths, nil)
	if err != nil {
		return err
	}
	changes, err := keyChanges(keys)
	if err != nil {
		return err
	}
	return holds(changes == 0, kindFlipped, "output length 6 %% above the 500-token boundary: the selected k2 key changed %d times in %d cycles", changes, len(keys)-keySettle)
}

// checkK2KeyFollows is checkK2KeyEdge's positive control: the noise-free
// length steps from 400 to 700 tokens, clearly across the 500-token
// boundary, with 2 % noise. The selected key must change: a key that ignores
// the output length would pass checkK2KeyEdge for the wrong reason, and fails
// here.
func checkK2KeyFollows(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		noise := arNoise(propRand(seed), 60, 0.02, 0.9)
		lengths := make([]float64, len(noise))
		for i, e := range noise {
			lengths[i] = 400 * (1 + e)
			if i >= 30 {
				lengths[i] = 700 * (1 + e)
			}
		}
		keys, err := selectedK2Keys(lengths, wrap)
		if err != nil {
			return err
		}
		changes, err := keyChanges(keys)
		if err != nil {
			return err
		}
		return holds(changes >= 1, kindNotDetected, "the output length stepped 400 -> 700 across the 500-token boundary, and the selected k2 key never changed")
	}
}

// The k2-key edge case's positive control. edges/k2/key-near-output-boundary
// passes when the selected key never changes, which an analyzer whose key
// ignores the output length would do too. Such an analyzer -- here, rows whose
// output length the key reads held at 530 -- fails the non-ledgered
// edges/k2/key-follows-output-length.
func TestPropertyK2KeyPositiveControl(t *testing.T) {
	ignores := func(rm *domain.ReplicaMetrics) { rm.AvgOutputTokens = 530 }
	run, err := runCase(checkK2KeyFollows(ignores), []uint64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.failures) != 3 {
		t.Errorf("a k2 key that ignores the output length: %s; want every seed to fail", run)
	}
	t.Logf("a k2 key that ignores the output length: %s", run)
}
