# The throughput floor

This page covers why the saturation analyzer floors its demand at what the
offered load requires, what the floor is and is not invariant to, which claims
are measured and which assumed, and why the Little's-law floor it replaced was
retired.

Code: the model and its arithmetic are in `internal/signals/floor`. The window
of saturated readings, the borrow between buckets and the application to the
measured demand are in `internal/engines/analyzers/saturation/throughput_floor.go`.

## The problem

Saturation's demand is **occupancy**: resident KV plus the waiting-queue
footprint. Both belong to the *fleet*, not the *load*, and both shrink as
capacity grows: resident KV because residence time falls, the queue because it
drains. So the signal that sizes the fleet depends on the fleet. Once the fleet
is big enough, the signal decays toward zero and takes the replica count with
it.

Measured on an H100 at 14 QPS (run `biran-20260822-021153-340`): demand fell from
7.5M tokens to 152k as ten replicas drained a 910-deep queue, and the target
followed from the ceiling down to one, mid-load. And on an H200 at 6 QPS (run
`biran-20260915-102548-571`): six replicas measured 100k tokens of occupancy
for the whole model, the target went to one, the one replica saturated within
a minute, and the loop ran twice in 38 minutes.

## The floor

What a replica can do does not move with the fleet: its completion rate when
saturated. With the queue over `queueLengthThreshold`, the engine completes
requests as fast as it can for this shape. So the P1 moment that records k2
(occupancy under a saturated queue) also records the replica's completion rate,
`mu`, under the same history key. The fleet needs `lambda / mu` replicas to
keep up. In the analyzer's (demand, per-replica capacity) contract that is a
demand of

```
floor_role = ((lambda + B_role / T) / mu_role) x P_role    tokens
demand     = max(resident KV, floor)                        never lowers, only raises
```

It is applied per role on a P/D fleet (every request passes through both roles,
so each must keep up with all of it), and on the total otherwise.

- `lambda` is the scheduler's arrival rate, or the generating replicas'
  completion rate when the scheduler reports none.
- `B` is the role's backlog: the requests waiting in its engines' queues plus
  the scheduler's. `T` is the drain target (`BacklogDrainSeconds`, 60). The
  fleet asked for clears the backlog in about `T` while keeping up with
  `lambda`. Every engine queue counts, whichever replica it stands on, and
  the scheduler's counts in full. `B` is requests to drain at `mu`, and an
  idle sibling's KV room drains none of them: in this model its `mu` is
  already spent on its share of `lambda`. Leaving out the share such a
  sibling had room for read three full replicas queued 60 beside a fourth
  never routed to at 5.88 replicas against the 9.41 the fleet needs with
  perfect routing (`TestUnevenRoutingShortFleetKeepsItsBacklog`).

  A request waiting in one engine's queue is served by that engine only, so
  each replica's own queue counts for at most what the replica drains itself
  in the drain horizon, `min(queue, mu x T)` (`localQueueBacklog`; `T` is
  `floor.DrainHorizon`, the drain target or one replica's start time where
  longer). A queued replica is then worth at most one replica, and the floor
  asks for `lambda / mu` plus the queued replicas. Charged in full, a queue
  of 1000 pinned on one of four replicas (`mu` 1, `lambda` 2) ordered 21.96
  replicas where 3.53 can help, and three replicas each queued 600 at
  `lambda` 5 ordered 41.2 against the 9.41 of use
  (`TestUnevenRoutingAPinnedQueueIsPricedAtWhatItsReplicaDrains`). A queue
  each replica drains inside `T` is unchanged, so the short fleet above
  still reads 9.41. The scheduler's queue is not capped: the router can send
  it to any replica, added ones included. Growth (below) is judged on the
  uncapped queues, logged as `queuedRequests`.

  The cap holds only while the queue stands. A queue that grows is arrivals
  above what the fleet serves, and added replicas can take those arrivals.
  `lambda` may not count them yet: the router's rate is a one-minute rate
  that reads a step late, and with no router rate `lambda` falls back to
  completions, which on a full fleet are `n x mu` whatever arrives. So while
  the role's backlog grows (`backlogGrew`) every engine queue is charged in
  full. With the cap on growing queues too, four full replicas (`mu` 1) read
  18.82 replicas 30 s into a step from 4 to 20 req/s, under the 23.53 the
  arrivals need. With no router rate, a true 12 req/s read 9.41 for two
  minutes against 14.12. Charged in full, both read 23.53
  (`TestUnevenRoutingAGrowingQueueIsNotCappedWhileLambdaLags`,
  `TestUnevenRoutingAGrowingQueueIsNotCappedWithNoRouterRate`). Once the
  queues have stood a minute the cap is back: four replicas queued 100 at
  `lambda` 2 read 7.06, and four queued 600 at `lambda` 8 read 14.12
  (`TestUnevenRoutingAStandingQueueIsCappedAtWhatItsReplicaDrains`).

  `backlogGrew` compares with the backlog one minute ago, so it stays on
  for up to a minute after the growth stops, and the queues are charged in
  full that long. That over-orders for a minute. We accept it: over-ordering
  is the cheap direction, and a shorter memory would let jitter flip the
  charge from cycle to cycle.
