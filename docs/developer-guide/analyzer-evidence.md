# Analyzer evidence

What the scaling signals were measured to do, and on which pass. The code
states the rule it follows; this file is why that rule and not another one,
with the run behind each figure so a reader can disagree with the evidence
rather than with an assertion.

Every entry names its date and its run. A figure without one of those is not
evidence, and does not belong here.

The stands: CoreWeave 8 x H200 nodes, Qwen3-0.6B on a P/D fleet, guidellm
driving a replayed trace at a constant arrival rate, the shape switching once
mid-run. `k1` (the memory-bound per-replica capacity the analyzer prices with)
is 930,508 tokens there; the engine's physical KV capacity is 1,163,136.

## The throughput floor

### Occupancy alone under-sizes a fleet that is keeping up

*2026-09-15, run `biran-20260915-102548-571`, 6 req/s at 6000/1000.*

Occupancy -- resident KV plus the waiting queues -- is a state of the fleet,
not a property of the load, and it falls as replicas are added. So is anything
built from a per-request cost measured on the current fleet: the service time
the engines report is ITL x output length, and ITL grows with the batch a
replica is running. Measured on one decode pod across the run:

| replicas sharing the load | batch | ITL | W (service time) |
|---|---|---|---|
| 6 | ~1 | 2.0 ms | 2.0 s |
| 1 | 136-178 | 18-50 ms | 16-52 s |

A 25x range in W at the same load, so `lambda x W x tokens` with the W of the
moment is current occupancy restated. The arrival-rate floor built that way
read 88k tokens at six replicas and authorised the scale-down to one; on the
way up it did the opposite harm, pricing the load at the inflated W of a fleet
that was behind, ordering replicas before any had saturated, and releasing
them once they had brought W back down -- a 2-4 replica oscillation with a
ten-minute period on the second phase (`docs/proposals/backlog-sizing.md`).

What a replica can do does not move with the fleet: its completion rate when
saturated. On the same run, mu read 5.4 req/s at 6000/1000 and 2.5 req/s at
1000/4000 against 6 req/s arriving -- 2 and 3 decode replicas, which is where
the fleet held when it was left alone at those sizes.

### The floor is not capped at the fleet's own size

*Four passes, 2026-09-16 to 09-19.*

The first version was capped, so that it could hold a fleet but never order
one, and scale-up stayed with occupancy and the queues. That left the order
too late: a lone replica at 6 req/s against a mu of 5.4-6.4 is at 94 % of what
it can do from the first cycle and tips into preemption at 75-145 s, while
occupancy crossed k1 at +69-78 s on every run -- which, with a 60-100 s
replica start, lands the second replica after the tip on most of them.
Uncapped, `lambda / mu` = 0.94 replicas orders it in the first cycle lambda is
measured, some 50 s earlier. What a mu that under-reads costs is bounded by
the under-read; what a late order cost was five to seven replicas at the first
ramp of every run.

Uncapped is safe, not merely better. What the floor can order is
`(lambda + B / T) / mu` by construction -- fixed by the offered load and by
what one replica does when saturated, neither of which moves as replicas are
added. That is the property occupancy lacks, and it is why removing the cap
does not reintroduce the ratchet the cap was there to prevent.

### A backlog is throughput, not residency

*Same stand.* Occupancy charged every queued request at its full KV footprint,
as if all of them had to be resident at once, and the engine sized the fleet
to hold them: 350 queued requests became five to seven extra replicas, each
arriving after the queue was gone. Priced as work to drain -- B requests
within T seconds on top of lambda arriving -- the same backlog is one more
replica.

`BacklogDrainSeconds` is 60 because a drain target shorter than a replica's
start orders capacity that drains nothing, and a replica on these stands takes
60-100 s to become Ready.

### Prefill's share of the scheduler queue is dropped

*Same stand.* A prefill replica holds a prompt's KV for its prefill time plus
the hand-off to decode; the only thing that makes it hold more is decode being
saturated, which more prefill replicas do not fix. Every prefill replica
beyond the first at the first ramp was ordered by that term, and none of them
prefilled anything a single one could not.

## The hold rules

### Two readings, not one, before a reading may order

*Shape-swap trace.* The first reading at a saturation under-reads: a 1-minute
rate on a replica that has been full for 20 s counts a third of a minute's
completions. Three consecutive saturated scrape pairs, 60-90 s apart, read
**3.67, then 5.23, then 7.13 req/s**. An order on the first over-provisions in
a way that removes the saturation which would have corrected it, so
`MinThroughputSamplesToOrder` is 2 -- and the two readings must come from two
rate windows, not the same window read twice.

