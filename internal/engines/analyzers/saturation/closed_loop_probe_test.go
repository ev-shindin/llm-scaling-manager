package saturation

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learn"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
)

// The closed-loop probe (analyzer-learner.md, 2.5): the saturation analyzer
// driven by a fleet simulator that turns its order into replicas
// (learnertest.Fleet, which the throughput analyzer's probe runs on too).
// Holds, latches, the sibling yield and a self-sealing low mu are closed-loop
// behaviours -- the order changes the load each replica sees, and so what the
// analyzer is next shown -- and only a loop sees them.
//
// What the saturation analyzer is shown, one 15 s cycle at a time:
//
//   - A replica serving under its mu holds KV in proportion (0.95 at its
//     limit); one at its limit, or queued, holds 0.95.
//   - The analyzer prices the Ready replicas' rows; its order is the demand
//     over the per-replica capacity at the scale-up target, rounded up.
//   - A replica just Ready is first seen by the analyzer then, and waits its
//     settle as any joined replica does.
//
// The verdict is against need: the fleet a truthful analyzer orders at the
// arrival rate once every replica serves the fleet's mean mu. At that point
// no replica queues, and the order is occupancy's: each replica holds 0.95
// lambda / (n mu) of its KV, priced against k1 (0.8 of it) at the target
// (0.85), so need is ceil(0.95 lambda / (0.68 mu)) whatever n is. The two
// verdicts, over- and under-ordered, are learnertest.Result.Verdict's.

// probeStart is S, from an order to the replica Ready.
const probeStart = learnertest.DefaultStartTime

// probeCase is one closed-loop scenario.
type probeCase struct {
	name string
	// pods is the fleet's true mu per replica at the start; added replicas
	// serve addedMu.
	pods    []float64
	addedMu float64
	// mu, when set, is a replica's true mu at an elapsed time, overriding
	// pods and addedMu (a slow replica that recovers).
	mu func(i int, elapsed time.Duration) float64
	// lambda is the arrival rate at an elapsed time, split equally over the
	// Ready replicas, or by weight when set (learnertest.Fleet.Weight).
	lambda  func(elapsed time.Duration) float64
	weight  func(i int, elapsed time.Duration) float64
	horizon time.Duration
	// meanMu is the fleet's mean true mu at the horizon, which need is
	// priced at.
	meanMu float64
	// decode runs the fleet as a P/D decode role whose rows carry an ITL
	// on probeITL, so the floor can price a derived mu, and the analyzer
	// reads a settle tracker observed before each cycle, as the engine runs
	// it.
	decode bool
	// restartAt, when set, restarts the controller then: a new analyzer and
	// a new settle tracker. With snapshot, the learned state the old one
	// exported is restored into the new one; without it nothing is.
	restartAt time.Duration
	snapshot  bool
	// sightings, with snapshot, also restores the old tracker's sightings
	// (settle.Tracker.Restore), as the engine's restore does; without it the
	// new tracker sees every replica first, as with learner PR 5d's switch
	// off.
	sightings bool
	// stabilization replaces the default scale-down stabilization window
	// (learnertest.DefaultStabilization, 5 min) when set.
	stabilization time.Duration
	// switches are set on every process (SetSwitchesForReplay).
	switches map[string]bool
}

// probeITL is the decode replicas' ITL at fill k: a line on which the mu
// derived at the pricing fill (0.68) is the true mu of 1, at the probe's
// shape (1000 in, 1000 out) and KV (imbKv).
func probeITL(k float64) float64 { return 0.119 + 0.6*k }

// probeProcess is one controller process of a decode probe: its analyzer
// over a store holding the variant's engine parameters, and its settle
// tracker, with switches set.
func probeProcess(t *testing.T, rig *propRig, switches map[string]bool) *settle.Tracker {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "v", capacity.Record{EngineParams: mtParams(1024, "vllm:v1")}, rig.clock)
	st := settle.NewTracker()
	st.SetInterval(propCycle)
	rig.a = NewSaturationAnalyzer(store).WithSettleView(st)
	rig.a.now = func() time.Time { return rig.clock }
	require.NoError(t, rig.a.SetSwitchesForReplay(switches))
	return st
}

