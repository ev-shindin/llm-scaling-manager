package saturation

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/internal/learnertest"
)

// The closed-loop probe at the measured range of a replica's start
// (analyzer-learner.md, PR 5 design: "the closed-loop probe runs C for true
// starts of 15 s and 463 s"), as liveness property cases: the load step, a
// decode step with no restart, the controller restart before that step, the
// restored snapshot through it, and the two slow-pod cases, each at 15 s (a
// snapshot restore), 90 s (the simulator's default) and 463 s (a cold JIT
// cache). Every row reports the true start as its measured one.
//
// Each case runs under both bounds on the real router (probeRouters): the
// even split, as the probe always ran, and the shortest-queue limit, whose
// case names end in -jsq. The EPP lies between the two, so a row on one is
// not a measurement of the observed router.
//
// Each case must:
//
//   - publish the true start: the start learner's figure
//     (AnalyzerResult.RoleStartSeconds) within a second of S on the last
//     cycle, else kindBiased. This is what makes the case about its start:
//     without it a run that ignored the start could fail the same way;
//   - never order more than peakCeiling, and end within tolerance of need by
//     its horizon, or by backlogClears when that is later, else
//     kindOverPriced;
//   - end no more than a replica under need, and sit below need no longer
//     than underBound, else kindStale;
//   - for the restored snapshot, sit below need exactly as long as the same
//     step with no restart, else kindMoved.
//
// Every check is evaluated. A case that breaks checks of two kinds fails
// with both, joined by "+" (startViolation), which no single-kind ledger row
// matches: a row ledgered for one kind still fails when the case also breaks
// another.
//
// A case that fails is a ledger row (testdata/properties/known-failures.yaml)
// with the kind it fails with, not a wider bound. The probe draws nothing from
// its seed, so each case runs on one.

// probeStarts is the measured range of a replica's start (docs/reference/
// start-time.md): 15 s a snapshot restore, 90 s the simulator's default,
// 463 s a cold JIT cache.
var probeStarts = []time.Duration{15 * time.Second, probeStart, 463 * time.Second}

// probeRouter is one of the two bounds on the real router a start probe runs
// under, and the suffix its case names carry.
type probeRouter struct {
	suffix     string
	queueBlind bool
}

// probeRouters are the even split (queue-blind, the probe's router before
// #520) and the shortest-queue limit (learnertest's route).
var probeRouters = []probeRouter{{suffix: "", queueBlind: true}, {suffix: "-jsq", queueBlind: false}}

// underBound is how long a fleet whose replicas take start to become Ready
// may sit below need. learnertest.UnderFor is the default start, the joined
// replica's settle, and a cycle or two of ordering either side, so only its
// start moves with the case's: 3m45s at 15 s, 5 min at 90 s, 11m13s at 463 s.
func underBound(start time.Duration) time.Duration {
	return learnertest.UnderFor - probeStart + start
}

// peakCeilingFactor bounds the fleet at its largest: half again over need,
// plus a replica of rounding. It is a declared bound, not derived from the
// floor's arithmetic. When it was set, on the even-split router, the 15 s
// cases reached at most 22 for a need of 17 (1.29x). A step may order over
// need while it drains what queued during the start; the bound says how far.
const peakCeilingFactor = 1.5

// peakCeiling is the most replicas a case may order for need.
func peakCeiling(need int) float64 { return peakCeilingFactor*float64(need) + 1 }

// overBound is learnertest.Result.Verdict's: the most a fleet may end at.
func overBound(need int) float64 { return float64(need)*(1+learnertest.OverTolerance) + 1 }

