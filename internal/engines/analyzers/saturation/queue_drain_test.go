package saturation

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Queued work priced by drain time before mu (queue_drain.go, #520 cause B).
// Each test runs SwitchQueuePricedAsDrain on and, as its negative control,
// off.

// drainRow is a decode replica's row: its queue, KV, and what it runs over
// its service time.
func drainRow(pod string, queue int, kv float64, running int, svc float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: pod, VariantName: "v", QueueLength: queue, KvCacheUsage: kv,
		RunningRequests: running, AvgServiceTime: svc, AvgInputTokens: 1000, AvgOutputTokens: 1000}
}

// drainCycle applies the throughput floor to one decode variant "v" of
// ready replicas (rows), pending more starting, at arrival rate lambda, each
// replica carrying mu from samples readings of its own (zero: no mu), and
// returns the demand after the floor. resident is the measured demand
// without the queues' residency charge, which is 2000 tokens a request.
func drainCycle(a *SaturationAnalyzer, lambda float64, rows []domain.ReplicaMetrics, pending int,
	mu float64, samples int, p, resident float64) float64 {
	return drainCycleShape(a, lambda, rows, pending, mu, samples, p, resident, false)
}

// drainCycleShape is drainCycle with the fleet's shape change outstanding
// (staleShape) or not.
func drainCycleShape(a *SaturationAnalyzer, lambda float64, rows []domain.ReplicaMetrics, pending int,
	mu float64, samples int, p, resident float64, staleShape bool) float64 {
	in := makeAnalyzerInput(rows, nil)
	in.ArrivalRate = lambda
	measured := resident
	rcs := make([]capacity.ReplicaCapacity, 0, len(rows))
	for _, rm := range rows {
		charge := float64(rm.QueueLength) * 2000
		measured += charge
		rcs = append(rcs, capacity.ReplicaCapacity{VariantName: "v", PodName: rm.PodName,
			QueueLength: rm.QueueLength, LocalQueueDemand: int64(charge),
			SaturatedThroughput: mu, SaturatedThroughputSamples: samples})
	}
	vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: len(rows),
		PendingReplicas: pending, PerReplicaCapacity: p}}
	return a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs,
		measured, nil, nil, 0, staleShape, logr.Discard())
}

// drainBatchCap is the batch cap drainAnalyzer's variant states.
const drainBatchCap = 16

// drainStore holds variant "v" of test-ns/test-model with a stated batch cap
// of drainBatchCap.
func drainStore(now time.Time) *capacity.Store {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "v", capacity.Record{EngineParams: &capacity.EngineParams{
		Engine: "vllm", MaxNumSeqs: drainBatchCap, MaxNumSeqsSet: true}}, now)
	return store
}

// drainAnalyzer is an analyzer with the switch set and a clock the test
// moves.
// Variant "v" states a batch cap of drainBatchCap (--max-num-seqs), so a row
// running that many is at its cap and gives a first read.
func drainAnalyzer(t *testing.T, on bool, clock *time.Time) *SaturationAnalyzer {
	t.Helper()
	a := NewSaturationAnalyzer(drainStore(*clock))
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchQueuePricedAsDrain: on}))
	a.now = func() time.Time { return *clock }
	return a
}

// pd-restart's decode role at 12:28:02, before its mu may order: two
// replicas at their cap of 16 running over 2.59 s and 2.57 s of service,
// 130 queued, 130 318 tokens resident at P 46 556, 14.18 req/s arriving.
// Occupancy charged the queue as resident (390 318, 9.86 replicas at the
// target); the drain price is (14.18 + 130 / 60) / (0.85 x 6.20) replicas
// at 100 %, 3.67 at the target.
func TestQueueDrain_BeforeTwoReadingsPricesTheQueueAsDrain(t *testing.T) {
	const p, resident, lambda = 46_556.0, 130_318.0, 14.18
	rows := []domain.ReplicaMetrics{drainRow("a", 67, 0.9, 16, 2.594), drainRow("b", 63, 0.9, 16, 2.572)}
	mu0 := (16/2.594 + 16/2.572) / 2
	want := (lambda + 130.0/60) / (FirstReadHaircut * mu0) * p
	for _, on := range []bool{true, false} {
		clock := propStart
		a := drainAnalyzer(t, on, &clock)
		got := drainCycle(a, lambda, rows, 0, 0, 0, p, resident)
		t.Logf("switch %v: demand %.0f, %.2f replicas at the target", on, got, got/(0.85*p))
		if on {
			assert.InDelta(t, want, got, 1, "the drain price at the first-read mu, haircut")
			assert.GreaterOrEqual(t, got, resident, "never under the resident KV")
			assert.GreaterOrEqual(t, got/(0.85*p), lambda/(0.85*6.2), "never under the arrivals at the settled mu")
			continue
		}
		assert.InDelta(t, resident+130*2000, got, 1, "off: occupancy's residency charge stands")
	}
}

