# The composite analyzer: how analyzers combine

**Status:** built (#34, then #167 and #197), still moving. This is the design
record: the rule as built, why, what it cannot do yet, and where it is going.
How each step works, function by function, is in the developer guide:
[How results combine](../developer-guide/multi-analyzer-pipeline.md#how-results-combine).

It lives in `docs/proposals/` because it is built and still moving: two open
issues change the rule (#305, #226), and the target below replaces part of it.

## What it is

Every analyzer reports demand `D` and a per-replica capacity `P` in its own
unit, so `D / P` is a replica count. The composite is the one layer between the
analyzers and the optimizer. It reads every analyzer that ran for a model and
hands the optimizer **one** result, `CompositeSignal`. The optimizers never see
the analyzers one by one.

This replaced what the [analyzer metric interface](analyzer-metric-interface.md)
proposed, where the optimizer would combine the analyzers itself.

## The rule as built

Code: `internal/engines/steadystate/composite.go` (`buildComposite`) and
`internal/engines/allocation/composite_decision.go` (`ResolveSO`).

1. **Need per variant.** For each variant, every eligible listed analyzer gives
   `N = D[role] / P`, from its own demand and its own `P`, with no threshold
   band applied. The variant's need is the largest `N`. The path is logged as
   `C1-single` (one analyzer), `C0-agree` (several) or `C4-no-signal` (none).
2. **Saturation's units.** The result is expressed in saturation's units. Each
   role is priced in one analyzer's *frame*. A frame is one analyzer's view of
   the role: its demand and each variant's per-replica capacity, in
   saturation's units. Saturation's frame is its own figures; another
   analyzer's frame turns its need back into saturation's units (`N x P_sat`
   as demand, `D / N` as the variant's `P`). The frame chosen is the one with
   the lowest *coverage*: running replicas over needed replicas, summed over
   the role's variants. That is the frame that needs most replicas for the
   fleet the role runs; saturation's wins a tie.
3. **One band.** From that frame the engine builds one required capacity and
   one spare capacity per role, at **saturation's** `scaleUpThreshold` and
   `scaleDownBoundary`. A scale-down is then capped so it is safe in every other
   frame too.
4. **Score.** The composite's `Score` is the largest score among the analyzers
   that contributed a need, saturation's when none did. Only the cross-model
   fair share uses it to rank models; the scale-down order also reads it as a
   tie-break, where it changes nothing unless it is 0 or less.
5. **Holds.** A role can be held whatever the needs say:
   - saturation is not listed and no listed analyzer sizes the role: no
     scale-up, no scale-down; on P/D the other role does not grow either;
   - a listed analyzer other than saturation is inside its staleness grace
     (three cycles) with no reading: the roles it covers do not scale down,
     or every role when it reports no variants (the *grace veto*);
   - saturation holds the role and another analyzer's figure replaced the
     held one: the role does not scale down.
6. **No signal.** A `C4-no-signal` variant keeps saturation's demand; it is
   never read as "needs zero". When every variant is `C4-no-signal`, the model
   reports the `no-composite-signal` blocked reason, and with saturation
   unlisted every role is held.
7. **Saturation always runs**, listed or not. It is the composite's variant
   list and its unit. When the policy does not list it, it sizes nothing.

## Decisions behind it

- **Combine in the engine, not the optimizer.** One decision path in one
  place, and the optimizers stay single-signal. Before #34 the optimizer
  helpers looped over every analyzer; that version is kept, unbuilt, in
  `internal/engines/allocation/multi_backup/` (`//go:build ignore`).
- **Largest need wins.** Scale-up follows the analyzer asking for most;
  scale-down needs every listed analyzer to agree.
- **Saturation is not a fallback.** It contributes only when listed. A policy
  that leaves it out and lists nothing else that can size a role holds that
  role, rather than quietly sizing on saturation.
- **Missing is not zero.** An analyzer that missed a cycle blocks scale-down
  for its grace window instead of letting the others shrink the fleet. One
  exception today: the external PromQL analyzer reads an empty query result
  as 0 demand, a present reading
  ([#344](https://github.com/ev-shindin/llm-scaling-manager/issues/344)).
- **Saturation's units, for now.** The optimizers, the warm pool and rescale
  all read demand in saturation's units. Converting into that unit let the
  composite land without changing any of them.
- **No default capacity path.** Saturation's own estimate is the only source
  of `P` for a variant with no reading (the unbuilt `C3-default-prc` path).

## Known limits

- **One band for everything** ([#305](https://github.com/ev-shindin/llm-scaling-manager/issues/305)).
  A `scaleUpThreshold` or `scaleDownBoundary` on any analyzer other than
  saturation shapes only that analyzer's own result; the config read logs a
  warning for it.
- **Score is not on a common scale** ([#226](https://github.com/ev-shindin/llm-scaling-manager/issues/226)).
  Adding an analyzer can move a model in the fair-share order with no change
  in its need.
- **A need is dropped where saturation cannot price the variant.** If
  saturation has no `P` for a variant, another analyzer's need for it counts
  only when that analyzer's own frame is the one chosen; under any other frame
  it is dropped and logged (`a listed analyzer's need is dropped`). An analyzer
  whose needs are all on such variants gets no frame at all, so its needs are
  always dropped.
- **Accelerators rated differently.** When analyzers disagree on how much one
  accelerator is worth against another, scale-up follows the chosen frame
  alone and may leave another frame short for a cycle. The next cycle can then
  grow in the other frame, while scale-down stays capped by every frame. A
  mixed-accelerator fleet can therefore end above the cheapest fleet that would
  satisfy every analyzer. Not measured.
- **Model-level analyzers cannot size P/D**
  ([#343](https://github.com/ev-shindin/llm-scaling-manager/issues/343)). An
  analyzer with no per-role demand (the external PromQL analyzer today) sizes
  no prefill or decode variant; only a log line says so. It still takes part
  in the grace veto: when its query fails, every role of the model is held
  against scale-down for up to three cycles, although it could never size
  any of them.
- **Only saturation's variants exist**
  ([#345](https://github.com/ev-shindin/llm-scaling-manager/issues/345)). A
  variant saturation does not report is not in the composite.

## Target direction

Not scheduled. Each step needs the deterministic replay harness from
[#341](https://github.com/ev-shindin/llm-scaling-manager/issues/341) (PR 0) first,
because that is what can prove a saturation-only policy decides exactly as
before.

1. **Each analyzer judged at its own band** ([#305](https://github.com/ev-shindin/llm-scaling-manager/issues/305)): required capacity at each
   analyzer's scale-up threshold, releasable capacity at each one's scale-down
   boundary, then the largest requirement and the smallest release.
2. **Units never need to agree.** Coordinate in replicas or coverage, as the
   metric interface proposed, instead of converting into saturation's units.
3. **A composite that does not depend on saturation**
   ([#345](https://github.com/ev-shindin/llm-scaling-manager/issues/345)). Discovery supplies the
   variant list; any analyzer can state a hold per role; saturation's side
   fields (`HeldRoleDemand`, `HotRankDemand`, `RoleStartSeconds`,
   `VariantBusiestQueueLoad`, `PricingK`, the warm-pool figures) become typed
   per-analyzer hints. This goes with stage 3 of the
   [engine structure](engine-structure.md) proposal: one registry, and the
   input builder before `Analyze`.
