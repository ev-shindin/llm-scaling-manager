# Proposal: a utilization-share optimizer

**Status:** stage 1 (shadow) and stage 2 (actuation, donor sets included) built; what stage 2 does not yet do is listed in §13. Stage 3 (a short window for urgent transfers) is design.
**Created:** 2026-10-05
**Last Updated:** 2026-10-16

## Summary

A third optimizer beside `CostAwareOptimizer` and `GreedyByScoreOptimizer`, for
a fleet that owns a fixed GPU allotment (a namespace quota, or a cluster-scope
limit) and wants all of it used. When it is on:

1. **Every model first gets what it needs**: the GPUs that put it at its own
   scale-up threshold — the GPU form of the engine's `RequiredCapacity`, before
   the rounding and scale-down hysteresis the cost-aware optimizer adds.
2. **The rest of the quota is handed out as spare headroom.** When the quota
   cannot hold every model at its recent peak at once, the headroom is capped
   at `maxHeadroomReplicas` replicas per role for weight 1 (one by default,
   §5.1) and `⌈w · maxHeadroomReplicas⌉` for weight `w`; what is left stays
   unassigned, and a model whose demand rises takes it at once, with no other
   model to drain first. A model's **weight** decides its share: weight 2 gets
   twice the headroom of weight 1, relative to its size. When the quota is
   too small for everyone, a higher weight is cut less.
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
   transfers and a demand step settles in the minimum number of moves, with
   none wasted, in simulation up to 16 models; slow shifts are followed. Load
   that swings back within about 100 minutes cannot be followed by moving
   GPUs, and there standing still does
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

**When the quota is contended, the headroom one role is given is capped
(`maxHeadroomReplicas`, default 1), by weight.** The quota is *contended* when
it cannot hold every role at once at its peak need over the swing window: each
role's claim at its peak need (`ShareLedger.PeakNeed`), rounded up to whole
replicas, summed over the group, exceeds the budget (`peakClaims`, computed
once per plan; `capHeadroom` runs when the sum is over). The peak is a
**sustained** one: the highest need that two consecutive readings over
`flappingWindow` both reach, `max_i min(N_{i−1}, N_i)`, or the current need
while fewer than two readings exist. One reading -- a scrape glitch, or a
tenant's forged metric -- would otherwise cap every other model's headroom for
a whole window. A role whose history has all aged out (absent for longer than
the window) starts it again (`RecordNeeds`): kept, its first-seen time would
call a peak known from one sample. A role whose need has been recorded for
less than one swing window (`ShareLedger.PeakKnown` false) claims one of two
things, depending on whether any role's peak is known (`AnyPeakKnown`):

```text
  P̂_r  = min(peak N_r, B_net)                        if PeakKnown(r)
       = min(max(peak N_r, H_r), B_net)               else, if AnyPeakKnown   (a joiner)
       = min(max(peak N_r, min(C_r, B_net)), B_net)   otherwise               (no history)
  P_r  = g_r · ⌈ min(max(P̂_r, F_r), C_r) / g_r ⌉      (the role's peak claim)
  contended  ⇔  Σ_r P_r  >  B_net
```

`H_r` is what the role holds. A role joining a group whose history is known
-- a new model, or one renamed to look new -- claims that: at its ceiling, it
would cap every other model's headroom for a window. Only while no role's peak
is known -- a new group, or every role after a restart with no stored history
-- is each taken to peak at its ceiling, or at the whole budget when it has
none (the roles so counted are `SharePlan.UnknownPeaks`).

Judged only on the history the ledger has -- one swing window long --
every role's peak read as its current need after a restart with no history,
on the
first alternation of two models, or with bursts further apart than the
window: the quota read as uncontended, the cap stayed off, and the first burst
found the spare parked on the model that had burst last, the cluster failure
the cap exists for. So the cap is on until a full window of history shows the
peaks fit.

That history -- the contention memory -- survives a restart, a leader change,
the optimizer leaving and re-entering active mode, and a group dropped after
the absence grace and seen again. The acting controller stores it in one
ConfigMap in its own namespace, `wva-utilization-share-history`
(`internal/engines/steadystate/utilization_share_history.go`), one data key
per group (the controller instance and group key, sanitized, with a hash).
Each entry is JSON: a format `version`, the `group`, `writtenAt`, the
`swingWindow` it was kept over, the writing `controllerInstance`, and per role
its `since` (when its need was first recorded) and its samples as
`[unix seconds, need]`, the need rounded up to four significant digits. A
sample is a bucket's sustained level: the highest `min` of two consecutive
readings ending in the minute (coarser past 180 minutes of window;
`ShareLedger.NeedHistory`), the quantity `PeakNeed` takes its maximum of. A
bucket's highest reading would make an isolated spike in each of two
neighbouring minutes a sustained peak after a restart; its lowest would erase
a real two-reading burst inside one minute. Each stored level is restored as
two readings at its instant, so `PeakNeed` after a restore is `PeakNeed`
before it. Each group's history is staged on change, at most once a minute,
and every staged group is written in one read-modify-write per cycle, with
the ConfigMap's `resourceVersion` as the concurrency check (a conflict
re-reads and retries once), and only on the acting path: shadow mode keeps no
ledger and writes nothing. Every ledger rebuilt in a cycle restores from one
read, through the API reader, before its first plan
(`ShareLedger.RestoreNeedHistory`): an entry of another version, group or
controller instance is ignored; a sample that is not a finite, non-negative
need, older than the window or more than a minute in the future is dropped;
and a role's `since` is restored -- making its peak known once a window has
passed since it -- only when its samples run to within `ShareHistoryMaxGap`
(ten minutes) of now, since a longer gap was not observed. Its samples are
kept either way. A restored `since` is never earlier than the start of the
bucket before the oldest sample kept, and an entry kept over a shorter
`swingWindow` than today's restores no `since` at all (the oldest sample
stands in): a peak is known only over what samples cover. A role not in the group is restored too;
`Retain` forgets it if it does not return. A NotFound read is no history; any
other failure, Forbidden included, is logged at Error once per change and
restores nothing, and the group writes nothing until a read, retried once a
minute, succeeds, so a history it could not read is never overwritten. A
vanished group's stored entry is kept (only the controller's memory of what it
wrote is dropped), so a group dropped after a couple of minutes of failed
collection gets its peaks back; an entry written longer ago than its window
plus ten minutes is removed by the next write of any group, one that does not
parse at once, and one of another format version once a week old (a
controller rolled back may still read it). Errors are logged per operation
and group, and a recovery only for the group that failed. Deleting the
ConfigMap is the flush: once the controller has read or written it, a read or
write that finds it gone makes the controller forget its in-memory history as
well (`ShareLedger.ForgetNeedHistory`) rather than writing it straight back.
Editing one key by hand is not a flush: the next write of that group replaces
it. The ConfigMap is bounded below the API server's object limit (128 roles
per group, 180 samples per role, and each group written gets an equal share of
900 KiB so one large group cannot crowd the others out); a group over a bound
leaves out its smallest stored peaks first, and the truncation is logged with
the roles it left out. `make undeploy-wva` deletes the ConfigMap; an install
upgraded from before the store existed reports its Role and RoleBinding as
optional prerequisites and runs without them until an admin re-runs
`make setup-prereqs`. RBAC is a namespaced Role
(`config/base/rbac/utilization-share-history-role.yaml`) with `get` and
`update` on that ConfigMap by name and `create` on ConfigMaps (create cannot be
scoped by name), bound to the controller's service account; it is in the base,
so every install overlay renders it. Metrics cannot be this store: they carry
no writer identity and no honest age. One side effect: the restored samples
are per-minute sustained levels, so for one window after a restart a swinging
role's mean need (§6.7 rule 5, `PlanningNeed`) is taken over those levels mixed
with the new per-cycle readings, and reads somewhat differently from the live
mean; the peak is unchanged. The two-readings rule stops a glitched reading,
not a tenant who controls its model's metrics: two forged readings in a row
are a peak, live or restored. Swing state itself is not stored, so a role
swings again only after two new reversals.