// Only rows at their stated batch cap give a first read. A row saturated by
// its KV under the cap is still growing its batch and reads high
// (ttft-staircase-b-off, 02:06:23: batches of 126 and 128 under 256 read 31.3
// req/s against 16.7 to 19.3 served), and a queue alone, on decode of a P/D
// model, is requests waiting for their KV from prefill (pd-restart's new
// replicas at 12:28:32 read 3.0 req/s that way against a true 6.2). Either
// row still counts in what the role completes. The control is the row at its
// cap.
func TestQueueDrain_OnlyRowsAtTheBatchCapGiveAFirstRead(t *testing.T) {
	roleOf := map[string]string{"v": domain.RoleDecode}
	read := func(kv float64, running int) drainRead {
		in := makeAnalyzerInput([]domain.ReplicaMetrics{drainRow("a", 20, kv, running, 2.6)}, nil)
		rcs := []capacity.ReplicaCapacity{{VariantName: "v", PodName: "a", QueueLength: 20}}
		vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 1}}
		return NewSaturationAnalyzer(drainStore(propStart)).drainReads(in, rcs, vcs, roleOf)[domain.RoleDecode]
	}
	deferred := read(0.04, 8)
	assert.Zero(t, deferred.mu0, "a deferred queue under the cap is not a saturated row")
	assert.InDelta(t, 8/2.6, deferred.throughput, 1e-9, "it still completes what it runs")
	assert.Zero(t, read(0.95, 8).mu0, "KV full under the cap is no first read either")
	assert.InDelta(t, drainBatchCap/2.6, read(0.95, drainBatchCap).mu0, 1e-9, "control: at the cap it is one")
}

// The growth added to the arrivals is the smaller of the slope over the last
// cycle and over the lookback: a queue that stopped growing adds nothing, and
// one that drains takes its drain off what the replicas complete,
// although backlogGrew lingers for a minute after it. Control: the slope
// over the minute alone still reads growth on that cycle.
func TestQueueDrain_GrowthIsTheSmallerSlope(t *testing.T) {
	clock := propStart
	a := drainAnalyzer(t, true, &clock)
	note := func(at time.Duration, b float64) float64 {
		clock = propStart.Add(at)
		a.noteBacklogGrowth("ns", "m", map[string]float64{"decode": b}, defaultTuning)
		return a.backlogGrowthRate("ns", "m", "decode")
	}
	assert.Zero(t, note(0, 100), "one reading: no slope")
	assert.InDelta(t, 4, note(15*time.Second, 160), 1e-9)
	assert.InDelta(t, 4, note(30*time.Second, 220), 1e-9)
	assert.InDelta(t, -5.0/15, note(45*time.Second, 215), 1e-9, "the queue stopped growing: the last cycle's slope")
	assert.Greater(t, (215.0-100)/45, 2.5, "control: the slope over the lookback still reads growth")
}

