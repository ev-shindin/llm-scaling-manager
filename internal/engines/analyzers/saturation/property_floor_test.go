package saturation

import (
	"fmt"
	"math"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Property cases for the throughput floor (inventory row 11), read from the
// AnalyzerResult the saturation analyzer returns.

func floorCases() []propCase {
	return []propCase{
		{class: classKnownTruth, learner: "floor", name: "steady-order", family: familyDirect, tuning: tunedDefault, check: checkFloorTruth(nil)},
		{class: classMonotonicity, learner: "floor", name: "lambda-and-queue-rise", family: familyDirect, tuning: tunedDefault, check: checkFloorMonotone},
		{class: classMonotonicity, learner: "floor", name: "standing-local-queue", family: familyDirect, tuning: tunedDefault, check: checkFloorDrainBound(nil)},
		{class: classEdges, learner: "floor", name: "backlog-grew", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkBacklogEdge(tuningDefault)},
	}
}

// The floor's shipped figures the cases price against: the test policy's
// scale-up target (makeAnalyzerInput) and the drain time.
const (
	floorTarget = 0.85
	floorDrainT = 60.0 // seconds
)

// floorRig is an aggregated variant whose measured mu is about 1 req/s,
// learned on four replicas over four saturated cycles a minute apart
// (imbFloorFleetAt's seed).
func floorRig() (*propRig, error) {
	rig := newPropRig()
	for i := range 4 {
		seed := make([]domain.ReplicaMetrics, 0, 4)
		for _, p := range []string{"r0", "r1", "r2", "r3"} {
			seed = append(seed, imbRow(p, "v", 0.95, 10, 1000, 1000, []float64{1.0, 1.02, 0.98, 1.01}[i]))
		}
		if _, err := rig.analyze(seed, aggStates(4), 4, ThroughputSampleSpacing+time.Second); err != nil {
			return nil, err
		}
	}
	return rig, nil
}

// impliedReplicas is the demand a result prices, in replicas at the target.
func impliedReplicas(res *domain.AnalyzerResult) float64 {
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "v" && vc.PerReplicaCapacity > 0 {
			return res.TotalDemand / (floorTarget * vc.PerReplicaCapacity)
		}
	}
	return math.NaN()
}

// floorOrder is what the floor orders for rows at lambda, settled, in
// replicas, with the measured mu it priced from; a mutant edits it.
type floorOrder func(rows []domain.ReplicaMetrics, lambda float64) (replicas, mu float64, err error)

func floorImplied(rows []domain.ReplicaMetrics, lambda float64) (float64, float64, error) {
	rig, err := floorRig()
	if err != nil {
		return 0, 0, err
	}
	var res *domain.AnalyzerResult
	start := rig.clock
	for rig.clock.Sub(start) <= IdleSettledFor+K2SettleLookback+2*propCycle {
		if res, err = rig.analyze(rows, aggStates(len(rows)), lambda, propCycle); err != nil {
			return 0, 0, err
		}
	}
	return impliedReplicas(res), rig.measuredMu(), nil
}

// checkFloorTruth: a steady fleet with no queue -- two of its six replicas
// idle -- is ordered lambda / (mu x target), mu the measured mu the analyzer
// holds.
func checkFloorTruth(order floorOrder) func(uint64) error {
	if order == nil {
		order = floorImplied
	}
	return func(seed uint64) error {
		r := propRand(seed)
		lambda := 2 + 2*r.Float64()
		rows := make([]domain.ReplicaMetrics, 6)
		for p := range rows {
			rate, frac := lambda/4, 0.4
			if p >= 4 {
				rate, frac = 0, 0.02 // idle
			}
			rows[p] = imbRow(fmt.Sprintf("r%d", p), "v", frac, 0, 1000, 1000, rate)
		}
		got, mu, err := order(rows, lambda)
		if err != nil {
			return err
		}
		if !(mu > 0) || math.IsNaN(got) {
			return violation(kindNotLearned, "no measured mu (%.3f) or no order (%.2f)", mu, got)
		}
		want := lambda / (mu * floorTarget)
		return holds(relErr(got, want) <= propTruthTol, kindBiased, "lambda %.2f at mu %.3f: ordered %.2f replicas against %.2f", lambda, mu, got, want)
	}
}

