# A release the swap can survive

**Status:** design. Nothing here is built. It completes one clause of
[WVA as a KEDA external scaler](wva-keda-external-scaler.md) §6 — the
`wvaOwnership` flag — and introduces no configuration surface that proposal has
not already reserved.

**Read §2 before §3.** The release latency this proposal shortens is measured.
The *swap* it is motivated by has never been observed firing, and the staging in
§9 is built around that gap rather than around the feature.

## Summary

When the optimizer wants to grow one role and shrink another on a cluster with no
spare GPUs, the growing role waits for GPUs the shrinking role has already been
told to give up. Measured on a Kubernetes cluster, a ten-replica release takes
**420 s** to complete after WVA first publishes a lower target, because
`behavior.scaleDown.stabilizationWindowSeconds` on the derived HPA is 300 s and
the HPA takes the **maximum** recommendation over that window.

The proposal is to shorten that one field, on the shrinking target only, for as
long as a swap is pending, through the opt-in ownership flag the external-scaler
proposal already reserves. An operator who has not opted in keeps the
ScaledObject they wrote.

This is deliberately the *weakest* lever in §5 of the parent proposal. It never
changes a replica count: it only lets the HPA act sooner on the count WVA has
already published.

## 1. What the measured run shows

Run on a Kubernetes cluster, 2026-10-01, P/D fleet (`Qwen3-0.6B`, H200,
`maxReplicaCount: 10` per role), from the controller's own
`steadystate/engine_v2.go:1237 scaling-decision` lines. `curr` is the scale
target's replica count, `tgt` is what WVA published that cycle:

    18:39:11   decode   curr=10  tgt=3      <- first lower target
    18:42:11   decode   curr=5   tgt=1      +180 s
    18:44:11   decode   curr=2   tgt=1      +300 s
    18:46:11   decode   curr=1   tgt=1      +420 s

The ScaledObject carried `stabilizationWindowSeconds: 300` with
`scaleDown.policies: [Percent 100 / 120 s]` throughout — the live patch that
replaced it with 60 s was written at 19:20 Z, after this drain, so it was not in
force here. With a 100 % policy the policy is not the limiter; the window is.

**Four hundred and twenty seconds is the number this proposal is about.** On a
600 s phase, a release that takes 420 s is a release the workload outlives.

## 2. What the run does **not** show

Two things, and both were asserted in an earlier draft of this document on the
strength of the same log. Neither survives it.

**It was not the window that held decode at ten.** From 18:33:25 to 18:39:11 both
roles sat at `curr=10 tgt=10 atMax=true` — WVA's *own* decision, at its own
ceiling, republished every cycle. The HPA was carrying out exactly what it was
told. The over-provisioning in that stretch is WVA's, and the mechanism is
visible in the same cycle: decode's demand had fallen to 303,206 tokens, and
`throughput_floor.go` re-priced it to **1,338,789** (`arrivalRate: 8.02`,
`backlogRequests: 3`, `drainSeconds: 60`, `saturatedThroughput: 3.07`,
`perReplicaCapacity: 508,472`). A ten-replica ask from a three-request backlog is
a floor question, not a KEDA question, and it is out of scope here — but it must
not be mistaken for this one, because the fix for it is in a different file.

**There was no swap in this run.** `grep -ci 'wasLimited\|ResourceConstrained\|
contention' = 0`. Both roles were at their *administrative* ceiling with the
cluster's GPUs available; nothing was denied anything. The run measures a release
latency, not a contention event.

**So the swap case is a projection, not a measurement.** The nearest real
starvation case is [a role at its ceiling](prefill-starved-by-exhausted-role.md)
— decode at `maxReplicaCount: 9`, prefill holding a queue of 1119 — and that was
one `break` in the paired allocator, now fixed, not a release latency. The claim
this proposal rests on is the conjunction of the two: *if* the allocator declines
a role for scarcity while a sibling is descending, the growing role waits out the
sibling's release, and that release is 420 s. The conjunction is sound
arithmetic. It has not been observed. §9.1 exists to observe it before anything
is written to a ScaledObject.

