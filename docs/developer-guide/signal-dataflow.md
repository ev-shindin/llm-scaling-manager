# Which component owns which number

The chain from a metric scrape to a replica count, what each component is
responsible for, and where every parameter in the arithmetic comes from.

Two neighbours, deliberately not repeated here:

- [The saturation analyzer](saturation-analyzer.md) — the formulas in the
  analyzer's own terms, grouped by what they compute.
- [Multi-analyzer pipeline](multi-analyzer-pipeline.md) — how analyzers are
  registered, run and scored.

This page is the one to read when the question is *"where did this number come
from?"* rather than *"what does the analyzer do?"*.

## The chain

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│ SOURCES (none of them ours)                                                 │
│  vLLM / SGLang  /metrics .............. per-pod engine counters             │
│  EPP inference scheduler .............. the router's own queue              │
│  Kubernetes API ....................... Pods, Deployments/LWS, ScaledObjects│
│  Deployment / LWS container args ...... the engine's launch flags           │
│  wva-scaling-policy-config ConfigMap .. thresholds                          │
│  ScaledObject trigger metadata ........ per-workload seeds                  │
└───────────────────────────────┬─────────────────────────────────────────────┘
                                │ scraped / listed / parsed once per cycle
                                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ internal/collector                                                          │
│   registration/*.go  the PromQL templates, per engine                       │
│   extract.go         query results -> per-INSTANCE podMetricData            │
│   pod_collapse.go    a pod's engines -> one replica (sum or weight, per field)│
│   attribute.go       replica -> variant, + the not-Ready and uptime guards  │
│   freshness.go       how old each driving metric is                         │
└───────────────────────────────┬─────────────────────────────────────────────┘
                                │ []domain.ReplicaMetrics
                                │ *domain.SchedulerQueueMetrics
                                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ internal/signals/*   stateless arithmetic, no opinions about scaling        │
│   shape     ILeff, KVreq, and whether two shapes are the same shape         │
│   itl       the ITL(k) line: Fit, ITLAt, Sequences, TokenRate               │
│   capacity  EngineParams parsed from container args; the k2 history store    │
│   fleet     one request-rate-weighted mean, for every caller                │
│   floor     the throughput floor and the backlog projection                 │
└───────────────────────────────┬─────────────────────────────────────────────┘
                                │ called by
                                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ internal/engines/analyzers/saturation    ONE cycle, eight stages            │
│   newCycle ........... variant index; is decode saturated                   │
│   observeFleetShape .. one (IL, OL, hitRate) for the fleet; shape tracker   │
│   resolvePricing ..... the shape mu is priced at (both halves or neither)   │
│   fitLines ........... one ITL(k) line per decode variant                   │
│   priceReplicas ...... k1, k2, effective capacity, mu -- per replica        │
│   settleShape ........ release the hold once the fleet measured itself      │
│   priceDemand ........ variant + role totals, plus the router's queue       │
│   applyFloorAndHolds . raise demand to what the load requires               │
└───────────────────────────────┬─────────────────────────────────────────────┘
                                │ *domain.AnalyzerResult
                                │   TotalDemand, RoleDemand, VariantCapacities
                                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ internal/engines/aggregation    supply, from the SAME per-replica capacity  │
│   TotalSupply = SUM ReplicaCount x perReplica                               │
│   TotalAnticipatedSupply = SUM (ReplicaCount + starting) x perReplica       │
└───────────────────────────────┬─────────────────────────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ internal/engines/steadystate   applyUniversalThreshold                      │
│   RC = max(0, TotalDemand/scaleUp - TotalAnticipatedSupply)                 │
│   SC = max(0, TotalSupply - TotalDemand/scaleDown)                          │
│   The analyzer does NOT set these. It emits only the measured (D, P).       │
└───────────────────────────────┬─────────────────────────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ internal/engines/allocation    the optimizer                                │
│   per-role paired allocation, GPU budget / quota limiters, role ceilings    │
└───────────────────────────────┬─────────────────────────────────────────────┘
                                │ target replicas per variant
                                ▼
            KEDA external scaler  ->  ScaledObject  ->  Deployment / LWS
```

The one direction worth stating explicitly: **the analyzer never reads a
target.** It measures demand and per-replica capacity; everything about what to
do with them lives downstream. That is why a wrong decision is diagnosed by
reading the analyzer's three log lines and then the optimizer's, in that order.

## What each component is responsible for

| component | owns | does NOT own |
| --- | --- | --- |
| `collector` | turning engine counters into one record per replica, and refusing readings it cannot trust (not-Ready pods, a latency longer than the pod's uptime, NaN) | any judgement about capacity |
| `signals/shape` | `ILeff`, `KVreq`, shape equality | where the lengths came from |
| `signals/itl` | the ITL(k) line and its validity bounds | which `k` to price at |
| `signals/capacity` | parsing the engine's launch flags; persisting k2 history | the k2 *decision* |
| `signals/fleet` | one weighted mean, with zero/range handled per caller | which field to average |
| `signals/floor` | the throughput floor and the landing projection | whether the floor is allowed to order |
| `analyzers/saturation` | demand `D`, per-replica capacity `P`, role attribution, and the holds | supply, `RC`, `SC`, targets |
| `engines/aggregation` | supply and anticipated supply | thresholds |
| `engines/steadystate` | `RC` / `SC` from one formula at every scope | per-role threshold overrides (there are none) |
| `engines/allocation` | targets, budgets, ceilings | measurement |

## The arithmetic, parameter by parameter

Every formula the decision rests on, each followed by its own inputs: the unit,
the component the value comes from, what the number means, and what moves if it
is wrong. The parameters are here rather than in a catalogue of their own
because most appear in more than one formula, and the second appearance is
usually the one that surprises.

Two conventions used throughout: `I` is a prompt length and `O` a generation
length, both in tokens per request; `C` is a KV budget in tokens.

### The shape — `signals/shape`

```
hitRate = clamp(hitRate, 0, 1)        // NaN -> 0
ILeff   = I x (1 - hitRate)
KVreq   = ILeff + O/2
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `I` = `AvgInputTokens` | tokens/request | collector, `request_prompt_tokens_sum/count` `[5m]` | the prompt length the fleet is serving |
| `O` = `AvgOutputTokens` | tokens/request | collector, `request_generation_tokens_sum/count` `[5m]` | the generation length |
| `hitRate` = `PrefixCacheHitRate` | fraction 0-1 | collector, `prefix_cache_hits / queries` | share of prompt tokens served from cache |

`ILeff` is the prompt the engine must actually compute — a cached prefix costs
nothing. `KVreq` is the KV footprint of **one resident request averaged over
its life**: the prompt is held throughout, the generation grows from 0 to `O`,
so it contributes its mean.

`KVreq` is the most load-bearing quantity in the analyzer; everything that asks
"how many requests fit" divides by it. `O` reaches the replica count through
**four** independent paths — this formula, k2, the queue's price, and the
derived mu's divisor — which is why an error in it is never small.

`hitRate` is the one quantity whose **zero is a reading rather than an
absence**: a fleet with caching off reports 0 on every replica. That is why
`fleet.Mean` needs `ZeroIsAReading()` for it and must not use it for the
lengths, where zero means "this replica has completed nothing".

### Per-replica capacity — `analyzers/saturation`

```
k1        = floor(TotalKvCapacityTokens x kvCacheThreshold)
N_steady  = min(B x O / (I + O), S)
k2        = floor(N_steady x (I + O/2))
effective = k2 if k2 < k1 else k1
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `TotalKvCapacityTokens` | tokens | collector, `cache_config_info`, else `NumGpuBlocks x BlockSize` | the replica's KV cache as a token count — the budget `C` |
| `kvCacheThreshold` | fraction (0.80) | ConfigMap | the usable share; the rest is headroom |
| `B` = `EffectiveMaxBatchedTokens` | tokens/step | **container args**, via `signals/capacity` | the engine's per-step token budget (`--max-num-batched-tokens`) |
| `S` = `MaxNumSeqs` | requests | **container args** | hard admission cap (`--max-num-seqs`) |
| `TotalKvTokensOverride` | tokens | container args | `C` when no live figure exists yet (a cold replica) |

`k1` is the memory bound. The headroom is not caution: an engine driven to a
full cache preempts and thrashes. Every capacity figure scales linearly with
`TotalKvCapacityTokens`, so a wrong value sizes the whole fleet wrong by the
same factor.

`k2` is the compute bound. `N_steady` is how many requests a
continuous-batching engine sustains: each step spends `B` token-slots, a request
needs `I` of prefill once and `O` decode steps, so the decode share of its life
is `O/(I+O)` and the batch holds `B·O/(I+O)` of them — capped by `S`.
Multiplying by `(I + O/2)` converts a count of requests into the tokens they
occupy, the same per-request footprint as `KVreq`.

`B`, `S` and `TotalKvTokensOverride` are **parsed from container args, not
scraped**. That is why editing a Deployment changes computed capacity with no
metric moving, and why a k2 that disagrees with reality is often a stale parse
rather than a bad measurement.

`S` appears **twice** — here and in the derived mu — and the second is the one
people forget: above a certain cache size, mu stops responding to capacity at
all because `S` binds first.

`computeK2` has a priority order (observed, historical, derived, fallback) and
`k2Source` in the log says which answered. The *observed* k2 is
`TokensInUse` on a replica that is full and queued — a replica in that state is
reporting its own ceiling.

### The ITL line — `signals/itl`

```
ITL(k)                 = A x k + B
A                      = (n·SUM(k·itl) - SUM(k)·SUM(itl)) / (n·SUM(k²) - SUM(k)²)
B                      = (SUM(itl) - A·SUM(k)) / n
Sequences(k, C, KVreq) = k x C / KVreq
TokenRate              = Sequences / ITL(k)
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `itl` = `AvgITL` | seconds/token | collector, `inter_token_latency_seconds_sum/count` | the gap between two generated tokens |
| `k` = `KvUsageInstant` | fraction 0-1 | collector, **instantaneous** `gpu_cache_usage_perc` | the occupancy that sample was taken at |

`KvUsageInstant` and `KvCacheUsage` are separate fields on purpose:
`KvCacheUsage` is a **one-minute maximum**, used by the saturation test, where a
mean would hide a replica that spent part of the minute full. Pairing a
one-minute max with an instantaneous latency would fit the line against the
wrong `k`.

A fit is refused unless `A` and `B` are finite, `A > 1e-12` (a flat or falling
line is not this physics) and `A·0.85 + B > 0`. The window withholds a line
below `DefaultMinSamples` 10 or a `k` spread under `DefaultMinKSpread` 0.30 —
samples clustered at one `k` fix an intercept, not a slope.

`GPSErrorPct` compares `TokenRate` against the observed `GenerationTokenRate`
and **reports without acting**: a disagreement means the line is suspect, not
that the fleet should move.

### The service rate, decode — `analyzers/saturation`

```
kPrice   = clamp(kvCacheThreshold x scaleUpThreshold, 0.15, 0.80)
seqs     = min(Sequences(kPrice, C, KVreq), S)
tokenSec = seqs / ITL(kPrice)
mu       = tokenSec / avgOutput
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `kvCacheThreshold` | fraction (0.80) | ConfigMap | as above |
| `scaleUpThreshold` | fraction (0.85) | ConfigMap | the utilisation above which capacity is required |
| `avgOutput` | tokens/request | the analyzer's `muDivisor` — see below | the generation length mu is divided by |

`kPrice` is **composed**: `0.80 x 0.85 = 0.68` with the shipped defaults, **not**
the `0.85` that `k_sat` suggests. `k_sat` is only where the ITL line is
validated; 0.68 is the occupancy the autoscaler actually targets. This
composition is the single easiest thing here to get wrong.

`mu` declines (and the floor then emits nothing for that role) when the model
is unfitted, `avgOutput <= 0`, `KVreq <= 0`, `seqs <= 0`, `ITL(kPrice) <= 0`,
or `kPrice` is outside `(0, 1]`.

#### `avgOutput` and its partner, during a shape change

```
muDivisor = ExpectedOutputTokens(fleetOutput, stableOutput, defaultOutputTokens, 512)
muInput   = fleetInput
if shapeChangedWithin(window) and AvgOutputTokensRecent > 0:
    muDivisor = AvgOutputTokensRecent
    if AvgInputTokensRecent > 0:
        muInput = AvgInputTokensRecent        // and KVreq is rebuilt from the pair
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `AvgOutputTokensRecent` | tokens/request | collector, the same counters over `[1m]` | the generation length arriving now |
| `AvgInputTokensRecent` | tokens/request | collector, `[1m]` | the prompt length arriving now |
| `defaultOutputTokens` | tokens | **ScaledObject trigger metadata** | the seed used while no replica has measured one |
| `window` | seconds | ConfigMap `shapeChangeHoldSeconds`, 5m cap | how long a declared change stays outstanding |

These two `[1m]` fields exist as a **pair and are read only together**. A `[5m]`
mean carries the departing shape's stragglers; a `[1m]` mean over *completed*
requests reads low while a fleet ramps. Taking one without the other prices the
arriving generation against the departing prompt, which is a request larger
than either real shape — see
[analyzer-evidence.md](analyzer-evidence.md) for the seven-replica overshoot
that produced, and for why `max(recent, [5m])` is not the fix.

`defaultOutputTokens` matters because `mu = tokenSec/avgOutput` divides by
zero on a fleet that has completed nothing, so the derived mu reports not-ok and
the floor emits nothing — in the one window the backlog projection exists for.
A measurement always displaces it, so it is a seed and not a setting; `"0"`
means unset.

`shapeChangeHoldSeconds` has **two** consumers and conflating them was a bug:
the fleet hold, and `shapeChangedWithin` above. `DisableShapeChangeHold` turns
off only the first.

### The service rate, prefill — `analyzers/saturation`

```
mu_prefill = PrefillComputedTokenRate / ILeff     preferred
           = RequestRate                           no token counter
           = (none)                                nothing observed
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `PrefillComputedTokenRate` | tokens/s, **summed** over a pod's engines | collector, `rate(request_prefill_kv_computed_tokens_sum)` | prompt tokens prefill actually computed |
| `RequestRate` | requests/s | collector, `rate(request_success_total)` | the fallback, and the weight on every fleet average |

Tokens rather than requests, and that is measured: on run PK, at 1, 2 and 10
prefill replicas the request rate read 4.75, 4.62 and 4.50 req/s while the token
rate went 138,875 -> 933,750. **A request rate under overload is the rate the
fleet is being served at, not a capacity** — divide by it and the fleet is sized
by its own current size. `ILeff` on the denominator because the counter already
excludes cached tokens: one discount, applied to both sides.

**Summed**, not averaged, across a pod's engines: each computes part of the
prefill and their rates add. Averaging halved a multi-engine replica.

`RequestRate`'s main job is elsewhere — it **weights every fleet average**, so
the replica serving most of the traffic decides the fleet's shape. A replica
with no completions has rate 0 and cannot drag the mean.

The ITL-derived mu is **decode-only**, so prefill cannot be priced for a shape
it has never saturated under. See [Prefill, specifically](#prefill-specifically).

### Demand — `analyzers/saturation`

```
replicaDemand  = TokensInUse + waitingQueueDemand(role)
inputTokensRaw = max(QueueBytes / BytesPerToken, QueueSize x avgInput)
prefill charge = inputTokensRaw x (1 - prefillHitRate)
decode charge  = QueueSize x avgOutput            // split fleet
                 inputTokens + outputTokens       // single role
TotalDemand    = SUM vc.TotalDemand + queue charge
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `TokensInUse` | tokens | collector, `kv_cache_usage x capacity` | the cache a replica is occupying now |
| `QueueLength` | requests, 1m max | collector, `num_requests_waiting` | requests the engine accepted and has not started |
| `QueueSize` | requests | **EPP scheduler** | requests the router holds that **no pod has started** |
| `QueueBytes` | bytes | EPP scheduler | an independent estimate of the same queue |
| `BytesPerToken` | 4 | constant | the bytes-to-tokens conversion |
| `prefillHitRate` | fraction | collector, prefill replicas only | prefill's **own** hit rate, not the fleet mean |

The router's queue is charged separately from a replica's own because the
distinction is real: `QueueLength` is demand a pod already holds, `QueueSize` is
demand nothing is working on. The larger of the two queue estimates wins, so a
queue of unusually long prompts is not under-priced by a stale average.

`prefillHitRate` is prefill's own because the prefix cache lives on the prefill
side; using the fleet mean charges prefill at a discount it does not get.

### Supply, and RC/SC — `engines/aggregation` and `engines/steadystate`

```
perReplica             = vc.PerReplicaCapacity        (0 if non-positive or +Inf)
TotalSupply            = SUM ReplicaCount x perReplica
TotalAnticipatedSupply = SUM (ReplicaCount + starting) x perReplica

RC = max(0, TotalDemand / scaleUp   - TotalAnticipatedSupply)
SC = max(0, TotalSupply  - TotalDemand / scaleDown)
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `ReplicaCount` = `CurrentReplicas` | count | Kubernetes, Deployment/LWS status | ready replicas |
| `starting` = `PendingReplicas` | count | Kubernetes | replicas on their way |
| `scaleUp` = `scaleUpThreshold` | fraction (0.85) | ConfigMap | as above |
| `scaleDown` = `scaleDownBoundary` | fraction (0.70) | ConfigMap | the utilisation below which capacity is spare |

The asymmetry is deliberate: starting replicas count toward supply, so the
engine does not double-order while pods launch, but **not** toward removable
capacity. Between `scaleDown x supply` and `scaleUp x anticipated` the engine
neither orders nor releases — and the two demand holds work by clamping a
role's demand *into* that band.

`atLeastZero` rejects NaN and `+Inf` explicitly, because demand is deliberately
not sanitised on the way in and this is where a non-finite figure must fail
closed. `v < 0` would not do it.

**The analyzer does not set RC or SC.** It emits only the measured demand and
per-replica capacity; these are recalibrated downstream from one formula at
every scope, with no per-role overrides.

### The throughput floor — `signals/floor`

```
cost     = median over the role's replicas of (P / mu)
mu       = median over the role's mus
horizon  = max(drainSeconds, startSeconds[role])
b        = max(0, backlog + lambda·T - mu·(ready·T + credit))
credit   = SUM startingCredit(T, pending, ages)        // pending x T/2 with no ages
rate     = lambda + b / horizon
floor    = rate x cost
implied  = rate / mu
```

| in | unit | from | what it is |
| --- | --- | --- | --- |
| `lambda` | requests/s | the analyzer's `offeredArrivalRate` | the arrival rate the model is offered |
| `backlog` | requests | EPP `QueueSize`, charged to prefill on a split fleet | the queue a new replica would land into |
| `T` = `startSeconds` | seconds | Kubernetes Pod status, else trigger `replicaStartSeconds` | how long a replica takes to become Ready |
| `drainSeconds` | 60 | `BacklogDrainSeconds` | the horizon a backlog is priced to drain over |
| `ready` | count | Kubernetes | replicas already serving |
| `pending`, `ages` | count, seconds | Kubernetes `PendingReplicas`/`PendingAges` | replicas starting, and how far in |
| `P` | tokens | the analyzer's per-replica capacity | `effective` from above |

Medians, not maxima: one replica draining a batch must not set the role's price.

The projection is what makes a backlog an **order** rather than a residency. A
replica ordered now lands in `T` seconds; by then the queue has grown by
`lambda·T` and the ready fleet has served `mu·ready·T`. A replica already
starting is credited for the part of `T` it has left — with no per-replica age,
`pending x T/2`.

`T` matters more than it looks: too small and the floor under-orders during
exactly the window it exists for, which is why `replicaStartSeconds` is
settable per workload for the case where no start has yet been observed.

Two hold rules gate ordering, both measured rather than reasoned:
`MinThroughputSamplesToOrder` = 2 readings a `ThroughputSampleSpacing` (1m)
apart, so the second cannot come from the same drain as the first; and a
**derived** mu may order immediately, because it is priced for the shape that
changed to.

### Keys — what a reading is filed under

```
throughput key: (model, namespace, variant, accelerator, gpus, role, outBucket, queueThreshold)
  outBucket = classifyOutputLength(O)      decode
            = "noout"                       prefill
  keyInput  = ILeff                         prefill
```

| in | from | why it is in the key |
| --- | --- | --- |
| `AcceleratorName` | Kubernetes node labels, via discovery | capacity is a property of the hardware; pooling two GPU products under one key is the `k1 <-> k2` oscillation of PR #40 |
| `GPUsPerReplica` | the scale target's pod spec | the same reading means something different at a different GPU count |
| `Role` | variant labels, via discovery | prefill and decode do different work |
| `queueLengthThreshold` | ConfigMap (5) | it defines saturation, so changing it re-buckets history |

Output buckets: short 100, medium 500, long 1500, extra long 3000, very long
6000 tokens. Prefill is keyed `"noout"` because it emits about one token per
request, so output length is not a property of its work.

### Readings the collector refuses

Not a formula, but it changes every number above, and both guards fail **open**:

| guard | from | what it drops |
| --- | --- | --- |
| `Ready` | Kubernetes Pod status | a not-Ready pod's `AvgITL`, `AvgServiceTime` and `AvgTTFT` — a loading replica's latencies are not the fleet's |
| uptime bound | Pod `StartSeconds` | any latency longer than the pod has existed |
| NaN/Inf | `extract.go`, per value | `rate()` over a window with no completions is 0/0, so NaN is ordinary here |
| `FromWarmPool` | the bridge resolver | a borrowed replica contributes demand but **not** supply |

Failing open is the point: a bound that cannot be established must not delete a
reading it is unable to judge, or one unreadable Pod listing empties the
namespace.

Two fields are collected and consumed by **nothing** today: `AvgTTFT` (the
input to the proposed prefill TTFT model) and `AvgServiceTime` (kept for
diagnostics; the Little's-law floor built on it was retired, because service
time is `ITL x O` and ITL grows with the batch, so it priced the same load
differently at every fleet size and oscillated).

## Prefill, specifically

Prefill is priced by the same machinery as decode and from different numbers,
and the asymmetry is worth stating in one place because it is spread across
four files in the code.

**Its capacity bounds are the same.** `k1` and `k2` are computed identically;
prefill is a replica with a KV cache like any other.

**Its throughput key is not.** The output axis is the constant
`prefillOutputBucket = "noout"`, because prefill emits about one token per
request — its work is the prompt, so output length is not a property of it. The
input axis is `ILeff`, not the raw prompt, so a cached prefix does not inflate
the key.

**Its service rate is measured, never derived.** Three tiers, in order:

```
mu_prefill = PrefillComputedTokenRate / ILeff     preferred
           = RequestRate                           no token counter (SGLang, older vLLM)
           = (none)                                nothing observed
```

Tokens rather than requests, and this is measured rather than assumed. On run
PK, at 1, 2 and 10 prefill replicas the request rate read 4.75, 4.62 and
4.50 req/s while the token rate went 138,875 -> 933,750. **A request rate under
overload is the rate the fleet is being served at, not a capacity** — dividing
by it sizes the fleet by its own current size. `ILeff` on the bottom for the
same reason the counter excludes cached tokens: one discount, applied to both
sides of the division.

**The ITL-derived mu is decode-only.** `fitLines` populates `itlModels` for
`RoleDecode` alone, and `priceReplicas` guards the `deriveMu` call on the same
condition. ITL is the gap between two *generated* tokens, so `tokenSec/O` does
not describe a replica that emits one token and hands the KV on. The
consequence is the real gap: decode can be priced for a shape it has never
saturated under, and **prefill cannot** — its mu needs an observed saturated
cycle, and a fleet over-provisioned for the shape it just moved to never
provides one.

**Its demand** is its own resident KV and waiting queue, plus its share of the
router's queue, `inputTokensRaw x (1 - prefillHitRate)` — discounted at
**prefill's own** hit rate rather than the fleet mean, because the prefix cache
lives on the prefill side.

**It owns the router queue on a split fleet.** `queueOwner = RolePrefill`, so
the EPP backlog is charged to prefill alone rather than to every role. On a
single-role fleet it goes to the one role there is.

**Two gates exist only for prefill**, both because a prefill request completes
only when decode admits it:

1. While decode is saturated, prefill's occupancy and completion rate are
   discarded as *readings* — they are decode's backlog seen from upstream, not
   a measurement of prefill (`downstreamSaturated`).
2. And its *demand* is clamped into the band where the engine neither orders
   nor releases (`holdPrefillDemand`) — except the router-queue share, which is
   exempt, because no pod has started those requests and prefill is what they
   are waiting for whatever decode is doing.

**A role at its ceiling does not block its partner.** `roleAtCeiling`
distinguishes a role finished at `maxReplicas` from one blocked by GPU
scarcity: the first is dropped from the allocation pass so its partner still
commits, the second ends the pass. Measured: decode at 9 of max 9 left prefill
ordered 1 -> 1 in every cycle while prefill held a queue of 1119 and its own
floor asked for five.

**What is collected and not used:** `AvgTTFT`. Prefill has no fitted
throughput ceiling today. [A TTFT model is
proposed](../proposals/prefill-ttft-model.md) — fit `TTFT(T) = A·T + B` so
`1/A` is the replica's prefill token ceiling, obtainable without driving it
into saturation, which is exactly the limitation above.

## What crosses each boundary

The contracts worth knowing, because each has been broken at least once:

| boundary | carries | the failure it has had |
| --- | --- | --- |
| engine → collector | per-pod counters | a wrong metric name returns no series, the field stays 0, and the consumer silently falls back |
| collector → analyzer | `[]ReplicaMetrics` | a NaN admitted by a `<= 0` guard poisons a fleet average and every figure priced from it |
| analyzer → aggregation | `VariantCapacities`, `TotalDemand`, `RoleDemand` | a role at its ceiling treated as *blocking* starved its partner at one replica through a 1119-deep queue |
| aggregation → steadystate | supply, anticipated supply | — |
| steadystate → allocation | `RC`, `SC` | a non-finite demand walked into `RC` before `atLeastZero` rejected NaN explicitly |
| allocation → KEDA | targets | — |

## Reading one decision

Three log lines, in this order, all at `logging.DEFAULT`:

1. `derived-mu` — every term of the service rate, because the result alone
   cannot be attributed to one of them. `kvReqPerSeq` is the priced shape,
   `kvReqFleet` the fleet's own; `muShortWindow` says whether the pair moved.
2. `replica-capacity-decision` — `k1MemoryBound`, `k2ComputeBound`,
   `k2Source`, `boundBy`, `saturatedThroughput`.
3. `throughput-demand-floor` — `arrivalRate`, `backlogRequests`,
   `saturatedThroughput`, `perReplicaCapacity`, `replicasImplied`,
   `heldAtFleet`, `heldWhy`.

None of these is a metric today; [publishing them is
proposed](../proposals/signals-as-metrics.md), with the names and label keys.
