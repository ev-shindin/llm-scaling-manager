# Multi-Analyzer Pipeline (developer reference)

llm-scaling-manager's scaling engine runs multiple **analyzers**
in series each cycle. Each analyzer consumes the same per-replica metrics
and produces an `*domain.AnalyzerResult` carrying per-variant capacity,
model-level totals, and (for P/D disaggregated models) per-role demand.
The engine builds each analyzer's capacities, then combines every listed
analyzer into **one** result, the composite (`CompositeSignal`). The
optimizer reads only that one entry and decides scaling actions over it via
shared free functions in `internal/engines/allocation/`. Why it combines
this way, and where it is going, is in the design record:
[the composite analyzer](../proposals/composite-analyzer.md).

---

## Architecture

### Data flow per optimize cycle

```text
┌──────────────────────────────────────────────────────────┐
│ Config (config.ScalingPolicy per model/namespace)        │
│   Priority, Analyzers[]:                                 │
│     name, enabled, Score,                                │
│     ScaleUpThreshold, ScaleDownBoundary                  │
└──────────────────────────┬───────────────────────────────┘
                           │ engine reads per cycle
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Engine: per-model preparation                            │
│   • BuildVariantStates (GPUsPerReplica per variant       │
│     from ScaleTarget / VA labels)                        │
│   • CollectSchedulerQueueMetrics (shared across          │
│     analyzers)                                           │
│   • cfg.AnalyzerThresholds(name) per analyzer           │
│     (per-analyzer override over model-level globals)     │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Engine: run analyzers (runAnalyzers)                     │
│ Saturation always runs, listed or not; then each         │
│ registered analyzer the policy enables:                  │
│   • Analyze(ctx, input) → *AnalyzerResult                │
│   • buildNamedResult → buildCapacities (supply) and      │
│     applyUniversalThreshold at its own band              │
│   • published as wva_analyzer_* under its own name       │
└──────────────────────────┬───────────────────────────────┘
                           │ []NamedAnalyzerResult
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Engine: build the composite (buildComposite)             │
│   • need per variant = max over listed analyzers of D/P  │
│   • each role priced in one analyzer's frame, in         │
│     saturation's units                                   │
│   • RC/SC at saturation's band; spare capped by every    │
│     frame; holds applied                                 │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Engine: build ModelScalingRequest                        │
│   CompositeSignal  ← the composite (one entry)           │
│   VariantStates, Variants ← discovery                    │
│   Priority, Disaggregated, Weight                        │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Optimizer (CostAware or GreedyByScore; the               │
│ utilization-share optimizer reads the same one entry)    │
│   • initRoleState → RolePairedState + RoleSpare          │
│   • Scale-up: allocateForModelPaired                     │
│       pick(role) → variant; joint Δ_util commit          │
│       applyAllocation → decrement Remaining              │
│   • Scale-down: scaleDownRoleIterated                    │
│       needsScaleDownForRole → composite live, spare > 0  │
│       safeRemovalReplicasForRole → composite spare       │
│       applyDeallocationForRole → decrement RoleSpare     │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
                       VariantDecisions
```

### Key concepts

| Concept | Definition |
|---|---|
| **Analyzer** | Implementation of `domain.Analyzer`. Built in: saturation (kv-token capacity, package `saturation`), which always runs and sizes the model when listed, and `slo` (package `slo`, [its page](slo-analyzer.md)), which observes. Registered in `cmd/main.go`: throughput (RPS/ITL-derived). External: PromQL definitions under `analyzerDefinitions` (package `external`). |
| **`VariantCapacity`** | Per-variant primitives: `ReplicaCount`, `PendingReplicas`, `PerReplicaCapacity` (analyzer-specific units), `Role`, `TotalDemand`, and the warm-pool trio `WarmPoolReplicas` / `WarmPoolPerReplicaCapacity` / `WarmPoolCapacity`. `Cost` and `AcceleratorName` are **not** on this struct — the optimizer reads them from `VariantMetadata`. |
| **`AnalyzerResult`** | Per-(model, analyzer) output: the pure (D, P) signal — `VariantCapacities[]`, `TotalDemand`, `RoleDemand`. It has **no** supply or scaling-signal fields; those live on `NamedAnalyzerResult` and are the builder's. |
| **`RoleCapacity`** | Per-role aggregate within an `AnalyzerResult`: `TotalSupply`, `TotalDemand`, `TotalAnticipatedSupply`, `RequiredCapacity` / `SpareCapacity` (engine-written). Used for P/D disaggregated models only. |
| **`NamedAnalyzerResult`** | Optimizer-side wrapper: `{Name, Result, Score, Remaining, Spare, RoleSpare, Live}`, and on the composite `Holds`, `ReleaseBound` and `OtherFrames`. Working `Remaining`/`Spare`/`RoleSpare` are decremented by helpers during allocation; `Result` is never mutated. `Live` is set by the engine each cycle and gates scale-down participation (see "How results combine"). |
| **Linearity invariant** | Adding *n* replicas of variant *v* reduces an entry's working `Remaining` (the composite's, in the optimizer) by exactly *n × PRC[v]*. Holds at model scope (non-disaggregated) and at role scope (disaggregated). |

