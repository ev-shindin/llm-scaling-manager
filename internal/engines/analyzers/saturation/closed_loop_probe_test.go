package saturation

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The closed-loop probe (analyzer-learner.md, 2.5): the saturation analyzer
// driven by a fleet simulator that turns its order into replicas. The open
// loop replays recorded inputs that do not depend on outputs; holds,
// latches, the sibling yield and a self-sealing low mu are closed-loop
// behaviours -- the order changes the load each replica sees, and so what the
// analyzer is next shown -- and only a loop sees them.
//
// The simulator, one 15 s cycle at a time:
//
//   - Arrivals split over the Ready replicas by weight (equal by default).
//   - Each replica serves up to its true mu; what it cannot serve queues on
//     it, and its queue drains when its share falls under its mu. A replica
//     serving under its mu holds KV in proportion (0.95 at its limit); one at
//     its limit, or queued, holds 0.95.
//   - The analyzer prices the Ready replicas' rows; its order is the demand
//     over the per-replica capacity at the scale-up target, rounded up.
//   - An order above the fleet starts replicas, Ready after the start time
//     S. An order below it removes replicas, newest first, once the highest
//     order of the last 5 min (the default scale-down stabilization) is
//     below the fleet, and no replica is starting.
//   - A replica just Ready is first seen by the analyzer then, and waits its
//     settle as any joined replica does.
//
// The verdict is against need: the fleet a truthful analyzer orders at the
// arrival rate once every replica serves the fleet's mean mu. At that point
// no replica queues, and the order is occupancy's: each replica holds 0.95
// lambda / (n mu) of its KV, priced against k1 (0.8 of it) at the target
// (0.85), so need is ceil(0.95 lambda / (0.68 mu)) whatever n is.

const (
	// probeStart is S, from an order to the replica Ready.
	probeStart = 90 * time.Second
	// probeStabilization is the scale-down stabilization window.
	probeStabilization = 5 * time.Minute
	// probeOverTolerance is how far over need the fleet may settle, as a
	// fraction, plus one replica of rounding.
	probeOverTolerance = 0.15
)

// probePod is one simulated replica.
type probePod struct {
	name    string
	readyAt time.Time
	backlog float64
}

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
	// lambda is the arrival rate at an elapsed time.
	lambda func(elapsed time.Duration) float64
	// weight is a replica's routing weight; nil splits equally.
	weight  func(i int) float64
	horizon time.Duration
	// meanMu is the fleet's mean true mu at the horizon, which need is
	// priced at.
	meanMu float64
}

// probeResult is what the loop did.
type probeResult struct {
	need, final, peak int
	// overSince is how long before the horizon the fleet has been over
	// need's tolerance without a break; zero when it ends inside it.
	overSince time.Duration
	trace     []int
}

// runProbe drives the case to its horizon and returns what the fleet did.
func runProbe(t *testing.T, pc probeCase) probeResult {
	t.Helper()
	rig := newPropRig()
	start := rig.clock
	pods := make([]*probePod, 0, len(pc.pods))
	for i := range pc.pods {
		pods = append(pods, &probePod{name: fmt.Sprintf("r%02d", i), readyAt: start.Add(-time.Hour)})
	}
	trueMu := func(i int, elapsed time.Duration) float64 {
		if pc.mu != nil {
			return pc.mu(i, elapsed)
		}
		if i < len(pc.pods) {
			return pc.pods[i]
		}
		return pc.addedMu
	}
	type order struct {
		at time.Time
		n  int
	}
	var orders []order
	res := probeResult{need: int(math.Ceil(0.95*pc.lambda(pc.horizon)/(0.68*pc.meanMu) - 1e-9))}
	bound := float64(res.need)*(1+probeOverTolerance) + 1
	var overFrom time.Time
	for rig.clock.Sub(start) < pc.horizon {
		elapsed := rig.clock.Sub(start)
		lambda := pc.lambda(elapsed)
		var ready []int
		total := 0.0
		for i, p := range pods {
			if !p.readyAt.After(rig.clock) {
				ready = append(ready, i)
				w := 1.0
				if pc.weight != nil {
					w = pc.weight(i)
				}
				total += w
			}
		}
		rows := make([]domain.ReplicaMetrics, 0, len(ready))
		dt := propCycle.Seconds()
		for _, i := range ready {
			p, mu := pods[i], trueMu(i, elapsed)
			w := 1.0
			if pc.weight != nil {
				w = pc.weight(i)
			}
			offered := lambda * w / total
			p.backlog = max(p.backlog+(offered-mu)*dt, 0)
			served, kv := offered, 0.95*offered/mu
			if p.backlog > 0 || offered >= mu {
				served, kv = mu, 0.95
			}
			rows = append(rows, imbRow(p.name, "v", kv, int(math.Round(p.backlog)), 1000, 1000, served))
		}
		out, err := rig.analyze(rows, aggStates(len(rows)), lambda, propCycle)
		require.NoError(t, err)
		ordered := int(math.Ceil(impliedReplicas(out) - 1e-9))
		if math.IsNaN(impliedReplicas(out)) || ordered < 1 {
			ordered = 1
		}
		orders = append(orders, order{at: rig.clock, n: ordered})
		for len(orders) > 0 && rig.clock.Sub(orders[0].at) > probeStabilization {
			orders = orders[1:]
		}
		starting := len(pods) - len(ready)
		switch {
		case ordered > len(pods):
			for range ordered - len(pods) {
				pods = append(pods, &probePod{name: fmt.Sprintf("r%02d", len(pods)), readyAt: rig.clock.Add(probeStart)})
			}
		case starting == 0:
			keep := 0
			for _, o := range orders {
				keep = max(keep, o.n)
			}
			if keep < len(pods) {
				pods = pods[:keep]
			}
		}
		res.trace = append(res.trace, len(pods))
		res.peak = max(res.peak, len(pods))
		if float64(len(pods)) > bound {
			if overFrom.IsZero() {
				overFrom = rig.clock
			}
		} else {
			overFrom = time.Time{}
		}
	}
	res.final = len(pods)
	if !overFrom.IsZero() {
		res.overSince = rig.clock.Sub(overFrom)
	}
	return res
}

