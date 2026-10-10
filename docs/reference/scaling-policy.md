# Saturation Scaling Configuration

## Overview

llm-scaling-manager supports saturation-based scaling using KV cache utilization and queue length metrics. This feature is enabled by default and configured via a ConfigMap.

**Key features:**
- ✅ ConfigMap-based configuration with global defaults and per-model overrides
- ✅ **Efficient caching** with single read on startup (zero API calls during reconciliation)
- ✅ **Automatic reload** via ConfigMap watch (immediate response to changes)
- ✅ **Thread-safe** concurrent access with RWMutex
- ✅ Graceful degradation if ConfigMap missing (the analyzer has hardcoded defaults — see [Default Configuration](#default-configuration))

## Analyzer Selection

There is one saturation analyzer: the token/capacity-based analyzer. The V1
percentage/spare-capacity analyzer was removed, so `analyzerName` and the
`analyzers:` list no longer select between engines — see the note on
`analyzerName` in `internal/config/saturation_scaling.go`.

### Threshold ownership

What each threshold controls:

| Threshold | Notes |
|-----------|-------|
| `kvCacheThreshold` | Ceiling on usable KV: capacity is the physical budget scaled by this (default 0.80) |
| `queueLengthThreshold` | Queue depth at which the compute-bound capacity estimate is taken |
| `scaleUpThreshold` | Utilization above which scale-up triggers — engine post-step (default 0.85) |
| `scaleDownBoundary` | Utilization below which scale-down is safe — engine post-step (default 0.70) |

## ScalingPolicy Schema (Phase 1)

Phase 1 of the [ScalingPolicy proposal](https://github.com/llm-d/llm-d-workload-variant-autoscaler/pull/1245)
extends this ConfigMap — additively, with no CRD, migration, or new install steps —
with a plugin envelope on each entry. Existing flat entries keep working unchanged;
adoption is optional per entry.

### Analyzer plugin envelope (`type` / `parameters`)

Each `analyzers:` entry accepts a `type` and a free-form `parameters` map:

```yaml
analyzers:
  - type: saturation
    parameters: { scaleUpThreshold: 0.95 }
```

- `type` selects the analyzer plugin (`saturation`, `queueing-model`, `throughput`).
  It falls back to `name` when omitted, so the legacy `- name: saturation` form still
  works and is treated as `type: saturation`.
- `parameters` carries plugin configuration. The well-known keys `scaleUpThreshold`,
  `scaleDownBoundary`, `score`, and `enabled` are equivalent to the corresponding
  top-level/typed fields (an explicit typed field wins over the same key in
  `parameters`). Unknown keys are tolerated (forward-compatible); a wrongly-typed
  known key fails validation and the entry is skipped.

There is nothing to opt out to: the token/capacity-based analyzer runs whether or
not `analyzers:` is set — see [Analyzer Selection](#analyzer-selection).

### `scaleToZero`

An entry carries the whole scale-to-zero policy, and is the only per-model surface
for it:

```yaml
scaleToZero:
  enabled: false
  retentionPeriod: 5m
```

| field | absent means | ends at |
| --- | --- | --- |
| `enabled` | inherit | the `WVA_SCALE_TO_ZERO` deployment flag |
| `retentionPeriod` | inherit | 10m (`DefaultScaleToZeroRetentionPeriod`) |

#### How long until a model actually reaches zero

**`retentionPeriod` and KEDA's `cooldownPeriod` add up.** They are sequential, not
overlapping, and neither knob alone predicts the result:

```text
last request ──┬─ retentionPeriod ─┬─ ≤1 optimize interval ─┬─ cooldownPeriod ─┬─ 0 replicas
               │ (the scaling manager's idle query)│ (the scaling manager publishes 0, the  │ (KEDA)           │
               │                   │  trigger goes inactive)│                  │
```

The scaling manager's external scaler reports the trigger active *unless the scaling manager has decided the model
needs zero replicas*, so KEDA's cooldown clock cannot start until the scaling manager has already
decided to park — and the scaling manager decides that only once `increase(...[retentionPeriod])`
reads zero. With both defaults that is **10m + 300s ≈ 15 minutes**, plus up to one
`GLOBAL_OPT_INTERVAL` (15s). A fleet that "will not park" is often just this sum.

This applies to the **deactivation step only** — the final drop to
`minReplicaCount`/`idleReplicaCount` once the trigger reports inactive. An ordinary
scale-down while the trigger is still active (10 → 3) goes through the HPA and is
governed by its own stabilization behaviour; neither KEDA cooldown is consulted.

The scaling manager adds no hold of its own here. KEDA already guards that one transition, per
ScaledObject, from cluster state — the same reason the scaling manager reads `minReplicaCount`
off the object rather than duplicating it into trigger metadata.

### `scaleFromZero`

An entry may tune how a model is woken from zero replicas:

```yaml
scaleFromZero: { requirePrefill: true }
```

| key | default | meaning |
| --- | --- | --- |
| `requirePrefill` | `false` | Refuse to wake a P/D-disaggregated model unless a prefill variant can be placed alongside decode. |

**Why the default is `false`.** When the scale-from-zero engine picks which
variants to wake, decode is mandatory and prefill is best-effort. That follows
from how the llm-d router behaves rather than from a preference: with no prefill
endpoint selected it routes the request to a decode endpoint, and the decode
worker runs both stages locally. The router already declines disaggregation on
its own for short prompts and high prefix-cache hits, so decode-only is a normal
serving mode, not a failure mode. If prefill cannot be placed — no free GPUs on
its accelerator, or a namespace quota that excludes it — waking decode alone
still serves the queued request, whereas refusing to wake trades a slower model
for no model at all.

Set `requirePrefill: true` when degraded prefill performance is worse than
unavailability for this model: a strict TTFT SLO, or decode nodes that must not
absorb prefill load. With it set, a model whose prefill cannot be placed is left
at zero and the engine logs why at INFO:

```text
Scale-from-zero: no variant woken for a model with pending requests
  namespace=... modelID=... reason=prefill-required
```

Other `reason` values are `no-decode-candidate` (the model has no inactive
decode/`both` variant to wake) and `no-capacity` (nothing fit the GPU budget).
Each is logged once per verdict change, not once per poll. A model that is
already serving is not a refusal and is not reported here at all.

Role comes from the `llm-d.ai/role` pod-template label (`prefill`, `decode`, or
absent/`both`). A model with no prefill variants is unaffected by this setting.

**Placement.** Selection prefers a variant that can actually get GPUs, checked
against the same GPU/quota limiters the optimizer honours (see
[`limiters`](#limiters-cluster-default-only-live)) and evaluated on the **sum**
of the variants being woken together — prefill and decode can each fit alone
while the pair does not.

The check is **best-effort**: when the GPU budget cannot be determined it is
skipped rather than denying the wake, because refusing to serve a queued request
on the strength of a missing measurement is the worse failure. It is skipped
when any of these hold:

| condition | why |
| --- | --- |
| no limiter is configured | declaring no `limiters:` list means nothing bounds scaling, so there is nothing to place against. This is the shipped default |
| no usage snapshot yet | the saturation engine is the sole producer of GPU usage; until it completes one cycle there is no denominator. This is the state on the first cycle after a restart — exactly when a request may be queued |
| a provider failed to compute constraints | a partial view would deny any accelerator type the surviving providers happen not to mention, turning "could not reach this provider" into "cannot place this variant" |
| — | *(An unresolved accelerator does not skip the check: that candidate simply contributes no demand, and the rest of the set is still checked. A variant with no nodeSelector/nodeAffinity GPU key cannot be charged to any pool, so it is not counted rather than denied.)* |

> **The GPU budget can still over-state free capacity**, affecting the
> GPU-aware optimizer as well as scale-from-zero: usage on an unresolved
> accelerator is charged to no pool, and the physical `Limit` counts every
> *installed* GPU rather than what is actually available. Both are described,
> with their fixes and current status, in
> [GPU Capacity Accounting](../concepts/gpu-capacity-accounting.md). The short version: make
> the accelerator resolvable with a `nodeSelector`/`nodeAffinity` GPU key — the scaling manager
> emits an `AcceleratorNotResolved` event otherwise, and resolves a RUNNING
> variant by observing the nodes its pods landed on — and declare an explicit
> quota limiter if you need a hard ceiling.

Note the usage snapshot is published independently of the optimizer branch, so
scale-from-zero can check placement on a cycle the optimizer never reached.

There is no separate enable flag: the `limiters` list below is the whole answer.
Declaring a limiter selects the GPU-aware optimizer (`GreedyByScore`) *and* arms
the scale-from-zero capacity check; declaring none leaves both off. `enableLimiter`
used to gate only the first of those, so a quota could be declared and silently
never enforced, while scale-from-zero consulted a limiter nobody had asked for.

### `limiters` (cluster-default only, live)

The `default` entry selects the GPU limiter for the scaling pipeline. This is the
**sole source** — there is no `--limiter-type` flag or quota config file — and it is
applied **live**: editing the ConfigMap switches the limiter without a controller
restart (the engine rebuilds it on the next optimization cycle).

A `limiters:` list selects a **single mode**, so declare **one** of these forms:

```yaml
# (a) physical-capacity limiter. NOT a default -- with no limiters: list,
# nothing limits at all (NewLimiterFromConfig returns a no-op limiter):
default: |
  analyzers:
    - type: saturation
  limiters:
    - type: gpu-inventory            # alias: inventory

# (b) operator-declared quota limiter:
default: |
  analyzers:
    - type: saturation
  limiters:
    - type: quota                    # inline quota entry — same schema as QuotaLimiterConfig
      name: cluster-h100
      scope: cluster
      quotas: { H100: 32 }
```

- **Single mode.** If a list contains both a `quota` and a `gpu-inventory` entry, the
  quota entry wins and the `gpu-inventory` entry is ignored — only one limiter is built.
  (Composition of physical + quota caps is tracked separately in #1003.)
- `type: quota` uses the **same schema** as `QuotaLimiterConfig`, so `scope`, `quotas`,
  `namespaceQuotas`, and `exclude` all apply; multiple quota entries are composed.
  A quota entry may also set `kueue: { enabled: true }` to be bounded by the quotas
  Kueue declares — see [Kueue as a quota source](quota-limiter.md#kueue-as-a-quota-source).
- **Cluster-default only.** The list is read only from the global (system-namespace)
  `default` entry — a budget-scope setting, like `enableRescale` — so a tenant cannot
  widen a cap via a per-model or namespace-local entry. A `limiters:` block placed on any
  other entry parses and validates but is **silently ignored** at runtime.
- With no `limiters:` block **nothing bounds scaling** — that is the shipped
  default, and the controller says so at startup: "scaling is UNCONSTRAINED".

### `optimizer` (cluster-default only, live)

The `optimizer:` block, beside `limiters:` in the same `default` entry, selects
the **utilization-share optimizer**. Without it, a quota that runs out is first
come, first served: whichever model scaled up earlier keeps its GPUs, and a model
that needs them later waits. With it, the GPUs a quota allows are shared out on
purpose. Every role (the decode and prefill of a P/D model, or the one role of an
aggregated model) is first given its *need* — the GPUs that put it exactly at its
`scaleUpThreshold` — and what is left is shared as headroom above that need, in
proportion to each model's weight. When the GPUs no longer match those targets,
the optimizer *transfers* replicas: it scales a donor down, waits until its GPUs
are actually released, and only then scales the receiver up.

The design and its reasoning are in the
[utilization-share optimizer proposal](../proposals/utilization-share-optimizer.md);
this section covers what you set and what you will see.

**It needs a budget.** The optimizer shares the GPUs a limiter allows, so it
needs a `limiters:` list. Without one it stays off (`wva_utilization_share_mode`
reports `invalid`) and the controller logs
`needs a limiters: list: there is no budget to share`. It plans:

- one group per namespace that a namespace quota (`scope: namespace`) covers, per
  accelerator type;
- one cluster group per accelerator type for every other model, when the cluster
  limiter is a `quota`. Under a `gpu-inventory` limiter alone there is no cluster
  group unless you set `physicalGroups: true` (see the table).

A model whose variants run on more than one accelerator type, or whose analyzer
result is not live this cycle, is left to today's optimizer for that cycle.

**Start in shadow mode.** `shadow` defaults to `false`, so
`optimizer: {type: utilizationShare}` on its own **acts**: it moves replicas
between models from the next cycle. Turn it on in shadow first, read what it
would do, and switch `shadow` off when the numbers look right:

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

In shadow mode every evaluation metric is published
([utilization-share metrics](prometheus.md#utilization-share-optimizer-metrics)),
and the controller logs `Utilization share: would rebalance (shadow)` whenever the
set of roles it would move changes (not every cycle). `wva_utilization_share_actionable == 1` names the roles it
would move; `wva_utilization_share_replicas_to_move` says how many replicas.

| Key (under `utilizationShare:`) | Default | Meaning |
| --- | --- | --- |
| `tolerance` | `0.15` | How far a role's GPUs may be from its target, as a fraction of the target, before a move is considered. Must be greater than 0 and less than 1. Wider means fewer, larger moves. The band is never narrower than half a replica -- four GPUs for 8-GPU replicas -- but a role below its need is never in band while its target covers that need. |
| `reserveGPUs` | `0` | GPUs held out of the shared budget so a scale-from-zero wake can start at once instead of waiting for a transfer. |
| `shadow` | `false` | Compute, publish and log everything; actuate nothing. |
| `physicalGroups` | `false` | Also plan groups bounded only by the physical GPU inventory (no quota), where spending the whole budget means holding every GPU of that type. |
| `weightClasses` | `best-effort: 0.5`, `standard: 1`, `important: 2`, `critical: 4` | Class names and their weights. Replaces the defaults whole when set. Exactly one class must have weight `1`; it is the default class. |
| `namespaces.<ns>.enabled` | `true` | `false` keeps that namespace's quota group on today's optimizer. |
| `clusterNamespaces` | empty (every namespace) | A list of namespaces. When set, the cluster groups plan only the models of these namespaces; every other model is left to today's optimizer. A canary for a cluster quota. An empty entry (`""`) makes the block invalid. Does not affect namespace quota groups. |
| `immediateRebalance.enabled` | `true` | Move GPUs at once, without the reversal hold or the second confirming cycle, from a model with plenty of headroom to one at its scale-up threshold (the two keys below). `false` makes every move wait. |
| `immediateRebalance.receiverLoadAtLeast` | `0.9` | The receiver's load, at or above which it may be funded at once. **Load** is a model's utilization as a fraction of its scale-up threshold: `1` is where it would scale up, `0.9` is 90% of the way there. |
| `immediateRebalance.donorLoadAtMost` | `0.6` | The donor's load on the GPUs it keeps after giving, at or below which it may give at once. Must be below `receiverLoadAtLeast`: the gap is what stops GPUs going back and forth. Raise it (or lower `receiverLoadAtLeast`) to rebalance smaller imbalances sooner, at the price of more moves that are later reversed. |

Nothing else is configurable. The timings a transfer runs on — how long a release
may take, how long a receiver may stay Pending, how long a role that gave must wait
before it receives — are derived per group, not per donor: from the slowest
donor configuration in the group (the longest scale-down window and polling
interval among the donors' ScaledObjects, and the longest termination grace among
their pod templates), and from the releases the group has measured. One model with
a 30-minute scale-down window therefore stretches every transfer in its group.
`wva_utilization_share_effective_seconds` shows the values in force and where each
came from.

**Weights.** A model's weight decides how much of the spare headroom its roles
get and, when the quota is short, whose shortfall counts most: a role of weight 2
gets twice the headroom of a role of weight 1 relative to its need, and is cut
last. Set it per model with **one** of:

```yaml
  # a per-model override (or the default entry, for every model)
  ibm_granite-13b.production: |
    model_id: ibm/granite-13b
    namespace: production
    weightClass: important     # a class name from weightClasses
    # weight: 1.5              # or a number, clamped into the range the classes span
```

A model that sets neither gets the default class. An unknown class, a weight that
is not a positive number, or both fields set fall back to the default class and
log `invalid weight on a model's scaling-policy entry`; none of them rejects the
entry. A number outside the classes' range (0.5 to 4 with the default classes) is
clamped into it.

A namespace with its own namespace-local scaling-policy map sets its models'
weights and thresholds itself. Such a model is planned in its namespace's own
quota group, which that namespace owns whole, but **not** in the cluster group,
where its weight would be weighed against other tenants'; there today's optimizer
keeps it.

**Disabling one namespace:**

```yaml
  optimizer:
    type: utilizationShare
    utilizationShare:
      namespaces:
        team-a: { enabled: false }
```

This applies to namespace quota groups only. A name that no namespace quota covers
disables nothing, and the controller logs
`Utilization share: the namespaces block disables namespaces no namespace quota names`.

**A canary on a cluster quota.** `namespaces:` cannot narrow a cluster group;
`clusterNamespaces:` does. With it set, the cluster group of each accelerator type
plans only the models of the listed namespaces, sharing among them the quota's
free GPUs plus what they hold. Every other model stays on today's optimizer, and
the GPUs it holds stay outside the group. Start with one or two namespaces whose
owners agreed to it, then widen the list or remove it (empty plans every
namespace):

```yaml
  optimizer:
    type: utilizationShare
    utilizationShare:
      clusterNamespaces: [team-canary, team-b]
```

A model left out this way is listed in the log line's `frozen` field as
`namespace not in utilizationShare.clusterNamespaces`.

**Where the block is read.** Like `limiters:`, only from the cluster `default`
entry, and live: an edit takes effect on the next cycle. An `optimizer:` block in
a namespace-local scaling-policy map has no effect; the controller logs
`ignoring optimizer blocks in namespace-local scaling-policy maps` with the
namespaces. A malformed block (an unknown key, a value out of range) turns only
the optimizer off and logs `Utilization share: invalid optimizer block; keeping today's optimizer`;
the limiters in the same entry keep working. `wva_utilization_share_mode` says
which state is in force (`off`, `invalid`, `shadow` or `active`); the leader
publishes it at the start of every cycle, from its first, including cycles with
no active model, and on any cycle it is not `active` it also removes the marks
described below.

On a **namespace-scoped install whose cluster policy lives in a separate policy
namespace**, the controller cannot watch that namespace: it reads the policy when
it starts, and an edit there takes effect only after a controller restart. That
includes switching the optimizer to `shadow` or off, and so the pre-downgrade
step in [marks left after a downgrade](troubleshooting.md#transfer-marks-left-on-pods-after-a-downgrade):
edit, then restart the controller, then wait a cycle.

**What it writes on pods.** While a transfer releases, each donor pod it chose
carries two annotations: `llm-d.ai/utilization-share-transfer` (the transfer, as
JSON, including the pod's previous deletion cost) and
`controller.kubernetes.io/pod-deletion-cost`, set below every sibling's so the
ReplicaSet removes that pod and not a sibling: `-1000`, or one below the lowest
cost a user set on a sibling when that is lower still. The mark records the cost
it wrote in its `cost` field (absent means `-1000`). A restarted controller
rebuilds its in-flight transfers from these marks. The controller removes both,
restoring the pod's previous deletion cost, when the transfer ends (including a
release judged by count, where another of the donor's pods went and the marked
one survives), when the optimizer is switched to shadow or off (including across
a controller restart), and when no quota has been read for two minutes. When it removes a mark it
restores the previous cost only if the pod still carries exactly the cost the
mark wrote: a deletion cost someone else set after the mark is left as they set
it.

A mark sits on the donor tenant's pod, so it names another model only when
every party is in one namespace. A transfer whose donor and receiver are in
different namespaces, or a donor set whose donors and receiver are not all in
one namespace, writes on every member's pod a mark with no receiver, no receiver
GPUs and no set link; after a controller restart each is restored as a release
with no receiver: the donor still releases, and the receiver is planned again
once the restart's quiet period (one fill timeout) ends. Transfer IDs end
in a random part, so an ID seen on one pod does not predict another.

Each mark records the `CONTROLLER_INSTANCE` that wrote it in its `instance` field
(omitted when the variable is unset), and the sweep after a restart or
switch-off removes only that instance's marks, so **several controllers sharing a
cluster must each set a distinct `CONTROLLER_INSTANCE`**; with the same value, one
controller's sweep removes the other's live marks. For the same reason, renaming
a controller's `CONTROLLER_INSTANCE` orphans the marks it wrote under the old
name: switch to shadow and wait a cycle before renaming, or clean them up as for
a downgrade. Downgrading to a version without the optimizer leaves them behind;
see [marks left after a downgrade](troubleshooting.md#transfer-marks-left-on-pods-after-a-downgrade).

**Namespace-scoped installs** cannot list nodes, so the optimizer plans by GPU
count alone: it does not check that the GPUs a donor frees are on a node where
the receiver's pod fits, and the two blocked reasons that need node information
(`release-taken`, `release-shape-mismatch`) never appear. Neither does the
transfer outcome `wrong-pod`: only a node-aware plan names the pods that must go,
so a count-planned release is done when the donor shrinks by a replica, whichever
pod left.

With node information, placement counts only nodes a new pod can land on: cordoned
nodes, nodes that are not Ready, nodes with a Kubernetes node-condition taint
(`node.kubernetes.io/...`, such as `unreachable`) and nodes a cluster autoscaler
is removing (a `ToBeDeletedByClusterAutoscaler`, `karpenter.sh/disrupted` or
`karpenter.sh/disruption` taint) are skipped; only `NoSchedule` and `NoExecute`
taints count. Other taints, such
as a GPU pool's own, are not read, because the pods the pool exists for tolerate
them. It does **not** match the receiver's `nodeSelector`, affinity or
tolerations, so a receiver restricted to some nodes can be planned into a hole it
cannot use; that transfer then ends `fill-timeout`. Nor does it read CPU,
memory, hugepages or RDMA devices: a node with the GPUs free but not the rest is
planned into the same way. When a fill times out with the receiver's new pods
still Pending, the receiver's raise is undone (the unscheduled pod goes) and it
is not funded again for one release timeout; kept raised, its Pending pod would
read as idle GPUs and it would be raised, and funded, again every fill timeout.

#### For model owners: what the optimizer may take, and how to protect a model

**What can be taken.** Only replicas above the model's floor (its
`minReplicaCount`, per variant) and above its whole-replica target. While
whole replicas can give every model its need -- each model's need, raised to
its floor and rounded up to its replica size, summed over the WHOLE group,
fits the quota -- only headroom moves, and no donor is taken below its own
need. Large replicas can make that impossible even when the needs add up to
less than the quota, and then the protection is off for the whole group, not
just the models with large replicas:

- a donor of 8-GPU replicas that needs 9 GPUs holds 16, and another model needs
  6 of a 17-GPU quota. Keeping the donor at 16 would hold the other at a sixth
  of its need, so the donor gives a replica and serves one GPU short while the
  other is brought to its need;
- weight decides closer calls too: two P/D models of 8-GPU replicas, needs 9
  and 8.5 of 24, the second at weight 2. The weighted shortfall of the second
  outranks yours, so you can go from 16 GPUs to 8 (11% below your need) to
  bring it from 8 to 16.

Such a model shows `whole-replica-short`. Moves then go to whichever model is
worse off by weight, as they do when the quota is short (`quota-short`):
targets fall below need, and a model can lose a replica it needs if another,
weighted, is worse off. A floor counts in the sum at its full replica size, so
one model's floor can switch the protection off for everyone. A model is never
taken below its need to raise another's headroom, only to bring a model that is
worse off up toward its need, and a move never makes the worse off of the pair
worse. A role that
holds a replica never gives its last one: taking a model to zero is
scale-to-zero's decision, with its own retention, never a transfer's. That last
replica is kept, but it is not reported as a floor: `floor-pinned`,
`floors-exceed-quota`, `wva_utilization_share_floor_excess_gpus` and the
`floors hold more than half the group's budget` log line count only the
`minReplicaCount` you configured.

**How much spike a model absorbs.** Its spike capacity is the headroom it holds
(`wva_utilization_share_headroom`). A spike larger than that waits for a
transfer, and a transfer releases the donor first: expect at least one donor
scale-down window before the new replica serves. For whole-node models the full
wait is the donor's scale-down window, its drain and termination grace, up to
one planning cycle to see the release, the KEDA poll and HPA sync that raise the
receiver, scheduling, and the receiver's model load -- often 3 to 10 minutes for
a large model.

**Whole-node models: the freed node is not reserved.** Between the donor's pod
going and the receiver's pod being created -- a planning cycle, a KEDA poll and
an HPA sync, typically 30 to 75 seconds -- the freed node is free to anyone.
Today's optimizer does not scale models into GPUs promised to a receiver, but a
pod from outside the controller's view (another tenant, a training job) can take
it, and one 1-GPU pod is enough to spoil an 8-GPU hole. The transfer then ends
`fill-timeout` with `release-taken`. Give share-group receivers a
`PriorityClass` above the cluster's other GPU workloads, so the scheduler places
their Pending pod first, and see
[proposal §15](../proposals/utilization-share-optimizer.md#15-deferred-to-later-prs)
for raising the receiver earlier. An idle model is not
shrunk to its floor: it stays at its headroom target, its weighted share of the
spare GPUs, which is what lets it take the next spike at once.

**How to protect a model.** Raise its `minReplicaCount` (a floor is never taken,
but it is paid for by every other model in the group: see `floor-pinned`), or
give it a higher `weightClass`. The optimizer does not read `priority`; only
`weight`/`weightClass` decide shares. On a cluster quota the admin's policy sets
the weights: a model is planned in a cluster group only when its namespace has no
scaling-policy map of its own, so its weight is the one the admin's policy
resolves (the `default` entry, a tier, or an override there). A model whose
namespace has its own map is excluded from the cluster group and stays on
today's optimizer; the controller log lists it in the `frozen` field with
`namespace has its own scaling-policy map; not planned in the cluster-wide group`.

**If your model bursts in turn with another.** A model that just gave a
replica cannot receive one for the reversal hold, about twice a release time
(on the order of 12 minutes with a 300 s scale-down window), so two models
that take turns do not trade GPUs back and forth. The hold is lifted when the
imbalance is hard: your model's load is at least 0.9 of its scale-up
threshold and another model's load, on the GPUs it keeps after giving, is at
most 0.6 of its own (`immediateRebalance` in the policy sets both). That
transfer starts on the first cycle, with no hold and no second confirming
cycle; it still waits for the donor's scale-down window. If no other model is
that lightly loaded, your model waits out the hold and shows
`reversal-hold`. The common case is a model whose burst of long outputs has
just ended: its draining backlog still reads as load, so it is not below 0.6,
and transfers can even go *to* it just as your
burst begins. That is not fixed. If your model must not start a burst short,
set its `minReplicaCount` to the replicas the burst needs: a floor is never
taken.

**P/D models: with long prompts, keep a prefill floor.** Prefill's demand is
read in tokens and grows with input length. When a prefill replica queues on
its own, with decode healthy, its capacity is measured and its need follows its
arrival rate. Otherwise it is priced against its KV memory, and memory is not
what limits prefill: compute throughput is. A prefill finishes fast and hands
its KV to decode, so few tokens are resident at any instant, and the need reads
small even for long prompts. A queue that builds while decode is saturated too
is not learned from, because prefill then waits on decode. And what is learned
does not last: it is kept in the controller's memory only, per band of prompt
length, so a restart, a leader change or a shift in prompt length puts prefill
back on KV-memory pricing until it queues on its own again. In the one P/D
benchmark attempt prefill never queued on its own and never left one replica
under 15 000-token prompts
([why](../well-lit-paths/utilization-share/measured.md#pd-not-measured)).

With short prompts that is right, and prefill is a fair donor. With long
prompts, a prefill whose need reads small also counts as a lightly loaded donor, so its
replicas can be taken at once for another model at its threshold. Keep a
prefill floor until a prefill capacity measure ships
([proposal §15](../proposals/utilization-share-optimizer.md#15-deferred-to-later-prs)).
Size it from what prefill must sustain at peak, over what one prefill replica
sustains:

1. The peak prompt-token rate across the model's prefill pods, over the busiest
   week:

   ```promql
   max_over_time(
     (sum(rate(vllm:prompt_tokens_total{namespace="<namespace>", pod=~"<prefill pods>"}[5m])))[7d:1m]
   )
   ```

2. What one prefill replica sustains -- the same rate summed over the pods of
   ONE replica (an LWS or multi-pod replica reports per pod), measured in a load
   test that saturates prefill while decode is NOT saturated. Read with decode
   saturated, it is decode's admission rate, and too low.
3. Divide, round up, and set that as the prefill variant's `minReplicaCount`.

A floor is never taken, and the rest of the group pays for it: it counts in
the whole-replica sum above at its full replica size, so it can switch off
need protection for every model of the group, and the model shows
`floor-pinned` -- on the model, not the role -- while the floor is a replica or
more above the need the optimizer reads. A floor of one or more also keeps the
model from ever scaling to zero, holding those GPUs off-peak.

**How a replica leaves.** A transfer lowers the donor's target, and the pod goes
through an ordinary scale-down (the ReplicaSet, or LeaderWorkerSet for an LWS).
In-flight requests are protected only by what the model server does on
termination: its `preStop` hook and `terminationGracePeriodSeconds`. Set them to
drain. The grace also lengthens every transfer in the group (it is part of the
release bound).

**Which pod leaves.** On a Deployment the optimizer marks the pod with the lowest
existing `controller.kubernetes.io/pod-deletion-cost`, so a cost you set to
protect a pod is respected. Ties between equal costs go to the most recently
Ready pod -- the coldest cache, and the ReplicaSet's own preference when no cost
is set -- then the name. That is only a preference: the mark's cost decides
which pod the ReplicaSet removes. With node information, the plan first picks
the node whose GPUs it frees (where the receiver's pod fits), and on that node
takes a donor's pods lowest cost first, then the most recently Ready, then by
name: a pod you protected goes last among the donor's pods on that node, not
across nodes. The marked pod's cost is set below every sibling that stays:
`-1000`, or one below the lowest cost you set on such a sibling when that is
lower.

When two transfers in flight take from one donor, each later mark sits above
the earlier live marks and below every unmarked sibling, so the ReplicaSet
removes the earlier transfer's pod first, in the order the transfers release.
When no cost fits strictly between them, or a sibling's cost is the int32
minimum (`-2147483648`) so nothing fits below it, the donor is refused
(`donor-not-steerable`).

A Deployment whose workload is changing is not asked to give: it is held for
one release timeout, as routine -- no back-off, no Warning, no blocked reason --
and its receiver tries another donor. Changing means:

- a pod is not Ready or not yet scheduled: the ReplicaSet removes such pods
  before it reads the cost;
- it is mid-rollout -- its status counts pods not of its current template (a
  new ReplicaSet whose pods do not exist yet included), its controller has not
  yet observed its latest spec, or it has live pods of more than one
  ReplicaSet: the Deployment controller splits a scale-down across them, and a
  cost ranks pods only within one. Pods are found by the Deployment's
  `spec.selector`, so a rollout that changed a pod-template label still shows
  its old ReplicaSet's pods;
- it has fewer pods than `spec.replicas` -- one a quota or an eviction left
  uncreated: lowering the count would remove none.

A paused Deployment stays mid-rollout, and does not give, until it is resumed.
Roll models in a share group with `maxSurge: 0` (and `maxUnavailable` of at
least 1): the group spends its whole quota, so a surge pod finds no GPUs, the
rollout stalls, and a model mid-rollout cannot give to make room for it.

A Deployment is refused as a fault -- it backs off with the blocked reason
`donor-not-steerable` and a `UtilizationShareDonorNotSteerable` Event -- only
when a sibling's cost leaves no room for the mark (above), or the patch fails.

A donor whose every pod is already given to transfers still in flight is held
until one of those releases lands, or for one release timeout, so its receiver
tries another donor. Nothing is wrong with it -- it has given all it can for
now -- so it counts no abort, does not back off, shows no blocked reason, gets
no Event and is not counted in `wva_utilization_share_withheld_total`.

On a LeaderWorkerSet the group that goes is the one LWS removes next; its
deletion cost is not consulted. LWS removes the highest indices first, so that
is the highest group index still below `spec.replicas` that no transfer in
flight has marked: groups at or above `spec.replicas` are already going, and a
marked group is an earlier transfer's and goes first. Groups are taken over the
LWS's own pods (the ones its StatefulSets own), terminating ones included; a pod
that only carries the LWS's labels is ignored. The LWS is held as changing,
like a Deployment, when that group is terminating or not yet scheduled (below
`spec.replicas` that is a rollout, not a release), during a rolling update (its
`UpdateInProgress` condition, or groups of two template revisions at or above
its partition -- a canary held at a partition is no rollout for the groups
below it), or when a group below `spec.replicas` has no pod: the StatefulSet
removes the highest ordinal, existing or not. When every group below
`spec.replicas` is marked, the donor is exhausted, as above.

**Reading the blocked reasons of a P/D model.** `wva_model_scaling_blocked` has
no `role` label, so a reason on a P/D model may come from its decode or its
prefill role. Check `wva_utilization_share_headroom` and
`wva_utilization_share_actionable` by `role` to see which one is short.

**Events are not history.** The `UtilizationShare*` Events on a scale target
explain recent transfers, but the API server deletes Events after its
`--event-ttl`, one hour by default. Use the counters and the controller log for
anything older.

**After a restart or a leader change** only the releases in flight are rebuilt,
from the marks on donor pods. Everything else the optimizer remembers starts
again from zero: reversal holds, swing state and abort back-offs. For one fill
timeout every planned model of the group holds what it runs (blocked reason
`quiet-period`); that quiet period is the only protection across the restart.

**Caveats for operators.**

- **Lowering a quota below what the scaling manager holds bounds growth only.**
  Nothing is evicted to meet the new quota. The group's budget becomes what its
  planned models hold, and spare GPUs and `quota-short` are judged against that;
  the fleet shrinks to the new quota only as models scale down on their own.
- **Free GPUs can be counted by more than one group in a cycle.** Each group
  plans over the quota's free GPUs as its limiter reports them, and so do the
  models left to today's optimizer: the models a `clusterNamespaces` canary
  leaves out, and, under a cluster quota, the other namespace quota groups. Two
  of them can plan into the same free GPUs in one cycle. The overshoot is short
  and the quota limiter bounds it; the canary does not reserve free GPUs for its
  namespaces.

## Configuration

### ConfigMap Structure

The saturation scaling configuration is stored in a ConfigMap named `wva-scaling-policy-config` in llm-scaling-manager controller's namespace.

**Location:** `deploy/configmap-scaling-policy.yaml`

### Parameters

| Parameter | Type | Description | Recommended |
|-----------|------|-------------|-------------|
| `kvCacheThreshold` | float64 | Replica is considered saturated if KV cache utilization ≥ threshold (0.0-1.0) | 0.80 |
| `queueLengthThreshold` | float64 | Replica is considered saturated if queue length ≥ threshold | 5 |
| `scaleUpThreshold` | float64 | Model-level utilization threshold above which scale-up is triggered (0.0-1.0). Applied by the engine post-step to every analyzer's result. | 0.85 |
| `scaleDownBoundary` | float64 | Model-level utilization boundary below which scale-down is safe (0.0-1.0). Applied by the engine post-step to every analyzer's result. | 0.70 |

### V2 Analyzer Parameters

These parameters apply when `analyzerName: "saturation"` is set or when the `analyzers:` list is populated.

| Parameter | Type | Description | Default |
|-----------|------|-------------|---------|
| `analyzerName` | string | Legacy selector, retained for backward compatibility: `"saturation"` names the token-based analyzer, which also runs when the field is empty. Prefer the `analyzers:` list, which the shipped default uses. | `""` |
| `priority` | float64 | Multiplier for this model's scaling urgency in fair-share GPU allocation | 1.0 |
| `analyzers` | list | Multi-analyzer pipeline registration — see [Multi-Analyzer Registration](#multi-analyzer-registration) | `[{name: "saturation", score: 1.0}]` |

> `scaleUpThreshold` and `scaleDownBoundary` are honored only for saturation on this branch; see the `multi-analyzer-threshold` PR for the universal post-step that calibrates RC/SC across all analyzers.

### Expected output length

This one is **not** a ConfigMap field. It is set on the ScaledObject, beside
`modelID`, and a `defaultOutputTokens` key in this ConfigMap is ignored:

```yaml
triggers:
  - type: external-push
    metadata:
      scalerAddress: wva-external-scaler.<wva-namespace>.svc.cluster.local:9090
      modelID: ibm/granite-13b
      defaultOutputTokens: "6000"
```

The scaling manager prices each request waiting in the router queue at the
generation length it expects that request to produce: a queued request owes the
decode role `O` tokens of work, and that is what makes a growing queue worth
ordering capacity for. `O` is the first of these that is a positive number:

| source | what it is |
| --- | --- |
| the fleet's own reading | the output length the running replicas are averaging over their recent completions |
| the fleet's last known shape | the stable shape the throughput keys are built from, which carries the figure across a gap in readings |
| `defaultOutputTokens` | this key |
| the built-in default | 512 tokens |

Set it when the deployment's generations are materially longer than a chat
completion. Until a replica has completed a generation there is no reading to
use — the first cycles after a restart, after a scale from zero, or after the
arriving traffic changes shape — and the built-in 512 under-states a
6000-token workload by a factor of twelve. A queue priced low is a queue that
does not order, and that is paid in time-to-first-token.

It is a seed for that window only. The fleet's own measurement displaces it as
soon as one exists, so a figure of the right order is what matters and an exact
one buys nothing.

A value the scaling manager cannot read as a whole number of tokens is
**rejected at the trigger that carried it**, with an error naming the key and
quoting the value — `"lots"`, `"-1"` and `"6000.5"` are all refused rather than
read as zero, because the only view an operator gets of a bad value is that
error. `"0"` is the one exception: it is accepted and means *unset*, exactly as
leaving the key out does, so the table above falls through to the built-in
default. Prefer omitting the key to writing `"0"` — the two behave identically
and the omission says so.

**Why the trigger and not this ConfigMap.** The figure is a fact about one
workload's traffic, not a threshold a class of workloads shares — a
fleet-wide generation length is already what the built-in 512 is. It sits beside
`replicaStartSeconds`, the other per-workload seed, for the same reason.

**When a model's variants disagree.** A model under P/D has a ScaledObject per
role, so the key can be written twice. The **largest** figure wins, and the
disagreement is logged with every figure that was named. Largest, because only
one role generates tokens: a prefill trigger carrying nothing must not pull
decode's figure down to the built-in fallback, and under-pricing a queue is the
failure the key exists to fix. Writing it on one variant and leaving it off the
others is the simplest way to get this right.

### Default Configuration

Since v0.9.0 the shipped `default` entry selects **V2** (token/capacity-based) via
the `analyzers:` list:

```yaml
analyzers:
  - name: saturation
    score: 1.0
# Scaling band:
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
# Capacity sizing:
kvCacheThreshold: 0.80
queueLengthThreshold: 5
```

Every field has a hardcoded default, so a missing ConfigMap (or a missing
`default` entry) degrades gracefully rather than pinning the analyzer at zero
thresholds. Deploying the ConfigMap with a `default` entry is still recommended
so the values are explicit and reviewable.

## Multi-Analyzer Registration

The V2 engine carries an analyzer registry so external analyzers (e.g.
throughput, SLO) can be plugged in without the engine package knowing the
concrete type. Each cycle the engine iterates every registered analyzer in
registration order and invokes its `Analyze` method.

> **Scope note.** Saturation drives every scaling decision on its own.
> Non-saturation analyzers are registered and invoked, but their results
> are **not yet consumed** — combine semantics and per-analyzer score
> consumption land in a follow-up PR (`multi-analyzer-optimizer`). The
> hooks below are wired so that follow-up can hang its behavior off them
> without further engine changes.

### Registering Analyzers

Pre-registered:

- The V2 saturation analyzer is pre-registered by `NewEngine` under
  `interfaces.SaturationAnalyzerName`. It always runs and drives the
  optimizer.

External analyzers are registered from `cmd/main.go` via:

```go
if err := engine.RegisterAnalyzer(name, analyzer); err != nil {
    // handle: duplicate name or called after StartOptimizeLoop
}
```

`RegisterAnalyzer` appends to the registry and returns an `error` for
two misuse conditions: calling it after `StartOptimizeLoop` or
re-registering an existing name. Callers must check the error. Registration
order defines processing order in the engine loop and should match the
operator's `analyzers:` config order.

`RegisterAnalyzer` must be called before `StartOptimizeLoop`.

### Configuring Analyzers

The `analyzers:` list shapes the configuration for the registered analyzers:

```yaml
analyzerName: saturation
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
analyzers:
  - name: saturation
    score: 1.0
  - name: throughput
    score: 1.0
```

When `analyzers:` is omitted **and** `analyzerName: "saturation"` is set, the list
defaults to `[{name: "saturation", score: 1.0}]`. When both are omitted the
token-based analyzer still runs (see [Analyzer Selection](#analyzer-selection));
the list is simply left empty, which also leaves the scaling band to be defaulted
post-merge.

### AnalyzerScoreConfig Fields

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `name` | string | Analyzer name (must match a `RegisterAnalyzer` call) | required |
| `enabled` | bool | Reserved — placeholder for future combine logic | `true` |
| `score` | float64 | Reserved — placeholder for future combine logic | `1.0` |
| `scaleUpThreshold` | float64 | Per-analyzer override for the scale-up threshold; honored by the engine post-step (see Universal Threshold Post-Step below) | global `scaleUpThreshold` |
| `scaleDownBoundary` | float64 | Per-analyzer override for the scale-down boundary; honored by the engine post-step (see Universal Threshold Post-Step below) | global `scaleDownBoundary` |

### Analyzer responsibilities and the universal threshold post-step

#### Per-variant data is canonical

`interfaces.VariantCapacity` is the single source of truth for per-variant primitives. Analyzers populate it; the engine and optimizer read it.

| Field | Written by | Read by |
|---|---|---|
| `ReplicaCount`, `PendingReplicas`, `PerReplicaCapacity`, `Cost`, `Role`, `AcceleratorName` | Analyzer | Optimizer (per-variant scaling math + picker) |
| `TotalCapacity`, `TotalDemand`, `Utilization` | Analyzer | Sat_v2 internal aggregation; `Utilization` passed through to `VariantDecision.Utilization` for metric emission |
| `r.TotalSupply`, `r.TotalAnticipatedSupply`, `r.TotalDemand` | Analyzer (via shared helpers) | Engine post-step |
| `r.RoleCapacities[role].TotalSupply/TotalAnticipatedSupply/TotalDemand` | Analyzer (via shared helpers) | Engine post-step |
| `r.RequiredCapacity`, `r.SpareCapacity` | **Engine post-step only** | Optimizer |
| `r.RoleCapacities[role].RequiredCapacity/SpareCapacity` | **Engine post-step only** | Optimizer (P/D disaggregation) |

#### Analyzer inputs

`interfaces.AnalyzerInput` carries the shared inputs every analyzer reads:
replica metrics, variant states, the model's resolved config, and
`SchedulerQueue`. None of these are analyzer-specific.

`SchedulerQueue` represents requests queued upstream of any pod (in the
llm-d flow control layer). Queue items are model-scoped and **not yet
attributed to any variant or role**. Any analyzer with a demand model may
use it — sat_v2 does today; the throughput analyzer will when it lands.

**Demand extraction from the queue is per-analyzer.** Each analyzer
converts queue depth/bytes into demand in its own unit (sat_v2:
kv-tokens; throughput: tokens/sec). Each analyzer also decides how to
attribute that demand across roles or variants — sat_v2 splits it among
active roles.

#### Per-replica demand and the role-aware waiting-queue charge

Distinct from `SchedulerQueue` above, each replica also reports its own **local
engine queue** (`vllm:num_requests_waiting` / `sglang:num_queue_reqs`) — requests
already admitted to that pod but not yet generating. sat_v2 charges them to the
replica, so per-replica demand has two terms:

```text
replicaDemand = <resident KV tokens> + QueueLength × <per-request charge>
```

The resident term is `TokensInUse` on the main path, or
`KvCacheUsage × effectiveCapacity` on the fallback path (no
`vllm:cache_config_info`). The per-request charge depends on the variant's P/D
role, because a replica only pays for KV it actually materializes:

| Role | Per-request charge | Rationale |
|------|--------------------|-----------|
| `prefill` | `AvgInputTokens` | Prompt KV only; output is generated on a decode pod |
| `decode` | `AvgInputTokens + AvgOutputTokens` | Holds prompt KV and grows it per generated token |
| `both` (default) | `AvgInputTokens + AvgOutputTokens` | Handles the full request lifecycle |

An absent or empty role canonicalizes to `both`, so **non-disaggregated
deployments take the decode-style charge** — correct for a replica serving the
whole request, but it means the majority of deployments include output tokens.

`AvgInputTokens + AvgOutputTokens` is the footprint at a request's *final* decode
step, chosen as a peak / no-preemption planning charge: KV grows monotonically
once generation starts and cannot be shed without preemption and recompute.

**`I + O` is the saturation analyzer's demand-side unit; `ILeff + O/2` is the
throughput analyzer's.** The two are separate analyzers with separate
demand/supply models, so the difference is by design, not an inconsistency:

| Analyzer | Per-request KV unit | Meaning |
|----------|--------------------|---------|
| Saturation V2 (demand) | `I + O` | Peak footprint — what a replica must be able to hold |
| Throughput (`shape.Shape.KVreq`) | `ILeff + O/2` | Time-averaged residency (`ILeff = I × (1 − PrefixHitRate)`) |

One asymmetry *is* internal to saturation V2 and should not be confused with the
above: its own k2 derivation (`estimateCapacityFromParams`) prices a *concurrent*
request at `I + O/2`. So a queued request is charged more than it occupies on
average, biasing this term toward scale-up — accepted deliberately, since
under-provisioning decode capacity causes preemption thrash. Changing it means
moving the demand/capacity pair together. See the `waitingQueueDemand` doc
comment for the full trade-off.

Note both demand terms derive from 1-minute maxima (`max_over_time` on
`kv_cache_usage_perc` and `num_requests_waiting`), whose peaks need not coincide,
so their sum can exceed any single instant's demand. That is a property of the
collector's queries, not of the role charge.

> **Operational note.** Because the default role is `both`, enabling or upgrading
> into this behavior raises computed demand wherever replicas have a non-empty
> local queue and report token metrics. `wva_saturation_utilization` and
> `wva_required_capacity{unit="continuous"}` step up, `wva_spare_capacity` steps
> down, and `wva_desired_replicas` may take a one-time step.
> `wva_kv_cache_tokens_used` is unaffected — it sums raw `TokensInUse` and does
> not include the queue projection. Utilization-based alerts (such as the sample
> `wva_saturation_utilization > 0.85` in
> [prometheus.md](prometheus.md)) may need re-baselining. The resolved role is
> emitted per variant on the `analyzer-result` log line.

#### Linearity invariant

The optimizer's per-variant scaling math (`bottleneckReplicas`, `safeRemovalReplicas`, `applyAllocation`) assumes that `n` replicas of variant `v` reduce model-level RC by exactly `n × PRC[v]`. That means `Total*` must equal the canonical sum over variants:

```text
r.TotalSupply            == Σ_v vc.ReplicaCount × vc.PerReplicaCapacity
r.TotalAnticipatedSupply == Σ_v (vc.ReplicaCount + vc.PendingReplicas) × vc.PerReplicaCapacity
r.TotalDemand            == Σ_v vc.TotalDemand
r.RoleCapacities[role].* == same sums filtered by vc.Role == role
```

Use the shared helpers in `internal/engines/aggregation/` to compute these. An analyzer that doesn't use the helpers takes responsibility for producing identical math — otherwise the optimizer's per-variant allocation silently breaks.

#### Shared aggregation helpers

```go
import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"

r.TotalSupply            = aggregation.SumTotalSupply(variantCapacities)
r.TotalAnticipatedSupply = aggregation.SumTotalAnticipatedSupply(variantCapacities)
r.TotalDemand            = aggregation.SumTotalDemand(variantCapacities)

// For P/D disaggregated models:
totals := aggregation.AggregateByRole(variantCapacities)
// → map[role]aggregation.ScopeTotals{TotalSupply, TotalAnticipatedSupply, TotalDemand}
```

`AggregateByRole` canonicalizes empty role to `interfaces.RoleBoth`.

#### Engine post-step formula

After each analyzer's `Analyze()` returns, the engine applies the universal threshold formula at **every scope** — model level and each `RoleCapacity` entry:

```text
RC = max(0, TotalDemand / scaleUpThreshold − TotalAnticipatedSupply)
SC = max(0, TotalSupply  − TotalDemand / scaleDownBoundary)
```

`TotalAnticipatedSupply` is read as-is — **zero is a literal value, not a sentinel**. For a model scaled to zero with positive demand, RC = TotalDemand/scaleUp (the correct "this much capacity needed" answer). Analyzers must populate `TotalAnticipatedSupply` via `SumTotalAnticipatedSupply`; the engine does not walk `VariantCapacities` as a fallback.

The asymmetry — anticipated supply for scale-up, steady-state `TotalSupply` for scale-down — preserves the conservative "don't double-scale while replicas are launching, don't count pending as removable" stance.

**P/D disaggregation.** The same formula and the same `(scaleUp, scaleDown)` values are applied to every `RoleCapacity` entry. Threshold configuration is common regardless of role — there are no per-role overrides. The optimizer receives fully calibrated per-role RC/SC and uses them for P/D replica allocation.

**Per-analyzer threshold overrides.** `analyzers[].scaleUpThreshold` / `analyzers[].scaleDownBoundary` are resolved per analyzer: a per-entry override takes precedence over the model-level global. The resolved pair is applied uniformly at model level and every role for that analyzer. An opt-out flag for analyzers with non-universal calibration math is deferred to a follow-up.

### Saturation Always Runs

The saturation analyzer executes on every cycle regardless of any future
`enabled` flag — its `VariantCapacities` carry `Cost` and `AcceleratorName`
required by the optimizer for variant selection and GPU accounting. On
this branch saturation's `RequiredCapacity` and `SpareCapacity` are the
only signals the optimizer consumes.

### Resilience

Each registered non-saturation analyzer runs in isolation: errors are
logged and discarded, and panics are recovered. A faulty plugin analyzer
cannot take down the optimize goroutine or block saturation from driving
the scaling decision.

## Best Practices: Coordinating with InferenceScheduler (End Point Picker)

### What is End Point Picker (EPP)?

The **End Point Picker (EPP)** is an intelligent request routing component in the InferenceScheduler that selects the optimal inference server replica to handle each incoming request. EPP monitors replica capacity metrics (KV cache utilization, queue depth), as well as other replica metrics and uses scoring algorithms to route requests to replicas.

### Deployment Architecture

**EPP Deployment Model**: Each model has a **1-to-1 relationship** with its EPP instance. Every model served by the inference infrastructure has a dedicated EPP component that routes requests specifically to that model's replicas.

**Example deployment pattern:**
- Model: `Qwen/Qwen3-0.6B` in namespace `<your-namespace>` → Dedicated EPP instance `gaie-workload-autoscaler-epp`
- Model: `ibm/granite-13b` in namespace `production` → Dedicated EPP instance `gaie-production-epp`
- Each model deployment has its own EPP instance (naming follows namespace/workload convention)

This 1-to-1 architecture means that saturation detection and request routing decisions are **model-specific**, with each EPP instance monitoring only its associated model's replicas.

### Threshold Alignment Recommendation

**For optimal cluster performance, we strongly recommend using the same threshold values for both the scaling manager (llm-scaling-manager) and InferenceScheduler (End Point Picker) for each model deployment.**

Using aligned thresholds ensures consistent capacity management across the cluster and prevents request drop situations.

**Why threshold alignment matters:**

1. **Reduced Request Drop Rates**: When the scaling manager and EPP use the same saturation thresholds, the scheduler will avoid routing requests to replicas that the scaling manager already considers saturated. This prevents the scheduler from overloading replicas that are about to trigger scale-up.

2. **Consistent Capacity Assessment**: Both components evaluate replica capacity using the same criteria (KV cache utilization and queue length), ensuring coordinated behavior across the entire inference stack.

3. **Improved GPU Utilization**: Aligned thresholds allow the cluster to maintain optimal GPU utilization without oversaturation. The scheduler respects the same capacity boundaries that drive autoscaling decisions.

4. **Faster Response to Load Changes**: When both components agree on saturation thresholds, the system responds more quickly to load changes with coordinated routing and scaling actions.

### Configuration Comparison

#### llm-scaling-manager Saturation Scaling Configuration

```yaml
# llm-scaling-manager Configuration (wva-scaling-policy-config ConfigMap)
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    analyzers:                    # selects V2 (default since v0.9.0)
      - name: saturation
        score: 1.0
    kvCacheThreshold: 0.80        # Should match EPP kvCacheUtilThreshold
    queueLengthThreshold: 5       # Should match EPP queueDepthThreshold
```

#### EPP Saturation Detector Configuration

The InferenceScheduler EPP component uses the [gateway-api-inference-extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/site-src/guides/epp-configuration/config-text.md) saturation detector to identify cluster overload.

**Per-Model Configuration**: Since each model has its own dedicated EPP instance, saturation detection is configured **per model deployment**. This allows different models to have different saturation thresholds based on their specific characteristics and SLO requirements.

```yaml
# EPP Saturation Detector Configuration (per-model EPP instance)
saturationDetector:
  ...
  queueDepthThreshold: 5          # Default: 5 - Backend waiting queue size threshold
  kvCacheUtilThreshold: 0.8       # Default: 0.8 - KV cache utilization threshold (0.0-1.0)
  ...
```

**Configuration Notes**:
- All parameters are optional; omitting them applies the documented defaults
- EPP configuration is **read only on startup** - changes require EPP pod restart
- Unlike the scaling manager, EPP does not currently support live ConfigMap updates
- **Each EPP instance** (one per model) can have different threshold values

### Parameter Mapping and Alignment

| Concept | llm-scaling-manager Field | EPP Field | Aligned Default | Description |
|---------|-----------|-----------|-----------------|-------------|
| **KV Cache Saturation** | `kvCacheThreshold` | `kvCacheUtilThreshold` | **0.80** (80%) | Replica is saturated when KV cache ≥ threshold |
| **Queue Saturation** | `queueLengthThreshold` | `queueDepthThreshold` | **5** | Replica is saturated when queue length ≥ threshold |
| **Scale-Up Trigger (KV)** | **Scale-Up Trigger (Queue)** 
### Configuration Workflow

#### Step 1: Define Thresholds

Choose thresholds based on your workload characteristics and SLO requirements:

| Workload Type | kvCacheThreshold | queueLengthThreshold | Rationale |
|---------------|------------------|----------------------|-----------|
| **Conservative** (Default) | 0.80 | 5 | Balanced performance and utilization |
| **Aggressive** (High GPU utilization) | 0.90 | 15 | Maximize GPU usage, higher latency variance |
| **Strict** (Low latency SLO) | 0.70 | 3 | Prioritize responsiveness, lower utilization |

#### Step 2: Apply to the scaling manager

Update `wva-scaling-policy-config` ConfigMap:

```bash
kubectl edit cm wva-scaling-policy-config -n <workload-variant-autoscaler-namespace>
```

Changes take effect **immediately** (the scaling manager watches ConfigMap and auto-reloads).

#### Step 3: Apply to EPP

**Important**: Since each model has its own dedicated EPP instance (1-to-1 relationship), you must configure the EPP instance for **each specific model deployment** separately.

**Current approach:**

1. Identify the EPP instance for your target model:
   ```bash
   # Example: Find EPP deployment for a specific model in namespace
   kubectl get deployments -n <your-namespace> | grep epp
   ```

2. Update the EPP instance's environment variables or configuration file for that specific model

3. Restart the EPP pod for that model:
   ```bash
   # Restart the specific model's EPP instance
   kubectl rollout restart deployment/gaie-<model-name>-epp -n <namespace>
   ```

**Example for multiple models:**
```bash
# Model 1: granite-13b in production
kubectl rollout restart deployment/gaie-granite-13b-epp -n production

# Model 2: llama-70b in lab
kubectl rollout restart deployment/gaie-llama-70b-epp -n lab
```

#### Step 4: Verify Configuration

**the scaling manager verification:**
```bash
kubectl get cm wva-scaling-policy-config -n <workload-variant-autoscaler-namespace> -o yaml
```

**EPP verification (per-model instance):**
```bash
# Check specific model's EPP pod logs for loaded configuration
kubectl logs -n <namespace> deployment/gaie-<model-name>-epp --all-containers | grep -i "saturation\|threshold"

# Example: Verify EPP configuration for granite-13b model in production
kubectl logs -n production deployment/gaie-granite-13b-epp --all-containers | grep -i "saturation\|threshold"
```

### Alignment Best Practices

1. **Core Thresholds Must Match Per Model**:
   - `kvCacheThreshold` (the scaling manager) = `kvCacheUtilThreshold` (EPP)
   - `queueLengthThreshold` (the scaling manager) = `queueDepthThreshold` (EPP)
   - **Important**: Since each model has its own EPP instance, ensure thresholds align for **each model deployment** individually

2. **Per-Model Configuration Strategy**:
   - Use the scaling manager's per-model override feature to set model-specific thresholds
   - Configure the corresponding EPP instance with matching thresholds
   - Document the threshold mapping for each model deployment
   - Example: If `ibm/granite-13b` uses `kvCacheThreshold: 0.85` in the scaling manager, its dedicated EPP must use `kvCacheUtilThreshold: 0.85`


4. **Testing Threshold Changes**:
   - Test in development environment first
   - Monitor impact on request drop rate and latency for the specific model
   - Adjust based on observed behavior
   - Remember to update both the scaling manager and the model's EPP instance

## Usage

### 1. Using Default Configuration

Deploying without a ConfigMap is safe — the analyzer falls back to its hardcoded
defaults. Shipping a `default` entry is still recommended so the thresholds are
explicit and reviewable.

If the ConfigMap is missing, the system will log a warning:
```text
INFO No cluster policy ConfigMap in the policy namespace
```

### 2. Customizing Global Defaults

Edit `deploy/configmap-scaling-policy.yaml`. Keeping the `analyzers:` section
is recommended: it states the analyzer and its score explicitly, and it makes the
entry V2-shaped so the scaling band is defaulted on the entry itself (see
[Analyzer Selection](#analyzer-selection)):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    analyzers:
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85      # scaling band
    scaleDownBoundary: 0.70     # scaling band
    kvCacheThreshold: 0.75      # capacity sizing
    queueLengthThreshold: 10    # capacity sizing
```

Apply the ConfigMap:
```bash
kubectl apply -f deploy/configmap-scaling-policy.yaml
```

**Note:** Changes take effect immediately! The controller watches the ConfigMap and automatically:
1. Reloads the cache when changes are detected
2. Triggers a reconcile of every discovered variant
3. Applies the new configuration without requiring pod restart

### 3. Named Policy Tiers

A tier is a reusable entry — `interactive`, `standard`, `batch` — that a variant
selects **by name** from its ScaledObject. A tier carries no model identity, which
is the whole point: one tier serves many models, and retuning the tier retunes all
of them at once. Per-model entries cannot express that, because they bind settings
to identity, so a fleet-wide change becomes an edit per model.

An entry is a tier when its key is not `default` and its body sets no `model_id`
(`PolicyEntryKey`).

```yaml
data:
  default: |
    analyzers:
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    priority: 1.0
    defaultPolicy: standard      # tier applied to variants that name none

  interactive: |
    priority: 5.0
    scaleUpThreshold: 0.75

  batch: |
    priority: 0.5
```

A variant selects its tier through KEDA trigger metadata — the trigger is already
the registration, so this needs nothing watched, listed, or matched:

```yaml
triggers:
  - type: external-push
    metadata:
      scalerAddress: wva-external-scaler.<wva-namespace>.svc.cluster.local:9090
      modelID: ibm/granite-13b
      scalingPolicy: interactive
```

**Resolution order** — most specific wins (`ResolveScalingPolicyForTier`):

| layer | source | scope |
| --- | --- | --- |
| `default` entry | the ConfigMap's `default` key | fleet-wide fallback |
| named tier | the variant's `scalingPolicy`, else `default.defaultPolicy` | a class of workloads |
| per-model override | an entry whose body names this model | one model in one namespace |
| trigger metadata | per-workload seeds on the ScaledObject, e.g. `defaultOutputTokens` | one workload |

`defaultPolicy` is read from the `default` entry only: a fallback chosen by a tier
or by a per-model entry would be that entry choosing for everyone but itself.

Two failure modes are resolved deterministically rather than silently, and both
are reported:

- **Unknown tier** — a `scalingPolicy` naming no entry falls back to `default`.
  That is the right outcome and the wrong silence, so it is logged against the
  variants that asked for it, listing the tiers that do exist.
- **Disagreeing variants** — when variants of one model name different tiers, the
  lexicographically smallest wins, so the allocation resolves the same way on
  every cycle and every replica rather than flipping with map order.

### 4. Per-Model Overrides

Add model-specific configuration entries to override defaults for specific model/namespace pairs.

The saturation engine resolves per-model config using a lookup key in the format
an entry whose body names the model (see `internal/config/scaling_policy.go` — `ResolveScalingPolicy()`).
The ConfigMap data key **must** match this format for overrides to take effect.
Lookup order: `modelID#namespace` → `default` → zero-value with defaults applied.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    analyzers:              # selects V2 (default since v0.9.0)
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5

  # Override for granite model in production namespace. Overrides inherit the
  # default's analyzer selection (V2) via field-level merge, so they only list the
  # thresholds they change.
  ibm_granite-13b.production: |
    model_id: ibm/granite-13b
    namespace: production
    kvCacheThreshold: 0.85

  # Override for llama model in lab namespace
  meta_llama-70b.lab: |
    model_id: meta/llama-70b
    namespace: lab
    kvCacheThreshold: 0.80
    queueLengthThreshold: 20
```

> **The `model_id` and `namespace` lines are not decoration — they are what makes
> this an override.** An entry whose body names no model is registered as a named
> policy *tier* instead (`PolicyEntryKey` in `internal/config/scaling_policy.go`),
> so it will never apply to the model you meant and will silently be selectable by
> any variant that names its key.
>
> Nor can identity live in the key: a ConfigMap data key admits only
> `[-._a-zA-Z0-9]`, so the `{modelID}#{namespace}` form earlier versions of this
> document showed — `"ibm/granite-13b#production"` — is rejected by the API server
> and could never be applied. `ModelOverrideKey()` suggests a legal, readable key.

**Key points:**
- An override entry is identified by its **body**: set `model_id` and `namespace`. Its ConfigMap key is arbitrary — pick something readable
- The `model_id` and `namespace` YAML fields inside the entry are parsed but **not used for lookup**
- Overrides use **field-level merge**: only non-zero fields in the override replace the corresponding values from `default`; any field you omit (or set to its zero value) inherits from `default`. See `Merge()` in `internal/config/saturation_scaling.go`.
- Multiple overrides can exist for different model/namespace combinations

### 5. Per-Model Overrides — Field-Level Inheritance

Overrides are merged onto `default` field by field: only non-zero fields in the override
replace values from `default`, and any field you omit inherits from `default`. To change
just one threshold, specify only that field:

```yaml
  # Inherits queueLengthThreshold from `default`
  my-org_my-model.my-namespace: |
    model_id: my-org/my-model
    namespace: my-namespace
    kvCacheThreshold: 0.90
```

> **Gotcha:** Because `Merge()` only overlays non-zero values, an override cannot force
> a field to its zero value — the override is treated as "unset" and inherits from
> `default` instead. This affects every field type, not just numeric thresholds:
>
> - `analyzers: []` → inherits from `default` (cannot clear a non-empty analyzer list)
> - `enableRescale: false` → inherits from `default` (cannot disable rescale per model)
>
> See `Merge()` in `internal/config/saturation_scaling.go`.

## Validation

The controller validates all configuration entries on load. Invalid entries are logged and skipped:

### Validation Rules

1. **KvCacheThreshold:** Must be between 0.0 and 1.0
2. **QueueLengthThreshold:** Must be ≥ 0
6. **Priority:** Must be ≥ 0
7. **V2 thresholds** (when set): `scaleUpThreshold` and `scaleDownBoundary` must be in (0, 1],
   and `scaleUpThreshold` must be > `scaleDownBoundary`. Per-analyzer overrides are range-checked too.
8. **Limiters:** each `type` must be `gpu-inventory`/`inventory` or `quota`; a `gpu-inventory` entry
   must carry no quota fields; `quota` entries are validated end-to-end against the `QuotaLimiterConfig`
   schema (name uniqueness, scope, per-type ranges).

> Analyzer `type`/`name` values are **not** restricted to a fixed set — an unrecognized analyzer is
> simply ignored at runtime, so externally-registered analyzers and newer built-ins do not fail validation.

### Example Validation Errors

**Invalid entry (logged and skipped):**
```yaml
  invalid-config: |
    model_id: test/model
    namespace: test
    kvCacheThreshold: 1.5  # ERROR: Must be ≤ 1.0
```

**Log output:**
```text
WARN Invalid saturation scaling config entry, skipping key=invalid-config error=kvCacheThreshold must be between 0 and 1, got 1.50
```

## Integration with Controller

### Caching Architecture

The controller uses an **efficient caching mechanism** with ConfigMap watch for optimal performance:

**Initialization (on controller startup):**

The ConfigMap reconciler watches the `wva-scaling-policy-config` ConfigMap and loads
configuration into the shared `Config` object. On startup, the controller bootstraps the
config cache (see `internal/controller/configmap_bootstrap.go`).

**Reconciliation (zero API calls):**

During the optimization loop, the engine reads config from the in-memory cache:
```go
// In optimize() - reads cached config (no API call)
saturationConfigMap := e.Config.SaturationConfigForNamespace(namespace)

// Resolve per-model config by matching model_id + namespace in each entry
scalingPolicy := config.ResolveScalingPolicy(scalingPolicyConfigMap, modelID, namespace)

// Use saturationConfig for saturation-based scaling decisions
// (thresholds drive the analyzer's saturation detection)
```

### Automatic Cache Updates

The `ConfigMapReconciler` watches the `wva-scaling-policy-config` ConfigMap for changes
(see `internal/controller/configmap_reconciler.go`):

1. **ConfigMap change detected** → Watch event triggered
2. **Cache automatically reloaded** → New configuration parsed and stored in `Config`
3. **Next optimization cycle** picks up the new config automatically

### Performance Characteristics

| Operation | Before (Without Cache) | After (With Cache) |
|-----------|------------------------|-------------------|
| Startup | N/A | Single ConfigMap read |
| Per Reconciliation | ConfigMap API call | Memory read only |
| Config Change | Manual pod restart needed | Automatic reload + reconcile |
| Latency Impact | Network round-trip per reconcile | Zero (memory access) |
| Concurrency | Serial API calls | Thread-safe concurrent reads |

**Cache benefits:**
- ✅ **Single read on startup** instead of per-reconciliation
- ✅ **Zero API calls during reconciliation** (cached access)
- ✅ **Event-driven updates** (immediate response to changes)
- ✅ **Thread-safe concurrent access** (RWMutex)
- ✅ **Defensive copying** prevents external modification

## Troubleshooting

### ConfigMap Not Found

**Symptom:** Warning log message
```text
INFO No cluster policy ConfigMap in the policy namespace: no limiters or quotas are in force
```

**Solution:** Deploy the ConfigMap:
```bash
kubectl apply -f deploy/configmap-scaling-policy.yaml
```

### Invalid Configuration Entry

**Symptom:** Warning log message
```text
WARN Invalid saturation scaling config entry, skipping key=my-config error=...
```

**Solution:** Fix the validation error in the ConfigMap entry and reapply.

### Missing Default Entry

**Symptom:** Warning log message
```text
WARN No 'default' entry in saturation scaling ConfigMap, using hardcoded defaults
```

**Solution:** Add a `default` entry to the ConfigMap (keeping the `analyzers:`
section states the analyzer explicitly):
```yaml
data:
  default: |
    analyzers:
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
```

### Override Not Applied

**Symptom:** Model-specific override is not being used

```yaml
# A per-model override. The key is yours to choose; the body binds it to a model.
data:
  granite-in-prod: |
    model_id: "ibm/granite-13b"
    namespace: "production"
    scaleUpThreshold: 0.75
    scaleDownBoundary: 0.60
```

**Checklist:**
1. Verify the entry sets `model_id` and `namespace` in its **body** (the key is arbitrary). A key like `"ibm/granite-13b#production"` cannot exist — Kubernetes allows only `[-._a-zA-Z0-9]` in a ConfigMap key, which excludes both `/` and `#`
2. Verify `modelID` exactly matches the model the variant serves
3. Verify `namespace` exactly matches the variant's namespace
4. Check controller logs for validation errors
5. Ensure entry passed validation (check for WARN logs)

### Config Changes Not Taking Effect

**Symptom:** Updated ConfigMap but controller still uses old values

**Solution:** The controller watches for ConfigMap changes and automatically reloads. Check:

1. **Verify ConfigMap was updated:**
   ```bash
   kubectl get cm wva-scaling-policy-config -n <workload-variant-autoscaler-namespace> -o yaml
   ```

2. **Check controller logs for reload confirmation:**
   ```bash
   kubectl logs -n <workload-variant-autoscaler-namespace> deploy/wva-controller-manager | grep -E "scaling policy|saturation config"
   ```

   Expected logs:
   ```text
   INFO  Updated global scaling policy from ConfigMap  entries=2
   INFO  Effective scaling policy  namespace=... modelID=... scalingPolicy="(default entry)"
   ```

3. **If no logs appear, verify watch is working:**
   - Check controller pod is running: `kubectl get pods -n <workload-variant-autoscaler-namespace>`
   - Check for errors: `kubectl logs -n <workload-variant-autoscaler-namespace> deploy/wva-controller-manager --tail=100`

4. **Manual restart (last resort):**
   ```bash
   kubectl rollout restart deploy/wva-controller-manager -n <workload-variant-autoscaler-namespace>
   ```

### Cache Initialization Failed

**Symptom:** Warning on controller startup
```text
WARN Failed to load initial saturation scaling config, will use defaults
```

**Solution:** This is non-fatal. The controller continues with the analyzer's hardcoded defaults. To fix:

1. Deploy the ConfigMap:
   ```bash
   kubectl apply -f deploy/configmap-scaling-policy.yaml
   ```

2. The watch mechanism will automatically reload the cache once ConfigMap is available

3. Verify cache loaded:
   ```bash
   kubectl logs -n <workload-variant-autoscaler-namespace> deploy/wva-controller-manager | grep "Updated global scaling policy from ConfigMap"
   ```

## Example: Production Setup

**deploy/configmap-scaling-policy.yaml:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  # Conservative defaults for most workloads (V2 analyzer — the default since v0.9.0)
  default: |
    analyzers:
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5

  # High-priority production workload - scale aggressively.
  # Per-model overrides inherit the default's analyzer selection (V2 here) via
  # field-level merge, so they need only the thresholds they change. model_id and
  # namespace are what bind the entry to a model; the key is only a label.
  ibm_granite-13b.production: |
    model_id: ibm/granite-13b
    namespace: production
    kvCacheThreshold: 0.70
    queueLengthThreshold: 3

  # Development workload - allow higher saturation
  meta_llama-70b.development: |
    model_id: meta/llama-70b
    namespace: development
    kvCacheThreshold: 0.90
    queueLengthThreshold: 15
```

Apply the configuration:
```bash
kubectl apply -f deploy/configmap-scaling-policy.yaml
```

Verify deployment:
```bash
kubectl get cm wva-scaling-policy-config -n <workload-variant-autoscaler-namespace>
kubectl describe cm wva-scaling-policy-config -n <workload-variant-autoscaler-namespace>
```

## API Reference

### Go Structs

**SaturationScalingConfig** (defined in `internal/config/saturation_scaling.go`):
```go
type SaturationScalingConfig struct {
    ModelID              string                `yaml:"model_id,omitempty"`
    Namespace            string                `yaml:"namespace,omitempty"`
    KvCacheThreshold     float64               `yaml:"kvCacheThreshold"`
    QueueLengthThreshold float64               `yaml:"queueLengthThreshold"`
    EnableRescale        bool                  `yaml:"enableRescale,omitempty"`
    AnalyzerName         string                `yaml:"analyzerName,omitempty"`
    ScaleUpThreshold     float64               `yaml:"scaleUpThreshold,omitempty"`   // default 0.85
    ScaleDownBoundary    float64               `yaml:"scaleDownBoundary,omitempty"`  // default 0.70
    Priority             float64               `yaml:"priority,omitempty"`           // default 1.0
    Analyzers            []AnalyzerScoreConfig `yaml:"analyzers,omitempty"`
    ScaleToZero          *ScaleToZeroEnvelope  `yaml:"scaleToZero,omitempty"`        // Phase 1: inline scale-to-zero
    ScaleFromZero        *ScaleFromZeroEnvelope `yaml:"scaleFromZero,omitempty"`     // Phase 1: inline scale-from-zero
    Limiters             []QuotaLimiterConfig  `yaml:"limiters,omitempty"`           // Phase 1: cluster-default-only GPU limiters
}

// ScaleFromZeroEnvelope tunes how a model is woken from zero replicas.
type ScaleFromZeroEnvelope struct {
    RequirePrefill *bool `yaml:"requirePrefill,omitempty"` // default false: decode alone may be woken
}

// AnalyzerScoreConfig configures one analyzer in the multi-analyzer pipeline.
// Type/Parameters implement the Phase 1 plugin envelope; well-known parameter
// keys are folded into the typed fields by Normalize(). Type falls back to Name.
type AnalyzerScoreConfig struct {
    Type              string         `yaml:"type,omitempty"`              // plugin type; falls back to Name
    Name              string         `yaml:"name"`
    Enabled           *bool          `yaml:"enabled,omitempty"`           // default true
    Score             float64        `yaml:"score,omitempty"`             // default 1.0
    ScaleUpThreshold  *float64       `yaml:"scaleUpThreshold,omitempty"`  // overrides global (saturation only today)
    ScaleDownBoundary *float64       `yaml:"scaleDownBoundary,omitempty"` // overrides global (saturation only today)
    Parameters        map[string]any `yaml:"parameters,omitempty"`        // plugin params; folded by Normalize
}

// ScaleToZeroEnvelope is the inline scale-to-zero setting on an entry.
type ScaleToZeroEnvelope struct {
    Enabled         *bool  `yaml:"enabled,omitempty"`         // nil = inherit, ending at WVA_SCALE_TO_ZERO
    RetentionPeriod string `yaml:"retentionPeriod,omitempty"` // "" = inherit, ending at 10m
}
```

`Limiters` reuses `QuotaLimiterConfig` (`internal/config/quota_limiter.go`); a
`gpu-inventory` entry carries no quota fields, a `quota` entry uses the full quota
schema. See [ScalingPolicy Schema (Phase 1)](#scalingpolicy-schema-phase-1).

**Methods:**
- `Normalize() error` - Folds each analyzer's `parameters` into the typed fields; call at parse time before `ApplyDefaults()`/`Validate()`
- `ApplyDefaults()` - Fills in zero-valued fields with their defaults and seeds the `Analyzers` list when empty; the scaling band is defaulted only for V2-shaped entries, so a partial override cannot clobber a tuned global during `Merge()`
- `Validate() error` - Validates configuration values (thresholds in range, consistency checks, per-analyzer overrides, analyzer/limiter types)
- `EffectiveType()` (on `AnalyzerScoreConfig`) - Returns `Type`, or `Name` when `Type` is empty

## Architecture Notes

### Caching Implementation Details

The caching mechanism uses the following components:

**Thread Safety:**
- Uses `sync.RWMutex` for concurrent access control
- Multiple reconciliation loops can read cache simultaneously
- Write operations (cache reload) are exclusive

**Defensive Copy:**
- `SaturationConfigForNamespace()` returns a deep copy
- Prevents external code from modifying cached configuration
- Each caller gets an independent copy

**Watch Mechanism:**
- Kubernetes watch on `wva-scaling-policy-config` ConfigMap
- Predicate filters to only relevant ConfigMap events
- Event handler reloads cache and triggers reconciliation

**Graceful Degradation:**
- Controller starts successfully even if ConfigMap missing
- The analyzer uses hardcoded defaults (`scaleUpThreshold: 0.85`, `scaleDownBoundary: 0.70`)
- Automatically loads config once ConfigMap becomes available