So the window of capped headroom is paid only when no usable history exists:
a new group, the first start, a flush, an unreadable ConfigMap, or a
controller down for more than ten minutes. That window may return spare a
spread would have kept warm. The planner
publishes the flip: `wva_utilization_share_headroom_capped` (1 while capped;
not published in a cycle the planner did not plan, such as the restart quiet
period, when the published targets are not the planner's), and an Info line
when it turns on or off, with `peakClaimGPUs` (`Σ_r P_r`), `budget` and a
`cause`: `peaks-unknown` (with `unknownPeaks`) when no role's peak is known, or
`peaks-exceed-budget`. `resetActuation` forgets the flip state, so a rebuilt
ledger logs it afresh.

While the peaks fit, the spare is spread by weight as above and nothing is
returned (§6.2): no burst can then find the GPUs it needs parked on another
model, and returning headroom would only make the next burst cold-start what
a spread spare held warm. The verification simulations measured it: capped
regardless, alternating bursts whose peaks fit together lost 4 to 8% of
demand against under 1% spread, and whole-node bursts in a slack quota lost 6
to 43% against none, to return-then-cold-refill cycles the swing rule could
not see. The cluster scenario below (peaks 8 + 8 under a quota of 10) stays
contended. Comparing each role's peak with its current spread share instead
was tried and rejected: the spread follows the current need, so a role in its
trough always reads as short.

While contended, `ShareCapSpare` lowers each role's ceiling, as the targets see
it, to its claim rounded up to whole replicas plus `h_r` replicas of headroom,
where `h_r = ⌈w_m · maxHeadroomReplicas⌉` (`ShareHeadroomReplicas`):

```text
  C'_r = g_r · (⌈N̄_r / g_r⌉ + ⌈w_m · n⌉)        n = maxHeadroomReplicas
```

wherever that is below the ceiling it had. With the default classes and
`n = 1`: `best-effort` 1, `standard` 1, `important` 2, `critical` 4. Scaling by
weight keeps the weight's meaning for headroom: a flat cap gave weights 1 : 1,
2 : 1 and 4 : 1 the same targets whenever the spare exceeded it, which a review
of the first, flat version found. The planner caps its roles after the swing
rule has set their planning need (§6.7 rule 5), on the higher of this cycle's
reading and the planning need (`needView`): capped on the reading, a swinging
role in its trough would be held below the mean it is planned on; capped on
the mean, one at its crest would be held below its reading, beside GPUs only
it could use.

The exception is a role whose need swings faster than a move can follow
while another role bursts for longer than one (`fastSwingers`:
`ShareLedger.SwingsFast` -- at least two completed bursts, none lasting
`ShareTimings.MoveLatency`, over at least `ShareFastSwingSpread` = 0.25 of its
highest need -- beside a role that `BurstsLong`, a burst lasting a move over at
least `ShareLongBurstSpread` = 0.5 of its highest need, and whose peak needs
more than its floor holds: a burster its floor covers waits on no release).
It is capped on its mean need (`MeanNeed`), listed in `SharePlan.MeanCapped`
when that is below its reading, and shows the blocked reason
`fast-swing-capped`. Capped on its reading it took the idle GPUs at each
crest; they started serving as the crest ended, and parked on a donor that is
never calm, they were what the long burst then waited a release for. In the
round-17 owner simulator, a model bursting 20 minutes an hour beside one
swinging every 3 minutes left 0.33 to 0.35 of its demand unserved, against 0.20
to 0.21 with the swinger capped on its mean, which cost the swinger 0.02 to
0.04 (up to 0.06 across the other noisy shapes simulated; still below its
`e55b02b2` figure). Without a long burster nothing waits
on the swinger's GPUs, and a model re-bursting every 12 minutes beside a steady
one lost up to 0.03 more capped on its mean, so the exception needs one.

A role is never capped below what the other roles' peak claims leave of the
budget, in whole replicas, nor above its own ceiling (`shareRoomForOthers`):

```text
  R_r   = g_r · ⌊ (B_net − Σ_{s≠r} P_s) / g_r ⌋
  C''_r = max(C'_r, min(R_r, C_r))
```

Otherwise a model that bursts alone gave back GPUs no other role's recent peak
could use, then cold-started them on its next burst. So GPUs are returned only
when another role's recent peak could use them.

While the group acts, the published evaluation (targets, `_actionable`,
`_replicas_to_move`, the blocked reasons) is the planner's, capped only while
contended. In shadow mode `evaluateUtilizationShare` applies the cap without
the contention test (it keeps no need history), so on a quota whose peaks fit,
shadow's targets read lower than the ones acting would move to. Before
`19e7be3f` an active group published that always-capped evaluation too, which
disagreed with what it actuated. Claims are unchanged (the lowered ceiling is
never below a claim), and a role with no need gets no spare however its
ceiling reads. `n` is bounded at 10000; a limit too large to hold, or one of
zero (which would read as unbounded), leaves the ceiling as it was. A role at its lowered ceiling
takes no further share, so whatever the cap leaves over is not allocated
(§5.4: "any leftover stays unallocated"). It stays unassigned in the quota,
and the idle fill (§6.2, "Free budget first") hands it to the first role whose
target rises above what it holds -- at once, with no donor to drain. The fill
runs on the planner's capped targets, so it and the planner agree on what is
unassigned. `0` gives no headroom at all; a value as large as the models'
`maxReplicas` restores the uncapped split of this section.

Why the cap exists, measured on a shared H200 cluster (two 1-GPU models under a
namespace quota of 10, each `maxReplicas` 8, bursting in turn; well-lit path,
[measured](../well-lit-paths/utilization-share/measured.md)): spread as
headroom by weight, the spare parked on the model whose burst had ended, 7
replicas at 5% load, each serving 6000-token outputs. When the other model
burst, every replica it got had to wait for one of those to drain, about
5 minutes 45 seconds each, two at a time: it went 3 -> 5 -> 7 -> 8 replicas
over 18 minutes, where today's optimizer, finding the GPUs idle, took it from
1 to 8 in 2. Its p95 time to first token was 313 s against 139 s under today's
optimizer. Headroom on another model is only as fast as that model's drain;
unassigned GPUs are as fast as a pod start.

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
     headroom series. The cause is carried in the per-cycle log line — not as a
     metric label, which would be unbounded.
   - A group whose floors hold more than half the budget above their need is
     logged once per change, naming the excess; `floor_excess_gpus` says whose.

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
`optimizer.type` (§8). Either removes every donor-pod mark this controller wrote
and gives each pod back the deletion cost it had before -- including after a
restart, when no ledger holds them yet. A downgrade to a version without the
optimizer does not: switch to `shadow: true` first, let one cycle pass, then
downgrade.

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
  of that transfer's start. A transfer cancelled before any pod moved removes
  exactly its own recorded moves (§6.3): it holds neither role and counts
  toward no swing. It holds only that donor -> receiver pair from starting
  again, for one reversal hold (`PairHeld`), so a start and a cancel cannot
  loop; the receiver can still take from other donors and the donor give to
  others. The hold blocks **transfers only**: a role on hold that falls below
  its need still draws on idle GPUs and the reserve.
- **A hard imbalance waits for no hold** (`shareHardImbalance`). A role's
  *load* is its need over the GPUs it holds: its utilization as a fraction of
  its scale-up threshold. A move whose receiver's load is at least 0.9 -- at or
  near its threshold -- and whose donor's load on what it keeps after giving
  is at most 0.6 waits for neither
  the reversal hold nor the confirmation cycles: it starts on the first cycle,
  and the transfer is logged with `skippedWaits=true`. Everything else still
  applies: the cancelled pair's hold, an aborted donor's back-off, an
  exhausted donor (`GivingBusy`), the admission test above (the pair's
  worst-off improves, the donor keeps its target and keep). The two thresholds are the rule's hysteresis: after the move
  the donor is at or below 0.6 and the receiver below 0.9, so the reverse move
  cannot qualify until demand itself changes by their ratio, about 1.5x. Every
  short role is above 0.9, so in practice the donor's load decides. Both
  limits, and the switch, are the operator's (`stabilization.skipWaitsWhen`,
  §8.1).

  Why it exists: the first rule exempted no receiver from the hold, on the
  simulated premise that a role that just gave rarely bursts within it.
  Alternating bursts break that premise. On a cluster run, GPUs moved to a
  model whose burst had just ended (its draining backlog still read as need)
  as the other's began, and the hold kept them there for 18 minutes -- the
  whole burst served on 3 of 8 replicas while the other model, idle, held 7,
  at a 564 s median time to first token against 1 s without sharing. An
  interim rule (a receiver below three quarters of its need, from a donor that
  keeps its own need) was sized in a simulation whose cancel behaved
  differently from production; its figures are superseded and the rule is
  gone.

  The thresholds come from the §6.7 simulator run in production's call order
  (5-20 seeds; ratios are shortfall against a fleet that never moves). On
  1-GPU sine and square swings with 15-60 minute periods and need lagging
  demand by 1-5 minutes, 0.9/0.6 stays within -0.18x..+0.06x of no exemption,
  and helps the 15-minute swing (1.43x -> 1.25x). A calm bound of 0.7 cost up
  to +0.52x (15-minute swing, 3-minute lag: 1.72x against 1.20x), and
  exempting every short receiver reached 3.1x where no exemption peaked at
  1.86x. Whole-node square bursts (8-GPU replicas, 5-10 minute cold start)
  stay within +/-0.06x of no exemption. The simulation never produces the
  failure the rule is for -- one model held short for its whole burst while
  the other, idle, holds the GPUs -- so it bounds the rule's cost, not its
  benefit.

  **A hard imbalance that persists is not paced either.** On the first cycle a
  role is a hard-imbalance receiver, its moves skip the waits but keep the
  pace: two replicas per role per cycle, two transfers in flight per group. If
  it is one again the next consecutive cycle (`ConfirmHard`,
  `ShareUnpacedAfterCycles` = 2), neither the pace nor the concurrency limit
  applies: `fundFrom` asks the same donor again until the receiver reaches its
  need -- the higher of this cycle's reading and its planning need, which is
  its mean over the swing window when it swings -- the move no longer qualifies (the receiver's load drops below 0.9, or
  the donor's after giving rises above 0.6), or the donor reaches its target or
  its keep; then the next donor is asked. So a receiver takes every replica it
  is short **of its need** in one cycle, from one donor or several. The replica
  of headroom above its need is an ordinary, paced move (`unpacedNow`). Donor
  sets (§6.5, a multi-pod or LeaderWorkerSet receiver) get the same treatment:
  no confirm cycle when hard, and set after set with no pace and no concurrency
  limit once the imbalance has held a second cycle. Every other bound still
  applies: the targets, the keep, the admission test, the pair hold, back-off and a busy donor (`GivingBusy`), and the ratio between
  the two loads still stops the reverse move.

  The second cycle is the guard against a bad reading. Unpaced on the first,
  one scrape glitch or forged metric could drain every calm model in a group
  at once; reviewing the first version, one stale signal moved five replicas
  that could not be undone. Paced on the first cycle, a single reading moves at
  most two replicas, which the next cycle cancels while they are free.

  **Nor when the burst ends first.** The second cycle frees a hard imbalance
  from the pace only while its burst is not expected to end before a move
  lands (`ShareLedger.Unpaced`: `!ModelBurstEnds(role, model, tm)`). The
  horizon is 1.25 times the move latency -- the time a move takes to land and
  a quarter of it again to serve in. A burst is expected to end when the
  role's last completed burst, less how long the one under way has lasted,
  is under the horizon (`BurstEndsWithin`). The last, not the longest: one
  long burst would make every short one after it look long for a window.
  There is no expected end -- the role keeps the unpaced path -- with no
  completed burst on record, once the burst under way has outlasted the
  last, and for a need whose range is under `ShareFastSwingSpread` (0.25) of
  its peak: a steady need with noise crosses its own midpoint every few
  readings and would otherwise read as a string of short bursts. The roles of
  one model (`ShareModelRoles`, from `ShareGroup.Origins`) are judged as
  one, and get the same answer: every role whose need swings across
  `ShareFastSwingSpread` and is in a burst must be expected to end it, and
  while none is in one, every swinging role's last burst must be. A role
  whose need does not swing is not judged -- counted as a burst with no end,
  a decode steady at 8 GPUs with noise unpaced its prefill's every short
  burst. So a prefill and decode do not run their model's burst at a skewed
  ratio, one taking its whole shortfall while the other moves at the pace. `needBursts` trusts no single reading: the range runs from
  levels held two readings in a row, a run starts and ends only on two
  readings in a row, and a run already under way when the history starts is
  not a completed burst, but counts as a lower bound toward the longest.
  Traced in the whole-node simulator, a 15-minute burst was seen about 30 s
  in, and a move took about 14 minutes to land: taken all at once, the moves
  served the burst a minute or two and left the donor short through its own
  next burst. Paced, tight 1x8 at 30 minutes went 0.247 -> 0.232 (0.225 for
  `e55b02b2`; switching the unpaced rule off entirely reached 0.223 but cost
  the 40- and 60-minute rows 0.010 and square2 at 45 minutes 0.019). The
  `transfer-limit` blocked reason reads the same rule, so a short-burst
  receiver held to the pace reports it; such a receiver also reports
  `burst-ending` from its second hard cycle, whether or not it is receiving,
  so a role taking a replica a cycle through a long shortfall shows a reason.

  Moves that skipped the waits count against the concurrency limit
  (`InFlight`); the unpaced moves of a hard imbalance from its second cycle
  pass it without consulting it. Uncounted (before `19e7be3f`), ordinary
  headroom moves started beside a burst's, drained the same long-drain donor,
  and landed after the burst had ended (c16 40m: 0.147 -> 0.119). Only a
  returned spare is not counted. The hard-imbalance
  test judges the receiver on `needView`, the higher of its reading and its
  planning need: on its mean, a swinging role at the crest of a burst never
  took the unpaced path. It judges the donor on `donorView`, the higher of that
  and the donor's peak need over its scale-down window (`RecentPeak` over
  `ShareTimings.Window`), as a scale-down is: judged on one reading, a calm
  donor read in a trough of its normal demand was drained in two cycles. The
  recent peak, like `PeakNeed`, needs two readings in a row; and for a
  receiver whose bursts outlast a move -- a completed one, or one under way
  when the history starts (`LongBursts` against
  `ShareTimings.MoveLatency`) the donor is judged on `needView` alone
  (`donorViewFor`), since such a move lands inside the burst and holding it a
  window at each handover only made the burst wait (the 40-minute whole-node
  losses of `6abc491d`). Both views apply in `hardReceivers`, `fundFrom` and
  donor sets (`shareSetRule.need` and `shareSetRule.donorNeed`), so a set does
  not drain a donor that a single move refuses; the `donor-peak` guard applies
  to sets too (`shareSetRule.keepsPeak`). Aborts of one donor in one cycle back it off once: transfers started
  together share a deadline, and five aborting together would otherwise reach
  the 16x back-off on their first stuck release. A donor whose pod could not be
  marked starts nothing more that cycle.

  Measured on a shared H200 cluster (§5.1), before this rule: every move to the
  bursting model skipped its waits and started on the first cycle its load
  crossed 0.9, yet it held 3 of the 8 replicas it needed for 12 minutes,
  because two releases at a time, each waiting about six minutes for
  6000-token requests to drain, were all the pace allowed.
- **A swinging role** is planned on its mean need (§6.7 rule 5).
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

  **The fill is unpaced below need.** A fill drains nobody, so it raises a
  receiver below its need by every replica it is short at once
  (`ShareFillPace`); a receiver at or above its need is raised two replicas
  per cycle. GPUs a releasing transfer gives no receiver are not idle
  (`ShareLedger.Unreleased`): all of a reserve refill's or an unassigned
  return's, and whatever a transfer or a set takes beyond its receiver's
  replica. The donor's pods still run on them and the
  quota still counts them. Counted idle, the fill would raise a model into a
  full quota, its pods would be refused at admission, and it would be raised
  again at every fill timeout.

  **A role still giving is no receiver** (`ShareLedger.Giving`). A role that is
  the donor of a transfer, a return or a refill still Releasing is not raised,
  by the planner or by the fill, until that release lands. A target that
  crossed a replica boundary otherwise filled a role while its own transfer
  drained; the transfer then never saw the role shrink, aborted, restored it
  one replica above target, and backed it off. The same rule stops a move that
  skips the waits from sending GPUs back to a role mid-drain when an
  alternation is shorter than a release. (A separate reverse-in-flight hold
  written for that was redundant under mutation testing and was removed.)

  **The planner counts the fill before it asks a donor** (`fromIdle`). Before
  funding a receiver from donors, it adds to that receiver's planned allocation
  what the idle fill will give it this cycle -- at the fill's pace, while
  `idle` covers a replica and the receiver is below its whole-replica target,
  and, with node information, only replicas whose pods fit free GPUs in the
  receiver's topology domain, as the fill places them -- so donors fund only
  the rest and no donor shrinks for GPUs that are free. Without the node check,
  fragmented free GPUs were credited that the fill could not place. A receiver the last fill left below its target (`FillShort`) is not
  credited: whatever kept it short, the fill sees and the
  planner does not. With the spare capped (§5.1), unassigned GPUs are common,
  and without this a donor would drain a pod for a receiver the fill was about
  to raise anyway.
