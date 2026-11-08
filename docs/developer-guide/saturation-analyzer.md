# The saturation analyzer

How `internal/engines/analyzers/saturation` decides, with every formula it
uses and where each one lives.

The package is laid out by what each file computes: `analyze.go` runs one
cycle, `replica_capacity.go` sizes a replica, `fleet_shape.go` averages the
fleet, `queue_demand.go` prices unstarted work, `keys.go` files readings,
`aggregate.go` sums them, `roles.go` holds the P/D predicates, and
`analyzer.go` is the analyzer itself.

This is the component page. Two neighbours cover parts of it in more depth and
are not repeated here:

- [The throughput floor](saturation-demand-floor.md) — why the floor exists and
  what it is not invariant to.
- [Analyzer evidence](analyzer-evidence.md) — the measured runs behind the hold
  rules, the windows and the arrival rate.
- [Which component owns which number](signal-dataflow.md) — the same formulas
  arranged by provenance: which component produces each parameter and which
  consumes it, with the component diagram.

`throughput-analyzer.md` is a **different** analyzer
(`internal/engines/analyzers/throughput`). It shares vocabulary and almost no
code.

## What the analyzer produces

One `*allocation.NamedAnalyzerResult` per model per cycle. The optimizer reads
four numbers from it, and everything below exists to compute them:

| field | meaning |
| --- | --- |
| `Result.TotalDemand` | tokens of work the model is being asked to hold |
| `TotalSupply` | tokens its **ready** replicas can hold |
| `TotalAnticipatedSupply` | tokens its ready **and starting** replicas can hold |
| `RoleCapacities[role]` | the same three, per P/D role |

