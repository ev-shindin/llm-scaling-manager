# Proposal: a utilization-share optimizer

**Status:** stage 1 (shadow) built; stage 2 (actuation) built for single-donor transfers, remaining pieces listed in §13; stage 3 is design.
**Created:** 2026-10-05
**Last Updated:** 2026-10-07

## Summary

A third optimizer beside `CostAwareOptimizer` and `GreedyByScoreOptimizer`, for
a fleet that owns a fixed GPU allotment (a namespace quota, or a cluster-scope
limit) and wants all of it used. When it is on:

1. **Every model first gets what it needs**: the GPUs that put it at its own
   scale-up threshold — the GPU form of the engine's `RequiredCapacity`, before
   the rounding and scale-down hysteresis the cost-aware optimizer adds.
2. **The rest of the quota is handed out as spare headroom**, so GPUs the quota
   grants are never idle while a model could use them. A model's **weight**
   decides its share: weight 2 gets twice the headroom of weight 1, relative to
   its size. When the quota is too small for everyone, a higher weight is cut
   less.
3. **Each cycle it compares desired against actual utilization**, and acts only
   when a model is outside a tolerance band (`±τ`). A rebalance is a *transfer*:
   one model scales down, another scales up.
4. **Scale-down goes first.** The receiving model is not scaled up until the
   donor's GPUs have actually been released — which, under the derived HPA's
   scale-down stabilization window, is minutes after WVA publishes the lower
   target, not the next cycle.
5. **It does not oscillate, and it says what it cannot follow.** Its moves
   land about twelve minutes after it decides them, so §6.7 builds it not to
   oscillate and measures the result in a simulator. Steady load produces no
   transfers; a demand step settles in the minimum number of moves, with none
   wasted; slow shifts are followed. Load that swings back within about 100
   minutes cannot be followed by moving GPUs, and there standing still does
   better. The rules limit that loss rather than pretend to remove it.

The two roles of a P/D model get the same headroom, so they run equally loaded
relative to their thresholds, and GPUs move between the two roles of one model
as readily as between models (§5.6). Replicas of many GPUs — an 8-GPU pod, a
multi-node LeaderWorkerSet — are funded per pod, on nodes where they fit (§6.5).
P/D and multi-GPU replicas are the normal case this optimizer is built for, not
an extension. `minReplicas` is honoured before anything is shared, including a
floor far above what the model needs (§5.5).

It runs on **quota-bounded groups** by default. A group bounded only by the
cluster's physical GPU inventory is opt-in, because "use the whole budget" there
means "take every GPU of that type in the cluster" (§7.3). Only models whose
variants span **more than one GPU type** keep today's sizing and are never moved
(§5.6).

The optimizer is configured in the installation's top-level scaling-policy
ConfigMap, beside `limiters:` (§8): whoever sets the budget also decides how it is
shared. That is the cluster admin on a cluster-scoped install and the tenant on a
per-tenant install.

## 1. Problem

Both existing optimizers size a model against its *own* demand:

- `CostAwareOptimizer` sizes every model independently at its scale-up / scale-down
  thresholds and ignores budgets.
- `GreedyByScoreOptimizer` does the same, then hands out **free** GPUs by
  fair-share value until the budget runs out. With rescale on, it redistributes
  by `priority × demand` — but **only when the group is contended**
  (`sumDemandGPUs > budget`, `applyRescale`). An uncontended group is left to the
  additive path.

So on a fleet with a 32-GPU quota and 12 GPUs of demand, 20 GPUs sit unused, and
when a burst arrives the model that gets it pays a full cold start on GPUs that
were idle a moment earlier. For an operator whose allotment is reserved and
already paid for — on-prem, a committed-use reservation, a team quota — an idle
GPU is pure loss and headroom is free. Priority today only matters at the moment
of scarcity; nothing says how the *spare* is shared.

## 2. Goals and non-goals

**Goals**

- Allocate the whole budget of each enabled group.
- Never **target** a model above its scale-up threshold to give another model
  idle headroom while the quota could cover both in whole replicas (§5.1). This is a property of the
  targets, not of every moment: while a transfer is releasing (§6.3) the
  receiver runs short for the length of the donor's stabilization window. §6.4
  is how that window is kept short for the receivers it hurts.
- Share the spare by a weight a user can choose without a formula.
- Move GPUs only when the imbalance exceeds a stated tolerance, and never in a
  way that makes it worse (no ping-pong).
- Scale down before scaling up, and scale up only into GPUs that are actually
  free — never into a donor's pods that are still running out their
  stabilization window.
- Change nothing for a fleet that does not opt in.

**Non-goals**

- **Exceeding the quota.** The budget is the limiter's answer
  (`ResourceConstraints`); this optimizer spends it, it never widens it.
  Limiters stay constraints-only.
- **Claiming a whole cluster by default.** On a group bounded only by the
  physical inventory, spending the budget means holding every GPU of a type,
  leaving nothing for workloads WVA does not manage. That is opt-in per group,
  never the default (§7.3).
- **Cost minimization.** This mode deliberately trades cost for headroom. A
  fleet billed per GPU-hour that can return idle GPUs should not enable it.
- **Overriding `minReplicas`.** KEDA enforces `minReplicaCount` whatever WVA
  publishes; the optimizer accounts for floors and reports their cost, it cannot
  refuse them (§5.5).
- **Preemption beyond the HPA.** A donor gives up replicas only through the
  normal scale-down path (KEDA → HPA → Deployment/LWS), with its stabilization
  window and drain hook intact.
- **Cross-accelerator sharing** and **multi-accelerator models**, as in rescale:
  each `(accelerator type, budget scope)` group is closed.
- **Binding placement.** A quota counts GPUs and a node holds them. The
  optimizer frees GPUs where a receiver replica fits, possibly from several
  donors (§6.5). It does not pin the receiver's pods to that node; the
  scheduler places them.

## 3. Explaining it to users

The arithmetic in §5 is for implementers. A user is told this, and nothing more:

> Every model gets the GPUs it needs first, whenever the quota allows —
> enough to sit at its scale-up threshold. GPUs left over in the quota are handed out as **spare
> headroom**, so a model can absorb a traffic spike without waiting minutes for
> a new replica. **Weight decides who gets more of
> that spare: a model with weight 2 can absorb a spike twice as large, relative
> to its size, as a model with weight 1.** When the quota is too small for
> everyone, higher-weight models are cut less.
>
> **`minReplicas` wins over weight.** Replicas you pin are yours, and they count
> as your headroom: a model whose `minReplicas` already gives it more than its
> share gets no spare on top. Large floors are paid for by the other models in
> the same quota, so set them only for capacity you really need held.

Picking a weight:

| class | weight | use it for |
| --- | --- | --- |
| `standard` | **1** (default) | most models. Everyone at 1 means everyone gets the same relative headroom — a sensible fleet with no tuning at all |
| `important` | **2** | latency-sensitive, or traffic that spikes harder than the rest |
| `critical` | **4** | the few models that must not queue |
| `best-effort` | **0.5** | batch work: least headroom, cut most when the quota is short |

Start everything at 1 and change one model at a time. Users pick a **class name**;
the numbers are defined once in the top-level policy (§8) and can be retuned there
without touching any model.

Users are never promised a utilization or a percentage. How much spare exists
depends on the quota and on everyone else's traffic, so any promised figure would
be broken whenever the spare runs out. "Your weight's share of whatever spare
exists" is the only promise that always holds. What they get instead is the
**outcome**, reported per model in the same words (§9): *"can absorb +80 % before
scaling"*, or *"+0 % — the quota has no spare"*, or *"+540 % — set by
minReplicas, not by weight"*. A user sets a class, looks at the headroom it
bought, and adjusts. "+80 %" means 80 % more traffic before the model reaches
its scale-up threshold and asks for another replica — not 80 % more before it
saturates.

## 4. Definitions

The unit of allocation is a **role** of a model: `prefill` and `decode` for a
P/D model, the single synthetic role `both` otherwise. For a role `r` of model
`m` in a group with GPU budget `B`:

| symbol | meaning | source |
| --- | --- | --- |
| `w_m` | weight, `> 0`, default 1 | per-model scaling policy: `weightClass` or `weight` (§8) |
| `D_r` | demand, in the role's own analyzer units | `TotalDemand` for `both`; `RoleCapacities[r].TotalDemand` for a P/D role (includes the backlog floor) |
| `c_r` | capacity per GPU, same units | role's most cost-efficient live variant: `PerReplicaCapacity / GPUsPerReplica` |
| `d_r = D_r / c_r` | demand in GPU-equivalents at 100 % of **effective** capacity | derived |
| `k_r` | scale-up threshold | `NamedAnalyzerResult.ScaleUpThreshold` — the value the engine already resolved for this cycle, never re-derived |
| `N_r = d_r / k_r` | **need**: GPUs that put the role exactly at its threshold | derived; the GPU form of the engine's `RequiredCapacity` arithmetic |
| `G_r` | GPUs the role **holds** | `Σ_v HeldReplicas × GPUsPerReplica`, where `HeldReplicas` counts scheduled pods until their containers exit, terminating ones included (§6.3) |
| `x_r = G_r / N_r − 1` | **headroom**: the traffic spike the role absorbs before it needs to scale | derived; negative = short |
| `u_r = d_r / G_r` | utilization | computed here from `G_r`, per role. Not the engine's `Utilization` field, which is `TotalDemand / (ReplicaCount × PerReplicaCapacity)`: it excludes terminating pods and is in capacity units |
| `F_r`, `C_r` | floor and ceiling, GPUs | §5.5 |

Headroom and utilization are two views of one number: `u_r = k_r / (1 + x_r)`.
Users are shown headroom, because "absorbs +40 %" is what they care about; the
band test (§6.1) and the existing metrics speak utilization.

Using GPU-equivalents rather than raw demand is what lets models — and the two
roles of one model, whose demands are in different tokens — share one budget; it
is the conversion `roleDemandGPUs` already makes for rescale. Anchoring need at
`k_r` rather than at 100 % means "needed" is defined by the thresholds the
operator already configured, not by a new number.

**Two thresholds are already inside these numbers, and neither may be applied
twice.** `PerReplicaCapacity` is *effective* capacity — the median
`effectiveCapacity` of ready replicas, already discounted by the KV-cache
threshold — so "100 %" above is 100 % of effective capacity, not of the GPU.
`k_r` is the scale-up threshold on top of it, read from the engine's resolved
`ScaleUpThreshold`, exactly as `RequiredCapacity = D / scaleUp − supply` uses it.
Composing the two thresholds by hand is a pricing error this codebase has made
before; the optimizer reads both from the analyzer result and derives neither.

**`G_r` is GPUs held, not replicas counted.** `CurrentReplicas` comes from the
scale target's `status.replicas`, which drops the moment a pod is marked for
deletion — while that pod still holds its GPU through the drain hook and
termination grace. Every budget figure in this optimizer (`G_r`, the free budget,
the release check) uses `HeldReplicas` instead; see §6.3.

A role with demand but no GPUs (parked) has `x_r = −100 %`; a role with no demand
has no need and no headroom to speak of, and receives nothing above its floor.

## 5. The target allocation

### 5.1 Need first, then spare

Everything in §5 runs over **`B_net`**, the budget this optimizer plans over:
the limiter's `B` minus what WVA has set aside or does not plan — fixed
consumers, frozen models, the reserve and the warm pool's carve-out (§6.2). Where
§5 writes `B` it means `B_net`.

Let each role's **claim** be its need, raised to its floor and capped at its
ceiling, `N̄_r = min(max(N_r, F_r), C_r)`, and let `S = B_net − Σ N̄_r` be the
spare. (Floors count here because KEDA enforces them whatever WVA publishes; a
budget that covers every need but not every floor is not a budget with spare.
§5.5 case 3 is that situation.)

**When there is spare (`S ≥ 0`)**, every role gets its claim — at least its
need — and the spare is split so that each role's headroom is proportional to
its model's weight. Roles pinned at a floor or a ceiling take no share; the
formula below runs over the others.

That guarantee is about GPUs, and GPUs come in replicas. With 8-GPU replicas,
two roles that each need 10 GPUs and a budget of 24 have `S = 4`, yet whole
replicas allow only 16 + 8: one role ends a replica short of its need. So the
guarantee is stated where it holds. **Every role gets at least its claim
whenever whole replicas allow it**, that is when

```text
  Σ_r  g_r · ⌈N̄_r / g_r⌉  ≤  B_net        (each claim rounded up to whole replicas)
```

Otherwise a role may end up to one replica below its claim, and it is the
role the integer allocator (§5.4) finds worst off by weight. The continuous
formulas below are the targets the band is judged against (§6.1); the integer
allocator decides what is actually held:

```text
  x_r = s × w_m                 G*_r = N_r × (1 + s × w_m)
  Σ G*_r = B   ⇒   s = S / Σ (w_m × N_r)
```

**When the quota is short (`S < 0`)**, every role is cut below its need, and
the cut is proportional to the *inverse* of its weight, so a heavier model loses
less:

```text
  x_r = −t / w_m                G*_r = N_r × (1 − t / w_m)
  Σ G*_r = B   ⇒   t = −S / Σ (N_r / w_m)
```

`s` and `t` are one common factor each, chosen so the allocations add up to `B`.
The two regimes meet at `S = 0`, where every role gets exactly its need and the
weights have no effect: there is nothing to share.

**Worked through**, 16 GPUs, weights 2 / 1 / 1, demands 4 / 3 / 1 GPU-equivalents,
`k = 0.8`:

| model | `w` | need `N = d/k` | headroom `x = s·w` | `G* = N(1+x)` | `u = d/G*` |
| --- | --- | --- | --- | --- | --- |
| A | 2 | 5.00 | 0.8 | 9.00 | 0.44 |
| B | 1 | 3.75 | 0.4 | 5.25 | 0.57 |
| C | 1 | 1.25 | 0.4 | 1.75 | 0.57 |

`S = 16 − 10 = 6`, `s = 6 / (2·5 + 3.75 + 1.25) = 0.4`. A's headroom is exactly
twice B's and C's — that is the whole meaning of the weight. A ends up 22 % cooler
than B, but that is an *outcome* of how much spare there is, not a setting: with
12 GPUs the same weights give A 10 % cooler, with 10 GPUs nothing, and with 8 GPUs
A is cut 13 % below its need while B and C are cut 27 %.

