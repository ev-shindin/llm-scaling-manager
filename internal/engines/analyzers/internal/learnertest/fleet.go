package learnertest

import (
	"fmt"
	"math"
	"time"
)

// The closed-loop probe's fleet simulator (analyzer-learner.md, 2.5): an
// analyzer driven by a fleet that turns its order into replicas. The open
// loop replays recorded inputs that do not depend on outputs; holds,
// latches, the sibling yield and a self-sealing low figure are closed-loop
// behaviours -- the order changes the load each replica sees, and so what the
// analyzer is next shown -- and only a loop sees them.
//
// The simulator, one Cycle at a time:
//
//   - Arrivals split equally over the Ready replicas, or by Weight, a
//     router that steers.
//   - Each replica serves up to its true mu; what it cannot serve queues on
//     it, and its queue drains when its share falls under its mu.
//   - The step (the analyzer under test, and how its rows are made from the
//     replicas) returns an order, in replicas.
//   - An order above the fleet starts replicas, Ready after the start time
//     S. An order below it removes replicas, newest first, once the highest
//     order of the last stabilization window is below the fleet, and no
//     replica is starting.
//   - A replica just Ready is first seen by the analyzer then.
//
// The verdict is against need, which the case defines: the fleet a truthful
// analyzer orders at the arrival rate of the moment.

// Simulator defaults.
const (
	// DefaultStartTime is S, from an order to the replica Ready.
	DefaultStartTime = 90 * time.Second
	// DefaultStabilization is the scale-down stabilization window.
	DefaultStabilization = 5 * time.Minute
	// OverTolerance is how far over need the fleet may settle, as a
	// fraction, plus one replica of rounding.
	OverTolerance = 0.15
	// UnderFor is how long the fleet may sit below need in one run: the
	// start time, the joined replica's settle, and a cycle or two of
	// ordering on either side of them.
	UnderFor = 5 * time.Minute
)

// Fleet is one closed-loop scenario.
type Fleet struct {
	// Pods is the fleet's true mu per replica at the start; added replicas
	// serve AddedMu.
	Pods    []float64
	AddedMu float64
	// Mu, when set, is a replica's true mu at an elapsed time, overriding
	// Pods and AddedMu (a slow replica that recovers).
	Mu func(i int, elapsed time.Duration) float64
	// Lambda is the arrival rate at an elapsed time.
	Lambda func(elapsed time.Duration) float64
	// Weight, when set, is a replica's share of the arrivals relative to
	// the other Ready replicas' at an elapsed time: a router that steers.
	// Nil splits them equally, and so do weights that sum to zero or less,
	// or to no number, on a cycle.
	Weight  func(i int, elapsed time.Duration) float64
	Horizon time.Duration
	// Need is the fleet a truthful analyzer orders at arrival rate lambda.
	Need func(lambda float64) int
	// Start is the first cycle's instant; Start (the package's) when zero.
	Start time.Time
	// StartTime replaces DefaultStartTime, and Stabilization
	// DefaultStabilization, when set.
	StartTime, Stabilization time.Duration
	// MaxReplicas caps an order, as a scale target's maxReplicas does; zero
	// is no cap. An analyzer whose order feeds itself (each joiner it starts
	// lowers its figure) runs into it rather than without end.
	MaxReplicas int
}

// Replica is one Ready replica on a cycle, as the step sees it.
type Replica struct {
	// Index is the replica's position in the fleet, Name its Pod name, and
	// ReadyAt when it became Ready.
	Index   int
	Name    string
	ReadyAt time.Time
	// Mu is its true rate, Offered its share of the arrivals, Served what
	// it served, and Backlog its queue after the cycle.
	Mu, Offered, Served, Backlog float64
	// Saturated is whether it served at its limit: it queued, or was
	// offered its mu or more.
	Saturated bool
}

// Step is one analyzer cycle at now: the Ready replicas under arrival rate
// lambda, elapsed into the run. It returns the replicas the analyzer orders,
// or Hold.
type Step func(now time.Time, elapsed time.Duration, lambda float64, ready []Replica) int

// Hold is a Step's order when the analyzer orders nothing on the cycle (it
// has no figure to price from): the fleet, starting replicas included, is
// held as it is, and the hold counts in the stabilization window as an
// order of that size.
const Hold = 0

// Result is what the loop did.
type Result struct {
	// Need is need at the horizon's arrival rate; Final and Peak the fleet
	// at the horizon and at its largest, starting replicas included.
	Need, Final, Peak int
	// OverSince is how long before the horizon the fleet has been over
	// need's tolerance without a break; zero when it ends inside it.
	OverSince time.Duration
	// UnderFor is the longest run the fleet sat below need at the arrival
	// rate of the moment.
	UnderFor time.Duration
	// Trace is the fleet on each cycle.
	Trace []int
}

// pod is one simulated replica.
type pod struct {
	name    string
	readyAt time.Time
	backlog float64
}