- For a role with a `mu`, the queues enter the model this way, and their
  residency charge (every queued request at its full KV footprint, as if all
  were resident at once) comes out -- but only where the floor's figure can
  carry the queue: the term may order (`mayOrder`) and is not held, and in
  proportion to how much of the resident KV its arrival half `lambda x P / mu`
  accounts for -- none at `FloorLagFraction` (0.5) or under, all of it at
  `FloorTrustFraction` (0.8) or over, linear between (`floorTrust`), so
  demand moves continuously with `lambda`; a hard switch took one replica
  499 446 -> 260 123 between 2.95 and 3.0 req/s. Elsewhere
  `demand = max(measured, floor)`. What is
  resident arrived within one residence time, so the resident KV implies an
  arrival rate of about `resident x mu / P`; a `lambda` well under it is a
  `[1m]` rate still reading the minute before a load step. Measured on the
  two-namespace rerun (2026-10-23, 11:20:09): `lambda` 0.8 req/s with 160
  queued cut 499 446 to the 260 123 resident, and the analyzer's `rc` fell
  312 936 -> 47 265 in one cycle.

  Nor while the role's backlog has grown over the last minute
  (`noteBacklogGrowth`, logged as `backlogGrew`): past
  `max(BacklogGrowthMinRequests, BacklogGrowthFraction x B then)` (2
  requests, 10 %) above the backlog `BacklogGrowthLookback` (1m) ago, or the
  oldest on record before there is one that old. 10 % sits above a standing
  queue's minute-to-minute jitter: at 5 %, four queues alternating 100 / 106
  lifted the engine-queue cap every other minute and flipped the order
  7.06 <-> 10.67 (`TestUnevenRoutingAJitteringStandingQueueStaysCapped`).
  A queue growing slower than that is read as standing. Not against the previous
  cycle: the engine queue is a one-minute max read every 15 s, and a fleet
  draining steadily reads a request or two either way. Counted strictly, a
  queue reading 160, 161, 160, 161 at 4.75 req/s moved demand
  326 185 <-> 500 942 every cycle
  (`TestThroughputFloor_AQueueJitterDoesNotFlipTheSwap`). The lag test cannot see a
  step above fleet capacity: on a saturated fleet the resident KV sits at
  about `P` per replica whatever the arrival rate, so the ratio reads
  `lambda / (n x mu)` and reaches full trust once a lagging rate reads 80 %
  of capacity, even with the true rate at 150 %. A queue that grows is
  arrivals above what the fleet drains. In review a step from 3 to 9 req/s on
  one replica of `mu` 6 read 319 763 before it and 280 704 / 354 736 under
  the occupancy (387 263 / 454 573) that held the queue for the cycles it
  tripled; it now keeps the occupancy demand
  (`TestThroughputFloor_AGrowingQueueKeepsTheOccupancyDemand`). The swap is
  made on a queue that has stood a cycle, so its first cycle is priced at
  `max(measured, floor)`, which only over-orders.
- A role with no `mu` keeps the residency charge, except prefill's share of the
  scheduler queue, which is dropped. A prefill replica holds a prompt for its
  prefill time plus the hand-off. Only a saturated decode makes it hold longer,
  and more prefill replicas do not fix that.

Properties, each with a spec in `throughput_floor_test.go`:

- **Invariant to fleet size.** `mu` is a per-replica constant, so the floor is
  the same at one replica and at six.
