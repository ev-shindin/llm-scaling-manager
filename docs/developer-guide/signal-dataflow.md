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

## Where every parameter comes from

Each entry gives the unit, what the number means, and what moves in the
decision if it is wrong. Nothing here is optional reading if you are changing
the arithmetic: most of these appear in more than one formula, and the second
use is usually the one that surprises.

### From the engine's own `/metrics`, via `collector`

All of these are fields of `domain.ReplicaMetrics`, one record per replica. The
PromQL is in `collector/registration/saturation.go` and `queueing_model.go`,
registered per engine.

**`TotalKvCapacityTokens`** — tokens. The replica's KV cache expressed as the
number of tokens it can hold, which is the budget `C` that every "how many
requests fit" question divides. Taken from `cache_config_info` where the engine
publishes it directly, otherwise `NumGpuBlocks x BlockSize`. Every capacity
figure the analyzer produces scales linearly with it, so a wrong value is not a
subtle error — it is the whole fleet sized wrong by the same factor.

**`NumGpuBlocks`, `BlockSize`** — counts, and tokens per block. vLLM allocates
KV in fixed blocks; their product is the cache. Only used to reconstruct
`TotalKvCapacityTokens` when the direct figure is absent.

**`TokensInUse`** — tokens. How much of the cache is occupied right now,
derived as `kv_cache_usage x capacity`. Two uses: it is the resident half of
`replicaDemand`, and on a saturated replica it becomes the *observed* k2 — the
highest-priority compute bound, because a replica that is full and queued is
telling you its own ceiling.

**`KvCacheUsage`** — fraction, 0 to 1, scraped as a **one-minute maximum**. The
saturation test reads this: a replica counts as full when it crosses
`kvCacheThreshold`. The max matters — a mean would hide a replica that spent
part of the minute saturated, and saturation is the condition under which a
reading is trustworthy.

**`KvUsageInstant`** — the same fraction, **instantaneous**. A different field
on purpose: this is the `k` at which an ITL sample was taken, and the ITL line
is a function of occupancy at the moment of measurement. Pairing a one-minute
max with an instantaneous latency would fit the line against the wrong `k`.

**`QueueLength`** — requests, one-minute maximum. Requests the engine has
accepted and not started. Feeds the saturation test (full *and* queued is the
condition) and `waitingQueueDemand`, which charges them as work the replica
already owes.

**`AvgInputTokens`** — tokens per request, `[5m]`. The prompt length `I`.
Appears in `shape.New` (hence `ILeff` and `KVreq`) and in k2's `N_steady`. Too
high and every request looks more expensive than it is, so fewer fit and the
fleet is over-sized.

**`AvgOutputTokens`** — tokens per request, `[5m]`. The generation length `O`.
The most load-bearing single number in the analyzer: it sets half of `KVreq`,
it is k2's other axis, it prices each queued request, and it is the divisor of
the derived mu. An error here reaches the replica count through four
independent paths.

**`AvgInputTokensRecent`, `AvgOutputTokensRecent`** — the same two quantities
over `[1m]`. They exist as a **pair** and are read only together, while a shape
change is outstanding. A `[5m]` mean carries the departing shape's stragglers;
a `[1m]` mean over *completed* requests reads low while a fleet ramps. Using
one of the pair without the other prices the arriving generation against the
departing prompt — see the evidence doc for the seven-replica overshoot that
produced.

**`PrefixCacheHitRate`** — fraction, 0 to 1. The share of prompt tokens served
from cache, so `ILeff = I x (1 - hitRate)` is the prompt the engine actually
computes. Note this is the one quantity where **zero is a reading, not an
absence** — a fleet with caching disabled reports 0 on every replica — which is
why `fleet.Mean` needs the `ZeroIsAReading` option for it and not for the
lengths.

**`AvgITL`** — seconds per token. Inter-token latency: the gap between two
generated tokens. Paired with `KvUsageInstant` it is one `(k, ITL)` observation
for the line fit. Decode only in any meaningful sense — a prefill replica emits
about one token per request.

**`AvgTTFT`** — seconds, `[1m]`. Time to first token, which is prefill's
latency rather than decode's. Not used in a decision today; it is the input to
the proposed prefill TTFT model, where `1/A` of a fitted `TTFT(T) = A·T + B`
would be prefill's token ceiling.

**`AvgServiceTime`** — seconds. End-to-end service time per request. Kept for
diagnostics only: the Little's-law floor built on it was **retired**, because
service time is `ITL x O` and ITL grows with the batch, so the floor priced the
same load differently at every fleet size and oscillated.

**`GenerationTokenRate`** — tokens per second. What the replica is actually
emitting. Two uses: the measured mu for a saturated decode replica, and the
cross-check in `noteLineMismatch`, which compares it against what the ITL line
predicts and **reports without acting** — a disagreement means the line is
suspect, not that the fleet should move.

**`PrefillComputedTokenRate`** — tokens per second, **summed** across a pod's
engines rather than averaged. Prompt tokens prefill has computed. It is
prefill's measured mu, as `rate / ILeff`. Summed because each engine of a pod
computes part of the prefill and their rates add; averaging would report a
multi-engine replica at a fraction of its real work.

