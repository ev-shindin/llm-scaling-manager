# Bound tenants by the quotas Kueue already holds

## Start here

- **Status:** Experimental. Cohort borrowing is **on by default** ([Borrowing](#borrowing)). A live-Kueue e2e covers borrowing, a release to a `Never` lender and recovery after preemption; passed locally on kind, not yet run in CI, so the path stays Experimental ([How it is tested](#how-it-is-tested)).
- **You need:**
  - everything [the tenant-quotas path](../tenant-gpu-quotas/#start-here) needs, including its Namespace-read note for the cluster policy on Kubernetes.
  - Kueue's `ClusterQueue`, `LocalQueue` and `ResourceFlavor` served, with LocalQueues in the managed namespaces.
  - for a namespace-scoped install, the `kueue-reader` prerequisite from `make setup-prereqs` (cluster admin; [What it needs](#what-it-needs)). Set up before this release? The admin re-runs it once ([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-to-this-release)).
  - ClusterQueues whose `namespaceSelector` admits only the namespaces meant to have them ([why](#what-it-is)).
  - **Not enforcement:** the limiter is advisory; back each tenant namespace with a GPU `ResourceQuota` ([bound at admission](../../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).
  - **For borrowing** (on by default): the [Borrowing](#borrowing) checklist. Without a GPU bound or Kueue-managed pods, borrowing is not counted.
- **Do this** (no guide covers this path yet; the steps are on this page):
  1. Check accelerators resolve: `kubectl get events -n <ns> --field-selector reason=AcceleratorNotResolved` (namespace owner; a cluster admin can use `-A`). No events is the healthy answer.
  2. Declare the quota once, with the types at `-1` so Kueue sets the numbers ([Declaring it](#declaring-it)), one of:
     - Namespace owner (advisory; you can edit it): `WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=-1' WVA_QUOTA_KUEUE=true make deploy-wva`.
     - Cluster admin (a budget the tenant cannot edit): `make enable-physical-limiter WVA_LIMITER_TYPE=quota WVA_QUOTAS='<accelerator>=-1' WVA_QUOTA_KUEUE=true`.
- **Check it works:** the controller log has `quota entry bounded by external caps` and `Kueue borrowing applied` ([Verifying it](#verifying-it)). `external quota source unreadable` means missing `kueue-reader` RBAC; `grants no GPUs` means no LocalQueue in the namespace reaches a GPU ClusterQueue.
- **Turn borrowing off, keep the Kueue cap:** add `WVA_QUOTA_KUEUE_BORROW=false` to the same command as `WVA_QUOTA_KUEUE=true`. It writes `countBorrowable: false` on the entry ([Borrowing](#borrowing)).
- **Undo** (no guide step). Left at `-1` without Kueue, the entry is no cap at all, so replace it rather than only dropping the flag:
  - One install (namespace owner): re-run `make deploy-wva` with `WVA_LIMITER=quota`, a finite `WVA_QUOTAS` and no `WVA_QUOTA_KUEUE`, or with no `WVA_LIMITER` to declare no limiter.
  - Cluster policy (cluster admin): re-run `make enable-physical-limiter WVA_LIMITER_TYPE=quota` with a finite `WVA_QUOTAS` and no `WVA_QUOTA_KUEUE`, or `make disable-physical-limiter WVA_LIMITER_TYPE=quota` to remove the quota. Without `WVA_LIMITER_TYPE=quota` it removes only the physical limiter.

## What it is

> **Experimental.** The merge rules, the reader, borrowing and giving it back
> are unit tested. One end-to-end spec proves the read-and-merge chain on
> Kueue's CRDs alone. A live-Kueue e2e covers borrowing, a release to a
> `Never` lender and recovery after preemption; passed locally on kind, not
> yet run in CI, so the path stays Experimental. Defaults that may still move: the 30s `refreshInterval`,
> how an untyped grant is applied, and the borrowing timings (`placementTimeout`
> 5m, `borrowCooldown` 10m), which the live suite is meant to measure.

If Kueue is already the place where GPU quotas are decided, a second copy of
every number in the scaling manager's ConfigMap is a second thing to keep right. On
this path the scaling manager reads Kueue's ClusterQueues and enforces, per namespace and accelerator type,
**the smaller of Kueue's grant and the static cap** — so the autoscaler does not
ask KEDA for a replica that Kueue would then hold pending. Where it is safe, the
grant includes what the namespace's ClusterQueue can **borrow** from its cohort,
and the scaling manager gives borrowed replicas back when a lender needs them.

It is an extension of [capping what each tenant may take](../tenant-gpu-quotas/):
everything there about scope, the reserved `default` key and what a quota does
*not* bound still applies. Read that page first; this one adds one block to its
entry.

**Use it when** the namespaces the scaling manager scales have LocalQueues, their GPU grants are
ClusterQueue nominal quotas, and you want the scaling manager to follow those numbers as they
change rather than mirror them by hand.

**Do not use it where several namespaces share one ClusterQueue and the shared
total is what must hold**: each such namespace is capped at the queue's full
grant (the second surprise below). And read the third surprise before choosing
`-1` as the static cap: it decides what happens when Kueue cannot be read.

**Check what your ClusterQueues admit where tenants can create their own
LocalQueues** and the static cap is `-1`. The controller grants a namespace the
quota of a ClusterQueue one of its LocalQueues points at only when that queue's
`namespaceSelector` admits the namespace, with Kueue's semantics: no selector
admits nothing, `{}` admits every namespace, anything else is matched against
the namespace's labels — and a namespace whose labels the controller cannot
read gets nothing from a selector-bearing queue. What remains open is a queue
whose selector is `{}`, or one that matches a label the tenant can set: a tenant
who points a LocalQueue at it gets its grant as the controller sees it, as
Kueue would admit their work there. Keep such queues out of tenants' reach (or
LocalQueue creation with the admin: check whether your Kueue install aggregates
LocalQueue rights into the namespace `admin` role with
`kubectl auth can-i create localqueues.kueue.x-k8s.io -n <ns> --as=<tenant>`),
or keep a finite static cap, which Kueue can only tighten.

## What it needs

- Everything [tenant-gpu-quotas](../tenant-gpu-quotas/) needs.
- Kueue's `ClusterQueue`, `LocalQueue` and `ResourceFlavor` kinds served by the
  cluster. The scaling manager lists them and never talks to Kueue's controller, so the CRDs
  alone are enough for the reader to work — but a real installation is what
  makes the grants mean anything.
- RBAC to read them. A **cluster-scoped** install has it in the manager
  ClusterRole. A **namespace-scoped** install reads LocalQueues in the namespace
  it manages through its Role, and needs the two cluster-scoped kinds, plus
  `get` on its managed Namespace (to evaluate a `namespaceSelector`), from the
  `kueue-reader` prerequisite, which the admin phase creates
  (`make setup-prereqs SCOPE=namespace ENVIRONMENT=kubernetes`, the same run
  that creates `node-reader`). An install whose `kueue-reader` predates the
  Namespace read does **not** upgrade with this block on: when the controller
  cannot `get` its managed Namespace, the tenant's `make deploy-wva` refuses
  and names the `make setup-prereqs` run a cluster admin makes first. Without
  the ClusterQueue and ResourceFlavor read, the reader logs
  `external quota source unreadable` each cycle.

## Declaring it

The installer writes it. Add `WVA_QUOTA_KUEUE=true` to the same `WVA_LIMITER=quota`
install the tenant-quotas path uses, and put the accelerator types in
`WVA_QUOTAS`:

```bash
# one install's own policy
WVA_LIMITER=quota WVA_QUOTAS='H100=-1' WVA_QUOTA_KUEUE=true make deploy-wva ...

# the cluster policy (see admin-gpu-bounding for which controllers read it). With WVA_QUOTA_KUEUE=true this also
# grants each target controller the cluster-scoped half of the Kueue read
# (ClusterQueues, ResourceFlavors, and get on its managed Namespace only), as it
# grants the node read for gpu-inventory.
WVA_LIMITER_TYPE=quota WVA_QUOTAS='H100=-1' WVA_QUOTA_KUEUE=true make enable-physical-limiter
```

On a cluster-scoped install, `make deploy-wva` with `WVA_QUOTA_KUEUE=true` also
declares a `gpu-inventory` limiter beside the quota, so scaling must fit the GPUs
that exist as well as the Kueue quota. It prints the resulting limiter set. Pass
`WVA_LIMITER_PHYSICAL=false` to leave it out. The physical limiter needs the
controller to list nodes; without that read the install refuses and says to run
`make setup-prereqs`. A namespace-scoped install never adds it.

The LocalQueue read is in the controller's Role (`wva-manager-role`), which is an
admin-phase object: the tenant's `make deploy-wva` does not apply Roles. So a
namespace-scoped controller installed before this feature existed needs **a
cluster admin to re-run `make setup-prereqs NAMESPACE=<ns>`** (with the same
`WVA_NS` / `WVA_WATCH_NS` as the original install) once; until then it logs
`external quota source unreadable` and runs on the static entry.

That renders as one more block on the quota entry (comments added here; the
installer writes the two lines under `kueue:`):

```yaml
limiters:
  - name: install-quota
    type: quota
    scope: namespace
    kueue:
      enabled: true
    namespaceQuotas:
      <managed namespace>:
        H100: -1                    # the TYPE is yours; the NUMBER is Kueue's
```

Hand-write the entry when you want several namespaces named, or a finite static
cap for some of them: wherever both sides name a type, the smaller figure wins.

A namespace is **governed** by Kueue when one of its LocalQueues reaches a
ClusterQueue that declares a GPU resource; it then gets that queue's grant. A
namespace with no such LocalQueue is **not governed**, and the static entry
alone decides for it. What each Kueue object becomes, and what is not read:
[the reference](../../reference/quota-limiter.md#what-is-read).

## Three things that surprise people

**Kueue's usual flavors name no accelerator type, so Kueue alone cannot open one.**
The quickstart's `default-flavor` has no node labels. Its grant is *untyped*: it
bounds every type the static entry names and, on its own, names none. Turn the
block on with **no static map** and untyped flavors, and the entry — now an
empty map — **denies every type**; the limiter says so once in its log
(`grants GPUs of no named accelerator type`, with the fix in the message). Hence
the recipe above: name the types, let Kueue give the numbers (`H100: -1`). Give
the flavors a product label (`nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3`)
and the grant becomes typed — and then a per-flavor split (PCIe 4, SXM 0) is
honoured exactly. The cost of the untyped shape: a namespace that names two
types may reach the untyped grant on *each*; the limiter has no per-namespace
total.

**A shared ClusterQueue is a ceiling per namespace, not a partition.** Each
namespace with a LocalQueue on it is capped at the queue's full grant, so three
of them may in sum still ask for three times what the queue admits — Kueue holds
the excess pending, as before. Give each tenant its own ClusterQueue, with a
cohort for borrowing, where the shared total matters.

**A Kueue outage keeps the last grant; a Kueue that was never read leaves only
the static entry.** A failed read keeps the last good snapshot in force, and the
log says how old it is. Before the *first* successful read — RBAC missing, CRDs
absent, a fresh controller — the static entry alone applies, and with `-1` that
is **no cap at all**. If a controller must stay bounded when Kueue cannot be
read, put a finite number in the static entry; Kueue then only tightens it. A
missing CRD or a Forbidden is reported as such every cycle, never folded into
"Kueue grants nothing"; both engines treat a limiter error as "no constraint",
so `Refresh` never surfaces one and the reader's errors go to the log.

## Borrowing

With `kueue.enabled` on a namespace-scope entry, a model may grow past its
namespace's own quota into capacity its cohort lends. This is **on by
default**. It is counted per namespace only where it is safe; elsewhere the
namespace caps at nominal and the log and `wva_quota_borrow_withheld` say why
([the conditions](../../reference/quota-limiter.md#cohort-borrowing)).

**Take it when** serving shares a cohort with batch work that leaves GPUs idle,
and a model can tolerate losing a borrowed replica when the lender wants it
back. **Do not take it** for a model that must never lose a replica: give it
the quota as nominal instead, or turn borrowing off.

Before relying on it:

- [ ] A drain hook on serving pods: Kueue deletes a preempted pod without the
  Eviction API, so a PodDisruptionBudget does not apply
  ([draining before scale-down](../../reference/workload-preparation.md#draining-before-scale-down)).
- [ ] `terminationGracePeriodSeconds` of at least one engine cycle (30
  seconds by default) on serving pods. The scaling manager sees a preemption
  only while the preempted pod or its Workload exists; a pod gone between two
  cycles is missed, and the model does not cool down before borrowing again
  ([cooldowns](../../reference/quota-limiter.md#kueue-as-a-quota-source)).
- [ ] A `WorkloadPriorityClass` above batch for models that must keep
  replicas: Kueue picks what to preempt by it.
- [ ] Kueue's group integration for multi-pod replicas (LeaderWorkerSet, P/D),
  so a replica is admitted, and preempted, whole.
- [ ] No `borrowWithinCohort` preemption on serving queues, so serving never
  preempts a lender's training.
- [ ] A GPU bound: a `gpu-inventory` limiter (added by default on a
  cluster-scoped install), or a GPU `ResourceQuota` at most nominal plus
  `borrowingLimit`. A namespace-scoped install needs the ResourceQuota.

**What protects a lender.** Only Kueue does, and only with
`reclaimWithinCohort: Any`. Under `Never` a lender gets capacity back because
the scaling manager releases it: the borrower's cooperation, nothing more.
`LowerPriority` is treated like `Never`: it reclaims only from lower-priority
work, and serving runs above batch. In a cohort shared between tenants, give
every lender that must get capacity back from serving `Any`. The check that a
namespace's GPU pods are Kueue-managed (each backed by a Workload with quota
reserved) is a safety heuristic, not an authorization boundary
([security model](../../reference/security-model.md#kueue-borrowing)).

**What it costs.** Borrowed replicas go back: Kueue preempts them for a
lender that can reclaim, and the scaling manager lowers the model itself for
a lender that cannot, or when a borrowed replica does not place within
`placementTimeout`. Each loss is a cold start. Scale-from-zero, the warm pool
and rescale never use borrowed capacity.

**Turn it off** with `WVA_QUOTA_KUEUE_BORROW=false` beside
`WVA_QUOTA_KUEUE=true` on `make deploy-wva` or `make enable-physical-limiter`,
or by hand with `countBorrowable: false` under the entry's `kueue:` block. The
cap is then nominal, exactly as before borrowing was counted.

**On upgrade** a Kueue-bounded entry starts counting borrowing without a
config change, where the conditions hold; a namespace-scoped install needs a
cluster admin to re-run `make setup-prereqs` for the new reads
([upgrading](../../reference/upgrade-and-uninstall.md#limiters-and-kueue-borrowing)).

When replicas drop: [the runbook](../../reference/troubleshooting.md#replicas-dropped-under-kueue-borrowing).

## Verifying it

```bash
# the effective entry, logged once each time Kueue's grant changes
kubectl logs -n <wva-namespace> deploy/wva-controller-manager \
  | grep -E 'quota entry bounded by external caps|external quota source'
```

The `bounded by external caps` line carries four maps: Kueue's grant per
namespace and for the cluster, and the **effective** namespace and cluster caps
after the merge — the numbers the scaling manager enforces. `external quota source unreadable`
names an RBAC or CRD problem and says whether a stale snapshot or the static
entry is in force. `grants GPUs of no named accelerator type` is the
untyped-flavor case above.

**Borrowing** has its own line, `Kueue borrowing applied where every condition
holds and withheld elsewhere`, per namespace with the guaranteed and borrowable
figures and the reason borrowing is withheld. The same figures are metrics
(`wva_quota_limit_gpus`, `wva_quota_borrow_withheld`) and the `limiterBudgets`
key of `wva-controller-status` ([Prometheus](../../reference/prometheus.md#kueue-borrowing-metrics)).

**The reader's own state is visible only in the log.** No metric reports a
Forbidden or a missing CRD.

A change in Kueue appears after at most `refreshInterval` plus one
[optimize cycle](../../reference/cycle-log.md). The e2e runs with
`refreshInterval: 5s` and allows three minutes; it asserts the transition, not
its latency.

## What it costs

Three list calls against the API server per `refreshInterval` (30s by default),
shared by both engines — no informer, no watch. And the lag above. No benchmark
scenario backs this page: a bound changes what the optimizer may order, not how
it scales.

## How it is tested

- Unit: `internal/config/quota_limiter_kueue_test.go` (every merge rule,
  untyped grants, the per-flavor split, the reserved key and exclusions),
  `internal/kueue/reader_test.go` (attribution, inactive queues, both served
  API versions, the refresh cache, a caller's cancelled context),
  `internal/engines/allocation/quota_inventory_source_test.go` (the effective
  entry reaching the optimizer's constraints, stale snapshots, one shared
  reader), `internal/accelerator/identity_test.go` (how a full product label
  meets a short static key). `hack/check-limiter-declaration.sh` pins what
  `WVA_QUOTA_KUEUE` emits.
- End-to-end: `test/e2e/kueue_quota_test.go` (label `full`, `kueue-quota`) on
  kind — Kueue grants one GPU against a static cap of three; the limiter's
  effective entry reads one, then three again after the LocalQueue is deleted.
  It runs with Kueue's CRDs only (`deploy/ci-pr-checks/install-kueue-crds.sh`,
  wired into `test-e2e-full-with-setup` on kind); it fails, not skips, when
  they are absent there, and skips with a reason on a cluster that does not
  serve them.
- End-to-end, live Kueue: `test/e2e/kueue_borrow_test.go` (label `kueue-borrow`)
  covers borrowing, a release to a `Never` lender and recovery after
  preemption: B1 withholds borrowing, A grows into the cohort with every pod
  placed, one replica is released to a waiting `Never` lender, and after
  Kueue preempts a borrowed pod A drops back, borrows nothing during the
  cooldown and grows again after it. Passed locally on kind (Kueue v0.20.0);
  not yet run in CI, so the path stays Experimental. Borrowing, its
  conditions, node fit and giving back are also unit-tested in
  `internal/engines/allocation/` (`borrow_*_test.go`,
  `quota_inventory_borrow_test.go`) and `internal/kueue/`.
- **Not covered:** inactive queues, finalizers and `stopPolicy` transitions
  (imitated by fixtures); enforcement past the
  effective entry, which is the same optimizer path
  [tenant-gpu-quotas](../tenant-gpu-quotas/) relies on, with the same gap; and
  the reader's state, which has no metric.

## Tuning it

Every field, the full merge table and what is not read:
[the quota limiter reference, "Kueue as a quota source"](../../reference/quota-limiter.md#kueue-as-a-quota-source)
— `resources` narrows which extended resources count as GPUs, `refreshInterval`
how often Kueue is re-read, and the borrowing settings (`countBorrowable`,
`placementTimeout`, `borrowCooldown`, `lenderDebounce`). The installer variables:
[configuration](../../reference/configuration.md). Why a quota counts only
the scaling manager's own consumption:
[GPU capacity accounting](../../concepts/gpu-capacity-accounting.md).
