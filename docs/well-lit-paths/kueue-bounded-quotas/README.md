# Bound tenants by the quotas Kueue already holds

If Kueue already decides your GPU quotas, the scaling manager can read them
instead of keeping a second copy. It then caps each namespace and accelerator
type at **the smaller of Kueue's grant and its own static cap**, so it never
asks KEDA for a replica Kueue would hold pending.

**Maturity:** **Experimental.** No run yet against a live Kueue controller
([details](#what-is-not-proven-yet)).

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
4. Check it works: the controller log shows `quota entry bounded by external caps` with the effective caps ([Verifying it](#verifying-it)).

To undo, replace the entry rather than only dropping the flag: left at `-1`
without Kueue, it is no cap at all ([how](#undoing-it)).

## What it costs

- Three list calls to the API server per `refreshInterval` (30s by default),
  shared by both engines. No informer, no watch.
- A lag: a change in Kueue takes effect after at most `refreshInterval` plus
  one [optimize cycle](../../reference/cycle-log.md).
- The reader's errors show only in the log; no metric reports them.

## Evidence

No benchmark scenario: a bound changes what the optimizer may order, not how
it scales. One end-to-end suite proves the read-and-merge chain against a real
API server with Kueue's CRDs only ([How it is tested](#how-it-is-tested)).

## Details

### What is not proven yet

The merge rules, the reader and the installer knob are unit tested. The
end-to-end suite installs only Kueue's **CRDs**, not Kueue, and stops at the
effective entry the limiter logs. **No run has been made against a live Kueue
controller.** The two things only a live one shows, a queue going inactive and
finalizers on its objects, are tested by imitation. Two defaults may still
move: the 30s `refreshInterval`, and how an untyped grant applies (to each
named type separately, and the largest across vendors rather than the sum).

### What it needs

- Everything [tenant-gpu-quotas](../tenant-gpu-quotas/) needs, including its
  Namespace-read note for the cluster policy on Kubernetes.
- Kueue's three kinds served. The scaling manager lists them and never talks to
  Kueue's controller, so the CRDs alone let the reader work; a real
  installation is what makes the grants mean anything.
- RBAC to read them. A **cluster-scoped** install has it in the manager
  ClusterRole. A **namespace-scoped** install reads LocalQueues through its
  Role (`wva-manager-role`). It gets the two cluster-scoped kinds, plus `get` on
  its managed Namespace (to evaluate a `namespaceSelector`), from the
  `kueue-reader` prerequisite. The admin phase creates it, in the same run that
  creates `node-reader`:
  `NAMESPACE=<managed ns> make setup-prereqs SCOPE=namespace ENVIRONMENT=kubernetes`,
  adding `WVA_NS=wva-<managed ns> WVA_WATCH_NS=<managed ns>` when the
  controller is out of the tenant's reach.
- Without the ClusterQueue and ResourceFlavor read, the reader logs
  `external quota source unreadable` each cycle and runs on the static entry.

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
    namespaceQuotas:
      <managed namespace>:
        H100: -1                    # the TYPE is yours; the NUMBER is Kueue's
```

For the cluster policy, `make enable-physical-limiter` with
`WVA_QUOTA_KUEUE=true` also grants each target controller the cluster-scoped
half of the Kueue read (ClusterQueues, ResourceFlavors, `get` on Namespaces),
as it grants the node read for `gpu-inventory`. A namespace-scoped controller
gets `get` on the namespace it manages, plus any name its role already holds.
A cluster-scoped controller the installer made already reads every Namespace.
Nothing needs naming in `WVA_LIMITER_TARGETS`. It warns only when a controller
that looks cluster-scoped lacks the cluster-wide Namespace read (naming the
`make setup-prereqs` run to fix it), or when it cannot tell.

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
tenant its own ClusterQueue, with a cohort for borrowing. Only `nominalQuota`
is read, so in a cohort that lends quota, a model is held at its nominal share
even when Kueue would admit more
([#136](https://github.com/ev-shindin/llm-scaling-manager/issues/136)).

**A Kueue outage keeps the last grant; a Kueue never read leaves only the
static entry.** A failed read keeps the last good snapshot, and the log says
how old it is. Before the *first* successful read (RBAC missing, CRDs absent,
a fresh controller) only the static entry applies, and `-1` is **no cap at
all**. A missing CRD or a Forbidden is reported as such every cycle, never
folded into "Kueue grants nothing". Both engines treat a limiter error as "no
constraint", so `Refresh` never surfaces one; the reader's errors go to the log.

### Verifying it

```bash
# the effective entry, logged once each time Kueue's grant changes
kubectl logs -n <wva-namespace> deploy/wva-controller-manager \
  | grep -E 'quota entry bounded by external caps|external quota source'
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

**The reader's state is visible only here.** No metric or condition reports a
Forbidden or a missing CRD. The tenant path's `wva_model_scaling_blocked`
still shows a model held at its cap, but not why the cap is what it is. The
e2e runs with `refreshInterval: 5s` and allows three minutes; it asserts the
transition, not its latency.

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
  meets a short static key). `hack/check-limiter-declaration.sh` pins what
  `WVA_QUOTA_KUEUE` emits.
- End-to-end: `test/e2e/kueue_quota_test.go` (labels `full`, `kueue-quota`) on
  kind. Kueue grants one GPU against a static cap of three; the effective
  entry reads one, then three again after the LocalQueue is deleted. It runs
  with Kueue's CRDs only (`deploy/ci-pr-checks/install-kueue-crds.sh`, wired
  into `test-e2e-full-with-setup` on kind). It fails, not skips, when they are
  absent there, and skips with a reason on a cluster that does not serve them.
- **Not covered:** a live Kueue controller (inactive queues, finalizers and
  `stopPolicy` transitions are imitated by fixtures); enforcement past the
  effective entry, the same optimizer path and gap as
  [tenant-gpu-quotas](../tenant-gpu-quotas/); and the reader's state, which has
  no metric.

### Tuning it

Every field, the full merge table and what is not read:
[Kueue as a quota source](../../reference/quota-limiter.md#kueue-as-a-quota-source).
`resources` narrows which extended resources count as GPUs; `refreshInterval`
sets how often Kueue is re-read. Installer variables:
[configuration](../../reference/configuration.md). Why a quota counts only the
scaling manager's own consumption:
[GPU capacity accounting](../../concepts/gpu-capacity-accounting.md).