**`RequestRate`** — requests per second, from `rate(request_success_total)`.
Rarely read for its own sake: its job is to **weight** every fleet average, so
the replica serving most of the traffic decides the fleet's shape rather than
every replica counting equally. A replica with no completions has rate 0 and
cannot drag the mean.

**`Ready`, `StartSeconds`** — bool, and seconds. Pod readiness and how long it
took to become Ready. `Ready` gates whether timing metrics are trusted at all;
`StartSeconds` is the horizon `T` the floor projects a backlog over, because a
replica ordered now does not serve anything until it has started.

**`FromWarmPool`** — bool. Marks a replica lent by the warm pool. It
contributes *demand* but not *supply*, so the fleet is not sized as though
borrowed capacity were its own.

**`PodName`, `VariantName`, `Namespace`, `ModelID`, `Metadata`** — identity and
freshness. `Metadata.FreshnessStatus` carries how old the driving metrics are,
which is how a stale scrape is distinguished from an idle fleet.

Two collector behaviours change the numbers above and are easy to miss:

- **`pod_collapse.go` is per field.** A pod's engines have token *rates*
  **summed** and *latencies* **request-rate weighted**. One rule for both
  halved a multi-engine replica's capacity.
- **`attribute.go` discards rather than passes through.** A not-Ready pod's
  timing metrics are dropped, as is any latency longer than the pod has
  existed. Both fail **open**: a bound that cannot be established must not
  delete a reading it is unable to judge, or one unreadable Pod listing empties
  the namespace.

### From the engine's launch flags, via `signals/capacity`

`capacity.EngineParams`, parsed from container args by `deployment_parser.go`
and `sglang_parser.go`. **Not scraped** — which is why editing a Deployment
changes the fleet's computed capacity without any metric moving, and why a k2
that disagrees with reality is often a stale parse rather than a bad
measurement.

**`EffectiveMaxBatchedTokens`** (`B`) — tokens per scheduler step, from
`--max-num-batched-tokens`. The engine's per-step token budget. It sets how
many requests a continuous-batching engine sustains:
`N_steady = min(B·O/(I+O), S)`.

**`MaxNumSeqs`** (`S`) — requests, from `--max-num-seqs`. A hard admission cap.
It appears **twice**, and the second is the one people forget: it caps
`N_steady` in k2, and it caps `seqs` in the derived mu. A fleet whose cache
would hold thousands of sequences is still limited to `S`, so above a certain
cache size mu stops responding to capacity at all.

**`TotalKvTokensOverride`** — tokens. A declared KV budget, used by
`capacityTokensFor` when the live per-replica figure is absent (a cold replica
that has reported nothing yet).

### From the EPP inference scheduler

`domain.SchedulerQueueMetrics`. This is the router's own queue — work that has
been admitted to the system but handed to **no pod**. That distinction is the
reason it is charged separately: a replica's own queue is demand it already
holds, while this is demand nothing is working on.

**`QueueSize`** — requests. Used three ways: the request count the queue is
priced from (`QueueSize x avgOutput`), the backlog the floor projects forward,
and the arrival signal that tells the shape tracker a switch is arriving before
any completion reflects it.

**`QueueBytes`** — bytes. An independent estimate of the queue's size in
tokens, as `QueueBytes / BytesPerToken` with `BytesPerToken = 4`. The larger of
the two estimates wins, so a queue of unusually long prompts is not under-priced
by a stale average prompt length.

### From Kubernetes

**`Role`** — `prefill`, `decode` or `both`, from variant labels via discovery.
Decides every split in the analyzer: which replicas average into which shape,
which role owns the scheduler queue, and which is priced from the ITL line.

**`GPUsPerReplica`** — count, from the scale target's pod spec. Part of the ITL
and k2 keys, and the unit the optimizer spends its budget in.

**`AcceleratorName`** — from node labels via discovery. Part of the same keys,
and the reason is specific: pooling two GPU products under one key is the
`k1 <-> k2` oscillation of PR #40 on a heterogeneous cluster, and a blended ITL
line is meaningless for either product.

**`CurrentReplicas`** — count, from Deployment/LWS status. Multiplied by the
per-replica capacity to give `TotalSupply`.

**`PendingReplicas`, `PendingAges`** — count, and seconds each. Replicas on
their way. They count toward `TotalAnticipatedSupply` but **not**
`TotalSupply`, which is what stops the engine double-ordering while pods
launch while also refusing to treat a starting pod as removable. `PendingAges`
lets the floor credit a replica for the part of its start time already served.

**`MinReplicas`, `MaxReplicas`** — from the ScaledObject. `MaxReplicas` is read
by `roleAtCeiling`, and the distinction it draws matters: a role at its
administrative ceiling is **finished**, not blocking, and must not veto its
partner — a role blocked by GPU scarcity still does.

**`StuckReplicas`, `DesiredReplicas`, `Engine`** — diagnostics and the previous
cycle's target; not inputs to the capacity arithmetic.

### From the `wva-scaling-policy-config` ConfigMap