- **A max over the window, not a mean.** A saturated completion rate
  under-reads its capacity while the replica is full -- in the first minute
  after it fills, the requests completing are the few admitted first; under
  KV pressure preemption drops it further. One run's readings for one
  replica were 5.4, 3.3 and 3.5 req/s while it was demonstrably completing
  5.4. The floor divides by `mu`, so a mean of under-reads would order
  replicas that are not needed.

  It does NOT only under-read: in the last
  minute of an episode that ends by a replica landing, the queue gate (a
  one-minute max) stays up while the rate is fresh, and the fresh rate is
  the batch draining -- sequences admitted together finish together, and no
  new ones come. Measured on the second cold pass of 2026-09-19
  (`guidellm-1789812977-xzf9ay_1`), from the pod's own counters: the replica
  sustained ~5.0 req/s full (token throughput on a plateau), read 6.67 --
  200 completions in 30 s, token throughput below the plateau -- on the
  last saturated cycle as the second replica took the arrivals, and the
  window carried 6.67 for the rest of the phase.

  The fleet holds `n`
  replicas while `lambda > 0.7 x (n - 1) x mu`, so a figure too high
  releases where the true one holds, in the band between the two: for
  6.67 against 5.0, 3.5-4.7 req/s from two to one, and this trace's
  phase-1 rate (5.4-6.2) sits where both hold two.

  The same pass's phase
  switch shows it biting: the `xxlong` window's max, 3.27, was a 30 s burst
  on a still-full replica (token throughput under the plateau; the
  sustained figure is ~2.75), and at 5.8-6.9 req/s -- where phase 2 ran --
  3.27 releases the fourth replica the true figure holds; the fourth left
  at +1584 s on it, two minutes before the true figure would have let it
  go.

  The max window has always taken these readings and nothing in it
  lowers one. A `mu` priced from the generation-token rate over the
  bucket's output length, which does not burst on a drain, is the
  follow-up; a reading gated on an instantaneous queue would catch the
  first case and not the second, where the burst was read with the queue
  up. The k2 history beside it is a mean over the same rows and
  takes a re-read row as many times as it repeats; that errs toward a lower
  k2, the safe direction, and is left as it is.
- **An order as well as a hold, and the order comes first.** The first
  version was capped at the fleet's own size, so that scale-up stayed with
  occupancy and the queues. Measured on three runs of the shape-swap trace,
  that put the second replica's order at +69-78 s after load start, from
  occupancy crossing k1 -- and the lone replica, at 94% of its saturated rate
  from the first cycle, tipped into preemption at +91 s on two of them and
  +144 s on the third. With a 60-100 s start the replica landed after the
  tip, and the queue that built in between was sized as five to seven extra
  replicas. `lambda / mu = 0.94` through the engine's scale-up headroom orders
  that replica in the first cycle `lambda` is measured.

  There is no cap; the
  bound is the formula, which does not move as replicas are added. What a
  `mu` that under-read costs is bounded by the under-read, and the window's
  max corrects it upward at the next saturation -- while the reading stays
  in the window: ten counted readings, a rate window apart, is ten minutes
  of one saturated replica, after which the earliest is evicted, and on one
  run the max fell 3.67 to 3.47 that way before the spacing existed.
