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
// mu before two readings is FirstReadHaircut of the first-read mu (mu0), the
// median of RunningRequests / AvgServiceTime over the role's saturated
// replicas; while the backlog grows it is the lower of the floor's mu and mu0.
// B is each replica's queue capped at what it drains in T, plus the
// scheduler's queue for the role that owns it; T is floor.DrainHorizon. No
// starting replica is credited: the engine subtracts them as anticipated
// supply already, and crediting them here too counted them twice (#520).

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
// batch and so faster. Measured on the first saturated row of three
// recordings: 7.13 against the 6.2 it settled at (+15 %, pd-restart), 14.29
// against 12.6 (+13 %, throughput-sweep-restart), 6.53 to 6.58 against 6.2
// (+5 % to +6 %, e1 and e1-tuned). A mu read high under-orders, the side that
// breaks the SLO. At 0.85 every one of those lands at or under its settled
// figure (6.06, 12.15, 5.58). The cost is at most 1 / 0.85 = 1.18x on the
// cycles it prices, and on the probe's 463 s starts it is what puts the first
// order at need: without it the time under need came within 1m42s of its
// bound.
const FirstReadHaircut = 0.85

// drainHoldFreshFor is how many cycles a haircut price holds after the cycle
// that set it whether or not any replica is reported starting: the order it
// placed is seen as starting only on a later cycle.
const drainHoldFreshFor = 2

// Drain regions, as the throughput-demand-floor line names them.
const (
	drainBeforeTwoReadings = "before-two-readings"
	drainGrowing           = "growing"
	drainHaircutHeld       = "haircut-held"
)

// drainHold is a role's haircut price, held while the replicas ordered under
// it start (SwitchQueuePricedAsDrain): the first-read mu it was priced at, and
// the cycles since it last priced.
type drainHold struct {
	mu0    float64
	cycles int
}

// drainRead is what one role's rows say this cycle for the drain price.
type drainRead struct {
	// mu0 is the first-read mu: the median, over the role's own replicas
	// that are saturated now, of RunningRequests / AvgServiceTime. Zero with
	// none.
	mu0 float64
	// throughput is what the role's own replicas complete now: the sum of
	// RunningRequests / AvgServiceTime over every replica that reports both.
	throughput float64
	// queues is each own replica's queue as the floor reads it
	// (ReplicaCapacity.QueueLength: on decode of a P/D model, the part that
	// waits for capacity, the full count at the sequence cap).
	queues []float64
	// perReplica is the median per-replica capacity (P) of the role's own
	// replicas, in tokens.
	perReplica float64
}

