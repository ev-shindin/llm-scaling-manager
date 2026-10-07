package floor

import (
	"math"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

const (
	// BacklogDrainSeconds is how long a queued request may wait for capacity
	// that is not yet running: the throughput model prices a backlog of B
	// requests as B / BacklogDrainSeconds extra arrivals per second, so the
	// fleet it asks for clears the backlog in about this long while keeping up
	// with the load.
	//
	// It must not be shorter than a replica's start, or it orders capacity
	// that drains nothing. Sixty keeps the order within one start on the
	// benchmark stands; see "A backlog is throughput, not residency" in
	// docs/developer-guide/analyzer-evidence.md.
	BacklogDrainSeconds = 60.0

	// MinThroughputSamplesToOrder is how many saturated readings a role's own
	// output-length bucket must hold before the throughput floor may ORDER a
	// replica from it; with fewer it holds the fleet and no more.
	//
	// The first reading at a saturation under-reads -- a 1m rate on a replica
	// that has been full for 20 s counts a third of a minute's completions --
	// and an order on it over-provisions in a way that removes the saturation
	// which would have corrected it. The two readings must come from two rate
	// windows, not the same window read twice (the saturation analyzer's
	// recordSaturatedThroughput and ThroughputSampleSpacing). See "Two
	// readings, not one, before a reading may order" in
	// docs/developer-guide/analyzer-evidence.md.
	MinThroughputSamplesToOrder = 2
)

// Floor is the demand the offered load implies per role once each
// role's saturated throughput is known, and the terms it was built from.
type Floor struct {
	// ByRole is the floor in tokens per role; a role with no saturated
	// throughput on record is absent.
	ByRole map[string]float64
	// Terms carries, per role in ByRole, the numbers a bound floor changes a
	// decision with.
	Terms map[string]Term
	// Lambda is the arrival rate the floors were built from.
	Lambda float64
	// DrainSeconds is the backlog drain target the floors were built with.
	DrainSeconds float64
}

// Term is one role's floor arithmetic, kept for the log line.
type Term struct {
	// Mu is the saturated completion rate of one replica, requests/s.
	Mu float64
	// PerReplica is the tokens one replica of the role is worth (P).
	PerReplica float64
	// Backlog is the queued requests priced into the floor: the role's own
	// engine queues plus the scheduler's.
	Backlog float64
	// Replicas is (lambda + Backlog / DrainSeconds) / Mu.
	Replicas float64
	// Held reports that the floor was capped at the fleet's anticipated size
	// because its mu is not one the floor may order on -- HeldWhy says
	// which: "borrowed" (a neighbouring bucket's reading) or "single-sample".
	Held    bool
	HeldWhy string
	// OrderedBehindQueue reports that a window too thin to order on was
	// allowed to anyway, because the SCHEDULER's queue was standing.
	//
	// Not the engines' own queues: a request parked awaiting a remote KV
	// transfer sits in num_requests_waiting and no further replica drains
	// it, which is why the two are separate parameters. And whether or not a
	// replica was already starting -- keeping supply in flight from being
	// re-ordered is the engine's job, through its anticipated-supply
	// subtraction, not this package's.
	//
	// Read it beside Held: the two are exclusive.
	OrderedBehindQueue bool
	// ProjectedBacklog is the backlog the role is priced for: the queue that
	// will exist when ordered capacity becomes Ready, rather than the one
	// standing at the moment of the decision. Equal to the observed backlog when
	// no start time is known.
	ProjectedBacklog float64
	// QueueJustifiedReplicas is how many replicas the standing queue was worth
	// when OrderedBehindQueue released the hold: Q/(mu x drainSeconds), floored
	// at one. Zero when the release did not fire.
	//
	// Logged because it is the number that decides how fast a ramp can climb,
	// and a run that shows only the resulting fleet cannot tell a cap that
	// granted one replica from a queue that only justified one.
	QueueJustifiedReplicas float64
}

// Estimate computes the per-role floor from lambda, the
// backlog per role and the saturated throughput each role's own replicas
// carry. Units: lambda is the model's arrival rate in requests/s; backlog
// is queued requests, keyed by role as canonicalRole spells it (an empty
// variant role is domain.RoleBoth, so a backlog keyed "" prices nothing);
// drainSeconds is the seconds the backlog may take to clear
// (BacklogDrainSeconds), and a drainSeconds <= 0 leaves the backlog
// unpriced; scaleUpThreshold is the (0, 1] scale-up threshold the engine
// sizes with (RC = D / scaleUpThreshold - anticipated), which bounds a hold
// (below). The result is in tokens per role, each role priced at its
// variants' P.
//
// Per role, the floor is (lambda + backlog / drainSeconds) x P / mu, with
// P / mu taken as the median over the role's own (non-bridge) replicas that
// have a throughput on record, each priced at its variant's P. A role none of
// whose replicas has one gets no floor: on a P/D fleet that is prefill in
// practice, whose queue is rarely the one that saturates, and a role that has
// never been seen saturated has no business being sized by this file.
//
// The floor is not capped at the fleet's size (see the file header for what
// the cap cost) -- with one exception. A role whose readings are all borrowed
// from a neighbouring bucket, or whose own window holds fewer than
// MinThroughputSamplesToOrder readings, may hold the fleet but not grow it:
// its floor is capped at scaleUpThreshold x the role's anticipated supply,
// the largest demand the engine's RC = D / scaleUpThreshold - anticipated
// turns into nothing. The term says so (Held, HeldWhy). scaleUpThreshold <= 0
// disables the cap.
//
// A borrowed reading never outvotes a replica's own. The median is taken over
// the role's replicas that read their OWN bucket when any does, and over the
// borrowed ones only when none does -- the rule the saturation analyzer's
// nearestSaturatedThroughput states per key ("used only until the bucket has
// a reading of its own"), applied to the role. Without it a shape switch
// flapped the fleet between two targets for ten minutes, because a fresh
// replica reads a short bucket and borrows the previous shape's mu; see "A
// borrowed reading never outvotes a replica's own" in
// docs/developer-guide/analyzer-evidence.md.
//
// A third kind of held reading arrives with this package's borrowed-LINE
// routing; see borrowedReading below.

// borrowedReading reports whether a replica's service rate is evidence about
// something other than this variant's own current load, and so may hold a fleet
// at its size but not grow it.
//
// Two kinds, and they are different in origin but identical in consequence. A
// reading borrowed from a neighbouring SHAPE bucket is wrong in a known
// direction. A figure derived from an ITL line borrowed from a sibling that
// merely shares an engine configuration is evidence about that CONFIGURATION:
// at a fixed k the resident sequence count is k*C/KVreq, and KVreq is the
// shape, so the same line implies different service rates for two variants
// serving different request shapes.
//
// A derived figure from the variant's OWN line is not borrowed and keeps its
// licence to order without a sample count.
func borrowedReading(rc capacity.ReplicaCapacity) bool {
	if rc.SaturatedThroughputLineBorrowed {
		return true
	}
	return rc.SaturatedThroughputBorrowed && !rc.SaturatedThroughputDerived
}

func Estimate(
	lambda float64,
	replicas []capacity.ReplicaCapacity,
	variants []domain.VariantCapacity,
	backlog map[string]float64,
	drainSeconds float64,
	scaleUpThreshold float64,
	// staleShape forces every role onto the hold path: the caller has seen
	// the fleet's shape change, so no window on record was taken under the
	// shape now arriving, however many readings it holds.
	staleShape bool,
	// schedulerQueued is the requests the SCHEDULER is holding, undispatched.
	// Model-wide, and applied to every role as lambda beside it is: a request
	// in that queue has been dispatched to no pod, and when it is it passes
	// through every role, so each must keep up with all of it -- the reasoning
	// applyThroughputFloor gives for lambda. So on a fleet whose roles both
	// carry thin windows, one queue releases both. That is deliberate, and
	// each release is bounded to one replica of its own role.
	// Separate from backlog, which merges it with the engines' own queues: a
	// request in an engine's queue may be waiting on a remote KV transfer,
	// which no further replica drains, while one in the scheduler's has not
	// been given to a pod at all. Only the latter releases the hold below.
	schedulerQueued float64,
	// startSeconds is how long one replica of each role takes to become Ready,
	// keyed by role. The projection above prices the queue that will exist after
	// that long rather than the one standing now. An absent or zero entry leaves
	// the role on its observed backlog.
	startSeconds map[string]float64,
) Floor {
	out := Floor{Lambda: lambda, DrainSeconds: drainSeconds}
	// !(lambda > 0), not lambda <= 0: every comparison against NaN is false, so
	// the rejected form ADMITS a NaN -- which priceable's own comment in this
	// file calls the exact inverse of the package's contract. It matters more
	// now that lambda is multiplied into the landing projection.
	if !(lambda > 0) || math.IsInf(lambda, 1) || len(replicas) == 0 || len(variants) == 0 {
		return out
	}

	perReplica := make(map[string]float64, len(variants))
	roleOf := make(map[string]string, len(variants))
	// Replica COUNTS per role, for the landing projection below. The token
	// aggregates beside them cannot serve: the projection multiplies a service
	// rate by a number of replicas and a duration, so it needs the count.
	readyByRole := make(map[string]int, len(variants))
	// Per VARIANT, not summed into the role. A role whose variants report
	// differently -- one Pod listing succeeded, another returned nil -- would
	// otherwise have the successful one's ages suppress the other's count
	// entirely, because the credit falls back on the count only when it has no
	// ages at all. That silently credited a six-replica variant with nothing.
	startingByRole := make(map[string][]startingReplicas, len(variants))
	for _, vc := range variants {
		perReplica[vc.VariantName] = vc.PerReplicaCapacity
		role := canonicalRole(vc.Role)
		roleOf[vc.VariantName] = role
		readyByRole[role] += vc.ReplicaCount
		if vc.PendingReplicas > 0 || len(vc.PendingAges) > 0 {
			startingByRole[role] = append(startingByRole[role],
				startingReplicas{count: vc.PendingReplicas, ages: vc.PendingAges})
		}
	}
	// The per-role anticipated supply the hold cap is measured against, from
	// the one place that defines it: the engine reads the same figure through
	// the same helper, so the cap and the RC it exists to zero cannot drift.
	anticipated := aggregation.AggregateByRole(variants)

	// tokens per unit of arrival rate, per role: P / mu for each replica that
	// can price it -- and whether any of them may order (own window, enough
	// readings).
	costs := make(map[string][]float64)
	mus := make(map[string][]float64)
	// The smallest replica in the role, for the release bound below. cost*mu
	// cannot serve: cost is median(P/mu) and mu is median(mu), independent
	// order statistics that need not come from the same replica, so on a role
	// of unequal variants their product is no variant's P at all.
	smallestP := make(map[string]float64)
	// The same, from the replicas whose reading is a neighbouring bucket's;
	// taken only for a role none of whose replicas reads its own.
	// lineBorrowed records, per role, that what was routed as borrowed came
	// from a borrowed ITL LINE rather than a neighbouring shape bucket -- so
	// the hold can name which, instead of reporting a bucket borrow for
	// something else entirely.
	lineBorrowed := make(map[string]bool)
	borrowedCosts := make(map[string][]float64)
	borrowedMus := make(map[string][]float64)
	mayOrder := make(map[string]bool)
	borrowedOnly := make(map[string]bool)
	for _, rc := range replicas {
		if rc.FromWarmPool || !priceable(rc.SaturatedThroughput) {
			continue
		}
		p := perReplica[rc.VariantName]
		if !priceable(p) {
			continue
		}
		role := roleOf[rc.VariantName]
		if _, seen := borrowedOnly[role]; !seen {
			borrowedOnly[role] = true
		}
		// A DERIVED figure is this role's own, whatever the measured window
		// beside it is doing. It was priced for the shape arriving now from
		// this variant's own ITL(k), so it is neither borrowed from another
		// shape's bucket nor a count of samples -- the two fields below
		// describe the measured window that was not used.
		// A figure derived from a BORROWED line is routed here too, and that
		// is the whole implementation of "may hold the fleet, may not grow
		// it": this branch keeps borrowedOnly[role] true, which applies the
		// cap below and names the reason, and it continues before mayOrder is
		// ever set.
		//
		// An earlier version of this instead added !LineBorrowed to the
		// mayOrder disjunction, which did nothing at all. The analyzer stamps
		// MinDerivedThroughputSamples -- equal to MinThroughputSamplesToOrder
		// by construction -- onto every derived figure, so the SAMPLE half of
		// that disjunction re-admitted exactly what the derived half had just
		// excluded. Gating a decision in one of two disjuncts gates nothing;
		// the reading has to be routed, not annotated.
		if borrowedReading(rc) {
			borrowedCosts[role] = append(borrowedCosts[role], p/rc.SaturatedThroughput)
			borrowedMus[role] = append(borrowedMus[role], rc.SaturatedThroughput)
			if rc.SaturatedThroughputLineBorrowed {
				lineBorrowed[role] = true
			}
			continue
		}
		costs[role] = append(costs[role], p/rc.SaturatedThroughput)
		mus[role] = append(mus[role], rc.SaturatedThroughput)
		if cur, seen := smallestP[role]; !seen || p < cur {
			smallestP[role] = p
		}
		borrowedOnly[role] = false
		// Ordering on a derived figure does not wait for samples, and does
		// not wait out a shape change either: it is priced for the shape
		// that changed TO, which is the whole reason the hold exists and
		// the reason it no longer has to.
		//
		// Nor does it wait on the GPS check. That check was built as a gate
		// here and measured as one in run T: it withheld ordering 28 times,
		// all of them in the phase-1 ramp, because the k it reads carries no
		// window while the rate it compares against is averaged over a minute.
		// It is now a diagnostic only -- saturation.noteLineMismatch says
		// why -- so this file is back to one disjunction.
		// Reaching here means the reading is this variant's own: a borrowed
		// line, like a borrowed bucket, took the branch above and continued.
		if rc.SaturatedThroughputDerived ||
			(rc.SaturatedThroughputSamples >= MinThroughputSamplesToOrder && !staleShape) {
			mayOrder[role] = true
		}
	}
	for role, c := range borrowedCosts {
		if len(costs[role]) == 0 {
			costs[role] = c
			mus[role] = borrowedMus[role]
		}
	}
	if len(costs) == 0 {
		return out
	}

	out.ByRole = make(map[string]float64, len(costs))
	out.Terms = make(map[string]Term, len(costs))
	for role, c := range costs {
		cost := median(c)
		mu := median(mus[role])
		rate := lambda
		var observed, b float64
		if drainSeconds > 0 {
			observed = max(backlog[role], 0)
			// Nothing to clear before capacity arrives, so the window the
			// backlog must clear in is at least as long as a replica takes to
			// start. drainSeconds is 60 s and the measured start is 67-82 s, so
			// dividing arrivals-over-T by drain alone priced the arrival rate at
			// 1 + T/drain -- 2.17x lambda at run T's figures, every cycle the
			// fleet was behind.
			horizon := drainSeconds
			if T := startSeconds[role]; T > horizon {
				horizon = T
			}
			b = backlogAtLanding(observed, lambda, mu, startSeconds[role],
				readyByRole[role], startingByRole[role])
			rate += b / horizon
		}
		floor := rate * cost
		// The two are kept APART. Backlog is what was measured; ProjectedBacklog
		// is what the floor priced. Carrying the projection in both left the
		// observed queue unrecoverable from the log -- and its meaning silently
		// changed relative to every earlier run the commit messages reason from.
		term := Term{Mu: mu, PerReplica: cost * mu, Backlog: observed,
			ProjectedBacklog: b, Replicas: rate / mu}
		// A single reading may order while the SCHEDULER holds a real queue.
		//
		// Nothing here withholds the figure while a replica is starting, and
		// an earlier version that did was measured inert: through every cycle
		// of a ramp the deployment has more replicas than are Ready, which is
		// what a ramp is, so the fleet stayed at two while the queue tripled.
		// The engine already does that job properly one layer up, where
		// RC = max(0, TotalDemand/scaleUp - TotalAnticipatedSupply) subtracts
		// the supply on its way and so cannot re-order it.
		//
		// What makes the climb safe is that the figure does not move:
		// (lambda + backlog/drain) / mu is fixed by the load and the queue,
		// not by the fleet, so repeated firings converge on it rather than
		// ratchet past it, and the rule stops firing when the queue drains.
		// See "A single reading may order behind a standing queue" in
		// ../../../docs/developer-guide/analyzer-evidence.md.
		//
		// A THIN window only. mayOrder is false for three different reasons
		// and this releases one of them: staleShape means every reading on
		// record was taken under a shape the fleet has left, and borrowedOnly
		// means the reading belongs to a neighbouring output-length bucket and
		// is wrong in a known direction. Ordering on either is the shape-swap
		// flap the two sections above this one in the guide describe; a queue
		// does not make a reading for the wrong shape right.
		// The queue must be worth more than a second of arrivals AND more
		// than one replica-second of service. Against lambda alone the test
		// degenerates as lambda falls: at 0.1 req/s a single stray request is
		// ten seconds of arrivals and would release the hold, which is jitter,
		// not a standing queue. mu is the other natural scale and needs no
		// constant.
		thinOwnWindow := !staleShape && !borrowedOnly[role]
		if !mayOrder[role] && thinOwnWindow && schedulerQueued > max(lambda, mu) && scaleUpThreshold > 0 {
			// Bounded at ONE replica beyond the fleet already anticipated.
			// Releasing the hold outright would also skip the cap below, and
			// that cap is the only thing bounding an order taken from a
			// window this thin -- the case the file header documents as
			// under-reading mu by about half (3.67 against a true 7.13). The
			// floor is rate x P/mu, so a halved mu doubles the order in one
			// shot. A standing queue justifies asking for a replica; it does
			// not make the reading accurate.
			//
			// scaleUpThreshold x (anticipated + the role's SMALLEST replica)
			// leaves the engine's RC = D / scaleUpThreshold - anticipated at
			// one replica, the same construction the hold below uses for zero.
			//
			// The smallest, not cost*mu: those are medians of P/mu and of mu
			// taken independently, so on a role whose variants differ they
			// multiply to no real replica -- 873,610 tokens where the two
			// variants are 930,000 and 600,000, which is 1.46 of the smaller.
			// The smallest P is exact when a role has one variant and can
			// never exceed one replica of any variant when it has several.
			mayOrder[role] = true
			term.OrderedBehindQueue = true
			// As many replicas as the QUEUE justifies, not exactly one.
			//
			// One was the safe step while nothing said how big a step should
			// be. The queue does: at mu requests per second per replica, a
			// queue of Q needs Q/(mu x drainSeconds) replicas to clear inside
			// the drain window. Measured, degrades to one when the queue is
			// small, and still bounded below by the floor's own figure because
			// this stays a min.
			//
			// Run T is why. Its log carried orderedBehindQueue=true for five
			// consecutive cycles with replicasImplied of 4.15, 4.35, 5.41,
			// 5.84 and 8.69 -- the floor knew it needed four to nine replicas
			// from the first cycle, and this cap granted one each time while
			// the queue climbed to 191. At mu about 1.0 and a 60 s drain, the
			// rule below permits THREE on that cycle (191/60 floored), which
			// reaches the needed fleet in two cycles instead of five.
			term.QueueJustifiedReplicas = queueJustifiedReplicas(schedulerQueued, mu, drainSeconds)
			// Against READY supply, not anticipated. Built on anticipated, the
			// cap became a per-cycle INCREMENT rather than a target: the engine
			// computes RC = step/scaleUp - anticipated, which cancels to exactly
			// k replicas however many are already in flight, and schedulerQueued
			// is not reduced by the ones ordered last cycle because they are not
			// Ready yet. So the same unserved requests justified k again every
			// cycle while k itself grew.
			//
			// Measured, not argued: run U ordered 1, 1, 1, 2, 1, 2 and reached
			// its ceiling of nine in 75 seconds against a steady-state need of
			// six. With ready supply the in-flight orders subtract, and the
			// grant is what the queue justifies MINUS what is already coming.
			if step := scaleUpThreshold * (nonNegativeSupply(anticipated[role].TotalSupply) +
				term.QueueJustifiedReplicas*smallestP[role]); floor > step {
				floor = step
			}
		}
		if !mayOrder[role] && scaleUpThreshold > 0 {
			// A hold, not an order, on either. Letting a single reading
			// order one replica was tried twice and dropped: against the
			// anticipated supply it ordered a replica at a phase switch
			// whose predecessor was still starting, and against the running
			// supply it was a ratchet, because nothing remembered that the
			// reading had already ordered -- once the ordered replica
			// reported, the same reading ordered the next, up to the full
			// figure one start at a time. The hold's own cost is bounded:
			// the second counted reading lands a window after the first, and
			// occupancy orders in the meantime. See "Two readings, not one,
			// before a reading may order" in
			// docs/developer-guide/analyzer-evidence.md.
			//
			// The anticipated supply is floored at zero, and a non-finite
			// supply is read as zero, before it becomes a cap. It is
			// (ReplicaCount + PendingReplicas) x P summed over the role, and a
			// negative product would make the cap negative -- at which point
			// every real floor is above it and this branch publishes the
			// negative, inverting a package whose whole contract is that it
			// only ever raises demand. A NaN is worse than negative: the
			// builtin max returns NaN for a NaN operand in either position, so
			// `max(x, 0)` does NOT clamp one, and a NaN cap disables the
			// comparison below entirely rather than binding it. Today every
			// producer clamps PendingReplicas (variantmeta/discovery.go does
			// it explicitly, and logs), so the guard costs nothing; it is
			// here because this package cannot see that clamp, and the cost
			// path above already skips a variant whose P is <= 0 while this
			// one reads P again over every variant of the role.
			hold := scaleUpThreshold * nonNegativeSupply(anticipated[role].TotalAnticipatedSupply)
			if floor > hold {
				floor = hold
				term.Held = true
				term.HeldWhy = "single-sample"
				if borrowedOnly[role] {
					term.HeldWhy = "borrowed"
					if lineBorrowed[role] {
						term.HeldWhy = "borrowed-line"
					}
				}
				if staleShape {
					// Both reasons can hold at once, and the cap is the same
					// number either way -- but what an operator has to fix is
					// not. Overwriting made every borrow invisible in a cycle
					// that also had a stale shape, which is most cycles while
					// a fleet is still ramping into a new shape, so the one
					// reason that gets named is the one that goes away on its
					// own. The composed value keeps the borrow visible; the
					// single-sample case still reads as "shape-change" alone,
					// because a stale shape is the reason that bounds it.
					if borrowedOnly[role] {
						term.HeldWhy += "+shape-change"
					} else {
						term.HeldWhy = "shape-change"
					}
				}
			}
		}
		out.ByRole[role] = floor
		out.Terms[role] = term
	}
	return out
}

// backlogAtLanding projects a role's queue forward to the moment ordered
// capacity becomes Ready.
//
// Arrivals keep coming while a replica starts -- at 6 req/s over the 67-82 s run
// T measured, about 420 requests -- while only the replicas already serving, plus
// whatever is part-way through starting, drain them. Pricing the queue as it
// stands at the moment of the decision sizes the fleet for a backlog it will have
// outgrown by the time it arrives.
//
// The pending term is an APPROXIMATION and the reason is worth stating: a replica
// ordered at some point in the last T seconds is, in expectation, half way
// through starting, so it drains for about T/2 of the window. The exact form
// needs each pending Pod's age, and there are none to be had -- PendingReplicas
// is a scale-target count, and a pending Pod reports no metrics, so no per-Pod
// row exists to carry an age.
//
// Crediting nothing would be worse, not safer. With no credit the projection
// re-counts the same arrivals on every cycle while replicas start, the engine
// subtracts anticipated supply from a demand inflated that way, and the fleet
// ratchets -- which is how run T reached nine replicas and could not come back.
//
// The result may be BELOW the observed backlog, and that is correct: a fleet that
// will have drained the queue before new capacity lands needs no capacity for it.
// Returns the observed backlog unchanged when no start time is known or mu is
// unusable, which is the behaviour before this existed.
func backlogAtLanding(backlog, lambda, mu, startSeconds float64, ready int,
	starting []startingReplicas) float64 {
	if !(startSeconds > 0) || !(mu > 0) {
		return backlog
	}
	arrived := lambda * startSeconds
	var credit float64
	for _, s := range starting {
		credit += startingCredit(startSeconds, s.count, s.ages)
	}
	served := mu * (float64(ready)*startSeconds + credit)
	projected := backlog + arrived - served
	if !(projected > 0) {
		return 0
	}
	return projected
}

// startingCredit is how many replica-seconds of draining the STARTING replicas
// contribute within the window, in seconds of one replica's service.
//
// With ages, exactly: a replica that is `age` into a start of `startSeconds` has
// `startSeconds - age` left, and is therefore Ready for the remainder of the
// window -- so it drains for `startSeconds - (startSeconds - age)` = `age`
// seconds of it. One ordered a second ago contributes a second; one 60 s into a
// 70 s start contributes 60.
//
// Without them, the count times half the window: a replica ordered at some point
// in the last `startSeconds` is on average half way through. That is right only
// when the ages are spread uniformly, and the queue-justified step orders in
// batches, which is exactly when they are not -- so the ages are used wherever
// they can be had, and this is the fallback rather than the rule.
//
// An age beyond the window contributes the whole window and no more: a replica
// that has been starting longer than a start takes is either about to be Ready
// or is not coming, and neither earns extra credit.
func startingCredit(startSeconds float64, pending int, ages []float64) float64 {
	if len(ages) == 0 {
		if pending <= 0 {
			return 0
		}
		return float64(pending) * startSeconds / 2
	}
	var credit float64
	for _, age := range ages {
		if !(age > 0) {
			continue
		}
		// Past twice a start, it is not starting. A Pod stuck on an image pull,
		// a crash loop or unschedulable on GPU quota is "not Ready" for as long
		// as it exists, and clamping its age to the window credited it exactly
		// as much as a fully Ready replica -- permanently, since it never
		// becomes Ready. The floor then stops asking for the capacity that would
		// clear the queue because it has been told phantom replicas are about to
		// serve. Between one and two starts is still plausibly a slow start and
		// earns the window.
		if age > staleStartFactor*startSeconds {
			continue
		}
		credit += min(age, startSeconds)
	}
	// Never more than the replicas there are. ReplicaCount comes from the
	// metrics rows and the ages from the Pod informer, two caches with
	// independent lag, so a replica that is Ready and scraped can still read
	// Ready=false here and be counted in both terms -- crediting it twice.
	//
	// A clamp rather than a proportional rescale, and one that does not skip
	// pending == 0. The rescale smeared: with one pending replica and ages
	// 70, 70, 1 it credited 47, which is neither of the two answers that could
	// be true. And pending reaches 0 whenever stale rows outnumber the target
	// during a scale-down, where the ages can still list Pods genuinely
	// starting -- the old guard let those through uncapped and under-ordered by
	// two or three replicas on a flap.
	if maxCredit := float64(pending) * startSeconds; credit > maxCredit {
		credit = maxCredit
	}
	return credit
}

// startingReplicas is one variant's starting replicas: how many there are, and
// their ages where those could be read. The two travel together because the
// credit falls back per VARIANT, not per role.
type startingReplicas struct {
	count int
	ages  []float64
}

// staleStartFactor is how many starts a Pod may have been starting for before it
// stops counting as one.
const staleStartFactor = 2.0

// queueJustifiedReplicas is how many replicas a standing queue of q requests is
// worth: what it takes to clear it within the drain window at mu requests per
// second per replica.
//
// Floored at ONE, never zero, because the caller has already decided the queue
// is standing (it is worth more than a second of arrivals and more than a
// replica-second of service) -- so the answer to "how many does it justify"
// cannot be none. The floor also keeps this from ever being more conservative
// than the single replica it replaces.
//
// Not rounded up beyond that. A queue worth 1.2 replicas justifies one, not two:
// the next cycle sees what the first one did and asks again, and rounding up
// every cycle of a long ramp is how a fleet overshoots.
//
// mu <= 0 or drainSeconds <= 0 yields one, the previous behaviour: without a
// service rate or a window there is no arithmetic to be had, and a queue is
// still standing.
func queueJustifiedReplicas(q, mu, drainSeconds float64) float64 {
	if !(q > 0) || !(mu > 0) || !(drainSeconds > 0) {
		return 1
	}
	n := math.Floor(q / (mu * drainSeconds))
	if !(n > 1) {
		return 1
	}
	return n
}

// median is the median of values, averaging the central pair on an even
// count: every value here is a learned per-replica figure, none is suspect,
// and the midpoint is the better estimate -- the same convention as the saturation
// analyzer's median() for capacities.
// priceable reports whether a measured rate or per-replica capacity may be
// priced into the floor.
//
// Written as `x > 0` rather than `!(x <= 0)` on purpose. Every comparison
// against NaN is false, so `x <= 0` ADMITS a NaN, and a NaN floor then defeats
// both caps below -- `floor > step` and `floor > hold` are both false, so the
// role would publish a NaN with Held false, the exact inverse of this
// package's contract. The ingest path that produces these readings already
// uses this form (saturation.recordSaturatedThroughput).
//
// +Inf is excluded explicitly because it passes `x > 0`: a replica of infinite
// throughput prices capacity at zero cost, which is not a reading, and it also
// makes cost*mu a NaN (0 x +Inf) in the Term this package publishes.
func priceable(x float64) bool {
	return x > 0 && !math.IsInf(x, 1)
}

// nonNegativeSupply floors an anticipated supply at zero for use as a cap,
// reading a non-finite supply -- NaN or +Inf -- as zero.
//
// It exists because the builtin max clamps neither: max(NaN, 0) is NaN and
// max(+Inf, 0) is +Inf, and both DISABLE the comparison this value caps rather
// than binding it, because `floor > NaN` and `floor > +Inf` are equally false.
//
// aggregation.perReplica now keeps the sums finite at their source, so this is
// the second line of defence rather than the first. It is kept because the cap
// it feeds is this package's only guarantee, and because a caller may hand us
// an anticipated supply we did not aggregate ourselves.
func nonNegativeSupply(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 1) || x < 0 {
		return 0
	}
	return x
}

func median(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, values)
	slices.Sort(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// canonicalRole reads an empty role as domain.RoleBoth, as aggregation does.
func canonicalRole(role string) string {
	if role == "" {
		return domain.RoleBoth
	}
	return role
}
