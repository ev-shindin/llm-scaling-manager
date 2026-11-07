# Kueue borrowing, and limiters that compose

- **Problem:** a Kueue-bounded namespace never grows into capacity its cohort
  lends ([#136](https://github.com/ev-shindin/llm-scaling-manager/issues/136)),
  and two declared limiters do not bound a decision together.
- **Decision:** first make every declared limiter bound every decision, and,
  under the physical limiter, make a replica fit a node (Part A). Then count
  borrowable cohort capacity where that is safe (Part B).
- **Shape:** Part A shipped as
  [#164](https://github.com/ev-shindin/llm-scaling-manager/pull/164)
  (`feat/limiters-compose`). Part B and its observability shipped as PR-B,
  [#180](https://github.com/ev-shindin/llm-scaling-manager/pull/180)
  (`feat/kueue-borrowing`). Counting capacity of lenders that Kueue does not
  reclaim for is the opt-in PR-C ([Appendix](#appendix-pr-c)).
- **Opt-out:** `countBorrowable: false` on the quota entry gives the Kueue cap
  without borrowing. Part A still applies.
- **Status:** built: Part A (#164) and Part B (#180), from revision 11. PR-C
  is built (`feat/kueue-borrow-release-v2`), opt-in and experimental. The operator pages are
  [the quota limiter](../reference/quota-limiter.md#cohort-borrowing) and
  [the GPU limiter](../reference/gpu-limiter.md).

Borrowing is experimental but on by default. Where a condition does not hold,
borrowing is either **withheld** (borrowable 0, capped at nominal as with
`countBorrowable: false`) or **frozen** (nothing new is borrowed and the last
figures are kept). Either way the reason is published on `wva_quota_borrow_withheld`.

## Terms

- **Nominal**: a ClusterQueue's `nominalQuota`.
- **Guaranteed**: the part of nominal the queue can count on now.
- **Within nominal**: `min(nominal, guaranteed + cohortFree)`, what Kueue would
  admit now without borrowing.
- **Lender**: a cohort member whose unused nominal others borrow.
- **`reclaimWithinCohort`**: whether a lender can take lent capacity back by
  preemption (`Any`, `LowerPriority`, `Never`).
- **cohortFree**: the cohort's free pool, what reclaiming lenders lend minus
  what members use beyond what they keep ([B3](#b3-how-much-a-queue-can-reach)).
- **Borrow cap**: how far past guaranteed a namespace may go.
- **Flavor**: a Kueue ResourceFlavor. A namespace's own flavors are those
  where its queue has nominal quota.
- **Closed limiter**: one whose list of GPU types is complete, so a type it
  omits is denied. Physical inventory and a cluster-scope quota are closed; a
  namespace-scope quota is closed for its own namespaces.
- **Family key**: a GPU name that covers products. `H100` is the family of
  `NVIDIA-H100-80GB-HBM3`.
- **Node view**: one cycle's free GPUs per schedulable node, debited as
  replicas are ordered.
- **Unit**: what is placed or released whole: a replica, a P/D pair, or a
  LeaderWorkerSet group.
- **Placement timeout**: how long a borrowed unit may stay unplaced before it
  is released (default 5 minutes).

## What changes for you

1. **A policy that declares both `gpu-inventory` and `quota` is bounded by
   both.** Today the quota wins and the physical limiter is dropped. Physical
   GPUs count every GPU pod, training included, so the bound can be tighter.
   It only stops growth, never shrinks a running model. For one release, a log
   line `Upgrade note (transitional)` names the models it now holds back.
2. **Limiter entries that list different GPU types stop allowing a type that
   one entry denies.** Bug fix.
3. **A replica must fit a node, not just the pool**, while the physical
   limiter bounds the cycle. Today 16 free GPUs as 4+4+8 order an 8+8 P/D pair
   whose prefill stays Pending forever. Bug fix.
4. **P/D pairs stop ordering both roles past a budget.** Today a budget of 8
   with two 8-GPU roles orders 16 GPUs. Bug fix.
5. **A Kueue-bounded quota counts capacity it can borrow**, by default, only
   where every [B1](#b1-when-borrowing-is-counted) condition holds and only
   from lenders Kueue would reclaim for. Opt out with `countBorrowable: false`,
   or `WVA_QUOTA_KUEUE_BORROW=false` at install.
6. **Borrowing needs a GPU bound you declare**: a `gpu-inventory` entry, or a
   GPU `ResourceQuota` whose `hard` is at most nominal plus `borrowingLimit`.
   The installer does not add one. Without it the reason is `no-gpu-bound`.
7. **New read-only RBAC**: `cohorts`, `resourcequotas` in managed namespaces,
   `events`, `batch` Jobs, and a Role in the Kueue namespace to `get` the
   `kueue-controller-manager` Deployment and its leader Lease. The cluster-wide
   `workloads` and `localqueues` read is its own ClusterRole
   (`wva-kueue-borrow-reader-role-<suffix>`): `make setup-prereqs` creates it
   only where a policy counts borrowing, and revokes it otherwise; without it,
   `lenders-unreadable`. **Without the new Role, borrowing freezes**
   (`kueue-controller-down`).
8. **`WVA_LIMITER` accepts a list**, and
   `make disable-limiter WVA_LIMITER_TYPE=gpu-inventory|quota|all` removes one
   kind or all.

## The problem

Today the Kueue quota source caps a namespace at its ClusterQueue's nominal.
When another queue in the cohort is idle, Kueue would admit more replicas, but
the scaling manager never asks for them.

Counting borrowed capacity raises a quota above the guaranteed figure. That is
safe only if something still checks that the GPUs exist and that a replica
fits on a node. Nothing does today, because limiters do not compose:

| Combination | Works today? | What happens |
|---|---|---|
| `gpu-inventory` alone, or one `quota` entry | yes | |
| `gpu-inventory` + `quota` | **no** | the quota wins; the physical limiter is never built |
| two entries that list **different** types | **no** | the allowed types are the **union** (measured) |
| one GPU spelled two ways (`H100`, `NVIDIA-H100-80GB-HBM3`) | **no** | treated as two types (measured) |
| P/D pair against a shared budget | **no** | both roles commit before the budget is reduced |
| free GPUs split across nodes | **no** | ordered by the pool total; a replica larger than any hole stays Pending |
| Kueue borrowing | **no** | not read (#136) |

The union leak installs today: a cluster quota `{H100: 8}` plus a namespace
entry `x: {A100: 4}` lets `x` run A100s the cluster quota denies.

## Part A: limiters compose (#164)

A decision must fit **every** declared limiter. A GPU type is allowed only if
every limiter with a view on it allows it, and the budget is the smallest
figure among them. Internals are in `docs/developer-guide/limiter-composition.md`,
which lands with #164.

- **A1. Merge by intersection, along the family tree.** A type a closed
  limiter omits becomes an explicit 0. An explicit family key covers unlisted
  products (`{H100: 8, HBM3: 4}` admits any H100 product up to 8, HBM3 up to
  4). A workload that names the bare family gets the family's summed bound.
  Demand is summed per family key, in the wake check too. A workload with no
  named accelerator fits a closed namespace allowlist only when it lists
  `unknown` or exactly one type.
- **A2. P/D pair commit.** The summed cost of all picked roles is checked
  before committing, and the roles scale down together.
- **A3. Build every declared limiter.** The physical limiter and every quota
  entry are built and grouped. A second physical entry is ignored and logged.
  The installer refuses a `WVA_LIMITER` that names `gpu-inventory` without the
  node read the admin phase grants. At runtime, a physical limiter beside a
  quota whose node read is denied is skipped (`node_read_denied`) and the pod
  stays Ready.
- **A4. One missing input never lifts the others.** A provider that cannot
  answer is skipped, logged by name and counted on
  `wva_constraint_provider_skipped{reason}`, with reason `failed`,
  `unobserved`, `no_constraints` or `node_read_denied`. Only when every
  provider is skipped does a decision go unchecked; scale-from-zero then uses
  its last snapshot if under 5 minutes old, else counts the wake on
  `wva_scale_from_zero_wake_unchecked_total`.
- **A5. A replica must fit a node** while the physical limiter bounds the
  cycle: declared, not skipped, and with a node snapshot under 2 minutes old.
  - Every replica ordered that cycle debits the node view, across models. "No
    node fits a whole replica" is scarcity; a P/D pair needs both roles placed.
  - A refusal by node fit is attributed to `node-fit` (the Event and
    `wva_decisions_limited_total`), not to the pool with the least free.
  - Rescale, the utilization-share idle fill and the warm pool use the same
    view. A node promised to a share receiver is held for it.
  - Placement is best-fit, and honours taints the pod template does not
    tolerate. A wide-EP group is placed within one domain. PR-B adds node
    labels, to match a flavor. CPU and memory are not checked. A replica whose
    pod layout is unknown and larger than every node is judged on the pool only.
  - Pending pods with a `nodeName` or `nominatedNodeName` debit their node.
  - Growth and wakes ordered but not yet placed hold their node holes in an
    in-flight ledger shared by both engines, until their pods are scheduled or
    nominated, for at most 5 minutes. A wake also records its pool take
    and its reclaim there.
- **A6. The warm pool takes the same bounds**, counted in whole units per node
  and published after the optimizer pass. 12 free GPUs as 4+4+4 afford no
  8-GPU pool pod.

## Part B: count Kueue borrowing (PR-B)

### B1. When borrowing is counted

```yaml
limiters:
  - name: gpu
    type: gpu-inventory              # or a GPU ResourceQuota per namespace
  - name: tenant-quota
    type: quota
    scope: namespace
    kueue:
      enabled: true
      # countBorrowable: false        # default true
      # borrowNamespaces: [team-a]    # pilot; empty = every governed namespace
      # assumeTestedVersion: true     # default false; see kueue-version-unknown
    namespaceQuotas:
      default: { H100: -1 }
```

Each namespace shows one reason. When several apply, the first in table order
wins: withheld, then frozen, then counted but 0.

**Withheld**: borrowable is 0 and the namespace is capped at its nominal.

| Borrowing needs | Reason when it fails |
|---|---|
| The namespace is in `borrowNamespaces`, when set | `not-selected` |
| Workloads are read cluster-wide, without error | `lenders-unreadable` |
| Kueue manages every GPU pod in the namespace | `not-kueue-managed` |
| Its ResourceQuotas are readable | `resourcequota-unreadable` |
| A GPU bound is in force | `no-gpu-bound` |
| The controller's version is readable | `kueue-version-unknown` |
| The controller runs a tested version | `kueue-version-untested` |
| The queue is in a cohort | `no-cohort` |
| The queue admits (no `stopPolicy`, its flavors exist) | `inactive` |
| Its borrowing fields parse | `malformed` |
| Every queue in the cohort has reservation status | `no-status` |
| The cohort is flat | `hierarchical-cohort`, `hierarchy-unknown` |
| No queue uses `status.effectiveQuotas` | `effective-quotas` |
| Kueue Fair Sharing is off | `fair-sharing` |
| No GPU flavor has a `topologyName` | `topology-aware` |
| No queue has admission checks (including MultiKueue) | `admission-checks` |
| No queue sets `preemption.borrowWithinCohort` other than `Never` | `borrow-within-cohort` |
| With more than one flavor offering the GPU, every variant pins the flavor's `nodeLabels` | `flavor-unpinned` |
| Every requested resource (CPU, memory, ...) is known from the pod template, and each resource group covering one has a single flavor | `co-resource-bound` |

**Frozen**: nothing new is borrowed, and the last figures are kept (0 when
there are none yet). A freeze never shrinks a running model.

| Borrowing needs | Reason when it fails |
|---|---|
| The snapshot is fresh: the last read succeeded and is under twice `refreshInterval` old | `stale` |
| The controller is up: its leader Lease `renewTime` is under twice `leaseDuration` old, or without a Lease its Deployment has ready replicas | `kueue-controller-down` |

**Counted, but 0 this cycle:**

| Reason | Meaning |
|---|---|
| `lender-cannot-reclaim` | the only unused capacity belongs to lenders Kueue would not reclaim for (B3); the namespace stays at guaranteed |
| `lender-waiting` | a lender waits for capacity it lent; no new borrowing in the cohort (B6) |
| `cohort-pool-empty` | the free pool holds no GPUs, after what is in flight and gated |
| `no-borrow-cap` | `borrowingLimit`, the static entry or the ResourceQuota stops the namespace at guaranteed |
| `preemptions-unreadable` | Kueue's preemptions in the namespace could not be read this cycle |

- **Managed**: a Workload with quota reserved backs the pod, requests its GPUs,
  and is owned by a kind a Kueue integration creates Workloads for. A label,
  queue name or gate is not proof. An unmanaged GPU pod makes the cohort look
  idle while it is not.
- **Forbidden is never absent.** A missing permission shows as the reason of
  the read it blocks: Workloads or LocalQueues not readable cluster-wide
  (re-probed every 5 minutes, so a revoked grant shows) as `lenders-unreadable`,
  an owner kind as `not-kueue-managed`, the Deployment or Lease (never found
  or Forbidden) as `kueue-controller-down`. So a missing Role means
  `kueue-controller-down`.
- **`assumeTestedVersion: true`** applies only to a version that cannot be
  read, and does not lift `kueue-controller-down`. The tested versions are the
  ones the `e2e-tests-kueue-borrow` CI job runs; a pre-release is untested.
- **A stale Lease** much older than the Deployment's last rollout (leader
  election turned off) falls back to the Deployment's ready replicas.
- **A failed Kueue read.** A Kueue-backed quota entry with a last good read
  freezes at that read (`stale`). An entry never read is kept, bounding with
  its finite static caps; its `-1` types bind nothing until a read succeeds.
  It is reported with `wva_kueue_never_read`, and not listed on
  `wva_constraint_provider_skipped` (A4).

### B2. What is read

All read-only; a cohort name with no Cohort object is a flat cohort.

| Object and field | Used for |
|---|---|
| ClusterQueue `spec.cohortName`; Cohort `spec.parentName`, any Cohort naming this one as parent | membership, nested cohorts |
| ClusterQueue `borrowingLimit`, `lendingLimit`, `resourceGroups[].coveredResources` | B3 formula, co-resources |
| ClusterQueue `status.flavorsReservation[].resources[].total`, `.borrowed` | reserved GPUs (Kueue charges at reservation); drift gauge |
| ClusterQueue `spec.preemption.reclaimWithinCohort`, `.borrowWithinCohort` | which lenders count; `borrow-within-cohort` |
| ClusterQueue flavors offering each resource (not `whenCanBorrow`: Kueue v0.20 defaults it to `MayStopSearch` everywhere) | `flavor-unpinned` |
| ClusterQueue `spec.admissionChecks`, `status.fairSharing`, `status.effectiveQuotas` | withheld reasons |
| ResourceFlavor `nodeLabels`, `tolerations`, `topologyName` | node fit per flavor, pinning, `topology-aware` |
| Workloads and LocalQueues, cluster-wide (informer) only for an entry that counts borrowing; Finished Workloads ignored | managed check, priority, lender requests per flavor, gated demand, preemption |
| Namespace `ResourceQuota` `hard`, `used` | GPU bound and ceiling; with several, the tightest |
| Deployment `kueue-controller-manager` image, ready replicas, last rollout; its leader Lease | version; liveness |

### B3. How much a queue can reach

A queue reaches its usage, or the part of nominal it keeps if larger, plus
cohortFree, and never past nominal plus its borrowing limit. Kueue's
[documented example](https://kueue.sigs.k8s.io/docs/concepts/cluster_queue/)
holds: team-a (`nominal 9`, `borrowingLimit 1`) with an idle, reclaiming
team-b (`nominal 12`) reaches 10.

Per flavor and resource, for each cohort member `m`:

```
N_m = nominalQuota      L_m = min(lendingLimit, N_m)   (unset: N_m)
B_m = borrowingLimit    U_m = reserved                  G_m = N_m - L_m

cohortFree = max(0, Σ_{m reclaims} L_m − Σ_m max(0, U_m − G_m))
reach_Q    = min(N_Q + B_Q, max(U_Q, G_Q) + cohortFree)
```

- **Only lenders Kueue would reclaim for count.** `Any` reclaims. `Never`
  does not: serving never finishes like a batch job, so that lender would wait
  indefinitely. `LowerPriority` reclaims for a model only when the lender,
  judged by its lowest-priority unfinished GPU Workload, strictly outranks the
  model's own class priority; a template with no class takes its namespace's
  highest. So such a lender's capacity is offered per model. A pool's
  restricted part stays closed, across cycles, while the cohort holds counted
  borrowing (in flight, or Kueue's `borrowed`) by a model the lender does not
  outrank.
- **Guaranteed.** A queue that can reclaim keeps `N_Q`; growth up to
  `PreemptAfter`, the point beyond which Kueue would have to preempt, needs a
  node hole. The part the queue keeps unused is never debited from cohortFree,
  only the rest; the growth is recorded in the shared record. A whole replica that lies past
  `PreemptAfter` (nothing of it left), covered by peers' actual borrowing
  (Kueue's `borrowed`), skips the node hole. Kueue preempts by priority, not
  by node, so the reclaim may free no whole node: every replica placed without
  a hole is under `placementTimeout`, and if it never schedules it is released,
  backed off and reported `borrow-unplaced`. A queue that cannot
  reclaim has `min(N_Q, max(U_Q, G_Q))`, qualified as `nominal-lent-out` (not
  a withheld reason). Guaranteed is per ClusterQueue.
- **Co-resources.** Every resource group covering a requested resource is
  computed the same way, capped at nominal plus `borrowingLimit`. The reach in
  replicas is the minimum over them of free capacity divided by the variant's
  per-replica request; guaranteed replicas are checked too.
- The static entry and the namespace ResourceQuota cap the reach further.

### B4. Where borrowed capacity is spent

- **Only growth borrows.** The greedy optimizer spends the namespace's
  guaranteed figure, then cohortFree (per cohort, type and flavor, spent once
  across namespaces) up to the borrow cap, a unit at a time. Each draw must
  fit the closed cluster bounds and a node (A5).
- **A P/D pair borrows only whole**, both roles borrowed. A reclaimed decode
  would leave a guaranteed prefill serving nothing. Kueue admits each role on
  its own; the placement timeout releases a half left alone. An odd
  guaranteed remainder is filled by one role alone only beside a partner
  replica that is not borrowed. A refusal that remains is `pd-pair-borrows-whole`, not scarcity.
- **Scale-from-zero, the warm pool and rescale use within nominal**, never
  borrowed: a model woken onto borrowed GPUs can be preempted before it
  serves. Not guaranteed alone either: under Kueue's default `Never`, an idle
  queue's guaranteed is 0. A wake blocked because nominal is lent to a peer
  that need not give it back reports `wake-nominal-lent-out` instead of waking into
  a gated pod. A model whose within-nominal capacity is used cannot wake, even
  with idle lenders. This holds for namespaces read but not counted too. The
  wake deducts borrowing in flight, gated GPUs, GPUs admitted but not yet
  seen by a read, and cohorts closed to new borrowing. Both engines write
  the one borrow record.
- **The utilization-share optimizer** keeps borrowed GPUs a group holds, as no
  debt, and starts no new borrowing.
- **Several controllers in one cohort.** Gated demand is counted from every
  namespace in the cohort's ClusterQueues, so a peer's ask is not borrowed
  twice. Still, run one controller per cohort, or `countBorrowable: false` on
  all but one. Two entries over one pool where only one sets
  `borrowNamespaces` log the warning `borrow-namespaces-split`.

### B5. A borrowed unit must place

Kueue admits on quota, not placement. A borrowed replica is spent only if its
pods fit the node view on its flavor's nodes. A variant with unplaced pods gets
no new borrowed growth until they place.

A borrowed unit, or a replica placed without a node hole (B3), whose pods stay
**unscheduled or Kueue-gated** past the placement timeout is released, and the model's cooldown starts. Pulling an
image or loading weights does not count: a scheduled pod is placed. A cluster
autoscaler `TriggeredScaleUp` pauses the clock, for at most three timeouts.

### B6. Giving capacity back

**A lender is waiting** when a pending Workload in its ClusterQueue requests a
borrowed resource on a flavor it allows, and fits its unused nominal. When the
allowed flavors are unknown it counts on all of them.

- **The first sighting stops new borrowing** in the cohort. The debounce (2
  fresh reads and 60 seconds) applies only to the alert and the cohort
  cooldown.
- **Kueue's preemption gives capacity back**; every counted lender can
  reclaim (with PR-C's opt-in, the scaling manager gives back what lenders
  Kueue does not reclaim for, once the wait is confirmed). A partner or group left without its preempted half is released by
  the placement timeout.
- **Backoff.** After a preemption or a placement-timeout release, the model
  borrows nothing new in that cohort for `borrowCooldown` (default 10
  minutes). A repeat doubles it, up to 4 times (cap 40 minutes); one clean
  cooldown resets it. A placement-timeout release during a preemption's
  cooldown is the same event.
- **Serving under preemption** has prerequisites; they are in the
  [Kueue path](../well-lit-paths/kueue-bounded-quotas/).

## Observability

"Why did my model drop from 6 to 4 replicas?" has four answers: Kueue
preempted pods, a borrowed unit did not place, a replica did not place within
guaranteed, or demand fell. Read these first:

1. `wva_quota_borrow_withheld{reason}`: the active B1 reason.
2. `wva_quota_limit_gpus{tier}`: `guaranteed` and `borrowable`.
3. `wva_constraint_provider_skipped{provider, reason}`: a limiter left out.
4. The log line `Kueue borrowing applied where every condition holds and
   withheld elsewhere`, on change, per namespace and type.
5. `wva-controller-status` `limiterBudgets`: per entry, `guaranteed`,
   `borrowable`, `used`, `reason`, `asOf`.

| Alert | Fires when |
|---|---|
| `WVAConstraintProviderSkipped` | a declared limiter is skipped |
| `WVAKueueReaderStale` | two or more namespaces of one entry are `stale` for 5 minutes, one alert per entry |
| `WVAKueueReaderStaleNamespace` | the same for a namespace that is its entry's only stale one |
| `WVAKueueControllerDown` | borrowing is frozen as `kueue-controller-down` for 5 minutes (warning) |
| `WVAKueueNeverRead` | a Kueue entry with a static `-1` has never read Kueue, for 10 minutes |
| `WVAKueueVersionUnknown` | `wva_kueue_controller_version_known` (version read, or `assumeTestedVersion`; published only while some entry counts borrowing) is 0; quiet while `kueue-controller-down` holds |
| `WVAKueueVersionUntested` | borrowing is withheld as `kueue-version-untested` for 30 minutes (info) |
| `WVAKueueBorrowDrift` | computed borrowed differs from Kueue's for 15 minutes (warning) |
| `WVAKueueLenderWaitingPastCooldown` | a lender waits past `wva_kueue_borrow_cooldown_seconds`, plus, in a cohort that opts in to PR-C, that cohort's `wva_kueue_borrow_release_latency_seconds`, while this controller borrows |
| `WVAKueuePodsUnplaced` | managed pods unplaced past the placement timeout |
| `WVAKueueBorrowThrashing` | more than 3.5 loss events (releases plus preemptions; a pair or group is one) per model per hour; series start at 0 |

**Never-read is not fail-closed.** The other limiters still bound, and Kueue
still gates pods past its quota, so the cost is churn of gated pods, not GPUs
over-allocated. The full signal list, a reason-to-action table and an "ACTION
REQUIRED" upgrade box ship with PR-B in the reference and troubleshooting
pages. Rolling back with `countBorrowable: false` evicts nothing: a lowered cap
only stops growth.

## Shipping

Built as planned: #164 merged Part A and #180 merged PR-B. PR-C is built
on `feat/kueue-borrow-release-v2`.

- **#164** carries Part A. No Kueue code. It enforces from its release.
- **PR-B** is based on #164 and carries Part B and Observability, one commit
  per step: read-only figures, counting with node fit, giving back,
  observability, e2e. There is no separate log-only release: default-on
  counting is gated by the live Kueue e2e passing in CI.
- **The live e2e** runs in its own CI job, `e2e-tests-kueue-borrow`. It
  installs Kueue from `registry.k8s.io`, runs a two-queue cohort, checks drift,
  and scales the Kueue controller to 0 for 60 s to show no growth past
  guaranteed. `REQUIRE_KUEUE_E2E=true` turns its skips into failures.
- **PR-B's developer page** lands with PR-B. **PR-C** was rebuilt on main
  after PR-B (`feat/kueue-borrow-release-v2`), one commit per step: config,
  counting, giving back, observability, e2e, docs.

## Open questions

- **Timeouts.** The cooldown (10 minutes) and the placement timeout (5
  minutes) are guesses; the e2e measures a reclaim cycle and sets them.

## Review

The record is in the comments of [#161](https://github.com/ev-shindin/llm-scaling-manager/pull/161)
and [#162](https://github.com/ev-shindin/llm-scaling-manager/pull/162).

## Appendix: PR-C

With `releaseToNonReclaimingLenders: true`, capacity of lenders that cannot
reclaim is also counted, and handed back when they wait. A waiting lender gets
`ceil(pending / unit)` units at once, bounded by the smaller of its pending
GPUs and what it lent, never below guaranteed or `minReplicas`,
lowest-priority model first, tracked in a versioned release ledger. A unit
larger than the need is still released; for whole-node models the alternative
is releasing nothing. The release lands after KEDA's scale-down window, and the
lender-waiting alert scales with that latency. It makes the scaling manager
Kueue's reclaim agent, which is why it is off by default.

**As built** (operator page:
[lenders Kueue does not reclaim for](../reference/quota-limiter.md#lenders-kueue-does-not-reclaim-for)):

- The release fires once the lender was read waiting twice in a row, or its
  Workload waited over a minute (one read can catch a Workload between
  creation and admission). New borrowing still stops at the first sighting.
- It is sized to what unblocks the lender: its smallest Workload Kueue could
  admit next (queue order; none in requeue backoff, none it admits by
  preempting its own, the head alone under `StrictFIFO`), less its kept
  part, less the pool's free GPUs and what the cycle already frees, less what
  Kueue preempts for it from models it outranks (at the head's priority).
  One budget per pool, so two lenders never count the same GPUs.
- All or nothing: units follow the reclaim yield's rule and its overshoot
  guard (at most one replica past the need), counted on replicas held, never
  on cancelled growth. If this controller cannot cover the need, nothing is
  handed back and the lender is reported (`wva_kueue_lender_release_blocked`).
- A release ends when the lender stops waiting, when Kueue's borrowed figure
  falls by what was handed back, or after twice the cohort's release latency
  (capped at 30 minutes). Landed without either, another borrower took it:
  nothing more goes to that lender while it waits. A P/D pair half landed at
  the timeout keeps its targets. The model's backoff in the cohort starts at
  the landing. The ledger is versioned (1), written only beside a release;
  nothing is released until it is read, and a newer version is held, not
  overwritten.
- Review of #203 found the first cut over-gave: it summed every fitting
  Workload, ignored the free pool and the kept part, counted cancelled growth
  as freed, and fired on one read. The rules above replace it.

## Appendix: decided by the owner

- Borrowing is counted by default: [B1](#b1-when-borrowing-is-counted).
- Only lenders Kueue would reclaim for: [B3](#b3-how-much-a-queue-can-reach);
  the rest is [PR-C](#appendix-pr-c).
- No implicit `gpu-inventory`: [What changes for you](#what-changes-for-you).
- An unknown version withholds unless `assumeTestedVersion`:
  [B1](#b1-when-borrowing-is-counted).
- Never-read is not fail-closed: [Observability](#observability).
- No optimizer tiers: Kueue picks which pod to preempt, and the lever is
  `WorkloadPriorityClass`.
- Within nominal, not borrowed, for wakes, the warm pool, rescale and share
  transfers: [B4](#b4-where-borrowed-capacity-is-spent).