Letting a single reading order one replica was tried twice and dropped. Against
the anticipated supply it ordered a fourth replica at a phase switch whose
third was still starting; against the running supply it was a ratchet, since
nothing remembered that the reading had already ordered, so once the ordered
replica reported, the same reading ordered the next. Across four passes the one
replica bought a single cycle over the plain hold -- a third replica 30 s ahead
of occupancy, on an under-read that then ran unneeded for 35 minutes -- and the
hold's own cost, replayed, is 15-45 s on the cold ramp's next replica.

### A single reading may order behind a standing queue

*2026-09-25, run 24 pass B, the 8k1000 -> 1k6000 trace at 6 req/s, with the
arrival rate already corrected.*

`MinThroughputSamplesToOrder` held decode at 2 replicas from t+75 s to t+135 s
while the scheduler queue went **32 to 217**. The floor's own figure through
those four cycles was **2.51, 2.67, 2.73 and 3.06** replicas: it wanted a third
and was not allowed to ask. The fleet reached 6 at t+225 and the queue peaked
at 282; the run's whole TTFT tail is that window, p95 26.5 s against a p50 of
114 ms.

The hold's argument has two halves: an order on a first under-read
over-provisions, **and** the over-provisioned fleet then never saturates again
to record the second reading that would have corrected it. The second half
fails while the scheduler is holding work -- the queue keeps the fleet
saturated until it drains, so the correcting reading arrives either way.

A first version also withheld the figure while any replica was starting, on
the reasoning that the attempt above over-ordered "at a phase switch whose
third was still starting". **Run 26 measured that version inert**: zero
firings across a full pass, four single-sample holds in both arms, a queue peak
of 359 against 380. Through every hold cycle the decode deployment had
`spec=2` and `ready=1`, so `PendingReplicas` was 1 and the test blocked the
rule -- a start outstanding is what a ramp is, so it excluded the case it was
written for.

It was redundant besides. A phase switch sets `staleShape`, which the first
condition below already blocks. And the pacing it imitated exists a layer up:
the engine computes `RC = max(0, TotalDemand / scaleUp - TotalAnticipatedSupply)`,
subtracting the supply already on its way, so it cannot re-order what is in
flight.

What makes the climb safe is not pacing but invariance: `(lambda + backlog /
drain) / mu` is fixed by the load and the queue, not by the fleet, so repeated
firings converge on it instead of ratcheting past it -- the same property the
floor's package comment rests on -- and the rule stops firing when the queue
drains. Measured over five cycles with a replica driven pending to ready
throughout: the floor reads 1,818,582 tokens every time, 1.95 replicas, and
does not grow.

Two conditions:

  - **The SCHEDULER's queue, not the merged backlog, and worth more than
    both a second of arrivals and one replica-second of service.** Against
    `lambda` alone the test degenerates as the load falls -- at 0.1 req/s one
    stray request is ten seconds of arrivals, which is jitter. vLLM counts a request waiting for its remote KV in
    `num_requests_waiting`, so a large model over a slow link keeps six or more
    there at all times while decode admits fine -- no further replica drains
    those. A request in the scheduler's queue has not been dispatched to any
    pod at all. The first version of this rule used the merged backlog and
    broke the spec that pins exactly that case.

### A borrowed reading never outvotes a replica's own

*2026-09-20, the 1000/6000 shape-swap trace, cycles 11:45:22-11:47:22.*

A fresh replica's first completions are its short requests -- they finish
first, and a replica with none yet reads an output length of 0 -- so its key
lands in a short bucket with no reading and borrows the previous shape's mu.
Measured: the borrowed figure was **4.38 req/s** from 1000-token outputs while
the replica that had been saturated under the new 6000-token shape read
**1.4-1.7** of its own. Two fresh replicas out of three put 4.38 at the median:
the backlog of 441 requests read as 2 replicas' worth instead of 5, the floor
fell from 10 M tokens to 3 M in one cycle, the target went from 10 to 4, and
the backlog kept growing (256 -> 642) under the figure that said it would not.
The target then swung 10 <-> 4 for ten minutes.

## The capacity windows

### The mu window reads the median, not the maximum

*2026-09-22 rerun, phase 1.*

`Max` was right while mu was a **completion** rate: that error is one-sided,
because a saturated replica's completion rate under-reads while it fills, so
the largest reading is the best estimate of what it sustains. The argument did
not survive the move to a **generation-token** rate, which bursts rather than
under-reads. Measured over the rerun's first phase, the per-replica rate ran
**4,028 min / 7,548 median / 11,663 max**, while the fleet's own total sat at
33,175 tokens/s against a demanded 36,000 -- the typical reading was the true
one and the peak was half again above it.

