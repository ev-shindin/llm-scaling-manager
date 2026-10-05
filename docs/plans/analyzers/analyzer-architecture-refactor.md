# Design & Plan: Analyzer Architecture Refactor — Metadata Discovery + a Pure `(D, P)` Contract

**Status:** Implemented — all six phases landed (see §5 for what deviated from this plan)
**Area:** engines/analyzers
**Related:** `docs/proposals/analyzer-metric-interface.md` (the `(demand, target)` contract & external analyzers), issue #1455 (external analyzers), #1444 (proposal, docs-only)

---

## 1. Motivation

> The three problems below are the ones this plan set out to fix. All three are now fixed;
> the present tense is kept as the historical record of why the work was done. §4 and §5
> describe the code as built.

WVA's analyzers carry three coupled problems:

1. **The saturation analyzer is privileged as the metadata keeper.** `saturationEntry`
   (`internal/engines/allocation/analyzer_helpers.go:87`) is the sole source of per-variant identity
   (`Cost`, `AcceleratorName`, `Role`, replica counts) the optimizer reads. Every other analyzer must
   either piggyback on it or be special-cased. The de-privilege TODO is already written in the code
   (`analyzer_helpers.go:90`: *"remove the sat_v2 special role once all analyzers populate variant
   metadata"*), and `throughput` demonstrates the wart — it leaves `Cost`/`AcceleratorName` empty and
   relies on saturation (`internal/engines/analyzers/throughput/analyzer.go:372-382`).

2. **The analyzer contract is bespoke and wide.** `Analyze` returns `*AnalyzerResult` with ~8
   engine-owned or observability fields (`RequiredCapacity`, `SpareCapacity`,
   `TotalAnticipatedSupply`, `TotalSupply`, `Utilization`, `TotalCapacity`, …) that analyzers must
   leave zero or populate by convention. The two numbers that actually matter — demand `D`
   (`AnalyzerResult.TotalDemand`) and per-replica target `P` (`VariantCapacity.PerReplicaCapacity`) —
   are split across two structs and muddied by everything else.

3. **Extending WVA requires writing Go.** A new signal (an SLO probe, a queue-depth source, a
   business metric) cannot be added without compiling a new analyzer. #1455 wants config-driven
   PromQL analyzers, but they can only be *clean* if there is a clean contract to implement.

### Key finding that shapes this design

The saturation analyzer **does not compute** the identity metadata — it **copies** it. `AcceleratorName`,
`Cost`, `Role`, and replica counts arrive **pre-computed on `domain.AnalyzerInput`** (via
`VariantStates` and `ReplicaMetrics`) and are copied onto each `VariantCapacity`
(`saturation/analyzer.go:352-360, 441-453`). The authoritative sources already exist:

| Field | Source | Site |
|---|---|---|
| `VariantName`, `ModelID`, `Cost`, `MinReplicas`, `MaxReplicas` | managed `ScaledObject`/`HPA` annotations (`llm-d.ai/managed`, `llm-d.ai/model-id`, `llm-d.ai/variant-cost`) | `annotationSourcedVariants` `utils/variant.go:198`; `annotations.VariantAutoscalingFromScaledObject`/`FromHPA` `variant_fromannotations.go:40,89` |
| `Role` | leader pod-template label `llm-d.ai/role` | `getRoleFromScaleTarget` `engine.go:1153` |
| `AcceleratorName` | pod-template `nodeSelector`/`nodeAffinity` GPU product keys | `accelerator.GetAcceleratorNameFromScaleTarget` `accelerator.go:90` |
| `CurrentReplicas`, `ReadyReplicas`, `PendingReplicas`, `GPUsPerReplica` | scale-target status/spec | `BuildVariantStates` `engine.go:1063-1151` via `ScaleTargetAccessor` |

**So this refactor is boundary-drawing, not greenfield.** We consolidate the sources above into one
authoritative record, hand it to analyzers *and* the optimizer, and stop laundering identity through
the analyzer's output. This substantially de-risks it.

---

## 2. Goals / Non-Goals

**Goals**

- Introduce a **variant-metadata discovery step** that produces one authoritative record per managed
  variant, sourced from the existing machinery, consumed by both analyzers and the optimizer.
- Reduce the analyzer contract to a **pure `(D, P)` producer**: demand `D` (model- and role-scoped)
  and per-replica target `P` (per variant/role). No identity, no engine-owned fields.
- **De-privilege saturation** — delete `saturationEntry`; the optimizer reads identity from discovery.
- Emit `wva_analyzer_demand` / `wva_analyzer_target` for every analyzer (the proposal's Phase 1
  observability), driven from the `(D, P)` outputs.
- Land the **external-analyzer wrapper (#1455)** as a Go analyzer that implements the pure contract
  from a PromQL definition, with **no metadata to supply**.

**Non-Goals**

- **V1 (`internal/saturationv1/`) and the queueing-model analyzer are out of scope** — both are being
  removed on a separate track. This design targets the **post-removal** engine
  (`saturation` + `throughput` + the external wrapper). Their removal is listed as a precondition
  (§6) only to flag the one shared dependency (`DefaultVariantCost`) that must be relocated.
- No change to the **optimizer's coordination math** (sum-over-variants / min-over-roles in
  utilization space). We change *where identity comes from* and *the shape analyzers emit*, not how
  replicas are chosen. `applyUniversalThreshold` + `roleBottleneckReplicas` stay the `⌈D/P⌉` engine.
- No hard KEDA dependency. WVA stays KEDA-*shaped*.

---

## 3. Target architecture

```text
┌─────────────────────┐     VariantMetadata[]      ┌──────────────────────────────┐
│  Discovery step      │ ─────────────────────────▶ │  Optimizer                    │
│  (annotationSourced  │   (identity: Cost, Accel,  │  reads identity from discovery │
│   + BuildVariant     │    Role, replicas, GPUs)   │  (no saturationEntry)          │
│   States + role/acc) │ ──┐                        └──────────────▲───────────────┘
└─────────────────────┘   │                                        │ (D, P) per variant/role
                          │ VariantMetadata[]                      │
                          ▼                                        │
                 ┌──────────────────┐   AnalyzerResult{D, P}  ┌────┴─────────────┐
                 │ Analyzers        │ ───────────────────────▶ │ wva_analyzer_*   │
                 │ (pure D/P):      │                          │ metrics          │
                 │  saturation   │                          └──────────────────┘
                 │  throughput      │
                 │  external wrapper│
                 └──────────────────┘
```

### 3.1 The discovery output — `VariantMetadata`

One record per managed variant, produced once per cycle. It is essentially today's
`domain.VariantReplicaState` **plus** `Cost` and `AcceleratorName` (currently smuggled through
`domain.ReplicaMetrics`). Sketch (final field set settled in Phase 1):

```go
// VariantMetadata is the authoritative per-variant identity + state for one cycle.
// Produced by the discovery step; consumed by analyzers (as input) and the optimizer.
type VariantMetadata struct {
    VariantName     string
    ModelID         string
    Namespace       string
    Role            string  // "prefill" | "decode" | "both" (llm-d.ai/role)
    Cost            float64 // llm-d.ai/variant-cost
    AcceleratorName string  // nodeSelector/affinity GPU product
    GPUsPerReplica  int
    CurrentReplicas int
    ReadyReplicas   int
    PendingReplicas int
    MinReplicas     int
    MaxReplicas     int
}
```

The discovery step is a consolidation of `annotationSourcedVariants` (identity/cost/model/min-max) +
`BuildVariantStates` (role/accelerator/replicas/GPUs). It does **not** add new parsing — it moves the
existing calls behind one boundary and returns `[]VariantMetadata` keyed by `VariantName`.
`EngineParams` (vLLM/SGLang arg parsing for the k2 capacity *estimate*) stays with `saturation` —
it is a capacity input, not identity.

### 3.2 The pure analyzer contract

Analyzers emit **only** demand and per-replica target. Sketch:

```go
type Analyzer interface {
    Name() string
    Analyze(ctx context.Context, input AnalyzerInput) (*AnalyzerResult, error)
}

// AnalyzerResult is a pure (D, P) result. No identity, no engine-owned fields.
type AnalyzerResult struct {
    TotalDemand float64            // D, model scope
    RoleDemand  map[string]float64 // D per role (prefill/decode); nil ⇒ non-disaggregated
    Targets     []VariantTarget    // P per (variant, role)
    Reason      string             // provenance/health hint (observability only)
}

type VariantTarget struct {
    VariantName        string
    Role               string
    PerReplicaCapacity float64 // P
}
```

- **Identity leaves the result.** `Cost`, `AcceleratorName`, `ReplicaCount`, `PendingReplicas`,
  `TotalCapacity`, `Utilization` are removed from the analyzer output — the optimizer gets them from
  discovery, keyed by `VariantName`.
- **Engine-owned fields leave the result.** `RequiredCapacity`, `SpareCapacity`,
  `TotalAnticipatedSupply`, `TotalSupply` move to the engine's post-step
  (`NamedAnalyzerResult`, `optimizer_interfaces.go`) — they are computed by
  `applyUniversalThreshold` from `D` and supply (`Σ replicas·P`, replicas from discovery), never by
  the analyzer.
- **Roles unify.** `RoleDemand` replaces the parallel `RoleCapacities map[string]RoleCapacity`;
  non-disaggregated is `RoleDemand == nil` (one synthetic `both`), exactly the shape `initRoleState`
  (`analyzer_helpers.go:127`) already normalizes to.

### 3.3 Optimizer reads discovery, not saturation

The optimizer keys its per-model pass on the **discovery set** (authoritative variant list + Cost +
Accelerator + Role + replica state) and asks each analyzer only *"what is `P` and demand for variant
`v` / role `r`?"*. Concretely, the reads currently satisfied by `saturationEntry` switch to discovery:

| Today (reads off saturation) | After |
|---|---|
| `saturationEntry(s)` `analyzer_helpers.go:87` | deleted |
| `vc.Cost` in cost-efficiency sort `cost_aware_optimizer.go:238,150,175` | `meta[v].Cost` |
| `vc.AcceleratorName` in `accFromVCs` `analyzer_helpers.go:416`; emitted `:283` | `meta[v].AcceleratorName` |
| `vc.Role` in `variantsForRole`/`rolesOf` `analyzer_helpers.go:226,18` | `meta[v].Role` |
| current replicas (already from `VariantReplicaState`, not vc) | `meta[v].CurrentReplicas` |

`roleBottleneckReplicas` (`analyzer_helpers.go:182`, `max_i ⌈state[i][role]/P_i[v]⌉`) is unchanged — it
already reads `P` per analyzer via `prcForVariant`; it just reads `P` from `VariantTarget` now.

### 3.4 `wva_analyzer_*` metrics

Emit, for every analyzer WVA runs (internal and external), driven from the `(D, P)` outputs:

```text
wva_analyzer_demand{analyzer, namespace, model, role?}          # per model instance
wva_analyzer_target{analyzer, namespace, model, scaledobject}   # per ScaledObject (variant)
```

Additive, alongside `RecordSaturationMetrics` (`internal/metrics/metrics.go:869`). Absence is
meaningful (a missing series is not a zero). This both delivers observability and *enforces* the
two-number discipline at the emission boundary.

### 3.5 External-analyzer wrapper (#1455)

A built-in Go analyzer implementing the pure contract from a PromQL definition (per
`docs/proposals/analyzer-metric-interface.md`): templated `{{model}}`/`{{ns}}` (escaped via
`EscapePromQLValue`) demand & target queries, pod→ScaledObject reduction (avg default), ordered target
fallbacks, `orZero`, three-state absent/missing/present. It registers its queries at runtime via the
new `QueryList.Upsert`/`Remove` (already landed, commit `83a04802`). Because identity now comes from
discovery, **the wrapper supplies no metadata** — only `D` and `P` per variant/role. Catalog lives in a
cluster `wva-analyzers` ConfigMap; a policy entry's `name` resolves **built-in registry first
(internal), then catalog (external)**.

---

## 4. Data-model changes (summary)

> **Status: as built**, with three deviations, each recorded in §5:
>
> 1. `AnalyzerResult` was trimmed to `{AnalyzerName, ModelID, Namespace, AnalyzedAt,
>    VariantCapacities, TotalDemand, RoleDemand}`. The engine-owned fields moved to
>    `allocation.NamedAnalyzerResult` rather than being deleted outright — the optimizer needs
>    them, it just must not receive them *from an analyzer*.
> 2. `VariantCapacity` was trimmed in place rather than renamed to `VariantTarget`, and it
>    **keeps `ReplicaCount`/`PendingReplicas`** (engine-instance units — see the boxed note
>    under §5 increment 2; taking these from `VariantMetadata` would undercount supply on
>    DP>1 deployments).
> 3. `ReplicaMetrics` still carries `Cost`/`AcceleratorName`; the analyzer stopped reading
>    them, but removing the fields is collector-side work outside this plan.

| Type | Change |
|---|---|
| **`VariantMetadata`** (new) | authoritative per-variant identity+state; discovery output |
| `domain.VariantReplicaState` | superseded by / folded into `VariantMetadata` (gains `Cost`, `AcceleratorName`) |
| `domain.AnalyzerResult` | ✅ trimmed to `{AnalyzerName, ModelID, Namespace, AnalyzedAt, VariantCapacities, TotalDemand, RoleDemand}` — the pure `(D, P)` |
| `domain.VariantCapacity` | ✅ identity removed (`Cost`, `AcceleratorName`) and `TotalCapacity` deleted; kept the name, and kept `ReplicaCount`/`PendingReplicas` (instance units — deviation 2 above) |
| `domain.RoleCapacity` / `RoleCapacities` | ✅ analyzers emit `RoleDemand map[string]float64`; the per-role RC/SC live on `NamedAnalyzerResult.RoleCapacities`, built by the engine |
| `domain.ReplicaMetrics` | ⏳ *not done* (deviation 3) — would lose `Cost` and `AcceleratorName`. Both are variant-level facts, resolved once per variant (`replica_metrics.go:983-1003`) and laundered onto every pod; discovery owns them. `ReplicaMetrics` becomes purely per-pod *signal* (KV usage, tokens, rates) + attribution keys (`PodName`/`VariantName`/`ModelID`/`Namespace`). Capacity-store keying already resolves accelerator directly (`engine_v2.go:48`), not from `ReplicaMetrics`. |
| `NamedAnalyzerResult` (`optimizer_interfaces.go`) | ✅ gained `TotalSupply`, `TotalAnticipatedSupply`, `Utilization`, `RequiredCapacity`, `SpareCapacity`, `RoleCapacities` |
| `saturationEntry` | ✅ **deleted**; `saturationNamedEntry` remains, special only in that its `P` sizes replicas |
| `variantRecord` (new, `pipeline/variant_records.go`) | ✅ the optimizer's per-variant view: embedded `VariantMetadata` (identity) + the analyzer's `PerReplicaCapacity`/`Utilization`. Built by `buildVariantRecords`, the single join point. |

---

## 5. Phasing (each phase compiles, tests green, independently landable)

**Phase 0 — Precondition (separate track): remove V1 + queueing-model.** See §6. This design targets
the post-removal engine. If not yet removed, Phases 1–3 still work but must carry the extra switch
arms; removal first is cleaner.

**Phase 1 — Discovery type + producer (additive; no consumer switch). ✅ DONE.**
Introduce `VariantMetadata` and a `DiscoverVariants(ctx) ([]VariantMetadata, error)` that consolidates
`annotationSourcedVariants` + `BuildVariantStates` + role/accelerator resolvers. Produce it each cycle
*alongside* the current path; log/assert it matches the identity the analyzer copies today. **No
behavior change.** De-risks by proving the consolidated source equals the laundered one.

**Phase 2 — Thread discovery metadata into the optimizer; make it the source of truth. ✅ DONE.**
Promote `VariantMetadata` to `domain`; thread the discovery output into
`ModelScalingRequest.Variants`. The engine runs discovery once per model, projects it to
`VariantReplicaState` for the analyzers, and *overlays* the authoritative `Cost`/`Accelerator`/`Role`
onto the analyzers' `VariantCapacity` output before the optimizer runs — so the optimizer reads
identity that came from discovery, not the analyzer's copies. **Behavior-preserving** (values equal
today); no-op on paths that don't run discovery. `saturationEntry` deletion is deferred to Phase 3
(it is still the variant-list/P source until analyzers emit pure `(D, P)`).