// probeResult is what the loop did.
type probeResult struct {
	need, final, peak int
	// overSince is how long before the horizon the fleet has been over
	// need's tolerance without a break; zero when it ends inside it.
	overSince time.Duration
	// underFor is the longest run the fleet sat below need at the arrival
	// rate of the moment.
	underFor time.Duration
	// muCounts sums mu's report counts over the run, and muSource is the
	// source mu priced the variant from on the last cycle that priced it.
	muCounts map[learn.Reason]int64
	muSource learn.Source
	trace    []int
	// fleet is the simulator's result, whose verdict this is.
	fleet learnertest.Result
	// sightings is how many restored sightings the last process installed.
	sightings int64
}

// runProbe drives the case to its horizon and returns what the fleet did.
func runProbe(t *testing.T, pc probeCase) probeResult {
	t.Helper()
	rig := newPropRig()
	res := probeResult{muCounts: map[learn.Reason]int64{}}
	var st *settle.Tracker
	if pc.decode {
		st = probeProcess(t, rig, pc.switches)
	} else {
		require.NoError(t, rig.a.SetSwitchesForReplay(pc.switches))
	}
	restarted := false
	fleet := learnertest.Fleet{
		Pods: pc.pods, AddedMu: pc.addedMu, Mu: pc.mu, Lambda: pc.lambda, Weight: pc.weight, Horizon: pc.horizon,
		Need:  func(lambda float64) int { return int(math.Ceil(0.95*lambda/(0.68*pc.meanMu) - 1e-9)) },
		Start: rig.clock, StartTime: probeStart, Stabilization: pc.stabilization,
	}
	out := fleet.Run(func(now time.Time, elapsed time.Duration, lambda float64, ready []learnertest.Replica) int {
		require.Equal(t, now, rig.clock, "the rig's clock and the fleet's")
		if pc.restartAt > 0 && !restarted && elapsed >= pc.restartAt {
			restarted = true
			doc := rig.a.ExportLearnedState()
			sightings := st.Sightings()
			st = probeProcess(t, rig, pc.switches)
			if pc.sightings {
				st.Restore(sightings, rig.clock)
			}
			if pc.snapshot {
				raw, err := json.Marshal(doc)
				require.NoError(t, err)
				var back LearnedState
				require.NoError(t, json.Unmarshal(raw, &back))
				rig.a.RestoreLearnedState(back)
			}
		}
		rows := make([]domain.ReplicaMetrics, 0, len(ready))
		for _, r := range ready {
			kv := 0.95 * r.Offered / r.Mu
			if r.Saturated {
				kv = 0.95
			}
			rm := imbRow(r.Name, "v", kv, int(math.Round(r.Backlog)), 1000, 1000, r.Served)
			if pc.decode {
				rm.KvCacheUsageKnown, rm.GenerationTokenRateKnown, rm.RequestRateKnown = true, true, true
				rm.AvgITL, rm.AvgITLKnown = probeITL(kv), true
				rm.ReadySince = r.ReadyAt
			}
			rows = append(rows, rm)
		}
		states := aggStates(len(rows))
		if pc.decode {
			states = decodeStates(len(rows))
			st.Observe(settleModel("test-ns", "test-model"), settlerow.Of(rows, map[string]string{"v": domain.RoleDecode}), rig.clock)
		}
		out, err := rig.analyze(rows, states, lambda, propCycle)
		require.NoError(t, err)
		for _, r := range rig.a.LearnerReports()["test-ns|test-model"] {
			if r.Learner != learn.Mu {
				continue
			}
			for reason, n := range r.Counts {
				res.muCounts[reason] += n
			}
			if r.Source != "" {
				res.muSource = r.Source
			}
		}
		return learnertest.Ceil(impliedReplicas(out))
	})
	res.fleet = out
	if st != nil {
		res.sightings = st.RestoredCount()
	}
	res.need, res.final, res.peak = out.Need, out.Final, out.Peak
	res.overSince, res.underFor, res.trace = out.OverSince, out.UnderFor, out.Trace
	return res
}