**`kvCacheThreshold`** — fraction, default 0.80. The usable share of the KV
cache; the rest is headroom, because an engine driven to a full cache preempts
and thrashes. Sets `k1` directly, and is one of the two factors in `kPrice`.

**`scaleUpThreshold`** — fraction, default 0.85. The utilisation above which
capacity is required: `RC = max(0, D/scaleUp - anticipated)`. Also the second
factor in `kPrice`, which is why the pricing point is **0.68** and not 0.85.

**`scaleDownBoundary`** — fraction, default 0.70. The utilisation below which
capacity is spare. The gap between it and `scaleUpThreshold` is the band where
the engine neither orders nor releases, and the two demand holds work by
clamping a role's demand *into* that band.

**`queueLengthThreshold`** — requests, default 5. How long a queue must be for
a replica to count as queued, which with the cache test defines saturation. It
is also part of the throughput key, so changing it re-buckets history.

**`shapeChangeHoldSeconds`** — seconds, capped at 5m. How long a declared shape
change stays outstanding. Two separate consumers, and conflating them was a
bug: the fleet hold, and `shapeChangedWithin`, which decides the mu window.
`DisableShapeChangeHold` turns off only the first.

### From ScaledObject trigger metadata

Per workload, beside `modelID` on the external-scaler trigger. Validated at the
trigger that carried it, because that error message is the operator's only view
of a bad value.

**`modelID`** — identity. Variants whose triggers name the same `modelID` are
variants of one model and are scaled as a group.

**`defaultOutputTokens`** — tokens. The generation length a queued request is
priced at while **no replica has measured one**. Without it a cold fleet values
a growing queue at the built-in 512, which under-states a 6000-token workload
twelvefold, and a queue priced low is a queue that does not order. A
measurement always displaces it, so it is a seed and not a setting; `"0"` means
unset.

**`replicaStartSeconds`** — seconds. The start time the floor projects a backlog
over before any start has been observed. Too small and the floor under-orders
during exactly the window it exists for.

**`scalingPolicy`** — names the policy tier this variant follows.

**`warmPool`, `warmPoolCopies`** — whether this variant may borrow from the warm
pool, and how many copies it wants held.

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

## The formulas, by owner

### `signals/shape`

```
hitRate = clamp(hitRate, 0, 1)          // NaN -> 0
ILeff   = I x (1 - hitRate)
KVreq   = ILeff + O/2
```

`KVreq` is the KV footprint of one resident request averaged over its life: the
prompt is held throughout, the generation grows from 0 to `O`, so it
contributes its mean. Everything that asks "how many requests fit" divides by
this.

### `signals/itl`

```
ITL(k)                 = A x k + B
A                      = (n·SUM(k·itl) - SUM(k)·SUM(itl)) / (n·SUM(k²) - SUM(k)²)
B                      = (SUM(itl) - A·SUM(k)) / n
Sequences(k, C, KVreq) = k x C / KVreq
TokenRate              = Sequences / ITL(k)
```

Refused unless `A` and `B` are finite, `A > 1e-12`, and `A·0.85 + B > 0`. The
window withholds a line below 10 samples or a `k` spread under 0.30.

### `analyzers/saturation`

```
k1        = floor(TotalKvCapacityTokens x kvCacheThreshold)
N_steady  = min(B x O / (I + O), S)
k2        = floor(N_steady x (I + O/2))
effective = k2 if k2 < k1 else k1

kPrice    = clamp(kvCacheThreshold x scaleUpThreshold, 0.15, 0.80)   // 0.68 shipped
seqs      = min(Sequences(kPrice, C, KVreq), S)
tokenSec  = seqs / ITL(kPrice)
mu        = tokenSec / avgOutput

inputTokensRaw = max(QueueBytes / 4, QueueSize x avgInput)
prefill charge = inputTokensRaw x (1 - prefillHitRate)
decode charge  = QueueSize x avgOutput         // split fleet
                 inputTokens + outputTokens    // single role
```

`kPrice` is **composed**. With the shipped defaults it is `0.80 × 0.85 = 0.68`,
not the `0.85` that `k_sat` suggests — `k_sat` is only where the ITL line is
validated.

### `signals/floor`

```
cost     = median over the role's replicas of (P / mu)
mu       = median over the role's mus
horizon  = max(drainSeconds, startSeconds[role])        // drain = 60s
b        = max(0, backlog + lambda·T - mu·(ready·T + credit))
rate     = lambda + b / horizon
floor    = rate x cost
implied  = rate / mu
```

Medians, not maxima: one replica draining a batch must not set the role's
price. `credit` is `pending x T / 2` when no per-replica age is known.

### `engines/aggregation` and `engines/steadystate`

```
TotalSupply            = SUM ReplicaCount x perReplica
TotalAnticipatedSupply = SUM (ReplicaCount + starting) x perReplica
TotalDemand            = SUM vc.TotalDemand

RC = max(0, TotalDemand / scaleUp   - TotalAnticipatedSupply)
SC = max(0, TotalSupply  - TotalDemand / scaleDown)
```

The asymmetry is deliberate: starting replicas count toward supply, so the
engine does not double-order while pods launch, but not toward removable
capacity.

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