- **Two readings may hold but not order**, and for those the cap at
  `scaleUp x anticipated supply` stays (`heldAtFleet`, `heldWhy` on the log
  line). A `mu` *borrowed* from a neighbouring bucket is wrong in a known
  direction and, from a longer shape, would over-order. A window with a
  *single* reading is the first window's under-read (3.67 against a true
  7.13 on the run), and an order on it over-provisions in a way that
  removes the saturation which would have recorded the second, corrected
  reading (`MinThroughputSamplesToOrder`, 2).

  Letting a single reading
  order one replica was tried, twice, and dropped: measured against the
  anticipated supply it ordered a fourth replica at a phase switch whose
  third was still starting; measured against the running supply it was a
  ratchet -- nothing remembered that the reading had already ordered, so
  once the ordered replica reported, the same reading ordered the next,
  one start at a time, up to the full figure. Across four passes the one
  replica bought a single cycle over the plain hold (a third replica 30 s
  ahead of occupancy, on the under-read that then ran unneeded for 35
  minutes).

  Two readings are two *rate windows*. The completion
  rate is `rate(...[1m])` evaluated afresh every 15 s cycle, so the next
  cycle reads mostly the same window -- at 30 s scrapes exactly the same
  two samples, the same value to the digit -- and a reading counts as a
  sample of its own only when it lands a rate window
  (`ThroughputSampleSpacing`, one minute, held equal to the collector's
  `RequestRateWindow` by a test) after the last one that counted and
  differs from it. Counted per cycle, the guard was dead at that scrape
  interval: the second cycle always re-read the first pair, and every
  saturation ordered on the first window's under-read, unbounded. With the
  spacing, the second reading is a minute away, and a fleet held at its
  size for that minute with its queues priced nowhere (the floor takes the
  residency charge out for a role with a `mu`) lands the cold ramp's next
  replica 15-45 s after the dead guard would have on the three measured
  cold passes -- occupancy orders it in the meantime, or the second
  counted reading does.

  A reading inside the
  spacing is *folded* into the last sample, which becomes the max of its
  window -- the max the window kept when every cycle was added, no more;
  dropping in-spacing readings instead lost the highest reading of an
  episode exactly when no next window would come, and on two replayed
  passes left `mu` 10-12 % low for the rest of the pass, on one of them
  ordering a fourth replica the run never needed. (What that highest
  reading is worth is the drain question above; the fold neither creates
  nor cures it.) Folding also makes two replicas saturating in one cycle
  one moment at the higher of the two, whichever row the collector's map
  yields first.

  Value
  equality is not the test -- the rate is an integer count of completions
  over the scrape interval (every saturated rate logged on four passes is
  N/30 -- 103/30 = 3.43, 110/30 = 3.67, 165/30 = 5.5 -- but one, a freshly
  started replica's first window, where the counter's zero point makes the
  rate an extrapolation), so two windows agree to the digit a few percent
  of the time; a reading equal to the last one *given* at the boundary is
  the same pair read again across it, or a repeat, and only touches the
  window, which costs a cycle or two.

  Measured on the shape-swap trace's first cold pass
  of 2026-09-19 (`guidellm-1789803750-vl4r9g_1`): one decode replica's 3.43 stood on four consecutive cycles,
  two scrape pairs, and let the floor order on it. What followed -- a third
  decode replica held for the remaining 35 minutes at `lambda / mu` = 1.75
  against a true ~5.4 -- was the under-read itself: occupancy would have
  ordered the third replica 30 s later, and the floor, uncapped at three
  once it exists, held it either way. The spacing fixes what the guard
  counts; an under-read that no later saturation corrects is still open.
- **A backlog is throughput, not residency.** 350 queued requests at 6 req/s
  arriving are 58 s of arrivals; two replicas at 5.4 req/s each clear them in
  about two minutes and three in one. Charged as resident KV they were five
  replicas, each arriving after the queue was gone. `B / T` prices them as
  what they are.
- **Ready pods and own replicas only.** The collector leaves a not-Ready pod's
  completion rate in place (only its timing is dropped), and a bridge's rate is
  the pool's, not the variant's.
- **Silent for a role never seen saturated.** Prefill, in practice: its queue
  is rarely the one that fills. No opinion rather than a guess.
