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

### From the engine's own `/metrics`, via `collector`

Every field below is `domain.ReplicaMetrics`. The PromQL lives in
`collector/registration/saturation.go` and `queueing_model.go`, per engine.

| field | from | consumed by | for |
| --- | --- | --- | --- |
| `TotalKvCapacityTokens` | `cache_config_info`, or `NumGpuBlocks x BlockSize` | `k1`, `deriveMu` | the KV budget `C` |
| `NumGpuBlocks`, `BlockSize` | `vllm:cache_config_info` | the above | when capacity is not direct |
| `TokensInUse` | `kv_cache_usage x capacity` | `replicaDemand`, observed k2 | occupancy as tokens |
| `KvCacheUsage` | `vllm:gpu_cache_usage_perc` (1m max) | saturation test | is this replica full |
| `KvUsageInstant` | the same, instantaneous | `itl.Fit`, `GPSErrorPct` | the `k` an ITL sample was taken at |
| `QueueLength` | `vllm:num_requests_waiting` (1m max) | saturation test, `waitingQueueDemand` | is this replica queued |
| `AvgInputTokens` | `request_prompt_tokens_sum/count` `[5m]` | `shape.New`, k2 | `I` |
| `AvgOutputTokens` | `request_generation_tokens_sum/count` `[5m]` | `shape.New`, k2, queue pricing | `O` |
| `AvgInputTokensRecent` | the same counters `[1m]` | `resolvePricing` | `I` during a shape change |
| `AvgOutputTokensRecent` | the same counters `[1m]` | `resolvePricing` | `O` during a shape change |
| `PrefixCacheHitRate` | `prefix_cache_hits / queries` | `shape.New`, prefill's queue charge | the discount on `I` |
| `AvgITL` | `inter_token_latency_seconds_sum/count` | `itl.Fit` | the ITL sample |
| `AvgTTFT` | `time_to_first_token_seconds_sum/count` `[1m]` | the prefill TTFT model (proposed) | prefill's ceiling |
| `GenerationTokenRate` | `rate(generation_tokens_total)` | `noteLineMismatch`, measured mu | observed tok/s |
| `PrefillComputedTokenRate` | `rate(request_prefill_kv_computed_tokens_sum)`, **summed** across a pod's engines | `saturatedCompletionRate` | prefill's measured mu |
| `RequestRate` | `rate(request_success_total)` | `fleet.Mean` weighting | how much each replica counts |
| `AvgServiceTime` | `request_inference_time_seconds` (SGLang: e2e − queue) | the retired Little's-law floor | — |

Two collector behaviours that change the numbers above and are easy to miss:

- **`pod_collapse.go` is per field.** A pod's engines have their token *rates*
  **summed** (each computes part of the prefill, so they add) and their
  *latencies* **weighted by request rate** (they are per-request costs). Using
  one rule for both halved a multi-engine replica's capacity.
- **`attribute.go` discards rather than passes through.** A not-Ready pod's
  timing metrics are dropped, and so is any latency longer than the pod has
  existed. Both fail *open* when the bound cannot be established — an
  unreadable Pod listing must not delete every reading in the namespace.

### From the engine's launch flags, via `signals/capacity`

`capacity.EngineParams`, parsed from container args by `deployment_parser.go`
and `sglang_parser.go` — **not** scraped.

| parameter | flag | consumed by | symbol |
| --- | --- | --- | --- |
| `EffectiveMaxBatchedTokens` | `--max-num-batched-tokens` | `estimateCapacityFromParams` | `B` |
| `MaxNumSeqs` | `--max-num-seqs` | `estimateCapacityFromParams`, `deriveMu` | `S` |
| `TotalKvTokensOverride` | computed/declared KV budget | `capacityTokensFor` | `C` when the live figure is absent |

### From the EPP inference scheduler

`domain.SchedulerQueueMetrics` — the router's queue, work **no pod has
started**, which is why it is charged separately from replica demand.

| field | consumed by | for |
| --- | --- | --- |
| `QueueSize` | `estimateSchedulerQueueDemand`, the floor's backlog | request count |
| `QueueBytes` | the same | the token estimate, `/ BytesPerToken` |

### From Kubernetes

| parameter | source | consumed by |
| --- | --- | --- |
| `Role` (`prefill`/`decode`/`both`) | variant labels via discovery | every role split |
| `GPUsPerReplica` | the scale target's pod spec | ITL key, allocation |
| `AcceleratorName` | node labels via discovery | ITL key, k2 key — pooling two GPU products under one key is the PR #40 oscillation |
| `CurrentReplicas`, `PendingReplicas`, `PendingAges` | Deployment / LWS status | supply, anticipated supply, the floor's starting credit |
| `MinReplicas`, `MaxReplicas` | the ScaledObject | `roleAtCeiling`, allocation |
| `Ready`, `StartSeconds` | Pod status | the collector's guards; the floor's horizon `T` |

### From the `wva-scaling-policy-config` ConfigMap

| parameter | default | consumed by |
| --- | --- | --- |
| `kvCacheThreshold` | 0.80 | `k1`, `pricingK` |
| `scaleUpThreshold` | 0.85 | `RC`, `pricingK`, the holds |
| `scaleDownBoundary` | 0.70 | `SC`, the holds |
| `queueLengthThreshold` | 5 | the saturation test, throughput keys |
| `shapeChangeHoldSeconds` | 5m cap | `ShapeChangeWindow`, the fleet hold |

### From ScaledObject trigger metadata

Per workload, beside `modelID`. Validated at the trigger that carried it,
because that error is the operator's only view of a bad value.

| key | consumed by | for |
| --- | --- | --- |
| `modelID` | everything | identity |
| `defaultOutputTokens` | `ExpectedOutputTokens` | the queue's price before any replica has measured one |
| `replicaStartSeconds` | the floor's horizon | when no start has been observed |
| `scalingPolicy` | tier selection | which policy applies |

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