`RequiredCapacity` and `SpareCapacity` are **not** the analyzer's. They are
recalibrated downstream — see [RC and SC](#rc-and-sc).

## One cycle, end to end

`SaturationAnalyzer.Analyze` (`analyze.go`):

1. Average the fleet's shape — `fleetOutputLength`, `servedPromptLength`,
   `fleetPrefixHitRate`.
2. Note the shape against the per-model tracker; decide whether a change is
   outstanding ([Shape change](#shape-change)).
3. Resolve the generation length the queue is priced at, and the shape `mu` is
   priced at ([The service rate](#the-service-rate-mu)).
4. Fit one ITL line per decode or aggregated variant ([The ITL line](#the-itl-line)).
5. Per replica: compute `k1`, `k2`, the effective capacity, and `mu`
   ([Per-replica capacity](#per-replica-capacity)).
6. Price the scheduler queue and attribute it to a role
   ([Demand](#demand)).
7. Aggregate to variant, role and model totals ([Aggregation](#aggregation)).
8. Apply the throughput floor ([The throughput floor](#the-throughput-floor)).

## The shape

`internal/signals/shape`. A workload's shape is the pair (input length, output
length) plus the prefix-cache hit rate, and two derived figures.

```
hitRate  = clamp(hitRate, 0, 1)        // NaN -> 0
ILeff    = IL x (1 - hitRate)
D        = E[O^2] / OL^2               // the spread of output lengths, >= 1
KVreq    = ILeff + D x OL/2
```

`ILeff` is the prompt the engine must actually compute — a prefix-cache hit
costs nothing. `KVreq` is the KV footprint of **one resident request**, averaged
over the time requests are resident: it holds the whole prompt throughout, and
its generation grows from nothing to its length.

When every request generates `OL` tokens, the generation contributes its mean,
`OL/2`, and `D` is 1. When lengths vary, a long request stays resident longer,
so a snapshot of the batch finds it in proportion to how long it stays, and the
average resident request holds `ILeff + E[O^2]/(2 OL)` (issue #276). With `D`
taken as 1, the resident requests were over-counted by 7% for uniform lengths,
20% for exponential ones and 44% for a lognormal with cv 1.5, at `IL = 2 OL`.

`D` is read from the engine's output-length histogram over the same `[5m]` as
`OL`: vLLM's `vllm:request_generation_tokens_bucket`, and SGLang's
`sglang:generation_tokens_histogram_bucket`, which SGLang publishes behind the
`--collect-tokens-histogram` its `OL` needs too. The buckets bound `E[O^2]`
rather than give it, so the collector takes the least `E[O^2]` the buckets and
the exact mean allow (`domain.SquaredMeanLowerBound`,
`ReplicaMetrics.AvgOutputTokensSquared`): every bucket's requests at one length,
a water level clamped into each bucket's bounds. The true `E[O^2]` is never
below it, so `D` moves `KVreq` towards the truth and never past it, and equal
lengths read `D = 1`. On vLLM's 1-2-5 buckets it recovers about half of the
spread of a smooth distribution, and all of it when lengths sit on the buckets'
upper bounds (a bucket's least length is taken as its lower bound, not one
above it). A mean up to 1% outside what the buckets allow (rate extrapolation)
is clamped onto them, and the bound is returned against the caller's own mean,
so the clamp does not inflate `D`.

`D` is 1, and `KVreq` is `ILeff + OL/2` as before, when:

- any generating replica lacks the histogram (an engine that does not publish
  it, a relabelling that drops it, or a histogram refused as not one reading:
  no `+Inf` bucket, a mean outside the buckets): its lengths would otherwise
  count as all one length beside the others' spread (`fleetOutputDispersion`).
  This sets the fleet's `D`, which the mu and the zero-replica estimate read;
  the derived k2 of each live replica reads that replica's own `D`, so on a
  mixed fleet the replicas that read the histogram keep theirs in k2;
- a shape change is outstanding (the window `shapeChangeHoldSeconds` opens),
  on every path: the `[5m]` histogram then holds both shapes, and their
  difference would read as a spread of one workload's lengths. At IL 2000 and
  1000 -> 100 out, half way through, it reads `D` 1.67 and prices k2 20% high;
- the replay switch `kv-per-request-output-dispersion`
  (`SwitchKVPerRequestOutputDispersion`) is off.

The fleet's `D` is request-weighted, as `OL` is. The derived k2 of a live
replica uses the replica's own `D`, from however many requests it completed in
the window: the ratio is biased low on few, so a quiet replica's k2 errs low
(more replicas) and moves from cycle to cycle. A zero-replica variant's
estimate multiplies the model's plain mean output length by
`modelOutputFactor = E[O^2] / (E[O] x plain mean)`, so its generated half is
the fleet's request-weighted `E[O^2] / (2 E[O])`; the request-weighted `D`
times the plain mean mixed two populations and priced up to 42% too much
capacity. The factor is under 1 when the request-weighted mean is well under
the plain one (99 requests a second of 100 tokens beside 1 of 1000 read 0.33),
and is then priced as 1: the estimate never moves below the `I + O/2` it had
before, which in that case is the old over-statement, not a new one. The log
carries the factor as computed. `derived-mu` logs `D` as `outputDispersion`, `k2-decision`
likewise, and `zero-replica-capacity-estimate` the factor as
`modelOutputFactor`. The slo analyzer prices `D` too, behind
`slo-kv-per-request-output-dispersion`, and the throughput analyzer, behind
`throughput-kv-per-request-output-dispersion` (#433). The slo analyzer has no
shape-change hold; the throughput analyzer holds `D` at 1 for the same window after its
variant's shape changes. All three read `D` through `fleet.OutputDispersion`, which
refuses a bound of +Inf or NaN.

### Prefix-cache hits stay resident

`ILeff` is right for compute and wrong for memory (issue #277). A hit saves KV
only when the block it hits is held by another running request too. A hit on a
block that a finished request left behind makes that block the new request's
own, so it stays resident as if it had been computed. With multi-turn reuse
most hits are of that kind, and `ILeff + D x OL/2` then over-counts the
requests a cache holds, by up to about 4x. The derived mu sizes its sequence
count by that figure, so it over-stated mu and ordered too few replicas.

The engine reports how many requests it is running
(`vllm:num_requests_running`, `sglang:num_running_reqs`,
`ReplicaMetrics.RunningRequests`). Where those are read, the derived mu and
the GPS check price a resident request at the KV the requests are measured to
hold, instead of the assumed discount (`prefix_cache_kv.go`):

```
measured = sum(KvUsageInstant x TotalKvCapacityTokens) / sum(RunningRequests)
KVreq    = clamp(measured, ILeff + D x OL/2, IL + D x OL/2)
```

The sums run over the Ready generating replicas that report both readings and
whose KV fill is at least the ITL band's lower edge,
`analyzerTuning.itlBand.minObservableK` (`fleetResidentKVPerRequest`).
Warm-pool bridges and replicas with no live capacity are left out.

A reading is taken only where there are enough requests in it to describe
the pricing point. A prefix that several running requests share is held once,
and the reading spreads it over the requests running now. Take 500 shared and
500 unique prompt tokens with 100 out, at hit 0.5. The shape says 550 a
request. A replica running 123 requests reads 554. A replica running 2 reads
800, and priced at that the KV-bound mu would be 31% low. Two gates, both at
the ITL band's lower edge, `minK`:

- **By fill:** only replicas whose KV fill is at least `minK` are read. The
  band is where the line mu is priced from is fitted.
- **By count:** the requests read must be at least `minK x C / KVreq`, where
  `C` is the summed capacity of the replicas read and `KVreq` is the shape's
  own, with the discount. That is the band's edge counted in requests. The
  fill gate alone does not stop long prompts against the cache. A
  500,000-token cache with 100,000-token prompts, 90% of them shared, holds 2
  requests at a fill of 0.22. Those read 55,500 a request where the shape says
  10,500: a KV-bound mu 5x low. The count gate asks for 8.

The count gate costs the issue's own case some of its reach. Where hits are on
blocks no running request shares, a request holds `IL + D x OL/2`, so the
count gate passes only from a fill of `minK x (IL + D x OL/2) / (ILeff + D x
OL/2)`:

| Discount on the KV per request | Fill at which the count gate passes |
|---|---|
| halves it | 0.30 |
| quarters it | 0.60 |
| cuts it more than about 4.5x | above the pricing point (0.68 on the defaults) |

Below that fill the price stays at the discount, as before. A line in the
running count that separates shared KV from each request's own would read
both cases; that is a follow-up.

The noise a reading keeps is one-sided. A reading below the shape's `KVreq` is
clamped to it and changes nothing, while a reading above it moves the price.
The spread of prompt lengths is not modelled, so long prompts read over few
requests err towards more replicas, at most up to the price with no discount.

The clamp keeps the reading to the hit term:

- Below the lower bound: requests part way through their prefill. Not taken as
  more sharing than the hit rate allows.
- Above the upper bound: the prompt length lags the workload. Or, on the decode
  side of a P/D split, vLLM has allocated KV for requests still pulling their
  prompt's KV from prefill. Those requests wait, so they are not counted as
  running. The reading then moves towards no discount. No gauge counts those
  blocks, so they are not subtracted.

With no reading, the price stays `ILeff + D x OL/2`, as before. No reading
means no replica in the band, a missing running-requests or KV series, or
fewer requests than `minResidentSequences` or the count gate asks for. So the switch moves the
price only where there is evidence, and only towards more KV: the derived mu
is never above the one before.

The fewest requests depends on the shape. Each request is caught at its own
age, so its generated part varies by about `D x OL / sqrt(12)`, while the
prompt is the same at every age. The fewest is the count that brings the
mean's spread within 5% of `IL + D x OL/2`, and at least 2. At `IL = OL`
that is 15; at `IL = 10 OL`, 2. A fixed count large enough for the first
would never be reached by one or two whole-node pods serving long prompts.

During a shape change the reading is kept. Each shape priced bounds it by its
own `ILeff` and `IL`, and the short-window shape is the new one. So a mixed
population of old and new requests moves the price no further than between
the new shape's discount and none.

SGLang publishes both readings as the busiest rank's (`max by pod`), against
the pod's whole KV capacity. On a pod running data-parallel attention ranks,
the reading would over-read by about the rank count and land on the upper
bound, which means more replicas, at most the price with no discount. This has
not been checked on a cluster.

The prompt that prefill's throughput key is discounted by (`throughputKeyInput`)
keeps `ILeff`, because a hit does save prefill compute. The derived k2 and the
zero-replica estimate already price the full prompt. `derived-mu` logs the
reading as `residentKVPerSeq`, the requests it was taken over as
`residentRunning`, and `residentKVBound`: `none` (priced as read), `lower`,
`upper`, `no-hit` (a shape with no hit, whose bounds are one figure),
`no-reading` or `off`. `residentKVBoundFleet` is the same for `kvReqFleet`,
the fleet's own shape, which differs from the priced one during a shape
change. The replay switch is
`kv-per-request-hits-resident` (`SwitchKVPerRequestHitsResident`). The
throughput and slo analyzers still price their KV per request at `ILeff`.

`KVreq` is the single most load-bearing quantity in the analyzer. Everything
that asks "how many requests fit" divides by it.

Two shapes are the same shape when both axes are within tolerance:

```
Within(a, b, tol)  =  |a.IL - b.IL| / b.IL <= tol
                  and |a.OL - b.OL| / b.OL <= tol
```

`shape.DefaultChangeTolerance = 0.20`.

## Per-replica capacity

`computeReplicaCapacity` in `replica_capacity.go`. A replica's capacity is a number of
**tokens**, and it is the smaller of two bounds.

### k1 — memory bound

```
k1 = floor(rm.TotalKvCapacityTokens x kvCacheThreshold)
```

`memoryBound()`. The usable fraction of the KV cache; `kvCacheThreshold`
defaults to `0.80`. The remaining fifth is headroom — an engine driven to a
full cache preempts and thrashes.

### k2 — compute bound

`estimateCapacityAtCapSpread()` in `replica_capacity.go`, from the engine's own parameters
(`capacity.EngineParams`, parsed from the deployment):

```
B = EffectiveMaxBatchedTokens
S = MaxNumSeqs
I = avgInputTokens
O = avgOutputTokens

N_steady = min(B x O / (I + O), S)
k2       = floor(N_steady x (I + D x O/2))
```

`N_steady` is how many requests a continuous-batching engine holds in steady
state: each step spends `B` token-slots, a request needs `I` of prefill once
and `O` decode steps, so the fraction of a request's life that is decode is
`O/(I+O)` and the batch sustains `B·O/(I+O)` of them — capped by the engine's
own `--max-num-seqs`. Multiplying by `(I + D x O/2)` converts a count of
requests into the tokens they occupy, the same per-request footprint as
`KVreq`, with the same spread `D` (the replica's own; see the shape above).
`N_steady` is a throughput over the token budget and keeps the means.

The spread moves the two figures it enters in opposite directions, both
towards the truth: `k2` rises (the sequences the batch cap admits hold more KV
each, so a replica holds more before its batch binds, and fewer replicas are
ordered), and the derived mu falls where the KV and not the batch cap binds
(fewer sequences fit at `kPrice`, so more replicas are ordered).

`S` is the batch cap `pricingBatchCap` (`batch_cap.go`) hands `computeK2`,
and the `k2-decision` line names its source (`batchCapSource`):

| source | `S` | when |
|---|---|---|
| `flags` | `EngineParams.ExplicitMaxNumSeqs()` | the flags state a cap |
| `declared` | `EngineParams.DeclaredBatchCap`, the scale target's `llm-d.ai/max-num-seqs` annotation | the flags state none (`SwitchBatchCapDeclared`) |
| `learned` | `batchCaps[namespace\|variant\|model\|].perRank` | the flags state none, none is declared, the policy sets `enableBatchCapLearning`, and it is lower than the default (`SwitchBatchCapLearned`) |
| `default` | `EngineParams.MaxNumSeqs`, the parser's 256 | otherwise |

The cap's other readers (issue #420) each sit behind their own replay
switch. The ones that price read the declared cap, and saturation's also
the learned one, except the zero-replica estimate. The ones that ask
whether a replica is AT its cap read the declared cap and never the learned
one (`atCapBatchCap`):

| reader | reads | switch |
|---|---|---|
| ITL admission at the cap (`itl.Admission.MaxNumSeqs`, `fitLines`) | `atCapBatchCap`: flags, else declared (only while `SwitchBatchCapDeclared` is on too), else 0; `itl-window` logs `batchCap` and `batchCapSource` (`none` for 0) | `SwitchBatchCapITLAdmission` |
| P/D decode view's at-cap exception (`decodeCapacityViews`, `atCapBatchCaps`) | `atCapBatchCap`, as above | `SwitchBatchCapDecodeView` |
| derived mu's sequence cap (`whatif.DeriveMuAtCap`) | `pricingBatchCap`: flags, declared, learned, default; `derived-mu` logs `batchCap` and `batchCapSource` | `SwitchBatchCapDerivedMu` |
| zero-replica estimate (`estimateStoredCapacity`, issue #427) | `zeroReplicaBatchCap`, per engine rank: flags, else declared (only while `SwitchBatchCapDeclared` is on too), else default; never learned. A prefill variant keeps flags, else default: P3 never prices prefill. The estimate is one rank's figure, also for a data-parallel variant (#438). `zero-replica-capacity-estimate` logs `batchCap` and `batchCapSource` on its `deployment-derived` line | `SwitchBatchCapZeroReplica` |
| `slo` analyzer's admission (`slo.admission`) | `EngineParams.StatedBatchCap`: flags, else declared (not gated by `SwitchBatchCapDeclared`, a saturation switch); `slo-variant` logs `lineBatchCap` and `lineBatchCapSource` | `slo.SwitchBatchCapAdmission` |
| `slo` analyzer's per-rank cap (`priceVariant`) | `StatedBatchCap`, else `BatchCap`; `slo-variant` logs `batchCap` and `batchCapSource` | `slo.SwitchBatchCapPerRank` |

Why the at-cap readers do not read the learned cap: it is the highest
plateau seen, so it may sit below the true cap. Below the true cap vLLM
still promotes a P/D decode's landed pulls, so the decode view would charge
in-flight pulls as backlog. `observeBatchCap` reads the view, so the
charged queue would then confirm the very cap that caused it. And the ITL
fit would admit a memory-bound reading above the band as capped. In mu and
P3 a learned cap only lowers the price, so it only errs toward more
replicas.

The `slo` analyzer does not read the learned cap: it lives in the saturation
analyzer's memory. A capped mu is still priced at `ITL(kPrice)`,
although a replica held by its cap runs below `kPrice` and so faster: mu
reads low, toward more replicas (the `slo` analyzer's `whatif.ReachableFill`
accounts for this; saturation does not).

With no `EngineParams` at all (the scale target was never read), a learned cap
still prices P3, with `B` taken as not binding (`estimateCapacityAtCapSpread`);
`min(k1, k2)` keeps that at or under the k1 it replaces.

The learned cap (`observeBatchCap`, issue #399) is the max, per engine rank,
of the running counts a pod held for `BatchCapPlateauCycles` (3) cycles in a
row, every cycle passing `batchCapRules`: serving (`admission.serving`), not
joined under `JoinedReplicaWindow`, a queue on the busiest rank of at least 1,
a decode row's queue reason known (`CapacityQueueKnown`), a data-parallel row's
busiest rank's KV within `BatchCapRankSpread` (10%) of the mean (the running
count is summed over ranks, so the sample is the mean per rank), a running count
known, `BusiestRankTokens` under k1, preemption known and zero. A running
count can never exceed the cap, so every sample is at or below it and the max
only rises toward it; each rule guards against a sample below it. A refused
cycle ends the pod's plateau. The cap is reset when the variant's `windowArgs`
change, swept after `BucketRetention` with neither a plateau nor a use (`pricingBatchCap`
stamps `usedAt`: it ages on last use, so a fleet it provisions, which stops
queueing, keeps it), and not in the learned-state
snapshot. A flow-controlled fleet whose engine queues read 0 gives no sample:
the router judges the pool, not the pod, so its queue is not evidence that any
one pod is full. Nor does a P/D decode at its cap: the rows are
`decodeCapacityViews`, and vLLM v0.20.0+ reports a capped decode's waiting
requests deferred, so the view's queue is 0 (`pd_decode_queue.go`;
`TestBatchCap_PDDecodeAtCapLearnsNothing`). The declared cap covers it.
Prefill rows are skipped, since P3 never prices prefill. The property cases
are `*/batch-cap/*` (run them with
`-run 'TestLearnerProperties/.*/batch-cap/.*'`), and `TestBatchCapMutants`
drops each rule in turn.

`B` and `S` bound one engine. A replica running several data-parallel ranks
runs that many engines, so `computeK2` multiplies the derived k2 by the ranks
the row reports (`ReplicaMetrics.Ranks`), and `deriveMu` caps the sequences at
the ranks times `S`. k1 needs no scaling: the replica's KV is already all of
its ranks'. See
[Data-parallel pods](signal-dataflow.md#data-parallel-pods).

`computeK2` has a priority order (observed, historical, derived, fallback);
`k2Source` in the `replica-capacity-decision` log line says which answered.
k2's persistence is deliberate and load-bearing — see
[analyzer-evidence.md](analyzer-evidence.md).

Because a recorded k2 persists, the observed priority admits a reading only
when it is a replica at its limit (`k2Unsettled` in `k2_settle.go`, the strict
admission `docs/plans/analyzers/k2-capacity-model.md` calls for). A reading is
suspect when the busiest rank's queue, at the request's footprint, is larger
than everything the rank holds:

```
joined   = later of Pod Ready and its first sight (else first sight) under
           JoinedReplicaWindow (1m) ago, read from the settle tracker,
        || the first return of load after an idle run in its current
           settle, under JoinedReplicaWindow ago (joined_served.go)
suspect  = queue x (I [+ O unless prefill]) > tokens / ranks
admitted = !joined && (!suspect
        || (rate >= K2SettleRateFraction x rateOnRecord             // 0.7
            && tokens >= K2SettleRateFraction x k2OnRecord)         // k2OnRecord > 0
        || settledOverWindow: the newest earlier reading at least
             K2SettleLookback (1m) old, it and every reading since queued
             with no gap over 1m, and
             (1 - K2SettleMaxRise) x tokens then
                 <= tokens <= (1 + K2SettleMaxRise) x tokens then)   // 0.25
```

The joined test reads the engine's settle tracker (`internal/signals/settle`,
`k2Joined` in `settle_view.go`): the analyzer takes a read-only view at
construction (`WithSettleView`) and never writes it. A replica waits from the
later of its Pod's Ready time and the controller's first sight of that Ready
time, so a restart under the same name (an engine or LWS group restart: the
row is the group leader's, with the latest Ready time over the leader and its
Ready workers, so a worker restarted alone under `restartPolicy: None` counts
too) waits again, a
node clock behind the controller's does not shorten the wait, and after a
controller restart every replica waits once, unless the snapshot gave the
tracker its sighting (`settle.Tracker.Restore`, `SwitchFirstSightRestored`) and
its first row is Ready with the same Ready time. A replica unseen for longer than
the tracker's absence tolerance (1 min, or 2.5 optimize intervals) is a new
sighting. An analyzer given no view, as a unit test builds one, observes a
tracker of its own and takes its first cycle's replicas as already watched.
The two changes from the analyzer's own memo of Ready times are replay
switches (`SwitchK2ReadsSettle`, `SwitchK2WaitsAfterRestart`).

The joined clock also starts when a replica is first routed to
(`joined_served.go`, #392, `SwitchJoinedFromFirstServed`). A replica Ready for
longer than a minute but idle until a burst reaches it has a rate[1m] that
covers only the part of the minute it served, and that reading read as a low
mu. What the replica did is read from the settle tracker, by role
(`settle.Row.Activity`): a row is idle only with its KV known and empty and no
token generated (prefill: no request completed). A replica holding KV is never
idle, so a full, queued replica with no completion yet is unknown, not idle.
The first row of load after an idle run is a return (`settle.Replica.Returns`);
the tracker keeps the returns of the current settle, which begins at the Pod's
Ready time, a restart, a rejoin, a rollout or a lend. A saturated reading under
`JoinedReplicaWindow` of the settle's FIRST return is refused as a joined one,
with its own reason (`P1-obs-unsettled`). The analyzer keeps no per-pod state
of its own for it.

Only the first return of a settle holds, so a replica is protected once per
settle. It is the replica first routed to after it became Ready, or after the
controller first saw it idle: the case of the issue. A replica that idles every
night and takes a morning burst is held on its first morning only; on later
mornings its first partial rate[1m] is admitted, as before #392.

The narrower-sounding rule, hold any return whose idle run left idle seconds in
the reading's window, is the same as holding every return: a return is the
first load row after an idle row, so unless rows were missed between them the
minute after it holds idle seconds. Holding every return was measured on the
pd-restart golden, whose decode Pods idle and return as the load steps.
Decode's mu was unpriced from 12:27:02 to 12:28:32, then read 5.06 and 4.65
req/s until 12:46:50 against the 6.21 the first-return rule prices (-25 %); its
k2 was 48 005 against 55 212; e1-tuned and ttft-staircase-b-off moved too. The
first attempt (f32a32ed7, reverted), which held every return for mu alone, left
decode's mu 27 % low on the same golden. The first-return rule holds one of the
two decode Pods there (the other had returned at 12:13:44) and moves decode's
mu toward its steady figure a cycle sooner.

k2 is refused with mu, not mu alone, and that costs valid k2 readings. Mu is
written only beside a k2 write, and k2's settle rate test reads the mu on
record: refusing mu alone left that test with no mu (f32a32ed7), and the k2
readings it then admitted wrote ramp readings as mu. But a max_over_time[1m]
KV peak is not lowered by the idle part of the minute, so the k2 reading of a
full replica in that minute is right, and it is lost. On the e1 golden,
model-02's variant-01 saturates only in the minute after its first return,
holding 363 871 tokens, so it never records an observed k2 and prices from the
derived figure (its capacity does not move). The cost in time is one rate
window (4 cycles at 15 s) before a late-routed replica's first k2 and mu, and
one cycle more for k2's first reading (`k2_confirm.go`).

Each cycle k2 also fills a `learn.Report` per variant (`k2_learner.go`;
`LearnerReports`), and logs `learner-write` at `--v=3` for a key whose figure
was set for the first time, moved more than `K2WriteBand` (2 %) from the value
last logged, or has not logged for `LearnerHeartbeat` (10 min). The
forget-learned-state annotation reaches k2 through `learn.Learners`.

The engine takes the cycle's reports once, at the end of the cycle
(`publishLearnerReports`, `TakeLearnerReports`, on every built-in analyzer: the
slo and throughput lines report under `learn.SLO` and `learn.Throughput`), and
publishes each learner's
`wva_learned_state_last_admitted_timestamp_seconds`. `RecordLearnerReports`
remembers the key behind each series, which no label carries, and deletes the
series only when a report without a `PricingKey` names that key `Forgotten`,
or when the variant is really gone (`retainLearnerSeries`). The active set is
not the test: a variant at zero replicas is in no model group, and its series
must outlive the park. Nor is the registry alone
(`utils.KnownVariantAutoscalings`): a KEDA outage past `registry.DefaultTTL`
empties it, and a trigger `ParseMeta` refuses leaves it. Each series records its
scale target at publish time, and a variant missing from the registry goes only
when a read of that target says NotFound, or finds it being deleted. Any other
answer, Forbidden included, keeps the series. The read is one `Get` through the
controller's client with no backoff, at most once per variant per
`learnerTargetCheckInterval` (1 min), and `learnerSeriesMissingMax` (24 h) ends
the wait whatever the reads say.

`RollingAverage.Restored` is true from `RestoreRollingAverage` until the
window's first write, and `itl.Tracker.Restored` from `RestoreBaseline` or
`RestoreSlope` until the window keeps a reading. The mark lives with the
figure, not in a map of the analyzer's, so a stash, a forget and an eviction
carry or drop it with no rule of their own. It is not stored in the snapshot.

A replica that joined the fleet under a rate window ago is filling, whatever
its queue: its one-minute gauges still hold the minute before it was routed
to. On the E1 run (2026-10-23) one just joined read 84 waiting on 174,327
resident, the queue no deeper than its batch, and was recorded on sight; the
window settled at 264,265 while every replica then held 331-351k with no queue.
The age is read from when the Pod last became Ready, as the collector reports
it in `ReplicaMetrics.ReadySince` (`replicaReadySince`, read through
`SaturationAnalyzer.readySince` so tests stand in for the collector). A pod
back from an engine or LWS group restart under the same name has a new Ready
time: in review one absent for 75 s kept its old first-seen time and its
filling reading (174,327 tokens, 1.0 req/s) became k2 and mu
(`TestReadySince_ARejoiningPodIsAsFreshAsANewOne`).
Where the time is unknown, `readySince` in the settle memo is when the analyzer
first saw the replica Ready. A replica already Ready on the memo's first cycle
then has an age the analyzer cannot know and is taken as established, so a
restart does not hold every reading back; one unseen for over `K2SettleMaxGap`
has rejoined.

`I` and `O` are the replica's own lengths, or the fleet's where it has none. A
replica that has completed nothing reports no lengths (the per-pod mean is
0/0), and a footprint of zero made every queue on it the overflow of a batch
"at least as large", so the test admitted exactly the replica it is for.

The band is two-sided. A fall is not settling: a replica full and queued at
270,539 that next reads 31,713 under 160 queued has had its traffic moved off
and handed back, and admitted on the rise test alone it took the window to
222,774 (imbalance audit, 2026-10-23).

The band is measured a window back, not a cycle back. Readings are one-minute
peaks every 15 s, so consecutive ones share 45 s and a 25 % band per cycle
allows 2.4x a minute. A batch capped by `max_num_seqs` on long outputs fills
for minutes: replaying the statistics review's simulated traces
(`testdata/k2-fill`), the cycle-back test first recorded 0.624 of the steady
figure on 1000/4000 and 0.489 on 1000/6000 at +37 s; a window back records
1.042 at +82 s and 1.053 at +127 s. The cost is refusals at a true limit with
a noisy gauge: 0.8 / 18 / 52 / 67 % at CV 0.1 / 0.2 / 0.5 / 0.8, and the first
k2 still comes within 13 / 24 / 40 cycles at worst for CV 0.2 / 0.5 / 0.8
(`TestK2Settle_ANoisyLimitStillLearns`).

The rate path needs residency too. A rate shows a replica is busy, not that it
is at its limit: a sibling already serving 1.5 req/s against 2.0 on record,
handed the hot queue (25 waiting on 160,000 resident), passed on rate alone
and pulled the k2 mean 250,000 -> 232,000. With no k2 on record only the
window test admits.

`rate` is the replica's own `saturatedCompletionRate`; `rateOnRecord` is the
bucket's measured saturated throughput, read without touching the window, or
the derived mu; `k2OnRecord` is the bucket's k2 window mean, also read without
touching it. A refused reading logs `P1-obs-unsettled` and writes neither the
k2 window nor the throughput window. Each replica's readings over
`K2SettleMaxGap` (2m) live in `k2Settle`, one memo per `namespace|model`, swept
beside `hotRankHeld`. A refusal can only keep k2 where it was; nothing here
raises a recorded k2.

A reading that passes and would be the first in an empty window is held, not
written (`k2_confirm.go`, #392): it logs `P1-obs-unconfirmed` and waits in
`k2Pending` under its key with the engine arguments it was read under. The
next cycle's admitted reading (a later `Analyze`, counted by `analyzeSeq`; a
sibling in the same cycle does not count) writes the held readings, at the
time they were read, and its own. That is averaging, not an agreement test,
and with one-minute peaks read every 15 s the second reading shares most of
the first's minute. Held readings under other arguments, or past
`EpisodeGap`, are replaced; the sweep drops them past the gap, and a forget
drops them. While held:

- the cycle is priced by `unadmittedPrice` at the lower of the reading and the
  chain (P2/P3/P4 can be k1, or a window from before the gap), and records
  nothing;
- the settle test reads the held readings' mean as `k2OnRecord`
  (`heldOnRecordLocked`), ahead of a window past the episode gap that still
  holds its old figure, so the next reading is judged as against a window of
  them;
- the mu written beside a `P1-obs` reading is judged as usual.

Held readings dropped for other arguments or the gap log `k2-held-dropped` at
`--v=3`. They are not in the learned-state snapshot: a controller restart
during the held cycle drops them, and the next admitted reading is held again.
The replay switch `SwitchK2FirstReadingWaits` turns the hold off.

### Which bound applies

```
effectiveCapacity = k2  if k2 < k1
                    k1  otherwise
```

Logged as `boundBy: "k1-memory"` or `"k2-compute"`.

## The ITL line

`internal/signals/itl`. Inter-token latency rises with KV occupancy, and over
the observable range it rises linearly:

```
ITL(k) = A x k + B
```

where `k` is KV usage (0–1) averaged over the same minute as the ITL rate
(`rm.KvUsageForITL()`: `KvUsageMean`, from `avg_over_time[1m]`, or
`KvUsageInstant` when that was not read; issue #393). Fitted by ordinary least
squares over a rolling window of `(k, ITL)` observations:

```
A = (n x SUM(k x itl) - SUM(k) x SUM(itl)) / (n x SUM(k^2) - SUM(k)^2)
B = (SUM(itl) - A x SUM(k)) / n
```

A fit is refused unless it is usable (`ValidModel`): `A` and `B` finite,
`A > 1e-12` (a flat or falling line is not this physics), and
`A x 0.85 + B > 0`.

### Which roles fit a line

`itlLineRole` (`itl_aggregated.go`) decides which variants fit a line:
decode always, an aggregated variant (`RoleBoth`, or no role label) with
`SwitchITLFitsAggregated` (`itl-line-fits-aggregated`, issue #432) on, and
prefill never. Before #432 only decode fitted, so an ordinary unlabeled
deployment had no line at all. The same rule gates the derived mu
(`priceReplicas`), the service-rate signal (`attachServiceRates`, which reads
`itlModels`), the GPS check (`noteLineMismatch`) and the learner report's
role. The settle tracker, the ITL band and the cap admission were never
role-gated beyond prefill and need nothing. What stays decode-only is
P/D-specific: the decode view of the queue (`pd_decode_queue.go`), the
settled-idle count that relaxes prefill's hold (`idle_room.go`) and decode's
saturation flag.

An aggregated line is **reported** until it has earned sizing
(`itlLineReportOnly`; #432 reported it only). `priceReplicas` derives its mu
and logs it (`derived-mu` with `reportOnly=true`), then prices the replica
with no derived figure (`derivedWhyReportOnly`): no order, no settled shape
change and no borrow record rests on it. The figure reaches
`wva_service_rate_per_replica` as `source=derived-report-only` when the
variant has no other rate. Three things stood in the way of sizing, and
`itl_aggregated_shape.go` handles each (issue #275,
[the proposal](../proposals/aggregated-itl-shape.md)):

- Shape dependence. An aggregated pod's ITL carries the chunked prefill it
  runs between decode steps, about `n*IL/OL` prompt tokens a step, so the
  slope at shape `g = C*IL_eff/(KVreq*OL)` is `A + c*g`. Every line carries
  the `g` it was fitted at (`lineShape.g` on `learnedLine`, `aggState.lineG`
  for a held slope); `noteAggregatedShape` drops the window
  (`itl.Tracker.ClearWindow`) when `g` moves past `aggShapeTolerance` for
  `aggShapeMoveCycles` cycles, so no fit mixes two shapes; `g` reads the
  variant's prompt-weighted hit rate (`promptWeightedHitRate`), so a
  scale-up's cold replicas barely move it. `c` is fitted from the slopes of
  two or more shapes (`lineShape.points`, `fitShapeTerm`).
  `resolveAggregatedPrice` prices a shape at or below the line's own `g` at
  the line's slope (never lower: `c >= 0`), any heavier one through `c` when
  it is identified, one up to 20% heavier at the worst case `A_g*g*/gFit`
  when it is not (`shape-worst-case`: `c <= A_g/gFit` since the decode
  slope is not negative), and a heavier one not at all (`shape-unpriced`).
  A lifted figure is published as `source=derived-aggregated`.
- Prefill occupancy. A sequence being prefilled holds KV and emits no token.
  `noteOccupancy` reads the prefill steps per prompt token (`sigma`) by
  Little's law, `RunningRequests/(RequestRate*ITL) - D` over `IL_eff`, with
  `D = OL/(1+s)` the decode steps (`s` the draft tokens a speculating step
  accepts), refusing a residence under `D` (`aggState.refused`);
  `occupancyFactor` scales the derived rate by `D/(D + sigma*IL_eff)`. An
  SGLang variant and a speculating one with no accepted-token count are
  blocked (`aggState.blocked`) and stay reported only.
- The lift. `noteAggregatedLift` lifts a variant once its GPS check
  (`gpsErrorPct`, with the shape term and the correction) agrees within 15%
  on `aggLiftCycles` consecutive comparable cycles, and drops it on as many
  disagreeing ones, a shape move, a missing figure or `sigma`, or an
  outstanding shape change. A lifted figure carries `capAtMeasured`, so
  `pricedDerivedRate` never prices it above a measured rate of the variant's
  own. The thresholds are named constants and unmeasured.
- One shape per fleet. The line is fitted at the fleet's shape, so a variant
  serving a shape of its own gets an average. Not handled.

`SwitchITLAggregatedShapeTerm` gates the tags, the window drop, the points
and the corrected figure; `SwitchITLAggregatedSizes` the lift, which needs
the tags. A line's shape is persisted in `itlLineShapes`; a held slope
restored across a restart takes its shape from the line it published.

An aggregated line is shared under its own key: `itlLineKeyForRole` appends
`|both` to the physics key, so the same model on the same engine
configuration keeps one line for decode pods and another for aggregated pods,
and neither lends to the other. The window key is per variant already.

### ITL per accepted token (speculative decoding)

vLLM observes `inter_token_latency` once per request per engine step, however
many tokens the step emitted. With speculative decoding or MTP a step emits
`1 + E` tokens, `E` the accepted draft tokens, so `AvgITL` is the step time and
a line fitted from it prices the token rate `1 + E` times low (1.5-3x on
typical acceptance; issue #274). SGLang divides each step's interval by the
tokens it emitted, so its ITL is already per token.

The collector reads `E` over the ITL's own window, per pod
(`registration.QuerySpecAcceptedPerStep`):

```
E = rate(vllm:spec_decode_num_accepted_tokens_total[1m])
  / rate(vllm:inter_token_latency_seconds_count[1m])
```

into `ReplicaMetrics.SpecAcceptedPerStep`, with `SpecAcceptedPerStepKnown`
set only for a present, finite, non-negative value. The denominator is the ITL
count, not `vllm:spec_decode_num_drafts_total`: a step that proposed no draft
(an n-gram drafter with no match) is an ITL observation and no draft.

Before the window takes a row, `fitLines` reads the variant's rows through
`domain.ReplicaMetrics.ITLPerToken` (`itl_per_token.go`), with
`capacity.EngineParams.SpeculatesPerStep()` (vLLM with `--speculative-config`):

| counter | flags speculate | the row the window reads |
| --- | --- | --- |
| known, at most `num_speculative_tokens` | either | `AvgITL / (1 + E)` |
| known, above `num_speculative_tokens` | yes | withheld (`ITLOverMax`): no step accepts more drafts than it proposed |
| missing | yes | withheld (`ITLNoCounter`): a missing counter is not "no speculation" |
| missing | no | as read: one token a step |

`num_speculative_tokens` is read from `--speculative-config` as one JSON
object or in the dotted `--speculative-config.num_speculative_tokens`
spelling (`EngineParams.SpeculativeTokens`); unstated, no bound applies. An
over-read `E` over-states the token rate, the direction that under-orders,
which is why a count above it is withheld rather than clamped. On an SGLang
engine (`domain.ITLStep.PerToken`) every row is read as it is.

A withheld reading counts as `noITL` in `itl-window`, which an idle replica
reads too, so `noteITLWithheld` logs `itl-withheld` with the reason once when
a variant's readings start being withheld or the reasons change (the
over-bound line carries the count read and `num_speculative_tokens`), and
`itl-withheld-ended` once a cycle keeps a positive reading and withholds none
(`itlWithheldLogged`, per window key, aged with the variant). A cycle with no
evidence either way -- idle, scaled to zero, or its rows not read during a
rollout -- changes nothing, so a load that comes and goes logs nothing new.
Once per change, within the default-verbosity budget (#303).

A bound a rounding error under the count read (1e-6 relative) is not a count
over it. An idle data-parallel rank (empty ITL, 0/0 count) does not make its
pod's count unknown.

A pod of several engine instances (external data-parallel load balancing)
merges `E` weighted by each instance's steps, `GenerationTokenRate / (1 + E)`;
ranks behind one API server are summed exactly in PromQL.

`E` counts drafts accepted, not tokens emitted: a draft accepted and then cut
by a stop string or `max_tokens` counts and is never sent. On very short
outputs (a P/D handoff) that over-states `E` slightly. It is a known limit,
small next to the step-time error it replaces.

SGLang's flags never speculate per step. The window, the baseline, the held
slope and the shared line are then per token. Their unit differs from a step
time only on a vLLM engine that speculates, so only there is it recorded: the
learned-state snapshot names such a variant in `itlPerToken` and such a line in
`itlLinesPerToken`. A speculating variant's entries in the other unit are not
installed (`RestoredVariant.ITLOtherUnit`, logged `itlOtherUnit`), and a line
in the other unit is not lent to a speculating borrower (`borrowLine`). A
snapshot from before the field marks nothing, so it reads as step time: on
the first restart after the upgrade a speculating variant fits its ITL line
again. Other engines and other fields restore as before; the export keeps a
non-speculating variant's pending entries for the same reason the install
does (`itlUnitsDiffer`). The mark is per variant and taken from the flags it
runs now, while a put-aside baseline belongs to the arguments it was learned
under: a step-time stash of speculating arguments, restored for a variant
that no longer speculates, comes back unmarked if it returns to them. That
errs safe (a baseline too high over-provisions) and ages out.

The switch is `SwitchITLPerToken` (`itl-per-accepted-token`), on in
production. Off, the line is fitted from the step time, as before. No
committed golden speculates, so it moves none; `heldBy` in
`learnreplay/switch_runs_test.go` names the unit tests that fail with it off,
and the property case `known-truth/itl/speculative-accepted-tokens` fails on
its stub mutant on 20 of 20 seeds.

The throughput and slo analyzers keep the step time: each fits its own line
from `AvgITL`, so on a speculating fleet their figures are still low by
`1 + E`.

The window and the fit's rules live in `itl.Tracker`, which the slo analyzer
uses too ([the slo analyzer](slo-analyzer.md#itltracker)). A full fit needs
evidence:

| constant | value | why |
| --- | --- | --- |
| `DefaultMinSamples` | 10 | a line through noise is not a line |
| `DefaultMinKSpread` | 0.30 | samples clustered at one `k` fix an intercept, not a slope |
| `DefaultWindowMaxSize` | 20 | |
| `DefaultObservationMaxAge` | 30m | |
| `DefaultMinObservableK` | 0.15 | below this the engine is not batching |
| `DefaultMaxObservableK` | 0.80 | above it, preemption makes ITL non-linear; a replica at its sequence cap is still read (below) |
| `DefaultSteadyFor` | 2m | a replica's `k` must have held still this long |
| `DefaultMaxKDrift` | 0.10 | how far it may have moved and still count as still |
| `DefaultSteadyCarry` | 5m | how long a steady reading may outlive the main window's oldest |
| `DefaultSteadyWindowMaxSize` | 40 | steady readings kept per key |

The full fit, and so the remembered `B`, uses **steady** readings only: a
replica whose `k` stayed within `DefaultMaxKDrift` over the last
`DefaultSteadyFor`, and that has served at least that long if it joined after
the key was first observed. `k` is instantaneous and ITL is a `rate[1m]` mean,
so across a step in load the two describe different moments; in a balanced
fleet that step is the only spread a window ever holds. Before this gate, two
identical models in two namespaces remembered `B` = 4.5 ms and 14.8 ms from
whichever transient each window caught, and priced the same replica at 5.3 and
7.7 requests/s (issue #270). `itl-window` reports `unsteady` and `steadyHeld`.

The pinned fit, which prices a balanced fleet, takes the steady readings too
whenever there are at least `DefaultMinSamples` of them, and every reading only
below that, so a fleet that has not settled still gets a line. Taking every
reading, it took each load step whole: on the issue #270 rerun `A` went from
0.077 to 0.207 on the cycle the load stepped, and the derived mu fell from 6.4
to 2.65 requests/s for two and a half minutes.

A saturated fleet has few steady readings: `k` swings between above the band
and its dips into it by more than `DefaultMaxKDrift`, and on a two-namespace
run the steady readings numbered 0 on almost every cycle. So the pinned fit
takes every reading, and a replica's first readings after it starts serving (a
pod that joined, or one the load came back to) sit many times above the line.
Three such readings of sixteen, at about 0.46 of `k` and 435 to 496 ms, moved
least squares' `A` from 0.101 to 0.170, and the derived mu read 3.1 to 4.2
requests/s against the 6.1 to 6.2 each replica served, for a whole
twenty-minute phase.

Those readings are left out by the replica's state, not by their value: a
reading taken while its replica is inside its settle (it joined, or the load
came back to it, less than `DefaultSteadyFor` ago) is marked when it is taken,
and the pinned fit skips it. When fewer than `DefaultMinSettledSamples` (3)
readings are left, the key's last `A` holds (`itl-fit` logs `slopeHeld`), and a
key with no `A` yet gets no line. `itl-window` reports the marked readings as
`settling`.

#### Set, clear and dwell

Every threshold that switches a source states three values, the safe side and
what a restart does ([3.5](../proposals/analyzer-learner.md#35-hysteresis)).
Add a row when you add such a threshold.

| threshold | set | clear | dwell | safe side | after a restart |
| --- | --- | --- | --- | --- | --- |
| ITL tier: full fit or pinned fit (`itl/tier_latch.go`, on the saturation, slo and throughput lines) | 10 steady readings, spread 0.30 of `k` | under 8 steady readings or spread 0.18, or a pinned fit pricing 3 % more | 10 min after the last change, unless the new line prices 3 % more replicas | the line that prices more replicas, from either tier, judged by what the line sustains where the analyzer prices it | the latch is not kept: a key with a restored slope stands at the pinned fit |
| Mu's stale latch: measured or derived mu (`measuredYield`, `muChoice`) | the ratio of measured to derived mu, smoothed over `MuLatchRatioSmoothing` (3 min), past `muChoice.maxRatio` (2.0), or one cycle's own ratio at or past `MuLatchRawSetFactor` times it (2.8x); under 1/2.0, smoothed only, once stale for `lowHoldFor` | the smoothed ratio back within `muChoice.returnRatio` (1.6) | none: the smoothing is the delay. A stale ratio stepping from 1x to 2.8x or more sets on the step, to 2.5x on the 14th cycle at 15 s, to 2.2x on the 22nd | the lower figure: latched over the derived one, unlatched under it | the latch is not kept, and restores to the safe side (`SwitchMuLatchRestoresSafe`); the smoothed ratio starts over with a restored, brought-back or new window |

**Price.** A line's price is sequences over ITL at the fill the analyzer reads
(`Admission.PricedRate`). At a fixed fill that is the ITL there. With a TPOT
target the fill is the line's own, lowered by the batch cap, and it often clamps
to the band's top for both lines: the ITL there is what tells them apart.

**Resting.** Between the clear and set values the full fit prices its own key's
line and teaches nothing: it writes neither `B` nor the held slope, and it is not
published, so the prior on `B`, a sibling's borrow, the shape points and the
snapshot all stay on the last fit at the set values. It stays only while
the pinned fit, through a `B` learned at the set values, does not price 3 % more.
So a resting line prices at most 3 % under the pinned fit.

**Evidence.** The latch goes back to the pinned tier when the steady window
empties, when the key goes past `DefaultSteadyCarry` without a steady reading,
and on `ClearWindow`, `ForgetSlope`, `ForgetSlopes` and `ForgetBaselines`.

**Noise.** "At most one reversal per excursion" (analyzer-learner.md 2.6) does not
hold near the set value, by choice: pricing safety over flip count. On synthetic
rate[1m] noise the tier changes a median of 7 to 9 times in 4 h at spread 0.26,
against 38 to 44 with the bare gate. The test bound is 16 on flat paths at spreads
0.26 to 0.30, 4 at 0.32 and 6 on a two-crossing ramp. Those changes are the resting
rule leaving for the safe side and the spread setting again. Dropping the rule
would give one change, and more cycles under the truth
([3.5](../proposals/analyzer-learner.md#the-itl-tier-as-built-495)). A load step
collapses the spread past any clear value, so the dwell does that work. The 3 % and
the 10 min have not been checked on a large fleet.

**While the dwell refuses a change** the log says `tierHeld`: `itl-fit` on the
saturation line, `slo-variant` (at `-v=3`) on the slo line, and the
`throughput analyzer: ITL line` summary on the throughput line, which logs only
when the line goes silent or comes back. Leaving the full fit holds the key's last
slope (`slopeHeld`), and entering it stays on the pinned fit; the what-if planner
reads both as `held` and flags `HeldLine`. A key's first full fit does not wait:
until then, only it can replace the default `B`. A key that has had one waits even
if its `B` came out at or under zero and was not kept. Once the dwell ends a new
line may price less than the one it replaces by more than 3 %, as it could before.

`SwitchITLTierHysteresis`, one per analyzer, turns the latch off for a replay.

#### One-sided at the cap (issue #467)

A replica at its stated cap reads above `itl.DefaultKSat`. While the key has
no learned B the pinned fit pins `DefaultBaselineSec`, and through such
readings it prices under the true line by `(B0 - B)(1 - k/k_cap)` at a fill
`k`. Under `SwitchITLOneSidedAtCap` (`saturation-itl-one-sided-at-cap`,
`itl_one_sided.go`) the admission sets `itl.Admission.OneSidedAboveKSat`, as
the throughput line does, with `OneSidedAt` the line's pricing k
(`pricingK`) and `BPrior` the prior on B (`itlBPrior`).

- **Scope.** `itlBPrior` reads only the variant's physics: the tracker's
  keys in `itl.PriorScope` (the same model, accelerator and GPU count, the
  same engine arguments in `itlArgs`, the same role this cycle; another
  namespace is out of scope, since its role is unknown here), and the line
  published for the variant's own physics key (`itlPhysicsKey`, shared only
  with `enableLearnedStateReuse`).
- **Floor.** `itl.BPrior` is `max(itl.DefaultBPrior, largest learned B in
  scope)`: a learned B only raises the prior, never lowers it under 30 ms.
- **Cap.** `itl.priorCap` caps the prior at the least ITL of any reading in
  the variant's own window, at any k: every reading is at least B.
- **Anchor.** `itl.priorLine` passes the prior's line through the mean
  `(kbar, ITLbar)` of the window's readings at k >= kSat that sit at or past
  the fill the line is priced at (`Admission.priceAnchor`: `OneSidedAt`, or
  the fill where the default-B fit meets `OneSidedTarget`), when there are
  three; else the three of highest k. `A = (ITLbar - b)/kbar`, never negative
  since the cap puts `b` under every ITL. On readings without noise the
  error at a fill k is `(b - B)(1 - k/kbar)`: over the truth for `k <= kbar`
  while `B <= b`, under it past `kbar`, which is why the anchor sits at or
  past the priced fill. When even the anchor's `kbar` is under it,
  `Reading.PriorBeyondReadings` is set (logged `priorBeyondReadings`) and
  the bound does not hold. A mean rather than the least reading: its noise
  falls by sqrt(n), and the least reading's selection bias is gone.
- **Line returned.** With a prior set, `oneSidedLine` always returns the
  line through the default B at the held A, whether or not the admission
  names its price: the prior's line can be flat (one reading, or the cap's
  ITL at every reading), which `PriceTPOT` and `ValidModel` refuse.
- **Not lent.** A line with `PinnedAtBPrior` is not published to
  `itlLines` (`analyze.go`): it carries this variant's at-cap readings and
  pricing k, so siblings borrow nothing from it while it is.

The tracker compares that line with the fit through the default B at the
pricing k, and when it is higher returns the line through the default B that
prices the same there (`oneSidedLine`): A above zero, B the pin, so
`DeriveMuAtCap`, `ValidModel` and line sharing read it as any pinned fit. The
held A is that line's A, so a hold prices the same. `itl-fit` logs
`pinnedAtBPrior`. The throughput line sets no prior and keeps 4b-3's line
through the least ITL (`atLeastITLLine`).

The slo line sets the same flag and prior under its own switch
(`slo-itl-one-sided-at-cap`, `slo/one_sided.go`), scoped the same way on its
own tracker and `args`, with `itl.Admission.OneSidedTarget`, the TPOT target:
each line is judged at the fill where it meets the target (`pricedFill`,
`PriceTPOT`'s clamp), and the lower fill wins.

The what-if bands rerun the same choice: `itl.Refit` takes the admission's
one-sided fields, `whatif.Refit.OneSided` carries them, and
`learnreplay.VariantState.refit` passes the admission the analyzer last
priced the key with (`SaturationAnalyzer.ITLOneSided`, kept per key in
`itlOneSidedLast` with whether the line was raised, and evicted with its
variant in `EvictStaleHistory`) for a line recorded as raised
(`PinnedAtBPrior`). The scope is built by `itl.NewPriorScope`, which reads
each analyzer's arguments map lazily, under its own lock.

#### When a replica's settle starts (learner PR 4a)

The settle is read from the engine's settle tracker (`internal/signals/settle`,
the view k2 reads), through `itl.Admission.Settle` (`itl_settle.go`;
`internal/signals/itl/settle.go`). A replica's settle starts at the latest of:
the tracker's settle (`settle.Replica.Since`: it joined, came back past the
absence tolerance, its Ready time changed under the same Pod name, it was lent
or rolled out), the last return of load after at least `idleRows` idle rows
(`ReturnedAfter`; idle on positive evidence, `settle.Row.Activity`), and its
first Ready row after one that was not (`settlerow.ReadyAgain`). The key
stamps the cycle it first read a start (`replicaTrace.settleSeen`) and the
settle runs `steadyFor` from there, on the analyzer's clock. It takes a start
only when it is later than the one it holds: the tracker keeps rows for 5 min
and its last eight returns, and a start that falls back when one of them ages
out is no event.

A start the key first read on its own first `Observe` is a controller restart
or a key made anew (a forget, new engine arguments). With
`Admission.WaitsAfterRestart` it is a settle that runs from the tracker's
start, so after a controller restart every replica waits `steadyFor` (a
restored sighting's start is the old one, so it does not) and a key
made anew does not make long-settled replicas wait. Without it, as before 4a,
such a replica is settled. With the key's own trace (the move off) it reads as
"every replica on the key's first cycle waits", a key made anew included; only
a replay runs that. An analyzer with no engine tracker (a unit test)
never waits: it stands for a controller that has watched the fleet.

`Admission.KeepsThroughMissingRow` keeps a replica's trace through a cycle it
is missing from; the tracker's absence tolerance decides when it is back as a
new sighting. `Admission.FiniteOnly` reads a reading's ITL from the presence
bits (`ITLState() == ITLFinite`), so a Pod with an Absent rank is refused.
`Admission.RestartUnderName` reads a new Ready time under the same name as a
restart, and `Admission.IdleOnEvidence` reads load returning from the
tracker's idle runs (idle on evidence); off, each falls back to the key's
own rule.

Each of the six is a replay switch, in both analyzers:
`itl-reads-the-settle-tracker`, `itl-restart-under-its-name-settles`,
`itl-idle-needs-evidence`, `itl-waits-after-controller-restart`,
`itl-keeps-the-settle-through-a-missing-row` and
`itl-reads-the-finite-pod-mean`, with an `slo-` prefix in the slo analyzer.
Off, each does what the line did before 4a. The line is a
learner on the shared rules too: a `learn.Report` per variant that fits a line
(`itl_learner.go`), `learner-write` at `--v=3` when A or B moves more than
`ITLWriteBand` (5 %) or the tier changes, and `learn.ITL` on `learn.Learners`,
so the forget-learned-state annotation reaches it by name. The line's `pods`
and its admission margins come from the reading: `itl.Observation` carries the
pod its row came from, `Observe` sets `itl.Reading.FitRows` and `FitPods` from
the readings the line was fitted from (none for a held slope), and
`Reading.WritePods(LearnerWriteMaxPods)` and `Reading.AdmissionMargins` give
the fields, so the saturation, throughput and slo lines log them alike. Nothing
fits from the pod.

The report's `LastAdmitted` is the newest reading the window holds, the fit
time of a held slope, or, for a key with neither, when its intercept B was
last admitted (`itl.Tracker.Report`). That stamp is the newest reading of the
full fit that set B (`Tracker.baselineAt`), not the cycle it ran, so a refit
from the readings the steady window carries does not make B younger. The
snapshot carries it beside B as `itlBaselineAdmittedAt`
(`LearnedState.ITLBaselineAdmittedAt`), and a restore installs it with B
(`RestoreBaselineAt`): a B restored alone reports when it was fitted, not 0.
`RecordLearnerReports` publishes a report only when it names a `PricingKey`, and
no line `Observe` prices reaches the stamp (both windows age out on
`DefaultObservationMaxAge`, so a priced line with an empty window rests on a
held slope), so the series does not show it today. `Report` reads the
intercept the key holds when it reports, not the reading's `BaselineStored`
from before the fit.
It goes wherever B goes: the sweep, a forget, `dropVariant`, the size trim (with
the variant's `seen` entry; a stamp left with no B is spare). A B with no
stamp is of unknown age and reports 0: one from a document written before the
field, one put back from `itlBaselineStash` (`SetBaseline`), or one whose stamp
was ahead of now past `maxClockSkew`. The learned-state store's digest leaves
the stamp out, as it does a slope's fit time. Replay switch
`itl-baseline-stamped` (`SwitchITLBaselineStamped`): off, nothing is exported
or installed.

The last `A` also holds when the window itself runs short: fewer than
`DefaultMinSamples` readings left after `DefaultObservationMaxAge`. A replica
below `analyzerTuning.itlBand.minObservableK` offers no reading, so a fleet that learned its line at
load and then served light load for half an hour used to lose the line, the
derived mu and the throughput floor with it, and was sized by KV occupancy
alone. At light load that reads far below the throughput share served: one H200
replica read 0.064 while serving 25% of its saturated rate (issue #284). The same
applies above the band, for a replica not at its sequence cap.

The hold is bounded. `itl.Admission.SlopeHoldFor` is the analyzer's
`BucketRetention` (the horizon `itlLines` ages a finished fit on), and
`Admission.Args` the variant's `windowArgs`: a slope older than the one or
fitted under other arguments is not held. `itlUnderArgs` also sweeps the key,
slope included, on an arguments change. `slopes` is not in the snapshot, so a
restart ends the hold too. A held line is the variant's own: it may order, and
a variant woken from zero inside the hold is priced from it on sight and can
settle a shape-change hold, as its fresh fit would have. It is **not**
republished to `itlLines`: `publishLine` stamps `learnedAt` with now, so a
held line republished every cycle would never age out and would overwrite a
sibling's fresh line on the same configuration. `itl-fit` logs `slopeHeld`
and `slopeAge`, `derived-mu` logs `lineHeld`.

A key
that never fitted, and has no sibling's line to borrow, still has no line;
`itl-line-none` says so each cycle (`derivedMu=false`), so `itl-window`'s
`offered: 0` beside `belowBand` reads as light load, not a broken window.

`FitPinnedB` then takes `A` as the `k`-weighted mean of the readings' own
slopes, `sum(ITL_i - B) / sum(k_i)`, with nothing trimmed. A trim by value
cannot tell a warm-up from a replica that is really slower, since both sit
above the line:

- The interquartile mean set aside one replica at slope 0.12 among four at
  0.08 entirely, pricing the fleet's capacity 8.5% high. It also let the
  warm-ups through once they carried more than a quarter of the weight: a
  fleet of three growing to six read `A` = 0.127, with mu about 20% low.
- The weighted median jumped from one group of slopes to the other as their
  weight crossed one half. A fleet serving two shapes side by side (0.08 and
  0.12, half each) had `A` swing 36% of its mean.

The mean blends a slow replica in at its weight, holds that two-shape fleet to
2%, and on the same fixture with three extra joiners reads 0.097. Replayed over
the two-namespace runs, the pinned `A` has a median of 0.097 and 0.101 for the
first run's two namespaces (worst 0.102) and 0.079 for the second.

`itl-window` counts every row under one reason (`offered`, `notReady`,
`noITL`, `noK`, `belowBand`, `aboveBand`), so they add up to `replicas`, and
reports `steadySpread` beside `minKSpread`, the other half of `ready`.

A reading above `DefaultMaxObservableK` is kept when its replica is at its
sequence cap (`itl.Admission.atSequenceCap`): `RunningRequests`
(`vllm:num_requests_running`) is exactly `max_num_seqs` times the replica's
ranks, `max_num_seqs` was read from the engine's flag
(`EngineParams.ExplicitMaxNumSeqs`; the parser's default of 256 is not vLLM
V1's 1024 on large cards), requests are waiting, and `PreemptionRate`
(`rate(vllm:num_preemptions_total[ITLWindow])`, the ITL mean's own window)
was read and is at most `analyzerTuning.itlBand.capMaxPreemptionRate` (default 0). The band
edges are the policy's `analyzerTuning.itlBand.minObservableK` and `analyzerTuning.itlBand.maxObservableK`
(`ScalingPolicy.ITLBand`); `Observe` takes them, the cap and the allowance as
one `itl.Admission`, built alike by both analyzers. `pricingK`, and the slo
analyzer's fill, clamp to the same band. Such a replica is not short of memory, so the reason for the band does
not apply to it. On one run a decode replica held 256 running and 768 waiting
at `k` 0.85-0.89 for 20 minutes while three siblings sat below 0.15, and no line
was ever fitted (issue #287). `itl-window` counts these readings as `capped`,
a part of `offered`, and logs the `batchCap` it judged them by (`--max-num-seqs`,
lowered by `--max-num-active-seqs`). An unknown
cap or preemption rate (SGLang publishes no preemption counter) keeps the band
closed.

The steady readings are kept apart from the main window, and outlive its oldest
reading by up to `DefaultSteadyCarry`. A load step is what gives a balanced
fleet its spread, and it fills the 20-reading main window with unsteady
readings: every replica is held out for `DefaultSteadyFor`, so N replicas add
8N of them at 15 s cycles. When the steady readings were trimmed to the main
window, the old level's were gone before the new level's were steady, and no
fleet of two or more replicas ever had the spread for a full fit: on the issue
#270 rerun, `itl-window` was `ready` on 0 of 294 multi-replica cycles, and `B`
stayed at the 6 ms default. Carried, the old level is still there when the new
one settles: on a step from `k` 0.35 to 0.70 (`steady_test.go`), every fleet of
1 to 16 replicas learns the true `B`. The carry is the full fit's only: the
pinned fit reads the steady readings the main window spans, reaching into the
carry only for the `DefaultMinSamples` it needs through a step. `A` moves with
the workload, and fitted on the carried window it followed a heavier shape at
the same fill twice as slowly (two replicas: 17 cycles to the new `A`, against
8). A level the fleet only passed through
never settles, so it never reaches the steady readings and teaches nothing.
A reading with no `k` (the collector's zero value) is not traced, so one
missed scrape does not make its replica unsteady. A replica the load comes back
to after two or more rows with neither an ITL nor a `k` starts over as a joiner
and waits out `DefaultSteadyFor`. A row with a `k` and no ITL is a busy replica
whose ITL was not read (the collector drops a failed query's values whole), and
does not count toward idling: counted, an `avg_itl` query that failed on two
cycles running restarted every replica of a busy fleet. An idle replica leaves nothing in its trace, so its first
reading, which carries the warm-up ITL, was otherwise steady: on a load step
that brought an idle replica back, the full fit stored `B` = 77 ms against a
true 12 ms. One such row between two readings is a missed scrape.

From the line, two closed forms:

```
Sequences(k, C, KVreq) = k x C / KVreq          // resident requests at occupancy k
TokenRate(m, k, C, KVreq) = Sequences / ITL(k)  // generated tokens/second
```

`GPSErrorPct` compares `TokenRate` against the observed
`rm.GenerationTokenRate`. It **reports and does not act** — `noteLineMismatch`
logs `itl-gps-mismatch` with `gates: false`.

## The service rate (mu)

`mu` is requests completed per second by one saturated replica. There are two
ways to get it. The derived one can be priced for a shape the fleet has never
run; the measured one wins where the fleet has measured this shape itself
(`useDerived`: an own window with at least `MinThroughputSamplesToOrder`
samples), until it is stale and sits more than `MeasuredDerivedMaxRatio` (2x)
above the derived figure (`measuredYieldsToDerived`, `muChoice`):

- **Fresh** is the bucket reading saturated, within `MeasuredMuFreshFor` (5m),
  at a rate within 2x of the window's median, whether that reading was
  recorded or refused (`noteSaturatedReading`; the newest sample stands in
  only where no such reading is on record). A fresh measurement wins either
  way. A replica queued every minute at 1.2 req/s used to have every reading
  refused under a derived 3.6, went stale by construction, and was priced at
  3.6 (`TestMuChoice_ContinuousSaturationKeepsItsMeasurement`).
- **Stale and disagreeing** prices the **lower** figure, which over-orders: a
  measurement far under derived keeps pricing, one far over it yields.
- **For at most `MeasuredLowHoldFor` (30m).** A measurement more than 2x
  under derived yields once its bucket has not read saturated in agreement
  for that long. A too-high derived figure corrects itself (the fleet
  saturates and re-freshens the measurement); a too-low measurement does not,
  because an over-ordered fleet never saturates. In review 1.2 req/s held for
  20 unsaturated minutes against a derived 3.585, three times the replicas
  the line priced, with no end date
  (`TestMuChoice_AStaleLowMeasurementHoldsForAtMostMeasuredLowHoldFor`). If
  the measurement was right, the fleet saturates and takes it back: at worst
  one saturated episode per 30 minutes.
- **A served rate contradicts it.** A measurement under derived yields at
  once, fresh or not, while a Ready replica of the variant serves more than
  2x it this cycle (latched down to 1.6x): that replica sustains what it
  serves (`servedBounds`,
  `TestMuChoice_ALowMeasurementAServedRateContradictsYields`). With no line
  there is nothing to yield to, and the measurement stands.
- **Its full siblings serve more.** A measurement under
  `SiblingMuTolerance` (0.8) of the lower median rate served by the
  variant's Ready replicas with a full cache, queued ones included, yields
  to that median when at least one of them is unqueued, line or no line, logged as `saturated-throughput-yields-to-siblings` with the
  bucket `served-siblings` (`siblingYield`). A replica at 0.55x its siblings
  passes the 2x served bound and, as the only queued replica, is the window.
  In review it priced the fleet at 1.8x the replicas for as long as it
  stayed slow. Neither the bound nor the 30-minute hold released it
  (`TestMuChoice_AModeratelySlowReplicaYieldsToItsFullSiblings`). The
  yield is remembered after the larger fleet stops filling the siblings,
  until newer sibling evidence moves it or the window starts over. A median
  read from queued replicas alone never opens a yield, so a slow replica
  queued alone is never its own sibling; it moves one already open, and
  ends it once the measurement is no longer under it. The queued replicas
  count because the router steers away from them: read from the unqueued
  alone, three replicas at 2.0 beside one at 3.6 priced every replica at
  3.6 and ordered 5.71 replicas for a load needing 7.84
  (`TestMuChoice_AMostlySlowFleetIsNotPricedAtItsFastReplica`,
  `TestMuChoice_ASiblingYieldEndsWhenItsSiblingsReadTheMeasurement`). 0.8 because equally saturated replicas at one shape read within
  about 10 % of each other (0.90-1.07 on E1). It bounds what one slow
  replica can over-order to 1.25x
  (`TestMuChoice_AMeasurementNearItsFullSiblingsStands`).
- **Latched.** Once a measurement has yielded it stays yielded until it comes
  back within `MeasuredDerivedReturnRatio` (1.6x) of derived, or the bucket
  reads saturated in agreement again. Derived mu is recomputed every cycle
  from the fleet's output length; with a hard edge at 2x a measurement near it
  flipped the priced mu 8-11 times in 20 cycles under 4 % noise on the length
  (`TestMuChoice_TheYieldDoesNotFlipNearTheRatio`).

Under EPP flow control the queue that writes a sample rarely builds again,
and a stale high reading must not under-order the fleet. Within 2x the
measurement keeps winning however old it is: run R's 1.27 req/s against a
derived 0.67 is the case it is there for.

Three rules keep the choice on the safe side where it has no evidence, each a
replay switch (`mu_switches.go`):

- **An exact repeat is use** (`SwitchMuRepeatIsUse`). A rate read past the
  sample spacing at exactly the value last read is the same scrape pair, or
  a frozen series. It touches the window's use clock only, and does not keep
  the window's episode (its write clock, which `EpisodeGap` ages) alive. Over
  the derived figure it does not keep the measurement fresh either; under
  it, or with no derived figure, it still counts as agreement, so a real low
  rate that repeats is not handed to the higher derived figure after the
  30-minute hold (`TestMuRepeat_*`). The prefill bound writes through the
  same fold and keeps taking a repeat as a write.
- **An unknown age resolves to the safe side** (`SwitchMuUnknownAgeSafe`). A
  window with no sample time and no agreeing reading on record is stale over
  the derived figure, and under it holds for `MeasuredLowHoldFor` from its
  own last write (`lowHeldOut`, `TestMuUnknownAge_*`).
- **The latch reads a smoothed ratio** (`SwitchMuLatchReadsSmoothedRatio`,
  #526). The stale rule sets and releases on the ratio of measured to derived
  mu smoothed exponentially over `MuLatchRatioSmoothing` (3 min), not on the
  cycle's own ratio (`muChoice.smoothRatio`). The per-pod rate noise measured
  on E1 is 7.5-18.9 % at lag-one 0.1-0.4, as wide as the 1.6-2.0 band, so on
  one cycle's ratio a stale measurement coming back across 1.6 let go and was
  set again several times in one excursion. Smoothed, it changes once on every
  seed at 10 % and 15 % (`TestMuLatchBand_*`). The cost is reaction time when
  the ratio moves while the measurement is already stale, and while the latch
  waits to set it prices the higher, measured figure, the SLO side. So one
  cycle's ratio at or past `MuLatchRawSetFactor` (1.4) times `maxRatio`, 2.8x,
  sets it at once; a step to 2.5x waits 14 cycles at 15 s, to 2.2x 22. The
  release always reads the smoothed ratio. At 1.25 (2.5x) the raw set re-set
  a released latch on 6 of 1000 seeds at 15 % noise, at 1.4 on none. A ratio
  already past 2x while the measurement was fresh latches on the cycle it goes
  stale. The smoothed ratio starts over when the key's window does, and with a
  restored or brought-back window. The served yield is new evidence and reads
  the cycle's own ratio; its cycle still counts in the smoothed one.
- **The latch restores to the safe side** (`SwitchMuLatchRestoresSafe`). The
  latch is not persisted. A window restored, or brought back, with no choice
  on record comes back latched over the derived figure and unlatched under
  it; its first choice is evidence, and the hysteresis runs as it does live
  from then on (`muChoice.latchKnown`, `TestMuLatch_*`). A restore that
  installs a window, and a bring-back from the stash, mark a memo made
  before them unknown (`unknownLatchLocked`).

Mu has no joined test of its own: a sample is written only beside a k2
`P1-obs` write, so it waits out k2's settle, the wait after a controller
restart included. Each admitted reading carries k2's stamp (how long the
replica had been settled, and which settle test passed it). Each cycle mu
fills a `learn.Report` per variant (`mu_learner.go`; `LearnerReports`, beside
k2's), and logs `learner-write` at `--v=3` for a key whose priced figure was
set for the first time, moved more than `MuWriteBand` (5 %) from the value
last logged, changed source, or has not logged for `LearnerHeartbeat`. It is
on `learn.Learners`: the annotation reaches `forgetMuLocked`, which clears what
`learner-write` last logged, and a key forgotten or evicted is reported
(`muForgotten`, `SwitchMuForgetReported`). The prefill bound shares mu's maps
and is not reported. The throughput floor's line names the key and source it priced from (`muKey`,
`muSource`, `muPricedFor`) and the start time it projected over.

### The pricing point

```
kPrice = clamp(kvCacheThreshold x scaleUpThreshold,
               itlBand.minObservableK, itlBand.maxObservableK)
```

The band is the policy's `analyzerTuning.itlBand` (0.15 and 0.80 unless the
policy sets it), the one the line is fitted over; with both thresholds unset
the product is `itl.DefaultKSat` before the clamp.
`pricingK()` reads the policy and calls `whatif.PricingK` in
`internal/signals/whatif`. With the shipped defaults that is
`0.80 x 0.85 = 0.68`, **not** `0.85`. This composition is easy to get wrong:
`k_sat` (0.85) is where the ITL line is validated, while 0.68 is the occupancy
the autoscaler actually targets.

### The derived mu

`whatif.DeriveMuAtCap` in `internal/signals/whatif/mu.go`, the arithmetic the
`slo` analyzer's pricing shares:

```
C        = whatif.CapacityTokens(params, ranks, rm.TotalKvCapacityTokens)
seqs     = whatif.CappedSequences(kPrice, C, KVreq, batchCap x ranks)
tokenSec = seqs / ITL(kPrice)
mu       = tokenSec / avgOutput
```

It declines (`OK: false`, and the floor then emits nothing for that role) when
the model is unfitted, `avgOutput <= 0`, `KVreq <= 0`, `seqs <= 0`,
`ITL(kPrice) <= 0`, or `kPrice` is outside `(0, 1]`.

Note `avgOutput` is a **separate parameter** from `fleet.AvgOutputTokens`. That
separation is deliberate and is the subject of the next section.

### Both halves of the shape, one window

`avgOutput` divides, and `KVreq` — inside the shape — decides how many requests
fit. During a shape change the divisor follows a short `[1m]` window so the old
shape's long stragglers cannot dominate it. **The shape must follow with it.**

`analyze.go` resolves the pair together:

```
muDivisor = ExpectedOutputTokens(fleetOutput, stableOutput, 512)
muInput   = fleetInput
if shapeChangedWithin(window):
    if recentOut := fleetOutputLengthRecent(...); recentOut > 0:
        muDivisor = recentOut
        if recentIn := servedPromptLengthRecent(...); recentIn > 0:
            muInput, muShortWindow = recentIn, true

muShape = shape.New(muInput, muDivisor, hitRate)  if muShortWindow
          fleetShape                               otherwise
```

Measured on a 6000/1000 → 1000/4000 swap: with only the divisor on the short
window, a request priced at **7017 tokens** — larger than either real shape
(6500 then 3000) — halved `mu` to 1.57 req/s against a settled 2.76, and the
floor ordered 7 decode replicas where 3 was right, on a router queue that was
zero throughout. Priced as a pair the same cycle reads `KVreq` 3000 and `mu`
3.20. Both halves or neither: if the engine publishes the short-window output
but not the short-window prompt, the divisor still moves alone, because the
alternative is the straggler bug the short window exists for and that error is
an order of magnitude larger.

`ExpectedOutputTokens` (in `internal/config`) is first-positive-of:
the fleet's own reading, the last stable shape, the ScaledObject's
`defaultOutputTokens` trigger metadata, then `DefaultExpectedOutputTokens`
(512).

### The measured mu

`saturatedCompletionRate` in `shape_change.go`, used when a replica is
saturated. For a generating role it is priced from tokens, not completions —
completions are bursty and a max window keeps the burst. For prefill:

```
mu_prefill = rm.PrefillComputedTokenRate / ILeff
```

`PrefillComputedTokenRate` is **summed** across a pod's engines, not averaged:
each computes part of the prefill and their rates add.

The window (`recordSaturatedThroughputFrom`) takes one sample per
`ThroughputSampleSpacing`. Readings inside the spacing fold into the last
sample: by their **max** for re-reads of one replica (the same rate window read
again) and by their **mean** across replicas (independent readings,
`throughputPodReads`). The mean because a fleet serves the sum of its
replicas' rates: a lower median ignored a slow replica once three read, and
four at 1, 1, 1 and 0.5 priced 1.0 against the 0.875 they serve
(`TestMeasuredMu_ASlowReplicaCountsInTheSample`). The readings that reach the
fold have passed the settle, joined, filling and served-bound tests. Only replicas that are themselves admitted saturated
readings (`P1-obs`, after the settle test) read into a sample at all, so on a
fleet the router loads unevenly the idle replicas neither enter nor dilute it
(`TestMeasuredMuAndK2_IgnoreTheIdleReplicasOfAnImbalancedFleet`: 1 of 4
saturated learns the balanced fleet's mu and k2). With a max across replicas the sample was the hottest of
N readings, and the window's median rose with the number of saturated replicas:
with 6 % reading noise on a true 6.0 req/s, +5.4 % at one replica and +13.6 %
at sixteen. Per replica it stays at the one-replica +5.4-5.5 % at any size
(`TestMeasuredMu_DoesNotGrowWithTheSaturatedFleet`).

An admitted k2 reading is not always a service rate. `throughputRefusal`
(`throughput_admission.go`) also refuses, logging
`saturated-throughput-refused`, a reading from a replica that completed no
request over the window (`RequestRate` 0: every sequence is still growing),
one whose generated tokens run more than `MaxTokensOverCompletions` (1.15x)
ahead of what its completions account for (`tokensOutrunCompletions`, the
same fill a minute on; not judged for prefill),
one under half of what a Ready replica of the variant served this cycle
(`servedBounds`, logged as `servedBound`: a served rate is a lower bound on
`mu`, so this is one slow replica), and one under `MinMeasuredOfDerived`
(0.5) of the derived mu for the same line. Only queued replicas are read, so
one slow replica -- a degraded GPU, a CPU-starved pod -- is often the only
reading: in review one queued at 0.3 req/s beside three serving 3.6 unqueued
was confirmed (below) and priced the fleet at 45 replicas for a load four
serve, and 55.5 for 30 minutes after it recovered; it now reads 3.8 and 4.0
(`TestMuChoice_OneSlowReplicaIsNotTheFleetsMu`). The bound is 2x, not 1x:
equally saturated replicas read a few percent apart. On
E1 a replica just joined, 84 waiting and no completion, generated 49 tokens/s:
0.0995 req/s became the bucket's mu against a derived 3.1-5.7 and ~6.2 served,
and priced 2 req/s as 12-20 replicas for the rest of the run
(`TestE1Replay_AJoiningReplicaIsNotAThroughputSample` replays it). A filling
replica reads low for a minute or so; one replica that reads under half the
derived mu on every saturated cycle for `MeasuredLowConfirmFor` (2m, no gap over
a minute) is a real low rate, the line the wrong figure, and its readings are
recorded from then on (`saturated-throughput-low-confirmed`,
`TestMuChoice_ALowRateThatRepeatsIsRecorded`). The runs live in
`muChoice.low`. A reading the served rate contradicts is never confirmed.

A filling replica can read high as well. On the E1 rerun two replicas that
had joined a minute before, still queued, read 6.70 and 6.99 req/s from
their token rate while they completed 5.08 and 4.86. Their batches were
still growing, so tokens went to sequences that had not completed yet. The
two readings became the window's median, and sg1 was priced at 6.70 against
the 6.0-6.1 its saturated replicas completed. That is 11 % high, which
under-orders. Full replicas past their fill read 0.90-1.07x tokens over
completions across 24 scrape intervals of the same run. The filling
readings read 1.32x and 1.44x, so a reading over 1.15x is refused. Such a
replica is also left out of `servedBounds`
(`TestE1_AFillingReplicaDoesNotRaiseMu`, which read 6.70 before).

## Demand

Two sources, added.

### A replica's own demand

```
replicaDemand = residentCharge(rm, rm.TokensInUse, capacity, routing) + waitingQueueDemand(rm, role)
```

`residentCharge` is `rm.TokensInUse`, the collector's one-minute peak, except
for a Ready replica with nothing waiting that is idle NOW (`KvUsageInstant`
under `idleKvUsage`, with `KvUsageInstantKnown` set) while a replica of its
role is busy now (`cycle.busyNowRoles`, from `rolesBusyNow`): that one is
charged `KvUsageInstant x capacity`, never above the peak
(`replicaRouting.kvFromInstant`). Summed per pod, peaks count a load the router
moved within the minute once on every pod it visited: four pods each full for
15 s, one full now, read 4,416,512 tokens for a load of 1,173,863. A replica
with requests waiting keeps its peak: its instant KV dips between prefill
batches and on preemption, and charged at the dip a fleet queued everywhere
ordered nothing (instants 0.9/0.2/0.2/0.2 priced 3,161,293 against 4,616,512).
A missing instant series is not a reading of zero: the collector sets
`KvUsageInstantKnown` only when the series was read (per pod, only when every
rank reported one), and without it the peak stands. With no replica busy now
every peak stands. Only demand reads this; `kvFull`, k2 and its admission keep
the peak.

This rule counts one load once in time; it is not a routing compensation and
moves no demand between replicas. The role's charge never falls under the sum
of what its replicas hold now: a replica busy now keeps its peak, one idle now
is charged its instant reading, and no queue is discounted.

The waiting queue is charged in full, on whichever replica it stands. Demand
and supply are both sums over the role's replicas, so a queue on the wrong
replica is in the sum either way, and taking it out can only hide demand. An
earlier revision moved the share a settled-idle sibling had room for out of
`LocalQueueDemand` and out of the floor's backlog, and charged it nowhere: in
review a two-replica variant at 0.93 of its supply read 0.50, three replicas
queued 60 each beside a fourth never routed to implied 5.88 replicas against
the 9.41 the fleet needed, and 439 prompts on one of four prefill pods priced
2,169,104 tokens against the 2,866,448 resident plus queued
(`idle_room_test.go`, `imbalance_test.go` pin the full charge). Uneven routing
is reported by the load-balance diagnostic, not compensated here; where it
moves demand it raises it.

`settledIdleDecode` (`idle_room.go`) still reads settled idleness, for one
purpose: a decode replica idle for `IdleSettledFor` (2m) relaxes the hold on
prefill's demand while decode is saturated (`applyFloorAndHolds`), which can
only raise prefill's order. Idle is `pricedIdle`: `replicaIdle` (Ready,
busiest-rank KV under `idleKvUsage` = 0.30 of the cache, nothing waiting) AND
busiest-rank tokens under 0.30 x `EffectiveCapacity` (min(k1, k2)), on every
cycle for `IdleSettledFor`, no two readings over `IdleMaxGap` (1m) apart, own
replicas only. The line is a band (#396): a run starts under `idleKvUsage` and,
once started, goes on under `idleBandLevel`, `IdleLeaveRatio` (1.4) times it
and never more than halfway to a full replica, on both figures. With one line,
a replica whose peak sat on 0.30 settled idle and cleared again on noise
(property case `edges/idle/idle-line-030`: 6 reversals in 116 cycles on 98 of
100 seeds). The runs live in `settleMemo.idle`. The band is the replay switch
`SwitchIdleBand`.

### The scheduler queue

`estimateSchedulerQueueDemand`. The router's queue holds requests no pod has
started, so it is charged separately:

```
tokensFromBytes = QueueBytes / BytesPerToken        // BytesPerToken = 4
tokensFromCount = QueueSize x avgInput
inputTokensRaw  = max(tokensFromBytes, tokensFromCount)

inputTokens        = inputTokensRaw x (1 - avgHitRate)
prefillInputTokens = inputTokensRaw x (1 - prefillHitRate)
outputTokens       = QueueSize x avgOutput
```

Attribution depends on the fleet's shape, because on a P/D split the two roles
owe different halves of the same request:

| fleet | prefill is charged | decode is charged |
| --- | --- | --- |
| P/D split | `prefillInputTokens` | `outputTokens` |
| single role | — | `inputTokens + outputTokens` |

The two hit rates are not the same figure: prefill is discounted at **its own**
role's hit rate, because prefill is where the prefix cache lives.

`withExpectedOutputTokens` fills `avgOutput` for output-generating replicas
reporting none, so a cold fleet does not price its queue at zero — `mu` divides
by `avgOutput`, and a fleet that has completed nothing would otherwise divide by
zero and emit no floor at all, in the one window the projection exists for.

With no replica reporting a shape at all (parked at zero, or Ready without
engine series), `estimateSchedulerQueueDemandAt` falls back for each half on
its own: `avgInput` to the fleet's tracked prompt length (`stableInput`; the
bytes still win when larger) and `avgOutput` to the same expected output
length, unless prefill is the only active role. Before, 100 queued requests
were worth 0, 100 000 or 51 200 tokens depending on whether there were zero
replicas or one without series and whether the queue reported bytes.

## Aggregation

`internal/engines/aggregation`, over `[]domain.VariantCapacity`:

```
perReplica(vc)         = vc.PerReplicaCapacity   (0 if non-positive or +Inf)
TotalSupply            = SUM vc.ReplicaCount x perReplica(vc)
TotalAnticipatedSupply = SUM (vc.ReplicaCount + starting(vc)) x perReplica(vc)
TotalDemand            = SUM vc.TotalDemand
```

`AggregateByRole` computes all three per role. The asymmetry is the point:
starting replicas count toward supply (so the engine does not double-order
while pods launch) but not toward removable capacity.

## RC and SC

Not the analyzer's own. `applyUniversalThreshold` in
`internal/engines/steadystate/engine_v2.go` recalibrates them at every scope
from one formula:

```
RC = max(0, TotalDemand / scaleUp   - TotalAnticipatedSupply)
SC = max(0, TotalSupply  - TotalDemand / scaleDown)
```

`scaleUp` defaults to `0.85`, `scaleDown` to `0.70`. Between
`scaleDown x supply` and `scaleUp x anticipated` the engine neither orders nor
releases.

`atLeastZero` floors these, and rejects NaN and `+Inf` explicitly: demand is
deliberately not sanitised on the way in, so this is where a non-finite figure
has to fail closed. `v < 0` would not do it — every comparison against NaN is
false.

## The throughput floor

`internal/signals/floor`. Occupancy alone under-sizes a fleet that is keeping
up, because a fleet exactly keeping pace looks half-empty. The floor sizes the
role from arrival rate and backlog instead. Per role:

```
cost   = median over the role's own replicas of (P / mu)   // tokens per unit arrival rate
         x scaleUp for a derived mu                        // replicaCost
mu     = median over the role's own mus
horizon = max(drainSeconds, startSeconds[role])            // BacklogDrainSeconds = 60
b      = backlogAtLanding(observed, lambda, mu, startSeconds, ready, starting)
rate   = lambda + b / horizon
floor  = rate x cost
replicasImplied = rate / mu
```

Medians, not maxima: one replica draining a batch must not set the role's price.

The `x scaleUp` on a derived mu is not extra headroom; it removes a second
copy. The engine scales out at `scaleUp` of what a replica is worth
(`RC = D / scaleUp - anticipated`). A measured mu is the rate at saturation, so
one `scaleUp` is right for it. A derived mu is already the rate at `kPrice`,
the target fill, so the engine's division applied the headroom twice: on the
defaults the floor scaled out at 85 % of the rate at 0.68 fill, which with
throughput concave in fill is about 0.32 fill (97 sequences on the run that
found it, against 205 at 0.68). With the factor, a replica at `kPrice` serving
`mu` arrivals reads `RC = 0` from occupancy and from the floor alike
(`TestThroughputFloor_AgreesWithOccupancyAtTheTarget`).

The projection is what makes a backlog an *order* rather than a residency:

```
arrived   = lambda x T                                     // T = startSeconds
credit    = SUM startingCredit(T, pending, ages)
served    = mu x (ready x T + credit)
projected = max(0, backlog + arrived - served)
```

A replica ordered now lands in `T` seconds; by then the queue has grown by
`lambda x T` and the ready fleet has served `mu x ready x T`. A replica already
starting is credited for the part of `T` it has left — with no age information,
`pending x T / 2`.

Two hold rules gate ordering, both measured rather than reasoned:

- `MinThroughputSamplesToOrder = 2` — two readings a
  `ThroughputSampleSpacing` (1m) apart, so the second cannot come from the same
  drain as the first.
- A **derived** mu may order immediately: it is priced for the shape that
  changed to, which is the whole reason it exists.

Both are covered in [analyzer-evidence.md](analyzer-evidence.md).

## Shape change

`shape_change.go`. One `shapeMemo` per `namespace|modelID`, guarded by
`SaturationAnalyzer.mu`.

- `noteFleetShape` records this cycle's shape and declares a change when it
  falls outside `Within(tolerance)` of the stable one. It stamps `changedAt`
  (the outstanding hold) and `lastChangedAt` (the fact of the change).
- `settleFleetShape` clears the hold once the fleet has measured itself.
- `shapeChangedWithin(window)` answers `now - lastChangedAt < window`.

The two timestamps are deliberately separate. `DisableShapeChangeHold` leaves
`changedAt` unset for ever, so anything keyed on the hold would see one cycle
of shape change and then none — putting the mu divisor back on the `[5m]` mean
for the rest of the straggler window, silently, through a flag documented as
only turning off the fleet hold.

`ShapeChangeHoldMax = 5m`, overridable per policy via `ShapeChangeWindow`.

### Output buckets

Throughput readings are keyed by output length so a reading taken under one
shape cannot price another:

| bucket | upper bound (tokens) |
| --- | --- |
| short | 100 |
| medium | 500 |
| long | 1500 |
| extra long | 3000 |
| very long | 6000 |

Prefill is keyed `prefillOutputBucket = "noout"` instead: it emits about one
token per request, so its output length is not a property of its work.

The k2 history is keyed by output length too (`k2KeyOutput` in `keys.go`),
but by the fleet's length, not the replica's own average: a workload at
`max_tokens=500` reads per-replica averages of 482-582 and, keyed on those,
alternated between the `medium` and `long` windows cycle by cycle, each holding
a different k2 (260 622 and 187 458 on the run that showed it). Prefill keeps
its own average, which is always `short`. The key strings are unchanged, so
stored windows are still found.

The fleet's length is not enough on its own either. The key used to read the
tracked length, which the shape tracker moves on one reading 20 % off its
anchor; with the length 6 % above 500 and 6 % noise it moved either side of
500, so the key went to `medium` and back (#396; property case
`edges/k2/key-near-output-boundary`, 13 of 50 seeds). The key now holds its
bucket by a band of its own (`keyLengths`, `k2_key_bucket.go`):

- It is judged on the fleet's length smoothed over `K2KeyBucketSmoothing`
  (10 min; the mean of every reading until then, so one early reading does not
  choose the starting bucket). It leaves its bucket when that is
  `K2KeyBucketMargin` (3 %) past the bucket's boundary.
- A swap: a length that stays `K2KeyBucketSwap` (20 %, the tracker's
  tolerance) past the boundary on every cycle for `K2KeyBucketDwell` (2m) moves
  the key to its own bucket and restarts the smoothing there. While a swap is
  pending the key holds its bucket, so a swap does not stop in the buckets
  between.
- Both rules are the same in both directions, no single reading moves the key,
  and a cycle with no length keeps the bucket.

The cost is staleness, for a fleet that is not saturated. A saturated fleet is
priced from its observation (`P1-obs`), whatever the key, and refills the
window it writes within a few cycles. One that keeps up is priced from the old
bucket's window (`P2-hist`) until the key moves: 2 minutes for a swap of 20 %
or more past the boundary, about 7 minutes for 530 -> 440 tokens, about 14 for
530 -> 470.

When the key moves, the bucket it lands in usually has no window, and k2 would
fall to k1 (`P4-k1`): about 60 % more capacity per replica on the test rig,
which a fleet that keeps up never corrects, as it never saturates there. The
old key made that move only on a tracker change of 20 %; this one also makes
it on the 10-20 % shifts the old key ignored. So a move seeds the new bucket's
lower bound with the variant's window from the bucket it left
(`seedMovedK2Key`), as the variant's own previous figure: `P2-prior`, lower
only, held 30 minutes, then relaxed to the variant's own capacity over 6
hours, and out of use once the new bucket has a window of its own
(`TestK2KeyBucket_AMovedKeyIsPricedFromTheBucketItLeft`: 8 000, not 12 800).
A move is logged at `--v=3` as `k2-key-bucket-moved`.

The memo is not persisted: after a controller restart the key chooses its
bucket again from its first readings, as the shape tracker does. The band is
the replay switch `SwitchK2KeyBucketHeld`; off, the k2 key reads the tracked
length as before.

The throughput (mu) key reads the same band (#415). It used to read the tracked
length, and on the same boundary case (`edges/mu/key-near-output-boundary`)
it left `long` for `medium` on 13 of 50 seeds: measured mu was read from,
and written to, another bucket's window at constant load, and k2's settle test
(`k2Unsettled`) compared a throughput reference from one bucket with a k2
reference from another. With one band the two keys sit in one bucket on every
cycle (`TestMuKeyBucket_TheKeysShareABucket`). The switch is
`SwitchMuKeyBucketHeld`; off, the throughput key reads the tracked length.

- **A pending swap freezes the throughput window** (`muSwapFrozen`). While a
  swap is pending the key still names the bucket it is leaving, and for a swap
  to longer outputs that bucket's measured mu is the higher one: on the
  1000 -> 5500-token shape-swap spec the first swapped cycle priced the old
  bucket's 3.08 against the new shape's 1.40, at full authority, and wrote the
  new shape's readings into the old window. So while a swap is pending the
  window takes no writes (counted as `swap-pending` in the mu learner's
  report) and its figure counts as borrowed: the derived mu prices where there
  is one, and the floor may hold the fleet on the window but not grow it
  (`TestMuKeyBucket_APendingSwapFreezesTheWindow`). Prefill is never frozen:
  its key has no output bucket
  (`TestMuKeyBucket_APendingSwapLeavesPrefillWriting`). The switch is
  `SwitchMuSwapFreeze`.
- **The freeze's cost.** Up to `K2KeyBucketDwell` (2 minutes) on the derived
  figure at a real swap. Under noise the same: the noise is correlated over
  minutes, so an excursion past the swap line can last the whole dwell, and
  one that lasts it lands a swap on noise, which now moves the throughput key
  too. Measured on `k2KeyMemo.note`, 50 seeds of 480 cycles at 15 s, noise
  with a 0.9 one-cycle correlation:

  | Fleet | Noise | Cycles frozen | Longest freeze | Swaps landed on noise |
  | --- | --- | --- | --- | --- |
  | long, 530 tokens | 6 % | 0 % | none | 0 |
  | long, 530 tokens | 10 % | 0.95 % | 8 cycles | 2 |
  | long, 530 tokens | 15 % | 5.4 % | 8 cycles | 36 |
  | 490 tokens | 10 % | 1.5 % | 8 cycles | 5 |
  | 490 tokens | 15 % | 6.4 % | 8 cycles | 41 |
  | medium, 110 tokens | 15 % | 3.9 % | 8 cycles | 24 |
  | long, 1400 tokens | 15 % | 2.9 % | 8 cycles | 14 |

  After a swap more than 20 % past a boundary, the shape-change hold settles
  two spaced samples after the swap lands, so it lasts the dwell longer than
  it did. A move by the margin is not affected.
- **A move by the margin clamps writes** (`marginClamp`). A length past the
  margin but short of the swap line (400-485 tokens out of `long`) moves the
  key only once the smoothed length crosses the margin: 5 min 15 s for
  530 -> 420, 13 min 45 s for 530 -> 470. Until then the key names `long`,
  and the shorter shape's readings, about 1.26x the requests/s at 420, would
  raise its median for when the fleet returns. So while this cycle's length
  is past the margin toward shorter outputs, a reading is written as the
  lower of itself and the window's median (nothing, when the window is
  empty), counted as `margin-clamped`: it can lower the window and never
  raise it. Readings toward longer outputs are lower, the safe side, and are
  written as read. On 530 -> 420 with a thin `long` window
  (`TestMuKeyBucket_AMarginMoveNeverRaisesTheWindow`), over the 21 cycles
  before the key moves: main, whose tracker moves the throughput key at once
  on this 21 % shift, leaves `long` at 3.10; the band without the clamp
  raises it to 3.86; with the clamp it stays at 3.10. The switch is
  `SwitchMuMarginClamp`. A freeze on every cycle past the margin, in either
  direction and pending swaps included, would have frozen 8.6 % of cycles at
  6 % noise around 530 (runs up to 26 cycles) and 22-33 % at 10-15 %. The
  clamp applies on fewer cycles, those toward shorter outputs with no swap
  pending, and on those it still writes, and only lowers:

  | Fleet | Noise | Cycles clamped | Longest run |
  | --- | --- | --- | --- |
  | long, 530 tokens | 6 % | 8.5 % | 26 cycles |
  | long, 530 tokens | 10 % | 18.1 % | 24 cycles |
  | long, 530 tokens | 15 % | 18.9 % | 25 cycles |
  | 490 tokens | 6 % | 3.9 % | 33 cycles |
  | 490 tokens | 10 % | 7.5 % | 23 cycles |
  | 490 tokens | 15 % | 10.0 % | 21 cycles |

  (`k2KeyMemo.note`, 50 seeds of 480 cycles at 15 s, cycles with a swap
  pending, which are frozen instead, left out.) The cost is a window biased
  low near a boundary with large swings: the lower readings of the noise are
  written as read, the higher ones of the shorter side capped at the median.
  A clamped reading never raises the window by being appended; a sample still
  being folded across pods can move by the pods already in it (a review probe
  saw a rise over the previous step on 0.17 % of clamped writes, and none over
  the median the clamping started from).
- **k2's settle test** still reads the frozen window as its throughput
  reference (`k2Unsettled`, `serviceRateOnRecordLocked`), at full weight, as
  before: that is the k2 window's own hold.
- **The bucket left does not expire.** `k2KeyMemo.left` stands until the
  next move, so a key whose new window is later swept can be carried from a
  bucket left hours earlier. The carry is lower only and borrowed, so that is
  never the risky side; the memo is swept with the model and not persisted.
- **A moved key is carried lower only** (`carriedThroughput`). When the band
  moves the throughput key to a bucket with no window, it borrows the nearest
  neighbour as before (`nearestSaturatedThroughput`, the shorter bucket on a
  tie), or the bucket it left where that is lower. A key that moves to a longer
  bucket with no derived mu is still priced from the shorter bucket's higher
  mu, as a borrow: that is the neighbour borrow's behaviour, not changed here.

## Prefill capacity, observed

The what-if PR P1 ([what-if capacity](../proposals/slo-capacity-planner.md),
the PR plan) learns what the end-to-end TTFT model
([end-to-end TTFT](../proposals/slo-capacity-planner-ttft.md)) needs of a
prefill pod, and only logs it. `internal/signals/prefill` holds the
estimators; `prefill_observe.go` feeds it each prefill variant's rows after
the replicas are priced, with this analyzer's decode back-pressure
(`decodeSaturated`) and the prefill lower-bound window (`prefill_bound.go`,
passed through `prefillBounds`). Nothing sizes from it.

| Figure | From | Refused |
| --- | --- | --- |
| `V_P`, computed tokens/s | busy minutes (median of at least 5 over the last 30 minutes, `prefill.BusyWindow`; a minute is busy when the row's `QueueMin` was read and is above zero, the pod's KV is not full and decode is not back-pressuring; a lower bound, `DataParallelRanks`, when any came from a multi-rank pod; switch `prefill-busy-minutes`), else the peak under a queue sustained over 3 minutes, else the back-pressure bound (at least 2 readings). The last two are lower bounds. `QueueMin` is collected in a namespace with a TTFT target and while a learner recording is on, so elsewhere in a default production install only the peak under a queue is live; the bound needs `enablePrefillThroughputBound` | `NoPrefillCapacity`, logged with `queuedMinutes` (over the 6 h kept), `longestQueuedRun` and `busyMinutes` (over the 30-minute window; not comparable with `queuedMinutes`) to say which source fell short, with `busyMinutesOn`, `queueMinPods` and `kvFullPods`; `SGLangNoComputedTokens` on SGLang |
| `a`, seconds | an untrimmed fit of `AvgServiceTime` against the effective prompt (`PrefillComputedTokenRate / RequestRate`) over low-load minutes (no queue, occupancy at most 0.1, prompt steady within 10% of every reading in the last 2 minutes, with at least one there), plus the least `AvgTTFT - AvgServiceTime`. `1/slope` is logged as `slopeBound`, an upper bound on `V_P` for validation only. On `ols-sweep`: intercept 8.36 ms, overhead 2.68 ms, `a` 11.04 ms, `slopeBound` about 520k tok/s | aggregated, fewer than 5 minutes, prompts spanning less than 1.5x between the second shortest and the second longest (so each end holds two minutes), one prompt length throughout (`NoPromptVariance`), a non-positive slope or intercept |
| `f` | the busiest pod's share of computed tokens over the mean, less the expected max of `N_p` Poisson counts, over minutes with at least 100 requests a pod on average; median of at least 5. It reads low on a real imbalance at few requests (2:1:1:1 reads about 1.50 for 1.6 at 100 a pod) and high on equal pods with variable prompts (about 1.03 at `Cs²` = 1); `imbalance.go` says why. Above 10,000 requests a pod the Poisson excess is its normal approximation (within 0.33% of the exact sum there, up to 64 pods), so its cost does not grow with the rate | `OnePod`, `NoComputedTokens`, `TooFewImbalanceMinutes` |

"Cannot over-state" holds up to the noise of `rate()`: the peak under a queue
is the max, over up to 6 hours, of each 3-minute window's least one-minute
rate, up to 358 overlapping windows a replica, so it keeps the luckiest one.
`vpLowerBound` keeps its name with that limit stated at the field
(`prefill.Estimate`).

A row is read only from a Ready, non-bridge replica settled for 2 minutes on
the settle tracker. Samples are spaced a minute apart and kept 6 hours, at most
360 a replica; the state is not persisted. It is keyed by the variant key's fields (namespace,
model, variant, the accelerator read through `stableAcceleratorFor`, which
touches nothing, and GPUs) and the engine arguments (`ArgsTag`, behind `#`),
so a redeploy under other arguments starts its own `V_P`, `a` and `f` and the
old key ages out; an image change alone does not.
`llm-d.ai/forget-learned-state` forgets it: `forgetVariant` drops the
variant's keys (not counted in its return), and `forgetLearnedState` drops
every key. The analyzer's eviction sweep (`EvictStaleHistory`) forgets a key
with no cycle for 6 hours, so a key does not outlive its variant or its
model, and counts it as `prefillObservations`. The back-pressure bound is
stashed for the cycle that read it only, at the highest a replica of the
variant read: the cycle takes its model's stashed bounds, those of variants
it does not have included; a cycle whose pricing fails clears them too;
`forgetVariant` drops the variant's; and the eviction sweep drops any left
over a minute, counted as `prefillBounds`.
The line is `prefill-capacity-observed`, at `--v=3`, one per prefill variant
per cycle. The replay switch is `prefill-capacity-observe`; it moves no
golden output.

## Constants

| constant | value | package |
| --- | --- | --- |
| `DefaultKvCacheThreshold` | 0.80 | `internal/config` |
| `DefaultScaleUpThreshold` | 0.85 | `internal/config` |
| `DefaultScaleDownBoundary` | 0.70 | `internal/config` |
| `DefaultQueueLengthThreshold` | 5 | `internal/config` |
| `DefaultChangeTolerance` | 0.20 | `internal/signals/shape` |
| `DefaultKSat` | 0.85 | `internal/signals/itl` |
| `DefaultMinObservableK` | 0.15 | `internal/signals/itl` |
| `DefaultMaxObservableK` | 0.80 | `internal/signals/itl` |
| `DefaultMinSamples` | 10 | `internal/signals/itl` |
| `DefaultMinKSpread` | 0.30 | `internal/signals/itl` |
| `DefaultClearMinSamples` | 8 | `internal/signals/itl` |
| `DefaultClearKSpread` | 0.18 | `internal/signals/itl` |
| `DefaultTierDwell` | 10m | `internal/signals/itl` |
| `DefaultTierSafeMargin` | 0.03 | `internal/signals/itl` |
| `DefaultSteadyFor` | 2m | `internal/signals/itl` |
| `DefaultMaxKDrift` | 0.10 | `internal/signals/itl` |
| `BacklogDrainSeconds` | 60 | `internal/signals/floor` |
| `MinThroughputSamplesToOrder` | 2 | `internal/signals/floor` |
| `ShapeChangeHoldMax` | 5m | `saturation` |
| `DecodeSaturationMemory` | 1m | `saturation` |
| `K2SettleRateFraction` | 0.7 | `saturation` |
| `K2SettleMaxRise` | 0.25 | `saturation` |
| `K2SettleLookback` | 1m | `saturation` |
| `K2SettleMaxGap` | 2m | `saturation` |
| `balance.IdleKvUsage` (as `tuning.idleKvUsage`) | 0.30 | `internal/signals/balance` |
| `IdleSettledFor` | 2m | `saturation` |
| `IdleMaxGap` | 1m | `saturation` |
| `IdleLeaveRatio` | 1.4 | `saturation` |
| `K2KeyBucketSmoothing` | 10m | `saturation` |
| `K2KeyBucketMargin` | 0.03 | `saturation` |
| `K2KeyBucketSwap` | 0.20 | `saturation` |
| `K2KeyBucketDwell` | 2m | `saturation` |
| `JoinedReplicaWindow` | 1m | `saturation` |
| `MinMeasuredOfDerived` | 0.5 | `saturation` |
| `MaxTokensOverCompletions` | 1.15 | `saturation` |
| `MeasuredLowConfirmFor` | 2m | `saturation` |
| `MeasuredDerivedMaxRatio` | 2.0 | `saturation` |
| `MeasuredDerivedReturnRatio` | 1.6 | `saturation` |
| `MeasuredMuFreshFor` | 5m | `saturation` |
| `MeasuredLowHoldFor` | 30m | `saturation` |
| `SiblingMuTolerance` | 0.8 | `saturation` |
| `FloorLagFraction` | 0.5 | `saturation` |
| `FloorTrustFraction` | 0.8 | `saturation` |
| `BacklogGrowthLookback` | 1m | `saturation` |
| `BacklogGrowthMinRequests` | 2 | `saturation` |
| `BacklogGrowthFraction` | 0.10 | `saturation` |
| `ThroughputSampleSpacing` | 1m | `saturation` |
| `DefaultExpectedOutputTokens` | 512 | `saturation` |
| `BytesPerToken` | 4 | `saturation` |
| `EpisodeGap` | 24h | `internal/signals/capacity` |
| `HistoryEvictionTimeout` | 24h | `internal/signals/capacity` |
| `HistoryRetention` | 7d | `internal/signals/capacity` |
| `EvictionTimeout` | 7d | `internal/signals/capacity` |

Most of the `saturation`, `itl` steady-gate and `balance` constants above are
**defaults of policy keys**, `analyzerTuning` and `loadImbalance`
(`internal/config/analyzer_tuning.go`; operator docs in
[Advanced tuning](../reference/scaling-policy.md#advanced-tuning-analyzertuning-and-loadimbalance)).
The `saturation` ones alias the `config.Default*` values, so they cannot drift.
The values in force are read once per cycle: `tuningOf(cfg)` (`saturation/tuning.go`)
fills the resolved policy's unset fields and is kept on `cycle.tune`, which
`computeReplicaCapacity` is handed; `applyThroughputFloor`, called once per
cycle, builds it from the `cfg` it is handed. The judgements take it as a
receiver or an argument, never from a package variable. The ITL gate reaches
`itl.Tracker.Observe` as `itl.Admission.Steady`, an `itl.Steadiness` mapped from
the keys by one constructor, `config.SteadyGateTuning.Steadiness`, in both the
saturation and the slo analyzer (`ScalingPolicy.ITLSteadiness`). The
diagnostic gets `balance.Thresholds` from `steadystate.loadBalanceThresholds`;
`Tracker.Measure` keeps them per model for `Observe` and `Failed`. Window-tied
values (`K2SettleLookback`, `K2SettleMaxGap`, `IdleSettledFor`, `IdleMaxGap`)
are multiples of `ThroughputSampleSpacing`, and `balance.StaleAfter` is the
model's `sustainFor`; none has a key of its own.
`TestTuningDefaultsAreTheCalibratedConstants` pins every default to the value
it had before it was configurable.

The first three are **defaults, not the values in force**. They are assembled into a
`capacity.Horizons` and handed to the analyzer by `WithHorizons`, which the engine
fills from `Config.LearnedStateHorizons()` — `LEARNED_STATE_EPISODE_GAP`,
`LEARNED_STATE_VARIANT_TIMEOUT`, `LEARNED_STATE_BUCKET_RETENTION`. `Horizons.Sanitized` floors each
field at `MinHorizon` (1m) independently and names the ones it replaced, so a single
unusable value does not discard the other two. A zero field takes its default without
being named, so an unconfigured controller logs no error. Zero is also what viper
returns for a value that was set and does not parse (`7d`) or was set to `0`; the
loader reports those itself (`learnedHorizon` in `internal/config/loader.go`), where
the value as written is still known. Read the pair in force off
`a.horizons`; `EvictStaleHistory()` takes no arguments, because a horizon passed per
sweep is a horizon no operator can reach.

### How long learned state lives

A sweep runs once per cycle from the engine, at the top, before any capacity is
computed. Three horizons govern it, and the differences are load-bearing —
getting any of them wrong silently changes replica counts.

**Per-variant state** — the fitted ITL window and its baseline, the replica
start estimate, its outlier counter and the set of pods it refused
(`startOutlierPods`) — ages on `HistoryEvictionTimeout`
measured from when the variant was last *reported*, or last found to exist:
the engine stamps every active and every parked variant just before the sweep
(`notePresentVariants` → `NoteVariantsPresent`). It is a liveness stamp,
because an empty ITL window is not evidence that a variant is gone: a healthy
fleet below k=0.15 offers readings every cycle and holds none, so sweeping on
emptiness deleted the state of exactly the fleets that were doing fine.

**Bucket-keyed windows** — the k2 history and the mu windows the demand floor
prices from — are keyed by workload bucket as well as by variant, and a bucket
the variant has stopped serving must expire while the variant lives, so they
do not use that stamp. They are kept for `HistoryRetention` (7d) from
when they were last **read**: `Touch()` on the read path is what makes "last
read" the measure. Their only writer needs a saturated queue, so aged on last
*write* they would expire on exactly the fleets that are coping, and the figure
a decision depends on every cycle would vanish because the fleet was healthy.

**There is deliberately no trust horizon on a retained measurement.** However
old the figure is, while the window exists it answers. This follows from
`effectiveCapacity = min(k1, k2)`: a measured k2 can only ever pull capacity
*below* k1, so it is the most conservative figure available for its bucket, and
everything it could fall through to — the derived figure, or k1 itself — is
greater than or equal to it. Refusing a measurement can therefore only raise
capacity and order fewer replicas. An intermediate revision of this code added
a 24h trust horizon and fell through to k1; measured across k1 regimes, that
cost between 1.8× and 48× the replicas. Priority 1 replaces the figure on the
first saturated cycle regardless.

**Two timestamps, not one.** `RollingAverage` tracks `lastUsed` and
`lastWritten` separately, because three different questions were being asked of
one field:

| question | field | asked by |
| --- | --- | --- |
| keep this entry? | `lastUsed` | the sweep, via `StaleAt` |
| is a new observation part of this window, or a new episode? | `lastWritten` | the write paths, via `WriteGapExceedsAt` |

Neither is ever read against the wall clock. Every method takes `now` from its
caller, and the analyzer passes its own clock, which the engine pins once per
cycle (`internal/signals/cycleclock`): every stamp in one cycle is the same
instant, and a test that moves the analyzer's fake clock past `EpisodeGap`
sees the window reset. `TestNoWallClockInLearnedState` fails on a `time.Now`,
`time.Since` or `time.Until` in the capacity package or the saturation,
throughput and slo analyzers.

Collapsing them is a live defect: reads happen every cycle, so one field made
every read look like a write, and the write path's "a long gap means a new
episode, so reset rather than blend" check could never fire for any bucket
still being served. Two saturation episodes weeks apart then blended into one
rolling average — measured at 3.85× for k2 and 7.9× for mu, both in the
fewer-replicas direction.

**Two memos age on the *bucket* horizon despite being per variant**, because
each one's value resolves into a bucket key: `lastAccelerator` (into the k2
history key, the throughput key and the ITL window key) and `fleetShape` (into
the throughput key). Expiring either ahead of the window it keys moves the key
to `unresolved` or to a different bucket and makes a retained measurement
unreachable — the measurement is still in the map, and nothing will ever ask for
it again. Both are also stamped when they are **read**, not only when written,
for the same reason the windows are: the read path is the one that runs every
cycle. Those two are the whole set; a new memo whose value reaches a bucket key
has to join them.

**Capacity records** age on `EvictionTimeout` (7d), so a variant parked at zero
over a weekend keeps its engine params and capacity record. The store has no
trust horizon on reads either; `Store.IsStale` and `StalenessTimeout` (30 min)
exist but have no caller on the reconcile path.

Net: at the defaults, a variant that is *deleted* loses its fitted per-variant
figures 24h later. One that is merely parked keeps them, because
`notePresentVariants` stamps every variant that exists just before the sweep,
including on the cycle a long-parked fleet wakes. Seven days without a read costs the bucket's measurement, parked or
not, and seven days costs the capacity record.

**The horizons tick on cycles, not on wall clock.** The sweep is called from
`optimizeV2`, and `optimize` returns early on "No active VariantAutoscalings
found" — so a fleet with *every* variant parked stops sweeping until something
wakes. A variant's state therefore ages out only while some variant is still
live, which is the normal case. This is not a second leak: a parked fleet
learns nothing new, so what it holds is bounded by what it had when it parked.
It does mean that a test which parks the whole fleet and waits measures
nothing — park one variant and leave another serving.

## Reading a decision from the logs

Three lines carry the whole chain. `derived-mu` and `replica-capacity-decision`
are written once per replica, so they sit at `perReplicaLogLevel`
(`logging.VERBOSE`, `--v=3`; `replica_log.go`); at the shipped `logging.DEFAULT`
the per-variant `replica-capacity-summary` stands in for them.
`throughput-demand-floor` is at Info:

- `derived-mu` — every term of the mu derivation, because a mu wrong by 8x
  looks identical in the log whether the fault is the output length, the
  sequence count, the ITL line or the pricing point. `kvReqPerSeq` is the
  priced shape and `kvReqFleet` the fleet's own; `outputDispersion` is the
  spread of output lengths priced into `kvReqPerSeq` (1 is none, or none
  read); `residentKVPerSeq` is the KV the running requests were measured to
  hold, over `residentRunning` requests, and `residentKVBound` which bound
  `kvReqPerSeq` held it to (`none`, `lower`, `upper`, `no-hit`,
  `no-reading`, `off`; issue #277), and `residentKVBoundFleet` the same for
  `kvReqFleet`;
  `muShortWindow` says whether the pair moved to the short window.
  It, and every ITL line (`itl-window`, `itl-fit`, `itl-fit-declined`,
  `itl-line-borrowed`, `itl-line-none`, `itl-gps-mismatch`), carries `namespace` beside
  `variant`: one Deployment applied to two namespaces logs the same variant
  name in both, and only the namespace tells their lines apart.
- `replica-capacity-decision` — `k1MemoryBound`, `k2ComputeBound`, `k2Source`,
  `boundBy`, `saturatedThroughput`, `saturatedThroughputBucket`.
- `throughput-demand-floor` — `arrivalRate`, `backlogRequests`,
  `saturatedThroughput`, `perReplicaCapacity`, `replicasImplied`,
  `heldAtFleet`, `heldWhy`.
