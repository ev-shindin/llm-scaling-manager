# The steady-state engine

llm-scaling-manager is a **global optimizer**. Each cycle it builds a capacity model of
your inference workloads and computes how many replicas each needs. It never patches replica
counts: it publishes a target over the KEDA external-scaler contract, and KEDA's HPA acts on it.

This page explains the **steady-state engine** for operators who tune and observe it: its
terms, the metrics it reads, its thresholds, and how it turns them into replica targets. Every
configuration field and the EPP coordination workflow are in
[Saturation Scaling Configuration](../reference/scaling-policy.md); every metric is in
[Metrics & Health Monitoring](../reference/metrics.md).

## Terminology

| Term | Meaning |
|---|---|
| **Model** | A served model (a `modelID`, e.g. `Qwen/Qwen3-0.6B`) in a namespace. The unit a scaling decision is made for. |
| **Variant** | One deployable configuration of a model — a Deployment/LeaderWorkerSet, typically pinned to an accelerator type (and, for disaggregated serving, a role). A model may have several variants (e.g. A100 vs H100, or prefill vs decode). |
| **Replica** | One running pod of a variant. |
| **Role** | For prefill/decode-**disaggregated** serving, a variant serves the `prefill` or `decode` stage; non-disaggregated variants are `both`. |
| **EPP** | *Endpoint Picker* — the request router in the llm-d inference scheduler ([Gateway API Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension)). EPP spreads requests across replicas; the scaling manager decides how many replicas exist. |
| **Capacity / supply** | How much work a replica (or the whole model) can serve, measured in **KV-cache tokens**. |
| **Demand** | How much work the model currently needs to serve, also in KV-cache tokens. |

## Overview

There is one saturation analyzer: the token-based engine described here. The
earlier percentage-based analyzer has been removed, so there is nothing to choose.

| Aspect | **Token-based analyzer** |
|---|---|
| Reasons in | Absolute **KV-cache tokens** (supply vs. demand) |
| Scale-up size | a division: `ceil(required tokens / per-replica capacity)` |
| Multi-replica jumps | native |
| Heterogeneous accelerators | first-class (per-variant capacity) |
| Zero-replica sizing | supported (capacity derived from deployment args) |
| GPU-constrained fair-share | supported (with the limiter) |

Because it models capacity in KV-cache tokens, one decision can jump from, say,
2 → 5 replicas, or size a variant that has **zero** replicas.

### Configuring it

The analyzer runs unconditionally. The shipped saturation-scaling ConfigMap names
it explicitly on the global `default` entry, which is the recommended form:

```yaml
analyzers:
  - name: saturation
    score: 1.0
```

The legacy `analyzerName: "saturation"` spelling is still accepted. Per-model and
per-namespace entries can override *thresholds*.

## How it works

Each cycle, for every model, the engine runs four core steps (plus an optional fifth when the GPU
limiter is enabled).

### 1. Per-replica capacity = the tighter of two ceilings

A replica stops at whichever limit it hits first, **memory** or **compute**. Its usable
capacity is the **smaller** of two ceilings, both in KV-cache tokens:

- **Memory ceiling** = `KV_token_capacity × kvCacheThreshold`.
  `KV_token_capacity` (= `num_gpu_blocks × block_size`) is the pod's physical KV-cache size,
  read from `vllm:cache_config_info` (or derived from the deployment's engine args when the
  metric is absent). `kvCacheThreshold` (default **0.80**) reserves headroom: only 80% of the
  cache counts as usable.
- **Compute ceiling** = how many concurrent tokens the GPU can *process* before scheduling
  throughput, not memory, is the bottleneck (common for long generations). The engine estimates
  it from live queue behavior when a replica is queue-saturated, else from recent history, else
  from the deployment's batch/sequence limits. *(The
  detailed estimation chain is in the [developer guide](../reference/scaling-policy.md).)*

A variant's per-replica capacity is the **median** of its ready replicas' capacities, so one
outlier pod does not skew it. A variant with no ready replicas borrows capacity from its
deployment args or a compatible sibling variant. That is how the engine sizes a zero-replica
variant.