- Fund the rest one **receiver replica** at a time. Each such move is one
  **transfer**: one receiver replica, funded by a **donor set** — one or more
  donor replicas, from one or more models, that open a hole for **each pod** of
  that replica, on a node that fits it whenever node information exists (§6.5).
  **A transfer is admitted only if both hold:**

  ```text
  min(z over receiver and every donor, after) > min(z over the same roles, before)   the worst-off role improves
  every donor keeps its target after the move                                         no donor is pushed below it
  ```

  The first condition is the anti-oscillation rule: every admitted move strictly
  improves the worst-off role among those it touches, so no sequence of moves
  returns to an earlier state. A move the targets allow can still fail it: a
  donor above its ceiling would give a role that does not need it headroom it
  already has (`TestShareMoveImprovesTheWorstOff`). It is also the rule the
  whole-node simulator's P/D cells rest on: switched off, two P/D models on
  1x8 replicas left more of their demand unserved (0.027 -> 0.038 at a
  20-minute swing, 0.038 -> 0.046 at 30 minutes) and, at 20 minutes, nearly
  doubled their churn (14 -> 26). The second is judged per donor kind. A single donor
  keeps at least its whole-replica target and its keep after giving
  (`Ḡ'_d ≥ G*_d`): a move goes only from a role above its target to one below
  its own, and the check is on what the donor keeps, which a donor replica
  larger than its target's step (an 8-GPU replica of a role whose replicas are
  otherwise 4) would otherwise take below. A **donor set** (§6.5) keeps every
  donor in band after the move, or still above its target (`Ḡ'_d ≥ Ĝ_d`),
  "in band" being the predicate of §6.1 step 3, with the same half-replica
  tolerance (`donorInBand`). A single donor was judged on that band until the
  simplification after round 20 (§13), which found it never changed a move.
  A transfer for an owed floor (§5.5 case 5) bypasses the admission, and
  §6.7's holds; it may still never take a donor below its own floor.

  With demand held fixed, the first condition alone guarantees convergence: each
  admitted move makes the group's sorted list of `z` values strictly larger in
  the max-min order, a finite set of allocations cannot increase forever, so the
  planner stops. Demand is not fixed, and §6.7 is what keeps noise from
  restarting it every cycle.
- **While spread, a headroom move does not take a donor below its own peak**
  (`fundFrom`, counted `donor-peak`). While the quota holds every role's peak
  (the cap is off, §5.1), a receiver already at its need takes no replica that
  would leave the donor below its peak claim `P_d` (`sharePlanRun.peaks`): the
  split covers both, and such a move -- from a donor that may drain for
  minutes -- lands after the receiver's burst and just before the donor's own.
  That was the c16 40-minute regression: in the reviewers' simulator, these
  moves drained a slow donor below its recent peak for a receiver already at
  its need, and withholding them took c16 at 40 minutes from 0.123 to 0.045
  (`e55b02b2`: 0.076) with no regression elsewhere. Moves to a receiver below
  its need are not affected, nor is anything while contended.
- **A return does not take a role into its scale-down band**
  (`returnSpare`, `sharePlanRun.bandKeep`, counted `scale-down-band`). A
  return is today's scale-down done while nobody waits for the GPUs, so it
  stops where today's optimizer would: at the GPUs on which the role's
  demand sits at today's release target, a quarter of the way up from its
  scale-down boundary (`ShareGroup.DownThresholds`, from
  `CompositeSignal.ScaleDownBoundary`) to its scale-up threshold
  (`releaseTargetUtilization`), in whole replicas and no more than its
  ceiling. At the boundary itself, a role of 8-GPU replicas at a need of 20
  kept 4 replicas where today's scale-down keeps 3. No band without a
  boundary below the scale-up threshold.
  Transfers are not banded: holding a receiver's donor in its band blocked
  the rising P/D model's prefill at each handover -- at its need of 10.1
  GPUs, needing about 15 within a minute, with a replica 10 minutes away --
  and took the whole-node simulator's worst P/D cell from 0.038 to 0.093;
  judging the band in whole replicas did not help (10.1 already rounds up to
  the 16 the role held). Returns-only is inert in all three simulators (41
  oscillation rows, 160 whole-node cells, 42 owner rows identical): a
  guarantee, not a gain. Open: a headroom move to a receiver at its need may
  still take a donor into its band; doing that safely needs the donor's need
  trend, not its reading.
- Bound the pace: at most two replicas given or received by any one role in a
  planning cycle, and at most two transfers in flight per group (constants,
  §8.4). A large
  rebalance completes over several cycles, each re-checked against fresh
  demand. A hard imbalance that holds a second cycle is the exception: it is
  not paced up to the receiver's need (above).
- **Return the spare** (`returnSpare`, after every receiver is funded). A role
  holding more than its whole-replica target -- typically one that was raised
  for a burst that has ended, and now holds more than its need plus its
  headroom -- gives the excess back to the group's unassigned GPUs. It is
  today's scale-down, done while nobody waits for the GPUs. A return is a
  **Returned** transfer: it has a donor and no receiver, and it is entitled (no
  `z` admission, no holds set). It starts only when all of these hold:
  - **the quota is contended** (§5.1): only a capped spare is returned. Spread,
    the targets give the whole spare away by weight, and what moves between
    roles moves by transfer;
  - **the role has held more than its target for the confirm cycles**
    (`MarkOver` counts consecutive cycles; `confirmCycles`,
    `stabilization.consecutiveCyclesBeforeMove`, default 2), as any move
    waits. The release that follows waits the donor's scale-down window --
    the clock today's optimizer shrinks it on -- and is cancelled while it
    waits if the role's demand comes back (below), so a burst that comes back
    within the window keeps its replicas warm. Until `6abc491d` the return
    also waited that window before it started (`MarkOver` against
    `ShareTimings.Window`), so a return took two windows where a scale-down
    takes one, and a burst on another model found the spare still parked.
    The cost is at a group's first start, or a restart with no usable
    stored history: for the first swing window (about 12 minutes) every
    unknown peak counts at its ceiling, and a role holding replicas it does
    not yet need cannot be told from one whose demand will come back. Holding
    them helps a role in a dip (a long-drain model's 12-minute dip at a
    10-minute window: 0.001 -> 0.038 since returns start after the confirm
    cycles); giving them back helps another role about to burst. Waiting the
    window while the contention is only assumed was tried in round 19 and
    dropped: it cost a burster beside a noisy long-drain model 0.305 -> 0.348
    at a 5-minute window. Restored history avoids both. Simulated, not
    measured.
    Without any wait, returns churned a pod a minute near replica boundaries:
    in review simulations, 175 return-then-refill pairs in 30 hours, and 96
    whole-node cold starts in 8 hours; the calm rule and the cancel (below)
    are the hysteresis now;
  - **no role short of its claim can grow**: a role below its claim rounded up
    to whole replicas, that has a variant to grow and is not held from filling,
    blocks every return (counted `return-role-short`). With such a role short,
    the GPUs go to it by a transfer, which the holds govern. A role short only
    of its headroom does not block, nor does one held from filling, nor one the
    planner found unfundable this cycle (no compatible donor): the first
    version blocked on the first two, so one tenant could pin everyone's spare;
  - **not for a receiver no node can hold**: while every role below its
    whole-replica target that could take the GPUs is one the planner found
    unfundable (no compatible donor), and no node holds its replica in the
    node's free GPUs plus the pods there of the roles that would return,
    nothing is returned (counted `return-receiver-unplaceable`). The GPUs
    would sit unassigned while the donors lose their headroom; in the
    whole-node kind run, small models returned 8 replicas for a large model
    no node could hold. Unfundable alone is not enough: a donor set pays the
    receiver's whole replica from donor replicas, never from the idle quota,
    so a large model that lent a node to a small one and then bursts is
    unfundable while the small model holds fewer GPUs than one large replica
    -- and gets the node back only by the small model returning its pods
    there and the idle fill raising it. Judged every cycle, and never without
    node information or the donors' pods, where the idle fill places blind
    and a return may still serve the receiver;
  - the role is not held from giving (`GivingHeld`: backing off after an
    aborted release, all it can give already in flight, or it received within
    the reversal hold). The band no longer gates a return: the idle-floor rule
    kept the idlest whole-node donors in band, so they never returned anything;
  - it never takes the role below its target, its floor or its keep;
  - **the role stays calm on what it keeps, less its headroom**: what its need
    serves is at most the return load (`stabilization.returnSpareWhenLoadAtMost`,
    default 0.6) times what it would hold after the return less its headroom
    serves, `s_r(N_r) ≤ ρ × s_r(Ḡ_r − g_r − g_r·h_r)` with `Ḡ_r` before the
    return, `h_r` its headroom replicas and `s_r` its capacity curve (the
    identity when every GPU serves alike; counted `return-not-calm` when it
    holds back). Read in GPUs, a role whose sparse replicas sit at the end of
    its curve returned GPUs at a served load of up to 0.76.

  Once due, everything above the target that passes the calm rule goes in one
  cycle, as a scale-down would: returns are unpaced and outside the
  concurrency limit.

  The calm rule is the return's hysteresis. The fill raises a role as soon as
  its target passes what it holds -- once its need passes what it holds less
  its headroom. Without a gap, a role whose need sits near a replica boundary
  would give a replica back and take it again every few cycles, each time
  draining one pod and cold-starting another. With it, a role that returned a
  replica is refilled only once its need has grown by `1/ρ` (1.67 times at
  0.6). The first version used the skip-waits donor load and assumed one spare
  replica, so at a headroom of 2 a role returned and refilled continuously;
  the return load is now its own key, and the rule subtracts the role's own
  headroom.

  A return goes through the ledger like any donor-only transfer: Releasing
  until its donor's pod is gone, then Filling and Done in the same cycle, since
  there is no receiver to wait for (`wva_utilization_share_transfers_total`
  counts it `outcome="done"`; `wva_utilization_share_starts_total` counts it
  `kind="return"`). Its GPUs are not idle until released (`Unreleased`, above).
  The `Returned` flag is written on the donor pod's transfer mark, so a
  controller restart restores a return as a return; a mark that claims a
  return with a receiver or a set is rejected, since the controller never
  writes that shape.

  **A return is always a return.** It is never handed to a role that turns
  short while it releases: its GPUs land in the unassigned GPUs, and the idle
  fill raises whoever needs them. Rounds 15 to 19 handed a return still
  Releasing to a short receiver (`fromReturns`); the switch-off study after
  round 20 found the fill places those GPUs anyway (4 rows worse, 2 better,
  worst +0.015), and the hand-over was removed with the state it added -- a
  return with a receiver, cancelled as a transfer, redirectable by a wake. The
  returned replica's pod shape is still recorded when the return starts
  (`ShareTransfer.DonorPodGPUs`, from `shareReplicaPods`), for
  `returnsCover` below. A receiver short while returns are still Releasing is
  credited with the whole replicas of it they will free (`returnsCover`):
  those land in the unassigned GPUs, where the idle fill places its pods, so a
  transfer or a set would drain a donor for GPUs already on their way. Only
  returned pods at least as large as the receiver's smallest pod count -- its
  whole replica for a single-pod receiver -- so a 2x4 return frees nothing for
  a 1x8 pod, and a return whose shape is not known counts for nothing. The
  receiver waits for the rest of its shortfall only on donors, counted
  `awaiting-return`. Until the simplification only a multi-pod receiver was
  credited, and a single-pod one waited on the hand-over instead.

  **A return is cancelled
  while it is free** (`cancelReversals`: still Releasing, inside the
  scale-down window, so no pod has moved) once its role's demand comes back:
  it is below its target again (or more than twice its tolerance below its
  continuous target), or busier than the return load on what it keeps less
  its headroom plus one replica -- one replica above the bar the return
  started under, for any headroom including 0, so the two do not alternate. In
  `7ff3004a` they shared a threshold at headroom 0, and returns started and
  were cancelled in alternate cycles. The cancel is entitled, so it sets no pair hold.

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
landing (§6.7).

