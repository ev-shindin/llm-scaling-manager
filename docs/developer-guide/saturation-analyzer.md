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
4. Fit one ITL line per decode variant ([The ITL line](#the-itl-line)).
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
KVreq    = ILeff + OL/2
```

`ILeff` is the prompt the engine must actually compute — a prefix-cache hit
costs nothing. `KVreq` is the KV footprint of **one resident request**, averaged
over its life: it holds the whole prompt throughout, and its generation grows
from nothing to `OL`, so the generation contributes its mean, `OL/2`.

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

`estimateCapacityFromParams()` in `replica_capacity.go`, from the engine's own parameters
(`capacity.EngineParams`, parsed from the deployment):

```
B = EffectiveMaxBatchedTokens
S = MaxNumSeqs
I = avgInputTokens
O = avgOutputTokens

N_steady = min(B x O / (I + O), S)
k2       = floor(N_steady x (I + O/2))
```

`N_steady` is how many requests a continuous-batching engine holds in steady
state: each step spends `B` token-slots, a request needs `I` of prefill once
and `O` decode steps, so the fraction of a request's life that is decode is
`O/(I+O)` and the batch sustains `B·O/(I+O)` of them — capped by the engine's
own `--max-num-seqs`. Multiplying by `(I + O/2)` converts a count of requests
into the tokens they occupy, the same per-request footprint as `KVreq`.

`computeK2` has a priority order (observed, historical, derived, fallback);
`k2Source` in the `replica-capacity-decision` log line says which answered.
k2's persistence is deliberate and load-bearing — see
[analyzer-evidence.md](analyzer-evidence.md).

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

where `k` is instantaneous KV usage (`rm.KvUsageInstant`, 0–1). Fitted by
ordinary least squares over a rolling window of `(k, ITL)` observations:

```
A = (n x SUM(k x itl) - SUM(k) x SUM(itl)) / (n x SUM(k^2) - SUM(k)^2)
B = (SUM(itl) - A x SUM(k)) / n
```

A fit is refused unless it is usable (`ValidModel`): `A` and `B` finite,
`A > 1e-12` (a flat or falling line is not this physics), and
`A x 0.85 + B > 0`.

The window (`itl.Window`) will not hand over a line until it has evidence:

| constant | value | why |
| --- | --- | --- |
| `DefaultMinSamples` | 10 | a line through noise is not a line |
| `DefaultMinKSpread` | 0.30 | samples clustered at one `k` fix an intercept, not a slope |
| `DefaultWindowMaxSize` | 20 | |
| `DefaultObservationMaxAge` | 30m | |
| `DefaultMinObservableK` | 0.15 | below this the engine is not batching |
| `DefaultMaxObservableK` | 0.80 | above it, preemption makes ITL non-linear |

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
ways to get it, and the derived one is preferred because it can be priced for a
shape the fleet has never run.

### The pricing point

```
kPrice = clamp(kvCacheThreshold x scaleUpThreshold,
               DefaultMinObservableK, DefaultMaxObservableK)
```

`pricingK()`. With the shipped defaults that is `0.80 x 0.85 = 0.68`, **not**
`0.85`. This composition is easy to get wrong: `k_sat` (0.85) is where the ITL
line is validated, while 0.68 is the occupancy the autoscaler actually targets.

### The derived mu

`deriveMu()` in `mu_from_itl.go`:

```
C        = capacityTokensFor(params, rm.TotalKvCapacityTokens)
seqs     = min(Sequences(kPrice, C, KVreq), MaxNumSeqs)
tokenSec = seqs / ITL(kPrice)
mu       = tokenSec / avgOutput
```

It declines (`ok: false`, and the floor then emits nothing for that role) when
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

## Demand

Two sources, added.

### A replica's own demand

```
replicaDemand = rm.TokensInUse + waitingQueueDemand(rm, role)
```

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
mu     = median over the role's own mus
horizon = max(drainSeconds, startSeconds[role])            // BacklogDrainSeconds = 60
b      = backlogAtLanding(observed, lambda, mu, startSeconds, ready, starting)
rate   = lambda + b / horizon
floor  = rate x cost
replicasImplied = rate / mu
```

Medians, not maxima: one replica draining a batch must not set the role's price.

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
| `BacklogDrainSeconds` | 60 | `internal/signals/floor` |
| `MinThroughputSamplesToOrder` | 2 | `internal/signals/floor` |
| `ShapeChangeHoldMax` | 5m | `saturation` |
| `DecodeSaturationMemory` | 1m | `saturation` |
| `ThroughputSampleSpacing` | 1m | `saturation` |
| `DefaultExpectedOutputTokens` | 512 | `saturation` |
| `BytesPerToken` | 4 | `saturation` |
| `HistoryEvictionTimeout` | 24h | `internal/signals/capacity` |
| `HistoryRetention` | 7d | `internal/signals/capacity` |
| `EvictionTimeout` | 7d | `internal/signals/capacity` |

### How long learned state lives

A sweep runs once per cycle from the engine, at the top, before any capacity is
computed. Three horizons govern it, and the differences are load-bearing —
getting any of them wrong silently changes replica counts.

**Per-variant state** — the fitted ITL window and its baseline, the replica
start estimate and its outlier counter — ages on `HistoryEvictionTimeout`
measured from when the variant was last *reported*. It is a liveness stamp,
because an empty ITL window is not evidence that a variant is gone: a healthy
fleet below k=0.15 offers readings every cycle and holds none, so sweeping on
emptiness deleted the state of exactly the fleets that were doing fine.

**Bucket-keyed windows** — the k2 history and the mu windows the demand floor
prices from — are keyed by workload bucket rather than by variant, so they
cannot use that stamp at all. They are kept for `HistoryRetention` (7d) from
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
| keep this entry? | `lastUsed` | the sweep, via `Stale` |
| is a new observation part of this window, or a new episode? | `lastWritten` | the write paths, via `WriteGapExceeds` |

Collapsing them is a live defect: reads happen every cycle, so one field made
every read look like a write, and the write path's "a long gap means a new
episode, so reset rather than blend" check could never fire for any bucket
still being served. Two saturation episodes weeks apart then blended into one
rolling average — measured at 3.85× for k2 and 7.9× for mu, both in the
fewer-replicas direction.

**The accelerator memo** (`lastAccelerator`) ages on the *bucket* horizon
despite being per variant, because its value resolves into the k2 history key.
Expiring it ahead of the window it keys moves the key to `unresolved` and makes
a retained measurement unreachable.

**Capacity records** age on `EvictionTimeout` (7d), so a variant parked at zero
over a weekend keeps its engine params and capacity record. The store has no
trust horizon on reads either; `Store.IsStale` and `StalenessTimeout` (30 min)
exist but have no caller on the reconcile path.

Net: 24h of quiet costs the *fitted* per-variant figures; seven days costs the
bucket's measurement and the capacity record. A fleet parked past 24h
re-measures its ITL baseline and start estimate on wake.

## Reading a decision from the logs

Three lines carry the whole chain, all at `logging.DEFAULT`:

- `derived-mu` — every term of the mu derivation, because a mu wrong by 8x
  looks identical in the log whether the fault is the output length, the
  sequence count, the ITL line or the pricing point. `kvReqPerSeq` is the
  priced shape and `kvReqFleet` the fleet's own; `muShortWindow` says whether
  the pair moved to the short window.
- `replica-capacity-decision` — `k1MemoryBound`, `k2ComputeBound`, `k2Source`,
  `boundBy`, `saturatedThroughput`, `saturatedThroughputBucket`.
- `throughput-demand-floor` — `arrivalRate`, `backlogRequests`,
  `saturatedThroughput`, `perReplicaCapacity`, `replicasImplied`,
  `heldAtFleet`, `heldWhy`.