Read with `Max`, the window ratcheted to a mu of 3.6 req/s and the demand floor
asked for 1.6 replicas where about 8 were needed. The fleet's average
per-replica rate over that window, 4,538 tokens/s, prices mu at 0.76, inside
the 0.61-0.85 the fleet's own queueing implies. The window reads `Median`.

## The arrival rate

### The arrival rate is a dispatch rate while the queue is building

*2026-09-24, the 8k1000 -> 1k6000 shape-swap trace at a constant 6 req/s, both
passes of run 23.*

`QueryModelArrivalRate` read
`rate(inference_extension_scheduler_attempts_total{status="success"}[1m])`.
That counter increments when the scheduler PLACES a request on a pod, so while
the fleet is saturated it measures what the fleet could take, not what the
workload offered. Against the flow-control enqueue counter, which increments
when a request is accepted, over the ramp of one pass:

| t+ | placements (what sized the fleet) | enqueues (what arrived) |
|---|---|---|
| 120 s | 5.38 | 6.10 |
| 180 s | 3.82 | 5.56 |
| **240 s** | **1.66** | **6.16** |
| 300 s | 4.88 | 5.90 |
| 360 s | 5.22 | 5.38 |
| 420 s | 6.50 | 6.50 |
| 480 s | 5.80 | 5.80 |

A **3.7x under-read at the worst moment**, with the scheduler queue at 234
and climbing (it peaks at 356 one sample later) -- and the two agree to the digit from 420 s. The queue itself
drained earlier, at about 270 s; placements stay low for the two samples after
that because the counter is still working through the drain burst, not because
the queue is still full. The error is not noise:
it is largest exactly when the fleet is furthest behind, which is when this
figure orders capacity.

What it cost on that run, arithmetically: `replicasImplied` is
`(arrivalRate + backlog / drainSeconds) / mu`, and at the 1.66 sample that is
`(1.66 + 605/60) / 2.8651` = **4.10**, which is what the log says. Substituting
the arrival rate that was really offered, 6.16, gives **5.67**.

So the 3.7x under-read did not become a 3.7x under-order. The backlog term was
supplying 10.08 req/s against the arrival term's 1.66 and had already absorbed
most of the error; what the under-read cost was **1.6 replicas, one replica
after the ceiling**. That is worth fixing -- one decode replica through a ramp
is the difference the rest of this section describes -- but the floor is not
blind while the queue is growing, because the queue itself is an input. The fleet stalled at 5 decode replicas through
minutes 4 and 5 while the scheduler queue climbed to its peak of 356 at
t+255 s; it was 3 by t+270 and 0 from t+285, so the queue is empty for the rest
of the twenty-minute pass. The run's whole TTFT tail is those first four and a
half minutes -- p95 44.7 s and p99 70.3 s over the pass, against a p50 of
112 ms.

The sibling pass of the same run drew slightly better numbers, held 6 replicas
instead of 5 at the matched offset, peaked at 284 queued instead of 356, and
landed p95 22.4 s. Both passes reached 7 eventually. Same
code, same trace, same hour; the difference is which side of a replica the
under-read happened to fall on.

`deriv(queue_size)` was tried as a correction -- placements plus queue growth
should be arrivals -- and rejected. Measured against the enqueue counter it was
wrong by up to 8.28 req/s and went NEGATIVE (-0.20, -2.38 req/s) at the two
samples where the queue was draining fastest, which is worse than the
under-read it was meant to repair.

*2026-09-28, run U (`feat/mu-from-itl` at 08ad4f49), P/D fleet, same 6 req/s
trace.* The same comparison on a fleet whose flow controller queued harder. The
placement arm does not merely under-read here, it reaches **zero**:

| wall clock | placements (the fallback arm) | enqueues (the arm in use) | EPP queue |
|---|---|---|---|
| 13:39:30 | 3.22 | 5.74 | 227 |
| 13:40:00 | 3.22 | 5.56 | 395 |
| **13:40:30** | **0.00** | **5.94** | **584** |
| **13:41:00** | **0.00** | **6.10** | **766** |
| 13:41:30 | 2.69 | 6.00 | 0 |

Two consecutive samples at 0.00, with 766 requests queued in the scheduler at
the second of them. Nothing was being placed because every replica that could
take work was already full, which is exactly the state the arrival rate is
supposed to describe.