// The haircut price holds until every replica ordered under it is Ready. A
// fleet of two replicas (mu 1, KV full, 60 queued each, 6 req/s) prices the
// queue before two mu readings at (6 + 102 / 60) / 0.85 = 9.06 replicas at
// 100 % (each queue capped at what its replica drains in 60 s at 0.85), 10.66
// at the target, and orders 11. When the second reading arrives with nine of
// them still starting, the floor's own swap reads (6 + 120 / 60) / 1 = 8:
// 9.41 at the target, a desired count of 10, one under what was ordered, and
// a scale-down stabilization window shorter than the start would take back a
// replica still starting. The hold keeps the haircut price until none is
// starting; then the swap stands. Off, the second reading's figure is the
// swap's.
func TestQueueDrain_TheHaircutHoldsUntilTheOrderLands(t *testing.T) {
	const p, lambda = 100_000.0, 6.0
	rows := []domain.ReplicaMetrics{drainRow("a", 60, 0.95, 16, 16), drainRow("b", 60, 0.95, 16, 16)}
	resident := 2 * 0.95 * p
	haircut := (lambda + 2*min(60, FirstReadHaircut*60)/60) / FirstReadHaircut * p
	desired := func(d float64) int { return int(math.Ceil(d/(0.85*p) - 1e-9)) }
	ordered := desired(haircut)
	run := func(on bool) []float64 {
		clock := propStart
		a := drainAnalyzer(t, on, &clock)
		out := make([]float64, 0, 6)
		for i, c := range []struct {
			pending, samples int
			mu               float64
		}{
			{0, 0, 0},            // first saturated cycle: no mu
			{ordered - 2, 2, 1},  // the second reading, the order starting
			{ordered - 2, 2, 1},  // still starting
			{0, 2, 1}, {0, 2, 1}, // landed
			{0, 2, 1},
		} {
			clock = propStart.Add(time.Duration(i) * propCycle)
			out = append(out, drainCycle(a, lambda, rows, c.pending, c.mu, c.samples, p, resident))
		}
		return out
	}
	on, off := run(true), run(false)
	t.Logf("ordered %d; on (replicas at 100 %%): %v", ordered, scaled(on, p))
	t.Logf("off (replicas at 100 %%): %v", scaled(off, p))
	require.Equal(t, 11, ordered)
	assert.InDelta(t, haircut, on[0], 1, "the first cycle prices the haircut")
	for i := 1; i <= 2; i++ {
		assert.InDelta(t, haircut, on[i], 1, "cycle %d: the haircut holds while the order starts", i)
		assert.GreaterOrEqual(t, desired(on[i]), ordered, "cycle %d: no replica of the order released", i)
		assert.Less(t, desired(off[i]), ordered, "control, cycle %d: off, the figure would release one", i)
	}
	assert.Less(t, on[5], haircut, "once the order has landed the hold lifts")
	assert.InDelta(t, off[5], on[5], 1, "and the swap stands, as off")
}

// scaled is demands in replicas of p.
func scaled(demands []float64, p float64) []string {
	out := make([]string, len(demands))
	for i, d := range demands {
		out[i] = fmt.Sprintf("%.2f", d/p)
	}
	return out
}

// drainFixedRows are the start probes' #520 ledger rows the drain price
// clears.
var drainFixedRows = []string{
	"closed-loop-load-step-start-15s-jsq",
	"closed-loop-load-step-start-90s-jsq",
	"closed-loop-load-step-start-463s",
	"closed-loop-load-step-start-463s-jsq",
	"closed-loop-restart-before-step-start-15s-jsq",
	"closed-loop-restart-before-step-start-90s-jsq",
}

// violationKinds is the kinds of a start probe's violation, nil when none.
func violationKinds(err error) []string {
	var v *learnertest.Violation
	if !errors.As(err, &v) {
		return nil
	}
	return strings.Split(v.Kind, "+")
}

// Every start probe of the floor, at each start under each router, with the
// switch on and off: the table the PR reports, and three holds. On, no case
// breaks a check of a kind it did not break off, and none sits under need
// past its bound (kindStale). The rows drainFixedRows names pass on and fail
// off, over-priced: the negative control.
func TestQueueDrain_StartProbesOnAndOff(t *testing.T) {
	for _, sp := range startProbes() {
		if sp.learner != "floor" {
			continue
		}
		for _, start := range probeStarts {
			for _, rt := range probeRouters {
				name := fmt.Sprintf("closed-loop-%s-start-%ds%s", sp.name, int(start.Seconds()), rt.suffix)
				run := func(on bool) (probeResult, error) {
					s := sp
					s.pc.switches = map[string]bool{SwitchQueuePricedAsDrain: on}
					r, err := judgeStartProbe(s, start, rt.queueBlind)
					var v *learnertest.Violation
					if err != nil && !errors.As(err, &v) {
						t.Fatalf("%s: %v", name, err)
					}
					return r, err
				}
				on, onErr := run(true)
				off, offErr := run(false)
				t.Logf("%-48s need %2d | on: peak %2d final %2d under %-7s %v | off: peak %2d final %2d under %-7s %v",
					name, on.need, on.peak, on.final, on.underFor, violationKinds(onErr),
					off.peak, off.final, off.underFor, violationKinds(offErr))
				onKinds, offKinds := violationKinds(onErr), violationKinds(offErr)
				assert.NotContains(t, onKinds, kindStale, "%s: under need on", name)
				for _, k := range onKinds {
					assert.Contains(t, offKinds, k, "%s: a kind on (%s) that off does not break", name, k)
				}
				if slices.Contains(drainFixedRows, name) {
					assert.NoError(t, onErr, "%s: on, the row passes", name)
					assert.Equal(t, []string{kindOverPriced}, offKinds, "control, %s: off, it fails over-priced", name)
				}
			}
		}
	}
}

