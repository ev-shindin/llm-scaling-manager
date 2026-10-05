# A role at its ceiling starves every other role

## Summary

On a P/D fleet, a role that has reached its own `maxReplicas` aborts the
scale-up allocation pass for **every** role. The sibling role is left at its
current replica count no matter how large its measured demand is.

Measured on a Kubernetes cluster on 2026-09-29: decode reached its ceiling
(`maxReplicaCount: 9`) three minutes into a run and stayed there; prefill then
held a queue of up to 1119 requests for nine minutes, with the demand floor
asking for ~5 replicas and a ceiling of 10 available, and was ordered
`1 -> 1 (no-change)` in **every cycle of the entire run**.

This is not the attribution guard, not the shape hold, and not KEDA. It is one
`break` in the paired allocator.

## Evidence

The trace was `1k/6000 -> 30k/250` (`shape_swap_replay_1k6000_30k250_prefill_isolated`),
engines at `VLLM_MAX_MODEL_LEN=32768`, verified by request: both roles returned
`prompt_tokens=30001`.

Prefill's queue against decode's replica count, from the run's own polling:

    time      prefillRepl  running  waiting  reason        decodeRepl
    13:28:17     1/1          4       80     100% capacity    9/9
    13:30:04     1/1          3      588     100% capacity    9/9
    13:33:46     1/1          3      666     100% capacity    9/9
    13:36:45     1/1          3      820     100% capacity    9/9

The demand floor was **not** held and asked for five replicas
(`throughput-demand-floor`, role `prefill`):

    arrivalRate        13.3          <- a TRUE arrival rate, not a completion rate
    backlogRequests    49
    projectedBacklog   4793.96
    saturatedThroughput 4.5
    perReplicaCapacity 292026
    replicasImplied    4.93
    heldAtFleet        false         <- mayOrder was true
    heldWhy            ""
    queueJustifiedReplicas 0

So `RequiredCapacity = flooredTo/scaleUp - anticipated = 1440069/0.85 - 292026
≈ 1.4M > 0`: prefill was on the scale-up branch with real required capacity.

And yet, across every `scaling-decision` line in the run:

    max prefill tgt = 1   distinct = [1]
    max decode  tgt = 9   distinct = [1,2,3,4,5,6,7,8,9]

Decode exercised its whole range. Prefill never moved.

## Cause

`internal/engines/allocation/analyzer_helpers.go`, `allocateForModelPaired`:

```go
for _, role := range roles {
    v, capN := pick(role, variants, stateMap, available, targets)
    if v == "" {
        allPicked = false
        break
    }
    ...
}
if !allPicked {
    break            // leaves the loop for EVERY role
}
```

`costGreedyRolePick` (`cost_aware_optimizer.go`) returns `"", 0` when no variant
of a role has headroom:

```go
if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
    headroom := *state.MaxReplicas - targets[vc.VariantName]
    if headroom <= 0 {
        continue
    }
    return vc.VariantName, headroom
}
```

Decode at `targets == MaxReplicas` yields `headroom == 0` for its only variant,
so `pick("decode", ...)` returns `""`, `allPicked` goes false, and the `break`
ends allocation before any role is committed. Prefill's pick is never even
attempted on the roles ordered after decode, and when it is attempted its result
is discarded.

The joint commit is deliberate — roles are scaled together so a P/D fleet keeps
its ratio, and `deltaUtil` is the min utilisation across roles precisely so no
role is over-ordered relative to its partner. That design is sound. What is
wrong is treating "this role cannot grow" as "no role may grow": a role at its
administrative ceiling is *finished*, not *blocking*.

## Fix

A role that cannot be picked is exhausted for this pass. Drop it from the
iteration and zero its remaining demand so the loop terminates, then commit the
roles that were picked:

- collect the pickable roles into `variantByRole` as now, but on a pick failure
  record the role as exhausted and keep going instead of breaking;
- set `pickerState[role] = 0` for each exhausted role — without this,
  `anyRoleNeedsScaleUp` stays true on demand that can never be served and the
  loop spins;
- break only when **no** role could be picked;
- iterate the utilisation, `k` and commit loops over the picked roles only. This
  matters: an unpicked role has `prcByRole[role] == 0`, so `utilByRole[role]`
  computes as `0` and `deltaUtil <= 0` would break the loop anyway — reproducing
  the bug by a different route.

Roles that are pickable but already satisfied (`pickerState <= 0`) keep today's
behaviour: `roleAggRemaining <= 0` gives `utilByRole = 1.0` and `k = 0`.

## What this does not fix

Three things seen in the same run are separate and stay open:

1. **`requestRate` is a completion rate.** Under saturation a server completes at
   exactly mu, so the per-replica decision line reads `rate=4.5, mu=4.5` and the
   ratio is pinned at 1.0 by construction. The demand floor is unaffected — it
   uses the EPP arrival rate (`QueryModelArrivalRate`, 13.3 req/s here) — but any
   consumer comparing `requestRate` against mu cannot detect overload.
   `internal/collector/registration/throughput_analyzer.go:76` already says so.

2. **Prefill's throughput history is bucketed by OUTPUT length.**
   `outputBuckets = {short, medium, long, xlong, xxlong, huge}` keyed on
   `avgOutput` (`saturation/types.go`). Prefill's cost is input tokens, so a
   change in output length invalidates prefill's mu window and raises
   `staleShape` for a quantity that does not affect prefill's capacity at all.
   `staleShape` gates `mayOrder` (`floor.go:256`) and the queue escape hatch
   (`floor.go:331`). It did not bite in this run (`heldAtFleet: false`), but it
   is the wrong key.

3. **Prompt length couples the roles rather than separating them.** Under
   NixlConnector decode receives the full prompt KV, so decode's KV per sequence
   is `input + output/2`, dominated by input. Longer prompts saturate decode
   sooner, which is the condition `holdPrefillDemand` keys on. Isolating prefill
   needs a model whose prefill compute is scarce, not longer prompts.

## Validation

A unit test in `internal/engines/allocation` with a two-role model, decode at
`MaxReplicas` and prefill with headroom and positive role demand, asserting
prefill's target rises. Run it against the parent commit first: it must fail
there, and the failure is the evidence the fix works.

The cluster check is the same trace with decode's ceiling raised, or the 8B
fleet where prefill saturates at a lower rate: prefill should leave 1.