// verdict is "" when the fleet ends within tolerance of need, else why not
// (learnertest.Result.Verdict).
func (r probeResult) verdict() string { return r.fleet.Verdict() }

// stepTo is an arrival rate of from until at, then to.
func stepTo(from, to float64, at time.Duration) func(time.Duration) float64 {
	return func(e time.Duration) float64 {
		if e < at {
			return from
		}
		return to
	}
}

// A fleet of equal replicas hit by a load step: they saturate, mu is
// measured at the truth, the floor orders on it, and the fleet settles
// within tolerance of need once the step is served.
func TestClosedLoopProbe_ALoadStepOnEqualReplicasSettlesAtNeed(t *testing.T) {
	r := runProbe(t, probeCase{
		name: "step", pods: []float64{1, 1}, addedMu: 1, meanMu: 1,
		lambda: stepTo(1.2, 6, 2*time.Minute), horizon: 40 * time.Minute,
	})
	t.Logf("need %d, final %d, peak %d, longest under need %s; fleet by cycle %v", r.need, r.final, r.peak, r.underFor, r.trace)
	require.Empty(t, r.verdict())
	require.Positive(t, r.muCounts[muReasonSampled], "premise: the step saturated the replicas and mu was measured")
	require.Equal(t, learn.Source(domain.ServiceRateSourceMeasured), r.muSource, "premise: the floor priced the measured mu")
}

// #282 review round 3 (H1): one replica degraded to 0.3 of its siblings'
// rate, queued, while they serve unqueued; then it recovers. Its low rate
// counts by its weight, with the unread siblings at what they serve over the
// target (mu_slow_pod.go), so the fleet grows while it is slow, to 24 at its
// peak, and settles at 20, inside the bound. 20 is a fixed point: each of
// the twenty serves 0.64, which over the target is the figure on record, so
// the figure sits at the fleet's served bound and the minority rule keeps it
// (minorityFigureExpired releases only a figure under that bound).
//
// Measured (60 min): need 18 (bound 21.7), peak 24, at 20 from 21.5 min to
// the end. The weighing's hold is what keeps the peak at 24: read fresh
// (h := mean in weighUnreadReplicas) it peaked at 28. Before #495, when the
// served bound refused the reading, peak 19 and 18 from 27 min.
func TestClosedLoopProbe_OneSlowReplicaUnderHalfItsSiblingsDoesNotSealTheFleet(t *testing.T) {
	r := runProbe(t, slowReplicaCase(0.3))
	t.Logf("need %d, final %d, peak %d, longest under need %s; fleet by cycle %v", r.need, r.final, r.peak, r.underFor, r.trace)
	require.Empty(t, r.verdict())
	require.LessOrEqual(t, r.peak, 24, "the weighing's hold keeps the fleet from chasing its own falling served rate")
	require.LessOrEqual(t, r.final, 20)
	require.Positive(t, r.muCounts[muReasonWeighedFleet], "premise: the slow replica's reading counted the fleet it did not read")
}