A **new** variant -- a canary, say -- has no compute ceiling of its own until one of its
replicas saturates. Until then it may start from a sibling's: same model, namespace,
hardware and capacity-relevant engine flags. Two rules keep that safe:

- **Only lower.** A sibling's figure is used only where it is below the variant's own
  estimate, so it can add replicas but never remove them. Too few replicas is the error
  that hurts latency; too many costs money.
- **It wears off.** A figure that is too low never corrects itself: lightly loaded replicas
  never saturate, and saturation is the only time the ceiling is measured. So the borrowed
  figure is held for 30 minutes of serving, then eases to the variant's own estimate over
  6 hours of serving, which lets a replica saturate and measure for itself. Hours spent
  parked at zero do not count (beyond a few minutes per pause, for a missed cycle), and
  once worn off it does not come back unless a sibling measures more than 20% lower or the
  variant's engine arguments change.

The same two rules cover the variant's own figure after a rollout. When its engine arguments
change -- a new image alone does not count -- what it measured before is no longer its
capacity, but it is still the best evidence there is, so it becomes a figure of the same kind:
used only where it is lower than the new estimate, and wearing off as the variant measures
itself again. It is the variant's own, not a sibling's, so turning the borrow off keeps it.

Operators can turn this off per model (`disableK2Borrow`), or make the engine forget what one
variant has learned; see [scaling policy](../reference/scaling-policy.md#disablek2borrow).

### 2. Demand — the tokens the model needs to serve

Demand is also in KV-cache tokens and sums three sources:

```text
demand = tokens_in_use          (live: kv_cache_usage × capacity)
       + local_queue_tokens     (requests waiting on the pod × avg input tokens)
       + scheduler_queue_tokens  (requests still queued upstream in the EPP flow-control layer,
                                   before they reach any pod)
```

The scheduler-queue term counts load that has not reached a pod yet. When known, the
prefix-cache hit rate discounts its input tokens.

#### The throughput floor

The three terms above are all **occupancy**: what the fleet holds right now.
Occupancy belongs to the fleet, not the load, and it falls as replicas are
added, so a fleet that keeps up looks like one that needs fewer replicas. At a
constant request rate with no queue, occupancy can point at one replica when
three are needed.

What a replica can do does not move with the fleet. The engine records **mu**,
the completion rate one replica sustained when last seen saturated, per role and
per output-length bucket, with the capacity reading taken at the same moment. A
fleet offered `lambda` requests per second needs `lambda / mu` replicas, which
in this engine's terms is a demand of `(lambda / mu) x capacity` tokens. That
figure is a **floor** under the occupancy demand: it only ever raises demand.

mu is measured per request shape, so a shape change re-prices the fleet with no
threshold re-tuned. This catches a shape change before a queue forms.

Three rules keep it honest. Each was learned from a measured mistake; the runs
are in [analyzer evidence](../developer-guide/analyzer-evidence.md):

- **The floor is not capped at the fleet's size.** What it orders depends on
  the load and the per-replica rate, not on how many replicas exist. Capped, it
  held a fleet but never grew one, and the order came after the lone replica
  had already tipped into preemption.
- **A reading the fleet has not earned may hold the fleet but not grow it.** A
  rate borrowed from a neighbouring output-length bucket, or a window with
  fewer than two saturated readings, is capped at the role's anticipated
  supply. The first reading at a saturation under-reads, and ordering on it
  over-provisions and removes the very saturation that would have corrected it.
- **A backlog is work to drain, not residency.** `B` queued requests are priced
  as `B / 60s` extra arrivals on top of `lambda`, not as `B` requests resident
  at once. Pricing them as resident sized fleets for queues that were gone
  before the replicas arrived. Prefill's share of the scheduler queue is dropped:
  more prefill replicas do not fix a full decode.

### 3. The scaling signal

The engine aggregates to model level:

- **`TotalSupply`** = Σ (ready replicas × per-replica capacity)
- **`TotalAnticipatedSupply`** = Σ ((ready + **pending**) replicas × per-replica capacity)
- **`TotalDemand`** = the demand above

Demand should sit at the target utilization of supply, so the supply *needed* is
`demand ÷ scaleUpThreshold` (at 0.85, demand should be 85% of supply). That gives two
signals:

```text
RequiredCapacity  = max(0, TotalDemand / scaleUpThreshold  − TotalAnticipatedSupply)   → scale up
SpareCapacity     = max(0, TotalSupply  − TotalDemand / scaleDownBoundary)             → scale down
```

- **`RequiredCapacity > 0`** → needed supply exceeds what is already in flight → **scale up**.
  *Anticipated* supply counts pending replicas, so an in-flight scale-up shrinks
  `RequiredCapacity` and is not counted twice.
- **`SpareCapacity > 0`** → supply is left over even with demand inflated to
  `scaleDownBoundary` → **scale-down is safe**.
- Neither → **no change**. The band between the two thresholds is the no-change region;
  keeping it wide is the main defence against flapping. A published scale-down is also held
  across cycles (`WVA_STICKY_SCALE_DOWN`, on by default). The HPA takes the maximum published
  inside its scale-down window, so one noisy sample would otherwise pin the fleet. The hold is
  counted in cycles, not seconds.

> The engine also reports `utilization = TotalDemand / TotalSupply`, but only for observability.
> The decision uses `RequiredCapacity`/`SpareCapacity`, **not** that ratio against the
> thresholds. (This `SpareCapacity` token signal is distinct from the
> `wva_spare_capacity` *metric*, which is a 0–1 headroom fraction — see [Metrics](#metrics).)

### 4. Replica target

```text
scale-up replicas   = ceil(RequiredCapacity / per_replica_capacity)
scale-down replicas = floor(SpareCapacity   / per_replica_capacity)
```

Scale-down respects each variant's `minReplicas`, removes highest-cost replicas first, and (for
prefill/decode-disaggregated models) keeps the two roles coupled so the model stays functional.

### 5. (Optional) GPU-constrained fair-share

With a `limiters:` entry declared, desired counts are capped by the GPUs available and
fair-shared across models, weighted by each model's `priority`. With no limiter, scale-up is
unconstrained and the optimizer minimizes cost. Priority applies **only** with a limiter.

## Thresholds

Set these in the saturation-scaling ConfigMap (see the
[configuration reference](../reference/scaling-policy.md) for structure and
per-model/per-namespace overrides).

| Key | Default | Range | Effect |
|---|---|---|---|
| `kvCacheThreshold` | `0.80` | 0–1 | Fraction of physical KV cache treated as usable → sets the **memory ceiling**. Lower = more headroom. **Agree with EPP — [see below](#aligning-thresholds-with-epp).** |
| `queueLengthThreshold` | `5` | ≥ 0 | Local waiting-queue depth at/above which a replica is "queue-saturated," letting its current token load set the **compute ceiling**. **Agree with EPP — [see below](#aligning-thresholds-with-epp).** |
| `scaleUpThreshold` | `0.85` | (0, 1], `> scaleDownBoundary` | Target peak utilization. Drives **`RequiredCapacity`**. Lower = scale up earlier. |
| `scaleDownBoundary` | `0.70` | (0, 1], `< scaleUpThreshold` | Utilization floor for safe scale-down. Drives **`SpareCapacity`**. The gap to `scaleUpThreshold` is the no-change band. |
| `limiters` | `[]` | list | Zero or more `{type: gpu-inventory\|quota, ...}` entries. Declaring one turns on step 5 (GPU-constrained fair-share) and arms the scale-from-zero capacity check; declaring none leaves both off. |
| `priority` | `1.0` | ≥ 0 | Relative weight when fair-sharing scarce GPUs (**limiter mode only**). |
| `analyzerName` / `analyzers` | `""` / — | — | Selects the saturation analyzer (`"saturation"`) and, via the `analyzers` list, per-analyzer scores and threshold overrides. |



**Tuning guidance**
- Keep `scaleUpThreshold` and `scaleDownBoundary` apart (e.g. 0.85 / 0.70): a narrow gap risks
  flapping, a wide gap makes scaling sluggish.
- Lower `kvCacheThreshold` if you see pressure near the KV-cache limit — and mirror it in EPP.
- Set values globally (`default`) and override per model (`{modelID}#{namespace}`).

### Aligning thresholds with EPP

`kvCacheThreshold` and `queueLengthThreshold` define **what "a saturated endpoint" means**, so
**agree them with EPP** (the request router). Both read the **same two model-server metrics**,
`vllm:kv_cache_usage_perc` and `vllm:num_requests_waiting`, but act at different layers. EPP
routes and, under load, queues or sheds requests as endpoints near saturation. The scaling
manager decides when the pool is saturated and adds replicas. EPP's Saturation Detector has two
thresholds that map one-to-one to this engine's:

| EPP (`saturationDetector` / `utilization-detector`) | Default | This engine | Default |
|---|---|---|---|
| `kvCacheUtilThreshold` | `0.8` | `kvCacheThreshold` | `0.80` |
| `queueDepthThreshold` | `5` | `queueLengthThreshold` | `5` |

If they disagree, routing and scaling fight:
- **This engine higher than EPP** → EPP queues requests in flow control (raising latency and
  this engine's `scheduler_queue` demand term) while the engine still sees headroom → scale-up
  lags.
- **This engine lower than EPP** → it scales up while EPP is still comfortably balancing →
  over-provisioning.

**Guidance:** set `kvCacheThreshold` = EPP's `kvCacheUtilThreshold` and `queueLengthThreshold`
= EPP's `queueDepthThreshold`, and mirror any change on both sides. This repo configures EPP's
scorers (`queue-scorer`, `kv-cache-utilization-scorer`) in `deploy/lib/epp-flow-control.values.yaml`.
See the developer guide's
[Coordinating with InferenceScheduler (EPP)](../reference/scaling-policy.md#best-practices-coordinating-with-inferencescheduler-end-point-picker)
for the full apply/verify workflow.

**References**
- EPP EndpointPicker configuration (`saturationDetector` fields, `headroom`, `metricsStalenessThreshold`): <https://gateway-api-inference-extension.sigs.k8s.io/guides/epp-configuration/config-text/>
- EPP model-server metrics protocol (queue size, KV-cache utilization): <https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/docs/proposals/003-model-server-protocol/README.md>

## Metrics

### Input — what the engine reads from your inference engines

Scraped from vLLM and the EPP. If they are missing or stale, the engine degrades or skips the
model; check the health metrics below.

| Metric | Type | Used for |
|---|---|---|
| `vllm:cache_config_info` (`num_gpu_blocks`, `block_size`) | info gauge | KV token capacity → **memory ceiling** |
| `vllm:kv_cache_usage_perc` | gauge | live tokens-in-use → **demand** |
| `vllm:num_requests_waiting` | gauge | local queue → **compute-ceiling** trigger + demand |
| `vllm:request_prompt_tokens_sum` / `_count` | histogram | average **input** tokens per request |
| `vllm:request_generation_tokens_sum` / `_count` | histogram | average **output** tokens per request |
| `vllm:prefix_cache_hits` / `vllm:prefix_cache_queries` | counters | prefix-cache hit rate (discounts input-token demand) — *optional* |
| `inference_extension_flow_control_queue_size` / `_bytes` | gauges (EPP) | upstream scheduler-queue demand — *optional* |

Deployment engine args (`--max-num-seqs`, `--gpu-memory-utilization`, `--block-size`,
`--max-model-len`, `--max-num-batched-tokens`, `--num-gpu-blocks-override`, …) are read from the
Deployment/LeaderWorkerSet — **not** Prometheus — and feed the derived-capacity path.

A pod that runs several data-parallel engine ranks is one replica, measured as all of its ranks.
This holds whether each rank has its own API server or the ranks share one (vLLM then labels
each series with `engine`). Its capacity is the sum over the ranks. Its cache use is used tokens
over that total, and its queue is the sum. Its latencies are means weighted by each rank's
traffic, not the busiest rank's. `--max-num-seqs` and `--max-num-batched-tokens` limit one
rank, so the pod's batch limit is the ranks times them.

Whether such a pod is saturated is judged on its busiest rank, because `queueLengthThreshold`
and `kvCacheThreshold` describe one engine. One request waiting on each of eight ranks is not
a saturated pod. One rank with a full cache and a queue at the threshold is, and the capacity
learned from it is that rank's use scaled to the whole pod. A pod with a full rank is not
released on its mean: only the other pods' spare capacity can be given back while the rank is
full.

### Output — metrics to observe the engine's decisions

| Metric | Type | Meaning |
|---|---|---|
| `wva_saturation_utilization` | gauge | Utilization = demand / capacity (0–1). |
| `wva_required_capacity` | gauge | > 0 ⇒ scale-up needed; the value is the KV-cache **token** deficit (per-role for P/D-disaggregated models). `unit=continuous`. |
| `wva_spare_capacity` | gauge | > 0 ⇒ safe scale-down headroom; the value is the KV-cache **token** surplus — the companion to `wva_required_capacity`. |
| `wva_kv_cache_tokens_used` / `_capacity` | gauges | Total KV tokens in use vs. total capacity across replicas. |
| `wva_desired_replicas` / `wva_current_replicas` | gauges | What the engine wants vs. what is running. |
| `wva_desired_ratio` | gauge | desired / current — the value HPA/KEDA actuate on. |
| `wva_replica_scaling_total` | counter | Scale actions, labeled by `direction` and `reason`. |
| `wva_config_info` | gauge | Active `analyzer_name` + feature flags — confirm the saturation analyzer is selected. |
| `wva_config_optimization_interval_seconds` | gauge | Decision cadence. |
| `wva_models_processed` | gauge | Models handled last cycle. |
| `wva_available_gpus` | gauge | GPUs available (limiter input). |
| `wva_metrics_freshness_status`, `wva_metrics_pods_discovered`, `wva_metrics_collection_errors_total` | gauges / counter | **Input health** — check these first when scaling looks stuck. |

## Operating the engine

### Verify the engine is active

Check `wva_config_info` — the active engine appears in the `analyzer_name` label:

```promql
wva_config_info{analyzer_name="saturation"}   # == 1 when the saturation analyzer is running
```

If it shows a different `analyzer_name` (or the metric is absent), it is not selected — confirm
the ConfigMap's `default` entry sets `analyzerName: "saturation"`.

### Why isn't it scaling?

Work down this list:

1. **No inputs.** `wva_metrics_pods_discovered == 0`, or `wva_metrics_freshness_status{status="stale"|"missing"}` — the controller isn't getting fresh metrics. Check pod discovery / scraping.
2. **Collection errors.** `wva_metrics_collection_errors_total` climbing — a query is failing (labeled by `query_type`).
3. **Not saturated.** `wva_saturation_utilization` is below `scaleUpThreshold` and `wva_required_capacity == 0` — expected no-change.
4. **Scale-up already in flight.** Pending replicas count toward *anticipated* supply, so `wva_required_capacity` stays `0` until they become ready.
5. **Capped by GPUs.** With a limiter declared, `wva_available_gpus` at/near 0 (or namespace quota exhausted) blocks scale-up even when demand warrants it.
6. **Actuation.** `wva_desired_ratio > 1` but replicas don't change → the KEDA object consuming the signal isn't acting; check the actuator, not the decision.

## Example configuration

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: workload-variant-autoscaler-system   # controller namespace (the name predates the rename)
data:
  default: |
    analyzerName: "saturation"   # select the saturation analyzer
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    limiters:
      - type: gpu-inventory
    priority: 1.0
  # optional per-model override. model_id and namespace in the BODY are what bind
  # it to a model — the key is only a label, and must be a legal ConfigMap key
  # ([-._a-zA-Z0-9] only, so no "/" or "#"). An entry that names no model is
  # registered as a named policy tier instead.
  my-model.my-namespace: |
    model_id: my-model
    namespace: my-namespace
    scaleUpThreshold: 0.90       # let this model run hotter before scaling
```

## See also

- [Saturation Scaling Configuration](../reference/scaling-policy.md) — full field reference, per-model/namespace resolution, and the EPP coordination workflow.
- [Metrics & Health Monitoring](../reference/metrics.md) — complete metrics catalog and health endpoints.
- [Monitoring](../reference/monitoring.md) — dashboards and observing the controller.
- [Quota Limiter](../reference/quota-limiter.md) — per-accelerator GPU caps used by the fair-share step.
