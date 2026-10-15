# llm-scaling-manager and KEDA alone

llm-d says its KEDA recipes do not cover everything WVA did. Its architecture
page says KEDA + EPP *"does not replace WVA feature for feature. Cost-aware
placement across variants has no current equivalent; the rest is covered,
though in some cases by a different layer than the autoscaler"*: a shared
accelerator budget by Kueue, pending-Pod awareness by kube-state-metrics. It
advises *"If a deployment depends on cost-aware placement across variants or on
fair sharing of a scarce accelerator pool, stay on WVA for now"*
([architecture](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/docs/architecture/autoscaling/README.md#what-changed-with-the-deprecation)).
At the same time, *"The WVA path is no longer developed and receives no further
releases; `v0.9.0` is the final version"*, and existing WVA deployments are told
to migrate to KEDA + EPP
([llm-d](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/workload-autoscaling/README.md)).
llm-scaling-manager is one way to stay on that path: an independently
developed, advanced fork of WVA, still maintained, not affiliated with or
endorsed by llm-d, and not upstream WVA (its actuation is a KEDA external
scaler).

**KEDA scales your pods in both setups.** This page covers the one thing that
differs: what computes the replica count KEDA applies.

- **KEDA alone** turns metrics into replicas with thresholds. With a Prometheus
  trigger at `metricType: AverageValue`, the count is the metric's sum divided
  by the threshold, rounded up. When a ScaledObject has several triggers, the
  HPA takes the largest count.
- **With llm-scaling-manager**, KEDA's trigger is an
  [external scaler](../reference/scaledobject-trigger.md) that answers with a
  replica count. The count comes from a capacity model of each replica. The
  scaler decides for every model and variant it manages together, and inside a
  GPU budget when you configure one. KEDA still owns activation, the HPA and its
  behaviour windows, fallback and pausing.

So the choice is not which tool to install, because KEDA is required either way.
The question is whether one threshold per workload is enough for your fleet.

## What "KEDA alone" means here

This page follows llm-d's own guidance at llm-d commit
[`7d5f86d`](https://github.com/llm-d/llm-d/tree/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/workload-autoscaling)
(2026-10-14). llm-d now recommends KEDA with the router's (EPP) metrics. Its
recommended default is the
[queue recipe](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/workload-autoscaling/keda-epp/optimized-baseline/overlays/k8s/queue/scaledobject.yaml):

| Setting | Value |
| --- | --- |
| Triggers | `sum(llm_d_epp_flow_control_queue_size)` at 1 per replica, and `sum(llm_d_epp_request_running)` at 16 per replica |
| Windows | 300 s stabilization on scale-up **and** scale-down; polling 15 s; cooldown 300 s (scale to zero only) |
| Bounds | min 1 (0 optional), max 8 |

Beyond the default, llm-d publishes these recipes:

- A router
  [pool-saturation overlay](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/workload-autoscaling/keda-epp/optimized-baseline/overlays/k8s/saturation/scaledobject.yaml)
  (experimental). It is upstream's migration target for WVA users, *"the
  closest replacement for WVA's saturation signal"*. llm-d credits it with
  *"more portable thresholds"*, and notes that it *"can potentially
  over-provision"* and *"has no nightly end-to-end coverage yet"*.
- [Token-aware sizing](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/workload-autoscaling/keda-epp-token-aware/README.md)
  (experimental). It divides in-flight prompt tokens by a hand-calibrated
  prefill throughput `V_P` and sums KV use, with per-role ScaledObjects for
  prefill and decode.
- An [SLO-aware recipe](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/workload-autoscaling/slo-aware/README.md),
  a control law on p90 TTFT and TPOT against the SLO.
- [Kueue rebalancing](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/workload-autoscaling/kueue-rebalancing/README.md)
  (experimental), which puts a GPU budget below the autoscaler.
- [FMA + KEDA](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/fast-model-actuation/fast-model-actuation-keda/README.md),
  which points the queue trigger at llm-d's fast model actuation to cut the cold
  start.

llm-d's renamed WVA repository,
[llm-d-autoscaling](https://github.com/llm-d/llm-d-autoscaling/blob/e036371f7d04317920ef03a596fbe0c64745942a/README.md),
now holds KEDA manifest blueprints. They include a recommended
[P/D blueprint](https://github.com/llm-d/llm-d-autoscaling/blob/e036371f7d04317920ef03a596fbe0c64745942a/benchmark/config/scenarios/guides/pd-disaggregation.yaml)
on vLLM's own metrics. The table below names each recipe where it answers a need
the default does not.

llm-d states the default's limits itself: *"No cross-variant placement"*, *"No
cost-aware choice of hardware"*, *"Warmup is paid at every scale-up"*. It also
says its thresholds *"are not universal … Do not assume a value validated for one
model, accelerator, or tensor-parallel degree transfers to another"*
([KEDA + EPP architecture](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/docs/architecture/autoscaling/keda-epp.md#limitations)).

## What each can do

Evidence levels follow [why.md](../why.md#how-mature-each-part-is).

| Need | llm-d's KEDA recipes | With llm-scaling-manager | Evidence for our column |
| --- | --- | --- | --- |
| **What sets the count** | Default: 16 running requests and 1 queued request per replica, per ScaledObject. llm-d says to tune these per model, accelerator and parallelism. Token-aware: one hand-calibrated prefill throughput per hardware and model. | A capacity model per replica, built from KV-cache use, queues and the token throughput one replica sustains, learned while it serves. Thresholds are tiers that set how full a replica may run, not per-model numbers. | [Scale on saturation](../well-lit-paths/scale-on-saturation/): end-to-end suites on kind with a simulator, which exercise the control path. The capacity model itself is measured only on the [P/D run](../well-lit-paths/pd-disaggregation/measured.md) |
| **Requests of very different cost** | The running-request trigger counts a 100-token request and an 8 000-token request alike. Token-aware prices prefill as in-flight prompt tokens over a hand-calibrated `V_P`. It prices decode by summed KV use, which follows occupancy and lags a shape change. | Demand is priced from measured throughput, so a change in request shape can order capacity before a queue forms. | Experimental P/D path, [run twice](../well-lit-paths/pd-disaggregation/measured.md): Qwen3-0.6B at 6 req/s, a tuned HPA (180 s scale-down window). It held 2 replicas at 5–15 % KV, where occupancy alone would have said 1 (computed, not a measured arm), and ordered a 3rd about four minutes after the shape change, once that shape's throughput was measured; no queue formed in the second phase. That was the second exposure: a cold controller sizes from occupancy first |
| **Reacting to a rise** | The default's 300 s scale-up window absorbs startup overshoot. llm-d calls it *"blunt"* because it delays every scale-up equally. The token-aware recipes use a 0 s window and add at most 1 pod per 180 s. The SLO-aware recipe uses a 30 s window, and its control law credits replicas that are still starting (ready over total). | Scale-up acts at once (0 s window). The capacity model counts replicas that are still starting, so it does not re-order them while they load. | End-to-end suites (simulator). Not benchmarked against llm-d's current recipes |
| **The cold start of a new replica** | The default pays it at every scale-up. In llm-d's fast-model-actuation benchmark, its plain KEDA + EPP baseline measured a cold p95 TTFT of 52–74 s. [FMA + KEDA](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/fast-model-actuation/fast-model-actuation-keda/README.md) cuts it: its hot arm measured p95 TTFT 1.5 s on Qwen3-14B and 13.4 s or 24.1 s (saturation or queue trigger) on Qwen3-32B, with 0 failures, in a pass where all six replicas were loaded at standup and put to sleep, so every scale-up woke an engine already on its GPU. On Qwen3-14B the warm arm (one idle requester, a new engine on a running launcher) matched it at 1.56 s with 0 failures, though its pod startup was 71.0 s against 3.3 s. Conditions: 3-run averages on H100, one GPU per replica; default thresholds with max 6 replicas, 0 s and 1 Pod per 30 s on scale-up, 300 s on scale-down; p95 over the whole run ([results](https://github.com/llm-d/llm-d/tree/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/fast-model-actuation/fast-model-actuation-keda/benchmark-results)). | Optional [warm pool](../well-lit-paths/warm-pool-bridge/): sleeping models lend a replica while the variant's own replica starts. | p95 TTFT per scale-up was 0.11–0.83 s with a one-Pod pool, against 5.2–8.8 s without it, on two 8B models. Conditions: H200, p95 per rise window, one run per arm. The models, load and percentile window differ from llm-d's, so this is not a head-to-head. [What we kept of FMA](#fast-model-actuation-what-we-kept-and-what-we-replaced) |
| **Several variants of one model** (GPU types, TP sizes) | One ScaledObject per Deployment. llm-d: *"No cross-variant placement"*. | Variants are decided together. Scale-up grows the one with the lowest cost per unit of capacity, and scale-down sheds the highest `variantCost` first. | [Accelerator variants](../well-lit-paths/accelerator-variants/): one benchmark run in July 2026, on an earlier build, with one GPU type and two TP sizes. Unit specs assert the cost order (`internal/engines/allocation/cost_aware_optimizer_test.go`); no end-to-end spec does. `limiter_test.go` runs two variants end to end but asserts discovery and the GPU bound |
| **Prefill and decode** The [token-aware P/D recipe](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/workload-autoscaling/keda-epp-token-aware/README.md) uses one ScaledObject per role: *"prefill and decode scale independently"*. The llm-d-autoscaling [P/D blueprint](https://github.com/llm-d/llm-d-autoscaling/blob/e036371f7d04317920ef03a596fbe0c64745942a/benchmark/config/scenarios/guides/pd-disaggregation.yaml) is also one ScaledObject per role. Neither couples the two. | Each role is sized from its own demand. The pair is committed together, so one role cannot outrun the other. | [P/D disaggregation](../well-lit-paths/pd-disaggregation/) (experimental): the benchmark, run twice, never scaled prefill (1 replica throughout). The joint commit has unit tests only (`internal/engines/allocation`) |
| **A GPU budget** | Below the autoscaler: Kueue cohorts with per-model floors, borrowing and preemption (experimental). Demand over budget waits as Pending Pods. | Optional `limiters:`: GPUs free, a quota per namespace, or the quotas Kueue holds. Each model is fitted inside the budget before KEDA sees a number. A capped decision raises `wva_decisions_limited_total` and a `ResourceConstrained` Event, though a fully spent quota can record neither. The limiter is advisory, and a `ResourceQuota` stays the hard bound. **With Kueue under both setups, enforcement is the same** (Kueue admits, holds or preempts); the difference is that this side reads Kueue's quota and sizes every model inside it, so it does not ask for Pods Kueue would leave Pending. It counts cohort borrowing where it is safe (on by default); Kueue takes borrowed replicas back for a lender that can reclaim, and the scaling manager hands them back to one that cannot only when told to (`releaseToNonReclaimingLenders`). On this release, Kueue grants are honoured only where tenants cannot create LocalQueues on queues not meant for them. | [Bound by GPUs](../well-lit-paths/bound-by-gpus/): end-to-end. [Tenant quotas](../well-lit-paths/tenant-gpu-quotas/): unit tests, with the refusal path end-to-end. [Kueue](../well-lit-paths/kueue-bounded-quotas/): experimental |
| **Many models on one fleet** | Each ScaledObject scales alone, and Kueue decides who is admitted. | An opt-in priority-weighted rescale under a budget (unit tests only). The experimental [utilization-share optimizer](../well-lit-paths/utilization-share/) moves GPUs between models as demand shifts. | Utilization share: single benchmark runs per arm. Sharing cost 24–81 % more GPU-seconds than the default optimizer in those runs; the 81 % is approximate and came from a build with a since-fixed bug, and one rerun's cost was not measured ([measured](../well-lit-paths/utilization-share/measured.md)) |
| **A latency SLO** | The SLO-aware recipe scales on p90 TTFT and TPOT against the SLO, for *"one variant, one SLO pair"*. llm-d measured 95.8 % attainment at 5.93 average replicas, where a static pool of 8 attains 100 %. Conditions: GKE, H100, Qwen3-32B at TP=2, TTFT ≤ 3 s and TPOT ≤ 100 ms, a staged 2→10→1 rps ramp, bounds 3–8, with a calibrated latency predictor (85.6 % with a cold one). | No SLO target yet ([#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141)). Replicas are sized from capacity, and tiers set how full they may run. | Not applicable |
| **Interactive and batch urgency** | A different threshold per ScaledObject. | Named tiers in one policy ([workload classes](../well-lit-paths/workload-classes/)). | End-to-end suite (simulator) |
| **Scale to zero** | KEDA's own `minReplicaCount: 0`. The router buffers requests, and its queue metric wakes the target. llm-d's caveat: a request queued at zero is lost if the router restarts. | KEDA's own mechanism. The controller also reads the router's queue every 100 ms and pushes activation, instead of waiting for the next poll. Only the 0 → 1 step is pushed. The request waits in the same router queue, so a router restart loses it here too. The wake is still a cold start, about 41 s on an H100 test cluster, unless a warm pool holds the model. | [Scale to zero](../well-lit-paths/scale-to-zero/): end-to-end, cluster-verified |
| **Flapping** | HPA behaviour windows (300 s both ways in the default) and the HPA's 10 % tolerance. | The same HPA behaviour (our generated ScaledObject uses 0 s up and 300 s down). The controller adds holds of its own: sticky scale-down and shape-change holds. | End-to-end suites |
| **Explaining a decision** | Each trigger's metric value, and the HPA's current and desired counts. | Two log lines per model per cycle that say why ([cycle log](../reference/cycle-log.md)), Events, `wva_model_scaling_blocked` reasons and dashboards. | [Monitoring](../reference/monitoring.md) |
| **When the signal source fails** | KEDA `fallback`, if configured. Prometheus is the only dependency. | One more controller in the path. Our generated ScaledObjects set no `fallback` (the template sample sets `currentReplicasIfHigher`). If the controller is unreachable when KEDA first builds the HPA, KEDA falls back to a CPU metric. | [After the install: first-line troubleshooting](../reference/operations.md#first-line-troubleshooting) (`cpu: <unknown>/80%`) |

## Fast model actuation: what we kept and what we replaced

llm-d's answer to the cold start is [FMA](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/fast-model-actuation/README.md):
a requester Pod reserves the GPU and binds to a launcher that keeps vLLM resident;
scaling down puts the engine to sleep and releases the GPU to Kubernetes. A hot
start (waking a sleeping engine) and a warm start (a new engine on an existing
launcher) are different paths: llm-d's benchmark measured 4.0 s against 85.3 s
mean pod startup on Qwen3-32B, in two separate passes; on Qwen3-14B the warm
arm matched the hot one on p95 TTFT (1.56 s against 1.50 s, 0 failures), though
pod startup was 71.0 s against 3.3 s. Which one a scale-up gets
is a matter of placement. The FMA guide says a hot start *"only occurs if the
Kubernetes Pod scheduler assigns the new requesting pod to the same node (and
GPU) where the sleeping vLLM instance resides"*
([FMA guide](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/fast-model-actuation/README.md#actuation-paths)),
and the FMA + KEDA guide says *"Which path you get depends on GPU assignment"*
([FMA + KEDA](https://github.com/llm-d/llm-d/blob/7d5f86d1de99c9df39f58eba0c8a7e20c00cfaa4/guides/fast-model-actuation/fast-model-actuation-keda/README.md)).

We kept FMA's launcher and replaced its requester/launcher binding. Every
warm-pool Pod runs the launcher, which spawns and supervises the vLLM engines,
and the model switch measured on the pool runs on it. The launcher has no sleep
or wake endpoint: the scaling manager drives vLLM's own sleep mode (`/sleep`,
`/wake_up`) directly, the role FMA's controller plays
([the split of responsibilities](../../warmpool/README.md#the-split-of-responsibilities)).
Single-Pod pools run upstream launcher v0.6.4 unchanged
([`warmpool-deployment.yaml`](../../config/warmpool/warmpool-deployment.yaml));
group pools need `--launcher-image ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless`,
our fork, which adds a port-conflict reclaim and routes LeaderWorkerSet
follower ranks to the headless executor, so engines that span machines can be
pooled too
([what is copied, and from where](../../warmpool/README.md#what-is-copied-and-from-where)).
Group pools have unit, manifest and end-to-end specs, and no benchmark yet.

The binding is what we dropped. We ran FMA's controller at v0.6.0-alpha.13 as
this project's scale-up path on an H100 test cluster on 2026-08-18/19
([post-mortem](../proposals/fma-post-mortem.md)):

- **Placement decided the path.** A wake is node-local; a requester that landed
  away from its sleeper paid a cold load. With placement pinned (3 of 3 correct)
  we still saw 0 wakes and 3 rebuilds.
- **2 of 3 unbound sleepers could not wake** (CUDA `invalid argument` and out of
  memory), because the GPU they slept on had been released and taken.
- **A GPU quota did not bound it**: a `LauncherConfig` does not express the pool.

llm-d's guide now pins FMA 0.6.5, a stable release published 2026-08-31, after
our test. We have not re-tested the binding on it, and our single-Pod pin is
still 0.6.4.

What replaced the binding is the [warm pool](../well-lit-paths/warm-pool-bridge/):
each pool Pod owns its GPU for its whole life, so there is no bind to place
wrongly and no released GPU to lose. The trade-off is the honest one: FMA's
sleeping engine gives its GPU back to the cluster while idle and may not get it
back; the pool keeps the GPU allocated, costs it while idle (17 % more
GPU-seconds than no pool, 12 % fewer than a floor of two replicas, in our run),
and its switch took 435–440 ms in the four switches measured.

## Where KEDA alone is enough

KEDA alone is one component fewer, and llm-d maintains its recipes upstream.
Its default queue recipe is, in llm-d's words, the *"Mature, nightly-covered
path"*. It is the better choice in these cases:

- **One model, one GPU type, one request shape.** You can tune a threshold for
  it and re-tune when traffic changes.
- **No shared GPU budget.** Each model has GPUs reserved for its maximum, or
  Kueue cohorts are enough.
- **You scale to a latency SLO** on a single variant. llm-d's SLO-aware recipe
  targets it directly; we do not yet ([#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141)).
- **Latency first, cost second.** A low threshold over-provisions, which is the
  safe direction when the tail is the SLO.

## What it costs to add llm-scaling-manager

- **One more controller in the scaling path.** KEDA asks it for every count. It
  runs leader-elected and holds no GPU, but its availability now matters. Set a
  `fallback` on your ScaledObjects.
- **It holds until it has decided.** After every controller restart and
  leader change, `GetMetrics` answers a model's current replica count until the
  controller's first decision for it, so the HPA holds
  ([before the first decision](../reference/scaledobject-trigger.md#before-the-first-decision-and-the-fallback)).
  While no replica holds the leader lease, the call errors instead. KEDA's
  `fallback` covers that case if it is set: the installer writes one
  (`currentReplicasIfHigher`) on generated ScaledObjects with a floor of 1 or
  more when KEDA is 2.17 or later, and none on a floor of 0.
- **Engine and router metrics.** Prometheus must scrape vLLM or SGLang. The
  router's (EPP) metrics supply the queues and the wake from zero.
- **It learns, and it forgets on restart.** A controller that has never seen a
  model or a request shape saturated sizes it from occupancy first. The
  throughput model applies from the second exposure
  ([known gaps](../why.md#known-gaps)). What it learned is held in memory by
  default, so a restart or leader change learns it again; the opt-in
  `LEARNED_STATE_SNAPSHOT` carries it across
  ([#137](https://github.com/ev-shindin/llm-scaling-manager/issues/137)).
- **One more port.** KEDA reaches the controller over gRPC, plaintext and
  unauthenticated by default. The admin phase (`make setup-prereqs`) writes a
  NetworkPolicy admitting the port only from KEDA's namespace when it knows
  that namespace, and warns that the port is open when it does not; it holds
  only where the network plugin enforces NetworkPolicy. Mutual TLS is opt-in
  and wired by hand
  ([network exposure](../reference/security-model.md#network-exposure));
  [#138](https://github.com/ev-shindin/llm-scaling-manager/issues/138) makes
  it the default.

## Measured against KEDA alone

One study in July 2026, of five scenarios with one run each, compared the two
on single-variant workloads, with KEDA actuating both arms: the
[scaling study](../../comparison/wva-vs-keda-scaling-study.md).

- **Runs 1 and 2 below saturation; Run 4 sustained decode-heavy at 24 rps, the
  KEDA arm pinned at its cap of 10.** The threshold arm held 1.9–2.7× the GPUs
  (1.88, 2.46 and 2.70 times ours).
- **Drops were not equal.** In Runs 1 and 2 the threshold arm dropped 30 and 21
  requests, against our 0. Those drops came under the 120 s scale-down window
  we gave the KEDA arm; llm-d's recipe ships 300 s. In Run 4 we dropped 6,
  against its 2.
- **Run 3, saturated.** On sustained prefill-heavy traffic the threshold arm
  held 4.01 GPUs against our 1.56, and it had the better tail: 14 dropped
  against our 101. Our leg there was the cheapest of six scale-down settings
  tried, against one KEDA configuration. Our most reliable of the six legs
  still had 78 errors against KEDA's 14.

**Read it as history.** Both sides have moved since:

- **The recipe.** The threshold arm used 16 running requests and a queue of 1 per
  replica, as llm-d's recipe still does. It used the older EPP metric names,
  max 10 and a 120 s scale-down window, where the recipe shipped max 8 and
  300 s. llm-d has since added a 300 s scale-up window against startup
  overshoot, the very behaviour the study measured.
- **The controller.** The capacity model has changed: backlog priced by
  throughput, the throughput-priced demand floor, shape-change holds and sticky
  scale-down. The study's one loss, Run 3's tail, has not been re-run.

A re-run, Run 3 included, with the threshold arm on llm-d's current default
recipe is planned
([#142](https://github.com/ev-shindin/llm-scaling-manager/issues/142)).