// drainReads reads, per role that generates output, the rows' first-read mu,
// current throughput, queues and P. A replica is saturated when it is at its
// stated batch cap (domain.ReplicaMetrics.AtBatchCap) or holds KV at or over
// k1 with a queue: a queue alone is not, since on decode of a P/D model the
// requests waiting for their KV from prefill read as one while the batch runs
// far under its cap (pd-restart, 12:28:32: 3.0 req/s against a true 6.2).
// The first-read mu is read only when at least half the role's replicas are
// saturated.
// AvgServiceTime excludes queue wait, and at the cap the batch is fixed, so
// RunningRequests / AvgServiceTime is the replica's own rate for this shape;
// that is not the retired Little's-law floor, which priced the service time
// of whatever batch the replica ran.
func (a *SaturationAnalyzer) drainReads(input domain.AnalyzerInput, replicas []capacity.ReplicaCapacity,
	variants []domain.VariantCapacity, roleOf map[string]string, k1 float64) map[string]drainRead {
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
		if p := perReplica[rm.VariantName]; p > 0 && !math.IsInf(p, 1) {
			ps[role] = append(ps[role], p)
		}
		svc, running := rm.AvgServiceTime, float64(rm.RunningRequests)
		if svc > 0 && running > 0 && !math.IsInf(svc, 1) {
			rate := running / svc
			d.throughput += rate
			c, seen := caps[rm.VariantName]
			if !seen {
				c = a.atCapBatchCap(engineParamsFor(a, input.Namespace, input.ModelID, rm.VariantName), true).perRank
				caps[rm.VariantName] = c
			}
			if rm.AtBatchCap(c) || (k1 > 0 && rm.KvCacheUsage >= k1 && q > 0) {
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
// or the horizon cannot price it.
func drainReplicas(arrival, backlog, horizon, mu float64) float64 {
	if !(mu > 0) || math.IsInf(mu, 1) || !(horizon > 0) {
		return 0
	}
	return (max(arrival, 0) + max(backlog, 0)/horizon) / mu
}

// backlogGrowthRate is how fast a role's backlog grew, in requests per
// second, on the cycle noteBacklogGrowth just recorded: the smaller of the
// slope over the last cycle and the slope since the reading it compared with.
// Negative while the backlog drains. Zero with fewer than two readings.
//
// Signed, because what the replicas complete includes the backlog they
// drain: served + dB/dt is the arrivals whichever way the queue moves. Floored
// at zero it read the drain as arrivals, and priced the same queue twice,
// once as arrivals and once as B / T: on pd-restart at 12:47:36 the replicas
// completed 25.4 req/s against 20.7 arriving, with 988 queued.
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
	if math.IsInf(rate, 1) || math.IsNaN(rate) {
		return 0
	}
	return rate
}

// noteDrainHold updates a role's haircut hold and returns it, and whether it
// holds this cycle. set records a haircut price taken this cycle at mu0. A
// hold lasts while a replica of the role is starting (freshPending) or for
// drainHoldFreshFor cycles after it was last set, whichever is longer.
func (a *SaturationAnalyzer) noteDrainHold(namespace, modelID, role string, set bool, mu0 float64,
	freshPending int, now time.Time) (drainHold, bool) {
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
	if held {
		h.cycles++
		if freshPending == 0 && h.cycles >= drainHoldFreshFor {
			held = false
		}
	}
	if set && mu0 > 0 {
		h, held = drainHold{mu0: mu0}, true
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

// freshPendingByRole is, per role, the replicas starting that are still
// plausibly starting: within floor's two starts where an age is known
// (startSeconds), every one where none is.
func freshPendingByRole(variants []domain.VariantCapacity, startSeconds map[string]float64) map[string]int {
	out := make(map[string]int)
	for _, vc := range variants {
		role := domain.CanonicalRole(vc.Role)
		s := startSeconds[role]
		if len(vc.PendingAges) == 0 || !(s > 0) {
			out[role] += max(vc.PendingReplicas, 0)
			continue
		}
		n := 0
		for _, age := range vc.PendingAges {
			if age <= floor.StaleStartFactor*s {
				n++
			}
		}
		out[role] += min(n, max(vc.PendingReplicas, 0))
	}
	return out
}

// drainContext is what every role's drain price reads this cycle.
type drainContext struct {
	lambda       float64
	startSeconds map[string]float64
	freshPending map[string]int
	grew         map[string]bool
	now          time.Time
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
// t.want; and, while a haircut price holds (noteDrainHold), at least that
// price at this cycle's arrivals and queue.
//
// The regions, on a role with a backlog:
//
//   - before two readings: no mu that may order (no reading, one, or held),
//     and a first-read mu from a saturated row. mu = FirstReadHaircut x mu0,
//     P the role's own.
//   - growing: a mu that may order, and the backlog grew (backlogGrew). mu =
//     min(the floor's mu, mu0), P the floor's.
//
// A mu that may order with the backlog standing is the floor's swap, as
// before. The price may raise demand above occupancy's: on a load step the
// arrivals outrun what is resident, and that raise is what puts the probe's
// 463 s starts at need on the first saturated cycle.
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
	// The first read this cycle, or, while a haircut price holds, the one it
	// was taken at: the replicas that land under it start unsaturated, and
	// a fleet whose majority is not yet saturated gives no read
	// (drainReads). Without the held read, the 463 s load step fell back to
	// occupancy's residency charge the cycle its first order landed, and
	// peaked at 15 against a ceiling of 14.5.
	firstRead := d.mu0
	if prev, ok := a.drainHoldOf(namespace, modelID, role); ok && !(firstRead > 0) {
		firstRead = prev.mu0
	}
	var p float64
	haircut := false
	switch {
	case !(queued > 0):
	case t.inFloor && t.term.MayOrder && !t.term.Held && !grew:
		// The floor's swap.
	case t.inFloor && t.term.MayOrder && grew:
		out.region, out.mu, p = drainGrowing, t.term.Mu, t.term.PerReplica
		if d.mu0 > 0 {
			out.mu = min(out.mu, d.mu0)
		}
	case firstRead > 0:
		out.region, out.mu, p, haircut = drainBeforeTwoReadings, a.drainHaircut*firstRead, d.perReplica, true
	}
	want := t.want
	if out.region != "" && out.mu > 0 && p > 0 {
		out.backlog = cappedBacklog(d.queues, t.scheduler, out.mu, out.horizon)
		out.replicas = drainReplicas(out.arrival, out.backlog, out.horizon, out.mu)
		want = max(t.resident, out.replicas*p)
		if t.inFloor {
			want = max(want, t.tokens)
		}
	} else {
		out.region, out.mu = "", 0
	}
	hold, held := a.noteDrainHold(namespace, modelID, role, haircut, firstRead, c.freshPending[role], c.now)
	if held && !haircut && d.perReplica > 0 {
		mu := hold.mu0
		if d.mu0 > 0 {
			mu = min(mu, d.mu0)
		}
		mu *= a.drainHaircut
		b := cappedBacklog(d.queues, t.scheduler, mu, out.horizon)
		n := drainReplicas(out.arrival, b, out.horizon, mu)
		if n*d.perReplica > want {
			want = n * d.perReplica
			out.region, out.mu, out.backlog, out.replicas = drainHaircutHeld, mu, b, n
		}
	}
	return want, out
}
