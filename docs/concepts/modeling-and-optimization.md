# Modeling and optimization

llm-scaling-manager decides how many replicas each variant of each model
needs, as the request rate and request shape (input and output token counts)
change. It is a *global* autoscaler, not a set of per-Deployment ones. Each
cycle it solves every variant together against one accelerator budget, using
the available accelerators, live load, each model's SLOs, and workload
priority.

This page covers the terms of that decision, what is modelled, and how
per-variant demands become a replica count. For the measurement
path that produces those demands, read
[the steady-state engine](steady-state-engine.md) first.

## Definitions and assumptions

- An **accelerator** is a unit of GPU allocation of one type and count. For example, 2xH100 is one accelerator made of two H100 GPUs, sized for the model's memory or performance needs.

- An **accelerator arrangement** consists of one or more accelerators in parallel (tensor or pipeline parallelism) assigned to a model server.

- A **variant** is a set of model servers (**replicas**) serving one model on the same accelerator arrangement. In the running system it is exactly one KEDA ScaledObject and the workload it scales. A replica belongs to the variant whose managed scaler its `ownerReferences` lead to, and the variant is named after that scaler.

- The **model SLOs** define target values for two metrics:

  1. *TTFT*: queueing time plus waiting for and running prefill.
  2. *ITL*: the decode time for one output token. Congestion stretches it: batching, prefill injected into a long decode cycle, and KV-cache effects such as memory swapping.

- **Workload priority** (or **criticality**) says how important one application's requests are. Other components use it differently: an admission controller to choose which requests to drop, a request scheduler to order its queue. Here it decides which variants get accelerators when there are not enough to meet every model's SLOs.

## What is modelled, and what is not

Everything below is learned from live metrics. There is no performance-profile
store, no offline benchmarking step, and no lookup per model-accelerator pair.
A variant never observed under load gets a conservative estimate. That is a
deliberate trade, and the project proposal states its cost: a shape has to
saturate once before it can be priced well.

**Per-replica capacity, in KV-cache tokens.** A replica's usable capacity is
the smaller of a memory ceiling (its physical KV-cache size scaled by
`kvCacheThreshold`) and a compute ceiling (how many concurrent tokens it can
process before scheduling, not memory, is the limit). A variant's capacity is
the median across its ready replicas. This makes a decision divisible: a
replica count is `ceil(required tokens / per-replica capacity)`, so one cycle
can order three replicas, not just one.

**The saturated completion rate, per role.** The demand floor is priced
against mu: the completion rate one replica sustained when last seen
saturated, recorded per output-length bucket. Occupancy belongs to the fleet
and falls as replicas are added. mu belongs to the hardware and the request
shape, and does not move with the fleet. A fleet needs `lambda / mu` replicas
to keep up. Prefill's mu is in prompt tokens: the tokens one replica computes
per second, at the prompt length arriving. A prefill that has only ever queued
behind a full decode has a lower bound on it instead. See
[the throughput floor](steady-state-engine.md#the-throughput-floor).

**Inter-token latency as a function of KV utilization.** A separate throughput
analyzer fits `ITL(k) = A*k + B` over observed KV utilization `k` by ordinary
least squares, evaluates it at the saturation point to derive a per-replica
decode rate, and compares that against arrival rate
(`internal/signals/itl`, `internal/engines/analyzers/throughput`). It is not
enabled in the shipped configuration, which names the saturation analyzer only.

**What is not modelled.** There is no queueing-theoretic model of the server,
no batch-size-to-latency fit, and no parameter tuner. Earlier versions of this
page described all three; none of it is in this repository. There is also no forecasting. The decision
uses what is measured now: an operator can read and argue with it, but it
cannot arrive before the load.

## Optimization

The optimizer turns each analyzer's per-variant demand into a replica count for
every variant of every model, inside whatever accelerator budget the limiter
declares. Two things decide the outcome.

**Cost, when there is room.** Variants are ranked by serving capacity per unit
of cost (the relative `variantCost` key in the ScaledObject trigger). The *most
efficient* variant grows first, which is not always the cheapest. A TP=2
variant at twice the cost of a TP=1 variant is the better buy whenever it
serves more than twice as much. See `cost_aware_optimizer.go` and
`greedy_score_optimizer.go`.

**Priority, when there is not.** When the budget cannot meet every model's
demand, allocation is a priority-weighted fair share: a variant's `priority`,
which normally comes from its [policy tier](../well-lit-paths/workload-classes/),
is the weight in that split (`fairShareValue`). The optimizer records what it
could not give: which variant was limited, by what, and how many GPUs it got.
That shows up as `wva_model_scaling_blocked` with a reason, not as silence.

**Constraints, not modes.** Limiters are ordinary constraints on the optimizer:
GPU inventory, declared quota, or none. The `limiters:` list on the scaling
policy chooses them; there is no mode switch. See
[bounding a fleet by real GPUs](../well-lit-paths/bound-by-gpus/) and
[GPU capacity accounting](gpu-capacity-accounting.md).
