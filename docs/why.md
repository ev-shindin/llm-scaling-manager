# Why llm-scaling-manager

A one-page case for evaluators: what it solves, what it was measured to do,
what it costs, how mature each part is, and how to try it and back out.

## The problem

Serving LLMs on Kubernetes, the signals a general-purpose autoscaler reads do
not say how much capacity a model needs. Two requests to one model can differ
by a thousand times in work. A running-request threshold, llm-d's default
trigger, is a per-replica constant that holds for one request shape. In one
benchmark the request rate never changed and no queue ever formed, and a
KV-occupancy trigger (as in llm-d's token-aware decode recipe) would have read
5–15 % and said one replica would do; the first request shape needed two, and
the second needed three. Scale
late and time to first token spikes for the
minutes a new replica spends loading. Scale early, or keep a floor, and GPUs
sit idle. With several models on one GPU budget, whichever scales first takes
the GPUs.

## What it does

llm-scaling-manager computes each model's replica count from a capacity model
(KV cache, queues and token throughput, per replica) and decides for every
model and variant together, inside a GPU budget when you declare one
(opt-in; off by default). KEDA and the HPA carry the
decision out. Around that core:

- a **warm pool** of sleeping models that covers the minutes a new replica takes
  to load;
- **scale to zero** through KEDA's own mechanism, with activation pushed to
  KEDA within about 100 ms of a request queuing for the model;
- **GPU budgets**: the GPUs physically free, per-namespace or cluster quotas,
  or quotas read from Kueue, and an experimental optimizer that shares one
  quota between models;
- **prefill and decode** scaled as separate roles.

[How it fits together](concepts/architecture.md) has the diagrams.

## Who it is for

Teams that run [llm-d](https://llm-d.ai) on Kubernetes or OpenShift, with
several models or several ways of serving one, a GPU budget that binds, and
KEDA on the cluster.

## What operators need, and what it gives them

| What an operator running LLMs on Kubernetes needs | What llm-scaling-manager does |
| --- | --- |
| **New replicas that serve fast when traffic rises.** A replica can take minutes to load, and the spike does not wait. | A [warm pool](well-lit-paths/warm-pool-bridge/) of sleeping models serves during those minutes: measured p95 TTFT per scale-up of 0.11–0.83 s instead of 5.2–8.8 s, within 50 ms of a two-replica floor on 3 of 4 rises (831 ms against 127 ms on the fourth). `make prepull`, `make weights` and `make engine-cache` take image pulls, weight downloads and kernel compilation off the [start path](reference/workload-preparation.md). |
| **Scaling early enough**, because by the time a queue forms the new replica is minutes late. | Demand is priced from the throughput one replica sustains, so a change in request shape can order capacity before a queue exists. Measured once on the experimental P/D path, and repeated a day later: the third replica was ordered about four minutes after the shape change, once that shape's throughput was measured; KV-cache use peaked at 62 %, and no queue formed in the second phase. That was the second exposure; a controller that has never seen a shape saturated sizes it from occupancy first ([known gaps](#known-gaps)). |
| **Load measured as work, not as requests.** One request can cost a thousand times another. | Sized from KV-cache use, queues and token throughput per replica, never from request counts. Measured on the P/D path: it held 2 replicas at 5–15 % KV-cache use, where a KV threshold would drop to 1, because the load needed 2 in throughput. |
| **Idle models off the GPUs, back when needed.** Most models in a large catalogue are idle most of the time. | [Scale to zero](well-lit-paths/scale-to-zero/) is KEDA's own mechanism; the controller reads the router's queue every 100 ms and pushes activation to KEDA for the first queued request, instead of waiting for KEDA's next poll. A wake from zero is a cold start (about 41 s, measured on an H100 test cluster); a model already resident in a [warm-pool](well-lit-paths/warm-pool-bridge/) Pod switches in about 437 ms instead. |
| **Prefill and decode sized for their own bottlenecks.** | [P/D roles](well-lit-paths/pd-disaggregation/) are sized separately from their own demand and committed together, so one role cannot outrun the other. The joint commit is unit-tested only, and the P/D benchmark never ordered a second prefill replica. |
| **Many models on one GPU fleet**, where one model's growth is another's shrink. | Every model and variant is decided together, inside a GPU budget when you declare one (opt-in; off by default). Two opt-in mechanisms move GPUs between models as demand shifts: priority-weighted rescale (`enableRescale`, built and off by default, acts only under a GPU budget, unit-tested only and not benchmarked) and the experimental [utilization-share optimizer](well-lit-paths/utilization-share/). |
| **Mixed GPU generations.** | Each [variant](well-lit-paths/accelerator-variants/) carries its own per-replica capacity and cost; the optimizer grows the one with the lowest cost per unit of capacity. Not measured across two GPU types: the two-variant benchmark varies tensor parallelism on one GPU type. Unit specs assert the cost order; no end-to-end spec asserts which variant grows. |
| **Per-team GPU budgets.** | Advisory budgets; the hard bound is a namespace GPU `ResourceQuota` ([bound the namespace's GPUs at admission](guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)). [Quotas per namespace or per cluster](well-lit-paths/tenant-gpu-quotas/), [quotas read from Kueue](well-lit-paths/kueue-bounded-quotas/), or the [GPUs physically free](well-lit-paths/bound-by-gpus/). Every model is fitted inside the budget before KEDA sees a number; a capped decision raises `wva_decisions_limited_total` and a `ResourceConstrained` Event, though a fully spent quota can record neither ([monitoring](reference/monitoring.md)). The GPU bound has an end-to-end suite. Tenant quotas are unit-tested, and end-to-end: the refusal path, a Kueue-bounded entry and the share optimizer; none bounds the default optimizer by a static quota. On this release, Kueue grants are honoured only where tenants cannot create LocalQueues on queues not meant for them. |
| **Different urgency for interactive and batch work.** | [Workload classes](well-lit-paths/workload-classes/): named threshold tiers, set per ScaledObject. |
| **Decisions an operator can trust and explain**, when every replica costs dollars an hour. | A closed-form capacity model, two log lines per model per cycle that say why ([cycle log](reference/cycle-log.md)), Events and metrics for every decision. It decides; KEDA acts, so nothing new runs your pods. |

**Not covered yet:** forecasting load ahead of time, scaling to an SLA target
([#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141)),
pre-scaling before planned maintenance, and telling a failed node from a
replica that is still starting.

## What it was measured to do

All on H200 nodes unless noted. Each row is a single run per arm unless it
says otherwise; the linked page has the method and every caveat.

| Result | Number | Conditions | Source |
| --- | --- | --- | --- |
| The warm pool covers scale-ups | p95 time to first token per rise: 5.2–8.8 s without it, 0.11–0.83 s with a one-Pod pool; within 50 ms of a floor of 2 replicas on 3 of 4 rises, 831 ms against 127 ms on the fourth | two 8B models bursting out of phase, 1 GPU per replica, 4 rises per arm | [warm pool, measured](well-lit-paths/warm-pool-bridge/measured.md) |
| ... and costs less than a floor | 14 138 GPU-seconds against 16 080 for a floor of 2 replicas (12 % less); 17 % more than no pool (12 129) | same run | [warm pool, measured](well-lit-paths/warm-pool-bridge/measured.md) |
| It sizes for a request shape, not only for a queue | at a constant 6 req/s: through the first phase it held 2 decode replicas while 5–15 % KV use alone said 1; about four minutes after the shape change, once that shape's throughput was measured, it ordered a 3rd (KV peaked at 62 %); no queue formed in the second phase. p95 TTFT 155 ms, 0 errors in 13 288 requests, on the second exposure (both shapes already seen). A cold controller: p95 204 ms, p99 3.9 s, and its first five-minute window ranged from 0.25 s to 5.6 s across the two runs | Qwen3-0.6B, P/D path (experimental); **run twice, a day apart**, within a tenth except that cold window | [P/D path, measured](well-lit-paths/pd-disaggregation/#measured) |
| A warm Pod switches models fast | about 437 ms (four switches: 435, 439, 440, 435 ms) against about 41 s cold | H100 test cluster, two models resident in one pool Pod | [warm pool](well-lit-paths/warm-pool-bridge/); measurement in [fast model loading](proposals/fast-model-loading.md#27-the-pool-was-built-and-measured-2026-08-21) |
| Sharing one quota serves the first burst at once | p95 TTFT of the model whose burst comes first: 95 ms with sharing, 42 s without. The other model's burst was overloaded in every arm: 177–257 of its 960 requests shed by the router, so its p95 (59 s with sharing, 73 s without) is a floor, censored by the router's 30 s queue TTL. An earlier, aggregated run lost the second burst (p95 313 s against 139 s) | two P/D 8B models, four roles under one 10-GPU quota; costs 36 % more GPU-seconds | [utilization share, measured](well-lit-paths/utilization-share/measured.md) |

![Warm pool: p95 time to first token per scale-up against GPU-seconds for autoscaling alone, a one-Pod warm pool, and a floor of 2 replicas](figures/warm-pool-latency-vs-cost.svg)

**Not yet measured:** a head-to-head of the default saturation scaling against
plain KEDA or HPA thresholds on this engine, and repeated runs with confidence
intervals for the warm pool.

## What it costs

- **The controller** is one Deployment that reads Prometheus. It holds no GPUs.
- **The warm pool** holds its GPUs continuously: in the run above, 17 % more
  GPU-seconds than scaling without one, 12 % fewer than a replica floor.
- **Sharing a quota** spends idle quota as headroom: 24–81 % more GPU-seconds
  in the measured runs (the 81 % approximate; one run's cost was not measured).
- **Adopting it** takes a one-time cluster-admin setup per namespace, the EPP's
  `flowControl` feature gate, Prometheus scraping the model servers, and per
  workload: a ScaledObject (generated for you), a drain hook and fast starts.
- **A restart is a short scaler outage.** Until a replica holds the leader
  lease KEDA's reads fail, which only a KEDA `fallback` covers; after that, and
  until its first decision for a model, the scaler answers the current replica
  count, so the HPA holds
  ([before the first decision](reference/scaledobject-trigger.md#before-the-first-decision-and-the-fallback)).
- **What it learned is held in memory**, so a restart or leader change learns
  it again ([#137](https://github.com/ev-shindin/llm-scaling-manager/issues/137)).
- **One more port.** The scaler port is plaintext and unauthenticated by
  default. `make setup-prereqs` writes a NetworkPolicy admitting it only from
  KEDA's namespace when it knows that namespace, and warns when it does not;
  mutual TLS is opt-in and wired by hand
  ([network exposure](reference/security-model.md#network-exposure));
  [#138](https://github.com/ev-shindin/llm-scaling-manager/issues/138) makes
  it the default.

## How mature each part is

| Scenario | Status | Evidence |
| --- | --- | --- |
| [Scale on saturation](well-lit-paths/scale-on-saturation/) | Stable | end-to-end suites; one historical single-run study against a KEDA threshold (July 2026, older controller) |
| [Scale to zero](well-lit-paths/scale-to-zero/) | Stable | end-to-end suites, cluster-verified |
| [Warm pool](well-lit-paths/warm-pool-bridge/) | Stable | end-to-end suites; one benchmark run per arm |
| [Bound by GPUs](well-lit-paths/bound-by-gpus/) | Stable | end-to-end suites |
| [Tenant quotas](well-lit-paths/tenant-gpu-quotas/) | Stable | unit tests; end-to-end: the warm pool's refusal path (`warm_pool_quota_test.go`), a Kueue-bounded quota entry (`kueue_quota_test.go`), and the utilization-share optimizer under a spent namespace quota (`utilization_share_test.go`) |
| [Workload classes](well-lit-paths/workload-classes/) | Stable | end-to-end suite (runs only with the simulator) |
| [Accelerator variants](well-lit-paths/accelerator-variants/) | Stable (control path); variant choice asserted by unit specs, not end to end | a benchmark scenario and one written-up run; unit specs assert the cost order (`cost_aware_optimizer_test.go`); `limiter_test.go` runs two variants end to end, but no end-to-end spec asserts which one grows |
| [P/D disaggregation](well-lit-paths/pd-disaggregation/) | Experimental | benchmark run twice |
| [Kueue quotas](well-lit-paths/kueue-bounded-quotas/) | Experimental | unit and API-server tests; no live Kueue run |
| [Retained pool](well-lit-paths/retained-pool/) | Experimental | unit tests, a benchmark scenario and one end-to-end spec (`warm_pool_retained_test.go`, simulator only) |
| [Sharing a quota](well-lit-paths/utilization-share/) | Experimental | unit and end-to-end tests; single benchmark runs |

Each path's *How it is tested* section names its suites and what they do not
cover.

## Against the alternatives

- **Plain KEDA or HPA on a metric** (running requests, queue length, KV use):
  llm-d now recommends this, with the router's queue and running requests. Its
  running-request trigger moves before a queue forms, but each threshold is per
  workload and has to be re-tuned per model, accelerator and parallelism
  (llm-d's own caveat), and again when the request shape changes. A GPU budget
  sits below it, in Kueue, where demand over budget waits as Pending Pods
  rather than entering the decision. llm-d covers fair sharing with Kueue below
  the autoscaler; this sizes inside the budget so demand is not left Pending.
  [KEDA alone](comparison/keda-alone.md)
  compares it need by need. Measured head to head once, in July 2026, on an older
  controller and recipe.
- **llm-d's workload-variant-autoscaler**: the project this one forked from,
  now deprecated upstream in favour of the KEDA recipes above (`v0.9.0` is its
  final release; llm-d advises staying on it for now where cost-aware placement
  across variants is needed, which the recipes do not offer). Upstream already had GPU budgets (the inventory and quota
  limiters) and scale-from-zero. This fork adds a KEDA external scaler over
  gRPC with pushed activation, quotas read from Kueue, the utilization-share
  optimizer, the warm pool, the install tooling, and a capacity model that
  prices demand from measured throughput and holds through a request-shape
  change.
- **NVIDIA Dynamo Planner**: forecasts load and profiles before deployment, for
  one model on one GPU type. [The comparison](comparison/) has the detail.

## Known gaps

No load forecasting; no scaling to an SLA target, only threshold tiers
([#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141)). A model
has to saturate once before its capacity is priced for a new request shape: the
P/D result above is the second exposure, and the cold pass before it ordered a
spare replica and reached a 3.9 s p99. The warm pool and P/D numbers are single
runs per arm on one cluster type (the P/D pair repeated once).

- After a restart or leader change, the scaler holds each model at its current
  replica count until its first decision; while no replica holds the leader
  lease KEDA cannot read it at all, and only a KEDA `fallback` covers that.
- Learned capacity is held in memory and re-learned after a restart
  ([#137](https://github.com/ev-shindin/llm-scaling-manager/issues/137)).
- The scaler port is plaintext by default; a NetworkPolicy is shipped and
  mutual TLS is opt-in and manual
  ([#138](https://github.com/ev-shindin/llm-scaling-manager/issues/138) makes
  it the default).
- The Kueue quota source counts cohort borrowing only on a flat cohort
  without Fair Sharing or `effectiveQuotas`; elsewhere it caps at nominal
  ([#136](https://github.com/ev-shindin/llm-scaling-manager/issues/136)).

Planned work, with its issues, is on the [roadmap](roadmap.md).

## Trying it

1. Install it in one namespace that already runs llm-d:
   [install in a namespace](guides/install-in-namespace/).
2. Register one model and watch its decisions for a day:
   [scale on saturation](well-lit-paths/scale-on-saturation/).
3. Add a GPU limiter, then the warm pool if traffic is bursty and models are
   slow to start. Treat the experimental paths as pilots.

## Backing out

Remove its ScaledObjects and KEDA returns each workload to the replica count it
had before autoscaling, or point their triggers back at KEDA's own scalers;
then uninstall ([upgrade and uninstall](reference/upgrade-and-uninstall.md)).
Kubernetes objects, make targets and metrics keep their `wva-` / `wva_` names
from upstream, which is the only naming to change in dashboards.

## Project status and support

An independently developed, advanced fork of
[llm-d/llm-d-workload-variant-autoscaler](https://github.com/llm-d/llm-d-workload-variant-autoscaler),
not affiliated with or endorsed by the llm-d project. Apache 2.0. Questions and
bugs: [GitHub issues](https://github.com/ev-shindin/llm-scaling-manager/issues).