// The self-sealing low mu of analyzer-learner.md 3.4 (cost side), fixed on
// #495. The same slow replica at 0.55 of its siblings' rate, with no line:
// before, its rate alone became mu (12.8 / 0.55 / 0.85 = 27.4), the fleet
// went to 29 and 28, nothing queued at 28, and nothing rewrote the figure --
// over-ordered for good. Now the sample counts the fifteen unread siblings
// at what they serve over the target, which keeps the fleet under the bound
// throughout. It settles at 20, the fixed point where the figure equals what
// the fleet serves over the target, so the minority rule, which releases
// only a figure under the fleet's served bound, does not act here.
//
// Measured (60 min): need 18 (bound 21.7), 21 from 1.75 min, 20 from 16.25
// min to the end; peak 21. The margin to the bound is zero replicas: 22
// would be over it. Negative control: with SwitchMuUnreadAtTarget off the
// peak is 29 and the fleet stays at 28 for good, as before #495.
func TestClosedLoopProbe_ASlowReplicaInsideTheServedBoundStaysUnderTheBound(t *testing.T) {
	r := runProbe(t, slowReplicaCase(0.55))
	t.Logf("need %d, final %d, peak %d; fleet by cycle %v", r.need, r.final, r.peak, r.trace)
	require.Empty(t, r.verdict())
	bound := float64(r.need)*(1+learnertest.OverTolerance) + 1
	require.LessOrEqual(t, float64(r.peak), bound, "over the bound while the replica was slow: the unread siblings were counted at a limit they never showed")
	require.LessOrEqual(t, r.final, 20)
	require.Positive(t, r.muCounts[muReasonWeighedFleet], "premise: the slow replica's reading counted the fleet it did not read")

	noCredit := slowReplicaCase(0.55)
	noCredit.switches = map[string]bool{SwitchMuUnreadAtTarget: false}
	c := runProbe(t, noCredit)
	t.Logf("unread replicas at what they serve: final %d, peak %d; %s", c.final, c.peak, c.verdict())
	require.Greater(t, float64(c.peak), bound, "the credit is what keeps the slow period under the bound")
	require.Contains(t, c.verdict(), "over-ordered", "without the credit the low figure seals the fleet")
}

// A router that steers, and a minority figure that is right. Eighteen
// replicas at mu 1 under 12 req/s, the router sending twice the share to
// four of them: those four queue and are read at 1, their limit, while the
// fourteen others serve unqueued, so a minority of the fleet wrote the figure.
// Then a lull at 2 req/s from 10 to 45 min, which queues nothing for longer
// than lowHoldFor, and the load returns at 12. The figure is not under what
// the fleet serves in the lull (a fleet at a fraction of its limit serves a
// fraction of it), so the minority rule keeps it, and the fleet meets the
// return at need.
//
// Measured (60 min): need 17; 17, down to 3 in the lull, back to 17 at 45
// min, peak 33 at the return, 19 at the end; never under need past the
// start. Negative control, F as first built (released to no mu, no bound
// gate), patched in by hand since it has no switch: the figure was released
// on 45 cycles and the fleet moved the same way, because the first queued
// reading at the return writes it back within a cycle. The simulator cannot
// show the cost of that release: its floor never binds above occupancy
// without a queue.
func TestClosedLoopProbe_ASteeringRouterKeepsARightMinorityFigure(t *testing.T) {
	pods := make([]float64, 18)
	for i := range pods {
		pods[i] = 1
	}
	pc := probeCase{
		name: "steered", pods: pods, addedMu: 1, meanMu: 1,
		lambda: func(e time.Duration) float64 {
			if e >= 10*time.Minute && e < 45*time.Minute {
				return 2
			}
			return 12
		},
		weight: func(i int, _ time.Duration) float64 {
			if i < 4 {
				return 2
			}
			return 1
		},
		horizon: 60 * time.Minute,
	}
	r := runProbe(t, pc)
	t.Logf("need %d, final %d, peak %d, longest under need %s; fleet by cycle %v", r.need, r.final, r.peak, r.underFor, r.trace)
	require.Empty(t, r.verdict())
	require.Zero(t, r.underFor, "the fleet never sat under need")
	require.Zero(t, r.muCounts[muReasonMinorityExpired], "a right minority figure, not under what the fleet serves, is kept")
	require.Positive(t, r.muCounts[muReasonSampled], "premise: the steered replicas were read")
}

