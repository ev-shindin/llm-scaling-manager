# Why llm-scaling-manager

A one-page case for evaluators: what it solves, what it was measured to do,
what it costs, how mature each part is, and how to try it and back out.

## The problem

The signals a general-purpose autoscaler reads do not say how much capacity an
LLM needs. Two requests to one model can differ by orders of magnitude in work.
llm-d's default trigger, a running-request threshold, is a per-replica constant
that holds for one request shape.

In one benchmark (Qwen3-0.6B at a constant 6 req/s) the request rate never
changed and no queue formed. KV-cache use read 5–15 %, which a KV-occupancy
trigger (as in llm-d's token-aware recipe) would size as one replica; that is
computed, not a measured arm. The first request shape needed two replicas, the
second needed three.

Scale late, and time to first token spikes for the minutes a new replica spends
loading. Scale early, or keep a floor, and GPUs sit idle. With several models
on one GPU budget, whichever scales first takes the GPUs.

## What it does

llm-scaling-manager computes each model's replica count from a capacity model
(KV cache, queues and token throughput, per replica). It decides for every
model and variant together, inside a GPU budget when you declare one (opt-in;
off by default). KEDA and the HPA carry the decision out. Around that core:

- a **warm pool** of sleeping models that covers the minutes a new replica takes
  to load;
- **scale to zero** through KEDA's own mechanism, with activation pushed to
  KEDA within about 100 ms of a request queuing;
- **GPU budgets**: the GPUs physically free, per-namespace or cluster quotas,
  or quotas read from Kueue (experimental), and an experimental optimizer that
  shares one quota between models;
- **prefill and decode** scaled as separate roles (experimental).

[How it fits together](concepts/architecture.md) has the diagrams.

## Who it is for

Teams that run [llm-d](https://llm-d.ai) on Kubernetes or OpenShift, with
several models or several ways of serving one, a GPU budget that binds, and
KEDA on the cluster. One model on one GPU type, or a latency-SLO target, is
better served by llm-d's own recipes ([below](#against-the-alternatives)).

## What operators need, and what it gives them

| Need | What llm-scaling-manager does |
| --- | --- |
| **New replicas that serve fast when traffic rises.** | A [warm pool](well-lit-paths/warm-pool-bridge/) serves while a replica loads: p95 TTFT per scale-up of 0.11–0.83 s instead of 5.2–8.8 s, within 50 ms of a two-replica floor on 3 of 4 rises (831 ms against 127 ms on the fourth). `make prepull`, `make weights` and `make engine-cache` shorten the [start path](reference/workload-preparation.md). |
| **Scaling before a queue forms.** | Demand is priced from the throughput one replica sustains, so a new request shape can order capacity before a queue exists. On the experimental P/D path (conditions [below](#what-it-was-measured-to-do)) the third replica came about four minutes after the shape change, once that shape's throughput was measured, and no queue formed. That was the second exposure; a controller that has never seen a shape saturated sizes it from occupancy first ([known gaps](#known-gaps)). |
| **Load measured as work.** One request can cost orders of magnitude more than another. | Sized from KV-cache use, queues and token throughput per replica, and from request rates priced by what one replica completes at that request shape, not by a fixed per-replica request threshold. On the P/D run it held 2 replicas at 5–15 % KV-cache use because the load needed 2 in throughput. |
| **Idle models off the GPUs, back when needed.** | [Scale to zero](well-lit-paths/scale-to-zero/) is KEDA's own mechanism; the controller polls the router queue every 100 ms and pushes activation without waiting for KEDA's poll. A wake from zero is a cold start (about 41 s on an H100 test cluster); a model resident in a [warm-pool](well-lit-paths/warm-pool-bridge/) Pod switches in about 437 ms. |
| **Prefill and decode sized apart.** | [P/D roles](well-lit-paths/pd-disaggregation/) (experimental) are sized from their own demand and committed together. The joint commit is unit-tested only; the P/D benchmark never ordered a second prefill replica. |
| **Many models on one GPU fleet.** | Every model and variant is decided together, inside a GPU budget when you declare one. Two opt-in ways move GPUs between models: priority-weighted rescale (`enableRescale`, off by default, acts only under a budget, unit-tested only) and the experimental [utilization-share optimizer](well-lit-paths/utilization-share/), which holds headroom in advance and slows the second model to rise (≈5.5 min in the first run). |
| **Mixed GPU generations.** | Each [variant](well-lit-paths/accelerator-variants/) carries its own per-replica capacity and cost; the cheapest per unit of capacity grows. Unit specs assert the cost order; no end-to-end spec asserts which variant grows, and the benchmark used one GPU type ([#142](https://github.com/ev-shindin/llm-scaling-manager/issues/142)). |
| **Per-team GPU budgets.** | [Quotas per namespace or cluster](well-lit-paths/tenant-gpu-quotas/), [from Kueue](well-lit-paths/kueue-bounded-quotas/) (experimental), or [the GPUs physically free](well-lit-paths/bound-by-gpus/). Budgets are advisory; the hard bound is a namespace GPU `ResourceQuota` ([bound at admission](guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)). A capped decision raises `wva_decisions_limited_total` and a `ResourceConstrained` Event; a fully spent quota can record neither ([monitoring](reference/monitoring.md)). On this release, Kueue grants are honoured only where tenants cannot create LocalQueues on queues not meant for them. |
| **Different urgency for interactive and batch work.** | [Workload classes](well-lit-paths/workload-classes/): named threshold tiers, set per ScaledObject. |
| **Decisions you can explain.** | A closed-form capacity model, two log lines per model per cycle that say why ([cycle log](reference/cycle-log.md)), Events and metrics. It decides; KEDA acts. |

**Not covered yet:** forecasting load, scaling to an SLA target
([#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141)),
pre-scaling before planned maintenance, and telling a failed node from a
replica that is still starting.

## What it was measured to do

All on H200 nodes unless noted. One run per arm unless it says otherwise; the
linked page has the method and every caveat.

| Result | Number | Conditions | Source |
| --- | --- | --- | --- |
| The warm pool covers scale-ups | p95 TTFT per rise: 5.2–8.8 s without it, 0.11–0.83 s with a one-Pod pool; within 50 ms of a 2-replica floor on 3 of 4 rises, 831 ms against 127 ms on the fourth | two 8B models bursting out of phase, 1 GPU per replica, 4 rises per arm | [warm pool, measured](well-lit-paths/warm-pool-bridge/measured.md) |
| ... and costs less than a floor | 14 138 GPU-seconds against 16 080 for the floor (12 % less); 17 % more than no pool (12 129) | same run | [warm pool, measured](well-lit-paths/warm-pool-bridge/measured.md) |
| It sizes for a request shape | first phase: 2 decode replicas held at 5–15 % KV (a KV threshold would read 1: computed, not measured; llm-d's default running-request trigger would likely have ordered more, unmeasured); a 3rd about four minutes after the shape change (KV peak 62 %); no queue. Second exposure: p95 TTFT 155 ms, 0 errors in 13 288 requests. Cold controller: p95 204 ms, p99 3.9 s; its first five-minute window ranged 0.25–5.6 s across the two runs | Qwen3-0.6B, constant 6 req/s, experimental P/D path; tuned HPA (180 s scale-down window, 50 % per 120 s); build `f5f98761`, 2026-09-23; **run twice, a day apart**, within a tenth except that cold window | [P/D path, measured](well-lit-paths/pd-disaggregation/measured.md) |
| A warm Pod switches models fast | about 437 ms (435, 439, 440, 435 ms) against about 41 s cold. This is the engine switch itself; in the warm-pool benchmark the lend waits for the scale-up decision and was first seen 33–143 s after each traffic rise | H100 test cluster, two models resident in one pool Pod | [fast model loading](proposals/fast-model-loading.md#27-the-pool-was-built-and-measured-2026-08-21); [warm pool, measured](well-lit-paths/warm-pool-bridge/measured.md) |
| Sharing one quota serves the first burst at once | p95 TTFT of the first model to burst: 95 ms with sharing, 42 s without. The other burst was overloaded in every arm (177–257 of 960 requests shed), so its p95 (59 s against 73 s) is a floor, censored by the router's 30 s queue TTL. An earlier aggregated run lost the second burst (p95 313 s against 139 s) | two P/D 8B models, four roles under one 10-GPU quota; 36 % more GPU-seconds | [utilization share, measured](well-lit-paths/utilization-share/measured.md) |

![Warm pool: p95 time to first token per scale-up against GPU-seconds for autoscaling alone, a one-Pod warm pool, and a floor of 2 replicas](figures/warm-pool-latency-vs-cost.svg)

**Not yet measured:** the default scaling head to head against plain KEDA or
HPA thresholds on this engine, and repeated warm-pool runs with confidence
intervals.

## What it costs

- **The controller** is one Deployment that reads Prometheus. It holds no GPUs.
- **The warm pool** holds its GPUs continuously: 17 % more GPU-seconds than no
  pool in the run above, 12 % fewer than a replica floor.
- **Sharing a quota** spends idle quota as headroom: 24–81 % more GPU-seconds
  in the measured runs (the 81 % approximate; one run's cost was not measured).
- **Adopting it** takes a one-time cluster-admin setup per namespace, the EPP's
  `flowControl` gate, Prometheus scraping the model servers, and per workload a
  ScaledObject (generated for you), a drain hook and fast starts.
- **A restart is a short scaler outage.** While no replica holds the leader
  lease, KEDA's reads fail; the KEDA `fallback` on generated ScaledObjects
  holds the count then. After that, until its first decision for a model, the
  scaler answers the current replica count, so the HPA holds
  ([before the first decision](reference/scaledobject-trigger.md#before-the-first-decision-and-the-fallback)).
- **What it learned is held in memory** by default, so a restart or leader
  change learns it again. `LEARNED_STATE_SNAPSHOT=true` carries it across
  ([learned state across a restart](reference/configuration.md#learned-state-across-a-restart);
  [#227](https://github.com/ev-shindin/llm-scaling-manager/issues/227) decides
  whether it becomes the default).
- **One more port.** KEDA calls the scaler on `:9090`. A new install serves
  it over mutual TLS when it can, and says why when it stays on plaintext; an
  upgrade with existing ScaledObjects stays on plaintext until you move them
  ([external scaler mutual TLS](reference/configuration.md#external-scaler-mutual-tls)).
  `make setup-prereqs` also writes a NetworkPolicy admitting only KEDA's
  namespace when it knows that namespace, and warns when it does not
  ([network exposure](reference/security-model.md#network-exposure)).

## How mature each part is

| Scenario | Status | Evidence |
| --- | --- | --- |
| [Scale on saturation](well-lit-paths/scale-on-saturation/) | Stable | end-to-end suites; one July 2026 study of five scenarios, one run each, against a KEDA threshold, on an older controller |
| [Scale to zero](well-lit-paths/scale-to-zero/) | Stable | end-to-end suites, cluster-verified |
| [Warm pool](well-lit-paths/warm-pool-bridge/) | Stable | end-to-end suites; one benchmark run per arm |
| [Bound by GPUs](well-lit-paths/bound-by-gpus/) | Stable | end-to-end suites |
| [Tenant quotas](well-lit-paths/tenant-gpu-quotas/) | Stable (control path); no end-to-end spec bounds the default optimizer | unit tests; end-to-end: the warm pool's refusal path (`warm_pool_quota_test.go`), a Kueue-bounded quota entry (`kueue_quota_test.go`), the utilization-share optimizer under a spent quota (`utilization_share_test.go`) |
| [Workload classes](well-lit-paths/workload-classes/) | Stable | end-to-end suite (simulator only) |
| [Accelerator variants](well-lit-paths/accelerator-variants/) | Stable (control path); variant choice asserted by unit specs only | a benchmark scenario and one written-up run on one GPU type; `limiter_test.go` runs two variants end to end but does not assert which grows |
| [P/D disaggregation](well-lit-paths/pd-disaggregation/) | Experimental | benchmark run twice |
| [Kueue quotas](well-lit-paths/kueue-bounded-quotas/) | Experimental; borrowing on by default | unit and API-server tests; the live e2e runs in CI job `e2e-tests-kueue-borrow` |
| [Retained pool](well-lit-paths/retained-pool/) | Experimental | unit tests, a benchmark scenario, one end-to-end spec (simulator only) |
| [Sharing a quota](well-lit-paths/utilization-share/) | Experimental | unit and end-to-end tests; single benchmark runs |

Each path's *How it is tested* section names its suites and what they do not
cover.

## Against the alternatives

- **llm-d's KEDA recipes** (running requests, queue length, KV use): what llm-d
  now recommends. Each threshold is per workload and has to be re-tuned per
  model, accelerator and parallelism (llm-d's own caveat), and again when the
  request shape changes. A GPU budget sits below it, in Kueue, where demand over
  budget waits as Pending Pods; this project sizes inside the budget instead.
  llm-d's [FMA + KEDA](comparison/keda-alone.md#fast-model-actuation-what-we-kept-and-what-we-replaced)
  attacks the cold start with sleeping engines, and its SLO-aware recipe scales
  one variant to a latency target, which this project does not yet do.
  [KEDA alone](comparison/keda-alone.md) compares them need by need; the only
  head-to-head is one July 2026 study of five scenarios, one run each, on an
  older controller and recipe.
- **llm-d's workload-variant-autoscaler**: the project this one forked from,
  deprecated upstream (`v0.9.0` is final; llm-d advises staying on it where
  cost-aware placement across variants is needed). Upstream already had GPU
  budgets and scale-from-zero. This fork adds the KEDA external scaler with
  pushed activation, Kueue quotas, the utilization-share optimizer, the warm
  pool, the install tooling, and throughput-priced demand.
- **NVIDIA Dynamo Planner**: SLA-driven: it forecasts load and profiles before
  deployment, for one model on one GPU type. SLA-target scaling is on this
  project's roadmap ([#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141)).
  [The comparison](comparison/) has the detail.

## Known gaps

No load forecasting; no scaling to an SLA target, only threshold tiers
([#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141)). A model
has to saturate once before its capacity is priced for a new request shape: the
P/D result above is the second exposure, and the cold pass before it ordered a
spare replica and reached a 3.9 s p99. The warm pool and P/D numbers are single
runs per arm on one cluster type (the P/D pair repeated once).

- While no replica holds the leader lease, as during a restart, KEDA cannot
  read the scaler. Only a KEDA `fallback` holds the count then, and a
  ScaledObject with a floor of 0 gets none
  ([before the first decision](reference/scaledobject-trigger.md#before-the-first-decision-and-the-fallback)).
- Learned capacity is re-learned after a restart unless the opt-in
  `LEARNED_STATE_SNAPSHOT` is on, and even then a held ITL line is not
  restored ([#316](https://github.com/ev-shindin/llm-scaling-manager/issues/316);
  the default is [#227](https://github.com/ev-shindin/llm-scaling-manager/issues/227)).
- The scaler port stays on plaintext where the installer cannot turn mutual
  TLS on, for example when KEDA is absent or not found, when the cluster
  admin has not yet written KEDA's side, or when existing ScaledObjects have
  not yet been moved; the installer says which
  ([every case](reference/configuration.md#external-scaler-mutual-tls)).
- The Kueue quota source counts cohort borrowing only on a flat cohort
  without Fair Sharing or `effectiveQuotas`, and, unless the experimental
  [hand-back](reference/quota-limiter.md#lenders-kueue-does-not-reclaim-for)
  is on, only from lenders Kueue would reclaim for; elsewhere it counts none
  and caps at nominal
  ([cohort borrowing](reference/quota-limiter.md#cohort-borrowing);
  [#214](https://github.com/ev-shindin/llm-scaling-manager/issues/214)).
- On KEDA 2.19 and older, a ScaledObject created soon after the controller
  restarts can stay unwired: its HPA keeps only the CPU default and never
  scales. It is a KEDA bug fixed in 2.20; upgrade KEDA to 2.21, or on older
  KEDA run `make scaledobjects-rewire`
  ([KEDA](reference/keda.md#unwired-hpa-before-keda-220);
  [#315](https://github.com/ev-shindin/llm-scaling-manager/issues/315)).

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
