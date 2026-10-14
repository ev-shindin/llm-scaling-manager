# Kueue borrowing, and limiters that compose

**Status:** approved after five review rounds (revision 5). Implementation in
progress. Revision 5 folds in
four review rounds by five reviewers (see [Review record](#review-record)).
Fixes [#136](https://github.com/ev-shindin/llm-scaling-manager/issues/136),
and the in-repo part of upstream
[llm-d WVA #1003](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1003)
(bounding by `min(physical, quota)`).

## What changes for you

1. **A policy that declares both `gpu-inventory` and `quota` is bounded by
   both.** Today the quota wins and the physical limiter is silently dropped.
2. **Limiter entries that list different GPU types stop allowing a type that
   one entry denies.** Today they allow it. This is a bug fix.
3. **P/D pairs stop ordering both roles past a budget.** Today a budget of 8
   with two 8-GPU roles orders 16 GPUs. This is a bug fix.
4. **A Kueue-bounded quota counts capacity it can borrow from its cohort.**
   This is **on by default**, from the release that also ships the live-Kueue
   end-to-end test. It applies only where it is safe
   ([B1](#b1-when-borrowing-is-counted)). Set `countBorrowable: false` on the
   entry, or `WVA_QUOTA_KUEUE_BORROW=false` at install, to get exactly
   today's Kueue cap. Items 1-3 still apply.
5. **The scaling manager can now scale borrowed replicas down by itself**,
   together with a P/D or LeaderWorkerSet partner when one side of the unit
   borrowed. This happens only when a lender that cannot take its GPUs back is
   waiting for them, or when a borrowed replica cannot be placed. Elsewhere a
   tighter bound still only stops growth.
6. **New RBAC:**
   - `get`/`list` on `cohorts.kueue.x-k8s.io`;
   - `get`/`list` on `resourcequotas` in the managed namespaces.

   A namespace-scoped install needs an admin to re-run `make setup-prereqs`.
7. **Installer changes:**
   - `WVA_LIMITER` accepts a list.
   - With `WVA_QUOTA_KUEUE=true` on a cluster-scoped install, a
     `gpu-inventory` entry is added unless `WVA_LIMITER_PHYSICAL=false`.

## The problem

Today the Kueue quota source caps a namespace at its ClusterQueue's
`nominalQuota`. When another queue in the same cohort is idle, Kueue would
admit more replicas, but the scaling manager never asks for them (#136).

Counting borrowed capacity raises a quota above the guaranteed figure. That is
safe only if something still checks that the GPUs exist and that a replica fits
on a node. Nothing does today, because limiters do not compose. So this
proposal fixes composition first (Part A), then adds borrowing (Part B).

## Can all limiters work together today?

No. Rows marked *measured* were run as throwaway tests against `main`
(39b9d0c8). The rest were read from the code.

| Combination | Works? | What happens | Evidence |
|---|---|---|---|
| `gpu-inventory` alone | yes | | read |
| one `quota` entry | yes | | read |
| `quota` with `kueue.enabled` | yes | the smaller of static and Kueue, inside the entry | read |
| `gpu-inventory` + `quota` | **no** | Quota wins; the physical limiter is never built. Only warnings say so. | read |
| two limiter entries that list **different** types | **no** | The allowed types are the **union**. | measured |
| entries that spell one GPU differently (`H100` vs `NVIDIA-H100-80GB-HBM3`) | **no** | The merge treats them as two types. | measured |
| Kueue borrowing | **no** | Not read (#136). | read |

### Bugs that exist today

**The union leak (measured).** A quota denies every type it does not list. The
merge takes the smallest budget per type, but only over the providers that list
that type. So a type one provider omits passes if any other provider lists it:

| Constraints | Demand | Allowed? |
|---|---|---|
| cluster quota `{H100: 8}` alone | A100 = 2 | no (correct) |
| that quota + a provider listing `{H100: 16, A100: 4}` | A100 = 2 | **yes** |
| namespace quota `x: {H100: 4}` alone | A100 = 2 in `x` | no (correct) |
| that + a second namespace entry `x: {A100: 4}` | A100 = 2 in `x` | **yes** |
| cluster quota `{H100: 8}` + namespace entry `x: {A100: 4}` | A100 = 2 in `x` | **yes** |

The last row is a config that installs today. Limit attribution (which limiter
gets named as the one that capped a decision) disagrees with the merge: in row
2 it names the second provider, not the quota (measured).

**P/D pairs overspend a shared budget (read).** Prefill and decode are each
picked against the same budget, and the budget is reduced only after both are
committed. Every picked role commits at least one replica.

**The optimizer and the wake check disagree on GPU names (measured).** The
greedy optimizer, rescale and the share planner look budgets up by the exact
accelerator name the workload declares. The wake check matches by accelerator
identity. With a physical pool `{H100: 16}`:

- a variant that declares `H100` grows to 3 replicas;
- one that declares `NVIDIA-H100-80GB-HBM3` is held at 1;
- the wake check allows both.

### Gaps that appear once physical and quota compose

Each was measured or read at the function named in
[Details](#details-code-touch-points).

- **A dropped type comes back through the namespace path (measured).** If the
  merge only removes a denied type, the namespace check reads the missing key
  as "no cluster bound", and allows it again.
- **Different spellings of one GPU (measured).** Physical keys are short names.
  Quota keys are whatever the operator wrote.
- **Workloads with no named accelerator skip cluster bounds (measured)** in a
  namespace that has its own cap.
- **One unavailable view lifts every bound (read).** If any provider's usage
  view has not been observed, both engines drop *all* constraints.
- **The warm pool ignores cluster-scope bounds (read).**
- **About ten places read the single limiter mode (read).**

## Part A: limiters compose

**The rule.** A decision must fit **every** declared limiter. A GPU type is
allowed only if every limiter that has a view on it allows it. The budget is
the smallest available figure among them.

A limiter is **closed** when a type it does not list is denied:

- Physical inventory is closed, because an undiscovered type has no GPUs.
- A cluster-scope quota is closed.
- A namespace-scope quota is closed **per namespace**.

A namespace-scope quota's cluster-wide figure is the sum of its namespace
caps. It keeps exactly its role today: it bounds the types it lists and denies
nothing.

### A1. Merge by intersection

- A type that a closed limiter omits becomes an **explicit 0**. The
  catch-all `unknown` pool is exempt, because physical inventory never lists
  it.
- Types are matched by accelerator identity, not by exact string. This is the
  rule the Kueue merge already uses: short names match long names, and two
  different long names never match. The merge emits one key per accelerator.
  Every consumer looks a workload's accelerator up by identity too: the greedy
  optimizer, its debit, rescale and the share planner. A workload is then never
  denied for spelling its GPU differently from the limiter.
- The types considered include namespace cap keys.
- A workload with no named accelerator is checked against namespace caps that
  are already bounded by the cluster caps.
- Limit attribution uses the same rule.
- Everything else keeps today's meaning. That includes how a namespace that is
  excluded from a namespace-scope quota is bounded.

This is a bug fix and ships first. Its tests are the five union-leak rows, the
spelling case (in the merge, in the wake check and in the greedy optimizer),
the namespace re-entry case and the unnamed-accelerator case.
A further test guards against a regression a reviewer measured on an earlier
draft: a namespace excluded from a namespace-scope-only quota must still scale.
Each new test must fail on the parent commit.

### A2. Fix the P/D pair commit

Check the summed cost of all picked roles against the budget before
committing, and scale them down together. This only matters when the roles
share an accelerator type. Keep the existing rule that a role at its own
`maxReplicas` drops out of the pair, which fixed a measured starvation. Test: a
budget of 8 with two 8-GPU roles orders 0, not 8+8.

### A3. Build every declared limiter

- The factory builds the physical limiter, when one is declared, and every
  quota entry, grouped together. Each provider already gets its own usage
  figure. A quota counts only the scaling manager's pods. The physical limiter
  counts every GPU on the nodes.
- The single limiter mode becomes a set. Every reader of the mode is updated,
  including the readiness gate, optimizer selection and the limiter change
  detector.
- Validation allows at most one physical entry, under either spelling
  (`gpu-inventory`, `inventory`).
- The installer:
  - `WVA_LIMITER` takes a list, and the policy helpers stop replacing the whole
    list.
  - A physical entry needs node read access, which only the admin setup phase
    grants. Without it, the installer refuses with that remedy instead of
    installing a controller that goes NotReady.

### A4. One missing input never lifts the others

- A provider that errors, or whose usage view is not yet observed, is skipped
  and logged by name. The other providers still bound the decision, in both
  engines.
- Only when **every** provider is unavailable does a decision go unchecked.
  Scale-from-zero then checks against its last snapshot if that snapshot is
  less than 5 minutes old. Older than that, it wakes unchecked, as it does
  today.
- The warm pool's headroom takes the same merged figures as the optimizer,
  cluster-scope bounds included.

**Upgrade note for Part A.** A policy that declares both kinds is now also
bounded by physical GPUs.

- A tighter physical or quota bound only stops growth. The one exception is
  the release of borrowed units, and their partners, in B5 and B6.
- The physical view counts every GPU-holding pod, training included. When
  training jobs hold GPUs, it can be tighter than the quota.
- For one release before enforcing, the controller logs the old and new
  allocation side by side for such policies.
- The physical limiter walks the pods on GPU nodes every interval.

## Part B: count Kueue borrowing

### B1. When borrowing is counted

```yaml
limiters:
  - name: gpu
    type: gpu-inventory
  - name: tenant-quota
    type: quota
    scope: namespace
    kueue:
      enabled: true
      # countBorrowable: false   # default true; false = exactly today's cap
    namespaceQuotas:
      default: { H100: -1 }
```

"On by default" means *counted where safe*. A namespace's borrowable figure is
0, with a visible reason, unless every condition holds:

| Condition | Reason when it fails |
|---|---|
| Kueue actually manages the model's pods | `not-kueue-managed` |
| A GPU bound is in force | `no-gpu-bound` |
| The Kueue snapshot is fresh, and status has been written | `stale`, `no-status` |
| The cohort is flat | `hierarchical-cohort`, `hierarchy-unknown` |
| No queue in the cohort uses `status.effectiveQuotas` | `effective-quotas` |
| Kueue Fair Sharing is off | `fair-sharing` |
| Its own ResourceQuota and Cohort objects are readable | `resourcequota-unreadable`, `hierarchy-unknown` |

Why each one:

- **Kueue manages the pods.** Every GPU pod in the namespace must carry
  Kueue's `kueue.x-k8s.io/managed` label, or its `kueue.x-k8s.io/admission`
  scheduling gate. Not just the model's own pods: any unmanaged GPU pod in the
  namespace hides GPUs from Kueue's figures too. That records what Kueue did,
  not what was asked of it:
  - a queue-name label alone is not proof, since Kueue may have that
    integration off or not select the namespace;
  - Kueue can also manage unlabelled pods, through defaulting.

  A pod of a Kueue-managed Job or other integration does not carry the label;
  Kueue suspends its owner instead. It counts as managed when its controlling
  owner carries `kueue.x-k8s.io/queue-name` and has a matching Workload.

  Otherwise Kueue's usage misses every serving GPU, the cohort looks idle
  while it is not, and a lender's job is admitted onto GPUs serving already
  holds. A namespace with no GPU pods keeps its previous answer. On the first
  read it counts as not managed.
- **A GPU bound.** Borrowed GPUs belong to another tenant, so something must
  check that they exist. That is the physical limiter, or a GPU
  `ResourceQuota` whose `hard` is at most nominal plus `borrowingLimit`. A
  ResourceQuota of 1000 proves nothing. When `borrowingLimit` is unset, only
  the physical limiter qualifies.
- **Fresh status.** A snapshot is stale when the last read failed or is older
  than twice `refreshInterval`. If the CRDs are installed without a running
  Kueue, status is never written.
- **Flat cohort, no `effectiveQuotas`, no Fair Sharing.** The formula in B3
  does not cover those cases yet (B9).
- **Readable objects.** A Forbidden read is never treated as "no ResourceQuota"
  or "flat cohort".

A namespace `ResourceQuota` also caps the ceiling. Past its `hard` limit, the
API server rejects the pods (`FailedCreate`).

With `countBorrowable: false`, none of Part B applies. The cap is nominal, as
today.

On a cluster-scoped install, `make deploy-wva` with `WVA_QUOTA_KUEUE=true` adds
a `gpu-inventory` entry unless `WVA_LIMITER_PHYSICAL=false`, and prints the
resulting limiter set. A namespace-scoped install cannot list nodes, so it
needs a ResourceQuota to borrow.

### B2. What is read

The reader uses fields already on Kueue's ClusterQueue and Cohort objects:

- borrowing and lending limits;
- reserved GPUs;
- pending workloads;
- reclaim policy;
- Fair Sharing and `effectiveQuotas` status;
- cohort parents.

The field list is in [Details](#details-what-is-read). New RBAC:

- `get`/`list` on Cohort objects;
- `get`/`list` on Kueue Workloads;
- `get`/`list` on the namespace's own ResourceQuotas;
- `get` on the owners of GPU pods, for the managed check.

### B3. How much a queue can reach

**In words.** Each queue keeps part of its nominal quota for itself and lends
the rest. The cohort's free pool is everything lent, minus what members use
beyond the part they keep. A queue can reach its own usage, or its kept part if
that is larger, plus the free pool. It never goes past nominal plus its
borrowing limit. The Kueue reviewer derived this from Kueue's own scheduler
code, and Kueue's [documented example](https://kueue.sigs.k8s.io/docs/concepts/cluster_queue/)
holds: team-a has `nominal 9` and `borrowingLimit 1`, and team-b has
`nominal 12` and is idle, so team-a reaches 10.

Per flavor and GPU resource, for each member `m` of the cohort:

```
N_m = nominalQuota
L_m = min(lendingLimit, N_m)       (unset: N_m)
B_m = borrowingLimit               (unset: no limit)
U_m = reserved GPUs
G_m = N_m - L_m                    kept for m, never lent

cohortFree = max(0, Σ L_m − Σ max(0, U_m − G_m))
```

From these, for this queue `Q`:

- **Guaranteed** is `N_Q` when `Q` can reclaim. When it cannot
  (`reclaimWithinCohort: Never`), guaranteed is `min(N_Q, max(U_Q, G_Q))`:
  what it already uses or keeps. Unused nominal that a peer has borrowed stays
  with the peer. The reason is `nominal-lent-out`.
- **Growth into lent-out nominal draws from the shared pool.** Kueue treats
  usage above `G_Q` as cohort capacity, even below `N_Q`. So growth from
  `max(U_Q, G_Q)` up to `N_Q` is taken from `cohortFree` in the same cycle.
  It does not count against the borrow cap, since it is within nominal.
  - **For a `Never` queue the pool gates this growth.** It can grow only as
    far as the pool has room. Example: a `Never` queue with
    `N = 8, U = 0, G = 0` and `cohortFree = 8`, plus a second borrower, order
    8 GPUs between them, not 16.
  - **For a queue that can reclaim, the pool does not gate it.** That growth is
    guaranteed, because Kueue preempts the borrowers. It still debits the pool,
    clamped at 0, so no other queue borrows the same GPUs in the same cycle.
    Today such a queue can grow to `N_Q`, and it still can.

    It is not subject to the placement-timeout release (B5), since it may be
    waiting on Kueue's reclaim.
- **Borrow cap** is `B_Q`, further bounded by the static entry and the
  namespace ResourceQuota.
- **The free pool** `cohortFree` is shared with every other queue in the
  cohort (B4).

All of this applies only when borrowing is counted. That means
`countBorrowable` is on **and** every B1 condition holds. Otherwise guaranteed
is `N_Q`, exactly as today.

### B4. One shape for borrowing

Every consumer reads the same three figures:

| Figure | Scope | What it is |
|---|---|---|
| guaranteed pool | per namespace and type | the namespace's limit, as today, at the guaranteed figure |
| borrow cap | per namespace and type | how much past guaranteed the namespace may go |
| shared pool | per cohort and type, kept as one chunk per flavor | the cohort's free GPUs, spent once across all namespaces in it |

A namespace that is borrowing uses more than its guaranteed pool. Its pool then
reads 0 available, and its borrowed replicas stay in place. Nothing in Part A
shrinks a model that is over its limit.

The engine, not the reader, takes out of the shared pool the GPUs the scaling
manager has asked for that Kueue has not reserved yet:

- managed pods behind Kueue's admission gate;
- borrowed growth that is not yet visible as pods. It stays deducted until the
  pods exist or the desired count goes back down.

The reader cannot see either.

How each consumer uses it:

- **The greedy optimizer** spends a namespace's guaranteed pool first. It then
  draws from the shared pool, up to the namespace's borrow cap. Every draw must
  also fit the closed cluster bounds and a node (B5). Draws are placed one
  replica at a time into a flavor chunk. A P/D pair is placed as one unit.
- **Rescale, the warm pool and scale-from-zero** see the guaranteed pool only.
- **The utilization-share optimizer** budgets guaranteed plus the borrowed GPUs
  each group holds right now, frozen for the cycle. Held borrowed GPUs are not
  debt. It starts no new borrowing. Under it, borrowed capacity is held but not
  grown in this proposal; a follow-up can add it.

A namespace-scoped controller sees only its own namespace. Two such
controllers in one cohort can both claim the shared pool. The ResourceQuota
cap (B1) bounds the damage, and the docs say so.

### B5. A borrowed replica must fit on nodes

Kueue admits on quota, not placement, unless Topology-Aware Scheduling is on.
Ten free GPUs spread over two nodes cannot hold one 8-GPU replica.

- **Node fit, when the physical limiter is present.** It already walks the
  pods on GPU nodes, so it reports free GPUs per schedulable node. A borrowed
  replica is spent only if its pods fit there, first-fit:
  - **Its flavor's nodes only.** A replica uses one flavor, and Kueue pins it
    to that flavor's node labels and tolerations. The reader already lists
    ResourceFlavors and carries their full `nodeLabels` on each shared-pool
    chunk. A flavor with no `nodeLabels` matches every GPU node.
  - **A 2x8 group needs two nodes.**
  - **Every replica ordered this cycle is debited from the node view**,
    guaranteed and borrowed, across all models. A guaranteed replica that fits
    no node debits nothing. It stays Pending and is reported as unplaced.
  - **One node view per cycle, used in order.** The greedy optimizer places
    first. The share planner's idle fill reads the same view afterwards, with
    greedy's debits applied, so the two cannot place into the same hole.

  The view counts GPUs only. CPU or memory exhaustion on a node is caught by
  the placement hold and timeout.
- **Release what cannot place.** A borrowed unit whose pods stay unschedulable
  or Kueue-gated for longer than the placement timeout is released:
  - the unit is a whole replica, a P/D pair at the pair's ratio including the
    partner that did place, or a whole LeaderWorkerSet group;
  - the model's borrowing cooldown starts;
  - the timeout defaults to 5 minutes and is configurable;
  - while a pod has a cluster-autoscaler `TriggeredScaleUp` event, the timeout
    waits, since a new 8-GPU node often takes longer than 5 minutes. The wait
    is capped at three times the timeout, and ends at once on a later
    `NotTriggerScaleUp` event. A scale-up that is never triggered does not
    pause the timeout.

  Without a node view this is the only protection, and the docs say so.
- **Placement hold.** A variant and role with *unplaced* pods gets no new
  borrowed growth until they place. Unplaced means `PodScheduled=False` with
  reason `Unschedulable`, or a Kueue gate older than one cycle. Pulling an image
  or loading weights does not count as unplaced. The count is a new field
  beside today's pending-replica count, from the same pod informer. For a
  LeaderWorkerSet the hold lifts only when the whole group has placed.

### B6. Giving capacity back

Inference Deployments never finish. A lender whose queue cannot reclaim
(`reclaimWithinCohort: Never`) gets its GPUs back only if the borrower hands
them back.

**A lender is waiting** when all of these hold, for the flavor and GPU resource
being borrowed:

- it has pending workloads;
- its reservation is below its nominal;
- both have held for 2 consecutive fresh reads and at least 60 seconds.

Without the second check, a stuck CPU job, or a borrower's own preempted pod,
would pin the whole cohort.

When a lender is waiting:

- **If the lender can reclaim**, no new borrowing starts, and Kueue preempts.
- **If it cannot**, the steady-state engine releases borrowed units, one per
  cycle, while the lender is still waiting:
  - **Units:** a whole replica, a P/D pair at the pair's ratio, or a whole
    LeaderWorkerSet group. A unit larger than the lender's need is still
    released. For whole-node models the alternative is releasing nothing. A
    pair of 8+8 GPUs against a 4-GPU shortfall releases the pair.
  - **Bound:** add up the lent-out nominal of every waiting lender on that
    flavor, and round up to a whole unit. The total released stays within
    that. Gated borrowed pods hold no reservation, so they do not count. A
    release never goes below a namespace's guaranteed figure or a variant's
    `minReplicas`.
  - **A release in flight blocks the next one.** A lowered target frees nothing
    until KEDA's scale-down window passes and the pods are deleted. Until then,
    the lender still reads as waiting. So a released unit counts as in flight
    until two things have happened:
    - its pods are gone (every pod, for a group);
    - a fresh read of the lender's reservation postdates their deletion.

    No further unit is released for that lender while one is in flight. This
    holds across a controller restart. Each waiting lender has its own entry,
    so two lenders can each have a unit in flight; the summed bound keeps that
    safe. A unit still in flight after twice the grace period plus KEDA's
    window, because the target was never actuated or a pod is stuck, keeps
    blocking. It surfaces as reason `release-not-actuated`.
  - **It overrides** the analyzer's demand floor and hold, which would
    otherwise raise the target again next cycle.
  - **Order:** lowest-priority model first, then the most recently grown. After
    a controller restart, "recently grown" is not known, so the order is
    priority, then cost.
  - Each release is logged with the lender's name, and shown as an event.
  - KEDA's scale-down window, up to 300 s, comes on top. Hand-back time is
    roughly refresh plus one cycle plus that window, and the docs say so.

**Cooldowns:**

- After a lender waits, the cohort borrows nothing new for the cooldown.
- After Kueue preempts one of a model's borrowed pods, that model borrows
  nothing new for the cooldown.
- The default is 10 minutes, about three times the start time of a
  whole-node replica. It is replaced by a value measured in the e2e.

**Stale reads freeze, never shrink.** A stale snapshot stops new borrowing and
keeps every figure where it was, the release ledger included. An API blip must
not cause a reclaim the cluster did not ask for. A Kueue controller that is down while its status stays
readable is not detected. The placement hold and the placement timeout bound
the damage. A stale reader also freezes the release for a waiting lender. The
stale alert covers that, and the runbook says to hand capacity back by hand
while the reader is stale.

**Pairs that lose one side.** Kueue preempts per workload. A prefill or decode
pod is one workload, and a LeaderWorkerSet group is one workload. When one
role's borrowed pods stay gated past the placement timeout after a preemption,
the matching partner replica is released too, so the partner does not hold GPUs
that produce nothing. A borrowed partner is preferred, and a guaranteed one is
released only if no borrowed one exists. A LeaderWorkerSet scales down its
highest group index; this is LeaderWorkerSet's behaviour, not Kueue's. That is
normally the newest group, the one that borrowed. The release checks this, and
logs it when it is not.

**What preemption does to a serving pod.** Kueue deletes the pod. It does not
use the Eviction API, so PodDisruptionBudgets do not apply, but the termination
grace period does. The ReplicaSet recreates the pod gated. The
[Kueue path](../well-lit-paths/kueue-bounded-quotas/) therefore requires:

- a preStop drain hook on serving pods;
- a `WorkloadPriorityClass` above batch for models that must keep replicas;
- the group integration for multi-pod replicas;
- no `borrowWithinCohort` preemption on serving queues, so serving never
  preempts a lender's training.

### B7. Where borrowed capacity may be spent

- **Scale-from-zero, the warm pool and rescale** use capacity **within
  nominal** only, never borrowed capacity. A model woken onto borrowed capacity
  can be preempted before it serves.
  - For a queue that can reclaim, within nominal is `N_Q`.
  - For a `Never` queue, it is `min(N_Q, max(U_Q, G_Q) + cohortFree)`, which is
    what Kueue would admit right now without borrowing.

  This is not the guaranteed figure. Under Kueue's default (`Never`), an idle
  queue's guaranteed figure is 0, and using it here would stop every idle
  namespace on a default Kueue setup from waking. Today it can wake, so it must
  still be able to. Step 4's implementation found this.
- **Growth** may spend borrowed capacity, through the greedy optimizer, under B4
  and B5.

### B8. What does not change

- Borrowing moves capacity between queues and creates none. So the
  cluster-scope quota figure, the sum of nominal quotas, stays as it is.
- Shared ClusterQueues are still a per-namespace ceiling, not a partition.

### B9. Not covered yet

Each of these shows its reason, and borrowable is 0:

- **Fair Sharing.** It is enabled controller-wide and seen through
  `status.fairSharing`. Kueue then admits borrowers by share, and other
  borrowers compete too. A follow-up can cap borrowing at the queue's
  fair-share entitlement.
- **Nested cohorts.** These are a cohort with a parent, with its own
  `resourceGroups`, or **named as a parent by any other Cohort**. A root can
  look flat while its children lend into it.
- **`effectiveQuotas`.** When present, Kueue schedules from it instead of
  `spec`.
- **Other resources in the flavor.** A flavor must satisfy every resource it
  covers. Borrowed GPUs with exhausted CPU in the same flavor leave pods gated.
  The placement hold and timeout handle it.

## Part C: what on-call sees

The question to answer is "why did my model drop from 6 to 4 replicas?" There
are four causes:

- Kueue preempted pods.
- The scaling manager released borrowed replicas.
- A replica did not place.
- Demand fell.

| Signal | What it adds |
|---|---|
| Log | The `quota entry bounded by external caps` line adds `guaranteed`, `borrowable`, Kueue's own `borrowed` figure as a cross-check, and the reason when borrowable is 0. |
| `wva_quota_limit_gpus{limiter_name, exported_namespace, accelerator, tier}` | `tier` is `guaranteed` or `borrowable` |
| `wva_quota_borrow_withheld{…, reason}` | 1 for the active reason |
| `wva_model_scaling_blocked` reasons | `borrow-released`, `lender-reclaimed`, `borrow-unplaced`, `nominal-lent-out` |
| Event on the scale target | when the scaling manager releases borrowed replicas, naming the lender ClusterQueue |
| Gauges | managed pods unplaced for longer than the placement timeout; managed pods Kueue preempted (counter) |
| `wva-controller-status` | `limiterBudgets`: per entry, namespace and type, `guaranteed`, `borrowable`, `used`, `reason`, `asOf`. The engine publishes these through a snapshot, as it already does for decisions. |

The Kueue path ships a "replicas dropped" decision tree over the four causes. It
also ships alerts for:

- the reader stale for more than 5 minutes;
- borrowing above 0 while a lender has been waiting longer than the cooldown;
- managed pods unplaced longer than the placement timeout;
- ResourceQuota `FailedCreate`;
- thrash: more than 3 `borrow-released` plus `lender-reclaimed` per model per
  hour.

## Decided

- **Borrowing is on by default** (owner's decision).
  - It turns on in the release that ships the live-Kueue e2e. The release
    before ships the figure as a log only.
  - The B1 conditions make default-on safe where Kueue does not govern the
    pods.
  - `countBorrowable: false` restores exactly today's cap, including the
    nominal floor.
- **No optimizer tiers.** The issue asks the optimizer to release borrowed
  replicas first. Kueue picks which pod to preempt, and a ReplicaSet picks
  which pod to delete on scale-down. The one release the scaling manager does
  own (B6) has an explicit order. For everything else the lever is Kueue's
  `WorkloadPriorityClass`.
- **Guaranteed capacity only** for scale-from-zero, the warm pool, rescale and
  new share transfers.

## Order of work

Each step is one PR. Its new tests must fail on the parent commit.

1. **A1, the merge fix.** No config change.
2. **A2, the P/D pair commit.**
3. **A3 and A4, composition.** Includes the installer, RBAC fixtures, the
   readiness gate, the side-by-side upgrade log, and the docs.
4. **Read-only borrowing.** The reader parses everything in B2 (new RBAC) and
   logs the figures it *would* apply, with reasons. Ships one release before
   step 5.
5. **Borrowing on by default, together with the live-Kueue e2e.** This covers
   B1 to B7 and Part C. The e2e installs the Kueue controller from
   `registry.k8s.io` and runs a two-queue cohort. It covers:
   - a borrowed replica preempted, then replicas lowered, with no gated pods
     left over;
   - a `Never` lender waiting, then a released pair, and the lender's own
     workload admitted (Kueue prefers entries that fit without borrowing).

   Unit tests cover:
   - the clamps;
   - the shared pool with two borrowers;
   - node fit with 5 + 5 free GPUs over two nodes against one 8-GPU replica;
   - the placement timeout;
   - the guaranteed-part merge (quota 12 with 4 borrowable, plus physical 10,
     gives 8 guaranteed);
   - the stale freeze;
   - the debounced lender rule;
   - one release in flight at a time (short 4, lent out 32, slow KEDA window:
     exactly one unit released);
   - a reclaiming queue whose nominal is all lent out still grows to nominal;
   - the managed check for Job-owned pods and for LeaderWorkerSet pods;
   - pair-ratio release, including an 8+8 pair against a 4-GPU shortfall
     (releases the pair, not nothing);
   - the timeout release taking the placed partner;
   - a `Never` queue and a borrower sharing 8 free GPUs (8 ordered, not 16);
   - the release overriding the demand floor;
   - share budget with borrowed GPUs held, which must create no debt;
   - v1beta1 against v1beta2.
6. **Follow-ups:** nested cohorts, Fair Sharing, `effectiveQuotas`, borrowing
   under the utilization-share optimizer, Topology-Aware Scheduling.

## Open questions

- **Scale-from-zero into borrowed capacity.** A model whose guaranteed pool is
  fully used cannot wake, even with idle lenders. A per-model `allowBorrowed`
  would need node fit too.
- **Timeouts.** The cooldown (10 minutes) and the placement timeout (5
  minutes) are guesses. The e2e measures a reclaim cycle and a whole-node
  start time, and both defaults, and the alerts, are set from that.

## Details: what is read

| Field | Used for |
|---|---|
| ClusterQueue `spec.cohortName` (`spec.cohort` in deprecated v1beta1) | cohort membership |
| `…resources[].borrowingLimit`, `…lendingLimit` | B3 |
| `status.flavorsReservation[].resources[].total` | reserved GPUs. Kueue charges quota at reservation; `flavorsUsage` lags it when admission checks are configured. |
| `status.flavorsReservation[].resources[].borrowed` | cross-check, logged |
| `status.pendingWorkloads` | lender rule (B6) |
| `spec.preemption.reclaimWithinCohort` | guaranteed figure (B3), lender rule (B6) |
| `status.fairSharing`, `status.effectiveQuotas` | B1 conditions |
| Cohort `spec.parentName`, `spec.resourceGroups` | nested cohorts, including any Cohort that names this one as its parent |
| Namespace `ResourceQuota` (GPU `hard`, `used`) | GPU bound and ceiling (B1) |
| Pod labels `kueue.x-k8s.io/managed`, scheduling gate `kueue.x-k8s.io/admission` | Kueue-managed check (B1), unreserved requests (B4), placement hold (B5) |

The reader resolves the version through the RESTMapper, which prefers the
served version with the highest discovery priority (v1beta2 today). A cohort
name with no Cohort object is a flat cohort.

## Details: code touch points

For implementers. Paths are under `internal/` unless noted.

- **Merge and attribution** (`engines/allocation/`):
  - `cost_aware_optimizer.go`: `mergeConstraints`, `mergeNamespaceConstraints`;
  - `gpu_limit_attribution.go`: `bindingProvider`, which skips unlisted
    cluster types, and `namespaceHeadroom`, which reads namespace pools only;
  - `gpu_budget.go`: `FitsGPUBudget`, whose unnamed-accelerator branch and
    open-path deny on a missing key both matter, and `GPUBudgets`;
  - `greedy_score_optimizer.go`: `effectiveAvailable`, `fairShareRolePick`;
  - `utilization_share_groups.go`: `BuildShareGroups`. Its `PhysicalFree` is
    renamed `ClusterFree`, the minimum over closed cluster pools, since under a
    composed set it is no longer physical.
  - `multi_backup/` is `//go:build ignore` and is not touched.
- **P/D pair, and shared-pool draws:** `engines/allocation/analyzer_helpers.go`
  `allocateForModelPaired`. Shared-pool draws need a per-replica first-fit
  there, not the `gpusAvail / gpusPR` arithmetic.
- **Exact-string accelerator lookups** to route through `accelerator.FindKey`:
  - `fairShareRolePick` and `effectiveAvailable` (greedy);
  - the `accFromVCs` debit (`analyzer_helpers.go`);
  - the accelerator match in `rescale.go`;
  - `BuildShareGroups`.
- **Placement timeout** and the share optimizer's `HoldFill` timeout share one
  setting, so the two cannot drift apart. Tuning it for cluster-autoscaler
  latency retunes both, and the configuration reference says so.
- **Readers of the single mode:**
  - `config/config.go`: `limiterModeOf`, `EffectiveLimiterSummary`,
    `UnenforcedLimiterTypes`;
  - `config/saturation_scaling.go`: validation, both aliases;
  - `engines/allocation/limiter_factory.go`: `LimiterSignature`, which encodes
    only quota entries today;
  - `engines/allocation/usage_basis.go`: `PhysicalUsageConfigured`;
  - `cmd/main.go`: the readiness `gpu-policy` gate, and the `LimitedMode` and
    unenforced warnings;
  - `scalingpolicy/inventory.go`: `ShouldCollectClusterInventory`;
  - `engines/steadystate/engine.go`: optimizer selection, the `MissingBasis`
    fallback, `ReportUnresolvedAccelerator`;
  - `engines/steadystate/engine_v2.go`: idle-fleet headroom;
  - `controller/configmap_reconciler.go`, `controller/configmap_helpers.go`;
  - `engines/allocation/utilization_share_shadow.go`: `ClusterIsQuota`;
  - `engines/scalefromzero/candidates.go`: drop-all on error, and a stale doc
    comment on the limiter mode.
- **Installer:**
  - `deploy/lib/cli.sh` (`WVA_LIMITER`), `deploy/lib/limiter_policy.sh`,
    `deploy/lib/physical_limiter.sh`;
  - fixtures in `hack/check-limiter-declaration.sh`,
    `hack/check-physical-limiter-targets.sh`, and the RBAC checks for
    `resourcequotas` and `cohorts`.
- **Kueue figures:**
  - `kueue/reader.go`;
  - `config/quota_limiter.go`: `ExternalCaps` and `BoundBy` carry guaranteed
    and borrow cap per namespace, and the shared pools per cohort;
  - `engines/allocation/quota_inventory.go`: pools and the `Refresh`
    fingerprint.
  - `ResourceConstraints` gains the borrow caps, the shared pools and a
    namespace-to-cohort map.
  - The guaranteed-only copy drops those three and follows `WithholdPromised`
    in `utilization_share_withhold.go`.
- **Node view:** the physical limiter's usage walk (`gpuusage`) reports free
  GPUs per schedulable node. `decision.NodeGPU` gains the node's taints, so a
  flavor's tolerations can be matched.
- **Placement hold:** a new unplaced-replica count beside `PendingReplicas`
  (`domain/saturation_analyzer.go`), from the same pod informer.
- **Release ledger:** one entry per lender and flavor. It resets when the lender
  stops waiting, and is persisted in `wva-controller-status` beside
  `limiterBudgets` so it survives a restart. Events are best effort and expire,
  so they are not the source. With no persisted entry, a waiting lender is
  treated as in flight until the next fresh read.
- **Status:** `controllerstatus/publisher.go` reads only config today.
- **Tests that encode single-mode behaviour:** `limiter_factory_test.go`,
  `engine_limiter_reload_test.go`,
  `TestPhysicalUsageConfiguredMatchesTheBuiltLimiter`.
- **Docs to update in step 3** (all under `docs/`):
  - `reference/quota-limiter.md`, `reference/gpu-limiter.md`,
    `reference/scaling-policy.md`, `reference/configuration.md`,
    `reference/metrics.md`;
  - `concepts/gpu-capacity-accounting.md`;
  - `guides/admin-gpu-bounding/`, `well-lit-paths/bound-by-gpus/`;
  - plus `config/base/manager/scaling-policy-configmap.yaml`.

## Review record

| Reviewer | Round 1 | Round 2 | Main round-2 findings, folded into revision 3 |
|---|---|---|---|
| Kueue expert | request changes | request changes | roots with child cohorts look flat; guaranteed under `Never`; lender rule per flavor; managed-pod detection |
| Maintainer | request changes | request changes | dropping the namespace aggregate stops excluded namespaces (measured); borrowing had three shapes; ownership of in-flight GPUs and release |
| SRE / cluster admin | request changes | request changes | queue-name label is not proof; below-nominal cap must not leak into the opt-out; release needs debounce and flavor match |
| Whole-node / P/D | request changes | request changes | node fit before spending; pair-aware release and repair; share budget with held borrowed GPUs |
| Docs | approve with changes | request changes | opt-out promise; four missing "what changes" items; internals to Details |

Round 3 (revision 3): docs and SRE approved. Kueue expert, maintainer and
whole-node each asked for one fix, folded into revision 4:

- lent-out nominal and the shared pool counted the same GPUs;
- the optimizer looks up GPU names by exact string;
- the timeout release split pairs, and a release smaller than one unit
  released nothing.

Their minors are folded in too.

Round 4 (revision 4): docs, SRE, Kueue expert and whole-node approved. The
maintainer asked for one fix: a release still in flight did not stop the next
one, so KEDA's scale-down window could let many units go for one small
shortfall. It is folded into revision 5 with every round-4 minor.

Round 5 (revision 5): all five approved, with no blocker or major. Their minors
are folded into this text. Implementation starts with step 1.

Amended during implementation (step 4): B7 uses capacity within nominal, not
the guaranteed figure, for scale-from-zero, the warm pool and rescale. B1 now
defines "stale".