// The first-read mu under AR(1) noise on the service time it is read from,
// at the measured rate noise (sigma 10 % and 15 %, phi 0.3) and at phi 0.9:
// on the 463 s load step and the restart before the step, the cases the
// haircut keeps at need, the fleet never sits under need past its bound
// (kindStale). Off, the same noise is not read: the control is the switch
// off at sigma 15 %, phi 0.9, which orders nothing from the first read.
func drainNoiseCases() []propCase {
	var out []propCase
	for _, noise := range []arNoiseSpec{rateNoise10, rateNoise15, {0.15, 0.9}} {
		for _, sp := range startProbes() {
			if sp.name != "load-step" && sp.name != "restart-before-step" {
				continue
			}
			for _, rt := range probeRouters {
				out = append(out, propCase{class: classLiveness, learner: "floor",
					name: fmt.Sprintf("first-read-noise-%.0f-phi%.0f-%s-start-463s%s", 100*noise.sigma, 10*noise.phi,
						sp.name, rt.suffix),
					family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds,
					check: checkFirstReadNoise(sp, rt.queueBlind, noise)})
			}
		}
	}
	return out
}

// checkFirstReadNoise runs sp at a 463 s start with noise on every replica's
// service time, and fails kindStale when the fleet sits under need past its
// bound or ends more than a replica under need.
func checkFirstReadNoise(sp startProbe, queueBlind bool, noise arNoiseSpec) func(uint64) error {
	return func(seed uint64) error {
		const start = 463 * time.Second
		s := sp
		r := propRand(seed)
		path := map[int][]float64{}
		s.pc.serviceNoise = func(i int, elapsed time.Duration) float64 {
			if _, ok := path[i]; !ok {
				path[i] = arNoise(r, int(time.Hour/propCycle), noise.sigma, noise.phi)
			}
			return path[i][min(int(elapsed/propCycle), len(path[i])-1)]
		}
		res, err := judgeStartProbe(s, start, queueBlind)
		var v *learnertest.Violation
		if err != nil && !errors.As(err, &v) {
			return err
		}
		if slices.Contains(violationKinds(err), kindStale) {
			return violation(kindStale, "%s at %s: under need for %s (bound %s), final %d for need %d",
				noise, start, res.underFor, underBound(start), res.final, res.need)
		}
		return nil
	}
}

// What the haircut buys, and the noise cases' control: the 463 s load step
// and restart before the step, each router, with the haircut and with it
// taken out (drainHaircut 1), noise-free and at sigma 15 %, phi 0.9 on the
// service time the first read divides by (eight seeds). With it the fleet is
// never longer under need than without it, and inside the bound.
//
// Measured: 30 s and 15 s under need noise-free with the haircut, 1m30s
// without; 2m30s and 45 s at the noise, 3m15s and 2m15s without. The bound
// is 11m13s, so taking the haircut out is not caught by the bound on these
// cases: what it buys is margin, not a pass.
func TestQueueDrain_TheHaircutKeepsTheSlowStartsAtNeed(t *testing.T) {
	type key struct {
		name  string
		h     float64
		noisy bool
	}
	worstOf := map[key]time.Duration{}
	for _, sp := range startProbes() {
		if sp.name != "load-step" && sp.name != "restart-before-step" {
			continue
		}
		for _, rt := range probeRouters {
			for _, h := range []float64{FirstReadHaircut, 1} {
				for _, noisy := range []bool{false, true} {
					s := sp
					s.pc.drainHaircut = h
					worst := time.Duration(0)
					seeds := []uint64{1}
					if noisy {
						seeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8}
					}
					for _, seed := range seeds {
						if noisy {
							r := propRand(seed)
							path := map[int][]float64{}
							s.pc.serviceNoise = func(i int, elapsed time.Duration) float64 {
								if _, ok := path[i]; !ok {
									path[i] = arNoise(r, int(time.Hour/propCycle), 0.15, 0.9)
								}
								return path[i][min(int(elapsed/propCycle), len(path[i])-1)]
							}
						}
						res, _ := judgeStartProbe(s, 463*time.Second, rt.queueBlind)
						worst = max(worst, res.underFor)
					}
					t.Logf("%s%s haircut %.2f noisy %v: worst under need %s (bound %s)", sp.name, rt.suffix, h, noisy,
						worst, underBound(463*time.Second))
					worstOf[key{sp.name + rt.suffix, h, noisy}] = worst
				}
			}
		}
	}
	for k, worst := range worstOf {
		if k.h != FirstReadHaircut {
			continue
		}
		assert.LessOrEqual(t, worst, underBound(463*time.Second), "%v", k)
		assert.LessOrEqual(t, worst, worstOf[key{k.name, 1, k.noisy}], "%v: longer under need with the haircut", k)
	}
}
