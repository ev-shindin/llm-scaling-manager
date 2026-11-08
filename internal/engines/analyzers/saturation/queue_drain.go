package saturation

import (
	"math"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
)

// Queued work priced by drain time before mu (issue #520, cause B;
// docs/proposals/analyzer-learner.md, "Queued work priced by drain time before
// mu"). Occupancy charges a queued request as if it were resident, queue x
// (input + output) tokens, and the throughput floor swaps that charge for a
// drain price only where its term may order and the backlog is not growing
// (flooredDemand). A load step hits both gates at once: on pd-restart the
// decode role read 9.87 replicas against a need of 3, 260k of its demand the
// residency charge on 130 queued requests.
//
// On a role that generates output, with a backlog, the queue is priced as work
// to drain instead, in two regions: before the role has a mu that may order,
// and while its backlog grows. Per role:
//
//	arrival = max(lambda, sum of RunningRequests / AvgServiceTime + dB/dt)
//	N       = (arrival + B / T) / mu
//	demand  = max(resident KV, N x P, the floor's tokens)
//
// dB/dt is signed and counted on every cycle (backlogGrowthRate). mu before
// two readings is FirstReadHaircut of the first-read mu (mu0), the median of
// RunningRequests / AvgServiceTime over the role's replicas at their stated
// batch cap; while the backlog grows it is the lowest of the floor's mu, mu0
// and what the queued replicas complete (drainRead.served). B is
// each replica's queue capped at what it drains in T, plus the scheduler's
// queue for the role that owns it; T is floor.DrainHorizon. No starting
// replica is credited: the engine subtracts them as anticipated supply
// already, and crediting them here too counted them twice (#520).

// SwitchQueuePricedAsDrain is the replay switch for queued work priced by
// drain time where the floor's swap does not run (queue_drain.go). On, a
// generating role with a backlog is priced at max(resident KV, N x P, the
// floor's tokens) before its mu may order and while its backlog grows, and a
// price taken under the first-read haircut holds until every replica ordered
// under it is Ready. Off, as before, occupancy's residency charge stood there.
const SwitchQueuePricedAsDrain = "queue-priced-as-drain"

// FirstReadHaircut is the share of the first-read mu the drain price divides
// by before the role has two mu readings.
//
// The first read is high: its one-minute service-time window still holds
// completions from before the replica reached its cap, which ran on a smaller
// batch and so faster. Measured on the first at-cap row of three recordings:
// 7.13 against the 6.2 it settled at (+15 %, pd-restart), 14.29 against 12.6
// (+13 %, throughput-sweep-restart), 6.53 to 6.58 against 6.2 (+5 % to +6 %,
// e1 and e1-tuned). A mu read high under-orders, the side that breaks the
// SLO. At 0.85 every one of those lands at or under its settled figure (6.06,
// 12.15, 5.58). The cost is at most 1 / 0.85 = 1.18x on the cycles it prices.
//
// It bounds a read at the batch cap only. A row saturated by its KV, under the
// cap, reads far higher, because its batch and its service time are still
// climbing: on ttft-staircase-b-off at 02:06:23 two replicas at batches of 126
// and 128 under a cap of 256 read 31.3 req/s against the 16.7 to 19.3 they
// served, 1.6x to 1.9x. So such rows give no first read (drainReads).
const FirstReadHaircut = 0.85

// drainHoldFreshFor is how many cycles a haircut price holds after the cycle
// that set it whether or not any replica is reported starting: the order it
// placed is seen as starting only on a later cycle.
const drainHoldFreshFor = 2

// drainHoldHorizons caps a hold at this many drain horizons after it was last
// set, however many replicas still report starting: a start longer than two
// horizons (each at least one start) is not a start (floor.FreshStarting), and
// a replica that reports no age cannot be told from one that never lands.
const drainHoldHorizons = 2

// Drain regions, as the throughput-demand-floor line names them.
const (
	drainBeforeTwoReadings = "before-two-readings"
	drainGrowing           = "growing"
	drainHaircutHeld       = "haircut-held"
)

// drainHold is a role's haircut price, held while the replicas ordered under
// it start (SwitchQueuePricedAsDrain): the first-read mu it was priced at, the
// cycles since it last priced, and when that was.
type drainHold struct {
	mu0    float64
	cycles int
	setAt  time.Time
}