- **A prefill saturation records only while decode is not saturated, and
  prefill is held meanwhile.** A prefill request is done when decode admits
  it and pulls its KV, so with a decode replica over its queue threshold what
  prefill shows -- the KV of finished prompts held until the pull, arrivals
  the scheduler's flow control releases in bursts, a completion rate that is
  decode's admission rate -- is metered by decode: decode's saturation seen
  from upstream.

  Measured on the shape-swap benchmark's cold pass
  (2026-09-18): the one prefill replica read queue 30 and 357 800 resident
  tokens for the four cycles both decode replicas were over the threshold
  (+220..+265 s after load start), was priced at k2 = 357 800 against a k1
  of 919 859 and `mu` = 5.57 req/s, and its own occupancy read 100 % of that
  k2 and ordered a second prefill replica on the spot. The `mu`, under the
  6 req/s offered, then held both (`lambda / mu` 1.08 at the median, 0.98
  to 1.17 between the 10th and 90th percentile of the cycles) for the
  remaining 33 minutes while their resident KV read at most 54k -- and
  nothing could correct either figure, since a prefill fleet of two never
  saturates again. The correlation is what was measured; the path is not
  settled (prefill's KV sat at 31 % of its cache on those rows, so it was
  not block-starved).

  The analyzer now leaves such a reading unrecorded
  (`P1-obs-downstream` on the `k2-decision` line; k2 falls through to
  history or k1, and no `mu` is written), and clamps prefill's demand for
  the cycle into the band where the engine neither orders nor releases,
  `[scaleDown x supply, scaleUp x anticipated supply]`
  (`prefill-demand-held`): against a small prefill k2 learned on some
  earlier, genuine saturation the held KV alone would read as several
  replicas, and against k1 on a fleet of two it reads as a release (537 800
  on 1 839 718 is 29 %).

  What the floor of the band buys is not a re-order
  avoided -- a replica released mid-episode would stay released, prefill
  reads near zero after one -- but the burst that ends an episode: the
  scheduler's flow control releases what it held in one go (124 requests,
  744k prompt tokens against a 919k k1 on the measured run), and it lands
  on prefill first.

  A genuine prefill order is deferred for as long as any
  decode replica's one-minute peak reads full and queued, plus the memory
  below: 195 s and two decode starts replayed on the measured cold pass,
  135 s and one on the warm, unbounded while decode is capped and cannot
  grow. A prefill bottleneck reduces decode's arrivals, so the two saturate at once only when decode
  is short at prefill's completion rate.

  "Decode saturated" is a decode
  replica full AND queued -- resident KV at its k1, queue at the
  threshold -- not the queue alone: vLLM counts a request waiting for its
  remote KV in `num_requests_waiting`, so a fleet whose transfer keeps as
  many in flight as the threshold (a large model over a slow link) would
  read saturated every cycle on the queue alone, and prefill would then
  never record and, held, never be ordered. Full is decode unable to
  allocate the blocks a pull needs, which the transfer pipeline does not
  produce.

  Any one such replica gates prefill's learning, whatever its siblings do:
  KV prefilled for it waits on it, so prefill's reading is its back-pressure,
  and a prefill k2 learned from it is never corrected (with decode-0 at 26 %
  and decode-1 full with 81 waiting, prefill would record 357 800 against a
  k1 of 919 449). Only the hold on prefill's DEMAND relaxes, and only once a
  decode replica has read idle for `IdleSettledFor` (two minutes;
  `settledIdleDecode`): then decode has room the router is not using, and
  prefill may be ordered on its own demand.

  The test is remembered for the collector's row window
  (`DecodeSaturationMemory`, one minute): on the measured episode decode's
  occupancy dropped under k1 on the fourth cycle while its queue and
  prefill's row -- a one-minute max, repeated to the token -- had not
  moved, and that row would otherwise have recorded.

  A decode bound by its
  sequence ceiling before its KV does not gate; that is a prefill
  mislearned low, which costs money, where a prefill that cannot be
  ordered costs latency. Newer vLLM splits the waiting count by reason
  (`vllm:num_requests_waiting_by_reason`, `reason="capacity"` for the
  scheduler's own queue); reading that for both this test and P1 itself is
  the follow-up, once the vLLM the stack ships is known to carry it.
- **Priced per row, and the rows may sit in different buckets.** The floor
  takes medians over the replicas' rows, and a fresh replica sits in the
  bucket of its own few completions for a cycle or two; on one pass a
  landing replica's `long` reading (the previous phase's 6.67) was
  medianed with two `xxlong` rows at 3.27 for a cycle. And `mayOrder` is
  set by any own row with two samples, so a row in an old bucket unlocks
  the full order for a role whose current bucket has one. Neither changed
  a decision on the measured passes; both are open, pre-existing: pricing
  a variant from one bucket per cycle is the fix.
- **Keyed by output length**, in factor-of-two buckets above 500 tokens
  (`classifyOutputLength`). `mu` falls roughly with output length, and two
  shapes sharing a bucket share a window whose max is the shorter shape's --
  which held a fleet at the shorter shape's size while the longer one was
  served. Measured on the shape-swap benchmark, where 1000 and 4000 tokens
  shared one bucket; they no longer do. A bucket with no reading borrows the
  nearest bucket's under the same key prefix until it has its own -- without
  that, the first cycle of a new shape had no floor at all, and a
  three-replica fleet was sized to one from 400k tokens of occupancy. The
  per-replica log line names the bucket the figure came from
  (`saturatedThroughputBucket`).