Zero is worse than an under-read, because it is not only a small number. The
floor hands `backlogAtLanding` its start times only when `input.ArrivalRate > 0`
(`throughput_floor.go`), so a fleet on the placement arm would lose the arrival
term **and** the landing projection at the same moment, and keep only the
backlog term -- at the one point in a ramp where all three are load-bearing.
This run was on the enqueue arm and read 5.48-6.08 throughout, matching the
offered rate; it is the control, not the failure.

The fleets still on the placement arm are those the model-label join drops,
where one pool serves several models (`arrivalModelLabel`). Their queue cannot
be attributed per model either, so the fix is not a better fallback query --
it is that those fleets need a per-model enqueue counter from EPP.

### Phase-1 TTFT is the router's admission gate, not the fleet size

*2026-09-28, runs T, U and V on a P/D fleet, the 1k6000 -> 8k1000 trace at
6 req/s. Run V ran with the EPP at `--v=4`; its timings are distorted by the
logging and are not used here, only its log fields.*

Three runs were compared on phase-1 TTFT while the demand floor was being
tuned. Run U ordered a larger first step than run T and reached nine ready
decode replicas 15 s SOONER (`spec` 9 at +90 s against +105 s, ready 9 at
+150 s against +165 s, pod start 60 s in both). Its phase-1 TTFT p95 was
nonetheless twice T's, 136 s against 68.5 s.

> Those two TTFT figures, and every engine-side TTFT comparison in this
> section, were later shown to be unreliable: the metric cannot see time spent
> in the router's queue, and the percentile lands inside a four-sample spike.
> The conclusion below -- that the router withholds work from Ready replicas --
> rests on the per-pod serving counts and the router's own queue depth, which
> are unaffected. See "Judge the ramp on the client's TTFT" below.

The replicas were Ready and receiving nothing. Per-pod `vllm:num_requests_running`
at the worst moment of run U:

| wall clock | ready decode | pods serving | requests held in EPP |
|---|---|---|---|
| 13:40:00 | 3 | 1 | 395 |
| 13:40:30 | 6 | 1 | 584 |
| **13:41:00** | **9** | **1** | **766** |
| 13:41:15 | 9 | 9 | 0 |

EPP saw the endpoints immediately -- `llm_d_epp_ready_endpoints` tracked
kube readiness with no lag -- and held the requests in its own flow-control
queue anyway. The release is not triggered by capacity arriving: it is
`saturation >= 1`, which fits every sample of both runs (15/15 in T, 17/17
in U), and the queue drains to zero in the same 15 s sample the signal crosses.

The signal is the default `utilization-detector`:

    endpointScore  = max(queueDepth/queueDepthThreshold, kvUsage/kvCacheUtilThreshold)
    poolSaturation = mean(endpointScore)

with `queueDepthThreshold` defaulting to **5**. Measured live during run V,
with `gpu_cache_usage_perc` at 0.000 on every pod so the KV arm contributes
nothing:

| pod | `num_requests_waiting` | score = queue/5 |
|---|---|---|
| ...cpph2 | 272 | 54.4 |
| ...hfz2w | 81 | 16.2 |
| eight others | 0 | 0.0 |

A vLLM engine at `max_num_seqs: 256` queues hundreds of requests as normal
batching. One pod at 272 waiting scores 54.4, and averaged over ten endpoints
that single pod puts the pool at 5.4 -- five times the ceiling on its own.
**Diluting one busy pod below the ceiling would take about 54 idle endpoints.**
Nine cannot do it.

So ordering replicas faster cannot shorten phase-1 TTFT. The block clears when
the LOADED pods' own queues fall under about five requests each, which is why
the release is sudden and total and why it coincides with the queue reaching
zero rather than with replicas turning Ready. Run T released earlier than run U
only because its loaded pod's queue was shallower.

What this bounds: the floor's three terms were measured at ~70 s of metric lag
and ~60 s of pod start, and this adds a third term of the same size that sits
entirely outside the autoscaler. Tuning the floor against phase-1 TTFT is
measuring the router.

#### Raising `queueDepthThreshold` is NOT the fix

The obvious reading -- the threshold is 5 and the engine queues hundreds, so
raise the threshold -- is wrong, and worth recording so it is not proposed
again. Tolerating a deep queue inside the engine is the failure flow control
exists to prevent. Once a request is dispatched it is committed to that pod:
it cannot be steered to a replica that becomes Ready a moment later, it cannot
be reordered behind a higher priority, and it is no longer covered by the
queue-wait TTL. A threshold of 5 correctly says "an engine should not be
sitting on a queue".