A cancelled transfer removes exactly its own recorded moves: each move is
tagged with its transfer's ID, and each role's last-gave and last-got times
and its swing state are re-derived from the moves that remain. So a cancel
holds neither role and counts toward no swing, whether or not the transfer
had already been confirmed. (The first version restored a snapshot taken when
the transfer started, which confirmation had already deleted, so in
production it did nothing.) The cancel then holds only that donor -> receiver
pair from starting again, for one reversal hold (`PairHeld`). Neither role is
held toward any other: the receiver can still take from other donors and the
donor give to others. Two earlier rules failed here:

- holding the pair in the reverse direction blocked exactly the move the
  cancel calls for. The cancel happens because the donor's demand rose (or
  the receiver's fell), so the donor is the role about to need GPUs. On a
  cluster run a prompt-heavy model whose burst began as it was about to give
  waited 16 minutes for GPUs that sat idle on the other model, serving at a
  575 s median time to first token;
- holding the receiver as a role, from receiving from anyone, let one model,
  by timing its own load to cancel transfers, keep another from receiving at
  all.

While the
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
`DecisionReasonUtilizationShare` (built as `utilization-share`) and letting the hold treat that reason like
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
on them, and WVA reads no HPAs. (The need history of §5.1 is a different
matter -- one group-wide record, not per-transfer state that must die with its
pod -- and is stored in one ConfigMap under a Role of its own.)

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
nodes, a namespace-scoped installation -- which may not list nodes, and whose
cache holds only its own namespace's pods -- or a cycle in which node discovery
failed), only the first row of the
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
index down and does not read a deletion cost, so only the group an LWS donor
removes next -- the highest below `spec.replicas` that no live transfer has
marked -- is a candidate, and it contributes a hole on each node its pods
occupy. That makes an LWS donor the natural funder of an LWS receiver of the
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
   tolerance above target without it. A cancelled transfer removes exactly
   its own recorded moves, so it neither holds its roles nor counts toward a
   swing; it holds only that donor -> receiver pair from starting again, for
   one reversal hold.
4. **Hold a role from reversing** (§6.2). A role that gave cannot receive, and
   one that received cannot give, for two release times measured from the
   transfer's start. That is about one release time after it completes. The
   hold blocks only the opposite direction. A move that rebalances a hard
   imbalance -- a receiver at 0.9 or more of what it holds, from a donor that
   stays at 0.6 or less of what it keeps -- is not held; if it holds a second
   cycle it is not paced either, up to the receiver's need (§6.2). A short role
   held otherwise shows the blocked reason `reversal-hold` while it waits. A
   spare return (§6.2, `returnSpare`) has its own hysteresis: it starts after
   the confirm cycles, its release waits a scale-down window and is cancelled
   if demand comes back meanwhile, and a role gives back only while its need is at most
   `returnSpareWhenLoadAtMost` (0.6) of what it keeps less its headroom, so it
   is refilled only after its need grows by 1/0.6.
