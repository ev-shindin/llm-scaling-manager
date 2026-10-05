# Proposals

Design notes for work that is not built yet, or is built and still moving. A
proposal here is **not** a description of current behaviour — check its status
header before trusting it, and prefer the guides and reference docs for how the
system works today.

## Warm capacity

Read the review first; the implementation design says what was built.

- **[Fast model loading, from first principles](fast-model-loading.md)** —
  **start here.** Restates the problem commercially before technically: the
  competitor is a spare replica, not doing nothing, so the pool has exactly one
  thing to sell — one slot covering many models. Sizes that with a loss model,
  names the assumption that can end the project (spike correlation), and lists
  the measurements to take before any code is written.
- **[Warm pool implementation](fast-model-loading-implementation.md)** — how to
  build it: objects, the supervisor API, how a woken model joins its
  InferencePool, the cache policy, the RBAC increase a readiness gate needs, and
  a three-week breakdown for phase 1.
- **[Warm pool: open questions](warm-pool-open-questions.md)** — the three
  things the pool raises that are bigger than a bug fix: the optimizer cannot
  tell a pool replica from a serving one, the pool's vLLM version belongs to FMA
  rather than to us, and what either costs to change. Also records what HAS been
  decided, so it is not re-litigated.
- **[FMA post-mortem](fma-post-mortem.md)** — Fast Model Actuation was the
  earlier route to the same goal. This records what was tried, the measurements
  that killed it, and the four things in this repo that are still FMA and must
  not be swept up with it — starting with the pool's supervisor, which IS FMA's
  launcher. It replaces eight documents removed on 2026-08-27; they are in git
  history.

- **[Warm pool: configuration surface](warm-pool-configuration.md)** — which
  knobs the pool needs and where they live. The pool is a Deployment and its
  settings are annotations on it; three flags were deleted as facts the cluster
  already states.
- **[Retained pools](warm-pool-retained.md)** — built. Holding several large
  models on one set of GPUs, and the rule deciding which one stays awake: the
  model under more pressure, never a coin-flip between two that both want to
  scale.
- **[Warming an engine that spans Pods](warm-pool-lws.md)** — implemented.
  Multi-node sleep and wake on a LeaderWorkerSet, demonstrated without Ray.
- **[Sleep level 1 or 2](warm-pool-sleep-levels.md)** — measured. The pool
  implements level 1 only; this is the case for that, and what level 2 would
  cost if host RAM ever becomes the binding constraint.
- **[Transferring weights instead of reading them](warm-pool-weight-transfer.md)**
  — investigated, nothing built. What the shipped machinery can and cannot do,
  and which of it is worth building.

## Scaling and actuation

- **[WVA as a KEDA external scaler](wva-keda-external-scaler.md)** — the actuation
  design: how WVA drives KEDA, and the ScalingPolicy tiers.
- **[Scale-from-zero: the missing signal](scale-from-zero-missing-signal.md)** —
  why a parked model needs a push, and where it comes from.
- **[Priority scoping](priority-scoping.md)** — parked. Read before redesigning.
- **[A release the swap can survive](managed-keda-behavior.md)** — measured: a
  ten-replica release takes 420 s under a 300 s stabilization window, so a role
  waiting on GPUs another role has been told to free waits that long. Proposes
  shortening that one field, on the shrinking target only, through the
  `wvaOwnership` opt-in. Also records what the same run does **not** show, and
  the plateau that was wrongly blamed on it.

- **[WVA as a KEDA external scaler: the argument](wva-external-scaler-proposal.md)**
  — the shorter framing of the same design: compute the target, let KEDA
  actuate, and why that beats publishing a metric.
- **[The alternative framing](wva-external-scaler-alternative.md)** — the reply
  to "WVA as a metric shop", keeping the capacity model in WVA.

## Engine and analyzers

- **[Analyzer metric interface](analyzer-metric-interface.md)**
- **[SGLang backend](sglang-backend.md)**
- **[Sizing a backlog](backlog-sizing.md)** — what the shape-swap P/D run
  showed: peaks of 7 and 9 decode replicas against a need of 2, troughs of 1
  that caused the next peak. Three fixes landed (the throughput floor among
  them); the open question is that a queued request is charged as KV held at
  once, when a backlog needs throughput.
- **[What counts as a serving replica](what-counts-as-a-serving-replica.md)** —
  the count WVA derives capacity from means "Pods that reported metrics", is
  used as though it meant "Pods taking traffic", and the gap produced three
  symptoms investigated as unrelated bugs.

Traffic shape and the units capacity is priced in — read the per-token economy
last; it is the unit change that makes the other two land.

- **[Traffic shape shifts](shape-shift-treatment.md)** — what a shape change
  does physically, what the analyzer sees late, and the four directions with
  which rows are measured and which are predicted. Derives decode's mu from the
  ITL line so a shift reprices on the cycle it is observed.
- **[Prefill capacity from a fitted TTFT model](prefill-ttft-model.md)** —
  prefill has no throughput ceiling today. Fits `TTFT(T) = A*T + B` so `1/A` is
  the replica's prefill token ceiling, obtainable without driving it into
  saturation.
- **[A per-token economy](per-token-economy.md)** — the reference for every
  component and formula in the saturation analyzer, and the argument for making
  tokens the native unit of demand and capacity on both roles. Both documents
  above convert their result back into requests per second at the boundary;
  this one deletes that conversion, which is what the stale-shape hold, the
  output-length bucket keys and prefill's dropped backlog all depend on.
  Measures the `I`-up direction the shape-shift table lists as unrun.

- **[Structuring the analyzers](analyzer-structure.md)** — the analyzers
  work; reading them is expensive, and that cost is now producing defects.
  Measures what is actually wrong (two grab-bag files of 35 and 43 functions,
  one 155-line pipeline, the same weighted mean written three times, a
  vestigial `_v2`) and what is NOT (function length, and the comment density,
  which is why the system is debuggable and must survive). Six stages, each
  landable alone, none permitted to change a decision.

- **[Every signal as a metric](signals-as-metrics.md)** — the numbers that
  produce a scaling decision exist only as log fields; 41 metrics are published
  and nearly all are outcomes or health. Proposes the names and the label
  vocabulary for the chain in between, with the cardinality arithmetic and the
  reason-label rule it has to obey. Draws the line this repo already argued:
  observability yes, a metric for KEDA to threshold instead of WVA computing
  the target no.

- **[Learned state across a restart](learned-state-across-restarts.md)** —
  nothing the analyzer learns survives a restart, and a rollout mid-ramp costs
  the ~8 minutes and 51 itlZero cycles measured on run QT. Two changes that only
  work together: key the learned figures on an ENGINE CONFIG fingerprint rather
  than a variant name (the relation already exists as IsCapacityCompatible, used
  only as a fallback), and rehydrate on becoming leader from a write-on-change
  ConfigMap — NOT from the metrics, which have no writer identity, no honest
  timestamp and no trust boundary. Revises signals-as-metrics.md, which called
  the restart gap unfixable.

## Product and lifecycle

- **[Capacity-planner positioning](capacity-planner-positioning.md)** — where a
  cluster capacity planner sits relative to WVA.