**Phase 3 — Trim the contract; analyzers emit pure `(D, P)`; delete `saturationEntry`. ✅ DONE.**
*Done:*
- *(3.0)* `saturation` stopped laundering per-pod `Cost`/`AcceleratorName` onto its output —
  identity now comes from discovery via the builder overlay.
- *(3.1)* Extracted the dedicated capacity-build step (`buildCapacities` in `engine_v2.go`) that runs
  between every analyzer's `Analyze()` and the optimizer.
- *(3.3a)* The builder assembles per-variant identity, model-level supply, and `RoleCapacities`
  (pairing the analyzer's `RoleDemand` with per-role supply grouped from `VariantCapacities`).
- *(3.3b)* Analyzers now emit **pure `(D, P)`**: `saturation`, `throughput`, and the external
  wrapper no longer set `TotalSupply`, `TotalAnticipatedSupply`, `Utilization`, or `RoleCapacities`.
  The builder derives all four from `VariantCapacities` + `RoleDemand`, so the linearity invariant
  (supply = Σ_v replicas × per-replica P) now holds **by construction** rather than by assertion —
  the analyzer-level specs that policed it were removed as redundant.
  `RequiredCapacity`/`SpareCapacity` were already engine-post-step-owned.

- *(3.4, increment 1)* **The result-level type trim landed.** `TotalSupply`,
  `TotalAnticipatedSupply`, `Utilization`, `RequiredCapacity`, `SpareCapacity` and
  `RoleCapacities` are gone from `domain.AnalyzerResult` and now live on
  `allocation.NamedAnalyzerResult`, which the capacity-build step owns. `AnalyzerResult` is
  down to `{AnalyzerName, ModelID, Namespace, AnalyzedAt, VariantCapacities, TotalDemand,
  RoleDemand}` — the pure `(D, P)`. The linearity invariant is now enforced by the type
  system: an analyzer *cannot* write a supply or a scaling signal.
  `buildNamedResult` constructs the entry, runs `buildCapacities`, then seeds the
  optimizer's mutable `Remaining`/`Spare` from the built RC/SC.

- *(3.5, increments 2-3)* **The per-variant trim and `saturationEntry` landed too.**
  `VariantCapacity` lost `TotalCapacity`, `Cost` and `AcceleratorName`; the optimizer
  reads identity from `VariantMetadata` through the engine-built `variantRecord`; the
  Phase-2 overlay shrank to `Role`; `saturationEntry` is deleted. §3.2/§3.3/§4 now
  describe the code as built, with one deviation: replica counts stay on the analyzer's
  output (see the boxed note under increment 2) and the trimmed type keeps the name
  `VariantCapacity` rather than becoming `VariantTarget`.

#### Doing the type-level trim: recipe and pitfalls

First attempted 2026-08-07 and reverted (the production change worked; the **test-fixture
migration** was done with regex and silently corrupted values). Re-done the same day in
increments. Increment 1 is landed; the recipe below is kept because increments 2–3 face
the same fixture problem.

**Increment 1 — engine-owned fields off `AnalyzerResult`. ✅ DONE.** What actually worked:

- Delete `TotalSupply`, `TotalAnticipatedSupply`, `Utilization`, `RequiredCapacity`,
  `SpareCapacity`, `RoleCapacities` from `domain.AnalyzerResult`; add them to
  `allocation.NamedAnalyzerResult`.
- `buildCapacities`, `applyUniversalThreshold` and `warnUnsizableShortfall` take
  `*allocation.NamedAnalyzerResult` instead of `*domain.AnalyzerResult`.
- `runAnalyzersAndScore` constructs the `NamedAnalyzerResult` *before* calling
  `buildCapacities(&nr, ...)`, then seeds `Remaining`/`Spare` from `nr.RequiredCapacity`/
  `nr.SpareCapacity` (they are the optimizer's mutable copies, so they can only be set
  after the build step).
- Add `saturationNamedEntry(s) *NamedAnalyzerResult` beside `saturationEntry`; four
  optimizer sites need it (`cost_aware_optimizer.go` decision loop, `rescale.go`
  `roleDemandGPUs`/`modelDemandGPUs`/`rescaleInputsForGroup`). `roleDemandGPUs` and
  `modelDemandGPUs` change signature to take the named result.

**The hard part is the fixtures, not the code.** ~160 field sites across six
`internal/engines/allocation/*_test.go` files set these fields inside
`domain.AnalyzerResult` literals. They appear in **four** shapes, and a regex that
handles one corrupts another:

1. `x := &domain.AnalyzerResult{` … `}` with one field per line (the common case);
2. comma-joined on one line — `ModelID: id, Namespace: "ns", RequiredCapacity: req,`;
3. inline — `Result: &domain.AnalyzerResult{RequiredCapacity: 20000},`;
4. nested inside a `NamedAnalyzerResult{Result: &domain.AnalyzerResult{…}}` literal
   (`analyzer_helpers_test.go`), where the fields hoist to the *outer* literal.

Plus: multi-line `RoleCapacities: map[string]domain.RoleCapacity{…}` blocks (21 of them),
and trailing `// comments` on a field line, which fold into the rest of the line if the
block is collapsed to one line.

**Gate the migration on a positional verifier.** Compare each rewritten fixture against
`git show HEAD:<path>`, matching the Nth `AnalyzerResult` literal to the Nth rewritten
signal literal *in document order*, per file. Collapsing by variable name gives false
positives (many blocks are all named `r`), and a global `str.replace` of a
`fooSig := satSignals{}` placeholder will copy one block's value onto every other block —
that is exactly what went wrong. Values that are expressions (`req`, not `25000`) must
survive verbatim.

Given the above, hand-migrate the fixtures or use an AST rewrite (`go/ast` +
`go/printer`); regex is not sufficient here.

**What actually worked (increment 1), and why it was cheap.** The fixtures split into two
populations, and each got a different treatment:

1. *Optimizer tests* (`cost_aware_optimizer_test.go`, `greedy_score_optimizer_test.go`,
   `optimizer_equivalence_test.go`, `rescale_optimize_test.go` — 96 literals) test what the
   optimizer **does with** a signal, not how it was derived. They all funnel through one
   `withSatEntry`-family helper. So a test-local builder `satEntryFixture` was added with
   the *pre-trim* field set, and the migration became a **one-token rename** of the literal
   head (`&domain.AnalyzerResult{` → `&satEntryFixture{`) with **every field left on its
   original line**. Zero value movement, therefore zero corruption risk. The helper does the
   split in one place (`named()`).
2. *Engine tests* (`engine_v2_capacity_build*_test.go`, `engine_v2_threshold_test.go`,
   `engine_v2_test.go` — 33 literals) pin the derivation itself, so they use the real
   `allocation.NamedAnalyzerResult`. Here fields genuinely move, so a brace-aware,
   **per-literal** script regrouped them (engine fields stay outer, `(D, P)` fields move into
   a nested `Result:`), copying each field's text verbatim. Never a global `str.replace`.

**Both populations were gated on a positional verifier that passed before anything was
committed:**

- population 1: `git show HEAD:<file> | sed '<the one rename>'` diffed against the working
  tree — the diff contained *only* the intended `NamedAnalyzerResult` collapses, proving no
  fixture value changed;
- population 2: per file, the Nth `&domain.AnalyzerResult{` literal in `HEAD` was matched to
  the Nth `&allocation.NamedAnalyzerResult{` literal now, and their field-line multisets
  compared (minus the two wrapper lines). 29/29 verified identical.

Run `gofmt` afterwards: regrouping leaves stale alignment padding. On this Windows checkout
gofmt must be run on LF-normalized copies (see the autocrlf note in the developer guide),
then written back.

One semantic change to watch for: a collapsing helper that sets `Live: true` where the
hand-built literal left it false. `Live` is read only by `needsScaleDownForRole` and
`safeRemovalReplicasForRole`, so it is inert for scale-up fixtures — but check, don't assume.

> **Correction (2026-08-07), from doing increment 1.** Two errors in the increment 2/3
> sketch below were found and the ordering became **3 before 2**. Both increments have
> since landed in that order. The boxed notes are kept because they record constraints
> that still bind anyone touching this code.

**Increment 2 — trim the per-variant contract. ✅ DONE.** `domain.VariantCapacity` is now
`{VariantName, Role, ReplicaCount, PendingReplicas, PerReplicaCapacity, Reason,
TotalDemand, Utilization}` — the analyzer's per-variant `(D, P)` and nothing else:

- `TotalCapacity` deleted (written by all three analyzers, read by nothing — only
  `metrics.go`'s help text mentioned it).
- `Cost` and `AcceleratorName` deleted. They are discovery's, and after increment 3
  nothing read them here. The capacity builder's Phase-2 overlay shrank to `Role` alone.

`Role` deliberately stays. It is not identity on this struct but the key the analyzer
attributed demand by: `buildRoleCapacities` pairs `RoleDemand` with the per-role supply
grouped from these entries, so both halves must be keyed the same way. The builder still
overlays it from discovery so the two cannot drift.

The type is not literally renamed to `VariantTarget` — the remaining fields are the
target plus the replica counts that make it a supply, and `VariantCapacity` still
describes that accurately.

> **Do NOT take replica counts from `VariantMetadata`.** The original sketch said
> "`aggregation` needs replica counts from metadata rather than from the analyzer output".
> That is wrong and would silently over-scale every DP>1 deployment.
> `VariantCapacity.ReplicaCount` is in **engine-instance units** — `saturation` sets it
> to `len(replicas)`, and the collector keys `ReplicaMetrics` by `pod_name:port` "to support
> multiple instances per pod" (`collector/replica_metrics.go:747`); a DP=8 pod hosts 8
> independently-capacitied instances. The analyzer even rescales `PendingReplicas` by
> `instancesPerUnit := len(replicas) / readyCount`. `VariantMetadata.CurrentReplicas` is the
> *scale-target* count (pods, or LWS groups). Sourcing supply from it would undercount
> `TotalSupply` by the DP factor. Replica counts are an analyzer-**measured** quantity;
> they stay on the analyzer's output. What `VariantTarget` sheds is identity
> (`Cost`, `AcceleratorName`) and the derived `Utilization` — and identity can only go once
> increment 3 has re-keyed the optimizer, which is why 3 now comes first.

**Increment 3 — delete `saturationEntry`. ✅ DONE.** The only increment that changed
optimizer *logic*. `saturationEntry` is gone; `saturationNamedEntry` remains, and what is
still special about that entry is only that its `P` sizes replicas — the coordination
math, deliberately unchanged per §2.

**What made it a small diff.** Rather than thread `[]domain.VariantMetadata` and look `P`
up separately at every site, the engine now *builds* the optimizer's per-variant view:

```go
type variantRecord struct {
    domain.VariantMetadata          // identity, embedded — discovery's
    PerReplicaCapacity float64      // the analyzer's P
    Utilization        float64      // the analyzer's per-variant ratio
}
```

Because the metadata is embedded, `vc.Cost`, `vc.AcceleratorName`, `vc.Role` and
`vc.PerReplicaCapacity` all still resolve — so every helper *body* was unchanged and only
the signatures moved from `[]domain.VariantCapacity` to `[]variantRecord` (37 mechanical
substitutions across four files). `buildVariantRecords` in `variant_records.go` is the
single join point, and `recordsForRequest` is what the optimizers call.

Helpers re-keyed: `rolesOf`, `variantsForRole`, `buildCapacityMap`,
`sortByCostEfficiencyAsc`, `accFromVCs`, `singleAccType`, `variantsOnType`,
`modelRolesOnType`, plus `costEfficiency`, `scaleDownVariantSet`,
`sortVariantsForScaleDown`, `anyHasReplicas`, `fillRole`, `reclaimRole`, the
`RolePickFn` signature and `allocateForModelPaired`. `roleCurrentGPUs`/`roleFloorGPUs`
now take the records and the state map instead of re-deriving both from the request.
`computeCurrentGPUUsage`/`…ByNamespace` in the saturation engine were unified into
`gpuUsageByType`, which reads accelerator from `req.Variants`.

> **`prcForVariant` is not one of them.** The original list said nine helpers including
> `prcForVariant`. `PerReplicaCapacity` is the analyzer's `P` — it is not on
> `VariantMetadata` and must keep reading the analyzer result.

Re-keying is safe because `Variants` is never sparser than the saturation analyzer's
output: `engine.go:1260-1262` derives `variantStates` from `variantMetadata` one-for-one,
and `saturation` emits exactly one `VariantCapacity` per `variantState`. (This does not
hold for *every* analyzer — throughput skips variants whose ITL model will not resolve —
but the helpers read the saturation entry specifically.)

**`req.Variants` is now required.** `buildVariantRecords` returns nil without it, so the
optimizer skips the model. There is no fallback to the analyzer's copies: with identity
gone from `VariantCapacity` such a fallback could supply neither cost nor accelerator, and
running the cost-aware math on zero costs would pick arbitrarily while looking like a
working decision. Metadata also *drives the result set* — a discovered variant the
analyzer did not size gets a zero `P` (every consumer skips those) instead of vanishing,
so the optimizer's view of the fleet no longer depends on which variants an analyzer
happened to emit.

**Test note.** Populating `req.Variants` in the fixtures is what makes the optimizer suite
exercise the discovery path at all; without it every test would have silently run the
no-discovery branch. After wiring `deriveVariants` into the `withSatEntry` helpers, the
branch was made to `panic` and the suite re-run — it reached zero call sites, which is how
the coverage was confirmed rather than assumed. `variant_records_test.go` covers the join,
the metadata-drives-the-set rule, ordering, and the nil cases directly.

**Phase 4 — `wva_analyzer_*` metrics. ✅ DONE.** `wva_analyzer_demand` (per model instance, per role
when disaggregated) and `wva_analyzer_target` (per-replica P per variant) are emitted for every
analyzer that runs each cycle, straight from the `(D, P)` it produced — see `recordAnalyzerMetrics`
in `engine_v2.go`.

§3.4's "absence is meaningful (a missing series is not a zero)" is enforced, not just asserted: the
engine evicts analyzer series it stops publishing. Prometheus gauges cannot enumerate their own
children, so `Engine.lastAnalyzerSeries` records what each model published last cycle and
`evictStaleAnalyzerSeries` deletes what is no longer emitted — a role that disappears when a fleet
stops being disaggregated, a variant that is removed, an analyzer that gets disabled.
`pruneAnalyzerSeries` clears a whole model instance when it stops being reconciled, mirroring
`pruneLastGoodAnalysis` including its empty-active-set guard. Eviction always runs **after** the
cycle's `Set` calls, per the existing "no zero-value window" rule, so a surviving series is never
briefly absent from a concurrent scrape.

**Phase 5 — External-analyzer wrapper (#1455). ✅ DONE.** Delivered as §10 describes (catalog CM +
per-engine bodies + runtime registry + per-cycle reconcile + built-in→catalog name resolution). Unit
+ envtest coverage at every layer. The KEDA external-scaler **kind e2e** has since been run green
(smoke 17/17, full 28/28), and the saturation suite was moved onto the external-scaler transport so
the analyzer decision itself is exercised over gRPC, not only the wrapper.

---

## 6. Removal of V1 + queueing-model (precondition detail) — ✅ DONE

Completed this session: V1 (`internal/saturationv1` + `optimizeV1` + helpers) and the queueing-model
analyzer (`internal/engines/analyzers/queueingmodel` + `engine_queueing_model.go`) are removed, V2 is
the sole analysis path, `DefaultVariantCost` was relocated to `internal/domain`, the dead
`internal/queueing` math package and the queueing-model *config* plumbing (`Config.QMAnalyzer*`,
`domain.QueueingModelScalingConfig`, the reconciler QM handlers) are removed;
`RegisterQueueingModelQueries` is retained (shared with the throughput analyzer). The `engine.go` line
references below are from the pre-removal plan and no longer resolve.

- **`saturationv1.DefaultVariantCost`** is used outside V1 — `engine.go:1505` and
  `collector/replica_metrics.go:998`. Removing `internal/saturationv1/` must **relocate this constant**
  (e.g. to `internal/domain` or `internal/annotations`, next to the `llm-d.ai/variant-cost` default).
- V1 wiring to delete: `engine.go` `v1Analyzer` iface (`:69`), `defaultV1AnalyzerFactory` (`:86`),
  `v1AnalyzerFactory` field/wiring (`:230,283`), `optimizeV1` (`:665`), switch default (`:558`).
- Queueing-model wiring to delete: `engine.go` field (`:181`), construction (`:278`), selection
  (`:508-524`), `optimizeQueueingModel` (`engine_queueing_model.go`), switch arm (`:554`).

---

## 7. Test impact

- **Analyzer suites** (`saturation/*_test.go`, `throughput/*_test.go`) — update result assertions to
  the trimmed `(D, P)` shape.
- **Engine** — `engine_register_test.go`, `engine_v2_threshold_test.go` (`applyUniversalThreshold`),
  `engine_v2_population_test.go`; add discovery-producer tests.
- **Optimizer** — `analyzer_helpers_test.go` (`makeNamed`/`makeNamedPD` builders switch to supplying
  metadata via discovery, not the sat result), `cost_aware_optimizer_test.go`,
  `optimizer_equivalence_test.go`.
- **Config** — catalog parsing + name resolution (Phase 5).
- **e2e** — re-run the KEDA external-scaler kind smoke on the epic branch after Phase 5.

---

## 8. Risks & mitigations

1. **Optimizer-core blast radius (Phase 3).** Mitigation: Phases 1–2 make the source switch
   behavior-preserving *before* the shape change; Phase 3 is then mechanical on a green base. The
   `optimizer_equivalence_test` is the guardrail.
2. **`DefaultVariantCost` relocation collides with the V1-removal track.** Mitigation: relocate the
   constant as the *first* step of whichever track lands first; the other rebases onto it.
3. **Discovery/laundering mismatch** (a field the analyzer massaged, e.g. DP-rank `ReplicaCount`
   conversion at `analyzer.go:400-416`). Mitigation: Phase 1's parallel-produce + assert catches any
   divergence before consumers switch; DP-rank conversion is a *capacity* concern and stays in the
   analyzer, not discovery.
4. **Shadow-pod / role attribution** relies on `llm-d.ai/role` + owner-walk. Mitigation: discovery
   reuses the exact existing resolvers (`getRoleFromScaleTarget`, locator), no new attribution logic.

---

## 9. Open questions

- **Discovery placement:** a new `internal/gpunodes` package, or a method on the engine consuming
  `internal/utils/variant.go`? (Leaning: thin `internal/engines/variantmeta` that returns
  `[]VariantMetadata`, so analyzers/optimizer import a type, not the engine.)
- **`VariantReplicaState` vs new `VariantMetadata`:** extend the former in place, or introduce the
  latter and deprecate? (Leaning: introduce `VariantMetadata`, alias/embed during transition.)
- ~~**Per-pod `AcceleratorName` on `ReplicaMetrics`:**~~ **Resolved** — drop `AcceleratorName` *and*
  `Cost` from `ReplicaMetrics`. Both are variant-level facts resolved once per variant
  (`replica_metrics.go:983-1003`) and stamped identically onto every pod; no consumer needs them
  per-pod (capacity keying already uses `GetAcceleratorNameFromScaleTarget` directly at
  `engine_v2.go:48`). Discovery owns them; `ReplicaMetrics` keeps only per-pod signal + attribution
  keys.

---

## 10. #1455 external analyzers — as built

The external-analyzer wrapper is wired through the **ScalingPolicy configuration system**, split
across ConfigMaps exactly as §7.6 of the KEDA-external-scaler proposal describes — not collapsed into
a single policy entry:

- **Catalog** (cluster CM **`wva-analyzers`**) — external analyzer *definitions*: `label →
  {engines: {vllm:{query,threshold}, sglang:{…}} | query,threshold}` (per-engine bodies, mirroring
  the collector's `registerForEngine`; an engine-agnostic body has no `engines:` map). Parsed by
  `config.ParseAnalyzerCatalogConfigMap`, stored on `Config`, refreshed by the ConfigMap reconciler.
- **Policy** (tier CM) — *selects/weights* by name (`analyzers: [{name, enabled, score}]`). A policy
  entry's `name` resolves **built-in registry first, then catalog**; because a plain `{name: ttft-slo}`
  entry has `EffectiveType() == Name`, the existing `effectiveEnabled`/`scoreForAnalyzer`/
  `resolveThresholds` matching needed no change — resolution is just "construct from the catalog when
  the name is not a built-in".
- **Wrapper** — `internal/engines/analyzers/external`: emits pure `(D, P)` (`desired = ceil(D/P)`),
  selects the query body by the variant's `Engine` (a discovery field via `inferenceengine.Detect`),
  and returns a nil result (not-defined → skipped) when no body matches.
- **Runtime add/remove** — the engine holds a lock-guarded, name-keyed external registry
  (`UpsertExternalAnalyzer`/`RemoveExternalAnalyzer`) separate from the frozen built-in snapshot, and
  `reconcileExternalAnalyzers` syncs it with the catalog **each optimize cycle**, so a `wva-analyzers`
  edit takes effect **without a restart**.

## 11. Security — PromQL trust model

The catalog introduces a raw-PromQL config surface. The posture matches KEDA's Prometheus scaler,
with one addition we already have:

- **The query body is trusted config.** Like KEDA's ScaledObject `query`, the catalog PromQL is run
  as-is — WVA does **not** semantically sanitize or whitelist it (infeasible, and KEDA doesn't). The
  trust boundary is **RBAC on the `wva-analyzers` ConfigMap**: editing it is a privileged operation.
- **Interpolated identity is escaped.** Unlike KEDA, WVA interpolates `{{.modelID}}`/`{{.namespace}}`
  (from a less-trusted ScaledObject annotation) into the query — and `PrometheusSource.executeQuery`
  runs both through `EscapePromQLValue` before substitution (`prometheus_source.go:121-123`), so a
  crafted `modelID` cannot break out of a label matcher. This is the genuine injection defense.
- **Bounded cost + fail-safe.** Each query has a 10s timeout (`prometheus_source.go:35,140`); a
  failed/empty/malformed query yields **0 demand → no scaling action**, so a bad definition cannot
  drive runaway scaling — it simply contributes nothing. Heavy expressions should be precomputed as
  Prometheus recording rules.
- **Load-time validation is deliberately minimal** — `external.New` rejects an empty query or a
  non-positive threshold (bad def skipped, logged); we do **not** pull in the full
  `github.com/prometheus/prometheus` PromQL parser just to syntax-check, since the timeout + fail-safe
  already contain a bad query's blast radius.