// verdict is "" when the fleet ends within tolerance of need, else why not.
func (r probeResult) verdict() string {
	bound := float64(r.need)*(1+probeOverTolerance) + 1
	switch {
	case float64(r.final) > bound:
		return fmt.Sprintf("over-ordered: %d replicas against a need of %d (bound %.1f), over it for the last %s",
			r.final, r.need, bound, r.overSince)
	case r.final < r.need-1:
		return fmt.Sprintf("under-ordered: %d replicas against a need of %d", r.final, r.need)
	}
	return ""
}

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
	t.Logf("need %d, final %d, peak %d; fleet by cycle %v", r.need, r.final, r.peak, r.trace)
	require.Empty(t, r.verdict())
	require.Positive(t, r.peak, "premise: the loop ran")
}

// #282 review round 3 (H1): one replica degraded to 0.3 of its siblings'
// rate, queued, while they serve unqueued; then it recovers. Its low rate is
// refused by the served bound (a sibling serves more than twice it), so it
// never becomes mu, and the fleet settles at need after the recovery.
func TestClosedLoopProbe_OneSlowReplicaUnderTheServedBoundDoesNotSealTheFleet(t *testing.T) {
	r := runProbe(t, slowReplicaCase(0.3))
	t.Logf("need %d, final %d, peak %d; fleet by cycle %v", r.need, r.final, r.peak, r.trace)
	require.Empty(t, r.verdict())
}

// The known-failing case, and the probe's negative control: the same slow
// replica at 0.55 of its siblings' rate passes the served bound (they serve
// 0.8, under twice 0.55), and with no line, and siblings not full, nothing
// it yields to. Its rate becomes mu, the floor orders the fleet on it, the
// replica stops queueing in the larger fleet -- and recovers -- and nothing
// saturates to correct the figure: the fleet stays over-ordered for good,
// the self-sealing low mu of analyzer-learner.md 3.4 (cost side). The probe
// must see it. Open: when a change fixes it, this test fails, and the fix
// turns it into a passing case.
func TestClosedLoopProbe_KnownFailing_ASlowReplicaInsideTheServedBoundSealsTheFleet(t *testing.T) {
	r := runProbe(t, slowReplicaCase(0.55))
	t.Logf("need %d, final %d, peak %d; fleet by cycle %v", r.need, r.final, r.peak, r.trace)
	v := r.verdict()
	require.NotEmpty(t, v, "the probe no longer sees the self-sealing low mu: if it was fixed, make this a passing case")
	require.Contains(t, v, "over-ordered")
	require.GreaterOrEqual(t, r.overSince, 20*time.Minute, "over-ordered for good, not for a transient: %s", v)
	t.Logf("the probe's verdict: %s", v)
}

// slowReplicaCase is sixteen replicas at mu 1, one of them at slow for its
// first 10 min and at 1 after, under 12.8 req/s split equally: 0.8 each, so
// the slow one queues and the rest serve unqueued under a full cache.
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
		horizon: 45 * time.Minute,
	}
}
