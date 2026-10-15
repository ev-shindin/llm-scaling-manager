# Bound tenants by the quotas Kueue already holds

If Kueue already decides your GPU quotas, the scaling manager can read them
instead of keeping a second copy. It then caps each namespace and accelerator
type at **the smaller of Kueue's grant and its own static cap**, so it never
asks KEDA for a replica Kueue would hold pending. Where it is safe, the grant
includes what the namespace's ClusterQueue can **borrow** from its cohort;
Kueue takes borrowed replicas back when a lender that can reclaim needs them.

**Maturity:** **Experimental.** A live-Kueue e2e covers borrowing, a stopped
Kueue controller, a `Never` lender with and without the opt-in release, and
recovery after preemption. It runs in its own CI job on PRs that touch
borrowing and nightly ([details](#what-is-not-proven-yet)).

## When not to take it

- Several namespaces share one ClusterQueue and the shared total must hold.
  Each namespace is capped at the queue's full grant
  ([a shared queue is not a partition](#three-things-that-surprise-people)).
- Tenants can create LocalQueues pointing at a queue that admits any namespace,
  and your static cap is `-1` (unlimited). They would get that queue's grant
  ([details](#check-what-your-clusterqueues-admit)).
- With `-1` only, if the controller must stay bounded when Kueue cannot be
  read. Before the first read, `-1` is no cap at all; use a finite static cap,
  which Kueue can only tighten.
- A model must never lose a replica, and its queue lends to or borrows from a
  cohort. Give it the quota as nominal, or turn borrowing off
  ([Borrowing](#borrowing)).

## Start here

This extends [capping what each tenant may take](../tenant-gpu-quotas/#start-here):
read that first; everything there still applies. You also need Kueue's
`ClusterQueue`, `LocalQueue` and `ResourceFlavor` kinds served, and
LocalQueues in the managed namespaces. No guide covers this path yet, so the
steps are here.

1. Namespace owner or admin: check accelerators resolve, as in [the tenant path's step 1](../tenant-gpu-quotas/#start-here).
2. Cluster admin, for a namespace-scoped install: run `make setup-prereqs` so the controller can read Kueue ([what it grants](#what-it-needs)).
3. Declare the quota once, naming the types with `-1` so Kueue sets the numbers ([Declaring it](#declaring-it)), one of:
   - Namespace owner, advisory: `WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=-1' WVA_QUOTA_KUEUE=true make deploy-wva`.
   - Cluster admin, a budget the tenant cannot edit: `make enable-physical-limiter WVA_LIMITER_TYPE=quota WVA_QUOTAS='<accelerator>=-1' WVA_QUOTA_KUEUE=true`.
4. For borrowing (on by default), work through [its checklist](#borrowing). Without a GPU bound or Kueue-managed pods, borrowing is not counted and the nominal cap applies.
5. Check it works: the controller log shows `quota entry bounded by external caps` with the effective caps, and `Kueue borrowing applied` with the borrowing figures ([Verifying it](#verifying-it)).

To keep the Kueue cap but not borrow, add `WVA_QUOTA_KUEUE_BORROW=false` to the
command in step 3. To undo, replace the entry rather than only dropping the
flag: left at `-1` without Kueue, it is no cap at all ([how](#undoing-it)).

## What it costs

- Three list calls to the API server per `refreshInterval` (30s by default),
  shared by both engines. No informer, no watch.
- A lag: a change in Kueue takes effect after at most `refreshInterval` plus
  one [optimize cycle](../../reference/cycle-log.md).
- The reader's errors show only in the log; no metric reports them.
- With borrowing: borrowed replicas go back, each loss a cold start
  ([Borrowing](#borrowing)).

## Evidence

No benchmark scenario: a bound changes what the optimizer may order, not how
it scales. Two end-to-end suites back it: the read-and-merge chain against
Kueue's CRDs, and borrowing against a live Kueue
([How it is tested](#how-it-is-tested)).

## Borrowing

**Every condition below that is not met behaves exactly as today:** the
namespace caps at its nominal quota, and the log and
`wva_quota_borrow_withheld` say why.

With `kueue.enabled` on a namespace-scope entry, a model may grow past its
namespace's own quota into capacity its cohort lends. This is **on by
default**, and counted only where it is safe
([the conditions](../../reference/quota-limiter.md#cohort-borrowing)).

**Take it when** serving shares a cohort with batch work that leaves GPUs idle,
and a model can lose a borrowed replica when the lender wants it back.

Before relying on it, check each item with the command beside it:

- [ ] **A GPU bound**: a `gpu-inventory` limiter
  (`WVA_LIMITER=gpu-inventory,quota`), or a GPU `ResourceQuota` at most
  nominal plus `borrowingLimit`. The installer adds neither; a
  namespace-scoped install needs the ResourceQuota. Without one, borrowing is
  withheld (`no-gpu-bound`).
  ```bash
  kubectl get cm wva-scaling-policy-config -n <policy-namespace> -o jsonpath='{.data.default}' | yq '[.limiters[].type]'
  kubectl get resourcequota -n <ns> -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.hard}{"\n"}{end}'
  ```
- [ ] **A drain hook on serving pods.** Kueue deletes a preempted pod without
  the Eviction API, so a PodDisruptionBudget does not apply. `make
  workload-patch NAMESPACE=<ns>` names the workloads without one and writes it
  ([draining before scale-down](../../reference/workload-preparation.md#draining-before-scale-down)).
- [ ] **`terminationGracePeriodSeconds` of at least one engine cycle** on
  serving pods. The default grace, 30 seconds, is above the default cycle
  (`GLOBAL_OPT_INTERVAL`, 15 seconds); only a lowered grace is a problem. A
  preemption is seen only while the preempted pod or its Workload exists; one
  gone between two cycles is missed, and the model does not cool down before
  borrowing again.
  ```bash
  kubectl get deploy -n <ns> -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.template.spec.terminationGracePeriodSeconds}{"\n"}{end}'
  ```
- [ ] **Tenants cannot write Kueue `Workloads`** in namespaces that count
  borrowing (or set `countBorrowable: false` there). The managed check reads
  the Workloads ([security model](../../reference/security-model.md#kueue-borrowing)).
  Expect `no`:
  ```bash
  kubectl auth can-i create workloads.kueue.x-k8s.io -n <ns> --as=<tenant>
  ```
- [ ] **A `WorkloadPriorityClass` above batch** for models that must keep
  replicas: Kueue picks what to preempt by it.
  `kubectl get workloadpriorityclass`.
- [ ] **Kueue's group integration for multi-pod replicas** (LeaderWorkerSet),
  so a replica is admitted, and preempted, whole. A P/D model deployed as two
  Deployments is two Kueue workloads: Kueue may admit or preempt one role
  without the other. The scaling manager releases P/D roles together; Kueue's
  preemption does not. `kubectl get workloads.kueue.x-k8s.io -n <ns>` shows
  one Workload per role.
- [ ] **No `borrowWithinCohort` preemption on serving queues**, so serving
  never preempts a lender's training, and **`reclaimWithinCohort: Any` on
  every lender that must get capacity back** (below):
  ```bash
  kubectl get clusterqueue -o jsonpath='{range .items[*]}{.metadata.name}{" "}{..preemption}{"\n"}{end}'
  ```

**What protects a lender.** Kueue does, with `reclaimWithinCohort: Any`: it
preempts borrowed replicas when the lender asks for its quota. For a lender
Kueue does not reclaim for -- `Never`, or `LowerPriority` when the lender does
not outrank the borrower, as batch below serving does not -- the scaling
manager, **by default, hands nothing back**. It stops new borrowing in that
cohort while the lender waits (reason `lender-waiting`) and reports the wait
(`wva_kueue_lender_waiting_seconds`, alert `WVAKueueLenderWaitingPastCooldown`);
the borrowers keep what they hold until their demand falls. To have it release
borrowed replicas to such a lender, one at a time, set
`releaseToNonReclaimingLenders: true` under the entry's `kueue:` block. That is
the borrower's cooperation, nothing more: in a cohort shared between tenants,
give every lender that must get capacity back from serving `Any`.

**What it costs.** Kueue preempts borrowed replicas for a lender that can
reclaim. The scaling manager lowers the model itself when a borrowed replica
does not place within `placementTimeout`, and, only with the opt-in above, for
a waiting lender that cannot reclaim. Each loss is a cold start. The placement
timeout counts only pods that are unscheduled or held at Kueue's admission
gate; a pod that has a node and is loading its weights is placed, however long
it takes to become Ready. Scale-from-zero, the warm pool and rescale never use
borrowed capacity.

**Check it works.** In Prometheus, where the model's namespace is the
`exported_namespace` label ([the series](../../reference/prometheus.md#kueue-borrowing-metrics)):

```promql
wva_quota_limit_gpus{tier="borrowable"} > 0          # what each namespace may borrow now
wva_quota_borrow_withheld                            # no series for the namespace: nothing withholds it
```

and a model's replicas above its guaranteed figure while every pod is placed:

```bash
kubectl get configmap wva-controller-status -n "$NS" -o jsonpath='{.data.limiterBudgets}' | jq '.entries[] | select(.namespace=="<ns>")'
kubectl get pods -n <ns> --field-selector status.phase=Pending
```

`used` above `guaranteed` in `limiterBudgets`, and no Pending pod, is a model
running on borrowed capacity.

**Turn it off** with `WVA_QUOTA_KUEUE_BORROW=false` beside
`WVA_QUOTA_KUEUE=true` on `make deploy-wva` or `make enable-physical-limiter`,
or by hand with `countBorrowable: false` under the entry's `kueue:` block. The
cap is then nominal, exactly as before borrowing was counted.

**On upgrade** a Kueue-bounded entry starts counting borrowing without a
config change, where the conditions hold. A namespace-scoped install needs a
cluster admin to re-run `make setup-prereqs` for the new reads. The installer
no longer adds a `gpu-inventory` entry with `WVA_QUOTA_KUEUE=true`: declare one,
or a GPU ResourceQuota, to give borrowing its bound
([upgrading](../../reference/upgrade-and-uninstall.md#limiters-and-kueue-borrowing)).

**Supported Kueue versions.** Borrowing reads Kueue fields and conditions
that are checked against these releases. The scaling manager reads the
version from the image tag of `kueue-controller-manager` (in `kueue-system`,
or the entry's `kueue.controllerNamespace`). Outside the tested range,
borrowing is withheld (`kueue-version-untested`); the nominal quotas still
apply. A version it cannot read (no permission, a digest-only image) is
logged and does not withhold. The same Deployment tells whether Kueue runs:
with no ready replica, or gone, borrowing freezes (`kueue-controller-down`):
nothing new is borrowed and nothing borrowed is taken away until it is back.
If Kueue runs in another namespace, set `kueue.controllerNamespace`, or
neither check runs.

| Kueue | Borrowing |
|---|---|
| v0.20.x | tested (the live e2e runs v0.20.0) |
| v0.19.x | tested |
| anything else | withheld: `kueue-version-untested` |

`wva_kueue_borrow_drift_gpus` checks the reading against Kueue itself: per
ClusterQueue, how far the borrowed figure computed here is from Kueue's own
`status.flavorsReservation` borrowed figure. It stays 0 while they agree.
**To roll back** to a release without borrowing:

1. First set `countBorrowable: false` (or re-run the install with
   `WVA_QUOTA_KUEUE_BORROW=false`) and wait one `borrowCooldown`: models drop
   back to nominal through their normal scale-down, and the releases in flight
   land.
2. Install the older release. It ignores the `borrowReleaseLedger` and
   `limiterBudgets` keys in `wva-controller-status`; nothing reads them, and
   the next release that does reads its own version.
3. A target the scaling manager lowered for a release stays lowered only until
   the older controller's next decision: it does not pin it.
4. The RBAC the borrowing reads added (Kueue `cohorts` and `workloads`;
   `resourcequotas`, `events` and the pod owners in managed namespaces) stays
   behind. Remove it by re-applying the older release's roles, or leave it:
   it grants reads only.

When replicas drop: [the runbook](../../reference/troubleshooting.md#replicas-dropped-under-kueue-borrowing).

## Details

### What is not proven yet

The merge rules, the reader, borrowing and giving it back are unit tested. One
end-to-end spec proves the read-and-merge chain on Kueue's CRDs alone. A
live-Kueue spec covers borrowing, a release to a `Never` lender and recovery
after preemption; it passed locally on kind and has not yet run in CI.
Defaults that may still move: the 30s `refreshInterval`, how an untyped grant
applies (to each named type separately, and the largest across vendors rather
than the sum), and the borrowing timings (`placementTimeout` 5m,
`borrowCooldown` 10m).

### What it needs

- Everything [tenant-gpu-quotas](../tenant-gpu-quotas/) needs, including its
  Namespace-read note for the cluster policy on Kubernetes.
- Kueue's three kinds served. The reader lists them and never talks to Kueue's
  controller, so the CRDs alone let it work; a real installation is what makes
  the grants mean anything, and borrowing needs one.
- RBAC to read them. A **cluster-scoped** install has it in the manager
  ClusterRole. A **namespace-scoped** install reads LocalQueues and Workloads
  through its Role (`wva-manager-role`). It gets the cluster-scoped kinds
  (ClusterQueues, Cohorts, ResourceFlavors), plus `get` on its managed
  Namespace (to evaluate a `namespaceSelector`), from the `kueue-reader`
  prerequisite. The admin phase creates it, in the same run that creates
  `node-reader`:
  `NAMESPACE=<managed ns> make setup-prereqs SCOPE=namespace ENVIRONMENT=kubernetes`,
  adding `WVA_NS=wva-<managed ns> WVA_WATCH_NS=<managed ns>` when the
  controller is out of the tenant's reach.
- Without the ClusterQueue and ResourceFlavor read, the reader logs
  `external quota source unreadable` each cycle and runs on the static entry.
  Without the Cohort read, borrowing is withheld (`hierarchy-unknown`); the
  nominal cap still applies.

**Older installs.** The tenant's `make deploy-wva` does not apply Roles. A
namespace-scoped controller installed before this feature, or whose
`kueue-reader` predates the Namespace read, needs a cluster admin to re-run
`make setup-prereqs NAMESPACE=<ns>` once, with the original `WVA_NS` /
`WVA_WATCH_NS` ([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-to-this-release)).
Until then, with this block on, the tenant's `make deploy-wva` refuses and
names that run. When it cannot tell (it may not impersonate the controller, or
cannot read whether a Kueue source is configured) it warns with the same fix
and installs.

### Declaring it

The installer writes it. Add `WVA_QUOTA_KUEUE=true` to the tenant path's
`WVA_LIMITER=quota` install and name the types in `WVA_QUOTAS`. It renders as
one more block on the quota entry:

```yaml
limiters:
  - name: install-quota
    type: quota
    scope: namespace
    kueue:
      enabled: true
      # countBorrowable: false     # WVA_QUOTA_KUEUE_BORROW=false: cap at nominal
    namespaceQuotas:
      <managed namespace>:
        H100: -1                    # the TYPE is yours; the NUMBER is Kueue's
```

For the cluster policy, `make enable-physical-limiter` with
`WVA_QUOTA_KUEUE=true` also grants each target controller the cluster-scoped
half of the Kueue read (ClusterQueues, Cohorts, ResourceFlavors, `get` on
Namespaces), as it grants the node read for `gpu-inventory`. A
namespace-scoped controller gets `get` on the namespace it manages, plus any
name its role already holds. A cluster-scoped controller the installer made
already reads every Namespace. Nothing needs naming in `WVA_LIMITER_TARGETS`.
It warns only when a controller that looks cluster-scoped lacks the
cluster-wide Namespace read (naming the `make setup-prereqs` run to fix it),
or when it cannot tell.

Hand-write the entry to name several namespaces, or a finite static cap for
some: wherever both sides name a type, the smaller figure wins.

A namespace is **governed** by Kueue when one of its LocalQueues reaches a
ClusterQueue that declares a GPU resource; it then gets that queue's grant. A
namespace with no such LocalQueue is decided by the static entry alone. What
each Kueue object becomes, and what is not read:
[the reference](../../reference/quota-limiter.md#what-is-read).

### Check what your ClusterQueues admit

This matters where tenants can create their own LocalQueues and the static cap
is `-1`. A namespace gets a ClusterQueue's quota only when the queue's
`namespaceSelector` admits it, with Kueue's semantics: no selector admits
nothing, `{}` admits every namespace, anything else matches the namespace's
labels. A namespace whose labels the controller cannot read gets nothing from
a selector-bearing queue.

What remains open is a queue whose selector is `{}`, or one matching a label
the tenant can set. A tenant who points a LocalQueue at it gets its grant, as
Kueue would admit their work there. Keep such queues out of tenants' reach, or
keep LocalQueue creation with the admin. To check whether your Kueue install
gives tenants that right:
`kubectl auth can-i create localqueues.kueue.x-k8s.io -n <ns> --as=<tenant>`.
Or keep a finite static cap, which Kueue can only tighten.

### Three things that surprise people

**Kueue's usual flavors name no accelerator type, so Kueue alone cannot open
one.** The quickstart's `default-flavor` has no node labels. Its grant is
*untyped*: it bounds every type the static entry names and names none itself.
With the block on, **no static map** and untyped flavors, the entry is an
empty map that **denies every type**. The limiter logs it once
(`grants GPUs of no named accelerator type`, with the fix). Hence the recipe:
name the types, let Kueue give the numbers (`H100: -1`). Give the flavors a
product label (`nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3`) and the grant
becomes typed; a per-flavor split (PCIe 4, SXM 0) is then honoured exactly.
The untyped shape's cost: a namespace naming two types may reach the untyped
grant on *each*, because there is no per-namespace total.

**A shared ClusterQueue is a ceiling per namespace, not a partition.** Three
namespaces on one queue may together ask for three times what it admits;
Kueue holds the excess pending. Where the shared total matters, give each
tenant its own ClusterQueue, with a cohort for borrowing.

**A Kueue outage keeps the last grant; a Kueue never read leaves only the
static entry.** A failed read keeps the last good snapshot, and the log says
how old it is; borrowing is frozen meanwhile (nothing new is borrowed, nothing
is given back). Before the *first* successful read (RBAC missing, CRDs absent,
a fresh controller) only the static entry applies, and `-1` is **no cap at
all**. A missing CRD or a Forbidden is reported as such every cycle, never
folded into "Kueue grants nothing". Both engines treat a limiter error as "no
constraint", so `Refresh` never surfaces one; the reader's errors go to the log.

### Verifying it

```bash
# the effective entry, logged once each time Kueue's grant changes
kubectl logs -n <wva-namespace> deploy/wva-controller-manager \
  | grep -E 'quota entry bounded by external caps|external quota source|Kueue borrowing applied'
```

- `bounded by external caps` carries four maps: Kueue's grant per namespace
  and for the cluster, and the **effective** namespace and cluster caps after
  the merge. The effective caps are what the scaling manager enforces.
- `external quota source unreadable` names an RBAC or CRD problem, usually
  missing `kueue-reader`, and says whether a stale snapshot or the static
  entry is in force.
- `external quota source grants no GPUs`: no LocalQueue in the namespace
  reaches a ClusterQueue with a GPU resource.
- `grants GPUs of no named accelerator type` is the untyped-flavor case above.
- `Kueue borrowing applied where every condition holds and withheld elsewhere`
  lists, per namespace, the guaranteed and borrowable figures and the reason
  borrowing is withheld. The same figures are metrics (`wva_quota_limit_gpus`,
  `wva_quota_borrow_withheld`) and the `limiterBudgets` key of
  `wva-controller-status` ([Prometheus](../../reference/prometheus.md#kueue-borrowing-metrics)).

Is borrowing counted for a namespace? A `borrowable` series above 0, and no
withheld reason:

```promql
wva_quota_limit_gpus{tier="borrowable"} > 0
wva_quota_borrow_withheld == 1
```

**The reader's own state is visible only in the log.** No metric reports a
Forbidden or a missing CRD. The e2e runs with `refreshInterval: 5s` and allows
three minutes; it asserts the transition, not its latency.

### Undoing it

- One install (namespace owner): re-run `make deploy-wva` with
  `WVA_LIMITER=quota`, a finite `WVA_QUOTAS` and no `WVA_QUOTA_KUEUE`, or with
  `WVA_LIMITER=none`. With no `WVA_LIMITER`, the installer keeps the entry.
- Cluster policy (cluster admin): re-run
  `make enable-physical-limiter WVA_LIMITER_TYPE=quota` with a finite
  `WVA_QUOTAS` and no `WVA_QUOTA_KUEUE`, or run
  `make disable-physical-limiter WVA_LIMITER_TYPE=quota` to remove the quota.
  Without `WVA_LIMITER_TYPE=quota` it removes only the physical limiter.

### How it is tested

- Unit: `internal/config/quota_limiter_kueue_test.go` (every merge rule,
  untyped grants, the per-flavor split, the reserved key and exclusions),
  `internal/kueue/reader_test.go` (attribution, inactive queues, both served
  API versions, the refresh cache, a caller's cancelled context),
  `internal/engines/allocation/quota_inventory_source_test.go` (the effective
  entry reaching the optimizer's constraints, stale snapshots, one shared
  reader), `internal/accelerator/identity_test.go` (how a full product label
  meets a short static key). Borrowing, its conditions, node fit and giving
  back: `internal/engines/allocation/` (`borrow_*_test.go`,
  `quota_inventory_borrow_test.go`), `internal/engines/steadystate/`
  (`borrowing_*_test.go`) and `internal/kueue/`.
  `hack/check-limiter-declaration.sh` pins what `WVA_QUOTA_KUEUE` and
  `WVA_QUOTA_KUEUE_BORROW` emit.
- End-to-end: `test/e2e/kueue_quota_test.go` (labels `full`, `kueue-quota`) on
  kind. Kueue grants one GPU against a static cap of three; the effective
  entry reads one, then three again after the LocalQueue is deleted. It runs
  with Kueue's CRDs only (`deploy/ci-pr-checks/install-kueue-crds.sh`, wired
  into `test-e2e-full-with-setup` on kind). It fails, not skips, when they are
  absent there, and skips with a reason on a cluster that does not serve them.
- End-to-end, live Kueue: `test/e2e/kueue_borrow_test.go` (label
  `kueue-borrow`). Borrowing is withheld while an unmanaged GPU pod runs; the
  model is not ordered past its nominal while the Kueue controller is down;
  it then grows into the cohort with every pod placed; a waiting `Never`
  lender gets nothing back by default and one replica once
  `releaseToNonReclaimingLenders` is set; after Kueue preempts a borrowed pod
  the model drops back, borrows nothing during the cooldown and grows again
  after it. Its own CI job, `e2e-tests-kueue-borrow`, runs it on PRs that
  touch borrowing and nightly
  ([running it](../../developer-guide/testing.md#kueue-specs)).
- **Not covered:** inactive queues, finalizers and `stopPolicy` transitions
  (imitated by fixtures); enforcement past the effective entry, the same
  optimizer path and gap as [tenant-gpu-quotas](../tenant-gpu-quotas/); and
  the reader's state, which has no metric.

### Tuning it

Every field, the full merge table and what is not read:
[Kueue as a quota source](../../reference/quota-limiter.md#kueue-as-a-quota-source).
`resources` narrows which extended resources count as GPUs; `refreshInterval`
sets how often Kueue is re-read; `countBorrowable`, `placementTimeout`,
`borrowCooldown` and `lenderDebounce` tune borrowing, and `borrowNamespaces`
pilots it on the namespaces it lists
([cohort borrowing](../../reference/quota-limiter.md#cohort-borrowing)).
Installer variables: [configuration](../../reference/configuration.md). Why a
quota counts only the scaling manager's own consumption:
[GPU capacity accounting](../../concepts/gpu-capacity-accounting.md).