// drainRead is what one role's rows say this cycle for the drain price.
type drainRead struct {
	// mu0 is the first-read mu: the median, over the role's own replicas at
	// their stated batch cap, of RunningRequests / AvgServiceTime. Zero with
	// none, or with fewer than half the role's replicas at the cap.
	mu0 float64
	// throughput is what the role's own replicas complete now: the sum of
	// RunningRequests / AvgServiceTime over every replica that reports both.
	throughput float64
	// served is what the role's own replicas with a queue complete each
	// (RequestRate), the median over them; zero with fewer than half the
	// role's replicas queued.
	served float64
	// queues is each own replica's queue as the floor reads it
	// (ReplicaCapacity.QueueLength: on decode of a P/D model, the part that
	// waits for capacity, the full count at the sequence cap).
	queues []float64
	// perReplica is the median per-replica capacity (P) of the role's own
	// replicas, in tokens.
	perReplica float64
}

// drainReads reads, per role that generates output, the rows' first-read mu,
// current throughput, completions, queues and P.
//
// The first read comes only from rows at their stated batch cap
// (domain.ReplicaMetrics.AtBatchCap), and only when at least half the role's
// replicas are there. At the cap the batch is fixed, so RunningRequests /
// AvgServiceTime (service time excludes queue wait) is the replica's own rate
// for this shape, high by no more than the window's mix of earlier
// completions (FirstReadHaircut); that is not the retired Little's-law floor,
// which priced the service time of whatever batch the replica ran. A row
// saturated by its KV under the cap is still growing its batch, and reads
// 1.6x to 1.9x high: it keeps occupancy's pricing. A queue alone is not
// saturation either: on decode of a P/D model the requests waiting for their
// KV from prefill read as one while the batch runs far under its cap.
func (a *SaturationAnalyzer) drainReads(input domain.AnalyzerInput, replicas []capacity.ReplicaCapacity,
	variants []domain.VariantCapacity, roleOf map[string]string) map[string]drainRead {
	perReplica := make(map[string]float64, len(variants))
	for _, vc := range variants {
		perReplica[vc.VariantName] = vc.PerReplicaCapacity
	}
	queueOf := make(map[string]int, len(replicas))
	for _, rc := range replicas {
		if !rc.FromWarmPool {
			queueOf[rc.VariantName+"|"+rc.PodName] = rc.QueueLength
		}
	}
	caps := make(map[string]int64)
	mus := make(map[string][]float64)
	ps := make(map[string][]float64)
	queuedServed := make(map[string][]float64)
	out := make(map[string]drainRead)
	for _, rm := range input.ReplicaMetrics {
		role, known := roleOf[rm.VariantName]
		if !known || role == domain.RolePrefill || rm.FromWarmPool {
			continue
		}
		q, own := queueOf[rm.VariantName+"|"+rm.PodName]
		if !own {
			continue
		}
		d := out[role]
		d.queues = append(d.queues, float64(q))
		if p := perReplica[rm.VariantName]; finitePositive(p) {
			ps[role] = append(ps[role], p)
		}
		if q > 0 && finitePositive(rm.RequestRate) {
			queuedServed[role] = append(queuedServed[role], rm.RequestRate)
		}
		svc, running := rm.AvgServiceTime, float64(rm.RunningRequests)
		if finitePositive(svc) && running > 0 {
			rate := running / svc
			d.throughput += rate
			c, seen := caps[rm.VariantName]
			if !seen {
				c = a.atCapBatchCap(engineParamsFor(a, input.Namespace, input.ModelID, rm.VariantName), true).perRank
				caps[rm.VariantName] = c
			}
			if rm.AtBatchCap(c) {
				mus[role] = append(mus[role], rate)
			}
		}
		out[role] = d
	}
	for role, d := range out {
		// From at least half the role's replicas, or none: a minority that
		// saturated is a slow replica, not the fleet's rate. One pod at 0.3
		// of its fifteen siblings read as the fleet's first read ordered 58
		// replicas for a need of 18 on the probe's slow-pod case; the fleet's
		// own mu learner keeps that pod to its weight (mu_slow_pod.go).
		if 2*len(mus[role]) >= len(d.queues) {
			d.mu0, _ = medianRate(mus[role])
		}
		d.perReplica, _ = medianRate(ps[role])
		if 2*len(queuedServed[role]) >= len(d.queues) {
			d.served, _ = medianRate(queuedServed[role])
		}
		out[role] = d
	}
	return out
}

