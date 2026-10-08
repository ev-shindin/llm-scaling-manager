# Share one GPU quota between models whose peaks do not coincide

> **Experimental.** The optimizer is built, in shadow mode and acting, with
> donor sets, P/D roles and node-aware placement. Unit tests cover it, and three
> end-to-end specs run it on kind with emulated GPUs and the inference
> simulator. **No benchmark has been run on it yet.** A benchmark scenario
> exists (below), but no real model and no real accelerator has gone through a
> transfer under measured load. That is the short leg. The defaults may also
> change: the `tolerance` of 0.15, the weight classes and a `reserveGPUs` of 0
> are starting points, and the
> [proposal](../../proposals/utilization-share-optimizer.md#14-open-questions)
> says they should be tuned from shadow-mode data. The short scale-down window
> for urgent transfers (stage 3) is still a design.

Several models share one GPU quota, and their traffic peaks at different times.
Today's optimizer hands out a quota first come, first served. A model that
scaled up earlier keeps its GPUs, and a model that needs them later is refused.
The worst case is a swap: one model's load falls while the other's rises. The
falling model keeps its replicas through its whole scale-down window, and the
rising model is refused the GPUs it needs during that time, even though the
quota as a whole has enough for both.

The utilization-share optimizer shares the quota on purpose. Each model first
gets its **need**: the GPUs that put it exactly at its `scaleUpThreshold`. The
rest of the quota is shared as **headroom** above need, in proportion to each
model's **weight**. When the GPUs a model holds drift too far from that target,
the optimizer moves GPUs with a **transfer**:

1. It marks one of the donor's pods, so the ReplicaSet removes that pod and no
   other.
2. It lowers the donor's target.
3. It waits until the donor's GPUs are actually released.
4. Only then does it raise the receiver.

The scale-down always comes first, and the receiver is raised only into GPUs
that are free.

**Use it when** two or more models scale under one quota, as a namespace quota
or a cluster quota per accelerator type, and their demand moves between them
over hours, not minutes. It also fits an allotment that is paid for whether
used or not (on-prem hardware, a reservation, a team quota), where an idle GPU
is pure loss and headroom costs nothing extra.

**Do not use it when:**

- **There is nothing to share.** With one aggregated model per quota there is
  no other model to move GPUs to or from. The only effect is that the model is
  raised into the quota's spare GPUs as headroom.
- **There is no quota limiter.** Without a `limiters:` list the optimizer has no
  budget, and it stays off (`invalid`). Under a `gpu-inventory` limiter alone, it
  plans no cluster group unless you set `physicalGroups: true`. Spending the
  budget there means holding **every GPU of that type** in the cluster.
- **You want to give unused GPUs back.** This mode deliberately trades cost for
  headroom: it spends the whole quota. A fleet billed per GPU-hour that can
  return idle GPUs should not turn it on.
- **The donors cannot be steered.** A transfer depends on choosing the pod that
  goes, and some workloads do not allow it. A Deployment that is often
  mid-rollout, a Deployment with pods stuck Pending or not Ready, or a
  LeaderWorkerSet whose highest-index group is terminating is refused as a donor
  (`donor-not-steerable`) and backs off. A fleet that is like this most of the
  time gets no transfers.
- **The real limit is not the scaling manager's quota.** A quota counts only
  what the scaling manager's own variants hold. If a Kubernetes `ResourceQuota`
  or unmanaged workloads on the same accelerators are what actually stops
  pods, a donor's released GPUs can be taken by a pod the scaling manager did
  not place (`release-taken`). The quota then shows room that the cluster does
  not have.
- **Load swings back faster than a transfer lands.** A move takes effect about
  twelve minutes after it is decided with default settings: two cycles to
  confirm the change, the donor's scale-down window and drain, then the
  receiver's start. In the proposal's simulator, standing still beats moving
  GPUs when the load reverses within about eight of those latencies, roughly
  100 minutes
  ([§6.7](../../proposals/utilization-share-optimizer.md#67-why-it-does-not-oscillate-and-what-it-cannot-follow)).
  The swing rule limits that loss but does not turn it into a gain. These are
  design figures, not measurements.

Also, a model whose variants run on more than one accelerator type is never
planned: it stays on today's optimizer.

## What it needs

- A quota limiter in the `limiters:` list of the **cluster default** policy
  entry, with accelerators that resolve. [Cap what each tenant may
  take](../tenant-gpu-quotas/) declares the limiter, and
  [bound-by-gpus](../bound-by-gpus/) checks the accelerators first. The
  optimizer block is read from that same entry and nowhere else.
- Model servers that drain on termination. A transfer removes a donor pod
  through an ordinary scale-down, and the only protection for requests in
  flight on that pod is its `preStop` hook and `terminationGracePeriodSeconds`.
- When several controllers share a cluster, a distinct `CONTROLLER_INSTANCE` on
  each one. Every transfer mark records the controller that wrote it, and a
  controller cleans up only its own marks. Two controllers with the same value
  remove each other's live marks.
- For placement, node information. A namespace-scoped install cannot list
  nodes, so it plans by GPU count alone. It does not check that the GPUs a donor
  frees are on a node where the receiver's pod fits, and the two blocked reasons
  that need nodes (`release-taken`, `release-shape-mismatch`) never appear.

## Declaring it

One block in the cluster `default` entry, beside `limiters:`:

```yaml
default: |
  analyzers:
    - type: saturation
  limiters:
    - type: quota
      name: cluster-h100
      scope: cluster
      quotas: { H100: 32 }
  optimizer:
    type: utilizationShare
    utilizationShare:
      shadow: true          # compute, publish and log; move nothing
```

**Roll it out in three steps:**

1. **Shadow.** The block above does everything except move pods. Leave it in
   shadow long enough to cover the load you care about, including each model's
   peak. Then read what it would have done (see
   [Verifying it](#verifying-it)).
2. **Canary, on a cluster quota.** `clusterNamespaces: [team-canary]` makes the
   cluster group plan only the models of the namespaces you list. Every other
   model stays on today's optimizer. On namespace quotas, use
   `namespaces.<ns>.enabled: false` to keep a namespace out instead.
3. **Active.** Set `shadow: false`. The key **defaults to `false`**, so a block
   with no `shadow` key acts from the next cycle.

Model owners pick a weight class on their own scaling-policy entry
(`weightClass: important`, or a `weight:` number). Raising a model's
`minReplicaCount` protects it outright, because a floor is never taken. Every
other model in the group pays for that floor. What can be taken from a model,
which pod goes and how to protect one are covered in the reference:
[for model owners](../../reference/scaling-policy.md#for-model-owners-what-the-optimizer-may-take-and-how-to-protect-a-model).

The step-by-step version, with the check after each step, is the guide:
[Turn on the utilization-share optimizer, safely](../../guides/utilization-share/).

## Four things that surprise people

**A missing `shadow` key acts.** `optimizer: {type: utilizationShare}` on its
own moves replicas from the next cycle. Write `shadow: true` explicitly until
you have read what it would do.

**Turning it on freezes the group for a few minutes.** When the optimizer starts
acting, when a controller restarts or the leader changes, and when a group first
appears, every planned model of the group holds what it runs for one fill
timeout. That is about three minutes with default timings, and nothing in the
group scales during it. Expect the same freeze on every upgrade.

**A model that just gave GPUs cannot receive them back for a while, even when it
is short.** That is the anti-oscillation rule. It applies to urgent receivers
(below their need) as well, and it lasts about twice a release time from the
start of the transfer it gave in. With a 300 s scale-down window, that is on
the order of 12 minutes. It shows as the blocked reason `reversal-hold`.

**One slow donor slows the whole group.** The release and fill timeouts are
derived per group, from the slowest donor configuration in it: the longest
scale-down window, polling interval and termination grace. One model with a
30-minute scale-down window stretches every transfer in its group.
`wva_utilization_share_effective_seconds` shows the values in force and where
each came from.

## Verifying it

```promql
# the mode in force: off, invalid, shadow or active
wva_utilization_share_mode == 1

# shadow and active: which roles a move would fix, and how many replicas
wva_utilization_share_actionable == 1
wva_utilization_share_replicas_to_move
wva_utilization_share_headroom          # < 0: the role is short

# active only: transfers that ended, by outcome (done is healthy)
sum by (outcome) (increase(wva_utilization_share_transfers_total[1h]))

# models held back, and why (the optimizer sets its reasons only while it acts)
wva_model_scaling_blocked == 1
```

```bash
# the transfers, on the Deployments and LeaderWorkerSets they moved
kubectl get events -n <model-namespace> --field-selector reason=UtilizationShareGiving
kubectl get events -n <model-namespace> --field-selector reason=UtilizationShareReceived
```

`invalid` means the block did not validate, or there is no `limiters:` list. The
controller log says which: `Utilization share: invalid optimizer block; keeping
today's optimizer`. Today's optimizer then runs, and the limiters keep working.

In shadow mode, nothing is blocked by the optimizer, so no
`wva_model_scaling_blocked` reason comes from it. The log line `Utilization
share: would rebalance (shadow)` is written when the set of roles it would move
changes, not every cycle. Its `frozen` field lists the models it leaves to
today's optimizer, with the reason.

Once it acts, rising `aborted`, `fill-timeout` or `wrong-pod` outcomes mean
transfers are not landing. The alerts for that, and for a block that turned
`invalid`, are in
[monitoring](../../reference/monitoring.md#the-metrics-that-answer-specific-questions).
When it evaluates but never moves anything, work down
[the troubleshooting list](../../reference/troubleshooting.md#the-optimizer-evaluates-but-never-moves-anything).

## What it costs

- **The whole quota.** GPUs that would otherwise sit idle are handed out as
  headroom, so the fleet holds what the quota allows, not what its load needs.
  That is the point of the mode, and it is a cost wherever GPUs could be given
  back.
- **A donor replica, with its in-flight requests.** A transfer is a donor pod
  going away through an ordinary scale-down. Requests in flight on it survive
  only as well as the model server drains, and the termination grace also
  lengthens every transfer in the group.
- **The receiver waits for the release.** The receiver is raised only after the
  donor's GPUs are free: after the donor's scale-down window, its drain, and
  then the receiver's own pod start and model load. For that whole time the
  receiver runs short.
- **The freeze and the hold above.** About one fill timeout of no scaling in a
  group after every restart, and a reversal hold that can keep a short model
  waiting.
- **Pod annotations.** While a transfer releases, each donor pod it chose
  carries a transfer mark and a low `pod-deletion-cost`. The controller removes
  both when the transfer ends or the optimizer is switched to shadow or off.
  A downgrade to a version without the optimizer leaves them behind:
  [marks left after a downgrade](../../reference/troubleshooting.md#transfer-marks-left-on-pods-after-a-downgrade).

## How it is tested

- Unit, configuration: `internal/config/utilization_share_test.go`,
  `utilization_share_active_test.go` and `utilization_share_canary_test.go`
  cover parsing, validation, the shadow and active modes and
  `clusterNamespaces`.
- Unit, planning: `internal/engines/allocation/utilization_share_*_test.go`
  (ten files) cover targets, groups, the plan, donor sets on nodes, the
  ledger and the history it retains, timings, back-off, withholding and
  concurrency.
- Unit, the cycle: `internal/engines/steadystate/utilization_share_*_test.go`
  (thirteen files) cover shadow, actuation, fills, floors, marks, namespaces,
  nodes, P/D pairs, blocked reasons, pod steering, mode transitions and errors.
- End-to-end, labels `full` and `utilization-share`, run by
  `make test-e2e-full` on kind with emulated GPUs:
  - `test/e2e/utilization_share_test.go` (*Utilization share optimizer*). Under
    a 4-GPU namespace quota, shadow mode evaluates the group and touches nothing.
    Then, active, A's marked pod goes first, and B grows after it.
  - `test/e2e/utilization_share_pd_test.go` (*on a P/D LeaderWorkerSet model*):
    B's decode grows by one group, funded by two of A's replicas as one donor
    set.
  - `test/e2e/utilization_share_nodes_test.go` (*on full GPU nodes*): B's 2-GPU
    pod is funded from two 1-GPU A pods the controller planned on one full node.

  All three skip when no node carries a GPU product label, and the last two
  also skip when no node has enough GPUs of one product. A skip reports as
  success, so run them with `-v`
  ([testing](../../developer-guide/testing.md)).
- **Not covered:**
  - A real model server or real accelerators: the e2e uses the inference
    simulator, so drain behaviour and model load time are not exercised.
  - Any measured load: the benchmark below has not been run.
  - A cluster quota group and the `clusterNamespaces` canary, which are
    unit-tested only. All three e2e specs use a namespace quota.
  - A controller restart that rebuilds transfers from pod marks, a
    scale-from-zero wake claiming released GPUs (`redirected`), and a donor
    refused as `donor-not-steerable`, which are unit-tested only.

## How it is benchmarked

**Not yet.** The scenario exists and has **not been run**, so this page has no
numbers and makes no claim about what the optimizer buys under load.

It is [Two models, anti-phase bursts, under one quota](../../guides/benchmarking/two-model-utilization-share.md):
the two-model anti-phase load of the warm-pool benchmark, under one quota
smaller than the two models' combined peak, run three times. The three arms
differ only in the policy's `optimizer:` block: none (`today`), shadow
(`shadow`) and acting (`share`). Read that page before you trust a number from
it. With the default load shape, a full swing between the two models takes
about 19 minutes, well below the roughly 100-minute break-even above. A run
meant to show the case this path is built for needs longer phases.

## Tuning it

Every key (`tolerance`, `reserveGPUs`, `shadow`, `physicalGroups`,
`weightClasses`, `namespaces`, `clusterNamespaces`), how weights are set per
model, and what the optimizer writes on pods:
[`optimizer` in the scaling policy](../../reference/scaling-policy.md#optimizer-cluster-default-only-live).
Every metric, transfer outcome and blocked reason:
[utilization-share metrics](../../reference/prometheus.md#utilization-share-optimizer-metrics).
The Events on scale targets:
[monitoring](../../reference/monitoring.md#events-on-the-models-scale-targets).
When it does not do what you expect:
[troubleshooting](../../reference/troubleshooting.md#utilization-share-optimizer).
The design and the simulator it was tested in:
[the proposal](../../proposals/utilization-share-optimizer.md).