### The three layers

Everything below follows from one split, and getting it wrong is the most common
mistake in this pipeline — logic written inside one analyzer is silently absent
from the other two.

| Layer | Owns | Where |
|---|---|---|
| **Collector** | Collecting metrics: which rows exist at all. Variant identity (the owner walk to the managed scaler), the Pod-state gate, collapsing a Pod's engine instances into one replica row, and the per-row flags `Ready` and `FromWarmPool`. | `internal/collector` |
| **Analyzers** | Analysis: the **measured** signal, and nothing else. Demand `D` (model-level and per role) and per-replica capacity `P`, per variant, in analyzer-specific units. | `internal/engines/analyzers/*` |
| **Builder** | Building demands and capacities: everything **derived**. Supply totals, utilization, per-role capacities, `RequiredCapacity`/`SpareCapacity`, and `WarmPoolCapacity`. | `buildCapacities`, `internal/engines/steadystate/engine_v2.go` |

`AnalyzerResult` has no supply fields at all, so an analyzer *cannot* write a
supply that contradicts its own (D, P) — the linearity invariant holds by
construction rather than by convention.

### Warm pool bridges: demand yes, supply no

A **bridge** is a warm pool Pod lent to a variant while it is short. It reports
metrics under the variant it serves, and the collector flags those rows
`FromWarmPool`.

- Its **demand counts**. The traffic is the variant's. Leave it out and demand
  reads lowest exactly while a bridge covers the shortfall, then reappears from
  nowhere when the Pod goes back.
- Its **capacity is never supply**. The Pod is borrowed; counting it would tell
  the optimizer the fleet is already big enough and suppress the scale-up the
  bridge exists to cover — after which the pool holds the Pod indefinitely,
  because the replicas that would release it were never created.
- **`P` is measured over the variant's own replicas only.** `ReplicaCount` is
  clamped to the scale target, which a warm pool Pod is not part of, so a `P`
  blended over a bridge would price the counted replicas at a figure none of them
  delivers. The readings genuinely differ: the pool runs its engines at a lower
  `--gpu-memory-utilization` than the workload (measured on an OpenShift cluster 2026-08-31:
  pool 0.90, workload 0.95), so less of the GPU is KV cache.

An analyzer that reads replica rows must therefore exclude `FromWarmPool` rows
from its per-replica maths. `saturation` splits them and reports the bridge's
own reading; `throughput` filters them via `ownReplicasOnly`; `external` needs
nothing, because its `P` is a constant target from config and it never reads
replica rows.

**Invariant: a variant is warmed by at most one pool.** `FromWarmPool` is a bool
rather than a pool name, and `decision.WarmPoolSupply` is keyed by variant alone,
both of which are correct only under that assumption — one pool means one
`--gpu-memory-utilization`, so a variant's bridges are comparable and a single
median over them means something.

Different variants of one model *may* sit in different pools, prefill and decode
included: a variant is named by its ScaledObject, role and all, and every figure
above is per variant, so they aggregate independently. What the invariant rules
out is one variant bridged by two pools at once — their bridges would carry
genuinely different capacities and the median would describe neither.

**The pool layer enforces it structurally**, so the accounting above is not
relying on operator discipline. A variant names at most one pool through the
`warmPool` trigger metadata key, and `warmpool.VariantsFor` hands a variant only
to the pool it names — or to the single pool, when the namespace has exactly one
and there is nothing to disambiguate. With several pools declared, a variant that
names none is *Unassignable*: it gets no warm copy and is reported, rather than
being warmed by whichever pool saw it first. Several pools per namespace is an
anticipated configuration, not a hazard — a warm copy is only reusable on the
accelerator it was loaded on, so a cluster with two GPU types needs one pool per
type.

### Responsibility table

| Field | Written by | Read by |
|---|---|---|
| Per-variant `ReplicaCount`, `PendingReplicas`, `PerReplicaCapacity` (own replicas only), `Role`, `TotalDemand` | Analyzer | Builder, then optimizer (picker + scaling math) |
| Per-variant `WarmPoolReplicas`, `WarmPoolPerReplicaCapacity` | Analyzer (measurements) | Builder |
| Per-variant `WarmPoolCapacity` | **Builder** — `WarmPoolReplicas × WarmPoolPerReplicaCapacity`; in no supply total. Analyzer-written values are overwritten | Retained-pool switching decision |
| Per-variant `Role`, and the `ReplicaCount` clamp to the scale target | **Builder** (from discovery) | Optimizer |
| Model-level `TotalSupply`, `TotalAnticipatedSupply` | **Builder** — not fields on `AnalyzerResult` at all | Engine post-step, optimizer |
| Per-role `RoleCapacities[role].Total*` | **Builder** (`buildRoleCapacities`), pairing analyzer `RoleDemand` with supply it groups by role | Engine post-step, optimizer |
| `RequiredCapacity`, `SpareCapacity` (model + role scope) | **Engine post-step only** — analyzer-written values are overwritten | Optimizer |
| `NamedAnalyzerResult.Remaining`, `Spare`, `RoleSpare` | Optimizer helpers (`applyAllocation`, `applyDeallocationForRole`) | Optimizer allocation loop |
| `NamedAnalyzerResult.Live` | Engine (`updateLivenessAndSetLive` in `runAnalyzers`, each cycle) | Eligibility for the composite (`allocation.Eligible`) and the grace veto; the composite's own `Live` gates scale-down (`needsScaleDownForRole`, `safeRemovalReplicasForRole`) |

