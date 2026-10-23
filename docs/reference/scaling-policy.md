# Saturation Scaling Configuration

## Overview

llm-scaling-manager scales on saturation, from KV cache utilization and queue
length. It is on by default and configured in a ConfigMap:

- global defaults, plus per-model overrides;
- read once at startup and cached, so reconciling makes no API calls;
- reloaded on change through a ConfigMap watch;
- thread-safe (RWMutex);
- a missing ConfigMap degrades gracefully to hardcoded defaults (see
  [Default Configuration](#default-configuration)).

## Analyzer Selection

There is one saturation analyzer, the token/capacity-based one. The V1
percentage/spare-capacity analyzer was removed, so `analyzerName` and the
`analyzers:` list no longer choose between engines (see the note on
`analyzerName` in `internal/config/saturation_scaling.go`).

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
adds a plugin envelope to each entry of this ConfigMap. It is additive: no CRD,
no migration, no new install step. Flat entries keep working; adopt it per entry
if you want.

### Analyzer plugin envelope (`type` / `parameters`)

Each `analyzers:` entry accepts a `type` and a free-form `parameters` map:

```yaml
analyzers:
  - type: saturation
    parameters: { scaleUpThreshold: 0.95 }
```

- `type` picks the analyzer plugin (`saturation`, `queueing-model`, `throughput`).
  Omitted, it falls back to `name`, so the legacy `- name: saturation` form still
  works as `type: saturation`.
- `parameters` holds plugin settings. The keys `scaleUpThreshold`,
  `scaleDownBoundary`, `score` and `enabled` equal the matching typed fields; an
  explicit typed field wins. Unknown keys are tolerated (forward-compatible); a
  known key with the wrong type fails validation and the entry is skipped.

There is nothing to opt out to: the token/capacity-based analyzer runs whether
or not `analyzers:` is set (see [Analyzer Selection](#analyzer-selection)).

### `scaleToZero`

An entry holds the whole scale-to-zero policy; it is the only per-model place to
set it:

```yaml
scaleToZero:
  enabled: false
  retentionPeriod: 5m
```

| field | absent means | ends at |
| --- | --- | --- |
| `enabled` | inherit | the `WVA_SCALE_TO_ZERO` deployment flag |
| `retentionPeriod` | inherit | 10m (`DefaultScaleToZeroRetentionPeriod`) |

You can set it in `default`, in a named tier, or in a per-model entry. When a
model's variants name different tiers, the model parks only if every one of
those tiers allows it. It then waits for the longest `retentionPeriod` among
them. A model parks whole, so that longest retention holds the GPUs of every
role, prefill and decode alike.

A variant that names no tier follows the variants of its model that do. Tag
only the decode of a P/D model with `pd-gpu`, and the untagged prefill parks,
scales and wakes under `pd-gpu` too, not under `default`. `default` applies only
when no variant of the model names a tier.

**A bad value is skipped, not fatal.** This covers these fields:

- `scaleToZero.retentionPeriod` that is not a duration (for example
  `5 minutes`) or is shorter than 30s (for example `0s` or `10s`). A retention
  that short would park a model between two requests and wake it on the next.
- `shapeChangeHoldSeconds` below zero.
- A number that is not finite (YAML `.inf`, `-.inf` or `.nan`) in `priority`,
  `kvCacheThreshold`, `queueLengthThreshold`, `scaleUpThreshold`,
  `scaleDownBoundary`, or an analyzer's `score`, `scaleUpThreshold` or
  `scaleDownBoundary`. Do not use `.inf` to give a model precedence:
  `priority` multiplies the model's weight in the fair share (priority x
  demand), so a finite value such as 5 weighs it five times as heavily.
- An analyzer `score` below zero. It is read as 1, as before; the change is
  that it is now reported.
- A `priority` or analyzer `score` above 1000000. A weight a million times
  another model's already takes the whole contended budget; a larger value
  only risks overflowing the fair-share arithmetic that every model in the GPU
  budget shares.

The scaling manager acts as if the entry did not set the field. The model then
gets the value of the layer below: the tier, then `default`, then the built-in
value (10m for the retention). Everything else in the entry still applies.

An analyzer's fields are the exception. The `analyzers` list is never merged
with the layer below; an entry that sets it replaces it whole. So a skipped
analyzer `score` is 1, and a skipped analyzer `scaleUpThreshold` or
`scaleDownBoundary` uses the same policy's own band.

You can see a skipped value in these places:

- a `ConfigFieldIgnored` Warning event on the ConfigMap;
- `wva_config_ignored_fields` for the ConfigMap, which returns to 0 once fixed;
- the controller log, each time the ConfigMap is read. The line starts with
  `WARNING: Ignored unusable saturation scaling config field` and names the
  namespace, ConfigMap, entry and field;
- `wva_errors_total{error_type="Ignored unusable saturation scaling config field"}`;
- the `policyIgnored` key of the
  [controller-status ConfigMap](operations.md#what-the-controller-is-running-with).
  This key shows only the `default` entry of the controller's own ConfigMap.

<details>
<summary>Details</summary>

- Values are cut to 64 bytes in every report. At most 10 fields are listed per
  read, in the log and as events; one more line counts the rest. The metrics
  count every field.
- `policyIgnored` starts with the ConfigMap it describes,
  `<namespace>/<name>: `, because the `policy*` keys beside it may name the
  policy namespace's ConfigMap instead.
- `policyIgnored` describes the `default` entry as parsed in the latest read.
  If that entry is rejected and the previous one stays in force, it is empty;
  `policyAccepted` and `policyError` say why.
- With a separate policy namespace, the ConfigMap there supplies only limiters
  and the optimizer. A `retentionPeriod` in it has no effect, and a bad one
  there is not reported.

</details>

#### How long until a model actually reaches zero

**`retentionPeriod` and KEDA's `cooldownPeriod` add up.** They run one after
the other, so neither alone predicts the result:

```text
last request ──┬─ retentionPeriod ─┬─ ≤1 optimize interval ─┬─ cooldownPeriod ─┬─ 0 replicas
               │ (the scaling manager's idle query)│ (the scaling manager publishes 0, the  │ (KEDA)           │
               │                   │  trigger goes inactive)│                  │
```

The external scaler reports the trigger active *unless the scaling manager has
decided the model needs zero replicas*. So KEDA's cooldown cannot start until the
scaling manager has decided to park, which it does only once
`increase(...[retentionPeriod])` reads zero. With both defaults that is
**10m + 300s ≈ 15 minutes**, plus up to one `GLOBAL_OPT_INTERVAL` (15s). A fleet
that "will not park" is often just this sum.

This applies only to the **deactivation step**: the final drop to
`minReplicaCount`/`idleReplicaCount` once the trigger reports inactive. An
ordinary scale-down while the trigger is active (10 → 3) goes through the HPA and
its own stabilization; neither KEDA cooldown applies.

The scaling manager adds no hold of its own. KEDA already guards that transition
per ScaledObject from cluster state, which is also why the scaling manager reads
`minReplicaCount` off the object rather than copying it into trigger metadata.

### `scaleFromZero`

An entry may tune how a model is woken from zero replicas:

```yaml
scaleFromZero: { requirePrefill: true }
```

| key | default | meaning |
| --- | --- | --- |
| `requirePrefill` | `false` | Refuse to wake a P/D-disaggregated model unless a prefill variant can be placed alongside decode. |

You can set it in `default`, in a named tier, or in a per-model entry. The more
specific one wins: `default`, then the tier, then the per-model entry. When a
model's variants name different tiers, the model waits for prefill if any one
of them resolves to `requirePrefill: true`. A variant that names no tier
follows the ones that do.

With `requirePrefill: true`, a P/D model whose prefill cannot be placed stays
at zero. It does not wake decode alone. A model with no prefill variant is not
held: there is no prefill to wait for, and its decode wakes as usual.

**Why the default is `false`.** When waking a model, decode is mandatory and
prefill is best-effort. That follows the llm-d router: with no prefill endpoint
it sends the request to a decode endpoint, which runs both stages itself. The
router already skips disaggregation for short prompts and high prefix-cache hits,
so decode-only is a normal mode, not a failure. If prefill cannot be placed (no
free GPUs on its accelerator, or its namespace quota does not allow that
accelerator), waking decode
alone still serves the queued request; refusing would trade a slower model for no
model.

Set `requirePrefill: true` when slow prefill is worse than no service for this
model: a strict TTFT SLO, or decode nodes that must not take prefill load. Then a
model whose prefill cannot be placed stays at zero, and the engine logs why at
INFO:

```text
Scale-from-zero: no variant woken for a model with pending requests
  namespace=... modelID=... reason=prefill-required
```

Other `reason` values: `no-decode-candidate` (no inactive decode/`both` variant
to wake), `no-capacity` (nothing fit the GPU budget) and `nominal-lent-out`
(Kueue borrowing: nothing fit because the queue's nominal quota is lent to a
peer it cannot reclaim from; on `wva_model_scaling_blocked` as
`wake-nominal-lent-out`). Each is logged once per
change of verdict, not per poll. A model already serving is not a refusal and is
not reported.

Role comes from the `llm-d.ai/role` pod-template label (`prefill`, `decode`, or
absent/`both`). A model with no prefill variants is unaffected.

**Placement.** Selection prefers a variant that can actually get GPUs, checked
against the same GPU/quota limiters the optimizer uses (see
[`limiters`](#limiters-cluster-default-only-live)), on the **sum** of the
variants woken together: prefill and decode can each fit alone while the pair
does not.

The check is **best-effort**, per limiter. A limiter that cannot answer is
skipped and the others still check the wake, the same rule the optimizer
follows ([security model](security-model.md#fail-open-and-fail-closed)).
Refusing a queued request over a missing measurement is the worse failure.

| condition | what the wake is checked against |
| --- | --- |
| no limiter is configured (the shipped default) | nothing: the wake is not checked |
| a limiter's usage is not observed: for `gpu-inventory`, no node-usage snapshot under 5 minutes old (the engine reads the nodes itself at the moment it decides, so this is rare); for a quota, no completed saturation cycle since the start | the other limiters; this one is skipped |
| a limiter fails to compute its figures (for `gpu-inventory`, usually the Node API) | the other limiters; this one is skipped |
| every limiter skipped, and this namespace was checked successfully in the last 5 minutes | that last answer, with each wake since then debited from it (best-effort: per namespace and per accelerator name as the wake declared it) |
| every limiter skipped, no answer in the last 5 minutes | nothing: the wake goes ahead unchecked and is counted in `wva_scale_from_zero_wake_unchecked_total` |
| a variant's accelerator does not resolve | the rest of the set: that variant adds no demand, since it cannot be charged to any pool |

> **The GPU budget can still over-state free capacity**, for the GPU-aware
> optimizer and scale-from-zero alike: a quota charges usage on an unresolved
> accelerator to no cap, and a cordoned node's GPUs still count toward the
> physical pool. These and the other cases are in
> [GPU Capacity Accounting](../concepts/gpu-capacity-accounting.md#where-the-numbers-can-still-be-wrong).
> In short: make the accelerator resolvable with a `nodeSelector`/`nodeAffinity`
> GPU key (otherwise the scaling manager emits an `AcceleratorNotResolved`
> event, and resolves a RUNNING variant from the nodes its pods landed on).

The usage snapshot is published independently of the optimizer branch, so
scale-from-zero can check placement on a cycle the optimizer never reached.

There is no separate enable flag; the `limiters` list below is the whole answer.
Declaring a limiter selects the GPU-aware optimizer (`GreedyByScore`) *and* arms
the scale-from-zero capacity check; declaring none leaves both off. (The old
`enableLimiter` gated only the first, so a quota could be declared and never
enforced, while scale-from-zero consulted a limiter nobody asked for.)

### `limiters` (cluster-default only, live)

The `default` entry selects the GPU limiter. It is the **only source** (no
`--limiter-type` flag, no quota config file), and it is **live**: editing the
ConfigMap switches the limiter without a restart; the engine rebuilds it on the
next cycle.

**The exception:** when a separate policy namespace resolves (see
[who is allowed to change the bound](gpu-limiter.md#who-is-allowed-to-change-the-bound)),
a **namespace-scoped** controller (`--watch-namespace` set) reads that
namespace's ConfigMap once at start and does not watch it, because its cache
cannot hold a namespace it has no RBAC in (`cmd/main.go`). An edit there needs a
restart (`kubectl rollout restart deploy/wva-controller-manager -n <its ns>`).
The same holds for an out-of-reach controller (`WVA_WATCH_NS` naming another
namespace) and its own ConfigMap in its install namespace.
`make enable-physical-limiter` and `make disable-limiter` do those
restarts. A cluster-scoped controller watches the policy namespace and picks the
edit up live.

Every entry in a `limiters:` list is built, and scaling must fit all of them. The
forms below combine: (a) and (b) in one list bound by the GPUs that exist **and**
the declared caps.

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

- **Every entry is built.** A list holding a `gpu-inventory` and `quota`
  entries bounds by both: a decision must fit the physical GPUs and every quota.
  One physical limiter is built: a second `gpu-inventory` entry (under either
  spelling) is ignored, with a warning, rather than rejecting the policy.
- `type: quota` uses the **same schema** as `QuotaLimiterConfig`: `scope`,
  `quotas`, `namespaceQuotas` and `exclude` all apply, and several quota entries
  compose. A quota entry may set `kueue: { enabled: true }` to be bounded by
  Kueue's quotas (see [Kueue as a quota source](quota-limiter.md#kueue-as-a-quota-source)).
- **Cluster-default only.** The list is read only from the global
  (system-namespace) `default` entry, or, when a policy namespace resolves, from
  the `default` entry there, which then overrides the system-namespace copy. It
  is a budget-scope setting like `enableRescale`, so a tenant cannot widen a cap
  through a per-model entry or a [namespace-local scaling-policy map](#namespace-local-scaling-policy-map).
  A `limiters:` block on any other entry parses and validates but is **silently
  ignored**.
- With no `limiters:` block **nothing bounds scaling**. That is the shipped
  default, and the controller says so at startup: "scaling is UNCONSTRAINED".

### `optimizer` (cluster-default only, live)

The `optimizer:` block, beside `limiters:` in the same `default` entry, selects
the **utilization-share optimizer**. Without it, a quota that runs out is first
come, first served: whichever model scaled up earlier keeps its GPUs, and a model
that needs them later waits. With it, the GPUs a quota allows are shared out on
purpose. Every role (the decode and prefill of a P/D model, or the one role of an
aggregated model) is first given its *need* — the GPUs that put it exactly at its
`scaleUpThreshold` — and what is left is shared as headroom above that need, in
proportion to each model's weight. The need is counted in the GPUs the role
actually holds and grows. A role that needs more than it serves now adds the
rest as GPUs of the variants it grows: its most cost-efficient one first, up to
that variant's `maxReplicas`, then the next. A role that needs less is priced
in the order it gives replicas back: its least cost-efficient variant first.
Its need is what it keeps after giving every whole replica it can spare while
the rest still serves its demand at `scaleUpThreshold`, plus part of the next
replica it would give. So a role that runs a variant with less capacity per GPU
than its best one needs more GPUs, not fewer. How busy a role is, and which
role is worst off, is judged on what its GPUs serve, not on how many it holds.
A variant with no capacity reading counts as serving as much per GPU as the
best one. When the quota cannot hold every model at
its recent peak at the same time, that headroom is capped at
`maxHeadroomReplicas` replicas per role (one by default) times the model's
weight, and the rest stays unassigned in the quota, where a model whose demand
rises takes it at once. When the GPUs no longer match those targets,
the optimizer *transfers* replicas: it scales a donor down, waits until its GPUs
are actually released, and only then scales the receiver up.
A role whose scale-down is held, for example after a shape change or while
decode is saturated, keeps every GPU it holds, and no other model is planned
those GPUs.

The design and its reasoning are in the
[utilization-share optimizer proposal](../proposals/utilization-share-optimizer.md);
this section covers what you set and what you will see.

**It needs a budget.** The optimizer shares the GPUs a limiter allows, so it
needs a `limiters:` list. Without one it stays off (`wva_utilization_share_mode`
reports `invalid`) and the controller logs
`needs a limiters: list: there is no budget to share`. It plans:

- one group per namespace that a namespace quota (`scope: namespace`) covers, per
  accelerator type;
- one cluster group per accelerator type for every other model, when a
  `scope: cluster` quota bounds it. Under a `gpu-inventory` limiter alone there
  is no cluster group unless you set `physicalGroups: true` (see the table).

**A namespace over its quota.** When a group's own models hold more GPUs than a
static quota allows (for example a Deployment scaled by hand), the optimizer
shares only the quota, takes no more GPUs, and gives the extra back. The
controller logs `the group's scope holds more GPUs than its quota allows`, and
`wva_utilization_share_reserve_debt_gpus` shows what is still to give back.
It gives back only in these cases:

- A namespace holds more than its `scope: namespace` quota, and the quota
  entry does not read Kueue.
- The namespaces of the cluster group hold more than the `scope: cluster`
  quota leaves them after the other namespaces, and the quota entry does not
  read Kueue. When the other namespaces
  already hold more than the cluster quota by themselves, the overage is
  theirs: the cluster group gives back only what it alone holds past the quota.
- A namespace that counts Kueue borrowing holds more than its queue's nominal
  quota plus `borrowingLimit`.

GPUs that Kueue granted are never given back, whether borrowed, held beside
another workload in the same queue, or held through several queues: Kueue
preempts what it must. With `countBorrowable: false` the optimizer starts no
new borrowing, but keeps what Kueue already lent. A count of the physical
inventory past its capacity is not given back either: it is a stale read, not
a spent quota.

**An excluded namespace needs a finite cluster bound.** To share the models of a
namespace that a namespace quota lists in `exclude`, add a finite
`scope: cluster` quota for their accelerator type. A cluster quota of `-1` is
unlimited and counts as none. A `gpu-inventory` limiter beside the namespace
quota is a finite bound too: with `physicalGroups: true` on a cluster-scoped
controller they are shared over it. Without a finite bound, their models stay on the default optimizer, and
the controller logs them once:
`Utilization share: not sharing models no quota bounds`. No line says when they
are shared again; their role series appear instead.

<details>
<summary>Details: why, and what the cluster group gets</summary>

The namespace quota does not bound an excluded namespace. Without a finite
cluster bound there is no budget to share.

With a `scope: cluster` quota, the excluded namespace's models form the cluster
group. Every GPU a tenant's own group fills is also charged to the cluster
quota. So the cluster group gets only what the cluster quota leaves after the
tenants' claims: its free GPUs, less every tenant group's free quota, never
below zero. For example, a cluster quota of 20
with 6 held and 14 free, and tenants with 6 and 4 free: the cluster group may
fill 4.

A tenant claims only whole replicas of the variant it grows. A tenant grows
its most cost-efficient variant that is below its `maxReplicas`. With 8-GPU
replicas, a tenant with 4 free GPUs claims none of them, so the cluster group
can still fill a free node. This holds even when the tenant also has a 1-GPU
variant: it does not grow that variant, so it claims nothing for it. It claims
the 4 GPUs only when it grows the 1-GPU variant and can place them.

Models the share does not plan count too: frozen ones, ones that span two
accelerator types, and those of a tenant with sharing turned off. Today's
optimizer can still grow them, so the claim rounds to the smallest replica
anything in the tenant can grow. A variant at its `maxReplicas`, or whose role
the analyzers hold from scaling up, grows nothing and does not count. A tenant
with nothing on the type that can grow claims nothing.

This stops the cluster group from counting GPUs a tenant may also fill. It does
not bound the tenants against each other: each tenant's group is bounded only by
its own free quota and the whole cluster free.

</details>

A model whose variants run on more than one accelerator type, or whose analyzer
result is not live this cycle, is left to the default optimizer for that cycle.

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
| `tolerance` | `0.15` | How far a role's GPUs may be from its target, as a fraction of the target, before a move is considered. Must be greater than 0 and less than 1. Wider means fewer, larger moves. The band is never narrower than half a replica -- four GPUs for 8-GPU replicas -- but a role below its need is never in band while its target covers that need. A need above the role's `maxReplicas` is judged against `maxReplicas`, so a role one replica below its ceiling that needs more is short, and takes its last replica. |
| `reserveGPUs` | `0` | GPUs held out of the shared budget so a scale-from-zero wake can start at once instead of waiting for a transfer. |
| `maxHeadroomReplicas` | `1` | Applies only while the quota is **contended**: when it cannot hold every model at its peak need over the last `stabilization.flappingWindow` at the same time (each model's peak need, or its `minReplicaCount` if higher, rounded up to whole replicas, summed, is more than the quota). A peak counts only once two readings in a row reach it, so one glitched reading does not cap the group for a window. While no model's peak is known -- a new group, or every model after a controller restart that finds no usable stored history, until a `flappingWindow` of history exists -- each counts at its `maxReplicas` (the whole quota without one), so the cap is on until a full window of history shows the peaks fit. The demand history is stored in the ConfigMap `wva-utilization-share-history` in the controller's namespace and read back after a restart or a leader change, so a restart costs a window of capped headroom only when that history is missing, cannot be read, or ends more than ten minutes before the restart (see **After a restart or a leader change** below). A model that joins a group whose peaks are known counts at what it holds until its own window fills, and a model absent for longer than a `flappingWindow` starts its history again. `wva_utilization_share_headroom_capped` shows whether the cap is on. While the peaks fit, the whole spare is spread by weight and nothing is given back: in simulation, capping a quota with room for every peak made whole-node models lose 6 to 43% of their demand to giving replicas back and cold-starting them again, against none with the spare spread. While they fit, a model already at its need takes no headroom from a donor that it would leave below the donor's recent peak: its peak over its scale-down window (counted as `donor-peak` in `wva_utilization_share_withheld_total`). Such a move, from a donor that may drain for minutes, would land after the receiver's burst and just before the donor's own. An older peak holds the donor only if it comes back: a donor that peaked twice within the last `stabilization.flappingWindow`, with its load lower for more than a scale-down window between the two, keeps its peak over that window, and one that peaked once can give its spare once its load has been lower for a scale-down window. While contended: how many replicas of headroom the quota's spare GPUs give a model of weight 1 above its need (or above its `minReplicaCount`, if that is higher). A model of weight `w` gets `w` times as many, rounded up: with the default classes, `best-effort` 1, `standard` 1, `important` 2, `critical` 4. Each role of a P/D model counts separately. An integer from `0` to `10000`. The rest of the spare stays unassigned, so a model whose demand rises takes it at once instead of waiting for another model's pods to finish their requests and release their GPUs. `0` gives no headroom; a value as large as the models' `maxReplicas` spreads the whole spare by weight (the behaviour before this key existed). While contended, a model that has held more than its need plus its headroom for `stabilization.consecutiveCyclesBeforeMove` cycles in a row (two by default) starts giving all of the extra back to the quota in one cycle, but only while no model that can grow is short of its need and only while it would stay calm (see `stabilization.returnSpareWhenLoadAtMost`). Nothing is given back while the only models below their target are ones no node can hold a new replica of: when the controller sees the nodes and no node can be emptied for such a model, the returned GPUs would sit unassigned, so the extra stays as headroom (counted as `return-receiver-unplaceable` in `wva_utilization_share_withheld_total`). This is judged every cycle: once a node can be emptied for that model, or another model below its target could use the GPUs, returns resume. A namespace-scoped controller cannot see nodes, so its returns go on as before. The release that follows still waits the model's scale-down window, as a scale-down does, and is called off while it waits if the model's demand comes back. A return always goes to the unassigned GPUs. A model that turns short while a return is still releasing waits for the returned GPUs that will free whole replicas of it, rather than draining other models for GPUs already on their way (`awaiting-return`), and the idle fill raises it once they are released; only returned pods its pods fit count (two 4-GPU pods do not make room for one 8-GPU pod), and a return whose pod shape is not known -- one rebuilt from its pod marks after a restart -- counts for nothing. It never goes below its need (the higher of its current and average need while it swings) plus its headroom, its `minReplicaCount` or its last replica, and never below what the other models' peaks leave of the quota in whole replicas: a model that bursts alone keeps its spare, and GPUs are returned only when another model's recent peak could use them. Weight buys more headroom replicas, not protection: a model's headroom is still taken for another model's need. While another model bursts for longer than a transfer takes to land, a model whose demand swings faster than that (up and down at least twice, by a quarter of its peak or more) is capped on its average need rather than its current one: capped on its crest, it took the spare GPUs that the longer burst then had to wait a release for. |
| `shadow` | `false` | Compute, publish and log everything; actuate nothing. |
| `physicalGroups` | `false` | Also plan groups bounded only by the physical GPU inventory (no quota), where spending the whole budget means holding every GPU of that type. Cluster-scoped controllers only. A namespace-scoped controller ignores it: where a quota bounds its namespace it changes nothing, and under `gpu-inventory` alone the optimizer runs without it, logs a warning and sets `utilizationShareNote` in `wva-controller-status`. The mode is not `invalid`. |
| `weightClasses` | `best-effort: 0.5`, `standard: 1`, `important: 2`, `critical: 4` | Class names and their weights. Replaces the defaults whole when set. Exactly one class must have weight `1`; it is the default class. |
| `namespaces.<ns>.enabled` | `true` | `false` keeps that namespace's quota group on the default optimizer. |
| `clusterNamespaces` | empty (every namespace) | A list of namespaces. When set, the cluster groups plan only the models of these namespaces; every other model is left to the default optimizer. A canary for a cluster quota. An empty entry (`""`) makes the block invalid. Does not affect namespace quota groups. |
| `stabilization` | see below | How fast GPUs move between models against how often they move back. |

**Stabilization.** GPUs that move between models take minutes to become useful
again (a replica must drain, then the receiver's must start), so moving them
back and forth costs both models. These keys decide how much evidence a move
needs and how long a move holds:

```yaml
utilizationShare:
  stabilization:
    consecutiveCyclesBeforeMove: 2
    waitBeforeReverseMove: auto     # about 12m with a 300 s scale-down window
    flappingWindow: auto            # about 2h
    swingReversals: 3
    skipWaitsWhen:
      enabled: true
      receiverLoadAtLeast: 0.9
      donorLoadAtMost: 0.6
    returnSpareWhenLoadAtMost: 0.6
```

| Key (under `stabilization:`) | Default | Meaning |
| --- | --- | --- |
| `consecutiveCyclesBeforeMove` | `2` | How many controller cycles in a row a model must look short (or have GPUs to spare) before GPUs move. It is also how many cycles a model must hold more than its need plus its headroom before it starts giving the extra back to a contended quota (the release then waits its scale-down window). Raise it to ignore brief spikes; `1` acts on the first reading. |
| `waitBeforeReverseMove` | `auto` | After a model gives GPUs it cannot receive any for this long, and after it receives it cannot give. A duration (`12m`) or `auto`: twice the time the group's donors take to release a replica (scale-down window, KEDA polling, pod drain, or the measured time once three releases have completed). `0s` turns the wait off. |
| `flappingWindow` | `auto` | A model whose moves change direction `swingReversals` times (three by default: a swing and a half) within this window is planned on its average demand over the window, not its current one, so a load that swings faster than GPUs can follow stops chasing it. A transfer called off because demand reversed before anything moved counts as the move it would have made, so it is a change of direction only when it reverses the model's move before it; a spare return called off counts for nothing, as it moves no GPUs between models. A single swap and swap back is not a swing: counting the move that first raised the model, it changes direction twice, and with the default `swingReversals` the models follow their current demand. This ends early when the load moves for good: once the model's demand has stayed more than a quarter above its average, or a quarter below it, for as long as a move takes and for longer than any earlier swing in the window, it is planned on its current demand again. A load that keeps swinging stays on the average as long as it changes direction `swingReversals` times within the window; with the default three, one whose halves each last more than about a third of the window is followed move by move. A duration or `auto`: eight times the time a move takes from decision to serving. `0s` turns the rule off. |
| `swingReversals` | `3` | How many changes of direction within `flappingWindow` make a model swinging, so that it is planned on its average demand. An integer from `2` to `20`. `2` holds a single swap and swap back on the average, as before this key existed: in the measured case two models sat halfway for eleven minutes after a swap back and reached their split after 28, against 9 with the rule off. Raise it to hold only a load that keeps swinging; with `flappingWindow` at `0s` it has no effect. |
| `skipWaitsWhen.enabled` | `true` | Let a large imbalance skip the two waits above (`consecutiveCyclesBeforeMove` and `waitBeforeReverseMove`). On its first cycle such a move goes at the usual pace (two replicas per model per cycle, two transfers in flight per quota group). If the imbalance is still there the next cycle, the receiver takes every replica it is short of its need (for a model planned on its average because its demand swings, the higher of its current and its average need) in that cycle, from one donor or several, and these moves do not wait for the two-in-flight limit, though they occupy it: other moves wait until they land. That holds only while the receiver's burst is not expected to end before such a move lands, with a quarter of that time to spare, judged from its longest burst so far; a burst that will be over by then keeps the usual pace, since moves landing after it only leave the donor short for its own next burst. A model with no completed burst on record is not held back. Every other move counts against the limit, including one that skipped the waits on its first cycle; a spare return never does. Whether the imbalance is large is judged for the receiver on the higher of its current and average need while its demand swings, so a swinging model at the crest of a burst qualifies as a receiver. A donor is judged, as a scale-down is, on the higher of that and its peak need over its scale-down window, so a calm model read in a trough of its normal demand is not drained in two cycles; the same holds for each donor of a set. The replica of headroom above its need is an ordinary move. A donor is still never taken below its own target, its floor or its last replica, nor past `donorLoadAtMost`. `false`: every move waits, even when one model is at its scale-up threshold and another is nearly idle. |
| `skipWaitsWhen.receiverLoadAtLeast` | `0.9` | The receiver's load at or above which it may be funded without waiting. **Load** is a model's utilization as a fraction of its scale-up threshold: `1` is where it would scale up, `0.9` is 90% of the way there. |
| `skipWaitsWhen.donorLoadAtMost` | `0.6` | The donor's load, on the GPUs it keeps after giving, at or below which it may give without waiting. The donor's demand here is the higher of its current need and its peak need (two readings in a row) over its scale-down window -- unless the receiving model's bursts outlast a move, when its current need alone: a move that lands inside a long burst would only be held back. One reading does not make a peak. Must be below `receiverLoadAtLeast`: the gap is what stops GPUs going back and forth. Raise it (or lower `receiverLoadAtLeast`) to correct smaller imbalances sooner, at the price of more moves that are later reversed. |
| `returnSpareWhenLoadAtMost` | `0.6` | When a model may give back headroom above `maxHeadroomReplicas` to the quota: only while its load would stay at or below this value on what it keeps, not counting its headroom. The load is what its demand needs over what those GPUs serve, not over how many there are: a model running variants of different speeds keeps its faster replicas and gives back its slower ones first, so a GPU count would read it calmer than it is. The same load decides when a return still waiting is called off. A second meaning: while more than `returnSpareWhenLoadAtMost` x `queueLengthThreshold` requests (3 by default) are waiting on one of the model's pods, for `consecutiveCyclesBeforeMove` cycles in a row, the return waits -- for at most the model's scale-down window, after which it goes. A decode pod of a P/D model counts only while its KV cache is full; prefill pods are not counted. Greater than 0 and less than 1. At `0.6`, a model that gave a replica back is raised again only once its demand has grown by about 1/0.6 (1.67 times), so a demand that hovers near a replica boundary does not drain a pod and start another every few cycles. Lower keeps replicas longer after demand falls. |

Nothing else is configurable. The timings a transfer runs on — how long a release
may take, how long a receiver may stay Pending, and, unless `stabilization` sets
them, how long a role that gave must wait before it receives and the flapping
window — are derived per group, not per donor: from the slowest
donor configuration in the group (the longest scale-down window and polling
interval among the donors' ScaledObjects, and the longest termination grace among
their pod templates), and from the releases the group has measured. One model with
a 30-minute scale-down window therefore stretches every transfer in its group.
`wva_utilization_share_effective_seconds` shows the values in force and where each
came from.

**Weights.** A model's weight decides how much of the spare headroom its roles
get and, when the quota is short, whose shortfall counts most: a role of weight 2
gets twice the headroom of a role of weight 1 relative to its need, and is cut
last. When the quota is contended and headroom is capped, the cap scales with
the weight too: `maxHeadroomReplicas` replicas for weight 1, twice as many for
weight 2, rounded up. Set it per model with **one** of:

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

A namespace with its own [namespace-local scaling-policy map](#namespace-local-scaling-policy-map) sets its models'
weights and thresholds itself. Such a model is planned in its namespace's own
quota group, which that namespace owns whole, but **not** in the cluster group,
where its weight would be weighed against other tenants'; there the default optimizer
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
free GPUs plus what they hold. Every other model stays on the default optimizer, and
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
entry (or the policy namespace's, when one resolves), and live: an edit takes
effect on the next cycle, except where a namespace-scoped controller reads it
outside the namespace it manages (a policy namespace, or an out-of-reach
controller's own ConfigMap), which it reads at start only (see
[`limiters`](#limiters-cluster-default-only-live)).
An `optimizer:` block in a [namespace-local scaling-policy map](#namespace-local-scaling-policy-map) has no effect; the controller logs
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
(`wva_utilization_share_headroom`), plus whatever GPUs of the quota are
unassigned, which it takes without a transfer. GPUs another model's target asks
for are not unassigned, even before that model holds them (its new replica is
still starting, or its Deployment was scaled down by hand). A spike larger than that waits
for a transfer, and a transfer releases the donor first: expect at least one donor
scale-down window before the new replica serves. For whole-node models the full
wait is the donor's scale-down window, its drain and termination grace, up to
one planning cycle to see the release, the KEDA poll and HPA sync that raise the
receiver, scheduling, and the receiver's model load -- often 3 to 10 minutes for
a large model.

**Whole-node models: give them a `PriorityClass`.** A node freed for a
whole-node model opens one GPU at a time as the donors' pods go, and the
model's pod fits only once all of them are free. The scheduler holds nothing
for a Pending pod of the same priority, so any Pending pod that needs fewer
GPUs (another tenant, a training job, one 1-GPU pod is enough) takes the
first GPU. The transfer then ends `fill-timeout` with `release-taken`, and the
donors have shrunk for nothing. Set `priorityClassName` on the whole-node
model's pod template, naming a `PriorityClass` above the cluster's other GPU
pods, with the default `preemptionPolicy` (`PreemptLowerPriority`; `Never` does
not help). For a LeaderWorkerSet, set it on the leader and the worker
template. The model's Pending pod then preempts lower-priority pods, is
nominated to the node it preempted on, and the scheduler keeps that node from
lower-priority pods until it lands.

```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: whole-node-inference
value: 100000          # above every other GPU workload's class
preemptionPolicy: PreemptLowerPriority
description: Whole-node models in a utilization-share group
---
# In the model's Deployment (or both LeaderWorkerSet templates):
spec:
  template:
    spec:
      priorityClassName: whole-node-inference
```

The cost, which is the cluster owner's call:

- A Pending pod of that model preempts whenever it waits, for any reason: a
  rollout's surge pod, a pod moved off a failed node, a replica raised by
  today's optimizer. Each can evict a node's worth of lower-priority pods,
  including pods the scaling manager does not manage. A whole-node Deployment
  with `maxSurge: 0` rolls out without that surge.
- The scheduler picks the victims, not the controller. It prefers the node
  where it breaks the fewest PodDisruptionBudgets, then whose victims have the
  lowest priority, then the fewest victims. That is usually the freed node,
  where one pod took one GPU, but it can be another node, and the victims can
  be other share-group models' pods. If one is a pod of a donor that is still
  releasing, a node-planned transfer ends `wrong-pod` and its GPUs go back to
  the budget: the move is lost, nothing else.
- Preempted pods get their termination grace period, so the receiver waits
  for it.

The controller logs `whole-node models set no priorityClassName` and names
them, once per change, while it has the node picture. It checks only that the
name is set, not that the class is above the others.

An idle model is not
shrunk to its floor: it stays at its headroom target, its weighted share of
the spare GPUs. While the quota is contended, that share is at most its need
rounded up to whole replicas plus `maxHeadroomReplicas` (one by default) times
its weight, rounded up -- or what the other models' recent peaks leave of the
quota, if that is more; a model that has held more than that for
`consecutiveCyclesBeforeMove` cycles starts giving the extra back to the quota
while no other model is short of its need. A return gives back no more than
today's optimizer would scale down: it stops where the model's demand sits a
quarter of the way up from `scaleDownBoundary` to `scaleUpThreshold` (0.7375
at the defaults), in whole replicas, and a return it holds back for that
counts `wva_utilization_share_withheld_total{reason="scale-down-band"}`. GPUs
moved to a model that is short are not held to that boundary. The release
still waits its scale-down window, and is called off if its demand comes
back meanwhile. The
spare then sits unassigned, where the next model to spike takes it at once
instead of waiting for a donor's pods to drain.

A model that is still giving GPUs -- a transfer, a return or a refill still
releasing -- is not raised, by a transfer or by the idle fill, until that
release lands.

**How to protect a model.** Raise its `minReplicaCount` (a floor is never taken,
but it is paid for by every other model in the group: see `floor-pinned`), or
give it a higher `weightClass`. Weight buys a larger share of the spare and a
smaller share of a shortfall, and scales its headroom replicas
(`⌈weight × maxHeadroomReplicas⌉` while contended); it does not keep that
headroom from being taken for another model's need. Headroom is counted in
replicas, so it shrinks relative to need as a model grows: at a need of about
9.6 replicas, one replica of headroom leaves the model near 0.87 of its
scale-up threshold. Only a floor protects it outright. The optimizer does not read `priority`; only
`weight`/`weightClass` decide shares. On a cluster quota the admin's policy sets
the weights: a model is planned in a cluster group only when its namespace has no
scaling-policy map of its own, so its weight is the one the admin's policy
resolves (the `default` entry, a tier, or an override there). A model whose
namespace has its own map is excluded from the cluster group and stays on
today's optimizer; the controller log lists it in the `frozen` field with
`namespace has its own scaling-policy map; not planned in the cluster-wide group`.

**What sharing can cost your model.** These were measured in simulation, not on
a cluster, and none of them is fixed by a setting other than a floor:

- **A noisy model beside one that bursts.** While another model in the group
  bursts for longer than a transfer takes to land, a model whose demand swings
  faster than that gets its headroom on its average demand, not its current
  one, so it no longer takes the spare GPUs at each crest. That served the
  bursting model better (0.20 to 0.21 of its demand unserved in simulation,
  against 0.33 to 0.35 before) and cost the noisy one 0.02 to 0.04 more of its
  demand (up to 0.06 in other noisy shapes), still less than under earlier
  versions of the optimizer. The noisy model shows the blocked reason
  `fast-swing-capped` while this applies. If yours is the noisy model and
  that cost matters, a floor is the protection.
- **Whole-node models with bursts of 20 to 30 minutes.** A model of one 8-GPU
  pod per replica whose bursts last 20 to 30 minutes still does slightly worse
  than before a hard imbalance's moves stopped waiting for the pace (in a tight
  quota at 30 minutes, 0.232 of its demand unserved in simulation, against
  0.225). Such moves now wait for the pace when the model's last completed
  burst says this one ends before they land, which recovered most of the
  cost (0.247 before); the model shows `burst-ending` meanwhile.
- **A move's latency is the slowest model's start.** A move's latency is
  estimated as the donor's release, the receiver's start, and three
  cycles. The start is the one the controller measures for the group's
  slowest model (`wva_replica_start_seconds_estimate`; 5 minutes until a replica
  has started), so a whole-node model's 10 minutes counts as 10. Being one
  figure per group, a small model beside a whole-node one is paced on the
  whole-node start.
- **A dip in a slow-draining model's demand, right after the group starts.**
  At a group's first start, or after a restart with no usable stored history,
  no model's peak is known for about the first 12 minutes. A model holding
  replicas it does not need yet then looks the same as one whose demand is
  about to come back. It gives them back after `consecutiveCyclesBeforeMove`
  cycles, and if its demand returns it cold-starts them again: in simulation a
  12-minute dip at a 10-minute scale-down window left 0.038 of its demand
  unserved, against 0.001 or less when returns waited the scale-down window. Waiting
  the window was tried and dropped: it holds the GPUs from a model about to
  burst (a model bursting beside a noisy, slow-draining one went from 0.305 to
  0.348 at a 5-minute window). Restored history avoids both. If a dip right
  after a fresh start matters for your model, a floor is the protection.
- **After a controller restart with no usable stored history.** The need
  history survives a restart in a ConfigMap (see **After a restart or a leader
  change** below).
  Without it -- the first start, a deleted or unreadable ConfigMap, or a
  controller down for more than ten minutes -- every model counts at its
  `maxReplicas` until a `flappingWindow` of need history exists, and headroom
  is capped even where the peaks would fit. For whole-node quotas whose peaks fit, that cost 2
  to 10% of demand for about two hours in simulation. The window is derived
  from the scale-down window, so a long scale-down window makes it hours; read
  it in `wva_utilization_share_effective_seconds{param="swing-window"}`.

**If your model bursts in turn with another.** A model that just gave a
replica cannot receive one for the reversal hold, about twice a release time
(on the order of 12 minutes with a 300 s scale-down window), so two models
that take turns do not trade GPUs back and forth. The hold is lifted when the
imbalance is hard: your model's load is at least 0.9 of its scale-up
threshold and another model's load, on the GPUs it keeps after giving, is at
most 0.6 of its own (`stabilization.skipWaitsWhen` in the policy sets both),
that donor's load read on the higher of its current demand and its peak over
its scale-down window. That
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
is not taken as prefill's capacity, because prefill then waits on decode. With
[`enablePrefillThroughputBound`](#enableprefillthroughputbound) on, it is kept
as a lower bound instead, which lets the router's arrival rate grow prefill.
What is learned is kept per band of prompt length, so a shift in prompt length
puts prefill back on KV-memory pricing until it queues again, and a restart
keeps it only with `LEARNED_STATE_SNAPSHOT` set. In the one P/D
benchmark attempt prefill never queued on its own and never left one replica
under 15 000-token prompts
([why](../well-lit-paths/utilization-share/measured.md#an-earlier-attempt-on-one-model)).

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
The demand history the contention test reads is not lost: the acting
controller writes it, when it changed and at most once a minute per quota
group, every changed group in one write, to the ConfigMap
`wva-utilization-share-history` in its own namespace (one key per group: each
model's first-seen time and, for each minute over the last `flappingWindow`,
the highest level two readings in a row reached in it -- the same rule a peak
is counted by, so a restart neither turns lone spikes into a peak nor loses a
short real one). When a group's state is rebuilt -- a restart, a leader change,
the optimizer switched from shadow to acting, or a group back after it was
dropped -- the history is read back before the group plans, so the models'
peaks are known at once and the cap does not turn on by itself. The log line
`Utilization share: need history restored` says so. A history kept over a
shorter `flappingWindow` than today's makes its peaks known only once today's
window is covered. Shadow mode writes nothing. A group that disappears keeps
its stored history, so a group dropped after a few minutes of failed reads gets
its peaks back; an entry older than its window is removed by a later write.
When there is no usable stored history -- a new group, a deleted ConfigMap, a
read that fails, or a history that ends more than ten minutes before now (a
controller down that long missed whatever peaked meanwhile) -- each model
counts at its `maxReplicas` in the contention test until a `flappingWindow` of
history exists, and headroom is capped meanwhile unless the models'
`maxReplicas` fit the quota together (see `maxHeadroomReplicas`). The
two-readings rule stops a glitched reading; it does not stop a tenant who
controls its own model's metrics and holds them high for two cycles, and a
history restored after a restart carries such a peak like any other. The
controller needs `get` and `update` on that ConfigMap and `create` on
ConfigMaps in its namespace; the shipped Role
`wva-utilization-share-history-role` grants exactly that, in every install
overlay. An install upgraded from before the store existed runs without it
until an admin re-runs `make setup-prereqs` for the namespace. Deleting the
ConfigMap flushes the history, in the running controller as well; editing a
key by hand does not, since the next write replaces it. Size bounds, the log
lines and uninstall: see
[troubleshooting](troubleshooting.md#utilization-share-optimizer).

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

The configuration lives in the ConfigMap `wva-scaling-policy-config` in the
controller's namespace. The exception is `limiters:` and `optimizer:`: when a
policy namespace resolves, those two come from the ConfigMap there, and the
controller's own copy no longer counts for them (see
[who is allowed to change the bound](gpu-limiter.md#who-is-allowed-to-change-the-bound)).

**Location:** `deploy/configmap-scaling-policy.yaml`

### Namespace-local scaling-policy map

A **namespace-local scaling-policy map** is a ConfigMap of the same name,
`wva-scaling-policy-config`, in a model namespace instead of the controller's.
For that namespace's models it **replaces the global map whole**. There is no
field-by-field merge: its `default` entry must carry every threshold it wants,
and tiers and per-model entries that exist only in the global map do not exist
there (`internal/config/config.go`, `resolveScalingPolicyConfigMap`). Deleting
it returns the namespace to the global map. It cannot set `limiters:` or
`optimizer:`; those come from the cluster `default` entry only.

A namespace gets one when the controller reads a ConfigMap of that name there:

- **Cluster-scoped install:** the namespace has registered workloads, or it
  carries the label `wva.llmd.ai/config-enabled=true` (read at start;
  `kubectl label namespace <ns> wva.llmd.ai/config-enabled=true`), and is not
  excluded with the `wva.llmd.ai/exclude` annotation
  (`internal/controller/configmap_reconciler.go`,
  `shouldWatchNamespaceLocalConfigMap`). The ConfigMap must also carry the
  label `app.kubernetes.io/name: workload-variant-autoscaler`, which the
  controller's ConfigMap cache filters on in this mode.
- **Namespace-scoped install, out-of-reach shape** (controller namespace ≠
  watched namespace): the ConfigMap in the watched tenant namespace is always
  read, no label needed (`internal/controller/predicates.go`). See
  [the security model](security-model.md) for why this is by design.

### Re-running the installer resets the default entry

`make deploy-wva` re-applies `wva-scaling-policy-config` in the controller's
namespace from the shipped manifest on every run, upgrades included. The
`default` entry comes back exactly as shipped — thresholds and analyzers, and
**no** `scaleToZero`, `defaultPolicy`, `optimizer:` block or `limiters:` — while
every other key (named tiers, per-model entries) is saved before the apply and
restored after it (`deploy/lib/infra_wva.sh`). The `limiters:` list is the
exception: with `WVA_LIMITER` unset, the installer reads the live list before
the apply and writes it back after, and says so. `WVA_LIMITER=gpu-inventory|quota`
replaces it, and `WVA_LIMITER=none` removes it. If the live list cannot be read
and `WVA_LIMITER` is unset, the install refuses.

So anything you changed in `default` by hand is gone after a re-run:

| you set, in `default` | after a re-run |
| --- | --- |
| `scaleToZero` | repeat it, or set it on a named tier or a per-model entry (an entry whose body names the model); both are kept |
| `defaultPolicy` | re-apply it; the tiers it names are kept |
| `optimizer:` (utilization share) | removed: run `make utilization-share-shadow`, then `make utilization-share-activate` again, each with `NAMESPACE=<models namespace> CONTROLLER_NAMESPACE=<controller namespace>` |
| `limiters:` | kept when `WVA_LIMITER` is not passed, hand-written entries included; `WVA_LIMITER=none` removes it |

This touches only the controller's own ConfigMap. A policy published in
`wva-policy` by `make enable-physical-limiter` is not re-applied — but that
command also writes into the own ConfigMap of each admin-owned controller, and
a re-run of `make deploy-wva` for one of those controllers resets it there.
A namespace-local map in a model namespace other than the controller's is not
touched.

### Parameters

| Parameter | Type | Description | Recommended |
|-----------|------|-------------|-------------|
| `kvCacheThreshold` | float64 | Ceiling on usable KV cache (0.0-1.0): a replica's capacity is its physical KV budget scaled by this, so utilization reaches 1.0 when KV occupancy reaches the ceiling. A capacity multiplier, not a saturation test: lower values scale out sooner. It compounds with `scaleUpThreshold` (scale-out at `kvCacheThreshold × scaleUpThreshold` of physical KV) | 0.80 |
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
| `shapeChangeHoldSeconds` | int | How long the fleet is held back from scale-down after the workload's shape changes. Raise it for long generations (long context, long chain-of-thought). A tier or a per-model override can set it. A negative value is skipped and the layer below applies; see [a bad value is skipped](#scaletozero) for where it is reported. | `0` (5 minutes) |
| `disableShapeChangeHold` | bool | Turns the shape-change hold off. A tier or a per-model override can turn it off. It cannot turn back on a hold that a lower layer (`default` or the tier) turned off. | `false` |

### Expected output length

This one is **not** a ConfigMap field. It is set on the ScaledObject, beside
`modelID`, and a `defaultOutputTokens` key in this ConfigMap is ignored. Every
key the trigger carries is listed in [ScaledObject trigger metadata](scaledobject-trigger.md):

```yaml
triggers:
  - type: external-push
    metadata:
      scalerAddress: wva-external-scaler.<controller-namespace>.svc:9090
      modelID: ibm/granite-13b
      defaultOutputTokens: "6000"
    authenticationRef:      # TLS, the default. Plaintext install: ...svc.cluster.local:9090 and no authenticationRef
      name: wva-external-scaler-<controller-namespace>
      kind: ClusterTriggerAuthentication
```

The scaling manager prices each request waiting in the router queue at the
output length it expects: a queued request owes the decode role `O` tokens of
work, which is what makes a growing queue worth ordering capacity for. `O` is
the first positive number of:

| source | what it is |
| --- | --- |
| the fleet's own reading | the output length the running replicas are averaging over their recent completions |
| the fleet's last known shape | the stable shape the throughput keys are built from, which carries the figure across a gap in readings |
| `defaultOutputTokens` | this key |
| the built-in default | 512 tokens |

Set it when generations are much longer than a chat completion. Until a
replica completes a generation there is no reading (the first cycles after a
restart, after a scale from zero, or after the traffic changes shape), and the
built-in 512 under-states a 6000-token workload twelvefold. A queue priced low
does not order capacity, and that costs time-to-first-token.

It only seeds that window. The fleet's own measurement replaces it as soon as
one exists, so the right order of magnitude is enough.

A value that is not a whole number of tokens is **rejected at its trigger**, with
an error naming the key and quoting the value: `"lots"`, `"-1"` and `"6000.5"`
are refused, not read as zero, because that error is the operator's only view of
a bad value. `"0"` is the exception: it means *unset*, like leaving the key out,
so the table falls through to the built-in default. Prefer omitting the key.

**Why the trigger and not this ConfigMap.** The figure is a fact about one
workload's traffic, not a threshold shared by a class of workloads; a fleet-wide
figure is what the built-in 512 already is. The trigger also declares
`replicaStartSeconds`, another per-workload seed, but that key is **reserved:
nothing reads it yet**; the analyzer uses its own default until it measures a
replica start ([trigger reference](scaledobject-trigger.md)).

**When a model's variants disagree.** A P/D model has a ScaledObject per role,
so the key can appear twice. The **largest** figure wins, and the disagreement is
logged with every figure. Largest, because only one role generates tokens: a
prefill trigger without the key must not pull decode's figure down to the
fallback, and under-pricing the queue is what the key exists to prevent. Setting
it on one variant only is the simplest way to get this right.

### `enableLearnedStateReuse`

Lets a sibling's measurement become a variant's answer, where it could price the
variant higher than its own data would. **Off by default** — see
[Why it is off](#why-it-is-off) before turning it on. A sibling's figure that can
only price a variant *lower* is used either way; see
[What it does not govern](#what-it-does-not-govern).

```yaml
default:
  enableLearnedStateReuse: true
```

It resolves like any per-entry field, so a named tier or a per-model entry can
also set it. A per-model entry is its own ConfigMap data key, identified by the
`model_id` and `namespace` in its **body**; the key name is arbitrary:

```yaml
llama-8b-prod:
  model_id: meta-llama/Llama-3.1-8B
  namespace: prod
  enableLearnedStateReuse: true
```

Both `model_id` and `namespace` are required: an entry missing either matches
no model. An entry whose body names no model becomes a selectable policy
**tier** under its own key. A misspelled key, such as the trigger's `modelID:`
in place of `model_id:`, is ignored and reported; see
[Example Validation Errors](#example-validation-errors). Full schema:
[Per-Model Overrides](#4-per-model-overrides).

The granularity is `(model_id, namespace)`, not a variant. Decode and prefill of
a P/D model resolve the same answer, so you cannot enable reuse for one alone.

One direction only: an entry can turn reuse **on** where the default leaves it
off, but cannot turn it **off** where the `default:` entry turned it on; change
the `default:` entry for that. The `Effective scaling policy` log line shows what
a model resolved `enableLearnedStateReuse` to. A typo in the key silently does
nothing, which means "still off".

#### What it turns on

A variant with nothing measured yet is priced from a sibling running the
**same engine configuration** on the same accelerator and GPU count, so a
scaled-out variant's first decision is not a guess. Two paths, and this key
governs **both**:

| path | what is reused |
| --- | --- |
| the shared ITL line | the inter-token-latency model `ITL(k) = A·k + B` another variant fitted, used to derive this one's service rate |
| the cross-variant capacity figure | another variant's measured per-replica capacity, as the estimate for a variant with no record of its own |

It does not control learning: a variant learns from its own readings either
way, and changing the key clears nothing stored.

#### What it does not govern

A sibling's figure that can only *lower* a variant's capacity is used whatever
this key says. Lowering capacity orders *more* replicas, which is the safe
direction for latency. The key governs where a sibling's figure would *become*
this variant's answer, never where it only limits it. There are two such places:

- **A ceiling on a stored estimate.** A variant with no ready replica is priced
  from its stored record, bounded above by a compatible sibling's.
- **The k2 borrow** (`P2-borrow` in the [cycle log](cycle-log.md), and
  `wva_learned_state_borrowed{quantity="k2"}` in
  [Prometheus](prometheus.md#wva_learned_state_borrowed)). A variant
  that has not yet measured its own k2 for the traffic it is serving uses a
  compatible sibling's measured k2 in the same namespace, but only where it is
  lower than the variant's own capacity, and only from a sibling whose measured
  KV cache is within 3% of the variant's. After 30 minutes it relaxes back to the
  variant's own figure over 6 hours, because a k2 that is too low never corrects
  itself: lightly loaded replicas never saturate, and saturation is the only time
  k2 is measured. The hours count only while the variant is serving, so a parked
  variant keeps its borrow for when it wakes. Once relaxed, it does not start
  again unless a sibling's figure is more than 20% below the one it last ran
  on, or the variant's engine arguments change (a new image alone does not).

What they cost is in the other direction: a sibling that only *looks*
compatible — two configurations whose unread flags both fell back to the
defaults, for example — can hold extra replicas, for up to the 6.5 serving hours
of a borrow. The k2 borrow can be turned off per model with
[`disableK2Borrow`](#disablek2borrow); the ceiling cannot.

#### Why it is off

Reuse rests on an equality: two variants share only if a digest over the engine
flags read from their deployments matches. **That equality is not sound.** Five
rounds of adversarial review found seven measured ways to make a false match or
break a true one. Two attempts to fix them in the flag parser were reverted for
costing more than they bought; one denied sharing to ordinary deployments because
a shell preamble contained `grep -q`, while `-qfp8` still hashed as no flag.

Worst, a configuration read *wrongly* (not just partly) still gives a confident
digest: SGLang flags parsed as vLLM read as a complete set of vLLM defaults, and
match every other default configuration of that model.

So everything that only **describes** a configuration is on by default: the
digest, the `wva_engine_config` metric, the drift checks. Letting one variant's
reading price another's decision waits for you to ask. The planned replacement
shares by **similarity**, not proven identity, so an unreadable flag becomes a
distance penalty instead of a false equality; see
[learned-state sharing by similarity](../proposals/learned-state-sharing-by-similarity.md).

#### What it costs to leave off

A variant with no readings of its own prices its first decisions from its
deployment-derived capacity, lowered by a compatible sibling's measured k2 where
that is lower (see [What it does not govern](#what-it-does-not-govern)). It does
not take a sibling's ITL line, or a sibling's capacity figure as its whole
estimate. Expect a slower first ramp on a newly scaled-out variant.

#### Before turning it on

Check what the manager read; the two paths degrade differently:

- the **shared ITL line** is refused when flags could not be read. An incomplete
  read gives no sharing key, so nothing is borrowed or lent.
- the **cross-variant capacity figure** is *not* gated on a complete read, on
  purpose. Gating it was tried and caused a 30× capacity over-estimate: refusing
  a sibling's measured figure falls back to a higher estimate, and higher
  capacity orders fewer replicas. So with this key on, a variant whose flags were
  only partly read **is** still priced from a sibling's capacity figure.

Read `wva_engine_config` to tell the cases apart:

| what you see | what it means |
| --- | --- |
| a series with a non-empty `unresolved` label | one or more flags could not be read, and `unresolved` names them (usually a value coming from a ConfigMap or `valueFrom`, which the manager cannot read). With the key on, no ITL line is shared, but the capacity figure is — so this is the riskiest case to enable. Fixing the flags is the better remedy. |
| **no series at all** | no parsed engine configuration: no capacity record yet, nothing readable in the pod template, or no scale target resolved. Not a reliable signal on its own — the series is also briefly absent on a cycle where the variant drops out of the analyzer's series. |
| a series with `unresolved` empty | the configuration was read in full, and with the key on the variant shares on **both** paths, keyed on `fingerprint`. This is the case the key is for. |

Joining two variants on the fingerprint takes all four of `model_name`,
`accelerator`, `gpus` and `fingerprint` — the sharing key is the tuple, not the
digest alone — and, for the ITL line, `unresolved` empty on both sides.

### `disableK2Borrow`

Stops the k2 borrow (`P2-borrow`) for a model. Its variants price from their
own figures only: their own measured k2, or the deployment-derived figure and
k1 until they measure one. A variant's own measurement from engine arguments
it ran before (`P2-prior`) is its own figure, not a borrow, and is kept. **The borrow is on by default**, because it can only
order more replicas, never fewer. Turn it off when it costs you: a sibling that
looks compatible but is not, holding a fleet larger than its load needs.

```yaml
llama-8b-prod:
  model_id: meta-llama/Llama-3.1-8B
  namespace: prod
  disableK2Borrow: true
```

- It takes effect on the next cycle. The `Effective scaling policy` log line
  shows what the model resolved `disableK2Borrow` to.
- A borrow already running is paused, not reset. If you turn the borrow back
  on, it resumes where it stood.
- One direction only, like `enableLearnedStateReuse`: a per-model entry can turn
  the borrow **off** where the default leaves it on, but not back on where the
  `default:` entry turned it off.
- To discard what one variant has learned, rather than stop it borrowing, see
  [Reset one variant's learned state](troubleshooting.md#reset-one-variants-learned-state).

### `enablePrefillThroughputBound`

Lets the throughput floor size prefill on a **lower bound**. This matters for
P/D models on vLLM only. **It is off by default.**

The bound is the prompt-token rate a prefill replica showed while its queue was
full and its own rate could not be recorded, usually because decode was full
too. Prefill can do at least that much. With the bound on, the router's arrival
rate grows prefill before its queue builds. With it off, prefill has no floor
until it saturates while decode is fine, and grows only once its queue builds.
Off, nothing about the bound is recorded or read.

Turn it on for every model in the `default:` entry, or for one model in its own
entry:

```yaml
default:
  enablePrefillThroughputBound: true
```

```yaml
llama-8b-prod:
  model_id: meta-llama/Llama-3.1-8B
  namespace: prod
  enablePrefillThroughputBound: true
```

**The risk goes one way: it can add prefill replicas, never remove them.** The
bound is low, so prefill can end up larger than its load needs. In the worst
case the extra is the ratio between what prefill can really do and the highest
rate it was seen doing behind a full decode. A prefill that large rarely
queues on its own, so the extra **lasts until you reset the variant's learned
state** ([how](troubleshooting.md#reset-one-variants-learned-state)). These
limits apply:

- It needs two readings a minute apart before it adds any replica. With one,
  it only holds the fleet.
- One cycle adds at most as many replicas as are ready, so the fleet at most
  doubles per replica start.
- It does not add prefill while decode is still full.
- It only rises. A lower reading, taken behind a full decode after prefill
  grew, does not lower it.
- Prefill's own measurement replaces it.

- It takes effect on the next cycle. The `Effective scaling policy` log line
  shows what the model resolved `enablePrefillThroughputBound` to.
- One direction only, like `enableLearnedStateReuse`: a per-model entry can
  turn the bound on where the default leaves it off, but not off where the
  `default:` entry turned it on.
- Turning it off stops it at once. A bound already learned is kept, unread,
  and ages out after a week; turned back on within that week, it is used
  again.
- The bound is kept with the rest of the learned state, so it survives a
  restart when `LEARNED_STATE_SNAPSHOT` is set. It is set aside when the
  variant's engine arguments change.
- To see when the bound is in use, read `lowerBound` on the
  [`throughput-demand-floor`](cycle-log.md#throughput-demand-floor) line, or
  `wva_service_rate_per_replica{source="lower-bound"}`.

### `slo` (observe only)

Latency targets, and the analyzer that judges a model against them. **On this
release it reports and does not decide**: replica counts still come from the
saturation analyzer. A policy written now stays observe-only when enforcement
ships, because enforcing will need an explicit `slo.mode: enforce`. Use it to
see whether a tier meets an SLO before anything sizes a fleet from one.
Experimental: its figures have not been checked against a live router yet.

One thing about turning it on **can** change scaling: a tier that lists
`analyzers:` replaces the list it would inherit, and with it saturation's
`score` and `parameters:`, unless it writes them out (step 3 below).

**Before you start**

- [ ] The llm-d router (EPP) runs in the model's namespace and Prometheus
      scrapes it. It must publish `llm_d_epp_request_ttft_seconds` (for TTFT)
      and `llm_d_epp_request_streaming_tpot_seconds` (for TPOT); an older router
      without them leaves the SLO unjudged, with no error. Check that
      `sum by (model_name, target_model_name) (rate(llm_d_epp_request_ttft_seconds_count{namespace="<ns>", streaming="true"}[5m]))`
      returns the model (as `target_model_name`, or as `model_name` where that
      is empty), and for TPOT that
      `count(llm_d_epp_request_streaming_tpot_seconds_count{namespace="<ns>"})`
      returns a series.
- [ ] Clients **stream**. Only streaming requests are counted.
- [ ] For a TPOT target: clients send `stream_options.include_usage`, or vLLM
      runs with `--enable-force-include-usage`. Without it TTFT is judged and
      TPOT is not.
- [ ] Enough traffic. A window is judged from 5 minutes of completed streaming
      requests: at least 50 for TPOT, and `max(50, 1000 ÷ (100 − p))` for a
      TTFT percentile p -- 100 at p90, 200 at p95, 1000 at p99. Below that the
      model is never judged and no alert fires, on purpose.
- [ ] For the alerts: the controller is installed with
      `DEPLOY_ALERTING_RULES=true` ([Installing the alerts](prometheus.md#installing-the-alerts)).
      They are off by default.

**Turn it on**

1. **Save the model's current `Effective scaling policy` line**, to compare
   with in step 5. It is logged only when the resolved policy changes, by the
   leader, so read every controller Pod:

   ```bash
   kubectl logs -n <controller-ns> -l app.kubernetes.io/name=workload-variant-autoscaler --tail=-1 | grep 'Effective scaling policy' | grep '"modelID": "<model>"' | tail -1
   ```

   Nothing back (the line has rotated out): save the ConfigMap entry the model
   resolves to instead (step 3), and compare that in step 5.

2. **Find the tier the model is on, and pick the tier to change.** A model's
   tier is the one its ScaledObjects name (`scalingPolicy:`); with none, the
   tier `default` names in `defaultPolicy:`, and only with neither, `default`
   itself. The policy line shows `"(default entry)"` for both of the last two,
   so read `default` for a `defaultPolicy:` key. Then, each needing no
   restart:
   - the tier the model is on: add to it, and keep its own values;
   - a new tier, named by each of the model's ScaledObjects (step 4). A tier
     inherits from `default`, never from another tier, so on a model already
     on a tier, a new one drops every field that tier sets: copy them all into
     the new one, or add to the tier instead;
   - the `defaultPolicy:` tier, or `default` when there is none. This turns it
     on for every model that uses it, except a model whose tier or per-model
     override lists its own `analyzers:`, which replaces this list.

   If an entry in the map names this model in its body (a per-model override),
   it is applied after the tier and wins over it, `analyzers:` and `slo`
   included. Put the `slo` block and the `analyzers:` list there instead, or
   take them out of it.

3. **Write the tier, copying saturation's entry.** Read the ConfigMap first:
   `kubectl get configmap wva-scaling-policy-config -n <controller-ns> -o yaml`
   (or the namespace-local map, if the model's namespace has one). A tier
   inherits every field it leaves unset from `default`, the band included,
   with one exception: its `analyzers:` list replaces the inherited one whole,
   and with it saturation's `score` and `parameters:`. So copy saturation's
   entry: the tier's own where it lists one, `default`'s where it does not,
   and `score: 1` with no `parameters:` where neither lists one.
   Leave the band and the thresholds out: written here, they would stop
   following `default`. On the tier the model is already on, keep its own
   values as they are. Replace each `<...>` below; do not paste the example
   values:

   ```yaml
   slo-watch: |
     analyzers:
       - name: saturation              # copied, with its current score and parameters
         score: <current>
         parameters: <current>         # omit if the entry you copy has none
       - name: slo
     slo:
       tpot: {target: 100ms}                  # a mean
       ttft: {target: 2s, percentile: 95}     # a percentile, in whole percent
   ```

4. **Point the model at the tier** (a new tier only): set
   `scalingPolicy: slo-watch` in the trigger metadata of every one of the
   model's ScaledObjects ([Named Policy Tiers](#3-named-policy-tiers)).

5. **Check nothing moved.** Every field of the model's new
   `Effective scaling policy` line must match the one saved in step 1 except
   `slo`, which shows the targets, for example
   `tpot mean<=100ms, ttft p95<=2s (observe)`, and, for a new tier,
   `scalingPolicy`, which must read `slo-watch`: `"(default entry)"` there means
   step 4 did not take. `slo=off` means the tier did not apply: the controller
   logs `Scaling-policy entry rejected` for an entry it could not parse, and
   `Scaling policy not found` for a tier name with no entry, and the model
   then runs on `default`. Saturation's `parameters:` and `score`,
   `queueLengthThreshold`, the scale-to-zero settings and the hold are not in
   that line: compare those in the ConfigMap. Nor is the `analyzers:` list:
   confirm the tier still lists `saturation`. A list of `slo` without
   `saturation` holds the model where it is; check that
   `wva_model_scaling_blocked{model_name="<model>", reason="no-composite-signal"}`
   is absent and the log has no `no listed analyzer can size this model` line.

**See the result**

- Is it met? `wva_slo_observed_seconds` against `wva_slo_target_seconds`, per
  objective:

  ```promql
  max by (exported_namespace, model_name, objective) (wva_slo_observed_seconds)
    > on (exported_namespace, model_name, objective)
  max by (exported_namespace, model_name, objective) (wva_slo_target_seconds)
  ```

  returns the objectives missed in the last window; `wva_slo_attainment_ratio`
  is the share of requests whose TTFT met the target. An empty result is not a
  pass: a model missing from
  `max by (exported_namespace, model_name, objective) (wva_slo_observed_seconds)`
  was not judged (see "When it does not judge" below).
- The **SLO** row of the operational dashboard (collapsed by default): observed
  against target per objective, what it judged and why it did not.
- The series under [SLO Analyzer Metrics](prometheus.md#slo-analyzer-metrics),
  and, if installed, three alerts, `WVAModelSLOAttainmentLow` (TTFT),
  `WVAModelSLOMissed` (TPOT) and `WVAModelSLOUnjudged`.
  Their runbook is
  [A model misses, or is never judged against, its SLO](troubleshooting.md#a-model-misses-or-is-never-judged-against-its-slo).
- The first judged point comes a window plus its grace after you turn it on
  (7 to 35 minutes; see below), and again after every controller restart. No
  `wva_slo_window_requests` series at all after a few minutes: work through the
  checklist above, then the runbook.

**When it does not judge**

A window is not judged while it could hold requests no steady fleet could have
served; `wva_slo_excluded` says why. The router's window is 5 minutes, and a
grace follows it: the longer of 120 s and 1.5 times the fleet's longest mean
request, at most 30 minutes.

- **A wake from zero, or a replica starting.** Excluded for the window plus the
  grace after the transient ends. The exclusion is per objective: a prefill
  replica starting excludes TTFT and leaves TPOT judged.
- **After the analyzer could not see the model** (`unobserved`), for the window
  plus the grace: after a controller restart or leader change, after it was
  first configured, and after any cycle on which the engine did not analyze the
  model -- as it does not analyze a woken model whose Pods have no metrics yet.
- **Capacity that cannot be placed is judged, not excluded** -- at once for a
  Deployment, late for a LeaderWorkerSet. A Pod that cannot be scheduled is not
  "starting", so a scale-up of a Deployment blocked on quota or nodes shows up
  as misses from the next window. A multi-Pod LeaderWorkerSet group lists no Pod
  ages, so its pending replicas count as starting until they have been pending
  continuously for 30 minutes: a whole-node P/D scale-up blocked on nodes shows
  misses only after about 30 + 5 + the grace, some 45 minutes in the example
  below.

On a whole-node P/D model this is long. With a 4-minute replica start and a mean
request of 400 s (a ~6 000-token generation at ~60 ms per token, plus its
prefill and queue time), one scale-up
leaves the model unjudged for about 4 + 5 + 10 = 19 minutes, for TTFT on a
prefill or decode start and for TPOT on a decode start. A fleet that scales up
more often than that is never judged, and `WVAModelSLOUnjudged` says so. The
share of the last day a model was not excluded, for either objective, is
`1 - avg_over_time(max by (exported_namespace, model_name) (wva_slo_excluded)[1d:5m])`
(not the share judged: a window with too few requests is not excluded, and not
judged either).

**Judged attainment reads higher than what users saw.** The windows left out are
the ones around a scale-up, which are when misses happen, so attainment,
`WVAModelSLOAttainmentLow` and `WVAModelSLOMissed` describe the fleet in steady
state. To score what clients
got over a whole run, score their own per-request records.

**Rules**

- **TPOT is a mean** and takes no percentile; setting one is a validation error.
  It is the mean over requests of the router's per-request TPOT, so a target
  like "p90 TPOT under 50 ms" cannot be expressed: a mean of 40 ms can hide a
  tail well above 50.
- **TTFT is a percentile**, a whole number from 50 to 99, default 90. A
  fraction (`0.95`) or a decimal (`99.9`) is refused. Attainment is exact when
  the target is one of the router's bucket bounds and interpolated between them
  otherwise; the upstream router's bounds near common targets are 1, 1.25, 1.5,
  2, 3, 4 and 5 s (check your router version's). Between bounds, in a tail the
  interpolation reads attainment a little low and the percentile a little high,
  so pick a bound where you can.
- **Targets are Go durations** (`100ms`, `3s`). A bare number is refused rather
  than read as nanoseconds. Like every validation error, a malformed `slo` block
  drops the whole entry it is on.
- **Both halves are needed.** Targets without `slo` under `analyzers:` are read
  by nothing; the analyzer without targets has nothing to aim at. Either half
  alone is logged once as `SLO policy is half-configured`, and is not an error.
- **Keep `saturation` in the list.** The list replaces the inherited one, and
  saturation is what sizes the fleet. Left out, it still runs but sizes
  nothing, and since `slo` only observes, the model would hold where it is.
- **The block is replaced whole by an override**, not merged per objective: a
  per-model entry that sets only `ttft` has no `tpot`.
- **A model gets the tier through its variants** (each names one in its
  ScaledObject trigger metadata, see [Named Policy Tiers](#3-named-policy-tiers)).
  If they name different tiers, the alphabetically first wins for the whole
  model, and the conflict is logged.
- **A namespace-local scaling-policy map replaces the global one whole.** A tier
  defined only globally does not exist there; a model naming it falls back to the
  namespace's `default` and loses its `slo` block (reported as an unknown policy).
- **The name `slo` is reserved.** An external analyzer named `slo` is not run,
  and the controller logs so once.
- **It costs nothing where no policy lists it**: no router query and no
  `wva_slo_*` series.
- **Unknown keys are ignored**, as everywhere in this map: `quantile: 95` sets
  nothing and leaves p90. The `slo` field of the policy line is where to check.

**Details: what the router measures**

- **Only completed streaming requests count.** The router records a request when
  its stream ends, with the TTFT it had; a request shed, timed out or abandoned
  is never recorded, so attainment reads high under overload. A request is
  recorded when its stream ends, up to a whole generation after its first
  token; the value recorded is still its TTFT.
- **The router's TTFT includes its flow-control queue**; the engine's own TTFT
  does not.
- **Every priority band is pooled.** The TTFT percentile is over all of the
  model's streaming requests, so where the router's flow control queues a
  low-priority band on purpose, that queueing counts against the target.
- **LoRA adapters are counted under the adapter's name.** The router labels a
  request with the model it was sent to, which for an adapter is the adapter,
  so a base model is judged on its own requests alone, not its adapters'.
- **Non-streaming requests are not counted**, and TPOT only counts responses
  that carry `usage`. `wva_slo_window_requests` is the judged count; nothing
  publishes the share of the model's traffic it is.

**Details: what the TPOT target would ask of each variant**

Phase 0 also works out, per decode or aggregated variant, what running at the
TPOT target would mean. These are estimates for whoever evaluates enforcing it,
not readings of the fleet:

- **ITL line**: the inter-token latency (ITL) a replica shows, fitted against its
  KV-cache fill, `ITL = A × fill + B`; *fill* is the share of physical KV cache in
  use. It is the same fit, with the same rules, the saturation analyzer makes.
- `wva_slo_fill` is the fill at which the line meets the TPOT target, clamped to
  [0.15, 0.80]; `wva_slo_predicted_itl_seconds` the ITL the fleet would show sized
  for it; `wva_slo_source` where the line came from (`own`: fitted; `pinned`: its
  intercept held at an earlier fit's; `pinned-default`: held at a default for no
  particular card). After a controller restart a line is `pinned-default` until
  the fleet spans enough fill to fit again -- on a whole-node replica, which
  rarely spans it, possibly for good. Read `pinned-default` as not a
  measurement: on an accelerator whose real floor is well above the default, it
  can call a target feasible that no fill meets. With no line: `no-line` (too
  few readings -- warming up, or replicas outside the band: idle below a fill
  of 0.15, or overloaded above 0.80) or
  `declined` (enough readings, and the fit refused them: most often a card whose
  ITL sits below the default intercept).
- `wva_slo_fill` is before any scale-up headroom: compare it with
  `kvCacheThreshold` (0.80 by default). Lower means the target needs more
  replicas than the tier gives. It is also lowered where `max_num_seqs` binds
  first, so the line's ITL there is at or below the target.
- Its demand and per-replica capacity are published as `wva_analyzer_demand` /
  `wva_analyzer_target` with `analyzer_name="slo"`, in output tokens per second.
  For one generating variant, `demand ÷ (target × scaleUpThreshold)` is the
  replica count the TPOT target alone would ask for; compare it with
  `wva_desired_replicas`. The target is `0` for a variant it could not price.
  With the tier's `scaleUpThreshold` (0.85 here):

  ```promql
  max by (exported_namespace, model_name) (wva_analyzer_demand{analyzer_name="slo"})
    / on (exported_namespace, model_name)
  (0.85 * max by (exported_namespace, model_name) (wva_analyzer_target{analyzer_name="slo"} > 0))
  ```
- **Known to be off** in two cases: speculative decoding (the engine's ITL is
  per step, not per token) and a high prefix-cache hit rate (the per-request KV
  footprint is under-counted). A pod running several data-parallel ranks, the
  normal wide-EP decode layout, is priced as all of its ranks: its batch cap is
  the ranks times `max_num_seqs`. The
  [proposal](../proposals/slo-tiers.md#before-phase-1-prices-anything) lists
  what is fixed before any of it sizes a fleet.
- **Prefill is not priced.** On a P/D model only decode variants get a figure,
  and TTFT is judged but not priced. Pods lent by a warm pool (bridges) are left
  out, so a variant served only by bridges reads `no-line` or
  `no-output-length`.
- Each cycle logs `slo-model`, `slo-variant` and `slo-transient` lines at
  verbosity 3: the judged figures; each variant's line (its key, A, B, the
  rows considered, and the readings the window kept and held) and the fill
  before the clamp; and why a window is excluded, since when, and for how long.
  The controller runs at `--v=2` by default, so none of these is logged until
  you add `- --v=3` under the container's `args:`
  (`kubectl edit deploy <controller> -n <controller-ns>`). The edit restarts
  the controller: its lines are learned again and the first windows read
  `unobserved`. So set it ahead of time, not during an incident:
  `wva_slo_excluded` alone says the reason, not since when.

### Default Configuration

The shipped `default` entry selects **V2** (token/capacity-based) via
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

Every field has a hardcoded default, so a missing ConfigMap (or `default`
entry) degrades gracefully instead of pinning the analyzer at zero thresholds.
Still deploy the ConfigMap with a `default` entry, so the values are explicit and
reviewable.

## Multi-Analyzer Registration

The V2 engine carries an analyzer registry so external analyzers (e.g.
throughput) can be plugged in without the engine package knowing the concrete
type. The built-in `slo` analyzer is registered by the engine itself, and the
name is reserved. Each cycle the engine iterates every registered analyzer in
registration order and invokes its `Analyze` method.

> **How listed analyzers combine.** Every analyzer the policy lists sizes
> each variant: its demand divided by its per-replica capacity gives the
> replicas it needs. The model is sized to the largest of those needs, so
> scale-up follows the analyzer asking for most, and scale-down waits until
> every listed analyzer agrees. Demand stays in saturation's units:
> saturation runs for every model, listed or not, and when it is not listed
> it only supplies the units and the variants. A policy with no `analyzers`
> section lists saturation alone. `slo` never sizes anything: it only
> observes.
>
> - **Nothing listed can size a role.** With saturation unlisted, the role
>   holds where it is: no scale-up, no scale-down. On a P/D model the other
>   role does not grow either. When that is every variant, the model reports
>   `no-composite-signal` ([Prometheus](prometheus.md#wva_model_scaling_blocked));
>   when it is one role, only the log says so (`composite holds a role`).
> - **A listed analyzer misses a cycle.** For up to three cycles after its
>   last good reading (45s at the default 15s `GLOBAL_OPT_INTERVAL`), the roles it covers
>   do not scale down, so one missed scrape from the analyzer asking for
>   most replicas does not shrink the fleet. Scale-up still follows the
>   others. Saturation's own grace works as it always has.
>
> The decision is the `CompositeSignal` analyzer in `wva_analyzer_*` and in
> the cycle log, and each held role logs `composite holds a role`.

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
post-merge. Either way saturation alone sizes the model.

> **Upgrading a policy that lists `throughput` or an external analyzer.** Each
> listed analyzer now sizes the model, where before only saturation did and the
> others were published and ignored. Check these policies before you upgrade:
>
> - **Any `analyzers:` list without `saturation`**, including a per-model or
>   tier override that replaces the list. Saturation no longer sizes the
>   model. If nothing else in the list can size it, which is the case for a
>   list of `slo` alone, the model holds where it is. Add `saturation` to
>   every such list before you upgrade, unless you mean the others to size
>   the model alone.
> - **Saturation and another analyzer listed.** The model is sized to the
>   larger need, so the fleet grows when the other analyzer asks for more
>   than saturation, and shrinks only when both agree.
> - **Only the other analyzer listed.** It sizes the model alone. A role it
>   cannot size (for example prefill, when it reports decode only) holds
>   where it is: no scale-up, no scale-down. On a P/D model the other role
>   then does not grow either, since decode grown against a frozen prefill
>   is capacity prefill cannot feed. Add `saturation` to the list to keep
>   saturation sizing the model.
> - **`slo` listed beside `saturation`.** No change: `slo` only observes.
> - **Dashboards and alerts on `wva_analyzer_*`.** These series gain
>   `analyzer_name="CompositeSignal"`. A panel or alert that sums across
>   `analyzer_name` now counts demand at least twice; filter it on
>   `analyzer_name`.
>
> To keep the old behaviour, list `saturation`, and remove the other entries
> or set `enabled: false` on them.

### AnalyzerScoreConfig Fields

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `name` | string | Analyzer name (must match a `RegisterAnalyzer` call) | required |
| `enabled` | bool | `false` keeps the entry but stops the analyzer running and sizing the model; on `saturation` it makes saturation unlisted (it still runs, for units and variants, and sizes nothing) | `true` |
| `score` | float64 | Weight in the cross-model fair share under a GPU limiter: the combined decision takes the largest score among the analyzers that sized the model this cycle (saturation's when none did). It does not change one model's replica count | `1.0` |
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

**Per-analyzer threshold overrides.** `analyzers[].scaleUpThreshold` / `analyzers[].scaleDownBoundary` are resolved per analyzer: a per-entry override takes precedence over the model-level global. The resolved pair is applied uniformly at model level and every role for that analyzer. Saturation's pair also sets the band of the combined decision, whose demand is in saturation's units. An opt-out flag for analyzers with non-universal calibration math is deferred to a follow-up.

### Saturation Always Runs

The saturation analyzer executes on every cycle, listed or not, and
`enabled: false` does not stop it: its `VariantCapacities` carry the variants,
`Cost` and `AcceleratorName` the optimizer needs for variant selection and GPU
accounting, and its demand is the unit every analyzer's need is converted into.
The optimizer consumes one combined decision (`CompositeSignal`): the largest
need among the listed analyzers, saturation included only when listed. `slo`
never contributes to it.

### Resilience

Each registered non-saturation analyzer runs in isolation: errors are
logged and discarded, and panics are recovered. A faulty plugin analyzer
cannot take down the optimize goroutine. While its last good reading is inside
the staleness window, the roles it covers do not scale down, so a run that
fails once does not shrink the fleet; after that it stops counting, and the
other listed analyzers size the model without it.

## Best Practices: Coordinating with InferenceScheduler (End Point Picker)

### What is End Point Picker (EPP)?

The **End Point Picker (EPP)** is the InferenceScheduler's request router. It
picks the replica for each request, scoring replicas on capacity metrics (KV
cache utilization, queue depth) and others.

### Deployment Architecture

Each model has **its own EPP instance**, which routes only to that model's
replicas. For example:

- `Qwen/Qwen3-0.6B` in `<your-namespace>` → EPP `gaie-workload-autoscaler-epp`
- `ibm/granite-13b` in `production` → EPP `gaie-production-epp`

(Names follow the namespace/workload convention.) So saturation detection and
routing are **per model**.

### Threshold Alignment Recommendation

**Use the same thresholds in the scaling manager and in each model's EPP.**
Aligned thresholds keep capacity management consistent and avoid dropped
requests:

1. **Fewer dropped requests.** The EPP stops routing to replicas the scaling
   manager already counts as saturated, instead of overloading replicas about to
   trigger a scale-up.
2. **One view of capacity.** Both judge replicas on the same criteria (KV cache
   utilization and queue length).
3. **Better GPU use.** The router respects the same capacity limits that drive
   scaling, so GPUs stay busy without oversaturation.
4. **Faster response.** Routing and scaling react to load changes together.

### Configuration Comparison

#### llm-scaling-manager Saturation Scaling Configuration

```yaml
# llm-scaling-manager Configuration (wva-scaling-policy-config ConfigMap)
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: <controller-namespace>
data:
  default: |
    analyzers:                    # selects V2 (the default)
      - name: saturation
        score: 1.0
    kvCacheThreshold: 0.80        # Should match EPP kvCacheUtilThreshold
    queueLengthThreshold: 5       # Should match EPP queueDepthThreshold
```

#### EPP Saturation Detector Configuration

The EPP detects overload with the [gateway-api-inference-extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/site-src/guides/epp-configuration/config-text.md)
saturation detector. Each model has its own EPP, so the thresholds are set **per
model deployment**, and models can differ by their traffic and SLOs.

```yaml
# EPP Saturation Detector Configuration (per-model EPP instance)
saturationDetector:
  ...
  queueDepthThreshold: 5          # Default: 5 - Backend waiting queue size threshold
  kvCacheUtilThreshold: 0.8       # Default: 0.8 - KV cache utilization threshold (0.0-1.0)
  ...
```

**Notes:**
- Every parameter is optional; omitted ones take the documented defaults.
- The EPP reads its configuration **only at startup**: restart the EPP pod after
  a change. Unlike the scaling manager, it has no live ConfigMap reload.
- **Each EPP instance** (one per model) can have its own thresholds.

### Parameter Mapping and Alignment

| Concept | llm-scaling-manager Field | EPP Field | Aligned Default | Description |
|---------|-----------|-----------|-----------------|-------------|
| **KV Cache Saturation** | `kvCacheThreshold` | `kvCacheUtilThreshold` | **0.80** (80%) | Replica is saturated when KV cache ≥ threshold |
| **Queue Saturation** | `queueLengthThreshold` | `queueDepthThreshold` | **5** | Replica is saturated when queue length ≥ threshold |

### Configuration Workflow

#### Step 1: Define Thresholds

Choose thresholds for your workload and SLOs:

| Workload Type | kvCacheThreshold | queueLengthThreshold | Rationale |
|---------------|------------------|----------------------|-----------|
| **Conservative** (Default) | 0.80 | 5 | Balanced performance and utilization |
| **Aggressive** (High GPU utilization) | 0.90 | 15 | Maximize GPU usage, higher latency variance |
| **Strict** (Low latency SLO) | 0.70 | 3 | Prioritize responsiveness, lower utilization |

#### Step 2: Apply to the scaling manager

Update `wva-scaling-policy-config` ConfigMap:

```bash
kubectl edit cm wva-scaling-policy-config -n <controller-namespace>
```

Changes apply **immediately** (the scaling manager watches the ConfigMap), except
on an out-of-reach controller (`WVA_WATCH_NS`), which reads it at start: restart
that one after the edit. Thresholds come from this copy in the controller's
namespace; `limiters:` and `optimizer:` do not, when a policy namespace resolves
(see [who is allowed to change the bound](gpu-limiter.md#who-is-allowed-to-change-the-bound)).

#### Step 3: Apply to EPP

Each model has its own EPP, so configure **each model's** EPP separately:

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
kubectl get cm wva-scaling-policy-config -n <controller-namespace> -o yaml
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
   - Use the scaling manager's per-model overrides for model-specific thresholds
   - Give the model's EPP the same thresholds
   - Document the threshold mapping for each model deployment
   - Example: if `ibm/granite-13b` uses `kvCacheThreshold: 0.85` in the scaling manager, its EPP must use `kvCacheUtilThreshold: 0.85`

3. **Testing Threshold Changes**:
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

Edit the installed ConfigMap and keep its `limiters:`. Keeping the `analyzers:` section
is recommended: it states the analyzer and its score explicitly, and it makes the
entry V2-shaped so the scaling band is defaulted on the entry itself (see
[Analyzer Selection](#analyzer-selection)):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: <controller-namespace>
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

Edit it in place:
```bash
kubectl edit configmap wva-scaling-policy-config -n <controller-namespace>
```

**Do not `kubectl apply -f deploy/configmap-scaling-policy.yaml`.** That sample has
no `limiters:` and names its own namespace, so applying it replaces the installed
`default` entry and silently removes the GPU limit the installer wrote. Edit the
installed ConfigMap instead, or re-run the install with your `WVA_LIMITER`.

Changes apply at once, with no pod restart: the controller watches the
ConfigMap, reloads its cache, and reconciles every discovered variant.

### 3. Named Policy Tiers

A tier is a reusable entry (`interactive`, `standard`, `batch`) that a variant
selects **by name** from its ScaledObject. A tier names no model, so one tier
serves many models and retuning it retunes them all. Per-model entries bind
settings to one model, so a fleet-wide change would be one edit per model.

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

A variant selects its tier in its KEDA trigger metadata. The trigger is already
the registration, so nothing needs to be watched, listed or matched:

```yaml
triggers:
  - type: external-push
    metadata:
      scalerAddress: wva-external-scaler.<controller-namespace>.svc:9090
      modelID: ibm/granite-13b
      scalingPolicy: interactive
    authenticationRef:      # TLS, the default. Plaintext install: ...svc.cluster.local:9090 and no authenticationRef
      name: wva-external-scaler-<controller-namespace>
      kind: ClusterTriggerAuthentication
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

Parking is stricter, because it takes the whole model to zero. Each variant's
tier has a say: a model parks only if every tier its variants name allows
`scaleToZero`, and it waits for the longest `retentionPeriod` among them. When
all variants name the same tier, this is just that tier's setting. When a tier
stops a model from parking, the controller logs `Scale-to-zero is disabled by
the scaling policy of a tier this model's variants name`, once per change, with
the model's `tiers` and the ones that disable it (`disabledBy`). Each tier is
resolved with the model's per-model entry merged in, so a per-model
`scaleToZero.enabled: false` disables every tier. The line then says
`disabledByPerModelEntry=true`, and its `fix` field says to enable it on that
entry. When no variant names a tier, `disabledBy` is `[default]` and `fix`
names the `default` entry.

A variant that names no tier follows the variants that do, for parking,
`requirePrefill` and the utilization-share weight, as it already did for the
thresholds. `default` (and its `defaultPolicy`) applies only when no variant of
the model names a tier.

Two failure modes resolve deterministically, and both are reported:

- **Unknown tier.** A `scalingPolicy` naming no entry falls back to `default`,
  and the controller logs it against the variants that asked, listing the tiers
  that exist: `Scaling policy not found; this variant is scaling under the default
  entry.` (once per change, with `scalingPolicy` and `knownPolicies` fields).
- **Disagreeing variants.** When variants of one model name different tiers, the
  lexicographically smallest wins, so every cycle and replica resolves the same
  way instead of flipping with map order. This picks the thresholds. Two
  settings look at every tier instead: the model parks only if every tier
  allows it, and it waits for prefill if any tier sets `requirePrefill: true`.
  The utilization-share weight comes from the same tier as the thresholds,
  for the planner and for a scale-from-zero wake alike.

### 4. Per-Model Overrides

Add model-specific configuration entries to override defaults for specific model/namespace pairs.

The engine finds a model's override by the entry's **body**: the entry whose
`model_id` and `namespace` equal the model's (`FindModelOverride()` in
`internal/config/scaling_policy.go`). The ConfigMap key is not part of the
lookup; any legal key works. If two entries name the same model and namespace,
the one with the lexicographically smallest key wins. With no match the model
scales under `default` (or its tier), and with no `default` under the built-in
defaults.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: <controller-namespace>
data:
  default: |
    analyzers:              # selects V2 (the default)
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

> **The `model_id` and `namespace` lines make this an override.** An entry whose
> body names no model becomes a named policy *tier* instead (`PolicyEntryKey` in
> `internal/config/scaling_policy.go`): it never applies to the model you meant,
> and any variant naming its key can select it.
>
> Identity cannot live in the key either: a ConfigMap data key allows only
> `[-._a-zA-Z0-9]`, so the `{modelID}#{namespace}` form earlier versions of this
> page showed (`"ibm/granite-13b#production"`) is rejected by the API server.
> `ModelOverrideKey()` suggests a legal, readable key.

**Key points:**
- An override entry is identified by its **body**: set `model_id` and `namespace`. Its ConfigMap key is arbitrary — pick something readable
- `model_id` must equal the ScaledObject's `modelID` exactly, and `namespace` the workload's namespace
- Overrides use **field-level merge**: only non-zero fields in the override replace the corresponding values from the layer below (the tier, then `default`); any field you omit (or set to its zero value) inherits from that layer. One exception: if the merged `scaleUpThreshold` is not above the merged `scaleDownBoundary`, both reset to the built-in 0.85 and 0.70. See `Merge()` in `internal/config/saturation_scaling.go`.
- Multiple overrides can exist for different model/namespace combinations

### 5. Per-Model Overrides — Field-Level Inheritance

Overrides are merged onto `default` field by field: only non-zero fields in the override
replace values from `default`, and any field you omit inherits from `default`. This
includes the band (`scaleUpThreshold`, `scaleDownBoundary`) and the `analyzers` list: an
override that sets only `analyzerName: saturation` keeps `default`'s band and analyzers.
To change just one threshold, specify only that field:

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
> - `disableShapeChangeHold: false` → inherits from `default` (cannot re-enable a hold `default` turned off)
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

A key the schema does not define (`scaleUpThreshhold`, `modelID`) is ignored
and the rest of the entry applies. It is logged, recorded as a
`ConfigEntryUnknownKeys` Warning event on the ConfigMap, and counted in
`wva_config_unknown_keys`.

A `scaleToZero.retentionPeriod` or `shapeChangeHoldSeconds` whose value cannot
be used is skipped and the rest of the entry applies. It is logged, recorded as
a `ConfigFieldIgnored` Warning event, and counted in
`wva_config_ignored_fields` (see [a bad value is skipped](#scaletozero)).

An entry with an invalid value or a wrong type is rejected. The entry
previously in force under the same key stays. Each rejection is logged,
recorded as a `ConfigEntryRejected` Warning event, and counted in
`wva_config_rejected_entries`.

At startup there is no previous entry. If the `default` entry of the ConfigMap
the limiters come from is invalid, the controller refuses to start (`refusing
to start unbounded`) rather than run without its GPU bound. That ConfigMap is
the policy-namespace one when a policy namespace is in use, otherwise the
controller's own.

So a bad `default` there is safe only until the next restart. While the
controller runs, the old entry stays in force. Once it restarts, there is no
old entry, and it will not start until you fix the entry. The
`ConfigEntryRejected` event and the log line both say so.

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

The controller caches the configuration and watches the ConfigMap for changes:

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

Reads are thread-safe (RWMutex) and return defensive copies, so callers cannot
modify the cache.

## Troubleshooting

### ConfigMap Not Found

**Symptom:** Warning log message
```text
INFO No cluster policy ConfigMap in the policy namespace: no limiters or quotas are in force
```

**Solution:** Re-run the install, which writes the ConfigMap with the limiter you pass:
```bash
NAMESPACE=<controller-namespace> WVA_LIMITER=<none|gpu-inventory|quota> make deploy-wva
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
   kubectl get cm wva-scaling-policy-config -n <controller-namespace> -o yaml
   ```

2. **Check controller logs for reload confirmation:**
   ```bash
   kubectl logs -n <controller-namespace> deploy/wva-controller-manager | grep -E "scaling policy|saturation config"
   ```

   Expected logs:
   ```text
   INFO  Updated global scaling policy from ConfigMap  entries=2
   INFO  Effective scaling policy  namespace=... modelID=... scalingPolicy="(default entry)"
   ```

3. **If no logs appear, verify watch is working:**
   - Check controller pod is running: `kubectl get pods -n <controller-namespace>`
   - Check for errors: `kubectl logs -n <controller-namespace> deploy/wva-controller-manager --tail=100`

4. **Manual restart (last resort):**
   ```bash
   kubectl rollout restart deploy/wva-controller-manager -n <controller-namespace>
   ```

### Cache Initialization Failed

**Symptom:** Warning on controller startup
```text
WARN Failed to load initial saturation scaling config, will use defaults
```

**Solution:** This is non-fatal. The controller continues with the analyzer's hardcoded defaults. To fix:

1. Re-run the install, which writes the ConfigMap with the limiter you pass:
   ```bash
   NAMESPACE=<controller-namespace> WVA_LIMITER=<none|gpu-inventory|quota> make deploy-wva
   ```

2. The watch mechanism will automatically reload the cache once ConfigMap is available

3. Verify cache loaded:
   ```bash
   kubectl logs -n <controller-namespace> deploy/wva-controller-manager | grep "Updated global scaling policy from ConfigMap"
   ```

## Example: Production Setup

**deploy/configmap-scaling-policy.yaml:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: <controller-namespace>
data:
  # Conservative defaults for most workloads (V2 analyzer — the default)
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

Merge these entries into the installed ConfigMap, keeping its `limiters:`:
```bash
kubectl edit configmap wva-scaling-policy-config -n <controller-namespace>
```

Verify deployment:
```bash
kubectl get cm wva-scaling-policy-config -n <controller-namespace>
kubectl describe cm wva-scaling-policy-config -n <controller-namespace>
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