// cappedBacklog is B for a drain price at mu over horizon: each replica's own
// queue capped at what it drains in the horizon (localQueueBacklog's cap,
// kept while the queue grows: growth is priced through dB/dt), plus the
// scheduler's queue, uncapped, which the router may send to any replica.
func cappedBacklog(queues []float64, scheduler, mu, horizon float64) float64 {
	b := max(scheduler, 0)
	for _, q := range queues {
		b += min(q, mu*horizon)
	}
	return b
}

// drainReplicas is N = (arrival + backlog / horizon) / mu, the replicas at
// 100 % that keep up with arrival and clear backlog in horizon. Zero when mu
// or the horizon cannot price it, or arrival or backlog is not finite.
func drainReplicas(arrival, backlog, horizon, mu float64) float64 {
	if !finitePositive(mu) || !(horizon > 0) || math.IsNaN(arrival) || math.IsInf(arrival, 0) ||
		math.IsNaN(backlog) || math.IsInf(backlog, 0) {
		return 0
	}
	return (max(arrival, 0) + max(backlog, 0)/horizon) / mu
}

// backlogGrowthRate is how fast a role's backlog grew, in requests per
// second, on the cycle noteBacklogGrowth just recorded: the smaller of the
// slope over the last cycle and the slope since the reading it compared with.
// Negative while the backlog drains. Zero with fewer than two readings.
//
// Signed, and read on every cycle, not only while backlogGrew is set: what
// the replicas complete includes the backlog they drain, so served + dB/dt is
// the arrivals whichever way the queue moves. Floored at zero it read the
// drain as arrivals, and priced the same queue twice, once as arrivals and
// once as B / T: on pd-restart at 12:47:36 the replicas completed 25.4 req/s
// against 20.7 arriving, with 988 queued. The max with lambda keeps a noisy
// negative slope from pricing under the router's rate.
//
// The smaller, not the larger: backlogGrew lingers for a minute after the
// growth stops, and so does the slope over that minute. Read as the larger,
// a queue that had stopped growing when the first replicas landed still
// added 2.83 req/s to the arrivals on the probe's 90 s load step (12:03:45,
// the queue down from 420 to 410), and the haircut price ordered 16 for a
// need of 9. The slope over the last cycle says the growth stopped; the slope
// over the minute keeps one cycle's jitter from reading as growth.
func (a *SaturationAnalyzer) backlogGrowthRate(namespace, modelID, role string) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	memo := a.k2Settle[namespace+"|"+modelID]
	if memo == nil || len(memo.backlogs) < 2 {
		return 0
	}
	cur := memo.backlogs[len(memo.backlogs)-1]
	rate := math.Inf(1)
	for _, ref := range []backlogReading{memo.backlogs[len(memo.backlogs)-2], memo.backlogs[0]} {
		if dt := cur.at.Sub(ref.at).Seconds(); dt > 0 {
			rate = min(rate, (cur.byRole[role]-ref.byRole[role])/dt)
		}
	}
	if math.IsInf(rate, 0) || math.IsNaN(rate) {
		return 0
	}
	return rate
}

// drainHoldStep reads and updates a role's haircut hold in one locked step.
// decide is given the hold as the last cycle left it (and whether there is
// one) and returns whether this cycle took a haircut price, at which first
// read. The hold then lasts while a replica of the role is still starting
// (freshPending), or for drainHoldFreshFor cycles after it was last set,
// whichever is longer, and never past drainHoldHorizons horizons after it was
// set. It returns the hold after the update and whether it holds. decide runs
// under a.mu and must take no lock.
func (a *SaturationAnalyzer) drainHoldStep(namespace, modelID, role string, freshPending int, horizon float64,
	now time.Time, decide func(prev drainHold, held bool) (set bool, mu0 float64)) (drainHold, bool) {
	model := namespace + "|" + modelID
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.k2Settle == nil {
		a.k2Settle = make(map[string]*settleMemo)
	}
	memo, ok := a.k2Settle[model]
	if !ok {
		memo = newSettleMemo(now)
		a.k2Settle[model] = memo
	}
	if memo.drainHolds == nil {
		memo.drainHolds = make(map[string]drainHold)
	}
	h, held := memo.drainHolds[role]
	set, mu0 := decide(h, held)
	if held {
		h.cycles++
		expired := horizon > 0 && now.Sub(h.setAt).Seconds() > drainHoldHorizons*horizon
		if expired || (freshPending == 0 && h.cycles >= drainHoldFreshFor) {
			held = false
		}
	}
	if set && mu0 > 0 {
		h, held = drainHold{mu0: mu0, setAt: now}, true
	}
	if !held {
		delete(memo.drainHolds, role)
		return drainHold{}, false
	}
	memo.drainHolds[role] = h
	return h, true
}