The defects are in what the score is, not where the line is drawn:

  - `poolSaturation = mean(endpointScore)` lets ONE loaded endpoint outvote
    every idle one. The pod measured at 272 waiting scores 54.4; averaged over
    ten endpoints it holds the pool at five times the ceiling by itself, and
    diluting it would take about 54 idle endpoints.
  - It measures how busy the engines are, not whether there is anywhere to put
    the next request -- which is the only question dispatch actually asks.

The shape that matches the goal -- spread arrivals onto fresh replicas, build a
queue in neither place -- is the `concurrency-detector`, which is not an Alpha
plugin and needs no `--allow-experimental-plugins`:

    poolSaturation = aggregateInflight / aggregateCapacity

Two properties follow, and they are the two this fleet needs. Capacity is a
function of the endpoint COUNT, so a replica turning Ready lowers saturation
immediately, with no scrape to wait for and no mean to dilute. And
`maxConcurrency` bounds what EPP will dispatch to any single endpoint, so with
it set below the engine's own running capacity (`max_num_seqs`) a local queue
never forms: the waiting happens in the EPP, where a request can still be sent
somewhere else, instead of in the engine, where it cannot.

    - type: in-flight-load-producer
      name: inflight
    - type: concurrency-detector
      name: conc
      parameters:
        concurrencyMode: hybrid
        maxConcurrency: <below the engine's max_num_seqs>
        inFlightLoadProducerName: inflight
    flowControl:
      saturationDetector:
        pluginRef: conc

The trade named in the tutorial is that this loses visibility into real KV
pressure; `hybrid` mode scores each endpoint as the larger of its request and
token ratios before averaging, which covers the worst of it. UNVERIFIED on this
fleet -- the reasoning above is from the tutorial and the measurements in this
section, not from a run.

Routing is a second, separable defect. The decode profile weights
`prefix-cache-scorer` at 3 and `active-request-scorer` at 2, so the scorer that
would spread arrivals across idle replicas is outranked -- by one whose
measured hit ratio on this workload is **0.000** for the whole run, with
`llm_d_epp_request_cached_tokens / input_tokens` also 0. Whatever holds the
queue closed, the scorer order is why the requests that DO get through
concentrate rather than spread.

`flow_control_stale_endpoints` is also not scraped on this cluster. It is the
metric that reports the other half of the same detector -- an endpoint whose
metrics are older than `metricsStalenessThreshold` (default 200 ms) scores a
full 1.0, fail-closed -- and without it that contribution is unobservable. The
signature was seen directly on an idle EPP at startup: `saturation: 1,
usageLimit: 1` logged with zero requests received.

### A run inherits the previous run's shape unless the controller restarts

*2026-09-28, runs U, W and X: the same 1k6000 -> 8k1000 trace, the same image
digest, three different controller lifetimes.*

The shape tracker keeps its fleet-shape memo in the analyzer, so it survives for
the lifetime of the controller POD, not the lifetime of a benchmark run. A run
whose trace opens at 1k-in leaves the memo at the 8k-in of its own phase 2. The
next run over the same trace therefore opens on a transition the tracker reads
as a genuine fleet-shape change -- and raises the hold before a single request
has arrived.

Run W, first shape event, 25 seconds BEFORE its load started:

    {"inputTokensWas": 8000, "inputTokensNow": 1000,
     "outputTokensWas": 999.99, "outputTokensNow": 999.99}

8000 in is run V's phase 2. The hold that followed ran from 16:51:35 to
16:54:05 with `heldAtFleet: true, heldWhy: "shape-change"`, the fleet pinned at
two replicas while `replicasImplied` climbed 4 -> 15.5 and the scheduler queue
384 -> 1178. Phase-1 TTFT p95 came out at 114.7 s.

What separated the runs was not the code and not the trace:

| run | image | rollout | controller age at start | first shape event |
|---|---|---|---|---|
| U | new digest | yes | fresh | 13:56, mid-run, at the real flip |
| W | same digest | **no** | 3 h | **16:50, before load** |
| X | same digest, restarted by hand | yes | fresh | 18:24, mid-run, at the real flip |

`kubectl set image` with an unchanged digest is a no-op, so it does not restart
anything. Run W reused run V's controller and inherited its memo; run X restarted
it explicitly and behaved like run U -- ramp to nine replicas in 80 s against
run W's five minutes, `heldAtFleet: false` throughout the ramp, and the hold
appearing only at the genuine phase transition.

The hold itself is not at fault in any of the three. It did what it is for, on
the input it was given.