## 3. Why the metric path cannot shorten the release

WVA's only actuation channel today is the number it returns to KEDA. The HPA
computes `ceil(metricValue / targetSize)`, clamps it to the envelope, and
**applies `behavior`**. On the way down `behavior` decides *when*, and WVA has no
say in it.

The codebase already knows this. `internal/engines/allocation/analyzer_helpers.go`,
on its own scale-down release:

> KEDA's HPA takes the MAX over a 300 s window on the way down, so one
> re-ordered replica in twenty cycles pins the fleet for the rest of the window
> and the release is worth nothing.

Three things that look like fixes and are not:

- **Publish a lower number sooner.** Already happening — 18:39:11 published a
  target of 3 against a count of 10. The window spent the next 420 s on it.
- **Shorten the window everywhere, in config.** This is what the benchmark
  scenario now does (60 s, 50 % per 60 s) and it is the right default *for a
  ten-minute phase*. It is not free: the window is damping, and a batch workload
  where transient idle is normal wants it long. Shortening it globally buys swap
  latency by spending stability on every workload, including the ones that never
  swap.
- **Let the sticky hold handle it.** `scalingpolicy.HoldPublishedScaleDown`
  explicitly stands down when `d.WasLimited` is set, and says why: a target the
  limiter bound is the limiter's answer. The hold exists to *slow* descent, which
  is the opposite of what a swap needs.

The window should be short **while a swap is pending** and long otherwise. That
is a condition WVA can evaluate and an operator cannot.

## 4. The signal already exists

Nothing new has to be measured. Three pieces are in the tree today:

1. **"I wanted GPUs and was denied them."**
   `internal/engines/allocation/gpu_limit_attribution.go` sets
   `VariantDecision.WasLimited` and `LimitedBy` for every variant a constraint
   pool bound, and emits
   `wva_decisions_limited_total{variant,namespace,limited_by}`.