// drainHoldOf is a role's haircut hold as the last cycle left it, and
// whether one is held; it changes nothing.
func (a *SaturationAnalyzer) drainHoldOf(namespace, modelID, role string) (drainHold, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	memo := a.k2Settle[namespace+"|"+modelID]
	if memo == nil {
		return drainHold{}, false
	}
	h, ok := memo.drainHolds[role]
	return h, ok
}

// clearDrainHolds drops every haircut hold of a model: a shape change
// (staleShape) makes the first read it was taken at another shape's.
func (a *SaturationAnalyzer) clearDrainHolds(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if memo := a.k2Settle[namespace+"|"+modelID]; memo != nil {
		memo.drainHolds = nil
	}
}

// freshPendingByRole is, per role, the replicas starting that are still
// plausibly starting (floor.FreshStarting): stuck ones left out, and those
// with an age past two starts.
func freshPendingByRole(variants []domain.VariantCapacity, startSeconds map[string]float64) map[string]int {
	out := make(map[string]int)
	for _, vc := range variants {
		role := domain.CanonicalRole(vc.Role)
		out[role] += floor.FreshStarting(vc.PendingReplicas, vc.StuckReplicas, vc.PendingAges, startSeconds[role])
	}
	return out
}

// drainContext is what every role's drain price reads this cycle.
type drainContext struct {
	// lambda is the model's arrival rate, zero when it is not a finite
	// positive figure (newDrainContext).
	lambda       float64
	startSeconds map[string]float64
	freshPending map[string]int
	grew         map[string]bool
	now          time.Time
	// minServedShare is the least share of the floor's mu what the queued
	// replicas complete may price at (MuChoice.MinMeasuredOfDerived).
	minServedShare float64
}

// newDrainContext is the drain price's view of a cycle. A lambda that is not
// a finite positive figure is read as none, as floor.Estimate refuses it: a
// NaN passes the builtin max, and an +Inf priced demand at +Inf.
func newDrainContext(lambda float64, startSeconds map[string]float64, variants []domain.VariantCapacity,
	grew map[string]bool, now time.Time, minServedShare float64) drainContext {
	if !finitePositive(lambda) {
		lambda = 0
	}
	return drainContext{lambda: lambda, startSeconds: startSeconds,
		freshPending: freshPendingByRole(variants, startSeconds), grew: grew, now: now,
		minServedShare: minServedShare}
}

// drainTerm is one role's floor term and demand, as applyThroughputFloor has
// them when the drain price is taken.
type drainTerm struct {
	term    floor.Term
	inFloor bool
	// tokens is the floor's figure for the role, zero when it has none.
	tokens float64
	// want is the role's demand without the drain price, and resident its
	// measured demand with the queues' residency charge taken out.
	want, resident float64
	// scheduler is the scheduler's queue, for the role that owns it.
	scheduler float64
}

// drainPriced is what priceDrain priced a role at, for the log line.
type drainPriced struct {
	region                     string
	arrival, growth, mu, mu0   float64
	backlog, horizon, replicas float64
}