// backlogClears is when the verdict on a fleet hit by a load step at step,
// whose replicas take start to become Ready, begins: the allowance it makes
// for the backlog a slow start leaves (#520).
//
// Until the first order lands, start plus the cycle that orders it, the
// fleet's first replicas take the new rate lambda1 over their capacity C (the
// sum of their mu), and each queues lambda1/C - 1 of its mu per second. A
// queued request stays on its replica, which serves it at its mu at best: at
// the shortest-queue limit, which sends that replica nothing while its queue
// is the longest, it drains in as long again times lambda1/C - 1; under an
// even split it takes longer. On top of the drain the verdict allows one
// scale-down stabilization window. A fleet that ordered need on the step's
// own cycle would need none of it; the window is an allowance for a fleet
// that ordered over need while the queue drained to come back down:
//
//	backlogClears = step + (start + cycle) * max(lambda1/C, 1) + stabilization
//
// For the decode step (4 replicas of mu 1, 2.5 -> 12 req/s at 15m15s) at a
// 463 s start: 915 + 478 x 3 + 300 s = 44m9s, past the 40 min horizon; at
// 90 s, 25m30s. For the load step (2 of mu 1, 1.2 -> 6 at 2 min) at 463 s:
// 30m54s.
func backlogClears(pc probeCase, step, start time.Duration) time.Duration {
	capacity := 0.0
	for _, mu := range pc.pods {
		capacity += mu
	}
	stabilization := learnertest.DefaultStabilization
	if pc.stabilization > 0 {
		stabilization = pc.stabilization
	}
	ratio := 1.0
	if capacity > 0 {
		ratio = max(pc.lambda(pc.horizon)/capacity, 1)
	}
	return step + time.Duration(ratio*float64(start+propCycle)) + stabilization
}

// verdictHorizon is the case's horizon, or backlogClears rounded up to a
// cycle when that is later. A case with no step (step zero) keeps its
// horizon.
func verdictHorizon(pc probeCase, step, start time.Duration) time.Duration {
	if step <= 0 {
		return pc.horizon
	}
	clears := backlogClears(pc, step, start)
	if rest := clears % propCycle; rest > 0 {
		clears += propCycle - rest
	}
	return max(pc.horizon, clears)
}

// traceSummary is a fleet trace as runs: "2 from 0s, 3 from 2m0s, ...".
func traceSummary(trace []int) string {
	var out []byte
	for i, n := range trace {
		if i > 0 && trace[i-1] == n {
			continue
		}
		if len(out) > 0 {
			out = append(out, ", "...)
		}
		out = fmt.Appendf(out, "%d from %s", n, time.Duration(i)*propCycle)
	}
	return string(out)
}

// startProbe is one probe scenario run at each of probeStarts.
type startProbe struct {
	name, learner string
	pc            probeCase
	// step is when the case's load steps, zero for none (verdictHorizon).
	step time.Duration
	// sameAsUnrestarted holds the run's time under need to the same case's
	// with no restart (the restored snapshot).
	sameAsUnrestarted bool
}

func startProbes() []startProbe {
	return []startProbe{
		{name: "load-step", learner: "floor", step: 2 * time.Minute, pc: probeCase{
			name: "step", pods: []float64{1, 1}, addedMu: 1, meanMu: 1,
			lambda: stepTo(1.2, 6, 2*time.Minute), horizon: 40 * time.Minute,
		}},
		{name: "decode-step", learner: "floor", step: restartStepAt, pc: restartStepCase(0, false, 0)},
		{name: "restart-before-step", learner: "floor", step: restartStepAt, pc: restartStepCase(15*time.Minute, false, 0)},
		{name: "restored-snapshot", learner: "floor", step: restartStepAt, pc: restartStepCase(15*time.Minute, true, 0), sameAsUnrestarted: true},
		{name: "slow-pod-030", learner: "mu", pc: slowReplicaCase(0.3)},
		{name: "slow-pod-055", learner: "mu", pc: slowReplicaCase(0.55)},
	}
}