Two consequences:

  - **Restart the controller between runs**, or the first phase of every run
    after the first is measured through a hold. Changing the image digest does
    it as a side effect, which is why this went unnoticed for as long as the
    comparisons happened to be between different builds.
  - **A comparison whose runs had different controller lifetimes is not a
    comparison.** Run W's phase-1 figures were read as a regression caused by an
    EPP scorer change, which they had nothing to do with. What carries the point
    is the FLEET: run W sat pinned at two replicas for two and a half minutes
    while the scheduler queue climbed 384 -> 1178, where run X -- same scorer
    weights, fresh controller -- ordered nine replicas in 75 s and never held at
    all.

    An earlier version of this section argued it with engine-side TTFT p95
    instead (67.5 s against 69.1 s, "within 1.6 s"). Both halves of that are
    wrong, and both are recorded below: the figures came from a truncated window
    and are 44.9 s and 69.1 s over the full phase, and the metric cannot
    adjudicate between runs in the first place -- see "Judge the ramp on the
    client's TTFT" and "The noise floor of this benchmark", which puts 1.6 s
    roughly twenty times inside the run-to-run variance. The conclusion stands;
    the evidence for it is the replica timeline, not the percentile.

### The image under test is not the branch, and its tag will not say so

*2026-09-28, runs U, V, W and X, all on `mu-from-itl-v7`.*

Feature-branch images on this repo are hand-built and hand-tagged; CI does not
build one per commit. So the tag says which BRANCH it came from and nothing
about which commit. `v7` was pushed at 13:33:40Z, and the branch kept moving:

| committed (+0300) | commit | in v7? |
|---|---|---|
| 16:06:05 | `1c03837f` price the backlog that will exist when capacity lands | yes |
| 16:20:23 | `4e52552c` age the starting replicas | yes |
| **16:33:40** | **── v7 built ──** | |
| 16:59:27 | `d03440a0` **the ramp ratcheted**, and seven other ways the sizing model was wrong | **no** |
| 17:19:55 | `08ad4f49` report the STUCK replicas | **no** |

The consequence was not a missing feature but a HALF-WIRED one. `backlogAtLanding`
was in the binary; the `startSeconds` plumbing and the
`horizon = max(drainSeconds, startSeconds)` fix that make it do anything arrived
in `d03440a0` and did not. The projection therefore returned its own input on
every cycle, which is invisible in the log because the field that would have
shown it (`projectedBacklog`) was added by the same missing commit.

It was caught by arithmetic, not by inspection. Run X, 18:06:44Z:

    lambda 5.78, mu 1.8774, replicasImplied 6.6228
      => rate = 6.6228 x 1.8774 = 12.43
      => b / horizon = 12.43 - 5.78 = 6.65
      => b = 399 at horizon 60  ==  backlogRequests 399  ==  observed

A live projection would have added `lambda x T` for the start time and landed
near 545. `b == observed` to the digit is the signature of a projection that
never ran.

What this cost: the ramp lag those runs measured (72 s to order nine replicas,
the cap granting 1, 1, 1, 2, 3, 5 over five cycles) is the behaviour
`d03440a0` had already fixed. Two separate attempts to fix it again were written
and reverted, each colliding with a spec -- because the specs describe the fixed
code, and the fixed code was never in the image.

So, before attributing a benchmark result to anything:

  - **Resolve the image to a COMMIT, not a tag.** Compare the registry's build
    timestamp against `git log --format="%h %ci"`, remembering the registry
    stamps UTC and the commits may not.
  - **Check that a mechanism you are reasoning about is actually running**, by a
    field it emits or by arithmetic on the fields it does emit. A half-wired
    mechanism logs nothing and looks exactly like a mechanism that is working
    and finding nothing to do.
  - A tag that has been reused or hand-incremented (`v1`..`v7`) carries no
    ordering guarantee relative to the branch at all.

### Judge the ramp on the client's TTFT, not the engine's

*2026-09-28, runs U through Z on the 1k6000 -> 8k1000 trace.*

`vllm:time_to_first_token_seconds` starts when a request reaches the engine.
Requests held in the router's flow-control queue have not reached one, so they
cost nothing on it. On a fleet where the router withholds a queue for ninety
seconds -- which is what the sections above are about -- the engine-side metric
is blind to precisely the interval under investigation.

It is worse than blind, because it inverts. Run Z's engine-side p95 by 30 s
bucket:

| time | TTFT p95 | ready | router queue |
|---|---|---|---|
| 20:06:00 | 0.23 s | 1 | 266 |
| 20:06:30 | 33.86 s | 2 | 340 |
| 20:07:00 | 61.74 s | 4 | 513 |
| 20:07:30 | **70.22 s** | 5 | **705** |
| 20:08:00 | 13.21 s | 9 | 0 |
| 20:08:30 | 0.23 s | 9 | 0 |

