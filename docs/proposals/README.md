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
- **[Reading the weights into memory while the engine starts](weights-prewarm-on-schedule.md)**
  — proposed. A background reader, started by the engine container's command
  before it execs the engine, reads the pod's weight files into the page cache
  while the engine imports: same container, UID and memory cgroup, no new
  object. Measured 19.9 s to 10.0 s of weight load on a 32B model; estimated
  60-90 s on a GLM-class node with one disk and ~0 with striped NVMe. Reads
  every file, because vLLM does. Off by default.

## Scaling and actuation

- **[WVA as a KEDA external scaler](wva-keda-external-scaler.md)** — the actuation
  design: how WVA drives KEDA, and the ScalingPolicy tiers.
- **[Scale-from-zero: the missing signal](scale-from-zero-missing-signal.md)** —
  why a parked model needs a push, and where it comes from.
- **[Priority scoping](priority-scoping.md)** — parked. Read before redesigning.
- **[Kueue borrowing, and limiters that compose](kueue-borrowing-and-limiter-composition.md)**
  — built (fork #136, upstream WVA #1003). #164 makes every declared limiter
  bound together and a replica fit a node; #180 counts borrowable cohort
  capacity from lenders Kueue would reclaim for, where Kueue governs the pods
  and a GPU bound is declared. Counting other lenders (PR-C) is an open,
  opt-in follow-up.
- **[One claims ledger](claims-ledger.md)** — proposed. What a replica that is
  ordered but not yet visible holds is kept in six stores today, each with its
  own keys and clocks; the limiter fixes in #183–#187 were gaps between them.
  One entry per replica claim, one lifecycle and timeout, persisted, read by
  both engines through one view; Kueue borrowing moves to its own package.
- **[Model Kueue per ClusterQueue](kueue-per-clusterqueue.md)** — proposed.
  Bind each variant to its ClusterQueue through its pod template's
  `kueue.x-k8s.io/queue-name` label and compute every Kueue figure per queue.
  Replaces #186's per-namespace min and `multi-queue-namespace`, and makes a
  shared queue one budget instead of a ceiling each namespace sees in full.
- **[Utilization-share optimizer](utilization-share-optimizer.md)** — built:
  shadow mode and actuation (stages 1 and 2); the short window for urgent
  transfers (stage 3) is design. Spend the whole quota: every model gets what it
  needs, and the spare is shared as headroom by weight; when the quota cannot hold every
  model's peak at once, capped at one replica per role times its weight by
  default (`maxHeadroomReplicas`), the rest left unassigned. Rebalance by transfer
  when a model leaves a tolerance band, scaling down first and scaling up only
  into GPUs the donor has actually released. Built for P/D models on multi-GPU
  replicas; what stage 2 does not yet do is listed in its §13.
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

- **[Replicas that change P/D role in place](pd-role-switch/)** —
  experimental; needs a patched vLLM that is not shipped. Why a stock llm-d
  deployment cannot re-role a replica (the role label is in an immutable
  selector), a deployment shape that can, and an
  [evaluation plan](pd-role-switch/evaluation.md) for what switchability costs.
- **[Analyzer metric interface](analyzer-metric-interface.md)** — partly
  built: the per-analyzer `(D, P)` contract, the `wva_analyzer_*` series and
  an external PromQL analyzer. How analyzers combine was replaced by the
  composite below.
- **[The composite analyzer](composite-analyzer.md)** — built, still moving.
  How listed analyzers combine into the one signal the optimizer reads, the
  decisions behind it, its known limits (#305, #226) and the target: each
  analyzer at its own band, and no dependence on saturation's units.
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

- **[One learner lifecycle for the analyzers](analyzer-learner.md)** —
  proposed. Fourteen learned quantities each admit, fit, forget and log in
  their own way, and the same defects (transients admitted, a first sample
  taken raw, stale values, missing readings read as zero, one reading
  dominating) were fixed in one after another. One lifecycle (admit with a
  reason, fit, predict with an age, validate, refit / forget / hold) and one
  settled-replica tracker; each learner keeps its model and its direction
  policy, so k2 still only falls.

- **[Learned-state sharing by similarity](learned-state-sharing-by-similarity.md)**
  — each variant stores only its own k2, throughput and ITL readings, and a
  variant with none reads the most compatible other variant's: same flags,
  then compatible on what matters, then the formula. Today k2 and saturated
  throughput are pooled across namespaces and engine configurations, and the
  pooled k2 becomes a variant's own, so a donor with a higher one makes it
  order fewer replicas. A borrowed reading may only make a variant more
  conservative, and it expires, because a k2 that is too low never corrects
  itself: nothing saturates, so nothing is measured. Replaces the identity
  digest as the gate for sharing on #126, after seven measured ways to make
  that digest wrong.
- **[Engine structure](engine-structure.md)** — scope and layering of the
  engine packages: what to split, and which way imports may point.
- **[A role at its ceiling starves every other role](prefill-starved-by-exhausted-role.md)**
  — on a P/D fleet, one role at its `maxReplicas` aborted the scale-up pass for
  both roles; one `break` in the paired allocator.
- **[Sizing against in-flight capacity](sizing-against-in-flight-capacity.md)**
  — count replicas already starting, and when they will serve, before ordering
  more or releasing one.
- **[Learned state across a restart: a snapshot](learned-state-snapshot.md)**
  — a restart forgets every k2, throughput window, ITL baseline and start
  estimate, and a forgotten k2 falls back to a higher capacity, so fewer
  replicas. Stores them in one ConfigMap, each entry with its own clocks so a
  restore never makes a figure younger; leader-only writes, deletion flushes,
  off by default. Supersedes the Prometheus refit in #116, which cannot
  attribute old readings to a variant now that learned state is per variant.
- **[SLO tiers](slo-tiers.md)** — phase 0 (the analyzer, observing) and step
  2a (the TTFT curve's inputs, observed) built; nothing that changes a replica
  count is. A separate `slo` analyzer prices each variant at the fill where its
  fitted ITL line meets a mean-TPOT target, and judges a TTFT percentile on the
  router's histogram, the one TTFT that includes the flow-control queue.
  Combined later by the composite analyzer: guard mode (max with saturation)
  or sizer mode (saturation explicitly disabled, the per-variant fallback).
  Review findings are folded in.

## Product and lifecycle

- **[Capacity-planner positioning](capacity-planner-positioning.md)** — where a
  cluster capacity planner sits relative to WVA.