// priceDrain is a role's demand under SwitchQueuePricedAsDrain, and what it
// priced: max(resident, N x P, the floor's tokens) in a drain region, else
// t.want; and, while a haircut price holds (drainHoldStep) and a backlog
// stands, at least that price at this cycle's arrivals and queue.
//
// The regions, on a role with a backlog:
//
//   - before two readings: no mu that may order (no reading, one, or held),
//     and a first read at the batch cap, this cycle's or the one a held
//     haircut was taken at. mu = FirstReadHaircut x the read, P the role's
//     own.
//   - growing: a mu that may order, and the backlog grew (backlogGrew). mu =
//     the lowest of the floor's mu, this cycle's first read and what the
//     queued replicas complete (drainRead.served), P the floor's.
//
// A mu that may order with the backlog standing is the floor's swap, as
// before. The price may raise demand above occupancy's: on a load step the
// arrivals outrun what is resident. With no backlog the hold prices nothing:
// the queue its order was for has drained, and what remains is the floor's.
func (a *SaturationAnalyzer) priceDrain(namespace, modelID, role string, d drainRead, c drainContext,
	t drainTerm) (float64, drainPriced) {
	queued := t.scheduler
	for _, q := range d.queues {
		queued += q
	}
	grew := c.grew[role]
	out := drainPriced{mu0: d.mu0, horizon: floor.DrainHorizon(floor.BacklogDrainSeconds, c.startSeconds[role])}
	out.growth = a.backlogGrowthRate(namespace, modelID, role)
	out.arrival = max(c.lambda, d.throughput+out.growth)
	var p, holdMu float64
	region := ""
	_, held := a.drainHoldStep(namespace, modelID, role, c.freshPending[role], out.horizon, c.now,
		func(prev drainHold, held bool) (bool, float64) {
			// The first read this cycle, or, while a haircut price holds, the
			// one it was taken at: the replicas that land under it start under
			// their cap, and a fleet whose majority is not at it gives no read.
			firstRead := d.mu0
			if held && !(firstRead > 0) {
				firstRead = prev.mu0
			}
			if held {
				holdMu = prev.mu0
				if d.mu0 > 0 {
					holdMu = min(holdMu, d.mu0)
				}
			}
			switch {
			case !(queued > 0):
			case t.inFloor && t.term.MayOrder && !t.term.Held && !grew:
				// The floor's swap.
			case t.inFloor && t.term.MayOrder && grew:
				region, out.mu, p = drainGrowing, t.term.Mu, t.term.PerReplica
				if d.mu0 > 0 {
					out.mu = min(out.mu, d.mu0)
				}
				// A replica with a queue completes as much as it can, so
				// what the queued replicas complete is the most a replica
				// serves: a mu above it (a derived mu, a first read taken
				// while the batch still climbed) prices them over what they
				// do. Read from a majority only, as the first read is: one
				// slow pod's queue is not the fleet's rate. And not under
				// MinMeasuredOfDerived of the floor's mu, the bar mu's own
				// choice sets a measurement: a one-minute rate that has
				// just begun to queue reads a step late, and priced
				// pd-restart's 12:45:05 at 18.9 replicas (one queued
				// replica, mu 6.2) and e1-tuned's 16:24:13 at 35.
				if d.served > 0 && d.served >= c.minServedShare*t.term.Mu {
					out.mu = min(out.mu, d.served)
				}
			case firstRead > 0:
				region, out.mu, p = drainBeforeTwoReadings, a.drainHaircut*firstRead, d.perReplica
				return true, firstRead
			}
			return false, 0
		})
	want := t.want
	if region != "" && finitePositive(out.mu) && finitePositive(p) {
		out.region = region
		out.backlog = cappedBacklog(d.queues, t.scheduler, out.mu, out.horizon)
		out.replicas = drainReplicas(out.arrival, out.backlog, out.horizon, out.mu)
		want = max(t.resident, out.replicas*p)
		if t.inFloor {
			want = max(want, t.tokens)
		}
	} else {
		out.mu = 0
	}
	if held && region != drainBeforeTwoReadings && queued > 0 && finitePositive(holdMu) && finitePositive(d.perReplica) {
		mu := holdMu * a.drainHaircut
		b := cappedBacklog(d.queues, t.scheduler, mu, out.horizon)
		n := drainReplicas(out.arrival, b, out.horizon, mu)
		if n*d.perReplica > want {
			want = n * d.perReplica
			out.region, out.mu, out.backlog, out.replicas = drainHaircutHeld, mu, b, n
		}
	}
	if math.IsNaN(want) || math.IsInf(want, 0) {
		return t.want, drainPriced{}
	}
	return want, out
}