---

## Components

- **Registration** — one registry, `analyzerRegistry` in
  `internal/engines/steadystate/analyzer_registry.go`, the engine's
  `analyzers` field. `RegisterAnalyzer(name, analyzer) error` adds a
  built-in; `cmd/main.go` registers the throughput analyzer this way before
  `StartOptimizeLoop`. The saturation analyzer is pre-registered at slot 0,
  and the slo analyzer after it. `StartOptimizeLoop` freezes the built-ins:
  a later registration returns an error. The config-driven analyzers from
  `analyzerDefinitions` live in the same registry
  (`UpsertExternalAnalyzer`/`RemoveExternalAnalyzer`, reconciled every
  cycle); one named like a built-in (`slo`, say) is stored but not run.
  Every read takes the registry's read lock.
- **Saturation's capabilities** — what the engine reaches of the saturation
  analyzer beyond `Analyze` is resolved once, when it is registered, into
  `saturationCapabilities`: the accelerator stabiliser, the history evictor,
  the forgetter, the presence noter, the learned-state store and the learner
  recording's hook. Each is a typed interface pinned at compile time on
  `*saturation.SaturationAnalyzer`; a nil field is a capability the
  registered analyzer lacks (a test double), and each caller skips it.
- **Input builder** — `buildAnalyzerInput` in `engine_v2.go`, called by
  `runAnalyzers` before any analyzer runs. Per variant it forgets on request
  (`forgetOnRequest`), files the scale target's engine parameters in the
  capacity store (`LoadFromScaleTarget`) and publishes `wva_engine_config`
  (`publishEngineConfig`), in that order. It returns the one
  `domain.AnalyzerInput` every analyzer of the model receives: rows,
  variant states, discovery's `VariantMetadata`, the merged policy, the
  scheduler queue and the arrival rate. No analyzer reads `VariantMetadata`
  yet; the role join and the clamp to the scale target still run after
  `Analyze`, in `buildCapacities`. `AnalyzerInput` has no capacity view yet.
- **Engine post-step** — `internal/engines/steadystate/engine_v2.go`:
  `applyUniversalThreshold(*allocation.NamedAnalyzerResult, scaleUp, scaleDown)`,
  called from `buildCapacities`, applies the formula
  `RC = max(0, TotalDemand/scaleUp − TotalAnticipatedSupply)` /
  `SC = max(0, TotalSupply − TotalDemand/scaleDown)` at model scope and
  each role in `RoleCapacities`, writing them on the `NamedAnalyzerResult`.
- **Capacity-build step** — `internal/engines/steadystate/engine_v2.go`:
  `buildCapacities(ctx, *NamedAnalyzerResult, metaByVariant, scaleUp, scaleDown)`
  runs between the analyzers and the optimizer. It joins discovery identity,
  clamps `ReplicaCount` to the scale target, derives each variant's
  `WarmPoolCapacity`, assembles model and per-role supply, and applies the
  universal threshold. Every derived capacity in the pipeline is written here.
- **Aggregation helpers** — `internal/engines/aggregation/`:
  `SumTotalSupply`, `SumTotalAnticipatedSupply`, `SumTotalDemand`,
  `AggregateByRole` over `[]VariantCapacity`. Used by the **builder** to assemble
  per-scope totals without reimplementing the math; analyzers do not call them to
  publish supply, because they have nowhere to publish it to.