2. **"…on this accelerator, in this namespace."** The same function publishes
   `decision.GPUContention` — a `map[namespace]map[accelerator]bool` with an
   `UpdatedAt` and a staleness rule — through `decision.PublishGPUContention`.
   It has exactly one consumer today (`cmd/main.go:1120`, the warm pool's
   don't-grow-into-a-starved-replica rule). Its doc comment states the pattern
   this proposal reuses verbatim: *"this is one component telling another what it
   just observed, which is what this package is for."*

3. **"…and something else is on its way down."** `VariantDecision` carries
   `TargetReplicas` and `CurrentReplicas`; a release in flight is
   `TargetReplicas < CurrentReplicas`.

A **pending swap** on `(namespace, accelerator)` is then: at least one variant
with `WasLimited` and a `LimitedBy` naming a GPU pool — *scarcity*, which
`roleAtCeiling` already distinguishes from a variant at its own administrative
ceiling — **and** at least one other variant in the same `(namespace,
accelerator)` whose target is below its current count. The second variant's
ScaledObject is the one that gets the short window. The first one's is untouched:
scale-up stabilization already defaults to 0.

Keyed on `(namespace, accelerator)` rather than on model, because that is the
granularity at which GPUs are actually fungible and it is the key
`GPUContention` already uses. A swap between two *models* under one quota is as
real as a swap between two roles of one model, and more common.

Note what the §1 run would have produced under this rule: nothing. Both roles
were at an administrative ceiling, `WasLimited` was never set, so no swap is
pending and no field is written. That is the correct answer for that run, and it
is why §9.1 must run before §9.3 — a condition that never fires is a feature
that never helps.

## 5. The configuration surface

Unchanged from the parent proposal §6. One key in trigger metadata:

```yaml
  triggers:
    - type: external
      metadata:
        scalerAddress: wva-external-scaler.wva-system.svc:9090
        modelID: Qwen/Qwen3-32B
        wvaOwnership: "true"        # opt in
```

The key is `wvaOwnership`, not `managed`. The `llm-d.ai/managed` **annotation**
is retired and `internal/annotations/annotations.go` records why — KEDA only
calls a scaler its trigger names, so being called is what being managed means,
and an opt-in *discovery* marker has nothing left to opt into. Ownership is a
different question from discovery, which is why it survives as its own key;
`wvaOwnership` is the name the parent proposal (§6, §13), its ScaledObject
templates and `docs/plans/engine/keda-driven-discovery.md` already use. Nothing
is invented here.

Absent or `"false"` — **unmanaged**: WVA writes nothing. `behavior` is read as a
constraint, and the swap is *reported* (§8) rather than fixed. This is the
default and stays the default.

`"true"` — **managed**: WVA may shorten the scale-down window on this target,
under the bounds in §6.

A second key sets the floor, per policy tier rather than per target, because a
floor is a tier property and `scalermeta.go` argues that case for
`scalingPolicy` already:

```yaml
scalingPolicies:
  interactive:
    swapScaleDownWindowSeconds: 30     # default; 0 disables the override
```

## 6. What managed mode writes, and what bounds it

One field:

    spec.advanced.horizontalPodAutoscalerConfig.behavior.scaleDown.stabilizationWindowSeconds

Server-Side Apply, under its own field manager (`wva`), so ownership is precise
and a GitOps `ignoreDifferences` can name exactly it (parent §6.1). Five bounds,
each of which exists to keep this reversible:

- **It only ever shortens.** `min(configured, swapScaleDownWindowSeconds)`. An
  operator who wrote 60 s never gets 300 s back from WVA.
- **It never touches `minReplicaCount` / `maxReplicaCount`.** Those are the
  urgent-ceiling lever, a separate decision with a separate gate. See §7.
- **It never touches the scale-down *policies*.** The percentage and period are
  the operator's statement about how fast their workload tolerates losing
  replicas. The window is a statement about how long to wait before believing a
  fall, and only that one is WVA's business.
- **It restores on clear, with a hold.** When no swap is pending for `swapHold`
  (one default: 2 × the optimizer interval), the field is reverted to the
  operator's value — which SSA makes exact, since dropping it from WVA's applied
  set hands it back rather than guessing at it.
- **A stale contention snapshot is not a swap.** The same `maxAge` rule
  `ContentionStore.Contended` already applies, for the reason its comment gives:
  a target held in an overridden state by a reading nobody refreshed is held
  there forever and the failure is invisible.

RBAC changes by exactly one verb. `internal/controller/rbac.go` grants
`scaledobjects: get` and says so emphatically — *"Nothing writes them — KEDA owns
these objects"* — with `list`/`watch` deliberately ungranted because a cached
read re-introduces the cluster-wide informer the design exists to remove. This
adds `patch` and nothing else; `list`/`watch` stay out. That comment must be
corrected in the same change, or the next reader will trust it.

## 7. Why this and not the urgent ceiling

Parent §5 already describes lowering `maxReplicaCount` to force an immediate
scale-down that bypasses stabilization entirely. That is a stronger lever and
should remain the second one tried:

| | window override | urgent ceiling |
|---|---|---|
| what changes | when the HPA may act | what the HPA is allowed to return |
| source of the count | WVA's metric, unchanged | the ceiling, overriding the metric |
| worst case if the signal is wrong | a target descends faster than the operator chose | a target is capped below what demand asks for |
| rebound risk | none — the metric already asked for the drop | none *if* the metric agrees; a bug is visible as oscillation |

The window override cannot produce a replica count WVA did not publish. That
property is worth keeping as the first line, and it covers the measured case:
decode had already been told to go to three, and 420 s was the whole of the
delay. The ceiling is for a release that must complete in less than one HPA
cycle, which no run has yet demonstrated needing.

## 8. How it is observed

Two series, following the conventions already in the tree:

- The pending condition is a **reason** on the existing
  `wva_model_scaling_blocked`, not a new gauge — a new reason
  `swap-pending-gpu-release` in its own owner group beside
  `ScalingBlockedReasonsPolicy` and `…Wake` in `internal/constants/metrics.go`,
  so the producer owns its reason set and clears it the way the others do.
  This is the part that must work in **unmanaged** mode too: an operator whose
  prefill is starving behind a 300 s window should be able to see that, and that
  this flag would address it, without having set it.
- The override action is a counter,
  `wva_managed_behavior_overrides_total{namespace,target,field,action}` with
  `action` in `applied|restored`. An action is not a diagnostic condition, so a
  counter is right here and a reason would not be.

## 9. Staging

1. **Diagnose only.** The `swap-pending-gpu-release` reason, derived from the
   signal in §4. No writes, no RBAC change, no flag. This is first because §2
   says the condition has never been observed: until a real run sets it, the rest
   of this document is arithmetic. It is also independently worth having — it
   turns "prefill is stuck" into "prefill is stuck behind decode's release",
   which cost most of a session to establish by hand and was initially got wrong.
2. **`wvaOwnership` plumbed.** Parsed in `internal/registry/scalermeta.go`
   beside the other trigger keys and surfaced on `Meta`, with the rest of managed
   mode inert. Cheap, and parent §6 needs it for everything else too.
3. **The override.** SSA patch, the five bounds, the `patch` verb, the rbac.go
   comment correction, the counter. Gated on §9.1 having fired in a real run.
4. **Measure.** A P/D shape-swap run with `maxReplicaCount` set *below* the
   available GPU budget so the limiter actually binds — which the §1 run did not
   do, and which is why it measured nothing about swaps. Arms: 300 s unmanaged
   (the current release latency), 60 s configured globally (what the scenario
   does today), and 300 s + `wvaOwnership: "true"`. The claim to test is that the
   third lands with the second on swap latency while keeping the first's damping
   on the phases where nothing swaps. A result where it matches the 60 s arm
   *everywhere* means the condition is firing all the time and the gate is not
   earning its complexity.

## 10. What this does not fix

**The swap is still not atomic.** Shortening the window takes the release from
420 s to roughly 60–120 s. It does not make the GPU exist at the moment the other
role asks for it: `allocateForModelPaired` still ends that pass on scarcity, and
the growing role still grows a cycle or two later from the freed GPUs. The honest
claim is a latency reduction of roughly an order of magnitude on the release, not
a reservation.

An atomic swap needs the allocator to spend GPUs it is *about to* free rather
than ones it holds — a reservation against a release in flight. That is a larger
change with a real failure mode (a reservation against a release that never
completes strands capacity on both sides) and should not be attempted before the
cheap version is measured. Recorded as the follow-on, not folded in.

**It does nothing about over-ordering.** §2's 1.34M-from-303k is a demand-floor
question. A fleet that asks for ten replicas it does not need will hold them
whatever the window says.

**It does nothing for an unmanaged target**, by construction. An operator who has
not opted in gets §8's diagnostic and their own `behavior`.

## 11. Open questions

- **Does KEDA re-derive the HPA promptly on a `behavior`-only SSA patch, and does
  the HPA's own window state survive it?** If KEDA recreates the HPA, the window
  resets to empty, which makes the override *stronger* than intended and the
  restore a second reset. To be checked against the KEDA version in use before
  §9.3, not after.
- **Tier floor or target floor?** §5 puts `swapScaleDownWindowSeconds` on the
  policy tier, matching how `scalingPolicy` is argued for — a reusable tier, so
  one edit changes a fleet. A latency-critical single target may want to go lower
  than its tier. Deferred until something asks.
- **Two shrinking targets on one accelerator** both get the short window, which
  is probably right and definitely untested.
