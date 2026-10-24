# Share one GPU quota between models

When several models share one GPU quota and peak at different times, this
optimizer shares the quota on purpose instead of first come, first served.
Each model gets what it needs, the rest is spread as headroom by weight, and
GPUs move between models as demand shifts. For GPUs that are paid for whether
used or not (on-prem, a reservation, a team allotment).

**Maturity:** **Experimental.** One or two runs per configuration, on 1 GPU per Pod
([Evidence](#evidence)). The defaults are starting points.

## When not to take it

- There is nothing to share: one model per quota gains only headroom.
- You want idle GPUs given back. It holds headroom on purpose
  ([What it costs](#what-it-costs)).
- The model that rises when load swaps cannot wait about 5.5 minutes longer
  than today, or load swings back faster than a move lands (about twelve
  minutes).
- More cases are in [Details](#more-cases-where-it-does-not-fit).

## Start here

You need two or more models scaled by the scaling manager under one limiter,
model servers that drain on termination, and `kubectl`, `jq`, `yq` and `make`
in a checkout of this repository. Every step is in
[Turn on the utilization-share optimizer, safely](../../guides/utilization-share/).
Steps 2 to 4 are run by whoever owns the policy: the namespace owner for an
own-install quota, the cluster admin for a policy in `wva-policy` or `wva-<ns>`.

1. Namespace owner: [install with a quota](../../guides/install-in-namespace/), adding `WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=<n>'`, or stand up the guide's [two-model trial](../../guides/utilization-share/README.md#a-trial-setup).
2. [Check there is a budget to share](../../guides/utilization-share/README.md#prerequisites).
3. [Turn it on in shadow, read what it would move, then activate it](../../guides/utilization-share/README.md#installation-instructions).
4. [Check it works](../../guides/utilization-share/README.md#verification): the mode reads `active` and transfers end `done`.

Models in several namespaces, one budget for all of them? Use [Share GPUs
between models in different namespaces](../../guides/utilization-share-cluster-wide/)
instead: one cluster-wide install, from llm-d already serving to GPUs moving
between namespaces. If the scaling manager may not read every namespace, use
[Share GPUs between namespaces with a namespace-scoped
install](../../guides/utilization-share-namespaced/): it manages only the
llm-d namespaces, with a Role in each, under one quota.

To undo, see [Cleanup](../../guides/utilization-share/README.md#cleanup). The
guide's two-model trial holds GPUs until you tear it down. If the controller
is out of your reach, see [Details](#when-the-controller-is-out-of-your-reach).

## What it costs

- **Headroom held ahead of need.** In the early runs, when the whole spare was
  spread, that was 24% and 66% more GPU-seconds than today's optimizer. On
  the current build, the three P/D runs cost 36%, 39% and 43% more, using up to
  every GPU the quota allowed. The current build's cost without P/D has not been
  measured.
- **A donor replica,** with its in-flight requests. They survive only as well
  as the model server drains.
- **Waiting.** The receiver waits for the donor's scale-down window, its drain,
  then its own start. Turning it on freezes the group for about three minutes.
- More costs, and traffic shapes that still lose something, are in
  [Details](#more-costs).

## Evidence

Nine benchmark runs so far, on H200s, two 8B models at 1 GPU per replica, in
seven configurations: two were repeated, the rest **run once**. Every number,
table and timeline is in [measured](measured.md); all nine runs are summarised
[below](#all-nine-runs).

- **Latest aggregated run (2026-10-12):** the model that burst first was
  served at once, median TTFT (time to first token) 0.07 s against 25.6 s with
  the same fleet planned but not moved. The second was no worse: same p95
  (287 s), median 144 s against 215 s. GPU cost not measured.
- **Both models P/D, latest run (15 000-token prompts):** with sharing,
  output-heavy p95 0.090 s against 51 s, prompt-heavy p95 6.0 s against 96 s
  with no dropped requests; +39% GPU-seconds. A repeat run agreed: 0.093 s
  against 73 s, 3.5 s against 100 s, no failed requests against 123, +43%
  GPU-seconds. Two runs, one rise per model each; shadow alone differed from
  today by 24 s and 32 s on the prompt-heavy model. At
  20 000-token prompts the prompt-heavy burst was overloaded in every arm.
- Not yet measured: whole-node or multi-GPU pods, a cluster-wide quota, and
  the physical GPU limiter.

## Details

### All nine runs

[Two models, anti-phase bursts, under one quota](../../guides/benchmarking/two-model-utilization-share.md)
runs the same traffic three times: no `optimizer:` block (`today`), `shadow`,
and acting (`share`).

| Run | What it showed |
| --- | --- |
| Same-shape bursts, 4-GPU quota | The first model to rise had headroom serving; the model rising at the swap grew about 5.5 minutes late; +24% GPU-seconds. |
| Different shapes, 10-GPU quota (two runs) | The second model to burst ran on 3 of its 8 replicas (median 575 s, then 564 s, against 1 s). Fixed since. |
| With those fixes | First burst gained (12 s against 35 s); second lost (p95 313 s against 139 s): the spare sat on the idle model, draining long outputs. Led to the headroom cap. |
| With the headroom cap | First burst 0.07 s against 41 s; second reached its fleet sooner; +34% GPU-seconds, mostly an idle-fill bug. |
| With the idle-fill fix (2026-10-12) | First burst 0.07 s against 25.6 s; second no worse; GPU cost lost to a sampling gap. |
| Both models P/D, four roles, 10-GPU quota, 20 000-token prompts | Output-heavy burst p95 0.095 s against 42 s; prompt-heavy burst overloaded in every arm (p95 59 s against 73 s); +36% GPU-seconds. Run on vLLM v0.31.0 ([why](measured.md#why-vllm-v0310)). |
| The same, 15 000-token prompts | Output-heavy burst p95 0.090 s against 51 s; prompt-heavy burst 6.0 s against 96 s, with no failed requests against 116; +39% GPU-seconds. A repeat agreed: 0.093 s against 73 s, 3.5 s against 100 s, +43%. |

All three [P/D runs](measured.md#pd-two-disaggregated-models-under-one-quota)
are valid. One more attempt was discarded because other workloads preempted
its engine Pods.

### Why it exists

Today's optimizer hands the quota out first come, first served. A model that
scaled up earlier keeps its GPUs, and one that needs them later is refused,
even when the quota has room for both. The worst case is a swap: one model's
load falls while the other's rises, and the rising model is refused for the
falling model's whole scale-down window.

### More cases where it does not fit

- **There is no limiter.** Without a `limiters:` list the optimizer has no
  budget and stays off (mode `invalid`).
- **A move lands too slowly.** Standing still beats moving when load reverses
  within roughly 100 minutes
  ([proposal §6.7](../../proposals/utilization-share-optimizer.md#67-why-it-does-not-oscillate-and-what-it-cannot-follow)).
- **The donors cannot be steered.** A Deployment often mid-rollout, or with
  pods Pending or not Ready, is held as a donor until it settles
  (`donor-changing`).
- **The real limit is something else.** If a Kubernetes `ResourceQuota` or
  unmanaged workloads stop pods, a donor's freed GPUs can be taken by a pod the
  scaling manager did not place (`release-taken`).
- **A model runs on more than one accelerator type.** It is never planned; it
  stays on the default optimizer.
- **A namespace a namespace quota excludes, with no finite cluster bound.** It
  is never planned and stays on the default optimizer. Add a `scope: cluster`
  quota for its accelerator type, or, on a cluster-scoped controller, set
  `physicalGroups: true` beside a `gpu-inventory` limiter.

### What it needs

- A limiter in the **cluster default** policy entry, with accelerators that
  resolve. Either a quota ([cap what each tenant may take](../tenant-gpu-quotas/),
  or numbers read from [Kueue](../kueue-bounded-quotas/)), or the physical GPU
  inventory from [bound-by-gpus](../bound-by-gpus/) with `physicalGroups: true`
  in the block, on a cluster-scoped controller only. The quota's key must be the accelerator name the controller
  logs (for example `H200` on one cluster, `NVIDIA-H200` on another). Only the
  quota has been measured. Under the physical limiter the budget is every GPU
  of a type on the cluster
  ([what differs](../../guides/utilization-share/README.md#under-the-physical-gpu-limiter)).
- The limiter is advisory. Back each tenant namespace with a GPU
  `ResourceQuota` ([bound at admission](../../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).
- Model servers that drain on termination (`preStop` and
  `terminationGracePeriodSeconds`): a transfer removes a donor pod through an
  ordinary scale-down.
- A distinct `CONTROLLER_INSTANCE` per controller when several share a
  cluster; each cleans up only its own transfer marks.
- Node information for placement. A namespace-scoped install plans without
  the node read, by GPU count alone and never reports `release-taken`,
  `release-shape-mismatch` or `wrong-pod`.

### When the controller is out of your reach

With the controller in `wva-<ns>`, the quota and the `optimizer:` block are
the cluster admin's, in `wva-<ns>`. That controller reads them only at start,
so the admin runs each step with `RESTART=1`. Your
[namespace-local map](../../reference/scaling-policy.md#namespace-local-scaling-policy-map)
sets your models' share weights only.

### How it shares the quota

In the order it is applied:

1. **Each model first gets its need:** the GPUs that put it at its
   `scaleUpThreshold`.
2. **The rest is headroom,** shared in proportion to each model's weight
   (`standard` 1, `important` 2, `critical` 4). While the quota cannot hold
   every model's recent peak at once, headroom is capped at
   `maxHeadroomReplicas` (default 1) times the weight. The rest stays
   unassigned, so a model whose demand rises starts pods into it at once.
3. **A model too far from its share gets GPUs through a transfer:** the
   optimizer marks one donor pod, lowers the donor, waits until the GPUs are
   actually free, and only then raises the receiver. The scale-down always
   comes first.

### Declaring it

One `optimizer:` block in the scaling policy's `default` entry, beside the
limiter. It rolls out in two steps, each one `make` target in the guide:
**shadow** (it computes and reports, and moves nothing), then **active**.

Re-running `make deploy-wva` resets the `default` entry in the controller's
own ConfigMap to the shipped one, without the block; the limiter is kept
unless `WVA_LIMITER` says otherwise. After an upgrade, repeat the check,
shadow and activate steps. Shadow also clears transfer marks the active
optimizer left
([why](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).

Model owners choose a weight on their own policy entry (`weightClass:
important`, or `weight:`). A weight buys a larger share of the spare, not
protection: only a floor (`minReplicaCount`) is never taken
([for model owners](../../reference/scaling-policy.md#for-model-owners-what-the-optimizer-may-take-and-how-to-protect-a-model)).

### Four things that surprise people

**A missing `shadow` key acts.** `shadow` defaults to `false`, so a block
without it moves replicas from the next cycle. Write `shadow: true` until you
have read what it would do.

**Turning it on freezes the group for about three minutes.** When it starts
acting, after a controller restart or leader change, and when a group first
appears, every planned model holds what it runs for one fill timeout and shows
the blocked reason `quiet-period`. Expect it on every upgrade.

**A model that just gave GPUs cannot get them back for a while, even when
short.** The reversal hold (`reversal-hold`) lasts about twice a release time,
around 12 minutes with a 300 s scale-down window. Only a hard imbalance skips
it: the short model at 0.9 or more of its scale-up threshold while a donor
would stay at 0.6 or less (`stabilization.skipWaitsWhen`).

**One slow donor slows the whole group.** Release and fill timeouts come from
the slowest donor in the group: one model with a 30-minute scale-down window
stretches every transfer. `wva_utilization_share_effective_seconds` shows the
values in force.

### More costs

- **How much headroom.** While every peak fits, the fleet holds the whole
  quota. While they contend, each model holds up to `maxHeadroomReplicas`
  times its weight above its need.
- **Spare given back, then started again.** While the quota is contended, a
  model above its need plus headroom returns the extra after
  `consecutiveCyclesBeforeMove` cycles, once its load is at or below
  `stabilization.returnSpareWhenLoadAtMost`. The return waits, for at most its
  scale-down window, while more than 0.6 x `queueLengthThreshold` requests (3
  by default) wait on one of its pods. A P/D decode pod counts only while its
  KV cache is full; prefill pods not at all. If demand comes back after the
  drain, it starts a pod again, cold
  ([troubleshooting](../../reference/troubleshooting.md#a-model-shrinks-while-no-other-model-grows)).
- **The reversal hold** above.
- **Capped headroom while no history is stored.** For a new group, or after a
  restart that finds no history in the ConfigMap
  `wva-utilization-share-history`, every model counts at its `maxReplicas`
  until a `flappingWindow` of history exists (about 100 minutes by default).
- **Pod annotations.** Each donor pod a transfer chose carries a transfer mark
  and a low `pod-deletion-cost` until the transfer ends. A downgrade to a
  version without the optimizer leaves them behind
  ([how to remove them](../../reference/troubleshooting.md#transfer-marks-left-on-pods-after-a-downgrade)).

Some traffic shapes still lose something in simulation (share of demand left
unserved). If a model looks like one of these, give it a floor
([for model owners](../../reference/scaling-policy.md#for-model-owners-what-the-optimizer-may-take-and-how-to-protect-a-model)):

- **A noisy model beside one that bursts for long.** The noisy model gets
  headroom on its average demand, not at each crest (blocked reason
  `fast-swing-capped`): 0.02 to 0.06 more unserved, while the bursting model
  goes from 0.33 to 0.21.
- **Whole-node models bursting for 20 to 30 minutes in a tight quota.** Moves
  that would land after the burst ends keep the usual pace (`burst-ending`);
  still about 0.007 worse than without that rule.
- **A dip right after a group starts.** For about the first 12 minutes no
  model's peak is known, so a model in a dip gives replicas back and
  cold-starts them when demand returns (0.038 unserved for a 12-minute dip).
  Stored history avoids it after a restart.

### Verifying it

The guide's [Verification](../../guides/utilization-share/#verification) has
the queries for each rollout step, for tenants and admins. Exactly one
`wva_utilization_share_mode` series is 1 (`off`, `invalid`, `shadow` or
`active`). Once active, `wva_utilization_share_transfers_total` by `outcome`
should show `done`; rising `aborted`, `fill-timeout` or `wrong-pod` mean
transfers are not landing. Its `namespace` label is the controller's and
`scope` is the quota's. When it evaluates but never moves anything, see
[troubleshooting](../../reference/troubleshooting.md#the-optimizer-evaluates-but-never-moves-anything).

### How it is tested

- Unit: configuration (`internal/config/utilization_share*_test.go`),
  planning (`internal/engines/allocation/utilization_share_*_test.go`) and the
  cycle (`internal/engines/steadystate/utilization_share_*_test.go`).
- End-to-end on kind with emulated GPUs (`make test-e2e-full`, labels `full`
  and `utilization-share`): shadow then active under a 4-GPU namespace quota
  (`utilization_share_test.go`), a P/D LeaderWorkerSet funded by a donor set
  (`utilization_share_pd_test.go`), and a 2-GPU receiver funded on a full node
  (`utilization_share_nodes_test.go`). They skip when no node carries a GPU
  product label, and a skip reports as success: run them with `-v`
  ([testing](../../developer-guide/testing.md)).
- **Not covered:** real model servers in the e2e; whole-node, multi-GPU or P/D
  pods under real load; a cluster quota and the `clusterNamespaces` canary
  (unit only); a restart that rebuilds transfers from pod marks (unit only).

### Tuning it

Every key (`tolerance`, `reserveGPUs`, `maxHeadroomReplicas`, `shadow`,
`physicalGroups`, `weightClasses`, `namespaces`, `clusterNamespaces`,
`stabilization`) and what the optimizer writes on pods:
[`optimizer` in the scaling policy](../../reference/scaling-policy.md#optimizer-cluster-default-only-live).
Metrics, transfer outcomes and blocked reasons:
[utilization-share metrics](../../reference/prometheus.md#utilization-share-optimizer-metrics).
Events: [monitoring](../../reference/monitoring.md#events-on-the-models-scale-targets).
When it misbehaves:
[troubleshooting](../../reference/troubleshooting.md#utilization-share-optimizer).
The design and its simulators: [the proposal](../../proposals/utilization-share-optimizer.md).