- **Composite** — `internal/engines/steadystate/composite.go`
  (`buildComposite`) and `internal/engines/allocation/composite_*.go`
  (`ResolveSO`, `Eligible`, `CompositeHasSignal`): reduces every analyzer
  that ran to the one entry the optimizer reads. See
  [How results combine](#how-results-combine).
- **Optimizer flow** — `internal/engines/allocation/`:
  `ModelScalingRequest.CompositeSignal` carries the composite plus working
  scratch state for the allocation loop. `CostAwareOptimizer` and
  `GreedyByScoreOptimizer` consume it via shared free functions
  (single-variant, paired P/D, and role-iterated helpers).

---

## User configuration

Analyzers are configured via `config.ScalingPolicy.Analyzers` (YAML key
`analyzers`). Each entry is an `AnalyzerScoreConfig` struct
(`internal/config/saturation_scaling.go`):

| Field | Type | Default | Purpose |
|---|---|---|---|
| `name` | string | required | Must match the name returned by `Analyzer.Name()` |
| `enabled` | bool | true (when the entry is present) | Set false to disable without removing the analyzer |
| `score` | float64 | 1.0 | Weight in the fair-share priority formula |
| `scaleUpThreshold` | float64 | global | Overrides the model-level `scaleUpThreshold` for this analyzer's own result (`buildNamedResult`). Only saturation's decides: the composite reads other analyzers' needs, not their bands |
| `scaleDownBoundary` | float64 | global | Overrides the model-level `scaleDownBoundary` for this analyzer's own result. Only saturation's decides, as above |

A band on an entry other than saturation is accepted, and
`parseScalingPolicyConfigRejects` logs `config.InertAnalyzerThresholdsMessage`
for it (`ScalingPolicy.InertAnalyzerThresholds`). Honoring it in the composite
would take more than one number: scaling the analyzer's need by
`satUp / itsUp` moves its scale-up point, but its scale-down point then sits at
`satDown x itsUp / satUp`, not at its own boundary.

Minimal YAML example:

```yaml
analyzers:
  - name: saturation
    score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
  - name: throughput
    enabled: false   # disable without removing
    score: 2.0
```

When `enabled` is false the analyzer is not run at all.

**Participation is opt-in.** An analyzer registered in code
(`Engine.RegisterAnalyzer`), or declared in `analyzerDefinitions`, runs in a
cycle only when it has an entry in `analyzers` with `enabled` `true` or
unset. With no entry it does not run, exactly as if `enabled: false` had been
set. Saturation is the exception: it always runs, because the composite takes
its variant list and units from it. It sizes the model only when listed
(`ScalingPolicy.SaturationListed`).

---

## Analyzer implementor guide

Implement `domain.Analyzer` (`internal/domain/analyzer.go`):

```go
type Analyzer interface {
    Name() string
    Analyze(ctx context.Context, input AnalyzerInput) (*AnalyzerResult, error)
}
```

### Input

Key `AnalyzerInput` fields:

| Field | Type | Description |
|---|---|---|
| `ModelID` | string | Model being analyzed |
| `Namespace` | string | Kubernetes namespace |
| `ReplicaMetrics` | `[]ReplicaMetrics` | Per-replica metric snapshots |
| `VariantStates` | `[]VariantReplicaState` | Current/desired/pending replica counts per variant |
| `Config` | `AnalyzerConfig` | Resolved config (cast to your config type as needed) |
| `SchedulerQueue` | `*SchedulerQueueMetrics` | Scheduler queue metrics; nil when flow control is off |
| `ArrivalRate` | float64 | Model-level request arrival rate (req/s), no per-pod labels; zero when EPP absent or no traffic yet |

### Output invariants

Emit the measured (D, P) signal and stop there. Concretely, fill in:

```go
result.VariantCapacities = []domain.VariantCapacity{ /* ReplicaCount, PendingReplicas,
    PerReplicaCapacity, Role, TotalDemand, and the WarmPool* measurements */ }
result.TotalDemand = /* model-level demand: yours to attribute */
result.RoleDemand  = /* per role, or nil when not disaggregated */
```

**There is nothing else to populate.** `AnalyzerResult` has no `TotalSupply`,
`TotalAnticipatedSupply`, `RoleCapacities`, `RequiredCapacity` or
`SpareCapacity` fields — the builder computes all of them on
`NamedAnalyzerResult`. This is what makes the **linearity invariant**
(`supply = Σ_v ReplicaCount × PerReplicaCapacity`, at model and role scope) hold
by construction: an analyzer has no way to state a supply that disagrees with its
own (D, P).

Two rules that follow from that invariant, and are easy to get wrong:

- **`P` must be measured over the same population `ReplicaCount` counts** — the
  variant's own replicas. The builder clamps that count to the scale target, so
  any row that is not part of the scale target (a warm pool bridge) must be kept
  out of the per-replica maths. See "Warm pool bridges" above.
- **`ReplicaCount` is what actually reported this cycle**, in scale-target units
  (pods, or LWS groups). The builder only ever lowers it.

Aggregation helpers (`aggregation.SumTotalSupply`, `SumTotalAnticipatedSupply`,
`AggregateByRole`) still exist, but they are the **builder's** tools, not the
analyzer's; an analyzer normally has no reason to call them.

---

## Pipeline flow

1. `cmd/main.go` calls `engine.RegisterAnalyzer(name, a)` for each built-in
   analyzer it adds before `StartOptimizeLoop`. The saturation analyzer is
   pre-registered at slot 0.
2. `StartOptimizeLoop` freezes the registry's built-ins.
3. Each cycle, `reconcileExternalAnalyzers` rebuilds the external analyzers
   from `analyzerDefinitions`, in the same registry.
4. Per cycle, for each model: `runAnalyzers` builds the model's
   `AnalyzerInput` (`buildAnalyzerInput`), runs the saturation analyzer on it
   unconditionally, then every other analyzer (`analyzerRunEntries`: the
   built-ins in registration order, then the external ones by name) that the
   policy enables, each on the same input.
5. For each analyzer that runs, `buildNamedResult` builds its capacities and
   applies `applyUniversalThreshold` at its own resolved band (per-analyzer
   override beats global). This shapes that analyzer's own result and its
   published series only.
6. `buildComposite` combines the results into one `NamedAnalyzerResult`,
   judged at saturation's band. That one entry is
   `ModelScalingRequest.CompositeSignal`.
7. Cost, accelerator and role reach the optimizer from discovery
   (`ModelScalingRequest.Variants`, joined in `allocation.buildVariantRecords`),
   not from any analyzer.

---

## How results combine

**The composite** (`buildComposite`, `steadystate/composite.go`): the
optimizer receives one result, built in `collectV2ModelRequest` from every
analyzer that ran. It starts as a copy of saturation's `AnalyzerResult` (every
field, maps and slices cloned) and changes the need, the demand that carries
it, the PRC of each variant another analyzer decided, and the holds:

