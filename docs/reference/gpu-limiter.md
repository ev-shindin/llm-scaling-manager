# Bounding scaling: the GPU limiter

The scaling manager scales without a GPU budget unless you give it one. This page
covers how, and what has to be true first.

> Part of the [the scaling manager deployment guide](../../deploy/).

## Turning it on

```bash
make deploy-wva-on-k8s NAMESPACE=<namespace> WVA_LIMITER=gpu-inventory   # bound by GPUs actually free
make deploy-wva-on-k8s NAMESPACE=<namespace> WVA_LIMITER=quota WVA_QUOTAS='H200=8 A100=4'   # bound by declared caps
```

- `WVA_QUOTAS` is **required** for `quota`. A type it does not name gets a
  budget of **zero**, not "unlimited". Use `-1` for no cap (`H100=-1`). The type
  is the accelerator name the controller logs per variant (`"accelerator": "H200"`).
- `WVA_QUOTA_SCOPE`: `namespace` (default; each managed namespace gets this
  budget) or `cluster` (one budget across all of them).
- `WVA_QUOTA_KUEUE=true` also reads Kueue's ClusterQueues; each namespace gets
  the smaller of Kueue's grant and the static figure, so `WVA_QUOTAS='H100=-1'`
  lets Kueue set it. Experimental: read
  [Kueue as a quota source](quota-limiter.md#kueue-as-a-quota-source) and the
  [Kueue-bounded quotas path](../well-lit-paths/kueue-bounded-quotas/) first.
- A re-run of the install without `WVA_LIMITER` keeps the limiter already
  declared; pass it only to change it.

You can also add a `limiters:` entry to the `default` entry of the
scaling-policy ConfigMap later. It applies live when that ConfigMap is in the
namespace the controller manages. An out-of-reach (`WVA_WATCH_NS`) controller
reads its own ConfigMap only at startup, so restart it. **Read the next section
first.**

**Confirm the controller accepted it.** A malformed entry is rejected on read,
and that costs the **whole** `default` policy, thresholds included, leaving no
limiter at all. Only the controller log reports it, at ERROR; the install
reports success either way. The install prints this command, the only check
that answers:

```bash
kubectl logs -n <wva-namespace> deploy/wva-controller-manager \
  | grep -E 'GPU limiter constructed|Invalid saturation scaling'
```

```
INFO  GPU limiter constructed  {"type": "quota", "limiters": "quota/install-quota", ...}                    # bounded
INFO  GPU limiter constructed  {"type": "inventory+quota", "limiters": "inventory+quota/gpu,install-quota", ...}  # bounded by both
INFO  GPU limiter constructed  {"type": "none", "limiters": "none", "name": "no-limiter"}                  # NOT bounded
```

> **Both kinds bound together.** Declare `gpu-inventory` and `quota` in one
> `limiters:` list and every one is built: a decision must fit the GPUs that
> exist **and** the allowance. One physical limiter is built: a second
> `gpu-inventory` entry is ignored, with a warning in the controller log.
>
> **Upgrading from a release that built only one.** There, a quota entry made
> the physical one inert. Now both apply, which can hold a model lower when
> other workloads (training jobs included) hold GPUs. For one release the
> controller logs `Upgrade note (transitional)` naming every variant whose
> target the quota alone would have set differently, checked every tenth cycle.
>
> If one limiter cannot be consulted in a cycle (its usage is not observed yet,
> or it errors), it is skipped and named in the log; the others still bound.

## Who is allowed to change the bound

By default the limiter is read from the scaling-policy ConfigMap in the
controller's own namespace. For a **cluster-scoped** install a cluster admin owns
that namespace, so nothing more is needed.

A **namespace-scoped** install needs care. The controller runs in the namespace
it manages, so that namespace's admin can edit its Deployment, args, env and
ServiceAccount. Nothing carried on the controller can bound the person who edits
it.

So a controller that manages its own namespace takes its limits from a policy
namespace (`wva-policy`, or one named by a label) **when it can see one**,
instead of its own ConfigMap. Two things decide whether it can:

- **On Kubernetes it needs a grant to see one.** A Role cannot reach a
  Namespace object, so `make setup-prereqs` and `make enable-physical-limiter`
  grant `get` on the few Namespaces the controller reads. Without it (an
  install set up before the grant existed) the controller keeps reading the
  ConfigMap its tenant can write, and says so; see the note below.
- **An admin-owned controller never reads `wva-policy`, by design.** A
  cluster-scoped controller, or one in the out-of-reach shape, reads its own
  ConfigMap, which already lives where only an admin can write.

Where neither the policy namespace nor the out-of-reach shape applies, the bound
is only as strong as a `ResourceQuota` on the namespace.

### What an admin sets, and where

`make enable-physical-limiter` does everything in this section (policy,
namespace, RBAC and the node read) in one command. Below is the same result by
hand, for a cluster where something else manages those objects.

Everything authoritative is **cluster-scoped**: a namespace admin can neither
create a `Namespace` nor edit one's labels or annotations.

```bash
# Once per cluster. Read by every controller that manages its own namespace
# and can read Namespace objects -- see "What the controller does with that".
kubectl create namespace wva-policy
kubectl -n wva-policy create configmap wva-scaling-policy-config \
  --from-file=default=cluster-limiters.yaml

# Let a tenant's controller read it (read only — never write)
kubectl -n wva-policy create role wva-policy-reader \
  --verb=get,list,watch --resource=configmaps
kubectl -n wva-policy create rolebinding team-a-wva \
  --role=wva-policy-reader \
  --serviceaccount=team-a:wva-controller-manager

# Let it SEE wva-policy at all. Namespaces are cluster-scoped, so a
# namespace-scoped controller's Role cannot read them; without this it never
# finds the policy above. `make setup-prereqs` creates the same grant
# (components/policy-namespace-reader). The controller only GETs these by
# name -- its own namespace and wva-policy, never a list -- so the read can be
# limited to them, as `make enable-physical-limiter` does.
kubectl create clusterrole wva-policy-namespace-reader-team-a --verb=get \
  --resource=namespaces --resource-name=team-a --resource-name=wva-policy
kubectl create clusterrolebinding wva-policy-namespace-reader-team-a \
  --clusterrole=wva-policy-namespace-reader-team-a \
  --serviceaccount=team-a:wva-controller-manager
```

A namespace that draws its limits from somewhere other than the default is
**labelled**:

```bash
kubectl label namespace team-a wva.llmd.ai/policy-namespace=platform-policy
```

It is a label, not an annotation, so you can select on it to audit which
namespaces a policy governs:

```bash
kubectl get ns -l wva.llmd.ai/policy-namespace=platform-policy
```

> **On Kubernetes, a namespace-scoped controller sees the label only through a
> grant.** Reading it needs `get` on Namespace objects, which a Role cannot
> give. `make setup-prereqs` (namespace scope) and `make enable-physical-limiter`
> grant it with a ClusterRole limited by name to the Namespaces the controller
> reads.
>
> For an install set up before that grant existed, a cluster admin re-runs
> `NAMESPACE=<ns> make setup-prereqs` (or applies the
> [manual grant](../guides/admin-cluster-setup/README.md#grant-the-namespace-read-by-hand)),
> then restarts the controller. Without the grant, the controller does not see
> the label or `wva-policy` and runs on its own ConfigMap. It says so: it logs
> `Could not read the managed Namespace` and `Cannot tell whether a cluster
> admin published policy`, and sets `wva_policy_namespace_unverified` to 1
> (alert `WVAPolicyNamespaceUnverified`).
>
> On OpenShift the `cluster-monitoring-view` binding is reported to carry that
> read, but nothing in this repository pins the role's contents. Confirm it with
> `kubectl auth can-i get namespaces --as=system:serviceaccount:<controller namespace>:wva-controller-manager`.

### What the controller does with that

Policy is resolved once at startup, in order:

1. the `wva.llmd.ai/policy-namespace` **label** on the namespace being managed
2. the **default policy namespace**, `wva-policy`, a name fixed in the scaling
   manager (so the install scripts never define it)
3. the controller's own namespace

Steps 1 and 2 apply only to a controller that manages its own namespace, and
only when it may read Namespace objects (see the note above). The startup log
says which step won. Steps 1 and 2 log `Cluster policy comes from the namespace
this one is labelled with` or `... the well-known namespace` at Info. Step 3
logs `Cluster policy comes from this controller's own namespace` at V(1), which
the default `-v=2` shows. A controller denied the Namespace read also logs
`Could not read the managed Namespace` at Info:

```bash
# NS: the controller's namespace
kubectl logs -n "$NS" deploy/wva-controller-manager \
  | grep -E 'Cluster policy comes from|Could not read the managed Namespace'
```

The policy is logged once, at startup. If the line has rotated out of the log,
the [`wva-controller-status` ConfigMap](operations.md#what-the-controller-is-running-with)
holds the same answer.

A **cluster-scoped** controller already runs where only a cluster admin can
write, so its own ConfigMap *is* cluster policy. It never reads `wva-policy`, on
purpose: otherwise creating `wva-policy` for one tenant would switch a correctly
bounded cluster install to an empty policy, making it **unbounded** with no edit
to that install.

When a policy namespace applies, limiters in the controller's own ConfigMap are
ignored *and logged*: a limiter that looks enforced but is not is worse than
either. Thresholds, tiers and per-model settings stay with the team running the
workload; those are tuning, not entitlement.

If the `get namespaces` read is **Forbidden**, the controller cannot tell
whether policy was published for it. It starts on its own ConfigMap, logs
`Cannot tell whether a cluster admin published policy` with the fix, and sets
`wva_policy_namespace_unverified` to **1**. Fix it with
`NAMESPACE=<ns> make setup-prereqs` or `make enable-physical-limiter`; both
create the grant, and `enable-physical-limiter` also restarts the affected
controllers.

A namespace-scoped controller caches only the namespace it manages
(`cmd/main.go`) and reads any other ConfigMap at startup. So every
`enable-physical-limiter` and `disable-physical-limiter` restarts **every
namespace-scoped controller whose policy source is outside the namespace it
manages**: a self-managed one reading `wva-policy` or its labelled namespace,
and an out-of-reach one reading its own ConfigMap. It lists them as
`WILL BE RESTARTED` first, and exits non-zero naming any it could not restart.

The label is read at startup too. After pointing `wva.llmd.ai/policy-namespace`
elsewhere, re-run `make enable-physical-limiter`: it grants the read of the new
namespace and restarts the controller.

If the policy ConfigMap **exists but cannot be read** (usually a missing
RoleBinding), the controller refuses to start rather than run unbounded while an
admin believes a quota applies. The error names the missing
`wva-policy-reader-<suffix>` RoleBinding. `make setup-prereqs` creates it when
the policy namespace already exists.

### When policy demands a limiter the controller cannot serve

If cluster policy declares `gpu-inventory` and the controller may not list nodes,
it reports **not ready** and stays that way.

Declared **beside a quota**, it does not go NotReady. It skips the physical
limiter and the quota still bounds every decision. It logs `Cluster policy
declares gpu-inventory beside quota limiters, but this controller may not list
nodes` when that starts, and
`wva_constraint_provider_skipped{provider="gpu-limiter",reason="node_read_denied"}`
is 1 until the read is granted. Such a policy was bounded by the quota alone
before limiters composed, so failing the pod would freeze scaling on upgrade.

It is a *readiness* check, not a startup one, because this can happen at two
times:

- **at install**: the rollout never completes, so the install fails loudly;
- **later**: an admin adds the limiter to cluster policy. A cluster-scoped
  controller reloads it live; a namespace-scoped one reads it at restart
  (`make enable-physical-limiter` restarts it). The pod goes NotReady, which is
  visible and alertable.

Without this, every variant would be charged to no accelerator pool, get no GPU
budget, and never scale up again, which looks exactly like an idle cluster.

Fix: grant the node read (`make setup-prereqs` creates the node-reader
ClusterRole and binding), or remove the limiter from cluster policy.

A namespace-scoped controller also counts the GPUs that Pods in **other
namespaces** hold on the same nodes. Its cache holds only its own namespace, so
it lists Pods uncached and cluster-wide. That needs `list` on `pods` cluster-wide,
which the node-reader grant does not include. Without it the list is Forbidden,
and usage counts only the controller's own namespace, so GPUs other namespaces
hold on those nodes read as free. While a `gpu-inventory` limiter is in force,
`wva_pod_access_denied` is **1** and an error is logged. Grant `list` on `pods`
cluster-wide, or drop `gpu-inventory` and bound with a quota limiter alone (a
quota declared beside `gpu-inventory` leaves the physical limiter reading this
count). Under quota limiters alone, or none, the undercount bounds nothing: the gauge stays 0 and the log says so once at info.

### What this does and does not bound

**A guardrail, not an enforcement boundary.** Whoever owns the controller's
Deployment owns its args, env and image, so a tenant running the scaling manager
in their own namespace can change what bounds them. What it does guarantee: the
GPU budget is authoritative for every controller that reads the policy holding
it. That covers misconfiguration and drift.

The limiter also bounds only what *the scaling manager* asks for. The
ScaledObject belongs to the workload's owner. Raising `maxReplicaCount` or
adding a second KEDA trigger bypasses it, because the HPA takes the maximum
across triggers.

**For a tenant you do not trust, bound the namespace at admission instead:**

```bash
kubectl -n team-a create quota gpus --hard=requests.nvidia.com/gpu=8
```

### The arrangement where the bound does hold

Run the controller **outside** the namespace it manages, so the tenant never owns
it:

```bash
# WVA_NS is admin-owned — where the controller runs.
# WVA_WATCH_NS is the tenant's — what it manages.
NAMESPACE=team-a make deploy-wva-on-k8s SCOPE=namespace WVA_NS=wva-team-a WVA_WATCH_NS=team-a
```

This is the recommended multi-tenant shape. The controller does not manage its
own namespace, so it reads limiters from **its own ConfigMap** in `wva-team-a`,
which the admin owns. The label and `wva-policy` do not apply to it;
`make enable-physical-limiter` writes the limiter into that ConfigMap instead.

Name those admin-owned namespaces when you publish the limiter:

```bash
make enable-physical-limiter WVA_LIMITER_TARGETS='wva-team-a wva-team-b'
```

Without `WVA_LIMITER_TARGETS`, `enable-physical-limiter` finds controllers by
label and grants those that run as `wva-controller-manager` with a `manager`
container on a known image. These are **signals, not proof**: any tenant can
meet all three. They keep the grant off an account a Deployment merely names,
such as `default`, but cannot tell a tenant's copy from yours.

## Why it ships off

The shipped configuration declares **no limiter**: a fresh install scales
unconstrained, and a scale-from-zero wake skips the capacity check. This is
deliberate. A GPU-aware optimizer allocates from per-accelerator pools, so a
variant whose accelerator it cannot resolve gets no budget and **never scales
up**, with no error. On by default, it would freeze the least carefully
configured workloads.

## What has to be true first: every accelerator must resolve

The scaling manager resolves a variant's accelerator from, in order:

1. a **GPU product key in the workload's `nodeSelector` or `nodeAffinity`**: the
   only source that works before any pod exists, so the only one for a workload
   parked at zero;
2. the **node its pods are running on**, once it has ready pods.

On a **single-accelerator** cluster neither is needed: there is one pool, so the
workload is placed in it. On a **heterogeneous** cluster a pod with no GPU
nodeSelector can land on *any* GPU node, so a workload that does not state its
accelerator really has not chosen one. Pin it:

```yaml
# in the model server's pod template
spec:
  nodeSelector:
    nvidia.com/gpu.product: NVIDIA-A100-PCIE-80GB
```

```bash
# NS: the controller's namespace. NAMESPACE: the models'. Equal unless WVA_WATCH_NS is set.
kubectl get nodes -L nvidia.com/gpu.product          # what your nodes advertise
kubectl get events -n "$NAMESPACE" --field-selector reason=AcceleratorNotResolved   # on the ScaledObjects
kubectl logs -n "$NS" deploy/wva-controller-manager | grep -i "Accelerator not resolved"
```

## Permission: nodes

The limiter reads **nodes** to learn what GPUs exist, and nodes are cluster-scoped.

The scaling manager reads nodes every cycle, limiter or not: a variant's
accelerator comes from the nodes its pods run on, and the capacity model keys
learned per-replica capacity by it. The limiter adds one thing: it charges the
variant to a GPU **budget** by that accelerator.

So what a missing node permission costs depends on the limiter:

| limiter | controller can list nodes | outcome |
| --- | --- | --- |
| `none` | no | degraded: accelerators stay unresolved, so metrics lose the accelerator label and the capacity model cannot reuse learned capacity across variants |
| `gpu-inventory` | no | **install refused.** Every variant would be charged to no pool, receive no budget and stop scaling up, silently |
| `gpu-inventory` | yes | as intended |

`make check-prereqs WVA_LIMITER=gpu-inventory` checks this before you install.

### If the limiter is turned on later

An admin can add a `limiters:` entry to the scaling-policy ConfigMap at any
time. It applies live (at the next restart for a namespace-scoped controller
whose policy is outside the namespace it manages), including to a controller
installed without node permission. That combination has no natural symptom, so
it is reported three ways:

- an **error log**, naming what has stopped working;
- `wva_node_access_denied` set to **1**;
- the **`WVANodeAccessDenied`** alert (critical, fires after 2m) if you installed
  with `DEPLOY_ALERTING_RULES=true`.

That is for `gpu-inventory` declared alone. Beside a quota, the controller
still scales: the physical limiter is skipped and the quota bounds. Then
`wva_node_access_denied` stays 0, and the skip is reported as
`wva_constraint_provider_skipped{reason="node_read_denied"}` and the
**`WVAConstraintProviderSkipped`** alert (warning, after 10m).

## FMA namespaces: the GPU picture is a lower bound

If a namespace runs Fast Model Actuation, **treat every GPU number here as a
minimum, not a measurement.**

FMA's launcher pods request no `nvidia.com/gpu`, on purpose: the requester
reserves the accelerator and the launcher binds onto it. Requesting on both
would double-book an N-GPU node. While a pair is bound, the GPU is charged to
the requester, the scale target this limiter already accounts against.

The gap opens when a pair unbinds. The launcher keeps its vLLM instance resident
(that is what makes the next bind take seconds, not minutes), so it still
occupies a physical GPU now charged to nobody. Measured on a real cluster, with
every requester at `replicas: 0`:

| | |
| --- | --- |
| GPU requests charged in the namespace | 1 |
| launcher pods running a vLLM instance | 9, on 9 distinct GPU UUIDs |

Three consequences:

- A `ResourceQuota` on `requests.nvidia.com/gpu` counts requesters only. It cannot
  express a warm pool, so GPU quota does not bound FMA's real consumption.
- This limiter's view of what is free is too optimistic by the size of the warm
  pool, which is the unsafe direction — it is the input the wake-capacity check
  and reclamation trust.
- The scheduler sees those GPUs as allocatable and may place another workload on
  one, which then contends with the sleeping instance when FMA wakes it.

**There is no fix from outside FMA.** Reading it off the pods does not work:
`dual-pods.llm-d.ai/vllm-config`, `/server-port` and `/accelerators` exist only
*while bound*. An orphaned launcher running an instance carries none of them, so
the API server has no record of its GPU. Only the launcher's own API knows, and
the scaling manager does not call workload APIs.

So in an FMA namespace, subtract the warm pool by hand when planning capacity.
Its ceiling is `launcherCount × maxInstances` per matching node, from the
`LauncherPopulationPolicy` and `LauncherConfig`. `deploy/lib` warns at plan time
when it detects launchers. The upstream request that would close this is item 1
in [the FMA post-mortem](../proposals/fma-post-mortem.md); none of those
requests were ever filed upstream.

## Checking

The install warns too: `WVA_LIMITER=gpu-inventory` counts the distinct GPU
products the cluster advertises and says whether pinning is needed. An
unresolved variant is logged once per change and counted in
`wva_unattributed_gpus`. See
[GPU Capacity Accounting](../concepts/gpu-capacity-accounting.md).