5. **Plan a swinging role on its mean need.** A role whose transfers change
   direction three times (`stabilization.swingReversals`, 2 to 20) within
   eight decide-to-serve latencies (about 96 minutes) is following load it
   cannot catch; a transfer cancelled on a reversal counts as the move it
   would have made, a change only when it reverses the one before, and a
   cancelled spare return, which moves nothing between roles, not at all.
   (Two, as first written, took a single swap
   back for a swing -- counting the move that first raised the model -- and
   held both models halfway: issue #269. Moves alone never reached three for
   a load swapping faster than the release window, where every transfer is
   cancelled before it moves, hence the cancels. The results below were
   measured under two.) For the next such window it is planned on its
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
  every scenario of the table above (three models) no transfer is wasted, and
  free cancels fall from hundreds to at most two dozen. With five or more
  models swinging at 30 minutes, the hard-imbalance exemption can undo a move
  within the reversal hold: at most 2 moves in three 6-hour runs (1 at N = 5,
  2 at N = 16, none at N = 4 or 8). 15-minute swings waste none.
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
optimizer's own purpose: spare GPUs are exactly what absorb a swing it
cannot follow -- and they absorb it fastest unassigned (§5.1,
`maxHeadroomReplicas`), where the rising model takes them without waiting for
another's pods to drain.

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
  **target size** (reserve + lent + 1, which the pool publishes to KEDA) is a
  carve-out like the reserve: `B_net` is the quota minus the larger of the
  pool's holdings and its target, and the unheld part of that carve-out stays
  free for the pool to grow into (§6.3). The pool can
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
- **An excluded namespace shares only under a finite cluster bound** (built in
  [#168](https://github.com/ev-shindin/llm-scaling-manager/pull/168)). A
  namespace a namespace quota lists in `exclude` is not bounded by that quota,
  so the quota's sum is no budget for it. Its models join the cluster group
  only when a finite `scope: cluster` quota covers their accelerator type, or
  a `gpu-inventory` limiter does with `physicalGroups: true`. A cluster quota
  of `-1` counts as none. Otherwise they keep today's path, logged once as
  `Utilization share: not sharing models no quota bounds`. Exclusions match by
  namespace name.

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

That is a complete configuration, and it acts at once: `shadow` defaults to
false. Run it with `shadow: true` first and read the series of §9 before
letting it move GPUs. Every other key is optional, and the common case needs
none of them:

```yaml
    utilizationShare:
      tolerance: 0.15               # the "±threshold percent": band in GPUs, as a fraction of the target (never under ½ replica)
      reserveGPUs: 0                # held out of the budget so a wake need not wait for a transfer (§7.2)
      maxHeadroomReplicas: 1        # while the quota cannot hold every peak at once: headroom replicas above need for weight 1; weight w gets ceil(w x this); the rest stays unassigned (§5.1)
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
      clusterNamespaces: []         # non-empty: the cluster groups plan only these namespaces' models (a canary)
      stabilization:                # how fast GPUs move against how often they move back (§6.7)
        consecutiveCyclesBeforeMove: 2  # cycles a model must look short (or spare) before a move
        waitBeforeReverseMove: auto     # a model that gave may not receive for this long; auto = 2 x release
        flappingWindow: auto            # swingReversals direction changes within it: plan on average demand; auto = 8 x latency
        swingReversals: 3               # direction changes within flappingWindow that make a swing; 2..20
        skipWaitsWhen:                  # a hard imbalance waits for neither (§6.2)
          enabled: true
          receiverLoadAtLeast: 0.9      # receiver's load, as a fraction of its scale-up threshold
          donorLoadAtMost: 0.6          # donor's load after giving; must be below receiverLoadAtLeast
        returnSpareWhenLoadAtMost: 0.6  # a role gives headroom back only while its load on what it keeps, less its headroom, stays at or below this (§6.2)
```

Nine keys, each a decision only an operator can make: how far off target is
worth a move, whether wakes may skip the queue, how much headroom a model of
weight 1 may hold ahead of its need rather than leave unassigned, whether to act at all, whether
WVA may hold every GPU of a type, what the priority classes are worth, which
namespace quota groups take part, which namespaces a cluster group plans, and
how fast GPUs move against how often they move back (`stabilization`: its
waits default to values derived from the cluster, and the operator may fix
them; it also holds how calm a model must be to give headroom back).
`stabilization` counts as one key.

`shadow` stays default `false`: that is a decision, not an oversight. The
operator who writes `type: utilizationShare` asked for it to act; the reference
documentation tells them to start with `shadow: true`, and
`wva_utilization_share_mode` reports which mode is in force.

- `optimizer.type` is a selector, not a flag, so a later optimizer is one more
  value rather than another boolean beside `enableRescale`.
- `namespaces:` disables the optimizer for a **namespace-quota group**, and
  does nothing else. It lives in the same top-level ConfigMap, so on a
  cluster-scoped install it is the admin's decision about a tenant, not the
  tenant's decision about themselves. It cannot touch a cluster-scope group.
- `clusterNamespaces:` is the cluster group's counterpart, inverted: when
  non-empty, the cluster groups plan only the models of the listed namespaces
  (`UtilizationShare.InClusterGroup`). Every other model is frozen there
  (`namespace not in utilizationShare.clusterNamespaces`), today's optimizer
  keeps it, and its GPUs stay outside the group's budget. It is how a cluster
  quota is canaried; an empty entry is a validation error.
- An `optimizer:` block in a **namespace-local** map is ignored and logged once
  per change, so whoever wrote it learns it had no effect rather than assuming it
  did. So is a `namespaces:` entry that no namespace quota names.

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
entry only, falling back to the weight-1 class, logged once per change.

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

Rules: `0 < tolerance < 1`; `reserveGPUs ≥ 0`; `maxHeadroomReplicas` an
integer in `[0, 10000]` (absent means 1); `0 < returnSpareWhenLoadAtMost < 1`
(absent means 0.6); class weights `> 0`, finite, and
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
| configured release bound | the stabilization window of the donors' ScaledObjects (`spec.advanced.horizontalPodAutoscalerConfig.behavior.scaleDown.stabilizationWindowSeconds`, 300 s if unset), plus the HPA's 15 s sync, plus the KEDA `pollingInterval` (30 s if unset), plus the donor pods' `terminationGracePeriodSeconds` from the scale target's pod template (leader and worker templates for an LWS; 30 s if unset). Each input is the **largest among the group's donors**: the timings are per group, set by its slowest donor, not per transfer. The scale-down `policies` are **not** included: a Pods/Percent policy that paces a release past the window (why a ten-replica release measured 420 s, not 300) is covered only by the 1.5× margin of the timeout below, and, once three releases have completed, by the measured release time |
| release timeout | 1.5 × the configured release bound + 2 cycles — from configured values, an upper bound, so a release that is merely slow is not aborted |
| reversal hold | 2 × release time, from a transfer's start (the opposite direction), or from a cancellation (that donor → receiver pair only) — the *measured* time, so a long drain grace that is rarely used does not stretch it |
| decide-to-serve latency | release time + the replica start + 3 cycles. The replica start is the saturation analyzer's measured start-to-Ready time of the group's slowest role (`AnalyzerResult.RoleStartSeconds`, `ShareReplicaStart`, at most 30 minutes), and a fixed 5 minutes until one is measured; its source is logged as `start` |
| swing window | 8 × decide-to-serve latency |
| fill timeout | the KEDA `pollingInterval` + the HPA's 15 s sync + 2 cycles + 1 minute of scheduling — Filling ends at *scheduled*, so this covers the scale-up's way through KEDA and the HPA and the scheduler, not model load. For an LWS receiver, + 1 minute for gang scheduling |
| weight clamp | the smallest and largest class weights |
| default class | the class of weight 1 |

**Constants** — in one Go file, each cross-referenced to the §6.7 simulation:

| value | constant | why it is not a knob |
| --- | --- | --- |
| confirmation | 2 cycles | part of the decide-to-serve latency the other values assume |
| pace | 2 replicas per role per cycle, 2 transfers in flight per group; a hard imbalance that holds a second cycle is not paced up to the receiver's need, returns are not paced, and the fill is not paced below need (§6.2) | simulated at these values |
| idle floor | 0.05 utilization | stops idle models churning; not a preference |
| floor warning | floors holding half the budget above need | decides only when a line is logged |

If field data argues for a different constant, the simulator is re-run with it
first and the constant changed second. It does not become a knob.

**Every derived value is reported with its source.** A gauge
`wva_utilization_share_effective_seconds{param, source}` (`param` =
`window`, `release-timeout`, `fill-timeout`, `reversal-hold`, `swing-window`;
`source` = `measured`, `scaledobject`, `pod`, `default`, or `policy` when
`stabilization` sets the hold or the swing window), and the sources in the
`Utilization share: ledger started` line when a group's ledger is (re)built. A
log line per group whenever a value changes is not built (§13). An operator can
always see the value in force. That is what quietly disappears when a setting is
removed, and it must not.

## 9. Observability

The user-facing outcome is headroom, in the words of §3. New conditions become a
`reason` on `wva_model_scaling_blocked`, not new gauges.

| series | labels | meaning |
| --- | --- | --- |
| `wva_utilization_share_headroom` | `model_name`, `role`, `exported_namespace` | `x_r`: the spike the role absorbs before scaling (negative = short) |
| `wva_utilization_share_target_gpus` | `model_name`, `role`, `exported_namespace` | `Ĝ_r`, the continuous target the band is judged against, in GPUs |
| `wva_utilization_share_actionable` | `model_name`, `role`, `exported_namespace` | 1 when the role is out of band and off its integer target — a move could fix it (§6.1) |
| `wva_utilization_share_replicas_to_move` | `accelerator_type`, `scope` | replicas the integer target would move; in shadow mode, what would be planned |
| `wva_utilization_share_withheld_total` | `accelerator_type`, `scope`, `reason` | transfers not planned: `reversal-hold` (every move a hold withheld: the reversal hold, §6.7 rule 4; a cancelled pair, §6.3; an aborted donor's back-off; an exhausted donor) / `not-actionable` (§6.1 step 4) / `return-not-calm` / `return-role-short` / `return-receiver-unplaceable` / `donor-peak` / `awaiting-return` / `scale-down-band` (§6.2). The swing rule withholds nothing; it changes the need a role is planned on, reported by `wva_utilization_share_swinging` |
| `wva_utilization_share_swinging` | `model_name`, `role`, `exported_namespace` | 1 while a role is planned on its mean need (§6.7 rule 5) |
| `wva_utilization_share_actual` | `model_name`, `role`, `exported_namespace` | `u_r` |
| `wva_utilization_share_spare_gpus` | `accelerator_type`, `scope` | `S`; negative when the quota is short |
| `wva_utilization_share_headroom_capped` | `accelerator_type`, `scope` | 1 while the group is contended (`Σ_r P_r > B_net`, §5.1) and each role's headroom is capped; 0 while the spare is spread. A condition on the group, not on a model; while acting only |
| `wva_utilization_share_floor_excess_gpus` | `model_name`, `role`, `exported_namespace` | GPUs a floor holds above need (§5.5) |
| `wva_utilization_share_transfers_total` | `accelerator_type`, `scope`, `outcome`, `urgent` | `done` / `fill-timeout` / `cancelled` / `aborted` / `redirected` / `wrong-pod`; every outcome × urgent series is published at 0 for an acting group, so `increase()` sees the first event (`constants.UtilizationShareOutcomes`) |
| `wva_utilization_share_reserve_debt_gpus` | `accelerator_type`, `scope` | `reserveDebt`: reserve GPUs spent and not yet refilled (§6.2) |
| `wva_utilization_share_effective_seconds` | `accelerator_type`, `scope`, `param`, `source` | the derived timings in force and where each came from (§8.4) |
| `wva_utilization_share_promised_gpus` | `accelerator_type`, `scope` | `P`: GPUs released for a receiver and not yet held by it — withheld from the warm pool, from wakes and from plans (§6.3) |
| `wva_utilization_share_release_seconds` | `accelerator_type`, `scope` | histogram, Releasing → Released |
| `wva_model_scaling_blocked` | `reason="quiet-period"` | every planned model of a group, for the one fill timeout after a restart, a leader change or the optimizer starting to act (§13, stage 2), during which nothing in the group scales |
| `wva_model_scaling_blocked` | `reason="marks-unreadable"` | every planned model of a group whose transfer marks cannot be read, so its ledger cannot start; nothing in the group scales until they can. Does not clear by itself |
| `wva_model_scaling_blocked` | `reason="whole-replica-short"` | below need while the needs fit the quota but whole replicas do not, so the worse off by weight gained |
| `wva_model_scaling_blocked` | `reason="awaiting-release"` | receiver waiting on a donor |
| `wva_model_scaling_blocked` | `reason="quota-short"` | below need, and the group as a whole is short. Naming the cause in the log (`causedBy`: load, whose floor, or which woken model) is not built (§13) |
| `wva_model_scaling_blocked` | `reason="floor-pinned"` | floor above the share (§5.5); only the configured `minReplicaCount` counts, not the last replica a running role keeps (§7.2) |
| `wva_model_scaling_blocked` | `reason="floors-exceed-quota"` | `Σ F > B_net`, over the configured floors only (`ShareRole.MinFloor`) |
| `wva_model_scaling_blocked` | `reason="donors-at-floor"` | out of band, no donor can give |
| `wva_model_scaling_blocked` | `reason="no-compatible-donor"` | out of band, and no donor set opens a fitting hole for every pod of a receiver replica — without node information, no single donor pod is large enough (§6.5) |
| `wva_model_scaling_blocked` | `reason="no-node-fits"` | below need, and with node information no node holds a receiver replica even if every pod the group's other giving roles run on it went (`allocation.ShareNoNodeFits`). Counted from the pods, marked and terminating ones included, so returns do not change it: set every cycle while the role is short and receives nothing, and it replaces `donors-at-floor`, `no-compatible-donor`, `reversal-hold`, `transfer-limit` and `burst-ending` on that role (issue #236) |
| `wva_utilization_share_claims_total` | `accelerator_type`, `scope`, `outcome` | wake claims on Releasing transfers: `redirected` / `refused-score` / `refused-fit` / `refused-held` / `none-releasing` |
| `wva_utilization_share_donors_per_transfer` | `accelerator_type`, `scope` | histogram: donor replicas funding one receiver replica |
| `wva_model_scaling_blocked` | `reason="release-shape-mismatch"` | GPUs released but the receiver's replica does not fit them (§6.5) |
| `wva_model_scaling_blocked` | `reason="release-taken"` | GPUs released, then occupied by a pod WVA did not place (§6.3) |
| `wva_model_scaling_blocked` | `reason="release-timeout"` | the role's last release was aborted; it backs off before giving again. Only an aborted release |
| `wva_model_scaling_blocked` | `reason="donor-not-steerable"` | the role's pods could not be marked when it was asked to give (a Deployment pod not Ready or not scheduled, a Deployment mid-rollout, no deletion cost left between earlier live marks and the lowest unmarked sibling, an LWS whose next group to go is terminating or unscheduled, or the patch failed); it backs off as after an abort, and the reason lasts only the back-off. A donor whose every pod is already given to live transfers is only held (`HoldGiving`), with no reason |
| `wva_model_scaling_blocked` | `reason="donor-changing"` | a donor is held because its workload is changing (rollout, a pod not yet created, scheduled or Ready); on the donor while it is not short, and on each role whose transfer it refused while that role is short, not receiving, and held by nothing else. Ends once the workload reads stable for the confirm cycles, or one release timeout at the latest |
| `wva_model_scaling_blocked` | `reason="reversal-hold"` | short and actionable, but it gave within the reversal hold (§6.7 rule 4). Not reported when the role is at its threshold and another role would stay calm after giving it a replica: that move is a hard imbalance and the hold is lifted (§6.2). A cancelled pair sets it on neither role |
| `wva_model_scaling_blocked` | `reason="swinging"` | planned on its mean need (§6.7 rule 5) |
| `wva_model_scaling_blocked` | `reason="transfer-limit"` | short and actionable, while the group already has the most donor transfers in flight (2). Not for a role whose moves are unpaced: a hard imbalance from its second cycle, from a calm donor, whose model's burst is not expected to end within 1.25 move latencies (none is, with no completed burst on record) |
| `wva_model_scaling_blocked` | `reason="burst-ending"` | short with its hard imbalance confirmed, but its model's burst is expected to end before a move lands, so it receives at the pace (§6.2) |
| `wva_model_scaling_blocked` | `reason="fast-swing-capped"` | its headroom is capped on its mean need, below its reading: it swings faster than a move beside a long burster (§5.1) |
| `wva_utilization_share_mode` | `mode` | 1 for the mode in force — `off` / `invalid` / `shadow` / `active` — 0 for the others; published by the leader at the start of every cycle, from its first, including cycles with no active model, so an alert needs no `absent()` |
| `wva_utilization_share_in_flight` | `accelerator_type`, `scope`, `state` | transfers in flight by `state` (`releasing`, `filling`), idle fills included; while acting |

The two gauges at the end are the exceptions to "reasons, not gauges": neither is a
condition on a model. The mode is one series set per controller, and the
in-flight count is per group.

Gauges are replaced each cycle and deleted when the optimizer stops or a cycle
has no active model; the counters and histograms are not deleted, and keep their
values. `release-taken` and `release-shape-mismatch` are reported for one release
timeout after the fill that set them, and `donor-not-steerable` and
`release-timeout` for the back-off, so they are alerted on by rate, not with a
long `for:`. `wva_model_scaling_blocked` carries no `role`; a P/D owner reads
the role from `_headroom` and `_actionable`.

Transfers are also recorded as Kubernetes Events on the donor's and the
receiver's scale targets (`internal/engines/steadystate/utilization_share_events.go`),
for the model owner who reads `kubectl describe`, not the controller's log:
`UtilizationShareGiving` and `UtilizationShareReceiving` at start,
`UtilizationShareReceived` when a transfer lands (an idle fill's says it came
from the quota group's unassigned GPUs, and whether because its demand rose
past what it held or as headroom that is given back when the model no longer
needs it), `UtilizationShareCancelled`
(both sides) on a reversal (a cancelled return says the model's demand came
back, so it keeps the replica it was giving back), `UtilizationShareRedirected` on the receiver when a
wake claims its GPUs, and the warnings `UtilizationShareReleaseAborted` (with
the end of the back-off, or, when its donor set could not complete, saying so
and setting none), `UtilizationShareWrongPod` (the donor stays one
replica lower), `UtilizationShareFillTimedOut` and
`UtilizationShareDonorNotSteerable` (the cause and the end of the back-off).
`UtilizationShareGiving` names the pods that go and why: the receiver is below
its need, or headroom by weight, or that the move skipped the waits
(`stabilization.skipWaitsWhen`); a set contributor names the receiver its set
funds, a refill names the scale-from-zero reserve, and a spare return
(`returnSpare`, §6.2) names "the quota group's unassigned GPUs", because the
model holds more headroom than `maxHeadroomReplicas` gives it while no model is
short and stays calm without it; its replica drains after the scale-down
window, and is kept if its demand comes back before then.
`wva_utilization_share_starts_total{kind, skipped_waits}` counts what started
(`transfer`, `fill`, `refill`, `return`), so a return is no longer counted as a
transfer.
A return is also logged, `Utilization share: spare returned to the quota`
when its replica is released, and every `transfer started` line carries
`returned`. An Event names the other
party only within its own namespace; across namespaces it says "a model in
another namespace of its quota group". An aggregated model is named without a
role.

Every log line the optimizer writes starts `Utilization share:`. The
`would rebalance (shadow)` and `rebalancing` Info lines are written when a
group's set of actionable roles changes, not every cycle. One `V(DEBUG)` line per
group per cycle, `Utilization share: evaluation`, carries the whole table —
each role's held GPUs, continuous and integer target, headroom and band
verdict — so "why did my model lose a replica" is answerable from one line.
Every target the optimizer sets carries the decision reason `utilization-share`
(`DecisionReasonUtilizationShare`).

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
| derived timings (§8.4): measured release from the ledger, decide-to-serve latency estimated from it; configured bounds from the ScaledObject and pod template | `internal/engines/allocation/utilization_share_timing.go` |
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
  log line for B names C's floor, A gets none, and `floorExcessGPUs`
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
  settles in the minimum number of transfers with none wasted; no
  three-model scenario wastes a transfer; the 30-minute swing stays below
  2.5 × standing still. With five or more models swinging at 30 minutes, the
  hard-imbalance exemption can undo a move within the reversal hold (at most
  2 moves in three 6-hour runs: 1 at N = 5, 2 at N = 16, none at N = 4 or 8);
  15-minute swings waste none.
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
  LWS donor offers only the group it removes next, with a hole on each of its
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
       after it, reported as the blocked reason `quiet-period` on every
       planned model of the group;
     - the overlay on the cycle's decisions under the `utilization-share`
       reason, which the sticky scale-down hold stands down for;
     - section 9's `_withheld_total`, `_actual` and `_floor_excess_gpus`, and
       the utilization-share reasons on `wva_model_scaling_blocked`:
       `awaiting-release`, `quota-short`, `floor-pinned`,
       `floors-exceed-quota`, `donors-at-floor`, `no-compatible-donor`,
       `release-timeout` (an aborted release only), `donor-not-steerable` (a
       donor whose pods could not be marked), `reversal-hold`, `swinging` and
       `transfer-limit`, plus `release-taken` and `release-shape-mismatch`
       below. They are a third reason-owner set, published only
       while the optimizer acts (in shadow mode "blocked by the optimizer"
       would be false) and cleared when a model stops being planned.
       `donors-at-floor` means the role is short and every other role holds
       no more than its floor. It does not also require the role to be out
       of band: the continuous targets already absorb floors, so a role whose
       GPUs are all pinned elsewhere sits in band at its shortfall.
     - donor sets without node information (section 6.5), the half that is
       valid everywhere:
       - When no single donor replica can host a receiver replica, the
         planner funds each receiver pod, largest first, by a donor pod of
         its own at least its size. It first uses a spare pod of a replica
         already in the set, then takes a new replica from the donor with the
         most to spare, smallest fitting pod.
       - Several smaller pods are never added up, so a receiver needing one
         16-GPU pod is not funded by two 8-GPU donors.
       - The whole set passes section 6.2 admission, with the holds, the pace
         and the concurrency limit applied to every member.
       - A set is one primary transfer, which carries the receiver, plus
         contributors that carry none, linked by `SetID`, which is also
         written into the donor marks.
       - The receiver is raised only once every member has released.
         Contributors' released GPUs are promised to it meanwhile.
       - Cancelling the primary cancels the contributors that still can be.
       - A set starts whole or not at all: if one member's pod cannot be
         marked, the whole set is dropped.
       - Measured by `wva_utilization_share_donors_per_transfer`.
     - wake claims (section 6.3), through one mutex-guarded store in
       `internal/decision`:
       - Each pass, every active group publishes the transfers still
         Releasing for a receiver, with the receiver's score at what it holds
         and the donor pods' GPU shape.
       - A scale-from-zero wake that found no capacity tries a claim, with
         its cheapest decode variant. A claim succeeds when the wake scores
         strictly lower (ties go to the receiver) and its replica fits the
         donor pods (`decision.PodsCover`, which `ShareCovers` now uses
         too). The wake's own namespace group is tried first, then the
         cluster group, where a namespace with its own map claims nothing.
       - The steady-state engine applies its group's claims before observing
         releases. The transfer becomes a release with no receiver, so the
         wake's waiting pod takes the hole, and its donor marks are
         rewritten to match.
       - Counted as `outcome="redirected"` on `_transfers_total`, and on
         `_claims_total` with `redirected`, `refused-score`, `refused-fit` or
         `none-releasing`. A refused claim is counted once per change, since
         the wake loop runs at 10 Hz.
     - the reserve (section 7.2). `reserveGPUs` comes out of every group's
       budget, so those quota GPUs stay free for a wake. Committed GPUs above
       the budget are reserve debt (`wva_utilization_share_reserve_debt_gpus`).
       The planner pays it back first, without waiting for confirmation, by
       refill transfers: a donor above its whole-replica target and its floor
       is lowered, and no receiver is raised.
       Unlike section 6.2, the debt is derived (committed minus budget), not kept
       in a separate ledger. Section 6.2 warned against deriving it because a
       fixed consumer's unfunded gap would read as debt, but fixed consumers are
       outside this budget. Debt therefore also covers a planned variant scaled
       past its target from outside the optimizer, which the refill likewise
       takes back.
     - restoring from marks only what this controller would have written. A
       mark is read from the donor variant's own pods (Deployment pods are
       matched by ReplicaSet ownership, not only by labels). It must name that
       donor variant, a receiver variant of the same group, both replica sizes,
       and a start in the past. Anything else is removed with a WARN.
     - concurrent transfers from one donor mark distinct pods, and each
       transfer unmarks only its own. Each later mark's cost sits above the
       earlier live marks and below every unmarked sibling (`markCost`), so
       the ReplicaSet removes pods in the order the ledger releases them; no
       room for such a cost, or a sibling at the int32 minimum, refuses the
       donor (`donor-not-steerable`). A donor whose every pod or group is
       already marked by live transfers (`errDonorExhausted`) is held until
       one of them lands, or one release timeout (`ShareLedger.HoldGiving`):
       no abort, no back-off, no blocked reason, no Event.
     - while whole replicas cover every role's claim
       (`WholeReplicaCoverage`), no donor is taken below its own claim
       rounded up to whole GPUs (`shareKeep`; the claim is capped at the
       ceiling, so a role needing more than its ceiling is held to the
       ceiling). The integer targets already hold that there; the keep is
       defence in depth. Covered only in fractions, keeping a large-replica
       donor at its need can hold a receiver far below its own (9 of 16 kept
       against 1 of 6), so only the floor holds, as when short (§6.2), and
       the role below its need shows `whole-replica-short`. Reserve refills
       keep only the floor: the debt is GPUs the budget does not have.
     - a donor is in band at or above its own whole-replica target
       (`donorInBand`, still the test for a donor set; a single donor is
       held to its target alone since the simplification), whatever its
       continuous band says: a band wider
       than a replica's step put that target outside it, and a short
       receiver starved for good (152 of 20 000 random fleets, 59 of them
       all 8-GPU replicas). And a role below its need is never in band
       while its continuous target covers that need (`EvaluateShare`): the
       band is never narrower than half a replica, so for whole-node
       replicas it hid shortfalls of up to four GPUs (8 held of 8.5
       needed). The need is judged against `min(N_r, C_r)`: a role needing
       more than its ceiling counted in band one replica below it, and never
       took its last replica (7 of 8 in the cluster scenario).
     - a donor whose workload is changing (`errDonorChanging`) is held
       (`ShareLedger.HoldChanging`) with no abort, back-off or Warning,
       until `settleChangingDonors` reads its workload stable for the
       confirm cycles (`ShareLedger.ObserveChanging`), or one release
       timeout at the latest. Each held cycle logs the cause at Info and
       sets `donor-changing` on the donor and on the receivers it
       refused (`ShareLedger.ChangingBlocks`) (issue #290: a fixed hold kept a donor ten minutes after
       its idle-fill pod was Ready, silently). Changing is: a Deployment mid-rollout by its status
       (`scaletarget.RollingOut`: updated replicas below replicas, or a
       spec not yet observed) or by pods of two ReplicaSets, with fewer
       active pods than `spec.replicas`, or with a pod not yet scheduled or
       Ready; an LWS in a rolling update (`UpdateInProgress`, or groups of
       two template revisions at or above its partition,
       `scaletarget.LWSPartition`), missing a group below `spec.replicas`,
       or whose next group is terminating or unscheduled. Only a sibling
       cost with no room and a failed patch are faults
       (`donor-not-steerable`).
     - the mark's cost is judged against the siblings that stay, not the
       pod being marked.
     - an exhausted donor's hold ends when it has no live transfer left
       (`ShareLedger.GivingBusy`), and is not counted as a reversal hold.
     - a ledger's first cycle creates the outcome counters at 0 and counts
       its outcomes on the next (`countOutcome`, `flushUncounted`). A mark
       that expired while no controller ran is removed, logged with its
       pod, and not counted: its outcome cannot be told from it, and a
       tenant can write such marks.
     - transfer marks that cannot be read set `marks-unreadable`, not
       `quiet-period`: the freeze does not end by itself.
     - whole-node receivers (reviewed with 8-GPU pods):
       - a fill that times out with the receiver's pods Pending undoes the
         raise and holds the receiver from funding for one release timeout
         (`ShareLedger.HoldFill`, `FillHeld`): a Pending pod holds no GPU,
         so kept raised it read as idle budget and the receiver ratcheted up
         a whole node per fill timeout (3 to 22 replicas in 20 minutes in a
         probe).
       - an LWS receiver's fill is judged by its FILLED replicas -- groups
         with every pod scheduled (`VariantMetadata.FilledReplicas`,
         `ShareGroup.Filled`, `ShareLedger.ReceiverFilled`) -- not held
         ones: a group whose leader alone was bound ended the fill and
         freed its other holes.
       - a donor set counts once against the concurrency limit
         (`InFlight`), and one donor may give a set as many replicas as the
         receiver has pods (`shareSetPace`): an LWS receiver of three or more
         8-GPU pods was never funded from 8-GPU donors.
       - marks written earlier in a cycle are read at the cost they were
         written with (`markDonorPods`'s `written`), so three marks on one
         donor in a cycle order strictly.
       - today's optimizer decides with promised GPUs withheld
         (`decideV2`), so it does not scale a model into a whole node freed
         for a share receiver; promised GPUs are taken from the freest node
         in one piece (`shareFreeNodes`), so they do not hide a whole free
         node; and an LWS receiver gets the gang-scheduling allowance in the
         fill timeout.
     - a transfer released by count -- another of the donor's pods went --
       unmarks its surviving marked pods, so a restart inside the release
       timeout cannot restore it and move it twice.
     - every outcome of `_transfers_total` published at 0 for an acting
       group, so the first event of an outcome is an `increase()`.
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
       `_promised_gpus`, `_effective_seconds`, `_swinging`, `_in_flight` and
       the `_release_seconds` histogram; `wva_utilization_share_mode`, set by
       the leader at the start of every cycle (`observeUtilizationShareMode`),
       which also removes the marks on any cycle it is not `active`, cycles
       with no active model included. A cycle with no active model clears the
       share gauges.
     - the transfer Events on the donor's and receiver's scale targets
       (section 9), naming the other party only within its own namespace,
       including Cancelled, Redirected and DonorNotSteerable.
     - `clusterNamespaces` (section 8.1), the cluster-quota canary.
     - a role that holds a replica never gives its last one: its floor is at
       least one replica, so parking stays scale-to-zero's decision (section
       7.2). That replica is not reported as a floor: `floor-pinned`,
       `floors-exceed-quota`, `_floor_excess_gpus` and the floor-heavy log
       count only `minReplicaCount` (`ShareRole.MinFloor`).
     - donor pod choice on a Deployment: the lowest existing deletion cost,
       then the most recently Ready (the coldest cache; only a preference,
       since the mark's cost decides), then the name. The marked pod's cost is below every unmarked sibling's: -1000,
       or one below the lowest cost a user set on a sibling, recorded in the
       mark's `cost`; unmarking restores the previous cost only if the pod
       still carries that exact value. A Deployment with a pod not Ready or
       not yet scheduled, or mid-rollout (by its status, or live pods of more
       than one ReplicaSet, listed by the Deployment's `spec.selector` so a
       rollout that changed a template label is still seen), is held as
       changing (see above), not refused. On an LWS the group that goes is the highest
       index below `spec.replicas` (those at or above are already going) that
       no live transfer has marked, over its own (StatefulSet-owned) pods,
       terminating ones included; that group terminating or unscheduled
       refuses the donor (below `spec.replicas` it is a rollout), and a pod
       that only carries the LWS's labels is ignored.
     - marks record the controller's `CONTROLLER_INSTANCE` (omitted when
       unset), and the sweep that runs when the optimizer stops acting removes
       only its own; renaming the instance orphans the old marks. A mark names
       its receiver only when the transfer -- or the donor set it belongs to,
       judged whole (`sharePrivate`) -- has its donors and receiver in one
       namespace: otherwise every member's mark carries no receiver, receiver
       GPUs or set link, and a restart restores each as a release with no
       receiver, the receiver being planned again after the quiet period.
       Transfer IDs end in a random part.
     - node-aware placement skips cordoned and not-Ready nodes, nodes
       with a `node.kubernetes.io/...` `NoSchedule`/`NoExecute` taint, and
       nodes an autoscaler is removing (`ToBeDeletedByClusterAutoscaler`,
       `karpenter.sh/disrupted`, `karpenter.sh/disruption`); a pool's own
       taints are not read (its pods tolerate them).
     - the restart quiet period still pins every planned variant for one
       fill timeout. Leaving the unrestored ones to today's optimizer was
       tried and reverted: today's optimizer lowered one model and raised
       another in the same cycle, the race the quiet period exists to
       prevent while a fill in flight has no mark.
     - the per-node half of donor sets (section 6.5):
       - the usage refresher's one pod walk also yields each GPU node's
         capacity, requested GPUs and labels
         (`gpunodes.DiscoverUsageWithNodes`), published as
         `decision.NodeGPUStore` and used for two minutes at most. The
         refresher now runs periodically whenever the optimizer acts, quota or
         not.
       - with node information, a hole is a node's free GPUs plus the chosen
         donor pods on it (`shareNodeSet`). Receiver pods are placed largest
         first, into the smallest hole that fits, else on the node needing the
         fewest further donor replicas. Free GPUs complete a hole but never
         stand in for a donor's quota: where the placement needs fewer donor
         replicas than the receiver's quota -- none, when its pods fit free
         GPUs on a cluster whose quota is smaller than its nodes -- donor
         replicas from any node make it up. (The kind e2e found this: with
         free GPUs on the node and the quota spent, nothing was funded.)
         When the pods fit free GPUs and the quota has the receiver's replica
         idle -- measured as the fill measures it, less what is held for
         wakes and capped by the cluster's free GPUs -- the receiver is left
         to the idle fill: no donor shrinks, neither another domain nor the
         node-blind search is tried, and it is not reported unfunded. The fill
         records whom it leaves short, and the planner does not leave those to
         it again, so a mismatch between the two costs a cycle, not a stall. The search runs before the node-blind
         one, which remains the fallback. The whole set passes the same
         admission as a node-blind set, the concurrency limit counted across
         all its donors.
       - only schedulable nodes count, and the GPUs already promised to a
         Pending receiver or held for a wake are taken off the nodes with the
         most free first: a Pending pod has no node, so they would otherwise
         show as free twice.
       - within one cycle, a started node-aware set spends its node state: the
         free GPUs it placed into and its donor pods are not offered to the
         next receiver.
       - a receiver LWS with the exclusive-topology annotation keeps every
         hole in one domain, trying each domain in turn; a node without the
         label is in none. It never falls back to the node-blind search, with
         node information or without.
       - a Deployment donor offers each Ready pod as a replica, lowest
         deletion cost first, then the most recently Ready, then by name,
         and nothing while any of its pods is not Ready. An LWS donor offers
         the group LWS removes next: the highest below `spec.replicas` that
         no live transfer has marked.
       - the planned pods, and only they, are marked; a mark carries
         `planned`, so a restart keeps the next check.
       - checking which pod went: a planned transfer whose donor shrank while
         a planned pod still runs ends `outcome="wrong-pod"`. Only planned
         transfers (`PlannedPods`) are checked: without node information, or
         for a replica that only pays quota, a release is judged by count and
         a different pod leaving counts as done. The receiver is
         not raised, the donor stays lowered and its GPUs return to the
         budget. Not the donor's fault, so it does not back off. A set
         contributor that ends so keeps its primary from releasing; the set is
         then broken and the primary aborted. A planned pod that cannot be
         read (only NotFound means gone) holds the transfer for the cycle.
     - fill-timeout attribution from the receiver's Pending pods and the node
       picture: `release-taken` when fewer GPUs are free than those pods
       request, `release-shape-mismatch` when enough are free but the pods do
       not fit the nodes, and no reason when they fit (a pod is Pending for
       another cause). Reported on the receiver's model for one release
       timeout, and cleared when a transfer to it completes.
     - claims for a model that must wake a prefill with its decode: the wake
       claims one releasing transfer per role it must start, all or none, on
       one accelerator (`ShareClaimStore.ClaimSet`: replicas matched largest
       first, each preferring the best-off receiver's transfer, backtracking
       when a later replica would be left without one). A role already serving
       is not claimed for. The steady-state engine then applies each claim on
       its own: the wake has happened, and its pods take whatever hole opens
       first, so refusing the redirect of a claim whose partner moved on would
       only raise the original receiver into a pod that stays Pending.
     - the kind e2e, single role and P/D: shadow evaluates and touches nothing.
       Active, an idle model's marked pod is the one its ReplicaSet removes,
       and the loaded model grows only after the release. A P/D decode LWS
       grows by one group funded by two of an idle model's replicas as one
       set, raised only after both released. On nodes with every other GPU
       taken by pause pods outside WVA, a 2-GPU receiver pod is funded by two
       1-GPU donor pods the node-aware search planned on one node. The first
       cluster run found that the engine keyed scale targets by Deployment
       name, not variant name, so no donor could be marked.
     - the warm pool's carve-out (section 7.2). Each reconcile pass, a pool
       publishes the part of its target it does not hold
       (`decision.WarmPoolUnheldStore`, believed five minutes):
       - the target is its size -- reserve + lent + 1 (`SizeFor`) after the
         contention hold, before the headroom cap -- times its Pods' GPUs;
       - held is its members' GPUs, or its scheduled Pods' when the pool
         Deployment says more (`PoolShapes`), so a Pod still starting is not
         counted twice;
       - a pool none of whose Pods is a member yet -- the first Pod in a full
         quota -- takes its GPUs per Pod and accelerator from its Deployment's
         template;
       - a pool that makes no progress into its carve-out for ten minutes
         (`CarveStall`) stops being carved for, with a WARNING: something the
         carve-out does not relieve is stopping it.
       The figure is taken out of every group whose bound the pool spends: its
       namespace's own quota group, and the cluster group, which every pool
       counts against. A pool yielding to a denied model publishes nothing,
       and an empty pass clears the figure.
     - re-planning after a wrong pod: the GPUs that did come free are idle,
       and the idle fill raises the receiver only where its pods fit the free
       node picture (`ShareFitPods`, in its domain when it has one), spending
       it. Without node information the fill counts GPUs, as before.
     - holds after the different-burst-shapes cluster runs (well-lit path,
       [measured](../well-lit-paths/utilization-share/measured.md)), where
       one model ran its whole burst short twice, first through a cancel and
       then through the reversal hold:
       - a cancel removes exactly its own moves. Moves are tagged with their
         transfer's ID, and the last-gave and last-got times and the swing
         state are re-derived (`ShareLedger.rederive`), so a cancel after
         confirmation works too; the first version restored a snapshot that
         confirmation had already deleted, and in production did nothing.
         The cancel holds only its donor -> receiver pair (`PairHeld`), not
         either role (§6.3);
       - a hard imbalance waits for no reversal hold and no confirmation
         (`shareHardImbalance`, `ShareRebalanceReceiverLoad` 0.9,
         `ShareRebalanceDonorLoad` 0.6; §6.2). It replaces the interim exemption
         for a receiver below three quarters of its need
         (`ShareUrgentHeldFraction`, `shareReversalExempt`), which is gone;
       - `reversal-hold` is not reported for a role whose hold a hard
         imbalance would lift, nor for a cancelled pair.
       Neither change has been measured on a cluster yet.

   **Not yet built in stage 2** (the design above describes them; the code does
   not do them):
   - funding for **fixed consumers** (§5.6) and for an **owed, raised floor**
     (§5.5 case 5) as entitled receivers. The only entitled transfers built are
     the reserve refill and the idle fill; a multi-accelerator model is simply
     frozen, its GPUs outside the budget;
   - the `causedBy` field on `quota-short` (§5.5 case 3), naming the floor
     responsible in the log line;
   - the log line when a model sets `priority` without a weight (§5.3);
     `priority` is ignored silently;
   - matching the receiver's `nodeSelector`, affinity and tolerations in
     node-aware placement (§6.5). Only nodes a new pod cannot land on at all
     are skipped; a receiver restricted to some nodes can be planned into a
     hole it cannot use, and the transfer ends `fill-timeout`;
   - the scale-down `policies` in the release bound (§8.4): the bound is the
     stabilization window + 15 s + polling + grace only;
   - a decide-to-serve latency measured end to end (§8.4): the code adds
     the release time, the analyzer's measured replica start (5 minutes
     until one is measured) and 3 cycles, so the queueing between them is
     not measured;
   - a log line per group whenever a derived timing changes (§8.4): the
     timings' sources are logged once, when a ledger starts.

   Decisions taken while building, not gaps: `shadow` stays default `false`
   (§8.1). The reversal hold applying to urgent receivers too (§6.7 rule 4)
   was a decision here; it is superseded by the hard-imbalance rule (§6.2),
   after a cluster run held a bursting model short for its whole burst. The refill pace guard remains untestable
   while the concurrency limit (2) equals the pace (2). Neither bounds a hard
   imbalance that holds a second cycle (§6.2): after the ceiling-8 rerun
   (well-lit path, [measured](../well-lit-paths/utilization-share/measured.md))
   lost the second burst to two-at-a-time releases of about six minutes each,
   such a move takes every replica its receiver is short of its need in one
   cycle. The same run added `maxHeadroomReplicas` (§5.1, scaled by weight
   after review), the fill credited before donors fund (`fromIdle`), and the
   spare return with its scale-down-window wait, its own calm load
   (`returnSpareWhenLoadAtMost`) and its cancel (`returnSpare`,
   `cancelReversals`; §6.2). Verification of that revision (`2892aad3`)
   limited the cap to a contended quota, made a role still giving no
   receiver, and handed returns still releasing to short roles. Not yet
   measured on a cluster. Round 16, the
   review of `38164c24` (`19e7be3f`, `5549d924`): every move but a return
   counts against the concurrency limit again; a capped role keeps at least
   what the others' peak claims leave of the budget, and is capped on the
   higher of its reading and its mean (§5.1); receivers and donor sets are
   judged on that higher need too, and a return is handed only to a receiver
   whose pod its hole fits (§6.2); an active group publishes the planner's
   evaluation, with `wva_utilization_share_headroom_capped` and a log line when
   the cap flips; and a role whose peak is not yet known counts at its ceiling,
   so the cap is on for a swing window after a restart (§5.1); since then the
   need history is stored in a ConfigMap and read back, so that window is paid
   only after a restart that finds no usable history. Round 17, the
   review of `053ebfcd` (`6abc491d`): a return starts after the confirm
   cycles, not after the scale-down window, and only its release waits that
   window; a donor's calm is judged on its peak over its scale-down window
   (`donorView`); a headroom move to a receiver at its need no longer takes a
   donor below its own peak while spread (`donor-peak`), which explained and
   fixed c16 at 40 minutes (0.123 -> 0.045, against 0.076 for `e55b02b2`); a
   peak counts only when two readings in a row reach it, a role whose history
   aged out starts it again, and a role joining a known group claims what it
   holds; a need above the ceiling is judged against the ceiling; a return
   carries its pod shape and a wake that claims a handed-over one ends it as a
   return; a multi-pod receiver waits for returns that cover it
   (`awaiting-return`); and the cap's log names its `cause`. Round 18, the
   review of `1820318e` (`d8b11b35`, `6f727fed`): the donor's recent peak
   needs two readings and applies only for receivers whose bursts are shorter
   than a move, which recovered the 40-minute whole-node shapes (whole-node
   total 22.08 -> 21.74, against 22.52 for `e55b02b2`); `donor-peak` covers
   donor sets; `awaiting-return` credits only returns whose pods fit, in whole
   replicas, funds the rest, and shows `awaiting-release` on the model; and
   the need-history store keeps the two-reading rule across a restart, writes
   once per cycle, and is optional at install. Round 19, the
   known costs of round 18: a hard imbalance stays paced when its burst ends
   before a move lands (the unpaced landing; tight 1x8 at 30 minutes 0.247 ->
   0.232), and a fast swinger beside a long burster is capped on its mean
   (the burster 0.33-0.35 -> 0.20-0.21, the swinger +0.02 to +0.04, up to
   +0.06 in other noisy shapes). A
   startup rule -- returns waiting the scale-down window while contention is
   only assumed -- was tried and dropped: it fixed a long-drain model's dip
   with no history (0.038 -> 0.000) but cost a burster beside a noisy
   long-drain model 0.305 -> 0.348 at a 5-minute window, and the cluster
   scenario's 60-minute bursts 0.190 -> 0.200. Still open: that dip cost,
   tight 1x8 at 30 minutes 0.007 behind `e55b02b2`, and square2 at 30
   minutes, which barely moved (0.334 -> 0.331). Round 20, the review of
   round 19: the burst summary (`needBursts`) takes its range from levels
   held two readings in a row and starts or ends a run only on two readings
   (A); a run under way when the history starts is not a completed burst,
   only a lower bound on the longest (B); `BurstEndsWithin` reads the last
   completed burst, not the longest, predicts no end once a burst outlasts
   it, and none for a need swinging less than 0.25 of its peak (C); a P/D
   model's roles are paced together (`ModelBurstEnds`, D); the fast-swinger
   cap needs a long burster whose peak exceeds its floor (E); a restored need
   is clamped to `MaxShareBudgetGPUs` (F); and the two pacing states are
   blocked reasons, `burst-ending` and `fast-swing-capped`, left out of the
   dashboard's blocked count (G). A return now stops where today's
   scale-down would (`scale-down-band`, §6.2). Sims identical to `cff3a02a`
   (41 osc / 160 whole-node / 42 owner rows, no row > 0.005). A second
   review of that delta found nothing high; it moved the band from the
   scale-down boundary to today's release target, judged a model's roles
   with one predicate that skips a role whose need does not swing, and
   reports `fast-swing-capped` and `scale-down-band` only where they change
   the outcome. Still open: a burst still under way does not count as long
   (`LongBursts` reads completed bursts), so a model in its first long burst
   does not yet cap a fast swinger. `MoveLatency` allowed a fixed 5 minutes
   for the receiver's start, where a whole-node replica of a large model can
   take about 10; it now takes the analyzer's measured start of the group's
   slowest role. A third review
   kept a role whose own need does not swing out of its partner's pace (its
   shortfall has no end) and compares the mean cap after the room the others'
   peaks leave. The cluster rerun on round 20 served the first burst from the
   start (p50 0.07 s against 41 s); the second reached its full fleet sooner
   than without sharing, with its tail set by one pod's queue (235 requests
   while seven pods sat idle), at 34% more GPU-seconds (well-lit path,
   measured page). Most of that cost was a bug the run found: the planner
   read the analyzer's shape-change hold -- demand held at the scale-down
   boundary of the current fleet -- as need, so each idle fill raised the
   next (A 1 -> 5 replicas with no load, the cap on). A held role is now
   sized on its demand before the hold (`AnalyzerResult.HeldRoleDemand`)
   and gives nothing while it lasts. The simplification after the merge: a
   switch-off study turned 25 rules off one at a time and reran the three
   simulators. Two that changed no row, or almost none, were dropped for the
   base behavior: a single donor's continuous-band check, replaced by its
   whole-replica target (§6.2; the band never changed a move, since a donor
   already stops at its target), and the hand-over of a return still
   releasing to a short receiver (`fromReturns`), whose GPUs the idle fill
   places anyway, so a return is always a return. Any short receiver, not
   only a multi-pod one, is now credited with the returns that will free it
   and shows `awaiting-return`. The `z` admission was kept: the study's first
   cut dropped it too, and it alone cost the whole-node P/D cells (0.027 ->
   0.038 at a 20-minute swing, churn 14 -> 26). Sims against round 20: no
   osc or whole-node row moves by more than 0.005 (three whole-node rows by
   0.002, two of them P/D and better; total 21.682 -> 21.680), and 4 owner
   rows are worse, 3 better, the worst +0.015 (N3, model A), the best -0.033
   (N3, model B). Rules the simulators cannot see stay: the return's
   scale-down band, `awaiting-return`, the keep, and a joining model's claim,
   whose test now fails without it.
   Two of the deferred items (§15) followed: a burst under way counts toward a role's longest
   (`shareBursts.sofar`), so a model in its first long burst caps a fast
   swinger beside it on its mean and stops reading as a fast swinger itself
   (sims: 0 rows worse, 3 owner rows better, the largest 0.206 -> 0.169;
   whole-node total unchanged at 21.682); and a return the fleet's load
   would let go waits while more than `returnSpareWhenLoadAtMost` x
   `queueLengthThreshold` requests have been waiting on one of the role's
   pods for the confirm cycles in a row -- for at most the donor's
   scale-down window, after which it goes, so a pod queued for good by
   routing cannot park the spare for good (`AnalyzerResult.
   VariantBusiestQueueLoad`, per variant so a group reads only its own
   accelerator's; `return-pod-busy`). Prefill is not counted: its queue is
   decode's back-pressure or the router's (439 on one pod while three sat
   idle), which a kept replica does not relieve. A P/D decode counts only
   while its KV is full, since requests waiting for their remote KV sit in
   the same count. The simulators have no per-pod queues, so the second is
   invisible to them; neither is measured on a cluster.
3. **Short window for urgent transfers**, through `wvaOwnership`, once
   managed-keda-behavior lands (§6.4), gated on a persistent hard imbalance,
   not on the load limits alone (§15).

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
  `maxHeadroom` cap would leave the rest unallocated instead. Decided by
  measurement, not by cost: `maxHeadroomReplicas` (§5.1, default 1, scaled by
  weight) caps it in replicas, because spare parked on one model is slow to
  reach another. A cap in headroom terms, or a default other than 1, stays
  open. The cap binds only while the quota is contended (§5.1). In
  simulation, small alternating bursts did worse at 40 minutes (0.076 ->
  0.147 shortfall), though their peaks fit together and the cap did not bind.
  Counting moves that skipped the waits against the concurrency limit again
  took it to 0.119 (§6.2); taking an unknown peak at the ceiling, which caps
  every run's first swing window, left it at 0.123. The rest is explained:
  while spread, ordinary headroom moves drained a slow donor below its own
  recent peak for a receiver already at its need, landing after the
  receiver's burst and just before the donor's own. Withholding them
  (`donor-peak`, §6.2) took it to 0.045, below `e55b02b2`'s 0.076.
  Whole-node 1x8 models in a tight quota with 20-30 minute bursts did about
  0.02 worse than before the unpaced hard-imbalance rule, because unpaced
  moves landed after a short burst ended; keeping such a burst's moves at the
  pace (§6.2) recovered most of it, and 0.007 at 30 minutes is still open.
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

## 15. Deferred to later PRs

Decided, not built here:

- **Holds and back-offs across a restart.** Reversal holds, swing state and
  abort back-offs live in the controller's memory; a restart or leader change
  rebuilds only the transfers in flight (from pod marks) and restarts the rest
  from zero, guarded by the quiet period alone. A donor backing off for hours
  is asked again three minutes after an upgrade. Persisting the deadlines (an
  annotation on the scale target is one place) is the fix.
- **Per-model transfer history.** The transfer counters carry group labels
  only, and Events expire after about an hour, so "when did my model give
  GPUs yesterday" has no answer in the owner's namespace. A per-role counter of
  GPUs given and received, by outcome, is the fix.
- **The urgent short window (stage 3).** Measured on two models bursting in
  turn (well-lit path, [measured](../well-lit-paths/utilization-share/measured.md)):
  at the swap the rising model started growing about one donor scale-down
  window (~5.5 min) later than under today's optimizer, because the falling
  model's headroom moves only through a transfer. A short scale-down window
  for urgent transfers is the designed answer and the next step. Simulated
  (§6.7 simulator), a 60 s release for hard-imbalance moves helped long
  bursts (whole-node, 1-hour period: 1.37x -> 1.14x the shortfall of a fleet
  that never moves; 1-GPU, 45 minutes: 1.19x -> 1.01x) but hurt 20-minute
  swings (whole-node: 1.15x -> 1.31x): a faster release chases swings that
  reverse before a replica starts. So stage 3 needs a persistence gate -- the
  hard imbalance holding for some minutes -- not only the load limits.
- **Price a donor by how long its pods take to drain.** The planner picks
  donors by `z` alone, so a donor whose pods serve 6000-token outputs and one
  whose pods serve 250-token outputs look the same, though the first takes
  minutes to release a replica and the second seconds. On the ceiling-8 rerun
  each release from the long-output model waited about 5 minutes 45 seconds
  (§5.1). Ordering donors (or charging a move) by their expected drain --
  measured per role from its releases, or estimated from its in-flight
  requests' remaining output tokens -- would take from the fastest donor
  first. Considered as option (c) beside the unpaced hard imbalance and the
  headroom cap, and not built: the cap leaves most spare unassigned, so a burst
  usually needs no donor at all, and per-role drain times need releases to
  measure.
- **Read the ScaledObject's scale-down rate policy into the release
  timeout.** The release bound counts the stabilization window but not a
  Pods/Percent `policies` rate (§8.4). An unpaced move lowers a donor by
  several replicas at once; a rate-limited policy releases them one batch at a
  time, so the tail of the move can pass the release timeout and abort.
- **Mark the least busy pod.** The pod a transfer marks to go is chosen without
  regard to the requests it is serving. An unpaced move drains several pods at
  once, and cuts whatever long requests they hold. Choosing the pods with the
  fewest or shortest requests in flight would shorten the drain and lose less
  work. It bears on the busiest-pod hold on returns too (`return-pod-busy`):
  a return goes once that hold's bound passes, and marking the queued pod
  would cut the very requests the hold waited on.
- **A draining backlog reads as need.** After a model's burst of long outputs
  ends, its backlog still reads as need while it drains, so transfers *to* it
  can start just as the other model's burst begins. The hard-imbalance rule
  (§6.2) shortens how long that lasts only when the draining model is calm
  after giving. Not fixed; a demand signal that discounts a draining backlog
  is the fix. Until then, an owner who cannot accept starting a burst short
  keeps a floor (`minReplicaCount`) sized for the burst.
- **A freed whole node is held only by priority (issue #234).** The receiver
  is raised when the donors' pods are gone, and any Pending GPU pod in the
  cluster can take a GPU of the freed node first (the transfer ends
  `release-taken`). Raising the receiver earlier, at the donor pod's
  `deletionTimestamp`, would not fix this, as an earlier draft of this item
  claimed. The node opens one GPU at a time, the receiver's pod fits only
  once all are free, and the scheduler holds nothing for a Pending pod of
  equal priority: a waiting pod that needs one GPU takes the first, however
  long the receiver's pod has been queued. Placeholder pods lose the same
  race, or are rejected by the kubelet when bound before the GPU is free, and
  would need a create grant on pods. Only priority closes it: a receiver at a
  higher `PriorityClass` (default `preemptionPolicy`) preempts the pod that
  took the GPU and is nominated to the node, which the scheduler then keeps
  from lower-priority pods. The class is the owner's to set, since such a pod
  evicts lower-priority pods whenever it waits; the controller logs the
  whole-node models that have none (`whole-node models set no
  priorityClassName`). The whole-node kind e2e "a released node taken" runs G
  at a class and expects the transfer to end `done`.
- **Resource-aware holes.** The node picture (`decision.NodeGPU`) and
  `ShareCovers` read GPUs only. A receiver needing more CPU, memory, hugepages
  or RDMA devices than its donor freed is planned into a hole it cannot use,
  and the fill timeout reports no cause. Comparing the donor's and receiver's
  pod requests for every resource, or reporting a cause, is the fix.
- **A wake reserve below a replica.** `reserveGPUs` is a quota count, not a
  reserved free node: below a replica's size (8 for whole-node models) it
  never serves a wake. Rounding it to whole replicas of the models it wakes,
  or saying so where it is set, is the fix.
- **A prefill capacity measure.** Prefill demand is read in tokens -- KV in use
  plus queued requests at their average input length -- so it does grow with
  input tokens. Once prefill has queued on its own, with decode healthy, k2 and
  mu are recorded (`computeK2`) and the throughput floor sizes it from its
  arrival rate. The gap is before that first saturation: a prefill replica is
  priced at its KV memory (k1), and KV memory is not what limits prefill;
  compute throughput is. A prefill finishes in well under a second and hands
  its KV to decode, so the tokens resident at any instant are few, and the need
  reads small even for long prompts at a high rate. A reading taken while
  decode is saturated too is withheld (`downstreamSaturated`), so a prefill
  that only ever queues with decode stays at k1. And what is learned is held
  in memory only (`computeCapacityHistory`, `saturatedThroughput`), keyed by
  input-length band (`throughputKey`), so a restart, a leader change or a
  shift in prompt length loses it. The fix is a prefill-specific capacity:
  sustained input tokens per second against a measured per-replica prefill
  ceiling, persisted. The hard-imbalance rule (§6.2) makes the gap matter
  more: a prefill whose need reads small counts as a calm donor, so its
  replicas can be taken with no hold and no confirmation. Until then, a P/D
  model with long prompts should keep a
  prefill `minReplicaCount` sized from its peak prompt-token rate (the recipe
  is in the reference, "For model owners"); with short prompts, prefill is
  rightly a donor.
- **The move latency per receiver.** `ShareTimings.MoveLatency` now takes
  the saturation analyzer's measured replica start, the slowest of the
  group's roles (`ShareReplicaStart`), so a whole-node model's 10 minutes is
  no longer judged as 5. Still open: it is one figure per group, so a small
  model beside a whole-node one is paced on the whole-node start; it is 5
  minutes until a replica of the group has started under the controller;
  and the queueing between release and start is not measured. Timing each
  transfer from decision to the receiver's pods Ready, per receiver, is the
  fix.
- **A burst needs a spread to count as long.** `LongBursts`, unlike
  `BurstsLong`, takes no `ShareLongBurstSpread`, and now that a burst under
  way counts (`sofar`) a steady need drifting across the middle of a narrow
  range can read as a long burst for as long as it stays above the
  midpoint. Where `LongBursts` decides (`donorViewFor`) that judges the donor
  on its plain need. Requiring the spread there too is the fix; it changes
  simulated rows, so it waits for its own pass.
- **The scale-down band for headroom moves.** A return never takes its role
  below where today's scale-down would leave it (`bandKeep`, §6.2), and a
  transfer to a receiver short of its need rightly may. A headroom move, to
  a receiver already at its need, still can: holding every transfer to the
  band blocked a rising P/D prefill at each handover and took the whole-node
  simulator's worst P/D cell from 0.038 to 0.093. Banding those moves safely
  needs the donor's need trend, so that a donor whose demand is falling gives
  and one whose demand holds keeps.