Note that weights 2 : 1 : 1 do **not** split the 6 spare GPUs 50 / 25 / 25. A
gets 4 of them, because the split is by `w × N` and A also needs the most. That
is deliberate: headroom only absorbs a burst if it scales with the model's size.

### 5.2 Why weights split headroom, not utilization or GPUs

Three readings of "a model with weight 2 gets more" were compared on the fleet
above, at four budgets (utilization per model; **bold** = past its threshold
of 0.8 while another model in the same column sits below its own):

| budget | **headroom split (chosen)** | utilization ratio, `u ∝ 1/w` | budget share, `G ∝ w` |
| --- | --- | --- | --- |
| 16 | A 0.44, B 0.57, C 0.57 | A 0.38, B 0.75, C 0.75 | A 0.50, B 0.75, C 0.25 |
| 12 | A 0.63, B 0.71, C 0.71 | A 0.50, **B 1.00, C 1.00** | A 0.67, **B 1.00**, C 0.33 |
| 10 | A 0.80, B 0.80, C 0.80 | A 0.60, **B 1.20, C 1.20** | A 0.80, **B 1.20**, C 0.40 |
| 8 | A 0.92, B 1.09, C 1.09 | A 0.75, **B 1.50, C 1.50** | **A 1.00, B 1.50**, C 0.50 |

- **Budget share** is a quota, not a scaler: a fixed share regardless of demand
  saturates B while C idles at 0.25 on GPUs B needs. At 8 GPUs every model is
  past its threshold under the headroom split too — the quota is short for
  everyone — which is why that row has no bold in the first column.
- **Utilization ratio** — the first draft of this proposal — takes capacity a
  model *needs* and turns it into another model's idle headroom: at 12 GPUs there
  is enough for everyone, and it still pushes B and C to 1.00 so that A can sit
  at 0.50. Its ratio is fixed (A is always 50 % cooler) whether or not anything
  is spare, which is why priority 2 : 1 felt far too aggressive. Softening it
  with an exponent or a curve narrows the gap; it does not remove the failure.
- **Headroom split** never takes a model below its need while the quota covers
  everyone, and its effect scales with what is actually spare. No exponent, curve
  or utilization cap is needed.

### 5.3 Weights: normalized, bounded, named

- **Normalized by construction.** `s` and `t` absorb the scale of the weights:
  `20 / 10 / 10` allocates exactly like `2 / 1 / 1`. Only ratios matter, and
  adding a model to the group does not change how any existing pair compares. No
  separate normalization step exists, so none can be misconfigured.
- **Bounded, by a fixed range.** In a group shared between tenants, the spare
  is a finite prize, but one model with `weight: 1000000` could still take
  nearly all of it. Every weight is clamped into the range spanned by the
  class weights — `[0.5, 4]` with the defaults — so no two weights are ever
  more than 8× apart. There is no separate clamp setting: the classes *are* the
  range. The range does **not** depend on which models are in the group: a clamp relative to "the smallest weight present" would let one
  newly added `0.5` model change how every existing pair is capped, breaking the
  property in the previous bullet.
- **Named.** Users choose a class (`weightClass: important`); the top-level policy
  maps classes to numbers (§8). A numeric `weight` is accepted too, for
  installations where one owner writes all of them.
- **A new field, not `priority`.** Under `GreedyByScore` and rescale, `priority`
  means *ordering*. Giving the same field a different meaning under another
  optimizer is a surprise nobody finds until it bites. This optimizer reads
  `weight` / `weightClass` and ignores `priority`, and says so once in the log
  when a model sets `priority` without a weight.

### 5.4 Integer form

GPUs come in replicas of `GPUsPerReplica`. Rounding a fractional allocation to
whole GPUs and then reclaiming whole replicas (what rescale does) leaves a role
holding a target it cannot reach. Allocate replicas directly, by one score that
covers both regimes:

```text
  z_r = x_r / w_m     when x_r ≥ 0      (headroom, discounted by weight)
  z_r = x_r × w_m     when x_r <  0     (shortfall, amplified by weight)

start every role at its floor F_r                                  (§5.5)
loop:
    candidates = roles with N_r > 0
                 whose next replica fits the remaining budget
                 and has a variant with room below its ceiling
    if no candidates: stop                       (any leftover stays unallocated)
    pick the candidate with the LOWEST z_r
        (ties → higher w_m, then (namespace, modelID, role))
    add one replica of its most cost-efficient variant with room below its ceiling
```

A role with no demand (`N_r = 0`) has no `z` and is never a candidate: it
keeps its floor. A role whose next replica is larger than the remaining budget is
skipped rather than chosen, which is what makes the loop terminate when replica
sizes are mixed. Equalizing `z` is exactly §5.1: `z = s` in the spare regime, `z = −t` in the short
one, continuous through zero. Giving the next replica to the worst-off role is
the standard greedy for this kind of max-min objective; with equal replica sizes
it is exact, with mixed sizes it is within one replica. It needs no rounding pass,
is deterministic, and is `O(R log M)` with a heap.

Replica granularity is visible on small models: one replica of model C above
(need 1.25) is 80 % of its need, so C's headroom moves in steps of 80 %. Reports
show the integer outcome, not the continuous target.

### 5.5 `minReplicas`, including large ones

A floor is a promise made outside this optimizer and enforced outside it: KEDA
applies `minReplicaCount` whatever WVA publishes. The effective floor of a role is

```text
  F_r = Σ_v minReplicaCount_v × GPUsPerReplica_v
```

where `minReplicaCount` is the **ScaledObject's**, as discovery already carries
it (`registry/enrich.go` maps `so.Spec.MinReplicaCount` onto the variant's
`MinReplicas`). Not the VariantAutoscaling CR's, which is being removed, and not
the derived HPA's `minReplicas`, which reads 1 whenever `minReplicaCount` is 0
and so cannot tell a parked-capable variant from a one-replica floor. The ceiling
`C_r` is likewise the ScaledObject's `maxReplicaCount`.

**A floor counts toward the model's allocation; it is never added on top.**
`G*_r = max(F_r, its share)`. A floor below the share has no effect. A floor
above it *is* that role's headroom — user-declared rather than weight-earned —
and the role gets no spare beyond it. This is the rule users are told in §3.

Five cases:

1. **Floor below the share** — the common case. The floor is the greedy's
   starting point and has no further effect.

2. **Floor above the share, quota still covers everyone's need.** The role is
   *pinned* at its floor; the rest share what is left by weight.

3. **A large floor that eats into other models' need.** This is the case to
   design for, because it is easy to create: one `minReplicas` written for "keep
   plenty warm". 16 GPUs, the fleet above, C at `minReplicas: 8`:

   | model | need | `G` | headroom |
   | --- | --- | --- | --- |
   | A (`w = 2`) | 5.00 | 5 | +0 % |
   | B (`w = 1`) | 3.75 | 3 | **−20 %** (u = 1.00) |
   | C (`w = 1`, floor 8) | 1.25 | 8 | +540 % |

   C's floor holds 6.75 GPUs it does not need, and B pays. The quota covers
   every *need* (10 of 16) but not every *claim* (5 + 3.75 + 8 = 16.75), so the
   group is short by 0.75 GPUs and the shortfall lands on the unpinned models
   by inverse weight — on B more than on A. At `minReplicas: 12`, B falls to one
   replica at `u = 3.0`. The optimizer **cannot refuse the floor**;
   what it does is make the cost impossible to miss:
   - C is reported as `floor-pinned`, with **`floorExcessGPUs = 6.75`** — the GPUs
     its floor holds above its need. That is the price of the floor in the unit
     an operator can act on.
   - B, which is below its need, reports `quota-short` **naming the floor
     responsible** (`causedBy: <namespace>/C minReplicas`), so the owner of the
     starved model can see it is not their quota or their weight. A, at +0 %, is
     not short and reports nothing; its zero headroom is visible in its
     headroom series. The cause is carried on a Kubernetes **Event** on B's scale
     target (reason `QuotaShort`, message naming the cause) and in the per-cycle
     log line — not as a metric label, which would be unbounded.
   - A group whose floors hold more than half the budget above their need logs
     a WARN once per change.

   The bound on a floor is admission, not this optimizer: in a cluster-scoped
   install a per-namespace `ResourceQuota` caps what any tenant's floors can
   claim. On a per-tenant install the tenant pays for their own floors, which is
   the right owner.

4. **Floors exceed the budget** (`Σ F_r > B_net`). Nothing is shared and no transfer
   is planned; the group reports `floors-exceed-quota`. Some of those pods will
   not be admitted — the HPA enforces the floor and the `ResourceQuota` refuses
   it — which is a configuration conflict only the operator can resolve.

5. **A floor raised above the current count** (an operator just set
   `minReplicas: 4` on a role holding 2). The floor is owed now, ahead of weights
   and ahead of the band. Free budget pays first; if there is none, the planner
   opens a transfer **bypassing the band, the confirmation and the admission rule
   of §6.2**, taking from the roles with the highest `z` — never from a role at
   its own floor. The admission rule has to be bypassed too. A floor raised on a
   model already above its need does not improve anyone's `z`, so the rule would
   refuse every transfer and the HPA's new pods would stay Pending forever. The
   one constraint that survives is the donor's own floor. The HPA is
   already creating those pods, so the transfer's job is to release room for
   them; until it completes they may sit Pending — or, on a quota group, not
   exist at all. A `ResourceQuota` refuses the pod at admission, and the
   ReplicaSet controller retries with an exponential backoff (up to about 17
   minutes) that nothing re-queues when the quota frees. So an owed floor may
   wait well past the moment its hole opens. This is reported under
   `awaiting-release`, with the backoff named. It is the one receiver whose
   target WVA does not control and so cannot gate.

A pinned role is **excluded from the band test** (§6.1): it is off target by
construction and nothing may move it, so letting it trigger a rebalance would
fire every cycle and change nothing. A donor at its floor cannot give; if every
candidate donor is at its floor, the receiver reports `donors-at-floor` rather
than appearing to wait for a transfer that will never be planned.

### 5.6 P/D roles

Both roles of a model carry the model's weight, so equalizing `z` across all
roles in the group gives the two roles of one model the **same headroom** as a
side effect — the same fraction above their own thresholds. Nothing
role-specific is needed. That is the right default: a request needs both roles,
so the model's throughput is set by the hotter one, and headroom in the cooler
one is waste. Where prefill and decode thresholds differ, equal headroom means
each runs the same distance below its own threshold, which is more meaningful
than equal raw utilization.

Floors are per role, so a pinned prefill keeps its floor while decode is still
held to the model's headroom. Equal headroom is the goal *above* the floors, not a
reason to inflate the other role to match a pinned one.

**Transfers inside one model.** Because roles are the unit, the two roles of
one model are donor and receiver to each other like any two roles in the group.
A model whose traffic shifts from long prompts to long outputs moves GPUs from
prefill to decode through the same ledger, with the same scale-down-first rule
and the same per-pod holes. Both roles are usually LeaderWorkerSets or 8-GPU
Deployments of the same shape, which makes them each other's natural donor
(§6.5).

**The weak link is prefill demand, and the guard is that need is today's
sizing.** Equal headroom is only as good as the two `d_r` it equalizes, and
prefill's is the weaker signal. Measured on a P/D fleet, prefill queues on
*decode* back-pressure and shows little demand of its own. Three things keep
that from starving prefill:

1. **Need is computed from the same role demand the paired allocator sizes
   from today** — `RoleCapacities[r].TotalDemand` *after* the analyzer's
   throughput floor (`applyThroughputFloor`) and backlog floor — divided by the
   same resolved `ScaleUpThreshold`. So `N_prefill` is, by construction, the
   prefill size today's path would ask for, and whenever the quota covers every
   claim in whole replicas (§5.1) prefill gets at least that, with headroom on
   top. Headroom is
   only ever *added* to today's answer; it never replaces it. A unit test pins
   this: for every fixture, the optimizer's prefill target is at least
   `allocateForModelPaired`'s.
2. **The bottleneck role is served first.** When decode is short, prefill
   queues behind it, and decode's `z` is the lower of the two. The greedy gives
   decode the next replica, which is the replica that drains prefill's queue.
   This is the paired allocator's own reasoning — "spending the last GPUs on one
   side of a P/D fleet whose other side cannot follow buys no throughput" —
   expressed as a score rather than as a joint commit.
3. **An uninformative role freezes the model.** If either role's analyzer result
   is not live and informative, or the role is reported unmodeled, the whole
   model is frozen for the cycle (§6.1). An under-read that the analyzer *knows*
   about never reaches the allocator.

What this does not fix is a prefill demand that is wrong *and* reported as
informative. That is an analyzer problem, and it would mis-size prefill under
today's optimizer just the same. Shadow mode (§13) records each role's headroom
against its queue, so a systematic under-read shows up as prefill queues at
positive reported headroom before any transfer acts on it.

There is **no P/D-specific switch**. The fleet this is built for is P/D, so a
switch that takes P/D models out is the same as turning the optimizer off.
Rollback is `shadow: true` (keep measuring, stop acting) or removing
`optimizer.type` (§8).

**Fixed consumers.** Models this optimizer does not size — multi-accelerator
models, below — are sized by today's path and never give GPUs away. But they
must still be able to **grow** in a full quota. Otherwise, because this
optimizer drives idle GPUs to about zero, they would be frozen at their
current size. So:

- `B_net` subtracts each fixed consumer's **`max(held, today's-path target)`**,
  not what it holds now. The GPUs it is entitled to are set aside before anything
  is shared.
- When that target is above what it holds, the gap is funded **first from idle
  GPUs, then by transfers** from planned roles, with the same donor sets,
  scale-down-first and per-pod holes as any transfer. A fixed consumer is never
  a donor.
- **Its higher target is not published until the GPUs are there.** Today's path
  would publish it at once. On a full quota group its pods would then be refused
  at admission, and the ReplicaSet's create backoff could keep them out long
  after the transfer opens their hole. So the engine publishes a fixed
  consumer's target the way it publishes any receiver's: raised by the ledger at
  release, held at what is funded until then.
- A fixed consumer has no `z` — it is not sized by weight — so it is an
  **entitled receiver**, like an owed floor (§5.5 case 5): its transfers bypass
  the `z` admission rule, the actionable and confirmation rules, and the holds
  of §6.7, and they set no hold. The donor-side checks still apply: a donor is
  never taken below its floor or pushed out of band.
- Within one cycle, entitled receivers — owed floors first, then fixed
  consumers, then the reserve (§6.2) — draw on idle GPUs before any planned
  receiver, so the same idle GPU is never handed out twice.

**Multi-accelerator models** — variants on more than one GPU *type*, not
replicas of many GPUs — are the one shape that stays outside. They are the
fixed consumers above, sized by today's path, in every group they draw
from. Sharing across accelerator types is a non-goal (§2). Replicas of many GPUs
of one type are fully in scope (§6.5).

### 5.7 More worked examples

All from the integer allocator (one GPU per replica, `k = 0.8`, `minReplicas: 1`
unless stated). Headroom is the spike each model absorbs before it must scale.

**Everyone at weight 1**, 16 GPUs: A 8, B 6, C 2 — every model at `u = 0.50`,
headroom +60 %. The untuned fleet is fair.

**Weights 2 / 1 / 1**, 16 GPUs: A 9 (+80 %), B 5 (+33 %), C 2 (+60 %; C moves in
steps of 80 % of its need).

**B's demand doubles to `d = 6`**: target A 6 (+20 %), B 8 (+7 %), C 2. B is at
5 GPUs against a continuous target of `Ĝ = 8.4` — 3.4 GPUs short, far outside
its 1.26-GPU tolerance — so the planner moves replicas A → B. Each step passes
the admission rule of §6.2: the pair's lowest `z` rises (−0.33 → −0.20 → −0.07
→ +0.07) and A stays in band (6 GPUs against `Ĝ = 6.2`, tolerance 0.93). A
fourth step fails both tests: the lowest `z` would fall to 0.00, and A at 5 GPUs
would be 1.2 below its target, outside the tolerance. So it stops at three
transfers, one replica each. At most two transfers run at once (§8.4), so two
start in the first cycle, and the third waits until one of them reaches Done, about one
release time later. The reversal hold does not delay it, because the hold only
blocks the opposite direction.

**A short quota**: demands doubled (8 / 6 / 2, need 20) on 16 GPUs. A 9 (−10 %),
B 5 (−33 %), C 2 (−20 %). The heavier model is cut least.

**A P/D model beside an aggregated one**: A (`w = 2`) prefill need 2.5, decode
need 5; B (`w = 1`) need 3.75; 16 GPUs. A prefill 4 (+60 %), A decode 7 (+40 %),
B 5 (+33 %). A's roles are within one replica of equal, and A as a whole holds
more headroom than B.

**Two P/D models on 8-GPU replicas** — the shape this is built for. A 64-GPU
quota, every role at `minReplicas: 1` (8 GPUs); A (`w = 2`) needs prefill 10,
decode 18; B (`w = 1`) needs prefill 6, decode 12. That is 46 GPUs of need but
48 of claims, because B's prefill floor of one 8-GPU replica exceeds its need of
6 (§5.1); 16 spare:

| role | need | `G` | headroom |
| --- | --- | --- | --- |
| A prefill | 10 | 16 | +60 % |
| A decode | 18 | 24 | +33 % |
| B prefill | 6 | 8 | +33 % |
| B decode | 12 | 16 | +33 % |

A's traffic then shifts to long outputs: prefill need falls to 6, decode rises to
26. The new target is A prefill 8, A decode 32, and B unchanged. That is one
transfer **inside model A**: one 8-GPU prefill replica released, then one 8-GPU
decode replica raised into the hole it leaves — the same shape, so it is valid
without combining donors (§6.5). Nothing moves between A and B.

At this granularity the weights show only coarsely. Eight replicas share 64
GPUs, and the 16 spare GPUs are two replicas, so they cannot express a 2 : 1
split precisely. The allocator gives each replica to whoever is worst off,
which is the best a two-replica spare allows. The larger the quota relative to
a replica, the closer the result tracks the weights; the half-replica band
(§6.1) is what stops the coarse steps from reading as permanent imbalance.

**A large `minReplicas`**: §5.5 case 3.

## 6. Each cycle

### 6.1 Measure and decide whether to act

For each enabled group:

1. Build `(w_m, d_r, k_r, G_r, F_r, C_r)` for every role of every model **with a
   live, informative signal**. A model whose composite signal is absent, stale
   or uninformative is *frozen*: it neither gives nor receives, and its current
   GPUs are subtracted from `B`. Taking GPUs from a model because its metrics
   went dark is the failure this rule exists to prevent. "Subtracted from `B`"
   here and below means from `B_net` (§6.2), once. A P/D model is frozen
   whole if either role's signal is not live, or either role is reported
   unmodeled. Multi-accelerator models are fixed consumers (§5.6): sized by
   today's path, subtracted from `B_net`, never planned.
2. Compute two targets, and judge every role, against its **committed**
   allocation `Ḡ_r`: current holdings adjusted by every transfer already in
   flight (§6.3). A receiver whose replica is still on its way already counts it,
   so a transfer started three cycles ago is not planned again. Judging the band
   on held GPUs instead re-plans every in-flight transfer every cycle until it
   lands — measured in §6.7 as the largest single source of oscillation:
   - the **continuous** target `Ĝ_r` (§5.1, water-filled over floors and
     ceilings), and its utilization `û_r = d_r / Ĝ_r`;
   - the **integer** target `G*_r` (§5.4), which only decides *which* replica
     moves.
3. A role is **in band** when either holds, and **out of band** otherwise:

   ```text
   |Ḡ_r − Ĝ_r| ≤ tol_r,   tol_r = max(τ · Ĝ_r,  ½ · g_r)     GPU distance  (τ = tolerance, default 0.15)
   |ū_r − û_r| ≤ 0.05    and  ū_r ≤ k_r                       idle floor    (a constant, §8.4)
   ```

   where `g_r` is one replica of the role, in GPUs, and `ū_r = d_r / Ḡ_r`. The
   idle floor applies only to a role that is not past its threshold. With a large
   spare every `û` is small, and a role far below its target could otherwise pass
   on the idle floor while it queues. The band is judged in **GPU
   space**, the space moves happen in. `τ` is the "±threshold percent"
   knob: 15 % of the role's target, in GPUs. The idle floor stops idle models
   from churning: `0.02` against `0.03` is a large relative miss of no
   consequence. The test is per role, so a P/D model whose decode is hot and
   prefill cold is out of band even if its average is on target. Pinned roles
   (§5.5) are excluded.

   **The tolerance is never smaller than half a replica.** With replicas of 8 or
   16 GPUs, a role's continuous target often falls between two integer sizes
   that are far apart. Take a decode role with `Ĝ = 12` and 8-GPU replicas: it can
   hold 8 or 16, never 12. Both are 4 GPUs away, and a 15 % tolerance (1.8 GPUs)
   would call either size out of band every cycle — a deviation no move can fix,
   reported forever. With `tol_r = max(1.8, 4) = 4`, both 8 and 16 are in band,
   and 0 is not. A role is out of band only when it is more than half a replica
   from its target, a distance one move can actually close. (An earlier draft
   stated this in utilization space, `u/û`. That is asymmetric — 8 GPUs reads
   50 % off and 16 reads 25 % off for the same 4-GPU distance — and it did not do
   what it claimed.)

   **Why the continuous target.** The integer target moves in whole replicas,
   and on a small model one replica is a large fraction: C's need is 1.25
   GPUs, so one replica is 80 % of it. Two models with nearly equal `z` swap the
   last replica between them on a tiny change in demand, and a band measured
   against the integer target would read that as a 50 %+ deviation on whichever
   one lost it — every cycle the tie flips. The continuous target does not jump:
   C at 2 GPUs against `Ĝ = 1.75` is 0.25 GPUs off, inside its half-replica
   tolerance. The band and the admission rule below use the **same in-band
   predicate** against `Ĝ`, so they cannot disagree about whether a move
   helps.
4. A role is **actionable** when it is out of band **and** its committed
   allocation differs from its integer target (`Ḡ_r ≠ G*_r`). A role already at
   its integer target can be out of band only through replica granularity, and
   no move can improve it. Before this rule, one such role — model C, 2 GPUs
   against a target of 1.4 — kept the whole group "out of band" forever and
   licensed a stream of A ↔ B moves (§6.7).
5. Act only if some role has been actionable for two consecutive cycles (a
   constant, §8.4), and plan only transfers that **involve a confirmed
   actionable role**, as receiver or as donor (§6.2). One role's deviation never
   licenses a move between two others. The demand feeding `d_r` is already
   windowed by the analyzers; the confirmation is a second, cheap guard against
   one noisy cycle. A floor that is owed (§5.5 case 5) skips the band, this wait
   and §6.7's holds.

### 6.2 Plan transfers

When the group acts:

- **Receivers** are roles with `Ḡ_r < G*_r`, lowest `z` first.
- **Donors** are roles with `Ḡ_r > G*_r` and `Ḡ_r > F_r`, highest `z` first. A
  donor need not itself be out of band — it is chosen because its target is
  lower. The band decides *whether* to
  rebalance; the target decides *who* pays.
- **Every transfer involves a confirmed actionable role** (§6.1 step 5), and
  respects the reversal hold (§6.7). A role that gave cannot receive, and a role
  that received cannot give, within the reversal hold (two release times, §8.4)
  of that transfer's start, or
  of its cancellation. The hold blocks **transfers only**: a role on hold that
  falls below its need still draws on idle GPUs and the reserve. Urgent
  receivers are not exempt from it. Simulated, an exemption changed no
  scenario (a role that just gave rarely bursts within the hold), and it would
  reopen the reversal path the hold closes. A swinging role is planned on its
  mean need (§6.7 rule 5).
- **Entitled receivers** — an owed floor (§5.5 case 5), a fixed consumer below
  its today's-path target (§5.6), the reserve's refill (below) — are funded
  first. They have no `z`, so they bypass the `z` admission rule, the actionable
  and confirmation rules, and the holds, and they set no hold. The donor-side
  checks still apply. Without that exemption an owed-floor donor taken from a
  role in hold could not be refilled for two release times. Only entries in
  Releasing or Filling count against the concurrency limit; a Planned entry
  publishes nothing.
- **Free budget first.** GPUs in the budget that nobody holds go to receivers
  immediately, with no transfer and no wait. Free is computed here, from held
  GPUs and the ledger — **never** from the limiter's `Used`:

  ```text
  B_net = B − (frozen models' held) − (fixed consumers' max(held, target)) − reserveGPUs − warm-pool carve-out
  idle  = B − (all WVA pods' held GPUs) − H_other − P − reserveRoom − pool carve-out not yet held
  free  = idle − (entitled receivers' gaps: owed floors, fixed consumers below target, reserve debt)

    B        the limiter's budget for the group, as ResourceConstraints report it
    B_net    what this optimizer plans over (§6.1 step 1, §7.2); every term subtracted
             here is WVA's, and is subtracted ONLY here
    G_r      GPUs held by the PLANNED roles, terminating pods included — fixed consumers,
             frozen models and warm-pool pods are not in this sum, because B_net
             already excludes them
    H_other  GPUs consumed from the same budget by pods WVA does not manage:
               physical group — gpuusage's scheduled GPU pods on the group's nodes, minus
                                every WVA pod (planned roles, fixed consumers, frozen
                                models, warm pool)
               quota group with a ResourceQuota on the GPU resource — the quota's
                                status.used minus every WVA pod, clamped at 0; the
                                namespace's other pods consume that quota, and a
                                receiver would be refused at admission for GPUs they
                                hold. status.used lags the quota controller by
                                seconds, which the fill timeout absorbs
               quota group without one — zero; WVA's declared quota is charged only
                                for what WVA holds
    P        promised GPUs: released by a donor, not yet held by the receiver's pods
  ```

  Every GPU lands in exactly one term. WVA's own non-planned consumers come
  out of `B_net`; WVA's planned roles are `G_r`; everyone else is `H_other`;
  in-flight transfers are `P`.

  `idle` is what nobody holds or has been promised — the GPUs a plan can hand
  out this cycle. `B_net` is what the shares of §5 are computed over. They
  differ by design: a fixed consumer's unheld gap is set aside in `B_net` (so
  planned roles' targets leave room for it), but it is not idle until someone
  frees it, and the gap is funded from `idle` first.

  **The reserve is a ledger of its own, not an inferred remainder.** A first
  draft inferred what was left of the reserve from a negative free figure. That
  counted a fixed consumer's unfunded gap as reserve spent, and planned refill
  transfers for a reserve nobody had used. Instead:

  ```text
  reserveDebt   += g   when a wake or an urgent receiver takes g GPUs from the reserve
  reserveDebt   −= g   when a refill of g GPUs lands
  reserveRoom    = reserveGPUs − reserveDebt
  ```

  `reserveRoom` is what a wake or an urgent receiver may still take. A positive
  `reserveDebt` is an entitled receiver, refilled after owed floors and fixed
  consumers. `free` is never negative; a deficit is attributed to entitled
  receivers in that order, and reported.

  Each GPU is counted once. While a donor is Releasing, its GPUs are still in
  `G_r` and are **not** also in `P`; at release they leave `G_r` and enter `P`;
  when the receiver's pods are scheduled they leave `P` and enter the receiver's
  `G_r`. Subtracting a Releasing donor's GPUs as both held and promised would
  under-state free by the size of every transfer in flight.

  The limiter's usage is `CurrentReplicas × GPUsPerReplica`
  (`gpuUsageByType`), and `CurrentReplicas` is `status.replicas`, which drops as
  soon as a donor's pod is marked for deletion. Read from there, a donor's
  still-occupied GPUs would show up as free in the very cycle the transfer
  starts, and "free budget first" would hand them to the receiver — bypassing
  the ledger and producing exactly the Pending pods §6.3 exists to prevent. On a
  namespace quota the fill is additionally bounded by the cluster's physically
  free GPUs (`fillable`), as in `applyRescale`.
- Fund the rest one **receiver replica** at a time. Each such move is one
  **transfer**: one receiver replica, funded by a **donor set** — one or more
  donor replicas, from one or more models, that open a hole for **each pod** of
  that replica, on a node that fits it whenever node information exists (§6.5).
  **A transfer is admitted only if both hold:**

  ```text
  min(z over receiver and every donor, after) > min(z over the same roles, before)   the worst-off role improves
  every donor is in band after the move, or still above its target (Ḡ'_d ≥ Ĝ_d)     no donor is pushed out of band
  ```

  "In band" is the predicate of §6.1 step 3, with the same half-replica
  tolerance, so an 8-GPU donor whose target sits between two sizes can still give.

  The first condition is the anti-oscillation rule: every admitted move strictly
  improves the worst-off role among those it touches, so no sequence of moves
  returns to an earlier state. The second stops a move that fixes one band
  violation by creating another, which is the residual churn the first
  condition allows when one replica is a large fraction of a donor. The first is
  judged in `z`; the second against the continuous target `Ĝ`, with the band's
  own predicate. A transfer for an owed floor (§5.5 case 5) bypasses both, and
  §6.7's holds; it may still never take a donor below its own floor.

  With demand held fixed, the first condition alone guarantees convergence: each
  admitted move makes the group's sorted list of `z` values strictly larger in
  the max-min order, a finite set of allocations cannot increase forever, so the
  planner stops. Demand is not fixed, and §6.7 is what keeps noise from
  restarting it every cycle.
- Bound the pace: at most two replicas given or received by any one role in a
  planning cycle, and at most two transfers in flight per group (constants,
  §8.4). A large
  rebalance completes over several cycles, each re-checked against fresh
  demand.

Variant choice reuses the existing helpers: a donor sheds through
`sortVariantsForScaleDown` / `scaleDownVariantSet` (most expensive first,
minReplicas and cheapest-at-1 protection); a receiver grows through
`sortByCostEfficiencyAsc` (cheapest per capacity first).

### 6.3 Execute: scale down first, scale up into released GPUs

This is the part with no precedent in the tree. A transfer is a small state
machine kept across cycles in a per-group **transfer ledger**:

```text
  Planned ──► Releasing ──► Released ──► Filling ──► Done
               │  │  │                       │
               │  │  │                       └─► Done (fill-timeout)
               │  │  └─► Releasing for a wake (claimed; the receiver returns to Planned)
               │  └─► Aborted (release timeout)
               └─► Cancelled
```

| state | what WVA publishes | exit condition |
| --- | --- | --- |
| **Planned** | nothing yet | the donor set is admitted this cycle → Releasing |
| **Releasing** | each donor's target lowered, its chosen pods marked with a low deletion cost (§6.5); receiver's target **unchanged** | every hole of the receiver's pod-to-node assignment is open, observed as GPUs **no longer held** on that node (terminating pods still count as held), or the set's GPUs released anywhere without node information → Released; demand reversal → Cancelled; release timeout → Aborted |
| **Released** | — | a bookkeeping step inside one cycle, never observed between cycles: the receiver's target is raised by one replica → Filling |
| **Filling** | receiver's raised target | receiver's new pods are **scheduled** — from then on they hold the GPUs and count in `G_r` → Done; still unscheduled past the fill timeout → Done with outcome `fill-timeout`. Not claimable: see below |
| **Cancelled** | donors' targets restored for pods not yet removed | terminal; the ledger entry is removed |
| **Aborted** | donors' targets restored | terminal; reported as `release-timeout` |
| **Done** | — | terminal; ledger entry removed; donor and receiver enter the reversal hold (§6.7) |

Only Releasing can be cancelled. Once the GPUs are released, a reversal is
handled by an ordinary plan next cycle, not by undoing the transfer.

**Why the receiver waits.** WVA's target reaches the pods through KEDA's HPA,
which takes the *maximum* recommendation over
`behavior.scaleDown.stabilizationWindowSeconds` (300 s by default). Measured on a
Kubernetes cluster, a ten-replica release took **420 s** from the first lower
target to the last pod gone ([A release the swap can survive](managed-keda-behavior.md) §1).
Raising the receiver in the same cycle would, on a full quota, create pods that
either sit `Pending` (physical inventory full) or are refused at admission
(`ResourceQuota` exhausted) — and a Pending replica is counted by the analyzers
as anticipated supply (`TotalAnticipatedSupply`), so the receiver would *look*
served while serving nothing.

**What counts as released.** Not `status.replicas`, for the reason given in
§6.2. Release is observed from the donor's pods: scheduled pods, including those
with a `DeletionTimestamp`, count as holding their GPUs until their containers
exit. The pod listing `variantmeta/discovery.go` already performs is the place to
count them, as a new `VariantReplicaState.HeldReplicas`; today that listing
*skips* terminating pods, which is the right answer for "how many are serving"
and the wrong one for "how many GPUs are held". For a LeaderWorkerSet the
listing must be extended. It matches the leader template's labels only, so
worker pods — which hold most of a multi-node replica's GPUs — are missed, and
it returns early for a group size above 1. Held GPUs, donor pods and the
per-pod holes of §6.5 all need every pod of a group, grouped by the
`leaderworkerset.sigs.k8s.io/group-index` label and matched by both the leader
and worker template labels.

**Committed allocation.** While a transfer is in flight the planner (§6.1 step 2)
treats every donor in the set as already holding its GPUs minus the replicas it
is giving, and the receiver as already holding one more replica. That is what stops the next cycle, which still sees the donor's pods
running, from planning the same transfer again or handing the promised GPUs to a
third model.

**Who else may use released GPUs.** While a transfer is Filling — released,
its receiver's pods not yet scheduled — the released GPUs are free on the nodes
and free in the quota, and other WVA components could see them. Two
other WVA components read exactly that: the warm pool grows into the headroom
published by `PublishNamespaceHeadroom`, and the scale-from-zero engine wakes a
model when `FitsGPUBudget` says it fits. They are treated differently, because
they want the GPUs for different reasons.

- **The warm pool may not take them.** The pool is speculative capacity, and a
  transfer's receiver is a model with measured demand. For an enabled group the
  headroom snapshot (`PublishNamespaceHeadroom`) is published **from this
  optimizer's `idle`, plus the pool's own carve-out it does not yet hold**
  (§6.2, §7.2), not from the limiter's `Used`. Publishing `idle` alone would
  hide from the pool exactly the GPUs set aside for it, and it could never
  reach its target. That one figure
  already excludes promised GPUs and counts terminating pods as held. The
  limiter's view gets neither right: `CurrentReplicas` drops when a pod starts
  terminating, so a donor's still-occupied GPUs would read as free to the pool.
  There is still one producer of "what is free"; for enabled groups it is this
  optimizer.
- **A wake may claim them, by the same rule as everything else.** A parked
  model with queued requests is serving nothing, and the scale-from-zero engine
  answers it faster than the next optimize cycle. Making it wait out someone
  else's transfer would turn a wake into a 300 s+ stall. So a wake may claim
  GPUs promised to a receiver **when the wake scores lower** — it is worse off
  by the measure that decided the transfer:

  ```text
  claim allowed  ⇔  z_wake < z_receiver(current holdings)        z_wake = −w of the parked model
  ```

  A parked `standard` model (`z = −1`) outranks any receiver of the same weight
  that still has replicas. It also outranks a `critical` receiver at −20 %
  (`z = −0.2 × 4 = −0.8`). A parked `best-effort` model (`z = −0.5`) does not
  displace that receiver; it waits for free GPUs, the reserve, or its own
  transfer. Ties go to the receiver, which was promised first. Whenever node
  information exists, the claim must also fit: the wake's replica has to fit in
  the holes the transfer opened, pod by pod (§6.5). Without it, each wake pod
  must fit in a hole left by one released donor pod at least its size.

  **A claim is taken while the transfer is still Releasing, never once it is
  Filling.** By Filling the receiver's target has been raised and its new pods
  are already queued at the scheduler, so they would win the holes. "Restoring"
  the receiver's target would not help either: a lower target is a scale-down,
  which the HPA holds for its whole stabilization window. So a claim
  **redirects** a Releasing transfer: the donor's release proceeds unchanged,
  the entry's receiver becomes the wake, and at release the *wake* is raised
  instead. The original receiver returns to Planned with its committed
  allocation removed, and the next cycle funds it from another donor set. A
  wake that arrives too late for any Releasing entry draws on idle GPUs or the
  reserve, or waits for a transfer of its own.

  Claims go through one place: the ledger store in `internal/decision`, the
  package that already carries one component's observations to another, behind
  one mutex. The scale-from-zero engine calls `Claim(namespace, accelerator,
  GPUs, z)` and receives the entries it redirected. That is how two components
  acting on the same transfer in the same second end with one of them holding
  it, not both. A claim sets no hold.

The order a wake draws from is: idle GPUs, then the reserve (§7.2), then a
claimable Releasing transfer. The warm pool draws from idle GPUs only.

**Cancellation is cheap until the first pod goes, and it has hysteresis.** If
during Releasing the donor's demand rises so that its reduced size would land
**more than twice its tolerance** below its target, or the receiver is now
**more than its tolerance above** its own target without the transfer, the
transfer is cancelled and the donor's target restored. The thresholds are deliberately wider than the
admission test's: a cancel rule that mirrors admission flips with demand noise
— plan, cancel, plan — and in simulation it kept a needed transfer from ever
landing (§6.7). A cancelled pair enters the reversal hold. While the
HPA is still holding the window's maximum, none of the donor's pods have been
touched, and the cancel costs nothing. That stops being true once the window
expires: the scale-down policies then remove pods step by step, and a cancel
after the first removal brings those replicas back **cold**. So cancellation is
decided per step: before the first pod of the transfer is removed, cancel; after
it, the transfer completes for the pods already gone and cancels only the rest,
and the donor's renewed demand is met by an ordinary plan next cycle.

**Timeouts.**

- **Release timeout** (derived: the donor's scale-down window + its pods'
  termination grace + 2 cycles, §8.4): the donor's GPUs did not come back. Abort, restore the donor's target, report
  `release-timeout`. The likely causes — a PodDisruptionBudget, a stuck
  finalizer, an operator who raised the ScaledObject floor — are outside WVA and
  must be visible, not retried forever. Built: the donor backs off one release
  timeout before it may give again. The back-off doubles with each consecutive
  abort, up to 16x, and resets on the first release that lands.
- **Fill timeout** (derived, §8.4 — Filling ends when the pods are *scheduled*,
  so this covers the scale-up's way through KEDA, the HPA and the scheduler, not
  model load): the receiver's new
  pods stayed Pending although the GPUs were released. Something outside WVA took them (another tenant on a physical pool,
  a non-WVA workload in the namespace), or the GPUs were released in the wrong
  shape (§6.5). Leave the receiver's target in place — the scheduler will place
  it when it can — but stop counting the transfer as committed, so the next plan
  sees the real state. The two causes are reported apart: `release-taken` when
  the hole was occupied by a pod WVA did not place, `release-shape-mismatch`
  when no pod took it but the receiver's pod still did not fit.

**Interaction with the sticky scale-down hold.** `scalingpolicy.HoldPublishedScaleDown`
holds a previously published lower target against a higher new one. A donor's
lowered target is the optimizer's own answer and must not be lifted by a later
cycle's ordinary decision; a cancelled transfer must be able to lift it. Both are
expressed by tagging transfer decisions with a new
`DecisionReasonUtilizationRebalance` and letting the hold treat that reason like
`WasLimited`: the ledger, not the hold, owns that variant's target while the
transfer lives.

**Controller restart.** The ledger must survive one, and nothing in memory
does. The published-target store (`internal/decision/store.go`) and
`lastDecided` (`engine.go`) are both in-memory, so after a restart there is
nothing to compare a donor's lowered target with. The first cycle's
demand-driven targets would restore every Releasing donor, and a Filling
receiver's Pending pods would be in neither `G_r` nor `P`, so their GPUs would
read as free and be handed out twice.

The ledger is therefore persisted **on the donor pods themselves**, which WVA
already patches. Both the cluster role and the tenant role grant
`patch` on pods (`config/base/rbac/manager-clusterrole.yaml`,
`config/components/tenant-installable/manager-role.yaml`), so this needs no new
permission. ConfigMaps would not do: WVA's roles grant only get, list and watch
on them, and WVA reads no HPAs.

- **When a transfer enters Releasing**, each chosen donor pod gets, beside its
  low `pod-deletion-cost`, an annotation
  `llm-d.ai/utilization-share-transfer` holding the transfer's id, its
  receiver (namespace and variant), its start time and its state's deadline.
  A redirected claim rewrites the receiver.
- **On start, the ledger is rebuilt from those marks.** Every pod carrying one
  is a Releasing donor, its variant's target is held at its running count minus
  the marked pods, and its receiver's entry is restored. Any **unscheduled** pod
  of a WVA variant above what it holds is counted in `P`. Nothing is scaled up
  into a donor's GPUs until they are observed free.
- **Staleness is judged per entry, against its own state's deadline**
  (the release timeout for Releasing, the fill timeout for Filling), not by the age of
  any record. A quiet ledger is not a stale one.
- **A mark only needs to live until its pod is gone.** Once a donor pod is gone
  its GPUs are observably free, and the mark has done its job.
- **A transfer in Filling has no mark left** — its donors are gone — and on a
  quota group its receiver may have no pods yet: the scale-up is still passing
  through KEDA and the HPA, or its pods were refused at admission. Nothing then
  lands in `P`, and the released GPUs would read as idle. So after a restart
  the planner **plans nothing — no transfer, no idle fill — for one fill
  timeout**. A Filling entry's receiver either gets its pods scheduled in that
  time, and they count as held, or its entry would have timed out anyway. A
  restart costs one fill timeout of planning, never a double-spend.

HPA `status.desiredReplicas` is no substitute: it is the *stabilized* value,
which during the window still equals the running count. A releasing donor
read that way looks steady, and the first cycle would restore it. Losing the
controller can delay a transfer; it cannot restore a releasing donor or
double-spend a promised GPU.

### 6.4 Receivers below their need

Goal 2 holds for targets; during Releasing it does not. A receiver that is
**below its need** (`x < 0` — past its threshold, queueing) is the case
that costs latency for the whole window, while the donor holds headroom it does
not need. Such transfers are marked **urgent**, and urgency buys three things, in
order:

1. **The reserve.** If `reserveRoom` (§6.2) covers a replica, the receiver is
   filled from it at once, and the reserve is refilled by the next plan (§7.2).
2. **The short window.** Urgent transfers are the first, and in stage 3 the only,
   ones for which the donor's stabilization window is shortened through
   `wvaOwnership` ([A release the swap can survive](managed-keda-behavior.md)).
   A Releasing entry for an urgent receiver is precisely the pending swap that
   proposal reasons about, with a better-defined trigger than the `WasLimited`
   it infers.
3. **Order.** Urgent transfers are planned before headroom-only ones, and
   the concurrency limit counts them first.

A transfer that only moves headroom (receiver already at or above its need) is
never urgent: it waits out the full window, which is the right price for a
change nobody is queueing on. The window remains the dominant cost of every
transfer, and the optimizer is correct with the full window — only faster with
the short one.

### 6.5 Funding one replica from several donors

A transfer moves **whole receiver replicas**, and a receiver replica can be
larger than any one donor replica. The optimizer therefore looks for **donor
sets**: one or more donor replicas, from one or more models, that together make
room for one receiver replica. What "make room" means is decided by the
receiver's **pods**, because GPUs are scheduled per pod, never per replica.

**A receiver replica is a list of pods.**

| receiver | its pods | each pod needs |
| --- | --- | --- |
| Deployment | one pod | `GPUsPerReplica` GPUs on one node |
| LeaderWorkerSet | the leader and `size − 1` workers | its own GPU request, on its own node |

An 8-GPU Deployment replica needs one 8-GPU hole. An LWS replica of 2 pods × 8
GPUs needs **two** 8-GPU holes. Those may be on different nodes, which is the
normal case for a multi-node engine. Sixteen GPUs scattered over sixteen nodes
satisfy neither.

**What a donor pod guarantees.** A pod's GPUs are always on one node, so
releasing a donor pod opens a hole of exactly its size, wherever it ran. That
fact needs no node information. Combining several donor pods into one larger
hole is a different claim: it holds only if those pods share a node, and two
pods of one Deployment, or of two models, usually do not. So:

| funding one receiver pod of `g` GPUs | valid when |
| --- | --- |
| one donor pod with `≥ g` GPUs | always — its GPUs are co-located by construction |
| free GPUs on a node, plus donor pods **on that same node** | only with node information that shows the co-location |
| several donor pods whose GPUs merely add up to `g` | **never** on GPU count alone |

**With node information: every receiver pod must fit on a node.** For a
candidate donor set, the hole on node `n` is

```text
  hole_n = free_n + Σ GPUs held by the set's donor pods on n
```

The set is valid only if every pod of the receiver replica can be assigned a
node whose remaining hole covers its GPU request. Two receiver pods may share a
node only if the hole holds both. Pod-level GPU requests and per-node capacity
are almost observable. `gpunodes` lists GPU capacity per node
(`DiscoverNodes`), and `DiscoverUsageByNamespace` already lists every scheduled
GPU pod with its `nodeName`, but it only returns totals per type and per
namespace. Per-node usage — and so `free_n`, capacity minus requests on the
node — needs a new per-node aggregation over that same listing (§11). WVA's own
pod listing gives each donor pod's node. This applies **whenever node information exists**, for a quota
group as much as for a physical one. A quota counts GPUs, but the receiver still
has to land on a node.

If the receiver LWS confines its group to one topology domain (the
`leaderworkerset.sigs.k8s.io/exclusive-topology` annotation, typically one
NVLink domain or rack), every one of its pods' holes must be in the same domain,
judged by that label on `NodeInfo.Labels`. A set that frees holes in two domains
is invalid for such a receiver, however many GPUs it frees.

**Without node information** (a quota-only installation that does not read
nodes, or a cycle in which node discovery failed), only the first row of the
table is usable. **Each receiver pod must be funded by one donor pod at least
its size**, and different receiver pods by different donor pods. Several smaller
donor pods are never combined, however many GPUs they add up to, because nothing
shows they share a node. A receiver whose pods are larger than every available
donor pod reports `no-compatible-donor` rather than being funded by a set that
probably cannot host it. The donor pod's surplus (an 8-GPU donor funding a 4-GPU
receiver pod) is not promised to anyone. It returns to the free budget, where
the next plan can use it. A receiver that still sits Pending past the fill timeout
(something outside WVA took the hole) reports `release-shape-mismatch`.

**Choosing the set.** The search is small. A replica has a handful of pods and a
group a handful of models.

1. Order the receiver's pods by GPU request, largest first.
2. For each pod, consider every node where `free_n` plus that node's candidate
   donor pods could cover it. Without node information, consider instead every
   single uncommitted donor pod at least the receiver pod's size. On each node take donor pods in order of their
   model's `z`, highest first, so the models with the most to spare give first.
   Score the node by the lowest `z` the group would be left with. Pick the best
   node, breaking ties by fewest donor replicas and then by the smallest hole
   that fits (best fit, so a large hole stays free for a large pod).
3. Commit that node's donors and hole to the set, and move on to the next pod.
   Donor pods and free GPUs already committed are not offered again.
4. If some pod cannot be placed, the set is invalid. The receiver reports
   `no-compatible-donor` for this cycle.

The admission rule of §6.2 then applies to the **whole set**: the lowest `z`
among the receiver and every donor must rise, and no donor may leave its band.
A set that must push a donor out of band to complete is refused.

**Donors are released per pod as well.** A Deployment donor frees one pod's
GPUs per replica removed. An LWS donor frees a whole group, meaning all its
pods at once, possibly on several nodes. LWS removes groups from the highest
index down and does not read a deletion cost, so only an LWS donor's
highest-index group is a candidate, and it contributes a hole on each node its
pods occupy. That makes an LWS donor the natural funder of an LWS receiver of the
same shape: one group out, one group's worth of holes in the same places.

**Steering which pod goes.** Lowering a Deployment's replica count lets the
ReplicaSet controller choose the victim, and it will not choose the pod on node
`n`. Before publishing the lower target, the optimizer marks each chosen donor
pod with `controller.kubernetes.io/pod-deletion-cost` below its siblings. The
ReplicaSet removes the cheapest first, the same lever the warm pool already uses
to keep a serving pod out of a shrink (`warmpool/pool/adapter.go`). It needs no new
permission: both WVA roles already grant `patch` on pods, which the warm pool
uses.

Deletion cost is not the ReplicaSet's first criterion. It ranks victims as:
unscheduled, then pending or unknown, then not ready, and only then by deletion
cost, then by co-location and age. A donor with a starting or not-ready pod
loses *that* pod first, wherever it is, and the planned hole never opens. Two
consequences:

- **Prefer donors whose pods are all Ready.** A donor with a not-ready pod is
  planned only as a GPU-count donor, funding a receiver pod on its own as one
  donor pod at least that size, never as part of a per-node set. Its victim is
  not ours to choose.
- **Check which pod actually went.** At release, the ledger compares the pods
  that disappeared with the pods it marked. If a different pod went, it does not
  wait for a hole that will never open; it re-plans the receiver from the holes
  that *did* open, and the donor's surplus returns to the free budget.

During a Deployment rollout, the old and new ReplicaSets are scaled
proportionally, and the deletion cost only ranks within each. Donors mid-rollout
are treated like donors with a not-ready pod.

**If an installation removes that grant**, a Deployment donor's victim cannot
be steered at all. It can then fund a receiver pod only on its own, as one donor pod at
least the receiver pod's size, because that is the only funding that is valid
wherever the removed pod ran. LWS donors are unaffected: their victim is fixed
by index, not by cost.

**The scheduler is not bound by the plan.** The holes are where the receiver's
pods *fit*, not where they are *sent*. If they are the only holes of that size,
the scheduler takes them. If something else lands first, a receiver pod goes
Pending, the fill timeout fires, and the report names `release-shape-mismatch` with
the node. The optimizer does not add node affinity to the receiver's pods
(§14).

**In the ledger**, a transfer is one receiver replica, its planned pod-to-node
assignment, and the donor set. Releasing ends when every hole in the assignment
is open, observed as GPUs **no longer held** on that node (a terminating pod
still holds its GPUs). Without node information it ends
when the set's GPUs are released anywhere. Cancellation is per donor, under the
rule in §6.3: donors whose pods are already gone stay gone. If the set is
cancelled after some donors released, their GPUs return to the free budget and
the next plan reuses them.

### 6.6 Where the ledger lives

The ledger must outlive a cycle, and today nothing in the optimizer path does:
`engine.go` assigns a **fresh** optimizer every cycle
(`e.optimizer = allocation.NewGreedyByScoreOptimizer()` or
`NewCostAwareOptimizer()`, lines 737-741), and `selectV2Optimizer` substitutes a
fresh `CostAwareOptimizer` on several fallback paths. State kept on the optimizer
instance would be gone by the next cycle, and with it the scale-down-first
guarantee.

So the **engine owns the ledger** (one per group, keyed by `(accelerator type,
budget scope)`), and the optimizer stays stateless: it receives the ledger as an
input to `Optimize` and returns the ledger changes it wants alongside its
decisions, which the engine applies after the enforcer.

**Ledger targets are applied by the engine, after whichever optimizer ran.** A
cycle can fall back to the cost-aware path (no constraints this cycle), and
that optimizer knows nothing of the ledger. It produces a demand-driven target
for every model, donors included, and a donor's demand-driven target is usually
*above* the lowered one the transfer published. Left alone, that would quietly
restore the donor mid-release. So the engine overlays the ledger every cycle: for
each variant the ledger owns, the published target is the ledger's, whatever the
optimizer said. In a fallback cycle no transfer advances or starts, and receivers
are not raised. Donors keep their lowered targets until a cycle with constraints
resumes the transfer, or the release timeout aborts it and restores them.

### 6.7 Why it does not oscillate, and what it cannot follow

Every move this optimizer makes lands long after it is decided. The
**decide-to-serve latency** is about twelve minutes with default settings:

- about a minute for the analyzers to see the change and a minute to confirm
  it (two cycles);
- the HPA's scale-down stabilization window (300 s) plus drain — the release;
- the receiver's pod start and model load (minutes for a large model).

A controller acting on that delay is a textbook oscillator. The admission rule
of §6.2 guarantees convergence only while demand holds still, and demand never
does. So the rules were tested in a simulator,
`hack/utilization-share-oscillation-sim.py`. Its run models six hours of
30-second cycles, a 300 s window, a 360 s release, a 180 s fill, and demand
seen 60 s late and averaged over 60 s. The demand patterns are steady with
noise, tied between two models, stepped, and swapping between models at
periods from 15 minutes to 4 hours, on 1-GPU replicas and on 8-GPU P/D
replicas. Every figure is a mean of five seeds.

#### The rules, and the failure each one removes

1. **Judge on the committed allocation `Ḡ`, not on held GPUs** (§6.1 step 2).
   A receiver whose replica is on its way looks just as short every cycle until
   it lands, so a planner judging held GPUs funds it again and again. Without
   this rule the stepped scenario on 12 GPUs produced 457 free cancels and
   ended *worse* than the rules as a whole (15.9 % shortfall vs 12.3 %); the
   tight P/D swing went from 13.2 % to 23.0 %.
2. **Move only for actionable roles, and only in transfers that involve one**
   (§6.1 steps 4–5). An actionable role is out of band *and* off its integer
   target. A role that is off target only through replica granularity — model
   C, 2 GPUs against a target of 1.4 — can never be fixed by a move. Before
   this rule it kept the whole group "out of band" and licensed moves between
   two other models every time noise flipped their integer targets.
3. **Cancel only on a clear reversal** (§6.3). While the HPA still holds the
   window's maximum, a cancel moves no pod, so a first draft cancelled whenever
   the admission test stopped holding. The two tests sat on the same threshold
   and noise flipped between them: plan, cancel, plan. Measured, that was 690
   cancels in six hours, and the transfer the step needed **never landed** — the
   receiver stayed exactly as short as if nothing had been done. With
   hysteresis, a transfer is cancelled only if its donor would now land more
   than twice its tolerance below target, or its receiver is more than its
   tolerance above target without it.
4. **Hold a pair from reversing** (§6.2). A role that gave cannot receive, and
   one that received cannot give, for two release times measured from the transfer's start, or from its cancellation. That is
   about one release time after it completes. The hold blocks only the opposite
   direction.
5. **Plan a swinging role on its mean need.** A role whose transfers change
   direction twice within eight decide-to-serve latencies (about 96 minutes) is
   following load it cannot catch. For the next such window it is planned on its
   **mean** need over that window, not its current need. This is the rule that addresses resonance, below.

#### Results

Cells: transfers landed / cancelled free inside the window / wasted (undone
within one release time of landing — the donor receives, or the receiver
gives, any replica in that time) / shortfall (GPUs below instantaneous need
that are actually serving, as a share of need — what makes requests queue).

| scenario | stand still | rules as first written | rules of this section |
| --- | --- | --- | --- |
| 1-GPU, 16 GPUs, steady load ±10 % | 0 / 0 / 0 / 0.0 % | 0 / 0 / 0 / 0.0 % | 0 / 0 / 0 / 0.0 % |
| 1-GPU, 16 GPUs, two models tied | 0 / 0 / 0 / 0.0 % | 0 / 0 / 0 / 0.0 % | 0 / 0 / 0 / 0.0 % |
| 1-GPU, 16 GPUs, B's demand doubles | 0 / 0 / 0 / 15.9 % | 2 / 581 / 0 / 3.6 % | **3 / 0 / 0 / 0.6 %** |
| 1-GPU, 12 GPUs, B's demand doubles | 0 / 0 / 0 / 17.8 % | 0 / 690 / 0 / 17.8 % | **3 / 0 / 0 / 12.3 %** |
| 1-GPU, 12 GPUs, A/B swap every 3 h | 0 / 0 / 0 / 5.5 % | 8 / 539 / 0 / 3.3 % | **16 / 0 / 0 / 1.4 %** |
| 1-GPU, 12 GPUs, A/B swap every 60 min | 0 / 0 / 0 / 5.5 % | 24 / 494 / 0 / 7.0 % | 22 / 0 / 0 / 7.1 % |
| 1-GPU, 12 GPUs, A/B swap every 30 min | 0 / 0 / 0 / 5.5 % | 46 / 364 / 0 / 11.0 % | 17 / 2 / 0 / 8.7 % |
| 1-GPU, 12 GPUs, A/B swap every 15 min | 0 / 0 / 0 / 5.5 % | 6 / 554 / 0 / 5.8 % | 1 / 24 / 0 / 7.8 % |
| 8-GPU P/D, 64 GPUs, steady load ±10 % | 0 / 0 / 0 / 0.0 % | 0 / 0 / 0 / 0.0 % | 0 / 0 / 0 / 0.0 % |
| 8-GPU P/D, 64 GPUs, A prompts ↔ outputs every 60 min | 0 / 0 / 0 / 1.8 % | 0 / 326 / 0 / 1.8 % | 7 / 0 / 0 / 1.9 % |
| 8-GPU P/D, 48 GPUs, A prompts ↔ outputs every 60 min | 0 / 0 / 0 / 12.3 % | 0 / 245 / 0 / 12.3 % | 9 / 0 / 0 / 13.2 % |

Under the rules of this section:

- **No oscillation.** Steady and tied load produce no transfers at all. In
  every scenario no transfer is wasted, and free cancels fall from hundreds to
  at most two dozen.
- **A real shift is followed in the minimum number of moves**, and the
  shortfall it was built for drops sharply: 15.9 % → 0.6 % on a demand step,
  5.5 % → 1.4 % on a three-hour swing.

#### What it cannot follow: load faster than its own latency

The bottom half of the table is the honest part. A swing that reverses
within about an hour cannot be followed by moving GPUs. The GPUs arrive after
the load has turned, and moving them takes away the slack that standing still
would have used to absorb the swing. Sweeping the swing period, with
shortfall weighted by model weight (in brackets, unweighted — the weights
deliberately short the lighter model first):

| swing period | stand still | without rule 5 | rules of this section |
| --- | --- | --- | --- |
| 15 min | 3.9 % (5.5 %) | 5.6 % (7.8 %) | 5.6 % (7.8 %) |
| 30 min | 3.9 % (5.5 %) | **13.4 % (14.0 %)** | 8.7 % (8.7 %) |
| 60 min | 3.9 % (5.5 %) | 8.4 % (8.3 %) | 7.1 % (7.1 %) |
| 90 min | 4.0 % (5.5 %) | 4.1 % (4.3 %) | 4.9 % (5.0 %) |
| 120 min | 3.9 % (5.5 %) | 2.3 % (2.6 %) | 3.5 % (3.7 %) |
| 180 min | 4.0 % (5.5 %) | 1.1 % (1.4 %) | 1.1 % (1.4 %) |
| 240 min | 3.3 % (4.6 %) | 0.6 % (0.8 %) | 0.6 % (0.8 %) |

Three things in that table matter:

- **Break-even is near eight decide-to-serve latencies.** Above about 100
  minutes moving pays, increasingly. Below it, standing still wins.
- **Without rule 5 there is a resonance.** At a period of about 2.5 latencies
  transfers land in anti-phase, and the shortfall is 3.4 times what standing
  still gives. None of those transfers counts as "wasted", because each lands a
  full release before it is undone. A waste counter alone would have called it
  healthy. Rule 5 cuts the peak to 2.2 times, at a cost of about one point at
  90–120 minutes.
- **What rule 5 does not do is win** below break-even. It limits the damage.

Alternatives that were measured and rejected, all available in the simulator:

| alternative | why not |
| --- | --- |
| **slow giving** — a donor gives only what it would still give at its peak need over a trailing window | duplicates the HPA window, which already holds a donor's pods for the whole window while a cancel inside it is free; costs shortfall wherever load really moves (1.4 % → 1.6 % on the 3-hour swing, 13.2 % → 14.5 % on the tight P/D swing) |
| **freeze a swinging role** instead of planning it on its mean need | holds the role wherever it happened to be: 7.1 % → 9.0 % on the 60-minute swing, 13.2 % → 16.0 % on the tight P/D swing |
| **move only to receivers below need** (no headroom-only transfers) | wins on the tight P/D swing (12.5 %) but drops the purpose of the optimizer — sharing spare by weight — and loses on the 3-hour swing (1.8 %) |

The real lever on fast swings is the latency itself. Shortening the HPA window
to 60 s for urgent transfers (§6.4, stage 3) cuts the decide-to-serve latency
from 12 to 8 minutes. In simulation that wins clearly at 120 minutes (1.0 %
against 3.9 % standing still) and cuts the loss at 60 minutes from 3.2 points to
0.7. It also makes 15 minutes worse (8.7 %), so it is no substitute for
rule 5. The other lever is the
optimizer's own purpose: spare headroom is exactly what absorbs a swing it
cannot follow.

The simulator is design evidence, not a test of code. It models one group,
single-variant roles, same-size replicas, and no floors above need, wakes,
fixed consumers or multi-donor sets. The implementation's tests (§12) re-run
its scenarios, and its ablations, against the real planner.


## 7. Interactions

### 7.1 Saturation and the existing analyzers

Sizing is by the composite signal's demand and the configured thresholds; nothing
in the analyzers changes. While the quota covers every role's claim in whole
replicas (need raised to its floor — §5.1), every model sits at or below its
threshold and the saturation path's own scale-up never fires.
When it does not, the shortfall is shared by inverse weight (§5.1) and reported as
`quota-short` (§9).

### 7.2 Scale-to-zero, scale-from-zero and the warm pool

A full quota has a consequence that must be designed for, not discovered:
**nothing is ever free.**

- **Scale-from-zero.** Judged against free GPUs alone, `FitsGPUBudget` would deny
  every wake, because the budget is spent. A parked role with demand has
  headroom −100 %, so it is the first receiver of the next plan, but a wake that
  waits for its own transfer waits out a donor's window, and a 300 s+ wake is a
  regression for any model that relies on scale-from-zero. A wake therefore has
  three sources, in order: idle GPUs; the reserve, below; and a transfer still
  Releasing for another receiver, which it may **claim** — redirect to itself —
  when it scores lower than that receiver (§6.3). Only if all three are empty
  does the wake become a transfer of its own. The **reserve**: `reserveGPUs` (default `0`;
  recommended: one replica of the largest scale-to-zero variant in the group) is
  held out of `B_net`, the scale-from-zero engine — and an urgent receiver
  (§6.4) — may spend up to `reserveRoom` of it immediately (§6.2), and the next
  plan refills it by transfer. A model
  with no demand receives nothing above its floor, so a model allowed to park
  still parks.
- **Parked models when the quota is short.** A parked role with demand sits at
  headroom −100 %, `z = −w`, and in a short quota that is usually the lowest
  score in the group. So it wins the next replica, and that replica is taken
  from a running model that is itself below its need. This is the weights doing
  what they say — one replica serving a model with none beats one more replica
  for a model that has some — but it means a wake under shortage makes another
  model's queue longer, and the reports say so: the donor's `quota-short` names
  the woken model as the cause, the same way §5.5 names a floor. An operator who
  wants parked models to wait instead gives them the `best-effort` class: at
  `w = 0.5` a parked role scores `−0.5` and yields to any model cut deeper than
  that.
- **Warm pool.** Pool pods are already charged to the quota (`addWarmPoolGPUs`).
  The two features compete by design. The pool grows only into free GPUs, and
  this optimizer drives free GPUs to about zero, so with nothing else said the
  pool could never grow once the optimizer is on. Instead, the pool's
  **configured target** is a carve-out like the reserve: `B_net` is the quota
  minus the larger of the pool's holdings and its configured target, and the
  unheld part of that carve-out is published to the pool as available (§6.3). The pool can
  reach its target from GPUs set aside for it. Growth beyond its target still
  needs genuinely free GPUs, which this mode rarely leaves. Operators who run
  both should size the pool's target deliberately; the optimizer will not
  donate headroom to it.
- **Scale-to-zero enforcement** runs after the optimizer
  (`applyScaleToZeroEnforcement`) and may still zero a model. Its GPUs come back
  as free budget next cycle.

### 7.3 Rescale and the limiters

- `ResourceConstraints` supply `B` — the limit, not the free figure. The
  optimizer spends the limiter's answer and never widens it, but it computes
  what is *free* itself, from held GPUs and the ledger (§6.2), because the
  limiter's `Used` does not count terminating pods. On a namespace quota the
  fill is still bounded by the cluster's physically free GPUs (`fillable`),
  exactly as in `applyRescale`: a quota can grant more than the nodes hold.
- When this optimizer is selected, `enableRescale` is ignored at every scope
  (rescale is a pass inside `GreedyByScoreOptimizer`, which is not running), and
  the engine logs that once per config change rather than every cycle.
- No finite budget → nothing to "use all of". A group whose budget is unlimited
  or unknown is handled by the cost-aware path, with a log line, as
  `selectV2Optimizer` already does when no constraints can be computed.
- **Quota groups by default; physical groups only on request.** A group's budget
  comes either from a quota (cluster- or namespace-scoped) or from the physical
  inventory alone. A quota is an allotment someone granted to WVA, and spending
  all of it is the point. A physical inventory is the cluster: spending all of
  it means WVA holds every GPU of that type, and every workload it does not
  manage — a training job, another team's service — goes Pending until a
  transfer or a scale-down happens to free a node. So the optimizer runs on
  physical-only groups only when `physicalGroups: true` is set in the top-level
  policy. Otherwise such a group keeps today's path (`GreedyByScore`). A
  namespace quota bounded *additionally* by the physical pool (`fillable`) is a
  quota group.

## 8. Configuration and who owns it

### 8.1 The optimizer block: the installation's top-level policy

The optimizer is configured in the **top-level scaling-policy ConfigMap — the one
that holds `limiters:` — and nowhere else**. It is read by the same rule as the
limiters, `effectiveLimitersLocked`: the separated cluster policy
(`clusterPolicy`, read from the policy namespace) when there is one, otherwise the
global map's `default` entry. It is **never** read from a namespace-local map.

Who that is depends on the installation, and the design holds for each:

| installation | who writes the top-level policy | what the optimizer shares |
| --- | --- | --- |
| cluster-scoped | the cluster admin | a cluster or per-namespace quota shared **between tenants** — weight clamp and classes matter |
| per-tenant (namespace-scoped) | the tenant | the tenant's own quota among **the tenant's own models**; isolation from other tenants is their `ResourceQuota`, not WVA |
| per-tenant, policy separated to an admin namespace | the admin | the tenant's quota, shared by rules the tenant cannot change |

The principle is one owner for the budget and for how it is spent. The limiter
decides how many GPUs a scope has; this optimizer decides which model gives GPUs
to which. Whoever may write one may write the other, and nobody else.

```yaml
# top-level scaling-policy ConfigMap (the one holding limiters:)
default:
  limiters:
    - type: quota
      name: team-quota
      # ...
  optimizer:
    type: utilizationShare          # absent → today's selection (cost-aware / greedy)
```

That is a complete configuration. Every other key is optional, and the common
case needs none of them:

```yaml
    utilizationShare:
      tolerance: 0.15               # the "±threshold percent": band in GPUs, as a fraction of the target (never under ½ replica)
      reserveGPUs: 0                # held out of the budget so a wake need not wait for a transfer (§7.2)
      shadow: false                 # true: compute, report and log everything, actuate nothing (stage 1, and the rollback)
      physicalGroups: false         # true: also run on groups bounded only by the physical inventory (§7.3)
      weightClasses:                # what users pick from (§3); these are the defaults
        best-effort: 0.5
        standard: 1                 # weight 1 is the default class
        important: 2
        critical: 4
      namespaces:                   # per namespace-quota group
        research:
          enabled: false            # this group keeps the greedy path
```

Six keys, each a decision only an operator can make: how far off target is
worth a move, whether wakes may skip the queue, whether to act at all, whether
WVA may hold every GPU of a type, what the priority classes are worth, and
which quota groups take part.

- `optimizer.type` is a selector, not a flag, so a later optimizer is one more
  value rather than another boolean beside `enableRescale`.
- `namespaces:` disables the optimizer for a **namespace-quota group**, and
  does nothing else. It lives in the same top-level ConfigMap, so on a
  cluster-scoped install it is the admin's decision about a tenant, not the
  tenant's decision about themselves. It cannot touch a cluster-scope group.
- An `optimizer:` block in a **namespace-local** map is ignored and reported once
  at WARN, so whoever wrote it learns it had no effect rather than assuming it did.

### 8.2 The per-model weight

The only per-model input, in the model's scaling-policy entry wherever that entry
resolves today (default → tier → override):

```yaml
my-model#my-namespace:
  weightClass: important     # preferred: a name from weightClasses
  # weight: 2                # or a number; at most one of the two (neither → the weight-1 class)
```

On a cluster-scoped install these entries can be tenant-written, which is why the
classes and the clamp live in the top-level policy: a tenant chooses *which* class,
the admin decides what a class is worth, and no number a tenant writes can leave
the range the classes span. An unknown class name is an error on that model's
entry only, falling back to the weight-1 class with a WARN.

A namespace's own scaling-policy map replaces the installation's for that
namespace, so whoever writes it sets every input its models are sized by:
`weightClass`/`weight`, `scaleUpThreshold`, and `kvCacheThreshold`, which sets
per-replica capacity. That map may be the tenant's. In the **cluster** group these
inputs would be weighed against every other tenant's: a threshold of 0.05
instead of 0.85 reads as 17x the need. Ignoring only the weight leaves that
open.

So a model whose namespace has its own map is **not planned in the cluster
group**. It is frozen there, today's optimizer keeps it, and its GPUs stay
outside the group's budget. In that namespace's own quota group, a budget the
tenant owns whole, it is planned with its own inputs; there they only order the
tenant's own models. To have such a model planned cluster-wide, an admin
removes the namespace's map or gives the namespace its own quota.

### 8.3 Validation must not cost the limiters

Today a malformed entry is rejected whole, and rejecting the `default` entry drops
its `limiters:` with it — the fleet goes unbounded
([gpu-limiter](../reference/gpu-limiter.md), "Confirm the controller accepted
it"). An invalid `optimizer:` block must not do that: it is validated on its own,
and on failure the optimizer falls back to today's selection with an ERROR naming
the field, while the limiters stay in force.

Rules: `0 < tolerance < 1`; `reserveGPUs ≥ 0`; class weights `> 0`, finite, and
exactly one class of weight 1 (the default class); a model entry setting both
`weight` and `weightClass` is an error on that entry; non-finite values are
rejected (`x <= 0` admits NaN); unknown keys and unknown namespace keys are
reported, not ignored silently; `type: utilizationShare` with no `limiters:`
declared is an error — there is no budget to share.

### 8.4 What is not configurable, and why

The values an earlier draft exposed as settings are not settings. Each is either a
fact the cluster already states, or a constant the oscillation guarantees of §6.7
were measured at. Exposing either kind would invite a value that looks
reasonable and quietly breaks those guarantees. The simulator showed how little
it takes: one rule changed reproduces each oscillation.

**Derived from the cluster** — recomputed per group, per cycle:

| value | derived from |
| --- | --- |
| release time | **measured**: the p90 of this group's completed releases (`wva_utilization_share_release_seconds`). Until enough have completed, the configured bound below |
| configured release bound | the donor's ScaledObject `spec.advanced.horizontalPodAutoscalerConfig.behavior.scaleDown`: the stabilization window (300 s if unset), plus the time its `policies` need to remove the replicas being given (a Pods/Percent policy per period paces a release past the window — why a ten-replica release measured 420 s, not 300), plus the HPA's 15 s sync and the KEDA `pollingInterval`, plus the donor pods' `terminationGracePeriodSeconds` from the scale target's pod template (leader and worker templates for an LWS) |
| release timeout | 1.5 × the configured release bound + 2 cycles — from configured values, an upper bound, so a release that is merely rate-limited is not aborted |
| reversal hold | 2 × release time, from a transfer's start or cancellation — the *measured* time, so a long drain grace that is rarely used does not stretch it |
| decide-to-serve latency | **measured** from the transfers the ledger has completed: decision to the receiver's pods Ready. Until one has, release time + 5 minutes of pod start and model load + 3 cycles |
| swing window | 8 × decide-to-serve latency |
| fill timeout | the KEDA `pollingInterval` + the HPA's 15 s sync + 2 cycles + 1 minute of scheduling — Filling ends at *scheduled*, so this covers the scale-up's way through KEDA and the HPA and the scheduler, not model load. For an LWS receiver, + 1 minute for gang scheduling |
| weight clamp | the smallest and largest class weights |
| default class | the class of weight 1 |

**Constants** — in one Go file, each cross-referenced to the §6.7 simulation:

| value | constant | why it is not a knob |
| --- | --- | --- |
| confirmation | 2 cycles | part of the decide-to-serve latency the other values assume |
| pace | 2 replicas per role per cycle, 2 transfers in flight per group | simulated at these values |
| idle floor | 0.05 utilization | stops idle models churning; not a preference |
| floor warning | floors holding half the budget above need | decides only when a WARN is logged |

If field data argues for a different constant, the simulator is re-run with it
first and the constant changed second. It does not become a knob.

**Every derived value is reported with its source.** One log line per group when
any of them changes, and a gauge
`wva_utilization_share_effective_seconds{param, source}` (`param` =
`release`, `releaseTimeout`, `reversalHold`, `latency`, `swingWindow`,
`fillTimeout`; `source` = `measured`, `scaledobject`, `pod`, `default`). An operator can always
see the value in force. That is what quietly disappears when a setting is
removed, and it must not.

## 9. Observability

The user-facing outcome is headroom, in the words of §3. New conditions become a
`reason` on `wva_model_scaling_blocked`, not new gauges.

| series | labels | meaning |
| --- | --- | --- |
| `wva_utilization_share_headroom` | model, `role`, `exported_namespace` | `x_r`: the spike the role absorbs before scaling (negative = short) |
| `wva_utilization_share_target_gpus` | model, `role`, `exported_namespace` | `Ĝ_r`, the continuous target the band is judged against, in GPUs |
| `wva_utilization_share_actionable` | model, `role`, `exported_namespace` | 1 when the role is out of band and off its integer target — a move could fix it (§6.1) |
| `wva_utilization_share_replicas_to_move` | `accelerator`, `scope` | replicas the integer target would move; in shadow mode, what would be planned |
| `wva_utilization_share_withheld_total` | `accelerator`, `scope`, `reason` | transfers not planned: `reversal-hold` (§6.7 rule 4) / `not-actionable` (§6.1 step 4). The swing rule withholds nothing; it changes the need a role is planned on, reported by `wva_utilization_share_swinging` |
| `wva_utilization_share_swinging` | model, `role`, `exported_namespace` | 1 while a role is planned on its mean need (§6.7 rule 5) |
| `wva_utilization_share_actual` | model, `role`, `exported_namespace` | `u_r` |
| `wva_utilization_share_spare_gpus` | `accelerator`, `scope` | `S`; negative when the quota is short |
| `wva_utilization_share_floor_excess_gpus` | model, `role`, `exported_namespace` | GPUs a floor holds above need (§5.5) |
| `wva_utilization_share_transfers_total` | `accelerator`, `scope`, `outcome`, `urgent` | `done` / `fill-timeout` / `cancelled` / `aborted` / `redirected` |
| `wva_utilization_share_reserve_debt_gpus` | `accelerator`, `scope` | `reserveDebt`: reserve GPUs spent and not yet refilled (§6.2) |
| `wva_utilization_share_effective_seconds` | `accelerator`, `scope`, `param`, `source` | the derived timings in force and where each came from (§8.4) |
| `wva_utilization_share_promised_gpus` | `accelerator`, `scope` | `P`: GPUs released for a receiver and not yet held by it — withheld from the warm pool, from wakes and from plans (§6.3) |
| `wva_utilization_share_release_seconds` | `accelerator`, `scope` | histogram, Releasing → Released |
| `wva_model_scaling_blocked` | `reason="awaiting-release"` | receiver waiting on a donor |
| `wva_model_scaling_blocked` | `reason="quota-short"` | below need; the cause (load, whose floor, or which woken model) is on a `QuotaShort` Event and in the log |
| `wva_model_scaling_blocked` | `reason="floor-pinned"` | floor above the share (§5.5) |
| `wva_model_scaling_blocked` | `reason="floors-exceed-quota"` | `Σ F > B_net` |
| `wva_model_scaling_blocked` | `reason="donors-at-floor"` | out of band, no donor can give |
| `wva_model_scaling_blocked` | `reason="no-compatible-donor"` | out of band, and no donor set opens a fitting hole for every pod of a receiver replica — without node information, no single donor pod is large enough (§6.5) |
| `wva_utilization_share_claims_total` | `accelerator`, `scope`, `outcome` | wake claims on Releasing transfers: `redirected` / `refused-score` / `refused-fit` / `none-releasing` |
| `wva_utilization_share_donors_per_transfer` | `accelerator`, `scope` | histogram: donor replicas funding one receiver replica |
| `wva_model_scaling_blocked` | `reason="release-shape-mismatch"` | GPUs released but the receiver's replica does not fit them (§6.5) |
| `wva_model_scaling_blocked` | `reason="release-taken"` | GPUs released, then occupied by a pod WVA did not place (§6.3) |
| `wva_model_scaling_blocked` | `reason="release-timeout"` | aborted transfer |

One `V(DEBUG)` line per group per cycle carries the whole table — `w, d, N, G, F,
headroom, u, û` per role, the band verdict, and the ledger — so "why did my model
lose a replica" is answerable from one line. Every transfer decision carries
`DecisionReasonUtilizationRebalance`.

## 10. Why a separate optimizer, not a rescale flag

The allocation is a weighted water-fill, as rescale's is. Everything around it
differs:

| | rescale | utilization share |
| --- | --- | --- |
| runs when | the group is contended | always, on enabled groups |
| weight acts on | the whole budget (`priority × demand`) | the spare above need, or the shortfall below it |
| per-model cap | demand | ceiling only |
| integerization | round GPUs, then reclaim replicas | allocate replicas |
| trigger | any difference from target | out of a tolerance band, confirmed over two cycles |
| state across cycles | none (reclaim frees "next cycle") | transfer ledger, owned by the engine |
| scale-up timing | free GPUs this cycle | released GPUs only |

Rescale's "reclaims free nothing until next cycle" is true of the quota accounting
(`CurrentReplicas`-based) and false of the cluster: the donor's pods live for the
length of the window, not one cycle. The ledger is the fix, and it is a different
shape of component — stateful, timed, cancellable — from a pass inside
`GreedyByScoreOptimizer.Optimize`. Sharing the pure helpers (variant ordering,
role records) and not the pass keeps rescale's behaviour pinned while this one
moves.

## 11. Where it lands

| change | site |
| --- | --- |
| optimizer, implementing `ScalingOptimizer` | `internal/engines/allocation/utilization_share_optimizer.go` |
| integer allocator (§5.4) | `internal/engines/allocation/utilization_share_target.go` |
| transfer ledger and state machine (pure types) | `internal/engines/allocation/utilization_share_ledger.go` |
| ledger ownership: one per group on the `Engine`, passed into `Optimize`, changes applied after the enforcer | `internal/engines/steadystate/engine.go`, `engine_v2.go` |
| headroom snapshot for enabled groups published from this optimizer's `idle` | `allocation.PublishNamespaceHeadroom` |
| ledger-owned targets overlaid on every cycle's decisions, whatever optimizer ran | `internal/engines/steadystate/engine_v2.go` (`optimizeV2`, after the enforcer) |
| `H_other` for physical groups: scheduled GPU pods on the group's nodes minus WVA's held | `internal/gpuusage` view, read by the optimizer |
| ledger store with `Claim(namespace, accelerator, GPUs, z)`, one mutex | `internal/decision` |
| wake draws free → reserve → claim | `internal/engines/scalefromzero` (`selection.go`, `candidates.go`) |
| donor-set search per receiver pod; LWS pod list and `exclusive-topology` domain | `internal/engines/allocation`, reading `gpunodes` (`NodeInfo.Labels`) |
| per-node GPU usage (new aggregation over the listing `DiscoverUsageByNamespace` already does), for `free_n` | `internal/gpunodes` |
| ledger persistence: `llm-d.ai/utilization-share-transfer` annotation on donor pods (existing pod `patch` grant); rebuild from marks and unscheduled pods | `internal/engines/steadystate` |
| oscillation guards of §6.7 | `internal/engines/allocation` (planner); evidence in `hack/utilization-share-oscillation-sim.py` |
| receiver and donor pod shapes (per-pod GPU request, node) | `internal/engines/variantmeta`, from the pod listing it already performs |
| mark chosen donor pods with `controller.kubernetes.io/pod-deletion-cost` (pod `patch` is already granted) | `internal/engines/steadystate` (actuation of a transfer) |
| `optimizer:` block (six optional keys), `weight` / `weightClass` on model entries, defaults, independent validation | `internal/config/saturation_scaling.go` |
| the constants of §8.4, in one file, each citing §6.7 | `internal/engines/allocation/utilization_share_constants.go` |
| derived timings (§8.4): measured release and decide-to-serve latency from the ledger; configured bounds from the ScaledObject and pod template | `internal/engines/allocation/utilization_share_timing.go` |
| extend `registry.Target` (today only `MinReplicas`/`MaxReplicas`) with `spec.advanced.horizontalPodAutoscalerConfig.behavior.scaleDown` (window and policies) and `pollingInterval`; read `terminationGracePeriodSeconds` from the scale target's pod template(s) | `internal/registry/enrich.go` (`TargetFromScaledObject`), `internal/utils/scaletarget` |
| LWS pods in the held/donor listing: worker pods and group identity (`leaderworkerset.sigs.k8s.io/group-index`), no early return for group size > 1 | `internal/engines/variantmeta/discovery.go` |
| top-level-policy-only accessor, read like `effectiveLimitersLocked`; clone in `UpdateClusterPolicy` | `internal/config/config.go` |
| `Weight` on `ModelScalingRequest` beside `Priority` | `internal/engines/allocation/optimizer_interfaces.go` |
| selection: generalise the GreedyByScore type check to a `ConstraintAware` interface | `internal/engines/steadystate/engine.go`, `selectV2Optimizer` |
| GPUs held including terminating pods | `internal/engines/variantmeta` → new `VariantReplicaState.HeldReplicas` |
| decision reason; hold stands down for it | `internal/domain`, `internal/scalingpolicy/sticky.go` |
| metrics | `internal/metrics` |
| user docs: §3 as the seed of the reference page | `docs/reference/` (configuration, scaling policy, metrics) |

The ledger lives on the engine, not on the optimizer: the engine builds a fresh
optimizer every cycle (§6.6).

## 12. Verification

- **Allocator, property tests:** `Σ G = B_net` whenever any role has demand and room;
  no role below its claim (need raised to floor) while the whole-replica
  condition of §5.1 holds, and no role more than one replica below it when it
  does not (fixture: two roles needing 10 on 8-GPU replicas with 24 GPUs); §5.5
  case 3
  as the fixture where `S < 0` although every need fits; the loop terminates
  with mixed replica sizes and a remainder smaller than any replica; a role with
  no demand stays at its floor; headroom ratio of two unclamped roles
  equals their weight ratio (within one replica); weights `×1000` allocate
  identically; monotone in weight; all-1 weights give equal headroom; the roles
  of a P/D model within one replica of equal headroom above their floors; floors
  and ceilings respected; deterministic under shuffled input.
- **Floors:** one fixture per case of §5.5. Case 3 asserts the starved model's
  `QuotaShort` Event on B names C's floor, A gets none, and `floorExcessGPUs`
  equals the excess;
  a pinned role never triggers a rebalance; `Σ F > B_net` plans nothing; a raised
  floor opens a transfer without waiting for confirmation; a donor at its floor
  is never selected.
- **Anti-oscillation:** a randomized sequence of demands, every admitted move
  strictly raises the lowest `z` among the roles it touches and leaves every
  donor in band; a
  two-model fixture where one replica overshoots the band produces no move; two
  models at near-equal `z` with demand jittering across the tie produce no
  transfer (the integer target flips, the continuous one does not). Negative
  controls: the same fixtures without the admission rule, and with the band
  judged against the integer target, both ping-pong.
- **Oscillation (§6.7):** every scenario of
  `hack/utilization-share-oscillation-sim.py`, and its period sweep, is re-run
  against the real planner with a fake clock, a release delay, a fill delay
  and lagged demand. Steady and tied load produce zero transfers; a demand step
  settles in the minimum number of transfers with none wasted; no scenario
  wastes a transfer; the 30-minute swing stays below 2.5 × standing still.
  Ablations, one per rule, must each reproduce the failure the rule removes:
  without `committed`, hundreds of cancels on the tight step; without
  hysteresis, a step whose transfer never lands; without the hold, wasted
  transfers on the 15-minute swing; without swing damping, the 30-minute
  resonance (> 3 × standing still).
- **Weights:** the clamp is fixed — adding a model with a new smallest weight
  changes no existing model's clamped weight.
- **Ledger, fake clock:** receiver never raised before the donor's held GPUs
  drop; a donor pod marked for deletion still counts as held, so the free budget
  does not rise until its container exits (negative control: computing free
  from the limiter's `Used` hands those GPUs to the receiver in the same cycle);
  cancel before the first pod is removed restores the donor, after it completes
  for the removed pods only; release timeout aborts; restart with a descending
  variant scales nothing up into it; the ledger survives the engine replacing
  its optimizer, and a cycle that falls back to cost-aware leaves it untouched.
- **Promised GPUs and claims:** while a transfer is Filling, the published
  headroom excludes its GPUs; neither the warm pool nor a wake can take them. A
  wake that scores lower than the receiver of a **Releasing** transfer redirects
  it: at release the wake is raised and the original receiver returns to
  Planned, and the donor is not restored. A Filling transfer is never claimable
  (negative control: claiming during Filling leaves the receiver's queued pods
  to win the holes). A wake that scores higher, or whose replica does not fit
  the holes, is refused. Two concurrent claims on one entry: exactly one wins.
- **Persistence:** after a simulated restart mid-Releasing, the ledger rebuilt
  from donor pod annotations holds the donor's target and restores the
  receiver's entry (negative control: rebuilding from HPA `desiredReplicas`
  restores the donor). A Releasing entry quiet for longer than a cycle is not
  discarded; it is discarded at its own release-timeout deadline. After a
  restart mid-Filling on a quota group, no transfer or idle fill is planned for
  one fill timeout (negative control: planning at once hands the receiver's
  released GPUs to another model).
- **Reserve and fixed consumers:** a fixed consumer's unfunded gap is never
  counted as reserve debt; reserve refill transfers happen only after a wake or
  an urgent receiver actually drew on the reserve.
- **Donor sets, per pod:** with node information, an 8-GPU Deployment receiver
  is funded only by a set that opens an 8-GPU hole on **one** node (given
  `free_n`), never by GPUs spread across nodes, on a quota group as well as a
  physical one. A 2 × 8-GPU LWS receiver needs two such holes, on two nodes or
  one node with 16 free. With `exclusive-topology`, both holes must be in one
  domain, and a set straddling two domains is refused. The planner picks the
  node with the best resulting lowest `z`, then best fit. A set that would push
  any donor out of band is refused. The chosen pods carry the lowest deletion
  cost, and the ReplicaSet removes them and not their siblings (kind e2e:
  envtest runs no ReplicaSet controller, so it cannot show this). An
  LWS donor offers only its highest-index group, with a hole on each of its
  nodes. Without node information each receiver pod is funded by exactly one
  donor pod at least its size. Two 4-GPU donor pods are **never** combined for
  an 8-GPU receiver pod (negative control: summing GPUs funds it, and the pod
  goes Pending), and a receiver larger than every donor pod reports
  `no-compatible-donor`. With node information, two 4-GPU donor pods fund it
  only when they are on the same node.
- **P/D:** for every fixture meeting the whole-replica condition of §5.1, each
  role's target is at least what
  `allocateForModelPaired` gives it (prefill included, after the throughput
  floor). When decode is short and prefill is not, the next replica goes to
  decode. A shift from long prompts to long outputs produces a prefill → decode
  transfer inside one model, through the ledger. A role reported unmodeled
  freezes the whole model. A multi-accelerator model (a fixed consumer) is never
  a donor **and still scales up** on a demand surge, by transfer if no GPUs are
  idle. Negative control: treating it as frozen leaves it at its size.
- **Large replicas:** an 8-GPU-replica role with `Ĝ = 12` is in band at 8 and at
  16 GPUs and plans nothing; at 0 GPUs it is out of band. Negative controls: a
  flat 15 % band reports it out of band every cycle, and the utilization-space
  form `u/û` reports 8 out and 16 in. An 8-GPU donor with `Ĝ = 12` going 16 → 8
  is admitted (negative control: a flat-τ admission refuses it). A 2 × 8-GPU LWS receiver is
  funded by one LWS donor group of the same shape.
- **Free figure:** a Releasing donor's GPUs are counted once (held, not
  promised); on a physical group, a GPU pod WVA does not manage is subtracted
  (`H_other`); the warm pool's snapshot equals `idle`.
- **Physical groups:** with `physicalGroups: false` a group bounded only by the
  inventory runs `GreedyByScore` and never allocates beyond demand.
- **Fallback cycle:** a cycle that falls back to cost-aware publishes the
  ledger's lowered target for a Releasing donor, not its demand-driven one
  (negative control: without the overlay the donor is restored).
- **Owed floor:** a floor raised on a model above its need gets a transfer
  although no `z` improves; the donor's own floor is still respected.
- **Steering:** a donor with a not-ready pod is never part of a per-node set; when
  a different pod than the marked one is removed, the receiver is re-planned from
  the hole that opened instead of waiting for the release timeout.
- **Config:** an `optimizer:` block in a namespace-local map has no effect; an
  invalid block leaves the limiters in force (negative control: today's
  whole-entry rejection drops them); a numeric weight outside the class range is
  clamped; a ScaledObject with a 60 s window yields a 60 s-based release time,
  hold and swing window, each reported with `source="scaledobject"`; with
  `shadow: true` nothing is actuated and every metric and log line is still
  produced.
- **envtest:** a terminating pod is counted as held.
- **kind e2e:** two **P/D** models under one namespace `ResourceQuota`, weight
  classes `important` vs `standard`, multi-GPU replicas (a fake GPU resource
  per pod, so placement is exercised without real GPUs), and a load shift.
  Assert that a transfer between models and a prefill → decode transfer inside
  one model each complete; that no receiver pod is ever `Pending` for quota; and
  that, with node data, the marked donor pod is the one removed. Images from
  fully-qualified registries only.
- **Cluster:** the shape-swap benchmark with a quota, scored on TTFT per weight
  class and on GPU-minutes held idle.

## 13. Staging

1. **Shadow.** Compute need, spare, targets, headroom, the band verdict and the
   transfers that *would* be planned — urgent or not, and how many would be
   refused for replica size; emit the metrics and the log line; actuate
   nothing. The swap case [managed-keda-behavior](managed-keda-behavior.md) builds
   on has never been observed firing; shadow mode measures how often this one
   would, and how large the transfers are, before anything is written. For P/D
   models it also records each role's headroom against its queue, the check
   that prefill's demand is not systematically under-read (§5.6).

   **Built** in `internal/config/utilization_share.go` (the §8 surface),
   `internal/engines/allocation/utilization_share_{target,groups}.go` (§5 and
   §6.1), `internal/engines/steadystate/utilization_share_shadow.go` (the
   per-cycle evaluation) and `internal/metrics/utilization_share_metrics.go`.
   In shadow mode the transfers that would be planned are reported as
   **replicas to move** (the integer target's shortfall).
2. **Actuate with the full window**, on quota groups, P/D and multi-GPU
   replicas included — that is the fleet this is for. Ledger, promised GPUs,
   per-pod donor sets, cancel, timeouts, the engine's overlay. Correct, slow.
   `shadow: true` is the rollback, if stage 1 or 2 showed prefill under-read.

   **Built:**
   - committed GPUs are `HeldReplicas` (`internal/engines/variantmeta/held_replicas.go`):
     scheduled, unfinished pods, terminating ones included, counted by group
     on a LeaderWorkerSet;
   - the ledger and planner (`utilization_share_{ledger,plan}.go`), sized by
     each role's give and grow variants. A transfer is admitted only when
     the donor replica's pods cover the receiver replica's, each receiver
     pod by a donor pod of its own at least its size (`ShareCovers`, from
     each variant's per-pod GPUs, leader first). Two 4-GPU donor pods never
     fund one 8-GPU receiver pod;
   - the derived timings (`utilization_share_timing.go`, §8.4), from the
     ScaledObject's scale-down window and polling interval (now carried on
     `registry.Target`), the pod templates' termination grace, and the group's
     measured releases;
   - the engine side (`internal/engines/steadystate/utilization_share_actuate.go`):
     - a ledger per group;
     - donor pods marked with the lowest `pod-deletion-cost` and the transfer
       annotation, unmarked on cancel or abort;
     - receivers raised only at release;
     - idle fills bounded by the cluster's physical free GPUs;
     - the restart rebuild from the marks, with one fill timeout of quiet
       after it;
     - the overlay on the cycle's decisions under the `utilization-share`
       reason, which the sticky scale-down hold stands down for;
     - restoring from marks only what this controller would have written. A
       mark is read from the donor variant's own pods (Deployment pods are
       matched by ReplicaSet ownership, not only by labels). It must name that
       donor variant, a receiver variant of the same group, both replica sizes,
       and a start in the past. Anything else is removed with a WARN.
     - concurrent transfers from one donor mark distinct pods, and each
       transfer unmarks only its own.
     - a donor lowered on a restart only if its marked pod was not yet
       terminating, and raised back on cancel or abort only if it was lowered.
     - promised GPUs withheld from the other two consumers of "what is
       free": the steady-state engine publishes them each pass
       (`decision.PublishSharePromised`), and both the warm pool's headroom
       and the scale-from-zero budget check count them as used
       (`allocation.WithholdPromised`);
     - re-anchoring: a variant held off its target by something outside the
       optimizer (a ResourceQuota denial, a ScaledObject ceiling, scale to
       zero), with no transfer in flight, returns to its running count after
       one release timeout, with a WARN;
     - `wva_utilization_share_transfers_total`, and, for an active group,
       `_promised_gpus`, `_effective_seconds`, `_swinging` and the
       `_release_seconds` histogram.

   **Not yet built in stage 2:**
   - donor *sets* across several donor replicas, with the per-node check
     (§6.5). A transfer has one donor replica today, so a receiver larger
     than every donor replica's pods is not funded at all.
   - the `redirected` outcome; the `_withheld_total`, `_actual`,
     `_floor_excess_gpus` and `_reserve_debt_gpus` series of §9; and the
     `awaiting-release` blocked reason.
   - wake *claims*: a wake that scores lower than a Releasing transfer's
     receiver redirecting it (§6.3). Until then a wake never takes promised
     GPUs and waits for idle ones -- at most a fill timeout. The warm pool's
     own carve-out from idle (§7.2) is not built either.
   - the P/D and multi-GPU kind e2e. The single-role e2e is built
     (`test/e2e/utilization_share_test.go`): shadow evaluates and touches
     nothing. Active, an idle model's marked pod is the one its ReplicaSet
     removes, and the loaded model grows only after the release. Its first
     cluster run found that the engine keyed scale targets by Deployment
     name, not variant name, so no donor could be marked.
3. **Short window for urgent transfers**, through `wvaOwnership`, once
   managed-keda-behavior lands (§6.4).

## 14. Open questions

- **Charging a floor to its own namespace first.** In a cluster-scope group, a
  large floor is paid by every tenant (§5.5 case 3). An option would take the
  floor's excess out of the *same namespace's* other models before anyone
  else's. It is fairer and it is a second level of sharing — the
  [priority-scoping](priority-scoping.md) problem in another form. Recommended:
  not in v1; report the cost and rely on per-namespace `ResourceQuota`, revisit
  with shadow-mode data on how often floors bind.
- **Idle headroom vs. cost.** Should a model accept GPUs far beyond any plausible
  burst (`x = +500 %`)? That is "use all GPUs" taken literally. A
  `maxHeadroom` cap would leave the rest unallocated instead. Recommended: no cap
  in v1; shadow mode reports how much headroom exceeds, say, +200 %.
- **Reserve default.** `0` keeps "all GPUs" literal; one replica keeps
  scale-from-zero fast. Recommended: `0`, with a WARN when a group contains a
  scale-to-zero model and no reserve.
- **Binding the receiver to the hole.** The planner frees GPUs on node `n`, and
  the scheduler usually takes that hole, but nothing forces it to (§6.5). The
  stronger form would give the receiver's new pod a preferred node affinity for
  `n`, which means editing a tenant's pod spec. Recommended: not in v1. Shadow
  and stage-2 data on `release-shape-mismatch` will show whether the scheduler
  already does the right thing.
- **Scoring a donor set by more than `z`.** Freeing a node can cost a donor a
  replica that is serving well (low latency, warm cache) where another node's
  replica is idle. v1 scores sets by `z` and replica count only. Per-pod load is
  available and could break ties. Recommended: measure first.
- **Load that swings faster than the optimizer can follow** (§6.7). Below
  about eight decide-to-serve latencies, standing still beats moving. The
  swing rule limits the loss, but does not turn it into a gain. Two ways to
  do better, neither designed: detect a periodic load from its own history and
  stop sharing headroom for that model, sizing it for its peak; or let the
  operator declare a model as cyclic. Recommended: measure in shadow mode how
  often real fleets trip the swing rule before designing either.
- **Default classes.** `0.5 / 1 / 2 / 4` is a starting point. Shadow mode should
  report, per group, the headroom each class actually received, so the defaults
  are chosen from data rather than taste.