A floor that binds logs `throughput-demand-floor` with `demandBeforeFloor`,
`arrivalRate`, `saturatedThroughput`, `perReplicaCapacity`, `replicasImplied`
and `heldAtFleet`; the per-replica `replica-capacity-decision` line carries
`saturatedThroughput` every cycle, 0 until saturation has been observed.

## What it is not invariant to

`mu` is learned only at saturation, and the queue defines saturation. A
replica that falls behind by growing its running batch instead of its queue
(admitting everything while its inter-token latency climbs) has not saturated
by that definition, and records nothing. Measured on the shape-swap
benchmark's second phase (1000 in / 4000 out at 6 req/s): two decode replicas
took their batch from 75 to 229 against a `max-num-seqs` of 256 over four
minutes, ITL from 5.8 to 19.9 ms, completing 5.5 req/s between them against 6
arriving, with `waiting` never above 1. Until the batch reaches the engine's
own ceiling and a queue forms, the floor has no `mu` for that shape and holds
whatever the previous shape's was.

That window closes on its own, because the batch does reach the ceiling,
unless something orders a replica first and drains the batch before saturation
is observed. The retired floor did exactly that.

EPP flow control keeps it open. The router holds the queue upstream, so
vLLM's own `num_requests_waiting` stays near zero and `P1-obs` (queue at or over
`queueLengthThreshold`) fires only in the minutes a replica joins or a step
lands -- which are the readings the admission refuses. On E1 (2026-10-23,
flow control on) no replica queued again after its first minute. A measured
`mu` then is rarely written or refreshed, and the floor prices from the
**derived** `mu` of the ITL line wherever it is the lower figure: an own
measured window more than 2x above it outranks it only while the bucket has
read saturated in agreement with it in the last five minutes, and a stale one
under it keeps pricing for up to 30 minutes (`MeasuredLowHoldFor`), or until a
Ready replica serves more than twice it (`measuredYieldsToDerived`). That holds for the
variant's **own** line only:

- **A borrowed line holds the fleet but does not grow it.** A derived `mu`
  from a line borrowed from another variant (a fresh namespace, a rename) is a
  `borrowedReading` in `floor.Estimate` and never sets `mayOrder`: it prices
  the replicas but caps the floor at the role's anticipated supply. In review,
  three replicas at KV 0.8 under flow control with a borrowed line read the
  same demand as with no line at all (1 082 760).
- **No line is no floor.** With no line fitted yet there is no derived `mu`
  either, and occupancy alone sizes the role until the variant fits its own
  line (about 28 cycles of readings across the k spread). Occupancy
  under-orders at a step: a step to twice the fleet's capacity (lambda 24 on
  a fleet good for 12) read 3.53 replicas at a router queue of 0 and 5.63 at
  360, against the 7.1 and 8.8 the load needs, closing only as the router's
  queue builds. This is the cost of refusing the join-time readings, which on
  E1 over-ordered 12-20x.

k2 is the same: it
is learned in those minutes or not at all, and otherwise comes from the
history window or the engine arguments (`P2-hist`, `P3-k2`).

## Why the Little's-law floor was retired

The first version of this file built a floor from the load directly: `lambda x
W x (avgIn + avgOut)`, with `W` the per-request service time the engines
report. The premise was that `W` belongs to the load. It does not. Service time
is output length times inter-token latency, and ITL grows with the batch a
replica runs, so `W` tracks how many replicas share the load. Measured on one
decode pod across a run at a constant 6 req/s and a constant 6000 in / 1000 out:

| replicas sharing the load | batch on this pod | ITL | `W` |
|---|---|---|---|
| 6 | ~1 | 2.0 ms | 2.0 s |
| 2-3 | 40-78 | 4-6 ms | 15-22 s |
| 1 | 136-178 | 18-50 ms | 16-52 s |

A 25x range at the same load and shape, so `lambda x W x tokens` with the `W`
of the moment is current occupancy restated (`L = lambda W` is an identity). On
the way down that made it useless: at six replicas it read 88k tokens against
85k occupancy and let the fleet go to one. On the way up it was harmful, and
that measurement retired it. In the shape-swap benchmark's
second phase, with the throughput floor holding two replicas:

```
13:25:47  W=36s   floor 1.29M -> ordered the 3rd replica   (occupancy 0.75M)
13:28:32  W=77s   floor 2.50M -> ordered the 4th           (occupancy 1.37M)
13:31-34  W=15s   floor 0.40M -> scale-down to 2           (occupancy 0.22M)
13:37:47  W=57s   floor 1.65M -> the 3rd again
13:40:32  W=76s   floor 2.41M -> the 4th again
```