TTFT is LOWEST when the fleet is smallest and the queue deepest, and peaks as
replicas turn Ready. It is not measuring the shortage; it is measuring the
discharge -- the router releasing a queue into however many engines happen to
be up, where it becomes engine-queue wait and finally becomes visible.

The percentile then compounds it. Over a twenty-minute phase only four samples
exceed 0.25 s, so a p95 across about forty-four samples lands on the
third-highest, which is inside the four-sample spike. The figure reports where
the sampling grid fell relative to the fleet arriving. Read that way, runs X, Y
and Z scored 44.9 s, 60.5 s and 61.7 s -- differences that were attributed to
code and are one measurement.

The harness already publishes the right figure. `analysis/summary.txt` carries
guidellm's own per-request percentiles, measured at the client, which include
the router wait:

    run Z   request latency  median  19.2 s   p95 194.6 s
            TTFT             median 103 ms    p95  34.2 s
            ITL              median 3.7 ms    p95  25.2 ms

So: compare runs on the harness summary, and treat `vllm:time_to_first_token_*`
as an engine diagnostic rather than a verdict. Two practical conditions follow,
both of which bit during these runs:

  - **The harness has to finish.** Four of the six runs hung before writing
    `analysis/` and `metrics/graphs/`, leaving only raw scrapes. A run that
    hangs produces no client-side data at all.
  - **Never run a comparison arm with the EPP at `--v=4`.** Run V did, pinning
    the router at 4.6 cores, and its client TTFT median came out at 244 s
    against run Z's 0.103 s. That is the logging, not the fleet, and it makes
    the run unusable for anything but reading the flow controller's own
    decisions.

### The noise floor of this benchmark

*2026-09-28/29, runs U through AA on the 1k6000 -> 8k1000 trace at 6 req/s.*

Four runs were taken whose only intended difference was one line -- the release
cap measured against ready supply or anticipated supply -- with the same trace,
the same EPP config and a controller restarted before each:

| run | cap against | spec>=9 | ready>=9 | phase-1 median | phase-1 repl-min |
|---|---|---|---|---|---|
| X (v7) | anticipated | +75 s | +135 s | 6 | 121.5 |
| Y (v8) | ready | +105 s | +180 s | 6 | 120.5 |
| Z (v9) | ready + projection | +105 s | +165 s | 9 | 151.5 |
| AA (v10) | anticipated | +90 s | +165 s | 8 | 139.0 |

X and AA carry the SAME cap term and differ by 15 s of ordering, 30 s to Ready,
two replicas of median and 17.5 replica-minutes. Whatever separates them is not
the thing under test.

Phase 2 gives a cleaner read, because two runs produced identical behaviour
there -- both settling at a median of 4 with a peak of 8-9 carried through the
shape-change hold:

    Z  (v9)   19.0 min   peak 9   median 4   116.0 repl-min
    AA (v10)  19.0 min   peak 8   median 4    99.0 repl-min

Seventeen replica-minutes apart, about 15%, on behaviour that matches step for
step.

So, on this trace and this cluster:

  - **ramp timing is good to about +/- 30 s**, no better. A 15-30 s difference
    between single runs says nothing.
  - **replica-minutes are good to about +/- 15%.**
  - **engine-side TTFT percentiles say nothing at all** between runs, for the
    separate structural reason in "Judge the ramp on the client's TTFT".

Every cap comparison attempted on this branch sat at or below those thresholds,
and three of them were claimed and then reverted:

  - a 30 s ramp regression attributed to `d03440a0` change 1 (retired: X is the
    sole outlier of four runs on both ramp metrics)
  - the release cap sized on the projected queue (reverted in 99b84c6e: 25% more
    GPU, no latency change)
  - the cap restored to anticipated supply (reverted in d76930eb: 18
    replica-minutes more than the run it was meant to beat, and it did not
    reproduce the median it was argued from)

The shared root cause is not any of the code. It is that one run per arm was
treated as a measurement. What clears this floor is a repeat per arm, or an
effect large enough not to need one -- the EPP scorer weights moved the phase-2
median from 9 replicas to 3 and serving replicas from 1-of-9 to 9-of-9, which no
amount of this variance explains.

### The mu divisor follows the shape change, not the ramp

*2026-10-03, runs QJ-QN, five passes with the WVA image the only variable.*

The derived mu divides a token rate by an output length
(`rate = tokenSec / avgOutput`), so the output length is where an error in the
shape reaches mu undamped. There are two candidate windows and each is wrong in
the case the other is right for.