// checkFloorDrainBound: a queue of 300 pinned on one replica, standing, beside
// idle siblings: the order is bounded by what the fleet can drain -- lambda
// plus the replica's own queue up to what it drains in the drain time
// (mu x T).
func checkFloorDrainBound(order floorOrder) func(uint64) error {
	if order == nil {
		order = floorImplied
	}
	return func(seed uint64) error {
		r := propRand(seed)
		lambda := 2 + r.Float64()
		rows := []domain.ReplicaMetrics{
			imbRow("r0", "v", 0.95, 300, 1000, 1000, 1.0),
			imbRow("r1", "v", 0.10, 0, 1000, 1000, 0.33),
			imbRow("r2", "v", 0.10, 0, 1000, 1000, 0.33),
			imbRow("r3", "v", 0.10, 0, 1000, 1000, 0.34),
		}
		got, mu, err := order(rows, lambda)
		if err != nil {
			return err
		}
		if !(mu > 0) || math.IsNaN(got) {
			return violation(kindNotLearned, "no measured mu (%.3f) or no order (%.2f)", mu, got)
		}
		bound := (lambda + math.Min(300, mu*floorDrainT)/floorDrainT) / (mu * floorTarget) * (1 + propTruthTol)
		return holds(got <= bound, kindUnbounded, "a standing queue of 300 on one replica at lambda %.2f: ordered %.2f replicas, over the %.2f it can drain", lambda, got, bound)
	}
}

// checkFloorMonotone: lambda rises and the queue with it, every replica
// busy; along the trajectory the order never falls by more than 2 %.
func checkFloorMonotone(seed uint64) error {
	r := propRand(seed)
	rig, err := floorRig()
	if err != nil {
		return err
	}
	prev, queue := 0.0, 0.0
	for i := range 40 {
		lambda := 2 + 4*float64(i)/40*(1+0.01*r.NormFloat64())
		queue += math.Max(0, lambda-4) * propCycle.Seconds() / 4
		rows := make([]domain.ReplicaMetrics, 4)
		for p := range rows {
			rows[p] = imbRow(fmt.Sprintf("r%d", p), "v", 0.95, int(queue), 1000, 1000, math.Min(lambda/4, 1))
		}
		res, err := rig.analyze(rows, aggStates(4), lambda, propCycle)
		if err != nil {
			return err
		}
		got := impliedReplicas(res)
		if math.IsNaN(got) {
			return violation(kindNotLearned, "cycle %d: no order", i)
		}
		if got < prev*(1-0.02) {
			return violation(kindFell, "cycle %d: lambda %.2f, queue %.0f per replica: the order fell %.2f -> %.2f", i, lambda, queue, prev, got)
		}
		prev = math.Max(prev, got)
	}
	return nil
}

// checkBacklogEdge: the backlog stands at 50 with 2 % autocorrelated noise; the
// noise-free backlog never grows, so backlogGrew never sets.
func checkBacklogEdge(tn config.AnalyzerTuning) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		a := NewSaturationAnalyzer(capacity.NewStore())
		clock := propStart
		a.now = func() time.Time { return clock }
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		noise := arNoise(r, 100, 0.02, 0.8)
		v := make([]bool, len(noise))
		for i, e := range noise {
			v[i] = a.noteBacklogGrowth("ns", "m", map[string]float64{"decode": 50 * (1 + e)}, t)["decode"]
			clock = clock.Add(propCycle)
		}
		return holds(reversals(v) == 0, kindFlipped, "a standing backlog of 50 with 2%% noise read as grown: %d reversals in %d cycles", reversals(v), len(v))
	}
}