At two replicas the load "needed" 2.5M tokens; at four, 450k. Each fleet size
justified the move to the other, with a ten-minute period. And every one of
those orders landed *before* the two replicas reached saturation, so the
throughput floor never got the `mu` that would have held three -- the floor
that oscillated was also the one preventing the floor that would not.

Its stated purpose, damping the collapse, is what the throughput floor does
with a term that does not move. What it did beyond that, ordering replicas on
an inflated `W`, occupancy and the queues already do, later and for the right
reason. So it was removed, not corrected: no `W` belongs to the load.

## Claims in this document, and their status

| Claim | Status |
|---|---|
| Demand fell 7.5M -> 152k as the queue drained | Measured, run `biran-20260822-021153-340` |
| Six replicas read 100k tokens; target went to one; re-saturated in a minute | Measured, run `biran-20260915-102548-571` |
| `W` ranges 25x with the batch at one load and shape | Measured, same run, one pod's own counters |
| `mu` = 5.4 req/s at 6000/1000, ~2.75 at 1000/4000 on an H200 | Measured, that run and the 2026-09-16 re-run |
| The throughput floor held two decode replicas through phase 1 | Measured, 2026-09-16 re-run (`throughput-demand-floor` at 13:12-13:25) |
| Two replicas grew their batch 75 -> 229 with `waiting` <= 1 in phase 2 | Measured, same re-run, pod counters |
| The retired floor ordered every phase-2 scale-up, at `W` 36-77s | Measured, same re-run, controller log |
| Splitting the buckets with no fallback loses the floor on a new shape (3 -> 1) | Measured, run `guidellm-1789577844-0smu2m_1` |
| Retired floor + split buckets + borrow: phase 2 goes 2 -> 3 -> 4 and holds, `mu` = 2.97 learned at the one saturation | Measured, run `guidellm-1789581140-kb3q2v_1` |
| The learning saturation costs one window at 4.8 s p95 TTFT | Measured, same run, engine histograms per 5-minute window |
| The second replica's order came from occupancy at +69-78 s on three runs; the lone replica tipped at +91 / +91 / +144 s | Measured, runs `wm9k0y_1`, `ydqs8h_1`, `t1pclo_1`, the first replica's own counters |
| A 350-request backlog charged as residency ordered five extra replicas that arrived after it was gone | Measured, run `biran-20260915-102548-571` |
| `(lambda + B/T) / mu` orders the second replica at +62 s and sizes the first-ramp backlog at 3 replicas | Measured, run `guidellm-1789645863-l32fnc_1` (warm controller) and `guidellm-1789642083-9yngog_1` (cold) |
| The first reading at a saturation under-reads and the window's max corrects it | Measured, same cold run: 3.67, 5.23, 7.13 req/s on three consecutive saturated cycles |
| A prefill saturation under a saturated decode, recorded as prefill's, ordered a second prefill replica and held it 33 minutes at near-zero resident KV | Measured, run `guidellm-1789743465-ckjz7y_1` (cold), full controller log of the pass (`wva_controller_full.log` beside the results; the harness's own copy starts at 15:09:46): prefill `P1-obs` at 15:01:33-15:02:18 with both decode replicas at queue 36-81, then `replicasImplied` for prefill 1.08 at the median (p10-p90 0.98-1.17, above 1.0 on 86 % of 133 cycles) through 15:35 |
| The persisted prefill `mu` alone ordered a second prefill replica at +51 s on the warm pass that followed, at zero resident KV | Measured, run `guidellm-1789746634-pov4xp_1`: `throughput-demand-floor` for prefill at 15:51:35, `arrivalRate` 5.5, `saturatedThroughput` 5.57, `residentDemand` 0, `replicasImplied` 0.99 |
| Left unrecorded, the same cycle prices prefill at k1 with no order | Replayed from the run's rows in `downstream_saturation_test.go` |
| With the gate, the hold and the memory, prefill stays at one replica on every pass; the hold engages only while decode is full and queued, and on the first cold pass it is what kept prefill at one | Measured, runs `guidellm-1789760128-zphd9d_1` (cold: 11 `prefill-demand-held` cycles at +130..+340 s, four of them capping 830 300 resident tokens -- 90 % of k1, no queue -- at 0.85 x k1; 0 `P1-obs-downstream`; all-pod 137.9 GPU-min against 167.8), `guidellm-1789768002-eb450o_1` (cold again: 9 and 0, 142.1, p95 241 ms) and `guidellm-1789763936-mr6h9x_1` (warm: 9 and 0, 132.9 against 168.0, p95 194 ms) |
| Every saturated completion rate logged is N/30 but a fresh replica's first, extrapolated window, and one reading stood on four consecutive cycles (two scrape pairs) and cleared the two-readings guard | Measured, runs `guidellm-1789760128-zphd9d_1` (the one exception, 4.715 on a replica 75 s old), `guidellm-1789768002-eb450o_1`, `guidellm-1789763936-mr6h9x_1` and `guidellm-1789803750-vl4r9g_1` (2026-09-19, the merged tree) (`replica-capacity-decision`, `saturatedThroughput`; 30 s scrapes): 3.43 = 103/30 at 07:44:41-07:45:26 |
| With a minute's spacing between counted readings one reading holds and does not order; the 35 minutes at three replicas that followed it are the under-read, not the count | Replayed from the run's rows in `throughput_floor_test.go` ("does not order on one saturated sample") |
| With the spacing, the cold ramp's second sample came a minute after the first (4.37, then 6.67 -- the drain, see above -- and the fresh second replica's extrapolated 2.81 in the same cycle, one moment at the higher), and phase 1 settled at two decode replicas from +412 s -- a third, ordered on occupancy under the queue at +143 s, came down then -- instead of three held for the rest of the pass on 3.43. The ramp's orders (+53 s, +143 s) were occupancy's, as on every pass, and its first-window p95 TTFT of 0.79 s is the start-time lottery, not this change's | Measured, run `guidellm-1789812977-xzf9ay_1` (2026-09-19, the branch head at `faa3d17a`, cold): `requestRate` 4.37 at +113 s, 3.33, then 6.67 at +173 s; `saturatedThroughput` 6.67 on every decode floor line from +173 s to the phase switch; decode 97.2 GPU-minutes against 109.7 on the pass whose reading stuck at 3.43; prefill at one throughout. On that head the phase switch's one reading (2.25) met an anticipated supply of three with the third still starting and ordered a fourth at +1434 s, released at +1584 s -- a single reading holds now, at the anticipated three; the phase-switch cycle is replayed in `throughput_floor_test.go`, not re-run |
| On the final rule -- spacing, fold, hold -- one reading holds where it used to order, and the cost is one cycle: at the cold ramp the floor's single reading implied 1.98 replicas' worth on a fleet of two with the second still starting, held at 0.85 x 2, and occupancy ordered the third 15 s later; at the phase switch two cycles of one reading implied 2.77 and 2.71 on an anticipated three, held at 0.85 x 3, and no fourth was ordered (2.77 / 0.85 rounds to four). Phase 1 ran at two decode replicas, phase 2 at three, prefill at one throughout; the sticky scale-down held a published two against a fresh three for three cycles in phase 1 (its first engagement on this trace); the lowest GPU-minutes of the series | Measured, run `guidellm-1789841919-vtvrsk_1` (2026-09-19, the branch's code at `505098e2`, cold): `throughput-demand-floor` `heldAtFleet` true, `heldWhy` `single-sample` at +160 s (`replicasImplied` 1.98, `saturatedThroughput` 3.63) and at +1452/+1467 s (2.77/2.71 on 2.23/2.4); `requestRate` 3.63 at +130 s, 3.1 and 3.73 folded, 4.47 a window later at +221 s; decode orders at +70 s, +175 s (both occupancy), +1391 s; 92.8 decode / 131.7 all-pod GPU-minutes over the harness window against 97.2 / 136.3 on the pass before and 109.7 / 148.2 on the pass whose reading stuck at 3.43. The p95 TTFT of the first five minutes, 3.25 s, is the ramp -- the second replica was ordered at +70 s on occupancy (959 k resident tokens; +53 s on the pass before) and served from +175 s (a 105 s start; +143 s before), so the lone replica queued 113 for 45 s where the pass before queued 30 -- and 15 s of it is the hold. Overall p95 4.88 s (the ramp and the phase switch at two, 5.05 s in minutes 20-25; 4.2 s there on the pass before), p50 104 ms |