// startProbeCases is each startProbe at each start under each router, as a
// property case.
func startProbeCases() []propCase {
	probes := startProbes()
	out := make([]propCase, 0, len(probes)*len(probeStarts)*len(probeRouters))
	for _, sp := range probes {
		for _, s := range probeStarts {
			for _, rt := range probeRouters {
				out = append(out, propCase{class: classLiveness, learner: sp.learner,
					name:   fmt.Sprintf("closed-loop-%s-start-%ds%s", sp.name, int(s.Seconds()), rt.suffix),
					family: familyDirect, tuning: tunedDefault, seeds: 1, check: checkStartProbe(sp, s, rt.queueBlind)})
			}
		}
	}
	return out
}

// startViolation is every broken check of a start probe as one violation:
// of the one kind they share, or of their kinds joined by "+" in the order
// they were found.
func startViolation(kinds, msgs []string, summary string) error {
	if len(kinds) == 0 {
		return nil
	}
	var distinct []string
	for _, k := range kinds {
		if !slices.Contains(distinct, k) {
			distinct = append(distinct, k)
		}
	}
	return violation(strings.Join(distinct, "+"), "%s; %s", strings.Join(msgs, "; "), summary)
}

// checkStartProbe runs sp with its replicas starting in start, under the
// even split (queueBlind) or the shortest-queue limit.
func checkStartProbe(sp startProbe, start time.Duration, queueBlind bool) func(uint64) error {
	return func(uint64) error {
		pc := sp.pc
		pc.start, pc.queueBlind = start, queueBlind
		pc.horizon = verdictHorizon(pc, sp.step, start)
		r, err := tryProbe(pc)
		if err != nil {
			return err
		}
		summary := fmt.Sprintf("start %s (published %.1f s): need %d, final %d at %s (bound %.1f), peak %d (ceiling %.1f), under need for %s (bound %s); fleet %s",
			start, r.startSeconds, r.need, r.final, pc.horizon, overBound(r.need), r.peak, peakCeiling(r.need), r.underFor,
			underBound(start), traceSummary(r.trace))
		var kinds, msgs []string
		broke := func(kind, format string, args ...any) {
			kinds, msgs = append(kinds, kind), append(msgs, fmt.Sprintf(format, args...))
		}
		if math.Abs(r.startSeconds-start.Seconds()) > 1 {
			broke(kindBiased, "the start learner publishes %.1f s against a true start of %s", r.startSeconds, start)
		}
		if float64(r.peak) > peakCeiling(r.need) {
			broke(kindOverPriced, "peak %d over the ceiling %.1f", r.peak, peakCeiling(r.need))
		}
		if float64(r.final) > overBound(r.need) {
			broke(kindOverPriced, "over-ordered at the horizon: %d against %.1f", r.final, overBound(r.need))
		}
		if r.final < r.need-1 || r.underFor > underBound(start) {
			broke(kindStale, "under-ordered: final %d, under need for %s", r.final, r.underFor)
		}
		if sp.sameAsUnrestarted {
			control := pc
			control.restartAt, control.snapshot = 0, false
			c, err := tryProbe(control)
			if err != nil {
				return err
			}
			if c.underFor != r.underFor {
				broke(kindMoved, "the restored snapshot under need for %s, %s without the restart", r.underFor, c.underFor)
			}
		}
		return startViolation(kinds, msgs, summary)
	}
}

// tryProbe is runProbe outside a test: a require failure inside the probe
// (the rig's clock and the fleet's disagree, an analyze error) comes back as
// an error.
func tryProbe(pc probeCase) (r probeResult, err error) {
	ft := &probeT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r = runProbe(ft, pc)
	}()
	<-done
	if ft.failed {
		return r, fmt.Errorf("probe %s: %s", pc.name, ft.msg)
	}
	return r, nil
}

// probeT is the probeTB a property case runs the probe with: it records a
// failure instead of failing a test, and FailNow ends the probe's goroutine.
type probeT struct {
	failed bool
	msg    string
}

func (p *probeT) Helper() {}

func (p *probeT) Errorf(format string, args ...any) {
	p.failed, p.msg = true, fmt.Sprintf(format, args...)
}

func (p *probeT) FailNow() {
	p.failed = true
	runtime.Goexit()
}