- **Need.** `allocation.ResolveSO` takes, for each eligible analyzer
  (`allocation.Eligible`: live, informative, non-nil) that has the variant
  without `ReasonNoData`/`ReasonError`, `N = D[role] / PRC` from the
  analyzer's own demand and capacity (`allocation.TotalReplicas`), and keeps
  the largest finite, non-negative one. Saturation is eligible only when the
  policy lists it (`ScalingPolicy.SaturationListed`: listed and enabled, or no
  `analyzers` section at all), and `slo` never is: it only observes. `Reason`
  records the path: `C0-agree` (several), `C1-single`, `C4-no-signal` (none).
- **Pricing** (`priceRole`, per role). The role is priced in one analyzer's
  frame (`roleFrames`: saturation's, and one per other analyzer from
  `Decision.Needs`), whichever needs more replicas for the fleet the role runs:
  the lowest coverage, the sum over its variants of replicas / N (1 per variant
  when it runs none), saturation's on a tie. A variant an analyzer does not
  size is read at saturation's ratio to the variants it does. Mixing frames per
  variant asked for more than any analyzer, and taking the largest need among
  the others per variant let one analyzer's zero need veto another's. A
  VERBOSE `composite-pricing` line names the frame and every frame's coverage
  and demand.
  - *Saturation's frame:* its demand and PRCs stand. `HeldRoleDemand` is
    raised to the others' need in saturation's units,
    `D_sat x coverage_sat / coverage_others`, and dropped when that reaches
    `D_sat`: the hold no longer sets the demand, and kept, the share optimizer
    would give nothing from the role.
  - *The others' frame:* the role's demand is the largest `N x PRC_sat` over
    the variants they size, defined when `D_sat[role]` is zero, so another
    analyzer can grow a role saturation sees as idle. Each variant they size
    gets `PRC = D / N`, so it needs exactly `N`, and variants on different
    accelerators keep their view of the relative worth. A variant they do not
    size keeps saturation's PRC. The model's `TotalDemand` moves with the role
    (or is the role's demand, aggregated), the role's variants share it in
    saturation's proportions (evenly when it attributed none), their
    utilization follows, and `HeldRoleDemand` is dropped.
  - *Scale-down* is bounded by every frame (`boundAllSpare`, `otherFrames`): shedding X of the
    chosen frame's capacity sheds at most `X x r` of frame f's, `r` the largest
    `PRC_f / PRC_chosen` over the running variants, so the role's spare (the
    model's on an aggregated model) is capped at `spare_f / r` for each f. The
    optimizer releases up to the larger of the spare and `RoleReleasable`, the
    capacity above the release target, so each frame's own releasable at the
    release target, over `r`, is published as `ReleaseBound` whenever another
    frame sizes the role, and `initRoleState` caps `RoleReleasable` at it.
    The share optimizer and rescale size from demand and never read the
    spare, so the other frames are published too (`OtherFrames`: each
    frame's supply, demand and `r`) and read as demand at the utilization
    the optimizer sizes to (`OtherFrameDemand`: the share's scale-up
    threshold, rescale's full utilization): `u x (S - Releasable(u))`, never
    above `u x S`, so another frame stops a release but never grows the
    role. As a GPU floor at the release target it held a role deeper than
    saturation alone would, even when the other analyzer agreed, and under
    contention took GPUs a shortfall shared by weight would have given
    another model.
    *Scale-up* follows the chosen frame alone: a variant it grows may leave
    another frame short for one cycle, until the new replicas are read.
  - With one variant per role every frame gives saturation's PRC. A need
    whose demand at saturation's PRC overflows is no reading. A need on a
    variant saturation has no PRC for counts only when its own analyzer's
    frame is chosen; under any other frame it is logged as dropped. An
    analyzer whose needs are all on such variants gets no frame
    (`roleFrames` keys demand on `PRC_sat > 0`), so they are always
    dropped.
- **Holds** (`compositeHolds`, `keepSaturationHolds`, `allocation.RoleHold`,
  per role). Three rules:
  - *Unsized.* With saturation unlisted, a role none of whose variants has a
    need is held both ways, and on P/D every other role is held against
    growth too, as the share optimizer already froze such a model whole.
  - *Grace veto.* A listed analyzer other than saturation that is live but
    not eligible -- inside its staleness grace with no reading, or returned
    nothing this cycle (`listedInGrace` adds an entry with no `Result`) --
    blocks scale-down of the roles it reports variants for, or of every role
    when it reports none. An entry from `listedInGrace` has no `Result`, so
    it always holds every role: a model-level analyzer whose query fails
    holds a P/D model it could never size
    ([#343](https://github.com/ev-shindin/llm-scaling-manager/issues/343)).
  - *Saturation's hold kept.* A role saturation holds (`HeldRoleDemand`)
    whose held figure the pricing dropped -- another frame prices it, or
    another analyzer's demand replaced it -- is held against scale-down:
    read as demand, the held figure let the share optimizer release part of
    a role saturation would keep.

  `applyHolds` zeroes the forbidden `RequiredCapacity`/`SpareCapacity`
  (per role, and model-level when every role is held that way); the share
  optimizer reads `Holds` itself, freezing the model on `NoScaleUp` and
  raising the role's floor to what it has on `NoScaleDown` -- and on a role
  saturation holds (`HeldRoleDemand`), which gives nothing either -- with its
  headroom cap counted above its need (`ShareRole.Held`). Greedy's rescale
  pass (`rescaleModelDecisions`) also reads them: a role held both ways freezes
  the model there, as in the share optimizer, and `rescaleInputsForGroup` gives
  such a model exactly its current GPUs in the water-fill, so the rest of the
  budget goes to the others; no role is reclaimed or filled in a direction it
  is held against. How rescale sizes a held model is in
  [Rescale under holds](#rescale-under-holds).
- **Warm-pool pressure** (`publishVariantPressure`) is published from the
  composite, after it is built, with saturation's scale-up threshold; a role
  held against growth never reports `NeedsScaleUp`. Warm-pool supply
  (`publishWarmPoolSupply`) is published from the composite too, so the two
  read one unit. A multi-variant role priced in another analyzer's frame has
  each variant it sizes priced at `D / N` there (`priceRole`), and the
  variant's bridge PRC (`WarmPoolPerReplicaCapacity`) moves by the same ratio;
  published from saturation's entry, its supply was in saturation's unit while
  its pressure was in the other frame's. A variant saturation has no PRC for
  keeps saturation's bridge reading: there is no ratio to convert it by.
  The ratio is the variant's own, so for a bridge of another shape (a pool
  Pod configured differently from the variant's replicas, read on its own as
  `bridgePerReplica`) that the other analyzer would rate differently, the
  converted figure is an approximation, not a measurement. Supply is not
  published on a cycle that builds no composite, which is only one whose
  saturation run fails; the last reading stands until the reader's `maxAge`.

The composite is judged against saturation's thresholds
(`AnalyzerThresholds(saturation)`), and recorded in the same
`recordAnalyzerMetrics` call as the analyzers, once a cycle, as analyzer
`CompositeSignal`. Its `Score` is the largest score among the analyzers that
contributed a need, saturation's when none did; the cross-model fair share
reads it. It is live when any analyzer contributed a need, and as live as
saturation otherwise. `allocation.CompositeHasSignal` is false when every variant
is `C4-no-signal`; the engine then sets the `no-composite-signal` blocked
reason (`reportSignalReason`), and clears it on every cycle that builds no
composite.

**Scale-down gate** (`needsScaleDownForRole`): a role scales down only when
the composite is live and its `RoleSpare` for the role is above zero;
`safeRemovalReplicasForRole` reads the same entry. Because the composite's
need is the largest analyzer need, scale-down requires every contributing
analyzer to agree. A version in which every live analyzer voted separately is
kept in `internal/engines/allocation/multi_backup/` behind `//go:build ignore`.

**Liveness.** An analyzer is live for the current cycle when it produced a
non-error, capacity-bearing result within the staleness window:
`analyzerLivenessStaleCycles` (3) times the optimization interval, with a 30s
interval when `Config` is absent or reports a non-positive one
(`livenessThreshold`). An informative result with a zero `AnalyzedAt` is
recorded as "now", not as stale. Liveness is tracked per model and analyzer
(`updateLivenessAndSetLive`), with no exemption for saturation.

What liveness decides:

- **Eligibility.** Only a live, informative analyzer with a result
  contributes a need (`allocation.Eligible`). A stale or erroring one stops
  counting once its last good reading leaves the window.
- **Grace veto.** A listed analyzer that is still live but has no reading
  this cycle (`listedInGrace` adds an entry with no `Result`) holds the roles
  it covers against scale-down (`compositeHolds`). A no-data blip on an
  analyzer with a recent good result therefore errs toward not scaling down.
- **The composite's own liveness.** The composite is live when any analyzer
  contributed a need, and as live as saturation otherwise. A non-live
  composite never scales down. This also makes leader failover safe: a new
  leader has no liveness history, so nothing scales down until an analyzer
  produces a fresh result.

An analyzer reporting `no-data` does not become non-live at once; it does so
when its last informative result ages out. One that never had good data (a
mislabelled metric at startup) is non-live from the start. A broken query is
not an *error*: it returns a well-formed result with no usable capacity, so
this reason-based check is what detects a durably broken analyzer. Liveness
reads the capacity (supply) signal, not demand.

**Demand-liveness telemetry (warn-only).** The engine separately watches for
the throughput analyzer having a live capacity signal while reporting no
demand (`TotalDemand == 0`) for at least the staleness window
(`detectDemandLiveness`). This usually means the request-arrival query is
misconfigured or EPP is not reporting arrivals. It logs a warning and gates
nothing.

**Scale-up gate** (`anyRoleNeedsScaleUp`): a role grows when the composite's
remaining required capacity for it is above zero. Liveness does not gate
scale-up; a role the composite holds against growth has its required
capacity zeroed (`applyHolds`).

Cost, accelerator and role are not read from any analyzer: the optimizer
takes them from discovery (`ModelScalingRequest.Variants`).

### Rescale under holds

A role held against scale-down keeps its GPUs both in
the water-fill and in the model's split across its roles. A model's
water-fill cap is each role's demand in GPUs capped at that role's
`maxReplicas` GPUs, summed: counted uncapped, prefill at its maximum of 16
GPUs sized the cap on 560, and the GPUs that share left free stayed free
while another model was short. The water-fill weight is priority times the
same raised demand the cap reads (`modelWeightDemand`: each role's demand,
or what another frame keeps at full utilization when that is more), not the
model's own `TotalDemand`.

A group is contended, and rescale runs, only
when what its models can take exceeds the budget: each cap before the
budget bounds it, so a role at its `maxReplicas` does not set off rescale
in a group that fits. A model with a variant that has no accelerator name
is left to the additive path whole, since the budget, the floors and the
demand count none of that variant's GPUs: the additive path may shrink it
and finds no budget to grow it.

A growing role's share is lowered to what `fillRole` can
place as whole replicas, on a trial copy (cost order, `maxReplicas`, each
variant's own pod size), and what that leaves goes to the roles cut most
(`roundFillsToReplicas`): split in GPUs, 8 more GPUs went +1 and +7 to a
model's two roles on 8-GPU pods, neither a pod, and stayed free. No role's
share exceeds what its `maxReplicas` allow, and what a role at its maximum
cannot hold re-splits over the others, each up to the larger of its demand
and its uncapped share (`roleShareCeilings`): uncapped, prefill at its
maximum took its share and decode was reclaimed to pay for it.

Reclaims are
whole replicas of the model's summed cut (`reclaimWholeReplicas`): each
role's own cut first, then what is left from the role cut most, never more
than the summed cut. A role counts toward the sum only what it can give:
not a cut no replica can meet (a variant with no capacity reading, the
cheapest held at one), and, while it has a share, not its last replica, so
the second pass never takes a role with demand to zero. Cut 4 GPUs from
each of two roles on 8-GPU pods, each role alone shed nothing.

Both roundings, fill and reclaim, are greedy, which is exact while the
model has one replica size. A model with more than one size, in one role or
across roles, is rounded by an exact pod search instead (`roundExact` in
`rescale_pods.go`): a bounded knapsack, on bitsets, over each role's
variants. It weighs two plans and takes the one that leaves the model more
GPUs, rounding each side alone on a tie:

- **Round each side alone.** Fill the most GPUs within the summed growth of
  the roles not cut, each role bounded as the greedy pass bounds it. Shed the
  most GPUs within the summed cut, each role no further than its cut rounded
  up to its smallest removable pod, keeping `minReplicas`, scale-down holds,
  a role's last replica while it has a share, and the cheapest-at-one rule.
- **Shed nothing.** Fill the most GPUs within what the shares add above what
  the model holds; open only when they add zero or more.

Each plan's GPUs are nondecreasing in the target, so the larger is too: a
larger target never leaves fewer GPUs. Greedy, prefill on an empty 8-GPU
variant and decode on 2- and 4-GPU pods placed 10 GPUs at a target of 10 and
8 at 11. A target a pod or more below what the model holds closes the second
plan, so the first sheds a pod. The second plan is what keeps a model at its
target still: rounded alone, a growing role short of a pod made a cut role
shed one, and the next cycle took it back.

The search takes the most GPUs first and cost order only among equal sums, a
change from the greedy pass: on a cheap 6-GPU variant and a dearer 4-GPU
one, +8 is two 4s. Each bound is clamped to `podSearchMaxUnits` (4096 units
of the pods' greatest common divisor), so a model grows or sheds no more
than that a cycle; one joint search over two roles at the bound takes
0.35-0.47 ms (`BenchmarkPodSearchAtTheBound`).
The real fill of a mixed role (`fillRoleExact`) uses the same search.

---

## Data model: AnalyzerResult → NamedAnalyzerResult

Understanding what transforms where prevents the most common mistake: treating
`Result.*` counters as live state during allocation.

**`domain.AnalyzerResult`** is the immutable record an analyzer returns.
The engine owns its calibration:

1. The analyzer populates `VariantCapacities[]`, `TotalDemand` and, for P/D
   models, `RoleDemand`. `AnalyzerResult` has no supply or capacity-signal
   fields: the analyzer cannot publish supply, `RequiredCapacity` or
   `SpareCapacity`.
2. The engine wraps the result in a `NamedAnalyzerResult`, and
   `buildCapacities` derives `TotalSupply`, `TotalAnticipatedSupply` and
   `RoleCapacities` on it from `VariantCapacities`, then calls
   `applyUniversalThreshold`, which writes `RequiredCapacity` / `SpareCapacity`
   on the `NamedAnalyzerResult` at model scope and on each
   `RoleCapacities[role]`.
3. `Result` is never mutated. `Result.*` values are stable read-only data for
   the rest of the cycle.

**`allocation.NamedAnalyzerResult`** is the working unit the optimizer operates on.
Its fields fall into three categories:

| Field | Category | Description |
|---|---|---|
| `Name`, `Score`, `Result` | Immutable | Set by engine; never written by optimizer |
| `Remaining`, `Spare` | Mutable scalars | Model-scope working counters; decremented by `applyAllocation` during scale-up |
| `RoleSpare` | Mutable per-role map | Populated by `initRoleState`; decremented by `applyDeallocationForRole` during scale-down |

`Remaining` and `Spare` are seeded from the entry's `RequiredCapacity` and
`SpareCapacity`. `RoleSpare` is seeded from
`RoleCapacities[role].SpareCapacity`. None of this flows back into `Result`.

The optimizer receives one such entry per model, the composite
(`ModelScalingRequest.CompositeSignal`); the per-analyzer entries stop at the
composite.

**`RolePairedState`** (`map[string]float64`, keyed by role) is picker-local
demand created per call to `initRoleState`. It holds per-role required
capacity for the scale-up loop and is decremented by the joint-commit step
inside `allocateForModelPaired`. It is not stored on `NamedAnalyzerResult`
and is discarded after each model's allocation pass.

---

## Optimizer internals and helper composition

Both optimizers share the same allocation and scale-down primitives from
`internal/engines/allocation/analyzer_helpers.go` and
`internal/engines/allocation/cost_aware_optimizer.go`. The optimizers own the
*when* and *which model*; the helpers own the *how*. Every helper reads the
one composite entry.

### Scale-up path

All scale-up goes through `allocateForModelPaired`:

```text
initRoleState(s)               → roles, RolePairedState (per-role demand + RoleSpare)
anyRoleNeedsScaleUp(ps, roles) → loop gate: any role still has demand?
  pick(role, ...)              → (variant, capN): optimizer-specific variant selector
  roleBottleneckReplicas       → ceil(state[role] / PRC[v]): replicas for the role's remaining demand
  roleAggRemaining             → the role's remaining demand
  Δ_util = min_role util_role  → joint commit bound: trim to the least-served role
  applyAllocation(s, v, k)     → decrement Remaining
```

`pick` is a `RolePickFn` — the only part that differs between optimizers:

- `costGreedyRolePick`: picks the cheapest cost-efficient variant; no GPU budget
  cap (no limiter declared).
- `fairShareRolePick`: picks the cheapest variant within available GPU budget;
  caps `capN` to the fair-share target (a limiter declared).

For non-disaggregated models, `initRoleState` synthesizes a single `"both"` role
from the model-level scalars, so `allocateForModelPaired` handles both the
disaggregated and non-disaggregated cases through the same loop.

### Scale-down path

Both optimizers call `scaleDownRoleIterated`, which handles both disaggregated
and non-disaggregated models through the same role loop (`"both"` is the
synthetic role for non-disaggregated):

```text
for each role (sorted for determinism):
  needsScaleDownForRole(s, role)           → gate: composite live and RoleSpare[role] > 0
  sortVariantsForScaleDown(s, vcs)         → cost-desc; tie-break: Score x PRC asc
  scaleDownVariantSet(...)
    safeRemovalReplicasForRole(s, v, role) → floor(max(RoleSpare, RoleReleasable)[role] / PRC[v])
    applyDeallocationForRole(s, v, role, n)→ decrement RoleSpare and RoleReleasable
```

### Fair-share iteration (GreedyByScoreOptimizer only)

`fairShareScaleUp` uses iterative mean equalization rather than fixed fractions:

1. Compute `mean` = average `remaining` (fair-share priority value) across active
   models.
2. Sort by `remaining` descending; take the highest.
3. Call `allocateForModel` with budget `target = remaining − mean`: allocates
   replicas via `allocateForModelPaired` until the model's priority value drops
   to or below `mean`.
4. Recompute `remaining = fairShareValue(priority, s, ps, roles)` from the
   post-allocation working state.
5. Repeat until no active models remain or no GPUs are left.

`fairShareValue = priority × Score × Σ_role pickerState[role]`, capped, where
`Score` is the composite's: the largest score among the analyzers that sized
the model this cycle. The only other reader is `sortVariantsForScaleDown`'s
tie-break (`Score x PRC`), which with one entry reorders nothing unless
`Score` is 0 or less. Scores are not on a common scale across analyzers
([#226](https://github.com/ev-shindin/llm-scaling-manager/issues/226)).

---

## Optimizer consumption

The composite is passed to one of two optimizers depending on whether the
cluster `default` policy declares any `limiters:` entry:

- **`CostAwareOptimizer`** (no limiters declared): reads cost and role from
  discovery and capacity from the composite; scales up the cheapest variant
  that covers the required capacity, scales down the most expensive variant
  with spare capacity.
- **`GreedyByScoreOptimizer`** (a limiter declared): respects
  `ResourceConstraints` (GPU budgets per accelerator type). Models are ordered
  by the fair-share value above.

Both optimizers are stateless and selected per-cycle from the engine's
`optimizer` field. The utilization-share optimizer and rescale read the same
composite, including its `Holds` and `OtherFrames`.

## Observability

The engine emits two structured INFO log lines per reconcile cycle per model —
one per analyzer (after the threshold post-step) and one after the optimizer
returns. See [cycle-log.md](../reference/cycle-log.md) for field schemas, grep patterns,
and an explanation of the `reason` values set by each analyzer.