// slowReplicaCase is sixteen replicas at mu 1, one of them at slow for its
// first 10 min and at 1 after, under 12.8 req/s split equally: 0.8 each, so
// the slow one queues and the rest serve unqueued at KV 0.76, under the 0.8
// line. The horizon is 60 min, past lowHoldFor (30 min) after the slow
// replica's last saturated reading and the 5 min scale-down stabilization,
// so the run covers the minority rule's decision.
func slowReplicaCase(slow float64) probeCase {
	pods := make([]float64, 16)
	for i := range pods {
		pods[i] = 1
	}
	pods[0] = slow
	return probeCase{
		name: fmt.Sprintf("slow-%.2f", slow), pods: pods, addedMu: 1, meanMu: 1,
		mu: func(i int, e time.Duration) float64 {
			if i == 0 && e < 10*time.Minute {
				return slow
			}
			return 1
		},
		lambda:  func(time.Duration) float64 { return 12.8 },
		horizon: 60 * time.Minute,
	}
}

// The probe through a controller restart (analyzer-learner.md 4.4, learner
// PR 4a). A decode fleet of four at 2.5 requests/s learns its ITL line;
// the controller restarts at 15 min, and 15 s later the load steps to 12
// (need 17). After the restart every replica waits its settle again: k2
// and the mu written beside it a minute, the ITL line steadyFor (2 min),
// so the floor is silent until the new process measures or derives a mu.
func restartStepCase(restart time.Duration, snapshot bool, stabilization time.Duration) probeCase {
	return probeCase{
		name: "restart-step", pods: []float64{1, 1, 1, 1}, addedMu: 1, meanMu: 1, decode: true,
		lambda: stepTo(2.5, 12, 15*time.Minute+15*time.Second), horizon: 40 * time.Minute,
		restartAt: restart, snapshot: snapshot, stabilization: stabilization,
	}
}

// With the snapshot off, the restart costs the step its floor: the fleet
// grows on occupancy alone until the new process has a mu, and sits under
// need for longer than without the restart -- but inside the bound a
// start and a settle take, and it settles at need. The SLO side, as 4.4
// says; measured here, not asserted.
func TestClosedLoopProbe_AControllerRestartBeforeALoadStepSettlesAtNeed(t *testing.T) {
	control := runProbe(t, restartStepCase(0, false, 0))
	require.Empty(t, control.verdict(), "premise: without a restart the step settles at need")
	r := runProbe(t, restartStepCase(15*time.Minute, false, 0))
	t.Logf("need %d, final %d, peak %d; under need for %s, against %s without the restart", r.need, r.final, r.peak,
		r.underFor, control.underFor)
	require.Empty(t, r.verdict())
	require.Greater(t, r.underFor, control.underFor, "the restart's settle costs the step its floor for a while")

	// The ITL line's share of it: the same restart with the line taking the
	// first cycle's replicas as settled, as before learner PR 4a.
	before := restartStepCase(15*time.Minute, false, 0)
	before.switches = map[string]bool{SwitchITLWaitsAfterRestart: false}
	b := runProbe(t, before)
	t.Logf("with the ITL line not waiting after the restart: under need for %s", b.underFor)
	require.Empty(t, b.verdict())
	require.GreaterOrEqual(t, r.underFor, b.underFor, "the line's wait adds to the restart's cost, never takes from it")
}

// With the snapshot on, the restored k2, mu and ITL slope price the step as
// the process that exported them would have: no longer under need than
// without the restart.
func TestClosedLoopProbe_ARestoredSnapshotCarriesTheFleetThroughARestart(t *testing.T) {
	control := runProbe(t, restartStepCase(0, false, 0))
	r := runProbe(t, restartStepCase(15*time.Minute, true, 0))
	t.Logf("need %d, final %d, peak %d; under need for %s, against %s without the restart", r.need, r.final, r.peak,
		r.underFor, control.underFor)
	require.Empty(t, r.verdict())
	require.Equal(t, control.underFor, r.underFor, "the snapshot restores what the floor prices from")
}