// Run drives the fleet to its horizon, one Cycle at a time, and returns what
// it did.
func (f Fleet) Run(step Step) Result {
	start := f.Start
	if start.IsZero() {
		start = Start
	}
	startTime, stabilization := DefaultStartTime, DefaultStabilization
	if f.StartTime > 0 {
		startTime = f.StartTime
	}
	if f.Stabilization > 0 {
		stabilization = f.Stabilization
	}
	pods := make([]*pod, 0, len(f.Pods))
	for i := range f.Pods {
		pods = append(pods, &pod{name: fmt.Sprintf("r%02d", i), readyAt: start.Add(-time.Hour)})
	}
	trueMu := func(i int, elapsed time.Duration) float64 {
		if f.Mu != nil {
			return f.Mu(i, elapsed)
		}
		if i < len(f.Pods) {
			return f.Pods[i]
		}
		return f.AddedMu
	}
	type order struct {
		at time.Time
		n  int
	}
	var orders []order
	res := Result{Need: f.Need(f.Lambda(f.Horizon))}
	bound := float64(res.Need)*(1+OverTolerance) + 1
	var overFrom, underFrom time.Time
	dt := Cycle.Seconds()
	for now := start; now.Sub(start) < f.Horizon; now = now.Add(Cycle) {
		elapsed := now.Sub(start)
		// next is the cycle's end: an order, a started replica and the
		// verdicts are stamped with it, as the analyzer's clock has moved
		// past the cycle it analyzed.
		next := now.Add(Cycle)
		lambda := f.Lambda(elapsed)
		var ready []int
		for i, p := range pods {
			if !p.readyAt.After(now) {
				ready = append(ready, i)
			}
		}
		replicas := make([]Replica, 0, len(ready))
		weights := 0.0
		weight := func(i int) float64 {
			if f.Weight == nil {
				return 1
			}
			return f.Weight(i, elapsed)
		}
		for _, i := range ready {
			weights += weight(i)
		}
		if !(weights > 0) || math.IsInf(weights, 1) {
			weight, weights = func(int) float64 { return 1 }, float64(len(ready))
		}
		for _, i := range ready {
			p, mu := pods[i], trueMu(i, elapsed)
			offered := lambda * weight(i) / weights
			p.backlog = max(p.backlog+(offered-mu)*dt, 0)
			saturated := p.backlog > 0 || offered >= mu
			served := offered
			if saturated {
				served = mu
			}
			replicas = append(replicas, Replica{Index: i, Name: p.name, ReadyAt: p.readyAt, Mu: mu,
				Offered: offered, Served: served, Backlog: p.backlog, Saturated: saturated})
		}
		ordered := step(now, elapsed, lambda, replicas)
		if ordered <= Hold {
			ordered = len(pods)
		}
		if f.MaxReplicas > 0 {
			ordered = min(ordered, f.MaxReplicas)
		}
		orders = append(orders, order{at: next, n: ordered})
		for len(orders) > 0 && next.Sub(orders[0].at) > stabilization {
			orders = orders[1:]
		}
		starting := len(pods) - len(ready)
		switch {
		case ordered > len(pods):
			for range ordered - len(pods) {
				pods = append(pods, &pod{name: fmt.Sprintf("r%02d", len(pods)), readyAt: next.Add(startTime)})
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
		res.Trace = append(res.Trace, len(pods))
		res.Peak = max(res.Peak, len(pods))
		if len(pods) < f.Need(lambda) {
			if underFrom.IsZero() {
				underFrom = now
			}
			res.UnderFor = max(res.UnderFor, next.Sub(underFrom))
		} else {
			underFrom = time.Time{}
		}
		if float64(len(pods)) > bound {
			if overFrom.IsZero() {
				overFrom = next
			}
		} else {
			overFrom = time.Time{}
		}
	}
	end := start.Add(time.Duration(len(res.Trace)) * Cycle)
	res.Final = len(pods)
	if !overFrom.IsZero() {
		res.OverSince = end.Sub(overFrom)
	}
	return res
}

// Verdict is "" when the fleet ends within tolerance of need, else why not.
// Over-ordered: the fleet ends more than OverTolerance plus one replica over
// need. Under-ordered: it ends more than one replica under need, or sat
// below need at the arrival rate of the moment for longer than UnderFor at
// any point of the run. A shorter dip, while an order is starting and
// settling, is the start time, not a verdict.
func (r Result) Verdict() string {
	bound := float64(r.Need)*(1+OverTolerance) + 1
	switch {
	case float64(r.Final) > bound:
		return fmt.Sprintf("over-ordered: %d replicas against a need of %d (bound %.1f), over it for the last %s",
			r.Final, r.Need, bound, r.OverSince)
	case r.Final < r.Need-1:
		return fmt.Sprintf("under-ordered: %d replicas against a need of %d", r.Final, r.Need)
	case r.UnderFor > UnderFor:
		return fmt.Sprintf("under-ordered mid-run: below need for %s, over the %s a start and a settle take",
			r.UnderFor, UnderFor)
	}
	return ""
}

// Ceil is the order for a fractional replica count: rounded up, at least
// one, and one for NaN.
func Ceil(replicas float64) int {
	n := int(math.Ceil(replicas - 1e-9))
	if math.IsNaN(replicas) || n < 1 {
		return 1
	}
	return n
}