A `[5m]` count-weighted mean is wrong **after a switch**: the previous shape's
long stragglers dominate the mean for minutes after they stop arriving.
Measured per decode pod across one 6000 -> 250 output switch, the `[5m]` figure
decayed 3750 -> 2814 -> 2382 -> 2278 -> 2136 -> 250 while the arriving work was
250 throughout; the `[1m]` form converged in about a minute.

A `[1m]` mean is wrong **while ramping**. Phase 1 of the shape-swap trace holds
6000-token generations in flight for about two minutes before any of them
completes, so a mean over COMPLETED requests reads far below 6000. That
over-states mu, and an over-stated mu under-orders replicas.

Bisected over five runs with EPP version, EPP config, `max_num_seqs` and the
cluster held identical. The unconditional short window took phase 1 from a
305-request router queue at 33,071 output tok/s to **2,503 at 8,763** -- eight
times the queue for a quarter of the throughput. Every build before it measured
good.

`max(recent, [5m])` is not the fix, and it is worth recording because it is the
obvious one: the recent figure is the LOWER of the two in both cases -- an
artefact while ramping, the truth after a switch -- so no magnitude test
separates them. Whether the shape changed does.

#### And the divisor must fall back rather than divide by zero

*2026-10-03, run QM, shape-swap phase 1.*

A fleet that has completed nothing has no output length at all, so
`tokenSec / avgOutput` divides by zero and the derived mu reports not-ok. With
no mu the demand floor emits nothing -- and the floor is where the router queue
is projected forward over a replica's start time (`floor.backlogAtLanding`). So
the one window the projection exists for was the one window it could not run
in.

The first queued cycle was 14:39:44 and the first `throughput-demand-floor`
line 14:41:59 -- **135 s later**, by which time the router queue had peaked at
522 and begun draining. Of 56 not-ok `derived-mu` cycles, 38 carried a complete
ITL fit (`itlA` 0.0277, `itlB` 0.0066, `itlAtKPrice` 0.0254) and failed on
`"avgOutputTokens": 0` alone.

A measurement always wins, so a warm fleet is unaffected; the seed answers only
where there was otherwise a zero. The error direction is the safe one for a
seed that is too LARGE: a bigger divisor under-states mu, and an under-stated
mu over-orders during the cold window rather than under-ordering, which is the
failure this exists for.

### Both halves of the shape move to the short window together

*2026-10-04, run QS, 6000/1000 -> 1000/4000 at 6 req/s, the pd-disaggregation
well-lit path whose documented answer is 2 decode replicas then 3.*

`deriveMu` reads the output length twice: once as the divisor, and once inside
the shape, where `KVreq = ILeff + OL/2` decides how many sequences fit. Moving
only the divisor to the short window leaves the two halves of one division on
different timescales, and a swap that raises the generation while dropping the
prompt is the worst case for it -- the divisor reaches the new output in about
a minute while `KVreq` still carries the old prompt.

With the divisor alone on the short window, across the switch:

| time | mu | divisor | kvReq |
|---|---|---|---|
| 08:55:42 | 5.47 | 1088 | 6251 |
| 08:55:57 | 2.12 | 2897 | 6030 |
| 08:56:12 | **1.57** | 4000 | 5896 |
| 08:58:13 | 2.05 | 4000 | 4146 |
| settled | 2.76 | 4000 | 3000 |

The floor read `replicasImplied` 5.77 and the fleet sat at **7 decode replicas
for about seven minutes**. The router queue was ZERO on every one of those
cycles, so none of it was a backlog response -- it was the price. A request
priced at the new generation on top of the old prompt is larger than either
real shape: 5017 + 4000/2 = **7017** against a phase-1 6500 and a phase-2 3000.

Paired, the same cycle prices `KVreq` 3000 and mu 3.20. Cold-to-cold against
QS, the same trace on the paired build (run QT, 2026-10-04) held decode at a
phase-2 rate of 2.91 replicas against QS's 4.49, peak 3 against 7. The cost is
real and stated: phase-2 client TTFT went from p50 53.5 / p90 68.9 ms to
58.9 / 86.6, which is the price of running the documented 3 replicas instead of
up to 7.

**Both halves or neither.** If an engine publishes the short-window output but
not the short-window prompt, the divisor still moves alone -- that is this
mismatch, deliberately accepted, because the alternative is the straggler bug
above and that is the larger error by an order of magnitude (3750 against 250).
vLLM and SGLang both publish the pair, so the single-sided path is the
engine-has-no-counter case, not a race.

## How to add to this file

One section per decision, with the date, the run identifier and the numbers
the decision rests on. Put the rule in the code and the evidence here, and
link from the code to the section rather than restating it: a comment that
carries a measurement has to be re-verified every time the code around it is
edited, and in practice is not.