// Learner PR 5d's change E through the same restart and step: the first
// sights come back with the snapshot. The fleet carries the step as the
// snapshot alone does, no longer under need, and settles at need. Measured,
// E moves no cycle of this fleet: the restored windows and held line already
// price it. What E moves is a price the probe's fleet does not depend on
// (first_sight_price_test.go has the two signs).
func TestClosedLoopProbe_ARestoredFirstSightCarriesTheStep(t *testing.T) {
	control := restartStepCase(15*time.Minute, true, 0)
	base := runProbe(t, control)
	withSight := control
	withSight.sightings = true
	r := runProbe(t, withSight)
	cycles, lower, higher := traceDelta(r.trace, base.trace)
	t.Logf("need %d, final %d, peak %d; under need for %s, against %s with the snapshot alone; fleet differs on %d cycles, lower by %d at most, higher by %d",
		r.need, r.final, r.peak, r.underFor, base.underFor, cycles, lower, higher)
	require.EqualValues(t, 4, r.sightings, "premise: every replica's first sight came back")
	require.Empty(t, r.verdict())
	require.LessOrEqual(t, r.underFor, base.underFor, "skipping the wait never leaves the fleet under need for longer")
}

// traceDelta compares two fleets cycle by cycle: on how many cycles they
// differ, and the most a is below and above b.
func traceDelta(a, b []int) (cycles, lower, higher int) {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			cycles++
		}
		lower = max(lower, b[i]-a[i])
		higher = max(higher, a[i]-b[i])
	}
	return cycles, lower, higher
}

// A settled, saturated fleet through a restart: load holds at 12 requests/s
// (need 17), and the controller restarts. The lowest order in the 10 min
// after, with first sight restored and with the wait. Measured, the two are
// the same fleet on every cycle, at need, so this holds the outcome and does
// not by itself show the bound: first_sight_price_test.go measures the case
// that sizes lower.
func TestClosedLoopProbe_ARestoredFirstSightDoesNotTakeAFleetBelowNeed(t *testing.T) {
	steady := func(sightings bool) probeResult {
		return runProbe(t, probeCase{
			name: "restart-steady", pods: []float64{1, 1, 1, 1}, addedMu: 1, meanMu: 1, decode: true,
			lambda: func(time.Duration) float64 { return 12 }, horizon: 40 * time.Minute,
			restartAt: 25 * time.Minute, snapshot: true, sightings: sightings,
		})
	}
	lowest := func(r probeResult) int {
		from := int(25 * time.Minute / propCycle)
		to := min(from+int(10*time.Minute/propCycle), len(r.trace))
		low := r.trace[from]
		for _, n := range r.trace[from:to] {
			low = min(low, n)
		}
		return low
	}
	waits, restored := steady(false), steady(true)
	cycles, lower, higher := traceDelta(restored.trace, waits.trace)
	t.Logf("need %d: lowest fleet in the 10 min after the restart %d restored, %d with the wait; final %d, %d; longest under need %s, %s; fleet differs on %d cycles, lower by %d at most, higher by %d",
		restored.need, lowest(restored), lowest(waits), restored.final, waits.final, restored.underFor, waits.underFor,
		cycles, lower, higher)
	require.Positive(t, restored.sightings, "premise: the first sights came back")
	require.Empty(t, restored.verdict())
	require.GreaterOrEqual(t, lowest(restored), restored.need, "the fleet stays at need through the restart")
	require.LessOrEqual(t, restored.underFor, waits.underFor)
}

// A scale-down stabilization of 120 s, under the 2.75 min the floor can be
// silent after a restart, with the load stepping in the gap. 4.4 names
// this as the case the default 300 s no longer masks. In the probe the
// gap shows on the way up, where stabilization does not enter: the fleet
// is under need exactly as long as with 300 s, inside the bound, and it
// settles at need. The probe's fleet orders at need on occupancy alone
// once no replica queues, so nothing it does after the restart asks for a
// scale-down for the stabilization to hold.
func TestClosedLoopProbe_AShortStabilizationThroughARestartSettlesAtNeed(t *testing.T) {
	long := runProbe(t, restartStepCase(15*time.Minute, false, 0))
	short := runProbe(t, restartStepCase(15*time.Minute, false, 2*time.Minute))
	t.Logf("need %d, final %d, peak %d; under need for %s at 120 s, %s at 300 s", short.need, short.final, short.peak,
		short.underFor, long.underFor)
	require.Empty(t, short.verdict())
	require.Equal(t, long.underFor, short.underFor)
}
